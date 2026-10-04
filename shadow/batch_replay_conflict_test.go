package shadow

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// 已成功的请求标识被用于不同内容时，冲突判断只按首次提交的内容进行，
// 不依赖本次引用的设备是否仍登记：更换或增加未登记设备、列表中已登记
// 与未登记设备混杂，都返回 ErrRequestConflict 而不是 ErrDeviceNotFound。
func TestBatchConflictWithUnregisteredDevices(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustRegister(t, s, "dev-2", "1.0")
	original := []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"b":2}`)},
	}
	first := mustBatch(t, s, "req-1", "alice", base, original)

	cases := []struct {
		name    string
		devices []BatchDevice
	}{
		{"switched to unregistered device", []BatchDevice{
			{DeviceID: "ghost", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
			{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{"b":2}`)},
		}},
		{"appended unregistered device", append(append([]BatchDevice{}, original...),
			BatchDevice{DeviceID: "ghost", Revision: 0, Config: json.RawMessage(`{}`)})},
		{"registered and unregistered devices only", []BatchDevice{
			{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{}`)},
			{DeviceID: "ghost", Revision: 0, Config: json.RawMessage(`{}`)},
		}},
		{"only an unregistered device", []BatchDevice{
			{DeviceID: "ghost", Revision: 0, Config: json.RawMessage(`{}`)},
		}},
	}
	for _, tc := range cases {
		if _, err := s.BatchUpdateDesired("req-1", "alice", base, tc.devices); !errors.Is(err, ErrRequestConflict) {
			t.Fatalf("%s: got %v, want ErrRequestConflict", tc.name, err)
		}
	}

	// 冲突后原请求记录仍可查询：操作者、提交时间与各设备首次修订号不变。
	got, err := s.GetBatchRequest("req-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Operator != "alice" || !got.Time.Equal(base) || len(got.Revisions) != 2 ||
		got.Revisions["dev-1"] != first.Revisions["dev-1"] ||
		got.Revisions["dev-2"] != first.Revisions["dev-2"] {
		t.Fatalf("original record changed by conflicts: %+v vs %+v", got, first)
	}
	// 所有已登记设备的期望配置、修订号与审计均不受影响。
	for _, id := range []string{"dev-1", "dev-2"} {
		v, _ := s.Get(id)
		if v.Revision != 1 {
			t.Fatalf("%s revision changed: %+v", id, v)
		}
		if recs, _ := s.Audit(id); len(recs) != 1 || recs[0].RequestID != "req-1" {
			t.Fatalf("%s audit changed: %+v", id, recs)
		}
	}
	if v1, _ := s.Get("dev-1"); string(v1.Desired) != `{"a":1}` {
		t.Fatalf("dev-1 desired changed: %s", v1.Desired)
	}
	if v2, _ := s.Get("dev-2"); string(v2.Desired) != `{"b":2}` {
		t.Fatalf("dev-2 desired changed: %s", v2.Desired)
	}

	// 冲突与失败提交都没有替换原记录：原始内容重发仍返回首次成功结果。
	again := mustBatch(t, s, "req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-2", Revision: 0, Config: json.RawMessage(`{ "b": 2.0 }`)},
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1.0}`)},
	})
	if again.Revisions["dev-1"] != 1 || again.Revisions["dev-2"] != 1 {
		t.Fatalf("original resubmit result: %+v", again.Revisions)
	}
	for _, id := range []string{"dev-1", "dev-2"} {
		if recs, _ := s.Audit(id); len(recs) != 1 {
			t.Fatalf("%s audit grew on resubmit: %+v", id, recs)
		}
	}
}

// 即使相关设备后来已被单独修改，已成功请求的冲突仍按最初携带的修订号
// 与配置识别；引用未登记设备同样报冲突，而不是按当前状态报别的错误。
func TestBatchConflictAfterDeviceMovedOn(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	mustBatch(t, s, "req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
	})
	if _, err := s.UpdateDesired("dev-1", "bob", base.Add(time.Hour), 1,
		json.RawMessage(`{"new":true}`)); err != nil {
		t.Fatal(err)
	}
	// 原内容重发：返回首次结果，不覆盖较新期望状态。
	again := mustBatch(t, s, "req-1", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
	})
	if again.Revisions["dev-1"] != 1 {
		t.Fatalf("stale resubmit result: %+v", again.Revisions)
	}
	// 换一台未登记设备：仍是冲突，不检查它的登记状态或修订号。
	if _, err := s.BatchUpdateDesired("req-1", "alice", base, []BatchDevice{
		{DeviceID: "ghost", Revision: 99, Config: json.RawMessage(`{"a":1}`)},
	}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("conflict after move-on: %v", err)
	}
	v, _ := s.Get("dev-1")
	if v.Revision != 2 || string(v.Desired) != `{"new":true}` {
		t.Fatalf("newer state overwritten: %+v", v)
	}
	if recs, _ := s.Audit("dev-1"); len(recs) != 2 {
		t.Fatalf("audit changed: %+v", recs)
	}
}

