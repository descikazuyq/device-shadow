package shadow

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// 升级阶段标识。
const (
	StageDownload = "download"
	StageInstall  = "install"
	StageRollback = "rollback"
)

// 活动与设备在活动中的状态。
const (
	CampaignRunning   = "running"
	CampaignSucceeded = "succeeded"
	CampaignFailed    = "failed"

	DevicePending     = "pending"     // 尚未领取下载
	DeviceDownloading = "downloading" // 已领取下载，未完成
	DeviceReady       = "ready"       // 下载成功，安装尚未领取
	DeviceInstalling  = "installing"  // 已领取安装，未完成
	DeviceSucceeded   = "succeeded"
	DeviceFailed      = "failed"
	DeviceTimeout     = "timeout"
	DeviceSkipped     = "skipped" // 前序批次失败后，后续批次记为未执行

	// 回滚相关状态，仅出现在开启回滚的活动中。
	DeviceAwaitingRollback  = "awaiting_rollback" // 安装失败已接受，等待领取回滚
	DeviceRollingBack       = "rolling_back"      // 已领取回滚，未完成
	DeviceRollbackSucceeded = "rollback_succeeded"
	DeviceRollbackFailed    = "rollback_failed"
	DeviceRollbackTimeout   = "rollback_timeout" // 到达截止时间时回滚仍未完成
)

// VersionSpec 是一个已登记的升级目标版本及其允许的当前版本。
type VersionSpec struct {
	// Target 是目标版本，与设备当前版本按字符串精确匹配。
	Target string
	// AllowedFrom 是允许升级到目标版本的当前版本列表。
	AllowedFrom []string
}

// versionState 是升级版本登记的内部状态。
type versionState struct {
	Target      string
	AllowedFrom []string
}

// CampaignSpec 描述创建分批升级活动所需的全部信息。
type CampaignSpec struct {
	// ID 是活动唯一标识，不能为空。
	ID string
	// Operator 是创建活动的操作者，不能为空。
	Operator string
	// CreatedAt 是活动创建时间。
	CreatedAt time.Time
	// TargetVersion 是已登记的目标版本。
	TargetVersion string
	// Devices 是按提交次序排列的有序设备列表，不能为空、不能重复。
	Devices []string
	// BatchSize 是批大小，必须是正整数。
	BatchSize int
	// WindowStart/WindowEnd 是维护窗口，开始时刻须早于结束时刻；
	// 窗口包含开始时刻、不包含结束时刻。
	WindowStart time.Time
	WindowEnd   time.Time
	// Deadline 是截止时间，必须晚于创建时间。
	Deadline time.Time
	// RollbackOnFailure 开启安装失败后的自动回滚，默认关闭。
	// 关闭时安装失败即设备失败；开启后安装失败会先等待设备回滚到
	// 首次领取下载时锁定的当前版本，活动最终仍为失败。
	RollbackOnFailure bool
}

// Operation 是派发给设备的一个升级操作。
type Operation struct {
	// ID 是操作的稳定标识，重复查询未完成操作时保持不变，重开存储后仍有效。
	ID         string
	CampaignID string
	DeviceID   string
	// Kind 是 StageDownload、StageInstall 或 StageRollback。
	Kind string
	// TargetVersion 仅在回滚操作上有值，指明必须恢复到的锁定版本，
	// 让设备明确知道要恢复哪个版本；下载与安装操作为空。
	TargetVersion string
}

// OperationResult 是设备提交的一次操作结果。
// 每次结果必须带活动、设备、操作标识及发生时间。
// 安装成功（Success 且操作是 install）时还须附带符合上报规则的设备上报：
// 正整数 Seq、等于目标版本的 Version 和完整 JSON 对象 Config。
// 回滚成功（Success 且操作是 rollback）时同样须附带上报，
// 但 Version 必须等于领取下载时锁定的回滚目标版本。
type OperationResult struct {
	CampaignID  string
	DeviceID    string
	OperationID string
	At          time.Time
	Success     bool
	// Reason 在失败时必填。
	Reason string
	// 安装成功时附带的设备上报。
	Seq     uint64
	Version string
	Config  json.RawMessage
}

// ResultRecord 是结果历史中的一条记录，按接受顺序保存。
type ResultRecord struct {
	DeviceID    string
	OperationID string
	Stage       string
	Success     bool
	Reason      string
	// Version 为安装或回滚成功时上报的版本，其余为空。
	Version string
	At      time.Time
}

// opState 是单个设备单个阶段的操作与结果状态。
type opState struct {
	ID string
	// Claimed 表示操作是否已被领取；未领取的操作不能提交结果。
	Claimed   bool
	ClaimedAt time.Time
	// 已接受的结果。
	HasResult     bool
	Success       bool
	Reason        string
	At            time.Time
	ResultSeq     uint64
	ResultVersion string
	ResultConfig  json.RawMessage
}

