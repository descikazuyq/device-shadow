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

// restoreBatches 恢复批量请求记录并做一致性校验：标识、操作者、时间、
// 设备列表、配置、修订号都必须自洽，且每台设备的审计中必须存在带同一
// 请求标识的对应记录；任何损坏都拒绝打开整个存储。
// 旧存储没有批量记录时 disk 为 nil，正常打开。
func (s *Store) restoreBatches(disk map[string]diskBatch) error {
	out := make(map[string]*batchRequestState, len(disk))
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
			// 设备审计中必须存在带同一请求标识、同一新修订号的记录。
			found := false
			for _, a := range dev.Audit {
				if a.RequestID == id && a.Revision == dd.NewRevision {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("%w: batch request %s device %s audit missing", ErrCorruptStorage, id, dd.DeviceID)
			}
			rec.Devices = append(rec.Devices, batchDeviceState{
				DeviceID:    dd.DeviceID,
				Revision:    dd.Revision,
				NewRevision: dd.NewRevision,
				Config:      cloneRaw(dd.Config),
			})
		}
		out[id] = rec
	}
	// 反向校验：审计中的非空请求标识必须对应一条已保存的批量记录
	// （单设备修改的审计标识为空，无需补填）。
	for devID, dev := range s.devices {
		for _, a := range dev.Audit {
			if a.RequestID == "" {
				continue
			}
			if _, ok := out[a.RequestID]; !ok {
				return fmt.Errorf("%w: device %s audit references unknown batch request %s",
					ErrCorruptStorage, devID, a.RequestID)
			}
		}
	}
	s.batches = out
	return nil
}
