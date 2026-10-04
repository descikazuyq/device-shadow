package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestOperationIDsDistinguishColonCampaignAndDevice 直接复现修复前的串用问题：
// 活动 a:b 中的设备 c 与活动 a 中的设备 b:c 在同一阶段会得到相同的旧标识
// "a:b:c:download"。新标识必须让两者区分开，且把一方标识用于另一方的结果提交
// 一律返回 ErrOperationNotFound，不推进任何一方、不改影子与结果历史。
func TestOperationIDsDistinguishColonCampaignAndDevice(t *testing.T) {
	s := setupUpgrade(t, "c", "b:c")
	mk := func(campaignID string, devices []string, createdAt time.Time) CampaignSpec {
		return CampaignSpec{
			ID: campaignID, Operator: "alice", CreatedAt: createdAt,
			TargetVersion: "v2", Devices: devices, BatchSize: 1,
			WindowStart: upBase, WindowEnd: upBase.Add(time.Hour),
			Deadline: upBase.Add(2 * time.Hour),
		}
	}
	specAB := mk("a:b", []string{"c"}, upBase)
	specA := mk("a", []string{"b:c"}, upBase.Add(time.Second))
	bringOnline(t, s, "c")
	bringOnline(t, s, "b:c")
	createCampaign(t, s, specAB)
	createCampaign(t, s, specA)

	opC, err := s.Claim("a:b", "c", upBase.Add(time.Minute))
	if err != nil || opC == nil {
		t.Fatalf("claim c download: %v %+v", err, opC)
	}
	opBC, err := s.Claim("a", "b:c", upBase.Add(2*time.Minute))
	if err != nil || opBC == nil {
		t.Fatalf("claim b:c download: %v %+v", err, opBC)
	}
	// 修复前两者都是 "a:b:c:download"；现在必须互不相同，且都不同于旧标识。
	legacy := "a:b:c:download"
	if opC.ID == opBC.ID {
		t.Fatalf("operation ids collide: %q", opC.ID)
	}
	if opC.ID == legacy || opBC.ID == legacy {
		t.Fatalf("new id must not reuse ambiguous legacy form: %q / %q", opC.ID, opBC.ID)
	}
	if want := operationID("a:b", "c", StageDownload); opC.ID != want {
		t.Fatalf("c id = %q want %q", opC.ID, want)
	}
	if want := operationID("a", "b:c", StageDownload); opBC.ID != want {
		t.Fatalf("b:c id = %q want %q", opBC.ID, want)
	}
	// 各查询返回的标识一致地指向同一操作。
	if w, _ := s.GetDeviceWork("c"); w.Pending == nil || w.Pending.ID != opC.ID {
		t.Fatalf("c work id inconsistent: %+v", w.Pending)
	}
	if w, _ := s.GetDeviceWork("b:c"); w.Pending == nil || w.Pending.ID != opBC.ID {
		t.Fatalf("b:c work id inconsistent: %+v", w.Pending)
	}
	if d := findDevice(mustGetCampaign(s, "a:b"), "c"); d.DownloadID != opC.ID {
		t.Fatalf("a:b view id = %q want %q", d.DownloadID, opC.ID)
	}
	if d := findDevice(mustGetCampaign(s, "a"), "b:c"); d.DownloadID != opBC.ID {
		t.Fatalf("a view id = %q want %q", d.DownloadID, opBC.ID)
	}

	// 把 a:b/c 的已领取标识用于 a/b:c 的提交：未知操作，拒绝且什么都不改。
	cross := OperationResult{
		CampaignID: "a", DeviceID: "b:c", OperationID: opC.ID,
		At: upBase.Add(3 * time.Minute), Success: true,
	}
	if err := s.SubmitResult(cross); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("cross-campaign id must be not found: %v", err)
	}
	// 反向串用同样拒绝；旧的歧义标识本身也不再被任何一方接受。
	if err := s.SubmitResult(OperationResult{
		CampaignID: "a:b", DeviceID: "c", OperationID: opBC.ID,
		At: upBase.Add(3 * time.Minute), Success: true,
	}); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("reverse cross-campaign id must be not found: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: "a", DeviceID: "b:c", OperationID: legacy,
		At: upBase.Add(3 * time.Minute), Success: true,
	}); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("ambiguous legacy id must be not found on new campaign: %v", err)
	}

	// 拒绝不得改变设备影子、两边活动进度或结果历史。
	if v, _ := s.Get("b:c"); v.Version != "v1" {
		t.Fatalf("b:c shadow changed: %+v", v)
	}
	if v, _ := s.Get("c"); v.Version != "v1" {
		t.Fatalf("c shadow changed: %+v", v)
	}
	if d := findDevice(mustGetCampaign(s, "a"), "b:c"); d.Status != DeviceDownloading {
		t.Fatalf("b:c advanced on cross submit: %+v", d)
	}
	if d := findDevice(mustGetCampaign(s, "a:b"), "c"); d.Status != DeviceDownloading {
		t.Fatalf("c advanced on cross submit: %+v", d)
	}
	if cv := mustGetCampaign(s, "a"); len(cv.Results) != 0 {
		t.Fatalf("a history changed: %+v", cv.Results)
	}
	if cv := mustGetCampaign(s, "a:b"); len(cv.Results) != 0 {
		t.Fatalf("a:b history changed: %+v", cv.Results)
	}

	// 正确归属的操作照常按阶段规则接收；同一设备不同阶段标识不同，
	// 尚未领取的安装仍返回 ErrOperationNotClaimed（不因改标识跳过阶段）。
	if err := s.SubmitResult(OperationResult{
		CampaignID: "a", DeviceID: "b:c", OperationID: opBC.ID,
		At: upBase.Add(4 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("correctly owned result must succeed: %v", err)
	}
	d := findDevice(mustGetCampaign(s, "a"), "b:c")
	if d.Status != DeviceReady {
		t.Fatalf("b:c should be ready: %+v", d)
	}
	if d.DownloadID == d.InstallID {
		t.Fatalf("stage ids must differ: %q", d.DownloadID)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: "a", DeviceID: "b:c",
		OperationID: operationID("a", "b:c", StageInstall),
		At:          upBase.Add(5 * time.Minute), Success: true,
	}); !errors.Is(err, ErrOperationNotClaimed) {
		t.Fatalf("unclaimed install must be ErrOperationNotClaimed: %v", err)
	}
}

