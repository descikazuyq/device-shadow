package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// 配置必须是完整的单一 JSON 对象：对象前后允许 JSON 标准空白，
// 对象结束后的第二个 JSON 值或其他非空白文本一律拒绝；
// 空对象、嵌套对象、数组字段合法，顶层数组/null/空输入/不完整对象仍拒绝；
// 字符串字段中的花括号或类 JSON 文本属于字段值。
func TestConfigMustBeSingleJSONObject(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")

	valid := []string{
		`{}`,
		` { } `,
		"\t{\n\"a\":1\r\n}\t",
		`{"a":{"b":1},"list":[1,2,null]}`,
		`{"s":"} { not an object"}`,
		`{"s":"{} null trailing-looking text"}`,
		`{"a":1} ` + "\n\t",
	}
	for i, cfg := range valid {
		if _, err := s.UpdateDesired("dev-1", "op", base.Add(time.Duration(i)*time.Second), uint64(i), json.RawMessage(cfg)); err != nil {
			t.Fatalf("valid config %q rejected: %v", cfg, err)
		}
	}

	invalid := []string{
		`{"mode":"auto"} null`,
		`{"mode":"auto"}{}`,
		`{"mode":"auto"} 1`,
		`{"mode":"auto"}"x"`,
		`{"mode":"auto"}garbage`,
		`{"mode":"auto"} ]`,
		`[1,2]`,
		`null`,
		`{"a":1`,
		``,
		`   `,
	}
	for _, cfg := range invalid {
		if _, err := s.UpdateDesired("dev-1", "op", base.Add(time.Hour), uint64(len(valid)), json.RawMessage(cfg)); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("config %q: got %v, want ErrInvalidConfig", cfg, err)
		}
	}
}

// rawEqual 对合法配置保持兼容比较，但尾随内容使比较失败（而非按首个值判等）。
func TestRawEqualStrictness(t *testing.T) {
	if rawEqual(json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":1} null`)) {
		t.Fatal("trailing null must not compare equal")
	}
	if rawEqual(json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":1} x`)) {
		t.Fatal("trailing text must not compare equal")
	}
	if !rawEqual(json.RawMessage(`{"a":1,"b":2}`), json.RawMessage("\t{ \"b\": 2.0, \"a\": 1.0 }\n")) {
		t.Fatal("field order, whitespace and numeric equivalents must compare equal")
	}
	if rawEqual(json.RawMessage(`{"list":[1,2]}`), json.RawMessage(`{"list":[2,1]}`)) {
		t.Fatal("array order must matter")
	}
	if rawEqual(json.RawMessage(`{"a":1}`), json.RawMessage(`{"a":1,"b":null}`)) {
		t.Fatal("missing field must differ from null")
	}
}

// 设备先成功上报后，用相同序号/版本/时间重发追加尾随内容的配置：
// 必须返回 ErrInvalidConfig，而不能被当成重复上报；影子等状态保持不变。
func TestReportReplayWithTrailingConfig(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.1")
	if err := s.Report("dev-1", 1, base, "1.1", json.RawMessage(`{"mode":"auto"}`)); err != nil {
		t.Fatal(err)
	}

	for _, cfg := range []string{
		`{"mode":"auto"} null`,
		`{"mode":"auto"}{}`,
		`{"mode":"auto"} 42`,
		`{"mode":"auto"}text`,
	} {
		before, _ := s.Get("dev-1")
		err := s.Report("dev-1", 1, base, "1.1", json.RawMessage(cfg))
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("replay %q: got %v, want ErrInvalidConfig", cfg, err)
		}
		after, _ := s.Get("dev-1")
		if after.LastSeq != before.LastSeq || string(after.Reported) != `{"mode":"auto"}` ||
			!after.Online || after.Version != "1.1" {
			t.Fatalf("state changed by rejected replay %q: %+v", cfg, after)
		}
	}

	// 合法重发：仅空白/字段顺序/数字等值写法不同，仍按重复上报成功。
	if err := s.Report("dev-1", 1, base, "1.1", json.RawMessage(`{ "mode": "auto" }`)); err != nil {
		t.Fatalf("legal whitespace replay: %v", err)
	}
	v, _ := s.Get("dev-1")
	if v.LastSeq != 1 || string(v.Reported) != `{"mode":"auto"}` {
		t.Fatalf("legal replay altered state: %+v", v)
	}
}

