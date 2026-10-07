package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// neverReportedDoc 构造一台“已经登记但从未上报”的设备：lastSeq 缺省为 0、
// 无上报时间、上报配置为空对象、设备离线；期望侧已经被修改过（非空期望、
// 审计与差异记录齐全），这些都不影响未上报状态的合法性。
func neverReportedDoc() fixStore {
	at := "2026-10-02T12:00:00Z"
	return fixStore{Format: 1, Devices: map[string]fixState{
		"dev-1": {
			Version:   "1.0",
			Revision:  1,
			Desired:   json.RawMessage(`{"a":1}`),
			Reported:  json.RawMessage(`{}`),
			DiffSince: map[string]string{"/a": at},
			Audit: []fixAudit{
				{Operator: "op", Time: at, Revision: 1, Before: json.RawMessage(`{}`),
					After: json.RawMessage(`{"a":1}`)},
			},
		},
	}}
}

// reportedDoc 构造一台“已有一次有效上报”的设备：lastSeq 为正整数、
// 上报时间非零。seq 允许跳号，这里直接使用 7。
func reportedDoc() fixStore {
	at := "2026-10-02T12:00:00Z"
	return fixStore{Format: 1, Devices: map[string]fixState{
		"dev-1": {
			Version:        "1.0",
			Online:         true,
			Desired:        json.RawMessage(`{}`),
			Reported:       json.RawMessage(`{"cpu":80}`),
			LastSeq:        7,
			LastReportTime: at,
			DiffSince:      map[string]string{"/cpu": at},
		},
	}}
}