// TestOperationIDsAllowColonAndChineseIdentifiers 含冒号、中文等非空标识照常
// 登记设备、登记版本并创建活动；新标识在存储内唯一，三个阶段标识互不相同。
func TestOperationIDsAllowColonAndChineseIdentifiers(t *testing.T) {
	s := setupUpgrade(t, "设备:α", "c")
	spec := CampaignSpec{
		ID: "活动:1", Operator: "bob", CreatedAt: upBase,
		TargetVersion: "v2", Devices: []string{"设备:α"}, BatchSize: 1,
		WindowStart: upBase, WindowEnd: upBase.Add(time.Hour),
		Deadline:          upBase.Add(2 * time.Hour),
		RollbackOnFailure: true,
	}
	bringOnline(t, s, "设备:α")
	createCampaign(t, s, spec)
	dl, err := s.Claim(spec.ID, "设备:α", upBase.Add(time.Minute))
	if err != nil || dl == nil {
		t.Fatalf("claim with colon/cjk id: %v %+v", err, dl)
	}
	ids := map[string]bool{
		operationID(spec.ID, "设备:α", StageDownload): true,
		operationID(spec.ID, "设备:α", StageInstall):  true,
		operationID(spec.ID, "设备:α", StageRollback): true,
	}
	if len(ids) != 3 {
		t.Fatalf("stage ids not distinct: %+v", ids)
	}
	if !ids[dl.ID] {
		t.Fatalf("claim returned id not derived from exact id strings: %q", dl.ID)
	}
	// 中文为多字节字符：长度按字节计数，但标识仍能精确还原活动与设备。
	cid, did, stage, ok := parseOperationID(dl.ID)
	if !ok || cid != spec.ID || did != "设备:α" || stage != StageDownload {
		t.Fatalf("parseOperationID round-trip failed: %q -> %q %q %q %v", dl.ID, cid, did, stage, ok)
	}
}

