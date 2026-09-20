# 星座在轨交付协同中枢

PIESAT-2 卫星发射入轨后，值班主管需要在有限测控窗口内完成健康确认、载荷开机、
交叉标定与整组验收。本服务是可独立运行的后端中枢，解决三类协同问题：

1. **指令依据可追溯**——遥测带星上时间、地面接收时间、源内序号与质量标记；
   每次批准冻结所依据的遥测帧与前置条件纪元，群消息里“凭哪版遥测下的令”有据可查。
2. **席位/资源不抢占**——同一颗卫星、同一个地面站同一时刻只允许一项计划持有执行权，
   两个席位并发争抢时只有一个成功，另一个得到 409 冲突。
3. **异常可续办、重启可找回**——安全模式、窗口取消、执行超时分别进入处置链，
   全程留痕、可改挂新窗口/待退出确认后续办；进程重启通过事件日志完整回放恢复未完成工作。

## 运行

```bash
go run ./cmd/server                         # 默认 :8080，事件日志 data/mission.log
go run ./cmd/server -addr :8080 -log /var/lib/mission/mission.log -lease-ttl 3m
go run ./cmd/server -sim-clock              # 演练模式：接受 X-Sim-At 头驱动命令时间
```

检查：

```bash
go test ./...        # 领域不变量 + HTTP 端到端，共 20+ 用例，含 -race
go vet ./...
```

## 架构

- **事件溯源（WAL）**：`internal/mission/store.go` 是 JSON Lines 追加日志，
  每批事件一次 `fsync`。状态全部由事件投影得到，无第二份持久化。
  启动时完整回放，执行中计划、开启的处置链、已签结论全部找回。
- **串行命令模型**：`Service` 以单互斥锁串行化所有写命令（`internal/mission/commands.go`），
  先落日志、再投影；派生事件（如遥测触发批准失效、安全模式触发撤权）与触发事件同批提交。
- **状态机**：计划流转
  `draft → submitted → approved → executing → executed/failed`；
  执行权丧失时进入 `awaiting`，由处置链续办回到 `submitted`。
- **HTTP 层**：`internal/api`，错误码映射 400/404/409/412。

### 核心不变量

| 需求 | 实现 |
|---|---|
| 乱序遥测不回退已确认状态 | 同一数据源按源内序号单调采纳；序号回退/重复只存档（`applied=false` 并记 `stale_reason`）；读数还须星上时间不早于当前依据 |
| 质量标记 | `bad` 帧永不参与结论，只留档；`verified`/`degraded` 可采纳 |
| 前置条件变化使高风险批准失效 | 姿态/能源达标结论翻转、安全模式进入/退出均推进“纪元（epoch）”；纪元一变，该星当前版本全部有效批准标记失效并记原因/时间，计划退回 `submitted` 重新双签；结论未翻转的常规更新帧只换依据、不误伤批准 |
| 指令版本 | 计划有自增 `revision` 与载荷摘要（默认 sha256）；重订版本后旧版批准全部失效 |
| 双人复核 | 需 `primary` 与 `reviewer` 两个席位、且必须是两个不同的人；批准瞬间校验窗口有效、无安全模式、姿态/能源达标，并冻结纪元快照与遥测依据帧 |
| 资源重叠只允许一项执行 | 执行权同时占用“卫星”和“地面站”两个维度，任一被占即 409；租约带截止时间（TTL，且不超过窗口结束），成功回执或撤权时释放 |
| 安全模式处置链 | 执行中收到安全模式遥测 → 自动撤销执行权、开 `safe_mode` 链；必须收到更新的非安全模式遥测确认退出后才能续办 |
| 窗口取消处置链 | 取消窗口 → 引用它的非终态计划全部开 `window_cancel` 链、执行权撤销、批准失效；续办须改挂同星有效新窗口 |
| 执行超时处置链 | 超过租约截止仍无回执（后台每秒巡检，或 `POST /api/admin/sweep`）→ 撤权、旧双签失效、开 `timeout` 链；迟到回执不再被接受，须按新窗口重新双签 |
| 处置链可续办、可交接 | 每条链支持多次处置留痕（`/work`，值班交接用）、`resolve`（含续办载荷）或 `fail` |
| 签署冻结证据 | `accepted`/`conditional`/`rejected` 签署瞬间深拷贝完整证据进事件；之后遥测/计划再变，已签结论不动 |
| 回答“还缺什么/谁批了哪版/执行结果” | `GET /api/satellites/{id}` 给出结构化缺口；证据中含每条计划的版本、双签人/席位/依据帧与星上回执 |

