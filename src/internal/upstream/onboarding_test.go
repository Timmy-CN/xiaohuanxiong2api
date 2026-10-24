package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newOnboardingClient(serverURL string) *Client {
	return NewClient(Options{
		LLMBase:        serverURL,
		AuthBase:       serverURL,
		PointsBase:     serverURL,
		Origin:         serverURL,
		BalanceTimeout: 5 * time.Second,
		BillsTimeout:   5 * time.Second,
		RefreshTimeout: 5 * time.Second,
	})
}

// 首登奖励发放：POST、桌面端头、granted=true + popup.points。
func TestClaimOnboardingGrantGranted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/web/desktop/v1/login/points/grant" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("X-Client-Platform") != "desktop-windows" || r.Header.Get("X-Client-Version") != "v1.0.0" {
			t.Errorf("桌面端头缺失: %v", r.Header)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "message": "ok",
			"data": map[string]any{"granted": true, "popup": map[string]any{"points": 3000}},
		})
	}))
	defer server.Close()

	grant, err := newOnboardingClient(server.URL).ClaimOnboardingGrant(context.Background(), "tok")
	if err != nil {
		t.Fatalf("ClaimOnboardingGrant: %v", err)
	}
	if !grant.Granted || grant.Points != 3000 {
		t.Fatalf("grant = %+v, 期望 granted=true points=3000", grant)
	}
}

// granted=true 但 popup 缺失 → 按 3000 计。
func TestClaimOnboardingGrantPointsFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "data": map[string]any{"granted": true, "popup": nil},
		})
	}))
	defer server.Close()

	grant, err := newOnboardingClient(server.URL).ClaimOnboardingGrant(context.Background(), "tok")
	if err != nil {
		t.Fatalf("ClaimOnboardingGrant: %v", err)
	}
	if !grant.Granted || grant.Points != 3000 {
		t.Fatalf("grant = %+v, 期望 granted=true points=3000（缺省回落）", grant)
	}
}

// popup.points 为字符串形态。
func TestClaimOnboardingGrantStringPoints(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "data": map[string]any{"granted": true, "popup": map[string]any{"points": "3000"}},
		})
	}))
	defer server.Close()

	grant, err := newOnboardingClient(server.URL).ClaimOnboardingGrant(context.Background(), "tok")
	if err != nil {
		t.Fatalf("ClaimOnboardingGrant: %v", err)
	}
	if !grant.Granted || grant.Points != 3000 {
		t.Fatalf("grant = %+v, 期望 granted=true points=3000", grant)
	}
}

// granted=false = 早已领取，积分 0。
func TestClaimOnboardingGrantAlreadyGranted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 0, "data": map[string]any{"granted": false},
		})
	}))
	defer server.Close()

	grant, err := newOnboardingClient(server.URL).ClaimOnboardingGrant(context.Background(), "tok")
	if err != nil {
		t.Fatalf("ClaimOnboardingGrant: %v", err)
	}
	if grant.Granted || grant.Points != 0 {
		t.Fatalf("grant = %+v, 期望 granted=false points=0", grant)
	}
}

// 非 2xx → StatusError 保留状态码（404 = 接口不可用，调用方按失败处理）。
func TestClaimOnboardingGrantHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	_, err := newOnboardingClient(server.URL).ClaimOnboardingGrant(context.Background(), "tok")
	var statusErr *StatusError
	if !errors.As(err, &statusErr) || statusErr.Status != http.StatusNotFound {
		t.Fatalf("err = %v, 期望 StatusError{404}", err)
	}
}

// 业务码 != 0 → 失败。
func TestClaimOnboardingGrantBusinessError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 200003, "message": "authorization_verify_error"})
	}))
	defer server.Close()

	_, err := newOnboardingClient(server.URL).ClaimOnboardingGrant(context.Background(), "tok")
	if err == nil || err.Error() == "" {
		t.Fatalf("业务码 200003 应返回错误")
	}
}
