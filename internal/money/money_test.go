package money

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"agento/internal/mongo"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		t.Skip("MONGODB_URI not set")
	}
	// Intentionally ignores MONGODB_DB and uses `miser_test` unconditionally:
	// `go test ./...` runs packages in parallel and this helper calls db.Drop
	// on the whole database — sharing CI's `MONGODB_DB=ci_test` with reports
	// (which reads the env) would drop another suite's rows mid-run. So
	// `MONGODB_DB=miser_test go test ./internal/money` works, but the env var
	// is a no-op for this package; isolation holds because the name is hardcoded.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := mongo.ConnectURI(uri)
	if err != nil {
		t.Skipf("mongo unreachable: %v", err)
	}
	db := c.Database("miser_test")
	_ = db.Drop(ctx)
	s, err := New(uri, "miser_test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	if _, err := s.CreateAccount(ctx, "Cash", "cash", 0); err != nil {
		t.Fatalf("seed Cash: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = db.Drop(ctx)
	})
	_ = bson.M{}
	_ = mongoDrv.ErrNoDocuments
	return s
}

func balances(t *testing.T, s *Store) map[string]float64 {
	t.Helper()
	ctx := context.Background()
	out, err := s.Balances(ctx)
	if err != nil {
		t.Fatalf("Balances: %v", err)
	}
	m := map[string]float64{}
	for _, a := range out["accounts"].([]bson.M) {
		name, _ := a["name"].(string)
		var bal float64
		switch v := a["balance"].(type) {
		case float64:
			bal = v
		case int32:
			bal = float64(v)
		case int64:
			bal = float64(v)
		}
		m[name] = bal
	}
	return m
}

// Operator "today" is IST, not server UTC: a log at 00:30 IST must land on
// the user's date, not UTC's yesterday. IST has no DST, so the fixed zone is
// exact with no tzdata dependency — and these midnight-boundary cases pin
// that down with fixed instants instead of racing time.Now().
func TestTodayInHandlesMidnightBoundaries(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
	}{
		{"2026-01-01T18:29:59Z", "2026-01-01"}, // 23:59:59 IST, still Jan 1
		{"2026-01-01T18:30:00Z", "2026-01-02"}, // 00:00 IST, already Jan 2
		{"2026-06-15T00:00:00Z", "2026-06-15"}, // 05:30 IST, same day
	} {
		in, err := time.Parse(time.RFC3339, tc.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := todayIn(IST, in); got != tc.want {
			t.Errorf("todayIn(IST, %s) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// Smoke test: TodayIST agrees with a UTC+5:30 shift of now. Both sides read
// the clock, so an IST-midnight tick between the two reads is accepted
// (either adjacent date is correct); anything else is a zone bug.
func TestTodayISTMatchesUTCPlus530(t *testing.T) {
	before := time.Now().UTC().Add(5*time.Hour + 30*time.Minute).Format("2006-01-02")
	got := TodayIST()
	after := time.Now().UTC().Add(5*time.Hour + 30*time.Minute).Format("2006-01-02")
	if got != before && got != after {
		t.Fatalf("TodayIST() = %s, want %s (or %s across a midnight tick)", got, before, after)
	}
}

func TestAccountsCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 1000); err != nil {
		t.Fatalf("create: %v", err)
	}
	names := map[string]bool{}
	rows, _ := s.ListAccounts(ctx, false)
	for _, a := range rows {
		names[a["name"].(string)] = true
	}
	if !names["Cash"] || !names["HDFC"] {
		t.Fatalf("got %v", names)
	}
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 0); err == nil {
		t.Fatal("expected duplicate error")
	}
	if _, err := s.CreateAccount(ctx, "X", "spaceship", 0); err == nil {
		t.Fatal("expected bad type error")
	}
	if _, ok, _ := s.ArchiveAccount(ctx, "HDFC"); !ok {
		t.Fatal("archive failed")
	}
	rows, _ = s.ListAccounts(ctx, false)
	for _, a := range rows {
		if a["name"] == "HDFC" {
			t.Fatal("archived visible")
		}
	}
	rows, _ = s.ListAccounts(ctx, true)
	found := false
	for _, a := range rows {
		if a["name"] == "HDFC" {
			found = true
		}
	}
	if !found {
		t.Fatal("archived missing with flag")
	}
}

func TestInsertBalances(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 1000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 300, "expense", "groceries", "HDFC", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-02", 5000, "income", "salary", "HDFC", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	b := balances(t, s)
	if b["HDFC"] != 1000-300+5000 || b["Cash"] != 0 {
		t.Fatalf("got %v", b)
	}
}

func TestTransferBalances(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 1000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 200, "transfer", "other", "HDFC", "Cash", "", "", ""); err != nil {
		t.Fatal(err)
	}
	b := balances(t, s)
	if b["HDFC"] != 800 || b["Cash"] != 200 {
		t.Fatalf("got %v", b)
	}
}

// A padded id-hex transfer receiver resolves like a padded account: `resolve`
// trims before hex-parse like `findAccount`, so `sending_to="  <hex>  "`
// finds the `_id` instead of looking up a name that equals the hex.
func TestTransferToPaddedIdHex(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 1000); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListAccounts(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var cashID string
	for _, a := range rows {
		if a["name"] == "Cash" {
			cashID, _ = a["_id"].(string)
		}
	}
	if cashID == "" {
		t.Fatal("Cash has no id")
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 200, "transfer", "other", "HDFC", "  "+cashID+"  ", "", "", ""); err != nil {
		t.Fatalf("padded hex sending_to failed: %v", err)
	}
	if b := balances(t, s); b["HDFC"] != 800 || b["Cash"] != 200 {
		t.Fatalf("got %v", b)
	}
}

