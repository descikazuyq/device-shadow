package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// mutateDeviceDoc 直接改写存储文件中某台设备的设备级磁盘记录
// （status/phase/reason/at 等，区别于各阶段操作记录）。
func mutateDeviceDoc(t *testing.T, dir, campaignID, deviceID string, fn func(map[string]any)) []byte {
	t.Helper()
	return rewriteStoreBytes(t, dir, func(doc map[string]any) {
		fn(campaignDeviceDoc(doc, campaignID, deviceID))
	})
}

// setupDownloadFailedClosed 构造未开启回滚的单设备活动：d1 已领取下载并提交
// 下载失败，活动已失败结束，随后关闭存储返回目录。
func setupDownloadFailedClosed(t *testing.T, reason string, failAt time.Time) (string, CampaignSpec) {
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
		At: failAt, Success: false, Reason: reason,
	}); err != nil {
		t.Fatalf("download failure: %v", err)
	}
	return closeStore(t, s), spec
}

// setupInstallFailedNoRollbackClosed 构造下载成功、安装失败已接受且未开启回滚
// 的单设备活动：设备直接以 failed/install 结束，随后关闭存储返回目录。
func setupInstallFailedNoRollbackClosed(t *testing.T) (string, CampaignSpec) {
	t.Helper()
	s, _, spec, _ := setupInstallFailure(t, false)
	return closeStore(t, s), spec
}

