package shadow

import (
	"encoding/json"
	"testing"
	"time"
)

// claimDownloadInDoc 把活动记录里 deviceID 的下载操作改写为已在 at 领取
// （设备记为下载中），并把活动时间基线抬到 at，用于构造“后批越过前批
// 领取下载”的损坏数据。开启回滚的活动同时补上领取时锁定的回滚目标。
func claimDownloadInDoc(doc map[string]any, campaignID, deviceID, at string, rollback bool) {
	d := campaignDeviceDoc(doc, campaignID, deviceID)
	d["status"] = DeviceDownloading
	d["phase"] = StageDownload
	d["at"] = at
	delete(d, "reason")
	dl := d["download"].(map[string]any)
	dl["claimed"] = true
	dl["claimedAt"] = at
	if rollback {
		d["rollbackTarget"] = "v1"
	}
	campaignDoc(doc, campaignID)["lastTime"] = at
}

// setupTwoBatchClaimed 构造两批（每批一台）活动：d1 在 12:03 安装成功，
// d2 在 12:04 合法领取下载（下载中）。返回关闭前记录的活动视图与存储目录。
func setupTwoBatchClaimed(t *testing.T) (*Store, CampaignView, string) {
	t.Helper()
	s := setupUpgrade(t, "d1", "d2")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	finishDevice(t, s, spec, "d1", upBase) // 安装成功时间 12:03
	dl, err := s.Claim(spec.ID, "d2", upBase.Add(4*time.Minute))
	if err != nil || dl == nil || dl.Kind != StageDownload {
		t.Fatalf("claim d2 download: %v %+v", err, dl)
	}
	before := mustGetCampaign(s, spec.ID)
	if d := findDevice(before, "d2"); d.Status != DeviceDownloading {
		t.Fatalf("precondition: d2 downloading, got %+v", d)
	}
	return s, before, s.dir
}

