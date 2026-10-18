// 设备授权码登录流程（Web 版账号绑定）。
//
// 上游实现依据（不要编造端点，均来自克隆仓库的实际代码）：
//   - linguo2625469/workbuddy2api-panel/cmd/login/main.go：runURL / runPoll
//   - ardeyouxipianyi/workbuddy2api-hub/wb_accounts.py：AUTH_STATE_PATH 等常量
//
// 流程：
//  1. POST {base}/v2/plugin/auth/state?platform=CLI → {state, authUrl}
//     base: cn=https://copilot.tencent.com, global=https://www.workbuddy.ai
//  2. 用户在浏览器打开 authUrl 完成登录
//  3. GET {base}/v2/plugin/auth/token?state= → 未完成时业务 code!=0；
//     完成时 {accessToken, refreshToken, expiresIn, domain}
//  4. GET {base}/v2/plugin/login/account?state= (+Bearer) → {uid, nickname}
//
// 注意：该上游是"打开链接授权"形态，没有 user_code；API 返回的 device_code
// 即 state，用于轮询关联。
package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"workbuddy2api-prime/internal/auth"
	"workbuddy2api-prime/internal/types"
)

// 上游常量：与 linguo panel cmd/login/main.go 一致。
const (
	upstreamBaseCN     = "https://copilot.tencent.com"
	upstreamBaseGlobal = "https://www.workbuddy.ai"
	originCN           = "https://www.codebuddy.cn"
	originGlobal       = "https://www.workbuddy.ai"
	loginUA            = "CLI/2.63.2 CodeBuddy/2.63.2"
)

// device 会话在服务端的存活时间（上游不返回 expires_in，用我们自己的超时）。
const deviceSessionTTL = 10 * time.Minute

// deviceSession 是一次设备授权流程的服务端会话。
type deviceSession struct {
	state     string
	realm     string // "cn" | "global"
	base      string
	origin    string
	createdAt time.Time
}

// deviceStore 存放进行中的设备授权会话（内存，mutex 保护）。
type deviceStore struct {
	mu       sync.Mutex
	sessions map[string]*deviceSession
}

func newDeviceStore() *deviceStore {
	return &deviceStore{sessions: map[string]*deviceSession{}}
}

func (s *deviceStore) put(code string, sess *deviceSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[code] = sess
}

func (s *deviceStore) get(code string) (*deviceSession, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[code]
	if !ok {
		return nil, false
	}
	if time.Since(sess.createdAt) > deviceSessionTTL {
		delete(s.sessions, code)
		return nil, false
	}
	return sess, true
}

func (s *deviceStore) del(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, code)
}

// realmUpstream 按 realm 返回上游 base 与 Origin。
func realmUpstream(realm string) (base, origin string) {
	if realm == "global" {
		return upstreamBaseGlobal, originGlobal
	}
	return upstreamBaseCN, originCN
}

// upstreamEnvelope 是上游 {code,msg,data} 信封。
type upstreamEnvelope struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// doUpstream 发上游请求并解析信封。code!=0 时返回业务错误（调用方自行区分
// "未完成"与"真失败"——设备流轮询中 code!=0 即"还没授权好"）。
func doUpstream(client *http.Client, method, url, origin, bearer string, body io.Reader) (json.RawMessage, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	req.Header.Set("Origin", origin)
	req.Header.Set("Referer", origin+"/")
	req.Header.Set("User-Agent", loginUA)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("上游请求失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("上游 HTTP %d", resp.StatusCode)
	}
	var env upstreamEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("上游响应解析失败: %w", err)
	}
	if env.Code != 0 {
		return nil, &upstreamBizError{code: env.Code, msg: env.Msg}
	}
	return env.Data, nil
}

// upstreamBizError 是上游业务 code!=0 的错误（区别于网络/HTTP 错误）。
type upstreamBizError struct {
	code int
	msg  string
}

func (e *upstreamBizError) Error() string {
	return fmt.Sprintf("上游业务错误 code=%d msg=%s", e.code, e.msg)
}

// isPending 判断是否为"用户还没完成授权"（业务 code 非 0 即未完成，
// 依据 linguo cmd/login runPoll：pending 时业务 code 非 0）。
func isPending(err error) bool {
	_, ok := err.(*upstreamBizError)
	return ok
}

