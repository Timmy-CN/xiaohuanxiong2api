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
