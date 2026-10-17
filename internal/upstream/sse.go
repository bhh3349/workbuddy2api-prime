package upstream

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"workbuddy2api-prime/internal/protocol"
)

// 上游 SSE 事件块的最小结构（只取聚合需要的字段，未知字段忽略）。
type sseChunk struct {
	ID      string `json:"id"`
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *protocol.ChatUsage `json:"usage"`
}

// aggregateSSE 从上游 SSE 流聚合出一条完整响应。
// 做法与 hub 的 aggregate_stream 一致：上游恒以 stream=true 请求，
// 非流式客户端由网关在本地聚合成完整 ChatResponse。
func aggregateSSE(r io.Reader, model string) (*protocol.ChatResponse, error) {
	var content strings.Builder
	// toolCalls 按 index 合并：arguments 分片拼接（SSE 里 tool_calls 按 index 增量下发）。
	type tcBuilder struct {
		id, typ, name string
		args          strings.Builder
		order         int
	}
	builders := map[int]*tcBuilder{}
	var order []int
	var finishReason string
	var usage *protocol.ChatUsage
	var respID string

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024) // 单行上限 1MB
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, ":") {
			continue // 空行 / SSE 注释
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk sseChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // 坏帧跳过（宽容）
		}
		if respID == "" && chunk.ID != "" {
			respID = chunk.ID
		}
		for _, ch := range chunk.Choices {
			content.WriteString(ch.Delta.Content)
			if ch.FinishReason != "" {
				finishReason = ch.FinishReason
			}
			for _, tc := range ch.Delta.ToolCalls {
				b, ok := builders[tc.Index]
				if !ok {
					b = &tcBuilder{order: len(order)}
					builders[tc.Index] = b
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					b.id = tc.ID
				}
				if tc.Type != "" {
					b.typ = tc.Type
				}
				if tc.Function.Name != "" {
					b.name = tc.Function.Name
				}
				b.args.WriteString(tc.Function.Arguments)
			}
		}
		if chunk.Usage != nil {
			usage = chunk.Usage // 取末帧 usage（含 stream_options.include_usage）
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("读取上游 SSE 流失败: %w", err)
	}

	// 组装 tool_calls（按 index 顺序）。
	var toolCalls []protocol.ToolCall
	for _, idx := range order {
		b := builders[idx]
		var tc protocol.ToolCall
		tc.ID = b.id
		tc.Type = b.typ
		if tc.Type == "" {
			tc.Type = "function"
		}
		// protocol.ToolCall.Function 是匿名结构体，逐字段赋值。
		tc.Function.Name = b.name
		tc.Function.Arguments = b.args.String()
		toolCalls = append(toolCalls, tc)
	}

	if respID == "" {
		respID = "chatcmpl-prime"
	}
	if finishReason == "" {
		finishReason = "stop"
	}
	resp := &protocol.ChatResponse{
		ID:      respID,
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   model,
		Choices: []protocol.ChatChoice{
			{
				Index: 0,
				Message: protocol.ChatMessage{
					Role:      "assistant",
					Content:   protocol.NewTextContent(content.String()),
					ToolCalls: toolCalls,
				},
				FinishReason: finishReason,
			},
		},
		Usage: usage,
	}
	return resp, nil
}
