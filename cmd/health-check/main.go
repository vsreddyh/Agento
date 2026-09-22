// Command health-check is the cal-in + cal-out + weight MCP server.
// Storage: MongoDB (hc_meals, hc_weight never pruned, hc_days).
// Runs over stdio for MCP clients.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"agento/internal/healthcheck"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var store *healthcheck.Store

func fail(err error) (*mcp.CallToolResult, map[string]any, error) {
	return nil, map[string]any{"ok": false, "error": err.Error()}, nil
}

func result(out map[string]any) (*mcp.CallToolResult, map[string]any, error) {
	b, _ := json.Marshal(out)
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
		StructuredContent: out,
	}, out, nil
}

func main() {
	var err error
	store, err = healthcheck.FromEnv()
	if err != nil {
		log.Fatalf("health-check: %v", err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "health-check", Version: "1.0.0"}, nil)

	dayOf := func(date string) string {
		if date == "" {
			return time.Now().Format("2006-01-02")
		}
		return date
	}

	mcp.AddTool(s, &mcp.Tool{Name: "log_meal",
		Description: "Log a meal. Agent parses user text into items[{name, qty?, kcal, protein, carbs, fat, fiber}] — MCP validates and names any missing macro field."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Description string           `json:"description"`
			Items       []map[string]any `json:"items"`
			Date        string           `json:"date"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			meal, err := store.LogMeal(ctx, dayOf(in.Date), in.Description, in.Items)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "meal": meal})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "fix_last_meal",
		Description: "Replace the most recent meal's items with corrected items."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Description string           `json:"description"`
			Items       []map[string]any `json:"items"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			meal, err := store.FixLastMeal(ctx, in.Description, in.Items)
			if err != nil {
				return fail(err)
			}
			if meal == nil {
				return result(map[string]any{"ok": false, "error": "No meals to fix."})
			}
			return result(map[string]any{"ok": true, "meal": meal})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "query_meals",
		Description: "List meals between start/end dates (YYYY-MM-DD inclusive)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Start string `json:"start"`
			End   string `json:"end"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			rows, err := store.QueryMeals(ctx, in.Start, in.End)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "meals": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_meals",
		Description: "Delete meals for one date (YYYY-MM-DD, default today)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Date string `json:"date"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			n, err := store.DeleteMeals(ctx, dayOf(in.Date))
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "deleted": n})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "log_weight",
		Description: "Log body weight in kg (upserts one row per date). Never pruned."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Kg   float64 `json:"kg"`
			Date string  `json:"date"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			w, err := store.LogWeight(ctx, dayOf(in.Date), in.Kg)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "weight": w})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "log_sleep",
		Description: "Log sleep hours. Date = morning of wake-up."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Hours float64 `json:"hours"`
			Date  string  `json:"date"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			out, err := store.LogSleep(ctx, dayOf(in.Date), in.Hours)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "sleep": out})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "log_workout",
		Description: "Log a workout (type e.g. run/lift/walk, minutes, optional kcal burn)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Type    string  `json:"type"`
			Minutes float64 `json:"minutes"`
			Kcal    float64 `json:"kcal"`
			Date    string  `json:"date"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			out, err := store.LogWorkout(ctx, dayOf(in.Date), in.Type, in.Minutes, in.Kcal)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "workout": out})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "daily_summary",
		Description: "Daily recap: cal-in totals vs cal-out (workouts + active) + weight/sleep/steps."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Date string `json:"date"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			sum, err := store.DailySummary(ctx, dayOf(in.Date))
			if err != nil {
				return fail(err)
			}
			sum["ok"] = true
			return result(sum)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "prune_old",
		Description: "Prune hc_meals/hc_days older than `days`. NEVER touches hc_weight. dry_run=true (default) only reports counts."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Days   int   `json:"days"`
			DryRun *bool `json:"dry_run"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			days := in.Days
			if days == 0 {
				days = 30
			}
			dry := true
			if in.DryRun != nil {
				dry = *in.DryRun
			}
			out, err := store.Prune(ctx, days, dry)
			if err != nil {
				return fail(err)
			}
			out["ok"] = true
			out["dry_run"] = dry
			return result(out)
		})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "health-check:", err)
		os.Exit(1)
	}
}
