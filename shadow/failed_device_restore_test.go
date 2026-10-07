package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// setupDownloadFailure 构造一台设备已在下载阶段失败的已关闭存储：
// 失败原因 "dl down"，失败时间 12:03，活动随即失败结束。
func setupDownloadFailure(t *testing.T, rollback bool, devices ...string) (*Store, string, CampaignSpec) {
	t.Helper()
	all := append([]string{"d1"}, devices...)
	s := setupUpgrade(t, all...)
	spec := upSpec()
	spec.Devices = all
	spec.BatchSize = len(all)
	spec.RollbackOnFailure = rollback
	createCampaign(t, s, spec)
	for _, id := range all {
		bringOnline(t, s, id)
	}
	dl, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(3 * time.Minute), Success: false, Reason: "dl down",
	}); err != nil {
		t.Fatalf("download failure: %v", err)
	}
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return s, dir, spec
}

// TestFailedDeviceMustMatchAcceptedResult 设备状态标为 failed 时，其阶段、原因
// 与时间必须由本设备在本活动中已接受的同阶段失败结果解释；任一处矛盾都必须
// 返回 ErrCorruptStorage 拒绝打开整个存储，且文件内容原样保留。
func TestFailedDeviceMustMatchAcceptedResult(t *testing.T) {
	t.Run("download failure: phase rewritten to install", func(t *testing.T) {
		_, dir, _ := setupDownloadFailure(t, false)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, "cmp-1", "d1")["phase"] = "install"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("download failure: phase rewritten to rollback", func(t *testing.T) {
		// 即便活动开启了回滚，下载失败也不能写成回滚阶段。
		_, dir, _ := setupDownloadFailure(t, true)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			d := campaignDeviceDoc(doc, "cmp-1", "d1")
			d["phase"] = "rollback"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("download failure: reason rewritten", func(t *testing.T) {
		_, dir, _ := setupDownloadFailure(t, false)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, "cmp-1", "d1")["reason"] = "download interrupted"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("download failure: reason emptied", func(t *testing.T) {
		_, dir, _ := setupDownloadFailure(t, false)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			delete(campaignDeviceDoc(doc, "cmp-1", "d1"), "reason")
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("download failure: time moved later", func(t *testing.T) {
		_, dir, _ := setupDownloadFailure(t, false)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, "cmp-1", "d1")["at"] = "2026-10-02T12:12:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("download failure: accepted result deleted", func(t *testing.T) {
		_, dir, _ := setupDownloadFailure(t, false)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			dl := campaignDeviceDoc(doc, "cmp-1", "d1")["download"].(map[string]any)
			dl["hasResult"] = false
			dl["success"] = false
			dl["reason"] = ""
			delete(dl, "at")
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("other device's failure cannot explain this device", func(t *testing.T) {
		// d1 下载失败后，同批的 d2 被记为 skipped。把 d2 改成 failed 并写上
		// 与 d1 相同的原因/时间，但 d2 自己没有已接受的下载失败结果：损坏。
		_, dir, _ := setupDownloadFailure(t, false, "d2")
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			d2 := campaignDeviceDoc(doc, "cmp-1", "d2")
			d2["status"] = DeviceFailed
			d2["phase"] = StageDownload
			d2["reason"] = "dl down"
			d2["at"] = "2026-10-02T12:03:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("install failure without rollback: phase rewritten to download", func(t *testing.T) {
		s, dir, spec, _ := setupInstallFailure(t, false)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, spec.ID, "d1")["phase"] = "download"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("install failure without rollback: reason rewritten", func(t *testing.T) {
		s, dir, spec, _ := setupInstallFailure(t, false)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, spec.ID, "d1")["reason"] = "install broken differently"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("install failure without rollback: time moved", func(t *testing.T) {
		s, dir, spec, _ := setupInstallFailure(t, false)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, spec.ID, "d1")["at"] = "2026-10-02T12:30:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})
}

// TestFailedDeviceTimezoneEqualInstant 设备状态时间与首次接受失败结果的时间
// 表示同一实际时刻、仅时区写法不同（Z vs +08:00）时必须正常打开。
func TestFailedDeviceTimezoneEqualInstant(t *testing.T) {
	t.Run("download failure same instant different zone", func(t *testing.T) {
		_, dir, _ := setupDownloadFailure(t, false)
		// 失败结果接受于 12:03:00Z，设备状态改写成 +08:00 表示的同一时刻。
		rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, "cmp-1", "d1")["at"] = "2026-10-02T20:03:00+08:00"
		})
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("same instant in different zone must open: %v", err)
		}
		defer s.Close()
		d := findDevice(mustGetCampaign(s, "cmp-1"), "d1")
		if d.Status != DeviceFailed || d.Phase != StageDownload || d.Reason != "dl down" ||
			!d.At.Equal(upBase.Add(3*time.Minute)) {
			t.Fatalf("restored failed device: %+v", d)
		}
	})

	t.Run("install failure same instant different zone", func(t *testing.T) {
		s, dir, spec, _ := setupInstallFailure(t, false)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, spec.ID, "d1")["at"] = "2026-10-02T20:03:00+08:00"
		})
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("same instant in different zone must open: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceFailed || d.Phase != StageInstall || d.Reason != "install boom" ||
			!d.At.Equal(upBase.Add(3*time.Minute)) {
			t.Fatalf("restored failed device: %+v", d)
		}
	})
}

// TestFailedDeviceLegitRecordsReopen 合法失败记录继续按原有行为读取和查询。
func TestFailedDeviceLegitRecordsReopen(t *testing.T) {
	t.Run("plain download failure kept", func(t *testing.T) {
		_, dir, spec := setupDownloadFailure(t, false)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("download failure must reopen: %v", err)
		}
		defer s.Close()
		cv := mustGetCampaign(s, spec.ID)
		d := findDevice(cv, "d1")
		failAt := upBase.Add(3 * time.Minute)
		if d.Status != DeviceFailed || d.Phase != StageDownload || d.Reason != "dl down" ||
			!d.At.Equal(failAt) {
			t.Fatalf("restored download failure: %+v", d)
		}
		if len(cv.Results) != 1 {
			t.Fatalf("history: %+v", cv.Results)
		}
		r := cv.Results[0]
		if r.Success || r.Stage != StageDownload || r.Reason != "dl down" ||
			!r.At.Equal(failAt) || r.OperationID != d.DownloadID {
			t.Fatalf("history record: %+v", r)
		}
		if !cv.Ended || cv.Status != CampaignFailed || !cv.EndedAt.Equal(failAt) {
			t.Fatalf("campaign conclusion: ended=%v status=%s endedAt=%v", cv.Ended, cv.Status, cv.EndedAt)
		}
	})

	t.Run("claim-time incompatible download failure kept", func(t *testing.T) {
		// 首次领取下载时发现版本不兼容而直接生成的下载失败同样有效，
		// 不要求它来自设备主动提交；设备影子当前版本不能否定它。
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		if err := s.Report("d1", 2, upBase.Add(time.Minute), "v3", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		failAt := upBase.Add(2 * time.Minute)
		if _, err := s.Claim(spec.ID, "d1", failAt); !errors.Is(err, ErrIncompatibleVersion) {
			t.Fatalf("want incompatible version, got %v", err)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("claim-time failure must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		d := findDevice(cv, "d1")
		wantReason := "version v3 is not compatible with target v2"
		if d.Status != DeviceFailed || d.Phase != StageDownload ||
			d.Reason != wantReason || !d.At.Equal(failAt) {
			t.Fatalf("restored claim-time failure: %+v", d)
		}
		if len(cv.Results) != 1 || cv.Results[0].Reason != wantReason ||
			!cv.Results[0].At.Equal(failAt) {
			t.Fatalf("history: %+v", cv.Results)
		}
	})

	t.Run("download failure in rollback campaign kept without rollback state", func(t *testing.T) {
		// 开启回滚不改变下载失败的形态：仍是 failed/download，无回滚结果。
		_, dir, spec := setupDownloadFailure(t, true)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("download failure under rollback campaign must reopen: %v", err)
		}
		defer s.Close()
		d := findDevice(mustGetCampaign(s, spec.ID), "d1")
		if d.Status != DeviceFailed || d.Phase != StageDownload || d.RollbackResult {
			t.Fatalf("download failure must not enter rollback: %+v", d)
		}
	})

	t.Run("one device failed while campaign still running", func(t *testing.T) {
		// 同批两台设备：d1 已失败，d2 仍在下载中，活动可以继续运行。
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 2
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		dl1, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
		dl2, _ := s.Claim(spec.ID, "d2", upBase.Add(2*time.Minute))
		failAt := upBase.Add(3 * time.Minute)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl1.ID,
			At: failAt, Success: false, Reason: "dl down",
		}); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("running campaign with a failed device must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if cv.Ended || cv.Status != CampaignRunning {
			t.Fatalf("campaign must still run: %+v", cv)
		}
		d1 := findDevice(cv, "d1")
		if d1.Status != DeviceFailed || d1.Reason != "dl down" || !d1.At.Equal(failAt) {
			t.Fatalf("failed device altered: %+v", d1)
		}
		d2 := findDevice(cv, "d2")
		if d2.Status != DeviceDownloading || d2.DownloadID != dl2.ID {
			t.Fatalf("other device must keep downloading: %+v", d2)
		}
	})

	t.Run("campaign ends later than the device failure", func(t *testing.T) {
		// d1 先失败、d2 后失败，活动在更晚时间结束；重开不得改动 d1 的失败信息。
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 2
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		dl1, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
		dl2, _ := s.Claim(spec.ID, "d2", upBase.Add(2*time.Minute))
		fail1 := upBase.Add(3 * time.Minute)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl1.ID,
			At: fail1, Success: false, Reason: "dl down",
		}); err != nil {
			t.Fatal(err)
		}
		fail2 := upBase.Add(20 * time.Minute)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d2", OperationID: dl2.ID,
			At: fail2, Success: false, Reason: "dl down too",
		}); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("ended campaign must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if !cv.Ended || cv.Status != CampaignFailed || !cv.EndedAt.Equal(fail2) {
			t.Fatalf("campaign conclusion: %+v", cv)
		}
		d1 := findDevice(cv, "d1")
		if d1.Reason != "dl down" || !d1.At.Equal(fail1) {
			t.Fatalf("earlier failure must keep original reason/time: %+v", d1)
		}
	})

	t.Run("install failure without rollback kept", func(t *testing.T) {
		s, dir, spec, _ := setupInstallFailure(t, false)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("install failure without rollback must reopen: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		failAt := upBase.Add(3 * time.Minute)
		if d.Status != DeviceFailed || d.Phase != StageInstall ||
			d.Reason != "install boom" || !d.At.Equal(failAt) {
			t.Fatalf("restored install failure: %+v", d)
		}
	})
}
