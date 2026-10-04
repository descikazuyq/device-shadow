package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// setCampaignOutcome 直接改写存储文件中活动的整体状态与结束标记，
// 用来构造“设备进度由正常流程产生、但活动结论被写错”的矛盾数据。
func setCampaignOutcome(t *testing.T, dir, campaignID string, mutate func(camp map[string]any)) []byte {
	t.Helper()
	return rewriteStoreBytes(t, dir, func(doc map[string]any) {
		mutate(campaignDoc(doc, campaignID))
	})
}

// markEnded 把活动整体结论改写为已结束的 given 状态。
func markEnded(t *testing.T, dir, campaignID, status string) []byte {
	t.Helper()
	return setCampaignOutcome(t, dir, campaignID, func(camp map[string]any) {
		camp["status"] = status
		camp["ended"] = true
	})
}

// singleDeviceSpec 构造一个 upBase 时间线、目标 v2 的活动规格。
func singleDeviceSpec(id string, devices []string, batchSize int) CampaignSpec {
	return activeCampaignSpec(id, upBase, devices, batchSize)
}

// claimDownloadUnfinished 领取下载但不提交结果：设备停留在 downloading。
func claimDownloadUnfinished(t *testing.T, s *Store, spec CampaignSpec, id string, at time.Time) {
	t.Helper()
	if _, err := s.Claim(spec.ID, id, at); err != nil {
		t.Fatalf("claim download %s: %v", id, err)
	}
}

