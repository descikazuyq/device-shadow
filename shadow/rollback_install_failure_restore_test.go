package shadow

import (
	"encoding/json"
	"testing"
	"time"
)

// 本文件保护“接受安装失败后才进入回滚”这一前提在打开本地存储时同样生效：
// 开启安装失败回滚的活动中，只要某台设备处于等待回滚、正在回滚、回滚成功、
// 回滚失败或回滚超时，就必须能从该设备在同一活动中已经接受的安装结果确认
// 安装失败。安装尚未领取、已领取但没有结果，或安装结果为成功，都不能支持
// 这些回滚状态；下载失败、其他设备或其他活动的失败记录也不能代替本次安装
// 失败。发现矛盾时 Open 返回 ErrCorruptStorage，整个存储不得进入可用状态，
// 原文件内容保持不变。

// installFailAt 是测试时间线中安装失败被接受的时刻：
// 领取下载 +2m、下载成功 +3m、领取安装 +4m、安装失败 +5m。
func installFailAt(base time.Time) time.Time { return base.Add(5 * time.Minute) }

// buildRollingBase 构造一台处于“正在回滚”的设备：完成下载、安装失败被接受、
// 再领取回滚但不完成。withReport 时在等待回滚期间插入一条 seq=2 的普通上报
// （版本 v9、空配置），用于让伪造的安装成功结果拥有更小序号、不与当前影子
// 对内容；普通上报不改变已接受的安装失败与回滚待办。
func buildRollingBase(t *testing.T, withReport bool) (*Store, CampaignSpec) {
	t.Helper()
	s := setupUpgrade(t, "d1")
	spec := rbSpec()
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	if withReport {
		if err := s.Report("d1", 2, upBase.Add(6*time.Minute), "v9", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("plain report: %v", err)
		}
	}
	rb, err := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute))
	if err != nil || rb == nil || rb.Kind != StageRollback {
		t.Fatalf("claim rollback: %v %+v", err, rb)
	}
	return s, spec
}

// finishRollbackSuccess 让正在回滚的设备回滚成功。seq 随是否已有 seq=2 的
// 普通上报取 3 或 2；附带上报配置引入 /ok 差异，影子由运行时正常维护。
func finishRollbackSuccess(t *testing.T, s *Store, spec CampaignSpec, seq uint64) {
	t.Helper()
	rb := findDevice(mustGetCampaign(s, spec.ID), "d1").RollbackID
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb,
		At: upBase.Add(90 * time.Minute), Success: true,
		Seq: seq, Version: "v1", Config: json.RawMessage(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("rollback success: %v", err)
	}
}

// finishRollbackFailure 让正在回滚的设备回滚失败。
func finishRollbackFailure(t *testing.T, s *Store, spec CampaignSpec) {
	t.Helper()
	rb := findDevice(mustGetCampaign(s, spec.ID), "d1").RollbackID
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb,
		At: upBase.Add(9 * time.Minute), Success: false, Reason: "rb boom",
	}); err != nil {
		t.Fatalf("rollback failure: %v", err)
	}
}

// timeoutRollingBase 把正在回滚的活动推进到截止，设备以回滚阶段超时结束。
func timeoutRollingBase(t *testing.T, s *Store, spec CampaignSpec) {
	t.Helper()
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(2*time.Hour)); err != nil {
		t.Fatalf("advance to deadline: %v", err)
	}
}

// installOpDoc / replaceInstallHistory 定位磁盘上的安装操作与安装结果历史。
func installOpDoc(doc map[string]any, campaignID, deviceID string) map[string]any {
	return campaignDeviceDoc(doc, campaignID, deviceID)["install"].(map[string]any)
}

// removeInstallHistory 删除该设备保存的安装结果历史条目，返回被删条目是否存在。
func removeInstallHistory(doc map[string]any, campaignID, deviceID string) bool {
	camp := campaignDoc(doc, campaignID)
	raw, _ := camp["results"].([]any)
	kept := make([]any, 0, len(raw))
	found := false
	for _, r := range raw {
		rec := r.(map[string]any)
		if rec["deviceId"] == deviceID && rec["stage"] == StageInstall {
			found = true
			continue
		}
		kept = append(kept, rec)
	}
	camp["results"] = kept
	return found
}

// setInstallNoResult 把安装操作改写为“已领取但没有结果”（claimed=true 时）
// 或“尚未领取”（claimed=false 时），并清掉全部结果字段；安装失败历史同步删除。
// 改写后除“缺少本设备本次安装失败”外的其余记录保持自洽。
func setInstallNoResult(doc map[string]any, campaignID, deviceID string, claimed bool) {
	inst := installOpDoc(doc, campaignID, deviceID)
	inst["claimed"] = claimed
	if claimed {
		if _, ok := inst["claimedAt"]; !ok {
			inst["claimedAt"] = "2026-10-02T12:04:00Z"
		}
	} else {
		delete(inst, "claimedAt")
	}
	inst["hasResult"] = false
	for _, k := range []string{"success", "reason", "at", "resultSeq", "resultVersion", "resultConfig"} {
		delete(inst, k)
	}
	if !removeInstallHistory(doc, campaignID, deviceID) {
		panic("test setup: install history missing")
	}
}

