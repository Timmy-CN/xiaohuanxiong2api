package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp 写一个临时配置文件并返回路径。
func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("写入临时配置失败: %v", err)
	}
	return path
}

// clearEnv 清空可能干扰测试的环境变量。
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"XHX_LISTEN", "XHX_API_KEY", "XHX_ADMIN_API_KEY", "XHX_CONFIG"} {
		t.Setenv(key, "")
	}
}

// TestJSONCCommentsAndTrailingComma 覆盖「JSONC 注释与尾逗号能解析」。
func TestJSONCCommentsAndTrailingComma(t *testing.T) {
	clearEnv(t)
	content := `{
		// 行注释
		"listen": "127.0.0.1:9999", /* 块注释 */
		"api_key": "secret",
		"models": {
			"default": "sn-glm-5-3-flash", // 尾逗号在下面
		},
	}`
	cfg, err := Load(writeTemp(t, content))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got := *cfg.Listen; got != "127.0.0.1:9999" {
		t.Fatalf("listen = %q, 期望 127.0.0.1:9999", got)
	}
	if got := *cfg.APIKey; got != "secret" {
		t.Fatalf("api_key = %q, 期望 secret", got)
	}
	if got := *cfg.Models.Default; got != "sn-glm-5-3-flash" {
		t.Fatalf("models.default = %q, 期望 sn-glm-5-3-flash", got)
	}
}

// TestStringLiteralKeepsSlashes 覆盖「字符串字面量内的 // 不被当注释」。
func TestStringLiteralKeepsSlashes(t *testing.T) {
	clearEnv(t)
	content := `{"listen": "127.0.0.1:8080", "upstream": {"llm_base": "https://example.com//path"}}`
	cfg, err := Load(writeTemp(t, content))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if got := *cfg.Upstream.LLMBase; got != "https://example.com//path" {
		t.Fatalf("llm_base = %q, 期望保留双斜杠", got)
	}
}

// TestUnknownKey 覆盖「未知键报错」。
func TestUnknownKey(t *testing.T) {
	clearEnv(t)
	content := `{"listen": "127.0.0.1:8080", "listne": "typo"}`
	if _, err := Load(writeTemp(t, content)); err == nil {
		t.Fatal("期望未知键报错，实际成功")
	}
}

// TestBadListen 覆盖「listen: bad → 报错（中文原因）」。
func TestBadListen(t *testing.T) {
	clearEnv(t)
	content := `{"listen": "bad"}`
	_, err := Load(writeTemp(t, content))
	if err == nil {
		t.Fatal("期望 listen 非法报错，实际成功")
	}
	if !strings.Contains(err.Error(), "listen") {
		t.Fatalf("错误原因应包含 listen，实际: %v", err)
	}
}

// TestDefaultsWhenMissing 覆盖「键缺失 → 默认值」。
func TestDefaultsWhenMissing(t *testing.T) {
	clearEnv(t)
	cfg, err := Load(writeTemp(t, `{}`))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if *cfg.Listen != DefaultListen {
		t.Fatalf("listen 默认值 = %q, 期望 %q", *cfg.Listen, DefaultListen)
	}
	if *cfg.Accounts.Strategy != DefaultStrategy {
		t.Fatalf("strategy 默认值 = %q", *cfg.Accounts.Strategy)
	}
	if *cfg.Accounts.Strategy != "round_robin" {
		t.Fatalf("strategy 默认值应为 round_robin，实际 %q", *cfg.Accounts.Strategy)
	}
	if *cfg.Accounts.MaxAttempts != DefaultMaxAttempts {
		t.Fatalf("max_attempts 默认值 = %d", *cfg.Accounts.MaxAttempts)
	}
	if *cfg.Accounts.RefreshOn401 != DefaultRefreshOn401 {
		t.Fatalf("refresh_on_401 默认值 = %v", *cfg.Accounts.RefreshOn401)
	}
	if *cfg.Upstream.RequestTimeoutSeconds != DefaultRequestTimeout {
		t.Fatalf("request_timeout_seconds 默认值 = %d", *cfg.Upstream.RequestTimeoutSeconds)
	}
	if *cfg.Log.AccessLog != DefaultAccessLog {
		t.Fatalf("access_log 默认值 = %v", *cfg.Log.AccessLog)
	}
	if *cfg.Models.Default != DefaultModel {
		t.Fatalf("models.default 默认值 = %q", *cfg.Models.Default)
	}
}

