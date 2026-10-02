package shadow

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const storeFileName = "shadow-store.json"
const storeFormat = 2

// View 是设备影子当前状态的只读快照。
type View struct {
	DeviceID string
	// Version 是设备当前版本（登记时的初始版本，随有效上报更新）。
	Version string
	// Online 表示设备是否在线。新登记设备离线，有效上报置为在线。
	Online bool
	// Revision 是当前期望配置的修订号，从零开始，每次成功修改加一。
	Revision uint64
	// Desired 是期望配置的完整 JSON 对象。
	Desired json.RawMessage
	// Reported 是上报配置的完整 JSON 对象。
	Reported json.RawMessage
	// LastSeq 是最后接受的上报序号。
	LastSeq uint64
}

// AuditRecord 是一次成功修改期望配置的审计记录。
type AuditRecord struct {
	DeviceID string
	Operator string
	Time     time.Time
	// Revision 是本次修改产生的新修订号。
	Revision uint64
	// Before 是修改前的完整期望配置。
	Before json.RawMessage
	// After 是修改后的完整期望配置。
	After json.RawMessage
}

// deviceState 是单台设备的内部状态。
type deviceState struct {
	Version        string
	Online         bool
	Revision       uint64
	Desired        json.RawMessage
	Reported       json.RawMessage
	LastSeq        uint64
	LastReportTime time.Time
	// DiffSince 记录每条差异路径首次出现的时间。
	DiffSince map[string]time.Time
	Audit     []AuditRecord
}

// refreshDiff 重新计算差异路径：已有路径保留原时间，新路径使用 t，消失的路径移除。
func (d *deviceState) refreshDiff(t time.Time) {
	entries := computeDiff(d.Desired, d.Reported)
	next := make(map[string]time.Time, len(entries))
	for _, e := range entries {
		if since, ok := d.DiffSince[e.Path]; ok {
			next[e.Path] = since
		} else {
			next[e.Path] = t
		}
	}
	d.DiffSince = next
}

// Store 是本地设备影子存储，所有状态与审计记录保存在指定目录。
// Store 可安全并发使用。
type Store struct {
	mu         sync.Mutex
	dir        string
	closed     bool
	devices    map[string]*deviceState
	upgrades   map[string]*upgradeRecord
	activities map[string]*activity
}

// Open 打开（必要时创建）位于 dir 的本地影子存储。
// 已有内容损坏或格式不识别时返回 ErrCorruptStorage，不会按空数据覆盖。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("shadow: create store dir: %w", err)
	}
	s := &Store{
		dir:        dir,
		devices:    map[string]*deviceState{},
		upgrades:   map[string]*upgradeRecord{},
		activities: map[string]*activity{},
	}
	data, err := os.ReadFile(s.filePath())
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("shadow: read store: %w", err)
	}
	if err := s.restore(data); err != nil {
		return nil, err
	}
	return s, nil
}

// Close 关闭存储。关闭后的操作返回 ErrClosed。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	s.closed = true
	return nil
}

// Register 登记一台模拟设备。设备标识和初始版本不能为空；
// 重复登记返回 ErrDeviceExists。新设备初始离线，双方配置均为空对象。
func (s *Store) Register(deviceID, version string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return err
	}
	if deviceID == "" {
		return ErrInvalidDeviceID
	}
	if version == "" {
		return ErrInvalidVersion
	}
	if _, ok := s.devices[deviceID]; ok {
		return fmt.Errorf("%w: %s", ErrDeviceExists, deviceID)
	}
	return s.commit(func() error {
		s.devices[deviceID] = &deviceState{
			Version:   version,
			Desired:   json.RawMessage(`{}`),
			Reported:  json.RawMessage(`{}`),
			DiffSince: map[string]time.Time{},
		}
		return nil
	})
}