// TestRestoreDownloadClaimBeforeEarlierBatchRefused 针对“打开存储时遗漏批次放行
// 核对”的问题：任何已领取下载的设备，其首次领取时刻必须不早于所有较早批次设备
// 首次接受安装成功结果的时刻，且前序设备必须全部 succeeded。违反时 Open 返回
// ErrCorruptStorage 并保留原文件，不能补造前批结果、撤销领取或调整时间来消除矛盾。
func TestRestoreDownloadClaimBeforeEarlierBatchRefused(t *testing.T) {
	// d1 12:03 安装成功，d2 的下载领取时间被改早到 12:02：越过前批。
	t.Run("claim before earlier install success", func(t *testing.T) {
		s, _, dir := setupTwoBatchClaimed(t)
		s.Close()
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, "cmp-1", "d2")["download"].(map[string]any)["claimedAt"] =
				"2026-10-02T12:02:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 前批两台设备分别 12:03、12:07 安装成功，后批领取时间被改到 12:05：
	// 只晚于其中一台，仍越过另一台，必须拒绝。
	t.Run("claim between two earlier successes", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2", "d3")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2", "d3"}
		spec.BatchSize = 2
		createCampaign(t, s, spec)
		for _, id := range spec.Devices {
			bringOnline(t, s, id)
		}
		finishDevice(t, s, spec, "d1", upBase)                       // 12:03 安装成功
		finishDevice(t, s, spec, "d2", upBase.Add(4*time.Minute))    // 12:07 安装成功
		dl, err := s.Claim(spec.ID, "d3", upBase.Add(8*time.Minute)) // 12:08 合法领取
		if err != nil || dl == nil {
			t.Fatalf("claim d3 download: %v %+v", err, dl)
		}
		dir := s.dir
		s.Close()
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, "cmp-1", "d3")["download"].(map[string]any)["claimedAt"] =
				"2026-10-02T12:05:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 前批 d1 只下载成功（ready，尚未安装）：后批领取下载即越序。
	t.Run("earlier batch only downloaded", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, err := s.Claim(spec.ID, "d1", upBase)
		if err != nil || dl == nil {
			t.Fatalf("claim d1 download: %v %+v", err, dl)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(time.Minute), Success: true,
		}); err != nil {
			t.Fatalf("d1 download result: %v", err)
		}
		dir := s.dir
		s.Close()
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			claimDownloadInDoc(doc, "cmp-1", "d2", "2026-10-02T12:02:00Z", false)
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 前批 d1 已领取安装、正在安装：同样不满足放行条件。
	t.Run("earlier batch installing", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, err := s.Claim(spec.ID, "d1", upBase)
		if err != nil || dl == nil {
			t.Fatalf("claim d1 download: %v %+v", err, dl)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(time.Minute), Success: true,
		}); err != nil {
			t.Fatalf("d1 download result: %v", err)
		}
		in, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
		if err != nil || in == nil || in.Kind != StageInstall {
			t.Fatalf("claim d1 install: %v %+v", err, in)
		}
		dir := s.dir
		s.Close()
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			claimDownloadInDoc(doc, "cmp-1", "d2", "2026-10-02T12:03:00Z", false)
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 开启回滚的活动：前批 d1 安装失败、正等待回滚，后批 d2 已被级联记为
	// 未执行；把 d2 改写成已领取下载必须拒绝——等待回滚不是安装成功。
	t.Run("earlier batch awaiting rollback", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 1
		spec.RollbackOnFailure = true
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		awaitRollback(t, s, spec, "d1", upBase) // 12:03 安装失败，等待回滚
		if d := findDevice(mustGetCampaign(s, spec.ID), "d2"); d.Status != DeviceSkipped {
			t.Fatalf("precondition: d2 skipped, got %+v", d)
		}
		dir := s.dir
		s.Close()
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			claimDownloadInDoc(doc, "cmp-1", "d2", "2026-10-02T12:04:00Z", true)
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 前批 d1 已回滚成功（只是恢复原版本，不算本次升级成功），活动已失败结束；
	// 把后批 d2 改写成已领取下载（后随截止超时）仍必须拒绝——活动已结束、
	// 下载后来超时都不能掩盖首次领取时越过前批。
	t.Run("earlier batch rollback succeeded", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 1
		spec.RollbackOnFailure = true
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		awaitRollback(t, s, spec, "d1", upBase)
		rb, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute))
		if err != nil || rb == nil || rb.Kind != StageRollback {
			t.Fatalf("claim d1 rollback: %v %+v", err, rb)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(5 * time.Minute), Success: true,
			Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("d1 rollback result: %v", err)
		}
		cv := mustGetCampaign(s, spec.ID)
		if !cv.Ended || findDevice(cv, "d1").Status != DeviceRollbackSucceeded {
			t.Fatalf("precondition: d1 rollback succeeded and campaign ended: %+v", cv)
		}
		dir := s.dir
		s.Close()
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			d := campaignDeviceDoc(doc, "cmp-1", "d2")
			d["status"] = DeviceTimeout
			d["phase"] = StageDownload
			d["reason"] = "deadline exceeded"
			d["at"] = "2026-10-02T12:06:00Z"
			dl := d["download"].(map[string]any)
			dl["claimed"] = true
			dl["claimedAt"] = "2026-10-02T12:06:00Z"
			d["rollbackTarget"] = "v1"
			campaignDoc(doc, "cmp-1")["lastTime"] = "2026-10-02T12:06:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 活动已成功结束：d2 的下载后来成功、活动 succeeded，也不能掩盖
	// 首次领取时（被改早到 12:02）越过前批 12:03 的安装成功。
	t.Run("ended campaign later succeeded", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		finishDevice(t, s, spec, "d1", upBase)
		finishDevice(t, s, spec, "d2", upBase.Add(4*time.Minute))
		if cv := mustGetCampaign(s, spec.ID); !cv.Ended || cv.Status != CampaignSucceeded {
			t.Fatalf("precondition: campaign succeeded: %+v", cv)
		}
		dir := s.dir
		s.Close()
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, "cmp-1", "d2")["download"].(map[string]any)["claimedAt"] =
				"2026-10-02T12:02:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// d2 的下载后来失败、活动以失败结束，同样不能掩盖首次领取越序。
	t.Run("ended campaign download later failed", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		finishDevice(t, s, spec, "d1", upBase)
		dl, err := s.Claim(spec.ID, "d2", upBase.Add(4*time.Minute))
		if err != nil || dl == nil {
			t.Fatalf("claim d2 download: %v %+v", err, dl)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d2", OperationID: dl.ID,
			At: upBase.Add(5 * time.Minute), Success: false, Reason: "download broken",
		}); err != nil {
			t.Fatalf("d2 download result: %v", err)
		}
		if cv := mustGetCampaign(s, spec.ID); !cv.Ended || cv.Status != CampaignFailed {
			t.Fatalf("precondition: campaign failed: %+v", cv)
		}
		dir := s.dir
		s.Close()
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, "cmp-1", "d2")["download"].(map[string]any)["claimedAt"] =
				"2026-10-02T12:02:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreBatchReleaseAcceptsLegitimateClaims 保护合法的批次推进：
