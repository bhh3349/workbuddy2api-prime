// Package scheduler 的成长任务引擎：做任务领积分（代替 runGrowth 的 TODO 空壳）。
//
// 移植自 linguo workbuddy2api-panel（internal/panel/taskcenter.go 的队列管线 +
// internal/panel/autotask.go 的 24 个自动动作），auth.Auth 替换为 types.Account。
//
// 管线：任务拉取（默认口径 + 小程序口径合并去重）→ 批量接受 → 逐个自动完成
// （行为事件上报点亮进度）→ 进度达标自动领奖。
// 账号内串行、账号间 2 秒错峰；跳过停用/global/企业号账号。
// 所有动作幂等：已 claimed/已达标的任务直接跳过。
//
// 与 linguo 的差异：
//   - hub 认为 RichMeow_Chat/Library_read/Buddy_App(_QQ) 需真实桌面操作
//     （DESKTOP_ONLY_TASKS），linguo 则称三账号实测纯 API 可点亮；本实现沿用
//     linguo 的 API 上报做法，失败只记日志不阻塞（两家结论分歧处以实测为准）。
//   - 不做 Expert_Philanthropy（需真实捐款，linguo 亦不做）。
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"workbuddy2api-prime/internal/types"
	"workbuddy2api-prime/internal/upstream"
)

// ---------------------------------------------------------------------------
// 任务动作表
// ---------------------------------------------------------------------------

// autoAction 是一个可自动化的任务动作。
type autoAction struct {
	TaskCode string // 目标任务 code
	Desc     string // 展示用说明
	Attempt  bool   // true = 尝试型（上游未证实可脚本化）
	run      func(e *GrowthEngine, acct *types.Account) (string, error)
}

// autoActions 已实现的任务动作表（顺序即执行顺序：先解锁依赖项）。
var autoActions = []autoAction{
	{TaskCode: "chat_5", Desc: "上报 5 条对话活跃事件（自动补足差额）", run: runChat5},
	{TaskCode: "first_buddy", Desc: "上报解锁 → 同意协议 → 领取第一只 Buddy（+300 分）", run: runFirstBuddy},
	{TaskCode: "Model_chat_GLM5.2", Desc: "接受任务 → glm-5.2 真实对话一次 → 对齐模型上报", run: runModelChat},
	{TaskCode: "RichMeow_Chat", Desc: "桌面指纹事件链上报（纯 API 点亮）", run: runRichMeow},
	{TaskCode: "Buddy_App", Desc: "上报「进入 Buddy 应用」事件链", run: runBuddyApp},
	{TaskCode: "Buddy_App_QQ", Desc: "上报「进入企鹅教师助手」事件链", run: runBuddyApp},
	{TaskCode: "automation_1", Desc: "上报「定时任务创建」事件", run: runAutomationCreate},
	{TaskCode: "Library_read", Desc: "上报「读资料库介绍」事件", run: runLibraryRead},
	{TaskCode: "template_5", Desc: "上报「使用模板创建任务」事件组 ×5", run: runTemplateUse},
	{TaskCode: "playbook_prompt", Desc: "上报「灵感案例做同款发送 Prompt」事件组", run: runPlaybookPrompt},
	{TaskCode: "create_canvas", Desc: "上报「设计创意画布创建」事件组（+300 分）", run: runCreateCanvas},
	{TaskCode: "expert_5", Desc: "真实专家召唤+使用链 ×5", run: runExpertUse},
	{TaskCode: "Expert_team_use_3", Desc: "真实专家团召唤+使用链 ×3", run: runExpertTeamUse},
	{TaskCode: "Hp_Appearance", Desc: "设置主题 API + 皮肤生效事件", run: runAppearance},
	{TaskCode: "skill_1", Desc: "真实对话 + skill_info 技能加载事件", run: runSkillFresh},
	{TaskCode: "Expert_lighthouse", Desc: "真实轻量云专家召唤+使用链", run: runExpertLighthouse},
	{TaskCode: "black_cat", Desc: "夜猫子：23:00–08:00 窗口内 glm-5.2 对话补足", Attempt: true, run: runBlackCat},
	{TaskCode: "school_season", Desc: "校园日（小程序口径）：accept → mini 对话+activityId 上报 → 领奖", run: runSchoolSeason},
	{TaskCode: "Sequential_Tasks_1", Desc: "小程序首对话：accept → mini 对话上报 → 领奖", run: runSequentialChat},
	{TaskCode: "Sequential_Tasks_2", Desc: "小程序选专家对话：市场专家 id → accept → expert_actual_use 上报 → 领奖", run: runMiniExpert},
	{TaskCode: "Sequential_Tasks_3", Desc: "小程序五次对话：accept → mini 对话上报 ×5 → 领奖", run: runSequentialChat5},
	{TaskCode: "Sequential_Tasks_4", Desc: "小程序定时任务：accept → 定时任务创建事件 → 领奖", run: runSequentialAutomation},
	{TaskCode: "Sequential_Tasks_5", Desc: "小程序使用 GLM5.2：accept → 带模型字段的 mini 对话上报 → 领奖", run: runSequentialModelChat},
	{TaskCode: "Sequential_Tasks_6", Desc: "小程序十次对话：accept → mini 对话上报 ×target → 领奖", run: runSequentialChat10},
	{TaskCode: "Sequential_Tasks_7", Desc: "体验灵感功能：accept → 灵感事件组（PC+mp 双形态）→ 领奖", run: runSequentialPlaybook},
}

// autoActionFor 查任务对应的动作；无则返回 nil（不可自动化）。
func autoActionFor(code string) *autoAction {
	for i := range autoActions {
		if autoActions[i].TaskCode == code {
			return &autoActions[i]
		}
	}
	return nil
}

// autoActionIndex 返回动作在表中的顺序（队列排序用，无动作的排最后）。
func autoActionIndex(code string) int {
	for i := range autoActions {
		if autoActions[i].TaskCode == code {
			return i
		}
	}
	return len(autoActions)
}

