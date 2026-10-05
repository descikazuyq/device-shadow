# 设备影子与远程升级

这是一个在本机运行的设备影子与远程升级模块。

## 功能

`shadow` 包提供本地设备影子：

- **登记设备**：`Register(deviceID, version)`，标识与初始版本不能为空，重复登记报错；新设备初始离线，期望与上报配置均为空对象。
- **修改期望配置**：`UpdateDesired(deviceID, operator, at, revision, config)`，携带操作者、操作时间和已读取的修订号；修订号冲突时影子与审计均不变。每次成功修改（含提交相同配置）修订号加一并留下审计记录。
- **批量修改期望配置**：`BatchUpdateDesired(requestID, operator, at, devices)` 用一次请求同时修改多台已登记设备，`devices` 为 `[]BatchDevice`（设备标识、已读取修订号、完整期望配置）。请求标识非空且在存储内唯一，操作者不能为空、时间不能缺失；设备列表为空、标识为空或重复、设备未登记、配置非法或任一修订号冲突时整批报错，所有设备状态、审计均不变，也不占用请求标识。首次成功提交每台设备完整替换期望配置、修订号各加一（含提交相同配置），返回各设备新修订号；每台设备留下带同一请求标识的审计，差异、离线设备接受修改等规则与单设备修改一致，设备版本、上报与升级待办不受影响。相同标识重发相同内容（设备换序、JSON 空白与对象字段顺序无关，数字按数值比较，数组有序，字段缺失与 `null` 有别，时间按同一时刻判断）直接返回首次结果，不重复修改、不增加审计，即使设备后来已有新修改也不用旧请求覆盖；相同标识改用不同操作者、时间、设备集合、原修订号或配置返回 `ErrRequestConflict`，原结果保持有效。`GetBatchRequest(requestID)` 查询成功提交的记录（操作者、提交时间、各设备新修订号）；未知标识返回 `ErrRequestNotFound`，失败请求不可查为成功。并发重发同一请求只产生一次修改；不同请求竞争同一设备同一修订号时只有一个成功。保存失败时整批状态、审计与请求记录一起回滚，恢复可写后可用原标识重试；关闭重开后查询与去重仍有效，旧存储没有批量记录可正常打开（原单设备审计标识为空），批量记录损坏时拒绝打开。
- **设备上报**：`Report(deviceID, seq, at, version, config)`，正整数序号、时间、非空版本和完整 JSON 对象配置缺一不可。更大序号更新并上线；相同序号内容一致视为重复；相同序号内容不同或更小序号报错。`SetOffline` 只改在线状态。
- **查询**：`Get` 返回版本、在线状态、修订号与双方配置；`Diff` 按 JSON Pointer 路径字典序列出配置差异（含双方值、存在性和差异首次出现时间）；`Audit` 按修订号递增返回审计记录。
- **持久化**：全部状态与审计保存在 `Open(dir)` 指定目录的 `shadow-store.json`，一次操作的状态与审计原子写入；保存失败回滚内存状态；已有内容损坏时拒绝打开，不会按空数据覆盖。打开时按当前期望配置与上报配置重新计算差异并对账差异首次出现时间：每条实际差异必须恰好对应一条非零时间记录，记录中不能残留当前已无差异的路径（父路径不能代替子路径，条数相同但路径不同也不行）；没有差异的设备允许不保存记录或保存空记录。缺少、多出或时间为零一律返回 `ErrCorruptStorage`，拒绝打开整个存储并保留原文件，不补当前时间、不删多余记录、不重写配置把矛盾变成正常数据。

配置比较忽略对象字段顺序，区分 `null` 与字段不存在；双方均为对象时逐层比较，数组等其余值按整体比较。

## 分批升级

`shadow` 包还提供面向已登记模拟设备的分批升级能力：

