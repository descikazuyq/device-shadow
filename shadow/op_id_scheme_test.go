package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// 操作标识消歧与兼容的回归测试：活动或设备标识含冒号时，
// 不同（活动, 设备, 阶段）组合必须拿到互不相同的操作标识。

// TestOperationIDDistinguishesColonAmbiguousCampaigns 复现串用场景：
// 活动 a:b 中设备 c 与活动 a 中设备 b:c 的同一阶段，旧方案会拼出相同标识。
// 两个活动与两台设备都是合法且互不相同的记录，必须各自领取和完成自己的操作。
func TestOperationIDDistinguishesColonAmbiguousCampaigns(t *testing.T) {
	s := setupUpgrade(t, "c", "b:c")
	specAB := upSpec()
	specAB.ID = "a:b"
	specAB.Devices = []string{"c"}
	specAB.BatchSize = 1
	createCampaign(t, s, specAB)
	specA := upSpec()
	specA.ID = "a"
	specA.Devices = []string{"b:c"}
	specA.BatchSize = 1
	createCampaign(t, s, specA)
	bringOnline(t, s, "c")
	bringOnline(t, s, "b:c")

	at := upBase.Add(time.Minute)
	opC, err := s.Claim("a:b", "c", at)
	if err != nil || opC == nil {
		t.Fatalf("claim a:b/c: %v %+v", err, opC)
	}
	opB, err := s.Claim("a", "b:c", at)
	if err != nil || opB == nil {
		t.Fatalf("claim a/b:c: %v %+v", err, opB)
	}
	if opC.ID == opB.ID {
		t.Fatalf("ambiguous operation id shared by both campaigns: %q", opC.ID)
	}
	// 标识必须明确归属各自的活动、设备与阶段。
	if !validV2OpID(opC.ID, "a:b", "c", StageDownload) {
		t.Fatalf("a:b/c download id does not identify its operation: %q", opC.ID)
	}
	if !validV2OpID(opB.ID, "a", "b:c", StageDownload) {
		t.Fatalf("a/b:c download id does not identify its operation: %q", opB.ID)
	}
	// 查询设备待办返回的标识与领取的一致，可直接用于提交。
	if w, err := s.GetDeviceWork("c"); err != nil || w.Pending == nil ||
		w.Pending.ID != opC.ID || w.CampaignID != "a:b" {
		t.Fatalf("device work for c: %v %+v", err, w)
	}
	if w, err := s.GetDeviceWork("b:c"); err != nil || w.Pending == nil ||
		w.Pending.ID != opB.ID || w.CampaignID != "a" {
		t.Fatalf("device work for b:c: %v %+v", err, w)
	}

	// 把 a:b/c 的标识用于 a/b:c 的提交：必须拒绝且不改任何状态。
	submitAt := upBase.Add(2 * time.Minute)
	err = s.SubmitResult(OperationResult{
		CampaignID: "a", DeviceID: "b:c", OperationID: opC.ID,
		At: submitAt, Success: true,
	})
	if !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("foreign op id must be rejected: %v", err)
	}
	// 反向同样拒绝。
	err = s.SubmitResult(OperationResult{
		CampaignID: "a:b", DeviceID: "c", OperationID: opB.ID,
		At: submitAt, Success: true,
	})
	if !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("foreign op id must be rejected (reverse): %v", err)
	}
	// 两边活动进度、结果历史与设备影子都未被改变。
	for id, dev := range map[string]string{"a:b": "c", "a": "b:c"} {
		v := mustGetCampaign(s, id)
		if len(v.Results) != 0 {
			t.Fatalf("campaign %s history changed by foreign submit: %+v", id, v.Results)
		}
		if d := findDevice(v, dev); d.Status != DeviceDownloading {
			t.Fatalf("campaign %s device %s state changed: %+v", id, dev, d)
		}
		sh, err := s.Get(dev)
		if err != nil || sh.Version != "v1" {
			t.Fatalf("device %s shadow changed: %v %+v", dev, err, sh)
		}
	}

	// 正确归属的操作继续按原有规则接受：各自完成下载。
	if err := s.SubmitResult(OperationResult{
		CampaignID: "a:b", DeviceID: "c", OperationID: opC.ID,
		At: submitAt, Success: true,
	}); err != nil {
		t.Fatalf("own download result a:b/c: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: "a", DeviceID: "b:c", OperationID: opB.ID,
		At: submitAt, Success: true,
	}); err != nil {
		t.Fatalf("own download result a/b:c: %v", err)
	}

	// 保存后再次打开：标识保持不变，串用拒绝与正常推进仍然有效。
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	inC, err := s2.Claim("a:b", "c", upBase.Add(3*time.Minute))
	if err != nil || inC == nil {
		t.Fatalf("claim install a:b/c after reopen: %v %+v", err, inC)
	}
	inB, err := s2.Claim("a", "b:c", upBase.Add(3*time.Minute))
	if err != nil || inB == nil {
		t.Fatalf("claim install a/b:c after reopen: %v %+v", err, inB)
	}
	if inC.ID == inB.ID {
		t.Fatalf("install ids collide after reopen: %q", inC.ID)
	}
	if inC.ID != findDevice(mustGetCampaign(s2, "a:b"), "c").InstallID ||
		inB.ID != findDevice(mustGetCampaign(s2, "a"), "b:c").InstallID {
		t.Fatalf("install ids changed after reopen: %q %q", inC.ID, inB.ID)
	}
	err = s2.SubmitResult(OperationResult{
		CampaignID: "a", DeviceID: "b:c", OperationID: inC.ID,
		At: upBase.Add(4 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{}`),
	})
	if !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("foreign install id must be rejected after reopen: %v", err)
	}
	// 各自完成安装，两个活动都成功结束。
	for _, tc := range []struct {
		campaign, device, opID string
	}{{"a:b", "c", inC.ID}, {"a", "b:c", inB.ID}} {
		if err := s2.SubmitResult(OperationResult{
			CampaignID: tc.campaign, DeviceID: tc.device, OperationID: tc.opID,
			At: upBase.Add(4 * time.Minute), Success: true,
			Seq: 2, Version: "v2", Config: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("own install result %s/%s: %v", tc.campaign, tc.device, err)
		}
		v := mustGetCampaign(s2, tc.campaign)
		if v.Status != CampaignSucceeded || findDevice(v, tc.device).Status != DeviceSucceeded {
			t.Fatalf("campaign %s must succeed: %+v", tc.campaign, v)
		}
	}
}

// TestNewOperationIDsAvoidStoredCollision 新生成的标识不能与存储内其他操作
// （含修复前保存的旧方案标识）相同：偶然撞上时必须避开，且旧活动继续用原标识。
func TestNewOperationIDsAvoidStoredCollision(t *testing.T) {
	s := setupUpgrade(t, "c")
	spec := upSpec()
	// 旧方案下活动 v2:0:1:a2 中设备 c 的下载标识为 v2:0:1:a2:c:download，
	// 恰好等于新活动 a 中设备 c: 的 v2 首选标识。
	spec.ID = "v2:0:1:a2"
	spec.Devices = []string{"c"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	// 修复前保存的形态：下载已领取并已接受结果，结果历史已落盘。
	bringOnline(t, s, "c")
	dl, err := s.Claim(spec.ID, "c", upBase.Add(time.Minute))
	if err != nil || dl == nil {
		t.Fatalf("claim download: %v %+v", err, dl)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "c", OperationID: dl.ID,
		At: upBase.Add(2 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("download result: %v", err)
	}
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// 把该活动改写为修复前保存的形态：无 idScheme，操作与历史均为旧方案标识。
	rewriteStoreBytes(t, dir, func(doc map[string]any) {
		camp := campaignDoc(doc, "v2:0:1:a2")
		delete(camp, "idScheme")
		dev := camp["devices"].([]any)[0].(map[string]any)
		dev["download"].(map[string]any)["id"] = "v2:0:1:a2:c:download"
		dev["install"].(map[string]any)["id"] = "v2:0:1:a2:c:install"
		dev["rollback"].(map[string]any)["id"] = "v2:0:1:a2:c:rollback"
		camp["results"].([]any)[0].(map[string]any)["operationId"] = "v2:0:1:a2:c:download"
	})
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("legacy campaign must reopen: %v", err)
	}
	defer s2.Close()
	// 已有结果的重复提交判断与历史保持有效：重放不增加历史。
	if err := s2.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "c", OperationID: "v2:0:1:a2:c:download",
		At: upBase.Add(2 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("replay of accepted legacy result must be idempotent: %v", err)
	}
	if v := mustGetCampaign(s2, spec.ID); len(v.Results) != 1 {
		t.Fatalf("legacy history must not grow on replay: %+v", v.Results)
	}

	// 新活动 a 中设备 c: 的标识必须避开存储内已有的旧标识。
	mustRegister(t, s2, "c:", "v1")
	spec2 := upSpec()
	spec2.ID = "a"
	spec2.Devices = []string{"c:"}
	spec2.BatchSize = 1
	createCampaign(t, s2, spec2)
	d := findDevice(mustGetCampaign(s2, "a"), "c:")
	if d.DownloadID == "v2:0:1:a2:c:download" {
		t.Fatalf("new download id collides with stored legacy id: %q", d.DownloadID)
	}
	if !validV2OpID(d.DownloadID, "a", "c:", StageDownload) ||
		!validV2OpID(d.InstallID, "a", "c:", StageInstall) ||
		!validV2OpID(d.RollbackID, "a", "c:", StageRollback) {
		t.Fatalf("new ids must identify their own operations: %+v", d)
	}
	if d.DownloadID == d.InstallID || d.DownloadID == d.RollbackID || d.InstallID == d.RollbackID {
		t.Fatalf("stages of one device must not share ids: %+v", d)
	}

	// 旧活动已领取的操作用原标识完成；新活动用自己的标识领取。
	legacyOp, err := s2.Claim(spec.ID, "c", upBase.Add(3*time.Minute))
	if err != nil || legacyOp == nil {
		t.Fatalf("claim legacy install: %v %+v", err, legacyOp)
	}
	if legacyOp.ID != "v2:0:1:a2:c:install" || legacyOp.Kind != StageInstall {
		t.Fatalf("legacy campaign must keep its original ids: %+v", legacyOp)
	}
	if err := s2.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "c", OperationID: legacyOp.ID,
		At: upBase.Add(4 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("legacy op result: %v", err)
	}
	if v := mustGetCampaign(s2, spec.ID); v.Status != CampaignSucceeded {
		t.Fatalf("legacy campaign must succeed with original ids: %+v", v)
	}
	bringOnline(t, s2, "c:")
	newOp, err := s2.Claim("a", "c:", upBase.Add(5*time.Minute))
	if err != nil || newOp == nil {
		t.Fatalf("claim new download: %v %+v", err, newOp)
	}
	if newOp.ID != d.DownloadID {
		t.Fatalf("claimed id must match campaign view: %q vs %q", newOp.ID, d.DownloadID)
	}
	if err := s2.SubmitResult(OperationResult{
		CampaignID: "a", DeviceID: "c:", OperationID: newOp.ID,
		At: upBase.Add(6 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("new op result: %v", err)
	}
}

// TestRestoreV2OperationIDValidation 新方案活动的存储校验不得借兼容旧标识放宽：
// 标识方案未知、标识不是规范 v2 编码、归属不符都按损坏拒绝打开。
func TestRestoreV2OperationIDValidation(t *testing.T) {
	setup := func(t *testing.T) string {
		t.Helper()
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		dir := s.dir
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	downloadDoc := func(doc map[string]any) map[string]any {
		return campaignDoc(doc, "cmp-1")["devices"].([]any)[0].(map[string]any)["download"].(map[string]any)
	}

	t.Run("unknown scheme", func(t *testing.T) {
		dir := setup(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDoc(doc, "cmp-1")["idScheme"] = 7
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("legacy id in v2 campaign", func(t *testing.T) {
		dir := setup(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			downloadDoc(doc)["id"] = "cmp-1:d1:download"
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("id of another device", func(t *testing.T) {
		dir := setup(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			downloadDoc(doc)["id"] = encodeOperationIDV2(0, "cmp-1", "d2", StageDownload)
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("id of another stage", func(t *testing.T) {
		dir := setup(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			downloadDoc(doc)["id"] = encodeOperationIDV2(0, "cmp-1", "d1", StageInstall)
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("id of another campaign", func(t *testing.T) {
		dir := setup(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			downloadDoc(doc)["id"] = encodeOperationIDV2(0, "cmp-2", "d1", StageDownload)
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("non canonical id", func(t *testing.T) {
		dir := setup(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			downloadDoc(doc)["id"] = "v2:00:5:cmp-12:d1download"
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("rollback id missing in v2 campaign", func(t *testing.T) {
		dir := setup(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			dev := campaignDoc(doc, "cmp-1")["devices"].([]any)[0].(map[string]any)
			dev["rollback"].(map[string]any)["id"] = ""
		})
		assertReopenCorrupt(t, dir, original)
	})
}
