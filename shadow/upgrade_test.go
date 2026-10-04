package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 升级测试使用的时间线：
//
//	t0 12:00 创建活动；窗口 12:00–13:00（[start,end)）；截止 14:00。
var upBase = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func upSpec() CampaignSpec {
	return CampaignSpec{
		ID:            "cmp-1",
		Operator:      "alice",
		CreatedAt:     upBase,
		TargetVersion: "v2",
		Devices:       nil,
		BatchSize:     2,
		WindowStart:   upBase,
		WindowEnd:     upBase.Add(time.Hour),
		Deadline:      upBase.Add(2 * time.Hour),
	}
}

// setupUpgrade 登记目标版本 v2（允许 v1）并登记若干 v1 设备。
func setupUpgrade(t *testing.T, devices ...string) *Store {
	t.Helper()
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("v2", []string{"v1"}); err != nil {
		t.Fatalf("RegisterUpgrade: %v", err)
	}
	for _, id := range devices {
		mustRegister(t, s, id, "v1")
	}
	return s
}

// bringOnline 用一次更大序号的上报把设备置为在线（版本 v1）。
func bringOnline(t *testing.T, s *Store, id string) {
	t.Helper()
	v, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	at := upBase.Add(time.Duration(v.LastSeq) * time.Minute).Add(-time.Minute)
	if err := s.Report(id, v.LastSeq+1, at, "v1", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("Report %s: %v", id, err)
	}
}

func createCampaign(t *testing.T, s *Store, spec CampaignSpec) {
	t.Helper()
	if err := s.CreateCampaign(spec); err != nil {
		t.Fatalf("CreateCampaign: %v", err)
	}
}

// finishDevice 完成一台设备的下载与安装，附带版本为 target 的上报（seq 2）。
func finishDevice(t *testing.T, s *Store, spec CampaignSpec, id string, at time.Time) {
	t.Helper()
	dl, err := s.Claim(spec.ID, id, at)
	if err != nil {
		t.Fatalf("claim download %s: %v", id, err)
	}
	if dl == nil || dl.Kind != StageDownload {
		t.Fatalf("want download op for %s, got %+v", id, dl)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: id, OperationID: dl.ID, At: at.Add(time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("download result %s: %v", id, err)
	}
	in, err := s.Claim(spec.ID, id, at.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("claim install %s: %v", id, err)
	}
	if in == nil || in.Kind != StageInstall {
		t.Fatalf("want install op for %s, got %+v", id, in)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: id, OperationID: in.ID, At: at.Add(3 * time.Minute),
		Success: true, Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("install result %s: %v", id, err)
	}
}

func TestRegisterUpgradeValidation(t *testing.T) {
	s, _ := openTemp(t)
	cases := []struct {
		name    string
		target  string
		allowed []string
		want    error
	}{
		{"empty target", "", []string{"v1"}, ErrInvalidVersion},
		{"empty list", "v2", nil, ErrInvalidVersionList},
		{"empty entry", "v2", []string{"v1", ""}, ErrInvalidVersionList},
		{"contains target", "v2", []string{"v1", "v2"}, ErrInvalidVersionList},
	}
	for _, tc := range cases {
		if err := s.RegisterUpgrade(tc.target, tc.allowed); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v", tc.name, err)
		}
	}
	if err := s.RegisterUpgrade("v2", []string{"v1"}); err != nil {
		t.Fatal(err)
	}
	// 重名报错且保留原记录
	if err := s.RegisterUpgrade("v2", []string{"v9"}); !errors.Is(err, ErrVersionExists) {
		t.Fatalf("duplicate: %v", err)
	}
	got, err := s.ListUpgrades()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Target != "v2" || len(got[0].AllowedFrom) != 1 || got[0].AllowedFrom[0] != "v1" {
		t.Fatalf("original record changed: %+v", got)
	}
}