var httpClient = &http.Client{Timeout: 30 * time.Second}

// startDeviceFlow 发起设备授权：返回授权链接与轮询用的 device_code（即 state）。
func (p *Panel) startDeviceFlow(realm string) (authURL, deviceCode string, err error) {
	if realm != "global" {
		realm = "cn"
	}
	base, origin := realmUpstream(realm)
	data, err := doUpstream(httpClient, http.MethodPost,
		base+"/v2/plugin/auth/state?platform=CLI", origin, "", bytes.NewReader([]byte("{}")))
	if err != nil {
		return "", "", err
	}
	var st struct {
		State   string `json:"state"`
		AuthURL string `json:"authUrl"`
	}
	if err := json.Unmarshal(data, &st); err != nil || st.State == "" || st.AuthURL == "" {
		return "", "", fmt.Errorf("上游未返回 state/authUrl")
	}
	if p.devices == nil {
		p.devices = newDeviceStore()
	}
	p.devices.put(st.State, &deviceSession{
		state: st.State, realm: realm, base: base, origin: origin, createdAt: time.Now(),
	})
	return st.AuthURL, st.State, nil
}

// tokenBundle 是 token 端点返回的凭证包。
type tokenBundle struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	ExpiresIn    int64  `json:"expiresIn"`
	Domain       string `json:"domain"`
}

// pollDeviceFlow 轮询一次。返回 done=false 表示用户还没授权好；
// done=true 时账号已入库（auth.Store），返回账号视图。
func (p *Panel) pollDeviceFlow(deviceCode string) (done bool, acct *types.Account, err error) {
	if p.devices == nil {
		return false, nil, fmt.Errorf("授权会话不存在或已过期")
	}
	sess, ok := p.devices.get(deviceCode)
	if !ok {
		return false, nil, fmt.Errorf("授权会话不存在或已过期")
	}
	data, err := doUpstream(httpClient, http.MethodGet,
		sess.base+"/v2/plugin/auth/token?state="+sess.state, sess.origin, "", nil)
	if err != nil {
		if isPending(err) {
			return false, nil, nil // 还没授权好，不是错误
		}
		return false, nil, err
	}
	var tok tokenBundle
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return false, nil, fmt.Errorf("token 响应异常")
	}
	// 拿 uid/nickname（带 Bearer，依据 linguo runPoll）。
	uid, nickname := "", ""
	acctData, err := doUpstream(httpClient, http.MethodGet,
		sess.base+"/v2/plugin/login/account?state="+sess.state, sess.origin, tok.AccessToken, nil)
	if err == nil {
		var a struct {
			UID      string `json:"uid"`
			Nickname string `json:"nickname"`
		}
		if json.Unmarshal(acctData, &a) == nil {
			uid, nickname = a.UID, a.Nickname
		}
	}
	if uid == "" {
		return false, nil, fmt.Errorf("已授权但未取到 uid，请重试")
	}
	_ = nickname
	region := "cn"
	if sess.realm == "global" {
		region = "intl"
	}
	account := &types.Account{
		ID:                uid,
		UID:               uid,
		Region:            region,
		AccessToken:       tok.AccessToken,
		RefreshToken:      tok.RefreshToken,
		DeviceFingerprint: auth.DeriveFingerprint(uid),
		Weight:            1,
		UpdatedAt:         time.Now(),
	}
	if tok.ExpiresIn > 0 {
		account.ExpiresAt = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
	}
	if p.auth == nil {
		return false, nil, fmt.Errorf("auth store 未初始化")
	}
	if err := p.auth.Save(account); err != nil {
		return false, nil, err
	}
	p.devices.del(deviceCode)
	// 余额拉取与进池由调用方（handleDevicePoll）经 enrichNewAccount 完成，
	// 以便把 balance_error 带回给前端。
	return true, account, nil
}

// enrichNewAccount 新账号入库后的增强：拉余额写回 + 推进 pool。
// 余额拉取失败只记录，不返回错误（不阻塞登录/导入流程）。
func (p *Panel) enrichNewAccount(account *types.Account) (balanceErr string) {
	if p.billing != nil {
		if _, _, err := p.refreshBalanceLocked(account); err != nil {
			balanceErr = err.Error()
		}
	}
	if p.pool != nil {
		p.pool.AddAccount(account)
	}
	return balanceErr
}

