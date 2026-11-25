package money

// Behavior pins for the #556 review batch (#558–#567, minus the already
// closed #557). Each test names the issue it pins. They run against the
// throwaway miser_test database like the rest of this package's DB tests.

import (
	"context"
	"math"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
)

// #559: rename keeps rows (they reference the id), collisions reject, retype
// normalizes, and a seed correction moves the balance with no new row.
func TestEditAccountLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 100000); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Cash comes from the testStore seed.
	id, _, err := s.Insert(ctx, "2026-09-01", 30000, "expense", "groceries", "HDFC", "", "", "", "")
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	// Rename.
	name := "HDFC Checking"
	acct, err := s.EditAccount(ctx, "HDFC", &name, nil, nil)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if acct["name"] != "HDFC Checking" {
		t.Fatalf("renamed account = %v", acct["name"])
	}
	rows, err := s.Match(ctx, map[string]any{"id": id})
	if err != nil || len(rows) != 1 || rows[0]["account"] != "HDFC Checking" {
		t.Fatalf("row did not follow the rename: %v %v", rows, err)
	}
	// Collision rejects.
	clash := "Cash"
	if _, err := s.EditAccount(ctx, "HDFC Checking", &clash, nil, nil); err == nil {
		t.Fatal("rename onto a taken name was accepted")
	}
	// Retype normalizes like create.
	typ := "Cash"
	acct, err = s.EditAccount(ctx, "HDFC Checking", nil, &typ, nil)
	if err != nil {
		t.Fatalf("retype: %v", err)
	}
	if acct["type"] != "cash" {
		t.Fatalf("retyped account = %v, want cash", acct["type"])
	}
	// Seed correction moves the balance by exactly the delta, writing no row.
	seed := int64(150000)
	acct, err = s.EditAccount(ctx, "HDFC Checking", nil, nil, &seed)
	if err != nil {
		t.Fatalf("seed correction: %v", err)
	}
	if acct["balance_paise"] != int64(120000) {
		t.Fatalf("balance after seed correction = %v, want 120000", acct["balance_paise"])
	}
	if n, err := s.txns.CountDocuments(ctx, bson.M{}); err != nil || n != 1 {
		t.Fatalf("seed correction wrote rows: count=%d err=%v", n, err)
	}
	// Unknown account and no-op edits reject.
	if _, err := s.EditAccount(ctx, "Nope", &name, nil, nil); err == nil {
		t.Fatal("edit of an unknown account was accepted")
	}
	same := "HDFC Checking"
	if _, err := s.EditAccount(ctx, "HDFC Checking", &same, nil, nil); err == nil {
		t.Fatal("a no-op edit was accepted")
	}
}

