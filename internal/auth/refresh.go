package auth

import (
	"errors"
	"time"

	"workbuddy2api-prime/internal/types"
)

// ErrRefreshNotImplemented 表示 token 刷新逻辑尚未实现。
// RefreshIfNeeded 在判定需要刷新但刷新器未就绪时返回此错误，
// 调用方可据此降级（例如跳过该账号、打日志告警）。
var ErrRefreshNotImplemented = errors.New("auth: token 刷新尚未实现")

// RefreshMargin 提前刷新窗口：token 在该时长内过期即视为需要刷新。
// 依据：linguo 的 NeedsRefresh 热路径检查思路（chat 热路径先判后刷）。
const RefreshMargin = 5 * time.Minute

// NeedsRefresh 判断账号 token 是否临近过期、需要刷新。
// ExpiresAt 为零值（无过期信息）时返回 false，不主动刷新。
func NeedsRefresh(acct *types.Account) bool {
	if acct == nil {
		return false
	}
	if acct.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().Add(RefreshMargin).After(acct.ExpiresAt)
}

// RefreshIfNeeded 在 token 临近过期时尝试刷新。
//
// 返回值：
//   - (false, nil)：无需刷新；
//   - (true, nil)：已刷新成功（当前占位实现不会走到这里）；
//   - (false, ErrRefreshNotImplemented)：需要刷新但刷新逻辑未实现。
//
// 刷新成功后应由调用方（或刷新器内部）调 Store.Save 原子写回，
// 依据 linguo 的"refresh → SaveAtomic"链路。
func RefreshIfNeeded(acct *types.Account) (bool, error) {
	if !NeedsRefresh(acct) {
		return false, nil
	}
	if err := refreshToken(acct); err != nil {
		return false, err
	}
	return true, nil
}

// refreshToken 执行实际的 token 刷新（占位实现）。
//
// 正式实现时应调：POST {chatBase}/v2/plugin/auth/token/refresh，
// 携带刷新专用头 X-Refresh-Token（安全红线：该头只允许出现在刷新请求里，
// chat 请求里绝不携带）与 X-Auth-Refresh-Source: plugin，
// 依据 linguo internal/upstream/client.go:RefreshToken 与 RefreshHeaders。
//
// 并发模型参考 linguo：锁内只做 refreshToken 快照读与一致性校验后写回，
// 网络 I/O 放在锁外（30s 超时）。
func refreshToken(acct *types.Account) error {
	_ = acct
	return ErrRefreshNotImplemented
}