// campaignDevice 是单台设备在一个活动中的完整状态。
type campaignDevice struct {
	DeviceID string
	Batch    int
	Download opState
	Install  opState
	// Rollback 是安装失败后的回滚操作，仅在开启回滚的活动中使用。
	Rollback opState
	// RollbackTarget 是首次领取下载时锁定的设备当前版本，回滚必须恢复到它。
	// 后续普通上报不改变它，也不要求它另行登记为升级目标。
	RollbackTarget string
	// Status/Phase/Reason/At 描述设备当前状态；
	// failed 必须记录阶段、原因和时间；timeout/skipped 同理。
	Status string
	Phase  string
	Reason string
	At     time.Time
}

// campaignState 是一个分批升级活动的内部状态。
type campaignState struct {
	ID          string
	Operator    string
	Target      string
	CreatedAt   time.Time
	BatchSize   int
	WindowStart time.Time
	WindowEnd   time.Time
	Deadline    time.Time
	// RollbackOnFailure 表示安装失败后是否进入回滚；旧存储缺省为关闭。
	RollbackOnFailure bool
	Devices           []*campaignDevice
	index             map[string]int
	Results           []ResultRecord
	// LastTime 是本活动已接受的最晚时间，用于拒绝时间倒退。
	LastTime time.Time
	Status   string
	Ended    bool
	EndedAt  time.Time
}

// RegisterUpgrade 登记一个升级目标版本及允许升级的当前版本列表。
// 版本字符串精确匹配；列表不能为空、不能含空串、不能包含目标版本。
// 目标版本重复登记时返回 ErrVersionExists 并保留原记录。
func (s *Store) RegisterUpgrade(targetVersion string, allowedFrom []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return err
	}
	if targetVersion == "" {
		return ErrInvalidVersion
	}
	if err := validateAllowedFrom(targetVersion, allowedFrom); err != nil {
		return err
	}
	if _, ok := s.versions[targetVersion]; ok {
		return fmt.Errorf("%w: %s", ErrVersionExists, targetVersion)
	}
	from := append([]string(nil), allowedFrom...)
	return s.commit(func() error {
		s.versions[targetVersion] = &versionState{Target: targetVersion, AllowedFrom: from}
		return nil
	})
}

func validateAllowedFrom(target string, allowedFrom []string) error {
	if len(allowedFrom) == 0 {
		return fmt.Errorf("%w: list is empty", ErrInvalidVersionList)
	}
	for _, v := range allowedFrom {
		if v == "" {
			return fmt.Errorf("%w: contains empty version", ErrInvalidVersionList)
		}
		if v == target {
			return fmt.Errorf("%w: contains target version %s", ErrInvalidVersionList, target)
		}
	}
	return nil
}

// CreateCampaign 创建分批升级活动。任何必填信息缺失、设备或目标版本未登记、
// 设备当前版本不兼容或设备已参加未结束活动时，整次创建失败，不留下部分记录。
func (s *Store) CreateCampaign(spec CampaignSpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return err
	}
	// 先做全部校验，全部通过后才写入，保证原子创建。
	if spec.ID == "" {
		return ErrInvalidCampaignID
	}
	if spec.Operator == "" {
		return ErrInvalidOperator
	}
	if spec.CreatedAt.IsZero() {
		return fmt.Errorf("%w: created at", ErrInvalidTime)
	}
	if spec.WindowStart.IsZero() || spec.WindowEnd.IsZero() {
		return fmt.Errorf("%w: maintenance window", ErrInvalidTime)
	}
	if spec.Deadline.IsZero() {
		return fmt.Errorf("%w: deadline", ErrInvalidTime)
	}
	if len(spec.Devices) == 0 {
		return ErrInvalidDeviceList
	}
	seen := make(map[string]bool, len(spec.Devices))
	for _, id := range spec.Devices {
		if id == "" {
			return ErrInvalidDeviceID
		}
		if seen[id] {
			return fmt.Errorf("%w: %s", ErrDuplicateDevice, id)
		}
		seen[id] = true
	}
	if spec.BatchSize <= 0 {
		return ErrInvalidBatchSize
	}
	if !spec.WindowStart.Before(spec.WindowEnd) {
		return ErrInvalidWindow
	}
	if !spec.Deadline.After(spec.CreatedAt) {
		return ErrInvalidDeadline
	}
	if spec.TargetVersion == "" {
		return ErrInvalidVersion
	}
	if _, ok := s.versions[spec.TargetVersion]; !ok {
		return fmt.Errorf("%w: %s", ErrVersionNotFound, spec.TargetVersion)
	}
	if _, ok := s.campaigns[spec.ID]; ok {
		return fmt.Errorf("%w: %s", ErrCampaignExists, spec.ID)
	}
	ver := s.versions[spec.TargetVersion]
	devs := make([]*campaignDevice, 0, len(spec.Devices))
	for i, id := range spec.Devices {
		d, ok := s.devices[id]
		if !ok {
			return fmt.Errorf("%w: %s", ErrDeviceNotFound, id)
		}
		if !containsString(ver.AllowedFrom, d.Version) {
			return fmt.Errorf("%w: device %s version %s", ErrIncompatibleVersion, id, d.Version)
		}
		if s.deviceBusy(id, spec.ID) {
			return fmt.Errorf("%w: %s", ErrDeviceBusy, id)
		}
		devs = append(devs, &campaignDevice{
			DeviceID: id,
			Batch:    i / spec.BatchSize,
			Status:   DevicePending,
			Phase:    StageDownload,
			Download: opState{ID: operationID(spec.ID, id, StageDownload)},
			Install:  opState{ID: operationID(spec.ID, id, StageInstall)},
			Rollback: opState{ID: operationID(spec.ID, id, StageRollback)},
		})
	}
	campaign := &campaignState{
		ID:                spec.ID,
		Operator:          spec.Operator,
		Target:            spec.TargetVersion,
		CreatedAt:         spec.CreatedAt,
		BatchSize:         spec.BatchSize,
		WindowStart:       spec.WindowStart,
		WindowEnd:         spec.WindowEnd,
		Deadline:          spec.Deadline,
		RollbackOnFailure: spec.RollbackOnFailure,
		Devices:           devs,
		index:             make(map[string]int, len(devs)),
		LastTime:          spec.CreatedAt,
		Status:            CampaignRunning,
	}
	for i, cd := range devs {
		campaign.index[cd.DeviceID] = i
	}
	return s.commit(func() error {
		s.campaigns[spec.ID] = campaign
		return nil
	})
}

