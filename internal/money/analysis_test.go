package money

import (
	"context"
	"testing"
)

// Pure-function tests for the analysis math: no MongoDB, so these run
// everywhere. Fixture numbers mirror the plan's shared March fixture.

func TestAvgPaiseHalfUp(t *testing.T) {
	if got := avgPaise(827025, 31); got != 26678 {
		t.Errorf("avgPaise(827025, 31) = %d, want 26678", got)
	}
	if got := avgPaise(500000, 8); got != 62500 {
		t.Errorf("avgPaise(500000, 8) = %d, want 62500", got)
	}
	// Exact .5 rounds up: 7/2 = 3.5 -> 4.
	if got := avgPaise(7, 2); got != 4 {
		t.Errorf("avgPaise(7, 2) = %d, want 4", got)
	}
	if got := avgPaise(0, 0); got != 0 {
		t.Errorf("avgPaise(0, 0) = %d, want 0", got)
	}
}

func TestRoundPct2(t *testing.T) {
	// Savings rate from the fixture: 7322975/8150000 = 89.8524...% -> 89.85.
	if got := roundPct2(7322975, 8150000); got != 89.85 {
		t.Errorf("roundPct2 savings = %v, want 89.85", got)
	}
	// Delta% from compare: 797025/30000 = 2656.75 exactly.
	if got := roundPct2(797025, 30000); got != 2656.75 {
		t.Errorf("roundPct2 delta = %v, want 2656.75", got)
	}
	// groceries share: 430050/827025 = 52.0007...% -> 52.00.
	if got := roundPct2(430050, 827025); got != 52.0 {
		t.Errorf("roundPct2 groceries = %v, want 52.00", got)
	}
}

func TestMedianPaise(t *testing.T) {
	if got := medianPaise(nil); got != 0 {
		t.Errorf("median of empty = %d, want 0", got)
	}
	if got := medianPaise([]int64{500}); got != 500 {
		t.Errorf("median of one = %d, want 500", got)
	}
	if got := medianPaise([]int64{300, 100, 200}); got != 200 {
		t.Errorf("median odd = %d, want 200", got)
	}
	// (2+3)/2 = 2.5 -> 3 (half-up).
	if got := medianPaise([]int64{1, 2, 3, 4}); got != 3 {
		t.Errorf("median even = %d, want 3", got)
	}
	// (1+2)/2 = 1.5 -> 2.
	if got := medianPaise([]int64{2, 1}); got != 2 {
		t.Errorf("median pair = %d, want 2", got)
	}
}

func TestShares100SumsExactly(t *testing.T) {
	amounts := []int64{430050, 240000, 45000, 12075, 99900}
	shares := shares100(amounts)
	var hundredths int64
	for _, s := range shares {
		hundredths += int64(s*100 + 0.5)
	}
	if hundredths != 10000 {
		t.Errorf("shares sum to %d hundredths, want 10000 (got %v)", hundredths, shares)
	}
	if shares[0] != 52.0 {
		t.Errorf("groceries share = %v, want 52.00", shares[0])
	}
	// A pathological split that naive rounding breaks: thirds.
	thirds := shares100([]int64{1, 1, 1})
	var thundredths int64
	for _, s := range thirds {
		thundredths += int64(s*100 + 0.5)
	}
	if thundredths != 10000 {
		t.Errorf("thirds sum to %d hundredths, want 10000", thundredths)
	}
	if got := len(thirds); got != 3 {
		t.Fatalf("len = %d", got)
	}
	// 33.34 + 33.33 + 33.33 in some order.
	big := 0
	for _, s := range thirds {
		if s == 33.34 {
			big++
		}
	}
	if big != 1 {
		t.Errorf("thirds = %v, want exactly one 33.34", thirds)
	}
}

func TestProrate(t *testing.T) {
	// 500000 × 13/28 = 232142.857... -> 232143.
	if got := prorate(500000, 13, 28); got != 232143 {
		t.Errorf("prorate Feb = %d, want 232143", got)
	}
	// 600000 × 15/31 = 290322.58... -> 290323.
	if got := prorate(600000, 15, 31); got != 290323 {
		t.Errorf("prorate Mar = %d, want 290323", got)
	}
	// Full month is exact.
	if got := prorate(600000, 31, 31); got != 600000 {
		t.Errorf("prorate full = %d, want 600000", got)
	}
	if got := prorate(600000, 0, 31); got != 0 {
		t.Errorf("prorate zero days = %d, want 0", got)
	}
}

