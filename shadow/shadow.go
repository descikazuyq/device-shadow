package shadow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"
)

// Shadow 是保存在本地文件中的设备影子集合。
// 所有方法都是并发安全的；查询返回的数据均为副本，调用者修改不影响已保存记录。
type Shadow struct {
	mu     sync.Mutex
	path   string
	data   *persisted
	closed bool
}

// Open 打开或创建一个保存在 path 的设备影子存储。
// path 指向一个本地文件；其内容损坏时返回错误，不会覆盖已有内容。
func Open(path string) (*Shadow, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: 路径为空", ErrInvalidArgument)
	}
	s := &Shadow{path: path}
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		// 新存储：建立空状态并立即落盘，确定存储位置。
		s.data = newPersisted()
		if err := s.saveLocked(); err != nil {
			return nil, err
		}
		return s, nil
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	if err := validatePersisted(&p); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCorrupted, err)
	}
	s.data = &p
	return s, nil
}

// Close 关闭存储。关闭后任何操作都返回 ErrClosed。
func (s *Shadow) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.data = nil
	return nil
}

// RegisterDevice 登记一台新设备。
// 设备标识和初始版本不能为空；重复登记返回 ErrDeviceExists，不产生记录。
// 新设备初始离线，期望配置和上报配置均为空对象。
func (s *Shadow) RegisterDevice(deviceID, version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if deviceID == "" {
		return fmt.Errorf("%w: 设备标识为空", ErrInvalidArgument)
	}
	if version == "" {
		return fmt.Errorf("%w: 初始版本为空", ErrInvalidArgument)
	}
	if _, ok := s.data.Devices[deviceID]; ok {
		return fmt.Errorf("%w: %q", ErrDeviceExists, deviceID)
	}
	dev := &deviceState{
		DeviceID:  deviceID,
		Version:   version,
		Online:    false,
		Desired:   json.RawMessage("{}"),
		Reported:  json.RawMessage("{}"),
		Revision:  0,
		LastSeq:   0,
		HasReport: false,
		Diffs:     map[string]time.Time{},
		Audit:     []*AuditRecord{},
	}
	s.data.Devices[deviceID] = dev
	if err := s.saveLocked(); err != nil {
		delete(s.data.Devices, deviceID)
		return err
	}
	return nil
}

