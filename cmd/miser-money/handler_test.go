package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests exist because the previous round of tests in this package proved the
// validators behave and said nothing about whether the handlers call them. Deleting a
// handler-side guard once left every unit test green, which is how a validation
// could be present and unwired at the same time.
//
// So the server is built exactly as main builds it — `registerTools` over an in-memory
// transport — and the tools are called through a real MCP client. `store` stays nil: a
// handler that reaches MongoDB before validating panics, and that panic is exactly the
// failure this file is here to catch.
//
// The gap being closed:
//
//   - `query_transactions` with empty bounds built `date >= "" && date <= ""`, matching
//     nothing: the caller saw zero transactions and no reason why.
//   - a lone `start` with no `end` (or the reverse) is rejected instead of being
//     silently ignored: the pair is either complete or absent, never half-applied.
func callTool(t *testing.T, name string, args map[string]any) (string, string) {
	t.Helper()

	// No Mongo here by design (see above): handlers validate before
	// touching the store, and a handler that reaches MongoDB before
	// validating panics — which is exactly the failure this file catches.

	s := mcp.NewServer(&mcp.Implementation{Name: "miser-money-test", Version: "0"}, nil)
	registerTools(s)

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := s.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connecting server: %v", err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connecting client: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}

	// `structured` is this package's structured payload: a map with "ok" and "error".
	raw, mErr := json.Marshal(res.StructuredContent)
	if mErr != nil {
		t.Fatalf("marshalling result: %v", mErr)
	}
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshalling result: %v (raw %s)", err, raw)
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return text, out.Error
}

func TestQueryTransactionsRejectsUnusableBounds(t *testing.T) {
	cases := []struct {
		name       string
		start, end string
	}{
		{"both empty — used to return zero rows silently", "", ""},
		{"start empty", "", "2026-03-31"},
		{"end empty", "2026-01-01", ""},
		{"malformed start", "yesterday", "2026-03-31"},
		{"malformed end", "2026-01-01", "soon"},
		{"impossible calendar date", "2026-02-30", "2026-03-31"},
		// Two valid dates whose range is empty — the store would have returned zero rows
		// as a real answer.
		{"reversed range", "2026-03-31", "2026-01-01"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errMsg := callTool(t, "query_transactions", map[string]any{
				"start": c.start, "end": c.end,
			})
			if errMsg == "" {
				t.Fatalf("query_transactions(%q, %q) was accepted; expected a rejection "+
					"rather than a silently empty result", c.start, c.end)
			}
			if !strings.Contains(errMsg, "query_transactions") {
				t.Errorf("error %q does not name the tool", errMsg)
			}
		})
	}
}

// A negative offset is rejected like a reversed range, before any store
// touch (`store` is nil here): paging backwards is a typo, not page zero.
func TestQueryTransactionsRejectsNegativeOffset(t *testing.T) {
	_, errMsg := callTool(t, "query_transactions", map[string]any{
		"start": "2026-01-01", "end": "2026-01-31", "offset": -1,
	})
	if errMsg == "" {
		t.Fatal("a negative offset was accepted; expected a rejection")
	}
	if !strings.Contains(errMsg, "offset") {
		t.Errorf("error %q does not name the offset", errMsg)
	}
}

// By-id mode: an id addresses one row, so it cannot be combined with any
// other filter (start/end/type/category/account) — same rule as delete by
// id. Non-zero limit/offset are also rejected with id; the zero values are
// the unset defaults and pass through. Rejected before any store touch
// (`store` is nil here).
func TestQueryByIdRejectsCombinedFilters(t *testing.T) {
	for _, extra := range []map[string]any{
		{"start": "2026-01-01", "end": "2026-01-31"},
		{"type": "expense"},
		{"category": "groceries"},
		{"account": "Cash"},
	} {
		args := map[string]any{"id": "68fakeid0000000000000001"}
		for k, v := range extra {
			args[k] = v
		}
		_, errMsg := callTool(t, "query_transactions", args)
		if errMsg == "" {
			t.Fatalf("query by id combined with %v was accepted; expected a rejection", extra)
		}
		if !strings.Contains(errMsg, "cannot be combined") {
			t.Errorf("error %q does not explain the exclusivity", errMsg)
		}
	}
}

