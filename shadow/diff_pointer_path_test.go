package shadow

import (
	"encoding/json"
	"testing"
	"time"
)

// 本文件通过公开入口（Register/UpdateDesired/Report 写入，Diff/Get/Audit 查询）
// 回归同一份配置里“名字相似、实际位置不同”的字段同时存在时的差异行为：
// 字段名里的斜杠不是对象层级（a/b 是平铺字段，与 a 对象下的 b 互不相干），
// 按 RFC 6901 转义后分别出现在 /a~1b 与 /a/b；名字里看起来像已转义文本的
// （a~1b）仍是原始名字，要再转义一次；波浪号与斜杠同时出现时保留完整原意；
// 空字符串是合法字段名。这些位置的值、存在性与首次出现时间各自独立，
// 查询结果按最终输出路径字典序排列，且查询本身不修改任何已保存状态。

// 平铺字段 a/b 与 a 对象下的 b 同时与上报不一致时，必须分别出现在
// /a~1b 与 /a/b：两条独立差异，值与存在性各自对应真实位置，
// 不合并成一条，也不相互覆盖；普通字段的结果保持原样。
func TestDiffSlashNameVersusNestedPath(t *testing.T) {
	s, _ := openTemp(t)
	seedDeviceConfigs(t, s, "dev-both",
		`{"a/b":"flat","a":{"b":"nested"},"plain":1}`,
		`{"a/b":"flat-r","a":{"b":"nested-r"},"plain":2}`)

	entries, err := s.Diff("dev-both")
	if err != nil {
		t.Fatal(err)
	}
	assertLexicographic(t, entries)
	// 按最终输出路径字典序：/a/b < /a~1b < /plain。
	if len(entries) != 3 || entries[0].Path != "/a/b" || entries[1].Path != "/a~1b" || entries[2].Path != "/plain" {
		t.Fatalf("want /a/b, /a~1b, /plain, got %+v", entries)
	}
	m := diffByPath(entries)
	nested := m["/a/b"]
	if !nested.DesiredExists || !nested.ReportedExists {
		t.Fatalf("/a/b both sides must exist: %+v", nested)
	}
	if string(nested.Desired) != `"nested"` || string(nested.Reported) != `"nested-r"` {
		t.Fatalf("/a/b must hold the nested values: desired=%s reported=%s", nested.Desired, nested.Reported)
	}
	flat := m["/a~1b"]
	if !flat.DesiredExists || !flat.ReportedExists {
		t.Fatalf("/a~1b both sides must exist: %+v", flat)
	}
	if string(flat.Desired) != `"flat"` || string(flat.Reported) != `"flat-r"` {
		t.Fatalf("/a~1b must hold the flat-field values: desired=%s reported=%s", flat.Desired, flat.Reported)
	}
	if string(m["/plain"].Desired) != "1" || string(m["/plain"].Reported) != "2" {
		t.Fatalf("plain field result changed: %+v", m["/plain"])
	}

	// 反向交叉：期望侧只有平铺 a/b，上报侧只有 a 对象下的 b。
	// 平铺字段在 /a~1b 单独列出；上报侧整个 a 对象只在 /a 列一条整体差异，
	// 不得因字段名含斜杠而虚构出 /a/b 子路径。
	seedDeviceConfigs(t, s, "dev-cross", `{"a/b":"x"}`, `{"a":{"b":"y"}}`)
	entries, err = s.Diff("dev-cross")
	if err != nil {
		t.Fatal(err)
	}
	assertLexicographic(t, entries)
	if len(entries) != 2 || entries[0].Path != "/a" || entries[1].Path != "/a~1b" {
		t.Fatalf("want whole-object /a and flat /a~1b, got %+v", entries)
	}
	m = diffByPath(entries)
	if _, ok := m["/a/b"]; ok {
		t.Fatalf("must not fabricate subpath /a/b for a missing whole object: %+v", entries)
	}
	whole := m["/a"]
	if whole.DesiredExists || !whole.ReportedExists {
		t.Fatalf("/a must be reported-only: %+v", whole)
	}
	if whole.Desired != nil || string(whole.Reported) != `{"b":"y"}` {
		t.Fatalf("/a values: desired=%s reported=%s", whole.Desired, whole.Reported)
	}
	flatOnly := m["/a~1b"]
	if !flatOnly.DesiredExists || flatOnly.ReportedExists {
		t.Fatalf("/a~1b must be desired-only: %+v", flatOnly)
	}
	if string(flatOnly.Desired) != `"x"` || flatOnly.Reported != nil {
		t.Fatalf("/a~1b values: desired=%s reported=%s", flatOnly.Desired, flatOnly.Reported)
	}
}

