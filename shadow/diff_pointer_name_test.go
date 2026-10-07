package shadow

import (
	"encoding/json"
	"testing"
	"time"
)

// 本文件通过公开入口（Register/UpdateDesired/Report 写入，Diff/Get/Audit
// 查询及重开）回归保障：同一份配置里名字相似但实际位置不同的字段同时存在时，
// JSON Pointer 路径、双方值、存在性与首次出现时间都必须精确对应到真实位置。
// 字段名里的斜杠只是字面字符而不是对象层级，名字里看起来像已转义文本的内容
// （如 a~1b）仍是原始名字，空字符串也是合法字段名；结果始终按最终输出路径
// （转义后）的字典序排列，而不是原始字段名或提交次序。

// entryExpectation 描述一条差异在路径、存在性、双方值与首次出现时间上的期望。
type entryExpectation struct {
	path           string
	desiredExists  bool
	reportedExists bool
	desired        string // 仅在 desiredExists 为 true 时比对
	reported       string // 仅在 reportedExists 为 true 时比对
	since          time.Time
}

func assertEntries(t *testing.T, got []DiffEntry, want []entryExpectation) {
	t.Helper()
	assertLexicographic(t, got)
	if len(got) != len(want) {
		t.Fatalf("diff count: want %d, got %+v", len(want), got)
	}
	for i, w := range want {
		e := got[i]
		if e.Path != w.path {
			t.Fatalf("entry %d path: want %s, got %s (all: %+v)", i, w.path, e.Path, got)
		}
		if e.DesiredExists != w.desiredExists || e.ReportedExists != w.reportedExists {
			t.Fatalf("%s existence flags: want d=%v r=%v, got %+v",
				w.path, w.desiredExists, w.reportedExists, e)
		}
		if e.DesiredExists {
			if string(e.Desired) != w.desired {
				t.Fatalf("%s desired value: want %s, got %s", w.path, w.desired, e.Desired)
			}
		} else if e.Desired != nil {
			t.Fatalf("%s missing desired side must carry no value, got %s", w.path, e.Desired)
		}
		if e.ReportedExists {
			if string(e.Reported) != w.reported {
				t.Fatalf("%s reported value: want %s, got %s", w.path, w.reported, e.Reported)
			}
		} else if e.Reported != nil {
			t.Fatalf("%s missing reported side must carry no value, got %s", w.path, e.Reported)
		}
		if !w.since.IsZero() && !e.Since.Equal(w.since) {
			t.Fatalf("%s first-seen: want %v, got %v", w.path, w.since, e.Since)
		}
	}
}

// 同一份配置同时包含字面字段 a/b 与对象 a 下的 b：两处必须分别出现在
// /a~1b 与 /a/b，双方值各自对应真实位置，不合并成一条也不相互覆盖；
// 一侧缺失按真实位置标记；结果按转义后的最终路径字典序（/a/b 在前）排列，
// 与字段提交次序无关。
func TestDiffSlashLiteralNameCoexistsWithObjectChild(t *testing.T) {
	s, _ := openTemp(t)
	const id = "dev-slash-coexist"
	// 提交时刻意让对象 a 出现在字面字段 "a/b" 之后，上报侧次序相反，
	// 证明排序与提交次序、原始字段名无关。
	desired := `{"a/b":"ld","a":{"b":"od"},"extra":1}`
	reported := `{"a":{"b":"or"},"a/b":"lr"}`
	seedDeviceConfigs(t, s, id, desired, reported)

	entries, err := s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertEntries(t, entries, []entryExpectation{
		{path: "/a/b", desiredExists: true, reportedExists: true, desired: `"od"`, reported: `"or"`},
		{path: "/a~1b", desiredExists: true, reportedExists: true, desired: `"ld"`, reported: `"lr"`},
		{path: "/extra", desiredExists: true, reportedExists: false, desired: "1"},
	})

	// 只让对象位置与期望一致（同时补齐 /extra）：/a/b 消失；字面字段的值
	// "lr" 仍与期望的 "ld" 不同，/a~1b 保留。
	if err := s.Report(id, 2, base.Add(2*time.Minute), "1.0",
		json.RawMessage(`{"a/b":"lr","a":{"b":"od"},"extra":1}`)); err != nil {
		t.Fatal(err)
	}
	entries, err = s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertEntries(t, entries, []entryExpectation{
		{path: "/a~1b", desiredExists: true, reportedExists: true, desired: `"ld"`, reported: `"lr"`},
	})
}

