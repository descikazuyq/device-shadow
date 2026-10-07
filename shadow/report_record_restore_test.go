package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// setupReportedDevice 构造一台已有一次有效上报（序号 7）的设备并关闭存储。
func setupReportedDevice(t *testing.T) string {
	t.Helper()
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
	return dir
}

// TestRestoreReportRecordCorrupt 校验打开时每台设备的记录必须能表示“已经登记
// 但从未上报”或“已有一次有效上报”：序号为 0 时上报时间必须为零值、上报配置
// 必须是空对象、设备必须离线；序号为正整数时必须保存非零上报时间；两种记录
// 的当前版本都必须非空。任一矛盾都拒绝打开整个存储并保留原文件内容。
func TestRestoreReportRecordCorrupt(t *testing.T) {
	// 报告中的例子：设备接受过序号 7 的上报，保存内容中序号变成 0、
	// 上报时间仍保留。若按记录恢复，较小的旧序号会被当成新上报接受。
	t.Run("seq zero but report time kept", func(t *testing.T) {
		dir := setupReportedDevice(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "dev-1")["lastSeq"] = 0
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 序号 0 却保存了非空的上报配置。
	t.Run("seq zero but reported config kept", func(t *testing.T) {
		dir := setupReportedDevice(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			d := deviceDoc(doc, "dev-1")
			d["lastSeq"] = 0
			delete(d, "lastReportTime")
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 序号 0 的从未上报设备却处于在线状态。
	t.Run("seq zero but online", func(t *testing.T) {
		s, dir := openTemp(t)
		mustRegister(t, s, "dev-1", "1.0")
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "dev-1")["online"] = true
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 序号为正整数却没有保存上报时间。
	t.Run("positive seq without report time", func(t *testing.T) {
		dir := setupReportedDevice(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			delete(deviceDoc(doc, "dev-1"), "lastReportTime")
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 从未上报的设备当前版本为空。
	t.Run("never reported with empty version", func(t *testing.T) {
		s, dir := openTemp(t)
		mustRegister(t, s, "dev-1", "1.0")
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "dev-1")["version"] = ""
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 已上报的设备当前版本为空。
	t.Run("reported with empty version", func(t *testing.T) {
		dir := setupReportedDevice(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "dev-1")["version"] = ""
		})
		assertReopenCorrupt(t, dir, original)
	})

	// 多台设备中只要一台矛盾，即使其他设备合法也不能只恢复其余设备。
	t.Run("one corrupt device rejects all", func(t *testing.T) {
		s, dir := openTemp(t)
		mustRegister(t, s, "good", "1.0")
		mustRegister(t, s, "bad", "1.0")
		if err := s.Report("good", 3, base, "1.1", json.RawMessage(`{"x":1}`)); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			deviceDoc(doc, "bad")["online"] = true
		})
		assertReopenCorrupt(t, dir, original)
	})
}

// TestRestoreReportRecordLegit 保护合法记录：从未上报的设备可以已经修改过
// 期望配置（期望非空、有审计、有差异都不拒绝）；已上报的设备可以上报空对象、
// 随后被标为离线；上报序号允许跳号。重开后保留原序号、时间、版本与上报配置，
// 新序号、重复、冲突和过旧判断沿用现有规则。
func TestRestoreReportRecordLegit(t *testing.T) {
	// 从未上报但已修改过期望配置：序号为 0 只要求上报侧为空对象，
	// 期望侧非空、存在审计与配置差异都合法。
	t.Run("never reported with desired changes", func(t *testing.T) {
		s, dir := openTemp(t)
		mustRegister(t, s, "dev-1", "1.0")
		if _, err := s.UpdateDesired("dev-1", "op", base, 0, json.RawMessage(`{"a":1}`)); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("never-reported device with desired changes must open: %v", err)
		}
		defer s2.Close()
		v, err := s2.Get("dev-1")
		if err != nil {
			t.Fatal(err)
		}
		if v.Online || v.LastSeq != 0 || v.Version != "1.0" ||
			v.Revision != 1 || string(v.Desired) != `{"a":1}` || string(v.Reported) != `{}` {
			t.Fatalf("restored view: %+v", v)
		}
		if recs, _ := s2.Audit("dev-1"); len(recs) != 1 {
			t.Fatalf("audit lost: %+v", recs)
		}
		if diffs, _ := s2.Diff("dev-1"); len(diffs) != 1 || diffs[0].Path != "/a" {
			t.Fatalf("diff lost: %+v", diffs)
		}
	})

	// 从未上报的设备上报配置是带合法空白的空对象，按 JSON 内容判断为空。
	t.Run("never reported with whitespace empty object", func(t *testing.T) {
		dir, _ := writeFixStore(t, fixStore{Format: 1, Devices: map[string]fixState{
			"dev-1": {
				Version: "1.0",
				Desired: json.RawMessage(`{}`),
				Reported: json.RawMessage(` {
 } `),
			},
		}})
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("whitespace empty object must open: %v", err)
		}
		defer s.Close()
	})

	// 已上报的设备可以上报空对象；首次上报序号允许跳号（不要求为 1）。
	t.Run("reported empty object with jumped seq", func(t *testing.T) {
		s, dir := openTemp(t)
		mustRegister(t, s, "dev-1", "1.0")
		if err := s.Report("dev-1", 7, base, "1.1", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("empty-object report must open: %v", err)
		}
		defer s2.Close()
		v, err := s2.Get("dev-1")
		if err != nil {
			t.Fatal(err)
		}
		if !v.Online || v.LastSeq != 7 || v.Version != "1.1" || string(v.Reported) != `{}` {
			t.Fatalf("restored view: %+v", v)
		}
	})

	// 已上报的设备随后被标为离线：重开后保留离线状态、序号、版本与上报配置。
	t.Run("reported then offline", func(t *testing.T) {
		s, dir := openTemp(t)
		mustRegister(t, s, "dev-1", "1.0")
		if err := s.Report("dev-1", 3, base, "1.1", json.RawMessage(`{"x":1}`)); err != nil {
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
		v, err := s2.Get("dev-1")
		if err != nil {
			t.Fatal(err)
		}
		if v.Online || v.LastSeq != 3 || v.Version != "1.1" || string(v.Reported) != `{"x":1}` {
			t.Fatalf("restored view: %+v", v)
		}
	})

	// 合法存储重开后，上报的新序号、重复、冲突和过旧判断沿用现有规则。
	t.Run("report rules continue after reopen", func(t *testing.T) {
		dir := setupReportedDevice(t)
		s2, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		defer s2.Close()
		// 更小序号过旧
		if err := s2.Report("dev-1", 6, base, "1.1", json.RawMessage(`{"a":2}`)); !errors.Is(err, ErrStaleSequence) {
			t.Fatalf("stale seq: %v", err)
		}
		// 相同序号内容不同：冲突
		if err := s2.Report("dev-1", 7, base, "1.1", json.RawMessage(`{"a":3}`)); !errors.Is(err, ErrReportConflict) {
			t.Fatalf("conflict: %v", err)
		}
		// 相同序号内容相同：重复，成功但不改变状态
		if err := s2.Report("dev-1", 7, base, "1.1", json.RawMessage(`{"a":2}`)); err != nil {
			t.Fatalf("duplicate: %v", err)
		}
		// 更大序号接受（允许跳号）
		if err := s2.Report("dev-1", 9, base.Add(time.Minute), "1.2", json.RawMessage(`{"a":1}`)); err != nil {
			t.Fatalf("newer seq: %v", err)
		}
		v, _ := s2.Get("dev-1")
		if v.LastSeq != 9 || v.Version != "1.2" {
			t.Fatalf("view after new report: %+v", v)
		}
	})
}
