package shadow

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// activeCampaignSpec 构造一个在 base 之后 offset 时刻创建的单目标 v2 活动规格，
// 窗口为创建后一小时、截止为创建后两小时；与 upSpec 的时间线不相交，
// 用于在同一份存储里放入多个按正常流程无法同时创建的活动。
func activeCampaignSpec(id string, base time.Time, devices []string, batchSize int) CampaignSpec {
	return CampaignSpec{
		ID:            id,
		Operator:      "bob",
		CreatedAt:     base,
		TargetVersion: "v2",
		Devices:       devices,
		BatchSize:     batchSize,
		WindowStart:   base,
		WindowEnd:     base.Add(time.Hour),
		Deadline:      base.Add(2 * time.Hour),
	}
}

// mergeCampaignIntoStore 把 srcDir 存储里 campaignID 的活动磁盘记录原样合并进
// dstDir 的存储文件（两边的版本登记与设备集合此前已分别建好），返回写回字节。
// 用来构造“每个活动单独看都合法、放在一起才冲突”的多活动恢复数据。
func mergeCampaignIntoStore(t *testing.T, dstDir, srcDir, campaignID string) []byte {
	t.Helper()
	readDoc := func(dir string) map[string]any {
		data, err := os.ReadFile(filepath.Join(dir, storeFileName))
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	dst := readDoc(dstDir)
	src := readDoc(srcDir)
	srcCampaign, ok := src["campaigns"].(map[string]any)[campaignID].(map[string]any)
	if !ok {
		t.Fatalf("source store has no campaign %s", campaignID)
	}
	dstCampaigns := dst["campaigns"].(map[string]any)
	if _, exists := dstCampaigns[campaignID]; exists {
		t.Fatalf("destination store already has campaign %s", campaignID)
	}
	dstCampaigns[campaignID] = srcCampaign
	out, err := json.Marshal(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dstDir, storeFileName), out, 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestRestoreOverlappingActiveCampaignsRefused 保护多个活动一同恢复时的活动设备
// 占用规则：同一台已登记设备出现在两项未结束活动中时，Open 必须返回
// ErrCorruptStorage，拒绝返回可用存储；不能只保留其中一项、删除重叠设备或把某项
// 活动改成结束，其余合法设备与活动也不能只恢复一部分。占用只看活动是否结束：
// 设备已安装成功但同批其他设备仍在等待时活动仍未结束；维护窗口不相交、设备离线
// 都不能提前解除占用。
func TestRestoreOverlappingActiveCampaignsRefused(t *testing.T) {
	// 两项未结束活动以最普通的 pending 状态共用 d1。
	t.Run("two pending campaigns share device", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2", "d3")
		spec1 := activeCampaignSpec("cmp-1", upBase, []string{"d1", "d2"}, 2)
		createCampaign(t, s, spec1)
		dstDir := s.dir
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}

		src := setupUpgrade(t, "d1", "d2", "d3")
		spec2 := activeCampaignSpec("cmp-2", upBase.Add(3*time.Hour), []string{"d1", "d3"}, 2)
		createCampaign(t, src, spec2)
		srcDir := src.dir
		src.Close()

		original := mergeCampaignIntoStore(t, dstDir, srcDir, "cmp-2")
		assertReopenCorrupt(t, dstDir, original)
	})

	// d1 在 cmp-1 中已安装成功，但同批 d2 仍 pending，活动尚未结束：
	// d1 依旧不能同时属于另一项未结束活动 cmp-2（其中 d3 合法也不允许只恢复它）。
	t.Run("device succeeded but its campaign is still active", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2", "d3")
		spec1 := activeCampaignSpec("cmp-1", upBase, []string{"d1", "d2"}, 2)
		createCampaign(t, s, spec1)
		bringOnline(t, s, "d1")
		finishDevice(t, s, spec1, "d1", upBase)
		cv := mustGetCampaign(s, "cmp-1")
		if cv.Ended || cv.Status != CampaignRunning {
			t.Fatalf("precondition: cmp-1 must stay running: %+v", cv)
		}
		if findDevice(cv, "d1").Status != DeviceSucceeded ||
			findDevice(cv, "d2").Status != DevicePending {
			t.Fatalf("precondition: d1 succeeded and d2 pending: %+v", cv.Devices)
		}
		dstDir := s.dir
		s.Close()

		src := setupUpgrade(t, "d1", "d2", "d3")
		spec2 := activeCampaignSpec("cmp-2", upBase.Add(3*time.Hour), []string{"d1", "d3"}, 2)
		createCampaign(t, src, spec2)
		srcDir := src.dir
		src.Close()

		original := mergeCampaignIntoStore(t, dstDir, srcDir, "cmp-2")
		assertReopenCorrupt(t, dstDir, original)
	})

	// 叠加两个“看起来暂时领不到操作”的条件：两项活动维护窗口不相交，
	// 且重叠设备此刻离线。占用判定不依赖窗口或在线状态，仍然必须拒绝打开。
	// 同时让 d1 在第二项活动里已经走完下载（ready），证明任一活动中的进度
	// 形态都不影响判定。
	t.Run("disjoint windows and offline do not release device", func(t *testing.T) {
		s := setupUpgrade(t, "d1", "d2", "d3")
		spec1 := activeCampaignSpec("cmp-1", upBase, []string{"d1", "d2"}, 2)
		createCampaign(t, s, spec1)
		bringOnline(t, s, "d1")
		finishDevice(t, s, spec1, "d1", upBase)
		if err := s.SetOffline("d1"); err != nil {
			t.Fatal(err)
		}
		if sh, _ := s.Get("d1"); sh.Online {
			t.Fatal("precondition: d1 must be offline")
		}
		dstDir := s.dir
		s.Close()

		src := setupUpgrade(t, "d1", "d2", "d3")
		base2 := upBase.Add(3 * time.Hour) // 窗口 15:00–16:00，与 cmp-1 的 12:00–13:00 不相交
		spec2 := activeCampaignSpec("cmp-2", base2, []string{"d1", "d3"}, 2)
		createCampaign(t, src, spec2)
		bringOnline(t, src, "d1")
		dl, err := src.Claim(spec2.ID, "d1", base2)
		if err != nil || dl == nil {
			t.Fatalf("claim download: %v %+v", err, dl)
		}
		if err := src.SubmitResult(OperationResult{
			CampaignID: spec2.ID, DeviceID: "d1", OperationID: dl.ID,
			At: base2.Add(time.Minute), Success: true,
		}); err != nil {
			t.Fatalf("download result: %v", err)
		}
		if d := findDevice(mustGetCampaign(src, "cmp-2"), "d1"); d.Status != DeviceReady {
			t.Fatalf("precondition: d1 ready in cmp-2, got %s", d.Status)
		}
		srcDir := src.dir
		src.Close()

		original := mergeCampaignIntoStore(t, dstDir, srcDir, "cmp-2")
		assertReopenCorrupt(t, dstDir, original)
	})
}

