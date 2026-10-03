package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 以下结构仅用于在测试中拼装存储文件，字段与磁盘格式对应。
type fixAudit struct {
	Operator  string          `json:"operator"`
	Time      string          `json:"time"`
	Revision  uint64          `json:"revision"`
	RequestID string          `json:"requestId,omitempty"`
	Before    json.RawMessage `json:"before"`
	After     json.RawMessage `json:"after"`
}

type fixState struct {
	Version        string     `json:"version"`
	Online         bool       `json:"online"`
	Revision       uint64     `json:"revision"`
	Desired        any        `json:"desired"`
	Reported       any        `json:"reported"`
	LastSeq        uint64     `json:"lastSeq,omitempty"`
	LastReportTime string     `json:"lastReportTime,omitempty"`
	Audit          []fixAudit `json:"audit,omitempty"`
}

type fixStore struct {
	Format  int                 `json:"format"`
	Devices map[string]fixState `json:"devices"`
}

// writeFixStore 写入一份拼装存储，返回目录与写入的原始字节。
func writeFixStore(t *testing.T, doc fixStore) (string, []byte) {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, storeFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, data
}

func twoAuditDoc() fixStore {
	return fixStore{Format: 1, Devices: map[string]fixState{
		"dev-1": {
			Version:  "1.0",
			Revision: 2,
			Desired:  json.RawMessage(`{"a":2}`),
			Reported: json.RawMessage(`{}`),
			Audit: []fixAudit{
				{Operator: "alice", Time: "2026-10-02T12:00:00Z", Revision: 1, Before: json.RawMessage(`{}`), After: json.RawMessage(`{"a":1}`)},
				{Operator: "bob", Time: "2026-10-02T12:01:00Z", Revision: 2, Before: json.RawMessage(`{"a":1}`), After: json.RawMessage(`{"a":2}`)},
			},
		},
	}}
}

// 正常的两次修改历史：修订号 1、2 衔接，末条修改后等于当前期望配置。
func TestRestoreAuditChainAccepted(t *testing.T) {
	dir, _ := writeFixStore(t, twoAuditDoc())
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("valid audit chain must open: %v", err)
	}
	defer s.Close()
	v, err := s.Get("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Revision != 2 || string(v.Desired) != `{"a":2}` {
		t.Fatalf("restored view: %+v", v)
	}
	// Get 与 Audit 保留已有内容和顺序
	recs, err := s.Audit("dev-1")
	if err != nil || len(recs) != 2 {
		t.Fatalf("restored audit: %v %v", recs, err)
	}
	if recs[0].Revision != 1 || recs[1].Revision != 2 ||
		recs[0].Operator != "alice" || recs[1].Operator != "bob" {
		t.Fatalf("audit content/order changed: %+v", recs)
	}
}

// 从未修改过期望配置的设备：修订号 0、空审计、空对象期望配置。
// 设备已经上报其他配置或版本不影响这一判断。
func TestRestoreZeroRevisionAccepted(t *testing.T) {
	dir, _ := writeFixStore(t, fixStore{Format: 1, Devices: map[string]fixState{
		"dev-1": {
			Version:        "1.1",
			Online:         true,
			Revision:       0,
			Desired:        json.RawMessage(`{}`),
			Reported:       json.RawMessage(`{"cpu":80}`),
			LastSeq:        7,
			LastReportTime: "2026-10-02T12:00:00Z",
		},
	}})
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("reported config must not affect revision-zero device: %v", err)
	}
	defer s.Close()
	v, _ := s.Get("dev-1")
	if v.Revision != 0 || string(v.Desired) != "{}" || v.LastSeq != 7 || v.Version != "1.1" {
		t.Fatalf("restored view: %+v", v)
	}
	if recs, _ := s.Audit("dev-1"); len(recs) != 0 {
		t.Fatalf("unexpected audit: %+v", recs)
	}
}

// 配置比较沿用 JSON 语义：空白与对象字段顺序无关，1 与 1.0 相等；
// 记录只按修订号衔接，不要求操作时间严格递增。
func TestRestoreAuditChainSemanticEquality(t *testing.T) {
	dir, _ := writeFixStore(t, fixStore{Format: 1, Devices: map[string]fixState{
		"dev-1": {
			Version:  "1.0",
			Revision: 2,
			Desired:  json.RawMessage(`{ "a": 1.0 }`),
			Reported: json.RawMessage(`{}`),
			Audit: []fixAudit{
				// 时间晚于第二条；after 用 1 与期望中的 1.0 按数值比较
				{Operator: "alice", Time: "2026-10-02T12:05:00Z", Revision: 1, Before: json.RawMessage(`{ }`), After: json.RawMessage(`{"a":1}`)},
				// before 字段顺序、空白不同，语义仍等于上一条 after
				{Operator: "bob", Time: "2026-10-02T12:00:00Z", Revision: 2, Before: json.RawMessage(`{ "a" : 1 }`), After: json.RawMessage(`{"a":1.0}`)},
			},
		},
	}})
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("semantic-equal chain with non-monotonic times must open: %v", err)
	}
	defer s.Close()
}

