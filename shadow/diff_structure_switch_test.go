package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// 本文件通过公开入口（Register/UpdateDesired/Report 写入，Diff/Get/Audit 查询）
// 回归保障同一字段在对象与普通值之间反复切换时，差异路径与首次出现时间的
// 现有行为：
//   - 双方同为对象时逐层比较，只在实际不同的子路径列差异；
//   - 一侧变成普通值后，子路径差异消失，父路径列一条整体差异并重新计时；
//   - 恢复为对象后子路径差异重新出现，其首次出现时间按恢复这次上报重新计时，
//     不继承父路径时间，也不找回更早的同路径时间；
//   - 同一路径仅改变不相等的值时保留本轮首次出现时间；
//   - 其他路径的持续差异在整个过程中保留自己的首次出现时间；
//   - 过旧序号的上报被拒绝（ErrStaleSequence），不改变任何已接受状态。

// assertDiffs 核对当前差异集合：路径严格按字典序、每条路径的双方存在性、
// 双方原始 JSON 与首次出现时间都与期望一致。
func assertDiffs(t *testing.T, s *Store, id string, want []DiffEntry) {
	t.Helper()
	entries, err := s.Diff(id)
	if err != nil {
		t.Fatalf("Diff(%s): %v", id, err)
	}
	if len(entries) != len(want) {
		t.Fatalf("want %d diffs, got %+v", len(want), entries)
	}
	for i, w := range want {
		e := entries[i]
		if i > 0 && entries[i-1].Path >= e.Path {
			t.Fatalf("paths not strictly lexicographic: %q then %q", entries[i-1].Path, e.Path)
		}
		if e.Path != w.Path {
			t.Fatalf("entry %d path: want %s, got %s (all: %+v)", i, w.Path, e.Path, entries)
		}
		if e.DesiredExists != w.DesiredExists || e.ReportedExists != w.ReportedExists {
			t.Fatalf("%s existence flags: %+v", e.Path, e)
		}
		if string(e.Desired) != string(w.Desired) {
			t.Fatalf("%s desired: want %s, got %s", e.Path, w.Desired, e.Desired)
		}
		if string(e.Reported) != string(w.Reported) {
			t.Fatalf("%s reported: want %s, got %s", e.Path, w.Reported, e.Reported)
		}
		if !e.Since.Equal(w.Since) {
			t.Fatalf("%s since: want %s, got %s", e.Path, w.Since, e.Since)
		}
	}
}

