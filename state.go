package mission

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// State 是事件重放得到的完整聚合状态，也是快照内容。
type State struct {
	Satellites      map[string]*Satellite        `json:"satellites"`
	Stations        map[string]*Station          `json:"stations"`
	Windows         map[string]*Window           `json:"windows"`
	Frames          map[string]*Frame            `json:"frames"`
	CurrentBySource map[string]map[string]*Frame `json:"current_by_source"` // 卫星 -> 数据源 -> 当前帧
	Commands        map[string]*CommandVersion   `json:"commands"`
	Plans           map[string]*Plan             `json:"plans"`
	Cases           map[string]*Case             `json:"cases"`
	Deliveries      []*DeliveryRecord            `json:"deliveries"`
	ActiveLocks     map[string]string            `json:"active_locks"` // 资源 -> 持有执行权的计划
}

// NewState 返回空状态。
func NewState() *State {
	return &State{
		Satellites:      map[string]*Satellite{},
		Stations:        map[string]*Station{},
		Windows:         map[string]*Window{},
		Frames:          map[string]*Frame{},
		CurrentBySource: map[string]map[string]*Frame{},
		Commands:        map[string]*CommandVersion{},
		Plans:           map[string]*Plan{},
		Cases:           map[string]*Case{},
		ActiveLocks:     map[string]string{},
	}
}

