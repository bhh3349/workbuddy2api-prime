package protocol

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// 本文件实现 POST /v1/messages（Anthropic 协议）的兼容层。
// 依据：分析文档 §4.3 /v1/messages 入站（messagesToChat）与出站（writeFinal）。
//
// 必修修复①（依据：§5.1 入站丢工具、§5.2 入站配对机制）：
// 入站必须把 tools / tool_choice / assistant.tool_use / user.tool_result
// 全部翻译成 OpenAI 形态，id 原样透传；否则上游收不到 tools，只回纯文本，
// Claude Code 拿到 stop_reason=end_turn 会直接收工（issue#2）。

// AnthropicBlock 是 Anthropic 消息内容块。
type AnthropicBlock struct {
	Type      string          `json:"type"` // text|image|tool_use|tool_result
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`          // tool_use 的 id
	Name      string          `json:"name,omitempty"`        // tool_use 的工具名
	Input     json.RawMessage `json:"input,omitempty"`       // tool_use 的入参（对象）
	ToolUseID string          `json:"tool_use_id,omitempty"` // tool_result 指向的 tool_use id
	Content   json.RawMessage `json:"content,omitempty"`     // tool_result 内容（字符串或块数组）
	IsError   bool            `json:"is_error,omitempty"`    // tool_result 是否报错
	Source    *struct {
		Type      string `json:"type"` // base64 | url
		MediaType string `json:"media_type,omitempty"`
		Data      string `json:"data,omitempty"`
		URL       string `json:"url,omitempty"`
	} `json:"source,omitempty"` // image 块的图片来源
}

// AnthropicMessage 是一条 Anthropic 消息，content 可为字符串或块数组。
type AnthropicMessage struct {
	Role    string          `json:"role"` // user | assistant
	Content json.RawMessage `json:"content"`
}

// AnthropicTool 是 Anthropic 的工具定义。
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

// AnthropicToolChoice 是 Anthropic 的工具调用策略。
type AnthropicToolChoice struct {
	Type string `json:"type"` // auto | any | tool | none
	Name string `json:"name,omitempty"`
}

// AnthropicRequest 是 POST /v1/messages 的请求结构。
type AnthropicRequest struct {
	Model         string               `json:"model"`
	System        json.RawMessage      `json:"system,omitempty"` // 字符串或块数组
	Messages      []AnthropicMessage   `json:"messages"`
	Tools         []AnthropicTool      `json:"tools,omitempty"`
	ToolChoice    *AnthropicToolChoice `json:"tool_choice,omitempty"`
	MaxTokens     int                  `json:"max_tokens"`
	Temperature   *float64             `json:"temperature,omitempty"`
	TopP          *float64             `json:"top_p,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
}

// anthropicTextBlocks 把"字符串或块数组"形态的 content 提取成纯文本。
func anthropicTextBlocks(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []AnthropicBlock
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// toolArgsString 把 tool_use 的 input 规范成 JSON 字符串（OpenAI arguments 形态）。
func toolArgsString(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return "{}"
	}
	if json.Valid(t) {
		return string(t)
	}
	b, _ := json.Marshal(string(t))
	return string(b)
}