func TestTransferValidation(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 1000); err != nil {
		t.Fatal(err)
	}
	bad := []struct {
		typ, dst string
		extra    string
	}{
		{"transfer", "HDFC", ""}, // same source/dest
		{"transfer", "", ""},     // no sending_to
		{"transfer", "Nope", ""}, // unknown dest
		{"expense", "Cash", "x"}, // sending_to on non-transfer
	}
	for _, c := range bad {
		st := c.dst
		if c.extra != "" {
			st = "Cash"
		}
		if _, _, err := s.Insert(ctx, "2026-09-01", 100, c.typ, "other", "HDFC", st, "", "", ""); err == nil {
			t.Fatalf("expected error for %+v", c)
		}
	}
	if _, _, err := s.ArchiveAccount(ctx, "Cash"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 100, "transfer", "other", "HDFC", "Cash", "", "", ""); err == nil {
		t.Fatal("expected archived-dest error")
	}
	rows, _, err := s.Query(ctx, "2026-01-01", "2026-12-31", nil, nil, nil, 0, 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("got %v %v", rows, err)
	}
	if b := balances(t, s); b["HDFC"] != 1000 {
		t.Fatalf("got %v", b)
	}
}

func TestBlankAccountRequiresSetup(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.accts.DeleteMany(ctx, bson.M{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 100, "expense", "other", "", "", "", "", ""); err == nil {
		t.Fatal("expected no-accounts error")
	} else if es := err.Error(); len(es) < 10 || !containsStr(es, "no accounts found") {
		t.Fatalf("got %v", err)
	}
	if _, err := s.CreateAccount(ctx, "Cash", "cash", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 100, "expense", "other", "", "", "", "", ""); err == nil {
		t.Fatal("expected account-required error")
	} else if !containsStr(err.Error(), "account is required") {
		t.Fatalf("got %v", err)
	}
	rows, _, err := s.Query(ctx, "2026-01-01", "2026-12-31", nil, nil, nil, 0, 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("got %v %v", rows, err)
	}
}

func TestDeleteInvertsBalances(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 1000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 200, "transfer", "other", "HDFC", "Cash", "", "", ""); err != nil {
		t.Fatal(err)
	}
	n, err := s.Delete(ctx, map[string]any{"category": "other"})
	if err != nil || n != 1 {
		t.Fatalf("got %d %v", n, err)
	}
	if b := balances(t, s); b["HDFC"] != 1000 || b["Cash"] != 0 {
		t.Fatalf("got %v", b)
	}
}

