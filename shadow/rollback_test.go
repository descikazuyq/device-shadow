package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// rbSpec 返回一个开启安装失败回滚的单设备活动规格（窗口 12:00–13:00，截止 14:00）。
func rbSpec() CampaignSpec {
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	spec.RollbackOnFailure = true
	return spec
}

// awaitRollback 完成下载并令安装失败，使开启回滚活动中的设备进入等待回滚。
// 时间线（相对 at）：领取下载 at、下载成功 at+1m、领取安装 at+2m、安装失败 at+3m。
// 返回安装操作标识与安装失败被接受的时间。
func awaitRollback(t *testing.T, s *Store, spec CampaignSpec, id string, at time.Time) (string, time.Time) {
	t.Helper()
	dl, err := s.Claim(spec.ID, id, at)
	if err != nil || dl == nil || dl.Kind != StageDownload {
		t.Fatalf("claim download: %v %+v", err, dl)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: id, OperationID: dl.ID,
		At: at.Add(time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("download result: %v", err)
	}
	in, err := s.Claim(spec.ID, id, at.Add(2*time.Minute))
	if err != nil || in == nil || in.Kind != StageInstall {
		t.Fatalf("claim install: %v %+v", err, in)
	}
	failAt := at.Add(3 * time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: id, OperationID: in.ID,
		At: failAt, Success: false, Reason: "install broken",
	}); err != nil {
		t.Fatalf("install failure: %v", err)
	}
	return in.ID, failAt
}

// 未开启回滚（默认）时安装失败沿用现有行为：设备直接失败，不锁定/等待回滚。
func TestRollbackDisabledKeepsLegacyBehavior(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")

	_, failAt := awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	v, _ := s.GetCampaign(spec.ID)
	d := findDevice(v, "d1")
	if d.Status != DeviceFailed || d.Phase != StageInstall {
		t.Fatalf("legacy install failure: %+v", d)
	}
	if d.RollbackTarget != "" || d.RollbackResult {
		t.Fatalf("legacy failure must not carry rollback state: %+v", d)
	}
	if !v.Ended || v.Status != CampaignFailed || !d.At.Equal(failAt) {
		t.Fatalf("legacy campaign: %+v", v)
	}
	if v.RollbackOnFailure {
		t.Fatal("view should report rollback disabled")
	}
	w, _ := s.GetDeviceWork("d1")
	if w.Pending != nil || w.CampaignID != "" {
		t.Fatalf("finished campaign has no work: %+v", w)
	}
}

