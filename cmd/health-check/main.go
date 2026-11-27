// Command health-check is the food + weight + sleep + workout MCP server.
// Storage: MongoDB (hc_food_log, hc_weight, hc_days, hc_sleep,
// hc_workouts — all permanent, no prune, no TTL). Runs over stdio for MCP
// clients.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"reflect"
	"time"

	"agento/internal/healthcheck"

	"github.com/google/jsonschema-go/jsonschema"
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

// Input types are named so schema_test.go pins the published contract from
// the same types AddTool publishes. Required = the store rejects the call
// without it (omitempty is the only switch); everything else is optional.
//
// Enums: the SDK infers plain strings, so sort_by/sort get real JSON-schema
// enums via enumSchema (inferred base + enum patch, one source). Unknown
// values throw on both layers — schema first, then the store's fail()
// naming the allowed set.
func enumSchema(in any, enums map[string][]string) *jsonschema.Schema {
	s, err := jsonschema.ForType(reflect.TypeOf(in), nil)
	if err != nil {
		panic(err)
	}
	for field, values := range enums {
		prop, ok := s.Properties[field]
		if !ok {
			panic(fmt.Sprintf("enum field %q missing from schema", field))
		}
		anyVals := make([]any, 0, len(values))
		for _, v := range values {
			anyVals = append(anyVals, v)
		}
		prop.Enum = anyVals
		s.Properties[field] = prop
	}
	return s
}

var sortEnum = []string{"asc", "desc"}

type logFoodInput struct {
	What     string   `json:"what"`
	Qty      string   `json:"qty,omitempty"`
	Kcal     *float64 `json:"kcal,omitempty"`
	Protein  *float64 `json:"protein,omitempty"`
	Carbs    *float64 `json:"carbs,omitempty"`
	Fat      *float64 `json:"fat,omitempty"`
	Fiber    *float64 `json:"fiber,omitempty"`
	Note     string   `json:"note,omitempty"`
	Datetime string   `json:"datetime,omitempty"`
}

type foodIDInput struct {
	ID string `json:"id"`
}

type listFoodInput struct {
	Start  string `json:"start"`
	End    string `json:"end"`
	SortBy string `json:"sort_by,omitempty"`
	Sort   string `json:"sort,omitempty"`
	What   string `json:"what,omitempty"`
}

type correctFoodInput struct {
	ID          string   `json:"id"`
	What        *string  `json:"what,omitempty"`
	Qty         *string  `json:"qty,omitempty"`
	Kcal        *float64 `json:"kcal,omitempty"`
	Protein     *float64 `json:"protein,omitempty"`
	Carbs       *float64 `json:"carbs,omitempty"`
	Fat         *float64 `json:"fat,omitempty"`
	Fiber       *float64 `json:"fiber,omitempty"`
	Note        *string  `json:"note,omitempty"`
	ClearMacros []string `json:"clear_macros,omitempty"`
	Datetime    *string  `json:"datetime,omitempty"`
}

type deleteFoodInput struct {
	ID     string `json:"id"`
	DryRun *bool  `json:"dry_run,omitempty"`
}

type logWeightInput struct {
	Kg   float64 `json:"kg"`
	Date string  `json:"date,omitempty"`
}

type listWeightInput struct {
	Start  string `json:"start"`
	End    string `json:"end"`
	SortBy string `json:"sort_by,omitempty"`
	Sort   string `json:"sort,omitempty"`
}

type deleteWeightInput struct {
	Date   string `json:"date"`
	DryRun *bool  `json:"dry_run,omitempty"`
}

type logSleepInput struct {
	Hours float64 `json:"hours"`
	Date  string  `json:"date,omitempty"`
}

type listSleepInput struct {
	Start  string `json:"start"`
	End    string `json:"end"`
	SortBy string `json:"sort_by,omitempty"`
	Sort   string `json:"sort,omitempty"`
}

