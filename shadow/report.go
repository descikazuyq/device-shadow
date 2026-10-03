package shadow

import (
	"encoding/json"
	"fmt"
	"time"
)

// reportOutcome 是按影子已接受的最后一条上报对新上报作出的判定。
type reportOutcome int

const (
	// reportAccept 表示序号更大，上报有效，调用方应把它写入影子。
	reportAccept reportOutcome = iota
	// reportDuplicate 表示同序号且版本、发生时间、配置完全一致，
	// 按重复处理：成功返回但不重新写入影子。
	reportDuplicate
	// reportStale 表示序号比已接受的更小。
	reportStale
	// reportConflict 表示同序号但版本、发生时间或配置不一致。
	reportConflict
)

// classifyReport 依据影子当前状态判定一条上报的序号结果。
// cfg 必须是已经通过 validateConfig 校验的完整 JSON 对象。
// 普通上报、安装成功与回滚成功的附带上报共用这一套序号与重复判断，
// 只在各自入口把判定映射为不同的错误类别。
func (d *deviceState) classifyReport(seq uint64, at time.Time, version string, cfg json.RawMessage) reportOutcome {
	switch {
	case seq < d.LastSeq:
		return reportStale
	case seq > d.LastSeq:
		return reportAccept
	default:
		// 同一序号只有在版本、发生时间与配置都一致时才算重复：
		// 时间按同一时刻判断，配置按 JSON 语义比较（忽略字段顺序与空白、
		// 数字按数值比较，数组有序，字段缺失与 null 有别）。
		if version == d.Version && at.Equal(d.LastReportTime) && rawEqual(cfg, d.Reported) {
			return reportDuplicate
		}
		return reportConflict
	}
}

// applyReport 把一条更大序号的有效上报写入影子：更新当前版本、上报配置、
// 序号与发生时间，标为在线，并按 at 重算配置差异（持续存在的差异保留首次
// 出现时间）。期望配置、期望修订号与审计不受影响。
func (d *deviceState) applyReport(seq uint64, at time.Time, version string, cfg json.RawMessage) {
	d.Reported = cfg
	d.Version = version
	d.Online = true
	d.LastSeq = seq
	d.LastReportTime = at
	d.refreshDiff(at)
}

// validateAttachedReport 校验安装/回滚成功附带的设备上报。
//
// 字段要求与序号/重复判断沿用普通上报规则（共用 validateConfig 与
// classifyReport），另有版本约束：安装必须等于活动目标版本，回滚必须等于
// 首次领取下载时锁定的回滚目标（wantVersion）。与普通上报不同，任何不合规
// 都统一归为 ErrInvalidReport，不能改成普通上报各自的错误类别。
// 发生时间缺失由 SubmitResult 在更早处按 ErrInvalidTime 拒绝。
// 返回 reportAccept 时调用方须在同一次写入中对影子 applyReport；
// 返回 reportDuplicate 时操作照常完成，但不得重新写入影子（不刷新在线
// 状态与差异时间）。
func (s *Store) validateAttachedReport(res OperationResult, wantVersion string) (reportOutcome, json.RawMessage, error) {
	if res.Seq == 0 {
		return 0, nil, fmt.Errorf("%w: report sequence", ErrInvalidReport)
	}
	if res.Version == "" {
		return 0, nil, fmt.Errorf("%w: report version", ErrInvalidReport)
	}
	if res.Version != wantVersion {
		return 0, nil, fmt.Errorf("%w: version %s", ErrInvalidReport, res.Version)
	}
	cfg, err := validateConfig(res.Config)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: config", ErrInvalidReport)
	}
	outcome := s.devices[res.DeviceID].classifyReport(res.Seq, res.At, res.Version, cfg)
	switch outcome {
	case reportStale:
		return 0, nil, fmt.Errorf("%w: stale sequence", ErrInvalidReport)
	case reportConflict:
		return 0, nil, fmt.Errorf("%w: sequence conflict", ErrInvalidReport)
	default:
		return outcome, cfg, nil
	}
}
