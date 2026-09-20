// Package api 提供在轨交付中枢的 HTTP 接口。
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"example.com/constellation-handover/internal/mission"
)

// Server 持有中枢服务与演练时钟。
type Server struct {
	Svc     *mission.Service
	Clk     *mission.SimClock
	SimMode bool // 仅演练模式接受 X-Sim-At 头
}

// NewRouter 构造全部路由。
func (s *Server) NewRouter() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", s.health)

	mux.HandleFunc("POST /api/satellites", s.registerSatellite)
	mux.HandleFunc("GET /api/satellites", s.listSatellites)
	mux.HandleFunc("GET /api/satellites/{id}", s.getSatellite)
	mux.HandleFunc("POST /api/satellites/{id}/telemetry", s.ingestTelemetry)
	mux.HandleFunc("GET /api/satellites/{id}/telemetry", s.listTelemetry)

	mux.HandleFunc("POST /api/windows", s.scheduleWindow)
	mux.HandleFunc("GET /api/windows", s.listWindows)
	mux.HandleFunc("GET /api/windows/{id}", s.getWindow)
	mux.HandleFunc("POST /api/windows/{id}/cancel", s.cancelWindow)
	mux.HandleFunc("POST /api/windows/{id}/close", s.closeWindow)

	mux.HandleFunc("POST /api/plans", s.createPlan)
	mux.HandleFunc("GET /api/plans", s.listPlans)
	mux.HandleFunc("GET /api/plans/{id}", s.getPlan)
	mux.HandleFunc("POST /api/plans/{id}/submit", s.submitPlan)
	mux.HandleFunc("POST /api/plans/{id}/revise", s.revisePlan)
	mux.HandleFunc("POST /api/plans/{id}/reject", s.rejectPlan)
	mux.HandleFunc("POST /api/plans/{id}/approvals", s.addApproval)
	mux.HandleFunc("POST /api/plans/{id}/execute", s.executePlan)
	mux.HandleFunc("POST /api/plans/{id}/receipt", s.recordReceipt)

	mux.HandleFunc("GET /api/issues", s.listIssues)
	mux.HandleFunc("GET /api/issues/{id}", s.getIssue)
	mux.HandleFunc("POST /api/issues/{id}/work", s.workIssue)
	mux.HandleFunc("POST /api/issues/{id}/resolve", s.resolveIssue)
	mux.HandleFunc("POST /api/issues/{id}/fail", s.failIssue)

	mux.HandleFunc("POST /api/deliveries", s.signDelivery)
	mux.HandleFunc("GET /api/deliveries", s.listDeliveries)
	mux.HandleFunc("GET /api/deliveries/{id}", s.getDelivery)

	mux.HandleFunc("POST /api/admin/sweep", s.sweep)

	return s.simClockMiddleware(mux)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// simClockMiddleware 解析 X-Sim-At 演练时间头；设置后中枢命令以该时间为准，
// 便于在测试/演练中确定性地触发窗口与执行超时。
func (s *Server) simClockMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if raw := r.Header.Get("X-Sim-At"); raw != "" {
			if !s.SimMode {
				writeError(w, http.StatusBadRequest, "服务未开启演练模式（-sim-clock），拒绝 X-Sim-At")
				return
			}
			t, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				writeError(w, http.StatusBadRequest, "X-Sim-At 必须是 RFC3339 时间: "+err.Error())
				return
			}
			s.Clk.Set(t)
		}
		next.ServeHTTP(w, r)
	})
}

// ---- 卫星与遥测 ----

type registerSatelliteReq struct {
	SatelliteID    string                           `json:"satellite_id"`
	Name           string                           `json:"name"`
	RequiredChecks []mission.CommandType            `json:"required_checks"`
	Thresholds     map[mission.ConditionKey]float64 `json:"thresholds"`
}

func (s *Server) registerSatellite(w http.ResponseWriter, r *http.Request) {
	var req registerSatelliteReq
	if !decode(w, r, &req) {
		return
	}
	err := s.Svc.RegisterSatellite(mission.RegisterSatelliteParams{
		SatelliteID: req.SatelliteID, Name: req.Name,
		RequiredChecks: req.RequiredChecks, Thresholds: req.Thresholds,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"satellite_id": req.SatelliteID})
}

func (s *Server) listSatellites(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Svc.ListSatellites())
}

