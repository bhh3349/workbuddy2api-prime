package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"workbuddy2api-prime/internal/types"
)

// mockModelDirServer 起一个模拟上游模型目录的 httptest 服务器。
// v3cfg：/v3/config 的响应体；enterprise：企业端点响应体；v2status：/v2 路径状态码（404 测 fallback）。
func mockModelDirServer(t *testing.T, v3cfg string, enterprise string, v2status int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/v3/config", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(v3cfg))
	})
	mux.HandleFunc("/v2/enterprises/personal/models", func(w http.ResponseWriter, r *http.Request) {
		if v2status != 200 {
			w.WriteHeader(v2status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(enterprise))
	})
	mux.HandleFunc("/console/enterprises/personal/models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(enterprise))
	})
	return httptest.NewServer(mux)
}

func testClient(srv *httptest.Server) *ModelDirClient {
	c := NewModelDirClient()
	c.cnBase = srv.URL
	c.intlBase = srv.URL
	return c
}

func testAcct(region string) *types.Account {
	return &types.Account{ID: "u1", UID: "u1", Region: region, AccessToken: "tok"}
}

const v3cfgBasic = `{"code":0,"data":{"models":[
	{"id":"model-a","name":"A","descriptionZh":"模型A","credits":"x0.05","tags":["badge:限时免费"],"maxOutputTokens":8192,"supportsImages":true,"supportsReasoning":true},
	{"id":"model-b","name":"B","credits":"x0.29","tags":[],"maxOutputTokens":8192},
	{"id":"nes-embed","name":"Emb","credits":"x0.01","maxOutputTokens":8192},
	{"id":"tiny-out","name":"Tiny","credits":"x0.01","maxOutputTokens":128}
],"modelPromotions":[
	{"enabled":true,"priority":100,"modelIds":["model-a"],"badge":{"label":"限时免费"},
	 "discount":{"discountedCredits":"0x","factor":0},"hover":{"textZh":"限时免费说明"}}
]}}`

const enterpriseBasic = `{"code":0,"data":{
	"models":[
		{"id":"model-b","name":"B2","credits":"x0.29","maxOutputTokens":8192},
		{"id":"model-c","name":"C","credits":"x1.20","maxOutputTokens":8192},
		{"id":"model-d","name":"D","credits":"x0.10","maxOutputTokens":8192,"disabled":true}
	],
	"agents":[{"name":"cli","models":["model-b","model-c","model-d"]}]
}}`

// TestFetchModelDirectory_Merge 验证两路合并去重：v3 为主，企业端点补缺；
// nonChatModel（nes- 前缀、maxOutputTokens≤256）被过滤；disabled 被剔除。
func TestFetchModelDirectory_Merge(t *testing.T) {
	srv := mockModelDirServer(t, v3cfgBasic, enterpriseBasic, 200)
	defer srv.Close()

	got, err := testClient(srv).FetchModelDirectory(testAcct("cn"))
	if err != nil {
		t.Fatalf("FetchModelDirectory: %v", err)
	}
	ids := map[string]ModelEntry{}
	for _, e := range got {
		ids[e.ID] = e
	}
	// 期望：a（v3 限时免费）、b（v3 优先，Description 取 v3 的"模型A"口径）、c（企业端点补入）
	for _, want := range []string{"model-a", "model-b", "model-c"} {
		if _, ok := ids[want]; !ok {
			t.Errorf("缺失模型 %s，实际 %v", want, keys(ids))
		}
	}
	if _, ok := ids["nes-embed"]; ok {
		t.Error("nes-embed 应被 nonChatModel 过滤")
	}
	if _, ok := ids["tiny-out"]; ok {
		t.Error("tiny-out（maxOutputTokens≤256）应被过滤")
	}
	if _, ok := ids["model-d"]; ok {
		t.Error("model-d（disabled）应被剔除")
	}
	if got[0].Realm != "cn" {
		t.Errorf("Realm 应为 cn，实际 %q", got[0].Realm)
	}
	// b 来自 v3（Description 为 v3 的值，而非企业端的空值）
	if ids["model-b"].Description != "" && ids["model-b"].Name != "B" {
		t.Errorf("model-b 应取 v3 条目，实际 %+v", ids["model-b"])
	}
}