// 首次领取下载时锁定设备当前版本为回滚目标；此后普通上报不得改变它。
func TestRollbackLocksVersionOnFirstDownloadClaim(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rbSpec()
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")

	if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.RollbackTarget != "" {
		t.Fatalf("target must be empty before download claim: %+v", d)
	}
	dl, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	if err != nil || dl == nil {
		t.Fatalf("claim: %v %+v", err, dl)
	}
	if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.RollbackTarget != "v1" {
		t.Fatalf("target locked at first download claim: %+v", d)
	}
	// 下载进行中普通上报把版本改成别的：锁定目标不变，也不要求该版本登记。
	if err := s.Report("d1", 2, upBase.Add(3*time.Minute), "v9-other", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.RollbackTarget != "v1" {
		t.Fatalf("plain report changed locked target: %+v", d)
	}
	if st := deviceStatus(mustGetCampaign(s, spec.ID), "d1"); st != DeviceDownloading {
		t.Fatalf("plain report must not advance campaign: %s", st)
	}
}

// 安装失败进入等待回滚：保留原因/时间；同批继续、后续批次立即 skipped；
// 活动要等回滚设备结束；即使回滚成功，活动最终仍失败。
func TestInstallFailureAwaitsRollbackAndCascades(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2", "d3")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2", "d3"}
	spec.BatchSize = 2
	spec.RollbackOnFailure = true
	createCampaign(t, s, spec)
	for _, id := range spec.Devices {
		bringOnline(t, s, id)
	}

	_, failAt := awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	v, _ := s.GetCampaign(spec.ID)
	d1 := findDevice(v, "d1")
	if d1.Status != DeviceAwaitingRollback || d1.Phase != StageRollback {
		t.Fatalf("d1 should await rollback: %+v", d1)
	}
	if d1.InstallFailReason != "install broken" || !d1.InstallFailAt.Equal(failAt) {
		t.Fatalf("install failure reason/time kept: %+v", d1)
	}
	if v.Ended {
		t.Fatal("campaign must wait for rollback device")
	}
	if st := deviceStatus(v, "d3"); st != DeviceSkipped {
		t.Fatalf("later batch skipped at once: %s", st)
	}
	// 等待回滚的设备不能参加其他活动。
	other := spec
	other.ID = "cmp-other"
	other.CreatedAt = upBase.Add(time.Minute)
	other.Devices = []string{"d1"}
	if err := s.CreateCampaign(other); !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("awaiting device busy: %v", err)
	}

	// 同批 d2 可继续成功；活动仍因 d1 未结束而运行。
	finishDevice(t, s, spec, "d2", upBase.Add(10*time.Minute))
	if v, _ = s.GetCampaign(spec.ID); v.Ended {
		t.Fatal("campaign still waits for d1 rollback")
	}

	// d1 领取并完成回滚（窗口外、截止前提交）。
	rb, err := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute))
	if err != nil || rb == nil || rb.Kind != StageRollback || rb.TargetVersion != "v1" {
		t.Fatalf("claim rollback: %v %+v", err, rb)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(90 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"rollback":true}`),
	}); err != nil {
		t.Fatalf("rollback result: %v", err)
	}
	v, _ = s.GetCampaign(spec.ID)
	d1 = findDevice(v, "d1")
	if d1.Status != DeviceRollbackSucceeded {
		t.Fatalf("rollback succeeded status: %+v", d1)
	}
	if !v.Ended || v.Status != CampaignFailed {
		t.Fatalf("campaign fails even with successful rollback: %+v", v)
	}
}

// 下载失败、领取前版本不兼容都按现有方式结束，不产生回滚。
func TestDownloadFailureAndIncompatibleNoRollback(t *testing.T) {
	// 下载失败。
	s := setupUpgrade(t, "d1")
	spec := rbSpec()
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(3 * time.Minute), Success: false, Reason: "dl down",
	}); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetCampaign(spec.ID)
	d := findDevice(v, "d1")
	if d.Status != DeviceFailed || d.Phase != StageDownload || d.RollbackResult {
		t.Fatalf("download failure ends without rollback: %+v", d)
	}
	if !v.Ended {
		t.Fatal("campaign ends on download failure")
	}

	// 领取前不兼容：普通上报把版本改成不兼容版本，领取即失败，无回滚。
	s2, dir2 := setupUpgradeDir(t, "x1")
	spec2 := rbSpec()
	spec2.ID = "cmp-2"
	spec2.Devices = []string{"x1"}
	createCampaign(t, s2, spec2)
	bringOnline(t, s2, "x1")
	if err := s2.Report("x1", 2, upBase.Add(time.Minute), "v9", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s2.Claim(spec2.ID, "x1", upBase.Add(2*time.Minute)); !errors.Is(err, ErrIncompatibleVersion) {
		t.Fatalf("incompatible claim: %v", err)
	}
	v2, _ := s2.GetCampaign(spec2.ID)
	x := findDevice(v2, "x1")
	if x.Status != DeviceFailed || x.Phase != StageDownload ||
		x.RollbackResult || x.RollbackTarget != "" {
		t.Fatalf("incompatible before claim must not rollback: %+v", x)
	}
	// 该边界状态（已领取下载但未锁定目标）必须能正常持久化重开。
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3, err := Open(dir2)
	if err != nil {
		t.Fatalf("reopen incompatible-claim store: %v", err)
	}
	defer s3.Close()
	v3, _ := s3.GetCampaign(spec2.ID)
	if x3 := findDevice(v3, "x1"); x3.Status != DeviceFailed || x3.RollbackTarget != "" {
		t.Fatalf("state after reopen: %+v", x3)
	}
}

// 回滚首次领取要求在线且位于窗口 [start,end)；离线保留待办；
// 待办查询显示回滚目标与未领取；已领取操作再次查询（含窗口外）返回同一标识。
func TestRollbackClaimWindowOnlineAndStableID(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rbSpec()
	spec.WindowEnd = upBase.Add(30 * time.Minute) // 窗口 12:00–12:30
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	inID, _ := awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))

	// 离线且位于窗口内：保留待办。
	if err := s.SetOffline("d1"); err != nil {
		t.Fatal(err)
	}
	if op, err := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute)); err != nil || op != nil {
		t.Fatalf("offline rollback waits: %v %+v", err, op)
	}
	w, _ := s.GetDeviceWork("d1")
	if w.Pending == nil || w.Pending.Kind != StageRollback || w.PendingClaimed {
		t.Fatalf("pending rollback unclaimed: %+v", w)
	}
	if w.Pending.TargetVersion != "v1" {
		t.Fatalf("work shows rollback target: %+v", w.Pending)
	}
	// 上线（普通上报不改变回滚待办）。
	if err := s.Report("d1", 2, upBase.Add(11*time.Minute), "v1", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	// 仍在窗口内：可领取。
	rb, err := s.Claim(spec.ID, "d1", upBase.Add(12*time.Minute))
	if err != nil || rb == nil {
		t.Fatalf("claim rollback in window: %v %+v", err, rb)
	}
	if rb.Kind != StageRollback || rb.TargetVersion != "v1" || rb.ID != "cmp-1:d1:rollback" {
		t.Fatalf("rollback op: %+v", rb)
	}
	if rb.ID == inID || rb.ID == "cmp-1:d1:download" {
		t.Fatalf("rollback id must differ from download/install: %s", rb.ID)
	}
	// 正在回滚的设备同样不能参加其他活动。
	other := spec
	other.ID = "cmp-other"
	other.CreatedAt = upBase.Add(time.Minute)
	if err := s.CreateCampaign(other); !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("rolling-back device busy: %v", err)
	}
	// 窗口外再次查询：返回同一已领取标识与锁定目标。
	again, err := s.Claim(spec.ID, "d1", upBase.Add(45*time.Minute))
	if err != nil || again == nil || again.ID != rb.ID || again.TargetVersion != "v1" {
		t.Fatalf("claimed rollback returns same id: %v %+v", err, again)
	}
}

// 设备在线但窗口外时，回滚首次领取保留待办（未领取、带目标版本）。
func TestRollbackClaimWaitsOutsideWindow(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rbSpec()
	spec.WindowEnd = upBase.Add(30 * time.Minute)
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))

	// 窗口结束之后、截止之前：在线也暂不派发回滚。
	if op, err := s.Claim(spec.ID, "d1", upBase.Add(45*time.Minute)); err != nil || op != nil {
		t.Fatalf("rollback waits outside window: %v %+v", err, op)
	}
	if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceAwaitingRollback {
		t.Fatalf("still awaiting: %+v", d)
	}
	w, _ := s.GetDeviceWork("d1")
	if w.Pending == nil || w.Pending.Kind != StageRollback || w.PendingClaimed ||
		w.Pending.TargetVersion != "v1" {
		t.Fatalf("unclaimed rollback todo outside window: %+v", w)
	}
}

// 回滚成功附带的上报沿用序号/重复规则，版本必须等于锁定目标；
// 接受后更新当前版本与上报配置与回滚状态，保留期望配置、修订号与审计。
func TestRollbackSuccessReportAndShadow(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rbSpec()
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	// 先设置期望配置，回滚后不得被升级前/上报配置覆盖。
	rev, err := s.UpdateDesired("d1", "bob", upBase.Add(-2*time.Minute), 0, json.RawMessage(`{"x":1}`))
	if err != nil || rev != 1 {
		t.Fatalf("update desired: %v %d", err, rev)
	}
	awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb, _ := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute))

	bad := func(mut func(*OperationResult)) {
		t.Helper()
		r := OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(9 * time.Minute), Success: true,
			Seq: 2, Version: "v1", Config: json.RawMessage(`{"x":2}`),
		}
		mut(&r)
		if err := s.SubmitResult(r); !errors.Is(err, ErrInvalidReport) {
			t.Fatalf("want ErrInvalidReport, got %v", err)
		}
	}
	bad(func(r *OperationResult) { r.Seq = 0 })
	bad(func(r *OperationResult) { r.Version = "v2" })
	bad(func(r *OperationResult) { r.Version = "" })
	bad(func(r *OperationResult) { r.Config = json.RawMessage(`[]`) })
	bad(func(r *OperationResult) { r.Seq = 1; r.Config = json.RawMessage(`{"x":9}`) }) // 同序号内容冲突

	// 所有拒绝都不改影子、活动与历史。
	view, _ := s.Get("d1")
	if view.Version != "v1" || view.LastSeq != 1 || view.Revision != 1 {
		t.Fatalf("shadow changed on rejected rollback: %+v", view)
	}
	cv, _ := s.GetCampaign(spec.ID)
	if findDevice(cv, "d1").Status != DeviceRollingBack {
		t.Fatalf("campaign changed on rejected rollback: %+v", findDevice(cv, "d1"))
	}
	historyBefore := len(cv.Results)

	// 合法回滚：窗口外、截止前提交。上报配置引入新的 /y 差异。
	rbAt := upBase.Add(90 * time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: rbAt, Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"x":2,"y":9}`),
	}); err != nil {
		t.Fatalf("valid rollback: %v", err)
	}
	view, _ = s.Get("d1")
	if view.Version != "v1" || !view.Online || view.LastSeq != 2 || string(view.Reported) != `{"x":2,"y":9}` {
		t.Fatalf("shadow after rollback: %+v", view)
	}
	// 期望配置、修订号与审计保留。
	if view.Revision != 1 || string(view.Desired) != `{"x":1}` {
		t.Fatalf("desired/revision changed: %+v", view)
	}
	if auds, _ := s.Audit("d1"); len(auds) != 1 {
		t.Fatalf("audit changed: %+v", auds)
	}
	// 配置差异按现有规则变化：/x 早已存在故保留原首次出现时间；
	// /y 由回滚上报新引入，首次出现时间为回滚上报时间；不拿升级前配置覆盖期望。
	diffs, _ := s.Diff("d1")
	got := map[string]DiffEntry{}
	for _, e := range diffs {
		got[e.Path] = e
	}
	if len(got) != 2 {
		t.Fatalf("diff after rollback: %+v", diffs)
	}
	if dx, ok := got["/x"]; !ok || !dx.Since.Equal(upBase.Add(-2*time.Minute)) {
		t.Fatalf("/x keeps original since: %+v", got)
	}
	if dy, ok := got["/y"]; !ok || !dy.ReportedExists || !dy.Since.Equal(rbAt) {
		t.Fatalf("/y first appears at rollback time: %+v", got)
	}
	cv, _ = s.GetCampaign(spec.ID)
	if len(cv.Results) != historyBefore+1 {
		t.Fatalf("history should grow by one rollback result")
	}
	if !cv.Ended || cv.Status != CampaignFailed {
		t.Fatalf("campaign failed after rollback success: %+v", cv)
	}
}

