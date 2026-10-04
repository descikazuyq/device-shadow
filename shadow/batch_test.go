package shadow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func mustBatch(t *testing.T, s *Store, reqID, op string, at time.Time, devices []BatchDevice) BatchRecord {
	t.Helper()
	rec, err := s.BatchUpdateDesired(reqID, op, at, devices)
	if err != nil {
		t.Fatalf("BatchUpdateDesired(%s): %v", reqID, err)
	}
	return rec
}

func TestBatchUpdateDesiredBasic(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustRegister(t, s, "dev-2", "2.0")
	// dev-2 已有一次单设备修改，修订号从 1 继续
	if _, err := s.UpdateDesired("dev-2", "op", base, 0, json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}

	rec := mustBatch(t, s, "req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"wifi":{"ssid":"a"},"n":1}`)},
		{DeviceID: "dev-2", Revision: 1, Config: json.RawMessage(`{"x":1}`)}, // 新旧配置相同也加一
	})
	if rec.RequestID != "req-1" || rec.Operator != "alice" || !rec.Time.Equal(base) {
		t.Fatalf("record meta: %+v", rec)
	}
	if rec.Revisions["dev-1"] != 1 || rec.Revisions["dev-2"] != 2 || len(rec.Revisions) != 2 {
		t.Fatalf("revisions: %+v", rec.Revisions)
	}

	v1, _ := s.Get("dev-1")
	if v1.Revision != 1 || string(v1.Desired) != `{"wifi":{"ssid":"a"},"n":1}` {
		t.Fatalf("dev-1 view: %+v", v1)
	}
	v2, _ := s.Get("dev-2")
	if v2.Revision != 2 || string(v2.Desired) != `{"x":1}` {
		t.Fatalf("dev-2 view: %+v", v2)
	}
	// 批量修改不影响版本、在线状态与上报配置
	if v1.Version != "1.0" || v1.Online || string(v1.Reported) != "{}" {
		t.Fatalf("dev-1 side effects: %+v", v1)
	}

	// 审计带同一请求标识，仍按修订号顺序查询
	a1, _ := s.Audit("dev-1")
	if len(a1) != 1 || a1[0].RequestID != "req-1" || a1[0].Operator != "alice" ||
		!a1[0].Time.Equal(base) || a1[0].Revision != 1 ||
		string(a1[0].Before) != `{}` || string(a1[0].After) != `{"wifi":{"ssid":"a"},"n":1}` {
		t.Fatalf("dev-1 audit: %+v", a1)
	}
	a2, _ := s.Audit("dev-2")
	if len(a2) != 2 || a2[0].RequestID != "" || a2[1].RequestID != "req-1" || a2[1].Revision != 2 {
		t.Fatalf("dev-2 audit: %+v", a2)
	}

	// 差异按现有规则：新增路径使用本次提交时间
	diffs, _ := s.Diff("dev-1")
	if len(diffs) != 2 || diffs[0].Path != "/n" || diffs[1].Path != "/wifi" {
		t.Fatalf("diff: %+v", diffs)
	}
	for _, d := range diffs {
		if !d.Since.Equal(base) {
			t.Fatalf("diff since: %+v", d)
		}
	}

	// 查询成功提交记录
	got, err := s.GetBatchRequest("req-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Operator != "alice" || !got.Time.Equal(base) ||
		got.Revisions["dev-1"] != 1 || got.Revisions["dev-2"] != 2 {
		t.Fatalf("GetBatchRequest: %+v", got)
	}
}