// messagesToChat 把 Anthropic 请求翻译成内部统一的 ChatRequest。
// 修复①的全部要点都在这里：tools、tool_choice、tool_use、tool_result 一个不丢，
// tool_use_id / tool_call_id 原样透传。
func messagesToChat(in *AnthropicRequest) *ChatRequest {
	out := &ChatRequest{
		Model:       in.Model,
		Temperature: in.Temperature,
		TopP:        in.TopP,
		Stop:        in.StopSequences,
	}
	if in.MaxTokens > 0 {
		out.MaxTokens = &in.MaxTokens
	}

	// system（字符串或块数组）→ 前置 role:system 消息。
	if len(bytes.TrimSpace(in.System)) > 0 {
		if text := anthropicTextBlocks(in.System); text != "" {
			out.Messages = append(out.Messages, ChatMessage{
				Role:    "system",
				Content: NewTextContent(text),
			})
		}
	}

	for _, m := range in.Messages {
		switch m.Role {
		case "user":
			// 纯字符串形态：直接一条 user 消息。
			var s string
			if err := json.Unmarshal(m.Content, &s); err == nil {
				out.Messages = append(out.Messages, ChatMessage{
					Role:    "user",
					Content: NewTextContent(s),
				})
				continue
			}
			// 块数组形态：tool_result 必须独立成 role:tool 消息，
			// 且排在同批文本消息之前（满足 wire 顺序）。
			var blocks []AnthropicBlock
			if err := json.Unmarshal(m.Content, &blocks); err != nil {
				continue
			}
			var texts []string
			var parts []ChatContentPart
			for _, b := range blocks {
				switch b.Type {
				case "text":
					texts = append(texts, b.Text)
				case "image":
					// Anthropic image → OpenAI image_url。
					url := ""
					if b.Source != nil {
						switch b.Source.Type {
						case "base64":
							url = "data:" + b.Source.MediaType + ";base64," + b.Source.Data
						case "url":
							url = b.Source.URL
						}
					}
					if url != "" {
						parts = append(parts, ChatContentPart{
							Type:     "image_url",
							ImageURL: &struct {
								URL string `json:"url"`
							}{URL: url},
						})
					}
				case "tool_result":
					// 修复①核心：tool_result → 独立 role:tool 消息，
					// tool_call_id 原样透传 tool_use_id（不丢、不改写）。
					text := anthropicTextBlocks(b.Content)
					if b.IsError {
						text = "[tool_error] " + text // OpenAI role:tool 无错误字段，打标记
					}
					out.Messages = append(out.Messages, ChatMessage{
						Role:       "tool",
						ToolCallID: b.ToolUseID,
						Content:    NewTextContent(text),
					})
				}
			}
			if len(texts) > 0 || len(parts) > 0 {
				for _, t := range texts {
					parts = append([]ChatContentPart{{Type: "text", Text: t}}, parts...)
				}
				out.Messages = append(out.Messages, ChatMessage{
					Role:    "user",
					Content: ChatMessageContent{Parts: parts},
				})
			}
		case "assistant":
			var s string
			if err := json.Unmarshal(m.Content, &s); err == nil {
				out.Messages = append(out.Messages, ChatMessage{
					Role:    "assistant",
					Content: NewTextContent(s),
				})
				continue
			}
			var blocks []AnthropicBlock
			if err := json.Unmarshal(m.Content, &blocks); err != nil {
				continue
			}
			msg := ChatMessage{Role: "assistant"}
			var texts []string
			for _, b := range blocks {
				switch b.Type {
				case "text":
					texts = append(texts, b.Text)
				case "tool_use":
					// 修复①核心：tool_use → tool_calls，id 原样透传；
					// 缺 id 时生成 toolu_ 前缀（Anthropic 要求 id 非空）。
					id := b.ID
					if id == "" {
						id = newID("toolu_")
					}
					tc := ToolCall{ID: id, Type: "function"}
					tc.Function.Name = b.Name
					tc.Function.Arguments = toolArgsString(b.Input)
					msg.ToolCalls = append(msg.ToolCalls, tc)
				}
			}
			msg.Content = NewTextContent(strings.Join(texts, ""))
			out.Messages = append(out.Messages, msg)
		}
	}

	// 工具定义翻译：缺 name 的跳过（上游会 400）。
	for _, t := range in.Tools {
		if t.Name == "" {
			continue
		}
		var ct ChatTool
		ct.Type = "function"
		ct.Function.Name = t.Name
		ct.Function.Description = t.Description
		if len(bytes.TrimSpace(t.InputSchema)) > 0 {
			var schema any
			if json.Unmarshal(t.InputSchema, &schema) == nil {
				ct.Function.Parameters = schema
			}
		}
		out.Tools = append(out.Tools, ct)
	}

	// tool_choice 翻译；无 tools 时不下发（防上游 400）。
	if in.ToolChoice != nil && len(out.Tools) > 0 {
		switch in.ToolChoice.Type {
		case "auto":
			out.ToolChoice = "auto"
		case "any":
			out.ToolChoice = "required"
		case "none":
			out.ToolChoice = "none"
		case "tool":
			if in.ToolChoice.Name != "" {
				out.ToolChoice = map[string]any{
					"type":     "function",
					"function": map[string]any{"name": in.ToolChoice.Name},
				}
			}
		}
	}
	return out
}

