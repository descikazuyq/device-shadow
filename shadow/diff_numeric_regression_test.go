package shadow

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

// setupDiffDevice 登记一台设备并写入期望与上报配置，返回存储与上报时间。
func setupDiffDevice(t *testing.T, desired, reported json.RawMessage) (*Store, time.Time) {
	t.Helper()
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-num", "1.0")
	if _, err := s.UpdateDesired("dev-num", "op", base, 0, desired); err != nil {
		t.Fatalf("UpdateDesired: %v", err)
	}
	at := base.Add(time.Minute)
	if err := s.Report("dev-num", 1, at, "1.0", reported); err != nil {
		t.Fatalf("Report: %v", err)
	}
	return s, at
}

// mustJSONCompact 返回 raw 去除 JSON 空白后的紧凑形式，用于比较原始数字写法。
func mustJSONCompact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %s: %v", raw, err)
	}
	return buf.String()
}

// TestDiffDistinguishesLargeIntegers 超过 float64 安全整数范围的相邻整数
// 仍必须按精确数值区分：既不能因 float64 精度丢失而漏掉真实差异，
// 返回的双方值也必须保留各自提交的完整数字（不能变成近似值或字符串）。
func TestDiffDistinguishesLargeIntegers(t *testing.T) {
	s, _ := setupDiffDevice(t,
		json.RawMessage(`{"big":9007199254740992}`),
		json.RawMessage(`{"big":9007199254740993}`),
	)
	diffs, err := s.Diff("dev-num")
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 {
		t.Fatalf("want one diff, got %+v", diffs)
	}
	e := diffs[0]
	if e.Path != "/big" {
		t.Fatalf("path: %s", e.Path)
	}
	if !e.DesiredExists || !e.ReportedExists {
		t.Fatalf("both sides must exist: %+v", e)
	}
	if string(e.Desired) != "9007199254740992" || string(e.Reported) != "9007199254740993" {
		t.Fatalf("full original digits must be preserved: desired=%s reported=%s", e.Desired, e.Reported)
	}
	// 确认返回的是 JSON 数字而不是字符串
	var probe float64
	if err := json.Unmarshal(e.Desired, &probe); err != nil {
		t.Fatalf("desired must be a JSON number: %s (%v)", e.Desired, err)
	}
	var probeStr string
	if err := json.Unmarshal(e.Desired, &probeStr); err == nil {
		t.Fatalf("desired must not be a JSON string: %s", e.Desired)
	}
	if !e.Since.Equal(base) {
		t.Fatalf("since: want %v got %v", base, e.Since)
	}
}

// TestDiffDistinguishesHighPrecisionDecimals 精度远超 float64 有效位数的两个
// 相邻小数必须被区分，同样不能因精度丢失而显示一致。
func TestDiffDistinguishesHighPrecisionDecimals(t *testing.T) {
	s, _ := setupDiffDevice(t,
		json.RawMessage(`{"f":0.10000000000000000001}`),
		json.RawMessage(`{"f":0.10000000000000000002}`),
	)
	diffs, err := s.Diff("dev-num")
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Path != "/f" {
		t.Fatalf("want single /f diff, got %+v", diffs)
	}
	e := diffs[0]
	if !e.DesiredExists || !e.ReportedExists {
		t.Fatalf("both sides must exist: %+v", e)
	}
	if string(e.Desired) != "0.10000000000000000001" || string(e.Reported) != "0.10000000000000000002" {
		t.Fatalf("full decimals must be preserved: desired=%s reported=%s", e.Desired, e.Reported)
	}
	if !e.Since.Equal(base) {
		t.Fatalf("since: want %v got %v", base, e.Since)
	}
}