func TestEditTransactionAmountAdjustsBalance(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tid, _, err := s.Insert(ctx, "2026-09-01", 300, "expense", "groceries", "Cash", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	amt := 350.0
	changed, err := s.EditTransaction(ctx, tid, &amt, nil, nil, nil, nil, nil, nil)
	if err != nil || !changed {
		t.Fatalf("got changed=%v err=%v", changed, err)
	}
	if b := balances(t, s); b["Cash"] != -350 {
		t.Fatalf("got %v", b)
	}
	// Same-value edit is a no-op success: no error, changed=false.
	changed, err = s.EditTransaction(ctx, tid, &amt, nil, nil, nil, nil, nil, nil)
	if err != nil || changed {
		t.Fatalf("got changed=%v err=%v", changed, err)
	}
}

func TestEditTransactionMovesTransferLegs(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAccount(ctx, "Savings", "bank", 0); err != nil {
		t.Fatal(err)
	}
	tid, _, err := s.Insert(ctx, "2026-09-01", 200, "transfer", "other", "HDFC", "Cash", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	dst, amt := "Savings", 300.0
	changed, err := s.EditTransaction(ctx, tid, &amt, nil, nil, nil, &dst, nil, nil)
	if err != nil || !changed {
		t.Fatalf("got changed=%v err=%v", changed, err)
	}
	if b := balances(t, s); b["HDFC"] != 700 || b["Cash"] != 0 || b["Savings"] != 300 {
		t.Fatalf("got %v", b)
	}
}

func TestEditTransactionRejectsBadEdits(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tid, _, err := s.Insert(ctx, "2026-09-01", 100, "expense", "groceries", "Cash", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	badAmt := 0.005
	if _, err := s.EditTransaction(ctx, tid, &badAmt, nil, nil, nil, nil, nil, nil); err == nil {
		t.Fatal("inexact-paise edit was accepted")
	}
	zero := 0.0
	if _, err := s.EditTransaction(ctx, tid, &zero, nil, nil, nil, nil, nil, nil); err == nil {
		t.Fatal("zero-amount edit was accepted")
	}
	toTransfer := "transfer"
	if _, err := s.EditTransaction(ctx, tid, nil, &toTransfer, nil, nil, nil, nil, nil); err == nil {
		t.Fatal("type change to transfer without sending_to was accepted")
	}
	dst := "Cash"
	if _, err := s.EditTransaction(ctx, tid, nil, nil, nil, nil, &dst, nil, nil); err == nil {
		t.Fatal("sending_to on a non-transfer was accepted")
	}
	bogus := "nope"
	if _, err := s.EditTransaction(ctx, tid, nil, nil, &bogus, nil, nil, nil, nil); err == nil {
		t.Fatal("unknown category edit was accepted")
	}
	if _, err := s.EditTransaction(ctx, "68fakeid0000000000000000", nil, nil, nil, nil, nil, nil, nil); err == nil {
		t.Fatal("unknown id edit was accepted")
	}
	// Row untouched by the rejected edits.
	if b := balances(t, s); b["Cash"] != -100 {
		t.Fatalf("got %v", b)
	}
}

// Resupplying the leg a row already sits on is not a move: it must work even
// when that account is archived, or the read-then-edit flow (copy `account`
// from a query back into the edit) breaks on exactly the rows the docs say
// stay editable. Moving the leg TO an archived account still rejects.
func TestEditTransactionKeepsArchivedLegWhenUnmoved(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tid, _, err := s.Insert(ctx, "2026-09-01", 100, "expense", "groceries", "Cash", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ArchiveAccount(ctx, "Cash"); err != nil {
		t.Fatal(err)
	}
	same := "Cash"
	note := "still archived"
	changed, err := s.EditTransaction(ctx, tid, nil, nil, nil, &same, nil, nil, &note)
	if err != nil {
		t.Fatalf("resupplying the archived leg rejected: %v", err)
	}
	if !changed {
		t.Fatal("note change on an archived leg reported no change")
	}
	if b := balances(t, s); b["Cash"] != -100 {
		t.Fatalf("got %v — editing around an archived leg moved the balance", b)
	}
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ArchiveAccount(ctx, "HDFC"); err != nil {
		t.Fatal(err)
	}
	other := "HDFC"
	if _, err := s.EditTransaction(ctx, tid, nil, nil, nil, &other, nil, nil, nil); err == nil {
		t.Fatal("moving a leg onto an archived account was accepted")
	}
}

// Converting AWAY from a transfer drops the destination leg; resupplying
// the same destination is the read-then-edit flow copying both legs back,
// not a contradiction, so it is ignored rather than rejected. A different
// destination on the way out, or converting INTO a transfer with none, is
// still rejected.
func TestEditTransactionConvertAwayFromTransfer(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAccount(ctx, "Savings", "bank", 0); err != nil {
		t.Fatal(err)
	}
	tid, _, err := s.Insert(ctx, "2026-09-01", 200, "transfer", "other", "HDFC", "Savings", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	toExpense := "expense"
	sameDst := "Savings"
	changed, err := s.EditTransaction(ctx, tid, nil, &toExpense, nil, nil, &sameDst, nil, nil)
	if err != nil {
		t.Fatalf("converting away while resupplying the old destination rejected: %v", err)
	}
	if !changed {
		t.Fatal("conversion reported no change")
	}
	if b := balances(t, s); b["HDFC"] != 800 || b["Savings"] != 0 {
		t.Fatalf("got %v — the transfer legs did not collapse into the expense leg", b)
	}
	rows, err := s.Match(ctx, map[string]any{"id": tid})
	if err != nil || len(rows) != 1 {
		t.Fatalf("re-read: %v %v", rows, err)
	}
	if _, ok := rows[0]["sending_to"]; ok {
		t.Fatalf("destination leg survived the conversion: %v", rows[0])
	}

	// A different destination on the way out is still a contradiction.
	tid2, _, err := s.Insert(ctx, "2026-09-02", 50, "transfer", "other", "HDFC", "Savings", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	otherDst := "Cash"
	if _, err := s.EditTransaction(ctx, tid2, nil, &toExpense, nil, nil, &otherDst, nil, nil); err == nil {
		t.Fatal("converting away with a different destination was accepted")
	}
	// Converting INTO a transfer with no destination is still rejected.
	toTransfer := "transfer"
	if _, err := s.EditTransaction(ctx, tid, nil, &toTransfer, nil, nil, nil, nil, nil); err == nil {
		t.Fatal("converting into a transfer without sending_to was accepted")
	}
}

func TestConcurrentInserts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 0); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func() {
			_, _, err := s.Insert(ctx, "2026-09-01", 10, "income", "salary", "HDFC", "", "", "", "")
			done <- err
		}()
	}
	for i := 0; i < 20; i++ {
		if err := <-done; err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	if b := balances(t, s); b["HDFC"] != 200 {
		t.Fatalf("got %v", b)
	}
}

func TestReconciliationAfterPrune(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 1000); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2020-01-01", 100, "expense", "other", "HDFC", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if b := balances(t, s); b["HDFC"] != 900 {
		t.Fatalf("got %v", b)
	}
	n, err := s.Prune(ctx, 90, false)
	if err != nil || n != 1 {
		t.Fatalf("got %d %v", n, err)
	}
	if b := balances(t, s); b["HDFC"] != 900 {
		t.Fatalf("got %v", b)
	}
	rows, _, err := s.Query(ctx, "2020-01-01", "2020-12-31", nil, nil, nil, 0, 0)
	if err != nil || len(rows) != 0 {
		t.Fatalf("got %v %v", rows, err)
	}
}

func TestUnknownCategoryIsRejected(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, _, err := s.Insert(ctx, "2026-09-01", 10, "expense", "utilities", "Cash", "", "", "", ""); err == nil {
		t.Fatal("an unknown category was accepted; it used to be refiled under 'other'")
	} else if !containsStr(err.Error(), "does not exist") {
		t.Fatalf("got %v", err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 10, "expense", "", "Cash", "", "", "", ""); err != nil {
		t.Fatalf("blank category should still default to other: %v", err)
	}
}

func TestIdempotencyKeyMakesARetryANoOp(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id1, created1, err := s.Insert(ctx, "2026-09-01", 300, "expense", "groceries", "Cash", "", "", "", "retry-1")
	if err != nil || !created1 {
		t.Fatalf("first: created=%v err=%v", created1, err)
	}
	id2, created2, err := s.Insert(ctx, "2026-09-01", 300, "expense", "groceries", "Cash", "", "", "", "retry-1")
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if created2 {
		t.Fatal("the retry wrote a second row")
	}
	if id1 != id2 {
		t.Fatalf("retry returned %s, want the original %s", id2, id1)
	}
	if b := balances(t, s); b["Cash"] != -300 {
		t.Fatalf("balance %v — the retry moved it", b)
	}
	rows, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("got %d rows, %v", len(rows), err)
	}
}

// An overlong key is rejected, never truncated: truncating to the cap makes a
// 201-byte key byte-identical to the 200-byte key that is its own prefix, so
// the unique index sees a duplicate and a genuinely different request is handed
// the first one's row as a "replay".
func TestOverlongIdempotencyKeyIsRejectedNotTruncated(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	atLimit := strings.Repeat("k", IdempotencyKeyMax)
	if _, _, err := s.Insert(ctx, "2026-09-01", 10, "expense", "other", "Cash", "", "", "", atLimit); err != nil {
		t.Fatalf("a key of exactly the limit must be accepted: %v", err)
	}
	over := atLimit + "X"
	if _, _, err := s.Insert(ctx, "2026-09-01", 99, "expense", "other", "Cash", "", "", "", over); err == nil {
		t.Fatal("an overlong key was accepted; it would collide with its own prefix")
	} else if !containsStr(err.Error(), "shorter key") {
		t.Fatalf("the error does not say how to fix it: %v", err)
	}
	rows, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("got %d rows, %v — the overlong request wrote", len(rows), err)
	}
}

func TestUnarchiveRestoresWrites(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, ok, err := s.ArchiveAccount(ctx, "Cash"); err != nil || !ok {
		t.Fatalf("archive: %v %v", ok, err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 10, "expense", "other", "Cash", "", "", "", ""); err == nil {
		t.Fatal("an archived account accepted a write")
	}
	if _, ok, err := s.UnarchiveAccount(ctx, "Cash"); err != nil || !ok {
		t.Fatalf("unarchive: %v %v", ok, err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 10, "expense", "other", "Cash", "", "", "", ""); err != nil {
		t.Fatalf("unarchived account still refuses writes: %v", err)
	}
}

// Every other account path trims; archive/unarchive must too, or the same
// name succeeds in one tool and reports "unknown account" in another. They
// return the resolved name, never the raw input.
func TestArchiveUnarchiveTrimNamesLikeWrites(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if name, ok, err := s.ArchiveAccount(ctx, " Cash "); err != nil || !ok {
		t.Fatalf("padded archive missed: %v %v", ok, err)
	} else if name != "Cash" {
		t.Fatalf("archive echoed %q, want the stored name %q", name, "Cash")
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 10, "expense", "other", "Cash", "", "", "", ""); err == nil {
		t.Fatal("a padded-archived account still accepts writes")
	}
	if _, ok, err := s.UnarchiveAccount(ctx, "  Cash  "); err != nil || !ok {
		t.Fatalf("padded unarchive missed: %v %v", ok, err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 10, "expense", "other", " Cash ", "", "", "", ""); err != nil {
		t.Fatalf("unarchived account still refuses padded writes: %v", err)
	}
}

func TestDeleteByIDLeavesSiblings(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id1, _, err := s.Insert(ctx, "2026-09-01", 300, "expense", "groceries", "Cash", "", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 300, "expense", "groceries", "Cash", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	n, err := s.Delete(ctx, map[string]any{"id": id1})
	if err != nil || n != 1 {
		t.Fatalf("deleted %d, %v", n, err)
	}
	rows, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("got %d rows, %v — the sibling went too", len(rows), err)
	}
	if b := balances(t, s); b["Cash"] != -300 {
		t.Fatalf("balance %v", b)
	}
}

func TestMatchIsADryRun(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, _, err := s.Insert(ctx, "2026-09-01", 100, "expense", "groceries", "Cash", "", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.Match(ctx, map[string]any{"category": "groceries"})
	if err != nil || len(rows) != 2 {
		t.Fatalf("matched %d, %v", len(rows), err)
	}
	if rows[0]["account"] != "Cash" {
		t.Fatalf("row has no account name: %v", rows[0])
	}
	after, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 0, 0)
	if err != nil || len(after) != 2 {
		t.Fatalf("Match deleted rows: %d, %v", len(after), err)
	}
}

// A rupee-float delete filter converts to paise: Match on 10.1 finds the row
// stored as 1010. Inexact floats and strings fail instead of silently
// matching nothing.
func TestMatchFloatAmountConvertsToPaise(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, _, err := s.Insert(ctx, "2026-09-01", 10.1, "expense", "groceries", "Cash", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Match(ctx, map[string]any{"amount": 10.1})
	if err != nil || len(rows) != 1 {
		t.Fatalf("float-amount match got %d rows, %v", len(rows), err)
	}
	if _, err := s.Match(ctx, map[string]any{"amount": 10.555}); err == nil {
		t.Fatal("inexact-paise amount matched instead of failing")
	}
	if _, err := s.Match(ctx, map[string]any{"amount": "ten"}); err == nil {
		t.Fatal("string amount matched instead of failing")
	}
}

// Opening balances validate like every other write: 0.005 is rejected,
// not silently stored as 1 paise. Zero and negatives still pass exactness.
func TestCreateAccountRejectsInexactPaise(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "Dust", "cash", 0.005); err == nil {
		t.Fatal("0.005 opening balance was rounded instead of rejected")
	} else if !containsStr(err.Error(), "exact paise") {
		t.Fatalf("error %q does not demand exact paise", err)
	}
	if _, err := s.CreateAccount(ctx, "Dust", "cash", 10.55); err != nil {
		t.Fatalf("exact-paise 10.55 rejected: %v", err)
	}
	rows, err := s.ListAccounts(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r["name"] == "Dust" && fnum(r["balance"]) != 10.55 {
			t.Fatalf("balance %v, want exactly 10.55", r["balance"])
		}
	}
}

func TestQueryNamesItsAccounts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 200, "transfer", "other", "HDFC", "Cash", "", "", ""); err != nil {
		t.Fatal(err)
	}
	rows, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("got %d rows, %v", len(rows), err)
	}
	if rows[0]["account"] != "HDFC" || rows[0]["sending_to_account"] != "Cash" {
		t.Fatalf("names missing from the row: %v", rows[0])
	}
	accts, err := s.ListAccounts(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range accts {
		if id, _ := a["_id"].(string); id == "" {
			t.Fatalf("account %v has no id", a["name"])
		}
	}
}

// Query must return hex `_id`s so the documented `delete by id` workflow is
// reachable from a query: without this, `delete_transactions id=<queried id>`
// cannot be built from `query_transactions` output.
func TestQueryReturnsIdsForDeleteById(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, _, err := s.Insert(ctx, "2026-09-01", 300, "expense", "groceries", "Cash", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	rows, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("got %d rows, %v", len(rows), err)
	}
	id, _ := rows[0]["_id"].(string)
	if id == "" {
		t.Fatalf("query row has no _id: %v", rows[0])
	}
	n, err := s.Delete(ctx, map[string]any{"id": id})
	if err != nil || n != 1 {
		t.Fatalf("delete by queried id %q: deleted %d, %v", id, n, err)
	}
}

// Writes lowercase category/type; reads must normalize the same way, or
// `query category=Groceries` silently matches nothing logged as `groceries`.
func TestFiltersAreCaseInsensitiveLikeWrites(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, _, err := s.Insert(ctx, "2026-09-01", 100, "expense", "groceries", "Cash", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	upperType, upperCat := "Expense", "Groceries"
	rows, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", &upperType, &upperCat, nil, 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("case-insensitive query got %d rows, %v", len(rows), err)
	}
	matched, err := s.Match(ctx, map[string]any{"category": "Groceries", "type": "Expense"})
	if err != nil || len(matched) != 1 {
		t.Fatalf("case-insensitive match got %d rows, %v", len(matched), err)
	}
	n, err := s.Delete(ctx, map[string]any{"category": "GROCERIES"})
	if err != nil || n != 1 {
		t.Fatalf("case-insensitive delete got %d, %v", n, err)
	}
}

// The same idempotency key with a different payload must fail, not return the
// old row as a false "replay".
func TestIdempotencyKeyRejectsDifferentPayload(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, _, err := s.Insert(ctx, "2026-09-01", 300, "expense", "groceries", "Cash", "", "", "", "same-key"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name                string
		date                string
		amount              float64
		typ, cat, acct, dst string
	}{
		{"different amount", "2026-09-01", 999, "expense", "groceries", "Cash", ""},
		{"different date", "2026-09-02", 300, "expense", "groceries", "Cash", ""},
		{"different category", "2026-09-01", 300, "expense", "bills", "Cash", ""},
		{"different type", "2026-09-01", 300, "income", "groceries", "Cash", ""},
	}
	for _, c := range cases {
		if _, _, err := s.Insert(ctx, c.date, c.amount, c.typ, c.cat, c.acct, c.dst, "", "", "same-key"); err == nil {
			t.Fatalf("%s was accepted under a reused key; expected a mismatch rejection", c.name)
		} else if !containsStr(err.Error(), "idempotency_key") {
			t.Fatalf("%s error %q does not name the key", c.name, err)
		}
	}
	rows, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("reused key wrote extra rows: %d, %v", len(rows), err)
	}
	if b := balances(t, s); b["Cash"] != -300 {
		t.Fatalf("balance %v — the mismatched retry moved it", b)
	}
}

// Notes cap at 300 bytes on a UTF-8 boundary, never mid-rune: a naive s[:300]
// can split a multi-byte character and store invalid bytes.
func TestLongNoteTruncatesOnUTF8Boundary(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	long := strings.Repeat("é", 200) // 400 bytes, 200 runes
	if _, _, err := s.Insert(ctx, "2026-09-01", 10, "expense", "other", "Cash", "", long, "", ""); err != nil {
		t.Fatal(err)
	}
	rows, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("got %d rows, %v", len(rows), err)
	}
	note, _ := rows[0]["note"].(string)
	if len(note) > 300 {
		t.Fatalf("note is %d bytes, over the 300-byte cap", len(note))
	}
	if !validString(note) {
		t.Fatal("truncated note is not valid UTF-8: cut mid-rune")
	}
}

