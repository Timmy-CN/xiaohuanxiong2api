package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xhx2api/internal/config"
)

// testJWTUser 造一个含 exp 与 user_id 的假 JWT(不验签,只解 payload)。
func testJWTUser(exp int64, userID string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	raw, _ := json.Marshal(map[string]any{"exp": exp, "user_id": userID})
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return header + "." + payload + ".sig"
}

// testJWTiss 造一个含 exp 与十六进制 iss 的假 JWT（真实小浣熊签发形态）。
func testJWTiss(exp int64, iss string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	raw, _ := json.Marshal(map[string]any{"exp": exp, "iss": iss})
	payload := base64.RawURLEncoding.EncodeToString(raw)
	return header + "." + payload + ".sig"
}

// writeReq 发一个写请求;key 非空时走 Authorization: Bearer 头。
func (e *testEnv) writeReq(t *testing.T, method, path, body, key string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.hs.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

// readAccountsFile 读取账号文件为「每账号一个原始字段 map」,便于检查未知字段。
func readAccountsFile(t *testing.T, path string) []map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取账号文件失败: %v", err)
	}
	var parsed struct {
		Accounts []map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("解析账号文件失败: %v", err)
	}
	return parsed.Accounts
}

func findAccount(accounts []map[string]json.RawMessage, id string) map[string]json.RawMessage {
	for _, account := range accounts {
		var got string
		if err := json.Unmarshal(account["id"], &got); err == nil && got == id {
			return account
		}
	}
	return nil
}

// TestAdminWriteRequiresKey 覆盖「未设 admin.api_key 时写操作一律 403 且文件不动」。
func TestAdminWriteRequiresKey(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "a1", "access_token": "tok", "enabled": true, "priority": 1,
	}}, nil) // admin.api_key 默认空

	before, err := os.ReadFile(env.s.cfg.Accounts.AccountsFile())
	if err != nil {
		t.Fatalf("读取账号文件失败: %v", err)
	}

	cases := []struct{ method, path, body string }{
		{http.MethodPost, "/admin/api/accounts", `{"access_token":"x"}`},
		{http.MethodPatch, "/admin/api/accounts/a1", `{"enabled":false}`},
		{http.MethodDelete, "/admin/api/accounts/a1", ""},
	}
	for _, tc := range cases {
		resp, data := env.writeReq(t, tc.method, tc.path, tc.body, "")
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s 状态码 = %d, 期望 403, body=%s", tc.method, tc.path, resp.StatusCode, data)
		}
		if !strings.Contains(string(data), "admin.api_key") {
			t.Fatalf("%s %s 未返回中文提示: %s", tc.method, tc.path, data)
		}
	}

	after, err := os.ReadFile(env.s.cfg.Accounts.AccountsFile())
	if err != nil {
		t.Fatalf("读取账号文件失败: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("未设 key 时账号文件不应被改动\nbefore=%s\nafter=%s", before, after)
	}
}

