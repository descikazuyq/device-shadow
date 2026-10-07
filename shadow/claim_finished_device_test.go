package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 活动仍在执行而其中一台设备已经结束（成功/失败/未执行/回滚结束）时，
// 截止前再次为它调用 Claim 必须是空操作的纯拒绝：返回空操作和
// ErrDeviceFinished（可用 errors.Is 判断），不重新派发操作、不推进活动的
// 已接受时间、不改设备状态与影子、不增加结果历史，也不替换存储文件。
// 被拒绝的领取时间不能成为活动新的已接受时间：最后接受时间停在 12:10 时，
// 已成功设备 12:20 的被拒绝领取，不能让另一台设备 12:15 的合法领取被判倒退。
func TestClaimFinishedDeviceIsPureReject(t *testing.T) {
	s, dir := setupUpgradeDir(t, "d1", "d2")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2"}
	spec.BatchSize = 2
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	finishDevice(t, s, spec, "d1", upBase.Add(1*time.Minute)) // d1 12:04 成功结束

	// d2 12:10 领取下载，活动的最后接受时间停在 12:10，活动仍在执行。
	dl2, err := s.Claim(spec.ID, "d2", upBase.Add(10*time.Minute))
	if err != nil || dl2 == nil || dl2.Kind != StageDownload {
		t.Fatalf("claim download d2: %v %+v", err, dl2)
	}

	before, _ := s.GetCampaign(spec.ID)
	d1Before := findDevice(before, "d1")
	d2Before := findDevice(before, "d2")
	shadowBefore, _ := s.Get("d1")
	workBefore, _ := s.GetDeviceWork("d1")
	storePath := filepath.Join(dir, storeFileName)
	storedBefore, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("read store file: %v", err)
	}
	infoBefore, err := os.Stat(storePath)
	if err != nil {
		t.Fatalf("stat store file: %v", err)
	}
	if before.Ended || d1Before.Status != DeviceSucceeded {
		t.Fatalf("setup: campaign still running with d1 succeeded: %+v", before)
	}

	// 12:20 为已成功的 d1 再次领取：空操作 + ErrDeviceFinished。
	op, err := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute))
	if !errors.Is(err, ErrDeviceFinished) || op != nil {
		t.Fatalf("claim finished device: op %+v err %v", op, err)
	}

	after, _ := s.GetCampaign(spec.ID)
	d1After := findDevice(after, "d1")
	d2After := findDevice(after, "d2")
	// 活动仍在执行，结论与结束时间不变。
	if after.Ended || after.Status != CampaignRunning || !after.EndedAt.IsZero() {
		t.Fatalf("rejected claim changed campaign: %+v", after)
	}
	// 已结束设备的状态、阶段、原因与时间保留。
	if d1After.Status != d1Before.Status || d1After.Phase != d1Before.Phase ||
		d1After.Reason != d1Before.Reason || !d1After.At.Equal(d1Before.At) {
		t.Fatalf("finished device state changed: before %+v after %+v", d1Before, d1After)
	}
	// 其他设备的状态与待办保持原样。
	if d2After.Status != d2Before.Status || d2After.Phase != d2Before.Phase ||
		!d2After.At.Equal(d2Before.At) {
		t.Fatalf("other device state changed: before %+v after %+v", d2Before, d2After)
	}
	// 结果历史不增加。
	if len(after.Results) != len(before.Results) {
		t.Fatalf("rejected claim added history: %d -> %d", len(before.Results), len(after.Results))
	}
	// 设备影子保持原样。
	sh, _ := s.Get("d1")
	if sh.Version != shadowBefore.Version || sh.Online != shadowBefore.Online ||
		sh.Revision != shadowBefore.Revision || sh.LastSeq != shadowBefore.LastSeq ||
		!rawEqual(sh.Desired, shadowBefore.Desired) || !rawEqual(sh.Reported, shadowBefore.Reported) {
		t.Fatalf("shadow changed by rejected claim: before %+v after %+v", shadowBefore, sh)
	}
	// 设备待办保持原样（活动标识仍在，但没有待执行操作）。
	workAfter, _ := s.GetDeviceWork("d1")
	if workAfter.CampaignID != workBefore.CampaignID ||
		(workAfter.Pending == nil) != (workBefore.Pending == nil) {
		t.Fatalf("device work changed: before %+v after %+v", workBefore, workAfter)
	}
	if workAfter.Pending != nil {
		t.Fatalf("finished device must have no pending op: %+v", workAfter.Pending)
	}
	// 存储文件不被替换：内容、修改时间与 inode 都保持原样。
	storedAfter, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("read store file after reject: %v", err)
	}
	if string(storedAfter) != string(storedBefore) {
		t.Fatalf("rejected claim replaced the storage file content")
	}
	infoAfter, err := os.Stat(storePath)
	if err != nil {
		t.Fatalf("stat store file after reject: %v", err)
	}
	if !infoAfter.ModTime().Equal(infoBefore.ModTime()) {
		t.Fatalf("rejected claim touched storage mtime: before %v after %v",
			infoBefore.ModTime(), infoAfter.ModTime())
	}
	if !os.SameFile(infoBefore, infoAfter) {
		t.Fatalf("rejected claim replaced the storage file inode")
	}

	// 被拒绝的 12:20 不成为活动新的已接受时间：d2 仍可在 12:15 提交结果、领取安装。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d2", OperationID: dl2.ID,
		At: upBase.Add(15 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("d2 submit at 12:15 after 12:20 rejected claim: %v", err)
	}
	in2, err := s.Claim(spec.ID, "d2", upBase.Add(16*time.Minute))
	if err != nil || in2 == nil || in2.Kind != StageInstall {
		t.Fatalf("d2 claim install at 12:16: %v %+v", err, in2)
	}

	// 真正早于已接受时间的请求仍判倒退；时间缺失仍报错，且不改变状态。
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute)); !errors.Is(err, ErrTimeRegression) {
		t.Fatalf("claim before accepted time: %v", err)
	}
	if _, err := s.Claim(spec.ID, "d1", time.Time{}); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("claim without time: %v", err)
	}

	// 拒绝只取决于设备是否终态：离线、窗口外（但早于截止）同样是 ErrDeviceFinished，
	// 不能当成暂时领不到操作而返回 (nil, nil)。
	if err := s.SetOffline("d1"); err != nil {
		t.Fatal(err)
	}
	if op, err := s.Claim(spec.ID, "d1", upBase.Add(25*time.Minute)); !errors.Is(err, ErrDeviceFinished) || op != nil {
		t.Fatalf("offline finished device claim: op %+v err %v", op, err)
	}
	if op, err := s.Claim(spec.ID, "d1", upBase.Add(90*time.Minute)); !errors.Is(err, ErrDeviceFinished) || op != nil {
		t.Fatalf("out-of-window finished device claim: op %+v err %v", op, err)
	}

	// 关闭重开后被拒绝的 12:20 仍未被保存：12:17 的推进不判倒退，
	// 12:18 为 d1 领取仍是设备已结束。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if err := s2.AdvanceCampaign(spec.ID, upBase.Add(17*time.Minute)); err != nil {
		t.Fatalf("rejected claim time persisted across reopen: %v", err)
	}
	if op, err := s2.Claim(spec.ID, "d1", upBase.Add(18*time.Minute)); !errors.Is(err, ErrDeviceFinished) || op != nil {
		t.Fatalf("claim finished device after reopen: op %+v err %v", op, err)
	}
}

