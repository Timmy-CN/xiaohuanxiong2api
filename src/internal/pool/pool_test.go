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