// mpTaskCodes 小程序口径专属任务（回读/接受/领奖走 mp 变体）。
var mpTaskCodes = map[string]bool{
	"school_season":      true,
	"Sequential_Tasks_1": true,
	"Sequential_Tasks_2": true,
	"Sequential_Tasks_3": true,
	"Sequential_Tasks_4": true,
	"Sequential_Tasks_5": true,
	"Sequential_Tasks_6": true,
	"Sequential_Tasks_7": true,
}

func isMPTaskCode(code string) bool { return mpTaskCodes[code] }

// growthPending 判断任务是否待办（未领取、未锁定、进度未达标）。
func growthPending(t upstream.Task) bool {
	if t.Claimed || t.Locked {
		return false
	}
	if t.Target > 0 && t.Current >= t.Target {
		return false
	}
	if t.AcceptStatus == "completed" {
		return false
	}
	return true
}

// reportGap 连续上报之间的间隔（对齐上游实测 1.05s 口径，防风控）。
var reportGap = 1050 * time.Millisecond

// claimPollAttempts / claimPollGap 达标回读的有界轮询参数（上游异步计分）。
var (
	claimPollAttempts = 4
	claimPollGap      = 3 * time.Second
)

// ---------------------------------------------------------------------------
// GrowthEngine 成长任务引擎
// ---------------------------------------------------------------------------

// GrowthEngine 执行成长任务队列：扫描→接受→自动完成→领奖，带进度快照。
type GrowthEngine struct {
	growth *upstream.Growth
	logf   func(string, ...any)

	mu         sync.Mutex
	running    bool
	total      int
	done       int
	current    string // 正在执行的任务 code
	currentUID string
	startedAt  time.Time
	lastErr    string
	results    []GrowthItemResult // 本轮各任务项结果（任务中心队列视图用）
}

// GrowthItemResult 单个任务项的执行结果。
type GrowthItemResult struct {
	UID     string `json:"uid"`
	Code    string `json:"code"`
	Title   string `json:"title"`
	Status  string `json:"status"` // done|skipped|error
	Message string `json:"message,omitempty"`
}

// recordResult 记录单个任务项结果（调用时已持有 e.mu 或单线程执行路径）。
func (e *GrowthEngine) recordResult(uid, code, title, status, message string) {
	e.mu.Lock()
	e.results = append(e.results, GrowthItemResult{UID: uid, Code: code, Title: title, Status: status, Message: message})
	e.mu.Unlock()
}

// Results 返回本轮各任务项结果快照。
func (e *GrowthEngine) Results() []GrowthItemResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]GrowthItemResult, len(e.results))
	copy(out, e.results)
	return out
}

// SetGrowth 注入成长任务上游客户端（未注入时 runGrowth 保持占位行为）。
func (s *Scheduler) SetGrowth(g *upstream.Growth) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.growth = g
}

// growthEngine 返回引擎（未注入时返回 nil）。
func (s *Scheduler) growthEngine() *GrowthEngine {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.growth == nil {
		return nil
	}
	if s.gEngine == nil {
		s.gEngine = &GrowthEngine{growth: s.growth, logf: s.logf}
	}
	return s.gEngine
}

// growthAccountOK 成长任务账号门控：跳过停用/global/企业号。
func growthAccountOK(acct *types.Account) bool {
	if acct == nil || acct.Disabled {
		return false
	}
	if acct.Region == "intl" {
		return false // global 无 cn 成长任务体系
	}
	if acct.EnterpriseID != "" {
		return false // 企业版无个人成长体系（上游一律 403）
	}
	return true
}

// taskByCode 在两口径列表中定位单个任务（mp 专属码自动回落 mp 列表）。
func (e *GrowthEngine) taskByCode(acct *types.Account, code string) (*upstream.Task, error) {
	tasks, err := e.growth.ListTasks(acct)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	if isMPTaskCode(code) {
		return e.taskByCodeMP(acct, code)
	}
	return nil, nil
}

// taskByCodeMP 以小程序口径定位单个任务。
func (e *GrowthEngine) taskByCodeMP(acct *types.Account, code string) (*upstream.Task, error) {
	tasks, err := e.growth.ListTasksMP(acct)
	if err != nil {
		return nil, err
	}
	for i := range tasks {
		if tasks[i].TaskCode == code {
			return &tasks[i], nil
		}
	}
	return nil, nil
}

// taskByCodeWaiting 回读任务，若未达标则在有界预算内轮询等待（上游异步计分）。
func (e *GrowthEngine) taskByCodeWaiting(acct *types.Account, code string) (*upstream.Task, error) {
	t, err := e.taskByCode(acct, code)
	if err != nil || t == nil {
		return t, err
	}
	if t.Claimable || t.Claimed {
		return t, nil
	}
	for i := 1; i < claimPollAttempts; i++ {
		time.Sleep(claimPollGap)
		t2, err2 := e.taskByCode(acct, code)
		if err2 != nil {
			return t, nil
		}
		if t2 != nil {
			t = t2
			if t.Claimable || t.Claimed {
				return t, nil
			}
		}
	}
	return t, nil
}

// truncateBytes 截断字节串（错误信息防刷屏）。
func truncateBytes(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n])
}

// taskProgressText 任务进度文本。
func taskProgressText(t *upstream.Task) string {
	if t == nil {
		return "?"
	}
	if t.Target > 0 {
		return fmt.Sprintf("%d/%d", t.Current, t.Target)
	}
	if t.Claimed {
		return "claimed"
	}
	return t.AcceptStatus
}

// listAll 拉取账号的全量任务（双口径合并去重，按动作表排序；不过滤）。
func (e *GrowthEngine) listAll(acct *types.Account) ([]upstream.Task, error) {
	tasks, err := e.growth.ListTasks(acct)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, t := range tasks {
		seen[t.TaskCode] = true
	}
	if mpTasks, mpErr := e.growth.ListTasksMP(acct); mpErr == nil {
		for _, t := range mpTasks {
			if !seen[t.TaskCode] {
				tasks = append(tasks, t)
				seen[t.TaskCode] = true
			}
		}
	}
	sort.Slice(tasks, func(i, j int) bool {
		return autoActionIndex(tasks[i].TaskCode) < autoActionIndex(tasks[j].TaskCode)
	})
	return tasks, nil
}

