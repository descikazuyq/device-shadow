package shadow

import (
	"errors"
	"testing"
	"time"
)

// 活动已因超时结束（12:10）后，用原截止时间或更晚的时间领取是纯拒绝：
// 返回空操作和 ErrCampaignEnded，不推进用于判断时间倒退的记录。
// 随后 12:15 的 AdvanceCampaign 正常返回，12:15 的领取仍是活动已结束
// 而不是时间倒退；关闭重开后被拒绝的 12:20 也不应被保存下来。
func TestClaimAfterTimeoutEndIsPureReject(t *testing.T) {
	s, dir := setupUpgradeDir(t, "d1")
	spec := endedSpec([]string{"d1"}, 1, false)
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute)) // 12:02 已领取下载，未完成
	if err != nil || dl == nil || dl.Kind != StageDownload {
		t.Fatalf("claim download: %v %+v", err, dl)
	}
	end := upBase.Add(10 * time.Minute) // 12:10 截止并超时结束
	if err := s.AdvanceCampaign(spec.ID, end); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetCampaign(spec.ID)
	if !before.Ended || !before.EndedAt.Equal(end) {
		t.Fatalf("campaign not ended at deadline: %+v", before)
	}
	shadowBefore, _ := s.Get("d1")

	// 12:20 的领取：空操作 + ErrCampaignEnded，且是纯拒绝。
	op, err := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute))
	if !errors.Is(err, ErrCampaignEnded) || op != nil {
		t.Fatalf("claim after end: op %+v err %v", op, err)
	}
	after, _ := s.GetCampaign(spec.ID)
	if !after.Ended || after.Status != CampaignFailed || !after.EndedAt.Equal(end) {
		t.Fatalf("campaign conclusion/end time changed: %+v", after)
	}
	d := findDevice(after, "d1")
	if d.Status != DeviceTimeout || d.Phase != StageDownload ||
		d.Reason == "" || !d.At.Equal(end) {
		t.Fatalf("device state changed by rejected claim: %+v", d)
	}
	if len(after.Results) != len(before.Results) {
		t.Fatalf("rejected claim added history: %d -> %d", len(before.Results), len(after.Results))
	}
	sh, _ := s.Get("d1")
	if sh.Version != shadowBefore.Version || sh.Online != shadowBefore.Online ||
		sh.Revision != shadowBefore.Revision || sh.LastSeq != shadowBefore.LastSeq ||
		!rawEqual(sh.Desired, shadowBefore.Desired) || !rawEqual(sh.Reported, shadowBefore.Reported) {
		t.Fatalf("shadow changed by rejected claim: before %+v after %+v", shadowBefore, sh)
	}

	// 被拒绝的 12:20 不计入活动时间：12:15 推进正常，12:15 领取是活动已结束。
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(15*time.Minute)); err != nil {
		t.Fatalf("advance after rejected claim must not regress: %v", err)
	}
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(15*time.Minute)); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("claim at 12:15: %v", err)
	}
	// 早于真正已接受时间（12:10）仍判倒退，且不改变记录。
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute)); !errors.Is(err, ErrTimeRegression) {
		t.Fatalf("claim before accepted time: %v", err)
	}
	// 领取时间缺失仍返回 ErrInvalidTime。
	if _, err := s.Claim(spec.ID, "d1", time.Time{}); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("claim without time: %v", err)
	}

	// 关闭重开后时间判断与拒绝领取前一致：被拒绝的 12:20 不能被保存下来。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if err := s2.AdvanceCampaign(spec.ID, upBase.Add(15*time.Minute)); err != nil {
		t.Fatalf("rejected claim time persisted across reopen: %v", err)
	}
	if _, err := s2.Claim(spec.ID, "d1", upBase.Add(15*time.Minute)); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("claim after reopen: %v", err)
	}
	cv, _ := s2.GetCampaign(spec.ID)
	if !cv.Ended || !cv.EndedAt.Equal(end) || findDevice(cv, "d1").Status != DeviceTimeout {
		t.Fatalf("reopen changed ended campaign: %+v", cv)
	}
}

// 截止前已成功结束的活动：无论领取时间在原截止前、恰好截止或截止后，
// 拒绝都不得改变活动结论、结束时间与时间基线。
func TestClaimAfterEarlySuccessIsPureReject(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	finishDevice(t, s, spec, "d1", upBase.Add(2*time.Minute)) // 12:05 成功结束
	before, _ := s.GetCampaign(spec.ID)
	if !before.Ended || before.Status != CampaignSucceeded {
		t.Fatalf("campaign not succeeded early: %+v", before)
	}
	end := before.EndedAt

	for _, at := range []time.Time{
		upBase.Add(30 * time.Minute), // 截止前
		spec.Deadline,                // 恰好截止
		spec.Deadline.Add(time.Hour), // 截止后
	} {
		op, err := s.Claim(spec.ID, "d1", at)
		if !errors.Is(err, ErrCampaignEnded) || op != nil {
			t.Fatalf("claim at %v: op %+v err %v", at, op, err)
		}
	}
	after, _ := s.GetCampaign(spec.ID)
	if !after.Ended || after.Status != CampaignSucceeded || !after.EndedAt.Equal(end) ||
		len(after.Results) != len(before.Results) {
		t.Fatalf("rejected claims changed campaign: %+v", after)
	}
	// 截止后的被拒绝领取不推进基线：截止前的合法时间仍不判倒退。
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(30*time.Minute)); err != nil {
		t.Fatalf("rejected claim advanced time baseline: %v", err)
	}
}

