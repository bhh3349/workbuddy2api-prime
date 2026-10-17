// Package protocol 是 workbuddy2api-prime 的入站兼容层。
//
// 设计依据：
//   - DESIGN.md §3：protocol 负责 /v1/chat/completions 主路径，
//     /v1/messages、/v1/responses 经 compatRun 思路复用整条 chat 链路；
//   - 分析文档 04-workbuddy2api-panel-plus-jacky.md §4.3：兼容层不复制调度逻辑，
//     只做"外部协议 → OpenAI chat → 外部协议"的翻译，中间复用同一后端。
//
// 因此三个 handler 都只依赖 ChatBackend 接口：pool 选号、upstream 出站、
// 熔断冷却、记账全部长在后端实现里，protocol 对它们零感知、零复制。
// pool / upstream 模块后续接入时，只需实现 ChatBackend 即可。
package protocol

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
)

// ChatBackend 是主路径 chat 逻辑的抽象。
//
// 实现方（后续的 pool+upstream 组合）负责：选号、鉴权头注入、上游调用、
// 错误分类、记账。protocol 的三个 handler 只调这一个方法，
// 这就是 panel-plus compatRun "mux 内调复用整条链路" 在本项目的落地形态。
type ChatBackend interface {
	Chat(ctx context.Context, req *ChatRequest) (*ChatResponse, error)
}

// BackendError 是 ChatBackend 返回的上游错误，携带 HTTP 状态码。
// protocol 据此做"按协议分发"的错误形态翻译；普通 error 一律按 502 处理。
type BackendError struct {
	StatusCode int    // 上游 HTTP 状态码（429/400/5xx…）
	Message    string // 给客户端看的错误信息
}

func (e *BackendError) Error() string { return e.Message }

// backendStatus 从 error 里提取状态码，提不出就按 502 网关错误处理。
func backendStatus(err error) int {
	var be *BackendError
	if errors.As(err, &be) && be.StatusCode >= 400 && be.StatusCode < 600 {
		return be.StatusCode
	}
	return http.StatusBadGateway
}

// RegisterRoutes 把三个协议入口挂到 mux 上，供 cmd/server/main.go 调用。
func RegisterRoutes(mux *http.ServeMux, backend ChatBackend) {
	mux.HandleFunc("/v1/chat/completions", ChatCompletionsHandler(backend))
	mux.HandleFunc("/v1/messages", MessagesHandler(backend))
	mux.HandleFunc("/v1/responses", ResponsesHandler(backend))
}

// maxBody 限制入站 body 大小，防超大请求打爆内存。
const maxBody = 8 << 20 // 8MB

// decodeBody 读 body 并做 JSON 解码；body 超限或 JSON 非法时返回错误。
func decodeBody(w http.ResponseWriter, r *http.Request, v any) error {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return errors.New("method not allowed")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	dec := json.NewDecoder(r.Body)
	// 不开 DisallowUnknownFields：客户端常带额外字段，宽容处理。
	if err := dec.Decode(v); err != nil {
		http.Error(w, "invalid json body: "+err.Error(), http.StatusBadRequest)
		return err
	}
	return nil
}

// writeJSON 写 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// newID 生成带前缀的随机 ID（如 resp_xxx / msg_xxx）。
func newID(prefix string) string {
	var b [12]byte
	_, _ = rand.Read(b[:]) // crypto/rand 极小概率失败，失败就用零值，不中断流程
	return prefix + hex.EncodeToString(b[:])
}
