package shadow

import (
	"errors"
	"testing"
	"time"
)

// 报告的核心场景：活动 12:10 已超时结束后，设备在 12:20 领取必须是空操作加
// ErrCampaignEnded 的纯拒绝，不能把时间基线推进到 12:20；随后 12:15 的
// AdvanceCampaign 与再次领取都应正常按“活动已结束”处理，而不是时间倒退。
// 活动结论、结束时间、设备状态与状态时间、结果历史、影子均不被拒绝改变，
// 关闭重开后判断保持一致。
func TestClaimAfterTimeoutIsPureReject(t *testing.T) {
	s, dir := setupUpgradeDir(t, "d1")
	spec := endedSpec([]string{"d1"}, 1, false)
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	claimInstall(t, s, spec, "d1", upBase.Add(2*time.Minute)) // 12:04 已领取安装

	end := upBase.Add(10 * time.Minute) // 12:10
	if err := s.AdvanceCampaign(spec.ID, end); err != nil {
		t.Fatalf("advance to deadline: %v", err)
	}
	before, _ := s.GetCampaign(spec.ID)
	shadowBefore, _ := s.Get("d1")

	// 12:20 领取（设备已有已领取未完成的安装）：无操作、ErrCampaignEnded。
	op, err := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute))
	if !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("claim after end: %v", err)
	}
	if op != nil {
		t.Fatalf("claimed unfinished op must not be returned after end: %+v", op)
	}

	// 被拒绝的 12:20 不得推进时间基线：12:15 的推进必须成功（空操作）。
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(15*time.Minute)); err != nil {
		t.Fatalf("advance after rejected claim must not regress: %v", err)
	}
	// 12:15 再领取同样是活动已结束，而不是时间倒退。
	if op, err := s.Claim(spec.ID, "d1", upBase.Add(15*time.Minute)); !errors.Is(err, ErrCampaignEnded) || op != nil {
		t.Fatalf("claim at 12:15: op=%+v err=%v", op, err)
	}

	after, _ := s.GetCampaign(spec.ID)
	if !after.Ended || after.Status != CampaignFailed || !after.EndedAt.Equal(end) {
		t.Fatalf("campaign conclusion/end time changed: %+v", after)
	}
	d := findDevice(after, "d1")
	if d.Status != DeviceTimeout || d.Phase != StageInstall ||
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

	// 关闭重开后时间判断与拒绝领取前一致：被拒绝的 12:20 没有落盘。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if err := s2.AdvanceCampaign(spec.ID, upBase.Add(15*time.Minute)); err != nil {
		t.Fatalf("advance after reopen must not regress: %v", err)
	}
	reopened, _ := s2.GetCampaign(spec.ID)
	if !reopened.Ended || !reopened.EndedAt.Equal(end) ||
		findDevice(reopened, "d1").Status != DeviceTimeout {
		t.Fatalf("campaign changed across reopen: %+v", reopened)
	}
}

