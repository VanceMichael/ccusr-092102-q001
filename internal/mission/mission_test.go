package mission_test

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"example.com/constellation-handover/internal/mission"
)

// 1. 乱序/重复遥测不得让已确认状态倒退。
func TestOutOfOrderTelemetryDoesNotRegress(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.register(t, satA)

	// 正常到达：序号 10，姿态 95（达标）
	h.healthy(t, satA, "SRC-1", 10, 10*time.Second)
	sv, err := h.svc.GetSatellite(satA)
	if err != nil {
		t.Fatal(err)
	}
	if !sv.Conditions[mission.CondAttitude].OK {
		t.Fatal("序号10应使姿态达标")
	}
	if got := sv.Conditions[mission.CondAttitude].Value; got != 95 {
		t.Fatalf("姿态值 = %v, 期望 95", got)
	}

	// 乱序到达：序号 9，姿态 10（不达标）——必须被存档但不采用
	f := h.tm(t, satA, "SRC-1", 9, 5*time.Second, mission.QualityVerified,
		map[mission.ConditionKey]float64{mission.CondAttitude: 10, mission.CondPower: 80}, false)
	if f.Applied {
		t.Fatalf("乱序帧不应被采用，原因: %q", f.StaleReason)
	}
	sv, _ = h.svc.GetSatellite(satA)
	if !sv.Conditions[mission.CondAttitude].OK || sv.Conditions[mission.CondAttitude].Value != 95 {
		t.Fatal("乱序帧导致已确认状态倒退")
	}

	// 重复帧
	f2 := h.tm(t, satA, "SRC-1", 10, 10*time.Second, mission.QualityVerified,
		map[mission.ConditionKey]float64{mission.CondAttitude: 95, mission.CondPower: 80}, false)
	if f2.Applied {
		t.Fatal("重复帧不应被采用")
	}

	// 新序号但星上时间更旧（迟到数据）——同样不回退
	f3 := h.tm(t, satA, "SRC-1", 11, 8*time.Second, mission.QualityVerified,
		map[mission.ConditionKey]float64{mission.CondAttitude: 5, mission.CondPower: 80}, false)
	if f3.Applied {
		t.Fatal("星上时间更旧的帧不应回退结论")
	}
	sv, _ = h.svc.GetSatellite(satA)
	if !sv.Conditions[mission.CondAttitude].OK {
		t.Fatal("迟到旧帧导致状态倒退")
	}

	// 全部帧仍可追溯
	frames, err := h.svc.ListTelemetry(satA)
	if err != nil || len(frames) != 4 {
		t.Fatalf("应收档 4 帧，实际 %d (err=%v)", len(frames), err)
	}
}

// 2. 质量标记为 bad 的帧永不参与结论。
func TestBadQualityFrameNeverApplied(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.register(t, satA)
	f := h.tm(t, satA, "SRC-1", 1, time.Second, mission.QualityBad,
		map[mission.ConditionKey]float64{mission.CondAttitude: 5}, false)
	if f.Applied {
		t.Fatal("bad 帧不应被采用")
	}
	sv, _ := h.svc.GetSatellite(satA)
	if sv.Conditions[mission.CondAttitude].Evidence != nil {
		t.Fatal("bad 帧不得成为依据")
	}
}

