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
- **持久化**：版本登记、回滚开关与锁定目标、活动进度、操作标识、时间基线与结果历史一并保存在 `shadow-store.json`；保存失败时版本记录、影子、活动和历史整体回滚；关闭后重新打开，回滚目标、进度、操作标识和重复结果判断仍然有效；旧存储可直接打开（回滚缺省关闭）并保留原有设备、配置差异与审计。打开时按已保存的操作结果与结果历史双向对账：每个已有结果的下载、安装或回滚操作恰好对应一条历史，每条历史也必须对应该设备该阶段实际已有的结果（设备、操作标识、阶段、成败、首次接受时间按同一时刻判断、失败原因一致，安装或回滚成功的版本一致）；已领取未完成、超时或因前批失败而未执行的阶段不要求有结果。缺少、多出、重复或内容矛盾一律返回 `ErrCorruptStorage`，拒绝打开整个存储并保留原文件，不通过删历史、补结果或改变进度掩盖矛盾。打开时还要核对 ready（等待安装）与 installing（正在安装）两种设备状态与本设备在本活动中保存的操作进度一致：两种状态都必须有本设备已接受的下载成功结果并停留在安装阶段；ready 表示安装从未领取、尚无安装结果（没有安装领取时间与结果时间是正常缺省），installing 表示安装已经领取但还没有接受安装结果。安装已经领取却停在 ready、已有安装成功或失败结果却仍是 ready/installing、阶段不是 install 或缺少本设备自己的下载成功结果，都返回 `ErrCorruptStorage` 拒绝打开整个存储；即使操作标识、时间和结果历史各自合法、设备当前版本已等于目标版本，也不能把已完成安装的设备解释成仍在等待安装。核对只依据该设备在该活动中的操作记录，不能借用同批其他设备的下载成功，也不能用当前版本等于目标版本替代下载或安装结果；设备离线、维护窗口已经结束或普通上报改变了当前版本都不构成损坏，也不自动补上安装结果。打开时还要按批次放行顺序核对升级结果：任一已经领取过下载的设备，首次领取下载时所有较早批次的设备都必须已成功完成安装，且每台前序设备首次接受安装成功结果的时间不晚于该次下载的首次领取时间；核对只按活动保存的升级结果，不能用设备当前是否在线或当前版本恰好等于目标版本替代，前序设备仅下载成功、正在安装、安装失败后等待回滚或已回滚成功都不满足放行条件，同一批设备互不等待。尚未领取下载的记录（后批正常等待、因前批失败而未执行、未领取即随截止超时）没有下载领取时间，不得仅因前批没有成功而拒绝。比较按实际时刻进行，时区写法不同的相同时刻合法。核对对仍在执行和已结束的活动都生效，下载后来成功、失败或超时都不能掩盖首次领取时越过前批的问题；违反顺序同样返回 `ErrCorruptStorage`，不补造前批成功结果、不撤销后批领取、不调整时间或删除活动。打开时还要对账活动整体结论与设备进度：任一设备仍处于等待下载、下载中、等待安装、安装中、等待回滚或回滚中时，活动必须是 running 且未结束（设备离线、维护窗口结束都不算设备已结束）；全部设备进入终态时活动必须已结束，且只有每台设备都 succeeded 才能 succeeded，其余组合（含失败、超时、未执行及回滚成功——回滚成功只是恢复原版本，不算本次升级成功）只能 failed。结论与进度矛盾同样返回 `ErrCorruptStorage`，不通过改写活动结论、改变设备状态或删除活动消除矛盾。

## 使用

```bash
go test ./...
```

测试通过表示基线包可以加载。后续能力在这个模块上继续增加。

### 示例：让一台模拟设备完成一次升级

下面的示例围绕**同一台设备** `sensor-001` 和**同一个活动** `cmp-2026-10-02` 展开，在本机从旧版本 `firmware-1.0` 升级到已登记的目标版本 `firmware-2.0`。它只使用 `shadow` 包的公开入口（`Open`、`Register`、`RegisterUpgrade`、`UpdateDesired`、`Report`、`CreateCampaign`、`GetDeviceWork`、`Claim`、`SubmitResult`、`Get`、`GetCampaign`、`Diff`），不依赖任何 `_test.go` 里的辅助代码，也不需要真实硬件或外网。完整代码在 [`examples/single-device-upgrade/main.go`](examples/single-device-upgrade/main.go)，可直接运行：

```bash
go run ./examples/single-device-upgrade
```

#### 时间线与配置（全部为 UTC，由调用方显式传时间）