// Scan 拉取账号的待办任务（只读，不执行；合并双口径去重，按动作表排序）。
func (e *GrowthEngine) Scan(acct *types.Account) ([]upstream.Task, error) {
	tasks, err := e.listAll(acct)
	if err != nil {
		return nil, err
	}
	var pending []upstream.Task
	for _, t := range tasks {
		if growthPending(t) {
			pending = append(pending, t)
		}
	}
	return pending, nil
}

// acceptPending 批量接受未接受的任务（双口径），返回接受个数。
func (e *GrowthEngine) acceptPending(acct *types.Account) int {
	tasks, err := e.growth.ListTasks(acct)
	if err != nil {
		return 0
	}
	var codes []string
	for _, t := range tasks {
		if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
			codes = append(codes, t.TaskCode)
		}
	}
	accepted := 0
	if len(codes) > 0 {
		if err := e.growth.AcceptTasks(acct, codes); err == nil {
			accepted += len(codes)
		} else {
			e.logf("growth: 接受任务失败 uid=%s: %v（不阻塞）", acct.UID, err)
		}
	}
	// mp 口径单独接受
	if mpTasks, err := e.growth.ListTasksMP(acct); err == nil {
		var mpCodes []string
		for _, t := range mpTasks {
			if !t.Claimed && !t.Locked && t.AcceptStatus != "accepted" && t.AcceptStatus != "completed" {
				mpCodes = append(mpCodes, t.TaskCode)
			}
		}
		if len(mpCodes) > 0 {
			if err := e.growth.AcceptTasksMP(acct, mpCodes); err == nil {
				accepted += len(mpCodes)
			}
		}
	}
	return accepted
}

// claimIfReady 进度达标则自动领奖，返回 "；自动领奖 +X 分 +Y 能" 后缀（无则空串）。
func (e *GrowthEngine) claimIfReady(acct *types.Account, code string, after *upstream.Task) string {
	if after == nil || !after.Claimable {
		return ""
	}
	var credit, energy int64
	var err error
	if isMPTaskCode(code) {
		credit, energy, err = e.growth.ClaimRewardMP(acct, code)
	} else {
		credit, energy, err = e.growth.ClaimReward(acct, code)
	}
	if err != nil {
		return "；达标但领奖失败（可在面板手动重试）"
	}
	if credit > 0 || energy > 0 {
		return fmt.Sprintf("；已自动领奖 +%d 分 +%d 能", credit, energy)
	}
	return "；奖励此前已领取"
}

// runOneAccount 对单个账号执行：接受→逐项自动完成→领奖。返回完成/跳过/失败计数。
func (e *GrowthEngine) runOneAccount(ctx context.Context, acct *types.Account) (done, skipped, failed int) {
	pending, err := e.Scan(acct)
	if err != nil {
		e.logf("growth: 扫描任务失败 uid=%s: %v", acct.UID, err)
		return 0, 0, 1
	}
	if len(pending) == 0 {
		return 0, 0, 0
	}
	if n := e.acceptPending(acct); n > 0 {
		time.Sleep(reportGap)
	}
	for _, t := range pending {
		select {
		case <-ctx.Done():
			return done, skipped, failed
		default:
		}
		act := autoActionFor(t.TaskCode)
		e.setCurrent(acct.UID, t.TaskCode)
		if act == nil {
			e.logf("growth: 任务 %s 无自动动作，跳过", t.TaskCode)
			e.recordResult(acct.UID, t.TaskCode, t.Title, "skipped", "无自动动作")
			skipped++
			continue
		}
		// 幂等：执行前再查一次状态
		before, err := e.taskByCode(acct, t.TaskCode)
		if err == nil && before != nil && (before.Claimed || (before.Target > 0 && before.Current >= before.Target)) {
			e.recordResult(acct.UID, t.TaskCode, t.Title, "skipped", "已完成/已领取")
			skipped++
			continue
		}
		msg, err := act.run(e, acct)
		if err != nil {
			e.logf("growth: 任务 %s 执行失败 uid=%s: %v", t.TaskCode, acct.UID, err)
			e.recordResult(acct.UID, t.TaskCode, t.Title, "error", err.Error())
			failed++
			continue
		}
		var after *upstream.Task
		if isMPTaskCode(t.TaskCode) {
			after, _ = e.taskByCodeMP(acct, t.TaskCode)
		} else {
			after, _ = e.taskByCodeWaiting(acct, t.TaskCode)
		}
		msg += e.claimIfReady(acct, t.TaskCode, after)
		if after != nil {
			msg += "（进度 " + taskProgressText(after) + "）"
		}
		e.logf("growth: uid=%s code=%s: %s", acct.UID, t.TaskCode, msg)
		e.recordResult(acct.UID, t.TaskCode, t.Title, "done", msg)
		done++
		e.markDone()
		time.Sleep(reportGap) // 项间节流
	}
	return done, skipped, failed
}

// setCurrent / markDone 更新队列进度快照。
func (e *GrowthEngine) setCurrent(uid, code string) {
	e.mu.Lock()
	e.currentUID, e.current = uid, code
	e.mu.Unlock()
}

func (e *GrowthEngine) markDone() {
	e.mu.Lock()
	e.done++
	e.mu.Unlock()
}

// Progress 返回队列进度快照（面板轮询用）。
type GrowthProgress struct {
	Running    bool   `json:"running"`
	Total      int    `json:"total"`
	Done       int    `json:"done"`
	Current    string `json:"current"`
	CurrentUID string `json:"current_uid"`
	StartedAt  string `json:"started_at"`
	LastErr    string `json:"last_err,omitempty"`
}

