package protocol

// 本文件收敛"出站终止原因"的翻译逻辑。
//
// 必修修复②（依据：分析文档 §5.1 根因、§5.2 stopReason 兜底）：
// 部分上游在返回 tool_calls 时，finish_reason 仍给 "stop"。
// 如果照字面映射成 Anthropic 的 "end_turn"，Claude Code 等 agent 客户端会判定
// "本轮已结束"直接收工——工具调用被当成对话终止，任务几秒中断、实际什么都没做
// （panel-plus issue#2 的同类病）。
//
// 规则：只要出站消息里带工具调用（tool_use，哪怕还没配对 tool_result），
// 一律映射为"继续工具调用"，覆盖上游 finish_reason=stop 的误导。

// stopReason 把 OpenAI finish_reason 翻译成 Anthropic stop_reason。
// hasToolCalls 表示出站 assistant 消息是否携带 tool_calls（即是否有 tool_use 块要发）。
func stopReason(finishReason string, hasToolCalls bool) string {
	// 有工具即 tool_use：覆盖上游 finish_reason=stop 的误导（issue#2）。
	if hasToolCalls {
		return "tool_use"
	}
	switch finishReason {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	default:
		return "end_turn"
	}
}

// anthropicErrType 把 HTTP 状态码映射成 Anthropic 形态的错误类型
//（依据：分析文档 §4.3 writeCompatError / anthropicErrType）。
func anthropicErrType(status int) string {
	switch status {
	case 401, 403:
		return "authentication_error"
	case 404:
		return "not_found_error"
	case 429:
		return "rate_limit_error"
	case 413:
		return "request_too_large"
	}
	if status >= 400 && status < 500 {
		return "invalid_request_error"
	}
	return "api_error"
}

// responsesStatus 把 OpenAI finish_reason 翻译成 Responses 的 response.status。
// 带工具调用时一律 completed（function_call 在 output 里即"继续"信号，
// 不因 finish_reason=stop 而收尾成终止——同样是修复②的覆盖面）。
func responsesStatus(finishReason string, hasToolCalls bool) (status string, incompleteReason string) {
	if hasToolCalls {
		return "completed", ""
	}
	if finishReason == "length" {
		return "incomplete", "max_output_tokens"
	}
	return "completed", ""
}