// 标识已有成功记录时，请求自身格式非法仍按输入错误拒绝，不能因为标识
// 存在（或引用的设备未登记）就跳过必填信息、重复设备或配置格式检查；
// 这些失败不删除原记录，之后重发原始内容仍得到首次成功结果。
func TestBatchExistingRecordStillValidatesRequestFormat(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	original := []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
	}
	first := mustBatch(t, s, "req-1", "alice", base, original)

	goodCfg := json.RawMessage(`{"a":1}`)
	cases := []struct {
		name    string
		op      string
		at      time.Time
		devices []BatchDevice
		want    error
	}{
		{"empty operator", "", base, []BatchDevice{
			{DeviceID: "ghost", Revision: 0, Config: goodCfg},
		}, ErrInvalidOperator},
		{"zero time", "alice", time.Time{}, []BatchDevice{
			{DeviceID: "ghost", Revision: 0, Config: goodCfg},
		}, ErrInvalidTime},
		{"empty device list", "alice", base, nil, ErrInvalidDeviceList},
		{"empty device id", "alice", base, []BatchDevice{
			{DeviceID: "", Revision: 0, Config: goodCfg},
		}, ErrInvalidDeviceID},
		{"duplicate unregistered device", "alice", base, []BatchDevice{
			{DeviceID: "ghost", Revision: 0, Config: goodCfg},
			{DeviceID: "ghost", Revision: 0, Config: goodCfg},
		}, ErrDuplicateDevice},
		{"config not an object", "alice", base, []BatchDevice{
			{DeviceID: "ghost", Revision: 0, Config: json.RawMessage(`[1]`)},
		}, ErrInvalidConfig},
		{"config with trailing content", "alice", base, []BatchDevice{
			{DeviceID: "ghost", Revision: 0, Config: json.RawMessage(`{"a":1} null`)},
		}, ErrInvalidConfig},
	}
	for _, tc := range cases {
		if _, err := s.BatchUpdateDesired("req-1", tc.op, tc.at, tc.devices); !errors.Is(err, tc.want) {
			t.Fatalf("%s: got %v, want %v", tc.name, err, tc.want)
		}
		// 失败提交不影响原记录。
		got, err := s.GetBatchRequest("req-1")
		if err != nil {
			t.Fatalf("%s: original record lost: %v", tc.name, err)
		}
		if got.Operator != first.Operator || !got.Time.Equal(first.Time) ||
			got.Revisions["dev-1"] != 1 {
			t.Fatalf("%s: original record altered: %+v", tc.name, got)
		}
	}
	// 全部失败之后，原始内容重发仍返回首次成功结果。
	again := mustBatch(t, s, "req-1", "alice", base, original)
	if again.Revisions["dev-1"] != 1 {
		t.Fatalf("original resubmit after rejected submissions: %+v", again.Revisions)
	}
	if recs, _ := s.Audit("dev-1"); len(recs) != 1 {
		t.Fatalf("audit grew from rejected submissions: %+v", recs)
	}
}

// 尚未成功使用过的标识引用未登记设备时执行整批校验：返回
// ErrDeviceNotFound，不修改其他设备，也不留下成功请求记录；
// 修正设备列表后可继续使用同一标识提交。
func TestBatchFreshIDUnknownDeviceWholeBatchFails(t *testing.T) {
	s, _ := openTemp(t)
	mustRegister(t, s, "dev-1", "1.0")
	_, err := s.BatchUpdateDesired("req-new", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
		{DeviceID: "ghost", Revision: 0, Config: json.RawMessage(`{"z":9}`)},
	})
	if !errors.Is(err, ErrDeviceNotFound) {
		t.Fatalf("got %v, want ErrDeviceNotFound", err)
	}
	v, _ := s.Get("dev-1")
	if v.Revision != 0 || string(v.Desired) != "{}" {
		t.Fatalf("dev-1 modified by failed batch: %+v", v)
	}
	if recs, _ := s.Audit("dev-1"); len(recs) != 0 {
		t.Fatalf("dev-1 audit left by failed batch: %+v", recs)
	}
	if _, err := s.GetBatchRequest("req-new"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("failed request queryable: %v", err)
	}
	// 修正（去掉未登记设备）后同一标识可成功提交。
	rec := mustBatch(t, s, "req-new", "alice", base, []BatchDevice{
		{DeviceID: "dev-1", Revision: 0, Config: json.RawMessage(`{"a":1}`)},
	})
	if rec.Revisions["dev-1"] != 1 {
		t.Fatalf("retry with same id: %+v", rec.Revisions)
	}
}
