package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// rollbackSpec 返回开启安装失败回滚的活动规格。
func rollbackSpec() CampaignSpec {
	spec := upSpec()
	spec.RollbackOnFailure = true
	return spec
}

// failInstallAndWaitRollback 完成下载后使安装失败，设备进入 rollback_pending。
func failInstallAndWaitRollback(t *testing.T, s *Store, spec CampaignSpec, id string, at time.Time) {
	t.Helper()
	dl, err := s.Claim(spec.ID, id, at)
	if err != nil {
		t.Fatal(err)
	}
	if dl == nil || dl.Kind != StageDownload {
		t.Fatalf("want download op for %s, got %+v", id, dl)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: id, OperationID: dl.ID,
		At: at.Add(time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	in, err := s.Claim(spec.ID, id, at.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if in == nil || in.Kind != StageInstall {
		t.Fatalf("want install op for %s, got %+v", id, in)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: id, OperationID: in.ID,
		At: at.Add(3 * time.Minute), Success: false, Reason: "install broken",
	}); err != nil {
		t.Fatal(err)
	}
}

func claimRollback(t *testing.T, s *Store, spec CampaignSpec, id string, at time.Time) *Operation {
	t.Helper()
	rb, err := s.Claim(spec.ID, id, at)
	if err != nil {
		t.Fatal(err)
	}
	if rb == nil || rb.Kind != StageRollback {
		t.Fatalf("want rollback op for %s, got %+v", id, rb)
	}
	return rb
}

// TestRollbackDefaultOffNoRollback 验证默认关闭时安装失败直接记为设备失败，不产生回滚。
func TestRollbackDefaultOffNoRollback(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec() // 默认 RollbackOnFailure=false
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))

	v, _ := s.GetCampaign(spec.ID)
	d := findDevice(v, "d1")
	if d.Status != DeviceFailed || d.Phase != StageInstall || d.Reason != "install broken" {
		t.Fatalf("default off should fail device directly: %+v", d)
	}
	if d.RollbackID != "" || d.RollbackTarget != "" {
		t.Fatalf("default off should have no rollback fields: %+v", d)
	}
	if !v.Ended || v.Status != CampaignFailed {
		t.Fatalf("campaign should end failed: %+v", v)
	}
}

// TestRollbackTargetLockedAtFirstDownloadClaim 验证回滚目标在首次领取下载时锁定，
// 之后的普通上报不改变它。
func TestRollbackTargetLockedAtFirstDownloadClaim(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")

	// 领取下载：回滚目标锁定为设备当前版本 v1
	dl, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	if err != nil || dl == nil {
		t.Fatalf("claim download: %v %+v", err, dl)
	}
	v, _ := s.GetCampaign(spec.ID)
	if d := findDevice(v, "d1"); d.RollbackTarget != "v1" {
		t.Fatalf("rollback target should be locked at v1, got %q", d.RollbackTarget)
	}

	// 普通上报把版本改成 v2：回滚目标不变
	if err := s.Report("d1", 2, upBase.Add(3*time.Minute), "v2", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	v, _ = s.GetCampaign(spec.ID)
	if d := findDevice(v, "d1"); d.RollbackTarget != "v1" {
		t.Fatalf("rollback target changed after report: %q", d.RollbackTarget)
	}
}

// TestRollbackOperationIDDistinct 验证回滚操作标识与下载、安装不同。
func TestRollbackOperationIDDistinct(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))

	v, _ := s.GetCampaign(spec.ID)
	d := findDevice(v, "d1")
	if d.RollbackID == "" || d.RollbackID == d.DownloadID || d.RollbackID == d.InstallID {
		t.Fatalf("rollback id should be distinct: %+v", d)
	}
	if d.RollbackID != "cmp-1:d1:rollback" {
		t.Fatalf("rollback id: %q", d.RollbackID)
	}
}