// TestAdminWriteCRUD 覆盖「设了 key 后新增/冲突/更新/删除/404」以及未知字段保留。
func TestAdminWriteCRUD(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model_catalog" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"categories": []map[string]any{{"type": "x", "models": []map[string]any{{"name": "m1"}, {"name": "m2"}}}},
		})
	}
	const key = "adminkey-123"
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "seed", "access_token": "tok-seed", "enabled": true, "priority": 1, "custom_field": "keep-me",
	}}, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	token := testJWTUser(time.Now().Add(time.Hour).Unix(), "12345")
	addBody := fmt.Sprintf(`{"name":"测试账号","access_token":%q,"refresh_token":"rt","priority":5}`, token)

	// 新增 → 201 + 视图。
	resp, data := env.writeReq(t, http.MethodPost, "/admin/api/accounts", addBody, key)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("新增状态码 = %d, 期望 201, body=%s", resp.StatusCode, data)
	}
	var view map[string]any
	_ = json.Unmarshal(data, &view)
	if view["id"] != "user-12345" {
		t.Fatalf("新增账号 id = %v, 期望 user-12345", view["id"])
	}
	if view["token_tail"] != "345" && !strings.HasSuffix(token, fmt.Sprint(view["token_tail"])) {
		t.Fatalf("token_tail 应只含尾 4 位: %v", view["token_tail"])
	}
	if strings.Contains(string(data), token) {
		t.Fatalf("响应不应包含完整 access_token: %s", data)
	}

	accounts := readAccountsFile(t, env.s.cfg.Accounts.AccountsFile())
	if a := findAccount(accounts, "user-12345"); a == nil {
		t.Fatalf("账号文件应出现新账号: %+v", accounts)
	}
	seed := findAccount(accounts, "seed")
	if seed == nil {
		t.Fatalf("原有账号丢失: %+v", accounts)
	}
	if string(seed["custom_field"]) != `"keep-me"` {
		t.Fatalf("未知字段 custom_field 应保留: %s", seed["custom_field"])
	}

	// 同一 id 再来一次 → 409。
	resp, data = env.writeReq(t, http.MethodPost, "/admin/api/accounts", addBody, key)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("重复新增状态码 = %d, 期望 409, body=%s", resp.StatusCode, data)
	}

	// PATCH enabled=false, priority=7 → 200 且文件同步。
	resp, data = env.writeReq(t, http.MethodPatch, "/admin/api/accounts/user-12345", `{"enabled":false,"priority":7}`, key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH 状态码 = %d, 期望 200, body=%s", resp.StatusCode, data)
	}
	accounts = readAccountsFile(t, env.s.cfg.Accounts.AccountsFile())
	patched := findAccount(accounts, "user-12345")
	if patched == nil {
		t.Fatalf("PATCH 后账号丢失: %+v", accounts)
	}
	var enabled bool
	var priority int
	_ = json.Unmarshal(patched["enabled"], &enabled)
	_ = json.Unmarshal(patched["priority"], &priority)
	if enabled || priority != 7 {
		t.Fatalf("PATCH 未同步到文件: enabled=%v priority=%d", enabled, priority)
	}

	// 不存在的 id → 404。
	resp, data = env.writeReq(t, http.MethodPatch, "/admin/api/accounts/nope", `{"enabled":true}`, key)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("PATCH 不存在状态码 = %d, 期望 404, body=%s", resp.StatusCode, data)
	}
	resp, data = env.writeReq(t, http.MethodDelete, "/admin/api/accounts/nope", "", key)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("DELETE 不存在状态码 = %d, 期望 404, body=%s", resp.StatusCode, data)
	}

	// DELETE → 200 {"deleted":...} 且文件里消失。
	resp, data = env.writeReq(t, http.MethodDelete, "/admin/api/accounts/user-12345", "", key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("DELETE 状态码 = %d, 期望 200, body=%s", resp.StatusCode, data)
	}
	var deleted map[string]any
	_ = json.Unmarshal(data, &deleted)
	if deleted["deleted"] != "user-12345" {
		t.Fatalf("DELETE 响应 = %s", data)
	}
	accounts = readAccountsFile(t, env.s.cfg.Accounts.AccountsFile())
	if findAccount(accounts, "user-12345") != nil {
		t.Fatalf("DELETE 后账号应消失: %+v", accounts)
	}
}

// TestAdminCreateInvalidJWT 覆盖「非法 JWT 仍保存成功且 expires_at=0」。
func TestAdminCreateInvalidJWT(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	const key = "k-invalid-jwt"
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "seed", "access_token": "tok", "enabled": true, "priority": 1,
	}}, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	resp, data := env.writeReq(t, http.MethodPost, "/admin/api/accounts", `{"name":"坏token","access_token":"not-a-jwt"}`, key)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("非法 JWT 新增状态码 = %d, 期望 201, body=%s", resp.StatusCode, data)
	}
	var view map[string]any
	_ = json.Unmarshal(data, &view)
	id, _ := view["id"].(string)
	if !strings.HasPrefix(id, "acct-") {
		t.Fatalf("取不到 userId 时 id 应以 acct- 开头: %v", view["id"])
	}
	accounts := readAccountsFile(t, env.s.cfg.Accounts.AccountsFile())
	created := findAccount(accounts, id)
	if created == nil {
		t.Fatalf("账号文件应出现新账号: %+v", accounts)
	}
	var expires int64
	_ = json.Unmarshal(created["expires_at"], &expires)
	if expires != 0 {
		t.Fatalf("非法 JWT 的 expires_at = %d, 期望 0", expires)
	}
}

