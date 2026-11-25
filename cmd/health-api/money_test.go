package main

// DB-free paths of the money read routes (#571): auth, method dispatch and
// query validation fail before any store is opened, so these hold without
// MONGODB_URI. Reads themselves go through the shared money store.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	} {
		w := moneyReq(t, http.MethodGet, target)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("GET %s: got %d, want 422 (%s)", target, w.Code, w.Body.String())
		}
	}
}
