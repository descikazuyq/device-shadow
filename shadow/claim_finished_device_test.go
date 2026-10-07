package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 活动仍在执行时，已成功设备在截止前再次领取是纯拒绝：返回空操作和
// ErrDeviceFinished，不推进时间基线、不改状态、不增加结果历史、不改影子、
// 不替换存储文件，也不重新派发操作；与设备是否在线、领取时间是否在维护
// 窗口内无关。存储目录不可写时同样返回 ErrDeviceFinished 而不是保存错误。
// 被拒绝的时间不成为新的已接受时间：另一台设备仍可按原有条件在更早的
// 合法时间领取；真正早于已接受时间的请求仍判倒退。
func TestClaimFinishedDeviceIsPureReject(t *testing.T) {
	s, dir := setupUpgradeDir(t, "d1", "d2")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	finishDevice(t, s, spec, "d1", upBase.Add(1*time.Minute)) // d1 于 12:04 成功，活动仍在执行

	before, _ := s.GetCampaign(spec.ID)
	if before.Ended {
		t.Fatalf("campaign should still be running: %+v", before)
	}
	storeFile := filepath.Join(dir, storeFileName)

	// 在线且位于窗口内的领取：纯拒绝。
	op, err := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute))
	if !errors.Is(err, ErrDeviceFinished) || op != nil {
		t.Fatalf("claim finished device: op %+v err %v", op, err)
	}
	// 离线且位于窗口外（截止前）的领取：同样的纯拒绝，不是 (nil, nil)。
	if err := s.SetOffline("d1"); err != nil {
		t.Fatal(err)
	}
	shadowBefore, _ := s.Get("d1")
	fileBefore, err := os.ReadFile(storeFile)
	if err != nil {
		t.Fatal(err)
	}
	op, err = s.Claim(spec.ID, "d1", upBase.Add(90*time.Minute))
	if !errors.Is(err, ErrDeviceFinished) || op != nil {
		t.Fatalf("claim finished device offline/outside window: op %+v err %v", op, err)
	}

	// 存储目录不可写时，被拒绝的领取不尝试保存：仍返回 ErrDeviceFinished。
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	op, err = s.Claim(spec.ID, "d1", upBase.Add(25*time.Minute))
	if !errors.Is(err, ErrDeviceFinished) || op != nil {
		t.Fatalf("claim with unwritable store: op %+v err %v", op, err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// 状态、结果历史、影子与存储文件均保持原样。
	after, _ := s.GetCampaign(spec.ID)
	d := findDevice(after, "d1")
	if d.Status != DeviceSucceeded || d.Phase != StageInstall ||
		!d.At.Equal(upBase.Add(4*time.Minute)) {
		t.Fatalf("finished device state changed: %+v", d)
	}
	if len(after.Results) != len(before.Results) {
		t.Fatalf("rejected claims added history: %d -> %d", len(before.Results), len(after.Results))
	}
	sh, _ := s.Get("d1")
	if sh.Version != shadowBefore.Version || sh.Online != shadowBefore.Online ||
		sh.Revision != shadowBefore.Revision || sh.LastSeq != shadowBefore.LastSeq ||
		!rawEqual(sh.Desired, shadowBefore.Desired) || !rawEqual(sh.Reported, shadowBefore.Reported) {
		t.Fatalf("shadow changed by rejected claims: before %+v after %+v", shadowBefore, sh)
	}
	fileAfter, err := os.ReadFile(storeFile)
	if err != nil {
		t.Fatal(err)
	}
	if !rawEqual(fileBefore, fileAfter) {
		t.Fatal("store file replaced by rejected claims")
	}
	// 已结束设备的待办保持为空。
	w, err := s.GetDeviceWork("d1")
	if err != nil {
		t.Fatal(err)
	}
	if w.Pending != nil || w.CampaignID != spec.ID {
		t.Fatalf("finished device work changed: %+v", w)
	}

	// 被拒绝的 12:20/12:25/13:30 不推进基线：d2 仍可在 12:15 领取。
	dl, err := s.Claim(spec.ID, "d2", upBase.Add(15*time.Minute))
	if err != nil || dl == nil || dl.Kind != StageDownload {
		t.Fatalf("rejected claims advanced time baseline: %v %+v", err, dl)
	}
	// 真正早于已接受时间（现在是 12:15）的请求仍判倒退。
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute)); !errors.Is(err, ErrTimeRegression) {
		t.Fatalf("claim before accepted time: %v", err)
	}
}

