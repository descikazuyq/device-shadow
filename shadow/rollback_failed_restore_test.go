package shadow

import (
	"encoding/json"
	"testing"
	"time"
)

// 本文件保护“rollback_failed 设备的当前失败说明必须锚定本设备在本活动中首次
// 接受的回滚失败结果”这一核对在打开本地存储时生效：开启安装失败回滚的活动中，
// 设备以 rollback_failed 结束时，其当前状态中的失败原因必须与该设备该活动首次
// 接受的回滚失败结果的原因逐字一致（多空格也算不同），状态时间必须与首次接受
// 该结果的时间表示同一实际时刻（时区写法不同但时刻相同合法）。即使回滚操作
// 结果与结果历史彼此自洽，把当前原因写成原安装失败原因或另一段非空文字、把
// 当前时间挪回安装失败或其他时刻，都必须返回 errors.Is 可判断的
// ErrCorruptStorage，整个存储拒绝打开、已有文件内容保持原样；不能只跳过这台
// 设备，也不能改写原因、挪动时间或删去历史让文件打开。判断只锚定本设备本活动
// 的回滚失败：同批其他设备的失败、原安装失败以及设备当前影子版本都不能替代它。
// 该核对对活动仍在等待同批其他设备与活动已经结束两种情况同样生效。

// rollbackFailAt 是测试时间线中回滚失败被接受的时刻（buildRollingBase +
// finishRollbackFailure）：安装失败 12:05、领取回滚 12:08、回滚失败 12:09。
func rollbackFailAt(base time.Time) time.Time { return base.Add(9 * time.Minute) }

// setupRollbackFailedClosed 构造开启回滚的单设备活动：d1 安装失败后回滚、
// 回滚失败被接受，设备以 rollback_failed 结束、活动失败结束，随后关闭存储
// 返回目录。
func setupRollbackFailedClosed(t *testing.T) (string, CampaignSpec) {
	t.Helper()
	s, spec := buildRollingBase(t, false)
	finishRollbackFailure(t, s, spec)
	if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceRollbackFailed {
		t.Fatalf("precondition status: %+v", d)
	}
	return closeStore(t, s), spec
}