// 普通上报收到顶层数组、null、空输入或尾随内容，均返回 ErrInvalidConfig 且不改状态。
func TestReportInvalidConfigShapes(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	for _, cfg := range []string{`[]`, `null`, ``, `{"a":1} null`} {
		if err := s.Report("dev-1", 1, base, "1.0", json.RawMessage(cfg)); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("config %q: got %v", cfg, err)
		}
	}
	v, _ := s.Get("dev-1")
	if v.LastSeq != 0 || v.Online || string(v.Reported) != "{}" {
		t.Fatalf("state changed: %+v", v)
	}
	// 字符串字段中的花括号属于字段值，不是尾随内容。
	if err := s.Report("dev-1", 1, base, "1.0", json.RawMessage(`{"note":"{} null"}`)); err != nil {
		t.Fatalf("braces inside string rejected: %v", err)
	}
}

// 已成功的批量请求用追加尾随内容的配置重发：返回 ErrInvalidConfig，
// 首次记录保留、不新增修订号与审计，请求标识仍可查到首次结果。
func TestBatchReplayWithTrailingConfig(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustRegister(t, s, "dev-2", "1.0")
	first := mustBatch(t, s, "req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"b":2}`)},
	})

	_, err := s.BatchUpdateDesired("req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1} null`)},
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"b":2}`)},
	})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("invalid replay: got %v, want ErrInvalidConfig", err)
	}

	got, err := s.GetBatchRequest("req-1")
	if err != nil {
		t.Fatalf("first record lost: %v", err)
	}
	if got.Revisions["dev-1"] != first.Revisions["dev-1"] || got.Revisions["dev-2"] != first.Revisions["dev-2"] {
		t.Fatalf("first record altered: %+v vs %+v", got.Revisions, first.Revisions)
	}
	for _, id := range []string{"dev-1", "dev-2"} {
		v, _ := s.Get(id)
		if v.Revision != 1 {
			t.Fatalf("%s revision bumped by rejected replay: %d", id, v.Revision)
		}
		if recs, _ := s.Audit(id); len(recs) != 1 {
			t.Fatalf("%s audit altered: %v", id, recs)
		}
	}

	// 合法重发（换序、空白、等值数字）仍返回首次结果。
	replay := mustBatch(t, s, "req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{ "b": 2.0 }`)},
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1.0}`)},
	})
	if replay.Revisions["dev-1"] != 1 || replay.Revisions["dev-2"] != 1 {
		t.Fatalf("legal replay changed revisions: %+v", replay.Revisions)
	}
}

// 全新批量请求中任一配置带尾随内容：整批不变且不占用请求标识。
func TestBatchNewRequestWithTrailingConfig(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustRegister(t, s, "dev-2", "1.0")
	_, err := s.BatchUpdateDesired("req-bad", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"b":2} {}`)},
	})
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("got %v, want ErrInvalidConfig", err)
	}
	if _, err := s.GetBatchRequest("req-bad"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("request id consumed: %v", err)
	}
	for _, id := range []string{"dev-1", "dev-2"} {
		v, _ := s.Get(id)
		if v.Revision != 0 || string(v.Desired) != "{}" {
			t.Fatalf("%s changed by failed batch: %+v", id, v)
		}
		if recs, _ := s.Audit(id); len(recs) != 0 {
			t.Fatalf("%s audit changed: %v", id, recs)
		}
	}
	// 同一标识修正后仍可首次成功提交。
	mustBatch(t, s, "req-bad", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
	})
}

// claimInstall 完成下载并领取安装，返回安装操作标识。
func claimInstall(t *testing.T, s *Store, spec CampaignSpec, id string, at time.Time) string {
	t.Helper()
	dl, err := s.Claim(spec.ID, id, at)
	if err != nil || dl == nil || dl.Kind != StageDownload {
		t.Fatalf("claim download: %v %+v", err, dl)
	}
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: id, OperationID: dl.ID,
		At: at.Add(time.Minute), Success: true,
	}); err != nil {
		t.Fatalf("download result: %v", err)
	}
	in, err := s.Claim(spec.ID, id, at.Add(2*time.Minute))
	if err != nil || in == nil || in.Kind != StageInstall {
		t.Fatalf("claim install: %v %+v", err, in)
	}
	return in.ID
}

