package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// endedSpec 构造一个窗口 12:00–12:09（[start,end)）、截止 12:10 的单/多设备活动。
func endedSpec(devices []string, batch int, rollback bool) CampaignSpec {
	spec := upSpec()
	spec.ID = "cmp-ended"
	spec.Devices = devices
	spec.BatchSize = batch
	spec.WindowStart = upBase
	spec.WindowEnd = upBase.Add(9 * time.Minute)
	spec.Deadline = upBase.Add(10 * time.Minute) // 12:10
	spec.RollbackOnFailure = rollback
	return spec
}

// 设备已领取安装，活动在 12:10 被 AdvanceCampaign 结束为超时；设备 12:20 的迟到
// 安装结果必须是不推进时间基线的纯拒绝（ErrCampaignEnded）。随后 12:15 的
// AdvanceCampaign 应按已结束活动规则成功返回，而不是 ErrTimeRegression；
// 活动结束时间、设备状态、结果历史与影子均保持 12:10 的样子。
func TestLateResultAfterTimeoutIsPureReject(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := endedSpec([]string{"d1"}, 1, false)
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	in := claimInstall(t, s, spec, "d1", upBase.Add(2*time.Minute)) // 12:04 已领取安装

	end := upBase.Add(10 * time.Minute) // 12:10
	if err := s.AdvanceCampaign(spec.ID, end); err != nil {
		t.Fatalf("advance to deadline: %v", err)
	}
	before, _ := s.GetCampaign(spec.ID)
	if !before.Ended || !before.EndedAt.Equal(end) {
		t.Fatalf("campaign not ended at deadline: %+v", before)
	}
	shadowBefore, _ := s.Get("d1")

	// 12:20 迟到的安装成功结果（附带上报完全合法也一样拒绝）。
	err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in,
		At: upBase.Add(20 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true}`),
	})
	if !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("late result after end: %v", err)
	}

	after, _ := s.GetCampaign(spec.ID)
	if !after.Ended || after.Status != CampaignFailed || !after.EndedAt.Equal(end) {
		t.Fatalf("campaign conclusion/end time changed: %+v", after)
	}
	d := findDevice(after, "d1")
	if d.Status != DeviceTimeout || d.Phase != StageInstall ||
		d.Reason == "" || !d.At.Equal(end) {
		t.Fatalf("device state changed by rejected result: %+v", d)
	}
	if len(after.Results) != len(before.Results) {
		t.Fatalf("rejected result added history: %d -> %d", len(before.Results), len(after.Results))
	}
	sh, _ := s.Get("d1")
	if sh.Version != shadowBefore.Version || sh.Online != shadowBefore.Online ||
		sh.Revision != shadowBefore.Revision || sh.LastSeq != shadowBefore.LastSeq ||
		!rawEqual(sh.Desired, shadowBefore.Desired) || !rawEqual(sh.Reported, shadowBefore.Reported) {
		t.Fatalf("shadow changed by rejected result: before %+v after %+v", shadowBefore, sh)
	}

	// 12:15（早于被拒绝的 12:20、晚于结束时间 12:10）推进已结束活动必须成功。
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(15*time.Minute)); err != nil {
		t.Fatalf("advance after rejected late result must not regress: %v", err)
	}
	final, _ := s.GetCampaign(spec.ID)
	if !final.Ended || !final.EndedAt.Equal(end) {
		t.Fatalf("advance altered ended campaign: %+v", final)
	}
}

// 活动结束后的迟到结果不校验内容：附带上报缺失、版本不符、配置非法、序号冲突，
// 以及失败结果缺少原因，都只返回 ErrCampaignEnded；不推进时间基线、不写影子。
func TestLateResultAfterTimeoutSkipsContentValidation(t *testing.T) {
	cases := []struct {
		name string
		res  OperationResult
	}{
		{"missing report", OperationResult{Success: true}},
		{"wrong version", OperationResult{Success: true, Seq: 2, Version: "v9", Config: json.RawMessage(`{}`)}},
		{"bad config", OperationResult{Success: true, Seq: 2, Version: "v2", Config: json.RawMessage(`[1]`)}},
		{"seq conflict", OperationResult{Success: true, Seq: 1, Version: "v2", Config: json.RawMessage(`{"x":1}`)}},
		{"failure without reason", OperationResult{Success: false}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := setupUpgrade(t, "d1")
			spec := endedSpec([]string{"d1"}, 1, false)
			createCampaign(t, s, spec)
			bringOnline(t, s, "d1")
			in := claimInstall(t, s, spec, "d1", upBase.Add(2*time.Minute))
			end := upBase.Add(10 * time.Minute)
			if err := s.AdvanceCampaign(spec.ID, end); err != nil {
				t.Fatal(err)
			}
			nResults := len(mustGetCampaign(s, spec.ID).Results)

			res := tc.res
			res.CampaignID = spec.ID
			res.DeviceID = "d1"
			res.OperationID = in
			res.At = upBase.Add(20 * time.Minute)
			if err := s.SubmitResult(res); !errors.Is(err, ErrCampaignEnded) {
				t.Fatalf("got %v, want ErrCampaignEnded", err)
			}
			cv, _ := s.GetCampaign(spec.ID)
			if !cv.Ended || !cv.EndedAt.Equal(end) ||
				findDevice(cv, "d1").Status != DeviceTimeout ||
				len(cv.Results) != nResults {
				t.Fatalf("rejected result changed campaign: %+v", cv)
			}
			// 拒绝不得推进时间基线：12:15 仍合法。
			if err := s.AdvanceCampaign(spec.ID, upBase.Add(15*time.Minute)); err != nil {
				t.Fatalf("rejection advanced time baseline: %v", err)
			}
		})
	}
}

// 已领取下载、尚未完成的设备在活动超时结束后迟到提交下载结果，同样是纯拒绝。
func TestLateDownloadResultAfterTimeoutIsPureReject(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := endedSpec([]string{"d1"}, 1, false)
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute)) // 下载中
	if err != nil || dl == nil || dl.Kind != StageDownload {
		t.Fatalf("claim download: %v %+v", err, dl)
	}
	end := upBase.Add(10 * time.Minute)
	if err := s.AdvanceCampaign(spec.ID, end); err != nil {
		t.Fatal(err)
	}

	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(20 * time.Minute), Success: true,
	}); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("late download after end: %v", err)
	}
	cv, _ := s.GetCampaign(spec.ID)
	d := findDevice(cv, "d1")
	if d.Status != DeviceTimeout || d.Phase != StageDownload || !d.At.Equal(end) ||
		!cv.EndedAt.Equal(end) {
		t.Fatalf("state changed by rejected download: %+v / %+v", d, cv)
	}
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(15*time.Minute)); err != nil {
		t.Fatalf("rejection advanced time baseline: %v", err)
	}
}

// 已超时活动中正在回滚（回滚已领取）的设备迟到提交回滚结果：纯拒绝、影子不变；
// 仍在等待回滚（回滚未领取）的设备提交回滚标识按未领取拒绝。两者都不推进时间。
func TestLateRollbackResultAfterTimeoutIsPureReject(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2")
	spec := endedSpec([]string{"d1", "d2"}, 2, true)
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	// d1 走到等待回滚后再领取回滚（正在回滚，12:05）；d2 停在等待回滚（12:08）。
	awaitRollback(t, s, spec, "d1", upBase.Add(1*time.Minute))
	rbClaimed, err := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
	if err != nil || rbClaimed == nil || rbClaimed.Kind != StageRollback {
		t.Fatalf("claim rollback d1: %v %+v", err, rbClaimed)
	}
	awaitRollback(t, s, spec, "d2", upBase.Add(5*time.Minute))

	end := upBase.Add(10 * time.Minute)
	if err := s.AdvanceCampaign(spec.ID, end); err != nil {
		t.Fatal(err)
	}
	cv0, _ := s.GetCampaign(spec.ID)
	nResults := len(cv0.Results)
	d2RollbackID := findDevice(cv0, "d2").RollbackID

	// d1 已领取回滚的迟到成功结果（附带上报合法）：纯拒绝，影子不被改回锁定版本。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rbClaimed.ID,
		At: upBase.Add(20 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	}); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("late rollback after end: %v", err)
	}
	// d2 尚未领取回滚：按既有未领取规则拒绝，而不是活动已结束。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d2", OperationID: d2RollbackID,
		At: upBase.Add(20 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	}); !errors.Is(err, ErrOperationNotClaimed) {
		t.Fatalf("unclaimed rollback after end: %v", err)
	}

	cv, _ := s.GetCampaign(spec.ID)
	d1 := findDevice(cv, "d1")
	d2 := findDevice(cv, "d2")
	if d1.Status != DeviceRollbackTimeout || !d1.At.Equal(end) ||
		d2.Status != DeviceRollbackTimeout || !d2.At.Equal(end) ||
		!cv.EndedAt.Equal(end) || len(cv.Results) != nResults {
		t.Fatalf("state changed by rejected rollbacks: d1 %+v d2 %+v cv %+v", d1, d2, cv)
	}
	if sh, _ := s.Get("d1"); sh.Version != "v1" || sh.LastSeq != 1 {
		t.Fatalf("shadow changed by rejected rollback: %+v", sh)
	}
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(15*time.Minute)); err != nil {
		t.Fatalf("rejection advanced time baseline: %v", err)
	}
}

// 保留首次触发截止处理的行为：活动仍在执行时，已领取操作第一次在截止时刻提交，
// 仍把未结束设备记为超时、以本次时间结束活动；这次推进的时间仍参与倒退判断。
// 此后第二次迟到提交才是不推进基线的纯拒绝。
func TestFirstDeadlineSubmissionStillAdvancesTime(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := endedSpec([]string{"d1"}, 1, false)
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	in := claimInstall(t, s, spec, "d1", upBase.Add(2*time.Minute))
	deadline := upBase.Add(10 * time.Minute)

	// 第一次在截止时刻提交：触发截止处理并结束活动。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in,
		At: deadline, Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true}`),
	}); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("first deadline submission: %v", err)
	}
	cv, _ := s.GetCampaign(spec.ID)
	if !cv.Ended || !cv.EndedAt.Equal(deadline) ||
		findDevice(cv, "d1").Status != DeviceTimeout {
		t.Fatalf("deadline processing not applied: %+v", cv)
	}
	// 首次触发确实推进了时间基线：更早的推进被判倒退。
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(5*time.Minute)); !errors.Is(err, ErrTimeRegression) {
		t.Fatalf("triggering submission should advance baseline, got %v", err)
	}
	// 第二次（12:20）迟到提交是纯拒绝，不把基线推过结束时间。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in,
		At: upBase.Add(20 * time.Minute), Success: false, Reason: "late",
	}); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("second late submission: %v", err)
	}
	if err := s.AdvanceCampaign(spec.ID, upBase.Add(15*time.Minute)); err != nil {
		t.Fatalf("second late submission must not advance baseline: %v", err)
	}
}
