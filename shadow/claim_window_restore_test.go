package shadow

import (
	"testing"
	"time"
)

// setOpClaimedAt 直接改写设备某阶段（download/install/rollback）首次领取的时间
// （RFC3339 字符串，可用不同时区写法），用来构造按正常流程无法产生的窗口外
// 首次领取记录。
func setOpClaimedAt(t *testing.T, dir, campaignID, deviceID, op, claimedAt string) []byte {
	t.Helper()
	return rewriteStoreBytes(t, dir, func(doc map[string]any) {
		d := campaignDeviceDoc(doc, campaignID, deviceID)
		if d == nil {
			t.Fatalf("device %s missing in campaign %s", deviceID, campaignID)
		}
		o := d[op].(map[string]any)
		o["claimed"] = true
		o["claimedAt"] = claimedAt
	})
}

// setupDownloading 构造单设备活动：设备 12:10 首次领取下载、尚未完成，
// 关闭后返回存储目录。窗口 12:00–13:00，截止 14:00。
func setupDownloading(t *testing.T) (*Store, string, CampaignSpec) {
	t.Helper()
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute)); err != nil {
		t.Fatalf("claim: %v", err)
	}
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return s, dir, spec
}

// TestRestoreClaimWindowRefused 保护打开存储时的维护窗口核对：每个已领取的
// 下载/安装/回滚操作，其首次领取时间必须落在活动保存的原维护窗口
// [start, end) 内；窗口外的首次领取必须使整个存储拒绝打开且内容不变，
// 活动是否结束、操作后来成功/失败/超时都不能掩盖。
func TestRestoreClaimWindowRefused(t *testing.T) {
	// 题设示例：窗口 12:00–13:00，记录表明下载在 11:59 首次领取，
	// 其余状态与结果历史都一致，也必须拒绝打开。
	t.Run("download claimed before window start", func(t *testing.T) {
		_, dir, spec := setupDownloading(t)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", "download", "2026-10-02T11:59:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 窗口不包含结束时刻：恰好 13:00 首次领取即窗口外。
	t.Run("download claimed exactly at window end", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute)); err != nil {
			t.Fatalf("claim: %v", err)
		}
		// 推进时间基线，使被改写的领取时间不触发其他时间核对。
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(90*time.Minute)); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		original := setOpClaimedAt(t, dir, spec.ID, "d1", "download", "2026-10-02T13:00:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 晚于窗口结束的首次领取同样拒绝。
	t.Run("download claimed after window end", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute)); err != nil {
			t.Fatalf("claim: %v", err)
		}
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(100*time.Minute)); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		original := setOpClaimedAt(t, dir, spec.ID, "d1", "download", "2026-10-02T13:30:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 安装的首次领取也受同一窗口约束。
	t.Run("install claimed outside window", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute)); err != nil {
			t.Fatalf("claim install: %v", err)
		}
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(100*time.Minute)); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		original := setOpClaimedAt(t, dir, spec.ID, "d1", "install", "2026-10-02T13:20:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 回滚的首次领取也受同一窗口约束。
	t.Run("rollback claimed outside window", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.RollbackOnFailure = true
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
		if err != nil || rb == nil || rb.Kind != StageRollback {
			t.Fatalf("claim rollback: %v %+v", err, rb)
		}
		dir := s.dir
		s.Close()
		original := setOpClaimedAt(t, dir, spec.ID, "d1", "rollback", "2026-10-02T11:59:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 活动已成功结束：不能用最终成功掩盖首次领取在窗口外。
	t.Run("succeeded campaign with out-of-window claim", func(t *testing.T) {
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
		original := setOpClaimedAt(t, dir, spec.ID, "d1", "download", "2026-10-02T11:59:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 操作后来失败、活动失败结束：同样不能掩盖。
	t.Run("failed device with out-of-window claim", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
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
		original := setOpClaimedAt(t, dir, spec.ID, "d1", "download", "2026-10-02T11:59:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 操作后来随截止超时：超时终态也不能掩盖。
	t.Run("timed out device with out-of-window claim", func(t *testing.T) {
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
		if cv := mustGetCampaign(s, spec.ID); !cv.Ended || findDevice(cv, "d1").Status != DeviceTimeout {
			t.Fatalf("precondition: %+v", cv)
		}
		dir := s.dir
		s.Close()
		original := setOpClaimedAt(t, dir, spec.ID, "d1", "download", "2026-10-02T11:59:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 不同时区写法表示的窗口外时刻仍须拒绝（19:59+08:00 即 11:59Z）。
	t.Run("out-of-window instant across timezone refused", func(t *testing.T) {
		_, dir, spec := setupDownloading(t)
		original := setOpClaimedAt(t, dir, spec.ID, "d1", "download", "2026-10-02T19:59:00+08:00")
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreClaimWindowAccepted 保护合法记录不被窗口核对误拒：窗口边界
// （含开始时刻）、窗口外完成的后续环节、尚未领取的缺省记录都必须正常打开，
// 且已领取操作的标识与待办保持原样。
func TestRestoreClaimWindowAccepted(t *testing.T) {
	// 恰好到达窗口开始时刻的首次领取合法（窗口包含开始时刻）。
	t.Run("claimed exactly at window start", func(t *testing.T) {
		_, dir, spec := setupDownloading(t)
		setOpClaimedAt(t, dir, spec.ID, "d1", "download", "2026-10-02T12:00:00Z")
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("claim at window start must open: %v", err)
		}
		defer s2.Close()
	})

	// 相同时刻的不同时区写法不能被判为窗口外：20:10+08:00 即 12:10Z。
	t.Run("in-window instant across timezone notation", func(t *testing.T) {
		_, dir, spec := setupDownloading(t)
		setOpClaimedAt(t, dir, spec.ID, "d1", "download", "2026-10-02T20:10:00+08:00")
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("same instant in another timezone must open: %v", err)
		}
		defer s2.Close()
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DeviceDownloading {
			t.Fatalf("d1 progress lost")
		}
	})

	// 窗口内领取、窗口外（截止前）提交结果合法：结果接受时间不要求在窗口内。
	t.Run("result accepted after window end reopens", func(t *testing.T) {
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
			t.Fatalf("result outside window must reopen: %v", err)
		}
		defer s2.Close()
		if findDevice(mustGetCampaign(s2, spec.ID), "d1").Status != DeviceReady {
			t.Fatalf("d1 must be ready")
		}
	})

	// 窗口内领取后，窗口外的重复查询返回同一操作：重查时间不要求在窗口内，
	// 重开后已领取操作仍使用原标识、待办与已领取标记保持原样。
	t.Run("re-query after window end keeps original operation", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		again, err := s.Claim(spec.ID, "d1", upBase.Add(80*time.Minute))
		if err != nil || again == nil || again.ID != dl.ID {
			t.Fatalf("re-query must return same op: %v %+v", err, again)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("re-queried operation must reopen: %v", err)
		}
		defer s2.Close()
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending == nil || w.Pending.ID != dl.ID || !w.PendingClaimed {
			t.Fatalf("claimed operation identity changed: %+v", w)
		}
	})

	// 尚未领取的操作不代表窗口外领取：等待下载、等待安装及未领取便
	// 随截止超时的记录都按已有规则读取。
	t.Run("unclaimed operations reopen", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.Devices = []string{"d1", "d2"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		// d1 下载成功后等待安装（安装未领取）；d2 等待下载（未领取）。
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(11 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("unclaimed stages must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if findDevice(cv, "d1").Status != DeviceReady || findDevice(cv, "d2").Status != DevicePending {
			t.Fatalf("pending states lost: %+v", cv.Devices)
		}
	})

	// 开启回滚的活动中回滚尚未领取（等待回滚）：缺省的回滚领取时间
	// 不代表窗口外领取，正常打开。
	t.Run("awaiting rollback without claim reopens", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.RollbackOnFailure = true
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
		if cv := mustGetCampaign(s, spec.ID); findDevice(cv, "d1").Status != DeviceAwaitingRollback {
			t.Fatalf("precondition: %+v", cv.Devices)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("awaiting rollback must reopen: %v", err)
		}
		defer s2.Close()
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.Pending == nil || w.Pending.Kind != StageRollback || w.PendingClaimed ||
			w.Pending.TargetVersion != "v1" {
			t.Fatalf("rollback pending changed: %+v", w)
		}
	})

	// 未开启回滚的既有活动：缺省的回滚操作不应被误判为已领取，
	// 首次领取下载时锁定的回滚目标与设备影子也不受核对影响。
	t.Run("non-rollback campaign default rollback op reopens", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("non-rollback campaign must reopen: %v", err)
		}
		defer s2.Close()
		d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
		if d.Status != DeviceReady || d.RollbackTarget != "" {
			t.Fatalf("non-rollback campaign changed: %+v", d)
		}
	})

	// 正常走完全流程的活动重开不受影响：终态、历史与影子版本保留。
	t.Run("legal succeeded campaign reopens", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		finishDevice(t, s, spec, "d1", upBase.Add(10*time.Minute))
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("legal campaign must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if !cv.Ended || cv.Status != CampaignSucceeded || len(cv.Results) != 2 {
			t.Fatalf("campaign state changed: %+v", cv)
		}
		v, err := s2.Get("d1")
		if err != nil {
			t.Fatal(err)
		}
		if v.Version != "v2" {
			t.Fatalf("shadow version changed: %+v", v)
		}
	})
}
