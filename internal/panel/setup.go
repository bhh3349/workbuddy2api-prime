// 首次运行 Web 初始化向导：全程不用命令行。
//
// 配套 main.go 接线（本包只暴露接口，不碰 main.go）：
//  1. 启动时调 panel.SetAdminKey(cfg.AdminKey)
//  2. 若 panel.SetupNeeded() 为 true，把 SetupRoutes 挂到**未鉴权**的 mux 上，
//     其余路由仍走 withAuth（adminKey 为空时 withAuth 会拒绝一切，
//     所以向导路由必须豁免）
//  3. onSetKey 回调里把新密钥写回 config.json 并更新内存中的 cfg
package panel

import (
	_ "embed"
	"encoding/json"
	"net/http"
)

//go:embed setup.html
var setupHTML []byte

// SetAdminKey 设置当前管理员密钥（main.go 启动时传入，供 SetupNeeded 判断）。
func (p *Panel) SetAdminKey(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.adminKey = key
}

// SetupNeeded 报告是否需要走初始化向导（admin key 为空时 true）。
func (p *Panel) SetupNeeded() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.adminKey == ""
}

// SetupRoutes 把向导页挂到 mux 上。main.go 必须把它挂在**未鉴权**的 mux 上。
// onSetKey 收到用户设置的新密钥：main.go 负责持久化到 config.json 并更新内存。
func (p *Panel) SetupRoutes(mux *http.ServeMux, onSetKey func(string) error) {
	mux.HandleFunc("GET /panel/setup", p.serveSetup)
	mux.HandleFunc("POST /panel/api/setup", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Key  string `json:"key"`
			Key2 string `json:"key2"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "请求体解析失败: "+err.Error())
			return
		}
		if len(body.Key) < 6 {
			writeError(w, http.StatusBadRequest, "密钥至少 6 位")
			return
		}
		if body.Key != body.Key2 {
			writeError(w, http.StatusBadRequest, "两次输入不一致")
			return
		}
		if err := onSetKey(body.Key); err != nil {
			writeError(w, http.StatusInternalServerError, "保存失败: "+err.Error())
			return
		}
		p.SetAdminKey(body.Key)
		writeJSON(w, map[string]any{"ok": true})
	})
}

// serveSetup 返回向导页。
func (p *Panel) serveSetup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(setupHTML)
}