// TestRollbackClaimRequiresOnlineAndWindow 验证首次领取回滚须在线且在窗口内。
func TestRollbackClaimRequiresOnlineAndWindow(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))

	// 离线：窗口内也无回滚操作
	s.SetOffline("d1")
	if op, err := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute)); err != nil || op != nil {
		t.Fatalf("offline rollback claim: %v %+v", err, op)
	}
	bringOnline(t, s, "d1")

	// 窗口内：可领取，操作带版本
	rb := claimRollback(t, s, spec, "d1", upBase.Add(10*time.Minute))
	if rb.Version != "v1" {
		t.Fatalf("rollback op should carry target version v1, got %q", rb.Version)
	}

	// 窗口外（窗口 12:00–13:00）再次领取：返回同一标识
	rb2, err := s.Claim(spec.ID, "d1", upBase.Add(70*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if rb2 == nil || rb2.ID != rb.ID {
		t.Fatalf("claimed rollback should return same id outside window: %+v", rb2)
	}
}

// TestRollbackClaimStableIDOutsideWindow 验证已领取未完成的回滚再次领取返回同一标识，
// 且可在窗口外、截止时间前提交结果。
func TestRollbackClaimStableIDOutsideWindow(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb := claimRollback(t, s, spec, "d1", upBase.Add(10*time.Minute))

	// 窗口外、截止前再次领取：返回同一标识
	rb2, err := s.Claim(spec.ID, "d1", upBase.Add(75*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if rb2 == nil || rb2.ID != rb.ID || rb2.Kind != StageRollback {
		t.Fatalf("claimed rollback should return same id: %+v", rb2)
	}

	// 窗口外、截止前提交结果：接受
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(80 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("rollback result outside window: %v", err)
	}
	v, _ := s.GetCampaign(spec.ID)
	if d := findDevice(v, "d1"); d.Status != DeviceRollbackSucceeded {
		t.Fatalf("device should be rollback_succeeded: %+v", d)
	}
}

// TestRollbackSuccessReportRules 验证回滚成功上报的校验规则。
func TestRollbackSuccessReportRules(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb := claimRollback(t, s, spec, "d1", upBase.Add(10*time.Minute))

	submit := func(mutate func(*OperationResult)) error {
		r := OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(11 * time.Minute), Success: true,
			Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
		}
		mutate(&r)
		return s.SubmitResult(r)
	}
	// 缺少序号
	if err := submit(func(r *OperationResult) { r.Seq = 0 }); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("seq 0: %v", err)
	}
	// 版本不符
	if err := submit(func(r *OperationResult) { r.Version = "v2" }); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("wrong version: %v", err)
	}
	// 版本为空
	if err := submit(func(r *OperationResult) { r.Version = "" }); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("empty version: %v", err)
	}
	// 配置非法
	if err := submit(func(r *OperationResult) { r.Config = json.RawMessage(`[]`) }); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("bad config: %v", err)
	}
	// 序号过旧（当前 LastSeq=1，提交 seq=1 且内容不同）
	if err := submit(func(r *OperationResult) { r.Seq = 1 }); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("stale seq: %v", err)
	}
	// 同序号内容冲突：seq=1 但版本/配置不同
	if err := submit(func(r *OperationResult) { r.Seq = 1; r.Version = "v2"; r.Config = json.RawMessage(`{"x":1}`) }); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("seq conflict: %v", err)
	}

	// 校验失败后状态不变
	v, _ := s.GetCampaign(spec.ID)
	if d := findDevice(v, "d1"); d.Status != DeviceRollingBack {
		t.Fatalf("status should be unchanged: %+v", d)
	}
	shadow, _ := s.Get("d1")
	if shadow.Version != "v1" || shadow.LastSeq != 1 {
		t.Fatalf("shadow should be unchanged: %+v", shadow)
	}

	// 合法回滚成功
	if err := submit(func(r *OperationResult) {}); err != nil {
		t.Fatalf("valid rollback: %v", err)
	}
}

