package mission

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// commit 落盘一批事件并逐条投影。调用方须持有 s.mu。
func (s *Service) commit(evs []Event) error {
	now := s.clock()
	saved, err := s.store.Append(now, evs)
	if err != nil {
		return err
	}
	for _, ev := range saved {
		s.apply(ev)
	}
	return nil
}

func event(typ string, data any) Event {
	raw, _ := json.Marshal(data)
	return Event{Type: typ, Data: raw}
}

func badf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidInput, fmt.Sprintf(format, args...))
}

// ---- 卫星注册 ----

type RegisterSatelliteParams struct {
	SatelliteID    string
	Name           string
	RequiredChecks []CommandType
	Thresholds     map[ConditionKey]float64
}

func (s *Service) RegisterSatellite(p RegisterSatelliteParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.SatelliteID == "" {
		return badf("卫星标识不能为空")
	}
	if _, exists := s.sats[p.SatelliteID]; exists {
		return fmt.Errorf("%w: 卫星 %s 已注册", ErrConflict, p.SatelliteID)
	}
	now := s.clock()
	return s.commit([]Event{event(evSatelliteRegistered, evSatelliteRegisteredData{
		SatelliteID: p.SatelliteID, Name: p.Name,
		RequiredChecks: p.RequiredChecks, Thresholds: p.Thresholds, At: now,
	})})
}

// ---- 遥测接入 ----

type IngestTelemetryParams struct {
	SatelliteID    string                   `json:"satellite_id"`
	SourceID       string                   `json:"source_id"`
	SourceSequence int64                    `json:"source_sequence"`
	SpacecraftTime time.Time                `json:"spacecraft_time"`
	ReceivedAt     time.Time                `json:"received_at"`
	Quality        TelemetryQuality         `json:"quality"`
	Readings       map[ConditionKey]float64 `json:"readings"`
	SafeMode       bool                     `json:"safe_mode"`
}

// IngestTelemetry 接收一帧带星上时间/地面接收时间/源内序号/质量标记的遥测。
// 乱序或重复帧只存档、不影响已确认结论；前置条件变化会联动失效批准与执行权。
func (s *Service) IngestTelemetry(p IngestTelemetryParams) (*TelemetryFrame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.sats[p.SatelliteID]
	if st == nil {
		return nil, fmt.Errorf("%w: 卫星 %s 未注册", ErrUnknown, p.SatelliteID)
	}
	if p.SourceID == "" {
		return nil, badf("数据源标识不能为空")
	}
	if p.SourceSequence < 0 {
		return nil, badf("源内序号不能为负")
	}
	if p.SpacecraftTime.IsZero() || p.ReceivedAt.IsZero() {
		return nil, badf("星上时间与地面接收时间均必填")
	}
	if p.Quality == "" {
		p.Quality = QualityVerified
	}
	switch p.Quality {
	case QualityVerified, QualityDegraded, QualityBad:
	default:
		return nil, badf("未知质量标记 %q", p.Quality)
	}

	frame := TelemetryFrame{
		TelemetryID: s.allocID("TM"),
		SatelliteID: p.SatelliteID, SourceID: p.SourceID,
		SourceSequence: p.SourceSequence,
		SpacecraftTime: p.SpacecraftTime, ReceivedAt: p.ReceivedAt,
		Quality: p.Quality, Readings: p.Readings, SafeMode: p.SafeMode,
	}
	if ok, reason := s.frameAcceptance(st, frame); ok {
		frame.Applied = true
	} else {
		frame.StaleReason = reason
	}

	oldEpochs := s.epochSnapshot(st)
	oldSafe := st.safe

	if err := s.commit([]Event{event(evTelemetryIngested, evTelemetryIngestedData{Frame: frame})}); err != nil {
		return nil, err
	}

	// 投影后，依据纪元变化联动失效相关批准 / 撤销执行权。
	if err := s.afterTelemetry(st, frame, oldEpochs, oldSafe); err != nil {
		return nil, err
	}
	return &frame, nil
}

