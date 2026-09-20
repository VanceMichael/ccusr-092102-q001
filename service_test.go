package mission

import (
	"testing"
	"time"
)

// fakeClock 为可手动推进的时钟。
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time { return c.t }
func (c *fakeClock) Add(d time.Duration) time.Time {
	c.t = c.t.Add(d)
	return c.t
}

// newTestService 在临时目录上打开服务。
func newTestService(t *testing.T) (*Service, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Date(2026, 9, 20, 1, 0, 0, 0, time.FixedZone("CST", 8*3600))}
	svc, err := OpenService(Config{Dir: t.TempDir(), Now: clk.Now, DefaultTimeout: time.Minute})
	if err != nil {
		t.Fatalf("打开服务失败: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	return svc, clk
}

// bootstrap 注册一颗星、一个站、一个当前窗口并上报一帧已验证遥测。
func bootstrap(t *testing.T, svc *Service, satID string) (string, string) {
	t.Helper()
	now := svc.now()
	if err := svc.RegisterSatellite(satID, "测试星 "+satID); err != nil {
		t.Fatalf("注册卫星: %v", err)
	}
	stationID := "GS-" + satID
	if err := svc.RegisterStation(stationID, "测试站"); err != nil {
		t.Fatalf("注册地面站: %v", err)
	}
	winID := "win-" + satID
	if _, err := svc.ScheduleWindow(ScheduleWindowInput{
		ID: winID, SatelliteID: satID, StationID: stationID,
		Start: now.Add(-time.Minute), End: now.Add(10 * time.Minute), Version: 1,
	}); err != nil {
		t.Fatalf("排窗: %v", err)
	}
	ingestOK(t, svc, FrameInput{
		SatelliteID: satID, SourceID: "bus", SourceSequence: 1,
		SpacecraftTime: now.Add(-2 * time.Second), ReceivedAt: now,
		Quality: QualityVerified, Data: map[string]any{"attitude_mode": "nadir", "battery_soc": 86.0},
	})
	return winID, stationID
}

func ingestOK(t *testing.T, svc *Service, in FrameInput) *IngestResult {
	t.Helper()
	r, err := svc.IngestFrame(in)
	if err != nil {
		t.Fatalf("接入遥测失败: %v", err)
	}
	return r
}

func makePlan(t *testing.T, svc *Service, satID, winID, milestone string, gates ...Gate) *Plan {
	t.Helper()
	cv, err := svc.CreateCommandVersion(CreateCommandInput{
		SatelliteID: satID, Command: "cmd-" + milestone, CreatedBy: "planner",
		Content: map[string]any{"op": milestone, "arg": 1},
	})
	if err != nil {
		t.Fatalf("创建指令版本: %v", err)
	}
	pl, err := svc.CreatePlan(CreatePlanInput{
		SatelliteID: satID, WindowID: winID, CommandVersionID: cv.ID,
		Kind: milestone, Milestone: Milestone(milestone), Gates: gates,
		TimeoutSeconds: 60, CreatedBy: "planner",
	})
	if err != nil {
		t.Fatalf("创建计划: %v", err)
	}
	return pl
}

func approveBoth(t *testing.T, svc *Service, planID string) {
	t.Helper()
	if _, _, err := svc.ApprovePlan(ApprovePlanInput{PlanID: planID, Role: RoleProposer, Operator: "alice"}); err != nil {
		t.Fatalf("提议批准失败: %v", err)
	}
	if _, _, err := svc.ApprovePlan(ApprovePlanInput{PlanID: planID, Role: RoleVerifier, Operator: "bob"}); err != nil {
		t.Fatalf("复核批准失败: %v", err)
	}
}

func TestOutOfOrderFramesNeverRegress(t *testing.T) {
	svc, clk := newTestService(t)
	sat := "PIESAT-2-01"
	if err := svc.RegisterSatellite(sat, "一号星"); err != nil {
		t.Fatal(err)
	}
	now := clk.Now()
	base := func(seq int64, sc time.Time) FrameInput {
		return FrameInput{SatelliteID: sat, SourceID: "bus", SourceSequence: seq,
			SpacecraftTime: sc, ReceivedAt: now, Quality: QualityVerified,
			Data: map[string]any{"v": float64(seq)}}
	}
	// 乱序到达：10 → 9 → 11
	r10 := ingestOK(t, svc, base(10, now.Add(-10*time.Second)))
	if !r10.BecameCurrent {
		t.Fatal("序号 10 应成为当前帧")
	}
	r9 := ingestOK(t, svc, base(9, now.Add(-11*time.Second)))
	if r9.BecameCurrent {
		t.Fatal("迟到的序号 9 不能抢占当前帧——已确认状态不得倒退")
	}
	r11 := ingestOK(t, svc, base(11, now.Add(-9*time.Second)))
	if !r11.BecameCurrent {
		t.Fatal("序号 11 应推进当前帧")
	}
	view, err := svc.GetSatellite(sat)
	if err != nil {
		t.Fatal(err)
	}
	cur := view.CurrentFrames["bus"]
	if cur.SourceSequence != 11 {
		t.Fatalf("当前帧序号 = %d, 期望 11", cur.SourceSequence)
	}
	if view.FrameCount != 3 {
		t.Fatalf("三帧都应留档，实际 %d", view.FrameCount)
	}
	// 重复帧：返回 duplicate，不改变裁决。
	dup := ingestOK(t, svc, base(11, now.Add(-9*time.Second)))
	if !dup.Duplicate || dup.BecameCurrent {
		t.Fatalf("重复帧应去重且不翻转当前版本: %+v", dup)
	}
	// 同序号不同内容必须拒绝（源内序号重用）。
	bad := base(11, now.Add(-9*time.Second))
	bad.Data = map[string]any{"v": 999.0}
	if _, err := svc.IngestFrame(bad); err == nil {
		t.Fatal("同序号不同哈希必须报错")
	}
}

func TestHealthConfirmationSurvivesNewerTelemetry(t *testing.T) {
	svc, clk := newTestService(t)
	sat := "PIESAT-2-02"
	bootstrap(t, svc, sat)
	if _, err := svc.ConfirmHealth(ConfirmHealthInput{SatelliteID: sat, Operator: "lead"}); err != nil {
		t.Fatalf("健康确认失败: %v", err)
	}
	// 更新的遥测到达。
	now := clk.Now()
	ingestOK(t, svc, FrameInput{SatelliteID: sat, SourceID: "bus", SourceSequence: 2,
		SpacecraftTime: now.Add(-time.Second), ReceivedAt: now, Quality: QualityVerified,
		Data: map[string]any{"attitude_mode": "nadir"}})
	r, err := svc.GetReadiness(sat)
	if err != nil {
		t.Fatal(err)
	}
	if r.Health == nil || r.Health.Operator != "lead" {
		t.Fatal("健康确认不能因乱序/更新遥测倒退")
	}
	if !r.Milestones[MilestoneHealth] {
		t.Fatal("健康确认里程碑必须保留")
	}
}

func TestApprovalInvalidatedByTelemetryShift(t *testing.T) {
	svc, clk := newTestService(t)
	sat := "PIESAT-2-03"
	win, _ := bootstrap(t, svc, sat)
	pl := makePlan(t, svc, sat, win, "payload_on", Gate{Type: GateAttitude, Param: map[string]any{"mode": "nadir"}})
	approveBoth(t, svc, pl.ID)

	st, err := svc.CheckPlan(pl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Ready {
		t.Fatal("双签后应可执行")
	}
	// 前置遥测版本推进：批准必须失效。
	now := clk.Now()
	res := ingestOK(t, svc, FrameInput{SatelliteID: sat, SourceID: "bus", SourceSequence: 2,
		SpacecraftTime: now.Add(-time.Second), ReceivedAt: now, Quality: QualityVerified,
		Data: map[string]any{"attitude_mode": "nadir", "battery_soc": 87.0}})
	if len(res.Invalidated) != 2 {
		t.Fatalf("应有 2 个批准失效，实际 %d: %+v", len(res.Invalidated), res.Invalidated)
	}
	st, _ = svc.CheckPlan(pl.ID)
	if st.Ready {
		t.Fatal("依据漂移后计划不得再处于可执行状态")
	}
	if _, _, err := svc.StartExecution(StartExecutionInput{PlanID: pl.ID, Operator: "alice"}); err == nil {
		t.Fatal("失效批准不得取得执行权")
	}
	// 重新双人复核后可执行。
	approveBoth(t, svc, pl.ID)
	if _, _, err := svc.StartExecution(StartExecutionInput{PlanID: pl.ID, Operator: "alice"}); err != nil {
		t.Fatalf("重新复核后应能执行: %v", err)
	}
}

func TestApprovalInvalidatedBySafeMode(t *testing.T) {
	svc, clk := newTestService(t)
	sat := "PIESAT-2-04"
	win, _ := bootstrap(t, svc, sat)
	pl := makePlan(t, svc, sat, win, "payload_on")
	approveBoth(t, svc, pl.ID)
	if _, _, err := svc.StartExecution(StartExecutionInput{PlanID: pl.ID, Operator: "alice"}); err != nil {
		t.Fatal(err)
	}
	c, err := svc.EnterSafeMode(sat, "姿态异常")
	if err != nil {
		t.Fatalf("进入安全模式失败: %v", err)
	}
	if c.Kind != CaseSafeMode || c.State != CaseOpen {
		t.Fatalf("应开立安全模式处置链: %+v", c)
	}
	st, _ := svc.CheckPlan(pl.ID)
	if st.Plan.State != PlanAborted {
		t.Fatalf("执行中计划应被中止，实际 %s", st.Plan.State)
	}
	for _, a := range st.Plan.Approvals {
		if a.Valid {
			t.Fatal("安全模式应使相关高风险批准失效")
		}
	}
	// 处置链可续办：先推进步骤，再解除安全模式并关单。
	if _, err := svc.AdvanceCase(c.ID, "diagnose", "bob", "检查姿态控制回路", nil); err != nil {
		t.Fatal(err)
	}
	clk.Add(time.Second)
	resolved, err := svc.ClearSafeMode(sat, "bob", "已切回主姿态控制器")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != CaseResolved || len(resolved.Steps) != 3 {
		t.Fatalf("解除后处置链应关闭且保留全部续办步骤（系统中止+诊断+解除）: %+v", resolved)
	}
}

func TestApprovalInvalidatedByWindowCancel(t *testing.T) {
	svc, _ := newTestService(t)
	sat := "PIESAT-2-05"
	win, _ := bootstrap(t, svc, sat)
	pl := makePlan(t, svc, sat, win, "payload_on")
	approveBoth(t, svc, pl.ID)
	c, err := svc.CancelWindow(win, "轨道冲突，整体改期")
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != CaseWindowCanceled {
		t.Fatalf("窗口取消应进入取消处置链，实际 %s", c.Kind)
	}
	st, _ := svc.CheckPlan(pl.ID)
	if st.GatesPassed {
		t.Fatal("窗口取消后门禁不得通过")
	}
	for _, a := range st.Plan.Approvals {
		if a.Valid {
			t.Fatal("窗口取消应使依据该窗口的批准失效")
		}
	}
}

func TestResourceOverlapSingleWinner(t *testing.T) {
	svc, _ := newTestService(t)
	satA, satB := "PIESAT-2-06", "PIESAT-2-07"
	winA, station := bootstrap(t, svc, satA)
	// B 星注册到同一地面站，窗口时间重叠。
	if err := svc.RegisterSatellite(satB, "七号星"); err != nil {
		t.Fatal(err)
	}
	now := svc.now()
	winB := "win-" + satB
	if _, err := svc.ScheduleWindow(ScheduleWindowInput{ID: winB, SatelliteID: satB, StationID: station,
		Start: now.Add(-30 * time.Second), End: now.Add(5 * time.Minute), Version: 1}); err != nil {
		t.Fatal(err)
	}
	ingestOK(t, svc, FrameInput{SatelliteID: satB, SourceID: "bus", SourceSequence: 1,
		SpacecraftTime: now.Add(-time.Second), ReceivedAt: now, Quality: QualityVerified,
		Data: map[string]any{"ok": true}})
	plA := makePlan(t, svc, satA, winA, "payload_on")
	plB := makePlan(t, svc, satB, winB, "payload_on")
	approveBoth(t, svc, plA.ID)
	approveBoth(t, svc, plB.ID)

	if _, _, err := svc.StartExecution(StartExecutionInput{PlanID: plA.ID, Operator: "alice"}); err != nil {
		t.Fatalf("A 应取得执行权: %v", err)
	}
	// 两个席位不能抢占同一地面站：B 必须失败。
	if _, _, err := svc.StartExecution(StartExecutionInput{PlanID: plB.ID, Operator: "carol"}); err == nil {
		t.Fatal("地面站资源重叠时只允许一项计划取得执行权")
	}
	if _, err := svc.ResolveExecution(ResolveExecutionInput{PlanID: plA.ID, Outcome: PlanSucceeded,
		ReceiptStatus: "exec_ok", ReceiptSeq: ptr(int64(1001))}); err != nil {
		t.Fatal(err)
	}
	// A 落定释放资源后，B 才能取得执行权。
	if _, _, err := svc.StartExecution(StartExecutionInput{PlanID: plB.ID, Operator: "carol"}); err != nil {
		t.Fatalf("资源释放后 B 应能执行: %v", err)
	}
}

func TestExecutionTimeoutChainAndReplan(t *testing.T) {
	svc, clk := newTestService(t)
	sat := "PIESAT-2-08"
	win, _ := bootstrap(t, svc, sat)
	pl := makePlan(t, svc, sat, win, "cross_cal")
	approveBoth(t, svc, pl.ID)
	if _, _, err := svc.StartExecution(StartExecutionInput{PlanID: pl.ID, Operator: "alice"}); err != nil {
		t.Fatal(err)
	}
	// 超过截止时间。
	clk.Add(61 * time.Second)
	swept, err := svc.SweepTimeouts()
	if err != nil {
		t.Fatal(err)
	}
	if len(swept) != 1 {
		t.Fatalf("应裁定 1 条超时计划，实际 %d", len(swept))
	}
	st, _ := svc.CheckPlan(pl.ID)
	if st.Plan.State != PlanTimedOut {
		t.Fatalf("计划应为 timed_out，实际 %s", st.Plan.State)
	}
	if len(st.ConflictWith) != 0 {
		t.Fatal("超时裁定必须释放资源锁")
	}
	c, err := svc.GetCase(swept[0].CaseID)
	if err != nil || c.State != CaseOpen {
		t.Fatalf("超时处置链应开立且可续办: %v", err)
	}
	// 续办：基于处置链重发计划，需重新双人复核。
	newPlan, c2, err := svc.ReplanFromCase(ReplanFromCaseInput{CaseID: c.ID, Operator: "bob", Note: "下个窗口重发"})
	if err != nil {
		t.Fatal(err)
	}
	if newPlan.State != PlanAwaitingApproval || len(newPlan.Approvals) != 0 {
		t.Fatal("后继计划必须重新双人复核")
	}
	if c2.PlanID == "" {
		t.Fatal("处置链应仍关联原计划")
	}
}

func TestPartnerSafeModeInvalidatesCrossCalApproval(t *testing.T) {
	svc, _ := newTestService(t)
	satA, satB := "PIESAT-2-41", "PIESAT-2-42"
	win, _ := bootstrap(t, svc, satA)
	bootstrap(t, svc, satB)
	cv, err := svc.CreateCommandVersion(CreateCommandInput{SatelliteID: satA, Command: "xcal",
		CreatedBy: "p", Content: map[string]any{"mode": "cross"}})
	if err != nil {
		t.Fatal(err)
	}
	pl, err := svc.CreatePlan(CreatePlanInput{SatelliteID: satA, PartnerSatelliteID: satB,
		WindowID: win, CommandVersionID: cv.ID, Kind: "xcal", Milestone: MilestoneCrossCal,
		Gates:          []Gate{{Type: GateSatelliteNominal, Param: map[string]any{"satellite_id": satB}}},
		TimeoutSeconds: 60, CreatedBy: "p"})
	if err != nil {
		t.Fatal(err)
	}
	approveBoth(t, svc, pl.ID)
	// 伴星进入安全模式：主星计划虽不直接涉及该星资源门禁之外的遥测，批准仍必须失效。
	if _, err := svc.EnterSafeMode(satB, "伴星电源异常"); err != nil {
		t.Fatal(err)
	}
	st, _ := svc.CheckPlan(pl.ID)
	for _, a := range st.Plan.Approvals {
		if a.Valid {
			t.Fatal("伴星进入安全模式必须使交叉标定批准失效")
		}
	}
	if st.GatesPassed {
		t.Fatal("伴星安全模式后门禁不得通过")
	}
}

func TestSameOperatorCannotFillBothSeats(t *testing.T) {
	svc, _ := newTestService(t)
	sat := "PIESAT-2-09"
	win, _ := bootstrap(t, svc, sat)
	pl := makePlan(t, svc, sat, win, "payload_on")
	if _, _, err := svc.ApprovePlan(ApprovePlanInput{PlanID: pl.ID, Role: RoleProposer, Operator: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.ApprovePlan(ApprovePlanInput{PlanID: pl.ID, Role: RoleVerifier, Operator: "alice"}); err == nil {
		t.Fatal("同一人不得占据两个复核席位")
	}
}

func TestGateFailuresBlockApproval(t *testing.T) {
	svc, clk := newTestService(t)
	sat := "PIESAT-2-10"
	win, _ := bootstrap(t, svc, sat)
	pl := makePlan(t, svc, sat, win, "payload_on",
		Gate{Type: GateAttitude, Param: map[string]any{"mode": "sun_point"}},
		Gate{Type: GateBattery, Param: map[string]any{"min": 95.0}},
		Gate{Type: GateTelemetryFresh, Param: map[string]any{"max_age_seconds": 1.0}},
	)
	clk.Add(5 * time.Second)
	_, gates, err := svc.ApprovePlan(ApprovePlanInput{PlanID: pl.ID, Role: RoleProposer, Operator: "alice"})
	if err == nil {
		t.Fatal("门禁不满足时不得批准")
	}
	if ae, ok := IsAPIError(err); !ok || ae.Code != CodePrecondition {
		t.Fatalf("应返回前置条件错误，实际 %v", err)
	}
	// 前两项为隐式门禁（窗口有效、卫星未安全模式），随后是三项显式门禁，全部失败在显式三项。
	if len(gates) != 5 || !gates[0].Passed || !gates[1].Passed ||
		gates[2].Passed || gates[3].Passed || gates[4].Passed {
		t.Fatalf("隐式门禁应通过、三项显式门禁应失败并给出明细: %+v", gates)
	}
}

func TestDeliveryFreezesEvidence(t *testing.T) {
	svc, clk := newTestService(t)
	sat := "PIESAT-2-11"
	win, _ := bootstrap(t, svc, sat)
	if _, err := svc.ConfirmHealth(ConfirmHealthInput{SatelliteID: sat, Operator: "lead"}); err != nil {
		t.Fatal(err)
	}
	// 载荷开机计划成功。
	pl := makePlan(t, svc, sat, win, "payload_on")
	approveBoth(t, svc, pl.ID)
	if _, _, err := svc.StartExecution(StartExecutionInput{PlanID: pl.ID, Operator: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveExecution(ResolveExecutionInput{PlanID: pl.ID, Outcome: PlanSucceeded,
		ReceiptStatus: "payload_on_ok", ReceiptSeq: ptr(int64(2001))}); err != nil {
		t.Fatal(err)
	}
	// 交叉标定计划成功（指定伴星）。
	partner := "PIESAT-2-12"
	bootstrap(t, svc, partner)
	cv, err := svc.CreateCommandVersion(CreateCommandInput{SatelliteID: sat, Command: "xcal", CreatedBy: "planner",
		Content: map[string]any{"mode": "cross"}})
	if err != nil {
		t.Fatal(err)
	}
	pl2, err := svc.CreatePlan(CreatePlanInput{SatelliteID: sat, PartnerSatelliteID: partner,
		WindowID: win, CommandVersionID: cv.ID, Kind: "xcal", Milestone: MilestoneCrossCal,
		Gates:          []Gate{{Type: GateSatelliteNominal, Param: map[string]any{"satellite_id": partner}}},
		TimeoutSeconds: 60, CreatedBy: "planner"})
	if err != nil {
		t.Fatal(err)
	}
	approveBoth(t, svc, pl2.ID)
	if _, _, err := svc.StartExecution(StartExecutionInput{PlanID: pl2.ID, Operator: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResolveExecution(ResolveExecutionInput{PlanID: pl2.ID, Outcome: PlanSucceeded,
		ReceiptStatus: "xcal_ok"}); err != nil {
		t.Fatal(err)
	}
	rec, err := svc.SignDelivery(SignDeliveryInput{Scope: "satellite", SatelliteID: sat,
		Conclusion: DeliveryAccepted, Signer: "director", Note: "单星验收通过"})
	if err != nil {
		t.Fatalf("全部里程碑完成后应能签署接收: %v", err)
	}
	frozenHash := rec.Bundle.Hash
	frozenReceipts := len(rec.Bundle.Satellites[0].Receipts)

	// 签署后世界继续变化：新遥测、安全模式、新指令版本。
	now := clk.Now()
	ingestOK(t, svc, FrameInput{SatelliteID: sat, SourceID: "bus", SourceSequence: 9,
		SpacecraftTime: now, ReceivedAt: now, Quality: QualityVerified, Data: map[string]any{"v": 9.0}})
	if _, err := svc.CreateCommandVersion(CreateCommandInput{SatelliteID: sat, Command: "xcal",
		CreatedBy: "planner", Content: map[string]any{"mode": "cross", "rev": 2}}); err != nil {
		t.Fatal(err)
	}
	again, err := svc.GetDelivery(rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Bundle.Hash != frozenHash {
		t.Fatal("签署证据必须冻结，不受后续状态变化影响")
	}
	if len(again.Bundle.Satellites[0].Receipts) != frozenReceipts {
		t.Fatal("冻结证据中的回执数量不得变化")
	}
	se := again.Bundle.Satellites[0]
	// 能回答“谁批准过哪版指令”。
	var foundApprover bool
	for _, cve := range se.CommandVersions {
		for _, a := range cve.Approvals {
			if a.Operator == "alice" && a.Basis.CommandHash != "" {
				foundApprover = true
			}
		}
	}
	if !foundApprover {
		t.Fatal("冻结证据应能回答谁批准过哪版指令（含指令哈希）")
	}
	// 能回答“实际执行结果”。
	if len(se.Receipts) != 2 || se.Receipts[0].State != PlanSucceeded {
		t.Fatalf("冻结证据应包含两次成功执行的实际回执: %+v", se.Receipts)
	}
}

func TestGroupDeliveryReportsMissingPerSatellite(t *testing.T) {
	svc, _ := newTestService(t)
	for _, id := range []string{"PIESAT-2-13", "PIESAT-2-14"} {
		bootstrap(t, svc, id)
	}
	// 整组尚有缺失，不能接收。
	_, err := svc.SignDelivery(SignDeliveryInput{Scope: "group", Conclusion: DeliveryAccepted, Signer: "director"})
	if err == nil {
		t.Fatal("存在缺失项时整组不得签署接收")
	}
	rs := svc.ListGroupReadiness()
	if len(rs) != 2 {
		t.Fatalf("应返回两颗星就绪度，实际 %d", len(rs))
	}
	for _, r := range rs {
		if r.Ready || len(r.Missing) == 0 {
			t.Fatalf("未完成任何里程碑的星 %s 应列出缺失项", r.SatelliteID)
		}
	}
	// 任何时候都可以拒收并冻结证据。
	rec, err := svc.SignDelivery(SignDeliveryInput{Scope: "group", Conclusion: DeliveryRejected,
		Signer: "director", Note: "窗口不足，待续办"})
	if err != nil {
		t.Fatalf("拒收签署应允许: %v", err)
	}
	if len(rec.Bundle.Satellites) != 2 {
		t.Fatal("整组拒收证据也应覆盖每颗星")
	}
}

func ptr[T any](v T) *T { return &v }
