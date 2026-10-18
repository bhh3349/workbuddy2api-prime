package scheduler

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"workbuddy2api-prime/internal/auth"
	"workbuddy2api-prime/internal/types"
	"workbuddy2api-prime/internal/upstream"
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

// checkinOne 对单个账号执行签到（定时任务内部用），语义与 CheckinAccount 一致。
func (s *Scheduler) checkinOne(ctx context.Context, acc *types.Account) types.CheckinResult {
	return s.CheckinAccount(ctx, acc)
}

// CheckinAccount 对单个账号执行签到并刷新余额（面板手动签到复用同一逻辑）。
//
// 流程（对齐 linguo panel accountCheckin）：
//  1. 国际版/企业号无签到体系 → 跳过签到，只刷新余额；
//  2. 调上游 daily-checkin，"今日已签到"视为幂等成功；
//  3. 无论签到成败都查一次余额并写回账号（CreditTotal/CreditExpiresAt），
//     已签到等业务错误不阻塞余额刷新。
func (s *Scheduler) CheckinAccount(_ context.Context, acc *types.Account) types.CheckinResult {
	res := types.CheckinResult{Time: time.Now(), AccountID: acc.ID}
	if acc.Disabled {
		res.Detail = "账号已停用，跳过"
		return res
	}
	s.mu.Lock()
	billing, saveAcc := s.billing, s.saveAcc
	s.mu.Unlock()
	if billing == nil {
		res.Detail = "签到客户端未初始化"
		return res
	}

	checkinMsg := ""
	switch err := billing.DailyCheckin(acc); {
	case err == nil:
		checkinMsg = "签到成功"
	case errors.Is(err, upstream.ErrAlreadyCheckedIn):
		checkinMsg = "今日已签到"
	case errors.Is(err, upstream.ErrCheckinUnsupported):
		checkinMsg = "无签到体系（仅刷新余额）"
	default:
		checkinMsg = "签到失败: " + err.Error()
	}

	// 余额刷新：签到业务错误不阻塞。
	remain, _, earliest, berr := billing.FetchBalance(acc)
	if berr != nil {
		res.Detail = joinDetail(checkinMsg, "余额刷新失败: "+berr.Error())
		return res
	}
	acc.CreditTotal = float64(remain)
	if !earliest.IsZero() {
		acc.CreditExpiresAt = earliest
	}
	acc.UpdatedAt = time.Now()
	if saveAcc != nil {
		if err := saveAcc(acc); err != nil {
			s.logf("scheduler: 账号 %s 余额落盘失败: %v", acc.ID, err)
		}
	}
	res.OK = true // 余额刷新成功即记成功（余额为准）；签到失败原因保留在明细里
	res.Detail = joinDetail(checkinMsg, fmt.Sprintf("剩余额度 %.0f", float64(remain)))
	return res
}

// joinDetail 拼接签到与余额两段明细。
func joinDetail(a, b string) string {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "；" + b
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