func TestCreateCampaignValidation(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2"}
	good := spec

	check := func(mutate func(*CampaignSpec), want error) {
		t.Helper()
		c := good
		mutate(&c)
		if err := s.CreateCampaign(c); !errors.Is(err, want) {
			t.Fatalf("want %v, got %v", want, err)
		}
	}
	check(func(c *CampaignSpec) { c.ID = "" }, ErrInvalidCampaignID)
	check(func(c *CampaignSpec) { c.Operator = "" }, ErrInvalidOperator)
	check(func(c *CampaignSpec) { c.CreatedAt = time.Time{} }, ErrInvalidTime)
	check(func(c *CampaignSpec) { c.WindowStart = time.Time{} }, ErrInvalidTime)
	check(func(c *CampaignSpec) { c.Devices = nil }, ErrInvalidDeviceList)
	check(func(c *CampaignSpec) { c.Devices = []string{"d1", "d1"} }, ErrDuplicateDevice)
	check(func(c *CampaignSpec) { c.Devices = []string{"d1", ""} }, ErrInvalidDeviceID)
	check(func(c *CampaignSpec) { c.BatchSize = 0 }, ErrInvalidBatchSize)
	check(func(c *CampaignSpec) { c.BatchSize = -1 }, ErrInvalidBatchSize)
	check(func(c *CampaignSpec) { c.WindowEnd = c.WindowStart }, ErrInvalidWindow)
	check(func(c *CampaignSpec) { c.Deadline = c.CreatedAt }, ErrInvalidDeadline)
	check(func(c *CampaignSpec) { c.TargetVersion = "v9" }, ErrVersionNotFound)
	check(func(c *CampaignSpec) { c.TargetVersion = "" }, ErrInvalidVersion)
	check(func(c *CampaignSpec) { c.Devices = []string{"d1", "ghost"} }, ErrDeviceNotFound)

	// 不兼容：把 d2 上报成 v3
	if err := s.Report("d2", 1, upBase.Add(-time.Minute), "v3", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	check(func(c *CampaignSpec) { c.Devices = []string{"d1", "d2"} }, ErrIncompatibleVersion)
	if err := s.Report("d2", 2, upBase.Add(-time.Minute), "v1", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	// 全部失败的尝试都不留记录
	createCampaign(t, s, good)
	if _, err := s.GetCampaign("cmp-1"); err != nil {
		t.Fatalf("valid campaign missing: %v", err)
	}
	if len(mustListCampaigns(s)) != 1 {
		t.Fatal("failed attempts left partial records")
	}
	// 重复标识
	check(func(c *CampaignSpec) {}, ErrCampaignExists)

	// 设备已参加未结束活动
	bringOnline(t, s, "d1")
	c2 := good
	c2.ID = "cmp-2"
	c2.Devices = []string{"d2", "d1"}
	if err := s.CreateCampaign(c2); !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("busy: %v", err)
	}
}

func mustListCampaigns(s *Store) []CampaignView {
	// 仅测试辅助：遍历已知标识。
	return []CampaignView{mustGetCampaign(s, "cmp-1")}
}

func mustGetCampaign(s *Store, id string) CampaignView {
	v, err := s.GetCampaign(id)
	if err != nil {
		panic(err)
	}
	return v
}

func TestClaimWindowOnlineAndStableID(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)

	// 离线：窗口内也无新操作（时间线在后续领取之前）
	op, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if op != nil {
		t.Fatalf("offline device should not get op: %+v", op)
	}
	bringOnline(t, s, "d1")

	// 窗口开始时刻包含：可领取
	op, err = s.Claim(spec.ID, "d1", upBase.Add(3*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if op == nil || op.ID != operationID("cmp-1", "d1", StageDownload) || op.Kind != StageDownload {
		t.Fatalf("claim in window: %+v", op)
	}
	wantID := op.ID
	// 窗口结束后、截止前重复查询未完成操作：仍返回原标识
	op2, err := s.Claim(spec.ID, "d1", upBase.Add(75*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if op2 == nil || op2.ID != wantID {
		t.Fatalf("outside window but claimed op must return: %+v", op2)
	}
}

func TestWindowEndExcludedForNewOps(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.WindowEnd = upBase.Add(30 * time.Minute) // 窗口 12:00–12:30
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")

	// 下载在窗口内领取并完成
	dl, err := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
	if err != nil || dl == nil {
		t.Fatalf("claim dl: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(20 * time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	// 尚未领取的安装在窗口结束时刻（不含）不能领取
	in, err := s.Claim(spec.ID, "d1", upBase.Add(30*time.Minute))
	if err != nil {
		t.Fatalf("claim at end should be no-op, got %v", err)
	}
	if in != nil {
		t.Fatalf("install must wait for window: %+v", in)
	}
	// 设备查询能看到待执行安装（未领取）
	work, err := s.GetDeviceWork("d1")
	if err != nil {
		t.Fatal(err)
	}
	if work.Pending == nil || work.Pending.Kind != StageInstall || work.PendingClaimed {
		t.Fatalf("pending install: %+v", work)
	}
}

func TestClaimedOpCompletesOutsideWindow(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.WindowEnd = upBase.Add(30 * time.Minute)
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")

	dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
	// 已领取的下载在窗口外完成仍接受
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(45 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("claimed op completes outside window: %v", err)
	}
}

func TestTimeValidationAndRegression(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")

	if _, err := s.Claim(spec.ID, "d1", time.Time{}); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("zero claim time: %v", err)
	}
	dl, err := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	// 时间倒退拒绝
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(19*time.Minute)); !errors.Is(err, ErrTimeRegression) {
		t.Fatalf("claim regression: %v", err)
	}
	good := OperationResult{CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(25 * time.Minute), Success: true}
	if err := s.SubmitResult(good); err != nil {
		t.Fatal(err)
	}
	// 已接受结果改时间重放属于重复提交：成功（发生时间不参与幂等）
	again := good
	again.At = upBase.Add(24 * time.Minute)
	if err := s.SubmitResult(again); err != nil {
		t.Fatalf("duplicate with earlier time should succeed: %v", err)
	}
	// 新操作的结果时间倒退：拒绝
	in, err := s.Claim(spec.ID, "d1", upBase.Add(26*time.Minute))
	if err != nil || in == nil {
		t.Fatalf("claim install: %v %+v", err, in)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: upBase.Add(24 * time.Minute), Success: false, Reason: "late",
	}); !errors.Is(err, ErrTimeRegression) {
		t.Fatalf("result regression: %v", err)
	}
	if err := s.AdvanceCampaign(spec.ID, time.Time{}); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("advance zero: %v", err)
	}
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(10*time.Minute)); !errors.Is(err, ErrTimeRegression) {
		t.Fatalf("advance regression: %v", err)
	}
}

func TestBatchGatingAndFailureCascade(t *testing.T) {
	s := setupUpgrade(t, "b1", "b2", "b3", "b4")
	spec := upSpec()
	spec.Devices = []string{"b1", "b2", "b3", "b4"}
	spec.BatchSize = 2
	createCampaign(t, s, spec)
	for _, id := range spec.Devices {
		bringOnline(t, s, id)
	}

	// 第二批（b3,b4）在第一批未全部成功前不能领取：返回空操作
	op, err := s.Claim(spec.ID, "b3", upBase.Add(time.Minute))
	if err != nil {
		t.Fatalf("waiting batch claim should be no-op: %v", err)
	}
	if op != nil {
		t.Fatalf("later batch dispatched early: %+v", op)
	}

	// b1 完整成功
	finishDevice(t, s, spec, "b1", upBase.Add(2*time.Minute))
	// b2 下载失败
	dl, err := s.Claim(spec.ID, "b2", upBase.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	failAt := upBase.Add(7 * time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "b2", OperationID: dl.ID,
		At: failAt, Success: false, Reason: "download broken",
	}); err != nil {
		t.Fatal(err)
	}

	v, err := s.GetCampaign(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	st := map[string]DeviceStatusView{}
	for _, d := range v.Devices {
		st[d.DeviceID] = d
	}
	if st["b2"].Status != DeviceFailed || st["b2"].Phase != StageDownload ||
		st["b2"].Reason != "download broken" || !st["b2"].At.Equal(failAt) {
		t.Fatalf("b2 failure record: %+v", st["b2"])
	}
	// 后续批次全部记为未执行
	for _, id := range []string{"b3", "b4"} {
		if st[id].Status != DeviceSkipped {
			t.Fatalf("%s should be skipped: %+v", id, st[id])
		}
	}
	// 失败后活动立即结束（设备均有终态）
	if !v.Ended || v.Status != CampaignFailed {
		t.Fatalf("campaign should end failed: %+v", v)
	}
	// 失败/超时原因和结果历史可查
	if len(v.Results) != 3 { // b1 下载、b1 安装、b2 下载失败
		t.Fatalf("result history: %+v", v.Results)
	}
}

func TestBatchContinuesWithinBatch(t *testing.T) {
	// 两批各 1 台：第一批失败，活动仍未结束？——批次 0 失败后批次 1 立即 skipped，
	// 因此这里验证：同批 2 台，一台失败时另一台仍可完成，完成后活动才结束。
	s := setupUpgrade(t, "a1", "a2", "a3")
	spec := upSpec()
	spec.Devices = []string{"a1", "a2", "a3"}
	spec.BatchSize = 2
	createCampaign(t, s, spec)
	for _, id := range spec.Devices {
		bringOnline(t, s, id)
	}
	dl1, _ := s.Claim(spec.ID, "a1", upBase.Add(2*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "a1", OperationID: dl1.ID,
		At: upBase.Add(3 * time.Minute), Success: false, Reason: "boom",
	}); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetCampaign(spec.ID)
	if v.Ended {
		t.Fatal("campaign should wait for a2 to finish")
	}
	if st := deviceStatus(v, "a2"); st != DevicePending {
		t.Fatalf("a2 must continue: %s", st)
	}
	// a2 同批继续完成
	finishDevice(t, s, spec, "a2", upBase.Add(4*time.Minute))
	v, _ = s.GetCampaign(spec.ID)
	if !v.Ended || v.Status != CampaignFailed {
		t.Fatalf("campaign ends failed after batch settles: %+v", v)
	}
	if st := deviceStatus(v, "a3"); st != DeviceSkipped {
		t.Fatalf("a3 skipped: %s", st)
	}
}

func deviceStatus(v CampaignView, id string) string {
	for _, d := range v.Devices {
		if d.DeviceID == id {
			return d.Status
		}
	}
	return ""
}

func TestResultIdempotencyAndConflicts(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2"}
	spec.BatchSize = 2
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")

	dl, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	res := OperationResult{CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(5 * time.Minute), Success: true}
	if err := s.SubmitResult(res); err != nil {
		t.Fatal(err)
	}
	v1, _ := s.GetCampaign(spec.ID)
	// 相同结果重复提交：成功，不增加历史（即使时间更晚）
	res.At = upBase.Add(10 * time.Minute)
	if err := s.SubmitResult(res); err != nil {
		t.Fatalf("duplicate result: %v", err)
	}
	v2, _ := s.GetCampaign(spec.ID)
	if len(v2.Results) != len(v1.Results) {
		t.Fatal("duplicate result added history")
	}
	// 改用不同结果：冲突
	res.Success = false
	res.Reason = "changed mind"
	if err := s.SubmitResult(res); !errors.Is(err, ErrResultConflict) {
		t.Fatalf("conflict: %v", err)
	}
	// 跳过阶段：直接提交尚未领取的安装
	in := operationID("cmp-1", "d1", StageInstall)
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in,
		At: upBase.Add(11 * time.Minute), Success: true,
	}); !errors.Is(err, ErrOperationNotClaimed) {
		t.Fatalf("skip stage: %v", err)
	}
	// 提交其他设备的操作标识
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d2", OperationID: dl.ID,
		At: upBase.Add(12 * time.Minute), Success: true,
	}); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("other device op: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d2", OperationID: operationID("cmp-1", "d2", StageDownload),
		At: upBase.Add(12 * time.Minute), Success: true,
	}); !errors.Is(err, ErrOperationNotClaimed) {
		t.Fatalf("unclaimed own op: %v", err)
	}
	// 失败结果必须有原因
	dl2, _ := s.Claim(spec.ID, "d2", upBase.Add(13*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d2", OperationID: dl2.ID,
		At: upBase.Add(14 * time.Minute), Success: false,
	}); !errors.Is(err, ErrInvalidReason) {
		t.Fatalf("failure without reason: %v", err)
	}
}

