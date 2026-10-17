// Package config 负责 config.json 的加载与环境变量覆盖。
package config

import (
	"encoding/json"
	"os"
)

// Config 是网关的全部配置。
type Config struct {
	// 服务
	ListenAddr string `json:"listen_addr"` // 默认 ":8788"
	AdminKey   string `json:"admin_key"`   // 管理员 API key（面板/管理接口）

	// 路径
	AuthDir string `json:"auth_dir"` // 默认 "./auths"
	DataDir string `json:"data_dir"` // 默认 "./data"

	// 上游（Phase 1 占位，upstream 模块实现后接）
	UpstreamBase string `json:"upstream_base"`

	// 风控默认（透传给 pool.PoolConfig，0 值用默认）
	MaxConsecutiveFails int `json:"max_consecutive_fails"`
}

// Default 返回默认配置。
func Default() *Config {
	return &Config{
		ListenAddr: ":8788",
		AuthDir:    "./auths",
		DataDir:    "./data",
	}
}

// Load 从 path 读 JSON；文件不存在则返回默认配置。
// 环境变量 WB2API_LISTEN / WB2API_ADMIN_KEY 可覆盖。
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			applyEnv(cfg)
			return cfg, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	applyEnv(cfg)
	return cfg, nil
}

func applyEnv(cfg *Config) {
	if v := os.Getenv("WB2API_LISTEN"); v != "" {
		cfg.ListenAddr = v
	}
	if v := os.Getenv("WB2API_ADMIN_KEY"); v != "" {
		cfg.AdminKey = v
	}
}
