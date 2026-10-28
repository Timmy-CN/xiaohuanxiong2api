package pool

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"xhx2api/internal/logx"
	"xhx2api/internal/upstream"
)

// makeJWT 造一个只含 exp 的假 JWT（不验签，只解 payload）。
func makeJWT(exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
	return header + "." + payload + ".sig"
}

// newRefreshStub 起一个只处理 /refresh 的桩上游，返回计数与客户端。
func newRefreshStub(t *testing.T, accessToken string, delay time.Duration) (*upstream.Client, *int64) {
	t.Helper()
	var count int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/refresh" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt64(&count, 1)
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    0,
			"message": "ok",
			"data": map[string]string{
				"access_token":  accessToken,
				"refresh_token": "rt-new",
			},
		})
	}))
	t.Cleanup(server.Close)
	client := upstream.NewClient(upstream.Options{
		AuthBase:       server.URL,
		LLMBase:        server.URL,
		PointsBase:     server.URL,
		RefreshTimeout: 5 * time.Second,
	})
	return client, &count
}

func writeAccounts(t *testing.T, accounts []map[string]any) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "accounts.json")
	payload := map[string]any{"accounts": accounts}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("序列化账号失败: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("写入账号文件失败: %v", err)
	}
	return path
}

func testLogger() *logx.Logger { return logx.New(io.Discard, "", logx.LevelInfo) }

// TestExpiringTriggersRefresh 覆盖「临期触发刷新」。
func TestExpiringTriggersRefresh(t *testing.T) {
	newToken := makeJWT(time.Now().Add(24 * time.Hour).Unix())
	client, count := newRefreshStub(t, newToken, 0)
	file := writeAccounts(t, []map[string]any{{
		"id":            "a1",
		"name":          "账号1",
		"access_token":  makeJWT(time.Now().Add(24 * time.Hour).Unix()),
		"refresh_token": "rt-old",
		"expires_at":    time.Now().Add(100 * time.Second).UnixMilli(), // 小于 300s 边距
		"enabled":       true,
		"priority":      1,
	}})
	p := New(Config{File: file, Strategy: "priority", RefreshMarginSeconds: 300, CooldownSeconds: 600, MaxAttempts: 3, RefreshOn401: true}, client, testLogger())

	token, err := p.Token(context.Background(), "a1", false)
	if err != nil {
		t.Fatalf("Token 失败: %v", err)
	}
	if token != newToken {
		t.Fatalf("临期应刷新并返回新 token")
	}
	if got := atomic.LoadInt64(count); got != 1 {
		t.Fatalf("刷新次数 = %d, 期望 1", got)
	}
	// 回写后 refresh_token 应轮换为新值。
	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), "rt-new") {
		t.Fatalf("账号文件应回写新 refresh_token，实际: %s", data)
	}
}

// TestConcurrentRefreshSingleFlight 覆盖「10 个并发请求只发出 1 次 refresh」。
func TestConcurrentRefreshSingleFlight(t *testing.T) {
	newToken := makeJWT(time.Now().Add(24 * time.Hour).Unix())
	client, count := newRefreshStub(t, newToken, 50*time.Millisecond)
	file := writeAccounts(t, []map[string]any{{
		"id":            "a1",
		"access_token":  "old-token",
		"refresh_token": "rt-old",
		"expires_at":    time.Now().Add(24 * time.Hour).UnixMilli(),
		"enabled":       true,
	}})
	p := New(Config{File: file, Strategy: "priority", RefreshMarginSeconds: 300, CooldownSeconds: 600, MaxAttempts: 3, RefreshOn401: true}, client, testLogger())

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.Token(context.Background(), "a1", true); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发刷新失败: %v", err)
	}
	if got := atomic.LoadInt64(count); got != 1 {
		t.Fatalf("并发刷新真实请求数 = %d, 期望 1", got)
	}
}

