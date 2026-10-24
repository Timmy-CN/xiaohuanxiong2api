package server

import (
	"testing"
	"time"

	"xhx2api/internal/upstream"
)

// 带时区的 RFC3339 直接解。
func TestParseBillExpiryRFC3339(t *testing.T) {
	got, ok := parseBillExpiry("2026-10-25T00:00:00+08:00")
	if !ok {
		t.Fatal("RFC3339 应可解析")
	}
	if want := time.Date(2026, 10, 25, 0, 0, 0, 0, beijingLoc); !got.Equal(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// 无时区标记的按北京 +08:00 解。
func TestParseBillExpiryNaiveBeijing(t *testing.T) {
	for _, value := range []string{"2026-10-25 00:00:00", "2026-10-25T00:00:00"} {
		got, ok := parseBillExpiry(value)
		if !ok {
			t.Fatalf("%q 应可解析", value)
		}
		if want := time.Date(2026, 10, 25, 0, 0, 0, 0, beijingLoc); !got.Equal(want) {
			t.Fatalf("%q: got %v, want %v", value, got, want)
		}
	}
}

// 空串与乱串 → 不过期（false）。
func TestParseBillExpiryUnparseable(t *testing.T) {
	for _, value := range []string{"", "   ", "not-a-date", "2026/10/25"} {
		if _, ok := parseBillExpiry(value); ok {
			t.Fatalf("%q 不应解析成功", value)
		}
	}
}

func billAt(id string, points int64, expireAt string) upstream.Bill {
	return upstream.Bill{BillID: id, Points: points, ExpireAt: expireAt, CreatedAt: "2026-10-09T00:00:17+08:00"}
}

// 已过期丢弃；最近到期时点的入账合计；同刻合并；不过期的不参与 expiring。
func TestAnalyzeCreditsBucketsAndExpiry(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, beijingLoc)
	bills := []upstream.Bill{
		billAt("gone", 300, "2026-10-01T00:00:00+08:00"),      // 已过期 → 丢弃
		billAt("soon-a", 300, "2026-10-10T00:00:00+08:00"),    // 最近到期
		billAt("soon-b", 100, "2026-10-10T00:00:00+08:00"),    // 同刻 → 合并
		billAt("later", 3000, "2026-12-01T00:00:00+08:00"),    // 更晚
		billAt("forever", 500, ""),                            // 无到期日 → granted 但不参与 expiring
		billAt("spent", -50, "2026-12-01T00:00:00+08:00"),     // 负数 → 忽略
		billAt("junk-date", 200, "not-a-date"),                // 解不出 → granted 不参与 expiring
	}
	credits := analyzeCredits(bills, 3600, now)

	if credits.GrantedPoints != 300+100+3000+500+200 {
		t.Fatalf("granted_points = %d, 期望 4100", credits.GrantedPoints)
	}
	if credits.ExpiringPoints != 400 {
		t.Fatalf("expiring_points = %d, 期望 400", credits.ExpiringPoints)
	}
	wantAt := time.Date(2026, 10, 10, 0, 0, 0, 0, beijingLoc).UnixMilli()
	if credits.ExpiringAt != wantAt {
		t.Fatalf("expiring_at = %d, 期望 %d", credits.ExpiringAt, wantAt)
	}
	if credits.UsedPoints == nil || *credits.UsedPoints != 500 {
		t.Fatalf("used_points = %v, 期望 500（granted 4100 - available 3600）", credits.UsedPoints)
	}
	if credits.BillsScanned != len(bills) {
		t.Fatalf("bills_scanned = %d, 期望 %d", credits.BillsScanned, len(bills))
	}
}

// granted < available（有充值/历史结余）→ 不给 used。
func TestAnalyzeCreditsNoUsedWhenGrantedBelowAvailable(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, beijingLoc)
	bills := []upstream.Bill{billAt("a", 300, "2026-10-10T00:00:00+08:00")}
	credits := analyzeCredits(bills, 10000, now)
	if credits.UsedPoints != nil {
		t.Fatalf("used_points 应缺省, 实际 %v", *credits.UsedPoints)
	}
	if credits.ExpiringAt == 0 || credits.ExpiringPoints != 300 {
		t.Fatalf("expiring = %d @ %d", credits.ExpiringPoints, credits.ExpiringAt)
	}
}

// 全部已过期 → granted 0、无 expiring、无 used。
func TestAnalyzeCreditsAllExpired(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, beijingLoc)
	bills := []upstream.Bill{billAt("a", 300, "2026-10-01T00:00:00+08:00")}
	credits := analyzeCredits(bills, 0, now)
	if credits.GrantedPoints != 0 || credits.ExpiringPoints != 0 || credits.ExpiringAt != 0 || credits.UsedPoints != nil {
		t.Fatalf("credits = %+v", credits)
	}
}
