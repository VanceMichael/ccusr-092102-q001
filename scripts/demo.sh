#!/usr/bin/env bash
# 端到端演示：四颗 PIESAT-2 卫星的在轨交付协同流程。
# 用法: ./scripts/demo.sh [BASE_URL]
set -euo pipefail
BASE="${1:-http://localhost:8080}"
J=/tmp/handover-demo
mkdir -p "$J"

# iso <分钟偏移>：输出相对当前 UTC 的 RFC3339 时间，保证演示窗口处于“当下”。
iso() { python3 -c 'import datetime,sys;print((datetime.datetime.now(datetime.timezone.utc)+datetime.timedelta(minutes=int(sys.argv[1]))).strftime("%Y-%m-%dT%H:%M:%SZ"))' "$1"; }

# req <期望状态码> <method> <path> [json]：发请求、校验 HTTP 状态码并输出响应体。
req() {
  local want="$1" m="$2" p="$3" body="${4:-}"
  local args=(-sS -X "$m" -w $'\n%{http_code}' "$BASE$p")
  if [[ -n "$body" ]]; then
    printf '%s' "$body" > "$J/body.json"
    args+=(-H 'Content-Type: application/json' --data-binary @"$J/body.json")
  fi
  local out code
  out=$(curl "${args[@]}")
  code="${out##*$'\n'}"
  out="${out%$'\n'*}"
  if [[ "$code" != "$want" ]]; then
    printf '请求 %s %s 期望状态 %s 实际 %s，响应: %s\n' "$m" "$p" "$want" "$code" "$out" >&2
    exit 1
  fi
  printf '%s' "$out"
}
post() { req "${1}" POST "${2}" "${3:-}"; }
say() { printf '\n\033[1;36m== %s ==\033[0m\n' "$1"; }

say "1. 注册四颗卫星与两个地面站"
for i in 1 2 3 4; do
  post 201 /v1/satellites "{\"satellite_id\":\"PIESAT-2-$i\",\"name\":\"二号组网星 $i\"}" >/dev/null
done
post 201 /v1/stations '{"station_id":"TY-GS-01","name":"太原站"}' >/dev/null
post 201 /v1/stations '{"station_id":"KS-GS-02","name":"喀什站"}' >/dev/null
echo "已注册 4 星 2 站"

say "2. 排定测控窗口（同站窗口在时间上重叠——排期允许，执行阶段才由资源锁仲裁）"
post 201 /v1/windows "{\"window_id\":\"w1\",\"satellite_id\":\"PIESAT-2-1\",\"station_id\":\"TY-GS-01\",\"start\":\"$(iso -2)\",\"end\":\"$(iso 30)\",\"version\":1}" >/dev/null
post 201 /v1/windows "{\"window_id\":\"w2\",\"satellite_id\":\"PIESAT-2-2\",\"station_id\":\"TY-GS-01\",\"start\":\"$(iso -2)\",\"end\":\"$(iso 30)\",\"version\":1}" >/dev/null
post 201 /v1/windows "{\"window_id\":\"w3\",\"satellite_id\":\"PIESAT-2-3\",\"station_id\":\"KS-GS-02\",\"start\":\"$(iso -2)\",\"end\":\"$(iso 30)\",\"version\":1}" >/dev/null
post 201 /v1/windows "{\"window_id\":\"w4\",\"satellite_id\":\"PIESAT-2-4\",\"station_id\":\"KS-GS-02\",\"start\":\"$(iso -2)\",\"end\":\"$(iso 30)\",\"version\":1}" >/dev/null
echo "已排定 4 个窗口"

say "3. 乱序上报遥测（seq=2 先于 seq=1）：迟到帧不得倒退当前版本"
post 201 /v1/telemetry/frames '{"satellite_id":"PIESAT-2-1","source_id":"bus","source_sequence":2,"spacecraft_time":"'$(iso -1)'","received_at":"'$(iso 0)'","quality":"verified","data":{"attitude_mode":"nadir","battery_soc":92}}'
echo
post 201 /v1/telemetry/frames '{"satellite_id":"PIESAT-2-1","source_id":"bus","source_sequence":1,"spacecraft_time":"'$(iso -3)'","received_at":"'$(iso 0)'","quality":"verified","data":{"attitude_mode":"sun_point","battery_soc":71}}'
echo

say "4. 健康确认（冻结所依据的遥测帧键与哈希）"
post 201 /v1/satellites/PIESAT-2-1/health-confirmations '{"operator":"lead-zhao","note":"入轨姿态能源正常"}'
echo

say "5. 创建指令版本与载荷开机计划（含姿态/能源门禁）"
post 201 /v1/commands '{"satellite_id":"PIESAT-2-1","command":"payload_power","created_by":"planner-qian","content":{"rail":"A","power":true}}' > "$J/cmd.json"
CMD=$(python3 -c 'import json;print(json.load(open("'$J'/cmd.json"))["id"])')
post 201 /v1/plans "{\"satellite_id\":\"PIESAT-2-1\",\"window_id\":\"w1\",\"command_version_id\":\"$CMD\",\"kind\":\"payload_power\",\"milestone\":\"payload_on\",\"timeout_seconds\":300,\"gates\":[{\"type\":\"attitude\",\"param\":{\"mode\":\"nadir\"}},{\"type\":\"battery\",\"param\":{\"min\":80}}],\"created_by\":\"planner-qian\"}" > "$J/plan.json"
PLAN=$(python3 -c 'import json;print(json.load(open("'$J'/plan.json"))["id"])')
echo "plan=$PLAN"