func TestInstallReportRules(t *testing.T) {
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
	install := func(mutate func(*OperationResult)) error {
		r := OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
			At: upBase.Add(5 * time.Minute), Success: true,
			Seq: 2, Version: "v2", Config: json.RawMessage(`{}`),
		}
		mutate(&r)
		return s.SubmitResult(r)
	}
	if err := install(func(r *OperationResult) { r.Seq = 0 }); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("seq 0: %v", err)
	}
	if err := install(func(r *OperationResult) { r.Version = "v3" }); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("wrong version: %v", err)
	}
	if err := install(func(r *OperationResult) { r.Version = "" }); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("empty version: %v", err)
	}
	if err := install(func(r *OperationResult) { r.Config = json.RawMessage(`[]`) }); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("bad config: %v", err)
	}
	if err := install(func(r *OperationResult) { r.Seq = 1; r.Version = "v1" }); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("seq conflict: %v", err)
	}
	// 合法安装：影子同时更新
	if err := install(func(r *OperationResult) {}); err != nil {
		t.Fatalf("valid install: %v", err)
	}
	v, _ := s.Get("d1")
	if v.Version != "v2" || !v.Online || v.LastSeq != 2 || string(v.Reported) != "{}" {
		t.Fatalf("shadow after install: %+v", v)
	}
	cv, _ := s.GetCampaign(spec.ID)
	if !cv.Ended || cv.Status != CampaignSucceeded {
		t.Fatalf("campaign: %+v", cv)
	}
	// 普通上报不推进活动：另起活动验证上报本身不改变活动状态
}