// deviceBusy 判断设备是否已参加另一个尚未结束的活动。
func (s *Store) deviceBusy(deviceID, excludeCampaign string) bool {
	for id, c := range s.campaigns {
		if id == excludeCampaign || c.Ended {
			continue
		}
		if _, ok := c.index[deviceID]; ok {
			return true
		}
	}
	return false
}

func operationID(campaignID, deviceID, stage string) string {
	return campaignID + ":" + deviceID + ":" + stage
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Claim 由调用方传入当前时间，为活动中的设备领取当前可执行的操作。
//
// 派发新的下载或安装操作要求设备在线、当前时间位于维护窗口 [start, end) 内、
// 所在批次已被前一批全部成功放行。首次领取下载前会再次核对设备当前版本，
// 不兼容则将该设备（带阶段、原因和时间）记为失败，返回 ErrIncompatibleVersion；
// 开启回滚的活动还会在首次领取下载时把设备当时的当前版本锁定为回滚目标，
// 后续普通上报不改变它。
// 开启回滚且安装失败已被接受后，设备领取回滚操作：首次领取同样要求设备在线
// 且时间位于原维护窗口 [start, end) 内（不受批次放行影响），返回的操作带锁定
// 目标版本；离线或窗口外保留待办。回滚已领取但未完成时再次查询返回同一标识，
// 且可在窗口外、截止前提交结果。
// 已领取但未完成的操作（下载/安装/回滚）再次查询时返回同一标识，不要求在线或位于窗口内。
// 离线设备保留待办；离线或窗口外暂无可领取的新操作时返回 (nil, nil)。
// 时间缺失或相对本活动已接受的时间倒退时拒绝，且不改变状态。
func (s *Store) Claim(campaignID, deviceID string, at time.Time) (*Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	c, cd, err := s.lookupCampaignDevice(campaignID, deviceID)
	if err != nil {
		return nil, err
	}
	if at.IsZero() {
		return nil, ErrInvalidTime
	}
	if at.Before(c.LastTime) {
		return nil, fmt.Errorf("%w: %v before %v", ErrTimeRegression, at, c.LastTime)
	}
	var op *Operation
	var reject error
	err = s.commit(func() error {
		// 到达截止时间：未结束设备全部超时并落盘，本次领取拒绝。
		// 即使查询的设备已有终态，其他未结束设备仍要级联超时。
		if !at.Before(c.Deadline) {
			c.LastTime = at
			s.applyTimeout(c, at)
			reject = ErrCampaignEnded
			return nil
		}
		if c.Ended {
			// 纯拒绝路径：不推进时间基线，不改状态。
			reject = ErrCampaignEnded
			return nil
		}
		if isTerminal(cd.Status) {
			reject = fmt.Errorf("%w: %s", ErrDeviceFinished, deviceID)
			return nil
		}
		c.LastTime = at
		// 已领取但未完成的操作：查询仍返回原标识，不要求在线或位于窗口内。
		if out := outstandingOp(c, cd); out != nil {
			op = out
			return nil
		}
		// 安装失败已接受、等待回滚：回滚是该设备的善后操作，不受批次放行影响。
		// 首次领取要求在线且位于原维护窗口 [start,end) 内；离线或窗口外保留待办。
		if cd.Status == DeviceAwaitingRollback {
			if !s.devices[deviceID].Online {
				return nil
			}
			if at.Before(c.WindowStart) || !at.Before(c.WindowEnd) {
				return nil
			}
			cd.Rollback.Claimed = true
			cd.Rollback.ClaimedAt = at
			cd.Status = DeviceRollingBack
			cd.Phase = StageRollback
			cd.At = at
			op = &Operation{
				ID:            cd.Rollback.ID,
				CampaignID:    c.ID,
				DeviceID:      deviceID,
				Kind:          StageRollback,
				TargetVersion: cd.RollbackTarget,
			}
			return nil
		}
		// 尚未领取的新阶段：先看批次是否放行（不放行时无操作可领）。
		if !s.batchOpen(c, cd) {
			return nil
		}
		if !s.devices[deviceID].Online {
			return nil // 离线保留待办，上线后继续
		}
		if at.Before(c.WindowStart) || !at.Before(c.WindowEnd) {
			return nil // 窗口外不派发新操作；窗口为 [start, end)
		}
		if !cd.Download.Claimed {
			// 首次领取下载前再次检查当前版本。
			current := s.devices[deviceID].Version
			if !containsString(s.versions[c.Target].AllowedFrom, current) {
				reason := fmt.Sprintf("version %s is not compatible with target %s", current, c.Target)
				// 该设备在下载阶段失败：领取即触发，记录阶段、原因和时间。
				cd.Download.Claimed = true
				cd.Download.ClaimedAt = at
				cd.Download.HasResult = true
				cd.Download.Success = false
				cd.Download.Reason = reason
				cd.Download.At = at
				s.failDevice(c, cd, StageDownload, reason, at)
				reject = fmt.Errorf("%w: device %s version %s", ErrIncompatibleVersion, deviceID, current)
				return nil
			}
			// 开启回滚时，把首次领取下载时设备的当前版本锁定为回滚目标；
			// 此后普通上报不改变它，也不要求该版本另行登记为升级目标。
			if c.RollbackOnFailure && cd.RollbackTarget == "" {
				cd.RollbackTarget = current
			}
			cd.Download.Claimed = true
			cd.Download.ClaimedAt = at
			cd.Status = DeviceDownloading
			cd.At = at
			op = &Operation{ID: cd.Download.ID, CampaignID: c.ID, DeviceID: deviceID, Kind: StageDownload}
			return nil
		}
		// 下载已成功：领取安装（尚未领取的安装仍须在线且位于窗口内）。
		cd.Install.Claimed = true
		cd.Install.ClaimedAt = at
		cd.Status = DeviceInstalling
		cd.At = at
		op = &Operation{ID: cd.Install.ID, CampaignID: c.ID, DeviceID: deviceID, Kind: StageInstall}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if reject != nil {
		return nil, reject
	}
	return op, nil
}

// outstandingOp 返回设备已领取但尚无结果的操作；没有则返回 nil。
func outstandingOp(c *campaignState, cd *campaignDevice) *Operation {
	switch {
	case cd.Download.Claimed && !cd.Download.HasResult:
		return &Operation{ID: cd.Download.ID, CampaignID: c.ID, DeviceID: cd.DeviceID, Kind: StageDownload}
	case cd.Install.Claimed && !cd.Install.HasResult:
		return &Operation{ID: cd.Install.ID, CampaignID: c.ID, DeviceID: cd.DeviceID, Kind: StageInstall}
	case cd.Rollback.Claimed && !cd.Rollback.HasResult:
		// 已领取但未完成的回滚再次领取仍返回同一标识，并继续告知恢复目标版本。
		return &Operation{
			ID:            cd.Rollback.ID,
			CampaignID:    c.ID,
			DeviceID:      cd.DeviceID,
			Kind:          StageRollback,
			TargetVersion: cd.RollbackTarget,
		}
	default:
		return nil
	}
}

// batchOpen 判断设备所在批次是否已放行：前序所有设备必须全部成功。
func (s *Store) batchOpen(c *campaignState, cd *campaignDevice) bool {
	for _, other := range c.Devices {
		if other.Batch < cd.Batch && other.Status != DeviceSucceeded {
			return false
		}
	}
	return true
}

// SubmitResult 接收设备提交的操作结果。
//
// 相同操作的相同结果重复提交返回成功，但不增加历史、不重复放行批次；
// 改用不同结果、跳过未领取阶段、提交未领取的回滚、提交其他设备的操作、
// 活动结束后的新结果均报错且不改变状态；已接受结果的重复提交在活动结束后仍有效。
// 下载完成后才能领取并安装；设备失败记录阶段、原因和时间，同批其他设备
// 继续完成，后续批次记为未执行。安装成功须附带版本等于目标版本的合规上报，
// 回滚成功须附带版本等于锁定回滚目标的合规上报，影子与活动在同一次持久化中更新。
// 开启回滚的活动中，安装失败不直接结束设备，而是保留原失败原因与时间、
// 令设备等待回滚；回滚失败必须给出原因，设备以回滚失败结束且影子不变。
// 已领取且尚未接受结果的操作在截止时刻及以后提交时，结果已经迟到：
// 不校验附带上报与失败原因、不接收为操作结果，直接按截止处理——未结束设备
// 全部超时（等待或正在回滚的记回滚超时），活动以失败结束，返回 ErrCampaignEnded。
// 普通设备上报不经过本方法，也不会推进活动。
func (s *Store) SubmitResult(res OperationResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return err
	}
	c, cd, err := s.lookupCampaignDevice(res.CampaignID, res.DeviceID)
	if err != nil {
		return err
	}
	if res.At.IsZero() {
		return ErrInvalidTime
	}
	// 定位操作标识所属阶段；不属于本设备的操作一律视为未知。
	var stage string
	var op *opState
	switch res.OperationID {
	case cd.Download.ID:
		stage, op = StageDownload, &cd.Download
	case cd.Install.ID:
		stage, op = StageInstall, &cd.Install
	case cd.Rollback.ID:
		stage, op = StageRollback, &cd.Rollback
	default:
		return fmt.Errorf("%w: %s", ErrOperationNotFound, res.OperationID)
	}
	// 已接受结果：相同结果幂等（活动结束后仍有效，不增加历史、不重复放行），
	// 不推进时间基线；不同结果冲突。均不改变状态。
	if op.HasResult {
		if resultMatches(op, res) {
			return nil
		}
		return fmt.Errorf("%w: operation %s", ErrResultConflict, res.OperationID)
	}
	if res.At.Before(c.LastTime) {
		return fmt.Errorf("%w: %v before %v", ErrTimeRegression, res.At, c.LastTime)
	}
	if !op.Claimed {
		// 标识是本设备未来阶段的操作但尚未领取：属于跳过阶段（含未领取的回滚）。
		return fmt.Errorf("%w: %s", ErrOperationNotClaimed, res.OperationID)
	}
	// 已领取且尚未接受结果的操作在截止时刻及以后提交：结果已经迟到，
	// 不再接收为操作结果，也不校验附带上报或失败原因——直接按截止处理：
	// 未结束设备全部超时（等待/正在回滚的记回滚超时），活动以失败结束，
	// 状态时间与活动结束时间采用本次提交时间，返回 ErrCampaignEnded。
	// 活动可能已被显式推进到截止而结束：applyTimeout 对已结束活动是空操作，
	// 结果仍按截止拒绝，不能因附带配置非法或缺少原因改成相应的校验错误。
	if !res.At.Before(c.Deadline) {
		if err := s.commit(func() error {
			c.LastTime = res.At
			s.applyTimeout(c, res.At)
			return nil
		}); err != nil {
			return err
		}
		return ErrCampaignEnded
	}
	if !res.Success && res.Reason == "" {
		return ErrInvalidReason
	}
	// 安装/回滚成功附带的设备上报与普通上报共用同一套校验与序号规则，
	// 只是错误统一归 ErrInvalidReport，且版本必须分别等于活动目标或锁定的
	// 回滚目标。outcome 与 cfg 在提交前算好，随后在同一次写入中应用。
	var attachedOutcome reportOutcome
	var reportCfg json.RawMessage
	if res.Success && (stage == StageInstall || stage == StageRollback) {
		// 安装上报版本必须等于升级目标；回滚上报版本必须等于锁定的回滚目标。
		wantVersion := c.Target
		if stage == StageRollback {
			wantVersion = cd.RollbackTarget
		}
		var err error
		attachedOutcome, reportCfg, err = s.validateAttachedReport(res, wantVersion)
		if err != nil {
			return err
		}
	}
	var reject error
	err = s.commit(func() error {
		// 活动已结束且提交时间越过截止时间：只推进时间基线并拒绝
		// （未结束活动的截止处理已在提交前完成，不会到达这里）。
		if !res.At.Before(c.Deadline) {
			c.LastTime = res.At
			s.applyTimeout(c, res.At)
			reject = ErrCampaignEnded
			return nil
		}
		if c.Ended {
			// 活动结束后的新结果：拒绝且不改状态（重复结果已在提交前处理）。
			reject = ErrCampaignEnded
			return nil
		}
		if isTerminal(cd.Status) {
			reject = fmt.Errorf("%w: %s", ErrDeviceFinished, res.DeviceID)
			return nil
		}
		c.LastTime = res.At
		op.HasResult = true
		op.Success = res.Success
		op.Reason = res.Reason
		op.At = res.At
		if !res.Success {
			switch {
			case stage == StageInstall && c.RollbackOnFailure:
				// 安装失败：保留原因和时间，设备转入等待回滚（非终态）；
				// 同批其他设备继续，后续批次立即记为未执行，活动暂不结束。
				s.enterRollback(c, cd, res.Reason, res.At)
			case stage == StageRollback:
				// 回滚失败必须给出原因：设备以回滚失败结束且影子不变。
				s.finishRollbackFailure(c, cd, res.Reason, res.At)
			default:
				s.failDevice(c, cd, stage, res.Reason, res.At)
			}
			return nil
		}
		switch stage {
		case StageDownload:
			cd.Status = DeviceReady
			cd.Phase = StageInstall
			cd.Reason = ""
			cd.At = res.At
		case StageInstall:
			// 安装成功：附带上报与活动同时更新、一起持久化。
			s.applyAttachedReport(cd.DeviceID, attachedOutcome, res, reportCfg)
			op.ResultSeq = res.Seq
			op.ResultVersion = res.Version
			op.ResultConfig = cloneRaw(res.Config)
			cd.Status = DeviceSucceeded
			cd.Phase = StageInstall
			cd.Reason = ""
			cd.At = res.At
		case StageRollback:
			// 回滚成功：按上报规则更新影子（版本回到锁定目标），同时推进回滚状态。
			// 期望配置、修订号与审计保持不变。
			s.applyAttachedReport(cd.DeviceID, attachedOutcome, res, reportCfg)
			op.ResultSeq = res.Seq
			op.ResultVersion = res.Version
			op.ResultConfig = cloneRaw(res.Config)
			cd.Status = DeviceRollbackSucceeded
			cd.Phase = StageRollback
			cd.Reason = ""
			cd.At = res.At
		}
		c.Results = append(c.Results, ResultRecord{
			DeviceID:    cd.DeviceID,
			OperationID: op.ID,
			Stage:       stage,
			Success:     true,
			Version:     op.ResultVersion,
			At:          res.At,
		})
		s.settle(c, res.At)
		return nil
	})
	if err != nil {
		return err
	}
	return reject
}

// resultMatches 判断提交是否为已接受结果的重复提交（同一操作、相同结果）。
// 安装成功结果还须附带相同的上报（序号、版本、配置）；发生时间不参与判定，
// 使活动结束后的重复提交仍然有效。
func resultMatches(op *opState, res OperationResult) bool {
	if op.Success != res.Success || op.Reason != res.Reason {
		return false
	}
	if op.Success && op.ResultSeq != 0 {
		return op.ResultSeq == res.Seq &&
			op.ResultVersion == res.Version &&
			rawEqual(op.ResultConfig, res.Config)
	}
	return true
}

// applyAttachedReport 在安装/回滚成功的同一次写入中应用附带上报（调用方已
// 经 validateAttachedReport 校验）。序号更大（reportAccept）时按普通上报规则
// 写入影子：更新版本、上报配置、序号、在线状态与配置差异；同序号完全重复
// （reportDuplicate，例如该上报此前已被普通 Report 接受）时影子原样保留——
// 不刷新在线状态与差异首次出现时间，操作仍照常完成。
// 只动上报侧，不影响期望配置、修订号与审计。
func (s *Store) applyAttachedReport(deviceID string, outcome reportOutcome, res OperationResult, cfg json.RawMessage) {
	if outcome != reportAccept {
		return
	}
	s.devices[deviceID].applyReport(res.Seq, res.At, res.Version, cfg)
}

// enterRollback 在开启回滚的活动中处理已接受的安装失败：
// 保留原安装失败的原因与时间，令设备等待回滚（非终态）；
// 同批其他设备可继续，后续批次立即记为未执行，但活动暂不结束。
func (s *Store) enterRollback(c *campaignState, cd *campaignDevice, reason string, at time.Time) {
	cd.Status = DeviceAwaitingRollback
	cd.Phase = StageRollback
	cd.Reason = reason
	cd.At = at
	c.Results = append(c.Results, ResultRecord{
		DeviceID:    cd.DeviceID,
		OperationID: cd.Install.ID,
		Stage:       StageInstall,
		Success:     false,
		Reason:      reason,
		At:          at,
	})
	skipLaterBatches(c, cd, at)
	// 不结束活动：设备进入非终态的等待回滚，活动要等其回滚结束。
}

// finishRollbackFailure 处理已接受的回滚失败：必须给出原因，
// 设备以回滚失败结束，影子不变（失败结果不附带、不应用上报）。
func (s *Store) finishRollbackFailure(c *campaignState, cd *campaignDevice, reason string, at time.Time) {
	cd.Status = DeviceRollbackFailed
	cd.Phase = StageRollback
	cd.Reason = reason
	cd.At = at
	c.Results = append(c.Results, ResultRecord{
		DeviceID:    cd.DeviceID,
		OperationID: cd.Rollback.ID,
		Stage:       StageRollback,
		Success:     false,
		Reason:      reason,
		At:          at,
	})
	s.settle(c, at)
}

// skipLaterBatches 把后续批次中尚未结束的设备立即记为未执行。
func skipLaterBatches(c *campaignState, cd *campaignDevice, at time.Time) {
	for _, other := range c.Devices {
		if other.Batch > cd.Batch && !isTerminal(other.Status) {
			other.Status = DeviceSkipped
			other.Phase = pendingPhase(other)
			other.Reason = "not executed: previous batch failed"
			other.At = at
		}
	}
}

// failDevice 将设备记为失败，记录阶段、原因和时间；后续批次全部记为未执行，
// 本批其他设备继续。若全部设备已到终态，则活动以失败结束。
func (s *Store) failDevice(c *campaignState, cd *campaignDevice, phase, reason string, at time.Time) {
	cd.Status = DeviceFailed
	cd.Phase = phase
	cd.Reason = reason
	cd.At = at
	c.Results = append(c.Results, ResultRecord{
		DeviceID:    cd.DeviceID,
		OperationID: opIDFor(cd, phase),
		Stage:       phase,
		Success:     false,
		Reason:      reason,
		At:          at,
	})
	skipLaterBatches(c, cd, at)
	s.settle(c, at)
}

func opIDFor(cd *campaignDevice, phase string) string {
	switch phase {
	case StageInstall:
		return cd.Install.ID
	case StageRollback:
		return cd.Rollback.ID
	default:
		return cd.Download.ID
	}
}

// pendingPhase 返回设备尚未完成的阶段：下载成功后停留在安装阶段。
func pendingPhase(cd *campaignDevice) string {
	if cd.Download.HasResult && cd.Download.Success {
		return StageInstall
	}
	return StageDownload
}

// applyTimeout 在当前时间达到截止时间时，把所有未结束设备记为超时，
// 已有终态保持不变，活动记为失败并结束。等待回滚或正在回滚的设备
// 以回滚阶段超时结束；不改写或清空影子。
func (s *Store) applyTimeout(c *campaignState, at time.Time) {
	if c.Ended {
		return
	}
	for _, cd := range c.Devices {
		if isTerminal(cd.Status) {
			continue
		}
		switch cd.Status {
		case DeviceAwaitingRollback, DeviceRollingBack:
			cd.Status = DeviceRollbackTimeout
			cd.Phase = StageRollback
		default:
			cd.Status = DeviceTimeout
			cd.Phase = pendingPhase(cd)
		}
		cd.Reason = "deadline exceeded"
		cd.At = at
	}
	c.Status = CampaignFailed
	c.Ended = true
	c.EndedAt = at
}

// settle 在全部设备到达终态时结束活动：全部成功则成功，否则失败。
// 即使回滚成功，设备也不是 succeeded，活动仍为失败。
func (s *Store) settle(c *campaignState, at time.Time) {
	allSucceeded := true
	for _, cd := range c.Devices {
		if !isTerminal(cd.Status) {
			return
		}
		if cd.Status != DeviceSucceeded {
			allSucceeded = false
		}
	}
	c.Ended = true
	c.EndedAt = at
	if allSucceeded {
		c.Status = CampaignSucceeded
	} else {
		c.Status = CampaignFailed
	}
}

func isTerminal(status string) bool {
	switch status {
	case DeviceSucceeded, DeviceFailed, DeviceTimeout, DeviceSkipped,
		DeviceRollbackSucceeded, DeviceRollbackFailed, DeviceRollbackTimeout:
		return true
	default:
		return false
	}
}

// AdvanceCampaign 用调用方给出的当前时间推进活动：到达截止时间时把未结束
// 设备全部记为超时并结束活动。时间缺失或相对已接受时间倒退时拒绝且不改状态。
// 未到截止时间或活动已结束时为空操作。
func (s *Store) AdvanceCampaign(campaignID string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return err
	}
	c, err := s.lookupCampaign(campaignID)
	if err != nil {
		return err
	}
	if at.IsZero() {
		return ErrInvalidTime
	}
	if at.Before(c.LastTime) {
		return fmt.Errorf("%w: %v before %v", ErrTimeRegression, at, c.LastTime)
	}
	if c.Ended {
		return nil
	}
	if at.Before(c.Deadline) {
		// 仅推进时间基线也持久化，使重开后的倒退判断仍然有效。
		return s.commit(func() error { c.LastTime = at; return nil })
	}
	return s.commit(func() error {
		c.LastTime = at
		s.applyTimeout(c, at)
		return nil
	})
}

func (s *Store) lookupCampaign(campaignID string) (*campaignState, error) {
	if campaignID == "" {
		return nil, ErrInvalidCampaignID
	}
	c, ok := s.campaigns[campaignID]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrCampaignNotFound, campaignID)
	}
	return c, nil
}