func (e *GrowthEngine) Progress() GrowthProgress {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := GrowthProgress{
		Running: e.running, Total: e.total, Done: e.done,
		Current: e.current, CurrentUID: e.currentUID, LastErr: e.lastErr,
	}
	if !e.startedAt.IsZero() {
		p.StartedAt = e.startedAt.Format(time.RFC3339)
	}
	return p
}

// RunOnce 执行一轮全账号成长任务（异步安全：已在跑则直接返回 false）。
// 账号内串行、账号间 2 秒错峰。
func (e *GrowthEngine) RunOnce(ctx context.Context, accounts []*types.Account) bool {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return false
	}
	e.running = true
	e.total, e.done = 0, 0
	e.current, e.currentUID, e.lastErr = "", "", ""
	e.results = nil
	e.startedAt = time.Now()
	e.mu.Unlock()

	defer func() {
		e.mu.Lock()
		e.running = false
		e.mu.Unlock()
	}()

	// 先扫描各账号待办总数（用于进度 total）
	type job struct {
		acct    *types.Account
		pending []upstream.Task
	}
	var jobs []job
	for _, acct := range accounts {
		if !growthAccountOK(acct) {
			continue
		}
		pending, err := e.Scan(acct)
		if err != nil {
			e.logf("growth: 扫描失败 uid=%s: %v", acct.UID, err)
			continue
		}
		if len(pending) > 0 {
			jobs = append(jobs, job{acct, pending})
		}
	}
	total := 0
	for _, j := range jobs {
		total += len(j.pending)
	}
	e.mu.Lock()
	e.total = total
	e.mu.Unlock()
	if total == 0 {
		e.logf("growth: 无可执行待办（全部账号任务已完成或无成长体系）")
		return true
	}
	e.logf("growth: 开始执行 %d 项待办（%d 个账号）", total, len(jobs))
	for i, j := range jobs {
		if i > 0 {
			select {
			case <-ctx.Done():
				return true
			case <-time.After(2 * time.Second): // 账号间错峰
			}
		}
		d, s, f := e.runOneAccount(ctx, j.acct)
		e.logf("growth: 账号 %s 完成：%d 项 done / %d 跳过 / %d 失败", j.acct.UID, d, s, f)
	}
	e.logf("growth: 本轮结束（共 %d 项）", total)
	return true
}

// ---------------------------------------------------------------------------
// scheduler 接入
// ---------------------------------------------------------------------------

// runGrowth 执行成长任务队列（每日 01:00），替代此前的 TODO 空壳。
func (s *Scheduler) runGrowth(ctx context.Context) error {
	engine := s.growthEngine()
	if engine == nil {
		s.logf("scheduler: 成长任务引擎未注入（SetGrowth），本轮跳过")
		return nil
	}
	engine.RunOnce(ctx, s.accounts())
	return nil
}

// GrowthScan 供面板"扫描"调用：返回各账号待办（只读）。
func (s *Scheduler) GrowthScan(ctx context.Context) (map[string][]upstream.Task, error) {
	engine := s.growthEngine()
	if engine == nil {
		return nil, fmt.Errorf("成长任务引擎未注入")
	}
	out := map[string][]upstream.Task{}
	for _, acct := range s.accounts() {
		if !growthAccountOK(acct) {
			continue
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		default:
		}
		pending, err := engine.Scan(acct)
		if err != nil {
			s.logf("growth: 扫描失败 uid=%s: %v", acct.UID, err)
			continue
		}
		out[acct.UID] = pending
		time.Sleep(time.Second) // 账号间节流
	}
	return out, nil
}

// GrowthTasks 供面板 overview 调用：各账号全量任务（只读）。
func (s *Scheduler) GrowthTasks(ctx context.Context) (map[string][]upstream.Task, error) {
	engine := s.growthEngine()
	if engine == nil {
		return nil, fmt.Errorf("成长任务引擎未注入")
	}
	out := map[string][]upstream.Task{}
	for _, acct := range s.accounts() {
		if !growthAccountOK(acct) {
			continue
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		default:
		}
		tasks, err := engine.listAll(acct)
		if err != nil {
			s.logf("growth: 拉取任务失败 uid=%s: %v", acct.UID, err)
			continue
		}
		out[acct.UID] = tasks
		time.Sleep(time.Second) // 账号间节流
	}
	return out, nil
}

// GrowthRunNow 异步触发一轮成长任务（面板"执行"按钮用）。
// 返回 (ok=引擎可用, started=本次真正启动；已在跑时 started=false)。
func (s *Scheduler) GrowthRunNow() (bool, bool) {
	engine := s.growthEngine()
	if engine == nil {
		return false, false
	}
	engine.mu.Lock()
	already := engine.running
	engine.mu.Unlock()
	if already {
		return true, false
	}
	go engine.RunOnce(context.Background(), s.accounts())
	return true, true
}

// GrowthHasAutoAction 报告任务码是否有自动完成动作（面板展示用）。
func GrowthHasAutoAction(code string) bool { return autoActionFor(code) != nil }

// GrowthProgress 返回队列进度（面板轮询用）。
func (s *Scheduler) GrowthProgress() GrowthProgress {
	engine := s.growthEngine()
	if engine == nil {
		return GrowthProgress{}
	}
	return engine.Progress()
}

// GrowthResults 返回本轮各任务项结果（任务中心队列视图用）。
func (s *Scheduler) GrowthResults() []GrowthItemResult {
	engine := s.growthEngine()
	if engine == nil {
		return nil
	}
	return engine.Results()
}

// growthFindAccount 按 uid 找账号（面板单账号操作用）。
func (s *Scheduler) growthFindAccount(uid string) *types.Account {
	for _, a := range s.accounts() {
		if a != nil && a.UID == uid {
			return a
		}
	}
	return nil
}