// setInstallSuccess 把安装操作改写为一条内容自洽的“安装成功”已接受结果
// （seq=1，版本等于活动目标 v2，空对象配置），历史条目同步改为安装成功。
// 当设备最近已接受更大序号（seq=2 的普通上报或回滚上报）时，这条 seq=1 的
// 成功记录属于被后续上报覆盖的历史，恢复核对不要求它与当前影子一致，因此
// 除“安装成功不能支持回滚状态”外记录完全合法。
func setInstallSuccess(doc map[string]any, campaignID, deviceID string) {
	inst := installOpDoc(doc, campaignID, deviceID)
	inst["claimed"] = true
	if _, ok := inst["claimedAt"]; !ok {
		inst["claimedAt"] = "2026-10-02T12:04:00Z"
	}
	inst["hasResult"] = true
	inst["success"] = true
	delete(inst, "reason")
	if _, ok := inst["at"]; !ok {
		inst["at"] = "2026-10-02T12:05:00Z"
	}
	inst["resultSeq"] = 1
	inst["resultVersion"] = "v2"
	inst["resultConfig"] = map[string]any{}
	camp := campaignDoc(doc, campaignID)
	raw, _ := camp["results"].([]any)
	replaced := false
	for _, r := range raw {
		rec := r.(map[string]any)
		if rec["deviceId"] == deviceID && rec["stage"] == StageInstall {
			rec["success"] = true
			delete(rec, "reason")
			rec["version"] = "v2"
			replaced = true
		}
	}
	if !replaced {
		panic("test setup: install history missing")
	}
}

// setDownloadFailure 把成功下载改写为下载失败（带原因），安装改为尚未领取、
// 安装失败历史删除，用来证明下载失败不能代替本次安装失败。回滚目标仍保留。
func setDownloadFailure(doc map[string]any, campaignID, deviceID string) {
	dev := campaignDeviceDoc(doc, campaignID, deviceID)
	dl := dev["download"].(map[string]any)
	dl["hasResult"] = true
	dl["success"] = false
	dl["reason"] = "dl down"
	if _, ok := dl["at"]; !ok {
		dl["at"] = dl["claimedAt"]
	}
	inst := dev["install"].(map[string]any)
	inst["claimed"] = false
	delete(inst, "claimedAt")
	inst["hasResult"] = false
	for _, k := range []string{"success", "reason", "at", "resultSeq", "resultVersion", "resultConfig"} {
		delete(inst, k)
	}
	removeInstallHistory(doc, campaignID, deviceID)
	camp := campaignDoc(doc, campaignID)
	raw, _ := camp["results"].([]any)
	for _, r := range raw {
		rec := r.(map[string]any)
		if rec["deviceId"] == deviceID && rec["stage"] == StageDownload {
			rec["success"] = false
			rec["reason"] = "dl down"
			delete(rec, "version")
		}
	}
}

// mutateCase 描述一种“抹掉本设备本次安装失败前提”的磁盘改写。
type mutateCase struct {
	name   string
	mutate func(doc map[string]any, campaignID, deviceID string)
}

func rollbackPremiseCases() []mutateCase {
	return []mutateCase{
		{
			name:   "install never claimed",
			mutate: func(doc map[string]any, id, dev string) { setInstallNoResult(doc, id, dev, false) },
		},
		{
			name:   "install claimed without result",
			mutate: func(doc map[string]any, id, dev string) { setInstallNoResult(doc, id, dev, true) },
		},
		{
			name:   "install result is success",
			mutate: func(doc map[string]any, id, dev string) { setInstallSuccess(doc, id, dev) },
		},
		{
			name:   "download failure instead of install failure",
			mutate: func(doc map[string]any, id, dev string) { setDownloadFailure(doc, id, dev) },
		},
	}
}

