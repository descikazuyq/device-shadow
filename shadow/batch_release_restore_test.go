package shadow

import (
	"encoding/json"
	"testing"
	"time"
)

// setDownloadClaimedAt 直接改写设备首次领取下载的时间（RFC3339 字符串，
// 可用不同时区写法），用来构造按正常流程无法产生的越批领取记录。
func setDownloadClaimedAt(t *testing.T, dir, campaignID, deviceID, claimedAt string) []byte {
	t.Helper()
	return rewriteStoreBytes(t, dir, func(doc map[string]any) {
		d := campaignDeviceDoc(doc, campaignID, deviceID)
		if d == nil {
			t.Fatalf("device %s missing in campaign %s", deviceID, campaignID)
		}
		dl := d["download"].(map[string]any)
		dl["claimed"] = true
		dl["claimedAt"] = claimedAt
	})
}

// buildBatchGateStore 构造两批活动（批大小 2）：第一批 b1、b2 分别在
// 12:10、12:20 开始推进并安装成功（安装成功结果时间为 12:13、12:23），
// 第二批 b3 于 12:25 首次领取下载。除 b3 的下载领取时间外其余记录都按
// 正常流程产生，测试通过改写该领取时间构造越批数据。
func buildBatchGateStore(t *testing.T) (*Store, string, CampaignSpec) {
	t.Helper()
	s := setupUpgrade(t, "b1", "b2", "b3")
	spec := upSpec()
	spec.Devices = []string{"b1", "b2", "b3"}
	spec.BatchSize = 2
	createCampaign(t, s, spec)
	for _, id := range spec.Devices {
		bringOnline(t, s, id)
	}
	finishDevice(t, s, spec, "b1", upBase.Add(10*time.Minute))
	finishDevice(t, s, spec, "b2", upBase.Add(20*time.Minute))
	dl, err := s.Claim(spec.ID, "b3", upBase.Add(25*time.Minute))
	if err != nil || dl == nil {
		t.Fatalf("claim b3 download: %v %+v", err, dl)
	}
	return s, s.dir, spec
}

