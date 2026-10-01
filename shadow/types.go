package shadow

import (
	"encoding/json"
	"time"
)

// Device 是设备影子在某一时刻的快照，返回内容均为副本。
type Device struct {
	// DeviceID 是设备标识。
	DeviceID string
	// Version 是当前版本（最近一次上报携带的版本）。
	Version string
	// Online 表示当前是否在线。
	Online bool
	// Revision 是当前期望修订号。
	Revision int64
	// Desired 是期望配置，始终为 JSON 对象。
	Desired json.RawMessage
	// Reported 是上报配置，始终为 JSON 对象。
	Reported json.RawMessage
}

// DiffValue 描述差异某一侧的值。
type DiffValue struct {
	// Exists 表示该侧是否存在此字段；null 也算存在。
	Exists bool
	// Value 是该侧的值，仅在 Exists 时有意义。
	Value json.RawMessage
}

// Diff 是期望配置与上报配置在某个 JSON Pointer 路径上的差异。
type Diff struct {
	// Path 是 JSON Pointer 路径，根路径为空字符串。
	Path string
	// Desired 是期望侧的值。
	Desired DiffValue
	// Reported 是上报侧的值。
	Reported DiffValue
	// Since 是差异首次出现的时间。
	Since time.Time
}

// AuditRecord 是一次期望配置修改的审计记录。
type AuditRecord struct {
	// DeviceID 是设备标识。
	DeviceID string
	// Operator 是操作者。
	Operator string
	// Time 是操作时间。
	Time time.Time
	// Revision 是修改后的新修订号。
	Revision int64
	// Before 是修改前的完整配置。
	Before json.RawMessage
	// After 是修改后的完整配置。
	After json.RawMessage
}