// UpdateDesired 修改设备的期望配置，完整替换旧配置。
// 需携带操作者、操作时间和已读取的期望修订号；修订号不等于当前值时
// 返回 ErrRevisionConflict，影子和审计均不改变。成功时返回新修订号。
func (s *Store) UpdateDesired(deviceID, operator string, at time.Time, revision uint64, config json.RawMessage) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return 0, err
	}
	d, err := s.lookup(deviceID)
	if err != nil {
		return 0, err
	}
	if operator == "" {
		return 0, ErrInvalidOperator
	}
	if at.IsZero() {
		return 0, ErrInvalidTime
	}
	cfg, err := validateConfig(config)
	if err != nil {
		return 0, err
	}
	if revision != d.Revision {
		return 0, fmt.Errorf("%w: want %d, got %d", ErrRevisionConflict, d.Revision, revision)
	}
	var newRevision uint64
	err = s.commit(func() error {
		before := d.Desired
		d.Desired = cfg
		d.Revision++
		newRevision = d.Revision
		d.Audit = append(d.Audit, AuditRecord{
			DeviceID: deviceID,
			Operator: operator,
			Time:     at,
			Revision: newRevision,
			Before:   cloneRaw(before),
			After:    cloneRaw(cfg),
		})
		d.refreshDiff(at)
		return nil
	})
	if err != nil {
		return 0, err
	}
	return newRevision, nil
}

// Report 处理设备上报。需携带正整数序号、时间、非空当前版本和完整配置。
// 更大序号更新上报并标为在线；相同序号且版本、配置、时间相同视为重复，
// 成功返回但不改变状态；相同序号内容不同或更小序号返回错误。
func (s *Store) Report(deviceID string, seq uint64, at time.Time, version string, config json.RawMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return err
	}
	d, err := s.lookup(deviceID)
	if err != nil {
		return err
	}
	if seq == 0 {
		return ErrInvalidSequence
	}
	if at.IsZero() {
		return ErrInvalidTime
	}
	if version == "" {
		return ErrInvalidVersion
	}
	cfg, err := validateConfig(config)
	if err != nil {
		return err
	}
	return s.commit(func() error {
		return applyReport(d, seq, at, version, cfg)
	})
}

// applyReport 把已校验的上报应用到 d。调用方持有 s.mu 并负责持久化。
// 相同序号、内容一致视为重复，成功返回但不改变状态；
// 更小序号返回 ErrStaleSequence；相同序号内容不同返回 ErrReportConflict。
func applyReport(d *deviceState, seq uint64, at time.Time, version string, cfg json.RawMessage) error {
	switch {
	case seq < d.LastSeq:
		return fmt.Errorf("%w: last accepted %d, got %d", ErrStaleSequence, d.LastSeq, seq)
	case seq == d.LastSeq:
		if version == d.Version && at.Equal(d.LastReportTime) && rawEqual(cfg, d.Reported) {
			return nil // 重复上报：成功返回，不改变状态
		}
		return fmt.Errorf("%w: sequence %d", ErrReportConflict, seq)
	}
	d.Reported = cfg
	d.Version = version
	d.Online = true
	d.LastSeq = seq
	d.LastReportTime = at
	d.refreshDiff(at)
	return nil
}

// SetOffline 将设备标为离线。只改变在线状态，保留版本及双方配置。
func (s *Store) SetOffline(deviceID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return err
	}
	d, err := s.lookup(deviceID)
	if err != nil {
		return err
	}
	return s.commit(func() error {
		d.Online = false
		return nil
	})
}

// Get 返回设备影子当前状态的快照。返回的数据是独立副本，
// 调用方修改不影响已保存的记录。
func (s *Store) Get(deviceID string) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return View{}, err
	}
	d, err := s.lookup(deviceID)
	if err != nil {
		return View{}, err
	}
	return View{
		DeviceID: deviceID,
		Version:  d.Version,
		Online:   d.Online,
		Revision: d.Revision,
		Desired:  cloneRaw(d.Desired),
		Reported: cloneRaw(d.Reported),
		LastSeq:  d.LastSeq,
	}, nil
}

// Diff 返回期望配置与上报配置之间的差异，按 JSON Pointer 路径字典序排列。
func (s *Store) Diff(deviceID string) ([]DiffEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	d, err := s.lookup(deviceID)
	if err != nil {
		return nil, err
	}
	entries := computeDiff(d.Desired, d.Reported)
	for i := range entries {
		entries[i].Since = d.DiffSince[entries[i].Path]
	}
	return entries, nil
}

// Audit 按修订号递增返回设备的审计记录。返回的数据是独立副本。
func (s *Store) Audit(deviceID string) ([]AuditRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	d, err := s.lookup(deviceID)
	if err != nil {
		return nil, err
	}
	out := make([]AuditRecord, len(d.Audit))
	for i, rec := range d.Audit {
		out[i] = rec
		out[i].Before = cloneRaw(rec.Before)
		out[i].After = cloneRaw(rec.After)
	}
	return out, nil
}

