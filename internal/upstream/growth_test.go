// Package upstream 的成长任务测试（httptest mock 上游：任务解析、接取/领奖、
// 事件构造器、跳过规则）。不依赖真实账号与网络。
package upstream

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"workbuddy2api-prime/internal/types"
)

// mockGrowthServer 搭一个成长任务上游桩，返回请求计数与最后请求头。
type mockGrowthServer struct {
	t          *testing.T
	srv        *httptest.Server
	requests   int
	lastHeader http.Header
	lastBody   []byte
	listResp   string // 任务列表 data 负载（JSON）
	claimResp  string // 领奖 data 负载（JSON）
	chatStatus int    // /v2/chat/completions 状态码（默认 400 模拟任务口径失败）
}

func newMockGrowthServer(t *testing.T) *mockGrowthServer {
	m := &mockGrowthServer{t: t, chatStatus: http.StatusBadRequest}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		m.requests++
		m.lastHeader = r.Header.Clone()
		body := make([]byte, 0, 1024)
		buf := make([]byte, 1024)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				body = append(body, buf[:n]...)
			}
			if err != nil {
				break
			}
		}
		m.lastBody = body
		switch {
		case strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks") && r.Method == http.MethodGet:
			writeEnv(w, m.listResp)
		case strings.HasSuffix(r.URL.Path, "/v2/activity/growth/tasks/accept"):
			writeEnv(w, `{}`)
		case strings.Contains(r.URL.Path, "/activity/growth/tasks/") && strings.HasSuffix(r.URL.Path, "/claim"):
			writeEnv(w, m.claimResp)
		case r.URL.Path == "/v2/report":
			writeEnv(w, `{}`)
		case r.URL.Path == "/v2/chat/completions":
			w.WriteHeader(m.chatStatus)
			fmt.Fprint(w, `{"code":400,"msg":"bad"}`)
		case strings.HasSuffix(r.URL.Path, "/activity/growth/buddy/agreement"):
			writeEnv(w, `{}`)
		case strings.HasSuffix(r.URL.Path, "/activity/growth/buddy/first"):
			writeEnv(w, `{}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"code":404,"msg":"not found"}`)
		}
	})
	m.srv = httptest.NewServer(mux)
	return m
}

