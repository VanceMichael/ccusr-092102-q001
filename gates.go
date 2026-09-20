package mission

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// EvaluationContext 是门禁求值时可观测的世界快照。
type EvaluationContext struct {
	now  time.Time
	st   *State
	plan *Plan
	sat  *Satellite
	win  *Window
	// latest 为按星上时间最新的“已验证”当前帧（跨数据源），无则 nil。
	latest *Frame
}

// evaluateGates 对计划的全部前置条件求值。
// 窗口有效与主星/伴星未处于安全模式是计划的固有门禁，由系统隐式加入，
// 因此窗口取消、换版或安全模式发生时，无需调用方声明也会阻止批准与执行。
func evaluateGates(st *State, p *Plan, now time.Time) []GateResult {
	sat := st.Satellites[p.SatelliteID]
	win := st.Windows[p.WindowID]
	ctx := &EvaluationContext{now: now, st: st, plan: p, sat: sat, win: win, latest: latestVerifiedFrame(st, p.SatelliteID)}
	implicit := []Gate{{Type: GateWindowOpen}, {Type: GateSatelliteNominal}}
	if p.PartnerSatelliteID != "" {
		implicit = append(implicit, Gate{Type: GateSatelliteNominal,
			Param: map[string]any{"satellite_id": p.PartnerSatelliteID}})
	}
	all := append(implicit, p.Gates...)
	results := make([]GateResult, 0, len(all))
	for _, g := range all {
		results = append(results, ctx.eval(g))
	}
	return results
}

// latestVerifiedFrame 选取卫星全部数据源中星上时间最新的已验证当前帧。
func latestVerifiedFrame(st *State, satelliteID string) *Frame {
	srcs := st.CurrentBySource[satelliteID]
	var best *Frame
	for _, f := range srcs {
		if f.Quality != QualityVerified {
			continue
		}
		if best == nil || f.SpacecraftTime.After(best.SpacecraftTime) {
			best = f
		}
	}
	return best
}

// frameFor 返回该门禁所针对卫星的最新已验证当前帧（默认主星）。
func (c *EvaluationContext) frameFor(g Gate) *Frame {
	if id, ok := g.Param["satellite_id"].(string); ok && id != "" && id != c.plan.SatelliteID {
		return latestVerifiedFrame(c.st, id)
	}
	return c.latest
}

