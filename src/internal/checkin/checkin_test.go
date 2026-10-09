package checkin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"xhx2api/internal/logx"
	"xhx2api/internal/pool"
	"xhx2api/internal/upstream"
)

// stub 是一个桩上游，记录各类调用次数（绝不打真实上游）。
type stub struct {
	day             string
	settingCode     int
	failSetting     bool
	setting401First bool
	billsItems      []upstream.Bill

	settingCount int64
	billsCount   int64
	refreshCount int64
}

func (s *stub) currentDay() string {
	if s.day != "" {
		return s.day
	}
	return "2026-10-09"
}

func (s *stub) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/web/office/v3/setting_info":
			n := atomic.AddInt64(&s.settingCount, 1)
			if s.setting401First && n == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 200003, "message": "authorization_verify_error"})
				return
			}
			if s.failSetting {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 500, "message": "boom"})
				return
			}
			stubJSON(w, http.StatusOK, map[string]any{
				"code":    s.settingCode,
				"message": "ok",
				"data": map[string]any{
					"point_grant_popups": []any{},
					"point_grant_toast":  nil,
				},
			})
		case "/api/web/points/v1/bills":
			atomic.AddInt64(&s.billsCount, 1)
			items := s.billsItems
			if items == nil {
				items = []upstream.Bill{}
			}
			stubJSON(w, http.StatusOK, map[string]any{"code": 0, "data": map[string]any{"items": items}})
		case "/refresh":
			atomic.AddInt64(&s.refreshCount, 1)
			stubJSON(w, http.StatusOK, map[string]any{
				"code": 0, "message": "ok",
				"data": map[string]string{"access_token": "new-token", "refresh_token": "rt-new"},
			})
		default:
			http.NotFound(w, r)
		}
	}
}

func stubJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

