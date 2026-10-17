package panel

import (
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
