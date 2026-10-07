package shadow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setupPendingStore 构造单设备活动但设备从未领取下载（pending），关闭后返回目录。
func setupPendingStore(t *testing.T) (*Store, string, CampaignSpec) {
	t.Helper()
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return s, dir, spec
}

// buildPendingOpen 同 setupPendingStore，但存储保持打开。
func buildPendingOpen(t *testing.T) (*Store, CampaignSpec) {
	t.Helper()
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	return s, spec
}

// TestRestorePendingDownloadingProgressRefused 保护重开存储时 pending（等待下载）
// 与 downloading（下载中）两种状态与本设备保存的下载操作进度之间的核对：两种
// 状态都只能停留在下载阶段；pending 表示本设备在本活动中的下载从未领取、尚无
// 下载结果，downloading 表示下载已经领取但还没有接受任何下载结果。保存的状态
// 与这些事实不符时 Open 必须返回 ErrCorruptStorage 拒绝打开整个存储，即使操作
// 标识、时间和结果历史各自合法，也不能把已领取或已有下载成功/失败结果的设备
// 解释成仍在等待或正在下载；阶段写成安装或回滚同样矛盾。核对只依据该设备在
// 该活动中的下载记录，同批其他设备已领取或下载成功不能替它补足进度，设备当前
// 版本恰好等于目标版本、普通上报改变了版本或配置也不能代替下载领取与结果。
func TestRestorePendingDownloadingProgressRefused(t *testing.T) {
	// ---- pending（等待下载）的矛盾记录 -----------------------------------

	// 下载已经领取（领取时间、时间基线与领取窗口核对都放行）却仍保存为
	// pending：等待下载要求从未领取。
	t.Run("pending with download claimed", func(t *testing.T) {
		_, dir, spec := setupDownloading(t)
		// 合法的 downloading 记录只改状态：领取标记与领取时间保留，
		// 其余记录（操作标识、时间基线、窗口）全部自洽，只能由下载进度核对发现。
		original := setDeviceStatusPhase(t, dir, spec.ID, "d1", DevicePending, StageDownload)
		assertReopenCorrupt(t, dir, original)
	})

	// 已有下载成功结果（配套历史自洽）、设备待办已指向安装（ready）却被保存成
	// 下载阶段的 pending：接受过下载结果就不能继续停在等待下载。
	t.Run("pending with download success result", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		original := setDeviceStatusPhase(t, dir, spec.ID, "d1", DevicePending, StageDownload)
		assertReopenCorrupt(t, dir, original)
	})

	// 已有下载失败结果（同批 d2 仍在下载中，活动因而尚未结束）也不能停在 pending。
	t.Run("pending with download failure result", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"} // 同批，批大小 2
		spec.BatchSize = 2
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		dl1, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl1.ID,
			At: upBase.Add(3 * time.Minute), Success: false, Reason: "download boom",
		}); err != nil {
			t.Fatalf("d1 download fail: %v", err)
		}
		// d2 已领取未完成，使活动保持 running，排除活动结论核对的干扰。
		if _, err := s.Claim(spec.ID, "d2", upBase.Add(4*time.Minute)); err != nil {
			t.Fatalf("d2 claim: %v", err)
		}
		if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceFailed {
			t.Fatalf("precondition d1: %+v", d)
		}
		dir := closeStore(t, s)
		original := setDeviceStatusPhase(t, dir, spec.ID, "d1", DevicePending, StageDownload)
		assertReopenCorrupt(t, dir, original)
	})

	// pending 必须停留在下载阶段；保存成安装阶段即矛盾。
	t.Run("pending with install phase", func(t *testing.T) {
		_, dir, spec := setupPendingStore(t)
		original := setDeviceStatusPhase(t, dir, spec.ID, "d1", DevicePending, StageInstall)
		assertReopenCorrupt(t, dir, original)
	})

	// 阶段写成回滚同样矛盾（开启回滚的活动，其他回滚字段均为缺省）。
	t.Run("pending with rollback phase", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := rbSpec()
		createCampaign(t, s, spec)
		dir := closeStore(t, s)
		original := setDeviceStatusPhase(t, dir, spec.ID, "d1", DevicePending, StageRollback)
		assertReopenCorrupt(t, dir, original)
	})

	// ---- downloading（下载中）的矛盾记录 --------------------------------

	// 下载尚未领取却保存为 downloading（其余记录自洽）。
	t.Run("downloading without download claim", func(t *testing.T) {
		_, dir, spec := setupPendingStore(t)
		original := setDeviceStatusPhase(t, dir, spec.ID, "d1", DeviceDownloading, StageDownload)
		assertReopenCorrupt(t, dir, original)
	})

	// 已接受下载成功结果（ready，配套历史自洽）却仍保存为 downloading。
	t.Run("downloading with download success result", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		original := setDeviceStatusPhase(t, dir, spec.ID, "d1", DeviceDownloading, StageDownload)
		assertReopenCorrupt(t, dir, original)
	})

	// 已接受下载失败结果（同批 d2 仍在下载中，活动尚未结束）也不能继续 downloading。
	t.Run("downloading with download failure result", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 2
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		dl1, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl1.ID,
			At: upBase.Add(3 * time.Minute), Success: false, Reason: "download boom",
		}); err != nil {
			t.Fatalf("d1 download fail: %v", err)
		}
		if _, err := s.Claim(spec.ID, "d2", upBase.Add(4*time.Minute)); err != nil {
			t.Fatalf("d2 claim: %v", err)
		}
		dir := closeStore(t, s)
		original := setDeviceStatusPhase(t, dir, spec.ID, "d1", DeviceDownloading, StageDownload)
		assertReopenCorrupt(t, dir, original)
	})

	// downloading 也必须停留在下载阶段。
	t.Run("downloading with install phase", func(t *testing.T) {
		_, dir, spec := setupDownloading(t)
		original := setDeviceStatusPhase(t, dir, spec.ID, "d1", DeviceDownloading, StageInstall)
		assertReopenCorrupt(t, dir, original)
	})

	// 同批另一台设备已完整成功，也不能替本设备补足下载领取：本设备下载未领取、
	// 却被标成已领取且停在 pending，仍属损坏。
	t.Run("pending claimed while only batchmate progressed", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 2
		createCampaign(t, s, spec)
		bringOnline(t, s, "d2")
		// d2 完整成功；d1 始终离线、从未领取，保持 pending。
		finishDevice(t, s, spec, "d2", upBase.Add(2*time.Minute))
		if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DevicePending {
			t.Fatalf("precondition d1: %+v", d)
		}
		dir := closeStore(t, s)
		original := mutateDeviceOp(t, dir, spec.ID, "d1", "download", func(o map[string]any) {
			o["claimed"] = true
			o["claimedAt"] = "2026-10-02T12:02:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreUnfinishedDownloadResumes 保护正常的未开始/未完成下载在重开后仍能
// 恢复使用：尚未领取的下载仍显示为未领取；已领取且未完成的下载保留原操作标识
// 与已领取状态，截止前再次领取返回同一下载标识，后续提交结果沿用现有规则。
// 设备离线、维护窗口已经结束或普通上报改变当前版本/配置，都不能单凭这些情况
// 认定进度损坏，也不能自动补造下载结果或改写文件。
func TestRestoreUnfinishedDownloadResumes(t *testing.T) {
	// pending：未领取标记与操作标识保留；窗口内上线领取得到同一标识并进入
	// downloading，随后按现有规则提交下载成功。
	t.Run("pending reopens claims same operation and completes", func(t *testing.T) {
		_, dir, spec := setupPendingStore(t)
		wantID := encodeOperationIDV2(0, spec.ID, "d1", StageDownload)
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
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.CampaignID != spec.ID || w.Pending == nil ||
			w.Pending.Kind != StageDownload || w.PendingClaimed || w.Pending.ID != wantID {
			t.Fatalf("unclaimed download todo lost: %+v", w)
		}
		bringOnline(t, s2, "d1")
		dl, err := s2.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
		if err != nil || dl == nil || dl.Kind != StageDownload || dl.ID != wantID {
			t.Fatalf("claim after reopen must reuse same id: %v %+v", err, dl)
		}
		if d := findDevice(mustGetCampaign(s2, spec.ID), "d1"); d.Status != DeviceDownloading {
			t.Fatalf("device must be downloading after claim: %+v", d)
		}
		if err := s2.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(3 * time.Minute), Success: true,
		}); err != nil {
			t.Fatalf("submit download after reopen: %v", err)
		}
		if d := findDevice(mustGetCampaign(s2, spec.ID), "d1"); d.Status != DeviceReady {
			t.Fatalf("device must be ready after accepted download: %+v", d)
		}
	})

	// pending：设备离线、维护窗口已结束后重开，仍保留未领取的下载待办；
	// 窗口外即使重新上线，领取仍返回空操作，状态与待办保持不变，不补造结果。
	t.Run("pending survives offline and ended window", func(t *testing.T) {
		s, spec := buildPendingOpen(t)
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
			t.Fatalf("pending device past window must reopen: %v", err)
		}
		defer s2.Close()
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DevicePending {
			t.Fatalf("pending status lost")
		}
		w, _ := s2.GetDeviceWork("d1")
		if w.Pending == nil || w.Pending.Kind != StageDownload || w.PendingClaimed ||
			w.Pending.ID != wantWork.Pending.ID {
			t.Fatalf("unclaimed download todo lost: %+v want %+v", w, wantWork)
		}
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
		if d := findDevice(mustGetCampaign(s2, spec.ID), "d1"); d.Status != DevicePending {
			t.Fatalf("pending status must remain: %+v", d)
		}
		if rs := mustGetCampaign(s2, spec.ID).Results; len(rs) != 0 {
			t.Fatalf("no download result may be fabricated: %+v", rs)
		}
	})

	// 普通上报把当前版本改成目标版本 v2、配置改为非空：只改影子，不代替下载
	// 领取与结果；pending、未领取标记原样重开。
	t.Run("pending reopens even if report moved version to target", func(t *testing.T) {
		s, spec := buildPendingOpen(t)
		if err := s.Report("d1", 1, upBase.Add(time.Minute), "v2", json.RawMessage(`{"x":1}`)); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("pending device with moved version must reopen: %v", err)
		}
		defer s2.Close()
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DevicePending {
			t.Fatalf("plain report must not advance download progress")
		}
		w, _ := s2.GetDeviceWork("d1")
		if w.Pending == nil || w.PendingClaimed || w.Pending.Kind != StageDownload {
			t.Fatalf("download must remain unclaimed: %+v", w)
		}
	})

	// downloading：原操作标识与已领取标记保留；窗口结束后再次领取仍返回同一
	// 标识（已领取操作不要求在线或位于窗口），窗口外、截止前提交下载成功按
	// 现有规则接受。
	t.Run("downloading reopens reclaims same id and submits result", func(t *testing.T) {
		_, dir, spec := setupDownloading(t)
		wantID := encodeOperationIDV2(0, spec.ID, "d1", StageDownload)
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
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending == nil || w.Pending.Kind != StageDownload || !w.PendingClaimed ||
			w.Pending.ID != wantID {
			t.Fatalf("claimed download not restored: %+v", w)
		}
		// 维护窗口已结束（12:10 领取，13:15 再次查询）：仍返回同一标识。
		again, err := s2.Claim(spec.ID, "d1", upBase.Add(75*time.Minute))
		if err != nil || again == nil || again.ID != wantID {
			t.Fatalf("re-claim must return same operation: %v %+v", err, again)
		}
		// 已领取操作可在窗口外、截止前提交结果。
		if err := s2.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: wantID,
			At: upBase.Add(80 * time.Minute), Success: true,
		}); err != nil {
			t.Fatalf("submit download result after reopen: %v", err)
		}
		if d := findDevice(mustGetCampaign(s2, spec.ID), "d1"); d.Status != DeviceReady {
			t.Fatalf("device must be ready after resumed download: %+v", d)
		}
	})

	// 已领取下载的设备随后离线：离线不影响已领取待办的恢复。
	t.Run("downloading reopens while offline", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
		if dl, err := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute)); err != nil || dl == nil {
			t.Fatalf("claim download: %v %+v", err, dl)
		}
		if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceDownloading {
			t.Fatalf("precondition: %+v", d)
		}
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
		wantID := encodeOperationIDV2(0, spec.ID, "d1", StageDownload)
		if w.Pending == nil || w.Pending.Kind != StageDownload || !w.PendingClaimed ||
			w.Pending.ID != wantID {
			t.Fatalf("claimed download todo lost while offline: %+v", w)
		}
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DeviceDownloading {
			t.Fatalf("downloading status lost")
		}
	})

	// 同批另一台设备已完整成功、本设备从未领取：本设备仍按 pending 正常打开，
	// 别人的领取与下载成功不能替代它的进度。
	t.Run("pending reopens while batchmate succeeded", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 2
		createCampaign(t, s, spec)
		bringOnline(t, s, "d2")
		finishDevice(t, s, spec, "d2", upBase.Add(2*time.Minute))
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("pending device with succeeded batchmate must reopen: %v", err)
		}
		defer s2.Close()
		if d := findDevice(mustGetCampaign(s2, spec.ID), "d1"); d.Status != DevicePending {
			t.Fatalf("d1 must remain pending: %+v", d)
		}
		w, _ := s2.GetDeviceWork("d1")
		if w.Pending == nil || w.PendingClaimed || w.Pending.Kind != StageDownload {
			t.Fatalf("d1 unclaimed download todo lost: %+v", w)
		}
	})

	// 尚未领取下载没有领取时间、尚无下载结果没有结果时间：正常缺省记录必须原样
	// 打开，Open 不得改写文件补这些时间。
	t.Run("missing claim and result times are normal", func(t *testing.T) {
		_, dir, spec := setupPendingStore(t)
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
	})
}
