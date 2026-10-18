// Package upstream 的成长任务（growth）域客户端：任务拉取/接受/领奖、
// 对话活跃上报、桌面事件上报、Buddy 领养、夜猫子、专家市场等。
//
// 移植自 linguo workbuddy2api-panel（internal/upstream/tasks.go、report.go、
// desktop.go、school.go、blackcat.go、travel.go 的 growth 相关部分），
// auth.Auth 替换为本项目的 types.Account，请求头复用本包 billingHeaders。
//
// 成长任务体系仅 cn 域有（global/企业号跳过，见 scheduler 层门控）。
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"workbuddy2api-prime/internal/types"
)

// growth 域基址与路径（照抄 linguo；测试可通过 httptest 覆盖 growthChatBase/growthWebBase）。
var (
	growthChatBase = "https://copilot.tencent.com" // /v2/activity/growth/* 走 CLI 域
	growthWebBase  = "https://www.workbuddy.cn"    // 领奖走 Web 域
)

const (
	growthTasksListPath   = "/v2/activity/growth/tasks"
	growthTasksAcceptPath = "/v2/activity/growth/tasks/accept"
	growthReportPath      = "/v2/report"
	growthBuddyFirstPath  = "/activity/growth/buddy/first"
	growthBuddyAgreePath  = "/activity/growth/buddy/agreement"

	mpPlatformHeader = "miniprogram" // 小程序口径任务头值
	growthChatPath   = "/v2/chat/completions"
)

// SetGrowthTestBases 测试钩子：把上游基址指向桩服务器。
func SetGrowthTestBases(chatBase, webBase string) {
	growthChatBase, growthWebBase = chatBase, webBase
}

// Growth 是成长任务域的上游客户端（无状态，可复用）。
type Growth struct {
	http *http.Client
}

// NewGrowth 创建成长任务客户端。
func NewGrowth() *Growth {
	return &Growth{http: &http.Client{Timeout: 30 * time.Second}}
}

// growthJSON 发 growth 域请求并解信封（{code,msg,data}），extraHeaders 叠加。
func (g *Growth) growthJSON(acct *types.Account, method, path string, body any, extraHeaders map[string]string) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, growthChatBase+path, rdr)
	if err != nil {
		return nil, err
	}
	req.Header = billingHeaders(acct)
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return parseBillingResp(resp)
}

// growthJSONMP 小程序口径（叠加 X-Client-Platform: miniprogram）。
func (g *Growth) growthJSONMP(acct *types.Account, method, path string, body any) (json.RawMessage, error) {
	return g.growthJSON(acct, method, path, body, map[string]string{"X-Client-Platform": mpPlatformHeader})
}

// ---------------------------------------------------------------------------
// 任务列表 / 接受 / 领奖
// ---------------------------------------------------------------------------

// Task 是单个成长任务的对外视图（字段名与上游 JSON 对齐）。
type Task struct {
	TaskCode     string `json:"task_code"`
	Title        string `json:"title,omitempty"`
	Description  string `json:"description,omitempty"`
	TaskDesc     string `json:"task_desc,omitempty"`
	Credit       int64  `json:"credit,omitempty"`
	Energy       int64  `json:"energy,omitempty"`
	HasReward    bool   `json:"has_reward,omitempty"`
	RewardBuddy  bool   `json:"reward_buddy,omitempty"`
	TaskType     string `json:"task_type,omitempty"`
	Tag          string `json:"tag,omitempty"`
	JumpURL      string `json:"jump_url,omitempty"`
	Locked       bool   `json:"locked,omitempty"`
	Target       int64  `json:"target"`
	Current      int64  `json:"current"`
	AcceptStatus string `json:"accept_status,omitempty"`
	Status       string `json:"status,omitempty"`
	Claimable    bool   `json:"claimable,omitempty"` // 进度达标且未领取（本地推算）
	Claimed      bool   `json:"claimed,omitempty"`   // 已领取
}

// ListTasks 拉取全量任务列表（默认口径）。
func (g *Growth) ListTasks(acct *types.Account) ([]Task, error) {
	data, err := g.growthJSON(acct, http.MethodGet, growthTasksListPath, nil, nil)
	if err != nil {
		return nil, err
	}
	return parseGrowthTasks(data)
}