// TestOperationIDsUniqueAcrossCampaigns 不同活动、不同设备的各阶段标识在整个
// 存储内互不相同。
func TestOperationIDsUniqueAcrossCampaigns(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2")
	mk := func(id string, created time.Time) CampaignSpec {
		return CampaignSpec{
			ID: id, Operator: "alice", CreatedAt: created, TargetVersion: "v2",
			Devices: []string{"d1"}, BatchSize: 1,
			WindowStart: upBase, WindowEnd: upBase.Add(time.Hour),
			Deadline: upBase.Add(2 * time.Hour),
		}
	}
	// 第二台设备放到第二个活动里，保证两个活动能并存。
	spec1 := mk("cmp-x", upBase)
	spec1.Devices = []string{"d1"}
	spec2 := mk("cmp-y", upBase.Add(time.Second))
	spec2.Devices = []string{"d2"}
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	createCampaign(t, s, spec1)
	createCampaign(t, s, spec2)
	seen := map[string]string{}
	add := func(id, who string) {
		if other, dup := seen[id]; dup {
			t.Fatalf("operation id %q reused by %s and %s", id, other, who)
		}
		seen[id] = who
	}
	for _, cid := range []string{"cmp-x", "cmp-y"} {
		dev := "d1"
		if cid == "cmp-y" {
			dev = "d2"
		}
		for _, stage := range []string{StageDownload, StageInstall, StageRollback} {
			add(operationID(cid, dev, stage), cid+"/"+dev+"/"+stage)
		}
	}
	if len(seen) != 6 {
		t.Fatalf("want 6 unique ids, got %d", len(seen))
	}
}