// TestRestoreFailedDeviceMatchesAcceptedResult 保护重开存储时活动查询中的设备
// 失败信息与本设备已接受失败结果之间的对账：下载失败以及未开启失败回滚时直接
// 结束的安装失败，失败阶段只能是 download 或 install，且设备状态中的失败原因
// 必须与该设备在本活动中该阶段已接受失败结果的原因逐字一致、失败时间必须与
// 首次接受该结果的时间表示同一实际时刻（时区写法不同但时刻相同合法）。
// 即使原因非空、时间非零、操作结果与结果历史彼此自洽，设备状态换了阶段、
// 原因或时间都必须返回 errors.Is 可判断的 ErrCorruptStorage，拒绝打开整个
// 存储：不能只跳过这台设备，也不能改写原因、挪动时间或删去历史让文件打开，
// 已有文件内容必须保持原样。
func TestRestoreFailedDeviceMatchesAcceptedResult(t *testing.T) {
	const dlReason = "download interrupted"
	failAt := upBase.Add(10 * time.Minute) // 12:10

	// ---- 下载失败 --------------------------------------------------------

	t.Run("download failure legit record reopens", func(t *testing.T) {
		dir, spec := setupDownloadFailedClosed(t, dlReason, failAt)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("legit download failure must reopen: %v", err)
		}
		cv := mustGetCampaign(s, spec.ID)
		if !cv.Ended || cv.Status != CampaignFailed {
			t.Fatalf("campaign failed/ended: %+v", cv)
		}
		d := findDevice(cv, "d1")
		if d.Status != DeviceFailed || d.Phase != StageDownload ||
			d.Reason != dlReason || !d.At.Equal(failAt) {
			t.Fatalf("device failure view altered: %+v", d)
		}
		if len(cv.Results) != 1 || cv.Results[0].Stage != StageDownload ||
			cv.Results[0].Reason != dlReason || !cv.Results[0].At.Equal(failAt) {
			t.Fatalf("failure history altered: %+v", cv.Results)
		}
		s.Close()
	})

	// 设备状态写成安装阶段（“安装失败”），但本设备唯一已接受的失败是下载失败：
	// 安装操作没有失败结果，必须拒绝打开。
	t.Run("download failure reported as install phase", func(t *testing.T) {
		dir, spec := setupDownloadFailedClosed(t, dlReason, failAt)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["phase"] = StageInstall
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 设备状态写成回滚阶段也不能解释下载失败。
	t.Run("download failure reported as rollback phase", func(t *testing.T) {
		dir, spec := setupDownloadFailedClosed(t, dlReason, failAt)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["phase"] = StageRollback
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 原因非空但与已接受结果逐字不同：操作结果与历史仍都是“download
	// interrupted”，设备状态却写成另一个原因，必须拒绝。
	t.Run("download failure reason mismatch", func(t *testing.T) {
		dir, spec := setupDownloadFailedClosed(t, dlReason, failAt)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["reason"] = "download interrupted differently"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 时间非零但不是首次接受结果的时刻：12:12 不能解释 12:10 的失败。
	t.Run("download failure time mismatch", func(t *testing.T) {
		dir, spec := setupDownloadFailedClosed(t, dlReason, failAt)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["at"] = "2026-10-02T12:12:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 同一实际时刻、仅时区写法不同（12:10Z == 20:10+08:00）必须正常打开。
	t.Run("download failure same instant different zone reopens", func(t *testing.T) {
		dir, spec := setupDownloadFailedClosed(t, dlReason, failAt)
		rewriteStoreBytes(t, dir, func(doc map[string]any) {
			d := campaignDeviceDoc(doc, spec.ID, "d1")
			d["at"] = "2026-10-02T20:10:00+08:00"
		})
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("same instant in another zone must reopen: %v", err)
		}
		defer s.Close()
		d := findDevice(mustGetCampaign(s, spec.ID), "d1")
		if !d.At.Equal(failAt) {
			t.Fatalf("failure time not restored as same instant: %v", d.At)
		}
	})

	// ---- 未开启回滚时的安装失败 ------------------------------------------

	t.Run("install failure legit record reopens", func(t *testing.T) {
		dir, spec := setupInstallFailedNoRollbackClosed(t)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("legit install failure must reopen: %v", err)
		}
		defer s.Close()
		d := findDevice(mustGetCampaign(s, spec.ID), "d1")
		wantAt := upBase.Add(3 * time.Minute)
		if d.Status != DeviceFailed || d.Phase != StageInstall ||
			d.Reason != "install boom" || !d.At.Equal(wantAt) {
			t.Fatalf("install failure view: %+v", d)
		}
	})

	// 安装失败写成下载阶段：下载结果是成功，无法支持 failed/download。
	t.Run("install failure reported as download phase", func(t *testing.T) {
		dir, spec := setupInstallFailedNoRollbackClosed(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["phase"] = StageDownload
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("install failure reason mismatch", func(t *testing.T) {
		dir, spec := setupInstallFailedNoRollbackClosed(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["reason"] = "another install cause"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("install failure time mismatch", func(t *testing.T) {
		dir, spec := setupInstallFailedNoRollbackClosed(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["at"] = "2026-10-02T12:30:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 安装失败时间用另一时区表示同一时刻：合法。
	t.Run("install failure same instant different zone reopens", func(t *testing.T) {
		dir, spec := setupInstallFailedNoRollbackClosed(t)
		wantAt := upBase.Add(3 * time.Minute)
		rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, spec.ID, "d1")["at"] = "2026-10-02T20:03:00+08:00"
		})
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("same instant in another zone must reopen: %v", err)
		}
		defer s.Close()
		if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); !d.At.Equal(wantAt) {
			t.Fatalf("failure time not restored as same instant: %v", d.At)
		}
	})
}

// TestRestoreFailedDeviceAnchoredToOwnResult 证明设备失败信息只能由本设备在
// 本活动中的失败结果锚定：同批其他设备恰好有相同原因的失败、活动后来在更晚
// 时间结束，都不能解释本设备状态与自己结果的矛盾；但活动晚于失败结束本身
// 合法，原失败原因与时间必须原样保留。
func TestRestoreFailedDeviceAnchoredToOwnResult(t *testing.T) {
	// 同批两台设备先后下载失败，原因各不相同；把 d1 的设备状态原因改成 d2
	// 的失败原因（d1 自己的操作结果与历史保持原样），即使该原因在同活动中
	// 真实存在也必须拒绝——不能借同批其他设备的失败记录解释 d1。
	t.Run("batchmate failure reason cannot anchor this device", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"} // 同批，批大小 2
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		dl1, _ := s.Claim(spec.ID, "d1", upBase)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl1.ID,
			At: upBase.Add(time.Minute), Success: false, Reason: "d1 boom",
		}); err != nil {
			t.Fatalf("d1 failure: %v", err)
		}
		dl2, _ := s.Claim(spec.ID, "d2", upBase.Add(2*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d2", OperationID: dl2.ID,
			At: upBase.Add(3 * time.Minute), Success: false, Reason: "d2 boom",
		}); err != nil {
			t.Fatalf("d2 failure: %v", err)
		}
		dir := closeStore(t, s)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["reason"] = "d2 boom" // 同批 d2 的真实失败原因，却不属于 d1。
		})
		assertReopenCorrupt(t, dir, original)
	})

	// d1 在 12:01 下载失败，同批 d2 继续处理并在 12:05 才成功，活动随之在
	// 12:05 失败结束（晚于 d1 的失败时间）：合法，重开后 d1 仍保留 12:01
	// 的原失败原因与时间，不能被活动结束时间覆盖或挪动。
	t.Run("campaign ends later without touching earlier failure", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		dl1, _ := s.Claim(spec.ID, "d1", upBase)
		d1FailAt := upBase.Add(time.Minute)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl1.ID,
			At: d1FailAt, Success: false, Reason: "d1 boom",
		}); err != nil {
			t.Fatalf("d1 failure: %v", err)
		}
		// 同批其他设备仍可继续：d2 完整成功，活动因 d1 失败而失败结束。
		finishDevice(t, s, spec, "d2", upBase.Add(2*time.Minute))
		cv := mustGetCampaign(s, spec.ID)
		if !cv.Ended || !cv.EndedAt.After(d1FailAt) {
			t.Fatalf("precondition: campaign ends after d1 failure, %+v", cv)
		}
		if findDevice(cv, "d2").Status != DeviceSucceeded {
			t.Fatalf("d2 should keep running and succeed: %+v", cv)
		}
		dir := closeStore(t, s)

		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("campaign ending later than a device failure must reopen: %v", err)
		}
		defer s2.Close()
		cv2 := mustGetCampaign(s2, spec.ID)
		d1 := findDevice(cv2, "d1")
		if d1.Status != DeviceFailed || d1.Phase != StageDownload ||
			d1.Reason != "d1 boom" || !d1.At.Equal(d1FailAt) {
			t.Fatalf("original failure info must be preserved: %+v", d1)
		}
		if !cv2.EndedAt.Equal(cv.EndedAt) {
			t.Fatalf("campaign end time changed: %v want %v", cv2.EndedAt, cv.EndedAt)
		}
	})
}

