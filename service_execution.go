package mission

import (
	"fmt"
	"sort"
	"time"
)

// 资源锁名：一颗卫星、一个地面站、（交叉标定的）伴星各为一项资源。
func planResources(pl *Plan) []string {
	set := map[string]struct{}{
		"sat:" + pl.SatelliteID:   {},
		"station:" + pl.StationID: {},
	}
	if pl.PartnerSatelliteID != "" {
		set["sat:"+pl.PartnerSatelliteID] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// StartExecutionInput 为上行执行输入。
type StartExecutionInput struct {
	PlanID   string `json:"plan_id"`
	Operator string `json:"operator"`
}

// StartExecution 尝试为已双签且门禁仍通过的计划取得执行权。
// 资源（卫星/伴星/地面站）已被其他执行中计划占有时，本计划被拒绝——
// 资源重叠时只允许一项计划取得执行权，裁决结果由持久化的开始事件确定。
func (s *Service) StartExecution(in StartExecutionInput) (*Plan, *ExecutionAttempt, error) {
	if in.PlanID == "" || in.Operator == "" {
		return nil, nil, apiErr(CodeValidation, "计划标识与操作人不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pl := s.state.Plans[in.PlanID]
	if pl == nil {
		return nil, nil, apiErr(CodeNotFound, "计划 %s 不存在", in.PlanID)
	}
	if pl.State == PlanExecuting {
		return nil, nil, apiErr(CodeStateConflict, "计划 %s 已在执行中", pl.ID)
	}
	if pl.State == PlanSucceeded || pl.State == PlanFailed || pl.State == PlanAborted || pl.State == PlanTimedOut {
		return nil, nil, apiErr(CodeStateConflict, "计划 %s 已终态（%s），不能重复执行", pl.ID, pl.State)
	}
	if !pl.HasValidApproval(RoleProposer) || !pl.HasValidApproval(RoleVerifier) {
		return nil, nil, apiErr(CodePrecondition, "计划 %s 缺少两个有效批准（前置条件漂移会使批准失效，请重新复核）", pl.ID)
	}
	now := s.now()
	gates := evaluateGates(s.state, pl, now)
	if !gatesPass(gates) {
		return nil, nil, apiErr(CodePrecondition, "执行前门禁复核未通过: %s", describeGateFailure(gates))
	}
	resources := planResources(pl)
	var holders []string
	for _, r := range resources {
		if holder, ok := s.state.ActiveLocks[r]; ok && holder != pl.ID {
			holders = append(holders, holder)
		}
	}
	if len(holders) > 0 {
		holders = uniqueSorted(holders)
		return nil, nil, apiErr(CodeConflict, "资源已被执行中计划占用（%v），本计划未取得执行权", holders)
	}
	attempt := ExecutionAttempt{StartedAt: now, Deadline: now.Add(pl.Timeout), Operator: in.Operator}
	payload := PlanExecutionStartedPayload{PlanID: pl.ID, Attempt: len(pl.Attempts) + 1,
		Resources: resources, Started: attempt}
	if _, err := s.emit(EvPlanExecutionStarted, payload); err != nil {
		return nil, nil, err
	}
	return clone(pl), clone(&attempt), nil
}

// ResolveExecutionInput 为星上回执/执行落定输入。
type ResolveExecutionInput struct {
	PlanID         string    `json:"plan_id"`
	Outcome        PlanState `json:"outcome"` // succeeded | failed | aborted
	ReceiptStatus  string    `json:"receipt_status,omitempty"`
	ReceiptAt      time.Time `json:"receipt_at,omitempty"`
	ReceiptSeq     *int64    `json:"receipt_seq,omitempty"`
	AcknowledgedAt time.Time `json:"acknowledged_at,omitempty"`
	Detail         string    `json:"detail,omitempty"`
}

// ResolveExecution 登记星上回执并使执行落定、释放资源锁。
// succeeded 时原子登记里程碑；failed/aborted 不登记。
func (s *Service) ResolveExecution(in ResolveExecutionInput) (*Plan, error) {
	if in.PlanID == "" {
		return nil, apiErr(CodeValidation, "计划标识不能为空")
	}
	switch in.Outcome {
	case PlanSucceeded, PlanFailed, PlanAborted:
	default:
		return nil, apiErr(CodeValidation, "执行结果必须是 succeeded / failed / aborted；超时由系统扫描裁定")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pl := s.state.Plans[in.PlanID]
	if pl == nil {
		return nil, apiErr(CodeNotFound, "计划 %s 不存在", in.PlanID)
	}
	if pl.State != PlanExecuting {
		return nil, apiErr(CodeStateConflict, "计划 %s 当前状态为 %s，无法登记回执", pl.ID, pl.State)
	}
	now := s.now()
	if in.ReceiptAt.IsZero() {
		in.ReceiptAt = now
	}
	result := ExecutionResult{Outcome: in.Outcome, At: now, Attempt: len(pl.Attempts),
		Receipt: in.ReceiptStatus, Detail: in.Detail}
	var milestone *MilestoneRecord
	if in.Outcome == PlanSucceeded && pl.Milestone != "" {
		milestone = &MilestoneRecord{Milestone: pl.Milestone, PlanID: pl.ID, At: now,
			Detail: "计划 " + pl.ID + " 成功执行并收到星上回执"}
	}
	var ackAt, rcpAt *time.Time
	if !in.AcknowledgedAt.IsZero() {
		t := in.AcknowledgedAt
		ackAt = &t
	}
	if !in.ReceiptAt.IsZero() {
		t := in.ReceiptAt
		rcpAt = &t
	}
	payload := PlanExecutionResolvedPayload{PlanID: pl.ID, Result: result, Milestone: milestone,
		AcknowledgedAt: ackAt, ReceiptStatus: in.ReceiptStatus, ReceiptAt: rcpAt, ReceiptSeq: in.ReceiptSeq}
	if _, err := s.emit(EvPlanExecutionResolved, payload); err != nil {
		return nil, err
	}
	return clone(s.state.Plans[pl.ID]), nil
}

// SweepResult 为一次超时扫描中被裁定的计划。
type SweepResult struct {
	PlanID string    `json:"plan_id"`
	CaseID string    `json:"case_id"`
	At     time.Time `json:"at"`
}

// SweepTimeouts 扫描执行中且已过截止时间的尝试：裁定为 timed_out、释放资源锁，
// 并为每条计划开立可续办的超时处置链。进程重启后未完成工作同样靠本扫描找回。
func (s *Service) SweepTimeouts() ([]SweepResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var out []SweepResult
	for _, pid := range s.state.SortedPlanIDs() {
		pl := s.state.Plans[pid]
		if pl.State != PlanExecuting || len(pl.Attempts) == 0 {
			continue
		}
		last := pl.Attempts[len(pl.Attempts)-1]
		if now.Before(last.Deadline) {
			continue
		}
		c := s.newCaseLocked(CaseExecutionTimeout, pl.SatelliteID, pl.ID,
			fmt.Sprintf("计划 %s（%s）执行超时，截止 %s，未收到星上回执", pl.ID, pl.Kind,
				last.Deadline.Format(time.RFC3339)))
		c.Steps = append(c.Steps, CaseStep{Action: "resources_released", Operator: "system", At: now,
			Note:    "超时裁定已释放卫星/地面站资源，其他计划可竞争执行权",
			Payload: map[string]any{"resources": planResources(pl)}})
		result := ExecutionResult{Outcome: PlanTimedOut, At: now, Attempt: len(pl.Attempts),
			Detail: "超过截止时间 " + last.Deadline.Format(time.RFC3339) + " 未收到回执"}
		payload := PlanExecutionResolvedPayload{PlanID: pl.ID, Result: result, Case: c}
		if _, err := s.emit(EvPlanExecutionResolved, payload); err != nil {
			return nil, err
		}
		out = append(out, SweepResult{PlanID: pl.ID, CaseID: c.ID, At: now})
	}
	return out, nil
}

// ---------- 安全模式 ----------

// EnterSafeMode 通报卫星进入安全模式：新一代安全条件原子地
// 失效相关高风险批准、中止执行中的计划（释放资源），并进入可续办处置链。
func (s *Service) EnterSafeMode(satelliteID, reason string) (*Case, error) {
	if satelliteID == "" {
		return nil, apiErr(CodeValidation, "卫星标识不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sat := s.state.Satellites[satelliteID]
	if sat == nil {
		return nil, apiErr(CodeNotFound, "卫星 %s 不存在", satelliteID)
	}
	if sat.Safe {
		return nil, apiErr(CodeStateConflict, "卫星 %s 已处于第 %d 代安全模式", satelliteID, sat.SafeGeneration)
	}
	now := s.now()
	generation := sat.SafeGeneration + 1
	var invalidated []InvalidatedApproval
	var aborted []string
	for _, pid := range s.state.SortedPlanIDs() {
		pl := s.state.Plans[pid]
		if !planTouchesSatellite(pl, satelliteID) {
			continue
		}
		if pl.State == PlanExecuting {
			aborted = append(aborted, pl.ID)
		}
		for _, a := range pl.Approvals {
			if a.Valid && a.Basis.SafeGenerationFor(satelliteID, pl.SatelliteID) < generation {
				invalidated = append(invalidated, InvalidatedApproval{
					PlanID: pl.ID, ApprovalID: a.ID,
					Reason: fmt.Sprintf("卫星 %s 进入第 %d 代安全模式: %s", satelliteID, generation, reason), At: now,
				})
			}
		}
	}
	c := s.newCaseLocked(CaseSafeMode, satelliteID, "",
		fmt.Sprintf("卫星 %s 进入安全模式（第 %d 代）: %s", satelliteID, generation, reason))
	if len(aborted) > 0 {
		c.Steps = append(c.Steps, CaseStep{Action: "plans_aborted", Operator: "system", At: now,
			Note:    fmt.Sprintf("%d 条执行中计划被中止并释放资源", len(aborted)),
			Payload: map[string]any{"plan_ids": aborted}})
	}
	payload := SafeModeEnteredPayload{SatelliteID: satelliteID, Reason: reason, At: now,
		Generation: generation, Invalidated: invalidated, AbortedPlanIDs: aborted, Case: c}
	if _, err := s.emit(EvSafeModeEntered, payload); err != nil {
		return nil, err
	}
	return clone(c), nil
}

// ClearSafeMode 解除安全模式：追加处置步骤、登记解除并关闭处置链（一次原子完成）。
func (s *Service) ClearSafeMode(satelliteID, operator, note string) (*Case, error) {
	if satelliteID == "" || operator == "" {
		return nil, apiErr(CodeValidation, "卫星标识与操作人不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sat := s.state.Satellites[satelliteID]
	if sat == nil {
		return nil, apiErr(CodeNotFound, "卫星 %s 不存在", satelliteID)
	}
	if !sat.Safe {
		return nil, apiErr(CodeStateConflict, "卫星 %s 当前不在安全模式", satelliteID)
	}
	var open *Case
	for _, id := range s.state.SortedCaseIDs() {
		c := s.state.Cases[id]
		if c.Kind == CaseSafeMode && c.SatelliteID == satelliteID && c.State == CaseOpen {
			open = c
		}
	}
	now := s.now()
	generation := sat.SafeGeneration
	if open != nil {
		step := CaseStep{Action: "safe_mode_cleared", Operator: operator, Note: note, At: now}
		if _, err := s.emit(EvCaseAdvanced, CaseAdvancedPayload{CaseID: open.ID, Step: step}); err != nil {
			return nil, err
		}
	}
	if _, err := s.emit(EvSafeModeCleared, SafeModeClearedPayload{
		SatelliteID: satelliteID, Generation: generation, At: now, Operator: operator}); err != nil {
		return nil, err
	}
	if open != nil {
		if _, err := s.emit(EvCaseResolved, CaseResolvedPayload{CaseID: open.ID, At: now,
			Note: "安全模式已解除，处置链关闭"}); err != nil {
			return nil, err
		}
	}
	if open == nil {
		return nil, nil
	}
	return clone(s.state.Cases[open.ID]), nil
}

// ---------- 处置链续办 ----------

// AdvanceCase 在处置链上续办一步（如排障记录、重发申请、改挂新窗口）。
func (s *Service) AdvanceCase(caseID, action, operator, note string, payload map[string]any) (*Case, error) {
	if caseID == "" || action == "" || operator == "" {
		return nil, apiErr(CodeValidation, "处置单、动作与操作人不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.state.Cases[caseID]
	if c == nil {
		return nil, apiErr(CodeNotFound, "处置单 %s 不存在", caseID)
	}
	if c.State == CaseResolved {
		return nil, apiErr(CodeStateConflict, "处置单 %s 已关闭", caseID)
	}
	step := CaseStep{Action: action, Operator: operator, Note: note, At: s.now(), Payload: payload}
	if _, err := s.emit(EvCaseAdvanced, CaseAdvancedPayload{CaseID: caseID, Step: step}); err != nil {
		return nil, err
	}
	return clone(c), nil
}

// ResolveCase 手动关闭处置链。
func (s *Service) ResolveCase(caseID, operator, note string) (*Case, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.state.Cases[caseID]
	if c == nil {
		return nil, apiErr(CodeNotFound, "处置单 %s 不存在", caseID)
	}
	if c.State == CaseResolved {
		return nil, apiErr(CodeStateConflict, "处置单 %s 已关闭", caseID)
	}
	now := s.now()
	if _, err := s.emit(EvCaseAdvanced, CaseAdvancedPayload{CaseID: caseID,
		Step: CaseStep{Action: "manually_resolved", Operator: operator, Note: note, At: now}}); err != nil {
		return nil, err
	}
	if _, err := s.emit(EvCaseResolved, CaseResolvedPayload{CaseID: caseID, At: now, Note: note}); err != nil {
		return nil, err
	}
	return clone(c), nil
}

// ReplanFromCaseInput 为超时/取消处置链续办重发计划的输入。
type ReplanFromCaseInput struct {
	CaseID      string `json:"case_id"`
	Operator    string `json:"operator"`
	NewWindowID string `json:"new_window_id,omitempty"` // 窗口取消续办时指定新窗口
	Note        string `json:"note,omitempty"`
}

// ReplanFromCase 基于处置链所涉计划，复制其指令版本与门禁生成后继计划（需重新双人复核），
// 并在处置链上登记续办步骤。后继计划与处置链相互引用。
func (s *Service) ReplanFromCase(in ReplanFromCaseInput) (*Plan, *Case, error) {
	if in.CaseID == "" || in.Operator == "" {
		return nil, nil, apiErr(CodeValidation, "处置单与操作人不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.state.Cases[in.CaseID]
	if c == nil {
		return nil, nil, apiErr(CodeNotFound, "处置单 %s 不存在", in.CaseID)
	}
	if c.State == CaseResolved {
		return nil, nil, apiErr(CodeStateConflict, "处置单 %s 已关闭，不能再续办", in.CaseID)
	}
	if c.Kind != CaseExecutionTimeout && c.Kind != CaseWindowCanceled {
		return nil, nil, apiErr(CodeValidation, "仅超时/窗口取消处置链支持直接重发，安全模式请先解除后新建计划")
	}
	src := s.state.Plans[c.PlanID]
	if src == nil {
		return nil, nil, apiErr(CodeStateConflict, "处置单 %s 没有关联的原计划", in.CaseID)
	}
	windowID := src.WindowID
	if in.NewWindowID != "" {
		nw := s.state.Windows[in.NewWindowID]
		if nw == nil {
			return nil, nil, apiErr(CodeNotFound, "新窗口 %s 不存在", in.NewWindowID)
		}
		if nw.SatelliteID != src.SatelliteID {
			return nil, nil, apiErr(CodeValidation, "新窗口 %s 不属于卫星 %s", in.NewWindowID, src.SatelliteID)
		}
		if nw.State == WindowCanceled {
			return nil, nil, apiErr(CodeStateConflict, "新窗口 %s 已取消", in.NewWindowID)
		}
		windowID = nw.ID
	}
	w := s.state.Windows[windowID]
	if w.State == WindowCanceled {
		return nil, nil, apiErr(CodeStateConflict, "原窗口仍取消，请指定新窗口续办")
	}
	n := len(s.state.Plans) + 1
	id := fmt.Sprintf("plan-%s-%d", src.SatelliteID, n)
	for s.state.Plans[id] != nil {
		n++
		id = fmt.Sprintf("plan-%s-%d", src.SatelliteID, n)
	}
	pl := Plan{
		ID: id, SatelliteID: src.SatelliteID, PartnerSatelliteID: src.PartnerSatelliteID,
		WindowID: w.ID, WindowVersion: w.Version, StationID: w.StationID,
		CommandVersionID: src.CommandVersionID, Kind: src.Kind, Milestone: src.Milestone,
		Gates: clone(src.Gates), Timeout: src.Timeout, CreatedBy: in.Operator,
		CreatedAt: s.now(), State: PlanAwaitingApproval, Approvals: []*Approval{},
		Attempts: []*ExecutionAttempt{},
	}
	if _, err := s.emit(EvPlanCreated, PlanCreatedPayload{Plan: pl}); err != nil {
		return nil, nil, err
	}
	step := CaseStep{Action: "replan", Operator: in.Operator, At: s.now(),
		Note: in.Note, Payload: map[string]any{
			"new_plan_id": pl.ID, "window_id": w.ID, "window_version": w.Version,
			"source_plan_id": src.ID,
		}}
	if _, err := s.emit(EvCaseAdvanced, CaseAdvancedPayload{CaseID: c.ID, Step: step}); err != nil {
		return nil, nil, err
	}
	return clone(&pl), clone(c), nil
}

// lockConflictsLocked 列出当前抢占计划所需资源的其他执行中计划。
func (s *Service) lockConflictsLocked(pl *Plan) []string {
	var out []string
	for _, r := range planResources(pl) {
		if holder, ok := s.state.ActiveLocks[r]; ok && holder != pl.ID {
			out = append(out, holder)
		}
	}
	return uniqueSorted(out)
}

func uniqueSorted(in []string) []string {
	seen := map[string]struct{}{}
	out := in[:0]
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
