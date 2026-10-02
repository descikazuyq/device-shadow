package shadow

import (
	"fmt"
	"time"
)

// 设备升级状态。
const (
	// DevicePending 表示设备尚未领取任何操作。
	DevicePending = "pending"
	// DeviceDownloading 表示设备已领取下载操作。
	DeviceDownloading = "downloading"
	// DeviceDownloaded 表示设备已完成下载，等待领取安装。
	DeviceDownloaded = "downloaded"
	// DeviceInstalling 表示设备已领取安装操作。
	DeviceInstalling = "installing"
	// DeviceSucceeded 表示设备升级成功（终态）。
	DeviceSucceeded = "succeeded"
	// DeviceFailed 表示设备升级失败（终态）。
	DeviceFailed = "failed"
	// DeviceTimeout 表示设备在截止时刻前未完成，超时（终态）。
	DeviceTimeout = "timeout"
	// DeviceSkipped 表示因前序批次失败而未执行（终态）。
	DeviceSkipped = "skipped"
)

// 活动状态。
const (
	// ActivityActive 表示活动进行中。
	ActivityActive = "active"
	// ActivitySucceeded 表示活动全部设备成功。
	ActivitySucceeded = "succeeded"
	// ActivityFailed 表示活动失败（含超时）。
	ActivityFailed = "failed"
)

// 操作阶段。
const (
	StageDownload = "download"
	StageInstall  = "install"
)

// upgradeRecord 是一个目标版本的升级兼容记录。
type upgradeRecord struct {
	target  string
	allowed []string
}

// activityDevice 是活动内单台设备的状态。
type activityDevice struct {
	deviceID   string
	batch      int
	status     string
	opID       string
	failStage  string
	failReason string
	failTime   time.Time
}

// currentStage 返回设备当前所处的操作阶段，用于超时记录与查询展示。
func (d *activityDevice) currentStage() string {
	switch d.status {
	case DeviceDownloading:
		return StageDownload
	case DeviceInstalling:
		return StageInstall
	case DeviceDownloaded:
		return StageInstall
	case DevicePending:
		return StageDownload
	default:
		return ""
	}
}

// historyEntry 是一条结果历史。
type historyEntry struct {
	deviceID string
	opID     string
	stage    string
	success  bool
	reason   string
	seq      uint64
	time     time.Time
}

// activity 是一次分批升级活动。
type activity struct {
	id          string
	operator    string
	createdAt   time.Time
	target      string
	windowStart time.Time
	windowEnd   time.Time
	deadline    time.Time
	batchSize   int
	lastTime    time.Time
	status      string
	devices     []*activityDevice
	history     []*historyEntry
}

func (a *activity) findDevice(deviceID string) *activityDevice {
	for _, d := range a.devices {
		if d.deviceID == deviceID {
			return d
		}
	}
	return nil
}

// findResult 返回操作 opID 已接受的结果历史；每个操作至多一条结果。
func (a *activity) findResult(opID string) *historyEntry {
	for _, h := range a.history {
		if h.opID == opID {
			return h
		}
	}
	return nil
}

func (a *activity) numBatches() int {
	return (len(a.devices) + a.batchSize - 1) / a.batchSize
}

// batchesReleased 返回当前已放行的批数：第 0 批创建时放行，
// 之后每一批在前一批全部成功后放行。
func (a *activity) batchesReleased() int {
	n := a.numBatches()
	released := 1
	for b := 0; b < n; b++ {
		if b >= released {
			break
		}
		allSucceeded := true
		for _, d := range a.devices {
			if d.batch == b && d.status != DeviceSucceeded {
				allSucceeded = false
				break
			}
		}
		if allSucceeded {
			released++
		}
	}
	if released > n {
		released = n
	}
	return released
}

// isTerminal 判断设备状态是否为终态。
func isTerminal(status string) bool {
	switch status {
	case DeviceSucceeded, DeviceFailed, DeviceTimeout, DeviceSkipped:
		return true
	}
	return false
}

// opID 生成稳定的操作标识：同一活动、设备、阶段唯一，重开后不变。
func opID(activityID, deviceID, stage string) string {
	return activityID + "/" + deviceID + "/" + stage
}

// inWindow 判断时刻 t 是否落在维护窗口内。窗口以 [start, end) 为区间，
// 每 24 小时重复一次；t 早于首次开始时刻返回 false。
func inWindow(t, start, end time.Time) bool {
	if t.Before(start) {
		return false
	}
	span := end.Sub(start)
	if span <= 0 || span >= 24*time.Hour {
		// 窗口跨度非法或不短于一天时退化为单次区间。
		return !t.Before(start) && t.Before(end)
	}
	offset := t.Sub(start) % (24 * time.Hour)
	return offset >= 0 && offset < span
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// failDevice 将设备记为失败（阶段、原因、时间），并把后续批次全部记为未执行。
func (s *Store) failDevice(a *activity, d *activityDevice, stage, reason string, t time.Time) {
	d.status = DeviceFailed
	d.failStage = stage
	d.failReason = reason
	d.failTime = t
	d.opID = ""
	for _, o := range a.devices {
		if o.batch > d.batch && !isTerminal(o.status) {
			o.status = DeviceSkipped
		}
	}
}

// timeoutAll 将所有未结束的设备记为超时。
func (s *Store) timeoutAll(a *activity, t time.Time) {
	for _, d := range a.devices {
		if !isTerminal(d.status) {
			d.failStage = d.currentStage()
			d.failReason = "timeout"
			d.failTime = t
			d.opID = ""
			d.status = DeviceTimeout
		}
	}
}

// checkActivityEnd 在全部设备到达终态时结束活动：全部成功才成功，否则失败。
func (s *Store) checkActivityEnd(a *activity) {
	for _, d := range a.devices {
		if !isTerminal(d.status) {
			return
		}
	}
	allOK := true
	for _, d := range a.devices {
		if d.status != DeviceSucceeded {
			allOK = false
			break
		}
	}
	if allOK {
		a.status = ActivitySucceeded
	} else {
		a.status = ActivityFailed
	}
}

// lookupActivity 按标识查找活动。
func (s *Store) lookupActivity(id string) (*activity, error) {
	if id == "" {
		return nil, ErrInvalidActivityID
	}
	a, ok := s.activities[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrActivityNotFound, id)
	}
	return a, nil
}

// deviceInActiveActivity 判断设备是否已参加未结束的活动。
func (s *Store) deviceInActiveActivity(deviceID string) bool {
	for _, a := range s.activities {
		if a.status != ActivityActive {
			continue
		}
		for _, d := range a.devices {
			if d.deviceID == deviceID {
				return true
			}
		}
	}
	return false
}
