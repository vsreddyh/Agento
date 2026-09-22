package money

import (
	"context"
	"os"
	"testing"
	"time"

	"agento/internal/mongo"

	"go.mongodb.org/mongo-driver/bson"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		t.Skip("MONGODB_URI not set")
	}
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

func TestClassifyLog(t *testing.T) {
	r := Classify("spent 300 on groceries")
	if r.Action != "log_expense" || r.Amount == nil || *r.Amount != 300 {
		t.Fatalf("got %+v", r)
	}
	if r.Category == nil || *r.Category != "groceries" {
		t.Fatalf("got %+v", r)
	}
	r = Classify("got 5000 salary")
	if r.Action != "log_income" || r.Amount == nil || *r.Amount != 5000 {
		t.Fatalf("got %+v", r)
	}
}

func TestClassifyQuery(t *testing.T) {
	r := Classify("what did I spend this month?")
	if r.Action != "query" {
		t.Fatalf("got %+v", r)
	}
	if r.Type == nil || *r.Type != "expense" {
		t.Fatalf("got %+v", r)
	}
	r = Classify("how much on eating out in June?")
	if r.Action != "query" {
		t.Fatalf("got %+v", r)
	}
	if len(r.Start) < 7 || r.Start[5:7] != "06" {
		t.Fatalf("got %+v", r)
	}
}

func TestClassifyEdit(t *testing.T) {
	if r := Classify("remove the 300 groceries entry"); r.Action != "delete" {
		t.Fatalf("got %+v", r)
	}
	r := Classify("fix that to 350")
	if r.Action != "fix" || r.Amount == nil || *r.Amount != 350 {
		t.Fatalf("got %+v", r)
	}
}

func TestPeriods(t *testing.T) {
	today := time.Date(2026, 9, 5, 0, 0, 0, 0, time.UTC)
	s, e, _ := ResolvePeriod("this month", today)
	if s != "2026-09-01" || e != "2026-09-30" {
		t.Fatalf("got %s %s", s, e)
	}
	s, e, _ = ResolvePeriod("last month", today)
	if s != "2026-08-01" || e != "2026-08-31" {
		t.Fatalf("got %s %s", s, e)
	}
}

func TestCategories(t *testing.T) {
	if NormalizeCategory("Uber ride") != "transport" {
		t.Fatal("transport")
	}
	if NormalizeCategory("Netflix") != "fun" {
		t.Fatal("fun")
	}
	if NormalizeCategory("blah blah") != "other" {
		t.Fatal("other")
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
	if ok, _ := s.ArchiveAccount(ctx, "HDFC"); !ok {
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
	if _, err := s.Insert(ctx, "2026-09-01", 300, "expense", "groceries", "HDFC", "", "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "2026-09-02", 5000, "income", "salary", "HDFC", "", "", ""); err != nil {
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
	if _, err := s.Insert(ctx, "2026-09-01", 200, "transfer", "other", "HDFC", "Cash", "", ""); err != nil {
		t.Fatal(err)
	}
	b := balances(t, s)
	if b["HDFC"] != 800 || b["Cash"] != 200 {
		t.Fatalf("got %v", b)
	}
	sum, err := s.Summarize(ctx, "2026-09-01", "2026-09-30", nil)
	if err != nil {
		t.Fatal(err)
	}
	if fnum(sum["income"]) != 0 || fnum(sum["expense"]) != 0 || inum(sum["count"]) != 1 {
		t.Fatalf("got %v", sum)
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
		{"transfer", "HDFC", ""},   // same source/dest
		{"transfer", "", ""},       // no sending_to
		{"transfer", "Nope", ""},   // unknown dest
		{"expense", "Cash", "x"},   // sending_to on non-transfer
	}
	for _, c := range bad {
		st := c.dst
		if c.extra != "" {
			st = "Cash"
		}
		if _, err := s.Insert(ctx, "2026-09-01", 100, c.typ, "other", "HDFC", st, "", ""); err == nil {
			t.Fatalf("expected error for %+v", c)
		}
	}
	if _, err := s.ArchiveAccount(ctx, "Cash"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "2026-09-01", 100, "transfer", "other", "HDFC", "Cash", "", ""); err == nil {
		t.Fatal("expected archived-dest error")
	}
	rows, err := s.Query(ctx, "2026-01-01", "2026-12-31", nil, nil, nil)
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
	if _, err := s.Insert(ctx, "2026-09-01", 100, "expense", "other", "", "", "", ""); err == nil {
		t.Fatal("expected no-accounts error")
	} else if es := err.Error(); len(es) < 10 || !containsStr(es, "no accounts found") {
		t.Fatalf("got %v", err)
	}
	if _, err := s.CreateAccount(ctx, "Cash", "cash", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Insert(ctx, "2026-09-01", 100, "expense", "other", "", "", "", ""); err == nil {
		t.Fatal("expected account-required error")
	} else if !containsStr(err.Error(), "account is required") {
		t.Fatalf("got %v", err)
	}
	rows, err := s.Query(ctx, "2026-01-01", "2026-12-31", nil, nil, nil)
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
	if _, err := s.Insert(ctx, "2026-09-01", 200, "transfer", "other", "HDFC", "Cash", "", ""); err != nil {
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

func TestFixLastAdjustsBalances(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.Insert(ctx, "2026-09-01", 300, "expense", "groceries", "Cash", "", "", ""); err != nil {
		t.Fatal(err)
	}
	done, err := s.FixLast(ctx, 350)
	if err != nil || !done {
		t.Fatalf("got %v %v", done, err)
	}
	if b := balances(t, s); b["Cash"] != -350 {
		t.Fatalf("got %v", b)
	}
	sum, err := s.Summarize(ctx, "2026-09-01", "2026-09-30", nil)
	if err != nil || fnum(sum["expense"]) != 350 {
		t.Fatalf("got %v %v", sum, err)
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
			_, err := s.Insert(ctx, "2026-09-01", 10, "income", "salary", "HDFC", "", "", "")
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
	if _, err := s.Insert(ctx, "2020-01-01", 100, "expense", "other", "HDFC", "", "", ""); err != nil {
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
	rows, err := s.Query(ctx, "2020-01-01", "2020-12-31", nil, nil, nil)
	if err != nil || len(rows) != 0 {
		t.Fatalf("got %v %v", rows, err)
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

func inum(v any) int { return int(fnum(v)) }
