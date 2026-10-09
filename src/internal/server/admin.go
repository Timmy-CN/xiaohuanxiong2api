package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"xhx2api/internal/pool"
	"xhx2api/internal/upstream"
)

// handleAdminAPI 分发管理接口：只读 GET + §G 的账号写操作。
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
	case "/admin/api/login/start":
		s.adminLoginStart(w, r)
	case "/admin/api/login/exchange":
		s.adminLoginExchange(w, r)
	default:
		if id, action, ok := accountSubPath(r.URL.Path); ok {
			s.adminAccountByID(w, r, id, action)
			return
		}
		http.NotFound(w, r)
	}
}

// accountSubPath 解析 /admin/api/accounts/{id} 与 /admin/api/accounts/{id}/test。
func accountSubPath(path string) (id, action string, ok bool) {
	const prefix = "/admin/api/accounts/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	rest := strings.TrimPrefix(path, prefix)
	parts := strings.Split(rest, "/")
	if len(parts) == 1 && parts[0] != "" {
		return parts[0], "", true
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "test" {
		return parts[0], "test", true
	}
	return "", "", false
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
		"login": map[string]any{
			"enabled": s.cfg.Login.LoginEnabled(),
			"pending": s.login.pendingCount(),
		},
	})
}

// adminAccounts 实现 GET /admin/api/accounts（列表）与 POST（新增，SPEC4 §G.2）。
func (s *Server) adminAccounts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.pool.Views())
	case http.MethodPost:
		s.adminCreateAccount(w, r)
	default:
		methodNotAllowed(w)
	}
}

// adminModels 实现 GET /admin/api/models（目录条目）。
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
	writeJSON(w, http.StatusOK, map[string]any{"models": entries})
}

// ── §G.2 账号写操作 ──

// adminKeyMissingMessage 是未设置 admin.api_key 时写操作返回的中文原因。
const adminKeyMissingMessage = "请先在 config.json 里设置 admin.api_key，再使用账号管理功能"

// requireAdminWrite 校验写操作前提：admin.api_key 必须已设置。
func (s *Server) requireAdminWrite(w http.ResponseWriter) bool {
	if s.cfg.Admin.AdminAPIKey() == "" {
		writeError(w, http.StatusForbidden, apiError{
			Message: adminKeyMissingMessage,
			Type:    typeAuthentication,
			Code:    "admin_key_required",
		})
		return false
	}
	return true
}

func methodNotAllowed(w http.ResponseWriter) {
	writeError(w, http.StatusMethodNotAllowed, apiError{
		Message: "方法不允许",
		Type:    typeInvalidRequest,
		Code:    "method_not_allowed",
	})
}

// decodeJSONBody 读取并解析写接口请求体（上限 1MB）。
func decodeJSONBody(r *http.Request, dst any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, dst)
}

// accountIDFromToken 生成账号 id：user-<userId>，取不到则 acct-<8 位随机十六进制>。
func accountIDFromToken(token string) string {
	if uid := upstream.JWTUserID(token); uid != "" {
		return "user-" + uid
	}
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("acct-%08x", uint32(time.Now().UnixNano()))
	}
	return "acct-" + hex.EncodeToString(buf[:])
}