// TestDiffLargeNumbersInNestedObject 大数字出现在嵌套对象里时，仍要逐层
// 比较并指出真正不同的叶子字段；相等字段（即使写法不同）不能混入。
func TestDiffLargeNumbersInNestedObject(t *testing.T) {
	s, _ := setupDiffDevice(t,
		json.RawMessage(`{"net":{"gw":9007199254740992,"mtu":1500,"deep":{"x":9007199254740992}}}`),
		json.RawMessage(`{"net":{"gw":9007199254740993,"mtu":1500,"deep":{"x":9007199254740992}}}`),
	)
	diffs, err := s.Diff("dev-num")
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Path != "/net/gw" {
		t.Fatalf("only /net/gw should differ, got %+v", diffs)
	}
	if string(diffs[0].Desired) != "9007199254740992" || string(diffs[0].Reported) != "9007199254740993" {
		t.Fatalf("values: %+v", diffs[0])
	}
}

// TestDiffNumericArrayComparedWhole 数组按整体比较：元素不同（含高精度
// 数字差别）时只列数组所在路径，绝不把数组下标展开成差异；数组元素
// 数值写法不同但值相等时，视为无差异。
func TestDiffNumericArrayComparedWhole(t *testing.T) {
	s, _ := setupDiffDevice(t,
		json.RawMessage(`{"vec":[9007199254740992,2,3]}`),
		json.RawMessage(`{"vec":[9007199254740993,2,3]}`),
	)
	diffs, err := s.Diff("dev-num")
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Path != "/vec" {
		t.Fatalf("array must be reported at /vec only, got %+v", diffs)
	}
	if !diffs[0].DesiredExists || !diffs[0].ReportedExists {
		t.Fatalf("existence flags: %+v", diffs[0])
	}
	// 返回的是双方完整数组，不是单个元素
	if mustJSONCompact(t, diffs[0].Desired) != "[9007199254740992,2,3]" ||
		mustJSONCompact(t, diffs[0].Reported) != "[9007199254740993,2,3]" {
		t.Fatalf("whole arrays must be preserved: desired=%s reported=%s",
			diffs[0].Desired, diffs[0].Reported)
	}

	// 数组元素仅换用相等的数字写法：无差异
	s2, _ := setupDiffDevice(t,
		json.RawMessage(`{"vec":[1000,0.0001]}`),
		json.RawMessage(`{"vec":[1e3,1e-4]}`),
	)
	if diffs, err := s2.Diff("dev-num"); err != nil || len(diffs) != 0 {
		t.Fatalf("numeric spellings in array must be equal: %+v err=%v", diffs, err)
	}
}

