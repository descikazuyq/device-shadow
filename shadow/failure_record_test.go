package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// findResultRecord 返回结果历史中指定操作标识的记录及是否存在。
func findResultRecord(cv CampaignView, opID string) (ResultRecord, bool) {
	for _, r := range cv.Results {
		if r.OperationID == opID {
			return r, true
		}
	}
	return ResultRecord{}, false
}

// 三种失败（下载失败、开启回滚时的安装失败、回滚失败）接受后遵循同一记录规则：
// 结果历史恰好增加一条，对应该设备、该阶段操作，保留首次接受的原因与时间；
// 相同原因的重复提交（即使提交时间变化、活动已经结束）不再增加记录，
// 改用不同原因仍返回冲突。
func TestFailureResultsFollowUnifiedRecordRule(t *testing.T) {
	// 开启回滚：安装失败 -> 等待回滚 -> 回滚失败。
	s := setupUpgrade(t, "d1")
	spec := rbSpec()
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")

	dl, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	dlOK := upBase.Add(3 * time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: dlOK, Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	in, _ := s.Claim(spec.ID, "d1", upBase.Add(4*time.Minute))
	installFailAt := upBase.Add(5 * time.Minute)
	installFail := OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: in.ID,
		At: installFailAt, Success: false, Reason: "install broken",
	}
	if err := s.SubmitResult(installFail); err != nil {
		t.Fatalf("install failure: %v", err)
	}

	cv, _ := s.GetCampaign(spec.ID)
	rec, ok := findResultRecord(cv, in.ID)
	if !ok {
		t.Fatalf("install failure record missing: %+v", cv.Results)
	}
	if rec.DeviceID != "d1" || rec.Stage != StageInstall || rec.Success ||
		rec.Reason != "install broken" || !rec.At.Equal(installFailAt) || rec.Version != "" {
		t.Fatalf("install failure record: %+v", rec)
	}
	nAfterInstall := len(cv.Results)

	// 相同原因、更晚时间重复提交：成功且不增加历史、不重复触发回滚。
	installFail.At = upBase.Add(40 * time.Minute)
	if err := s.SubmitResult(installFail); err != nil {
		t.Fatalf("replay install failure: %v", err)
	}
	if cv, _ = s.GetCampaign(spec.ID); len(cv.Results) != nAfterInstall {
		t.Fatalf("replay install failure added history: %+v", cv.Results)
	}
	// 改用不同原因：冲突，原记录（原因与首次时间）不变。
	installFail.At = upBase.Add(41 * time.Minute)
	installFail.Reason = "install broken differently"
	if err := s.SubmitResult(installFail); !errors.Is(err, ErrResultConflict) {
		t.Fatalf("changed install reason conflict: %v", err)
	}
	cv, _ = s.GetCampaign(spec.ID)
	if rec, _ = findResultRecord(cv, in.ID); rec.Reason != "install broken" || !rec.At.Equal(installFailAt) {
		t.Fatalf("original install failure altered: %+v", rec)
	}
	if findDevice(cv, "d1").Status != DeviceAwaitingRollback {
		t.Fatalf("conflict must not change state: %+v", findDevice(cv, "d1"))
	}

	// 回滚失败：同一条统一规则下的另一条记录，保留自己的原因与时间。
	rb, _ := s.Claim(spec.ID, "d1", upBase.Add(8*time.Minute))
	rbFailAt := upBase.Add(9 * time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: rbFailAt, Success: false, Reason: "rb boom",
	}); err != nil {
		t.Fatalf("rollback failure: %v", err)
	}
	// 活动已结束后，相同回滚失败原因以更晚时间重放：仍成功且不增加历史。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: rb.ID,
		At: upBase.Add(3 * time.Hour), Success: false, Reason: "rb boom",
	}); err != nil {
		t.Fatalf("replay rollback failure after end: %v", err)
	}

	cv, _ = s.GetCampaign(spec.ID)
	if !cv.Ended || cv.Status != CampaignFailed {
		t.Fatalf("campaign ends rollback-failed: %+v", cv)
	}
	// 下载成功、安装失败、回滚失败各一条，共三条。
	if len(cv.Results) != 3 {
		t.Fatalf("history should hold exactly 3 records: %+v", cv.Results)
	}
	rbRec, ok := findResultRecord(cv, rb.ID)
	if !ok {
		t.Fatalf("rollback failure record missing: %+v", cv.Results)
	}
	if rbRec.DeviceID != "d1" || rbRec.Stage != StageRollback || rbRec.Success ||
		rbRec.Reason != "rb boom" || !rbRec.At.Equal(rbFailAt) || rbRec.Version != "" {
		t.Fatalf("rollback failure record: %+v", rbRec)
	}
	// 安装失败原记录仍在、内容不变。
	inRec, _ := findResultRecord(cv, in.ID)
	if inRec.Reason != "install broken" || !inRec.At.Equal(installFailAt) {
		t.Fatalf("install failure record changed after rollback: %+v", inRec)
	}
}

