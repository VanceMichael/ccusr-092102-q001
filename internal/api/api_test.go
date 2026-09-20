package api_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"example.com/constellation-handover/internal/api"
	"example.com/constellation-handover/internal/mission"
)

func setup(t *testing.T) (*api.Server, func()) {
	t.Helper()
	dir := t.TempDir()
	store, _, err := mission.OpenStore(filepath.Join(dir, "mission.log"))
	if err != nil {
		t.Fatal(err)
	}
	clk := mission.NewSimClock()
	base := time.Date(2026, 9, 20, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))
	clk.Set(base)
	svc, err := mission.NewService(store, clk.Now, mission.Config{LeaseTTL: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	srv := &api.Server{Svc: svc, Clk: clk, SimMode: true}
	return srv, func() { store.Close() }
}

func do(t *testing.T, h http.Handler, method, path string, body any, simAt string) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rdr = bytes.NewReader(raw)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if simAt != "" {
		req.Header.Set("X-Sim-At", simAt)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec.Code, out
}

// 端到端走通：注册 → 遥测 → 窗口 → 计划 → 双签 → 执行 → 回执 → 缺口清零。
func TestEndToEndWorkflow(t *testing.T) {
	srv, cleanup := setup(t)
	defer cleanup()
	h := srv.NewRouter()

	must := func(code int, got int) {
		t.Helper()
		if got != code {
			t.Fatalf("期望状态码 %d，实际 %d", code, got)
		}
	}
	simBase := time.Date(2026, 9, 20, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))
	sim := func(min int) string {
		return simBase.Add(time.Duration(min) * time.Minute).Format(time.RFC3339)
	}

	must(201, func() int {
		c, _ := do(t, h, "POST", "/api/satellites", map[string]any{"satellite_id": "S1"}, "")
		return c
	}())
	must(202, func() int {
		c, _ := do(t, h, "POST", "/api/satellites/S1/telemetry", map[string]any{
			"source_id": "src1", "source_sequence": 1,
			"spacecraft_time": sim(0), "received_at": sim(0),
			"quality":  "verified",
			"readings": map[string]float64{"attitude": 95, "power": 80},
		}, "")
		return c
	}())
	must(201, func() int {
		c, _ := do(t, h, "POST", "/api/windows", map[string]any{
			"window_id": "W1", "satellite_id": "S1", "station_id": "GS1",
			"start": sim(0), "end": sim(30),
		}, "")
		return c
	}())

	c, plan := do(t, h, "POST", "/api/plans", map[string]any{
		"satellite_id": "S1", "window_id": "W1",
		"command_type": "health_check", "payload": "BODY", "created_by": "li",
	}, "")
	must(201, c)
	planID := plan["plan_id"].(string)

	must(200, func() int { c, _ := do(t, h, "POST", "/api/plans/"+planID+"/submit", nil, ""); return c }())
	must(201, func() int {
		c, _ := do(t, h, "POST", "/api/plans/"+planID+"/approvals",
			map[string]string{"role": "primary", "approver": "li"}, "")
		return c
	}())
	must(201, func() int {
		c, _ := do(t, h, "POST", "/api/plans/"+planID+"/approvals",
			map[string]string{"role": "reviewer", "approver": "wang"}, "")
		return c
	}())

	// 窗口未开始，执行应 412
	c, _ = do(t, h, "POST", "/api/plans/"+planID+"/execute", map[string]string{"seat": "A"}, sim(-5))
	must(412, c)

	// 用 X-Sim-At 推进到窗口内执行
	c, execResp := do(t, h, "POST", "/api/plans/"+planID+"/execute", map[string]string{"seat": "A"}, sim(1))
	must(200, c)
	if execResp["lease"] == nil {
		t.Fatal("应返回执行权租约")
	}

	// 同站抢占第二个计划应 409
	c, plan2 := do(t, h, "POST", "/api/plans", map[string]any{
		"satellite_id": "S1", "window_id": "W1",
		"command_type": "payload_power_on", "payload": "BODY2", "created_by": "zhao",
	}, sim(1))
	must(201, c)
	pid2 := plan2["plan_id"].(string)
	do(t, h, "POST", "/api/plans/"+pid2+"/submit", nil, sim(1))
	do(t, h, "POST", "/api/plans/"+pid2+"/approvals", map[string]string{"role": "primary", "approver": "zhao"}, sim(1))
	do(t, h, "POST", "/api/plans/"+pid2+"/approvals", map[string]string{"role": "reviewer", "approver": "qian"}, sim(1))
	c, _ = do(t, h, "POST", "/api/plans/"+pid2+"/execute", map[string]string{"seat": "B"}, sim(1))
	must(409, c)

	// 回执成功
	c, _ = do(t, h, "POST", "/api/plans/"+planID+"/receipt", map[string]any{
		"success": true, "spacecraft_time": sim(1), "received_at": sim(1),
	}, sim(1))
	must(200, c)

	// 卫星缺口仍包含另外两项必做
	c, sat := do(t, h, "GET", "/api/satellites/S1", nil, "")
	must(200, c)
	gaps := sat["gaps"].([]any)
	if len(gaps) != 2 {
		t.Fatalf("完成健康确认后应剩 2 项缺口，实际 %d: %v", len(gaps), gaps)
	}
}