// TestRestoreEndedCampaignHistoryCoexistsWithActive 保护合法活动历史：
// 同一台设备可以留在已结束的旧活动中并参加新的未结束活动，合并恢复必须成功。
// GetCampaign 同时保留旧活动终态与历史、新活动当前进度；GetDeviceWork 只指向
// 新活动的待办，待办的阶段、操作标识与领取状态与保存时一致。
func TestRestoreEndedCampaignHistoryCoexistsWithActive(t *testing.T) {
	// 新活动中 d1 已下载成功、安装尚未领取：待办应为未领取的安装操作。
	t.Run("unclaimed install in new campaign", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		oldSpec := activeCampaignSpec("cmp-1", upBase, []string{"d1"}, 1)
		createCampaign(t, s, oldSpec)
		bringOnline(t, s, "d1")
		finishDevice(t, s, oldSpec, "d1", upBase)
		oldBefore := mustGetCampaign(s, "cmp-1")
		if !oldBefore.Ended || oldBefore.Status != CampaignSucceeded {
			t.Fatalf("precondition: cmp-1 must be ended: %+v", oldBefore)
		}
		dstDir := s.dir
		s.Close()

		src := setupUpgrade(t, "d1")
		base2 := upBase.Add(3 * time.Hour)
		newSpec := activeCampaignSpec("cmp-2", base2, []string{"d1"}, 1)
		createCampaign(t, src, newSpec)
		bringOnline(t, src, "d1")
		dl, err := src.Claim(newSpec.ID, "d1", base2)
		if err != nil || dl == nil {
			t.Fatalf("claim download: %v %+v", err, dl)
		}
		if err := src.SubmitResult(OperationResult{
			CampaignID: newSpec.ID, DeviceID: "d1", OperationID: dl.ID,
			At: base2.Add(time.Minute), Success: true,
		}); err != nil {
			t.Fatalf("download result: %v", err)
		}
		wantWork, err := src.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		srcDir := src.dir
		src.Close()

		mergeCampaignIntoStore(t, dstDir, srcDir, "cmp-2")
		s2, err := Open(dstDir)
		if err != nil {
			t.Fatalf("ended + active campaign sharing a device must open: %v", err)
		}
		defer s2.Close()

		// 旧活动终态与结果历史原样保留。
		old := mustGetCampaign(s2, "cmp-1")
		if !old.Ended || old.Status != CampaignSucceeded {
			t.Fatalf("old campaign terminal state lost: %+v", old)
		}
		if findDevice(old, "d1").Status != DeviceSucceeded {
			t.Fatalf("old campaign device state lost: %+v", findDevice(old, "d1"))
		}
		if len(old.Results) != len(oldBefore.Results) {
			t.Fatalf("old history count changed: %+v vs %+v", old.Results, oldBefore.Results)
		}
		for i := range oldBefore.Results {
			if old.Results[i] != oldBefore.Results[i] {
				t.Fatalf("old history %d changed: %+v vs %+v", i, old.Results[i], oldBefore.Results[i])
			}
		}

		// 新活动当前进度保留：running、d1 ready、仅一条下载历史。
		newC := mustGetCampaign(s2, "cmp-2")
		if newC.Ended || newC.Status != CampaignRunning {
			t.Fatalf("new campaign must stay running: %+v", newC)
		}
		if d := findDevice(newC, "d1"); d.Status != DeviceReady || d.Phase != StageInstall {
			t.Fatalf("new campaign progress lost: %+v", d)
		}
		if len(newC.Results) != 1 || newC.Results[0].Stage != StageDownload ||
			!newC.Results[0].Success {
			t.Fatalf("new campaign history lost: %+v", newC.Results)
		}

		// 待办指向新活动的未领取安装，阶段、操作标识、领取状态与保存时一致。
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.CampaignID != "cmp-2" {
			t.Fatalf("work must point to active campaign cmp-2, got %q", w.CampaignID)
		}
		if w.Pending == nil {
			t.Fatalf("pending install missing: %+v", w)
		}
		if w.Pending.Kind != StageInstall ||
			w.Pending.ID != "cmp-2:d1:install" ||
			w.PendingClaimed {
			t.Fatalf("pending work changed after restore: %+v", w)
		}
		if wantWork.Pending == nil ||
			w.Pending.ID != wantWork.Pending.ID ||
			w.Pending.Kind != wantWork.Pending.Kind ||
			w.PendingClaimed != wantWork.PendingClaimed {
			t.Fatalf("pending work differs from saved state: %+v vs %+v", w, wantWork)
		}
	})

	// 新活动中 d1 已领取下载但尚未完成：重开后待办仍是同一个已领取下载标识，
	// 领取状态不能因读取旧活动历史而丢失。
	t.Run("claimed unfinished download in new campaign", func(t *testing.T) {
		s := setupUpgrade(t, "d1")
		oldSpec := activeCampaignSpec("cmp-1", upBase, []string{"d1"}, 1)
		createCampaign(t, s, oldSpec)
		bringOnline(t, s, "d1")
		finishDevice(t, s, oldSpec, "d1", upBase)
		dstDir := s.dir
		s.Close()

		src := setupUpgrade(t, "d1")
		base2 := upBase.Add(3 * time.Hour)
		newSpec := activeCampaignSpec("cmp-2", base2, []string{"d1"}, 1)
		createCampaign(t, src, newSpec)
		bringOnline(t, src, "d1")
		dl, err := src.Claim(newSpec.ID, "d1", base2)
		if err != nil || dl == nil || dl.ID != "cmp-2:d1:download" {
			t.Fatalf("claim download: %v %+v", err, dl)
		}
		wantWork, err := src.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		srcDir := src.dir
		src.Close()

		mergeCampaignIntoStore(t, dstDir, srcDir, "cmp-2")
		s2, err := Open(dstDir)
		if err != nil {
			t.Fatalf("ended + active campaign sharing a device must open: %v", err)
		}
		defer s2.Close()

		newC := mustGetCampaign(s2, "cmp-2")
		if d := findDevice(newC, "d1"); d.Status != DeviceDownloading || d.Phase != StageDownload {
			t.Fatalf("new campaign progress lost: %+v", d)
		}
		if len(newC.Results) != 0 {
			t.Fatalf("claimed unfinished download must have no history: %+v", newC.Results)
		}
		w, err := s2.GetDeviceWork("d1")
		if err != nil {
			t.Fatal(err)
		}
		if w.CampaignID != "cmp-2" || w.Pending == nil {
			t.Fatalf("work must point to cmp-2 pending op: %+v", w)
		}
		if w.Pending.Kind != StageDownload || w.Pending.ID != "cmp-2:d1:download" ||
			!w.PendingClaimed {
			t.Fatalf("claimed download not restored as saved: %+v", w)
		}
		if wantWork.Pending == nil ||
			w.Pending.ID != wantWork.Pending.ID ||
			w.Pending.Kind != wantWork.Pending.Kind ||
			w.PendingClaimed != wantWork.PendingClaimed {
			t.Fatalf("pending work differs from saved state: %+v vs %+v", w, wantWork)
		}
	})
}

