package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"workbuddy2api-prime/internal/apikeys"
	"workbuddy2api-prime/internal/auth"
	"workbuddy2api-prime/internal/billing"
	"workbuddy2api-prime/internal/pool"
	"workbuddy2api-prime/internal/scheduler"
	"workbuddy2api-prime/internal/types"
)

func newTestPanel(t *testing.T) (*Panel, *http.ServeMux) {
	t.Helper()
	dir := t.TempDir()
	authStore, err := auth.NewStore(dir + "/auths")
	if err != nil {
		t.Fatal(err)
	}
	p := pool.NewPool(nil, pool.DefaultPoolConfig())
	sched, err := scheduler.New(func() []*types.Account { return nil }, dir+"/data",
		func(string, ...any) {})
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := billing.NewLedger(dir + "/data")
	if err != nil {
		t.Fatal(err)
	}
	keys, err := apikeys.NewStore(dir + "/data")
	if err != nil {
		t.Fatal(err)
	}
	pn := New(p, sched, ledger, keys, authStore)
	mux := http.NewServeMux()
	pn.RegisterRoutes(mux)
	return pn, mux
}

func get(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func TestIndexPage(t *testing.T) {
	_, mux := newTestPanel(t)
	rec := get(t, mux, "/panel/")
	if rec.Code != http.StatusOK {
		t.Fatalf("index code = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Fatalf("content-type = %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "workbuddy2api-prime") {
		t.Fatal("页面内容缺失")
	}
}

func TestAccountsEmpty(t *testing.T) {
	_, mux := newTestPanel(t)
	rec := get(t, mux, "/panel/api/accounts")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var list []accountView
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("期望空账号列表，实际 %d", len(list))
	}
}

func TestUsageEmpty(t *testing.T) {
	_, mux := newTestPanel(t)
	rec := get(t, mux, "/panel/api/usage?hours=24")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var sum billing.Summary
	if err := json.NewDecoder(rec.Body).Decode(&sum); err != nil {
		t.Fatal(err)
	}
	if sum.Requests != 0 {
		t.Fatalf("期望 0 请求，实际 %d", sum.Requests)
	}
}

func TestKeyLifecycle(t *testing.T) {
	_, mux := newTestPanel(t)

	// 新建
	req := httptest.NewRequest("POST", "/panel/api/keys",
		strings.NewReader(`{"name":"测试钥匙"}`))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var created struct {
		Plain string  `json:"plain"`
		Key   keyView `json:"key"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.Plain, "wbk_") {
		t.Fatalf("明文前缀错误: %q", created.Plain)
	}
	if created.Key.ID == "" {
		t.Fatal("返回的 key 缺少 ID")
	}

	// 列表应包含新建的钥匙
	rec = get(t, mux, "/panel/api/keys")
	var list []keyView
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != created.Key.ID {
		t.Fatalf("列表异常: %+v", list)
	}

	// 删除
	delReq := httptest.NewRequest("DELETE", "/panel/api/keys/"+created.Key.ID, nil)
	delRec := httptest.NewRecorder()
	mux.ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Fatalf("delete code = %d", delRec.Code)
	}

	// 列表应为空
	rec = get(t, mux, "/panel/api/keys")
	list = nil
	if err := json.NewDecoder(rec.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("删除后列表应为空，实际 %d", len(list))
	}
}

func TestTaskRunUnknown(t *testing.T) {
	_, mux := newTestPanel(t)
	req := httptest.NewRequest("POST", "/panel/api/tasks/notexist/run", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("未知任务应返回错误")
	}
}

func post(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func del(t *testing.T, mux *http.ServeMux, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("DELETE", path, nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestAccountDisableEnableRemove 覆盖停用/启用/删除接口（不碰上游）。
func TestAccountDisableEnableRemove(t *testing.T) {
	pn, mux := newTestPanel(t)
	acc := &types.Account{ID: "u1", UID: "u1", Region: "cn", AccessToken: "x"}
	if err := pn.auth.Save(acc); err != nil {
		t.Fatal(err)
	}
	pn.pool.AddAccount(acc)

	// 停用
	rec := post(t, mux, "/panel/api/accounts/u1/disable")
	if rec.Code != 200 {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body.String())
	}
	if !pn.pool.Get("u1").Disabled {
		t.Fatal("停用后 pool 未生效")
	}
	// 停用后不再参与选号
	if _, err := pn.pool.Pick(types.PickRequest{}); err == nil {
		t.Fatal("停用账号仍被选中")
	}
	// 启用
	rec = post(t, mux, "/panel/api/accounts/u1/enable")
	if rec.Code != 200 || pn.pool.Get("u1").Disabled {
		t.Fatalf("enable 失败: %d", rec.Code)
	}
	// 不存在的账号 → 404
	if rec := post(t, mux, "/panel/api/accounts/nope/disable"); rec.Code != 404 {
		t.Fatalf("want 404, got %d", rec.Code)
	}
	// 删除
	rec = del(t, mux, "/panel/api/accounts/u1")
	if rec.Code != 200 {
		t.Fatalf("remove: %d %s", rec.Code, rec.Body.String())
	}
	if pn.pool.Get("u1") != nil {
		t.Fatal("删除后 pool 仍有账号")
	}
	if _, err := pn.auth.Load("u1"); err == nil {
		t.Fatal("删除后凭证文件仍在")
	}
	if rec := del(t, mux, "/panel/api/accounts/u1"); rec.Code != 404 {
		t.Fatalf("重复删除 want 404, got %d", rec.Code)
	}
}

// TestCheckinAllTriggersTask 全量签到触发任务（异步，验证 accepted）。
func TestCheckinAllTriggersTask(t *testing.T) {
	pn, mux := newTestPanel(t)
	pn.sched.RegisterBuiltinTasks()
	ctx := context.Background()
	pn.sched.Start(ctx)
	defer pn.sched.Stop()
	rec := post(t, mux, "/panel/api/checkin_all")
	if rec.Code != 200 {
		t.Fatalf("checkin_all: %d %s", rec.Code, rec.Body.String())
	}
	var j map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &j); err != nil || j["started"] != true {
		t.Fatalf("bad resp: %s", rec.Body.String())
	}
}

// TestAccountBalanceNoUpstream 无 billing 桩时余额接口应 502 而非 panic。
func TestAccountBalanceNoUpstream(t *testing.T) {
	pn, mux := newTestPanel(t)
	acc := &types.Account{ID: "u1", UID: "u1", Region: "cn"}
	pn.pool.AddAccount(acc)
	pn.billing = nil // 模拟未初始化
	rec := post(t, mux, "/panel/api/accounts/u1/balance")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "余额刷新失败") {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
}