// afterTelemetry 处理遥测引发的派生事件：
// 安全模式翻转或姿态/能源结论纪元推进 → 失效该星全部现行有效批准；
// 执行中的计划撞上安全模式 → 撤销执行权并开启安全模式处置链。
func (s *Service) afterTelemetry(st *satState, f TelemetryFrame, oldEpochs map[string]int, oldSafe bool) error {
	if !f.Applied {
		return nil
	}
	newEpochs := s.epochSnapshot(st)
	changed := oldSafe != st.safe
	for k, v := range newEpochs {
		if oldEpochs[k] != v {
			changed = true
		}
	}
	if !changed {
		return nil
	}
	var reason string
	switch {
	case f.SafeMode:
		reason = "卫星进入安全模式"
	case oldSafe && !st.safe:
		reason = "卫星退出安全模式，前置条件纪元已推进"
	default:
		reason = "姿态/能源前置条件结论变化，纪元已推进"
	}
	var evs []Event
	for _, p := range s.plansOfSat(st.id) {
		if p.status.IsTerminal() {
			continue
		}
		var ids []string
		for _, a := range p.approvals {
			if !a.Invalidated && a.PlanRevision == p.revision {
				ids = append(ids, a.ApprovalID)
			}
		}
		if len(ids) > 0 {
			evs = append(evs, event(evApprovalsInvalidated, evApprovalsInvalidatedData{
				PlanID: p.id, Reason: reason, ApprovalIDs: ids,
			}))
		}
		if f.SafeMode && p.status == PlanExecuting {
			issueID := s.allocID("IS")
			evs = append(evs,
				event(evLeaseRevoked, evLeaseRevokedData{
					PlanID: p.id, Reason: reason, IssueID: issueID,
				}),
				event(evIssueOpened, evIssueOpenedData{Issue: s.newIssue(
					issueID, IssueSafeMode, st.id, p.id, p.windowID,
					"执行期间卫星进入安全模式，执行权已自动撤销",
					map[string]json.RawMessage{
						"resume": mustJSON(map[string]string{"action": "等待安全模式退出确认后重新双签并执行"}),
					},
				)}),
			)
		}
	}
	if len(evs) == 0 {
		return nil
	}
	return s.commit(evs)
}