// TestRestoreBatchReleaseOrderingRefused 保护打开存储时的批次放行核对：
// 已领取下载的设备，其首次领取时间不得早于任一早批设备的首次安装成功时间。
// 早批设备仅下载成功、正在安装、安装失败后等待回滚或已回滚都不算放行。
func TestRestoreBatchReleaseOrderingRefused(t *testing.T) {
	// 题设示例：第一批两台设备分别在 12:13、12:23 安装成功，第二批设备
	// 12:15 首次领取下载，即使活动最新时间足够晚（12:25）也必须拒绝。
	t.Run("claimed before slowest predecessor install succeeded", func(t *testing.T) {
		s, dir, spec := buildBatchGateStore(t)
		s.Close()
		original := setDownloadClaimedAt(t, dir, spec.ID, "b3", "2026-10-02T12:15:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 活动已结束、越批领取的下载后来成功：不能用最终成功掩盖首次领取时越批。
	t.Run("later download eventually succeeded", func(t *testing.T) {
		s, dir, spec := buildBatchGateStore(t)
		dl := findDevice(mustGetCampaign(s, spec.ID), "b3").DownloadID
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "b3", OperationID: dl,
			At: upBase.Add(26 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		in, err := s.Claim(spec.ID, "b3", upBase.Add(27*time.Minute))
		if err != nil || in == nil {
			t.Fatalf("claim b3 install: %v %+v", err, in)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "b3", OperationID: in.ID,
			At: upBase.Add(28 * time.Minute), Success: true,
			Seq: 2, Version: "v2", Config: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
		cv := mustGetCampaign(s, spec.ID)
		if !cv.Ended || cv.Status != CampaignSucceeded {
			t.Fatalf("precondition: campaign succeeded: %+v", cv)
		}
		s.Close()
		original := setDownloadClaimedAt(t, dir, spec.ID, "b3", "2026-10-02T12:15:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 越批领取的下载后来失败、设备以失败结束：同样不能掩盖。
	t.Run("later download later failed", func(t *testing.T) {
		s, dir, spec := buildBatchGateStore(t)
		dl := findDevice(mustGetCampaign(s, spec.ID), "b3").DownloadID
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "b3", OperationID: dl,
			At: upBase.Add(26 * time.Minute), Success: false, Reason: "boom",
		}); err != nil {
			t.Fatal(err)
		}
		cv := mustGetCampaign(s, spec.ID)
		if !cv.Ended || findDevice(cv, "b3").Status != DeviceFailed {
			t.Fatalf("precondition: b3 failed: %+v", cv)
		}
		s.Close()
		original := setDownloadClaimedAt(t, dir, spec.ID, "b3", "2026-10-02T12:15:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 越批领取后随截止超时：超时终态也不能掩盖首次领取越批。
	t.Run("later download later timed out", func(t *testing.T) {
		s, dir, spec := buildBatchGateStore(t)
		if err := s.AdvanceCampaign(spec.ID, spec.Deadline); err != nil {
			t.Fatal(err)
		}
		cv := mustGetCampaign(s, spec.ID)
		if !cv.Ended || findDevice(cv, "b3").Status != DeviceTimeout {
			t.Fatalf("precondition: b3 timeout: %+v", cv)
		}
		s.Close()
		original := setDownloadClaimedAt(t, dir, spec.ID, "b3", "2026-10-02T12:15:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 前序设备只走到“下载成功、等待安装”（ready），不算成功完成安装：
	// 后批设备已领取下载即损坏，即使领取时间晚于活动其他全部记录。
	t.Run("predecessor only downloaded is not released", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2", "d3")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2", "d3"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		for _, id := range spec.Devices {
			bringOnline(t, s, id)
		}
		finishDevice(t, s, spec, "d1", upBase)
		// d2 只完成下载，停在 ready（安装未领取）。
		dl2, _ := s.Claim(spec.ID, "d2", upBase.Add(20*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d2", OperationID: dl2.ID,
			At: upBase.Add(21 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		// 推进时间基线，使被改写的 d3 领取时间不早于基线之外的其他检查。
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(40*time.Minute)); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			d := campaignDeviceDoc(doc, spec.ID, "d3")
			dl := d["download"].(map[string]any)
			dl["claimed"] = true
			dl["claimedAt"] = "2026-10-02T12:30:00Z"
			d["status"] = DeviceDownloading
			d["phase"] = StageDownload
			d["at"] = "2026-10-02T12:30:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 前序设备安装失败后等待回滚：即使开启了回滚，也不是“安装成功”，
	// 后批被跳过设备若出现已领取下载记录，必须拒绝打开。
	t.Run("predecessor awaiting rollback is not released", func(t *testing.T) {
		s := setupUpgrade(t, "r1", "r2")
		spec := upSpec()
		spec.ID = "cmp-rb"
		spec.Devices = []string{"r1", "r2"}
		spec.BatchSize = 1
		spec.RollbackOnFailure = true
		createCampaign(t, s, spec)
		bringOnline(t, s, "r1")
		bringOnline(t, s, "r2")
		dl, _ := s.Claim(spec.ID, "r1", upBase)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "r1", OperationID: dl.ID,
			At: upBase.Add(time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		in, _ := s.Claim(spec.ID, "r1", upBase.Add(2*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "r1", OperationID: in.ID,
			At: upBase.Add(3 * time.Minute), Success: false, Reason: "install boom",
		}); err != nil {
			t.Fatal(err)
		}
		cv := mustGetCampaign(s, spec.ID)
		if findDevice(cv, "r1").Status != DeviceAwaitingRollback ||
			findDevice(cv, "r2").Status != DeviceSkipped {
			t.Fatalf("precondition: %+v", cv.Devices)
		}
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(40*time.Minute)); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			d := campaignDeviceDoc(doc, spec.ID, "r2")
			// skipped 终态改写为“已领取下载中”，并补上首次领取下载时锁定
			// 的回滚目标，使其单独看是一条自洽的下载中记录；矛盾只在批次
			// 放行顺序上。
			d["status"] = DeviceDownloading
			d["phase"] = StageDownload
			d["at"] = "2026-10-02T12:30:00Z"
			d["rollbackTarget"] = "v1"
			delete(d, "reason")
			dl := d["download"].(map[string]any)
			dl["claimed"] = true
			dl["claimedAt"] = "2026-10-02T12:30:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreBatchReleaseOrderingAccepted 保护合法的批次与时间记录不被误拒。
func TestRestoreBatchReleaseOrderingAccepted(t *testing.T) {
	// 题设示例的合法边界：第二批设备在 12:23（等于最后一台前序设备安装
	// 成功时刻）首次领取下载，必须接受。
	t.Run("claimed exactly at slowest predecessor success time", func(t *testing.T) {
		s, dir, spec := buildBatchGateStore(t)
		s.Close()
		setDownloadClaimedAt(t, dir, spec.ID, "b3", "2026-10-02T12:23:00Z")
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("claim at equal instant must open: %v", err)
		}
		defer s2.Close()
		w, err := s2.GetDeviceWork("b3")
		if err != nil {
			t.Fatal(err)
		}
		if w.CampaignID != spec.ID || w.Pending == nil ||
			w.Pending.Kind != StageDownload || !w.PendingClaimed {
			t.Fatalf("b3 claimed download must be preserved: %+v", w)
		}
	})

	// 相同时刻的不同时区写法不能被判为乱序：20:23+08:00 即 12:23Z。
	t.Run("equal instant with different timezone notation", func(t *testing.T) {
		s, dir, spec := buildBatchGateStore(t)
		s.Close()
		setDownloadClaimedAt(t, dir, spec.ID, "b3", "2026-10-02T20:23:00+08:00")
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("same instant in another timezone must open: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "b3")
		if d.Status != DeviceDownloading {
			t.Fatalf("b3 progress lost: %+v", d)
		}
	})

	// 不同时区写法表示的更早时刻仍须拒绝（13:22+01:00 即 12:22Z）。
	t.Run("earlier instant across timezone refused", func(t *testing.T) {
		s, dir, spec := buildBatchGateStore(t)
		s.Close()
		original := setDownloadClaimedAt(t, dir, spec.ID, "b3", "2026-10-02T13:22:00+01:00")
		assertReopenCorrupt(t, dir, original)
	})

	// 正常走完全部批次的活动重开不受影响，批次顺序、终态与历史保留。
	t.Run("legal multi batch success reopens", func(t *testing.T) {
		s, dir, spec := buildBatchGateStore(t)
		dl := findDevice(mustGetCampaign(s, spec.ID), "b3").DownloadID
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "b3", OperationID: dl,
			At: upBase.Add(26 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		in, _ := s.Claim(spec.ID, "b3", upBase.Add(27*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "b3", OperationID: in.ID,
			At: upBase.Add(28 * time.Minute), Success: true,
			Seq: 2, Version: "v2", Config: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
		want := mustGetCampaign(s, spec.ID)
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("legal multi-batch campaign must reopen: %v", err)
		}
		defer s2.Close()
		got := mustGetCampaign(s2, spec.ID)
		if !got.Ended || got.Status != CampaignSucceeded || len(got.Results) != len(want.Results) {
			t.Fatalf("campaign state changed: %+v", got)
		}
	})

	// 同批设备各自推进：一台已领取下载未完成，同批另一台随后领取，
	// 不需要等待同批设备成功，重开必须接受。
	t.Run("same batch devices do not gate each other", func(t *testing.T) {
		s := setupUpgrade(t, "a1", "a2")
		spec := upSpec()
		spec.Devices = []string{"a1", "a2"}
		spec.BatchSize = 2
		createCampaign(t, s, spec)
		bringOnline(t, s, "a1")
		bringOnline(t, s, "a2")
		if _, err := s.Claim(spec.ID, "a1", upBase.Add(5*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Claim(spec.ID, "a2", upBase.Add(6*time.Minute)); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("same-batch claims must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if findDevice(cv, "a1").Status != DeviceDownloading ||
			findDevice(cv, "a2").Status != DeviceDownloading {
			t.Fatalf("both downloads must remain claimed: %+v", cv.Devices)
		}
	})

	// 后批正常等待、尚未领取下载：前批是否成功都不能成为拒绝理由。
	t.Run("later batch never claimed while predecessor unfinished", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		// 第一批仅领取下载、未完成；第二批一直 pending。
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute)); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("waiting later batch must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if findDevice(cv, "d2").Status != DevicePending {
			t.Fatalf("d2 must stay pending: %+v", findDevice(cv, "d2"))
		}
	})

	// 前批失败导致后批未执行（skipped）：后批没有下载领取时间，正常打开。
	t.Run("later batch skipped after predecessor failure reopens", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(6 * time.Minute), Success: false, Reason: "boom",
		}); err != nil {
			t.Fatal(err)
		}
		cv0 := mustGetCampaign(s, spec.ID)
		if !cv0.Ended || findDevice(cv0, "d2").Status != DeviceSkipped {
			t.Fatalf("precondition: %+v", cv0)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("skipped later batch must reopen: %v", err)
		}
		defer s2.Close()
	})

	// 尚未领取下载就随活动截止超时：没有下载领取时间，正常打开。
	t.Run("unclaimed device times out at deadline reopens", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2", "d3")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2", "d3"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		// d1 完成；d2 已领取下载未完成；d3 从未领取。
		finishDevice(t, s, spec, "d1", upBase.Add(5*time.Minute))
		if _, err := s.Claim(spec.ID, "d2", upBase.Add(10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.AdvanceCampaign(spec.ID, spec.Deadline); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("deadline-timeout campaign must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if !cv.Ended || findDevice(cv, "d2").Status != DeviceTimeout ||
			findDevice(cv, "d3").Status != DeviceTimeout {
			t.Fatalf("timeout states lost: %+v", cv.Devices)
		}
	})
}
