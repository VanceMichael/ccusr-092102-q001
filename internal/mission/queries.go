package mission

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// ---- 视图辅助（均加锁，返回深拷贝，调用方可安全持有） ----

func (s *Service) GetSatellite(id string) (*SatelliteView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.sats[id]
	if st == nil {
		return nil, fmt.Errorf("%w: 卫星 %s 未注册", ErrUnknown, id)
	}
	v := s.satelliteView(st)
	return &v, nil
}

func (s *Service) ListSatellites() []SatelliteView {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(s.sats))
	for id := range s.sats {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]SatelliteView, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.satelliteView(s.sats[id]))
	}
	return out
}

func (s *Service) satelliteView(st *satState) SatelliteView {
	conds := map[ConditionKey]ConditionView{}
	for _, key := range []ConditionKey{CondAttitude, CondPower} {
		if c := st.cond[key]; c != nil && c.everEstablished {
			conds[key] = ConditionView{
				OK: c.ok, Value: c.value, Evidence: cloneRef(c.ref), Epoch: c.epoch,
			}
		} else {
			conds[key] = ConditionView{OK: false, Epoch: 0}
		}
	}
	v := SatelliteView{
		SatelliteID:    st.id,
		Name:           st.name,
		RegisteredAt:   st.registeredAt,
		SafeMode:       st.safe,
		Conditions:     conds,
		RequiredChecks: append([]CommandType(nil), st.required...),
		Gaps:           s.gapsForSat(st),
	}
	for _, p := range s.plansOfSat(st.id) {
		if p.openIssueID != "" {
			v.OpenIssues = append(v.OpenIssues, p.openIssueID)
		}
	}
	for _, i := range s.issues {
		if i.view.SatelliteID == st.id && (i.view.Status == IssueOpen || i.view.Status == IssueInProgress) {
			if !contains(v.OpenIssues, i.view.IssueID) {
				v.OpenIssues = append(v.OpenIssues, i.view.IssueID)
			}
		}
	}
	sort.Strings(v.OpenIssues)
	return v
}