// GrowthAccountTasks 拉取单个账号的全量任务（面板任务弹窗用）。
func (s *Scheduler) GrowthAccountTasks(uid string) ([]upstream.Task, error) {
	engine := s.growthEngine()
	if engine == nil {
		return nil, fmt.Errorf("成长任务引擎未注入")
	}
	acct := s.growthFindAccount(uid)
	if acct == nil {
		return nil, fmt.Errorf("账号不存在")
	}
	return engine.listAll(acct)
}

// GrowthAccept 接受单个账号的指定任务。
func (s *Scheduler) GrowthAccept(uid string, codes []string) error {
	engine := s.growthEngine()
	if engine == nil {
		return fmt.Errorf("成长任务引擎未注入")
	}
	acct := s.growthFindAccount(uid)
	if acct == nil {
		return fmt.Errorf("账号不存在")
	}
	return engine.growth.AcceptTasks(acct, codes)
}

// GrowthAcceptAll 接受单个账号全部待接受任务，返回接受数。
func (s *Scheduler) GrowthAcceptAll(uid string) (int, error) {
	engine := s.growthEngine()
	if engine == nil {
		return 0, fmt.Errorf("成长任务引擎未注入")
	}
	acct := s.growthFindAccount(uid)
	if acct == nil {
		return 0, fmt.Errorf("账号不存在")
	}
	return engine.acceptPending(acct), nil
}

// GrowthClaim 领取单个账号指定任务的奖励，返回 (积分, 能量)。
func (s *Scheduler) GrowthClaim(uid, code string) (int64, int64, error) {
	engine := s.growthEngine()
	if engine == nil {
		return 0, 0, fmt.Errorf("成长任务引擎未注入")
	}
	acct := s.growthFindAccount(uid)
	if acct == nil {
		return 0, 0, fmt.Errorf("账号不存在")
	}
	return engine.growth.ClaimReward(acct, code)
}

// growthSingleResult 单任务自动执行的结果（面板弹窗用）。
type growthSingleResult struct {
	Skipped        bool
	Message        string
	ProgressBefore string
	ProgressAfter  string
	Claimed        bool
	Claimable      bool
}

// GrowthRunSingle 对单个账号执行单个任务的自动动作（含领奖）。
func (s *Scheduler) GrowthRunSingle(uid, code string) (*growthSingleResult, error) {
	engine := s.growthEngine()
	if engine == nil {
		return nil, fmt.Errorf("成长任务引擎未注入")
	}
	acct := s.growthFindAccount(uid)
	if acct == nil {
		return nil, fmt.Errorf("账号不存在")
	}
	act := autoActionFor(code)
	if act == nil {
		return &growthSingleResult{Skipped: true, Message: "该任务无自动动作，需在官方客户端内完成"}, nil
	}
	before, _ := engine.taskByCode(acct, code)
	msg, err := act.run(engine, acct)
	if err != nil {
		return nil, err
	}
	res := &growthSingleResult{Message: msg}
	if before != nil {
		res.ProgressBefore = taskProgressText(before)
	}
	after, _ := engine.taskByCodeWaiting(acct, code)
	if after != nil {
		res.ProgressAfter = taskProgressText(after)
		res.Claimable = after.Claimable
	}
	res.Message += engine.claimIfReady(acct, code, after)
	if after != nil && after.Claimed {
		res.Claimed = true
	}
	return res, nil
}

// GrowthRunAllSingles 对单个账号依次执行全部可自动任务。
func (s *Scheduler) GrowthRunAllSingles(uid string) []map[string]string {
	engine := s.growthEngine()
	out := []map[string]string{}
	if engine == nil {
		return out
	}
	acct := s.growthFindAccount(uid)
	if acct == nil {
		return out
	}
	tasks, err := engine.listAll(acct)
	if err != nil {
		return out
	}
	for _, t := range tasks {
		if t.Claimed || t.Locked {
			continue
		}
		act := autoActionFor(t.TaskCode)
		if act == nil {
			out = append(out, map[string]string{"code": t.TaskCode, "status": "skipped", "message": "无自动动作"})
			continue
		}
		r, err := s.GrowthRunSingle(uid, t.TaskCode)
		if err != nil {
			out = append(out, map[string]string{"code": t.TaskCode, "status": "error", "message": err.Error()})
			continue
		}
		st := "done"
		if r.Skipped {
			st = "skipped"
		}
		out = append(out, map[string]string{"code": t.TaskCode, "status": st, "message": r.Message})
		time.Sleep(reportGap)
	}
	return out
}

// ---------------------------------------------------------------------------
// 各任务动作实现（移植自 linguo autotask.go）
// ---------------------------------------------------------------------------

// runChat5 补足 chat_5 的进度：按差额上报 chat_request_send。
func runChat5(e *GrowthEngine, acct *types.Account) (string, error) {
	t, err := e.taskByCode(acct, "chat_5")
	if err != nil {
		return "", err
	}
	if t == nil {
		return "", fmt.Errorf("任务不存在")
	}
	target := t.Target
	if target <= 0 {
		target = 5
	}
	need := target - t.Current
	if need <= 0 {
		return "进度已达标，无需上报", nil
	}
	for i := int64(0); i < need; i++ {
		cid := fmt.Sprintf("wb2a-chat5-%d-%d", time.Now().UnixMilli(), i)
		if err := e.growth.ReportChatActivity(acct, cid, ""); err != nil {
			return fmt.Sprintf("上报第 %d/%d 条失败: %v", i+1, need, err), nil
		}
		if i < need-1 {
			time.Sleep(reportGap)
		}
	}
	return fmt.Sprintf("已补报 %d 条对话事件", need), nil
}

