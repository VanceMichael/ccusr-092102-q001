package mission_test

import (
	"path/filepath"
	"testing"
	"time"

	"example.com/constellation-handover/internal/mission"
)

const (
	satA = "PIESAT-2-13"
	satB = "PIESAT-2-14"
	gs1  = "TY-GS-01"
	gs2  = "TY-GS-02"
)

var t0 = time.Date(2026, 9, 20, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))

type harness struct {
	svc *mission.Service
	clk *mission.SimClock
	dir string
}

func newHarness(t *testing.T, ttl time.Duration) *harness {
	t.Helper()
	dir := t.TempDir()
	store, _, err := mission.OpenStore(filepath.Join(dir, "mission.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	clk := mission.NewSimClock()
	clk.Set(t0)
	svc, err := mission.NewService(store, clk.Now, mission.Config{LeaseTTL: ttl})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{svc: svc, clk: clk, dir: dir}
}

func (h *harness) at(d time.Duration) time.Time { return t0.Add(d) }
func (h *harness) setClock(d time.Duration)     { h.clk.Set(h.at(d)) }

func (h *harness) register(t *testing.T, sat string) {
	t.Helper()
	if err := h.svc.RegisterSatellite(mission.RegisterSatelliteParams{SatelliteID: sat}); err != nil {
		t.Fatalf("注册卫星失败: %v", err)
	}
}

func (h *harness) window(t *testing.T, id, sat, gs string, start, end time.Duration) {
	t.Helper()
	err := h.svc.ScheduleWindow(mission.ScheduleWindowParams{
		WindowID: id, SatelliteID: sat, StationID: gs,
		Start: h.at(start), End: h.at(end),
	})
	if err != nil {
		t.Fatalf("安排窗口失败: %v", err)
	}
}

// healthy 发一帧姿态/能源均达标的遥测。
func (h *harness) healthy(t *testing.T, sat, src string, seq int64, d time.Duration) {
	t.Helper()
	h.tm(t, sat, src, seq, d, mission.QualityVerified, map[mission.ConditionKey]float64{
		mission.CondAttitude: 95, mission.CondPower: 80,
	}, false)
}

func (h *harness) tm(t *testing.T, sat, src string, seq int64, d time.Duration,
	q mission.TelemetryQuality, r map[mission.ConditionKey]float64, safe bool) *mission.TelemetryFrame {
	t.Helper()
	sc := h.at(d)
	f, err := h.svc.IngestTelemetry(mission.IngestTelemetryParams{
		SatelliteID: sat, SourceID: src, SourceSequence: seq,
		SpacecraftTime: sc, ReceivedAt: sc.Add(2 * time.Second),
		Quality: q, Readings: r, SafeMode: safe,
	})
	if err != nil {
		t.Fatalf("遥测接入失败: %v", err)
	}
	return f
}

func (h *harness) makePlan(t *testing.T, sat, win string, cmd mission.CommandType, by string) *mission.PlanView {
	t.Helper()
	p, err := h.svc.CreatePlan(mission.CreatePlanParams{
		SatelliteID: sat, WindowID: win, CommandType: cmd,
		Payload: "CMD-BODY-v1", CreatedBy: by,
	})
	if err != nil {
		t.Fatalf("创建计划失败: %v", err)
	}
	return p
}

func (h *harness) approve2(t *testing.T, planID string) {
	t.Helper()
	if _, err := h.svc.AddApproval(planID, "primary", "operator-li"); err != nil {
		t.Fatalf("主操作席批准失败: %v", err)
	}
	if _, err := h.svc.AddApproval(planID, "reviewer", "reviewer-wang"); err != nil {
		t.Fatalf("复核席批准失败: %v", err)
	}
}

func (h *harness) submitApprove(t *testing.T, p *mission.PlanView) {
	t.Helper()
	if err := h.svc.SubmitPlan(p.PlanID); err != nil {
		t.Fatalf("提交计划失败: %v", err)
	}
	h.approve2(t, p.PlanID)
}

func (h *harness) execute(t *testing.T, planID, seat string) *mission.Lease {
	t.Helper()
	l, err := h.svc.ExecutePlan(planID, seat)
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	return l
}

func (h *harness) successReceipt(t *testing.T, planID string, d time.Duration) {
	t.Helper()
	sc := h.at(d)
	if _, err := h.svc.RecordReceipt(planID, mission.Receipt{
		Success: true, Code: "OK", SpacecraftTime: sc, ReceivedAt: sc.Add(time.Second),
	}); err != nil {
		t.Fatalf("回执失败: %v", err)
	}
}

// 走完一颗星的三项必做指令，使用同一窗口。
func (h *harness) completeAllChecks(t *testing.T, sat, win string) {
	cmds := []mission.CommandType{
		mission.CmdHealthCheck, mission.CmdPayloadPowerOn, mission.CmdCrossCalibration,
	}
	for i, c := range cmds {
		base := time.Duration(5+i*4) * time.Minute
		p := h.makePlan(t, sat, win, c, "operator-li")
		h.submitApprove(t, p)
		h.setClock(base)
		h.execute(t, p.PlanID, "seat-1")
		h.successReceipt(t, p.PlanID, base+30*time.Second)
		h.setClock(base + time.Minute)
	}
}