func TestDaysBetween(t *testing.T) {
	if got, _ := DaysBetween("2026-03-01", "2026-03-31"); got != 31 {
		t.Errorf("March days = %d, want 31", got)
	}
	if got, _ := DaysBetween("2026-02-01", "2026-02-28"); got != 28 {
		t.Errorf("Feb days = %d, want 28", got)
	}
	if got, _ := DaysBetween("2026-03-05", "2026-03-05"); got != 1 {
		t.Errorf("single day = %d, want 1", got)
	}
}

func TestBucketGridMarch2026(t *testing.T) {
	// 2026-03-01 is a Sunday: the first week bucket starts Monday 2026-02-23
	// and clips to 1 day inside the window.
	weeks, err := bucketGrid("2026-03-01", "2026-03-31", "week")
	if err != nil {
		t.Fatalf("bucketGrid: %v", err)
	}
	if len(weeks) != 6 {
		t.Fatalf("weeks = %d, want 6", len(weeks))
	}
	if weeks[0].Key != "2026-02-23" || weeks[0].From != "2026-03-01" || weeks[0].Days != 1 {
		t.Errorf("first week = %+v, want key 2026-02-23 from 2026-03-01 days 1", weeks[0])
	}
	last := weeks[len(weeks)-1]
	if last.Key != "2026-03-30" || last.Days != 2 {
		t.Errorf("last week = %+v, want key 2026-03-30 days 2", last)
	}
}

func TestBucketGridZeroFillMonths(t *testing.T) {
	months, err := bucketGrid("2026-01-01", "2026-03-31", "month")
	if err != nil {
		t.Fatalf("bucketGrid: %v", err)
	}
	if len(months) != 3 {
		t.Fatalf("months = %d, want 3", len(months))
	}
	if months[0].Key != "2026-01" || months[0].Days != 31 {
		t.Errorf("Jan = %+v", months[0])
	}
	if months[1].Key != "2026-02" || months[1].Days != 28 {
		t.Errorf("Feb = %+v (2026 is not a leap year)", months[1])
	}
}

func TestBucketGridDayCap(t *testing.T) {
	if _, err := bucketGrid("2025-01-01", "2026-12-31", "day"); err == nil {
		t.Fatal("730-day grid was accepted; expected the 500-bucket rejection")
	} else if got := err.Error(); !containsSub(got, "500") {
		t.Errorf("error %q does not name the cap", got)
	}
	if _, err := bucketGrid("2026-02-30", "2026-03-01", "day"); err == nil {
		t.Fatal("impossible date was accepted")
	}
}