// TestRollbackSuccessUpdatesShadow 验证回滚成功更新影子版本与上报配置，
// 保留期望配置、修订号与审计，且不覆盖期望。
func TestRollbackSuccessUpdatesShadow(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	// 设置期望配置
	if _, err := s.UpdateDesired("d1", "alice", upBase, 0, json.RawMessage(`{"mode":"auto"}`)); err != nil {
		t.Fatal(err)
	}
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb := claimRollback(t, s, spec, "d1", upBase.Add(10*time.Minute))

	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(11 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"mode":"manual"}`),
	}); err != nil {
		t.Fatal(err)
	}

	shadow, _ := s.Get("d1")
	if shadow.Version != "v1" || !shadow.Online || shadow.LastSeq != 2 {
		t.Fatalf("shadow after rollback: %+v", shadow)
	}
	if string(shadow.Reported) != `{"mode":"manual"}` {
		t.Fatalf("reported config: %s", shadow.Reported)
	}
	// 期望配置与修订号不变
	if string(shadow.Desired) != `{"mode":"auto"}` || shadow.Revision != 1 {
		t.Fatalf("desired/revision should be preserved: %+v", shadow)
	}
	// 审计保留
	if recs, _ := s.Audit("d1"); len(recs) != 1 {
		t.Fatalf("audit should be preserved: %+v", recs)
	}
	// 配置差异：期望 auto vs 上报 manual，差异存在
	diffs, _ := s.Diff("d1")
	if len(diffs) != 1 || diffs[0].Path != "/mode" {
		t.Fatalf("diffs after rollback: %+v", diffs)
	}
}

// TestRollbackFailureEndsDevice 验证回滚失败使设备以回滚失败结束，影子不变。
func TestRollbackFailureEndsDevice(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb := claimRollback(t, s, spec, "d1", upBase.Add(10*time.Minute))

	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(11 * time.Minute), Success: false, Reason: "rollback broken",
	}); err != nil {
		t.Fatal(err)
	}

	v, _ := s.GetCampaign(spec.ID)
	d := findDevice(v, "d1")
	if d.Status != DeviceRollbackFailed || d.Phase != StageRollback ||
		d.RollbackReason != "rollback broken" || d.RollbackAt.IsZero() {
		t.Fatalf("rollback failure record: %+v", d)
	}
	// 影子不变
	shadow, _ := s.Get("d1")
	if shadow.Version != "v1" || shadow.LastSeq != 1 {
		t.Fatalf("shadow should be unchanged after rollback failure: %+v", shadow)
	}
	if !v.Ended || v.Status != CampaignFailed {
		t.Fatalf("campaign should end failed: %+v", v)
	}
}

// TestRollbackCampaignEndsWhenAllDone 验证活动等待所有设备结束才结束，
// 即使回滚成功活动最终仍失败。
func TestRollbackCampaignEndsWhenAllDone(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2")
	spec := rollbackSpec()
	spec.Devices = []string{"d1", "d2"}
	spec.BatchSize = 2
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))

	// d1 等待回滚时活动未结束
	v, _ := s.GetCampaign(spec.ID)
	if v.Ended {
		t.Fatal("campaign should wait for rollback")
	}
	// d2 仍可继续（同批其他设备）
	if st := deviceStatus(v, "d2"); st != DevicePending {
		t.Fatalf("d2 should continue: %s", st)
	}

	// d1 回滚成功
	rb := claimRollback(t, s, spec, "d1", upBase.Add(10*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(11 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	// d2 完成下载（同批继续）
	dl2, _ := s.Claim(spec.ID, "d2", upBase.Add(12*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d2", OperationID: dl2.ID,
		At: upBase.Add(13 * time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	in2, _ := s.Claim(spec.ID, "d2", upBase.Add(14*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d2", OperationID: in2.ID,
		At: upBase.Add(15 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}

	v, _ = s.GetCampaign(spec.ID)
	if !v.Ended || v.Status != CampaignFailed {
		t.Fatalf("campaign should end failed even with rollback success: %+v", v)
	}
}

// TestDuplicateInstallFailureNoRollback 验证重复提交同一安装失败不产生新回滚。
func TestDuplicateInstallFailureNoRollback(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))

	v1, _ := s.GetCampaign(spec.ID)
	// 重复提交同一安装失败：幂等成功，不增加历史，不改变状态
	inID := "cmp-1:d1:install"
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: inID,
		At: upBase.Add(20 * time.Minute), Success: false, Reason: "install broken",
	}); err != nil {
		t.Fatalf("duplicate install failure: %v", err)
	}
	v2, _ := s.GetCampaign(spec.ID)
	if len(v2.Results) != len(v1.Results) {
		t.Fatal("duplicate install failure added history")
	}
	if d := findDevice(v2, "d1"); d.Status != DeviceRollbackPending {
		t.Fatalf("status should stay rollback_pending: %+v", d)
	}
}

// TestDuplicateRollbackResultIdempotent 验证同一回滚操作的相同结果重复提交成功，
// 不增加历史。
func TestDuplicateRollbackResultIdempotent(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb := claimRollback(t, s, spec, "d1", upBase.Add(10*time.Minute))
	res := OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(11 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"ok":true}`),
	}
	if err := s.SubmitResult(res); err != nil {
		t.Fatal(err)
	}
	v1, _ := s.GetCampaign(spec.ID)
	// 重复提交（时间更晚）：成功，不增加历史
	res.At = upBase.Add(30 * time.Minute)
	if err := s.SubmitResult(res); err != nil {
		t.Fatalf("duplicate rollback result: %v", err)
	}
	v2, _ := s.GetCampaign(spec.ID)
	if len(v2.Results) != len(v1.Results) {
		t.Fatal("duplicate rollback result added history")
	}
}

