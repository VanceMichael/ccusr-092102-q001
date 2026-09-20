package mission

import (
	"testing"
	"time"
)

// TestRestartRecoversUnfinishedWork 验证进程重启后：
//   - 全部注册/遥测/计划/批准状态可从事件日志找回；
//   - 执行中但未回执的尝试仍在，超时扫描能找回并裁定、开立处置链；
//   - 未关闭处置链仍可续办。
func TestRestartRecoversUnfinishedWork(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 9, 20, 3, 0, 0, 0, time.FixedZone("CST", 8*3600))}
	open := func() *Service {
		svc, err := OpenService(Config{Dir: dir, Now: clk.Now, DefaultTimeout: time.Minute})
		if err != nil {
			t.Fatalf("重开服务失败: %v", err)
		}
		return svc
	}

	svc := open()
	sat := "PIESAT-2-21"
	win, _ := bootstrap(t, svc, sat)
	if _, err := svc.ConfirmHealth(ConfirmHealthInput{SatelliteID: sat, Operator: "lead"}); err != nil {
		t.Fatal(err)
	}
	pl := makePlan(t, svc, sat, win, "payload_on")
	approveBoth(t, svc, pl.ID)
	if _, _, err := svc.StartExecution(StartExecutionInput{PlanID: pl.ID, Operator: "alice"}); err != nil {
		t.Fatal(err)
	}
	// 另开一条安全模式处置链，留作未关闭。
	sat2 := "PIESAT-2-22"
	bootstrap(t, svc, sat2)
	c2, err := svc.EnterSafeMode(sat2, "电源异常")
	if err != nil {
		t.Fatal(err)
	}
	planID := pl.ID
	caseID2 := c2.ID
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}

	// 重启：状态全部来自日志重放。
	svc = open()
	defer func() { _ = svc.Close() }()
	st, err := svc.CheckPlan(planID)
	if err != nil {
		t.Fatalf("重启后找不到执行中计划: %v", err)
	}
	if st.Plan.State != PlanExecuting || len(st.Plan.Approvals) != 2 {
		t.Fatalf("重启后计划状态/双签丢失: %+v", st.Plan.State)
	}
	if len(st.Plan.Attempts) != 1 || st.Plan.Attempts[0].Operator != "alice" {
		t.Fatal("重启后必须找回未完成的执行尝试")
	}
	// 遥测也找回。
	view, err := svc.GetSatellite(sat)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.CurrentFrames) != 1 {
		t.Fatal("重启后当前帧裁决丢失")
	}

	// 时钟越过截止时间，扫描找回未完成工作。
	clk.Add(2 * time.Minute)
	swept, err := svc.SweepTimeouts()
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 1 || swept[0].PlanID != planID {
		t.Fatalf("重启后超时扫描必须找回未完成尝试: %+v", swept)
	}
	// 安全模式处置链仍可续办。
	if _, err := svc.AdvanceCase(caseID2, "power_cycle", "bob", "重启电源控制器", nil); err != nil {
		t.Fatalf("重启后处置链应可续办: %v", err)
	}
	resolved, err := svc.ClearSafeMode(sat2, "bob", "电源恢复")
	if err != nil {
		t.Fatalf("重启后应能解除安全模式: %v", err)
	}
	if resolved.State != CaseResolved {
		t.Fatal("处置链应关闭")
	}
}

// TestRestartWithSnapshot 验证快照截断日志后重启仍正确恢复，且 offset 全局单调。
func TestRestartWithSnapshot(t *testing.T) {
	dir := t.TempDir()
	clk := &fakeClock{t: time.Date(2026, 9, 20, 4, 0, 0, 0, time.FixedZone("CST", 8*3600))}
	open := func(snap int) *Service {
		svc, err := OpenService(Config{Dir: dir, Now: clk.Now, SnapshotEvery: snap, DefaultTimeout: time.Minute})
		if err != nil {
			t.Fatalf("打开服务失败: %v", err)
		}
		return svc
	}
	svc := open(5)
	for i := 0; i < 12; i++ {
		sat := "PIESAT-2-3" + string(rune('0'+i))
		if err := svc.RegisterSatellite(sat, "星"); err != nil {
			t.Fatal(err)
		}
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	svc = open(5)
	defer func() { _ = svc.Close() }()
	if len(svc.ListSatellites()) != 12 {
		t.Fatalf("快照+日志重放后应恢复 12 颗星，实际 %d", len(svc.ListSatellites()))
	}
	// 快照截断后新事件 offset 仍单调、可再快照。
	if err := svc.RegisterSatellite("PIESAT-2-99", "新星"); err != nil {
		t.Fatalf("快照后追加事件失败: %v", err)
	}
	if err := svc.RegisterStation("GS-99", "新站"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	svc2, err := OpenService(Config{Dir: dir, Now: clk.Now})
	if err != nil {
		t.Fatalf("二次重启失败: %v", err)
	}
	if len(svc2.ListSatellites()) != 13 || len(svc2.ListStations()) != 1 {
		t.Fatalf("多次快照重启后状态不一致: sats=%d stations=%d",
			len(svc2.ListSatellites()), len(svc2.ListStations()))
	}
	_ = svc2.Close()
}
