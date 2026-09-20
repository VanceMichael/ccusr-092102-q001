package mission

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Store 是 JSON Lines 追加事件日志：每批 append 一次 fsync，进程重启后回放。
// 单进程内通过 Service 持有的互斥锁串行化命令；Store 自身也线程安全。
type Store struct {
	mu     sync.Mutex
	path   string
	f      *os.File
	w      *bufio.Writer
	next   int64
	events []Event
}

// OpenStore 打开（或创建）事件日志并回放全部历史事件。
func OpenStore(path string) (*Store, []Event, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, nil, err
	}
	s := &Store{path: path, f: f, next: 1}

	events := make([]Event, 0, 256)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			f.Close()
			return nil, nil, fmt.Errorf("事件日志第 %d 行损坏: %w", lineNo, err)
		}
		events = append(events, ev)
		if ev.ID >= s.next {
			s.next = ev.ID + 1
		}
	}
	if err := sc.Err(); err != nil {
		f.Close()
		return nil, nil, err
	}
	s.w = bufio.NewWriter(f)
	s.events = events
	return s, events, nil
}

// History 返回日志中的全部事件（含本次进程启动前的历史）。
func (s *Store) History() ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out, nil
}

// Append 给一批事件分配自增 ID、序列化并以一次 fsync 落盘。
// now 为事件时间来源，便于测试注入演练时钟。
func (s *Store) Append(now time.Time, evs []Event) ([]Event, error) {
	if len(evs) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	buf := make([]byte, 0, 4096)
	for i := range evs {
		evs[i].ID = s.next
		s.next++
		if evs[i].At.IsZero() {
			evs[i].At = now
		}
		line, err := json.Marshal(evs[i])
		if err != nil {
			return nil, err
		}
		buf = append(buf, line...)
		buf = append(buf, '\n')
	}
	if _, err := s.f.Write(buf); err != nil {
		return nil, err
	}
	if err := s.f.Sync(); err != nil {
		return nil, err
	}
	s.events = append(s.events, evs...)
	return evs, nil
}

// Close 关闭底层文件。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.f.Close()
}
