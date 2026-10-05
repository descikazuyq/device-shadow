// 单设备分批升级的完整示例：从旧版本 1.9.0 升级到已登记的目标版本 2.4.0。
//
// 运行：
//
//	go run ./examples/single-upgrade
//
// 全程只使用本机临时目录中的 shadow 存储，不需要真实硬件或外网。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/descikazuyq/device-shadow/shadow"
)

const layout = "2006-01-02 15:04:05"

// 示例统一使用 UTC，保证任何机器上输出一致。
func at(s string) time.Time {
	t, err := time.ParseInLocation(layout, s, time.UTC)
	if err != nil {
		panic(err)
	}
	return t
}

func must(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "!! 发生未预期的调用错误，升级流程停止: %v\n", err)
		os.Exit(1)
	}
}

func compact(raw json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return string(raw)
	}
	return b.String()
}

func printWork(title string, w shadow.DeviceWork) {
	fmt.Printf("    %s: version=%s online=%t campaign=%q\n", title, w.Version, w.Online, w.CampaignID)
	if w.Pending == nil {
		fmt.Printf("      待办: 无\n")
		return
	}
	fmt.Printf("      待办: kind=%s claimed=%t id=%s", w.Pending.Kind, w.PendingClaimed, w.Pending.ID)
	if w.Pending.TargetVersion != "" {
		fmt.Printf(" rollbackTarget=%s", w.Pending.TargetVersion)
	}
	fmt.Println()
}

