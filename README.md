# 星座在轨交付协同中枢（constellation-handover）

四颗 PIESAT-2 卫星确认入轨后，值班主管要在有限测控窗口内完成**健康确认、载荷开机、交叉标定与整组验收**。本服务是这一过程的后端中枢：接收遥测、管理测控窗口与前置条件、管理指令版本与双人复核、仲裁地面站/卫星资源、跟踪执行回执，并在主管签署时冻结全部交付证据。

仅依赖 Go 1.24 标准库，可独立运行；所有状态保存在本地数据目录，**进程重启后自动找回全部未完成工作**。

## 它解决的核心问题

| 风险 | 机制 |
| --- | --- |
| 群消息说不清指令依据了哪一版遥测 | 每次双人复核都**冻结依据包**：遥测帧键+内容哈希、星上条件签名、窗口版本、安全模式代数、指令版本哈希、当时的门禁结果 |
| 乱序遥测让已确认状态倒退 | 帧按 `(卫星, 数据源, 源内序号)` 去重；当前帧只按源内序号**单调向前**，迟到帧留档但永不抢占；健康确认、里程碑只增不减 |
| 前置条件变化后旧批准仍被执行 | 遥测换版、窗口取消/换版、进入安全模式都会**原子地使相关批准失效**；执行前再次复核全部门禁 |
| 两个席位抢占同一颗星/同一个站 | 计划开始执行即对 `卫星 + 伴星 + 地面站` 加资源锁；重叠时**只有一项计划取得执行权**，裁决由持久化事件确定，并发安全 |
| 安全模式 / 窗口取消 / 执行超时 | 各自进入一条**可续办处置链（case）**：记录每一步处置、可重发计划（重新双签）、可关闭；重启后仍可找回 |
| 主管签署后证据再被改动 | 签署瞬间把每颗星的当前遥测、健康确认、里程碑、指令版本+批准人、执行回执、窗口、未关闭处置链与缺失项**冻结成不可变证据包**（带哈希） |
| 每颗星还缺什么 | `GET /v1/readiness`：逐项列出缺失（安全模式/遥测/里程碑/未关闭处置链），并能回答“谁批准过哪版指令、实际执行结果是什么” |

## 运行

```bash
go run ./cmd/server
# 或
go build -o handover ./cmd/server && ./handover
```

配置（环境变量）：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `HANDOVER_ADDR` | `:8080` | 监听地址 |
| `HANDOVER_DATA_DIR` | `./data` | 事件日志与快照目录 |
| `HANDOVER_SWEEP_INTERVAL` | `1s` | 执行超时扫描周期 |
| `HANDOVER_SNAPSHOT_EVENTS` | `200` | 每多少条事件落一次快照（0 关闭） |
| `HANDOVER_DEFAULT_TIMEOUT` | `90s` | 计划默认执行超时 |

检查与演示：

```bash
go test ./...            # 单元/恢复/并发/HTTP 测试（含 -race）
./scripts/demo.sh        # 对 http://localhost:8080 跑完整四星座接流程
```

## 架构

```
HTTP/JSON (api.go, cmd/server)
        │
  Service（单进程写串行化；领域规则）
  service.go / service_plans.go / service_execution.go / service_delivery.go
        │  先 append 事件(fsync) → 再归约内存状态
  EventLog(events.log, 仅追加 JSONL)  +  Snapshot(snapshot.json, 原子替换)
        │
  State（事件重放得到的聚合：星/站/窗口/帧/指令/计划/处置链/锁/交付）
```

- **事件溯源**：每次状态变更都是一条不可变事件（`events.go`），写入 `events.log` 并 `fsync`；内存状态只能由事件归约得到（`state.go`）。
- **崩溃恢复**：启动时加载快照、重放其后的事件。执行中但未回执的尝试、未关闭的处置链都会恢复；后台超时扫描负责找回并裁定。
- **原子联动**：窗口取消、进入安全模式这类事件在**同一条事件**内携带“批准失效列表 + 中止的计划 + 开立的处置链”，不会出现只生效一半的中间状态。
- **时间**：全部时间为带时区的 RFC 3339。遥测帧必须同时带星上时间 `spacecraft_time`、地面接收时间 `received_at`、源内序号 `source_sequence` 与质量标记 `quality`（`verified`/`degraded`/`invalid`）；门禁只采用 `verified` 的当前帧。

## 前置条件（门禁）类型

| type | 参数 | 含义 |
| --- | --- | --- |
| `window_open` | （隐式） | 窗口未取消、版本未漂移、当前时间在窗口内 |
| `satellite_nominal` | `satellite_id?`（隐式，可指伴星） | 卫星未处于安全模式（按安全模式代数裁决） |
| `telemetry_fresh` | `max_age_seconds` | 最新已验证帧星上时间距今不超过阈值 |
| `attitude` | `mode` | 遥测 `attitude_mode` 等于期望值 |
| `battery` | `min` | 遥测 `battery_soc` ≥ 阈值 |
| `payload` | `on` | 遥测 `payload_on` 与期望一致 |
| `data` | `path`/`op`/`value` | 点分路径的通用比较（`eq/ne/gt/ge/lt/le`） |

`window_open` 与 `satellite_nominal`（含交叉标定伴星）由系统隐式加入，无需调用方声明。

## 双人复核与指令版本

- 一条指令（`command`）可有多个不可变 `revision`，每版有内容哈希；计划绑定**确定的一版**。
- 计划需要 `proposer`（提议）与 `verifier`（复核）两个**不同操作人**签署；签署时门禁必须全部通过，依据被冻结进批准记录。
- 依据随后任何漂移（新遥测成为当前版本、窗口取消/换版、新一代安全模式）都会把批准标记为 `valid=false` 并记录原因与时间；计划必须重新双签才能执行。

