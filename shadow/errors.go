package shadow

import "errors"

// 设备影子操作的错误。调用方可用 errors.Is 判断。
var (
	// ErrInvalidDeviceID 表示设备标识为空。
	ErrInvalidDeviceID = errors.New("shadow: device id must not be empty")
	// ErrInvalidVersion 表示版本为空。
	ErrInvalidVersion = errors.New("shadow: version must not be empty")
	// ErrInvalidOperator 表示操作者为空。
	ErrInvalidOperator = errors.New("shadow: operator must not be empty")
	// ErrInvalidTime 表示操作时间缺失（零值）。
	ErrInvalidTime = errors.New("shadow: operation time must not be zero")
	// ErrInvalidConfig 表示配置不是合法的 JSON 对象。
	ErrInvalidConfig = errors.New("shadow: config must be a valid JSON object")
	// ErrDeviceExists 表示重复登记同一设备。
	ErrDeviceExists = errors.New("shadow: device already registered")
	// ErrDeviceNotFound 表示访问未登记的设备。
	ErrDeviceNotFound = errors.New("shadow: device not found")
	// ErrRevisionConflict 表示提交的期望修订号与当前值不一致。
	ErrRevisionConflict = errors.New("shadow: desired revision conflict")
	// ErrInvalidSequence 表示上报序号不是正整数。
	ErrInvalidSequence = errors.New("shadow: report sequence must be a positive integer")
	// ErrStaleSequence 表示上报序号小于已接受的序号。
	ErrStaleSequence = errors.New("shadow: report sequence is stale")
	// ErrReportConflict 表示相同序号的上报内容不一致。
	ErrReportConflict = errors.New("shadow: report conflicts with accepted sequence")
	// ErrCorruptStorage 表示已有存储内容损坏，拒绝打开。
	ErrCorruptStorage = errors.New("shadow: storage is corrupt")
	// ErrClosed 表示存储已关闭。
	ErrClosed = errors.New("shadow: store is closed")
	// ErrUpgradeExists 表示目标版本已登记，原记录保留。
	ErrUpgradeExists = errors.New("shadow: upgrade target version already registered")
	// ErrUpgradeNotFound 表示目标版本未登记。
	ErrUpgradeNotFound = errors.New("shadow: upgrade target version not registered")
	// ErrUpgradeListEmpty 表示允许升级的当前版本列表为空。
	ErrUpgradeListEmpty = errors.New("shadow: allowed versions list must not be empty")
	// ErrUpgradeTargetListed 表示目标版本出现在允许列表中。
	ErrUpgradeTargetListed = errors.New("shadow: target version must not be in allowed list")
	// ErrActivityExists 表示活动标识已存在。
	ErrActivityExists = errors.New("shadow: activity already exists")
	// ErrActivityNotFound 表示活动不存在。
	ErrActivityNotFound = errors.New("shadow: activity not found")
	// ErrInvalidActivityID 表示活动标识为空。
	ErrInvalidActivityID = errors.New("shadow: activity id must not be empty")
	// ErrEmptyDeviceList 表示活动设备列表为空。
	ErrEmptyDeviceList = errors.New("shadow: device list must not be empty")
	// ErrDuplicateDevice 表示活动设备列表中存在重复设备。
	ErrDuplicateDevice = errors.New("shadow: duplicate device in activity")
	// ErrInvalidBatchSize 表示批大小不是正整数。
	ErrInvalidBatchSize = errors.New("shadow: batch size must be a positive integer")
	// ErrInvalidWindow 表示维护窗口开始时刻不早于结束时刻。
	ErrInvalidWindow = errors.New("shadow: maintenance window start must be before end")
	// ErrInvalidDeadline 表示截止时刻不晚于创建时刻。
	ErrInvalidDeadline = errors.New("shadow: deadline must be after creation time")
	// ErrDeviceInActivity 表示设备已参加未结束的活动。
	ErrDeviceInActivity = errors.New("shadow: device already in an unfinished activity")
	// ErrDeviceIncompatible 表示设备当前版本与目标版本不兼容。
	ErrDeviceIncompatible = errors.New("shadow: device version is not compatible with upgrade")
	// ErrInvalidOperation 表示操作标识无效或设备无待执行操作。
	ErrInvalidOperation = errors.New("shadow: invalid operation")
	// ErrOperationConflict 表示操作结果与已接受的结果冲突。
	ErrOperationConflict = errors.New("shadow: operation result conflict")
	// ErrActivityEnded 表示活动已结束，不再接受新结果。
	ErrActivityEnded = errors.New("shadow: activity has ended")
	// ErrTimeBackwards 表示同一活动的当前时间倒退。
	ErrTimeBackwards = errors.New("shadow: activity time must not go backwards")
	// ErrDeviceNotInActivity 表示设备不在该活动中。
	ErrDeviceNotInActivity = errors.New("shadow: device not in activity")
)
