package shadow

import (
	"encoding/json"
	"testing"
	"time"
)

// clearInstallResult 把保存记录中指定设备的安装操作改为“已领取、尚无结果”，
// 并同步删除安装结果对应的历史条目，使记录除缺少已接受的安装失败外自洽。
func clearInstallResult(doc map[string]any, campaignID, deviceID string) {
	in := campaignDeviceDoc(doc, campaignID, deviceID)["install"].(map[string]any)
	in["hasResult"] = false
	delete(in, "reason")
	delete(in, "at")
	dropStageResult(doc, campaignID, deviceID, StageInstall)
}

// dropStageResult 删除结果历史中指定设备、指定阶段的条目。
func dropStageResult(doc map[string]any, campaignID, deviceID, stage string) {
	c := campaignDoc(doc, campaignID)
	raw, _ := c["results"].([]any)
	keep := make([]any, 0, len(raw))
	for _, r := range raw {
		e := r.(map[string]any)
		if e["deviceId"] == deviceID && e["stage"] == stage {
			continue
		}
		keep = append(keep, r)
	}
	c["results"] = keep
}

// setupRollingBack 创建开启回滚的单设备活动并推进到正在回滚
// （安装失败已接受、回滚已领取未完成），返回未关闭的存储与安装失败时间。
func setupRollingBack(t *testing.T) (*Store, CampaignSpec, time.Time) {
	t.Helper()
	s, spec := setupRollbackCampaign(t)
	_, failAt := awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute)); err != nil {
		t.Fatalf("claim rollback: %v", err)
	}
	if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceRollingBack {
		t.Fatalf("precondition: %+v", d)
	}
	return s, spec, failAt
}

