package shadow

import (
	"encoding/json"
	"testing"
	"time"
)

// setCampaignConclusion 直接改写存储文件中活动的整体状态与结束标记，
// 用来构造“活动结论与设备进度矛盾”的损坏数据。
func setCampaignConclusion(t *testing.T, dir, id, status string, ended bool, endedAt string) []byte {
	t.Helper()
	return rewriteStoreBytes(t, dir, func(doc map[string]any) {
		c := campaignDoc(doc, id)
		c["status"] = status
		c["ended"] = ended
		if endedAt != "" {
			c["endedAt"] = endedAt
		} else {
			delete(c, "endedAt")
		}
	})
}

// TestRestoreCampaignConclusionMustMatchDevices 校验重开存储时活动整体结论
// 必须与设备进度一致：仍有设备处于非终态的活动不得写成已结束；全部设备终态后
// 活动必须已结束，且只有全部 succeeded 才能 succeeded，其余组合（含回滚成功）
// 只能 failed。矛盾时 Open 返回 ErrCorruptStorage，原文件保持不变。
func TestRestoreCampaignConclusionMustMatchDevices(t *testing.T) {
	endedAt := "2026-10-02T13:00:00Z"

	// 各类“设备尚未终态却把活动写成已结束”的矛盾：
	// pending、ready（等待安装）、downloading、installing、
	// awaiting_rollback、rolling_back 都必须让整个存储拒绝打开。
	t.Run("ended while devices unfinished", func(t *testing.T) {
		cases := map[string]func(t *testing.T) (dir, campaignID string){
			"pending": func(t *testing.T) (string, string) {
				s := setupUpgrade(t, "d1")
				spec := upSpec()
				spec.Devices = []string{"d1"}
				spec.BatchSize = 1
				createCampaign(t, s, spec)
				dir := s.dir
				s.Close()
				return dir, spec.ID
			},
			"ready": func(t *testing.T) (string, string) {
				_, dir, spec := setupReady(t)
				return dir, spec.ID
			},
			"downloading": func(t *testing.T) (string, string) {
				s := setupUpgrade(t, "d1")
				spec := upSpec()
				spec.Devices = []string{"d1"}
				spec.BatchSize = 1
				createCampaign(t, s, spec)
				bringOnline(t, s, "d1")
				if _, err := s.Claim(spec.ID, "d1", upBase); err != nil {
					t.Fatal(err)
				}
				dir := s.dir
				s.Close()
				return dir, spec.ID
			},
			"installing": func(t *testing.T) (string, string) {
				s := setupUpgrade(t, "d1")
				spec := upSpec()
				spec.Devices = []string{"d1"}
				spec.BatchSize = 1
				createCampaign(t, s, spec)
				bringOnline(t, s, "d1")
				dl, _ := s.Claim(spec.ID, "d1", upBase)
				if err := s.SubmitResult(OperationResult{
					CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
					At: upBase.Add(time.Minute), Success: true,
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute)); err != nil {
					t.Fatal(err)
				}
				dir := s.dir
				s.Close()
				return dir, spec.ID
			},
			"awaiting rollback": func(t *testing.T) (string, string) {
				s, dir, spec, _ := setupInstallFailure(t, true)
				s.Close()
				return dir, spec.ID
			},
			"rolling back": func(t *testing.T) (string, string) {
				s, dir, spec, _ := setupInstallFailure(t, true)
				if _, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil {
					t.Fatal(err)
				}
				s.Close()
				return dir, spec.ID
			},
		}
		for name, build := range cases {
			t.Run(name, func(t *testing.T) {
				dir, id := build(t)
				original := setCampaignConclusion(t, dir, id, CampaignFailed, true, endedAt)
				assertReopenCorrupt(t, dir, original)
			})
		}
	})

	// 全部设备安装成功，活动结论却保存为 failed：不能展示错误的升级结果。
	t.Run("all succeeded saved as failed", func(t *testing.T) {
		_, dir, spec := setupSucceeded(t)
		original := setCampaignConclusion(t, dir, spec.ID, CampaignFailed, true, endedAt)
		assertReopenCorrupt(t, dir, original)
	})

	// 存在失败设备，活动却保存为 succeeded。
	t.Run("failed device but campaign succeeded", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(time.Minute), Success: false, Reason: "boom",
		}); err != nil {
			t.Fatal(err)
		}
		cv := mustGetCampaign(s, spec.ID)
		if !cv.Ended || cv.Status != CampaignFailed ||
			findDevice(cv, "d1").Status != DeviceFailed ||
			findDevice(cv, "d2").Status != DeviceSkipped {
			t.Fatalf("precondition: %+v", cv)
		}
		dir := s.dir
		s.Close()
		original := setCampaignConclusion(t, dir, spec.ID, CampaignSucceeded, true, endedAt)
		assertReopenCorrupt(t, dir, original)
	})

	// 回滚成功只表示恢复到了原版本，不算本次升级成功：活动必须 failed，
	// 保存成 succeeded 即损坏。
	t.Run("rollback succeeded but campaign succeeded", func(t *testing.T) {
		s, dir, spec, rb := setupInstallFailure(t, true)
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(5 * time.Minute), Success: true,
			Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
		cv := mustGetCampaign(s, spec.ID)
		if !cv.Ended || cv.Status != CampaignFailed ||
			findDevice(cv, "d1").Status != DeviceRollbackSucceeded {
			t.Fatalf("precondition: %+v", cv)
		}
		s.Close()
		original := setCampaignConclusion(t, dir, spec.ID, CampaignSucceeded, true, endedAt)
		assertReopenCorrupt(t, dir, original)
	})

	// 全部设备终态（succeeded）后活动却仍标记 running/未结束。
	t.Run("all terminal but still running", func(t *testing.T) {
		_, dir, spec := setupSucceeded(t)
		original := setCampaignConclusion(t, dir, spec.ID, CampaignRunning, false, "")
		assertReopenCorrupt(t, dir, original)
	})

	// 全部设备超时终态、活动却未结束。
	t.Run("all timeout but still running", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		if err := s.AdvanceCampaign(spec.ID, spec.Deadline); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		original := setCampaignConclusion(t, dir, spec.ID, CampaignRunning, false, "")
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreLegitUnfinishedCampaignsReopen 校验尚未完成的正常活动继续保留：
// 设备原有待办、批次顺序和操作标识重开后仍可查。
func TestRestoreLegitUnfinishedCampaignsReopen(t *testing.T) {
	// 同批一台设备已经失败（终态），另一台仍未完成时，整体仍在运行：
	// 正常打开，未失败设备的待办继续可查。
	t.Run("one failed one pending in same batch", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 2
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(time.Minute), Success: false, Reason: "boom",
		}); err != nil {
			t.Fatal(err)
		}
		wantWork, err := s.GetDeviceWork("d2")
		if err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()

		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("running campaign with a failed device must open: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if cv.Ended || cv.Status != CampaignRunning {
			t.Fatalf("campaign must stay running: %+v", cv)
		}
		if findDevice(cv, "d1").Status != DeviceFailed ||
			findDevice(cv, "d2").Status != DevicePending {
			t.Fatalf("device progress lost: %+v", cv.Devices)
		}
		w, err := s2.GetDeviceWork("d2")
		if err != nil {
			t.Fatal(err)
		}
		if w.CampaignID != spec.ID || w.Pending == nil ||
			w.Pending.Kind != StageDownload || w.PendingClaimed ||
			w.Pending.ID != wantWork.Pending.ID {
			t.Fatalf("d2 pending work lost after restore: %+v want %+v", w, wantWork)
		}
	})

	// 开启回滚：前批安装失败后，后续批次已记为 skipped，但失败设备还在
	// 等待回滚时，活动不能提前结束；重开后回滚待办（带锁定目标版本）仍可查。
	t.Run("awaiting rollback while later batch skipped", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 1
		spec.RollbackOnFailure = true
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		in, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
			At: upBase.Add(3 * time.Minute), Success: false, Reason: "install boom",
		}); err != nil {
			t.Fatal(err)
		}
		wantWork, err := s.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		cv0 := mustGetCampaign(s, spec.ID)
		if cv0.Ended || findDevice(cv0, "d1").Status != DeviceAwaitingRollback ||
			findDevice(cv0, "d2").Status != DeviceSkipped {
			t.Fatalf("precondition: %+v", cv0)
		}
		dir := s.dir
		s.Close()

		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("awaiting-rollback campaign must open: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if cv.Ended || cv.Status != CampaignRunning {
			t.Fatalf("campaign must stay running during rollback: %+v", cv)
		}
		if findDevice(cv, "d1").Status != DeviceAwaitingRollback ||
			findDevice(cv, "d2").Status != DeviceSkipped {
			t.Fatalf("device progress lost: %+v", cv.Devices)
		}
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.CampaignID != spec.ID || w.Pending == nil ||
			w.Pending.Kind != StageRollback || w.PendingClaimed ||
			w.Pending.TargetVersion != "v1" ||
			w.Pending.ID != wantWork.Pending.ID {
			t.Fatalf("rollback pending work lost: %+v want %+v", w, wantWork)
		}
	})

	// 回滚各终态（成功/失败/超时）都是 failed 结束活动的合法组合，
	// 未开启回滚的旧活动按原有终态判断同样正常打开。
	t.Run("terminal rollback campaigns reopen failed", func(t *testing.T) {
		// 回滚成功
		s, dir, spec, rb := setupInstallFailure(t, true)
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(5 * time.Minute), Success: true,
			Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("rollback-succeeded campaign must reopen failed: %v", err)
		}
		cv := mustGetCampaign(s2, spec.ID)
		if !cv.Ended || cv.Status != CampaignFailed {
			t.Fatalf("rollback success must end campaign failed: %+v", cv)
		}
		s2.Close()
	})
}
