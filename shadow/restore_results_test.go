package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// resultsCampaignDir 用公开 API 构造一份合法存储并关闭：
// 活动 cmp-1（未开启回滚）中 d1 已完成下载与安装，d2 下载成功、等待安装。
// 结果历史按接受顺序为：d1 下载、d1 安装、d2 下载。
func resultsCampaignDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterUpgrade("v2", []string{"v1"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "d1", "v1")
	mustRegister(t, s, "d2", "v1")
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2"}
	spec.BatchSize = 2
	createCampaign(t, s, spec)
	finishDevice(t, s, spec, "d1", upBase.Add(time.Minute))
	dl, err := s.Claim(spec.ID, "d2", upBase.Add(10*time.Minute))
	if err != nil || dl == nil || dl.Kind != StageDownload {
		t.Fatalf("claim d2 download: %v %+v", err, dl)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d2", OperationID: dl.ID,
		At: upBase.Add(11 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("d2 download result: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func readStoreDoc(t *testing.T, dir string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, storeFileName))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func writeStoreDoc(t *testing.T, dir string, doc map[string]any) {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, storeFileName), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func campaignResults(t *testing.T, doc map[string]any) []any {
	t.Helper()
	c := doc["campaigns"].(map[string]any)["cmp-1"].(map[string]any)
	return c["results"].([]any)
}

func setCampaignResults(t *testing.T, doc map[string]any, results []any) {
	t.Helper()
	c := doc["campaigns"].(map[string]any)["cmp-1"].(map[string]any)
	c["results"] = results
}

// 结果历史与已接受的操作结果必须一一对应：缺少记录、多出记录、同一操作
// 重复出现或对应内容矛盾都视为损坏，Open 返回 ErrCorruptStorage 并保留原数据。
func TestRestoreResultHistoryCorrupt(t *testing.T) {
	cases := map[string]func(t *testing.T, doc map[string]any){
		// 下载成功等待安装的设备，其下载记录被删掉
		"ready device download record deleted": func(t *testing.T, doc map[string]any) {
			res := campaignResults(t, doc)
			setCampaignResults(t, doc, res[:len(res)-1])
		},
		// 已完成设备的安装记录被删掉
		"install record deleted": func(t *testing.T, doc map[string]any) {
			res := campaignResults(t, doc)
			setCampaignResults(t, doc, append(res[:1:1], res[2:]...))
		},
		// 全部历史被清空，但设备已有结果
		"all history deleted": func(t *testing.T, doc map[string]any) {
			setCampaignResults(t, doc, nil)
		},
		// 为尚未领取的安装阶段补写一条记录
		"extra record without accepted result": func(t *testing.T, doc map[string]any) {
			res := campaignResults(t, doc)
			at := res[0].(map[string]any)["at"]
			setCampaignResults(t, doc, append(res, map[string]any{
				"deviceId": "d2", "operationId": "cmp-1:d2:install",
				"stage": "install", "success": true, "at": at,
			}))
		},
		// 同一操作的历史记录重复出现
		"duplicated record": func(t *testing.T, doc map[string]any) {
			res := campaignResults(t, doc)
			setCampaignResults(t, doc, append(res, res[0]))
		},
		// 成败与已接受结果不一致
		"success flipped": func(t *testing.T, doc map[string]any) {
			r := campaignResults(t, doc)[0].(map[string]any)
			r["success"] = false
			r["reason"] = "boom"
		},
		// 失败原因与已接受结果不一致
		"reason changed": func(t *testing.T, doc map[string]any) {
			r := campaignResults(t, doc)[0].(map[string]any)
			r["reason"] = "boom"
		},
		// 安装成功记录的版本与当次接受的版本不一致
		"version changed": func(t *testing.T, doc map[string]any) {
			r := campaignResults(t, doc)[1].(map[string]any)
			r["version"] = "v9"
		},
		// 记录时间与首次接受结果的时间不是同一时刻
		"time shifted": func(t *testing.T, doc map[string]any) {
			r := campaignResults(t, doc)[0].(map[string]any)
			r["at"] = "2026-10-02T12:30:00Z"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			dir := resultsCampaignDir(t)
			doc := readStoreDoc(t, dir)
			mutate(t, doc)
			writeStoreDoc(t, dir, doc)
			original, err := os.ReadFile(filepath.Join(dir, storeFileName))
			if err != nil {
				t.Fatal(err)
			}
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
}

// 合法数据重开后设备进度、结果内容与接受顺序保持原样；
// 已接受的结果再次提交仍沿用现有重复判断，不增加历史。
func TestRestoreResultHistoryPreserved(t *testing.T) {
	dir := resultsCampaignDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("valid history must open: %v", err)
	}
	defer s.Close()
	v, err := s.GetCampaign("cmp-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Devices) != 2 || v.Devices[0].Status != DeviceSucceeded || v.Devices[1].Status != DeviceReady {
		t.Fatalf("device progress changed: %+v", v.Devices)
	}
	if len(v.Results) != 3 {
		t.Fatalf("results: %+v", v.Results)
	}
	want := []struct{ dev, stage string }{
		{"d1", StageDownload}, {"d1", StageInstall}, {"d2", StageDownload},
	}
	for i, w := range want {
		if v.Results[i].DeviceID != w.dev || v.Results[i].Stage != w.stage || !v.Results[i].Success {
			t.Fatalf("result %d changed: %+v", i, v.Results[i])
		}
	}
	if v.Results[1].Version != "v2" {
		t.Fatalf("install record version: %+v", v.Results[1])
	}
	// 重开后重复提交已接受的下载结果：成功且不增加历史。
	if err := s.SubmitResult(OperationResult{
		CampaignID: "cmp-1", DeviceID: "d2", OperationID: "cmp-1:d2:download",
		At: upBase.Add(30 * time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("replay after reopen: %v", err)
	}
	if v, _ := s.GetCampaign("cmp-1"); len(v.Results) != 3 {
		t.Fatalf("replay added history: %+v", v.Results)
	}
}

// 历史记录的时间与操作结果时间按同一时刻判断：同一时刻的不同时区表示不冲突。
func TestRestoreResultHistoryTimezoneRepresentation(t *testing.T) {
	dir := resultsCampaignDir(t)
	doc := readStoreDoc(t, dir)
	r := campaignResults(t, doc)[0].(map[string]any)
	at, err := time.Parse(time.RFC3339, r["at"].(string))
	if err != nil {
		t.Fatal(err)
	}
	r["at"] = at.In(time.FixedZone("UTC+8", 8*3600)).Format(time.RFC3339)
	writeStoreDoc(t, dir, doc)
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("same instant in another zone must open: %v", err)
	}
	defer s.Close()
}

// 安装失败后等待回滚或正在回滚：下载与安装失败记录必须保留，
// 尚未完成的回滚不要求有记录；前批失败而未执行的设备也不产生历史。
func TestRestoreAwaitingRollbackHistory(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterUpgrade("v2", []string{"v1"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "d1", "v1")
	mustRegister(t, s, "d2", "v1")
	bringOnline(t, s, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1", "d2"}
	spec.BatchSize = 1
	spec.RollbackOnFailure = true
	createCampaign(t, s, spec)
	dl, err := s.Claim(spec.ID, "d1", upBase.Add(time.Minute))
	if err != nil || dl == nil {
		t.Fatalf("claim download: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(2 * time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	in, err := s.Claim(spec.ID, "d1", upBase.Add(3*time.Minute))
	if err != nil || in == nil {
		t.Fatalf("claim install: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: upBase.Add(4 * time.Minute), Success: false, Reason: "boom",
	}); err != nil {
		t.Fatal(err)
	}
	// 领取回滚但不完成：正在回滚也不要求回滚结果记录。
	rb, err := s.Claim(spec.ID, "d1", upBase.Add(5*time.Minute))
	if err != nil || rb == nil || rb.Kind != StageRollback {
		t.Fatalf("claim rollback: %v %+v", err, rb)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("awaiting/rolling-back history must open: %v", err)
	}
	v, err := s2.GetCampaign(spec.ID)
	if err != nil {
		t.Fatal(err)
	}
	// d1 正在回滚，d2 因前批失败未执行；历史只有下载与安装失败两条。
	if v.Devices[0].Status != DeviceRollingBack || v.Devices[1].Status != DeviceSkipped {
		t.Fatalf("device progress changed: %+v", v.Devices)
	}
	if len(v.Results) != 2 || v.Results[1].Stage != StageInstall || v.Results[1].Success {
		t.Fatalf("results: %+v", v.Results)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	// 删掉安装失败记录：等待回滚的设备无法解释其进度，必须拒绝打开。
	doc := readStoreDoc(t, dir)
	res := campaignResults(t, doc)
	if got := res[1].(map[string]any)["stage"]; got != StageInstall {
		t.Fatalf("unexpected record order: %v", got)
	}
	setCampaignResults(t, doc, res[:1])
	writeStoreDoc(t, dir, doc)
	if s3, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		if s3 != nil {
			s3.Close()
		}
		t.Fatalf("install failure record missing must be corrupt: %v", err)
	}
}

// 首次领取下载时因版本不兼容产生的下载失败也是已有结果，其记录必须保留。
func TestRestoreIncompatibleDownloadFailureHistory(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterUpgrade("v2", []string{"v1"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "d1", "v1")
	bringOnline(t, s, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	// 普通上报把当前版本改为不兼容版本，首次领取下载即记为下载失败。
	if err := s.Report("d1", 2, upBase.Add(time.Minute), "v9", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute)); !errors.Is(err, ErrIncompatibleVersion) {
		t.Fatalf("want ErrIncompatibleVersion, got %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("incompatible download failure history must open: %v", err)
	}
	v, _ := s2.GetCampaign(spec.ID)
	if len(v.Results) != 1 || v.Results[0].Stage != StageDownload || v.Results[0].Success {
		t.Fatalf("results: %+v", v.Results)
	}
	if err := s2.Close(); err != nil {
		t.Fatal(err)
	}
	doc := readStoreDoc(t, dir)
	setCampaignResults(t, doc, nil)
	writeStoreDoc(t, dir, doc)
	if s3, err := Open(dir); !errors.Is(err, ErrCorruptStorage) {
		if s3 != nil {
			s3.Close()
		}
		t.Fatalf("download failure record missing must be corrupt: %v", err)
	}
}

// 已领取但未完成的操作与超时结束的阶段不产生结果历史，重开照常。
func TestRestoreTimeoutWithoutResults(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterUpgrade("v2", []string{"v1"}); err != nil {
		t.Fatal(err)
	}
	mustRegister(t, s, "d1", "v1")
	bringOnline(t, s, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	if dl, err := s.Claim(spec.ID, "d1", upBase.Add(time.Minute)); err != nil || dl == nil {
		t.Fatalf("claim download: %v %+v", err, dl)
	}
	if err := s.AdvanceCampaign(spec.ID, spec.Deadline); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("timeout without results must open: %v", err)
	}
	defer s2.Close()
	v, _ := s2.GetCampaign(spec.ID)
	if v.Devices[0].Status != DeviceTimeout || len(v.Results) != 0 {
		t.Fatalf("view: %+v results %+v", v.Devices[0], v.Results)
	}
}

// 设备后来通过普通上报改变当前版本，不能因此否定此前合法的安装历史。
func TestRestoreLaterReportDoesNotInvalidateHistory(t *testing.T) {
	dir := resultsCampaignDir(t)
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	// d1 已安装成功（当前版本 v2），普通上报把版本改为 v3。
	if err := s.Report("d1", 3, upBase.Add(20*time.Minute), "v3", json.RawMessage(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("later report must not invalidate install history: %v", err)
	}
	defer s2.Close()
	v, _ := s2.GetCampaign("cmp-1")
	if v.Devices[0].Status != DeviceSucceeded || len(v.Results) != 3 {
		t.Fatalf("view: %+v", v.Devices[0])
	}
}
