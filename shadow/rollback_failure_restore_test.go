package shadow

import (
	"encoding/json"
	"testing"
	"time"
)

// 本文件保护 rollback_failed 设备在打开本地存储时的对账：开启安装失败回滚的
// 活动中，设备以回滚失败结束时，当前状态中的失败原因必须与本设备在本活动中
// 首次接受的回滚失败结果逐字一致，状态时间必须与首次接受该结果的时间表示
// 同一实际时刻。即使回滚操作结果与结果历史彼此自洽，把当前原因写成另一段
// 非空文字（含原安装失败原因、仅多空格），或把状态时间退回原安装失败等
// 其他时刻，都必须返回 errors.Is 可判断的 ErrCorruptStorage，整个存储拒绝
// 打开、已有文件内容保持原样——不能改写失败说明、删除历史或跳过设备让数据
// 继续可用。判断依据只能是本设备本活动的回滚失败：其他设备的失败、原安装
// 失败与设备当前影子版本都不能替代它；活动仍在等待同批其他设备或已经结束
// 都同样生效。

// rbFailAt 是测试时间线中回滚失败被接受的时刻（见 buildRollingBase /
// finishRollbackFailure）：安装失败 +5m，领取回滚 +8m，回滚失败 +9m。
func rbFailAt(base time.Time) time.Time { return base.Add(9 * time.Minute) }

// setupRollbackFailedClosed 构造开启回滚的单设备活动：d1 安装失败进入回滚，
// 回滚以 "rb boom" 失败，活动随之失败结束；随后关闭存储返回目录。
func setupRollbackFailedClosed(t *testing.T) (string, CampaignSpec) {
	t.Helper()
	s, spec := buildRollingBase(t, false)
	finishRollbackFailure(t, s, spec)
	return closeStore(t, s), spec
}

