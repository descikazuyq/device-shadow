// Command single-device-upgrade 在本机模拟一台设备完成一次完整的分批升级：
// 离线登记 -> 查询到尚未领取的下载待办 -> 有效上报上线 -> 在维护窗口内领取
// 下载并提交成功 -> 领取安装并附带合规上报成功，最后核对设备版本、上报配置、
// 期望配置与活动状态。程序只使用 shadow 包的公开入口，不需要真实硬件或外网。
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/descikazuyq/device-shadow/shadow"
)

func main() {
	// 全部状态写在一个临时目录的 shadow-store.json 中，运行结束即删除。
	dir, err := os.MkdirTemp("", "shadow-example-*")
	must(err)
	defer os.RemoveAll(dir)
	fmt.Printf("存储目录: %s（程序退出时自动清理）\n\n", dir)

	store, err := shadow.Open(dir)
	must(err)
	defer store.Close()

	const (
		deviceID      = "sensor-001"
		oldVersion    = "firmware-1.0" // 设备出厂版本
		targetVersion = "firmware-2.0" // 本次活动目标版本
		campaignID    = "cmp-2026-10-02"
	)

	// 固定的演示时间线（UTC）：
	//	11:45 升级前下发期望配置
	//	11:50 创建活动
	//	12:00 维护窗口开始（包含）
	//	13:00 维护窗口结束（不包含）
	//	14:00 截止时间
	tDesired := time.Date(2026, 10, 2, 11, 45, 0, 0, time.UTC)
	tCreated := time.Date(2026, 10, 2, 11, 50, 0, 0, time.UTC)
	tWindowStart := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	tWindowEnd := time.Date(2026, 10, 2, 13, 0, 0, 0, time.UTC)
	tDeadline := time.Date(2026, 10, 2, 14, 0, 0, 0, time.UTC)

	// ---- 1. 登记版本与设备 -------------------------------------------------
	fmt.Println("== 1. 登记升级目标版本与设备 ==")
	must(store.RegisterUpgrade(targetVersion, []string{oldVersion}))
	must(store.Register(deviceID, oldVersion)) // 新设备初始离线，双方配置均为 {}
	fmt.Printf("已登记目标版本 %s，允许从 %v 升级；设备 %s 初始版本 %s、离线\n\n",
		targetVersion, []string{oldVersion}, deviceID, oldVersion)

	// ---- 2. 升级前先设置一份期望配置，并留存副本 ---------------------------
	fmt.Println("== 2. 升级前下发期望配置（升级过程中不应被改动） ==")
	desiredCfg := json.RawMessage(`{"telemetry":{"intervalSec":30},"mode":"safe"}`)
	rev, err := store.UpdateDesired(deviceID, "ops-bot", tDesired, 0, desiredCfg)
	must(err)
	expectedDesired := json.RawMessage(string(desiredCfg)) // 留存升级前期望
	fmt.Printf("期望配置已设置: %s（修订号 %d）\n\n", compact(desiredCfg), rev)

	// ---- 3. 创建活动 -------------------------------------------------------
	fmt.Println("== 3. 创建单设备升级活动 ==")
	must(store.CreateCampaign(shadow.CampaignSpec{
		ID:            campaignID,
		Operator:      "ops-bot",
		CreatedAt:     tCreated,
		TargetVersion: targetVersion,
		Devices:       []string{deviceID},
		BatchSize:     1,
		WindowStart:   tWindowStart,
		WindowEnd:     tWindowEnd,
		Deadline:      tDeadline,
	}))
	fmt.Printf("活动 %s 已创建: 窗口 [%s, %s)，截止 %s\n\n",
		campaignID, tWindowStart.Format(time.RFC3339),
		tWindowEnd.Format(time.RFC3339), tDeadline.Format(time.RFC3339))

	// ---- 4. 设备离线：查得到下载待办，但窗口内领取仍是空操作 ---------------
	fmt.Println("== 4. 离线设备：GetDeviceWork 看得到待办，Claim 不派发 ==")
	tOfflineInWindow := time.Date(2026, 10, 2, 12, 5, 0, 0, time.UTC)
	showWork(store, deviceID, "    ")
	op, err := store.Claim(campaignID, deviceID, tOfflineInWindow)
	must(err)
	fmt.Printf("    Claim@%s（设备离线，但时间在窗口内）: op=%v err=%v\n",
		tOfflineInWindow.Format("15:04"), op, err)
	fmt.Println("    op 为 nil 且 err 为 nil：这是“暂时领不到”的正常等待，")
	fmt.Println("    不是调用失败；绝不能拿空标识去 SubmitResult。")
	fmt.Println()

	// ---- 5. 一次有效上报让设备上线 ----------------------------------------
	fmt.Println("== 5. 设备用正整数序号上报，转为在线 ==")
	tReport := time.Date(2026, 10, 2, 12, 10, 0, 0, time.UTC)
	reportedV1 := json.RawMessage(`{"telemetry":{"intervalSec":60},"mode":"safe"}`)
	must(store.Report(deviceID, 1, tReport, oldVersion, reportedV1))
	showShadow(store, deviceID, "    ")
	showWork(store, deviceID, "    ")
	fmt.Println()

	// ---- 6. 窗口内领取下载；查询返回的标识此刻才真正属于设备 ---------------
	fmt.Println("== 6. 在维护窗口内领取下载 ==")
	tClaimDownload := time.Date(2026, 10, 2, 12, 20, 0, 0, time.UTC)
	dl, err := store.Claim(campaignID, deviceID, tClaimDownload)
	must(err)
	if dl == nil {
		log.Fatal("在线且在窗口内却没有领取到下载操作，停止升级流程")
	}
	fmt.Printf("    Claim@%s: 领取到 kind=%s id=%s\n",
		tClaimDownload.Format("15:04"), dl.Kind, dl.ID)
	showWork(store, deviceID, "    ") // 同一标识，PendingClaimed 变为 true
	showCampaignDevice(store, campaignID, deviceID, "    ")
	fmt.Println()

	// ---- 7. 提交下载成功：只进入等待安装，当前版本仍是旧版本 ---------------
	fmt.Println("== 7. 提交下载成功结果 ==")
	tDownloadDone := time.Date(2026, 10, 2, 12, 25, 0, 0, time.UTC)
	must(store.SubmitResult(shadow.OperationResult{
		CampaignID:  campaignID,
		DeviceID:    deviceID,
		OperationID: dl.ID,
		At:          tDownloadDone,
		Success:     true,
	}))
	showShadow(store, deviceID, "    ")
	work := showWork(store, deviceID, "    ") // 出现尚未领取的安装待办
	showCampaignDevice(store, campaignID, deviceID, "    ")
	fmt.Println()

	// ---- 8. 查询不等于领取：直接提交“查到但未领取”的安装会被拒绝 ----------
	fmt.Println("== 8. 待办标识能查到，但未领取就提交结果会被拒绝 ==")
	tSkip := time.Date(2026, 10, 2, 12, 26, 0, 0, time.UTC)
	installCfg := json.RawMessage(`{"telemetry":{"intervalSec":30},"mode":"safe","features":{"beta":false}}`)
	err = store.SubmitResult(shadow.OperationResult{
		CampaignID: campaignID, DeviceID: deviceID,
		OperationID: work.Pending.ID, // 直接使用查询里看到的安装标识
		At:          tSkip, Success: true,
		Seq: 2, Version: targetVersion, Config: installCfg,
	})
	fmt.Printf("    未领取先提交安装结果: err = %v\n", err)
	if !errors.Is(err, shadow.ErrOperationNotClaimed) {
		log.Fatalf("期望 ErrOperationNotClaimed，实际: %v", err)
	}
	fmt.Println("    （这是调用方自己的流程错误：真实程序收到该错误应打印并停止；")
	fmt.Println("     本示例为了演示，改用正确的“先领取再提交”继续。）")
	fmt.Println()

	// ---- 9. 窗口内领取安装，提交带合规上报的成功结果 -----------------------
	fmt.Println("== 9. 领取安装并提交成功（附带正整数序号、目标版本、完整 JSON 对象） ==")
	tClaimInstall := time.Date(2026, 10, 2, 12, 30, 0, 0, time.UTC)
	in, err := store.Claim(campaignID, deviceID, tClaimInstall)
	must(err)
	if in == nil || in.Kind != shadow.StageInstall {
		log.Fatalf("期望领取到安装操作，得到 op=%v err=%v，停止升级流程", in, err)
	}
	fmt.Printf("    Claim@%s: 领取到 kind=%s id=%s\n",
		tClaimInstall.Format("15:04"), in.Kind, in.ID)

	tInstallDone := time.Date(2026, 10, 2, 12, 35, 0, 0, time.UTC)
	must(store.SubmitResult(shadow.OperationResult{
		CampaignID: campaignID, DeviceID: deviceID, OperationID: in.ID,
		At: tInstallDone, Success: true,
		// 安装成功的附带上报：正整数序号（2 > 已接受的 1）、
		// 版本必须恰好等于活动目标、配置必须是完整 JSON 对象。
		Seq: 2, Version: targetVersion, Config: installCfg,
	}))
	fmt.Println("    安装成功已接受。")
	fmt.Println()

	// ---- 10. 核对最终状态 --------------------------------------------------
	fmt.Println("== 10. 升级完成后的状态核对 ==")
	view := showShadow(store, deviceID, "    ")
	showWork(store, deviceID, "    ") // 活动结束，待办不再出现
	cv := showCampaign(store, campaignID, "    ")
	fmt.Printf("    结果历史条数: %d（下载成功、安装成功各一条）\n", len(cv.Results))

	fmt.Println("    期望配置核对:")
	fmt.Printf("      升级前期望: %s\n", compact(expectedDesired))
	fmt.Printf("      当前期望  : %s（修订号仍为 %d，安装上报没有改写它）\n",
		compact(view.Desired), view.Revision)
	diffs := must1(store.Diff(deviceID))
	fmt.Printf("      当前差异 %d 条:\n", len(diffs))
	for _, d := range diffs {
		fmt.Printf("        %s: 期望存在=%t 上报存在=%t（上报内容变化只影响上报侧）\n",
			d.Path, d.DesiredExists, d.ReportedExists)
	}
	fmt.Println()

	// ---- 11. 窗口边界：开始时刻包含、结束时刻不包含（另开小活动演示） ------
	fmt.Println("== 11. 维护窗口边界（独立的边界演示，不影响上面的活动） ==")
	demonstrateWindowBoundaries(store, tCreated, tWindowStart, tWindowEnd, tDeadline, oldVersion)
}

