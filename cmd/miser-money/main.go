// Command miser-money is the multi-account money-management MCP server.
//
// Storage: MongoDB (money_accounts + money_transactions). Every
// balance-changing write runs in a multi-document transaction.
// Runs over stdio for MCP clients.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"agento/internal/money"
	"agento/internal/validate"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var store *money.Store

func fail(err error) (*mcp.CallToolResult, map[string]any, error) {
	return nil, map[string]any{"ok": false, "error": err.Error()}, nil
}

func ok(payload map[string]any) (*mcp.CallToolResult, map[string]any, error) {
	payload["ok"] = true
	return nil, payload, nil
}

func result(out map[string]any) (*mcp.CallToolResult, map[string]any, error) {
	b, _ := json.Marshal(out)
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
		StructuredContent: out,
	}, out, nil
}

func today() string { return money.TodayIST() }

// Input types are named rather than inline so `schema_test.go` can infer the schema from
// the same types `AddTool` publishes. Nothing in this file carried `omitempty`, so all
// thirty fields across ten tools were advertised as required — including arguments the
// handler fills in itself (`log_transaction.date`, `create_account.type`,
// `prune_old.days`) and filters the tool descriptions explicitly call optional.
//
// Two kinds of "required" appear below and the difference matters:
//
//   - required because the server rejects the call without it (store validation)
//   - required because absence produces a SILENTLY wrong answer rather than an error —
//     `query_transactions` is the case in point: store.Query does no date validation, so
//     empty start/end builds `date >= "" && date <= ""` and returns nothing at all. That is
//     worse than a rejection, and required is the honest way to say so.
//
// And one tool has no required field at all, because its constraint is "at least one
// of" — `delete_transactions` (at least one filter). It is enforced server-side.
// Expressing that as `anyOf` would be more precise than this schema can be, and
// a constraint the caller cannot see is still better than one it sees wrongly.
type createAccountInput struct {
	// store.CreateAccount rejects an empty name.
	Name string `json:"name"`
	// The handler defaults "" to cash.
	Type string `json:"type,omitempty"`
	// Opening balance; 0 is a real starting balance.
	Balance float64 `json:"balance,omitempty"`
}

type listAccountsInput struct {
	// False by default: archived accounts stay hidden.
	IncludeArchived bool `json:"include_archived,omitempty"`
}

type archiveAccountInput struct {
	// A name or id hex (the hex goes in this same `name` field); trimmed and
	// resolved, so this one cannot be defaulted.
	Name string `json:"name"`
}

// getBalancesInput is empty on purpose: the tool takes no arguments, and naming it keeps
// the schema test able to assert that it stays empty.
type getBalancesInput struct{}

