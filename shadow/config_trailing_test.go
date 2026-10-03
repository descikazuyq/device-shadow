package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// 配置校验：对象前后允许 JSON 标准空白；对象结束后的第二个 JSON 值或
// 其他非空白文本必须拒绝；空对象、嵌套对象、数组字段与字符串中的花括号合法；
// 顶层数组、null、空输入与不完整对象仍拒绝。
func TestConfigTrailingContentValidation(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")

	valid := []string{
		`{"mode":"auto"}`,
		" \t\r\n{\"mode\":\"auto\"} \t\r\n",
		`{}`,
		`{"a":{"b":{"c":1}}}`,
		`{"list":[1,2,{"x":null}]}`,
		`{"a":"} {\"not\":\"trailing\"} {"}`,
		`{"n":1.0}`,
	}
	for i, cfg := range valid {
		if _, err := s.UpdateDesired("dev-1", "op", base, uint64(i), json.RawMessage(cfg)); err != nil {
			t.Fatalf("valid config %q: got %v", cfg, err)
		}
	}

	invalid := []string{
		`{"mode":"auto"} null`,
		`{"mode":"auto"} {}`,
		`{"mode":"auto"} []`,
		`{"mode":"auto"} 1`,
		`{"mode":"auto"} x`,
		`{"mode":"auto"}` + "\n" + `{"mode":"auto"}`,
		`{"a":1}` + " ", // 非 JSON 标准空白
		`[1,2]`,
		`null`,
		``,
		`   `,
		`{"a":1`,
		`{"a":}`,
	}
	for _, cfg := range invalid {
		if _, err := s.UpdateDesired("dev-1", "op", base, 100, json.RawMessage(cfg)); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("invalid config %q: got %v", cfg, err)
		}
	}
	// 全部非法配置被拒绝后，修订号与审计不变（合法修改共 len(valid) 次）。
	v, err := s.Get("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if v.Revision != uint64(len(valid)) {
		t.Fatalf("revision changed by invalid config: %d", v.Revision)
	}
	audit, err := s.Audit("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != len(valid) {
		t.Fatalf("audit changed by invalid config: %d records", len(audit))
	}
}

// 任务场景：设备成功上报 {"mode":"auto"} 后，以相同序号、版本和时间重发
// {"mode":"auto"} null，必须返回 ErrInvalidConfig 而非当作重复上报。
func TestReportTrailingContentResend(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	if err := s.Report("dev-1", 1, base, "1.0", json.RawMessage(`{"mode":"auto"}`)); err != nil {
		t.Fatalf("first report: %v", err)
	}
	err := s.Report("dev-1", 1, base, "1.0", json.RawMessage(`{"mode":"auto"} null`))
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("trailing resend: got %v", err)
	}
	// 状态保持首次上报结果。
	v, err := s.Get("dev-1")
	if err != nil {
		t.Fatal(err)
	}
	if v.LastSeq != 1 || !v.Online || string(v.Reported) != `{"mode":"auto"}` {
		t.Fatalf("state changed: %+v", v)
	}
	// 合法重发（含空白与字段顺序差异）仍视为重复。
	if err := s.Report("dev-1", 1, base, "1.0", json.RawMessage(" { \"mode\": \"auto\" }\n")); err != nil {
		t.Fatalf("legal resend: %v", err)
	}
	// 更大序号的非法上报同样拒绝且不改变状态。
	if err := s.Report("dev-1", 2, base.Add(time.Minute), "1.0", json.RawMessage(`{"mode":"auto"} {}`)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("trailing new seq: got %v", err)
	}
	if v, _ := s.Get("dev-1"); v.LastSeq != 1 {
		t.Fatalf("seq advanced by invalid report: %d", v.LastSeq)
	}
}