//  3. 同序号下，更新的遥测（仍达标）只换依据、不失效批准；
//     而结论翻转（不达标）必须失效高风险批准。
func TestPreconditionChangeInvalidatesApprovals(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.register(t, satA)
	h.healthy(t, satA, "SRC-1", 10, 0)
	h.window(t, "W1", satA, gs1, 30*time.Second, 20*time.Minute)
	p := h.makePlan(t, satA, "W1", mission.CmdPayloadPowerOn, "li")
	h.submitApprove(t, p)
	pv, _ := h.svc.GetPlan(p.PlanID)
	if pv.Status != mission.PlanApproved || len(pv.Approvals) != 2 {
		t.Fatalf("双签后应为 approved，实际 %s", pv.Status)
	}

	// 同序号下新帧仍达标（值变化但结论 ok）：批准应保持有效
	h.tm(t, satA, "SRC-1", 11, 20*time.Second, mission.QualityVerified,
		map[mission.ConditionKey]float64{mission.CondAttitude: 99, mission.CondPower: 88}, false)
	pv, _ = h.svc.GetPlan(p.PlanID)
	if pv.Status != mission.PlanApproved {
		t.Fatalf("结论未翻转时批准不应失效，实际状态 %s", pv.Status)
	}

	// 新帧姿态跌破门限：结论翻转，批准必须失效
	h.tm(t, satA, "SRC-1", 12, 40*time.Second, mission.QualityVerified,
		map[mission.ConditionKey]float64{mission.CondAttitude: 10, mission.CondPower: 88}, false)
	pv, _ = h.svc.GetPlan(p.PlanID)
	if pv.Status != mission.PlanSubmitted {
		t.Fatalf("前置条件变化后计划应退回 submitted，实际 %s", pv.Status)
	}
	for _, a := range pv.Approvals {
		if !a.Invalidated {
			t.Fatal("既有批准必须被标记失效")
		}
		if a.InvalidReason == "" {
			t.Fatal("失效必须留痕原因")
		}
	}

	// 条件不满足时重新批准应被拒绝
	_, err := h.svc.AddApproval(p.PlanID, "primary", "operator-li")
	if !errors.Is(err, mission.ErrPrecondition) {
		t.Fatalf("条件不满足时批准应返回前置条件错误，实际 %v", err)
	}

	// 恢复达标后可重新双签
	h.healthy(t, satA, "SRC-1", 13, 60*time.Second)
	h.approve2(t, p.PlanID)
	pv, _ = h.svc.GetPlan(p.PlanID)
	if pv.Status != mission.PlanApproved {
		t.Fatalf("重新双签后应 approved，实际 %s", pv.Status)
	}
}

// 4. 指令版本重订必须使旧版批准失效。
func TestPlanRevisionInvalidatesApprovals(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.register(t, satA)
	h.healthy(t, satA, "SRC-1", 1, 0)
	h.window(t, "W1", satA, gs1, 30*time.Second, 20*time.Minute)
	p := h.makePlan(t, satA, "W1", mission.CmdHealthCheck, "li")
	h.submitApprove(t, p)

	if err := h.svc.RevisePlan(p.PlanID, "CMD-BODY-v2", ""); err != nil {
		t.Fatal(err)
	}
	pv, _ := h.svc.GetPlan(p.PlanID)
	if pv.Revision != 2 {
		t.Fatalf("版本号应为 2，实际 %d", pv.Revision)
	}
	if pv.Status != mission.PlanSubmitted {
		t.Fatalf("重订后应退回 submitted，实际 %s", pv.Status)
	}
	for _, a := range pv.Approvals {
		if a.PlanRevision == 1 && !a.Invalidated {
			t.Fatal("r1 批准必须随版本重订失效")
		}
	}
}

