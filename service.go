package mission

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// APIError 为带错误码的领域错误，HTTP 层据此映射状态码。
type APIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *APIError) Error() string { return e.Message }

// 常用错误码。
const (
	CodeNotFound      = "not_found"
	CodeConflict      = "conflict"
	CodeValidation    = "validation"
	CodePrecondition  = "precondition_failed"
	CodeStateConflict = "state_conflict"
)

func apiErr(code, format string, args ...any) *APIError {
	return &APIError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// IsAPIError 提取 APIError。
func IsAPIError(err error) (*APIError, bool) {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae, true
	}
	return nil, false
}

// Config 为服务配置。
type Config struct {
	Dir            string
	Now            func() time.Time // 注入时钟（测试用）
	SnapshotEvery  int              // 每多少条事件落一次快照，0 表示不自动快照
	DefaultTimeout time.Duration    // 计划默认执行超时
}

// Service 是后端中枢：所有状态变更串行化在一把写锁内，
// 先追加仅追加事件日志（fsync）再归约内存状态。
type Service struct {
	mu         sync.RWMutex
	state      *State
	log        *EventLog
	dir        string
	now        func() time.Time
	snapEvery  int
	defTimeout time.Duration

	snapOffset int64 // 最近一次快照覆盖到的 offset
	sinceSnap  int   // 快照后追加的事件数
}

// OpenService 打开数据目录、恢复快照并重放事件日志。
func OpenService(cfg Config) (*Service, error) {
	if cfg.Dir == "" {
		return nil, errors.New("数据目录不能为空")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	defTimeout := cfg.DefaultTimeout
	if defTimeout <= 0 {
		defTimeout = 60 * time.Second
	}
	lg, err := OpenEventLog(cfg.Dir)
	if err != nil {
		return nil, err
	}
	st := NewState()
	var snapOffset int64
	if off, ok, err := LoadSnapshot(cfg.Dir, st); err != nil {
		return nil, err
	} else if ok {
		snapOffset = off
	}
	// 日志已被截断时，用快照位点播种 offset，保证全局单调。
	lg.SeedNext(snapOffset + 1)
	if err := lg.Replay(snapOffset, func(e Envelope) error {
		if err := st.Apply(e); err != nil {
			return fmt.Errorf("重放事件 %d 失败: %w", e.Offset, err)
		}
		snapOffset = e.Offset
		return nil
	}); err != nil {
		return nil, err
	}
	return &Service{
		state:      st,
		log:        lg,
		dir:        cfg.Dir,
		now:        now,
		snapEvery:  cfg.SnapshotEvery,
		defTimeout: defTimeout,
		snapOffset: snapOffset,
	}, nil
}

// Close 做一次最终快照并关闭日志。
func (s *Service) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.snapshotLocked(); err != nil {
		// 快照失败不阻止关闭：完整日志仍可重放。
		_ = err
	}
	return s.log.Close()
}

// emit 的调用方必须持有 mu 写锁。
func (s *Service) emit(typ string, payload any) (Envelope, error) {
	raw, err := toMap(payload)
	if err != nil {
		return Envelope{}, err
	}
	e := Envelope{Type: typ, OccurredAt: s.now(), Payload: raw}
	if err := s.log.Append(&e); err != nil {
		return Envelope{}, err
	}
	if err := s.state.Apply(e); err != nil {
		return Envelope{}, fmt.Errorf("状态归约失败(offset=%d): %w", e.Offset, err)
	}
	s.sinceSnap++
	if s.snapEvery > 0 && s.sinceSnap >= s.snapEvery {
		if err := s.snapshotLocked(); err != nil {
			// 快照失败不影响已确认的事件：下次重试。
			_ = err
		}
	}
	return e, nil
}

// snapshotLocked 的调用方必须持有写锁。
func (s *Service) snapshotLocked() error {
	off := s.log.NextOffset() - 1
	if off < 0 || off <= s.snapOffset {
		return nil
	}
	if err := SaveSnapshot(s.dir, off, s.state); err != nil {
		return err
	}
	if err := s.log.Truncate(); err != nil {
		return err
	}
	s.snapOffset = off
	s.sinceSnap = 0
	return nil
}

func toMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("序列化事件载荷: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("事件载荷必须是对象: %w", err)
	}
	return m, nil
}

