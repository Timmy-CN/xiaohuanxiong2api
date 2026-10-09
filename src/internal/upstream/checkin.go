package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// StatusError 表示上游返回了非 2xx 状态码。
//
// 调用方（如每日签到）用 errors.As 取出 Status，以区分 401 需要强制刷新等情形。
type StatusError struct {
	Status  int
	Message string
}

// Error 实现 error 接口。
func (e *StatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("HTTP %d", e.Status)
}

// DailyGrant 是 setting_info 返回的发放提示（SPEC2 §A.1）。
type DailyGrant struct {
	Popups []json.RawMessage `json:"point_grant_popups"`
	Toast  json.RawMessage   `json:"point_grant_toast"`
}

// ClaimDailyGrant 触发每日积分发放（GET {origin}/api/web/office/v3/setting_info）。
//
// 判定：HTTP 2xx 且 code == 0 → 触发成功。popups / toast 原样返回。
// 超时沿用 upstream.balance_timeout_seconds（SPEC2 §A.1）。
func (c *Client) ClaimDailyGrant(ctx context.Context, token string) (*DailyGrant, error) {
	url := c.opts.Origin + "/api/web/office/v3/setting_info"
	headers := map[string]string{
		"Accept":             "application/json",
		"X-Client-Platform":  "desktop-windows",
		"X-Client-Version":   "v1.0.0",
		"X-Raccoon-Language": "zh",
	}
	status, data, err := c.doJSON(ctx, http.MethodGet, url, nil, token, headers, c.opts.BalanceTimeout, maxCatalogBytes)
	if err != nil {
		return nil, fmt.Errorf("触发每日积分发放失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, &StatusError{Status: status, Message: jsonMessage(data)}
	}
	var resp struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Data    struct {
			PointGrantPopups []json.RawMessage `json:"point_grant_popups"`
			PointGrantToast  json.RawMessage   `json:"point_grant_toast"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("解析每日积分发放响应失败: %w", err)
	}
	if resp.Code != nil && *resp.Code != 0 {
		return nil, fmt.Errorf("触发每日积分发放失败: code=%d message=%s", *resp.Code, resp.Message)
	}
	return &DailyGrant{
		Popups: resp.Data.PointGrantPopups,
		Toast:  resp.Data.PointGrantToast,
	}, nil
}

// Bill 是一条积分账单（SPEC2 §A.1）。
type Bill struct {
	BillID     string `json:"bill_id"`
	ClientType string `json:"client_type"`
	EventName  string `json:"event_name"`
	BizType    string `json:"biz_type"`
	Points     int64  `json:"points"`
	CreatedAt  string `json:"created_at"`
}

// Bills 查询积分账单（GET {origin}/api/web/points/v1/bills?paging.limit=N&paging.offset=0）。
//
// 超时用 upstream.bills_timeout_seconds（SPEC2 §A.1）。
func (c *Client) Bills(ctx context.Context, token string, limit int) ([]Bill, error) {
	if limit <= 0 {
		limit = 20
	}
	url := fmt.Sprintf("%s/api/web/points/v1/bills?paging.limit=%d&paging.offset=0", c.opts.Origin, limit)
	status, data, err := c.doJSON(ctx, http.MethodGet, url, nil, token,
		map[string]string{"Accept": "application/json"}, c.opts.BillsTimeout, maxCatalogBytes)
	if err != nil {
		return nil, fmt.Errorf("查询积分账单失败: %w", err)
	}
	if status < 200 || status >= 300 {
		return nil, &StatusError{Status: status, Message: jsonMessage(data)}
	}
	var resp struct {
		Code    *int   `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Items []Bill `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, fmt.Errorf("解析积分账单响应失败: %w", err)
	}
	if resp.Code != nil && *resp.Code != 0 {
		return nil, fmt.Errorf("查询积分账单失败: code=%d message=%s", *resp.Code, resp.Message)
	}
	return resp.Data.Items, nil
}
