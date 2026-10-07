package shadow

import (
	"encoding/json"
	"testing"
	"time"
)

// TestRestoreResultAtDeadlineRefused 保护打开存储时的结果截止时间核对：下载、
// 安装、回滚中每个已经接受结果的操作，其保存的首次结果时间都必须严格早于所属
// 活动的截止时间；恰好等于截止也属于迟到，成功与失败适用同一规则。命中任意
// 一条都必须以 ErrCorruptStorage 拒绝打开整个存储且原文件内容不变，不能删除
// 迟到结果、调整时间或把设备改成超时来继续打开。
func TestRestoreResultAtDeadlineRefused(t *testing.T) {
	// 题设示例：窗口 12:00–13:00，截止 14:00，设备 12:10 领取下载，保存的
	// 下载成功结果时间为 14:00，对应历史也记为同一时刻；操作标识、历史内容、
	// 设备进度与活动最新时间（同步改写为 14:00）都能互相对应，仍须拒绝。
	t.Run("download success at exactly deadline", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(15 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		if cv := mustGetCampaign(s, spec.ID); cv.Ended || findDevice(cv, "d1").Status != DeviceReady {
			t.Fatalf("precondition: %+v", cv)
		}
		dir := closeStore(t, s)
		setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T14:00:00Z")
		original := setCampaignLastTime(t, dir, spec.ID, "2026-10-02T14:00:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 下载失败结果同样适用：失败原因自洽、活动已失败结束也不能掩盖。
	t.Run("download failure at deadline", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(15 * time.Minute), Success: false, Reason: "dl boom",
		}); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T14:00:00Z")
		original := setCampaignLastTime(t, dir, spec.ID, "2026-10-02T14:00:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 安装成功结果晚于截止：设备整体已成功、活动已结束也不能让早先的迟到
	// 结果合法。改写后的 14:30 晚于下载结果，保证结果历史次序自洽；更晚的
	// 普通上报覆盖附带上报，使该记录成为历史序号，不与设备影子的最近上报
	// 比对时间，从而隔离出本项核对。
	t.Run("install success after deadline in ended campaign", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(10 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		in, _ := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
			At: upBase.Add(30 * time.Minute), Success: true,
			Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true}`),
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.Report("d1", 3, upBase.Add(40*time.Minute), "v2", json.RawMessage(`{"x":1}`)); err != nil {
			t.Fatal(err)
		}
		if cv := mustGetCampaign(s, spec.ID); !cv.Ended || cv.Status != CampaignSucceeded {
			t.Fatalf("precondition: %+v", cv)
		}
		dir := closeStore(t, s)
		setOpResultAt(t, dir, spec.ID, "d1", "install", "2026-10-02T14:30:00Z")
		original := setCampaignLastTime(t, dir, spec.ID, "2026-10-02T14:30:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 回滚成功结果恰好等于截止：回滚完成、当前版本已回到锁定目标都不能让
	// 迟到的回滚结果合法。22:00+08:00 即 14:00Z，时区写法不同不改变判断。
	t.Run("rollback success at deadline across timezone notation", func(t *testing.T) {
		s, _, spec, rbOp := setupInstallFailure(t, true)
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rbOp.ID,
			At: upBase.Add(5 * time.Minute), Success: true,
			Seq: 2, Version: "v1", Config: json.RawMessage(`{"ok":true}`),
		}); err != nil {
			t.Fatal(err)
		}
		// 更晚的普通上报覆盖回滚附带上报，隔离出截止时间核对。
		if err := s.Report("d1", 3, upBase.Add(6*time.Minute), "v1", json.RawMessage(`{"x":1}`)); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		setOpResultAt(t, dir, spec.ID, "d1", "rollback", "2026-10-02T22:00:00+08:00")
		original := setCampaignLastTime(t, dir, spec.ID, "2026-10-02T14:00:00Z")
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreResultBeforeDeadlineAccepted 保护合法记录不被截止时间核对误拒：
// 窗口内领取、窗口结束后截止前完成的结果，已领取但无结果随后超时的操作，
// 以及等于或晚于截止的活动超时状态时间与结束时间，都必须正常打开。
func TestRestoreResultBeforeDeadlineAccepted(t *testing.T) {
	// 窗口内（12:50）领取，窗口结束后、截止前（13:59）提交结果合法。
	t.Run("result after window before deadline opens", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(50*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(119 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("result before deadline must open: %v", err)
		}
		defer s2.Close()
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DeviceReady {
			t.Fatalf("d1 progress lost")
		}
	})

	// 截止前同一时刻的不同时区写法不能被误判：21:59+08:00 即 13:59Z。
	t.Run("before deadline across timezone notation", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(50*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(119 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T21:59:00+08:00")
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("same instant in another timezone must open: %v", err)
		}
		defer s2.Close()
	})

	// 已领取但尚无结果、随后因截止而超时的操作没有结果时间：设备超时状态
	// 时间与活动结束时间恰好等于截止，不应被当作迟到结果。
	t.Run("claimed then timed out at deadline opens", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute)); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := s.AdvanceCampaign(spec.ID, spec.Deadline); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("timeout at deadline must open: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if findDevice(cv, "d1").Status != DeviceTimeout || !cv.EndedAt.Equal(spec.Deadline) {
			t.Fatalf("timeout state lost: %+v", cv)
		}
	})

	// 活动结束时间晚于截止（截止后才推进到截止）同样合法：结束时间不能
	// 代替操作结果时间参与判断。
	t.Run("ended after deadline opens", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute)); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := s.AdvanceCampaign(spec.ID, spec.Deadline.Add(30*time.Minute)); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("end time after deadline must open: %v", err)
		}
		defer s2.Close()
	})

	// 截止前已接受的结果，后来重复提交时不改写首次记录：重开按首次接受
	// 时间判断，不能按重复提交时间判为损坏。
	t.Run("duplicate submission does not rewrite first record", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(15 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		// 截止前的重复提交（13:50）成功返回但不改写 12:15 的首次记录。
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(110 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("first record before deadline must open: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if len(cv.Results) != 1 || !cv.Results[0].At.Equal(upBase.Add(15*time.Minute)) {
			t.Fatalf("first record rewritten: %+v", cv.Results)
		}
	})
}
