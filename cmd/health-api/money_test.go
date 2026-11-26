package main

// DB-free paths of the money read routes (#571): auth, method dispatch and
// query validation fail before any store is opened, so these hold without
// MONGODB_URI. Reads themselves go through the shared money store.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"agento/internal/money"
	"agento/internal/mongo"
)

func moneyReq(t *testing.T, method, target string) *httptest.ResponseRecorder {
	t.Helper()
	t.Setenv("PASSWORD", "test-secret-12345678")
	r := httptest.NewRequest(method, target, nil)
	r.Header.Set("Authorization", "Bearer test-secret-12345678")
	w := httptest.NewRecorder()
	if strings.HasPrefix(r.URL.Path, "/api/money/transactions") {
		moneyTransactions(w, r)
	} else {
		moneyBalances(w, r)
	}
	return w
}

func TestMoneyRequiresAuth(t *testing.T) {
	t.Setenv("PASSWORD", "test-secret-12345678")
	for _, target := range []string{"/api/money/balances", "/api/money/transactions?start=2026-01-01&end=2026-01-31"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		w := httptest.NewRecorder()
		if strings.HasPrefix(target, "/api/money/transactions") {
			moneyTransactions(w, req)
		} else {
			moneyBalances(w, req)
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("GET %s: got %d, want 401", target, w.Code)
		}
	}
}

func TestMoneyRejectsMethod(t *testing.T) {
	w := moneyReq(t, http.MethodPost, "/api/money/balances")
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("got %d, want 405", w.Code)
	}
}

func TestMoneyTransactionsRequiresBounds(t *testing.T) {
	for _, target := range []string{
		"/api/money/transactions",
		"/api/money/transactions?start=2026-01-01",
		"/api/money/transactions?end=2026-01-31",
		"/api/money/transactions?start=2026-02-30&end=2026-03-31",
		"/api/money/transactions?start=2026-03-31&end=2026-01-01",
		"/api/money/transactions?start=2026-01-01&end=2026-01-31&limit=0",
		"/api/money/transactions?start=2026-01-01&end=2026-01-31&limit=abc",
		"/api/money/transactions?start=2026-01-01&end=2026-01-31&offset=-1",
		"/api/money/transactions?start=2026-01-01&end=2026-01-31&order=sideways",
	} {
		w := moneyReq(t, http.MethodGet, target)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("GET %s: got %d, want 422 (%s)", target, w.Code, w.Body.String())
		}
	}
}

func TestDescWindowAnchorsAtTotal(t *testing.T) {
	for _, tc := range []struct {
		name            string
		total           int64
		offset, limit   int
		wantSkip, wantN int64
	}{
		{"first page", 250, 0, 100, 150, 100},
		{"second page", 250, 100, 100, 50, 100},
		{"last partial", 250, 200, 100, 0, 50},
		{"past end", 250, 300, 100, 0, 0},
		{"short window", 30, 0, 100, 0, 30},
		{"empty window", 0, 0, 100, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			skip, n := descWindow(tc.total, tc.offset, tc.limit)
			if skip != tc.wantSkip || n != tc.wantN {
				t.Errorf("descWindow(%d, %d, %d) = (%d, %d), want (%d, %d)",
					tc.total, tc.offset, tc.limit, skip, n, tc.wantSkip, tc.wantN)
			}
		})
	}
}

// Pages from the newest end against a real store: with 5 rows and limit 2,
// desc pages are [05,04], [03,02], [01] with matching truncated flags.
func TestMoneyTransactionsDescPagesFromNewest(t *testing.T) {
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		t.Skip("MONGODB_URI not set")
	}
	const dbName = "money_api_desc_test"
	t.Setenv("MONGODB_URI", uri)
	t.Setenv("MONGODB_DB", dbName)
	t.Setenv("PASSWORD", "test-secret-12345678")
	moneyStoreMu.Lock()
	moneyCache = nil
	moneyStoreMu.Unlock()
	defer func() {
		moneyStoreMu.Lock()
		moneyCache = nil
		moneyStoreMu.Unlock()
	}()
	// Fresh database per run: a previous failing run leaves rows behind.
	c, err := mongo.ConnectURI(uri)
	if err != nil {
		t.Skipf("mongo unreachable: %v", err)
	}
	if err := c.Database(dbName).Drop(context.Background()); err != nil {
		t.Fatalf("drop test db: %v", err)
	}
	ms, err := money.New(uri, dbName)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	if _, err := ms.CreateAccount(ctx, "Cash", "cash", 0); err != nil {
		t.Fatal(err)
	}
	for d := 1; d <= 5; d++ {
		day := "2026-09-0" + string(rune('0'+d))
		if _, _, err := ms.Insert(ctx, day, int64(d*1000), "expense", "groceries", "Cash", "", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	get := func(target string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer test-secret-12345678")
		w := httptest.NewRecorder()
		moneyTransactions(w, req)
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", target, err)
		}
		return w.Code, body
	}
	dates := func(body map[string]any) []string {
		t.Helper()
		rows, _ := body["transactions"].([]any)
		out := make([]string, 0, len(rows))
		for _, r := range rows {
			m, _ := r.(map[string]any)
			d, _ := m["date"].(string)
			out = append(out, d)
		}
		return out
	}
	base := "/api/money/transactions?start=2026-09-01&end=2026-09-30&limit=2&order=desc"
	code, b1 := get(base)
	if code != http.StatusOK {
		t.Fatalf("page 1: got %d (%v)", code, b1)
	}
	if got := dates(b1); len(got) != 2 || got[0] != "2026-09-05" || got[1] != "2026-09-04" {
		t.Fatalf("page 1 = %v, want [09-05 09-04]", got)
	}
	if b1["total"] != float64(5) || b1["truncated"] != true || b1["next_offset"] != float64(2) {
		t.Fatalf("page 1 footer = %v (want total 5, truncated, next 2)", b1)
	}
	_, b2 := get(base + "&offset=2")
	if got := dates(b2); len(got) != 2 || got[0] != "2026-09-03" || got[1] != "2026-09-02" {
		t.Fatalf("page 2 = %v, want [09-03 09-02]", got)
	}
	_, b3 := get(base + "&offset=4")
	if got := dates(b3); len(got) != 1 || got[0] != "2026-09-01" {
		t.Fatalf("page 3 = %v, want [09-01]", got)
	}
	if b3["truncated"] != false {
		t.Fatalf("last page still truncated: %v", b3)
	}
}

// Shape before order: start=2026-02-30 sorts after any January end as a
// string, so the range check would report "start is after end" for what is
// really a non-calendar date. The date problem must win.
func TestMoneyTransactionsValidatesShapeBeforeOrder(t *testing.T) {
	w := moneyReq(t, http.MethodGet,
		"/api/money/transactions?start=2026-02-30&end=2026-01-01")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422", w.Code)
	}
	if !strings.Contains(w.Body.String(), "YYYY-MM-DD") {
		t.Fatalf("body %q names the range, not the date", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "after end") {
		t.Fatalf("body %q reports a range error for a non-date", w.Body.String())
	}
}
