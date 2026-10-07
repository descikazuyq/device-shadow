package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// diffSinceDoc 构造一台唯一差异在 /wifi/ssid 的设备：/mode 双方相等，
// 时间记录与差异路径一一对应，审计链完整。设备已经有过一次有效上报
// （lastSeq 为 1、上报时间非零），各用例在此基础上篡改计时记录。
func diffSinceDoc() fixStore {
	at := "2026-10-02T12:00:00Z"
	return fixStore{Format: 1, Devices: map[string]fixState{
		"dev-1": {
			Version:        "1.0",
			Online:         true,
			Revision:       1,
			Desired:        json.RawMessage(`{"wifi":{"ssid":"a"},"mode":"x"}`),
			Reported:       json.RawMessage(`{"wifi":{"ssid":"b"},"mode":"x"}`),
			LastSeq:        1,
			LastReportTime: at,
			DiffSince:      map[string]string{"/wifi/ssid": at},
			Audit: []fixAudit{
				{Operator: "op", Time: at, Revision: 1, Before: json.RawMessage(`{}`),
					After: json.RawMessage(`{"wifi":{"ssid":"a"},"mode":"x"}`)},
			},
		},
	}}
}

// 合法计时记录原样打开：差异值、存在性、排列次序和首次出现时间全部保留；
// 对象字段顺序与等值数字不产生差异；没有差异的设备不保存记录或保存空记录
// 都可以；新登记且双方配置均为空对象的设备同样正常打开。
func TestRestoreDiffSinceAccepted(t *testing.T) {
	at := "2026-10-02T12:00:00Z"
	t1 := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 10, 2, 12, 5, 0, 0, time.UTC)

	t.Run("paths values order and times preserved", func(t *testing.T) {
		doc := fixStore{Format: 1, Devices: map[string]fixState{
			"dev-1": {
				Version:  "1.0",
				Online:   true,
				Revision: 1,
				// 两处差异：/a 与 /wifi/ssid；字段顺序刻意与上报侧不同。
				Desired:        json.RawMessage(`{"wifi":{"ssid":"a"},"a":1}`),
				Reported:       json.RawMessage(`{"a":2,"wifi":{"ssid":"b"}}`),
				LastSeq:        1,
				LastReportTime: at,
				DiffSince:      map[string]string{"/wifi/ssid": at, "/a": "2026-10-02T12:05:00Z"},
				Audit: []fixAudit{
					{Operator: "op", Time: at, Revision: 1, Before: json.RawMessage(`{}`),
						After: json.RawMessage(`{"wifi":{"ssid":"a"},"a":1}`)},
				},
			},
		}}
		dir, _ := writeFixStore(t, doc)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("valid diff times must open: %v", err)
		}
		defer s.Close()
		diffs, err := s.Diff("dev-1")
		if err != nil {
			t.Fatal(err)
		}
		if len(diffs) != 2 || diffs[0].Path != "/a" || diffs[1].Path != "/wifi/ssid" {
			t.Fatalf("diff paths/order: %+v", diffs)
		}
		if !diffs[0].DesiredExists || !diffs[0].ReportedExists ||
			string(diffs[0].Desired) != "1" || string(diffs[0].Reported) != "2" {
			t.Fatalf("/a entry: %+v", diffs[0])
		}
		if string(diffs[1].Desired) != `"a"` || string(diffs[1].Reported) != `"b"` {
			t.Fatalf("/wifi/ssid entry: %+v", diffs[1])
		}
		if !diffs[0].Since.Equal(t2) || !diffs[1].Since.Equal(t1) {
			t.Fatalf("first-seen times changed: %v (%v), %v (%v)",
				diffs[0].Since, t2, diffs[1].Since, t1)
		}
	})

	// 路径集合与对外 Diff 完全一致：数组整体比较、字段缺失与 null 有别、
	// 一侧整个字段缺失只列一处，斜杠与波浪号按 RFC 6901 转义。
	t.Run("paths follow diff semantics", func(t *testing.T) {
		desired := `{"arr":[1,2],"nul":null,"onlyDesired":{"x":1},"a/b":"d","w~z":"d"}`
		reported := `{"arr":[1,2,3],"onlyReported":5,"a/b":"r","w~z":"r"}`
		want := []string{"/arr", "/a~1b", "/nul", "/onlyDesired", "/onlyReported", "/w~0z"}
		since := map[string]string{}
		for _, p := range want {
			since[p] = at
		}
		doc := fixStore{Format: 1, Devices: map[string]fixState{
			"dev-1": {
				Version:        "1.0",
				Online:         true,
				Revision:       1,
				Desired:        json.RawMessage(desired),
				Reported:       json.RawMessage(reported),
				LastSeq:        1,
				LastReportTime: at,
				DiffSince:      since,
				Audit: []fixAudit{
					{Operator: "op", Time: at, Revision: 1, Before: json.RawMessage(`{}`),
						After: json.RawMessage(desired)},
				},
			},
		}}
		dir, _ := writeFixStore(t, doc)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("escaped/array/null diff paths must open: %v", err)
		}
		defer s.Close()
		diffs, _ := s.Diff("dev-1")
		if len(diffs) != len(want) {
			t.Fatalf("diff count: %+v", diffs)
		}
		for i, p := range want {
			if diffs[i].Path != p {
				t.Fatalf("diff[%d]: want %s got %s", i, p, diffs[i].Path)
			}
			if diffs[i].Since.IsZero() {
				t.Fatalf("diff %s restored zero time", p)
			}
		}
	})

	t.Run("semantic equality needs no record", func(t *testing.T) {
		doc := fixStore{Format: 1, Devices: map[string]fixState{
			"dev-1": {
				Version:  "1.0",
				Online:   true,
				Revision: 1,
				// 字段顺序不同、1 与 1.0 等值：无差异，无计时记录。
				Desired:        json.RawMessage(`{"a":1,"b":{}}`),
				Reported:       json.RawMessage(`{"b":{},"a":1.0}`),
				LastSeq:        1,
				LastReportTime: at,
				Audit: []fixAudit{
					{Operator: "op", Time: at, Revision: 1, Before: json.RawMessage(`{}`),
						After: json.RawMessage(`{"a":1,"b":{}}`)},
				},
			},
		}}
		dir, _ := writeFixStore(t, doc)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("semantic-equal configs with no diff times must open: %v", err)
		}
		defer s.Close()
		if diffs, _ := s.Diff("dev-1"); len(diffs) != 0 {
			t.Fatalf("unexpected diffs: %+v", diffs)
		}
	})

	t.Run("new empty device and explicit empty record", func(t *testing.T) {
		// 走正常流程登记：双方均为空对象，无差异。
		s, dir := openTemp(t)
		mustRegister(t, s, "dev-1", "1.0")
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("new empty-object device must reopen: %v", err)
		}
		s2.Close()

		// 手写一份显式保存空计时记录的无差异存储，同样必须能打开。
		dir2 := t.TempDir()
		raw := `{"format":1,"devices":{"dev-1":{"version":"1.0","revision":0,` +
			`"desired":{},"reported":{},"diffSince":{}}}}`
		if err := os.WriteFile(filepath.Join(dir2, storeFileName), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		s3, err := Open(dir2)
		if err != nil {
			t.Fatalf("empty diffSince record must open: %v", err)
		}
		defer s3.Close()
		if diffs, _ := s3.Diff("dev-1"); len(diffs) != 0 {
			t.Fatalf("unexpected diffs: %+v", diffs)
		}
	})
}