// 批量修改：任一配置非法整批不变且不占用请求标识；已成功请求的非法重发
// 返回配置错误并保留首次记录。
func TestBatchTrailingContent(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustRegister(t, s, "dev-2", "1.0")

	// 任一设备配置非法：整批报错，状态不变，请求标识未占用。
	devices := []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"b":2} null`)},
	}
	if _, err := s.BatchUpdateDesired("req-1", "op", base, devices); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid batch: got %v", err)
	}
	for _, id := range []string{"dev-1", "dev-2"} {
		v, err := s.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if v.Revision != 0 || string(v.Desired) != `{}` {
			t.Fatalf("device %s changed: %+v", id, v)
		}
	}
	if _, err := s.GetBatchRequest("req-1"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("request id consumed: got %v", err)
	}

	// 修正配置后可用同一标识成功提交。
	devices[1].Config = json.RawMessage(`{"b":2}`)
	rec, err := s.BatchUpdateDesired("req-1", "op", base, devices)
	if err != nil {
		t.Fatalf("valid batch: %v", err)
	}
	if rec.Revisions["dev-1"] != 1 || rec.Revisions["dev-2"] != 1 {
		t.Fatalf("revisions: %+v", rec.Revisions)
	}

	// 已成功请求的非法重发：返回配置错误，首次记录保留。
	bad := []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1} null`)},
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"b":2}`)},
	}
	if _, err := s.BatchUpdateDesired("req-1", "op", base, bad); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid resend: got %v", err)
	}
	got, err := s.GetBatchRequest("req-1")
	if err != nil {
		t.Fatalf("first record lost: %v", err)
	}
	if got.Revisions["dev-1"] != 1 || got.Revisions["dev-2"] != 1 {
		t.Fatalf("first record changed: %+v", got.Revisions)
	}
	// 合法重发仍返回首次结果（设备换序、空白与字段顺序无关）。
	legal := []BatchDevice{
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{ "b": 2.0 }`)},
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
	}
	again, err := s.BatchUpdateDesired("req-1", "op", base, legal)
	if err != nil {
		t.Fatalf("legal resend: %v", err)
	}
	if again.Revisions["dev-1"] != 1 || again.Revisions["dev-2"] != 1 {
		t.Fatalf("legal resend result: %+v", again.Revisions)
	}
	v1, _ := s.Get("dev-1")
	v2, _ := s.Get("dev-2")
	if v1.Revision != 1 || v2.Revision != 1 {
		t.Fatalf("resend re-applied: %d/%d", v1.Revision, v2.Revision)
	}
}

// 安装成功的首次结果附带非法配置：返回 ErrInvalidReport，活动与影子不变。
func TestInstallResultTrailingConfig(t *testing.T) {
	s := setupUpgrade(t, "dev-1")
	bringOnline(t, s, "dev-1")
	spec := upSpec()
	spec.Devices = []string{"dev-1"}
	createCampaign(t, s, spec)

	dl, err := s.Claim(spec.ID, "dev-1", upBase)
	if err != nil {
		t.Fatalf("claim download: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "dev-1", OperationID: dl.ID,
		At: upBase.Add(time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("download result: %v", err)
	}
	in, err := s.Claim(spec.ID, "dev-1", upBase.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("claim install: %v", err)
	}
	// 首次安装成功结果附带尾随内容：ErrInvalidReport，状态不变。
	err = s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "dev-1", OperationID: in.ID,
		At: upBase.Add(3 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true} null`),
	})
	if !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("trailing install report: got %v", err)
	}
	cv := mustGetCampaign(s, spec.ID)
	if cv.Devices[0].Status != DeviceInstalling || len(cv.Results) != 1 {
		t.Fatalf("campaign changed: %+v", cv.Devices[0])
	}
	v, _ := s.Get("dev-1")
	if v.Version != "v1" || v.LastSeq != 1 {
		t.Fatalf("shadow changed: %+v", v)
	}

	// 修正后接受；此后重发追加非空白内容返回 ErrResultConflict。
	ok := OperationResult{
		CampaignID: spec.ID, DeviceID: "dev-1", OperationID: in.ID,
		At: upBase.Add(4 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true}`),
	}
	if err := s.SubmitResult(ok); err != nil {
		t.Fatalf("valid install result: %v", err)
	}
	bad := ok
	bad.Config = json.RawMessage(`{"ok":true} null`)
	if err := s.SubmitResult(bad); !errors.Is(err, ErrResultConflict) {
		t.Fatalf("trailing resend: got %v", err)
	}
	// 合法重发仍成功：设备后来上报更大序号、活动已结束都不影响。
	if err := s.Report("dev-1", 3, upBase.Add(5*time.Minute), "v2", json.RawMessage(`{"ok":true,"x":1}`)); err != nil {
		t.Fatalf("later report: %v", err)
	}
	if cv := mustGetCampaign(s, spec.ID); !cv.Ended {
		t.Fatal("campaign should have ended")
	}
	if err := s.SubmitResult(ok); err != nil {
		t.Fatalf("legal resend after end: %v", err)
	}
}

