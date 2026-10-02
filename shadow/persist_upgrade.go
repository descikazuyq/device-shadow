package shadow

import (
	"encoding/json"
	"fmt"
	"time"
)

// diskVersion 是升级版本登记的磁盘格式。
type diskVersion struct {
	Target      string   `json:"target"`
	AllowedFrom []string `json:"allowedFrom"`
}

// diskCampaign 是活动的磁盘格式，设备按提交次序保存。
type diskCampaign struct {
	ID                string       `json:"id"`
	Operator          string       `json:"operator"`
	Target            string       `json:"target"`
	CreatedAt         time.Time    `json:"createdAt"`
	BatchSize         int          `json:"batchSize"`
	WindowStart       time.Time    `json:"windowStart"`
	WindowEnd         time.Time    `json:"windowEnd"`
	Deadline          time.Time    `json:"deadline"`
	RollbackOnFailure bool         `json:"rollbackOnFailure,omitempty"`
	LastTime          time.Time    `json:"lastTime"`
	Status            string       `json:"status"`
	Ended             bool         `json:"ended"`
	EndedAt           time.Time    `json:"endedAt,omitempty"`
	Devices           []diskDev    `json:"devices"`
	Results           []diskResult `json:"results,omitempty"`
}

type diskOp struct {
	ID            string          `json:"id"`
	Claimed       bool            `json:"claimed"`
	ClaimedAt     time.Time       `json:"claimedAt,omitempty"`
	HasResult     bool            `json:"hasResult"`
	Success       bool            `json:"success"`
	Reason        string          `json:"reason,omitempty"`
	At            time.Time       `json:"at,omitempty"`
	ResultSeq     uint64          `json:"resultSeq,omitempty"`
	ResultVersion string          `json:"resultVersion,omitempty"`
	ResultConfig  json.RawMessage `json:"resultConfig,omitempty"`
}

type diskDev struct {
	DeviceID       string    `json:"deviceId"`
	Batch          int       `json:"batch"`
	Status         string    `json:"status"`
	Phase          string    `json:"phase"`
	Reason         string    `json:"reason,omitempty"`
	At             time.Time `json:"at,omitempty"`
	Download       diskOp    `json:"download"`
	Install        diskOp    `json:"install"`
	Rollback       diskOp    `json:"rollback"`
	RollbackTarget string    `json:"rollbackTarget,omitempty"`
}

type diskResult struct {
	DeviceID    string    `json:"deviceId"`
	OperationID string    `json:"operationId"`
	Stage       string    `json:"stage"`
	Success     bool      `json:"success"`
	Reason      string    `json:"reason,omitempty"`
	Version     string    `json:"version,omitempty"`
	At          time.Time `json:"at"`
}

func (s *Store) marshalVersions() map[string]diskVersion {
	out := make(map[string]diskVersion, len(s.versions))
	for k, v := range s.versions {
		out[k] = diskVersion{Target: v.Target, AllowedFrom: append([]string(nil), v.AllowedFrom...)}
	}
	return out
}

func (s *Store) marshalCampaigns() (map[string]diskCampaign, error) {
	out := make(map[string]diskCampaign, len(s.campaigns))
	for id, c := range s.campaigns {
		dc := diskCampaign{
			ID:                c.ID,
			Operator:          c.Operator,
			Target:            c.Target,
			CreatedAt:         c.CreatedAt,
			BatchSize:         c.BatchSize,
			WindowStart:       c.WindowStart,
			WindowEnd:         c.WindowEnd,
			Deadline:          c.Deadline,
			RollbackOnFailure: c.RollbackOnFailure,
			LastTime:          c.LastTime,
			Status:            c.Status,
			Ended:             c.Ended,
			EndedAt:           c.EndedAt,
			Results:           make([]diskResult, 0, len(c.Results)),
		}
		for _, cd := range c.Devices {
			dc.Devices = append(dc.Devices, diskDev{
				DeviceID:       cd.DeviceID,
				Batch:          cd.Batch,
				Status:         cd.Status,
				Phase:          cd.Phase,
				Reason:         cd.Reason,
				At:             cd.At,
				Download:       encodeOp(cd.Download),
				Install:        encodeOp(cd.Install),
				Rollback:       encodeOp(cd.Rollback),
				RollbackTarget: cd.RollbackTarget,
			})
		}
		for _, r := range c.Results {
			dc.Results = append(dc.Results, diskResult{
				DeviceID:    r.DeviceID,
				OperationID: r.OperationID,
				Stage:       r.Stage,
				Success:     r.Success,
				Reason:      r.Reason,
				Version:     r.Version,
				At:          r.At,
			})
		}
		out[id] = dc
	}
	return out, nil
}

