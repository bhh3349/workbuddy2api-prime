package upstream

import (
	"net/http"
	"regexp"
	"strings"

	"workbuddy2api-prime/internal/types"
)

// 错误分类逻辑移植自 workbuddy2api-panel internal/upstream/client.go 的 Classify()，
// 映射到本项目的 types.ReportStatus（pool 据此决定冷却/熔断策略）。
//
// 判定优先级（与上游一致，具体优先于宽泛）：
//  1. 11102「该后端无此模型」→ 限流（模型级冷却避让）
//  2. 402 → 积分耗尽
//  3. 会话死亡/账号故障标记（12153、11140、14017）→ 凭证失效/账号故障
//  4. 429 + 业务码 14018 → 积分耗尽；429 → 限流
//  5. 余额关键词（非 429）→ 积分耗尽
//  6. 400 参数/图片/内容拦截/超长 → 请求问题（不罚账号）
//  7. 404/5xx → 上游故障；401 → 凭证失效

// 余额不足关键词（移植自 panel hardMarkers，小写+中文双通道）。
var hardMarkers = []string{
	"insufficient credit", "no credit", "credit exhausted", "credits exhausted", "out of credit",
	"quota exceeded", "quota exhaust", "payment required", "credit not enough",
	"not enough credit",
	"积分不足", "额度不足", "余额不足", "积分用完", "额度用尽", "没有积分",
}

// 限流关键词（移植自 panel softRateMarkers 的核心子集）。
var softRateMarkers = []string{
	"rate limit", "rate_limit", "ratelimit", "too many requests",
	"frequency limit", "frequency_limit", "限流", "请求过于频繁",
}

// 会话死亡标记（移植自 panel sessionDeadMarkers）。
var sessionDeadMarkers = []string{"Offline user session not found", "12153"}

// 账号级故障标记（移植自 panel accountFaultMarkers）。
var accountFaultMarkers = []string{
	"request illegal", // code 11140
	"trial not activated",
	"trial version is not yet activated",
	"14017",
}

// 内容策略拦截关键词（移植自 panel contentBlockedMarkers）。
var contentBlockedMarkers = []string{
	"blocked by security policy",
	"unapproved channel",
	"illegal api invocation",
}

// 图片错误关键词。
var imageInvalidMarkers = []string{"11135", "invalid_image_data", "invalid image"}

// 参数错误标记（移植自 panel badParamsMarkerMsg/Code）。
var badParamsMarkers = []string{"Unmarshal chat params failed", "11101"}

// 上下文超长标记。
var promptTooLongMarkers = []string{"11115", "prompt is too long", "context too long", "context_length_exceeds"}

// 业务码提取：上游信封 {"code": 14018, ...}，code 可能是数字或字符串。
var businessCodeRe = regexp.MustCompile(`"code"\s*:\s*"?(\d+)"?`)

func businessCode(body string) string {
	m := businessCodeRe.FindStringSubmatch(body)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

func containsAnyFold(s string, markers []string) bool {
	lower := strings.ToLower(s)
	for _, m := range markers {
		if strings.Contains(lower, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// classifyUpstreamError 把上游 HTTP 状态码 + 响应正文映射到 types.ReportStatus。
func classifyUpstreamError(status int, body string) types.ReportStatus {
	// 11102「该后端无此模型」最先判：确定性答复，语义比计费/限流更具体。
	// 本项目 pool 无 (账号,模型) 负缓存，用模型级冷却近似（ReportRateLimited 会写 modelCooldown）。
	if (status == http.StatusBadRequest || status == http.StatusNotFound) && businessCode(body) == "11102" {
		return types.ReportRateLimited
	}
	// 11128「conversation id missing」：上游渠道校验未通过（会话头缺失/格式不对，
	// 2026-10-16 起收紧，见 linguo issue #188）。注意外层 code 是 no_healthy_account，
	// 11128 藏在转义的内层 message 里，businessCode 正则抓不到，按 marker 直接判。
	// 请求级问题：换号重试无用（头不对换谁都一样），不罚账号。
	if containsAny(body, []string{"11128", "conversation id missing"}) {
		return types.ReportBadRequest
	}
	// 402：真正的余额耗尽状态码。
	if status == http.StatusPaymentRequired {
		return types.ReportCreditExhausted
	}
	// 会话死亡 / 账号级故障先于 429：终态错误不得被限流语义掩盖。
	if containsAny(body, sessionDeadMarkers) {
		return types.ReportAuthFailed
	}
	if containsAnyFold(body, accountFaultMarkers) || businessCode(body) == "11140" {
		// 11140 上游账号风控限制：hub 按 2h 冷却处理；本项目 pool 无独立档位，
		// 用限流退避近似（指数退避），避免 ReportAuthFailed 的永久停用过于激进。
		// TODO: pool 增加"账号风控限制"独立冷却档位后改映射。
		return types.ReportRateLimited
	}
	// 14018 是明确的账号积分耗尽业务码，必须先于通用 429 兜底。
	if status == http.StatusTooManyRequests && businessCode(body) == "14018" {
		return types.ReportCreditExhausted
	}
	if status == http.StatusTooManyRequests {
		return types.ReportRateLimited
	}
	// 非 429 状态码下的余额关键词 → 积分耗尽。
	if containsAnyFold(body, hardMarkers) {
		return types.ReportCreditExhausted
	}
	if containsAnyFold(body, softRateMarkers) {
		return types.ReportRateLimited
	}
	// 401：凭证失效。
	if status == http.StatusUnauthorized {
		return types.ReportAuthFailed
	}
	if status == http.StatusNotFound {
		return types.ReportUpstreamError
	}
	if status >= 500 {
		return types.ReportUpstreamError
	}
	// 400 系列：请求级问题，不罚账号。
	if status == http.StatusBadRequest {
		if containsAnyFold(body, imageInvalidMarkers) ||
			containsAny(body, badParamsMarkers) ||
			containsAnyFold(body, promptTooLongMarkers) ||
			containsAnyFold(body, contentBlockedMarkers) {
			return types.ReportBadRequest
		}
		return types.ReportBadRequest
	}
	if status == http.StatusForbidden {
		// 403 内容审核 → 不罚账号；其他 403 按上游故障计一次。
		if containsAnyFold(body, contentBlockedMarkers) {
			return types.ReportBadRequest
		}
		return types.ReportUpstreamError
	}
	// 其他 4xx：请求问题，不罚账号。
	if status >= 400 {
		return types.ReportBadRequest
	}
	return types.ReportOK
}
