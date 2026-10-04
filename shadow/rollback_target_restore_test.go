package shadow

import (
	"encoding/json"
	"testing"
	"time"
)

// campaignDeviceDoc 返回存储文档中指定活动里指定设备的磁盘记录。
func campaignDeviceDoc(doc map[string]any, campaignID, deviceID string) map[string]any {
	for _, d := range campaignDoc(doc, campaignID)["devices"].([]any) {
		dm := d.(map[string]any)
		if dm["deviceId"] == deviceID {
			return dm
		}
	}
	return nil
}

// 开启回滚的活动中，设备在各进度下被删掉或清空锁定回滚目标（rollbackTarget）
// 都属于损坏：Open 必须返回 ErrCorruptStorage，整个存储不能打开，原文件保持不变；
// 不能仅删除这台设备或这个活动让其余记录照常可用。
func TestRestoreRollbackTargetMissingRefused(t *testing.T) {
	// 每个用例构造一份开启回滚的活动存储并关闭，返回目录；
	// 之后统一删掉 d1 的 rollbackTarget 再验证拒绝打开。
	cases := map[string]func(t *testing.T) string{
		// 已领取下载、尚无下载结果。
		"download claimed unfinished": func(t *testing.T) string {
			s, dir := setupUpgradeDir(t, "d1")
			spec := rbSpec()
			createCampaign(t, s, spec)
			bringOnline(t, s, "d1")
			if dl, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute)); err != nil || dl == nil {
				t.Fatalf("claim download: %v %+v", err, dl)
			}
			if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceDownloading {
				t.Fatalf("precondition: downloading, got %+v", d)
			}
			s.Close()
			return dir
		},
		// 下载成功、等待安装（同活动还有正常 pending 设备，也不能单独可用）。
		"download succeeded ready": func(t *testing.T) string {
			s, dir := setupUpgradeDir(t, "d1", "d2")
			spec := rbSpec()
			spec.Devices = []string{"d1", "d2"}
			spec.BatchSize = 2
			createCampaign(t, s, spec)
			bringOnline(t, s, "d1")
			dl, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
			if err != nil || dl == nil {
				t.Fatalf("claim download: %v %+v", err, dl)
			}
			if err := s.SubmitResult(OperationResult{
				CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
				At: upBase.Add(3 * time.Minute), Success: true,
			}); err != nil {
				t.Fatalf("download result: %v", err)
			}
			if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceReady {
				t.Fatalf("precondition: ready, got %+v", d)
			}
			s.Close()
			return dir
		},
		// 已领取安装、尚无安装结果。
		"install claimed unfinished": func(t *testing.T) string {
			s, dir := setupUpgradeDir(t, "d1")
			spec := rbSpec()
			createCampaign(t, s, spec)
			bringOnline(t, s, "d1")
			dl, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
			if err := s.SubmitResult(OperationResult{
				CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
				At: upBase.Add(3 * time.Minute), Success: true,
			}); err != nil {
				t.Fatalf("download result: %v", err)
			}
			if in, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil || in == nil {
				t.Fatalf("claim install: %v %+v", err, in)
			}
			s.Close()
			return dir
		},
		// 安装失败已接受、等待回滚。
		"awaiting rollback": func(t *testing.T) string {
			s, dir := setupUpgradeDir(t, "d1")
			spec := rbSpec()
			createCampaign(t, s, spec)
			bringOnline(t, s, "d1")
			awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
			s.Close()
			return dir
		},
		// 安装成功、活动已结束：要求同样不取消。
		"install succeeded campaign ended": func(t *testing.T) string {
			s, dir := setupUpgradeDir(t, "d1")
			spec := rbSpec()
			createCampaign(t, s, spec)
			bringOnline(t, s, "d1")
			finishDevice(t, s, spec, "d1", upBase.Add(2*time.Minute))
			if v := mustGetCampaign(s, spec.ID); !v.Ended || v.Status != CampaignSucceeded {
				t.Fatalf("precondition: ended succeeded, got %+v", v)
			}
			s.Close()
			return dir
		},
		// 回滚成功、活动已结束：目标仍须保留。
		"rollback succeeded campaign ended": func(t *testing.T) string {
			s, dir := setupUpgradeDir(t, "d1")
			spec := rbSpec()
			createCampaign(t, s, spec)
			bringOnline(t, s, "d1")
			awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
			rb, err := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute))
			if err != nil || rb == nil {
				t.Fatalf("claim rollback: %v %+v", err, rb)
			}
			if err := s.SubmitResult(OperationResult{
				CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
				At: upBase.Add(9 * time.Minute), Success: true,
				Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
			}); err != nil {
				t.Fatalf("rollback result: %v", err)
			}
			if v := mustGetCampaign(s, spec.ID); !v.Ended {
				t.Fatalf("precondition: ended, got %+v", v)
			}
			s.Close()
			return dir
		},
	}
	for name, setup := range cases {
		t.Run(name+" target deleted", func(t *testing.T) {
			dir := setup(t)
			original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
				delete(campaignDeviceDoc(doc, "cmp-1", "d1"), "rollbackTarget")
			})
			assertReopenCorrupt(t, dir, original)
		})
		t.Run(name+" target emptied", func(t *testing.T) {
			dir := setup(t)
			// 空串与字段缺失按同一种损坏处理。
			original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
				campaignDeviceDoc(doc, "cmp-1", "d1")["rollbackTarget"] = ""
			})
			assertReopenCorrupt(t, dir, original)
		})
	}
}