// TestRestoreRollbackFailedMatchesAcceptedResult 保护核心修复：rollback_failed
// 设备的当前失败说明与状态时间此前不与已接受的回滚失败结果对账，原因被换成
// 另一段非空文字、或时间被挪走（含退回原安装失败时刻）也能打开，活动查询会
// 同时显示互相矛盾的当前失败说明与回滚结果。这些矛盾记录现在都必须在 Open
// 时整体拒绝。
func TestRestoreRollbackFailedMatchesAcceptedResult(t *testing.T) {
	installAt := installFailAt(upBase) // 12:05，安装失败
	rbAt := rbFailAt(upBase)           // 12:09，回滚失败

	// 合法记录照常打开：当前失败说明对应回滚失败，原安装失败说明、回滚结果
	// 与历史全部保留；设备影子（版本、双方配置、修订号、审计）保持原值。
	t.Run("legit rollback failed record reopens", func(t *testing.T) {
		dir, spec := setupRollbackFailedClosed(t)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("legit rollback failure must reopen: %v", err)
		}
		defer s.Close()
		cv := mustGetCampaign(s, spec.ID)
		if !cv.Ended || cv.Status != CampaignFailed {
			t.Fatalf("campaign failed/ended: %+v", cv)
		}
		d := findDevice(cv, "d1")
		if d.Status != DeviceRollbackFailed || d.Phase != StageRollback ||
			d.Reason != "rb boom" || !d.At.Equal(rbAt) {
			t.Fatalf("device rollback failure view altered: %+v", d)
		}
		if d.InstallFailReason != "install broken" || !d.InstallFailAt.Equal(installAt) {
			t.Fatalf("install failure info altered: %+v", d)
		}
		if !d.RollbackResult || d.RollbackSuccess ||
			d.RollbackReason != "rb boom" || !d.RollbackAt.Equal(rbAt) {
			t.Fatalf("rollback result altered: %+v", d)
		}
		// 历史保留下载成功、安装失败、回滚失败三条，原因与时间原样。
		if len(cv.Results) != 3 {
			t.Fatalf("result history altered: %+v", cv.Results)
		}
		if cv.Results[1].Stage != StageInstall || cv.Results[1].Success ||
			cv.Results[1].Reason != "install broken" || !cv.Results[1].At.Equal(installAt) {
			t.Fatalf("install failure history altered: %+v", cv.Results[1])
		}
		if cv.Results[2].Stage != StageRollback || cv.Results[2].Success ||
			cv.Results[2].Reason != "rb boom" || !cv.Results[2].At.Equal(rbAt) {
			t.Fatalf("rollback failure history altered: %+v", cv.Results[2])
		}
		// 回滚失败不附带、不应用上报：影子保持回滚前的版本与双方配置。
		sh, err := s.Get("d1")
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if sh.Version != "v1" || sh.Revision != 0 || sh.LastSeq != 1 ||
			string(sh.Desired) != `{}` || string(sh.Reported) != `{}` {
			t.Fatalf("shadow altered: %+v", sh)
		}
		if audit, _ := s.Audit("d1"); len(audit) != 0 {
			t.Fatalf("audit altered: %+v", audit)
		}
	})

	// 当前原因被换成原安装失败原因：该原因在本设备自己的安装结果与历史中
	// 真实存在，但它不是回滚失败结果，必须拒绝。
	t.Run("reason replaced with install failure reason", func(t *testing.T) {
		dir, spec := setupRollbackFailedClosed(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["reason"] = "install broken"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 当前原因被换成另一段非空文字：回滚操作结果与历史仍是 "rb boom"。
	t.Run("reason replaced with other text", func(t *testing.T) {
		dir, spec := setupRollbackFailedClosed(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["reason"] = "image checksum mismatch"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 原因中增加空格也属于内容不同。
	t.Run("reason with extra space", func(t *testing.T) {
		dir, spec := setupRollbackFailedClosed(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["reason"] = "rb boom "
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 状态时间退回原安装失败的时刻（12:05）：该时刻在本设备自己的安装结果
	// 与历史中真实存在，但它不是接受回滚失败的时刻，必须拒绝。
	t.Run("time moved back to install failure", func(t *testing.T) {
		dir, spec := setupRollbackFailedClosed(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["at"] = "2026-10-02T12:05:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 状态时间被改成另一个非零时刻：12:30 不能解释 12:09 接受的回滚失败。
	t.Run("time moved to another instant", func(t *testing.T) {
		dir, spec := setupRollbackFailedClosed(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["at"] = "2026-10-02T12:30:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 同一实际时刻、仅时区写法不同（12:09Z == 20:09+08:00）必须正常打开。
	t.Run("same instant different zone reopens", func(t *testing.T) {
		dir, spec := setupRollbackFailedClosed(t)
		rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, spec.ID, "d1")["at"] = "2026-10-02T20:09:00+08:00"
		})
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("same instant in another zone must reopen: %v", err)
		}
		defer s.Close()
		d := findDevice(mustGetCampaign(s, spec.ID), "d1")
		if d.Status != DeviceRollbackFailed || d.Reason != "rb boom" || !d.At.Equal(rbAt) {
			t.Fatalf("failure time not restored as same instant: %+v", d)
		}
	})
}

// TestRestoreRollbackFailedAnchoredToOwnResult 证明对账只能锚定本设备在本活动
// 中的回滚失败结果：同活动另一台设备的回滚失败原因真实存在，也不能解释本
// 设备状态与自己回滚结果的矛盾。
func TestRestoreRollbackFailedAnchoredToOwnResult(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2")
	spec := rbSpec()
	spec.Devices = []string{"d1", "d2"}
	spec.BatchSize = 2 // 同批，各自推进
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	// d1 先完成回滚失败（原因 "rb boom"）。
	awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute)); err != nil {
		t.Fatalf("claim d1 rollback: %v", err)
	}
	finishRollbackFailure(t, s, spec)
	// d2 随后也回滚失败，原因不同（"d2 rb dead"），活动失败结束。
	awaitRollback(t, s, spec, "d2", upBase.Add(10*time.Minute))
	rb2, err := s.Claim(spec.ID, "d2", upBase.Add(16*time.Minute))
	if err != nil || rb2 == nil {
		t.Fatalf("claim d2 rollback: %v %+v", err, rb2)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d2", OperationID: rb2.ID,
		At: upBase.Add(17 * time.Minute), Success: false, Reason: "d2 rb dead",
	}); err != nil {
		t.Fatalf("d2 rollback failure: %v", err)
	}
	if cv := mustGetCampaign(s, spec.ID); !cv.Ended {
		t.Fatalf("precondition: campaign ended, %+v", cv)
	}
	dir := closeStore(t, s)
	// 把 d1 的当前失败说明改成 d2 的真实回滚失败原因；d1 自己的回滚结果与
	// 历史保持原样。即使该原因在同一活动中真实存在也必须拒绝。
	original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
		d["reason"] = "d2 rb dead"
	})
	assertReopenCorrupt(t, dir, original)
}

// TestRestoreRollbackFailedWhileCampaignRunning 覆盖活动仍在等待同批其他设备
// 的情形：d1 已回滚失败、d2 尚未领取任何操作时活动仍在执行，合法记录照常
// 打开；此时篡改 d1 的当前失败说明同样必须整体拒绝。
func TestRestoreRollbackFailedWhileCampaignRunning(t *testing.T) {
	build := func(t *testing.T) (string, CampaignSpec) {
		s := setupUpgrade(t, "d1", "d2")
		spec := rbSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 2
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute)); err != nil {
			t.Fatalf("claim d1 rollback: %v", err)
		}
		finishRollbackFailure(t, s, spec)
		if cv := mustGetCampaign(s, spec.ID); cv.Ended || cv.Status != CampaignRunning {
			t.Fatalf("precondition: campaign still running, %+v", cv)
		}
		return closeStore(t, s), spec
	}

	t.Run("legit running campaign reopens", func(t *testing.T) {
		dir, spec := build(t)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("running campaign with rollback failure must reopen: %v", err)
		}
		defer s.Close()
		cv := mustGetCampaign(s, spec.ID)
		if cv.Ended || cv.Status != CampaignRunning {
			t.Fatalf("campaign must stay running: %+v", cv)
		}
		d1 := findDevice(cv, "d1")
		if d1.Status != DeviceRollbackFailed || d1.Reason != "rb boom" ||
			!d1.At.Equal(rbFailAt(upBase)) {
			t.Fatalf("d1 rollback failure view: %+v", d1)
		}
		if d2 := findDevice(cv, "d2"); d2.Status != DevicePending {
			t.Fatalf("d2 still pending: %+v", d2)
		}
	})

	t.Run("reason mismatch rejected while running", func(t *testing.T) {
		dir, spec := build(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["reason"] = "install broken"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("time mismatch rejected while running", func(t *testing.T) {
		dir, spec := build(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["at"] = "2026-10-02T12:05:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreRollbackFailedCampaignEndsLater 覆盖同批其他设备后来才结束、活动
// 结束时间晚于本设备回滚失败时间的情形：这不构成矛盾，合法记录照常打开，
// 本设备的回滚失败说明与时间保持原值，不被活动结束时间覆盖。
func TestRestoreRollbackFailedCampaignEndsLater(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2")
	spec := rbSpec()
	spec.Devices = []string{"d1", "d2"}
	spec.BatchSize = 2
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	// d1 在 12:09 回滚失败；同批 d2 继续处理，12:13 才安装成功，
	// 活动随之在 12:13 失败结束（晚于 d1 的回滚失败时间）。
	awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute)); err != nil {
		t.Fatalf("claim d1 rollback: %v", err)
	}
	finishRollbackFailure(t, s, spec)
	finishDevice(t, s, spec, "d2", upBase.Add(10*time.Minute))
	cv := mustGetCampaign(s, spec.ID)
	if !cv.Ended || !cv.EndedAt.After(rbFailAt(upBase)) {
		t.Fatalf("precondition: campaign ends after d1 rollback failure, %+v", cv)
	}
	dir := closeStore(t, s)

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("campaign ending later than the rollback failure must reopen: %v", err)
	}
	defer s2.Close()
	cv2 := mustGetCampaign(s2, spec.ID)
	d1 := findDevice(cv2, "d1")
	if d1.Status != DeviceRollbackFailed || d1.Reason != "rb boom" ||
		!d1.At.Equal(rbFailAt(upBase)) {
		t.Fatalf("original rollback failure info must be preserved: %+v", d1)
	}
	if !cv2.EndedAt.Equal(cv.EndedAt) {
		t.Fatalf("campaign end time changed: %v want %v", cv2.EndedAt, cv.EndedAt)
	}
	if d2 := findDevice(cv2, "d2"); d2.Status != DeviceSucceeded {
		t.Fatalf("d2 success preserved: %+v", d2)
	}
}

// TestRestoreRollbackFailedShadowUntouched 回滚失败不附带、不应用上报：恢复后
// 设备影子保持安装失败前的版本与双方配置，普通上报流程也不因回滚失败记录而
// 改变（沿用已有公开行为）。
func TestRestoreRollbackFailedShadowUntouched(t *testing.T) {
	dir, spec := setupRollbackFailedClosed(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if w, _ := s.GetDeviceWork("d1"); w.CampaignID != "" || w.Pending != nil {
		t.Fatalf("no pending work after rollback failure: %+v", w)
	}
	// 活动已结束，普通上报仍按既有规则接受并更新影子。
	if err := s.Report("d1", 2, upBase.Add(30*time.Minute), "v1", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatalf("plain report after rollback failure: %v", err)
	}
	sh, _ := s.Get("d1")
	if sh.Version != "v1" || sh.LastSeq != 2 {
		t.Fatalf("shadow follows plain report rules: %+v", sh)
	}
	d := findDevice(mustGetCampaign(s, spec.ID), "d1")
	if d.Status != DeviceRollbackFailed || d.Reason != "rb boom" {
		t.Fatalf("rollback failure record untouched by plain report: %+v", d)
	}
}