- **登记升级版本**：`RegisterUpgrade(targetVersion, allowedFrom)`，目标版本与允许升级的当前版本列表按字符串精确匹配；列表不能为空、不能含空串、不能包含目标版本；目标版本重名返回 `ErrVersionExists` 且保留原记录。`ListUpgrades()` 查看全部登记。
- **创建活动**：`CreateCampaign(CampaignSpec)`，需提供唯一标识、操作者、创建时间、有序设备列表、正整数批大小、维护窗口与截止时间。设备列表不能为空、不能重复，窗口开始须早于结束，截止须晚于创建。设备或版本未登记、任一设备当前版本不兼容、设备已参加未结束的活动时，整次创建失败，不留下部分记录。`CampaignSpec.RollbackOnFailure` 可选开启安装失败回滚，默认关闭；未开启的活动及旧存储中的活动保持原行为。
- **领取与推进**：`Claim(campaignID, deviceID, at)` 由调用方传入当前时间。设备在线且时间位于窗口 `[start, end)` 才能领取新的下载或安装；未完成操作再查仍返回同一标识；离线保留待办、上线后继续；已领取的操作允许在窗口外完成，尚未领取的安装仍须等窗口。首次领取下载前复查当前版本，不兼容则该设备记为下载失败。设备按提交次序分批，前一批全部成功才放行下一批。
- **安装失败回滚（可选）**：开启 `RollbackOnFailure` 后，设备**首次领取下载**时把其当时的当前版本锁定为回滚目标（此后普通上报不得改变它，也不要求该版本另行登记为升级目标），回滚操作携带该目标版本让设备明确知道要恢复哪个版本。截止时间前接受**安装失败**结果后，保存原失败原因和时间，设备转入等待回滚（`awaiting_rollback`）；下载失败、领取前版本不兼容仍按原方式结束，不产生回滚。同批其他设备可继续，后续批次立即记为未执行（skipped），活动要等回滚设备结束，且即使回滚成功活动最终仍为失败。等待回滚或正在回滚（`rolling_back`）的设备不能参加其他活动。回滚**首次领取**要求设备在线且位于原维护窗口 `[start, end)`，离线或窗口外保留待办；已领取但未完成的回滚再查返回同一标识（与下载、安装标识区分），并可在窗口外、截止前提交结果。回滚未完成时普通上报只更新影子，不替代回滚结果或清除待办。回滚**成功**必须附带正整数序号、版本等于锁定目标的完整 JSON 对象配置，沿用现有上报的序号与重复判断；缺上报、版本不符、序号过旧或同序号内容冲突都报错且影子、活动、历史均不变；接受后同时更新当前版本、上报配置与回滚状态，保留当前期望配置、修订号与审计，配置差异及首次出现时间按现有规则变化。回滚**失败**必须给出原因，设备以 `rollback_failed` 结束且影子不变。到达原截止时间仍未完成回滚的设备以回滚阶段超时（`rollback_timeout`）结束，不再接收新结果、不改写或清空影子；已接受结果的重复提交仍有效。
- **结果提交**：`SubmitResult(OperationResult)`，每次结果带活动、设备、操作标识和发生时间。相同操作的相同结果重复提交成功但不增加历史、不重复放行；不同结果、跳过未领取阶段、提交未领取的回滚、提交其他设备的操作或活动结束后的新结果一律报错且不改状态；已接受结果的重复提交在活动结束后仍有效。重复提交同一安装失败不会再次产生回滚。设备失败记录阶段、原因和时间，本批其他设备继续，后续批次记为未执行（skipped），活动最终失败。已领取但尚未接受结果的下载、安装或回滚操作在截止时刻及以后提交时，结果迟到，不校验内容（附带上报缺失、版本不符、配置非法或失败结果缺少原因都不改变结论）、不写入结果历史与影子：提交前活动仍在执行的，由该次提交首次触发截止处理，把尚未结束的设备记为超时（等待或正在回滚的记回滚阶段超时），以本次提交时间结束活动并返回 `ErrCampaignEnded`，本次确实推进的时间仍参与后续倒退判断；活动此前已超时结束的，只返回 `ErrCampaignEnded`，是不推进时间基线、不改结束时间与设备状态的纯拒绝，随后不早于结束时间的合法时间不会因这次拒绝而被判为倒退。未知操作、未领取操作、时间缺失、时间倒退仍按原有错误规则拒绝。
- **安装成功的附带上报**：安装成功必须附带正整数序号、版本**等于目标版本**的完整 JSON 对象上报，校验沿用 `Report` 的序号规则；影子与活动在同一次原子写入中更新。普通 `Report` 不推进任何活动。
- **截止时间**：`AdvanceCampaign(campaignID, at)` 显式推进；领取或接收结果时若时间达到截止时间，同样把所有未结束设备记为超时（回滚中的设备记为回滚阶段超时，terminal 状态不变），活动失败结束，不再派发操作。时间缺失或同一活动时间倒退均拒绝且不改状态。
- **查询**：`GetCampaign(id)` 返回创建者、目标版本、回滚开关、按批次分组与按提交次序排列的设备状态、安装失败与回滚结果各自的原因/时间、等待回滚/正在回滚/回滚成功/回滚失败（含回滚超时）状态及结果历史；`GetDeviceWork(deviceID)` 返回设备当前版本与待执行操作（含是否已领取；回滚待办带锁定目标版本）。
- **持久化**：版本登记、回滚开关与锁定目标、活动进度、操作标识、时间基线与结果历史一并保存在 `shadow-store.json`；保存失败时版本记录、影子、活动和历史整体回滚；关闭后重新打开，回滚目标、进度、操作标识和重复结果判断仍然有效；旧存储可直接打开（回滚缺省关闭）并保留原有设备、配置差异与审计。打开时按已保存的操作结果与结果历史双向对账：每个已有结果的下载、安装或回滚操作恰好对应一条历史，每条历史也必须对应该设备该阶段实际已有的结果（设备、操作标识、阶段、成败、首次接受时间按同一时刻判断、失败原因一致，安装或回滚成功的版本一致）；已领取未完成、超时或因前批失败而未执行的阶段不要求有结果。缺少、多出、重复或内容矛盾一律返回 `ErrCorruptStorage`，拒绝打开整个存储并保留原文件，不通过删历史、补结果或改变进度掩盖矛盾。打开时还要对账活动整体结论与设备进度：任一设备仍处于等待下载、下载中、等待安装、安装中、等待回滚或回滚中时，活动必须是 running 且未结束（设备离线、维护窗口结束都不算设备已结束）；全部设备进入终态时活动必须已结束，且只有每台设备都 succeeded 才能 succeeded，其余组合（含失败、超时、未执行及回滚成功——回滚成功只是恢复原版本，不算本次升级成功）只能 failed。结论与进度矛盾同样返回 `ErrCorruptStorage`，不通过改写活动结论、改变设备状态或删除活动消除矛盾。