// ListTasksMP 拉取小程序口径任务列表（含 mp 专属任务，是默认口径的超集）。
func (g *Growth) ListTasksMP(acct *types.Account) ([]Task, error) {
	data, err := g.growthJSONMP(acct, http.MethodGet, growthTasksListPath, nil)
	if err != nil {
		return nil, err
	}
	return parseGrowthTasks(data)
}

// parseGrowthTasks 解析 data.tasks[]（progress 可能是 {current,target} 对象或平铺字段）。
func parseGrowthTasks(data json.RawMessage) ([]Task, error) {
	var resp struct {
		Tasks []struct {
			TaskCode     string          `json:"task_code"`
			Title        string          `json:"title"`
			Description  string          `json:"description"`
			TaskDesc     string          `json:"task_desc"`
			RewardCredit int64           `json:"reward_credit"`
			RewardEnergy int64           `json:"reward_energy"`
			HasReward    bool            `json:"has_reward"`
			RewardBuddy  bool            `json:"reward_buddy"`
			TaskType     string          `json:"task_type"`
			Tag          string          `json:"tag"`
			JumpURL      string          `json:"jump_url"`
			Locked       bool            `json:"locked"`
			AcceptStatus string          `json:"accept_status"`
			Status       string          `json:"status"`
			Target       int64           `json:"target"`
			Current      int64           `json:"current"`
			Progress     json.RawMessage `json:"progress"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	out := make([]Task, 0, len(resp.Tasks))
	for _, t := range resp.Tasks {
		cur, tgt := t.Current, t.Target
		if len(t.Progress) > 0 && string(t.Progress) != "null" {
			var pr struct {
				Current int64 `json:"current"`
				Target  int64 `json:"target"`
			}
			if json.Unmarshal(t.Progress, &pr) == nil && (pr.Target > 0 || pr.Current > 0) {
				cur, tgt = pr.Current, pr.Target
			}
		}
		claimed := t.AcceptStatus == "claimed"
		out = append(out, Task{
			TaskCode: t.TaskCode, Title: t.Title, Description: t.Description,
			TaskDesc: t.TaskDesc, Credit: t.RewardCredit, Energy: t.RewardEnergy,
			HasReward: t.HasReward, RewardBuddy: t.RewardBuddy, TaskType: t.TaskType,
			Tag: t.Tag, JumpURL: t.JumpURL, Locked: t.Locked,
			Target: tgt, Current: cur, AcceptStatus: t.AcceptStatus, Status: t.Status,
			Claimable: !claimed && tgt > 0 && cur >= tgt, Claimed: claimed,
		})
	}
	return out, nil
}

// AcceptTasks 批量接受任务（幂等）。
func (g *Growth) AcceptTasks(acct *types.Account, taskCodes []string) error {
	_, err := g.growthJSON(acct, http.MethodPost, growthTasksAcceptPath, map[string]any{"task_codes": taskCodes}, nil)
	return err
}

// AcceptTasksMP 接受小程序限定任务（mp 头）。
func (g *Growth) AcceptTasksMP(acct *types.Account, taskCodes []string) error {
	_, err := g.growthJSONMP(acct, http.MethodPost, growthTasksAcceptPath, map[string]any{"task_codes": taskCodes})
	return err
}

// parseClaimReward 解析领奖响应 data。
func parseClaimReward(data json.RawMessage) (credit, energy int64, err error) {
	var resp struct {
		AlreadyClaimed bool  `json:"already_claimed"`
		Credit         int64 `json:"credit"`
		Energy         int64 `json:"energy"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, err
	}
	return resp.Credit, resp.Energy, nil
}

// ClaimReward 领取单个任务奖励（Web 域，task_code 在路径里）。
func (g *Growth) ClaimReward(acct *types.Account, taskCode string) (int64, int64, error) {
	req, err := http.NewRequest(http.MethodPost,
		growthWebBase+"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+acct.AccessToken)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://www.workbuddy.cn")
	req.Header.Set("Referer", "https://www.workbuddy.cn/profile/growth-center")
	req.Header.Set("x-client-platform", "web")
	if acct.UID != "" {
		req.Header.Set("X-User-Id", acct.UID)
	}
	if acct.EnterpriseID != "" {
		req.Header.Set("X-Enterprise-Id", acct.EnterpriseID)
		req.Header.Set("X-Tenant-Id", acct.EnterpriseID)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	data, err := parseBillingResp(resp)
	if err != nil {
		return 0, 0, err
	}
	return parseClaimReward(data)
}

// ClaimRewardMP 领取小程序任务奖励（chat 域 mp 口径；400 时回落 Web 域）。
func (g *Growth) ClaimRewardMP(acct *types.Account, taskCode string) (int64, int64, error) {
	req, err := http.NewRequest(http.MethodPost,
		growthChatBase+"/activity/growth/tasks/"+url.PathEscape(taskCode)+"/claim", nil)
	if err != nil {
		return 0, 0, err
	}
	req.Header = billingHeaders(acct)
	req.Header.Set("X-Client-Platform", mpPlatformHeader)
	resp, err := g.http.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	data, err := parseBillingResp(resp)
	if err != nil {
		var be *BillingError
		if errors.As(err, &be) && be.HTTPStatus == http.StatusBadRequest {
			return g.ClaimReward(acct, taskCode) // chat 域 400 → Web 域降级
		}
		return 0, 0, err
	}
	return parseClaimReward(data)
}

// ---------------------------------------------------------------------------
// 对话活跃上报
// ---------------------------------------------------------------------------

// chatRequestEvent 是 chat_request_send 事件完整形状（照抄 linguo，防上游加严）。
// userId 必填（= 账号 UID），缺失则服务端 200 但静默丢弃。
type chatRequestEvent struct {
	EventCode            string `json:"eventCode"`
	Timestamp            int64  `json:"timestamp"`
	ReportDelay          int    `json:"reportDelay"`
	Mode                 string `json:"mode"`
	ConversationID       string `json:"conversationId"`
	RequestID            string `json:"requestId"`
	InputLength          int    `json:"inputLength"`
	RequestModelID       string `json:"requestModelId"`
	RequestModelName     string `json:"requestModelName"`
	IsPlan               bool   `json:"isPlan"`
	AgentName            string `json:"agentName"`
	AgentType            string `json:"agentType"`
	UserID               string `json:"userId"`
	RootRequestID        string `json:"rootRequestId"`
	ParentConversationID string `json:"parentConversationId"`
	PresentAt            int64  `json:"presentAt"`
}

// ReportChatActivity 上报一条对话活跃事件（chat_request_send）。
func (g *Growth) ReportChatActivity(acct *types.Account, conversationID, requestID string) error {
	return g.ReportChatActivityModel(acct, conversationID, requestID, "deepseek-v4-flash", "DeepSeek V4 Flash")
}

// ReportChatActivityModel 同上，可指定上报携带的模型。
func (g *Growth) ReportChatActivityModel(acct *types.Account, conversationID, requestID, modelID, modelName string) error {
	if requestID == "" {
		requestID = conversationID
	}
	if modelID == "" {
		modelID = "deepseek-v4-flash"
	}
	if modelName == "" {
		modelName = modelID
	}
	now := time.Now().UnixMilli()
	uid := acct.UID
	if uid == "" {
		uid = acct.ID
	}
	ev := chatRequestEvent{
		EventCode: "chat_request_send", Timestamp: now, Mode: "craft",
		ConversationID: conversationID, RequestID: requestID, InputLength: 12,
		RequestModelID: modelID, RequestModelName: modelName,
		AgentName: "default", AgentType: "conversation", UserID: uid,
		RootRequestID: requestID, ParentConversationID: conversationID, PresentAt: now,
	}
	_, err := g.growthJSON(acct, http.MethodPost, growthReportPath, []chatRequestEvent{ev}, nil)
	return err
}

// ---------------------------------------------------------------------------
// Buddy 领养
// ---------------------------------------------------------------------------

// buddyTaskIncompleteMarker 领养门槛未达标的错误关键词（当日不应重试）。
const buddyTaskIncompleteMarker = "first_buddy task not completed yet"

// BuddyFirst 领取第一只 Buddy（+300 分）。
func (g *Growth) BuddyFirst(acct *types.Account) error {
	_, err := g.growthJSON(acct, http.MethodPost, growthBuddyFirstPath, map[string]any{}, nil)
	return err
}

// BuddyAgreement 同意 Buddy 协议（幂等）。
func (g *Growth) BuddyAgreement(acct *types.Account) error {
	_, err := g.growthJSON(acct, http.MethodPost, growthBuddyAgreePath, map[string]any{"agree": true}, nil)
	return err
}

// IsBuddyTaskIncomplete 判定"领养门槛未达标"（HTTP 400 + 关键词），当日不应重试。
func IsBuddyTaskIncomplete(err error) bool {
	if err == nil {
		return false
	}
	var be *BillingError
	if !errors.As(err, &be) || be.HTTPStatus != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(be.Msg), buddyTaskIncompleteMarker)
}

// ---------------------------------------------------------------------------
// 夜猫子
// ---------------------------------------------------------------------------

// InNightWindow 是否处于夜猫子计数窗口（23:00–08:00 本地时区）。
func InNightWindow(now time.Time) bool {
	h := now.Hour()
	return h >= 23 || h < 8
}

// BlackcatNeed 查 black_cat 任务剩余差额（0 = 无可做）。
func (g *Growth) BlackcatNeed(acct *types.Account) (int64, error) {
	tasks, err := g.ListTasks(acct)
	if err != nil {
		return 0, err
	}
	for _, t := range tasks {
		if t.TaskCode == "black_cat" {
			if t.Claimed || t.Current >= t.Target {
				return 0, nil
			}
			return t.Target - t.Current, nil
		}
	}
	return 0, nil
}

// RunNightChats 发 need 次 glm-5.2 真实对话（读干流）并上报，返回成功次数。
func (g *Growth) RunNightChats(acct *types.Account, need int) (int64, error) {
	var ok int64
	for i := 0; i < need; i++ {
		body, _ := json.Marshal(map[string]any{
			"model":    "glm-5.2",
			"messages": []map[string]any{{"role": "user", "content": "1+1等于几？直接回答。"}},
			"stream":   true,
		})
		rc, status, respBody, err := g.GrowthChatStream(acct, body)
		if err != nil || status >= 400 {
			if rc != nil {
				rc.Close()
			}
			return ok, fmt.Errorf("第 %d 次对话失败: http=%d err=%v body=%.120s", i+1, status, err, respBody)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20))
		rc.Close()
		if err := g.ReportChatActivityModel(acct,
			fmt.Sprintf("wb2a-night-%d-%d", time.Now().UnixMilli(), i), "", "glm-5.2", "GLM-5.2"); err != nil {
			return ok, fmt.Errorf("第 %d 次上报失败: %w", i+1, err)
		}
		ok++
		time.Sleep(4 * time.Second)
	}
	return ok, nil
}

