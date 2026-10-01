package shadow

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func newTestShadow(t *testing.T) (*Shadow, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "shadow.json")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, path
}

func raw(s string) json.RawMessage {
	return json.RawMessage(s)
}

// jsonEqual 按 JSON 语义比较原始字节与字符串是否相等。
func jsonEqual(a json.RawMessage, b string) bool {
	va, err := parseConfig(a)
	if err != nil {
		return false
	}
	vb, err := parseConfig(json.RawMessage(b))
	if err != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}

func openDevice(t *testing.T, s *Shadow, id, version string) {
	t.Helper()
	if err := s.RegisterDevice(id, version); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
}

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestRegisterDevice(t *testing.T) {
	s, _ := newTestShadow(t)
	defer s.Close()

	// 正常登记。
	if err := s.RegisterDevice("dev1", "1.0"); err != nil {
		t.Fatalf("RegisterDevice: %v", err)
	}
	dev, err := s.Device("dev1")
	if err != nil {
		t.Fatalf("Device: %v", err)
	}
	if dev.DeviceID != "dev1" || dev.Version != "1.0" || dev.Online || dev.Revision != 0 {
		t.Fatalf("初始状态异常: %+v", dev)
	}
	if string(dev.Desired) != "{}" || string(dev.Reported) != "{}" {
		t.Fatalf("期望/上报配置应为空对象: desired=%s reported=%s", dev.Desired, dev.Reported)
	}

	// 设备标识为空。
	if err := s.RegisterDevice("", "1.0"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("空设备标识应返回参数错误, 得到 %v", err)
	}
	// 初始版本为空。
	if err := s.RegisterDevice("dev2", ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("空版本应返回参数错误, 得到 %v", err)
	}
	// 重复登记。
	if err := s.RegisterDevice("dev1", "2.0"); !errors.Is(err, ErrDeviceExists) {
		t.Fatalf("重复登记应返回已存在错误, 得到 %v", err)
	}
	// 重复登记不产生记录：版本仍是初始值。
	dev, _ = s.Device("dev1")
	if dev.Version != "1.0" {
		t.Fatalf("重复登记改变了版本: %s", dev.Version)
	}

	// 访问未登记设备。
	if _, err := s.Device("ghost"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("访问未登记设备应返回未找到错误, 得到 %v", err)
	}
	if _, err := s.Diffs("ghost"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("查询未登记设备差异应返回未找到错误, 得到 %v", err)
	}
	if _, err := s.Audit("ghost"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("查询未登记设备审计应返回未找到错误, 得到 %v", err)
	}
	if _, err := s.UpdateDesired("ghost", 0, "op", t0, raw(`{}`)); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("修改未登记设备应返回未找到错误, 得到 %v", err)
	}
	if err := s.Report("ghost", 1, t0, "1.0", raw(`{}`)); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("上报未登记设备应返回未找到错误, 得到 %v", err)
	}
	if err := s.Offline("ghost"); !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("未登记设备离线应返回未找到错误, 得到 %v", err)
	}
}

