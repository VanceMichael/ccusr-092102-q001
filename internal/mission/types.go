// Package mission 实现星座在轨交付协同中枢的核心领域逻辑。
//
// 中枢以追加写事件日志（WAL，每批事件一次 fsync）为唯一持久化载体，
// 进程重启后通过完整回放重建内存状态，从而找回所有未完成的处置工作。
package mission

import (
	"encoding/json"
	"errors"
	"time"
)

// 常规错误，API 层据此映射为 4xx/409/422。
var (
	ErrInvalidInput  = errors.New("输入不合法")
	ErrUnknown       = errors.New("对象不存在")
	ErrConflict      = errors.New("资源冲突")
	ErrPrecondition  = errors.New("前置条件不满足")
	ErrNotAcceptable = errors.New("当前状态不允许该操作")
)

// IssueType 为三类可续办处置链。
type IssueType string

const (
	IssueSafeMode     IssueType = "safe_mode"     // 安全模式处置链
	IssueTimeout      IssueType = "timeout"       // 执行超时处置链
	IssueWindowCancel IssueType = "window_cancel" // 窗口取消处置链
)

// TelemetryQuality 遥测质量标记。
type TelemetryQuality string

const (
	QualityVerified TelemetryQuality = "verified"
	QualityDegraded TelemetryQuality = "degraded"
	QualityBad      TelemetryQuality = "bad"
)

// ConditionKey 前置条件键。
type ConditionKey string

const (
	CondAttitude ConditionKey = "attitude" // 姿态
	CondPower    ConditionKey = "power"    // 能源
	CondSafeMode ConditionKey = "safe_mode"
)

type WindowStatus string

const (
	WinScheduled WindowStatus = "scheduled"
	WinCancelled WindowStatus = "cancelled"
	WinClosed    WindowStatus = "closed"
)

// PlanStatus 指令计划状态。
type PlanStatus string

const (
	PlanDraft     PlanStatus = "draft"     // 初稿
	PlanSubmitted PlanStatus = "submitted" // 已提交，待复核
	PlanApproved  PlanStatus = "approved"  // 双人复核通过
	PlanExecuting PlanStatus = "executing" // 已取得执行权并上注
	PlanExecuted  PlanStatus = "executed"  // 星上成功回执
	PlanFailed    PlanStatus = "failed"    // 星上失败回执
	PlanRejected  PlanStatus = "rejected"  // 复核拒绝（终态）
	PlanCancelled PlanStatus = "cancelled" // 窗口取消时尚未提交（终态）
	PlanAwaiting  PlanStatus = "awaiting"  // 执行权丧失，等待处置链续办
)

// IssueStatus 处置链状态。
type IssueStatus string

const (
	IssueOpen       IssueStatus = "open"
	IssueInProgress IssueStatus = "in_progress"
	IssueResolved   IssueStatus = "resolved"
	IssueFailed     IssueStatus = "failed"
)

// Decision 交付结论。
type Decision string

const (
	DecisionAccepted    Decision = "accepted"
	DecisionConditional Decision = "conditional"
	DecisionRejected    Decision = "rejected"
)

// CommandType 指令计划类型，同时对应交付检查项。
type CommandType string

const (
	CmdHealthCheck      CommandType = "health_check"
	CmdPayloadPowerOn   CommandType = "payload_power_on"
	CmdCrossCalibration CommandType = "cross_calibration"
	CmdOther            CommandType = "other"
)

// FrameRef 指向某一帧遥测，作为结论或批准的依据。
type FrameRef struct {
	SourceID       string           `json:"source_id"`
	SourceSequence int64            `json:"source_sequence"`
	SpacecraftTime time.Time        `json:"spacecraft_time"`
	ReceivedAt     time.Time        `json:"received_at"`
	Quality        TelemetryQuality `json:"quality"`
}

// TelemetryFrame 存档的遥测帧。
type TelemetryFrame struct {
	TelemetryID    string                   `json:"telemetry_id"`
	SatelliteID    string                   `json:"satellite_id"`
	SourceID       string                   `json:"source_id"`
	SourceSequence int64                    `json:"source_sequence"`
	SpacecraftTime time.Time                `json:"spacecraft_time"`
	ReceivedAt     time.Time                `json:"received_at"`
	Quality        TelemetryQuality         `json:"quality"`
	Readings       map[ConditionKey]float64 `json:"readings,omitempty"`
	SafeMode       bool                     `json:"safe_mode"`
	// Applied 表示该帧是否参与了状态结论；乱序/重复帧为 false，仅存档留痕。
	Applied bool `json:"applied"`
	// StaleReason 说明未被采用的原因。
	StaleReason string `json:"stale_reason,omitempty"`
}

// ConditionView 单项前置条件的当前结论及其依据。
type ConditionView struct {
	OK       bool      `json:"ok"`
	Value    float64   `json:"value,omitempty"`
	Evidence *FrameRef `json:"evidence,omitempty"`
	Epoch    int       `json:"epoch"`
}

// Lease 资源执行权租约。
type Lease struct {
	LeaseID     string    `json:"lease_id"`
	PlanID      string    `json:"plan_id"`
	SatelliteID string    `json:"satellite_id"`
	StationID   string    `json:"station_id"`
	Seat        string    `json:"seat"`
	AcquiredAt  time.Time `json:"acquired_at"`
	Deadline    time.Time `json:"deadline"`
	Released    bool      `json:"released,omitempty"`
}

