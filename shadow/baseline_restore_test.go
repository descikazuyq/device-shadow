package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// setCampaignLastTime 直接改写存储文件中活动的时间基线，返回写回字节。
func setCampaignLastTime(t *testing.T, dir, id, at string) []byte {
	t.Helper()
	return rewriteStoreBytes(t, dir, func(doc map[string]any) {
		campaignDoc(doc, id)["lastTime"] = at
	})
}

// deleteCampaignLastTime 删除保存的时间基线，模拟旧记录缺少基线。
func deleteCampaignLastTime(t *testing.T, dir, id string) []byte {
	t.Helper()
	return rewriteStoreBytes(t, dir, func(doc map[string]any) {
		delete(campaignDoc(doc, id), "lastTime")
	})
}

// TestRestoreCampaignTimeBaselineMustCoverRecords 针对“保存的基线只要不早于
// 创建时间就能打开”的问题：重开存储时，活动时间基线必须不早于任一设备已经
// 领取下载/安装/回滚的时间、任一操作首次接受结果的时间、设备当前状态时间；
// 活动已结束时还必须不早于保存的结束时间。矛盾时 Open 返回 ErrCorruptStorage，
// 拒绝打开整个存储并保留原文件，不能抬高基线或改写记录掩盖矛盾。
func TestRestoreCampaignTimeBaselineMustCoverRecords(t *testing.T) {
	// 报告中的例子：12:00 创建，12:10 领取下载，12:15 接受下载成功，
	// 保存的基线却是 12:05。设备进度与结果历史各自自洽，仍必须拒绝打开。
	t.Run("baseline before claim and result times", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, err := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err != nil || dl == nil {
			t.Fatalf("claim download: %v %+v", err, dl)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(15 * time.Minute), Success: true,
		}); err != nil {
			t.Fatalf("download result: %v", err)
		}
		dir := s.dir
		s.Close()
		original := setCampaignLastTime(t, dir, spec.ID, "2026-10-02T12:05:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 基线不早于领取时间、却早于首次接受结果的时间，同样矛盾。
	t.Run("baseline before first accepted result only", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(15 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		// 12:12 晚于 12:10 的领取，却早于 12:15 的结果接受时间。
		original := setCampaignLastTime(t, dir, spec.ID, "2026-10-02T12:12:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 已领取但尚未完成的下载没有结果时间，领取时间本身已是已接受时刻：
	// 基线早于它也必须拒绝，缺结果历史不能让旧时间混进来。
	t.Run("baseline before claim of unfinished download", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute)); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		original := setCampaignLastTime(t, dir, spec.ID, "2026-10-02T12:05:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 回滚活动：基线早于回滚领取时间也必须拒绝。
	t.Run("baseline before rollback claim", func(t *testing.T) {
		s, dir, spec, _ := setupInstallFailure(t, true)
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(6*time.Minute)); err != nil {
			t.Fatal(err)
		}
		s.Close()
		original := setCampaignLastTime(t, dir, spec.ID, "2026-10-02T12:05:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 活动无任何领取/结果/状态时间，仅由截止推进在 14:00 超时结束：
	// 基线 13:00 晚于创建时间，却早于保存的结束时间（以及超时状态时间），
	// 只能由结束时间/设备状态时间检查发现。
	t.Run("baseline before saved end time only", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		if err := s.AdvanceCampaign(spec.ID, spec.Deadline); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		original := setCampaignLastTime(t, dir, spec.ID, "2026-10-02T13:00:00Z")
		assertReopenCorrupt(t, dir, original)
	})

	// 旧记录缺少基线时以创建时间作为基线，并须满足同一检查：
	// 已接受 12:15 的结果却没有基线，不能按创建时间 12:00 静默打开。
	t.Run("missing baseline with later records refused", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase)
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(15 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		original := deleteCampaignLastTime(t, dir, spec.ID)
		assertReopenCorrupt(t, dir, original)
	})

	// 零值基线与缺少基线同样处理。
	t.Run("zero baseline with later records refused", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		original := setCampaignLastTime(t, dir, spec.ID, "0001-01-01T00:00:00Z")
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreCampaignTimeBaselineLegitReopen 保护合法基线：
// 相等时刻、晚于全部记录的基线（显式推进或离线/窗口外查询领取）、
// 尚未领取与尚未推进的正常缺省时间都不得导致打开失败；不同时区表示
// 同一时刻判断相同。重开后保留原基线，倒退请求仍被拒绝。
func TestRestoreCampaignTimeBaselineLegitReopen(t *testing.T) {
	// 报告中的合法侧：12:00 创建、12:10 领取、12:15 接受结果，
	// 基线可以晚于所有记录（显式推进到 12:30），不要求恰好等于最近一条结果。
	t.Run("baseline later than all records", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		dl, _ := s.Claim(spec.ID, "d1", upBase.Add(10*time.Minute))
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: upBase.Add(15 * time.Minute), Success: true,
		}); err != nil {
			t.Fatal(err)
		}
		// 12:30 显式推进（截止前），只推进基线。
		if err := s.AdvanceCampaign(spec.ID, upBase.Add(30*time.Minute)); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()

		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("baseline later than all records must open: %v", err)
		}
		defer s2.Close()
		// 重开后基线保留：12:20 的领取早于 12:30，仍判倒退。
		if _, err := s2.Claim(spec.ID, "d1", upBase.Add(20*time.Minute)); !errors.Is(err, ErrTimeRegression) {
			t.Fatalf("regression after reopen must be refused: %v", err)
		}
		// 12:31 在窗口外查询领取（ready 设备的安装派发不出去），
		// 时间不早于基线，属于正常接受更晚时间，不应报错为倒退。
		if _, err := s2.Claim(spec.ID, "d1", upBase.Add(31*time.Minute)); err != nil {
			t.Fatalf("out-of-window claim query at later time must not regress: %v", err)
		}
	})

	// 基线与最新记录恰好相等合法，且 Open 不得改动合法文件。
	t.Run("baseline equal to latest record", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		original := setCampaignLastTime(t, dir, spec.ID, "2026-10-02T12:01:00Z")
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("equal instant must open: %v", err)
		}
		s.Close()
		data, rerr := os.ReadFile(filepath.Join(dir, storeFileName))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if string(data) != string(original) {
			t.Fatalf("open of valid store must leave file untouched")
		}
	})

	// 基线用 +08:00 表示与最新记录相同的时刻（12:01Z == 20:01+08:00），
	// 必须按时区无关的同一时刻判断，正常打开。
	t.Run("baseline same instant different zone", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		setCampaignLastTime(t, dir, spec.ID, "2026-10-02T20:01:00+08:00")
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("same instant in another zone must open: %v", err)
		}
		defer s.Close()
		// 早于该时刻（12:00:30Z）仍判倒退。
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(30*time.Second)); !errors.Is(err, ErrTimeRegression) {
			t.Fatalf("regression across zone representation must be refused: %v", err)
		}
	})

	// 新建、尚未推进的活动：设备 pending、无任何领取/结果/状态时间，
	// 正常缺省值不能造成打开失败；旧记录缺少基线时以创建时间为基线打开。
	t.Run("fresh pending campaign without baseline", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		dir := s.dir
		s.Close()
		deleteCampaignLastTime(t, dir, spec.ID)
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("fresh campaign without saved baseline must open: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if cv.Ended || findDevice(cv, "d1").Status != DevicePending {
			t.Fatalf("fresh campaign restored wrong: %+v", cv)
		}
		// 基线即创建时间：创建时刻的领取请求合法（相等不判倒退）。
		bringOnline(t, s2, "d1")
		op, err := s2.Claim(spec.ID, "d1", upBase)
		if err != nil || op == nil || op.Kind != StageDownload {
			t.Fatalf("claim at creation time must be accepted: %v %+v", err, op)
		}
	})

	// 已结束活动的纯拒绝与已接受结果的重复提交不推进基线：
	// 重开前后基线保持为结束时刻。
	t.Run("pure reject and duplicate replay keep baseline", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		spec.BatchSize = 1
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		finishDevice(t, s, spec, "d1", upBase) // 12:03 成功结束
		cv0 := mustGetCampaign(s, spec.ID)
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(20*time.Minute)); !errors.Is(err, ErrCampaignEnded) {
			t.Fatalf("ended claim must be pure reject")
		}
		dir := s.dir
		s.Close()

		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("ended campaign must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if !cv.EndedAt.Equal(cv0.EndedAt) {
			t.Fatalf("end time changed: %v vs %v", cv.EndedAt, cv0.EndedAt)
		}
		// 已接受结果的重复提交在结束后仍有效，且不推进基线。
		installRes := cv.Results[1]
		if err := s2.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: installRes.OperationID,
			At: upBase.Add(20 * time.Minute), Success: true,
			Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true}`),
		}); err != nil {
			t.Fatalf("duplicate replay after end: %v", err)
		}
		cv2 := mustGetCampaign(s2, spec.ID)
		if len(cv2.Results) != len(cv.Results) {
			t.Fatalf("replay added history: %+v", cv2.Results)
		}
		// 被拒绝的 12:20 未进入基线：12:10 的显式推进仍合法（不判倒退）。
		if err := s2.AdvanceCampaign(spec.ID, upBase.Add(10*time.Minute)); err != nil {
			t.Fatalf("pure reject must not advance baseline across reopen: %v", err)
		}
	})
}

// TestRestoreNoCampaignsStoreOpens 没有升级活动的旧存储必须正常打开。
func TestRestoreNoCampaignsStoreOpens(t *testing.T) {
	s, dir := openTemp(t)
	if err := s.Register("d1", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("store without campaigns must open: %v", err)
	}
	defer s2.Close()
}
