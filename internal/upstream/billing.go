package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"workbuddy2api-prime/internal/auth"
	"workbuddy2api-prime/internal/types"
)

// Package billing 实现 billing 域（签到 / 余额）的上游调用。
//
// 端点与协议细节以克隆仓库实际代码为准：
//   - linguo internal/upstream/client.go:753-775：billingBaseCN="https://www.codebuddy.cn"；
//     billingMeterPath="/billing/meter/get-user-resource"（global 首选），
//     billingMeterPathV2="/v2/billing/meter/get-user-resource"（CN 现状 / global fallback）；
//     dailyCheckinPath="/billing/meter/daily-checkin"，dailyCheckinPathV2="/v2/billing/meter/daily-checkin"。
//     global 按 [无/v2, 有/v2] 顺序、404 时 fallback；cn 只走有 /v2 的单路径。
//   - linguo report.go:billingJSON：billing 域请求头统一 BillingHeaders；
//     信封 {code,msg,data}，HTTP 非 2xx 或 code!=0 即错误。
//   - 余额请求体（client.go:2051 UserResourceDetailedWithExpiry）：
//     {PageNumber:1, PageSize:100, ProductCode:"p_tcaca", Status:[0,3],
//     PackageEndTimeRangeBegin: now, PackageEndTimeRangeEnd: now+101年}，
//     时间格式 "2006-01-02 15:04:05"。
//   - 响应 Response.Data.Accounts[]：PackageName / CycleEndTime /
//     CapacitySize|Remain|Used / CycleCapacitySize|Remain|Used；
//     单套餐取数规则见 packageRemainUsed（Cycle 期优先，remain 钳 [0,size]）。
//   - 签到（client.go:2178 DailyCheckin）：POST daily-checkin，body {}；
//     已签到（code=10001 或 msg 含"已签到"/"already"）视为幂等成功；
//     偶发 5xx 做有界重试（2 次，间隔 2s/4s）。
//   - hub wb_accounts.py:checkin：签到仅国内版；企业号无签到体系。
const (
	billingBaseCN   = "https://www.codebuddy.cn"
	billingBaseIntl = "https://www.workbuddy.ai"

	billingResPath   = "/billing/meter/get-user-resource"
	billingResPathV2 = "/v2/billing/meter/get-user-resource"
	checkinPath      = "/billing/meter/daily-checkin"
	checkinPathV2    = "/v2/billing/meter/daily-checkin"

	packageEndLayout = "2006-01-02 15:04:05"
)

// 哨兵错误：调用方用 errors.Is 区分。
var (
	// ErrAlreadyCheckedIn 今日已签到（幂等成功）。
	ErrAlreadyCheckedIn = errors.New("今日已签到")
	// ErrCheckinUnsupported 该账号无签到体系（国际版/企业号），应跳过签到。
	ErrCheckinUnsupported = errors.New("该账号无签到体系，跳过签到")
)

// BillingError 是上游 billing 域的业务错误（HTTP 2xx 但 code!=0，或 HTTP 错误）。
type BillingError struct {
	Code       int    // 上游业务 code（HTTP 错误时为 -1）
	HTTPStatus int    // HTTP 状态码
	Msg        string // 上游 msg 或 body 片段
}

func (e *BillingError) Error() string {
	if e.Code >= 0 {
		return fmt.Sprintf("上游返回 code=%d msg=%s", e.Code, e.Msg)
	}
	return fmt.Sprintf("上游 HTTP %d: %s", e.HTTPStatus, e.Msg)
}

// AlreadyCheckedIn 报告是否为"今日已签到"的幂等拒绝。
func (e *BillingError) AlreadyCheckedIn() bool {
	if e.Code == 10001 {
		return true
	}
	m := strings.ToLower(e.Msg)
	return strings.Contains(m, "已签到") || strings.Contains(m, "already")
}

// Billing 是 billing 域（签到/余额）上游客户端。
type Billing struct {
	http *http.Client
}

// NewBilling 创建 billing 客户端。
func NewBilling() *Billing {
	return &Billing{http: &http.Client{Timeout: 30 * time.Second}}
}