// TestRollbackDifferentResultConflict 验证同一回滚操作改用不同结果报错且不改状态。
func TestRollbackDifferentResultConflict(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb := claimRollback(t, s, spec, "d1", upBase.Add(10*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(11 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	// 改用失败结果：冲突
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(12 * time.Minute), Success: false, Reason: "changed mind",
	}); !errors.Is(err, ErrResultConflict) {
		t.Fatalf("different rollback result: %v", err)
	}
}

// TestRollbackUnclaimedError 验证提交未领取的回滚结果报错且不改状态。
func TestRollbackUnclaimedError(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	// 未领取回滚即提交结果
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: "cmp-1:d1:rollback",
		At: upBase.Add(10 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	}); !errors.Is(err, ErrOperationNotClaimed) {
		t.Fatalf("unclaimed rollback: %v", err)
	}
	v, _ := s.GetCampaign(spec.ID)
	if d := findDevice(v, "d1"); d.Status != DeviceRollbackPending {
		t.Fatalf("status should stay rollback_pending: %+v", d)
	}
}

// TestRollbackDeadlineTimeout 验证截止时间时未完成回滚的设备以回滚阶段超时结束，
// 不再接收新结果，影子不变；已接受结果的重复提交仍有效。
func TestRollbackDeadlineTimeout(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb := claimRollback(t, s, spec, "d1", upBase.Add(10*time.Minute))

	// 推进到截止时间
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetCampaign(spec.ID)
	if !v.Ended || v.Status != CampaignFailed {
		t.Fatalf("deadline: %+v", v)
	}
	d := findDevice(v, "d1")
	if d.Status != DeviceTimeout || d.Phase != StageRollback || d.Reason == "" || d.At.IsZero() {
		t.Fatalf("rollback timeout record: %+v", d)
	}
	// 不再接收新结果
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(2*time.Hour + time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	}); err == nil {
		t.Fatal("new rollback result after deadline should be rejected")
	}
	// 影子不变
	shadow, _ := s.Get("d1")
	if shadow.Version != "v1" || shadow.LastSeq != 1 {
		t.Fatalf("shadow should be unchanged: %+v", shadow)
	}

	// 已接受结果的重复提交仍有效：先制造一个已接受的回滚失败结果
	s2 := setupUpgrade(t, "d2")
	spec2 := rollbackSpec()
	spec2.ID = "cmp-2"
	spec2.Devices = []string{"d2"}
	spec2.BatchSize = 1
	createCampaign(t, s2, spec2)
	bringOnline(t, s2, "d2")
	failInstallAndWaitRollback(t, s2, spec2, "d2", upBase.Add(2*time.Minute))
	rb2 := claimRollback(t, s2, spec2, "d2", upBase.Add(10*time.Minute))
	if err := s2.SubmitResult(OperationResult{
		CampaignID: spec2.ID, DeviceID: "d2", OperationID: rb2.ID,
		At: upBase.Add(11 * time.Minute), Success: false, Reason: "rollback broken",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s2.AdvanceCampaign(spec2.ID, upBase.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	// 已接受的回滚失败结果重复提交仍有效
	if err := s2.SubmitResult(OperationResult{
		CampaignID: spec2.ID, DeviceID: "d2", OperationID: rb2.ID,
		At: upBase.Add(3 * time.Hour), Success: false, Reason: "rollback broken",
	}); err != nil {
		t.Fatalf("duplicate after deadline: %v", err)
	}
}

// TestRollbackCampaignView 验证活动查询能分别看到安装失败和回滚结果的原因、时间。
func TestRollbackCampaignView(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))

	v, _ := s.GetCampaign(spec.ID)
	d := findDevice(v, "d1")
	if d.Status != DeviceRollbackPending || d.Phase != StageRollback {
		t.Fatalf("status: %+v", d)
	}
	if d.InstallFailReason != "install broken" || d.InstallFailAt.IsZero() {
		t.Fatalf("install failure record: %+v", d)
	}
	if d.RollbackReason != "" || !d.RollbackAt.IsZero() {
		t.Fatalf("rollback should have no result yet: %+v", d)
	}
	if d.RollbackClaimed {
		t.Fatal("rollback should not be claimed yet")
	}
}