type correctSleepInput struct {
	ID      string   `json:"id"`
	Minutes *int     `json:"minutes,omitempty"`
	Hours   *float64 `json:"hours,omitempty"`
}

type deleteSleepInput struct {
	ID     string `json:"id"`
	DryRun *bool  `json:"dry_run,omitempty"`
}

type logWorkoutInput struct {
	Type    string   `json:"type,omitempty"`
	Minutes float64  `json:"minutes"`
	Kcal    *float64 `json:"kcal,omitempty"`
	Date    string   `json:"date,omitempty"`
}

type listWorkoutsInput struct {
	Start  string `json:"start"`
	End    string `json:"end"`
	SortBy string `json:"sort_by,omitempty"`
	Sort   string `json:"sort,omitempty"`
	Type   string `json:"type,omitempty"`
}

type workoutIDInput struct {
	ID string `json:"id"`
}

type correctWorkoutInput struct {
	ID        string   `json:"id"`
	Type      *string  `json:"type,omitempty"`
	Minutes   *float64 `json:"minutes,omitempty"`
	Kcal      *float64 `json:"kcal,omitempty"`
	ClearKcal *bool    `json:"clear_kcal,omitempty"`
}

type deleteWorkoutInput struct {
	ID     string `json:"id"`
	DryRun *bool  `json:"dry_run,omitempty"`
}

type dailySummaryInput struct {
	Date string `json:"date,omitempty"`
}

func dayOf(date string) string {
	if date == "" {
		return time.Now().In(healthcheck.IST()).Format("2006-01-02")
	}
	return date
}

func dryRun(in *bool) bool {
	return in == nil || *in
}