// 回滚失败必须给出原因；设备以回滚失败结束且影子不变。
func TestRollbackFailureRequiresReasonAndKeepsShadow(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rbSpec()
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	_, failAt := awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb, _ := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute))

	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(9 * time.Minute), Success: false,
	}); !errors.Is(err, ErrInvalidReason) {
		t.Fatalf("rollback failure needs reason: %v", err)
	}
	rbFailAt := upBase.Add(10 * time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: rbFailAt, Success: false, Reason: "rb boom",
	}); err != nil {
		t.Fatalf("rollback failure: %v", err)
	}
	v, _ := s.Get("d1")
	if v.Version != "v1" || v.LastSeq != 1 {
		t.Fatalf("shadow must not change on rollback failure: %+v", v)
	}
	cv, _ := s.GetCampaign(spec.ID)
	d := findDevice(cv, "d1")
	if d.Status != DeviceRollbackFailed || d.Phase != StageRollback ||
		d.RollbackReason != "rb boom" || !d.RollbackAt.Equal(rbFailAt) {
		t.Fatalf("rollback failure record: %+v", d)
	}
	// 安装失败与回滚结果的原因/时间可分别查看。
	if d.InstallFailReason != "install broken" || !d.InstallFailAt.Equal(failAt) {
		t.Fatalf("install failure kept separately: %+v", d)
	}
	if !d.RollbackResult || d.RollbackSuccess {
		t.Fatalf("rollback result flags: %+v", d)
	}
	if !cv.Ended || cv.Status != CampaignFailed {
		t.Fatalf("campaign: %+v", cv)
	}
}