// 5. 双人复核：同一人不能批两次，同一席位不能批两次。
func TestTwoPersonRule(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.register(t, satA)
	h.healthy(t, satA, "SRC-1", 1, 0)
	h.window(t, "W1", satA, gs1, 30*time.Second, 20*time.Minute)
	p := h.makePlan(t, satA, "W1", mission.CmdHealthCheck, "li")
	if err := h.svc.SubmitPlan(p.PlanID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.AddApproval(p.PlanID, "primary", "li"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.AddApproval(p.PlanID, "reviewer", "li"); !errors.Is(err, mission.ErrConflict) {
		t.Fatalf("同一人双签应冲突，实际 %v", err)
	}
	if _, err := h.svc.AddApproval(p.PlanID, "primary", "wang"); !errors.Is(err, mission.ErrConflict) {
		t.Fatalf("同一席位重复批准应冲突，实际 %v", err)
	}
	if _, err := h.svc.AddApproval(p.PlanID, "reviewer", "wang"); err != nil {
		t.Fatal(err)
	}
	pv, _ := h.svc.GetPlan(p.PlanID)
	if pv.Status != mission.PlanApproved {
		t.Fatalf("双签后应 approved，实际 %s", pv.Status)
	}
}

// 6. 两个席位不能抢占同一颗卫星或同一地面站：只有一个计划取得执行权。
func TestResourceLeaseMutualExclusion(t *testing.T) {
	h := newHarness(t, 5*time.Minute)
	h.register(t, satA)
	h.register(t, satB)
	h.healthy(t, satA, "SA", 1, 0)
	h.healthy(t, satB, "SB", 1, 0)
	h.window(t, "WA", satA, gs1, 30*time.Second, 20*time.Minute)
	h.window(t, "WB", satB, gs1, 30*time.Second, 20*time.Minute) // 同站、不同星
	h.window(t, "WC", satB, gs2, 30*time.Second, 20*time.Minute)

	pa := h.makePlan(t, satA, "WA", mission.CmdHealthCheck, "li")
	pb := h.makePlan(t, satB, "WB", mission.CmdHealthCheck, "wang")
	h.submitApprove(t, pa)
	h.submitApprove(t, pb)

	h.setClock(time.Minute)
	h.execute(t, pa.PlanID, "seat-A")
	// pb 与 pa 争同一地面站 -> 拒绝
	_, err := h.svc.ExecutePlan(pb.PlanID, "seat-B")
	if !errors.Is(err, mission.ErrConflict) {
		t.Fatalf("同地面站抢占应冲突，实际 %v", err)
	}

	// 另一颗星 B 在另一地面站的计划仍可执行（证明只是资源互斥而非全局锁）
	pc := h.makePlan(t, satB, "WC", mission.CmdPayloadPowerOn, "wang")
	h.submitApprove(t, pc)
	h.execute(t, pc.PlanID, "seat-B")

	// pa 未释放前，同星再取执行权也必须冲突（换一个同站窗口的新计划）
	pd := h.makePlan(t, satA, "WA", mission.CmdPayloadPowerOn, "li")
	h.submitApprove(t, pd)
	_, err = h.svc.ExecutePlan(pd.PlanID, "seat-A")
	if !errors.Is(err, mission.ErrConflict) {
		t.Fatalf("同卫星抢占应冲突，实际 %v", err)
	}

	// 成功回执释放资源后，pd 可执行
	h.successReceipt(t, pa.PlanID, 70*time.Second)
	h.execute(t, pd.PlanID, "seat-A")
}

// 7. 执行超时进入可续办处置链。
func TestTimeoutEntersResumableChain(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.register(t, satA)
	h.healthy(t, satA, "S", 1, 0)
	h.window(t, "W1", satA, gs1, 30*time.Second, 30*time.Minute)
	p := h.makePlan(t, satA, "W1", mission.CmdHealthCheck, "li")
	h.submitApprove(t, p)
	h.setClock(time.Minute)
	lease := h.execute(t, p.PlanID, "seat-1")

	// 超过截止时间未回执，巡检转入超时处置链
	h.setClock(time.Duration(lease.Deadline.Sub(t0) + time.Second))
	if err := h.svc.Sweep(); err != nil {
		t.Fatal(err)
	}
	pv, _ := h.svc.GetPlan(p.PlanID)
	if pv.Status != mission.PlanAwaiting {
		t.Fatalf("超时后应为 awaiting，实际 %s", pv.Status)
	}
	if len(pv.IssueIDs) != 1 {
		t.Fatalf("应开启 1 条处置链，实际 %d", len(pv.IssueIDs))
	}
	issue, _ := h.svc.GetIssue(pv.IssueIDs[0])
	if issue.Type != mission.IssueTimeout || issue.Status != mission.IssueOpen {
		t.Fatalf("处置链类型/状态错误: %s/%s", issue.Type, issue.Status)
	}

	// 处置留痕（值班交接）
	if err := h.svc.WorkIssue(issue.IssueID, "zhang", "已联系测控站核查链路"); err != nil {
		t.Fatal(err)
	}

	// 续办需指定新窗口；不指定应被拒绝
	err := h.svc.ResolveIssue(mission.ResolveIssueParams{
		IssueID: issue.IssueID, By: "zhang", Resolution: "续办",
	})
	if !errors.Is(err, mission.ErrPrecondition) {
		t.Fatalf("超时链续办无新窗口应拒绝，实际 %v", err)
	}

	h.window(t, "W2", satA, gs2, 10*time.Minute, 40*time.Minute)
	if err := h.svc.ResolveIssue(mission.ResolveIssueParams{
		IssueID: issue.IssueID, By: "zhang", Resolution: "改挂 W2 重新复核",
		NewWindowID: "W2",
	}); err != nil {
		t.Fatal(err)
	}
	issue, _ = h.svc.GetIssue(issue.IssueID)
	if issue.Status != mission.IssueResolved {
		t.Fatalf("处置链应已闭环，实际 %s", issue.Status)
	}
	pv, _ = h.svc.GetPlan(p.PlanID)
	if pv.WindowID != "W2" || pv.Status != mission.PlanSubmitted {
		t.Fatalf("续办后应改挂 W2 并回到 submitted，实际 %s/%s", pv.WindowID, pv.Status)
	}
	for _, a := range pv.Approvals {
		if !a.Invalidated {
			t.Fatal("超时后续办，旧批准应失效，需重新双签")
		}
	}
}

// 8. 执行期间进入安全模式：撤销执行权并开安全模式处置链；退出确认后才可续办。
func TestSafeModeChain(t *testing.T) {
	h := newHarness(t, 5*time.Minute)
	h.register(t, satA)
	h.healthy(t, satA, "S", 1, 0)
	h.window(t, "W1", satA, gs1, 30*time.Second, 30*time.Minute)
	p := h.makePlan(t, satA, "W1", mission.CmdPayloadPowerOn, "li")
	h.submitApprove(t, p)
	h.setClock(time.Minute)
	h.execute(t, p.PlanID, "seat-1")

	// 安全模式遥测
	h.tm(t, satA, "S", 2, 65*time.Second, mission.QualityVerified,
		map[mission.ConditionKey]float64{mission.CondAttitude: 95, mission.CondPower: 80}, true)

	pv, _ := h.svc.GetPlan(p.PlanID)
	if pv.Status != mission.PlanAwaiting {
		t.Fatalf("安全模式应撤销执行权，实际 %s", pv.Status)
	}
	if pv.ActiveLease != nil && !pv.ActiveLease.Released {
		t.Fatal("执行权租约应已释放")
	}
	var issueID string
	for _, i := range h.svc.ListIssues(mission.IssueSafeMode, true) {
		if i.PlanID == p.PlanID {
			issueID = i.IssueID
		}
	}
	if issueID == "" {
		t.Fatal("应开启安全模式处置链")
	}

	// 未退出安全模式不得续办
	err := h.svc.ResolveIssue(mission.ResolveIssueParams{IssueID: issueID, By: "z"})
	if !errors.Is(err, mission.ErrPrecondition) {
		t.Fatalf("安全模式未退出不得续办，实际 %v", err)
	}

	// 退出安全模式（重建条件），续办后重新双签、重新执行成功
	h.tm(t, satA, "S", 3, 2*time.Minute, mission.QualityVerified,
		map[mission.ConditionKey]float64{mission.CondAttitude: 90, mission.CondPower: 70}, false)
	if err := h.svc.ResolveIssue(mission.ResolveIssueParams{
		IssueID: issueID, By: "z", Resolution: "安全模式已退出，条件重建",
	}); err != nil {
		t.Fatal(err)
	}
	pv, _ = h.svc.GetPlan(p.PlanID)
	if pv.Status != mission.PlanSubmitted {
		t.Fatalf("续办后应回到 submitted，实际 %s", pv.Status)
	}
	h.approve2(t, p.PlanID)
	h.setClock(3 * time.Minute)
	h.execute(t, p.PlanID, "seat-1")
	h.successReceipt(t, p.PlanID, 3*time.Minute+20*time.Second)
	pv, _ = h.svc.GetPlan(p.PlanID)
	if pv.Status != mission.PlanExecuted {
		t.Fatalf("应执行成功，实际 %s", pv.Status)
	}
}

// 9. 窗口取消进入处置链，执行权撤销，改挂新窗口可续办。
func TestWindowCancellationChain(t *testing.T) {
	h := newHarness(t, 5*time.Minute)
	h.register(t, satA)
	h.healthy(t, satA, "S", 1, 0)
	h.window(t, "W1", satA, gs1, 30*time.Second, 30*time.Minute)
	p := h.makePlan(t, satA, "W1", mission.CmdHealthCheck, "li")
	h.submitApprove(t, p)
	h.setClock(time.Minute)
	h.execute(t, p.PlanID, "seat-1")

	if err := h.svc.CancelWindow("W1", "director", "任务调整"); err != nil {
		t.Fatal(err)
	}
	pv, _ := h.svc.GetPlan(p.PlanID)
	if pv.Status != mission.PlanAwaiting {
		t.Fatalf("窗口取消后应 awaiting，实际 %s", pv.Status)
	}
	issue, _ := h.svc.GetIssue(pv.IssueIDs[0])
	if issue.Type != mission.IssueWindowCancel {
		t.Fatalf("处置链类型错误: %s", issue.Type)
	}

	h.window(t, "W2", satA, gs2, 10*time.Minute, 40*time.Minute)
	if err := h.svc.ResolveIssue(mission.ResolveIssueParams{
		IssueID: issue.IssueID, By: "director", Resolution: "改挂 W2", NewWindowID: "W2",
	}); err != nil {
		t.Fatal(err)
	}
	pv, _ = h.svc.GetPlan(p.PlanID)
	if pv.WindowID != "W2" || pv.Status != mission.PlanSubmitted {
		t.Fatalf("续办后应改挂 W2，实际 %s/%s", pv.WindowID, pv.Status)
	}
}

// 10. 进程重启后通过回放找回未完成工作（等待回执的计划、开启的处置链）。
func TestRestartRecoversInFlightWork(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.register(t, satA)
	h.healthy(t, satA, "S", 1, 0)
	h.window(t, "W1", satA, gs1, 30*time.Second, 30*time.Minute)
	p := h.makePlan(t, satA, "W1", mission.CmdHealthCheck, "li")
	h.submitApprove(t, p)
	h.setClock(time.Minute)
	h.execute(t, p.PlanID, "seat-1")
	planID := p.PlanID

	// 重启：重新打开同一日志回放，使用新时钟从 2 分钟处继续
	store, _, err := mission.OpenStore(filepath.Join(h.dir, "mission.log"))
	if err != nil {
		t.Fatal(err)
	}
	clk2 := mission.NewSimClock()
	clk2.Set(h.at(2*time.Minute + 5*time.Second))
	svc2, err := mission.NewService(store, clk2.Now, mission.Config{LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc2.Sweep(); err != nil {
		t.Fatal(err)
	}
	pv, err := svc2.GetPlan(planID)
	if err != nil {
		t.Fatalf("重启后找不到计划: %v", err)
	}
	if pv.Status != mission.PlanAwaiting {
		t.Fatalf("重启巡检应识别超时并转 awaiting，实际 %s", pv.Status)
	}
	open := svc2.ListIssues(mission.IssueTimeout, true)
	if len(open) != 1 {
		t.Fatalf("应找回 1 条未闭环超时处置链，实际 %d", len(open))
	}
	sv, _ := svc2.GetSatellite(satA)
	if !sv.Conditions[mission.CondAttitude].OK {
		t.Fatal("重启后条件结论应完整恢复")
	}
	store.Close()
}

// 11. 整组交付：有缺口不能签接收；证据被冻结，后续变化不影响已签结论。
func TestGroupDeliveryFreezesEvidence(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.register(t, satA)
	h.register(t, satB)
	for i, s := range []string{satA, satB} {
		h.healthy(t, s, "S", 1, 0)
		h.window(t, "W-"+s, s, gs1, 30*time.Second, 60*time.Minute)
		_ = i
	}
	// 只完成 A 的三项必做
	h.completeAllChecks(t, satA, "W-"+satA)

	// 整组签接收应因 B 有缺口而失败
	_, err := h.svc.SignDelivery(mission.SignDeliveryParams{
		SatelliteIDs: []string{satA, satB}, Decision: mission.DecisionAccepted, By: "director",
	})
	if !errors.Is(err, mission.ErrPrecondition) {
		t.Fatalf("有缺口时整组接收应被拒绝，实际 %v", err)
	}

	svB, _ := h.svc.GetSatellite(satB)
	if len(svB.Gaps) == 0 {
		t.Fatal("B 应报告缺口")
	}

	// 完成 B 后整组接收
	h.completeAllChecks(t, satB, "W-"+satB)
	dv, err := h.svc.SignDelivery(mission.SignDeliveryParams{
		SatelliteIDs: []string{satB, satA}, Decision: mission.DecisionAccepted, By: "director",
		Notes: "整组验收通过",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dv.Evidence.Satellites) != 2 {
		t.Fatal("证据快照应覆盖两颗星")
	}

	// 冻结后：A 再出安全模式，已签结论证据不变
	h.tm(t, satA, "S", 99, 30*time.Minute, mission.QualityVerified,
		map[mission.ConditionKey]float64{mission.CondAttitude: 95, mission.CondPower: 80}, true)
	stored, err := h.svc.GetDelivery(dv.DeliveryID)
	if err != nil {
		t.Fatal(err)
	}
	for _, se := range stored.Evidence.Satellites {
		if se.SatelliteID == satA && se.SafeMode {
			t.Fatal("已冻结证据不应被后续遥测改写")
		}
		if len(se.Gaps) != 0 {
			t.Fatal("接收结论的冻结证据应无缺口")
		}
	}

	// 证据中可回答：谁批准过哪版指令、执行结果
	var checked int
	for _, se := range stored.Evidence.Satellites {
		for _, pl := range se.Plans {
			if len(pl.Approvals) != 2 {
				t.Fatalf("%s 应有双签留痕", pl.PlanID)
			}
			if pl.Receipt == nil || !pl.Receipt.Success {
				t.Fatalf("%s 应冻结成功回执", pl.PlanID)
			}
			checked++
		}
	}
	if checked != 6 {
		t.Fatalf("两星三项共 6 条计划证据，实际 %d", checked)
	}
}

// 12. 有缺口时可以签署 conditional 结论，证据同样冻结。
func TestConditionalDeliveryAllowed(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.register(t, satA)
	h.healthy(t, satA, "S", 1, 0)
	dv, err := h.svc.SignDelivery(mission.SignDeliveryParams{
		SatelliteIDs: []string{satA}, Decision: mission.DecisionConditional, By: "director",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(dv.Evidence.Satellites[0].Gaps) == 0 {
		t.Fatal("条件性接收的证据中应保留缺口清单")
	}
}

// 13. 单星缺口查询逐项反映还缺什么。
func TestSatelliteGaps(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.register(t, satA)
	sv, _ := h.svc.GetSatellite(satA)
	codes := map[string]bool{}
	for _, g := range sv.Gaps {
		codes[g.Code] = true
	}
	for _, want := range []string{"missing_attitude", "missing_power",
		"check_incomplete_health_check", "check_incomplete_payload_power_on",
		"check_incomplete_cross_calibration"} {
		if !codes[want] {
			t.Fatalf("缺口清单缺少 %s，实际 %v", want, sv.Gaps)
		}
	}
}

// 14. 窗口外不得执行。
func TestExecuteRequiresOpenWindow(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.register(t, satA)
	h.healthy(t, satA, "S", 1, 0)
	h.window(t, "W1", satA, gs1, 10*time.Minute, 20*time.Minute)
	p := h.makePlan(t, satA, "W1", mission.CmdHealthCheck, "li")
	h.submitApprove(t, p)
	// 当前 t0，窗口未开始
	_, err := h.svc.ExecutePlan(p.PlanID, "seat")
	if !errors.Is(err, mission.ErrPrecondition) {
		t.Fatalf("窗口外执行应拒绝，实际 %v", err)
	}
}

// 15. 多数据源乱序：进入安全模式后，其他数据源迟到的旧帧不得把安全模式“翻回去”。
func TestLateSafeModeFrameFromAnotherSource(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.register(t, satA)
	h.healthy(t, satA, "SRC-A", 100, time.Minute)

	// SRC-B 一帧极旧数据（星上时间 30s）声称非安全模式 —— 在进入安全模式前，
	// 读数只按星上时间裁定，不影响结论。先制造安全模式：
	h.tm(t, satA, "SRC-A", 101, 2*time.Minute, mission.QualityVerified,
		map[mission.ConditionKey]float64{mission.CondAttitude: 95, mission.CondPower: 80}, true)
	sv, _ := h.svc.GetSatellite(satA)
	if !sv.SafeMode {
		t.Fatal("应处于安全模式")
	}

	// 另一数据源迟到一帧星上时间仅 10s 的非安全模式帧，不得退出安全模式。
	f := h.tm(t, satA, "SRC-B", 1, 10*time.Second, mission.QualityVerified,
		map[mission.ConditionKey]float64{mission.CondAttitude: 95, mission.CondPower: 80}, false)
	sv, _ = h.svc.GetSatellite(satA)
	if !sv.SafeMode {
		t.Fatal("迟到旧帧不得解除安全模式")
	}
	_ = f

	// 真正更新的退出帧可以解除
	h.tm(t, satA, "SRC-A", 102, 3*time.Minute, mission.QualityVerified,
		map[mission.ConditionKey]float64{mission.CondAttitude: 90, mission.CondPower: 70}, false)
	sv, _ = h.svc.GetSatellite(satA)
	if sv.SafeMode {
		t.Fatal("更新的退出帧应解除安全模式")
	}
}

// 16. 超时后收到迟到的成功回执：处置链仍需显式闭环，但计划可凭回执恢复为成功。
func TestLateReceiptAfterTimeout(t *testing.T) {
	h := newHarness(t, time.Minute)
	h.register(t, satA)
	h.healthy(t, satA, "S", 1, 0)
	h.window(t, "W1", satA, gs1, 30*time.Second, 30*time.Minute)
	p := h.makePlan(t, satA, "W1", mission.CmdHealthCheck, "li")
	h.submitApprove(t, p)
	h.setClock(time.Minute)
	lease := h.execute(t, p.PlanID, "seat-1")

	// 超时
	h.setClock(time.Duration(lease.Deadline.Sub(t0) + time.Second))
	if err := h.svc.Sweep(); err != nil {
		t.Fatal(err)
	}
	pv, _ := h.svc.GetPlan(p.PlanID)
	if pv.Status != mission.PlanAwaiting {
		t.Fatalf("应 awaiting，实际 %s", pv.Status)
	}
	issueID := pv.IssueIDs[0]

	// 此时迟到回执不再被接受（执行权已释放），须走处置链续办。
	_, err := h.svc.RecordReceipt(p.PlanID, mission.Receipt{
		Success: true, SpacecraftTime: h.at(90 * time.Second), ReceivedAt: h.at(3 * time.Minute),
	})
	if !errors.Is(err, mission.ErrNotAcceptable) {
		t.Fatalf("执行权释放后迟到回执应被拒绝并引导走处置链，实际 %v", err)
	}

	// 指定新窗口续办后计划可重新双签执行
	h.window(t, "W2", satA, gs2, 10*time.Minute, 40*time.Minute)
	if err := h.svc.ResolveIssue(mission.ResolveIssueParams{
		IssueID: issueID, By: "zhang",
		Resolution: "核查星上日志确认指令实际已执行，按新窗口流程复核补录", NewWindowID: "W2",
	}); err != nil {
		t.Fatal(err)
	}
	// 处置链留痕可被查询
	iv, _ := h.svc.GetIssue(issueID)
	if iv.Status != mission.IssueResolved || iv.Resolution == "" {
		t.Fatal("处置链应记录闭环结论")
	}
}
