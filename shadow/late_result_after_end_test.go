package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// 活动已经超时结束后，已领取但未接受结果的迟到提交只返回 ErrCampaignEnded，
// 不再推进活动时间基线：之后以原结束时刻与迟到提交之间的时间调用
// AdvanceCampaign 仍按已结束活动规则成功，不能误报 ErrTimeRegression；
// 活动结束时间、设备超时时间、结果历史与影子均保持不变。
func TestLateResultAfterEndedDoesNotAdvanceTime(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := CampaignSpec{
		ID:            "cmp-late-ended",
		Operator:      "alice",
		CreatedAt:     upBase,
		TargetVersion: "v2",
		Devices:       []string{"d1"},
		BatchSize:     1,
		WindowStart:   upBase,
		WindowEnd:     upBase.Add(5 * time.Minute),
		Deadline:      upBase.Add(10 * time.Minute), // 12:10
	}
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	in := claimInstall(t, s, spec, "d1", upBase.Add(time.Minute))

	end := spec.Deadline       // 12:10 调用方显式结束
	late := end.Add(10 * time.Minute) // 12:20 设备迟到提交
	mid := end.Add(5 * time.Minute)   // 12:15 随后的合法推进时间

	if err := s.AdvanceCampaign(spec.ID, end); err != nil {
		t.Fatalf("advance to deadline: %v", err)
	}
	before, _ := s.GetCampaign(spec.ID)
	shadowBefore, _ := s.Get("d1")
	if !before.Ended || before.Status != CampaignFailed || !before.EndedAt.Equal(end) {
		t.Fatalf("campaign should be ended at deadline: %+v", before)
	}
	d0 := findDevice(before, "d1")
	if d0.Status != DeviceTimeout || d0.Phase != StageInstall || !d0.At.Equal(end) {
		t.Fatalf("device should be timed out at deadline: %+v", d0)
	}
	nResults := len(before.Results)

	if err := s.SubmitResult(OperationResult{
		CampaignID:  spec.ID,
		DeviceID:    "d1",
		OperationID: in,
		At:          late,
		Success:     true,
		Seq:         2,
		Version:     "v2",
		Config:      json.RawMessage(`{"ok":true}`),
	}); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("late install result: got %v, want ErrCampaignEnded", err)
	}

	after, _ := s.GetCampaign(spec.ID)
	if !after.Ended || after.Status != CampaignFailed {
		t.Fatalf("campaign status changed: %+v", after)
	}
	if !after.EndedAt.Equal(end) {
		t.Fatalf("ended at advanced to %v, want %v", after.EndedAt, end)
	}
	d := findDevice(after, "d1")
	if d.Status != DeviceTimeout || d.Phase != StageInstall ||
		d.Reason == "" || !d.At.Equal(end) {
		t.Fatalf("device timeout record changed: %+v", d)
	}
	if len(after.Results) != nResults {
		t.Fatalf("late result changed history: %d -> %d", nResults, len(after.Results))
	}
	shadowAfter, _ := s.Get("d1")
	if shadowAfter.Version != shadowBefore.Version ||
		shadowAfter.LastSeq != shadowBefore.LastSeq ||
		shadowAfter.Revision != shadowBefore.Revision ||
		string(shadowAfter.Reported) != string(shadowBefore.Reported) ||
		string(shadowAfter.Desired) != string(shadowBefore.Desired) {
		t.Fatalf("shadow changed by rejected late result: before %+v after %+v",
			shadowBefore, shadowAfter)
	}

	// 12:15 介于原结束时间与被拒绝的迟到提交时间之间：已结束活动应为空操作成功，
	// 修正前这里会因 LastTime 被推到 12:20 而报 ErrTimeRegression。
	if err := s.AdvanceCampaign(spec.ID, mid); err != nil {
		t.Fatalf("advance at %v after rejected submit: %v", mid, err)
	}
	final, _ := s.GetCampaign(spec.ID)
	if !final.Ended || !final.EndedAt.Equal(end) {
		t.Fatalf("campaign changed on no-op advance: %+v", final)
	}
}

