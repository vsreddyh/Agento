package main

import (
	"reflect"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// Nothing in this server carried `omitempty`, so the SDK published all eighteen fields
// across nine tools as required. That included arguments the handler actively defaults —
// `delete_meals.date` (defaults to today), `prune_old.days` (0 means 30) and
// `prune_old.dry_run` (nil means dry run, the safe default for a delete) — so the schema
// required callers to supply values the server was about to invent anyway.
//
// These required sets come from store validation and from each tool's own description,
// which is where the two disagree: `query_meals.start`/`end` are required because
// `mustDay` rejects "", while `log_meal.date` is optional because `dayOf` fills it in.
func TestToolInputSchemaRequired(t *testing.T) {
	cases := []struct {
		tool     string
		input    any
		required []string
	}{
		// checkItems rejects an absent items list. `description` is not validated by
		// the store, so it is deliberately NOT in this list — a schema stricter than
		// the server rejects calls the server would have accepted.
		{"log_meal", logMealInput{}, []string{"items"}},
		{"fix_last_meal", fixLastMealInput{}, []string{"items"}},
		// mustDay("") fails, so these two cannot be defaulted.
		{"query_meals", queryMealsInput{}, []string{"start", "end"}},
		// dayOf("") is today.
		{"delete_meals", deleteMealsInput{}, nil},
		// The store rejects an implausible weight or duration, so 0 cannot mean unset.
		{"log_weight", logWeightInput{}, []string{"kg"}},
		{"log_sleep", logSleepInput{}, []string{"hours"}},
		// `type` is NOT in this list: store.LogWorkout defaults "" to "workout", so
		// requiring it would make the schema stricter than the server.
		{"log_workout", logWorkoutInput{}, []string{"minutes"}},
		{"daily_summary", dailySummaryInput{}, nil},
		// days=0 means 30 in the handler; dry_run=nil means true.
		{"prune_old", pruneOldInput{}, nil},
	}

	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			schema := schemaFor(t, c.input)
			got := slices.Clone(schema.Required)
			slices.Sort(got)
			want := slices.Clone(c.required)
			slices.Sort(want)

			if !slices.Equal(got, want) {
				t.Errorf("required = %v, want %v", got, want)
			}
			for _, name := range want {
				prop, ok := schema.Properties[name]
				if !ok {
					t.Errorf("required field %q has no property in the schema", name)
					continue
				}
				// A pointer field is emitted as a union — `["null","boolean"]` — so
				// Type is empty and Types is populated. Checking Type alone would fail
				// every optional-typed field in this file.
				if prop.Type == "" && len(prop.Types) == 0 && prop.Ref == "" {
					t.Errorf("required field %q has no type (Type=%q Types=%v Ref=%q)",
						name, prop.Type, prop.Types, prop.Ref)
				}
			}
		})
	}
}

// Optional must not mean invisible, and it must not mean "the handler now defaults it to
// something destructive": prune_old is the one tool here that deletes, and `dry_run` is
// the field that makes it safe. Both directions are checked.
func TestOptionalFieldsAreStillDocumented(t *testing.T) {
	optional := map[string][]string{
		"log_meal":      {"description", "date"},
		"fix_last_meal": {"description"},
		"delete_meals":  {"date"},
		"log_weight":    {"date"},
		"log_sleep":     {"date"},
		"log_workout":   {"type", "kcal", "date"},
		"daily_summary": {"date"},
		"prune_old":     {"days", "dry_run"},
	}
	inputs := map[string]any{
		"log_meal":      logMealInput{},
		"fix_last_meal": fixLastMealInput{},
		"delete_meals":  deleteMealsInput{},
		"log_weight":    logWeightInput{},
		"log_sleep":     logSleepInput{},
		"log_workout":   logWorkoutInput{},
		"daily_summary": dailySummaryInput{},
		"prune_old":     pruneOldInput{},
	}

	for tool, names := range optional {
		t.Run(tool, func(t *testing.T) {
			schema := schemaFor(t, inputs[tool])
			for _, name := range names {
				if slices.Contains(schema.Required, name) {
					t.Errorf("%s is required but should be optional", name)
				}
				if _, ok := schema.Properties[name]; !ok {
					t.Errorf("%s is missing from the schema entirely", name)
				}
			}
		})
	}
}

// schemaFor asks the SDK's own inferencer for a type's schema — the same call AddTool
// makes, so this asserts the published contract rather than a hand-written copy of it.
func schemaFor(t *testing.T, input any) *jsonschema.Schema {
	t.Helper()
	schema, err := jsonschema.ForType(reflect.TypeOf(input), nil)
	if err != nil {
		t.Fatalf("inferring schema: %v", err)
	}
	return schema
}
