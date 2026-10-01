package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

var base = time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

func openTemp(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, dir
}

func mustRegister(t *testing.T, s *Store, id, version string) {
	t.Helper()
	if err := s.Register(id, version); err != nil {
		t.Fatalf("Register(%s): %v", id, err)
	}
}

func TestRegisterValidation(t *testing.T) {
	s, _ := openTemp(t)
	if err := s.Register("", "1.0"); !errors.Is(err, ErrInvalidDeviceID) {
		t.Fatalf("empty id: got %v", err)
	}
	if err := s.Register("dev-1", ""); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("empty version: got %v", err)
	}
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Register("dev-1", "2.0"); !errors.Is(err, ErrDeviceExists) {
		t.Fatalf("duplicate: got %v", err)
	}
	// 重复登记不覆盖原记录
	v, err := s.Get("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Version != "1.0" {
		t.Fatalf("version overwritten: %s", v.Version)
	}
	// 新设备初始离线，双方配置为空对象
	if v.Online {
		t.Fatal("new device should be offline")
	}
	if string(v.Desired) != "{}" || string(v.Reported) != "{}" {
		t.Fatalf("initial configs: desired=%s reported=%s", v.Desired, v.Reported)
	}
	if v.Revision != 0 || v.LastSeq != 0 {
		t.Fatalf("initial revision/seq: %d/%d", v.Revision, v.LastSeq)
	}
}

func TestUnregisteredDevice(t *testing.T) {
	s, _ := openTemp(t)
	if _, err := s.Get("ghost"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("Get: %v", err)
	}
	if _, err := s.UpdateDesired("ghost", "op", base, 0, json.RawMessage(`{}`)); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("UpdateDesired: %v", err)
	}
	if err := s.Report("ghost", 1, base, "1.0", json.RawMessage(`{}`)); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("Report: %v", err)
	}
	if err := s.SetOffline("ghost"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("SetOffline: %v", err)
	}
	if _, err := s.Diff("ghost"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("Diff: %v", err)
	}
	if _, err := s.Audit("ghost"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("Audit: %v", err)
	}
}

func TestUpdateDesiredAndRevision(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")

	// 操作者为空、时间缺失、配置无效：报错且不改状态
	if _, err := s.UpdateDesired("dev-1", "", base, 0, json.RawMessage(`{}`)); !errors.Is(err, ErrInvalidOperator) {
		t.Fatalf("empty operator: %v", err)
	}
	if _, err := s.UpdateDesired("dev-1", "op", time.Time{}, 0, json.RawMessage(`{}`)); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("zero time: %v", err)
	}
	for _, bad := range []string{`[1,2]`, `"str"`, `42`, `null`, `{broken`, ``} {
		if _, err := s.UpdateDesired("dev-1", "op", base, 0, json.RawMessage(bad)); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("config %q: %v", bad, err)
		}
	}
	v, _ := s.Get("dev-1")
	if v.Revision != 0 || string(v.Desired) != "{}" {
		t.Fatalf("state changed by invalid updates: %+v", v)
	}

	// 修订号冲突：影子和审计均不改变
	if _, err := s.UpdateDesired("dev-1", "op", base, 5, json.RawMessage(`{"a":1}`)); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("conflict: %v", err)
	}
	v, _ = s.Get("dev-1")
	if v.Revision != 0 || string(v.Desired) != "{}" {
		t.Fatalf("state changed by conflict: %+v", v)
	}
	if recs, _ := s.Audit("dev-1"); len(recs) != 0 {
		t.Fatalf("audit changed by conflict: %v", recs)
	}

	// 成功修改，修订号从 1 开始
	rev, err := s.UpdateDesired("dev-1", "alice", base, 0, json.RawMessage(`{"a":1}`))
	if err != nil || rev != 1 {
		t.Fatalf("update: rev=%d err=%v", rev, err)
	}
	// 提交相同配置也增加修订号
	rev, err = s.UpdateDesired("dev-1", "bob", base.Add(time.Minute), 1, json.RawMessage(`{"a":1}`))
	if err != nil || rev != 2 {
		t.Fatalf("same config update: rev=%d err=%v", rev, err)
	}

	// 审计按修订号递增，含前后完整配置
	recs, err := s.Audit("dev-1")
	if err != nil || len(recs) != 2 {
		t.Fatalf("audit: %v %v", recs, err)
	}
	if recs[0].Revision != 1 || recs[1].Revision != 2 {
		t.Fatalf("audit order: %d %d", recs[0].Revision, recs[1].Revision)
	}
	if recs[0].Operator != "alice" || !recs[0].Time.Equal(base) {
		t.Fatalf("audit meta: %+v", recs[0])
	}
	if string(recs[0].Before) != "{}" || string(recs[0].After) != `{"a":1}` {
		t.Fatalf("audit configs: %s -> %s", recs[0].Before, recs[0].After)
	}
	if recs[0].DeviceID != "dev-1" {
		t.Fatalf("audit device: %s", recs[0].DeviceID)
	}
}