// 回滚幂等、冲突与未领取规则。
func TestRollbackIdempotencyConflictUnclaimed(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rbSpec()
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	inID, _ := awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))

	// 提交未领取的回滚操作：报错且不改状态。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: "cmp-1:d1:rollback",
		At: upBase.Add(8 * time.Minute), Success: false, Reason: "early",
	}); !errors.Is(err, ErrOperationNotClaimed) {
		t.Fatalf("unclaimed rollback: %v", err)
	}
	// 重复提交同一安装失败：不再产生回滚，不增加历史。
	cv, _ := s.GetCampaign(spec.ID)
	n := len(cv.Results)
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: inID,
		At: upBase.Add(40 * time.Minute), Success: false, Reason: "install broken",
	}); err != nil {
		t.Fatalf("duplicate install failure: %v", err)
	}
	cv, _ = s.GetCampaign(spec.ID)
	if len(cv.Results) != n || findDevice(cv, "d1").Status != DeviceAwaitingRollback {
		t.Fatalf("duplicate install failure altered state: %+v", cv)
	}

	// 领取回滚并提交失败。
	rb, _ := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(9 * time.Minute), Success: false, Reason: "rb boom",
	}); err != nil {
		t.Fatalf("rollback failure: %v", err)
	}
	// 相同结果重复提交：成功但不增加历史。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(60 * time.Minute), Success: false, Reason: "rb boom",
	}); err != nil {
		t.Fatalf("duplicate rollback result: %v", err)
	}
	cv, _ = s.GetCampaign(spec.ID)
	if len(cv.Results) != n+1 {
		t.Fatalf("duplicate rollback added history: %+v", cv.Results)
	}
	// 改成不同结果（失败改成功）：冲突且不改状态。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(61 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	}); !errors.Is(err, ErrResultConflict) {
		t.Fatalf("changed rollback result: %v", err)
	}
	if d := findDevice(mustGetCampaign(s, spec.ID), "d1"); d.Status != DeviceRollbackFailed {
		t.Fatalf("state changed after conflict: %+v", d)
	}
}

