package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// 本文件通过公开入口（Register/UpdateDesired/Report 写入，Diff/Get/Audit 观察）
// 回归同一字段在“对象”与“普通值”之间连续切换时的差异行为：
// 双方均为对象时逐层比较子路径；任一侧变成标量后只在父路径列一条整体差异；
// 父子路径互不相干——子路径消失后再次出现必须重新计时，既不能沿用父路径时间，
// 也不能找回更早一轮子路径的时间；同一路径仅改变不相等的值时保留本轮首次时间；
// 全程持续存在的另一处差异保留自己的原时间。过旧序号被拒绝时不得引发父子切换。

// diffByPath 把按字典序返回的差异列表转成按路径索引，便于逐路径核对。
func diffByPath(entries []DiffEntry) map[string]DiffEntry {
	out := make(map[string]DiffEntry, len(entries))
	for _, e := range entries {
		out[e.Path] = e
	}
	return out
}

// assertLexicographic 断言差异严格按 JSON Pointer 字典序排列。
func assertLexicographic(t *testing.T, entries []DiffEntry) {
	t.Helper()
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Path >= entries[i].Path {
			t.Fatalf("paths not strictly lexicographic: %q then %q", entries[i-1].Path, entries[i].Path)
		}
	}
}

func TestDiffFieldSwitchingBetweenObjectAndScalar(t *testing.T) {
	s, _ := openTemp(t)
	const id = "dev-type-switch"
	mustRegister(t, s, id, "1.0")

	// 期望侧 wifi 是含 ssid 与 channel 的对象；/level 与上报侧始终不同，
	// 作为全程持续存在的另一处差异，其首次出现时间必须始终不被 wifi 变化重置。
	tDesired := base
	desiredCfg := json.RawMessage(`{"wifi":{"ssid":"a","channel":6},"level":1}`)
	rev, err := s.UpdateDesired(id, "op", tDesired, 0, desiredCfg)
	if err != nil || rev != 1 {
		t.Fatalf("UpdateDesired: rev=%d err=%v", rev, err)
	}

	// 第一次上报：wifi 结构与期望相同，仅 ssid 不同。
	t1 := base.Add(time.Minute)
	r1 := json.RawMessage(`{"wifi":{"ssid":"b","channel":6},"level":2}`)
	if err := s.Report(id, 1, t1, "1.0", r1); err != nil {
		t.Fatalf("report seq 1: %v", err)
	}
	entries, err := s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertLexicographic(t, entries)
	// 期望侧字段在前：/level < /wifi/ssid；不应有 /wifi 父路径差异。
	if len(entries) != 2 || entries[0].Path != "/level" || entries[1].Path != "/wifi/ssid" {
		t.Fatalf("after report 1 want /level and /wifi/ssid, got %+v", entries)
	}
	m := diffByPath(entries)
	ssid := m["/wifi/ssid"]
	if !ssid.DesiredExists || !ssid.ReportedExists {
		t.Fatalf("/wifi/ssid both sides must exist: %+v", ssid)
	}
	if string(ssid.Desired) != `"a"` || string(ssid.Reported) != `"b"` {
		t.Fatalf("/wifi/ssid values: desired=%s reported=%s", ssid.Desired, ssid.Reported)
	}
	// 子路径差异首次出现于本次上报，不能沿用上报前“期望有、上报无”的 /wifi 时间。
	if !ssid.Since.Equal(t1) {
		t.Fatalf("/wifi/ssid since: want %v, got %v", t1, ssid.Since)
	}
	// 持续差异 /level 在写入期望时（上报侧还是空对象）即已存在。
	if lv := m["/level"]; !lv.Since.Equal(tDesired) {
		t.Fatalf("/level since: want %v, got %v", tDesired, lv.Since)
	}

	// 第二次上报：wifi 整体变成字符串，期望侧仍是对象。
	t2 := base.Add(2 * time.Minute)
	r2 := json.RawMessage(`{"wifi":"off","level":2}`)
	if err := s.Report(id, 2, t2, "1.0", r2); err != nil {
		t.Fatalf("report seq 2: %v", err)
	}
	entries, err = s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertLexicographic(t, entries)
	if len(entries) != 2 || entries[0].Path != "/level" || entries[1].Path != "/wifi" {
		t.Fatalf("after report 2 want /level and whole /wifi, got %+v", entries)
	}
	m = diffByPath(entries)
	if _, ok := m["/wifi/ssid"]; ok {
		t.Fatalf("child path /wifi/ssid must disappear when types diverge: %+v", entries)
	}
	w := m["/wifi"]
	// 父路径是一条整体差异：双方都存在，分别返回期望对象与上报字符串。
	if !w.DesiredExists || !w.ReportedExists {
		t.Fatalf("/wifi both sides must exist: %+v", w)
	}
	if string(w.Desired) != `{"ssid":"a","channel":6}` || string(w.Reported) != `"off"` {
		t.Fatalf("/wifi values: desired=%s reported=%s", w.Desired, w.Reported)
	}
	// 结构变化产生的是一条全新父路径差异，从 t2 重新计时，
	// 不能沿用原子路径 /wifi/ssid 的 t1。
	if !w.Since.Equal(t2) {
		t.Fatalf("/wifi since: want %v, got %v", t2, w.Since)
	}
	if lv := m["/level"]; !lv.Since.Equal(tDesired) {
		t.Fatalf("/level time must survive wifi type change: want %v, got %v", tDesired, lv.Since)
	}

	// 第三次上报：wifi 恢复为对象，channel 与期望相同，ssid 仍不同。
	t3 := base.Add(3 * time.Minute)
	r3 := json.RawMessage(`{"wifi":{"ssid":"c","channel":6},"level":2}`)
	if err := s.Report(id, 3, t3, "1.0", r3); err != nil {
		t.Fatalf("report seq 3: %v", err)
	}
	entries, err = s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertLexicographic(t, entries)
	if len(entries) != 2 || entries[0].Path != "/level" || entries[1].Path != "/wifi/ssid" {
		t.Fatalf("after report 3 want /level and /wifi/ssid, got %+v", entries)
	}
	m = diffByPath(entries)
	if _, ok := m["/wifi"]; ok {
		t.Fatalf("parent path /wifi must disappear after object report: %+v", entries)
	}
	ssid = m["/wifi/ssid"]
	if !ssid.DesiredExists || !ssid.ReportedExists {
		t.Fatalf("/wifi/ssid both sides must exist: %+v", ssid)
	}
	if string(ssid.Desired) != `"a"` || string(ssid.Reported) != `"c"` {
		t.Fatalf("/wifi/ssid values: desired=%s reported=%s", ssid.Desired, ssid.Reported)
	}
	// 子路径虽在 t1 出现过，但中途随标量上报消失过；恢复后必须按本轮 t3 重新计时：
	// 不能继承父路径的 t2，也不能找回最早那次子路径时间 t1。
	if !ssid.Since.Equal(t3) {
		t.Fatalf("/wifi/ssid reappeared must restart at %v, got %v", t3, ssid.Since)
	}
	if lv := m["/level"]; !lv.Since.Equal(tDesired) {
		t.Fatalf("/level time must survive restore to object: want %v, got %v", tDesired, lv.Since)
	}

	// 第四次上报：同一路径仅把不相等的值再改一次，保留本轮（t3）首次出现时间。
	t4 := base.Add(4 * time.Minute)
	r4 := json.RawMessage(`{"wifi":{"ssid":"d","channel":6},"level":2}`)
	if err := s.Report(id, 4, t4, "1.0", r4); err != nil {
		t.Fatalf("report seq 4: %v", err)
	}
	entries, err = s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertLexicographic(t, entries)
	if len(entries) != 2 || entries[0].Path != "/level" || entries[1].Path != "/wifi/ssid" {
		t.Fatalf("after report 4 want /level and /wifi/ssid, got %+v", entries)
	}
	m = diffByPath(entries)
	ssid = m["/wifi/ssid"]
	if string(ssid.Desired) != `"a"` || string(ssid.Reported) != `"d"` {
		t.Fatalf("/wifi/ssid values: desired=%s reported=%s", ssid.Desired, ssid.Reported)
	}
	if !ssid.Since.Equal(t3) {
		t.Fatalf("/wifi/ssid must keep first-seen %v of this round, got %v", t3, ssid.Since)
	}
	if lv := m["/level"]; !lv.Since.Equal(tDesired) {
		t.Fatalf("/level time changed by unrelated value change: want %v, got %v", tDesired, lv.Since)
	}

	// 过旧序号（2 < 最近已接受的 4），内容试图再次把 wifi 切成字符串：
	// 必须返回 ErrStaleSequence，且已接受的配置、序号与全部差异时间原样保留，
	// 这次拒绝不能造成父子路径切换。
	tStale := base.Add(5 * time.Minute)
	err = s.Report(id, 2, tStale, "1.0", json.RawMessage(`{"wifi":"off","level":2}`))
	if !errors.Is(err, ErrStaleSequence) {
		t.Fatalf("stale seq: want ErrStaleSequence, got %v", err)
	}
	v, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if v.LastSeq != 4 {
		t.Fatalf("LastSeq changed by rejected report: %d", v.LastSeq)
	}
	if string(v.Reported) != string(r4) {
		t.Fatalf("accepted reported config altered by rejected report: %s", v.Reported)
	}
	if string(v.Desired) != string(desiredCfg) {
		t.Fatalf("desired config altered: %s", v.Desired)
	}
	if v.Version != "1.0" {
		t.Fatalf("version altered by rejected report: %s", v.Version)
	}
	entries, err = s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertLexicographic(t, entries)
	if len(entries) != 2 || entries[0].Path != "/level" || entries[1].Path != "/wifi/ssid" {
		t.Fatalf("stale report must not switch parent/child paths: %+v", entries)
	}
	m = diffByPath(entries)
	if ssid := m["/wifi/ssid"]; string(ssid.Reported) != `"d"` || !ssid.Since.Equal(t3) {
		t.Fatalf("/wifi/ssid after stale report: %+v", ssid)
	}
	if lv := m["/level"]; !lv.Since.Equal(tDesired) {
		t.Fatalf("/level time altered by stale report: want %v, got %v", tDesired, lv.Since)
	}

	// 全过程期望配置只设置过一次：修订号与审计记录保持原样。
	if v.Revision != 1 {
		t.Fatalf("revision changed: %d", v.Revision)
	}
	recs, err := s.Audit(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("audit must stay untouched by reports: %+v", recs)
	}
	if recs[0].Revision != 1 || recs[0].Operator != "op" || !recs[0].Time.Equal(tDesired) {
		t.Fatalf("audit record altered: %+v", recs[0])
	}
	if string(recs[0].Before) != "{}" || string(recs[0].After) != string(desiredCfg) {
		t.Fatalf("audit configs altered: before=%s after=%s", recs[0].Before, recs[0].After)
	}
}
