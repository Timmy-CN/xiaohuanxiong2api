package server

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

	"xhx2api/internal/config"
	"xhx2api/internal/logx"
	"xhx2api/internal/pool"
	"xhx2api/internal/upstream"
)

func sp(v string) *string { return &v }
func ip(v int) *int       { return &v }
func bp(v bool) *bool     { return &v }

func testJWT(exp int64) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf(`{"exp":%d}`, exp)))
	return header + "." + payload + ".sig"
}

func buildClient(cfg *config.Config) *upstream.Client {
	return upstream.NewClient(upstream.Options{
		LLMBase:               cfg.Upstream.LLMBaseURL(),
		AuthBase:              cfg.Upstream.AuthBaseURL(),
		PointsBase:            cfg.Upstream.PointsBaseURL(),
		Origin:                cfg.Upstream.OriginURL(),
		ConnectTimeout:        cfg.Upstream.ConnectTimeout(),
		ResponseHeaderTimeout: cfg.Upstream.ResponseHeaderTimeout(),
		RequestTimeout:        cfg.Upstream.RequestTimeout(),
		RefreshTimeout:        cfg.Upstream.RefreshTimeout(),
		CatalogTimeout:        cfg.Upstream.CatalogTimeout(),
		BalanceTimeout:        cfg.Upstream.BalanceTimeout(),
		BillsTimeout:          cfg.Upstream.BillsTimeout(),
		UserAgent:             cfg.Upstream.UserAgentValue(),
	})
}

// testEnv 是一套「本机反代 + 桩上游」的测试环境。
type testEnv struct {
	s  *Server
	hs *httptest.Server
	up *httptest.Server
}

// newTestEnv 起一个桩上游与一个真实 HTTP 服务（全部走本地，不打真实上游）。
func newTestEnv(t *testing.T, handler http.HandlerFunc, accounts []map[string]any, mutate func(*config.Config)) *testEnv {
	t.Helper()
	upstreamServer := httptest.NewServer(handler)
	t.Cleanup(upstreamServer.Close)

	cfg := config.Default()
	cfg.Upstream.LLMBase = sp(upstreamServer.URL)
	cfg.Upstream.AuthBase = sp(upstreamServer.URL)
	cfg.Upstream.PointsBase = sp(upstreamServer.URL)
	cfg.Models.CatalogCacheFile = sp(filepath.Join(t.TempDir(), "catalog.json"))
	cfg.Upstream.RequestTimeoutSeconds = ip(0)
	cfg.Log.AccessLog = bp(false)
	if mutate != nil {
		mutate(cfg)
	}

	accountsFile := filepath.Join(t.TempDir(), "accounts.json")
	payload, _ := json.Marshal(map[string]any{"accounts": accounts})
	if err := os.WriteFile(accountsFile, payload, 0o600); err != nil {
		t.Fatalf("写入账号文件失败: %v", err)
	}
	cfg.Accounts.File = sp(accountsFile)

	logger := logx.New(io.Discard, "", logx.LevelInfo)
	client := buildClient(cfg)
	accountPool := pool.New(pool.Config{
		File:                 cfg.Accounts.AccountsFile(),
		Strategy:             cfg.Accounts.StrategyValue(),
		RefreshMarginSeconds: int(cfg.Accounts.RefreshMargin().Seconds()),
		CooldownSeconds:      int(cfg.Accounts.Cooldown().Seconds()),
		MaxAttempts:          cfg.Accounts.MaxAttemptsValue(),
		RefreshOn401:         cfg.Accounts.RefreshOn401Value(),
	}, client, logger)
	catalog := NewCatalog(client, cfg.Models.CatalogCacheTTL(), cfg.Models.CatalogCacheFileValue(), cfg.Models.Fallback)
	app := New(cfg, client, accountPool, catalog, logger, "test")
	httpServer := httptest.NewServer(app.Handler())
	t.Cleanup(httpServer.Close)
	return &testEnv{s: app, hs: httpServer, up: upstreamServer}
}

