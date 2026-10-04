package shadow

import (
	"bytes"
	"encoding/json"
	"math/big"
	"testing"
	"time"
)

// 本文件通过公开入口（UpdateDesired/Report 写入，Diff/Get/Audit 查询及重开）
// 回归保障配置差异查询中“数字按数值比较”的现有行为：
// 真实数值差别（包括超过 float64 精度的大整数与高精度小数）必须列出，
// 仅仅写法不同的等值数字不算差异，数字与字符串不做类型自动转换；
// 查询结果保留双方提交的完整数字原文，且查询本身不改写任何已保存状态。

// seedDeviceConfigs 登记新设备并分别写入期望配置与上报配置，
// 使后续测试可以直接通过 Diff 观察双方比较结果。
func seedDeviceConfigs(t *testing.T, s *Store, id, desired, reported string) {
	t.Helper()
	mustRegister(t, s, id, "1.0")
	if _, err := s.UpdateDesired(id, "op", base, 0, json.RawMessage(desired)); err != nil {
		t.Fatalf("UpdateDesired(%s): %v", id, err)
	}
	if err := s.Report(id, 1, base.Add(time.Minute), "1.0", json.RawMessage(reported)); err != nil {
		t.Fatalf("Report(%s): %v", id, err)
	}
}

// decodeJSONNumber 把一段 JSON 标量解码为 json.Number，
// 确保差异里返回的数字仍是 JSON 数字，而不是字符串或近似后的浮点值。
func decodeJSONNumber(t *testing.T, raw json.RawMessage) json.Number {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("decode %q: %v", raw, err)
	}
	n, ok := v.(json.Number)
	if !ok {
		t.Fatalf("value %s decoded as %T, want JSON number", raw, v)
	}
	return n
}

func mustRat(t *testing.T, literal string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(literal)
	if !ok {
		t.Fatalf("test premise: %q is not parseable as an exact number", literal)
	}
	return r
}