func TestBatchValidation(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustRegister(t, s, "dev-2", "1.0")
	good := []BatchDevice{{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)}}

	cases := []struct {
		name    string
		reqID   string
		op      string
		at      time.Time
		devices []BatchDevice
		want    error
	}{
		{"empty request id", "", "op", base, good, ErrInvalidRequestID},
		{"empty operator", "r1", "", base, good, ErrInvalidOperator},
		{"zero time", "r1", "op", time.Time{}, good, ErrInvalidTime},
		{"empty device list", "r1", "op", base, nil, ErrInvalidDeviceList},
		{"empty device id", "r1", "op", base, []BatchDevice{{DeviceID: "", Revision: 0, Config: json.RawMessage(`{}`)}}, ErrInvalidDeviceID},
		{"duplicate device", "r1", "op", base, []BatchDevice{
			{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{}`)},
			{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{}`)},
		}, ErrDuplicateDevice},
		{"unknown device", "r1", "op", base, []BatchDevice{{DeviceID: "ghost", Revision: 0, Config: json.RawMessage(`{}`)}}, ErrDeviceNotFound},
		{"bad config", "r1", "op", base, []BatchDevice{{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`[1]`)}}, ErrInvalidConfig},
		{"revision conflict", "r1", "op", base, []BatchDevice{{DeviceID: "dev-1", Revision: 3, Config: json.RawMessage(`{}`)}}, ErrRevisionConflict},
	}
	for _, tc := range cases {
		if _, err := s.BatchUpdateDesired(tc.reqID, tc.op, tc.at, tc.devices); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
		}
		// 失败不占用请求标识
		reqID := tc.reqID
		if reqID == "" {
			reqID = "unused"
		}
		if _, err := s.GetBatchRequest(reqID); !errors.Is(err, ErrRequestNotFound) {
			t.Fatalf("%s: request id consumed: %v", tc.name, err)
		}
	}
	// 所有设备状态与审计均不变
	for _, id := range []string{"dev-1", "dev-2"} {
		v, _ := s.Get(id)
		if v.Revision != 0 || string(v.Desired) != "{}" {
			t.Fatalf("%s changed: %+v", id, v)
		}
		if recs, _ := s.Audit(id); len(recs) != 0 {
			t.Fatalf("%s audit changed: %+v", id, recs)
		}
	}

	// 一台设备修订号冲突时整批失败，其他设备不被修改，请求标识不占用
	_, err := s.BatchUpdateDesired("req-x", "op", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
		{DeviceID: "dev-2", Revision: 9, Config: json.RawMessage(`{"b":2}`)},
	})
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("batch conflict: %v", err)
	}
	v1, _ := s.Get("dev-1")
	if v1.Revision != 0 || string(v1.Desired) != "{}" {
		t.Fatalf("dev-1 modified by failed batch: %+v", v1)
	}
	if _, err := s.GetBatchRequest("req-x"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("failed request queryable: %v", err)
	}
	// 同一标识修正后可重新提交
	if _, err := s.BatchUpdateDesired("req-x", "op", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"b":2}`)},
	}); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
}

func TestBatchIdempotentResubmit(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustRegister(t, s, "dev-2", "1.0")

	first := mustBatch(t, s, "req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1,"list":[1,2],"obj":{"x":1,"y":2}}`)},
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"b":2}`)},
	})

	// 设备换序、JSON 空白与对象字段顺序变化、数字 1.0 与 1 相等、
	// 提交时间同一时刻不同时区：均视为相同内容
	sameInstant := base.In(time.FixedZone("UTC+8", 8*3600))
	second := mustBatch(t, s, "req-1", "alice", sameInstant, []BatchDevice{
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{ "b": 2.0 }`)},
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"obj":{"y":2,"x":1},"list":[1,2],"a":1.0}`)},
	})
	if second.Revisions["dev-1"] != first.Revisions["dev-1"] ||
		second.Revisions["dev-2"] != first.Revisions["dev-2"] {
		t.Fatalf("resubmit revisions: %+v vs %+v", second.Revisions, first.Revisions)
	}
	// 不重复修改、不增加审计
	for _, id := range []string{"dev-1", "dev-2"} {
		v, _ := s.Get(id)
		if v.Revision != 1 {
			t.Fatalf("%s revision after resubmit: %d", id, v.Revision)
		}
		if recs, _ := s.Audit(id); len(recs) != 1 {
			t.Fatalf("%s audit after resubmit: %+v", id, recs)
		}
	}

	// 设备后来已有新修改，旧请求重发仍返回首次结果，不覆盖新状态
	if _, err := s.UpdateDesired("dev-1", "bob", base.Add(time.Hour), 1, json.RawMessage(`{"new":true}`)); err != nil {
		t.Fatal(err)
	}
	third := mustBatch(t, s, "req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1,"list":[1,2],"obj":{"x":1,"y":2}}`)},
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"b":2}`)},
	})
	if third.Revisions["dev-1"] != 1 || third.Revisions["dev-2"] != 1 {
		t.Fatalf("stale resubmit overwrote: %+v", third.Revisions)
	}
	v1, _ := s.Get("dev-1")
	if v1.Revision != 2 || string(v1.Desired) != `{"new":true}` {
		t.Fatalf("dev-1 overwritten by stale request: %+v", v1)
	}
	if recs, _ := s.Audit("dev-1"); len(recs) != 2 {
		t.Fatalf("dev-1 audit grew on resubmit: %+v", recs)
	}
}

func TestBatchRequestConflict(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustRegister(t, s, "dev-2", "1.0")
	original := []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1,"list":[1,2],"n":null}`)},
	}
	first := mustBatch(t, s, "req-1", "alice", base, original)

	cases := []struct {
		name    string
		op      string
		at      time.Time
		devices []BatchDevice
	}{
		{"different operator", "bob", base, original},
		{"different time", "alice", base.Add(time.Second), original},
		{"different device set", "alice", base, []BatchDevice{
			{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"a":1,"list":[1,2],"n":null}`)},
		}},
		{"extra device", "alice", base, append(append([]BatchDevice{}, original...),
			BatchDevice{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{}`)})},
		{"different revision", "alice", base, []BatchDevice{
			{DeviceID: "dev-1", Revision: 1, Config: json.RawMessage(`{"a":1,"list":[1,2],"n":null}`)},
		}},
		{"different config value", "alice", base, []BatchDevice{
			{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":2,"list":[1,2],"n":null}`)},
		}},
		{"array order matters", "alice", base, []BatchDevice{
			{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1,"list":[2,1],"n":null}`)},
		}},
		{"missing field vs null", "alice", base, []BatchDevice{
			{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1,"list":[1,2]}`)},
		}},
	}
	for _, tc := range cases {
		if _, err := s.BatchUpdateDesired("req-1", tc.op, tc.at, tc.devices); !errors.Is(err, ErrRequestConflict) {
			t.Fatalf("%s: got %v, want ErrRequestConflict", tc.name, err)
		}
	}
	// 原结果保持有效且未被修改
	got, err := s.GetBatchRequest("req-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Operator != "alice" || !got.Time.Equal(base) || got.Revisions["dev-1"] != first.Revisions["dev-1"] {
		t.Fatalf("original record changed: %+v", got)
	}
	v, _ := s.Get("dev-1")
	if v.Revision != 1 || string(v.Desired) != `{"a":1,"list":[1,2],"n":null}` {
		t.Fatalf("state changed by conflicts: %+v", v)
	}
	if recs, _ := s.Audit("dev-1"); len(recs) != 1 {
		t.Fatalf("audit changed by conflicts: %+v", recs)
	}
	// 不同标识不受冲突影响
	if _, err := s.BatchUpdateDesired("req-2", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 1, Config: json.RawMessage(`{}`)},
	}); err != nil {
		t.Fatalf("new request id: %v", err)
	}
}

