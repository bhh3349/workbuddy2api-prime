package upstream

// Package modeldir 实现上游模型目录抓取（模型倍率 / 限时免费 / 折扣标签）。
//
// 设计目标：每天查一次上游并缓存到 data/models.json，面板直接读缓存展示，
// 不用每次打开页面都打上游（linguo 是每次打开面板都实时查，很烧账号）。
//
// 移植依据（以克隆仓库实际代码为准）：
//   - linguo internal/upstream/client.go FetchModels（约1238行）：两路并发
//     fetchEnterpriseModels（/console/enterprises/personal/models，agents[cli]
//     过滤）+ fetchV3Models（/v3/config，IDE UA 拿完整能力）；合并去重 key=模型
//     id，v3 条目优先（credits 等字段以 v3 为准），企业端点只补 v3 缺失的模型。
//   - global 两域见 global_models.go：v3/config 走桌面/IDE/CLI 三 UA 并集；企业端点
//     /v2/enterprises/personal/models 首选 → /console/... 兜底；id 有宽松回退链
//     id→modelId→model→name。
//   - 优惠 modelPromotions（/v3/config data.modelPromotions）：Credits 是牌价，
//     Promo* 是当前生效的限时优惠；同模型多条命中取 priority 最高；schedule 为
//     nil 视为全天生效，时区恒 Asia/Shanghai（FixedZone 免 tzdata 依赖）。
//   - 试用横幅 ModelTrialBanner：只在此处出现的模型（如 hy4-preview-f）要补入，
//     Credits/Tags 显式清空（那是转正后的计费信息，会误导展示）。
//   - nonChatModel 过滤（harness buddy.ts:547-555）：id 前缀 nes-/completion-/
//     codewise-、maxOutputTokens≤256、tags 含图片/视频生成类。
//   - 请求头：复用本包 billing.go 的 billingHeaders（Bearer + X-CodeBuddy-Request: 1
//     + Accept-Language + X-User-Id），v3/config 额外加 X-Requested-With /
//     X-Domain / X-Product: SaaS / UA（client.go fetchV3ConfigModelMap）。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"workbuddy2api-prime/internal/types"
)

const (
	modelDirCNBase   = "https://copilot.tencent.com"
	modelDirIntlBase = "https://www.workbuddy.ai"

	enterprisePathCN        = "/console/enterprises/personal/models"
	enterprisePathIntlV2    = "/v2/enterprises/personal/models"
	enterprisePathIntlPlain = "/console/enterprises/personal/models"
	v3ConfigPath            = "/v3/config"

	// v3/config 对 UA 敏感：IDE UA 拿完整能力版目录（linguo codeBuddyIDEUA）。
	modelDirIDEUA = "CodeBuddyIDE/4.12.0 CodeBuddy/4.12.0"
	modelDirCLIUA = "CLI/2.63.2 CodeBuddy/2.63.2"

	modelDirCacheFile = "models.json"
)

// ModelEntry 是面板展示用的模型目录精简条目。
type ModelEntry struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Realm             string   `json:"realm"`                   // "cn" | "intl"
	Credits           string   `json:"credits"`                 // 牌价倍率原文，如 "x0.05"
	PromoFactor       *float64 `json:"promo_factor,omitempty"`  // 折扣系数：0=限时免费，0.5=五折；nil=无机器可读折扣
	PromoCredits      string   `json:"promo_credits,omitempty"` // 折扣后倍率原文，如 "0x"
	PromoLabel        string   `json:"promo_label,omitempty"`   // 徽章文案：限时免费 / 夜间折扣 / 错峰使用
	PromoNote         string   `json:"promo_note,omitempty"`    // hover 说明原文（含时段/日期描述）
	Tags              []string `json:"tags,omitempty"`          // 模型标签，含 badge:限时免费 等
	SupportsReasoning bool     `json:"supports_reasoning"`
	SupportsImages    bool     `json:"supports_images"`
	Description       string   `json:"description,omitempty"`
}

