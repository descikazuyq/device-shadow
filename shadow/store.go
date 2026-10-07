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
const storeFormat = 1

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
	// RequestID 是产生本次修改的批量请求标识；单设备修改为空。
	RequestID string
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
	mu        sync.Mutex
	dir       string
	closed    bool
	devices   map[string]*deviceState
	versions  map[string]*versionState
	campaigns map[string]*campaignState
	batches   map[string]*batchRequestState
}

// Open 打开（必要时创建）位于 dir 的本地影子存储。
// 已有内容损坏或格式不识别时返回 ErrCorruptStorage，不会按空数据覆盖。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("shadow: create store dir: %w", err)
	}
	s := &Store{
		dir:       dir,
		devices:   map[string]*deviceState{},
		versions:  map[string]*versionState{},
		campaigns: map[string]*campaignState{},
		batches:   map[string]*batchRequestState{},
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
//
// 配置校验、修订号判断、配置替换、审计与差异时间更新与批量修改共用同一
// 实现（checkDesiredMeta/checkDesiredRevision/applyDesired），仅校验次序
// 与错误消息格式保持各自既有形式。
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
	if err := checkDesiredMeta(operator, at); err != nil {
		return 0, err
	}
	cfg, err := validateConfig(config)
	if err != nil {
		return 0, err
	}
	if err := checkDesiredRevision(d, deviceID, revision); err != nil {
		return 0, desiredSingleError(err.(errDesiredRevision))
	}
	var newRevision uint64
	err = s.commit(func() error {
		newRevision = applyDesired(d, deviceID, operator, at, "", cfg)
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
//
// 校验、序号判断与影子写入规则与安装/回滚成功的附带上报共用同一实现
// （checkReport/applyReport），仅错误类别保持各自公开的哨兵不变。
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
	in := reportInput{DeviceID: deviceID, Seq: seq, At: at, Version: version, Config: config}
	cfg, verdict, err := checkReport(d, in, "")
	if err != nil {
		return reportPlainError(err.(errBadReport))
	}
	if verdict == reportDuplicate {
		// 重复上报：成功返回，不写入影子，也不触发持久化。
		return nil
	}
	return s.commit(func() error {
		applyReport(d, in, cfg, verdict, at)
		return nil
	})
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
	Format    int                     `json:"format"`
	Devices   map[string]diskState    `json:"devices"`
	Versions  map[string]diskVersion  `json:"versions,omitempty"`
	Campaigns map[string]diskCampaign `json:"campaigns,omitempty"`
	Batches   map[string]diskBatch    `json:"batches,omitempty"`
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
	Operator  string          `json:"operator"`
	Time      time.Time       `json:"time"`
	Revision  uint64          `json:"revision"`
	RequestID string          `json:"requestId,omitempty"`
	Before    json.RawMessage `json:"before"`
	After     json.RawMessage `json:"after"`
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
				Operator:  rec.Operator,
				Time:      rec.Time,
				Revision:  rec.Revision,
				RequestID: rec.RequestID,
				Before:    cloneRaw(rec.Before),
				After:     cloneRaw(rec.After),
			})
		}
		disk.Devices[id] = ds
	}
	disk.Versions = s.marshalVersions()
	campaigns, err := s.marshalCampaigns()
	if err != nil {
		return nil, err
	}
	disk.Campaigns = campaigns
	disk.Batches = s.marshalBatches()
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
	if disk.Format != storeFormat {
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
		repObj, ok := decodeObject(ds.Reported)
		if !ok {
			return fmt.Errorf("%w: device %s reported config invalid", ErrCorruptStorage, id)
		}
		// 每台设备的记录必须能表示“已经登记但从未上报”或“已有一次有效
		// 上报”：最后上报序号为 0 表示从未上报，此时上报时间必须为零值、
		// 上报配置必须是空 JSON 对象（按 JSON 内容判断，允许合法空白）、
		// 设备必须离线；序号为正整数表示已有有效上报，此时必须保存非零的
		// 上报时间。两种记录的当前版本都必须非空。任一条件不满足都按存储
		// 损坏拒绝，不自动补时间、改序号、清空上报配置或替换版本。
		if ds.Version == "" {
			return fmt.Errorf("%w: device %s version empty", ErrCorruptStorage, id)
		}
		if ds.LastSeq == 0 {
			if !ds.LastReportTime.IsZero() {
				return fmt.Errorf("%w: device %s never reported but has report time", ErrCorruptStorage, id)
			}
			if len(repObj) != 0 {
				return fmt.Errorf("%w: device %s never reported but has reported config", ErrCorruptStorage, id)
			}
			if ds.Online {
				return fmt.Errorf("%w: device %s never reported but is online", ErrCorruptStorage, id)
			}
		} else if ds.LastReportTime.IsZero() {
			return fmt.Errorf("%w: device %s reported but has no report time", ErrCorruptStorage, id)
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
		for _, da := range ds.Audit {
			if da.Operator == "" || da.Time.IsZero() || da.Revision == 0 {
				return fmt.Errorf("%w: device %s audit invalid", ErrCorruptStorage, id)
			}
			if _, ok := decodeObject(da.Before); !ok {
				return fmt.Errorf("%w: device %s audit invalid", ErrCorruptStorage, id)
			}
			if _, ok := decodeObject(da.After); !ok {
				return fmt.Errorf("%w: device %s audit invalid", ErrCorruptStorage, id)
			}
			d.Audit = append(d.Audit, AuditRecord{
				DeviceID:  id,
				Operator:  da.Operator,
				Time:      da.Time,
				Revision:  da.Revision,
				RequestID: da.RequestID,
				Before:    cloneRaw(da.Before),
				After:     cloneRaw(da.After),
			})
		}
		// 审计必须能完整解释当前期望配置：修订号从 1 开始逐条连续到当前
		// 修订号；首条修改前是登记时的空对象，每条修改后等于下一条修改前，
		// 末条修改后等于当前期望配置。比较沿用配置的 JSON 语义，不要求
		// 操作时间严格递增。修订号为 0 时审计必须为空、期望必须为空对象。
		if uint64(len(d.Audit)) != d.Revision {
			return fmt.Errorf("%w: device %s audit history does not cover revision %d",
				ErrCorruptStorage, id, d.Revision)
		}
		expected := json.RawMessage(`{}`)
		for i, rec := range d.Audit {
			if rec.Revision != uint64(i+1) {
				return fmt.Errorf("%w: device %s audit revision %d out of sequence",
					ErrCorruptStorage, id, rec.Revision)
			}
			if !rawEqual(rec.Before, expected) {
				return fmt.Errorf("%w: device %s audit revision %d broken chain",
					ErrCorruptStorage, id, rec.Revision)
			}
			expected = rec.After
		}
		if !rawEqual(expected, d.Desired) {
			return fmt.Errorf("%w: device %s desired config not explained by audit",
				ErrCorruptStorage, id)
		}
		// 差异计时记录必须与双方当前配置重新计算出的差异完全一致：
		// 每条实际差异都要有一条非零的首次出现时间，记录中也不能残留
		// 当前已经没有差异的路径（父路径不能代替子路径，路径不同即使
		// 条数相同也不行）。没有差异时允许不保存记录或保存空记录。
		entries := computeDiff(d.Desired, d.Reported)
		if len(entries) != len(d.DiffSince) {
			return fmt.Errorf("%w: device %s diff times do not match current diff paths",
				ErrCorruptStorage, id)
		}
		for _, e := range entries {
			since, ok := d.DiffSince[e.Path]
			if !ok {
				return fmt.Errorf("%w: device %s diff %s missing first-seen time",
					ErrCorruptStorage, id, e.Path)
			}
			if since.IsZero() {
				return fmt.Errorf("%w: device %s diff %s has zero first-seen time",
					ErrCorruptStorage, id, e.Path)
			}
		}
		devices[id] = d
	}
	s.devices = devices
	if err := s.restoreVersions(disk.Versions); err != nil {
		return err
	}
	if err := s.restoreCampaigns(disk.Campaigns); err != nil {
		return err
	}
	if err := s.restoreBatches(disk.Batches); err != nil {
		return err
	}
	return nil
}