// Apply 将一条事件归约到状态上。所有状态变更只能经由本函数。
func (s *State) Apply(e Envelope) error {
	var err error
	switch e.Type {
	case EvSatelliteRegistered:
		var p SatelliteRegisteredPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		s.Satellites[p.SatelliteID] = &Satellite{ID: p.SatelliteID, Name: p.Name, Milestones: map[Milestone]*MilestoneRecord{}}

	case EvStationRegistered:
		var p StationRegisteredPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		s.Stations[p.StationID] = &Station{ID: p.StationID, Name: p.Name}

	case EvWindowScheduled:
		var p WindowScheduledPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		w := p.Window
		if existing, ok := s.Windows[w.ID]; ok && w.Version <= existing.Version {
			// 乱序/重放旧版本窗口事件：忽略，新版本优先。
			return nil
		}
		s.Windows[w.ID] = &w

	case EvWindowCanceled:
		var p WindowCanceledPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		w := s.Windows[p.WindowID]
		if w == nil || w.State == WindowCanceled {
			break
		}
		at := p.At
		w.State = WindowCanceled
		w.CanceledAt = &at
		w.CancelReason = p.Reason
		for _, inv := range p.Invalidated {
			invalidateApproval(s.Plans[inv.PlanID], inv.ApprovalID, inv.Reason, inv.At)
		}
		for _, pid := range p.AbortedPlanIDs {
			s.abortExecutingPlan(pid, at)
		}
		if p.Case != nil {
			c := *p.Case
			s.Cases[c.ID] = &c
		}

	case EvFrameIngested:
		var p FrameIngestedPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		f := p.Frame
		if existing, ok := s.Frames[f.Key()]; ok {
			// 去重：同键帧只保留首次落库的裁决（became_current 不允许反复翻转）。
			_ = existing
			break
		}
		f.BecameCurrent = p.BecameCurrent
		s.Frames[f.Key()] = &f
		if p.BecameCurrent {
			srcs := s.CurrentBySource[f.SatelliteID]
			if srcs == nil {
				srcs = map[string]*Frame{}
				s.CurrentBySource[f.SatelliteID] = srcs
			}
			srcs[f.SourceID] = &f
		}
		if sat := s.Satellites[f.SatelliteID]; sat != nil {
			sat.ConditionSig = p.ConditionSig
		}
		for _, inv := range p.Invalidated {
			invalidateApproval(s.Plans[inv.PlanID], inv.ApprovalID, inv.Reason, inv.At)
		}

	case EvHealthConfirmed:
		var p HealthConfirmedPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		sat := s.Satellites[p.Confirmation.SatelliteID]
		if sat != nil {
			c := p.Confirmation
			sat.Health = &c
			if sat.Milestones == nil {
				sat.Milestones = map[Milestone]*MilestoneRecord{}
			}
			// 健康确认即“健康确认”里程碑的完成证据，只增不减、乱序数据不能倒退。
			if _, ok := sat.Milestones[MilestoneHealth]; !ok {
				sat.Milestones[MilestoneHealth] = &MilestoneRecord{
					Milestone: MilestoneHealth, At: c.At,
					Detail: "健康确认人 " + c.Operator + "，依据遥测 " + c.FrameKey}
			}
		}

	case EvCommandVersionCreated:
		var p CommandVersionCreatedPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		v := p.Version
		s.Commands[v.ID] = &v

	case EvPlanCreated:
		var p PlanCreatedPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		pl := p.Plan
		s.Plans[pl.ID] = &pl

	case EvPlanApproved:
		var p PlanApprovedPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		pl := s.Plans[p.Approval.PlanID]
		if pl == nil {
			return fmt.Errorf("事件 %d: 计划 %s 不存在", e.Offset, p.Approval.PlanID)
		}
		// 同角色重放时幂等。
		for _, a := range pl.Approvals {
			if a.ID == p.Approval.ID {
				return nil
			}
		}
		a := p.Approval
		pl.Approvals = append(pl.Approvals, &a)

	case EvApprovalInvalidated:
		var p ApprovalInvalidatedPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		invalidateApproval(s.Plans[p.PlanID], p.ApprovalID, p.Reason, p.At)

	case EvPlanExecutionStarted:
		var p PlanExecutionStartedPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		pl := s.Plans[p.PlanID]
		if pl == nil {
			return fmt.Errorf("事件 %d: 计划 %s 不存在", e.Offset, p.PlanID)
		}
		// 重放时已含同一尝试则幂等。
		if len(pl.Attempts) < p.Attempt {
			at := p.Started
			pl.Attempts = append(pl.Attempts, &at)
		}
		pl.State = PlanExecuting
		pl.ActiveResources = p.Resources
		for _, r := range p.Resources {
			s.ActiveLocks[r] = pl.ID
		}

	case EvPlanExecutionResolved:
		var p PlanExecutionResolvedPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		pl := s.Plans[p.PlanID]
		if pl == nil {
			return fmt.Errorf("事件 %d: 计划 %s 不存在", e.Offset, p.PlanID)
		}
		r := p.Result
		pl.Result = &r
		pl.State = r.Outcome
		if idx := r.Attempt - 1; idx >= 0 && idx < len(pl.Attempts) {
			a := pl.Attempts[idx]
			if p.AcknowledgedAt != nil {
				t := *p.AcknowledgedAt
				a.AcknowledgedAt = &t
			}
			if p.ReceiptAt != nil {
				t := *p.ReceiptAt
				a.ReceiptAt = &t
			}
			if p.ReceiptSeq != nil {
				v := *p.ReceiptSeq
				a.ReceiptSeq = &v
			}
			if p.ReceiptStatus != "" {
				a.ReceiptStatus = p.ReceiptStatus
			}
		}
		for _, res := range pl.ActiveResources {
			if holder, ok := s.ActiveLocks[res]; ok && holder == pl.ID {
				delete(s.ActiveLocks, res)
			}
		}
		pl.ActiveResources = nil
		if p.Milestone != nil {
			if sat := s.Satellites[pl.SatelliteID]; sat != nil {
				if sat.Milestones == nil {
					sat.Milestones = map[Milestone]*MilestoneRecord{}
				}
				mr := *p.Milestone
				sat.Milestones[mr.Milestone] = &mr
			}
		}
		if p.Case != nil {
			c := *p.Case
			s.Cases[c.ID] = &c
		}

	case EvSafeModeEntered:
		var p SafeModeEnteredPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		sat := s.Satellites[p.SatelliteID]
		if sat == nil {
			return fmt.Errorf("事件 %d: 卫星 %s 不存在", e.Offset, p.SatelliteID)
		}
		if sat.Safe && sat.SafeGeneration == p.Generation {
			break // 幂等：同一代安全模式事件重复到达
		}
		sat.Safe = true
		sat.SafeReason = p.Reason
		sat.SafeGeneration = p.Generation
		for _, inv := range p.Invalidated {
			invalidateApproval(s.Plans[inv.PlanID], inv.ApprovalID, inv.Reason, inv.At)
		}
		for _, pid := range p.AbortedPlanIDs {
			s.abortExecutingPlan(pid, p.At)
		}
		if p.Case != nil {
			c := *p.Case
			s.Cases[c.ID] = &c
		}

	case EvSafeModeCleared:
		var p SafeModeClearedPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		sat := s.Satellites[p.SatelliteID]
		if sat != nil && sat.SafeGeneration == p.Generation {
			sat.Safe = false
			sat.SafeReason = ""
		}

	case EvCaseOpened:
		var p CaseOpenedPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		c := p.Case
		s.Cases[c.ID] = &c

	case EvCaseAdvanced:
		var p CaseAdvancedPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		c := s.Cases[p.CaseID]
		if c == nil {
			return fmt.Errorf("事件 %d: 处置单 %s 不存在", e.Offset, p.CaseID)
		}
		c.Steps = append(c.Steps, p.Step)

	case EvCaseResolved:
		var p CaseResolvedPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		c := s.Cases[p.CaseID]
		if c == nil {
			return fmt.Errorf("事件 %d: 处置单 %s 不存在", e.Offset, p.CaseID)
		}
		c.State = CaseResolved
		at := p.At
		c.ResolvedAt = &at

	case EvCaseLinked:
		var p CaseLinkedPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		if pre := s.Cases[p.PredecessorID]; pre != nil {
			pre.SuccessorID = p.SuccessorID
		}
		if suc := s.Cases[p.SuccessorID]; suc != nil {
			suc.PredecessorID = p.PredecessorID
		}

	case EvDeliverySigned:
		var p DeliverySignedPayload
		if err = decode(e.Payload, &p); err != nil {
			return err
		}
		r := p.Record
		s.Deliveries = append(s.Deliveries, &r)

	default:
		return fmt.Errorf("未知事件类型 %q", e.Type)
	}
	return nil
}

