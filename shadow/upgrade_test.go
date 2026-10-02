package shadow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 升级活动测试共用的时刻与窗口。
var (
	upBase     = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	upCreated  = upBase
	upWinStart = upBase.Add(time.Hour)
	upWinEnd   = upBase.Add(3 * time.Hour)
	upDeadline = upBase.Add(24 * time.Hour)
)

// setupUpgrade 登记目标版本并登记若干在线设备，返回存储与设备标识。
func setupUpgrade(t *testing.T, allowed []string, devVersions map[string]string) (*Store, []string) {
	t.Helper()
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", allowed); err != nil {
		t.Fatalf("RegisterUpgrade: %v", err)
	}
	ids := make([]string, 0, len(devVersions))
	for id, ver := range devVersions {
		ids = append(ids, id)
		mustRegister(t, s, id, ver)
		// 上报使设备上线，版本保持为当前版本。
		if err := s.Report(id, 1, upBase, ver, json.RawMessage(`{}`)); err != nil {
			t.Fatalf("Report(%s): %v", id, err)
		}
	}
	return s, ids
}

func mustCreateActivity(t *testing.T, s *Store, id, target string, devices []string, batchSize int) ActivityView {
	t.Helper()
	v, err := s.CreateActivity(id, target, "op", upCreated, devices, batchSize, upWinStart, upWinEnd, upDeadline)
	if err != nil {
		t.Fatalf("CreateActivity: %v", err)
	}
	return v
}

func TestRegisterUpgradeValidation(t *testing.T) {
	s, _ := openTemp(t)
	// 目标版本为空
	if err := s.RegisterUpgrade("", []string{"1.0"}); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("empty target: %v", err)
	}
	// 列表为空
	if err := s.RegisterUpgrade("2.0", nil); !errors.Is(err, ErrUpgradeListEmpty) {
		t.Fatalf("empty list: %v", err)
	}
	// 列表为空切片
	if err := s.RegisterUpgrade("2.0", []string{}); !errors.Is(err, ErrUpgradeListEmpty) {
		t.Fatalf("empty slice: %v", err)
	}
	// 列表包含空版本
	if err := s.RegisterUpgrade("2.0", []string{""}); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("empty version in list: %v", err)
	}
	// 目标版本在列表中
	if err := s.RegisterUpgrade("2.0", []string{"1.0", "2.0"}); !errors.Is(err, ErrUpgradeTargetListed) {
		t.Fatalf("target in list: %v", err)
	}
	// 合法登记
	if err := s.RegisterUpgrade("2.0", []string{"1.0", "1.1"}); err != nil {
		t.Fatalf("valid register: %v", err)
	}
	// 重名报错且保留原记录
	if err := s.RegisterUpgrade("2.0", []string{"3.0"}); !errors.Is(err, ErrUpgradeExists) {
		t.Fatalf("duplicate: %v", err)
	}
	got, err := s.GetUpgrade("2.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "1.0" || got[1] != "1.1" {
		t.Fatalf("original record kept: %v", got)
	}
	// 未登记的目标版本
	if _, err := s.GetUpgrade("9.9"); !errors.Is(err, ErrUpgradeNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	// 返回副本，调用方修改不影响已保存记录
	got[0] = "hacked"
	got2, _ := s.GetUpgrade("2.0")
	if got2[0] != "1.0" {
		t.Fatalf("stored allowed mutated: %v", got2)
	}
}