type accountCreateRequest struct {
	Name         string `json:"name"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	Priority     *int   `json:"priority"`
	Enabled      *bool  `json:"enabled"`
}

// adminCreateAccount 实现 POST /admin/api/accounts。
func (s *Server) adminCreateAccount(w http.ResponseWriter, r *http.Request) {
	if !s.requireAdminWrite(w) {
		return
	}
	var req accountCreateRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, apiError{
			Message: "请求体不是合法 JSON：" + err.Error(),
			Type:    typeInvalidRequest,
			Code:    "invalid_json",
		})
		return
	}
	if strings.TrimSpace(req.AccessToken) == "" {
		writeError(w, http.StatusBadRequest, apiError{
			Message: "access_token 不能为空",
			Type:    typeInvalidRequest,
			Code:    "access_token_required",
		})
		return
	}
	priority := 100
	if req.Priority != nil {
		priority = *req.Priority
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	account := &pool.Account{
		ID:           accountIDFromToken(req.AccessToken),
		Name:         req.Name,
		UserID:       upstream.JWTUserID(req.AccessToken),
		AccessToken:  req.AccessToken,
		RefreshToken: req.RefreshToken,
		ExpiresAt:    upstream.JWTExpiry(req.AccessToken),
		Enabled:      enabled,
		Priority:     priority,
	}
	if err := s.pool.Add(account); err != nil {
		if errors.Is(err, pool.ErrAccountExists) {
			writeError(w, http.StatusConflict, apiError{
				Message: "账号 " + account.ID + " 已存在",
				Type:    typeInvalidRequest,
				Code:    "account_exists",
			})
			return
		}
		writeError(w, http.StatusInternalServerError, apiError{
			Message: "回写账号文件失败：" + err.Error(),
			Type:    typeUpstream,
			Code:    "persist_failed",
		})
		return
	}
	s.logger.Infof("[admin] 新增账号 %s", account.ID)
	view, _ := s.pool.View(account.ID)
	writeJSON(w, http.StatusCreated, view)
}

type accountPatchRequest struct {
	Name         *string `json:"name"`
	Priority     *int    `json:"priority"`
	Enabled      *bool   `json:"enabled"`
	AccessToken  *string `json:"access_token"`
	RefreshToken *string `json:"refresh_token"`
}

// adminAccountByID 分发 /admin/api/accounts/{id} 的 PATCH/DELETE 与 /test。
func (s *Server) adminAccountByID(w http.ResponseWriter, r *http.Request, id, action string) {
	if action == "test" {
		if r.Method != http.MethodPost {
			methodNotAllowed(w)
			return
		}
		s.adminTestAccount(w, r, id)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		s.adminUpdateAccount(w, r, id)
	case http.MethodDelete:
		s.adminDeleteAccount(w, r, id)
	default:
		methodNotAllowed(w)
	}
}

// adminUpdateAccount 实现 PATCH /admin/api/accounts/{id}。
func (s *Server) adminUpdateAccount(w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAdminWrite(w) {
		return
	}
	var req accountPatchRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, apiError{
			Message: "请求体不是合法 JSON：" + err.Error(),
			Type:    typeInvalidRequest,
			Code:    "invalid_json",
		})
		return
	}
	err := s.pool.Update(id, pool.AccountPatch{
		Name:         req.Name,
		Priority:     req.Priority,
		Enabled:      req.Enabled,
		AccessToken:  req.AccessToken,
		RefreshToken: req.RefreshToken,
	})
	if errors.Is(err, pool.ErrAccountNotFound) {
		writeError(w, http.StatusNotFound, apiError{
			Message: "账号 " + id + " 不存在",
			Type:    typeInvalidRequest,
			Code:    "account_not_found",
		})
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, apiError{
			Message: "回写账号文件失败：" + err.Error(),
			Type:    typeUpstream,
			Code:    "persist_failed",
		})
		return
	}
	s.logger.Infof("[admin] 更新账号 %s", id)
	view, _ := s.pool.View(id)
	writeJSON(w, http.StatusOK, view)
}

// adminDeleteAccount 实现 DELETE /admin/api/accounts/{id}。
func (s *Server) adminDeleteAccount(w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAdminWrite(w) {
		return
	}
	if err := s.pool.Remove(id); err != nil {
		if errors.Is(err, pool.ErrAccountNotFound) {
			writeError(w, http.StatusNotFound, apiError{
				Message: "账号 " + id + " 不存在",
				Type:    typeInvalidRequest,
				Code:    "account_not_found",
			})
			return
		}
		writeError(w, http.StatusInternalServerError, apiError{
			Message: "回写账号文件失败：" + err.Error(),
			Type:    typeUpstream,
			Code:    "persist_failed",
		})
		return
	}
	s.logger.Infof("[admin] 删除账号 %s", id)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": id})
}

// adminTestAccount 实现 POST /admin/api/accounts/{id}/test。
//
// 用账号当前 token 只读打一次上游目录；上游失败也返回 HTTP 200，用 ok/status 表达。
func (s *Server) adminTestAccount(w http.ResponseWriter, r *http.Request, id string) {
	if !s.requireAdminWrite(w) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Upstream.CatalogTimeout())
	defer cancel()
	status, count, err := s.pool.TestToken(ctx, id)
	if errors.Is(err, pool.ErrAccountNotFound) {
		writeError(w, http.StatusNotFound, apiError{
			Message: "账号 " + id + " 不存在",
			Type:    typeInvalidRequest,
			Code:    "account_not_found",
		})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":     false,
			"status": status,
			"error":  err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"status": status,
		"models": count,
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
