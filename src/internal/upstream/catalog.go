package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Model 是一条模型目录条目（SPEC §4.3 指定 JSON tag）。
type Model struct {
	ID                         string   `json:"id"`
	DisplayName                string   `json:"display_name"`
	Description                string   `json:"description"`
	Tags                       []string `json:"tags"`
	MaxInputTokens             *int64   `json:"max_input_tokens,omitempty"`
	MaxOutputTokens            *int64   `json:"max_output_tokens,omitempty"`
	BillingMultiplier          *float64 `json:"billing_multiplier,omitempty"`
	BillingEffectiveMultiplier *float64 `json:"billing_effective_multiplier,omitempty"`
	Visible                    bool     `json:"visible"`
}

// catalogModel 是上游目录条目的原始形态（字段名照 §1.2）。
type catalogModel struct {
	Name                       string   `json:"name"`
	ModelName                  string   `json:"model_name"`
	Description                string   `json:"description"`
	DisplayDescription         string   `json:"display_description"`
	Tags                       []string `json:"tags"`
	Visible                    *bool    `json:"visible"`
	ContextWindow              *float64 `json:"context_window"`
	MaxTokens                  *float64 `json:"max_tokens"`
	BillingMultiplier          *float64 `json:"billing_multiplier"`
	BillingEffectiveMultiplier *float64 `json:"billing_effective_multiplier"`
	Params                     *struct {
		ContextWindow *float64 `json:"context_window"`
		MaxTokens     *float64 `json:"max_tokens"`
	} `json:"params"`
}

// FetchCatalog 拉取模型目录（GET {llm_base}/model_catalog）。
//
// 解析口径（SPEC §4.3）：根上没 data 就把根当 data；平铺所有 categories[].models；
// 同名模型首次出现者胜；空 name 丢弃；context_window/max_tokens 先看顶层再看 params；
// visible 缺省 true。
func (c *Client) FetchCatalog(ctx context.Context, token string) ([]Model, error) {
	url := c.opts.LLMBase + "/model_catalog"
	status, data, err := c.doJSON(ctx, http.MethodGet, url, nil, token,
		map[string]string{"Accept": "application/json"}, c.opts.CatalogTimeout, maxCatalogBytes)
	if err != nil {
		return nil, fmt.Errorf("拉取模型目录失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("拉取模型目录失败: HTTP %d: %s", status, jsonMessage(data))
	}
	return parseCatalog(data)
}

// CatalogProbe 只读地拉一次模型目录，返回上游 HTTP 状态码与解析出的模型条数。
//
// 不刷新 token、不写回（SPEC4 §G.2 的账号「测试」用）。上游非 2xx 时仍返回真实状态码。
func (c *Client) CatalogProbe(ctx context.Context, token string) (int, int, error) {
	url := c.opts.LLMBase + "/model_catalog"
	status, data, err := c.doJSON(ctx, http.MethodGet, url, nil, token,
		map[string]string{"Accept": "application/json"}, c.opts.CatalogTimeout, maxCatalogBytes)
	if err != nil {
		return 0, 0, fmt.Errorf("拉取模型目录失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return status, 0, fmt.Errorf("HTTP %d: %s", status, jsonMessage(data))
	}
	models, err := parseCatalog(data)
	if err != nil {
		return status, 0, err
	}
	return status, len(models), nil
}

// parseCatalog 把上游目录响应体解析成归一化后的模型列表。
func parseCatalog(data []byte) ([]Model, error) {
	var probe struct {
		Code       *int            `json:"code"`
		Message    string          `json:"message"`
		Data       json.RawMessage `json:"data"`
		Categories json.RawMessage `json:"categories"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("解析模型目录失败: %w", err)
	}
	if probe.Code != nil && *probe.Code != 0 {
		return nil, fmt.Errorf("模型目录返回异常 code=%d message=%s", *probe.Code, probe.Message)
	}
	// 根上没 data（或 data 为 null）→ 把根当 data。
	root := json.RawMessage(data)
	if len(probe.Data) > 0 && string(probe.Data) != "null" {
		root = probe.Data
	}
	var parsed struct {
		Categories []struct {
			Type   string         `json:"type"`
			Models []catalogModel `json:"models"`
		} `json:"categories"`
	}
	if err := json.Unmarshal(root, &parsed); err != nil {
		return nil, fmt.Errorf("解析模型目录失败: %w", err)
	}
	models := make([]Model, 0)
	seen := map[string]bool{}
	for _, category := range parsed.Categories {
		for i := range category.Models {
			model, ok := normalizeCatalogEntry(&category.Models[i])
			if !ok {
				continue
			}
			if seen[model.ID] {
				continue
			}
			seen[model.ID] = true
			models = append(models, model)
		}
	}
	return models, nil
}

// normalizeCatalogEntry 把上游条目归一成 Model。
func normalizeCatalogEntry(entry *catalogModel) (Model, bool) {
	name := strings.TrimSpace(entry.Name)
	if name == "" {
		name = strings.TrimSpace(entry.ModelName)
	}
	if name == "" {
		return Model{}, false
	}
	codename := strings.TrimSpace(entry.Description)
	displayDescription := strings.TrimSpace(entry.DisplayDescription)
	displayName := truncateRunes(displayDescription, 64)
	if displayName == "" {
		displayName = codename
	}
	if displayName == "" {
		displayName = name
	}
	description := displayDescription
	if description == "" {
		description = codename
	}
	// context_window / max_tokens：先顶层，再看 params。
	pickFloat := func(top, nested *float64) *int64 {
		v := top
		if v == nil {
			v = nested
		}
		if v == nil {
			return nil
		}
		n := int64(*v)
		return &n
	}
	var nestedContext, nestedMax *float64
	if entry.Params != nil {
		nestedContext = entry.Params.ContextWindow
		nestedMax = entry.Params.MaxTokens
	}
	tags := entry.Tags
	if tags == nil {
		tags = []string{}
	}
	visible := true
	if entry.Visible != nil {
		visible = *entry.Visible
	}
	return Model{
		ID:                         name,
		DisplayName:                displayName,
		Description:                description,
		Tags:                       tags,
		MaxInputTokens:             pickFloat(entry.ContextWindow, nestedContext),
		MaxOutputTokens:            pickFloat(entry.MaxTokens, nestedMax),
		BillingMultiplier:          entry.BillingMultiplier,
		BillingEffectiveMultiplier: entry.BillingEffectiveMultiplier,
		Visible:                    visible,
	}, true
}

// truncateRunes 按字符（rune）截断到 n，超出时截断。
func truncateRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