// runFirstBuddy 领养：report（解锁前置）→ agreement → first。
func runFirstBuddy(e *GrowthEngine, acct *types.Account) (string, error) {
	if err := e.growth.ReportChatActivity(acct, fmt.Sprintf("wb2a-adopt-%d", time.Now().UnixMilli()), ""); err != nil {
		return "", fmt.Errorf("前置上报: %w", err)
	}
	time.Sleep(reportGap)
	if err := e.growth.BuddyAgreement(acct); err != nil {
		return "", fmt.Errorf("同意协议: %w", err)
	}
	if err := e.growth.BuddyFirst(acct); err != nil {
		if upstream.IsBuddyTaskIncomplete(err) {
			return "前置已上报，但领养门槛未过（上游要求当日活跃），请稍后重试", nil
		}
		return "", fmt.Errorf("领取 Buddy: %w", err)
	}
	return "已领取 Buddy（+300 分 +8 能量）", nil
}

// runModelChat 完成 Model_chat_GLM5.2：accept → 真实对话 → 对齐模型上报。
func runModelChat(e *GrowthEngine, acct *types.Account) (string, error) {
	const code, modelID, modelName = "Model_chat_GLM5.2", "glm-5.2", "GLM-5.2"
	if err := e.growth.AcceptTasks(acct, []string{code}); err != nil {
		e.logf("growth: accept %s: %v（继续走行为链路）", code, err)
	}
	time.Sleep(reportGap)
	body, _ := json.Marshal(map[string]any{
		"model": modelID,
		"messages": []map[string]any{
			{"role": "user", "content": "hi，请回复一句话"},
		},
		"stream": true,
	})
	rc, status, respBody, err := e.growth.GrowthChatStream(acct, body)
	if err != nil {
		return "", fmt.Errorf("对话请求: %w", err)
	}
	if status >= 400 {
		rc.Close()
		return "", fmt.Errorf("对话失败 http=%d: %s", status, truncateBytes(respBody, 160))
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 1<<20)) // 读干 SSE
	rc.Close()
	time.Sleep(reportGap)
	if err := e.growth.ReportChatActivityModel(acct, fmt.Sprintf("wb2a-glm52-%d", time.Now().UnixMilli()), "", modelID, modelName); err != nil {
		return "对话已完成，但进度上报失败：" + err.Error(), nil
	}
	return "已完成 glm-5.2 对话并上报", nil
}

// runRichMeow 完成 RichMeow_Chat（桌面端对话1次，桌面指纹事件链）。
func runRichMeow(e *GrowthEngine, acct *types.Account) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2a-rm-%d", ms)
	req := fmt.Sprintf("wb2a-rm-req-%d", ms)
	msg := fmt.Sprintf("req-%d-user", ms)
	events := upstream.DesktopChatSequence(conv, req, msg, "fast-model", "fast-model")
	if err := e.growth.ReportDesktopEvent(acct, events...); err != nil {
		return "", err
	}
	return "已按桌面端指纹上报完整对话事件链", nil
}

// runBuddyApp 完成 Buddy_App / Buddy_App_QQ（进入 Buddy 应用五连事件）。
func runBuddyApp(e *GrowthEngine, acct *types.Account) (string, error) {
	events := upstream.DesktopBuddyAppSequence("cb_y5Dy46tPQGGWtueMxXbe", "企鹅教师助手")
	if err := e.growth.ReportDesktopEvent(acct, events...); err != nil {
		return "", err
	}
	return "已上报 buddyapp 进入五连事件（同时覆盖 Buddy_App 与 Buddy_App_QQ）", nil
}

// runAutomationCreate 完成 automation_1（定时任务创建事件）。
func runAutomationCreate(e *GrowthEngine, acct *types.Account) (string, error) {
	if err := e.growth.ReportDesktopEvent(acct,
		upstream.DesktopAutomationCreateEvent("wb2a 自动化")); err != nil {
		return "", err
	}
	return "已上报定时任务创建事件", nil
}

// runLibraryRead 完成 Library_read（资料库介绍阅读事件，Web 域）。
func runLibraryRead(e *GrowthEngine, acct *types.Account) (string, error) {
	const docURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
	if err := e.growth.ReportWebEvent(acct, "web_element_click", docURL,
		"library_doc_intro_click", "WorkBuddy资料库介绍"); err != nil {
		return "", err
	}
	return "已上报资料库介绍阅读事件", nil
}

// runBlackCat 完成 black_cat（夜猫子，23:00–08:00 窗口内 glm-5.2 对话）。
func runBlackCat(e *GrowthEngine, acct *types.Account) (string, error) {
	if !upstream.InNightWindow(time.Now()) {
		return "当前不在 23:00–08:00 计数窗口，行为不计分；每日 01:00 排程在窗口内会自动补足", nil
	}
	need, err := e.growth.BlackcatNeed(acct)
	if err != nil {
		return "", err
	}
	if need <= 0 {
		return "进度已达标，无需补足", nil
	}
	ok, err := e.growth.RunNightChats(acct, int(need))
	if err != nil {
		return fmt.Sprintf("完成 %d/%d 次后中断: %v", ok, need, err), nil
	}
	return fmt.Sprintf("已完成 %d 次夜间对话并上报", ok), nil
}

// runSkillFresh 完成 skill_1（真实对话 + skill_info 技能加载事件）。
func runSkillFresh(e *GrowthEngine, acct *types.Account) (string, error) {
	conv, req, err := e.growth.DesktopChatWithExpert(acct, "")
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	msgID := "msg-" + req[len(req)-8:]
	events := upstream.DesktopChatSequence(conv, req, msgID, "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "chat_message_response" {
			ev["finishReason"] = "tool_calls" // 模型发起工具调用（技能加载）语义
		}
	}
	events = append(events, upstream.DesktopEvent{
		"eventCode": "skill_info", "id": "润泽小馆·日报撰写",
		"skillId": "skill_2097350077599879168", "skillVersion": "1.0.0",
		"toolStatus": "success", "fileCount": 56, "source": "workbuddy-desktop",
		"conversationId": conv, "requestId": req, "messageId": msgID,
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"traceId": req,
	})
	if err := e.growth.ReportDesktopEvent(acct, events...); err != nil {
		return "", fmt.Errorf("skill_info 事件: %w", err)
	}
	return "已上报真实对话 + skill_info 技能加载事件", nil
}