// importToken 粘贴 token 入库（设备流不可用时的备选）。
// uid 为空时尝试用 hub 的 /console/account 端点（仅 Bearer）反查，
// 依据 hub wb_accounts.py fetch_account_profile。
func (p *Panel) importToken(accessToken, refreshToken, uid, region string) (*types.Account, error) {
	if accessToken == "" {
		return nil, fmt.Errorf("access_token 不能为空")
	}
	if region == "global" || region == "intl" {
		region = "intl"
	} else {
		region = "cn"
	}
	if uid == "" {
		// 尝试用 token 反查 uid（hub 做法：GET {origin}/console/account + Bearer）。
		_, origin := realmUpstream(mapRegionToRealm(region))
		req, _ := http.NewRequest(http.MethodGet, origin+"/console/account", nil)
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("Accept", "application/json, text/plain, */*")
		req.Header.Set("User-Agent", loginUA)
		resp, err := httpClient.Do(req)
		if err == nil {
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			var prof struct {
				UID      string `json:"uid"`
				Nickname string `json:"nickname"`
			}
			if json.Unmarshal(raw, &prof) == nil && prof.UID != "" {
				uid = prof.UID
			}
		}
	}
	if uid == "" {
		return nil, fmt.Errorf("未能确定 uid：请在粘贴内容里带上 uid，或改用设备授权方式")
	}
	account := &types.Account{
		ID:                uid,
		UID:               uid,
		Region:            region,
		AccessToken:       accessToken,
		RefreshToken:      refreshToken,
		DeviceFingerprint: auth.DeriveFingerprint(uid),
		Weight:            1,
		UpdatedAt:         time.Now(),
	}
	if p.auth == nil {
		return nil, fmt.Errorf("auth store 未初始化")
	}
	if err := p.auth.Save(account); err != nil {
		return nil, err
	}
	// 与设备授权一致：立即拉余额 + 推进 pool（余额失败不阻塞导入）。
	p.enrichNewAccount(account)
	return account, nil
}

// mapRegionToRealm 把 region 映射回 realm（仅用于选 origin）。
func mapRegionToRealm(region string) string {
	if region == "intl" {
		return "global"
	}
	return "cn"
}

// handleDeviceStart: POST /panel/api/accounts/device/start {realm}
func (p *Panel) handleDeviceStart(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Realm string `json:"realm"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // 空 body 也允许，默认 cn
	authURL, deviceCode, err := p.startDeviceFlow(body.Realm)
	if err != nil {
		writeError(w, http.StatusBadGateway, "发起授权失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"auth_url":    authURL,
		"device_code": deviceCode,
		"expires_in":  int(deviceSessionTTL.Seconds()),
	})
}

// handleDevicePoll: GET /panel/api/accounts/device/poll?device_code=
func (p *Panel) handleDevicePoll(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("device_code")
	if code == "" {
		writeError(w, http.StatusBadRequest, "缺少 device_code")
		return
	}
	done, acct, err := p.pollDeviceFlow(code)
	if err != nil {
		writeError(w, http.StatusBadGateway, "轮询失败: "+err.Error())
		return
	}
	if !done {
		writeJSON(w, map[string]any{"done": false})
		return
	}
	resp := map[string]any{"done": true, "account": map[string]string{
		"id": acct.ID, "uid": acct.UID, "region": acct.Region,
	}}
	if berr := p.enrichNewAccount(acct); berr != "" {
		resp["balance_error"] = berr
	} else {
		resp["credits"] = acct.CreditTotal
	}
	writeJSON(w, resp)
}

// handleAccountImport: POST /panel/api/accounts/import
// {access_token, refresh_token?, uid?, region?}
func (p *Panel) handleAccountImport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		UID          string `json:"uid"`
		Region       string `json:"region"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	acct, err := p.importToken(body.AccessToken, body.RefreshToken, body.UID, body.Region)
	if err != nil {
		writeError(w, http.StatusBadRequest, "导入失败: "+err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true, "account": map[string]string{
		"id": acct.ID, "uid": acct.UID, "region": acct.Region,
	}})
}