// A non-calendar date must fail like the read path rejects it: storing
// `2026-02-30` used to succeed while query/summarize rejected it, and the bad
// row's expiresAt fell back to year-1 (immediate TTL purge, balance surviving).
func TestInsertRejectsNonCalendarDate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, _, err := s.Insert(ctx, "2026-02-30", 10, "expense", "other", "Cash", "", "", "", ""); err == nil {
		t.Fatal("non-calendar date was stored; query/summarize reject it")
	} else if !containsStr(err.Error(), "real calendar date") {
		t.Fatalf("error %q does not explain the calendar problem", err)
	}
	if _, _, err := s.Insert(ctx, " 2026-09-01 ", 10, "expense", "other", "Cash", "", "", "", ""); err != nil {
		t.Fatalf("padded date should trim like reads: %v", err)
	}
	rows, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("padded write not stored trimmed: %d rows, %v", len(rows), err)
	}
	if fmt.Sprint(rows[0]["date"]) != "2026-09-01" {
		t.Fatalf("stored date %q is not trimmed", rows[0]["date"])
	}
}

// Account types normalize like transaction types: `Bank` stores `bank`
// instead of rejecting what `log_transaction type="Bank"` accepts.
func TestCreateAccountNormalizesTypeLikeTransactions(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "Padded", " Bank ", 0); err != nil {
		t.Fatalf("padded/cased type rejected: %v", err)
	}
	rows, err := s.ListAccounts(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range rows {
		if a["name"] == "Padded" && a["type"] != "bank" {
			t.Fatalf("stored type %q, want bank", a["type"])
		}
	}
}

