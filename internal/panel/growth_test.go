// Package panel 的成长任务接口测试。
package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"workbuddy2api-prime/internal/upstream"
)

// TestGrowthRoutesRegistered 四个接口路由已注册且形状正确（引擎未注入时的降级行为）。
func TestGrowthRoutesRegistered(t *testing.T) {
	_, mux := newTestPanel(t)

	// queue：引擎未注入 → 空快照（不报错）
	rec := get(t, mux, "/panel/api/growth/queue")
	if rec.Code != http.StatusOK {
		t.Fatalf("queue 状态码=%d", rec.Code)
	}
	var q map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
		t.Fatalf("queue 解析: %v", err)
	}
	if _, ok := q["running"]; !ok {
		t.Errorf("queue 缺 running 字段: %s", rec.Body.String())
	}

	// overview：引擎未注入 → 500（带错误信息）
	rec = get(t, mux, "/panel/api/growth/overview")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("overview 未注入时应 500，got=%d", rec.Code)
	}

	// scan：引擎未注入 → 500
	req := httptest.NewRequest("POST", "/panel/api/growth/scan", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("scan 未注入时应 500，got=%d", rec.Code)
	}

	// run：引擎未注入 → 503
	req = httptest.NewRequest("POST", "/panel/api/growth/run", nil)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("run 未注入时应 503，got=%d", rec.Code)
	}
}

// TestGrowthQueueShape queue 响应字段完整。
func TestGrowthQueueShape(t *testing.T) {
	_, mux := newTestPanel(t)
	rec := get(t, mux, "/panel/api/growth/queue")
	var q struct {
		Running bool   `json:"running"`
		Total   int    `json:"total"`
		Done    int    `json:"done"`
		Current string `json:"current"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &q); err != nil {
		t.Fatalf("解析: %v", err)
	}
	if q.Running || q.Total != 0 || q.Done != 0 {
		t.Errorf("初始快照应全零: %+v", q)
	}
}

// TestGrowthTaskStatus 状态推算覆盖全分支。
func TestGrowthTaskStatus(t *testing.T) {
	cases := []struct {
		task upstream.Task
		want string
	}{
		{upstream.Task{AcceptStatus: "claimed", Claimed: true}, "claimed"},
		{upstream.Task{Locked: true}, "locked"},
		{upstream.Task{Target: 2, Current: 2, Claimable: true}, "claimable"},
		{upstream.Task{Target: 2, Current: 0}, "pending"},
		{upstream.Task{Target: 0, Current: 0}, "pending"},
	}
	for i, c := range cases {
		if got := growthTaskStatus(c.task); got != c.want {
			t.Errorf("case %d: got=%s want=%s", i, got, c.want)
		}
	}
}
