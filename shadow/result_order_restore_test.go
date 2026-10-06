package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// setOpResultAt 同时改写设备某阶段操作的首次结果发生时间（op.at）与结果历史
// 中对应操作标识那条记录的时间，用来构造“结果与历史一致、但结果早于该操作
// 首次领取”的按正常流程无法产生的记录。只改这两处，不改领取时间、设备状态
// 或活动时间基线。
func setOpResultAt(t *testing.T, dir, campaignID, deviceID, op, resultAt string) []byte {
	t.Helper()
	return rewriteStoreBytes(t, dir, func(doc map[string]any) {
		d := campaignDeviceDoc(doc, campaignID, deviceID)
		if d == nil {
			t.Fatalf("device %s missing in campaign %s", deviceID, campaignID)
		}
		o := d[op].(map[string]any)
		opID := o["id"].(string)
		o["at"] = resultAt
		results := campaignDoc(doc, campaignID)["results"].([]any)
		found := false
		for _, r := range results {
			rec := r.(map[string]any)
			if rec["operationId"] == opID {
				rec["at"] = resultAt
				found = true
			}
		}
		if !found {
			t.Fatalf("result history for operation %s missing", opID)
		}
	})
}

// setupReadyAt 构造单设备活动：设备在 claimAt 首次领取下载，并在 resultAt
// 接受下载成功（等待安装），推进活动最新时间到 baselineAt 后关闭，返回目录。
func setupReadyAt(t *testing.T, claimAt, resultAt, baselineAt time.Time) (string, CampaignSpec) {
	t.Helper()
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec.ID, "d1", claimAt)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: resultAt, Success: true,
	}); err != nil {
		t.Fatalf("download result: %v", err)
	}
	if err := s.AdvanceCampaign(spec.ID, baselineAt); err != nil {
		t.Fatal(err)
	}
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir, spec
}

