package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 本组回归测试保护“重新打开本地存储时的活动设备占用规则”：
// 一台已登记设备不能同时属于两项未结束（未 ended）的升级活动，
// Open 恢复多项活动时必须与 CreateCampaign 遵守同一规则，违反即 ErrCorruptStorage。
//
// 占用与否只看所属活动是否结束：设备自身已安装成功但同批仍有设备在等待、
// 维护窗口互不相交、设备暂时离线领不到操作，都不解除占用。

// overlapStore 在一个全新临时目录中打开存储，由 build 构造场景后关闭，返回目录。
func overlapStore(t *testing.T, build func(s *Store)) string {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	build(s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

// overlapBaseline 登记 v2<-v1、v3<-v2 两个升级目标和 d1/d2/d3 三台 v1 设备。
// 多个场景目录保持相同的设备与版本集合，使合并后的版本、设备引用都合法。
func overlapBaseline(s *Store) {
	if err := s.RegisterUpgrade("v2", []string{"v1"}); err != nil {
		panic(err)
	}
	if err := s.RegisterUpgrade("v3", []string{"v2"}); err != nil {
		panic(err)
	}
	for _, id := range []string{"d1", "d2", "d3"} {
		if err := s.Register(id, "v1"); err != nil {
			panic(err)
		}
	}
}

func overlapSpec(id string, created time.Time, target string, devices []string) CampaignSpec {
	return CampaignSpec{
		ID:            id,
		Operator:      "alice",
		CreatedAt:     created,
		TargetVersion: target,
		Devices:       devices,
		BatchSize:     len(devices),
		WindowStart:   created,
		WindowEnd:     created.Add(time.Hour),
		Deadline:      created.Add(2 * time.Hour),
	}
}

// readyDownload 让设备在线（保持其当前版本与序号）、领取下载并提交成功，
// 设备停在 ready（安装待领取）。
func readyDownload(t *testing.T, s *Store, spec CampaignSpec, id string, at time.Time) {
	t.Helper()
	v, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	if err := s.Report(id, v.LastSeq+1, at.Add(-time.Minute), v.Version, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("bring online %s: %v", id, err)
	}
	dl, err := s.Claim(spec.ID, id, at)
	if err != nil || dl == nil {
		t.Fatalf("claim download %s: %v %+v", id, err, dl)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: id, OperationID: dl.ID,
		At: at.Add(time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("download result %s: %v", id, err)
	}
}

func readStoreDoc(t *testing.T, dir string) map[string]any {
	t.Helper()
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

func writeStoreDoc(t *testing.T, dir string, doc map[string]any) []byte {
	t.Helper()
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, storeFileName), out, 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

// mergeCampaigns 把各来源存储里的活动整体并入目标存储文件（每个来源单独打开
// 都是合法存储），返回写回后的字节。
func mergeCampaigns(t *testing.T, dst string, srcs ...string) []byte {
	t.Helper()
	doc := readStoreDoc(t, dst)
	camps := doc["campaigns"].(map[string]any)
	for _, sd := range srcs {
		src := readStoreDoc(t, sd)
		for k, v := range src["campaigns"].(map[string]any) {
			camps[k] = v
		}
	}
	return writeStoreDoc(t, dst, doc)
}

// TestRestoreActiveCampaignDeviceOverlapRefused 两份不同的未结束活动包含同一台
// 已登记设备时，即使每项活动单独看设备列表、版本引用、阶段进度与结果历史都合法，
// Open 也必须返回 ErrCorruptStorage，且原文件保持原样——不能只恢复“合法部分”。
func TestRestoreActiveCampaignDeviceOverlapRefused(t *testing.T) {
	t.Run("two running campaigns share a ready device with history", func(t *testing.T) {
		// 活动 cmp-a：d1 下载成功（带结果历史、等待安装），d2 尚未开始。
		dirA := overlapStore(t, func(s *Store) {
			overlapBaseline(s)
			spec := overlapSpec("cmp-a", upBase, "v2", []string{"d1", "d2"})
			createCampaign(t, s, spec)
			readyDownload(t, s, spec, "d1", upBase)
		})
		// 活动 cmp-b：同样的 d1 下载成功（带历史），d3 尚未开始。
		dirB := overlapStore(t, func(s *Store) {
			overlapBaseline(s)
			spec := overlapSpec("cmp-b", upBase, "v2", []string{"d1", "d3"})
			createCampaign(t, s, spec)
			readyDownload(t, s, spec, "d1", upBase)
		})
		original := mergeCampaigns(t, dirA, dirB)
		// d2、d3 与两项活动其余内容都合法，也不能只恢复不冲突的部分。
		assertReopenCorrupt(t, dirA, original)
	})

	t.Run("succeeded device still busy while same campaign awaits another device", func(t *testing.T) {
		// d1 在 cmp-a 中已安装成功，但同批 d2 仍在等待：cmp-a 未结束，
		// d1 不得同时属于另一项未结束活动 cmp-b。
		dirA := overlapStore(t, func(s *Store) {
			overlapBaseline(s)
			spec := overlapSpec("cmp-a", upBase, "v2", []string{"d1", "d2"})
			createCampaign(t, s, spec)
			bringOnline(t, s, "d1")
			finishDevice(t, s, spec, "d1", upBase) // d1 成功；d2 仍 pending，活动未结束
			if cv := mustGetCampaign(s, spec.ID); cv.Ended {
				t.Fatalf("test setup: campaign must stay running while d2 pending")
			}
		})
		dirB := overlapStore(t, func(s *Store) {
			overlapBaseline(s)
			// 与目标存储一致：d1 已在普通上报中升级到 v2，再以 v3 为目标参加 cmp-b。
			bringOnline(t, s, "d1")
			if err := s.Report("d1", 2, upBase.Add(3*time.Minute), "v2", json.RawMessage(`{"ok":true}`)); err != nil {
				t.Fatal(err)
			}
			spec := overlapSpec("cmp-b", upBase, "v3", []string{"d1"})
			createCampaign(t, s, spec)
			readyDownload(t, s, spec, "d1", upBase.Add(4*time.Minute))
		})
		original := mergeCampaigns(t, dirA, dirB)
		assertReopenCorrupt(t, dirA, original)
	})

	t.Run("disjoint maintenance windows do not release the hold", func(t *testing.T) {
		dirA := overlapStore(t, func(s *Store) {
			overlapBaseline(s)
			spec := overlapSpec("cmp-a", upBase, "v2", []string{"d1", "d2"})
			createCampaign(t, s, spec)
			bringOnline(t, s, "d1")
		})
		later := upBase.Add(48 * time.Hour)
		dirB := overlapStore(t, func(s *Store) {
			overlapBaseline(s)
			// 维护窗口与 cmp-a 完全不相交：占用判定与窗口无关。
			spec := overlapSpec("cmp-b", later, "v2", []string{"d1", "d3"})
			createCampaign(t, s, spec)
			bringOnline(t, s, "d1")
		})
		original := mergeCampaigns(t, dirA, dirB)
		assertReopenCorrupt(t, dirA, original)
	})

	t.Run("offline device with no claimable op is still occupied", func(t *testing.T) {
		// d1 在 cmp-a 中已领取下载但未完成，随后离线；cmp-b 中 d1 尚未开始。
		// 暂时领不到操作不等于没有占用。
		dirA := overlapStore(t, func(s *Store) {
			overlapBaseline(s)
			spec := overlapSpec("cmp-a", upBase, "v2", []string{"d1", "d2"})
			createCampaign(t, s, spec)
			bringOnline(t, s, "d1")
			dl, err := s.Claim(spec.ID, "d1", upBase.Add(time.Minute))
			if err != nil || dl == nil {
				t.Fatalf("claim: %v %+v", err, dl)
			}
			if err := s.SetOffline("d1"); err != nil {
				t.Fatal(err)
			}
		})
		dirB := overlapStore(t, func(s *Store) {
			overlapBaseline(s) // d1 全程离线、pending
			spec := overlapSpec("cmp-b", upBase, "v2", []string{"d1", "d3"})
			createCampaign(t, s, spec)
		})
		original := mergeCampaigns(t, dirA, dirB)
		assertReopenCorrupt(t, dirA, original)
	})

	t.Run("dropping the shared device cannot paper over the conflict", func(t *testing.T) {
		// 重叠数据本身必须拒绝打开并原样保留，不能靠删除重叠设备继续。
		dirA := overlapStore(t, func(s *Store) {
			overlapBaseline(s)
			spec := overlapSpec("cmp-a", upBase, "v2", []string{"d1", "d2"})
			createCampaign(t, s, spec)
			readyDownload(t, s, spec, "d1", upBase)
		})
		dirB := overlapStore(t, func(s *Store) {
			overlapBaseline(s)
			spec := overlapSpec("cmp-b", upBase, "v2", []string{"d1", "d3"})
			createCampaign(t, s, spec)
			readyDownload(t, s, spec, "d1", upBase)
		})
		original := mergeCampaigns(t, dirA, dirB)
		assertReopenCorrupt(t, dirA, original)

		// 手工“修复”：从 cmp-b 设备列表删掉 d1，但其下载结果历史仍引用 d1，
		// 仍属损坏，改写后的文件也不得被覆盖。
		tampered := rewriteStoreBytes(t, dirA, func(doc map[string]any) {
			b := campaignDoc(doc, "cmp-b")
			devs := b["devices"].([]any)
			kept := make([]any, 0, len(devs))
			for _, d := range devs {
				if d.(map[string]any)["deviceId"] != "d1" {
					kept = append(kept, d)
				}
			}
			b["devices"] = kept
		})
		assertReopenCorrupt(t, dirA, tampered)
	})
}

// TestRestoreEndedCampaignHistoryKeptWithNewWork 同一台设备可以留在已结束的旧
// 活动历史中，同时参加一项新的未结束活动：重开必须成功，旧活动终态与历史保留，
// 新活动进度延续，GetDeviceWork 指向新活动待办，阶段/操作标识/领取状态不变。
func TestRestoreEndedCampaignHistoryKeptWithNewWork(t *testing.T) {
	newBase := upBase.Add(24 * time.Hour)
	dir := overlapStore(t, func(s *Store) {
		overlapBaseline(s)
		// 旧活动 cmp-old：d1 下载并安装成功，活动成功结束，含两条历史。
		old := overlapSpec("cmp-old", upBase, "v2", []string{"d1"})
		createCampaign(t, s, old)
		bringOnline(t, s, "d1")
		finishDevice(t, s, old, "d1", upBase)
		// 新活动 cmp-new：d1 从 v2 升级到 v3，下载成功、等待安装。
		spec := overlapSpec("cmp-new", newBase, "v3", []string{"d1"})
		createCampaign(t, s, spec)
		readyDownload(t, s, spec, "d1", newBase)
	})

	s, err := Open(dir)
	if err != nil {
		t.Fatalf("ended campaign plus active campaign with same device must open: %v", err)
	}
	defer s.Close()

	// 旧活动的终态与结果历史原样保留。
	old := mustGetCampaign(s, "cmp-old")
	if !old.Ended || old.Status != CampaignSucceeded {
		t.Fatalf("old campaign terminal state lost: %+v", old)
	}
	if d := findDevice(old, "d1"); d.Status != DeviceSucceeded {
		t.Fatalf("old campaign device state lost: %+v", d)
	}
	if !old.EndedAt.Equal(upBase.Add(3 * time.Minute)) {
		t.Fatalf("old campaign endedAt lost: %v", old.EndedAt)
	}
	if len(old.Results) != 2 {
		t.Fatalf("old campaign history lost: %+v", old.Results)
	}
	if old.Results[0].Stage != StageDownload || old.Results[0].Success != true ||
		old.Results[0].OperationID != "cmp-old:d1:download" {
		t.Fatalf("old download history changed: %+v", old.Results[0])
	}
	if old.Results[1].Stage != StageInstall || old.Results[1].Version != "v2" ||
		old.Results[1].OperationID != "cmp-old:d1:install" {
		t.Fatalf("old install history changed: %+v", old.Results[1])
	}

	// 新活动进度保留：仍在执行，d1 ready，一条下载历史。
	newc := mustGetCampaign(s, "cmp-new")
	if newc.Ended || newc.Status != CampaignRunning {
		t.Fatalf("new campaign must stay running: %+v", newc)
	}
	if d := findDevice(newc, "d1"); d.Status != DeviceReady {
		t.Fatalf("new campaign progress lost: %+v", d)
	}

	// 待办指向新活动尚未领取的安装，而不是旧活动。
	w, err := s.GetDeviceWork("d1")
	if err != nil {
		t.Fatal(err)
	}
	if w.CampaignID != "cmp-new" || w.Version != "v2" {
		t.Fatalf("work must point at new campaign: %+v", w)
	}
	if w.Pending == nil || w.Pending.Kind != StageInstall ||
		w.Pending.ID != "cmp-new:d1:install" || w.PendingClaimed {
		t.Fatalf("pending install after reopen: %+v", w)
	}

	// 恢复后创建路径仍遵守同一规则：d1 被未结束的 cmp-new 占用。
	extra := overlapSpec("cmp-extra", newBase.Add(3*time.Hour), "v3", []string{"d1"})
	if err := s.CreateCampaign(extra); !errors.Is(err, ErrDeviceBusy) {
		t.Fatalf("restored active campaign must block CreateCampaign: %v", err)
	}

	// 在重开后的存储上领取安装并再次重开：领取状态与操作标识必须延续。
	in, err := s.Claim("cmp-new", "d1", newBase.Add(2*time.Minute))
	if err != nil || in == nil || in.Kind != StageInstall || in.ID != "cmp-new:d1:install" {
		t.Fatalf("claim install after reopen: %v %+v", err, in)
	}
	before, _ := s.GetDeviceWork("d1")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after claim: %v", err)
	}
	defer s2.Close()
	after, err := s2.GetDeviceWork("d1")
	if err != nil {
		t.Fatal(err)
	}
	if after.CampaignID != "cmp-new" || !after.PendingClaimed ||
		after.Pending == nil || after.Pending.Kind != StageInstall ||
		after.Pending.ID != "cmp-new:d1:install" {
		t.Fatalf("claimed todo not preserved: before %+v after %+v", before, after)
	}
	if d := findDevice(mustGetCampaign(s2, "cmp-new"), "d1"); d.Status != DeviceInstalling {
		t.Fatalf("installing status not preserved: %+v", d)
	}
	// 旧活动历史不受再次重开影响。
	if old2 := mustGetCampaign(s2, "cmp-old"); len(old2.Results) != 2 ||
		old2.Results[1].Version != "v2" || !old2.Ended {
		t.Fatalf("old history changed on second reopen: %+v", old2.Results)
	}
}

// TestRestoreMultipleActiveCampaignsDisjointDevices 多项未结束活动使用互不重叠
// 的设备时正常打开，各自进度与设备待办都指向所属活动。
func TestRestoreMultipleActiveCampaignsDisjointDevices(t *testing.T) {
	dir := overlapStore(t, func(s *Store) {
		overlapBaseline(s)
		if err := s.Register("d4", "v1"); err != nil {
			panic(err)
		}
		specA := overlapSpec("cmp-a", upBase, "v2", []string{"d1", "d2"})
		specB := overlapSpec("cmp-b", upBase.Add(3*time.Hour), "v2", []string{"d3", "d4"})
		createCampaign(t, s, specA)
		// cmp-a 未结束时创建设备不重叠的 cmp-b：成功。
		createCampaign(t, s, specB)
		// cmp-a：d1 下载成功待安装，d2 未开始。
		readyDownload(t, s, specA, "d1", upBase)
		// cmp-b：d3 已领取下载未完成，d4 未开始。
		bringOnline(t, s, "d3")
		dl, err := s.Claim(specB.ID, "d3", upBase.Add(3*time.Hour).Add(time.Minute))
		if err != nil || dl == nil {
			t.Fatalf("claim d3: %v %+v", err, dl)
		}
	})

	before := map[string]DeviceWork{}
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("multiple running campaigns with disjoint devices must open: %v", err)
	}
	defer s.Close()

	for _, id := range []string{"cmp-a", "cmp-b"} {
		cv := mustGetCampaign(s, id)
		if cv.Ended || cv.Status != CampaignRunning {
			t.Fatalf("%s must stay running: %+v", id, cv)
		}
	}
	if d := findDevice(mustGetCampaign(s, "cmp-a"), "d1"); d.Status != DeviceReady {
		t.Fatalf("d1 progress lost: %+v", d)
	}
	if d := findDevice(mustGetCampaign(s, "cmp-b"), "d3"); d.Status != DeviceDownloading {
		t.Fatalf("d3 progress lost: %+v", d)
	}

	want := []struct {
		device   string
		campaign string
		kind     string
		opID     string
		claimed  bool
	}{
		{"d1", "cmp-a", StageInstall, "cmp-a:d1:install", false},
		{"d2", "cmp-a", StageDownload, "cmp-a:d2:download", false},
		{"d3", "cmp-b", StageDownload, "cmp-b:d3:download", true},
		{"d4", "cmp-b", StageDownload, "cmp-b:d4:download", false},
	}
	for _, w := range want {
		got, err := s.GetDeviceWork(w.device)
		if err != nil {
			t.Fatal(err)
		}
		before[w.device] = got
		if got.CampaignID != w.campaign || got.Pending == nil ||
			got.Pending.Kind != w.kind || got.Pending.ID != w.opID ||
			got.PendingClaimed != w.claimed {
			t.Fatalf("work for %s: got %+v, want campaign=%s kind=%s op=%s claimed=%v",
				w.device, got, w.campaign, w.kind, w.opID, w.claimed)
		}
	}

	// 恢复后占用规则仍然有效：任一活动中的设备都不能再参加第三个未结束活动。
	for _, dev := range []string{"d1", "d2", "d3", "d4"} {
		spec := overlapSpec("cmp-c", upBase.Add(8*time.Hour), "v2", []string{dev})
		if err := s.CreateCampaign(spec); !errors.Is(err, ErrDeviceBusy) {
			t.Fatalf("%s must stay busy after restore, got %v", dev, err)
		}
	}

	// 再次重开，待办与首次恢复结果完全一致。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("second reopen: %v", err)
	}
	defer s2.Close()
	for _, w := range want {
		got, err := s2.GetDeviceWork(w.device)
		if err != nil {
			t.Fatal(err)
		}
		prev := before[w.device]
		if got.CampaignID != prev.CampaignID || got.PendingClaimed != prev.PendingClaimed ||
			got.Pending == nil || prev.Pending == nil ||
			got.Pending.ID != prev.Pending.ID || got.Pending.Kind != prev.Pending.Kind {
			t.Fatalf("work for %s changed across reopen: %+v vs %+v", w.device, prev, got)
		}
	}
}