// 到达原截止时间：等待/正在回滚的设备以回滚阶段超时结束，不改写影子；
// 不再接收新结果；已接受结果的重复提交仍有效。
func TestRollbackDeadlineTimeout(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2"}
	spec.BatchSize = 2
	spec.RollbackOnFailure = true
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")

	// d1 领取回滚并成功；d2 安装失败后停在等待回滚。
	awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb1, _ := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute))
	rb1At := upBase.Add(9 * time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb1.ID,
		At: rb1At, Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("d1 rollback: %v", err)
	}
	awaitRollback(t, s, spec, "d2", upBase.Add(12*time.Minute))
	// d2 领取回滚但不完成（正在回滚）。
	rb2, _ := s.Claim(spec.ID, "d2", upBase.Add(20*time.Minute))

	if err := s.AdvanceCampaign(spec.ID, upBase.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetCampaign(spec.ID)
	if !v.Ended || v.Status != CampaignFailed {
		t.Fatalf("deadline ends campaign failed: %+v", v)
	}
	d2 := findDevice(v, "d2")
	if d2.Status != DeviceRollbackTimeout || d2.Phase != StageRollback || d2.Reason == "" {
		t.Fatalf("rolling-back device rollback-timeout: %+v", d2)
	}
	d1 := findDevice(v, "d1")
	if d1.Status != DeviceRollbackSucceeded {
		t.Fatalf("finished rollback kept: %+v", d1)
	}
	// 影子不被超时改写。
	if sh, _ := s.Get("d2"); sh.Version != "v1" || sh.LastSeq != 1 {
		t.Fatalf("shadow changed on timeout: %+v", sh)
	}
	// 不再接收新结果。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d2", OperationID: rb2.ID,
		At: upBase.Add(2*time.Hour + time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	}); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("new rollback result after deadline: %v", err)
	}
	// 已接受结果的重复提交仍有效，不增加历史。
	n := len(v.Results)
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb1.ID,
		At: upBase.Add(3 * time.Hour), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("accepted result replay after end: %v", err)
	}
	if cv, _ := s.GetCampaign(spec.ID); len(cv.Results) != n {
		t.Fatal("replay after deadline added history")
	}
}

