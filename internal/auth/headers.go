package auth

import (
	"net/http"

	"workbuddy2api-prime/internal/types"
)

// 出站鉴权相关头名。
const (
	HeaderAuthorization = "Authorization"   // Bearer <accessToken>
	HeaderDeviceToken   = "X-Device-Token"  // 设备风控头（宿主提供）
	HeaderMachineID     = "X-Machine-ID"    // 每账号固定虚拟设备
	HeaderSessionID     = "X-Session-ID"    // 每账号固定会话设备
	HeaderIDEName       = "X-IDE-Name"      // 用量归属伪装：桌面端指纹
	HeaderIDEType       = "X-IDE-Type"      // 用量归属伪装
	HeaderIDEVersion    = "X-IDE-Version"   // 用量归属伪装
	HeaderProduct       = "X-Product"       // 用量归属伪装
	HeaderAgentIntent   = "X-Agent-Intent"  // 用量归属伪装
)

// 客户端版本与默认 UA。
//
// 依据：hub 的 wb_identity 按 2026-10-16 mitmproxy 实测 5.8.4 客户端对齐；
// linguo 的三段式 UA 形态 WorkBuddy/<ver> WorkBuddy/<ver> CLI/<ver>。
const (
	// ClientVersion 出站伪装的客户端版本号。
	ClientVersion = "5.8.4"
	// DefaultUserAgent 三段式 UA（调用方未传 ua 时使用）。
	DefaultUserAgent = "WorkBuddy/" + ClientVersion + " WorkBuddy/" + ClientVersion + " CLI/" + ClientVersion
)

// BuildOutboundHeaders 构造发往上游的鉴权头。
//
// 包含四类（依据 linguo headers.go 的四类构造器思路，取 chat 场景子集）：
//  1. 身份：Authorization: Bearer <accessToken>
//  2. 设备风控：X-Device-Token（acct.DeviceToken，宿主提供；为空则不发，
//     依据 hub resolve_device_token"读不到不影响请求"）、
//     X-Machine-ID / X-Session-ID（每账号固定虚拟设备指纹，防关联）
//  3. 用量归属伪装：X-IDE-Name/Type/Version、X-Product、X-Agent-Intent，
//     伪装成桌面端，避免上游用量明细出现网关特征
//     （依据 linguo injectAttribution、hub build_identity_headers）
//  4. UA：调用方传入的 ua 优先，否则用三段式默认 UA
//
// 只用标准库。返回的 http.Header 可直接 Set 到出站请求上。
func BuildOutboundHeaders(acct *types.Account, ua string) http.Header {
	h := make(http.Header)

	// 1. 身份
	if acct.AccessToken != "" {
		h.Set(HeaderAuthorization, "Bearer "+acct.AccessToken)
	}

	// 2. 设备风控头
	// 本网关不生成 device token（linguo device_token.go 头注释：
	// 容器内无桌面端 Turing Shield SDK，token 需由宿主落盘提供）。
	if acct.DeviceToken != "" {
		h.Set(HeaderDeviceToken, acct.DeviceToken)
	}
	uid := acct.UID
	if uid == "" {
		uid = acct.ID
	}
	h.Set(HeaderMachineID, DeriveFingerprint(uid))
	h.Set(HeaderSessionID, deriveSessionID(uid))

	// 3. 用量归属伪装头：伪装成官方桌面端
	h.Set(HeaderIDEName, "WorkBuddy")
	h.Set(HeaderIDEType, "WorkBuddy")
	h.Set(HeaderIDEVersion, ClientVersion)
	h.Set(HeaderProduct, "SaaS")
	h.Set(HeaderAgentIntent, "craft")

	// 4. 三段式 UA
	if ua == "" {
		ua = DefaultUserAgent
	}
	h.Set("User-Agent", ua)

	return h
}