func (e *testEnv) postChat(t *testing.T, body string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Post(e.hs.URL+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求 chat 失败: %v", err)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, data
}

// ── 用例 1：别名映射与缺省模型回填 ──

func TestAliasAndDefaultModel(t *testing.T) {
	var mu sync.Mutex
	var received []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body map[string]json.RawMessage
		_ = json.NewDecoder(r.Body).Decode(&body)
		var model string
		_ = json.Unmarshal(body["model"], &model)
		mu.Lock()
		received = append(received, model)
		mu.Unlock()
		writeJSON(w, 200, map[string]any{
			"id":      "chatcmpl-1",
			"object":  "chat.completion",
			"model":   "raccoon-internal",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "hi"}}},
		})
	}
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "a1", "access_token": "tok", "enabled": true, "priority": 1, "expires_at": 0,
	}}, func(cfg *config.Config) {
		cfg.Models.Aliases = map[string]string{"glm-5.3-flash": "sn-glm-5-3-flash"}
	})

	resp, data := env.postChat(t, `{"model":"glm-5.3-flash","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, data)
	}
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	if out["model"] != "glm-5.3-flash" {
		t.Fatalf("响应 model = %v, 期望别名 glm-5.3-flash", out["model"])
	}
	mu.Lock()
	if len(received) != 1 || received[0] != "sn-glm-5-3-flash" {
		t.Fatalf("上游收到的 model = %v, 期望 sn-glm-5-3-flash", received)
	}
	mu.Unlock()

	// 缺省模型回填。
	resp, data = env.postChat(t, `{"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, data)
	}
	_ = json.Unmarshal(data, &out)
	if out["model"] != "sn-glm-5-3-flash" {
		t.Fatalf("缺省模型响应 model = %v, 期望 sn-glm-5-3-flash", out["model"])
	}
	mu.Lock()
	if received[1] != "sn-glm-5-3-flash" {
		t.Fatalf("缺省模型上游收到 = %v", received)
	}
	mu.Unlock()
}

// ── 用例 2：validate=catalog 未知模型 400；目录为空放行 ──