// billingBase 按账号区域返回 billing 域 base。
func billingBase(acct *types.Account) string {
	if acct.Region == "cn" {
		return billingBaseCN
	}
	return billingBaseIntl
}

// billingHeaders 构造 billing 域请求头：在出站鉴权头基础上追加计费域专用头。
// 依据 linguo BillingHeaders：Authorization Bearer + X-CodeBuddy-Request: 1 +
// Accept-Language（按区域）+ X-User-Id + X-Device-Token（有则带）。
func billingHeaders(acct *types.Account) http.Header {
	h := auth.BuildOutboundHeaders(acct, "")
	h.Set("Accept", "application/json")
	h.Set("Content-Type", "application/json")
	h.Set("X-CodeBuddy-Request", "1") // 官方客户端风控闸门头，billing 域必带
	if acct.Region == "cn" {
		h.Set("Accept-Language", "zh-CN,zh;q=0.9")
	} else {
		h.Set("Accept-Language", "en-US,en;q=0.9")
	}
	uid := acct.UID
	if uid == "" {
		uid = acct.ID
	}
	if uid != "" {
		h.Set("X-User-Id", uid)
	}
	return h
}

// meterPaths 按区域返回路径候选序列（intl 404 时 fallback 到 /v2）。
func meterPaths(acct *types.Account, noV2, v2 string) []string {
	if acct.Region == "cn" {
		return []string{v2}
	}
	return []string{noV2, v2}
}

// doBilling 发 billing 域请求并解信封，返回 data 段。
// 仅 404 换候选路径（路径分叉）；其他错误直接返回。
func (b *Billing) doBilling(acct *types.Account, paths []string, body any) (json.RawMessage, error) {
	var rawBody []byte
	if body != nil {
		var err error
		rawBody, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	base := billingBase(acct)
	var lastErr error
	for i, path := range paths {
		var rdr io.Reader
		if rawBody != nil {
			rdr = bytes.NewReader(rawBody)
		}
		req, err := http.NewRequest(http.MethodPost, base+path, rdr)
		if err != nil {
			return nil, err
		}
		req.Header = billingHeaders(acct)
		resp, err := b.http.Do(req)
		if err != nil {
			lastErr = err
			continue // 网络层错误：换路径重试一次也无妨，属瞬时错误范畴
		}
		data, berr := parseBillingResp(resp)
		resp.Body.Close()
		if berr == nil {
			return data, nil
		}
		lastErr = berr
		var be *BillingError
		// 只有"路径不存在"值得换候选路径；业务错误/其他 HTTP 错误直接返回。
		if !errors.As(berr, &be) || be.HTTPStatus != http.StatusNotFound || i == len(paths)-1 {
			return nil, berr
		}
	}
	return nil, lastErr
}

// billingEnvelope 上游 billing 域信封。
type billingEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// parseBillingResp 读响应并解信封：HTTP 非 2xx 或 code!=0 即 *BillingError。
func parseBillingResp(resp *http.Response) (json.RawMessage, error) {
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, &BillingError{Code: -1, HTTPStatus: resp.StatusCode, Msg: truncateStr(string(raw), 200)}
	}
	var env billingEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("上游响应解析失败: %w", err)
	}
	if env.Code != 0 {
		return nil, &BillingError{Code: env.Code, HTTPStatus: resp.StatusCode, Msg: truncateStr(env.Msg, 200)}
	}
	return env.Data, nil
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// isTransientBilling 报告 err 是否值得重试：5xx 或网络层错误；
// 业务错误（code!=0 的已签到/参数错等）不重试。
func isTransientBilling(err error) bool {
	if err == nil {
		return false
	}
	var be *BillingError
	if errors.As(err, &be) {
		return be.HTTPStatus >= 500 && be.HTTPStatus < 600
	}
	return true // 非 BillingError 视为传输层错误
}