// 截止前已成功或失败结束的活动：领取时间在原截止前、恰好截止或截止后，
// 拒绝都不得推进时间基线，也不得改变结论、结束时间、设备状态、历史与影子。
func TestClaimAgainstEarlyEndedCampaignIsPureReject(t *testing.T) {
	cases := []struct {
		name      string
		rollback  bool
		setup     func(t *testing.T, s *Store, spec CampaignSpec) // 在 12:05 前令活动结束
		wantState string
	}{
		{
			name:      "succeeded before deadline",
			wantState: DeviceSucceeded,
			setup: func(t *testing.T, s *Store, spec CampaignSpec) {
				finishDevice(t, s, spec, "d1", upBase.Add(2*time.Minute)) // 12:05 全部成功
			},
		},
		{
			name:      "failed before deadline",
			wantState: DeviceFailed,
			setup: func(t *testing.T, s *Store, spec CampaignSpec) {
				claimInstall(t, s, spec, "d1", upBase.Add(2*time.Minute)) // 12:04 已领取安装
				in := findDevice(mustGetCampaign(s, spec.ID), "d1").InstallID
				if err := s.SubmitResult(OperationResult{
					CampaignID: spec.ID, DeviceID: "d1", OperationID: in,
					At: upBase.Add(5 * time.Minute), Success: false, Reason: "boom",
				}); err != nil {
					t.Fatalf("install failure: %v", err)
				}
			},
		},
	}
	claimTimes := map[string]time.Time{
		"before deadline": upBase.Add(5 * time.Minute),  // 12:05（等于结束时间）
		"exact deadline":  upBase.Add(10 * time.Minute), // 12:10
		"after deadline":  upBase.Add(20 * time.Minute), // 12:20
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for ctName, ct := range claimTimes {
				t.Run(ctName, func(t *testing.T) {
					s := setupUpgrade(t, "d1")
					spec := endedSpec([]string{"d1"}, 1, tc.rollback)
					createCampaign(t, s, spec)
					bringOnline(t, s, "d1")
					tc.setup(t, s, spec)
					endAt := upBase.Add(5 * time.Minute)
					cv0, _ := s.GetCampaign(spec.ID)
					if !cv0.Ended || !cv0.EndedAt.Equal(endAt) {
						t.Fatalf("campaign should be ended at 12:05: %+v", cv0)
					}
					nResults := len(cv0.Results)
					shadowBefore, _ := s.Get("d1")

					op, err := s.Claim(spec.ID, "d1", ct)
					if !errors.Is(err, ErrCampaignEnded) || op != nil {
						t.Fatalf("claim at %s: op=%+v err=%v", ctName, op, err)
					}
					// 12:06 的推进作为时间基线探针：若拒绝把基线推到了
					// 12:10/12:20，这里会被判倒退。
					if err := s.AdvanceCampaign(spec.ID, upBase.Add(6*time.Minute)); err != nil {
						t.Fatalf("rejected claim advanced baseline: %v", err)
					}
					cv, _ := s.GetCampaign(spec.ID)
					if !cv.Ended || !cv.EndedAt.Equal(endAt) ||
						findDevice(cv, "d1").Status != tc.wantState ||
						len(cv.Results) != nResults {
						t.Fatalf("state changed by rejected claim: %+v", cv)
					}
					sh, _ := s.Get("d1")
					if sh.Version != shadowBefore.Version || sh.LastSeq != shadowBefore.LastSeq ||
						sh.Revision != shadowBefore.Revision ||
						!rawEqual(sh.Reported, shadowBefore.Reported) {
						t.Fatalf("shadow changed: before %+v after %+v", shadowBefore, sh)
					}
				})
			}
		})
	}
}

// 已领取但未完成的下载、安装或回滚，活动结束后再次领取也不能返回操作。
func TestClaimAfterEndNeverReturnsUnfinishedOp(t *testing.T) {
	cases := []struct {
		name      string
		rollback  bool
		claimOnce func(t *testing.T, s *Store, spec CampaignSpec)
		wantState string
		wantPhase string
	}{
		{
			name: "downloading",
			claimOnce: func(t *testing.T, s *Store, spec CampaignSpec) {
				op, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
				if err != nil || op == nil || op.Kind != StageDownload {
					t.Fatalf("claim download: %v %+v", err, op)
				}
			},
			wantState: DeviceTimeout, wantPhase: StageDownload,
		},
		{
			name: "installing",
			claimOnce: func(t *testing.T, s *Store, spec CampaignSpec) {
				claimInstall(t, s, spec, "d1", upBase.Add(2*time.Minute))
			},
			wantState: DeviceTimeout, wantPhase: StageInstall,
		},
		{
			name:      "rolling back",
			rollback:  true,
			wantState: DeviceRollbackTimeout, wantPhase: StageRollback,
			claimOnce: func(t *testing.T, s *Store, spec CampaignSpec) {
				awaitRollback(t, s, spec, "d1", upBase.Add(1*time.Minute))
				op, err := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
				if err != nil || op == nil || op.Kind != StageRollback {
					t.Fatalf("claim rollback: %v %+v", err, op)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := setupUpgrade(t, "d1")
			spec := endedSpec([]string{"d1"}, 1, tc.rollback)
			createCampaign(t, s, spec)
			bringOnline(t, s, "d1")
			tc.claimOnce(t, s, spec)

			end := upBase.Add(10 * time.Minute)
			if err := s.AdvanceCampaign(spec.ID, end); err != nil {
				t.Fatal(err)
			}
			shadowBefore, _ := s.Get("d1")

			op, err := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute))
			if !errors.Is(err, ErrCampaignEnded) || op != nil {
				t.Fatalf("unfinished op returned after end: op=%+v err=%v", op, err)
			}
			cv, _ := s.GetCampaign(spec.ID)
			d := findDevice(cv, "d1")
			if d.Status != tc.wantState || d.Phase != tc.wantPhase || !d.At.Equal(end) ||
				!cv.EndedAt.Equal(end) {
				t.Fatalf("state changed: %+v / %+v", d, cv)
			}
			sh, _ := s.Get("d1")
			if sh.Version != shadowBefore.Version || sh.LastSeq != shadowBefore.LastSeq {
				t.Fatalf("shadow changed: before %+v after %+v", shadowBefore, sh)
			}
			// 拒绝不推进基线：12:15 合法。
			if err := s.AdvanceCampaign(spec.ID, upBase.Add(15*time.Minute)); err != nil {
				t.Fatalf("baseline advanced by rejected claim: %v", err)
			}
		})
	}
}

