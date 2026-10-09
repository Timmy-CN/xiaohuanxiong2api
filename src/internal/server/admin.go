package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"

	"xhx2api/internal/pool"
)

// handleAdminAPI 分发 §7.1 的只读管理接口（全部 GET）。
func (s *Server) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/admin/api/status":
		s.adminStatus(w, r)
	case "/admin/api/accounts":
		s.adminAccounts(w, r)
	case "/admin/api/accounts/balance":
		s.adminBalance(w, r)
	case "/admin/api/models":
		s.adminModels(w, r)
	case "/admin/api/requests":
		s.adminRequests(w, r)
	case "/admin/api/checkin":
		s.adminCheckin(w, r)
	default:
		http.NotFound(w, r)
	}
}

// adminStatus 实现 GET /admin/api/status。
func (s *Server) adminStatus(w http.ResponseWriter, _ *http.Request) {
	total, enabled, available, cooldown := s.pool.Counts()
	effective, err := s.cfg.MaskedJSON()
	if err != nil {
		effective = []byte("{}")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":          s.version,
		"started_at":       s.startedAt.Format(time.RFC3339),
		"uptime_seconds":   int64(time.Since(s.startedAt).Seconds()),
		"listen":           s.cfg.ListenAddr(),
		"effective_config": json.RawMessage(effective),
		"accounts": map[string]any{
			"total":     total,
			"enabled":   enabled,
			"available": available,
			"cooldown":  cooldown,
		},
		"models": map[string]any{
			"count":        s.catalog.Count(),
			"source":       s.catalog.Source(),
			"refreshed_at": s.catalog.RefreshedAt(),
		},
	})
}

// adminAccounts 实现 GET /admin/api/accounts。
func (s *Server) adminAccounts(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.pool.Views())
}

// adminModels 实现 GET /admin/api/models（目录条目 + 别名映射）。
func (s *Server) adminModels(w http.ResponseWriter, _ *http.Request) {
	models := s.catalog.Models(s.pool.FirstToken())
	entries := make([]map[string]any, 0, len(models))
	for _, model := range models {
		entry := map[string]any{
			"id":           model.ID,
			"display_name": model.DisplayName,
			"description":  model.Description,
			"tags":         model.Tags,
			"visible":      model.Visible,
			"credits":      creditsOf(model),
		}
		if model.MaxInputTokens != nil {
			entry["max_input_tokens"] = *model.MaxInputTokens
		}
		if model.MaxOutputTokens != nil {
			entry["max_output_tokens"] = *model.MaxOutputTokens
		}
		entries = append(entries, entry)
	}
	aliases := map[string]string{}
	for alias, target := range s.cfg.Models.Aliases {
		aliases[alias] = target
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"models":  entries,
		"aliases": aliases,
	})
}

// adminRequests 实现 GET /admin/api/requests?limit=N。
func (s *Server) adminRequests(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > s.cfg.Log.BufferSizeValue() {
		limit = s.cfg.Log.BufferSizeValue()
	}
	writeJSON(w, http.StatusOK, s.logs.Last(limit))
}

// adminCheckin 实现 GET /admin/api/checkin（SPEC2 §A.4）。
func (s *Server) adminCheckin(w http.ResponseWriter, _ *http.Request) {
	if s.checkin == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, s.checkin.Views())
}

// ── 积分（§7.1 /admin/api/accounts/balance，结果缓存 30s）──

type balanceCacheEntry struct {
	at    time.Time
	value map[string]any
}

var (
	balanceCacheMu sync.Mutex
	balanceCache   = map[string]balanceCacheEntry{}
)

const balanceCacheTTL = 30 * time.Second

// adminBalance 实现 GET /admin/api/accounts/balance。
// 单账号失败只在该条 error 里写原因，不整体失败。
func (s *Server) adminBalance(w http.ResponseWriter, _ *http.Request) {
	views := s.pool.Views()
	result := make([]map[string]any, 0, len(views))
	for _, view := range views {
		result = append(result, s.accountBalance(view))
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) accountBalance(view pool.AccountView) map[string]any {
	balanceCacheMu.Lock()
	cached, ok := balanceCache[view.ID]
	balanceCacheMu.Unlock()
	if ok && time.Since(cached.at) < balanceCacheTTL {
		return cached.value
	}

	entry := map[string]any{
		"id":   view.ID,
		"name": view.Name,
	}
	token := s.pool.TokenFor(view.ID)
	if token == "" {
		entry["error"] = "账号没有可用 token"
		entry["wallets"] = map[string]any{}
		entry["subscription"] = map[string]any{}
		balanceCacheMu.Lock()
		balanceCache[view.ID] = balanceCacheEntry{at: time.Now(), value: entry}
		balanceCacheMu.Unlock()
		return entry
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Upstream.BalanceTimeout())
	defer cancel()

	balance, err := s.client.Balance(ctx, token)
	if err != nil {
		entry["error"] = err.Error()
		entry["wallets"] = map[string]any{}
		entry["subscription"] = map[string]any{}
	} else {
		entry["available_points"] = balance.AvailablePoints
		entry["wallets"] = map[string]any{
			"daily_points":   balance.DailyPoints,
			"monthly_points": balance.MonthlyPoints,
			"reward_points":  balance.RewardPoints,
			"topup_points":   balance.TopupPoints,
		}
		subscription := map[string]any{}
		if raw, err := s.client.Entitlement(ctx, token); err == nil {
			var ent struct {
				Office struct {
					ProEnable  *bool `json:"pro_enable"`
					ActivePlan *struct {
						Type string `json:"type"`
					} `json:"active_plan"`
				} `json:"office"`
			}
			if json.Unmarshal(raw, &ent) == nil {
				if ent.Office.ProEnable != nil {
					subscription["office_pro_enable"] = *ent.Office.ProEnable
				}
				if ent.Office.ActivePlan != nil {
					subscription["office_active_plan_type"] = ent.Office.ActivePlan.Type
				}
			}
		}
		entry["subscription"] = subscription
	}
	balanceCacheMu.Lock()
	balanceCache[view.ID] = balanceCacheEntry{at: time.Now(), value: entry}
	balanceCacheMu.Unlock()
	return entry
}