// TestRestoreRollbackRequiresInstallFailure 保护核心修复：正在回滚与回滚终态
// 此前缺少“已接受本次安装失败”的核对，安装尚未领取、已领取但没有结果、安装
// 成功，或只有下载失败的设备也能被恢复成回滚状态、查到回滚待办。这些自洽
// （回滚目标非空、操作标识合法、回滚结果与历史一致）的矛盾记录现在都必须在
// Open 时整体拒绝。
func TestRestoreRollbackRequiresInstallFailure(t *testing.T) {
	// statusBases 给出每种“修复前未核对”的回滚状态及其磁盘构造方式。
	// 伪造安装成功/下载失败等情形依赖设备最近已接受 seq=2 的上报，
	// 因此基座统一带一条等待回滚期间的普通上报（不否定安装失败）。
	type base struct {
		status string
		build  func(t *testing.T) (*Store, CampaignSpec)
	}
	bases := []base{
		{DeviceRollingBack, func(t *testing.T) (*Store, CampaignSpec) {
			return buildRollingBase(t, true)
		}},
		{DeviceRollbackSucceeded, func(t *testing.T) (*Store, CampaignSpec) {
			s, spec := buildRollingBase(t, true)
			finishRollbackSuccess(t, s, spec, 3)
			return s, spec
		}},
		{DeviceRollbackFailed, func(t *testing.T) (*Store, CampaignSpec) {
			s, spec := buildRollingBase(t, true)
			finishRollbackFailure(t, s, spec)
			return s, spec
		}},
		{DeviceRollbackTimeout, func(t *testing.T) (*Store, CampaignSpec) {
			s, spec := buildRollingBase(t, true)
			timeoutRollingBase(t, s, spec)
			return s, spec
		}},
	}
	for _, b := range bases {
		for _, mc := range rollbackPremiseCases() {
			name := b.status + "/" + mc.name
			t.Run(name, func(t *testing.T) {
				s, spec := b.build(t)
				if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != b.status {
					t.Fatalf("precondition status: got %s want %s", d.Status, b.status)
				}
				dir := closeStore(t, s)
				original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
					mc.mutate(doc, spec.ID, "d1")
				})
				assertReopenCorrupt(t, dir, original)
			})
		}
	}
}

// TestRestoreAwaitingRollbackTimeoutRequiresInstallFailure 覆盖等待回滚超时
// （回滚未领取、无回滚结果）这一形态：它同样必须有已接受的安装失败，
// 不能因为没有回滚领取和结果就把缺少安装失败当成正常记录。
func TestRestoreAwaitingRollbackTimeoutRequiresInstallFailure(t *testing.T) {
	for _, mc := range rollbackPremiseCases() {
		t.Run(mc.name, func(t *testing.T) {
			s := setupUpgrade(t, "d1")
			spec := rbSpec()
			createCampaign(t, s, spec)
			bringOnline(t, s, "d1")
			awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
			if err := s.Report("d1", 2, upBase.Add(6*time.Minute), "v9", json.RawMessage(`{}`)); err != nil {
				t.Fatalf("plain report: %v", err)
			}
			// 不领取回滚，直接推进到截止：等待回滚超时（回滚未领取、无结果）。
			if err := s.AdvanceCampaign(spec.ID, upBase.Add(2*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceRollbackTimeout {
				t.Fatalf("precondition: %+v", d)
			}
			dir := closeStore(t, s)
			original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
				mc.mutate(doc, spec.ID, "d1")
			})
			assertReopenCorrupt(t, dir, original)
		})
	}
}

// TestRestoreRollbackPremiseOtherFailuresDontCount 证明安装失败前提必须是
// “本设备、本活动、本次安装”的失败：同活动另一台设备的安装失败，以及另一个
// 正常活动里的回滚记录，都不能代替本设备本次安装失败；即使其余设备与活动
// 全部正常，整个存储仍必须拒绝打开，原文件保持不变。
func TestRestoreRollbackPremiseOtherFailuresDontCount(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2", "d3")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2"}
	spec.BatchSize = 2
	spec.RollbackOnFailure = true
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	// 两台设备都安装失败、进入回滚：d2 保留合法的安装失败记录。
	awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	awaitRollback(t, s, spec, "d2", upBase.Add(12*time.Minute))
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute)); err != nil {
		t.Fatalf("claim d1 rollback: %v", err)
	}

	// 另一个完全正常的回滚活动（d3），重开本应照常可用。
	other := rbSpec()
	other.ID = "cmp-other"
	other.Devices = []string{"d3"}
	createCampaign(t, s, other)
	bringOnline(t, s, "d3")
	inID, _ := awaitRollback(t, s, other, "d3", upBase.Add(22*time.Minute))
	if inID == "" {
		t.Fatal("other campaign setup")
	}

	dir := closeStore(t, s)
	original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
		// d1 的安装失败被抹成“已领取但没有结果”，d2 与 cmp-other 不动。
		setInstallNoResult(doc, spec.ID, "d1", true)
	})
	// d2、d3 的合法回滚记录不能让存储部分可用：整个存储必须原样拒绝。
	assertReopenCorrupt(t, dir, original)
}