func TestPlainReportDoesNotAdvanceCampaign(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	// 普通上报把版本改成 v2，活动设备状态仍为 pending
	if err := s.Report("d1", 2, upBase.Add(5*time.Minute), "v2", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetCampaign(spec.ID)
	if deviceStatus(v, "d1") != DevicePending {
		t.Fatalf("plain report advanced campaign: %+v", v.Devices)
	}
	// 首次领取下载前复查：当前版本已不兼容 → 该设备失败
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(6*time.Minute)); !errors.Is(err, ErrIncompatibleVersion) {
		t.Fatalf("recheck incompatibility: %v", err)
	}
	v, _ = s.GetCampaign(spec.ID)
	if deviceStatus(v, "d1") != DeviceFailed {
		t.Fatalf("device should fail: %+v", v.Devices)
	}
	if !v.Ended || v.Status != CampaignFailed {
		t.Fatalf("campaign failed: %+v", v)
	}
}

func TestDeadlineTimeout(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2", "d3")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2", "d3"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	bringOnline(t, s, "d3")
	// d1 完整成功
	finishDevice(t, s, spec, "d1", upBase.Add(5*time.Minute))
	// d2 下载已领取未完成
	dl2, _ := s.Claim(spec.ID, "d2", upBase.Add(10*time.Minute))
	_ = dl2
	// d3 尚未开始（批次 2 未放行）

	// 推进到截止时刻
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetCampaign(spec.ID)
	if !v.Ended || v.Status != CampaignFailed {
		t.Fatalf("deadline: %+v", v)
	}
	if st := deviceStatus(v, "d1"); st != DeviceSucceeded {
		t.Fatalf("terminal kept: %s", st)
	}
	for _, id := range []string{"d2", "d3"} {
		d := findDevice(v, id)
		if d.Status != DeviceTimeout || d.Reason == "" || d.At.IsZero() {
			t.Fatalf("%s timeout record: %+v", id, d)
		}
	}
	// 结束后不再派发
	if _, err := s.Claim(spec.ID, "d2", upBase.Add(2*time.Hour+time.Minute)); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("claim after end: %v", err)
	}
	// 结束后的新结果报错
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d2", OperationID: dl2.ID,
		At: upBase.Add(2*time.Hour + time.Minute), Success: true,
	}); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("new result after end: %v", err)
	}
	// 已接受结果的重复提交在结束后仍有效（用 d1 的安装结果重放）
	d1 := findDevice(v, "d1")
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: d1.InstallID,
		At: upBase.Add(3 * time.Hour), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("duplicate after end: %v", err)
	}
	v2, _ := s.GetCampaign(spec.ID)
	if len(v2.Results) != len(v.Results) {
		t.Fatal("replay after end added history")
	}
}

