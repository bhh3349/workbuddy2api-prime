package protocol

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// 本文件实现 POST /v1/responses（OpenAI Responses 协议）的兼容层。
// 依据：分析文档 §4.3 /v1/responses 入站（responsesToChat）与出站（responseObject）。
//
// 同样复用同一个 ChatBackend；工具调用的"修复①/②"在此同样生效：
// 入站 function_call / function_call_output 全量翻译、id 原样透传；
// 出站只要有 tool_calls，output 就带 function_call 项（继续信号），
// 不因上游 finish_reason=stop 而收尾成终止。

// ResponseItem 是 Responses 协议的输入/输出条目。
type ResponseItem struct {
	Type      string          `json:"type"` // message|function_call|function_call_output
	Role      string          `json:"role,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"` // message 条目的内容块
	ID        string          `json:"id,omitempty"`
	CallID    string          `json:"call_id,omitempty"`   // function_call / output 的调用 id
	Name      string          `json:"name,omitempty"`      // function_call 的函数名
	Arguments string          `json:"arguments,omitempty"` // function_call 的入参（JSON 字符串）
	Output    json.RawMessage `json:"output,omitempty"`    // function_call_output 的结果
	Status    string          `json:"status,omitempty"`
}

// ResponsesTool 是 Responses 的工具定义（function 形态）。
type ResponsesTool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ResponsesRequest 是 POST /v1/responses 的请求结构。
type ResponsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"` // 字符串或 []ResponseItem
	Instructions    json.RawMessage `json:"instructions,omitempty"`
	Tools           []ResponsesTool `json:"tools,omitempty"`
	ToolChoice      any             `json:"tool_choice,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	ParallelToolCalls *bool         `json:"parallel_tool_calls,omitempty"`
}

// responsesContentText 提取 Responses message 条目的文本
//（input_text 块或纯字符串）。
func responsesContentText(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"` // input_text | input_image | output_text
		Text string `json:"text,omitempty"`
	}
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var sb strings.Builder
		for _, b := range blocks {
			if b.Type == "input_text" || b.Type == "output_text" || b.Type == "text" {
				sb.WriteString(b.Text)
			}
		}
		return sb.String()
	}
	return ""
}

// responsesToChat 把 Responses 请求翻译成内部统一的 ChatRequest。
func responsesToChat(in *ResponsesRequest) *ChatRequest {
	out := &ChatRequest{
		Model:       in.Model,
		Temperature: in.Temperature,
		TopP:        in.TopP,
	}
	// max_output_tokens → max_tokens。
	out.MaxTokens = in.MaxOutputTokens
	// 注：Responses 的 parallel_tool_calls 在 OpenAI chat 侧无等价参数，暂不支持。

	// instructions（字符串或块数组）→ system 消息。
	if len(bytes.TrimSpace(in.Instructions)) > 0 {
		if text := responsesContentText(in.Instructions); text != "" {
			out.Messages = append(out.Messages, ChatMessage{
				Role:    "system",
				Content: NewTextContent(text),
			})
		}
	}

	// input：字符串 → 单条 user 消息；数组 → 逐项翻译。
	var s string
	if err := json.Unmarshal(in.Input, &s); err == nil {
		out.Messages = append(out.Messages, ChatMessage{
			Role:    "user",
			Content: NewTextContent(s),
		})
	} else {
		var items []ResponseItem
		if err := json.Unmarshal(in.Input, &items); err == nil {
			// 连续的 function_call 合并成一条 assistant 消息一次带齐
			//（OpenAI 要求一轮调用一次带齐）。
			var pending []ToolCall
			flush := func() {
				if len(pending) == 0 {
					return
				}
				out.Messages = append(out.Messages, ChatMessage{
					Role:      "assistant",
					Content:   NewTextContent(""),
					ToolCalls: pending,
				})
				pending = nil
			}
			for _, it := range items {
				switch it.Type {
				case "function_call":
					// 修复①：call_id 原样透传为 tool_call id。
					tc := ToolCall{ID: it.CallID, Type: "function"}
					if tc.ID == "" {
						tc.ID = newID("call_")
					}
					tc.Function.Name = it.Name
					tc.Function.Arguments = it.Arguments
					if tc.Function.Arguments == "" {
						tc.Function.Arguments = "{}"
					}
					pending = append(pending, tc)
				case "function_call_output":
					flush()
					// 会话回放：function_call_output → role:tool。
					// 缺了它，Codex 等客户端后续轮次会因"调用无结果"报 400。
					out.Messages = append(out.Messages, ChatMessage{
						Role:       "tool",
						ToolCallID: it.CallID,
						Content:    NewTextContent(responsesContentText(it.Output)),
					})
				case "message":
					flush()
					role := it.Role
					if role == "" {
						role = "user"
					}
					out.Messages = append(out.Messages, ChatMessage{
						Role:    role,
						Content: NewTextContent(responsesContentText(it.Content)),
					})
				default:
					flush()
				}
			}
			flush()
		}
	}

	// 工具定义翻译。
	for _, t := range in.Tools {
		if t.Name == "" {
			continue
		}
		var ct ChatTool
		ct.Type = "function"
		ct.Function.Name = t.Name
		ct.Function.Description = t.Description
		if len(bytes.TrimSpace(t.Parameters)) > 0 {
			var schema any
			if json.Unmarshal(t.Parameters, &schema) == nil {
				ct.Function.Parameters = schema
			}
		}
		out.Tools = append(out.Tools, ct)
	}
	out.ToolChoice = in.ToolChoice
	return out
}