// 回滚未完成时的普通上报只更新影子，不替代回滚结果或清除待办；
// 回滚最终仍必须恢复到锁定版本。
func TestPlainReportDuringRollbackOnlyUpdatesShadow(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rbSpec()
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))

	// 等待回滚期间普通上报把版本/配置改成别的：影子更新，回滚待办不变。
	if err := s.Report("d1", 2, upBase.Add(6*time.Minute), "v-stray", json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if sh, _ := s.Get("d1"); sh.Version != "v-stray" || sh.LastSeq != 2 {
		t.Fatalf("plain report should update shadow: %+v", sh)
	}
	cv, _ := s.GetCampaign(spec.ID)
	d := findDevice(cv, "d1")
	if d.Status != DeviceAwaitingRollback || d.RollbackTarget != "v1" || d.RollbackResult {
		t.Fatalf("plain report must not clear rollback todo: %+v", d)
	}
	w, _ := s.GetDeviceWork("d1")
	if w.Pending == nil || w.Pending.Kind != StageRollback || w.PendingClaimed ||
		w.Pending.TargetVersion != "v1" {
		t.Fatalf("rollback todo survives plain report: %+v", w)
	}
	// 回滚成功以更大序号、锁定版本 v1 接受：影子回到锁定版本与上报配置。
	rb, _ := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(9 * time.Minute), Success: true,
		Seq: 3, Version: "v1", Config: json.RawMessage(`{"a":0}`),
	}); err != nil {
		t.Fatalf("rollback to locked target: %v", err)
	}
	if sh, _ := s.Get("d1"); sh.Version != "v1" || sh.LastSeq != 3 || string(sh.Reported) != `{"a":0}` {
		t.Fatalf("shadow restored to locked target: %+v", sh)
	}
}

