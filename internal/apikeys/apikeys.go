// Package apikeys 实现 wbk_ 子钥匙分发：签发、两段式校验、额度扣减、
// IP 白名单、模型白名单。移植自 panel-plus 的 internal/apikeys 设计
//（分析文档 04-workbuddy2api-panel-plus-jacky.md §4.1）。
//
// 安全要点（与 panel-plus 一致）：
//   - 服务端只存明文的 sha256 摘要，明文仅在创建时返回一次；
//   - `wbk_` 前缀优先命中子密钥库，不再比管理员 key；
//   - 落盘 data/keys.json 经 tmp+rename 原子写，文件 0600、目录 0700。
package apikeys

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"workbuddy2api-prime/internal/types"
)

const (
	// SubKeyPrefix 是子钥匙明文前缀。命中它的 token 一律走子密钥库校验，
	// 不再与管理员 api_key 比对；未命中则不由本包处理。
	SubKeyPrefix = "wbk_"

	// fileName 是子钥匙库的落盘文件名。
	fileName = "keys.json"
	// fileVersion 是落盘格式版本。
	fileVersion = 1
)

// AuthError 是鉴权失败错误，携带 HTTP 状态码与机器可读错误码，
// 供 main.go 的 withAuth 直接映射成 HTTP 响应。
type AuthError struct {
	Status int    // HTTP 状态码
	Code   string // 如 key_disabled、quota_exhausted
	Msg    string // 给人看的中文说明
}

func (e *AuthError) Error() string { return e.Code + ": " + e.Msg }

// 错误码口径（对齐 panel-plus §4.1）：
//   - 停用/过期 → 403；
//   - 配额用尽 → 429（避免客户端误读成"密钥无效"）；
//   - IP/模型 → 400。
var (
	ErrKeyInvalid      = &AuthError{Status: 401, Code: "key_invalid", Msg: "API key 无效"}
	ErrKeyDisabled     = &AuthError{Status: 403, Code: "key_disabled", Msg: "API key 已停用"}
	ErrKeyExpired      = &AuthError{Status: 403, Code: "key_expired", Msg: "API key 已过期"}
	ErrQuotaExhausted  = &AuthError{Status: 429, Code: "quota_exhausted", Msg: "API key 配额已用尽"}
	ErrIPNotAllowed    = &AuthError{Status: 400, Code: "ip_not_allowed", Msg: "当前 IP 不在白名单内"}
	ErrModelNotAllowed = &AuthError{Status: 400, Code: "model_not_allowed", Msg: "该 key 不允许调用此模型"}
)

// persisted 是 keys.json 的落盘结构。
type persisted struct {
	Version int             `json:"version"`
	Keys    []*types.APIKey `json:"keys"`
}

// Store 是子钥匙库：内存索引 + 落盘，并发安全。
type Store struct {
	mu    sync.RWMutex
	path  string
	keys  map[string]*types.APIKey // id → key
	index map[string]*types.APIKey // sha256(明文) → key，O(1) 查表
}

// NewStore 打开（不存在则创建）dataDir 下的子钥匙库。
func NewStore(dataDir string) (*Store, error) {
	// 目录 0700：0600 的目录连属主自己都进不去，功能会全废；
	// 0700 仅属主可访问，权限意图与"仅自己可见"一致（与 auth 模块同例）。
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("apikeys: 创建目录失败: %w", err)
	}
	_ = os.Chmod(dataDir, 0o700) // 已有目录也收紧
	s := &Store{
		path:  filepath.Join(dataDir, fileName),
		keys:  make(map[string]*types.APIKey),
		index: make(map[string]*types.APIKey),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// load 从落盘恢复；文件不存在视为新库。
func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("apikeys: 读取落盘失败: %w", err)
	}
	var p persisted
	if err := json.Unmarshal(data, &p); err != nil {
		return fmt.Errorf("apikeys: 解析落盘失败: %w", err)
	}
	for _, k := range p.Keys {
		if k == nil || k.ID == "" || k.Hash == "" {
			continue
		}
		s.keys[k.ID] = k
		s.index[k.Hash] = k
	}
	return nil
}

// flush 把内存状态原子落盘。调用者必须持有写锁。
func (s *Store) flush() error {
	keys := make([]*types.APIKey, 0, len(s.keys))
	for _, k := range s.keys {
		keys = append(keys, k)
	}
	data, err := json.MarshalIndent(persisted{Version: fileVersion, Keys: keys}, "", "  ")
	if err != nil {
		return fmt.Errorf("apikeys: 序列化失败: %w", err)
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("apikeys: 写临时文件失败: %w", err)
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("apikeys: 原子替换失败: %w", err)
	}
	return nil
}