// TestRollbackDeviceWork 验证设备待办查询显示回滚目标和是否已领取。
func TestRollbackDeviceWork(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))

	// 未领取回滚
	w, _ := s.GetDeviceWork("d1")
	if w.Pending == nil || w.Pending.Kind != StageRollback || w.PendingClaimed {
		t.Fatalf("pending rollback: %+v", w)
	}
	if w.RollbackTarget != "v1" || w.Pending.Version != "v1" {
		t.Fatalf("rollback target: %+v", w)
	}

	// 领取后
	claimRollback(t, s, spec, "d1", upBase.Add(10*time.Minute))
	w, _ = s.GetDeviceWork("d1")
	if w.Pending == nil || w.Pending.Kind != StageRollback || !w.PendingClaimed {
		t.Fatalf("claimed rollback: %+v", w)
	}
	if w.RollbackTarget != "v1" {
		t.Fatalf("rollback target after claim: %+v", w)
	}
}

// TestRollbackPersistenceReopen 验证关闭后重新打开保留目标、进度、操作标识和重复判断。
func TestRollbackPersistenceReopen(t *testing.T) {
	s, dir := setupUpgradeDir(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb := claimRollback(t, s, spec, "d1", upBase.Add(10*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(11 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"ok":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()

	v, err := s2.GetCampaign(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !v.RollbackOnFailure {
		t.Fatal("rollback flag not preserved")
	}
	d := findDevice(v, "d1")
	if d.Status != DeviceRollbackSucceeded || d.RollbackTarget != "v1" ||
		d.RollbackID != "cmp-1:d1:rollback" || d.RollbackReason != "" || d.RollbackAt.IsZero() {
		t.Fatalf("rollback state after reopen: %+v", d)
	}
	// 影子恢复
	shadow, _ := s2.Get("d1")
	if shadow.Version != "v1" || shadow.LastSeq != 2 {
		t.Fatalf("shadow after reopen: %+v", shadow)
	}
	// 重复判断延续：重放回滚成功不增加历史
	if err := s2.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(20 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("replay after reopen: %v", err)
	}
	v2, _ := s2.GetCampaign(spec.ID)
	if len(v2.Results) != len(v.Results) {
		t.Fatal("replay added history after reopen")
	}
}

// TestRollbackSaveFailureRollback 验证保存失败时影子、活动、历史全部保持操作前结果。
func TestRollbackSaveFailureRollback(t *testing.T) {
	s, dir := setupUpgradeDir(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb := claimRollback(t, s, spec, "d1", upBase.Add(10*time.Minute))

	// 堵住存储文件
	if err := os.Remove(filepath.Join(dir, storeFileName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, storeFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	// 回滚成功涉及影子+活动+历史：保存失败全部保持原状
	err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(11 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"x":1}`),
	})
	if err == nil {
		t.Fatal("expected save failure")
	}
	shadow, _ := s.Get("d1")
	if shadow.Version != "v1" || shadow.LastSeq != 1 {
		t.Fatalf("shadow not rolled back: %+v", shadow)
	}
	v, _ := s.GetCampaign(spec.ID)
	if d := findDevice(v, "d1"); d.Status != DeviceRollingBack {
		t.Fatalf("campaign not rolled back: %+v", d)
	}
	// 恢复后重提成功
	if err := os.Remove(filepath.Join(dir, storeFileName)); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(12 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"x":1}`),
	}); err != nil {
		t.Fatalf("retry after recovery: %v", err)
	}
}

// TestRollbackDeviceBusy 验证等待回滚或正在回滚的设备不能参加其他活动。
func TestRollbackDeviceBusy(t *testing.T) {
	s := setupUpgrade(t, "d1")
	if err := s.RegisterUpgrade("v3", []string{"v1"}); err != nil {
		t.Fatal(err)
	}
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))

	// 设备等待回滚：不能参加新活动
	spec2 := upSpec()
	spec2.ID = "cmp-2"
	spec2.TargetVersion = "v3"
	spec2.Devices = []string{"d1"}
	spec2.BatchSize = 1
	if err := s.CreateCampaign(spec2); !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("device in rollback should be busy: %v", err)
	}
}

// TestRollbackDownloadFailureNoRollback 验证下载失败不产生回滚。
func TestRollbackDownloadFailureNoRollback(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(3 * time.Minute), Success: false, Reason: "download broken",
	}); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetCampaign(spec.ID)
	d := findDevice(v, "d1")
	if d.Status != DeviceFailed || d.Phase != StageDownload {
		t.Fatalf("download failure should fail device directly: %+v", d)
	}
	// 下载失败不产生回滚：回滚操作未被领取，设备直接失败
	if d.RollbackClaimed {
		t.Fatalf("download failure should not produce rollback: %+v", d)
	}
}

