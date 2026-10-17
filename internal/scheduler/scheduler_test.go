package scheduler

import (
	"context"
	"testing"
	"time"

	"workbuddy2api-prime/internal/types"
)

func TestDailyAtNext(t *testing.T) {
	loc := BeijingTime()
	s, err := DailyAt(loc, "21:00", "09:00") // 乱序输入，应自动排序
	if err != nil {
		t.Fatal(err)
	}
	// 当天 08:00 → 当天 09:00
	after := time.Date(2026, 10, 17, 8, 0, 0, 0, loc)
	got := s.Next(after)
	want := time.Date(2026, 10, 17, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// 当天 09:00 整 → 下一个是 21:00（不含 after 本身）
	got = s.Next(want)
	want = time.Date(2026, 10, 17, 21, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// 当天 22:00 → 次日 09:00
	after = time.Date(2026, 10, 17, 22, 0, 0, 0, loc)
	got = s.Next(after)
	want = time.Date(2026, 10, 18, 9, 0, 0, 0, loc)
	if !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDailyAtInvalid(t *testing.T) {
	if _, err := DailyAt(nil, "25:00"); err == nil {
		t.Fatal("非法时刻应返回错误")
	}
	if _, err := DailyAt(nil); err == nil {
		t.Fatal("空时刻应返回错误")
	}
}

func TestEveryNext(t *testing.T) {
	s := Every(5 * time.Minute)
	after := time.Now()
	got := s.Next(after)
	if got.Sub(after) != 5*time.Minute {
		t.Fatalf("got %v", got.Sub(after))
	}
}

func TestCheckinStorePruneAndHistory(t *testing.T) {
	dir := t.TempDir()
	cs, err := NewCheckinStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	// 写入 3 条：一条超 7 天（应被裁掉）、两条有效
	_ = cs.Append(types.CheckinResult{Time: now.Add(-8 * 24 * time.Hour), AccountID: "old", OK: true})
	_ = cs.Append(types.CheckinResult{Time: now.Add(-time.Hour), AccountID: "a1", OK: true})
	_ = cs.Append(types.CheckinResult{Time: now, AccountID: "a2", OK: false, Detail: "x"})

	h := cs.History()
	if len(h) != 2 {
		t.Fatalf("裁剪后应剩 2 条，实际 %d", len(h))
	}
	if h[0].AccountID != "a2" || h[1].AccountID != "a1" {
		t.Fatalf("History 应按时间倒序，实际 %+v", h)
	}

	// 重新加载，应从磁盘恢复
	cs2, err := NewCheckinStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs2.History()) != 2 {
		t.Fatalf("重载后应剩 2 条，实际 %d", len(cs2.History()))
	}
}

func TestSchedulerRegisterDuplicate(t *testing.T) {
	s, err := New(nil, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	noop := func(ctx context.Context) error { return nil }
	if err := s.Register("t1", Every(time.Hour), noop); err != nil {
		t.Fatal(err)
	}
	if err := s.Register("t1", Every(time.Hour), noop); err == nil {
		t.Fatal("重复注册同名任务应返回错误")
	}
	if err := s.TriggerNow("nope"); err == nil {
		t.Fatal("触发未注册任务应返回错误")
	}
	s.Unregister("t1")
	if err := s.TriggerNow("t1"); err == nil {
		t.Fatal("反注册后触发应返回错误")
	}
}
