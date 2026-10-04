package shadow

import (
	"encoding/json"
	"fmt"
	"time"
)

// BatchDevice 是批量修改期望配置中单台设备的请求项。
type BatchDevice struct {
	DeviceID string
	// Revision 是调用方已读取的期望修订号，必须等于设备当前修订号。
	Revision uint64
	// Config 是完整替换期望配置的 JSON 对象。
	Config json.RawMessage
}

// BatchRecord 是一次成功提交的批量修改记录。
type BatchRecord struct {
	RequestID string
	Operator  string
	Time      time.Time
	// Revisions 是各设备本次修改产生的新修订号，按设备标识索引。
	Revisions map[string]uint64
}

// batchRequestState 是已成功批量请求的内部记录，用于去重与查询。
type batchRequestState struct {
	ID       string
	Operator string
	Time     time.Time
	// Devices 按首次提交的次序保存。
	Devices []batchDeviceState
}

type batchDeviceState struct {
	DeviceID string
	// Revision 是提交时已读取的修订号。
	Revision uint64
	// NewRevision 是本次修改产生的新修订号。
	NewRevision uint64
	Config      json.RawMessage
}

// BatchUpdateDesired 用一次请求同时修改多台已登记设备的期望配置，
// 每台设备完整替换旧配置、修订号各加一并留下带同一请求标识的审计。
//
// 请求标识在当前存储内唯一：相同标识再次提交相同内容（设备次序、JSON
// 空白与对象字段顺序无关，数字按数值比较，时间按同一时刻判断）直接返回
// 首次结果，不重复修改；相同标识提交不同内容（更换、增加或减少设备，无
// 论涉及的设备当前是否登记）返回 ErrRequestConflict，原结果保持有效。
// 标识首次使用时，任一设备校验失败（列表为空、标识为空或重复、设备未
// 登记、配置非法、修订号冲突）整批报错，所有设备状态、审计均不变，也
// 不占用请求标识；标识已有成功记录时仍先做请求自身的格式校验（必填信
// 息、重复设备、配置格式），非法输入按对应错误拒绝且不影响原记录。离
// 线设备同样接受修改。
func (s *Store) BatchUpdateDesired(requestID, operator string, at time.Time, devices []BatchDevice) (BatchRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return BatchRecord{}, err
	}
	if requestID == "" {
		return BatchRecord{}, ErrInvalidRequestID
	}
	if operator == "" {
		return BatchRecord{}, ErrInvalidOperator
	}
	if at.IsZero() {
		return BatchRecord{}, ErrInvalidTime
	}
	if len(devices) == 0 {
		return BatchRecord{}, ErrInvalidDeviceList
	}
	// 先校验请求自身的格式并复制全部输入：设备标识非空且不重复、配置都
	// 是合法的完整 JSON 对象。这些检查与设备是否登记无关，因此对已有成
	// 功记录的重发同样执行，非法输入按对应错误拒绝，且不影响原记录。
	type prepared struct {
		dev *deviceState
		id  string
		rev uint64
		cfg json.RawMessage
	}
	items := make([]prepared, 0, len(devices))
	seen := make(map[string]bool, len(devices))
	for _, bd := range devices {
		if bd.DeviceID == "" {
			return BatchRecord{}, ErrInvalidDeviceID
		}
		if seen[bd.DeviceID] {
			return BatchRecord{}, fmt.Errorf("%w: %s", ErrDuplicateDevice, bd.DeviceID)
		}
		seen[bd.DeviceID] = true
		cfg, err := validateConfig(bd.Config)
		if err != nil {
			return BatchRecord{}, err
		}
		items = append(items, prepared{id: bd.DeviceID, rev: bd.Revision, cfg: cfg})
	}
	// 已成功的请求标识：内容一致直接返回首次结果（即使设备此后已有新
	// 修改，也不用旧请求覆盖）；内容不同（含更换或增删设备，无论涉及
	// 的设备当前是否登记）返回可区分的冲突错误。此判断不要求引用设备
	// 仍然登记，因此必须在设备登记与修订号校验之前完成。
	if rec, ok := s.batches[requestID]; ok {
		if rec.matches(operator, at, devices) {
			return rec.record(), nil
		}
		return BatchRecord{}, fmt.Errorf("%w: %s", ErrRequestConflict, requestID)
	}
	// 尚未成功使用过的标识执行整批校验：逐项关联已登记设备，任何一步
	// 失败都不改变状态、也不留下成功请求记录。
	for i := range items {
		d, ok := s.devices[items[i].id]
		if !ok {
			return BatchRecord{}, fmt.Errorf("%w: %s", ErrDeviceNotFound, items[i].id)
		}
		items[i].dev = d
	}
	// 修订号冲突检查在任何修改之前完成，失败整批不变。
	for _, it := range items {
		if it.rev != it.dev.Revision {
			return BatchRecord{}, fmt.Errorf("%w: device %s want %d, got %d",
				ErrRevisionConflict, it.id, it.dev.Revision, it.rev)
		}
	}
	rec := &batchRequestState{ID: requestID, Operator: operator, Time: at}
	err := s.commit(func() error {
		for _, it := range items {
			d := it.dev
			before := d.Desired
			d.Desired = it.cfg
			d.Revision++
			d.Audit = append(d.Audit, AuditRecord{
				DeviceID:  it.id,
				Operator:  operator,
				Time:      at,
				Revision:  d.Revision,
				RequestID: requestID,
				Before:    cloneRaw(before),
				After:     cloneRaw(it.cfg),
			})
			d.refreshDiff(at)
			rec.Devices = append(rec.Devices, batchDeviceState{
				DeviceID:    it.id,
				Revision:    it.rev,
				NewRevision: d.Revision,
				Config:      cloneRaw(it.cfg),
			})
		}
		s.batches[requestID] = rec
		return nil
	})
	if err != nil {
		return BatchRecord{}, err
	}
	return rec.record(), nil
}