// TestRollbackIncompatibleBeforeClaimNoRollback 验证领取前版本不兼容不产生回滚。
func TestRollbackIncompatibleBeforeClaimNoRollback(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	// 普通上报把版本改成不兼容的 v3
	if err := s.Report("d1", 2, upBase.Add(2*time.Minute), "v3", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(3*time.Minute)); !errors.Is(err, ErrIncompatibleVersion) {
		t.Fatalf("incompatible: %v", err)
	}
	v, _ := s.GetCampaign(spec.ID)
	d := findDevice(v, "d1")
	if d.Status != DeviceFailed || d.Phase != StageDownload {
		t.Fatalf("incompatible should fail device: %+v", d)
	}
	if d.RollbackTarget != "" {
		t.Fatalf("incompatible should not lock rollback target: %+v", d)
	}
}

// TestRollbackSkipsSubsequentBatches 验证安装失败后后续批次立即记为未执行。
func TestRollbackSkipsSubsequentBatches(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2", "d3")
	spec := rollbackSpec()
	spec.Devices = []string{"d1", "d2", "d3"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	bringOnline(t, s, "d3")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))

	v, _ := s.GetCampaign(spec.ID)
	for _, id := range []string{"d2", "d3"} {
		if st := deviceStatus(v, id); st != DeviceSkipped {
			t.Fatalf("%s should be skipped: %s", id, st)
		}
	}
	// d1 等待回滚，活动未结束
	if v.Ended {
		t.Fatal("campaign should wait for rollback")
	}
}

// TestRollbackPlainReportDoesNotClearTodo 验证回滚未完成时普通上报只更新影子，
// 不替代回滚结果或清除待办。
func TestRollbackPlainReportDoesNotClearTodo(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rollbackSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	failInstallAndWaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))

	// 普通上报更新影子版本
	if err := s.Report("d1", 2, upBase.Add(5*time.Minute), "v2", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	// 回滚待办仍在，回滚目标不变
	w, _ := s.GetDeviceWork("d1")
	if w.Pending == nil || w.Pending.Kind != StageRollback || w.RollbackTarget != "v1" {
		t.Fatalf("rollback todo should remain: %+v", w)
	}
	v, _ := s.GetCampaign(spec.ID)
	if d := findDevice(v, "d1"); d.Status != DeviceRollbackPending {
		t.Fatalf("status should stay rollback_pending: %+v", d)
	}
}

// TestOldStorageOpensWithoutRollback 验证旧存储（无回滚字段）可直接打开。
func TestOldStorageOpensWithoutRollback(t *testing.T) {
	dir := t.TempDir()
	old := `{"format":1,"devices":{"d1":{"version":"v1","online":false,"revision":0,"desired":{},"reported":{},"lastSeq":0}},"versions":{"v2":{"target":"v2","allowedFrom":["v1"]}},"campaigns":{"cmp-1":{"id":"cmp-1","operator":"alice","target":"v2","createdAt":"2026-10-02T12:00:00Z","batchSize":1,"windowStart":"2026-10-02T12:00:00Z","windowEnd":"2026-10-02T13:00:00Z","deadline":"2026-10-02T14:00:00Z","lastTime":"2026-10-02T12:00:00Z","status":"running","ended":false,"devices":[{"deviceId":"d1","batch":0,"status":"pending","phase":"download","download":{"id":"cmp-1:d1:download"},"install":{"id":"cmp-1:d1:install"}}]}}}`
	if err := os.WriteFile(filepath.Join(dir, storeFileName), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("old store with campaign: %v", err)
	}
	defer s.Close()
	v, err := s.GetCampaign("cmp-1")
	if err != nil {
		t.Fatal(err)
	}
	if v.RollbackOnFailure {
		t.Fatal("old campaign should have rollback disabled")
	}
	d := findDevice(v, "d1")
	if d.RollbackID != "" || d.RollbackTarget != "" {
		t.Fatalf("old campaign should have no rollback fields: %+v", d)
	}
}
