package upstream

import (
	"strings"
	"testing"

	"workbuddy2api-prime/internal/types"
)

func TestClassifyUpstreamError(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   types.ReportStatus
	}{
		{"429 普通限流", 429, `{"code":14003,"msg":"too many requests"}`, types.ReportRateLimited},
		{"429+14018 积分耗尽", 429, `{"code":14018,"msg":"credits exhausted"}`, types.ReportCreditExhausted},
		{"402 余额耗尽", 402, `{"msg":"insufficient credit"}`, types.ReportCreditExhausted},
		{"401 凭证失效", 401, `unauthorized`, types.ReportAuthFailed},
		{"401+12153 会话死亡", 401, `{"code":12153,"msg":"Offline user session not found"}`, types.ReportAuthFailed},
		{"403+11140 账号风控", 403, `{"code":11140,"msg":"request illegal"}`, types.ReportRateLimited},
		{"400+11101 参数错误", 400, `Unmarshal chat params failed`, types.ReportBadRequest},
		{"400+11133 参数无效", 400, `{"code":11133,"msg":"model_param_invalid"}`, types.ReportBadRequest},
		{"400+11102 无此模型", 400, `{"code":11102,"msg":"service info not found"}`, types.ReportRateLimited},
		{"400 内容拦截", 400, `blocked by security policy`, types.ReportBadRequest},
		{"400+11115 超长", 400, `{"code":11115,"msg":"prompt is too long"}`, types.ReportBadRequest},
		{"404", 404, `not found`, types.ReportUpstreamError},
		{"500", 500, `internal error`, types.ReportUpstreamError},
		{"余额关键词非429", 200, `{"code":0,"msg":"积分不足"}`, types.ReportCreditExhausted},
		{"503+11128 渠道校验未通过", 503, `{"code":"no_healthy_account","message":"{\"code\":11128,\"msg\":\"conversation id missing\",\"requestId\":\"xxx\"}"}`, types.ReportBadRequest},
	}
	for _, c := range cases {
		if got := classifyUpstreamError(c.status, c.body); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAggregateSSE(t *testing.T) {
	sse := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"你好\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"世界\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}}\n\n" +
		"data: [DONE]\n"
	resp, err := aggregateSSE(strings.NewReader(sse), "test-model")
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Choices[0].Message.Content.Text(); got != "你好世界" {
		t.Errorf("content = %q", got)
	}
	if resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q", resp.Choices[0].FinishReason)
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 14 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestAggregateSSEToolCalls(t *testing.T) {
	sse := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"北京\\\"}\"}}]},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n"
	resp, err := aggregateSSE(strings.NewReader(sse), "test-model")
	if err != nil {
		t.Fatal(err)
	}
	tcs := resp.Choices[0].Message.ToolCalls
	if len(tcs) != 1 {
		t.Fatalf("tool_calls = %d", len(tcs))
	}
	if tcs[0].ID != "call_1" || tcs[0].Function.Name != "get_weather" {
		t.Errorf("tool_call = %+v", tcs[0])
	}
	if !strings.Contains(tcs[0].Function.Arguments, "北京") {
		t.Errorf("arguments = %q", tcs[0].Function.Arguments)
	}
}

func TestSessionHeadersFormat(t *testing.T) {
	h := sessionHeaders()
	uuidV4 := func(s string) bool {
		if len(s) != 36 {
			return false
		}
		// version 位为 4，variant 位为 8/9/a/b
		return s[14] == '4' && (s[19] == '8' || s[19] == '9' || s[19] == 'a' || s[19] == 'b')
	}
	for _, k := range []string{"X-Conversation-ID", "X-Conversation-Request-ID", "X-Conversation-Message-ID", "X-Root-Request-ID"} {
		v := h.Get(k)
		if !uuidV4(v) {
			t.Errorf("%s 应为 UUID v4，got %q", k, v)
		}
	}
	// X-Request-ID：32 位 hex（B3 兼容，不与 Message-ID 同值）
	rid := h.Get("X-Request-ID")
	if len(rid) != 32 {
		t.Errorf("X-Request-ID 应为 32 hex，got %q", rid)
	}
	for _, c := range rid {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			t.Errorf("X-Request-ID 含非 hex 字符：%q", rid)
			break
		}
	}
	// X-Trace-ID = X-Conversation-Request-ID
	if h.Get("X-Trace-ID") != h.Get("X-Conversation-Request-ID") {
		t.Errorf("X-Trace-ID 应等于 X-Conversation-Request-ID")
	}
}