// Receipt 星上执行回执。
type Receipt struct {
	Success        bool      `json:"success"`
	Code           string    `json:"code,omitempty"`
	Message        string    `json:"message,omitempty"`
	SpacecraftTime time.Time `json:"spacecraft_time"`
	ReceivedAt     time.Time `json:"received_at"`
	SourceID       string    `json:"source_id,omitempty"`
	SourceSequence int64     `json:"source_sequence,omitempty"`
}

// ApprovalView 批准留痕视图。
type ApprovalView struct {
	ApprovalID     string         `json:"approval_id"`
	PlanRevision   int            `json:"plan_revision"`
	Role           string         `json:"role"`
	Approver       string         `json:"approver"`
	At             time.Time      `json:"at"`
	WindowID       string         `json:"window_id"`
	EpochSnapshot  map[string]int `json:"epoch_snapshot"`
	TelemetryBasis *FrameRef      `json:"telemetry_basis"`
	Invalidated    bool           `json:"invalidated"`
	InvalidReason  string         `json:"invalid_reason,omitempty"`
	InvalidAt      *time.Time     `json:"invalid_at,omitempty"`
}

// PlanView 计划查询视图。
type PlanView struct {
	PlanID        string         `json:"plan_id"`
	SatelliteID   string         `json:"satellite_id"`
	WindowID      string         `json:"window_id"`
	StationID     string         `json:"station_id"`
	CommandType   CommandType    `json:"command_type"`
	PayloadDigest string         `json:"payload_digest"`
	Revision      int            `json:"revision"`
	Status        PlanStatus     `json:"status"`
	CreatedBy     string         `json:"created_by"`
	CreatedAt     time.Time      `json:"created_at"`
	Approvals     []ApprovalView `json:"approvals"`
	ActiveLease   *Lease         `json:"active_lease,omitempty"`
	UplinkedAt    *time.Time     `json:"uplinked_at,omitempty"`
	Receipt       *Receipt       `json:"receipt,omitempty"`
	IssueIDs      []string       `json:"issue_ids,omitempty"`
}

// IssueView 处置链视图。
type IssueView struct {
	IssueID     string                     `json:"issue_id"`
	Type        IssueType                  `json:"type"`
	Status      IssueStatus                `json:"status"`
	SatelliteID string                     `json:"satellite_id"`
	PlanID      string                     `json:"plan_id,omitempty"`
	WindowID    string                     `json:"window_id,omitempty"`
	Reason      string                     `json:"reason"`
	OpenedAt    time.Time                  `json:"opened_at"`
	Steps       []IssueStep                `json:"steps"`
	Resolution  string                     `json:"resolution,omitempty"`
	ResolvedAt  *time.Time                 `json:"resolved_at,omitempty"`
	ResolvedBy  string                     `json:"resolved_by,omitempty"`
	Resumable   map[string]json.RawMessage `json:"resumable,omitempty"`
}

// IssueStep 处置链中的续办记录。
type IssueStep struct {
	At   time.Time `json:"at"`
	By   string    `json:"by"`
	Note string    `json:"note"`
}

// Gap 描述一颗卫星尚缺的交付条件。
type Gap struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// SatelliteView 卫星状态视图。
type SatelliteView struct {
	SatelliteID    string                         `json:"satellite_id"`
	Name           string                         `json:"name,omitempty"`
	RegisteredAt   time.Time                      `json:"registered_at"`
	SafeMode       bool                           `json:"safe_mode"`
	Conditions     map[ConditionKey]ConditionView `json:"conditions"`
	RequiredChecks []CommandType                  `json:"required_checks"`
	Gaps           []Gap                          `json:"gaps"`
	OpenIssues     []string                       `json:"open_issues"`
}

// PlanEvidence 冻结在交付结论中的计划证据。
type PlanEvidence struct {
	PlanID        string         `json:"plan_id"`
	CommandType   CommandType    `json:"command_type"`
	Revision      int            `json:"revision"`
	PayloadDigest string         `json:"payload_digest"`
	Status        PlanStatus     `json:"status"`
	Approvals     []ApprovalView `json:"approvals"`
	Receipt       *Receipt       `json:"receipt,omitempty"`
}

// SatelliteEvidence 单星冻结证据。
type SatelliteEvidence struct {
	SatelliteID string                         `json:"satellite_id"`
	SafeMode    bool                           `json:"safe_mode"`
	Conditions  map[ConditionKey]ConditionView `json:"conditions"`
	Gaps        []Gap                          `json:"gaps"`
	Plans       []PlanEvidence                 `json:"plans"`
	OpenIssues  []string                       `json:"open_issues"`
}

// EvidenceSnapshot 签署时冻结的完整证据快照。
type EvidenceSnapshot struct {
	FrozenAt   time.Time           `json:"frozen_at"`
	Satellites []SatelliteEvidence `json:"satellites"`
}

// DeliveryView 交付结论视图。
type DeliveryView struct {
	DeliveryID   string            `json:"delivery_id"`
	Scope        string            `json:"scope"`
	SatelliteIDs []string          `json:"satellite_ids"`
	Decision     Decision          `json:"decision"`
	By           string            `json:"by"`
	At           time.Time         `json:"at"`
	Notes        string            `json:"notes,omitempty"`
	Evidence     *EvidenceSnapshot `json:"evidence"`
}