## 使用

```bash
go test ./...
```

测试通过表示基线包可以加载。后续能力在这个模块上继续增加。

### 单设备升级示例

`examples/single-upgrade` 是一个可直接运行的完整示例，展示调用方怎样让一台模拟设备在一个活动中完成「下载 → 安装」升级。它只用 `shadow` 包的公开入口（`Open`、`RegisterUpgrade`、`Register`、`UpdateDesired`、`Report`、`SetOffline`、`CreateCampaign`、`GetDeviceWork`、`Claim`、`SubmitResult`、`Get`、`Diff`、`GetCampaign`），状态保存在本机临时目录里，不需要真实硬件或外网，也不依赖测试代码。

```bash
go run ./examples/single-upgrade
```

场景围绕同一台设备 `sensor-a17`（初始版本 `1.9.0`）和同一个活动 `upgrade-240`（目标版本 `2.4.0`，只允许从 `1.9.0` 升级）展开。活动 08:00 创建，维护窗口为当天 `[09:00, 11:00)`，截止时间为次日 18:00；升级开始前先设好一份期望配置，用来验证安装不会改写它。

完整代码（同时保存在 `examples/single-upgrade/main.go`）：

```go
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
```

预期输出（操作标识是按活动、设备、阶段编码的稳定标识，各机一致）：

