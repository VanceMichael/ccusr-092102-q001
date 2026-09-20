package mission

import (
	"fmt"
	"time"
)

// ---------- 健康确认 ----------

// ConfirmHealthInput 为健康确认输入。
type ConfirmHealthInput struct {
	SatelliteID string `json:"satellite_id"`
	Operator    string `json:"operator"`
	Note        string `json:"note"`
	SourceID    string `json:"source_id"` // 依据哪个数据源的当前帧；为空取最新已验证帧
}

// ConfirmHealth 以某版遥测为依据完成健康确认，证据（帧键与哈希）随之冻结。
func (s *Service) ConfirmHealth(in ConfirmHealthInput) (*HealthConfirmation, error) {
	if in.SatelliteID == "" || in.Operator == "" {
		return nil, apiErr(CodeValidation, "卫星标识与操作人不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sat := s.state.Satellites[in.SatelliteID]
	if sat == nil {
		return nil, apiErr(CodeNotFound, "卫星 %s 不存在", in.SatelliteID)
	}
	f := s.basisFrameLocked(in.SatelliteID, in.SourceID)
	if f == nil {
		return nil, apiErr(CodePrecondition, "卫星 %s 没有已验证遥测，无法确认健康", in.SatelliteID)
	}
	c := HealthConfirmation{SatelliteID: in.SatelliteID, Operator: in.Operator,
		Note: in.Note, FrameKey: f.Key(), FrameHash: f.Hash, At: s.now()}
	if _, err := s.emit(EvHealthConfirmed, HealthConfirmedPayload{Confirmation: c}); err != nil {
		return nil, err
	}
	return &c, nil
}

// basisFrameLocked 取健康确认/批准依据的遥测帧。
func (s *Service) basisFrameLocked(satelliteID, sourceID string) *Frame {
	if sourceID != "" {
		if f := s.state.CurrentBySource[satelliteID][sourceID]; f != nil && f.Quality == QualityVerified {
			return f
		}
		return nil
	}
	return latestVerifiedFrame(s.state, satelliteID)
}

// ---------- 指令版本 ----------

// CreateCommandInput 为创建指令版本输入。
type CreateCommandInput struct {
	SatelliteID string         `json:"satellite_id"`
	Command     string         `json:"command"`
	Revision    int            `json:"revision"` // 0 表示自动递增
	Content     map[string]any `json:"content"`
	CreatedBy   string         `json:"created_by"`
}

// CreateCommandVersion 登记一版不可变指令；同 (卫星,指令) 下 revision 唯一且递增。
func (s *Service) CreateCommandVersion(in CreateCommandInput) (*CommandVersion, error) {
	if in.SatelliteID == "" || in.Command == "" || in.CreatedBy == "" {
		return nil, apiErr(CodeValidation, "卫星标识、指令名与创建人不能为空")
	}
	if in.Content == nil {
		in.Content = map[string]any{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Satellites[in.SatelliteID]; !ok {
		return nil, apiErr(CodeNotFound, "卫星 %s 不存在", in.SatelliteID)
	}
	maxRev := 0
	for _, cv := range s.state.Commands {
		if cv.SatelliteID == in.SatelliteID && cv.Command == in.Command && cv.Revision > maxRev {
			maxRev = cv.Revision
		}
	}
	if in.Revision == 0 {
		in.Revision = maxRev + 1
	}
	if in.Revision <= maxRev {
		return nil, apiErr(CodeConflict, "指令 %s 的 revision 必须 > %d", in.Command, maxRev)
	}
	cv := CommandVersion{
		ID:          fmt.Sprintf("cmd-%s-%s-r%d", in.SatelliteID, in.Command, in.Revision),
		SatelliteID: in.SatelliteID, Command: in.Command, Revision: in.Revision,
		Content: in.Content, Hash: StableHash(in.Content),
		CreatedBy: in.CreatedBy, CreatedAt: s.now(),
	}
	if _, err := s.emit(EvCommandVersionCreated, CommandVersionCreatedPayload{Version: cv}); err != nil {
		return nil, err
	}
	return &cv, nil
}

// ---------- 指令计划 ----------

// CreatePlanInput 为创建计划输入。
type CreatePlanInput struct {
	SatelliteID        string    `json:"satellite_id"`
	PartnerSatelliteID string    `json:"partner_satellite_id,omitempty"`
	WindowID           string    `json:"window_id"`
	CommandVersionID   string    `json:"command_version_id"`
	Kind               string    `json:"kind"`
	Milestone          Milestone `json:"milestone,omitempty"`
	Gates              []Gate    `json:"gates"`
	TimeoutSeconds     int       `json:"timeout_seconds,omitempty"`
	CreatedBy          string    `json:"created_by"`
}

// CreatePlan 建立待复核计划。计划绑定窗口版本；窗口换版后旧计划的门禁不再通过。
func (s *Service) CreatePlan(in CreatePlanInput) (*Plan, error) {
	if in.SatelliteID == "" || in.WindowID == "" || in.CommandVersionID == "" || in.CreatedBy == "" {
		return nil, apiErr(CodeValidation, "卫星、窗口、指令版本与创建人不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sat := s.state.Satellites[in.SatelliteID]
	if sat == nil {
		return nil, apiErr(CodeNotFound, "卫星 %s 不存在", in.SatelliteID)
	}
	w := s.state.Windows[in.WindowID]
	if w == nil {
		return nil, apiErr(CodeNotFound, "窗口 %s 不存在", in.WindowID)
	}
	if w.SatelliteID != in.SatelliteID {
		return nil, apiErr(CodeValidation, "窗口 %s 不属于卫星 %s", in.WindowID, in.SatelliteID)
	}
	if w.State == WindowCanceled {
		return nil, apiErr(CodeStateConflict, "窗口 %s 已取消，不能据此创建计划", in.WindowID)
	}
	cv := s.state.Commands[in.CommandVersionID]
	if cv == nil {
		return nil, apiErr(CodeNotFound, "指令版本 %s 不存在", in.CommandVersionID)
	}
	if cv.SatelliteID != in.SatelliteID {
		return nil, apiErr(CodeValidation, "指令版本 %s 不属于卫星 %s", in.CommandVersionID, in.SatelliteID)
	}
	if in.PartnerSatelliteID != "" && s.state.Satellites[in.PartnerSatelliteID] == nil {
		return nil, apiErr(CodeNotFound, "伴星 %s 不存在", in.PartnerSatelliteID)
	}
	if in.Kind == "" {
		in.Kind = cv.Command
	}
	gates := in.Gates
	if gates == nil {
		gates = []Gate{}
	}
	timeout := time.Duration(in.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = s.defTimeout
	}
	n := len(s.state.Plans) + 1
	id := fmt.Sprintf("plan-%s-%d", in.SatelliteID, n)
	for s.state.Plans[id] != nil {
		n++
		id = fmt.Sprintf("plan-%s-%d", in.SatelliteID, n)
	}
	pl := Plan{
		ID: id, SatelliteID: in.SatelliteID, PartnerSatelliteID: in.PartnerSatelliteID,
		WindowID: w.ID, WindowVersion: w.Version, StationID: w.StationID,
		CommandVersionID: cv.ID, Kind: in.Kind, Milestone: in.Milestone,
		Gates: gates, Timeout: timeout, CreatedBy: in.CreatedBy,
		CreatedAt: s.now(), State: PlanAwaitingApproval, Approvals: []*Approval{},
		Attempts: []*ExecutionAttempt{},
	}
	if _, err := s.emit(EvPlanCreated, PlanCreatedPayload{Plan: pl}); err != nil {
		return nil, err
	}
	return clone(&pl), nil
}

// ApprovePlanInput 为复核签名输入。
type ApprovePlanInput struct {
	PlanID   string       `json:"plan_id"`
	Role     ApprovalRole `json:"role"`
	Operator string       `json:"operator"`
	Note     string       `json:"note"`
	SourceID string       `json:"source_id,omitempty"`
}

// PlanStatus 返回计划的只读快照与当前门禁求值。
type PlanStatus struct {
	Plan           *Plan        `json:"plan"`
	Gates          []GateResult `json:"gates"`
	GatesPassed    bool         `json:"gates_passed"`
	CanApproveRole ApprovalRole `json:"can_approve_role,omitempty"`
	Ready          bool         `json:"ready"`
	LockWinner     bool         `json:"lock_winner"` // 当前资源锁是否由本计划持有/可取得
	ConflictWith   []string     `json:"conflict_with,omitempty"`
}

// ApprovePlan 由双人席位之一签署批准。要求：
//   - 提议/复核两个不同操作人；
//   - 批准当下全部前置条件通过；
//   - 冻结依据（遥测帧键+哈希、条件签名、窗口版本、安全模式代数、指令哈希、门禁结果）。
//
// 依据随后发生任何漂移，该批准即由相应事件原子失效。
func (s *Service) ApprovePlan(in ApprovePlanInput) (*Approval, []GateResult, error) {
	if in.Operator == "" || in.PlanID == "" {
		return nil, nil, apiErr(CodeValidation, "计划标识与操作人不能为空")
	}
	if in.Role != RoleProposer && in.Role != RoleVerifier {
		return nil, nil, apiErr(CodeValidation, "角色必须是 proposer 或 verifier")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pl := s.state.Plans[in.PlanID]
	if pl == nil {
		return nil, nil, apiErr(CodeNotFound, "计划 %s 不存在", in.PlanID)
	}
	if pl.State == PlanSucceeded || pl.State == PlanFailed || pl.State == PlanAborted || pl.State == PlanTimedOut {
		return nil, nil, apiErr(CodeStateConflict, "计划 %s 已终态（%s），不能再批准", pl.ID, pl.State)
	}
	// 同角色已有有效批准：冲突，避免重复/越权签署。
	if pl.HasValidApproval(in.Role) {
		return nil, nil, apiErr(CodeConflict, "计划 %s 已有有效的 %s 批准", pl.ID, roleName(in.Role))
	}
	now := s.now()
	gates := evaluateGates(s.state, pl, now)
	if !gatesPass(gates) {
		return nil, gates, apiErr(CodePrecondition, "前置条件未全部满足: %s", describeGateFailure(gates))
	}
	// 另一席位的有效批准必须由不同操作人持有（双人复核）。
	for _, a := range pl.Approvals {
		if a.Valid && a.Operator == in.Operator {
			return nil, nil, apiErr(CodeConflict, "操作人 %s 已在本计划担任复核席位，双人复核必须由两人完成", in.Operator)
		}
	}
	frame := s.basisFrameLocked(pl.SatelliteID, in.SourceID)
	if frame == nil {
		return nil, nil, apiErr(CodePrecondition, "缺少已验证遥测作为批准依据")
	}
	win := s.state.Windows[pl.WindowID]
	cv := s.state.Commands[pl.CommandVersionID]
	sat := s.state.Satellites[pl.SatelliteID]
	sigs := map[string]string{}
	gens := map[string]int{}
	for _, sid := range pl.BasisSatellites() {
		if other := s.state.Satellites[sid]; other != nil {
			sigs[sid] = other.ConditionSig
			gens[sid] = other.SafeGeneration
		}
	}
	basis := Basis{
		FrameKey: frame.Key(), FrameHash: frame.Hash,
		ConditionSig:  sat.ConditionSig,
		ConditionSigs: sigs,
		WindowID:      win.ID, WindowVersion: win.Version, WindowState: win.State,
		SafeGeneration:   sat.SafeGeneration,
		SafeGenerations:  gens,
		CommandVersionID: cv.ID, CommandHash: cv.Hash,
		Gates: gates,
	}
	n := len(pl.Approvals) + 1
	a := Approval{
		ID:     fmt.Sprintf("apr-%s-%d", pl.ID, n),
		PlanID: pl.ID, Role: in.Role, Operator: in.Operator, Note: in.Note,
		At: now, Basis: basis, Valid: true,
	}
	if _, err := s.emit(EvPlanApproved, PlanApprovedPayload{Approval: a}); err != nil {
		return nil, nil, err
	}
	return clone(&a), clone(gates), nil
}

// CheckPlan 求值计划当前状态与门禁（只读）。
func (s *Service) CheckPlan(planID string) (*PlanStatus, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	pl := s.state.Plans[planID]
	if pl == nil {
		return nil, apiErr(CodeNotFound, "计划 %s 不存在", planID)
	}
	now := s.now()
	gates := evaluateGates(s.state, pl, now)
	st := &PlanStatus{Plan: clone(pl), Gates: clone(gates), GatesPassed: gatesPass(gates)}
	st.Ready = pl.HasValidApproval(RoleProposer) && pl.HasValidApproval(RoleVerifier)
	if pl.State == PlanExecuting {
		st.LockWinner = true
	} else {
		st.Ready = st.Ready && st.GatesPassed
	}
	if !st.Ready {
		switch {
		case !pl.HasValidApproval(RoleProposer):
			st.CanApproveRole = RoleProposer
		case !pl.HasValidApproval(RoleVerifier):
			st.CanApproveRole = RoleVerifier
		}
	}
	st.ConflictWith = s.lockConflictsLocked(pl)
	return st, nil
}

func roleName(r ApprovalRole) string {
	if r == RoleProposer {
		return "提议人"
	}
	return "复核人"
}
