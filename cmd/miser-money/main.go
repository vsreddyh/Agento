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
	"time"

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

func today() string { return time.Now().Format("2006-01-02") }

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
// And two tools have no required field at all, because their constraint is "at least one
// of" — `summarize` (a period phrase or an explicit start/end) and `delete_transactions`
// (at least one filter). Both are enforced server-side. Expressing that as `anyOf` would
// be more precise than this schema can be, and a constraint the caller cannot see is still
// better than one it sees wrongly.
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
	// The account is identified by name, so this one cannot be defaulted.
	Name string `json:"name"`
}

// getBalancesInput is empty on purpose: the tool takes no arguments, and naming it keeps
// the schema test able to assert that it stays empty.
type getBalancesInput struct{}

type logTransactionInput struct {
	// store.Insert rejects a non-positive amount and an unknown type.
	Type   string  `json:"type"`
	Amount float64 `json:"amount"`
	// NOT required: the store lowercases an empty category to "other" and does the same
	// for one it does not recognise, so an absent category is never rejected — it is
	// filed under "other". Marking it required would reject a call the server accepts,
	// which is the failure mode this change exists to remove.
	Category string `json:"category,omitempty"`
	// "account required" per the tool description.
	Account string `json:"account"`
	// Required only for a transfer, which a schema cannot express conditionally; the
	// store enforces it when type is transfer.
	SendingTo string `json:"sending_to,omitempty"`
	Note      string `json:"note,omitempty"`
	// The handler fills in today() when absent.
	Date string `json:"date,omitempty"`
}

type logTextInput struct {
	Text string `json:"text"`
	// "Optional account override" per the tool description; blank means resolve it.
	Account string `json:"account,omitempty"`
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
}

type summarizeInput struct {
	// A period phrase, or an explicit start/end pair — one of the two is required, and
	// the server decides which. No single field is required.
	Period  string `json:"period,omitempty"`
	Start   string `json:"start,omitempty"`
	End     string `json:"end,omitempty"`
	Account string `json:"account,omitempty"`
}

type fixLastTransactionInput struct {
	// The corrected amount. 0 is not a correction — it would invert the balance.
	Amount float64 `json:"amount"`
}

type deleteTransactionsInput struct {
	// At least one filter is required, and the server rejects none — so no field can be
	// marked required without forbidding a legitimate combination.
	Amount   float64 `json:"amount,omitempty"`
	Category string  `json:"category,omitempty"`
	Date     string  `json:"date,omitempty"`
	Account  string  `json:"account,omitempty"`
}

type pruneOldInput struct {
	// 0 means 90 in the handler.
	Days int `json:"days,omitempty"`
	// A pointer because nil is meaningful: nil means dry run, which is the safe default
	// for anything that deletes.
	DryRun *bool `json:"dry_run,omitempty"`
}

// normalizeDayRange validates a start/end pair and returns it NORMALIZED, for three
// reasons that each produce a wrong answer rather than an error if skipped:
//
//   - empty or malformed bounds build a filter like `date >= "" && date <= ""`, which
//     matches nothing, so `query_transactions` returns zero transactions with no reason
//     given;
//   - a lone bound makes `summarize` fall through to the period branch and summarise a
//     window nobody asked for;
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

