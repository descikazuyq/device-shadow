package shadow

import (
	"encoding/json"
	"time"
)

// desiredChange 是一次期望配置修改的统一内容：操作者、提交时间、
// 完整替换用的新配置以及来源请求标识（单设备修改为空，批量修改为
// 整批共用的请求标识）。
type desiredChange struct {
	Operator  string
	At        time.Time
	Config    json.RawMessage
	RequestID string
}

// applyDesiredChange 按唯一的一套规则把一次已校验的修改写入设备影子
// （调用方持有 s.mu 且位于同一次 commit 内）：完整替换期望配置，
// 修订号加一，追加记录操作者、提交时间、新修订号、请求标识及修改
// 前后完整配置的审计，并按提交时间刷新差异首次出现时间。单设备修改
// 与批量修改都经此函数写入，两种方式交替使用时审计仍按修订号连续
// 排列、修改前后的配置自然衔接。离线、版本、上报配置与升级待办均不
// 触碰。返回本次修改产生的新修订号。
func applyDesiredChange(d *deviceState, deviceID string, ch desiredChange) uint64 {
	before := d.Desired
	d.Desired = ch.Config
	d.Revision++
	d.Audit = append(d.Audit, AuditRecord{
		DeviceID:  deviceID,
		Operator:  ch.Operator,
		Time:      ch.At,
		Revision:  d.Revision,
		RequestID: ch.RequestID,
		Before:    cloneRaw(before),
		After:     cloneRaw(ch.Config),
	})
	d.refreshDiff(ch.At)
	return d.Revision
}
