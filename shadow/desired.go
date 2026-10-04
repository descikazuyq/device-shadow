package shadow

import (
	"encoding/json"
	"fmt"
	"time"
)

// 本文件集中维护修改期望配置的唯一一套业务规则，单台修改（UpdateDesired）
// 与批量修改（BatchUpdateDesired）共用，避免两处各自维护配置替换、修订号
// 递增、审计记录和差异时间更新。各入口只保留自己的参数解析、校验次序与
// 错误消息格式。

// checkDesiredMeta 校验两种修改入口共用的请求级字段：操作者非空、时间非零。
func checkDesiredMeta(operator string, at time.Time) error {
	if operator == "" {
		return ErrInvalidOperator
	}
	if at.IsZero() {
		return ErrInvalidTime
	}
	return nil
}

// checkDesiredRevision 校验调用方已读取的修订号等于设备当前修订号。
// 只校验、不改状态；失败时返回携带细节的 errDesiredRevision，由各入口
// 映射为自己公开格式的 ErrRevisionConflict。
func checkDesiredRevision(d *deviceState, deviceID string, revision uint64) error {
	if revision != d.Revision {
		return errDesiredRevision{deviceID: deviceID, current: d.Revision, given: revision}
	}
	return nil
}

// applyDesired 把一次已通过校验的修改写入影子（调用方持有 s.mu 且位于同一
// 次 commit 内）：完整替换期望配置（即使与当前相同也产生新修订）、修订号
// 加一、追加一条审计（含操作者、提交时间、新修订号、批量请求标识及修改前
// 后的完整配置，均为独立副本），并按提交时间刷新差异首次出现时间。
// requestID 在单台修改时为空。返回新修订号。
func applyDesired(d *deviceState, deviceID, operator string, at time.Time, requestID string, cfg json.RawMessage) uint64 {
	before := d.Desired
	d.Desired = cfg
	d.Revision++
	d.Audit = append(d.Audit, AuditRecord{
		DeviceID:  deviceID,
		Operator:  operator,
		Time:      at,
		Revision:  d.Revision,
		RequestID: requestID,
		Before:    cloneRaw(before),
		After:     cloneRaw(cfg),
	})
	d.refreshDiff(at)
	return d.Revision
}

// errDesiredRevision 携带统一规则判定出的修订号冲突细节。单台修改经
// desiredSingleError 映射；批量修改经 desiredBatchError 映射并带上设备标识。
type errDesiredRevision struct {
	deviceID string
	current  uint64 // 设备当前修订号
	given    uint64 // 调用方提交的已读取修订号
}

func (e errDesiredRevision) Error() string {
	return desiredSingleError(e).Error()
}

// desiredSingleError 把统一规则的冲突细节映射为单台修改公开的错误，
// 消息与既有实现保持一致。
func desiredSingleError(e errDesiredRevision) error {
	return fmt.Errorf("%w: want %d, got %d", ErrRevisionConflict, e.current, e.given)
}

// desiredBatchError 把统一规则的冲突细节映射为批量修改公开的错误，
// 消息与既有实现保持一致。
func desiredBatchError(e errDesiredRevision) error {
	return fmt.Errorf("%w: device %s want %d, got %d", ErrRevisionConflict, e.deviceID, e.current, e.given)
}