func (s *Store) lookupCampaignDevice(campaignID, deviceID string) (*campaignState, *campaignDevice, error) {
	c, err := s.lookupCampaign(campaignID)
	if err != nil {
		return nil, nil, err
	}
	if deviceID == "" {
		return nil, nil, ErrInvalidDeviceID
	}
	i, ok := c.index[deviceID]
	if !ok {
		return nil, nil, fmt.Errorf("%w: %s", ErrDeviceNotInCampaign, deviceID)
	}
	return c, c.Devices[i], nil
}

// DeviceStatusView 是一台设备在活动中的状态快照。
type DeviceStatusView struct {
	DeviceID string
	Batch    int
	// Status 取 Device* 状态常量，含等待回滚、正在回滚、回滚成功、
	// 回滚失败与回滚阶段超时。
	Status string
	// Phase 是设备当前停留或失败的阶段（download/install/rollback）。
	Phase string
	// Reason 是失败、超时或未执行（skipped）的原因；成功时为空。
	Reason string
	// At 是进入当前状态的时间。
	At time.Time
	// DownloadID/InstallID/RollbackID 是各阶段操作的稳定标识。
	DownloadID string
	InstallID  string
	RollbackID string
	// RollbackTarget 是开启回滚时首次领取下载锁定的回滚目标版本；未锁定为空。
	RollbackTarget string
	// InstallFailReason/InstallFailAt 是已接受的安装失败的原因与时间，
	// 进入回滚后仍保留原值，便于与回滚结果分别查看。
	InstallFailReason string
	InstallFailAt     time.Time
	// RollbackResult 表示是否已接受回滚结果。
	RollbackResult bool
	// RollbackSuccess 表示已接受的回滚结果是否成功。
	RollbackSuccess bool
	// RollbackReason/RollbackAt 是回滚结果（成功或失败）的原因与时间。
	RollbackReason string
	RollbackAt     time.Time
}