// The account filter matches EITHER side of a transfer by design ($or on
// accountId/sending_to): document it and pin it, so a delete-by-account of
// the receiver does not surprise.
func TestAccountFilterMatchesEitherSideOfTransfer(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 200, "transfer", "other", "HDFC", "Cash", "", "", ""); err != nil {
		t.Fatal(err)
	}
	receiver := "Cash"
	rows, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, &receiver, 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("receiver-side query got %d rows, %v", len(rows), err)
	}
	matched, err := s.Match(ctx, map[string]any{"account": "Cash"})
	if err != nil || len(matched) != 1 {
		t.Fatalf("receiver-side match got %d rows, %v", len(matched), err)
	}
}

// The delete date filter trims like writes/reads: a padded filter finds what
// `log_transaction` stored trimmed instead of reporting `would_delete: 0`.
func TestDeleteDateFilterTrimsLikeWrites(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, _, err := s.Insert(ctx, "2026-09-01", 10, "expense", "other", "Cash", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Match(ctx, map[string]any{"date": " 2026-09-01 "})
	if err != nil || len(rows) != 1 {
		t.Fatalf("padded delete filter matched %d rows, %v", len(rows), err)
	}
}

// Paise storage is exact: three 10-paise writes total exactly 30 paise.
// With float64 storage the balance compared as -0.30000000000000004 != -0.3.
func TestPaiseArithmeticIsExact(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, _, err := s.Insert(ctx, "2026-09-01", 0.10, "expense", "groceries", "Cash", "", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if b := balances(t, s); b["Cash"] != -0.3 {
		t.Fatalf("balance %v, want exactly -0.3", b)
	}
	rows, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 0, 0)
	if err != nil || len(rows) != 3 {
		t.Fatalf("got %d rows, %v", len(rows), err)
	}
	for _, r := range rows {
		if fnum(r["amount"]) != 0.1 {
			t.Fatalf("amount %v, want exactly 0.1", r["amount"])
		}
	}
}

func TestFractionalRupeesRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, _, err := s.Insert(ctx, "2026-09-01", 10.55, "expense", "groceries", "Cash", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	rows, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("got %d rows, %v", len(rows), err)
	}
	if fnum(rows[0]["amount"]) != 10.55 {
		t.Fatalf("amount %v, want exactly 10.55", rows[0]["amount"])
	}
	if b := balances(t, s); b["Cash"] != -10.55 {
		t.Fatalf("balance %v, want exactly -10.55", b)
	}
}