// 实际差异缺时间、残留无差异路径、时间为零、父路径代替子路径、
// 条数相同但路径不同：都必须返回 ErrCorruptStorage 拒绝打开整个存储，
// 且原文件内容保持不变，不补当前时间、不删多余记录、不重写配置。
func TestRestoreDiffSinceCorruptRefused(t *testing.T) {
	cases := map[string]func(fixStore){
		// 实际存在 /wifi/ssid 差异，却完全没有计时记录。
		"missing time record": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.DiffSince = nil
			doc.Devices["dev-1"] = d
		},
		// 已相等的 /mode 仍残留时间记录。
		"stale record for equal path": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.DiffSince["/mode"] = "2026-10-02T12:00:00Z"
			doc.Devices["dev-1"] = d
		},
		// 只残留无关路径，实际差异反而没有记录。
		"only stale record": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.DiffSince = map[string]string{"/mode": "2026-10-02T12:00:00Z"}
			doc.Devices["dev-1"] = d
		},
		// 路径正确但时间为零值。
		"zero first-seen time": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.DiffSince = map[string]string{"/wifi/ssid": "0001-01-01T00:00:00Z"}
			doc.Devices["dev-1"] = d
		},
		// 父路径 /wifi 的时间不能替代子路径 /wifi/ssid（条数相同）。
		"parent path cannot cover child": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.DiffSince = map[string]string{"/wifi": "2026-10-02T12:00:00Z"}
			doc.Devices["dev-1"] = d
		},
		// 记录条数与差异条数相同，但路径对不上。
		"same count different path": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.DiffSince = map[string]string{"/wifi/chan": "2026-10-02T12:00:00Z"}
			doc.Devices["dev-1"] = d
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			doc := diffSinceDoc()
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
			data, rerr := os.ReadFile(filepath.Join(dir, storeFileName))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if string(data) != string(original) {
				t.Fatalf("corrupt file modified on rejected open")
			}
		})
	}

	// 没有差异的设备保存了时间记录同样损坏。
	t.Run("record on device without diffs", func(t *testing.T) {
		doc := fixStore{Format: 1, Devices: map[string]fixState{
			"dev-1": {
				Version:        "1.0",
				Online:         true,
				Revision:       1,
				Desired:        json.RawMessage(`{"a":1}`),
				Reported:       json.RawMessage(`{"a":1}`),
				LastSeq:        1,
				LastReportTime: "2026-10-02T12:00:00Z",
				DiffSince:      map[string]string{"/a": "2026-10-02T12:00:00Z"},
				Audit: []fixAudit{
					{Operator: "op", Time: "2026-10-02T12:00:00Z", Revision: 1,
						Before: json.RawMessage(`{}`), After: json.RawMessage(`{"a":1}`)},
				},
			},
		}}
		dir, original := writeFixStore(t, doc)
		if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
			t.Fatalf("stale time on equal device must be refused: %v", err)
		}
		if data, _ := os.ReadFile(filepath.Join(dir, storeFileName)); string(data) != string(original) {
			t.Fatalf("corrupt file modified on rejected open")
		}
	})
}

