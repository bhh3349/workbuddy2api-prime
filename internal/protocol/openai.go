package protocol

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// 本文件定义 OpenAI chat 协议的最小可用结构，以及主路径 handler。
// 它是整个兼容层的"内部统一表示"：Anthropic / Responses 入站都要先翻译成
// ChatRequest，再走同一个 ChatBackend；出站再翻译回去。

// ChatContentPart 是 OpenAI 消息内容的多段形态（文本 / 图片）。
type ChatContentPart struct {
	Type     string `json:"type"` // "text" | "image_url"
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
}

// ChatMessageContent 兼容 OpenAI content 的两种形态：纯字符串或多段数组。
type ChatMessageContent struct {
	Raw   string // 纯字符串形态
	Parts []ChatContentPart
}

// MarshalJSON：有多段就发数组，否则发字符串；全空发 null（工具调用场景）。
func (c ChatMessageContent) MarshalJSON() ([]byte, error) {
	if len(c.Parts) > 0 {
		return json.Marshal(c.Parts)
	}
	if c.Raw == "" {
		return []byte("null"), nil
	}
	return json.Marshal(c.Raw)
}

// UnmarshalJSON：先试字符串，再试数组；未知形态按空处理（宽容）。
func (c *ChatMessageContent) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		c.Raw = s
		return nil
	}
	var parts []ChatContentPart
	if err := json.Unmarshal(data, &parts); err == nil {
		c.Parts = parts
		return nil
	}
	return nil
}

// Text 提取消息里的纯文本（多段时拼接 text 段）。
func (c ChatMessageContent) Text() string {
	if c.Raw != "" {
		return c.Raw
	}
	var sb strings.Builder
	for _, p := range c.Parts {
		if p.Type == "text" {
			sb.WriteString(p.Text)
		}
	}
	return sb.String()
}

// NewTextContent 构造纯文本 content。
func NewTextContent(s string) ChatMessageContent { return ChatMessageContent{Raw: s} }

// ChatMessage 是一条 OpenAI chat 消息。
type ChatMessage struct {
	Role       string             `json:"role"` // system|user|assistant|tool
	Content    ChatMessageContent `json:"content"`
	Name       string             `json:"name,omitempty"`
	ToolCalls  []ToolCall         `json:"tool_calls,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"` // role=tool 时必填
}

// ToolCall 是 OpenAI 的工具调用。
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // "function"
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON 字符串
	} `json:"function"`
}

// ChatTool 是 OpenAI 的工具定义。
type ChatTool struct {
	Type     string `json:"type"` // "function"
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
		Parameters  any    `json:"parameters,omitempty"`
	} `json:"function"`
}

// ChatRequest 是 POST /v1/chat/completions 的最小可用请求结构。
type ChatRequest struct {
	Model       string         `json:"model"`
	Messages    []ChatMessage  `json:"messages"`
	Tools       []ChatTool     `json:"tools,omitempty"`
	ToolChoice  any            `json:"tool_choice,omitempty"` // string 或 {type,function:{name}}
	Temperature *float64       `json:"temperature,omitempty"`
	TopP        *float64       `json:"top_p,omitempty"`
	MaxTokens   *int           `json:"max_tokens,omitempty"`
	Stream      bool           `json:"stream,omitempty"`
	Stop        []string       `json:"stop,omitempty"`
}

// ChatChoice 是 chat 响应的一个候选项。
type ChatChoice struct {
	Index        int         `json:"index"`
	Message      ChatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"` // stop|length|tool_calls|content_filter
}

// ChatUsage 是 token 用量。
type ChatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResponse 是 POST /v1/chat/completions 的非流式响应。
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"` // "chat.completion"
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []ChatChoice `json:"choices"`
	Usage   *ChatUsage   `json:"usage,omitempty"`
}

// ChatCompletionsHandler 处理 POST /v1/chat/completions。
// 只做协议收发：解码请求 → 调 ChatBackend → 编码响应。
// 选号/上游/记账都在 backend 里，handler 不碰。
func ChatCompletionsHandler(backend ChatBackend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		if err := decodeBody(w, r, &req); err != nil {
			return // decodeBody 已写错误响应
		}
		if req.Stream {
			// TODO(流式)：SSE 透传尚未实现。先明确拒绝，避免客户端挂起空等。
			http.Error(w, "streaming not implemented yet", http.StatusNotImplemented)
			return
		}
		resp, err := backend.Chat(r.Context(), &req)
		if err != nil {
			writeOpenAIError(w, backendStatus(err), err.Error())
			return
		}
		if resp.Object == "" {
			resp.Object = "chat.completion"
		}
		if resp.Created == 0 {
			resp.Created = time.Now().Unix()
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// openAIError 是 OpenAI 形态的错误体。
type openAIError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
	} `json:"error"`
}

// writeOpenAIError 按 OpenAI 形态写错误（chat 主路径用）。
func writeOpenAIError(w http.ResponseWriter, status int, msg string) {
	var e openAIError
	e.Error.Message = msg
	e.Error.Type = "gateway_error"
	writeJSON(w, status, e)
}