// TestRestoreRollbackRequiresInstallFailure 保护“接受安装失败后才进入回滚”的
// 规则在恢复保存进度时同样成立：开启回滚的活动中，设备处于等待回滚、正在回滚、
// 回滚成功、回滚失败或回滚超时，都必须能从该设备在同一活动中已接受的安装结果
// 确认安装失败。安装尚未领取、已领取但没有结果、或安装结果为成功，都不能支持
// 这些回滚状态——即使回滚目标非空、操作标识合法、回滚结果与历史一致或活动已经
// 结束；其他设备或其他活动的失败记录也不能代替本次安装失败。发现矛盾时 Open
// 必须返回 ErrCorruptStorage，整个存储拒绝打开且原文件内容保持不变。
func TestRestoreRollbackRequiresInstallFailure(t *testing.T) {
	// 等待回滚，但安装已领取而无结果。
	t.Run("awaiting rollback without install result", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
		awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
		if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceAwaitingRollback {
			t.Fatalf("precondition: %+v", d)
		}
		dir := closeStore(t, s)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			clearInstallResult(doc, spec.ID, "d1")
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 正在回滚，但安装尚未领取。
	t.Run("rolling back with install unclaimed", func(t *testing.T) {
		s, spec, _ := setupRollingBack(t)
		dir := closeStore(t, s)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			in := campaignDeviceDoc(doc, spec.ID, "d1")["install"].(map[string]any)
			in["claimed"] = false
			delete(in, "claimedAt")
			clearInstallResult(doc, spec.ID, "d1")
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 正在回滚，但安装已领取而无结果。
	t.Run("rolling back without install result", func(t *testing.T) {
		s, spec, _ := setupRollingBack(t)
		dir := closeStore(t, s)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			clearInstallResult(doc, spec.ID, "d1")
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 正在回滚，但安装结果为成功：回滚目标非空、操作标识合法、影子与附带
	// 上报一致都不能代替本次安装失败。
	t.Run("rolling back with install succeeded", func(t *testing.T) {
		s, spec, failAt := setupRollingBack(t)
		dir := closeStore(t, s)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			in := campaignDeviceDoc(doc, spec.ID, "d1")["install"].(map[string]any)
			in["success"] = true
			delete(in, "reason")
			in["resultVersion"] = "v2"
			in["resultConfig"] = map[string]any{}
			in["resultSeq"] = 1
			// 影子同步改为安装成功上报后的样子：版本 v2、最近上报时间为
			// 安装结果时间，使附带上报核对无法发现矛盾。
			ds := doc["devices"].(map[string]any)["d1"].(map[string]any)
			ds["version"] = "v2"
			ds["lastReportTime"] = failAt.Format(time.RFC3339Nano)
			// 历史中的安装记录同步改为成功。
			for _, e := range resultDocs(doc, spec.ID) {
				if e["deviceId"] == "d1" && e["stage"] == StageInstall {
					e["success"] = true
					delete(e, "reason")
					e["version"] = "v2"
				}
			}
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 回滚成功、活动已结束，但安装没有失败结果：活动结束与回滚结果自洽
	// 都不能取消安装失败这一前提。
	t.Run("rollback succeeded without install failure", func(t *testing.T) {
		s, spec, _ := setupRollingBack(t)
		rb, err := s.Claim(spec.ID, "d1", upBase.Add(9*time.Minute))
		if err != nil {
			t.Fatalf("re-claim rollback: %v", err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(10 * time.Minute), Success: true,
			Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("rollback result: %v", err)
		}
		if v := mustGetCampaign(s, spec.ID); !v.Ended ||
			findDevice(v, "d1").Status != DeviceRollbackSucceeded {
			t.Fatalf("precondition: %+v", v)
		}
		dir := closeStore(t, s)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			clearInstallResult(doc, spec.ID, "d1")
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 回滚失败，但安装没有失败结果。
	t.Run("rollback failed without install failure", func(t *testing.T) {
		s, spec, _ := setupRollingBack(t)
		rb, err := s.Claim(spec.ID, "d1", upBase.Add(9*time.Minute))
		if err != nil {
			t.Fatalf("re-claim rollback: %v", err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(10 * time.Minute), Success: false, Reason: "rollback broken",
		}); err != nil {
			t.Fatalf("rollback result: %v", err)
		}
		if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceRollbackFailed {
			t.Fatalf("precondition: %+v", d)
		}
		dir := closeStore(t, s)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			clearInstallResult(doc, spec.ID, "d1")
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 等待回滚超时（回滚从未领取），但安装没有失败结果：没有回滚领取和
	// 结果属正常，缺少安装失败不是。
	t.Run("awaiting rollback timeout without install failure", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
		awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceRollbackTimeout {
			t.Fatalf("precondition: %+v", d)
		}
		dir := closeStore(t, s)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			clearInstallResult(doc, spec.ID, "d1")
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 正在回滚超时（已领取未完成），但安装没有失败结果。
	t.Run("rolling back timeout without install failure", func(t *testing.T) {
		s, spec, _ := setupRollingBack(t)
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceRollbackTimeout {
			t.Fatalf("precondition: %+v", d)
		}
		dir := closeStore(t, s)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			clearInstallResult(doc, spec.ID, "d1")
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 同活动其他设备的安装失败不能代替本设备的安装失败；存在正常设备时
	// 整个存储仍必须拒绝打开。
	t.Run("other device failure does not count", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 2
		spec.RollbackOnFailure = true
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		// d1 的安装失败是合法的；d2 也推进到正在回滚后被抹去安装失败。
		awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
		awaitRollback(t, s, spec, "d2", upBase.Add(6*time.Minute))
		if _, err := s.Claim(spec.ID, "d2", upBase.Add(12*time.Minute)); err != nil {
			t.Fatalf("claim rollback: %v", err)
		}
		dir := closeStore(t, s)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			clearInstallResult(doc, spec.ID, "d2")
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 其他活动的安装失败不能代替本活动本设备的安装失败。
	t.Run("other campaign failure does not count", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec1 := rbSpec()
		createCampaign(t, s, spec1)
		spec2 := rbSpec()
		spec2.ID = "cmp-2"
		spec2.Devices = []string{"d2"}
		createCampaign(t, s, spec2)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		// cmp-1 的 d1 合法等待回滚；cmp-2 的 d2 正在回滚但被抹去安装失败。
		awaitRollback(t, s, spec1, "d1", upBase.Add(2*time.Minute))
		awaitRollback(t, s, spec2, "d2", upBase.Add(2*time.Minute))
		if _, err := s.Claim(spec2.ID, "d2", upBase.Add(8*time.Minute)); err != nil {
			t.Fatalf("claim rollback: %v", err)
		}
		dir := closeStore(t, s)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			clearInstallResult(doc, spec2.ID, "d2")
		})
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreRollbackLegitProgressReopen 保护合法回滚记录按原行为恢复：
// 安装失败后尚未领取回滚、已领取但未完成，以及回滚成功、失败或超时，都保留
// 原进度、锁定目标与安装失败的原因和时间；等待回滚超时可以没有回滚领取和
// 结果，正在回滚超时可以没有回滚结果。尚未进入回滚流程的正常升级记录，以及
// 未开启回滚的旧活动，不因没有安装失败而被拒绝。
func TestRestoreRollbackLegitProgressReopen(t *testing.T) {
	// 等待回滚：安装失败的原因与时间保留；失败后普通上报改变当前版本，
	// 不否定此前已接受的安装失败，回滚待办仍指向锁定目标。
	t.Run("awaiting rollback keeps install failure", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
		_, failAt := awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
		if err := s.Report("d1", 2, upBase.Add(6*time.Minute), "v9", json.RawMessage(`{"x":1}`)); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceAwaitingRollback || d.RollbackTarget != "v1" ||
			d.InstallFailReason != "install broken" || !d.InstallFailAt.Equal(failAt) {
			t.Fatalf("awaiting rollback after reopen: %+v", d)
		}
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending == nil || w.Pending.Kind != StageRollback || w.Pending.TargetVersion != "v1" {
			t.Fatalf("rollback todo after reopen: %+v", w)
		}
		if sh, _ := s2.Get("d1"); sh.Version != "v9" {
			t.Fatalf("shadow version after reopen: %s", sh.Version)
		}
	})

	// 正在回滚：已领取未完成，回滚结果可以没有，进度与安装失败保留。
	t.Run("rolling back reopens", func(t *testing.T) {
		s, spec, failAt := setupRollingBack(t)
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceRollingBack || d.RollbackTarget != "v1" ||
			d.RollbackResult || !d.InstallFailAt.Equal(failAt) {
			t.Fatalf("rolling back after reopen: %+v", d)
		}
	})

	// 回滚成功：终态与回滚结果保留，活动以失败结束不变。
	t.Run("rollback succeeded reopens", func(t *testing.T) {
		s, spec, _ := setupRollingBack(t)
		rb, err := s.Claim(spec.ID, "d1", upBase.Add(9*time.Minute))
		if err != nil {
			t.Fatalf("re-claim rollback: %v", err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(10 * time.Minute), Success: true,
			Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("rollback result: %v", err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		v := mustGetCampaign(s2, spec.ID)
		d := findDevice(v, "d1")
		if d.Status != DeviceRollbackSucceeded || !d.RollbackResult || !d.RollbackSuccess ||
			d.RollbackTarget != "v1" || !v.Ended || v.Status != CampaignFailed {
			t.Fatalf("rollback succeeded after reopen: %+v %+v", d, v)
		}
	})

	// 回滚失败：终态与失败原因保留。
	t.Run("rollback failed reopens", func(t *testing.T) {
		s, spec, _ := setupRollingBack(t)
		rb, err := s.Claim(spec.ID, "d1", upBase.Add(9*time.Minute))
		if err != nil {
			t.Fatalf("re-claim rollback: %v", err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(10 * time.Minute), Success: false, Reason: "rollback broken",
		}); err != nil {
			t.Fatalf("rollback result: %v", err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceRollbackFailed || !d.RollbackResult || d.RollbackSuccess ||
			d.RollbackReason != "rollback broken" {
			t.Fatalf("rollback failed after reopen: %+v", d)
		}
	})

	// 等待回滚超时：没有回滚领取和结果属正常，照常打开。
	t.Run("awaiting rollback timeout reopens", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
		_, failAt := awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceRollbackTimeout || d.RollbackResult ||
			!d.InstallFailAt.Equal(failAt) {
			t.Fatalf("rollback timeout after reopen: %+v", d)
		}
	})

	// 正在回滚超时：已领取但没有回滚结果属正常，照常打开。
	t.Run("rolling back timeout reopens", func(t *testing.T) {
		s, spec, failAt := setupRollingBack(t)
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(2*time.Hour)); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceRollbackTimeout || d.RollbackResult ||
			!d.InstallFailAt.Equal(failAt) {
			t.Fatalf("rollback timeout after reopen: %+v", d)
		}
	})

	// 开启回滚的活动中，下载阶段失败的设备没有安装失败属正常。
	t.Run("download failure without install failure reopens", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
		dl, err := s.Claim(spec.ID, "d1", upBase)
		if err != nil {
			t.Fatalf("claim download: %v", err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(time.Minute), Success: false, Reason: "network down",
		}); err != nil {
			t.Fatalf("download result: %v", err)
		}
		if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceFailed {
			t.Fatalf("precondition: %+v", d)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		if d := findDevice(mustGetCampaign(s2, spec.ID), "d1"); d.Status != DeviceFailed ||
			d.Phase != StageDownload {
			t.Fatalf("download failure after reopen: %+v", d)
		}
	})

	// 未开启回滚的旧活动：成功的设备没有安装失败，照常打开。
	t.Run("legacy non-rollback campaign reopens", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		finishDevice(t, s, spec, "d1", upBase)
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		v := mustGetCampaign(s2, spec.ID)
		if d := findDevice(v, "d1"); d.Status != DeviceSucceeded || !v.Ended ||
			v.Status != CampaignSucceeded {
			t.Fatalf("legacy campaign after reopen: %+v", v)
		}
	})
}
