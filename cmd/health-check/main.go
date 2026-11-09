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

// Input types are named rather than inline so `schema_test.go` can infer the schema from
// the same types `AddTool` publishes — an inline struct can only be checked by reading the
// handler, and here the handler and the published schema disagreed: nothing carried
// `omitempty`, so every field was advertised as required.
//
// The rule used for each field, since `omitempty` is the only switch available:
//
//   - required when the server rejects the call without it — that is, when store
//     validation fails on a zero value
//   - optional otherwise, INCLUDING fields a tool description names as inputs. A
//     description saying "type e.g. run" does not make `log_workout.type` required:
//     `store.LogWorkout` defaults "" to "workout", so requiring it would make the schema
//     stricter than the server.
//
// The rule is deliberately one-directional, because a schema is a client-side hint and
// the failure modes are not symmetric. Looser than the server and a caller omits
// something they meant to send; stricter, and the server rejects a call it would have
// accepted. The description is the agent's only guide to what a tool wants, so where it
// and the store disagree the store wins and the disagreement is written down.
//
// That is why `log_meal.description` is optional even though every meal ought to have
// one: nothing validates it, so requiring it would be inventing a contract the store does
// not have. And `prune_old.days` is optional for the opposite reason: zero means 30, so
// the zero value IS the default and there is no absence to distinguish.
type logMealInput struct {
	// checkItems rejects an absent or empty items list.
	Items []map[string]any `json:"items"`
	// Free text the agent parsed. Neither validated nor stored: store.LogMeal accepts
	// `description` and never writes it to the document. Kept in the schema because the
	// SDK emits `additionalProperties: false`, so removing the field would make every
	// existing caller's `description` fail validation — dropping it is a breaking change,
	// not a cleanup. Persisting it is a separate decision about the meal document's shape.
	Description string `json:"description,omitempty"`
	// dayOf("") is today.
	Date string `json:"date,omitempty"`
}

type fixLastMealInput struct {
	Items []map[string]any `json:"items"`
	// Also accepted and never stored by store.FixLastMeal — see logMealInput.
	Description string `json:"description,omitempty"`
}

type queryMealsInput struct {
	// Both go through mustDay, which rejects "" — so these are genuinely required,
	// unlike the `date` argument elsewhere in this server.
	Start string `json:"start"`
	End   string `json:"end"`
}

type deleteMealsInput struct {
	// "default today" per the tool description, via dayOf.
	Date string `json:"date,omitempty"`
}

type logWeightInput struct {
	// The store rejects an implausible weight, so 0 cannot mean "unset".
	Kg float64 `json:"kg"`
	// hc_weight is upserted per day; dayOf("") is today.
	Date string `json:"date,omitempty"`
}

type logSleepInput struct {
	// The store rejects an implausible duration.
	Hours float64 `json:"hours"`
	Date  string  `json:"date,omitempty"`
}

type logWorkoutInput struct {
	// store.LogWorkout truncates an over-long type and defaults "" to "workout", so a
	// workout logged without one is still stored — labelled "workout". Optional, because
	// the tool description naming a field as an input is not on its own a reason to
	// require it: a schema stricter than the server rejects calls the server would have
	// accepted, which is the failure mode this change exists to remove everywhere else.
	Type string `json:"type,omitempty"`
	// The store rejects minutes <= 0, so 0 cannot mean "unset".
	Minutes float64 `json:"minutes"`
	// "optional kcal burn" — and 0 is a real value.
	Kcal float64 `json:"kcal,omitempty"`
	Date string  `json:"date,omitempty"`
}

type dailySummaryInput struct {
	Date string `json:"date,omitempty"`
}

type pruneOldInput struct {
	// 0 means 30 in the handler, so the zero value is the default.
	Days int `json:"days,omitempty"`
	// A pointer because nil is meaningful: nil means dry run, which is the safe
	// default for anything that deletes.
	DryRun *bool `json:"dry_run,omitempty"`
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
		func(ctx context.Context, _ *mcp.CallToolRequest, in logMealInput) (*mcp.CallToolResult, map[string]any, error) {
			meal, err := store.LogMeal(ctx, dayOf(in.Date), in.Description, in.Items)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "meal": meal})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "fix_last_meal",
		Description: "Replace the most recent meal's items with corrected items."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in fixLastMealInput) (*mcp.CallToolResult, map[string]any, error) {
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
		func(ctx context.Context, _ *mcp.CallToolRequest, in queryMealsInput) (*mcp.CallToolResult, map[string]any, error) {
			rows, err := store.QueryMeals(ctx, in.Start, in.End)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "meals": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_meals",
		Description: "Delete meals for one date (YYYY-MM-DD, default today)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deleteMealsInput) (*mcp.CallToolResult, map[string]any, error) {
			n, err := store.DeleteMeals(ctx, dayOf(in.Date))
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "deleted": n})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "log_weight",
		Description: "Log body weight in kg (upserts one row per date). Never pruned."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in logWeightInput) (*mcp.CallToolResult, map[string]any, error) {
			w, err := store.LogWeight(ctx, dayOf(in.Date), in.Kg)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "weight": w})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "log_sleep",
		Description: "Log sleep hours. Date = morning of wake-up."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in logSleepInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := store.LogSleep(ctx, dayOf(in.Date), in.Hours)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "sleep": out})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "log_workout",
		Description: "Log a workout (type e.g. run/lift/walk, minutes, optional kcal burn)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in logWorkoutInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := store.LogWorkout(ctx, dayOf(in.Date), in.Type, in.Minutes, in.Kcal)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "workout": out})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "daily_summary",
		Description: "Daily recap: cal-in totals vs cal-out (workouts + active) + weight/sleep/steps."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in dailySummaryInput) (*mcp.CallToolResult, map[string]any, error) {
			sum, err := store.DailySummary(ctx, dayOf(in.Date))
			if err != nil {
				return fail(err)
			}
			sum["ok"] = true
			return result(sum)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "prune_old",
		Description: "Prune hc_meals/hc_days older than `days`. NEVER touches hc_weight. dry_run=true (default) only reports counts."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in pruneOldInput) (*mcp.CallToolResult, map[string]any, error) {
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