func (s *Server) getSatellite(w http.ResponseWriter, r *http.Request) {
	v, err := s.Svc.GetSatellite(r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type ingestTelemetryReq struct {
	SourceID       string                           `json:"source_id"`
	SourceSequence int64                            `json:"source_sequence"`
	SpacecraftTime time.Time                        `json:"spacecraft_time"`
	ReceivedAt     time.Time                        `json:"received_at"`
	Quality        mission.TelemetryQuality         `json:"quality"`
	Readings       map[mission.ConditionKey]float64 `json:"readings"`
	SafeMode       bool                             `json:"safe_mode"`
}

func (s *Server) ingestTelemetry(w http.ResponseWriter, r *http.Request) {
	var req ingestTelemetryReq
	if !decode(w, r, &req) {
		return
	}
	f, err := s.Svc.IngestTelemetry(mission.IngestTelemetryParams{
		SatelliteID:    r.PathValue("id"),
		SourceID:       req.SourceID,
		SourceSequence: req.SourceSequence,
		SpacecraftTime: req.SpacecraftTime,
		ReceivedAt:     req.ReceivedAt,
		Quality:        req.Quality,
		Readings:       req.Readings,
		SafeMode:       req.SafeMode,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, f)
}

func (s *Server) listTelemetry(w http.ResponseWriter, r *http.Request) {
	frames, err := s.Svc.ListTelemetry(r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, frames)
}

// ---- 窗口 ----

type windowReq struct {
	WindowID    string    `json:"window_id"`
	SatelliteID string    `json:"satellite_id"`
	StationID   string    `json:"station_id"`
	Start       time.Time `json:"start"`
	End         time.Time `json:"end"`
}

func (s *Server) scheduleWindow(w http.ResponseWriter, r *http.Request) {
	var req windowReq
	if !decode(w, r, &req) {
		return
	}
	err := s.Svc.ScheduleWindow(mission.ScheduleWindowParams{
		WindowID: req.WindowID, SatelliteID: req.SatelliteID,
		StationID: req.StationID, Start: req.Start, End: req.End,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"window_id": req.WindowID})
}

func (s *Server) listWindows(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Svc.ListWindows(r.URL.Query().Get("satellite_id")))
}

func (s *Server) getWindow(w http.ResponseWriter, r *http.Request) {
	v, err := s.Svc.GetWindow(r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type byReasonReq struct {
	By     string `json:"by"`
	Reason string `json:"reason"`
}

func (s *Server) cancelWindow(w http.ResponseWriter, r *http.Request) {
	var req byReasonReq
	if !decode(w, r, &req) {
		return
	}
	if err := s.Svc.CancelWindow(r.PathValue("id"), req.By, req.Reason); err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"window_id": r.PathValue("id"), "status": "cancelled"})
}

func (s *Server) closeWindow(w http.ResponseWriter, r *http.Request) {
	if err := s.Svc.CloseWindow(r.PathValue("id")); err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"window_id": r.PathValue("id"), "status": "closed"})
}

// ---- 计划 / 复核 / 执行 ----

type createPlanReq struct {
	SatelliteID   string              `json:"satellite_id"`
	WindowID      string              `json:"window_id"`
	CommandType   mission.CommandType `json:"command_type"`
	Payload       string              `json:"payload"`
	PayloadDigest string              `json:"payload_digest"`
	CreatedBy     string              `json:"created_by"`
}

