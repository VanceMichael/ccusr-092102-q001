package mission

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// EventLog 是仅追加的事件日志：每条事件占一行 JSON，追加后 fsync。
// 状态完全可由日志重放恢复；快照仅用于加速重启。
type EventLog struct {
	mu   sync.Mutex
	dir  string
	path string
	f    *os.File
	next int64 // 下一条事件的 offset
}

// OpenEventLog 打开（必要时创建）目录下的事件日志，并读出已有事件数量。
func OpenEventLog(dir string) (*EventLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录: %w", err)
	}
	path := filepath.Join(dir, "events.log")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("打开事件日志: %w", err)
	}
	l := &EventLog{dir: dir, path: path, f: f}
	var expect int64 = -1 // 截断后文件首条事件可从任意 offset 开始，之后必须连续
	if err := l.scan(func(e Envelope) error {
		if expect >= 0 && e.Offset != expect {
			return fmt.Errorf("事件日志 offset 不连续: 期望 %d 实际 %d", expect, e.Offset)
		}
		expect = e.Offset + 1
		l.next = e.Offset + 1
		return nil
	}); err != nil {
		_ = f.Close()
		return nil, err
	}
	return l, nil
}

// NextOffset 返回下一条事件的 offset。
func (l *EventLog) NextOffset() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.next
}

// SeedNext 在日志为空（已被快照截断）时，用快照之后的位点播种 offset 计数器。
// 日志非空时取较大值，保证 offset 全局单调。
func (l *EventLog) SeedNext(offset int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if offset > l.next {
		l.next = offset
	}
}

func (l *EventLog) scan(visit func(Envelope) error) error {
	if _, err := l.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	sc := bufio.NewScanner(l.f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e Envelope
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("事件日志损坏: %w", err)
		}
		if err := visit(e); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("读取事件日志: %w", err)
	}
	_, err := l.f.Seek(0, io.SeekEnd)
	return err
}

// Append 在锁内序列化、写入并 fsync 一条事件。offset 由日志统一分配。
func (l *EventLog) Append(e *Envelope) error {
	if e.Type == "" {
		return errors.New("事件类型不能为空")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	e.Offset = l.next
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("序列化事件: %w", err)
	}
	line = append(line, '\n')
	if _, err := l.f.Write(line); err != nil {
		return fmt.Errorf("写入事件日志: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("刷盘事件日志: %w", err)
	}
	l.next++
	return nil
}

// Replay 重放 offset 大于 after 的全部事件（按追加顺序）。
func (l *EventLog) Replay(after int64, visit func(Envelope) error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.scan(func(e Envelope) error {
		if e.Offset > after {
			return visit(e)
		}
		return nil
	})
}

// Truncate 在快照落盘后清空日志（快照必须已经 fsync）。
func (l *EventLog) Truncate() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.f.Truncate(0); err != nil {
		return err
	}
	if _, err := l.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if err := l.f.Sync(); err != nil {
		return err
	}
	// 注意：不重置 l.next —— offset 必须全局单调，
	// 否则快照之后新追加的事件 offset 会小于快照位点而被重放跳过。
	return nil
}

// Close 关闭底层文件。
func (l *EventLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Close()
}
