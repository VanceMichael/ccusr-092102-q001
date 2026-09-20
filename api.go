// Package mission 下的 api.go 提供后端中枢的 HTTP/JSON 接口。
package mission

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// Server 装配领域服务与 HTTP 路由。
type Server struct {
	svc *Service
	mux *http.ServeMux
}

// NewServer 构造 HTTP 服务。
func NewServer(svc *Service) *Server {
	s := &Server{svc: svc, mux: http.NewServeMux()}
	s.routes()
	return s
}

// Handler 返回带日志与恢复中间件的根处理器。
func (s *Server) Handler() http.Handler {
	return s.recoverLog(s.mux)
}

func (s *Server) routes() {
	m := s.mux

	m.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "time": s.svc.now().Format(time.RFC3339)})
	})

	// 卫星与地面站
	m.HandleFunc("POST /v1/satellites", s.registerSatellite)
	m.HandleFunc("GET /v1/satellites", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.svc.ListSatellites())
	})
	m.HandleFunc("GET /v1/satellites/{id}", s.getSatellite)
	m.HandleFunc("POST /v1/stations", s.registerStation)
	m.HandleFunc("GET /v1/stations", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.svc.ListStations())
	})

	// 测控窗口
	m.HandleFunc("POST /v1/windows", s.scheduleWindow)
	m.HandleFunc("GET /v1/windows", s.listWindows)
	m.HandleFunc("GET /v1/windows/{id}", s.getWindow)
	m.HandleFunc("POST /v1/windows/{id}/cancel", s.cancelWindow)

	// 遥测
	m.HandleFunc("POST /v1/telemetry/frames", s.ingestFrame)
	m.HandleFunc("GET /v1/telemetry/frames", s.getFrame)
	m.HandleFunc("GET /v1/satellites/{id}/frames", s.listSatelliteFrames)
	m.HandleFunc("POST /v1/satellites/{id}/health-confirmations", s.confirmHealth)
	m.HandleFunc("POST /v1/satellites/{id}/safe-mode", s.enterSafeMode)
	m.HandleFunc("POST /v1/satellites/{id}/safe-mode/clear", s.clearSafeMode)

	// 指令版本
	m.HandleFunc("POST /v1/commands", s.createCommand)
	m.HandleFunc("GET /v1/commands", s.listCommands)

	// 计划、复核、执行
	m.HandleFunc("POST /v1/plans", s.createPlan)
	m.HandleFunc("GET /v1/plans", s.listPlans)
	m.HandleFunc("GET /v1/plans/{id}", s.checkPlan)
	m.HandleFunc("POST /v1/plans/{id}/approvals", s.approvePlan)
	m.HandleFunc("POST /v1/plans/{id}/execution/start", s.startExecution)
	m.HandleFunc("POST /v1/plans/{id}/execution/resolve", s.resolveExecution)

	// 处置链
	m.HandleFunc("GET /v1/cases", s.listCases)
	m.HandleFunc("GET /v1/cases/{id}", s.getCase)
	m.HandleFunc("POST /v1/cases/{id}/advance", s.advanceCase)
	m.HandleFunc("POST /v1/cases/{id}/resolve", s.resolveCase)
	m.HandleFunc("POST /v1/cases/{id}/replan", s.replanCase)

	// 就绪度与交付
	m.HandleFunc("GET /v1/readiness", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.svc.ListGroupReadiness())
	})
	m.HandleFunc("GET /v1/satellites/{id}/readiness", s.getReadiness)
	m.HandleFunc("POST /v1/deliveries", s.signDelivery)
	m.HandleFunc("GET /v1/deliveries", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.svc.ListDeliveries())
	})
	m.HandleFunc("GET /v1/deliveries/{id}", s.getDelivery)

	// 运维：手动触发超时扫描
	m.HandleFunc("POST /v1/maintenance/sweep-timeouts", func(w http.ResponseWriter, _ *http.Request) {
		res, err := s.svc.SweepTimeouts()
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"timed_out": res})
	})
}

// ---------- handlers ----------

type registerSatelliteReq struct {
	SatelliteID string `json:"satellite_id"`
	Name        string `json:"name"`
}

func (s *Server) registerSatellite(w http.ResponseWriter, r *http.Request) {
	var req registerSatelliteReq
	if !decodeReq(w, r, &req) {
		return
	}
	if err := s.svc.RegisterSatellite(req.SatelliteID, req.Name); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"satellite_id": req.SatelliteID})
}

func (s *Server) getSatellite(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.GetSatellite(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type registerStationReq struct {
	StationID string `json:"station_id"`
	Name      string `json:"name"`
}

func (s *Server) registerStation(w http.ResponseWriter, r *http.Request) {
	var req registerStationReq
	if !decodeReq(w, r, &req) {
		return
	}
	if err := s.svc.RegisterStation(req.StationID, req.Name); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"station_id": req.StationID})
}