func (s *Server) createPlan(w http.ResponseWriter, r *http.Request) {
	var req createPlanReq
	if !decode(w, r, &req) {
		return
	}
	v, err := s.Svc.CreatePlan(mission.CreatePlanParams{
		SatelliteID: req.SatelliteID, WindowID: req.WindowID,
		CommandType: req.CommandType, Payload: req.Payload,
		Digest: req.PayloadDigest, CreatedBy: req.CreatedBy,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) listPlans(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Svc.ListPlans(r.URL.Query().Get("satellite_id")))
}

func (s *Server) getPlan(w http.ResponseWriter, r *http.Request) {
	v, err := s.Svc.GetPlan(r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) submitPlan(w http.ResponseWriter, r *http.Request) {
	if err := s.Svc.SubmitPlan(r.PathValue("id")); err != nil {
		writeDomainError(w, err)
		return
	}
	v, _ := s.Svc.GetPlan(r.PathValue("id"))
	writeJSON(w, http.StatusOK, v)
}

type reviseReq struct {
	Payload       string `json:"payload"`
	PayloadDigest string `json:"payload_digest"`
}

func (s *Server) revisePlan(w http.ResponseWriter, r *http.Request) {
	var req reviseReq
	if !decode(w, r, &req) {
		return
	}
	if err := s.Svc.RevisePlan(r.PathValue("id"), req.Payload, req.PayloadDigest); err != nil {
		writeDomainError(w, err)
		return
	}
	v, _ := s.Svc.GetPlan(r.PathValue("id"))
	writeJSON(w, http.StatusOK, v)
}

type rejectReq struct {
	By     string `json:"by"`
	Reason string `json:"reason"`
}

func (s *Server) rejectPlan(w http.ResponseWriter, r *http.Request) {
	var req rejectReq
	if !decode(w, r, &req) {
		return
	}
	if err := s.Svc.RejectPlan(r.PathValue("id"), req.By, req.Reason); err != nil {
		writeDomainError(w, err)
		return
	}
	v, _ := s.Svc.GetPlan(r.PathValue("id"))
	writeJSON(w, http.StatusOK, v)
}

type approvalReq struct {
	Role     string `json:"role"`
	Approver string `json:"approver"`
}

func (s *Server) addApproval(w http.ResponseWriter, r *http.Request) {
	var req approvalReq
	if !decode(w, r, &req) {
		return
	}
	a, err := s.Svc.AddApproval(r.PathValue("id"), req.Role, req.Approver)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	v, _ := s.Svc.GetPlan(r.PathValue("id"))
	writeJSON(w, http.StatusCreated, map[string]any{"approval": a, "plan": v})
}

type executeReq struct {
	Seat string `json:"seat"`
}

func (s *Server) executePlan(w http.ResponseWriter, r *http.Request) {
	var req executeReq
	if !decode(w, r, &req) {
		return
	}
	lease, err := s.Svc.ExecutePlan(r.PathValue("id"), req.Seat)
	if err != nil {
		writeDomainError(w, err)
		return
	}
	v, _ := s.Svc.GetPlan(r.PathValue("id"))
	writeJSON(w, http.StatusOK, map[string]any{"lease": lease, "plan": v})
}

type receiptReq struct {
	Success        bool      `json:"success"`
	Code           string    `json:"code"`
	Message        string    `json:"message"`
	SpacecraftTime time.Time `json:"spacecraft_time"`
	ReceivedAt     time.Time `json:"received_at"`
	SourceID       string    `json:"source_id"`
	SourceSequence int64     `json:"source_sequence"`
}

func (s *Server) recordReceipt(w http.ResponseWriter, r *http.Request) {
	var req receiptReq
	if !decode(w, r, &req) {
		return
	}
	v, err := s.Svc.RecordReceipt(r.PathValue("id"), mission.Receipt{
		Success: req.Success, Code: req.Code, Message: req.Message,
		SpacecraftTime: req.SpacecraftTime, ReceivedAt: req.ReceivedAt,
		SourceID: req.SourceID, SourceSequence: req.SourceSequence,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

// ---- 处置链 ----

func (s *Server) listIssues(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	writeJSON(w, http.StatusOK, s.Svc.ListIssues(
		mission.IssueType(q.Get("type")),
		q.Get("open") == "1" || strings.EqualFold(q.Get("open"), "true"),
	))
}

func (s *Server) getIssue(w http.ResponseWriter, r *http.Request) {
	v, err := s.Svc.GetIssue(r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type workIssueReq struct {
	By   string `json:"by"`
	Note string `json:"note"`
}

func (s *Server) workIssue(w http.ResponseWriter, r *http.Request) {
	var req workIssueReq
	if !decode(w, r, &req) {
		return
	}
	if err := s.Svc.WorkIssue(r.PathValue("id"), req.By, req.Note); err != nil {
		writeDomainError(w, err)
		return
	}
	v, _ := s.Svc.GetIssue(r.PathValue("id"))
	writeJSON(w, http.StatusOK, v)
}

type resolveIssueReq struct {
	By          string `json:"by"`
	Resolution  string `json:"resolution"`
	NewWindowID string `json:"new_window_id"`
}

func (s *Server) resolveIssue(w http.ResponseWriter, r *http.Request) {
	var req resolveIssueReq
	if !decode(w, r, &req) {
		return
	}
	err := s.Svc.ResolveIssue(mission.ResolveIssueParams{
		IssueID: r.PathValue("id"), By: req.By,
		Resolution: req.Resolution, NewWindowID: req.NewWindowID,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	v, _ := s.Svc.GetIssue(r.PathValue("id"))
	writeJSON(w, http.StatusOK, v)
}

type failIssueReq struct {
	By         string `json:"by"`
	Resolution string `json:"resolution"`
}

func (s *Server) failIssue(w http.ResponseWriter, r *http.Request) {
	var req failIssueReq
	if !decode(w, r, &req) {
		return
	}
	if err := s.Svc.FailIssue(r.PathValue("id"), req.By, req.Resolution); err != nil {
		writeDomainError(w, err)
		return
	}
	v, _ := s.Svc.GetIssue(r.PathValue("id"))
	writeJSON(w, http.StatusOK, v)
}

// ---- 交付结论 ----

type signDeliveryReq struct {
	Scope        string           `json:"scope"`
	SatelliteIDs []string         `json:"satellite_ids"`
	Decision     mission.Decision `json:"decision"`
	By           string           `json:"by"`
	Notes        string           `json:"notes"`
}

func (s *Server) signDelivery(w http.ResponseWriter, r *http.Request) {
	var req signDeliveryReq
	if !decode(w, r, &req) {
		return
	}
	v, err := s.Svc.SignDelivery(mission.SignDeliveryParams{
		Scope: req.Scope, SatelliteIDs: req.SatelliteIDs,
		Decision: req.Decision, By: req.By, Notes: req.Notes,
	})
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) listDeliveries(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Svc.ListDeliveries())
}

func (s *Server) getDelivery(w http.ResponseWriter, r *http.Request) {
	v, err := s.Svc.GetDelivery(r.PathValue("id"))
	if err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) sweep(w http.ResponseWriter, _ *http.Request) {
	if err := s.Svc.Sweep(); err != nil {
		writeDomainError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "swept"})
}

// ---- 辅助 ----

func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func writeDomainError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, mission.ErrInvalidInput):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, mission.ErrUnknown):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, mission.ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, mission.ErrPrecondition):
		writeError(w, http.StatusPreconditionFailed, err.Error())
	case errors.Is(err, mission.ErrNotAcceptable):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}