// TestRestoreClaimTimeDownloadFailureIsValid 首次领取下载时复查版本不兼容而
// 直接生成的下载失败同样是有效失败结果（不要求设备主动提交）：合法记录原样
// 打开并可查询；设备状态原因一旦与该结果不一致，仍拒绝打开整个存储。
func TestRestoreClaimTimeDownloadFailureIsValid(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	// 首次领取前把版本上报成不兼容的 v3。
	if err := s.Report("d1", 2, upBase.Add(-time.Minute), "v3", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	failAt := upBase
	if _, err := s.Claim(spec.ID, "d1", failAt); !errors.Is(err, ErrIncompatibleVersion) {
		t.Fatalf("want ErrIncompatibleVersion, got %v", err)
	}
	wantReason := "version v3 is not compatible with target v2"
	dir := closeStore(t, s)

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("claim-time download failure must reopen: %v", err)
	}
	d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
	if d.Status != DeviceFailed || d.Phase != StageDownload ||
		d.Reason != wantReason || !d.At.Equal(failAt) {
		t.Fatalf("claim-time failure view: %+v", d)
	}
	s2.Close()

	original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(doc map[string]any) {
		doc["reason"] = "version v9 is not compatible with target v2"
	})
	assertReopenCorrupt(t, dir, original)
}

// TestRestoreRollbackCampaignDownloadFailureRecord 开启回滚的活动中，下载失败
// 仍直接以 failed/download 结束、不进入回滚：合法记录正常打开；把它写成回滚
// 阶段时由失败设备对账拒绝（未开启回滚活动的回滚阶段另有统一拦截）。
func TestRestoreRollbackCampaignDownloadFailureRecord(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	spec.RollbackOnFailure = true
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, _ := s.Claim(spec.ID, "d1", upBase)
	failAt := upBase.Add(time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: failAt, Success: false, Reason: "dl down",
	}); err != nil {
		t.Fatalf("download failure in rollback campaign: %v", err)
	}
	dir := closeStore(t, s)

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("download failure in rollback campaign must reopen: %v", err)
	}
	d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
	// 首次领取下载时已锁定回滚目标，但下载失败直接结束，不产生回滚结果。
	if d.Status != DeviceFailed || d.Phase != StageDownload ||
		d.RollbackTarget != "v1" || d.RollbackResult {
		t.Fatalf("download failure must end directly despite locked target: %+v", d)
	}
	s2.Close()

	original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(doc map[string]any) {
		doc["phase"] = StageRollback
	})
	assertReopenCorrupt(t, dir, original)
}

// TestRestoreFailedDeviceFileUntouchedOnRejection 拒绝打开时不仅返回
// ErrCorruptStorage，已有的损坏文件内容还必须逐字节保留（这里用矛盾时间的
// 下载失败做一次端到端确认）。
func TestRestoreFailedDeviceFileUntouchedOnRejection(t *testing.T) {
	dir, spec := setupDownloadFailedClosed(t, "download interrupted", upBase.Add(10*time.Minute))
	original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
		d["at"] = "2026-10-02T12:12:00Z"
	})
	if s, err := Open(dir); err == nil {
		s.Close()
		t.Fatalf("expected ErrCorruptStorage")
	} else if !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("error must wrap ErrCorruptStorage: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, storeFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(original) {
		t.Fatalf("corrupt file must be left untouched")
	}
}
