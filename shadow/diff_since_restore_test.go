package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// 打开已有存储时，差异计时记录必须与设备当前的期望/上报配置一致：
// 每条差异路径都有一条非零的首次出现时间，且不存在没有差异的残留
// 路径。任何矛盾都按损坏存储处理，拒绝打开整个存储且不改写原文件。
func TestRestoreDiffSinceCorruptRefused(t *testing.T) {
	valid := func() fixStore {
		return fixStore{Format: 1, Devices: map[string]fixState{
			"dev-1": {
				Version:  "1.0",
				Revision: 1,
				Desired:  json.RawMessage(`{"wifi":{"ssid":"a","chan":1},"mode":"x"}`),
				Reported: json.RawMessage(`{"wifi":{"ssid":"b","chan":1},"mode":"x"}`),
				DiffSince: map[string]string{
					"/wifi/ssid": "2026-10-02T12:00:00Z",
				},
				Audit: []fixAudit{
					{Operator: "op", Time: "2026-10-02T12:00:00Z", Revision: 1,
						Before: json.RawMessage(`{}`),
						After:  json.RawMessage(`{"wifi":{"ssid":"a","chan":1},"mode":"x"}`)},
				},
			},
		}}
	}

	cases := map[string]func(fixStore){
		// 实际差异缺少时间记录
		"diff path missing time": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.DiffSince = nil
			doc.Devices["dev-1"] = d
		},
		// 差异路径的时间为零值
		"diff path zero time": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.DiffSince = map[string]string{"/wifi/ssid": "0001-01-01T00:00:00Z"}
			doc.Devices["dev-1"] = d
		},
		// 残留已经没有差异的路径（/mode 双方相等）
		"stale path without diff": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.DiffSince = map[string]string{
				"/wifi/ssid": "2026-10-02T12:00:00Z",
				"/mode":      "2026-10-02T11:00:00Z",
			}
			doc.Devices["dev-1"] = d
		},
		// 父路径的时间不能替代子路径的时间
		"parent path cannot cover child": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.DiffSince = map[string]string{"/wifi": "2026-10-02T12:00:00Z"}
			doc.Devices["dev-1"] = d
		},
		// 记录条数相同但路径不同
		"same count different path": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.DiffSince = map[string]string{"/wifi/chan": "2026-10-02T12:00:00Z"}
			doc.Devices["dev-1"] = d
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			doc := valid()
			mutate(doc)
			dir, original := writeFixStore(t, doc)
			s, err := Open(dir)
			if err == nil {
				s.Close()
				t.Fatalf("expected ErrCorruptStorage")
			}
			if !errors.Is(err, ErrCorruptStorage) {
				t.Fatalf("error must wrap ErrCorruptStorage: %v", err)
			}
			// 失败时原存储文件内容保持不变
			data, rerr := os.ReadFile(filepath.Join(dir, storeFileName))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(data) != string(original) {
				t.Fatalf("corrupt file modified on rejected open")
			}
		})
	}
}

// 一台设备的计时记录矛盾即拒绝打开整个存储，不能跳过它只打开其余设备。
func TestRestoreDiffSinceOneDeviceCorruptRejectsAll(t *testing.T) {
	doc := fixStore{Format: 1, Devices: map[string]fixState{
		"good": {
			Version:  "1.0",
			Revision: 1,
			Desired:  json.RawMessage(`{"a":1}`),
			Reported: json.RawMessage(`{}`),
			DiffSince: map[string]string{
				"/a": "2026-10-02T12:00:00Z",
			},
			Audit: []fixAudit{
				{Operator: "op", Time: "2026-10-02T12:00:00Z", Revision: 1,
					Before: json.RawMessage(`{}`), After: json.RawMessage(`{"a":1}`)},
			},
		},
		"bad": {
			Version:  "1.0",
			Revision: 1,
			Desired:  json.RawMessage(`{"b":2}`),
			Reported: json.RawMessage(`{}`),
			// 实际差异 /b 没有时间记录
			Audit: []fixAudit{
				{Operator: "op", Time: "2026-10-02T12:00:00Z", Revision: 1,
					Before: json.RawMessage(`{}`), After: json.RawMessage(`{"b":2}`)},
			},
		},
	}}
	dir, _ := writeFixStore(t, doc)
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("whole store must be refused when one device timing mismatches: %v", err)
	}
}