// 字面字段 a~1b 与 a/b 同时存在时仍要区分：名字看起来已经转义过也不能
// 共用路径，a~1b 要再转义为 /a~01b；字段名同时含波浪号和斜杠时按
// RFC 6901 完整转义（a~/b → /a~0~1b），原意不丢失。
func TestDiffLiteralEscapedLookingNames(t *testing.T) {
	s, _ := openTemp(t)
	desired := `{"a~1b":"tilde","a/b":"slash","a~/b":"both"}`
	reported := `{"a~1b":"tilde-r","a/b":"slash-r","a~/b":"both-r"}`
	seedDeviceConfigs(t, s, "dev-escaped", desired, reported)

	entries, err := s.Diff("dev-escaped")
	if err != nil {
		t.Fatal(err)
	}
	assertLexicographic(t, entries)
	// 三条独立路径，按转义后文本排序：/a~01b < /a~0~1b < /a~1b。
	want := []struct {
		path     string
		desired  string
		reported string
	}{
		{"/a~01b", `"tilde"`, `"tilde-r"`},
		{"/a~0~1b", `"both"`, `"both-r"`},
		{"/a~1b", `"slash"`, `"slash-r"`},
	}
	if len(entries) != len(want) {
		t.Fatalf("want %d distinct paths, got %+v", len(want), entries)
	}
	for i, w := range want {
		if entries[i].Path != w.path {
			t.Fatalf("entry %d: want %s, got %s (all: %+v)", i, w.path, entries[i].Path, entries)
		}
		if !entries[i].DesiredExists || !entries[i].ReportedExists {
			t.Fatalf("%s both sides must exist: %+v", w.path, entries[i])
		}
		if string(entries[i].Desired) != w.desired || string(entries[i].Reported) != w.reported {
			t.Fatalf("%s values crossed with a lookalike field: desired=%s reported=%s",
				w.path, entries[i].Desired, entries[i].Reported)
		}
	}

	// 合法字段名不被改写：查询返回的双方配置保留原始字段名。
	v, err := s.Get("dev-escaped")
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Desired) != desired || string(v.Reported) != reported {
		t.Fatalf("stored configs must keep raw field names: %s / %s", v.Desired, v.Reported)
	}
}