// TestAdminTestAccount 覆盖 /test 的 200 与 401 两条路径(HTTP 都应是 200)。
func TestAdminTestAccount(t *testing.T) {
	var catalogStatus int32 = 200
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/model_catalog" {
			http.NotFound(w, r)
			return
		}
		if atomic.LoadInt32(&catalogStatus) == 401 {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 200003, "message": "authorization_verify_error"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"categories": []map[string]any{{"type": "x", "models": []map[string]any{{"name": "m1"}, {"name": "m2"}}}},
		})
	}
	const key = "k-test"
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "a1", "access_token": "tok", "enabled": true, "priority": 1,
	}}, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	resp, data := env.writeReq(t, http.MethodPost, "/admin/api/accounts/a1/test", "", key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test 状态码 = %d, 期望 200, body=%s", resp.StatusCode, data)
	}
	var ok map[string]any
	_ = json.Unmarshal(data, &ok)
	if ok["ok"] != true || fmt.Sprint(ok["status"]) != "200" || fmt.Sprint(ok["models"]) != "2" {
		t.Fatalf("test 成功响应 = %s", data)
	}

	atomic.StoreInt32(&catalogStatus, 401)
	resp, data = env.writeReq(t, http.MethodPost, "/admin/api/accounts/a1/test", "", key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("test 上游失败时 HTTP 仍应 200, 实际 %d, body=%s", resp.StatusCode, data)
	}
	var failed map[string]any
	_ = json.Unmarshal(data, &failed)
	if failed["ok"] != false || fmt.Sprint(failed["status"]) != "401" {
		t.Fatalf("test 失败响应 = %s", data)
	}
	if msg, _ := failed["error"].(string); msg == "" {
		t.Fatalf("test 失败应带 error: %s", data)
	}
}

// TestAdminPatchAccessToken 覆盖「PATCH access_token 时同步重算 user_id/expires_at」。
func TestAdminPatchAccessToken(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	const key = "k-patch-token"
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "a1", "access_token": "old-token", "enabled": true, "priority": 1,
	}}, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	newToken := testJWTUser(time.Now().Add(2*time.Hour).Unix(), "888")
	resp, data := env.writeReq(t, http.MethodPatch, "/admin/api/accounts/a1", fmt.Sprintf(`{"access_token":%q}`, newToken), key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("PATCH access_token 状态码 = %d, 期望 200, body=%s", resp.StatusCode, data)
	}
	var view map[string]any
	_ = json.Unmarshal(data, &view)
	if view["user_id"] != "888" {
		t.Fatalf("PATCH 后 user_id = %v, 期望 888", view["user_id"])
	}
	if fmt.Sprint(view["expires_at"]) == "0" {
		t.Fatalf("PATCH 后 expires_at 应重算，实际 %v", view["expires_at"])
	}
	accounts := readAccountsFile(t, env.s.cfg.Accounts.AccountsFile())
	updated := findAccount(accounts, "a1")
	if updated == nil {
		t.Fatalf("PATCH 后账号丢失: %+v", accounts)
	}
	var uid string
	var expires int64
	_ = json.Unmarshal(updated["user_id"], &uid)
	_ = json.Unmarshal(updated["expires_at"], &expires)
	if uid != "888" || expires == 0 {
		t.Fatalf("文件未同步重算: user_id=%q expires_at=%d", uid, expires)
	}
}

// TestAdminWriteQueryKeyAuth 覆盖「?key= 鉴权对写操作同样有效」。
func TestAdminWriteQueryKeyAuth(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	const key = "k-query"
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "a1", "access_token": "tok", "enabled": true, "priority": 1,
	}}, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	resp, data := env.writeReq(t, http.MethodPatch, "/admin/api/accounts/a1?key="+key, `{"enabled":false}`, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("?key= 写操作状态码 = %d, 期望 200, body=%s", resp.StatusCode, data)
	}
}

