package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xhx2api/internal/config"
)

// loginStart 调 /admin/api/login/start 并解析响应。
func (e *testEnv) loginStart(t *testing.T, key string) (int, map[string]any) {
	t.Helper()
	resp, data := e.writeReq(t, http.MethodPost, "/admin/api/login/start", "", key)
	var parsed map[string]any
	_ = json.Unmarshal(data, &parsed)
	return resp.StatusCode, parsed
}

// loginExchange 调 /admin/api/login/exchange，body 为已序列化的 JSON 字符串。
func (e *testEnv) loginExchange(t *testing.T, key, body string) (int, []byte) {
	t.Helper()
	resp, data := e.writeReq(t, http.MethodPost, "/admin/api/login/exchange", body, key)
	return resp.StatusCode, data
}

// TestLoginStart 覆盖 §L.1：url 组成 / appname 编码 / state 唯一 / enabled=false → 403。
func TestLoginStart(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	const key = "k-login-start"
	env := newTestEnv(t, handler, nil, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	status, first := env.loginStart(t, key)
	if status != http.StatusOK {
		t.Fatalf("start 状态码 = %d, 期望 200", status)
	}
	rawURL, _ := first["url"].(string)
	state, _ := first["state"].(string)
	if rawURL == "" || state == "" {
		t.Fatalf("start 返回缺少 url/state: %+v", first)
	}
	// 64 位十六进制是授权页 loopback 契约的硬性要求（短了报「授权链接无效」）。
	if len(state) != 64 {
		t.Fatalf("state 长度 = %d, 期望 64 位十六进制", len(state))
	}
	for _, r := range state {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("state 含非十六进制字符: %q", state)
		}
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("返回的 url 不可解析: %v", err)
	}
	if parsed.Path != "/code/authorize" {
		t.Fatalf("url path = %q, 期望 /code/authorize", parsed.Path)
	}
	if !strings.Contains(rawURL, "login_source=desktop") {
		t.Fatalf("url 应含 login_source=desktop: %s", rawURL)
	}
	if got := parsed.Query().Get("appname"); got != "办公小浣熊客户端" {
		t.Fatalf("appname = %q, 期望 办公小浣熊客户端", got)
	}
	if got := parsed.Query().Get("state"); got != state {
		t.Fatalf("url 里的 state = %q, 与返回的 state %q 不一致", got, state)
	}
	if strings.Contains(rawURL, "办公") {
		t.Fatalf("appname 应已百分号编码，不应出现原始中文: %s", rawURL)
	}
	if ttl, _ := first["ttl_seconds"].(float64); int(ttl) != 600 {
		t.Fatalf("ttl_seconds = %v, 期望 600", first["ttl_seconds"])
	}
	if exp, _ := first["expires_at"].(float64); exp <= 0 {
		t.Fatalf("expires_at 应 > 0，实际 %v", first["expires_at"])
	}

	// 连续两次 state 不同。
	status, second := env.loginStart(t, key)
	if status != http.StatusOK {
		t.Fatalf("第二次 start 状态码 = %d", status)
	}
	if second["state"] == state {
		t.Fatalf("两次调用 state 应不同，实际都是 %v", state)
	}

	// /admin/api/status 暴露 login.enabled 与 pending 数。
	resp, err := http.Get(env.hs.URL + "/admin/api/status?key=" + key)
	if err != nil {
		t.Fatalf("请求 status 失败: %v", err)
	}
	statusBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var statusPayload struct {
		Login struct {
			Enabled bool `json:"enabled"`
			Pending int  `json:"pending"`
		} `json:"login"`
	}
	if err := json.Unmarshal(statusBody, &statusPayload); err != nil {
		t.Fatalf("解析 status 失败: %v", err)
	}
	if !statusPayload.Login.Enabled || statusPayload.Login.Pending < 2 {
		t.Fatalf("status.login = %+v, 期望 enabled=true pending>=2", statusPayload.Login)
	}

	// login.enabled=false → 403。
	env2 := newTestEnv(t, handler, nil, func(cfg *config.Config) {
		cfg.Admin.APIKey = sp(key)
		cfg.Login.Enabled = bp(false)
	})
	resp, data := env2.writeReq(t, http.MethodPost, "/admin/api/login/start", "", key)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("enabled=false start 状态码 = %d, 期望 403, body=%s", resp.StatusCode, data)
	}
	if !strings.Contains(string(data), "网页登录已禁用") {
		t.Fatalf("enabled=false 文案不对: %s", data)
	}
}

