// Package mission 实现星座在轨交付协同的后端中枢领域模型与规则。
package mission

import "time"

// Quality 为遥测帧质量标记。
type Quality string

const (
	QualityVerified Quality = "verified"
	QualityDegraded Quality = "degraded"
	QualityInvalid  Quality = "invalid"
)

// Milestone 是单星交付必须完成的里程碑。
type Milestone string

const (
	MilestoneHealth   Milestone = "health"     // 健康确认
	MilestonePayload  Milestone = "payload_on" // 载荷开机
	MilestoneCrossCal Milestone = "cross_cal"  // 交叉标定
)

// RequiredMilestones 按交付顺序列出全部必需里程碑。
var RequiredMilestones = []Milestone{MilestoneHealth, MilestonePayload, MilestoneCrossCal}

// MilestoneName 为对外展示的中文名。
func MilestoneName(m Milestone) string {
	switch m {
	case MilestoneHealth:
		return "健康确认"
	case MilestonePayload:
		return "载荷开机"
	case MilestoneCrossCal:
		return "交叉标定"
	}
	return string(m)
}

// Frame 是一帧遥测：同时携带星上时间、地面接收时间、源内序号与质量标记。
type Frame struct {
	SatelliteID    string         `json:"satellite_id"`
	SourceID       string         `json:"source_id"`
	SourceSequence int64          `json:"source_sequence"`
	SpacecraftTime time.Time      `json:"spacecraft_time"`
	ReceivedAt     time.Time      `json:"received_at"`
	Quality        Quality        `json:"quality"`
	Data           map[string]any `json:"data"`
	IngestedAt     time.Time      `json:"ingested_at"`
	Hash           string         `json:"hash"`
	BecameCurrent  bool           `json:"became_current"`
}

// Key 返回帧在 (卫星, 数据源, 源内序号) 维度上的唯一键。
func (f Frame) Key() string { return FrameKey(f.SatelliteID, f.SourceID, f.SourceSequence) }