// AnthropicUsage 是 Anthropic 形态的用量（恒带缓存字段，缺源数据回落 0）。
type AnthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// AnthropicResponse 是 POST /v1/messages 的非流式响应。
type AnthropicResponse struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"` // "message"
	Role       string           `json:"role"` // "assistant"
	Model      string           `json:"model"`
	Content    []AnthropicBlock `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      *AnthropicUsage  `json:"usage,omitempty"`
	CreatedAt  int64            `json:"created_at,omitempty"`
}

// chatToAnthropic 把 ChatResponse 翻译成 AnthropicResponse。
// 修复②的落点：stop_reason 经 stopReason() 计算，有工具调用即 tool_use。
func chatToAnthropic(resp *ChatResponse) *AnthropicResponse {
	out := &AnthropicResponse{
		ID:        newID("msg_"),
		Type:      "message",
		Role:      "assistant",
		Model:     resp.Model,
		CreatedAt: time.Now().Unix(),
	}
	if len(resp.Choices) == 0 {
		out.Content = []AnthropicBlock{{Type: "text", Text: ""}}
		out.StopReason = "end_turn"
		return out
	}
	choice := resp.Choices[0]
	msg := choice.Message
	hasTools := len(msg.ToolCalls) > 0

	// 文本块（如有）。
	if text := msg.Content.Text(); text != "" {
		out.Content = append(out.Content, AnthropicBlock{Type: "text", Text: text})
	}
	// tool_calls → tool_use 块，id 原样带回（客户端下一轮用同一 id 发 tool_result 闭环）。
	for _, tc := range msg.ToolCalls {
		var input any = map[string]any{}
		_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
		raw, _ := json.Marshal(input)
		out.Content = append(out.Content, AnthropicBlock{
			Type:  "tool_use",
			ID:    tc.ID,
			Name:  tc.Function.Name,
			Input: raw,
		})
	}
	// Anthropic 要求 content 非空：兜底一个空 text 块。
	if len(out.Content) == 0 {
		out.Content = []AnthropicBlock{{Type: "text", Text: ""}}
	}

	// 修复②：有工具调用即 tool_use，覆盖上游 finish_reason=stop 的误导。
	out.StopReason = stopReason(choice.FinishReason, hasTools)

	if resp.Usage != nil {
		out.Usage = &AnthropicUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
		}
	}
	return out
}

// anthropicError 是 Anthropic 形态的错误体。
type anthropicError struct {
	Type  string `json:"type"` // "error"
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// writeAnthropicError 按 Anthropic 形态写错误（错误类型按状态码分发）。
func writeAnthropicError(w http.ResponseWriter, status int, msg string) {
	var e anthropicError
	e.Type = "error"
	e.Error.Type = anthropicErrType(status)
	e.Error.Message = msg
	writeJSON(w, status, e)
}

// MessagesHandler 处理 POST /v1/messages。
// 流程：Anthropic 入站 → 翻译成 ChatRequest → 同一个 ChatBackend → 翻译回 Anthropic。
// 调度/冷却/记账零复制——全在 backend 里（compatRun 思路）。
func MessagesHandler(backend ChatBackend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in AnthropicRequest
		if err := decodeBody(w, r, &in); err != nil {
			return
		}
		if in.Stream {
			// TODO(流式)：Anthropic SSE 事件序列（message_start/ping/content_block_*）
			// 尚未实现，先明确拒绝。
			writeAnthropicError(w, http.StatusNotImplemented, "streaming not implemented yet")
			return
		}
		chatReq := messagesToChat(&in)
		resp, err := backend.Chat(r.Context(), chatReq)
		if err != nil {
			writeAnthropicError(w, backendStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, chatToAnthropic(resp))
	}
}