// TestRestoreResultBeforeClaimRefused 保护打开存储时的时间顺序核对：对已经
// 接受结果的下载/安装/回滚操作，保存的首次结果发生时间不得早于该操作自身的
// 首次领取时间。发现任意一条这样的记录，Open 必须返回可被 errors.Is 识别的
// ErrCorruptStorage，拒绝打开整个存储且原文件内容不变；后来的成功或回滚
// 完成不能掩盖早先阶段的矛盾。
func TestRestoreResultBeforeClaimRefused(t *testing.T) {
	// 题设示例：窗口 12:00–13:00，下载 12:10 首次领取，操作结果与对应历史
	// 都记为 12:05，活动最新时间 12:20、设备已等待安装、其他记录自洽，
	// 仍必须拒绝打开。
	t.Run("download result before own claim while ready", func(t *testing.T) {
		dir, spec := setupReadyAt(t,
			upBase.Add(10*time.Minute), // claim 12:10
			upBase.Add(15*time.Minute), // result 12:15
			upBase.Add(20*time.Minute)) // lastTime 12:20
		original := setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T12:05:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 设备后来安装成功、活动成功结束：不能用最终成功掩盖下载阶段的时间倒置。
	t.Run("succeeded campaign cannot mask earlier download inversion", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		finishDevice(t, s, spec, "d1", upBase.Add(10*time.Minute))
		if cv := mustGetCampaign(s, spec.ID); !cv.Ended || cv.Status != CampaignSucceeded {
			t.Fatalf("precondition: %+v", cv)
		}
		dir := s.dir
		s.Close()
		// 下载领取 12:10；把下载结果与历史都改成 12:05（早于其自己的领取，
		// 也早于安装结果，但其他核对仍自洽）。
		original := setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T12:05:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 失败结果适用同一规则：普通下载失败早于自己的领取也必须拒绝。
	t.Run("failed download result before own claim", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(11 * time.Minute), Success: false, Reason: "boom",
		}); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		original := setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T12:05:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 安装失败结果早于安装自己的首次领取：即使后来回滚完成、活动失败结束，
	// 也不能掩盖安装阶段的矛盾。
	t.Run("install failure before own claim with rollback finished", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := rbSpec()
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		in, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
			At: upBase.Add(3 * time.Minute), Success: false, Reason: "install boom",
		}); err != nil {
			t.Fatal(err)
		}
		rb, err := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err != nil || rb == nil {
			t.Fatalf("claim rollback: %v", err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(11 * time.Minute), Success: false, Reason: "rb boom",
		}); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		// 安装领取 12:02；把安装失败结果与历史改成 12:01（与下载结果同时刻，
		// 历史次序仍合法），早于安装自己的领取。
		original := setOpResultAt(t, dir, spec.ID, "d1", "install", "2026-10-02T12:01:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 回滚结果早于回滚自己的首次领取：即使安装失败、回滚失败都已尘埃落定，
	// 回滚阶段自身的时间倒置仍必须拒绝。
	t.Run("rollback result before own claim", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := rbSpec()
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		in, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
			At: upBase.Add(3 * time.Minute), Success: false, Reason: "install boom",
		}); err != nil {
			t.Fatal(err)
		}
		rb, err := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err != nil || rb == nil {
			t.Fatalf("claim rollback: %v", err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(11 * time.Minute), Success: false, Reason: "rb boom",
		}); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		// 回滚 12:10 首次领取；把回滚失败结果与历史改成 12:04（晚于安装失败，
		// 历史次序合法），早于回滚自己的领取。
		original := setOpResultAt(t, dir, spec.ID, "d1", "rollback", "2026-10-02T12:04:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 判断只看该操作自己的两个时刻：另一台设备更早的领取时间不能替代本设备的
	// 首次领取，活动最新时间覆盖该结果也不能使其合法。
	t.Run("other device claim time cannot substitute", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		bringOnline(t, s, "d2")
		// d2 12:05 领取、d1 12:10 领取（同批互不等待）。
		dl2, _ := s.Claim(spec.ID, "d2", upBase.Add(5*time.Minute))
		dl1, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl1.ID,
			At: upBase.Add(15 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d2", OperationID: dl2.ID,
			At: upBase.Add(16 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		// d1 的结果改成 12:05：恰好等于 d2 的领取时刻，却早于 d1 自己的 12:10。
		original := setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T12:05:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 比较按实际时刻进行：结果用另一时区写法表示同一“早于领取”的时刻，
	// 仍必须拒绝（20:05+08:00 即 12:05Z，领取为 12:10Z）。
	t.Run("earlier instant across timezone notation refused", func(t *testing.T) {
		dir, spec := setupReadyAt(t,
			upBase.Add(10*time.Minute),
			upBase.Add(15*time.Minute),
			upBase.Add(20*time.Minute))
		original := setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T20:05:00+08:00")
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreResultOrderAccepted 保护合法记录不被时间顺序核对误拒：两个时刻
// 相等（含不同时区写法的同一时刻）、结果在维护窗口结束后截止前提交、首次
// 领取即因版本不兼容形成的下载失败，都必须正常打开；尚未接受结果的操作
// （已领取未完成、随后因截止超时、未领取的等待步骤）不因缺少结果时间触发。
func TestRestoreResultOrderAccepted(t *testing.T) {
	// 两个时刻恰好相等允许打开。
	t.Run("result at same instant as claim", func(t *testing.T) {
		dir, spec := setupReadyAt(t,
			upBase.Add(10*time.Minute),
			upBase.Add(15*time.Minute),
			upBase.Add(20*time.Minute))
		setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T12:10:00Z")
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("result equal to claim must open: %v", err)
		}
		defer s2.Close()
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DeviceReady {
			t.Fatalf("d1 progress lost")
		}
	})

	// 同一时刻使用不同时区写法不能被误判：领取 12:10Z，结果写 20:10+08:00。
	t.Run("equal instant across timezone notation", func(t *testing.T) {
		dir, spec := setupReadyAt(t,
			upBase.Add(10*time.Minute),
			upBase.Add(15*time.Minute),
			upBase.Add(20*time.Minute))
		setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T20:10:00+08:00")
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("same instant in another timezone must open: %v", err)
		}
		defer s2.Close()
	})

	// 已领取操作在窗口结束后、截止前提交结果：结果不要求落在维护窗口内。
	t.Run("result after window before deadline", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(50*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(70 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("result after window end must open: %v", err)
		}
		defer s2.Close()
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DeviceReady {
			t.Fatalf("d1 must be ready")
		}
	})

	// 首次领取时因版本不兼容直接形成的下载失败：领取与失败为同一时刻，
	// 必须正常保留并打开。
	t.Run("incompatible claim failure at same instant", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		if err := s.Report("d1", 2, upBase.Add(time.Minute), "v9", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		incompatAt := upBase.Add(2 * time.Minute)
		if _, err := s.Claim(spec.ID, "d1", incompatAt); !errors.Is(err, ErrIncompatibleVersion) {
			t.Fatalf("incompatible claim: %v", err)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("claim-time failure at same instant must open: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		d := findDevice(cv, "d1")
		if d.Status != DeviceFailed || d.Phase != StageDownload {
			t.Fatalf("device state changed: %+v", d)
		}
		if len(cv.Results) != 1 {
			t.Fatalf("failure history must be kept: %+v", cv.Results)
		}
	})

	// 已领取但未完成、随后因截止而超时的操作没有结果时间，不触发本错误。
	t.Run("claimed unfinished then timed out opens", func(t *testing.T) {
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
		if findDevice(mustGetCampaign(s, spec.ID), "d1").Status != DeviceTimeout {
			t.Fatalf("precondition")
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("timeout without result must open: %v", err)
		}
		defer s2.Close()
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DeviceTimeout {
			t.Fatalf("timeout state lost")
		}
	})

	// 未领取的等待步骤（等待下载）没有结果时间，沿用原规则正常打开。
	t.Run("pending unclaimed device opens", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("pending device must open: %v", err)
		}
		defer s2.Close()
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DevicePending {
			t.Fatalf("pending state lost")
		}
	})
}
