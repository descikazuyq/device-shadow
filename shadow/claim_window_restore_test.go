package shadow

import (
	"encoding/json"
	"testing"
	"time"
)

// 本文件保护打开本地存储时对“首次领取时刻必须位于活动原维护窗口
// [start,end)（开始包含、结束不包含）”的核对：运行时 Claim 已强制窗口，
// 但保存的活动被读取时仍要防止窗口外的首次领取时间被当作有效进度。
// 核对对下载、安装、回滚都生效，活动仍在执行、已成功或已失败结束，
// 以及操作后来成功、失败或超时都使用同一项检查。

// setOpClaimedAt 改写磁盘上某设备某阶段操作的 claimed/claimedAt，
// 可选地把活动时间基线一并抬高（用于构造除窗口外其余记录自洽的数据）。
func setOpClaimedAt(t *testing.T, dir, campaignID, deviceID, stage, claimedAt string, lastTime string) []byte {
	t.Helper()
	return rewriteStoreBytes(t, dir, func(doc map[string]any) {
		d := campaignDeviceDoc(doc, campaignID, deviceID)
		if d == nil {
			t.Fatalf("device %s missing in campaign %s", deviceID, campaignID)
		}
		key := stage
		op := d[key].(map[string]any)
		if claimedAt == "" {
			op["claimed"] = false
			delete(op, "claimedAt")
		} else {
			op["claimed"] = true
			op["claimedAt"] = claimedAt
		}
		if lastTime != "" {
			campaignDoc(doc, campaignID)["lastTime"] = lastTime
		}
	})
}