## HTTP 接口

所有时间为 RFC 3339 带时区字符串。

### 卫星与遥测
- `POST /api/satellites` — `{satellite_id, name?, required_checks?, thresholds?}`；默认必做三项：健康确认、载荷开机、交叉标定；默认门限姿态 80、能源 60
- `GET /api/satellites` / `GET /api/satellites/{id}` — 后者含 `safe_mode`、各条件结论与依据帧纪元、`gaps`、未闭环处置链
- `POST /api/satellites/{id}/telemetry` — `{source_id, source_sequence, spacecraft_time, received_at, quality, readings:{attitude,power}, safe_mode}`，返回帧的 `applied`/`stale_reason`
- `GET /api/satellites/{id}/telemetry` — 全部存档帧（含乱序/坏帧）

### 测控窗口
- `POST /api/windows` — `{window_id, satellite_id, station_id, start, end}`
- `GET /api/windows?satellite_id=` / `GET /api/windows/{id}`
- `POST /api/windows/{id}/cancel` — `{by, reason}`，触发窗口取消处置链
- `POST /api/windows/{id}/close`

### 指令计划、复核、执行
- `POST /api/plans` — `{satellite_id, window_id, command_type, payload|payload_digest, created_by}`
- `POST /api/plans/{id}/submit` / `revise`（`{payload|payload_digest}`，新版本号）/ `reject`（`{by,reason}`）
- `POST /api/plans/{id}/approvals` — `{role: primary|reviewer, approver}`，两签齐备转 `approved`
- `POST /api/plans/{id}/execute` — `{seat}`；须在窗口内、条件达标、资源空闲；返回租约与截止时间
- `POST /api/plans/{id}/receipt` — `{success, code?, message?, spacecraft_time, received_at, ...}`
- `GET /api/plans?satellite_id=` / `GET /api/plans/{id}`

### 处置链
- `GET /api/issues?type=safe_mode|timeout|window_cancel&open=1` / `GET /api/issues/{id}`
- `POST /api/issues/{id}/work` — `{by, note}` 处置/交接留痕
- `POST /api/issues/{id}/resolve` — `{by, resolution, new_window_id?}`；安全模式链须先确认退出，超时/窗口取消链须给同星有效新窗口
- `POST /api/issues/{id}/fail` — `{by, resolution}` 无法续办时终态留痕

### 交付结论
- `POST /api/deliveries` — `{scope?, satellite_ids:[...], decision: accepted|conditional|rejected, by, notes?}`；
  整组 `accepted` 时任一卫星有缺口返回 412（可改签 `conditional`）
- `GET /api/deliveries` / `GET /api/deliveries/{id}` — 返回冻结证据：每星条件结论、缺口、全部计划的版本/双签/回执、未闭环处置链

### 运维
- `GET /health`
- `POST /api/admin/sweep` — 手动推进一次超时巡检
- 请求头 `X-Sim-At: <RFC3339>` — 仅 `-sim-clock` 模式生效，把中枢时钟拨到指定时刻（演练/测试用，生产模式发送该头会被 400 拒绝）

## 典型时序（单星）

```
注册卫星 → 达标遥测 → 安排窗口 → 建计划(draft)
  → submit → 主操作席批准 → 复核席批准(approved，冻结遥测依据)
  → 窗口内 execute（占用卫星+地面站，获得带截止时间的租约）
  → 星上回执成功(executed，释放资源)
三项必做全部成功 → 主管签 accepted（证据冻结）
```

异常分支：

```
执行中进入安全模式 → 自动撤权 + safe_mode 链
  → /work 留痕 → 收到退出确认遥测 → /resolve → 重新双签 → 重新执行
窗口被取消       → 撤权 + window_cancel 链 → 安排新窗口 → /resolve(new_window_id) → 重新双签
超时无回执       → 巡检撤权 + timeout 链  → 安排新窗口 → /resolve(new_window_id) → 重新双签
```

## 目录

```
cmd/server/            服务入口（WAL、演练时钟、后台巡检）
internal/mission/      领域：事件日志、投影状态机、命令、查询视图（核心）
internal/api/          HTTP 路由与错误映射
fixtures/mission.json  遥测/窗口字段示例
```
