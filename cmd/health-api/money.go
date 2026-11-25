package main

// Money routes: the app-facing read surface over the money ledger (#571).
// The agent writes through the miser-money MCP server; the app reads here.
// Read-only in v1: no log/edit/delete/budget writes — those ride the MCP
// contract (and the audit story) and are out of scope.

import (
	"agento/internal/money"
	"agento/internal/validate"
	"context"
	"net/http"
	"strconv"
	"strings"
	syncpkg "sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// registerMoney wires this domain's routes. One function per file so main
// stays a list of domains, not a list of paths (like registerHealth).
func registerMoney(mux *http.ServeMux) {
	mux.HandleFunc("/api/money/balances", moneyBalances)
	mux.HandleFunc("/api/money/transactions", moneyTransactions)
}

// moneyStore opens the shared money collections (the same rows the agent
// reads and writes over the miser-money MCP). Cached process-wide like the
// other stores; failures are not cached, so a bad env recovers without a
// restart.
var (
	moneyStoreMu syncpkg.Mutex
	moneyCache   *money.Store
)

func moneyStore(w http.ResponseWriter) (*money.Store, bool) {
	moneyStoreMu.Lock()
	defer moneyStoreMu.Unlock()
	if moneyCache != nil {
		return moneyCache, true
	}
	store, err := money.FromEnv()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, bson.M{"detail": "MONGODB_URI not set"})
		return nil, false
	}
	moneyCache = store
	return moneyCache, true
}

// moneyBalances serves the app's Money section: per-account balance_paise
// rows (derived, archived included) plus total_paise over active accounts —
// the MCP get_balances semantics, so the two surfaces cannot disagree.
func moneyBalances(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "method not allowed"})
		return
	}
	store, ok := moneyStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	out, err := store.Balances(ctx)
	if err != nil {
		writeMoneyErr(w, err)
		return
	}
	// Canonical `id` like the other app surfaces.
	if rows, ok := out["accounts"].([]bson.M); ok {
		for _, r := range rows {
			if id, ok := r["_id"].(string); ok {
				r["id"] = id
				delete(r, "_id")
			}
		}
	}
	out["ok"] = true
	writeJSON(w, http.StatusOK, out)
}

// moneyTransactions pages the ledger read-only: start/end required
// (YYYY-MM-DD inclusive), limit default 100 clamped to 500, offset default
// 0, optional account (name or id hex, either side of a transfer like the
// MCP). Rows carry amount_paise integers; same shapes as MCP query rows.
func moneyTransactions(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "method not allowed"})
		return
	}
	q := r.URL.Query()
	start, end := strings.TrimSpace(q.Get("start")), strings.TrimSpace(q.Get("end"))
	if start == "" || end == "" {
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "start and end (YYYY-MM-DD) are required"})
		return
	}
	if start > end {
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "start is after end"})
		return
	}
	limit := 100
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "limit must be a positive integer"})
			return
		}
		limit = min(n, 500)
	}
	offset := 0
	if raw := strings.TrimSpace(q.Get("offset")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "offset must be a non-negative integer"})
			return
		}
		offset = n
	}
	var acct *string
	if a := strings.TrimSpace(q.Get("account")); a != "" {
		acct = &a
	}
	// Calendar-validated before any store touch, so a non-date fails the
	// same way with or without a database behind it.
	if _, err := validate.CheckDay(start); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "start must be YYYY-MM-DD"})
		return
	}
	if _, err := validate.CheckDay(end); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "end must be YYYY-MM-DD"})
		return
	}
	store, ok := moneyStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rows, total, err := store.Query(ctx, start, end, nil, nil, acct, limit, offset)
	if err != nil {
		writeMoneyErr(w, err)
		return
	}
	if rows == nil {
		rows = []bson.M{}
	}
	// Canonical `id` like the other app surfaces (#433): the app never
	// sees a raw `_id`.
	for _, r := range rows {
		if id, ok := r["_id"].(string); ok {
			r["id"] = id
			delete(r, "_id")
		}
	}
	truncated := int64(offset+len(rows)) < total
	var next any
	if truncated {
		next = offset + len(rows)
	}
	writeJSON(w, http.StatusOK, bson.M{
		"ok": true, "start": start, "end": end, "limit": limit,
		"count": len(rows), "total": total,
		"truncated": truncated, "next_offset": next,
		"transactions": rows,
	})
}

func writeMoneyErr(w http.ResponseWriter, err error) {
	if _, ok := err.(*money.StoreError); ok {
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusInternalServerError, bson.M{"detail": "internal error"})
}
