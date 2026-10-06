package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// setOpResultAt 直接改写设备某阶段（download/install/rollback）已接受结果的
// 首次发生时间（RFC3339 字符串，可用不同时区写法），操作记录与对应的结果
// 历史条目一起改写，用来构造按正常流程无法产生的“结果早于首次领取”记录。
func setOpResultAt(t *testing.T, dir, campaignID, deviceID, op, at string) []byte {
	t.Helper()
	return rewriteStoreBytes(t, dir, func(doc map[string]any) {
		d := campaignDeviceDoc(doc, campaignID, deviceID)
		if d == nil {
			t.Fatalf("device %s missing in campaign %s", deviceID, campaignID)
		}
		o := d[op].(map[string]any)
		opID, _ := o["id"].(string)
		if opID == "" {
			t.Fatalf("device %s %s has no operation id", deviceID, op)
		}
		o["at"] = at
		rs := resultDocs(doc, campaignID)
		var hit bool
		for _, r := range rs {
			if r["operationId"] == opID {
				r["at"] = at
				hit = true
			}
		}
		if !hit {
			t.Fatalf("device %s %s result history missing", deviceID, op)
		}
	})
}

// TestRestoreResultBeforeClaimRefused 保护打开存储时的时间顺序核对：对已经
// 接受结果的下载、安装、回滚操作，保存的首次结果发生时间不得早于同一活动、
// 同一设备、同一阶段的首次领取时间；成功与失败适用同一规则，后来阶段的
// 成功或回滚完成不能掩盖早先阶段的矛盾。命中任意一条都必须以 ErrCorruptStorage
// 拒绝打开整个存储且原文件内容不变。
func TestRestoreResultBeforeClaimRefused(t *testing.T) {
	// 题设示例：窗口 12:00–13:00，下载 12:10 首次领取，下载结果与历史都记为
	// 12:05；活动最新时间 12:20、设备已进入等待安装、其他记录自洽，仍须拒绝。
	t.Run("download result before first claim while ready", func(t *testing.T) {
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
		// 活动最新时间推进到 12:20，覆盖被改写的 12:05，证明不能靠时间基线放行。
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(20*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if cv := mustGetCampaign(s, spec.ID); cv.Ended || findDevice(cv, "d1").Status != DeviceReady {
			t.Fatalf("precondition: %+v", cv)
		}
		dir := closeStore(t, s)
		original := setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T12:05:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 下载失败结果同样适用：失败原因自洽、活动已失败结束也不能掩盖。
	t.Run("download failure before first claim", func(t *testing.T) {
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
		original := setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T12:05:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 安装成功结果早于安装自己的首次领取：即便设备整体已成功、活动已结束，
	// 早先矛盾仍须拒绝。改写后的 12:15 晚于下载结果 12:10，保证结果历史
	// 次序自洽；再用一次更晚的普通上报覆盖附带上报，使该记录不与设备影子
	// 的最近上报比对，从而隔离出本项核对。
	t.Run("install success before own claim", func(t *testing.T) {
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
		// 更晚的普通上报覆盖附带上报：安装成功记录成为历史序号，不再与当前
		// 影子比较时间，但其结果时间仍不得早于自己的首次领取时间。
		if err := s.Report("d1", 3, upBase.Add(40*time.Minute), "v2", json.RawMessage(`{"x":1}`)); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		original := setOpResultAt(t, dir, spec.ID, "d1", "install", "2026-10-02T12:15:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 回滚失败结果早于回滚自己的首次领取：安装失败、等待回滚后的善后也不能
	// 先于回滚领取发生。
	t.Run("rollback failure before own claim", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
		dl, _ := s.Claim(spec.ID, "d1", upBase)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(10 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		in, _ := s.Claim(spec.ID, "d1", upBase.Add(15*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
			At: upBase.Add(20 * time.Minute), Success: false, Reason: "install boom",
		}); err != nil {
			t.Fatal(err)
		}
		rb, err := s.Claim(spec.ID, "d1", upBase.Add(30*time.Minute))
		if err != nil || rb == nil || rb.Kind != StageRollback {
			t.Fatalf("claim rollback: %v %+v", err, rb)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(40 * time.Minute), Success: false, Reason: "rb boom",
		}); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		original := setOpResultAt(t, dir, spec.ID, "d1", "rollback", "2026-10-02T12:25:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 回滚成功同样适用：后来回滚完成不能掩盖回滚结果早于回滚首次领取，
	// 更晚的普通上报同样不能替代两个时刻的直接比较。
	t.Run("rollback success before own claim", func(t *testing.T) {
		s, spec := setupRollbackCampaign(t)
		dl, _ := s.Claim(spec.ID, "d1", upBase)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(10 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		in, _ := s.Claim(spec.ID, "d1", upBase.Add(15*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
			At: upBase.Add(20 * time.Minute), Success: false, Reason: "install boom",
		}); err != nil {
			t.Fatal(err)
		}
		rb, err := s.Claim(spec.ID, "d1", upBase.Add(30*time.Minute))
		if err != nil || rb == nil || rb.Kind != StageRollback {
			t.Fatalf("claim rollback: %v %+v", err, rb)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(40 * time.Minute), Success: true,
			Seq: 2, Version: "v1", Config: json.RawMessage(`{"ok":true}`),
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.Report("d1", 3, upBase.Add(50*time.Minute), "v1", json.RawMessage(`{"x":1}`)); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		original := setOpResultAt(t, dir, spec.ID, "d1", "rollback", "2026-10-02T12:25:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 比较按绝对时刻进行：结果写为 20:05+08:00（即 12:05Z），领取为 12:10Z，
	// 时区写法不同仍须识别为结果早于领取。
	t.Run("result before claim across timezone notation", func(t *testing.T) {
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
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(20*time.Minute)); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		original := setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T20:05:00+08:00")
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreResultBeforeClaimAccepted 保护合法记录不被时间顺序核对误拒：
// 结果与首次领取同一时刻（含首次领取即失败的下载）、窗口外提交结果、
// 尚未接受结果的已领取/超时操作、未领取的等待步骤都必须正常打开。
func TestRestoreResultBeforeClaimAccepted(t *testing.T) {
	// 结果发生时间与首次领取时间相等允许打开。
	t.Run("result at exactly first claim time", func(t *testing.T) {
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
		dir := closeStore(t, s)
		setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T12:10:00Z")
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("result at claim time must open: %v", err)
		}
		defer s2.Close()
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DeviceReady {
			t.Fatalf("d1 progress lost")
		}
	})

	// 相等的两个时刻使用不同时区写法不能被误判：20:10+08:00 即 12:10Z。
	t.Run("equal instant across timezone notation", func(t *testing.T) {
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
		dir := closeStore(t, s)
		setOpResultAt(t, dir, spec.ID, "d1", "download", "2026-10-02T20:10:00+08:00")
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("same instant in another timezone must open: %v", err)
		}
		defer s2.Close()
	})

	// 首次领取时因版本不兼容直接形成下载失败：领取与失败是同一时刻，
	// 必须正常保留并打开。
	t.Run("incompatible claim failure same instant", func(t *testing.T) {
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
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("claim-time failure must open: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		d := findDevice(cv, "d1")
		if d.Status != DeviceFailed || d.Phase != StageDownload {
			t.Fatalf("download failure state lost: %+v", d)
		}
	})

	// 窗口内领取、窗口结束后截止前提交结果合法：结果时间不要求在维护窗口内。
	t.Run("result after window before deadline opens", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(50*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(80 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("result outside window must open: %v", err)
		}
		defer s2.Close()
	})

	// 已领取但尚未接受结果、仍在执行的操作没有结果时间，不触发本项核对。
	t.Run("claimed without result opens", func(t *testing.T) {
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

	// 已领取未完成、随后随截止超时：超时记录没有结果时间，正常打开。
	t.Run("claimed then timed out opens", func(t *testing.T) {
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
			t.Fatalf("timed-out claimed operation must open: %v", err)
		}
		defer s2.Close()
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DeviceTimeout {
			t.Fatalf("timeout state lost")
		}
	})

	// 未领取的等待步骤（等待下载、因前批失败而未执行）没有领取与结果时间，
	// 沿用原规则正常打开。
	t.Run("unclaimed and skipped devices open", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(15 * time.Minute), Success: false, Reason: "boom",
		}); err != nil {
			t.Fatal(err)
		}
		dir := closeStore(t, s)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("unclaimed/skipped devices must open: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if findDevice(cv, "d1").Status != DeviceFailed || findDevice(cv, "d2").Status != DeviceSkipped {
			t.Fatalf("states lost: %+v", cv.Devices)
		}
	})
}