```text
== 第一部分：一台设备在一个活动中完成下载 + 安装 ==
[2] 升级前设置期望配置，新修订号=1: {"firmware":{"channel":"lts"},"telemetry":{"interval":30}}
[3] 已创建活动 upgrade-240，设备登记后初始离线
[4] 08:10 GetDeviceWork（设备离线）：
    查询: version=1.9.0 online=false campaign="upgrade-240"
      待办: kind=download claimed=false id=v2:0:11:upgrade-24010:sensor-a17download
[5] 08:40 序号 1 上报成功，设备上线，当前版本仍是 1.9.0
[6] 08:45（在线、窗口开始前）领取: operation=<nil> err=<nil> => 正常等待，未领取
[7] 10:00（离线、窗口内）领取: operation=<nil> err=<nil> => 待办保留，未领取
[8] 10:05 序号 2 上报成功，设备重新上线
[9] 10:10 领取下载成功: kind=download id=v2:0:11:upgrade-24010:sensor-a17download（与查询到的待办标识相同: true）
[10] 10:15 下载成功结果已接受
     此刻影子: version=1.9.0 online=true lastSeq=2
    待办: version=1.9.0 online=true campaign="upgrade-240"
      待办: kind=install claimed=false id=v2:0:11:upgrade-24010:sensor-a17install
[11] 10:18 未领取就提交安装结果: err=shadow: operation has not been claimed: v2:0:11:upgrade-24010:sensor-a17install
     被拒绝后待办仍是安装、claimed=false，活动状态不变
[12] 10:20 领取安装成功: kind=install id=v2:0:11:upgrade-24010:sensor-a17install
[13] 10:20 安装成功结果（含序号 3、版本 2.4.0、完整配置）已接受
[14] 升级后影子: version=2.4.0 online=true lastSeq=3
     上报配置: {"firmware":{"channel":"lts","version":"2.4.0"},"telemetry":{"interval":30}}
     期望配置: {"firmware":{"channel":"lts"},"telemetry":{"interval":30}}（与升级前相同: true，修订号仍为 1）
     配置差异 1 处:
       - /firmware/version: desired="" reported="\"2.4.0\""
    升级后查询: version=2.4.0 online=true campaign=""
      待办: 无
     活动: status=succeeded ended=true endedAt=2026-09-01 10:20:00，设备状态=succeeded，结果历史 2 条
       - stage=download success=true version="" at=2026-09-01 10:15:00
       - stage=install success=true version="2.4.0" at=2026-09-01 10:20:00

== 第二部分：维护窗口边界补充演示（同一台设备、另一个活动） ==
[15] 14:00:00（窗口开始时刻）领取: kind=download id=v2:0:18:upgrade-250-window10:sensor-a17download => 开始时刻包含
[16] 15:00:00（窗口结束时刻）再次领取: kind=download 同一标识=true => 已领取操作不受窗口限制
[17] 15:30 窗口外提交下载成功：已领取操作可以在窗口外完成
[18] 15:40（窗口结束之后）领取安装: operation=<nil> err=<nil> => 尚未领取的新操作仍须等窗口
    待办: version=2.4.0 online=true campaign="upgrade-250-window"
      待办: kind=install claimed=false id=v2:0:18:upgrade-250-window10:sensor-a17install
     该活动若一直等不到下一个窗口，设备将在 2026-09-02 18:00 截止后被记为超时。

示例完成：全部等待均为空操作 + nil 错误，真正的错误（未领取就提交）被明确拒绝。
```

