package shadow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// buildReadyOpen 构造下载已成功、安装尚未领取（ready）的单设备活动，存储保持打开。
func buildReadyOpen(t *testing.T) (*Store, CampaignSpec) {
	t.Helper()
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec.ID, "d1", upBase)
	if err != nil || dl == nil {
		t.Fatalf("claim download: %v %+v", err, dl)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("download result: %v", err)
	}
	return s, spec
}

// buildInstallingOpen 在 buildReadyOpen 之后再领取安装（installing），存储保持打开。
func buildInstallingOpen(t *testing.T) (*Store, CampaignSpec, *Operation) {
	s, spec := buildReadyOpen(t)
	in, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	if err != nil || in == nil || in.Kind != StageInstall {
		t.Fatalf("claim install: %v %+v", err, in)
	}
	return s, spec, in
}

// setupInstalling 构造 downloading->ready->installing 的设备并关闭存储，返回目录。
func setupInstalling(t *testing.T) (*Store, string, CampaignSpec) {
	t.Helper()
	s, spec, _ := buildInstallingOpen(t)
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return s, dir, spec
}

// setDeviceStatusPhase 直接改写存储文件中某台设备的状态与阶段。
func setDeviceStatusPhase(t *testing.T, dir, campaignID, deviceID, status, phase string) []byte {
	t.Helper()
	return rewriteStoreBytes(t, dir, func(doc map[string]any) {
		d := campaignDeviceDoc(doc, campaignID, deviceID)
		d["status"] = status
		d["phase"] = phase
	})
}

// readStoreBytesForTest 读取存储文件当前字节，供多次改写后捕获最终磁盘内容。
func readStoreBytesForTest(t *testing.T, dir string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, storeFileName))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// mutateDeviceOp 改写设备某阶段（download/install/rollback）的操作磁盘记录。
func mutateDeviceOp(t *testing.T, dir, campaignID, deviceID, op string, fn func(map[string]any)) []byte {
	t.Helper()
	return rewriteStoreBytes(t, dir, func(doc map[string]any) {
		d := campaignDeviceDoc(doc, campaignID, deviceID)
		fn(d[op].(map[string]any))
	})
}

// dropDeviceStageHistory 删除某设备某阶段的结果历史条目。
func dropDeviceStageHistory(t *testing.T, dir, campaignID, deviceID, stage string) {
	t.Helper()
	rewriteStoreBytes(t, dir, func(doc map[string]any) {
		camp := campaignDoc(doc, campaignID)
		raw := camp["results"].([]any)
		kept := make([]any, 0, len(raw))
		for _, r := range raw {
			rr := r.(map[string]any)
			if rr["deviceId"] == deviceID && rr["stage"] == stage {
				continue
			}
			kept = append(kept, rr)
		}
		camp["results"] = kept
	})
}

// appendInstallHistory 在活动结果历史末尾追加一条本设备的安装结果记录。
func appendInstallHistory(t *testing.T, dir, campaignID string, rec map[string]any) {
	t.Helper()
	rewriteStoreBytes(t, dir, func(doc map[string]any) {
		camp := campaignDoc(doc, campaignID)
		var rs []any
		if v, ok := camp["results"].([]any); ok {
			rs = v
		}
		camp["results"] = append(rs, rec)
	})
}

// installHistoryRec 组装一条与被改写安装操作匹配的安装结果历史条目。
func installHistoryRec(spec CampaignSpec, success bool, at string) map[string]any {
	rec := map[string]any{
		"deviceId":    "d1",
		"operationId": encodeOperationIDV2(0, spec.ID, "d1", StageInstall),
		"stage":       "install",
		"success":     success,
		"at":          at,
	}
	if success {
		rec["version"] = "v2"
	} else {
		rec["reason"] = "install boom"
	}
	return rec
}