// TestLoginExchangeSuccessAndUpdate 覆盖 §L.2：换凭证落账号 + 重复登录 updated=true。
func TestLoginExchangeSuccessAndUpdate(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).Unix()
	token := testJWTUser(exp, "7590626")
	var calls int64
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login_with_authorization_code" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt64(&calls, 1)
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0, "message": "ok",
			"data": map[string]any{
				"access_token":    token,
				"refresh_token":   "rt-1",
				"office_identity": "id-1",
				"office_org_name": "组织A",
				"office_org_role": "admin",
			},
		})
	}
	const key = "k-login-exchange"
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "seed", "access_token": "tok", "enabled": true, "priority": 1,
	}}, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	_, start := env.loginStart(t, key)
	state, _ := start["state"].(string)
	callback := fmt.Sprintf("office-raccoon://auth/callback?code=CODE-1&state=%s", state)
	status, data := env.loginExchange(t, key, fmt.Sprintf(`{"callback_url":%q}`, callback))
	if status != http.StatusOK {
		t.Fatalf("exchange 状态码 = %d, 期望 200, body=%s", status, data)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("解析 exchange 响应失败: %v", err)
	}
	if out["id"] != "user-7590626" {
		t.Fatalf("id = %v, 期望 user-7590626", out["id"])
	}
	if out["updated"] != false {
		t.Fatalf("首次登录 updated = %v, 期望 false", out["updated"])
	}
	account, _ := out["account"].(map[string]any)
	if account == nil || account["id"] != "user-7590626" {
		t.Fatalf("account 字段应与 GET /admin/api/accounts 单条同形: %v", out["account"])
	}
	if account["has_refresh_token"] != true {
		t.Fatalf("account.has_refresh_token 应为 true: %v", account)
	}

	accounts := readAccountsFile(t, env.s.cfg.Accounts.AccountsFile())
	created := findAccount(accounts, "user-7590626")
	if created == nil {
		t.Fatalf("账号文件应出现新账号: %+v", accounts)
	}
	var gotExp int64
	_ = json.Unmarshal(created["expires_at"], &gotExp)
	if gotExp != exp*1000 {
		t.Fatalf("expires_at = %d, 期望 %d（与 JWT 一致）", gotExp, exp*1000)
	}
	if string(created["office_identity"]) != `"id-1"` {
		t.Fatalf("office_identity 应保留: %s", created["office_identity"])
	}
	if string(created["office_org_name"]) != `"组织A"` || string(created["office_org_role"]) != `"admin"` {
		t.Fatalf("office_org_* 应保留: %s / %s", created["office_org_name"], created["office_org_role"])
	}

	// 同一账号再登录一次 → updated:true，文件里只有一条该 id。
	_, start2 := env.loginStart(t, key)
	state2, _ := start2["state"].(string)
	callback2 := fmt.Sprintf("office-raccoon://auth/callback?code=CODE-2&state=%s", state2)
	status, data = env.loginExchange(t, key, fmt.Sprintf(`{"callback_url":%q}`, callback2))
	if status != http.StatusOK {
		t.Fatalf("第二次 exchange 状态码 = %d, body=%s", status, data)
	}
	_ = json.Unmarshal(data, &out)
	if out["updated"] != true {
		t.Fatalf("同 id 再登录 updated = %v, 期望 true", out["updated"])
	}
	accounts = readAccountsFile(t, env.s.cfg.Accounts.AccountsFile())
	count := 0
	for _, a := range accounts {
		var id string
		_ = json.Unmarshal(a["id"], &id)
		if id == "user-7590626" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("同一 id 在文件里应只有一条，实际 %d 条: %+v", count, accounts)
	}
	if atomic.LoadInt64(&calls) != 2 {
		t.Fatalf("上游调用次数 = %d, 期望 2", calls)
	}
}