// TestPersistPreservesUnknownFields 覆盖「回写保留未知字段」。
func TestPersistPreservesUnknownFields(t *testing.T) {
	newToken := makeJWT(time.Now().Add(24 * time.Hour).Unix())
	client, _ := newRefreshStub(t, newToken, 0)
	file := writeAccounts(t, []map[string]any{{
		"id":            "a1",
		"access_token":  "old",
		"refresh_token": "rt-old",
		"expires_at":    time.Now().Add(100 * time.Second).UnixMilli(),
		"enabled":       true,
		"custom_field":  "keep-me",
		"nested":        map[string]any{"x": 1},
	}})
	p := New(Config{File: file, Strategy: "priority", RefreshMarginSeconds: 300, CooldownSeconds: 600, MaxAttempts: 3, RefreshOn401: true}, client, testLogger())
	if _, err := p.Token(context.Background(), "a1", false); err != nil {
		t.Fatalf("Token 失败: %v", err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("读取账号文件失败: %v", err)
	}
	var parsed struct {
		Accounts []map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("解析回写文件失败: %v", err)
	}
	if len(parsed.Accounts) != 1 {
		t.Fatalf("账号数 = %d", len(parsed.Accounts))
	}
	if _, ok := parsed.Accounts[0]["custom_field"]; !ok {
		t.Fatalf("未知字段 custom_field 丢失: %s", data)
	}
	if _, ok := parsed.Accounts[0]["nested"]; !ok {
		t.Fatalf("未知字段 nested 丢失: %s", data)
	}
}

// issJWT 造一个含 exp 与十六进制 iss 的假 JWT（真实小浣熊签发形态，uid 由 iss 派生）。
func issJWT(exp int64, iss string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d,"iss":%q}`, exp, iss)))
	return header + "." + payload + ".sig"
}

// writeDesktopAuth 把 auth.json 写进独立目录并把环境变量指过去。
func writeDesktopAuth(t *testing.T, accessToken string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auth.json"), []byte(fmt.Sprintf(`{"access_token": %q, "refresh_token": "desktop-rt"}`, accessToken)), 0o600); err != nil {
		t.Fatalf("写 auth.json 失败: %v", err)
	}
	t.Setenv("RACCOON_BOX_AGENT_CONFIG_DIR", dir)
	t.Setenv("BOX_AGENT_CONFIG_DIR", "")
}

// 活席：desktop 源账号取 token 前自动接管 auth.json 里同一账号的更新 token，
// 并回写账号文件（含 source 标记）；refresh_token 为空的桌面账号不触发刷新。
func TestDesktopSeatAdoptsNewerToken(t *testing.T) {
	newToken := issJWT(time.Now().Add(48*time.Hour).Unix(), "74350b") // 0x74350b = 7615755
	writeDesktopAuth(t, newToken)

	client, _ := newRefreshStub(t, makeJWT(time.Now().Add(24*time.Hour).Unix()), 0)
	file := writeAccounts(t, []map[string]any{{
		"id": "user-7615755", "name": "桌面", "user_id": "7615755", "access_token": issJWT(time.Now().Add(1*time.Hour).Unix(), "74350b"),
		"refresh_token": "", "expires_at": time.Now().Add(time.Hour).UnixMilli(),
		"enabled": true, "priority": 1, "source": "desktop",
	}})
	p := New(Config{File: file, Strategy: "priority", RefreshMarginSeconds: 300, CooldownSeconds: 600, MaxAttempts: 3, RefreshOn401: true}, client, testLogger())

	token, err := p.Token(context.Background(), "user-7615755", false)
	if err != nil {
		t.Fatalf("Token 失败: %v", err)
	}
	if token != newToken {
		t.Fatalf("应接管桌面端新 token")
	}

	data, _ := os.ReadFile(file)
	if !strings.Contains(string(data), newToken) || !strings.Contains(string(data), `"source": "desktop"`) {
		t.Fatalf("接管结果未回写账号文件: %s", data)
	}
}

// uid 不一致（客户端换了账号）绝不接管；文件缺失也静默沿用原 token。
func TestDesktopSeatNeverAdoptsOtherAccount(t *testing.T) {
	otherAccountToken := issJWT(time.Now().Add(48*time.Hour).Unix(), "ffff01") // 另一个账号
	writeDesktopAuth(t, otherAccountToken)
	ownToken := issJWT(time.Now().Add(1*time.Hour).Unix(), "74350b")

	client, _ := newRefreshStub(t, makeJWT(time.Now().Add(24*time.Hour).Unix()), 0)
	file := writeAccounts(t, []map[string]any{{
		"id": "user-7615755", "name": "桌面", "access_token": ownToken,
		"refresh_token": "", "expires_at": time.Now().Add(time.Hour).UnixMilli(),
		"enabled": true, "priority": 1, "source": "desktop",
	}})
	p := New(Config{File: file, Strategy: "priority", RefreshMarginSeconds: 300, CooldownSeconds: 600, MaxAttempts: 3, RefreshOn401: true}, client, testLogger())

	token, err := p.Token(context.Background(), "user-7615755", false)
	if err != nil {
		t.Fatalf("Token 失败: %v", err)
	}
	if token != ownToken {
		t.Fatalf("uid 不一致时不应接管: got %q", token)
	}
}

// 普通账号（source 为空）不读桌面端文件。
func TestDesktopSyncSkipsNormalAccounts(t *testing.T) {
	newToken := issJWT(time.Now().Add(48*time.Hour).Unix(), "74350b")
	writeDesktopAuth(t, newToken)
	ownToken := issJWT(time.Now().Add(1*time.Hour).Unix(), "74350b")

	client, _ := newRefreshStub(t, makeJWT(time.Now().Add(24*time.Hour).Unix()), 0)
	file := writeAccounts(t, []map[string]any{{
		"id": "user-7615755", "name": "普通", "access_token": ownToken,
		"refresh_token": "rt", "expires_at": time.Now().Add(time.Hour).UnixMilli(),
		"enabled": true, "priority": 1,
	}})
	p := New(Config{File: file, Strategy: "priority", RefreshMarginSeconds: 300, CooldownSeconds: 600, MaxAttempts: 3, RefreshOn401: true}, client, testLogger())

	token, err := p.Token(context.Background(), "user-7615755", false)
	if err != nil {
		t.Fatalf("Token 失败: %v", err)
	}
	if token != ownToken {
		t.Fatalf("非 desktop 账号不应接管: got %q", token)
	}
}

// TestDesktopAuthPathProfiles 插件 0.1.4（#459）口径：新客户端的
// client profile（Windows %USERPROFILE%\.office-raccoon / macOS
// Application Support\office-raccoon）优先于旧版 ~/.box-agent。
func TestDesktopAuthPathProfiles(t *testing.T) {
	// 环境变量显式覆盖仍是最高优先级
	t.Setenv("RACCOON_BOX_AGENT_CONFIG_DIR", t.TempDir())
	want := filepath.Join(os.Getenv("RACCOON_BOX_AGENT_CONFIG_DIR"), "auth.json")
	if got := DesktopAuthPath(); got != want {
		t.Fatalf("env 覆盖应优先: got %s want %s", got, want)
	}
	os.Unsetenv("RACCOON_BOX_AGENT_CONFIG_DIR")
	os.Unsetenv("BOX_AGENT_CONFIG_DIR")

	// 本机布局探测（Windows: %APPDATA%\office-raccoon → ~/.office-raccoon →
	// ~/.box-agent；Linux: XDG_CONFIG_HOME/office-raccoon → ~/.box-agent）
	// 只断言「返回的路径合法且在候选集里」，具体命中哪层取决于本机。
	got := DesktopAuthPath()
	switch runtime.GOOS {
	case "windows":
		valid := []string{
			filepath.Join(os.Getenv("APPDATA"), "office-raccoon", "agent", "config", "auth.json"),
			filepath.Join(mustHome(t), ".office-raccoon", "agent", "config", "auth.json"),
			filepath.Join(mustHome(t), ".box-agent", "config", "auth.json"),
		}
		if !slices.Contains(valid, got) {
			t.Fatalf("got %s not in candidates %v", got, valid)
		}
	default:
		if !strings.HasSuffix(got, filepath.Join("office-raccoon", "agent", "config", "auth.json")) &&
			!strings.HasSuffix(got, filepath.Join(".box-agent", "config", "auth.json")) {
			t.Fatalf("got %s 不在候选布局里", got)
		}
	}
}

func mustHome(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return home
}
