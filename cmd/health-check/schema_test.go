package main

import (
	"reflect"
	"slices"
	"testing"

	"agento/internal/healthcheck"

	"github.com/google/jsonschema-go/jsonschema"
)

// Required sets come from store validation: required when the store
// rejects the call on a zero value, optional otherwise (omitempty is the
// only switch). Enums and IDs pin the second half of the contract below.
func TestToolInputSchemaRequired(t *testing.T) {
	cases := []struct {
		tool     string
		input    any
		required []string
	}{
		{"log_food", logFoodInput{}, []string{"what"}},
		{"get_food", foodIDInput{}, []string{"id"}},
		{"list_food", listFoodInput{}, []string{"start", "end"}},
		{"correct_food", correctFoodInput{}, []string{"id"}},
		{"delete_food", deleteFoodInput{}, []string{"id"}},
		{"log_weight", logWeightInput{}, []string{"kg"}},
		{"list_weight", listWeightInput{}, []string{"start", "end"}},
		{"delete_weight", deleteWeightInput{}, []string{"date"}},
		{"log_sleep", logSleepInput{}, []string{"hours"}},
		{"list_sleep", listSleepInput{}, []string{"start", "end"}},
		{"correct_sleep", correctSleepInput{}, []string{"id"}},
		{"delete_sleep", deleteSleepInput{}, []string{"id"}},
		{"log_workout", logWorkoutInput{}, []string{"minutes"}},
		{"list_workouts", listWorkoutsInput{}, []string{"start", "end"}},
		{"correct_workout", correctWorkoutInput{}, []string{"id"}},
		{"delete_workout", deleteWorkoutInput{}, []string{"id"}},
		{"daily_summary", dailySummaryInput{}, nil},
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
				if prop.Type == "" && len(prop.Types) == 0 && prop.Ref == "" {
					t.Errorf("required field %q has no type (Type=%q Types=%v Ref=%q)",
						name, prop.Type, prop.Types, prop.Ref)
				}
			}
		})
	}
}

// Optional must not mean invisible: every optional field is present in the
// schema and not required.
func TestOptionalFieldsAreStillDocumented(t *testing.T) {
	optional := map[string][]string{
		"log_food":        {"qty", "kcal", "protein", "carbs", "fat", "fiber", "note", "datetime"},
		"get_food":        {},
		"list_food":       {"sort_by", "sort", "what"},
		"correct_food":    {"what", "qty", "kcal", "protein", "carbs", "fat", "fiber", "note", "clear_macros", "datetime"},
		"delete_food":     {"dry_run"},
		"log_weight":      {"date"},
		"list_weight":     {"sort_by", "sort"},
		"delete_weight":   {"dry_run"},
		"log_sleep":       {"date"},
		"list_sleep":      {"sort_by", "sort"},
		"correct_sleep":   {"minutes", "hours"},
		"delete_sleep":    {"dry_run"},
		"log_workout":     {"type", "kcal", "date"},
		"list_workouts":   {"sort_by", "sort", "type"},
		"correct_workout": {"type", "minutes", "kcal", "clear_kcal"},
		"delete_workout":  {"dry_run"},
		"daily_summary":   {"date"},
	}
	inputs := map[string]any{
		"log_food": logFoodInput{}, "get_food": foodIDInput{},
		"list_food": listFoodInput{}, "correct_food": correctFoodInput{},
		"delete_food": deleteFoodInput{}, "log_weight": logWeightInput{},
		"list_weight": listWeightInput{}, "delete_weight": deleteWeightInput{},
		"log_sleep": logSleepInput{}, "list_sleep": listSleepInput{},
		"correct_sleep": correctSleepInput{}, "delete_sleep": deleteSleepInput{},
		"log_workout": logWorkoutInput{}, "list_workouts": listWorkoutsInput{},
		"correct_workout": correctWorkoutInput{}, "delete_workout": deleteWorkoutInput{},
		"daily_summary": dailySummaryInput{},
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

// Enums are published in the schema (not just validated server-side) so
// unknown values throw before the handler. Each list's sort_by names only
// its own fields; sort is asc|desc everywhere.
func TestSortEnumsPublished(t *testing.T) {
	cases := []struct {
		input  any
		sortBy []string
	}{
		{listFoodInput{}, healthcheck.FoodSortFields},
		{listWeightInput{}, healthcheck.WeightSortFields},
		{listSleepInput{}, healthcheck.SleepSortFields},
		{listWorkoutsInput{}, healthcheck.WorkoutSortFields},
	}
	for _, c := range cases {
		name := reflect.TypeOf(c.input).Name()
		t.Run(name, func(t *testing.T) {
			schema := enumSchema(c.input, map[string][]string{
				"sort_by": c.sortBy, "sort": {"asc", "desc"}})
			for field, want := range map[string][]string{"sort_by": c.sortBy, "sort": {"asc", "desc"}} {
				prop, ok := schema.Properties[field]
				if !ok {
					t.Fatalf("%s missing from schema", field)
				}
				var got []string
				for _, v := range prop.Enum {
					s, ok := v.(string)
					if !ok {
						t.Fatalf("%s enum value %v is not a string", field, v)
					}
					got = append(got, s)
				}
				slices.Sort(got)
				wantSorted := slices.Clone(want)
				slices.Sort(wantSorted)
				if !slices.Equal(got, wantSorted) {
					t.Errorf("%s enum = %v, want %v", field, got, wantSorted)
				}
			}
		})
	}
}

// Dry-run is a pointer so nil means dry run: every delete defaults safe.
func TestDeleteDryRunDefaultsSafe(t *testing.T) {
	for _, in := range []any{deleteFoodInput{}, deleteWeightInput{}, deleteSleepInput{}, deleteWorkoutInput{}} {
		schema := schemaFor(t, in)
		prop, ok := schema.Properties["dry_run"]
		if !ok {
			t.Fatalf("%T has no dry_run in schema", in)
		}
		// Pointer emits a union including null; a bare boolean would not.
		hasNull := false
		for _, ty := range prop.Types {
			if ty == "null" {
				hasNull = true
			}
		}
		if len(prop.Types) > 0 && !hasNull {
			t.Errorf("%T dry_run should be nullable (nil = dry run)", in)
		}
	}
}

// schemaFor asks the SDK's own inferencer for a type's schema — the same
// call AddTool makes, so this asserts the published contract rather than a
// hand-written copy of it.
func schemaFor(t *testing.T, input any) *jsonschema.Schema {
	t.Helper()
	schema, err := jsonschema.ForType(reflect.TypeOf(input), nil)
	if err != nil {
		t.Fatalf("inferring schema: %v", err)
	}
	return schema
}