func TestUpdateDesired(t *testing.T) {
	s, _ := newTestShadow(t)
	defer s.Close()
	openDevice(t, s, "dev1", "1.0")

	// 首次修改，修订号 0 -> 1。
	rev, err := s.UpdateDesired("dev1", 0, "alice", t0, raw(`{"a":1}`))
	if err != nil {
		t.Fatalf("UpdateDesired: %v", err)
	}
	if rev != 1 {
		t.Fatalf("新修订号应为 1, 得到 %d", rev)
	}
	dev, _ := s.Device("dev1")
	if dev.Revision != 1 || string(dev.Desired) != `{"a":1}` {
		t.Fatalf("修改后状态异常: %+v", dev)
	}

	// 相同配置也递增修订号。
	rev, err = s.UpdateDesired("dev1", 1, "alice", t0.Add(time.Second), raw(`{"a":1}`))
	if err != nil {
		t.Fatalf("UpdateDesired 相同配置: %v", err)
	}
	if rev != 2 {
		t.Fatalf("相同配置也应递增修订号, 得到 %d", rev)
	}

	// 修订号冲突：影子和审计均不改变。
	_, err = s.UpdateDesired("dev1", 1, "alice", t0.Add(2*time.Second), raw(`{"a":2}`))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("修订号不一致应返回冲突, 得到 %v", err)
	}
	dev, _ = s.Device("dev1")
	if dev.Revision != 2 || string(dev.Desired) != `{"a":1}` {
		t.Fatalf("冲突不应改变状态: %+v", dev)
	}
	audits, _ := s.Audit("dev1")
	if len(audits) != 2 {
		t.Fatalf("冲突不应产生审计记录, 得到 %d 条", len(audits))
	}

	// 操作者为空。
	if _, err := s.UpdateDesired("dev1", 2, "", t0, raw(`{}`)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("操作者为空应返回参数错误, 得到 %v", err)
	}
	// 时间缺失。
	if _, err := s.UpdateDesired("dev1", 2, "alice", time.Time{}, raw(`{}`)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("时间缺失应返回参数错误, 得到 %v", err)
	}
	// 配置无效：非对象。
	if _, err := s.UpdateDesired("dev1", 2, "alice", t0, raw(`[1,2]`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("非对象配置应返回配置错误, 得到 %v", err)
	}
	// 配置无效：null。
	if _, err := s.UpdateDesired("dev1", 2, "alice", t0, raw(`null`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("null 配置应返回配置错误, 得到 %v", err)
	}
	// 配置无效：损坏 JSON。
	if _, err := s.UpdateDesired("dev1", 2, "alice", t0, raw(`{`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("损坏 JSON 应返回配置错误, 得到 %v", err)
	}
	// 配置无效：重复键。
	if _, err := s.UpdateDesired("dev1", 2, "alice", t0, raw(`{"a":1,"a":2}`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("重复键应返回配置错误, 得到 %v", err)
	}
	// 配置无效：尾随数据。
	if _, err := s.UpdateDesired("dev1", 2, "alice", t0, raw(`{}x`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("尾随数据应返回配置错误, 得到 %v", err)
	}
	// 参数错误不改变状态。
	dev, _ = s.Device("dev1")
	if dev.Revision != 2 {
		t.Fatalf("参数错误改变了修订号: %d", dev.Revision)
	}
}

func TestAudit(t *testing.T) {
	s, _ := newTestShadow(t)
	defer s.Close()
	openDevice(t, s, "dev1", "1.0")

	t1 := t0
	t2 := t0.Add(time.Second)
	if _, err := s.UpdateDesired("dev1", 0, "alice", t1, raw(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDesired("dev1", 1, "bob", t2, raw(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}

	audits, err := s.Audit("dev1")
	if err != nil {
		t.Fatalf("Audit: %v", err)
	}
	if len(audits) != 2 {
		t.Fatalf("应有 2 条审计记录, 得到 %d", len(audits))
	}
	// 按修订号递增。
	if audits[0].Revision != 1 || audits[1].Revision != 2 {
		t.Fatalf("审计未按修订号递增: %+v", audits)
	}
	// 字段完整。
	if audits[0].DeviceID != "dev1" || audits[0].Operator != "alice" || !audits[0].Time.Equal(t1) {
		t.Fatalf("第 1 条审计字段异常: %+v", audits[0])
	}
	if string(audits[0].Before) != "{}" || string(audits[0].After) != `{"a":1}` {
		t.Fatalf("第 1 条审计配置异常: before=%s after=%s", audits[0].Before, audits[0].After)
	}
	if audits[1].Operator != "bob" || !audits[1].Time.Equal(t2) {
		t.Fatalf("第 2 条审计字段异常: %+v", audits[1])
	}
	if string(audits[1].Before) != `{"a":1}` || string(audits[1].After) != `{"a":2}` {
		t.Fatalf("第 2 条审计配置异常: before=%s after=%s", audits[1].Before, audits[1].After)
	}
}

func TestReport(t *testing.T) {
	s, _ := newTestShadow(t)
	defer s.Close()
	openDevice(t, s, "dev1", "1.0")

	// 缺少参数。
	if err := s.Report("dev1", 0, t0, "1.0", raw(`{}`)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("序号为 0 应返回参数错误, 得到 %v", err)
	}
	if err := s.Report("dev1", -1, t0, "1.0", raw(`{}`)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("负序号应返回参数错误, 得到 %v", err)
	}
	if err := s.Report("dev1", 1, time.Time{}, "1.0", raw(`{}`)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("时间缺失应返回参数错误, 得到 %v", err)
	}
	if err := s.Report("dev1", 1, t0, "", raw(`{}`)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("版本为空应返回参数错误, 得到 %v", err)
	}
	if err := s.Report("dev1", 1, t0, "1.0", raw(`[1]`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("非对象配置应返回配置错误, 得到 %v", err)
	}

	// 首次上报：更新并标为在线。
	if err := s.Report("dev1", 1, t0, "1.0", raw(`{"a":1}`)); err != nil {
		t.Fatalf("Report: %v", err)
	}
	dev, _ := s.Device("dev1")
	if !dev.Online || dev.Version != "1.0" || string(dev.Reported) != `{"a":1}` {
		t.Fatalf("首次上报后状态异常: %+v", dev)
	}

	// 更大序号：更新。
	if err := s.Report("dev1", 5, t0.Add(time.Second), "1.1", raw(`{"a":2}`)); err != nil {
		t.Fatalf("更大序号上报: %v", err)
	}
	dev, _ = s.Device("dev1")
	if dev.Version != "1.1" || string(dev.Reported) != `{"a":2}` {
		t.Fatalf("更大序号上报后状态异常: %+v", dev)
	}

	// 相同序号重复上报：成功但不改变状态。
	if err := s.Report("dev1", 5, t0.Add(time.Second), "1.1", raw(`{"a":2}`)); err != nil {
		t.Fatalf("重复上报应成功: %v", err)
	}
	dev, _ = s.Device("dev1")
	if dev.Version != "1.1" || string(dev.Reported) != `{"a":2}` {
		t.Fatalf("重复上报改变了状态: %+v", dev)
	}

	// 相同序号但时间不同：错误。
	if err := s.Report("dev1", 5, t0.Add(2*time.Second), "1.1", raw(`{"a":2}`)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("相同序号时间不同应返回参数错误, 得到 %v", err)
	}
	// 相同序号但版本不同：错误。
	if err := s.Report("dev1", 5, t0.Add(time.Second), "1.2", raw(`{"a":2}`)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("相同序号版本不同应返回参数错误, 得到 %v", err)
	}
	// 相同序号但配置不同：错误。
	if err := s.Report("dev1", 5, t0.Add(time.Second), "1.1", raw(`{"a":3}`)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("相同序号配置不同应返回参数错误, 得到 %v", err)
	}
	// 更小序号：错误。
	if err := s.Report("dev1", 4, t0.Add(3*time.Second), "1.1", raw(`{"a":2}`)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("更小序号应返回参数错误, 得到 %v", err)
	}
	// 被拒绝的上报不改变状态。
	dev, _ = s.Device("dev1")
	if dev.Version != "1.1" || string(dev.Reported) != `{"a":2}` {
		t.Fatalf("被拒绝的上报改变了状态: %+v", dev)
	}
}

func TestOffline(t *testing.T) {
	s, _ := newTestShadow(t)
	defer s.Close()
	openDevice(t, s, "dev1", "1.0")

	if err := s.Report("dev1", 1, t0, "1.0", raw(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	// 离线：只改变在线状态，保留版本及双方配置。
	if err := s.Offline("dev1"); err != nil {
		t.Fatalf("Offline: %v", err)
	}
	dev, _ := s.Device("dev1")
	if dev.Online {
		t.Fatalf("设备应离线")
	}
	if dev.Version != "1.0" || string(dev.Reported) != `{"a":1}` {
		t.Fatalf("离线改变了版本或上报配置: %+v", dev)
	}

	// 离线期间仍能修改期望。
	if _, err := s.UpdateDesired("dev1", 0, "alice", t0.Add(time.Second), raw(`{"a":2}`)); err != nil {
		t.Fatalf("离线期间修改期望: %v", err)
	}
	dev, _ = s.Device("dev1")
	if dev.Revision != 1 || string(dev.Desired) != `{"a":2}` {
		t.Fatalf("离线期间修改期望未生效: %+v", dev)
	}

	// 上线后的有效上报继续核对差异。
	if err := s.Report("dev1", 2, t0.Add(2*time.Second), "1.1", raw(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	dev, _ = s.Device("dev1")
	if !dev.Online {
		t.Fatalf("有效上报后应在线")
	}
	diffs, err := s.Diffs("dev1")
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Fatalf("期望与上报一致时应无差异, 得到 %+v", diffs)
	}
}

func TestDiffs(t *testing.T) {
	s, _ := newTestShadow(t)
	defer s.Close()
	openDevice(t, s, "dev1", "1.0")

	// 字段顺序忽略：b,a,c 与 a,c,b 相等。
	if _, err := s.UpdateDesired("dev1", 0, "alice", t0, raw(`{"b":2,"a":1,"c":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Report("dev1", 1, t0, "1.0", raw(`{"a":1,"c":2,"b":2}`)); err != nil {
		t.Fatal(err)
	}
	diffs, err := s.Diffs("dev1")
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 0 {
		t.Fatalf("字段顺序不应产生差异, 得到 %+v", diffs)
	}

	// 新增差异：/b 值不同，/c 上报侧缺失。
	if err := s.Report("dev1", 2, t0.Add(time.Second), "1.1", raw(`{"a":1,"b":3}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ = s.Diffs("dev1")
	if len(diffs) != 2 {
		t.Fatalf("应有 2 条差异, 得到 %+v", diffs)
	}
	// 按路径字典序排列。
	if diffs[0].Path != "/b" || diffs[1].Path != "/c" {
		t.Fatalf("差异应按路径字典序排列, 得到 %s, %s", diffs[0].Path, diffs[1].Path)
	}
	// /b：双方都存在，值不同。
	if !diffs[0].Desired.Exists || !diffs[0].Reported.Exists {
		t.Fatalf("/b 两侧都应存在: %+v", diffs[0])
	}
	if string(diffs[0].Desired.Value) != "2" || string(diffs[0].Reported.Value) != "3" {
		t.Fatalf("/b 值异常: %s vs %s", diffs[0].Desired.Value, diffs[0].Reported.Value)
	}
	// /c：仅期望侧存在。
	if !diffs[1].Desired.Exists || diffs[1].Reported.Exists {
		t.Fatalf("/c 应仅期望侧存在: %+v", diffs[1])
	}
	if string(diffs[1].Desired.Value) != "2" {
		t.Fatalf("/c 期望值异常: %s", diffs[1].Desired.Value)
	}

	// 上报一致后清除 /b、/c 差异（上报变为空对象）。
	if err := s.Report("dev1", 3, t0.Add(2*time.Second), "1.2", raw(`{}`)); err != nil {
		t.Fatal(err)
	}

	// null 与字段不存在的区分。
	if _, err := s.UpdateDesired("dev1", 1, "alice", t0.Add(3*time.Second), raw(`{"a":null}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ = s.Diffs("dev1")
	if len(diffs) != 1 || diffs[0].Path != "/a" {
		t.Fatalf("null 应产生 /a 差异, 得到 %+v", diffs)
	}
	if !diffs[0].Desired.Exists || diffs[0].Reported.Exists {
		t.Fatalf("null 侧应存在, 另一侧不存在: %+v", diffs[0])
	}
	if string(diffs[0].Desired.Value) != "null" {
		t.Fatalf("null 值应为 null, 得到 %s", diffs[0].Desired.Value)
	}

	// 数组不拆分：整体比较。
	if err := s.Report("dev1", 4, t0.Add(4*time.Second), "1.3", raw(`{"a":[1,2]}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ = s.Diffs("dev1")
	if len(diffs) != 1 || diffs[0].Path != "/a" {
		t.Fatalf("数组应只在 /a 列一条差异, 得到 %+v", diffs)
	}
	if !diffs[0].Desired.Exists || !diffs[0].Reported.Exists {
		t.Fatalf("数组两侧都应存在: %+v", diffs[0])
	}

	// 嵌套对象逐层比较。
	if _, err := s.UpdateDesired("dev1", 2, "alice", t0.Add(5*time.Second), raw(`{"o":{"x":1}}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Report("dev1", 5, t0.Add(5*time.Second), "1.4", raw(`{"o":{"x":2}}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ = s.Diffs("dev1")
	if len(diffs) != 1 || diffs[0].Path != "/o/x" {
		t.Fatalf("嵌套对象应在 /o/x 列一条差异, 得到 %+v", diffs)
	}

	// 一侧整个字段不存在时只列一条差异。
	if _, err := s.UpdateDesired("dev1", 3, "alice", t0.Add(6*time.Second), raw(`{"o":{"x":1},"p":9}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Report("dev1", 6, t0.Add(6*time.Second), "1.5", raw(`{"o":{"x":1}}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ = s.Diffs("dev1")
	if len(diffs) != 1 || diffs[0].Path != "/p" {
		t.Fatalf("字段缺失应只在 /p 列一条差异, 得到 %+v", diffs)
	}
}

func TestDiffTiming(t *testing.T) {
	s, _ := newTestShadow(t)
	defer s.Close()
	openDevice(t, s, "dev1", "1.0")

	t1 := t0
	t2 := t0.Add(time.Second)
	t3 := t0.Add(2 * time.Second)
	t4 := t0.Add(3 * time.Second)

	// t1：期望 {a:1}，差异首次出现，时间为 t1。
	if _, err := s.UpdateDesired("dev1", 0, "alice", t1, raw(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ := s.Diffs("dev1")
	if len(diffs) != 1 || !diffs[0].Since.Equal(t1) {
		t.Fatalf("差异时间应为 t1, 得到 %+v", diffs)
	}

	// t2：上报 {a:2}，值变了但仍不相等，保留原时间 t1。
	if err := s.Report("dev1", 1, t2, "1.0", raw(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ = s.Diffs("dev1")
	if len(diffs) != 1 || !diffs[0].Since.Equal(t1) {
		t.Fatalf("值变了但仍不相等应保留原时间 t1, 得到 %+v", diffs)
	}

	// t3：上报 {a:1}，相等后差异移除。
	if err := s.Report("dev1", 2, t3, "1.0", raw(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ = s.Diffs("dev1")
	if len(diffs) != 0 {
		t.Fatalf("相等后差异应移除, 得到 %+v", diffs)
	}

	// t4：期望 {a:3}，再次不同，重新计时为 t4。
	if _, err := s.UpdateDesired("dev1", 1, "alice", t4, raw(`{"a":3}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ = s.Diffs("dev1")
	if len(diffs) != 1 || !diffs[0].Since.Equal(t4) {
		t.Fatalf("再次不同应重新计时为 t4, 得到 %+v", diffs)
	}
}

func TestDiffTimingOfflineAndDuplicate(t *testing.T) {
	s, _ := newTestShadow(t)
	defer s.Close()
	openDevice(t, s, "dev1", "1.0")

	t1 := t0
	t2 := t0.Add(time.Second)
	t3 := t0.Add(2 * time.Second)

	if _, err := s.UpdateDesired("dev1", 0, "alice", t1, raw(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	// 离线不刷新差异时间。
	if err := s.Offline("dev1"); err != nil {
		t.Fatal(err)
	}
	diffs, _ := s.Diffs("dev1")
	if !diffs[0].Since.Equal(t1) {
		t.Fatalf("离线不应刷新差异时间, 得到 %v", diffs[0].Since)
	}
	// 重复上报不刷新差异时间。
	if err := s.Report("dev1", 1, t2, "1.0", raw(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Report("dev1", 1, t2, "1.0", raw(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ = s.Diffs("dev1")
	if !diffs[0].Since.Equal(t1) {
		t.Fatalf("重复上报不应刷新差异时间, 得到 %v", diffs[0].Since)
	}
	// 有效上报使用上报时间。
	if err := s.Report("dev1", 2, t3, "1.1", raw(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ = s.Diffs("dev1")
	if len(diffs) != 0 {
		t.Fatalf("上报一致后应无差异, 得到 %+v", diffs)
	}
}

func TestPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shadow.json")

	// 写入并关闭。
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	openDevice(t, s, "dev1", "1.0")
	if _, err := s.UpdateDesired("dev1", 0, "alice", t0, raw(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Report("dev1", 1, t0.Add(time.Second), "1.1", raw(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 重新打开，状态可查。
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("重新打开: %v", err)
	}
	defer s2.Close()
	dev, err := s2.Device("dev1")
	if err != nil {
		t.Fatal(err)
	}
	if dev.Version != "1.1" || !dev.Online || dev.Revision != 1 {
		t.Fatalf("重开后状态异常: %+v", dev)
	}
	if !jsonEqual(dev.Desired, `{"a":1}`) || !jsonEqual(dev.Reported, `{"a":2}`) {
		t.Fatalf("重开后配置异常: desired=%s reported=%s", dev.Desired, dev.Reported)
	}
	diffs, err := s2.Diffs("dev1")
	if err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || diffs[0].Path != "/a" {
		t.Fatalf("重开后差异异常: %+v", diffs)
	}
	if !diffs[0].Since.Equal(t0) {
		t.Fatalf("重开后差异时间应保留, 得到 %v", diffs[0].Since)
	}
	audits, err := s2.Audit("dev1")
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 || audits[0].Operator != "alice" {
		t.Fatalf("重开后审计异常: %+v", audits)
	}

	// 重开后能继续处理。
	if _, err := s2.UpdateDesired("dev1", 1, "bob", t0.Add(2*time.Second), raw(`{"a":2}`)); err != nil {
		t.Fatalf("重开后继续修改: %v", err)
	}
	if err := s2.Report("dev1", 2, t0.Add(3*time.Second), "1.2", raw(`{"a":2}`)); err != nil {
		t.Fatalf("重开后继续上报: %v", err)
	}
	diffs, _ = s2.Diffs("dev1")
	if len(diffs) != 0 {
		t.Fatalf("重开后继续处理应消除差异, 得到 %+v", diffs)
	}
}

func TestCorruptedRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shadow.json")

	// 损坏的 JSON。
	if err := os.WriteFile(path, []byte(`{not json`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("损坏 JSON 应拒绝打开, 得到 %v", err)
	}
	// 文件内容未被覆盖。
	data, _ := os.ReadFile(path)
	if string(data) != `{not json` {
		t.Fatalf("损坏文件被覆盖: %s", data)
	}

	// 结构损坏：devices 字段缺失。
	if err := os.WriteFile(path, []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("结构损坏应拒绝打开, 得到 %v", err)
	}

	// 结构损坏：未知版本。
	if err := os.WriteFile(path, []byte(`{"version":2,"devices":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("未知版本应拒绝打开, 得到 %v", err)
	}

	// 结构损坏：设备标识不一致。
	if err := os.WriteFile(path, []byte(`{"version":1,"devices":{"dev1":{"deviceId":"dev2","desired":{},"reported":{}}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); !errors.Is(err, ErrCorrupted) {
		t.Fatalf("设备标识不一致应拒绝打开, 得到 %v", err)
	}
}

func TestSaveFailureRollback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shadow.json")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	openDevice(t, s, "dev1", "1.0")

	// 删除存储目录使保存失败。
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	_, err = s.UpdateDesired("dev1", 0, "alice", t0, raw(`{"a":1}`))
	if err == nil {
		t.Fatalf("保存失败应返回错误")
	}
	// 查询仍显示操作前的结果。
	dev, err := s.Device("dev1")
	if err != nil {
		t.Fatalf("保存失败后查询: %v", err)
	}
	if dev.Revision != 0 || string(dev.Desired) != "{}" {
		t.Fatalf("保存失败后应回滚到操作前: %+v", dev)
	}

	// 恢复目录后仍能继续处理。
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 重新建立存储文件（目录被删，文件也没了，重新打开）。
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	openDevice(t, s2, "dev1", "1.0")
	if _, err := s2.UpdateDesired("dev1", 0, "alice", t0, raw(`{"a":1}`)); err != nil {
		t.Fatalf("恢复后继续处理: %v", err)
	}
}

func TestConcurrencyRevision(t *testing.T) {
	s, _ := newTestShadow(t)
	defer s.Close()
	openDevice(t, s, "dev1", "1.0")

	var wg sync.WaitGroup
	success := make(chan int64, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rev, err := s.UpdateDesired("dev1", 0, fmt.Sprintf("op%d", i), t0.Add(time.Duration(i)*time.Millisecond), raw(fmt.Sprintf(`{"a":%d}`, i)))
			if err == nil {
				success <- rev
			}
		}(i)
	}
	wg.Wait()
	close(success)

	if len(success) != 1 {
		t.Fatalf("并发修改应只有 1 次成功, 得到 %d 次", len(success))
	}
	dev, _ := s.Device("dev1")
	if dev.Revision != 1 {
		t.Fatalf("并发后修订号应为 1, 得到 %d", dev.Revision)
	}
}

func TestConcurrencySeq(t *testing.T) {
	s, _ := newTestShadow(t)
	defer s.Close()
	openDevice(t, s, "dev1", "1.0")

	var wg sync.WaitGroup
	var ok, dup, fail int64
	var mu sync.Mutex
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := s.Report("dev1", 1, t0, "1.0", raw(`{"a":1}`))
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				// 首次成功，其余为重复。
				if ok == 0 {
					ok++
				} else {
					dup++
				}
			default:
				fail++
			}
		}(i)
	}
	wg.Wait()

	if ok != 1 {
		t.Fatalf("并发上报应只有 1 次接受, 得到 %d", ok)
	}
	// 相同序号重复上报应成功，不返回错误。
	if dup != 31 {
		t.Fatalf("其余 31 次应为重复成功, 得到 dup=%d fail=%d", dup, fail)
	}
	dev, _ := s.Device("dev1")
	if !dev.Online || dev.Version != "1.0" {
		t.Fatalf("并发上报后状态异常: %+v", dev)
	}
}

func TestReturnedDataIsolation(t *testing.T) {
	s, _ := newTestShadow(t)
	defer s.Close()
	openDevice(t, s, "dev1", "1.0")
	if _, err := s.UpdateDesired("dev1", 0, "alice", t0, raw(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Report("dev1", 1, t0, "1.0", raw(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}

	// 修改返回的 Device 数据。
	dev, _ := s.Device("dev1")
	dev.Desired[0] = 'X'
	dev.Reported[0] = 'Y'
	dev.Version = "hacked"
	dev.Online = false

	// 已保存记录不受影响。
	dev2, _ := s.Device("dev1")
	if dev2.Version != "1.0" || !dev2.Online {
		t.Fatalf("修改返回数据影响了已保存记录: %+v", dev2)
	}
	if string(dev2.Desired) != `{"a":1}` || string(dev2.Reported) != `{"a":2}` {
		t.Fatalf("修改返回配置影响了已保存记录: desired=%s reported=%s", dev2.Desired, dev2.Reported)
	}

	// 修改返回的 Diff 数据。
	diffs, _ := s.Diffs("dev1")
	diffs[0].Path = "/hacked"
	diffs[0].Desired.Value[0] = 'Z'
	diffs2, _ := s.Diffs("dev1")
	if diffs2[0].Path != "/a" || string(diffs2[0].Desired.Value) != "1" {
		t.Fatalf("修改返回差异影响了已保存记录: %+v", diffs2[0])
	}

	// 修改返回的 Audit 数据。
	audits, _ := s.Audit("dev1")
	audits[0].Operator = "hacked"
	audits[0].Before[0] = 'Z'
	audits2, _ := s.Audit("dev1")
	if audits2[0].Operator != "alice" || string(audits2[0].Before) != "{}" {
		t.Fatalf("修改返回审计影响了已保存记录: %+v", audits2[0])
	}
}

func TestClose(t *testing.T) {
	s, _ := newTestShadow(t)
	openDevice(t, s, "dev1", "1.0")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// 关闭后操作返回 ErrClosed。
	if err := s.RegisterDevice("dev2", "1.0"); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后登记应返回 ErrClosed, 得到 %v", err)
	}
	if _, err := s.Device("dev1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后查询应返回 ErrClosed, 得到 %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("重复关闭应返回 nil, 得到 %v", err)
	}
}

func TestDiffPathEscaping(t *testing.T) {
	s, _ := newTestShadow(t)
	defer s.Close()
	openDevice(t, s, "dev1", "1.0")

	// 含 / 和 ~ 的键。
	if _, err := s.UpdateDesired("dev1", 0, "alice", t0, raw(`{"a/b":1,"c~d":2,"e/f":{"g~h":3}}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Report("dev1", 1, t0, "1.0", raw(`{"a/b":2,"c~d":2,"e/f":{"g~h":4}}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ := s.Diffs("dev1")
	paths := make([]string, len(diffs))
	for i, d := range diffs {
		paths[i] = d.Path
	}
	expected := []string{"/a~1b", "/e~1f/g~0h"}
	if !reflect.DeepEqual(paths, expected) {
		t.Fatalf("路径转义异常: 得到 %v, 期望 %v", paths, expected)
	}
}