func TestBatchGetRequestNotFound(t *testing.T) {
	s, _ := openTemp(t)
	if _, err := s.GetBatchRequest(""); !errors.Is(err, ErrInvalidRequestID) {
		t.Fatalf("empty id: %v", err)
	}
	if _, err := s.GetBatchRequest("ghost"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
}

func TestBatchOfflineDeviceAccepted(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Report("dev-1", 1, base, "1.1", json.RawMessage(`{"r":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOffline("dev-1"); err != nil {
		t.Fatal(err)
	}
	mustBatch(t, s, "req-1", "op", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"r":1}`)},
	})
	v, _ := s.Get("dev-1")
	if v.Online || v.Version != "1.1" || string(v.Reported) != `{"r":1}` || v.LastSeq != 1 {
		t.Fatalf("offline device side effects: %+v", v)
	}
	// 已一致的路径差异消失
	if diffs, _ := s.Diff("dev-1"); len(diffs) != 0 {
		t.Fatalf("diff after sync: %+v", diffs)
	}
}

func TestBatchConcurrentResubmit(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	const n = 20
	var wg sync.WaitGroup
	results := make(chan BatchRecord, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec, err := s.BatchUpdateDesired("req-1", "op", base, []BatchDevice{
				{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
			})
			if err != nil {
				t.Errorf("concurrent resubmit: %v", err)
				return
			}
			results <- rec
		}()
	}
	wg.Wait()
	close(results)
	for rec := range results {
		if rec.Revisions["dev-1"] != 1 {
			t.Fatalf("concurrent resubmit revision: %+v", rec.Revisions)
		}
	}
	// 只产生一次修改
	v, _ := s.Get("dev-1")
	if v.Revision != 1 {
		t.Fatalf("revision after concurrent resubmit: %d", v.Revision)
	}
	if recs, _ := s.Audit("dev-1"); len(recs) != 1 {
		t.Fatalf("audit after concurrent resubmit: %+v", recs)
	}
}