// 未开启回滚时的下载失败（含首次领取时版本不兼容的自动失败）同样遵循统一
// 记录规则：一条对应下载操作的失败历史，记录原因与首次接受时间，可重放、
// 不因换原因被覆盖，且不产生任何回滚记录。
func TestDownloadFailureFollowsUnifiedRecordRule(t *testing.T) {
	check := func(t *testing.T, cv CampaignView, dlID, wantReason string, failAt time.Time) {
		t.Helper()
		rec, ok := findResultRecord(cv, dlID)
		if !ok {
			t.Fatalf("download failure record missing: %+v", cv.Results)
		}
		if rec.DeviceID != "d1" || rec.Stage != StageDownload || rec.Success ||
			rec.Reason != wantReason || !rec.At.Equal(failAt) || rec.Version != "" {
			t.Fatalf("download failure record: %+v", rec)
		}
		if len(cv.Results) != 1 {
			t.Fatalf("download failure adds exactly one record: %+v", cv.Results)
		}
		if d := findDevice(cv, "d1"); d.Status != DeviceFailed || d.Phase != StageDownload ||
			d.RollbackResult || d.RollbackTarget != "" {
			t.Fatalf("download failure must not rollback: %+v", d)
		}
	}

	// 普通下载失败。
	s := setupUpgrade(t, "d1")
	spec := upSpec()
	spec.Devices = []string{"d1"}
	spec.BatchSize = 1
	createCampaign(t, s, spec)
	bringOnline(t, s, "d1")
	dl, _ := s.Claim(spec.ID, "d1", upBase.Add(2*time.Minute))
	failAt := upBase.Add(3 * time.Minute)
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: failAt, Success: false, Reason: "dl down",
	}); err != nil {
		t.Fatal(err)
	}
	// 活动结束后相同原因、变化时间重放：保留原历史。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(3 * time.Hour), Success: false, Reason: "dl down",
	}); err != nil {
		t.Fatalf("replay download failure after end: %v", err)
	}
	// 不同原因仍冲突。
	if err := s.SubmitResult(OperationResult{
		CampaignID: spec.ID, DeviceID: "d1", OperationID: dl.ID,
		At: upBase.Add(3*time.Hour + time.Minute), Success: false, Reason: "other",
	}); !errors.Is(err, ErrResultConflict) {
		t.Fatalf("changed download reason conflict: %v", err)
	}
	cv, _ := s.GetCampaign(spec.ID)
	check(t, cv, dl.ID, "dl down", failAt)

	// 首次领取下载时版本不兼容：领取即接受下载失败，走同一条记录规则。
	s2 := setupUpgrade(t, "d1")
	spec2 := upSpec()
	spec2.Devices = []string{"d1"}
	spec2.BatchSize = 1
	createCampaign(t, s2, spec2)
	bringOnline(t, s2, "d1")
	if err := s2.Report("d1", 2, upBase.Add(time.Minute), "v9", json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	incompatAt := upBase.Add(2 * time.Minute)
	if _, err := s2.Claim(spec2.ID, "d1", incompatAt); !errors.Is(err, ErrIncompatibleVersion) {
		t.Fatalf("incompatible claim: %v", err)
	}
	cv2, _ := s2.GetCampaign(spec2.ID)
	dlID2 := findDevice(cv2, "d1").DownloadID
	wantReason := "version v9 is not compatible with target v2"
	check(t, cv2, dlID2, wantReason, incompatAt)
}
