package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"xhx2api/internal/upstream"
)

// Catalog 是模型目录的内存状态 + 持久化缓存（SPEC §6.2）。
//
// 来源优先级：内存缓存（TTL 内）→ 持久化缓存文件 → 配置 fallback → 内置 §1.2。
// 任何情况下 Models() 都返回非空内容。
type Catalog struct {
	mu          sync.Mutex
	models      []upstream.Model
	source      string // remote | cache | fallback
	refreshedAt int64  // 毫秒
	nextRefresh time.Time
	inFlight    bool

	ttl       time.Duration
	cacheFile string
	fallback  []upstream.Model
	client    *upstream.Client
}

// NewCatalog 构造目录；fallback 为空时使用内置 9 条。
func NewCatalog(client *upstream.Client, ttl time.Duration, cacheFile string, fallback []upstream.Model) *Catalog {
	if len(fallback) == 0 {
		fallback = upstream.BuiltinModels()
	}
	return &Catalog{
		source:    "fallback",
		models:    fallback,
		ttl:       ttl,
		cacheFile: cacheFile,
		fallback:  fallback,
		client:    client,
	}
}

type catalogCacheFile struct {
	Models      []upstream.Model `json:"models"`
	RefreshedAt int64            `json:"refreshed_at"`
}

// LoadCache 从持久化缓存文件恢复目录（没有则保持 fallback）。
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
	c.models = cached.Models
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
	c.models = models
	c.source = "remote"
	c.refreshedAt = time.Now().UnixMilli()
	c.nextRefresh = time.Now().Add(c.ttl)
	c.inFlight = false
	c.mu.Unlock()
	c.saveCache(models)
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

// Models 返回当前目录（非空）。
func (c *Catalog) Models(token string) []upstream.Model {
	c.mu.Lock()
	models := c.models
	c.mu.Unlock()
	c.refreshAsync(token)
	if len(models) == 0 {
		return c.fallback
	}
	return models
}

// Has 判断目录是否包含某模型 id。
func (c *Catalog) Has(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, model := range c.models {
		if model.ID == id {
			return true
		}
	}
	return false
}

// Count 返回目录条数。
func (c *Catalog) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.models)
}

// Source 返回目录来源。
func (c *Catalog) Source() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.source
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
	data := make([]map[string]any, 0, len(models)+len(s.cfg.Models.Aliases))
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
	aliases := make([]string, 0, len(s.cfg.Models.Aliases))
	for alias := range s.cfg.Models.Aliases {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		data = append(data, map[string]any{
			"id":           alias,
			"object":       "model",
			"created":      created,
			"owned_by":     "alias",
			"display_name": alias,
			"credits":      "",
		})
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