func (s *Store) checkOpen() error {
	if s.closed {
		return ErrClosed
	}
	return nil
}

func (s *Store) lookup(deviceID string) (*deviceState, error) {
	if deviceID == "" {
		return nil, ErrInvalidDeviceID
	}
	d, ok := s.devices[deviceID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrDeviceNotFound, deviceID)
	}
	return d, nil
}

// commit 执行一次状态修改并持久化。保存失败时回滚内存状态并返回错误，
// 保证一次操作涉及的状态与审计一起保存、查询仍显示操作前的结果。
// 调用时必须持有 s.mu。
func (s *Store) commit(mutate func() error) error {
	snapshot, err := s.marshal()
	if err != nil {
		return err
	}
	if err := mutate(); err != nil {
		return err
	}
	if err := s.persist(); err != nil {
		// 回滚内存状态；snapshot 由本进程序列化，恢复不应失败。
		_ = s.restore(snapshot)
		return err
	}
	return nil
}

func (s *Store) filePath() string {
	return filepath.Join(s.dir, storeFileName)
}

// persist 把当前状态原子写入存储文件。
func (s *Store) persist() error {
	data, err := s.marshal()
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".shadow-store-*.tmp")
	if err != nil {
		return fmt.Errorf("shadow: save store: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("shadow: save store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("shadow: save store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("shadow: save store: %w", err)
	}
	if err := os.Rename(tmpName, s.filePath()); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("shadow: save store: %w", err)
	}
	return nil
}

// diskStore 是存储文件的磁盘格式。
type diskStore struct {
	Format     int                      `json:"format"`
	Devices    map[string]diskState     `json:"devices"`
	Upgrades   map[string][]string      `json:"upgrades,omitempty"`
	Activities map[string]*diskActivity `json:"activities,omitempty"`
}

type diskState struct {
	Version        string               `json:"version"`
	Online         bool                 `json:"online"`
	Revision       uint64               `json:"revision"`
	Desired        json.RawMessage      `json:"desired"`
	Reported       json.RawMessage      `json:"reported"`
	LastSeq        uint64               `json:"lastSeq"`
	LastReportTime time.Time            `json:"lastReportTime,omitempty"`
	DiffSince      map[string]time.Time `json:"diffSince,omitempty"`
	Audit          []diskAudit          `json:"audit,omitempty"`
}

type diskAudit struct {
	Operator string          `json:"operator"`
	Time     time.Time       `json:"time"`
	Revision uint64          `json:"revision"`
	Before   json.RawMessage `json:"before"`
	After    json.RawMessage `json:"after"`
}

type diskActivity struct {
	ID          string                `json:"id"`
	Operator    string                `json:"operator"`
	CreatedAt   time.Time             `json:"createdAt"`
	Target      string                `json:"target"`
	WindowStart time.Time             `json:"windowStart"`
	WindowEnd   time.Time             `json:"windowEnd"`
	Deadline    time.Time             `json:"deadline"`
	BatchSize   int                   `json:"batchSize"`
	LastTime    time.Time             `json:"lastTime,omitempty"`
	Status      string                `json:"status"`
	Devices     []*diskActivityDevice `json:"devices"`
	History     []*diskHistoryEntry   `json:"history,omitempty"`
}

type diskActivityDevice struct {
	DeviceID   string    `json:"deviceId"`
	Batch      int       `json:"batch"`
	Status     string    `json:"status"`
	OpID       string    `json:"opId,omitempty"`
	FailStage  string    `json:"failStage,omitempty"`
	FailReason string    `json:"failReason,omitempty"`
	FailTime   time.Time `json:"failTime,omitempty"`
}

type diskHistoryEntry struct {
	DeviceID string    `json:"deviceId"`
	OpID     string    `json:"opId"`
	Stage    string    `json:"stage"`
	Success  bool      `json:"success"`
	Reason   string    `json:"reason,omitempty"`
	Seq      uint64    `json:"seq,omitempty"`
	Time     time.Time `json:"time"`
}