// TestRestoreReadyInstallingProgressRefused 保护重开存储时 ready（等待安装）与
// installing（正在安装）两种状态与本设备保存的操作进度之间的核对：两种状态都
// 必须有本设备在本活动中已接受的下载成功结果并停留在安装阶段；ready 表示安装
// 从未领取、尚无安装结果，installing 表示安装已经领取但还没有接受安装结果。
// 保存的状态与这些事实不符时 Open 必须返回 ErrCorruptStorage 拒绝打开整个存储，
// 即使操作标识、时间和结果历史各自合法，也不能把已完成安装的设备解释成仍在
// 等待安装；已有安装成功或失败结果都不能继续使用这两种状态。核对只依据该设备
// 在该活动中的操作记录，不能借用同批其他设备的下载成功，也不能用当前版本恰好
// 等于目标版本替代下载结果。
func TestRestoreReadyInstallingProgressRefused(t *testing.T) {
	// ---- ready（等待安装）的矛盾记录 -------------------------------------

	// 安装已经领取（有领取时间）却仍保存为 ready：等待安装要求从未领取。
	// 领取时刻与已接受的下载结果同一时刻，时间基线与领取窗口核对都会放行，
	// 只能由 ready 进度核对发现矛盾。
	t.Run("ready with install claimed", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		original := mutateDeviceOp(t, dir, spec.ID, "d1", "install", func(o map[string]any) {
			o["claimed"] = true
			o["claimedAt"] = "2026-10-02T12:01:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 已有安装成功结果（配套历史、附带上报与设备影子全部自洽，当前版本已等于
	// 目标版本，时间基线也覆盖全部记录）却保存为 ready：已完成安装的设备不能
	// 解释成仍在等待安装。
	t.Run("ready with install success result", func(t *testing.T) {
		s, spec := buildReadyOpen(t)
		// 与安装结果同一时刻的普通上报：版本 v2、配置 {}、序号 2，使附带上报
		// 核对（序号等于最近序号时比对影子）完全通过；当前版本等于目标版本也
		// 不能替代状态核对。
		if err := s.Report("d1", 2, upBase.Add(3*time.Minute), "v2", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		mutateDeviceOp(t, dir, spec.ID, "d1", "install", func(o map[string]any) {
			o["claimed"] = true
			o["claimedAt"] = "2026-10-02T12:02:00Z"
			o["hasResult"] = true
			o["success"] = true
			o["at"] = "2026-10-02T12:03:00Z"
			o["resultSeq"] = 2.0
			o["resultVersion"] = "v2"
			o["resultConfig"] = map[string]any{}
		})
		appendInstallHistory(t, dir, spec.ID, installHistoryRec(spec, true, "2026-10-02T12:03:00Z"))
		// 把时间基线抬高到全部记录之后，排除时间基线核对的干扰。
		setCampaignLastTime(t, dir, spec.ID, "2026-10-02T12:04:00Z")
		original := readStoreBytesForTest(t, dir)
		assertReopenCorrupt(t, dir, original)
	})

	// 已有安装失败结果（配套失败历史自洽）也不能停在 ready。
	t.Run("ready with install failure result", func(t *testing.T) {
		s, spec := buildReadyOpen(t)
		dir := closeStore(t, s)
		mutateDeviceOp(t, dir, spec.ID, "d1", "install", func(o map[string]any) {
			o["claimed"] = true
			o["claimedAt"] = "2026-10-02T12:02:00Z"
			o["hasResult"] = true
			o["success"] = false
			o["reason"] = "install boom"
			o["at"] = "2026-10-02T12:03:00Z"
		})
		appendInstallHistory(t, dir, spec.ID, installHistoryRec(spec, false, "2026-10-02T12:03:00Z"))
		setCampaignLastTime(t, dir, spec.ID, "2026-10-02T12:04:00Z")
		original := readStoreBytesForTest(t, dir)
		assertReopenCorrupt(t, dir, original)
	})

	// ready 必须停留在安装阶段；保存成 download 阶段即矛盾。
	t.Run("ready with download phase", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		original := setDeviceStatusPhase(t, dir, spec.ID, "d1", DeviceReady, StageDownload)
		assertReopenCorrupt(t, dir, original)
	})

	// ready 缺少本设备自己的下载成功结果：即使同批另一台设备已下载成功并完整
	// 完成安装，也不能借用别人的下载结果把本设备当作等待安装。
	t.Run("ready without own download while batchmate succeeded", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"} // 同批，批大小 2
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		dl1, _ := s.Claim(spec.ID, "d1", upBase)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl1.ID,
			At: upBase.Add(time.Minute), Success: true,
		}); err != nil {
			t.Fatalf("d1 download: %v", err)
		}
		// d2 完整成功（下载+安装），d1 停在 ready。
		finishDevice(t, s, spec, "d2", upBase.Add(2*time.Minute))
		if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceReady {
			t.Fatalf("precondition d1: %+v", d)
		}
		dir := closeStore(t, s)
		// 删去 d1 自己的下载成功结果与对应历史；d2 的记录原样保留。
		mutateDeviceOp(t, dir, spec.ID, "d1", "download", func(o map[string]any) {
			o["hasResult"] = false
			o["success"] = false
		})
		dropDeviceStageHistory(t, dir, spec.ID, "d1", StageDownload)
		original := readStoreBytesForTest(t, dir)
		assertReopenCorrupt(t, dir, original)
	})

	// 缺少本设备的下载成功结果，即使设备当前版本已被普通上报改成目标版本，
	// 也不能替代下载结果打开存储。
	t.Run("ready without download even if version already target", func(t *testing.T) {
		s, spec := buildReadyOpen(t)
		// 普通上报把当前版本改成目标版本 v2：只改影子，不推进活动。
		if err := s.Report("d1", 2, upBase.Add(2*time.Minute), "v2", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		mutateDeviceOp(t, dir, spec.ID, "d1", "download", func(o map[string]any) {
			o["hasResult"] = false
			o["success"] = false
		})
		dropDeviceStageHistory(t, dir, spec.ID, "d1", StageDownload)
		original := readStoreBytesForTest(t, dir)
		assertReopenCorrupt(t, dir, original)
	})

	// ---- installing（正在安装）的矛盾记录 --------------------------------

	// 安装尚未领取却保存为 installing（其余记录自洽）。
	t.Run("installing without install claim", func(t *testing.T) {
		_, dir, spec := setupInstalling(t)
		original := mutateDeviceOp(t, dir, spec.ID, "d1", "install", func(o map[string]any) {
			o["claimed"] = false
			delete(o, "claimedAt")
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 已接受安装成功结果（配套历史、附带上报与设备影子自洽）却仍保存为
	// installing：有了结果就不再是“正在安装”。
	t.Run("installing with install success result", func(t *testing.T) {
		s, spec, _ := buildInstallingOpen(t)
		if err := s.Report("d1", 2, upBase.Add(3*time.Minute), "v2", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		mutateDeviceOp(t, dir, spec.ID, "d1", "install", func(o map[string]any) {
			o["hasResult"] = true
			o["success"] = true
			o["at"] = "2026-10-02T12:03:00Z"
			o["resultSeq"] = 2.0
			o["resultVersion"] = "v2"
			o["resultConfig"] = map[string]any{}
		})
		appendInstallHistory(t, dir, spec.ID, installHistoryRec(spec, true, "2026-10-02T12:03:00Z"))
		setCampaignLastTime(t, dir, spec.ID, "2026-10-02T12:04:00Z")
		original := readStoreBytesForTest(t, dir)
		assertReopenCorrupt(t, dir, original)
	})

	// 已接受安装失败结果（配套失败历史自洽）也不能继续使用 installing。
	t.Run("installing with install failure result", func(t *testing.T) {
		s, spec, _ := buildInstallingOpen(t)
		dir := closeStore(t, s)
		mutateDeviceOp(t, dir, spec.ID, "d1", "install", func(o map[string]any) {
			o["hasResult"] = true
			o["success"] = false
			o["reason"] = "install boom"
			o["at"] = "2026-10-02T12:03:00Z"
		})
		appendInstallHistory(t, dir, spec.ID, installHistoryRec(spec, false, "2026-10-02T12:03:00Z"))
		setCampaignLastTime(t, dir, spec.ID, "2026-10-02T12:04:00Z")
		original := readStoreBytesForTest(t, dir)
		assertReopenCorrupt(t, dir, original)
	})

	// installing 也必须停留在安装阶段。
	t.Run("installing with download phase", func(t *testing.T) {
		_, dir, spec := setupInstalling(t)
		original := setDeviceStatusPhase(t, dir, spec.ID, "d1", DeviceInstalling, StageDownload)
		assertReopenCorrupt(t, dir, original)
	})

	// installing 同样必须有本设备自己接受的下载成功结果。
	t.Run("installing without own download success", func(t *testing.T) {
		_, dir, spec := setupInstalling(t)
		mutateDeviceOp(t, dir, spec.ID, "d1", "download", func(o map[string]any) {
			o["hasResult"] = false
			o["success"] = false
		})
		dropDeviceStageHistory(t, dir, spec.ID, "d1", StageDownload)
		original := readStoreBytesForTest(t, dir)
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreUnfinishedInstallResumes 保护正常的未完成安装在重开后仍能恢复使用：
// 等待安装的设备保留原安装待办及未领取标记；已领取安装的设备保留原操作标识和
// 已领取标记，后续仍能按现有规则提交结果。设备离线、维护窗口已经结束或普通上报
// 改变当前版本，都不能单凭这些情况认定进度损坏，也不能自动补上安装结果。
func TestRestoreUnfinishedInstallResumes(t *testing.T) {
	// ready：设备离线、维护窗口已结束后重开，仍保留未领取的安装待办；
	// 窗口外领取仍返回空操作，不补造任何结果。
	t.Run("ready survives offline and ended window", func(t *testing.T) {
		s, spec := buildReadyOpen(t)
		// 离线并把时间推进到维护窗口结束之后、截止之前。
		if err := s.SetOffline("d1"); err != nil {
			t.Fatal(err)
		}
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(90*time.Minute)); err != nil {
			t.Fatal(err)
		}
		wantWork, err := s.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)

		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("ready device offline past window must reopen: %v", err)
		}
		defer s2.Close()

		cv := mustGetCampaign(s2, spec.ID)
		if cv.Ended || findDevice(cv, "d1").Status != DeviceReady ||
			findDevice(cv, "d1").Phase != StageInstall {
			t.Fatalf("ready progress must be preserved: %+v", cv)
		}
		if len(cv.Results) != 1 || cv.Results[0].Stage != StageDownload {
			t.Fatalf("no install result may be fabricated: %+v", cv.Results)
		}
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.CampaignID != spec.ID || w.Pending == nil ||
			w.Pending.Kind != StageInstall || w.PendingClaimed ||
			w.Pending.ID != wantWork.Pending.ID {
			t.Fatalf("unclaimed install todo lost: %+v want %+v", w, wantWork)
		}
		// 窗口已结束：即使重新上线，未领取的安装仍领不到（正常等待，不是错误），
		// 状态与待办保持不变。
		bringOnline(t, s2, "d1")
		op, err := s2.Claim(spec.ID, "d1", upBase.Add(91*time.Minute))
		if err != nil {
			t.Fatalf("claim outside window must be a no-op, not error: %v", err)
		}
		if op != nil {
			t.Fatalf("unclaimed install must not be dispatched past window: %+v", op)
		}
		w2, _ := s2.GetDeviceWork("d1")
		if w2.Pending == nil || w2.PendingClaimed || w2.Pending.ID != w.Pending.ID {
			t.Fatalf("pending install changed after out-of-window claim: %+v", w2)
		}
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DeviceReady {
			t.Fatalf("ready status must remain after out-of-window claim")
		}
	})

	// ready 原样重开（在线、窗口内）：未领取标记与操作标识保留，
	// 领取后可按现有规则提交安装成功，活动正常成功结束。
	t.Run("ready reopens and completes", func(t *testing.T) {
		s, spec := buildReadyOpen(t)
		wantWork, _ := s.GetDeviceWork("d1")
		dir := closeStore(t, s)

		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("ready store must reopen: %v", err)
		}
		defer s2.Close()
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending == nil || w.Pending.Kind != StageInstall || w.PendingClaimed ||
			w.Pending.ID != wantWork.Pending.ID {
			t.Fatalf("pending install not restored: %+v want %+v", w, wantWork)
		}
		in, err := s2.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
		if err != nil || in == nil || in.Kind != StageInstall || in.ID != wantWork.Pending.ID {
			t.Fatalf("claim restored install: %v %+v", err, in)
		}
		if err := s2.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
			At: upBase.Add(3 * time.Minute), Success: true,
			Seq: 2, Version: "v2", Config: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("submit install after reopen: %v", err)
		}
		cv := mustGetCampaign(s2, spec.ID)
		if !cv.Ended || cv.Status != CampaignSucceeded ||
			findDevice(cv, "d1").Status != DeviceSucceeded {
			t.Fatalf("campaign should succeed after resumed install: %+v", cv)
		}
	})

	// installing：原操作标识与已领取标记保留；领取后的普通上报改变了影子，
	// 重开后仍能用原标识按现有规则提交安装成功（已领取操作可在窗口外提交）。
	t.Run("installing reopens and submits result after plain report", func(t *testing.T) {
		s, spec, in := buildInstallingOpen(t)
		// 领取安装后来了一次普通上报：只改影子，不替代安装结果、不清待办。
		if err := s.Report("d1", 2, upBase.Add(3*time.Minute), "v1", json.RawMessage(`{"x":1}`)); err != nil {
			t.Fatal(err)
		}
		wantWork, _ := s.GetDeviceWork("d1")
		dir := closeStore(t, s)

		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("installing store must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if cv.Ended || findDevice(cv, "d1").Status != DeviceInstalling ||
			findDevice(cv, "d1").Phase != StageInstall {
			t.Fatalf("installing progress must be preserved: %+v", cv)
		}
		if len(cv.Results) != 1 {
			t.Fatalf("no install result may be fabricated: %+v", cv.Results)
		}
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending == nil || w.Pending.Kind != StageInstall || !w.PendingClaimed ||
			w.Pending.ID != in.ID || w.Pending.ID != wantWork.Pending.ID {
			t.Fatalf("claimed install not restored: %+v want %+v", w, wantWork)
		}
		// 已领取操作可在窗口外、截止前提交结果：沿用原标识与现有序号规则。
		if err := s2.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
			At: upBase.Add(75 * time.Minute), Success: true,
			Seq: 3, Version: "v2", Config: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("submit install result after reopen: %v", err)
		}
		view, _ := s2.Get("d1")
		if view.Version != "v2" || view.LastSeq != 3 {
			t.Fatalf("shadow not updated by resumed install success: %+v", view)
		}
		if cv2 := mustGetCampaign(s2, spec.ID); !cv2.Ended || cv2.Status != CampaignSucceeded {
			t.Fatalf("campaign should succeed: %+v", cv2)
		}
	})

	// 已领取安装的设备离线重开：离线不影响已领取待办的恢复。
	t.Run("installing reopens while offline", func(t *testing.T) {
		s, spec, in := buildInstallingOpen(t)
		if err := s.SetOffline("d1"); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("offline installing store must reopen: %v", err)
		}
		defer s2.Close()
		if sh, _ := s2.Get("d1"); sh.Online {
			t.Fatalf("offline state must be preserved")
		}
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending == nil || w.Pending.Kind != StageInstall || !w.PendingClaimed ||
			w.Pending.ID != in.ID {
			t.Fatalf("claimed install todo lost while offline: %+v", w)
		}
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DeviceInstalling {
			t.Fatalf("installing status lost")
		}
	})

	// 尚未领取安装没有安装领取时间、尚无安装结果没有结果时间：正常缺省记录
	// 必须原样打开，Open 不得改写文件补这些时间。
	t.Run("missing claim and result times are normal", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		path := filepath.Join(dir, storeFileName)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("ready store with default install times must reopen: %v", err)
		}
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DeviceReady {
			t.Fatalf("ready status lost")
		}
		s2.Close()
		data2, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data2) != string(data) {
			t.Fatalf("open must not fabricate missing times")
		}
	})
}
