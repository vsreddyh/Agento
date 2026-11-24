package money

import (
	"context"
	"testing"
)

// DB-backed analysis tests: they seed the plan's shared fixture and pin the
// exact vectors, so a wrong aggregation fails here instead of in review.
// They skip without MONGODB_URI like the rest of this package's DB tests.

// seedAnalysisFixture builds the shared fixture: four accounts, nine March
// rows, two February rows (archived-account history), then archives Old
// Wallet. Amounts are rupees; storage is paise.
func seedAnalysisFixture(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	_, err := s.CreateAccount(ctx, "Cash", "cash", 1000)
	must(err)
	_, err = s.CreateAccount(ctx, "HDFC Checking", "bank", 50000)
	must(err)
	_, err = s.CreateAccount(ctx, "Equity Card", "card", 0)
	must(err)
	_, err = s.CreateAccount(ctx, "Old Wallet", "cash", 250)
	must(err)
	log := func(date string, amount float64, typ, cat, acct, dst string) {
		t.Helper()
		_, _, err := s.Insert(ctx, date, amount, typ, cat, acct, dst, "", "mcp", "")
		must(err)
	}
	// February: archived-account history. The 250 transfer leaves HDFC, so
	// HDFC's March opening is 49750.00, not its 50000 seed.
	log("2026-02-05", 300, "expense", "health", "Old Wallet", "")
	log("2026-02-10", 250, "transfer", "other", "HDFC Checking", "Old Wallet")
	log("2026-03-01", 80000, "income", "salary", "HDFC Checking", "")
	log("2026-03-02", 2500.50, "expense", "groceries", "HDFC Checking", "")
	log("2026-03-03", 450, "expense", "eating out", "Equity Card", "")
	log("2026-03-05", 5000, "transfer", "other", "HDFC Checking", "Cash")
	log("2026-03-08", 120.75, "expense", "transport", "Cash", "")
	log("2026-03-12", 1800, "expense", "groceries", "Equity Card", "")
	log("2026-03-20", 999, "expense", "fun", "HDFC Checking", "")
	log("2026-03-28", 2400, "expense", "bills", "HDFC Checking", "")
	log("2026-03-31", 1500, "income", "other", "Cash", "")
	if _, _, err := s.ArchiveAccount(ctx, "Old Wallet"); err != nil {
		t.Fatalf("archive Old Wallet: %v", err)
	}
}

func TestAnalysisPeriodTotals(t *testing.T) {
	s := testStore(t)
	seedAnalysisFixture(t, s)
	ctx := context.Background()
	got, err := s.PeriodTotals(ctx, "2026-03-01", "2026-03-31", nil, nil)
	if err != nil {
		t.Fatalf("PeriodTotals: %v", err)
	}
	if got.IncomePaise != 8150000 || got.ExpensePaise != 827025 || got.NetPaise != 7322975 {
		t.Errorf("totals = %d/%d/%d, want 8150000/827025/7322975",
			got.IncomePaise, got.ExpensePaise, got.NetPaise)
	}
	if got.RowsTotal != 9 || got.RowsIncome != 2 || got.RowsExpense != 6 || got.RowsTransfer != 1 {
		t.Errorf("rows = %d/%d/%d/%d, want 9/2/6/1",
			got.RowsTotal, got.RowsIncome, got.RowsExpense, got.RowsTransfer)
	}
	if got.SavingsRatePct == nil || *got.SavingsRatePct != 89.85 {
		t.Errorf("savings rate = %v, want 89.85", got.SavingsRatePct)
	}
	if got.AvgDailyExpensePaise != 26678 || got.AvgDailyIncomePaise != 262903 {
		t.Errorf("daily avgs = %d/%d, want 26678/262903",
			got.AvgDailyExpensePaise, got.AvgDailyIncomePaise)
	}
	if got.TransferCount != 1 || got.TransferVolumePaise != 500000 {
		t.Errorf("transfers = %d/%d, want 1/500000", got.TransferCount, got.TransferVolumePaise)
	}
}

func TestAnalysisBreakdownMatchesTotals(t *testing.T) {
	s := testStore(t)
	seedAnalysisFixture(t, s)
	ctx := context.Background()
	tot, err := s.PeriodTotals(ctx, "2026-03-01", "2026-03-31", nil, nil)
	if err != nil {
		t.Fatalf("PeriodTotals: %v", err)
	}
	res, err := s.BreakdownByCategory(ctx, "2026-03-01", "2026-03-31", "expense", nil, nil, "amount", "desc")
	if err != nil {
		t.Fatalf("BreakdownByCategory: %v", err)
	}
	if res.TotalAmountPaise != tot.ExpensePaise {
		t.Errorf("breakdown total %d != totals expense %d", res.TotalAmountPaise, tot.ExpensePaise)
	}
	if res.TotalCount != tot.RowsExpense {
		t.Errorf("breakdown count %d != totals rows %d", res.TotalCount, tot.RowsExpense)
	}
	if len(res.Rows) == 0 || res.Rows[0].Category != "groceries" || res.Rows[0].AmountPaise != 430050 {
		t.Errorf("first row = %+v, want groceries 430050", res.Rows)
	}
	// Subset filter: footer follows the selection.
	sub, err := s.BreakdownByCategory(ctx, "2026-03-01", "2026-03-31", "expense",
		[]string{"groceries", "bills"}, nil, "amount", "desc")
	if err != nil {
		t.Fatalf("subset breakdown: %v", err)
	}
	if sub.TotalAmountPaise != 430050+240000 || sub.TotalCount != 3 {
		t.Errorf("subset footer = %d/%d, want 670050/3", sub.TotalAmountPaise, sub.TotalCount)
	}
	var sum float64
	for _, g := range sub.Rows {
		sum += g.SharePct
	}
	if sum < 99.995 || sum > 100.005 {
		t.Errorf("subset shares sum = %v, want 100.00", sum)
	}
}

