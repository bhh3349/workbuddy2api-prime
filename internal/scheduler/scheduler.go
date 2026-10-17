// Package scheduler 提供网关的定时任务能力：签到、token 保活、成长任务等。
//
// 设计融合三家优点：
//   - hub（ardeyouxipianyi）：单线程轮询 + 任务重叠保护（_run_lock）+ 手动触发 trigger_now
//   - linguo：多类独立排程、独立开关、可运行期重配
//   - HanawaBanana：触发时刻抖动、错过窗口手动补跑、任务台账
//
// 全部只用标准库。
package scheduler

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"workbuddy2api-prime/internal/types"
)

// TaskFunc 是单个定时任务的执行函数。ctx 在 Scheduler.Stop() 时取消，
// 长耗时任务应监听 ctx.Done() 及时退出。
type TaskFunc func(ctx context.Context) error

// Schedule 描述任务的触发时刻表。
type Schedule interface {
	// Next 返回 after 之后最近的一次触发时刻（不含 after 本身）。
	Next(after time.Time) time.Time
	String() string
}

// dayTime 是一天中的时刻（时:分）。
type dayTime struct {
	hour, min int
}

// dailyAt 是"每天固定时刻"调度。
type dailyAt struct {
	times []dayTime // 已按从早到晚排序
	loc   *time.Location
}

// DailyAt 创建每天在给定时刻（"HH:MM"，24 小时制）触发的调度。
// loc 为空时使用北京时间。
func DailyAt(loc *time.Location, hhmm ...string) (Schedule, error) {
	if len(hhmm) == 0 {
		return nil, fmt.Errorf("scheduler: DailyAt 至少需要一个时刻")
	}
	if loc == nil {
		loc = BeijingTime()
	}
	times := make([]dayTime, 0, len(hhmm))
	for _, s := range hhmm {
		var h, m int
		if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
			return nil, fmt.Errorf("scheduler: 非法时刻 %q，应为 HH:MM", s)
		}
		times = append(times, dayTime{h, m})
	}
	sort.Slice(times, func(i, j int) bool {
		if times[i].hour != times[j].hour {
			return times[i].hour < times[j].hour
		}
		return times[i].min < times[j].min
	})
	return &dailyAt{times: times, loc: loc}, nil
}

// MustDailyAt 是 DailyAt 的 panic 版本，用于内置任务注册（时刻写死、必合法）。
func MustDailyAt(hhmm ...string) Schedule {
	s, err := DailyAt(nil, hhmm...)
	if err != nil {
		panic(err)
	}
	return s
}

func (d *dailyAt) Next(after time.Time) time.Time {
	y, m, day := after.In(d.loc).Date()
	for _, t := range d.times {
		cand := time.Date(y, m, day, t.hour, t.min, 0, 0, d.loc)
		if cand.After(after) {
			return cand
		}
	}
	// 当天已过，取明天第一个时刻
	t := d.times[0]
	return time.Date(y, m, day+1, t.hour, t.min, 0, 0, d.loc)
}

func (d *dailyAt) String() string {
	ss := make([]string, 0, len(d.times))
	for _, t := range d.times {
		ss = append(ss, fmt.Sprintf("%02d:%02d", t.hour, t.min))
	}
	return "每天 " + fmt.Sprint(ss)
}

// interval 是"固定间隔"调度。
type interval time.Duration

// Every 创建每隔 d 触发一次的调度。
func Every(d time.Duration) Schedule { return interval(d) }

func (i interval) Next(after time.Time) time.Time { return after.Add(time.Duration(i)) }

func (i interval) String() string { return "每隔 " + time.Duration(i).String() }

// BeijingTime 返回北京时间 location；系统无 tzdata 时回退到固定 UTC+8。
// 三家项目的定时任务默认都按北京时间（CST）排程。
func BeijingTime() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
}

// taskEntry 是一个已注册任务的运行态。
type taskEntry struct {
	name     string
	schedule Schedule
	fn       TaskFunc

	mu      sync.Mutex
	running bool // 上一轮是否仍在执行（重叠保护）
}