// demonstrateWindowBoundaries 用两台额外设备分别演示：
// 在线但时间不在窗口内（窗口前、窗口结束时刻）领取返回 (nil, nil)；
// 在窗口开始时刻领取可以成功。
func demonstrateWindowBoundaries(s *shadow.Store, created, windowStart, windowEnd, deadline time.Time, oldVersion string) {
	// 设备 A：窗口 12:00–12:30。在线时于 11:55（窗口前）和 12:30（结束时刻）领取。
	const devA = "sensor-edge-a"
	must(s.Register(devA, oldVersion))
	must(s.Report(devA, 1, created.Add(-5*time.Minute), oldVersion, json.RawMessage(`{}`)))
	must(s.CreateCampaign(shadow.CampaignSpec{
		ID: "cmp-edge-a", Operator: "ops-bot", CreatedAt: created,
		TargetVersion: "firmware-2.0", Devices: []string{devA}, BatchSize: 1,
		WindowStart: windowStart, WindowEnd: windowStart.Add(30 * time.Minute), Deadline: deadline,
	}))
	tBefore := windowStart.Add(-5 * time.Minute) // 11:55，设备在线但窗口未开始
	op, err := s.Claim("cmp-edge-a", devA, tBefore)
	must(err)
	fmt.Printf("    设备 %s Claim@%s（在线、窗口未开始）: op=%v err=%v\n",
		devA, tBefore.Format("15:04"), op, err)
	tEnd := windowStart.Add(30 * time.Minute) // 12:30，恰好窗口结束（不包含）
	op, err = s.Claim("cmp-edge-a", devA, tEnd)
	must(err)
	fmt.Printf("    设备 %s Claim@%s（在线、恰为窗口结束时刻）: op=%v err=%v\n",
		devA, tEnd.Format("15:04"), op, err)
	w := showWork(s, devA, "      ")
	fmt.Printf("      待办仍在（kind=%s, claimed=%t）：设备在正常等待下一个窗口，不是失败\n",
		w.Pending.Kind, w.PendingClaimed)

	// 设备 B：在窗口开始时刻 12:00 领取，可以领取成功（开始时刻包含）。
	const devB = "sensor-edge-b"
	must(s.Register(devB, oldVersion))
	must(s.Report(devB, 1, created.Add(-5*time.Minute), oldVersion, json.RawMessage(`{}`)))
	must(s.CreateCampaign(shadow.CampaignSpec{
		ID: "cmp-edge-b", Operator: "ops-bot", CreatedAt: created,
		TargetVersion: "firmware-2.0", Devices: []string{devB}, BatchSize: 1,
		WindowStart: windowStart, WindowEnd: windowEnd, Deadline: deadline,
	}))
	op, err = s.Claim("cmp-edge-b", devB, windowStart)
	must(err)
	if op == nil {
		log.Fatal("窗口开始时刻应当可以领取")
	}
	fmt.Printf("    设备 %s Claim@%s（恰为窗口开始时刻）: 领取到 kind=%s（开始时刻包含）\n",
		devB, windowStart.Format("15:04"), op.Kind)
}