// A malformed id fails in toMongoFilter before any store touch
// (`store` is nil here) — never a silent empty page.
func TestQueryByIdRejectsMalformedId(t *testing.T) {
	_, errMsg := callTool(t, "query_transactions", map[string]any{"id": "nope"})
	if errMsg == "" {
		t.Fatal("a malformed id was accepted; expected a rejection")
	}
	if !strings.Contains(errMsg, "transaction id") {
		t.Errorf("error %q does not name the id problem", errMsg)
	}
}

// Paging args are filters on a range, not on a row: a non-zero limit or
// offset with id is rejected rather than silently dropped, before any
// store touch (`store` is nil here). Zero values are the unset defaults
// and pass through to the by-id lookup.
func TestQueryByIdRejectsPagingArgs(t *testing.T) {
	for _, args := range []map[string]any{
		{"id": "68fakeid0000000000000001", "limit": 10},
		{"id": "68fakeid0000000000000001", "offset": 5},
		{"id": "68fakeid0000000000000001", "offset": -1},
	} {
		_, errMsg := callTool(t, "query_transactions", args)
		if errMsg == "" {
			t.Fatalf("query by id with paging %v was accepted; expected a rejection", args)
		}
		if !strings.Contains(errMsg, "cannot be combined") {
			t.Errorf("error %q does not explain the exclusivity", errMsg)
		}
	}
}

// An edit with no fields is rejected like a reversed range, before any
// store touch (`store` is nil here): setting nothing is a no-op the caller
// almost certainly did not mean.
func TestEditTransactionRejectsEmptyEdit(t *testing.T) {
	_, errMsg := callTool(t, "edit_transaction", map[string]any{
		"id": "68fakeid0000000000000000",
	})
	if errMsg == "" {
		t.Fatal("an empty edit was accepted; expected a rejection, not a silent no-op")
	}
	if !strings.Contains(errMsg, "at least one") {
		t.Errorf("error %q does not state the one-field rule", errMsg)
	}
}

// An unknown category used to be refiled under "other" and reported as if it had
// been stored: the caller asked for "utilities", got "other", and only found out
// by reading the row back. The store now rejects it before any write, so the
// handler surfaces a real error. `store` is nil here — the rejection happens
// before the receiver is dereferenced.
func TestLogTransactionRejectsUnknownCategory(t *testing.T) {
	_, errMsg := callTool(t, "log_transaction", map[string]any{
		"type": "expense", "amount": 100, "account": "Cash", "category": "utilities",
	})
	if errMsg == "" {
		t.Fatal("an unknown category was accepted; it used to be silently stored as 'other'")
	}
	if !strings.Contains(errMsg, "does not exist") || !strings.Contains(errMsg, "utilities") {
		t.Errorf("error %q does not name the offending category", errMsg)
	}
}

// A non-calendar date must be rejected like the read path rejects it, before
// any store touch (`store` is nil here): `2026-02-30` matches YYYY-MM-DD but
// is not a real date, so the write is invalid.
func TestLogTransactionRejectsNonCalendarDate(t *testing.T) {
	_, errMsg := callTool(t, "log_transaction", map[string]any{
		"type": "expense", "amount": 100, "account": "Cash", "date": "2026-02-30",
	})
	if errMsg == "" {
		t.Fatal("a non-calendar date was accepted; the query bounds reject it")
	}
	if !strings.Contains(errMsg, "log_transaction") {
		t.Errorf("error %q does not name the tool", errMsg)
	}
}

// The delete date filter is validated like writes/reads, before any store
// touch: a padded or non-calendar date must fail instead of silently matching
// nothing as `would_delete: 0`.
func TestDeleteTransactionsRejectsBadDate(t *testing.T) {
	for _, date := range []string{"2026-02-30", "not-a-date"} {
		_, errMsg := callTool(t, "delete_transactions", map[string]any{
			"date": date, "dry_run": true,
		})
		if errMsg == "" {
			t.Fatalf("delete date %q was accepted; expected a rejection, not a silent zero-match", date)
		}
		if !strings.Contains(errMsg, "delete_transactions") {
			t.Errorf("error %q does not name the tool", errMsg)
		}
	}
}

