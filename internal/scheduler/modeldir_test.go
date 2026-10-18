package scheduler

import (
	"context"
	"testing"
	"time"

	"workbuddy2api-prime/internal/types"
)

// newTestScheduler 建一个带内存账号的调度器（不启动排程循环）。
func newTestScheduler(t *testing.T, accts []*types.Account) *Scheduler {
	s, err := New(func() []*types.Account { return accts }, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestModelDirRegistered 确认 modeldir 任务已注册且排程为每天 03:00（北京时间）。
func TestModelDirRegistered(t *testing.T) {
	s := newTestScheduler(t, nil)
	s.RegisterBuiltinTasks()
	s.mu.Lock()
	e, ok := s.tasks[TaskModelDir]
	s.mu.Unlock()
	if !ok {
		t.Fatal("modeldir 任务未注册")
	}
	// 03:00 之后 → 下一次是次日 03:00
	loc := BeijingTime()
	after := time.Date(2026, 10, 17, 4, 0, 0, 0, loc)
	got := e.schedule.Next(after)
	want := time.Date(2026, 10, 18, 3, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("下次触发=%v, want %v", got, want)
	}
}

// TestPickModelDirAccount 验证选号：cn 优先、跳过停用、无可用返回 nil。
func TestPickModelDirAccount(t *testing.T) {
	mk := func(id, region string, disabled bool) *types.Account {
		return &types.Account{ID: id, Region: region, Disabled: disabled}
	}
	s := newTestScheduler(t, []*types.Account{
		mk("intl-1", "intl", false),
		mk("cn-off", "cn", true),
		mk("cn-1", "cn", false),
	})
	if got := s.pickModelDirAccount(); got == nil || got.ID != "cn-1" {
		t.Fatalf("应选中 cn-1，got %+v", got)
	}
	// 全停用 → nil
	s2 := newTestScheduler(t, []*types.Account{mk("x", "cn", true)})
	if got := s2.pickModelDirAccount(); got != nil {
		t.Fatalf("全停用应返回 nil，got %+v", got)
	}
	// 无 cn → 取第一个可用
	s3 := newTestScheduler(t, []*types.Account{mk("i1", "intl", false), mk("i2", "intl", false)})
	if got := s3.pickModelDirAccount(); got == nil || got.ID != "i1" {
		t.Fatalf("应选中 i1，got %+v", got)
	}
}

// TestRefreshModelDirNoClient 未注入客户端时返回错误（不 panic）。
func TestRefreshModelDirNoClient(t *testing.T) {
	s := newTestScheduler(t, []*types.Account{{ID: "a", Region: "cn"}})
	if _, err := s.RefreshModelDir(context.Background()); err == nil {
		t.Fatal("未注入客户端应返回错误")
	}
}

// TestRunModelDirRecordsHistory 失败也只记日志+历史，不抛错。
func TestRunModelDirRecordsHistory(t *testing.T) {
	s := newTestScheduler(t, []*types.Account{{ID: "a", Region: "cn"}})
	// 未注入客户端 → 抓取失败
	if err := s.runModelDir(context.Background()); err != nil {
		t.Fatalf("runModelDir 不应抛错: %v", err)
	}
	h := s.History()
	if len(h) == 0 {
		t.Fatal("失败结果应记入 History")
	}
	if h[0].AccountID != TaskModelDir || h[0].OK {
		t.Fatalf("历史记录异常: %+v", h[0])
	}
}
