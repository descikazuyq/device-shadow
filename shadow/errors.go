package shadow

import "errors"

// 包内可识别的错误。调用者可以用 errors.Is 判断失败原因。
var (
	// ErrClosed 表示存储已关闭。
	ErrClosed = errors.New("shadow: 存储已关闭")
	// ErrCorrupted 表示存储内容损坏，拒绝打开。
	ErrCorrupted = errors.New("shadow: 存储内容损坏")
	// ErrDeviceExists 表示设备已登记。
	ErrDeviceExists = errors.New("shadow: 设备已登记")
	// ErrDeviceNotFound 表示访问了未登记的设备。
	ErrDeviceNotFound = errors.New("shadow: 设备未登记")
	// ErrConflict 表示期望修订号冲突。
	ErrConflict = errors.New("shadow: 期望修订号冲突")
	// ErrInvalidConfig 表示配置不是合法的 JSON 对象。
	ErrInvalidConfig = errors.New("shadow: 配置必须是 JSON 对象")
	// ErrInvalidArgument 表示调用参数无效。
	ErrInvalidArgument = errors.New("shadow: 参数无效")
)