func TestReportSequenceRules(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")

	// 缺少信息的拒绝
	if err := s.Report("dev-1", 0, base, "1.1", json.RawMessage(`{}`)); !errors.Is(err, ErrInvalidSequence) {
		t.Fatalf("seq 0: %v", err)
	}
	if err := s.Report("dev-1", 1, time.Time{}, "1.1", json.RawMessage(`{}`)); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("zero time: %v", err)
	}
	if err := s.Report("dev-1", 1, base, "", json.RawMessage(`{}`)); !errors.Is(err, ErrInvalidVersion) {
		t.Fatalf("empty version: %v", err)
	}
	if err := s.Report("dev-1", 1, base, "1.1", json.RawMessage(`[1]`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("bad config: %v", err)
	}

	// 有效上报：更新并上线
	if err := s.Report("dev-1", 1, base, "1.1", json.RawMessage(`{"cpu":80}`)); err != nil {
		t.Fatal(err)
	}
	v, _ := s.Get("dev-1")
	if !v.Online || v.Version != "1.1" || v.LastSeq != 1 || string(v.Reported) != `{"cpu":80}` {
		t.Fatalf("after report: %+v", v)
	}

	// 相同序号、内容相同：重复，成功但不改变状态
	if err := s.Report("dev-1", 1, base, "1.1", json.RawMessage(`{"cpu":80}`)); err != nil {
		t.Fatalf("duplicate report: %v", err)
	}
	// 相同序号但内容不同：错误
	if err := s.Report("dev-1", 1, base, "1.1", json.RawMessage(`{"cpu":81}`)); !errors.Is(err, ErrReportConflict) {
		t.Fatalf("same seq diff config: %v", err)
	}
	if err := s.Report("dev-1", 1, base.Add(time.Minute), "1.1", json.RawMessage(`{"cpu":80}`)); !errors.Is(err, ErrReportConflict) {
		t.Fatalf("same seq diff time: %v", err)
	}
	if err := s.Report("dev-1", 1, base, "1.2", json.RawMessage(`{"cpu":80}`)); !errors.Is(err, ErrReportConflict) {
		t.Fatalf("same seq diff version: %v", err)
	}
	// 更小序号：错误
	if err := s.Report("dev-1", 1, base, "1.1", json.RawMessage(`{"cpu":80}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Report("dev-1", 0+1, base, "1.1", json.RawMessage(`{"cpu":80}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Report("dev-1", 2, base, "1.2", json.RawMessage(`{"cpu":90}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Report("dev-1", 1, base, "1.1", json.RawMessage(`{"cpu":80}`)); !errors.Is(err, ErrStaleSequence) {
		t.Fatalf("smaller seq: %v", err)
	}
	v, _ = s.Get("dev-1")
	if v.Version != "1.2" || v.LastSeq != 2 {
		t.Fatalf("after seq 2: %+v", v)
	}
}

func TestOfflineKeepsState(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	if _, err := s.UpdateDesired("dev-1", "op", base, 0, json.RawMessage(`{"mode":"auto"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Report("dev-1", 1, base, "1.1", json.RawMessage(`{"mode":"manual"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOffline("dev-1"); err != nil {
		t.Fatal(err)
	}
	v, _ := s.Get("dev-1")
	if v.Online {
		t.Fatal("should be offline")
	}
	if v.Version != "1.1" || string(v.Desired) != `{"mode":"auto"}` || string(v.Reported) != `{"mode":"manual"}` {
		t.Fatalf("offline changed state: %+v", v)
	}
	// 离线期间仍能修改期望
	if _, err := s.UpdateDesired("dev-1", "op", base.Add(time.Minute), 1, json.RawMessage(`{"mode":"eco"}`)); err != nil {
		t.Fatalf("update while offline: %v", err)
	}
	// 上线后的有效上报继续核对差异
	if err := s.Report("dev-1", 2, base.Add(2*time.Minute), "1.1", json.RawMessage(`{"mode":"eco"}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ := s.Diff("dev-1")
	if len(diffs) != 0 {
		t.Fatalf("diffs after sync: %+v", diffs)
	}
	v, _ = s.Get("dev-1")
	if !v.Online {
		t.Fatal("should be online after report")
	}
}

func TestDiffSemantics(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	t1 := base
	if _, err := s.UpdateDesired("dev-1", "op", t1, 0, json.RawMessage(`{
		"wifi": {"ssid": "a", "chan": 6},
		"nullField": null,
		"arr": [1, 2],
		"onlyDesired": {"deep": true},
		"num": 1
	}`)); err != nil {
		t.Fatal(err)
	}
	t2 := base.Add(time.Minute)
	if err := s.Report("dev-1", 1, t2, "1.0", json.RawMessage(`{
		"wifi": {"ssid": "b", "chan": 6},
		"arr": [1, 2, 3],
		"onlyReported": 5,
		"num": 1.0
	}`)); err != nil {
		t.Fatal(err)
	}
	diffs, err := s.Diff("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	// 期望路径（字典序）：
	//   /arr          数组整体比较
	//   /nullField    null 与不存在不同
	//   /onlyDesired  一侧缺失，只列一条（不下钻）
	//   /onlyReported 一侧缺失
	//   /wifi/ssid    逐层比较
	// num: 1 与 1.0 数值相等，无差异
	want := []string{"/arr", "/nullField", "/onlyDesired", "/onlyReported", "/wifi/ssid"}
	if len(diffs) != len(want) {
		t.Fatalf("diffs: %+v", diffs)
	}
	for i, p := range want {
		if diffs[i].Path != p {
			t.Fatalf("diff[%d] path: want %s got %s", i, p, diffs[i].Path)
		}
	}
	// 存在性标志
	if !diffs[0].DesiredExists || !diffs[0].ReportedExists {
		t.Fatalf("arr exists flags: %+v", diffs[0])
	}
	if !diffs[1].DesiredExists || diffs[1].ReportedExists {
		t.Fatalf("nullField flags: %+v", diffs[1])
	}
	if diffs[2].ReportedExists || !diffs[2].DesiredExists {
		t.Fatalf("onlyDesired flags: %+v", diffs[2])
	}
	if diffs[3].DesiredExists || !diffs[3].ReportedExists {
		t.Fatalf("onlyReported flags: %+v", diffs[3])
	}
	// 双方值
	if string(diffs[4].Desired) != `"a"` || string(diffs[4].Reported) != `"b"` {
		t.Fatalf("ssid values: %+v", diffs[4])
	}
	// 差异时间：/wifi/ssid 等在上报时首次出现
	for _, d := range diffs {
		want := t2
		if d.Path == "/onlyDesired" || d.Path == "/nullField" || d.Path == "/arr" {
			// 上报前 desired 已有而 reported 为空对象，差异在修改期望时已出现
			want = t1
		}
		if !d.Since.Equal(want) {
			t.Fatalf("%s since: want %v got %v", d.Path, want, d.Since)
		}
	}
}

func TestDiffSinceLifecycle(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	t1 := base
	if _, err := s.UpdateDesired("dev-1", "op", t1, 0, json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ := s.Diff("dev-1")
	if len(diffs) != 1 || diffs[0].Path != "/a" || !diffs[0].Since.Equal(t1) {
		t.Fatalf("initial diff: %+v", diffs)
	}
	// 相同路径的值变了但仍不相等：保留原时间
	t2 := base.Add(time.Minute)
	if _, err := s.UpdateDesired("dev-1", "op", t2, 1, json.RawMessage(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ = s.Diff("dev-1")
	if len(diffs) != 1 || !diffs[0].Since.Equal(t1) {
		t.Fatalf("since should be kept: %+v", diffs)
	}
	// 相等后移除
	t3 := base.Add(2 * time.Minute)
	if err := s.Report("dev-1", 1, t3, "1.0", json.RawMessage(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	if diffs, _ = s.Diff("dev-1"); len(diffs) != 0 {
		t.Fatalf("diff should clear: %+v", diffs)
	}
	// 再次不同则重新计时
	t4 := base.Add(3 * time.Minute)
	if _, err := s.UpdateDesired("dev-1", "op", t4, 2, json.RawMessage(`{"a":3}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ = s.Diff("dev-1")
	if len(diffs) != 1 || !diffs[0].Since.Equal(t4) {
		t.Fatalf("since should restart: %+v", diffs)
	}
	// 离线和重复上报不刷新差异时间
	if err := s.SetOffline("dev-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Report("dev-1", 1, t3, "1.0", json.RawMessage(`{"a":2}`)); err != nil {
		t.Fatalf("duplicate report: %v", err)
	}
	diffs, _ = s.Diff("dev-1")
	if len(diffs) != 1 || !diffs[0].Since.Equal(t4) {
		t.Fatalf("since refreshed by offline/duplicate: %+v", diffs)
	}
}

func TestDiffIgnoresFieldOrder(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	if _, err := s.UpdateDesired("dev-1", "op", base, 0, json.RawMessage(`{"a":1,"b":{"x":1,"y":2}}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Report("dev-1", 1, base, "1.0", json.RawMessage(`{"b":{"y":2,"x":1},"a":1}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ := s.Diff("dev-1")
	if len(diffs) != 0 {
		t.Fatalf("field order should not matter: %+v", diffs)
	}
}

func TestPersistenceReopen(t *testing.T) {
	s, dir := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	if _, err := s.UpdateDesired("dev-1", "op", base, 0, json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Report("dev-1", 7, base, "1.1", json.RawMessage(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	v, err := s2.Get("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if !v.Online || v.Version != "1.1" || v.Revision != 1 || v.LastSeq != 7 {
		t.Fatalf("restored view: %+v", v)
	}
	diffs, _ := s2.Diff("dev-1")
	if len(diffs) != 1 || diffs[0].Path != "/a" || !diffs[0].Since.Equal(base) {
		t.Fatalf("restored diff: %+v", diffs)
	}
	recs, _ := s2.Audit("dev-1")
	if len(recs) != 1 || recs[0].Revision != 1 {
		t.Fatalf("restored audit: %+v", recs)
	}
	// 重开后继续处理：修订号与序号规则延续
	if _, err := s2.UpdateDesired("dev-1", "op", base, 0, json.RawMessage(`{}`)); !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("revision should continue: %v", err)
	}
	if err := s2.Report("dev-1", 6, base, "1.1", json.RawMessage(`{}`)); !errors.Is(err, ErrStaleSequence) {
		t.Fatalf("seq should continue: %v", err)
	}
	if _, err := s2.UpdateDesired("dev-1", "op", base, 1, json.RawMessage(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	if diffs, _ := s2.Diff("dev-1"); len(diffs) != 0 {
		t.Fatalf("diff after resync: %+v", diffs)
	}
}

func TestCorruptStorageRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, storeFileName), []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("corrupt json: %v", err)
	}
	// 损坏内容不被覆盖
	data, _ := os.ReadFile(filepath.Join(dir, storeFileName))
	if string(data) != `{not json` {
		t.Fatalf("corrupt file overwritten: %s", data)
	}
	// 格式版本不识别
	dir2 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir2, storeFileName), []byte(`{"format":99,"devices":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir2); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("bad format: %v", err)
	}
	// 设备配置损坏
	dir3 := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir3, storeFileName),
		[]byte(`{"format":1,"devices":{"d":{"version":"1","desired":[1],"reported":{}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir3); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("bad device config: %v", err)
	}
}

func TestSaveFailureRollsBack(t *testing.T) {
	s, dir := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	// 用同名目录堵住存储文件路径，使原子替换失败
	if err := os.Remove(filepath.Join(dir, storeFileName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, storeFileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDesired("dev-1", "op", base, 0, json.RawMessage(`{"a":1}`)); err == nil {
		t.Fatal("expected save error")
	}
	// 查询仍显示操作前的结果
	v, err := s.Get("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Revision != 0 || string(v.Desired) != "{}" {
		t.Fatalf("state not rolled back: %+v", v)
	}
	if recs, _ := s.Audit("dev-1"); len(recs) != 0 {
		t.Fatalf("audit not rolled back: %+v", recs)
	}
	// 恢复可写后操作成功
	if err := os.Remove(filepath.Join(dir, storeFileName)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDesired("dev-1", "op", base, 0, json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentSameDevice(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	const n = 20
	var wg sync.WaitGroup
	// 并发提交相同修订号：只有一个能成功
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cfg, _ := json.Marshal(map[string]int{"i": i})
			_, err := s.UpdateDesired("dev-1", "op", base, 0, cfg)
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	var ok, conflict int
	for err := range results {
		if err == nil {
			ok++
		} else if errors.Is(err, ErrRevisionConflict) {
			conflict++
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("revision race: ok=%d conflict=%d", ok, conflict)
	}
	// 并发上报同一序号：只有一个不同内容的能成功
	results = make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cfg, _ := json.Marshal(map[string]int{"r": i})
			results <- s.Report("dev-1", 1, base, "1.1", cfg)
		}(i)
	}
	wg.Wait()
	close(results)
	ok, conflict = 0, 0
	for err := range results {
		if err == nil {
			ok++
		} else if errors.Is(err, ErrReportConflict) {
			conflict++
		}
	}
	if ok != 1 || conflict != n-1 {
		t.Fatalf("report race: ok=%d conflict=%d", ok, conflict)
	}
	v, _ := s.Get("dev-1")
	if v.Revision != 1 || v.LastSeq != 1 {
		t.Fatalf("final state: %+v", v)
	}
}

func TestReturnedDataIsIsolated(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	if _, err := s.UpdateDesired("dev-1", "op", base, 0, json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	v, _ := s.Get("dev-1")
	v.Desired[1] = 'X' // 调用方篡改返回值
	recs, _ := s.Audit("dev-1")
	recs[0].After[1] = 'X'
	v2, _ := s.Get("dev-1")
	if string(v2.Desired) != `{"a":1}` {
		t.Fatalf("stored desired mutated: %s", v2.Desired)
	}
	recs2, _ := s.Audit("dev-1")
	if string(recs2[0].After) != `{"a":1}` {
		t.Fatalf("stored audit mutated: %s", recs2[0].After)
	}
	// 调用方传入的配置被篡改也不影响已保存记录
	cfg := json.RawMessage(`{"b":2}`)
	if _, err := s.UpdateDesired("dev-1", "op", base, 1, cfg); err != nil {
		t.Fatal(err)
	}
	cfg[1] = 'X'
	v3, _ := s.Get("dev-1")
	if string(v3.Desired) != `{"b":2}` {
		t.Fatalf("stored desired affected by caller: %s", v3.Desired)
	}
}

func TestClosedStore(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get("dev-1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after close: %v", err)
	}
	if err := s.Register("dev-2", "1.0"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Register after close: %v", err)
	}
	if err := s.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("double close: %v", err)
	}
}
