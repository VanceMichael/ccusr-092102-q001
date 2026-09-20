package mission

import (
	"sort"
	"time"
)

// SatelliteView 为卫星详情视图。
type SatelliteView struct {
	Satellite     *Satellite        `json:"satellite"`
	CurrentFrames map[string]*Frame `json:"current_frames"` // 按数据源
	FrameCount    int               `json:"frame_count"`
}

// GetSatellite 返回卫星与其各数据源当前帧。
func (s *Service) GetSatellite(id string) (*SatelliteView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sat := s.state.Satellites[id]
	if sat == nil {
		return nil, apiErr(CodeNotFound, "卫星 %s 不存在", id)
	}
	v := &SatelliteView{Satellite: clone(sat), CurrentFrames: map[string]*Frame{}, FrameCount: 0}
	for _, f := range s.state.Frames {
		if f.SatelliteID == id {
			v.FrameCount++
		}
	}
	for src, f := range s.state.CurrentBySource[id] {
		v.CurrentFrames[src] = clone(f)
	}
	return v, nil
}

// ListSatellites 列出全部卫星。
func (s *Service) ListSatellites() []*Satellite {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.state.Satellites))
	for id := range s.state.Satellites {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*Satellite, 0, len(ids))
	for _, id := range ids {
		out = append(out, clone(s.state.Satellites[id]))
	}
	return out
}

// ListStations 列出全部地面站。
func (s *Service) ListStations() []*Station {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := make([]string, 0, len(s.state.Stations))
	for id := range s.state.Stations {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*Station, 0, len(ids))
	for _, id := range ids {
		out = append(out, clone(s.state.Stations[id]))
	}
	return out
}

// FrameView 为帧详情。
type FrameView struct {
	Frame   Frame `json:"frame"`
	Current bool  `json:"current"`
}

// GetFrame 按键读取一帧。
func (s *Service) GetFrame(key string) (*FrameView, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	f := s.state.Frames[key]
	if f == nil {
		return nil, apiErr(CodeNotFound, "帧 %s 不存在", key)
	}
	cur := s.state.CurrentBySource[f.SatelliteID][f.SourceID]
	return &FrameView{Frame: *clone(f), Current: cur != nil && cur.Key() == key}, nil
}

// ListFrames 按卫星列出帧（可限数据源），按 (源,序号) 排序。
func (s *Service) ListFrames(satelliteID, sourceID string, limit int) []*Frame {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Frame
	for _, f := range s.state.Frames {
		if satelliteID != "" && f.SatelliteID != satelliteID {
			continue
		}
		if sourceID != "" && f.SourceID != sourceID {
			continue
		}
		out = append(out, clone(f))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SourceID != out[j].SourceID {
			return out[i].SourceID < out[j].SourceID
		}
		return out[i].SourceSequence < out[j].SourceSequence
	})
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// ListWindows 列出窗口，可按卫星或地面站过滤。
func (s *Service) ListWindows(satelliteID, stationID string) []*Window {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Window
	for _, w := range s.state.Windows {
		if satelliteID != "" && w.SatelliteID != satelliteID {
			continue
		}
		if stationID != "" && w.StationID != stationID {
			continue
		}
		out = append(out, clone(w))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Start.Equal(out[j].Start) {
			return out[i].Start.Before(out[j].Start)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// GetWindow 读取窗口。
func (s *Service) GetWindow(id string) (*Window, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	w := s.state.Windows[id]
	if w == nil {
		return nil, apiErr(CodeNotFound, "窗口 %s 不存在", id)
	}
	return clone(w), nil
}

// GetCase 读取处置链。
func (s *Service) GetCase(id string) (*Case, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.state.Cases[id]
	if c == nil {
		return nil, apiErr(CodeNotFound, "处置单 %s 不存在", id)
	}
	return clone(c), nil
}

// ListCases 列出处置链，openOnly 为 true 时只返回未关闭项。
func (s *Service) ListCases(kind CaseKind, openOnly bool) []*Case {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*Case
	for _, id := range s.state.SortedCaseIDs() {
		c := s.state.Cases[id]
		if kind != "" && c.Kind != kind {
			continue
		}
		if openOnly && c.State != CaseOpen {
			continue
		}
		out = append(out, clone(c))
	}
	return out
}

// GetCommandVersion 读取指令版本。
func (s *Service) GetCommandVersion(id string) (*CommandVersion, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cv := s.state.Commands[id]
	if cv == nil {
		return nil, apiErr(CodeNotFound, "指令版本 %s 不存在", id)
	}
	return clone(cv), nil
}

// ListCommandVersions 列出某星的指令版本。
func (s *Service) ListCommandVersions(satelliteID string) []*CommandVersion {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*CommandVersion
	for _, cv := range s.state.Commands {
		if satelliteID != "" && cv.SatelliteID != satelliteID {
			continue
		}
		out = append(out, clone(cv))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SatelliteID != out[j].SatelliteID {
			return out[i].SatelliteID < out[j].SatelliteID
		}
		if out[i].Command != out[j].Command {
			return out[i].Command < out[j].Command
		}
		return out[i].Revision < out[j].Revision
	})
	return out
}

// ListPlans 列出计划，可按卫星过滤。
func (s *Service) ListPlans(satelliteID string) []*Plan {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var ids []string
	for id, pl := range s.state.Plans {
		if satelliteID == "" || pl.SatelliteID == satelliteID {
			ids = append(ids, id)
		}
	}
	sortPlanIDsByTime(s.state, ids)
	out := make([]*Plan, 0, len(ids))
	for _, id := range ids {
		out = append(out, clone(s.state.Plans[id]))
	}
	return out
}

// Now 返回服务当前时钟（测试/调试用）。
func (s *Service) Now() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.now()
}

// LogOffset 返回日志下一条事件 offset（调试用）。
func (s *Service) LogOffset() int64 { return s.log.NextOffset() }