func TestBatchConcurrentCompetingRequests(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustRegister(t, s, "dev-2", "1.0")
	const n = 10
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := s.BatchUpdateDesired(fmt.Sprintf("req-%d", i), "op", base, []BatchDevice{
				{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(fmt.Sprintf(`{"i":%d}`, i))},
				{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{}`)},
			})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	var ok, conflict int
	for err := range errs {
		if err == nil {
			ok++
		} else if errors.Is(err, ErrRevisionConflict) {
			conflict++
		} else {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("competing batches: ok=%d conflict=%d", ok, conflict)
	}
	// 失败的一批不能修改其他设备：dev-2 只被成功的一批修改过一次
	v2, _ := s.Get("dev-2")
	if v2.Revision != 1 {
		t.Fatalf("dev-2 revision: %d", v2.Revision)
	}
	if recs, _ := s.Audit("dev-2"); len(recs) != 1 {
		t.Fatalf("dev-2 audit: %+v", recs)
	}
}

func TestBatchPersistenceReopen(t *testing.T) {
	s, dir := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustRegister(t, s, "dev-2", "1.0")
	mustBatch(t, s, "req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"b":2}`)},
	})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	// 重开后仍可查询
	got, err := s2.GetBatchRequest("req-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Operator != "alice" || !got.Time.Equal(base) ||
		got.Revisions["dev-1"] != 1 || got.Revisions["dev-2"] != 1 {
		t.Fatalf("restored record: %+v", got)
	}
	// 审计中的请求标识保留
	recs, _ := s2.Audit("dev-1")
	if len(recs) != 1 || recs[0].RequestID != "req-1" {
		t.Fatalf("restored audit: %+v", recs)
	}
	// 重开后去重仍然有效
	again := mustBatch(t, s2, "req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"b":2}`)},
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
	})
	if again.Revisions["dev-1"] != 1 || again.Revisions["dev-2"] != 1 {
		t.Fatalf("dedup after reopen: %+v", again.Revisions)
	}
	if recs, _ := s2.Audit("dev-1"); len(recs) != 1 {
		t.Fatalf("audit grew after reopen resubmit: %+v", recs)
	}
	// 重开后冲突判断仍然有效
	if _, err := s2.BatchUpdateDesired("req-1", "bob", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"b":2}`)},
	}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("conflict after reopen: %v", err)
	}
}

func TestBatchOldStorageOpens(t *testing.T) {
	// 旧存储没有批量记录：正常打开，单设备审计无需补填标识
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, storeFileName), []byte(
		`{"format":1,"devices":{"dev-1":{"version":"1.0","online":false,"revision":1,`+
			`"desired":{"a":1},"reported":{},"lastSeq":0,`+
			`"diffSince":{"/a":"2026-10-02T12:00:00Z"},`+
			`"audit":[{"operator":"op","time":"2026-10-02T12:00:00Z","revision":1,"before":{},"after":{"a":1}}]}}}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	recs, _ := s.Audit("dev-1")
	if len(recs) != 1 || recs[0].RequestID != "" {
		t.Fatalf("old audit: %+v", recs)
	}
	if _, err := s.GetBatchRequest("req-1"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("old storage batch query: %v", err)
	}
	// 旧存储上新的批量功能正常
	mustBatch(t, s, "req-1", "op", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 1, Config: json.RawMessage(`{"a":2}`)},
	})
}

