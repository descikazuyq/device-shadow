package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestRollbackTwoDevicesIndependentFlow 回归同一活动、同一批次内两台从不同旧版本
// 升级的设备，安装失败后各自回滚的完整行为：
//   - 活动开启安装失败回滚，目标版本同时允许两个旧版本（恢复目标无需另行登记）；
//   - 两台设备在线，首次领取下载均发生在维护窗口内，全部结果早于截止时间；
//   - 首次领取下载分别锁定各自当时的版本为回滚目标，两份回滚待办操作标识互不相同；
//   - 两台先后完成下载、领取安装后分别提交原因不同的安装失败：第一台失败时
//     第二台仍能就自己已领取的安装提交结果，两台的失败原因与时间各自保留；
//   - 先让后失败的设备回滚成功：其版本/上报配置/回滚状态按自己的结果更新，
//     另一台仍保留原回滚待办，活动继续执行、结束时间为空；另一台随后回滚成功，
//     活动才以失败结束；两台均为回滚成功，恢复旧版本不算本次升级成功；
//   - 各自的期望配置、修订号与审计保持升级前内容；
//   - 两种归属相关的拒绝（提交他人的回滚标识、用自己的标识填他人锁定的版本）
//     分别返回 ErrOperationNotFound 与 ErrInvalidReport，且不改变影子、回滚进度
//     与活动历史，之后两台仍能用各自正确的结果完成回滚；
//   - 有效结果历史按实际接受顺序保留所属设备、操作标识、阶段、成败与恢复版本，
//     每台设备的下载、安装失败、回滚成功各留一条。
func TestRollbackTwoDevicesIndependentFlow(t *testing.T) {
	const (
		campaignID = "cmp-pair"
		dA         = "pa"
		dB         = "pb"
		verA       = "firmware-1.0" // A 升级前的旧版本
		verB       = "firmware-1.3" // B 升级前的旧版本（与 A 不同）
		target     = "firmware-2.0"
	)

	s, _ := openTemp(t)
	if err := s.RegisterUpgrade(target, []string{verA, verB}); err != nil {
		t.Fatalf("RegisterUpgrade: %v", err)
	}
	// 回滚恢复的两个旧版本不另行登记为升级目标。
	mustRegister(t, s, dA, verA)
	mustRegister(t, s, dB, verB)

	// 固定时间线（UTC）：11:00 下发期望 -> 11:10 创建活动 ->
	// 窗口 11:20–12:20（[start,end)）-> 截止 13:20。
	t0 := time.Date(2026, 10, 3, 11, 0, 0, 0, time.UTC)
	desiredAt := t0
	created := t0.Add(10 * time.Minute)
	windowStart := t0.Add(20 * time.Minute)
	windowEnd := windowStart.Add(time.Hour)
	deadline := windowEnd.Add(time.Hour)
	spec := CampaignSpec{
		ID: campaignID, Operator: "ops-bot", CreatedAt: created,
		TargetVersion: target, Devices: []string{dA, dB}, BatchSize: 2,
		WindowStart: windowStart, WindowEnd: windowEnd, Deadline: deadline,
		RollbackOnFailure: true,
	}

	// 升级前两台各有一份已修改的期望配置（修订号 0 -> 1），留存副本。
	desiredA := json.RawMessage(`{"mode":"safe","telemetry":{"intervalSec":30}}`)
	desiredB := json.RawMessage(`{"mode":"fast","telemetry":{"intervalSec":5}}`)
	for _, c := range []struct {
		id  string
		cfg json.RawMessage
	}{
		{dA, desiredA},
		{dB, desiredB},
	} {
		if rev, err := s.UpdateDesired(c.id, "ops-bot", desiredAt, 0, c.cfg); err != nil || rev != 1 {
			t.Fatalf("UpdateDesired %s: %v rev=%d", c.id, err, rev)
		}
	}
	// 两台设备在线（上报序号 1，配置各自不同，版本为各自旧版本）。
	if err := s.Report(dA, 1, windowStart.Add(-time.Minute), verA,
		json.RawMessage(`{"mode":"safe","telemetry":{"intervalSec":60}}`)); err != nil {
		t.Fatalf("report A online: %v", err)
	}
	if err := s.Report(dB, 1, windowStart.Add(-time.Minute), verB,
		json.RawMessage(`{"mode":"fast","telemetry":{"intervalSec":10}}`)); err != nil {
		t.Fatalf("report B online: %v", err)
	}
	createCampaign(t, s, spec)

	// ---- 两台先后完成下载、领取安装 ---------------------------------------
	// A：领取下载（首次领取即锁定 verA）、下载成功、领取安装。
	dlA, err := s.Claim(campaignID, dA, windowStart.Add(2*time.Minute))
	if err != nil || dlA == nil || dlA.Kind != StageDownload {
		t.Fatalf("A claim download: %v %+v", err, dlA)
	}
	if d := findDevice(mustGetCampaign(s, campaignID), dA); d.RollbackTarget != verA {
		t.Fatalf("A target locked at first download claim: %+v", d)
	}
	dlAAt := windowStart.Add(3 * time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: campaignID, DeviceID: dA, OperationID: dlA.ID,
		At: dlAAt, Success: true,
	}); err != nil {
		t.Fatalf("A download result: %v", err)
	}
	inA, err := s.Claim(campaignID, dA, windowStart.Add(4*time.Minute))
	if err != nil || inA == nil || inA.Kind != StageInstall {
		t.Fatalf("A claim install: %v %+v", err, inA)
	}

	// B：领取下载（锁定 verB）、下载成功、领取安装。此时 A 尚未提交失败。
	dlB, err := s.Claim(campaignID, dB, windowStart.Add(5*time.Minute))
	if err != nil || dlB == nil || dlB.Kind != StageDownload {
		t.Fatalf("B claim download: %v %+v", err, dlB)
	}
	if d := findDevice(mustGetCampaign(s, campaignID), dB); d.RollbackTarget != verB {
		t.Fatalf("B target locked at first download claim: %+v", d)
	}
	dlBAt := windowStart.Add(6 * time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: campaignID, DeviceID: dB, OperationID: dlB.ID,
		At: dlBAt, Success: true,
	}); err != nil {
		t.Fatalf("B download result: %v", err)
	}
	inB, err := s.Claim(campaignID, dB, windowStart.Add(7*time.Minute))
	if err != nil || inB == nil || inB.Kind != StageInstall {
		t.Fatalf("B claim install: %v %+v", err, inB)
	}

	// ---- 分别提交原因不同的安装失败 ---------------------------------------
	// 第一台（A）失败。
	failAAt := windowStart.Add(8 * time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: campaignID, DeviceID: dA, OperationID: inA.ID,
		At: failAAt, Success: false, Reason: "A checksum mismatch",
	}); err != nil {
		t.Fatalf("A install failure: %v", err)
	}
	// A 失败后，第二台（B）仍能就自己已领取的安装提交结果（同样失败）。
	if st := deviceStatus(mustGetCampaign(s, campaignID), dB); st != DeviceInstalling {
		t.Fatalf("B keeps its claimed install after A failure: %s", st)
	}
	failBAt := windowStart.Add(9 * time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: campaignID, DeviceID: dB, OperationID: inB.ID,
		At: failBAt, Success: false, Reason: "B power loss during install",
	}); err != nil {
		t.Fatalf("B install failure: %v", err)
	}

	// ---- 两份回滚待办：目标分别为各自锁定版本，标识互不相同 ---------------
	cv := mustGetCampaign(s, campaignID)
	va := findDevice(cv, dA)
	vb := findDevice(cv, dB)
	if va.Status != DeviceAwaitingRollback || va.Phase != StageRollback ||
		vb.Status != DeviceAwaitingRollback || vb.Phase != StageRollback {
		t.Fatalf("both awaiting rollback: A=%+v B=%+v", va, vb)
	}
	if va.RollbackTarget != verA || vb.RollbackTarget != verB {
		t.Fatalf("rollback targets are the versions at each device's first download claim: A=%q B=%q",
			va.RollbackTarget, vb.RollbackTarget)
	}
	if va.RollbackID == "" || va.RollbackID == vb.RollbackID {
		t.Fatalf("rollback op ids must differ: A=%q B=%q", va.RollbackID, vb.RollbackID)
	}
	if va.RollbackID == va.DownloadID || va.RollbackID == va.InstallID ||
		vb.RollbackID == vb.DownloadID || vb.RollbackID == vb.InstallID {
		t.Fatal("rollback op id must differ from download/install ids")
	}
	// 两台的安装失败原因和时间各自保留，不被另一台的失败记录替换。
	if va.InstallFailReason != "A checksum mismatch" || !va.InstallFailAt.Equal(failAAt) {
		t.Fatalf("A install failure record kept: %+v", va)
	}
	if vb.InstallFailReason != "B power loss during install" || !vb.InstallFailAt.Equal(failBAt) {
		t.Fatalf("B install failure record kept: %+v", vb)
	}
	if cv.Ended || cv.Status != CampaignRunning || !cv.EndedAt.IsZero() {
		t.Fatalf("campaign waits for both rollbacks: %+v", cv)
	}
	for _, x := range []struct {
		id, target string
	}{
		{dA, verA},
		{dB, verB},
	} {
		w, err := s.GetDeviceWork(x.id)
		if err != nil {
			t.Fatalf("work %s: %v", x.id, err)
		}
		if w.Pending == nil || w.Pending.Kind != StageRollback || w.PendingClaimed ||
			w.Pending.TargetVersion != x.target {
			t.Fatalf("unclaimed rollback todo for %s: %+v", x.id, w)
		}
	}

	// ---- 领取两台设备的回滚（均在维护窗口内） -----------------------------
	rbA, err := s.Claim(campaignID, dA, windowStart.Add(10*time.Minute))
	if err != nil || rbA == nil || rbA.Kind != StageRollback || rbA.TargetVersion != verA {
		t.Fatalf("A claim rollback: %v %+v", err, rbA)
	}
	if rbA.ID != va.RollbackID {
		t.Fatalf("A rollback id matches todo: claim=%q todo=%q", rbA.ID, va.RollbackID)
	}
	rbB, err := s.Claim(campaignID, dB, windowStart.Add(11*time.Minute))
	if err != nil || rbB == nil || rbB.Kind != StageRollback || rbB.TargetVersion != verB {
		t.Fatalf("B claim rollback: %v %+v", err, rbB)
	}
	if rbB.ID != vb.RollbackID || rbB.ID == rbA.ID {
		t.Fatalf("B rollback id matches its todo and differs from A: %+v", rbB)
	}

	// ---- 两种与设备归属直接相关的拒绝 -------------------------------------
	// 快照两台影子、回滚进度与活动历史，随后断言两次被拒绝的提交不改变任何内容。
	type shadowSnap struct {
		version  string
		seq      uint64
		online   bool
		revision uint64
		desired  string
		reported string
	}
	snap := func(id string) shadowSnap {
		t.Helper()
		sh, err := s.Get(id)
		if err != nil {
			t.Fatalf("snapshot %s: %v", id, err)
		}
		return shadowSnap{sh.Version, sh.LastSeq, sh.Online, sh.Revision,
			string(sh.Desired), string(sh.Reported)}
	}
	snapA0, snapB0 := snap(dA), snap(dB)
	hist0 := append([]ResultRecord(nil), mustGetCampaign(s, campaignID).Results...)

	// 拒绝 1：以 A 的身份提交 B 已领取的回滚操作标识 -> ErrOperationNotFound。
	if err := s.SubmitResult(OperationResult{
		CampaignID: campaignID, DeviceID: dA, OperationID: rbB.ID,
		At: windowStart.Add(12 * time.Minute), Success: true,
		Seq: 2, Version: verB, Config: json.RawMessage(`{"restored":"B"}`),
	}); !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("submit other device's rollback id: want ErrOperationNotFound, got %v", err)
	}
	// 拒绝 2：A 使用自己的回滚标识，却在成功结果中填入 B 锁定的恢复版本
	// -> ErrInvalidReport。
	if err := s.SubmitResult(OperationResult{
		CampaignID: campaignID, DeviceID: dA, OperationID: rbA.ID,
		At: windowStart.Add(13 * time.Minute), Success: true,
		Seq: 2, Version: verB, Config: json.RawMessage(`{"restored":"wrong"}`),
	}); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("rollback success with other device's locked version: want ErrInvalidReport, got %v", err)
	}

	// 两次提交都不能改变任一设备的影子、回滚进度或活动历史。
	if got := snap(dA); got != snapA0 {
		t.Fatalf("A shadow changed after rejected submits: %+v vs %+v", got, snapA0)
	}
	if got := snap(dB); got != snapB0 {
		t.Fatalf("B shadow changed after rejected submits: %+v vs %+v", got, snapB0)
	}
	cvRej := mustGetCampaign(s, campaignID)
	if st := deviceStatus(cvRej, dA); st != DeviceRollingBack {
		t.Fatalf("A rollback progress changed after rejects: %s", st)
	}
	if st := deviceStatus(cvRej, dB); st != DeviceRollingBack {
		t.Fatalf("B rollback progress changed after rejects: %s", st)
	}
	if len(cvRej.Results) != len(hist0) {
		t.Fatalf("history length changed after rejects: %d vs %d", len(cvRej.Results), len(hist0))
	}
	for i := range hist0 {
		if cvRej.Results[i] != hist0[i] {
			t.Fatalf("history[%d] changed after rejects: %+v vs %+v", i, cvRej.Results[i], hist0[i])
		}
	}
	if cvRej.Ended {
		t.Fatal("campaign must not end after rejected submits")
	}

	// ---- 后发生安装失败的设备（B）先回滚成功 ------------------------------
	rbCfgB := json.RawMessage(`{"mode":"fast","telemetry":{"intervalSec":10},"restored":true}`)
	rbBAt := windowStart.Add(14 * time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: campaignID, DeviceID: dB, OperationID: rbB.ID,
		At: rbBAt, Success: true,
		Seq: 2, Version: verB, Config: rbCfgB,
	}); err != nil {
		t.Fatalf("B valid rollback: %v", err)
	}
	shB, _ := s.Get(dB)
	if shB.Version != verB || shB.LastSeq != 2 || !shB.Online || string(shB.Reported) != string(rbCfgB) {
		t.Fatalf("B current version/reported config updated by its rollback: %+v", shB)
	}
	cv2 := mustGetCampaign(s, campaignID)
	dvb := findDevice(cv2, dB)
	if dvb.Status != DeviceRollbackSucceeded || dvb.Phase != StageRollback ||
		!dvb.RollbackResult || !dvb.RollbackSuccess || !dvb.RollbackAt.Equal(rbBAt) {
		t.Fatalf("B rollback status: %+v", dvb)
	}
	// A 仍有原来的回滚待办（已领取、原标识、原锁定目标）；活动继续执行，结束时间为空。
	if st := deviceStatus(cv2, dA); st != DeviceRollingBack {
		t.Fatalf("A still rolling back while B finished: %s", st)
	}
	wA, _ := s.GetDeviceWork(dA)
	if wA.Pending == nil || wA.Pending.Kind != StageRollback || !wA.PendingClaimed ||
		wA.Pending.ID != rbA.ID || wA.Pending.TargetVersion != verA {
		t.Fatalf("A keeps its original rollback todo: %+v", wA)
	}
	if cv2.Ended || cv2.Status != CampaignRunning || !cv2.EndedAt.IsZero() {
		t.Fatalf("campaign still running with empty end time while A pending: %+v", cv2)
	}
	// B 的期望配置、修订号与审计保持升级前内容。
	if shB.Revision != 1 || string(shB.Desired) != string(desiredB) {
		t.Fatalf("B desired/revision preserved: rev=%d desired=%s", shB.Revision, shB.Desired)
	}
	if auds, _ := s.Audit(dB); len(auds) != 1 {
		t.Fatalf("B audit preserved: %+v", auds)
	}

	// ---- 另一台（A）随后提交合规回滚成功，活动才以失败结束 ----------------
	rbCfgA := json.RawMessage(`{"mode":"safe","telemetry":{"intervalSec":60},"restored":true}`)
	rbAAt := windowStart.Add(15 * time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: campaignID, DeviceID: dA, OperationID: rbA.ID,
		At: rbAAt, Success: true,
		Seq: 2, Version: verA, Config: rbCfgA,
	}); err != nil {
		t.Fatalf("A valid rollback: %v", err)
	}
	shA, _ := s.Get(dA)
	if shA.Version != verA || shA.LastSeq != 2 || !shA.Online || string(shA.Reported) != string(rbCfgA) {
		t.Fatalf("A current version/reported config updated by its rollback: %+v", shA)
	}
	final := mustGetCampaign(s, campaignID)
	if !final.Ended || final.Status != CampaignFailed || !final.EndedAt.Equal(rbAAt) {
		t.Fatalf("campaign ends failed only after A rollback: %+v", final)
	}
	// 两台均显示回滚成功；恢复旧版本不把本次升级算成成功。
	for _, x := range []struct {
		id, ver string
	}{
		{dA, verA},
		{dB, verB},
	} {
		d := findDevice(final, x.id)
		if d.Status != DeviceRollbackSucceeded || d.Phase != StageRollback ||
			!d.RollbackResult || !d.RollbackSuccess {
			t.Fatalf("%s shows rollback success (not upgrade success): %+v", x.id, d)
		}
		if d.Status == DeviceSucceeded {
			t.Fatalf("%s restoring old version must not count as upgrade success", x.id)
		}
		if d.RollbackTarget != x.ver {
			t.Fatalf("%s keeps its own rollback target: %+v", x.id, d)
		}
	}
	// 各自的期望配置、修订号和审计保持原有内容。
	if shA.Revision != 1 || string(shA.Desired) != string(desiredA) {
		t.Fatalf("A desired/revision preserved: rev=%d desired=%s", shA.Revision, shA.Desired)
	}
	if auds, _ := s.Audit(dA); len(auds) != 1 {
		t.Fatalf("A audit preserved: %+v", auds)
	}
	if shB, _ = s.Get(dB); shB.Revision != 1 || string(shB.Desired) != string(desiredB) {
		t.Fatalf("B desired/revision preserved at end: rev=%d desired=%s", shB.Revision, shB.Desired)
	}
	if auds, _ := s.Audit(dB); len(auds) != 1 {
		t.Fatalf("B audit preserved at end: %+v", auds)
	}
	// 活动结束后两台均无待办。
	for _, id := range []string{dA, dB} {
		w, _ := s.GetDeviceWork(id)
		if w.Pending != nil || w.CampaignID != "" {
			t.Fatalf("%s has no work after campaign ended: %+v", id, w)
		}
	}

	// ---- 有效结果历史按实际接受顺序保留 -----------------------------------
	// 每台设备的下载、安装失败、回滚成功各一条；被拒绝的两次提交不留痕。
	wantHistory := []struct {
		device, op, stage string
		success           bool
		version, reason   string
		at                time.Time
	}{
		{dA, dlA.ID, StageDownload, true, "", "", dlAAt},
		{dB, dlB.ID, StageDownload, true, "", "", dlBAt},
		{dA, inA.ID, StageInstall, false, "", "A checksum mismatch", failAAt},
		{dB, inB.ID, StageInstall, false, "", "B power loss during install", failBAt},
		{dB, rbB.ID, StageRollback, true, verB, "", rbBAt},
		{dA, rbA.ID, StageRollback, true, verA, "", rbAAt},
	}
	if len(final.Results) != len(wantHistory) {
		t.Fatalf("history length = %d, want %d: %+v", len(final.Results), len(wantHistory), final.Results)
	}
	for i, want := range wantHistory {
		got := final.Results[i]
		if got.DeviceID != want.device || got.OperationID != want.op || got.Stage != want.stage ||
			got.Success != want.success || got.Version != want.version ||
			got.Reason != want.reason || !got.At.Equal(want.at) {
			t.Fatalf("history[%d] = %+v\nwant device=%s op=%s stage=%s success=%t version=%q reason=%q at=%v",
				i, got, want.device, want.op, want.stage, want.success,
				want.version, want.reason, want.at)
		}
	}
}
