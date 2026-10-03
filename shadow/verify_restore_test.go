package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 辅助：构造一次正常存储并返回磁盘内容。
func buildStore(t *testing.T, dir string) {
	t.Helper()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.Register("dev1", "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDesired("dev1", "op", at, 0, json.RawMessage(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDesired("dev1", "op", at, 1, json.RawMessage(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func readDisk(t *testing.T, dir string) diskStore {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, storeFileName))
	if err != nil {
		t.Fatal(err)
	}
	var disk diskStore
	if err := json.Unmarshal(data, &disk); err != nil {
		t.Fatal(err)
	}
	return disk
}

func writeDisk(t *testing.T, dir string, disk diskStore) {
	t.Helper()
	data, err := json.Marshal(disk)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, storeFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyDeletedFirstAuditRejected(t *testing.T) {
	dir := t.TempDir()
	buildStore(t, dir)
	disk := readDisk(t, dir)
	ds := disk.Devices["dev1"]
	ds.Audit = ds.Audit[1:] // 删掉第一条审计
	disk.Devices["dev1"] = ds
	writeDisk(t, dir, disk)
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("want ErrCorruptStorage, got %v", err)
	}
}

func TestVerifyDesiredMismatchRejected(t *testing.T) {
	dir := t.TempDir()
	buildStore(t, dir)
	disk := readDisk(t, dir)
	ds := disk.Devices["dev1"]
	ds.Desired = json.RawMessage(`{"a":3}`) // 与末条审计 after 不同
	disk.Devices["dev1"] = ds
	writeDisk(t, dir, disk)
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("want ErrCorruptStorage, got %v", err)
	}
}

func TestVerifyChainBreakRejected(t *testing.T) {
	dir := t.TempDir()
	buildStore(t, dir)
	disk := readDisk(t, dir)
	ds := disk.Devices["dev1"]
	ds.Audit[1].Before = json.RawMessage(`{"a":9}`) // 与前一条 after 不符
	disk.Devices["dev1"] = ds
	writeDisk(t, dir, disk)
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("want ErrCorruptStorage, got %v", err)
	}
}

func TestVerifyRevisionZeroWithDesiredRejected(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Register("dev1", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	disk := readDisk(t, dir)
	ds := disk.Devices["dev1"]
	ds.Desired = json.RawMessage(`{"x":1}`) // 修订号 0 但期望非空对象
	disk.Devices["dev1"] = ds
	writeDisk(t, dir, disk)
	if _, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		t.Fatalf("want ErrCorruptStorage, got %v", err)
	}
}

func TestVerifySameConfigUpdatesAndSemanticsAccepted(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.Register("dev1", "v1"); err != nil {
		t.Fatal(err)
	}
	// 提交相同配置也产生修订号与审计。
	if _, err := s.UpdateDesired("dev1", "op", at, 0, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDesired("dev1", "op", at, 1, json.RawMessage(`{"n": 1}`)); err != nil {
		t.Fatal(err)
	}
	// 批量与单设备交替。
	if _, err := s.BatchUpdateDesired("req1", "op", at, []BatchDevice{
		{DeviceID: "dev1", Revision: 2, Config: json.RawMessage(`{"n":1,"k":[1,2]}`)},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateDesired("dev1", "op", at, 3, json.RawMessage(`{"k":[1,2],"n":1.0}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// 字段顺序/空白/1 vs 1.0 的语义相等应被接受。
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := s2.Get("dev1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Revision != 4 {
		t.Fatalf("revision = %d, want 4", v.Revision)
	}
	recs, err := s2.Audit("dev1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 4 {
		t.Fatalf("audit len = %d, want 4", len(recs))
	}
	if recs[2].RequestID != "req1" {
		t.Fatalf("audit[2].RequestID = %q, want req1", recs[2].RequestID)
	}
	if _, err := s2.GetBatchRequest("req1"); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRevisionZeroWithReportAccepted(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.Register("dev1", "v1"); err != nil {
		t.Fatal(err)
	}
	// 只上报、不改期望：修订号 0、空审计、空对象期望，应正常打开。
	if err := s.Report("dev1", 1, at, "v2", json.RawMessage(`{"r":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err != nil {
		t.Fatalf("want open success, got %v", err)
	}
}
