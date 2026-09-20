package mission

import (
	"fmt"
	"sort"
)

// Readiness 为单星交付就绪度视图。
type Readiness struct {
	SatelliteID     string                    `json:"satellite_id"`
	Name            string                    `json:"name"`
	Ready           bool                      `json:"ready"`
	Safe            bool                      `json:"safe"`
	SafeReason      string                    `json:"safe_reason,omitempty"`
	Health          *HealthConfirmation       `json:"health,omitempty"`
	Milestones      map[Milestone]bool        `json:"milestones"`
	Missing         []MissingItem             `json:"missing"`
	OpenCases       []*Case                   `json:"open_cases"`
	CommandVersions []*CommandVersionEvidence `json:"command_versions"`
	Plans           []*Plan                   `json:"plans"`
}

// SignDeliveryInput 为交付签署输入。
type SignDeliveryInput struct {
	Scope       string             `json:"scope"` // satellite | group
	SatelliteID string             `json:"satellite_id,omitempty"`
	Conclusion  DeliveryConclusion `json:"conclusion"`
	Signer      string             `json:"signer"`
	Note        string             `json:"note"`
}

// SignDelivery 主管签署单星或整组交付结论，签署瞬间冻结全部所用证据。
// accepted 要求每颗星无缺失项；rejected 任何时候可签，证据同样冻结。
func (s *Service) SignDelivery(in SignDeliveryInput) (*DeliveryRecord, error) {
	if in.Signer == "" {
		return nil, apiErr(CodeValidation, "签署人不能为空")
	}
	if in.Conclusion != DeliveryAccepted && in.Conclusion != DeliveryRejected {
		return nil, apiErr(CodeValidation, "结论必须是 accepted 或 rejected")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var ids []string
	switch in.Scope {
	case "satellite":
		if in.SatelliteID == "" {
			return nil, apiErr(CodeValidation, "单星签署必须提供 satellite_id")
		}
		if _, ok := s.state.Satellites[in.SatelliteID]; !ok {
			return nil, apiErr(CodeNotFound, "卫星 %s 不存在", in.SatelliteID)
		}
		ids = []string{in.SatelliteID}
	case "group", "":
		in.Scope = "group"
		for id := range s.state.Satellites {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		if len(ids) == 0 {
			return nil, apiErr(CodeStateConflict, "尚无注册卫星，无法整组签署")
		}
	default:
		return nil, apiErr(CodeValidation, "非法签署范围 %q", in.Scope)
	}

	bundle := s.buildBundleLocked(ids)
	if in.Conclusion == DeliveryAccepted {
		for _, se := range bundle.Satellites {
			if len(se.Missing) > 0 {
				return nil, apiErr(CodePrecondition,
					"卫星 %s 尚有 %d 项缺失（首项: %s），不能签署接收", se.SatelliteID, len(se.Missing), se.Missing[0].Detail)
			}
		}
	}
	n := len(s.state.Deliveries) + 1
	rec := DeliveryRecord{
		ID: fmt.Sprintf("dlv-%d", n), Scope: in.Scope, SatelliteIDs: ids,
		Conclusion: in.Conclusion, Signer: in.Signer, Note: in.Note,
		SignedAt: s.now(), Bundle: bundle,
	}
	if _, err := s.emit(EvDeliverySigned, DeliverySignedPayload{Record: rec}); err != nil {
		return nil, err
	}
	return clone(&rec), nil
}

// buildBundleLocked 冻结一组卫星的全部交付证据；调用方持写锁。
func (s *Service) buildBundleLocked(ids []string) *EvidenceBundle {
	se := make([]*SatelliteEvidence, 0, len(ids))
	for _, id := range ids {
		se = append(se, s.buildSatelliteEvidenceLocked(id))
	}
	b := &EvidenceBundle{Satellites: se}
	b.Hash = StableHash(b.satelliteHashes())
	return b
}

func (b *EvidenceBundle) satelliteHashes() []map[string]any {
	out := make([]map[string]any, 0, len(b.Satellites))
	for _, se := range b.Satellites {
		out = append(out, map[string]any{"satellite_id": se.SatelliteID, "missing": se.Missing,
			"frame": se.CurrentFrameHash(), "milestones": se.MilestoneStatus()})
	}
	return out
}

// CurrentFrameHash 暴露冻结帧指纹。
func (se *SatelliteEvidence) CurrentFrameHash() string {
	if se.CurrentFrame == nil {
		return ""
	}
	return se.CurrentFrame.Hash
}

// MilestoneStatus 暴露里程碑完成情况。
func (se *SatelliteEvidence) MilestoneStatus() map[string]bool {
	out := map[string]bool{}
	for _, m := range se.Milestones {
		out[string(m.Milestone)] = m.Complete
	}
	return out
}

// buildSatelliteEvidenceLocked 组装单星证据并计算缺失项。
func (s *Service) buildSatelliteEvidenceLocked(id string) *SatelliteEvidence {
	sat := s.state.Satellites[id]
	se := &SatelliteEvidence{SatelliteID: id, Name: sat.Name, Safe: sat.Safe, SafeReason: sat.SafeReason}

	if f := latestVerifiedFrame(s.state, id); f != nil {
		fc := *f
		se.CurrentFrame = &fc
	}
	if sat.Health != nil {
		h := *sat.Health
		se.Health = &h
	}

	// 里程碑
	for _, m := range RequiredMilestones {
		me := &MilestoneEvidence{Milestone: m, Name: MilestoneName(m)}
		if rec, ok := sat.Milestones[m]; ok {
			me.Complete = true
			me.PlanID = rec.PlanID
			me.Detail = rec.Detail
		}
		se.Milestones = append(se.Milestones, me)
	}

	// 该星全部窗口（按开始时间）
	for _, w := range s.state.Windows {
		if w.SatelliteID == id {
			se.Windows = append(se.Windows, *w)
		}
	}
	sort.Slice(se.Windows, func(i, j int) bool { return se.Windows[i].Start.Before(se.Windows[j].Start) })

	// 该星全部计划，含“谁批准过哪版指令”与“实际执行结果”
	var planIDs []string
	for pid, pl := range s.state.Plans {
		if pl.SatelliteID == id {
			planIDs = append(planIDs, pid)
		}
	}
	sortPlanIDsByTime(s.state, planIDs)
	cmdByID := map[string]*CommandVersionEvidence{}
	for _, pid := range planIDs {
		pl := s.state.Plans[pid]
		pc := clone(pl)
		se.Plans = append(se.Plans, pc)
		cv := s.state.Commands[pl.CommandVersionID]
		if cv == nil {
			continue
		}
		ce := cmdByID[cv.ID]
		if ce == nil {
			cvc := *cv
			ce = &CommandVersionEvidence{Version: cvc}
			cmdByID[cv.ID] = ce
		}
		for _, a := range pl.Approvals {
			ce.Approvals = append(ce.Approvals, &ApprovalEvidence{
				ID: a.ID, PlanID: pl.ID, PlanKind: pl.Kind, Role: a.Role,
				Operator: a.Operator, Note: a.Note, At: a.At, Valid: a.Valid,
				InvalidatedReason: a.InvalidatedReason, Basis: a.Basis,
			})
		}
		if pl.State == PlanSucceeded || pl.State == PlanFailed || pl.State == PlanAborted || pl.State == PlanTimedOut {
			se.Receipts = append(se.Receipts, &ReceiptEvidence{
				PlanID: pl.ID, Kind: pl.Kind, Milestone: pl.Milestone, State: pl.State,
				Result: clone(pl.Result), Attempts: clone(pl.Attempts), CommandHash: cv.Hash,
			})
		}
	}
	for _, ce := range cmdByID {
		sort.Slice(ce.Approvals, func(i, j int) bool { return ce.Approvals[i].At.Before(ce.Approvals[j].At) })
		se.CommandVersions = append(se.CommandVersions, ce)
	}
	sort.Slice(se.CommandVersions, func(i, j int) bool {
		if se.CommandVersions[i].Version.Command == se.CommandVersions[j].Version.Command {
			return se.CommandVersions[i].Version.Revision < se.CommandVersions[j].Version.Revision
		}
		return se.CommandVersions[i].Version.Command < se.CommandVersions[j].Version.Command
	})
	sort.Slice(se.Receipts, func(i, j int) bool {
		ri, rj := se.Receipts[i].Result, se.Receipts[j].Result
		if ri == nil || rj == nil {
			return se.Receipts[i].PlanID < se.Receipts[j].PlanID
		}
		return ri.At.Before(rj.At)
	})

	// 未关闭处置链
	for _, cid := range s.state.SortedCaseIDs() {
		c := s.state.Cases[cid]
		if c.SatelliteID == id && c.State == CaseOpen {
			se.OpenCases = append(se.OpenCases, clone(c))
		}
	}

	// 缺失项（健康确认已原子登记“健康确认”里程碑，此处不重复列示）。
	se.Missing = s.computeMissingLocked(sat, se)
	return se
}

func (s *Service) computeMissingLocked(sat *Satellite, se *SatelliteEvidence) []MissingItem {
	var missing []MissingItem
	add := func(code, detail string) {
		missing = append(missing, MissingItem{Code: code, Detail: detail})
	}
	if sat.Safe {
		add("safe_mode", "卫星处于安全模式: "+sat.SafeReason)
	}
	if se.CurrentFrame == nil {
		add("no_verified_telemetry", "缺少已验证遥测帧")
	}
	for _, me := range se.Milestones {
		if !me.Complete {
			add("milestone_missing:"+string(me.Milestone), "缺少里程碑: "+me.Name)
		}
	}
	for _, c := range se.OpenCases {
		add("open_case:"+string(c.Kind), fmt.Sprintf("处置链未关闭: %s（%s）", c.ID, c.Summary))
	}
	return missing
}

func sortPlanIDsByTime(st *State, ids []string) {
	sort.Slice(ids, func(i, j int) bool {
		pi, pj := st.Plans[ids[i]], st.Plans[ids[j]]
		if pi.CreatedAt.Equal(pj.CreatedAt) {
			return ids[i] < ids[j]
		}
		return pi.CreatedAt.Before(pj.CreatedAt)
	})
}

// GetReadiness 返回单星就绪度（不冻结，实时计算）。
func (s *Service) GetReadiness(satelliteID string) (*Readiness, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sat := s.state.Satellites[satelliteID]
	if sat == nil {
		return nil, apiErr(CodeNotFound, "卫星 %s 不存在", satelliteID)
	}
	se := s.buildSatelliteEvidenceLocked(satelliteID)
	r := &Readiness{
		SatelliteID: satelliteID, Name: sat.Name, Safe: sat.Safe, SafeReason: sat.SafeReason,
		Health: se.Health, Milestones: map[Milestone]bool{},
		CommandVersions: se.CommandVersions,
	}
	for _, me := range se.Milestones {
		r.Milestones[me.Milestone] = me.Complete
	}
	r.OpenCases = se.OpenCases
	r.Plans = se.Plans
	r.Missing = se.Missing
	r.Ready = len(r.Missing) == 0
	return r, nil
}

// ListGroupReadiness 返回整组每颗星的就绪度。
func (s *Service) ListGroupReadiness() []*Readiness {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.state.Satellites))
	for id := range s.state.Satellites {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*Readiness, 0, len(ids))
	for _, id := range ids {
		se := s.buildSatelliteEvidenceLocked(id)
		sat := s.state.Satellites[id]
		r := &Readiness{SatelliteID: id, Name: sat.Name, Safe: sat.Safe,
			SafeReason: sat.SafeReason, Health: se.Health, Milestones: map[Milestone]bool{},
			OpenCases: se.OpenCases, CommandVersions: se.CommandVersions, Plans: se.Plans}
		for _, me := range se.Milestones {
			r.Milestones[me.Milestone] = me.Complete
		}
		r.Missing = se.Missing
		r.Ready = len(r.Missing) == 0
		out = append(out, r)
	}
	return out
}

// GetDelivery 读取签署记录（含冻结证据）。
func (s *Service) GetDelivery(id string) (*DeliveryRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, d := range s.state.Deliveries {
		if d.ID == id {
			return clone(d), nil
		}
	}
	return nil, apiErr(CodeNotFound, "交付记录 %s 不存在", id)
}

// ListDeliveries 列出全部签署记录。
func (s *Service) ListDeliveries() []*DeliveryRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*DeliveryRecord, len(s.state.Deliveries))
	for i, d := range s.state.Deliveries {
		out[i] = clone(d)
	}
	return out
}