// TestRestoreRunningCampaignsAccepted 保护尚未完成的正常活动：
// 设备仍处于任一非终态时活动必须保持 running，重开后正常恢复，
// 设备原有待办继续可查。设备离线或维护窗口结束都不等于设备已结束。
func TestRestoreRunningCampaignsAccepted(t *testing.T) {
	// 单个非终态设备的各种进度形态，活动都必须仍是 running，待办不丢失。
	for _, tc := range []struct {
		name    string
		setup   func(t *testing.T) (*Store, CampaignSpec)
		status  string
		phase   string
		pending string
		claimed bool
	}{
		{
			"pending download",
			func(t *testing.T) (*Store, CampaignSpec) {
				s := setupUpgrade(t, "d1")
				spec := singleDeviceSpec("cmp-1", []string{"d1"}, 1)
				createCampaign(t, s, spec)
				return s, spec
			},
			DevicePending, StageDownload, StageDownload, false,
		},
		{
			"downloading claimed unfinished",
			func(t *testing.T) (*Store, CampaignSpec) {
				s := setupUpgrade(t, "d1")
				spec := singleDeviceSpec("cmp-1", []string{"d1"}, 1)
				createCampaign(t, s, spec)
				bringOnline(t, s, "d1")
				claimDownloadUnfinished(t, s, spec, "d1", upBase)
				return s, spec
			},
			DeviceDownloading, StageDownload, StageDownload, true,
		},
		{
			"ready awaiting install",
			func(t *testing.T) (*Store, CampaignSpec) {
				s := setupUpgrade(t, "d1")
				spec := singleDeviceSpec("cmp-1", []string{"d1"}, 1)
				createCampaign(t, s, spec)
				bringOnline(t, s, "d1")
				dl, err := s.Claim(spec.ID, "d1", upBase)
				if err != nil {
					t.Fatalf("claim: %v", err)
				}
				if err := s.SubmitResult(OperationResult{
					CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
					At: upBase.Add(time.Minute), Success: true,
				}); err != nil {
					t.Fatalf("download result: %v", err)
				}
				return s, spec
			},
			DeviceReady, StageInstall, StageInstall, false,
		},
		{
			"installing claimed unfinished",
			func(t *testing.T) (*Store, CampaignSpec) {
				s := setupUpgrade(t, "d1")
				spec := singleDeviceSpec("cmp-1", []string{"d1"}, 1)
				createCampaign(t, s, spec)
				bringOnline(t, s, "d1")
				dl, err := s.Claim(spec.ID, "d1", upBase)
				if err != nil {
					t.Fatalf("claim download: %v", err)
				}
				if err := s.SubmitResult(OperationResult{
					CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
					At: upBase.Add(time.Minute), Success: true,
				}); err != nil {
					t.Fatalf("download result: %v", err)
				}
				if _, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute)); err != nil {
					t.Fatalf("claim install: %v", err)
				}
				return s, spec
			},
			DeviceInstalling, StageInstall, StageInstall, true,
		},
		{
			"awaiting rollback",
			func(t *testing.T) (*Store, CampaignSpec) {
				s, _, spec, _ := setupInstallFailure(t, true)
				return s, spec
			},
			DeviceAwaitingRollback, StageRollback, StageRollback, false,
		},
		{
			"rolling back claimed unfinished",
			func(t *testing.T) (*Store, CampaignSpec) {
				s, _, spec, _ := setupInstallFailure(t, true)
				if _, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil {
					t.Fatalf("claim rollback: %v", err)
				}
				return s, spec
			},
			DeviceRollingBack, StageRollback, StageRollback, true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, spec := tc.setup(t)
			cv := mustGetCampaign(s, spec.ID)
			if cv.Ended || cv.Status != CampaignRunning {
				t.Fatalf("precondition: campaign must be running: %+v", cv)
			}
			dir := s.dir
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s2, err := Open(dir)
			if err != nil {
				t.Fatalf("unfinished campaign must open: %v", err)
			}
			defer s2.Close()
			restored := mustGetCampaign(s2, spec.ID)
			if restored.Ended || restored.Status != CampaignRunning {
				t.Fatalf("unfinished campaign must restore running: %+v", restored)
			}
			d := findDevice(restored, "d1")
			if d.Status != tc.status || d.Phase != tc.phase {
				t.Fatalf("device progress must survive restore: want %s/%s got %s/%s",
					tc.status, tc.phase, d.Status, d.Phase)
			}
			// 设备待办继续可查，仍指向这个未结束活动。
			w, err := s2.GetDeviceWork("d1")
			if err != nil {
				t.Fatal(err)
			}
			if w.CampaignID != spec.ID || w.Pending == nil {
				t.Fatalf("pending work must survive restore: %+v", w)
			}
			if w.Pending.Kind != tc.pending || w.PendingClaimed != tc.claimed {
				t.Fatalf("pending work changed: kind=%s claimed=%t, want %s/%t",
					w.Pending.Kind, w.PendingClaimed, tc.pending, tc.claimed)
			}
		})
	}

	// 同批一台设备已经失败，另一台还没有完成：整体仍在运行，未完成设备待办保留。
	t.Run("one failed with batchmate unfinished", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := singleDeviceSpec("cmp-1", []string{"d1", "d2"}, 2)
		createCampaign(t, s, spec)
		// d1 在首次领取下载前变成不兼容版本，领取即下载失败；d2 同批不受影响。
		if err := s.Report("d1", 1, upBase.Add(-time.Minute), "v3", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		bringOnline(t, s, "d2")
		if _, err := s.Claim(spec.ID, "d1", upBase); !errors.Is(err, ErrIncompatibleVersion) {
			t.Fatalf("want incompatible version, got %v", err)
		}
		cv := mustGetCampaign(s, spec.ID)
		if cv.Ended || cv.Status != CampaignRunning {
			t.Fatalf("precondition: campaign must stay running: %+v", cv)
		}
		if findDevice(cv, "d1").Status != DeviceFailed ||
			findDevice(cv, "d2").Status != DevicePending {
			t.Fatalf("precondition: d1 failed, d2 pending: %+v", cv.Devices)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("failed plus unfinished same-batch device must open: %v", err)
		}
		defer s2.Close()
		restored := mustGetCampaign(s2, spec.ID)
		if restored.Ended || restored.Status != CampaignRunning {
			t.Fatalf("campaign must restore running: %+v", restored)
		}
		w, err := s2.GetDeviceWork("d2")
		if err != nil {
			t.Fatal(err)
		}
		if w.CampaignID != spec.ID || w.Pending == nil ||
			w.Pending.Kind != StageDownload || w.PendingClaimed {
			t.Fatalf("d2 pending work lost: %+v", w)
		}
	})

	// 开启回滚：后续批次已记为未执行，但失败设备还在等待回滚时，活动不能提前结束。
	t.Run("later batches skipped while awaiting rollback", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := singleDeviceSpec("cmp-1", []string{"d1", "d2"}, 1)
		spec.RollbackOnFailure = true
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, err := s.Claim(spec.ID, "d1", upBase)
		if err != nil {
			t.Fatalf("claim download: %v", err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(time.Minute), Success: true,
		}); err != nil {
			t.Fatalf("download result: %v", err)
		}
		in, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("claim install: %v", err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
			At: upBase.Add(3 * time.Minute), Success: false, Reason: "install boom",
		}); err != nil {
			t.Fatalf("install failure: %v", err)
		}
		cv := mustGetCampaign(s, spec.ID)
		if cv.Ended || cv.Status != CampaignRunning {
			t.Fatalf("precondition: campaign must stay running while rollback pending: %+v", cv)
		}
		if findDevice(cv, "d1").Status != DeviceAwaitingRollback ||
			findDevice(cv, "d2").Status != DeviceSkipped {
			t.Fatalf("precondition: d1 awaiting rollback, d2 skipped: %+v", cv.Devices)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("awaiting rollback with skipped later batch must open: %v", err)
		}
		defer s2.Close()
		restored := mustGetCampaign(s2, spec.ID)
		if restored.Ended || restored.Status != CampaignRunning {
			t.Fatalf("campaign must restore running: %+v", restored)
		}
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending == nil || w.Pending.Kind != StageRollback || w.PendingClaimed {
			t.Fatalf("rollback todo lost: %+v", w)
		}
	})

	// 设备离线或维护窗口结束都不改变活动结论：下载已领取但未完成、设备离线，
	// 活动仍必须按 running 恢复。
	t.Run("offline after claim stays running", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := singleDeviceSpec("cmp-1", []string{"d1"}, 1)
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		if _, err := s.Claim(spec.ID, "d1", upBase); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := s.SetOffline("d1"); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("offline device with claimed op must open running: %v", err)
		}
		defer s2.Close()
		restored := mustGetCampaign(s2, spec.ID)
		if restored.Ended || restored.Status != CampaignRunning {
			t.Fatalf("offline is not a terminal device state: %+v", restored)
		}
	})
}

