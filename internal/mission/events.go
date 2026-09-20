package mission

import (
	"encoding/json"
	"time"
)

// Event 是事件日志中的一条不可变记录。Data 为具体事件负载。
type Event struct {
	ID   int64           `json:"id"`
	At   time.Time       `json:"at"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// 事件类型常量。
const (
	evSatelliteRegistered  = "satellite_registered"
	evTelemetryIngested    = "telemetry_ingested"
	evWindowScheduled      = "window_scheduled"
	evWindowCancelled      = "window_cancelled"
	evWindowClosed         = "window_closed"
	evPlanCreated          = "plan_created"
	evPlanSubmitted        = "plan_submitted"
	evPlanRevised          = "plan_revised"
	evPlanRejected         = "plan_rejected"
	evApprovalAdded        = "approval_added"
	evApprovalsInvalidated = "approvals_invalidated"
	evPlanUplinked         = "plan_uplinked"
	evLeaseRevoked         = "lease_revoked"
	evLeaseExpired         = "lease_expired"
	evReceiptRecorded      = "receipt_recorded"
	evPlanReassigned       = "plan_reassigned"
	evIssueOpened          = "issue_opened"
	evIssueWorked          = "issue_worked"
	evIssueResolved        = "issue_resolved"
	evIssueFailed          = "issue_failed"
	evDeliverySigned       = "delivery_signed"
)

type evSatelliteRegisteredData struct {
	SatelliteID    string                   `json:"satellite_id"`
	Name           string                   `json:"name"`
	RequiredChecks []CommandType            `json:"required_checks"`
	Thresholds     map[ConditionKey]float64 `json:"thresholds"`
	At             time.Time                `json:"at"`
}

type evTelemetryIngestedData struct {
	Frame TelemetryFrame `json:"frame"`
}

type evWindowScheduledData struct {
	WindowID    string    `json:"window_id"`
	SatelliteID string    `json:"satellite_id"`
	StationID   string    `json:"station_id"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
	At          time.Time `json:"at"`
}

type evWindowCancelledData struct {
	WindowID string    `json:"window_id"`
	At       time.Time `json:"at"`
	Reason   string    `json:"reason"`
	By       string    `json:"by"`
}

type evWindowClosedData struct {
	WindowID string    `json:"window_id"`
	At       time.Time `json:"at"`
}

type evPlanCreatedData struct {
	PlanID        string      `json:"plan_id"`
	SatelliteID   string      `json:"satellite_id"`
	WindowID      string      `json:"window_id"`
	CommandType   CommandType `json:"command_type"`
	PayloadDigest string      `json:"payload_digest"`
	Payload       string      `json:"payload,omitempty"`
	CreatedBy     string      `json:"created_by"`
	At            time.Time   `json:"at"`
}

type evPlanSubmittedData struct {
	PlanID   string    `json:"plan_id"`
	Revision int       `json:"revision"`
	At       time.Time `json:"at"`
}

type evPlanRevisedData struct {
	PlanID        string    `json:"plan_id"`
	Revision      int       `json:"revision"`
	PayloadDigest string    `json:"payload_digest"`
	Payload       string    `json:"payload,omitempty"`
	At            time.Time `json:"at"`
}

type evPlanRejectedData struct {
	PlanID string    `json:"plan_id"`
	By     string    `json:"by"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
}

type evApprovalAddedData struct {
	PlanID         string         `json:"plan_id"`
	ApprovalID     string         `json:"approval_id"`
	PlanRevision   int            `json:"plan_revision"`
	Role           string         `json:"role"`
	Approver       string         `json:"approver"`
	At             time.Time      `json:"at"`
	WindowID       string         `json:"window_id"`
	EpochSnapshot  map[string]int `json:"epoch_snapshot"`
	TelemetryBasis FrameRef       `json:"telemetry_basis"`
}

type evApprovalsInvalidatedData struct {
	PlanID string    `json:"plan_id"`
	Reason string    `json:"reason"`
	At     time.Time `json:"at"`
	// ApprovalIDs 为本次失效的具体批准；空表示当前版本全部有效批准。
	ApprovalIDs []string `json:"approval_ids,omitempty"`
}

type evPlanUplinkedData struct {
	PlanID     string    `json:"plan_id"`
	Lease      Lease     `json:"lease"`
	At         time.Time `json:"at"`
	CommandRev int       `json:"plan_revision"`
}

type evLeaseRevokedData struct {
	PlanID  string    `json:"plan_id"`
	Reason  string    `json:"reason"`
	IssueID string    `json:"issue_id,omitempty"`
	At      time.Time `json:"at"`
}

type evLeaseExpiredData struct {
	PlanID  string    `json:"plan_id"`
	IssueID string    `json:"issue_id"`
	At      time.Time `json:"at"`
}

type evReceiptRecordedData struct {
	PlanID  string    `json:"plan_id"`
	Receipt Receipt   `json:"receipt"`
	At      time.Time `json:"at"`
}

type evPlanReassignedData struct {
	PlanID    string     `json:"plan_id"`
	WindowID  string     `json:"window_id"`
	StationID string     `json:"station_id"`
	RestoreTo PlanStatus `json:"restore_to"`
	IssueID   string     `json:"issue_id"`
	At        time.Time  `json:"at"`
}

type evIssueOpenedData struct {
	Issue IssueView `json:"issue"`
}

type evIssueWorkedData struct {
	IssueID string    `json:"issue_id"`
	Step    IssueStep `json:"step"`
}

type evIssueResolvedData struct {
	IssueID    string    `json:"issue_id"`
	Resolution string    `json:"resolution"`
	By         string    `json:"by"`
	At         time.Time `json:"at"`
}

type evIssueFailedData struct {
	IssueID    string    `json:"issue_id"`
	Resolution string    `json:"resolution"`
	By         string    `json:"by"`
	At         time.Time `json:"at"`
}

type evDeliverySignedData struct {
	Delivery DeliveryView `json:"delivery"`
}