// 空字符串是合法字段名：顶层与嵌套对象中都要准确定位为路径 "/" 与 "/o/"，
// 不能当成整个配置（空路径）或忽略；一侧缺失时正确指出缺失侧；
// 含空名字段的整个对象缺失时仍只列该对象位置的一条整体差异。
func TestDiffEmptyFieldName(t *testing.T) {
	s, _ := openTemp(t)

	// 顶层空名字段值不同：差异在 "/"，双方都存在。
	seedDeviceConfigs(t, s, "dev-empty-top", `{"":"top","keep":1}`, `{"":"top-r","keep":1}`)
	entries, err := s.Diff("dev-empty-top")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "/" {
		t.Fatalf("top-level empty name must diff at \"/\", got %+v", entries)
	}
	e := entries[0]
	if !e.DesiredExists || !e.ReportedExists {
		t.Fatalf("empty-name field exists on both sides: %+v", e)
	}
	if string(e.Desired) != `"top"` || string(e.Reported) != `"top-r"` {
		t.Fatalf("empty-name values: desired=%s reported=%s", e.Desired, e.Reported)
	}

	// 嵌套对象里的空名字段：差异在 "/o/"。
	seedDeviceConfigs(t, s, "dev-empty-nested", `{"o":{"":"nested"},"z":1}`, `{"o":{"":"nested-r"},"z":1}`)
	entries, err = s.Diff("dev-empty-nested")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "/o/" {
		t.Fatalf("nested empty name must diff at \"/o/\", got %+v", entries)
	}
	if string(entries[0].Desired) != `"nested"` || string(entries[0].Reported) != `"nested-r"` {
		t.Fatalf("nested empty-name values: %+v", entries[0])
	}

	// 空名字段只在期望侧存在，同时含空名字段的对象 o 整体缺失：
	// "/" 单独列出，o 只在 "/o" 列一条整体差异，不虚构 "/o/" 子路径。
	seedDeviceConfigs(t, s, "dev-empty-missing", `{"":"top","o":{"":1}}`, `{}`)
	entries, err = s.Diff("dev-empty-missing")
	if err != nil {
		t.Fatal(err)
	}
	assertLexicographic(t, entries)
	if len(entries) != 2 || entries[0].Path != "/" || entries[1].Path != "/o" {
		t.Fatalf("want \"/\" and whole-object \"/o\", got %+v", entries)
	}
	m := diffByPath(entries)
	if _, ok := m["/o/"]; ok {
		t.Fatalf("must not fabricate \"/o/\" for a missing whole object: %+v", entries)
	}
	top := m["/"]
	if !top.DesiredExists || top.ReportedExists || string(top.Desired) != `"top"` || top.Reported != nil {
		t.Fatalf("top-level empty name must be desired-only: %+v", top)
	}
	whole := m["/o"]
	if !whole.DesiredExists || whole.ReportedExists || string(whole.Desired) != `{"":1}` || whole.Reported != nil {
		t.Fatalf("whole object /o must be desired-only: %+v", whole)
	}
}

// 查询结果按最终输出路径（转义后）的字典序排列，而不是原始字段名字典序，
// 也与字段写入次序无关。
func TestDiffSortsByEscapedPath(t *testing.T) {
	s, _ := openTemp(t)
	// 原始字段名字典序为 a < a/b < ab < z~y；转义后路径字典序中
	// /ab 要排在 /a~1b 之前（'b' < '~'）。两侧字段次序刻意不同。
	desired := `{"z~y":1,"ab":1,"a/b":1,"a":{"b":1}}`
	reported := `{"a":{"b":2},"a/b":2,"ab":2,"z~y":2}`
	seedDeviceConfigs(t, s, "dev-order", desired, reported)

	entries, err := s.Diff("dev-order")
	if err != nil {
		t.Fatal(err)
	}
	assertLexicographic(t, entries)
	wantPaths := []string{"/a/b", "/ab", "/a~1b", "/z~0y"}
	if len(entries) != len(wantPaths) {
		t.Fatalf("want %d diffs, got %+v", len(wantPaths), entries)
	}
	for i, p := range wantPaths {
		if entries[i].Path != p {
			t.Fatalf("entry %d: want %s, got %s (all: %+v)", i, p, entries[i].Path, entries)
		}
		if string(entries[i].Desired) != "1" || string(entries[i].Reported) != "2" {
			t.Fatalf("%s values: %+v", p, entries[i])
		}
	}
}

