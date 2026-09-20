package mission

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Clock 为时间来源；测试与演练可替换。
type Clock func() time.Time

// Config 中枢配置。
type Config struct {
	LeaseTTL          time.Duration
	DefaultThresholds map[ConditionKey]float64
}

// Service 是交付中枢的应用服务：所有命令经互斥锁串行化，
// 先落事件日志再更新内存投影；查询只读内存。
type Service struct {
	store *Store
	mu    sync.Mutex
	clock Clock
	cfg   Config

	sats       map[string]*satState
	windows    map[string]*windowState
	plans      map[string]*planState
	issues     map[string]*issueState
	deliveries map[string]*DeliveryView

	satLease     map[string]*leaseEntry // 按卫星占用
	stationLease map[string]*leaseEntry // 按地面站占用

	seq map[string]int // 各前缀对象计数，回放时随创建事件重建
}

type satState struct {
	id           string
	name         string
	required     []CommandType
	thresholds   map[ConditionKey]float64
	registeredAt time.Time

	latestSeq  map[string]int64         // 每个数据源已采用的最大源内序号
	frames     []TelemetryFrame         // 全部帧（含乱序/坏帧，留痕）
	frameBySrc map[string]map[int64]int // (source -> seq -> frames 下标)，去重用
	cond       map[ConditionKey]*condState
	safe       bool
	safeEpoch  int
	safeScTime time.Time
}

type condState struct {
	has             bool
	everEstablished bool
	value           float64
	ok              bool
	scTime          time.Time
	ref             *FrameRef
	epoch           int
}

type windowState struct {
	id, satID, stationID string
	start, end           time.Time
	status               WindowStatus
	cancelReason         string
}

type planState struct {
	id, satID, windowID, stationID string
	createdBy                      string
	commandType                    CommandType
	digest, payload                string
	revision                       int
	status                         PlanStatus
	createdAt                      time.Time
	approvals                      []*ApprovalView
	lease                          *Lease
	uplinkedAt                     *time.Time
	receipt                        *Receipt
	issueIDs                       []string
	openIssueID                    string
}

type issueState struct {
	view IssueView
}

type leaseEntry struct {
	lease  *Lease
	planID string
}

// NewService 打开事件日志并回放，返回可立即服务的中枢。
func NewService(store *Store, clock Clock, cfg Config) (*Service, error) {
	if clock == nil {
		clock = time.Now
	}
	if cfg.LeaseTTL <= 0 {
		cfg.LeaseTTL = 3 * time.Minute
	}
	if cfg.DefaultThresholds == nil {
		cfg.DefaultThresholds = map[ConditionKey]float64{
			CondAttitude: 80,
			CondPower:    60,
		}
	}
	s := &Service{
		store: store, clock: clock, cfg: cfg,
		sats:         map[string]*satState{},
		windows:      map[string]*windowState{},
		plans:        map[string]*planState{},
		issues:       map[string]*issueState{},
		deliveries:   map[string]*DeliveryView{},
		satLease:     map[string]*leaseEntry{},
		stationLease: map[string]*leaseEntry{},
		seq:          map[string]int{},
	}
	evs, err := store.History()
	if err != nil {
		return nil, err
	}
	for _, ev := range evs {
		s.apply(ev)
	}
	return s, nil
}

// allocID 分配对象标识；计数在回放创建事件时一并累加，重启后不重复。
func (s *Service) allocID(prefix string) string {
	s.seq[prefix]++
	return prefix + "-" + strconv.Itoa(s.seq[prefix])
}

func (s *Service) parseSeqNum(id string) int {
	parts := strings.SplitN(id, "-", 2)
	if len(parts) != 2 {
		return 0
	}
	n, _ := strconv.Atoi(parts[1])
	return n
}

// bumpSeq 在回放创建事件时重建计数器。
func (s *Service) bumpSeq(prefix, id string) {
	if n := s.parseSeqNum(id); n > s.seq[prefix] {
		s.seq[prefix] = n
	}
}

// ---- 事件投影 ----

