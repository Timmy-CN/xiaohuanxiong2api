package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"xhx2api/internal/pool"
	"xhx2api/internal/upstream"
)

// 网页登录的协议常量（SPEC5 §I）：login_source 与回调的 scheme/host/path 固定，
// 不做配置项。
const (
	loginSource           = "desktop"
	raccoonCallbackScheme = "office-raccoon"
	raccoonCallbackHost   = "auth"
	raccoonCallbackPath   = "/callback"

	maxLoginCodeLength  = 8192
	maxLoginStateLength = 512
)

// errInvalidLoginCallback 表示回调 URL 的形态不对（SPEC5 §K2）。
var errInvalidLoginCallback = errors.New("登录回调地址无效")

// loginManager 维护网页登录的 pending state（SPEC5 §I）。
//
// state 由本服务用 crypto/rand 生成，登记时带过期时间；回调必须与其中一条
// 逐字相等且未过期，且一次性消费。now 可注入，便于测试模拟过期。
type loginManager struct {
	mu      sync.Mutex
	pending map[string]time.Time // state -> 过期时刻
	now     func() time.Time
}

// newLoginManager 构造 pending 表。
func newLoginManager() *loginManager {
	return &loginManager{pending: map[string]time.Time{}, now: time.Now}
}

// newLoginState 生成 16 字节随机 state → 32 位十六进制（SPEC5 §I）。
func newLoginState() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// pruneLocked 删除已过期项（调用方需持锁）。
func (m *loginManager) pruneLocked(now time.Time) {
	for state, expires := range m.pending {
		if !expires.After(now) {
			delete(m.pending, state)
		}
	}
}

// issue 生成新 state 并登记，返回 state 与过期毫秒时间戳；顺手清理过期项。
func (m *loginManager) issue(ttl time.Duration) (string, int64, error) {
	state, err := newLoginState()
	if err != nil {
		return "", 0, err
	}
	now := m.now()
	m.mu.Lock()
	m.pruneLocked(now)
	expires := now.Add(ttl)
	m.pending[state] = expires
	m.mu.Unlock()
	return state, expires.UnixMilli(), nil
}

// pendingCount 返回当前未过期的 pending state 数（供 /admin/api/status）。
func (m *loginManager) pendingCount() int {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.pruneLocked(now)
	return len(m.pending)
}

// consume 校验并一次性消费 state。
//
// 返回 ok=false 时用 expired 区分「已过期」与「缺失/不匹配/已用过」。
func (m *loginManager) consume(state string) (ok bool, expired bool) {
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	expires, found := m.pending[state]
	if !found {
		return false, false
	}
	delete(m.pending, state)
	if !expires.After(now) {
		return false, true
	}
	return true, false
}

// buildAuthorizeURL 用 net/url 构造授权页地址（SPEC5 §K1）。
//
// appname 是中文、state 是十六进制，必须正确百分号编码，不做字符串拼接。
func buildAuthorizeURL(origin, appName, state string) (string, error) {
	base, err := url.Parse(origin)
	if err != nil {
		return "", err
	}
	base.Path = "/code/authorize"
	query := base.Query()
	query.Set("login_source", loginSource)
	query.Set("appname", appName)
	query.Set("state", state)
	base.RawQuery = query.Encode()
	return base.String(), nil
}

// loginCallback 是从回调 URL 里解析出的授权码与 state。
type loginCallback struct {
	Code  string
	State string
}

// parseLoginCallback 解析回调 URL 并逐项比对 scheme/host/path（SPEC5 §I）。
//
// 不允许用 strings.HasPrefix 判断：`office-raccoon://auth/callback@evil.com`
// 会被前缀匹配放行。这里解析后逐项比对；host 大小写不敏感，path 精确。
func parseLoginCallback(raw string) (loginCallback, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return loginCallback{}, errInvalidLoginCallback
	}
	if parsed.Scheme != raccoonCallbackScheme ||
		!strings.EqualFold(parsed.Host, raccoonCallbackHost) ||
		parsed.Path != raccoonCallbackPath {
		return loginCallback{}, errInvalidLoginCallback
	}
	query := parsed.Query()
	return loginCallback{
		Code:  strings.TrimSpace(query.Get("code")),
		State: strings.TrimSpace(query.Get("state")),
	}, nil
}