// 活动已超时结束后，迟到结果的内容不再校验：附带上报缺失、版本不符、配置非法、
// 失败结果缺少原因，都只返回 ErrCampaignEnded，且不推进时间、不写影子。
func TestLateResultAfterEndedSkipsContentAndDoesNotAdvance(t *testing.T) {
	cases := []struct {
		name string
		res  OperationResult
	}{
		{"missing report", OperationResult{Success: true}},
		{"wrong version", OperationResult{Success: true, Seq: 2, Version: "v9", Config: json.RawMessage(`{}`)}},
		{"bad config", OperationResult{Success: true, Seq: 2, Version: "v2", Config: json.RawMessage(`[1]`)}},
		{"failure without reason", OperationResult{Success: false}},
	}
	end := upBase.Add(10 * time.Minute)
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := setupUpgrade(t, "d1")
			spec := CampaignSpec{
				ID:            "cmp-late-content",
				Operator:      "alice",
				CreatedAt:     upBase,
				TargetVersion: "v2",
				Devices:       []string{"d1"},
				BatchSize:     1,
				WindowStart:   upBase,
				WindowEnd:     upBase.Add(5 * time.Minute),
				Deadline:      end,
			}
			createCampaign(t, s, spec)
			bringOnline(t, s, "d1")
			in := claimInstall(t, s, spec, "d1", upBase.Add(time.Minute))
			if err := s.AdvanceCampaign(spec.ID, end); err != nil {
				t.Fatalf("advance to deadline: %v", err)
			}
			shadowBefore, _ := s.Get("d1")

			res := tc.res
			res.CampaignID = spec.ID
			res.DeviceID = "d1"
			res.OperationID = in
			res.At = end.Add(time.Duration(10+i) * time.Minute)
			if err := s.SubmitResult(res); !errors.Is(err, ErrCampaignEnded) {
				t.Fatalf("late result: got %v, want ErrCampaignEnded", err)
			}

			cv, _ := s.GetCampaign(spec.ID)
			if !cv.Ended || !cv.EndedAt.Equal(end) {
				t.Fatalf("ended time changed: %+v", cv)
			}
			if d := findDevice(cv, "d1"); d.Status != DeviceTimeout || !d.At.Equal(end) {
				t.Fatalf("device record changed: %+v", d)
			}
			shadowAfter, _ := s.Get("d1")
			if shadowAfter.Version != shadowBefore.Version ||
				shadowAfter.LastSeq != shadowBefore.LastSeq ||
				string(shadowAfter.Reported) != string(shadowBefore.Reported) {
				t.Fatalf("shadow changed: before %+v after %+v", shadowBefore, shadowAfter)
			}
			// 时间基线未被推进：结束时刻之后、迟到提交之前的推进仍成功。
			if err := s.AdvanceCampaign(spec.ID, end.Add(5*time.Minute)); err != nil {
				t.Fatalf("advance after rejected submit: %v", err)
			}
		})
	}
}

// 已领取下载但未提交结果的设备在活动超时结束后迟到提交：同样只拒绝、
// 不推进时间，设备保持下载阶段超时。
func TestLateDownloadResultAfterEndedDoesNotAdvanceTime(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := CampaignSpec{
		ID:            "cmp-late-dl",
		Operator:      "alice",
		CreatedAt:     upBase,
		TargetVersion: "v2",
		Devices:       []string{"d1"},
		BatchSize:     1,
		WindowStart:   upBase,
		WindowEnd:     upBase.Add(5 * time.Minute),
		Deadline:      upBase.Add(10 * time.Minute),
	}
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec.ID, "d1", upBase.Add(time.Minute))
	if err != nil || dl == nil || dl.Kind != StageDownload {
		t.Fatalf("claim download: %v %+v", err, dl)
	}
	end := spec.Deadline
	if err := s.AdvanceCampaign(spec.ID, end); err != nil {
		t.Fatalf("advance to deadline: %v", err)
	}

	if err := s.SubmitResult(OperationResult{
		CampaignID:  spec.ID,
		DeviceID:    "d1",
		OperationID: dl.ID,
		At:          end.Add(10 * time.Minute),
		Success:     true,
	}); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("late download result: got %v, want ErrCampaignEnded", err)
	}

	cv, _ := s.GetCampaign(spec.ID)
	if !cv.Ended || !cv.EndedAt.Equal(end) {
		t.Fatalf("ended time changed: %+v", cv)
	}
	if d := findDevice(cv, "d1"); d.Status != DeviceTimeout || d.Phase != StageDownload ||
		!d.At.Equal(end) {
		t.Fatalf("device record changed: %+v", d)
	}
	if err := s.AdvanceCampaign(spec.ID, end.Add(5*time.Minute)); err != nil {
		t.Fatalf("advance after rejected submit: %v", err)
	}
}