// TestFetchModelDirectory_Promo 验证优惠解析：factor=0 → 限时免费；
// 过期 schedule 的优惠不生效。
func TestFetchModelDirectory_Promo(t *testing.T) {
	v3 := `{"code":0,"data":{"models":[
		{"id":"hy3","name":"Hy3","credits":"x0.10","maxOutputTokens":8192},
		{"id":"glm-x","name":"Glm","credits":"x0.50","maxOutputTokens":8192}
	],"modelPromotions":[
		{"enabled":true,"priority":100,"modelIds":["hy3"],"badge":{"label":"限时免费"},
		 "discount":{"discountedCredits":"0x","factor":0},"hover":{"textZh":"免费中"}},
		{"enabled":true,"priority":50,"modelIds":["glm-x"],"badge":{"label":"夜间五折"},
		 "discount":{"discountedCredits":"0.25x","factor":0.5},
		 "schedule":{"daily":[{"start":"23:00","end":"07:00"}],"validUntil":"2020-01-01T00:00:00Z"}}
	]}}`
	srv := mockModelDirServer(t, v3, `{"code":0,"data":{"models":[],"agents":[{"name":"cli","models":[]}]}}`, 200)
	defer srv.Close()

	got, err := testClient(srv).FetchModelDirectory(testAcct("cn"))
	if err != nil {
		t.Fatalf("FetchModelDirectory: %v", err)
	}
	byID := map[string]ModelEntry{}
	for _, e := range got {
		byID[e.ID] = e
	}
	hy3 := byID["hy3"]
	if hy3.PromoFactor == nil || *hy3.PromoFactor != 0 {
		t.Errorf("hy3 PromoFactor 应为 0（限时免费），实际 %+v", hy3.PromoFactor)
	}
	if hy3.PromoLabel != "限时免费" || hy3.PromoCredits != "0x" || hy3.PromoNote != "免费中" {
		t.Errorf("hy3 优惠字段不对：%+v", hy3)
	}
	// 过期 schedule 的优惠不应挂上
	if glm := byID["glm-x"]; glm.PromoFactor != nil || glm.PromoLabel != "" {
		t.Errorf("过期优惠不应生效，实际 %+v", glm)
	}
}

// TestFetchModelDirectory_TrialBanner 验证试用横幅补入：Credits/Tags 清空。
func TestFetchModelDirectory_TrialBanner(t *testing.T) {
	v3 := `{"code":0,"data":{"models":[
		{"id":"hy4-preview","name":"Hy4","credits":"x0.29","tags":["badge:热门"],"maxOutputTokens":8192,"supportsImages":true}
	],
	"productFeaturesConfig":{"ModelTrialBanner":{"banners":[
		{"modelId":"hy4-preview-f","targetModelId":"hy4-preview"}
	]}}}}`
	srv := mockModelDirServer(t, v3, `{"code":0,"data":{"models":[]}}`, 200)
	defer srv.Close()

	got, err := testClient(srv).FetchModelDirectory(testAcct("cn"))
	if err != nil {
		t.Fatalf("FetchModelDirectory: %v", err)
	}
	byID := map[string]ModelEntry{}
	for _, e := range got {
		byID[e.ID] = e
	}
	f, ok := byID["hy4-preview-f"]
	if !ok {
		t.Fatalf("试用横幅模型 hy4-preview-f 应被补入，实际 %v", keys(byID))
	}
	if f.Credits != "" || len(f.Tags) != 0 {
		t.Errorf("试用版 Credits/Tags 应清空，实际 credits=%q tags=%v", f.Credits, f.Tags)
	}
	if !f.SupportsImages {
		t.Error("试用版应继承目标模型的能力字段（SupportsImages）")
	}
}

// TestFetchModelDirectory_IntlFallback 验证 intl 企业端点 /v2 404 时 fallback 到 /console。
func TestFetchModelDirectory_IntlFallback(t *testing.T) {
	srv := mockModelDirServer(t,
		`{"code":0,"data":{"models":[{"id":"m1","name":"M1","credits":"x0.05","maxOutputTokens":8192}]}}`,
		`{"code":0,"data":{"models":[{"id":"m2","name":"M2","credits":"x0.20","maxOutputTokens":8192}]}}`,
		404)
	defer srv.Close()

	got, err := testClient(srv).FetchModelDirectory(testAcct("intl"))
	if err != nil {
		t.Fatalf("FetchModelDirectory: %v", err)
	}
	ids := map[string]bool{}
	for _, e := range got {
		ids[e.ID] = true
		if e.Realm != "intl" {
			t.Errorf("Realm 应为 intl，实际 %q", e.Realm)
		}
	}
	if !ids["m1"] || !ids["m2"] {
		t.Errorf("m1（v3）与 m2（企业端点 fallback）都应在，实际 %v", keys2(ids))
	}
}

