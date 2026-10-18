package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"workbuddy2api-prime/internal/types"
)

func TestMeterPaths(t *testing.T) {
	cn := meterPaths(testAccount("cn"), billingResPath, billingResPathV2)
	if len(cn) != 1 || cn[0] != "/v2/billing/meter/get-user-resource" {
		t.Fatalf("cn paths = %v", cn)
	}
	intl := meterPaths(testAccount("intl"), billingResPath, billingResPathV2)
	if len(intl) != 2 || intl[0] != "/billing/meter/get-user-resource" || intl[1] != "/v2/billing/meter/get-user-resource" {
		t.Fatalf("intl paths = %v", intl)
	}
}

func testAccount(region string) *types.Account {
	return &types.Account{ID: "u1", UID: "u1", Region: region, AccessToken: "tok", DeviceFingerprint: "fp"}
}

func TestAggregateBalance(t *testing.T) {
	now := time.Date(2026, 10, 17, 0, 0, 0, 0, cst)
	accts := []resourceAccount{
		// Cycle 期优先：remain 取 CycleCapacityRemain=30
		{PackageName: "p1", CycleEndTime: "2026-12-01 00:00:00",
			CapacitySize: 50, CapacityRemain: 20, CapacityUsed: 30,
			CycleCapacitySize: 100, CycleCapacityRemain: 30, CycleCapacityUsed: 70},
		// 无 Cycle 期：回退 Capacity；无到期时间 → 不计入 earliest
		{PackageName: "p2", CycleEndTime: "",
			CapacitySize: 50, CapacityRemain: 20, CapacityUsed: 30},
		// 已过期包：仍计入 remain/total，但不计入 earliest
		{PackageName: "p3", CycleEndTime: "2020-01-01 00:00:00",
			CapacitySize: 10, CapacityRemain: 5, CapacityUsed: 5},
	}
	remain, total, earliest := aggregateBalance(accts, now)
	if remain != 55 || total != 160 {
		t.Fatalf("remain/total = %d/%d, want 55/160", remain, total)
	}
	want, _ := parsePackageEndTime("2026-12-01 00:00:00")
	if !earliest.Equal(want) {
		t.Fatalf("earliest = %v, want %v", earliest, want)
	}
}

func TestParseBillingResp(t *testing.T) {
	b := NewBilling()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"ok","data":{"a":1}}`))
	}))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL, nil)
	resp, err := b.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, err := parseBillingResp(resp)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]int
	if err := json.Unmarshal(data, &m); err != nil || m["a"] != 1 {
		t.Fatalf("data = %s", data)
	}

	// code != 0 → BillingError，且 10001 识别为已签到
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":10001,"msg":"今天已签到","data":null}`))
	}))
	defer srv2.Close()
	req2, _ := http.NewRequest("POST", srv2.URL, nil)
	resp2, _ := b.http.Do(req2)
	_, err = parseBillingResp(resp2)
	resp2.Body.Close()
	be, ok := err.(*BillingError)
	if !ok || be.Code != 10001 || !be.AlreadyCheckedIn() {
		t.Fatalf("want AlreadyCheckedIn BillingError, got %v", err)
	}
}

func TestPackageRemainUsed(t *testing.T) {
	// Cycle 期优先
	r, _, size := packageRemainUsed(resourceAccount{CycleCapacitySize: 100, CycleCapacityRemain: 30, CycleCapacityUsed: 70})
	if r != 30 || size != 100 {
		t.Fatalf("cycle: got %d/%d", r, size)
	}
	// remain 钳制
	r, _, _ = packageRemainUsed(resourceAccount{CycleCapacitySize: 100, CycleCapacityRemain: 150})
	if r != 100 {
		t.Fatalf("clamp: got %d", r)
	}
	// 回退 Capacity
	r, _, size = packageRemainUsed(resourceAccount{CapacitySize: 50, CapacityRemain: 20, CapacityUsed: 30})
	if r != 20 || size != 50 {
		t.Fatalf("capacity: got %d/%d", r, size)
	}
}

func TestDailyCheckinUnsupportedRegion(t *testing.T) {
	b := NewBilling()
	if err := b.DailyCheckin(testAccount("intl")); err != ErrCheckinUnsupported {
		t.Fatalf("want ErrCheckinUnsupported, got %v", err)
	}
}

func TestParsePackageEndTime(t *testing.T) {
	tm, ok := parsePackageEndTime("2026-12-01 00:00:00")
	if !ok || tm.Year() != 2026 {
		t.Fatalf("parse failed: %v %v", tm, ok)
	}
	if _, ok := parsePackageEndTime(""); ok {
		t.Fatal("empty should fail")
	}
	if _, ok := parsePackageEndTime("not-a-time"); ok {
		t.Fatal("garbage should fail")
	}
}