func (s *Store) marshal() ([]byte, error) {
	disk := diskStore{Format: storeFormat, Devices: make(map[string]diskState, len(s.devices))}
	for id, d := range s.devices {
		ds := diskState{
			Version:        d.Version,
			Online:         d.Online,
			Revision:       d.Revision,
			Desired:        cloneRaw(d.Desired),
			Reported:       cloneRaw(d.Reported),
			LastSeq:        d.LastSeq,
			LastReportTime: d.LastReportTime,
			DiffSince:      make(map[string]time.Time, len(d.DiffSince)),
		}
		for p, t := range d.DiffSince {
			ds.DiffSince[p] = t
		}
		for _, rec := range d.Audit {
			ds.Audit = append(ds.Audit, diskAudit{
				Operator: rec.Operator,
				Time:     rec.Time,
				Revision: rec.Revision,
				Before:   cloneRaw(rec.Before),
				After:    cloneRaw(rec.After),
			})
		}
		disk.Devices[id] = ds
	}
	disk.Upgrades = make(map[string][]string, len(s.upgrades))
	for target, rec := range s.upgrades {
		disk.Upgrades[target] = append([]string(nil), rec.allowed...)
	}
	disk.Activities = make(map[string]*diskActivity, len(s.activities))
	for id, a := range s.activities {
		da := &diskActivity{
			ID:          a.id,
			Operator:    a.operator,
			CreatedAt:   a.createdAt,
			Target:      a.target,
			WindowStart: a.windowStart,
			WindowEnd:   a.windowEnd,
			Deadline:    a.deadline,
			BatchSize:   a.batchSize,
			LastTime:    a.lastTime,
			Status:      a.status,
			Devices:     make([]*diskActivityDevice, 0, len(a.devices)),
			History:     make([]*diskHistoryEntry, 0, len(a.history)),
		}
		for _, d := range a.devices {
			da.Devices = append(da.Devices, &diskActivityDevice{
				DeviceID:   d.deviceID,
				Batch:      d.batch,
				Status:     d.status,
				OpID:       d.opID,
				FailStage:  d.failStage,
				FailReason: d.failReason,
				FailTime:   d.failTime,
			})
		}
		for _, h := range a.history {
			da.History = append(da.History, &diskHistoryEntry{
				DeviceID: h.deviceID,
				OpID:     h.opID,
				Stage:    h.stage,
				Success:  h.success,
				Reason:   h.reason,
				Seq:      h.seq,
				Time:     h.time,
			})
		}
		disk.Activities[id] = da
	}
	data, err := json.Marshal(disk)
	if err != nil {
		return nil, fmt.Errorf("shadow: encode store: %w", err)
	}
	return data, nil
}