// 一台设备的计时记录损坏即拒绝打开整个存储，其他设备再正常也不可只跳过坏设备。
func TestRestoreDiffSinceOneBadDeviceRejectsAll(t *testing.T) {
	doc := diffSinceDoc() // dev-1 合法
	doc.Devices["good"] = fixState{
		Version:  "1.0",
		Desired:  json.RawMessage(`{}`),
		Reported: json.RawMessage(`{}`),
	}
	bad := diffSinceDoc().Devices["dev-1"]
	bad.DiffSince = nil // dev-2 有 /wifi/ssid 差异但缺时间记录
	doc.Devices["bad"] = bad
	dir, _ := writeFixStore(t, doc)
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("whole store must be refused when one device lacks diff times: %v", err)
	}
}

// 跨关闭重开的计时生命周期：同一路径持续不同保留原时间；差异消失后再次出现
// 才重新计时，且恢复出的计时记录与正常流程产生的记录行为一致。
func TestRestoreDiffSinceLifecycleAcrossReopen(t *testing.T) {
	t1 := base
	s, dir := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	if _, err := s.UpdateDesired("dev-1", "op", t1, 0, json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 重开后同一路径仍不同：保留首次时间。
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen with diff: %v", err)
	}
	if diffs, _ := s2.Diff("dev-1"); len(diffs) != 1 || !diffs[0].Since.Equal(t1) {
		t.Fatalf("diff time must survive reopen: %+v", diffs)
	}
	// 重开后继续改值但仍不相等：沿用原时间。
	t2 := base.Add(time.Minute)
	if _, err := s2.UpdateDesired("dev-1", "op", t2, 1, json.RawMessage(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	if diffs, _ := s2.Diff("dev-1"); !diffs[0].Since.Equal(t1) {
		t.Fatalf("persistent diff must keep original time: %+v", diffs)
	}
	// 差异消失，关闭重开：无差异、无计时记录仍合法。
	t3 := base.Add(2 * time.Minute)
	if err := s2.Report("dev-1", 1, t3, "1.0", json.RawMessage(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	s3, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen without diffs: %v", err)
	}
	if diffs, _ := s3.Diff("dev-1"); len(diffs) != 0 {
		t.Fatalf("cleared diff must stay cleared: %+v", diffs)
	}
	// 同一路径再次出现差异：必须重新计时，不能沿用消失前的 t1。
	t4 := base.Add(3 * time.Minute)
	if _, err := s3.UpdateDesired("dev-1", "op", t4, 2, json.RawMessage(`{"a":3}`)); err != nil {
		t.Fatal(err)
	}
	if err := s3.Close(); err != nil {
		t.Fatal(err)
	}
	s4, err := Open(dir)
	if err != nil {
		t.Fatalf("final reopen: %v", err)
	}
	defer s4.Close()
	diffs, _ := s4.Diff("dev-1")
	if len(diffs) != 1 || diffs[0].Path != "/a" || !diffs[0].Since.Equal(t4) {
		t.Fatalf("reappeared diff must restart timing at t4: %+v", diffs)
	}
}