// TestRestoreMultipleActiveCampaignsDisjointDevices 多项未结束活动使用互不重叠
// 的设备集合时属于合法数据，必须一同正常打开，各设备待办仍指向各自的活动。
func TestRestoreMultipleActiveCampaignsDisjointDevices(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2", "d3")
	spec1 := activeCampaignSpec("cmp-1", upBase, []string{"d1"}, 1)
	createCampaign(t, s, spec1)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec1.ID, "d1", upBase)
	if err != nil {
		t.Fatalf("claim d1 download: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec1.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("d1 download: %v", err)
	}
	dstDir := s.dir
	s.Close()

	src := setupUpgrade(t, "d1", "d2", "d3")
	spec2 := activeCampaignSpec("cmp-2", upBase.Add(3*time.Hour), []string{"d2", "d3"}, 2)
	createCampaign(t, src, spec2)
	srcDir := src.dir
	src.Close()

	mergeCampaignIntoStore(t, dstDir, srcDir, "cmp-2")
	s2, err := Open(dstDir)
	if err != nil {
		t.Fatalf("active campaigns with disjoint device sets must open: %v", err)
	}
	defer s2.Close()

	c1 := mustGetCampaign(s2, "cmp-1")
	c2 := mustGetCampaign(s2, "cmp-2")
	if c1.Ended || c2.Ended || c1.Status != CampaignRunning || c2.Status != CampaignRunning {
		t.Fatalf("both campaigns must be restored running: %+v %+v", c1.Status, c2.Status)
	}
	if findDevice(c1, "d1").Status != DeviceReady {
		t.Fatalf("cmp-1 progress lost: %+v", findDevice(c1, "d1"))
	}
	if findDevice(c2, "d2").Status != DevicePending || findDevice(c2, "d3").Status != DevicePending {
		t.Fatalf("cmp-2 devices lost: %+v %+v", findDevice(c2, "d2"), findDevice(c2, "d3"))
	}

	w1, err := s2.GetDeviceWork("d1")
	if err != nil {
		t.Fatal(err)
	}
	if w1.CampaignID != "cmp-1" || w1.Pending == nil ||
		w1.Pending.Kind != StageInstall || w1.PendingClaimed {
		t.Fatalf("d1 work must point at cmp-1 install: %+v", w1)
	}
	for _, id := range []string{"d2", "d3"} {
		w, err := s2.GetDeviceWork(id)
		if err != nil {
			t.Fatal(err)
		}
		if w.CampaignID != "cmp-2" || w.Pending == nil ||
			w.Pending.Kind != StageDownload || w.PendingClaimed {
			t.Fatalf("%s work must point at cmp-2 download: %+v", id, w)
		}
	}
}