// TestRestoreEndedCampaignsAccepted 保护全部设备终态的合法结论：
// 全部 succeeded 才能 succeeded；失败、超时、未执行及任何回滚终态
// （含回滚成功——只表示恢复原版本）都只能是 failed。
func TestRestoreEndedCampaignsAccepted(t *testing.T) {
	t.Run("all succeeded", func(t *testing.T) {
		_, dir, spec := setupSucceeded(t)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("all-succeeded campaign must open: %v", err)
		}
		defer s.Close()
		cv := mustGetCampaign(s, spec.ID)
		if !cv.Ended || cv.Status != CampaignSucceeded {
			t.Fatalf("all-succeeded must restore succeeded/ended: %+v", cv)
		}
	})

	for _, tc := range []struct {
		name   string
		setup  func(t *testing.T) (*Store, CampaignSpec, string)
		device string
	}{
		{
			"download failure then skipped",
			func(t *testing.T) (*Store, CampaignSpec, string) {
				s := setupUpgrade(t, "d1", "d2")
				spec := singleDeviceSpec("cmp-1", []string{"d1", "d2"}, 1)
				createCampaign(t, s, spec)
				if err := s.Report("d1", 1, upBase.Add(-time.Minute), "v3", json.RawMessage(`{}`)); err != nil {
					t.Fatal(err)
				}
				if _, err := s.Claim(spec.ID, "d1", upBase); !errors.Is(err, ErrIncompatibleVersion) {
					t.Fatalf("want incompatible, got %v", err)
				}
				return s, spec, "d1"
			},
			"d1",
		},
		{
			"deadline timeout",
			func(t *testing.T) (*Store, CampaignSpec, string) {
				s := setupUpgrade(t, "d1")
				spec := singleDeviceSpec("cmp-1", []string{"d1"}, 1)
				createCampaign(t, s, spec)
				bringOnline(t, s, "d1")
				if _, err := s.Claim(spec.ID, "d1", upBase); err != nil {
					t.Fatal(err)
				}
				if err := s.AdvanceCampaign(spec.ID, spec.Deadline); err != nil {
					t.Fatal(err)
				}
				return s, spec, "d1"
			},
			"d1",
		},
		{
			"rollback succeeded is still failure",
			func(t *testing.T) (*Store, CampaignSpec, string) {
				s, _, spec, rbOp := setupInstallFailure(t, true)
				if _, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil {
					t.Fatal(err)
				}
				if err := s.SubmitResult(OperationResult{
					CampaignID: spec.ID, DeviceID: "d1", OperationID: rbOp.ID,
					At: upBase.Add(5 * time.Minute), Success: true,
					Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
				}); err != nil {
					t.Fatalf("rollback success: %v", err)
				}
				return s, spec, "d1"
			},
			"d1",
		},
		{
			"rollback failed is failure",
			func(t *testing.T) (*Store, CampaignSpec, string) {
				s, _, spec, rbOp := setupInstallFailure(t, true)
				if _, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil {
					t.Fatal(err)
				}
				if err := s.SubmitResult(OperationResult{
					CampaignID: spec.ID, DeviceID: "d1", OperationID: rbOp.ID,
					At: upBase.Add(5 * time.Minute), Success: false, Reason: "rollback boom",
				}); err != nil {
					t.Fatalf("rollback failure: %v", err)
				}
				return s, spec, "d1"
			},
			"d1",
		},
		{
			"rollback timeout is failure",
			func(t *testing.T) (*Store, CampaignSpec, string) {
				s, _, spec, _ := setupInstallFailure(t, true)
				if _, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil {
					t.Fatal(err)
				}
				if err := s.AdvanceCampaign(spec.ID, spec.Deadline); err != nil {
					t.Fatal(err)
				}
				return s, spec, "d1"
			},
			"d1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, spec, device := tc.setup(t)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s2, err := Open(s.dir)
			if err != nil {
				t.Fatalf("terminal failure campaign must open: %v", err)
			}
			defer s2.Close()
			cv := mustGetCampaign(s2, spec.ID)
			if !cv.Ended || cv.Status != CampaignFailed {
				t.Fatalf("%s must restore failed/ended: %+v", tc.name, cv)
			}
			if !isTerminal(findDevice(cv, device).Status) {
				t.Fatalf("device must be terminal: %+v", findDevice(cv, device))
			}
			// 已结束活动不再占用设备待办。
			w, err := s2.GetDeviceWork(device)
			if err != nil || w.CampaignID != "" || w.Pending != nil {
				t.Fatalf("ended campaign must leave no pending work: %+v err=%v", w, err)
			}
		})
	}
}

