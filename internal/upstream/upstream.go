// Package upstream 负责把内部统一请求发往腾讯上游并做协议转换。
//
// 上游端点与协议细节全部以本地克隆仓库的实际代码为准：
//   - 端点：hub wb_accounts.REALM_CONFIGS chat_upstream（cn=https://copilot.tencent.com，
//     intl=https://www.workbuddy.ai）+ CHAT_PATH="/v2/chat/completions"；
//     Go panel internal/upstream/client.go New() ChatBaseCN="https://copilot.tencent.com"。
//   - 会话头族：hub wb_identity.build_identity_headers —— 每请求新生成
//     X-Conversation-ID（UUID）、X-Conversation-Request-ID/X-Root-Request-ID（同值）、
//     X-Conversation-Message-ID/X-Request-ID（同值）；缺 X-Conversation-ID 上游
//     渠道校验回 400 code 11-128。
//   - 上游恒以 stream=true 请求、网关本地聚合（hub build_upstream_body 固定
//     body["stream"]=True + aggregate_stream；Go panel ChatStreamContext 走 SSE）。
//   - 错误分类：移植 Go panel Classify()，见 classify.go。
package upstream

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api-prime/internal/auth"
	"workbuddy2api-prime/internal/pool"
	"workbuddy2api-prime/internal/protocol"
	"workbuddy2api-prime/internal/types"
)

// 上游端点（依据见包注释）。
const (
	chatBaseCN   = "https://copilot.tencent.com"
	chatBaseIntl = "https://www.workbuddy.ai"
	chatPath     = "/v2/chat/completions"
)

// chatBase 按账号区域返回上游 base URL。
func chatBase(acct *types.Account) string {
	if acct.Region == "cn" {
		return chatBaseCN
	}
	return chatBaseIntl
}

// newUUID 生成随机 UUID（v4 形态）。
// hub 用 UUIDv7（时间有序），此处用 crypto/rand 的 v4 实现；
// 上游只要求"有且形态贴近"，不校验版本（hub wb_identity.uuid7 注释）。
func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极小概率回退：时间戳 hex（保证不空）
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexStr := hex.EncodeToString(b[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hexStr[0:8], hexStr[8:12], hexStr[12:16], hexStr[16:20], hexStr[20:32])
}

// sessionHeaders 构造每请求新生成的会话头族（依据 hub build_identity_headers，
// 2026-10-16 上游收紧渠道校验后按 linguo commit 062c7b5 对齐：
//   - X-Conversation-ID / X-Conversation-Request-ID / X-Conversation-Message-ID
//     三头必发且值须为标准 UUID v4（缺失或格式不对回 11128 "conversation id missing"）；
//   - X-Request-ID 保持 32 位 hex（B3 链路兼容，不与 Message-ID 同值）；
//   - 新增 X-Trace-ID（= Conversation-Request-ID）。
func sessionHeaders() http.Header {
	h := make(http.Header)
	convID := newUUID()
	reqID := newUUID() // X-Conversation-Request-ID 与 X-Root-Request-ID 同值
	msgUUID := newUUID()
	msgID := strings.ReplaceAll(msgUUID, "-", "") // X-Request-ID：32 hex
	h.Set("X-Conversation-ID", convID)
	h.Set("X-Conversation-Request-ID", reqID)
	h.Set("X-Root-Request-ID", reqID)
	h.Set("X-Conversation-Message-ID", msgUUID)
	h.Set("X-Request-ID", msgID)
	h.Set("X-Trace-ID", reqID)
	return h
}

// buildUpstreamBody 把内部 ChatRequest 翻译成上游请求体。
// 上游恒以 SSE 请求（stream=true），网关本地聚合后再返回给非流式客户端
// （hub build_upstream_body / Go panel ChatStreamContext 的做法）。
func buildUpstreamBody(req *protocol.ChatRequest) ([]byte, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	m["stream"] = true
	m["stream_options"] = map[string]any{"include_usage": true}
	// TODO: prompt 模式（hub apply_prompt_mode / panel ensureConsoleSystem）：
	// 首条非 system 时的前置兜底 system、防 11-128，目前由调用方保证，暂未实现。
	// TODO: thinking/effort 注入（hub 针对 deepseek 模型），暂未实现。
	// TODO: tool-call 配对修复（hub merge_adjacent_tool_calls 等），暂未实现。
	return json.Marshal(m)
}

// Backend 是 protocol.ChatBackend 的实现：选号 → 出站头注入 → 上游调用 → 上报。
type Backend struct {
	pool *pool.Pool
	auth *auth.Store
	http *http.Client
}