func TestAnalysisAccountFlow(t *testing.T) {
	s := testStore(t)
	seedAnalysisFixture(t, s)
	ctx := context.Background()
	res, err := s.AccountFlow(ctx, "2026-03-01", "2026-03-31", nil, nil, "closing", "desc")
	if err != nil {
		t.Fatalf("AccountFlow: %v", err)
	}
	byName := map[string]AccountFlowRow{}
	for _, r := range res.Rows {
		byName[r.Name] = r
	}
	hdfc := byName["HDFC Checking"]
	if hdfc.OpeningPaise != 4975000 || hdfc.ClosingPaise != 11885050 || hdfc.NetPaise != 6910050 {
		t.Errorf("HDFC = opening %d closing %d net %d, want 4975000/11885050/6910050",
			hdfc.OpeningPaise, hdfc.ClosingPaise, hdfc.NetPaise)
	}
	cash := byName["Cash"]
	if cash.OpeningPaise != 100000 || cash.ClosingPaise != 737925 {
		t.Errorf("Cash = opening %d closing %d, want 100000/737925", cash.OpeningPaise, cash.ClosingPaise)
	}
	old, ok := byName["Old Wallet"]
	if !ok || !old.Archived || old.ClosingPaise != 20000 {
		t.Errorf("Old Wallet = %+v present=%v, want archived closing 20000", old, ok)
	}
	if res.TotalClosingPaise != 12417975 {
		t.Errorf("total closing = %d, want 12417975", res.TotalClosingPaise)
	}
	if res.TotalActiveClosingPaise != 12397975 {
		t.Errorf("active closing = %d, want 12397975", res.TotalActiveClosingPaise)
	}
	// Date-equal cross-check with get_balances: the fixture has no rows after
	// March and none before February, so the March closing already absorbs
	// the whole ledger — active closing must equal the lifetime total.
	bal, err := s.Balances(ctx)
	if err != nil {
		t.Fatalf("Balances: %v", err)
	}
	balTotal, _ := bal["total"].(float64)
	if want := ToRupees(res.TotalActiveClosingPaise); balTotal != want {
		t.Errorf("balances total = %v, want active closing %v", balTotal, want)
	}
}

func TestAnalysisSeriesSumsToTotals(t *testing.T) {
	s := testStore(t)
	seedAnalysisFixture(t, s)
	ctx := context.Background()
	tot, err := s.PeriodTotals(ctx, "2026-01-01", "2026-03-31", nil, nil)
	if err != nil {
		t.Fatalf("PeriodTotals: %v", err)
	}
	buckets, err := s.Series(ctx, "2026-01-01", "2026-03-31", "month", nil, nil, "asc")
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(buckets) != 3 {
		t.Fatalf("buckets = %d, want 3", len(buckets))
	}
	var income, expense, tvol int64
	for _, b := range buckets {
		income += b.IncomePaise
		expense += b.ExpensePaise
		tvol += b.TransferVolumePaise
	}
	// January is empty; February adds the health expense + the 250 transfer.
	if buckets[0].ExpensePaise != 0 || buckets[0].RowsTotal != 0 {
		t.Errorf("Jan bucket = %+v, want zeros", buckets[0])
	}
	if income != tot.IncomePaise || expense != tot.ExpensePaise || tvol != tot.TransferVolumePaise {
		t.Errorf("series sums %d/%d/%d != totals %d/%d/%d",
			income, expense, tvol, tot.IncomePaise, tot.ExpensePaise, tot.TransferVolumePaise)
	}
}

