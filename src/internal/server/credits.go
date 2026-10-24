package server

// 积分构成分析（/admin/api/accounts/balance 的 credits 字段）。
//
// 口径：只把账单里的正数入账算作发放；已过期的丢弃；没有 expire_at 的视为
// 不过期；最近到期时点的入账合计单列，供面板提示「这批积分何时作废」。

import (
	"encoding/json"
	"strings"
	"time"

	"xhx2api/internal/upstream"
)

// beijingLoc 是账单时间的缺省时区：上游是商汤（中国），无时区标记的时间戳按 +08:00 解。
var beijingLoc = time.FixedZone("CST", 8*3600)

// parseBillExpiry 解析账单的 expire_at；解不出返回 false（视为不过期）。
func parseBillExpiry(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, true
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05"} {
		if t, err := time.ParseInLocation(layout, value, beijingLoc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseTimeAny 把上游时间字段（数字毫秒/秒，或字符串）转成 Unix 毫秒；解不出
// 返回 0。数字 ≥1e12 按毫秒、≥1e9 按秒；字符串无时区标记时按 +08:00 解。
func parseTimeAny(raw json.RawMessage) int64 {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}
	var num json.Number
	if err := json.Unmarshal(raw, &num); err == nil {
		if value, err := num.Float64(); err == nil && value > 0 {
			if value >= 1e12 {
				return int64(value)
			}
			if value >= 1e9 {
				return int64(value * 1000)
			}
		}
		return 0
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0
	}
	if exp, ok := parseBillExpiry(text); ok {
		return exp.UnixMilli()
	}
	return 0
}

// Credits 是单个账号的积分构成分析结果。
type Credits struct {
	GrantedPoints  int64  `json:"granted_points"`        // 未过期发放合计（含永不过期的）
	UsedPoints     *int64 `json:"used_points,omitempty"` // granted >= available 时给出
	ExpiringPoints int64  `json:"expiring_points"`       // 最近到期时点的入账合计
	ExpiringAt     int64  `json:"expiring_at,omitempty"` // 最近到期时刻（Unix 毫秒）
	BillsScanned   int    `json:"bills_scanned"`         // 参与分析的账单条数
}

// analyzeCredits 按账单分析积分构成；available 是当前可用积分（来自 balance）。
func analyzeCredits(bills []upstream.Bill, available int64, now time.Time) Credits {
	var granted, expiring int64
	var soonest time.Time
	for _, bill := range bills {
		if bill.Points <= 0 {
			continue
		}
		exp, ok := parseBillExpiry(bill.ExpireAt)
		if ok && !exp.After(now) {
			continue // 已过期
		}
		granted += bill.Points
		if !ok {
			continue // 永不过期的入账不参与「即将过期」
		}
		switch {
		case soonest.IsZero() || exp.Before(soonest):
			soonest = exp
			expiring = bill.Points
		case exp.Equal(soonest):
			expiring += bill.Points
		}
	}
	credits := Credits{
		GrantedPoints:  granted,
		ExpiringPoints: expiring,
		BillsScanned:   len(bills),
	}
	if !soonest.IsZero() {
		credits.ExpiringAt = soonest.UnixMilli()
	}
	if granted > 0 && granted >= available {
		used := granted - available
		credits.UsedPoints = &used
	}
	return credits
}
