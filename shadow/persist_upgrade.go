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
				// 标为 failed 的设备只能是下载失败，或未开启失败回滚时直接结束
				// 的安装失败：阶段只能是 download 或 install（回滚阶段失败另有
				// rollback_failed 状态；开启回滚时安装失败进入回滚流程，也不会
				// 以此状态结束），且必须对应本设备在本活动中已接受的该阶段失败
				// 结果。设备状态中的失败原因必须与结果原因逐字一致，失败时间
				// 必须表示首次接受该结果的同一实际时刻。不能把下载失败写成回滚
				// 阶段，也不能借同批其他设备、其他活动的失败记录或设备当前影子
				// 版本来解释它。矛盾一律拒绝打开整个存储，不能跳过该设备提供
				// 其余记录，也不能改写原因、挪动时间或删去历史使其打开。
				if err := validateFailedDeviceDisk(id, dd); err != nil {
					return err
				}
			}
			// pending（等待下载）与 downloading（下载中）都必须停留在下载阶段，
			// 且只能依据本设备在本活动保存的下载操作记录核对。
			if err := validateDownloadStageDisk(id, dd); err != nil {
				return err
			}
			// ready（等待安装）与 installing（正在安装）都必须停留在安装阶段，
			// 且只能依据本设备在本活动保存的下载/安装操作记录核对。
			if err := validateInstallStageDisk(id, dd); err != nil {
				return err
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
		// 每个已领取操作的首次领取时间必须落在活动保存的原维护窗口内；
		// 窗口外的首次领取不能当作有效进度接受，也不能靠挪动领取时间、
		// 撤销领取或重写进度来消除矛盾。
		if err := checkCampaignClaimWindow(id, c); err != nil {
			return err
		}
		// 时间基线必须能解释全部已存在的业务记录，否则重开后本应被拒绝的
		// 旧时间请求又能推进活动。不能抬高基线或改写任何记录来掩盖矛盾。
		if err := checkCampaignTimeBaseline(id, c); err != nil {
			return err
		}
		// 已接受结果的操作，其首次结果发生时间不得早于该操作自己的首次领取
		// 时间；结果先于领取发生在因果上不可能，不能靠其他设备的领取时间、
		// 设备影子的最近上报时间或活动结束时间把它“合法化”。
		if err := checkCampaignResultOrder(id, c); err != nil {
			return err
		}
		// 已接受结果的操作，其首次结果发生时间还必须严格早于活动截止时间；
		// 截止时刻及以后正常提交路径不再接收结果，已有存储中的迟到结果不能
		// 当作有效进度读取。
		if err := checkCampaignResultDeadline(id, c); err != nil {
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

// checkCampaignClaimWindow 核对每个已领取的下载/安装/回滚操作的首次领取时间
// 都落在活动保存的原维护窗口 [start, end) 内：开始时刻包含、结束时刻不包含。
// 首次领取必须遵守维护窗口——早于开始或恰好到达、晚于结束的领取记录都使整个
// 存储损坏，即使其余状态与结果历史完全自洽、活动仍在执行或已成功/失败结束、
// 该操作后来成功、失败或超时，也不能把窗口外的首次领取当作有效进度接受，
// 更不能通过挪动领取时间、撤销领取、删去活动或重写进度来消除矛盾。
// 这项核对只针对真正首次领取的时刻：尚未领取的操作没有领取时间（等待下载、
// 等待安装、等待回滚及未领取便结束的记录），属正常缺省，不参与核对；未开启
// 回滚的活动内存中只有缺省的回滚操作标识，不算已领取。已经在窗口内领取的
// 操作，后来在窗口外的重复查询、截止前的结果接受以及活动结束时间都不要求
// 重新满足窗口。比较按绝对时刻进行，时区写法不同不视为矛盾。
func checkCampaignClaimWindow(campaignID string, c *campaignState) error {
	for _, cd := range c.Devices {
		ops := []struct {
			stage string
			op    *opState
		}{
			{StageDownload, &cd.Download},
			{StageInstall, &cd.Install},
			{StageRollback, &cd.Rollback},
		}
		for _, item := range ops {
			if !item.op.Claimed {
				continue
			}
			at := item.op.ClaimedAt
			// 窗口为 [start, end)：恰好到达开始合法，恰好到达结束不合法。
			// time.Time 的比较按绝对时刻进行，同一时刻的不同时区写法结果相同。
			if at.Before(c.WindowStart) || !at.Before(c.WindowEnd) {
				return fmt.Errorf("%w: campaign %s device %s %s first claimed at %s outside maintenance window [%s, %s)",
					ErrCorruptStorage, campaignID, cd.DeviceID, item.stage,
					at.Format(time.RFC3339Nano),
					c.WindowStart.Format(time.RFC3339Nano),
					c.WindowEnd.Format(time.RFC3339Nano))
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

// checkCampaignResultOrder 校验每个已经接受结果的下载/安装/回滚操作，其首次
// 结果发生时间都不早于该操作自己的首次领取时间：同一活动、同一设备、同一阶段
// 的两个时刻直接比较，成功与失败适用同一规则。结果先于领取在因果上不可能，
// 必须使整个存储拒绝打开；不能用其他设备的领取时间、设备影子的最近上报时间、
// 活动的最新时间或结束时间替代这两个时刻，也不能通过修改时间、删除结果历史、
// 撤销领取或改变设备状态让记录变得合法。后来安装成功或回滚完成同样不能掩盖
// 早先阶段的矛盾，因此该核对对仍在执行和已结束的活动都生效。
// 只比较该操作自己确实存在的两个时刻：尚未接受结果（含已领取未完成、随后因
// 截止超时）的操作没有结果时间，尚未领取的等待步骤没有领取时间，均属正常
// 缺省，跳过核对。首次领取时因版本不兼容而直接形成的下载失败，领取与失败
// 可以是同一时刻；两时刻相等合法。已领取操作在窗口结束后、截止前提交结果
// 不受限制——本核对不要求结果落在维护窗口内。比较按绝对时刻进行，时区写法
// 不同不视为矛盾。重复提交不覆盖首次记录，这里使用的就是首次接受的两个时刻。
func checkCampaignResultOrder(campaignID string, c *campaignState) error {
	for _, cd := range c.Devices {
		ops := []struct {
			stage string
			op    *opState
		}{
			{StageDownload, &cd.Download},
			{StageInstall, &cd.Install},
			{StageRollback, &cd.Rollback},
		}
		for _, item := range ops {
			o := item.op
			// 未接受结果（含已领取未完成、超时）或未领取的操作不参与核对：
			// HasResult 为 true 时上方的操作校验已保证 Claimed 且两个时间非零。
			if !o.HasResult {
				continue
			}
			// time.Time 的比较按绝对时刻进行：结果早于首次领取即损坏，
			// 相同时刻的不同时区写法结果相同，相等合法。
			if o.At.Before(o.ClaimedAt) {
				return fmt.Errorf("%w: campaign %s device %s %s result at %s before first claim at %s",
					ErrCorruptStorage, campaignID, cd.DeviceID, item.stage,
					o.At.Format(time.RFC3339Nano), o.ClaimedAt.Format(time.RFC3339Nano))
			}
		}
	}
	return nil
}

// checkCampaignResultDeadline 校验每个已经接受结果的下载/安装/回滚操作，其首次
// 结果发生时间都严格早于所属活动的截止时间：正常提交路径在截止时刻及以后不再
// 把结果接收为操作结果（已领取未完成的操作随截止记为超时），因此已有存储中
// 等于或晚于截止的结果时间不可能由正常提交产生，必须使整个存储拒绝打开。
// 成功与失败适用同一规则；恰好等于截止时间也属于迟到。比较按绝对时刻进行，
// 时区写法不同不改变判断。该核对对仍在执行和已结束的活动都生效：设备后来
// 安装成功、回滚完成或当前版本恰好等于目标版本，都不能让早先的迟到结果合法。
// 只比较操作自己确实保存的结果时间：已领取但尚无结果、随后因截止而超时的操作
// 没有结果时间，属正常缺省，不参与核对；活动的超时状态时间与结束时间可以等于
// 或晚于截止，普通设备上报也不受这项限制，这些时间不能代替操作结果时间参与
// 判断。截止前已接受的结果后来重复提交时不改写首次记录，这里使用的是首次接受
// 的时刻，不按重复提交时间判断。命中任意一条都必须返回 ErrCorruptStorage，
// 不能通过删除迟到结果、调整时间或把设备改成超时来继续打开。
func checkCampaignResultDeadline(campaignID string, c *campaignState) error {
	for _, cd := range c.Devices {
		ops := []struct {
			stage string
			op    *opState
		}{
			{StageDownload, &cd.Download},
			{StageInstall, &cd.Install},
			{StageRollback, &cd.Rollback},
		}
		for _, item := range ops {
			o := item.op
			// 未接受结果（含已领取未完成、随后超时）的操作没有结果时间，
			// 属正常缺省，跳过核对。
			if !o.HasResult {
				continue
			}
			// time.Time 的比较按绝对时刻进行：结果不早于截止（相等或更晚）
			// 即属迟到，同一时刻的不同时区写法判断相同。
			if !o.At.Before(c.Deadline) {
				return fmt.Errorf("%w: campaign %s device %s %s result at %s not before deadline %s",
					ErrCorruptStorage, campaignID, cd.DeviceID, item.stage,
					o.At.Format(time.RFC3339Nano), c.Deadline.Format(time.RFC3339Nano))
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

// validateDownloadStageDisk 校验 pending（等待下载）与 downloading（下载中）两种
// 设备状态与本设备在本活动中保存的下载操作进度一致。两种状态都必须停留在下载
// 阶段：pending 表示本设备在本活动中的下载从未领取、尚无下载结果；downloading
// 表示下载已经领取、但尚未接受任何下载结果。即使操作标识、时间与结果历史各自
// 合法，下载已领取却仍为等待下载、下载未领取却标为下载中，或已经保存下载成功
// 或失败结果却仍使用任一状态，都与事实矛盾；阶段已写成安装或回滚同样矛盾。
// 核对只看本设备在本活动中的下载操作记录：同批其他设备已领取或下载成功不能替
// 它补足进度，设备当前版本恰好等于目标版本、普通上报改变了版本或配置也不能
// 代替下载领取与结果。尚未领取下载时没有领取时间、尚无下载结果时没有结果时间，
// 是正常缺省；设备后来离线、维护窗口已结束都不使已保存的下载进度失效。矛盾
// 一律返回 ErrCorruptStorage，拒绝打开整个存储，不通过调整状态、撤销领取、
// 删除历史或补造下载结果消除矛盾。
func validateDownloadStageDisk(campaignID string, dd diskDev) error {
	bad := func(msg string) error {
		return fmt.Errorf("%w: campaign %s device %s %s", ErrCorruptStorage, campaignID, dd.DeviceID, msg)
	}
	switch dd.Status {
	case DevicePending, DeviceDownloading:
	default:
		return nil
	}
	if dd.Phase != StageDownload {
		return bad("download-stage status without download phase")
	}
	switch dd.Status {
	case DevicePending:
		// 等待下载：下载从未领取、尚无下载结果；没有领取时间与结果时间是正常
		// 缺省。下载已领取或已有成功/失败结果都与 pending 矛盾。
		if dd.Download.Claimed || dd.Download.HasResult {
			return bad("pending state with download claimed or resulted")
		}
	case DeviceDownloading:
		// 下载中：下载已经领取，但还没有接受下载结果（成功或失败都不行）。
		if !dd.Download.Claimed {
			return bad("downloading state without download claim")
		}
		if dd.Download.HasResult {
			return bad("downloading state with download result")
		}
	}
	return nil
}

// validateFailedDeviceDisk 校验 failed 设备的失败阶段、原因与时间与本设备在
// 本活动中已接受的失败结果一致。适用于下载失败，以及未开启失败回滚时直接结束
// 的安装失败（开启回滚的活动中安装失败会进入回滚流程，不以此状态结束；下载
// 失败在两种活动中都直接 failed）。这样的设备失败阶段只能是 download 或
// install——回滚阶段的失败另有 rollback_failed 状态，不能把下载失败写成回滚
// 阶段；且所选阶段必须恰好对应本设备自己在本活动中已接受的该阶段失败结果：
// 设备状态中的失败原因要与结果原因逐字一致，失败时间要与首次接受该结果的时间
// 表示同一实际时刻（时区写法不同但时刻相同合法）。
// 核对只看本设备在本活动中的操作记录：不能借同批其他设备或其他活动的失败
// 记录解释它，也不能用设备当前影子版本替代失败结果。活动结束时间晚于该设备
// 失败时间、其他设备仍在处理或已先行失败都不改变本核对。矛盾一律返回
// ErrCorruptStorage，拒绝打开整个存储：不能只跳过这台设备提供其余记录，也不
// 能改写原因、挪动时间或删去历史使文件可以打开。
func validateFailedDeviceDisk(campaignID string, dd diskDev) error {
	bad := func(msg string) error {
		return fmt.Errorf("%w: campaign %s device %s %s", ErrCorruptStorage, campaignID, dd.DeviceID, msg)
	}
	switch dd.Phase {
	case StageDownload, StageInstall:
	default:
		return bad("failed device phase must be download or install")
	}
	// 锚定本设备自己在所选阶段的操作记录：该操作必须已有失败结果。
	// 开启回滚活动中的安装失败必然进入回滚流程、不会以 failed/install
	// 结束（由回滚状态校验统一拦截），因此这里只需要核对阶段与结果本身。
	failedOp := dd.Download
	if dd.Phase == StageInstall {
		failedOp = dd.Install
	}
	if !failedOp.HasResult || failedOp.Success {
		return bad("failed device without accepted failed result for its phase")
	}
	if failedOp.Reason == "" {
		return bad("failed result without reason")
	}
	if dd.Reason == "" {
		return bad("failure without reason")
	}
	// 设备状态的失败原因必须与已接受失败结果的原因逐字一致。
	if dd.Reason != failedOp.Reason {
		return bad("failure reason does not match accepted failed result")
	}
	// 设备状态的失败时间必须与首次接受该失败结果的时间表示同一实际时刻；
	// time.Time 的比较按绝对时刻进行，时区写法不同不视为矛盾。
	if dd.At.IsZero() || failedOp.At.IsZero() || !dd.At.Equal(failedOp.At) {
		return bad("failure time does not match accepted failed result time")
	}
	return nil
}

// validateInstallStageDisk 校验 ready（等待安装）与 installing（正在安装）两种
// 设备状态与本设备在本活动中保存的操作进度一致。两种状态都必须已有本设备接受
// 的下载成功结果并停留在安装阶段：ready 表示安装从未领取、尚无安装结果；
// installing 表示安装已经领取、但还没有接受安装结果。即使操作标识、时间与结果
// 历史各自合法，已完成安装（安装成功或失败已有结果）的设备也不能解释成仍在
// 等待或正在安装，安装已经领取的记录同样不能停在 ready。核对只看本设备本活动
// 的下载/安装操作记录：不能借用同批其他设备的下载成功，也不能用设备当前版本
// 恰好等于目标版本替代下载或安装结果。尚未领取安装时没有安装领取时间、尚无
// 安装结果时没有结果时间，是正常缺省；设备离线、维护窗口已结束或普通上报改变
// 了当前版本都不在此核对范围内。矛盾一律返回 ErrCorruptStorage，拒绝打开整个
// 存储，不通过调整状态、撤销领取、删除历史或补造结果消除矛盾。
func validateInstallStageDisk(campaignID string, dd diskDev) error {
	bad := func(msg string) error {
		return fmt.Errorf("%w: campaign %s device %s %s", ErrCorruptStorage, campaignID, dd.DeviceID, msg)
	}
	switch dd.Status {
	case DeviceReady, DeviceInstalling:
	default:
		return nil
	}
	if dd.Phase != StageInstall {
		return bad("install-stage status without install phase")
	}
	// 两种状态都必须有本设备在本活动中已接受的下载成功结果。
	if !(dd.Download.HasResult && dd.Download.Success) {
		return bad("install-stage status without accepted download success")
	}
	switch dd.Status {
	case DeviceReady:
		// 等待安装：安装从未领取、尚无安装结果；没有安装领取时间与结果时间
		// 是正常缺省。安装已领取或已有成功/失败结果都与 ready 矛盾。
		if dd.Install.Claimed || dd.Install.HasResult {
			return bad("ready state with install claimed or resulted")
		}
	case DeviceInstalling:
		// 正在安装：安装已经领取，但还没有接受安装结果（成功或失败都不行）。
		if !dd.Install.Claimed {
			return bad("installing state without install claim")
		}
		if dd.Install.HasResult {
			return bad("installing state with install result")
		}
	}
	return nil
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
