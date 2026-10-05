package shadow

import (
	"encoding/json"
	"testing"
	"time"
)

// deviceOpDoc 返回存储文件中某活动某设备某阶段的操作记录。
func deviceOpDoc(doc map[string]any, campaignID, deviceID, stage string) map[string]any {
	for _, d := range campaignDoc(doc, campaignID)["devices"].([]any) {
		dm := d.(map[string]any)
		if dm["deviceId"] == deviceID {
			return dm[stage].(map[string]any)
		}
	}
	return nil
}

// deviceDoc 返回存储文件中某台设备的影子记录。
func deviceDoc(doc map[string]any, id string) map[string]any {
	return doc["devices"].(map[string]any)[id].(map[string]any)
}

// setupRollbackSucceeded 构造开启回滚的活动：下载成功、安装失败、回滚成功
// （附带上报 seq 2、版本 v1、配置 {}），活动已以 failed 结束。
func setupRollbackSucceeded(t *testing.T) (dir string, spec CampaignSpec) {
	t.Helper()
	s, dir, spec, rb := setupInstallFailure(t, true)
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(5 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("rollback result: %v", err)
	}
	s.Close()
	return dir, spec
}

// TestRestoreSuccessReportSeqChecked 校验重开存储时必须核对安装/回滚成功记录
// 保存的附带上报序号与设备影子最近接受上报的关系：序号缺失、为零或大于设备
// 最近接受的序号都拒绝打开整个存储并保留原文件内容。
func TestRestoreSuccessReportSeqChecked(t *testing.T) {
	t.Run("install seq missing", func(t *testing.T) {
		_, dir, spec := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			delete(deviceOpDoc(doc, spec.ID, "d1", StageInstall), "resultSeq")
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("install seq zero", func(t *testing.T) {
		_, dir, spec := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceOpDoc(doc, spec.ID, "d1", StageInstall)["resultSeq"] = 0
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("install seq beyond device", func(t *testing.T) {
		_, dir, spec := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceOpDoc(doc, spec.ID, "d1", StageInstall)["resultSeq"] = 3
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("rollback seq missing", func(t *testing.T) {
		dir, spec := setupRollbackSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			delete(deviceOpDoc(doc, spec.ID, "d1", StageRollback), "resultSeq")
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("rollback seq zero", func(t *testing.T) {
		dir, spec := setupRollbackSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceOpDoc(doc, spec.ID, "d1", StageRollback)["resultSeq"] = 0
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("rollback seq beyond device", func(t *testing.T) {
		dir, spec := setupRollbackSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceOpDoc(doc, spec.ID, "d1", StageRollback)["resultSeq"] = 3
		})
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreSuccessReportLatestMustMatchShadow 序号等于设备最近接受序号时，
// 成功记录代表设备最近一次上报：版本、完整上报配置与发生时间必须分别与影子
// 保存的版本、上报配置和最近上报时间一致，任一不符都拒绝打开。
func TestRestoreSuccessReportLatestMustMatchShadow(t *testing.T) {
	t.Run("install version mismatch", func(t *testing.T) {
		_, dir, _ := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "d1")["version"] = "v9"
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("install config mismatch", func(t *testing.T) {
		_, dir, _ := setupSucceeded(t)
		// 改影子上报配置的值（差异路径不变，绕过差异计时核对，专门命中配置比对）。
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "d1")["reported"] = json.RawMessage(`{"ok":false}`)
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("install time mismatch", func(t *testing.T) {
		_, dir, _ := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "d1")["lastReportTime"] = "2026-10-02T12:04:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("rollback version mismatch", func(t *testing.T) {
		dir, _ := setupRollbackSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "d1")["version"] = "v9"
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("rollback config mismatch", func(t *testing.T) {
		dir, _ := setupRollbackSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "d1")["reported"] = json.RawMessage(`{"x":1}`)
			// 差异路径随之改变，同步差异计时记录，专门命中配置比对。
			deviceDoc(doc, "d1")["diffSince"] = map[string]any{"/x": "2026-10-02T12:05:00Z"}
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("rollback time mismatch", func(t *testing.T) {
		dir, _ := setupRollbackSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "d1")["lastReportTime"] = "2026-10-02T12:06:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreSuccessReportHistoricalSeq 成功记录的序号小于设备最近接受序号时，
// 它是已被后续上报覆盖的历史记录：不要求版本、配置或时间等于当前影子，
// 重开后影子、活动结论与结果历史保持原样，旧记录不会被再次应用。
func TestRestoreSuccessReportHistoricalSeq(t *testing.T) {
	_, dir, spec := setupSucceeded(t)
	// 安装成功（seq 2、v2、{"ok":true}）之后，设备用更大序号上报另一版本。
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Report("d1", 3, upBase.Add(10*time.Minute), "v3", json.RawMessage(`{"n":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s2.SetOffline("d1"); err != nil {
		t.Fatal(err)
	}
	cv0 := mustGetCampaign(s2, spec.ID)
	s2.Close()

	s3, err := Open(dir)
	if err != nil {
		t.Fatalf("historical success record must open: %v", err)
	}
	defer s3.Close()
	// 影子保持最近一次上报，旧成功记录不作为新上报再次应用。
	v, err := s3.Get("d1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != "v3" || v.LastSeq != 3 || v.Online {
		t.Fatalf("shadow changed by historical record: %+v", v)
	}
	// 活动结论与结果历史保持原样。
	cv := mustGetCampaign(s3, spec.ID)
	if cv.Status != cv0.Status || cv.Ended != cv0.Ended || len(cv.Results) != len(cv0.Results) {
		t.Fatalf("campaign changed: %+v vs %+v", cv, cv0)
	}
	for i := range cv0.Results {
		if cv0.Results[i] != cv.Results[i] {
			t.Fatalf("result %d changed: %+v vs %+v", i, cv0.Results[i], cv.Results[i])
		}
	}
	// 已接受结果的相同重提仍成功且不增加历史。
	if err := s3.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: cv.Results[1].OperationID,
		At: upBase.Add(3 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("resubmit accepted result: %v", err)
	}
	if got := len(mustGetCampaign(s3, spec.ID).Results); got != len(cv0.Results) {
		t.Fatalf("resubmit added history: %d -> %d", len(cv0.Results), got)
	}
}

// TestRestoreSuccessReportSharedWithPlainReport 普通上报先被接受、相同内容随后
// 作为安装成功附带上报被接受（重复上报不重新写入影子）的情况仍然合法。
func TestRestoreSuccessReportSharedWithPlainReport(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec.ID, "d1", upBase)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	// 普通上报先到：seq 2、v2、{"ok":true}。
	if err := s.Report("d1", 2, upBase.Add(3*time.Minute), "v2", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	// 相同内容随后作为安装成功附带上报被接受。
	inID := findDevice(mustGetCampaign(s, spec.ID), "d1").InstallID
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: inID,
		At: upBase.Add(3 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("shared report must be accepted: %v", err)
	}
	dir := s.dir
	s.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("shared plain/attached report must open: %v", err)
	}
	defer s2.Close()
	v, err := s2.Get("d1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != "v2" || v.LastSeq != 2 {
		t.Fatalf("shadow: %+v", v)
	}
	if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DeviceSucceeded {
		t.Fatal("install success lost")
	}
}

// TestRestoreSuccessReportCompareSemantics 配置比较沿用 JSON 语义（字段顺序、
// 空白与数字表示不影响结果），时间按同一时刻判断（时区表示不同不报错）。
func TestRestoreSuccessReportCompareSemantics(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec.ID, "d1", upBase)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	in, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: upBase.Add(3 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"b":1,"a":"x"}`),
	}); err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	s.Close()

	rewriteStoreBytes(t, dir, func(doc map[string]any) {
		d := deviceDoc(doc, "d1")
		// 字段顺序、空白与数字表示不同（1 vs 1.0），语义相同。
		d["reported"] = json.RawMessage(`{ "a" : "x", "b" : 1.0 }`)
		// 同一时刻的不同时区表示：12:03:00Z == 20:03:00+08:00。
		d["lastReportTime"] = "2026-10-02T20:03:00+08:00"
		deviceOpDoc(doc, spec.ID, "d1", StageInstall)["at"] = "2026-10-02T20:03:00+08:00"
	})
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("equivalent config and same instant must open: %v", err)
	}
	defer s2.Close()
	v, err := s2.Get("d1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != "v2" || v.LastSeq != 2 {
		t.Fatalf("shadow: %+v", v)
	}
}

// TestRestoreSuccessReportOfflineDevice 设备后来被标为离线不影响合法成功记录。
func TestRestoreSuccessReportOfflineDevice(t *testing.T) {
	_, dir, spec := setupSucceeded(t)
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.SetOffline("d1"); err != nil {
		t.Fatal(err)
	}
	s2.Close()

	s3, err := Open(dir)
	if err != nil {
		t.Fatalf("offline device with legit success record must open: %v", err)
	}
	defer s3.Close()
	v, err := s3.Get("d1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Online || v.Version != "v2" || v.LastSeq != 2 {
		t.Fatalf("shadow: %+v", v)
	}
	if findDevice(mustGetCampaign(s3, spec.ID), "d1").Status != DeviceSucceeded {
		t.Fatal("install success lost")
	}
}
