package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"xhx2api/internal/upstream"
)

// Catalog 是模型目录的内存状态 + 持久化缓存（SPEC §6.2）。
//
// 服务优先级：内存缓存（TTL 内）→ 持久化缓存文件 → 内置兜底表（source=fallback，
// 照 raccoon 插件的 FALLBACK_MODELS 抄录）；兜底表始终保证上游默认对话模型在列。
// 模型校验只看真实目录（remote/cache），目录为空时跳过校验放行——与兜底展示互不影响。
type Catalog struct {
	mu          sync.Mutex
	models      []upstream.Model
	source      string // remote | cache | empty
	refreshedAt int64  // 毫秒
	nextRefresh time.Time
	inFlight    bool

	ttl       time.Duration
	cacheFile string
	client    *upstream.Client
}

// defaultChatModel 是上游网页端的默认对话模型：目录有时不列它，但上游接受
//（raccoon 插件实测），所以始终保证在列。
const defaultChatModel = "raccoon-chat-ml-5-5"

// fallbackModels 是目录完全不可用（远程拉不到且无缓存文件）时的内置兜底表。
// 上下文/输出/倍率照 raccoon 插件的 FALLBACK_MODELS 抄录（同一上游的已知值）。
func fallbackModels() []upstream.Model {
	entry := func(id, name string, in, out int64, base, eff float64) upstream.Model {
		return upstream.Model{
			ID: id, DisplayName: name, Visible: true,
			MaxInputTokens: &in, MaxOutputTokens: &out,
			BillingMultiplier: &base, BillingEffectiveMultiplier: &eff,
		}
	}
	return []upstream.Model{
		entry("raccoon-8c4485", "Raccoon-Work", 1_000_000, 100_000, 1, 1),
		entry("raccoon-19b265", "Raccoon-Work-260817-A", 1_000_000, 100_000, 1, 1),
		entry("raccoon-405a1c", "Raccoon-Work-260817-B", 1_000_000, 100_000, 1, 1),
		entry("sn-sensenova-6-8-flash", "SenseNova-6.8-Flash", 256_000, 63_999, 0.5, 0),
		entry("sn-sensenova-6-8-flash-lite", "SenseNova-6.8-Flash-Lite", 256_000, 63_999, 0.5, 0),
		entry("sn-glm-5-3", "GLM-5-3", 1_000_000, 100_000, 0.75, 0.75),
		entry("sn-kimi-k3", "Kimi-K3", 1_000_000, 100_000, 1, 1),
		entry("sn-glm-5-3-flash", "GLM-5-3-Flash", 1_000_000, 100_000, 0.2, 0.2),
		entry("sn-deepseek-v4-1-flash", "DeepSeek-V4.1-Flash", 1_000_000, 100_000, 0.25, 0.25),
		entry(defaultChatModel, "Raccoon Chat (默认)", 180_000, 80_000, 1, 1),
	}
}

// ensureDefaultChatModel 保证上游默认对话模型在列（就地追加，幂等）。
func ensureDefaultChatModel(models []upstream.Model) []upstream.Model {
	for _, model := range models {
		if model.ID == defaultChatModel {
			return models
		}
	}
	return append(models, upstream.Model{ID: defaultChatModel, DisplayName: "Raccoon Chat (默认)", Visible: true})
}

// NewCatalog 构造目录；初始为空（等待 LoadCache 或 Refresh）。
func NewCatalog(client *upstream.Client, ttl time.Duration, cacheFile string) *Catalog {
	return &Catalog{
		source:    "empty",
		ttl:       ttl,
		cacheFile: cacheFile,
		client:    client,
	}
}

type catalogCacheFile struct {
	Models      []upstream.Model `json:"models"`
	RefreshedAt int64            `json:"refreshed_at"`
}

// LoadCache 从持久化缓存文件恢复目录（没有则保持空）。
func (c *Catalog) LoadCache() bool {
	if c.cacheFile == "" {
		return false
	}
	data, err := os.ReadFile(c.cacheFile)
	if err != nil {
		return false
	}
	var cached catalogCacheFile
	if err := json.Unmarshal(data, &cached); err != nil || len(cached.Models) == 0 {
		return false
	}
	c.mu.Lock()
	c.models = ensureDefaultChatModel(cached.Models)
	c.source = "cache"
	c.refreshedAt = cached.RefreshedAt
	c.mu.Unlock()
	return true
}