func TestCreateActivityValidation(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Report("dev-1", 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "dev-2", "1.0")
	if err := s.Report("dev-2", 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}

	// 缺少必填信息
	badCases := []struct {
		name      string
		id        string
		target    string
		operator  string
		createdAt time.Time
		devices   []string
		batch     int
		winStart  time.Time
		winEnd    time.Time
		deadline  time.Time
		want      error
	}{
		{"empty id", "", "2.0", "op", upCreated, []string{"dev-1"}, 1, upWinStart, upWinEnd, upDeadline, ErrInvalidActivityID},
		{"empty operator", "a1", "2.0", "", upCreated, []string{"dev-1"}, 1, upWinStart, upWinEnd, upDeadline, ErrInvalidOperator},
		{"zero created", "a1", "2.0", "op", time.Time{}, []string{"dev-1"}, 1, upWinStart, upWinEnd, upDeadline, ErrInvalidTime},
		{"empty devices", "a1", "2.0", "op", upCreated, nil, 1, upWinStart, upWinEnd, upDeadline, ErrEmptyDeviceList},
		{"duplicate device", "a1", "2.0", "op", upCreated, []string{"dev-1", "dev-1"}, 1, upWinStart, upWinEnd, upDeadline, ErrDuplicateDevice},
		{"zero batch", "a1", "2.0", "op", upCreated, []string{"dev-1"}, 0, upWinStart, upWinEnd, upDeadline, ErrInvalidBatchSize},
		{"negative batch", "a1", "2.0", "op", upCreated, []string{"dev-1"}, -1, upWinStart, upWinEnd, upDeadline, ErrInvalidBatchSize},
		{"window equal", "a1", "2.0", "op", upCreated, []string{"dev-1"}, 1, upWinStart, upWinStart, upDeadline, ErrInvalidWindow},
		{"window reversed", "a1", "2.0", "op", upCreated, []string{"dev-1"}, 1, upWinEnd, upWinStart, upDeadline, ErrInvalidWindow},
		{"deadline equal", "a1", "2.0", "op", upCreated, []string{"dev-1"}, 1, upWinStart, upWinEnd, upCreated, ErrInvalidDeadline},
		{"deadline before", "a1", "2.0", "op", upCreated, []string{"dev-1"}, 1, upWinStart, upWinEnd, upCreated.Add(-time.Hour), ErrInvalidDeadline},
		{"target not registered", "a1", "9.9", "op", upCreated, []string{"dev-1"}, 1, upWinStart, upWinEnd, upDeadline, ErrUpgradeNotFound},
		{"device not registered", "a1", "2.0", "op", upCreated, []string{"ghost"}, 1, upWinStart, upWinEnd, upDeadline, ErrDeviceNotFound},
	}
	for _, tc := range badCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.CreateActivity(tc.id, tc.target, tc.operator, tc.createdAt, tc.devices, tc.batch, tc.winStart, tc.winEnd, tc.deadline)
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}

	// 版本不兼容
	_, err := s.CreateActivity("a1", "2.0", "op", upCreated, []string{"dev-1"}, 1, upWinStart, upWinEnd, upDeadline)
	if err != nil {
		t.Fatal(err)
	}
	// dev-2 版本不兼容：先登记一个高版本设备
	mustRegister(t, s, "dev-3", "3.0")
	if err := s.Report("dev-3", 1, upBase, "3.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	_, err = s.CreateActivity("a2", "2.0", "op", upCreated, []string{"dev-3"}, 1, upWinStart, upWinEnd, upDeadline)
	if !errors.Is(err, ErrDeviceIncompatible) {
		t.Fatalf("incompatible: %v", err)
	}
	// 设备已参加未结束的活动
	_, err = s.CreateActivity("a3", "2.0", "op", upCreated, []string{"dev-1"}, 1, upWinStart, upWinEnd, upDeadline)
	if !errors.Is(err, ErrDeviceInActivity) {
		t.Fatalf("device in activity: %v", err)
	}
	// 活动标识重复
	_, err = s.CreateActivity("a1", "2.0", "op", upCreated, []string{"dev-2"}, 1, upWinStart, upWinEnd, upDeadline)
	if !errors.Is(err, ErrActivityExists) {
		t.Fatalf("duplicate activity: %v", err)
	}
	// 整次失败不留下记录：a2、a3 都不存在
	if _, err := s.GetActivity("a2"); !errors.Is(err, ErrActivityNotFound) {
		t.Fatalf("a2 should not exist: %v", err)
	}
	if _, err := s.GetActivity("a3"); !errors.Is(err, ErrActivityNotFound) {
		t.Fatalf("a3 should not exist: %v", err)
	}
}

