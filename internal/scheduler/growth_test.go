// Package scheduler 的成长任务引擎测试（mock 上游：任务解析、点亮链路、跳过规则）。
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"workbuddy2api-prime/internal/types"
	"workbuddy2api-prime/internal/upstream"
)

// ---------------------------------------------------------------------------
// 纯逻辑测试（无网络）
// ---------------------------------------------------------------------------

func TestGrowthPending(t *testing.T) {
	cases := []struct {
		name string
		task upstream.Task
		want bool
	}{
		{"待办", upstream.Task{TaskCode: "x", Target: 5, Current: 3}, true},
		{"已领取", upstream.Task{TaskCode: "x", Claimed: true, Target: 1, Current: 1}, false},
		{"锁定", upstream.Task{TaskCode: "x", Locked: true, Target: 2, Current: 0}, false},
		{"进度达标", upstream.Task{TaskCode: "x", Target: 2, Current: 2}, false},
		{"已完成", upstream.Task{TaskCode: "x", AcceptStatus: "completed"}, false},
		{"无target待办", upstream.Task{TaskCode: "x", AcceptStatus: "accepted"}, true},
	}
	for _, c := range cases {
		if got := growthPending(c.task); got != c.want {
			t.Errorf("%s: got=%v want=%v", c.name, got, c.want)
		}
	}
}

func TestAutoActionCoverage(t *testing.T) {
	// 24 个动作码全部有实现。
	want := []string{
		"chat_5", "first_buddy", "Model_chat_GLM5.2", "RichMeow_Chat",
		"Buddy_App", "Buddy_App_QQ", "automation_1", "Library_read",
		"template_5", "playbook_prompt", "create_canvas", "expert_5",
		"Expert_team_use_3", "Hp_Appearance", "skill_1", "Expert_lighthouse",
		"black_cat", "school_season",
		"Sequential_Tasks_1", "Sequential_Tasks_2", "Sequential_Tasks_3",
		"Sequential_Tasks_4", "Sequential_Tasks_5", "Sequential_Tasks_6",
		"Sequential_Tasks_7",
	}
	if len(autoActions) != len(want) {
		t.Errorf("动作数=%d，期望 %d", len(autoActions), len(want))
	}
	for _, code := range want {
		if autoActionFor(code) == nil {
			t.Errorf("缺少动作: %s", code)
		}
	}
	// 不可自动化：未知码、需真实捐款的 Expert_Philanthropy。
	if autoActionFor("whatever") != nil {
		t.Error("未知码应返回 nil")
	}
	if autoActionFor("Expert_Philanthropy") != nil {
		t.Error("Expert_Philanthropy（真实捐款）必须无动作")
	}
}

func TestGrowthAccountOK(t *testing.T) {
	ok := &types.Account{ID: "1", UID: "u1", Region: "cn"}
	if !growthAccountOK(ok) {
		t.Error("正常 cn 账号应通过")
	}
	cases := []struct {
		name string
		acct *types.Account
	}{
		{"停用", &types.Account{ID: "2", UID: "u2", Region: "cn", Disabled: true}},
		{"global", &types.Account{ID: "3", UID: "u3", Region: "intl"}},
		{"企业号", &types.Account{ID: "4", UID: "u4", Region: "cn", EnterpriseID: "ent-1"}},
	}
	for _, c := range cases {
		if growthAccountOK(c.acct) {
			t.Errorf("%s 应被跳过", c.name)
		}
	}
	if growthAccountOK(nil) {
		t.Error("nil 应被跳过")
	}
}

func TestIsMPTaskCode(t *testing.T) {
	for _, code := range []string{"school_season", "Sequential_Tasks_1", "Sequential_Tasks_7"} {
		if !isMPTaskCode(code) {
			t.Errorf("%s 应为 mp 口径", code)
		}
	}
	if isMPTaskCode("chat_5") {
		t.Error("chat_5 不应为 mp 口径")
	}
}

// ---------------------------------------------------------------------------
// mock 上游测试
// ---------------------------------------------------------------------------

// growthMock 桩：任务列表/领奖/上报；统计请求数。
type growthMock struct {
	requests  atomic.Int64
	listData  string
	claimData string
}