func encodeOp(o opState) diskOp {
	return diskOp{
		ID:            o.ID,
		Claimed:       o.Claimed,
		ClaimedAt:     o.ClaimedAt,
		HasResult:     o.HasResult,
		Success:       o.Success,
		Reason:        o.Reason,
		At:            o.At,
		ResultSeq:     o.ResultSeq,
		ResultVersion: o.ResultVersion,
		ResultConfig:  cloneRaw(o.ResultConfig),
	}
}

func decodeOp(d diskOp) opState {
	return opState{
		ID:            d.ID,
		Claimed:       d.Claimed,
		ClaimedAt:     d.ClaimedAt,
		HasResult:     d.HasResult,
		Success:       d.Success,
		Reason:        d.Reason,
		At:            d.At,
		ResultSeq:     d.ResultSeq,
		ResultVersion: d.ResultVersion,
		ResultConfig:  cloneRaw(d.ResultConfig),
	}
}

// restoreVersions 恢复并校验版本登记。
func (s *Store) restoreVersions(disk map[string]diskVersion) error {
	out := make(map[string]*versionState, len(disk))
	for k, dv := range disk {
		if k == "" || dv.Target == "" || k != dv.Target {
			return fmt.Errorf("%w: invalid upgrade version entry", ErrCorruptStorage)
		}
		if err := validateAllowedFrom(dv.Target, dv.AllowedFrom); err != nil {
			return fmt.Errorf("%w: version %s: %v", ErrCorruptStorage, dv.Target, err)
		}
		out[k] = &versionState{Target: dv.Target, AllowedFrom: append([]string(nil), dv.AllowedFrom...)}
	}
	s.versions = out
	return nil
}