func cloneRef(r *FrameRef) *FrameRef {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// gapsForSat 回答“这颗星还缺什么”。调用方持锁。
func (s *Service) gapsForSat(st *satState) []Gap {
	var gaps []Gap
	if st.safe {
		gaps = append(gaps, Gap{Code: "safe_mode_active", Message: "卫星处于安全模式，须先退出并确认"})
	}
	for _, key := range []ConditionKey{CondAttitude, CondPower} {
		c := st.cond[key]
		label := map[ConditionKey]string{CondAttitude: "姿态", CondPower: "能源"}[key]
		switch {
		case c == nil || !c.everEstablished:
			gaps = append(gaps, Gap{Code: "missing_" + string(key), Message: "缺少合格的" + label + "遥测依据"})
		case !c.ok:
			gaps = append(gaps, Gap{Code: string(key) + "_below_threshold",
				Message: fmt.Sprintf("%s读数 %.1f 低于门限 %.1f", label, c.value, st.thresholds[key])})
		}
	}
	openIssueSet := map[string]bool{}
	for _, i := range s.issues {
		if i.view.SatelliteID == st.id && (i.view.Status == IssueOpen || i.view.Status == IssueInProgress) {
			openIssueSet[i.view.IssueID] = true
			gaps = append(gaps, Gap{
				Code:    "open_issue_" + string(i.view.Type),
				Message: fmt.Sprintf("处置链 %s（%s）未闭环：%s", i.view.IssueID, i.view.Type, i.view.Reason),
			})
		}
	}
	for _, check := range st.required {
		done := false
		for _, p := range s.plansOfSat(st.id) {
			if p.commandType == check && p.status == PlanExecuted && p.receipt != nil && p.receipt.Success {
				done = true
				break
			}
		}
		if !done {
			gaps = append(gaps, Gap{
				Code:    "check_incomplete_" + string(check),
				Message: fmt.Sprintf("必做项目 %s 尚无成功执行回执", check),
			})
		}
	}
	_ = openIssueSet
	return gaps
}

// WindowView 窗口视图。
type WindowView struct {
	WindowID     string       `json:"window_id"`
	SatelliteID  string       `json:"satellite_id"`
	StationID    string       `json:"station_id"`
	Start        time.Time    `json:"start"`
	End          time.Time    `json:"end"`
	Status       WindowStatus `json:"status"`
	CancelReason string       `json:"cancel_reason,omitempty"`
}

func (s *Service) GetWindow(id string) (*WindowView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.windows[id]
	if w == nil {
		return nil, fmt.Errorf("%w: 窗口 %s 不存在", ErrUnknown, id)
	}
	v := s.windowView(w)
	return &v, nil
}

func (s *Service) ListWindows(satelliteID string) []WindowView {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []WindowView
	for _, w := range s.windows {
		if satelliteID != "" && w.satID != satelliteID {
			continue
		}
		out = append(out, s.windowView(w))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out
}

func (s *Service) windowView(w *windowState) WindowView {
	return WindowView{
		WindowID: w.id, SatelliteID: w.satID, StationID: w.stationID,
		Start: w.start, End: w.end, Status: w.status, CancelReason: w.cancelReason,
	}
}

func (s *Service) GetPlan(id string) (*PlanView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.plans[id]
	if p == nil {
		return nil, fmt.Errorf("%w: 计划 %s 不存在", ErrUnknown, id)
	}
	v := s.planView(p)
	return &v, nil
}

func (s *Service) ListPlans(satelliteID string) []PlanView {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []PlanView
	for _, id := range sortedPlanIDs(s.plans) {
		p := s.plans[id]
		if satelliteID != "" && p.satID != satelliteID {
			continue
		}
		out = append(out, s.planView(p))
	}
	return out
}

func (s *Service) planView(p *planState) PlanView {
	aps := make([]ApprovalView, 0, len(p.approvals))
	for _, a := range p.approvals {
		aps = append(aps, *cloneApproval(a))
	}
	v := PlanView{
		PlanID: p.id, SatelliteID: p.satID, WindowID: p.windowID,
		StationID: p.stationID, CommandType: p.commandType,
		PayloadDigest: p.digest, Revision: p.revision, Status: p.status,
		CreatedBy: p.createdBy, CreatedAt: p.createdAt, Approvals: aps,
		IssueIDs: append([]string(nil), p.issueIDs...),
	}
	if p.lease != nil {
		l := *p.lease
		v.ActiveLease = &l
	}
	if p.uplinkedAt != nil {
		t := *p.uplinkedAt
		v.UplinkedAt = &t
	}
	if p.receipt != nil {
		r := *p.receipt
		v.Receipt = &r
	}
	return v
}

func cloneApproval(a *ApprovalView) *ApprovalView {
	b := *a
	if a.TelemetryBasis != nil {
		r := *a.TelemetryBasis
		b.TelemetryBasis = &r
	}
	if a.InvalidAt != nil {
		t := *a.InvalidAt
		b.InvalidAt = &t
	}
	if a.EpochSnapshot != nil {
		b.EpochSnapshot = map[string]int{}
		for k, v := range a.EpochSnapshot {
			b.EpochSnapshot[k] = v
		}
	}
	return &b
}

func (s *Service) GetIssue(id string) (*IssueView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.issues[id]
	if i == nil {
		return nil, fmt.Errorf("%w: 处置链 %s 不存在", ErrUnknown, id)
	}
	v := i.view
	return &v, nil
}

func (s *Service) ListIssues(typ IssueType, openOnly bool) []IssueView {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []IssueView
	for _, i := range s.issues {
		if typ != "" && i.view.Type != typ {
			continue
		}
		if openOnly && i.view.Status != IssueOpen && i.view.Status != IssueInProgress {
			continue
		}
		out = append(out, cloneIssue(i.view))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenedAt.Before(out[j].OpenedAt) })
	return out
}

func cloneIssue(v IssueView) IssueView {
	b := v
	b.Steps = append([]IssueStep(nil), v.Steps...)
	if v.ResolvedAt != nil {
		t := *v.ResolvedAt
		b.ResolvedAt = &t
	}
	if v.Resumable != nil {
		b.Resumable = map[string]json.RawMessage{}
		for k, v := range v.Resumable {
			b.Resumable[k] = append(json.RawMessage(nil), v...)
		}
	}
	return b
}

func (s *Service) ListTelemetry(satelliteID string) ([]TelemetryFrame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.sats[satelliteID]
	if st == nil {
		return nil, fmt.Errorf("%w: 卫星 %s 未注册", ErrUnknown, satelliteID)
	}
	out := make([]TelemetryFrame, len(st.frames))
	copy(out, st.frames)
	sort.Slice(out, func(i, j int) bool {
		if out[i].ReceivedAt.Equal(out[j].ReceivedAt) {
			return out[i].TelemetryID < out[j].TelemetryID
		}
		return out[i].ReceivedAt.Before(out[j].ReceivedAt)
	})
	return out, nil
}

// ---- 交付签署与证据冻结 ----

type SignDeliveryParams struct {
	Scope        string // "satellite" 或 "group"
	SatelliteIDs []string
	Decision     Decision
	By           string
	Notes        string
}

func (s *Service) SignDelivery(p SignDeliveryParams) (*DeliveryView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.By == "" {
		return nil, badf("签署主管不能为空")
	}
	if len(p.SatelliteIDs) == 0 {
		return nil, badf("至少指定一颗卫星")
	}
	switch p.Decision {
	case DecisionAccepted, DecisionConditional, DecisionRejected:
	default:
		return nil, badf("交付结论必须是 accepted / conditional / rejected")
	}
	seen := map[string]bool{}
	for _, id := range p.SatelliteIDs {
		if s.sats[id] == nil {
			return nil, fmt.Errorf("%w: 卫星 %s 未注册", ErrUnknown, id)
		}
		if seen[id] {
			return nil, badf("卫星列表重复: %s", id)
		}
		seen[id] = true
	}
	// 签署瞬间冻结证据快照（深拷贝进入事件，此后不可变）。
	snapshot := s.buildEvidence(p.SatelliteIDs)
	if p.Decision == DecisionAccepted {
		for _, sat := range snapshot.Satellites {
			if len(sat.Gaps) > 0 {
				return nil, fmt.Errorf("%w: 卫星 %s 仍有 %d 项缺口，不能签署接收；可改签 conditional",
					ErrPrecondition, sat.SatelliteID, len(sat.Gaps))
			}
		}
	}
	ids := append([]string(nil), p.SatelliteIDs...)
	sort.Strings(ids)
	dv := DeliveryView{
		DeliveryID: s.allocID("DL"),
		Scope:      p.Scope, SatelliteIDs: ids, Decision: p.Decision,
		By: p.By, At: s.clock(), Notes: p.Notes, Evidence: snapshot,
	}
	if dv.Scope == "" {
		if len(ids) > 1 {
			dv.Scope = "group"
		} else {
			dv.Scope = "satellite"
		}
	}
	err := s.commit([]Event{event(evDeliverySigned, evDeliverySignedData{Delivery: dv})})
	if err != nil {
		return nil, err
	}
	out := *s.deliveries[dv.DeliveryID]
	return &out, nil
}

// buildEvidence 冻结给定卫星集的完整交付证据。调用方持锁。
func (s *Service) buildEvidence(satIDs []string) *EvidenceSnapshot {
	snap := &EvidenceSnapshot{FrozenAt: s.clock()}
	ids := append([]string(nil), satIDs...)
	sort.Strings(ids)
	for _, id := range ids {
		st := s.sats[id]
		se := SatelliteEvidence{
			SatelliteID: id,
			SafeMode:    st.safe,
			Conditions:  map[ConditionKey]ConditionView{},
			Gaps:        s.gapsForSat(st),
		}
		for _, key := range []ConditionKey{CondAttitude, CondPower} {
			if c := st.cond[key]; c != nil && c.everEstablished {
				se.Conditions[key] = ConditionView{
					OK: c.ok, Value: c.value, Evidence: cloneRef(c.ref), Epoch: c.epoch,
				}
			} else {
				se.Conditions[key] = ConditionView{OK: false}
			}
		}
		for _, pid := range sortedPlanIDs(s.plans) {
			p := s.plans[pid]
			if p.satID != id {
				continue
			}
			pe := PlanEvidence{
				PlanID: p.id, CommandType: p.commandType, Revision: p.revision,
				PayloadDigest: p.digest, Status: p.status,
				Approvals: make([]ApprovalView, 0, len(p.approvals)),
			}
			for _, a := range p.approvals {
				pe.Approvals = append(pe.Approvals, *cloneApproval(a))
			}
			if p.receipt != nil {
				r := *p.receipt
				pe.Receipt = &r
			}
			se.Plans = append(se.Plans, pe)
		}
		for _, i := range s.issues {
			if i.view.SatelliteID == id && (i.view.Status == IssueOpen || i.view.Status == IssueInProgress) {
				se.OpenIssues = append(se.OpenIssues, i.view.IssueID)
			}
		}
		sort.Strings(se.OpenIssues)
		snap.Satellites = append(snap.Satellites, se)
	}
	return snap
}

func (s *Service) GetDelivery(id string) (*DeliveryView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.deliveries[id]
	if d == nil {
		return nil, fmt.Errorf("%w: 交付结论 %s 不存在", ErrUnknown, id)
	}
	out := *d
	return &out, nil
}

func (s *Service) ListDeliveries() []DeliveryView {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []DeliveryView
	for _, d := range s.deliveries {
		out = append(out, *d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}