// #560: categories are data, not a fixed slice — create/list/rename/delete,
// with renames rewriting rows and deletes guarded by references.
func TestCategoriesLifecycle(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	live, err := s.ListCategories(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(live) != len(DefaultCategories) {
		t.Fatalf("defaults = %d, want %d (%v)", len(live), len(DefaultCategories), live)
	}
	if _, err := s.CreateCategory(ctx, "Subscriptions"); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := s.CreateCategory(ctx, "subscriptions"); err == nil {
		t.Fatal("case-insensitive duplicate was accepted")
	}
	if _, err := s.CreateCategory(ctx, "other"); err == nil {
		t.Fatal("reserved 'other' was created")
	}
	// The new name validates everywhere immediately.
	if _, _, err := s.Insert(ctx, "2026-09-01", 1000, "expense", "subscriptions", "Cash", "", "", "", ""); err != nil {
		t.Fatalf("log with new category: %v", err)
	}
	if _, err := s.CreateBudget(ctx, "subscriptions", "2026-09", 500000); err != nil {
		t.Fatalf("budget with new category: %v", err)
	}
	// Rename rewrites transactions and budgets.
	to, ntxn, nbud, err := s.UpdateCategory(ctx, "subscriptions", "streaming")
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if to != "streaming" || ntxn != 1 || nbud != 1 {
		t.Fatalf("rename = %q/%d/%d, want streaming/1/1", to, ntxn, nbud)
	}
	rows, err := s.Match(ctx, map[string]any{"category": "streaming"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("renamed rows = %d, %v", len(rows), err)
	}
	// 'other' is reserved on every axis.
	if _, _, _, err := s.UpdateCategory(ctx, "other", "misc"); err == nil {
		t.Fatal("renaming 'other' was accepted")
	}
	if _, _, _, err := s.UpdateCategory(ctx, "streaming", "other"); err == nil {
		t.Fatal("renaming to 'other' was accepted")
	}
	if _, _, _, err := s.DeleteCategory(ctx, "other", "", false); err == nil {
		t.Fatal("deleting 'other' was accepted")
	}
	// Dry run validates reassign_to too, so the preview catches the typo
	// instead of the live call.
	if _, _, _, err := s.DeleteCategory(ctx, "streaming", "typo-target", true); err == nil {
		t.Fatal("dry run with typo'd reassign_to was accepted")
	}
	if _, ntxn, nbud, err := s.DeleteCategory(ctx, "streaming", "", true); err != nil || ntxn != 1 || nbud != 1 {
		t.Fatalf("dry run = %d/%d/%v, want 1/1 refs reported", ntxn, nbud, err)
	}
	if _, _, _, err := s.DeleteCategory(ctx, "streaming", "", false); err == nil {
		t.Fatal("deleting a referenced category was accepted")
	}
	movedCat, movedTxn, movedBud, err := s.DeleteCategory(ctx, "streaming", "fun", false)
	if err != nil || movedCat != "streaming" {
		t.Fatalf("reassign delete: %v %q", err, movedCat)
	}
	// The echoed counts are the moved rows, not zeros — the operator sees
	// that something moved.
	if movedTxn != 1 || movedBud != 1 {
		t.Fatalf("reassign delete moved = %d/%d, want 1/1", movedTxn, movedBud)
	}
	rows, err = s.Match(ctx, map[string]any{"category": "fun"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("reassigned rows = %d, %v", len(rows), err)
	}
	lim, ok, err := s.resolveLimit(ctx, "fun", "2026-09")
	if err != nil || !ok || lim != 500000 {
		t.Fatalf("reassigned budget resolves %d/%v/%v, want 500000/true/nil", lim, ok, err)
	}
	// An unused category deletes cleanly.
	if _, _, _, err := s.DeleteCategory(ctx, "streaming", "", true); err == nil {
		t.Fatal("deleting an already-moved category was accepted")
	}
	// An unused category deletes outright even with a typo'd reassign_to —
	// nothing needs moving, so the target is never validated.
	if _, err := s.CreateCategory(ctx, "spare"); err != nil {
		t.Fatalf("create spare: %v", err)
	}
	if _, _, _, err := s.DeleteCategory(ctx, "spare", "typo-target", false); err != nil {
		t.Fatalf("unused delete with typo'd reassign_to rejected: %v", err)
	}
}

// #563: transfer volume counts each transfer once per returned set —
// filtering to one leg must not halve the moved money.
func TestAccountFlowTransferVolumeFiltered(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "Bank", "bank", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 100000, "transfer", "other", "Cash", "Bank", "", "", ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		accts []string
	}{
		{"one leg", []string{"Cash"}},
		{"both legs", []string{"Cash", "Bank"}},
		{"no filter", nil},
	} {
		res, err := s.AccountFlow(ctx, "2026-09-01", "2026-09-30", nil, tc.accts, "closing", "desc")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if res.TotalTransferVolumePaise != 100000 {
			t.Errorf("%s: volume = %d, want 100000", tc.name, res.TotalTransferVolumePaise)
		}
	}
}

// #564: the categories filter narrows flow columns only — opening and
// closing stay the account's real balances.
func TestAccountFlowCategoryFilterKeepsRealBalances(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 50000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-02-01", 1000, "expense", "groceries", "HDFC", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-02-02", 2000, "expense", "rent", "HDFC", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-03-01", 500, "expense", "groceries", "HDFC", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-03-02", 5000, "expense", "rent", "HDFC", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	res, err := s.AccountFlow(ctx, "2026-03-01", "2026-03-31", []string{"groceries"}, nil, "closing", "desc")
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]AccountFlowRow{}
	for _, row := range res.Rows {
		byName[row.Name] = row
	}
	r, ok := byName["HDFC"]
	if !ok {
		t.Fatalf("HDFC missing from rows: %+v", res.Rows)
	}
	// Real balances: seed 50000 − 1000 − 2000 = 47000 opening;
	// 47000 − 500 − 5000 = 41500 closing.
	if r.OpeningPaise != 47000 {
		t.Errorf("opening = %d, want 47000 (seed + ALL pre legs)", r.OpeningPaise)
	}
	if r.ClosingPaise != 41500 {
		t.Errorf("closing = %d, want 41500 (real balance)", r.ClosingPaise)
	}
	// Flow columns hold only the groceries slice.
	if r.ExpensePaise != 500 || r.OutflowsPaise != 500 {
		t.Errorf("flows = expense %d outflows %d, want 500/500", r.ExpensePaise, r.OutflowsPaise)
	}
}

// #565: effective_now reflects the row in force today, independent of the
// month filter — a superseded row must not flag just because the filter cuts
// off its successor.
func TestListBudgetsEffectiveNowIgnoresMonthFilter(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateBudget(ctx, "groceries", "2026-01", 100000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateBudget(ctx, "groceries", "2026-03", 200000); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListBudgets(ctx, "", "2026-01", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("filtered rows = %d, want 1", len(rows))
	}
	if rows[0].EffectiveNow {
		t.Error("January row flags effective_now while March (outside the filter) governs today")
	}
	rows, err = s.ListBudgets(ctx, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	flagged := 0
	for _, r := range rows {
		if r.EffectiveNow {
			flagged++
			if r.Month != "2026-03" {
				t.Errorf("flagged month = %s, want 2026-03", r.Month)
			}
		}
	}
	if flagged != 1 {
		t.Errorf("flagged rows = %d, want exactly 1", flagged)
	}
}

// #566: category shares sum to exactly 100.00 per window, even on even
// splits that independent rounding drifts to 99.99.
func TestCompareCategoriesSharesSumTo100(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, cat := range []string{"groceries", "bills", "rent"} {
		if _, _, err := s.Insert(ctx, "2026-03-01", 10000, "expense", cat, "Cash", "", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	deltas, err := s.CompareCategories(ctx, "2026-03-01", "2026-03-31", "2026-03-01", "2026-03-31", nil, nil, "delta", "desc")
	if err != nil {
		t.Fatal(err)
	}
	var aSum, bSum float64
	for _, d := range deltas {
		aSum += d.ASharePct
		bSum += d.BSharePct
	}
	// Largest remainder is exact in hundredths; float addition is not, so
	// compare rounded hundredths instead of raw =.
	if math.Round(aSum*100) != 10000 || math.Round(bSum*100) != 10000 {
		t.Errorf("shares sum = %v/%v, want exactly 100.00/100.00", aSum, bSum)
	}
	// Largest remainder is deterministic: names sort before sharing, so the
	// extra 0.01 on the even split always lands on bills (alphabetically
	// first), not on whichever category the map order served first.
	byName := map[string]CategoryDelta{}
	for _, d := range deltas {
		byName[d.Category] = d
	}
	if byName["bills"].ASharePct != 33.34 || byName["groceries"].ASharePct != 33.33 || byName["rent"].ASharePct != 33.33 {
		t.Errorf("shares = bills %v groceries %v rent %v, want 33.34/33.33/33.33",
			byName["bills"].ASharePct, byName["groceries"].ASharePct, byName["rent"].ASharePct)
	}
}

// #567: zero-spend buckets are normal, not anomalies — a below-median flag on
// an empty day is noise, since the ratio test against 0 is always true.
func TestBucketAnomaliesSkipsZeroSpendBuckets(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Sixteen days at 50000 (median 50000), one low day at 5000, rest zeros
	// in a 30-day window.
	for d := 1; d <= 16; d++ {
		if _, _, err := s.Insert(ctx, "2026-03-"+two(d), 50000, "expense", "groceries", "Cash", "", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.Insert(ctx, "2026-03-17", 5000, "expense", "groceries", "Cash", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	out, _, _, insufficient, err := s.BucketAnomalies(ctx, "2026-03-01", "2026-03-30", "day", nil, nil, "medium", 10000, "delta", "desc")
	if err != nil {
		t.Fatal(err)
	}
	if insufficient {
		t.Fatal("30-day window reported insufficient baseline")
	}
	for _, o := range out {
		if o.ExpensePaise == 0 {
			t.Errorf("zero-spend bucket %s flagged %s — quiet days are not anomalies", o.Key, o.Direction)
		}
	}
	found := false
	for _, o := range out {
		if o.Key == "2026-03-17" && o.Direction == "below" && o.DeltaPaise == -45000 {
			found = true
		}
	}
	if !found {
		t.Errorf("real low-spend day missing from outliers: %+v", out)
	}
}

func two(d int) string {
	if d < 10 {
		return "0" + string(rune('0'+d))
	}
	return "1" + string(rune('0'+d-10))
}