// 演练时钟推进触发超时处置链。
func TestTimeoutOverHTTP(t *testing.T) {
	srv, cleanup := setup(t)
	defer cleanup()
	h := srv.NewRouter()
	simBase := time.Date(2026, 9, 20, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))
	sim := func(min int) string {
		return simBase.Add(time.Duration(min) * time.Minute).Format(time.RFC3339)
	}

	do(t, h, "POST", "/api/satellites", map[string]any{"satellite_id": "S1"}, "")
	do(t, h, "POST", "/api/satellites/S1/telemetry", map[string]any{
		"source_id": "s", "source_sequence": 1,
		"spacecraft_time": sim(0), "received_at": sim(0),
		"quality":  "verified",
		"readings": map[string]float64{"attitude": 95, "power": 80},
	}, "")
	do(t, h, "POST", "/api/windows", map[string]any{
		"window_id": "W1", "satellite_id": "S1", "station_id": "GS1",
		"start": sim(0), "end": sim(30),
	}, "")
	_, plan := do(t, h, "POST", "/api/plans", map[string]any{
		"satellite_id": "S1", "window_id": "W1",
		"command_type": "health_check", "payload": "B", "created_by": "li",
	}, "")
	pid := plan["plan_id"].(string)
	do(t, h, "POST", "/api/plans/"+pid+"/submit", nil, sim(1))
	do(t, h, "POST", "/api/plans/"+pid+"/approvals", map[string]string{"role": "primary", "approver": "li"}, sim(1))
	do(t, h, "POST", "/api/plans/"+pid+"/approvals", map[string]string{"role": "reviewer", "approver": "wang"}, sim(1))
	do(t, h, "POST", "/api/plans/"+pid+"/execute", map[string]string{"seat": "A"}, sim(1))

	// 推进到 TTL（2 分钟）之后
	do(t, h, "POST", "/api/admin/sweep", nil, sim(4))

	c, got := do(t, h, "GET", "/api/plans/"+pid, nil, "")
	if c != 200 {
		t.Fatalf("查询计划失败: %d", c)
	}
	if got["status"] != "awaiting" {
		t.Fatalf("超时后应为 awaiting，实际 %v", got["status"])
	}

	// 处置链列表为顶层 JSON 数组
	req := httptest.NewRequest("GET", "/api/issues?type=timeout&open=1", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("查询处置链失败: %d", rec.Code)
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0]["type"] != "timeout" {
		t.Fatalf("应有 1 条开启的超时处置链，实际 %v", rec.Body.String())
	}
}

func TestBadInputMapping(t *testing.T) {
	srv, cleanup := setup(t)
	defer cleanup()
	h := srv.NewRouter()
	// 未知卫星
	c, body := do(t, h, "GET", "/api/satellites/NOPE", nil, "")
	if c != 404 {
		t.Fatalf("未知卫星应 404，实际 %d %v", c, body)
	}
	// 错误时间头
	req := httptest.NewRequest("GET", "/health", nil)
	req.Header.Set("X-Sim-At", "not-a-time")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("错误 X-Sim-At 应 400，实际 %d", rec.Code)
	}
}
