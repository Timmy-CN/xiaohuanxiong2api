package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Refresh 调鉴权 API 刷新 token（POST {auth_base}/refresh）。
//
// 返回新的 access_token 与 refresh_token。refresh_token 为空表示上游未轮换，
// 调用方应沿用旧值（SPEC §1.3）。非 2xx / code != 0 / data.access_token 为空都返回
// error，error 文本带上游 HTTP 状态码与上游 message（SPEC §4.4）。
func (c *Client) Refresh(ctx context.Context, refreshToken string) (string, string, error) {
	url := c.opts.AuthBase + "/refresh"
	body, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		return "", "", fmt.Errorf("序列化刷新请求失败: %w", err)
	}
	status, data, err := c.doJSON(ctx, http.MethodPost, url, body, "",
		map[string]string{"Content-Type": "application/json"}, c.opts.RefreshTimeout, maxErrorBytes)
	if err != nil {
		return "", "", fmt.Errorf("刷新 token 请求失败: %w", err)
	}
	var resp struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Data    struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		} `json:"data"`
	}
	_ = json.Unmarshal(data, &resp)
	if status < 200 || status >= 300 {
		return "", "", fmt.Errorf("刷新 token 失败: HTTP %d: %s", status, jsonMessage(data))
	}
	if resp.Code != nil && *resp.Code != 0 {
		return "", "", fmt.Errorf("刷新 token 失败: code=%d message=%s", *resp.Code, resp.Message)
	}
	if resp.Data.AccessToken == "" {
		return "", "", fmt.Errorf("刷新 token 失败: 上游未返回 access_token（HTTP %d message=%s）", status, resp.Message)
	}
	return resp.Data.AccessToken, resp.Data.RefreshToken, nil
}