// growthChatStream 以指定账号直发一次 chat completions（stream），返回流/状态/错误体。
// 成长任务专用：不走 pool 选号，直接用账号 token。
func (g *Growth) GrowthChatStream(acct *types.Account, body []byte) (io.ReadCloser, int, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, growthChatBase+growthChatPath, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header = billingHeaders(acct)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, raw, &BillingError{
			Code: -1, HTTPStatus: resp.StatusCode, Msg: truncateStr(string(raw), 200),
		}
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// growthChatStreamCtx 带 ctx 的版本（DesktopChatWithExpert 用）。
func (g *Growth) growthChatStreamCtx(ctx context.Context, acct *types.Account, body []byte, extraHeaders map[string]string) (io.ReadCloser, int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, growthChatBase+growthChatPath, bytes.NewReader(body))
	if err != nil {
		return nil, 0, nil, err
	}
	req.Header = billingHeaders(acct)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, 0, nil, err
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		return nil, resp.StatusCode, raw, &BillingError{
			Code: -1, HTTPStatus: resp.StatusCode, Msg: truncateStr(string(raw), 200),
		}
	}
	return resp.Body, resp.StatusCode, nil, nil
}

// ---------------------------------------------------------------------------
// 专家市场
// ---------------------------------------------------------------------------

