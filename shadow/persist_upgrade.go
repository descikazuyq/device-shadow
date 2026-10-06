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
	ID                string    `json:"id"`
	Operator          string    `json:"operator"`
	Target            string    `json:"target"`
	CreatedAt         time.Time `json:"createdAt"`
	BatchSize         int       `json:"batchSize"`
	WindowStart       time.Time `json:"windowStart"`
	WindowEnd         time.Time `json:"windowEnd"`
	Deadline          time.Time `json:"deadline"`
	RollbackOnFailure bool      `json:"rollbackOnFailure,omitempty"`
	// IDScheme 是操作标识方案（opIDScheme*）；缺省为修复前的旧方案。
	IDScheme int          `json:"idScheme,omitempty"`
	LastTime time.Time    `json:"lastTime"`
	Status   string       `json:"status"`
	Ended    bool         `json:"ended"`
	EndedAt  time.Time    `json:"endedAt,omitempty"`
	Devices  []diskDev    `json:"devices"`
	Results  []diskResult `json:"results,omitempty"`
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
			IDScheme:          c.idScheme,
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

// expectedHist 是从已保存操作结果推导出的应有历史条目。
type expectedHist struct {
	deviceID string
	stage    string
	op       diskOp
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
		if dc.IDScheme != opIDSchemeLegacy && dc.IDScheme != opIDSchemeV2 {
			return fmt.Errorf("%w: campaign %s unknown operation id scheme", ErrCorruptStorage, id)
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
			idScheme:          dc.IDScheme,
			LastTime:          dc.LastTime,
			Status:            dc.Status,
			Ended:             dc.Ended,
			EndedAt:           dc.EndedAt,
			index:             map[string]int{},
		}
		// 旧记录可能没有时间基线：缺省（零值）时仍以创建时间作为基线。
		// 基线与各业务记录时间是否自洽在设备与历史全部恢复后统一检查。
		if c.LastTime.IsZero() {
			c.LastTime = c.CreatedAt
		}
		seen := map[string]bool{}
		// expected 按操作标识收集每个已有结果（HasResult）的下载/安装/回滚
		// 操作所对应的唯一应有历史条目，随后与保存的结果历史逐条对账。
		expected := map[string]expectedHist{}
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
			if dc.IDScheme == opIDSchemeV2 {
				// 新方案：标识必须是无歧义编码且恰好归属本活动、本设备、本阶段。
				if !validV2OpID(dd.Download.ID, id, dd.DeviceID, StageDownload) ||
					!validV2OpID(dd.Install.ID, id, dd.DeviceID, StageInstall) ||
					!validV2OpID(dd.Rollback.ID, id, dd.DeviceID, StageRollback) {
					return fmt.Errorf("%w: campaign %s operation id mismatch", ErrCorruptStorage, id)
				}
			} else {
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
				} else if dd.Rollback.ID != "" && dd.Rollback.ID != wantRB {
					// 未开启回滚的旧活动：回滚标识允许缺省或等于旧稳定标识。
					return fmt.Errorf("%w: campaign %s rollback op id mismatch", ErrCorruptStorage, id)
				}
			}
			if !dc.RollbackOnFailure {
				// 未开启回滚的活动（含旧存储）不得残留任何回滚进展。
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
				if err := checkSuccessReport(id, dd, dd.Install, StageInstall, s.devices[dd.DeviceID]); err != nil {
					return err
				}
			}
			if dc.RollbackOnFailure {
				if err := validateRollbackDisk(id, dd); err != nil {
					return err
				}
			}
			if dd.Rollback.HasResult && dd.Rollback.Success {
				// 未开启回滚的活动存在回滚结果已在上方拒绝；这里只需核对
				// 开启回滚的活动中已接受的回滚成功记录。
				if err := checkSuccessReport(id, dd, dd.Rollback, StageRollback, s.devices[dd.DeviceID]); err != nil {
					return err
				}
			}
			rbOp := decodeOp(dd.Rollback)
			if !dc.RollbackOnFailure {
				// 与创建路径一致：内存中始终持有本方案稳定的回滚操作标识。
				if dc.IDScheme == opIDSchemeV2 {
					rbOp = opState{ID: dd.Rollback.ID}
				} else {
					rbOp = opState{ID: operationID(id, dd.DeviceID, StageRollback)}
				}
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
			// 每个已有结果的操作都应当恰好对应一条结果历史；
			// 已领取未完成、超时或因前批失败而未执行的阶段没有结果，不入账。
			addExpected := func(o diskOp, stage string) {
				if !o.HasResult {
					return
				}
				expected[o.ID] = expectedHist{deviceID: dd.DeviceID, stage: stage, op: o}
			}
			addExpected(dd.Download, StageDownload)
			addExpected(dd.Install, StageInstall)
			if dc.RollbackOnFailure {
				addExpected(dd.Rollback, StageRollback)
			}
		}
		// seenHistory 记录已经与保存历史对过账的操作标识。
		seenHistory := map[string]bool{}
		var prevAt time.Time
		for i, dr := range dc.Results {
			if dr.At.IsZero() {
				return fmt.Errorf("%w: campaign %s result without time", ErrCorruptStorage, id)
			}
			// 历史按接受顺序保存：时间不得倒退；同一时刻按保存次序排列。
			if i > 0 && dr.At.Before(prevAt) {
				return fmt.Errorf("%w: campaign %s result history out of order", ErrCorruptStorage, id)
			}
			prevAt = dr.At
			if _, ok := c.index[dr.DeviceID]; !ok {
				return fmt.Errorf("%w: campaign %s result device mismatch", ErrCorruptStorage, id)
			}
			if dr.Stage != StageDownload && dr.Stage != StageInstall && dr.Stage != StageRollback {
				return fmt.Errorf("%w: campaign %s result stage invalid", ErrCorruptStorage, id)
			}
			if dr.Stage == StageRollback && !dc.RollbackOnFailure {
				return fmt.Errorf("%w: campaign %s rollback result in non-rollback campaign", ErrCorruptStorage, id)
			}
			if dc.IDScheme == opIDSchemeLegacy {
				// 旧方案标识可按（活动, 设备, 阶段）直接重算；新方案标识的
				// 归属已在设备操作校验中确认，这里靠下方的历史对账锚定。
				if wantOp := operationID(id, dr.DeviceID, dr.Stage); dr.OperationID != wantOp {
					return fmt.Errorf("%w: campaign %s result op id mismatch", ErrCorruptStorage, id)
				}
			}
			if seenHistory[dr.OperationID] {
				return fmt.Errorf("%w: campaign %s duplicated result history", ErrCorruptStorage, id)
			}
			seenHistory[dr.OperationID] = true
			// 历史必须对应该设备该阶段实际已有的结果：多出记录、设备或阶段不符
			// 都按损坏处理，不能通过删历史或补结果把矛盾隐藏起来。
			want, ok := expected[dr.OperationID]
			if !ok {
				return fmt.Errorf("%w: campaign %s result history without operation result", ErrCorruptStorage, id)
			}
			if want.deviceID != dr.DeviceID || want.stage != dr.Stage {
				return fmt.Errorf("%w: campaign %s result history stage mismatch", ErrCorruptStorage, id)
			}
			if !dr.Success && dr.Reason == "" {
				return fmt.Errorf("%w: campaign %s failed result without reason", ErrCorruptStorage, id)
			}
			o := want.op
			if o.Success != dr.Success {
				return fmt.Errorf("%w: campaign %s result history success mismatch", ErrCorruptStorage, id)
			}
			// 失败原因必须一致。成功结果的原因不进入历史（提交路径亦不拒绝
			// 成功结果附带原因），故只对失败结果比对原因。
			if !o.Success && o.Reason != dr.Reason {
				return fmt.Errorf("%w: campaign %s result history reason mismatch", ErrCorruptStorage, id)
			}
			// 时间按同一时刻判断，不因时区表示不同而冲突。
			if !o.At.Equal(dr.At) {
				return fmt.Errorf("%w: campaign %s result history time mismatch", ErrCorruptStorage, id)
			}
			// 安装或回滚成功时记录的版本必须与当次操作接受的版本一致。
			if dr.Success && (dr.Stage == StageInstall || dr.Stage == StageRollback) {
				if dr.Version != o.ResultVersion || dr.Version == "" {
					return fmt.Errorf("%w: campaign %s result history version mismatch", ErrCorruptStorage, id)
				}
			} else if dr.Version != "" {
				// 下载成功及任何失败结果都不携带版本。
				return fmt.Errorf("%w: campaign %s result history with unexpected version", ErrCorruptStorage, id)
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
		// 反向对账：每个已有结果的操作都必须恰好有一条历史；缺少记录即损坏。
		for opID, want := range expected {
			if !seenHistory[opID] {
				return fmt.Errorf("%w: campaign %s device %s %s result missing history",
					ErrCorruptStorage, id, want.deviceID, want.stage)
			}
		}
		// 活动整体结论必须与设备进度一致：与接收设备结果时的 settle 共用
		// campaignConclusion 的同一套结论规则——任一设备仍处于非终态（等待下载、
		// 下载中、等待安装、安装中、等待回滚、回滚中），活动必须 running 且
		// Ended 为 false；全部设备终态时活动必须已结束，且只有每台设备都
		// succeeded 才能 succeeded，其余组合（失败、超时、未执行，以及回滚
		// 成功/失败/超时——回滚成功只是恢复原版本，不算本次升级成功）只能
		// failed。设备离线或维护窗口结束不改变这一判断。
		wantEnded, wantStatus := campaignConclusion(c)
		if !wantEnded {
			if c.Ended || c.Status != wantStatus {
				return fmt.Errorf("%w: campaign %s ended while devices unfinished", ErrCorruptStorage, id)
			}
		} else {
			if !c.Ended || c.Status != wantStatus {
				return fmt.Errorf("%w: campaign %s status %s does not match device results",
					ErrCorruptStorage, id, c.Status)
			}
		}
		// 批次放行顺序必须与保存的升级结果自洽：任一设备首次领取下载时，
		// 较早批次的每台设备都必须已经首次接受“安装成功”结果，且其成功
		// 时间不晚于该次下载领取时间。只看活动保存的结果，不看设备当前在线
		// 状态或当前版本；下载成功、安装中、安装失败后等待回滚、已回滚等
		// 都不算放行。该核对对仍在执行和已结束的活动都生效，下载后来的
		// 成功、失败或超时不能掩盖首次领取时越批的矛盾。
		if err := checkCampaignBatchRelease(id, c); err != nil {
			return err
		}
		// 每个已领取操作的首次领取时刻都必须落在活动保存的原维护窗口
		// [start,end) 内。运行时领取已强制窗口，重开时还要防止保存的
		// 领取时间被挪到窗口外；该核对对仍在执行和已结束的活动、对下载/
		// 安装/回滚都生效，操作后来的成功、失败或超时都不能掩盖窗口外的
		// 首次领取。尚未领取的操作没有领取时间，不在此列。
		if err := checkCampaignClaimWindows(id, c); err != nil {
			return err
		}
		// 时间基线必须能解释全部已存在的业务记录，否则重开后本应被拒绝的
		// 旧时间请求又能推进活动。不能抬高基线或改写任何记录来掩盖矛盾。
		if err := checkCampaignTimeBaseline(id, c); err != nil {
			return err
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

// checkCampaignBatchRelease 核对活动的批次放行顺序与保存的升级结果自洽：
// 任一已经领取过下载的设备，在其首次领取下载时，所有较早批次的设备都必须
// 已经成功完成安装，且每台前序设备首次接受安装成功结果的时间不晚于该次
// 下载的首次领取时间。只按活动保存的升级结果判断：设备当前是否在线、当前
// 版本是否恰好等于目标版本都不能替代前序安装成功；前序设备仅下载成功、
// 正在安装、安装失败后等待回滚或已经回滚成功均不满足放行条件。
// 尚未领取下载的设备（正常等待后批、因前批失败而未执行、未领取即随截止
// 超时）不参与核对，也不能仅因前批没有成功而拒绝。时间按同一时刻比较，
// 时区写法不同不视为矛盾；同一时刻合法。核对对仍在执行和已结束的活动都
// 生效，下载后来成功、失败或超时都不能掩盖首次领取时越过前批的问题。
func checkCampaignBatchRelease(campaignID string, c *campaignState) error {
	for _, cd := range c.Devices {
		if !cd.Download.Claimed || cd.Download.ClaimedAt.IsZero() {
			continue
		}
		claimedAt := cd.Download.ClaimedAt
		for _, prev := range c.Devices {
			if prev.Batch >= cd.Batch {
				// 同批设备各自推进，不等待同批其他设备；更晚批次不相关。
				continue
			}
			if !(prev.Install.HasResult && prev.Install.Success) {
				return fmt.Errorf("%w: campaign %s device %s download claimed while previous-batch device %s not installed",
					ErrCorruptStorage, campaignID, cd.DeviceID, prev.DeviceID)
			}
			// time.Time 的比较按绝对时刻进行，不因时区写法不同而拒绝
			// 相同时刻；安装成功时刻晚于下载领取时刻即属越批领取，相等合法。
			if prev.Install.At.After(claimedAt) {
				return fmt.Errorf("%w: campaign %s device %s download claimed at %s before device %s install succeeded at %s",
					ErrCorruptStorage, campaignID, cd.DeviceID,
					claimedAt.Format(time.RFC3339Nano), prev.DeviceID, prev.Install.At.Format(time.RFC3339Nano))
			}
		}
	}
	return nil
}

// checkCampaignClaimWindows 核对每个已领取操作的首次领取时刻都位于活动保存
// 的原维护窗口 [start,end) 内（开始时刻包含、结束时刻不包含）。运行时只有
// 在线且位于窗口内才能首次领取下载、安装或回滚，因此保存的活动中一旦出现
// 窗口外的首次领取时间（早于开始、恰好结束或晚于结束），说明存储记录与领取
// 规则矛盾，必须拒绝打开。
// 核对只针对真正的首次领取时刻，按绝对时刻比较：带不同时区写法但表示同一
// 时刻的时间得到相同结果。尚未领取的操作（等待下载、等待安装、等待回滚及
// 未领取便随截止结束）没有领取时间，不参与核对。已领取的操作后来在窗口外
// 重复查询、在窗口外截止前提交结果、活动已成功或失败结束，以及该操作后来
// 成功、失败或超时，都不能让窗口外的首次领取合法化——重复查询时间、结果
// 接受时间与活动结束时间都不与窗口比较。未开启回滚的活动不检查缺省的回滚
// 操作，它从未被领取。
func checkCampaignClaimWindows(campaignID string, c *campaignState) error {
	check := func(cd *campaignDevice, stage string, op *opState) error {
		if !op.Claimed || op.ClaimedAt.IsZero() {
			return nil
		}
		at := op.ClaimedAt
		if at.Before(c.WindowStart) || !at.Before(c.WindowEnd) {
			return fmt.Errorf("%w: campaign %s device %s %s claimed at %s outside maintenance window [%s, %s)",
				ErrCorruptStorage, campaignID, cd.DeviceID, stage,
				at.Format(time.RFC3339Nano),
				c.WindowStart.Format(time.RFC3339Nano), c.WindowEnd.Format(time.RFC3339Nano))
		}
		return nil
	}
	for _, cd := range c.Devices {
		if err := check(cd, StageDownload, &cd.Download); err != nil {
			return err
		}
		if err := check(cd, StageInstall, &cd.Install); err != nil {
			return err
		}
		if c.RollbackOnFailure {
			if err := check(cd, StageRollback, &cd.Rollback); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkCampaignTimeBaseline 校验活动时间基线（已接受的最新时间）与保存的
// 业务记录自洽：基线必须不早于创建时间、任一设备已经领取下载/安装/回滚的
// 时间、任一操作首次接受结果的时间、设备当前状态中已记录的时间；活动已结束
// 时还必须不早于保存的结束时间。只比较确实存在的记录：尚未领取的操作没有
// 领取时间、尚未接受结果的操作没有结果时间、新建且尚未推进的设备允许没有
// 状态时间，这些正常缺省值不导致打开失败。比较按绝对时刻进行，相等合法。
func checkCampaignTimeBaseline(campaignID string, c *campaignState) error {
	baseline := c.LastTime
	bad := func(format string, args ...any) error {
		msg := fmt.Sprintf(format, args...)
		return fmt.Errorf("%w: campaign %s time baseline %s %s",
			ErrCorruptStorage, campaignID, baseline.Format(time.RFC3339Nano), msg)
	}
	if baseline.Before(c.CreatedAt) {
		return bad("before creation time %s", c.CreatedAt.Format(time.RFC3339Nano))
	}
	if c.Ended && !c.EndedAt.IsZero() && baseline.Before(c.EndedAt) {
		return bad("before end time %s", c.EndedAt.Format(time.RFC3339Nano))
	}
	for _, cd := range c.Devices {
		if !cd.At.IsZero() && baseline.Before(cd.At) {
			return bad("before device %s state time %s",
				cd.DeviceID, cd.At.Format(time.RFC3339Nano))
		}
		// 已领取（含尚未完成）的操作一定接受过携带领取时刻的请求；
		// 已接受结果的操作还必须比较首次接受结果的时间。未领取/未接受
		// 的操作对应时间为零值，属正常缺省，跳过比较。
		ops := []struct {
			stage string
			op    *opState
		}{
			{StageDownload, &cd.Download},
			{StageInstall, &cd.Install},
			{StageRollback, &cd.Rollback},
		}
		for _, item := range ops {
			if !item.op.ClaimedAt.IsZero() && baseline.Before(item.op.ClaimedAt) {
				return bad("before device %s %s claim time %s",
					cd.DeviceID, item.stage, item.op.ClaimedAt.Format(time.RFC3339Nano))
			}
			if item.op.HasResult && !item.op.At.IsZero() && baseline.Before(item.op.At) {
				return bad("before device %s %s result time %s",
					cd.DeviceID, item.stage, item.op.At.Format(time.RFC3339Nano))
			}
		}
	}
	return nil
}

// checkSuccessReport 核对一条已接受的安装/回滚成功记录保存的附带上报与
// 对应设备影子最近接受的上报之间的关系。序号必须是正整数且不大于设备最近
// 接受的序号：缺失、为零或大于设备记录都按损坏处理。两边序号相等时，这条
// 成功记录代表的就是设备最近一次上报，其版本、完整上报配置与首次接受结果时
// 保存的发生时间必须分别与影子保存的版本、上报配置和最近上报时间一致
// （配置按 JSON 语义比较，时间按同一时刻判断，不因时区表示不同而报错）。
// 序号更小则是已被后续上报覆盖的历史记录，不要求与当前影子一致。
// 设备当前是否在线不影响本核对；下载结果、失败结果与没有成功结果的阶段
// 不保存附带上报序号，也不进入本核对。
func checkSuccessReport(campaignID string, dd diskDev, o diskOp, stage string, d *deviceState) error {
	bad := func(format string, args ...any) error {
		msg := fmt.Sprintf(format, args...)
		return fmt.Errorf("%w: campaign %s device %s %s success report %s",
			ErrCorruptStorage, campaignID, dd.DeviceID, stage, msg)
	}
	if o.ResultSeq == 0 {
		return bad("missing report sequence")
	}
	if o.ResultSeq > d.LastSeq {
		return bad("sequence %d not accepted by device (last accepted %d)", o.ResultSeq, d.LastSeq)
	}
	if o.ResultSeq == d.LastSeq {
		if o.ResultVersion != d.Version {
			return bad("version %s does not match latest reported version %s", o.ResultVersion, d.Version)
		}
		if !rawEqual(o.ResultConfig, d.Reported) {
			return bad("config does not match latest reported config")
		}
		if !o.At.Equal(d.LastReportTime) {
			return bad("time %s does not match latest report time %s",
				o.At.Format(time.RFC3339Nano), d.LastReportTime.Format(time.RFC3339Nano))
		}
	}
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
	// 已领取下载且尚无下载结果，或已有成功下载结果：回滚目标必须已锁定，
	// 字段缺失与空串按同一种损坏处理。这项要求不因设备进入安装阶段或
	// 活动已结束而取消——否则安装失败后会留下没有明确恢复版本的回滚待办。
	// 不能用当前影子版本、活动目标版本或允许升级列表替代丢失的目标。
	// 首次领取前复查版本不兼容、直接在下载阶段失败的设备（下载已有失败
	// 结果）允许没有目标：这种失败不进入安装或回滚。
	if dd.RollbackTarget == "" && dd.Download.Claimed &&
		(!dd.Download.HasResult || dd.Download.Success) {
		return bad("missing locked rollback target")
	}
	// 进入回滚流程的设备必然经过成功下载，目标一定已锁定。
	if isRollbackFlowStatus(dd.Status) && dd.RollbackTarget == "" {
		return bad("rollback status without locked target")
	}
	// 回滚流程的唯一入口是“已接受本次安装的失败结果”：等待回滚、正在回滚
	// 以及回滚成功/失败/超时的设备，都必须能从本设备在本活动中已经接受的
	// 安装结果确认安装失败。安装尚未领取、已领取但没有结果，或安装结果为
	// 成功，都不能支持任何回滚状态；即使回滚目标非空、操作标识合法、回滚
	// 结果与历史一致或活动已结束，也必须核对这一前提。当前版本恰好等于旧
	// 版本、下载失败以及其他设备或其他活动的失败记录都不能代替本次安装失败
	// （这里只看本设备本活动保存的安装操作结果本身）。
	installFailed := dd.Install.HasResult && !dd.Install.Success
	if isRollbackFlowStatus(dd.Status) && !installFailed {
		return bad("rollback state without accepted install failure")
	}
	switch dd.Status {
	case DeviceAwaitingRollback:
		// 已接受安装失败、回滚尚未领取：回滚无结果。
		if dd.Phase != StageRollback || rb.Claimed || rb.HasResult {
			return bad("awaiting rollback state mismatch")
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