say "6. 双人复核（proposer=alice, verifier=bob），批准依据（遥测版本/窗口版本/安全代数/指令哈希/门禁结果）被冻结"
post 201 "/v1/plans/$PLAN/approvals" '{"role":"proposer","operator":"alice","note":"一审通过"}' >/dev/null
post 201 "/v1/plans/$PLAN/approvals" '{"role":"verifier","operator":"bob","note":"二审通过"}' >/dev/null
echo "双签完成"

say "7. 资源重叠仲裁：同星同站再建一条已双签计划，只有一项能取得执行权（第二项预期 409）"
post 201 /v1/commands '{"satellite_id":"PIESAT-2-1","command":"collide_demo","created_by":"x","content":{"x":1}}' > "$J/cmd2.json"
CMD2=$(python3 -c 'import json;print(json.load(open("'$J'/cmd2.json"))["id"])')
post 201 /v1/plans "{\"satellite_id\":\"PIESAT-2-1\",\"window_id\":\"w1\",\"command_version_id\":\"$CMD2\",\"kind\":\"collide_demo\",\"created_by\":\"x\"}" > "$J/plan2.json"
PLAN2=$(python3 -c 'import json;print(json.load(open("'$J'/plan2.json"))["id"])')
post 201 "/v1/plans/$PLAN2/approvals" '{"role":"proposer","operator":"carol"}' >/dev/null
post 201 "/v1/plans/$PLAN2/approvals" '{"role":"verifier","operator":"dave"}' >/dev/null
post 201 "/v1/plans/$PLAN/execution/start" '{"operator":"alice"}' >/dev/null
echo "计划 $PLAN 取得执行权"
echo "--- 计划 $PLAN2 竞争结果（预期 conflict）:"
post 409 "/v1/plans/$PLAN2/execution/start" '{"operator":"carol"}'
echo

say "8. 登记星上回执：执行落定、载荷开机里程碑完成、资源释放"
post 200 "/v1/plans/$PLAN/execution/resolve" '{"outcome":"succeeded","receipt_status":"payload_powered","receipt_seq":9001}' >/dev/null
echo "已记录成功回执"
# 资源释放后，第二条计划现在可以取得执行权；随后登记失败回执。
post 201 "/v1/plans/$PLAN2/execution/start" '{"operator":"carol"}' >/dev/null
post 200 "/v1/plans/$PLAN2/execution/resolve" '{"outcome":"failed","receipt_status":"nak","detail":"演示失败回执也如实留档"}' >/dev/null
echo "第二条计划失败回执已留档（不产生里程碑）"

say "9. 窗口取消：相关批准原子失效、执行中计划中止，并开立可续办处置链"
# 为 2 号星准备一条绑定 w2 的已批准计划。
post 201 /v1/telemetry/frames '{"satellite_id":"PIESAT-2-2","source_id":"bus","source_sequence":1,"spacecraft_time":"'$(iso -1)'","received_at":"'$(iso 0)'","quality":"verified","data":{"attitude_mode":"nadir","battery_soc":90}}' >/dev/null
post 201 /v1/satellites/PIESAT-2-2/health-confirmations '{"operator":"lead-zhao"}' >/dev/null
post 201 /v1/commands '{"satellite_id":"PIESAT-2-2","command":"payload_power","created_by":"x","content":{"p":1}}' > "$J/cmdw2.json"
CMDW2=$(python3 -c 'import json;print(json.load(open("'$J'/cmdw2.json"))["id"])')
post 201 /v1/plans "{\"satellite_id\":\"PIESAT-2-2\",\"window_id\":\"w2\",\"command_version_id\":\"$CMDW2\",\"kind\":\"payload_power\",\"created_by\":\"x\"}" > "$J/planw2.json"
PLANW2=$(python3 -c 'import json;print(json.load(open("'$J'/planw2.json"))["id"])')
post 201 "/v1/plans/$PLANW2/approvals" '{"role":"proposer","operator":"alice"}' >/dev/null
post 201 "/v1/plans/$PLANW2/approvals" '{"role":"verifier","operator":"bob"}' >/dev/null
post 200 /v1/windows/w2/cancel '{"reason":"轨道预警，整体后移"}'
echo

say "10. 被取消窗口的计划：批准已失效、门禁不通过"
req 200 GET "/v1/plans/$PLANW2" | python3 -c '
import json,sys
st=json.load(sys.stdin)
print("gates_passed =", st["gates_passed"])
print("有效批准数   =", sum(1 for a in st["plan"]["approvals"] if a["valid"]))
print("失效原因     =", [a["invalidated_reason"] for a in st["plan"]["approvals"]][0])'

say "11. 查看每颗星还缺什么（GET /v1/readiness 摘要）"
req 200 GET /v1/readiness | python3 -c '
import json,sys
for r in json.load(sys.stdin):
    sid=r["satellite_id"]; miss=[m["detail"] for m in r["missing"]]
    print(sid, "ready="+str(r["ready"]), "缺失="+str(miss))'

say "12. 整组接收签署（尚有缺失，预期 412）"
post 412 /v1/deliveries '{"scope":"group","conclusion":"accepted","signer":"director-min"}' | python3 -c 'import json,sys;print("被拒绝签署:",json.load(sys.stdin)["error"]["message"])'

say "13. 拒收签署始终允许，且证据被冻结"
post 201 /v1/deliveries '{"scope":"group","conclusion":"rejected","signer":"director-min","note":"窗口取消，待续办后重新验收"}' | python3 -c '
import json,sys
d=json.load(sys.stdin)
print("交付记录:", d["id"], d["conclusion"], "证据哈希:", d["bundle"]["hash"][:16], "...覆盖星数:", len(d["bundle"]["satellites"]))'
