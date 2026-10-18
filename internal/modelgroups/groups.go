// Package modelgroups 提供自定义模型分组的存储。
// 用户在面板创建分组（命名 + 勾选模型），子钥匙绑定分组后，
// /v1/models 只返回该组内的模型。
package modelgroups

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Group 是自定义模型分组。
type Group struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Models    []string `json:"models"` // 模型 ID 列表
	CreatedAt time.Time `json:"created_at"`
}

// Store 是分组存储（JSON 文件，0600）。
type Store struct {
	mu     sync.RWMutex
	path   string
	groups map[string]*Group // id -> group
	order  []string          // 创建顺序
}

// NewStore 打开 dataDir 下的分组库。
func NewStore(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("modelgroups: 创建目录失败: %w", err)
	}
	s := &Store{
		path:   filepath.Join(dataDir, "model_groups.json"),
		groups: map[string]*Group{},
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) load() error {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("modelgroups: 读取失败: %w", err)
	}
	var saved struct {
		Groups []*Group `json:"groups"`
		Order  []string `json:"order"`
	}
	if err := json.Unmarshal(data, &saved); err != nil {
		return fmt.Errorf("modelgroups: 解析失败: %w", err)
	}
	for _, g := range saved.Groups {
		s.groups[g.ID] = g
	}
	s.order = saved.Order
	// 补齐缺失的 order（防御旧文件）
	for id := range s.groups {
		found := false
		for _, oid := range s.order {
			if oid == id {
				found = true
				break
			}
		}
		if !found {
			s.order = append(s.order, id)
		}
	}
	return nil
}

func (s *Store) save() error {
	saved := struct {
		Groups []*Group `json:"groups"`
		Order  []string `json:"order"`
	}{}
	for _, id := range s.order {
		if g, ok := s.groups[id]; ok {
			saved.Groups = append(saved.Groups, g)
		}
	}
	data, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(s.path, data, 0o600)
}

// List 返回所有分组（按创建顺序）。
func (s *Store) List() []*Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Group, 0, len(s.order))
	for _, id := range s.order {
		if g, ok := s.groups[id]; ok {
			c := *g
			c.Models = append([]string(nil), g.Models...)
			out = append(out, &c)
		}
	}
	return out
}

// Get 返回指定分组（不存在返回 nil）。
func (s *Store) Get(id string) *Group {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if g, ok := s.groups[id]; ok {
		c := *g
		c.Models = append([]string(nil), g.Models...)
		return &c
	}
	return nil
}

// Create 创建分组，返回新分组。
func (s *Store) Create(name string, models []string) (*Group, error) {
	if name == "" {
		return nil, fmt.Errorf("分组名不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := fmt.Sprintf("mg_%d", time.Now().UnixNano())
	g := &Group{
		ID:        id,
		Name:      name,
		Models:    append([]string(nil), models...),
		CreatedAt: time.Now(),
	}
	s.groups[id] = g
	s.order = append(s.order, id)
	if err := s.save(); err != nil {
		delete(s.groups, id)
		s.order = s.order[:len(s.order)-1]
		return nil, err
	}
	c := *g
	return &c, nil
}

// Update 更新分组的名称和模型列表。
func (s *Store) Update(id, name string, models []string) (*Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.groups[id]
	if !ok {
		return nil, fmt.Errorf("分组不存在")
	}
	if name != "" {
		g.Name = name
	}
	g.Models = append([]string(nil), models...)
	if err := s.save(); err != nil {
		return nil, err
	}
	c := *g
	return &c, nil
}

// Delete 删除分组。
func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.groups[id]; !ok {
		return fmt.Errorf("分组不存在")
	}
	delete(s.groups, id)
	for i, oid := range s.order {
		if oid == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	return s.save()
}
