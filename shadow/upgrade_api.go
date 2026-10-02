package shadow

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// RegisterUpgrade 登记一个目标版本及允许升级的当前版本列表。
// 版本字符串精确匹配；列表不能为空且不能包含目标版本；
// 目标版本已登记时报错且保留原记录。
func (s *Store) RegisterUpgrade(target string, allowed []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return err
	}
	if target == "" {
		return ErrInvalidVersion
	}
	if len(allowed) == 0 {
		return ErrUpgradeListEmpty
	}
	seen := make(map[string]bool, len(allowed))
	for _, v := range allowed {
		if v == "" {
			return ErrInvalidVersion
		}
		seen[v] = true
	}
	if seen[target] {
		return ErrUpgradeTargetListed
	}
	if _, ok := s.upgrades[target]; ok {
		return fmt.Errorf("%w: %s", ErrUpgradeExists, target)
	}
	allowedCopy := append([]string(nil), allowed...)
	return s.commit(func() error {
		s.upgrades[target] = &upgradeRecord{target: target, allowed: allowedCopy}
		return nil
	})
}

// GetUpgrade 返回目标版本允许升级的当前版本列表副本。
func (s *Store) GetUpgrade(target string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return nil, err
	}
	rec, ok := s.upgrades[target]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUpgradeNotFound, target)
	}
	return append([]string(nil), rec.allowed...), nil
}

// CreateActivity 创建一次分批升级活动。
// 需提供唯一标识、目标版本、操作者、创建时间、有序设备列表、正整数批大小、
// 维护窗口（开始早于结束）和截止时间（晚于创建时间）。
// 设备或版本未登记、设备版本不兼容、设备已参加未结束活动时整次失败，
// 不留下任何记录。
func (s *Store) CreateActivity(id, target, operator string, createdAt time.Time,
	deviceIDs []string, batchSize int, windowStart, windowEnd, deadline time.Time) (ActivityView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return ActivityView{}, err
	}
	if id == "" {
		return ActivityView{}, ErrInvalidActivityID
	}
	if operator == "" {
		return ActivityView{}, ErrInvalidOperator
	}
	if createdAt.IsZero() {
		return ActivityView{}, ErrInvalidTime
	}
	if len(deviceIDs) == 0 {
		return ActivityView{}, ErrEmptyDeviceList
	}
	if batchSize <= 0 {
		return ActivityView{}, ErrInvalidBatchSize
	}
	if !windowStart.Before(windowEnd) {
		return ActivityView{}, ErrInvalidWindow
	}
	if !deadline.After(createdAt) {
		return ActivityView{}, ErrInvalidDeadline
	}
	rec, ok := s.upgrades[target]
	if !ok {
		return ActivityView{}, fmt.Errorf("%w: %s", ErrUpgradeNotFound, target)
	}
	seen := make(map[string]bool, len(deviceIDs))
	for _, d := range deviceIDs {
		if d == "" {
			return ActivityView{}, ErrInvalidDeviceID
		}
		if seen[d] {
			return ActivityView{}, ErrDuplicateDevice
		}
		seen[d] = true
	}
	devs := make([]*activityDevice, 0, len(deviceIDs))
	for _, devID := range deviceIDs {
		d, err := s.lookup(devID)
		if err != nil {
			return ActivityView{}, err
		}
		if !containsString(rec.allowed, d.Version) {
			return ActivityView{}, fmt.Errorf("%w: %s (current %s)", ErrDeviceIncompatible, devID, d.Version)
		}
		if s.deviceInActiveActivity(devID) {
			return ActivityView{}, fmt.Errorf("%w: %s", ErrDeviceInActivity, devID)
		}
		devs = append(devs, &activityDevice{
			deviceID: devID,
			batch:    len(devs) / batchSize,
			status:   DevicePending,
		})
	}
	if _, ok := s.activities[id]; ok {
		return ActivityView{}, fmt.Errorf("%w: %s", ErrActivityExists, id)
	}
	a := &activity{
		id:          id,
		operator:    operator,
		createdAt:   createdAt,
		target:      target,
		windowStart: windowStart,
		windowEnd:   windowEnd,
		deadline:    deadline,
		batchSize:   batchSize,
		lastTime:    createdAt,
		status:      ActivityActive,
		devices:     devs,
	}
	if err := s.commit(func() error {
		s.activities[id] = a
		return nil
	}); err != nil {
		return ActivityView{}, err
	}
	return s.activityView(a), nil
}

