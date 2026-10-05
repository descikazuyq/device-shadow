package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// TestTwoDeviceRollbackFromDistinctOldVersions 回归同一活动、同一批次内两台设备
// 从各自不同的旧版本升级、先后安装失败后各自回滚的完整行为。
//
// 时间线（相对 upBase：窗口 [12:00,13:00)，截止 14:00）：
//
//	-2m/-90s 两台设备升级前各下发一份已修改的期望配置（修订号均为 1）
//	 12:02   dev-a 首次领取下载，锁定回滚目标 firmware-1.0-a
//	 12:03   dev-a 下载成功
//	 12:04   dev-a 领取安装
//	 12:05   dev-b 首次领取下载，锁定回滚目标 firmware-1.0-b
//	 12:06   dev-b 下载成功
//	 12:07   dev-b 领取安装
//	 12:08   dev-a 安装失败（原因 alpha）；dev-b 已领取的安装不受影响
//	 12:09   dev-b 安装失败（原因 beta）；两份回滚待办并存
//	 12:10/11 两台设备先后领取各自的回滚（操作标识互不相同）
//	 12:20   以 dev-a 身份提交 dev-b 已领取的回滚标识 -> ErrOperationNotFound
//	 12:21   dev-a 用自己的回滚标识、却填 dev-b 的恢复版本 -> ErrInvalidReport
//	 12:25   后失败的 dev-b 先提交合规回滚成功（恢复 v1-b）；活动继续执行
//	 12:30   dev-a 提交合规回滚成功（恢复 v1-a）；活动才以失败结束
//
// v1-a/v1-b 都只是目标版本允许的升级来源，不另行登记为升级目标。
func TestTwoDeviceRollbackFromDistinctOldVersions(t *testing.T) {
	s, _ := openTemp(t)
	const (
		cid    = "cmp-2dev"
		d1     = "dev-a"
		d2     = "dev-b"
		oldV1  = "firmware-1.0-a"
		oldV2  = "firmware-1.0-b"
		target = "firmware-2.0"
	)
	// 目标版本同时允许两个不同的旧版本；恢复用的旧版本不另行登记。
	if err := s.RegisterUpgrade(target, []string{oldV1, oldV2}); err != nil {
		t.Fatalf("RegisterUpgrade: %v", err)
	}
	mustRegister(t, s, d1, oldV1)
	mustRegister(t, s, d2, oldV2)

	// 升级前两台设备各有一份已修改的期望配置，留存副本用于最终核对。
	desired1 := json.RawMessage(`{"channel":1,"mode":"a"}`)
	desired2 := json.RawMessage(`{"channel":2,"mode":"b"}`)
	if rev, err := s.UpdateDesired(d1, "ops-a", upBase.Add(-2*time.Minute), 0, desired1); err != nil || rev != 1 {
		t.Fatalf("d1 update desired: rev=%d err=%v", rev, err)
	}
	if rev, err := s.UpdateDesired(d2, "ops-b", upBase.Add(-90*time.Second), 0, desired2); err != nil || rev != 1 {
		t.Fatalf("d2 update desired: rev=%d err=%v", rev, err)
	}

	spec := upSpec()
	spec.ID = cid
	spec.TargetVersion = target
	spec.Devices = []string{d1, d2}
	spec.BatchSize = 2 // 两台设备同属批次 0
	spec.RollbackOnFailure = true
	createCampaign(t, s, spec)
	// 用各自旧版本的正整数序号上报让两台设备上线（不能复用硬编码 v1 的 bringOnline）。
	if err := s.Report(d1, 1, upBase.Add(-time.Minute), oldV1, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("d1 online report: %v", err)
	}
	if err := s.Report(d2, 1, upBase.Add(-time.Minute), oldV2, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("d2 online report: %v", err)
	}

	dv := func(id string) DeviceStatusView {
		t.Helper()
		return findDevice(mustGetCampaign(s, cid), id)
	}
	claim := func(id string, at time.Time, wantStage string) *Operation {
		t.Helper()
		op, err := s.Claim(cid, id, at)
		if err != nil {
			t.Fatalf("claim %s %s at %v: %v", id, wantStage, at, err)
		}
		if op == nil || op.Kind != wantStage {
			t.Fatalf("claim %s %s: got %+v", id, wantStage, op)
		}
		return op
	}
	submit := func(r OperationResult) {
		t.Helper()
		if err := s.SubmitResult(r); err != nil {
			t.Fatalf("submit result: %v", err)
		}
	}

	// 两台设备在同一批次。
	if a, b := dv(d1), dv(d2); a.Batch != 0 || b.Batch != 0 {
		t.Fatalf("devices must share batch 0: %+v %+v", a, b)
	}

	// dev-a 先后完成下载、领取安装；首次领取下载锁定其当时版本 v1-a。
	dl1 := claim(d1, upBase.Add(2*time.Minute), StageDownload)
	if tgt := dv(d1).RollbackTarget; tgt != oldV1 {
		t.Fatalf("d1 rollback target locked at first download claim: %q", tgt)
	}
	submit(OperationResult{
		CampaignID: cid, DeviceID: d1, OperationID: dl1.ID,
		At: upBase.Add(3 * time.Minute), Success: true,
	})
	in1 := claim(d1, upBase.Add(4*time.Minute), StageInstall)

	// dev-b 同样在窗口内领取下载（锁定 v1-b）、完成下载、领取安装。
	dl2 := claim(d2, upBase.Add(5*time.Minute), StageDownload)
	if tgt := dv(d2).RollbackTarget; tgt != oldV2 {
		t.Fatalf("d2 rollback target locked at first download claim: %q", tgt)
	}
	submit(OperationResult{
		CampaignID: cid, DeviceID: d2, OperationID: dl2.ID,
		At: upBase.Add(6 * time.Minute), Success: true,
	})
	in2 := claim(d2, upBase.Add(7*time.Minute), StageInstall)

	// dev-a 先安装失败；原因/时间保留，转入等待回滚。
	fail1At := upBase.Add(8 * time.Minute)
	submit(OperationResult{
		CampaignID: cid, DeviceID: d1, OperationID: in1.ID,
		At: fail1At, Success: false, Reason: "install fail alpha",
	})
	if a := dv(d1); a.Status != DeviceAwaitingRollback || a.Phase != StageRollback ||
		a.InstallFailReason != "install fail alpha" || !a.InstallFailAt.Equal(fail1At) {
		t.Fatalf("d1 awaiting rollback with own failure: %+v", a)
	}
	// dev-a 失败时，dev-b 仍能查到并完成自己已领取的安装（同一标识）。
	out, err := s.Claim(cid, d2, upBase.Add(8*time.Minute+30*time.Second))
	if err != nil || out == nil || out.ID != in2.ID || out.Kind != StageInstall {
		t.Fatalf("d2 claimed install must survive d1 failure: %v %+v", err, out)
	}
	if b := dv(d2); b.Status != DeviceInstalling || b.InstallFailReason != "" {
		t.Fatalf("d2 keeps installing untouched by d1 failure: %+v", b)
	}

	// dev-b 随后提交自己的、原因不同的安装失败；两台设备的失败原因与时间
	// 各自保留，不能被另一台的失败记录替换。
	fail2At := upBase.Add(9 * time.Minute)
	submit(OperationResult{
		CampaignID: cid, DeviceID: d2, OperationID: in2.ID,
		At: fail2At, Success: false, Reason: "install fail beta",
	})
	v := mustGetCampaign(s, cid)
	if v.Ended || v.Status != CampaignRunning || !v.EndedAt.IsZero() {
		t.Fatalf("campaign must wait for both rollbacks: %+v", v)
	}
	a, b := dv(d1), dv(d2)
	if a.Status != DeviceAwaitingRollback || a.RollbackTarget != oldV1 ||
		a.InstallFailReason != "install fail alpha" || !a.InstallFailAt.Equal(fail1At) {
		t.Fatalf("d1 failure record overwritten by d2: %+v", a)
	}
	if b.Status != DeviceAwaitingRollback || b.RollbackTarget != oldV2 ||
		b.InstallFailReason != "install fail beta" || !b.InstallFailAt.Equal(fail2At) {
		t.Fatalf("d2 failure record: %+v", b)
	}
	// 两份回滚待办并存：目标是各自首次领取下载时的版本，操作标识互不相同，
	// 且与各自的下载、安装标识区分。
	rb1Want := encodeOperationIDV2(0, cid, d1, StageRollback)
	rb2Want := encodeOperationIDV2(0, cid, d2, StageRollback)
	if a.RollbackID != rb1Want || b.RollbackID != rb2Want ||
		rb1Want == rb2Want ||
		rb1Want == dl1.ID || rb1Want == in1.ID ||
		rb2Want == dl2.ID || rb2Want == in2.ID {
		t.Fatalf("rollback ids must be distinct per device/stage: %q %q (dl1=%s in1=%s dl2=%s in2=%s)",
			a.RollbackID, b.RollbackID, dl1.ID, in1.ID, dl2.ID, in2.ID)
	}
	for _, tc := range []struct {
		id, target string
	}{{d1, oldV1}, {d2, oldV2}} {
		w, err := s.GetDeviceWork(tc.id)
		if err != nil {
			t.Fatalf("work %s: %v", tc.id, err)
		}
		if w.Pending == nil || w.Pending.Kind != StageRollback || w.PendingClaimed ||
			w.Pending.TargetVersion != tc.target {
			t.Fatalf("unclaimed rollback todo for %s: %+v", tc.id, w)
		}
	}
	// 安装失败阶段两台设备的影子版本仍是各自旧版本，序号沿用现有规则。
	if sh, _ := s.Get(d1); sh.Version != oldV1 || sh.LastSeq != 1 {
		t.Fatalf("d1 shadow after install failure: %+v", sh)
	}
	if sh, _ := s.Get(d2); sh.Version != oldV2 || sh.LastSeq != 1 {
		t.Fatalf("d2 shadow after install failure: %+v", sh)
	}

	// 两台设备先后在窗口内领取各自的回滚。
	rb1 := claim(d1, upBase.Add(10*time.Minute), StageRollback)
	if rb1.ID != rb1Want || rb1.TargetVersion != oldV1 {
		t.Fatalf("d1 rollback op: %+v", rb1)
	}
	rb2 := claim(d2, upBase.Add(11*time.Minute), StageRollback)
	if rb2.ID != rb2Want || rb2.TargetVersion != oldV2 {
		t.Fatalf("d2 rollback op: %+v", rb2)
	}

	// 两种与设备归属直接相关的拒绝行为，均不得改变任一设备的影子、
	// 回滚进度或活动历史。
	nHistory := len(mustGetCampaign(s, cid).Results) // 两条下载成功 + 两条安装失败
	// 1) 以 dev-a 身份提交 dev-b 已领取的回滚操作标识：ErrOperationNotFound。
	err = s.SubmitResult(OperationResult{
		CampaignID: cid, DeviceID: d1, OperationID: rb2.ID,
		At: upBase.Add(20 * time.Minute), Success: true,
		Seq: 2, Version: oldV1, Config: json.RawMessage(`{}`),
	})
	if !errors.Is(err, ErrOperationNotFound) {
		t.Fatalf("submit other device's rollback op: want ErrOperationNotFound, got %v", err)
	}
	// 2) dev-a 使用自己的回滚标识，却在成功结果里填入 dev-b 锁定的恢复版本：
	//    序号与配置本身合规，唯一问题是恢复版本不属于本设备 -> ErrInvalidReport。
	err = s.SubmitResult(OperationResult{
		CampaignID: cid, DeviceID: d1, OperationID: rb1.ID,
		At: upBase.Add(21 * time.Minute), Success: true,
		Seq: 2, Version: oldV2, Config: json.RawMessage(`{"channel":1,"mode":"a"}`),
	})
	if !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("rollback to other device's locked version: want ErrInvalidReport, got %v", err)
	}
	// 两次拒绝后：两台设备仍是正在回滚、无回滚结果，安装失败记录原样保留，
	// 影子不变，历史不增，活动继续执行。
	cv := mustGetCampaign(s, cid)
	if cv.Ended || cv.Status != CampaignRunning || len(cv.Results) != nHistory {
		t.Fatalf("rejections changed campaign: %+v history=%d", cv, len(cv.Results))
	}
	for _, tc := range []struct {
		id, old, reason string
		failAt          time.Time
	}{
		{d1, oldV1, "install fail alpha", fail1At},
		{d2, oldV2, "install fail beta", fail2At},
	} {
		d := findDevice(cv, tc.id)
		if d.Status != DeviceRollingBack || d.RollbackResult ||
			d.InstallFailReason != tc.reason || !d.InstallFailAt.Equal(tc.failAt) {
			t.Fatalf("%s rollback progress changed by rejected submissions: %+v", tc.id, d)
		}
		if sh, _ := s.Get(tc.id); sh.Version != tc.old || sh.LastSeq != 1 {
			t.Fatalf("%s shadow changed by rejected submissions: %+v", tc.id, sh)
		}
	}

	// 后发生安装失败的 dev-b 先提交回滚成功：附带自己的恢复版本和完整配置。
	rbCfg2 := json.RawMessage(`{"channel":2,"mode":"b","restored":true}`)
	rb2At := upBase.Add(25 * time.Minute)
	submit(OperationResult{
		CampaignID: cid, DeviceID: d2, OperationID: rb2.ID,
		At: rb2At, Success: true,
		Seq: 2, Version: oldV2, Config: rbCfg2,
	})
	// dev-b 当前版本、上报配置与回滚状态随结果更新；期望配置/修订号不动。
	sh2, _ := s.Get(d2)
	if sh2.Version != oldV2 || !sh2.Online || sh2.LastSeq != 2 || !rawEqual(sh2.Reported, rbCfg2) {
		t.Fatalf("d2 shadow after rollback success: %+v", sh2)
	}
	if sh2.Revision != 1 || !rawEqual(sh2.Desired, desired2) {
		t.Fatalf("d2 desired changed by rollback: rev=%d desired=%s", sh2.Revision, sh2.Desired)
	}
	b = dv(d2)
	if b.Status != DeviceRollbackSucceeded || b.Phase != StageRollback ||
		!b.RollbackResult || !b.RollbackSuccess || !b.RollbackAt.Equal(rb2At) ||
		b.RollbackReason != "" ||
		b.InstallFailReason != "install fail beta" || !b.InstallFailAt.Equal(fail2At) {
		t.Fatalf("d2 rollback-succeeded record: %+v", b)
	}
	// dev-a 仍保留原来的回滚待办（已领取未完成），活动继续执行、结束时间为空。
	a = dv(d1)
	if a.Status != DeviceRollingBack || a.RollbackResult || a.RollbackTarget != oldV1 {
		t.Fatalf("d1 must keep its own rollback pending: %+v", a)
	}
	w1, _ := s.GetDeviceWork(d1)
	if w1.Pending == nil || w1.Pending.Kind != StageRollback || !w1.PendingClaimed ||
		w1.Pending.ID != rb1.ID || w1.Pending.TargetVersion != oldV1 {
		t.Fatalf("d1 rollback todo after d2 success: %+v", w1)
	}
	if cv = mustGetCampaign(s, cid); cv.Ended || cv.Status != CampaignRunning || !cv.EndedAt.IsZero() {
		t.Fatalf("campaign still runs while d1 rolls back: %+v", cv)
	}

	// dev-a 再提交合规回滚成功，活动这才结束；恢复旧版本不算本次升级成功。
	rbCfg1 := json.RawMessage(`{"channel":1,"mode":"a","restored":true}`)
	rb1At := upBase.Add(30 * time.Minute)
	submit(OperationResult{
		CampaignID: cid, DeviceID: d1, OperationID: rb1.ID,
		At: rb1At, Success: true,
		Seq: 2, Version: oldV1, Config: rbCfg1,
	})
	cv = mustGetCampaign(s, cid)
	if !cv.Ended || cv.Status != CampaignFailed || !cv.EndedAt.Equal(rb1At) {
		t.Fatalf("campaign ends failed only after both rollbacks: %+v", cv)
	}
	a, b = dv(d1), dv(d2)
	if a.Status != DeviceRollbackSucceeded || b.Status != DeviceRollbackSucceeded {
		t.Fatalf("both devices rollback_succeeded: %+v %+v", a, b)
	}
	if a.Status == DeviceSucceeded || b.Status == DeviceSucceeded {
		t.Fatal("restoring an old version must not count the upgrade as succeeded")
	}
	if !a.RollbackAt.Equal(rb1At) || !b.RollbackAt.Equal(rb2At) ||
		a.InstallFailReason != "install fail alpha" || b.InstallFailReason != "install fail beta" {
		t.Fatalf("rollback/install failure records at end: %+v %+v", a, b)
	}

	// 两台设备影子：版本回到各自锁定旧版本、上报为各自完整配置；
	// 期望配置、修订号与审计保持升级前的原有内容。
	sh1, _ := s.Get(d1)
	if sh1.Version != oldV1 || !sh1.Online || sh1.LastSeq != 2 || !rawEqual(sh1.Reported, rbCfg1) {
		t.Fatalf("d1 shadow after rollback: %+v", sh1)
	}
	if sh1.Revision != 1 || !rawEqual(sh1.Desired, desired1) {
		t.Fatalf("d1 desired/revision not preserved: %+v", sh1)
	}
	if sh2.Revision != 1 || !rawEqual(sh2.Desired, desired2) {
		t.Fatalf("d2 desired/revision not preserved: %+v", sh2)
	}
	for _, tc := range []struct {
		id, operator string
		after        json.RawMessage
	}{
		{d1, "ops-a", desired1},
		{d2, "ops-b", desired2},
	} {
		auds, err := s.Audit(tc.id)
		if err != nil {
			t.Fatalf("audit %s: %v", tc.id, err)
		}
		if len(auds) != 1 || auds[0].Revision != 1 || auds[0].Operator != tc.operator ||
			string(auds[0].Before) != "{}" || !rawEqual(auds[0].After, tc.after) {
			t.Fatalf("%s audit must keep original content: %+v", tc.id, auds)
		}
	}

	// 有效结果历史按实际接受顺序保留所属设备、操作标识、阶段、成败与恢复版本；
	// 每台设备的下载成功、安装失败、回滚成功各留一条，共 6 条。
	wantHistory := []ResultRecord{
		{DeviceID: d1, OperationID: dl1.ID, Stage: StageDownload, Success: true, At: upBase.Add(3 * time.Minute)},
		{DeviceID: d2, OperationID: dl2.ID, Stage: StageDownload, Success: true, At: upBase.Add(6 * time.Minute)},
		{DeviceID: d1, OperationID: in1.ID, Stage: StageInstall, Success: false, Reason: "install fail alpha", At: fail1At},
		{DeviceID: d2, OperationID: in2.ID, Stage: StageInstall, Success: false, Reason: "install fail beta", At: fail2At},
		{DeviceID: d2, OperationID: rb2.ID, Stage: StageRollback, Success: true, Version: oldV2, At: rb2At},
		{DeviceID: d1, OperationID: rb1.ID, Stage: StageRollback, Success: true, Version: oldV1, At: rb1At},
	}
	if len(cv.Results) != len(wantHistory) {
		t.Fatalf("history len: want %d, got %d: %+v", len(wantHistory), len(cv.Results), cv.Results)
	}
	perDevice := map[string]int{}
	for i, want := range wantHistory {
		got := cv.Results[i]
		if got.DeviceID != want.DeviceID || got.OperationID != want.OperationID ||
			got.Stage != want.Stage || got.Success != want.Success ||
			got.Reason != want.Reason || got.Version != want.Version || !got.At.Equal(want.At) {
			t.Fatalf("history[%d]:\n want %+v\n got  %+v", i, want, got)
		}
		perDevice[got.DeviceID]++
	}
	if perDevice[d1] != 3 || perDevice[d2] != 3 {
		t.Fatalf("each device must keep exactly 3 records: %+v", perDevice)
	}
}