// 超过 float64 安全整数范围的相邻大整数、超出 float64 小数精度的高精度小数
// 必须被识别为真实差异；嵌套对象里只列实际不同的字段，数组整体比较只列数组
// 所在路径（不展开下标）。返回值保留提交时的完整数字，双方存在性均为真。
func TestDiffDistinguishesLargeAndHighPrecisionNumbers(t *testing.T) {
	s, _ := openTemp(t)
	cases := []struct {
		name         string
		id           string
		desired      string
		reported     string
		path         string
		wantDesired  string
		wantReported string
		// wholeValue 表示该路径差异的双方值是数组等整体值，
		// 不应按 JSON 数字标量解码；同时用于核对数组不拆下标。
		wholeValue bool
	}{
		{
			name:         "adjacent integers beyond float64 safe range",
			id:           "dev-bigint",
			desired:      `{"x":9007199254740992}`,
			reported:     `{"x":9007199254740993}`,
			path:         "/x",
			wantDesired:  "9007199254740992",
			wantReported: "9007199254740993",
		},
		{
			name:         "high precision decimals distinct past float64 precision",
			id:           "dev-decimal",
			desired:      `{"x":0.10000000000000000001}`,
			reported:     `{"x":0.10000000000000000002}`,
			path:         "/x",
			wantDesired:  "0.10000000000000000001",
			wantReported: "0.10000000000000000002",
		},
		{
			name:         "large integers inside nested object",
			id:           "dev-nested-bigint",
			desired:      `{"o":{"x":9007199254740992}}`,
			reported:     `{"o":{"x":9007199254740993}}`,
			path:         "/o/x",
			wantDesired:  "9007199254740992",
			wantReported: "9007199254740993",
		},
		{
			name:         "high precision decimals inside nested object",
			id:           "dev-nested-decimal",
			desired:      `{"o":{"y":0.10000000000000000001}}`,
			reported:     `{"o":{"y":0.10000000000000000002}}`,
			path:         "/o/y",
			wantDesired:  "0.10000000000000000001",
			wantReported: "0.10000000000000000002",
		},
		{
			name:         "signed values genuinely different",
			id:           "dev-signed",
			desired:      `{"x":-1}`,
			reported:     `{"x":1}`,
			path:         "/x",
			wantDesired:  "-1",
			wantReported: "1",
		},
		{
			name:         "signed small decimals genuinely different",
			id:           "dev-signed-decimal",
			desired:      `{"x":-0.0001}`,
			reported:     `{"x":0.0001}`,
			path:         "/x",
			wantDesired:  "-0.0001",
			wantReported: "0.0001",
		},
		{
			name:         "large integers inside array compared as whole array",
			id:           "dev-array-bigint",
			desired:      `{"xs":[9007199254740992]}`,
			reported:     `{"xs":[9007199254740993]}`,
			path:         "/xs",
			wantDesired:  "[9007199254740992]",
			wantReported: "[9007199254740993]",
			wholeValue:   true,
		},
		{
			name:         "high precision decimals inside array compared as whole array",
			id:           "dev-array-decimal",
			desired:      `{"xs":[0.10000000000000000001]}`,
			reported:     `{"xs":[0.10000000000000000002]}`,
			path:         "/xs",
			wantDesired:  "[0.10000000000000000001]",
			wantReported: "[0.10000000000000000002]",
			wholeValue:   true,
		},
		{
			name:         "array nested in object keeps only array path",
			id:           "dev-nested-array",
			desired:      `{"o":{"xs":[9007199254740992,1]}}`,
			reported:     `{"o":{"xs":[9007199254740993,1]}}`,
			path:         "/o/xs",
			wantDesired:  "[9007199254740992,1]",
			wantReported: "[9007199254740993,1]",
			wholeValue:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seedDeviceConfigs(t, s, tc.id, tc.desired, tc.reported)
			entries, err := s.Diff(tc.id)
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			// 每对配置只在指定路径存在一处差异；数组用例借此证明
			// 不会把 /xs/0 之类下标展开成额外差异。
			if len(entries) != 1 {
				t.Fatalf("want exactly one diff at %s, got %+v", tc.path, entries)
			}
			e := entries[0]
			if e.Path != tc.path {
				t.Fatalf("path: want %s, got %s", tc.path, e.Path)
			}
			if !e.DesiredExists || !e.ReportedExists {
				t.Fatalf("both sides must exist: %+v", e)
			}
			if string(e.Desired) != tc.wantDesired {
				t.Fatalf("desired value: want %s, got %s", tc.wantDesired, e.Desired)
			}
			if string(e.Reported) != tc.wantReported {
				t.Fatalf("reported value: want %s, got %s", tc.wantReported, e.Reported)
			}
			if tc.wholeValue {
				return
			}
			// 标量数字：差异值必须仍是 JSON 数字且逐字保留提交内容，
			// 不能被近似成另一个数字或转成字符串。
			dn := decodeJSONNumber(t, e.Desired)
			rn := decodeJSONNumber(t, e.Reported)
			if dn.String() != tc.wantDesired || rn.String() != tc.wantReported {
				t.Fatalf("numbers not preserved verbatim: %s vs %s", dn, rn)
			}
			// 确认测试前提：两侧确实是不同的精确数值。
			if mustRat(t, tc.wantDesired).Cmp(mustRat(t, tc.wantReported)) == 0 {
				t.Fatalf("test premise broken: %s and %s are numerically equal", tc.wantDesired, tc.wantReported)
			}
		})
	}
}