func TestBatchCorruptRecordRefused(t *testing.T) {
	newStore := func(t *testing.T) string {
		s, dir := openTemp(t)
		mustRegister(t, s, "dev-1", "1.0")
		mustBatch(t, s, "req-1", "alice", base, []BatchDevice{
			{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
		})
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	rewrite := func(t *testing.T, dir string, mutate func(map[string]any)) {
		data, err := os.ReadFile(filepath.Join(dir, storeFileName))
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		mutate(doc)
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, storeFileName), out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	batches := func(doc map[string]any) map[string]any {
		return doc["batches"].(map[string]any)
	}

	// 批量记录缺操作者
	dir := newStore(t)
	rewrite(t, dir, func(doc map[string]any) {
		batches(doc)["req-1"].(map[string]any)["operator"] = ""
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("missing operator: %v", err)
	}
	// 批量记录引用未登记设备
	dir = newStore(t)
	rewrite(t, dir, func(doc map[string]any) {
		devs := batches(doc)["req-1"].(map[string]any)["devices"].([]any)
		devs[0].(map[string]any)["deviceId"] = "ghost"
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("unknown device: %v", err)
	}
	// 批量记录配置损坏
	dir = newStore(t)
	rewrite(t, dir, func(doc map[string]any) {
		devs := batches(doc)["req-1"].(map[string]any)["devices"].([]any)
		devs[0].(map[string]any)["config"] = []any{1}
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("bad config: %v", err)
	}
	// 批量记录修订号与设备审计不一致
	dir = newStore(t)
	rewrite(t, dir, func(doc map[string]any) {
		devs := batches(doc)["req-1"].(map[string]any)["devices"].([]any)
		devs[0].(map[string]any)["newRevision"] = 5
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("bad revision: %v", err)
	}
	// 审计带请求标识但批量记录缺失
	dir = newStore(t)
	rewrite(t, dir, func(doc map[string]any) {
		delete(doc, "batches")
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("audit without batch record: %v", err)
	}
}

func TestBatchSaveFailureRollsBack(t *testing.T) {
	s, dir := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustRegister(t, s, "dev-2", "1.0")
	// 用同名目录堵住存储文件路径，使原子替换失败
	if err := os.Remove(filepath.Join(dir, storeFileName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, storeFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := s.BatchUpdateDesired("req-1", "op", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"b":2}`)},
	})
	if err == nil {
		t.Fatal("expected save error")
	}
	// 整批状态、审计和请求记录一起撤销
	for _, id := range []string{"dev-1", "dev-2"} {
		v, _ := s.Get(id)
		if v.Revision != 0 || string(v.Desired) != "{}" {
			t.Fatalf("%s not rolled back: %+v", id, v)
		}
		if recs, _ := s.Audit(id); len(recs) != 0 {
			t.Fatalf("%s audit not rolled back: %+v", id, recs)
		}
	}
	if _, err := s.GetBatchRequest("req-1"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("request record not rolled back: %v", err)
	}
	// 恢复可写后仍可用原标识重试
	if err := os.Remove(filepath.Join(dir, storeFileName)); err != nil {
		t.Fatal(err)
	}
	rec := mustBatch(t, s, "req-1", "op", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"b":2}`)},
	})
	if rec.Revisions["dev-1"] != 1 || rec.Revisions["dev-2"] != 1 {
		t.Fatalf("retry after save failure: %+v", rec.Revisions)
	}
}

func TestBatchCallerDataIsolated(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	cfg := json.RawMessage(`{"a":1}`)
	devices := []BatchDevice{{DeviceID: "dev-1", Revision: 0, Config: cfg}}
	rec := mustBatch(t, s, "req-1", "op", base, devices)
	// 修改传入的配置与设备列表不影响已保存状态
	cfg[5] = '9'
	devices[0].Config = json.RawMessage(`{"a":999}`)
	v, _ := s.Get("dev-1")
	if string(v.Desired) != `{"a":1}` {
		t.Fatalf("stored config affected by caller: %s", v.Desired)
	}
	// 修改返回的记录不影响后续查询与重复判断
	rec.Revisions["dev-1"] = 99
	rec.Operator = "mallory"
	got, _ := s.GetBatchRequest("req-1")
	if got.Revisions["dev-1"] != 1 || got.Operator != "op" {
		t.Fatalf("stored record affected by caller: %+v", got)
	}
	// 用被篡改过的入参重发：与首次内容一致（篡改未生效），仍按首次结果去重
	again := mustBatch(t, s, "req-1", "op", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
	})
	if again.Revisions["dev-1"] != 1 {
		t.Fatalf("dedup after caller mutation: %+v", again.Revisions)
	}
	if recs, _ := s.Audit("dev-1"); len(recs) != 1 {
		t.Fatalf("audit after caller mutation: %+v", recs)
	}
}

func TestBatchClosedStore(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BatchUpdateDesired("req-1", "op", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{}`)},
	}); !errors.Is(err, ErrClosed) {
		t.Fatalf("batch on closed store: %v", err)
	}
	if _, err := s.GetBatchRequest("req-1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("query on closed store: %v", err)
	}
}

// batchStoreDir 创建一台设备的成功批量请求（req-1，dev-1，修订号 0->1，
// 配置 {"a":1}）并关闭存储，返回目录，供篡改后重开。
func batchStoreDir(t *testing.T) string {
	t.Helper()
	s, dir := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustBatch(t, s, "req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
	})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func rewriteStore(t *testing.T, dir string, mutate func(map[string]any)) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, storeFileName))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	mutate(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, storeFileName), out, 0o644); err != nil {
		t.Fatal(err)
	}
}

func firstAudit(doc map[string]any) map[string]any {
	return doc["devices"].(map[string]any)["dev-1"].(map[string]any)["audit"].([]any)[0].(map[string]any)
}

func firstBatchDev(doc map[string]any) map[string]any {
	return doc["batches"].(map[string]any)["req-1"].(map[string]any)["devices"].([]any)[0].(map[string]any)
}

// 审计被改成其他操作者/时间/配置后，即使请求标识与修订号对得上也必须拒绝。
func TestBatchRestoreTamperedAuditRefused(t *testing.T) {
	// 操作者被改
	dir := batchStoreDir(t)
	rewriteStore(t, dir, func(doc map[string]any) {
		firstAudit(doc)["operator"] = "mallory"
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("tampered audit operator: %v", err)
	}

	// 发生时间被改为另一时刻（仍非零值）
	dir = batchStoreDir(t)
	rewriteStore(t, dir, func(doc map[string]any) {
		firstAudit(doc)["time"] = "2026-10-02T12:05:00Z"
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("tampered audit time: %v", err)
	}

	// 修改后配置值不同：{"a":2}
	dir = batchStoreDir(t)
	rewriteStore(t, dir, func(doc map[string]any) {
		firstAudit(doc)["after"] = map[string]any{"a": 2}
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("tampered audit after config: %v", err)
	}

	// 审计误挂到另一个存在、但实际未包含该设备的请求：构造 req-2（仅
	// dev-2）；dev-1 后续的单台审计（请求标识本应为空）被改成 req-2。
	// 此时 req-1 的正向核对仍通过，只有反向核对能发现误挂。
	s, dir2 := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustRegister(t, s, "dev-2", "1.0")
	mustBatch(t, s, "req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
	})
	if _, err := s.UpdateDesired("dev-1", "bob", base.Add(30*time.Minute), 1, json.RawMessage(`{"a":8}`)); err != nil {
		t.Fatal(err)
	}
	mustBatch(t, s, "req-2", "alice", base.Add(time.Hour), []BatchDevice{
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"b":2}`)},
	})
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	rewriteStore(t, dir2, func(doc map[string]any) {
		audits := doc["devices"].(map[string]any)["dev-1"].(map[string]any)["audit"].([]any)
		audits[1].(map[string]any)["requestId"] = "req-2"
	})
	if _, err := Open(dir2); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("audit misattached to other existing request: %v", err)
	}

	// 损坏内容不被覆盖
	data, _ := os.ReadFile(filepath.Join(dir2, storeFileName))
	if len(data) == 0 || !bytes.Contains(data, []byte(`"req-2"`)) {
		t.Fatalf("corrupt file overwritten: %s", data)
	}
}

