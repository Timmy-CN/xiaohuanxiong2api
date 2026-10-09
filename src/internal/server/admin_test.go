package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
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