func (c *EvaluationContext) eval(g Gate) GateResult {
	r := GateResult{Type: g.Type, Expected: g.Param}
	switch g.Type {
	case GateWindowOpen:
		if c.win == nil {
			r.Detail = "窗口不存在"
			return r
		}
		if c.win.State == WindowCanceled {
			r.Detail = "窗口已取消（版本 " + itoa(int64(c.win.Version)) + "）"
			r.Actual = c.win.State
			return r
		}
		if c.plan.WindowVersion != 0 && c.win.Version != c.plan.WindowVersion {
			r.Detail = fmt.Sprintf("窗口版本漂移: 批准依据 v%d，当前 v%d", c.plan.WindowVersion, c.win.Version)
			r.Actual = c.win.Version
			return r
		}
		if c.now.Before(c.win.Start) {
			r.Detail = "窗口尚未开始"
			r.Actual = c.win.Start
			return r
		}
		if !c.now.Before(c.win.End) {
			r.Detail = "窗口已结束"
			r.Actual = c.win.End
			return r
		}
		r.Passed = true

	case GateSatelliteNominal:
		satID := c.plan.SatelliteID
		if id, ok := g.Param["satellite_id"].(string); ok && id != "" {
			satID = id
		}
		sat := c.st.Satellites[satID]
		if sat == nil {
			r.Detail = "卫星不存在: " + satID
			return r
		}
		r.Actual = map[string]any{"satellite_id": satID, "safe": sat.Safe, "safe_generation": sat.SafeGeneration}
		if sat.Safe {
			r.Detail = "卫星 " + satID + " 处于安全模式（第 " + itoa(int64(sat.SafeGeneration)) + " 代）: " + sat.SafeReason
			return r
		}
		r.Passed = true

	case GateTelemetryFresh:
		f := c.frameFor(g)
		if f == nil {
			r.Detail = "没有已验证遥测"
			return r
		}
		maxAge := paramFloat(g.Param, "max_age_seconds", 0)
		age := c.now.Sub(f.SpacecraftTime).Seconds()
		r.Actual = map[string]any{"age_seconds": age, "frame_key": f.Key()}
		if maxAge <= 0 {
			r.Detail = "缺少 max_age_seconds 参数"
			return r
		}
		if age > maxAge {
			r.Detail = fmt.Sprintf("遥测过期: 星龄 %.0fs 超过 %.0fs", age, maxAge)
			return r
		}
		r.Passed = true

	case GateAttitude:
		v, ok := c.valueFor(g, "attitude_mode")
		if !ok {
			r.Detail = "遥测缺少 attitude_mode"
			return r
		}
		want := g.Param["mode"]
		r.Actual = v
		if fmt.Sprint(v) != fmt.Sprint(want) {
			r.Detail = fmt.Sprintf("姿态模式不符: 期望 %v，实际 %v", want, v)
			return r
		}
		r.Passed = true

	case GateBattery:
		v, ok := c.valueFor(g, "battery_soc")
		if !ok {
			r.Detail = "遥测缺少 battery_soc"
			return r
		}
		got, err := toFloat(v)
		if err != nil {
			r.Detail = "battery_soc 不是数值: " + err.Error()
			r.Actual = v
			return r
		}
		min := paramFloat(g.Param, "min", 0)
		r.Actual = got
		if got < min {
			r.Detail = fmt.Sprintf("电量不足: %.1f < %.1f", got, min)
			return r
		}
		r.Passed = true

	case GatePayload:
		v, ok := c.valueFor(g, "payload_on")
		if !ok {
			r.Detail = "遥测缺少 payload_on"
			return r
		}
		want, has := g.Param["on"]
		if !has {
			want = true
		}
		r.Actual = v
		if fmt.Sprint(v) != fmt.Sprint(want) {
			r.Detail = fmt.Sprintf("载荷状态不符: 期望 %v，实际 %v", want, v)
			return r
		}
		r.Passed = true

	case GateData:
		path, _ := g.Param["path"].(string)
		v, ok := c.valueFor(g, path)
		op := strings.TrimSpace(fmt.Sprint(g.Param["op"]))
		want := g.Param["value"]
		r.Actual = v
		if !ok {
			r.Detail = "遥测缺少路径 " + path
			return r
		}
		switch op {
		case "eq":
			if fmt.Sprint(v) != fmt.Sprint(want) {
				r.Detail = fmt.Sprintf("%s: 期望 %v，实际 %v", path, want, v)
				return r
			}
		case "ne":
			if fmt.Sprint(v) == fmt.Sprint(want) {
				r.Detail = fmt.Sprintf("%s: 不应等于 %v", path, want)
				return r
			}
		case "ge", "gt", "le", "lt":
			gv, err1 := toFloat(v)
			wv, err2 := toFloat(want)
			if err1 != nil || err2 != nil {
				r.Detail = fmt.Sprintf("%s: 数值比较失败 (%v, %v)", path, err1, err2)
				return r
			}
			bad := map[string]bool{"ge": gv < wv, "gt": gv <= wv, "le": gv > wv, "lt": gv >= wv}[op]
			if bad {
				r.Detail = fmt.Sprintf("%s: %.3f 不满足 %s %.3f", path, gv, op, wv)
				return r
			}
		default:
			r.Detail = "不支持的比较操作 " + op
			return r
		}
		r.Passed = true

	default:
		r.Detail = "未知前置条件类型: " + string(g.Type)
	}
	return r
}

// valueFor 从门禁所针对卫星的最新已验证帧中按点分路径取值。
func (c *EvaluationContext) valueFor(g Gate, path string) (any, bool) {
	f := c.frameFor(g)
	if f == nil {
		return nil, false
	}
	if path == "" {
		return f.Data, true
	}
	var cur any = f.Data
	for _, seg := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

func paramFloat(m map[string]any, key string, def float64) float64 {
	if m == nil {
		return def
	}
	v, ok := m[key]
	if !ok {
		return def
	}
	f, err := toFloat(v)
	if err != nil {
		return def
	}
	return f
}

func toFloat(v any) (float64, error) {
	switch t := v.(type) {
	case float64:
		return t, nil
	case float32:
		return float64(t), nil
	case int:
		return float64(t), nil
	case int64:
		return float64(t), nil
	case json.Number:
		return t.Float64()
	case string:
		return strconv.ParseFloat(t, 64)
	default:
		return 0, fmt.Errorf("无法转为数值: %T", v)
	}
}

// gatesPass 汇总门禁结果。
func gatesPass(rs []GateResult) bool {
	for _, r := range rs {
		if !r.Passed {
			return false
		}
	}
	return true
}

func describeGateFailure(rs []GateResult) string {
	for _, r := range rs {
		if !r.Passed {
			return string(r.Type) + ": " + r.Detail
		}
	}
	return ""
}