// normalizeDayRangePair additionally allows NEITHER bound, which is how `summarize` is told
// to use its period phrase, and rejects exactly one — the case a caller is most likely to
// send by accident, having filled in one bound and forgotten the other.
//
// Both bounds are trimmed before the either/or test, so a whitespace-only "bound" counts as
// absent rather than as half a pair.
func normalizeDayRangePair(what, start, end string) (string, string, error) {
	st, et := strings.TrimSpace(start), strings.TrimSpace(end)
	if (st == "") != (et == "") {
		return "", "", fmt.Errorf("%s needs both start and end, or neither — got start=%q end=%q",
			what, start, end)
	}
	if st == "" {
		return "", "", nil
	}
	return normalizeDayRange(what, st, et)
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
		Description: "Create a money account (e.g. Cash, HDFC Checking). name unique; type cash|bank|card|wallet|other."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in createAccountInput) (*mcp.CallToolResult, map[string]any, error) {
			if in.Type == "" {
				in.Type = "cash"
			}
			acct, err := store.CreateAccount(ctx, in.Name, in.Type, in.Balance)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "account": acct})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_accounts",
		Description: "List accounts with their current (stored) balances."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listAccountsInput) (*mcp.CallToolResult, map[string]any, error) {
			rows, err := store.ListAccounts(ctx, in.IncludeArchived)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "accounts": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "archive_account",
		Description: "Soft-delete an account (history stays queryable; blocked from new writes)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in archiveAccountInput) (*mcp.CallToolResult, map[string]any, error) {
			done, err := store.ArchiveAccount(ctx, in.Name)
			if err != nil {
				return fail(err)
			}
			if !done {
				return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown account '%s'", in.Name)})
			}
			return result(map[string]any{"ok": true, "archived": in.Name})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_balances",
		Description: "Current per-account balances plus total across active accounts."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in getBalancesInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := store.Balances(ctx)
			if err != nil {
				return fail(err)
			}
			out["ok"] = true
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "log_transaction",
		Description: "Log an income, expense, or transfer (atomic with balance update). account required; sending_to required for transfers; date YYYY-MM-DD defaults to today."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in logTransactionInput) (*mcp.CallToolResult, map[string]any, error) {
			day := in.Date
			if day == "" {
				day = today()
			}
			typ := strings.ToLower(in.Type)
			tid, err := store.Insert(ctx, day, in.Amount, typ, in.Category, in.Account, in.SendingTo, in.Note, "log_transaction")
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "id": tid, "date": day, "type": typ, "amount": in.Amount})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "log_text",
		Description: "Log from free-form text ('spent 300 on groceries'). Optional account override; blank requires an existing account."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in logTextInput) (*mcp.CallToolResult, map[string]any, error) {
			r := money.Classify(in.Text)
			note := in.Text
			if len(note) > 300 {
				note = note[:300]
			}
			switch r.Action {
			case "log_income", "log_expense":
				if r.Amount == nil {
					return result(map[string]any{"ok": false, "error": "No amount found — ask the user how much."})
				}
				txType := "income"
				if r.Action == "log_expense" {
					txType = "expense"
				}
				cat := "other"
				if r.Category != nil {
					cat = *r.Category
				}
				tid, err := store.Insert(ctx, today(), *r.Amount, txType, cat, in.Account, "", note, "log_text")
				if err != nil {
					return fail(err)
				}
				return result(map[string]any{"ok": true, "id": tid, "type": txType, "amount": *r.Amount, "category": cat})
			case "log_transfer":
				if r.Amount == nil {
					return result(map[string]any{"ok": false, "error": "No amount found — ask the user how much."})
				}
				dest := ""
				if r.SendingTo != nil {
					dest = *r.SendingTo
				}
				if dest == "" {
					return result(map[string]any{"ok": false, "error": "No destination account found — ask the user where to transfer to."})
				}
				tid, err := store.Insert(ctx, today(), *r.Amount, "transfer", "other", in.Account, dest, note, "log_text")
				if err != nil {
					return fail(err)
				}
				return result(map[string]any{"ok": true, "id": tid, "type": "transfer", "amount": *r.Amount, "sending_to": dest})
			}
			return result(map[string]any{"ok": false, "action": r.Action,
				"error": fmt.Sprintf("Not a loggable statement (classified as '%s').", r.Action)})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "query_transactions",
		Description: "List transactions between start and end dates (YYYY-MM-DD inclusive, start <= end; anything else is rejected rather than returning nothing). Optional type/category/account filters."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in queryTransactionsInput) (*mcp.CallToolResult, map[string]any, error) {
			var typ, cat, acct *string
			if in.Type != "" {
				typ = &in.Type
			}
			if in.Category != "" {
				cat = &in.Category
			}
			if in.Account != "" {
				acct = &in.Account
			}
			start, end, err := normalizeDayRange("query_transactions", in.Start, in.End)
			if err != nil {
				return fail(err)
			}
			rows, err := store.Query(ctx, start, end, typ, cat, acct)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "transactions": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "summarize",
		Description: "Summarize income, expenses, net total and per-category breakdown. Optional account filter. Give a period phrase OR an explicit start/end pair — half a pair is rejected, and an explicit pair wins over a period phrase if you send both."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in summarizeInput) (*mcp.CallToolResult, map[string]any, error) {
			// An explicit range wins over `period` — see the tool description, which now
			// says so instead of leaving the precedence to be discovered.
			start, end, err := normalizeDayRangePair("summarize", in.Start, in.End)
			if err != nil {
				return fail(err)
			}
			label := ""
			if start != "" && end != "" {
				label = start + " to " + end
			} else {
				if in.Period == "" {
					in.Period = "this month"
				}
				s, e, l := money.ResolvePeriod(in.Period, time.Now())
				start, end, label = s, e, l
			}
			var acct *string
			if in.Account != "" {
				acct = &in.Account
			}
			sum, err := store.Summarize(ctx, start, end, acct)
			if err != nil {
				return fail(err)
			}
			pairs := []map[string]any{}
			if raw, ok := sum["by_category"].([]map[string]any); ok {
				for _, e := range raw {
					pairs = append(pairs, map[string]any{"category": e["category"], "total": e["total"]})
				}
			}
			sum["by_category"] = pairs
			sum["ok"] = true
			sum["label"] = label
			sum["start"] = start
			sum["end"] = end
			return result(sum)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "fix_last_transaction",
		Description: "Correct the most recent transaction's amount (balances adjusted atomically)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in fixLastTransactionInput) (*mcp.CallToolResult, map[string]any, error) {
			done, err := store.FixLast(ctx, in.Amount)
			if err != nil {
				return fail(err)
			}
			if !done {
				return result(map[string]any{"ok": false, "error": "No transactions to fix."})
			}
			return result(map[string]any{"ok": true, "amount": in.Amount})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_transactions",
		Description: "Delete matching transactions (balances inverted atomically). At least one filter required."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deleteTransactionsInput) (*mcp.CallToolResult, map[string]any, error) {
			filt := map[string]any{}
			if in.Amount != 0 {
				filt["amount"] = in.Amount
			}
			if in.Category != "" {
				filt["category"] = in.Category
			}
			if in.Date != "" {
				filt["date"] = in.Date
			}
			if in.Account != "" {
				filt["account"] = in.Account
			}
			if len(filt) == 0 {
				return result(map[string]any{"ok": false, "error": "Provide at least one of amount, category, date, account."})
			}
			n, err := store.Delete(ctx, filt)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "deleted": n})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "prune_old",
		Description: "Immediate 90-day purge (TTL handles this natively in the background). dry_run=true (default) only reports."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in pruneOldInput) (*mcp.CallToolResult, map[string]any, error) {
			days := in.Days
			if days == 0 {
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
