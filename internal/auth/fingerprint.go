package auth

import (
	"crypto/sha256"
	"encoding/hex"
)

// DeriveFingerprint 为每个账号派生一台"固定虚拟设备"的指纹。
//
// 实现：sha256("wb2a:" + uid) 的 hex 编码。
// 依据：linguo 的 deriveAccountStableID —— sha256("wb2a:"+purpose+":"+uid)。
//
// 设计意图（反设备关联风控）：
//   - 同一账号永远派生出同一个指纹，不会漂移（防"随机机器码"触发风控）；
//   - 不同账号的指纹天然隔离，多号不会被按设备指纹关联。
// 用途：出站头 X-Machine-ID / X-Session-ID（见 headers.go）。
func DeriveFingerprint(uid string) string {
	sum := sha256.Sum256([]byte("wb2a:" + uid))
	return hex.EncodeToString(sum[:])
}

// deriveSessionID 为每个账号派生固定的会话设备 ID。
// 与 DeriveFingerprint 用不同的 purpose 前缀，避免两个头撞值。
func deriveSessionID(uid string) string {
	sum := sha256.Sum256([]byte("wb2a:session:" + uid))
	return hex.EncodeToString(sum[:])
}
