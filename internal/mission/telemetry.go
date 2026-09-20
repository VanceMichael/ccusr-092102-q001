package mission

import "time"

func (s *Service) applySatelliteRegistered(d evSatelliteRegisteredData) {
	if _, exists := s.sats[d.SatelliteID]; exists {
		return
	}
	th := map[ConditionKey]float64{}
	for k, v := range s.cfg.DefaultThresholds {
		th[k] = v
	}
	for k, v := range d.Thresholds {
		th[k] = v
	}
	checks := d.RequiredChecks
	if len(checks) == 0 {
		checks = []CommandType{CmdHealthCheck, CmdPayloadPowerOn, CmdCrossCalibration}
	}
	s.sats[d.SatelliteID] = &satState{
		id: d.SatelliteID, name: d.Name, required: checks, thresholds: th,
		registeredAt: d.At,
		latestSeq:    map[string]int64{},
		frames:       []TelemetryFrame{},
		frameBySrc:   map[string]map[int64]int{},
		cond:         map[ConditionKey]*condState{},
	}
}

// applyTelemetry 把一帧写入投影。
//
// 乱序规则（保证“已确认状态不倒退”）：同一数据源内，只有源内序号严格大于
// 已采用序号的帧才能更新结论；星上时间早于当前依据的帧同样不采用。
// 被跳过的帧仍完整存档并标注原因，质量标记为 bad 的帧永不参与结论。
func (s *Service) applyTelemetry(f TelemetryFrame) {
	st := s.sats[f.SatelliteID]
	if st == nil {
		return
	}
	s.bumpSeq("TM", f.TelemetryID)
	st.frames = append(st.frames, f)
	if f.SourceSequence > st.latestSeq[f.SourceID] {
		st.latestSeq[f.SourceID] = f.SourceSequence
	}
	bySrc := st.frameBySrc[f.SourceID]
	if bySrc == nil {
		bySrc = map[int64]int{}
		st.frameBySrc[f.SourceID] = bySrc
	}
	bySrc[f.SourceSequence] = len(st.frames) - 1
	if !f.Applied {
		return
	}

	// 质量坏帧永不更新结论；纪元不推进。
	if f.Quality == QualityBad {
		return
	}

	// 安全模式优先：进入安全模式推进安全纪元（姿态/能源结论冻结，
	// 等安全模式退出后的新帧重建）。
	if f.SafeMode {
		// 仅当该帧星上时间不早于已知安全模式进入时刻才采纳，
		// 防止其他数据源的旧帧把状态“翻回去”。
		if !st.safe {
			st.safe = true
			st.safeEpoch++
			st.safeScTime = f.SpacecraftTime
		} else if f.SpacecraftTime.After(st.safeScTime) {
			st.safeScTime = f.SpacecraftTime
		}
		return
	}
	if st.safe {
		// 早于安全模式进入时刻的帧属于历史数据，不得用于退出确认。
		if !f.SpacecraftTime.After(st.safeScTime) {
			return
		}
		// 收到明确更新的非安全模式帧视为退出安全模式：推进纪元，
		// 此前所有条件结论作废，等待本帧及后续帧重建。
		st.safe = false
		st.safeEpoch++
		for _, c := range st.cond {
			c.has = false
			c.epoch++
		}
	}

	for key, val := range f.Readings {
		c := st.cond[key]
		if c == nil {
			c = &condState{}
			st.cond[key] = c
		}
		// 只接受星上时间更新的读数；同刻或更早的帧不回退结论。
		if c.has && !f.SpacecraftTime.After(c.scTime) {
			continue
		}
		established, prevOK := c.everEstablished, c.ok
		c.has = true
		c.value = val
		c.scTime = f.SpacecraftTime
		th, ok := st.thresholds[key]
		if !ok {
			th = 0
		}
		c.ok = val >= th
		fr := frameRefOf(f)
		c.ref = &fr
		// 纪元仅在结论首次建立或达标与否翻转时推进；
		// 同样达标的更新帧只换依据、不使既有批准失效。
		if !established || c.ok != prevOK {
			c.epoch++
		}
		c.everEstablished = true
	}
}

func frameRefOf(f TelemetryFrame) FrameRef {
	return FrameRef{
		SourceID:       f.SourceID,
		SourceSequence: f.SourceSequence,
		SpacecraftTime: f.SpacecraftTime,
		ReceivedAt:     f.ReceivedAt,
		Quality:        f.Quality,
	}
}

// frameAccepted 判定一帧是否应参与结论（命令侧校验，与投影规则一致）。
// 返回 false 时给出存档原因。
func (s *Service) frameAcceptance(st *satState, f TelemetryFrame) (bool, string) {
	if f.Quality == QualityBad {
		return false, "quality=bad"
	}
	if dup, ok := st.frameBySrc[f.SourceID][f.SourceSequence]; ok {
		_ = dup
		return false, "duplicate_source_sequence"
	}
	if cur, ok := st.latestSeq[f.SourceID]; ok && f.SourceSequence <= cur {
		return false, "out_of_order_sequence"
	}
	// 安全模式状态翻转帧总是采用（由投影按星上时间裁定先后）。
	if f.SafeMode || st.safe {
		return true, ""
	}
	for key := range f.Readings {
		if c := st.cond[key]; c != nil && c.has && !f.SpacecraftTime.After(c.scTime) {
			return false, "stale_spacecraft_time"
		}
	}
	return true, ""
}

var _ = time.Now