func TestSubPaiseAmountsAreRejected(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, _, err := s.Insert(ctx, "2026-09-01", 0.001, "expense", "other", "Cash", "", "", "", ""); err == nil {
		t.Fatal("a sub-paise amount was stored; it has no paise representation")
	} else if !containsStr(err.Error(), "paise") {
		t.Fatalf("error %q does not name the paise floor", err)
	}
	// >2-decimal inputs are rejected, never silently rounded (0.005 must not
	// become a 1-paise row, 10.555 must not become 1056 paise).
	if _, _, err := s.Insert(ctx, "2026-09-01", 0.005, "expense", "other", "Cash", "", "", "", ""); err == nil {
		t.Fatal("0.005 was rounded instead of rejected")
	} else if !containsStr(err.Error(), "exact paise") {
		t.Fatalf("error %q does not demand exact paise", err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 10.555, "expense", "other", "Cash", "", "", "", ""); err == nil {
		t.Fatal("10.555 was rounded instead of rejected")
	}
	// Exact 2-decimal inputs pass, including floats no binary can represent
	// exactly (10.55 stores 1055 paise and reads back equal).
	if _, _, err := s.Insert(ctx, "2026-09-01", 10.55, "expense", "groceries", "Cash", "", "", "", ""); err != nil {
		t.Fatalf("exact-paise 10.55 rejected: %v", err)
	}
}

// Legacy float-rupee docs migrate to int64 paise on EnsureSchema, idempotently:
// the filter matches doubles and decimals only (ints are already paise),
// so reruns and fresh databases find nothing.
func TestLegacyFloatDocsMigrateToPaise(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.accts.InsertOne(ctx, bson.M{"name": "Legacy", "type": "cash",
		"balance": 100.5, "archived": false,
		"createdAt": primitive.NewDateTimeFromTime(time.Now().UTC())}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.txns.InsertOne(ctx, bson.M{"date": "2026-09-01", "amount": 10.1,
		"type": "expense", "category": "groceries", "note": "", "source": "mcp",
		"accountId": primitive.NewObjectID(),
		"createdAt": primitive.NewDateTimeFromTime(time.Now().UTC()),
		"expiresAt": expiry("2026-09-01", RetentionDays)}); err != nil {
		t.Fatal(err)
	}
	// A stray Decimal128 converts exactly like a double, not left behind.
	dec, err := primitive.ParseDecimal128("3.02")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.txns.InsertOne(ctx, bson.M{"date": "2026-09-02", "amount": dec,
		"type": "expense", "category": "bills", "note": "", "source": "mcp",
		"accountId": primitive.NewObjectID(),
		"createdAt": primitive.NewDateTimeFromTime(time.Now().UTC()),
		"expiresAt": expiry("2026-09-02", RetentionDays)}); err != nil {
		t.Fatal(err)
	}
	// Legacy sub-paise dust (allowed by the old amount<=0 check) floors to
	// the 1-paise minimum instead of becoming a 0 row the new code calls
	// invalid and the tool cannot delete by amount.
	if _, err := s.txns.InsertOne(ctx, bson.M{"date": "2026-09-03", "amount": 0.004,
		"type": "expense", "category": "groceries", "note": "", "source": "mcp",
		"accountId": primitive.NewObjectID(),
		"createdAt": primitive.NewDateTimeFromTime(time.Now().UTC()),
		"expiresAt": expiry("2026-09-03", RetentionDays)}); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("EnsureSchema: %v", err)
	}
	var a bson.M
	if err := s.accts.FindOne(ctx, bson.M{"name": "Legacy"}).Decode(&a); err != nil {
		t.Fatal(err)
	}
	if bal, ok := a["balance"].(int64); !ok || bal != 10050 {
		t.Fatalf("balance migrated to %v (%T), want int64 10050", a["balance"], a["balance"])
	}
	var txn bson.M
	if err := s.txns.FindOne(ctx, bson.M{"date": "2026-09-01"}).Decode(&txn); err != nil {
		t.Fatal(err)
	}
	if amt, ok := txn["amount"].(int64); !ok || amt != 1010 {
		t.Fatalf("amount migrated to %v (%T), want int64 1010", txn["amount"], txn["amount"])
	}
	var decTxn bson.M
	if err := s.txns.FindOne(ctx, bson.M{"date": "2026-09-02"}).Decode(&decTxn); err != nil {
		t.Fatal(err)
	}
	if amt, ok := decTxn["amount"].(int64); !ok || amt != 302 {
		t.Fatalf("decimal amount migrated to %v (%T), want int64 302", decTxn["amount"], decTxn["amount"])
	}
	var dustTxn bson.M
	if err := s.txns.FindOne(ctx, bson.M{"date": "2026-09-03"}).Decode(&dustTxn); err != nil {
		t.Fatal(err)
	}
	if amt, ok := dustTxn["amount"].(int64); !ok || amt != 1 {
		t.Fatalf("sub-paise amount migrated to %v (%T), want int64 1 (floored, never 0)", dustTxn["amount"], dustTxn["amount"])
	}
	// No zero-amount rows exist post-migration, so the tool's 0-rejection
	// ("no zero rows exist") holds on migrated DBs too.
	n, err := s.txns.CountDocuments(ctx, bson.M{"amount": 0})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("migration left %d zero-amount rows, which the tool cannot delete by amount", n)
	}
	// The floored row reads back as 0.01 rupees and is matchable by its
	// paise value.
	matched, err := s.Match(ctx, map[string]any{"amount": int64(1)})
	if err != nil || len(matched) != 1 {
		t.Fatalf("paise-1 match got %d rows, %v", len(matched), err)
	}
	if fnum(matched[0]["amount"]) != 0.01 {
		t.Fatalf("floored row reads back as %v, want 0.01", matched[0]["amount"])
	}
	// Idempotent: a second run changes nothing and errors nothing.
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatalf("second EnsureSchema: %v", err)
	}
	// New writes compose with migrated balances exactly.
	if _, _, err := s.Insert(ctx, "2026-09-02", 0.5, "expense", "other", "Legacy", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListAccounts(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r["name"] == "Legacy" && fnum(r["balance"]) != 100.0 {
			t.Fatalf("balance %v, want exactly 100.0", r["balance"])
		}
	}
}