type logTransactionInput struct {
	// store.Insert rejects a non-positive amount and an unknown type.
	Type   string  `json:"type"`
	Amount float64 `json:"amount"`
	// NOT required: the store lowercases an empty category to "other", so an
	// absent category is never rejected. An *unknown* name is rejected — the
	// caller must learn that it does not exist — but marking the field required
	// would reject a call the server accepts (blank means "other").
	Category string `json:"category,omitempty"`
	// "account required" per the tool description.
	Account string `json:"account"`
	// Required only for a transfer, which a schema cannot express conditionally; the
	// store enforces it when type is transfer.
	SendingTo string `json:"sending_to,omitempty"`
	Note      string `json:"note,omitempty"`
	// The handler fills in today() when absent.
	Date string `json:"date,omitempty"`
	// Makes a retried call a no-op: the first write wins and the same id comes
	// back. Optional, so a caller that never retries need not supply one.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

type queryTransactionsInput struct {
	// Required for the reason in the note above: the store does not validate these, so
	// absent bounds return nothing silently rather than failing.
	Start string `json:"start"`
	End   string `json:"end"`
	// "Optional type/category/account filters."
	Type     string `json:"type,omitempty"`
	Category string `json:"category,omitempty"`
	Account  string `json:"account,omitempty"`
	// Page size; <=0 means the 100-row default, clamped to 500. The store
	// speaks limit<=0 as unlimited, but the handler never forwards that —
	// an unbounded MCP read is how 90-day dumps happen.
	Limit int `json:"limit,omitempty"`
	// Page start; negative is rejected like a reversed range.
	Offset int `json:"offset,omitempty"`
}

type deleteTransactionsInput struct {
	// Targets exactly one row. Cannot be combined with the filters below —
	// "the entry I just logged" must not turn into "every row like it".
	ID string `json:"id,omitempty"`
	// At least one filter (or id) is required, and the server rejects none — so no field
	// can be marked required without forbidding a legitimate combination.
	// Amount is a pointer so an explicit 0 is distinguishable from absent:
	// 0 would otherwise silently widen the filter (no zero rows exist).
	Amount   *float64 `json:"amount,omitempty"`
	Type     string   `json:"type,omitempty"`
	Category string   `json:"category,omitempty"`
	Date     string   `json:"date,omitempty"`
	Account  string   `json:"account,omitempty"`
	// A pointer because nil is meaningful: nil means dry run, which is the safe
	// default for anything that deletes.
	DryRun *bool `json:"dry_run,omitempty"`
}

type editTransactionInput struct {
	// A transaction id hex; the row must exist (a missing id is rejected,
	// not a silent 0, like delete by id).
	ID string `json:"id"`
	// Every edit field is optional: absent or whitespace-only means leave it
	// alone (a note cannot be cleared this way — blank means no change).
	// Amount is a pointer so an explicit 0 is rejected, not mistaken for
	// absent; at least one field must change or the call is rejected.
	Amount    *float64 `json:"amount,omitempty"`
	Type      string   `json:"type,omitempty"`
	Category  string   `json:"category,omitempty"`
	Account   string   `json:"account,omitempty"`
	SendingTo string   `json:"sending_to,omitempty"`
	Date      string   `json:"date,omitempty"`
	Note      string   `json:"note,omitempty"`
}

type unarchiveAccountInput struct {
	// A name or id hex like archive_account (the hex goes in `name`).
	Name string `json:"name"`
}

type pruneOldInput struct {
	// Any value <= 0 means the 90-day retention window in handler + store.
	Days int `json:"days,omitempty"`
	// A pointer because nil is meaningful: nil means dry run, which is the safe default
	// for anything that deletes.
	DryRun *bool `json:"dry_run,omitempty"`
}

// normalizeDayRange validates a start/end pair and returns it NORMALIZED, for two
// reasons that each produce a wrong answer rather than an error if skipped:
//
//   - empty or malformed bounds build a filter like `date >= "" && date <= ""`, which
//     matches nothing, so `query_transactions` returns zero transactions with no reason
//     given;
//   - a REVERSED pair is two individually valid dates and still matches nothing, so it
//     needs an explicit comparison. Lexicographic is safe here precisely because both
//     sides have been normalised to YYYY-MM-DD.
//
// It returns the normalized values because `validate.CheckDay` trims whitespace while
// `store.Query` compares raw strings: forwarding `" 2026-01-01 "` would pass validation and
// then match nothing at all.
//
// A schema `required` list cannot do any of this. It is a client-side hint, and a caller
// can send `"start": ""` explicitly and pass validation — so the check lives here, where it
// holds whatever the client believes. Errors are plain `error` values; handlers wrap them
// with `fail`, which is this package's MCP-result helper, not an error constructor.
func normalizeDayRange(what, start, end string) (string, string, error) {
	sd, err := validate.CheckDay(start)
	if err != nil {
		return "", "", fmt.Errorf("%s start: %w", what, err)
	}
	ed, err := validate.CheckDay(end)
	if err != nil {
		return "", "", fmt.Errorf("%s end: %w", what, err)
	}
	// Safe as a string compare: both sides are exactly YYYY-MM-DD by this point.
	if sd > ed {
		return "", "", fmt.Errorf("%s: start %s is after end %s — that range matches nothing",
			what, sd, ed)
	}
	return sd, ed, nil
}

// clampLimit maps the requested query page size to the applied one: absent
// or non-positive means the 100-row default, above 500 clamps to the cap.
// There is no unlimited MCP page — only Query itself accepts limit==0 for
// no cap (direct store use only).
func clampLimit(in int) int {
	if in <= 0 {
		return 100
	}
	if in > 500 {
		return 500
	}
	return in
}

// pageWindow reports whether more rows remain after this page and the offset
// to fetch them with. `next` is nil when done — always present as JSON null,
// never omitted — so a client can distinguish "last page" from a missing
// field. An exact-multiple page (count reaches total exactly) is not
// truncated: there is nothing after it.
func pageWindow(offset, count int, total int64) (truncated bool, next any) {
	if int64(offset+count) < total {
		return true, offset + count
	}
	return false, nil
}

func main() {
	var err error
	store, err = money.FromEnv()
	if err != nil {
		log.Fatalf("miser-money: %v", err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "miser-money", Version: "1.0.0"}, nil)
	registerTools(s)

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "miser-money:", err)
		os.Exit(1)
	}
}

