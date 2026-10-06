package shadow

import (
	"encoding/json"
	"testing"
	"time"
)

// TestCampaignWaitsForUnfinishedSameBatchDevice 锁定活动结论规则：只要活动中
// 还有设备未完成自己的步骤，活动就继续执行，不能因为某台设备已经成功或失败
// 而提前结束。这里构造同批两台设备 d1/d2 与后续批次 d3：d1 安装失败转入等待
// 回滚（开启回滚）、d3 因此被记为未执行，但同批 d2 仍在安装；此后 d1 进入
// 回滚中、d2 安装成功，活动都必须等待仍未完成的设备，直到 d1 回滚结束才以
// 失败结束（回滚成功只恢复原版本）。
func TestCampaignWaitsForUnfinishedSameBatchDevice(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2", "d3")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2", "d3"}
	spec.BatchSize = 2 // d1、d2 同属批次 0，d3 属批次 1
	spec.RollbackOnFailure = true
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")

	dl1, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	dl2, _ := s.Claim(spec.ID, "d2", upBase.Add(3*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl1.ID,
		At: upBase.Add(4 * time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d2", OperationID: dl2.ID,
		At: upBase.Add(5 * time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	in1, _ := s.Claim(spec.ID, "d1", upBase.Add(6*time.Minute))
	in2, err := s.Claim(spec.ID, "d2", upBase.Add(7*time.Minute))
	if err != nil {
		t.Fatal(err)
	}

	// d1 安装失败：d1 等待回滚（未完成），后续批次 d3 记为未执行，
	// 但同批 d2 仍在安装，活动必须继续且没有结束时间。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in1.ID,
		At: upBase.Add(8 * time.Minute), Success: false, Reason: "install boom",
	}); err != nil {
		t.Fatal(err)
	}
	assertMixedProgress := func(stage string) {
		t.Helper()
		cv := mustGetCampaign(s, spec.ID)
		if cv.Ended || cv.Status != CampaignRunning || !cv.EndedAt.IsZero() {
			t.Fatalf("%s: campaign must wait for unfinished devices, got ended=%v status=%s endedAt=%v",
				stage, cv.Ended, cv.Status, cv.EndedAt)
		}
		d := func(id string) DeviceStatusView { return findDevice(cv, id) }
		if d("d1").Status != DeviceAwaitingRollback && d("d1").Status != DeviceRollingBack {
			t.Fatalf("%s: d1 must be unfinished in rollback flow, got %s", stage, d("d1").Status)
		}
		if d("d2").Status != DeviceInstalling {
			t.Fatalf("%s: d2 must stay installing, got %s", stage, d("d2").Status)
		}
		if d("d3").Status != DeviceSkipped {
			t.Fatalf("%s: d3 must be skipped, got %s", stage, d("d3").Status)
		}
	}
	assertMixedProgress("d1 awaiting rollback, d2 installing")

	// d1 领取回滚进入回滚中，d2 仍在安装：活动继续等待。
	rb1, err := s.Claim(spec.ID, "d1", upBase.Add(9*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	assertMixedProgress("d1 rolling back, d2 installing")

	// 重开存储：仍有未完成设备却保存为 running，必须依据同一套规则正常打开，
	// 已保存状态原样保留，不补写结束时间、不推进任何设备。
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatalf("running campaign with unfinished devices must reopen: %v", err)
	}
	assertMixedProgress("after reopen")

	// 设备离线只是暂时无法领取新操作，不等于步骤结束：d2 离线不影响结论，
	// 其已领取的安装结果仍可在窗口外、截止前提交。
	if err := s.SetOffline("d2"); err != nil {
		t.Fatal(err)
	}
	assertMixedProgress("d2 offline mid-install")

	// d2 安装成功（终态），但 d1 仍在回滚中：活动继续等待 d1。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d2", OperationID: in2.ID,
		At: upBase.Add(11 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true}`),
	}); err != nil {
		t.Fatal(err)
	}
	cv := mustGetCampaign(s, spec.ID)
	if cv.Ended || cv.Status != CampaignRunning || !cv.EndedAt.IsZero() {
		t.Fatalf("campaign must wait for d1 rollback: %+v", cv)
	}
	if findDevice(cv, "d1").Status != DeviceRollingBack ||
		findDevice(cv, "d2").Status != DeviceSucceeded {
		t.Fatalf("unexpected device states: %+v", cv.Devices)
	}

	// d1 回滚成功（恢复 v1）：全部设备终态，但回滚成功不算升级成功，
	// 活动以失败结束，结束时间沿用最后一台设备结果的时间。
	endAt := upBase.Add(12 * time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb1.ID,
		At: endAt, Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	cv = mustGetCampaign(s, spec.ID)
	if !cv.Ended || cv.Status != CampaignFailed || !cv.EndedAt.Equal(endAt) {
		t.Fatalf("campaign must end failed at last result time: %+v", cv)
	}
	got := map[string]string{}
	for _, d := range cv.Devices {
		got[d.DeviceID] = d.Status
	}
	if got["d1"] != DeviceRollbackSucceeded || got["d2"] != DeviceSucceeded ||
		got["d3"] != DeviceSkipped {
		t.Fatalf("final device states: %+v", got)
	}

	// 终态活动重开：已保存的结束时间与设备状态时间保持原值，结论核对共用规则。
	dir = s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("ended campaign must reopen: %v", err)
	}
	defer s2.Close()
	cv2 := mustGetCampaign(s2, spec.ID)
	if !cv2.Ended || cv2.Status != CampaignFailed || !cv2.EndedAt.Equal(endAt) {
		t.Fatalf("ended campaign conclusion/time must be preserved: %+v", cv2)
	}
	for _, before := range cv.Devices {
		after := findDevice(cv2, before.DeviceID)
		if after.Status != before.Status || !after.At.Equal(before.At) {
			t.Fatalf("device %s state/time changed on reopen: before %+v after %+v",
				before.DeviceID, before, after)
		}
	}
}