func (s *Service) plansOfSat(satID string) []*planState {
	var out []*planState
	for _, p := range s.plans {
		if p.satID == satID {
			out = append(out, p)
		}
	}
	return out
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// ---- 测控窗口 ----

type ScheduleWindowParams struct {
	WindowID    string
	SatelliteID string
	StationID   string
	Start       time.Time
	End         time.Time
}

func (s *Service) ScheduleWindow(p ScheduleWindowParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.WindowID == "" || p.SatelliteID == "" || p.StationID == "" {
		return badf("窗口、卫星、地面站标识均必填")
	}
	if s.windows[p.WindowID] != nil {
		return fmt.Errorf("%w: 窗口 %s 已存在", ErrConflict, p.WindowID)
	}
	if s.sats[p.SatelliteID] == nil {
		return fmt.Errorf("%w: 卫星 %s 未注册", ErrUnknown, p.SatelliteID)
	}
	if !p.End.After(p.Start) {
		return badf("窗口结束时间必须晚于开始时间")
	}
	if !p.End.After(s.clock()) {
		return badf("窗口结束时间已过，无法安排测控窗口")
	}
	return s.commit([]Event{event(evWindowScheduled, evWindowScheduledData{
		WindowID: p.WindowID, SatelliteID: p.SatelliteID,
		StationID: p.StationID, Start: p.Start, End: p.End, At: s.clock(),
	})})
}

// CancelWindow 取消窗口：引用该窗口的非终态计划全部进入窗口取消处置链，
// 执行中的计划先撤销执行权。
func (s *Service) CancelWindow(windowID, by, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.windows[windowID]
	if w == nil {
		return fmt.Errorf("%w: 窗口 %s 不存在", ErrUnknown, windowID)
	}
	if w.status == WinCancelled {
		return fmt.Errorf("%w: 窗口已取消", ErrConflict)
	}
	if err := s.sweepLocked(s.clock()); err != nil {
		return err
	}
	now := s.clock()
	var evs []Event
	evs = append(evs, event(evWindowCancelled, evWindowCancelledData{
		WindowID: windowID, At: now, Reason: reason, By: by,
	}))
	for _, p := range s.plans {
		if p.windowID != windowID || p.status.IsTerminal() {
			continue
		}
		if s.activeApprovals(p) > 0 {
			var ids []string
			for _, a := range p.approvals {
				if !a.Invalidated && a.PlanRevision == p.revision {
					ids = append(ids, a.ApprovalID)
				}
			}
			if len(ids) > 0 {
				evs = append(evs, event(evApprovalsInvalidated, evApprovalsInvalidatedData{
					PlanID: p.id, Reason: "测控窗口已取消", ApprovalIDs: ids,
				}))
			}
		}
		issueID := s.allocID("IS")
		restore := PlanSubmitted
		if p.status == PlanDraft {
			restore = PlanDraft
		}
		if p.status == PlanExecuting {
			evs = append(evs, event(evLeaseRevoked, evLeaseRevokedData{
				PlanID: p.id, Reason: "测控窗口已取消", IssueID: issueID,
			}))
		}
		evs = append(evs, event(evIssueOpened, evIssueOpenedData{Issue: s.newIssue(
			issueID, IssueWindowCancel, p.satID, p.id, windowID,
			"窗口 "+windowID+" 已取消："+reason,
			map[string]json.RawMessage{
				"restore_to":  mustJSON(string(restore)),
				"old_window":  mustJSON(windowID),
				"old_station": mustJSON(p.stationID),
			},
		)}))
	}
	return s.commit(evs)
}

// CloseWindow 正常关闭已结束的窗口。
func (s *Service) CloseWindow(windowID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.windows[windowID]
	if w == nil {
		return fmt.Errorf("%w: 窗口 %s 不存在", ErrUnknown, windowID)
	}
	if w.status != WinScheduled {
		return fmt.Errorf("%w: 窗口状态为 %s，不可关闭", ErrNotAcceptable, w.status)
	}
	return s.commit([]Event{event(evWindowClosed, evWindowClosedData{
		WindowID: windowID, At: s.clock(),
	})})
}

// ---- 指令计划 ----

type CreatePlanParams struct {
	SatelliteID string
	WindowID    string
	CommandType CommandType
	Payload     string
	Digest      string
	CreatedBy   string
}

func (s *Service) CreatePlan(p CreatePlanParams) (*PlanView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sats[p.SatelliteID] == nil {
		return nil, fmt.Errorf("%w: 卫星 %s 未注册", ErrUnknown, p.SatelliteID)
	}
	w := s.windows[p.WindowID]
	if w == nil {
		return nil, fmt.Errorf("%w: 窗口 %s 不存在", ErrUnknown, p.WindowID)
	}
	if w.satID != p.SatelliteID {
		return nil, badf("窗口不属于该卫星")
	}
	if w.status != WinScheduled {
		return nil, fmt.Errorf("%w: 窗口状态为 %s", ErrNotAcceptable, w.status)
	}
	switch p.CommandType {
	case CmdHealthCheck, CmdPayloadPowerOn, CmdCrossCalibration, CmdOther:
	default:
		return nil, badf("未知指令类型 %q", p.CommandType)
	}
	if p.CreatedBy == "" {
		return nil, badf("创建人不能为空")
	}
	digest := p.Digest
	if digest == "" && p.Payload != "" {
		sum := sha256.Sum256([]byte(p.Payload))
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	if digest == "" {
		return nil, badf("指令内容或载荷摘要至少提供一项")
	}
	id := s.allocID("PL")
	err := s.commit([]Event{event(evPlanCreated, evPlanCreatedData{
		PlanID: id, SatelliteID: p.SatelliteID, WindowID: p.WindowID,
		CommandType: p.CommandType, PayloadDigest: digest, Payload: p.Payload,
		CreatedBy: p.CreatedBy, At: s.clock(),
	})})
	if err != nil {
		return nil, err
	}
	v := s.planView(s.plans[id])
	return &v, nil
}

// SubmitPlan 提交计划进入双人复核。
func (s *Service) SubmitPlan(planID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.mustPlan(planID)
	if err != nil {
		return err
	}
	if p.status != PlanDraft {
		return fmt.Errorf("%w: 计划状态为 %s，不可提交", ErrNotAcceptable, p.status)
	}
	return s.commit([]Event{event(evPlanSubmitted, evPlanSubmittedData{
		PlanID: planID, Revision: p.revision, At: s.clock(),
	})})
}

// RevisePlan 重订指令版本：新版本号自增，旧版本批准全部失效。
func (s *Service) RevisePlan(planID, payload, digest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.mustPlan(planID)
	if err != nil {
		return err
	}
	if p.status != PlanSubmitted && p.status != PlanAwaiting && p.status != PlanApproved {
		return fmt.Errorf("%w: 计划状态为 %s，不可重订版本", ErrNotAcceptable, p.status)
	}
	if digest == "" && payload != "" {
		sum := sha256.Sum256([]byte(payload))
		digest = "sha256:" + hex.EncodeToString(sum[:])
	}
	if digest == "" || digest == p.digest {
		return badf("新版本内容摘要缺失或与当前版本相同")
	}
	var evs []Event
	newRev := p.revision + 1
	evs = append(evs, event(evPlanRevised, evPlanRevisedData{
		PlanID: planID, Revision: newRev, PayloadDigest: digest, Payload: payload, At: s.clock(),
	}))
	if s.activeApprovals(p) > 0 {
		var ids []string
		for _, a := range p.approvals {
			if !a.Invalidated && a.PlanRevision == p.revision {
				ids = append(ids, a.ApprovalID)
			}
		}
		if len(ids) > 0 {
			evs = append(evs, event(evApprovalsInvalidated, evApprovalsInvalidatedData{
				PlanID: planID, Reason: fmt.Sprintf("指令版本由 r%d 更新为 r%d", p.revision, newRev),
				ApprovalIDs: ids,
			}))
		}
	}
	return s.commit(evs)
}

// RejectPlan 复核人拒绝计划（终态）。
func (s *Service) RejectPlan(planID, by, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.mustPlan(planID)
	if err != nil {
		return err
	}
	if p.status != PlanSubmitted {
		return fmt.Errorf("%w: 计划状态为 %s，不可拒绝", ErrNotAcceptable, p.status)
	}
	if by == "" {
		return badf("复核人不能为空")
	}
	return s.commit([]Event{event(evPlanRejected, evPlanRejectedData{
		PlanID: planID, By: by, Reason: reason, At: s.clock(),
	})})
}

// ---- 双人复核 ----

// AddApproval 增加一个席位的批准。每个计划需主操作席与复核席两个不同的人各批一次，
// 且批准瞬间姿态/能源/安全模式条件全部满足；批准冻结当时纪元与遥测依据。
func (s *Service) AddApproval(planID, role, approver string) (*ApprovalView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, err := s.mustPlan(planID)
	if err != nil {
		return nil, err
	}
	if role != "primary" && role != "reviewer" {
		return nil, badf("席位角色必须是 primary 或 reviewer")
	}
	if approver == "" {
		return nil, badf("批准人不能为空")
	}
	if p.status != PlanSubmitted {
		return nil, fmt.Errorf("%w: 计划状态为 %s，不可批准", ErrNotAcceptable, p.status)
	}
	if w := s.windows[p.windowID]; w == nil || w.status != WinScheduled {
		return nil, fmt.Errorf("%w: 测控窗口已失效", ErrPrecondition)
	}
	st := s.sats[p.satID]
	if err := s.checkPreconditions(st); err != nil {
		return nil, err
	}
	for _, a := range p.approvals {
		if a.Invalidated || a.PlanRevision != p.revision {
			continue
		}
		if a.Role == role {
			return nil, fmt.Errorf("%w: %s 席已由 %s 批准本版本", ErrConflict, role, a.Approver)
		}
		if a.Approver == approver {
			return nil, fmt.Errorf("%w: 双人复核要求两个不同的人，%s 已批过", ErrConflict, approver)
		}
	}
	basis := s.latestBasis(st)
	ap := &ApprovalView{
		ApprovalID: s.allocID("AP"), PlanRevision: p.revision,
		Role: role, Approver: approver, At: s.clock(),
		WindowID: p.windowID, EpochSnapshot: s.epochSnapshot(st),
		TelemetryBasis: basis,
	}
	err = s.commit([]Event{event(evApprovalAdded, evApprovalAddedData{
		PlanID: p.id, ApprovalID: ap.ApprovalID, PlanRevision: ap.PlanRevision,
		Role: role, Approver: approver, At: ap.At, WindowID: p.windowID,
		EpochSnapshot: ap.EpochSnapshot, TelemetryBasis: derefRef(basis),
	})})
	if err != nil {
		return nil, err
	}
	return ap, nil
}

func derefRef(r *FrameRef) FrameRef {
	if r == nil {
		return FrameRef{}
	}
	return *r
}

func (s *Service) checkPreconditions(st *satState) error {
	if st.safe {
		return fmt.Errorf("%w: 卫星处于安全模式", ErrPrecondition)
	}
	for _, key := range []ConditionKey{CondAttitude, CondPower} {
		c := st.cond[key]
		if c == nil || !c.has {
			return fmt.Errorf("%w: 缺少 %s 遥测依据", ErrPrecondition, key)
		}
		if !c.ok {
			return fmt.Errorf("%w: %s 读数 %.1f 低于门限 %.1f", ErrPrecondition, key, c.value, st.thresholds[key])
		}
	}
	return nil
}

// latestBasis 取当前条件依据中星上时间最新的一帧，作为批准遥测依据。
func (s *Service) latestBasis(st *satState) *FrameRef {
	var best *FrameRef
	for _, key := range []ConditionKey{CondAttitude, CondPower} {
		if c := st.cond[key]; c != nil && c.has {
			r := *c.ref
			if best == nil || r.SpacecraftTime.After(best.SpacecraftTime) {
				best = &r
			}
		}
	}
	return best
}

// ---- 执行权与上注 ----

// ExecutePlan 双签齐备后取得卫星与地面站执行权（任一被占即拒绝），
// 指令上注并进入执行中；执行权带截止时间，超时自动进入处置链。
func (s *Service) ExecutePlan(planID, seat string) (*Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sweepLocked(s.clock()); err != nil {
		return nil, err
	}
	p, err := s.mustPlan(planID)
	if err != nil {
		return nil, err
	}
	if seat == "" {
		return nil, badf("执行席位不能为空")
	}
	if p.status != PlanApproved {
		return nil, fmt.Errorf("%w: 计划状态为 %s，需双人复核通过", ErrNotAcceptable, p.status)
	}
	if s.activeApprovals(p) != 2 {
		return nil, fmt.Errorf("%w: 有效批准不足 2 个", ErrPrecondition)
	}
	w := s.windows[p.windowID]
	if w == nil || w.status != WinScheduled {
		return nil, fmt.Errorf("%w: 测控窗口不可用", ErrPrecondition)
	}
	now := s.clock()
	if now.Before(w.start) || now.After(w.end) {
		return nil, fmt.Errorf("%w: 当前时间不在测控窗口内", ErrPrecondition)
	}
	st := s.sats[p.satID]
	if err := s.checkPreconditions(st); err != nil {
		return nil, err
	}
	if l, ok := s.satLease[p.satID]; ok && !l.lease.Released {
		return nil, fmt.Errorf("%w: 卫星 %s 正由计划 %s（席位 %s）占用",
			ErrConflict, p.satID, l.planID, l.lease.Seat)
	}
	if l, ok := s.stationLease[p.stationID]; ok && !l.lease.Released {
		return nil, fmt.Errorf("%w: 地面站 %s 正由计划 %s（席位 %s）占用",
			ErrConflict, p.stationID, l.planID, l.lease.Seat)
	}
	deadline := now.Add(s.cfg.LeaseTTL)
	if w.end.Before(deadline) {
		deadline = w.end
	}
	lease := Lease{
		LeaseID: s.allocID("LS"), PlanID: p.id, SatelliteID: p.satID,
		StationID: p.stationID, Seat: seat,
		AcquiredAt: now, Deadline: deadline,
	}
	err = s.commit([]Event{event(evPlanUplinked, evPlanUplinkedData{
		PlanID: p.id, Lease: lease, At: now, CommandRev: p.revision,
	})})
	if err != nil {
		return nil, err
	}
	return &lease, nil
}

// RecordReceipt 记录星上执行回执并释放执行权。
func (s *Service) RecordReceipt(planID string, r Receipt) (*PlanView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.sweepLocked(s.clock()); err != nil {
		return nil, err
	}
	p, err := s.mustPlan(planID)
	if err != nil {
		return nil, err
	}
	if p.status != PlanExecuting {
		return nil, fmt.Errorf("%w: 计划状态为 %s，无执行权可回执", ErrNotAcceptable, p.status)
	}
	if r.SpacecraftTime.IsZero() || r.ReceivedAt.IsZero() {
		return nil, badf("回执需含星上时间与地面接收时间")
	}
	// 允许星上时钟少量偏差，但回执时间不能早于上注前 1 分钟。
	if r.SpacecraftTime.Before(p.uplinkedAt.Add(-time.Minute)) {
		return nil, badf("回执星上时间早于指令上注时间")
	}
	err = s.commit([]Event{event(evReceiptRecorded, evReceiptRecordedData{
		PlanID: planID, Receipt: r, At: s.clock(),
	})})
	if err != nil {
		return nil, err
	}
	v := s.planView(s.plans[planID])
	return &v, nil
}

// Sweep 显式推进时钟：把已过截止时间仍在执行的计划转入超时处置链。
func (s *Service) Sweep() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sweepLocked(s.clock())
}

func (s *Service) sweepLocked(now time.Time) error {
	var evs []Event
	for _, p := range s.plans {
		if p.status != PlanExecuting || p.lease == nil || p.lease.Released {
			continue
		}
		if now.After(p.lease.Deadline) {
			issueID := s.allocID("IS")
			if s.activeApprovals(p) > 0 {
				var ids []string
				for _, a := range p.approvals {
					if !a.Invalidated && a.PlanRevision == p.revision {
						ids = append(ids, a.ApprovalID)
					}
				}
				if len(ids) > 0 {
					evs = append(evs, event(evApprovalsInvalidated, evApprovalsInvalidatedData{
						PlanID: p.id, Reason: "执行超时，原双人复核结论失效，须重新复核", ApprovalIDs: ids,
					}))
				}
			}
			evs = append(evs,
				event(evLeaseExpired, evLeaseExpiredData{PlanID: p.id, IssueID: issueID, At: now}),
				event(evIssueOpened, evIssueOpenedData{Issue: s.newIssue(
					issueID, IssueTimeout, p.satID, p.id, p.windowID,
					fmt.Sprintf("执行超过截止时间 %s 未收到星上回执", p.lease.Deadline.Format(time.RFC3339)),
					map[string]json.RawMessage{
						"deadline":   mustJSON(p.lease.Deadline),
						"resume":     mustJSON(map[string]string{"action": "确认新测控窗口后续办，重新双人复核"}),
						"old_window": mustJSON(p.windowID),
					},
				)}),
			)
		}
	}
	if len(evs) == 0 {
		return nil
	}
	return s.commit(evs)
}

func (s *Service) mustPlan(planID string) (*planState, error) {
	p := s.plans[planID]
	if p == nil {
		return nil, fmt.Errorf("%w: 计划 %s 不存在", ErrUnknown, planID)
	}
	return p, nil
}

// IsTerminal 计划是否处于终态。
func (st PlanStatus) IsTerminal() bool {
	switch st {
	case PlanExecuted, PlanFailed, PlanRejected, PlanCancelled:
		return true
	}
	return false
}

// ---- 处置链续办 ----

// WorkIssue 向处置链追加处置记录（值班交接留痕）。
func (s *Service) WorkIssue(issueID, by, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, err := s.mustIssue(issueID)
	if err != nil {
		return err
	}
	if i.view.Status != IssueOpen && i.view.Status != IssueInProgress {
		return fmt.Errorf("%w: 处置链状态为 %s", ErrNotAcceptable, i.view.Status)
	}
	return s.commit([]Event{event(evIssueWorked, evIssueWorkedData{
		IssueID: issueID, Step: IssueStep{At: s.clock(), By: by, Note: note},
	})})
}

// ResolveIssueParams 续办处置链。
type ResolveIssueParams struct {
	IssueID     string
	By          string
	Resolution  string
	NewWindowID string // 超时/窗口取消链：指定新的测控窗口续办
}

// ResolveIssue 续办处置链：
//   - safe_mode：须已收到安全模式退出遥测；计划回到待提交，重新双人复核后续办；
//   - timeout / window_cancel：须指定同星的有效新窗口，计划改挂新窗口后重新双签。
func (s *Service) ResolveIssue(p ResolveIssueParams) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, err := s.mustIssue(p.IssueID)
	if err != nil {
		return err
	}
	if i.view.Status != IssueOpen && i.view.Status != IssueInProgress {
		return fmt.Errorf("%w: 处置链状态为 %s，不可续办", ErrNotAcceptable, i.view.Status)
	}
	now := s.clock()
	var evs []Event

	switch i.view.Type {
	case IssueSafeMode:
		st := s.sats[i.view.SatelliteID]
		if st.safe {
			return fmt.Errorf("%w: 尚未收到安全模式退出确认遥测", ErrPrecondition)
		}
		if planID := i.view.PlanID; planID != "" {
			pl := s.plans[planID]
			if pl != nil && pl.status == PlanAwaiting {
				evs = append(evs, event(evPlanReassigned, evPlanReassignedData{
					PlanID: pl.id, WindowID: pl.windowID,
					RestoreTo: PlanSubmitted, IssueID: p.IssueID, At: now,
				}))
			}
		}
	case IssueTimeout, IssueWindowCancel:
		w := s.windows[p.NewWindowID]
		if w == nil {
			return fmt.Errorf("%w: 续办必须指定有效的新测控窗口", ErrPrecondition)
		}
		if w.satID != i.view.SatelliteID {
			return badf("新窗口不属于该卫星")
		}
		if w.status != WinScheduled {
			return fmt.Errorf("%w: 新窗口状态为 %s", ErrPrecondition, w.status)
		}
		if !w.end.After(now) {
			return fmt.Errorf("%w: 新窗口已结束", ErrPrecondition)
		}
		pl := s.plans[i.view.PlanID]
		if pl != nil && !pl.status.IsTerminal() {
			restore := pl.status
			if restore == PlanAwaiting {
				restore = PlanSubmitted // 超时/撤销执行权后续办，回到待双签
			}
			evs = append(evs, event(evPlanReassigned, evPlanReassignedData{
				PlanID: pl.id, WindowID: w.id, RestoreTo: restore,
				IssueID: p.IssueID, At: now,
			}))
		}
	default:
		return badf("未知处置链类型")
	}

	evs = append(evs, event(evIssueResolved, evIssueResolvedData{
		IssueID: p.IssueID, Resolution: p.Resolution, By: p.By, At: now,
	}))
	return s.commit(evs)
}

// FailIssue 将处置链标记为失败终态（无法续办时留痕）。
func (s *Service) FailIssue(issueID, by, resolution string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i, err := s.mustIssue(issueID)
	if err != nil {
		return err
	}
	if i.view.Status != IssueOpen && i.view.Status != IssueInProgress {
		return fmt.Errorf("%w: 处置链状态为 %s", ErrNotAcceptable, i.view.Status)
	}
	return s.commit([]Event{event(evIssueFailed, evIssueFailedData{
		IssueID: issueID, Resolution: resolution, By: by, At: s.clock(),
	})})
}

func (s *Service) mustIssue(id string) (*issueState, error) {
	i := s.issues[id]
	if i == nil {
		return nil, fmt.Errorf("%w: 处置链 %s 不存在", ErrUnknown, id)
	}
	return i, nil
}

func (s *Service) newIssue(id string, typ IssueType, satID, planID, windowID, reason string,
	resumable map[string]json.RawMessage) IssueView {
	return IssueView{
		IssueID: id, Type: typ, Status: IssueOpen,
		SatelliteID: satID, PlanID: planID, WindowID: windowID,
		Reason: reason, OpenedAt: s.clock(), Steps: []IssueStep{},
		Resumable: resumable,
	}
}