func (s *Service) apply(ev Event) {
	var err error
	switch ev.Type {
	case evSatelliteRegistered:
		var d evSatelliteRegisteredData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			s.applySatelliteRegistered(d)
		}
	case evTelemetryIngested:
		var d evTelemetryIngestedData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			s.applyTelemetry(d.Frame)
		}
	case evWindowScheduled:
		var d evWindowScheduledData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			s.windows[d.WindowID] = &windowState{
				id: d.WindowID, satID: d.SatelliteID, stationID: d.StationID,
				start: d.Start, end: d.End, status: WinScheduled,
			}
		}
	case evWindowCancelled:
		var d evWindowCancelledData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			if w := s.windows[d.WindowID]; w != nil {
				w.status = WinCancelled
				w.cancelReason = d.Reason
			}
		}
	case evWindowClosed:
		var d evWindowClosedData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			if w := s.windows[d.WindowID]; w != nil && w.status == WinScheduled {
				w.status = WinClosed
			}
		}
	case evPlanCreated:
		var d evPlanCreatedData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			s.bumpSeq("PL", d.PlanID)
			st := s.sats[d.SatelliteID]
			station := ""
			if w := s.windows[d.WindowID]; w != nil {
				station = w.stationID
			}
			_ = st
			s.plans[d.PlanID] = &planState{
				id: d.PlanID, satID: d.SatelliteID, windowID: d.WindowID,
				stationID: station, commandType: d.CommandType,
				digest: d.PayloadDigest, payload: d.Payload,
				createdBy: d.CreatedBy, createdAt: d.At,
				revision: 1, status: PlanDraft,
			}
		}
	case evPlanSubmitted:
		var d evPlanSubmittedData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			if p := s.plans[d.PlanID]; p != nil {
				p.status = PlanSubmitted
				p.revision = d.Revision
			}
		}
	case evPlanRevised:
		var d evPlanRevisedData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			if p := s.plans[d.PlanID]; p != nil {
				p.revision = d.Revision
				p.digest = d.PayloadDigest
				p.payload = d.Payload
				p.status = PlanSubmitted
			}
		}
	case evPlanRejected:
		var d evPlanRejectedData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			if p := s.plans[d.PlanID]; p != nil {
				p.status = PlanRejected
			}
		}
	case evApprovalAdded:
		var d evApprovalAddedData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			if p := s.plans[d.PlanID]; p != nil {
				a := &ApprovalView{
					ApprovalID: d.ApprovalID, PlanRevision: d.PlanRevision,
					Role: d.Role, Approver: d.Approver, At: d.At,
					WindowID: d.WindowID, EpochSnapshot: d.EpochSnapshot,
					TelemetryBasis: ptrRef(d.TelemetryBasis),
				}
				p.approvals = append(p.approvals, a)
				s.bumpSeq("AP", d.ApprovalID)
				if s.activeApprovals(p) == 2 {
					p.status = PlanApproved
				}
			}
		}
	case evApprovalsInvalidated:
		var d evApprovalsInvalidatedData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			if p := s.plans[d.PlanID]; p != nil {
				ids := map[string]bool{}
				for _, id := range d.ApprovalIDs {
					ids[id] = true
				}
				for _, a := range p.approvals {
					if !a.Invalidated && (len(ids) == 0 || ids[a.ApprovalID]) {
						now := d.At
						a.Invalidated = true
						a.InvalidReason = d.Reason
						a.InvalidAt = &now
					}
				}
				if p.status == PlanApproved && s.activeApprovals(p) < 2 {
					p.status = PlanSubmitted
				}
			}
		}
	case evPlanUplinked:
		var d evPlanUplinkedData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			if p := s.plans[d.PlanID]; p != nil {
				l := d.Lease
				s.bumpSeq("LS", l.LeaseID)
				p.status = PlanExecuting
				p.lease = &l
				p.uplinkedAt = &d.At
				s.satLease[l.SatelliteID] = &leaseEntry{lease: &l, planID: p.id}
				s.stationLease[l.StationID] = &leaseEntry{lease: &l, planID: p.id}
			}
		}
	case evLeaseRevoked, evLeaseExpired:
		var planID, issueID string
		var at time.Time
		if ev.Type == evLeaseRevoked {
			var d evLeaseRevokedData
			err = json.Unmarshal(ev.Data, &d)
			planID, issueID, at = d.PlanID, d.IssueID, d.At
		} else {
			var d evLeaseExpiredData
			err = json.Unmarshal(ev.Data, &d)
			planID, issueID, at = d.PlanID, d.IssueID, d.At
		}
		if err == nil {
			if p := s.plans[planID]; p != nil && p.lease != nil {
				p.lease.Released = true
				delete(s.satLease, p.satID)
				delete(s.stationLease, p.lease.StationID)
				if p.status == PlanExecuting {
					p.status = PlanAwaiting
				}
				p.openIssueID = issueID
			}
			_ = at
		}
	case evReceiptRecorded:
		var d evReceiptRecordedData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			if p := s.plans[d.PlanID]; p != nil {
				r := d.Receipt
				p.receipt = &r
				if p.lease != nil {
					p.lease.Released = true
					delete(s.satLease, p.satID)
					delete(s.stationLease, p.lease.StationID)
				}
				if r.Success {
					p.status = PlanExecuted
				} else {
					p.status = PlanFailed
				}
			}
		}
	case evPlanReassigned:
		var d evPlanReassignedData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			if p := s.plans[d.PlanID]; p != nil {
				if w := s.windows[d.WindowID]; w != nil {
					p.windowID = w.id
					p.stationID = w.stationID
				}
				p.status = d.RestoreTo
			}
		}
	case evIssueOpened:
		var d evIssueOpenedData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			v := d.Issue
			s.bumpSeq("IS", v.IssueID)
			s.issues[v.IssueID] = &issueState{view: v}
			if p := s.plans[v.PlanID]; p != nil {
				p.issueIDs = append(p.issueIDs, v.IssueID)
				if v.Status == IssueOpen || v.Status == IssueInProgress {
					p.openIssueID = v.IssueID
				}
			}
		}
	case evIssueWorked:
		var d evIssueWorkedData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			if i := s.issues[d.IssueID]; i != nil {
				i.view.Steps = append(i.view.Steps, d.Step)
				i.view.Status = IssueInProgress
			}
		}
	case evIssueResolved:
		var d evIssueResolvedData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			if i := s.issues[d.IssueID]; i != nil {
				i.view.Status = IssueResolved
				i.view.Resolution = d.Resolution
				i.view.ResolvedAt = &d.At
				i.view.ResolvedBy = d.By
				if p := s.plans[i.view.PlanID]; p != nil && p.openIssueID == d.IssueID {
					p.openIssueID = ""
				}
			}
		}
	case evIssueFailed:
		var d evIssueFailedData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			if i := s.issues[d.IssueID]; i != nil {
				i.view.Status = IssueFailed
				i.view.Resolution = d.Resolution
				i.view.ResolvedAt = &d.At
				i.view.ResolvedBy = d.By
				if p := s.plans[i.view.PlanID]; p != nil && p.openIssueID == d.IssueID {
					p.openIssueID = ""
				}
			}
		}
	case evDeliverySigned:
		var d evDeliverySignedData
		err = json.Unmarshal(ev.Data, &d)
		if err == nil {
			v := d.Delivery
			s.bumpSeq("DL", v.DeliveryID)
			s.deliveries[v.DeliveryID] = &v
		}
	}
	if err != nil {
		// 回放阶段事件均由本服务写入；出现损坏应在 OpenStore 阶段之外显式暴露。
		panic("事件投影失败: " + err.Error())
	}
}

func ptrRef(r FrameRef) *FrameRef {
	return &r
}

// activeApprovals 统计当前版本上未失效的批准数。
func (s *Service) activeApprovals(p *planState) int {
	n := 0
	for _, a := range p.approvals {
		if !a.Invalidated && a.PlanRevision == p.revision {
			n++
		}
	}
	return n
}

// epochSnapshot 取卫星前置条件纪元快照（含安全模式）。
func (s *Service) epochSnapshot(st *satState) map[string]int {
	m := map[string]int{
		string(CondSafeMode): st.safeEpoch,
	}
	for _, k := range []ConditionKey{CondAttitude, CondPower} {
		if c := st.cond[k]; c != nil {
			m[string(k)] = c.epoch
		} else {
			m[string(k)] = 0
		}
	}
	return m
}

// sortedPlans 返回按创建时间排序的计划（视图用）。
func sortedPlanIDs(m map[string]*planState) []string {
	ids := make([]string, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return m[ids[i]].createdAt.Before(m[ids[j]].createdAt)
	})
	return ids
}