// adminLoginStart 实现 POST /admin/api/login/start（SPEC5 §K1）。
func (s *Server) adminLoginStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.requireAdminWrite(w) {
		return
	}
	if !s.cfg.Login.LoginEnabled() {
		writeError(w, http.StatusForbidden, apiError{
			Message: "网页登录已禁用（config.json 的 login.enabled=false）",
			Type:    typeAuthentication,
			Code:    "login_disabled",
		})
		return
	}
	ttl := s.cfg.Login.StateTTL()
	state, expiresAt, err := s.login.issue(ttl)
	if err != nil {
		writeError(w, http.StatusInternalServerError, apiError{
			Message: "生成登录 state 失败：" + err.Error(),
			Type:    typeUpstream,
			Code:    "login_state_failed",
		})
		return
	}
	authorizeURL, err := buildAuthorizeURL(s.cfg.Upstream.OriginURL(), s.cfg.Login.LoginAppName(), state)
	if err != nil {
		writeError(w, http.StatusInternalServerError, apiError{
			Message: "构造登录地址失败：" + err.Error(),
			Type:    typeUpstream,
			Code:    "login_url_failed",
		})
		return
	}
	s.logger.Infof("[login] 发起网页登录")
	writeJSONUnescaped(w, http.StatusOK, map[string]any{
		"url":         authorizeURL,
		"state":       state,
		"expires_at":  expiresAt,
		"ttl_seconds": int(ttl.Seconds()),
	})
}

// writeJSONUnescaped 写 2xx JSON，但不转义 HTML 字符。
//
// 仅用于 /login/start 的响应：授权 URL 里的 & 需要保持原样，方便用户直接
// 复制去浏览器打开；Go 默认的 json.Encoder 会把它写成 \u0026。不改共享的
// writeJSON，避免影响既有接口的响应字节。
func writeJSONUnescaped(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(payload)
}

type loginExchangeRequest struct {
	CallbackURL string `json:"callback_url"`
	Code        string `json:"code"`
	State       string `json:"state"`
}

// adminLoginExchange 实现 POST /admin/api/login/exchange（SPEC5 §K2）。
func (s *Server) adminLoginExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	if !s.requireAdminWrite(w) {
		return
	}
	if !s.cfg.Login.LoginEnabled() {
		writeError(w, http.StatusForbidden, apiError{
			Message: "网页登录已禁用（config.json 的 login.enabled=false）",
			Type:    typeAuthentication,
			Code:    "login_disabled",
		})
		return
	}
	var req loginExchangeRequest
	if err := decodeJSONBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, apiError{
			Message: "请求体不是合法 JSON：" + err.Error(),
			Type:    typeInvalidRequest,
			Code:    "invalid_json",
		})
		return
	}

	code, state := strings.TrimSpace(req.Code), strings.TrimSpace(req.State)
	if strings.TrimSpace(req.CallbackURL) != "" {
		parsed, err := parseLoginCallback(req.CallbackURL)
		if err != nil {
			s.logger.Warnf("[login] 网页登录失败：回调地址形态无效")
			writeError(w, http.StatusBadRequest, apiError{
				Message: errInvalidLoginCallback.Error(),
				Type:    typeInvalidRequest,
				Code:    "invalid_login_callback",
			})
			return
		}
		code, state = parsed.Code, parsed.State
	}

	if code == "" {
		writeError(w, http.StatusBadRequest, apiError{
			Message: "登录回调里没有授权码",
			Type:    typeInvalidRequest,
			Code:    "login_code_required",
		})
		return
	}
	if len([]rune(code)) > maxLoginCodeLength {
		writeError(w, http.StatusBadRequest, apiError{
			Message: "登录回调里没有授权码",
			Type:    typeInvalidRequest,
			Code:    "login_code_required",
		})
		return
	}
	// state 缺失/过长/不匹配/已用过/已过期：一律 400，且绝不调用上游
	//（否则等于拿别人的 code 换凭证写进自己的账号库）。
	if state == "" || len([]rune(state)) > maxLoginStateLength {
		s.logger.Warnf("[login] 网页登录失败：state 校验未通过")
		writeError(w, http.StatusBadRequest, apiError{
			Message: "登录回调 state 校验失败，请重新发起登录",
			Type:    typeInvalidRequest,
			Code:    "login_state_invalid",
		})
		return
	}
	ok, expired := s.login.consume(state)
	if !ok {
		s.logger.Warnf("[login] 网页登录失败：state 校验未通过")
		if expired {
			writeError(w, http.StatusBadRequest, apiError{
				Message: "本次登录已超时，请重新发起网页登录",
				Type:    typeInvalidRequest,
				Code:    "login_state_expired",
			})
			return
		}
		writeError(w, http.StatusBadRequest, apiError{
			Message: "登录回调 state 校验失败，请重新发起登录",
			Type:    typeInvalidRequest,
			Code:    "login_state_invalid",
		})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Upstream.RefreshTimeout())
	defer cancel()
	tokens, err := s.client.ExchangeAuthorizationCode(ctx, code)
	if err != nil {
		s.logger.Warnf("[login] 网页登录失败：%s", loginErrorTag(err))
		s.writeLoginExchangeError(w, err)
		return
	}

	account := &pool.Account{
		ID:           accountIDFromToken(tokens.AccessToken),
		UserID:       upstream.JWTUserID(tokens.AccessToken),
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		ExpiresAt:    upstream.JWTExpiry(tokens.AccessToken),
		Enabled:      true,
		Priority:     100,
	}
	setOfficeFields(account, tokens)
	updated, err := s.pool.Upsert(account)
	if err != nil {
		s.logger.Warnf("[login] 网页登录失败：回写账号文件失败")
		writeError(w, http.StatusInternalServerError, apiError{
			Message: "回写账号文件失败：" + err.Error(),
			Type:    typeUpstream,
			Code:    "persist_failed",
		})
		return
	}
	s.logger.Infof("[login] 网页登录成功 id=%s updated=%v", account.ID, updated)
	view, _ := s.pool.View(account.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      account.ID,
		"updated": updated,
		"account": view,
	})
}

