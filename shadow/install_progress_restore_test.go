package shadow

import (
	"encoding/json"
	"testing"
	"time"
)

// setupInstalling 构造下载成功、安装已领取但尚无安装结果的设备，
// 返回安装操作标识与关闭后的存储目录。
func setupInstalling(t *testing.T) (dir string, spec CampaignSpec, installID string) {
	t.Helper()
	_, dir, spec = setupReady(t)
	// setupReady 已关闭存储，重新打开后领取安装。
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen ready: %v", err)
	}
	in, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("claim install: %v", err)
	}
	if in == nil || in.Kind != StageInstall {
		t.Fatalf("want install op, got %+v", in)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, spec, in.ID
}

// TestInstallProgressRestoreCorrupt 针对等待安装（ready）与正在安装
// （installing）两种状态的恢复核对：保存的状态与本设备在该活动中已接受的
// 操作结果不符时，必须拒绝打开整个存储并原样保留文件。
func TestInstallProgressRestoreCorrupt(t *testing.T) {
	t.Run("ready but install already claimed", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			op := deviceOpDoc(doc, spec.ID, "d1", StageInstall)
			op["claimed"] = true
			op["claimedAt"] = "2026-10-02T12:01:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("ready but wrong phase", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, spec.ID, "d1")["phase"] = "download"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("installing but install never claimed", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, spec.ID, "d1")["status"] = "installing"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("installing but wrong phase", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			dev := campaignDeviceDoc(doc, spec.ID, "d1")
			dev["status"] = "installing"
			dev["phase"] = "download"
			op := deviceOpDoc(doc, spec.ID, "d1", StageInstall)
			op["claimed"] = true
			op["claimedAt"] = "2026-10-02T12:01:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("install success recorded as ready", func(t *testing.T) {
		// 操作标识、时间与结果历史各自合法，也不能把已完成安装的设备
		// 解释成仍在等待安装。
		_, dir, spec := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, spec.ID, "d1")["status"] = "ready"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("install success recorded as installing", func(t *testing.T) {
		_, dir, spec := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, spec.ID, "d1")["status"] = "installing"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("install failure recorded as ready", func(t *testing.T) {
		_, dir, spec, _ := setupInstallFailure(t, false)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, spec.ID, "d1")["status"] = "ready"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("install failure recorded as installing", func(t *testing.T) {
		_, dir, spec, _ := setupInstallFailure(t, false)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, spec.ID, "d1")["status"] = "installing"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("ready without download success", func(t *testing.T) {
		_, dir, spec := setupDownloading(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			dev := campaignDeviceDoc(doc, spec.ID, "d1")
			dev["status"] = "ready"
			dev["phase"] = "install"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("installing without download success", func(t *testing.T) {
		_, dir, spec := setupDownloading(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			dev := campaignDeviceDoc(doc, spec.ID, "d1")
			dev["status"] = "installing"
			dev["phase"] = "install"
			op := deviceOpDoc(doc, spec.ID, "d1", StageInstall)
			op["claimed"] = true
			op["claimedAt"] = "2026-10-02T12:00:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("ready cannot borrow other device download", func(t *testing.T) {
		// 同批 d1 已下载成功，d2 仅领取下载：d2 不能借用 d1 的下载成功。
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.BatchSize = 2
		spec.Devices = []string{"d1", "d2"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		dl, err := s.Claim(spec.ID, "d1", upBase)
		if err != nil {
			t.Fatalf("claim d1: %v", err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(time.Minute), Success: true,
		}); err != nil {
			t.Fatalf("download d1: %v", err)
		}
		if _, err := s.Claim(spec.ID, "d2", upBase.Add(2*time.Minute)); err != nil {
			t.Fatalf("claim d2: %v", err)
		}
		dir := s.dir
		s.Close()
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			dev := campaignDeviceDoc(doc, spec.ID, "d2")
			dev["status"] = "ready"
			dev["phase"] = "install"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("current version at target does not substitute download", func(t *testing.T) {
		// 普通上报已把当前版本改成目标版本，也不能替代下载成功结果。
		_, dir, spec := setupDownloading(t)
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Report("d1", 2, upBase.Add(11*time.Minute), "v2", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("report: %v", err)
		}
		s.Close()
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			dev := campaignDeviceDoc(doc, spec.ID, "d1")
			dev["status"] = "ready"
			dev["phase"] = "install"
		})
		assertReopenCorrupt(t, dir, original)
	})
}

// TestInstallProgressLegitStatesReopen 校验正常的未完成安装进度重开后保留：
// 等待安装保留安装待办与未领取标记，正在安装保留原操作标识与已领取标记，
// 后续仍能按现有规则领取与提交结果。
func TestInstallProgressLegitStatesReopen(t *testing.T) {
	t.Run("ready keeps unclaimed install todo", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("ready state must open: %v", err)
		}
		defer s.Close()
		cv := mustGetCampaign(s, spec.ID)
		d := findDevice(cv, "d1")
		if d.Status != DeviceReady || d.Phase != StageInstall {
			t.Fatalf("restored view: status=%s phase=%s", d.Status, d.Phase)
		}
		w, err := s.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending == nil || w.Pending.Kind != StageInstall || w.PendingClaimed {
			t.Fatalf("pending install todo lost: %+v claimed=%v", w.Pending, w.PendingClaimed)
		}
		if w.Pending.ID != d.InstallID {
			t.Fatalf("install op id changed: %s vs %s", w.Pending.ID, d.InstallID)
		}
		// 重开后仍能按现有规则领取安装并提交结果。
		in, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("claim install after reopen: %v", err)
		}
		if in.ID != d.InstallID {
			t.Fatalf("claimed op id changed: %s vs %s", in.ID, d.InstallID)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
			At: upBase.Add(3 * time.Minute), Success: true,
			Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true}`),
		}); err != nil {
			t.Fatalf("install result after reopen: %v", err)
		}
		if got := findDevice(mustGetCampaign(s, spec.ID), "d1").Status; got != DeviceSucceeded {
			t.Fatalf("status after install: %s", got)
		}
	})

	t.Run("installing keeps claimed operation", func(t *testing.T) {
		dir, spec, installID := setupInstalling(t)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("installing state must open: %v", err)
		}
		defer s.Close()
		cv := mustGetCampaign(s, spec.ID)
		d := findDevice(cv, "d1")
		if d.Status != DeviceInstalling || d.Phase != StageInstall {
			t.Fatalf("restored view: status=%s phase=%s", d.Status, d.Phase)
		}
		w, err := s.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending == nil || w.Pending.Kind != StageInstall || !w.PendingClaimed {
			t.Fatalf("claimed install todo lost: %+v claimed=%v", w.Pending, w.PendingClaimed)
		}
		if w.Pending.ID != installID {
			t.Fatalf("install op id changed: %s vs %s", w.Pending.ID, installID)
		}
		// 已领取的安装后续仍能按现有规则提交结果。
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: installID,
			At: upBase.Add(3 * time.Minute), Success: true,
			Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true}`),
		}); err != nil {
			t.Fatalf("install result after reopen: %v", err)
		}
		cv2 := mustGetCampaign(s, spec.ID)
		if got := findDevice(cv2, "d1").Status; got != DeviceSucceeded || cv2.Status != CampaignSucceeded {
			t.Fatalf("after install: device=%s campaign=%s", got, cv2.Status)
		}
	})

	t.Run("offline device keeps ready progress", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.SetOffline("d1"); err != nil {
			t.Fatal(err)
		}
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("offline ready state must open: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceReady {
			t.Fatalf("offline changed progress: %s", d.Status)
		}
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Online || w.Pending == nil || w.Pending.Kind != StageInstall || w.PendingClaimed {
			t.Fatalf("offline ready work: %+v", w)
		}
	})

	t.Run("window ended keeps ready progress", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		// 维护窗口结束但截止未到：设备停留在等待安装，不超时也不补结果。
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(90*time.Minute)); err != nil {
			t.Fatal(err)
		}
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("ready past window must open: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		d := findDevice(cv, "d1")
		if d.Status != DeviceReady || len(cv.Results) != 1 {
			t.Fatalf("window end changed progress: status=%s results=%+v", d.Status, cv.Results)
		}
	})

	t.Run("version drift to target keeps ready progress", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		// 普通上报把当前版本改成目标版本：既不损坏进度，也不补造安装结果。
		if err := s.Report("d1", 2, upBase.Add(5*time.Minute), "v2", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("version drift must not corrupt ready progress: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		d := findDevice(cv, "d1")
		if d.Status != DeviceReady || len(cv.Results) != 1 {
			t.Fatalf("report auto-completed install: status=%s results=%+v", d.Status, cv.Results)
		}
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Version != "v2" || w.Pending == nil || w.Pending.Kind != StageInstall || w.PendingClaimed {
			t.Fatalf("ready work after drift: %+v", w)
		}
	})

	t.Run("version drift keeps installing progress", func(t *testing.T) {
		dir, spec, installID := setupInstalling(t)
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Report("d1", 2, upBase.Add(5*time.Minute), "v3", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("version drift must not corrupt installing progress: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceInstalling {
			t.Fatalf("report changed progress: %s", d.Status)
		}
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending == nil || w.Pending.ID != installID || !w.PendingClaimed {
			t.Fatalf("installing work after drift: %+v", w)
		}
	})
}