func containsSub(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestCadenceBands(t *testing.T) {
	for interval, want := range map[int]string{
		3: "irregular", 5: "weekly", 9: "weekly", 10: "irregular",
		12: "fortnightly", 17: "fortnightly", 25: "monthly", 35: "monthly",
		36: "irregular", 80: "quarterly", 100: "quarterly", 45: "irregular",
	} {
		if got := cadenceFor(interval); got != want {
			t.Errorf("cadenceFor(%d) = %q, want %q", interval, got, want)
		}
	}
}

func TestClusterAmountsExactVsTolerance(t *testing.T) {
	rows := []TxRow{
		{ID: "a", Date: "2026-01-05", AmountPaise: 120000},
		{ID: "b", Date: "2026-02-05", AmountPaise: 132000},
		{ID: "c", Date: "2026-03-05", AmountPaise: 125000},
	}
	if cls := clusterAmounts(rows, 0); len(cls) != 0 {
		t.Errorf("exact clustering kept %d clusters of near-equal amounts, want 0 (single-row clusters drop)", len(cls))
	}
	cls := clusterAmounts(rows, 10)
	if len(cls) != 1 {
		t.Fatalf("tolerance clustering = %d clusters, want 1", len(cls))
	}
	if len(cls[0]) != 3 {
		t.Errorf("cluster size = %d, want 3", len(cls[0]))
	}
}

func TestDescribeClusterBills(t *testing.T) {
	rows := []TxRow{
		{ID: "a", Date: "2026-01-05", AmountPaise: 240000, Category: "bills", Type: "expense", Account: "HDFC Checking"},
		{ID: "b", Date: "2026-02-05", AmountPaise: 240000, Category: "bills", Type: "expense", Account: "HDFC Checking"},
		{ID: "c", Date: "2026-03-05", AmountPaise: 240000, Category: "bills", Type: "expense", Account: "HDFC Checking"},
	}
	c, ok := describeCluster(rows)
	if !ok {
		t.Fatal("describeCluster failed on a clean monthly triple")
	}
	// Jan05->Feb05 is 31 days, Feb05->Mar05 is 28: median 30 (half-up of
	// 29.5), jitter 2, still monthly/high-confidence.
	if c.Cadence != "monthly" || c.MedianIntervalDays != 30 {
		t.Errorf("cadence = %q interval %d, want monthly 30", c.Cadence, c.MedianIntervalDays)
	}
	if c.IntervalJitterDays != 2 {
		t.Errorf("jitter = %d, want 2", c.IntervalJitterDays)
	}
	if c.Confidence != "high" {
		t.Errorf("confidence = %q, want high", c.Confidence)
	}
	if c.NextExpectedDate != "2026-04-04" {
		t.Errorf("next = %q, want 2026-04-04", c.NextExpectedDate)
	}
}

func TestNormalizeCategories(t *testing.T) {
	got := NormalizeCategories([]string{" Groceries ", "bills", "groceries", "  "})
	if len(got) != 2 || got[0] != "groceries" || got[1] != "bills" {
		t.Errorf("got %v, want [groceries bills]", got)
	}
	// Unknown names are kept, not rejected: a name that does not exist
	// matches no row, so the read returns the empty table (#619).
	got = NormalizeCategories([]string{"grocerys"})
	if len(got) != 1 || got[0] != "grocerys" {
		t.Errorf("got %v, want [grocerys]", got)
	}
	if got := NormalizeCategory(" Groceries "); got != "groceries" {
		t.Errorf("got %q, want groceries", got)
	}
	if got := NormalizeCategory("   "); got != "" {
		t.Errorf("blank normalizes to %q, want absent", got)
	}
	// A user-added category normalizes the same way: the set is live, not fixed.
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateCategory(ctx, "subscriptions"); err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	if got := NormalizeCategories([]string{"subscriptions"}); len(got) != 1 || got[0] != "subscriptions" {
		t.Errorf("fresh category normalized to %v", got)
	}
}

func TestNormalizeSortAndOrder(t *testing.T) {
	if got, _ := NormalizeSort("t", "", "amount", []string{"amount", "name"}); got != "amount" {
		t.Errorf("blank sort default = %q, want amount", got)
	}
	if got, _ := NormalizeSort("t", "NAME", "amount", []string{"amount", "name"}); got != "name" {
		t.Errorf("sort normalize = %q, want name", got)
	}
	if _, err := NormalizeSort("t", "colour", "amount", []string{"amount", "name"}); err == nil {
		t.Fatal("unknown sort was accepted")
	}
	if got, _ := NormalizeOrder("t", "", "desc"); got != "desc" {
		t.Errorf("blank order default = %q, want desc", got)
	}
	if _, err := NormalizeOrder("t", "sideways", "desc"); err == nil {
		t.Fatal("unknown order was accepted")
	}
	if got := SensitivityRatio("low"); got != 2.00 {
		t.Errorf("low ratio = %v, want 2.00", got)
	}
	if got := SensitivityRatio(""); got != 1.50 {
		t.Errorf("default ratio = %v, want 1.50", got)
	}
	if _, err := NormalizeSensitivity("t", "max"); err == nil {
		t.Fatal("unknown sensitivity was accepted")
	}
}

func TestCheckBudgetValidators(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CheckBudgetCategory(ctx, "t", "salary"); err == nil {
		t.Fatal("salary was accepted as a budget category")
	}
	if _, err := s.CheckBudgetCategory(ctx, "t", "grocerys"); err == nil {
		t.Fatal("unknown category was accepted")
	}
	if m, err := CheckBudgetMonth("t", "2026-03"); err != nil || m != "2026-03" {
		t.Errorf("valid month: %v %v", m, err)
	}
	for _, bad := range []string{"", "2026-13", "2026-1", "26-03"} {
		if _, err := CheckBudgetMonth("t", bad); err == nil {
			t.Errorf("month %q was accepted", bad)
		}
	}
	if _, err := CheckBudgetLimit("t", 0); err == nil {
		t.Fatal("zero limit was accepted")
	}
	if _, err := CheckBudgetLimit("t", -5); err == nil {
		t.Fatal("negative limit was accepted")
	}
	if p, err := CheckBudgetLimit("t", 500000); err != nil || p != 500000 {
		t.Errorf("valid limit: %d %v", p, err)
	}
}