// TestRestoreLastReportContradictionsRefused 针对“序号被清成 0、上报时间仍
// 保留时仍能打开，旧序号随后被当成新上报接受”的问题：每台设备的记录必须
// 自洽地表示“从未上报”或“已有一次有效上报”，任一矛盾都必须以
// ErrCorruptStorage 拒绝打开整个存储，且原文件内容保留，不自动补时间、
// 改序号、清空上报配置或替换版本。
func TestRestoreLastReportContradictionsRefused(t *testing.T) {
	cases := map[string]func(fixStore){
		// 报告中的例子：接受过序号 7 的设备被改成序号 0，上报时间仍保留。
		"seq reset to zero with time kept": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.LastSeq = 0
			doc.Devices["dev-1"] = d
		},
		// 从未上报却保留上报时间。
		"never reported with report time": func(doc fixStore) {
			d := neverReportedDoc().Devices["dev-1"]
			d.LastReportTime = "2026-10-02T12:00:00Z"
			doc.Devices["dev-1"] = d
		},
		// 从未上报却保存了非空上报配置。
		"never reported with non-empty reported": func(doc fixStore) {
			d := neverReportedDoc().Devices["dev-1"]
			d.Reported = json.RawMessage(`{"cpu":80}`)
			doc.Devices["dev-1"] = d
		},
		// 从未上报却标记在线。
		"never reported but online": func(doc fixStore) {
			d := neverReportedDoc().Devices["dev-1"]
			d.Online = true
			doc.Devices["dev-1"] = d
		},
		// 已有上报（正序号）却缺少上报时间。
		"reported without time": func(doc fixStore) {
			d := reportedDoc().Devices["dev-1"]
			d.LastReportTime = ""
			doc.Devices["dev-1"] = d
		},
		// 已有上报但时间是显式零值。
		"reported with zero time": func(doc fixStore) {
			d := reportedDoc().Devices["dev-1"]
			d.LastReportTime = "0001-01-01T00:00:00Z"
			doc.Devices["dev-1"] = d
		},
		// 未上报设备版本为空。
		"never reported with empty version": func(doc fixStore) {
			d := neverReportedDoc().Devices["dev-1"]
			d.Version = ""
			doc.Devices["dev-1"] = d
		},
		// 已上报设备版本为空。
		"reported with empty version": func(doc fixStore) {
			d := reportedDoc().Devices["dev-1"]
			d.Version = ""
			doc.Devices["dev-1"] = d
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			// 默认以已上报文档为底，各用例自行决定改成什么形态。
			doc := reportedDoc()
			mutate(doc)
			dir, original := writeFixStore(t, doc)
			assertReopenCorrupt(t, dir, original)
		})
	}

	// 用真实流程复现报告中的攻击：接受序号 7 后把保存内容的序号改成 0，
	// 时间保留，存储必须拒绝打开。
	t.Run("real flow seq zeroed after close", func(t *testing.T) {
		s, dir := openTemp(t)
		mustRegister(t, s, "dev-1", "1.0")
		at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
		if err := s.Report("dev-1", 7, at, "1.1", json.RawMessage(`{"cpu":80}`)); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "dev-1")["lastSeq"] = 0
		})
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreLastReportLegitRecords 合法的两种记录形态必须正常打开并原样
// 保留含义：未上报设备可以修改过期望配置（非空期望、审计、差异齐全），
// 上报侧允许带合法空白的空对象；已上报设备可以上报空对象、序号允许跳号、
// 随后也可以被标记离线，序号、时间、版本与上报配置全部保留。
func TestRestoreLastReportLegitRecords(t *testing.T) {
	t.Run("never reported with modified desired and whitespace empty object", func(t *testing.T) {
		doc := neverReportedDoc()
		d := doc.Devices["dev-1"]
		d.Reported = json.RawMessage(` { } `) // 合法空白不影响空对象判断
		doc.Devices["dev-1"] = d
		dir, _ := writeFixStore(t, doc)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("never-reported device with modified desired must open: %v", err)
		}
		defer s.Close()
		v, err := s.Get("dev-1")
		if err != nil {
			t.Fatal(err)
		}
		if v.LastSeq != 0 || v.Online || v.Version != "1.0" || v.Revision != 1 {
			t.Fatalf("never-reported view changed: %+v", v)
		}
		if !rawEqual(v.Reported, json.RawMessage(`{}`)) {
			t.Fatalf("reported config not restored as empty object: %s", v.Reported)
		}
		if recs, _ := s.Audit("dev-1"); len(recs) != 1 {
			t.Fatalf("audit must be preserved: %+v", recs)
		}
		if diffs, _ := s.Diff("dev-1"); len(diffs) != 1 || diffs[0].Path != "/a" {
			t.Fatalf("diff must be preserved: %+v", diffs)
		}
	})

	t.Run("reported empty config keeps seq time and version", func(t *testing.T) {
		at := "2026-10-02T12:00:00Z"
		doc := fixStore{Format: 1, Devices: map[string]fixState{
			"dev-1": {
				Version:        "1.1",
				Online:         true,
				Desired:        json.RawMessage(`{}`),
				Reported:       json.RawMessage(`{}`),
				LastSeq:        3, // 跳号合法，不要求首次为 1
				LastReportTime: at,
			},
		}}
		dir, _ := writeFixStore(t, doc)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("device reporting empty object must open: %v", err)
		}
		defer s.Close()
		v, _ := s.Get("dev-1")
		if v.LastSeq != 3 || v.Version != "1.1" || !v.Online || !rawEqual(v.Reported, json.RawMessage(`{}`)) {
			t.Fatalf("reported view changed: %+v", v)
		}
	})

	t.Run("reported then offline keeps seq time version and config", func(t *testing.T) {
		s, dir := openTemp(t)
		mustRegister(t, s, "dev-1", "1.0")
		at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
		// 首次上报直接使用序号 7（跳号）。
		if err := s.Report("dev-1", 7, at, "1.1", json.RawMessage(`{"cpu":80}`)); err != nil {
			t.Fatal(err)
		}
		if err := s.SetOffline("dev-1"); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("offline reported device must open: %v", err)
		}
		defer s2.Close()
		v, _ := s2.Get("dev-1")
		if v.Online || v.LastSeq != 7 || v.Version != "1.1" {
			t.Fatalf("reported fields must survive offline + reopen: %+v", v)
		}
		if string(v.Reported) != `{"cpu":80}` {
			t.Fatalf("reported config changed: %s", v.Reported)
		}
		// 重开后序号规则沿用：更小的旧序号仍被判为过旧，不能覆盖影子。
		err = s2.Report("dev-1", 1, at.Add(time.Minute), "9.9", json.RawMessage(`{"x":1}`))
		if !errors.Is(err, ErrStaleSequence) {
			t.Fatalf("old small seq must be stale after reopen: %v", err)
		}
		v2, _ := s2.Get("dev-1")
		if v2.LastSeq != 7 || v2.Version != "1.1" || string(v2.Reported) != `{"cpu":80}` {
			t.Fatalf("stale report overwrote shadow: %+v", v2)
		}
	})
}

