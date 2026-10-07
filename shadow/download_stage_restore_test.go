package shadow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// buildPendingOpen 构造下载尚未领取（pending）的单设备活动，存储保持打开。
func buildPendingOpen(t *testing.T) (*Store, CampaignSpec) {
	t.Helper()
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	return s, spec
}

// buildDownloadingOpen 在 buildPendingOpen 之后领取下载（downloading），存储保持打开。
func buildDownloadingOpen(t *testing.T) (*Store, CampaignSpec, *Operation) {
	t.Helper()
	s, spec := buildPendingOpen(t)
	dl, err := s.Claim(spec.ID, "d1", upBase)
	if err != nil || dl == nil || dl.Kind != StageDownload {
		t.Fatalf("claim download: %v %+v", err, dl)
	}
	return s, spec, dl
}

// setupPending 构造 pending 设备并关闭存储，返回目录。
func setupPending(t *testing.T) (*Store, string, CampaignSpec) {
	t.Helper()
	s, spec := buildPendingOpen(t)
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return s, dir, spec
}

// appendDownloadHistory 在活动结果历史末尾追加一条本设备的下载结果记录。
func appendDownloadHistory(t *testing.T, dir, campaignID string, rec map[string]any) {
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

// downloadHistoryRec 组装一条与被改写下载操作匹配的下载结果历史条目。
func downloadHistoryRec(spec CampaignSpec, success bool, at string) map[string]any {
	rec := map[string]any{
		"deviceId":    "d1",
		"operationId": encodeOperationIDV2(0, spec.ID, "d1", StageDownload),
		"stage":       "download",
		"success":     success,
		"at":          at,
	}
	if !success {
		rec["reason"] = "download boom"
	}
	return rec
}

// TestRestorePendingDownloadingProgressRefused 保护重开存储时 pending（等待下载）
// 与 downloading（下载中）两种状态与本设备保存的下载操作进度之间的核对：两种状态
// 都必须停留在下载阶段；pending 表示本设备在本活动中的下载从未领取、尚无结果，
// downloading 表示下载已经领取但还没有接受任何结果。保存的状态与这些事实不符时
// Open 必须返回 ErrCorruptStorage 拒绝打开整个存储，即使操作标识、时间和结果历史
// 各自合法：下载已领取却仍为等待下载、下载未领取却标为下载中、已保存下载成功或
// 失败结果却仍使用任一状态，都是矛盾。核对只依据该设备在该活动中的下载操作记录，
// 同批其他设备已领取或下载成功不能替它补足进度，设备当前版本恰好等于目标版本也
// 不能代替下载领取与结果。
func TestRestorePendingDownloadingProgressRefused(t *testing.T) {
	// ---- pending（等待下载）的矛盾记录 -------------------------------------

	// 下载已经领取（有领取时间）却仍保存为 pending：等待下载要求从未领取。
	// 领取时刻落在维护窗口内且不早于时间基线，领取窗口与基线核对都会放行，
	// 只能由下载进度核对发现矛盾。
	t.Run("pending with download claimed", func(t *testing.T) {
		_, dir, spec := setupPending(t)
		original := mutateDeviceOp(t, dir, spec.ID, "d1", "download", func(o map[string]any) {
			o["claimed"] = true
			o["claimedAt"] = "2026-10-02T12:00:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 已有下载成功结果（配套历史自洽，时间基线覆盖全部记录）却保存为 pending：
	// 已有结果的设备不能解释成仍在等待下载。
	t.Run("pending with download success result", func(t *testing.T) {
		_, dir, spec := setupPending(t)
		mutateDeviceOp(t, dir, spec.ID, "d1", "download", func(o map[string]any) {
			o["claimed"] = true
			o["claimedAt"] = "2026-10-02T12:00:00Z"
			o["hasResult"] = true
			o["success"] = true
			o["at"] = "2026-10-02T12:01:00Z"
		})
		appendDownloadHistory(t, dir, spec.ID, downloadHistoryRec(spec, true, "2026-10-02T12:01:00Z"))
		// 把时间基线抬高到全部记录之后，排除时间基线核对的干扰。
		setCampaignLastTime(t, dir, spec.ID, "2026-10-02T12:02:00Z")
		original := readStoreBytesForTest(t, dir)
		assertReopenCorrupt(t, dir, original)
	})

	// 已有下载失败结果（配套失败历史自洽）也不能停在 pending。
	t.Run("pending with download failure result", func(t *testing.T) {
		_, dir, spec := setupPending(t)
		mutateDeviceOp(t, dir, spec.ID, "d1", "download", func(o map[string]any) {
			o["claimed"] = true
			o["claimedAt"] = "2026-10-02T12:00:00Z"
			o["hasResult"] = true
			o["success"] = false
			o["reason"] = "download boom"
			o["at"] = "2026-10-02T12:01:00Z"
		})
		appendDownloadHistory(t, dir, spec.ID, downloadHistoryRec(spec, false, "2026-10-02T12:01:00Z"))
		setCampaignLastTime(t, dir, spec.ID, "2026-10-02T12:02:00Z")
		original := readStoreBytesForTest(t, dir)
		assertReopenCorrupt(t, dir, original)
	})

	// pending 必须停留在下载阶段；保存成 install 阶段即矛盾。
	t.Run("pending with install phase", func(t *testing.T) {
		_, dir, spec := setupPending(t)
		original := setDeviceStatusPhase(t, dir, spec.ID, "d1", DevicePending, StageInstall)
		assertReopenCorrupt(t, dir, original)
	})

	// 开启回滚的活动中，pending 保存成 rollback 阶段同样矛盾（未开启回滚的
	// 活动由非回滚活动不得残留回滚进展的核对拒绝，这里覆盖开启回滚的情形）。
	t.Run("pending with rollback phase in rollback campaign", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		spec.RollbackOnFailure = true
		createCampaign(t, s, spec)
		dir := closeStore(t, s)
		original := setDeviceStatusPhase(t, dir, spec.ID, "d1", DevicePending, StageRollback)
		assertReopenCorrupt(t, dir, original)
	})

	// ---- downloading（下载中）的矛盾记录 ----------------------------------

	// 下载尚未领取却保存为 downloading（其余记录自洽）。
	t.Run("downloading without download claim", func(t *testing.T) {
		_, dir, spec := setupDownloading(t)
		original := mutateDeviceOp(t, dir, spec.ID, "d1", "download", func(o map[string]any) {
			o["claimed"] = false
			delete(o, "claimedAt")
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 已接受下载成功结果（配套历史自洽）却仍保存为 downloading：
	// 有了结果就不再是“下载中”。
	t.Run("downloading with download success result", func(t *testing.T) {
		_, dir, spec := setupDownloading(t)
		mutateDeviceOp(t, dir, spec.ID, "d1", "download", func(o map[string]any) {
			o["hasResult"] = true
			o["success"] = true
			o["at"] = "2026-10-02T12:11:00Z"
		})
		appendDownloadHistory(t, dir, spec.ID, downloadHistoryRec(spec, true, "2026-10-02T12:11:00Z"))
		setCampaignLastTime(t, dir, spec.ID, "2026-10-02T12:12:00Z")
		original := readStoreBytesForTest(t, dir)
		assertReopenCorrupt(t, dir, original)
	})

	// 已接受下载失败结果（配套失败历史自洽）也不能继续使用 downloading。
	t.Run("downloading with download failure result", func(t *testing.T) {
		_, dir, spec := setupDownloading(t)
		mutateDeviceOp(t, dir, spec.ID, "d1", "download", func(o map[string]any) {
			o["hasResult"] = true
			o["success"] = false
			o["reason"] = "download boom"
			o["at"] = "2026-10-02T12:11:00Z"
		})
		appendDownloadHistory(t, dir, spec.ID, downloadHistoryRec(spec, false, "2026-10-02T12:11:00Z"))
		setCampaignLastTime(t, dir, spec.ID, "2026-10-02T12:12:00Z")
		original := readStoreBytesForTest(t, dir)
		assertReopenCorrupt(t, dir, original)
	})

	// downloading 也必须停留在下载阶段。
	t.Run("downloading with install phase", func(t *testing.T) {
		_, dir, spec := setupDownloading(t)
		original := setDeviceStatusPhase(t, dir, spec.ID, "d1", DeviceDownloading, StageInstall)
		assertReopenCorrupt(t, dir, original)
	})

	// 同批另一台设备已领取下载并完整完成安装，也不能替本设备补足进度：
	// 本设备未领取下载却标为 downloading 仍是矛盾。
	t.Run("downloading without claim while batchmate succeeded", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"} // 同批，批大小 2
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		// d2 完整成功（下载+安装），d1 停在 pending。
		finishDevice(t, s, spec, "d2", upBase)
		if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DevicePending {
			t.Fatalf("precondition d1: %+v", d)
		}
		dir := closeStore(t, s)
		// 只把 d1 改成 downloading：d2 的领取与成功记录原样保留。
		original := setDeviceStatusPhase(t, dir, spec.ID, "d1", DeviceDownloading, StageDownload)
		assertReopenCorrupt(t, dir, original)
	})

	// 设备当前版本已被普通上报改成目标版本，也不能代替下载领取：
	// 未领取下载却标为 downloading 仍是矛盾。
	t.Run("downloading without claim even if version already target", func(t *testing.T) {
		s, spec := buildPendingOpen(t)
		// 普通上报把当前版本改成目标版本 v2：只改影子，不推进活动。
		if err := s.Report("d1", 2, upBase.Add(time.Minute), "v2", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		original := setDeviceStatusPhase(t, dir, spec.ID, "d1", DeviceDownloading, StageDownload)
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreUnfinishedDownloadResumes 保护正常的未完成下载在重开后仍能恢复使用：
// 尚未领取下载的设备保留未领取标记与原操作标识；已领取下载的设备保留原操作标识
// 和已领取标记，截止前再次领取仍返回同一下载标识，后续提交结果沿用现有规则。
// 设备离线、维护窗口已经结束或普通上报改变了当前版本，都不能单凭这些情况认定
// 进度损坏，也不能自动补上下载结果。
func TestRestoreUnfinishedDownloadResumes(t *testing.T) {
	// pending 原样重开（在线、窗口内）：未领取标记与操作标识保留，
	// 领取后状态进入 downloading，提交下载成功后进入 ready。
	t.Run("pending reopens and claims", func(t *testing.T) {
		s, spec := buildPendingOpen(t)
		wantWork, err := s.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)

		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("pending store must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if cv.Ended || findDevice(cv, "d1").Status != DevicePending ||
			findDevice(cv, "d1").Phase != StageDownload {
			t.Fatalf("pending progress must be preserved: %+v", cv)
		}
		if len(cv.Results) != 0 {
			t.Fatalf("no download result may be fabricated: %+v", cv.Results)
		}
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.CampaignID != spec.ID || w.Pending == nil ||
			w.Pending.Kind != StageDownload || w.PendingClaimed ||
			w.Pending.ID != wantWork.Pending.ID {
			t.Fatalf("unclaimed download todo lost: %+v want %+v", w, wantWork)
		}
		dl, err := s2.Claim(spec.ID, "d1", upBase.Add(time.Minute))
		if err != nil || dl == nil || dl.Kind != StageDownload || dl.ID != wantWork.Pending.ID {
			t.Fatalf("claim restored download: %v %+v", err, dl)
		}
		if err := s2.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(2 * time.Minute), Success: true,
		}); err != nil {
			t.Fatalf("submit download after reopen: %v", err)
		}
		cv = mustGetCampaign(s2, spec.ID)
		if cv.Ended || findDevice(cv, "d1").Status != DeviceReady ||
			findDevice(cv, "d1").Phase != StageInstall {
			t.Fatalf("download should complete into ready: %+v", cv)
		}
	})

	// pending：设备离线、维护窗口已结束后重开，仍保留未领取的下载待办；
	// 窗口外领取仍返回空操作，不补造任何结果。
	t.Run("pending survives offline and ended window", func(t *testing.T) {
		s, spec := buildPendingOpen(t)
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
			t.Fatalf("pending device offline past window must reopen: %v", err)
		}
		defer s2.Close()

		cv := mustGetCampaign(s2, spec.ID)
		if cv.Ended || findDevice(cv, "d1").Status != DevicePending ||
			findDevice(cv, "d1").Phase != StageDownload {
			t.Fatalf("pending progress must be preserved: %+v", cv)
		}
		if len(cv.Results) != 0 {
			t.Fatalf("no download result may be fabricated: %+v", cv.Results)
		}
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.CampaignID != spec.ID || w.Pending == nil ||
			w.Pending.Kind != StageDownload || w.PendingClaimed ||
			w.Pending.ID != wantWork.Pending.ID {
			t.Fatalf("unclaimed download todo lost: %+v want %+v", w, wantWork)
		}
		// 窗口已结束：即使重新上线，未领取的下载仍领不到（正常等待，不是错误），
		// 状态与待办保持不变。
		bringOnline(t, s2, "d1")
		op, err := s2.Claim(spec.ID, "d1", upBase.Add(91*time.Minute))
		if err != nil {
			t.Fatalf("claim outside window must be a no-op, not error: %v", err)
		}
		if op != nil {
			t.Fatalf("unclaimed download must not be dispatched past window: %+v", op)
		}
		w2, _ := s2.GetDeviceWork("d1")
		if w2.Pending == nil || w2.PendingClaimed || w2.Pending.ID != w.Pending.ID {
			t.Fatalf("pending download changed after out-of-window claim: %+v", w2)
		}
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DevicePending {
			t.Fatalf("pending status must remain after out-of-window claim")
		}
	})

	// downloading：原操作标识与已领取标记保留；截止前再次领取返回同一下载标识，
	// 已领取操作可在窗口外、截止前按现有规则提交下载结果。
	t.Run("downloading reopens and keeps claimed operation", func(t *testing.T) {
		s, spec, dl := buildDownloadingOpen(t)
		wantWork, _ := s.GetDeviceWork("d1")
		dir := closeStore(t, s)

		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("downloading store must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if cv.Ended || findDevice(cv, "d1").Status != DeviceDownloading ||
			findDevice(cv, "d1").Phase != StageDownload {
			t.Fatalf("downloading progress must be preserved: %+v", cv)
		}
		if len(cv.Results) != 0 {
			t.Fatalf("no download result may be fabricated: %+v", cv.Results)
		}
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending == nil || w.Pending.Kind != StageDownload || !w.PendingClaimed ||
			w.Pending.ID != dl.ID || w.Pending.ID != wantWork.Pending.ID {
			t.Fatalf("claimed download not restored: %+v want %+v", w, wantWork)
		}
		// 截止前再次领取（即使在窗口外）仍返回同一下载标识。
		again, err := s2.Claim(spec.ID, "d1", upBase.Add(75*time.Minute))
		if err != nil || again == nil || again.ID != dl.ID {
			t.Fatalf("re-claim must return same download id: %v %+v", err, again)
		}
		// 已领取操作可在窗口外、截止前提交结果：沿用原标识与现有规则。
		if err := s2.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(76 * time.Minute), Success: true,
		}); err != nil {
			t.Fatalf("submit download result after reopen: %v", err)
		}
		cv = mustGetCampaign(s2, spec.ID)
		if cv.Ended || findDevice(cv, "d1").Status != DeviceReady ||
			findDevice(cv, "d1").Phase != StageInstall {
			t.Fatalf("download should complete into ready: %+v", cv)
		}
		if len(cv.Results) != 1 || cv.Results[0].Stage != StageDownload ||
			cv.Results[0].OperationID != dl.ID {
			t.Fatalf("download result history: %+v", cv.Results)
		}
	})

	// 已领取下载的设备离线重开：离线不影响已领取待办的恢复。
	t.Run("downloading reopens while offline", func(t *testing.T) {
		s, spec, dl := buildDownloadingOpen(t)
		if err := s.SetOffline("d1"); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("offline downloading store must reopen: %v", err)
		}
		defer s2.Close()
		if sh, _ := s2.Get("d1"); sh.Online {
			t.Fatalf("offline state must be preserved")
		}
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending == nil || w.Pending.Kind != StageDownload || !w.PendingClaimed ||
			w.Pending.ID != dl.ID {
			t.Fatalf("claimed download todo lost while offline: %+v", w)
		}
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DeviceDownloading {
			t.Fatalf("downloading status lost")
		}
	})

	// 尚未领取下载没有领取时间、尚无下载结果没有结果时间：正常缺省记录
	// 必须原样打开，Open 不得改写文件补这些时间。
	t.Run("missing claim and result times are normal", func(t *testing.T) {
		// pending：下载从未领取，没有领取时间与结果时间。
		_, dir, spec := setupPending(t)
		path := filepath.Join(dir, storeFileName)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("pending store with default download times must reopen: %v", err)
		}
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DevicePending {
			t.Fatalf("pending status lost")
		}
		s2.Close()
		data2, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(data2) != string(data) {
			t.Fatalf("open must not fabricate missing times")
		}

		// downloading：下载已领取但尚无结果，没有结果时间。
		_, dir2, spec2 := setupDownloading(t)
		path2 := filepath.Join(dir2, storeFileName)
		data, err = os.ReadFile(path2)
		if err != nil {
			t.Fatal(err)
		}
		s3, err := Open(dir2)
		if err != nil {
			t.Fatalf("downloading store without result time must reopen: %v", err)
		}
		if findDevice(mustGetCampaign(s3, spec2.ID), "d1").Status != DeviceDownloading {
			t.Fatalf("downloading status lost")
		}
		s3.Close()
		data2, err = os.ReadFile(path2)
		if err != nil {
			t.Fatal(err)
		}
		if string(data2) != string(data) {
			t.Fatalf("open must not fabricate missing times")
		}
	})
}