// TestNewOperationIDsStableAcrossReopen 修复后新活动的标识保存后再次打开保持不变，
// 且可继续用返回值提交结果。
func TestNewOperationIDsStableAcrossReopen(t *testing.T) {
	s, dir := setupUpgradeDir(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	if dl.ID != operationID(spec.ID, "d1", StageDownload) {
		t.Fatalf("unexpected new id: %q", dl.ID)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	d := findDevice(mustGetCampaign(s2, spec.ID), "d1")
	if d.DownloadID != dl.ID || d.InstallID != operationID(spec.ID, "d1", StageInstall) {
		t.Fatalf("ids changed after reopen: %+v", d)
	}
	if err := s2.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(3 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("submit with reopened id: %v", err)
	}
}

// legacyClaimedStore 构造修复前正常保存的活动：设备在线、下载已领取未完成、
// 安装尚未领取，操作标识为旧格式 c1:d1:*。
func writeLegacyClaimedStore(t *testing.T, dir string) {
	t.Helper()
	old := `{
 "format":1,
 "devices":{"d1":{"version":"v1","online":true,"revision":0,"desired":{},"reported":{},"lastSeq":1,"lastReportTime":"2026-10-02T11:59:00Z"}},
 "versions":{"v2":{"target":"v2","allowedFrom":["v1"]}},
 "campaigns":{"c1":{"id":"c1","operator":"alice","target":"v2","createdAt":"2026-10-02T12:00:00Z","batchSize":1,"windowStart":"2026-10-02T12:00:00Z","windowEnd":"2026-10-02T13:00:00Z","deadline":"2026-10-02T14:00:00Z","lastTime":"2026-10-02T12:02:00Z","status":"running","ended":false,"devices":[{"deviceId":"d1","batch":0,"status":"downloading","phase":"download","download":{"id":"c1:d1:download","claimed":true,"claimedAt":"2026-10-02T12:02:00Z"},"install":{"id":"c1:d1:install"}}]}}
}`
	if err := os.WriteFile(filepath.Join(dir, storeFileName), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLegacyCampaignKeepsOldIDsAfterReopen 修复前保存的未结束活动仍能打开，
// 已领取操作继续使用原（旧）标识领取与提交，重复提交判断与结果历史保持有效。
func TestLegacyCampaignKeepsOldIDsAfterReopen(t *testing.T) {
	dir := t.TempDir()
	writeLegacyClaimedStore(t, dir)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("legacy claimed store must open: %v", err)
	}
	defer s.Close()

	d := findDevice(mustGetCampaign(s, "c1"), "d1")
	if d.Status != DeviceDownloading || d.DownloadID != "c1:d1:download" ||
		d.InstallID != "c1:d1:install" {
		t.Fatalf("legacy ids not preserved: %+v", d)
	}
	// 再查仍返回原标识。
	op, err := s.Claim("c1", "d1", upBase.Add(3*time.Minute))
	if err != nil || op == nil || op.ID != "c1:d1:download" {
		t.Fatalf("re-claim legacy op: %v %+v", err, op)
	}
	if w, _ := s.GetDeviceWork("d1"); w.CampaignID != "c1" || w.Pending == nil ||
		w.Pending.ID != "c1:d1:download" {
		t.Fatalf("legacy work id: %+v", w.Pending)
	}
	// 用原标识完成下载：历史记录仍保留旧标识。
	if err := s.SubmitResult(OperationResult{
		CampaignID: "c1", DeviceID: "d1", OperationID: "c1:d1:download",
		At: upBase.Add(4 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("submit legacy download id: %v", err)
	}
	cv := mustGetCampaign(s, "c1")
	if len(cv.Results) != 1 || cv.Results[0].OperationID != "c1:d1:download" {
		t.Fatalf("history must keep legacy id: %+v", cv.Results)
	}
	// 重复提交仍幂等、不增加历史。
	if err := s.SubmitResult(OperationResult{
		CampaignID: "c1", DeviceID: "d1", OperationID: "c1:d1:download",
		At: upBase.Add(5 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("legacy replay: %v", err)
	}
	if len(mustGetCampaign(s, "c1").Results) != 1 {
		t.Fatal("legacy replay added history")
	}
	// 继续领取并完成安装，安装仍用旧标识。
	in, err := s.Claim("c1", "d1", upBase.Add(6*time.Minute))
	if err != nil || in == nil || in.ID != "c1:d1:install" {
		t.Fatalf("legacy install claim: %v %+v", err, in)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: "c1", DeviceID: "d1", OperationID: "c1:d1:install",
		At: upBase.Add(7 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("legacy install submit: %v", err)
	}
}

// TestLegacyCampaignAcceptsNewFormAlias 修复前保存的活动除了继续接受旧标识，
// 也接受按新格式为同一活动同一设备算出的等价标识；接受后历史仍保留旧标识。
func TestLegacyCampaignAcceptsNewFormAlias(t *testing.T) {
	dir := t.TempDir()
	writeLegacyClaimedStore(t, dir)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	alias := operationID("c1", "d1", StageDownload)
	if alias == "c1:d1:download" {
		t.Fatal("alias must differ from legacy id")
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: "c1", DeviceID: "d1", OperationID: alias,
		At: upBase.Add(4 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("new-form alias on legacy campaign must resolve: %v", err)
	}
	cv := mustGetCampaign(s, "c1")
	if len(cv.Results) != 1 || cv.Results[0].OperationID != "c1:d1:download" {
		t.Fatalf("history must retain stored legacy id: %+v", cv.Results)
	}
	// 旧标识的重复提交仍然幂等。
	if err := s.SubmitResult(OperationResult{
		CampaignID: "c1", DeviceID: "d1", OperationID: "c1:d1:download",
		At: upBase.Add(5 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("legacy id after alias: %v", err)
	}
}

// TestLegacyCampaignNewFormAliasRejectsOtherCampaignOrDevice 即便在旧活动上，
// 新格式标识也必须精确匹配活动与设备，不能借别名串用。
func TestLegacyCampaignNewFormAliasRejectsOtherCampaignOrDevice(t *testing.T) {
	dir := t.TempDir()
	writeLegacyClaimedStore(t, dir)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	submit := func(campaignID, deviceID, opID string) error {
		return s.SubmitResult(OperationResult{
			CampaignID: campaignID, DeviceID: deviceID, OperationID: opID,
			At: upBase.Add(4 * time.Minute), Success: true,
		})
	}
	if err := submit("c1", "d1", operationID("other", "d1", StageDownload)); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("alias naming another campaign: %v", err)
	}
	if err := submit("c1", "d1", operationID("c1", "other", StageDownload)); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("alias naming another device: %v", err)
	}
	if d := findDevice(mustGetCampaign(s, "c1"), "d1"); d.Status != DeviceDownloading {
		t.Fatalf("rejected alias changed state: %+v", d)
	}
}

// TestLegacyAmbiguousOperationIDsAcrossCampaignsRejected 两项修复前保存的活动
// 若因活动/设备标识含冒号而让同一旧操作标识指向两台不同设备（活动 a:b 的设备 c
// 与活动 a 的设备 b:c 共享 "a:b:c:download"），归属无法判定，必须按损坏拒绝打开，
// 而不能允许旧标识在两边串用。两项活动单独看都合法。
func TestLegacyAmbiguousOperationIDsAcrossCampaignsRejected(t *testing.T) {
	dir := t.TempDir()
	doc := `{
 "format":1,
 "devices":{
   "c":{"version":"v1","online":false,"revision":0,"desired":{},"reported":{},"lastSeq":0},
   "b:c":{"version":"v1","online":false,"revision":0,"desired":{},"reported":{},"lastSeq":0}
 },
 "versions":{"v2":{"target":"v2","allowedFrom":["v1"]}},
 "campaigns":{
   "a:b":{"id":"a:b","operator":"alice","target":"v2","createdAt":"2026-10-02T12:00:00Z","batchSize":1,"windowStart":"2026-10-02T12:00:00Z","windowEnd":"2026-10-02T13:00:00Z","deadline":"2026-10-02T14:00:00Z","lastTime":"2026-10-02T12:00:00Z","status":"running","ended":false,"devices":[{"deviceId":"c","batch":0,"status":"pending","phase":"download","download":{"id":"a:b:c:download"},"install":{"id":"a:b:c:install"}}]},
   "a":{"id":"a","operator":"alice","target":"v2","createdAt":"2026-10-02T11:00:00Z","batchSize":1,"windowStart":"2026-10-02T11:00:00Z","windowEnd":"2026-10-02T12:00:00Z","deadline":"2026-10-02T13:00:00Z","lastTime":"2026-10-02T11:00:00Z","status":"running","ended":false,"devices":[{"deviceId":"b:c","batch":0,"status":"pending","phase":"download","download":{"id":"a:b:c:download"},"install":{"id":"a:b:c:install"}}]}
 }}
}`
	if err := os.WriteFile(filepath.Join(dir, storeFileName), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err == nil {
		s.Close()
		t.Fatal("ambiguous legacy operation ids across two devices must be corrupt")
	}
	if !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("want ErrCorruptStorage, got %v", err)
	}
}

// TestMixedOperationIDFamiliesRejected 同一活动混用新旧标识家族属于保存矛盾，
// 仍必须返回 ErrCorruptStorage，不能借兼容旧标识放宽校验。
func TestMixedOperationIDFamiliesRejected(t *testing.T) {
	_, dir, _ := setupReady(t)
	original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
		// setupReady 由修复后代码写入（新标识）：把安装标识改成旧家族，制造混用。
		camp := campaignDoc(doc, "cmp-1")
		dev := camp["devices"].([]any)[0].(map[string]any)
		dev["install"].(map[string]any)["id"] = "cmp-1:d1:install"
	})
	assertReopenCorrupt(t, dir, original)
}

// TestLegacyHistoryOperationIDTamperedRejected 修复前活动的历史若使用了与其
// 保存标识不符的操作标识，仍按损坏拒绝。
func TestLegacyHistoryOperationIDTamperedRejected(t *testing.T) {
	dir := t.TempDir()
	writeLegacyClaimedStore(t, dir)
	// 先正常打开并用旧标识产生一条下载历史，再把历史标识改坏。
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: "c1", DeviceID: "d1", OperationID: "c1:d1:download",
		At: upBase.Add(4 * time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
		resultDocs(doc, "c1")[0]["operationId"] = operationID("c1", "d1", StageDownload)
	})
	assertReopenCorrupt(t, dir, original)
}