// ── 桌面端登录态导入 ──

// 首次导入：从 auth.json 取 access_token 落账号，id 取自 hex iss，不带 refresh_token。
func TestImportDesktopAccount(t *testing.T) {
	dir := t.TempDir()
	access := testJWTiss(time.Now().Add(2*time.Hour).Unix(), "74350b")
	authJSON := fmt.Sprintf(`{"access_token": %q, "refresh_token": "desktop-rt"}`, access)
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(authJSON), 0o600); err != nil {
		t.Fatalf("预写 auth.json 失败: %v", err)
	}
	t.Setenv("RACCOON_BOX_AGENT_CONFIG_DIR", dir)
	t.Setenv("BOX_AGENT_CONFIG_DIR", "")

	handler := func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	const key = "k-import"
	env := newTestEnv(t, handler, nil, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	resp, data := env.writeReq(t, http.MethodPost, "/admin/api/accounts/import-desktop", "", key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("导入状态码 = %d, body=%s", resp.StatusCode, data)
	}
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	if out["id"] != "user-7615755" || out["updated"] != false {
		t.Fatalf("导入结果 = %v", out)
	}
	accounts := readAccountsFile(t, env.s.cfg.Accounts.AccountsFile())
	if len(accounts) != 1 {
		t.Fatalf("应只落一条账号, 实际 %d", len(accounts))
	}
	var hasRefresh bool
	_ = json.Unmarshal(accounts[0]["has_refresh_token"], &hasRefresh)
	if hasRefresh {
		t.Fatal("桌面端导入不应带 refresh_token（避免顶掉客户端会话）")
	}
	var source string
	_ = json.Unmarshal(accounts[0]["source"], &source)
	if source != "desktop" {
		t.Fatalf("source = %q, 期望 desktop（活席标记）", source)
	}

	// 同 id 再导入 = 更新。
	resp2, data2 := env.writeReq(t, http.MethodPost, "/admin/api/accounts/import-desktop", "", key)
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("二次导入状态码 = %d, body=%s", resp2.StatusCode, data2)
	}
	var out2 map[string]any
	_ = json.Unmarshal(data2, &out2)
	if out2["updated"] != true {
		t.Fatalf("二次导入 updated = %v, 期望 true", out2["updated"])
	}
	accounts = readAccountsFile(t, env.s.cfg.Accounts.AccountsFile())
	if len(accounts) != 1 {
		t.Fatalf("二次导入不应新增账号, 实际 %d", len(accounts))
	}
}

// 已有同 id 手动账号（带 refresh_token）→ 导入只更新 token，refresh_token 保留。
func TestImportDesktopKeepsRefreshToken(t *testing.T) {
	dir := t.TempDir()
	// 0x73d2e3 = 7590627，与手动账号同 id（user-7590627）。
	access := testJWTiss(time.Now().Add(2*time.Hour).Unix(), "73d2e3")
	authJSON := fmt.Sprintf(`{"access_token": %q}`, access)
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(authJSON), 0o600); err != nil {
		t.Fatalf("预写 auth.json 失败: %v", err)
	}
	t.Setenv("RACCOON_BOX_AGENT_CONFIG_DIR", dir)
	t.Setenv("BOX_AGENT_CONFIG_DIR", "")

	handler := func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	const key = "k-import-keep"
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "user-7590627", "name": "手动", "access_token": testJWTUser(time.Now().Add(time.Hour).Unix(), "7590627"),
		"refresh_token": "rt-keep", "enabled": true, "priority": 1,
	}}, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	resp, data := env.writeReq(t, http.MethodPost, "/admin/api/accounts/import-desktop", "", key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("导入状态码 = %d, body=%s", resp.StatusCode, data)
	}
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	if out["updated"] != true {
		t.Fatalf("同 id 导入 updated = %v, 期望 true", out["updated"])
	}
	accounts := readAccountsFile(t, env.s.cfg.Accounts.AccountsFile())
	var refresh string
	_ = json.Unmarshal(accounts[0]["refresh_token"], &refresh)
	if refresh != "rt-keep" {
		t.Fatalf("refresh_token = %q, 期望保留 rt-keep", refresh)
	}
}