| 时间 | 事件 |
| --- | --- |
| 11:45 | 升级前下发一份期望配置（修订号 0 → 1），程序留存一份副本 |
| 11:50 | 创建活动；窗口 12:00–13:00（`[start, end)`），截止 14:00 |
| 12:05 | 设备仍离线：能查到下载待办，但窗口内领取返回空操作且无错误 |
| 12:10 | 一次有效上报（序号 1）让设备上线 |
| 12:20 | 窗口内领取下载（活动内状态 `pending → downloading`） |
| 12:25 | 提交下载成功（`downloading → ready`，版本仍旧） |
| 12:26 | 演示：未领取就直接提交查到的安装标识，返回错误并应停止 |
| 12:30 | 窗口内领取安装（`ready → installing`） |
| 12:35 | 提交安装成功并附带合规上报，设备与活动成功结束 |

创建时间、维护窗口、截止时间彼此满足：创建（11:50）早于窗口开始（12:00），窗口开始早于窗口结束（13:00），截止（14:00）晚于创建；后续所有操作时间不倒退，领取都落在窗口内、结果都早于截止。

#### 完整代码

```go
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
```

#### 运行输出

下面是本机真实输出（临时目录名的随机后缀每次运行不同；操作标识是按“活动 + 设备 + 阶段”生成的稳定长度前缀编码，因此每次运行一致）：

```text
存储目录: /tmp/shadow-example-XXXXXXXXXX（程序退出时自动清理）

== 1. 登记升级目标版本与设备 ==
已登记目标版本 firmware-2.0，允许从 [firmware-1.0] 升级；设备 sensor-001 初始版本 firmware-1.0、离线

== 2. 升级前下发期望配置（升级过程中不应被改动） ==
期望配置已设置: {"telemetry":{"intervalSec":30},"mode":"safe"}（修订号 1）

== 3. 创建单设备升级活动 ==
活动 cmp-2026-10-02 已创建: 窗口 [2026-10-02T12:00:00Z, 2026-10-02T13:00:00Z)，截止 2026-10-02T14:00:00Z

== 4. 离线设备：GetDeviceWork 看得到待办，Claim 不派发 ==
    GetDeviceWork: version=firmware-1.0 online=false campaign="cmp-2026-10-02" 待办 kind=download claimed=false id=v2:0:14:cmp-2026-10-0210:sensor-001download
    Claim@12:05（设备离线，但时间在窗口内）: op=<nil> err=<nil>
    op 为 nil 且 err 为 nil：这是“暂时领不到”的正常等待，
    不是调用失败；绝不能拿空标识去 SubmitResult。

== 5. 设备用正整数序号上报，转为在线 ==
    设备影子: version=firmware-1.0 online=true lastSeq=1 修订号=1
      desired ={"telemetry":{"intervalSec":30},"mode":"safe"}
      reported={"telemetry":{"intervalSec":60},"mode":"safe"}
    GetDeviceWork: version=firmware-1.0 online=true campaign="cmp-2026-10-02" 待办 kind=download claimed=false id=v2:0:14:cmp-2026-10-0210:sensor-001download

== 6. 在维护窗口内领取下载 ==
    Claim@12:20: 领取到 kind=download id=v2:0:14:cmp-2026-10-0210:sensor-001download
    GetDeviceWork: version=firmware-1.0 online=true campaign="cmp-2026-10-02" 待办 kind=download claimed=true id=v2:0:14:cmp-2026-10-0210:sensor-001download
    活动内状态: status=downloading phase=download

== 7. 提交下载成功结果 ==
    设备影子: version=firmware-1.0 online=true lastSeq=1 修订号=1
      desired ={"telemetry":{"intervalSec":30},"mode":"safe"}
      reported={"telemetry":{"intervalSec":60},"mode":"safe"}
    GetDeviceWork: version=firmware-1.0 online=true campaign="cmp-2026-10-02" 待办 kind=install claimed=false id=v2:0:14:cmp-2026-10-0210:sensor-001install
    活动内状态: status=ready phase=install

== 8. 待办标识能查到，但未领取就提交结果会被拒绝 ==
    未领取先提交安装结果: err = shadow: operation has not been claimed: v2:0:14:cmp-2026-10-0210:sensor-001install
    （这是调用方自己的流程错误：真实程序收到该错误应打印并停止；
     本示例为了演示，改用正确的“先领取再提交”继续。）

== 9. 领取安装并提交成功（附带正整数序号、目标版本、完整 JSON 对象） ==
    Claim@12:30: 领取到 kind=install id=v2:0:14:cmp-2026-10-0210:sensor-001install
    安装成功已接受。

== 10. 升级完成后的状态核对 ==
    设备影子: version=firmware-2.0 online=true lastSeq=2 修订号=1
      desired ={"telemetry":{"intervalSec":30},"mode":"safe"}
      reported={"telemetry":{"intervalSec":30},"mode":"safe","features":{"beta":false}}
    GetDeviceWork: version=firmware-2.0 online=true campaign="" 待办=<无>
    活动: status=succeeded ended=true endedAt=2026-10-02T12:35:00Z
      设备 sensor-001: status=succeeded phase=install
    结果历史条数: 2（下载成功、安装成功各一条）
    期望配置核对:
      升级前期望: {"telemetry":{"intervalSec":30},"mode":"safe"}
      当前期望  : {"telemetry":{"intervalSec":30},"mode":"safe"}（修订号仍为 1，安装上报没有改写它）
      当前差异 1 条:
        /features: 期望存在=false 上报存在=true（上报内容变化只影响上报侧）

== 11. 维护窗口边界（独立的边界演示，不影响上面的活动） ==
    设备 sensor-edge-a Claim@11:55（在线、窗口未开始）: op=<nil> err=<nil>
    设备 sensor-edge-a Claim@12:30（在线、恰为窗口结束时刻）: op=<nil> err=<nil>
      GetDeviceWork: version=firmware-1.0 online=true campaign="cmp-edge-a" 待办 kind=download claimed=false id=v2:0:10:cmp-edge-a13:sensor-edge-adownload
      待办仍在（kind=download, claimed=false）：设备在正常等待下一个窗口，不是失败
    设备 sensor-edge-b Claim@12:00（恰为窗口开始时刻）: 领取到 kind=download（开始时刻包含）
```

