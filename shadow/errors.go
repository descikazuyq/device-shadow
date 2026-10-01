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
)