// ResponsesUsage 是 Responses 形态的用量。
type ResponsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ResponsesResponse 是 POST /v1/responses 的非流式响应。
type ResponsesResponse struct {
	ID         string          `json:"id"`     // resp_xxx
	Object     string          `json:"object"` // "response"
	CreatedAt  int64           `json:"created_at"`
	Status     string          `json:"status"` // completed | incomplete
	Model      string          `json:"model"`
	Output     []ResponseItem  `json:"output"`
	OutputText string          `json:"output_text"`
	Usage      *ResponsesUsage `json:"usage,omitempty"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details,omitempty"`
}

// responsesError 是 Responses 形态的错误体。
type responsesError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
	} `json:"error"`
}

// writeResponsesError 按 Responses（OpenAI 形态）写错误。
func writeResponsesError(w http.ResponseWriter, status int, msg string) {
	var e responsesError
	e.Error.Message = msg
	e.Error.Type = "gateway_error"
	writeJSON(w, status, e)
}

// chatToResponses 把 ChatResponse 翻译成 ResponsesResponse。
// 修复②的落点：只要有 tool_calls，output 就带 function_call 项（"继续"信号），
// responsesStatus() 保证不因 finish_reason=stop 而误收尾。
func chatToResponses(resp *ChatResponse) *ResponsesResponse {
	out := &ResponsesResponse{
		ID:        newID("resp_"),
		Object:    "response",
		CreatedAt: time.Now().Unix(),
		Model:     resp.Model,
	}
	if len(resp.Choices) == 0 {
		out.Status = "completed"
		out.Output = []ResponseItem{{
			Type:   "message",
			Role:   "assistant",
			Status: "completed",
		}}
		return out
	}
	choice := resp.Choices[0]
	msg := choice.Message
	hasTools := len(msg.ToolCalls) > 0

	// 主 message 条目。
	text := msg.Content.Text()
	out.OutputText = text
	out.Output = append(out.Output, ResponseItem{
		Type:    "message",
		ID:      newID("msg_"),
		Role:    "assistant",
		Status:  "completed",
		Content: mustJSON([]map[string]string{{"type": "output_text", "text": text}}),
	})
	// tool_calls → function_call 条目挂在尾部，call_id 原样透传。
	for _, tc := range msg.ToolCalls {
		out.Output = append(out.Output, ResponseItem{
			Type:      "function_call",
			ID:        newID("fc_"),
			CallID:    tc.ID,
			Name:      tc.Function.Name,
			Arguments: tc.Function.Arguments,
			Status:    "completed",
		})
	}

	// 修复②：有工具调用即 completed（function_call 项在 output 里就是"继续"信号）。
	status, incompleteReason := responsesStatus(choice.FinishReason, hasTools)
	out.Status = status
	if incompleteReason != "" {
		out.IncompleteDetails = &struct {
			Reason string `json:"reason"`
		}{Reason: incompleteReason}
	}

	if resp.Usage != nil {
		out.Usage = &ResponsesUsage{
			InputTokens:  resp.Usage.PromptTokens,
			OutputTokens: resp.Usage.CompletionTokens,
			TotalTokens:  resp.Usage.TotalTokens,
		}
	}
	return out
}

// mustJSON 把 v 编成 JSON，失败返回空数组（兜底不中断）。
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage("[]")
	}
	return b
}

// ResponsesHandler 处理 POST /v1/responses。
// 流程：Responses 入站 → 翻译成 ChatRequest → 同一个 ChatBackend → 翻译回 Responses。
func ResponsesHandler(backend ChatBackend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in ResponsesRequest
		if err := decodeBody(w, r, &in); err != nil {
			return
		}
		if in.Stream {
			// TODO(流式)：Responses SSE 事件序列
			//（response.created/in_progress/output_item.added/…）尚未实现，先明确拒绝。
			writeResponsesError(w, http.StatusNotImplemented, "streaming not implemented yet")
			return
		}
		chatReq := responsesToChat(&in)
		resp, err := backend.Chat(r.Context(), chatReq)
		if err != nil {
			writeResponsesError(w, backendStatus(err), err.Error())
			return
		}
		writeJSON(w, http.StatusOK, chatToResponses(resp))
	}
}