// 首次提交的安装成功结果附带尾随内容：返回 ErrInvalidReport，
// 不等到保存阶段，影子、活动进度与结果历史均不变；修正为合法配置后可正常成功。
func TestInstallSuccessTrailingConfigFirstTime(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	in := claimInstall(t, s, spec, "d1", upBase.Add(2*time.Minute))
	shadowBefore, _ := s.Get("d1")
	cv0, _ := s.GetCampaign(spec.ID)
	nResults := len(cv0.Results)

	for _, cfg := range []string{`{"ok":true} null`, `{"ok":true} {}`, `{"ok":true}x`} {
		err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: in,
			At: upBase.Add(5 * time.Minute), Success: true,
			Seq: 2, Version: "v2", Config: json.RawMessage(cfg),
		})
		if !errors.Is(err, ErrInvalidReport) {
			t.Fatalf("config %q: got %v, want ErrInvalidReport", cfg, err)
		}
	}
	v, _ := s.GetCampaign(spec.ID)
	if st := deviceStatus(v, "d1"); st != DeviceInstalling {
		t.Fatalf("device advanced on invalid report: %s", st)
	}
	if len(v.Results) != nResults {
		t.Fatalf("result history changed: %d -> %d", nResults, len(v.Results))
	}
	after, _ := s.Get("d1")
	if after.Version != shadowBefore.Version || after.LastSeq != shadowBefore.LastSeq {
		t.Fatalf("shadow changed on invalid report: %+v vs %+v", shadowBefore, after)
	}

	// 合法配置随后仍可成功。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in,
		At: upBase.Add(5 * time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true}`),
	}); err != nil {
		t.Fatalf("legal install: %v", err)
	}
}

// 已接受的安装成功结果重发时追加非空白内容：返回 ErrResultConflict，
// 已接受结果保留；合法重发即使设备后来上报更大序号、活动已结束也继续成功。
func TestInstallSuccessReplayTrailingConflicts(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	in := claimInstall(t, s, spec, "d1", upBase.Add(2*time.Minute))
	installAt := upBase.Add(5 * time.Minute)
	first := OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in,
		At: installAt, Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"a":1,"b":2}`),
	}
	if err := s.SubmitResult(first); err != nil {
		t.Fatalf("first install: %v", err)
	}
	cv, _ := s.GetCampaign(spec.ID)
	nResults := len(cv.Results)

	// 追加非空白内容：冲突，历史不增加。
	for _, cfg := range []string{`{"a":1,"b":2} null`, `{"a":1,"b":2}z`} {
		bad := first
		bad.Config = json.RawMessage(cfg)
		if err := s.SubmitResult(bad); !errors.Is(err, ErrResultConflict) {
			t.Fatalf("replay %q: got %v, want ErrResultConflict", cfg, err)
		}
	}
	cv, _ = s.GetCampaign(spec.ID)
	if len(cv.Results) != nResults {
		t.Fatalf("history changed on conflicting replay: %+v", cv.Results)
	}
	if d := findDevice(cv, "d1"); d.Status != DeviceSucceeded {
		t.Fatalf("accepted result altered: %+v", d)
	}
	shadow, _ := s.Get("d1")
	if shadow.Version != "v2" || string(shadow.Reported) != `{"a":1,"b":2}` {
		t.Fatalf("shadow altered by conflict: %+v", shadow)
	}

	// 设备随后普通上报更大序号；活动在单设备成功后已经结束。
	if err := s.Report("d1", 3, upBase.Add(20*time.Minute), "v2", json.RawMessage(`{"later":true}`)); err != nil {
		t.Fatal(err)
	}
	// 合法重发（字段顺序、空白、等值数字不同）仍依据已接受结果成功。
	legal := first
	legal.At = upBase.Add(3 * time.Hour)
	legal.Config = json.RawMessage(`{ "b": 2.0, "a": 1 }`)
	if err := s.SubmitResult(legal); err != nil {
		t.Fatalf("legal replay after larger seq / campaign end: %v", err)
	}
	cv, _ = s.GetCampaign(spec.ID)
	if len(cv.Results) != nResults {
		t.Fatal("legal replay added history")
	}
}

// 首次提交的回滚成功结果附带尾随内容：返回 ErrInvalidReport，设备仍停在
// rolling_back，影子与历史不变；合法配置随后可成功。
func TestRollbackSuccessTrailingConfigFirstTime(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rbSpec()
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb, err := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute))
	if err != nil || rb == nil || rb.Kind != StageRollback {
		t.Fatalf("claim rollback: %v %+v", err, rb)
	}
	cv0, _ := s.GetCampaign(spec.ID)
	nResults := len(cv0.Results)

	for _, cfg := range []string{`{} null`, `{"a":1} {}`} {
		err := s.SubmitResult(OperationResult{
			CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
			At: upBase.Add(9 * time.Minute), Success: true,
			Seq: 2, Version: "v1", Config: json.RawMessage(cfg),
		})
		if !errors.Is(err, ErrInvalidReport) {
			t.Fatalf("config %q: got %v, want ErrInvalidReport", cfg, err)
		}
	}
	cv, _ := s.GetCampaign(spec.ID)
	if d := findDevice(cv, "d1"); d.Status != DeviceRollingBack {
		t.Fatalf("device advanced on invalid rollback report: %s", d.Status)
	}
	if len(cv.Results) != nResults {
		t.Fatal("history changed on invalid rollback report")
	}

	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(9 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("legal rollback: %v", err)
	}
}