// NewBackend 创建 Backend。保留原签名。
func NewBackend(p *pool.Pool, a *auth.Store) *Backend {
	return &Backend{
		pool: p,
		auth: a,
		http: &http.Client{
			// 聚合模式一次读完：总超时兜底。
			// TODO: 流式透传实现后改用 ResponseHeaderTimeout + 空闲超时
			// （hub upstream_timeouts / Go panel HeaderTimeout+IdleTimeout）。
			Timeout: 300 * time.Second,
		},
	}
}

// Chat 实现 protocol.ChatBackend。
func (b *Backend) Chat(ctx context.Context, req *protocol.ChatRequest) (*protocol.ChatResponse, error) {
	// 流式客户端暂不支持：上游侧已按 SSE 实现，客户端透传是下一步。
	// TODO: 实现客户端 SSE 透传（上游流 → 客户端流直转）。
	if req.Stream {
		return nil, &protocol.BackendError{
			StatusCode: http.StatusNotImplemented,
			Message:    "stream=true 暂不支持流式透传（TODO），请用非流式请求",
		}
	}

	// 1. 选号（三层选号 + 熔断过滤）。
	pick, err := b.pool.Pick(types.PickRequest{Model: req.Model})
	if err != nil {
		return nil, &protocol.BackendError{
			StatusCode: http.StatusServiceUnavailable,
			Message:    "无可用账号: " + err.Error(),
		}
	}
	acct := pick.Account

	// 2. 构造上游请求体。
	body, err := buildUpstreamBody(req)
	if err != nil {
		return nil, &protocol.BackendError{StatusCode: http.StatusInternalServerError, Message: "构造上游请求失败: " + err.Error()}
	}

	// 3. 出站头：鉴权/设备指纹/用量归属（auth 已实现）+ 每请求会话头族。
	url := chatBase(acct) + chatPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, &protocol.BackendError{StatusCode: http.StatusInternalServerError, Message: err.Error()}
	}
	for k, vs := range auth.BuildOutboundHeaders(acct, "") {
		for _, v := range vs {
			httpReq.Header[k] = append(httpReq.Header[k], v)
		}
	}
	for k, vs := range sessionHeaders() {
		for _, v := range vs {
			httpReq.Header[k] = append(httpReq.Header[k], v)
		}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")

	// 4. 发起上游调用。
	start := time.Now()
	resp, err := b.http.Do(httpReq)
	latencyMs := time.Since(start).Milliseconds()
	if err != nil {
		// 传输层失败：按上游故障计一次（不断网误伤由调用方重试语义处理）。
		// TODO: 区分本机断网（hub looks_like_local_network：不罚账号）。
		b.pool.Report(types.CallReport{
			AccountID: acct.ID, Model: req.Model,
			Status: types.ReportUpstreamError, LatencyMs: latencyMs, ErrCode: "transport",
		})
		return nil, &protocol.BackendError{StatusCode: http.StatusBadGateway, Message: "上游连接失败: " + err.Error()}
	}
	defer resp.Body.Close()

	// 5. 错误路径：限量读正文 → 分类 → 上报 → 透传原文。
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		bodyStr := string(raw)
		status := classifyUpstreamError(resp.StatusCode, bodyStr)
		errCode := strings.TrimSpace(bodyStr)
		if len(errCode) > 120 {
			errCode = errCode[:120]
		}
		b.pool.Report(types.CallReport{
			AccountID: acct.ID, Model: req.Model,
			Status: status, LatencyMs: latencyMs, ErrCode: errCode,
		})
		// 错误透传：message 保留上游原文（hub/panel 一致做法）。
		msg := strings.TrimSpace(bodyStr)
		if msg == "" {
			msg = fmt.Sprintf("上游返回 HTTP %d", resp.StatusCode)
		}
		return nil, &protocol.BackendError{StatusCode: resp.StatusCode, Message: msg}
	}

	// 6. 成功路径：聚合 SSE → 上报 → 返回。
	chatResp, err := aggregateSSE(resp.Body, req.Model)
	if err != nil {
		b.pool.Report(types.CallReport{
			AccountID: acct.ID, Model: req.Model,
			Status: types.ReportUpstreamError, LatencyMs: latencyMs, ErrCode: "sse_parse",
		})
		return nil, &protocol.BackendError{StatusCode: http.StatusBadGateway, Message: err.Error()}
	}
	var credit float64
	if chatResp.Usage != nil {
		// TODO: 上游 usage 信封如带真实积分字段（如 credit），在此提取填入 CreditCost；
		// 目前按 token 计，EMA 账本仍可工作。
		credit = float64(chatResp.Usage.TotalTokens)
	}
	b.pool.Report(types.CallReport{
		AccountID: acct.ID, Model: req.Model,
		Status: types.ReportOK, LatencyMs: latencyMs, CreditCost: credit,
	})
	return chatResp, nil
}