// TestRestoreLastReportOneBadDeviceRejectsAll 多台设备中只要一台的上报记录
// 自相矛盾，即使其他设备、活动与审计都合法，也不能只恢复其余设备：整个存储
// 拒绝打开且原文件内容保留。
func TestRestoreLastReportOneBadDeviceRejectsAll(t *testing.T) {
	doc := reportedDoc() // dev-1 合法（已上报）
	doc.Devices["good"] = fixState{
		Version:  "2.0",
		Desired:  json.RawMessage(`{}`),
		Reported: json.RawMessage(`{}`),
	}
	bad := neverReportedDoc().Devices["dev-1"]
	bad.LastReportTime = "2026-10-02T12:00:00Z" // 未上报却带上报时间
	doc.Devices["bad"] = bad
	dir, original := writeFixStore(t, doc)
	s, err := Open(dir)
	if err == nil {
		s.Close()
		t.Fatalf("expected ErrCorruptStorage")
	}
	if !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("error must wrap ErrCorruptStorage: %v", err)
	}
	data, rerr := os.ReadFile(filepath.Join(dir, storeFileName))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(data) != string(original) {
		t.Fatalf("corrupt file modified on rejected open")
	}
}

// TestRestoreLastReportRulesContinueAfterReopen 合法存储打开后，上报的新序号、
// 重复、冲突和过旧判断继续沿用现有规则。
func TestRestoreLastReportRulesContinueAfterReopen(t *testing.T) {
	s, dir := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	at := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	if err := s.Report("dev-1", 7, at, "1.1", json.RawMessage(`{"cpu":80}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("legit store must open: %v", err)
	}
	defer s2.Close()

	// 更大序号接受并更新影子。
	if err := s2.Report("dev-1", 9, at.Add(2*time.Minute), "1.2", json.RawMessage(`{"cpu":70}`)); err != nil {
		t.Fatalf("larger seq must be accepted: %v", err)
	}
	// 相同序号、相同内容（含同一时刻）视为重复，不报错也不改状态。
	if err := s2.Report("dev-1", 9, at.Add(2*time.Minute), "1.2", json.RawMessage(`{"cpu":70}`)); err != nil {
		t.Fatalf("duplicate must succeed: %v", err)
	}
	// 相同序号内容不同判冲突。
	err = s2.Report("dev-1", 9, at.Add(3*time.Minute), "1.3", json.RawMessage(`{"cpu":60}`))
	if !errors.Is(err, ErrReportConflict) {
		t.Fatalf("same seq different content must conflict: %v", err)
	}
	// 更小序号判过旧。
	err = s2.Report("dev-1", 8, at.Add(4*time.Minute), "1.3", json.RawMessage(`{}`))
	if !errors.Is(err, ErrStaleSequence) {
		t.Fatalf("smaller seq must be stale: %v", err)
	}
	v, _ := s2.Get("dev-1")
	if v.LastSeq != 9 || v.Version != "1.2" || string(v.Reported) != `{"cpu":70}` {
		t.Fatalf("shadow after reopen reports wrong: %+v", v)
	}
}