// 存储目录不可写时，为已结束设备的领取仍返回 ErrDeviceFinished 而不是保存错误：
// 这种领取根本不尝试写入存储。作为对照，未结束设备的领取确实需要保存，
// 在同一只读目录下拿到保存错误并整体回滚。
func TestClaimFinishedDeviceDoesNotSave(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only directory does not block writes for root")
	}
	s, dir := setupUpgradeDir(t, "d0", "d1")
	spec := upSpec()
	spec.Devices = []string{"d0", "d1"}
	spec.BatchSize = 2
	createCampaign(t, s, spec)
	bringOnline(t, s, "d0")
	bringOnline(t, s, "d1")
	dl0, err := s.Claim(spec.ID, "d0", upBase.Add(1*time.Minute))
	if err != nil || dl0 == nil {
		t.Fatalf("claim download d0: %v %+v", err, dl0)
	}
	// d0 下载失败（终态）；同批 d1 仍待领取，活动继续执行。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d0", OperationID: dl0.ID,
		At: upBase.Add(2 * time.Minute), Success: false, Reason: "download broken",
	}); err != nil {
		t.Fatal(err)
	}
	cv, _ := s.GetCampaign(spec.ID)
	if cv.Ended || findDevice(cv, "d0").Status != DeviceFailed {
		t.Fatalf("setup: running campaign with d0 failed: %+v", cv)
	}

	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod read-only: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	// 对照：d1 的领取需要推进时间并保存，目录只读后拿到保存错误。
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(3*time.Minute)); err == nil ||
		errors.Is(err, ErrDeviceFinished) || !strings.Contains(err.Error(), "shadow: save store") {
		t.Fatalf("pending device claim should fail with save error, got %v", err)
	}
	// 保存失败整体回滚：d1 仍未领取。
	if d1 := findDevice(mustCampaign(t, s, spec.ID), "d1"); d1.Status != DevicePending {
		t.Fatalf("failed save did not roll back: %+v", d1)
	}

	// 被修复的行为：已结束设备的领取不保存，调用方拿到的是设备已结束本身。
	if op, err := s.Claim(spec.ID, "d0", upBase.Add(20*time.Minute)); !errors.Is(err, ErrDeviceFinished) || op != nil {
		t.Fatalf("finished device claim under read-only dir: op %+v err %v", op, err)
	}
}