// registerTools attaches every tool to s. It is separate from main so a test can build the
// same server and call a handler without a live MongoDB — which is how the input
// validation below is verified as *wired up*, not merely as a function that behaves.
//
// `store` is the package variable, so a test that only exercises validation can leave it
// nil: a handler that reaches the store before validating panics, and that panic is the
// signal that the guard was removed.
func registerTools(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "create_account",
		Description: "Create a money account (e.g. Cash, HDFC Checking). name unique, trimmed, case-sensitive (Cash and cash are different accounts); type cash|bank|card|wallet|other, case-insensitive (trimmed, so Bank becomes bank; empty or whitespace-only defaults to cash). Optional opening balance in the same call (defaults to 0, must be an exact paise value like every amount, so no separate funding entry is needed)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in createAccountInput) (*mcp.CallToolResult, map[string]any, error) {
			if strings.TrimSpace(in.Type) == "" {
				in.Type = "cash"
			}
			acct, err := store.CreateAccount(ctx, in.Name, in.Type, in.Balance)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "account": acct})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_accounts",
		Description: "List accounts with their current (stored) balances and ids. Hides archived accounts by default — pass include_archived=true to see them. Transaction rows reference accounts by id, so this is how an id maps back to a name."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listAccountsInput) (*mcp.CallToolResult, map[string]any, error) {
			rows, err := store.ListAccounts(ctx, in.IncludeArchived)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "accounts": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "archive_account",
		Description: "Soft-delete an account (history stays queryable; blocked from new log_transaction writes — delete/edit can still move its balance). `name` accepts a name or id hex (trimmed, so an _id from list_accounts round-trips); the stored name is echoed back, never the raw input. The name stays taken."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in archiveAccountInput) (*mcp.CallToolResult, map[string]any, error) {
			// Trimmed up front so the unknown-account error quotes the same
			// value the success path echoes and the store looked up.
			in.Name = strings.TrimSpace(in.Name)
			resolved, done, err := store.ArchiveAccount(ctx, in.Name)
			if err != nil {
				return fail(err)
			}
			if !done {
				return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown account '%s'", in.Name)})
			}
			return result(map[string]any{"ok": true, "archived": resolved})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "unarchive_account",
		Description: "Restore an archived account so it accepts writes again (inverse of archive_account). `name` accepts a name or id hex (trimmed); the stored name is echoed back."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in unarchiveAccountInput) (*mcp.CallToolResult, map[string]any, error) {
			in.Name = strings.TrimSpace(in.Name)
			resolved, done, err := store.UnarchiveAccount(ctx, in.Name)
			if err != nil {
				return fail(err)
			}
			if !done {
				return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown account '%s'", in.Name)})
			}
			return result(map[string]any{"ok": true, "unarchived": resolved})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_balances",
		Description: "Current per-account balances (including archived accounts) plus total across active accounts only — total is NOT the sum of the listed rows once anything is archived, unlike list_accounts which hides archived by default."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in getBalancesInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := store.Balances(ctx)
			if err != nil {
				return fail(err)
			}
			out["ok"] = true
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "log_transaction",
		Description: "Log an income, expense, or transfer (atomic with balance update). account required (case-sensitive exact name or id hex); sending_to required for transfers (name or id hex, like account, and must differ from the source — same-account transfers are rejected; forbidden on non-transfers except whitespace-only, which counts as absent); date YYYY-MM-DD defaults to today in IST (empty or whitespace-only also means today, unlike query's rejected empty bounds; must be a real calendar date — 2026-02-30 is rejected, like query — and padded dates are trimmed; any real date stores with immediate balance effect, even a far-future one (visible only to explicit start/end queries covering it) or a far-past one the TTL purges quickly while the balance stays). category must be one of groceries|eating out|transport|bills|rent|shopping|health|fun|salary|other (blank or whitespace-only = other; anything else is rejected). amount must be an exact paise value (> 0, at most 2 decimals — stored as int64 paise, never rounded or floated). note capped at 300 bytes (cut on a UTF-8 boundary, never rejected). idempotency_key (trimmed, max 200 bytes — a UUID is 36; longer is rejected; blank/whitespace-only means no key, each call logs anew) makes a retry a no-op; the same key with a different date/amount/type/category/account/sending_to is rejected — use a new key for a new request; note differences are ignored on replay (first write wins, correction needs a new key). Returns the stored category/account/note (normalized, not the raw input) plus sending_to/sending_to_account for transfers; if the post-write re-read itself fails, ok stays true (the write succeeded — retrying without a key would double-log) and readback_error names the failure instead of silently omitting fields."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in logTransactionInput) (*mcp.CallToolResult, map[string]any, error) {
			day := in.Date
			if strings.TrimSpace(day) == "" {
				day = today()
			}
			// Validated like the read path, so a non-calendar date never stores:
			// `CheckDay` trims + rejects bad format and impossible dates, and the
			// normalized value is what reaches the store and the echo below.
			var err error
			if day, err = validate.CheckDay(day); err != nil {
				return fail(fmt.Errorf("log_transaction date: %w", err))
			}
			// Normalized exactly like the store, so the echoed `type` equals the
			// stored row: without the trim, `type=" Expense "` stored `expense`
			// but returned `" expense "`.
			typ := strings.ToLower(strings.TrimSpace(in.Type))
			tid, created, err := store.Insert(ctx, day, in.Amount, typ, in.Category, in.Account, in.SendingTo, in.Note, "log_transaction", in.IdempotencyKey)
			if err != nil {
				return fail(err)
			}
			// Read the stored row back by id so the result reports normalized
			// values (category lowercased, blank→other; account resolved from
			// hex; note truncated; both transfer legs) instead of the raw input.
			// If the re-read itself fails, ok stays true — the write succeeded
			// and retrying without a key would double-log — with readback_error
			// naming the failure instead of silently omitting fields.
			out := map[string]any{"ok": true, "id": tid, "date": day, "type": typ,
				"amount": in.Amount, "created": created}
			if rows, merr := store.Match(ctx, map[string]any{"id": tid}); merr != nil {
				out["readback_error"] = merr.Error()
			} else if len(rows) == 1 {
				out["category"] = rows[0]["category"]
				out["account"] = rows[0]["account"]
				if n, ok := rows[0]["note"]; ok {
					out["note"] = n
				}
				if st, ok := rows[0]["sending_to"]; ok {
					out["sending_to"] = st
				}
				if sta, ok := rows[0]["sending_to_account"]; ok {
					out["sending_to_account"] = sta
				}
			} else {
				out["readback_error"] = fmt.Sprintf("re-read by id %s returned %d rows", tid, len(rows))
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "query_transactions",
		Description: "List transactions between start and end dates (YYYY-MM-DD inclusive, padded bounds trimmed like log/delete, start <= end; anything else is rejected rather than returning nothing. Bounds are plain calendar-day strings with no timezone conversion — pass IST dates to match how rows are stored (stored rows default to IST today). Optional type/category/account filters (whitespace-only means absent); account is a case-sensitive exact name or id hex, unlike type/category — `cash` does not find `Cash` — and matches EITHER side of a transfer (sender or receiver), so a transfer appears under both accounts. Unknown type/category rejected like writes (not a silent zero). Paged: limit defaults to 100, clamped to 500; offset defaults to 0. Paging typos: offset: -1 is rejected (a negative skip is a typo, not page 0); limit: 0 or negative silently means 100, not unlimited (check the echoed limit — expect 100); limit above 500 (e.g. 9999) silently clamps to 500 — check the echoed limit. The response carries count (rows in this page — unlike total, which counts all matches), limit (the applied cap), total, truncated and next_offset so a short page is never mistaken for the end. There is no unlimited `query_transactions` page on MCP (delete dry-runs stay unbounded by design) — to read everything, repeat with offset = next_offset until truncated is false (next_offset is null then). Rows come back date-ascending with an _id tiebreak (oldest first; deterministic order, but not a snapshot — a concurrent log or TTL expiry between page fetches can shift rows across page boundaries)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in queryTransactionsInput) (*mcp.CallToolResult, map[string]any, error) {
			var typ, cat, acct *string
			if t := strings.TrimSpace(in.Type); t != "" {
				typ = &t
			}
			if c := strings.TrimSpace(in.Category); c != "" {
				cat = &c
			}
			if a := strings.TrimSpace(in.Account); a != "" {
				acct = &a
			}
			start, end, err := normalizeDayRange("query_transactions", in.Start, in.End)
			if err != nil {
				return fail(err)
			}
			limit := clampLimit(in.Limit)
			if in.Offset < 0 {
				return fail(fmt.Errorf("query_transactions offset must be >= 0 — got %d", in.Offset))
			}
			rows, total, err := store.Query(ctx, start, end, typ, cat, acct, limit, in.Offset)
			if err != nil {
				return fail(err)
			}
			truncated, next := pageWindow(in.Offset, len(rows), total)
			return result(map[string]any{"ok": true, "count": len(rows), "total": total,
				"truncated": truncated, "next_offset": next, "limit": limit,
				"transactions": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_transactions",
		Description: "Delete transactions (balances inverted atomically). Target one row by id — a non-existent id is rejected, not a silent 0 — or filter by amount (exact paise: > 0, at most 2 decimals; an explicit 0 or >2-decimal value is rejected, not a wildcard)/type/category/date/account (case-sensitive exact name or hex, unlike type/category; whitespace-only means absent); dates are plain calendar-day strings with no timezone conversion — pass IST dates to match how rows are stored. id cannot be combined with filters. account matches EITHER side of a transfer, so deleting by account removes transfers where it only received; check dry_run first. Unknown type/category rejected like writes (not a silent would_delete: 0). dry_run=true (default) only reports what would go (uncapped by design — a dry run must show everything it would delete); pass dry_run=false to delete."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deleteTransactionsInput) (*mcp.CallToolResult, map[string]any, error) {
			// Trimmed before the emptiness checks like the query bounds, so
			// whitespace-only means absent (not an "unknown account" error):
			// `account="   "` filters nothing instead of failing.
			in.ID = strings.TrimSpace(in.ID)
			in.Type = strings.TrimSpace(in.Type)
			in.Category = strings.TrimSpace(in.Category)
			in.Date = strings.TrimSpace(in.Date)
			in.Account = strings.TrimSpace(in.Account)
			filt := map[string]any{}
			if in.ID != "" {
				if in.Amount != nil || in.Type != "" || in.Category != "" || in.Date != "" || in.Account != "" {
					return result(map[string]any{"ok": false, "error": "id cannot be combined with amount, type, category, date or account."})
				}
				filt["id"] = in.ID
			} else {
				if in.Amount != nil {
					// Rejected like writes: 0 is not a narrower filter (no
					// zero-amount rows exist), it would silently widen to
					// every row matching the remaining filters. Converted to
					// paise with exactness enforced: a >2-decimal filter
					// would match paise it never stored.
					if *in.Amount <= 0 {
						return fail(fmt.Errorf("delete_transactions amount must be > 0 — 0 means absent and cannot narrow a delete"))
					}
					paise, ok := money.ExactPaise(*in.Amount)
					if !ok {
						return fail(fmt.Errorf("delete_transactions amount %v is not an exact paise value — use at most 2 decimals", *in.Amount))
					}
					filt["amount"] = paise
				}
				if in.Type != "" {
					filt["type"] = in.Type
				}
				if in.Category != "" {
					filt["category"] = in.Category
				}
				if in.Date != "" {
					// Trimmed + calendar-validated like writes/reads: without this,
					// `date=" 2026-09-01 "` silently reports `would_delete: 0`
					// for a row that exists, and `2026-02-30` matches nothing
					// instead of being rejected.
					d, err := validate.CheckDay(in.Date)
					if err != nil {
						return fail(fmt.Errorf("delete_transactions date: %w", err))
					}
					filt["date"] = d
				}
				if in.Account != "" {
					filt["account"] = in.Account
				}
				if len(filt) == 0 {
					return result(map[string]any{"ok": false, "error": "Provide id, or at least one of amount, type, category, date, account."})
				}
			}
			dry := true
			if in.DryRun != nil {
				dry = *in.DryRun
			}
			// A non-existent id is rejected like unknown account/type/category,
			// not a silent 0: with id there is no second signal, so 0 would read
			// as "nothing to delete" for a typo. Other filters may validly match
			// nothing, so they keep the 0.
			byID := in.ID != ""
			if dry {
				rows, err := store.Match(ctx, filt)
				if err != nil {
					return fail(err)
				}
				if byID && len(rows) == 0 {
					return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown transaction id '%s'", in.ID)})
				}
				return result(map[string]any{"ok": true, "dry_run": true,
					"would_delete": len(rows), "transactions": rows})
			}
			n, err := store.Delete(ctx, filt)
			if err != nil {
				return fail(err)
			}
			if byID && n == 0 {
				return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown transaction id '%s'", in.ID)})
			}
			return result(map[string]any{"ok": true, "dry_run": false, "deleted": n})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "edit_transaction",
		Description: "Edit one transaction by id (balances rebalanced atomically — old legs reversed, new legs applied, so a type/account/amount move can never half-land). Required: id (a missing id is rejected, not a silent 0). Optional: amount (exact paise: > 0, at most 2 decimals; explicit 0 rejected), type/category (normalized + validated like writes, except blank means no change — pass explicit `other` to reset the category; unknown rejected), account/sending_to (case-sensitive exact name or id hex like writes, unlike type/category; transfer requires sending_to when the result would otherwise have no destination (converting a non-transfer into one) — edits on an existing transfer keep its legs unless a new destination is passed, and it must differ; forbidden on non-transfers except whitespace-only (counts as absent); moving a leg onto an archived account rejects, but resupplying the leg already on the row does not — it is not a move, so copying `account` from a query back into the edit keeps working — even when converting away from a transfer, where the resupplied destination is ignored and the leg drops), date (real YYYY-MM-DD calendar date, IST day like writes when supplied — empty/whitespace means no change, unlike log where it means today; moves expiresAt with it — a far-past date purges quickly via TTL while the rebalanced delta stays, like a logged far-past row), note (capped at 300 bytes like writes; blank means leave alone, it cannot be cleared). Absent or whitespace-only means no change — except converting to transfer still requires `sending_to`; at least one field must change. Returns the edited row read back by id; if the re-read fails, `ok` stays true with `readback_error` (like log_transaction)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in editTransactionInput) (*mcp.CallToolResult, map[string]any, error) {
			// Trimmed before the emptiness checks like delete's filters, so
			// whitespace-only means absent (no change) instead of an error.
			in.ID = strings.TrimSpace(in.ID)
			if in.ID == "" {
				return result(map[string]any{"ok": false, "error": "transaction id is required."})
			}
			in.Type = strings.TrimSpace(in.Type)
			in.Category = strings.TrimSpace(in.Category)
			in.Account = strings.TrimSpace(in.Account)
			in.SendingTo = strings.TrimSpace(in.SendingTo)
			in.Date = strings.TrimSpace(in.Date)
			var typ, cat, acct, dst, date, note *string
			if in.Type != "" {
				typ = &in.Type
			}
			if in.Category != "" {
				cat = &in.Category
			}
			if in.Account != "" {
				acct = &in.Account
			}
			if in.SendingTo != "" {
				dst = &in.SendingTo
			}
			if in.Date != "" {
				// Calendar-validated like delete's filter: a padded or
				// non-calendar date fails instead of storing an expiry the
				// TTL reads as year-1.
				d, err := validate.CheckDay(in.Date)
				if err != nil {
					return fail(fmt.Errorf("edit_transaction date: %w", err))
				}
				date = &d
			}
			if strings.TrimSpace(in.Note) != "" {
				note = &in.Note
			}
			if in.Amount == nil && typ == nil && cat == nil && acct == nil && dst == nil && date == nil && note == nil {
				return result(map[string]any{"ok": false, "error": "provide at least one of amount, type, category, account, sending_to, date, note."})
			}
			changed, err := store.EditTransaction(ctx, in.ID, in.Amount, typ, cat, acct, dst, date, note)
			if err != nil {
				return fail(err)
			}
			// Read the edited row back by id so the result reports stored
			// values (normalized, truncated, re-legged) instead of the input.
			out := map[string]any{"ok": true, "id": in.ID, "changed": changed}
			if rows, merr := store.Match(ctx, map[string]any{"id": in.ID}); merr != nil {
				out["readback_error"] = merr.Error()
			} else if len(rows) == 1 {
				out["transaction"] = rows[0]
			} else {
				out["readback_error"] = fmt.Sprintf("re-read by id %s returned %d rows", in.ID, len(rows))
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "prune_old",
		Description: "Immediate purge, defaulting to the 90-day retention window (TTL handles this natively in the background). Cutoff uses the same UTC frame as TTL (not IST like user-visible dates); boundary note: prune keeps `date == cutoff` while TTL expires it at 00:00 UTC today, so dry-run counts can trail TTL by a day. Does NOT touch balances — unlike delete_transactions, which inverts the legs, and edit_transaction, which reverses/applies them. days <= 0 means 90 — including negatives, never an error. dry_run=true (default) only reports."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in pruneOldInput) (*mcp.CallToolResult, map[string]any, error) {
			days := in.Days
			if days <= 0 {
				days = 90
			}
			dry := true
			if in.DryRun != nil {
				dry = *in.DryRun
			}
			n, err := store.Prune(ctx, days, dry)
			if err != nil {
				return fail(err)
			}
			would, removed := n, int64(0)
			if !dry {
				would, removed = 0, n
			}
			return result(map[string]any{"ok": true, "dry_run": dry, "would_remove": would, "removed": removed})
		})
}