// BatchView 是一个批次内所有设备的状态，按提交次序排列。
type BatchView struct {
	Index   int
	Devices []DeviceStatusView
}

// CampaignView 是分批升级活动的只读快照。
type CampaignView struct {
	ID            string
	Operator      string
	TargetVersion string
	CreatedAt     time.Time
	BatchSize     int
	WindowStart   time.Time
	WindowEnd     time.Time
	Deadline      time.Time
	// RollbackOnFailure 表示该活动是否开启安装失败回滚。
	RollbackOnFailure bool
	// Status 取 Campaign* 状态常量。
	Status  string
	Ended   bool
	EndedAt time.Time
	// Devices 按提交次序给出每台设备的状态。
	Devices []DeviceStatusView
	// Batches 按批次号分组，保留设备提交次序。
	Batches []BatchView
	// Results 是按接受顺序排列的结果历史（重复提交不产生新记录）。
	Results []ResultRecord
}

// GetCampaign 查询活动：创建者、目标版本、每批设备状态、
// 失败或超时原因及结果历史。返回数据为独立副本。
func (s *Store) GetCampaign(campaignID string) (CampaignView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return CampaignView{}, err
	}
	c, err := s.lookupCampaign(campaignID)
	if err != nil {
		return CampaignView{}, err
	}
	v := CampaignView{
		ID:                c.ID,
		Operator:          c.Operator,
		TargetVersion:     c.Target,
		CreatedAt:         c.CreatedAt,
		BatchSize:         c.BatchSize,
		WindowStart:       c.WindowStart,
		WindowEnd:         c.WindowEnd,
		Deadline:          c.Deadline,
		RollbackOnFailure: c.RollbackOnFailure,
		Status:            c.Status,
		Ended:             c.Ended,
		EndedAt:           c.EndedAt,
		Results:           make([]ResultRecord, len(c.Results)),
	}
	copy(v.Results, c.Results)
	for _, cd := range c.Devices {
		dv := DeviceStatusView{
			DeviceID:       cd.DeviceID,
			Batch:          cd.Batch,
			Status:         cd.Status,
			Phase:          cd.Phase,
			Reason:         cd.Reason,
			At:             cd.At,
			DownloadID:     cd.Download.ID,
			InstallID:      cd.Install.ID,
			RollbackID:     cd.Rollback.ID,
			RollbackTarget: cd.RollbackTarget,
		}
		// 安装失败原因与时间独立保留，进入回滚后也不被回滚结果覆盖。
		if cd.Install.HasResult && !cd.Install.Success {
			dv.InstallFailReason = cd.Install.Reason
			dv.InstallFailAt = cd.Install.At
		}
		// 回滚结果的原因与时间单独呈现。
		if cd.Rollback.HasResult {
			dv.RollbackResult = true
			dv.RollbackSuccess = cd.Rollback.Success
			dv.RollbackReason = cd.Rollback.Reason
			dv.RollbackAt = cd.Rollback.At
		}
		v.Devices = append(v.Devices, dv)
		for len(v.Batches) <= cd.Batch {
			v.Batches = append(v.Batches, BatchView{Index: len(v.Batches)})
		}
		v.Batches[cd.Batch].Devices = append(v.Batches[cd.Batch].Devices, dv)
	}
	return v, nil
}