// 同一数值的整数、小数、科学计数法写法（含正负零、嵌套对象与数组元素、
// 对象字段换序与 JSON 空白变化）必须判为配置一致，不产生任何差异。
func TestDiffTreatsNumericSpellingsAsEqual(t *testing.T) {
	s, _ := openTemp(t)
	cases := []struct {
		name     string
		id       string
		desired  string
		reported string
	}{
		{"integer vs decimal", "dev-eq-1", `{"x":1000}`, `{"x":1000.00}`},
		{"integer vs scientific", "dev-eq-2", `{"x":1000}`, `{"x":1e3}`},
		{"decimal vs scientific", "dev-eq-3", `{"x":1000.00}`, `{"x":1e3}`},
		{"scientific on desired side", "dev-eq-4", `{"x":1e3}`, `{"x":1000}`},
		{"small decimal vs scientific", "dev-eq-5", `{"x":0.0001}`, `{"x":1e-4}`},
		{"large integer vs exact scientific spelling", "dev-eq-6", `{"x":9007199254740992}`, `{"x":9.007199254740992e15}`},
		{"positive zero vs negative zero", "dev-eq-7", `{"x":0}`, `{"x":-0}`},
		{"negative zero decimal forms", "dev-eq-8", `{"x":-0.0}`, `{"x":0.0e0}`},
		{"numeric forms inside nested object", "dev-eq-9", `{"o":{"x":1000,"y":0.0001}}`, `{"o":{"x":1e3,"y":1e-4}}`},
		{"numeric forms as array elements", "dev-eq-10", `{"xs":[1000,0.0001]}`, `{"xs":[1.0e3,1e-4]}`},
		{"integer vs decimal array element", "dev-eq-11", `{"xs":[1000]}`, `{"xs":[1000.00]}`},
		{"field reorder whitespace and numeric forms", "dev-eq-12",
			"{\n  \"a\": 1000,\n  \"b\": 0.0001\n}", `{"b":1e-4,"a":1e3}`},
		{"whitespace around compact number", "dev-eq-13", `{ "x": 1000 }`, `{"x":1e3}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seedDeviceConfigs(t, s, tc.id, tc.desired, tc.reported)
			entries, err := s.Diff(tc.id)
			if err != nil {
				t.Fatalf("Diff: %v", err)
			}
			if len(entries) != 0 {
				t.Fatalf("numeric spellings must compare equal, got diffs: %+v", entries)
			}
		})
	}
}

// 数字与写着相同字符的字符串属于不同类型：必须保留差异且不自动转换类型。
func TestDiffDoesNotCoerceNumberAndString(t *testing.T) {
	s, _ := openTemp(t)

	seedDeviceConfigs(t, s, "dev-typestr", `{"x":1000}`, `{"x":"1000"}`)
	entries, err := s.Diff("dev-typestr")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "/x" {
		t.Fatalf("want single /x diff, got %+v", entries)
	}
	e := entries[0]
	if !e.DesiredExists || !e.ReportedExists {
		t.Fatalf("both sides must exist: %+v", e)
	}
	if string(e.Desired) != "1000" {
		t.Fatalf("desired number: %s", e.Desired)
	}
	if string(e.Reported) != `"1000"` {
		t.Fatalf("reported string must keep quotes: %s", e.Reported)
	}
	if n := decodeJSONNumber(t, e.Desired); n.String() != "1000" {
		t.Fatalf("desired should be JSON number: %s", n)
	}
	var str string
	if err := json.Unmarshal(e.Reported, &str); err != nil || str != "1000" {
		t.Fatalf("reported should be JSON string \"1000\": %v %q", err, str)
	}

	// 数组内元素类型不同：数组整体比较，只列数组路径。
	seedDeviceConfigs(t, s, "dev-typestr-arr", `{"xs":[1000]}`, `{"xs":["1000"]}`)
	entries, err = s.Diff("dev-typestr-arr")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Path != "/xs" {
		t.Fatalf("want whole-array diff /xs, got %+v", entries)
	}
	if string(entries[0].Desired) != "[1000]" || string(entries[0].Reported) != `["1000"]` {
		t.Fatalf("array values: %s vs %s", entries[0].Desired, entries[0].Reported)
	}
}

// 等值数字与不等值数字同时存在时，结果只包含实际不同的路径，按 JSON Pointer
// 字典序返回，且每条差异中的双方值与对应字段精确匹配。
func TestDiffMixedEqualAndUnequalNumbers(t *testing.T) {
	s, _ := openTemp(t)
	desired := `{"same":1000,"big":9007199254740992,"deep":{"eq":0.0001,"neq":0.10000000000000000001},"arr":[1,2],"kind":1}`
	reported := `{"same":1e3,"big":9007199254740993,"deep":{"eq":1e-4,"neq":0.10000000000000000002},"arr":[1,2,3],"kind":"1"}`
	seedDeviceConfigs(t, s, "dev-mix", desired, reported)

	entries, err := s.Diff("dev-mix")
	if err != nil {
		t.Fatal(err)
	}
	// /same 与 /deep/eq 仅数字写法不同，不应出现。
	want := []struct {
		path     string
		desired  string
		reported string
	}{
		{"/arr", "[1,2]", "[1,2,3]"},
		{"/big", "9007199254740992", "9007199254740993"},
		{"/deep/neq", "0.10000000000000000001", "0.10000000000000000002"},
		{"/kind", "1", `"1"`},
	}
	if len(entries) != len(want) {
		t.Fatalf("want %d diffs, got %+v", len(want), entries)
	}
	for i := range entries {
		if i > 0 && entries[i-1].Path >= entries[i].Path {
			t.Fatalf("paths not strictly lexicographic: %q then %q", entries[i-1].Path, entries[i].Path)
		}
		if entries[i].Path != want[i].path {
			t.Fatalf("entry %d path: want %s, got %s", i, want[i].path, entries[i].Path)
		}
		if !entries[i].DesiredExists || !entries[i].ReportedExists {
			t.Fatalf("%s existence flags: %+v", entries[i].Path, entries[i])
		}
		if string(entries[i].Desired) != want[i].desired {
			t.Fatalf("%s desired: want %s, got %s", entries[i].Path, want[i].desired, entries[i].Desired)
		}
		if string(entries[i].Reported) != want[i].reported {
			t.Fatalf("%s reported: want %s, got %s", entries[i].Path, want[i].reported, entries[i].Reported)
		}
	}
}

// Diff/Get/Audit 都是只读查询：不得改写设备保存的期望配置或上报配置，
// 不得增加修订号或审计记录；再次读取仍看到原先提交的完整数字。
// 返回给调用方的数据被篡改也不影响存储与后续查询。
func TestDiffNumericQueriesAreReadOnly(t *testing.T) {
	s, _ := openTemp(t)
	desiredCfg := `{"big":9007199254740992,"frac":0.10000000000000000001,"same":1000}`
	reportedCfg := `{"big":9007199254740993,"frac":0.10000000000000000002,"same":1e3}`
	seedDeviceConfigs(t, s, "dev-ro", desiredCfg, reportedCfg)

	before, err := s.Get("dev-ro")
	if err != nil {
		t.Fatal(err)
	}
	if before.Revision != 1 || before.LastSeq != 1 {
		t.Fatalf("unexpected starting state: %+v", before)
	}

	first, err := s.Diff("dev-ro")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].Path != "/big" || first[1].Path != "/frac" {
		t.Fatalf("initial diffs: %+v", first)
	}
	// 重复查询结果一致。
	again, err := s.Diff("dev-ro")
	if err != nil || len(again) != 2 {
		t.Fatalf("second Diff: %v %+v", err, again)
	}
	recs, err := s.Audit("dev-ro")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("audit must not grow from queries: %+v", recs)
	}

	after, err := s.Get("dev-ro")
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || after.LastSeq != before.LastSeq {
		t.Fatalf("revision/seq changed by queries: before %+v after %+v", before, after)
	}
	if string(after.Desired) != desiredCfg {
		t.Fatalf("desired config rewritten by queries: %s", after.Desired)
	}
	if string(after.Reported) != reportedCfg {
		t.Fatalf("reported config rewritten by queries: %s", after.Reported)
	}
	if string(recs[0].Before) != "{}" || string(recs[0].After) != desiredCfg {
		t.Fatalf("audit configs altered: before=%s after=%s", recs[0].Before, recs[0].After)
	}

	// 篡改查询返回值后，存储内容与后续查询不受影响。
	first[0].Desired[0] = '0'
	first[1].Reported[0] = '0'
	again[0].Desired = nil
	after.Desired[0] = 'X'
	after.Reported[0] = 'X'
	final, err := s.Get("dev-ro")
	if err != nil {
		t.Fatal(err)
	}
	if string(final.Desired) != desiredCfg || string(final.Reported) != reportedCfg {
		t.Fatalf("stored configs mutated by caller: %s / %s", final.Desired, final.Reported)
	}
	if final.Revision != 1 || final.LastSeq != 1 {
		t.Fatalf("state changed by caller mutation: %+v", final)
	}
	diffs, err := s.Diff("dev-ro")
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 2 || string(diffs[0].Desired) != "9007199254740992" ||
		string(diffs[1].Reported) != "0.10000000000000000002" {
		t.Fatalf("diff results mutated by caller: %+v", diffs)
	}
}

// 关闭重开后数字比较行为与完整数字原文保持不变：真实差异仍按完整数字列出，
// 仅写法不同的等值数字不会在打开时重算差异，且双方配置、修订号与审计保持原样。
func TestDiffNumericBehaviorSurvivesReopen(t *testing.T) {
	s, dir := openTemp(t)
	desiredCfg := `{"big":9007199254740992,"frac":0.10000000000000000001,"same":1000}`
	reportedCfg := `{"big":9007199254740993,"frac":0.10000000000000000002,"same":1e3}`
	seedDeviceConfigs(t, s, "dev-persist", desiredCfg, reportedCfg)
	// 另一台设备只有等值写法差异：重开重算差异后仍应为零条。
	seedDeviceConfigs(t, s, "dev-equal", `{"x":1000}`, `{"x":1e3}`)
	if entries, _ := s.Diff("dev-equal"); len(entries) != 0 {
		t.Fatalf("precondition: equal spellings diff: %+v", entries)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()

	v, err := s2.Get("dev-persist")
	if err != nil {
		t.Fatal(err)
	}
	if string(v.Desired) != desiredCfg || string(v.Reported) != reportedCfg {
		t.Fatalf("full digits lost across reopen: %s / %s", v.Desired, v.Reported)
	}
	if v.Revision != 1 || v.LastSeq != 1 {
		t.Fatalf("revision/seq changed across reopen: %+v", v)
	}
	entries, err := s2.Diff("dev-persist")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want /big and /frac, got %+v", entries)
	}
	want := []struct {
		path     string
		desired  string
		reported string
	}{
		{"/big", "9007199254740992", "9007199254740993"},
		{"/frac", "0.10000000000000000001", "0.10000000000000000002"},
	}
	for i, w := range want {
		if entries[i].Path != w.path || !entries[i].DesiredExists || !entries[i].ReportedExists {
			t.Fatalf("entry %d flags/path: %+v", i, entries[i])
		}
		if string(entries[i].Desired) != w.desired || string(entries[i].Reported) != w.reported {
			t.Fatalf("entry %d values: %s / %s", i, entries[i].Desired, entries[i].Reported)
		}
	}
	if recs, _ := s2.Audit("dev-persist"); len(recs) != 1 || string(recs[0].After) != desiredCfg {
		t.Fatalf("audit changed across reopen: %+v", recs)
	}

	equalEntries, err := s2.Diff("dev-equal")
	if err != nil {
		t.Fatal(err)
	}
	if len(equalEntries) != 0 {
		t.Fatalf("equal numeric spellings must stay equal after reopen: %+v", equalEntries)
	}
}
