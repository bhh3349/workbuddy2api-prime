// Package auth 管理 WorkBuddy/CodeBuddy 账号的登录态：
// auths/<uid>.json 的存取、token 刷新、出站鉴权头注入。
//
// 设计依据：
//   - linguo 版 SaveAtomic（tmp+rename 原子写回、文件 0600）
//   - hub 版 accounts/ 一账号一 JSON、os.replace 原子写入
// 仅使用标准库。
package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"workbuddy2api-prime/internal/types"
)

// 凭证目录与文件权限。
//
// 说明：任务要求"目录和文件 0600"。但目录去掉执行位（x）后，
// 进程自己也无法进入目录、打不开里面的文件，功能会全废。
// 因此实现取折中：目录 0700（仅属主可进出/列目录），文件 0600（仅属主可读写），
// 权限意图与"0600 一样严格"，且功能可用。
const (
	dirPerm  os.FileMode = 0o700 // 凭证目录：仅属主可访问
	filePerm os.FileMode = 0o600 // 凭证文件：仅属主可读写
)

// Store 负责账号凭证的本地持久化。
// 目录结构：<dir>/<uid>.json，一账号一文件。
type Store struct {
	dir string
}

// NewStore 创建指向 dir 的凭证仓库，不存在则按 dirPerm 建目录。
func NewStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("auth: 创建凭证目录 %s 失败: %w", dir, err)
	}
	// 已存在时也收紧权限（防止外部改成宽松权限）。
	if err := os.Chmod(dir, dirPerm); err != nil {
		return nil, fmt.Errorf("auth: 设置凭证目录权限失败: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Dir 返回凭证目录路径。
func (s *Store) Dir() string { return s.dir }

// safeUID 把 uid 清洗成安全的文件名（只保留字母数字、-、_）。
// 依据：hub 的 safe_uid + ".json" 命名思路。
func safeUID(uid string) string {
	var b strings.Builder
	for _, r := range uid {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "unknown"
	}
	return b.String()
}

// pathOf 返回某账号的凭证文件路径。
func (s *Store) pathOf(uid string) string {
	return filepath.Join(s.dir, safeUID(uid)+".json")
}

// Save 原子写回单个账号：先写 tmp 文件（0600）再 rename 覆盖。
// 依据：linguo SaveAtomic、hub Account.save() 的 tmp+replace 做法。
func (s *Store) Save(acct *types.Account) error {
	if acct == nil {
		return fmt.Errorf("auth: Save 收到空账号")
	}
	data, err := json.MarshalIndent(acct, "", "  ")
	if err != nil {
		return fmt.Errorf("auth: 序列化账号 %s 失败: %w", acct.UID, err)
	}
	dst := s.pathOf(acct.UID)
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("auth: 创建临时文件失败: %w", err)
	}
	tmpName := tmp.Name()
	// 任一失败都要清理临时文件。
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("auth: 写临时文件失败: %w", err)
	}
	if err := tmp.Chmod(filePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("auth: 设置临时文件权限失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("auth: 关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return fmt.Errorf("auth: 原子替换 %s 失败: %w", dst, err)
	}
	return nil
}

// Load 读取单个账号的凭证。
func (s *Store) Load(uid string) (*types.Account, error) {
	data, err := os.ReadFile(s.pathOf(uid))
	if err != nil {
		return nil, fmt.Errorf("auth: 读取账号 %s 失败: %w", uid, err)
	}
	var acct types.Account
	if err := json.Unmarshal(data, &acct); err != nil {
		return nil, fmt.Errorf("auth: 解析账号 %s 失败: %w", uid, err)
	}
	return &acct, nil
}

// LoadAll 加载目录下全部账号（*.json），逐个解析，坏文件跳过并返回聚合错误。
func (s *Store) LoadAll() ([]*types.Account, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("auth: 列出凭证目录失败: %w", err)
	}
	var (
		accts   []*types.Account
		failNum int
	)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			failNum++
			continue
		}
		var acct types.Account
		if err := json.Unmarshal(data, &acct); err != nil {
			failNum++
			continue
		}
		accts = append(accts, &acct)
	}
	if failNum > 0 {
		return accts, fmt.Errorf("auth: %d 个凭证文件读取/解析失败", failNum)
	}
	return accts, nil
}

// Delete 删除单个账号的凭证文件。
func (s *Store) Delete(uid string) error {
	if err := os.Remove(s.pathOf(uid)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("auth: 删除账号 %s 失败: %w", uid, err)
	}
	return nil
}