// 已接受的回滚成功结果重发时追加非空白内容：返回 ErrResultConflict；
// 合法重发（空白/字段顺序/数字等值）在设备上报更大序号后仍成功。
func TestRollbackSuccessReplayTrailingConflicts(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rbSpec()
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb, _ := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute))
	first := OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(9 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"a":1,"b":2}`),
	}
	if err := s.SubmitResult(first); err != nil {
		t.Fatalf("first rollback: %v", err)
	}
	cv, _ := s.GetCampaign(spec.ID)
	nResults := len(cv.Results)

	bad := first
	bad.Config = json.RawMessage(`{"a":1,"b":2} null`)
	if err := s.SubmitResult(bad); !errors.Is(err, ErrResultConflict) {
		t.Fatalf("rollback replay trailing: got %v, want ErrResultConflict", err)
	}
	cv, _ = s.GetCampaign(spec.ID)
	if len(cv.Results) != nResults {
		t.Fatal("conflicting rollback replay added history")
	}
	if d := findDevice(cv, "d1"); d.Status != DeviceRollbackSucceeded {
		t.Fatalf("accepted rollback altered: %+v", d)
	}

	// 设备后来普通上报更大序号（版本偏离锁定目标也不影响幂等判断）。
	if err := s.Report("d1", 3, upBase.Add(20*time.Minute), "v9", json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	legal := first
	legal.At = upBase.Add(30 * time.Minute)
	legal.Config = json.RawMessage(`{ "b": 2, "a": 1.0 }`)
	if err := s.SubmitResult(legal); err != nil {
		t.Fatalf("legal rollback replay: %v", err)
	}
	cv, _ = s.GetCampaign(spec.ID)
	if len(cv.Results) != nResults {
		t.Fatal("legal rollback replay added history")
	}
}

// 已领取安装但尚未接受结果，到达截止时间时提交附带非法配置的成功结果：
// 仍按截止结束活动并返回 ErrCampaignEnded，不改成配置错误；设备超时、
// 结果历史与影子均不变。
func TestClaimedInstallInvalidConfigAtDeadline(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	in := claimInstall(t, s, spec, "d1", upBase.Add(5*time.Minute))
	shadowBefore, _ := s.Get("d1")
	cv0, _ := s.GetCampaign(spec.ID)
	nResults := len(cv0.Results)

	err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in,
		At: upBase.Add(2 * time.Hour), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true} null`),
	})
	if !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("got %v, want ErrCampaignEnded", err)
	}
	cv, _ := s.GetCampaign(spec.ID)
	if !cv.Ended || cv.Status != CampaignFailed {
		t.Fatalf("campaign: %+v", cv)
	}
	if d := findDevice(cv, "d1"); d.Status != DeviceTimeout || d.Phase != StageInstall {
		t.Fatalf("device record: %+v", d)
	}
	if len(cv.Results) != nResults {
		t.Fatal("late result entered history")
	}
	after, _ := s.Get("d1")
	if after.LastSeq != shadowBefore.LastSeq || after.Version != shadowBefore.Version ||
		string(after.Reported) != string(shadowBefore.Reported) {
		t.Fatalf("shadow changed: before %+v after %+v", shadowBefore, after)
	}
}

// 活动已被显式推进到截止而结束后，已领取但未接受的安装结果（带非法配置）
// 仍返回 ErrCampaignEnded，而不是 ErrInvalidReport。
func TestClaimedInstallInvalidConfigAfterAdvanceDeadline(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	in := claimInstall(t, s, spec, "d1", upBase.Add(5*time.Minute))

	if err := s.AdvanceCampaign(spec.ID, upBase.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in,
		At: upBase.Add(2*time.Hour + time.Minute), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"ok":true} null`),
	})
	if !errors.Is(err, ErrCampaignEnded) {
		t.Fatalf("got %v, want ErrCampaignEnded", err)
	}
	cv, _ := s.GetCampaign(spec.ID)
	if d := findDevice(cv, "d1"); d.Status != DeviceTimeout {
		t.Fatalf("device record: %+v", d)
	}
}