func TestCreateActivityBatches(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	devices := []string{"d1", "d2", "d3", "d4", "d5"}
	for _, d := range devices {
		mustRegister(t, s, d, "1.0")
		if err := s.Report(d, 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	v := mustCreateActivity(t, s, "a1", "2.0", devices, 2)
	if v.Status != ActivityActive {
		t.Fatalf("status: %s", v.Status)
	}
	if len(v.Batches) != 3 {
		t.Fatalf("batches: %d", len(v.Batches))
	}
	wantSizes := []int{2, 2, 1}
	for i, want := range wantSizes {
		if len(v.Batches[i]) != want {
			t.Fatalf("batch %d size: want %d got %d", i, want, len(v.Batches[i]))
		}
	}
	// 设备按提交次序分批
	if v.Batches[0][0].DeviceID != "d1" || v.Batches[0][1].DeviceID != "d2" {
		t.Fatalf("batch 0 order: %+v", v.Batches[0])
	}
	if v.Batches[1][0].DeviceID != "d3" || v.Batches[1][1].DeviceID != "d4" {
		t.Fatalf("batch 1 order: %+v", v.Batches[1])
	}
	if v.Batches[2][0].DeviceID != "d5" {
		t.Fatalf("batch 2 order: %+v", v.Batches[2])
	}
	// 全部待执行
	for _, b := range v.Batches {
		for _, d := range b {
			if d.Status != DevicePending || d.OpID != "" {
				t.Fatalf("device should be pending: %+v", d)
			}
		}
	}
}

func TestAdvanceRequiresOnlineAndWindow(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "dev-1", "1.0")
	// 截止时间设为两天后，使次日窗口仍在截止前
	deadline := upBase.Add(48 * time.Hour)
	if _, err := s.CreateActivity("a1", "2.0", "op", upCreated, []string{"dev-1"}, 1, upWinStart, upWinEnd, deadline); err != nil {
		t.Fatal(err)
	}

	// 离线不派发
	if err := s.Advance("a1", upWinStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	info, _ := s.GetDeviceUpgrade("dev-1")
	if info.Status != DevicePending || info.OpID != "" {
		t.Fatalf("offline should stay pending: %+v", info)
	}
	// 上线后仍在窗口内才派发
	if err := s.Report("dev-1", 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	// 窗口外（结束时刻）不派发
	if err := s.Advance("a1", upWinEnd); err != nil {
		t.Fatal(err)
	}
	info, _ = s.GetDeviceUpgrade("dev-1")
	if info.Status != DevicePending {
		t.Fatalf("at window end should not dispatch: %+v", info)
	}
	// 窗口外（结束后）不派发
	if err := s.Advance("a1", upWinEnd.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	info, _ = s.GetDeviceUpgrade("dev-1")
	if info.Status != DevicePending {
		t.Fatalf("after window should not dispatch: %+v", info)
	}
	// 次日窗口开始时刻（含开始时刻）派发下载
	nextStart := upWinStart.Add(24 * time.Hour)
	if err := s.Advance("a1", nextStart); err != nil {
		t.Fatal(err)
	}
	info, _ = s.GetDeviceUpgrade("dev-1")
	if info.Status != DeviceDownloading || info.Stage != StageDownload || info.OpID == "" {
		t.Fatalf("should claim download: %+v", info)
	}
	wantOp := "a1/dev-1/download"
	if info.OpID != wantOp {
		t.Fatalf("op id: want %s got %s", wantOp, info.OpID)
	}
	// 查询未完成操作仍返回原标识
	info2, _ := s.GetDeviceUpgrade("dev-1")
	if info2.OpID != info.OpID {
		t.Fatalf("op id changed: %s -> %s", info.OpID, info2.OpID)
	}
}

func TestAdvanceTimeValidation(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Report("dev-1", 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	mustCreateActivity(t, s, "a1", "2.0", []string{"dev-1"}, 1)

	// 时间缺失
	if err := s.Advance("a1", time.Time{}); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("zero time: %v", err)
	}
	// 当前时间倒退
	if err := s.Advance("a1", upWinStart.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.Advance("a1", upWinStart.Add(time.Minute)); !errors.Is(err, ErrTimeBackwards) {
		t.Fatalf("backwards: %v", err)
	}
	// 拒绝操作且不改状态：设备仍在下载
	info, _ := s.GetDeviceUpgrade("dev-1")
	if info.Status != DeviceDownloading {
		t.Fatalf("state changed by rejected advance: %+v", info)
	}
}

func TestFirstDownloadRechecksVersion(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Report("dev-1", 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	mustCreateActivity(t, s, "a1", "2.0", []string{"dev-1"}, 1)
	// 领取下载前版本变为不兼容
	if err := s.Report("dev-1", 2, upBase.Add(time.Minute), "3.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Advance("a1", upWinStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetActivity("a1")
	if v.Status != ActivityFailed {
		t.Fatalf("activity should fail: %s", v.Status)
	}
	d := v.Batches[0][0]
	if d.Status != DeviceFailed || d.FailStage != StageDownload || d.FailReason != "incompatible" || d.FailTime.IsZero() {
		t.Fatalf("device should fail with stage/reason/time: %+v", d)
	}
}

func TestBatchReleaseGating(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	devices := []string{"d1", "d2", "d3", "d4"}
	for _, d := range devices {
		mustRegister(t, s, d, "1.0")
		if err := s.Report(d, 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	mustCreateActivity(t, s, "a1", "2.0", devices, 2)
	cur := upWinStart.Add(time.Minute)
	advance := func() {
		t.Helper()
		if err := s.Advance("a1", cur); err != nil {
			t.Fatal(err)
		}
		cur = cur.Add(time.Minute)
	}
	// 第一次推进：只放行第 0 批
	advance()
	v, _ := s.GetActivity("a1")
	for _, d := range v.Batches[0] {
		if d.Status != DeviceDownloading {
			t.Fatalf("batch 0 should be downloading: %+v", d)
		}
	}
	for _, d := range v.Batches[1] {
		if d.Status != DevicePending {
			t.Fatalf("batch 1 should stay pending: %+v", d)
		}
	}
	// 第 0 批下载成功
	for _, d := range v.Batches[0] {
		if err := s.ReportDownload("a1", d.DeviceID, d.OpID, cur, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	cur = cur.Add(time.Minute)
	// 再次推进：第 0 批领取安装，第 1 批仍不放行
	advance()
	v, _ = s.GetActivity("a1")
	for _, d := range v.Batches[0] {
		if d.Status != DeviceInstalling {
			t.Fatalf("batch 0 should be installing: %+v", d)
		}
	}
	for _, d := range v.Batches[1] {
		if d.Status != DevicePending {
			t.Fatalf("batch 1 should stay pending: %+v", d)
		}
	}
	// 第 0 批安装成功
	for _, d := range v.Batches[0] {
		report := ReportInput{Seq: 2, Version: "2.0", Config: json.RawMessage(`{}`)}
		if err := s.ReportInstall("a1", d.DeviceID, d.OpID, cur, true, "", report); err != nil {
			t.Fatal(err)
		}
	}
	cur = cur.Add(time.Minute)
	// 再次推进：第 1 批放行下载
	advance()
	v, _ = s.GetActivity("a1")
	for _, d := range v.Batches[1] {
		if d.Status != DeviceDownloading {
			t.Fatalf("batch 1 should be downloading: %+v", d)
		}
	}
	if v.Status != ActivityActive {
		t.Fatalf("activity should still be active: %s", v.Status)
	}
	// 第 1 批也成功后活动成功
	for _, d := range v.Batches[1] {
		if err := s.ReportDownload("a1", d.DeviceID, d.OpID, cur, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	cur = cur.Add(time.Minute)
	advance()
	v, _ = s.GetActivity("a1")
	for _, d := range v.Batches[1] {
		report := ReportInput{Seq: 2, Version: "2.0", Config: json.RawMessage(`{}`)}
		if err := s.ReportInstall("a1", d.DeviceID, d.OpID, cur, true, "", report); err != nil {
			t.Fatal(err)
		}
	}
	v, _ = s.GetActivity("a1")
	if v.Status != ActivitySucceeded {
		t.Fatalf("activity should succeed: %s", v.Status)
	}
	for _, d := range v.Batches[0] {
		if d.Status != DeviceSucceeded {
			t.Fatalf("batch 0 should succeed: %+v", d)
		}
	}
	for _, d := range v.Batches[1] {
		if d.Status != DeviceSucceeded {
			t.Fatalf("batch 1 should succeed: %+v", d)
		}
	}
}

func TestFailureSkipsLaterBatches(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	devices := []string{"d1", "d2", "d3", "d4"}
	for _, d := range devices {
		mustRegister(t, s, d, "1.0")
		if err := s.Report(d, 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	mustCreateActivity(t, s, "a1", "2.0", devices, 2)
	if err := s.Advance("a1", upWinStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetActivity("a1")
	// d1 下载失败
	d1 := v.Batches[0][0]
	if err := s.ReportDownload("a1", d1.DeviceID, d1.OpID, upWinStart.Add(2*time.Minute), false, "download error"); err != nil {
		t.Fatal(err)
	}
	v, _ = s.GetActivity("a1")
	// d1 失败：阶段、原因、时间
	if v.Batches[0][0].Status != DeviceFailed || v.Batches[0][0].FailStage != StageDownload ||
		v.Batches[0][0].FailReason != "download error" || v.Batches[0][0].FailTime.IsZero() {
		t.Fatalf("d1 should be failed: %+v", v.Batches[0][0])
	}
	// 后续批次全部记为未执行
	for _, d := range v.Batches[1] {
		if d.Status != DeviceSkipped {
			t.Fatalf("later batch should be skipped: %+v", d)
		}
	}
	// 本批其他设备继续完成
	d2 := v.Batches[0][1]
	if d2.Status != DeviceDownloading {
		t.Fatalf("d2 should still be downloading: %+v", d2)
	}
	if err := s.ReportDownload("a1", d2.DeviceID, d2.OpID, upWinStart.Add(3*time.Minute), true, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Advance("a1", upWinStart.Add(4*time.Minute)); err != nil {
		t.Fatal(err)
	}
	v, _ = s.GetActivity("a1")
	d2 = v.Batches[0][1]
	if d2.Status != DeviceInstalling {
		t.Fatalf("d2 should claim install: %+v", d2)
	}
	report := ReportInput{Seq: 2, Version: "2.0", Config: json.RawMessage(`{}`)}
	if err := s.ReportInstall("a1", d2.DeviceID, d2.OpID, upWinStart.Add(5*time.Minute), true, "", report); err != nil {
		t.Fatal(err)
	}
	v, _ = s.GetActivity("a1")
	// 活动最终失败
	if v.Status != ActivityFailed {
		t.Fatalf("activity should fail: %s", v.Status)
	}
	if v.Batches[0][1].Status != DeviceSucceeded {
		t.Fatalf("d2 should succeed: %+v", v.Batches[0][1])
	}
	for _, d := range v.Batches[1] {
		if d.Status != DeviceSkipped {
			t.Fatalf("later batch should stay skipped: %+v", d)
		}
	}
}

func TestDeadlineTimeoutsUnfinished(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	devices := []string{"d1", "d2", "d3"}
	for _, d := range devices {
		mustRegister(t, s, d, "1.0")
		if err := s.Report(d, 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	mustCreateActivity(t, s, "a1", "2.0", devices, 2)
	if err := s.Advance("a1", upWinStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	// 达到截止时间：所有未结束设备记为超时
	if err := s.Advance("a1", upDeadline); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetActivity("a1")
	if v.Status != ActivityFailed {
		t.Fatalf("activity should fail: %s", v.Status)
	}
	for _, b := range v.Batches {
		for _, d := range b {
			if d.Status != DeviceTimeout || d.FailReason != "timeout" || d.FailTime.IsZero() {
				t.Fatalf("device should timeout: %+v", d)
			}
		}
	}
	// 不再派发操作
	if err := s.Advance("a1", upDeadline.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	v, _ = s.GetActivity("a1")
	for _, b := range v.Batches {
		for _, d := range b {
			if d.Status != DeviceTimeout {
				t.Fatalf("device should stay timeout: %+v", d)
			}
		}
	}
}

func TestDeadlineKeepsTerminalStates(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	devices := []string{"d1", "d2"}
	for _, d := range devices {
		mustRegister(t, s, d, "1.0")
		if err := s.Report(d, 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	mustCreateActivity(t, s, "a1", "2.0", devices, 2)
	if err := s.Advance("a1", upWinStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetActivity("a1")
	// d1 下载成功
	if err := s.ReportDownload("a1", v.Batches[0][0].DeviceID, v.Batches[0][0].OpID, upWinStart.Add(2*time.Minute), true, ""); err != nil {
		t.Fatal(err)
	}
	// d2 下载失败（终态）
	if err := s.ReportDownload("a1", v.Batches[0][1].DeviceID, v.Batches[0][1].OpID, upWinStart.Add(2*time.Minute), false, "boom"); err != nil {
		t.Fatal(err)
	}
	// d1 领取安装并成功（终态）
	if err := s.Advance("a1", upWinStart.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	v, _ = s.GetActivity("a1")
	report := ReportInput{Seq: 2, Version: "2.0", Config: json.RawMessage(`{}`)}
	if err := s.ReportInstall("a1", v.Batches[0][0].DeviceID, v.Batches[0][0].OpID, upWinStart.Add(4*time.Minute), true, "", report); err != nil {
		t.Fatal(err)
	}
	// 达到截止：已有终态保持不变
	if err := s.Advance("a1", upDeadline); err != nil {
		t.Fatal(err)
	}
	v, _ = s.GetActivity("a1")
	if v.Status != ActivityFailed {
		t.Fatalf("activity should fail: %s", v.Status)
	}
	if v.Batches[0][0].Status != DeviceSucceeded {
		t.Fatalf("d1 should stay succeeded: %+v", v.Batches[0][0])
	}
	if v.Batches[0][1].Status != DeviceFailed {
		t.Fatalf("d2 should stay failed: %+v", v.Batches[0][1])
	}
}

func TestResultIdempotencyAndConflicts(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Report("dev-1", 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	mustCreateActivity(t, s, "a1", "2.0", []string{"dev-1"}, 1)
	if err := s.Advance("a1", upWinStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetActivity("a1")
	opID := v.Batches[0][0].OpID
	at := upWinStart.Add(2 * time.Minute)

	// 相同操作的相同结果重复提交：成功返回，不增加历史
	if err := s.ReportDownload("a1", "dev-1", opID, at, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.ReportDownload("a1", "dev-1", opID, at, true, ""); err != nil {
		t.Fatalf("duplicate should succeed: %v", err)
	}
	v, _ = s.GetActivity("a1")
	if len(v.History) != 1 {
		t.Fatalf("history should not grow: %d", len(v.History))
	}
	// 改用不同结果：报错且不改状态
	if err := s.ReportDownload("a1", "dev-1", opID, at, false, "different"); !errors.Is(err, ErrOperationConflict) {
		t.Fatalf("different result: %v", err)
	}
	v, _ = s.GetActivity("a1")
	if v.Batches[0][0].Status != DeviceDownloaded {
		t.Fatalf("state changed by conflict: %+v", v.Batches[0][0])
	}
	// 跳过阶段：下载已完成，提交安装结果但用下载的 opID
	if err := s.ReportInstall("a1", "dev-1", opID, at, true, "", ReportInput{Seq: 2, Version: "2.0", Config: json.RawMessage(`{}`)}); !errors.Is(err, ErrInvalidOperation) {
		t.Fatalf("skip stage: %v", err)
	}
	// 提交其他设备的操作
	if err := s.ReportDownload("a1", "dev-2", opID, at, true, ""); !errors.Is(err, ErrDeviceNotInActivity) {
		t.Fatalf("other device: %v", err)
	}
	// 时间缺失
	if err := s.ReportDownload("a1", "dev-1", opID, time.Time{}, true, ""); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("zero time: %v", err)
	}
	// 时间倒退
	if err := s.ReportDownload("a1", "dev-1", opID, at.Add(-time.Minute), true, ""); !errors.Is(err, ErrTimeBackwards) {
		t.Fatalf("backwards: %v", err)
	}
}

func TestInstallSuccessUpdatesShadow(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Report("dev-1", 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	mustCreateActivity(t, s, "a1", "2.0", []string{"dev-1"}, 1)
	if err := s.Advance("a1", upWinStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetActivity("a1")
	downloadOp := v.Batches[0][0].OpID
	if err := s.ReportDownload("a1", "dev-1", downloadOp, upWinStart.Add(2*time.Minute), true, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Advance("a1", upWinStart.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	v, _ = s.GetActivity("a1")
	installOp := v.Batches[0][0].OpID
	if installOp == downloadOp {
		t.Fatalf("install op should differ from download op")
	}
	// 安装成功须附带版本等于目标版本的设备上报
	report := ReportInput{Seq: 2, Version: "2.0", Config: json.RawMessage(`{"mode":"auto"}`)}
	if err := s.ReportInstall("a1", "dev-1", installOp, upWinStart.Add(4*time.Minute), true, "", report); err != nil {
		t.Fatal(err)
	}
	// 影子与活动同时更新
	view, _ := s.Get("dev-1")
	if view.Version != "2.0" || view.LastSeq != 2 || string(view.Reported) != `{"mode":"auto"}` {
		t.Fatalf("shadow not updated: %+v", view)
	}
	v, _ = s.GetActivity("a1")
	if v.Status != ActivitySucceeded || v.Batches[0][0].Status != DeviceSucceeded {
		t.Fatalf("activity not succeeded: %+v", v.Batches[0][0])
	}
	// 历史记录含上报序号
	var found bool
	for _, h := range v.History {
		if h.Stage == StageInstall && h.Success && h.Seq == 2 {
			found = true
		}
	}
	if !found {
		t.Fatalf("install history missing: %+v", v.History)
	}
	// 安装成功但上报版本不等于目标版本：报错且不改状态
	s2, _ := openTemp(t)
	if err := s2.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s2, "dev-1", "1.0")
	if err := s2.Report("dev-1", 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	mustCreateActivity(t, s2, "a1", "2.0", []string{"dev-1"}, 1)
	if err := s2.Advance("a1", upWinStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	v2, _ := s2.GetActivity("a1")
	dl := v2.Batches[0][0].OpID
	if err := s2.ReportDownload("a1", "dev-1", dl, upWinStart.Add(2*time.Minute), true, ""); err != nil {
		t.Fatal(err)
	}
	if err := s2.Advance("a1", upWinStart.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	v2, _ = s2.GetActivity("a1")
	il := v2.Batches[0][0].OpID
	bad := ReportInput{Seq: 2, Version: "1.0", Config: json.RawMessage(`{}`)}
	if err := s2.ReportInstall("a1", "dev-1", il, upWinStart.Add(4*time.Minute), true, "", bad); !errors.Is(err, ErrDeviceIncompatible) {
		t.Fatalf("version mismatch: %v", err)
	}
	view2, _ := s2.Get("dev-1")
	if view2.Version != "1.0" {
		t.Fatalf("shadow should not change: %+v", view2)
	}
}

func TestOrdinaryReportDoesNotAdvanceActivity(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Report("dev-1", 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	mustCreateActivity(t, s, "a1", "2.0", []string{"dev-1"}, 1)
	// 普通上报不能推进活动
	if err := s.Report("dev-1", 2, upBase.Add(time.Minute), "2.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	info, _ := s.GetDeviceUpgrade("dev-1")
	if info.Status != DevicePending || info.OpID != "" {
		t.Fatalf("activity should not advance: %+v", info)
	}
	v, _ := s.GetActivity("a1")
	if v.Status != ActivityActive {
		t.Fatalf("activity should stay active: %s", v.Status)
	}
}

func TestResultAfterActivityEnds(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Report("dev-1", 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	mustCreateActivity(t, s, "a1", "2.0", []string{"dev-1"}, 1)
	if err := s.Advance("a1", upWinStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetActivity("a1")
	opID := v.Batches[0][0].OpID
	at := upWinStart.Add(2 * time.Minute)
	if err := s.ReportDownload("a1", "dev-1", opID, at, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Advance("a1", upDeadline); err != nil {
		t.Fatal(err)
	}
	// 活动结束后的新结果报错
	if err := s.ReportDownload("a1", "dev-1", opID, upDeadline.Add(time.Minute), false, "new"); !errors.Is(err, ErrActivityEnded) {
		t.Fatalf("new result after end: %v", err)
	}
	// 已接受结果的重复提交在活动结束后仍有效
	if err := s.ReportDownload("a1", "dev-1", opID, at, true, ""); err != nil {
		t.Fatalf("duplicate after end should succeed: %v", err)
	}
}

func TestGetDeviceUpgrade(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Report("dev-1", 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	mustCreateActivity(t, s, "a1", "2.0", []string{"dev-1"}, 1)
	info, err := s.GetDeviceUpgrade("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Version != "1.0" || info.ActivityID != "a1" || info.Status != DevicePending || info.OpID != "" {
		t.Fatalf("initial info: %+v", info)
	}
	if err := s.Advance("a1", upWinStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	info, _ = s.GetDeviceUpgrade("dev-1")
	if info.Version != "1.0" || info.Status != DeviceDownloading || info.Stage != StageDownload || info.OpID == "" {
		t.Fatalf("after claim: %+v", info)
	}
	// 未登记设备
	if _, err := s.GetDeviceUpgrade("ghost"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("ghost: %v", err)
	}
}

func TestUpgradePersistenceReopen(t *testing.T) {
	s, dir := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0", "1.1"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Report("dev-1", 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	mustCreateActivity(t, s, "a1", "2.0", []string{"dev-1"}, 1)
	if err := s.Advance("a1", upWinStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetActivity("a1")
	opID := v.Batches[0][0].OpID
	if err := s.ReportDownload("a1", "dev-1", opID, upWinStart.Add(2*time.Minute), true, ""); err != nil {
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
	// 版本记录保留
	got, _ := s2.GetUpgrade("2.0")
	if len(got) != 2 || got[0] != "1.0" || got[1] != "1.1" {
		t.Fatalf("upgrades restored: %v", got)
	}
	// 活动进度保留
	v2, err := s2.GetActivity("a1")
	if err != nil {
		t.Fatal(err)
	}
	if v2.Status != ActivityActive || v2.Batches[0][0].Status != DeviceDownloaded {
		t.Fatalf("activity restored: %+v", v2.Batches[0][0])
	}
	// 操作标识保留在历史中
	if len(v2.History) != 1 || v2.History[0].OpID != opID {
		t.Fatalf("op id restored in history: %+v", v2.History)
	}
	// 重复结果判断仍有效
	if err := s2.ReportDownload("a1", "dev-1", opID, upWinStart.Add(2*time.Minute), true, ""); err != nil {
		t.Fatalf("duplicate after reopen: %v", err)
	}
	// 重开后继续推进
	if err := s2.Advance("a1", upWinStart.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	v2, _ = s2.GetActivity("a1")
	if v2.Batches[0][0].Status != DeviceInstalling {
		t.Fatalf("should claim install after reopen: %+v", v2.Batches[0][0])
	}
}

func TestUpgradeSaveFailureRollsBack(t *testing.T) {
	s, dir := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Report("dev-1", 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	// 用同名目录堵住存储文件路径，使原子替换失败
	if err := os.Remove(filepath.Join(dir, storeFileName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, storeFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateActivity("a1", "2.0", "op", upCreated, []string{"dev-1"}, 1, upWinStart, upWinEnd, upDeadline); err == nil {
		t.Fatal("expected save error")
	}
	// 查询仍显示操作前的结果：活动不存在
	if _, err := s.GetActivity("a1"); !errors.Is(err, ErrActivityNotFound) {
		t.Fatalf("activity should not exist after rollback: %v", err)
	}
	// 版本记录、影子保持原状
	got, _ := s.GetUpgrade("2.0")
	if len(got) != 1 {
		t.Fatalf("upgrade rolled back: %v", got)
	}
	view, _ := s.Get("dev-1")
	if view.Version != "1.0" {
		t.Fatalf("shadow rolled back: %+v", view)
	}
}

func TestFormat1StoreOpens(t *testing.T) {
	dir := t.TempDir()
	// 写入一个 format 1 的存储文件
	data := `{"format":1,"devices":{"dev-1":{"version":"1.0","online":true,"revision":0,"desired":{},"reported":{},"lastSeq":1}}}`
	if err := os.WriteFile(filepath.Join(dir, storeFileName), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open format 1: %v", err)
	}
	defer s.Close()
	// 原有设备、配置保留
	view, err := s.Get("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if view.Version != "1.0" || !view.Online {
		t.Fatalf("device restored: %+v", view)
	}
	// 现有公开入口继续可用
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	mustCreateActivity(t, s, "a1", "2.0", []string{"dev-1"}, 1)
	if err := s.Advance("a1", upWinStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	info, _ := s.GetDeviceUpgrade("dev-1")
	if info.Status != DeviceDownloading {
		t.Fatalf("should claim download: %+v", info)
	}
}

func TestWindowBoundary(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Report("dev-1", 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	// 截止时间设为两天后，使次日窗口仍在截止前
	deadline := upBase.Add(48 * time.Hour)
	if _, err := s.CreateActivity("a1", "2.0", "op", upCreated, []string{"dev-1"}, 1, upWinStart, upWinEnd, deadline); err != nil {
		t.Fatal(err)
	}
	// 窗口开始时刻包含
	if err := s.Advance("a1", upWinStart); err != nil {
		t.Fatal(err)
	}
	info, _ := s.GetDeviceUpgrade("dev-1")
	if info.Status != DeviceDownloading {
		t.Fatalf("window start should be included: %+v", info)
	}
	// 已领取的下载可在窗口外完成
	v, _ := s.GetActivity("a1")
	opID := v.Batches[0][0].OpID
	if err := s.ReportDownload("a1", "dev-1", opID, upWinEnd.Add(time.Hour), true, ""); err != nil {
		t.Fatalf("complete outside window: %v", err)
	}
	// 尚未领取的安装仍须等窗口
	if err := s.Advance("a1", upWinEnd.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	v, _ = s.GetActivity("a1")
	if v.Batches[0][0].Status != DeviceDownloaded {
		t.Fatalf("should stay downloaded outside window: %+v", v.Batches[0][0])
	}
	// 次日窗口领取安装
	nextWinStart := upWinStart.Add(24 * time.Hour)
	if err := s.Advance("a1", nextWinStart); err != nil {
		t.Fatal(err)
	}
	v, _ = s.GetActivity("a1")
	if v.Batches[0][0].Status != DeviceInstalling {
		t.Fatalf("should claim install in next window: %+v", v.Batches[0][0])
	}
}

func TestOfflineKeepsTodo(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "dev-1", "1.0")
	mustCreateActivity(t, s, "a1", "2.0", []string{"dev-1"}, 1)
	// 离线推进：保留待办
	if err := s.Advance("a1", upWinStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	info, _ := s.GetDeviceUpgrade("dev-1")
	if info.Status != DevicePending {
		t.Fatalf("offline should keep pending: %+v", info)
	}
	// 上线后继续
	if err := s.Report("dev-1", 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Advance("a1", upWinStart.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	info, _ = s.GetDeviceUpgrade("dev-1")
	if info.Status != DeviceDownloading {
		t.Fatalf("online should claim: %+v", info)
	}
}

func TestResultAtDeadline(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Report("dev-1", 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	mustCreateActivity(t, s, "a1", "2.0", []string{"dev-1"}, 1)
	if err := s.Advance("a1", upWinStart.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	v, _ := s.GetActivity("a1")
	opID := v.Batches[0][0].OpID
	// 截止时刻提交新结果：设备超时，活动失败，结果不被接受
	if err := s.ReportDownload("a1", "dev-1", opID, upDeadline, true, ""); !errors.Is(err, ErrActivityEnded) {
		t.Fatalf("result at deadline: %v", err)
	}
	v, _ = s.GetActivity("a1")
	if v.Status != ActivityFailed || v.Batches[0][0].Status != DeviceTimeout {
		t.Fatalf("should timeout: %+v", v.Batches[0][0])
	}
}

func TestConcurrentActivities(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.RegisterUpgrade("2.0", []string{"1.0"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("dev-%d", i)
		mustRegister(t, s, id, "1.0")
		if err := s.Report(id, 1, upBase, "1.0", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	// 同一设备不能同时参加两个未结束活动
	mustCreateActivity(t, s, "a1", "2.0", []string{"dev-0"}, 1)
	_, err := s.CreateActivity("a2", "2.0", "op", upCreated, []string{"dev-0"}, 1, upWinStart, upWinEnd, upDeadline)
	if !errors.Is(err, ErrDeviceInActivity) {
		t.Fatalf("device in two activities: %v", err)
	}
	// 不同设备可以并行活动
	mustCreateActivity(t, s, "a2", "2.0", []string{"dev-1"}, 1)
	v1, _ := s.GetActivity("a1")
	v2, _ := s.GetActivity("a2")
	if v1.Status != ActivityActive || v2.Status != ActivityActive {
		t.Fatalf("both should be active: %s %s", v1.Status, v2.Status)
	}
}