// `id` cannot be combined with any filter, including the new `type` filter —
// validated before any store touch.
func TestDeleteTransactionsRejectsIdWithType(t *testing.T) {
	_, errMsg := callTool(t, "delete_transactions", map[string]any{
		"id": "68fakeid0000000000000000", "type": "expense", "dry_run": true,
	})
	if errMsg == "" {
		t.Fatal("id+type was accepted; id must be exclusive")
	}
	if !strings.Contains(errMsg, "cannot be combined") {
		t.Errorf("error %q does not explain exclusivity", errMsg)
	}
}

// An explicit 0 amount is rejected like writes, before any store touch: 0
// would otherwise silently widen to every row matching the other filters.
func TestDeleteTransactionsRejectsZeroAmount(t *testing.T) {
	_, errMsg := callTool(t, "delete_transactions", map[string]any{
		"amount": 0.0, "category": "groceries", "dry_run": true,
	})
	if errMsg == "" {
		t.Fatal("amount=0 was accepted; 0 means absent and cannot narrow a delete")
	}
	if !strings.Contains(errMsg, "> 0") {
		t.Errorf("error %q does not state the positive-amount rule", errMsg)
	}
}

// A >2-decimal filter is rejected, not rounded onto paise it never stored
// (`store` is nil here — ExactPaise is pure, so this fails before any touch).
func TestDeleteTransactionsRejectsInexactPaise(t *testing.T) {
	_, errMsg := callTool(t, "delete_transactions", map[string]any{
		"amount": 10.555, "dry_run": true,
	})
	if errMsg == "" {
		t.Fatal("amount=10.555 was accepted; it would match paise it never stored")
	}
	if !strings.Contains(errMsg, "exact paise") {
		t.Errorf("error %q does not demand exact paise", errMsg)
	}
}

// Whitespace-only filters trim to absent like the query bounds: a padded
// account that trims to nothing must not error as "unknown account".
func TestDeleteTransactionsTrimsWhitespaceFilters(t *testing.T) {
	_, errMsg := callTool(t, "delete_transactions", map[string]any{
		"account": "   ", "dry_run": true,
	})
	if errMsg == "" {
		t.Fatal("whitespace-only account was accepted as a filter")
	}
	if !strings.Contains(errMsg, "Provide id") {
		t.Errorf("error %q should be the empty-filter error, not an unknown-account error", errMsg)
	}
}

// Paging math lives in pure helpers with unit tests (no handler wiring):
// absent/negative limits map to the 100 default (clamped to 500), and the
// window reports truncated/next_offset with an exact-multiple page reading
// as done (next_offset present as null, never omitted).
func TestClampLimit(t *testing.T) {
	for _, tc := range []struct {
		in, want int
	}{{-5, 100}, {0, 100}, {1, 1}, {100, 100}, {500, 500}, {501, 500}, {9999, 500}} {
		if got := clampLimit(tc.in); got != tc.want {
			t.Errorf("clampLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestPageWindow(t *testing.T) {
	truncated, next := pageWindow(0, 2, 5)
	if !truncated || next != 2 {
		t.Fatalf("partial page: truncated=%v next=%v, want true/2", truncated, next)
	}
	truncated, next = pageWindow(2, 2, 5)
	if !truncated || next != 4 {
		t.Fatalf("mid page: truncated=%v next=%v, want true/4", truncated, next)
	}
	// Exact multiple: offset+count reaches total exactly — done, not truncated.
	truncated, next = pageWindow(0, 5, 5)
	if truncated || next != nil {
		t.Fatalf("exact-multiple page: truncated=%v next=%v, want false/nil", truncated, next)
	}
	// Past the end: empty page, still done and still null (not missing).
	truncated, next = pageWindow(5, 0, 5)
	if truncated || next != nil {
		t.Fatalf("past-end page: truncated=%v next=%v, want false/nil", truncated, next)
	}
}