// clone 通过 JSON 往返深拷贝，用于冻结证据与返回只读快照。
func clone[T any](v T) T {
	var out T
	b, _ := json.Marshal(v)
	_ = json.Unmarshal(b, &out)
	return out
}

// ---------- 注册与窗口 ----------

// RegisterSatellite 注册卫星；同名 ID 重复注册且名字一致时幂等。
func (s *Service) RegisterSatellite(id, name string) error {
	if id == "" {
		return apiErr(CodeValidation, "卫星标识不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sat, ok := s.state.Satellites[id]; ok {
		if name != "" && sat.Name != name {
			return apiErr(CodeConflict, "卫星 %s 已注册且名称不一致", id)
		}
		return nil
	}
	_, err := s.emit(EvSatelliteRegistered, SatelliteRegisteredPayload{SatelliteID: id, Name: name})
	return err
}

// RegisterStation 注册地面测控站。
func (s *Service) RegisterStation(id, name string) error {
	if id == "" {
		return apiErr(CodeValidation, "地面站标识不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.state.Stations[id]; ok {
		if name != "" && st.Name != name {
			return apiErr(CodeConflict, "地面站 %s 已注册且名称不一致", id)
		}
		return nil
	}
	_, err := s.emit(EvStationRegistered, StationRegisteredPayload{StationID: id, Name: name})
	return err
}

// ScheduleWindowInput 为排窗输入。
type ScheduleWindowInput struct {
	ID          string    `json:"window_id"`
	SatelliteID string    `json:"satellite_id"`
	StationID   string    `json:"station_id"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	Version     int       `json:"version"`
}

// ScheduleWindow 排定或改期窗口。已取消窗口不能改期，需新建窗口 ID。
func (s *Service) ScheduleWindow(in ScheduleWindowInput) (*Window, error) {
	if in.ID == "" || in.SatelliteID == "" || in.StationID == "" {
		return nil, apiErr(CodeValidation, "窗口、卫星、地面站标识均不能为空")
	}
	if in.Start.IsZero() || in.End.IsZero() || !in.Start.Before(in.End) {
		return nil, apiErr(CodeValidation, "窗口开始时间必须早于结束时间")
	}
	if in.Version < 1 {
		in.Version = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Satellites[in.SatelliteID]; !ok {
		return nil, apiErr(CodeNotFound, "卫星 %s 不存在", in.SatelliteID)
	}
	if _, ok := s.state.Stations[in.StationID]; !ok {
		return nil, apiErr(CodeNotFound, "地面站 %s 不存在", in.StationID)
	}
	if old, ok := s.state.Windows[in.ID]; ok {
		if old.State == WindowCanceled {
			return nil, apiErr(CodeStateConflict, "窗口 %s 已取消，不能改期，请新建窗口", in.ID)
		}
		if in.Version <= old.Version {
			return nil, apiErr(CodeConflict, "窗口 %s 当前为 v%d，改期版本号必须更大", in.ID, old.Version)
		}
	}
	w := Window{ID: in.ID, SatelliteID: in.SatelliteID, StationID: in.StationID,
		Start: in.Start, End: in.End, State: WindowScheduled, Version: in.Version}
	if _, err := s.emit(EvWindowScheduled, WindowScheduledPayload{Window: w}); err != nil {
		return nil, err
	}
	return clone(&w), nil
}

// CancelWindow 取消窗口：原子失效全部相关批准、中止执行中计划并进入可续办处置链。
func (s *Service) CancelWindow(windowID, reason string) (*Case, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.state.Windows[windowID]
	if w == nil {
		return nil, apiErr(CodeNotFound, "窗口 %s 不存在", windowID)
	}
	if w.State == WindowCanceled {
		return nil, apiErr(CodeStateConflict, "窗口 %s 已取消", windowID)
	}
	now := s.now()
	var invalidated []InvalidatedApproval
	var aborted []string
	for _, pid := range s.state.SortedPlanIDs() {
		pl := s.state.Plans[pid]
		if pl.WindowID != windowID {
			continue
		}
		if pl.State == PlanExecuting {
			aborted = append(aborted, pl.ID)
		}
		for _, a := range pl.Approvals {
			if a.Valid {
				invalidated = append(invalidated, InvalidatedApproval{
					PlanID: pl.ID, ApprovalID: a.ID,
					Reason: "窗口 " + windowID + " 已取消: " + reason, At: now,
				})
			}
		}
	}
	c := s.newCaseLocked(CaseWindowCanceled, w.SatelliteID, "",
		fmt.Sprintf("窗口 %s（%s，v%d）取消: %s", windowID, w.StationID, w.Version, reason))
	if len(aborted) > 0 {
		c.Steps = append(c.Steps, CaseStep{Action: "plans_aborted", Operator: "system", At: now,
			Note:    fmt.Sprintf("%d 条执行中计划被中止并释放资源", len(aborted)),
			Payload: map[string]any{"plan_ids": aborted}})
	}
	payload := WindowCanceledPayload{WindowID: windowID, Version: w.Version, At: now,
		Reason: reason, Invalidated: invalidated, AbortedPlanIDs: aborted, Case: c}
	if _, err := s.emit(EvWindowCanceled, payload); err != nil {
		return nil, err
	}
	return clone(c), nil
}

// newCaseLocked 生成处置单（ID 确定性，便于幂等与测试）。
func (s *Service) newCaseLocked(kind CaseKind, satelliteID, planID, summary string) *Case {
	n := 1
	for _, c := range s.state.Cases {
		if c.Kind == kind && c.SatelliteID == satelliteID {
			n++
		}
	}
	id := fmt.Sprintf("case-%s-%s-%d", kind, HashString(satelliteID)[:8], n)
	return &Case{ID: id, Kind: kind, SatelliteID: satelliteID, PlanID: planID,
		Summary: summary, State: CaseOpen, OpenedAt: s.now()}
}

// ---------- 遥测接入 ----------

// FrameInput 为遥测上报输入。
type FrameInput struct {
	SatelliteID    string         `json:"satellite_id"`
	SourceID       string         `json:"source_id"`
	SourceSequence int64          `json:"source_sequence"`
	SpacecraftTime time.Time      `json:"spacecraft_time"`
	ReceivedAt     time.Time      `json:"received_at"`
	Quality        Quality        `json:"quality"`
	Data           map[string]any `json:"data"`
}

// IngestResult 返回接入裁决。
type IngestResult struct {
	FrameKey      string                `json:"frame_key"`
	Hash          string                `json:"hash"`
	Duplicate     bool                  `json:"duplicate"`
	BecameCurrent bool                  `json:"became_current"`
	ConditionSig  string                `json:"condition_sig"`
	Invalidated   []InvalidatedApproval `json:"invalidated"`
}

// IngestFrame 接入一帧遥测。乱序/重复安全：同一 (卫星,源,序号) 视为同一帧，
// 当前帧只按源内序号向前推进；任何依据旧遥测的有效批准随之失效。
func (s *Service) IngestFrame(in FrameInput) (*IngestResult, error) {
	if in.SatelliteID == "" || in.SourceID == "" {
		return nil, apiErr(CodeValidation, "卫星标识与数据源标识不能为空")
	}
	if in.SourceSequence < 0 {
		return nil, apiErr(CodeValidation, "源内序号不能为负")
	}
	if in.SpacecraftTime.IsZero() || in.ReceivedAt.IsZero() {
		return nil, apiErr(CodeValidation, "星上时间与地面接收时间均必填")
	}
	switch in.Quality {
	case QualityVerified, QualityDegraded, QualityInvalid:
	default:
		return nil, apiErr(CodeValidation, "非法质量标记 %q", in.Quality)
	}
	if in.Data == nil {
		in.Data = map[string]any{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Satellites[in.SatelliteID]; !ok {
		return nil, apiErr(CodeNotFound, "卫星 %s 不存在", in.SatelliteID)
	}
	key := FrameKey(in.SatelliteID, in.SourceID, in.SourceSequence)
	hash := StableHash(in.Data)
	if existing, ok := s.state.Frames[key]; ok {
		if existing.Hash != hash {
			return nil, apiErr(CodeConflict,
				"帧 %s 已存在但内容哈希不一致（%s vs %s），疑似源内序号重用", key, existing.Hash, hash)
		}
		return &IngestResult{FrameKey: key, Hash: hash, Duplicate: true,
			BecameCurrent: false, ConditionSig: s.state.Satellites[in.SatelliteID].ConditionSig}, nil
	}
	frame := Frame{SatelliteID: in.SatelliteID, SourceID: in.SourceID,
		SourceSequence: in.SourceSequence, SpacecraftTime: in.SpacecraftTime,
		ReceivedAt: in.ReceivedAt, Quality: in.Quality, Data: in.Data,
		IngestedAt: s.now(), Hash: hash}
	srcs := s.state.CurrentBySource[in.SatelliteID]
	cur := srcs[in.SourceID]
	became := cur == nil || in.SourceSequence > cur.SourceSequence

	// 预判接入后的条件签名与受影响批准（不修改状态，事件归约时再落状态）。
	postSig := s.projectedConditionSig(in, became)
	invalidated := s.invalidationsForTelemetry(in.SatelliteID, postSig,
		"依据的遥测版本已变化：新帧 "+key+" 成为当前版本")

	if _, err := s.emit(EvFrameIngested, FrameIngestedPayload{
		Frame: frame, Duplicate: false, BecameCurrent: became,
		ConditionSig: postSig, Invalidated: invalidated,
	}); err != nil {
		return nil, err
	}
	// ConditionSig 与失效均由事件归约落状态，服务层不直接改写。
	return &IngestResult{FrameKey: key, Hash: hash, BecameCurrent: became,
		ConditionSig: postSig, Invalidated: invalidated}, nil
}

// projectedConditionSig 在不修改状态的前提下，计算本帧接入后卫星的条件签名。
// 签名覆盖该星每个数据源的当前帧（键、哈希、星上时间、质量）——任何一源的当前版本
// 推进都会改变签名，从而使依据旧版本的批准失效。
func (s *Service) projectedConditionSig(in FrameInput, became bool) string {
	type item struct {
		Source  string `json:"source"`
		Key     string `json:"key"`
		Hash    string `json:"hash"`
		SCTime  string `json:"sc_time"`
		Quality string `json:"quality"`
	}
	srcs := s.state.CurrentBySource[in.SatelliteID]
	keys := make([]string, 0, len(srcs)+1)
	bySource := map[string]*Frame{}
	for k, f := range srcs {
		keys = append(keys, k)
		bySource[k] = f
	}
	items := make([]item, 0, len(keys)+1)
	for _, src := range keys {
		f := bySource[src]
		items = append(items, item{src, f.Key(), f.Hash, f.SpacecraftTime.UTC().Format(time.RFC3339Nano), string(f.Quality)})
	}
	if became {
		// 替换或追加本源。
		idx := -1
		for i := range items {
			if items[i].Source == in.SourceID {
				idx = i
			}
		}
		ni := item{in.SourceID, FrameKey(in.SatelliteID, in.SourceID, in.SourceSequence),
			StableHash(in.Data), in.SpacecraftTime.UTC().Format(time.RFC3339Nano), string(in.Quality)}
		if idx >= 0 {
			items[idx] = ni
		} else {
			items = append(items, ni)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Source < items[j].Source })
	return StableHash(items)
}

// invalidationsForTelemetry 找出因遥测条件变化而应失效的有效批准。
// 受影响计划：主星为该星，或前置条件显式引用该星（如交叉标定引用伴星）。
func (s *Service) invalidationsForTelemetry(satelliteID, postSig, reason string) []InvalidatedApproval {
	now := s.now()
	var out []InvalidatedApproval
	for _, pid := range s.state.SortedPlanIDs() {
		pl := s.state.Plans[pid]
		if !planTouchesSatellite(pl, satelliteID) {
			continue
		}
		for _, a := range pl.Approvals {
			if !a.Valid {
				continue
			}
			if a.Basis.ConditionSigFor(satelliteID, pl.SatelliteID) != "" &&
				a.Basis.ConditionSigFor(satelliteID, pl.SatelliteID) == postSig {
				continue
			}
			out = append(out, InvalidatedApproval{PlanID: pl.ID, ApprovalID: a.ID, Reason: reason, At: now})
		}
	}
	return out
}

// planTouchesSatellite 判断计划是否以某星为主星或在门禁中引用该星。
func planTouchesSatellite(pl *Plan, satelliteID string) bool {
	if pl.SatelliteID == satelliteID || pl.PartnerSatelliteID == satelliteID {
		return true
	}
	for _, g := range pl.Gates {
		if id, _ := g.Param["satellite_id"].(string); id == satelliteID {
			return true
		}
	}
	return false
}
