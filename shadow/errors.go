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

	// 分批升级相关错误。
	// ErrInvalidVersionList 表示允许升级的版本列表为空、含空串或含目标版本。
	ErrInvalidVersionList = errors.New("shadow: allowed-from version list invalid")
	// ErrVersionExists 表示重复登记同一目标版本，原记录保留。
	ErrVersionExists = errors.New("shadow: upgrade target version already registered")
	// ErrVersionNotFound 表示引用的升级目标版本未登记。
	ErrVersionNotFound = errors.New("shadow: upgrade target version not found")
	// ErrInvalidCampaignID 表示活动标识为空。
	ErrInvalidCampaignID = errors.New("shadow: campaign id must not be empty")
	// ErrInvalidDeviceList 表示活动设备列表为空。
	ErrInvalidDeviceList = errors.New("shadow: campaign device list must not be empty")
	// ErrDuplicateDevice 表示活动设备列表中设备重复。
	ErrDuplicateDevice = errors.New("shadow: campaign device list contains duplicates")
	// ErrInvalidBatchSize 表示批大小不是正整数。
	ErrInvalidBatchSize = errors.New("shadow: batch size must be a positive integer")
	// ErrInvalidWindow 表示维护窗口开始时刻不早于结束时刻。
	ErrInvalidWindow = errors.New("shadow: maintenance window start must be before end")
	// ErrInvalidDeadline 表示截止时间不晚于创建时间。
	ErrInvalidDeadline = errors.New("shadow: deadline must be after creation time")
	// ErrCampaignExists 表示活动标识重复。
	ErrCampaignExists = errors.New("shadow: campaign already exists")
	// ErrCampaignNotFound 表示访问不存在的活动。
	ErrCampaignNotFound = errors.New("shadow: campaign not found")
	// ErrDeviceNotInCampaign 表示设备不属于该活动。
	ErrDeviceNotInCampaign = errors.New("shadow: device is not in campaign")
	// ErrDeviceBusy 表示设备已参加尚未结束的活动。
	ErrDeviceBusy = errors.New("shadow: device already in an active campaign")
	// ErrIncompatibleVersion 表示设备当前版本不在允许升级的版本列表中。
	ErrIncompatibleVersion = errors.New("shadow: device version is not compatible")
	// ErrTimeRegression 表示同一活动传入的当前时间早于已接受的时间。
	ErrTimeRegression = errors.New("shadow: campaign time must not go backwards")
	// ErrCampaignEnded 表示活动已结束，不再接受新操作。
	ErrCampaignEnded = errors.New("shadow: campaign has ended")
	// ErrDeviceFinished 表示设备在活动中已处于终态。
	ErrDeviceFinished = errors.New("shadow: device already finished in campaign")
	// ErrBatchWaiting 表示设备所在批次尚未放行（前一批未全部成功）。
	ErrBatchWaiting = errors.New("shadow: device batch is waiting for previous batches")
	// ErrOperationNotClaimed 表示为尚未领取的操作提交结果（含跳过阶段）。
	ErrOperationNotClaimed = errors.New("shadow: operation has not been claimed")
	// ErrOperationNotFound 表示操作标识未知或属于其他设备/活动。
	ErrOperationNotFound = errors.New("shadow: operation not found")
	// ErrResultConflict 表示同一操作改用与已接受结果不同的结果。
	ErrResultConflict = errors.New("shadow: result conflicts with accepted result")
	// ErrInvalidReason 表示失败结果未附带原因。
	ErrInvalidReason = errors.New("shadow: failure result requires a reason")
	// ErrInvalidReport 表示安装成功未附带符合上报规则、版本等于目标版本的设备上报。
	ErrInvalidReport = errors.New("shadow: install success requires a valid device report at target version")

	// 批量修改期望配置相关错误。
	// ErrInvalidRequestID 表示批量请求标识为空。
	ErrInvalidRequestID = errors.New("shadow: request id must not be empty")
	// ErrRequestConflict 表示同一请求标识被内容不同的批量请求重复使用，原结果保持有效。
	ErrRequestConflict = errors.New("shadow: request id reused with different content")
	// ErrRequestNotFound 表示按标识查询的批量请求不存在（含从未成功提交的请求）。
	ErrRequestNotFound = errors.New("shadow: batch request not found")
)