// auth.json 缺失 → 400 并带说明。
func TestImportDesktopMissing(t *testing.T) {
	t.Setenv("RACCOON_BOX_AGENT_CONFIG_DIR", t.TempDir())
	t.Setenv("BOX_AGENT_CONFIG_DIR", "")

	handler := func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	const key = "k-import-miss"
	env := newTestEnv(t, handler, nil, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	resp, data := env.writeReq(t, http.MethodPost, "/admin/api/accounts/import-desktop", "", key)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "未检测到本机桌面端登录态") {
		t.Fatalf("缺失 auth.json = %d %s", resp.StatusCode, data)
	}
}

// admin.api_key 未设置 → 403。
func TestImportDesktopRequiresKey(t *testing.T) {
	t.Setenv("RACCOON_BOX_AGENT_CONFIG_DIR", t.TempDir())
	t.Setenv("BOX_AGENT_CONFIG_DIR", "")

	handler := func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }
	env := newTestEnv(t, handler, nil, nil)
	resp, data := env.writeReq(t, http.MethodPost, "/admin/api/accounts/import-desktop", "", "")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("无 key 导入 = %d, 期望 403, body=%s", resp.StatusCode, data)
	}
}

// ── 积分构成（/admin/api/accounts/balance 的 credits 字段）──

func TestBalanceIncludesCredits(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/balance":
			writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"available_points": 2000}})
		case "/entitlement_info":
			writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": map[string]any{
				"office": map[string]any{"pro_enable": true, "pro_expired_time": 1790000000000},
			}})
		case "/api/web/points/v1/bills":
			writeJSON(w, http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"items": []map[string]any{
				{"bill_id": "b1", "points": 300, "created_at": "2026-10-09T00:00:17+08:00", "expire_at": "2027-01-15T00:00:00+08:00"},
				{"bill_id": "b2", "points": 3000, "created_at": "2026-10-09T00:00:17+08:00", "expire_at": "2026-12-01T00:00:00+08:00"},
				{"bill_id": "b3", "points": -50, "created_at": "2026-10-09T00:00:17+08:00"},
			}}})
		default:
			http.NotFound(w, r)
		}
	}
	const key = "k-balance-credits"
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "user-9900773", "name": "积", "access_token": "tok", "enabled": true, "priority": 1,
	}}, func(cfg *config.Config) { cfg.Admin.APIKey = sp(key) })

	resp, data := env.writeReq(t, http.MethodGet, "/admin/api/accounts/balance", "", key)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("balance 状态码 = %d, body=%s", resp.StatusCode, data)
	}
	var list []map[string]any
	if err := json.Unmarshal(data, &list); err != nil || len(list) != 1 {
		t.Fatalf("balance 响应 = %s", data)
	}
	credits, _ := list[0]["credits"].(map[string]any)
	if credits == nil {
		t.Fatalf("缺 credits 字段: %s", data)
	}
	if credits["granted_points"] != float64(3300) {
		t.Fatalf("granted_points = %v, 期望 3300", credits["granted_points"])
	}
	if credits["expiring_points"] != float64(3000) {
		t.Fatalf("expiring_points = %v, 期望 3000（2026-12-01 更早）", credits["expiring_points"])
	}
	if credits["used_points"] != float64(1300) {
		t.Fatalf("used_points = %v, 期望 1300", credits["used_points"])
	}
	wantAt := time.Date(2026, 12, 1, 0, 0, 0, 0, beijingLoc).UnixMilli()
	if credits["expiring_at"] != float64(wantAt) {
		t.Fatalf("expiring_at = %v, 期望 %d", credits["expiring_at"], wantAt)
	}
	subscription, _ := list[0]["subscription"].(map[string]any)
	if subscription == nil || subscription["office_pro_enable"] != true {
		t.Fatalf("subscription 缺 pro_enable: %s", data)
	}
	if subscription["office_pro_until"] != float64(1790000000000) {
		t.Fatalf("office_pro_until = %v, 期望 1790000000000", subscription["office_pro_until"])
	}
}