func TestAnalysisCompareAndRecurring(t *testing.T) {
	s := testStore(t)
	seedAnalysisFixture(t, s)
	ctx := context.Background()
	deltas, err := s.CompareTotals(ctx, "2026-02-01", "2026-02-28", "2026-03-01", "2026-03-31", nil, nil)
	if err != nil {
		t.Fatalf("CompareTotals: %v", err)
	}
	if len(deltas) != 3 {
		t.Fatalf("metrics = %d, want 3", len(deltas))
	}
	var exp *MetricDelta
	for i := range deltas {
		if deltas[i].Metric == "expense" {
			exp = &deltas[i]
		}
	}
	if exp == nil || exp.APaise != 30000 || exp.BPaise != 827025 || exp.DeltaPaise != 797025 {
		t.Errorf("expense delta = %+v, want a=30000 b=827025 delta=797025", exp)
	}
	if exp.DeltaPct == nil || *exp.DeltaPct != 2656.75 {
		t.Errorf("expense delta pct = %v, want 2656.75", exp.DeltaPct)
	}
	// Recurring over Jan-Jun needs more rows than the fixture has; assert the
	// empty answer shape instead of a false detection.
	cands, total, err := s.DetectRecurring(ctx, "2026-03-01", "2026-03-31", nil, nil, 3, 0, 10, "next", "asc")
	if err != nil {
		t.Fatalf("DetectRecurring: %v", err)
	}
	if total != 0 || len(cands) != 0 {
		t.Errorf("March-only recurring = %d candidates, want 0 (nothing repeats 3× in March)", total)
	}
}

func TestBudgetCRUDAndStatus(t *testing.T) {
	s := testStore(t)
	seedAnalysisFixture(t, s)
	ctx := context.Background()
	b, err := s.CreateBudget(ctx, "groceries", "2026-01", 5000.00)
	if err != nil {
		t.Fatalf("CreateBudget: %v", err)
	}
	if b.MonthlyLimitPaise != 500000 {
		t.Errorf("limit = %d, want 500000", b.MonthlyLimitPaise)
	}
	if _, err := s.CreateBudget(ctx, "groceries", "2026-01", 6000.00); err == nil {
		t.Fatal("duplicate category+month was accepted")
	}
	if _, err := s.CreateBudget(ctx, "Groceries", "2026-03", 6000.00); err != nil {
		t.Fatalf("second version: %v", err)
	}
	// 2026-02 resolves to the January row.
	lim, ok, err := s.resolveLimit(ctx, "groceries", "2026-02")
	if err != nil || !ok || lim != 500000 {
		t.Errorf("Feb resolution = %d/%v/%v, want 500000/true/nil", lim, ok, err)
	}
	// Status for the full month: limit 600000, spend 430050.
	st, err := s.BudgetStatus(ctx, "2026-03-01", "2026-03-31", nil, "remaining", "asc")
	if err != nil {
		t.Fatalf("BudgetStatus: %v", err)
	}
	var groc *BudgetStatusRow
	for i := range st.Rows {
		if st.Rows[i].Category == "groceries" {
			groc = &st.Rows[i]
		}
	}
	if groc == nil {
		t.Fatalf("no groceries row: %+v", st.Rows)
	}
	if groc.LimitPaise != 600000 || groc.SpendPaise != 430050 || groc.RemainingPaise == nil || *groc.RemainingPaise != 169950 {
		t.Errorf("groceries = %+v, want limit 600000 spend 430050 remaining 169950", groc)
	}
	if groc.UsedPct == nil || *groc.UsedPct != 71.68 {
		t.Errorf("used = %v, want 71.68", groc.UsedPct)
	}
	// Partial month proration: 232143 + 290323 = 522466.
	part, err := s.BudgetStatus(ctx, "2026-02-16", "2026-03-15", []string{"groceries"}, "remaining", "asc")
	if err != nil {
		t.Fatalf("partial BudgetStatus: %v", err)
	}
	if len(part.Rows) != 1 || part.Rows[0].LimitPaise != 522466 {
		t.Errorf("partial limit = %+v, want 522466", part.Rows)
	}
	if part.Rows[0].SpendPaise != 430050 {
		t.Errorf("partial spend = %d, want 430050", part.Rows[0].SpendPaise)
	}
	// Update then delete with fallback.
	upd, err := s.UpdateBudget(ctx, b.ID, 5500.00)
	if err != nil {
		t.Fatalf("UpdateBudget: %v", err)
	}
	if upd.MonthlyLimitPaise != 550000 {
		t.Errorf("updated limit = %d, want 550000", upd.MonthlyLimitPaise)
	}
	// Deleting the non-latest version scopes unbudgeted to its own month:
	// 2026-01 would have no budget, but 2026-04 still resolves through the
	// surviving 2026-03 row.
	early, err := s.DeleteBudget(ctx, upd.ID, true)
	if err != nil {
		t.Fatalf("dry-run delete non-latest: %v", err)
	}
	if !early.Unbudgeted || early.HasFallback {
		t.Errorf("non-latest dry run = %+v, want Unbudgeted with no fallback", early)
	}
	limApr, okApr, err := s.resolveLimit(ctx, "groceries", "2026-04")
	if err != nil || !okApr || limApr != 600000 {
		t.Errorf("Apr resolution after non-latest delete = %d/%v/%v, want 600000/true/nil", limApr, okApr, err)
	}
	del, err := s.DeleteBudget(ctx, upd.ID, true)
	if err != nil {
		t.Fatalf("dry-run delete: %v", err)
	}
	if !del.DryRun || del.Deleted != 0 {
		t.Errorf("dry run deleted: %+v", del)
	}
	del2, err := s.DeleteBudget(ctx, upd.ID, false)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if del2.Deleted != 1 {
		t.Errorf("deleted = %d, want 1", del2.Deleted)
	}
}