func (s *Server) scheduleWindow(w http.ResponseWriter, r *http.Request) {
	var in ScheduleWindowInput
	if !decodeReq(w, r, &in) {
		return
	}
	if in.ID == "" {
		// 允许客户端省略 window_id，由系统生成。
		in.ID = "win-" + HashString(in.SatelliteID + "|" + in.StationID + "|" + in.Start.Format(time.RFC3339Nano))[:12]
	}
	w2, err := s.svc.ScheduleWindow(in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, w2)
}

func (s *Server) listWindows(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.ListWindows(r.URL.Query().Get("satellite_id"), r.URL.Query().Get("station_id")))
}

func (s *Server) getWindow(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.GetWindow(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

type cancelWindowReq struct {
	Reason string `json:"reason"`
}

func (s *Server) cancelWindow(w http.ResponseWriter, r *http.Request) {
	var req cancelWindowReq
	if !decodeReq(w, r, &req) {
		return
	}
	c, err := s.svc.CancelWindow(r.PathValue("id"), req.Reason)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"case": c})
}

func (s *Server) ingestFrame(w http.ResponseWriter, r *http.Request) {
	var in FrameInput
	if !decodeReq(w, r, &in) {
		return
	}
	res, err := s.svc.IngestFrame(in)
	if err != nil {
		writeError(w, err)
		return
	}
	status := http.StatusCreated
	if res.Duplicate {
		status = http.StatusOK
	}
	writeJSON(w, status, res)
}

func (s *Server) getFrame(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"code": CodeValidation, "message": "缺少 key 查询参数"})
		return
	}
	v, err := s.svc.GetFrame(key)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) listSatelliteFrames(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sourceID := r.URL.Query().Get("source_id")
	writeJSON(w, http.StatusOK, s.svc.ListFrames(id, sourceID, 200))
}

type confirmHealthReq struct {
	Operator string `json:"operator"`
	Note     string `json:"note"`
	SourceID string `json:"source_id"`
}

func (s *Server) confirmHealth(w http.ResponseWriter, r *http.Request) {
	var req confirmHealthReq
	if !decodeReq(w, r, &req) {
		return
	}
	c, err := s.svc.ConfirmHealth(ConfirmHealthInput{
		SatelliteID: r.PathValue("id"), Operator: req.Operator, Note: req.Note, SourceID: req.SourceID})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

type safeModeReq struct {
	Reason   string `json:"reason"`
	Operator string `json:"operator"`
	Note     string `json:"note"`
}

func (s *Server) enterSafeMode(w http.ResponseWriter, r *http.Request) {
	var req safeModeReq
	if !decodeReq(w, r, &req) {
		return
	}
	c, err := s.svc.EnterSafeMode(r.PathValue("id"), req.Reason)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"case": c})
}

func (s *Server) clearSafeMode(w http.ResponseWriter, r *http.Request) {
	var req safeModeReq
	if !decodeReq(w, r, &req) {
		return
	}
	c, err := s.svc.ClearSafeMode(r.PathValue("id"), req.Operator, req.Note)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"case": c})
}

func (s *Server) createCommand(w http.ResponseWriter, r *http.Request) {
	var in CreateCommandInput
	if !decodeReq(w, r, &in) {
		return
	}
	cv, err := s.svc.CreateCommandVersion(in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, cv)
}

func (s *Server) listCommands(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.ListCommandVersions(r.URL.Query().Get("satellite_id")))
}

func (s *Server) createPlan(w http.ResponseWriter, r *http.Request) {
	var in CreatePlanInput
	if !decodeReq(w, r, &in) {
		return
	}
	pl, err := s.svc.CreatePlan(in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, pl)
}

func (s *Server) listPlans(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.ListPlans(r.URL.Query().Get("satellite_id")))
}

func (s *Server) checkPlan(w http.ResponseWriter, r *http.Request) {
	st, err := s.svc.CheckPlan(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, st)
}

type approvePlanReq struct {
	Role     ApprovalRole `json:"role"`
	Operator string       `json:"operator"`
	Note     string       `json:"note"`
	SourceID string       `json:"source_id"`
}