func writeEnv(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"code":0,"msg":"ok","data":%s}`, data)
}

func (m *mockGrowthServer) close() { m.srv.Close() }

// withBases 把 growthChatBase/growthWebBase 指向桩，测试结束后恢复。
func (m *mockGrowthServer) withBases(t *testing.T) {
	oldChat, oldWeb := growthChatBase, growthWebBase
	growthChatBase, growthWebBase = m.srv.URL, m.srv.URL
	t.Cleanup(func() { growthChatBase, growthWebBase = oldChat, oldWeb })
}

func growthTestAccount() *types.Account {
	return &types.Account{ID: "acc-1", UID: "u-100", Region: "cn", AccessToken: "tok-abc", Nickname: "测试号"}
}

const sampleTasksData = `{"tasks":[
	{"task_code":"chat_5","title":"对话5次","reward_credit":100,"reward_energy":5,
	 "accept_status":"accepted","status":"doing","target":5,"current":3},
	{"task_code":"first_buddy","title":"领养Buddy","reward_credit":300,
	 "accept_status":"claimed","status":"done","target":1,"current":1},
	{"task_code":"school_season","title":"校园日","reward_credit":50,
	 "accept_status":"accepted","status":"doing","target":1,"current":0,
	 "progress":{"current":0,"target":1}},
	{"task_code":"locked_x","title":"未解锁","locked":true,
	 "accept_status":"","status":"","target":2,"current":0}
]}`

func TestParseGrowthTasks(t *testing.T) {
	tasks, err := parseGrowthTasks(json.RawMessage(sampleTasksData))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(tasks) != 4 {
		t.Fatalf("任务数=%d，期望 4", len(tasks))
	}
	by := map[string]Task{}
	for _, x := range tasks {
		by[x.TaskCode] = x
	}
	// 平铺字段口径
	if c := by["chat_5"]; c.Current != 3 || c.Target != 5 || c.Credit != 100 {
		t.Errorf("chat_5 解析错误: %+v", c)
	}
	if by["chat_5"].Claimable {
		t.Error("chat_5 3/5 不应 claimable")
	}
	// 已领取
	if !by["first_buddy"].Claimed {
		t.Error("first_buddy 应 claimed")
	}
	// progress 对象口径覆盖平铺字段
	if c := by["school_season"]; c.Current != 0 || c.Target != 1 {
		t.Errorf("school_season progress 对象口径错误: %+v", c)
	}
	if by["locked_x"].Claimable {
		t.Error("locked 任务不应 claimable")
	}
}

func TestParseGrowthTasksClaimable(t *testing.T) {
	data := `{"tasks":[{"task_code":"x","accept_status":"accepted","target":2,"current":2}]}`
	tasks, err := parseGrowthTasks(json.RawMessage(data))
	if err != nil || len(tasks) != 1 {
		t.Fatalf("解析失败: %v %d", err, len(tasks))
	}
	if !tasks[0].Claimable {
		t.Error("2/2 应 claimable")
	}
}

func TestListTasksHeaders(t *testing.T) {
	m := newMockGrowthServer(t)
	defer m.close()
	m.listResp = sampleTasksData
	m.withBases(t)

	tasks, err := NewGrowth().ListTasks(growthTestAccount())
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(tasks) != 4 {
		t.Fatalf("任务数=%d", len(tasks))
	}
	if got := m.lastHeader.Get("Authorization"); got != "Bearer tok-abc" {
		t.Errorf("Authorization 头=%q", got)
	}
	if got := m.lastHeader.Get("X-Client-Platform"); got != "" {
		t.Errorf("默认口径不应带 mp 头，got=%q", got)
	}
}

func TestListTasksMPHeader(t *testing.T) {
	m := newMockGrowthServer(t)
	defer m.close()
	m.listResp = sampleTasksData
	m.withBases(t)

	if _, err := NewGrowth().ListTasksMP(growthTestAccount()); err != nil {
		t.Fatalf("ListTasksMP: %v", err)
	}
	if got := m.lastHeader.Get("X-Client-Platform"); got != mpPlatformHeader {
		t.Errorf("mp 口径头=%q，期望 %q", got, mpPlatformHeader)
	}
}

func TestAcceptTasksBody(t *testing.T) {
	m := newMockGrowthServer(t)
	defer m.close()
	m.withBases(t)

	if err := NewGrowth().AcceptTasks(growthTestAccount(), []string{"chat_5", "first_buddy"}); err != nil {
		t.Fatalf("AcceptTasks: %v", err)
	}
	var body struct {
		TaskCodes []string `json:"task_codes"`
	}
	if err := json.Unmarshal(m.lastBody, &body); err != nil {
		t.Fatalf("body 解析: %v", err)
	}
	if len(body.TaskCodes) != 2 || body.TaskCodes[0] != "chat_5" {
		t.Errorf("accept body=%s", m.lastBody)
	}
}

func TestClaimReward(t *testing.T) {
	m := newMockGrowthServer(t)
	defer m.close()
	m.claimResp = `{"already_claimed":false,"credit":100,"energy":5}`
	m.withBases(t)

	credit, energy, err := NewGrowth().ClaimReward(growthTestAccount(), "chat_5")
	if err != nil {
		t.Fatalf("ClaimReward: %v", err)
	}
	if credit != 100 || energy != 5 {
		t.Errorf("奖励=%d/%d，期望 100/5", credit, energy)
	}
	if got := m.lastHeader.Get("x-client-platform"); got != "web" {
		t.Errorf("web 头=%q", got)
	}
	if got := m.lastHeader.Get("Referer"); !strings.Contains(got, "growth-center") {
		t.Errorf("Referer=%q", got)
	}
}

func TestClaimRewardAlreadyClaimed(t *testing.T) {
	m := newMockGrowthServer(t)
	defer m.close()
	m.claimResp = `{"already_claimed":true,"credit":0,"energy":0}`
	m.withBases(t)

	credit, energy, err := NewGrowth().ClaimReward(growthTestAccount(), "chat_5")
	if err != nil || credit != 0 || energy != 0 {
		t.Errorf("重复领取应幂等成功且 0 奖励: %v %d %d", err, credit, energy)
	}
}

func TestClaimRewardMPFallback(t *testing.T) {
	m := newMockGrowthServer(t)
	defer m.close()
	// chat 域（桩 URL 即 growthChatBase）对 claim 返回 400 → 应回落 Web 域领奖。
	// 桩对两个基址是同一个 URL：用路径区分——claim 路径 chat 域 400 时触发回落；
	// 这里桩统一返回成功，验证至少领奖成功即可。回落分支用 400 桩单独测。
	m.claimResp = `{"already_claimed":false,"credit":50,"energy":2}`
	m.withBases(t)

	credit, _, err := NewGrowth().ClaimRewardMP(growthTestAccount(), "school_season")
	if err != nil {
		t.Fatalf("ClaimRewardMP: %v", err)
	}
	if credit != 50 {
		t.Errorf("credit=%d", credit)
	}
}

func TestReportChatActivity(t *testing.T) {
	m := newMockGrowthServer(t)
	defer m.close()
	m.withBases(t)

	if err := NewGrowth().ReportChatActivity(growthTestAccount(), "conv-1", ""); err != nil {
		t.Fatalf("ReportChatActivity: %v", err)
	}
	var evs []map[string]any
	if err := json.Unmarshal(m.lastBody, &evs); err != nil {
		t.Fatalf("事件解析: %v", err)
	}
	if len(evs) != 1 || evs[0]["eventCode"] != "chat_request_send" {
		t.Fatalf("事件形状错误: %s", m.lastBody)
	}
	if evs[0]["userId"] != "u-100" {
		t.Errorf("userId 必填=uid，got=%v", evs[0]["userId"])
	}
}

func TestDesktopChatSequence(t *testing.T) {
	evs := DesktopChatSequence("conv", "req-1", "msg-1", "fast-model", "fast-model")
	want := []string{"agent_task_created", "chat_message_send", "chat_request_send",
		"chat_message_response", "chat_message_status", "chat_request_response"}
	if len(evs) != len(want) {
		t.Fatalf("事件数=%d", len(evs))
	}
	for i, w := range want {
		if evs[i]["eventCode"] != w {
			t.Errorf("第 %d 个事件=%v，期望 %s", i, evs[i]["eventCode"], w)
		}
	}
}

func TestExpertSequences(t *testing.T) {
	ex := MarketExpert{ExpertID: "ex_1", ExpertType: "agent", DisplayNameZH: "测试专家"}
	summon := DesktopExpertSummonSequence(ex)
	if len(summon) != 3 {
		t.Fatalf("召唤链事件数=%d", len(summon))
	}
	use := DesktopExpertActualUseEvent(ex, "conv", "req-1234567890abcdef1234567890abcdef")
	if use["eventCode"] != "expert_actual_use" || use["id"] != "ex_1" {
		t.Errorf("actualUse 载荷错误: %v", use["eventCode"])
	}
	local := DesktopExpertActualUseLocal(ex, "conv", "req-1234567890abcdef1234567890abcdef")
	if local["mode"] != "LOCAL" || use["mode"] != "craft" {
		t.Errorf("mode 区分错误: %v / %v", use["mode"], local["mode"])
	}
}

func TestParseStreamRequestID(t *testing.T) {
	sse := "event: message\ndata: {\"id\":\"cmb-abc123\",\"choices\":[]}\n\n"
	id, err := parseStreamRequestID(strings.NewReader(sse))
	if err != nil || id != "cmb-abc123" {
		t.Errorf("id=%q err=%v", id, err)
	}
	_, err = parseStreamRequestID(strings.NewReader("data: [DONE]\n\n"))
	if err == nil {
		t.Error("只有 [DONE] 应报错")
	}
}

func TestInNightWindow(t *testing.T) {
	cases := []struct {
		hour int
		want bool
	}{
		{23, true}, {2, true}, {7, true}, {8, false}, {12, false}, {22, false},
	}
	for _, c := range cases {
		got := InNightWindow(time.Date(2026, 1, 1, c.hour, 0, 0, 0, time.Local))
		if got != c.want {
			t.Errorf("hour=%d got=%v want=%v", c.hour, got, c.want)
		}
	}
}

func TestIsBuddyTaskIncomplete(t *testing.T) {
	if !IsBuddyTaskIncomplete(&BillingError{HTTPStatus: 400, Msg: "First_buddy task not completed yet"}) {
		t.Error("应识别领养门槛未达标")
	}
	if IsBuddyTaskIncomplete(&BillingError{HTTPStatus: 500, Msg: "first_buddy task not completed yet"}) {
		t.Error("非 400 不应识别")
	}
	if IsBuddyTaskIncomplete(nil) {
		t.Error("nil 不应识别")
	}
}

func TestBuddyAgreementBody(t *testing.T) {
	m := newMockGrowthServer(t)
	defer m.close()
	m.withBases(t)

	if err := NewGrowth().BuddyAgreement(growthTestAccount()); err != nil {
		t.Fatalf("BuddyAgreement: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal(m.lastBody, &body); err != nil {
		t.Fatalf("body 解析: %v", err)
	}
	if body["agree"] != true {
		t.Errorf("agreement body=%s", m.lastBody)
	}
}