func main() {
	var err error
	store, err = healthcheck.FromEnv()
	if err != nil {
		log.Fatalf("health-check: %v", err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "health-check", Version: "2.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "log_food",
		Description: "Log one flat food entry: what eaten, free-text qty, optional estimated macros + note. Macros absent mean unknown, never zero; implausible values fail (kcal>100000, others>10000). datetime is RFC3339 (naive read as UTC), =now when absent."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in logFoodInput) (*mcp.CallToolResult, map[string]any, error) {
			dt := time.Now().UTC()
			if in.Datetime != "" {
				t, ok := healthcheck.ParseSyncTime(in.Datetime)
				if !ok {
					return fail(fmt.Errorf("bad datetime '%s' — use RFC3339 (naive read as UTC)", in.Datetime))
				}
				dt = t
			}
			macros := map[string]float64{}
			for k, v := range map[string]*float64{"kcal": in.Kcal, "protein": in.Protein, "carbs": in.Carbs, "fat": in.Fat, "fiber": in.Fiber} {
				if v != nil {
					macros[k] = *v
				}
			}
			day := dt.In(mustIST()).Format("2006-01-02")
			row, err := store.LogFood(ctx, day, dt, in.What, in.Qty, macros, in.Note)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "food": row})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_food",
		Description: "Fetch one food entry by id. Unknown ids fail."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in foodIDInput) (*mcp.CallToolResult, map[string]any, error) {
			row, err := store.GetFood(ctx, in.ID)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "food": row})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_food",
		Description: "List food entries between start/end dates (YYYY-MM-DD inclusive), capped at 500 rows (total echoed; truncated means narrow the dates). Sort_by names the field, sort the direction; rows without the sort field sort first in asc (unknown, not zero). What is exact match on the lowercased entry text. No match returns the empty page.",
		InputSchema: enumSchema(listFoodInput{},
			map[string][]string{"sort_by": healthcheck.FoodSortFields, "sort": sortEnum})},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listFoodInput) (*mcp.CallToolResult, map[string]any, error) {
			rows, total, err := store.ListFood(ctx, in.Start, in.End, in.SortBy, in.Sort, in.What)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "total": total,
				"truncated": int64(len(rows)) < total, "foods": rows,
				"sort_by": orDefault(in.SortBy, healthcheck.FoodSortFields[0]), "sort": orDefault(in.Sort, "asc")})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "correct_food",
		Description: "Patch one food entry by id: supplied fields replace, omitted keep; clear_macros names macros to remove (JSON null and omitted both decode absent, so null cannot clear). datetime is RFC3339 (naive read as UTC). Ask-gated like edit_transaction."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in correctFoodInput) (*mcp.CallToolResult, map[string]any, error) {
			macros := map[string]*float64{}
			for k, v := range map[string]*float64{"kcal": in.Kcal, "protein": in.Protein, "carbs": in.Carbs, "fat": in.Fat, "fiber": in.Fiber} {
				if v != nil {
					macros[k] = v
				}
			}
			var dt *time.Time
			if in.Datetime != nil {
				t, ok := healthcheck.ParseSyncTime(*in.Datetime)
				if !ok {
					return fail(fmt.Errorf("bad datetime '%s' — use RFC3339 (naive read as UTC)", *in.Datetime))
				}
				dt = &t
			}
			row, err := store.CorrectFood(ctx, in.ID, in.What, in.Qty, in.Note, macros, in.ClearMacros, dt)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "food": row})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_food",
		Description: "Hard-delete one food entry by id. Dry-runs by default: reports the row, deletes nothing unless dry_run=false. Permanent, no restore."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deleteFoodInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := store.DeleteFood(ctx, in.ID, dryRun(in.DryRun))
			if err != nil {
				return fail(err)
			}
			out["ok"] = true
			out["dry_run"] = dryRun(in.DryRun)
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "log_weight",
		Description: "Log body weight in kg (0 < kg <= 500; upserts one row per date). Re-logging is the correction."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in logWeightInput) (*mcp.CallToolResult, map[string]any, error) {
			w, err := store.LogWeight(ctx, dayOf(in.Date), in.Kg)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "weight": w})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_weight",
		Description: "List weights between start/end dates (YYYY-MM-DD inclusive), capped at 500 rows (total echoed; truncated means narrow the dates). No match returns the empty page.",
		InputSchema: enumSchema(listWeightInput{},
			map[string][]string{"sort_by": healthcheck.WeightSortFields, "sort": sortEnum})},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listWeightInput) (*mcp.CallToolResult, map[string]any, error) {
			rows, total, err := store.ListWeight(ctx, in.Start, in.End, in.SortBy, in.Sort)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "total": total,
				"truncated": int64(len(rows)) < total, "weights": rows,
				"sort_by": orDefault(in.SortBy, healthcheck.WeightSortFields[0]), "sort": orDefault(in.Sort, "asc")})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_weight",
		Description: "Hard-delete one date's weight row. Dry-runs by default."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deleteWeightInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := store.DeleteWeight(ctx, in.Date, dryRun(in.DryRun))
			if err != nil {
				return fail(err)
			}
			out["ok"] = true
			out["dry_run"] = dryRun(in.DryRun)
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "log_sleep",
		Description: "Log a manual sleep in hours (0 < hours <= 24, no start time — sync sessions carry starts). Date = morning of wake-up."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in logSleepInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := store.LogSleep(ctx, dayOf(in.Date), in.Hours)
			if err != nil {
				return fail(err)
			}
			out["ok"] = true
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_sleep",
		Description: "List per-day sleep (derived hours + full sessions) between start/end dates, capped at 500 day-docs (total echoed; a returned day always carries all sessions; value sorts apply within the page). No match returns the empty page.",
		InputSchema: enumSchema(listSleepInput{},
			map[string][]string{"sort_by": healthcheck.SleepSortFields, "sort": sortEnum})},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listSleepInput) (*mcp.CallToolResult, map[string]any, error) {
			rows, total, err := store.ListSleep(ctx, in.Start, in.End, in.SortBy, in.Sort)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "total": total,
				"truncated": int64(len(rows)) < total, "days": rows,
				"sort_by": orDefault(in.SortBy, healthcheck.SleepSortFields[0]), "sort": orDefault(in.Sort, "asc")})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "correct_sleep",
		Description: "Patch one sleep session: minutes or hours (one of the two — both-set fails). Ask-gated."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in correctSleepInput) (*mcp.CallToolResult, map[string]any, error) {
			if in.Minutes != nil && in.Hours != nil {
				return fail(fmt.Errorf("correct_sleep takes minutes or hours, not both"))
			}
			minutes := 0
			if in.Minutes != nil {
				minutes = *in.Minutes
			} else if in.Hours != nil {
				minutes = int(*in.Hours*60 + 0.5)
			} else {
				return fail(fmt.Errorf("correct_sleep needs minutes or hours"))
			}
			row, err := store.CorrectSleep(ctx, in.ID, minutes)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "sleep": row})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_sleep",
		Description: "Hard-delete one sleep session by entry id. Dry-runs by default."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deleteSleepInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := store.DeleteSleep(ctx, in.ID, dryRun(in.DryRun))
			if err != nil {
				return fail(err)
			}
			out["ok"] = true
			out["dry_run"] = dryRun(in.DryRun)
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "log_workout",
		Description: "Log one manual workout (type free text, lowercased on write, max 60 chars, empty becomes other; whole minutes required; kcal optional, absent means unknown never 0, max 100000). Sync workouts arrive deduplicated through the same store."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in logWorkoutInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := store.LogWorkout(ctx, dayOf(in.Date), in.Type, in.Minutes, in.Kcal)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "workout": out})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_workouts",
		Description: "List per-day workouts between start/end dates, capped at 500 day-docs (total echoed; a returned day always carries all workouts; value sorts apply within the page). Sort_by names the field, sort the direction; type is exact match on the lowered type (max 60 chars, empty means no filter). No match returns the empty page.",
		InputSchema: enumSchema(listWorkoutsInput{},
			map[string][]string{"sort_by": healthcheck.WorkoutSortFields, "sort": sortEnum})},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listWorkoutsInput) (*mcp.CallToolResult, map[string]any, error) {
			rows, total, err := store.ListWorkouts(ctx, in.Start, in.End, in.SortBy, in.Sort, in.Type)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "total": total,
				"truncated": int64(len(rows)) < total, "days": rows,
				"sort_by": orDefault(in.SortBy, healthcheck.WorkoutSortFields[0]), "sort": orDefault(in.Sort, "asc")})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "correct_workout",
		Description: "Patch one workout by entry id: supplied fields replace, omitted keep; type capped at 60 chars and cannot be cleared to empty; kcal clears with clear_kcal:true (setting both fails); minutes must be whole. Ask-gated."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in correctWorkoutInput) (*mcp.CallToolResult, map[string]any, error) {
			row, err := store.CorrectWorkout(ctx, in.ID, in.Type, in.Minutes, in.Kcal, in.ClearKcal != nil && *in.ClearKcal)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "workout": row})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_workout",
		Description: "Hard-delete one workout by entry id. Dry-runs by default."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deleteWorkoutInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := store.DeleteWorkout(ctx, in.ID, dryRun(in.DryRun))
			if err != nil {
				return fail(err)
			}
			out["ok"] = true
			out["dry_run"] = dryRun(in.DryRun)
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "daily_summary",
		Description: "Daily recap: food-derived cal-in totals with per-macro counts (a partial-macro day reads as a lower bound), workout_kcal + active_kcal as separate cal-out lines (no single net), weight, sleep, steps. Missing data reads as null on every scalar — never a measured zero."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in dailySummaryInput) (*mcp.CallToolResult, map[string]any, error) {
			sum, err := store.DailySummary(ctx, dayOf(in.Date))
			if err != nil {
				return fail(err)
			}
			sum["ok"] = true
			return result(sum)
		})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "health-check:", err)
		os.Exit(1)
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// mustIST is the day-bucketing zone: IST, via the shared healthcheck
// helper (fixed offset — LoadLocation depends on tzdata the minimal
// container may not have).
func mustIST() *time.Location {
	return healthcheck.IST()
}