// retryBilling 对低频维护调用做有界重试：最多补 2 次（间隔 2s、4s）。
func (b *Billing) retryBilling(fn func() error) error {
	err := fn()
	if err == nil || !isTransientBilling(err) {
		return err
	}
	for i := 1; i <= 2; i++ {
		time.Sleep(time.Duration(i) * 2 * time.Second)
		if err = fn(); err == nil || !isTransientBilling(err) {
			return err
		}
	}
	return err
}

// resourceAccount 上游资源包单条目（Response.Data.Accounts[]）。
type resourceAccount struct {
	PackageName         string `json:"PackageName"`
	CycleEndTime        string `json:"CycleEndTime"` // "2006-01-02 15:04:05"，缺省/空=无到期
	CapacitySize        int64  `json:"CapacitySize"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
}

// packageRemainUsed 聚合单套餐 remain/used/size。
// 依据 linguo packageRemainUsed：Cycle 期套餐优先用 CycleCapacity 三字段，
// remain 钳 [0,size]；否则回退 Capacity 三字段。
func packageRemainUsed(a resourceAccount) (remain, used, size int64) {
	if a.CycleCapacitySize > 0 {
		remain = a.CycleCapacityRemain
		size = a.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if a.CycleCapacityUsed > used {
			used = a.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = a.CapacityRemain
	used = a.CapacityUsed
	size = a.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

// cst 是上游套餐到期时间的墙钟时区（UTC+8）。
var cst = time.FixedZone("CST", 8*3600)

// parsePackageEndTime 解析套餐到期时间；空值/格式异常返回 false，
// 调用方保守地不把该包计入最早到期。
func parsePackageEndTime(raw string) (time.Time, bool) {
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation(packageEndLayout, raw, cst)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// FetchBalance 查询账号剩余额度。
// 返回：remain（剩余积分）、total（总额度）、earliest（最早到期时刻，无有效批次时为零值）。
// 依据 linguo UserResourceDetailedWithExpiry（企业号分流暂不支持，返回错误说明）。
func (b *Billing) FetchBalance(acct *types.Account) (remain, total int64, earliest time.Time, err error) {
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}
	var data json.RawMessage
	derr := b.retryBilling(func() error {
		var e error
		data, e = b.doBilling(acct, meterPaths(acct, billingResPath, billingResPathV2), body)
		return e
	})
	if derr != nil {
		return 0, 0, time.Time{}, derr
	}
	var resp struct {
		Response struct {
			Data struct {
				Accounts []resourceAccount `json:"Accounts"`
			} `json:"Data"`
		} `json:"Response"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return 0, 0, time.Time{}, fmt.Errorf("余额响应解析失败: %w", err)
	}
	remain, total, earliest = aggregateBalance(resp.Response.Data.Accounts, now)
	return remain, total, earliest, nil
}

// aggregateBalance 聚合资源包列表：总额度/剩余额度/最早到期时刻（纯函数，可单测）。
func aggregateBalance(accounts []resourceAccount, now time.Time) (remain, total int64, earliest time.Time) {
	for _, a := range accounts {
		r, _, size := packageRemainUsed(a)
		if r < 0 {
			r = 0
		}
		if size < r {
			size = r
		}
		remain += r
		total += size
		if r <= 0 {
			continue
		}
		end, ok := parsePackageEndTime(a.CycleEndTime)
		if !ok || !end.After(now) {
			continue
		}
		if earliest.IsZero() || end.Before(earliest) {
			earliest = end
		}
	}
	return remain, total, earliest
}

// DailyCheckin 执行每日签到。
// 非 cn 区域返回 ErrCheckinUnsupported（国际版无签到体系，hub 口径）；
// 今日已签到返回 ErrAlreadyCheckedIn（幂等成功，调用方按成功处理）。
func (b *Billing) DailyCheckin(acct *types.Account) error {
	if acct.Region != "cn" {
		return ErrCheckinUnsupported
	}
	err := b.retryBilling(func() error {
		_, e := b.doBilling(acct, meterPaths(acct, checkinPath, checkinPathV2), map[string]any{})
		return e
	})
	if err == nil {
		return nil
	}
	var be *BillingError
	if errors.As(err, &be) && be.AlreadyCheckedIn() {
		return ErrAlreadyCheckedIn
	}
	return err
}