// Advance 调用方传入当前时间推进活动：为在线且处于维护窗口内的设备
// 派发新的下载或安装操作。时间缺失或当前时间倒退时拒绝且不改状态；
// 达到截止时间时所有未结束设备记为超时，活动失败结束，不再派发操作。
func (s *Store) Advance(activityID string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return err
	}
	a, err := s.lookupActivity(activityID)
	if err != nil {
		return err
	}
	if now.IsZero() {
		return ErrInvalidTime
	}
	if now.Before(a.lastTime) {
		return fmt.Errorf("%w: activity %s", ErrTimeBackwards, activityID)
	}
	if a.status != ActivityActive {
		return nil
	}
	a.lastTime = now
	if !now.Before(a.deadline) {
		return s.commit(func() error {
			s.timeoutAll(a, now)
			a.status = ActivityFailed
			return nil
		})
	}
	rec := s.upgrades[a.target]
	released := a.batchesReleased()
	mutated := false
	for _, d := range a.devices {
		if d.batch >= released {
			continue
		}
		sd, err := s.lookup(d.deviceID)
		if err != nil {
			continue
		}
		switch d.status {
		case DevicePending:
			// 首次领取下载前再次核对当前版本，不兼容则该设备失败。
			if !containsString(rec.allowed, sd.Version) {
				s.failDevice(a, d, StageDownload, "incompatible", now)
				mutated = true
				continue
			}
			if sd.Online && inWindow(now, a.windowStart, a.windowEnd) {
				d.status = DeviceDownloading
				d.opID = opID(a.id, d.deviceID, StageDownload)
				mutated = true
			}
		case DeviceDownloaded:
			if sd.Online && inWindow(now, a.windowStart, a.windowEnd) {
				d.status = DeviceInstalling
				d.opID = opID(a.id, d.deviceID, StageInstall)
				mutated = true
			}
		}
	}
	if !mutated {
		return nil
	}
	s.checkActivityEnd(a)
	return s.commit(func() error { return nil })
}

// ReportInput 是安装成功时附带的设备上报，须符合影子上报规则。
type ReportInput struct {
	Seq     uint64
	Version string
	Config  json.RawMessage
}