// 同一台设备连续修改配置时，/a~1b（平铺 a/b）与 /a/b（a 对象下的 b）
// 各自独立计时：不同时刻产生的差异分别记录首次出现时间；一处恢复一致后
// 从结果中消失，另一处保留原时间与对应的值；已消失的位置再次不一致时
// 从这次产生差异的时刻重新计时，不继承名字相似的另一处记录；持续存在的
// 位置值再变化也不重置时间。关闭重开后计时记录保持不变。
func TestDiffLookalikePathsIndependentTiming(t *testing.T) {
	s, dir := openTemp(t)
	const id = "dev-timing"
	mustRegister(t, s, id, "1.0")

	// t1：写入期望配置。上报侧还是空对象：平铺字段在 /a~1b 单独列出，
	// 整个 a 对象只在 /a 列一条整体差异，两处都从 t1 计时。
	t1 := base
	if _, err := s.UpdateDesired(id, "op", t1, 0, json.RawMessage(`{"a/b":"d1","a":{"b":"n1"}}`)); err != nil {
		t.Fatal(err)
	}
	entries, err := s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Path != "/a" || entries[1].Path != "/a~1b" {
		t.Fatalf("initial diffs: %+v", entries)
	}
	m := diffByPath(entries)
	if string(m["/a"].Desired) != `{"b":"n1"}` || !m["/a"].Since.Equal(t1) {
		t.Fatalf("/a whole-object entry: %+v", m["/a"])
	}
	if string(m["/a~1b"].Desired) != `"d1"` || !m["/a~1b"].Since.Equal(t1) {
		t.Fatalf("/a~1b entry: %+v", m["/a~1b"])
	}

	// t2：上报使平铺字段一致、嵌套字段不一致。/a~1b 与 /a 消失，
	// 新出现的 /a/b 从 t2 计时。
	t2 := base.Add(time.Minute)
	if err := s.Report(id, 1, t2, "1.0", json.RawMessage(`{"a/b":"d1","a":{"b":"nX"}}`)); err != nil {
		t.Fatal(err)
	}
	entries, err = s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "/a/b" {
		t.Fatalf("after report 1 want only /a/b, got %+v", entries)
	}
	if !entries[0].Since.Equal(t2) {
		t.Fatalf("/a/b must start at %v, got %v", t2, entries[0].Since)
	}
	if string(entries[0].Desired) != `"n1"` || string(entries[0].Reported) != `"nX"` {
		t.Fatalf("/a/b values: %+v", entries[0])
	}

	// t3：上报使嵌套字段一致、平铺字段再次不一致。/a/b 消失；
	// /a~1b 重新出现，必须从 t3 重新计时——不能沿用自己在 t1 的旧记录，
	// 也不能继承名字相似的 /a/b 的 t2。
	t3 := base.Add(2 * time.Minute)
	if err := s.Report(id, 2, t3, "1.0", json.RawMessage(`{"a/b":"dX","a":{"b":"n1"}}`)); err != nil {
		t.Fatal(err)
	}
	entries, err = s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "/a~1b" {
		t.Fatalf("after report 2 want only /a~1b, got %+v", entries)
	}
	if !entries[0].Since.Equal(t3) {
		t.Fatalf("reappeared /a~1b must restart at %v, got %v", t3, entries[0].Since)
	}
	if string(entries[0].Desired) != `"d1"` || string(entries[0].Reported) != `"dX"` {
		t.Fatalf("/a~1b values: %+v", entries[0])
	}

	// t4：上报使两处同时不一致。/a~1b 一直未一致，保留 t3；
	// /a/b 是本轮新出现的差异，从 t4 计时。两处时间各自独立。
	t4 := base.Add(3 * time.Minute)
	if err := s.Report(id, 3, t4, "1.0", json.RawMessage(`{"a/b":"dY","a":{"b":"nY"}}`)); err != nil {
		t.Fatal(err)
	}
	entries, err = s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertLexicographic(t, entries)
	if len(entries) != 2 || entries[0].Path != "/a/b" || entries[1].Path != "/a~1b" {
		t.Fatalf("after report 3 want /a/b and /a~1b, got %+v", entries)
	}
	m = diffByPath(entries)
	if !m["/a/b"].Since.Equal(t4) {
		t.Fatalf("/a/b must start at %v, got %v", t4, m["/a/b"].Since)
	}
	if !m["/a~1b"].Since.Equal(t3) {
		t.Fatalf("persistent /a~1b must keep %v, got %v", t3, m["/a~1b"].Since)
	}

	// t5：修改期望配置使平铺字段恢复一致。/a~1b 从结果中消失；
	// /a/b 仍保留 t4 的原时间和对应的双方值。
	t5 := base.Add(4 * time.Minute)
	if _, err := s.UpdateDesired(id, "op", t5, 1, json.RawMessage(`{"a/b":"dY","a":{"b":"n1"}}`)); err != nil {
		t.Fatal(err)
	}
	entries, err = s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "/a/b" {
		t.Fatalf("after update want only /a/b, got %+v", entries)
	}
	if !entries[0].Since.Equal(t4) {
		t.Fatalf("/a/b must survive lookalike disappearing: want %v, got %v", t4, entries[0].Since)
	}
	if string(entries[0].Desired) != `"n1"` || string(entries[0].Reported) != `"nY"` {
		t.Fatalf("/a/b values after update: %+v", entries[0])
	}

	// t6：持续不一致的 /a/b 仅值再变化（双方仍未一致），首次出现时间不重置。
	t6 := base.Add(5 * time.Minute)
	if _, err := s.UpdateDesired(id, "op", t6, 2, json.RawMessage(`{"a/b":"dY","a":{"b":"n2"}}`)); err != nil {
		t.Fatal(err)
	}
	entries, err = s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "/a/b" || !entries[0].Since.Equal(t4) {
		t.Fatalf("persistent /a/b must keep %v through value change: %+v", t4, entries)
	}
	if string(entries[0].Desired) != `"n2"` || string(entries[0].Reported) != `"nY"` {
		t.Fatalf("/a/b values after second update: %+v", entries[0])
	}

	// 关闭重开：易混淆路径的计时记录原样恢复。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	entries, err = s2.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "/a/b" || !entries[0].Since.Equal(t4) {
		t.Fatalf("lookalike diff timing must survive reopen: %+v", entries)
	}
}

