package shadow

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// persisted 是落盘的完整状态。
type persisted struct {
	Version int                     `json:"version"`
	Devices map[string]*deviceState `json:"devices"`
}

// deviceState 是单台设备的落盘状态。
type deviceState struct {
	DeviceID       string               `json:"deviceId"`
	Version        string               `json:"version"`
	Online         bool                 `json:"online"`
	Desired        json.RawMessage      `json:"desired"`
	Reported       json.RawMessage      `json:"reported"`
	Revision       int64                `json:"revision"`
	LastSeq        int64                `json:"lastSeq"`
	LastReportTime time.Time            `json:"lastReportTime"`
	HasReport      bool                 `json:"hasReport"`
	Diffs          map[string]time.Time `json:"diffs"`
	Audit          []*AuditRecord       `json:"audit"`
}

func newPersisted() *persisted {
	return &persisted{Version: 1, Devices: map[string]*deviceState{}}
}

// saveLocked 把完整状态原子写入 s.path。
// 调用方必须持有 s.mu。
func (s *Shadow) saveLocked() error {
	data, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".shadow-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		tmp.Close()
		os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// restore 用快照恢复内存状态，用于保存失败后的回滚。
func (s *Shadow) restore(snap []byte) {
	var p persisted
	if err := json.Unmarshal(snap, &p); err == nil {
		s.data = &p
	}
}

// validatePersisted 校验从磁盘读入的状态结构是否完整。
func validatePersisted(p *persisted) error {
	if p.Version != 1 {
		return fmt.Errorf("未知的存储版本 %d", p.Version)
	}
	if p.Devices == nil {
		return fmt.Errorf("devices 字段缺失")
	}
	for id, dev := range p.Devices {
		if dev == nil {
			return fmt.Errorf("设备 %q 状态为空", id)
		}
		if dev.DeviceID != id {
			return fmt.Errorf("设备标识不一致: key=%q, deviceId=%q", id, dev.DeviceID)
		}
		if dev.Desired == nil {
			dev.Desired = json.RawMessage("{}")
		}
		if dev.Reported == nil {
			dev.Reported = json.RawMessage("{}")
		}
		if err := validateObjectConfig(dev.Desired); err != nil {
			return fmt.Errorf("设备 %q 的期望配置无效: %v", id, err)
		}
		if err := validateObjectConfig(dev.Reported); err != nil {
			return fmt.Errorf("设备 %q 的上报配置无效: %v", id, err)
		}
		if dev.Diffs == nil {
			dev.Diffs = map[string]time.Time{}
		}
		if dev.Audit == nil {
			dev.Audit = []*AuditRecord{}
		}
	}
	return nil
}
