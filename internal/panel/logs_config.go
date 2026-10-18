package panel

// 运行日志与配置视图的后端接口。

import (
	"encoding/json"
	"net/http"
	"strconv"

	"workbuddy2api-prime/internal/logbuf"
)

// handleLogs: GET /panel/api/logs?limit=200&ch=all|task|chat|sys
// 返回内存环形缓冲中的最近日志（时间正序）。
func (p *Panel) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if s := r.URL.Query().Get("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			limit = n
		}
	}
	ch := r.URL.Query().Get("ch")
	entries := logbuf.Query(limit, ch)
	if entries == nil {
		entries = []logbuf.Entry{}
	}
	writeJSON(w, map[string]any{"entries": entries})
}

// configView 是脱敏后的配置展示形态（绝不返回 admin_key 明文）。
type configView struct {
	ListenAddr          string `json:"listen_addr"`
	AuthDir             string `json:"auth_dir"`
	DataDir             string `json:"data_dir"`
	UpstreamBase        string `json:"upstream_base"`
	MaxConsecutiveFails int    `json:"max_consecutive_fails"`
	HasAdminKey         bool   `json:"has_admin_key"`
}

// handleConfigGet: GET /panel/api/config
func (p *Panel) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	if p.cfg == nil {
		writeError(w, http.StatusInternalServerError, "配置未加载")
		return
	}
	writeJSON(w, configView{
		ListenAddr:          p.cfg.ListenAddr,
		AuthDir:             p.cfg.AuthDir,
		DataDir:             p.cfg.DataDir,
		UpstreamBase:        p.cfg.UpstreamBase,
		MaxConsecutiveFails: p.cfg.MaxConsecutiveFails,
		HasAdminKey:         p.cfg.AdminKey != "",
	})
}

// handleConfigSet: POST /panel/api/config
// 允许修改非敏感字段；listen_addr/auth_dir/data_dir 需重启生效，
// max_consecutive_fails 可热生效（直接写回 pool）。
func (p *Panel) handleConfigSet(w http.ResponseWriter, r *http.Request) {
	if p.cfg == nil {
		writeError(w, http.StatusInternalServerError, "配置未加载")
		return
	}
	var body struct {
		MaxConsecutiveFails *int    `json:"max_consecutive_fails"`
		UpstreamBase        *string `json:"upstream_base"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if body.MaxConsecutiveFails != nil {
		if *body.MaxConsecutiveFails < 0 || *body.MaxConsecutiveFails > 100 {
			writeError(w, http.StatusBadRequest, "max_consecutive_fails 取值范围 0-100")
			return
		}
		p.cfg.MaxConsecutiveFails = *body.MaxConsecutiveFails
	}
	if body.UpstreamBase != nil {
		p.cfg.UpstreamBase = *body.UpstreamBase
	}
	// TODO: 落盘 config.json（当前仅内存生效，重启丢失）
	writeJSON(w, map[string]any{"ok": true})
}