// TestExplicitFalseAndZero 覆盖「显式 false/0 → 该值」。
func TestExplicitFalseAndZero(t *testing.T) {
	clearEnv(t)
	content := `{
		"accounts": {"refresh_on_401": false, "max_attempts": 1},
		"log": {"access_log": false, "buffer_size": 0},
		"upstream": {"request_timeout_seconds": 0},
		"admin": {"enabled": false}
	}`
	cfg, err := Load(writeTemp(t, content))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if *cfg.Accounts.RefreshOn401 {
		t.Fatal("refresh_on_401 显式 false 应为 false")
	}
	if *cfg.Accounts.MaxAttempts != 1 {
		t.Fatalf("max_attempts 显式 1 = %d", *cfg.Accounts.MaxAttempts)
	}
	if *cfg.Log.AccessLog {
		t.Fatal("access_log 显式 false 应为 false")
	}
	if *cfg.Log.BufferSize != 0 {
		t.Fatalf("buffer_size 显式 0 = %d", *cfg.Log.BufferSize)
	}
	if *cfg.Upstream.RequestTimeoutSeconds != 0 {
		t.Fatalf("request_timeout_seconds 显式 0 = %d", *cfg.Upstream.RequestTimeoutSeconds)
	}
	if *cfg.Admin.Enabled {
		t.Fatal("admin.enabled 显式 false 应为 false")
	}
}

// TestMask 覆盖密钥脱敏。
func TestMask(t *testing.T) {
	cases := map[string]string{"": "", "ab": "***", "abcd": "abcd…", "abcdefgh": "abcd…"}
	for input, want := range cases {
		if got := Mask(input); got != want {
			t.Fatalf("Mask(%q) = %q, 期望 %q", input, got, want)
		}
	}
}

// TestBadBaseURL 覆盖 base URL 校验。
func TestBadBaseURL(t *testing.T) {
	clearEnv(t)
	content := `{"upstream": {"llm_base": "ftp://example.com"}}`
	_, err := Load(writeTemp(t, content))
	if err == nil || !strings.Contains(err.Error(), "llm_base") {
		t.Fatalf("期望 llm_base 校验失败，实际: %v", err)
	}
}

// TestExampleConfigParses 确保随仓库交付的 config.example.json 可解析。
func TestExampleConfigParses(t *testing.T) {
	clearEnv(t)
	cfg, err := Load("../../../config.example.json")
	if err != nil {
		t.Fatalf("config.example.json 解析失败: %v", err)
	}
	if *cfg.Listen != "0.0.0.0:3066" {
		t.Fatalf("example listen = %q", *cfg.Listen)
	}
	if cfg.Accounts.StrategyValue() != "round_robin" {
		t.Fatalf("example strategy = %q, 期望 round_robin", cfg.Accounts.StrategyValue())
	}
	if !cfg.Checkin.EnabledValue() || cfg.Checkin.TimeValue() != "00:01" {
		t.Fatalf("example checkin = %+v", cfg.Checkin)
	}
	if cfg.Upstream.OriginURL() != "https://xiaohuanxiong.com" {
		t.Fatalf("example origin = %q", cfg.Upstream.OriginURL())
	}
}

// TestRemovedKeysRejected 覆盖「已移除的 aliases / fallback 留在配置里会启动失败」。
func TestRemovedKeysRejected(t *testing.T) {
	clearEnv(t)
	for name, content := range map[string]string{
		"aliases":  `{"models": {"aliases": {"a": "b"}}}`,
		"fallback": `{"models": {"fallback": []}}`,
	} {
		if _, err := Load(writeTemp(t, content)); err == nil {
			t.Fatalf("%s 已移除，留在配置里应报未知键错误", name)
		}
	}
}

// TestCheckinDefaults 覆盖「checkin 缺失 → 默认值」。
func TestCheckinDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := Load(writeTemp(t, `{}`))
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if !cfg.Checkin.EnabledValue() || cfg.Checkin.TimeValue() != DefaultCheckinTime {
		t.Fatalf("checkin 默认值错误: %+v", cfg.Checkin)
	}
	if cfg.Checkin.MaxAttemptsPerDayValue() != DefaultCheckinMaxAttemptsPerDay {
		t.Fatalf("max_attempts_per_day 默认值 = %d", cfg.Checkin.MaxAttemptsPerDayValue())
	}
	if cfg.Checkin.HistoryFileValue() != DefaultCheckinHistoryFile {
		t.Fatalf("history_file 默认值 = %q", cfg.Checkin.HistoryFileValue())
	}
	if cfg.Upstream.OriginURL() != DefaultOrigin {
		t.Fatalf("origin 默认值 = %q", cfg.Upstream.OriginURL())
	}
	if cfg.Upstream.BillsTimeout().Seconds() != DefaultBillsTimeout {
		t.Fatalf("bills_timeout 默认值 = %v", cfg.Upstream.BillsTimeout())
	}
}

// TestBadCheckinTime 覆盖「checkin.time 非法报错」。
func TestBadCheckinTime(t *testing.T) {
	clearEnv(t)
	content := `{"checkin": {"time": "25:00"}}`
	_, err := Load(writeTemp(t, content))
	if err == nil || !strings.Contains(err.Error(), "checkin.time") {
		t.Fatalf("期望 checkin.time 校验失败，实际: %v", err)
	}
}

// TestBadOrigin 覆盖「upstream.origin 必须以 http(s) 开头」。
func TestBadOrigin(t *testing.T) {
	clearEnv(t)
	content := `{"upstream": {"origin": "ftp://example.com"}}`
	_, err := Load(writeTemp(t, content))
	if err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("期望 origin 校验失败，实际: %v", err)
	}
}