// assertDesiredIntact 核对上报流程不触碰期望侧：期望配置原文、修订号与
// 审计记录保持 UpdateDesired 之后的样子。
func assertDesiredIntact(t *testing.T, s *Store, id, desiredCfg string, lastSeq uint64) {
	t.Helper()
	v, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Revision != 1 {
		t.Fatalf("revision changed by reports: %+v", v)
	}
	if string(v.Desired) != desiredCfg {
		t.Fatalf("desired config changed by reports: %s", v.Desired)
	}
	if v.LastSeq != lastSeq {
		t.Fatalf("last seq: want %d, got %d", lastSeq, v.LastSeq)
	}
	recs, err := s.Audit(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Revision != 1 ||
		string(recs[0].Before) != "{}" || string(recs[0].After) != desiredCfg {
		t.Fatalf("audit changed by reports: %+v", recs)
	}
}

// 同一字段 wifi 在对象与字符串之间来回切换：差异位置在子路径与父路径之间
// 切换，每次结构变化都按当次上报时间重新计时；另一处持续差异 /volume 的
// 首次出现时间全程不变。
func TestDiffStructureSwitchRetimesPaths(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-wifi", "1.0")

	desiredCfg := `{"volume":50,"wifi":{"ssid":"home","channel":6}}`
	if _, err := s.UpdateDesired("dev-wifi", "op", base, 0, json.RawMessage(desiredCfg)); err != nil {
		t.Fatalf("UpdateDesired: %v", err)
	}

	t1 := base.Add(time.Minute)
	t2 := base.Add(2 * time.Minute)
	t3 := base.Add(3 * time.Minute)
	t4 := base.Add(4 * time.Minute)

	// 第一次上报：wifi 同为对象，仅 ssid 不同；差异只落在 /wifi/ssid，
	// 双方值是对应的字段值，首次出现时间为本次上报时间。
	// /volume 是另一处持续差异：它在设置期望配置时（上报侧还是空对象）
	// 就已经出现，首次出现时间一直是 base，不随后续上报改变。
	if err := s.Report("dev-wifi", 1, t1, "1.0",
		json.RawMessage(`{"volume":40,"wifi":{"ssid":"cafe","channel":6}}`)); err != nil {
		t.Fatalf("Report seq 1: %v", err)
	}
	assertDiffs(t, s, "dev-wifi", []DiffEntry{
		{Path: "/volume", DesiredExists: true, ReportedExists: true,
			Desired: json.RawMessage("50"), Reported: json.RawMessage("40"), Since: base},
		{Path: "/wifi/ssid", DesiredExists: true, ReportedExists: true,
			Desired: json.RawMessage(`"home"`), Reported: json.RawMessage(`"cafe"`), Since: t1},
	})

	// 第二次上报：wifi 整体变成字符串。期望侧仍是对象，但不再下钻：
	// 只在 /wifi 列一条整体差异，双方都存在，分别返回期望对象与上报字符串；
	// 原 /wifi/ssid 差异消失，父路径从本次结构变化重新计时，不沿用子路径的 t1。
	if err := s.Report("dev-wifi", 2, t2, "1.0",
		json.RawMessage(`{"volume":40,"wifi":"open"}`)); err != nil {
		t.Fatalf("Report seq 2: %v", err)
	}
	assertDiffs(t, s, "dev-wifi", []DiffEntry{
		{Path: "/volume", DesiredExists: true, ReportedExists: true,
			Desired: json.RawMessage("50"), Reported: json.RawMessage("40"), Since: base},
		{Path: "/wifi", DesiredExists: true, ReportedExists: true,
			Desired: json.RawMessage(`{"ssid":"home","channel":6}`),
			Reported: json.RawMessage(`"open"`), Since: t2},
	})

	// 第三次上报：wifi 恢复为对象，channel 与期望相同、ssid 仍不同。
	// 差异重新落在 /wifi/ssid，父路径消失；子路径虽然曾经出现过，
	// 但中途已经消失，首次出现时间是本次恢复上报的 t3，
	// 既不继承父路径的 t2，也不找回最早的 t1。
	if err := s.Report("dev-wifi", 3, t3, "1.0",
		json.RawMessage(`{"volume":40,"wifi":{"ssid":"cafe","channel":6}}`)); err != nil {
		t.Fatalf("Report seq 3: %v", err)
	}
	assertDiffs(t, s, "dev-wifi", []DiffEntry{
		{Path: "/volume", DesiredExists: true, ReportedExists: true,
			Desired: json.RawMessage("50"), Reported: json.RawMessage("40"), Since: base},
		{Path: "/wifi/ssid", DesiredExists: true, ReportedExists: true,
			Desired: json.RawMessage(`"home"`), Reported: json.RawMessage(`"cafe"`), Since: t3},
	})

	// 第四次上报：同一路径仅改变不相等的具体值，保留本轮首次出现时间 t3。
	if err := s.Report("dev-wifi", 4, t4, "1.0",
		json.RawMessage(`{"volume":40,"wifi":{"ssid":"lab","channel":6}}`)); err != nil {
		t.Fatalf("Report seq 4: %v", err)
	}
	assertDiffs(t, s, "dev-wifi", []DiffEntry{
		{Path: "/volume", DesiredExists: true, ReportedExists: true,
			Desired: json.RawMessage("50"), Reported: json.RawMessage("40"), Since: base},
		{Path: "/wifi/ssid", DesiredExists: true, ReportedExists: true,
			Desired: json.RawMessage(`"home"`), Reported: json.RawMessage(`"lab"`), Since: t3},
	})

	// 整个上报过程中期望配置、修订号与修改审计保持原样。
	assertDesiredIntact(t, s, "dev-wifi", desiredCfg, 4)
}

// 序号小于最近已接受序号的上报返回 ErrStaleSequence：设备保留此前接受的
// 配置、序号与全部差异时间，被拒绝的内容（即使会让 wifi 从对象变回字符串）
// 不会造成父子路径切换。
func TestDiffStaleReportKeepsStructureAndTimes(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-stale", "1.0")

	desiredCfg := `{"volume":50,"wifi":{"ssid":"home","channel":6}}`
	if _, err := s.UpdateDesired("dev-stale", "op", base, 0, json.RawMessage(desiredCfg)); err != nil {
		t.Fatalf("UpdateDesired: %v", err)
	}

	t1 := base.Add(time.Minute)
	t2 := base.Add(2 * time.Minute)

	// 先建立子路径差异：wifi 同为对象、ssid 不同。
	if err := s.Report("dev-stale", 1, t1, "1.0",
		json.RawMessage(`{"volume":40,"wifi":{"ssid":"cafe","channel":6}}`)); err != nil {
		t.Fatalf("Report seq 1: %v", err)
	}
	// 再推进到父路径整体差异：wifi 上报为字符串。
	if err := s.Report("dev-stale", 2, t2, "1.0",
		json.RawMessage(`{"volume":40,"wifi":"open"}`)); err != nil {
		t.Fatalf("Report seq 2: %v", err)
	}
	want := []DiffEntry{
		{Path: "/volume", DesiredExists: true, ReportedExists: true,
			Desired: json.RawMessage("50"), Reported: json.RawMessage("40"), Since: base},
		{Path: "/wifi", DesiredExists: true, ReportedExists: true,
			Desired: json.RawMessage(`{"ssid":"home","channel":6}`),
			Reported: json.RawMessage(`"open"`), Since: t2},
	}
	assertDiffs(t, s, "dev-stale", want)

	// 过旧序号：内容若被接受会把 wifi 变回对象（触发父→子路径切换），
	// 必须被拒绝且不留任何痕迹。
	err := s.Report("dev-stale", 1, base.Add(3*time.Minute), "1.0",
		json.RawMessage(`{"volume":50,"wifi":{"ssid":"home","channel":6}}`))
	if !errors.Is(err, ErrStaleSequence) {
		t.Fatalf("stale report: want ErrStaleSequence, got %v", err)
	}

	// 已接受的上报配置与序号保持原样。
	v, err := s.Get("dev-stale")
	if err != nil {
		t.Fatal(err)
	}
	if v.LastSeq != 2 {
		t.Fatalf("last seq moved by stale report: %+v", v)
	}
	if string(v.Reported) != `{"volume":40,"wifi":"open"}` {
		t.Fatalf("reported config changed by stale report: %s", v.Reported)
	}
	// 差异位置与全部首次出现时间保持拒绝前的状态。
	assertDiffs(t, s, "dev-stale", want)
	// 期望侧同样不受影响。
	assertDesiredIntact(t, s, "dev-stale", desiredCfg, 2)
}