// Pagination slices by date order with a stable _id tiebreak, and reports
// the unpaged total so a short page is never mistaken for the end.
// limit==0 means no cap (offset still applies); negative limits are rejected
// like negative offsets.
func TestQueryPaginationSlicesAndTotals(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 1; i <= 5; i++ {
		day := fmt.Sprintf("2026-09-%02d", i)
		if _, _, err := s.Insert(ctx, day, float64(i*100), "expense", "groceries", "Cash", "", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	rows, total, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 2, 0)
	if err != nil || len(rows) != 2 || total != 5 {
		t.Fatalf("page 1: got %d rows total %d, %v", len(rows), total, err)
	}
	if fmt.Sprint(rows[0]["date"]) != "2026-09-01" || fmt.Sprint(rows[1]["date"]) != "2026-09-02" {
		t.Fatalf("page 1 order wrong: %v, %v", rows[0]["date"], rows[1]["date"])
	}
	rows, total, err = s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 2, 2)
	if err != nil || len(rows) != 2 || total != 5 {
		t.Fatalf("page 2: got %d rows total %d, %v", len(rows), total, err)
	}
	if fmt.Sprint(rows[0]["date"]) != "2026-09-03" || fmt.Sprint(rows[1]["date"]) != "2026-09-04" {
		t.Fatalf("page 2 order wrong: %v, %v", rows[0]["date"], rows[1]["date"])
	}
	rows, total, err = s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 2, 4)
	if err != nil || len(rows) != 1 || total != 5 {
		t.Fatalf("page 3: got %d rows total %d, %v", len(rows), total, err)
	}
	if fmt.Sprint(rows[0]["date"]) != "2026-09-05" {
		t.Fatalf("page 3 row %v, want 2026-09-05", rows[0]["date"])
	}
	rows, total, err = s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 2, 5)
	if err != nil || len(rows) != 0 || total != 5 {
		t.Fatalf("past-end page: got %d rows total %d, %v", len(rows), total, err)
	}
	rows, total, err = s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 0, 0)
	if err != nil || len(rows) != 5 || total != 5 {
		t.Fatalf("unlimited: got %d rows total %d, %v", len(rows), total, err)
	}
	// Offset applies without a limit: limit 0 means no cap, not restart.
	rows, total, err = s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 0, 2)
	if err != nil || len(rows) != 3 || total != 5 {
		t.Fatalf("uncapped offset page: got %d rows total %d, %v", len(rows), total, err)
	}
	if fmt.Sprint(rows[0]["date"]) != "2026-09-03" || fmt.Sprint(rows[1]["date"]) != "2026-09-04" || fmt.Sprint(rows[2]["date"]) != "2026-09-05" {
		t.Fatalf("uncapped offset order wrong: %v, %v, %v", rows[0]["date"], rows[1]["date"], rows[2]["date"])
	}
	// Negative offsets fail here too, not just in the handler.
	if _, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 2, -1); err == nil {
		t.Fatal("negative store offset was accepted; expected a rejection")
	}
	// A negative limit fails instead of silently dumping everything: only
	// an explicit 0 means no cap.
	if _, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, -1, 0); err == nil {
		t.Fatal("negative store limit was accepted as unlimited; expected a rejection")
	} else if !containsStr(err.Error(), "limit") {
		t.Fatalf("error %q does not name the limit", err)
	}
}