// DeviceWork 是设备当前版本与待执行操作的查询结果。
type DeviceWork struct {
	DeviceID string
	Version  string
	Online   bool
	// CampaignID 是设备当前所在的未结束活动；没有时为空。
	CampaignID string
	// Pending 是设备仍待完成的操作（至多一个）：已领取未完成或下一阶段可领取。
	// 等待回滚时为尚未领取的回滚操作，其 TargetVersion 给出锁定的回滚目标。
	Pending *Operation
	// PendingClaimed 表示 Pending 是否已领取（未领取的安装或回滚仍须等维护窗口）。
	PendingClaimed bool
}

// GetDeviceWork 查询设备当前版本及其待执行操作。
func (s *Store) GetDeviceWork(deviceID string) (DeviceWork, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return DeviceWork{}, err
	}
	d, err := s.lookup(deviceID)
	if err != nil {
		return DeviceWork{}, err
	}
	w := DeviceWork{DeviceID: deviceID, Version: d.Version, Online: d.Online}
	for id, c := range s.campaigns {
		if c.Ended {
			continue
		}
		i, ok := c.index[deviceID]
		if !ok {
			continue
		}
		cd := c.Devices[i]
		w.CampaignID = id
		if out := outstandingOp(c, cd); out != nil {
			w.Pending = out
			w.PendingClaimed = true
		} else if cd.Status == DeviceAwaitingRollback {
			// 回滚待领取：显示回滚目标，标记为尚未领取。
			w.Pending = &Operation{
				ID:            cd.Rollback.ID,
				CampaignID:    c.ID,
				DeviceID:      deviceID,
				Kind:          StageRollback,
				TargetVersion: cd.RollbackTarget,
			}
			w.PendingClaimed = false
		} else if !isTerminal(cd.Status) {
			kind := StageDownload
			id := cd.Download.ID
			if cd.Download.HasResult {
				kind, id = StageInstall, cd.Install.ID
			}
			w.Pending = &Operation{ID: id, CampaignID: c.ID, DeviceID: deviceID, Kind: kind}
			w.PendingClaimed = false
		}
		break
	}
	return w, nil
}

// ListUpgrades 返回已登记的全部升级目标版本，按目标版本字典序排列。
func (s *Store) ListUpgrades() ([]VersionSpec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	out := make([]VersionSpec, 0, len(s.versions))
	for _, v := range s.versions {
		out = append(out, VersionSpec{Target: v.Target, AllowedFrom: append([]string(nil), v.AllowedFrom...)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Target < out[j].Target })
	return out, nil
}
