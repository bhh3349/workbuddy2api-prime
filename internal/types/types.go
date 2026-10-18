// Package types 定义 workbuddy2api-prime 全项目共享的核心类型。
// 各模块（auth/pool/upstream/protocol/apikeys/router/scheduler/billing）都依赖它，
// 它不依赖任何内部包，避免循环引用。
package types

import "time"

// Account 是网关托管的一个 WorkBuddy/CodeBuddy 账号。
type Account struct {
	ID                string // 内部稳定 ID（uid 或派生）
	UID               string // 上游用户 ID
	Region            string // "cn" | "intl"
	AccessToken       string // 明文只在内存；落盘见 auth.Store（0600）
	RefreshToken      string
	DeviceFingerprint string // 每账号固定虚拟设备指纹，防关联
	DeviceToken       string // X-Device-Token（宿主提供）
	Nickname          string // 昵称（成长任务桌面指纹上报用）
	EnterpriseID      string // 企业 ID（非空=企业号，跳过个人成长任务体系）
	ExpiresAt         time.Time
	Disabled          bool
	DisabledModels    []string  // 账号级模型黑名单（精确匹配）
	Paused            bool      // 暂停选号：退出选号池，但照常签到/保活/刷新余额
	Weight            float64   // 手动权重，默认 1
	CreditTotal       float64   // 剩余额度（积分）
	CreditExpiresAt   time.Time // 额度到期时间（最早到期优先路由用）
	UpdatedAt         time.Time
}

// ModelPolicy 描述一次请求期望的模型策略。
type ModelPolicy struct {
	Model    string // 具体模型名，或 "auto" / "cn:auto"
	Fallback []string
}

// PickRequest 是 pool.Pick 的输入。
type PickRequest struct {
	Model      string // 解析后的目标模型
	SessionKey string // 会话粘性键（可空）
	Background bool   // 是否后台任务（触发防烧钱重定向）
	APIKeyID   string // 调用方 key id（记账用）
	TopLevel   string // 顶层 key 名称（记账用）
}

// PickResult 是 pool.Pick 的输出。
type PickResult struct {
	Account *Account
	Reason  string // 选号原因（debug/日志）
}

// ReportStatus 是单次上游调用的结果分类。
type ReportStatus int

const (
	ReportOK              ReportStatus = iota
	ReportRateLimited                  // 429 / 限流 → 指数退避
	ReportCreditExhausted              // 积分耗尽 → 分钟级软冷却
	ReportBadRequest                   // 400 类 → 不罚账号，记日志
	ReportUpstreamError                // 5xx / 网络 → 连败计数
	ReportAuthFailed                   // 401 → 标记账号需刷新/停用
)

// CallReport 是 pool.Report 的输入。
type CallReport struct {
	AccountID  string
	Model      string
	Status     ReportStatus
	LatencyMs  int64
	CreditCost float64 // 上游上报的实际消耗积分
	APIKeyID   string
	ErrCode    string
}

// UsageRecord 是 billing 的逐请求账本条目。
type UsageRecord struct {
	Time      time.Time
	APIKeyID  string
	AccountID string
	Model     string
	PromptTok int
	CompTok   int
	Credit    float64
	LatencyMs int64
	OK        bool
}

// APIKey 是 apikeys 管理的子钥匙。
type APIKey struct {
	ID         string // wbk_ 前缀
	Hash       string // sha256（服务端只存摘要）
	Name       string
	Quota      float64 // -1 不限
	Used       float64
	Models     []string // 模型白名单，空=全部
	IPAllow    []string
	Disabled   bool
	ExpiresAt  time.Time
	CreatedAt  time.Time
	Concurrent int // 并发上限，0=不限
}

// CheckinResult 是 scheduler 的签到结果（落盘）。
type CheckinResult struct {
	Time      time.Time
	AccountID string
	OK        bool
	Detail    string
}