// Same-day rows come back in insertion (_id) order: the date sort ties, so
// without the _id tiebreak their order would be unspecified. Distinct amounts
// mark insertion order unambiguously. Fetched across page boundaries (not one
// fetch) so a date-only sort is less likely to pass by luck on Mongo's
// natural order (which on a fresh test collection often matches insertion
// order — paging reduces, not eliminates, that luck).
func TestQuerySameDayRowsFollowInsertionOrder(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, amt := range []float64{100, 200, 300} {
		if _, _, err := s.Insert(ctx, "2026-09-15", amt, "expense", "groceries", "Cash", "", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	// Page across the same-day set: 2 + 1, asserting amounts per page so a
	// date-only sort that returns same-day rows in any other order fails.
	rows, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 2, 0)
	if err != nil || len(rows) != 2 {
		t.Fatalf("same-day page 1: got %d rows, %v", len(rows), err)
	}
	for i, want := range []float64{100, 200} {
		if fnum(rows[i]["amount"]) != want {
			t.Fatalf("page 1 row %d amount %v, want %v (insertion order)", i, rows[i]["amount"], want)
		}
	}
	rows2, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 2, 2)
	if err != nil || len(rows2) != 1 {
		t.Fatalf("same-day page 2: got %d rows, %v", len(rows2), err)
	}
	if fnum(rows2[0]["amount"]) != 300 {
		t.Fatalf("page 2 amount %v, want 300 (insertion order)", rows2[0]["amount"])
	}
	rows = append(rows, rows2...)
	for i, want := range []float64{100, 200, 300} {
		if fnum(rows[i]["amount"]) != want {
			t.Fatalf("row %d amount %v, want %v (insertion order)", i, rows[i]["amount"], want)
		}
	}
	prev := ""
	for i, r := range rows {
		id, _ := r["_id"].(string)
		if id == "" || (prev != "" && id <= prev) {
			t.Fatalf("row %d id %q not strictly increasing: tiebreak broken", i, id)
		}
		prev = id
	}
}

// An account literally named with 24 hex chars resolves by name (like
// findAccount's $or), not just by `_id`: the old if/else preferred `_id` and
// made hex-shaped names unreachable via `log`.
func TestHexShapedAccountNameResolvesByName(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	hexName := "5f1d7f3a2b3c4d5e6f7a8b9c"
	if _, err := s.CreateAccount(ctx, hexName, "cash", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 10, "expense", "other", hexName, "", "", "", ""); err != nil {
		t.Fatalf("hex-shaped account name failed via log path: %v", err)
	}
	rows, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, &hexName, 0, 0)
	if err != nil || len(rows) != 1 {
		t.Fatalf("query by hex-shaped name got %d rows, %v", len(rows), err)
	}
}

// An `_id` from `list_accounts` round-trips through archive/unarchive: they
// accept id hex like every other account path, not name-only.
func TestArchiveAcceptsIdHexLikeOtherPaths(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.CreateAccount(ctx, "HDFC", "bank", 0); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListAccounts(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for _, a := range rows {
		if a["name"] == "HDFC" {
			id, _ = a["_id"].(string)
		}
	}
	if id == "" {
		t.Fatal("HDFC has no id to round-trip")
	}
	if _, ok, err := s.ArchiveAccount(ctx, id); err != nil || !ok {
		t.Fatalf("archive by id missed: %v %v", ok, err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 10, "expense", "other", "HDFC", "", "", "", ""); err == nil {
		t.Fatal("id-archived account still accepts writes")
	}
	if _, ok, err := s.UnarchiveAccount(ctx, id); err != nil || !ok {
		t.Fatalf("unarchive by id missed: %v %v", ok, err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 10, "expense", "other", "HDFC", "", "", "", ""); err != nil {
		t.Fatalf("id-unarchived account still refuses writes: %v", err)
	}
}

// Unknown type/category filters are rejected like writes (and like unknown
// accounts), instead of silently returning zero rows for a typo.
func TestUnknownFilterValuesAreRejectedLikeWrites(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	badType, badCat := "banana", "utilities"
	if _, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", &badType, nil, nil, 0, 0); err == nil {
		t.Fatal("unknown query type matched nothing instead of failing")
	} else if !containsStr(err.Error(), "does not exist") {
		t.Fatalf("query type error %q does not name the problem", err)
	}
	if _, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, &badCat, nil, 0, 0); err == nil {
		t.Fatal("unknown query category matched nothing instead of failing")
	}
	if _, err := s.Match(ctx, map[string]any{"category": "utilities"}); err == nil {
		t.Fatal("unknown match category matched nothing instead of failing")
	}
}

// `type` is a first-class delete filter like query: deleting by type narrows
// to that type instead of being unreachable.
func TestDeleteByTypeFilter(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, _, err := s.Insert(ctx, "2026-09-01", 100, "expense", "groceries", "Cash", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Insert(ctx, "2026-09-01", 5000, "income", "salary", "Cash", "", "", "", ""); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Match(ctx, map[string]any{"type": "Expense"})
	if err != nil || len(rows) != 1 {
		t.Fatalf("case-insensitive type match got %d rows, %v", len(rows), err)
	}
	n, err := s.Delete(ctx, map[string]any{"type": "expense"})
	if err != nil || n != 1 {
		t.Fatalf("delete by type got %d, %v", n, err)
	}
	after, _, err := s.Query(ctx, "2026-09-01", "2026-09-30", nil, nil, nil, 0, 0)
	if err != nil || len(after) != 1 {
		t.Fatalf("got %d rows after type delete, %v", len(after), err)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func validString(s string) bool { return utf8.ValidString(s) }

func fnum(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case int64:
		return float64(n)
	case int32:
		return float64(n)
	}
	return 0
}