// TestFetchModelDirectory_AllFail 验证两路全失败时返回 error（不 panic）。
func TestFetchModelDirectory_AllFail(t *testing.T) {
	srv := mockModelDirServer(t, `{"code":500,"msg":"boom"}`, `{"code":500}`, 500)
	defer srv.Close()

	_, err := testClient(srv).FetchModelDirectory(testAcct("cn"))
	if err == nil {
		t.Error("两路全失败应返回 error")
	}
}

// TestPromoActive_Schedule 验证 daily 窗口与跨午夜逻辑。
func TestPromoActive_Schedule(t *testing.T) {
	mk := func(start, end string) *modelPromotion {
		p := &modelPromotion{Enabled: true}
		p.Schedule = &struct {
			Daily []struct {
				Start string `json:"start"`
				End   string `json:"end"`
			} `json:"daily"`
			ValidFrom  string `json:"validFrom"`
			ValidUntil string `json:"validUntil"`
		}{Daily: []struct {
			Start string `json:"start"`
			End   string `json:"end"`
		}{{Start: start, End: end}}}
		return p
	}
	// 固定一个北京时间 2026-10-18 02:00（落在 23:00→07:00 跨午夜窗口内）
	now := time.Date(2026, 10, 18, 2, 0, 0, 0, promoZone)
	if !promoActive(mk("23:00", "07:00"), now) {
		t.Error("02:00 应落在 23:00→07:00 窗口内")
	}
	if promoActive(mk("09:00", "18:00"), now) {
		t.Error("02:00 不应在 09:00→18:00 窗口内")
	}
	if promoActive(&modelPromotion{Enabled: false}, now) {
		t.Error("disabled 的优惠不应生效")
	}
}

// TestCacheRoundTrip 验证缓存写读 round-trip。
func TestCacheRoundTrip(t *testing.T) {
	dir := t.TempDir()
	f := 0.0
	entries := []ModelEntry{
		{ID: "hy3", Name: "Hy3", Realm: "cn", Credits: "x0.10", PromoFactor: &f, PromoLabel: "限时免费", Tags: []string{"badge:限时免费"}},
		{ID: "gpt-x", Name: "GX", Realm: "cn", Credits: "x1.20"},
	}
	if err := SaveCache(dir, entries); err != nil {
		t.Fatalf("SaveCache: %v", err)
	}
	// 文件权限应为 0600
	fi, err := os.Stat(filepath.Join(dir, modelDirCacheFile))
	if err != nil {
		t.Fatalf("缓存文件不存在: %v", err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("缓存文件权限应为 0600，实际 %o", fi.Mode().Perm())
	}
	got, at, err := LoadCache(dir)
	if err != nil {
		t.Fatalf("LoadCache: %v", err)
	}
	if time.Since(at) > time.Minute {
		t.Errorf("FetchedAt 应为刚才，实际 %v", at)
	}
	if len(got) != 2 || got[0].ID != "hy3" || got[0].PromoFactor == nil || *got[0].PromoFactor != 0 {
		t.Errorf("round-trip 数据不对：%+v", got)
	}
	// 坏 JSON 应返回错误而非 panic
	os.WriteFile(filepath.Join(dir, modelDirCacheFile), []byte("{bad"), 0600)
	if _, _, err := LoadCache(dir); err == nil {
		t.Error("坏缓存应返回 error")
	}
	// 不存在目录读缓存应返回 error
	if _, _, err := LoadCache(filepath.Join(dir, "nope")); err == nil {
		t.Error("缺失缓存应返回 error")
	}
}

// TestCacheJSONShape 验证缓存文件 JSON 结构稳定（含 fetched_at）。
func TestCacheJSONShape(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCache(dir, []ModelEntry{{ID: "a", Realm: "cn"}}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, modelDirCacheFile))
	var cf struct {
		FetchedAt string       `json:"fetched_at"`
		Entries   []ModelEntry `json:"entries"`
	}
	if err := json.Unmarshal(raw, &cf); err != nil {
		t.Fatalf("缓存 JSON 解析失败: %v", err)
	}
	if cf.FetchedAt == "" || len(cf.Entries) != 1 {
		t.Errorf("缓存结构不对: %s", raw)
	}
}

func keys(m map[string]ModelEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func keys2(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