## 处置链（可续办）

- `safe_mode`：进入安全模式时开立；解除安全模式时自动追加步骤并关单。
- `window_canceled`：取消窗口时开立，受影响批准失效、执行中计划被中止并释放资源；可用新窗口 `replan` 续办。
- `execution_timeout`：超过计划截止时间未收到回执，由扫描裁定为 `timed_out`、释放资源并开立；可 `replan` 重发（重新双签）。

## HTTP API 摘要

```
POST   /v1/satellites                         注册卫星
GET    /v1/satellites                         卫星列表
GET    /v1/satellites/{id}                    卫星详情（各数据源当前帧）
POST   /v1/stations                           注册地面站
GET    /v1/stations

POST   /v1/windows                            排定/改期窗口（version 必须递增）
GET    /v1/windows
GET    /v1/windows/{id}
POST   /v1/windows/{id}/cancel                取消窗口（失效批准+中止计划+处置链）

POST   /v1/telemetry/frames                   上报遥测帧
GET    /v1/telemetry/frames?key=...           读取帧（含是否当前版本）
GET    /v1/satellites/{id}/frames
POST   /v1/satellites/{id}/health-confirmations
POST   /v1/satellites/{id}/safe-mode
POST   /v1/satellites/{id}/safe-mode/clear

POST   /v1/commands                           创建指令版本
GET    /v1/commands?satellite_id=...

POST   /v1/plans                              创建计划（窗口+指令版本+门禁+里程碑）
GET    /v1/plans
GET    /v1/plans/{id}                         计划状态+门禁求值+冲突方
POST   /v1/plans/{id}/approvals               双人复核（proposer/verifier）
POST   /v1/plans/{id}/execution/start         争取执行权（资源仲裁）
POST   /v1/plans/{id}/execution/resolve       登记星上回执（succeeded/failed/aborted）

GET    /v1/cases?kind=&open_only=             处置链
GET    /v1/cases/{id}
POST   /v1/cases/{id}/advance                 续办一步
POST   /v1/cases/{id}/resolve                 手动关单
POST   /v1/cases/{id}/replan                  超时/取消后继办重发

GET    /v1/readiness                          整组每颗星缺什么
GET    /v1/satellites/{id}/readiness
POST   /v1/deliveries                         主管签署（scope=satellite|group，accepted|rejected），冻结证据
GET    /v1/deliveries
GET    /v1/deliveries/{id}                    读取冻结证据包

POST   /v1/maintenance/sweep-timeouts         手动触发超时扫描
```

错误码：`validation`(400)、`not_found`(404)、`conflict`/`state_conflict`(409)、`precondition_failed`(412)。

### 最小流程示例

```bash
# 1) 基础数据
curl -s -X POST localhost:8080/v1/satellites -d '{"satellite_id":"PIESAT-2-1","name":"一号"}'
curl -s -X POST localhost:8080/v1/stations   -d '{"station_id":"TY-GS-01","name":"太原"}'
curl -s -X POST localhost:8080/v1/windows    -d '{"window_id":"w1","satellite_id":"PIESAT-2-1","station_id":"TY-GS-01","start":"<RFC3339>","end":"<RFC3339>","version":1}'

# 2) 遥测（四要素齐全）
curl -s -X POST localhost:8080/v1/telemetry/frames -d '{
  "satellite_id":"PIESAT-2-1","source_id":"bus","source_sequence":1,
  "spacecraft_time":"<RFC3339>","received_at":"<RFC3339>",
  "quality":"verified","data":{"attitude_mode":"nadir","battery_soc":92}}'

# 3) 健康确认 → 指令 → 计划 → 双签 → 执行 → 回执
curl -s -X POST localhost:8080/v1/satellites/PIESAT-2-1/health-confirmations -d '{"operator":"lead"}'
curl -s -X POST localhost:8080/v1/commands -d '{"satellite_id":"PIESAT-2-1","command":"payload_power","created_by":"p","content":{"power":true}}'
# 用返回的 id 创建 /v1/plans（milestone=payload_on），随后：
# POST /v1/plans/{id}/approvals {"role":"proposer","operator":"alice"}
# POST /v1/plans/{id}/approvals {"role":"verifier","operator":"bob"}
# POST /v1/plans/{id}/execution/start   {"operator":"alice"}
# POST /v1/plans/{id}/execution/resolve {"outcome":"succeeded","receipt_status":"ok","receipt_seq":9001}

# 4) 查缺与签署
curl -s localhost:8080/v1/readiness
curl -s -X POST localhost:8080/v1/deliveries -d '{"scope":"group","conclusion":"accepted","signer":"director"}'
```

## 数据目录

- `events.log`：仅追加 JSONL，每行一条带全局单调 `offset` 的事件，追加后 `fsync`。
- `snapshot.json`：周期性快照（临时文件 + `rename` 原子替换）；快照截断日志后 offset 仍全局单调，重启时“快照 + 增量重放”得到完整状态。

## 测试

```bash
go test -race ./...
```

覆盖：乱序/重复帧不倒退、健康确认与里程碑不回退、遥测/安全模式/窗口取消三类批准失效、资源重叠唯一胜出（8 路并发）、双人复核约束、门禁失败明细、超时处置链与重发、安全模式处置链续办、证据冻结不受后续变化影响、整组/单星就绪度与签署、**进程重启（含快照截断）后找回执行中尝试与未关闭处置链**、完整 HTTP 端到端流程。
