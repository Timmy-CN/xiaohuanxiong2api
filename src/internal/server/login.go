package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

// newLoginState 生成 32 字节随机 state → 64 位十六进制。
//
// 64 字符是授权页 loopback 契约的硬性要求（raccoon 插件实测：desktop_callback=loopback
// 只认 64 字符十六进制 state，短了授权页直接报「授权链接无效」）；手动粘贴流程共用
// 同一长度，上游对非 loopback 形态不做长度校验。
func newLoginState() (string, error) {
	var buf [32]byte
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

	account := loginAccountFromTokens(tokens)
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

// loginAccountFromTokens 把换回的凭证组装成账号记录（office_* 字段一并写入）。
func loginAccountFromTokens(tokens *upstream.AuthorizationCode) *pool.Account {
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
	return account
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

// ── 浏览器自动回跳（desktop_callback=loopback）──
//
// 与手动粘贴的区别：本机起一个一次性回跳监听器，授权页带上
// desktop_callback=loopback 与 redirect=http://127.0.0.1:<port>/auth/callback，
// 浏览器确认授权后会自动跳回本服务完成登录，无需手动复制回调地址。
// 仅当浏览器与 xhx2api 同机（或能访问其 127.0.0.1）时可用；否则请用手动粘贴。

// loopbackCallbackPath 是回跳监听器唯一接受的 GET 路径。
const loopbackCallbackPath = "/auth/callback"

// loopbackListener 是一次自动回跳登录的本地监听器及其结果。
type loopbackListener struct {
	state string
	port  int
	ln    net.Listener
	srv   *http.Server
	timer *time.Timer

	mu        sync.Mutex
	received  bool   // 已受理过一次有效回调（重复回调拒绝）
	status    string // pending | success | failed
	accountID string
	errText   string
}

func newLoopbackListener(state string, port int) *loopbackListener {
	return &loopbackListener{state: state, port: port, status: "pending"}
}

// snapshot 返回当前状态（供 status 轮询）。
func (l *loopbackListener) snapshot() (status, accountID, errText string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.status, l.accountID, l.errText
}

// settle 落定结果并停掉监听；只有 pending 态才落定（幂等）。
//
// 立即关停监听端口（不再接受新回调），srv 延迟 1 秒再关：settle 多从回调
// 处理器内部调用，同步 srv.Close() 会把在途的回调响应一起掐断。
func (l *loopbackListener) settle(status, accountID, errText string) {
	l.mu.Lock()
	if l.status != "pending" {
		l.mu.Unlock()
		return
	}
	l.status = status
	l.accountID = accountID
	l.errText = errText
	timer := l.timer
	ln := l.ln
	l.mu.Unlock()
	if timer != nil {
		timer.Stop()
	}
	if ln != nil {
		ln.Close()
	}
	time.AfterFunc(time.Second, func() { l.srv.Close() })
}

// loopbackManager 管理当前活动的回跳监听器；同一时刻只保留一个，
// 新发起的会顶掉旧的（旧的结果随之一并作废）。
type loopbackManager struct {
	mu   sync.Mutex
	item *loopbackListener
}

func (m *loopbackManager) replace(l *loopbackListener) {
	m.mu.Lock()
	prev := m.item
	m.item = l
	m.mu.Unlock()
	if prev != nil {
		prev.settle("failed", "", "已被新的自动回跳登录取代")
	}
}

func (m *loopbackManager) lookup(state string) (*loopbackListener, bool) {
	m.mu.Lock()
	l := m.item
	m.mu.Unlock()
	if l != nil && l.state == state {
		return l, true
	}
	return nil, false
}

// buildLoopbackAuthorizeURL 构造带自动回跳参数的授权页地址。
//
// 当前授权页的 loopback 契约（raccoon 插件实测）：desktop_callback=loopback，
// redirect 指向 http://127.0.0.1:<port>/auth/callback。
func buildLoopbackAuthorizeURL(origin, appName, state, redirect string) (string, error) {
	base, err := url.Parse(origin)
	if err != nil {
		return "", err
	}
	base.Path = "/code/authorize"
	query := base.Query()
	query.Set("login_source", loginSource)
	query.Set("appname", appName)
	query.Set("state", state)
	query.Set("desktop_callback", "loopback")
	query.Set("redirect", redirect)
	base.RawQuery = query.Encode()
	return base.String(), nil
}

// newLoopbackHandler 生成回跳监听器的 HTTP 处理器。
//
// 只接受：回跳 URL 里的那个 host、GET /auth/callback、逐字相等的 state。
// 响应页不含 code/token，且禁缓存。浏览器中途关页不会通知本服务，
// 由 state 的 TTL 定时器兜底关停。
func (s *Server) newLoopbackHandler(l *loopbackListener) http.Handler {
	wantHost := fmt.Sprintf("127.0.0.1:%d", l.port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := func(status int, text string) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, text)
		}
		if r.URL.Path != loopbackCallbackPath || r.Host != wantHost {
			reply(http.StatusNotFound, "Not found")
			return
		}
		if r.Method != http.MethodGet {
			reply(http.StatusMethodNotAllowed, "Method not allowed")
			return
		}
		q := r.URL.Query()
		if q.Get("state") != l.state {
			reply(http.StatusBadRequest, "回调不属于本次登录，请回到小浣熊反代重新发起。")
			return
		}
		l.mu.Lock()
		received := l.received
		pending := l.status == "pending"
		l.mu.Unlock()
		if received || !pending {
			reply(http.StatusConflict, "本次回调已经处理，请回到小浣熊反代查看结果。")
			return
		}
		if q.Get("error") != "" {
			reply(http.StatusBadRequest, "登录未获授权，请回到小浣熊反代重新发起。")
			l.settle("failed", "", "上游授权被拒绝")
			return
		}
		code := strings.TrimSpace(q.Get("code"))
		if code == "" || len([]rune(code)) > maxLoginCodeLength {
			reply(http.StatusBadRequest, "回调缺少有效授权码，请回到原授权页重试。")
			return
		}
		// state 一次性消费：与手动粘贴路径共用同一张 pending 表，谁先到谁赢。
		ok, expired := s.login.consume(l.state)
		if !ok {
			text := "回调不属于本次登录，请回到小浣熊反代重新发起。"
			if expired {
				text = "本次登录已超时，请重新发起自动回跳登录。"
			}
			reply(http.StatusBadRequest, text)
			l.settle("failed", "", "登录状态已失效")
			return
		}
		// 用独立于浏览器连接的超时：浏览器等不到响应就关页，登录也应完成。
		ctx, cancel := context.WithTimeout(context.Background(), s.cfg.Upstream.RefreshTimeout())
		defer cancel()
		tokens, err := s.client.ExchangeAuthorizationCode(ctx, code)
		if err != nil {
			reply(http.StatusBadGateway, "登录未完成，请回到小浣熊反代查看错误并重试。")
			l.settle("failed", "", "换取凭证失败："+loginErrorTag(err))
			return
		}
		account := loginAccountFromTokens(tokens)
		updated, err := s.pool.Upsert(account)
		if err != nil {
			reply(http.StatusBadGateway, "登录未完成，请回到小浣熊反代查看错误并重试。")
			l.settle("failed", "", "回写账号文件失败："+err.Error())
			return
		}
		s.logger.Infof("[login] 自动回跳登录成功 id=%s updated=%v", account.ID, updated)
		reply(http.StatusOK, "登录成功，可以关闭此页并回到小浣熊反代。")
		l.settle("success", account.ID, "")
	})
}