// 一侧缺少这样的字段时，结果要正确指出哪一侧不存在，并与显式 null 区分：
// 缺失侧没有值（Exists 为假、值为 nil），显式 null 是双方都存在、
// 该侧值为 null。
func TestDiffLookalikeMissingSideVersusNull(t *testing.T) {
	s, _ := openTemp(t)

	// 期望侧两处都是显式 null，上报侧两处都有值：两条差异双方都存在，
	// 期望侧值是 null 而不是“不存在”。
	seedDeviceConfigs(t, s, "dev-null", `{"a/b":null,"a":{"b":null}}`, `{"a/b":"v","a":{"b":"v"}}`)
	entries, err := s.Diff("dev-null")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Path != "/a/b" || entries[1].Path != "/a~1b" {
		t.Fatalf("want /a/b and /a~1b, got %+v", entries)
	}
	for _, e := range entries {
		if !e.DesiredExists || !e.ReportedExists {
			t.Fatalf("explicit null must count as existing on both sides: %+v", e)
		}
		if string(e.Desired) != "null" || string(e.Reported) != `"v"` {
			t.Fatalf("%s values: desired=%s reported=%s", e.Path, e.Desired, e.Reported)
		}
	}

	// 上报侧完全没有这两个位置：平铺字段在 /a~1b 指出上报侧不存在，
	// 整个 a 对象在 /a 列一条整体差异；缺失侧没有值。
	seedDeviceConfigs(t, s, "dev-missing", `{"a/b":"v","a":{"b":"v"}}`, `{}`)
	entries, err = s.Diff("dev-missing")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Path != "/a" || entries[1].Path != "/a~1b" {
		t.Fatalf("want whole-object /a and flat /a~1b, got %+v", entries)
	}
	for _, e := range entries {
		if !e.DesiredExists || e.ReportedExists {
			t.Fatalf("%s must be desired-only: %+v", e.Path, e)
		}
		if e.Reported != nil {
			t.Fatalf("%s missing side must have no value, got %s", e.Path, e.Reported)
		}
	}
	m := diffByPath(entries)
	if string(m["/a"].Desired) != `{"b":"v"}` || string(m["/a~1b"].Desired) != `"v"` {
		t.Fatalf("desired values: /a=%s /a~1b=%s", m["/a"].Desired, m["/a~1b"].Desired)
	}

	// 平铺字段只在上报侧存在：/a~1b 指出期望侧不存在，期望侧没有值。
	seedDeviceConfigs(t, s, "dev-reported-only", `{"a":{"b":"v"}}`, `{"a":{"b":"v"},"a/b":"r"}`)
	entries, err = s.Diff("dev-reported-only")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "/a~1b" {
		t.Fatalf("want only /a~1b, got %+v", entries)
	}
	e := entries[0]
	if e.DesiredExists || !e.ReportedExists {
		t.Fatalf("/a~1b must be reported-only: %+v", e)
	}
	if e.Desired != nil || string(e.Reported) != `"r"` {
		t.Fatalf("/a~1b values: desired=%s reported=%s", e.Desired, e.Reported)
	}
}

