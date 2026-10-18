package panel

// 自定义模型分组的面板接口。

import (
	"encoding/json"
	"net/http"
)

// handleModelGroupsList: GET /panel/api/model-groups
func (p *Panel) handleModelGroupsList(w http.ResponseWriter, r *http.Request) {
	if p.groups == nil {
		writeJSON(w, []any{})
		return
	}
	writeJSON(w, p.groups.List())
}

// handleModelGroupsCreate: POST /panel/api/model-groups {"name": "...", "models": [...]}
func (p *Panel) handleModelGroupsCreate(w http.ResponseWriter, r *http.Request) {
	if p.groups == nil {
		writeError(w, http.StatusInternalServerError, "分组存储未初始化")
		return
	}
	var body struct {
		Name   string   `json:"name"`
		Models []string `json:"models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	g, err := p.groups.Create(body.Name, body.Models)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, g)
}

// handleModelGroupsUpdate: PUT /panel/api/model-groups/{id} {"name": "...", "models": [...]}
func (p *Panel) handleModelGroupsUpdate(w http.ResponseWriter, r *http.Request) {
	if p.groups == nil {
		writeError(w, http.StatusInternalServerError, "分组存储未初始化")
		return
	}
	id := r.PathValue("id")
	var body struct {
		Name   string   `json:"name"`
		Models []string `json:"models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	g, err := p.groups.Update(id, body.Name, body.Models)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, g)
}

// handleModelGroupsDelete: DELETE /panel/api/model-groups/{id}
func (p *Panel) handleModelGroupsDelete(w http.ResponseWriter, r *http.Request) {
	if p.groups == nil {
		writeError(w, http.StatusInternalServerError, "分组存储未初始化")
		return
	}
	id := r.PathValue("id")
	if err := p.groups.Delete(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// 解绑使用该分组的子钥匙（置空，不删钥匙）
	if p.keys != nil {
		for _, k := range p.keys.List() {
			if k.ModelGroupID == id {
				_ = p.keys.SetModelGroup(k.ID, "")
			}
		}
	}
	writeJSON(w, map[string]any{"ok": true})
}

// handleKeyModelGroup: POST /panel/api/keys/{id}/model-group {"group_id": "..."}
// 给子钥匙绑定模型分组（空字符串=解绑）。绑定后该 key 的 /v1/models 只返回组内模型。
func (p *Panel) handleKeyModelGroup(w http.ResponseWriter, r *http.Request) {
	if p.keys == nil {
		writeError(w, http.StatusInternalServerError, "子钥匙存储未初始化")
		return
	}
	id := r.PathValue("id")
	var body struct {
		GroupID string `json:"group_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	if body.GroupID != "" && p.groups != nil && p.groups.Get(body.GroupID) == nil {
		writeError(w, http.StatusBadRequest, "分组不存在")
		return
	}
	if err := p.keys.SetModelGroup(id, body.GroupID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
