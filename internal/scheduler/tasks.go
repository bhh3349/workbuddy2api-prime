package scheduler

import (
	"context"
	"math/rand/v2"
	"time"

	"workbuddy2api-prime/internal/auth"
	"workbuddy2api-prime/internal/types"
)

// 内置任务名。
const (
	TaskCheckin   = "checkin"   // 签到（09:00 / 21:00）
	TaskKeepalive = "keepalive" // token 保活（22:00）
	TaskGrowth    = "growth"    // 成长任务（01:00）
)

// RegisterBuiltinTasks 注册三个内置任务（默认按北京时间排程）。
// 调用方可在 Start 之前用 Unregister 移除或重新 Register 自定义版本。
func (s *Scheduler) RegisterBuiltinTasks() {
	_ = s.Register(TaskCheckin, MustDailyAt("09:00", "21:00"), s.runCheckin)
	_ = s.Register(TaskKeepalive, MustDailyAt("22:00"), s.runKeepalive)
	_ = s.Register(TaskGrowth, MustDailyAt("01:00"), s.runGrowth)
}

// runCheckin 执行全账号签到。
// 账号间 2–5 秒随机错峰（hub v1.7.12 issue #394）：防同一出口 IP 多账号
// 同秒并发触发上游频率风控；每轮内子任务后还有 1 秒固定间隔。
func (s *Scheduler) runCheckin(ctx context.Context) error {
	accounts := s.accounts()
	for i, acc := range accounts {
		if acc == nil || acc.Disabled {
			continue
		}
		if i > 0 {
			if !sleepJitter(ctx, 2*time.Second, 5*time.Second) {
				return context.Canceled
			}
		}
		res := s.checkinOne(ctx, acc)
		if err := s.store.Append(res); err != nil {
			s.logf("scheduler: 签到结果落盘失败（账号 %s）: %v", acc.ID, err)
		}
		// 子任务后固定 1 秒间隔（hub 做法）
		if !sleepJitter(ctx, time.Second, time.Second) {
			return context.Canceled
		}
	}
	return nil
}

// checkinOne 对单个账号执行签到。
//
// TODO: 接入真实签到接口。参考实现：
//   - linguo:  ~/workspace/workbuddy2api-analysis/repos/workbuddy2api-panel/internal/panel/taskcenter.go
//     （签到 + 余额查询；余额恢复后应解冻 pool 中的冷却账号）
//   - hub:    ~/workspace/workbuddy2api-analysis/repos/workbuddy2api-hub/wb_accounts.py::Account.checkin()
//     （国内版签到；国际版对应 daily_chat()，按日幂等）
//
// 注意：global（国际版）账号无签到体系，应跳过（参考 linguo：global 跳过签到/成长）。
func (s *Scheduler) checkinOne(_ context.Context, acc *types.Account) types.CheckinResult {
	return types.CheckinResult{
		Time:      time.Now(),
		AccountID: acc.ID,
		OK:        false,
		Detail:    "TODO: 上游签到接口未实现",
	}
}

// runKeepalive 集中刷新 token。
// 只刷新剩余寿命不足的账号，且全程串行：上游刷新时会轮换 refresh token，
// 并发刷新会互相作废（hub 3.7 节教训）。
func (s *Scheduler) runKeepalive(_ context.Context) error {
	for _, acc := range s.accounts() {
		if acc == nil || acc.Disabled {
			continue
		}
		if !auth.NeedsRefresh(acc) {
			continue
		}
		refreshed, err := auth.RefreshIfNeeded(acc)
		if err != nil {
			s.logf("scheduler: 账号 %s token 刷新失败: %v", acc.ID, err)
			continue
		}
		if refreshed {
			s.logf("scheduler: 账号 %s token 已刷新", acc.ID)
		}
	}
	return nil
}

// runGrowth 执行成长任务队列（每日 01:00，避开零点任务解锁竞态，linguo 做法）。
//
// TODO: 接入真实成长任务引擎。参考实现：
//   - linguo: ~/workspace/workbuddy2api-analysis/repos/workbuddy2api-panel/internal/panel/taskcenter.go:296
//     RunGrowthQueueOnce（任务拉取→批量接取→逐个点亮→领奖；17/18 可自动完成）
//   - hub:    ~/workspace/workbuddy2api-analysis/repos/workbuddy2api-hub/wb_tasks.py::run_growth_tasks()
//     （accept_tasks 批量接取 chunk=20 → _progress_one 逐个点亮 → claim_task 领奖；
//     跳过 unforgeable 任务与 DESKTOP_ONLY_TASKS）
//
// 注意：global（国际版）账号与企业号（enterpriseId 非空）跳过个人成长体系。
func (s *Scheduler) runGrowth(_ context.Context) error {
	s.logf("scheduler: 成长任务引擎尚未实现（TODO），本轮跳过")
	return nil
}

// sleepJitter 睡眠 [min, max] 区间内的随机时长；ctx 取消时返回 false。
func sleepJitter(ctx context.Context, min, max time.Duration) bool {
	if max < min {
		max = min
	}
	d := min
	if max > min {
		d += time.Duration(rand.Float64() * float64(max-min))
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