// 字面字段 a~1b（名字本身含波浪号与数字 1）、字面字段 a/b 与对象 a 下的 b
// 三者同时存在：转义结果分别是 /a~01b、/a~1b、/a/b，三条独立差异；
// 不能因为一个名字看起来“已经转义过”就与另一条共用路径。
// 同时覆盖：名字里同时含波浪号和斜杠（x~y/z 与 ~/）时保留完整原意；
// 空字符串字段名在顶层（路径 /）与嵌套对象中（路径 /o/）都准确定位，
// 既不被当成整份配置，也不被忽略。
func TestDiffEscapedLookingAndEmptyNamesStayDistinct(t *testing.T) {
	s, _ := openTemp(t)

	t.Run("literal a~1b vs literal a/b vs object child", func(t *testing.T) {
		const id = "dev-tilde-looking"
		desired := `{"a~1b":"T","a/b":"S","a":{"b":"C"},"keep":1}`
		reported := `{"a":{"b":"c"},"a/b":"s","a~1b":"t","keep":1}`
		seedDeviceConfigs(t, s, id, desired, reported)
		entries, err := s.Diff(id)
		if err != nil {
			t.Fatal(err)
		}
		// 字典序：'/'(0x2F) < '~'(0x7E)，同以 /a~ 开头时 '0' < '1'。
		assertEntries(t, entries, []entryExpectation{
			{path: "/a/b", desiredExists: true, reportedExists: true, desired: `"C"`, reported: `"c"`},
			{path: "/a~01b", desiredExists: true, reportedExists: true, desired: `"T"`, reported: `"t"`},
			{path: "/a~1b", desiredExists: true, reportedExists: true, desired: `"S"`, reported: `"s"`},
		})
	})

	t.Run("name containing both tilde and slash", func(t *testing.T) {
		const id = "dev-tilde-slash"
		// x~y/z 转义为 x~0y~1z；~/ 转义为 ~0~1。
		desired := `{"x~y/z":{"k":1},"~/":"d"}`
		reported := `{"x~y/z":{"k":2},"~/":"r"}`
		seedDeviceConfigs(t, s, id, desired, reported)
		entries, err := s.Diff(id)
		if err != nil {
			t.Fatal(err)
		}
		assertEntries(t, entries, []entryExpectation{
			{path: "/x~0y~1z/k", desiredExists: true, reportedExists: true, desired: "1", reported: "2"},
			{path: "/~0~1", desiredExists: true, reportedExists: true, desired: `"d"`, reported: `"r"`},
		})
	})

	t.Run("empty string key at top level and nested", func(t *testing.T) {
		const id = "dev-empty-name"
		desired := `{"":"de","z":1,"o":{"":"ne","keep":1}}`
		reported := `{"":"re","o":{"keep":1}}`
		seedDeviceConfigs(t, s, id, desired, reported)
		entries, err := s.Diff(id)
		if err != nil {
			t.Fatal(err)
		}
		// / 是其余路径的前缀，字典序最小；空名嵌套字段为 /o/。
		assertEntries(t, entries, []entryExpectation{
			{path: "/", desiredExists: true, reportedExists: true, desired: `"de"`, reported: `"re"`},
			{path: "/o/", desiredExists: true, reportedExists: false, desired: `"ne"`},
			{path: "/z", desiredExists: true, reportedExists: false, desired: "1"},
		})
	})
}

// 一侧缺少易混淆字段时，结果必须指出真实缺失的一侧，缺失侧不带任何值，
// 并与显式 null 区分；缺少整个对象时只在该对象位置列一条整体差异，
// 绝不能因为对象内字段名含斜杠而虚构出 /o/a~1b 之类的子路径。
func TestDiffMissingSideAndNullWithConfusableNames(t *testing.T) {
	s, _ := openTemp(t)
	const id = "dev-missing"
	// /a/b：上报侧独有（双方都有对象 a，期望侧缺少其中的 b）；
	// /a~1b：期望侧显式为 null、上报侧整个字段缺失——必须与“不存在”区分；
	// /o：期望侧独有整个对象，内部字面字段 a/b 不得被展开成子路径；
	// /p：上报侧独有整个对象，同理只能有 /p 一条。
	desired := `{"a":{},"a/b":null,"o":{"a/b":1,"c":2},"eq":1}`
	reported := `{"a":{"b":"child"},"p":{"a/b":9},"eq":1}`
	seedDeviceConfigs(t, s, id, desired, reported)

	entries, err := s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertEntries(t, entries, []entryExpectation{
		{path: "/a/b", desiredExists: false, reportedExists: true, reported: `"child"`},
		{path: "/a~1b", desiredExists: true, reportedExists: false, desired: "null"},
		{path: "/o", desiredExists: true, reportedExists: false, desired: `{"a/b":1,"c":2}`},
		{path: "/p", desiredExists: false, reportedExists: true, reported: `{"a/b":9}`},
	})

	// 显式钉死：结果里不得出现任何由斜杠字段名虚构出的对象内部子路径。
	for _, e := range entries {
		if e.Path == "/o/a~1b" || e.Path == "/p/a~1b" {
			t.Fatalf("fabricated child path for a missing whole object: %+v", entries)
		}
	}
}

