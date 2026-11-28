package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"agento/internal/money"
	"agento/internal/mongo"

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
//
// withTestStore swaps the package-global store for a seeded throwaway
// database (miser_cmd_test — never the internal package's miser_test, since
// `go test ./...` runs packages in parallel and sharing it would drop
// another suite's rows mid-run). Category existence is data-dependent now,
// so the unknown-category rejections run here instead of against nil.
func withTestStore(t *testing.T) {
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
	db := c.Database("miser_cmd_test")
	_ = db.Drop(ctx)
	s, err := money.New(uri, "miser_cmd_test")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	store = s
	// Every handler test assumes a Cash account exists, like the internal
	// package's testStore seed.
	if _, err := s.CreateAccount(ctx, "Cash", "cash", 0); err != nil {
		t.Fatalf("seed Cash: %v", err)
	}
	t.Cleanup(func() {
		store = nil
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = db.Drop(ctx)
	})
}

func callTool(t *testing.T, name string, args map[string]any) (string, string) {
	t.Helper()
	text, errMsg, _ := callToolFull(t, name, args)
	return text, errMsg
}

// callToolFull is callTool plus the protocol-level IsError flag, for the
// failure-contract test: a rejection must set IsError, a success must not.
func callToolFull(t *testing.T, name string, args map[string]any) (string, string, bool) {
	t.Helper()
	res, err := doCall(t, name, args)
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
	return text, out.Error, res.IsError
}