// adminLoginLoopbackStart 实现 POST /admin/api/login/loopback/start。
//
// 返回授权页 URL（带 desktop_callback=loopback 与 redirect）与本地监听端口。
func (s *Server) adminLoginLoopbackStart(w http.ResponseWriter, r *http.Request) {
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
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		_, _ = s.login.consume(state) // 监听起不来就别留一个无法使用的 state
		s.logger.Warnf("[login] 自动回跳监听启动失败：%v", err)
		writeError(w, http.StatusInternalServerError, apiError{
			Message: "启动回跳监听失败：" + err.Error(),
			Type:    typeUpstream,
			Code:    "loopback_listen_failed",
		})
		return
	}
	port := ln.Addr().(*net.TCPAddr).Port
	listener := newLoopbackListener(state, port)
	listener.ln = ln
	listener.srv = &http.Server{
		Handler:           s.newLoopbackHandler(listener),
		ReadHeaderTimeout: 10 * time.Second,
	}
	s.loopback.replace(listener)
	go func() { _ = listener.srv.Serve(ln) }()
	listener.mu.Lock()
	listener.timer = time.AfterFunc(ttl, func() {
		listener.settle("failed", "", "登录超时，请重新发起自动回跳登录")
	})
	listener.mu.Unlock()

	redirect := fmt.Sprintf("http://127.0.0.1:%d%s", port, loopbackCallbackPath)
	authorizeURL, err := buildLoopbackAuthorizeURL(s.cfg.Upstream.OriginURL(), s.cfg.Login.LoginAppName(), state, redirect)
	if err != nil {
		listener.settle("failed", "", "构造登录地址失败")
		s.logger.Warnf("[login] 自动回跳登录地址构造失败：%v", err)
		writeError(w, http.StatusInternalServerError, apiError{
			Message: "构造登录地址失败：" + err.Error(),
			Type:    typeUpstream,
			Code:    "login_url_failed",
		})
		return
	}
	s.logger.Infof("[login] 发起自动回跳登录 port=%d", port)
	writeJSONUnescaped(w, http.StatusOK, map[string]any{
		"url":         authorizeURL,
		"state":       state,
		"port":        port,
		"expires_at":  expiresAt,
		"ttl_seconds": int(ttl.Seconds()),
	})
}

// adminLoginLoopbackStatus 实现 GET /admin/api/login/loopback/status?state=…。
//
// status: pending（等待回调）/ success（id 为新登录的账号）/ failed（error 为原因）。
func (s *Server) adminLoginLoopbackStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	if !s.requireAdminWrite(w) {
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state == "" {
		writeError(w, http.StatusBadRequest, apiError{
			Message: "缺少 state 参数",
			Type:    typeInvalidRequest,
			Code:    "state_required",
		})
		return
	}
	listener, ok := s.loopback.lookup(state)
	if !ok {
		writeError(w, http.StatusNotFound, apiError{
			Message: "没有进行中的自动回跳登录（可能已过期、被新请求取代或服务已重启）",
			Type:    typeInvalidRequest,
			Code:    "loopback_not_found",
		})
		return
	}
	status, accountID, errText := listener.snapshot()
	payload := map[string]any{"status": status}
	if accountID != "" {
		payload["id"] = accountID
	}
	if errText != "" {
		payload["error"] = errText
	}
	writeJSON(w, http.StatusOK, payload)
}