// 连续查询本身不得修改双方配置、期望修订号或审计，也不得刷新差异时间；
// 篡改查询返回的数据不影响存储与后续查询。
func TestDiffLookalikeQueriesAreReadOnly(t *testing.T) {
	s, _ := openTemp(t)
	const id = "dev-readonly"
	desiredCfg := `{"a/b":"flat","a":{"b":"nested"},"plain":1}`
	reportedCfg := `{"a/b":"flat-r","a":{"b":"nested-r"},"plain":2}`
	seedDeviceConfigs(t, s, id, desiredCfg, reportedCfg)

	before, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 3 {
		t.Fatalf("precondition: want 3 diffs, got %+v", first)
	}
	recsBefore, err := s.Audit(id)
	if err != nil {
		t.Fatal(err)
	}

	// 反复查询：结果逐字一致，差异时间不刷新。
	for i := 0; i < 3; i++ {
		again, err := s.Diff(id)
		if err != nil {
			t.Fatal(err)
		}
		if len(again) != len(first) {
			t.Fatalf("diff count changed by querying: %+v", again)
		}
		for j := range again {
			if again[j].Path != first[j].Path || !again[j].Since.Equal(first[j].Since) {
				t.Fatalf("diff %d changed by querying: %+v vs %+v", j, again[j], first[j])
			}
		}
	}

	// 篡改查询返回值不影响存储。
	first[0].Desired[0] = 'X'
	first[1].Reported[0] = 'X'
	first[2].Path = "/forged"

	after, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || after.LastSeq != before.LastSeq {
		t.Fatalf("revision/seq changed by queries: before %+v after %+v", before, after)
	}
	if string(after.Desired) != desiredCfg || string(after.Reported) != reportedCfg {
		t.Fatalf("configs rewritten by queries: %s / %s", after.Desired, after.Reported)
	}
	recsAfter, err := s.Audit(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(recsAfter) != len(recsBefore) {
		t.Fatalf("audit grew from queries: %+v", recsAfter)
	}
	if len(recsAfter) != 1 || recsAfter[0].Revision != 1 || string(recsAfter[0].After) != desiredCfg {
		t.Fatalf("audit altered by queries: %+v", recsAfter)
	}

	final, err := s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != 3 || final[0].Path != "/a/b" || final[1].Path != "/a~1b" || final[2].Path != "/plain" {
		t.Fatalf("diff paths mutated by caller: %+v", final)
	}
	if string(final[0].Desired) != `"nested"` || string(final[1].Reported) != `"flat-r"` {
		t.Fatalf("diff values mutated by caller: %+v", final)
	}
}