// 没有任何差异的设备允许不保存时间记录或保存空记录；新登记且双方配置
// 都是空对象的设备能正常打开。
func TestRestoreDiffSinceAbsentOrEmptyWithoutDiff(t *testing.T) {
	doc := fixStore{Format: 1, Devices: map[string]fixState{
		"no-record": {
			Version:  "1.0",
			Desired:  json.RawMessage(`{}`),
			Reported: json.RawMessage(`{}`),
		},
		"empty-record": {
			Version:   "1.0",
			Revision:  1,
			Desired:   json.RawMessage(`{"a":1}`),
			Reported:  json.RawMessage(`{"a":1}`),
			DiffSince: map[string]string{},
			Audit: []fixAudit{
				{Operator: "op", Time: "2026-10-02T12:00:00Z", Revision: 1,
					Before: json.RawMessage(`{}`), After: json.RawMessage(`{"a":1}`)},
			},
		},
	}}
	dir, _ := writeFixStore(t, doc)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("diff-free devices must open: %v", err)
	}
	defer s.Close()
	for _, id := range []string{"no-record", "empty-record"} {
		diffs, err := s.Diff(id)
		if err != nil || len(diffs) != 0 {
			t.Fatalf("%s diff: %v %+v", id, err, diffs)
		}
	}
}

// 字段名含斜杠、波浪号时，计时记录的路径键沿用 Diff 的 RFC 6901 转义：
// 记录必须写在转义后的路径上，未转义的写法视为不一致。
func TestRestoreDiffSinceEscapedPaths(t *testing.T) {
	doc := fixStore{Format: 1, Devices: map[string]fixState{
		"dev-1": {
			Version:  "1.0",
			Revision: 1,
			Desired:  json.RawMessage(`{"a/b":1,"~x":2}`),
			Reported: json.RawMessage(`{"a/b":9,"~x":8}`),
			DiffSince: map[string]string{
				"/a~1b": "2026-10-02T12:00:00Z",
				"/~0x":  "2026-10-02T12:01:00Z",
			},
			Audit: []fixAudit{
				{Operator: "op", Time: "2026-10-02T12:00:00Z", Revision: 1,
					Before: json.RawMessage(`{}`), After: json.RawMessage(`{"a/b":1,"~x":2}`)},
			},
		},
	}}
	dir, _ := writeFixStore(t, doc)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("escaped timing paths must open: %v", err)
	}
	diffs, err := s.Diff("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 2 || diffs[0].Path != "/a~1b" || diffs[1].Path != "/~0x" {
		t.Fatalf("diff paths: %+v", diffs)
	}
	s.Close()

	// 未转义的路径键与实际差异路径不符，按损坏处理
	d := doc.Devices["dev-1"]
	d.DiffSince = map[string]string{
		"/a/b": "2026-10-02T12:00:00Z",
		"/~0x": "2026-10-02T12:01:00Z",
	}
	doc.Devices["dev-1"] = d
	dir, _ = writeFixStore(t, doc)
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("unescaped timing path must be refused: %v", err)
	}
}

// 合法存储打开后，差异的首次出现时间按原值恢复：持续不同的路径保留
// 原时间，不会用打开时间覆盖。
func TestRestoreDiffSincePreserved(t *testing.T) {
	doc := fixStore{Format: 1, Devices: map[string]fixState{
		"dev-1": {
			Version:  "1.0",
			Revision: 1,
			Desired:  json.RawMessage(`{"a":1,"b":{"c":2}}`),
			Reported: json.RawMessage(`{"a":9,"b":{"c":3}}`),
			DiffSince: map[string]string{
				"/a":   "2026-10-02T12:00:00Z",
				"/b/c": "2026-10-02T12:05:00Z",
			},
			Audit: []fixAudit{
				{Operator: "op", Time: "2026-10-02T12:00:00Z", Revision: 1,
					Before: json.RawMessage(`{}`), After: json.RawMessage(`{"a":1,"b":{"c":2}}`)},
			},
		},
	}}
	dir, _ := writeFixStore(t, doc)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("valid timing records must open: %v", err)
	}
	defer s.Close()
	diffs, err := s.Diff("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 2 || diffs[0].Path != "/a" || diffs[1].Path != "/b/c" {
		t.Fatalf("diff order/content changed: %+v", diffs)
	}
	if diffs[0].Since.Format("2006-01-02T15:04:05Z07:00") != "2026-10-02T12:00:00Z" ||
		diffs[1].Since.Format("2006-01-02T15:04:05Z07:00") != "2026-10-02T12:05:00Z" {
		t.Fatalf("since times not preserved: %+v", diffs)
	}
}
