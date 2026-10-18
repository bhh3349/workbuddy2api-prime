package scheduler

import (
	"context"
	"fmt"
	"time"

	"workbuddy2api-prime/internal/types"
	"workbuddy2api-prime/internal/upstream"
)

// TaskModelDir 是模型目录抓取任务名（每天 03:00 北京时间执行）。
//
// 设计：每天查一次上游并缓存到 data/models.json，面板直接读缓存展示，
// 不用每次打开页面都打上游（linguo 是每次打开面板都实时查，很烧账号）。
const TaskModelDir = "modeldir"

// SetModelDir 注入模型目录抓取客户端与缓存目录（参考 SetBilling 的注入方式）。
// 未注入时 RefreshModelDir 返回错误，定时任务仅记日志跳过。
func (s *Scheduler) SetModelDir(c *upstream.ModelDirClient, cacheDir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.modelDir = c
	s.modelDirCacheDir = cacheDir
}

// pickModelDirAccount 选抓取用账号：cn 优先，否则取第一个可用账号；
// 无可用账号返回 nil。
func (s *Scheduler) pickModelDirAccount() *types.Account {
	var fallback *types.Account
	for _, acc := range s.accounts() {
		if acc == nil || acc.Disabled {
			continue
		}
		if fallback == nil {
			fallback = acc
		}
		if acc.Region == "cn" {
			return acc
		}
	}
	return fallback
}

// RefreshModelDir 立刻抓取一次模型目录并写缓存，返回模型条数。
// 定时任务与面板"立即刷新"共用同一函数。
func (s *Scheduler) RefreshModelDir(_ context.Context) (int, error) {
	s.mu.Lock()
	client, dir := s.modelDir, s.modelDirCacheDir
	s.mu.Unlock()
	if client == nil {
		return 0, fmt.Errorf("模型目录客户端未初始化")
	}
	acc := s.pickModelDirAccount()
	if acc == nil {
		return 0, fmt.Errorf("没有可用账号")
	}
	entries, err := client.FetchModelDirectory(acc)
	if err != nil {
		return 0, err
	}
	if err := upstream.SaveCache(dir, entries); err != nil {
		return 0, fmt.Errorf("缓存落盘失败: %w", err)
	}
	return len(entries), nil
}

// ModelDirSnapshot 读模型目录缓存（面板展示用，只读不触发上游请求）。
func (s *Scheduler) ModelDirSnapshot() ([]upstream.ModelEntry, time.Time, error) {
	s.mu.Lock()
	dir := s.modelDirCacheDir
	s.mu.Unlock()
	return upstream.LoadCache(dir)
}

// runModelDir 是定时任务入口：抓取失败只记日志不抛错（下次继续），
// 执行结果记入 History（面板签到历史可见）。
func (s *Scheduler) runModelDir(ctx context.Context) error {
	n, err := s.RefreshModelDir(ctx)
	res := types.CheckinResult{Time: time.Now(), AccountID: TaskModelDir}
	if err != nil {
		res.Detail = "模型目录抓取失败: " + err.Error()
		s.logf("scheduler: %s", res.Detail)
	} else {
		res.OK = true
		res.Detail = fmt.Sprintf("模型目录已更新：%d 个模型", n)
		s.logf("scheduler: %s", res.Detail)
	}
	s.RecordCheckin(res)
	return nil
}