// UpdateDesired 提交期望配置修改。
// revision 是调用者已读取的期望修订号，从零开始；每次成功提交（含相同配置）修订号加一。
// 提交的修订号不等于当前值时返回 ErrConflict，影子和审计均不改变。
// 成功后返回新的修订号。
func (s *Shadow) UpdateDesired(deviceID string, revision int64, operator string, t time.Time, config []byte) (int64, error) {
	if operator == "" {
		return 0, fmt.Errorf("%w: 操作者为空", ErrInvalidArgument)
	}
	if t.IsZero() {
		return 0, fmt.Errorf("%w: 操作时间缺失", ErrInvalidArgument)
	}
	if err := validateObjectConfig(json.RawMessage(config)); err != nil {
		return 0, err
	}
	var newRev int64
	err := s.mutate(deviceID, func(dev *deviceState) error {
		if revision != dev.Revision {
			return fmt.Errorf("%w: 当前修订号为 %d，收到 %d", ErrConflict, dev.Revision, revision)
		}
		before := cloneRaw(dev.Desired)
		dev.Desired = cloneRaw(json.RawMessage(config))
		dev.Revision++
		newRev = dev.Revision
		// 审计记录：设备、操作者、时间、新修订号、修改前后完整配置。
		rec := &AuditRecord{
			DeviceID: deviceID,
			Operator: operator,
			Time:     t,
			Revision: dev.Revision,
			Before:   before,
			After:    cloneRaw(dev.Desired),
		}
		dev.Audit = append(dev.Audit, rec)
		// 重新计算差异，差异时间取本次操作时间。
		entries, err := computeDiffEntries(dev.Desired, dev.Reported)
		if err != nil {
			return err
		}
		dev.Diffs = refreshDiffs(dev.Diffs, entries, t)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newRev, nil
}

// Report 提交设备上报。
// seq 为正整数序号；更大序号更新上报并标为在线；
// 相同序号且版本、配置和时间相同视为重复，成功返回但不改变状态；
// 相同序号内容不同或更小序号返回错误。
func (s *Shadow) Report(deviceID string, seq int64, t time.Time, version string, config []byte) error {
	if seq <= 0 {
		return fmt.Errorf("%w: 序号必须为正整数", ErrInvalidArgument)
	}
	if t.IsZero() {
		return fmt.Errorf("%w: 时间缺失", ErrInvalidArgument)
	}
	if version == "" {
		return fmt.Errorf("%w: 版本为空", ErrInvalidArgument)
	}
	if err := validateObjectConfig(json.RawMessage(config)); err != nil {
		return err
	}
	return s.mutate(deviceID, func(dev *deviceState) error {
		if dev.HasReport {
			if seq < dev.LastSeq {
				return fmt.Errorf("%w: 序号 %d 小于已接受序号 %d", ErrInvalidArgument, seq, dev.LastSeq)
			}
			if seq == dev.LastSeq {
				if version == dev.Version && t.Equal(dev.LastReportTime) && configsEqual(json.RawMessage(config), dev.Reported) {
					return nil // 重复上报：成功返回，不改变状态，不刷新差异时间。
				}
				return fmt.Errorf("%w: 序号 %d 已用于不同内容", ErrInvalidArgument, seq)
			}
		}
		// 接受新序号上报。
		dev.HasReport = true
		dev.LastSeq = seq
		dev.LastReportTime = t
		dev.Version = version
		dev.Reported = cloneRaw(json.RawMessage(config))
		dev.Online = true
		entries, err := computeDiffEntries(dev.Desired, dev.Reported)
		if err != nil {
			return err
		}
		dev.Diffs = refreshDiffs(dev.Diffs, entries, t)
		return nil
	})
}

// Offline 将设备置为离线。只改变在线状态，保留版本及双方配置，不刷新差异时间。
func (s *Shadow) Offline(deviceID string) error {
	return s.mutate(deviceID, func(dev *deviceState) error {
		dev.Online = false
		return nil
	})
}

// Device 返回设备当前状态的副本。
func (s *Shadow) Device(deviceID string) (Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Device{}, ErrClosed
	}
	dev, ok := s.data.Devices[deviceID]
	if !ok {
		return Device{}, fmt.Errorf("%w: %q", ErrDeviceNotFound, deviceID)
	}
	return Device{
		DeviceID: dev.DeviceID,
		Version:  dev.Version,
		Online:   dev.Online,
		Revision: dev.Revision,
		Desired:  cloneRaw(dev.Desired),
		Reported: cloneRaw(dev.Reported),
	}, nil
}

// Diffs 返回期望配置与上报配置的差异，按 JSON Pointer 路径字典序排列。
func (s *Shadow) Diffs(deviceID string) ([]Diff, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	dev, ok := s.data.Devices[deviceID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrDeviceNotFound, deviceID)
	}
	entries, err := computeDiffEntries(dev.Desired, dev.Reported)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	out := make([]Diff, 0, len(entries))
	for _, e := range entries {
		since, ok := dev.Diffs[e.path]
		if !ok {
			since = time.Time{}
		}
		d := Diff{Path: e.path, Since: since}
		if e.dExists {
			d.Desired = DiffValue{Exists: true, Value: marshalValue(e.dValue)}
		}
		if e.rExists {
			d.Reported = DiffValue{Exists: true, Value: marshalValue(e.rValue)}
		}
		out = append(out, d)
	}
	return out, nil
}

// Audit 返回设备的审计记录，按修订号递增排列。
func (s *Shadow) Audit(deviceID string) ([]AuditRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	dev, ok := s.data.Devices[deviceID]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrDeviceNotFound, deviceID)
	}
	out := make([]AuditRecord, len(dev.Audit))
	for i, rec := range dev.Audit {
		out[i] = AuditRecord{
			DeviceID: rec.DeviceID,
			Operator: rec.Operator,
			Time:     rec.Time,
			Revision: rec.Revision,
			Before:   cloneRaw(rec.Before),
			After:    cloneRaw(rec.After),
		}
	}
	return out, nil
}

// mutate 对设备执行一次修改，并把状态与审计一起保存。
// fn 返回错误或保存失败时回滚内存状态，查询仍显示操作前的结果。
func (s *Shadow) mutate(deviceID string, fn func(*deviceState) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	snap, err := json.Marshal(s.data)
	if err != nil {
		return err
	}
	dev, ok := s.data.Devices[deviceID]
	if !ok {
		return fmt.Errorf("%w: %q", ErrDeviceNotFound, deviceID)
	}
	if err := fn(dev); err != nil {
		s.restore(snap)
		return err
	}
	if err := s.saveLocked(); err != nil {
		s.restore(snap)
		return err
	}
	return nil
}