// setOfficeFields 把上游返回的非空 office_* 写进账号记录的未知字段（SPEC5 §K2）。
func setOfficeFields(account *pool.Account, tokens *upstream.AuthorizationCode) {
	for key, value := range map[string]string{
		"office_identity": tokens.OfficeIdentity,
		"office_org_name": tokens.OfficeOrgName,
		"office_org_role": tokens.OfficeOrgRole,
	} {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		raw, err := json.Marshal(value)
		if err != nil {
			continue
		}
		account.SetExtra(key, raw)
	}
}

// writeLoginExchangeError 把换凭证失败映射成 SPEC5 §K2 的状态码与中文文案。
func (s *Server) writeLoginExchangeError(w http.ResponseWriter, err error) {
	var codeErr *upstream.AuthCodeError
	if errors.As(err, &codeErr) {
		if codeErr.HasCode && codeErr.Code == upstream.CodeExpired {
			writeError(w, http.StatusBadRequest, apiError{
				Message: "授权码已失效，请重新发起网页登录",
				Type:    typeInvalidRequest,
				Code:    "login_code_expired",
			})
			return
		}
		if codeErr.HTTPStatus >= 400 && codeErr.HTTPStatus < 500 {
			message := codeErr.Message
			if message == "" || message == "internal_server_error" {
				message = "登录服务异常，请稍后重试"
			}
			writeError(w, codeErr.HTTPStatus, apiError{
				Message: message,
				Type:    typeInvalidRequest,
				Code:    "login_upstream_error",
			})
			return
		}
		message := codeErr.Message
		if message == "" {
			message = "登录服务异常，请稍后重试"
		}
		writeError(w, http.StatusBadGateway, apiError{
			Message: "网页登录换取凭证失败: " + message,
			Type:    typeUpstream,
			Code:    "login_upstream_error",
		})
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		writeError(w, http.StatusGatewayTimeout, apiError{
			Message: "网页登录换取凭证失败: 请求超时",
			Type:    typeUpstream,
			Code:    "login_timeout",
		})
		return
	}
	writeError(w, http.StatusBadGateway, apiError{
		Message: "网页登录换取凭证失败: " + err.Error(),
		Type:    typeUpstream,
		Code:    "login_upstream_error",
	})
}

// loginErrorTag 给日志用的短分类（绝不包含 code / token）。
func loginErrorTag(err error) string {
	var codeErr *upstream.AuthCodeError
	if errors.As(err, &codeErr) {
		if codeErr.HasCode && codeErr.Code == upstream.CodeExpired {
			return "授权码已失效"
		}
		return fmt.Sprintf("上游 HTTP %d", codeErr.HTTPStatus)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "上游超时"
	}
	return "上游网络错误"
}