// 回滚成功的首次结果附带非法配置返回 ErrInvalidReport；
// 已接受的回滚成功结果重发时追加非空白内容返回 ErrResultConflict。
func TestRollbackResultTrailingConfig(t *testing.T) {
	s := setupUpgrade(t, "dev-1")
	bringOnline(t, s, "dev-1")
	spec := upSpec()
	spec.Devices = []string{"dev-1"}
	spec.RollbackOnFailure = true
	createCampaign(t, s, spec)

	dl, err := s.Claim(spec.ID, "dev-1", upBase)
	if err != nil {
		t.Fatalf("claim download: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "dev-1", OperationID: dl.ID,
		At: upBase.Add(time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("download result: %v", err)
	}
	in, err := s.Claim(spec.ID, "dev-1", upBase.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("claim install: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "dev-1", OperationID: in.ID,
		At: upBase.Add(3 * time.Minute), Success: false, Reason: "flash failed",
	}); err != nil {
		t.Fatalf("install failure: %v", err)
	}
	rb, err := s.Claim(spec.ID, "dev-1", upBase.Add(4*time.Minute))
	if err != nil {
		t.Fatalf("claim rollback: %v", err)
	}
	if rb == nil || rb.Kind != StageRollback || rb.TargetVersion != "v1" {
		t.Fatalf("rollback op: %+v", rb)
	}
	// 首次回滚成功结果附带尾随内容：ErrInvalidReport，状态不变。
	err = s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "dev-1", OperationID: rb.ID,
		At: upBase.Add(5 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"r":1} x`),
	})
	if !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("trailing rollback report: got %v", err)
	}
	cv := mustGetCampaign(s, spec.ID)
	if cv.Devices[0].Status != DeviceRollingBack || len(cv.Results) != 2 {
		t.Fatalf("campaign changed: %+v", cv.Devices[0])
	}
	// 修正后接受；重发追加非空白内容返回 ErrResultConflict，合法重发成功。
	ok := OperationResult{
		CampaignID: spec.ID, DeviceID: "dev-1", OperationID: rb.ID,
		At: upBase.Add(6 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"r":1}`),
	}
	if err := s.SubmitResult(ok); err != nil {
		t.Fatalf("valid rollback result: %v", err)
	}
	bad := ok
	bad.Config = json.RawMessage(`{"r":1} {}`)
	if err := s.SubmitResult(bad); !errors.Is(err, ErrResultConflict) {
		t.Fatalf("trailing resend: got %v", err)
	}
	if err := s.SubmitResult(ok); err != nil {
		t.Fatalf("legal resend: %v", err)
	}
}

// 已领取且尚未接受结果的操作到达截止时间：仍按原规则结束活动并返回
// ErrCampaignEnded，不因附带配置非法改成配置错误。
func TestDeadlineWithInvalidConfig(t *testing.T) {
	s := setupUpgrade(t, "dev-1")
	bringOnline(t, s, "dev-1")
	spec := upSpec()
	spec.Devices = []string{"dev-1"}
	createCampaign(t, s, spec)

	dl, err := s.Claim(spec.ID, "dev-1", upBase)
	if err != nil {
		t.Fatalf("claim download: %v", err)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "dev-1", OperationID: dl.ID,
		At: upBase.Add(time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("download result: %v", err)
	}
	in, err := s.Claim(spec.ID, "dev-1", upBase.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("claim install: %v", err)
	}
	// 截止时刻提交，附带非法配置：按截止处理，返回 ErrCampaignEnded。
	err = s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "dev-1", OperationID: in.ID,
		At: spec.Deadline, Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true} null`),
	})
	if !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("deadline submit: got %v", err)
	}
	cv := mustGetCampaign(s, spec.ID)
	if !cv.Ended || cv.Devices[0].Status != DeviceTimeout {
		t.Fatalf("campaign not ended by deadline: %+v", cv.Devices[0])
	}
}