// TestRestoreLegitRollbackStatesKeepProgress 保护合法回滚记录的恢复行为不变：
// 安装失败后等待回滚、正在回滚、回滚成功、回滚失败、等待回滚超时与正在回滚
// 超时都按原进度、锁定目标与安装失败原因/时间恢复；这些记录即使缺回滚领取
// 或回滚结果（超时形态）也照常打开。
func TestRestoreLegitRollbackStatesKeepProgress(t *testing.T) {
	failAt := installFailAt(upBase)

	t.Run("awaiting rollback", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := rbSpec()
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceAwaitingRollback || d.RollbackTarget != "v1" ||
			d.InstallFailReason != "install broken" || !d.InstallFailAt.Equal(failAt) {
			t.Fatalf("awaiting rollback restored: %+v", d)
		}
		w, _ := s2.GetDeviceWork("d1")
		if w.Pending == nil || w.Pending.Kind != StageRollback ||
			w.PendingClaimed || w.Pending.TargetVersion != "v1" {
			t.Fatalf("rollback todo restored: %+v", w)
		}
	})

	t.Run("rolling back", func(t *testing.T) {
		s, spec := buildRollingBase(t, false)
		rbBefore := findDevice(mustGetCampaign(s, spec.ID), "d1").RollbackID
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceRollingBack || d.RollbackTarget != "v1" ||
			d.RollbackID != rbBefore ||
			d.InstallFailReason != "install broken" || !d.InstallFailAt.Equal(failAt) {
			t.Fatalf("rolling back restored: %+v", d)
		}
		again, err := s2.Claim(spec.ID, "d1", upBase.Add(85*time.Minute))
		if err != nil || again == nil || again.ID != rbBefore {
			t.Fatalf("claimed rollback id stable: %v %+v", err, again)
		}
	})

	t.Run("rollback succeeded", func(t *testing.T) {
		s, spec := buildRollingBase(t, false)
		finishRollbackSuccess(t, s, spec, 2)
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceRollbackSucceeded || d.RollbackTarget != "v1" ||
			!d.RollbackResult || !d.RollbackSuccess ||
			d.InstallFailReason != "install broken" || !d.InstallFailAt.Equal(failAt) {
			t.Fatalf("rollback success restored: %+v", d)
		}
		if sh, _ := s2.Get("d1"); sh.Version != "v1" {
			t.Fatalf("shadow version restored: %s", sh.Version)
		}
	})

	t.Run("rollback failed", func(t *testing.T) {
		s, spec := buildRollingBase(t, false)
		finishRollbackFailure(t, s, spec)
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceRollbackFailed || d.RollbackReason != "rb boom" ||
			!d.RollbackAt.Equal(upBase.Add(9*time.Minute)) ||
			d.InstallFailReason != "install broken" || !d.InstallFailAt.Equal(failAt) {
			t.Fatalf("rollback failure restored: %+v", d)
		}
	})

	t.Run("awaiting rollback timeout", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := rbSpec()
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
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
		// 等待回滚超时：可以没有回滚领取和结果，但安装失败原因/时间保留。
		if d.Status != DeviceRollbackTimeout || d.RollbackResult ||
			d.RollbackID == "" || d.RollbackTarget != "v1" ||
			d.InstallFailReason != "install broken" || !d.InstallFailAt.Equal(failAt) {
			t.Fatalf("awaiting-timeout restored: %+v", d)
		}
	})

	t.Run("rolling back timeout", func(t *testing.T) {
		s, spec := buildRollingBase(t, false)
		timeoutRollingBase(t, s, spec)
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		// 正在回滚超时：已领取回滚但可以没有回滚结果，安装失败仍保留。
		if d.Status != DeviceRollbackTimeout || d.RollbackResult ||
			d.RollbackTarget != "v1" ||
			d.InstallFailReason != "install broken" || !d.InstallFailAt.Equal(failAt) {
			t.Fatalf("rolling-timeout restored: %+v", d)
		}
	})
}

// TestRestoreLaterPlainReportKeepsInstallFailure 保护：安装失败被接受之后的
// 普通上报即使改变当前版本，也不否定此前已接受的安装失败；携带这种后续上报
// 的回滚记录重开仍合法，回滚待办仍指向锁定目标。
func TestRestoreLaterPlainReportKeepsInstallFailure(t *testing.T) {
	s, spec := buildRollingBase(t, true)
	if sh, _ := s.Get("d1"); sh.Version != "v9" {
		t.Fatalf("precondition shadow: %+v", sh)
	}
	dir := closeStore(t, s)
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen with later plain report: %v", err)
	}
	defer s2.Close()
	d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
	if d.Status != DeviceRollingBack || d.RollbackTarget != "v1" ||
		d.InstallFailReason != "install broken" {
		t.Fatalf("rollback with later report restored: %+v", d)
	}
	w, _ := s2.GetDeviceWork("d1")
	if w.Pending == nil || w.Pending.Kind != StageRollback ||
		w.Pending.TargetVersion != "v1" || w.Version != "v9" {
		t.Fatalf("todo target stays locked while shadow moved: %+v", w)
	}
}