// hashOf 计算 token 的 sha256 hex 摘要。服务端只存这个，不存明文。
func hashOf(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// randHex 生成 n 个随机字节的 hex 字符串。
func randHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("apikeys: 随机数失败: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// IsSubKey 判断 token 是否为子钥匙（wbk_ 前缀）。
// main.go 的 withAuth 用它决定走子密钥库还是管理员 key：
// 前缀命中 → 只查子密钥库，不再比管理员 key；未命中 → 走管理员 key。
func IsSubKey(token string) bool {
	return strings.HasPrefix(token, SubKeyPrefix)
}

// Create 签发一把子钥匙。明文 = wbk_ + 24 字节随机 hex，
// 只在返回值里出现一次；服务端只存 sha256 摘要。
// 新钥匙默认：不限配额（Quota=-1）、无 IP 限制、模型全放行。
func (s *Store) Create(name string) (plain string, key *types.APIKey, err error) {
	if strings.TrimSpace(name) == "" {
		return "", nil, errors.New("apikeys: 名称不能为空")
	}
	raw, err := randHex(24) // 24 字节 → 48 hex 字符
	if err != nil {
		return "", nil, err
	}
	idRaw, err := randHex(8) // 16 hex 字符，做 key ID
	if err != nil {
		return "", nil, err
	}
	plain = SubKeyPrefix + raw
	key = &types.APIKey{
		ID:        SubKeyPrefix + idRaw,
		Hash:      hashOf(plain),
		Name:      name,
		Quota:     -1, // -1 = 不限
		CreatedAt: time.Now(),
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.keys[key.ID]; dup {
		return "", nil, errors.New("apikeys: ID 碰撞，请重试")
	}
	s.keys[key.ID] = key
	s.index[key.Hash] = key
	if err := s.flush(); err != nil {
		delete(s.keys, key.ID)
		delete(s.index, key.Hash)
		return "", nil, err
	}
	return plain, key, nil
}

// Verify 是第一段校验（读 body 前）：前缀→Hash 查表→停用→过期→配额→IP 白名单。
// 非 wbk_ 前缀的 token 返回 (nil, nil)，表示"不由本包处理"，交给管理员 key。
// 返回的 *types.APIKey 指向内部状态，调用方不要直接修改；改配额请走 Consume。
func (s *Store) Verify(token, ip string) (*types.APIKey, error) {
	if !IsSubKey(token) {
		return nil, nil
	}
	h := hashOf(token)

	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.index[h]
	if !ok {
		return nil, ErrKeyInvalid
	}
	if k.Disabled {
		return nil, ErrKeyDisabled
	}
	// ExpiresAt 零值 = 永不过期（与 panel-plus 一致）。
	if !k.ExpiresAt.IsZero() && time.Now().After(k.ExpiresAt) {
		return nil, ErrKeyExpired
	}
	// Quota < 0 = 不限。
	if k.Quota >= 0 && k.Used >= k.Quota {
		return nil, ErrQuotaExhausted
	}
	if len(k.IPAllow) > 0 && !ipAllowed(ip, k.IPAllow) {
		return nil, ErrIPNotAllowed
	}
	return k, nil
}

// VerifyRequest 是第二段校验（读 body 后）：模型白名单。
// Models 为空 = 全部允许。比对时去掉 "cn:" 前缀；
// 白名单条目以 ":" 结尾（如 "cn:"）时按前缀通配。
func (s *Store) VerifyRequest(k *types.APIKey, model string) error {
	if k == nil {
		return ErrKeyInvalid
	}
	if len(k.Models) == 0 {
		return nil
	}
	norm := func(v string) string { return strings.TrimPrefix(v, "cn:") }
	m := norm(model)
	for _, w := range k.Models {
		// 条目以 ":" 结尾（如 "cn:"）时按前缀通配：同时对原始 model
		//（含 cn: 前缀）和规范化后的 m 比对，避免 "" 前缀恒匹配。
		if strings.HasSuffix(w, ":") {
			p := strings.TrimSuffix(w, ":")
			if strings.HasPrefix(model, p) || strings.HasPrefix(m, strings.TrimPrefix(p, "cn:")) {
				return nil
			}
			continue
		}
		if norm(w) == m {
			return nil
		}
	}
	return ErrModelNotAllowed
}

// Consume 扣减额度。调用点：流式/非流式成功时（用上游返回的真实 credit），
// 以及空回复降级重试前（先记账，防"换模型重试白嫖上游消耗"）。
// types.APIKey 只有一对 Quota/Used：credit 与 tokens 按同一单位累计。
func (s *Store) Consume(id string, credit, tokens float64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return fmt.Errorf("apikeys: 未知 key id %q", id)
	}
	k.Used += credit + tokens
	return s.flush()
}

// Get 按 ID 取钥匙（返回内部指针，不要直接修改）。
func (s *Store) Get(id string) (*types.APIKey, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.keys[id]
	return k, ok
}

// List 返回全部钥匙（供 Phase 3 面板用）。
func (s *Store) List() []*types.APIKey {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*types.APIKey, 0, len(s.keys))
	for _, k := range s.keys {
		out = append(out, k)
	}
	return out
}

// Delete 删除一把钥匙。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	k, ok := s.keys[id]
	if !ok {
		return fmt.Errorf("apikeys: 未知 key id %q", id)
	}
	delete(s.keys, id)
	delete(s.index, k.Hash)
	return s.flush()
}

// ipAllowed 判断 ip 是否命中白名单。白名单条目支持单个 IP 或 CIDR。
// TrustProxy 说明：默认只认 TCP 对端 IP；只有显式声明可信反代后才可读 XFF，
// 否则伪造 XFF 可绕过白名单（panel-plus §4.1 实锤的安全问题）。
func ipAllowed(ip string, allow []string) bool {
	host := ip
	if h, _, err := net.SplitHostPort(ip); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(strings.TrimSpace(host))
	if err != nil {
		return false
	}
	// 去掉 IPv6 zone 等后缀影响比对，统一用无 zone 形式。
	addr = addr.Unmap()
	for _, entry := range allow {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if strings.Contains(entry, "/") {
			prefix, err := netip.ParsePrefix(entry)
			if err != nil {
				continue
			}
			if prefix.Contains(addr) {
				return true
			}
			continue
		}
		if other, err := netip.ParseAddr(entry); err == nil && other.Unmap() == addr {
			return true
		}
	}
	return false
}