func (m *growthMock) handler(w http.ResponseWriter, r *http.Request) {
	m.requests.Add(1)
	write := func(data string) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"code":0,"msg":"ok","data":%s}`, data)
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks") && r.Method == http.MethodGet:
		write(m.listData)
	case strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks/accept"):
		write(`{}`)
	case strings.Contains(r.URL.Path, "/claim"):
		write(m.claimData)
	case r.URL.Path == "/v2/report":
		write(`{}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"code":404,"msg":"not found"}`)
	}
}

func newGrowthEngine(t *testing.T, m *growthMock) *GrowthEngine {
	srv := httptest.NewServer(http.HandlerFunc(m.handler))
	t.Cleanup(srv.Close)
	upstream.SetGrowthTestBases(srv.URL, srv.URL)
	return &GrowthEngine{growth: upstream.NewGrowth(), logf: t.Logf}
}

// TestRunOnceSkipsIneligible 跳过规则：停用/global/企业号账号零网络请求。
func TestRunOnceSkipsIneligible(t *testing.T) {
	m := &growthMock{listData: `{"tasks":[]}`, claimData: `{}`}
	e := newGrowthEngine(t, m)
	accounts := []*types.Account{
		{ID: "1", UID: "u1", Region: "cn", Disabled: true},
		{ID: "2", UID: "u2", Region: "intl"},
		{ID: "3", UID: "u3", Region: "cn", EnterpriseID: "ent-1"},
		nil,
	}
	if !e.RunOnce(context.Background(), accounts) {
		t.Fatal("RunOnce 应返回 true")
	}
	if n := m.requests.Load(); n != 0 {
		t.Errorf("被跳过的账号产生了 %d 次上游请求，期望 0", n)
	}
	if p := e.Progress(); p.Running || p.Total != 0 {
		t.Errorf("进度快照错误: %+v", p)
	}
}

// TestRunOnceUnknownTaskSkipped 无动作任务被跳过、不阻塞队列。
func TestRunOnceUnknownTaskSkipped(t *testing.T) {
	m := &growthMock{
		listData:  `{"tasks":[{"task_code":"mystery_task","target":1,"current":0,"accept_status":"accepted"}]}`,
		claimData: `{}`,
	}
	e := newGrowthEngine(t, m)
	accts := []*types.Account{{ID: "1", UID: "u1", Region: "cn", AccessToken: "tok"}}
	if !e.RunOnce(context.Background(), accts) {
		t.Fatal("RunOnce 应返回 true")
	}
	p := e.Progress()
	if p.Total != 1 || p.Done != 0 {
		t.Errorf("进度=%+v，期望 total=1 done=0（无动作被跳过）", p)
	}
	if p.Running {
		t.Error("结束后 running 应为 false")
	}
}

// TestRunOnceConcurrentGuard 并发二次触发被拒绝。
func TestRunOnceConcurrentGuard(t *testing.T) {
	m := &growthMock{listData: `{"tasks":[]}`, claimData: `{}`}
	e := newGrowthEngine(t, m)
	e.mu.Lock()
	e.running = true
	e.mu.Unlock()
	if e.RunOnce(context.Background(), nil) {
		t.Error("运行中再次触发应返回 false")
	}
	e.mu.Lock()
	e.running = false
	e.mu.Unlock()
}

// TestClaimIfReady 达标任务自动领奖并返回奖励文本。
func TestClaimIfReady(t *testing.T) {
	m := &growthMock{claimData: `{"already_claimed":false,"credit":100,"energy":5}`}
	e := newGrowthEngine(t, m)
	acct := &types.Account{ID: "1", UID: "u1", Region: "cn", AccessToken: "tok"}
	after := &upstream.Task{TaskCode: "chat_5", Claimable: true}
	msg := e.claimIfReady(acct, "chat_5", after)
	if !strings.Contains(msg, "+100") || !strings.Contains(msg, "+5") {
		t.Errorf("领奖文本=%q", msg)
	}
	// 未达标 → 空串且无请求
	n0 := m.requests.Load()
	if got := e.claimIfReady(acct, "chat_5", &upstream.Task{TaskCode: "x"}); got != "" {
		t.Errorf("未达标应返回空串，got=%q", got)
	}
	if m.requests.Load() != n0 {
		t.Error("未达标不应发领奖请求")
	}
}

// TestEngineScanSort 扫描结果按动作表顺序排序。
func TestEngineScanSort(t *testing.T) {
	m := &growthMock{listData: `{"tasks":[
		{"task_code":"create_canvas","target":1,"current":0,"accept_status":"accepted"},
		{"task_code":"chat_5","target":5,"current":3,"accept_status":"accepted"},
		{"task_code":"mystery","target":1,"current":0,"accept_status":"accepted"}
	]}`}
	e := newGrowthEngine(t, m)
	acct := &types.Account{ID: "1", UID: "u1", Region: "cn", AccessToken: "tok"}
	pending, err := e.Scan(acct)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(pending) != 3 {
		t.Fatalf("待办数=%d", len(pending))
	}
	// chat_5(index 0) < create_canvas(index 10) < mystery(无动作, 最后)
	if pending[0].TaskCode != "chat_5" || pending[1].TaskCode != "create_canvas" || pending[2].TaskCode != "mystery" {
		t.Errorf("排序错误: %s %s %s", pending[0].TaskCode, pending[1].TaskCode, pending[2].TaskCode)
	}
}

// TestGrowthProgressFields 进度快照字段完整。
func TestGrowthProgressFields(t *testing.T) {
	e := &GrowthEngine{logf: t.Logf}
	e.mu.Lock()
	e.running, e.total, e.done = true, 10, 3
	e.current, e.currentUID = "chat_5", "u1"
	e.startedAt = time.Now()
	e.mu.Unlock()
	p := e.Progress()
	if !p.Running || p.Total != 10 || p.Done != 3 || p.Current != "chat_5" || p.CurrentUID != "u1" || p.StartedAt == "" {
		t.Errorf("快照字段缺失: %+v", p)
	}
	var js map[string]any
	raw, _ := json.Marshal(p)
	if err := json.Unmarshal(raw, &js); err != nil {
		t.Fatalf("JSON 序列化: %v", err)
	}
	for _, k := range []string{"running", "total", "done", "current", "started_at"} {
		if _, ok := js[k]; !ok {
			t.Errorf("JSON 缺字段 %s", k)
		}
	}
}