// 保留原有功能：活动仍在执行时，领取时间首次到达截止时间仍触发截止处理——
// 不派发操作，全部未结束设备超时结束，以本次领取时间结束活动并返回
// ErrCampaignEnded；即使请求领取的设备早已成功，也必须级联处理其他未结束设备。
// 本次结束时间仍参与倒退判断。
func TestClaimAtDeadlineStillTriggersCascade(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2")
	spec := endedSpec([]string{"d1", "d2"}, 2, false) // 同批两台
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	finishDevice(t, s, spec, "d1", upBase.Add(2*time.Minute)) // d1 12:05 已成功；d2 未开始

	deadline := upBase.Add(10 * time.Minute) // 12:10
	op, err := s.Claim(spec.ID, "d1", deadline)
	if !errors.Is(err, ErrCampaignEnded) || op != nil {
		t.Fatalf("claim at deadline: op=%+v err=%v", op, err)
	}
	cv, _ := s.GetCampaign(spec.ID)
	if !cv.Ended || cv.Status != CampaignFailed || !cv.EndedAt.Equal(deadline) {
		t.Fatalf("deadline not applied by claim: %+v", cv)
	}
	if d := findDevice(cv, "d1"); d.Status != DeviceSucceeded {
		t.Fatalf("terminal device changed: %+v", d)
	}
	if d := findDevice(cv, "d2"); d.Status != DeviceTimeout ||
		d.Phase != StageDownload || !d.At.Equal(deadline) {
		t.Fatalf("other unfinished device not timed out: %+v", d)
	}
	// 触发截止的领取确实推进了基线：更早的时间被判倒退。
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(5*time.Minute)); !errors.Is(err, ErrTimeRegression) {
		t.Fatalf("deadline claim should advance baseline, got %v", err)
	}
	// 此后 12:20 的领取是纯拒绝，不把基线推过结束时间。
	if op, err := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute)); !errors.Is(err, ErrCampaignEnded) || op != nil {
		t.Fatalf("claim after deadline: op=%+v err=%v", op, err)
	}
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(15*time.Minute)); err != nil {
		t.Fatalf("rejected claim must not advance baseline: %v", err)
	}
}

// 活动结束后，领取的时间缺失与时间倒退规则保持不变，且拒绝不改任何记录。
func TestClaimValidationUnchangedAfterEnd(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := endedSpec([]string{"d1"}, 1, false)
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	claimInstall(t, s, spec, "d1", upBase.Add(2*time.Minute))
	end := upBase.Add(10 * time.Minute)
	if err := s.AdvanceCampaign(spec.ID, end); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Claim(spec.ID, "d1", time.Time{}); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("missing time after end: %v", err)
	}
	// 早于活动真正已接受时间（12:10）仍是倒退，而不是活动已结束。
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute)); !errors.Is(err, ErrTimeRegression) {
		t.Fatalf("regression after end: %v", err)
	}
	cv, _ := s.GetCampaign(spec.ID)
	if !cv.Ended || !cv.EndedAt.Equal(end) ||
		findDevice(cv, "d1").Status != DeviceTimeout {
		t.Fatalf("validation rejection changed campaign: %+v", cv)
	}
	// 未知活动、不属于活动的设备保留现有错误规则。
	if _, err := s.Claim("nope", "d1", upBase.Add(15*time.Minute)); !errors.Is(err, ErrCampaignNotFound) {
		t.Fatalf("unknown campaign: %v", err)
	}
	mustRegister(t, s, "d2", "v1")
	if _, err := s.Claim(spec.ID, "d2", upBase.Add(15*time.Minute)); !errors.Is(err, ErrDeviceNotInCampaign) {
		t.Fatalf("device not in campaign: %v", err)
	}
}