// 关闭重开保留回滚目标、进度、操作标识与重复判断，并可继续完成。
func TestRollbackPersistenceReopen(t *testing.T) {
	s, dir := openTemp(t)
	if err := s.RegisterUpgrade("v2", []string{"v1"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "d1", "v1")
	mustRegister(t, s, "d2", "v1")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2"}
	spec.BatchSize = 2
	spec.RollbackOnFailure = true
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	_, failAt := awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rbBefore, _ := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute)) // 正在回滚
	// d2 停在等待回滚。
	awaitRollback(t, s, spec, "d2", upBase.Add(12*time.Minute))
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
		t.Fatal("rollback flag lost after reopen")
	}
	d1 := findDevice(v, "d1")
	if d1.Status != DeviceRollingBack || d1.RollbackTarget != "v1" ||
		d1.RollbackID != "cmp-1:d1:rollback" || d1.InstallFailAt.IsZero() {
		t.Fatalf("d1 rollback progress after reopen: %+v", d1)
	}
	if !d1.InstallFailAt.Equal(failAt) {
		t.Fatalf("install failure time after reopen: %v", d1.InstallFailAt)
	}
	d2 := findDevice(v, "d2")
	if d2.Status != DeviceAwaitingRollback || d2.RollbackTarget != "v1" {
		t.Fatalf("d2 awaiting after reopen: %+v", d2)
	}
	// d2 待办显示回滚目标、未领取。
	w, _ := s2.GetDeviceWork("d2")
	if w.Pending == nil || w.Pending.Kind != StageRollback || w.PendingClaimed ||
		w.Pending.TargetVersion != "v1" {
		t.Fatalf("d2 work after reopen: %+v", w)
	}
	// d1 已领取的回滚再次领取仍是同一标识。
	again, err := s2.Claim(spec.ID, "d1", upBase.Add(85*time.Minute))
	if err != nil || again == nil || again.ID != rbBefore.ID {
		t.Fatalf("stable rollback id after reopen: %v %+v", err, again)
	}
	// 时间倒退判断延续。
	if err := s2.AdvanceCampaign(spec.ID, upBase.Add(7*time.Minute)); !errors.Is(err, ErrTimeRegression) {
		t.Fatalf("regression after reopen: %v", err)
	}
	// 重复结果判断延续：回滚成功只接受一次，重放不增加历史。
	res := OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rbBefore.ID,
		At: upBase.Add(86 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"ok":1}`),
	}
	if err := s2.SubmitResult(res); err != nil {
		t.Fatalf("rollback after reopen: %v", err)
	}
	n := len(mustGetCampaign(s2, spec.ID).Results)
	res.At = upBase.Add(87 * time.Minute)
	if err := s2.SubmitResult(res); err != nil {
		t.Fatalf("rollback replay: %v", err)
	}
	if len(mustGetCampaign(s2, spec.ID).Results) != n {
		t.Fatal("rollback replay added history after reopen")
	}
	if sh, _ := s2.Get("d1"); sh.Version != "v1" {
		t.Fatalf("shadow version after reopen rollback: %s", sh.Version)
	}
}

// 回滚成功涉及影子、活动与历史；保存失败时三者全部保持操作前结果。
func TestRollbackSaveFailureRollback(t *testing.T) {
	s, dir := setupUpgradeDir(t, "d1")
	spec := rbSpec()
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb, _ := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute))

	if err := os.Remove(filepath.Join(dir, storeFileName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, storeFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(9 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"x":1}`),
	})
	if err == nil {
		t.Fatal("expected save failure")
	}
	// 影子回滚。
	if sh, _ := s.Get("d1"); sh.Version != "v1" || sh.LastSeq != 1 {
		t.Fatalf("shadow not rolled back: %+v", sh)
	}
	cv, _ := s.GetCampaign(spec.ID)
	d := findDevice(cv, "d1")
	if d.Status != DeviceRollingBack || d.RollbackResult {
		t.Fatalf("campaign not rolled back: %+v", d)
	}
	// 历史中只有下载成功、安装失败两条，没有回滚结果。
	if len(cv.Results) != 2 {
		t.Fatalf("history not rolled back: %+v", cv.Results)
	}
	// 恢复后用原操作标识重提仍成功。
	if err := os.Remove(filepath.Join(dir, storeFileName)); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(10 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"x":1}`),
	}); err != nil {
		t.Fatalf("retry after recovery: %v", err)
	}
	if sh, _ := s.Get("d1"); sh.Version != "v1" || sh.LastSeq != 2 {
		t.Fatalf("shadow after retry: %+v", sh)
	}
}

// 旧存储中的活动（无任何回滚字段）可直接打开，回滚缺省关闭并保持旧行为。
func TestOldCampaignStorageRollbackDefaultsOff(t *testing.T) {
	dir := t.TempDir()
	old := `{
 "format":1,
 "devices":{"d1":{"version":"v1","online":true,"revision":0,"desired":{},"reported":{},"lastSeq":1,"lastReportTime":"2026-10-02T11:59:00Z"}},
 "versions":{"v2":{"target":"v2","allowedFrom":["v1"]}},
 "campaigns":{"c1":{"id":"c1","operator":"alice","target":"v2","createdAt":"2026-10-02T12:00:00Z","batchSize":1,"windowStart":"2026-10-02T12:00:00Z","windowEnd":"2026-10-02T13:00:00Z","deadline":"2026-10-02T14:00:00Z","lastTime":"2026-10-02T12:00:00Z","status":"running","ended":false,"devices":[{"deviceId":"d1","batch":0,"status":"pending","phase":"download","download":{"id":"c1:d1:download"},"install":{"id":"c1:d1:install"}}]}}
}`
	if err := os.WriteFile(filepath.Join(dir, storeFileName), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open old campaign store: %v", err)
	}
	defer s.Close()
	v, err := s.GetCampaign("c1")
	if err != nil {
		t.Fatalf("get old campaign: %v", err)
	}
	if v.RollbackOnFailure {
		t.Fatal("old campaign must default rollback off")
	}
	if d := findDevice(v, "d1"); d.Status != DevicePending || d.RollbackTarget != "" {
		t.Fatalf("old campaign device: %+v", d)
	}
	// 旧活动上安装失败直接结束设备，不进入回滚。
	dl, _ := s.Claim("c1", "d1", upBase.Add(5*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: "c1", DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(6 * time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	in, _ := s.Claim("c1", "d1", upBase.Add(7*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: "c1", DeviceID: "d1", OperationID: in.ID,
		At: upBase.Add(8 * time.Minute), Success: false, Reason: "old fail",
	}); err != nil {
		t.Fatal(err)
	}
	cv, _ := s.GetCampaign("c1")
	if d := findDevice(cv, "d1"); d.Status != DeviceFailed || d.Phase != StageInstall {
		t.Fatalf("old campaign install failure legacy behavior: %+v", d)
	}
}
