package upstream

import (
	"encoding/base64"
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
)

// JWTExpiry 解码 JWT payload 的 exp（秒），返回毫秒；解不出返回 0。
//
// 不验签，只读 exp（SPEC §1.5）。返回 0 表示过期时间未知。
func JWTExpiry(token string) int64 {
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return 0
		}
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	dec := json.NewDecoder(strings.NewReader(string(payload)))
	dec.UseNumber()
	if err := dec.Decode(&claims); err != nil {
		return 0
	}
	seconds, err := strconv.ParseFloat(claims.Exp.String(), 64)
	if err != nil || seconds <= 0 {
		return 0
	}
	return int64(seconds * 1000)
}

// JWTUserID 解码 JWT payload 里的用户 id（不验签）。
//
// 商汤小浣熊签发的 JWT 把用户 id 放在 sid / iss（十六进制串，如 "74350b" 即
// 十进制 7615755），先按十六进制转十进制；非十六进制串（如 iss 是 URL）或转换
// 失败则回落为直接取值：依次尝试 user_id / userId / uid / userID / sub / id，
// 解析不出返回空串（SPEC4 §G.2）。
func JWTUserID(token string) string {
	token = strings.TrimSpace(strings.TrimPrefix(token, "Bearer "))
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		payload, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	for _, key := range []string{"sid", "iss"} {
		var text string
		if raw, ok := claims[key]; ok && json.Unmarshal(raw, &text) == nil {
			if dec := hexIDToDecimal(text); dec != "" {
				return dec
			}
		}
	}
	for _, key := range []string{"user_id", "userId", "uid", "userID", "sub", "id"} {
		raw, ok := claims[key]
		if !ok {
			continue
		}
		var text string
		if err := json.Unmarshal(raw, &text); err == nil && text != "" {
			return text
		}
		var num json.Number
		if err := json.Unmarshal(raw, &num); err == nil {
			if s := num.String(); s != "" && s != "0" {
				return s
			}
		}
	}
	return ""
}

// hexIDToDecimal 把十六进制 id 串转成十进制字符串；空串、含非十六进制字符或
// 解析失败返回空串。接受可选的 0x / 0X 前缀。
func hexIDToDecimal(value string) string {
	raw := strings.TrimSpace(value)
	if len(raw) >= 2 && raw[0] == '0' && (raw[1] == 'x' || raw[1] == 'X') {
		raw = raw[2:]
	}
	if raw == "" {
		return ""
	}
	for _, r := range raw {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return ""
		}
	}
	n, ok := new(big.Int).SetString(raw, 16)
	if !ok {
		return ""
	}
	return n.String()
}

// TokenTail 返回 token 的最后 4 个字符（不足则整体返回）。
func TokenTail(token string) string {
	runes := []rune(token)
	if len(runes) <= 4 {
		return string(runes)
	}
	return string(runes[len(runes)-4:])
}

// extractMessage 从上游错误体里取 message / msg / error.message。
func extractMessage(data []byte) string {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil {
		return ""
	}
	for _, key := range []string{"message", "msg"} {
		if raw, ok := payload[key]; ok {
			var text string
			if err := json.Unmarshal(raw, &text); err == nil && text != "" {
				return text
			}
		}
	}
	if raw, ok := payload["error"]; ok {
		var inner map[string]json.RawMessage
		if err := json.Unmarshal(raw, &inner); err == nil {
			var text string
			if err := json.Unmarshal(inner["message"], &text); err == nil && text != "" {
				return text
			}
		}
	}
	return ""
}

// flexInt 把 JSON 数字或数字字符串转成 *int64；缺失/null/非法返回 nil。
func flexInt(raw json.RawMessage) *int64 {
	if len(raw) == 0 {
		return nil
	}
	text := strings.TrimSpace(string(raw))
	if text == "null" || text == `""` || text == "" {
		return nil
	}
	text = strings.Trim(text, `"`)
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil
	}
	n := int64(value)
	return &n
}