// restore 从存储文件内容恢复状态。内容损坏或格式不识别时返回
// ErrCorruptStorage，绝不按空数据覆盖。
func (s *Store) restore(data []byte) error {
	var disk diskStore
	if err := json.Unmarshal(data, &disk); err != nil {
		return fmt.Errorf("%w: %v", ErrCorruptStorage, err)
	}
	if disk.Format != 1 && disk.Format != storeFormat {
		return fmt.Errorf("%w: unsupported format %d", ErrCorruptStorage, disk.Format)
	}
	devices := make(map[string]*deviceState, len(disk.Devices))
	for id, ds := range disk.Devices {
		if id == "" {
			return fmt.Errorf("%w: empty device id", ErrCorruptStorage)
		}
		if _, ok := decodeObject(ds.Desired); !ok {
			return fmt.Errorf("%w: device %s desired config invalid", ErrCorruptStorage, id)
		}
		if _, ok := decodeObject(ds.Reported); !ok {
			return fmt.Errorf("%w: device %s reported config invalid", ErrCorruptStorage, id)
		}
		d := &deviceState{
			Version:        ds.Version,
			Online:         ds.Online,
			Revision:       ds.Revision,
			Desired:        cloneRaw(ds.Desired),
			Reported:       cloneRaw(ds.Reported),
			LastSeq:        ds.LastSeq,
			LastReportTime: ds.LastReportTime,
			DiffSince:      make(map[string]time.Time, len(ds.DiffSince)),
		}
		for p, t := range ds.DiffSince {
			d.DiffSince[p] = t
		}
		var lastRev uint64
		for _, da := range ds.Audit {
			if da.Operator == "" || da.Time.IsZero() || da.Revision == 0 || da.Revision <= lastRev {
				return fmt.Errorf("%w: device %s audit invalid", ErrCorruptStorage, id)
			}
			if _, ok := decodeObject(da.Before); !ok {
				return fmt.Errorf("%w: device %s audit invalid", ErrCorruptStorage, id)
			}
			if _, ok := decodeObject(da.After); !ok {
				return fmt.Errorf("%w: device %s audit invalid", ErrCorruptStorage, id)
			}
			lastRev = da.Revision
			d.Audit = append(d.Audit, AuditRecord{
				DeviceID: id,
				Operator: da.Operator,
				Time:     da.Time,
				Revision: da.Revision,
				Before:   cloneRaw(da.Before),
				After:    cloneRaw(da.After),
			})
		}
		devices[id] = d
	}
	s.devices = devices
	s.upgrades = make(map[string]*upgradeRecord, len(disk.Upgrades))
	for target, allowed := range disk.Upgrades {
		if target == "" {
			return fmt.Errorf("%w: empty upgrade target", ErrCorruptStorage)
		}
		s.upgrades[target] = &upgradeRecord{target: target, allowed: append([]string(nil), allowed...)}
	}
	s.activities = make(map[string]*activity, len(disk.Activities))
	for id, da := range disk.Activities {
		if id == "" || da.ID != id {
			return fmt.Errorf("%w: activity id mismatch", ErrCorruptStorage)
		}
		if da.Operator == "" || da.CreatedAt.IsZero() || da.WindowStart.IsZero() || da.WindowEnd.IsZero() ||
			da.Deadline.IsZero() || da.BatchSize <= 0 {
			return fmt.Errorf("%w: activity %s invalid", ErrCorruptStorage, id)
		}
		if !da.WindowStart.Before(da.WindowEnd) || !da.Deadline.After(da.CreatedAt) {
			return fmt.Errorf("%w: activity %s invalid window/deadline", ErrCorruptStorage, id)
		}
		if _, ok := s.upgrades[da.Target]; !ok {
			return fmt.Errorf("%w: activity %s target not registered", ErrCorruptStorage, id)
		}
		a := &activity{
			id:          da.ID,
			operator:    da.Operator,
			createdAt:   da.CreatedAt,
			target:      da.Target,
			windowStart: da.WindowStart,
			windowEnd:   da.WindowEnd,
			deadline:    da.Deadline,
			batchSize:   da.BatchSize,
			lastTime:    da.LastTime,
			status:      da.Status,
		}
		if a.lastTime.IsZero() {
			a.lastTime = da.CreatedAt
		}
		switch da.Status {
		case ActivityActive, ActivitySucceeded, ActivityFailed:
		default:
			return fmt.Errorf("%w: activity %s invalid status", ErrCorruptStorage, id)
		}
		seenDev := make(map[string]bool, len(da.Devices))
		for _, dd := range da.Devices {
			if dd.DeviceID == "" || seenDev[dd.DeviceID] {
				return fmt.Errorf("%w: activity %s invalid device", ErrCorruptStorage, id)
			}
			seenDev[dd.DeviceID] = true
			if _, ok := s.devices[dd.DeviceID]; !ok {
				return fmt.Errorf("%w: activity %s device %s not registered", ErrCorruptStorage, id, dd.DeviceID)
			}
			if dd.Batch < 0 || dd.Batch >= a.numBatchesFromLen(len(da.Devices)) {
				return fmt.Errorf("%w: activity %s device %s invalid batch", ErrCorruptStorage, id, dd.DeviceID)
			}
			a.devices = append(a.devices, &activityDevice{
				deviceID:   dd.DeviceID,
				batch:      dd.Batch,
				status:     dd.Status,
				opID:       dd.OpID,
				failStage:  dd.FailStage,
				failReason: dd.FailReason,
				failTime:   dd.FailTime,
			})
		}
		if len(a.devices) == 0 {
			return fmt.Errorf("%w: activity %s has no devices", ErrCorruptStorage, id)
		}
		for _, h := range da.History {
			if h.DeviceID == "" || h.OpID == "" || h.Stage == "" || h.Time.IsZero() {
				return fmt.Errorf("%w: activity %s invalid history", ErrCorruptStorage, id)
			}
			a.history = append(a.history, &historyEntry{
				deviceID: h.DeviceID,
				opID:     h.OpID,
				stage:    h.Stage,
				success:  h.Success,
				reason:   h.Reason,
				seq:      h.Seq,
				time:     h.Time,
			})
		}
		s.activities[id] = a
	}
	return nil
}

// numBatchesFromLen 根据设备总数计算批数。
func (a *activity) numBatchesFromLen(n int) int {
	return (n + a.batchSize - 1) / a.batchSize
}
