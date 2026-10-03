package shadow

import (
	"encoding/json"
	"testing"
	"time"
)

// 同序号、版本、发生时间与配置完全一致的重复上报：成功返回，但不重新写入
// 影子——已被标为离线的设备不能因重复上报重新上线，差异首次出现时间也不变。
func TestDuplicateReportKeepsOfflineAndDiffTime(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	if _, err := s.UpdateDesired("dev-1", "op", base, 0, json.RawMessage(`{"mode":"auto"}`)); err != nil {
		t.Fatal(err)
	}
	rAt := base.Add(time.Minute)
	if err := s.Report("dev-1", 1, rAt, "1.1", json.RawMessage(`{"mode":"manual"}`)); err != nil {
		t.Fatal(err)
	}
	diffs, _ := s.Diff("dev-1")
	if len(diffs) != 1 {
		t.Fatalf("unexpected diffs: %+v", diffs)
	}
	since := diffs[0].Since

	if err := s.SetOffline("dev-1"); err != nil {
		t.Fatal(err)
	}
	// 重复上报（空白/字段顺序/等值数字写法不同不影响一致判断）。
	if err := s.Report("dev-1", 1, rAt, "1.1", json.RawMessage(`{ "mode": "manual" }`)); err != nil {
		t.Fatalf("duplicate report must succeed: %v", err)
	}
	v, _ := s.Get("dev-1")
	if v.Online {
		t.Fatal("duplicate report must not bring an offline device online")
	}
	if v.LastSeq != 1 || v.Version != "1.1" {
		t.Fatalf("shadow rewritten by duplicate: %+v", v)
	}
	diffs2, _ := s.Diff("dev-1")
	if len(diffs2) != 1 || !diffs2[0].Since.Equal(since) {
		t.Fatalf("diff time refreshed by duplicate: %+v", diffs2)
	}
}

// 附带上报若此前已被普通 Report 接受（更大序号），合法的同序号内容仍能完成
// 安装操作，但不再写入影子：不刷新在线状态与差异首次出现时间。
func TestInstallAttachedReportAlreadyAcceptedByPlainReport(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	in := claimInstall(t, s, spec, "d1", upBase.Add(2*time.Minute))

	installAt := upBase.Add(5 * time.Minute)
	cfg := json.RawMessage(`{"mode":"auto"}`)
	// 普通上报先以同一序号、同一时间、同一内容接受，设备随之上线。
	if err := s.Report("d1", 2, installAt, "v2", cfg); err != nil {
		t.Fatal(err)
	}
	diffBefore, _ := s.Diff("d1")
	// 随后把设备标为离线：附带上报若是重复，则不能把它重新上线。
	if err := s.SetOffline("d1"); err != nil {
		t.Fatal(err)
	}

	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in,
		At: installAt, Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{ "mode": "auto" }`),
	}); err != nil {
		t.Fatalf("install with already-accepted report must succeed: %v", err)
	}

	cv, _ := s.GetCampaign(spec.ID)
	if d := findDevice(cv, "d1"); d.Status != DeviceSucceeded {
		t.Fatalf("device should succeed: %+v", d)
	}
	if len(cv.Results) != 2 {
		t.Fatalf("install result must be recorded once: %+v", cv.Results)
	}
	v, _ := s.Get("d1")
	if v.Online {
		t.Fatal("duplicate attached report must not bring an offline device online")
	}
	if v.LastSeq != 2 || v.Version != "v2" || string(v.Reported) != string(cfg) {
		t.Fatalf("shadow rewritten by duplicate attached report: %+v", v)
	}
	diffAfter, _ := s.Diff("d1")
	if len(diffAfter) != len(diffBefore) {
		t.Fatalf("diff set changed: before=%+v after=%+v", diffBefore, diffAfter)
	}
	for i := range diffBefore {
		if diffAfter[i].Path != diffBefore[i].Path || !diffAfter[i].Since.Equal(diffBefore[i].Since) {
			t.Fatalf("diff time refreshed: before=%+v after=%+v", diffBefore, diffAfter)
		}
	}
}