func doCall(t *testing.T, name string, args map[string]any) (*mcp.CallToolResult, error) {
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

	return session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
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

// Sort is two enums, not one string: sort_by names the field (date|amount),
// sort the direction (asc|desc); both default (date, asc) and both are echoed.
// Unknown values are rejected before any store touch (`store` is nil here),
// like the other range-mode validations.
func TestQueryTransactionsRejectsUnknownSort(t *testing.T) {
	// The schema enum rejects before the handler: a protocol-level failure
	// (IsError) whose text names the field — no structured payload, so like
	// the fractional-paise rejection this asserts on text, not errMsg.
	text, _, isErr := callToolFull(t, "query_transactions", map[string]any{
		"start": "2026-01-01", "end": "2026-01-31", "sort_by": "colour",
	})
	if !isErr {
		t.Fatal("an unknown sort_by was accepted; expected the enum rejection")
	}
	if !strings.Contains(text, "sort_by") {
		t.Errorf("rejection %q does not name the field", text)
	}
	// A value the schema enum allows but the store forbids is impossible by
	// construction (both read money.TxSortFields), so the second layer is
	// pinned at the store level in TestQuerySorted instead.
	text, _, isErr = callToolFull(t, "query_transactions", map[string]any{
		"start": "2026-01-01", "end": "2026-01-31", "sort": "sideways",
	})
	if !isErr {
		t.Fatal("an unknown sort was accepted; expected the enum rejection")
	}
	if !strings.Contains(text, "sort") {
		t.Errorf("rejection %q does not name the field", text)
	}
}

// Like limit/offset, order args are filters on a range, not on a row: a
// non-blank sort with id is rejected rather than silently dropped (`store` is
// nil here).
func TestQueryByIdRejectsSortArgs(t *testing.T) {
	for _, extra := range []map[string]any{
		{"sort_by": "amount"},
		{"sort": "desc"},
	} {
		args := map[string]any{"id": "000000000000000000000001"}
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

// The applied order is echoed so the caller never guesses: defaults read
// back as date/asc, and an explicit pair orders + echoes.
func TestQueryTransactionsSortEchoAndOrder(t *testing.T) {
	withTestStore(t)
	ctx := context.Background()
	for _, row := range []struct {
		date  string
		paise int64
	}{{"2026-03-01", 50000}, {"2026-03-02", 10000}, {"2026-03-03", 30000}} {
		if _, _, err := store.Insert(ctx, row.date, row.paise, "expense", "groceries", "Cash", "", "", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	text, errMsg := callTool(t, "query_transactions", map[string]any{
		"start": "2026-03-01", "end": "2026-03-31",
	})
	if errMsg != "" {
		t.Fatalf("default query failed: %s", errMsg)
	}
	for _, want := range []string{`"sort_by":"date"`, `"sort":"asc"`, `"2026-03-01"`, `"2026-03-03"`} {
		if !strings.Contains(text, want) {
			t.Errorf("default query text misses %s: %q", want, text)
		}
	}
	text, errMsg = callTool(t, "query_transactions", map[string]any{
		"start": "2026-03-01", "end": "2026-03-31", "sort_by": "amount", "sort": "desc",
	})
	if errMsg != "" {
		t.Fatalf("amount-desc query failed: %s", errMsg)
	}
	for _, want := range []string{`"sort_by":"amount"`, `"sort":"desc"`} {
		if !strings.Contains(text, want) {
			t.Errorf("amount-desc query text misses %s: %q", want, text)
		}
	}
	first, third := strings.Index(text, "50000"), strings.Index(text, "10000")
	if first < 0 || third < 0 || first > third {
		t.Errorf("amount-desc did not order 50000 before 10000: %q", text)
	}
}

// An unknown account/category filter on a read is the empty page, not a
// rejection (#619): no row can carry the name, so count 0 is the truth.
// Closed sets (type) and by-id lookups still fail.
func TestQueryTransactionsUnknownFilterEmptiesOut(t *testing.T) {
	withTestStore(t)
	for _, args := range []map[string]any{
		{"start": "2026-03-01", "end": "2026-03-31", "category": "grocerys"},
		{"start": "2026-03-01", "end": "2026-03-31", "account": "no-such-account"},
	} {
		text, errMsg := callTool(t, "query_transactions", args)
		if errMsg != "" {
			t.Fatalf("unknown filter %v was rejected: %s", args, errMsg)
		}
		if !strings.Contains(text, `"count":0`) {
			t.Errorf("unknown filter %v did not return the empty page: %q", args, text)
		}
	}
	_, errMsg := callTool(t, "query_transactions", map[string]any{
		"start": "2026-03-01", "end": "2026-03-31", "type": "banana",
	})
	if errMsg == "" || !strings.Contains(errMsg, "does not exist") {
		t.Errorf("unknown type did not fail closed: %q", errMsg)
	}
	_, errMsg = callTool(t, "delete_transactions", map[string]any{
		"category": "grocerys", "dry_run": true,
	})
	if errMsg != "" {
		t.Fatalf("unknown delete category was rejected: %s", errMsg)
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
		args := map[string]any{"id": "000000000000000000000001"}
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

// A whitespace-only id counts as absent and falls through to range-mode
// bounds validation — it does not produce an id error. Rejected before any
// store touch (`store` is nil here).
func TestQueryBlankIdFallsThroughToRange(t *testing.T) {
	_, errMsg := callTool(t, "query_transactions", map[string]any{
		"id": "   ", "start": "2026-03-31", "end": "2026-01-01",
	})
	if errMsg == "" {
		t.Fatal("a blank id with a reversed range was accepted; expected the range rejection")
	}
	if !strings.Contains(errMsg, "after end") {
		t.Errorf("error %q is the range error, proving the blank id fell through", errMsg)
	}
	_, errMsg = callTool(t, "query_transactions", map[string]any{"id": "   "})
	if errMsg == "" {
		t.Fatal("a lone blank id was accepted; expected the missing-bounds rejection")
	}
}

// Paging args are filters on a range, not on a row: a non-zero limit or
// offset with id is rejected rather than silently dropped, before any
// store touch (`store` is nil here). Zero values are the unset defaults
// and pass through to the by-id lookup.
func TestQueryByIdRejectsPagingArgs(t *testing.T) {
	for _, args := range []map[string]any{
		{"id": "000000000000000000000001", "limit": 10},
		{"id": "000000000000000000000001", "offset": 5},
		{"id": "000000000000000000000001", "offset": -1},
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
// handler surfaces a real error. Categories are user-managed, so existence is
// data-dependent: this runs against a seeded test store (whose defaults lack
// "utilities"), unlike the nil-store validations above.
func TestLogTransactionRejectsUnknownCategory(t *testing.T) {
	withTestStore(t)
	_, errMsg := callTool(t, "log_transaction", map[string]any{
		"type": "expense", "amount_paise": 10000, "account": "Cash", "category": "utilities",
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
		"type": "expense", "amount_paise": 10000, "account": "Cash", "date": "2026-02-30",
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
		"amount_paise": 0, "category": "groceries", "dry_run": true,
	})
	if errMsg == "" {
		t.Fatal("amount_paise=0 was accepted; 0 means absent and cannot narrow a delete")
	}
	if !strings.Contains(errMsg, "> 0") {
		t.Errorf("error %q does not state the positive-amount rule", errMsg)
	}
}

// amount_paise is an integer: a fractional value cannot even be expressed.
// The SDK's own schema validation rejects it before the handler runs (the
// message names the wanted type), so a stale rupee-float caller can never
// silently log truncated paise.
func TestDeleteTransactionsRejectsFractionalPaise(t *testing.T) {
	text, _ := callTool(t, "delete_transactions", map[string]any{
		"amount_paise": 10.555, "dry_run": true,
	})
	if !strings.Contains(text, "integer") {
		t.Fatalf("amount_paise=10.555 was not rejected as non-integer: %q", text)
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

// A failed tool call is a protocol-level failure: IsError must be set, so a
// client keying off the protocol sees the rejection instead of a success with
// prose to parse. A valid call must not set it.
func TestFailuresSetMCPIsError(t *testing.T) {
	_, _, isErr := callToolFull(t, "query_transactions", map[string]any{
		"start": "", "end": "2026-03-31",
	})
	if !isErr {
		t.Fatal("a rejected call did not set IsError")
	}
	withTestStore(t)
	_, _, isErr = callToolFull(t, "get_balances", map[string]any{})
	if isErr {
		t.Fatal("a successful call set IsError")
	}
}

// edit_account validates before any store touch: blank name and an empty edit
// are rejected with nil store.
func TestEditAccountRejectsEmptyEdits(t *testing.T) {
	_, errMsg := callTool(t, "edit_account", map[string]any{"name": "   "})
	if errMsg == "" {
		t.Fatal("a blank name was accepted; expected the name requirement")
	}
	_, errMsg = callTool(t, "edit_account", map[string]any{"name": "Cash"})
	if errMsg == "" {
		t.Fatal("an empty edit was accepted; expected the one-field rule")
	}
	if !strings.Contains(errMsg, "at least one") {
		t.Errorf("error %q does not state the one-field rule", errMsg)
	}
}

// Category writes validate names before any store touch: blank names are
// rejected with nil store.
func TestCategoryToolsRejectBlankNames(t *testing.T) {
	_, errMsg := callTool(t, "create_category", map[string]any{"name": "   "})
	if errMsg == "" {
		t.Fatal("a blank category name was accepted")
	}
	_, errMsg = callTool(t, "update_category", map[string]any{"name": "groceries", "new_name": "  "})
	if errMsg == "" {
		t.Fatal("a blank new_name was accepted")
	}
	_, errMsg = callTool(t, "delete_category", map[string]any{"name": "  "})
	if errMsg == "" {
		t.Fatal("a blank delete name was accepted")
	}
}

// Null means no change; "" clears the note. End to end through a test store:
// log with a note, clear it, read the row back.
func TestEditTransactionNoteNullVsEmpty(t *testing.T) {
	withTestStore(t)
	text, errMsg := callTool(t, "log_transaction", map[string]any{
		"type": "expense", "amount_paise": 10000, "account": "Cash", "note": "lunch",
	})
	if errMsg != "" {
		t.Fatalf("log: %s", errMsg)
	}
	var logged struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &logged); err != nil || logged.ID == "" {
		t.Fatalf("log result has no id: %v (%s)", err, text)
	}
	// Omitted note leaves the stored note alone.
	_, errMsg = callTool(t, "edit_transaction", map[string]any{
		"id": logged.ID, "category": "groceries",
	})
	if errMsg != "" {
		t.Fatalf("edit: %s", errMsg)
	}
	rows, err := store.Match(context.Background(), map[string]any{"id": logged.ID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("re-read: %v %v", len(rows), err)
	}
	if rows[0]["note"] != "lunch" {
		t.Fatalf("omitted note changed the stored note: %v", rows[0]["note"])
	}
	// Explicit "" clears it.
	_, errMsg = callTool(t, "edit_transaction", map[string]any{
		"id": logged.ID, "note": "",
	})
	if errMsg != "" {
		t.Fatalf("clear: %s", errMsg)
	}
	rows, err = store.Match(context.Background(), map[string]any{"id": logged.ID})
	if err != nil || len(rows) != 1 {
		t.Fatalf("re-read: %v %v", len(rows), err)
	}
	if n, _ := rows[0]["note"].(string); n != "" {
		t.Fatalf("empty note did not clear the stored note: %q", n)
	}
}

// restore_transactions validates before any store touch: a blank id is
// rejected with nil store.
func TestRestoreTransactionRejectsBlankId(t *testing.T) {
	_, errMsg := callTool(t, "restore_transactions", map[string]any{"id": "   "})
	if errMsg == "" {
		t.Fatal("a blank id was accepted; expected the id requirement")
	}
}

// Unknown ids and live rows are rejected, not silent no-ops — through a
// test store, since existence is data-dependent.
func TestRestoreTransactionRejectsUnknownAndLive(t *testing.T) {
	withTestStore(t)
	_, errMsg := callTool(t, "restore_transactions", map[string]any{"id": "000000000000000000000000"})
	if errMsg == "" {
		t.Fatal("an unknown id was accepted")
	}
	text, errMsg := callTool(t, "log_transaction", map[string]any{
		"type": "expense", "amount_paise": 10000, "account": "Cash",
	})
	if errMsg != "" {
		t.Fatalf("log: %s", errMsg)
	}
	var logged struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &logged); err != nil || logged.ID == "" {
		t.Fatalf("log result has no id: %v (%s)", err, text)
	}
	_, errMsg = callTool(t, "restore_transactions", map[string]any{"id": logged.ID})
	if errMsg == "" {
		t.Fatal("restoring a live row was accepted; expected the not-deleted rejection")
	}
	if !strings.Contains(errMsg, "not deleted") {
		t.Errorf("error %q does not say the row is not deleted", errMsg)
	}
}

// A soft-deleted id is already deleted, not unknown — both dry-run and live
// delete name restore_transactions instead of reading as a typo.
func TestDeleteFlagsDeletedIdAsAlreadyDeleted(t *testing.T) {
	withTestStore(t)
	text, errMsg := callTool(t, "log_transaction", map[string]any{
		"type": "expense", "amount_paise": 10000, "account": "Cash",
	})
	if errMsg != "" {
		t.Fatalf("log: %s", errMsg)
	}
	var logged struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &logged); err != nil || logged.ID == "" {
		t.Fatalf("log result has no id: %v (%s)", err, text)
	}
	if _, err := store.Delete(context.Background(), map[string]any{"id": logged.ID}); err != nil {
		t.Fatalf("flag: %v", err)
	}
	_, errMsg = callTool(t, "delete_transactions", map[string]any{"id": logged.ID, "dry_run": true})
	if errMsg == "" || !strings.Contains(errMsg, "already deleted") {
		t.Fatalf("dry-run re-delete: %q, want the already-deleted error", errMsg)
	}
	_, errMsg = callTool(t, "delete_transactions", map[string]any{"id": logged.ID, "dry_run": false})
	if errMsg == "" || !strings.Contains(errMsg, "already deleted") {
		t.Fatalf("live re-delete: %q, want the already-deleted error", errMsg)
	}
}

// A soft-deleted id reads as already-deleted on by-id query too — not as
// unknown (Match is live-only and would report 0 rows either way).
func TestQueryByIdFlagsDeletedIdAsAlreadyDeleted(t *testing.T) {
	withTestStore(t)
	text, errMsg := callTool(t, "log_transaction", map[string]any{
		"type": "expense", "amount_paise": 10000, "account": "Cash",
	})
	if errMsg != "" {
		t.Fatalf("log: %s", errMsg)
	}
	var logged struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(text), &logged); err != nil || logged.ID == "" {
		t.Fatalf("log result has no id: %v (%s)", err, text)
	}
	if _, err := store.Delete(context.Background(), map[string]any{"id": logged.ID}); err != nil {
		t.Fatalf("flag: %v", err)
	}
	_, errMsg = callTool(t, "query_transactions", map[string]any{"id": logged.ID})
	if errMsg == "" || !strings.Contains(errMsg, "already deleted") {
		t.Fatalf("query by flagged id: %q, want the already-deleted error", errMsg)
	}
}
