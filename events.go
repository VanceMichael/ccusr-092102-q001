package mission

import "time"

// 事件类型常量。事件一经追加不可变，系统状态全部由事件重放得到；
// 进程重启后从日志（加快照）恢复，未完成的执行尝试与处置链都能找回。
const (
	EvSatelliteRegistered   = "satellite_registered"
	EvStationRegistered     = "station_registered"
	EvWindowScheduled       = "window_scheduled"
	EvWindowCanceled        = "window_canceled"
	EvFrameIngested         = "frame_ingested"
	EvHealthConfirmed       = "health_confirmed"
	EvCommandVersionCreated = "command_version_created"
	EvPlanCreated           = "plan_created"
	EvPlanApproved          = "plan_approved"
	EvApprovalInvalidated   = "approval_invalidated"
	EvPlanExecutionStarted  = "plan_execution_started"
	EvPlanExecutionResolved = "plan_execution_resolved"
	EvSafeModeEntered       = "safe_mode_entered"
	EvSafeModeCleared       = "safe_mode_cleared"
	EvCaseOpened            = "case_opened"
	EvCaseAdvanced          = "case_advanced"
	EvCaseResolved          = "case_resolved"
	EvCaseLinked            = "case_linked"
	EvDeliverySigned        = "delivery_signed"
)

// Envelope 是事件日志中每条记录的统一外壳。
type Envelope struct {
	Offset     int64          `json:"offset"`
	Type       string         `json:"type"`
	OccurredAt time.Time      `json:"occurred_at"`
	Payload    map[string]any `json:"payload"`
}

// SatelliteRegisteredPayload 注册卫星。
type SatelliteRegisteredPayload struct {
	SatelliteID string `json:"satellite_id"`
	Name        string `json:"name"`
}

// StationRegisteredPayload 注册地面站。
type StationRegisteredPayload struct {
	StationID string `json:"station_id"`
	Name      string `json:"name"`
}

// WindowScheduledPayload 排定/改期测控窗口；改期使用新版本号。
type WindowScheduledPayload struct {
	Window Window `json:"window"`
}

// WindowCanceledPayload 取消窗口；同时原子失效相关批准、中止执行中的计划并入立处置链。
type WindowCanceledPayload struct {
	WindowID       string                `json:"window_id"`
	Version        int                   `json:"version"`
	At             time.Time             `json:"at"`
	Reason         string                `json:"reason"`
	Invalidated    []InvalidatedApproval `json:"invalidated,omitempty"`
	AbortedPlanIDs []string              `json:"aborted_plan_ids,omitempty"`
	Case           *Case                 `json:"case,omitempty"`
}

// FrameIngestedPayload 记录一帧遥测的接收与裁决结果。
type FrameIngestedPayload struct {
	Frame         Frame                 `json:"frame"`
	Duplicate     bool                  `json:"duplicate"`
	BecameCurrent bool                  `json:"became_current"`
	ConditionSig  string                `json:"condition_sig"`
	Invalidated   []InvalidatedApproval `json:"invalidated,omitempty"`
}

// InvalidatedApproval 描述随本次事件一并失效的批准。
type InvalidatedApproval struct {
	PlanID     string    `json:"plan_id"`
	ApprovalID string    `json:"approval_id"`
	Reason     string    `json:"reason"`
	At         time.Time `json:"at"`
}

// HealthConfirmedPayload 健康确认。
type HealthConfirmedPayload struct {
	Confirmation HealthConfirmation `json:"confirmation"`
}

// CommandVersionCreatedPayload 登记指令新版本。
type CommandVersionCreatedPayload struct {
	Version CommandVersion `json:"version"`
}

// PlanCreatedPayload 创建指令计划。
type PlanCreatedPayload struct {
	Plan Plan `json:"plan"`
}

// PlanApprovedPayload 双人复核签名，携带冻结的依据。
type PlanApprovedPayload struct {
	Approval Approval `json:"approval"`
}

// ApprovalInvalidatedPayload 前置条件漂移导致批准失效。
type ApprovalInvalidatedPayload struct {
	PlanID     string    `json:"plan_id"`
	ApprovalID string    `json:"approval_id"`
	Reason     string    `json:"reason"`
	At         time.Time `json:"at"`
}

// PlanExecutionStartedPayload 取得执行权并开始上行；Start 即锁事件。
type PlanExecutionStartedPayload struct {
	PlanID    string           `json:"plan_id"`
	Attempt   int              `json:"attempt"`
	Resources []string         `json:"resources"`
	Started   ExecutionAttempt `json:"started"`
}

// PlanExecutionResolvedPayload 执行落定（成功/失败/中止/超时），锁随之释放；
// 成功时原子登记里程碑；超时时原子开立可续办的处置链。
type PlanExecutionResolvedPayload struct {
	PlanID         string           `json:"plan_id"`
	Result         ExecutionResult  `json:"result"`
	Milestone      *MilestoneRecord `json:"milestone,omitempty"`
	Case           *Case            `json:"case,omitempty"`
	AcknowledgedAt *time.Time       `json:"acknowledged_at,omitempty"`
	ReceiptStatus  string           `json:"receipt_status,omitempty"`
	ReceiptAt      *time.Time       `json:"receipt_at,omitempty"`
	ReceiptSeq     *int64           `json:"receipt_seq,omitempty"`
}

// SafeModeEnteredPayload 卫星进入安全模式；同时原子失效相关批准、中止执行中的计划并入立处置链。
type SafeModeEnteredPayload struct {
	SatelliteID    string                `json:"satellite_id"`
	Reason         string                `json:"reason"`
	At             time.Time             `json:"at"`
	Generation     int                   `json:"generation"`
	Invalidated    []InvalidatedApproval `json:"invalidated,omitempty"`
	AbortedPlanIDs []string              `json:"aborted_plan_ids,omitempty"`
	Case           *Case                 `json:"case,omitempty"`
}

// SafeModeClearedPayload 安全模式解除（续办结果）。
type SafeModeClearedPayload struct {
	SatelliteID string    `json:"satellite_id"`
	Generation  int       `json:"generation"`
	At          time.Time `json:"at"`
	Operator    string    `json:"operator"`
}

// CaseOpenedPayload 开立处置链。
type CaseOpenedPayload struct {
	Case Case `json:"case"`
}

// CaseAdvancedPayload 处置链续办一步。
type CaseAdvancedPayload struct {
	CaseID string   `json:"case_id"`
	Step   CaseStep `json:"step"`
}

// CaseResolvedPayload 处置链关闭。
type CaseResolvedPayload struct {
	CaseID string    `json:"case_id"`
	At     time.Time `json:"at"`
	Note   string    `json:"note"`
}

// CaseLinkedPayload 处置链前后衔接（如取消后改挂新窗口）。
type CaseLinkedPayload struct {
	PredecessorID string `json:"predecessor_id"`
	SuccessorID   string `json:"successor_id"`
}

// DeliverySignedPayload 主管签署交付结论并冻结证据。
type DeliverySignedPayload struct {
	Record DeliveryRecord `json:"record"`
}
