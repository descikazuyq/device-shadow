package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// 重复上报成功返回但不重新写入影子：设备已被 SetOffline 标为离线后，
// 同序号同内容（版本、发生时间、配置一致）的重发不得把设备重新置为在线；
// 更大序号的有效上报才重新上线。
func TestReportDuplicateKeepsOfflineAndDoesNotRewrite(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	at := base
	if err := s.Report("dev-1", 1, at, "1.1", json.RawMessage(`{"cpu":80}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOffline("dev-1"); err != nil {
		t.Fatal(err)
	}
	// 完全重复：成功返回，设备保持离线，影子不重写。
	if err := s.Report("dev-1", 1, at, "1.1", json.RawMessage(` { "cpu": 80 } `)); err != nil {
		t.Fatalf("duplicate report should succeed: %v", err)
	}
	v, _ := s.Get("dev-1")
	if v.Online {
		t.Fatal("duplicate report must not bring an offline device online")
	}
	if v.Version != "1.1" || v.LastSeq != 1 || string(v.Reported) != `{"cpu":80}` {
		t.Fatalf("duplicate report rewrote shadow: %+v", v)
	}
	// 同序号内容不同仍是冲突，不改变离线状态。
	if err := s.Report("dev-1", 1, at, "1.1", json.RawMessage(`{"cpu":81}`)); err == nil {
		t.Fatal("same seq with different content must conflict")
	}
	if v, _ = s.Get("dev-1"); v.Online {
		t.Fatal("conflicting report changed online state")
	}
	// 更大序号的有效上报重新上线并写入。
	if err := s.Report("dev-1", 2, at.Add(time.Minute), "1.2", json.RawMessage(`{"cpu":90}`)); err != nil {
		t.Fatal(err)
	}
	if v, _ = s.Get("dev-1"); !v.Online || v.Version != "1.2" || v.LastSeq != 2 {
		t.Fatalf("larger seq should update and bring online: %+v", v)
	}
}

// 安装成功附带的上报若已被普通上报以相同序号接受（版本、发生时间、配置均
// 一致），提交安装结果仍能完成设备与活动，但不重新写入影子：不刷新在线
// 状态（离线设备保持离线），也不刷新差异首次出现时间。
func TestInstallAttachedReportAcceptedByPlainReportCompletesWithoutRewrite(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")

	// 先制造一条持续存在的差异，记录其首次出现时间。
	desiredAt := upBase.Add(-2 * time.Minute)
	if _, err := s.UpdateDesired("d1", "bob", desiredAt, 0, json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	dl, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(3 * time.Minute), Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	in, _ := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute))

	// 普通上报先以安装附带上报将使用的同一序号/时间/版本/配置被接受。
	reportAt := upBase.Add(5 * time.Minute)
	cfg := json.RawMessage(`{"x":2}`)
	if err := s.Report("d1", 2, reportAt, "v2", cfg); err != nil {
		t.Fatal(err)
	}
	diffsBefore, _ := s.Diff("d1")
	sinceBefore := diffsBefore[0].Since
	if !sinceBefore.Equal(desiredAt) {
		t.Fatalf("/x should already exist since desired update: %v", sinceBefore)
	}
	// 设备随后被标为离线：附带的重复上报不得把它重新置为在线。
	if err := s.SetOffline("d1"); err != nil {
		t.Fatal(err)
	}
	cv0, _ := s.GetCampaign(spec.ID)
	nResults := len(cv0.Results)

	// 提交安装成功：附带上报与影子中已接受的内容完全一致，按重复处理，
	// 但安装操作本身正常完成、历史照常增加一条。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: reportAt, Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(` { "x": 2.0 } `),
	}); err != nil {
		t.Fatalf("install with already-accepted report should succeed: %v", err)
	}
	cv, _ := s.GetCampaign(spec.ID)
	d := findDevice(cv, "d1")
	if d.Status != DeviceSucceeded || !cv.Ended || cv.Status != CampaignSucceeded {
		t.Fatalf("install should complete: %+v device=%+v", cv, d)
	}
	if len(cv.Results) != nResults+1 {
		t.Fatalf("install result must enter history: %+v", cv.Results)
	}
	// 影子未被重新写入：设备保持离线，序号/版本/配置不变。
	v, _ := s.Get("d1")
	if v.Online {
		t.Fatal("duplicate attached report must not refresh online state")
	}
	if v.Version != "v2" || v.LastSeq != 2 || string(v.Reported) != `{"x":2}` {
		t.Fatalf("shadow rewritten by duplicate attached report: %+v", v)
	}
	// 差异仍在，且首次出现时间未被安装时间刷新。
	diffs, _ := s.Diff("d1")
	if len(diffs) != 1 || diffs[0].Path != "/x" || !diffs[0].Since.Equal(sinceBefore) {
		t.Fatalf("diff since refreshed: %+v", diffs)
	}
	// 已接受安装结果的重复提交仍成功且不增加历史。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: reportAt.Add(time.Hour), Success: true,
		Seq: 2, Version: "v2", Config: json.RawMessage(`{"x":2}`),
	}); err != nil {
		t.Fatalf("replay accepted install: %v", err)
	}
	if cv2, _ := s.GetCampaign(spec.ID); len(cv2.Results) != len(cv.Results) {
		t.Fatal("replay added history")
	}
}

// 回滚成功附带的上报已被普通上报以相同序号接受时，回滚仍能完成（活动因
// 回滚按规则失败结束），但影子不重新写入：离线设备保持离线，版本/序号/
// 上报配置不变；期望配置、修订号与审计不受影响。
func TestRollbackAttachedReportAcceptedByPlainReportCompletesWithoutRewrite(t *testing.T) {
	s := setupUpgrade(t, "d1")
	spec := rbSpec()
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	if _, err := s.UpdateDesired("d1", "bob", upBase.Add(-2*time.Minute), 0, json.RawMessage(`{"x":1}`)); err != nil {
		t.Fatal(err)
	}
	awaitRollback(t, s, spec, "d1", upBase.Add(2*time.Minute))
	rb, err := s.Claim(spec.ID, "d1", upBase.Add(6*time.Minute))
	if err != nil || rb == nil || rb.Kind != StageRollback {
		t.Fatalf("claim rollback: %v %+v", err, rb)
	}

	// 普通上报先接受回滚附带上报将使用的同一序号/时间/版本/配置。
	reportAt := upBase.Add(7 * time.Minute)
	if err := s.Report("d1", 2, reportAt, "v1", json.RawMessage(`{"x":2}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOffline("d1"); err != nil {
		t.Fatal(err)
	}
	cv0, _ := s.GetCampaign(spec.ID)
	nResults := len(cv0.Results)

	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: reportAt, Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"x": 2.0}`),
	}); err != nil {
		t.Fatalf("rollback with already-accepted report should succeed: %v", err)
	}
	cv, _ := s.GetCampaign(spec.ID)
	d := findDevice(cv, "d1")
	if d.Status != DeviceRollbackSucceeded {
		t.Fatalf("rollback should complete: %+v", d)
	}
	if !cv.Ended || cv.Status != CampaignFailed {
		t.Fatalf("campaign fails even after successful rollback: %+v", cv)
	}
	if len(cv.Results) != nResults+1 {
		t.Fatalf("rollback result must enter history: %+v", cv.Results)
	}
	v, _ := s.Get("d1")
	if v.Online {
		t.Fatal("duplicate attached rollback report must not refresh online state")
	}
	if v.Version != "v1" || v.LastSeq != 2 || string(v.Reported) != `{"x":2}` {
		t.Fatalf("shadow rewritten by duplicate attached report: %+v", v)
	}
	if v.Revision != 1 || string(v.Desired) != `{"x":1}` {
		t.Fatalf("desired/revision changed: %+v", v)
	}
	if auds, _ := s.Audit("d1"); len(auds) != 1 {
		t.Fatalf("audit changed: %+v", auds)
	}
	// 回滚结果接受后再改用不同配置重提：按已接受结果冲突拒绝（既有幂等
	// 语义），不增加历史、不改状态。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(8 * time.Minute), Success: true,
		Seq: 2, Version: "v1", Config: json.RawMessage(`{"x":9}`),
	}); !errors.Is(err, ErrResultConflict) {
		t.Fatalf("changed accepted result should conflict: %v", err)
	}
}