// 名字相似、位置不同的两处差异在连续修改中各自独立计时：
//   - 两处差异产生于不同时刻时，首次出现时间分别记录；
//   - 只让一处恢复一致，它从结果中消失，另一处保留原时间和原值；
//   - 已消失的位置后来再次不一致，从本次时刻重新计时，
//     不继承名字相似的另一处记录，也不找回自己更早一轮的时间；
//   - 持续存在的位置即使值多次变化，只要双方从未一致，首次出现时间不重置。
//
// 计时记录在关闭重开后仍按转义路径原样保留；Diff/Get/Audit 等查询全程只读。
func TestDiffConfusablePathsKeepIndependentTiming(t *testing.T) {
	s, dir := openTemp(t)
	const id = "dev-timing"
	mustRegister(t, s, id, "1.0")

	t1 := base
	// 初始期望同时包含对象 a（下有 b）与字面字段 a/b；上报前上报侧是空对象，
	// 因此只在 /a 与 /a~1b 处各有一条“期望侧独有”的整体差异，不会下钻出 /a/b。
	firstDesired := json.RawMessage(`{"a":{"b":"D"},"a/b":"L0"}`)
	if rev, err := s.UpdateDesired(id, "op", t1, 0, firstDesired); err != nil || rev != 1 {
		t.Fatalf("UpdateDesired: rev=%d err=%v", rev, err)
	}
	entries, err := s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertEntries(t, entries, []entryExpectation{
		{path: "/a", desiredExists: true, reportedExists: false, desired: `{"b":"D"}`, since: t1},
		{path: "/a~1b", desiredExists: true, reportedExists: false, desired: `"L0"`, since: t1},
	})

	// t2 的首报让双方都具备对象 a（子字段 b 仍不一致），并把字面字段 a/b
	// 报成与期望一致：/a 整体差异消失，子路径 /a/b 首次出现并从 t2 计时；
	// 曾在 t1 出现过的 /a~1b 恢复一致后移除。
	t2 := base.Add(time.Minute)
	if err := s.Report(id, 1, t2, "1.0",
		json.RawMessage(`{"a":{"b":"R1"},"a/b":"L0"}`)); err != nil {
		t.Fatal(err)
	}
	entries, err = s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertEntries(t, entries, []entryExpectation{
		{path: "/a/b", desiredExists: true, reportedExists: true, desired: `"D"`, reported: `"R1"`, since: t2},
	})

	// t3 修改期望：只改字面字段 a/b 的值。/a~1b 再次出现，必须从 t3 重新计时，
	// 不能找回它在 t1 的旧记录；已存在的 /a/b 沿用 t2，两处互不串时间。
	t3 := base.Add(2 * time.Minute)
	desiredCfg := json.RawMessage(`{"a":{"b":"D"},"a/b":"Ld"}`)
	if rev, err := s.UpdateDesired(id, "op", t3, 1, desiredCfg); err != nil || rev != 2 {
		t.Fatalf("UpdateDesired rev2: rev=%d err=%v", rev, err)
	}
	entries, err = s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertEntries(t, entries, []entryExpectation{
		{path: "/a/b", desiredExists: true, reportedExists: true, desired: `"D"`, reported: `"R1"`, since: t2},
		{path: "/a~1b", desiredExists: true, reportedExists: true, desired: `"Ld"`, reported: `"L0"`, since: t3},
	})

	// t4 只让对象位置恢复一致（报成期望的 "D"），字面字段仍不一致：
	// /a/b 消失；/a~1b 保留 t3 与原值 "L0"。
	t4 := base.Add(3 * time.Minute)
	if err := s.Report(id, 2, t4, "1.0",
		json.RawMessage(`{"a":{"b":"D"},"a/b":"L0"}`)); err != nil {
		t.Fatal(err)
	}
	entries, err = s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertEntries(t, entries, []entryExpectation{
		{path: "/a~1b", desiredExists: true, reportedExists: true, desired: `"Ld"`, reported: `"L0"`, since: t3},
	})

	// t5 对象位置再次不一致：必须从 t5 重新计时，不能沿用自己更早的 t2，
	// 也不能继承名字相似、仍在结果中的 /a~1b 的 t3。
	t5 := base.Add(4 * time.Minute)
	if err := s.Report(id, 3, t5, "1.0",
		json.RawMessage(`{"a":{"b":"R2"},"a/b":"L0"}`)); err != nil {
		t.Fatal(err)
	}
	entries, err = s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertEntries(t, entries, []entryExpectation{
		{path: "/a/b", desiredExists: true, reportedExists: true, desired: `"D"`, reported: `"R2"`, since: t5},
		{path: "/a~1b", desiredExists: true, reportedExists: true, desired: `"Ld"`, reported: `"L0"`, since: t3},
	})

	// t6 只改持续不一致的字面字段值：/a~1b 的时间不重置；/a/b 保持 t5。
	t6 := base.Add(5 * time.Minute)
	if err := s.Report(id, 4, t6, "1.0",
		json.RawMessage(`{"a":{"b":"R2"},"a/b":"L1"}`)); err != nil {
		t.Fatal(err)
	}
	entries, err = s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertEntries(t, entries, []entryExpectation{
		{path: "/a/b", desiredExists: true, reportedExists: true, desired: `"D"`, reported: `"R2"`, since: t5},
		{path: "/a~1b", desiredExists: true, reportedExists: true, desired: `"Ld"`, reported: `"L1"`, since: t3},
	})

	// t7 再改对象位置的值：双方仍未一致过，/a/b 继续保留 t5，/a~1b 保留 t3。
	t7 := base.Add(6 * time.Minute)
	lastReported := json.RawMessage(`{"a":{"b":"R3"},"a/b":"L1"}`)
	if err := s.Report(id, 5, t7, "1.0", lastReported); err != nil {
		t.Fatal(err)
	}
	entries, err = s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertEntries(t, entries, []entryExpectation{
		{path: "/a/b", desiredExists: true, reportedExists: true, desired: `"D"`, reported: `"R3"`, since: t5},
		{path: "/a~1b", desiredExists: true, reportedExists: true, desired: `"Ld"`, reported: `"L1"`, since: t3},
	})

	// 连续查询是只读的：重复 Diff 时间不刷新，Get/Audit 看到的配置、
	// 修订号、序号与审计记录都不变。
	again, err := s.Diff(id)
	if err != nil {
		t.Fatal(err)
	}
	assertEntries(t, again, []entryExpectation{
		{path: "/a/b", desiredExists: true, reportedExists: true, desired: `"D"`, reported: `"R3"`, since: t5},
		{path: "/a~1b", desiredExists: true, reportedExists: true, desired: `"Ld"`, reported: `"L1"`, since: t3},
	})
	v, err := s.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if v.Revision != 2 || v.LastSeq != 5 {
		t.Fatalf("revision/seq changed by queries: %+v", v)
	}
	if string(v.Desired) != string(desiredCfg) {
		t.Fatalf("desired config altered: %s", v.Desired)
	}
	if string(v.Reported) != string(lastReported) {
		t.Fatalf("reported config altered: %s", v.Reported)
	}
	recs, err := s.Audit(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[0].Revision != 1 || recs[1].Revision != 2 {
		t.Fatalf("audit must stay untouched by reports and queries: %+v", recs)
	}
	if string(recs[0].Before) != "{}" || string(recs[0].After) != string(firstDesired) ||
		string(recs[1].Before) != string(firstDesired) || string(recs[1].After) != string(desiredCfg) {
		t.Fatalf("audit configs altered: %+v", recs)
	}

	// 关闭重开：两条易混淆路径的时间按转义路径原样恢复。
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
	assertEntries(t, entries, []entryExpectation{
		{path: "/a/b", desiredExists: true, reportedExists: true, desired: `"D"`, reported: `"R3"`, since: t5},
		{path: "/a~1b", desiredExists: true, reportedExists: true, desired: `"Ld"`, reported: `"L1"`, since: t3},
	})
}