func main() {
	dir, err := os.MkdirTemp("", "shadow-upgrade-example-")
	must(err)
	defer os.RemoveAll(dir)

	store, err := shadow.Open(dir)
	must(err)
	defer store.Close()

	const (
		deviceID  = "sensor-a17"
		oldVer    = "1.9.0"
		targetVer = "2.4.0"
		campaignA = "upgrade-240"
	)

	// 升级前就设好的期望配置：升级结束后它必须原样保留。
	desiredCfg := json.RawMessage(`{"firmware":{"channel":"lts"},"telemetry":{"interval":30}}`)
	// 设备上线时的上报配置：与期望配置存在差异。
	onlineCfg := json.RawMessage(`{"firmware":{"channel":"stable"},"telemetry":{"interval":60}}`)
	// 安装成功时附带的上报配置：完整 JSON 对象，频道改回 lts，并带来新字段。
	installCfg := json.RawMessage(`{"firmware":{"channel":"lts","version":"2.4.0"},"telemetry":{"interval":30}}`)

	fmt.Println("== 第一部分：一台设备在一个活动中完成下载 + 安装 ==")

	// [1] 登记目标版本：只允许从 1.9.0 升级；登记设备，初始版本 1.9.0。
	must(store.RegisterUpgrade(targetVer, []string{oldVer}))
	must(store.Register(deviceID, oldVer))

	// [2] 升级开始前先设好期望配置（修订号从 0 读起），稍后验证它不被安装改写。
	rev, err := store.UpdateDesired(deviceID, "ops-admin", at("2026-09-01 08:05:00"), 0, desiredCfg)
	must(err)
	fmt.Printf("[2] 升级前设置期望配置，新修订号=%d: %s\n", rev, compact(desiredCfg))

	// [3] 创建活动：创建 08:00，维护窗口 [09:00, 11:00)，截止次日 18:00。
	must(store.CreateCampaign(shadow.CampaignSpec{
		ID:            campaignA,
		Operator:      "ops-admin",
		CreatedAt:     at("2026-09-01 08:00:00"),
		TargetVersion: targetVer,
		Devices:       []string{deviceID},
		BatchSize:     1,
		WindowStart:   at("2026-09-01 09:00:00"),
		WindowEnd:     at("2026-09-01 11:00:00"),
		Deadline:      at("2026-09-02 18:00:00"),
	}))
	fmt.Println("[3] 已创建活动 upgrade-240，设备登记后初始离线")

	// [4] 设备离线时查询待办：能看到尚未领取的下载操作，但查询本身不领取、不推进活动。
	w, err := store.GetDeviceWork(deviceID)
	must(err)
	fmt.Println("[4] 08:10 GetDeviceWork（设备离线）：")
	printWork("查询", w)
	saveDownloadID := w.Pending.ID

	// [5] 设备先通过一次有效上报上线（普通上报不推进活动）。
	must(store.Report(deviceID, 1, at("2026-09-01 08:40:00"), oldVer, onlineCfg))
	fmt.Println("[5] 08:40 序号 1 上报成功，设备上线，当前版本仍是 1.9.0")

	// [6] 在线但时间在窗口外（早于 09:00）领取：返回空操作且没有错误，这是正常等待。
	op, err := store.Claim(campaignA, deviceID, at("2026-09-01 08:45:00"))
	must(err)
	fmt.Printf("[6] 08:45（在线、窗口开始前）领取: operation=%v err=<nil> => 正常等待，未领取\n", op)

	// 模拟设备又掉线。
	must(store.SetOffline(deviceID))

	// [7] 离线时即使正处在维护窗口内，领取同样返回空操作、没有错误。
	op, err = store.Claim(campaignA, deviceID, at("2026-09-01 10:00:00"))
	must(err)
	fmt.Printf("[7] 10:00（离线、窗口内）领取: operation=%v err=<nil> => 待办保留，未领取\n", op)

	// [8] 再次上线：重复提交序号 1 的旧上报不能把设备置回在线，必须用更大序号。
	must(store.Report(deviceID, 2, at("2026-09-01 10:05:00"), oldVer, onlineCfg))
	fmt.Println("[8] 10:05 序号 2 上报成功，设备重新上线")

	// [9] 窗口内、在线时领取下载：拿到与步骤 [4] 查询相同的稳定标识。
	op, err = store.Claim(campaignA, deviceID, at("2026-09-01 10:10:00"))
	must(err)
	if op == nil || op.Kind != shadow.StageDownload {
		fmt.Fprintf(os.Stderr, "!! 期望领取到下载操作，实际 %v，升级流程停止\n", op)
		os.Exit(1)
	}
	fmt.Printf("[9] 10:10 领取下载成功: kind=%s id=%s（与查询到的待办标识相同: %t）\n",
		op.Kind, op.ID, op.ID == saveDownloadID)

	// [10] 设备完成下载并提交成功结果。
	must(store.SubmitResult(shadow.OperationResult{
		CampaignID:  campaignA,
		DeviceID:    deviceID,
		OperationID: op.ID,
		At:          at("2026-09-01 10:15:00"),
		Success:     true,
	}))
	fmt.Println("[10] 10:15 下载成功结果已接受")

	// 下载成功只让设备进入“等待安装”：当前版本仍是旧版本。
	v, err := store.Get(deviceID)
	must(err)
	w, err = store.GetDeviceWork(deviceID)
	must(err)
	fmt.Printf("     此刻影子: version=%s online=%t lastSeq=%d\n", v.Version, v.Online, v.LastSeq)
	printWork("待办", w)
	installID := w.Pending.ID

	// [11] 故意演示错误用法：尚未领取安装，就拿查询到的待办标识直接提交结果。
	// 这会返回错误且什么都不改变；示例不会使用这个结果继续流程。
	err = store.SubmitResult(shadow.OperationResult{
		CampaignID:  campaignA,
		DeviceID:    deviceID,
		OperationID: installID, // 来自 GetDeviceWork 的“未领取”待办
		At:          at("2026-09-01 10:18:00"),
		Success:     true,
		Seq:         3,
		Version:     targetVer,
		Config:      installCfg,
	})
	fmt.Printf("[11] 10:18 未领取就提交安装结果: err=%v\n", err)
	if !errors.Is(err, shadow.ErrOperationNotClaimed) {
		fmt.Fprintf(os.Stderr, "!! 期望 ErrOperationNotClaimed，升级流程停止\n")
		os.Exit(1)
	}
	w, err = store.GetDeviceWork(deviceID)
	must(err)
	fmt.Printf("     被拒绝后待办仍是安装、claimed=%t，活动状态不变\n", w.PendingClaimed)

	// [12] 正确流程：在窗口内先领取安装。
	op, err = store.Claim(campaignA, deviceID, at("2026-09-01 10:20:00"))
	must(err)
	if op == nil || op.Kind != shadow.StageInstall {
		fmt.Fprintf(os.Stderr, "!! 期望领取到安装操作，实际 %v，升级流程停止\n", op)
		os.Exit(1)
	}
	fmt.Printf("[12] 10:20 领取安装成功: kind=%s id=%s\n", op.Kind, op.ID)

	// [13] 提交安装成功：必须附带正整数序号、与活动目标一致的版本和完整 JSON 对象配置。
	must(store.SubmitResult(shadow.OperationResult{
		CampaignID:  campaignA,
		DeviceID:    deviceID,
		OperationID: op.ID,
		At:          at("2026-09-01 10:20:00"),
		Success:     true,
		Seq:         3,
		Version:     targetVer,
		Config:      installCfg,
	}))
	fmt.Println("[13] 10:20 安装成功结果（含序号 3、版本 2.4.0、完整配置）已接受")

	// [14] 验证最终状态。
	v, err = store.Get(deviceID)
	must(err)
	fmt.Printf("[14] 升级后影子: version=%s online=%t lastSeq=%d\n", v.Version, v.Online, v.LastSeq)
	fmt.Printf("     上报配置: %s\n", compact(v.Reported))
	fmt.Printf("     期望配置: %s（与升级前相同: %t，修订号仍为 %d）\n",
		compact(v.Desired), bytes.Equal(bytes.TrimSpace(v.Desired), bytes.TrimSpace(desiredCfg)), v.Revision)

	diffs, err := store.Diff(deviceID)
	must(err)
	fmt.Printf("     配置差异 %d 处:\n", len(diffs))
	for _, d := range diffs {
		fmt.Printf("       - %s: desired=%q reported=%q\n", d.Path, string(d.Desired), string(d.Reported))
	}

	w, err = store.GetDeviceWork(deviceID)
	must(err)
	printWork("升级后查询", w)

	camp, err := store.GetCampaign(campaignA)
	must(err)
	fmt.Printf("     活动: status=%s ended=%t endedAt=%s，设备状态=%s，结果历史 %d 条\n",
		camp.Status, camp.Ended, camp.EndedAt.Format(layout), camp.Devices[0].Status, len(camp.Results))
	for _, r := range camp.Results {
		fmt.Printf("       - stage=%s success=%t version=%q at=%s\n",
			r.Stage, r.Success, r.Version, r.At.Format(layout))
	}

	fmt.Println()
	fmt.Println("== 第二部分：维护窗口边界补充演示（同一台设备、另一个活动） ==")

	// 设备已在 2.4.0，再登记一个 2.5.0 目标用于演示窗口边界。
	const campaignB = "upgrade-250-window"
	must(store.RegisterUpgrade("2.5.0", []string{targetVer}))
	must(store.CreateCampaign(shadow.CampaignSpec{
		ID:            campaignB,
		Operator:      "ops-admin",
		CreatedAt:     at("2026-09-01 12:00:00"),
		TargetVersion: "2.5.0",
		Devices:       []string{deviceID},
		BatchSize:     1,
		WindowStart:   at("2026-09-01 14:00:00"),
		WindowEnd:     at("2026-09-01 15:00:00"),
		Deadline:      at("2026-09-02 18:00:00"),
	}))

	// 窗口开始时刻包含：14:00:00 整可以领取新操作。
	dl, err := store.Claim(campaignB, deviceID, at("2026-09-01 14:00:00"))
	must(err)
	fmt.Printf("[15] 14:00:00（窗口开始时刻）领取: kind=%s id=%s => 开始时刻包含\n", dl.Kind, dl.ID)

	// 已领取未完成的操作再查返回同一标识，不要求还在窗口内。
	same, err := store.Claim(campaignB, deviceID, at("2026-09-01 15:00:00"))
	must(err)
	fmt.Printf("[16] 15:00:00（窗口结束时刻）再次领取: kind=%s 同一标识=%t => 已领取操作不受窗口限制\n",
		same.Kind, same.ID == dl.ID)

	// 已领取的下载允许在窗口外、截止前完成。
	must(store.SubmitResult(shadow.OperationResult{
		CampaignID:  campaignB,
		DeviceID:    deviceID,
		OperationID: dl.ID,
		At:          at("2026-09-01 15:30:00"),
		Success:     true,
	}))
	fmt.Println("[17] 15:30 窗口外提交下载成功：已领取操作可以在窗口外完成")

	// 但尚未领取的安装在结束时刻及以后仍是新操作，15:40 领取返回空操作（结束时刻不包含）。
	none, err := store.Claim(campaignB, deviceID, at("2026-09-01 15:40:00"))
	must(err)
	fmt.Printf("[18] 15:40（窗口结束之后）领取安装: operation=%v err=<nil> => 尚未领取的新操作仍须等窗口\n", none)
	w, err = store.GetDeviceWork(deviceID)
	must(err)
	printWork("待办", w)
	fmt.Println("     该活动若一直等不到下一个窗口，设备将在 2026-09-02 18:00 截止后被记为超时。")

	fmt.Println()
	fmt.Println("示例完成：全部等待均为空操作 + nil 错误，真正的错误（未领取就提交）被明确拒绝。")
}