// runExpertLighthouse 完成 Expert_lighthouse（腾讯轻量云专家召唤+使用链）。
func runExpertLighthouse(e *GrowthEngine, acct *types.Account) (string, error) {
	const lhID = "ex_2cvvUZQhDyeJ"
	lh := upstream.MarketExpert{
		ExpertID: lhID, ExpertType: "agent",
		DisplayNameZH: "腾讯轻量云专家", ProfessionZH: "腾讯轻量云专家", Version: "1.0.2",
	}
	if experts, err := e.growth.MarketExpertList(acct, "agent"); err == nil {
		for _, ex := range experts {
			if ex.ExpertID == lhID {
				lh = ex
				break
			}
		}
	}
	if err := e.growth.ReportDesktopEvent(acct, upstream.DesktopExpertSummonSequence(lh)...); err != nil {
		return "", fmt.Errorf("召唤链: %w", err)
	}
	conv, req, err := e.growth.DesktopChatWithExpert(acct, lhID)
	if err != nil {
		return "", fmt.Errorf("真实对话: %w", err)
	}
	events := upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model")
	for _, ev := range events {
		if ev["eventCode"] == "agent_task_created" {
			ev["has_expert"] = true
			ev["expert_id"] = lh.ExpertID
			ev["expert_name"] = lh.DisplayNameZH
			ev["expert_industry_id"] = ""
		}
	}
	last := upstream.DesktopExpertActualUseLocal(lh, conv, req)
	last["type"] = "" // 对齐真实样本：轻量云 actual_use 的 type 为空、cost=0
	last["cost"] = 0
	events = append(events, last)
	if err := e.growth.ReportDesktopEvent(acct, events...); err != nil {
		return "", fmt.Errorf("使用事件: %w", err)
	}
	return "已上报轻量云专家召唤+使用链（真实对话 requestId）", nil
}

// runAppearance 完成 Hp_Appearance（设置主题 + 皮肤生效事件）。
func runAppearance(e *GrowthEngine, acct *types.Account) (string, error) {
	const themeKey = "theme-tkmw7j" // 判据主题
	if err := e.growth.SetAppearanceTheme(acct, themeKey); err != nil {
		return "", fmt.Errorf("设置主题: %w", err)
	}
	time.Sleep(2 * time.Second)
	if err := e.growth.ReportDesktopEvent(acct, upstream.DesktopEvent{
		"eventCode": "appearance_skin_apply", "action": "apply", "source": "settings_close",
		"id": themeKey, "vipLevel": 0, "series": "", "type": "unknown",
	}); err != nil {
		return "", err
	}
	return "已设置主题并上报皮肤生效事件", nil
}