// 提交相同配置同样连续产生修订号和审计，不能因前后配置相等而视为损坏。
func TestRestoreSameConfigCommitsAccepted(t *testing.T) {
	s, dir := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	if _, err := s.UpdateDesired("dev-1", "op", base, 0, json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDesired("dev-1", "op", base.Add(time.Minute), 1, json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDesired("dev-1", "op", base.Add(2*time.Minute), 2, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("equal-config revisions must reopen: %v", err)
	}
	defer s2.Close()
	recs, _ := s2.Audit("dev-1")
	if len(recs) != 3 {
		t.Fatalf("audit: %+v", recs)
	}
	v, _ := s2.Get("dev-1")
	if v.Revision != 3 || string(v.Desired) != "{}" {
		t.Fatalf("view: %+v", v)
	}
}

// 历史中交替出现单设备修改与批量修改：较早的批量请求只对应当时那条审计，
// 重开后链式校验与批量查询都仍以各自修订号为准。
func TestRestoreInterleavedBatchAndSingle(t *testing.T) {
	s, dir := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustBatch(t, s, "req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
	})
	if _, err := s.UpdateDesired("dev-1", "bob", base.Add(time.Hour), 1, json.RawMessage(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	mustBatch(t, s, "req-2", "alice", base.Add(2*time.Hour), []BatchDevice{
		{DeviceID: "dev-1", Revision: 2, Config: json.RawMessage(`{"a":3}`)},
	})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("interleaved history must reopen: %v", err)
	}
	defer s2.Close()
	recs, _ := s2.Audit("dev-1")
	if len(recs) != 3 || recs[0].RequestID != "req-1" || recs[1].RequestID != "" || recs[2].RequestID != "req-2" {
		t.Fatalf("audit order/markers: %+v", recs)
	}
	// 较早批量请求仍记录当时的新修订号 1，不能拿设备最新配置比较
	rec, err := s2.GetBatchRequest("req-1")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Revisions["dev-1"] != 1 {
		t.Fatalf("old batch record: %+v", rec.Revisions)
	}
	v, _ := s2.Get("dev-1")
	if v.Revision != 3 || string(v.Desired) != `{"a":3}` {
		t.Fatalf("latest view: %+v", v)
	}
}

// 任一设备历史缺失、断开或与当前期望不符都拒绝打开整个存储，
// 且原文件内容保持不变。
func TestRestoreAuditChainCorruptRefused(t *testing.T) {
	op := "op"
	t1 := "2026-10-02T12:00:00Z"
	t2 := "2026-10-02T12:01:00Z"
	empty := json.RawMessage(`{}`)
	a1 := json.RawMessage(`{"a":1}`)
	a2 := json.RawMessage(`{"a":2}`)
	a3 := json.RawMessage(`{"a":3}`)
	a9 := json.RawMessage(`{"a":9}`)
	x1 := json.RawMessage(`{"x":1}`)

	cases := map[string]func(fixStore){
		// 删掉修订号 1 的审计，只留修订号 2 与当前影子
		"first audit missing": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.Audit = d.Audit[1:]
			doc.Devices["dev-1"] = d
		},
		// 修订号断开：1 之后直接是 3
		"revision gap": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.Audit[1].Revision = 3
			doc.Devices["dev-1"] = d
		},
		// 审计未从修订号 1 开始（条数与修订号相符，但首条修订号为 2）
		"audit not from one": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.Audit = []fixAudit{
				{Operator: op, Time: t1, Revision: 2, Before: empty, After: a1},
				{Operator: op, Time: t2, Revision: 3, Before: a1, After: a2},
			}
			doc.Devices["dev-1"] = d
		},
		// 首条修改前不是登记时的空对象
		"first before not empty": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.Revision = 1
			d.Desired = a1
			d.Audit = []fixAudit{
				{Operator: op, Time: t1, Revision: 1, Before: x1, After: a1},
			}
			doc.Devices["dev-1"] = d
		},
		// 相邻记录断开：第二条修改前不等于第一条修改后
		"chain broken": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.Audit[1].Before = a9
			doc.Devices["dev-1"] = d
		},
		// 末条修改后与当前期望配置不同
		"last after mismatches desired": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.Desired = a3
			doc.Devices["dev-1"] = d
		},
		// 当前修订号大于审计条数：历史解释不了现状
		"revision beyond audit": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.Revision = 3
			doc.Devices["dev-1"] = d
		},
		// 有审计却声称修订号 0
		"audit with revision zero": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.Revision = 0
			d.Desired = empty
			doc.Devices["dev-1"] = d
		},
		// 修订号 0、无审计，但期望配置不是空对象
		"desired not empty at revision zero": func(doc fixStore) {
			d := doc.Devices["dev-1"]
			d.Revision = 0
			d.Desired = a1
			d.Audit = nil
			doc.Devices["dev-1"] = d
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			doc := twoAuditDoc()
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
			// 原文件内容保持不变
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

// 一台设备损坏即拒绝打开整个存储，其他正常设备也不可用。
func TestRestoreOneDeviceCorruptRejectsAll(t *testing.T) {
	doc := twoAuditDoc()
	doc.Devices["good"] = fixState{
		Version:  "1.0",
		Desired:  json.RawMessage(`{}`),
		Reported: json.RawMessage(`{}`),
	}
	bad := doc.Devices["dev-1"]
	bad.Audit = []fixAudit{
		{Operator: "op", Time: "2026-10-02T12:00:00Z", Revision: 1, Before: json.RawMessage(`{}`), After: json.RawMessage(`{"a":2}`)},
	}
	doc.Devices["bad"] = bad
	delete(doc.Devices, "dev-1")
	dir, _ := writeFixStore(t, doc)
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("whole store must be refused when one device history mismatches: %v", err)
	}
}