// ---- 输出辅助 -------------------------------------------------------------

func must(err error) {
	if err != nil {
		log.Fatalf("调用失败，停止当前升级流程: %v", err)
	}
}

func must1[T any](v T, err error) T {
	must(err)
	return v
}

func compact(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

func showWork(s *shadow.Store, deviceID, indent string) shadow.DeviceWork {
	w, err := s.GetDeviceWork(deviceID)
	must(err)
	switch {
	case w.Pending == nil:
		fmt.Printf("%sGetDeviceWork: version=%s online=%t campaign=%q 待办=<无>\n",
			indent, w.Version, w.Online, w.CampaignID)
	default:
		fmt.Printf("%sGetDeviceWork: version=%s online=%t campaign=%q 待办 kind=%s claimed=%t id=%s\n",
			indent, w.Version, w.Online, w.CampaignID,
			w.Pending.Kind, w.PendingClaimed, w.Pending.ID)
	}
	return w
}

func showShadow(s *shadow.Store, deviceID, indent string) shadow.View {
	v, err := s.Get(deviceID)
	must(err)
	fmt.Printf("%s设备影子: version=%s online=%t lastSeq=%d 修订号=%d\n",
		indent, v.Version, v.Online, v.LastSeq, v.Revision)
	fmt.Printf("%s  desired =%s\n", indent, compact(v.Desired))
	fmt.Printf("%s  reported=%s\n", indent, compact(v.Reported))
	return v
}

func showCampaignDevice(s *shadow.Store, campaignID, deviceID, indent string) {
	cv, err := s.GetCampaign(campaignID)
	must(err)
	for _, d := range cv.Devices {
		if d.DeviceID == deviceID {
			fmt.Printf("%s活动内状态: status=%s phase=%s\n", indent, d.Status, d.Phase)
			return
		}
	}
}

func showCampaign(s *shadow.Store, campaignID, indent string) shadow.CampaignView {
	cv, err := s.GetCampaign(campaignID)
	must(err)
	fmt.Printf("%s活动: status=%s ended=%t endedAt=%s\n",
		indent, cv.Status, cv.Ended, cv.EndedAt.Format(time.RFC3339))
	for _, d := range cv.Devices {
		fmt.Printf("%s  设备 %s: status=%s phase=%s\n", indent, d.DeviceID, d.Status, d.Phase)
	}
	return cv
}