// 截止前已失败结束的活动同样适用：截止后的领取是纯拒绝，不推进基线。
func TestClaimAfterEarlyFailureIsPureReject(t *testing.T) {
	s := setupUpgrade(t, "d1")
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
		At: upBase.Add(3 * time.Minute), Success: false, Reason: "download broken",
	}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.GetCampaign(spec.ID)
	if !before.Ended || before.Status != CampaignFailed {
		t.Fatalf("campaign not failed early: %+v", before)
	}

	op, err := s.Claim(spec.ID, "d1", spec.Deadline.Add(time.Hour))
	if !errors.Is(err, ErrCampaignEnded) || op != nil {
		t.Fatalf("claim after failed campaign: op %+v err %v", op, err)
	}
	after, _ := s.GetCampaign(spec.ID)
	if !after.Ended || after.Status != CampaignFailed || !after.EndedAt.Equal(before.EndedAt) ||
		len(after.Results) != len(before.Results) {
		t.Fatalf("rejected claim changed campaign: %+v", after)
	}
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(30*time.Minute)); err != nil {
		t.Fatalf("rejected claim advanced time baseline: %v", err)
	}
}

// 回滚已领取但未完成，活动超时结束后领取不能再返回该回滚操作；
// 等待回滚（未领取）的设备同样只能得到纯拒绝。
func TestClaimAfterEndReturnsNoRollbackOp(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2")
	spec := endedSpec([]string{"d1", "d2"}, 2, true)
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	awaitRollback(t, s, spec, "d1", upBase.Add(1*time.Minute))
	rb, err := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute)) // d1 正在回滚
	if err != nil || rb == nil || rb.Kind != StageRollback {
		t.Fatalf("claim rollback: %v %+v", err, rb)
	}
	awaitRollback(t, s, spec, "d2", upBase.Add(5*time.Minute)) // d2 等待回滚

	end := upBase.Add(10 * time.Minute)
	if err := s.AdvanceCampaign(spec.ID, end); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"d1", "d2"} {
		op, err := s.Claim(spec.ID, id, upBase.Add(20*time.Minute))
		if !errors.Is(err, ErrCampaignEnded) || op != nil {
			t.Fatalf("claim %s after end: op %+v err %v", id, op, err)
		}
	}
	cv, _ := s.GetCampaign(spec.ID)
	if findDevice(cv, "d1").Status != DeviceRollbackTimeout ||
		findDevice(cv, "d2").Status != DeviceRollbackTimeout {
		t.Fatalf("rejected claims changed rollback timeout state: %+v", cv)
	}
	// 纯拒绝不推进基线。
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(15*time.Minute)); err != nil {
		t.Fatalf("rejected claim advanced time baseline: %v", err)
	}
	// 活动结束后设备查询也不再给出待办操作。
	for _, id := range []string{"d1", "d2"} {
		w, err := s.GetDeviceWork(id)
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending != nil || w.CampaignID != "" {
			t.Fatalf("device %s still has work after end: %+v", id, w)
		}
	}
}

// 保留活动仍在执行时领取首次到达截止时间的原有功能：不派发操作，
// 全部未结束设备按阶段记为超时，已有终态保持不变，以本次领取时间结束活动；
// 即使领取的设备早已成功，其他未结束设备也必须被处理；此次结束时间参与倒退判断。
func TestFirstClaimAtDeadlineStillCascades(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2")
	spec := endedSpec([]string{"d1", "d2"}, 1, false)
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	finishDevice(t, s, spec, "d1", upBase.Add(1*time.Minute)) // d1 已成功
	dl2, err := s.Claim(spec.ID, "d2", upBase.Add(5*time.Minute))
	if err != nil || dl2 == nil {
		t.Fatalf("claim download d2: %v %+v", err, dl2)
	}
	end := upBase.Add(10 * time.Minute)
	// 由早已成功的 d1 在截止时刻领取：仍要级联处理 d2。
	op, err := s.Claim(spec.ID, "d1", end)
	if !errors.Is(err, ErrCampaignEnded) || op != nil {
		t.Fatalf("claim at deadline: op %+v err %v", op, err)
	}
	cv, _ := s.GetCampaign(spec.ID)
	if !cv.Ended || cv.Status != CampaignFailed || !cv.EndedAt.Equal(end) {
		t.Fatalf("deadline claim did not end campaign: %+v", cv)
	}
	if findDevice(cv, "d1").Status != DeviceSucceeded {
		t.Fatalf("terminal device changed: %+v", findDevice(cv, "d1"))
	}
	d2 := findDevice(cv, "d2")
	if d2.Status != DeviceTimeout || d2.Phase != StageDownload || !d2.At.Equal(end) {
		t.Fatalf("unfinished device not timed out: %+v", d2)
	}
	// 此次结束时间参与倒退判断。
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(5*time.Minute)); !errors.Is(err, ErrTimeRegression) {
		t.Fatalf("deadline claim time must join regression check: %v", err)
	}
	// 之后的领取是纯拒绝，不再推进基线。
	if _, err := s.Claim(spec.ID, "d2", upBase.Add(20*time.Minute)); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("claim after deadline end: %v", err)
	}
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(15*time.Minute)); err != nil {
		t.Fatalf("rejected claim advanced time baseline: %v", err)
	}
}
