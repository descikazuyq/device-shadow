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
// 首次结果，不重复修改；相同标识提交不同内容返回 ErrRequestConflict，
// 原结果保持有效。只要请求自身格式合法（标识、操作者、时间、设备列表
// 非空且不重复、配置为完整 JSON 对象），已成功的标识就按首次提交内容
// 判断重发或冲突，更换或增加设备即使涉及未登记设备也返回冲突而非
// ErrDeviceNotFound。新标识下任一设备校验失败（列表为空、标识为空或
// 重复、设备未登记、配置非法、修订号冲突）时整批报错，所有设备状态、
// 审计均不变，也不占用请求标识。离线设备同样接受修改。
//
// 每台设备的配置校验、修订号判断、配置替换、审计与差异时间更新与单台
// 修改共用同一实现（checkDesiredMeta/checkDesiredRevision/applyDesired），
// 仅校验次序与错误消息格式保持各自既有形式。
func (s *Store) BatchUpdateDesired(requestID, operator string, at time.Time, devices []BatchDevice) (BatchRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return BatchRecord{}, err
	}
	if requestID == "" {
		return BatchRecord{}, ErrInvalidRequestID
	}
	if err := checkDesiredMeta(operator, at); err != nil {
		return BatchRecord{}, err
	}
	if len(devices) == 0 {
		return BatchRecord{}, ErrInvalidDeviceList
	}
	// 先校验并复制全部输入，再决定是否落库；任何一步失败都不改变状态。
	// 此处只检查请求自身格式（标识非空且不重复、配置为完整 JSON 对象），
	// 不查设备登记：标识已有成功记录时按首次内容判断重发或冲突，与设备
	// 当前是否登记无关。
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
	// 修改，也不用旧请求覆盖）；内容不同返回可区分的冲突错误。更换或
	// 增加设备都属于内容变化，无论涉及的设备是否已登记。
	if rec, ok := s.batches[requestID]; ok {
		if rec.matches(operator, at, devices) {
			return rec.record(), nil
		}
		return BatchRecord{}, fmt.Errorf("%w: %s", ErrRequestConflict, requestID)
	}
	// 新标识才做整批校验：任一设备未登记则整批报错，不占用请求标识。
	for i := range items {
		d, ok := s.devices[items[i].id]
		if !ok {
			return BatchRecord{}, fmt.Errorf("%w: %s", ErrDeviceNotFound, items[i].id)
		}
		items[i].dev = d
	}
	// 修订号冲突检查在任何修改之前完成，失败整批不变。
	for _, it := range items {
		if err := checkDesiredRevision(it.dev, it.id, it.rev); err != nil {
			return BatchRecord{}, desiredBatchError(err.(errDesiredRevision))
		}
	}
	rec := &batchRequestState{ID: requestID, Operator: operator, Time: at}
	err := s.commit(func() error {
		for _, it := range items {
			// 与单台修改共用同一写入规则，审计携带本批次的请求标识。
			newRevision := applyDesired(it.dev, it.id, operator, at, requestID, it.cfg)
			rec.Devices = append(rec.Devices, batchDeviceState{
				DeviceID:    it.id,
				Revision:    it.rev,
				NewRevision: newRevision,
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