// ModelDirClient 是模型目录抓取客户端。失败只返回 error，不 panic，
// 由调用方（scheduler 定时任务）决定重试策略。
type ModelDirClient struct {
	http *http.Client
	// base 可注入，测试时指向 httptest 服务器。
	cnBase   string
	intlBase string
}

// NewModelDirClient 创建模型目录客户端（单次请求 15s 超时）。
func NewModelDirClient() *ModelDirClient {
	return &ModelDirClient{
		http:     &http.Client{Timeout: 15 * time.Second},
		cnBase:   modelDirCNBase,
		intlBase: modelDirIntlBase,
	}
}

// dynModelEntry 上游模型目录单条解析形态（linguo dynModelEntry 精简版）。
type dynModelEntry struct {
	ID              string   `json:"id"`
	ModelID         string   `json:"modelId"`
	Model           string   `json:"model"`
	Name            string   `json:"name"`
	Description     string   `json:"descriptionZh"`
	Credits         string   `json:"credits"`
	Tags            []string `json:"tags"`
	MaxOutputTokens int64    `json:"maxOutputTokens"`
	Disabled        bool     `json:"disabled"`
	SupportsImages  bool     `json:"supportsImages"`
	SupportsReason  bool     `json:"supportsReasoning"`
}

// entryID 取模型的展示/去重 id：id→modelId→model→name 宽松回退（global 目录变体用）。
func (m dynModelEntry) entryID() string {
	for _, s := range []string{m.ID, m.ModelID, m.Model, m.Name} {
		if strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

// ModelPromotion 是 /v3/config data.modelPromotions 的单条优惠定义
// （linguo v3ModelPromotion 精简版）。导出供缓存与面板透出原始规则，
// 生效判断移到展示时（promoActiveAt），避免抓取时刻写死时段类优惠。
type ModelPromotion struct {
	Enabled  bool     `json:"enabled"`
	Priority int      `json:"priority"`
	ModelIDs []string `json:"modelIds"`
	Realm    string   `json:"realm,omitempty"` // 抓取域：cn|intl（合并多域时填充）
	Badge    *struct {
		Label string `json:"label"`
	} `json:"badge"`
	Discount *struct {
		DiscountedCredits string  `json:"discountedCredits"`
		Factor            float64 `json:"factor"`
	} `json:"discount"`
	Hover *struct {
		TextZh string `json:"textZh"`
	} `json:"hover"`
	Schedule *struct {
		Daily []struct {
			Start string `json:"start"` // "23:00"
			End   string `json:"end"`   // "7:50"，可跨午夜
		} `json:"daily"`
		ValidFrom  string `json:"validFrom"` // RFC3339，可缺省
		ValidUntil string `json:"validUntil"`
	} `json:"schedule"`
}

// PromoActiveAt 报告优惠在 now 是否生效（面板展示时调用，时区恒 Asia/Shanghai）。
func PromoActiveAt(p *ModelPromotion, now time.Time) bool {
	return promoActive(p, now)
}

// trialBanner 是试用横幅条目（ModelTrialBanner.banners）。
type trialBanner struct {
	ModelID       string `json:"modelId"`
	TargetModelID string `json:"targetModelId"`
}

// promoZone 优惠时区：上游恒 Asia/Shanghai，用 FixedZone 免依赖系统 tzdata。
var promoZone = time.FixedZone("CST", 8*3600)

// promoClock 解析 "HH:MM" 为当日分钟数。
func promoClock(hhmm string) (int, bool) {
	parts := strings.Split(hhmm, ":")
	if len(parts) != 2 {
		return -1, false
	}
	h, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
	m, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err1 != nil || err2 != nil || h < 0 || h > 24 || m < 0 || m > 59 {
		return -1, false
	}
	return h*60 + m, true
}

// promoActive 评估优惠在 now 是否生效：enabled + validFrom/validUntil 内 +
// 落在任一 daily 窗口（支持跨午夜）；schedule 为 nil 视为全天生效。
func promoActive(p *ModelPromotion, now time.Time) bool {
	if !p.Enabled {
		return false
	}
	if sc := p.Schedule; sc != nil {
		if sc.ValidFrom != "" {
			if from, err := time.Parse(time.RFC3339, sc.ValidFrom); err == nil && now.Before(from) {
				return false
			}
		}
		if sc.ValidUntil != "" {
			if until, err := time.Parse(time.RFC3339, sc.ValidUntil); err == nil && !now.Before(until) {
				return false
			}
		}
		if len(sc.Daily) > 0 {
			cur := now.Hour()*60 + now.Minute()
			inWindow := false
			for _, w := range sc.Daily {
				st, ok1 := promoClock(w.Start)
				ed, ok2 := promoClock(w.End)
				if !ok1 || !ok2 {
					continue
				}
				if st <= ed {
					if cur >= st && cur < ed {
						inWindow = true
						break
					}
				} else if cur >= st || cur < ed { // 跨午夜
					inWindow = true
					break
				}
			}
			if !inWindow {
				return false
			}
		}
	}
	return true
}

// nonChatModel 判定是否非对话模型（应过滤掉）。来源 harness buddy.ts:547-555：
// id 前缀 nes-/completion-/codewise-；maxOutputTokens≤256；tags 含图片/视频生成类。
func nonChatModel(id string, maxOutputTokens int64, tags []string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range []string{"nes-", "completion-", "codewise-"} {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	if maxOutputTokens > 0 && maxOutputTokens <= 256 {
		return true
	}
	for _, t := range tags {
		switch t {
		case "text-to-image", "image-to-image", "text-to-video", "image-to-video":
			return true
		}
	}
	return false
}

// modelDirHeaders 构造模型目录请求头：billing 头族 + v3/config 专用头。
func (c *ModelDirClient) modelDirHeaders(acct *types.Account, base, ua string) http.Header {
	h := billingHeaders(acct) // Bearer + X-CodeBuddy-Request:1 + Accept-Language + X-User-Id
	h.Set("Accept", "application/json, text/plain, */*")
	h.Set("X-Requested-With", "XMLHttpRequest")
	h.Set("X-Product", "SaaS")
	if u, err := url.Parse(base); err == nil && u.Host != "" {
		h.Set("X-Domain", u.Host)
	}
	if ua != "" {
		h.Set("User-Agent", ua)
	}
	return h
}

// doGet 发 GET 请求并解 {code,data} 信封，返回 data 段。
func (c *ModelDirClient) doGet(url string, acct *types.Account, ua string, base string) (json.RawMessage, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header = c.modelDirHeaders(acct, base, ua)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snip := string(raw)
		if len(snip) > 120 {
			snip = snip[:120]
		}
		return nil, fmt.Errorf("模型目录 HTTP %d: %s", resp.StatusCode, snip)
	}
	var env struct {
		Code int             `json:"code"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("模型目录解析失败: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("模型目录业务码 code=%d", env.Code)
	}
	return env.Data, nil
}

// toModelEntry 把解析条目转为展示条目（过滤非对话模型）。
func toModelEntry(m dynModelEntry, realm string) (ModelEntry, bool) {
	id := m.entryID()
	if id == "" || m.Disabled || nonChatModel(id, m.MaxOutputTokens, m.Tags) {
		return ModelEntry{}, false
	}
	return ModelEntry{
		ID:                id,
		Name:              m.Name,
		Realm:             realm,
		Credits:           m.Credits,
		Tags:              m.Tags,
		SupportsReasoning: m.SupportsReason,
		SupportsImages:    m.SupportsImages,
		Description:       m.Description,
	}, true
}

// v3result 是单路 /v3/config 抓取结果。
type v3result struct {
	entries []ModelEntry
	promos  []ModelPromotion
	banners []trialBanner
	err     error
}

// fetchV3Config 抓 /v3/config：全量模型 + 试用横幅 + 优惠定义。
func (c *ModelDirClient) fetchV3Config(base string, acct *types.Account, ua string, realm string) v3result {
	data, err := c.doGet(base+v3ConfigPath, acct, ua, base)
	if err != nil {
		return v3result{err: err}
	}
	var d struct {
		Models                []dynModelEntry `json:"models"`
		ProductFeaturesConfig struct {
			ModelTrialBanner struct {
				Banners []trialBanner `json:"banners"`
			} `json:"ModelTrialBanner"`
		} `json:"productFeaturesConfig"`
		ModelPromotions []ModelPromotion `json:"modelPromotions"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return v3result{err: fmt.Errorf("v3/config data 解析失败: %w", err)}
	}
	entries := make([]ModelEntry, 0, len(d.Models))
	for _, m := range d.Models {
		if e, ok := toModelEntry(m, realm); ok {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return v3result{err: fmt.Errorf("v3/config 返回空模型列表")}
	}
	return v3result{entries: entries, promos: d.ModelPromotions, banners: d.ProductFeaturesConfig.ModelTrialBanner.Banners}
}

// fetchEnterprise 抓企业模型端点：cn 用 agents[cli] 过滤，intl 取全量（id 宽松回退）。
// intl 按 [/v2/..., /console/...] 顺序，成功即停。
func (c *ModelDirClient) fetchEnterprise(base string, acct *types.Account, realm string) ([]ModelEntry, error) {
	paths := []string{enterprisePathCN}
	cliFilter := true
	if realm != "cn" {
		paths = []string{enterprisePathIntlV2, enterprisePathIntlPlain}
		cliFilter = false
	}
	var lastErr error
	for _, p := range paths {
		entries, err := c.fetchEnterpriseOnce(base+p, acct, base, realm, cliFilter)
		if err == nil {
			return entries, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func (c *ModelDirClient) fetchEnterpriseOnce(url string, acct *types.Account, base, realm string, cliFilter bool) ([]ModelEntry, error) {
	data, err := c.doGet(url, acct, "", base)
	if err != nil {
		return nil, err
	}
	var d struct {
		Models []dynModelEntry `json:"models"`
		Agents []struct {
			Name   string   `json:"name"`
			Models []string `json:"models"`
		} `json:"agents"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("企业模型端点 data 解析失败: %w", err)
	}
	// 字符串数组形态兜底（global 目录变体偶发纯 id 数组）。
	if len(d.Models) == 0 {
		var ids []string
		if err := json.Unmarshal(data, &ids); err == nil && len(ids) > 0 {
			entries := make([]ModelEntry, 0, len(ids))
			for _, id := range ids {
				if id = strings.TrimSpace(id); id != "" {
					entries = append(entries, ModelEntry{ID: id, Realm: realm})
				}
			}
			return entries, nil
		}
	}
	dynMap := make(map[string]dynModelEntry, len(d.Models))
	for _, m := range d.Models {
		id := m.entryID()
		if id == "" || nonChatModel(id, m.MaxOutputTokens, m.Tags) {
			continue
		}
		dynMap[id] = m
	}
	var ids []string
	if cliFilter {
		// CN：只取 agents[cli] 名单内的模型（linguo 口径）。
		found := false
		for _, ag := range d.Agents {
			if ag.Name == "cli" {
				ids = ag.Models
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("企业模型端点未返回 cli agent 名单")
		}
	} else {
		for id := range dynMap {
			ids = append(ids, id)
		}
		sort.Strings(ids)
	}
	entries := make([]ModelEntry, 0, len(ids))
	for _, id := range ids {
		m, ok := dynMap[id]
		if !ok || m.Disabled {
			continue
		}
		if e, ok := toModelEntry(m, realm); ok {
			entries = append(entries, e)
		}
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("企业模型端点返回空模型列表")
	}
	return entries, nil
}

// FetchModelDirectory 按账号 realm 抓取模型目录并合并：v3/config 为主（多 UA 并集），
// 企业端点补缺；挂试用横幅与当前生效优惠。两路独立容错，全失败才返回 error。
func (c *ModelDirClient) FetchModelDirectory(acct *types.Account) ([]ModelEntry, []ModelPromotion, error) {
	realm := acct.Region
	base := c.cnBase
	if realm != "cn" {
		realm = "intl"
		base = c.intlBase
	}

	// v3 UA 路：cn 单 IDE UA；intl 加 CLI UA（不同 UA 下发不同模型集合，取并集）。
	uas := []string{modelDirIDEUA}
	if realm == "intl" {
		uas = append(uas, modelDirCLIUA)
	}
	v3ch := make(chan v3result, len(uas))
	for _, ua := range uas {
		go func(ua string) {
			v3ch <- c.fetchV3Config(base, acct, ua, realm)
		}(ua)
	}
	type entRes struct {
		entries []ModelEntry
		err     error
	}
	entCh := make(chan entRes, 1)
	go func() {
		entries, err := c.fetchEnterprise(base, acct, realm)
		entCh <- entRes{entries, err}
	}()

	// 合并 v3 多 UA：去重，首个命中的条目为准。
	v3map := map[string]*ModelEntry{}
	var promos []ModelPromotion
	var banners []trialBanner
	var v3err error
	for range uas {
		r := <-v3ch
		if r.err != nil {
			v3err = r.err
			continue
		}
		for i := range r.entries {
			e := r.entries[i]
			if _, ok := v3map[e.ID]; !ok {
				cp := e
				v3map[e.ID] = &cp
			}
		}
		promos = append(promos, r.promos...)
		banners = append(banners, r.banners...)
	}

	// 试用横幅：只在此处出现的模型要补入；Credits/Tags 显式清空
	//（那是转正后的计费信息，用在试用版上会误导展示）。
	for _, b := range banners {
		id := strings.TrimSpace(b.ModelID)
		if id == "" {
			continue
		}
		if _, exists := v3map[id]; exists {
			continue
		}
		e := ModelEntry{ID: id, Realm: realm}
		if tgt := strings.TrimSpace(b.TargetModelID); tgt != "" {
			if base, ok := v3map[tgt]; ok {
				e = *base
				e.ID = id
			}
		}
		e.Credits = ""
		e.Tags = nil
		v3map[id] = &e
	}
	// 挂当前生效的限时优惠（牌价 Credits 不动，Promo* 为生效价）。
	// 优惠规则原样缓存（带域标记），生效判断移到展示时（面板前端按当前时间
	// 实时计算），避免抓取时刻把时段类优惠（夜间折扣/错峰使用）写死。
	for i := range promos {
		promos[i].Realm = realm
	}

	// 企业端点只补 v3 缺失的模型。
	er := <-entCh
	if er.err == nil {
		for _, e := range er.entries {
			if _, ok := v3map[e.ID]; !ok {
				cp := e
				v3map[e.ID] = &cp
			}
		}
	}

	if len(v3map) == 0 {
		return nil, nil, fmt.Errorf("模型目录抓取失败: v3=%v enterprise=%v", v3err, er.err)
	}
	out := make([]ModelEntry, 0, len(v3map))
	for _, e := range v3map {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, promos, nil
}

// modelDirCacheData 是缓存文件结构：模型条目 + 优惠原始规则（含时段），
// 生效判断在展示时做（见 PromoActiveAt），缓存本身不写死时段类优惠。
type modelDirCacheData struct {
	FetchedAt time.Time        `json:"fetched_at"`
	Entries   []ModelEntry     `json:"entries"`
	Promos    []ModelPromotion `json:"promos"`
}

// SaveCache 把模型目录原子写到 dir/models.json（0600）。dir 不存在则创建。
func SaveCache(dir string, entries []ModelEntry, promos []ModelPromotion) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(modelDirCacheData{
		FetchedAt: time.Now(),
		Entries:   entries,
		Promos:    promos,
	}, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, modelDirCacheFile+".tmp")
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, modelDirCacheFile))
}

// LoadCache 从 dir/models.json 读缓存，返回条目、优惠规则与抓取时间。
// 兼容旧版无 promos 字段的缓存（promos 为空）。
func LoadCache(dir string) ([]ModelEntry, []ModelPromotion, time.Time, error) {
	data, err := os.ReadFile(filepath.Join(dir, modelDirCacheFile))
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	var cf modelDirCacheData
	if err := json.Unmarshal(data, &cf); err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("模型目录缓存解析失败: %w", err)
	}
	return cf.Entries, cf.Promos, cf.FetchedAt, nil
}
