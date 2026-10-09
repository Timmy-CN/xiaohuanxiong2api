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

func floatPtr(v float64) *float64 { return &v }

func int64Ptr(v int64) *int64 { return &v }

// BuiltinModels 返回 SPEC §1.2 实测的 9 条静态兜底模型。
//
// 这是 `models.fallback` 为空时的兜底内容：任何情况下 /v1/models 都必须有内容。
func BuiltinModels() []Model {
	generalOffice := []string{"general", "office", "code", "vision", "debug", "analysis", "html", "reasoning", "auto"}
	officeAnalysis := []string{"general", "office", "code", "analysis", "auto"}
	chatLight := []string{"general", "chat", "rewrite", "summary", "fast"}
	chatLightVision := []string{"general", "chat", "rewrite", "summary", "fast", "vision"}
	officeNoAuto := []string{"general", "office", "code", "vision", "debug", "analysis", "html", "reasoning"}
	codeAnalysis := []string{"general", "code", "html", "analysis", "reasoning", "auto"}

	return []Model{
		{
			ID:                         "raccoon-8c4485",
			DisplayName:                "适合复杂办公任务、代码开发、调试和图像识别等任务",
			Description:                "适合复杂办公任务、代码开发、调试和图像识别等任务",
			Tags:                       generalOffice,
			MaxInputTokens:             int64Ptr(1000000),
			MaxOutputTokens:            int64Ptr(100000),
			BillingMultiplier:          floatPtr(1),
			BillingEffectiveMultiplier: floatPtr(1),
			Visible:                    false,
		},
		{
			ID:                         "raccoon-19b265",
			DisplayName:                "适合复杂办公任务、代码开发和综合分析等任务",
			Description:                "适合复杂办公任务、代码开发和综合分析等任务",
			Tags:                       officeAnalysis,
			MaxInputTokens:             int64Ptr(1000000),
			MaxOutputTokens:            int64Ptr(100000),
			BillingMultiplier:          floatPtr(1),
			BillingEffectiveMultiplier: floatPtr(1),
			Visible:                    false,
		},
		{
			ID:                         "raccoon-405a1c",
			DisplayName:                "适合通用对话、轻量代码和内容处理等任务",
			Description:                "适合通用对话、轻量代码和内容处理等任务",
			Tags:                       chatLight,
			MaxInputTokens:             int64Ptr(1000000),
			MaxOutputTokens:            int64Ptr(100000),
			BillingMultiplier:          floatPtr(1),
			BillingEffectiveMultiplier: floatPtr(1),
			Visible:                    false,
		},
		{
			ID:                         "sn-sensenova-6-8-flash",
			DisplayName:                "适合简单问答、内容改写和摘要等轻量任务",
			Description:                "适合简单问答、内容改写和摘要等轻量任务",
			Tags:                       chatLightVision,
			MaxInputTokens:             int64Ptr(256000),
			MaxOutputTokens:            int64Ptr(63999),
			BillingMultiplier:          floatPtr(0.5),
			BillingEffectiveMultiplier: floatPtr(0),
			Visible:                    true,
		},
		{
			ID:                         "sn-sensenova-6-8-flash-lite",
			DisplayName:                "适合简单问答、内容改写和摘要等轻量任务",
			Description:                "适合简单问答、内容改写和摘要等轻量任务",
			Tags:                       chatLightVision,
			MaxInputTokens:             int64Ptr(256000),
			MaxOutputTokens:            int64Ptr(63999),
			BillingMultiplier:          floatPtr(0.5),
			BillingEffectiveMultiplier: floatPtr(0),
			Visible:                    true,
		},
		{
			ID:                         "sn-glm-5-3",
			DisplayName:                "适合复杂办公任务、代码开发、调试和图像识别等任务",
			Description:                "适合复杂办公任务、代码开发、调试和图像识别等任务",
			Tags:                       officeNoAuto,
			MaxInputTokens:             int64Ptr(1000000),
			MaxOutputTokens:            int64Ptr(100000),
			BillingMultiplier:          floatPtr(0.75),
			BillingEffectiveMultiplier: floatPtr(0.75),
			Visible:                    true,
		},
		{
			ID:                         "sn-kimi-k3",
			DisplayName:                "适合复杂办公任务、代码开发和综合分析等任务",
			Description:                "适合复杂办公任务、代码开发和综合分析等任务",
			Tags:                       officeNoAuto,
			MaxInputTokens:             int64Ptr(1000000),
			MaxOutputTokens:            int64Ptr(100000),
			BillingMultiplier:          floatPtr(1),
			BillingEffectiveMultiplier: floatPtr(1),
			Visible:                    true,
		},
		{
			ID:                         "sn-glm-5-3-flash",
			DisplayName:                "适合通用对话、轻量代码和内容处理等任务",
			Description:                "适合通用对话、轻量代码和内容处理等任务",
			Tags:                       chatLight,
			MaxInputTokens:             int64Ptr(1000000),
			MaxOutputTokens:            int64Ptr(100000),
			BillingMultiplier:          floatPtr(0.2),
			BillingEffectiveMultiplier: floatPtr(0.2),
			Visible:                    true,
		},
		{
			ID:                         "sn-deepseek-v4-1-flash",
			DisplayName:                "适合通用代码开发、复杂分析和推理任务",
			Description:                "适合通用代码开发、复杂分析和推理任务",
			Tags:                       codeAnalysis,
			MaxInputTokens:             int64Ptr(1000000),
			MaxOutputTokens:            int64Ptr(100000),
			BillingMultiplier:          floatPtr(0.25),
			BillingEffectiveMultiplier: floatPtr(0.25),
			Visible:                    true,
		},
	}
}