func invalidateApproval(pl *Plan, approvalID, reason string, at time.Time) {
	if pl == nil {
		return
	}
	for _, a := range pl.Approvals {
		if a.ID == approvalID && a.Valid {
			a.Valid = false
			a.InvalidatedReason = reason
			t := at
			a.InvalidatedAt = &t
		}
	}
}

// abortExecutingPlan 将执行中的计划置为 aborted 并释放其资源锁。
func (s *State) abortExecutingPlan(planID string, at time.Time) {
	pl := s.Plans[planID]
	if pl == nil || pl.State != PlanExecuting {
		return
	}
	pl.State = PlanAborted
	if pl.Result == nil {
		pl.Result = &ExecutionResult{Outcome: PlanAborted, At: at, Attempt: len(pl.Attempts), Detail: "执行被外部事件中止"}
	}
	for _, res := range pl.ActiveResources {
		if holder, ok := s.ActiveLocks[res]; ok && holder == planID {
			delete(s.ActiveLocks, res)
		}
	}
	pl.ActiveResources = nil
}

func decode(raw map[string]any, dst any) error {
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return fmt.Errorf("解析事件载荷: %w", err)
	}
	return nil
}

// SortedPlanIDs 返回按创建时间排序的计划 ID（稳定输出）。
func (s *State) SortedPlanIDs() []string {
	ids := make([]string, 0, len(s.Plans))
	for id := range s.Plans {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if s.Plans[ids[i]].CreatedAt.Equal(s.Plans[ids[j]].CreatedAt) {
			return ids[i] < ids[j]
		}
		return s.Plans[ids[i]].CreatedAt.Before(s.Plans[ids[j]].CreatedAt)
	})
	return ids
}

// SortedCaseIDs 返回按开立时间排序的处置单 ID。
func (s *State) SortedCaseIDs() []string {
	ids := make([]string, 0, len(s.Cases))
	for id := range s.Cases {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if s.Cases[ids[i]].OpenedAt.Equal(s.Cases[ids[j]].OpenedAt) {
			return ids[i] < ids[j]
		}
		return s.Cases[ids[i]].OpenedAt.Before(s.Cases[ids[j]].OpenedAt)
	})
	return ids
}