// MarketExpert 是专家市场条目。
type MarketExpert struct {
	ExpertID      string `json:"expert_id"`
	ExpertType    string `json:"expert_type"`
	DisplayNameZH string `json:"display_name_zh"`
	ProfessionZH  string `json:"profession_zh"`
	Version       string `json:"version"`
}

// MarketExpertList 拉取专家市场真实专家列表（expert_actual_use 要求 id 真实存在）。
func (g *Growth) MarketExpertList(acct *types.Account, expertType string) ([]MarketExpert, error) {
	body := map[string]any{"page": 1, "page_size": 20, "sort_by": "reco_rank", "sort_order": "desc"}
	if expertType != "" {
		body["expert_type"] = expertType
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, growthChatBase+"/portal/operation-platform/market/expert/list", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+acct.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", desktopUA)
	req.Header.Set("X-Domain", growthChatBase)
	req.Header.Set("X-Product", "SaaS")
	if acct.UID != "" {
		req.Header.Set("X-User-Id", acct.UID)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := parseBillingResp(resp)
	if err != nil {
		return nil, err
	}
	var out struct {
		Experts []MarketExpert `json:"experts"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("expert list parse: %w", err)
	}
	return out.Experts, nil
}

// DesktopChatWithExpert 发一条真实桌面指纹 chat（可带 X-Expert-Id），
// 从 SSE 流解析服务端返回的 requestId（expert_actual_use 要求真实 id）。
func (g *Growth) DesktopChatWithExpert(acct *types.Account, expertID string) (conversationID, requestID string, err error) {
	conversationID = fmt.Sprintf("wb2a-conv-%d", time.Now().UnixNano())
	body, _ := json.Marshal(map[string]any{
		"model": "fast-model",
		"messages": []map[string]any{
			{"role": "user", "content": "你好，请简单介绍一下你自己。"},
		},
		"stream": true,
	})
	extra := map[string]string{"User-Agent": desktopUA}
	if expertID != "" {
		extra["X-Expert-Id"] = expertID
	}
	rc, status, respBody, err := g.growthChatStreamCtx(context.Background(), acct, body, extra)
	if err != nil {
		return "", "", err
	}
	if status >= 400 {
		rc.Close()
		return "", "", fmt.Errorf("chat http=%d: %.160s", status, respBody)
	}
	// 从 SSE 的 data.id 提取服务端 requestId。
	requestID, perr := parseStreamRequestID(rc)
	rc.Close()
	if perr != nil {
		return "", "", fmt.Errorf("解析服务端 requestId: %w", perr)
	}
	return conversationID, requestID, nil
}

// parseStreamRequestID 从 SSE 流首个 data: 行提取 id（cmb-xxx 或 32hex）。
func parseStreamRequestID(rc io.Reader) (string, error) {
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 1024)
	for len(buf) < 65536 {
		n, err := rc.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			if idx := bytes.Index(buf, []byte("\n\n")); idx >= 0 {
				return extractDataID(buf[:idx])
			}
		}
		if err != nil {
			break
		}
	}
	return extractDataID(buf)
}

// extractDataID 从 SSE chunk 提取 data.id。
func extractDataID(chunk []byte) (string, error) {
	for _, line := range bytes.Split(chunk, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if string(payload) == "[DONE]" {
			continue
		}
		var m struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(payload, &m) == nil && m.ID != "" {
			return m.ID, nil
		}
	}
	return "", fmt.Errorf("SSE 流中未找到 data.id")
}

// ---------------------------------------------------------------------------
// 小程序（mp）事件上报
// ---------------------------------------------------------------------------

// ReportMPEvent 以上报小程序指纹批量上报事件（X-Client-Product: workbuddy-mp）。
func (g *Growth) ReportMPEvent(acct *types.Account, events ...map[string]any) error {
	if len(events) == 0 {
		return fmt.Errorf("mp report: no events")
	}
	base := mpEventBase(acct)
	arr := make([]map[string]any, 0, len(events))
	for _, ev := range events {
		m := map[string]any{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range ev {
			m[k] = v
		}
		arr = append(arr, m)
	}
	raw, err := json.Marshal(arr)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, mpWebBase+mpReportPath, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+acct.AccessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if acct.UID != "" {
		req.Header.Set("X-User-Id", acct.UID)
	}
	req.Header.Set("X-Client-Product", "workbuddy-mp")
	req.Header.Set("X-Client-Version", "2.4.0")
	req.Header.Set("X-Client-Platform", "mp-weixin")
	req.Header.Set("X-Platform", "wechatmp")
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = parseBillingResp(resp)
	return err
}

// mpReportPath 小程序事件上报路径（基址 mpWebBase，测试可覆盖）。
const mpReportPath = "/v2/report"

// mpWebBase 小程序事件上报基址。
var mpWebBase = "https://www.codebuddy.cn"

// mpEventBase 小程序公共指纹。
func mpEventBase(acct *types.Account) map[string]any {
	now := time.Now().UnixMilli()
	uid := acct.UID
	if uid == "" {
		uid = acct.ID
	}
	return map[string]any{
		"timezone": "Asia/Shanghai", "userId": uid,
		"timestamp": now, "presentAt": now,
		"product": "workbuddy-mp", "platform": "wechatmp",
	}
}

// ---------------------------------------------------------------------------
// 小程序/校园事件构造器（纯函数，照抄 linguo school.go）
// ---------------------------------------------------------------------------

// SchoolChatTimesEvents 构造一条 chat_request_send 事件（小程序对话计数）。
func SchoolChatTimesEvents(conversationID string) map[string]any {
	return map[string]any{
		"eventCode": "chat_request_send", "conversationId": conversationID,
		"requestId": fmt.Sprintf("wb2a-mp-%d", time.Now().UnixNano()),
		"timestamp": time.Now().UnixMilli(), "source": "miniprogram",
	}
}

// SchoolSeasonChatEvent 校园日 mini 对话事件（含 activityId）。
func SchoolSeasonChatEvent(conversationID string) map[string]any {
	return map[string]any{
		"eventCode": "mini_chat_send", "conversationId": conversationID,
		"requestId":  fmt.Sprintf("wb2a-sch-%d", time.Now().UnixNano()),
		"activityId": "school_season_2026", "timestamp": time.Now().UnixMilli(),
	}
}

// MiniExpertUseEvent 小程序专家使用事件（mp 指纹 expert_actual_use）。
func MiniExpertUseEvent(expertID, expertName, expertType string) map[string]any {
	return map[string]any{
		"eventCode": "expert_actual_use", "expertId": expertID,
		"expertName": expertName, "expertType": expertType,
		"requestId": fmt.Sprintf("wb2a-mpex-%d", time.Now().UnixNano()),
		"timestamp": time.Now().UnixMilli(), "source": "miniprogram",
	}
}

// MiniChatModelEvent 小程序带模型对话事件。
func MiniChatModelEvent(conversationID, modelID, modelName string) map[string]any {
	return map[string]any{
		"eventCode": "mini_chat_send", "conversationId": conversationID,
		"requestId":      fmt.Sprintf("wb2a-mpcm-%d", time.Now().UnixNano()),
		"requestModelId": modelID, "requestModelName": modelName,
		"timestamp": time.Now().UnixMilli(),
	}
}

// MiniPlaybookEvents 小程序灵感事件组。
func MiniPlaybookEvents(caseID, caseName string) []map[string]any {
	now := time.Now().UnixMilli()
	mk := func(code string) map[string]any {
		return map[string]any{
			"eventCode": code, "caseId": caseID, "caseName": caseName,
			"requestId": fmt.Sprintf("wb2a-mppb-%d", now), "timestamp": now,
			"source": "miniprogram",
		}
	}
	return []map[string]any{
		mk("playbook_card_view"), mk("playbook_cta_click"), mk("playbook_prompt_send"),
	}
}