// TestRestoreCampaignOutcomeContradictionsRefused 针对“保存内容把仍有待办的
// 活动写成已结束，或把全部安装成功的活动写成失败仍能打开”的问题：
// 活动整体状态或结束标记只要与设备进度矛盾，Open 必须返回 ErrCorruptStorage，
// 不返回可用存储，原文件保持原样，不改写结论、不改动设备状态、不删除活动。
func TestRestoreCampaignOutcomeContradictionsRefused(t *testing.T) {
	// 仍有非终态设备的活动被写成已结束（或整体状态非 running）。
	t.Run("unfinished devices but campaign ended failed", func(t *testing.T) {
		_, dir, _ := setupReady(t)
		assertReopenCorrupt(t, dir, markEnded(t, dir, "cmp-1", CampaignFailed))
	})
	t.Run("unfinished devices but campaign ended succeeded", func(t *testing.T) {
		_, dir, _ := setupReady(t)
		assertReopenCorrupt(t, dir, markEnded(t, dir, "cmp-1", CampaignSucceeded))
	})
	t.Run("unfinished devices but running status marked ended", func(t *testing.T) {
		_, dir, _ := setupReady(t)
		original := setCampaignOutcome(t, dir, "cmp-1", func(camp map[string]any) {
			camp["status"] = CampaignRunning
			camp["ended"] = true
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("unfinished devices marked failed without ended flag", func(t *testing.T) {
		_, dir, _ := setupReady(t)
		original := setCampaignOutcome(t, dir, "cmp-1", func(camp map[string]any) {
			camp["status"] = CampaignFailed
			camp["ended"] = false
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("awaiting rollback but campaign ended", func(t *testing.T) {
		s, _, spec, _ := setupInstallFailure(t, true)
		dir := s.dir
		s.Close()
		assertReopenCorrupt(t, dir, markEnded(t, dir, spec.ID, CampaignFailed))
	})
	t.Run("rolling back but campaign ended", func(t *testing.T) {
		s, _, spec, _ := setupInstallFailure(t, true)
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		assertReopenCorrupt(t, dir, markEnded(t, dir, spec.ID, CampaignFailed))
	})
	t.Run("one failed one pending but campaign ended", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := singleDeviceSpec("cmp-1", []string{"d1", "d2"}, 2)
		createCampaign(t, s, spec)
		if err := s.Report("d1", 1, upBase.Add(-time.Minute), "v3", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Claim(spec.ID, "d1", upBase); !errors.Is(err, ErrIncompatibleVersion) {
			t.Fatalf("want incompatible, got %v", err)
		}
		dir := s.dir
		s.Close()
		assertReopenCorrupt(t, dir, markEnded(t, dir, spec.ID, CampaignFailed))
	})
	t.Run("skipped later batch but awaiting rollback ended", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := singleDeviceSpec("cmp-1", []string{"d1", "d2"}, 1)
		spec.RollbackOnFailure = true
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, err := s.Claim(spec.ID, "d1", upBase)
		if err != nil {
			t.Fatalf("claim download: %v", err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(time.Minute), Success: true,
		}); err != nil {
			t.Fatalf("download result: %v", err)
		}
		in, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
		if err != nil {
			t.Fatalf("claim install: %v", err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
			At: upBase.Add(3 * time.Minute), Success: false, Reason: "install boom",
		}); err != nil {
			t.Fatalf("install failure: %v", err)
		}
		dir := s.dir
		s.Close()
		// d2 已 skipped，但 d1 仍在等待回滚：活动不能提前结束。
		assertReopenCorrupt(t, dir, markEnded(t, dir, spec.ID, CampaignFailed))
	})

	// 全部设备终态的活动被写成未结束，或结论与设备终态组合不符。
	t.Run("all succeeded written as failed", func(t *testing.T) {
		_, dir, _ := setupSucceeded(t)
		original := setCampaignOutcome(t, dir, "cmp-1", func(camp map[string]any) {
			camp["status"] = CampaignFailed
			camp["ended"] = true
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("all succeeded but status running not ended", func(t *testing.T) {
		_, dir, _ := setupSucceeded(t)
		original := setCampaignOutcome(t, dir, "cmp-1", func(camp map[string]any) {
			camp["status"] = CampaignRunning
			camp["ended"] = false
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("all succeeded but ended flag cleared", func(t *testing.T) {
		_, dir, _ := setupSucceeded(t)
		original := setCampaignOutcome(t, dir, "cmp-1", func(camp map[string]any) {
			camp["status"] = CampaignSucceeded
			camp["ended"] = false
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("failed devices written as succeeded", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := singleDeviceSpec("cmp-1", []string{"d1", "d2"}, 1)
		createCampaign(t, s, spec)
		if err := s.Report("d1", 1, upBase.Add(-time.Minute), "v3", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Claim(spec.ID, "d1", upBase); !errors.Is(err, ErrIncompatibleVersion) {
			t.Fatalf("want incompatible, got %v", err)
		}
		dir := s.dir
		s.Close()
		original := setCampaignOutcome(t, dir, "cmp-1", func(camp map[string]any) {
			camp["status"] = CampaignSucceeded
			camp["ended"] = true
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("rollback succeeded device written as campaign succeeded", func(t *testing.T) {
		s, _, spec, rbOp := setupInstallFailure(t, true)
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rbOp.ID,
			At: upBase.Add(5 * time.Minute), Success: true,
			Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("rollback success: %v", err)
		}
		dir := s.dir
		s.Close()
		// 回滚成功只表示恢复原版本，不能算本次升级成功。
		original := setCampaignOutcome(t, dir, spec.ID, func(camp map[string]any) {
			camp["status"] = CampaignSucceeded
			camp["ended"] = true
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("timeout devices but campaign not ended", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := singleDeviceSpec("cmp-1", []string{"d1"}, 1)
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		if _, err := s.Claim(spec.ID, "d1", upBase); err != nil {
			t.Fatal(err)
		}
		if err := s.AdvanceCampaign(spec.ID, spec.Deadline); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		original := setCampaignOutcome(t, dir, "cmp-1", func(camp map[string]any) {
			camp["status"] = CampaignRunning
			camp["ended"] = false
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 即使其余活动与设备全部合法，一个活动结论矛盾也必须拒绝打开整个存储，
	// 不能只恢复合法部分；原文件保持不变。
	t.Run("one contradictory campaign rejects whole store", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2", "d3")
		// cmp-1：d1 全部成功，合法已结束。
		oldSpec := activeCampaignSpec("cmp-1", upBase, []string{"d1"}, 1)
		createCampaign(t, s, oldSpec)
		bringOnline(t, s, "d1")
		finishDevice(t, s, oldSpec, "d1", upBase)
		dstDir := s.dir
		s.Close()

		// cmp-2：d2、d3 仍在 pending（合法 running），合并后再把结论改坏。
		src := setupUpgrade(t, "d1", "d2", "d3")
		newSpec := activeCampaignSpec("cmp-2", upBase.Add(3*time.Hour), []string{"d2", "d3"}, 2)
		createCampaign(t, src, newSpec)
		srcDir := src.dir
		src.Close()

		mergeCampaignIntoStore(t, dstDir, srcDir, "cmp-2")
		original := markEnded(t, dstDir, "cmp-2", CampaignFailed)
		assertReopenCorrupt(t, dstDir, original)
	})
}