// TestLoginStateSecurity 覆盖 §L.3：state 不匹配/已消费/超时/回调形态不对。
func TestLoginStateSecurity(t *testing.T) {
	var calls int64
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login_with_authorization_code" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt64(&calls, 1)
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0, "message": "ok",
			"data": map[string]any{"access_token": testJWTUser(time.Now().Add(time.Hour).Unix(), "1"), "refresh_token": "rt"},
		})
	}
	const key = "k-login-sec"
	env := newTestEnv(t, handler, nil, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	_, start := env.loginStart(t, key)
	realState, _ := start["state"].(string)

	// (a) state 不匹配 → 400 且桩上游零调用。
	status, data := env.loginExchange(t, key, `{"code":"C","state":"bogus"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("state 不匹配状态码 = %d, 期望 400, body=%s", status, data)
	}
	if !strings.Contains(string(data), "state 校验失败") {
		t.Fatalf("state 不匹配文案不对: %s", data)
	}
	if atomic.LoadInt64(&calls) != 0 {
		t.Fatalf("state 校验失败时不应调用上游，实际调用 %d 次", calls)
	}

	// (b) 回调形态不对 → 400，且零上游调用。
	for _, bad := range []string{
		"https://evil/callback?code=C&state=" + realState,
		"office-raccoon://auth/callback@evil.com?code=C&state=" + realState,
	} {
		status, data = env.loginExchange(t, key, fmt.Sprintf(`{"callback_url":%q}`, bad))
		if status != http.StatusBadRequest {
			t.Fatalf("形态不对 %q 状态码 = %d, 期望 400, body=%s", bad, status, data)
		}
		if !strings.Contains(string(data), "登录回调地址无效") {
			t.Fatalf("形态不对文案不对: %s", data)
		}
	}
	if atomic.LoadInt64(&calls) != 0 {
		t.Fatalf("形态不对时不应调用上游，实际调用 %d 次", calls)
	}

	// (c) 正常消费一次 → 200。
	callback := fmt.Sprintf("office-raccoon://auth/callback?code=C&state=%s", realState)
	status, data = env.loginExchange(t, key, fmt.Sprintf(`{"callback_url":%q}`, callback))
	if status != http.StatusOK {
		t.Fatalf("正常 exchange 状态码 = %d, body=%s", status, data)
	}
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("正常 exchange 应调用上游 1 次，实际 %d", calls)
	}

	// (d) 同一 state 用第二次 → 400（已消费），上游不再被调用。
	status, data = env.loginExchange(t, key, fmt.Sprintf(`{"callback_url":%q}`, callback))
	if status != http.StatusBadRequest {
		t.Fatalf("重复使用 state 状态码 = %d, 期望 400, body=%s", status, data)
	}
	if !strings.Contains(string(data), "state 校验失败") {
		t.Fatalf("重复使用 state 文案不对: %s", data)
	}
	if atomic.LoadInt64(&calls) != 1 {
		t.Fatalf("重复使用 state 不应再调用上游，实际 %d", calls)
	}

	// (e) 回调里没有 code → 400。
	status, data = env.loginExchange(t, key,
		fmt.Sprintf(`{"callback_url":%q}`, "office-raccoon://auth/callback?state="+realState))
	if status != http.StatusBadRequest || !strings.Contains(string(data), "没有授权码") {
		t.Fatalf("缺 code 响应 = %d %s", status, data)
	}

	// (f) state_ttl_seconds=0 → 超时（注入时钟，确定性）。
	env2 := newTestEnv(t, handler, nil, func(cfg *config.Config) {
		cfg.Admin.APIKey = sp(key)
		cfg.Login.StateTTLSeconds = ip(0)
	})
	base := time.Now()
	env2.s.login.now = func() time.Time { return base }
	_, start2 := env2.loginStart(t, key)
	state2, _ := start2["state"].(string)
	status, data = env2.loginExchange(t, key, fmt.Sprintf(`{"code":"C","state":%q}`, state2))
	if status != http.StatusBadRequest {
		t.Fatalf("ttl=0 状态码 = %d, 期望 400, body=%s", status, data)
	}
	if !strings.Contains(string(data), "本次登录已超时") {
		t.Fatalf("ttl=0 文案不对: %s", data)
	}
}

// TestLoginUpstreamErrors 覆盖 §L.4：200035 → 400；500 → 502；4xx 透传。
func TestLoginUpstreamErrors(t *testing.T) {
	var mode int32 = 1
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login_with_authorization_code" {
			http.NotFound(w, r)
			return
		}
		switch atomic.LoadInt32(&mode) {
		case 1:
			writeJSON(w, http.StatusOK, map[string]any{"code": 200035, "message": "code expired"})
		case 2:
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 500000, "message": "boom"})
		case 3:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 400001, "message": "bad code"})
		case 4:
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 400002, "message": "internal_server_error"})
		}
	}
	const key = "k-login-err"
	env := newTestEnv(t, handler, nil, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	exchange := func() (int, string) {
		_, start := env.loginStart(t, key)
		state, _ := start["state"].(string)
		status, data := env.loginExchange(t, key, fmt.Sprintf(`{"code":"C","state":%q}`, state))
		return status, string(data)
	}

	status, body := exchange()
	if status != http.StatusBadRequest || !strings.Contains(body, "授权码已失效") {
		t.Fatalf("200035 响应 = %d %s", status, body)
	}

	atomic.StoreInt32(&mode, 2)
	status, body = exchange()
	if status != http.StatusBadGateway || !strings.Contains(body, "网页登录换取凭证失败") {
		t.Fatalf("上游 500 响应 = %d %s", status, body)
	}

	atomic.StoreInt32(&mode, 3)
	status, body = exchange()
	if status != http.StatusBadRequest || !strings.Contains(body, "bad code") {
		t.Fatalf("上游 400 响应 = %d %s", status, body)
	}

	atomic.StoreInt32(&mode, 4)
	status, body = exchange()
	if status != http.StatusBadRequest || !strings.Contains(body, "登录服务异常，请稍后重试") {
		t.Fatalf("internal_server_error 响应 = %d %s", status, body)
	}
}

// TestLoginRequiresAdminKey 覆盖 §L.5：admin.api_key 为空 → start/exchange 都 403。
func TestLoginRequiresAdminKey(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	env := newTestEnv(t, handler, nil, nil) // admin.api_key 默认空

	for _, path := range []string{"/admin/api/login/start", "/admin/api/login/exchange"} {
		resp, data := env.writeReq(t, http.MethodPost, path, `{"code":"x","state":"y"}`, "")
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s 状态码 = %d, 期望 403, body=%s", path, resp.StatusCode, data)
		}
		if !strings.Contains(string(data), "admin.api_key") {
			t.Fatalf("%s 未返回 admin.api_key 提示: %s", path, data)
		}
	}
}

// TestLoginExchangeDedupesHexIssAccount：真实小浣熊 JWT 把用户 id 放在 iss
// （十六进制串）。同账号重复登录必须落同一个 user-<十进制> id（updated=true），
// 而不是识别不出 uid、各落一个随机 acct-*。
func TestLoginExchangeDedupesHexIssAccount(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login_with_authorization_code" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0, "message": "ok",
			"data": map[string]any{
				"access_token":  testJWTiss(time.Now().Add(2*time.Hour).Unix(), "74350b"),
				"refresh_token": "rt-1",
			},
		})
	}
	const key = "k-login-hex-iss"
	env := newTestEnv(t, handler, nil, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	_, start := env.loginStart(t, key)
	state, _ := start["state"].(string)
	callback := fmt.Sprintf("office-raccoon://auth/callback?code=CODE-1&state=%s", state)
	status, data := env.loginExchange(t, key, fmt.Sprintf(`{"callback_url":%q}`, callback))
	if status != http.StatusOK {
		t.Fatalf("exchange 状态码 = %d, 期望 200, body=%s", status, data)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("解析 exchange 响应失败: %v", err)
	}
	if out["id"] != "user-7615755" {
		t.Fatalf("id = %v, 期望 user-7615755（0x74350b 的十进制）", out["id"])
	}

	// 同一账号再登录一次 → updated:true，文件里只有一条该 id。
	_, start2 := env.loginStart(t, key)
	state2, _ := start2["state"].(string)
	callback2 := fmt.Sprintf("office-raccoon://auth/callback?code=CODE-2&state=%s", state2)
	status2, data2 := env.loginExchange(t, key, fmt.Sprintf(`{"callback_url":%q}`, callback2))
	if status2 != http.StatusOK {
		t.Fatalf("二次 exchange 状态码 = %d, 期望 200, body=%s", status2, data2)
	}
	var out2 map[string]any
	if err := json.Unmarshal(data2, &out2); err != nil {
		t.Fatalf("解析二次 exchange 响应失败: %v", err)
	}
	if out2["updated"] != true {
		t.Fatalf("同 id 再登录 updated = %v, 期望 true", out2["updated"])
	}
	accounts := readAccountsFile(t, env.s.cfg.Accounts.AccountsFile())
	count := 0
	for _, a := range accounts {
		var id string
		_ = json.Unmarshal(a["id"], &id)
		if id == "user-7615755" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("同账号重复登录应只落一条 user-7615755，实际 %d 条", count)
	}
}

// loopbackStart 调 /admin/api/login/loopback/start，返回 url/state/port。
func loopbackStart(t *testing.T, e *testEnv, key string) (string, string, int) {
	t.Helper()
	resp, data := e.writeReq(t, http.MethodPost, "/admin/api/login/loopback/start", "", key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback start 状态码 = %d, body=%s", resp.StatusCode, data)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("解析 start 响应失败: %v", err)
	}
	rawURL, _ := out["url"].(string)
	state, _ := out["state"].(string)
	port := int(out["port"].(float64))
	if rawURL == "" || state == "" || port == 0 {
		t.Fatalf("start 返回缺少 url/state/port: %v", out)
	}
	return rawURL, state, port
}

func loopbackStatus(t *testing.T, e *testEnv, key, state string) (int, map[string]any) {
	t.Helper()
	resp, data := e.writeReq(t, http.MethodGet, "/admin/api/login/loopback/status?state="+url.QueryEscape(state), "", key)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return resp.StatusCode, out
}

// 完整流程：start → URL 带回跳参数 → 浏览器回调（真实打到 127.0.0.1 监听器）
// → 换凭证落账号 → status success → state 已消费。
func TestLoginLoopbackFlow(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login_with_authorization_code" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"code": 0, "message": "ok",
			"data": map[string]any{
				"access_token":  testJWTiss(time.Now().Add(2*time.Hour).Unix(), "74350b"),
				"refresh_token": "rt-1",
			},
		})
	}
	const key = "k-loopback-flow"
	env := newTestEnv(t, handler, nil, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	rawURL, state, port := loopbackStart(t, env, key)

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("授权地址不可解析: %v", err)
	}
	if parsed.Path != "/code/authorize" {
		t.Fatalf("授权地址 path = %q", parsed.Path)
	}
	q := parsed.Query()
	if q.Get("desktop_callback") != "loopback" {
		t.Fatalf("应带 desktop_callback=loopback: %s", rawURL)
	}
	if q.Get("redirect") != fmt.Sprintf("http://127.0.0.1:%d/auth/callback", port) {
		t.Fatalf("redirect = %q", q.Get("redirect"))
	}
	if q.Get("state") != state {
		t.Fatalf("授权地址 state 与返回值不一致")
	}

	if code, out := loopbackStatus(t, env, key, state); code != http.StatusOK || out["status"] != "pending" {
		t.Fatalf("回调前 status = %d %v", code, out)
	}

	// 模拟浏览器回跳：真实请求 127.0.0.1 监听器。
	cb, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/auth/callback?code=CODE-1&state=%s", port, url.QueryEscape(state)))
	if err != nil {
		t.Fatalf("请求回跳监听器失败: %v", err)
	}
	body, _ := io.ReadAll(cb.Body)
	cb.Body.Close()
	if cb.StatusCode != http.StatusOK || !strings.Contains(string(body), "登录成功") {
		t.Fatalf("回跳响应 = %d %s", cb.StatusCode, body)
	}

	code, out := loopbackStatus(t, env, key, state)
	if code != http.StatusOK || out["status"] != "success" || out["id"] != "user-7615755" {
		t.Fatalf("回调后 status = %d %v", code, out)
	}

	// 账号已落库。
	accounts := readAccountsFile(t, env.s.cfg.Accounts.AccountsFile())
	count := 0
	for _, a := range accounts {
		var id string
		_ = json.Unmarshal(a["id"], &id)
		if id == "user-7615755" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("应落一条 user-7615755，实际 %d 条", count)
	}

	// state 已被监听器消费：手动 exchange 同 state → 400。
	status, data := env.loginExchange(t, key, fmt.Sprintf(`{"code":"C2","state":%q}`, state))
	if status != http.StatusBadRequest {
		t.Fatalf("已消费 state 的 exchange = %d, 期望 400, body=%s", status, data)
	}

	// 重复回调：监听器已落定收摊（端口关闭）或竞态窗口内 409，都不再受理。
	cb2, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/auth/callback?code=CODE-2&state=%s", port, url.QueryEscape(state)))
	if err == nil {
		cb2.Body.Close()
		if cb2.StatusCode != http.StatusConflict {
			t.Fatalf("重复回调 = %d, 期望 409", cb2.StatusCode)
		}
	}
}

// state 不匹配 → 400 且保持 pending；error 参数 → failed。
func TestLoginLoopbackRejectsAndFails(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	const key = "k-loopback-reject"
	env := newTestEnv(t, handler, nil, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	_, state, port := loopbackStart(t, env, key)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/auth/callback?code=X&state=wrong", port))
	if err != nil {
		t.Fatalf("请求回跳监听器失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("错误 state = %d, 期望 400", resp.StatusCode)
	}
	if code, out := loopbackStatus(t, env, key, state); code != http.StatusOK || out["status"] != "pending" {
		t.Fatalf("错误 state 后应保持 pending: %d %v", code, out)
	}

	resp2, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/auth/callback?state=%s&error=access_denied", port, url.QueryEscape(state)))
	if err != nil {
		t.Fatalf("请求回跳监听器失败: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("error 参数 = %d, 期望 400", resp2.StatusCode)
	}
	code, out := loopbackStatus(t, env, key, state)
	if code != http.StatusOK || out["status"] != "failed" || out["error"] != "上游授权被拒绝" {
		t.Fatalf("error 后 status = %d %v", code, out)
	}
}

// 新的 start 顶掉旧的：旧 state 查不到。
func TestLoginLoopbackSuperseded(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	const key = "k-loopback-sup"
	env := newTestEnv(t, handler, nil, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	_, state1, _ := loopbackStart(t, env, key)
	_, state2, _ := loopbackStart(t, env, key)

	if code, _ := loopbackStatus(t, env, key, state1); code != http.StatusNotFound {
		t.Fatalf("被顶掉的旧 state = %d, 期望 404", code)
	}
	if code, out := loopbackStatus(t, env, key, state2); code != http.StatusOK || out["status"] != "pending" {
		t.Fatalf("新 state = %d %v", code, out)
	}
}

// TTL 到点 → failed（登录超时）。
func TestLoginLoopbackTimeout(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	const key = "k-loopback-ttl"
	env := newTestEnv(t, handler, nil, func(cfg *config.Config) {
		cfg.Admin.APIKey = sp(key)
		cfg.Login.StateTTLSeconds = ip(1)
	})

	_, state, _ := loopbackStart(t, env, key)

	deadline := time.Now().Add(3 * time.Second)
	for {
		code, out := loopbackStatus(t, env, key, state)
		if code == http.StatusOK && out["status"] == "failed" {
			if msg, _ := out["error"].(string); !strings.Contains(msg, "超时") {
				t.Fatalf("超时原因 = %q", msg)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("3s 内未落定为 failed: %d %v", code, out)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// login.enabled=false → 403。
func TestLoginLoopbackDisabled(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	const key = "k-loopback-dis"
	env := newTestEnv(t, handler, nil, func(cfg *config.Config) {
		cfg.Admin.APIKey = sp(key)
		cfg.Login.Enabled = bp(false)
	})
	resp, data := env.writeReq(t, http.MethodPost, "/admin/api/login/loopback/start", "", key)
	if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(data), "网页登录已禁用") {
		t.Fatalf("disabled start = %d %s", resp.StatusCode, data)
	}
}
