// Package billing 用量记账：逐请求追加写 usage/usage.jsonl，查询时重读聚合。
//
// 移植自 ardeyouxipianyi/workbuddy2api-hub 的记账思路（见分析文档 §4.1 用量账本）：
//   - 一行一请求，追加写盘；请求路径不维护累计值，聚合全部在查询时重读计算
//   - credit 为上游上报的该请求实际消耗积分（不是剩余积分），计费是读时计算
//   - 失败请求也记行（不丢样本），用 OK 字段区分
package billing

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"workbuddy2api-prime/internal/types"
)

// ledgerFileName 是账本文件名，放在 <dir>/usage/ 下。
const ledgerFileName = "usage.jsonl"

// Summary 是 Query 的聚合结果。
type Summary struct {
	Requests         int     // 总请求数（含失败请求，hub 思路：不丢样本）
	PromptTokens     int     // 输入 token 合计
	CompletionTokens int     // 输出 token 合计
	TotalCredit      float64 // 上游上报的实际消耗积分合计
}

// Ledger 是用量账本。并发安全。
type Ledger struct {
	mu   sync.Mutex
	path string
}

// NewLedger 创建账本，在 dir 下建 usage/ 目录。
// 目录权限取 0700（目录去掉执行位后进程自身无法进入，参考 auth 模块的折中方案），
// 账本文件 0600。
func NewLedger(dir string) (*Ledger, error) {
	usageDir := filepath.Join(dir, "usage")
	if err := os.MkdirAll(usageDir, 0o700); err != nil {
		return nil, fmt.Errorf("billing: 创建 usage 目录失败: %w", err)
	}
	// 纠正已存在目录的权限（umask 可能放宽过）
	if err := os.Chmod(usageDir, 0o700); err != nil {
		return nil, fmt.Errorf("billing: 设置 usage 目录权限失败: %w", err)
	}
	return &Ledger{path: filepath.Join(usageDir, ledgerFileName)}, nil
}

// Record 追加写一条用量记录。每次打开-追加-关闭，崩溃不丢已写数据。
func (l *Ledger) Record(r types.UsageRecord) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	data, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("billing: 序列化用量记录失败: %w", err)
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("billing: 打开账本文件失败: %w", err)
	}
	defer f.Close()
	if err := os.Chmod(l.path, 0o600); err != nil {
		return fmt.Errorf("billing: 设置账本文件权限失败: %w", err)
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("billing: 写入账本失败: %w", err)
	}
	return nil
}

// Bucket 是时间序列的一个桶：桶起始时间 + 该桶内的请求数与消耗积分。
type Bucket struct {
	Time      time.Time `json:"time"`
	Requests  int       `json:"requests"`
	Credit    float64   `json:"credit"`
	PromptTok int       `json:"prompt_tokens"` // 输入 token 合计（供面板堆叠图）
	CompTok   int       `json:"completion_tokens"`
}

// BucketSeries 按时间把 [since, now] 切成 buckets 个等宽桶，返回每桶聚合。
// 用于面板手写 SVG 趋势图（x 轴真实时间轴）。buckets<=0 时默认 24。
func (l *Ledger) BucketSeries(apiKeyID string, since time.Time, buckets int) ([]Bucket, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if buckets <= 0 {
		buckets = 24
	}
	now := time.Now()
	if since.IsZero() || !since.Before(now) {
		since = now.Add(-24 * time.Hour)
	}
	out := make([]Bucket, buckets)
	width := now.Sub(since) / time.Duration(buckets)
	for i := range out {
		out[i].Time = since.Add(time.Duration(i) * width)
	}

	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil // 还没记过账，返回全零序列而非报错
		}
		return nil, fmt.Errorf("billing: 打开账本文件失败: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var r types.UsageRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		if apiKeyID != "" && r.APIKeyID != apiKeyID {
			continue
		}
		if r.Time.Before(since) || !r.Time.Before(now) {
			continue
		}
		idx := int(r.Time.Sub(since) / width)
		if idx >= buckets {
			idx = buckets - 1
		}
		out[idx].Requests++
		out[idx].Credit += r.Credit
		out[idx].PromptTok += r.PromptTok
		out[idx].CompTok += r.CompTok
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("billing: 读取账本失败: %w", err)
	}
	return out, nil
}

// Recent 返回最新的 limit 条用量记录（新→旧）。用于面板"最近请求"。
func (l *Ledger) Recent(apiKeyID string, limit int) ([]types.UsageRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if limit <= 0 {
		limit = 50
	}
	out := []types.UsageRecord{}
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return out, nil
		}
		return nil, fmt.Errorf("billing: 打开账本文件失败: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var r types.UsageRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		if apiKeyID != "" && r.APIKeyID != apiKeyID {
			continue
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("billing: 读取账本失败: %w", err)
	}
	// 取最后 limit 条并反转为新→旧
	if len(out) > limit {
		out = out[len(out)-limit:]
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// Query 按 API Key 与起始时间聚合用量。
// apiKeyID 为空表示统计全部 key；since 为零值表示不限起始时间。
func (l *Ledger) Query(apiKeyID string, since time.Time) (Summary, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var s Summary
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return s, nil // 还没记过账，返回空汇总而非报错
		}
		return s, fmt.Errorf("billing: 打开账本文件失败: %w", err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024) // 单行上限 1MB
	for sc.Scan() {
		var r types.UsageRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue // 坏行跳过，个别脏数据不影响整体查询
		}
		if apiKeyID != "" && r.APIKeyID != apiKeyID {
			continue
		}
		if !since.IsZero() && r.Time.Before(since) {
			continue
		}
		s.Requests++
		s.PromptTokens += r.PromptTok
		s.CompletionTokens += r.CompTok
		s.TotalCredit += r.Credit
	}
	if err := sc.Err(); err != nil {
		return s, fmt.Errorf("billing: 读取账本失败: %w", err)
	}
	return s, nil
}

// Detail 是带维度拆分的用量明细。
type Detail struct {
	Summary
	Errors    int                `json:"errors"`     // 失败请求数
	ByModel   map[string]Summary `json:"by_model"`   // 按模型拆分
	ByAccount map[string]Summary `json:"by_account"` // 按账号拆分
}

// QueryDetail 返回带维度拆分的用量明细（与 Query 同样的过滤语义）。
func (l *Ledger) QueryDetail(apiKeyID string, since time.Time) (Detail, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var d Detail
	d.ByModel = map[string]Summary{}
	d.ByAccount = map[string]Summary{}
	f, err := os.Open(l.path)
	if err != nil {
		if os.IsNotExist(err) {
			return d, nil
		}
		return d, fmt.Errorf("billing: 打开账本文件失败: %w", err)
	}
	defer f.Close()

	add := func(s Summary, r types.UsageRecord) Summary {
		s.Requests++
		s.PromptTokens += r.PromptTok
		s.CompletionTokens += r.CompTok
		s.TotalCredit += r.Credit
		return s
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var r types.UsageRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			continue
		}
		if apiKeyID != "" && r.APIKeyID != apiKeyID {
			continue
		}
		if !since.IsZero() && r.Time.Before(since) {
			continue
		}
		d.Summary = add(d.Summary, r)
		if !r.OK {
			d.Errors++
		}
		if r.Model != "" {
			d.ByModel[r.Model] = add(d.ByModel[r.Model], r)
		}
		if r.AccountID != "" {
			d.ByAccount[r.AccountID] = add(d.ByAccount[r.AccountID], r)
		}
	}
	if err := sc.Err(); err != nil {
		return d, fmt.Errorf("billing: 读取账本失败: %w", err)
	}
	return d, nil
}