// 尚未锁定目标的正常记录必须保留：新活动中还没领取下载的设备可以没有回滚目标。
func TestRestoreRollbackPendingWithoutTargetAccepted(t *testing.T) {
	s, dir := setupUpgradeDir(t, "d1")
	spec := rbSpec()
	createCampaign(t, s, spec)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("pending device without rollback target must open: %v", err)
	}
	defer s2.Close()
	if d := findDevice(mustGetCampaign(s2, spec.ID), "d1"); d.Status != DevicePending || d.RollbackTarget != "" {
		t.Fatalf("pending device after reopen: %+v", d)
	}
}

// 锁定目标代表首次领取下载时的设备版本：下载后普通上报改变影子版本，
// 重开后记录仍须原样保留原目标（它并未单独登记为升级目标）；
// 之后安装失败时，回滚待办仍指向这个原先锁定的版本，而不是当前影子版本。
func TestRestoreRollbackTargetKeptDespiteVersionDrift(t *testing.T) {
	s, dir := setupUpgradeDir(t, "d1")
	spec := rbSpec()
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	if err != nil || dl == nil {
		t.Fatalf("claim download: %v %+v", err, dl)
	}
	// 下载后普通上报把影子版本改成未登记的版本：锁定目标不变。
	if err := s.Report("d1", 2, upBase.Add(3*time.Minute), "v9-other", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(4 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("download result: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("locked target not registered as upgrade target must still open: %v", err)
	}
	defer s2.Close()
	if sh, _ := s2.Get("d1"); sh.Version != "v9-other" {
		t.Fatalf("shadow version after reopen: %+v", sh)
	}
	if d := findDevice(mustGetCampaign(s2, spec.ID), "d1"); d.RollbackTarget != "v1" {
		t.Fatalf("locked target must survive reopen: %+v", d)
	}
	// 安装失败后，回滚待办仍指向原先锁定的 v1，而非当前影子版本。
	in, err := s2.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
	if err != nil || in == nil {
		t.Fatalf("claim install: %v %+v", err, in)
	}
	if err := s2.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: upBase.Add(6 * time.Minute), Success: false, Reason: "install broken",
	}); err != nil {
		t.Fatalf("install failure: %v", err)
	}
	w, err := s2.GetDeviceWork("d1")
	if err != nil {
		t.Fatal(err)
	}
	if w.Pending == nil || w.Pending.Kind != StageRollback || w.Pending.TargetVersion != "v1" {
		t.Fatalf("rollback todo must point at originally locked version: %+v", w)
	}
}

// 未开启回滚的活动不受锁定目标要求约束：下载成功等待安装的记录没有
// rollbackTarget 仍正常打开，沿用现有规则。
func TestRestoreNonRollbackCampaignWithoutTargetAccepted(t *testing.T) {
	s, dir := setupUpgradeDir(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	if err != nil || dl == nil {
		t.Fatalf("claim download: %v %+v", err, dl)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(3 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("download result: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("non-rollback campaign without target must open: %v", err)
	}
	defer s2.Close()
	v := mustGetCampaign(s2, spec.ID)
	if v.RollbackOnFailure {
		t.Fatal("rollback flag must stay off")
	}
	if d := findDevice(v, "d1"); d.Status != DeviceReady || d.RollbackTarget != "" {
		t.Fatalf("non-rollback ready device after reopen: %+v", d)
	}
}