// TestRestoreClaimOutsideWindowRefused：窗口为 12:00–13:00 时，首次领取
// 时间早于开始、恰好结束或晚于结束的记录都必须让 Open 返回
// ErrCorruptStorage，整个存储拒绝打开且原文件保持不变；活动结论与操作
// 后来的成败都不能掩盖窗口外的首次领取。
func TestRestoreClaimOutsideWindowRefused(t *testing.T) {
	const (
		beforeStart = "2026-10-02T11:59:00Z"
		atEnd       = "2026-10-02T13:00:00Z"
		afterEnd    = "2026-10-02T13:30:00Z"
		baseline    = "2026-10-02T13:45:00Z"
	)

	// 题设主例：记录表明下载在 11:59 首次领取，其他状态与结果历史都一致，
	// 也不允许继续使用这份活动。
	t.Run("download before window while running", func(t *testing.T) {
		s, dir, spec := setupClaimedDownload(t, upBase.Add(5*time.Minute))
		closeStore(t, s)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", StageDownload, beforeStart, "")
		assertReopenCorrupt(t, dir, original)
	})

	// 活动仍在执行，下载已领取未完成：恰好窗口结束 / 晚于结束都拒绝。
	// 把时间基线抬到 13:45，使除窗口外的记录（含时间基线）全部自洽，
	// 证明矛盾只能由窗口核对拦截。
	t.Run("download at window end while running", func(t *testing.T) {
		s, dir, spec := setupClaimedDownload(t, upBase.Add(5*time.Minute))
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(105*time.Minute)); err != nil {
			t.Fatal(err)
		}
		closeStore(t, s)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", StageDownload, atEnd, baseline)
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("download after window while running", func(t *testing.T) {
		s, dir, spec := setupClaimedDownload(t, upBase.Add(5*time.Minute))
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(105*time.Minute)); err != nil {
			t.Fatal(err)
		}
		closeStore(t, s)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", StageDownload, afterEnd, baseline)
		assertReopenCorrupt(t, dir, original)
	})

	// 活动已成功结束：下载首次领取在窗口前/结束时刻仍拒绝，不能用成功掩盖。
	t.Run("download before window but campaign succeeded", func(t *testing.T) {
		s, dir, spec := setupSucceededLateTimeline(t)
		closeStore(t, s)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", StageDownload, beforeStart, "")
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("download at window end but campaign succeeded", func(t *testing.T) {
		// 自然时间线：12:20 领下载、12:30 领安装、13:30 接受安装成功
		// （结果允许在窗口外、截止前接受），基线本来就晚于 13:00。
		s, dir, spec := setupSucceededLateTimeline(t)
		closeStore(t, s)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", StageDownload, atEnd, "")
		assertReopenCorrupt(t, dir, original)
	})

	// 活动已失败结束（下载失败）：窗口外的首次领取同样拒绝。
	t.Run("download before window then failed", func(t *testing.T) {
		s, dir, spec := setupFailedDownload(t, upBase.Add(5*time.Minute), upBase.Add(6*time.Minute))
		closeStore(t, s)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", StageDownload, beforeStart, "")
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("download at window end then failed", func(t *testing.T) {
		// 12:55 领取、13:30 接受下载失败（窗口外接受结果合法），基线为 13:30。
		s, dir, spec := setupFailedDownload(t, upBase.Add(55*time.Minute), upBase.Add(90*time.Minute))
		closeStore(t, s)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", StageDownload, atEnd, "")
		assertReopenCorrupt(t, dir, original)
	})

	// 已领取的下载后来随截止超时：超时终态不能掩盖窗口外首次领取。
	t.Run("download before window then timeout", func(t *testing.T) {
		s, dir, spec := setupClaimedDownload(t, upBase.Add(5*time.Minute))
		if err := s.AdvanceCampaign(spec.ID, spec.Deadline); err != nil {
			t.Fatal(err)
		}
		closeStore(t, s)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", StageDownload, beforeStart, "")
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("download after window then timeout", func(t *testing.T) {
		s, dir, spec := setupClaimedDownload(t, upBase.Add(5*time.Minute))
		if err := s.AdvanceCampaign(spec.ID, spec.Deadline); err != nil {
			t.Fatal(err)
		}
		closeStore(t, s)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", StageDownload, afterEnd, "")
		assertReopenCorrupt(t, dir, original)
	})

	// 安装首次领取在窗口外：拒绝。
	t.Run("install claimed before window", func(t *testing.T) {
		s, dir, spec := setupClaimedInstall(t)
		closeStore(t, s)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", StageInstall, beforeStart, "")
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("install claimed at window end", func(t *testing.T) {
		s, dir, spec := setupClaimedInstall(t)
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(105*time.Minute)); err != nil {
			t.Fatal(err)
		}
		closeStore(t, s)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", StageInstall, atEnd, baseline)
		assertReopenCorrupt(t, dir, original)
	})

	// 回滚首次领取在窗口外：拒绝（与下载、安装同一项检查）。
	t.Run("rollback claimed before window", func(t *testing.T) {
		s, dir, spec := setupClaimedRollback(t, upBase.Add(20*time.Minute))
		closeStore(t, s)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", StageRollback, beforeStart, "")
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("rollback claimed at window end", func(t *testing.T) {
		s, dir, spec := setupClaimedRollback(t, upBase.Add(20*time.Minute))
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(105*time.Minute)); err != nil {
			t.Fatal(err)
		}
		closeStore(t, s)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", StageRollback, atEnd, baseline)
		assertReopenCorrupt(t, dir, original)
	})

	// 按实际时刻判断：不同时区写法表示的同一时刻得到相同结果。
	t.Run("before window written in +08:00 timezone", func(t *testing.T) {
		// 2026-10-02T19:59:00+08:00 == 11:59Z，窗口前。
		s, dir, spec := setupClaimedDownload(t, upBase.Add(5*time.Minute))
		closeStore(t, s)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", StageDownload,
			"2026-10-02T19:59:00+08:00", "")
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("exactly window end written in +08:00 timezone", func(t *testing.T) {
		// 2026-10-02T21:00:00+08:00 == 13:00Z，恰为结束时刻（不包含）。
		// 抬高基线到 13:45，使时间基线等其他核对都自洽，只由窗口核对拦截。
		s, dir, spec := setupClaimedDownload(t, upBase.Add(5*time.Minute))
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(105*time.Minute)); err != nil {
			t.Fatal(err)
		}
		closeStore(t, s)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", StageDownload,
			"2026-10-02T21:00:00+08:00", baseline)
		assertReopenCorrupt(t, dir, original)
	})

	// 同一活动里另一台设备记录完全正常，也不能只恢复它：整个存储拒绝打开。
	t.Run("sibling device not partially available", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 2
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		dl1, err := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
		if err != nil || dl1 == nil {
			t.Fatalf("claim d1: %v", err)
		}
		if _, err := s.Claim(spec.ID, "d2", upBase.Add(6*time.Minute)); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		// d2 的领取保持窗口内合法；只把 d1 的下载领取挪到窗口前。
		original := setOpClaimedAt(t, dir, spec.ID, "d1", StageDownload, beforeStart, "")
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreClaimWindowLegitRecordsReopen 保护合法记录不被误拒，并重申窗口
// 核对只针对真正的首次领取时刻。
func TestRestoreClaimWindowLegitRecordsReopen(t *testing.T) {
	// 窗口开始时刻包含：恰在 12:00 首次领取下载，重开合法且进度原样。
	t.Run("claim exactly at window start", func(t *testing.T) {
		s, dir, spec := setupClaimedDownload(t, upBase)
		wantID := findDevice(mustGetCampaign(s, spec.ID), "d1").DownloadID
		closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("claim at window start must reopen: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceDownloading || d.DownloadID != wantID {
			t.Fatalf("claimed download not preserved: %+v", d)
		}
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending == nil || w.Pending.ID != wantID || !w.PendingClaimed {
			t.Fatalf("pending claimed work changed: %+v", w)
		}
	})

	// 不同时区写法但表示窗口开始的同一时刻：合法。
	t.Run("window start instant in +08:00 notation", func(t *testing.T) {
		s, dir, spec := setupClaimedDownload(t, upBase)
		closeStore(t, s)
		// 2026-10-02T20:00:00+08:00 == 12:00Z。
		setOpClaimedAt(t, dir, spec.ID, "d1", StageDownload, "2026-10-02T20:00:00+08:00", "")
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("same instant at window start must reopen: %v", err)
		}
		defer s2.Close()
		if d := findDevice(mustGetCampaign(s2, spec.ID), "d1"); d.Status != DeviceDownloading {
			t.Fatalf("legally claimed download lost: %+v", d)
		}
	})

	// 已在窗口内领取的操作，结果在窗口外、截止前接受仍合法：
	// 不能拿结果接受时间要求重新满足窗口。窗口 12:00–12:30，12:45 接受结果。
	t.Run("claimed in window result accepted after window", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		spec.WindowEnd = upBase.Add(30 * time.Minute)
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, err := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err != nil || dl == nil {
			t.Fatalf("claim download: %v", err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(45 * time.Minute), Success: true,
		}); err != nil {
			t.Fatalf("result after window but before deadline must be accepted: %v", err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("outside-window completion of in-window claim must reopen: %v", err)
		}
		defer s2.Close()
		if d := findDevice(mustGetCampaign(s2, spec.ID), "d1"); d.Status != DeviceReady {
			t.Fatalf("ready state lost: %+v", d)
		}
	})

	// 安装在窗口内领取、窗口外截止前成功：合法重开。
	t.Run("install claimed in window succeeds after window", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		spec.WindowEnd = upBase.Add(30 * time.Minute)
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(15 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		in, err := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute))
		if err != nil || in == nil {
			t.Fatalf("claim install: %v", err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
			At: upBase.Add(45 * time.Minute), Success: true,
			Seq: 2, Version: "v2", Config: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("install result after window: %v", err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("in-window install claim with late result must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if !cv.Ended || cv.Status != CampaignSucceeded {
			t.Fatalf("succeeded campaign lost: %+v", cv)
		}
	})

	// 回滚在窗口内领取、窗口外截止前成功：合法重开，锁定目标不变。
	t.Run("rollback claimed in window succeeds after window", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := rbSpec()
		spec.WindowEnd = upBase.Add(30 * time.Minute) // 窗口 12:00–12:30
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
		rb, err := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute)) // 窗口内领取回滚
		if err != nil || rb == nil || rb.TargetVersion != "v1" {
			t.Fatalf("claim rollback: %v %+v", err, rb)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(90 * time.Minute), Success: true, // 窗口外、截止前
			Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("rollback result after window: %v", err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("in-window rollback claim with late result must reopen: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceRollbackSucceeded || d.RollbackTarget != "v1" {
			t.Fatalf("rollback record changed: %+v", d)
		}
	})

	// 尚未领取的缺省领取时间不代表窗口外领取：等待安装、等待回滚，
	// 以及未领取便随截止结束的记录照常打开。
	t.Run("unclaimed stages have no claim time and reopen", func(t *testing.T) {
		// 等待安装（ready）。
		_, dir, _ := setupReady(t)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("ready with unclaimed install must reopen: %v", err)
		}
		s2.Close()

		// 等待回滚：回滚未领取。
		s, dir2, _, _ := setupInstallFailure(t, true)
		closeStore(t, s)
		s3, err := Open(dir2)
		if err != nil {
			t.Fatalf("awaiting rollback with unclaimed rollback must reopen: %v", err)
		}
		s3.Close()

		// 未领取下载就随截止超时。
		s = setupUpgrade(t, "d1")
		spec3 := upSpec()
		spec3.Devices = []string{"d1"}
		spec3.BatchSize = 1
		createCampaign(t, s, spec3)
		if err := s.AdvanceCampaign(spec3.ID, spec3.Deadline); err != nil {
			t.Fatal(err)
		}
		dir3 := closeStore(t, s)
		s4, err := Open(dir3)
		if err != nil {
			t.Fatalf("unclaimed deadline timeout must reopen: %v", err)
		}
		defer s4.Close()
		if d := findDevice(mustGetCampaign(s4, spec3.ID), "d1"); d.Status != DeviceTimeout {
			t.Fatalf("timeout state lost: %+v", d)
		}
	})

	// 未开启回滚的既有活动：缺省回滚操作不得被误判为已领取，
	// 窗口内领取的下载与安装失败终态照常打开。
	t.Run("rollback disabled default rollback op not claimed", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(6 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		in, _ := s.Claim(spec.ID, "d1", upBase.Add(7*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
			At: upBase.Add(8 * time.Minute), Success: false, Reason: "old fail",
		}); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("rollback-disabled failed campaign must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		d := findDevice(cv, "d1")
		if d.Status != DeviceFailed || cv.RollbackOnFailure || d.RollbackTarget != "" {
			t.Fatalf("non-rollback campaign restored wrong: %+v rollback=%t", d, cv.RollbackOnFailure)
		}
	})

	// 首次领取下载时锁定的回滚目标与设备影子不被窗口核对改变。
	t.Run("locked rollback target and shadow untouched", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := rbSpec()
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, err := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
		if err != nil || dl == nil {
			t.Fatalf("claim download: %v", err)
		}
		before, _ := s.Get("d1")
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.RollbackTarget != "v1" {
			t.Fatalf("locked rollback target changed: %q", d.RollbackTarget)
		}
		after, _ := s2.Get("d1")
		if after.Version != before.Version || after.LastSeq != before.LastSeq ||
			!rawEqual(after.Reported, before.Reported) {
			t.Fatalf("shadow changed by window validation: before %+v after %+v", before, after)
		}
	})
}

// setupClaimedDownload 构造一台在线设备在 at 首次领取下载、尚未完成的活动。
func setupClaimedDownload(t *testing.T, at time.Time) (*Store, string, CampaignSpec) {
	t.Helper()
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec.ID, "d1", at)
	if err != nil || dl == nil {
		t.Fatalf("claim download at %v: %v %+v", at, err, dl)
	}
	return s, s.dir, spec
}

// setupClaimedInstall 构造下载已成功、安装在 12:05 首次领取但尚未完成的活动。
func setupClaimedInstall(t *testing.T) (*Store, string, CampaignSpec) {
	t.Helper()
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(3 * time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	in, err := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
	if err != nil || in == nil {
		t.Fatalf("claim install: %v %+v", err, in)
	}
	return s, s.dir, spec
}

// setupClaimedRollback 构造安装失败已接受、回滚在 at 首次领取但尚未完成的
// 开启回滚活动（窗口 12:00–13:00）。
func setupClaimedRollback(t *testing.T, at time.Time) (*Store, string, CampaignSpec) {
	t.Helper()
	s, dir, spec, _ := setupInstallFailure(t, true)
	rb, err := s.Claim(spec.ID, "d1", at)
	if err != nil || rb == nil || rb.Kind != StageRollback {
		t.Fatalf("claim rollback at %v: %v %+v", at, err, rb)
	}
	return s, dir, spec
}

// setupSucceededLateTimeline 构造一条自然合法、但全部领取都在窗口内而最后
// 结果在窗口外（13:30，截止 14:00 前）的成功时间线：
// 12:20 领下载、12:25 下载成功、12:30 领安装、13:30 安装成功。
func setupSucceededLateTimeline(t *testing.T) (*Store, string, CampaignSpec) {
	t.Helper()
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute))
	if err != nil || dl == nil {
		t.Fatalf("claim download: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(25 * time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	in, err := s.Claim(spec.ID, "d1", upBase.Add(30*time.Minute))
	if err != nil || in == nil {
		t.Fatalf("claim install: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: upBase.Add(90 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("install result: %v", err)
	}
	cv := mustGetCampaign(s, spec.ID)
	if !cv.Ended || cv.Status != CampaignSucceeded {
		t.Fatalf("precondition: succeeded campaign: %+v", cv)
	}
	return s, s.dir, spec
}

// setupFailedDownload 构造下载在 claimAt 首次领取、在 failAt 接受失败结果
// 而失败结束的活动。
func setupFailedDownload(t *testing.T, claimAt, failAt time.Time) (*Store, string, CampaignSpec) {
	t.Helper()
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec.ID, "d1", claimAt)
	if err != nil || dl == nil {
		t.Fatalf("claim download: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: failAt, Success: false, Reason: "download broken",
	}); err != nil {
		t.Fatalf("download failure: %v", err)
	}
	return s, s.dir, spec
}