// 各种设备终态（失败、未执行、回滚失败、回滚成功）在活动仍执行时都返回
// ErrDeviceFinished；等待回滚与正在回滚仍有未完成工作，不能被拒绝。
func TestClaimFinishedDeviceStatuses(t *testing.T) {
	// 失败与未执行：批次大小 2，d0 下载失败后同批 d1 继续、次批 d2 记为未执行，
	// 活动因 d1 尚未结束而继续。
	t.Run("failed and skipped", func(t *testing.T) {
		s := setupUpgrade(t, "d0", "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d0", "d1", "d2"}
		spec.BatchSize = 2
		createCampaign(t, s, spec)
		for _, id := range []string{"d0", "d1", "d2"} {
			bringOnline(t, s, id)
		}
		dl0, _ := s.Claim(spec.ID, "d0", upBase.Add(1*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d0", OperationID: dl0.ID,
			At: upBase.Add(2 * time.Minute), Success: false, Reason: "download broken",
		}); err != nil {
			t.Fatal(err)
		}
		cv, _ := s.GetCampaign(spec.ID)
		if cv.Ended || findDevice(cv, "d0").Status != DeviceFailed ||
			findDevice(cv, "d1").Status != DevicePending ||
			findDevice(cv, "d2").Status != DeviceSkipped {
			t.Fatalf("unexpected setup states: %+v", cv)
		}
		for _, id := range []string{"d0", "d2"} {
			if op, err := s.Claim(spec.ID, id, upBase.Add(20*time.Minute)); !errors.Is(err, ErrDeviceFinished) || op != nil {
				t.Fatalf("claim %s: op %+v err %v", id, op, err)
			}
		}
		// 被拒绝的 12:20/12:21 不推进基线：同批 d1 仍可在 12:05 领取下载。
		if op, err := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute)); err != nil || op == nil || op.Kind != StageDownload {
			t.Fatalf("d1 claim after rejected claims: %v %+v", err, op)
		}
	})

	// 回滚失败：等待回滚与正在回滚都不是终态，仍能领取/查询回滚操作；
	// 回滚失败被接受后设备才终态，再次领取被拒绝。
	t.Run("rollback failed", func(t *testing.T) {
		s := setupUpgrade(t, "d0", "d1")
		spec := upSpec()
		spec.Devices = []string{"d0", "d1"}
		spec.BatchSize = 2
		spec.RollbackOnFailure = true
		createCampaign(t, s, spec)
		bringOnline(t, s, "d0")
		bringOnline(t, s, "d1")
		awaitRollback(t, s, spec, "d0", upBase.Add(1*time.Minute))
		cv, _ := s.GetCampaign(spec.ID)
		if cv.Ended || findDevice(cv, "d0").Status != DeviceAwaitingRollback {
			t.Fatalf("setup: %+v", cv)
		}
		// 等待回滚且离线：暂时领不到操作，但不是设备已结束。
		if err := s.SetOffline("d0"); err != nil {
			t.Fatal(err)
		}
		if op, err := s.Claim(spec.ID, "d0", upBase.Add(5*time.Minute)); err != nil || op != nil {
			t.Fatalf("offline awaiting-rollback claim: op %+v err %v", op, err)
		}
		bringOnline(t, s, "d0")
		// 窗口内首次领取回滚：返回回滚操作（等待回滚不被拒绝）。
		rb, err := s.Claim(spec.ID, "d0", upBase.Add(5*time.Minute))
		if err != nil || rb == nil || rb.Kind != StageRollback {
			t.Fatalf("claim rollback: %v %+v", err, rb)
		}
		// 正在回滚再次查询：同一操作，同样不是设备已结束。
		if again, err := s.Claim(spec.ID, "d0", upBase.Add(6*time.Minute)); err != nil || again == nil || again.ID != rb.ID {
			t.Fatalf("re-claim rolling-back: %v %+v", err, again)
		}
		// 回滚失败：设备终态，活动仍因 d1 未结束而继续。
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d0", OperationID: rb.ID,
			At: upBase.Add(7 * time.Minute), Success: false, Reason: "rollback broken",
		}); err != nil {
			t.Fatal(err)
		}
		if cv, _ := s.GetCampaign(spec.ID); cv.Ended || findDevice(cv, "d0").Status != DeviceRollbackFailed {
			t.Fatalf("after rollback failure: %+v", cv)
		}
		if op, err := s.Claim(spec.ID, "d0", upBase.Add(20*time.Minute)); !errors.Is(err, ErrDeviceFinished) || op != nil {
			t.Fatalf("claim rollback-failed device: op %+v err %v", op, err)
		}
		// d1 仍可在早于被拒绝时间的 12:08 领取，未被判倒退。
		if op, err := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute)); err != nil || op == nil || op.Kind != StageDownload {
			t.Fatalf("d1 claim after rejected claim: %v %+v", err, op)
		}
	})

	// 回滚成功：设备以回滚成功结束（不算升级成功），活动仍在等待 d1，
	// 再次领取返回 ErrDeviceFinished。
	t.Run("rollback succeeded", func(t *testing.T) {
		s := setupUpgrade(t, "d0", "d1")
		spec := upSpec()
		spec.Devices = []string{"d0", "d1"}
		spec.BatchSize = 2
		spec.RollbackOnFailure = true
		createCampaign(t, s, spec)
		bringOnline(t, s, "d0")
		bringOnline(t, s, "d1")
		awaitRollback(t, s, spec, "d0", upBase.Add(1*time.Minute))
		rb, err := s.Claim(spec.ID, "d0", upBase.Add(5*time.Minute))
		if err != nil || rb == nil || rb.Kind != StageRollback {
			t.Fatalf("claim rollback: %v %+v", err, rb)
		}
		// 回滚成功上报的版本必须是锁定的回滚目标（首次领取下载时为 v1）。
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d0", OperationID: rb.ID,
			At: upBase.Add(6 * time.Minute), Success: true,
			Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
		if cv, _ := s.GetCampaign(spec.ID); cv.Ended || findDevice(cv, "d0").Status != DeviceRollbackSucceeded {
			t.Fatalf("after rollback success: %+v", cv)
		}
		if op, err := s.Claim(spec.ID, "d0", upBase.Add(20*time.Minute)); !errors.Is(err, ErrDeviceFinished) || op != nil {
			t.Fatalf("claim rollback-succeeded device: op %+v err %v", op, err)
		}
		if op, err := s.Claim(spec.ID, "d1", upBase.Add(7*time.Minute)); err != nil || op == nil || op.Kind != StageDownload {
			t.Fatalf("d1 claim after rejected claim: %v %+v", err, op)
		}
	})
}

// mustCampaign 读取活动视图，失败时终止测试。
func mustCampaign(t *testing.T, s *Store, id string) CampaignView {
	t.Helper()
	v, err := s.GetCampaign(id)
	if err != nil {
		t.Fatalf("GetCampaign: %v", err)
	}
	return v
}