// runTemplateUse 完成 template_5（模板事件组 ×5）。
func runTemplateUse(e *GrowthEngine, acct *types.Account) (string, error) {
	templates := [][2]string{{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"}, {"4", "活动策划"}, {"5", "代码评审"}}
	for i, tp := range templates {
		ms := time.Now().UnixMilli()
		conv := fmt.Sprintf("wb2a-tpl-%d-%d", ms, i)
		req := fmt.Sprintf("wb2a-tpl-req-%d-%d", ms, i)
		events := upstream.DesktopTemplateUseSequence(conv, req, tp[0], tp[1])
		if err := e.growth.ReportDesktopEvent(acct, events...); err != nil {
			return fmt.Sprintf("第 %d 组模板事件上报失败: %v", i+1, err), nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "已上报 template_used ×5", nil
}

// runPlaybookPrompt 完成 playbook_prompt（灵感案例 prompt 发送）。
func runPlaybookPrompt(e *GrowthEngine, acct *types.Account) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2a-pb-%d", ms)
	req := fmt.Sprintf("wb2a-pb-req-%d", ms)
	events := upstream.DesktopPlaybookPromptSequence(conv, req, "pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")
	if err := e.growth.ReportDesktopEvent(acct, events...); err != nil {
		return "", err
	}
	return "已上报 playbook_cta_click + playbook_prompt_send", nil
}

// runCreateCanvas 完成 create_canvas（设计画布创建，+300 分）。
func runCreateCanvas(e *GrowthEngine, acct *types.Account) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2a-canvas-%d", ms)
	req := fmt.Sprintf("wb2a-canvas-req-%d", ms)
	events := upstream.DesktopDesignCanvasSequence(conv, req)
	if err := e.growth.ReportDesktopEvent(acct, events...); err != nil {
		return "", err
	}
	return "已上报 wbx_design_canvas_task_create/open", nil
}

// expertSummonGap 专家召唤链间隔（真实使用节奏）。
const expertSummonGap = 6 * time.Second

// runExpertUse 完成 expert_5（5 个平台专家召唤+使用）。
func runExpertUse(e *GrowthEngine, acct *types.Account) (string, error) {
	return runExpertBatch(e, acct, "agent", 5)
}

// runExpertTeamUse 完成 Expert_team_use_3（3 个专家团）。
func runExpertTeamUse(e *GrowthEngine, acct *types.Account) (string, error) {
	return runExpertBatch(e, acct, "team", 3)
}

// runExpertBatch 专家召唤+使用的公共实现（失败逐个继续）。
func runExpertBatch(e *GrowthEngine, acct *types.Account, expertType string, count int) (string, error) {
	experts, err := e.growth.MarketExpertList(acct, expertType)
	if err != nil {
		return "", fmt.Errorf("拉取专家列表: %w", err)
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家市场列表为空")
	}
	ok, fail := 0, 0
	for i, ex := range experts {
		if ok >= count {
			break
		}
		if err := e.growth.ReportDesktopEvent(acct, upstream.DesktopExpertSummonSequence(ex)...); err != nil {
			fail++
			continue
		}
		conv, req, cerr := e.growth.DesktopChatWithExpert(acct, ex.ExpertID)
		if cerr != nil {
			fail++
			continue
		}
		events := append(upstream.DesktopChatSequence(conv, req, "msg-"+req[len(req)-8:], "fast-model", "fast-model"),
			upstream.DesktopExpertActualUseEvent(ex, conv, req))
		if err := e.growth.ReportDesktopEvent(acct, events...); err != nil {
			fail++
			continue
		}
		ok++
		if i < len(experts)-1 {
			time.Sleep(expertSummonGap)
		}
	}
	_ = fail
	return fmt.Sprintf("已对 %d 位真实专家完成召唤+使用链（类型 %s）", ok, expertType), nil
}

// runSchoolSeason 完成 school_season（校园日，小程序口径）。
func runSchoolSeason(e *GrowthEngine, acct *types.Account) (string, error) {
	conv := fmt.Sprintf("wb2a-sch-%d", time.Now().UnixNano())
	if err := e.growth.ReportMPEvent(acct, upstream.SchoolSeasonChatEvent(conv)); err != nil {
		return "", fmt.Errorf("校园日对话上报: %w", err)
	}
	return "已上报校园日 mini 对话事件", nil
}

// runSequentialChat 完成 Sequential_Tasks_1（小程序首对话）。
func runSequentialChat(e *GrowthEngine, acct *types.Account) (string, error) {
	conv := fmt.Sprintf("wb2a-seq1-%d", time.Now().UnixNano())
	if err := e.growth.ReportMPEvent(acct, upstream.SchoolChatTimesEvents(conv)); err != nil {
		return "", fmt.Errorf("小程序对话上报: %w", err)
	}
	return "已上报小程序首对话事件", nil
}

// runMiniExpert 完成 Sequential_Tasks_2（小程序选专家对话）。
func runMiniExpert(e *GrowthEngine, acct *types.Account) (string, error) {
	experts, err := e.growth.MarketExpertList(acct, "agent")
	if err != nil || len(experts) == 0 {
		return "", fmt.Errorf("拉取专家列表失败: %v", err)
	}
	ex := experts[0]
	if err := e.growth.ReportMPEvent(acct,
		upstream.MiniExpertUseEvent(ex.ExpertID, ex.DisplayNameZH, ex.ExpertType)); err != nil {
		return "", fmt.Errorf("专家使用上报: %w", err)
	}
	return fmt.Sprintf("已上报小程序专家使用事件（%s）", ex.DisplayNameZH), nil
}

// runSequentialChat5 完成 Sequential_Tasks_3（小程序五次对话，按差额补足）。
func runSequentialChat5(e *GrowthEngine, acct *types.Account) (string, error) {
	return runSequentialChatN(e, acct, "Sequential_Tasks_3", 5)
}

// runSequentialChat10 完成 Sequential_Tasks_6（小程序十次对话，按差额补足）。
func runSequentialChat10(e *GrowthEngine, acct *types.Account) (string, error) {
	return runSequentialChatN(e, acct, "Sequential_Tasks_6", 10)
}

// runSequentialChatN 小程序对话 N 次（按任务 target 差额补足）。
func runSequentialChatN(e *GrowthEngine, acct *types.Account, code string, fallback int) (string, error) {
	t, err := e.taskByCodeMP(acct, code)
	target := int64(fallback)
	var current int64
	if err == nil && t != nil {
		if t.Target > 0 {
			target = t.Target
		}
		current = t.Current
	}
	need := target - current
	if need <= 0 {
		return "进度已达标，无需上报", nil
	}
	for i := int64(0); i < need; i++ {
		conv := fmt.Sprintf("wb2a-seqn-%d-%d", time.Now().UnixNano(), i)
		if err := e.growth.ReportMPEvent(acct, upstream.SchoolChatTimesEvents(conv)); err != nil {
			return fmt.Sprintf("上报第 %d/%d 条失败: %v", i+1, need, err), nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Sprintf("已补报 %d 条小程序对话事件", need), nil
}

// runSequentialAutomation 完成 Sequential_Tasks_4（小程序定时任务创建事件）。
func runSequentialAutomation(e *GrowthEngine, acct *types.Account) (string, error) {
	if err := e.growth.ReportMPEvent(acct, map[string]any{
		"eventCode": "automated_task_create_suc", "name": "wb2a 小程序自动化",
		"source": "manually", "timestamp": time.Now().UnixMilli(),
	}); err != nil {
		return "", fmt.Errorf("定时任务事件上报: %w", err)
	}
	return "已上报小程序定时任务创建事件", nil
}

// runSequentialModelChat 完成 Sequential_Tasks_5（小程序 GLM5.2 对话上报）。
func runSequentialModelChat(e *GrowthEngine, acct *types.Account) (string, error) {
	conv := fmt.Sprintf("wb2a-seqm-%d", time.Now().UnixNano())
	if err := e.growth.ReportMPEvent(acct,
		upstream.MiniChatModelEvent(conv, "glm-5.2", "GLM-5.2")); err != nil {
		return "", fmt.Errorf("模型对话上报: %w", err)
	}
	return "已上报小程序 GLM5.2 对话事件", nil
}

// runSequentialPlaybook 完成 Sequential_Tasks_7（灵感事件组 PC+mp 双形态）。
func runSequentialPlaybook(e *GrowthEngine, acct *types.Account) (string, error) {
	ms := time.Now().UnixMilli()
	conv := fmt.Sprintf("wb2a-seqpb-%d", ms)
	req := fmt.Sprintf("wb2a-seqpb-req-%d", ms)
	// PC 形态
	if err := e.growth.ReportDesktopEvent(acct,
		upstream.DesktopPlaybookPromptSequence(conv, req, "pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸")...); err != nil {
		e.logf("growth: PC 灵感事件上报失败（继续 mp）: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	// mp 形态
	for _, ev := range upstream.MiniPlaybookEvents("pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸") {
		if err := e.growth.ReportMPEvent(acct, ev); err != nil {
			return "", fmt.Errorf("mp 灵感事件上报: %w", err)
		}
		time.Sleep(300 * time.Millisecond)
	}
	return "已上报灵感事件组（PC+mp 双形态）", nil
}
