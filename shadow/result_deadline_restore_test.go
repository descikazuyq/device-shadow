package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// bumpCampaignLastTime 直接改写活动保存的时间基线（lastTime），使被改写为
// 截止时刻或更晚的结果时间不先触发时间基线核对，从而隔离出“迟到结果”核对。
func bumpCampaignLastTime(t *testing.T, dir, campaignID, at string) []byte {
	t.Helper()
	return rewriteStoreBytes(t, dir, func(doc map[string]any) {
		campaignDoc(doc, campaignID)["lastTime"] = at
	})
}

// TestRestoreResultAtDeadlineRefused 保护打开存储时的结果截止核对：每个已经
// 接受结果的下载、安装、回滚操作，其保存的结果时间都必须严格早于所属活动的
// 截止时间；成功与失败适用同一规则，恰好等于截止也算迟到。命中任意一条都
// 必须以 ErrCorruptStorage 拒绝打开整个存储且原文件内容不变，即使操作标识、
// 结果历史、设备进度与活动最新时间互相对应，设备后来安装成功、回滚完成或
// 当前版本已符合目标也不能让早先的迟到结果合法。
func TestRestoreResultAtDeadlineRefused(t *testing.T) {
	// 题设示例：窗口 12:00–13:00，截止 14:00，设备 12:10 领取下载，保存的
	// 下载成功结果与对应历史都记为 14:00；设备停在等待安装、活动仍在执行、
	// 时间基线已覆盖该时刻，仍必须拒绝打开。
	t.Run("download success exactly at deadline while ready", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(110 * time.Minute), Success: true, // 13:50，截止前
		}); err != nil {
			t.Fatal(err)
		}
		if cv := mustGetCampaign(s, spec.ID); cv.Ended || findDevice(cv, "d1").Status != DeviceReady {
			t.Fatalf("precondition: %+v", cv)
		}
		dir := closeStore(t, s)
		original := setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T14:00:00Z")
		original = bumpCampaignLastTime(t, dir, spec.ID, "2026-10-02T14:00:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 结果晚于截止同样拒绝。
	t.Run("download success after deadline", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(110 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		original := setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T14:30:00Z")
		original = bumpCampaignLastTime(t, dir, spec.ID, "2026-10-02T14:30:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 下载失败结果恰好等于截止：失败与成功同一规则，活动已失败结束也不能掩盖。
	t.Run("download failure exactly at deadline", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(110 * time.Minute), Success: false, Reason: "dl boom",
		}); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		original := setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T14:00:00Z")
		original = bumpCampaignLastTime(t, dir, spec.ID, "2026-10-02T14:00:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 安装失败结果（未开启回滚）等于截止：设备已 failed、活动已结束仍拒绝。
	t.Run("install failure exactly at deadline", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(20 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		in, _ := s.Claim(spec.ID, "d1", upBase.Add(30*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
			At: upBase.Add(110 * time.Minute), Success: false, Reason: "install boom",
		}); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		original := setOpResultAt(t, dir, spec.ID, "d1", "install", "2026-10-02T14:00:00Z")
		original = bumpCampaignLastTime(t, dir, spec.ID, "2026-10-02T14:00:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 安装成功结果等于截止：设备后来安装成功、当前版本已是目标版本也不能让
	// 迟到结果合法。更晚的普通上报覆盖附带上报，隔离出本项核对。
	t.Run("install success exactly at deadline in succeeded campaign", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(20 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		in, _ := s.Claim(spec.ID, "d1", upBase.Add(30*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
			At: upBase.Add(110 * time.Minute), Success: true,
			Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true}`),
		}); err != nil {
			t.Fatal(err)
		}
		// 更晚的普通上报覆盖附带上报，使安装成功记录不再与当前影子比对时间。
		if err := s.Report("d1", 3, upBase.Add(115*time.Minute), "v2", json.RawMessage(`{"x":1}`)); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		original := setOpResultAt(t, dir, spec.ID, "d1", "install", "2026-10-02T14:00:00Z")
		original = bumpCampaignLastTime(t, dir, spec.ID, "2026-10-02T14:00:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 回滚失败结果等于截止：回滚已完成、活动已结束仍拒绝。
	t.Run("rollback failure exactly at deadline", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
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
			At: upBase.Add(30 * time.Minute), Success: false, Reason: "install boom",
		}); err != nil {
			t.Fatal(err)
		}
		rb, err := s.Claim(spec.ID, "d1", upBase.Add(40*time.Minute))
		if err != nil || rb == nil || rb.Kind != StageRollback {
			t.Fatalf("claim rollback: %v %+v", err, rb)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(110 * time.Minute), Success: false, Reason: "rb boom",
		}); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		original := setOpResultAt(t, dir, spec.ID, "d1", "rollback", "2026-10-02T14:00:00Z")
		original = bumpCampaignLastTime(t, dir, spec.ID, "2026-10-02T14:00:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 回滚成功结果等于截止：回滚完成不能掩盖早先（本阶段）的迟到结果。
	t.Run("rollback success exactly at deadline", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
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
			At: upBase.Add(30 * time.Minute), Success: false, Reason: "install boom",
		}); err != nil {
			t.Fatal(err)
		}
		rb, err := s.Claim(spec.ID, "d1", upBase.Add(40*time.Minute))
		if err != nil || rb == nil || rb.Kind != StageRollback {
			t.Fatalf("claim rollback: %v %+v", err, rb)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(110 * time.Minute), Success: true,
			Seq: 2, Version: "v1", Config: json.RawMessage(`{"ok":true}`),
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.Report("d1", 3, upBase.Add(115*time.Minute), "v1", json.RawMessage(`{"x":1}`)); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		original := setOpResultAt(t, dir, spec.ID, "d1", "rollback", "2026-10-02T14:00:00Z")
		original = bumpCampaignLastTime(t, dir, spec.ID, "2026-10-02T14:00:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 比较按绝对时刻：结果写为 22:00+08:00（即 14:00Z），恰好等于截止，
	// 时区写法不同仍须识别为迟到。
	t.Run("result at deadline across timezone notation", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(110 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		original := setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T22:00:00+08:00")
		original = bumpCampaignLastTime(t, dir, spec.ID, "2026-10-02T22:00:00+08:00")
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreResultAtDeadlineAccepted 保护合法记录不被截止核对误拒：严格早于
// 截止的结果（含窗口结束后、截止前完成，含相同时刻的不同时区写法）正常打开；
// 截止后重复提交不改变首次记录；已领取无结果而超时的操作没有结果时间，
// 活动的超时状态时间与结束时间等于或晚于截止也都合法。
func TestRestoreResultAtDeadlineAccepted(t *testing.T) {
	// 窗口内领取、窗口结束后截止前完成：结果在 13:50，严格早于 14:00。
	t.Run("result after window before deadline opens", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(50*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(110 * time.Minute), Success: true,
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

	// 失败结果同样只要严格早于截止就合法。
	t.Run("failure result before deadline opens", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(119 * time.Minute), Success: false, Reason: "boom",
		}); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("failure result before deadline must open: %v", err)
		}
		defer s2.Close()
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DeviceFailed {
			t.Fatalf("d1 failure state lost")
		}
	})

	// 改写为另一时区写法表示的同一提前时刻（21:59+08:00 即 13:59Z）必须打开。
	t.Run("before-deadline instant across timezone notation", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(110 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T21:59:00+08:00")
		bumpCampaignLastTime(t, dir, spec.ID, "2026-10-02T21:59:00+08:00")
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("same instant in another timezone must open: %v", err)
		}
		defer s2.Close()
	})

	// 截止前已接受的结果，截止后重复提交不改写首次记录：存储中的首次结果
	// 仍是截止前的时刻，活动随截止超时结束后重开必须正常。
	t.Run("duplicate submission after deadline keeps first record", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(110 * time.Minute), Success: true, // 13:50 首次接受
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.AdvanceCampaign(spec.ID, spec.Deadline); err != nil {
			t.Fatal(err)
		}
		// 截止后重复提交同一结果：幂等成功，首次记录不被改写。
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(3 * time.Hour), Success: true,
		}); err != nil {
			t.Fatalf("duplicate after deadline must stay valid: %v", err)
		}
		cv := mustGetCampaign(s, spec.ID)
		if len(cv.Results) != 1 || !cv.Results[0].At.Equal(upBase.Add(110*time.Minute)) {
			t.Fatalf("first record rewritten by late duplicate: %+v", cv.Results)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("store with pre-deadline result must open: %v", err)
		}
		defer s2.Close()
	})

	// 已领取但尚无结果、随截止超时的操作没有结果时间，正常打开；活动结束
	// 时间恰好等于截止也合法。
	t.Run("claimed without result timed out opens", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.AdvanceCampaign(spec.ID, spec.Deadline); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("timed-out operation without result must open: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if findDevice(cv, "d1").Status != DeviceTimeout || !cv.EndedAt.Equal(spec.Deadline) {
			t.Fatalf("timeout record lost: %+v", cv)
		}
	})

	// 仍在执行的活动中已领取未完成操作没有结果时间，正常打开。
	t.Run("running claimed unfinished opens", func(t *testing.T) {
		_, dir, spec := setupDownloading(t)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("claimed unfinished operation must open: %v", err)
		}
		defer s2.Close()
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DeviceDownloading {
			t.Fatalf("d1 progress lost")
		}
	})

	// 全流程在截止前成功完成：安装成功结果严格早于截止，重开保留终态与影子。
	t.Run("succeeded before deadline opens", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		finishDevice(t, s, spec, "d1", upBase.Add(10*time.Minute))
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("legal succeeded campaign must open: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if !cv.Ended || cv.Status != CampaignSucceeded || len(cv.Results) != 2 {
			t.Fatalf("campaign state changed: %+v", cv)
		}
	})
}

// TestLateSuccessAtDeadlineNotPersisted 锁定正常提交规则：活动仍在执行时，
// 已领取操作第一次在截止时刻提交成功结果不会被保存为操作结果，而是把设备
// 记为超时并失败结束活动；这样产生的存储重开必须合法，且结果历史中没有
// 这条迟到结果。
func TestLateSuccessAtDeadlineNotPersisted(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(20 * time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	in, _ := s.Claim(spec.ID, "d1", upBase.Add(30*time.Minute))
	nResults := len(mustGetCampaign(s, spec.ID).Results)

	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: spec.Deadline, Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{}`),
	}); !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("submission at deadline must end campaign: %v", err)
	}
	cv := mustGetCampaign(s, spec.ID)
	if !cv.Ended || findDevice(cv, "d1").Status != DeviceTimeout || len(cv.Results) != nResults {
		t.Fatalf("late result must not be saved: %+v", cv)
	}
	dir := closeStore(t, s)
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("store produced by deadline processing must open: %v", err)
	}
	defer s2.Close()
	if len(mustGetCampaign(s2, spec.ID).Results) != nResults {
		t.Fatalf("late result leaked into history")
	}
}