// FrameKey 构造帧全局唯一键。
func FrameKey(satelliteID, sourceID string, seq int64) string {
	return satelliteID + "|" + sourceID + "|" + itoa(seq)
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// HealthConfirmation 为值班主管对单星健康状态的确认。
type HealthConfirmation struct {
	SatelliteID string    `json:"satellite_id"`
	Operator    string    `json:"operator"`
	Note        string    `json:"note"`
	FrameKey    string    `json:"frame_key"`
	FrameHash   string    `json:"frame_hash"`
	At          time.Time `json:"at"`
}

// MilestoneRecord 记录一个里程碑的完成证据。
type MilestoneRecord struct {
	Milestone Milestone `json:"milestone"`
	PlanID    string    `json:"plan_id"`
	At        time.Time `json:"at"`
	Detail    string    `json:"detail"`
}

// Satellite 为单星聚合状态。
type Satellite struct {
	ID             string                         `json:"id"`
	Name           string                         `json:"name"`
	Safe           bool                           `json:"safe"`
	SafeReason     string                         `json:"safe_reason"`
	SafeGeneration int                            `json:"safe_generation"` // 每次进入安全模式递增，批准据此失效
	Health         *HealthConfirmation            `json:"health,omitempty"`
	Milestones     map[Milestone]*MilestoneRecord `json:"milestones"`
	ConditionSig   string                         `json:"condition_sig"`
}

// Station 为地面测控站。
type Station struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// WindowState 为测控窗口状态。
type WindowState string

const (
	WindowScheduled WindowState = "scheduled"
	WindowCanceled  WindowState = "canceled"
)

// Window 为一段测控窗口。
type Window struct {
	ID           string      `json:"id"`
	SatelliteID  string      `json:"satellite_id"`
	StationID    string      `json:"station_id"`
	Start        time.Time   `json:"start"`
	End          time.Time   `json:"end"`
	State        WindowState `json:"state"`
	Version      int         `json:"version"` // 窗口排期版本，取消/改期即换版
	CanceledAt   *time.Time  `json:"canceled_at,omitempty"`
	CancelReason string      `json:"cancel_reason,omitempty"`
}

// Overlaps 判断两个同站窗口时间是否重叠（端点相接不算重叠）。
func (w *Window) Overlaps(other *Window) bool {
	return w.Start.Before(other.End) && other.Start.Before(w.End)
}

// CommandVersion 为某条指令的不可变版本。
type CommandVersion struct {
	ID          string         `json:"id"`
	SatelliteID string         `json:"satellite_id"`
	Command     string         `json:"command"`
	Revision    int            `json:"revision"`
	Content     map[string]any `json:"content"`
	Hash        string         `json:"hash"`
	CreatedBy   string         `json:"created_by"`
	CreatedAt   time.Time      `json:"created_at"`
}

// PlanState 为计划生命周期状态。
type PlanState string

const (
	PlanAwaitingApproval PlanState = "awaiting_approval"
	PlanReady            PlanState = "ready" // 双人复核通过、待执行
	PlanExecuting        PlanState = "executing"
	PlanSucceeded        PlanState = "succeeded"
	PlanFailed           PlanState = "failed"
	PlanAborted          PlanState = "aborted"
	PlanTimedOut         PlanState = "timed_out"
)

// GateType 为前置条件类型。
type GateType string

const (
	GateWindowOpen       GateType = "window_open"       // 处于窗口时间内且窗口未取消
	GateSatelliteNominal GateType = "satellite_nominal" // 卫星未处于安全模式
	GateAttitude         GateType = "attitude"          // P["mode"] == 遥测 attitude_mode
	GateBattery          GateType = "battery"           // 遥测 battery_soc >= P["min"]
	GateTelemetryFresh   GateType = "telemetry_fresh"   // now - 星上时间 <= P["max_age_seconds"]
	GatePayload          GateType = "payload"           // 遥测 payload_on == P["on"]
	GateData             GateType = "data"              // P["path"] 点分路径，P["op"] 比较 P["value"]
)

// Gate 是一项前置条件。
type Gate struct {
	Type  GateType       `json:"type"`
	Param map[string]any `json:"param,omitempty"`
}

// GateResult 为一次前置条件求值结果。
type GateResult struct {
	Type     GateType `json:"type"`
	Passed   bool     `json:"passed"`
	Expected any      `json:"expected,omitempty"`
	Actual   any      `json:"actual,omitempty"`
	Detail   string   `json:"detail,omitempty"`
}

// ApprovalRole 为双人复核席位角色。
type ApprovalRole string

const (
	RoleProposer ApprovalRole = "proposer" // 提议/一审
	RoleVerifier ApprovalRole = "verifier" // 复核/二审
)

// Basis 是批准所依据的证据指纹；任何一项变化都会使批准失效。
type Basis struct {
	FrameKey         string            `json:"frame_key"`
	FrameHash        string            `json:"frame_hash"`
	ConditionSig     string            `json:"condition_sig"`  // 主星星上条件签名（兼容保留）
	ConditionSigs    map[string]string `json:"condition_sigs"` // 涉及的每颗星在批准时的条件签名
	WindowID         string            `json:"window_id"`
	WindowVersion    int               `json:"window_version"`
	WindowState      WindowState       `json:"window_state"`
	SafeGeneration   int               `json:"safe_generation"`  // 主星安全模式代数（兼容保留）
	SafeGenerations  map[string]int    `json:"safe_generations"` // 涉及的每颗星在批准时的安全模式代数
	CommandVersionID string            `json:"command_version_id"`
	CommandHash      string            `json:"command_hash"`
	Gates            []GateResult      `json:"gates"`
}

// BasisSatellites 返回计划依据覆盖的卫星（主星 + 伴星）。
func (p *Plan) BasisSatellites() []string {
	if p.PartnerSatelliteID == "" {
		return []string{p.SatelliteID}
	}
	return []string{p.SatelliteID, p.PartnerSatelliteID}
}

// ConditionSigFor 取依据中某颗星的条件签名（兼容旧记录）。
func (b *Basis) ConditionSigFor(satelliteID, primaryID string) string {
	if b.ConditionSigs != nil {
		return b.ConditionSigs[satelliteID]
	}
	if satelliteID == primaryID {
		return b.ConditionSig
	}
	return ""
}

// SafeGenerationFor 取依据中某颗星的安全模式代数（兼容旧记录）。
func (b *Basis) SafeGenerationFor(satelliteID, primaryID string) int {
	if b.SafeGenerations != nil {
		return b.SafeGenerations[satelliteID]
	}
	if satelliteID == primaryID {
		return b.SafeGeneration
	}
	return 0
}

// Approval 为一次双人复核签名。
type Approval struct {
	ID                string       `json:"id"`
	PlanID            string       `json:"plan_id"`
	Role              ApprovalRole `json:"role"`
	Operator          string       `json:"operator"`
	Note              string       `json:"note"`
	At                time.Time    `json:"at"`
	Basis             Basis        `json:"basis"`
	Valid             bool         `json:"valid"`
	InvalidatedReason string       `json:"invalidated_reason,omitempty"`
	InvalidatedAt     *time.Time   `json:"invalidated_at,omitempty"`
}

// ExecutionAttempt 为一次上行执行尝试与其星上回执。
type ExecutionAttempt struct {
	StartedAt      time.Time  `json:"started_at"`
	Deadline       time.Time  `json:"deadline"`
	Operator       string     `json:"operator"`
	AcknowledgedAt *time.Time `json:"acknowledged_at,omitempty"`
	ReceiptStatus  string     `json:"receipt_status,omitempty"`
	ReceiptAt      *time.Time `json:"receipt_at,omitempty"`
	ReceiptSeq     *int64     `json:"receipt_seq,omitempty"`
	Detail         string     `json:"detail,omitempty"`
}

// ExecutionResult 为计划最终执行结果。
type ExecutionResult struct {
	Outcome PlanState `json:"outcome"` // succeeded / failed / aborted / timed_out
	At      time.Time `json:"at"`
	Attempt int       `json:"attempt"`
	Receipt string    `json:"receipt,omitempty"`
	Detail  string    `json:"detail,omitempty"`
}

// Plan 为一条经复核后上行的指令计划。
type Plan struct {
	ID                 string              `json:"id"`
	SatelliteID        string              `json:"satellite_id"`
	PartnerSatelliteID string              `json:"partner_satellite_id,omitempty"`
	WindowID           string              `json:"window_id"`
	WindowVersion      int                 `json:"window_version"` // 创建时绑定的窗口版本，换版后旧计划门禁失败
	StationID          string              `json:"station_id"`
	CommandVersionID   string              `json:"command_version_id"`
	Kind               string              `json:"kind"`
	Milestone          Milestone           `json:"milestone,omitempty"`
	Gates              []Gate              `json:"gates"`
	Timeout            time.Duration       `json:"timeout"`
	CreatedBy          string              `json:"created_by"`
	CreatedAt          time.Time           `json:"created_at"`
	State              PlanState           `json:"state"`
	Approvals          []*Approval         `json:"approvals"`
	Attempts           []*ExecutionAttempt `json:"attempts"`
	Result             *ExecutionResult    `json:"result,omitempty"`
	ActiveResources    []string            `json:"active_resources,omitempty"`
}

// HasValidApproval 判断计划是否持有某角色的有效批准。
func (p *Plan) HasValidApproval(role ApprovalRole) bool {
	for _, a := range p.Approvals {
		if a.Valid && a.Role == role {
			return true
		}
	}
	return false
}

// CaseKind 为可续办处置链的类型。
type CaseKind string

const (
	CaseSafeMode         CaseKind = "safe_mode"         // 安全模式
	CaseWindowCanceled   CaseKind = "window_canceled"   // 窗口取消
	CaseExecutionTimeout CaseKind = "execution_timeout" // 执行超时
)

// CaseState 为处置单状态。
type CaseState string

const (
	CaseOpen     CaseState = "open"
	CaseResolved CaseState = "resolved"
)

// CaseStep 为处置链上的一次续办动作。
type CaseStep struct {
	Action   string         `json:"action"`
	Operator string         `json:"operator"`
	Note     string         `json:"note"`
	At       time.Time      `json:"at"`
	Payload  map[string]any `json:"payload,omitempty"`
}

// Case 为处置链单据，进程重启后仍可从事件日志中找回并继续办理。
type Case struct {
	ID            string     `json:"id"`
	Kind          CaseKind   `json:"kind"`
	SatelliteID   string     `json:"satellite_id"`
	PlanID        string     `json:"plan_id,omitempty"`
	Summary       string     `json:"summary"`
	State         CaseState  `json:"state"`
	OpenedAt      time.Time  `json:"opened_at"`
	ResolvedAt    *time.Time `json:"resolved_at,omitempty"`
	Steps         []CaseStep `json:"steps"`
	PredecessorID string     `json:"predecessor_id,omitempty"`
	SuccessorID   string     `json:"successor_id,omitempty"`
}

// DeliveryConclusion 为主管交付结论。
type DeliveryConclusion string

const (
	DeliveryAccepted DeliveryConclusion = "accepted"
	DeliveryRejected DeliveryConclusion = "rejected"
)

// MissingItem 描述单星尚缺的一项交付条件。
type MissingItem struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// EvidenceBundle 是签署时冻结的证据包，签署后不再变化。
type EvidenceBundle struct {
	Hash       string               `json:"hash"`
	Satellites []*SatelliteEvidence `json:"satellites"`
}

// ApprovalEvidence 冻结一次批准的完整信息。
type ApprovalEvidence struct {
	ID                string       `json:"id"`
	PlanID            string       `json:"plan_id"`
	PlanKind          string       `json:"plan_kind"`
	Role              ApprovalRole `json:"role"`
	Operator          string       `json:"operator"`
	Note              string       `json:"note"`
	At                time.Time    `json:"at"`
	Valid             bool         `json:"valid"`
	InvalidatedReason string       `json:"invalidated_reason,omitempty"`
	Basis             Basis        `json:"basis"`
}

// CommandVersionEvidence 冻结指令版本全文与批准记录。
type CommandVersionEvidence struct {
	Version   CommandVersion      `json:"version"`
	Approvals []*ApprovalEvidence `json:"approvals"`
}

// ReceiptEvidence 冻结执行结果。
type ReceiptEvidence struct {
	PlanID      string              `json:"plan_id"`
	Kind        string              `json:"kind"`
	Milestone   Milestone           `json:"milestone,omitempty"`
	State       PlanState           `json:"state"`
	Result      *ExecutionResult    `json:"result,omitempty"`
	Attempts    []*ExecutionAttempt `json:"attempts,omitempty"`
	CommandHash string              `json:"command_hash"`
}

// MilestoneEvidence 为单个里程碑的冻结证据。
type MilestoneEvidence struct {
	Milestone Milestone `json:"milestone"`
	Name      string    `json:"name"`
	Complete  bool      `json:"complete"`
	PlanID    string    `json:"plan_id,omitempty"`
	Detail    string    `json:"detail,omitempty"`
}

// SatelliteEvidence 为单星冻结证据。
type SatelliteEvidence struct {
	SatelliteID     string                    `json:"satellite_id"`
	Name            string                    `json:"name"`
	Safe            bool                      `json:"safe"`
	SafeReason      string                    `json:"safe_reason"`
	CurrentFrame    *Frame                    `json:"current_frame,omitempty"`
	Health          *HealthConfirmation       `json:"health,omitempty"`
	Milestones      []*MilestoneEvidence      `json:"milestones"`
	CommandVersions []*CommandVersionEvidence `json:"command_versions"`
	Plans           []*Plan                   `json:"plans"`
	Receipts        []*ReceiptEvidence        `json:"receipts"`
	Windows         []Window                  `json:"windows"`
	OpenCases       []*Case                   `json:"open_cases"`
	Missing         []MissingItem             `json:"missing"`
}

// DeliveryRecord 为一次单星或整组交付签署记录。
type DeliveryRecord struct {
	ID           string             `json:"id"`
	Scope        string             `json:"scope"` // satellite | group
	SatelliteIDs []string           `json:"satellite_ids"`
	Conclusion   DeliveryConclusion `json:"conclusion"`
	Signer       string             `json:"signer"`
	Note         string             `json:"note"`
	SignedAt     time.Time          `json:"signed_at"`
	Bundle       *EvidenceBundle    `json:"bundle"`
}