// TestDiffNumericSpellingsAreEqual 同一数值的整数、小数、科学计数法写法
// 视为配置一致，不产生差异；对象字段换序、JSON 空白变化同样无影响。
func TestDiffNumericSpellingsAreEqual(t *testing.T) {
	cases := []struct {
		name     string
		desired  string
		reported string
	}{
		{"int vs decimal", `{"v":1000}`, `{"v":1000.00}`},
		{"int vs sci", `{"v":1000}`, `{"v":1e3}`},
		{"decimal vs sci", `{"v":1000.00}`, `{"v":1e3}`},
		{"small decimal vs sci", `{"v":0.0001}`, `{"v":1e-4}`},
		{"capital e", `{"v":1000}`, `{"v":1E3}`},
		{"sci plus sign", `{"v":1000}`, `{"v":1e+3}`},
		{"nested", `{"o":{"v":1000}}`, `{"o":{"v":1e3}}`},
		{"in array", `{"a":[1000,0.0001]}`, `{"a":[1e3,1e-4]}`},
		{"nested in array", `{"a":[{"v":1000}]}`, `{"a":[{"v":1e3}]}`},
		{"signed zero in array", `{"a":[0]}`, `{"a":[-0]}`},
		{"signed zero decimal", `{"v":0.0}`, `{"v":-0}`},
		{"field reordered + whitespace",
			"{\n\t\"b\": 2, \"a\": 1000.00\n}",
			`{"a": 1e3, "b": 2}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _ := setupDiffDevice(t, json.RawMessage(c.desired), json.RawMessage(c.reported))
			diffs, err := s.Diff("dev-num")
			if err != nil {
				t.Fatal(err)
			}
			if len(diffs) != 0 {
				t.Fatalf("numeric spellings must compare equal: %+v", diffs)
			}
		})
	}
}

// TestDiffSignedZero 正零与负零按数值相等；真正不同的正负数仍有差异。
func TestDiffSignedZero(t *testing.T) {
	s, _ := setupDiffDevice(t,
		json.RawMessage(`{"z":0,"nz":-0}`),
		json.RawMessage(`{"z":-0,"nz":5}`),
	)
	diffs, err := s.Diff("dev-num")
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Path != "/nz" {
		t.Fatalf("only /nz should differ (positive vs negative): %+v", diffs)
	}
	if string(diffs[0].Desired) != "-0" || string(diffs[0].Reported) != "5" {
		t.Fatalf("values: desired=%s reported=%s", diffs[0].Desired, diffs[0].Reported)
	}

	// 绝对值相同但符号相反的非零数属于真实数值差别，必须检出
	s2, _ := setupDiffDevice(t,
		json.RawMessage(`{"p":-5}`),
		json.RawMessage(`{"p":5}`),
	)
	if diffs, _ := s2.Diff("dev-num"); len(diffs) != 1 || diffs[0].Path != "/p" {
		t.Fatalf("-5 vs 5 must differ: %+v", diffs)
	}
}

// TestDiffNumberVsStringType 数字与写着相同字符的字符串属于不同类型，
// 必须保留差异，不能自动做类型转换；双方值原样返回。
func TestDiffNumberVsStringType(t *testing.T) {
	s, _ := setupDiffDevice(t,
		json.RawMessage(`{"v":1000}`),
		json.RawMessage(`{"v":"1000"}`),
	)
	diffs, err := s.Diff("dev-num")
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Path != "/v" {
		t.Fatalf("number vs string must differ: %+v", diffs)
	}
	e := diffs[0]
	if !e.DesiredExists || !e.ReportedExists {
		t.Fatalf("existence flags: %+v", e)
	}
	if string(e.Desired) != "1000" || string(e.Reported) != `"1000"` {
		t.Fatalf("number and string must be returned verbatim: desired=%s reported=%s",
			e.Desired, e.Reported)
	}
	if !e.Since.Equal(base) {
		t.Fatalf("since: want %v got %v", base, e.Since)
	}
}

// TestDiffMixedNumericPaths 两份配置中相等与不等数字同时存在时，
// 结果只包含实际不同的路径，按 JSON Pointer 字典序返回，且每条差异
// 携带的双方值与对应字段一一匹配（含高精度数字原样保留）。
func TestDiffMixedNumericPaths(t *testing.T) {
	s, _ := setupDiffDevice(t,
		json.RawMessage(`{
			"a": 1000,
			"b": 9007199254740992,
			"c": 0.0001,
			"d": 7,
			"e": {"x": 0.10000000000000000001, "y": 1000},
			"f": [1, 2],
			"g": 3
		}`),
		json.RawMessage(`{
			"a": 1e3,
			"b": 9007199254740993,
			"c": 1e-4,
			"d": 7,
			"e": {"x": 0.10000000000000000002, "y": 1000.00},
			"f": [1, 2],
			"g": "3"
		}`),
	)
	diffs, err := s.Diff("dev-num")
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		path              string
		desired, reported string
	}{
		{"/b", "9007199254740992", "9007199254740993"},
		{"/e/x", "0.10000000000000000001", "0.10000000000000000002"},
		{"/g", "3", `"3"`},
	}
	if len(diffs) != len(want) {
		t.Fatalf("want %d diffs, got %+v", len(want), diffs)
	}
	for i, w := range want {
		e := diffs[i]
		if e.Path != w.path {
			t.Fatalf("diff[%d] path: want %s got %s (full: %+v)", i, w.path, e.Path, diffs)
		}
		if !e.DesiredExists || !e.ReportedExists {
			t.Fatalf("%s existence flags: %+v", w.path, e)
		}
		if string(e.Desired) != w.desired || string(e.Reported) != w.reported {
			t.Fatalf("%s values: want %s/%s got %s/%s",
				w.path, w.desired, w.reported, e.Desired, e.Reported)
		}
		// /b、/g 在期望写入时（reported 为空对象）即已存在；而此时整个 /e
		// 子树在上报侧缺失，差异只记在 /e（不下钻），所以 /e/x 是上报后
		// 双方都有 /e 时才首次出现，时间为上报时刻 base+1m。
		wantSince := base
		if e.Path == "/e/x" {
			wantSince = base.Add(time.Minute)
		}
		if !e.Since.Equal(wantSince) {
			t.Fatalf("%s since: want %v got %v", w.path, wantSince, e.Since)
		}
	}
}

// TestDiffDoesNotMutateState Diff 是只读查询：不得改写设备保存的期望/上报
// 配置（包括其中数字的原始写法），不得增加修订号或审计记录；再次读取
// 仍看到原先提交的完整内容。
func TestDiffDoesNotMutateState(t *testing.T) {
	desired := json.RawMessage("{\n  \"big\": 9007199254740992,\n  \"same\": 1000.00\n}")
	reported := json.RawMessage(`{"same":1e3,"big":9007199254740993}`)
	s, _ := setupDiffDevice(t, desired, reported)

	before, err := s.Get("dev-num")
	if err != nil {
		t.Fatal(err)
	}
	recsBefore, err := s.Audit("dev-num")
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 3; i++ {
		diffs, err := s.Diff("dev-num")
		if err != nil {
			t.Fatal(err)
		}
		if len(diffs) != 1 || diffs[0].Path != "/big" {
			t.Fatalf("iteration %d: %+v", i, diffs)
		}
	}

	after, err := s.Get("dev-num")
	if err != nil {
		t.Fatal(err)
	}
	// 原始提交内容（含空白与数字写法）逐字节保留
	if !bytes.Equal(after.Desired, desired) {
		t.Fatalf("desired rewritten:\nbefore: %s\nafter:  %s", desired, after.Desired)
	}
	if !bytes.Equal(after.Reported, reported) {
		t.Fatalf("reported rewritten:\nbefore: %s\nafter:  %s", reported, after.Reported)
	}
	if after.Revision != before.Revision {
		t.Fatalf("revision changed: %d -> %d", before.Revision, after.Revision)
	}
	if after.Revision != 1 {
		t.Fatalf("revision should still be 1, got %d", after.Revision)
	}
	recsAfter, err := s.Audit("dev-num")
	if err != nil {
		t.Fatal(err)
	}
	if len(recsAfter) != len(recsBefore) {
		t.Fatalf("audit records changed: %d -> %d", len(recsBefore), len(recsAfter))
	}
	// 审计中保存的配置原文同样未被数字归一化
	if !bytes.Equal(recsAfter[0].After, desired) {
		t.Fatalf("audit after rewritten: %s", recsAfter[0].After)
	}
}

// TestDiffReturnedValuesAreIsolated 调用方篡改 Diff 返回的 RawMessage
// 不影响后续查询结果。
func TestDiffReturnedValuesAreIsolated(t *testing.T) {
	s, _ := setupDiffDevice(t,
		json.RawMessage(`{"big":9007199254740992}`),
		json.RawMessage(`{"big":9007199254740993}`),
	)
	diffs, _ := s.Diff("dev-num")
	diffs[0].Desired[0] = '1'
	diffs[0].Reported[0] = '1'
	diffs2, _ := s.Diff("dev-num")
	if string(diffs2[0].Desired) != "9007199254740992" || string(diffs2[0].Reported) != "9007199254740993" {
		t.Fatalf("stored diff values mutated by caller: %+v", diffs2[0])
	}
}