// newService 起桩上游 + 真实账号池，返回可注入时钟的签到服务。
func newService(t *testing.T, s *stub, mutate func(*Config)) (*Service, func(time.Time)) {
	t.Helper()
	server := httptest.NewServer(s.handler())
	t.Cleanup(server.Close)

	client := upstream.NewClient(upstream.Options{
		LLMBase:        server.URL,
		AuthBase:       server.URL,
		PointsBase:     server.URL,
		Origin:         server.URL,
		RefreshTimeout: 5 * time.Second,
		BalanceTimeout: 5 * time.Second,
		BillsTimeout:   5 * time.Second,
	})

	accountsFile := filepath.Join(t.TempDir(), "accounts.json")
	payload, _ := json.Marshal(map[string]any{"accounts": []map[string]any{{
		"id": "user-7591340", "name": "账号1", "access_token": "tok",
		"refresh_token": "rt", "enabled": true, "priority": 1, "expires_at": 0,
	}}})
	if err := os.WriteFile(accountsFile, payload, 0o600); err != nil {
		t.Fatalf("写入账号文件失败: %v", err)
	}

	logger := logx.New(io.Discard, "", logx.LevelError)
	accountPool := pool.New(pool.Config{
		File: accountsFile, Strategy: "priority", RefreshMarginSeconds: 300,
		CooldownSeconds: 600, MaxAttempts: 3, RefreshOn401: true,
	}, client, logger)

	cfg := Config{
		Enabled: true, Time: "00:01", OnStart: false, VerifyBills: true,
		BillsLimit: 20, RetryInterval: 30 * time.Minute, MaxAttemptsPerDay: 5,
		HistoryFile: filepath.Join(t.TempDir(), "checkin.json"), HistoryDays: 30,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	svc := New(cfg, client, accountPool, logger)
	svc.spacing = 0

	clock := time.Date(2026, 10, 9, 0, 30, 0, 0, time.Local)
	svc.SetClock(func() time.Time { return clock })
	return svc, func(tm time.Time) { clock = tm }
}

func bill(day string, points int64) upstream.Bill {
	return upstream.Bill{
		BillID: "b1", ClientType: "web", EventName: "每日积分发放", BizType: "daily_grant",
		Points: points, CreatedAt: day + "T00:00:17+08:00",
	}
}

// 用例 1：setting_info code==0 + bills 有今日入账 300 → success=true、points=300；
// 并断言上游调用次数（setting_info 1 次、bills 1 次）。
func TestCheckinSuccessWithBills(t *testing.T) {
	s := &stub{billsItems: []upstream.Bill{bill("2026-10-09", 300)}}
	svc, _ := newService(t, s, nil)

	svc.CheckAll(context.Background())

	views := svc.Views()
	if len(views) != 1 {
		t.Fatalf("视图条数 = %d", len(views))
	}
	if !views[0].Today.Success || views[0].Today.Points != 300 {
		t.Fatalf("今日结果 = %+v, 期望 success=true points=300", views[0].Today)
	}
	if views[0].Today.Msg != "今日积分 +300" {
		t.Fatalf("今日 msg = %q", views[0].Today.Msg)
	}
	if got := atomic.LoadInt64(&s.settingCount); got != 1 {
		t.Fatalf("setting_info 调用次数 = %d, 期望 1", got)
	}
	if got := atomic.LoadInt64(&s.billsCount); got != 1 {
		t.Fatalf("bills 调用次数 = %d, 期望 1", got)
	}
}

// 用例 2：同一天再跑一次 → 不再调用上游，结果带 skipped_reason。
func TestCheckinSameDaySkips(t *testing.T) {
	s := &stub{billsItems: []upstream.Bill{bill("2026-10-09", 300)}}
	svc, _ := newService(t, s, nil)

	svc.CheckAll(context.Background())
	svc.CheckAll(context.Background())

	if got := atomic.LoadInt64(&s.settingCount); got != 1 {
		t.Fatalf("第二次不应再调 setting_info，实际 = %d", got)
	}
	if got := atomic.LoadInt64(&s.billsCount); got != 1 {
		t.Fatalf("第二次不应再调 bills，实际 = %d", got)
	}
	if reason := svc.Views()[0].SkippedReason; reason != "今天已成功" {
		t.Fatalf("skipped_reason = %q, 期望 今天已成功", reason)
	}
}

// 用例 3：bills 无今日入账 → success=false，msg 说明「今日未见积分入账」。
func TestCheckinNoBills(t *testing.T) {
	s := &stub{billsItems: []upstream.Bill{}}
	svc, _ := newService(t, s, nil)

	svc.CheckAll(context.Background())

	view := svc.Views()[0]
	if view.Today.Success {
		t.Fatalf("无今日入账不应成功: %+v", view.Today)
	}
	if !strings.Contains(view.Today.Msg, "今日未见积分入账") {
		t.Fatalf("msg = %q, 期望含「今日未见积分入账」", view.Today.Msg)
	}
	if view.Today.Points != 0 {
		t.Fatalf("points = %d, 期望 0", view.Today.Points)
	}
}

// 用例 4：setting_info 返回 401 → 触发刷新 → 重试成功。
func TestCheckin401ForcesRefresh(t *testing.T) {
	s := &stub{setting401First: true, billsItems: []upstream.Bill{bill("2026-10-09", 300)}}
	svc, _ := newService(t, s, nil)

	svc.CheckAll(context.Background())

	if got := atomic.LoadInt64(&s.refreshCount); got != 1 {
		t.Fatalf("refresh 调用次数 = %d, 期望 1", got)
	}
	if got := atomic.LoadInt64(&s.settingCount); got != 2 {
		t.Fatalf("setting_info 调用次数 = %d, 期望 2（先 401 再重试）", got)
	}
	if !svc.Views()[0].Today.Success {
		t.Fatalf("401 后重试应成功: %+v", svc.Views()[0].Today)
	}
}

// 用例 5：跨天 → 重新签到（上游调用计数 +1）。
func TestCheckinCrossDay(t *testing.T) {
	s := &stub{billsItems: []upstream.Bill{bill("2026-10-09", 300)}}
	svc, setClock := newService(t, s, nil)

	svc.CheckAll(context.Background())
	if got := atomic.LoadInt64(&s.settingCount); got != 1 {
		t.Fatalf("首日 setting_info 次数 = %d", got)
	}

	// 推到次日，并让桩上游返回次日的入账。
	s.day = "2026-10-10"
	s.billsItems = []upstream.Bill{bill("2026-10-10", 300)}
	setClock(time.Date(2026, 10, 10, 0, 30, 0, 0, time.Local))
	svc.CheckAll(context.Background())

	if got := atomic.LoadInt64(&s.settingCount); got != 2 {
		t.Fatalf("跨天后 setting_info 次数 = %d, 期望 2", got)
	}
	if !svc.Views()[0].Today.Success || svc.Views()[0].Today.Points != 300 {
		t.Fatalf("次日的今日结果 = %+v", svc.Views()[0].Today)
	}
}

// 用例 6：setting_info 5xx → attempts_today 递增；达到 max_attempts_per_day 后当天不再试。
func TestCheckinMaxAttempts(t *testing.T) {
	s := &stub{failSetting: true}
	svc, _ := newService(t, s, func(cfg *Config) { cfg.MaxAttemptsPerDay = 2 })

	for i := 0; i < 3; i++ {
		svc.CheckAll(context.Background())
	}

	if got := atomic.LoadInt64(&s.settingCount); got != 2 {
		t.Fatalf("setting_info 次数 = %d, 期望 2（达到上限后不再试）", got)
	}
	view := svc.Views()[0]
	if view.AttemptsToday != 2 {
		t.Fatalf("attempts_today = %d, 期望 2", view.AttemptsToday)
	}
	if view.SkippedReason != "今日尝试次数已达上限" {
		t.Fatalf("skipped_reason = %q", view.SkippedReason)
	}
}

// TestStatePreservesUnknownFields 覆盖「保留未知字段」与原子落盘权限 0600。
func TestStatePreservesUnknownFields(t *testing.T) {
	historyFile := filepath.Join(t.TempDir(), "checkin.json")
	content := `{
		"top_extra": {"keep": true},
		"accounts": {
			"user-7591340": {
				"last_date": "2026-10-08",
				"last_success_at": 1,
				"last_msg": "旧",
				"attempts_today": 1,
				"last_error": "",
				"account_extra": "keep-me",
				"history": [
					{"date": "2026-10-08", "success": true, "points": 300, "msg": "旧", "at": 1, "entry_extra": 42}
				]
			}
		}
	}`
	if err := os.WriteFile(historyFile, []byte(content), 0o600); err != nil {
		t.Fatalf("预写历史失败: %v", err)
	}

	s := &stub{billsItems: []upstream.Bill{bill("2026-10-09", 300)}}
	svc, _ := newService(t, s, func(cfg *Config) { cfg.HistoryFile = historyFile })
	svc.CheckAll(context.Background())

	data, err := os.ReadFile(historyFile)
	if err != nil {
		t.Fatalf("读取历史失败: %v", err)
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("解析历史失败: %v", err)
	}
	if _, ok := parsed["top_extra"]; !ok {
		t.Fatalf("顶层未知字段丢失: %s", data)
	}
	if !strings.Contains(string(data), "account_extra") {
		t.Fatalf("账号未知字段丢失: %s", data)
	}
	if !strings.Contains(string(data), "entry_extra") {
		t.Fatalf("历史条目未知字段丢失: %s", data)
	}
	info, err := os.Stat(historyFile)
	if err != nil {
		t.Fatalf("Stat 失败: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("文件权限 = %o, 期望 600", perm)
	}
}

// TestParseClock 覆盖 HH:MM 解析。
func TestParseClock(t *testing.T) {
	if h, m, err := parseClock("00:01"); err != nil || h != 0 || m != 1 {
		t.Fatalf("parseClock(00:01) = %d:%d, %v", h, m, err)
	}
	for _, bad := range []string{"", "24:00", "12:60", "abc", "1"} {
		if _, _, err := parseClock(bad); err == nil {
			t.Fatalf("parseClock(%q) 应报错", bad)
		}
	}
}