// Refresh 拉取远程目录并落地（含持久化缓存）。失败时保留现有内容。
func (c *Catalog) Refresh(ctx context.Context, token string) error {
	models, err := c.client.FetchCatalog(ctx, token)
	if err != nil {
		c.mu.Lock()
		c.nextRefresh = time.Now().Add(c.ttl)
		c.inFlight = false
		c.mu.Unlock()
		return err
	}
	if len(models) == 0 {
		c.mu.Lock()
		c.nextRefresh = time.Now().Add(c.ttl)
		c.inFlight = false
		c.mu.Unlock()
		return errors.New("上游返回的模型目录为空")
	}
	c.mu.Lock()
	c.models = ensureDefaultChatModel(models)
	c.source = "remote"
	c.refreshedAt = time.Now().UnixMilli()
	c.nextRefresh = time.Now().Add(c.ttl)
	c.inFlight = false
	c.mu.Unlock()
	c.saveCache(c.models)
	return nil
}

func (c *Catalog) saveCache(models []upstream.Model) {
	if c.cacheFile == "" {
		return
	}
	payload := catalogCacheFile{Models: models, RefreshedAt: time.Now().UnixMilli()}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	tmp := c.cacheFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, c.cacheFile)
}

// refreshAsync 在 TTL 到期后触发一次后台刷新（不阻塞请求）。
func (c *Catalog) refreshAsync(token string) {
	c.mu.Lock()
	if c.inFlight || time.Now().Before(c.nextRefresh) {
		c.mu.Unlock()
		return
	}
	c.inFlight = true
	c.mu.Unlock()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Refresh(ctx, token)
	}()
}

// Models 返回当前生效的目录（真实目录为空时退回内置兜底表）。
func (c *Catalog) Models(token string) []upstream.Model {
	c.mu.Lock()
	models := c.servingLocked()
	c.mu.Unlock()
	c.refreshAsync(token)
	return models
}

// Has 判断当前生效目录是否包含某模型 id。
func (c *Catalog) Has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, model := range c.servingLocked() {
		if model.ID == id {
			return true
		}
	}
	return false
}

// Count 返回当前生效目录条数。
func (c *Catalog) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.servingLocked())
}

// Empty 判断真实目录（remote/cache）是否为空——模型校验的「跳过校验」门槛。
func (c *Catalog) Empty() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.models) == 0
}

// Source 返回目录来源：remote / cache / fallback（真实目录为空时的兜底表）。
func (c *Catalog) Source() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.models) > 0 {
		return c.source
	}
	return "fallback"
}

// servingLocked 返回当前生效的目录（调用方需持锁）：真实目录为空时退回内置
// 兜底表；兜底表本身已含默认对话模型。
func (c *Catalog) servingLocked() []upstream.Model {
	if len(c.models) > 0 {
		return c.models
	}
	return fallbackModels()
}

// RefreshedAt 返回最近一次远程刷新时间（毫秒）。
func (c *Catalog) RefreshedAt() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refreshedAt
}

// handleModels 实现 GET /v1/models（SPEC §6.2）。
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	models := s.catalog.Models(s.pool.FirstToken())
	created := s.startedAt.Unix()
	data := make([]map[string]any, 0, len(models))
	for _, model := range models {
		entry := map[string]any{
			"id":           model.ID,
			"object":       "model",
			"created":      created,
			"owned_by":     "xiaohuanxiong",
			"display_name": model.DisplayName,
			"credits":      creditsOf(model),
		}
		if model.MaxInputTokens != nil {
			entry["max_input_tokens"] = *model.MaxInputTokens
		}
		if model.MaxOutputTokens != nil {
			entry["max_output_tokens"] = *model.MaxOutputTokens
		}
		data = append(data, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// creditsOf 生成形如 "x0.2 credits" 的计费文本。
//
// 用 billing_effective_multiplier（实付），缺省回落基准倍率；都没有则为空串。
func creditsOf(model upstream.Model) string {
	value := model.BillingEffectiveMultiplier
	if value == nil {
		value = model.BillingMultiplier
	}
	if value == nil {
		return ""
	}
	return "x" + formatMultiplier(*value) + " credits"
}

// formatMultiplier 把倍率格式化成紧凑文本（整数不带小数点，其余最多两位并裁零）。
func formatMultiplier(value float64) string {
	rounded := float64(int64(value*100+0.5)) / 100
	if rounded == float64(int64(rounded)) {
		return strconv.FormatInt(int64(rounded), 10)
	}
	text := strconv.FormatFloat(rounded, 'f', 2, 64)
	return strings.TrimRight(strings.TrimRight(text, "0"), ".")
}
