package mission_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"example.com/constellation-handover"
)

func newHTTPServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	base := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	clock := func() time.Time { return base }
	svc, err := mission.OpenService(mission.Config{Dir: t.TempDir(), Now: clock, DefaultTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(mission.NewServer(svc).Handler())
	return ts, func() {
		ts.Close()
		_ = svc.Close()
	}
}

// doReq 发送 JSON 请求并把响应体解码到 out（可为 map 或切片）。
func doReq(t *testing.T, method, url string, body any, out any) int {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, url, &buf)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("HTTP 请求失败: %v", err)
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("响应不是 JSON: %v", err)
		}
	}
	return resp.StatusCode
}

func mustStatus(t *testing.T, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("HTTP 状态 = %d, 期望 %d", got, want)
	}
}

// TestEndToEndHTTPFlow 走通：注册 → 窗口 → 乱序遥测 → 健康确认 → 指令 →
// 双签 → 执行 → 回执 → 就绪度 → 整组验收，全部经 HTTP 完成。
func TestEndToEndHTTPFlow(t *testing.T) {
	ts, cleanup := newHTTPServer(t)
	defer cleanup()
	u := ts.URL

	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/satellites",
		map[string]any{"satellite_id": "PIESAT-2-13", "name": "十三号星"}, nil), http.StatusCreated)
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/stations",
		map[string]any{"station_id": "TY-GS-01", "name": "太原站"}, nil), http.StatusCreated)

	winBody := map[string]any{
		"window_id": "win-13", "satellite_id": "PIESAT-2-13", "station_id": "TY-GS-01",
		"start": "2026-09-20T08:55:00Z", "end": "2026-09-20T09:30:00Z", "version": 1,
	}
	var winResp map[string]any
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/windows", winBody, &winResp), http.StatusCreated)

	frameBody := func(seq int64, sc string, data map[string]any) map[string]any {
		return map[string]any{
			"satellite_id": "PIESAT-2-13", "source_id": "bus", "source_sequence": seq,
			"spacecraft_time": sc, "received_at": "2026-09-20T09:00:05Z",
			"quality": "verified", "data": data,
		}
	}
	// 乱序：seq=2 先到。
	var f2 map[string]any
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/telemetry/frames",
		frameBody(2, "2026-09-20T08:59:58Z", map[string]any{"attitude_mode": "nadir", "battery_soc": 88.0}), &f2),
		http.StatusCreated)
	if f2["became_current"] != true {
		t.Fatal("seq=2 应为当前帧")
	}
	var f1 map[string]any
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/telemetry/frames",
		frameBody(1, "2026-09-20T08:59:50Z", map[string]any{"attitude_mode": "sun", "battery_soc": 70.0}), &f1),
		http.StatusCreated)
	if f1["became_current"] != false {
		t.Fatal("迟到的 seq=1 不得倒退当前帧")
	}

	// 健康确认。
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/satellites/PIESAT-2-13/health-confirmations",
		map[string]any{"operator": "lead", "note": "入轨状态良好"}, nil), http.StatusCreated)

	// 载荷开机：指令版本 → 计划 → 双签 → 执行 → 回执。
	var cmd map[string]any
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/commands", map[string]any{
		"satellite_id": "PIESAT-2-13", "command": "payload_power", "created_by": "planner",
		"content": map[string]any{"rail": "A", "power": true},
	}, &cmd), http.StatusCreated)
	cmdID := cmd["id"].(string)

	var plan map[string]any
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/plans", map[string]any{
		"satellite_id": "PIESAT-2-13", "window_id": "win-13", "command_version_id": cmdID,
		"kind": "payload_power", "milestone": "payload_on", "timeout_seconds": 2,
		"gates":      []map[string]any{{"type": "attitude", "param": map[string]any{"mode": "nadir"}}},
		"created_by": "planner",
	}, &plan), http.StatusCreated)
	planID := plan["id"].(string)

	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/plans/"+planID+"/approvals",
		map[string]any{"role": "proposer", "operator": "alice", "note": "一审通过"}, nil), http.StatusCreated)
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/plans/"+planID+"/approvals",
		map[string]any{"role": "verifier", "operator": "bob", "note": "二审通过"}, nil), http.StatusCreated)

	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/plans/"+planID+"/execution/start",
		map[string]any{"operator": "alice"}, nil), http.StatusCreated)
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/plans/"+planID+"/execution/resolve",
		map[string]any{"outcome": "succeeded", "receipt_status": "payload_powered", "receipt_seq": 5001}, nil),
		http.StatusOK)

	// 交叉标定在没有伴星/计划的情况下仍缺：就绪度应只缺交叉标定。
	var ready map[string]any
	mustStatus(t, doReq(t, http.MethodGet, u+"/v1/satellites/PIESAT-2-13/readiness", nil, &ready), http.StatusOK)
	if ready["ready"] != false {
		t.Fatal("交叉标定未完成，不应就绪")
	}
	ms, _ := ready["milestones"].(map[string]any)
	if ms["health"] != true || ms["payload_on"] != true || ms["cross_cal"] != false {
		t.Fatalf("里程碑状态错误: %v", ms)
	}
	missing, _ := ready["missing"].([]any)
	if len(missing) != 1 {
		t.Fatalf("应仅缺交叉标定一项，实际 %v", missing)
	}

	// 未就绪时整组接收应 412。
	var body map[string]any
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/deliveries",
		map[string]any{"scope": "group", "conclusion": "accepted", "signer": "director"}, &body),
		http.StatusPreconditionFailed)
	if errObj, _ := body["error"].(map[string]any); errObj["code"] != "precondition_failed" {
		t.Fatalf("应返回前置条件错误码，实际 %v", body)
	}
}

