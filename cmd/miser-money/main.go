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

func main() {
	var err error
	store, err = money.FromEnv()
	if err != nil {
		log.Fatalf("miser-money: %v", err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "miser-money", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "create_account",
		Description: "Create a money account (e.g. Cash, HDFC Checking). name unique; type cash|bank|card|wallet|other."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Name    string  `json:"name"`
			Type    string  `json:"type"`
			Balance float64 `json:"balance"`
		}) (*mcp.CallToolResult, map[string]any, error) {
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
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			IncludeArchived bool `json:"include_archived"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			rows, err := store.ListAccounts(ctx, in.IncludeArchived)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "accounts": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "archive_account",
		Description: "Soft-delete an account (history stays queryable; blocked from new writes)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Name string `json:"name"`
		}) (*mcp.CallToolResult, map[string]any, error) {
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
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, map[string]any, error) {
			out, err := store.Balances(ctx)
			if err != nil {
				return fail(err)
			}
			out["ok"] = true
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "log_transaction",
		Description: "Log an income, expense, or transfer (atomic with balance update). account required; sending_to required for transfers; date YYYY-MM-DD defaults to today."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Type      string  `json:"type"`
			Amount    float64 `json:"amount"`
			Category  string  `json:"category"`
			Account   string  `json:"account"`
			SendingTo string  `json:"sending_to"`
			Note      string  `json:"note"`
			Date      string  `json:"date"`
		}) (*mcp.CallToolResult, map[string]any, error) {
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
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Text    string `json:"text"`
			Account string `json:"account"`
		}) (*mcp.CallToolResult, map[string]any, error) {
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
		Description: "List transactions between start and end dates (YYYY-MM-DD inclusive). Optional type/category/account filters."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Start    string `json:"start"`
			End      string `json:"end"`
			Type     string `json:"type"`
			Category string `json:"category"`
			Account  string `json:"account"`
		}) (*mcp.CallToolResult, map[string]any, error) {
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
			rows, err := store.Query(ctx, in.Start, in.End, typ, cat, acct)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "transactions": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "summarize",
		Description: "Summarize income, expenses, net total and per-category breakdown. Optional account filter; period phrase or explicit start/end."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Period  string `json:"period"`
			Start   string `json:"start"`
			End     string `json:"end"`
			Account string `json:"account"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			start, end, label := in.Start, in.End, ""
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
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Amount float64 `json:"amount"`
		}) (*mcp.CallToolResult, map[string]any, error) {
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
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Amount   float64 `json:"amount"`
			Category string  `json:"category"`
			Date     string  `json:"date"`
			Account  string  `json:"account"`
		}) (*mcp.CallToolResult, map[string]any, error) {
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
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Days   int  `json:"days"`
			DryRun *bool `json:"dry_run"`
		}) (*mcp.CallToolResult, map[string]any, error) {
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

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "miser-money:", err)
		os.Exit(1)
	}
}