// GetBatchRequest 按请求标识查询成功提交的批量修改记录。
// 标识为空返回 ErrInvalidRequestID；未知标识（含从未成功提交的请求）
// 返回 ErrRequestNotFound。返回的数据是独立副本。
func (s *Store) GetBatchRequest(requestID string) (BatchRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return BatchRecord{}, err
	}
	if requestID == "" {
		return BatchRecord{}, ErrInvalidRequestID
	}
	rec, ok := s.batches[requestID]
	if !ok {
		return BatchRecord{}, fmt.Errorf("%w: %s", ErrRequestNotFound, requestID)
	}
	return rec.record(), nil
}

// matches 判断重新提交的请求内容是否与首次成功提交一致。
// 设备次序无关；配置按语义比较（忽略空白与对象字段顺序，数字按数值，
// 数组有序，字段缺失与 null 不同）；时间按同一时刻判断。
func (r *batchRequestState) matches(operator string, at time.Time, devices []BatchDevice) bool {
	if r.Operator != operator || !r.Time.Equal(at) || len(r.Devices) != len(devices) {
		return false
	}
	byID := make(map[string]batchDeviceState, len(r.Devices))
	for _, d := range r.Devices {
		byID[d.DeviceID] = d
	}
	for _, bd := range devices {
		d, ok := byID[bd.DeviceID]
		if !ok || d.Revision != bd.Revision || !rawEqual(d.Config, bd.Config) {
			return false
		}
	}
	return true
}

// record 生成批量记录的独立副本。
func (r *batchRequestState) record() BatchRecord {
	revisions := make(map[string]uint64, len(r.Devices))
	for _, d := range r.Devices {
		revisions[d.DeviceID] = d.NewRevision
	}
	return BatchRecord{
		RequestID: r.ID,
		Operator:  r.Operator,
		Time:      r.Time,
		Revisions: revisions,
	}
}
