package shadow

import (
	"encoding/json"
	"fmt"
	"time"
)

// reportInput 是一次设备上报的统一输入：普通上报与安装/回滚成功附带的
// 上报共用同一结构，保证配置校验、序号判断与影子写入规则只有一处维护。
type reportInput struct {
	DeviceID string
	Seq      uint64
	At       time.Time
	Version  string
	Config   json.RawMessage
}

// reportVerdict 是一次上报按序号规则判定的结果。
type reportVerdict int

const (
	// reportAccept 表示序号更大、内容有效，必须写入影子。
	reportAccept reportVerdict = iota
	// reportDuplicate 表示相同序号且版本、发生时间、配置均一致：
	// 成功返回但不重新写入影子，尤其不改变在线状态与差异首次出现时间。
	reportDuplicate
)

// checkReport 按唯一的一套规则校验上报并给出判定：必填字段（正整数序号、
// 时间、非空版本、完整 JSON 对象配置）、可选的版本必须等于约束（安装为
// 活动目标、回滚为锁定回滚目标；空串表示不约束），以及相对已接受序号的
// 过旧/重复/冲突判断。只校验、不改状态，返回的 cfg 是配置的独立副本。
func checkReport(d *deviceState, in reportInput, requireVersion string) (cfg json.RawMessage, verdict reportVerdict, err error) {
	if in.Seq == 0 {
		return nil, 0, errBadReport{kind: reportErrSequence}
	}
	if in.At.IsZero() {
		return nil, 0, errBadReport{kind: reportErrTime}
	}
	if in.Version == "" {
		return nil, 0, errBadReport{kind: reportErrVersion}
	}
	if requireVersion != "" && in.Version != requireVersion {
		return nil, 0, errBadReport{kind: reportErrVersion}
	}
	cfg, cerr := validateConfig(in.Config)
	if cerr != nil {
		return nil, 0, errBadReport{kind: reportErrConfig}
	}
	switch {
	case in.Seq < d.LastSeq:
		return nil, 0, errBadReport{kind: reportErrStale, lastSeq: d.LastSeq, seq: in.Seq}
	case in.Seq == d.LastSeq:
		if in.Version == d.Version && in.At.Equal(d.LastReportTime) && rawEqual(cfg, d.Reported) {
			return cfg, reportDuplicate, nil
		}
		return nil, 0, errBadReport{kind: reportErrConflict, seq: in.Seq}
	}
	return cfg, reportAccept, nil
}

// applyReport 把一条已通过 checkReport 的上报写入影子（调用方持有 s.mu 且
// 位于同一次 commit 内）。重复上报原样保留影子：不重新写入、不把已被标为
// 离线的设备重新置为在线，也不刷新任何差异的首次出现时间。
func applyReport(d *deviceState, in reportInput, cfg json.RawMessage, verdict reportVerdict, at time.Time) {
	if verdict == reportDuplicate {
		return
	}
	d.Reported = cfg
	d.Version = in.Version
	d.Online = true
	d.LastSeq = in.Seq
	d.LastReportTime = at
	d.refreshDiff(at)
}

// 统一规则在校验阶段返回的错误类别；由各入口映射回自己公开的错误哨兵，
// 使普通上报与安装/回滚附带上报保持各自既有的错误类别。
type reportErrKind int

const (
	reportErrSequence reportErrKind = iota
	reportErrTime
	reportErrVersion
	reportErrConfig
	reportErrStale
	reportErrConflict
)

// errBadReport 携带统一规则判定出的错误类别。普通上报经 reportPlainError
// 映射为 ErrInvalidSequence 等；附带上报一律映射为 ErrInvalidReport。
type errBadReport struct {
	kind    reportErrKind
	lastSeq uint64
	seq     uint64
}

func (e errBadReport) Error() string {
	return reportPlainError(e).Error()
}

// reportPlainError 把统一规则的错误类别映射为普通 Report 公开的错误，
// 消息与既有实现保持一致。
func reportPlainError(e errBadReport) error {
	switch e.kind {
	case reportErrSequence:
		return ErrInvalidSequence
	case reportErrTime:
		return ErrInvalidTime
	case reportErrVersion:
		return ErrInvalidVersion
	case reportErrConfig:
		return ErrInvalidConfig
	case reportErrStale:
		return fmt.Errorf("%w: last accepted %d, got %d", ErrStaleSequence, e.lastSeq, e.seq)
	case reportErrConflict:
		return fmt.Errorf("%w: sequence %d", ErrReportConflict, e.seq)
	default:
		return ErrInvalidReport
	}
}

// reportAttachedError 把统一规则的错误类别映射为安装/回滚成功附带上报的
// 错误：任何不合规都返回 ErrInvalidReport，不改成普通上报的错误类别。
func reportAttachedError(e errBadReport) error {
	switch e.kind {
	case reportErrSequence:
		return fmt.Errorf("%w: report sequence", ErrInvalidReport)
	case reportErrTime:
		return fmt.Errorf("%w: report time", ErrInvalidReport)
	case reportErrVersion:
		return fmt.Errorf("%w: report version", ErrInvalidReport)
	case reportErrConfig:
		return fmt.Errorf("%w: config", ErrInvalidReport)
	case reportErrStale:
		return fmt.Errorf("%w: stale sequence", ErrInvalidReport)
	case reportErrConflict:
		return fmt.Errorf("%w: sequence conflict", ErrInvalidReport)
	default:
		return ErrInvalidReport
	}
}