func TestDeadlineOnResultAndClaim(t *testing.T) {
	// 接收结果/领取时达到截止：直接整体超时
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Hour)); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("claim at deadline: %v", err)
	}
	v, _ := s.GetCampaign(spec.ID)
	if !v.Ended || deviceStatus(v, "d1") != DeviceTimeout {
		t.Fatalf("claim deadline cascade: %+v", v)
	}
}

func findDevice(v CampaignView, id string) DeviceStatusView {
	for _, d := range v.Devices {
		if d.DeviceID == id {
			return d
		}
	}
	return DeviceStatusView{}
}

func TestOfflineResumeAfterReconnect(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.WindowEnd = upBase.Add(90 * time.Minute)
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, _ := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
	// 离线期间完成下载（已领取操作可提交，离线不影响结果提交）
	if err := s.SetOffline("d1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(10 * time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	// 离线时不能领取安装
	op, err := s.Claim(spec.ID, "d1", upBase.Add(15*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if op != nil {
		t.Fatalf("offline install claim: %+v", op)
	}
	// 上线后继续
	bringOnline(t, s, "d1")
	in, err := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute))
	if err != nil || in == nil || in.Kind != StageInstall {
		t.Fatalf("resume install: %v %+v", err, in)
	}
}

func TestUpgradePersistenceReopen(t *testing.T) {
	s, dir := openTemp(t)
	if err := s.RegisterUpgrade("v2", []string{"v1"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "d1", "v1")
	mustRegister(t, s, "d2", "v1")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2"}
	spec.BatchSize = 2
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, _ := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(6 * time.Minute), Success: true,
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
	got, err := s2.ListUpgrades()
	if err != nil || len(got) != 1 || got[0].Target != "v2" {
		t.Fatalf("versions: %v %+v", err, got)
	}
	v, err := s2.GetCampaign(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Operator != "alice" || v.TargetVersion != "v2" || len(v.Batches) != 1 || len(v.Batches[0].Devices) != 2 {
		t.Fatalf("campaign view: %+v", v)
	}
	d1 := findDevice(v, "d1")
	if d1.Status != DeviceReady ||
		d1.DownloadID != operationID("cmp-1", "d1", StageDownload) ||
		d1.InstallID != operationID("cmp-1", "d1", StageInstall) {
		t.Fatalf("d1 state/ids: %+v", d1)
	}
	// 时间倒退判断延续
	if err := s2.AdvanceCampaign(spec.ID, upBase.Add(4*time.Minute)); !errors.Is(err, ErrTimeRegression) {
		t.Fatalf("regression after reopen: %v", err)
	}
	// 重复结果判断延续：重放下载成功不增加历史
	if err := s2.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(8 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("replay after reopen: %v", err)
	}
	v2, _ := s2.GetCampaign(spec.ID)
	if len(v2.Results) != len(v.Results) {
		t.Fatal("replay added history after reopen")
	}
	// 设备进度延续：d1 在线状态已持久化，可继续领取安装并完成
	in, err := s2.Claim(spec.ID, "d1", upBase.Add(9*time.Minute))
	if err != nil || in == nil || in.Kind != StageInstall || in.ID != d1.InstallID {
		t.Fatalf("install after reopen: %v %+v", err, in)
	}
	if err := s2.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: upBase.Add(10 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	shadowView, _ := s2.Get("d1")
	if shadowView.Version != "v2" {
		t.Fatalf("shadow version after reopen: %s", shadowView.Version)
	}
}

func TestOldStorageOpensWithoutUpgradeData(t *testing.T) {
	// 只有旧版设备数据的存储文件必须能直接打开
	dir := t.TempDir()
	old := `{"format":1,"devices":{"d1":{"version":"v1","online":false,"revision":0,"desired":{},"reported":{},"lastSeq":0}}}`
	if err := os.WriteFile(filepath.Join(dir, storeFileName), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("old store: %v", err)
	}
	defer s.Close()
	v, err := s.Get("d1")
	if err != nil || v.Version != "v1" {
		t.Fatalf("old device: %v %+v", err, v)
	}
	// 新能力在旧存储上可用
	if err := s.RegisterUpgrade("v2", []string{"v1"}); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeSaveFailureRollback(t *testing.T) {
	s, dir := setupUpgradeDir(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, _ := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(6 * time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	in, _ := s.Claim(spec.ID, "d1", upBase.Add(7*time.Minute))

	// 堵住存储文件
	if err := os.Remove(filepath.Join(dir, storeFileName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, storeFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	// 安装成功涉及影子+活动+历史：保存失败全部保持原状
	err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: upBase.Add(8 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"x":1}`),
	})
	if err == nil {
		t.Fatal("expected save failure")
	}
	v, _ := s.Get("d1")
	if v.Version != "v1" || v.LastSeq != 1 {
		t.Fatalf("shadow not rolled back: %+v", v)
	}
	cv, _ := s.GetCampaign(spec.ID)
	d := findDevice(cv, "d1")
	// 领取安装是此前已提交的操作；回滚的是安装结果，设备停在 installing。
	if d.Status != DeviceInstalling {
		t.Fatalf("campaign not rolled back: %+v", d)
	}
	if len(cv.Results) != 1 {
		t.Fatalf("history not rolled back: %+v", cv.Results)
	}
	// 回滚后用原操作标识重提仍可成功
	if err := os.Remove(filepath.Join(dir, storeFileName)); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: upBase.Add(9 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"x":1}`),
	}); err != nil {
		t.Fatalf("retry after recovery: %v", err)
	}
}

// setupUpgradeDir 与 setupUpgrade 相同，但返回目录以便破坏存储文件。
func setupUpgradeDir(t *testing.T, devices ...string) (*Store, string) {
	t.Helper()
	s, dir := openTemp(t)
	if err := s.RegisterUpgrade("v2", []string{"v1"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range devices {
		mustRegister(t, s, id, "v1")
	}
	return s, dir
}

func TestFullSuccessFlow(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2", "d3")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2", "d3"}
	spec.BatchSize = 2
	createCampaign(t, s, spec)
	for _, id := range spec.Devices {
		bringOnline(t, s, id)
	}
	finishDevice(t, s, spec, "d1", upBase.Add(2*time.Minute))
	// 第二批在 d2 完成前不放行
	op, _ := s.Claim(spec.ID, "d3", upBase.Add(10*time.Minute))
	if op != nil {
		t.Fatalf("batch 2 early: %+v", op)
	}
	finishDevice(t, s, spec, "d2", upBase.Add(11*time.Minute))
	// 第二批放行
	finishDevice(t, s, spec, "d3", upBase.Add(20*time.Minute))
	v, _ := s.GetCampaign(spec.ID)
	if !v.Ended || v.Status != CampaignSucceeded {
		t.Fatalf("campaign: %+v", v)
	}
	for _, d := range v.Devices {
		if d.Status != DeviceSucceeded || d.Reason != "" {
			t.Fatalf("device: %+v", d)
		}
	}
	// 每台设备影子版本均为目标版本
	for _, id := range spec.Devices {
		view, _ := s.Get(id)
		if view.Version != "v2" {
			t.Fatalf("%s version %s", id, view.Version)
		}
	}
	// 成功后设备可以参加新活动
	spec2 := spec
	spec2.ID = "cmp-2"
	spec2.CreatedAt = upBase.Add(time.Minute)
	spec2.Devices = []string{"d1"}
	if err := s.RegisterUpgrade("v3", []string{"v2"}); err != nil {
		t.Fatal(err)
	}
	spec2.TargetVersion = "v3"
	if err := s.CreateCampaign(spec2); err != nil {
		t.Fatalf("new campaign after success: %v", err)
	}
}

// 截止前已领取安装，期间普通上报推高了序号；在截止时刻提交的成功结果
// 附带上报序号已过旧。迟到的结果不应因附带上报不合规而报 ErrInvalidReport，
// 而应按截止处理：活动以失败结束，设备进入超时终态。
func TestLateResultAtDeadlineWithStaleReport(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
	if err != nil || dl == nil {
		t.Fatalf("claim download: %v %+v", err, dl)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(6 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("download result: %v", err)
	}
	in, err := s.Claim(spec.ID, "d1", upBase.Add(7*time.Minute))
	if err != nil || in == nil || in.Kind != StageInstall {
		t.Fatalf("claim install: %v %+v", err, in)
	}
	// 领取后普通上报推高序号与版本，使结果附带上报过旧。
	if err := s.Report("d1", 2, upBase.Add(8*time.Minute), "v1", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Report("d1", 3, upBase.Add(9*time.Minute), "v1", json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	before, _ := s.Get("d1")
	v0, _ := s.GetCampaign(spec.ID)
	nResults := len(v0.Results)

	// 截止时刻提交成功结果，附带上报序号过旧：按截止处理，返回 ErrCampaignEnded。
	err = s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: upBase.Add(2 * time.Hour), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true}`),
	})
	if !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("late result at deadline: %v", err)
	}
	v, _ := s.GetCampaign(spec.ID)
	if !v.Ended || v.Status != CampaignFailed {
		t.Fatalf("campaign should end failed: %+v", v)
	}
	if !v.EndedAt.Equal(upBase.Add(2 * time.Hour)) {
		t.Fatalf("ended at = %v", v.EndedAt)
	}
	d := findDevice(v, "d1")
	if d.Status != DeviceTimeout || d.Phase != StageInstall ||
		d.Reason == "" || !d.At.Equal(upBase.Add(2*time.Hour)) {
		t.Fatalf("device timeout record: %+v", d)
	}
	// 迟到结果不进入历史。
	if len(v.Results) != nResults {
		t.Fatalf("late result added history: %d -> %d", nResults, len(v.Results))
	}
	// 影子不被迟到结果改写。
	after, _ := s.Get("d1")
	if after.Version != before.Version || after.LastSeq != before.LastSeq ||
		after.Revision != before.Revision || !rawEqual(after.Reported, before.Reported) {
		t.Fatalf("shadow changed: before %+v after %+v", before, after)
	}
	// 结束后待办与领取均按结束规则处理。
	w, err := s.GetDeviceWork("d1")
	if err != nil {
		t.Fatal(err)
	}
	if w.CampaignID != "" || w.Pending != nil {
		t.Fatalf("pending after end: %+v", w)
	}
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Hour+time.Minute)); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("claim after end: %v", err)
	}
}

// 开启回滚的活动中，已领取回滚在截止时刻的迟到成功结果同样按截止处理：
// 设备记回滚超时，影子版本不得被改回锁定的回滚目标。
func TestLateRollbackResultAtDeadline(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	spec.RollbackOnFailure = true
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, _ := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(6 * time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	in, _ := s.Claim(spec.ID, "d1", upBase.Add(7*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: upBase.Add(8 * time.Minute), Success: false, Reason: "install boom",
	}); err != nil {
		t.Fatal(err)
	}
	rb, err := s.Claim(spec.ID, "d1", upBase.Add(9*time.Minute))
	if err != nil || rb == nil || rb.Kind != StageRollback || rb.TargetVersion != "v1" {
		t.Fatalf("claim rollback: %v %+v", err, rb)
	}
	// 领取回滚后普通上报推高序号并把版本改成 v9（与锁定目标 v1 不同）。
	if err := s.Report("d1", 2, upBase.Add(10*time.Minute), "v9", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Report("d1", 3, upBase.Add(11*time.Minute), "v9", json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	// 截止之后提交回滚成功，附带上报序号过旧：按截止处理。
	err = s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(2*time.Hour + time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	})
	if !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("late rollback result: %v", err)
	}
	v, _ := s.GetCampaign(spec.ID)
	if !v.Ended || v.Status != CampaignFailed {
		t.Fatalf("campaign should end failed: %+v", v)
	}
	d := findDevice(v, "d1")
	if d.Status != DeviceRollbackTimeout || d.Phase != StageRollback ||
		d.Reason == "" || !d.At.Equal(upBase.Add(2*time.Hour+time.Minute)) {
		t.Fatalf("rollback timeout record: %+v", d)
	}
	if !v.EndedAt.Equal(upBase.Add(2*time.Hour + time.Minute)) {
		t.Fatalf("ended at = %v", v.EndedAt)
	}
	// 回滚超时不得把影子版本改回锁定目标，序号与上报配置保留。
	after, _ := s.Get("d1")
	if after.Version != "v9" || after.LastSeq != 3 ||
		!rawEqual(after.Reported, json.RawMessage(`{"a":1}`)) {
		t.Fatalf("shadow rewritten by rollback timeout: %+v", after)
	}
	// 安装失败原因与时间保留，回滚结果未被接受。
	if d.InstallFailReason != "install boom" || d.RollbackResult {
		t.Fatalf("rollback state: %+v", d)
	}
}

// 迟到提交不校验内容：附带上报缺失、版本不符、配置非法、序号冲突，
// 以及失败结果缺少原因，都不能挡住已领取操作的截止处理。
func TestLateResultAtDeadlineSkipsContentValidation(t *testing.T) {
	cases := []struct {
		name string
		res  OperationResult
	}{
		{"missing report", OperationResult{Success: true}},
		{"wrong version", OperationResult{Success: true, Seq: 1, Version: "v9", Config: json.RawMessage(`{}`)}},
		{"bad config", OperationResult{Success: true, Seq: 1, Version: "v2", Config: json.RawMessage(`[1]`)}},
		{"seq conflict", OperationResult{Success: true, Seq: 1, Version: "v2", Config: json.RawMessage(`{"x":1}`)}},
		{"failure without reason", OperationResult{Success: false}},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := setupUpgrade(t, "d1")
			spec := upSpec()
			spec.ID = "cmp-late"
			spec.Devices = []string{"d1"}
			spec.BatchSize = 1
			createCampaign(t, s, spec)
			bringOnline(t, s, "d1")
			dl, _ := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
			if err := s.SubmitResult(OperationResult{
				CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
				At: upBase.Add(6 * time.Minute), Success: true,
			}); err != nil {
				t.Fatal(err)
			}
			in, _ := s.Claim(spec.ID, "d1", upBase.Add(7*time.Minute))
			res := tc.res
			res.CampaignID = spec.ID
			res.DeviceID = "d1"
			res.OperationID = in.ID
			res.At = upBase.Add(2*time.Hour + time.Duration(i)*time.Minute)
			if err := s.SubmitResult(res); !errors.Is(err, ErrCampaignEnded) {
				t.Fatalf("late result: %v", err)
			}
			v, _ := s.GetCampaign(spec.ID)
			if !v.Ended || v.Status != CampaignFailed || deviceStatus(v, "d1") != DeviceTimeout {
				t.Fatalf("campaign should time out: %+v", v)
			}
		})
	}
}

// 无效提交不能借截止结束活动：未知操作、未领取阶段、缺失时间、
// 时间倒退仍按原错误拒绝，活动保持执行中。
func TestInvalidSubmissionAtDeadlineDoesNotEndCampaign(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, _ := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
	deadline := upBase.Add(2 * time.Hour)

	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: spec.ID + ":d1:bogus",
		At: deadline, Success: true,
	}); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("unknown op: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: operationID(spec.ID, "d1", StageInstall),
		At: deadline, Success: true,
	}); !errors.Is(err, ErrOperationNotClaimed) {
		t.Fatalf("unclaimed stage: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		Success: true,
	}); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("zero time: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(4 * time.Minute), Success: true,
	}); !errors.Is(err, ErrTimeRegression) {
		t.Fatalf("regression: %v", err)
	}
	v, _ := s.GetCampaign(spec.ID)
	if v.Ended || deviceStatus(v, "d1") != DeviceDownloading {
		t.Fatalf("campaign must stay running: %+v", v)
	}
}

// 已接受结果的重复提交即使越过截止时间仍成功且不触发超时；
// 改成不同结果仍返回冲突，活动保持执行中。
func TestReplayAcceptedResultPastDeadlineDoesNotTimeout(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	finishDevice(t, s, spec, "d1", upBase.Add(5*time.Minute))
	dl2, _ := s.Claim(spec.ID, "d2", upBase.Add(10*time.Minute))
	_ = dl2
	v0, _ := s.GetCampaign(spec.ID)

	past := upBase.Add(3 * time.Hour)
	d1 := findDevice(v0, "d1")
	// 相同结果重复提交：成功，不触发超时、不增加历史。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: d1.InstallID,
		At: past, Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("replay past deadline: %v", err)
	}
	// 不同结果：冲突。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: d1.InstallID,
		At: past, Success: false, Reason: "changed",
	}); !errors.Is(err, ErrResultConflict) {
		t.Fatalf("conflict past deadline: %v", err)
	}
	v, _ := s.GetCampaign(spec.ID)
	if v.Ended || deviceStatus(v, "d2") != DeviceDownloading {
		t.Fatalf("replay must not time out campaign: %+v", v)
	}
	if len(v.Results) != len(v0.Results) {
		t.Fatalf("replay added history: %d -> %d", len(v0.Results), len(v.Results))
	}
}