func (s *Server) approvePlan(w http.ResponseWriter, r *http.Request) {
	var req approvePlanReq
	if !decodeReq(w, r, &req) {
		return
	}
	a, gates, err := s.svc.ApprovePlan(ApprovePlanInput{
		PlanID: r.PathValue("id"), Role: req.Role, Operator: req.Operator,
		Note: req.Note, SourceID: req.SourceID})
	if err != nil {
		// 前置条件失败时同时返回门禁明细，便于席位定位原因。
		if ae, ok := IsAPIError(err); ok && ae.Code == CodePrecondition {
			writeJSON(w, http.StatusPreconditionFailed, map[string]any{"error": ae, "gates": gates})
			return
		}
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"approval": a, "gates": gates})
}

type operatorReq struct {
	Operator string `json:"operator"`
}

func (s *Server) startExecution(w http.ResponseWriter, r *http.Request) {
	var req operatorReq
	if !decodeReq(w, r, &req) {
		return
	}
	pl, attempt, err := s.svc.StartExecution(StartExecutionInput{PlanID: r.PathValue("id"), Operator: req.Operator})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"plan": pl, "attempt": attempt})
}

func (s *Server) resolveExecution(w http.ResponseWriter, r *http.Request) {
	var in ResolveExecutionInput
	if !decodeReq(w, r, &in) {
		return
	}
	in.PlanID = r.PathValue("id")
	pl, err := s.svc.ResolveExecution(in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, pl)
}

func (s *Server) listCases(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	writeJSON(w, http.StatusOK, s.svc.ListCases(CaseKind(q.Get("kind")), q.Get("open_only") == "true" || q.Get("open_only") == "1"))
}

func (s *Server) getCase(w http.ResponseWriter, r *http.Request) {
	c, err := s.svc.GetCase(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

type advanceCaseReq struct {
	Action   string         `json:"action"`
	Operator string         `json:"operator"`
	Note     string         `json:"note"`
	Payload  map[string]any `json:"payload"`
}

func (s *Server) advanceCase(w http.ResponseWriter, r *http.Request) {
	var req advanceCaseReq
	if !decodeReq(w, r, &req) {
		return
	}
	c, err := s.svc.AdvanceCase(r.PathValue("id"), req.Action, req.Operator, req.Note, req.Payload)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

type resolveCaseReq struct {
	Operator string `json:"operator"`
	Note     string `json:"note"`
}

func (s *Server) resolveCase(w http.ResponseWriter, r *http.Request) {
	var req resolveCaseReq
	if !decodeReq(w, r, &req) {
		return
	}
	c, err := s.svc.ResolveCase(r.PathValue("id"), req.Operator, req.Note)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, c)
}

type replanReq struct {
	Operator    string `json:"operator"`
	NewWindowID string `json:"new_window_id"`
	Note        string `json:"note"`
}

func (s *Server) replanCase(w http.ResponseWriter, r *http.Request) {
	var req replanReq
	if !decodeReq(w, r, &req) {
		return
	}
	pl, c, err := s.svc.ReplanFromCase(ReplanFromCaseInput{
		CaseID: r.PathValue("id"), Operator: req.Operator, NewWindowID: req.NewWindowID, Note: req.Note})
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"plan": pl, "case": c})
}

func (s *Server) getReadiness(w http.ResponseWriter, r *http.Request) {
	v, err := s.svc.GetReadiness(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) signDelivery(w http.ResponseWriter, r *http.Request) {
	var in SignDeliveryInput
	if !decodeReq(w, r, &in) {
		return
	}
	rec, err := s.svc.SignDelivery(in)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, rec)
}

func (s *Server) getDelivery(w http.ResponseWriter, r *http.Request) {
	rec, err := s.svc.GetDelivery(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rec)
}

// ---------- 基础设施 ----------

func decodeReq(w http.ResponseWriter, r *http.Request, dst any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"code": CodeValidation, "message": "请求体不是合法 JSON: " + err.Error()})
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		log.Printf("写响应失败: %v", err)
	}
}

// errorBody 与 APIError 对应。
type errorBody struct {
	Error *APIError `json:"error"`
}

func writeError(w http.ResponseWriter, err error) {
	ae, ok := IsAPIError(err)
	if !ok {
		if errors.Is(err, http.ErrAbortHandler) {
			return
		}
		ae = &APIError{Code: "internal", Message: err.Error()}
	}
	writeJSON(w, statusForCode(ae.Code), errorBody{Error: ae})
}

func statusForCode(code string) int {
	switch code {
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict, CodeStateConflict:
		return http.StatusConflict
	case CodeValidation:
		return http.StatusBadRequest
	case CodePrecondition:
		return http.StatusPreconditionFailed
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) recoverLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic %s %s: %v", r.Method, r.URL.Path, rec)
				writeJSON(w, http.StatusInternalServerError, map[string]string{
					"code": "internal", "message": "服务内部错误"})
			}
		}()
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, strings.TrimPrefix(r.URL.Path, "/"), time.Since(start))
	})
}