// 双向核对：请求缺审计、审计修订号指向请求未产生的修订号，都必须拒绝。
func TestBatchRestorePairingRefused(t *testing.T) {
	// 请求中设备的新修订号改指向设备后来一次单台修改的修订号：
	// 请求不再有对应的批量审计（缺审计），原批量审计也变成误挂。
	s, dir := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustBatch(t, s, "req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
	})
	if _, err := s.UpdateDesired("dev-1", "bob", base.Add(time.Hour), 1, json.RawMessage(`{"a":9}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	rewriteStore(t, dir, func(doc map[string]any) {
		bd := firstBatchDev(doc)
		bd["newRevision"] = 2
		bd["revision"] = 1
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("request without matching audit: %v", err)
	}

	// 审计修订号被改：请求产生 1，审计指向 2；审计不再描述该请求项
	dir = batchStoreDir(t)
	rewriteStore(t, dir, func(doc map[string]any) {
		firstAudit(doc)["revision"] = 2
	})
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("audit revision not produced by request: %v", err)
	}
}

// 配置按批量提交的既有语义比较：1 与 1.0 相等、时区不同的同一时刻相等。
func TestBatchRestoreSemanticEqualityAccepted(t *testing.T) {
	dir := batchStoreDir(t)
	rewriteStore(t, dir, func(doc map[string]any) {
		firstAudit(doc)["after"] = map[string]any{"a": 1.0}
		firstAudit(doc)["time"] = "2026-10-02T20:00:00+08:00"
	})
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("numeric/time representation differences must open: %v", err)
	}
	defer s.Close()
	rec, err := s.GetBatchRequest("req-1")
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Time.Equal(base) {
		t.Fatalf("restored request time: %v", rec.Time)
	}
	recs, _ := s.Audit("dev-1")
	if len(recs) != 1 || !recs[0].Time.Equal(base) || !rawEqual(recs[0].After, json.RawMessage(`{"a":1}`)) {
		t.Fatalf("restored audit: %+v", recs)
	}
}

// 核对的是历史修改：设备后来接受单台修改或其他批量修改后，旧请求仍正常恢复；
// 查询返回首次结果，原内容重发不覆盖较新配置、不增加审计。
func TestBatchRestoreHistoryAfterLaterChanges(t *testing.T) {
	s, dir := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustBatch(t, s, "req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
	})
	// 随后一次单台修改，审计请求标识为空
	if _, err := s.UpdateDesired("dev-1", "bob", base.Add(time.Hour), 1, json.RawMessage(`{"a":9}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after later single update: %v", err)
	}
	defer s2.Close()
	got, err := s2.GetBatchRequest("req-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Operator != "alice" || got.Revisions["dev-1"] != 1 {
		t.Fatalf("historical record changed: %+v", got)
	}
	recs, _ := s2.Audit("dev-1")
	if len(recs) != 2 || recs[0].RequestID != "req-1" || recs[1].RequestID != "" {
		t.Fatalf("audits after reopen: %+v", recs)
	}
	// 原内容重发：返回首次结果，不覆盖较新配置，不增加审计
	again := mustBatch(t, s2, "req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
	})
	if again.Revisions["dev-1"] != 1 {
		t.Fatalf("stale resubmit result: %+v", again.Revisions)
	}
	v, _ := s2.Get("dev-1")
	if v.Revision != 2 || string(v.Desired) != `{"a":9}` {
		t.Fatalf("newer config overwritten: %+v", v)
	}
	if recs, _ := s2.Audit("dev-1"); len(recs) != 2 {
		t.Fatalf("audit grew on resubmit: %+v", recs)
	}
}