// restoreCampaigns 恢复活动并做一致性校验。版本、设备引用、操作标识、
// 结果历史都必须自洽，否则整个存储拒绝打开。
func (s *Store) restoreCampaigns(disk map[string]diskCampaign) error {
	out := make(map[string]*campaignState, len(disk))
	for id, dc := range disk {
		if id == "" || dc.ID != id {
			return fmt.Errorf("%w: campaign id mismatch", ErrCorruptStorage)
		}
		if dc.Operator == "" || dc.Target == "" || dc.BatchSize <= 0 {
			return fmt.Errorf("%w: campaign %s invalid", ErrCorruptStorage, id)
		}
		if dc.CreatedAt.IsZero() || dc.WindowStart.IsZero() || dc.WindowEnd.IsZero() || dc.Deadline.IsZero() {
			return fmt.Errorf("%w: campaign %s missing time", ErrCorruptStorage, id)
		}
		if !dc.WindowStart.Before(dc.WindowEnd) || !dc.Deadline.After(dc.CreatedAt) {
			return fmt.Errorf("%w: campaign %s invalid time range", ErrCorruptStorage, id)
		}
		if _, ok := s.versions[dc.Target]; !ok {
			return fmt.Errorf("%w: campaign %s target %s missing", ErrCorruptStorage, id, dc.Target)
		}
		if dc.Status != CampaignRunning && dc.Status != CampaignSucceeded && dc.Status != CampaignFailed {
			return fmt.Errorf("%w: campaign %s bad status", ErrCorruptStorage, id)
		}
		if dc.Ended == (dc.Status == CampaignRunning) {
			return fmt.Errorf("%w: campaign %s ended/status mismatch", ErrCorruptStorage, id)
		}
		if len(dc.Devices) == 0 {
			return fmt.Errorf("%w: campaign %s has no devices", ErrCorruptStorage, id)
		}
		c := &campaignState{
			ID:                dc.ID,
			Operator:          dc.Operator,
			Target:            dc.Target,
			CreatedAt:         dc.CreatedAt,
			BatchSize:         dc.BatchSize,
			WindowStart:       dc.WindowStart,
			WindowEnd:         dc.WindowEnd,
			Deadline:          dc.Deadline,
			RollbackOnFailure: dc.RollbackOnFailure,
			LastTime:          dc.LastTime,
			Status:            dc.Status,
			Ended:             dc.Ended,
			EndedAt:           dc.EndedAt,
			index:             map[string]int{},
		}
		if c.LastTime.IsZero() {
			c.LastTime = c.CreatedAt
		}
		if c.LastTime.Before(c.CreatedAt) {
			return fmt.Errorf("%w: campaign %s last time before creation", ErrCorruptStorage, id)
		}
		seen := map[string]bool{}
		for i, dd := range dc.Devices {
			if dd.DeviceID == "" || seen[dd.DeviceID] {
				return fmt.Errorf("%w: campaign %s device list invalid", ErrCorruptStorage, id)
			}
			seen[dd.DeviceID] = true
			if _, ok := s.devices[dd.DeviceID]; !ok {
				return fmt.Errorf("%w: campaign %s device %s missing", ErrCorruptStorage, id, dd.DeviceID)
			}
			if dd.Batch != i/dc.BatchSize {
				return fmt.Errorf("%w: campaign %s batch mismatch", ErrCorruptStorage, id)
			}
			if !validDeviceStatus(dd.Status) {
				return fmt.Errorf("%w: campaign %s bad device status", ErrCorruptStorage, id)
			}
			if dd.Phase != StageDownload && dd.Phase != StageInstall && dd.Phase != StageRollback {
				return fmt.Errorf("%w: campaign %s bad phase", ErrCorruptStorage, id)
			}
			wantDL := operationID(id, dd.DeviceID, StageDownload)
			wantIN := operationID(id, dd.DeviceID, StageInstall)
			wantRB := operationID(id, dd.DeviceID, StageRollback)
			if dd.Download.ID != wantDL || dd.Install.ID != wantIN {
				return fmt.Errorf("%w: campaign %s operation id mismatch", ErrCorruptStorage, id)
			}
			// 旧存储没有回滚字段：补齐回滚标识，保持兼容。
			if dd.Rollback.ID == "" {
				dd.Rollback.ID = wantRB
			} else if dd.Rollback.ID != wantRB {
				return fmt.Errorf("%w: campaign %s operation id mismatch", ErrCorruptStorage, id)
			}
			// 终态设备必须带时间；失败还须带阶段与原因。
			if isTerminal(dd.Status) && dd.At.IsZero() {
				return fmt.Errorf("%w: campaign %s terminal without time", ErrCorruptStorage, id)
			}
			if dd.Status == DeviceFailed && dd.Reason == "" {
				return fmt.Errorf("%w: campaign %s failure without reason", ErrCorruptStorage, id)
			}
			if dd.Status == DeviceSucceeded &&
				!(dd.Download.HasResult && dd.Download.Success && dd.Install.HasResult && dd.Install.Success) {
				return fmt.Errorf("%w: campaign %s succeeded device without results", ErrCorruptStorage, id)
			}
			if dd.Status == DeviceFailed {
				failedOp := dd.Download
				if dd.Phase == StageInstall {
					failedOp = dd.Install
				}
				if !failedOp.HasResult || failedOp.Success {
					return fmt.Errorf("%w: campaign %s failed device without failed result", ErrCorruptStorage, id)
				}
			}
			if dd.Status == DeviceReady && !(dd.Download.HasResult && dd.Download.Success) {
				return fmt.Errorf("%w: campaign %s ready device without download", ErrCorruptStorage, id)
			}
			for _, o := range []diskOp{dd.Download, dd.Install} {
				if o.HasResult {
					if !o.Claimed {
						return fmt.Errorf("%w: campaign %s unclaimed result", ErrCorruptStorage, id)
					}
					if o.At.IsZero() {
						return fmt.Errorf("%w: campaign %s result without time", ErrCorruptStorage, id)
					}
					if !o.Success && o.Reason == "" {
						return fmt.Errorf("%w: campaign %s failed result without reason", ErrCorruptStorage, id)
					}
				}
				if o.Claimed && o.ClaimedAt.IsZero() {
					return fmt.Errorf("%w: campaign %s claim without time", ErrCorruptStorage, id)
				}
			}
			if dd.Install.HasResult && !(dd.Download.HasResult && dd.Download.Success) {
				return fmt.Errorf("%w: campaign %s install before download", ErrCorruptStorage, id)
			}
			if dd.Install.Claimed && !(dd.Download.HasResult && dd.Download.Success) {
				return fmt.Errorf("%w: campaign %s install claimed before download", ErrCorruptStorage, id)
			}
			if dd.Install.HasResult && dd.Install.Success {
				if dd.Install.ResultVersion != dc.Target {
					return fmt.Errorf("%w: campaign %s install version mismatch", ErrCorruptStorage, id)
				}
				if _, ok := decodeObject(dd.Install.ResultConfig); !ok {
					return fmt.Errorf("%w: campaign %s install config invalid", ErrCorruptStorage, id)
				}
			}
			// 回滚状态校验。
			if isRollbackStatus(dd.Status) {
				if !dc.RollbackOnFailure {
					return fmt.Errorf("%w: campaign %s rollback state without rollback enabled", ErrCorruptStorage, id)
				}
				if dd.RollbackTarget == "" {
					return fmt.Errorf("%w: campaign %s rollback without target", ErrCorruptStorage, id)
				}
			}
			switch dd.Status {
			case DeviceRollbackPending:
				if dd.Rollback.Claimed || dd.Rollback.HasResult {
					return fmt.Errorf("%w: campaign %s rollback pending but started", ErrCorruptStorage, id)
				}
			case DeviceRollingBack:
				if !dd.Rollback.Claimed || dd.Rollback.HasResult {
					return fmt.Errorf("%w: campaign %s rolling back state invalid", ErrCorruptStorage, id)
				}
			case DeviceRollbackSucceeded:
				if !(dd.Rollback.HasResult && dd.Rollback.Success) {
					return fmt.Errorf("%w: campaign %s rollback succeeded without result", ErrCorruptStorage, id)
				}
				if dd.Rollback.ResultVersion != dd.RollbackTarget {
					return fmt.Errorf("%w: campaign %s rollback version mismatch", ErrCorruptStorage, id)
				}
				if _, ok := decodeObject(dd.Rollback.ResultConfig); !ok {
					return fmt.Errorf("%w: campaign %s rollback config invalid", ErrCorruptStorage, id)
				}
			case DeviceRollbackFailed:
				if !dd.Rollback.HasResult || dd.Rollback.Success {
					return fmt.Errorf("%w: campaign %s rollback failed without failed result", ErrCorruptStorage, id)
				}
			}
			if dd.Rollback.HasResult {
				if !dd.Rollback.Claimed {
					return fmt.Errorf("%w: campaign %s unclaimed rollback result", ErrCorruptStorage, id)
				}
				if dd.Rollback.At.IsZero() {
					return fmt.Errorf("%w: campaign %s rollback result without time", ErrCorruptStorage, id)
				}
				if !dd.Rollback.Success && dd.Rollback.Reason == "" {
					return fmt.Errorf("%w: campaign %s rollback failed result without reason", ErrCorruptStorage, id)
				}
			}
			if dd.Rollback.Claimed && dd.Rollback.ClaimedAt.IsZero() {
				return fmt.Errorf("%w: campaign %s rollback claim without time", ErrCorruptStorage, id)
			}
			cd := &campaignDevice{
				DeviceID:       dd.DeviceID,
				Batch:          dd.Batch,
				Status:         dd.Status,
				Phase:          dd.Phase,
				Reason:         dd.Reason,
				At:             dd.At,
				Download:       decodeOp(dd.Download),
				Install:        decodeOp(dd.Install),
				Rollback:       decodeOp(dd.Rollback),
				RollbackTarget: dd.RollbackTarget,
			}
			c.index[cd.DeviceID] = len(c.Devices)
			c.Devices = append(c.Devices, cd)
		}
		seenResult := map[string]bool{}
		for _, dr := range dc.Results {
			if dr.At.IsZero() {
				return fmt.Errorf("%w: campaign %s result without time", ErrCorruptStorage, id)
			}
			if _, ok := c.index[dr.DeviceID]; !ok {
				return fmt.Errorf("%w: campaign %s result device mismatch", ErrCorruptStorage, id)
			}
			if dr.Stage != StageDownload && dr.Stage != StageInstall && dr.Stage != StageRollback {
				return fmt.Errorf("%w: campaign %s result stage invalid", ErrCorruptStorage, id)
			}
			wantOp := operationID(id, dr.DeviceID, dr.Stage)
			if dr.OperationID != wantOp {
				return fmt.Errorf("%w: campaign %s result op id mismatch", ErrCorruptStorage, id)
			}
			key := dr.OperationID
			if seenResult[key] {
				return fmt.Errorf("%w: campaign %s duplicated result history", ErrCorruptStorage, id)
			}
			seenResult[key] = true
			if !dr.Success && dr.Reason == "" {
				return fmt.Errorf("%w: campaign %s failed result without reason", ErrCorruptStorage, id)
			}
			c.Results = append(c.Results, ResultRecord{
				DeviceID:    dr.DeviceID,
				OperationID: dr.OperationID,
				Stage:       dr.Stage,
				Success:     dr.Success,
				Reason:      dr.Reason,
				Version:     dr.Version,
				At:          dr.At,
			})
		}
		out[id] = c
	}
	// 设备不能同时参加多个未结束活动。
	busy := map[string]string{}
	for id, c := range out {
		if c.Ended {
			continue
		}
		for _, cd := range c.Devices {
			if other, ok := busy[cd.DeviceID]; ok {
				return fmt.Errorf("%w: device %s in active campaigns %s and %s",
					ErrCorruptStorage, cd.DeviceID, other, id)
			}
			busy[cd.DeviceID] = id
		}
	}
	s.campaigns = out
	return nil
}

func validDeviceStatus(status string) bool {
	switch status {
	case DevicePending, DeviceDownloading, DeviceReady, DeviceInstalling,
		DeviceSucceeded, DeviceFailed, DeviceTimeout, DeviceSkipped,
		DeviceRollbackPending, DeviceRollingBack, DeviceRollbackSucceeded, DeviceRollbackFailed:
		return true
	default:
		return false
	}
}

// isRollbackStatus 判断设备状态是否属于回滚阶段。
func isRollbackStatus(status string) bool {
	switch status {
	case DeviceRollbackPending, DeviceRollingBack, DeviceRollbackSucceeded, DeviceRollbackFailed:
		return true
	default:
		return false
	}
}