// 开启回滚的活动超时结束后（回滚已领取未完成，设备记回滚阶段超时），
// 迟到的回滚结果只返回 ErrCampaignEnded，不推进时间、不把影子改回锁定版本。
func TestLateRollbackResultAfterEndedDoesNotAdvanceTime(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := CampaignSpec{
		ID:                "cmp-late-rb",
		Operator:          "alice",
		CreatedAt:         upBase,
		TargetVersion:     "v2",
		Devices:           []string{"d1"},
		BatchSize:         1,
		WindowStart:       upBase,
		WindowEnd:         upBase.Add(5 * time.Minute),
		Deadline:          upBase.Add(10 * time.Minute),
		RollbackOnFailure: true,
	}
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, _ := s.Claim(spec.ID, "d1", upBase.Add(time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(2 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("download result: %v", err)
	}
	in, _ := s.Claim(spec.ID, "d1", upBase.Add(3*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: upBase.Add(4 * time.Minute), Success: false, Reason: "install boom",
	}); err != nil {
		t.Fatalf("install failure: %v", err)
	}
	rb, err := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute-time.Second))
	if err != nil || rb == nil || rb.Kind != StageRollback || rb.TargetVersion != "v1" {
		t.Fatalf("claim rollback: %v %+v", err, rb)
	}
	end := spec.Deadline
	if err := s.AdvanceCampaign(spec.ID, end); err != nil {
		t.Fatalf("advance to deadline: %v", err)
	}
	before, _ := s.GetCampaign(spec.ID)
	if d := findDevice(before, "d1"); d.Status != DeviceRollbackTimeout {
		t.Fatalf("device should be rollback-timeout: %+v", d)
	}

	// 迟到回滚成功缺附带上报，仍只按已结束拒绝。
	if err := s.SubmitResult(OperationResult{
		CampaignID:  spec.ID,
		DeviceID:    "d1",
		OperationID: rb.ID,
		At:          end.Add(10 * time.Minute),
		Success:     true,
	}); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("late rollback result: got %v, want ErrCampaignEnded", err)
	}

	cv, _ := s.GetCampaign(spec.ID)
	if !cv.Ended || !cv.EndedAt.Equal(end) {
		t.Fatalf("ended time changed: %+v", cv)
	}
	if d := findDevice(cv, "d1"); d.Status != DeviceRollbackTimeout ||
		d.Phase != StageRollback || !d.At.Equal(end) {
		t.Fatalf("device record changed: %+v", d)
	}
	if len(cv.Results) != len(before.Results) {
		t.Fatalf("history changed: %d -> %d", len(before.Results), len(cv.Results))
	}
	if err := s.AdvanceCampaign(spec.ID, end.Add(5*time.Minute)); err != nil {
		t.Fatalf("advance after rejected submit: %v", err)
	}
}

// 保留首次触发截止处理的行为：活动仍在执行时，已领取操作第一次在截止时刻或
// 之后提交结果，仍以本次提交时间结束活动并推进时间基线，该时间参与后续倒退
// 判断（随后更早的推进报 ErrTimeRegression）。
func TestFirstLateResultOnRunningCampaignStillAdvancesTime(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := CampaignSpec{
		ID:            "cmp-late-first",
		Operator:      "alice",
		CreatedAt:     upBase,
		TargetVersion: "v2",
		Devices:       []string{"d1"},
		BatchSize:     1,
		WindowStart:   upBase,
		WindowEnd:     upBase.Add(5 * time.Minute),
		Deadline:      upBase.Add(10 * time.Minute),
	}
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	in := claimInstall(t, s, spec, "d1", upBase.Add(time.Minute))

	trigger := spec.Deadline.Add(10 * time.Minute) // 12:20 首次迟到提交触发截止处理
	if err := s.SubmitResult(OperationResult{
		CampaignID:  spec.ID,
		DeviceID:    "d1",
		OperationID: in,
		At:          trigger,
		Success:     true,
		Seq:         2,
		Version:     "v2",
		Config:      json.RawMessage(`{"ok":true}`),
	}); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("triggering late result: got %v, want ErrCampaignEnded", err)
	}
	cv, _ := s.GetCampaign(spec.ID)
	if !cv.Ended || !cv.EndedAt.Equal(trigger) {
		t.Fatalf("campaign should end at submit time: %+v", cv)
	}
	if d := findDevice(cv, "d1"); d.Status != DeviceTimeout || !d.At.Equal(trigger) {
		t.Fatalf("device should time out at submit time: %+v", d)
	}
	// 触发截止处理确实推进了时间基线：早于该提交时间的推进仍判倒退。
	if err := s.AdvanceCampaign(spec.ID, spec.Deadline.Add(5*time.Minute));
		!errors.Is(err, ErrTimeRegression) {
		t.Fatalf("earlier advance: got %v, want ErrTimeRegression", err)
	}
}
