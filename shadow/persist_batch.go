package shadow

import (
	"encoding/json"
	"fmt"
	"time"
)

// diskBatch 是批量请求记录的磁盘格式。
type diskBatch struct {
	ID       string         `json:"id"`
	Operator string         `json:"operator"`
	Time     time.Time      `json:"time"`
	Devices  []diskBatchDev `json:"devices"`
}

type diskBatchDev struct {
	DeviceID    string          `json:"deviceId"`
	Revision    uint64          `json:"revision"`
	NewRevision uint64          `json:"newRevision"`
	Config      json.RawMessage `json:"config"`
}

func (s *Store) marshalBatches() map[string]diskBatch {
	out := make(map[string]diskBatch, len(s.batches))
	for id, b := range s.batches {
		db := diskBatch{
			ID:       b.ID,
			Operator: b.Operator,
			Time:     b.Time,
			Devices:  make([]diskBatchDev, 0, len(b.Devices)),
		}
		for _, d := range b.Devices {
			db.Devices = append(db.Devices, diskBatchDev{
				DeviceID:    d.DeviceID,
				Revision:    d.Revision,
				NewRevision: d.NewRevision,
				Config:      cloneRaw(d.Config),
			})
		}
		out[id] = db
	}
	return out
}

// restoreBatches 恢复批量请求记录并做双向一致性校验。
//
// 正向：每条成功请求中的每台设备，其审计里必须恰好存在一条对应记录——
// 请求标识与本次新修订号一致、操作者相同、时间为同一时刻、修改后配置与
// 请求提交的完整配置语义相等（忽略 JSON 空白与对象字段顺序，数字按数值
// 比较，数组有序，字段缺失与 null 不同）。
// 反向：审计中凡带批量请求标识的记录，必须对应那个请求实际包含的设备及
// 其新修订号，不能仅因请求存在就接受。
// 任何不一致都返回 ErrCorruptStorage 拒绝打开整个存储，不自动修正、
// 不丢弃记录。旧存储没有批量记录时 disk 为 nil，正常打开。
func (s *Store) restoreBatches(disk map[string]diskBatch) error {
	out := make(map[string]*batchRequestState, len(disk))
	// expected 按请求标识记录每台设备实际参与修改时的新修订号，供反向核对。
	expected := make(map[string]map[string]uint64, len(disk))
	for id, db := range disk {
		if id == "" || db.ID != id {
			return fmt.Errorf("%w: batch request id mismatch", ErrCorruptStorage)
		}
		if db.Operator == "" || db.Time.IsZero() {
			return fmt.Errorf("%w: batch request %s invalid", ErrCorruptStorage, id)
		}
		if len(db.Devices) == 0 {
			return fmt.Errorf("%w: batch request %s has no devices", ErrCorruptStorage, id)
		}
		rec := &batchRequestState{ID: id, Operator: db.Operator, Time: db.Time}
		seen := map[string]bool{}
		devRevs := make(map[string]uint64, len(db.Devices))
		for _, dd := range db.Devices {
			if dd.DeviceID == "" || seen[dd.DeviceID] {
				return fmt.Errorf("%w: batch request %s device list invalid", ErrCorruptStorage, id)
			}
			seen[dd.DeviceID] = true
			dev, ok := s.devices[dd.DeviceID]
			if !ok {
				return fmt.Errorf("%w: batch request %s device %s missing", ErrCorruptStorage, id, dd.DeviceID)
			}
			if _, ok := decodeObject(dd.Config); !ok {
				return fmt.Errorf("%w: batch request %s device %s config invalid", ErrCorruptStorage, id, dd.DeviceID)
			}
			if dd.NewRevision != dd.Revision+1 || dev.Revision < dd.NewRevision {
				return fmt.Errorf("%w: batch request %s device %s revision mismatch", ErrCorruptStorage, id, dd.DeviceID)
			}
			// 设备审计中必须恰好存在一条与该请求项描述同一次修改的记录。
			matches := 0
			for _, a := range dev.Audit {
				if a.RequestID != id || a.Revision != dd.NewRevision {
					continue
				}
				matches++
				if a.Operator != db.Operator || !a.Time.Equal(db.Time) || !rawEqual(a.After, dd.Config) {
					return fmt.Errorf("%w: batch request %s device %s audit content mismatch",
						ErrCorruptStorage, id, dd.DeviceID)
				}
			}
			if matches != 1 {
				return fmt.Errorf("%w: batch request %s device %s audit missing",
					ErrCorruptStorage, id, dd.DeviceID)
			}
			devRevs[dd.DeviceID] = dd.NewRevision
			rec.Devices = append(rec.Devices, batchDeviceState{
				DeviceID:    dd.DeviceID,
				Revision:    dd.Revision,
				NewRevision: dd.NewRevision,
				Config:      cloneRaw(dd.Config),
			})
		}
		expected[id] = devRevs
		out[id] = rec
	}
	// 反向核对：审计中的非空请求标识必须对应一条实际包含该设备、且新修订号
	// 一致的批量记录（单设备修改的审计标识为空，无需补填）。
	for devID, dev := range s.devices {
		for _, a := range dev.Audit {
			if a.RequestID == "" {
				continue
			}
			devs, ok := expected[a.RequestID]
			if !ok {
				return fmt.Errorf("%w: device %s audit references unknown batch request %s",
					ErrCorruptStorage, devID, a.RequestID)
			}
			if rev, ok := devs[devID]; !ok || rev != a.Revision {
				return fmt.Errorf("%w: device %s audit misattached to batch request %s",
					ErrCorruptStorage, devID, a.RequestID)
			}
		}
	}
	s.batches = out
	return nil
}