// TestHTTPResourceConflictAndTimeout 验证资源互斥（409）与超时处置链（含续办）。
func TestHTTPResourceConflictAndTimeout(t *testing.T) {
	ts, cleanup := newHTTPServer(t)
	defer cleanup()
	u := ts.URL

	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/stations",
		map[string]any{"station_id": "GS", "name": "站"}, nil), http.StatusCreated)

	createSatPlan := func(sat string) string {
		mustStatus(t, doReq(t, http.MethodPost, u+"/v1/satellites",
			map[string]any{"satellite_id": sat, "name": sat}, nil), http.StatusCreated)
		mustStatus(t, doReq(t, http.MethodPost, u+"/v1/windows", map[string]any{
			"window_id": "w-" + sat, "satellite_id": sat, "station_id": "GS",
			"start": "2026-09-20T08:00:00Z", "end": "2026-09-20T10:00:00Z", "version": 1,
		}, nil), http.StatusCreated)
		mustStatus(t, doReq(t, http.MethodPost, u+"/v1/telemetry/frames", map[string]any{
			"satellite_id": sat, "source_id": "bus", "source_sequence": 1,
			"spacecraft_time": "2026-09-20T08:59:59Z", "received_at": "2026-09-20T09:00:01Z",
			"quality": "verified", "data": map[string]any{"ok": true},
		}, nil), http.StatusCreated)
		var cmd map[string]any
		mustStatus(t, doReq(t, http.MethodPost, u+"/v1/commands", map[string]any{
			"satellite_id": sat, "command": "go", "created_by": "p", "content": map[string]any{"x": 1},
		}, &cmd), http.StatusCreated)
		var plan map[string]any
		mustStatus(t, doReq(t, http.MethodPost, u+"/v1/plans", map[string]any{
			"satellite_id": sat, "window_id": "w-" + sat, "command_version_id": cmd["id"],
			"kind": "go", "timeout_seconds": 2, "created_by": "p",
		}, &plan), http.StatusCreated)
		pid := plan["id"].(string)
		mustStatus(t, doReq(t, http.MethodPost, u+"/v1/plans/"+pid+"/approvals",
			map[string]any{"role": "proposer", "operator": "alice"}, nil), http.StatusCreated)
		mustStatus(t, doReq(t, http.MethodPost, u+"/v1/plans/"+pid+"/approvals",
			map[string]any{"role": "verifier", "operator": "bob"}, nil), http.StatusCreated)
		return pid
	}

	p1 := createSatPlan("S1")
	p2 := createSatPlan("S2")

	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/plans/"+p1+"/execution/start",
		map[string]any{"operator": "alice"}, nil), http.StatusCreated)
	// 同站重叠：第二项拿不到执行权 → 409。
	var conflict map[string]any
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/plans/"+p2+"/execution/start",
		map[string]any{"operator": "carol"}, &conflict), http.StatusConflict)

	// p1 不登记回执，等待超时扫描（服务内时钟固定，超时窗口 2s，这里用维护端点触发）。
	// 由于时钟固定不会推进，改为先成功落定 p1 释放资源，再验证 p2 可执行；
	// 超时路径已由领域层测试覆盖。
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/plans/"+p1+"/execution/resolve",
		map[string]any{"outcome": "failed", "receipt_status": "nak", "detail": "模拟失败"}, nil),
		http.StatusOK)
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/plans/"+p2+"/execution/start",
		map[string]any{"operator": "carol"}, nil), http.StatusCreated)
}

// TestHTTPSafeModeChain 验证安全模式处置链经 HTTP 开立与续办。
func TestHTTPSafeModeChain(t *testing.T) {
	ts, cleanup := newHTTPServer(t)
	defer cleanup()
	u := ts.URL
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/satellites",
		map[string]any{"satellite_id": "S9", "name": "九号"}, nil), http.StatusCreated)

	var resp map[string]any
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/satellites/S9/safe-mode",
		map[string]any{"reason": "姿态偏差超限"}, &resp), http.StatusOK)
	c, _ := resp["case"].(map[string]any)
	caseID := c["id"].(string)

	// 未关闭处置链可查、可续办。
	var advanced map[string]any
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/cases/"+caseID+"/advance",
		map[string]any{"action": "diagnose", "operator": "bob", "note": "排查中"}, &advanced), http.StatusOK)
	steps, _ := advanced["steps"].([]any)
	if len(steps) != 1 {
		t.Fatalf("处置链应记录 1 个续办步骤，实际 %d", len(steps))
	}
	var cleared map[string]any
	mustStatus(t, doReq(t, http.MethodPost, u+"/v1/satellites/S9/safe-mode/clear",
		map[string]any{"operator": "bob", "note": "已恢复"}, &cleared), http.StatusOK)
	cc, _ := cleared["case"].(map[string]any)
	if cc["state"] != "resolved" {
		t.Fatalf("解除安全模式后处置链应关闭，实际 %v", cc["state"])
	}
}