// 领取时刻等于（含不同时区写法的同一时刻）或晚于前批最晚安装成功时刻、
// 同批设备互不等待、以及尚未领取下载的后批设备（等待放行、因前批失败未执行）
// 都不被拒绝；打开后批次顺序、进度、操作标识与结果历史原样保留。
func TestRestoreBatchReleaseAcceptsLegitimateClaims(t *testing.T) {
	// 领取时刻恰好等于前批最晚安装成功时刻（12:03）：不晚于即合法。
	t.Run("claim equal to last install success", func(t *testing.T) {
		s, before, dir := setupTwoBatchClaimed(t)
		s.Close()
		rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, "cmp-1", "d2")["download"].(map[string]any)["claimedAt"] =
				"2026-10-02T12:03:00Z"
		})
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("claim equal to earlier install success must open: %v", err)
		}
		defer s2.Close()
		assertCampaignPreserved(t, s2, before)
	})

	// 同一时刻的不同时区写法（14:03+02:00 即 12:03Z）不得被误判为越序。
	t.Run("same instant in another timezone", func(t *testing.T) {
		s, before, dir := setupTwoBatchClaimed(t)
		s.Close()
		rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, "cmp-1", "d2")["download"].(map[string]any)["claimedAt"] =
				"2026-10-02T14:03:00+02:00"
		})
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("same instant in another timezone must open: %v", err)
		}
		defer s2.Close()
		assertCampaignPreserved(t, s2, before)
	})

	// 未改写的合法记录（领取晚于前批全部安装成功）重开后原样保留，
	// 已领取的下载仍作为同一待办返回。
	t.Run("claim after earlier batch succeeded", func(t *testing.T) {
		s, before, dir := setupTwoBatchClaimed(t)
		wantWork, err := s.GetDeviceWork("d2")
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("legitimate claim must open: %v", err)
		}
		defer s2.Close()
		assertCampaignPreserved(t, s2, before)
		w, err := s2.GetDeviceWork("d2")
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending == nil || w.Pending.Kind != StageDownload || !w.PendingClaimed ||
			w.Pending.ID != wantWork.Pending.ID {
			t.Fatalf("claimed download not preserved: %+v vs %+v", w, wantWork)
		}
	})

	// 同一批内的设备互不等待：d1 已领取下载、d2 尚未领取，合法。
	t.Run("same batch does not wait", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 2
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, err := s.Claim(spec.ID, "d1", upBase)
		if err != nil || dl == nil {
			t.Fatalf("claim d1 download: %v %+v", err, dl)
		}
		before := mustGetCampaign(s, spec.ID)
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("same-batch progress must open: %v", err)
		}
		defer s2.Close()
		assertCampaignPreserved(t, s2, before)
	})

	// 后批设备尚未领取下载（前批 d1 下载中，d2 等待放行）：没有领取时间，
	// 不能仅因前批没有成功而拒绝。
	t.Run("unclaimed later batch waits", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		if dl, err := s.Claim(spec.ID, "d1", upBase); err != nil || dl == nil {
			t.Fatalf("claim d1 download: %v %+v", err, dl)
		}
		before := mustGetCampaign(s, spec.ID)
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("unclaimed later batch must open: %v", err)
		}
		defer s2.Close()
		assertCampaignPreserved(t, s2, before)
	})

	// 前批失败后后批记为未执行（无领取、无结果）：合法记录，重开保留。
	t.Run("skipped later batch without claim", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, err := s.Claim(spec.ID, "d1", upBase)
		if err != nil || dl == nil {
			t.Fatalf("claim d1 download: %v %+v", err, dl)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(time.Minute), Success: false, Reason: "download broken",
		}); err != nil {
			t.Fatalf("d1 download result: %v", err)
		}
		before := mustGetCampaign(s, spec.ID)
		if d := findDevice(before, "d2"); d.Status != DeviceSkipped {
			t.Fatalf("precondition: d2 skipped, got %+v", d)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("skipped later batch must open: %v", err)
		}
		defer s2.Close()
		assertCampaignPreserved(t, s2, before)
	})
}

// assertCampaignPreserved 校验重开后活动的批次顺序、进度、操作标识与结果
// 历史与关闭前一致。
func assertCampaignPreserved(t *testing.T, s *Store, before CampaignView) {
	t.Helper()
	after := mustGetCampaign(s, before.ID)
	if after.Status != before.Status || after.Ended != before.Ended ||
		!after.EndedAt.Equal(before.EndedAt) || after.BatchSize != before.BatchSize {
		t.Fatalf("campaign summary changed: %+v vs %+v", after, before)
	}
	if len(after.Devices) != len(before.Devices) {
		t.Fatalf("device count changed: %+v vs %+v", after.Devices, before.Devices)
	}
	for i := range before.Devices {
		if after.Devices[i] != before.Devices[i] {
			t.Fatalf("device %d changed: %+v vs %+v", i, after.Devices[i], before.Devices[i])
		}
	}
	if len(after.Results) != len(before.Results) {
		t.Fatalf("result history changed: %+v vs %+v", after.Results, before.Results)
	}
	for i := range before.Results {
		if after.Results[i] != before.Results[i] {
			t.Fatalf("result %d changed: %+v vs %+v", i, after.Results[i], before.Results[i])
		}
	}
}
