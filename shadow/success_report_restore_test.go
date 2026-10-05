package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// installDoc/rollbackDoc 取设备磁盘记录上的安装/回滚操作对象。
func installDoc(dev map[string]any) map[string]any  { return dev["install"].(map[string]any) }
func rollbackDoc(dev map[string]any) map[string]any { return dev["rollback"].(map[string]any) }

// deviceDoc 取存储文档中某台设备的影子记录。
func deviceDoc(doc map[string]any, deviceID string) map[string]any {
	return doc["devices"].(map[string]any)[deviceID].(map[string]any)
}

// TestSuccessReportSeqMustBeAcceptedByShadow 每条安装/回滚成功记录保存的附带
// 上报序号必须是已被设备影子接受的正整数：序号缺失（零）或大于设备最近接受
// 的序号都必须拒绝打开——即使活动已经结束。
func TestSuccessReportSeqMustBeAcceptedByShadow(t *testing.T) {
	t.Run("install seq missing", func(t *testing.T) {
		_, dir, _ := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			dev := campaignDeviceDoc(doc, "cmp-1", "d1")
			delete(installDoc(dev), "resultSeq")
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("install seq zero", func(t *testing.T) {
		_, dir, _ := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			installDoc(campaignDeviceDoc(doc, "cmp-1", "d1"))["resultSeq"] = 0
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("install seq ahead of shadow", func(t *testing.T) {
		// 成功记录声称序号 5，设备影子最近只接受到 2：从未被影子接受。
		_, dir, _ := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			installDoc(campaignDeviceDoc(doc, "cmp-1", "d1"))["resultSeq"] = 5
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("device last seq zero", func(t *testing.T) {
		// 影子记录里没有任何已接受上报（lastSeq=0），安装成功记录却带序号 2。
		_, dir, _ := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "d1")["lastSeq"] = 0
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("rollback seq zero", func(t *testing.T) {
		_, dir, spec, _ := setupRollbackSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			rollbackDoc(campaignDeviceDoc(doc, spec.ID, "d1"))["resultSeq"] = 0
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("rollback seq ahead of shadow", func(t *testing.T) {
		_, dir, spec, _ := setupRollbackSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			rollbackDoc(campaignDeviceDoc(doc, spec.ID, "d1"))["resultSeq"] = 9
		})
		assertReopenCorrupt(t, dir, original)
	})
}

// TestSuccessReportEqualSeqMustMatchShadow 成功记录序号与设备最近序号相等时，
// 这条记录就是设备最近一次上报：版本、完整上报配置、首次接受时保存的发生
// 时间都必须与影子分别一致，任一不符都拒绝打开已结束的活动。
func TestSuccessReportEqualSeqMustMatchShadow(t *testing.T) {
	t.Run("shadow version drifted", func(t *testing.T) {
		_, dir, _ := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			// 只改影子版本：成功记录版本 v2 与影子 v9 矛盾。
			deviceDoc(doc, "d1")["version"] = "v9"
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("op config differs from shadow", func(t *testing.T) {
		_, dir, _ := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			installDoc(campaignDeviceDoc(doc, "cmp-1", "d1"))["resultConfig"] = `{"z":9}`
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("shadow report time differs", func(t *testing.T) {
		// 改影子的最近上报时间：同序号成功记录保存的发生时间与之不再是同一时刻。
		_, dir, _ := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "d1")["lastReportTime"] = "2026-10-02T12:09:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("rollback shadow version drifted", func(t *testing.T) {
		_, dir, _, _ := setupRollbackSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "d1")["version"] = "v9"
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("rollback op config differs from shadow", func(t *testing.T) {
		_, dir, spec, _ := setupRollbackSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			rollbackDoc(campaignDeviceDoc(doc, spec.ID, "d1"))["resultConfig"] = `{"z":9}`
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("rollback shadow report time differs", func(t *testing.T) {
		_, dir, _, _ := setupRollbackSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "d1")["lastReportTime"] = "2026-10-02T12:09:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})
}

// TestSuccessReportEqualSemanticEquivalenceOpens 序号相等时，配置比较沿用现有
// 语义：对象字段顺序、空白不影响结果，数字按数值比较；时间按同一时刻判断，
// 时区表示不同不报错。
func TestSuccessReportEqualSemanticEquivalenceOpens(t *testing.T) {
	t.Run("config whitespace and field order", func(t *testing.T) {
		// 成功时附带 {"ok":true,"n":1}，落盘后改写操作保存的配置写法。
		_, dir, spec := setupSucceededWithConfig(t, json.RawMessage(`{"ok":true,"n":1}`))
		rewriteStoreBytes(t, dir, func(doc map[string]any) {
			installDoc(campaignDeviceDoc(doc, spec.ID, "d1"))["resultConfig"] =
				json.RawMessage(` { "n": 1.0 , "ok": true } `)
		})
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("semantically equal config (whitespace/order/numeric) must open: %v", err)
		}
		defer s.Close()
	})
	t.Run("same instant different zone", func(t *testing.T) {
		// 安装结果于 12:03:00Z 接受；把操作时间与对应历史一起改为 +08:00 的同一时刻。
		_, dir, _ := setupSucceeded(t)
		rewriteStoreBytes(t, dir, func(doc map[string]any) {
			installDoc(campaignDeviceDoc(doc, "cmp-1", "d1"))["at"] = "2026-10-02T20:03:00+08:00"
			resultDocs(doc, "cmp-1")[1]["at"] = "2026-10-02T20:03:00+08:00"
		})
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("same instant in another zone must open: %v", err)
		}
		defer s.Close()
		want := upBase.Add(3 * time.Minute)
		if got := mustGetCampaign(s, "cmp-1").Results[1].At; !got.Equal(want) {
			t.Fatalf("restored instant %v != %v", got, want)
		}
	})
	t.Run("shadow time written in another zone", func(t *testing.T) {
		// 影子最近上报时间换一种时区表示，仍与成功记录时间是同一时刻。
		_, dir, _ := setupSucceeded(t)
		rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "d1")["lastReportTime"] = "2026-10-02T20:03:00+08:00"
		})
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("shadow time with zone offset equal instant must open: %v", err)
		}
		s.Close()
	})
}

// TestSuccessReportSmallerSeqIsHistory 成功记录序号小于设备最近序号时，它是被
// 后续上报覆盖的历史：不要求版本/配置/时间等于当前影子；原成功记录与活动
// 结论保持原样，影子也不能被旧记录再次应用。
func TestSuccessReportSmallerSeqIsHistory(t *testing.T) {
	_, dir, spec := setupSucceeded(t)
	// 安装成功（seq 2、配置 {"ok":true}）后再用更大序号上报另一版本与配置。
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	later := upBase.Add(90 * time.Minute)
	if err := s.Report("d1", 3, later, "v3", json.RawMessage(`{"later":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// 只把旧成功记录保存的配置改成与当前影子完全不同的值：其版本（v2）也
	// 不等于当前影子版本（v3）。因为序号已较小，这些历史字段不参与与当前
	// 影子的对账，存储必须照常打开。
	rewriteStoreBytes(t, dir, func(doc map[string]any) {
		installDoc(campaignDeviceDoc(doc, spec.ID, "d1"))["resultConfig"] =
			json.RawMessage(`{"ancient":true}`)
	})
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("older success record is history and must open: %v", err)
	}
	defer s2.Close()
	cv := mustGetCampaign(s2, spec.ID)
	d := findDevice(cv, "d1")
	if d.Status != DeviceSucceeded || cv.Status != CampaignSucceeded || !cv.Ended {
		t.Fatalf("conclusion changed: device=%s campaign=%s ended=%t", d.Status, cv.Status, cv.Ended)
	}
	// 历史结论保持原样（版本仍为当时的目标版本 v2）。
	if cv.Results[1].Version != "v2" {
		t.Fatalf("history record changed: %+v", cv.Results[1])
	}
	// 旧记录没有被当作新上报再次应用：影子保留更大序号上报后的样子。
	v, _ := s2.Get("d1")
	if v.LastSeq != 3 || v.Version != "v3" || !rawEqual(v.Reported, json.RawMessage(`{"later":true}`)) {
		t.Fatalf("old success record reapplied to shadow: %+v", v)
	}
}

// TestSuccessReportOfflineDoesNotMatter 设备后来被标为离线不影响合法成功记录
// 与影子的对账。
func TestSuccessReportOfflineDoesNotMatter(t *testing.T) {
	_, dir, spec := setupSucceeded(t)
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
		t.Fatalf("offline device with equal-seq success record must open: %v", err)
	}
	defer s2.Close()
	cv := mustGetCampaign(s2, spec.ID)
	d := findDevice(cv, "d1")
	if d.Status != DeviceSucceeded {
		t.Fatalf("status changed: %s", d.Status)
	}
	v, _ := s2.Get("d1")
	if v.Online || v.LastSeq != 2 || v.Version != "v2" {
		t.Fatalf("shadow changed: %+v", v)
	}
}

// TestSuccessReportPlainReportFirstReopens 普通上报先被接受、相同内容随后作为
// 安装附带上报被接受（按重复处理、不重写影子，设备可处于离线）的状态必须
// 正常重开；重开后已接受结果的相同重提仍成功且不增加历史。
func TestSuccessReportPlainReportFirstReopens(t *testing.T) {
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
	in, _ := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute))
	reportAt := upBase.Add(5 * time.Minute)
	cfg := json.RawMessage(`{"x":2}`)
	// 普通上报先接受序号 2。
	if err := s.Report("d1", 2, reportAt, "v2", cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOffline("d1"); err != nil {
		t.Fatal(err)
	}
	// 相同内容作为安装附带上报被接受（重复上报，影子不重写）。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: reportAt, Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(` { "x": 2.0 } `),
	}); err != nil {
		t.Fatalf("install with duplicated attached report: %v", err)
	}
	dir := s.dir
	s.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("plain-report-first success record must reopen: %v", err)
	}
	defer s2.Close()
	v, _ := s2.Get("d1")
	if v.Online || v.Version != "v2" || v.LastSeq != 2 || !rawEqual(v.Reported, cfg) {
		t.Fatalf("shadow changed after reopen: %+v", v)
	}
	sh, _ := s2.GetDeviceWork("d1")
	if sh.CampaignID != "" || sh.Pending != nil {
		t.Fatalf("ended campaign must have no pending work: %+v", sh)
	}
	// 已接受安装结果的相同重提仍成功且不增加历史。
	if err := s2.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: reportAt.Add(time.Hour), Success: true,
		Seq: 2, Version: "v2", Config: cfg,
	}); err != nil {
		t.Fatalf("replay after reopen: %v", err)
	}
	if cv := mustGetCampaign(s2, spec.ID); len(cv.Results) != 2 {
		t.Fatalf("replay added history: %+v", cv.Results)
	}
}

// TestSuccessReportOneBadDeviceRejectsWholeStore 多设备存储中任一台设备的一条
// 成功记录违反规则，都拒绝打开整个存储并保留原文件内容。
func TestSuccessReportOneBadDeviceRejectsWholeStore(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2"}
	spec.BatchSize = 2
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	finishDevice(t, s, spec, "d1", upBase.Add(2*time.Minute))
	finishDevice(t, s, spec, "d2", upBase.Add(10*time.Minute))
	dir := s.dir
	s.Close()
	// 只把 d2 影子的最近上报时间改坏：d1 合法也不能打开。
	original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
		deviceDoc(doc, "d2")["lastReportTime"] = "2026-10-02T01:00:00Z"
	})
	s2, err := Open(dir)
	if err == nil {
		s2.Close()
		t.Fatal("store with one bad device must not open")
	}
	if !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("must wrap ErrCorruptStorage: %v", err)
	}
	assertReopenCorrupt(t, dir, original)
}

// TestSuccessReportCheckedWhileCampaignRunning 检查不豁免尚未结束的活动：
// d1 已安装成功、d2 尚未开始（活动 running）时，d1 的成功记录仍必须与影子
// 对账，合法状态正常打开，序号矛盾拒绝打开。
func TestSuccessReportCheckedWhileCampaignRunning(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2"}
	spec.BatchSize = 2
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	// 只完成 d1；d2 离线、活动保持 running。
	finishDevice(t, s, spec, "d1", upBase.Add(2*time.Minute))
	dir := s.dir
	s.Close()

	// 合法的 running 状态正常打开，d1 的成功记录与影子一致。
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("running campaign with a succeeded device must open: %v", err)
	}
	cv := mustGetCampaign(s2, spec.ID)
	if cv.Status != CampaignRunning || cv.Ended || findDevice(cv, "d1").Status != DeviceSucceeded {
		t.Fatalf("unexpected restored state: %+v", cv)
	}
	s2.Close()

	// d1 成功记录序号超过影子记录：活动未结束同样拒绝打开。
	original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
		installDoc(campaignDeviceDoc(doc, spec.ID, "d1"))["resultSeq"] = 7
	})
	assertReopenCorrupt(t, dir, original)
}

// setupSucceededWithConfig 与 setupSucceeded 相同，但安装成功附带指定配置
// （配置中可带数字以验证数值比较）；设备安装时间固定为 12:03Z。
func setupSucceededWithConfig(t *testing.T, config json.RawMessage) (*Store, string, CampaignSpec) {
	t.Helper()
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
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
		t.Fatalf("download: %v", err)
	}
	in, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("claim install: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: upBase.Add(3 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: config,
	}); err != nil {
		t.Fatalf("install: %v", err)
	}
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return s, dir, spec
}

// setupRollbackSucceeded 构造回滚成功已接受（活动失败结束）的存储：
// 设备影子版本回到锁定目标 v1、序号 2、配置 {}；返回回滚操作。
func setupRollbackSucceeded(t *testing.T) (*Store, string, CampaignSpec, *Operation) {
	t.Helper()
	s, _, spec, rbOp := setupInstallFailure(t, true)
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil {
		t.Fatalf("claim rollback: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rbOp.ID,
		At: upBase.Add(5 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("rollback success: %v", err)
	}
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return s, dir, spec, rbOp
}