// ReportResult 提交一次操作结果。结果须带活动、设备、操作标识和发生时间。
// stage 为操作阶段（download/install），须与操作标识一致。
// 相同操作的相同结果重复提交成功返回，不增加历史；不同结果、跳过阶段、
// 提交其他设备的操作或活动结束后的新结果均报错且不改状态。
// 安装成功须附带版本等于目标版本的设备上报，影子与活动同时更新。
func (s *Store) ReportResult(activityID, deviceID, opID string, at time.Time,
	success bool, reason, stage string, report *ReportInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return err
	}
	a, err := s.lookupActivity(activityID)
	if err != nil {
		return err
	}
	if stage != StageDownload && stage != StageInstall {
		return fmt.Errorf("%w: stage %s", ErrInvalidOperation, stage)
	}
	if !strings.HasSuffix(opID, "/"+stage) {
		return fmt.Errorf("%w: op %s does not match stage %s", ErrInvalidOperation, opID, stage)
	}
	d := a.findDevice(deviceID)
	if d == nil {
		return fmt.Errorf("%w: %s", ErrDeviceNotInActivity, deviceID)
	}
	if at.IsZero() {
		return ErrInvalidTime
	}

	prev := a.findResult(opID)
	duplicate := prev != nil && prev.deviceID == deviceID && prev.success == success && prev.reason == reason

	// 活动已结束：重复结果幂等返回，其余新结果报错。
	if a.status != ActivityActive {
		if duplicate {
			return nil
		}
		return ErrActivityEnded
	}

	// 活动进行中：时间不得倒退（含重复结果）。
	if at.Before(a.lastTime) {
		return fmt.Errorf("%w: activity %s", ErrTimeBackwards, activityID)
	}
	if duplicate {
		return nil
	}

	// 新结果：达到截止时间则所有未结束设备超时，活动失败结束。
	deadlineReached := !at.Before(a.deadline)
	if deadlineReached {
		s.timeoutAll(a, at)
		a.status = ActivityFailed
		a.lastTime = at
		if err := s.commit(func() error { return nil }); err != nil {
			return err
		}
		return ErrActivityEnded
	}

	conflict := prev != nil && !(prev.success == success && prev.reason == reason)
	if conflict {
		return ErrOperationConflict
	}
	if isTerminal(d.status) {
		return ErrOperationConflict
	}
	if d.opID != opID {
		return fmt.Errorf("%w: op %s (current %s)", ErrInvalidOperation, opID, d.opID)
	}
	// 阶段核对：设备必须处于该阶段对应的状态。
	if stage == StageDownload && d.status != DeviceDownloading {
		return fmt.Errorf("%w: device %s is not downloading", ErrInvalidOperation, deviceID)
	}
	if stage == StageInstall && d.status != DeviceInstalling {
		return fmt.Errorf("%w: device %s is not installing", ErrInvalidOperation, deviceID)
	}

	switch stage {
	case StageDownload:
		if success {
			d.status = DeviceDownloaded
			d.opID = ""
		} else {
			s.failDevice(a, d, StageDownload, reason, at)
		}
		a.history = append(a.history, &historyEntry{
			deviceID: deviceID, opID: opID, stage: StageDownload,
			success: success, reason: reason, time: at,
		})
	case StageInstall:
		if success {
			if report == nil {
				return ErrInvalidOperation
			}
			if report.Seq == 0 {
				return ErrInvalidSequence
			}
			if report.Version != a.target {
				return fmt.Errorf("%w: reported %s, target %s", ErrDeviceIncompatible, report.Version, a.target)
			}
			cfg, err := validateConfig(report.Config)
			if err != nil {
				return err
			}
			sd, err := s.lookup(deviceID)
			if err != nil {
				return err
			}
			if err := applyReport(sd, report.Seq, at, a.target, cfg); err != nil {
				return err
			}
			d.status = DeviceSucceeded
			d.opID = ""
			a.history = append(a.history, &historyEntry{
				deviceID: deviceID, opID: opID, stage: StageInstall,
				success: true, time: at, seq: report.Seq,
			})
		} else {
			s.failDevice(a, d, StageInstall, reason, at)
			a.history = append(a.history, &historyEntry{
				deviceID: deviceID, opID: opID, stage: StageInstall,
				success: false, reason: reason, time: at,
			})
		}
	}

	a.lastTime = at
	s.checkActivityEnd(a)
	return s.commit(func() error { return nil })
}

// ReportDownload 提交下载操作结果。
func (s *Store) ReportDownload(activityID, deviceID, opID string, at time.Time, success bool, reason string) error {
	return s.ReportResult(activityID, deviceID, opID, at, success, reason, StageDownload, nil)
}

// ReportInstall 提交安装操作结果；成功时须附带设备上报。
func (s *Store) ReportInstall(activityID, deviceID, opID string, at time.Time, success bool, reason string, report ReportInput) error {
	if !success {
		return s.ReportResult(activityID, deviceID, opID, at, false, reason, StageInstall, nil)
	}
	return s.ReportResult(activityID, deviceID, opID, at, true, reason, StageInstall, &report)
}

