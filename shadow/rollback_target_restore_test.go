package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// campaignDeviceDoc 返回存储文档中指定活动、指定设备的磁盘记录。
func campaignDeviceDoc(doc map[string]any, campaignID, deviceID string) map[string]any {
	for _, d := range campaignDoc(doc, campaignID)["devices"].([]any) {
		dev := d.(map[string]any)
		if dev["deviceId"] == deviceID {
			return dev
		}
	}
	return nil
}

// setupRollbackCampaign 创建开启回滚的单设备活动并让 d1 在线，返回未关闭的存储。
func setupRollbackCampaign(t *testing.T) (*Store, CampaignSpec) {
	t.Helper()
	s := setupUpgrade(t, "d1")
	spec := rbSpec()
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	return s, spec
}

// closeStore 关闭存储并返回其目录，供改写磁盘记录后验证拒绝打开。
func closeStore(t *testing.T, s *Store) string {
	t.Helper()
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRestoreRollbackTargetRequired 保护开启回滚活动的回滚目标完整性：
// 设备已领取下载且尚无下载结果，或已有成功下载结果时，保存记录必须带非空的
// 锁定回滚版本；字段缺失与空串按同一种损坏处理。这项要求不因设备进入安装
// 阶段或活动已结束而取消。任一设备违反时 Open 必须返回 ErrCorruptStorage，
// 整个存储都不能打开、原文件内容保持不变——不能仅删除这台设备或这个活动，
// 让其他记录照常进入可用状态。
func TestRestoreRollbackTargetRequired(t *testing.T) {
	// 已领取下载、尚未有下载结果（downloading）时目标丢失。
	t.Run("downloading without locked target", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
		if _, err := s.Claim(spec.ID, "d1", upBase); err != nil {
			t.Fatalf("claim download: %v", err)
		}
		if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceDownloading ||
			d.RollbackTarget != "v1" {
			t.Fatalf("precondition: %+v", d)
		}
		dir := closeStore(t, s)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			delete(campaignDeviceDoc(doc, spec.ID, "d1"), "rollbackTarget")
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 下载成功、等待安装（ready）时目标被置为空串：与字段缺失同样处理。
	t.Run("ready with empty target", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
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
		if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceReady {
			t.Fatalf("precondition: %+v", d)
		}
		dir := closeStore(t, s)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDeviceDoc(doc, spec.ID, "d1")["rollbackTarget"] = ""
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 已领取安装（installing）时目标丢失：进入安装阶段不取消这项要求。
	t.Run("installing without locked target", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
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
		if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceInstalling {
			t.Fatalf("precondition: %+v", d)
		}
		dir := closeStore(t, s)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			delete(campaignDeviceDoc(doc, spec.ID, "d1"), "rollbackTarget")
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 设备已成功、活动已结束时目标丢失：活动结束也不取消这项要求。
	t.Run("ended campaign without locked target", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
		finishDevice(t, s, spec, "d1", upBase)
		if v := mustGetCampaign(s, spec.ID); !v.Ended || v.Status != CampaignSucceeded {
			t.Fatalf("precondition: %+v", v)
		}
		dir := closeStore(t, s)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			delete(campaignDeviceDoc(doc, spec.ID, "d1"), "rollbackTarget")
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 同一存储里还有其他合法活动与设备时，仍必须整体拒绝打开，
	// 不能只删掉损坏的设备或活动、让其余记录照常可用。
	t.Run("whole store rejected alongside valid records", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
		mustRegister(t, s, "d2", "v1")
		other := upSpec()
		other.ID = "cmp-2"
		other.Devices = []string{"d2"}
		other.BatchSize = 1
		createCampaign(t, s, other)
		if _, err := s.Claim(spec.ID, "d1", upBase); err != nil {
			t.Fatalf("claim download: %v", err)
		}
		dir := closeStore(t, s)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			delete(campaignDeviceDoc(doc, spec.ID, "d1"), "rollbackTarget")
		})
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreRollbackTargetLegitStatesReopen 保护允许没有回滚目标的正常记录：
// 尚未领取下载的设备、首次领取前复查版本不兼容而直接在下载阶段失败的设备，
// 以及目标已正常锁定的记录（即使锁定版本没有单独登记为升级目标、设备影子
// 版本此后已被普通上报改变）都必须照常打开，回滚待办仍指向原先锁定的版本。
func TestRestoreRollbackTargetLegitStatesReopen(t *testing.T) {
	// 新活动中的设备还没领取下载，可以没有回滚目标。
	t.Run("pending device without target", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		if d := findDevice(mustGetCampaign(s2, spec.ID), "d1"); d.Status != DevicePending ||
			d.RollbackTarget != "" {
			t.Fatalf("pending device after reopen: %+v", d)
		}
	})

	// 首次领取前复查版本不兼容、直接在下载阶段失败的设备允许没有目标：
	// 这种失败不进入安装或回滚，不能只因记录中已有下载领取信息就拒绝打开。
	t.Run("incompatible download failure without target", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
		// 领取前通过普通上报把版本改为不兼容的 v9。
		if err := s.Report("d1", 2, upBase.Add(-time.Minute), "v9", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("report: %v", err)
		}
		_, err := s.Claim(spec.ID, "d1", upBase)
		if !errors.Is(err, ErrIncompatibleVersion) {
			t.Fatalf("claim must fail incompatible: %v", err)
		}
		d := findDevice(mustGetCampaign(s, spec.ID), "d1")
		if d.Status != DeviceFailed || d.Phase != StageDownload || d.RollbackTarget != "" {
			t.Fatalf("precondition: %+v", d)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		d = findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceFailed || d.Phase != StageDownload || d.RollbackTarget != "" {
			t.Fatalf("failed device after reopen: %+v", d)
		}
	})

	// 锁定目标代表首次领取下载时的设备版本：下载后普通上报把影子版本改为
	// v9，锁定的 v1 不变；v1 并未单独登记为升级目标，记录仍须原样打开，
	// 安装失败后的回滚待办仍指向 v1，不能用当前影子版本或活动目标替代。
	t.Run("locked target kept despite later version change", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
		dl, err := s.Claim(spec.ID, "d1", upBase)
		if err != nil {
			t.Fatalf("claim download: %v", err)
		}
		// 下载已领取、目标已锁定为 v1；此后普通上报只改影子，不改锁定目标。
		if err := s.Report("d1", 2, upBase.Add(30*time.Second), "v9", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("report: %v", err)
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
			At: upBase.Add(3 * time.Minute), Success: false, Reason: "install broken",
		}); err != nil {
			t.Fatalf("install failure: %v", err)
		}
		if sh, _ := s.Get("d1"); sh.Version != "v9" {
			t.Fatalf("precondition: shadow version %s", sh.Version)
		}
		dir := closeStore(t, s)

		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceAwaitingRollback || d.RollbackTarget != "v1" {
			t.Fatalf("awaiting rollback after reopen: %+v", d)
		}
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending == nil || w.Pending.Kind != StageRollback || w.Pending.TargetVersion != "v1" {
			t.Fatalf("rollback todo must point at locked target: %+v", w)
		}
		if sh, _ := s2.Get("d1"); sh.Version != "v9" {
			t.Fatalf("shadow version after reopen: %s", sh.Version)
		}
	})
}