// TestRestoreRollbackFailedMatchesAcceptedResult 保护核心修复：rollback_failed
// 设备的当前失败原因与时间此前不与已接受的回滚失败结果对账，原因被换成另一段
// 非空文字（含原安装失败原因）或时间被挪回安装失败时刻都能打开存储，活动查询
// 同时显示互相矛盾的当前失败说明与回滚结果。这些矛盾记录现在都必须在 Open 时
// 整体拒绝；只改原因或只改时间同样不能接受。
func TestRestoreRollbackFailedMatchesAcceptedResult(t *testing.T) {
	rbAt := rollbackFailAt(upBase)     // 12:09
	installAt := installFailAt(upBase) // 12:05

	// 合法记录（活动已结束）照常打开：当前失败说明、回滚结果、安装失败说明与
	// 结果历史全部保留原值。
	t.Run("legit record reopens", func(t *testing.T) {
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
		if !d.RollbackResult || d.RollbackSuccess ||
			d.RollbackReason != "rb boom" || !d.RollbackAt.Equal(rbAt) {
			t.Fatalf("rollback result view altered: %+v", d)
		}
		if d.InstallFailReason != "install broken" || !d.InstallFailAt.Equal(installAt) {
			t.Fatalf("install failure info altered: %+v", d)
		}
		// 结果历史保留安装失败与回滚失败两条记录，原因与时间原样。
		if len(cv.Results) != 3 {
			t.Fatalf("result history altered: %+v", cv.Results)
		}
		last := cv.Results[len(cv.Results)-1]
		if last.Stage != StageRollback || last.Success ||
			last.Reason != "rb boom" || !last.At.Equal(rbAt) {
			t.Fatalf("rollback failure history altered: %+v", last)
		}
	})

	// 当前原因被换成原安装失败原因：回滚操作结果与历史仍都是“rb boom”，
	// 设备状态却写回安装失败原因，必须拒绝。
	t.Run("reason replaced with install failure reason", func(t *testing.T) {
		dir, spec := setupRollbackFailedClosed(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["reason"] = "install broken"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 当前原因被换成另一段非空文字。
	t.Run("reason replaced with other text", func(t *testing.T) {
		dir, spec := setupRollbackFailedClosed(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["reason"] = "image verify failed"
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

	// 状态时间被挪回安装失败时刻（12:05）：只改时间同样不能接受。
	t.Run("time moved back to install failure", func(t *testing.T) {
		dir, spec := setupRollbackFailedClosed(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["at"] = "2026-10-02T12:05:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 状态时间被挪到另一时刻（12:08，领取回滚的时刻）：不晚于活动时间基线，
	// 只有与回滚失败结果时间的对账能发现它。
	t.Run("time moved to another moment", func(t *testing.T) {
		dir, spec := setupRollbackFailedClosed(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["at"] = "2026-10-02T12:08:00Z"
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
			t.Fatalf("rollback failure not restored as same instant: %+v", d)
		}
	})
}

// TestRestoreRollbackFailedWhileCampaignRunning 保护：活动仍在等待同批其他
// 设备时，rollback_failed 设备的合法记录照常打开、活动继续执行；把它的当前
// 原因或时间改成与已接受回滚失败不一致，同样必须整体拒绝。
func TestRestoreRollbackFailedWhileCampaignRunning(t *testing.T) {
	setup := func(t *testing.T) (string, CampaignSpec) {
		t.Helper()
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"} // 同批，批大小 2
		spec.RollbackOnFailure = true
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
		rb, err := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute))
		if err != nil || rb == nil {
			t.Fatalf("claim rollback: %v %+v", err, rb)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(9 * time.Minute), Success: false, Reason: "rb boom",
		}); err != nil {
			t.Fatalf("rollback failure: %v", err)
		}
		// d2 尚未领取，活动仍在执行。
		if cv := mustGetCampaign(s, spec.ID); cv.Ended {
			t.Fatalf("precondition: campaign must still run, %+v", cv)
		}
		return closeStore(t, s), spec
	}

	t.Run("legit record reopens while campaign running", func(t *testing.T) {
		dir, spec := setup(t)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("legit rollback failure must reopen: %v", err)
		}
		defer s.Close()
		cv := mustGetCampaign(s, spec.ID)
		if cv.Ended || cv.Status != CampaignRunning {
			t.Fatalf("campaign must still run: %+v", cv)
		}
		d := findDevice(cv, "d1")
		if d.Status != DeviceRollbackFailed || d.Reason != "rb boom" ||
			!d.At.Equal(upBase.Add(9*time.Minute)) {
			t.Fatalf("rollback failure view altered: %+v", d)
		}
		if d2 := findDevice(cv, "d2"); d2.Status != DevicePending {
			t.Fatalf("batchmate must stay pending: %+v", d2)
		}
	})

	t.Run("reason mismatch refused while campaign running", func(t *testing.T) {
		dir, spec := setup(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["reason"] = "install broken"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("time mismatch refused while campaign running", func(t *testing.T) {
		dir, spec := setup(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["at"] = "2026-10-02T12:05:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreRollbackFailedAnchoredToOwnResult 证明当前失败说明只能由本设备在
// 本活动中的回滚失败锚定：同批其他设备真实的回滚失败原因与时间不能替代它；
// 同批其他设备后来才结束、活动结束时间晚于本设备回滚失败时间则不构成矛盾，
// 原回滚失败原因与时间必须原样保留。
func TestRestoreRollbackFailedAnchoredToOwnResult(t *testing.T) {
	// 两台设备同批，先后安装失败并各自回滚失败，原因与时间各不相同。
	setupTwoFailures := func(t *testing.T) (string, CampaignSpec) {
		t.Helper()
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.RollbackOnFailure = true
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
		awaitRollback(t, s, spec, "d2", upBase.Add(12*time.Minute))
		rb1, err := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute))
		if err != nil || rb1 == nil {
			t.Fatalf("claim d1 rollback: %v %+v", err, rb1)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb1.ID,
			At: upBase.Add(21 * time.Minute), Success: false, Reason: "d1 rb boom",
		}); err != nil {
			t.Fatalf("d1 rollback failure: %v", err)
		}
		rb2, err := s.Claim(spec.ID, "d2", upBase.Add(22*time.Minute))
		if err != nil || rb2 == nil {
			t.Fatalf("claim d2 rollback: %v %+v", err, rb2)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d2", OperationID: rb2.ID,
			At: upBase.Add(23 * time.Minute), Success: false, Reason: "d2 rb boom",
		}); err != nil {
			t.Fatalf("d2 rollback failure: %v", err)
		}
		return closeStore(t, s), spec
	}

	// d1 的当前原因被改成同批 d2 的真实回滚失败原因（d1 自己的回滚结果与
	// 历史保持原样）：即使该原因在同活动中真实存在，也不属于 d1，必须拒绝。
	t.Run("batchmate rollback reason cannot anchor this device", func(t *testing.T) {
		dir, spec := setupTwoFailures(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["reason"] = "d2 rb boom"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// d1 的当前时间被改成 d2 的回滚失败时间：同样必须拒绝。
	t.Run("batchmate rollback time cannot anchor this device", func(t *testing.T) {
		dir, spec := setupTwoFailures(t)
		original := mutateDeviceDoc(t, dir, spec.ID, "d1", func(d map[string]any) {
			d["at"] = "2026-10-02T12:23:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})

	// d1 在 12:09 回滚失败，同批 d2 继续处理并在 12:20 才成功，活动随之在
	// 12:20 失败结束（晚于 d1 的回滚失败时间）：合法，重开后 d1 仍保留
	// 12:09 的原回滚失败原因与时间，不能被活动结束时间覆盖或挪动。
	t.Run("campaign ends later without touching earlier rollback failure", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.RollbackOnFailure = true
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
		rb, err := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute))
		if err != nil || rb == nil {
			t.Fatalf("claim rollback: %v %+v", err, rb)
		}
		rbFailAt := upBase.Add(9 * time.Minute)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: rbFailAt, Success: false, Reason: "rb boom",
		}); err != nil {
			t.Fatalf("rollback failure: %v", err)
		}
		// 同批其他设备仍可继续：d2 完整成功，活动因 d1 回滚失败而失败结束。
		finishDevice(t, s, spec, "d2", upBase.Add(18*time.Minute))
		cv := mustGetCampaign(s, spec.ID)
		if !cv.Ended || cv.Status != CampaignFailed || !cv.EndedAt.After(rbFailAt) {
			t.Fatalf("precondition: campaign ends after d1 rollback failure, %+v", cv)
		}
		dir := closeStore(t, s)

		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("campaign ending later than a rollback failure must reopen: %v", err)
		}
		defer s2.Close()
		cv2 := mustGetCampaign(s2, spec.ID)
		d1 := findDevice(cv2, "d1")
		if d1.Status != DeviceRollbackFailed || d1.Reason != "rb boom" ||
			!d1.At.Equal(rbFailAt) {
			t.Fatalf("original rollback failure info must be preserved: %+v", d1)
		}
		if !cv2.EndedAt.Equal(cv.EndedAt) {
			t.Fatalf("campaign end time changed: %v want %v", cv2.EndedAt, cv.EndedAt)
		}
		if d2 := findDevice(cv2, "d2"); d2.Status != DeviceSucceeded {
			t.Fatalf("batchmate success must be preserved: %+v", d2)
		}
	})
}

// TestRestoreRollbackFailedKeepsShadowAndHistory 保护合法记录恢复后的完整视图：
// 设备影子版本、期望与上报配置、修订号和审计保持原值，活动查询保留原安装失败
// 说明、回滚失败结果与结果历史，已接受结果的重复提交在重开后仍然有效。
func TestRestoreRollbackFailedKeepsShadowAndHistory(t *testing.T) {
	s := setupUpgrade(t, "d1")
	// 回滚失败前留下期望配置与审计记录；回滚失败不得改写它们。
	if _, err := s.UpdateDesired("d1", "op", upBase.Add(-2*time.Minute), 0, json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatalf("update desired: %v", err)
	}
	spec := rbSpec()
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb, err := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute))
	if err != nil || rb == nil {
		t.Fatalf("claim rollback: %v %+v", err, rb)
	}
	rbAt := upBase.Add(9 * time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: rbAt, Success: false, Reason: "rb boom",
	}); err != nil {
		t.Fatalf("rollback failure: %v", err)
	}
	dir := closeStore(t, s)

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("legit rollback failure must reopen: %v", err)
	}
	defer s2.Close()

	// 影子保持回滚失败前的样子：版本仍是锁定的旧版本，双方配置、修订号、
	// 审计原样保留（回滚失败不附带、不应用上报）。
	v, err := s2.Get("d1")
	if err != nil {
		t.Fatalf("get shadow: %v", err)
	}
	if v.Version != "v1" || v.Revision != 1 ||
		string(v.Desired) != `{"a":1}` || string(v.Reported) != `{}` {
		t.Fatalf("shadow altered by rollback failure restore: %+v", v)
	}
	audit, err := s2.Audit("d1")
	if err != nil || len(audit) != 1 || audit[0].Revision != 1 {
		t.Fatalf("audit altered: %+v %v", audit, err)
	}

	// 活动视图保留安装失败说明、回滚失败结果与完整历史。
	cv := mustGetCampaign(s2, spec.ID)
	d := findDevice(cv, "d1")
	if d.InstallFailReason != "install broken" ||
		!d.InstallFailAt.Equal(upBase.Add(5*time.Minute)) ||
		!d.RollbackResult || d.RollbackReason != "rb boom" || !d.RollbackAt.Equal(rbAt) {
		t.Fatalf("failure views altered: %+v", d)
	}
	if len(cv.Results) != 3 ||
		cv.Results[1].Stage != StageInstall || cv.Results[1].Reason != "install broken" ||
		cv.Results[2].Stage != StageRollback || cv.Results[2].Reason != "rb boom" {
		t.Fatalf("result history altered: %+v", cv.Results)
	}

	// 已接受的回滚失败结果在重开后重复提交仍然有效（幂等，不增加历史）。
	if err := s2.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: rbAt, Success: false, Reason: "rb boom",
	}); err != nil {
		t.Fatalf("idempotent resubmit after reopen: %v", err)
	}
	if got := len(mustGetCampaign(s2, spec.ID).Results); got != 3 {
		t.Fatalf("resubmit must not add history: %d", got)
	}
}