// 失败与被记为未执行的设备同样属于已结束：活动仍在执行时再次领取返回
// 空操作和 ErrDeviceFinished，且不推进时间基线。
func TestClaimFailedAndSkippedDeviceIsPureReject(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2", "d3")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2", "d3"}
	spec.BatchSize = 2 // d1、d2 首批，d3 后续批次
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	bringOnline(t, s, "d3")

	dl, err := s.Claim(spec.ID, "d1", upBase.Add(1*time.Minute))
	if err != nil || dl == nil {
		t.Fatalf("claim download d1: %v %+v", err, dl)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(2 * time.Minute), Success: false, Reason: "download broken",
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetCampaign(spec.ID)
	if before.Ended ||
		findDevice(before, "d1").Status != DeviceFailed ||
		findDevice(before, "d3").Status != DeviceSkipped {
		t.Fatalf("unexpected campaign state: %+v", before)
	}

	for _, id := range []string{"d1", "d3"} {
		op, err := s.Claim(spec.ID, id, upBase.Add(20*time.Minute))
		if !errors.Is(err, ErrDeviceFinished) || op != nil {
			t.Fatalf("claim %s: op %+v err %v", id, op, err)
		}
	}
	after, _ := s.GetCampaign(spec.ID)
	if len(after.Results) != len(before.Results) ||
		findDevice(after, "d1").Status != DeviceFailed ||
		findDevice(after, "d3").Status != DeviceSkipped {
		t.Fatalf("rejected claims changed campaign: %+v", after)
	}
	// 被拒绝的 12:20 不推进基线：d2 仍可在 12:05 领取。
	if dl2, err := s.Claim(spec.ID, "d2", upBase.Add(5*time.Minute)); err != nil || dl2 == nil {
		t.Fatalf("rejected claims advanced time baseline: %v %+v", err, dl2)
	}
}

// 已完成回滚的设备属于已结束，再次领取是纯拒绝；等待回滚和正在回滚的设备
// 仍有未完成工作，不在拒绝范围内，领取行为保持不变。
func TestClaimRollbackFinishedDeviceIsPureReject(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2", "d3")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2", "d3"}
	spec.BatchSize = 2 // d1、d2 首批，d3 后续批次（安装失败后记为未执行）
	spec.RollbackOnFailure = true
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	bringOnline(t, s, "d3")

	awaitRollback(t, s, spec, "d1", upBase.Add(1*time.Minute)) // d1 12:04 等待回滚
	rb1, err := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
	if err != nil || rb1 == nil || rb1.Kind != StageRollback {
		t.Fatalf("claim rollback d1: %v %+v", err, rb1)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb1.ID,
		At: upBase.Add(6 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("rollback result d1: %v", err)
	}
	awaitRollback(t, s, spec, "d2", upBase.Add(7*time.Minute)) // d2 12:10 等待回滚

	before, _ := s.GetCampaign(spec.ID)
	if before.Ended || findDevice(before, "d1").Status != DeviceRollbackSucceeded {
		t.Fatalf("unexpected campaign state: %+v", before)
	}

	// 已回滚成功的 d1 在 12:20 领取：纯拒绝。
	op, err := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute))
	if !errors.Is(err, ErrDeviceFinished) || op != nil {
		t.Fatalf("claim rollback-finished device: op %+v err %v", op, err)
	}
	// 被拒绝的 12:20 不推进基线：等待回滚的 d2 仍可在 12:12 领取回滚。
	rb2, err := s.Claim(spec.ID, "d2", upBase.Add(12*time.Minute))
	if err != nil || rb2 == nil || rb2.Kind != StageRollback {
		t.Fatalf("claim rollback d2: %v %+v", err, rb2)
	}
	// 正在回滚的 d2 再次查询返回同一标识，不属于已结束拒绝。
	again, err := s.Claim(spec.ID, "d2", upBase.Add(13*time.Minute))
	if err != nil || again == nil || again.ID != rb2.ID {
		t.Fatalf("re-claim rolling back device: %v %+v", err, again)
	}
	// 再次被拒绝的 12:30 也不推进基线：d2 的回滚结果可在 12:14 提交。
	op, err = s.Claim(spec.ID, "d1", upBase.Add(30*time.Minute))
	if !errors.Is(err, ErrDeviceFinished) || op != nil {
		t.Fatalf("second claim rollback-finished device: op %+v err %v", op, err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d2", OperationID: rb2.ID,
		At: upBase.Add(14 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("rejected claims advanced time baseline: %v", err)
	}

	after, _ := s.GetCampaign(spec.ID)
	d1 := findDevice(after, "d1")
	if d1.Status != DeviceRollbackSucceeded || !d1.At.Equal(upBase.Add(6*time.Minute)) ||
		len(after.Results) != len(before.Results)+1 { // 仅 d2 回滚成功新增一条
		t.Fatalf("rejected claims changed campaign: %+v", after)
	}
}
