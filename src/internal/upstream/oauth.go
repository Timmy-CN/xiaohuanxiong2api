package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// CodeExpired 是「授权码已失效」的上游业务码（SPEC5 §I）。
//
// 它值得单独识别：这不是「登录服务坏了」，而是用户把回调链接放太久、
// 或重复点了两次，文案必须提示重新发起，而不是让服务端重试。
const CodeExpired = 200035

// AuthCodeError 表示用授权码换凭证失败，携带上游 HTTP 状态码与业务码。
//
// HTTPStatus 用于区分「上游 4xx 透传」与「其余归 502」；HasCode 表示上游
// 响应体里确实带了 code 字段。
type AuthCodeError struct {
	HTTPStatus int
	Code       int
	HasCode    bool
	Message    string
}

// Error 实现 error 接口（不含授权码本身）。
func (e *AuthCodeError) Error() string {
	if e.HasCode {
		return fmt.Sprintf("HTTP %d: code=%d message=%s", e.HTTPStatus, e.Code, e.Message)
	}
	return fmt.Sprintf("HTTP %d: %s", e.HTTPStatus, e.Message)
}

// AuthorizationCode 是 login_with_authorization_code 换回的凭证（SPEC5 §I）。
type AuthorizationCode struct {
	AccessToken    string
	RefreshToken   string
	OfficeIdentity string
	OfficeOrgName  string
	OfficeOrgRole  string
}

// ExchangeAuthorizationCode 用一次性授权码换凭证（POST {auth_base}/login_with_authorization_code）。
//
// 判定口径（SPEC5 §I / 参照 agent2api oauth.rs）：
//   - 业务码 200035 → *AuthCodeError{Code: 200035}（调用方映射成 400「授权码已失效」）；
//   - 非 2xx 或 access_token/refresh_token 缺失 → *AuthCodeError（带真实 HTTP 状态码）；
//   - 传输层错误 → 原样返回（调用方按超时/网络错误区分 504/502）。
//
// 超时沿用 upstream.refresh_timeout_seconds（同为鉴权域的一次性动作）。
func (c *Client) ExchangeAuthorizationCode(ctx context.Context, code string) (*AuthorizationCode, error) {
	url := c.opts.AuthBase + "/login_with_authorization_code"
	body, err := json.Marshal(map[string]string{"authorization_code": code})
	if err != nil {
		return nil, fmt.Errorf("序列化网页登录请求失败: %w", err)
	}
	status, data, err := c.doJSON(ctx, http.MethodPost, url, body, "",
		map[string]string{"Content-Type": "application/json"}, c.opts.RefreshTimeout, maxErrorBytes)
	if err != nil {
		return nil, fmt.Errorf("网页登录换取凭证请求失败: %w", err)
	}
	var resp struct {
		Code    *int            `json:"code"`
		Message string          `json:"message"`
		Msg     string          `json:"msg"`
		Data    json.RawMessage `json:"data"`
	}
	_ = json.Unmarshal(data, &resp)

	hasCode := resp.Code != nil
	upstreamCode := 0
	if hasCode {
		upstreamCode = *resp.Code
	}
	if hasCode && upstreamCode == CodeExpired {
		return nil, &AuthCodeError{
			HTTPStatus: status,
			Code:       upstreamCode,
			HasCode:    true,
			Message:    "授权码已失效",
		}
	}

	var dataMap map[string]json.RawMessage
	if len(resp.Data) > 0 && string(resp.Data) != "null" {
		_ = json.Unmarshal(resp.Data, &dataMap)
	}
	textOf := func(key string) string { return stripBearer(jsonText(dataMap[key])) }

	accessToken := textOf("access_token")
	refreshToken := textOf("refresh_token")
	if status < 200 || status >= 300 || accessToken == "" || refreshToken == "" {
		message := resp.Message
		if message == "" {
			message = resp.Msg
		}
		if message == "" {
			message = jsonMessage(data)
		}
		return nil, &AuthCodeError{
			HTTPStatus: status,
			Code:       upstreamCode,
			HasCode:    hasCode,
			Message:    message,
		}
	}
	return &AuthorizationCode{
		AccessToken:    accessToken,
		RefreshToken:   refreshToken,
		OfficeIdentity: textOf("office_identity"),
		OfficeOrgName:  textOf("office_org_name"),
		OfficeOrgRole:  textOf("office_org_role"),
	}, nil
}

// jsonText 把一段原始 JSON 尽量取成字符串（兼容字符串与数字）。
func jsonText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var num json.Number
	if err := json.Unmarshal(raw, &num); err == nil {
		return num.String()
	}
	return ""
}

// stripBearer 去掉可能出现的 "Bearer " 前缀并 trim。
func stripBearer(value string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "Bearer "))
}