func TestValidateCatalog(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"id": "c", "object": "chat.completion", "model": "internal",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "a1", "access_token": "tok", "enabled": true, "priority": 1, "expires_at": 0,
	}}, nil)

	resp, data := env.postChat(t, `{"model":"no-such-model","messages":[]}`)
	if resp.StatusCode != 400 {
		t.Fatalf("未知模型状态码 = %d, body=%s", resp.StatusCode, data)
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(data, &envelope)
	if envelope.Error.Code != "model_not_found" {
		t.Fatalf("错误 code = %q, 期望 model_not_found", envelope.Error.Code)
	}

	// 目录为空 → 放行。
	env.s.catalog.mu.Lock()
	env.s.catalog.models = nil
	env.s.catalog.mu.Unlock()
	resp, data = env.postChat(t, `{"model":"no-such-model","messages":[]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("目录为空时应放行，状态码 = %d, body=%s", resp.StatusCode, data)
	}
}

// ── 用例 3：401 → 强制刷新 → 同账号重试成功 ──

func Test401ForcesRefreshAndRetries(t *testing.T) {
	var chatCount, refreshCount int64
	var mu sync.Mutex
	var tokens []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/refresh":
			atomic.AddInt64(&refreshCount, 1)
			writeJSON(w, 200, map[string]any{
				"code": 0, "message": "ok",
				"data": map[string]string{
					"access_token":  testJWT(time.Now().Add(24 * time.Hour).Unix()),
					"refresh_token": "rt-new",
				},
			})
		case "/chat/completions":
			atomic.AddInt64(&chatCount, 1)
			mu.Lock()
			tokens = append(tokens, r.Header.Get("Authorization"))
			mu.Unlock()
			if atomic.LoadInt64(&chatCount) == 1 {
				w.WriteHeader(401)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 200003, "message": "authorization_verify_error", "details": "authorization verify failed"})
				return
			}
			writeJSON(w, 200, map[string]any{
				"id": "c", "object": "chat.completion", "model": "internal",
				"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}}},
			})
		default:
			http.NotFound(w, r)
		}
	}
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "a1", "access_token": "old-token", "refresh_token": "rt-old", "enabled": true, "priority": 1,
		"expires_at": time.Now().Add(24 * time.Hour).UnixMilli(), // 不临期，靠 401 触发强制刷新
	}}, nil)

	resp, data := env.postChat(t, `{"model":"sn-glm-5-3-flash","messages":[]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, data)
	}
	if atomic.LoadInt64(&chatCount) != 2 {
		t.Fatalf("chat 调用次数 = %d, 期望 2（先 401 再 200）", chatCount)
	}
	if atomic.LoadInt64(&refreshCount) != 1 {
		t.Fatalf("refresh 次数 = %d, 期望 1", refreshCount)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 || tokens[0] != "Bearer old-token" {
		t.Fatalf("首次 token = %v", tokens)
	}
	if tokens[1] == "Bearer old-token" || !strings.HasPrefix(tokens[1], "Bearer ") {
		t.Fatalf("重试应使用新 token，实际 = %v", tokens)
	}
}

// ── 用例 4：429 冷却换号 ──

func Test429CooldownSwitchesAccount(t *testing.T) {
	var mu sync.Mutex
	var tokens []string
	handler := func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		auth := r.Header.Get("Authorization")
		mu.Lock()
		tokens = append(tokens, auth)
		mu.Unlock()
		if auth == "Bearer token-a" {
			w.WriteHeader(429)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 429001, "message": "too many requests"})
			return
		}
		writeJSON(w, 200, map[string]any{
			"id": "c", "object": "chat.completion", "model": "internal",
			"choices": []map[string]any{{"index": 0, "message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}
	env := newTestEnv(t, handler, []map[string]any{
		{"id": "a1", "access_token": "token-a", "enabled": true, "priority": 1, "expires_at": 0},
		{"id": "a2", "access_token": "token-b", "enabled": true, "priority": 2, "expires_at": 0},
	}, nil)

	resp, data := env.postChat(t, `{"model":"sn-glm-5-3-flash","messages":[]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, data)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(tokens) != 2 || tokens[0] != "Bearer token-a" || tokens[1] != "Bearer token-b" {
		t.Fatalf("两次请求应打到不同账号 token，实际 = %v", tokens)
	}
	// 账号 a1 应处于冷却中。
	views := env.s.pool.Views()
	for _, view := range views {
		if view.ID == "a1" && view.CooldownUntil <= time.Now().UnixMilli() {
			t.Fatalf("账号 a1 应处于冷却中，cooldown_until=%d", view.CooldownUntil)
		}
	}
}

// ── 用例 5：流式改写 ──

func TestStreamingRewrite(t *testing.T) {
	frames := "" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"raccoon-internal\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"思考\"}}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"raccoon-internal\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你\"}}]}\n\n" +
		"data: {\"id\":\"c\",\"object\":\"chat.completion.chunk\",\"model\":\"raccoon-internal\",\"choices\":[{\"index\":0,\"delta\":{}}],\"usage\":{\"completion_tokens\":5,\"prompt_tokens\":2,\"total_tokens\":7,\"completion_tokens_details\":{\"reasoning_tokens\":3}}}\n\n" +
		"data: [DONE]\n\n"
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frames)
	}
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "a1", "access_token": "tok", "enabled": true, "priority": 1, "expires_at": 0,
	}}, func(cfg *config.Config) {
		cfg.Models.Aliases = map[string]string{"glm-5.3-flash": "sn-glm-5-3-flash"}
	})

	resp, data := env.postChat(t, `{"model":"glm-5.3-flash","messages":[],"stream":true}`)
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, data)
	}
	text := string(data)
	if strings.Contains(text, "raccoon-internal") {
		t.Fatalf("model 未被改写: %s", text)
	}
	if !strings.Contains(text, `"model":"glm-5.3-flash"`) {
		t.Fatalf("每帧 model 应改写为客户端请求名: %s", text)
	}
	if !strings.Contains(text, `"reasoning_content":"思考"`) {
		t.Fatalf("reasoning_content 应原样保留: %s", text)
	}
	if !strings.Contains(text, "data: [DONE]") {
		t.Fatalf("[DONE] 应透传: %s", text)
	}
	if !strings.Contains(text, `"completion_tokens":5`) {
		t.Fatalf("usage 帧应保留: %s", text)
	}
	// 日志应记录 usage。
	logs := env.s.logs.Last(1)
	if len(logs) != 1 {
		t.Fatalf("请求日志条数 = %d", len(logs))
	}
	if logs[0].PromptTokens != 2 || logs[0].CompletionTokens != 5 || logs[0].ReasoningTokens != 3 {
		t.Fatalf("日志 token 记录错误: %+v", logs[0])
	}
	if !logs[0].Stream {
		t.Fatal("日志 stream 应为 true")
	}
}

// ── 用例 6：非流式改写与字段保留 ──

func TestNonStreamingRewriteKeepsFields(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{
			"id":    "c",
			"model": "raccoon-internal",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":              "assistant",
					"content":           nil,
					"reasoning_content": "想",
					"tool_calls": []map[string]any{{
						"id": "call_1", "type": "function",
						"function": map[string]any{"name": "f", "arguments": "{}"},
					}},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
		})
	}
	env := newTestEnv(t, handler, []map[string]any{{
		"id": "a1", "access_token": "tok", "enabled": true, "priority": 1, "expires_at": 0,
	}}, nil)

	resp, data := env.postChat(t, `{"model":"sn-glm-5-3-flash","messages":[]}`)
	if resp.StatusCode != 200 {
		t.Fatalf("状态码 = %d, body=%s", resp.StatusCode, data)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("解析响应失败: %v", err)
	}
	var model string
	_ = json.Unmarshal(out["model"], &model)
	if model != "sn-glm-5-3-flash" {
		t.Fatalf("model = %q, 期望 sn-glm-5-3-flash", model)
	}
	if _, ok := out["usage"]; !ok {
		t.Fatal("usage 字段丢失")
	}
	if !strings.Contains(string(out["choices"]), "tool_calls") {
		t.Fatalf("tool_calls 字段丢失: %s", out["choices"])
	}
	if !strings.Contains(string(out["choices"]), "reasoning_content") {
		t.Fatalf("reasoning_content 字段丢失: %s", out["choices"])
	}
}

// ── 用例 7：目录远程失败 → 缓存 → 兜底 ──

func TestCatalogRemoteFailureUsesCache(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer upstreamServer.Close()
	client := upstream.NewClient(upstream.Options{LLMBase: upstreamServer.URL, CatalogTimeout: time.Second})

	cacheFile := filepath.Join(t.TempDir(), "catalog.json")
	cached := catalogCacheFile{
		Models:      []upstream.Model{{ID: "cached-model", DisplayName: "缓存模型", Visible: true}},
		RefreshedAt: time.Now().UnixMilli(),
	}
	data, _ := json.Marshal(cached)
	if err := os.WriteFile(cacheFile, data, 0o600); err != nil {
		t.Fatalf("写缓存失败: %v", err)
	}
	catalog := NewCatalog(client, time.Minute, cacheFile, nil)
	if !catalog.LoadCache() {
		t.Fatal("应从缓存加载")
	}
	if err := catalog.Refresh(context.Background(), ""); err == nil {
		t.Fatal("远程失败时 Refresh 应返回错误")
	}
	models := catalog.Models("")
	if len(models) == 0 || models[0].ID != "cached-model" {
		t.Fatalf("应使用持久化缓存，实际: %+v", models)
	}
	if catalog.Source() != "cache" {
		t.Fatalf("来源 = %q, 期望 cache", catalog.Source())
	}
}

func TestCatalogNoCacheUsesFallback(t *testing.T) {
	upstreamServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}))
	defer upstreamServer.Close()
	client := upstream.NewClient(upstream.Options{LLMBase: upstreamServer.URL, CatalogTimeout: time.Second})
	catalog := NewCatalog(client, time.Minute, filepath.Join(t.TempDir(), "missing.json"), nil)
	if catalog.LoadCache() {
		t.Fatal("不应从缺失缓存加载")
	}
	if err := catalog.Refresh(context.Background(), ""); err == nil {
		t.Fatal("远程失败时 Refresh 应返回错误")
	}
	if len(catalog.Models("")) == 0 {
		t.Fatal("无缓存时应使用内置兜底，且非空")
	}
	if catalog.Source() != "fallback" {
		t.Fatalf("来源 = %q, 期望 fallback", catalog.Source())
	}
}

// TestModelsEndpointNeverEmpty 覆盖「上游不可达时 /v1/models 也不能返回空」。
func TestModelsEndpointNeverEmpty(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", 500)
	}
	env := newTestEnv(t, handler, nil, nil)
	// 让目录退化为纯兜底。
	env.s.catalog.mu.Lock()
	env.s.catalog.models = upstream.BuiltinModels()
	env.s.catalog.source = "fallback"
	env.s.catalog.mu.Unlock()

	resp, err := http.Get(env.hs.URL + "/v1/models")
	if err != nil {
		t.Fatalf("请求 /v1/models 失败: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(payload.Data) == 0 {
		t.Fatalf("/v1/models 不应为空: %s", data)
	}
}
