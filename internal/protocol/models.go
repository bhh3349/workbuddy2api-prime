package protocol

// /v1/models 模型列表接口（OpenAI 兼容格式 + 分组元数据）。
//
// 背景：agent 添加供应商时会调 /v1/models 拉取可用模型。标准格式是 flat list，
// 没有分组概念。这里在每个 model 对象上加自定义字段：
//   - group: "discount"（折扣/限免）| "full_price"（原价）
//   - promo_label: 优惠标签原文（无则省略）
// 不认这些字段的客户端按普通列表用，零影响。

import (
	"encoding/json"
	"net/http"
	"time"
)

// ModelInfo 是 /v1/models 的单个模型信息，由调用方（main.go）从
// scheduler 的模型目录快照转换而来，避免 protocol 依赖 upstream/scheduler。
type ModelInfo struct {
	ID         string
	Group      string // "discount" | "full_price"
	PromoLabel string // 优惠标签，无则空
}

// ModelProvider 提供模型列表（main.go 用闭包接入 scheduler）。
// 传入 *http.Request 以便读取鉴权 key（按 key 的分组过滤）。
type ModelProvider func(r *http.Request) []ModelInfo

// openAIModel 是 /v1/models 返回的单个模型对象（OpenAI 格式 + 扩展字段）。
type openAIModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// 扩展：分组元数据（非标准字段，标准客户端忽略）
	Group      string `json:"group,omitempty"`
	PromoLabel string `json:"promo_label,omitempty"`
}

// ModelsHandler 处理 GET /v1/models。
func ModelsHandler(provider ModelProvider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		now := time.Now().Unix()
		models := []openAIModel{}
		if provider != nil {
			for _, m := range provider(r) {
				om := openAIModel{
					ID:      m.ID,
					Object:  "model",
					Created: now,
					OwnedBy: "workbuddy2api-prime",
					Group:   m.Group,
				}
				if m.PromoLabel != "" {
					om.PromoLabel = m.PromoLabel
				}
				models = append(models, om)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   models,
		})
	}
}