#### 输出解释

**查询待办与领取是两件事（第 4、6、7、8 段）。** `GetDeviceWork` 是只读查询：第 4 段设备离线时它已经返回 `kind=download claimed=false` 的待办和稳定操作标识，但这**不替设备领取操作，也不推进活动**（活动内状态仍是 `pending`）。真正的状态变化只发生在 `Claim`：12:05 设备离线、即使时间在窗口内，`Claim` 也返回 `(nil, nil)`——没有操作、也没有错误。这是“暂时领不到、下次再来”的**正常等待**，与“调用失败”（`err != nil`）是两回事；代码显式判断 `op == nil`，**绝不会把空操作的标识传给 `SubmitResult`**。12:20 设备在线且在窗口内，`Claim` 才返回下载标识，再查同一设备时该标识的 `claimed` 变为 `true`、活动状态变为 `downloading`；已领取但未完成的操作重复领取/查询始终返回同一标识。第 8 段进一步说明“查得到 ≠ 领得到”：直接拿查询里看到的安装标识提交结果，返回 `ErrOperationNotClaimed`。真实调用方遇到这类错误应当打印并停止当前升级流程（示例里的 `must`/`log.Fatal` 就是这个约定），示例仅为教学才改用“先领取再提交”继续。

**下载成功不等于版本升级（第 7 段）。** 下载结果只需 `Success: true`，不附带上报。接受后设备进入 `ready`（等待安装），而影子里 `version` 仍是 `firmware-1.0`、`lastSeq=1`；此时 `GetDeviceWork` 改而显示 `kind=install claimed=false` 的安装待办。

**安装成功的附带上报与序号规则（第 9、10 段）。** 安装成功必须同时带上：正整数序号（这里 `Seq: 2`，必须**大于**已接受的序号 1；同序号内容不同会冲突、更小序号会被拒，规则与 `Report` 完全一致）、与活动目标**精确相等**的版本 `firmware-2.0`、以及一个**完整 JSON 对象**配置。三者缺一或不合规都会返回 `ErrInvalidReport` 且不改变任何状态。被接受后，影子与活动在同一次原子写入中更新：设备当前版本变为 `firmware-2.0`、上报配置变为安装时附带的对象、`lastSeq=2`；设备 `succeeded`，活动在 12:35 `succeeded` 结束，结果历史留下下载、安装两条记录；之后 `GetDeviceWork` 的 `campaign` 为空、`待办=<无>`，不再显示这个活动的任何待办。

**期望配置不被安装上报改动（第 2、10 段）。** 程序在升级前用 `expectedDesired` 留存了一份期望配置。安装附带的上报只写**上报侧**：升级后 `desired` 仍是升级前那份、修订号仍为 1，没有新增审计；变化的只有 `reported`。因此差异从升级前的 `/telemetry/intervalSec`（30 对 60）收敛为安装后的 1 条 `/features`——上报配置多了 `features` 字段而期望侧没有，期望本身原样保留。

**尚未领取操作的领取限制与窗口边界（第 4、11 段）。** 新阶段的首次领取要求“设备在线 **且** 当前时间位于维护窗口 `[start, end)`”，二者缺一即返回 `(nil, nil)` 且保留待办：第 4 段是离线、第 11 段 `sensor-edge-a` 是在线但不在窗口内（11:55 窗口未开始、12:30 恰为窗口结束时刻），两次都是空操作、无错误，`GetDeviceWork` 仍显示 `claimed=false` 的待办——设备在正常等待下一个窗口。窗口边界为**开始时刻包含、结束时刻不包含**：`sensor-edge-b` 在恰好 12:00（开始时刻）领取成功，而 12:30（结束时刻）领不到。注意区分：这些 `(nil, nil)` 是正常等待；时间缺失、时间倒退、活动已结束等才是返回非空错误的调用失败。另外，已领取但未完成的操作再查仍返回同一标识，并允许在窗口外、截止前提交结果；本示例的领取与提交全部在窗口内完成。

