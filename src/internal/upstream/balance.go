package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Balance 是积分余额（SPEC §1.4）。
type Balance struct {
	AvailablePoints *int64 `json:"available_points"`
	DailyPoints     *int64 `json:"daily_points"`
	MonthlyPoints   *int64 `json:"monthly_points"`
	RewardPoints    *int64 `json:"reward_points"`
	TopupPoints     *int64 `json:"topup_points"`
}

// UnmarshalJSON 兼容数字与字符串两种形态。
func (b *Balance) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	b.AvailablePoints = flexInt(raw["available_points"])
	b.DailyPoints = flexInt(raw["daily_points"])
	b.MonthlyPoints = flexInt(raw["monthly_points"])
	b.RewardPoints = flexInt(raw["reward_points"])
	b.TopupPoints = flexInt(raw["topup_points"])
	return nil
}

// Balance 查询积分余额（GET {points_base}/balance）。code != 0 视为失败。
func (c *Client) Balance(ctx context.Context, token string) (*Balance, error) {
	url := c.opts.PointsBase + "/balance"
	status, data, err := c.doJSON(ctx, http.MethodGet, url, nil, token,
		map[string]string{"Accept": "application/json"}, c.opts.BalanceTimeout, maxCatalogBytes)
	if err != nil {
		return nil, fmt.Errorf("查询积分失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("查询积分失败: HTTP %d: %s", status, jsonMessage(data))
	}
	var resp struct {
		Code    *int            `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("解析积分响应失败: %w", err)
	}
	if resp.Code != nil && *resp.Code != 0 {
		return nil, fmt.Errorf("查询积分失败: code=%d message=%s", *resp.Code, resp.Message)
	}
	var balance Balance
	if len(resp.Data) > 0 && string(resp.Data) != "null" {
		if err := json.Unmarshal(resp.Data, &balance); err != nil {
			return nil, fmt.Errorf("解析积分响应失败: %w", err)
		}
	}
	return &balance, nil
}

// Entitlement 查询订阅权益（GET {auth_base}/entitlement_info）。
// 返回 data 节点原始 JSON。code != 0 视为失败。
func (c *Client) Entitlement(ctx context.Context, token string) (json.RawMessage, error) {
	url := c.opts.AuthBase + "/entitlement_info"
	status, data, err := c.doJSON(ctx, http.MethodGet, url, nil, token,
		map[string]string{"Accept": "application/json"}, c.opts.BalanceTimeout, maxCatalogBytes)
	if err != nil {
		return nil, fmt.Errorf("查询订阅失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("查询订阅失败: HTTP %d: %s", status, jsonMessage(data))
	}
	var resp struct {
		Code    *int            `json:"code"`
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("解析订阅响应失败: %w", err)
	}
	if resp.Code != nil && *resp.Code != 0 {
		return nil, fmt.Errorf("查询订阅失败: code=%d message=%s", *resp.Code, resp.Message)
	}
	if len(resp.Data) == 0 {
		return json.RawMessage("null"), nil
	}
	return resp.Data, nil
}