// DeviceStatus 是活动内单台设备状态的只读快照。
type DeviceStatus struct {
	DeviceID   string
	Status     string
	OpID       string
	Stage      string
	FailStage  string
	FailReason string
	FailTime   time.Time
}

// HistoryEntryView 是一条结果历史的只读快照。
type HistoryEntryView struct {
	DeviceID string
	OpID     string
	Stage    string
	Success  bool
	Reason   string
	Seq      uint64
	Time     time.Time
}

// ActivityView 是活动状态的只读快照。
type ActivityView struct {
	ID          string
	Operator    string
	CreatedAt   time.Time
	Target      string
	Status      string
	WindowStart time.Time
	WindowEnd   time.Time
	Deadline    time.Time
	BatchSize   int
	Batches     [][]DeviceStatus
	History     []HistoryEntryView
}

func (s *Store) deviceStatusView(d *activityDevice) DeviceStatus {
	return DeviceStatus{
		DeviceID:   d.deviceID,
		Status:     d.status,
		OpID:       d.opID,
		Stage:      d.currentStage(),
		FailStage:  d.failStage,
		FailReason: d.failReason,
		FailTime:   d.failTime,
	}
}

func (s *Store) activityView(a *activity) ActivityView {
	n := a.numBatches()
	batches := make([][]DeviceStatus, n)
	for _, d := range a.devices {
		batches[d.batch] = append(batches[d.batch], s.deviceStatusView(d))
	}
	hist := make([]HistoryEntryView, len(a.history))
	for i, h := range a.history {
		hist[i] = HistoryEntryView{
			DeviceID: h.deviceID,
			OpID:     h.opID,
			Stage:    h.stage,
			Success:  h.success,
			Reason:   h.reason,
			Seq:      h.seq,
			Time:     h.time,
		}
	}
	return ActivityView{
		ID:          a.id,
		Operator:    a.operator,
		CreatedAt:   a.createdAt,
		Target:      a.target,
		Status:      a.status,
		WindowStart: a.windowStart,
		WindowEnd:   a.windowEnd,
		Deadline:    a.deadline,
		BatchSize:   a.batchSize,
		Batches:     batches,
		History:     hist,
	}
}

// GetActivity 返回活动状态的只读快照：创建者、目标版本、每批设备状态、
// 失败或超时原因和结果历史。
func (s *Store) GetActivity(activityID string) (ActivityView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return ActivityView{}, err
	}
	a, err := s.lookupActivity(activityID)
	if err != nil {
		return ActivityView{}, err
	}
	return s.activityView(a), nil
}

// DeviceUpgradeInfo 是设备升级状态的只读快照。
type DeviceUpgradeInfo struct {
	DeviceID   string
	Version    string
	ActivityID string
	Status     string
	OpID       string
	Stage      string
}

// GetDeviceUpgrade 返回设备当前版本及待执行操作。
func (s *Store) GetDeviceUpgrade(deviceID string) (DeviceUpgradeInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.checkOpen(); err != nil {
		return DeviceUpgradeInfo{}, err
	}
	sd, err := s.lookup(deviceID)
	if err != nil {
		return DeviceUpgradeInfo{}, err
	}
	info := DeviceUpgradeInfo{DeviceID: deviceID, Version: sd.Version}
	var best *activity
	for _, a := range s.activities {
		for _, d := range a.devices {
			if d.deviceID == deviceID {
				if a.status == ActivityActive {
					best = a
					break
				}
				if best == nil || a.createdAt.After(best.createdAt) {
					best = a
				}
			}
		}
		if best != nil && best.status == ActivityActive {
			break
		}
	}
	if best != nil {
		for _, d := range best.devices {
			if d.deviceID == deviceID {
				info.ActivityID = best.id
				info.Status = d.status
				info.OpID = d.opID
				info.Stage = d.currentStage()
				break
			}
		}
	}
	return info, nil
}
