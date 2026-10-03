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
	Rollback       diskOp    `json:"rollback,omitempty"`
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
		// expected 收集本活动已接受结果的操作：每个这样的操作都必须在结果
		// 历史中恰好对应一条记录，键为操作标识。
		expected := map[string]ResultRecord{}
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
			if dc.RollbackOnFailure {
				if dd.Rollback.ID != wantRB {
					return fmt.Errorf("%w: campaign %s rollback op id mismatch", ErrCorruptStorage, id)
				}
			} else {
				// 未开启回滚的活动（含旧存储）不得残留任何回滚进展；
				// 回滚标识允许缺省（旧存储）或等于稳定标识（新存储）。
				if dd.Rollback.ID != "" && dd.Rollback.ID != wantRB {
					return fmt.Errorf("%w: campaign %s rollback op id mismatch", ErrCorruptStorage, id)
				}
				if dd.RollbackTarget != "" ||
					dd.Rollback.Claimed || dd.Rollback.HasResult ||
					dd.Status == DeviceAwaitingRollback || dd.Status == DeviceRollingBack ||
					dd.Phase == StageRollback || isRollbackTerminal(dd.Status) {
					return fmt.Errorf("%w: campaign %s rollback state in non-rollback campaign", ErrCorruptStorage, id)
				}
			}
			// 终态设备必须带时间；失败还须带阶段与原因。
			if isTerminal(dd.Status) && dd.At.IsZero() {
				return fmt.Errorf("%w: campaign %s terminal without time", ErrCorruptStorage, id)
			}
			if dd.Status == DeviceFailed && dd.Reason == "" {
				return fmt.Errorf("%w: campaign %s failure without reason", ErrCorruptStorage, id)
			}
			if (dd.Status == DeviceRollbackFailed || dd.Status == DeviceRollbackTimeout ||
				dd.Status == DeviceAwaitingRollback) && dd.Reason == "" {
				return fmt.Errorf("%w: campaign %s rollback without reason", ErrCorruptStorage, id)
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
			ops := []diskOp{dd.Download, dd.Install}
			if dc.RollbackOnFailure {
				ops = append(ops, dd.Rollback)
			}
			for _, o := range ops {
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
			if dc.RollbackOnFailure {
				if err := validateRollbackDisk(id, dd); err != nil {
					return err
				}
			}
			// 已接受结果的操作必须能在结果历史中找到对应记录。下载成功等待
			// 安装、安装失败等待回滚等中间状态同样已有结果；已领取但未完成的
			// 操作、超时或未执行的阶段没有结果，也不要求历史记录。
			// 未开启回滚的活动上面已校验回滚操作无结果，这里一并遍历是安全的。
			for _, e := range []struct {
				stage string
				o     diskOp
			}{
				{StageDownload, dd.Download},
				{StageInstall, dd.Install},
				{StageRollback, dd.Rollback},
			} {
				if !e.o.HasResult {
					continue
				}
				expected[e.o.ID] = ResultRecord{
					DeviceID:    dd.DeviceID,
					OperationID: e.o.ID,
					Stage:       e.stage,
					Success:     e.o.Success,
					Reason:      e.o.Reason,
					Version:     e.o.ResultVersion,
					At:          e.o.At,
				}
			}
			rbOp := decodeOp(dd.Rollback)
			if !dc.RollbackOnFailure {
				// 与创建路径一致：内存中始终持有稳定的回滚操作标识。
				rbOp = opState{ID: wantRB}
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
				Rollback:       rbOp,
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
			if dr.Stage == StageRollback && !dc.RollbackOnFailure {
				return fmt.Errorf("%w: campaign %s rollback result in non-rollback campaign", ErrCorruptStorage, id)
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
			// 每条历史记录都必须对应该设备该阶段实际已有的结果；设备、操作
			// 标识与阶段已由上面的标识校验保证一致，这里比对成败、原因、版本
			// 与首次接受结果的时间。时间按同一时刻判断，不因时区表示不同而冲突。
			exp, ok := expected[dr.OperationID]
			if !ok {
				return fmt.Errorf("%w: campaign %s result history without accepted result", ErrCorruptStorage, id)
			}
			if exp.Success != dr.Success || exp.Reason != dr.Reason ||
				exp.Version != dr.Version || !exp.At.Equal(dr.At) {
				return fmt.Errorf("%w: campaign %s result history mismatches accepted result", ErrCorruptStorage, id)
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
		// 结果历史与已接受的操作结果必须一一对应：上面已保证每条记录
		// 不重复且对应一个已接受结果，数量相等即每个已接受结果都恰好
		// 有一条记录；缺少记录同样视为损坏，拒绝打开整个存储。
		if len(dc.Results) != len(expected) {
			return fmt.Errorf("%w: campaign %s accepted result missing from result history", ErrCorruptStorage, id)
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
		DeviceAwaitingRollback, DeviceRollingBack,
		DeviceRollbackSucceeded, DeviceRollbackFailed, DeviceRollbackTimeout:
		return true
	default:
		return false
	}
}

// isRollbackTerminal 判断状态是否属于回滚相关终态。
func isRollbackTerminal(status string) bool {
	switch status {
	case DeviceRollbackSucceeded, DeviceRollbackFailed, DeviceRollbackTimeout:
		return true
	default:
		return false
	}
}

// isRollbackFlowStatus 判断状态是否处于回滚流程（等待/进行中/任一回滚终态）。
func isRollbackFlowStatus(status string) bool {
	switch status {
	case DeviceAwaitingRollback, DeviceRollingBack,
		DeviceRollbackSucceeded, DeviceRollbackFailed, DeviceRollbackTimeout:
		return true
	default:
		return false
	}
}

// validateRollbackDisk 校验开启回滚的活动中单台设备的回滚状态自洽。
func validateRollbackDisk(campaignID string, dd diskDev) error {
	bad := func(msg string) error {
		return fmt.Errorf("%w: campaign %s device %s %s", ErrCorruptStorage, campaignID, dd.DeviceID, msg)
	}
	rb := dd.Rollback
	// 回滚目标只能在首次领取下载时或之后锁定：有目标就必已领取下载。
	// 反向不成立——领取下载时复查版本不兼容会直接失败，不锁定目标、也不回滚。
	if dd.RollbackTarget != "" && !dd.Download.Claimed {
		return bad("rollback target locked before download claim")
	}
	// 进入回滚流程的设备必然经过成功下载，目标一定已锁定。
	if isRollbackFlowStatus(dd.Status) && dd.RollbackTarget == "" {
		return bad("rollback status without locked target")
	}
	switch dd.Status {
	case DeviceAwaitingRollback:
		// 已接受安装失败、回滚尚未领取：回滚无结果。
		if dd.Phase != StageRollback || rb.Claimed || rb.HasResult {
			return bad("awaiting rollback state mismatch")
		}
		if !(dd.Install.HasResult && !dd.Install.Success) {
			return bad("awaiting rollback without install failure")
		}
	case DeviceRollingBack:
		if dd.Phase != StageRollback || !rb.Claimed || rb.HasResult {
			return bad("rolling back state mismatch")
		}
	case DeviceRollbackSucceeded:
		if dd.Phase != StageRollback || !rb.HasResult || !rb.Success {
			return bad("rollback succeeded without success result")
		}
		if rb.ResultVersion != dd.RollbackTarget {
			return bad("rollback version mismatch")
		}
		if _, ok := decodeObject(rb.ResultConfig); !ok {
			return bad("rollback config invalid")
		}
	case DeviceRollbackFailed:
		if dd.Phase != StageRollback || !rb.HasResult || rb.Success || rb.Reason == "" {
			return bad("rollback failed without failure result")
		}
	case DeviceRollbackTimeout:
		if dd.Phase != StageRollback {
			return bad("rollback timeout phase mismatch")
		}
		// 等待回滚超时（未领取）或正在回滚超时（已领取未完成）都可能。
		if rb.HasResult {
			return bad("rollback timeout with result")
		}
	default:
		// 其余状态：开启回滚后安装一旦失败必然进入回滚流程，
		// 因此这里不能有安装失败，回滚操作也必须从未被领取或提交；
		// 但回滚目标可能因已领取下载而存在（下载失败/超时/成功等）。
		if dd.Install.HasResult && !dd.Install.Success {
			return bad("install failure without rollback state")
		}
		if rb.Claimed || rb.HasResult {
			return bad("rollback progressed without install failure")
		}
	}
	return nil
}