// runOnce 执行一轮；上一轮未结束时跳过本轮（hub _run_lock 思路）。
func (e *taskEntry) runOnce(ctx context.Context, logf func(string, ...any)) (skipped bool) {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		logf("scheduler: 任务 %s 上一轮仍在执行，跳过本轮触发", e.name)
		return true
	}
	e.running = true
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
	}()

	start := time.Now()
	if err := e.fn(ctx); err != nil {
		logf("scheduler: 任务 %s 执行失败（耗时 %s）: %v", e.name, time.Since(start).Round(time.Millisecond), err)
		return false
	}
	logf("scheduler: 任务 %s 执行完成（耗时 %s）", e.name, time.Since(start).Round(time.Millisecond))
	return false
}

// Scheduler 是定时任务调度器。
type Scheduler struct {
	mu       sync.Mutex
	tasks    map[string]*taskEntry
	accounts func() []*types.Account // 账号快照提供者（由 main.go 注入，避免循环依赖）
	store    *CheckinStore            // 签到结果落盘
	logf     func(string, ...any)

	running   bool
	runCtx    context.Context
	runCancel context.CancelFunc
	wg        sync.WaitGroup
}

// New 创建调度器。accounts 提供当前账号快照；dataDir 为数据目录（签到落盘用）。
// logf 为空时使用 log.Printf。
func New(accounts func() []*types.Account, dataDir string, logf func(string, ...any)) (*Scheduler, error) {
	store, err := NewCheckinStore(dataDir)
	if err != nil {
		return nil, err
	}
	if logf == nil {
		logf = log.Printf
	}
	return &Scheduler{
		tasks:    make(map[string]*taskEntry),
		accounts: accounts,
		store:    store,
		logf:     logf,
	}, nil
}

// Register 注册一个任务；同名重复注册返回错误。
func (s *Scheduler) Register(name string, sched Schedule, fn TaskFunc) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.tasks[name]; ok {
		return fmt.Errorf("scheduler: 任务 %q 已注册", name)
	}
	s.tasks[name] = &taskEntry{name: name, schedule: sched, fn: fn}
	return nil
}

// Unregister 移除一个任务；运行中调用只影响下一次触发判定（当前轮不受影响）。
func (s *Scheduler) Unregister(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasks, name)
}

// Start 启动所有已注册任务的独立排程循环。重复调用无副作用。
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return
	}
	s.running = true
	s.runCtx, s.runCancel = context.WithCancel(ctx)
	entries := make([]*taskEntry, 0, len(s.tasks))
	for _, e := range s.tasks {
		entries = append(entries, e)
	}
	s.mu.Unlock()

	for _, e := range entries {
		s.wg.Add(1)
		go s.loop(e)
	}
	s.logf("scheduler: 已启动 %d 个定时任务", len(entries))
}

// Stop 停止调度器，等待正在执行的任务轮次结束。
func (s *Scheduler) Stop() {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	cancel := s.runCancel
	s.mu.Unlock()

	cancel()
	s.wg.Wait()
	s.logf("scheduler: 已停止")
}

// loop 是单个任务的排程循环。
func (s *Scheduler) loop(e *taskEntry) {
	defer s.wg.Done()
	for {
		next := e.schedule.Next(time.Now())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-s.runCtx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		// 任务被中途反注册则跳过
		s.mu.Lock()
		_, ok := s.tasks[e.name]
		s.mu.Unlock()
		if !ok {
			return
		}
		e.runOnce(s.runCtx, s.logf)
	}
}

// TriggerNow 手动触发一次任务（异步执行，参考 hub trigger_now /
// HanawaBanana 的 POST /admin/tasks/{name}/run）。任务不存在或正在执行中返回错误。
func (s *Scheduler) TriggerNow(name string) error {
	s.mu.Lock()
	e, ok := s.tasks[name]
	ctx := s.runCtx
	running := s.running
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("scheduler: 任务 %q 未注册", name)
	}
	if !running {
		return fmt.Errorf("scheduler: 调度器未启动")
	}
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return fmt.Errorf("scheduler: 任务 %q 正在执行中", name)
	}
	e.mu.Unlock()
	go e.runOnce(ctx, s.logf)
	return nil
}

// History 返回签到历史（按时间倒序，只读，不触发任何上游请求）。
func (s *Scheduler) History() []types.CheckinResult {
	return s.store.History()
}