#### 输出解释：查询待办与领取操作的关系

- **[3]→[4] 查询不领取、不推进**：`GetDeviceWork` 返回设备当前版本和待执行操作，但它是只读查询。设备登记后初始离线（`online=false`），此时待办里已经有下载操作，`claimed=false` 明确表示它**尚未被领取**；设备在活动中的状态仍是 `pending`，活动也不因此前进。待办标识是创建活动时就分配好的稳定标识，所以 [9] 真正领取下载时拿到的标识与 [4] 查询到的完全相同（输出中的 `相同: true`）。
- **[5] 上线靠有效上报**：普通 `Report` 只更新影子（更大序号更新上报并置为在线），**不推进任何活动**；上报版本仍是 `1.9.0`，升级尚未开始。
- **[6]/[7] 空操作是“正常等待”，不是失败**：尚未领取的新操作只有在「设备在线 **且** 时间位于维护窗口 `[start, end)`」时才派发。08:45 在线但早于窗口、10:00 在窗口内但设备离线，`Claim` 都返回 `(nil, nil)`——没有操作、也没有错误，待办原样保留。调用方必须把这种情况与真正的错误区分开：示例对 `nil` 操作不做任何提交，稍后重试领取即可。
- **[9]/[10] 领取后才进入下载**：10:10 在线且在窗口内，领取把设备状态从 `pending` 改为 `downloading`；10:15 提交下载成功后，设备进入 `ready`（等待安装），待办变成 `claimed=false` 的安装操作。注意此刻影子仍是 `version=1.9.0`、`lastSeq=2`——**下载成功不改当前版本**。
- **[11] 不能拿未领取待办的标识直接提交**：安装标识虽然能通过查询提前看到，但没有先 `Claim` 就提交结果会返回 `ErrOperationNotClaimed`，影子和活动都不变。示例检查 `errors.Is(err, shadow.ErrOperationNotClaimed)`，打印错误并停止当前这一步，随后改用正确顺序（先 [12] 领取、再 [13] 提交）。任何调用错误都会打印出来并中止升级流程，而不是带着空操作或被拒绝的标识继续走。
- **[13]/[14] 安装成功的附带上报**：安装成功必须一次性附带正整数序号（`Seq: 3`，大于已接受的 2，序号规则与 `Report` 完全一致）、与活动目标一致的版本（`2.4.0`）和**完整 JSON 对象**配置。被接受后，设备当前版本与上报配置在同一次原子写入中更新为 `2.4.0` 和安装配置，设备状态变为 `succeeded`；唯一的设备成功后活动 `status=succeeded` 并以 10:20 结束；此后 `GetDeviceWork` 的 `campaign` 为空、待办为“无”，这个活动不再出现。
- **期望配置不被安装改写**：升级前 [2] 设置的期望配置在 [14] 中原样保留（`相同: true`），修订号仍为 1，安装附带的上报只写上报侧。唯一的配置差异 `/firmware/version` 是“上报里多了版本字段”，期望侧并无该字段。

#### 输出解释：维护窗口边界（第二部分）

- **[15] 开始时刻包含**：14:00:00 整领取成功，窗口区间是 `[start, end)`。
- **[16] 已领取操作不受窗口限制**：15:00:00（结束时刻）再次领取返回的是同一个下载标识——未完成操作再查始终返回同一标识，不要求在线或仍在窗口内。
- **[17] 窗口外可以完成已领取操作**：下载在 15:30 提交成功，只要在截止时间之前即可。
- **[18] 结束时刻不包含、未领取的新操作仍须等窗口**：安装尚未领取，15:40 领取又是 `(nil, nil)` 的正常等待，`GetDeviceWork` 仍显示 `claimed=false` 的安装待办。若始终等不到下一个窗口，达到截止时间（次日 18:00）后设备会被记为超时、活动失败，而不是被静默派发。
