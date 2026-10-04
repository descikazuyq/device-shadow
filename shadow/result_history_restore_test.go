package shadow

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// rewriteStore 读入已保存的存储文件，用 mutate 改写后写回，返回写回后的字节
// （即拒绝打开时必须原样保留的损坏内容）。
func rewriteStoreBytes(t *testing.T, dir string, mutate func(map[string]any)) []byte {
	t.Helper()
	path := filepath.Join(dir, storeFileName)
	data, err := os.ReadFile(path)
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
	if err := os.WriteFile(path, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return out
}

func campaignDoc(doc map[string]any, id string) map[string]any {
	return doc["campaigns"].(map[string]any)[id].(map[string]any)
}

func resultDocs(doc map[string]any, id string) []map[string]any {
	raw := campaignDoc(doc, id)["results"].([]any)
	out := make([]map[string]any, len(raw))
	for i, r := range raw {
		out[i] = r.(map[string]any)
	}
	return out
}

// assertReopenCorrupt 校验被改写的存储拒绝打开，且原文件内容保留不变。
func assertReopenCorrupt(t *testing.T, dir string, original []byte) {
	t.Helper()
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
	// 拒绝打开后重写的内容必须原样保留，不能被空数据或“修复”覆盖。
	if string(data) != string(original) {
		// original 是改写后的字节（即磁盘现状），这里只确认 Open 未再改动文件。
		t.Fatalf("store file modified by rejected open")
	}
}

// TestResultHistoryMissingOrExtraRefused 针对“下载成功等待安装、删掉历史仍能打开”
// 的问题：缺少、多出、重复或内容矛盾的结果历史都必须拒绝打开整个存储。
func TestResultHistoryMissingOrExtraRefused(t *testing.T) {
	t.Run("history deleted", func(t *testing.T) {
		s, dir, _ := setupReady(t)
		defer s.Close()
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			delete(campaignDoc(doc, "cmp-1"), "results")
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("history emptied", func(t *testing.T) {
		_, dir, _ := setupReady(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			campaignDoc(doc, "cmp-1")["results"] = []any{}
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("history duplicated", func(t *testing.T) {
		_, dir, _ := setupReady(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			rs := resultDocs(doc, "cmp-1")
			camp := campaignDoc(doc, "cmp-1")
			camp["results"] = []any{rs[0], rs[0]}
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("history extra record", func(t *testing.T) {
		_, dir, _ := setupReady(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			rs := resultDocs(doc, "cmp-1")
			extra := map[string]any{
				"deviceId":    "d1",
				"operationId": "cmp-1:d1:install",
				"stage":       "install",
				"success":     true,
				"version":     "v2",
				"at":          "2026-10-02T12:05:00Z",
			}
			campaignDoc(doc, "cmp-1")["results"] = []any{rs[0], extra}
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("success flag contradicted", func(t *testing.T) {
		_, dir, _ := setupReady(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			rs := resultDocs(doc, "cmp-1")
			rs[0]["success"] = false
			rs[0]["reason"] = "tampered"
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("time contradicted", func(t *testing.T) {
		_, dir, _ := setupReady(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			resultDocs(doc, "cmp-1")[0]["at"] = "2026-10-02T12:09:00Z"
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("wrong operation id", func(t *testing.T) {
		_, dir, _ := setupReady(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			resultDocs(doc, "cmp-1")[0]["operationId"] = "cmp-1:d1:install"
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("wrong device", func(t *testing.T) {
		_, dir, _ := setupReady(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			resultDocs(doc, "cmp-1")[0]["deviceId"] = "ghost"
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("history out of order", func(t *testing.T) {
		// 安装成功的设备有两条历史；颠倒顺序即与接受顺序矛盾。
		_, dir, _ := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			rs := resultDocs(doc, "cmp-1")
			campaignDoc(doc, "cmp-1")["results"] = []any{rs[1], rs[0]}
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("install version contradicted", func(t *testing.T) {
		_, dir, _ := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			resultDocs(doc, "cmp-1")[1]["version"] = "v9"
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("unexpected version on download", func(t *testing.T) {
		_, dir, _ := setupReady(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			resultDocs(doc, "cmp-1")[0]["version"] = "v2"
		})
		assertReopenCorrupt(t, dir, original)
	})
	t.Run("download record missing on success", func(t *testing.T) {
		_, dir, _ := setupSucceeded(t)
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			rs := resultDocs(doc, "cmp-1")
			campaignDoc(doc, "cmp-1")["results"] = []any{rs[1]}
		})
		assertReopenCorrupt(t, dir, original)
	})
}

// setupReady 关闭后返回存储目录，设备处于 ready（下载成功、安装未领取）。
func setupReady(t *testing.T) (*Store, string, CampaignSpec) {
	t.Helper()
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec.ID, "d1", upBase)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("download: %v", err)
	}
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return s, dir, spec
}

// setupSucceeded 构造下载与安装均成功的设备，历史含下载与安装两条记录。
func setupSucceeded(t *testing.T) (*Store, string, CampaignSpec) {
	t.Helper()
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	finishDevice(t, s, spec, "d1", upBase)
	dir := s.dir
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return s, dir, spec
}

// TestResultHistoryLegitStatesReopen 校验各种合法进度重开后历史与结果保持对应，
// 不凭空要求未完成或未执行阶段产生结果。
func TestResultHistoryLegitStatesReopen(t *testing.T) {
	t.Run("download success awaiting install", func(t *testing.T) {
		_, dir, spec := setupReady(t)
		s, err := Open(dir)
		if err != nil {
			t.Fatalf("ready state must open: %v", err)
		}
		defer s.Close()
		cv, err := s.GetCampaign(spec.ID)
		if err != nil {
			t.Fatal(err)
		}
		d := findDevice(cv, "d1")
		if d.Status != DeviceReady || len(cv.Results) != 1 || cv.Results[0].Stage != StageDownload {
			t.Fatalf("restored ready view: status=%s results=%+v", d.Status, cv.Results)
		}
		// 已接受结果的重复提交沿用现有幂等判断，不新增历史。
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1",
			OperationID: cv.Results[0].OperationID,
			At:          cv.Results[0].At, Success: true,
		}); err != nil {
			t.Fatalf("duplicate replay: %v", err)
		}
		if cv2, _ := s.GetCampaign(spec.ID); len(cv2.Results) != 1 {
			t.Fatalf("replay added history: %+v", cv2.Results)
		}
	})

	t.Run("claim-time download failure kept", func(t *testing.T) {
		// 首次领取下载时版本已不兼容：下载失败是已有结果，必须保留历史。
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		if err := s.Report("d1", 1, upBase.Add(-time.Minute), "v3", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Claim(spec.ID, "d1", upBase); !errors.Is(err, ErrIncompatibleVersion) {
			t.Fatalf("want incompatible version, got %v", err)
		}
		cv := mustGetCampaign(s, spec.ID)
		if len(cv.Results) != 1 || cv.Results[0].Success {
			t.Fatalf("download failure history: %+v", cv.Results)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("claim-time failure must reopen: %v", err)
		}
		defer s2.Close()
		cv2 := mustGetCampaign(s2, spec.ID)
		if findDevice(cv2, "d1").Status != DeviceFailed || len(cv2.Results) != 1 {
			t.Fatalf("restored failure view: %+v", cv2)
		}
		// 失败原因矛盾也必须被拒绝。
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			resultDocs(doc, "cmp-1")[0]["reason"] = "other reason"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("download claimed unfinished timeout", func(t *testing.T) {
		// 已领取但未完成的下载在截止时超时：没有结果，也不应有历史。
		s := setupUpgrade(t, "d1")
		spec := upSpec()
		spec.Devices = []string{"d1"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		if _, err := s.Claim(spec.ID, "d1", upBase); err != nil {
			t.Fatal(err)
		}
		if err := s.AdvanceCampaign(spec.ID, spec.Deadline); err != nil {
			t.Fatal(err)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("timeout without results must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if findDevice(cv, "d1").Status != DeviceTimeout || len(cv.Results) != 0 {
			t.Fatalf("restored timeout view: %+v", cv.Results)
		}
	})

	t.Run("later batch skipped has no history", func(t *testing.T) {
		// 前一批失败后，后续批次未执行：不为其凭空产生结果历史。
		s := setupUpgrade(t, "d1", "d2")
		spec := upSpec()
		spec.BatchSize = 1
		spec.Devices = []string{"d1", "d2"}
		createCampaign(t, s, spec)
		bringOnline(t, s, "d1")
		// d1 在首次领取前变成不兼容版本，下载失败并使 d2 记为 skipped。
		if err := s.Report("d1", 2, upBase.Add(-30*time.Second), "v3", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Claim(spec.ID, "d1", upBase); !errors.Is(err, ErrIncompatibleVersion) {
			t.Fatalf("want incompatible, got %v", err)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("skipped batch state must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if findDevice(cv, "d2").Status != DeviceSkipped || len(cv.Results) != 1 {
			t.Fatalf("restored skipped view: %+v", cv.Results)
		}
	})
}

// TestResultHistoryRollbackStatesReopen 覆盖回滚开关开启后的各阶段对账。
func TestResultHistoryRollbackStatesReopen(t *testing.T) {
	// 安装失败 → 等待回滚 / 正在回滚 / 回滚超时：安装失败历史保留，
	// 尚未完成的回滚不要求有结果。
	for _, tc := range []struct {
		name    string
		advance func(s *Store, spec CampaignSpec)
		want    string
	}{
		{"awaiting rollback", func(s *Store, spec CampaignSpec) {}, DeviceAwaitingRollback},
		{"rolling back", func(s *Store, spec CampaignSpec) {
			if _, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil {
				t.Fatalf("claim rollback: %v", err)
			}
		}, DeviceRollingBack},
		{"rollback timeout", func(s *Store, spec CampaignSpec) {
			if _, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil {
				t.Fatalf("claim rollback: %v", err)
			}
			if err := s.AdvanceCampaign(spec.ID, spec.Deadline); err != nil {
				t.Fatal(err)
			}
		}, DeviceRollbackTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, dir, spec, _ := setupInstallFailure(t, true)
			tc.advance(s, spec)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			s2, err := Open(dir)
			if err != nil {
				t.Fatalf("%s must reopen: %v", tc.name, err)
			}
			defer s2.Close()
			cv2 := mustGetCampaign(s2, spec.ID)
			d := findDevice(cv2, "d1")
			if d.Status != tc.want {
				t.Fatalf("status: want %s got %s", tc.want, d.Status)
			}
			// 只有下载成功、安装失败两条记录；回滚尚无结果。
			if len(cv2.Results) != 2 {
				t.Fatalf("history: %+v", cv2.Results)
			}
			if cv2.Results[1].Success || cv2.Results[1].Reason != "install boom" {
				t.Fatalf("install failure not preserved: %+v", cv2.Results[1])
			}
		})
	}

	t.Run("rollback failure kept", func(t *testing.T) {
		s, _, spec, rbOp := setupInstallFailure(t, true)
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rbOp.ID,
			At: upBase.Add(5 * time.Minute), Success: false, Reason: "rollback boom",
		}); err != nil {
			t.Fatalf("rollback failure: %v", err)
		}
		dir := s.dir
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("rollback failed state must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if findDevice(cv, "d1").Status != DeviceRollbackFailed || len(cv.Results) != 3 {
			t.Fatalf("view: %+v", cv.Results)
		}
		// 缺少回滚失败记录即损坏。
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			rs := resultDocs(doc, "cmp-1")
			campaignDoc(doc, "cmp-1")["results"] = []any{rs[0], rs[1]}
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("rollback success version correspondence", func(t *testing.T) {
		s, _, spec, rbOp := setupInstallFailure(t, true)
		if _, err := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rbOp.ID,
			At: upBase.Add(5 * time.Minute), Success: true,
			Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("rollback success: %v", err)
		}
		dir := s.dir
		s.Close()
		// 设备后来通过普通上报改变当前版本，不能否定此前合法的回滚历史。
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("rollback success state must reopen: %v", err)
		}
		if err := s2.Report("d1", 3, upBase.Add(90*time.Minute), "v3", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		s2.Close()
		s3, err := Open(dir)
		if err != nil {
			t.Fatalf("report after rollback must not invalidate history: %v", err)
		}
		defer s3.Close()
		cv := mustGetCampaign(s3, spec.ID)
		d := findDevice(cv, "d1")
		if d.Status != DeviceRollbackSucceeded || len(cv.Results) != 3 {
			t.Fatalf("view: %+v", cv.Results)
		}
		if cv.Results[2].Version != "v1" {
			t.Fatalf("rollback history version: %+v", cv.Results[2])
		}
		// 回滚记录的版本与当次接受版本矛盾即损坏。
		original := rewriteStoreBytes(t, dir, func(doc map[string]any) {
			resultDocs(doc, "cmp-1")[2]["version"] = "v3"
		})
		assertReopenCorrupt(t, dir, original)
	})

	t.Run("install success then version drifts", func(t *testing.T) {
		// 普通上报改变当前版本，不否定此前合法的安装历史（记录版本仍为目标版本）。
		_, dir, spec := setupSucceeded(t)
		s, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Report("d1", 3, upBase.Add(90*time.Minute), "v3", json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
		s.Close()
		s2, err := Open(dir)
		if err != nil {
			t.Fatalf("version drift after install must reopen: %v", err)
		}
		defer s2.Close()
		cv := mustGetCampaign(s2, spec.ID)
		if findDevice(cv, "d1").Status != DeviceSucceeded || cv.Results[1].Version != "v2" {
			t.Fatalf("view: %+v", cv.Results)
		}
	})
}

// setupInstallFailure 构造下载成功、安装失败已接受（等待回滚）的状态。
func setupInstallFailure(t *testing.T, rollback bool) (*Store, string, CampaignSpec, *Operation) {
	t.Helper()
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.RollbackOnFailure = rollback
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec.ID, "d1", upBase)
	if err != nil {
		t.Fatalf("claim download: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("download: %v", err)
	}
	in, err := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("claim install: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: upBase.Add(3 * time.Minute), Success: false, Reason: "install boom",
	}); err != nil {
		t.Fatalf("install failure: %v", err)
	}
	rbID := findDevice(mustGetCampaign(s, spec.ID), "d1").RollbackID
	return s, s.dir, spec, &Operation{ID: rbID}
}

// TestResultHistorySuccessWithExtraReasonReopen 提交成功结果时附带了非空原因
// （提交路径不拒绝）：原因只存在操作结果上、不进入历史，重开不得误判为矛盾。
func TestResultHistorySuccessWithExtraReasonReopen(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, err := s.Claim(spec.ID, "d1", upBase)
	if err != nil {
		t.Fatal(err)
	}
	// 成功结果携带原因：被接受，历史记录不含原因。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(time.Minute), Success: true, Reason: "informational note",
	}); err != nil {
		t.Fatalf("success result with reason must be accepted: %v", err)
	}
	dir := s.dir
	s.Close()
	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("success op result carrying a reason must not break history check: %v", err)
	}
	defer s2.Close()
	cv := mustGetCampaign(s2, spec.ID)
	if findDevice(cv, "d1").Status != DeviceReady || len(cv.Results) != 1 || cv.Results[0].Reason != "" {
		t.Fatalf("view: %+v", cv.Results)
	}
}

// TestResultHistoryTimezoneEqualInstant 历史时间与操作结果时间是同一时刻、
// 仅时区表示不同（Z vs +08:00）时不得判为冲突。
func TestResultHistoryTimezoneEqualInstant(t *testing.T) {
	_, dir, _ := setupReady(t)
	// 下载结果接受于 12:01:00Z，改写为 +08:00 表示的同一时刻 20:01:00+08:00。
	rewriteStoreBytes(t, dir, func(doc map[string]any) {
		resultDocs(doc, "cmp-1")[0]["at"] = "2026-10-02T20:01:00+08:00"
	})
	s, err := Open(dir)
	if err != nil {
		t.Fatalf("same instant in different zone must open: %v", err)
	}
	defer s.Close()
	cv := mustGetCampaign(s, "cmp-1")
	want := upBase.Add(time.Minute)
	if !cv.Results[0].At.Equal(want) {
		t.Fatalf("restored time %v not equal to %v", cv.Results[0].At, want)
	}
}

// TestResultHistoryPreservesOrderAndContent 合法数据按原样打开：
// 设备进度、结果内容与接受顺序不变。
func TestResultHistoryPreservesOrderAndContent(t *testing.T) {
	s := setupUpgrade(t, "d1", "d2")
	spec := upSpec()
	spec.BatchSize = 2
	spec.Devices = []string{"d1", "d2"}
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	bringOnline(t, s, "d2")
	finishDevice(t, s, spec, "d1", upBase)
	finishDevice(t, s, spec, "d2", upBase.Add(10*time.Minute))
	cv0 := mustGetCampaign(s, spec.ID)
	dir := s.dir
	s.Close()

	s2, err := Open(dir)
	if err != nil {
		t.Fatalf("valid history must reopen: %v", err)
	}
	defer s2.Close()
	cv := mustGetCampaign(s2, spec.ID)
	if cv.Status != CampaignSucceeded || len(cv.Results) != len(cv0.Results) {
		t.Fatalf("restored campaign: status=%s results=%+v", cv.Status, cv.Results)
	}
	for i := range cv0.Results {
		a, b := cv0.Results[i], cv.Results[i]
		if a != b {
			t.Fatalf("result %d changed: %+v vs %+v", i, a, b)
		}
	}
}
