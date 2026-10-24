package upstream

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// mintJWT 造一个只含给定 claims 的假 JWT（不验签，只解 payload）。
func mintJWT(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("序列化 claims 失败: %v", err)
	}
	return "x." + base64.RawURLEncoding.EncodeToString(raw) + ".y"
}

// 真实小浣熊 JWT 把用户 id 放在 iss（十六进制串），必须转成十进制。
func TestJWTUserIDFromHexIss(t *testing.T) {
	tok := mintJWT(t, map[string]any{"iss": "74350b", "name": "RaccoonMason", "exp": 1800000000})
	if got := JWTUserID(tok); got != "7615755" {
		t.Fatalf("JWTUserID = %q, 期望 7615755", got)
	}
}

func TestJWTUserIDFromHexSidWinsOverUserId(t *testing.T) {
	tok := mintJWT(t, map[string]any{"sid": "74350b", "user_id": "999"})
	if got := JWTUserID(tok); got != "7615755" {
		t.Fatalf("JWTUserID = %q, 期望 7615755（十六进制 sid 优先于直接取值）", got)
	}
}

func TestJWTUserIDHexPrefixCaseInsensitive(t *testing.T) {
	tok := mintJWT(t, map[string]any{"iss": "0X74350B"})
	if got := JWTUserID(tok); got != "7615755" {
		t.Fatalf("JWTUserID = %q, 期望 7615755", got)
	}
}

// 十六进制 id 超出 uint64 也要能转（实测签发为 7 位以内，这里取 72 位保边界）。
func TestJWTUserIDWideHex(t *testing.T) {
	tok := mintJWT(t, map[string]any{"iss": "ffffffffffffffffff"})
	if got := JWTUserID(tok); got != "4722366482869645213695" {
		t.Fatalf("JWTUserID = %q, 期望 4722366482869645213695", got)
	}
}

// 非十六进制的 iss（如 URL）不参与转换，回落到直接取值。
func TestJWTUserIDFallsBackToDirectClaims(t *testing.T) {
	tok := mintJWT(t, map[string]any{"iss": "https://auth.xiaohuanxiong.com", "user_id": "7590626", "exp": 1800000000})
	if got := JWTUserID(tok); got != "7590626" {
		t.Fatalf("JWTUserID = %q, 期望 7590626", got)
	}
	subOnly := mintJWT(t, map[string]any{"iss": "https://auth.xiaohuanxiong.com", "sub": "42"})
	if got := JWTUserID(subOnly); got != "42" {
		t.Fatalf("JWTUserID = %q, 期望 42（sub 回落）", got)
	}
	num := mintJWT(t, map[string]any{"user_id": 7615755})
	if got := JWTUserID(num); got != "7615755" {
		t.Fatalf("JWTUserID = %q, 期望 7615755（数字 user_id）", got)
	}
}

func TestJWTUserIDUnknown(t *testing.T) {
	tok := mintJWT(t, map[string]any{"exp": 1800000000})
	if got := JWTUserID(tok); got != "" {
		t.Fatalf("JWTUserID = %q, 期望空串", got)
	}
	if got := JWTUserID("not-a-jwt"); got != "" {
		t.Fatalf("JWTUserID = %q, 期望空串", got)
	}
}
