package mission

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// snapshotFile 为快照文件结构：日志偏移量加完整聚合状态。
type snapshotFile struct {
	Offset int64           `json:"offset"`
	State  json.RawMessage `json:"state"`
}

// SaveSnapshot 将状态原子写入快照文件（临时文件 fsync 后 rename）。
// 调用方必须保证 offset 之前的事件日志已经落盘。
func SaveSnapshot(dir string, offset int64, state any) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("序列化快照: %w", err)
	}
	tmp := filepath.Join(dir, "snapshot.tmp")
	final := filepath.Join(dir, "snapshot.json")
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("创建快照临时文件: %w", err)
	}
	enc := json.NewEncoder(f)
	if err := enc.Encode(snapshotFile{Offset: offset, State: raw}); err != nil {
		_ = f.Close()
		return fmt.Errorf("写入快照: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("刷盘快照: %w", err)
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("替换快照: %w", err)
	}
	// 目录项变更也需要落盘，失败不致命（最坏退化为重放整段日志）。
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// LoadSnapshot 读取快照并反序列化到 state；无快照文件时 ok=false。
func LoadSnapshot(dir string, state any) (offset int64, ok bool, err error) {
	final := filepath.Join(dir, "snapshot.json")
	raw, err := os.ReadFile(final)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("读取快照: %w", err)
	}
	var sf snapshotFile
	if err := json.Unmarshal(raw, &sf); err != nil {
		return 0, false, fmt.Errorf("解析快照: %w", err)
	}
	if err := json.Unmarshal(sf.State, state); err != nil {
		return 0, false, fmt.Errorf("还原快照状态: %w", err)
	}
	return sf.Offset, true, nil
}
