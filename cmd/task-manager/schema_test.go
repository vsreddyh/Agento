package main

import (
	"reflect"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// The published input schema is what an agent reads before calling, so a field the
// server treats as optional but the schema calls required is a field the agent must
// invent a value for. That is not a cosmetic mismatch: `update_task` is nil-safe, so an
// agent forced to resend the whole record will overwrite a newer value with the stale one
// it read earlier.
//
// These are the required sets each tool actually enforces, taken from the handler and
// from store.Create's validation rather than from the schema — the schema is what this
// test checks.
//
// Note that "pointer" does not imply "optional": create_task rejects a task with no
// estimated_minutes and one with no parallelable, so those two are pointers and required.
// The reverse also holds — name is optional in update_task and is a plain string, because
// "" is how "untouched" is spelled. Optionality is a property of the contract, not of the
// Go type, which is exactly why it needs a test.
func TestToolInputSchemaRequired(t *testing.T) {
	cases := []struct {
		tool     string
		input    any
		required []string
	}{
		// store.Create rejects every one of these when absent.
		{"create_task", createTaskInput{},
			[]string{"name", "description", "due_date", "due_time",
				"estimated_minutes", "parallelable"}},
		// Every field has a documented default: state=open, overdue=false, no search,
		// limit=200. Requiring them makes the natural call — the one the tool's own
		// description shows — fail validation before the handler ever runs.
		{"list_tasks", listTasksInput{}, nil},
		{"get_task", getTaskInput{}, []string{"id"}},
		// Nil pointer = untouched, per the handler.
		{"update_task", updateTaskInput{}, []string{"id"}},
		{"complete_task", completeTaskInput{}, []string{"id"}},
		{"skip_task", skipTaskInput{}, []string{"id"}},
		{"reopen_task", reopenTaskInput{}, []string{"id"}},
		{"delete_task", deleteTaskInput{}, []string{"id"}},
		// Every filter is optional: the whole log is the default reading of
		// the question, and a tool that made the agent invent a filter to
		// ask "what happened?" would be a tool nobody calls.
		{"task_history", taskHistoryInput{}, nil},
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
			// A required field with no type information is a field the agent cannot
			// satisfy meaningfully, so an empty property set is as broken as a wrong
			// required list.
			for _, name := range want {
				prop, ok := schema.Properties[name]
				if !ok {
					t.Errorf("required field %q has no property in the schema", name)
					continue
				}
				// A pointer field is emitted as a union — `["null","integer"]` — so
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

// An optional field must still be *described*. Dropping a field's schema because it
// became optional would make it undiscoverable — the agent would not know the field
// exists, which is a different bug from the one this file exists to prevent.
func TestOptionalFieldsAreStillDocumented(t *testing.T) {
	optional := map[string][]string{
		"create_task": {"repeat_every", "repeat_unit", "repeat_custom", "repeat_rule"},
		"list_tasks":  {"state", "overdue", "search", "limit"},
		"update_task": {"name", "description", "due_date", "due_time",
			"estimated_minutes", "repeat_every", "repeat_unit", "repeat_custom",
			"repeat_rule", "expected_revision", "parallelable"},
		"task_history": {"task_id", "op", "since", "limit"},
	}

	inputs := map[string]any{
		"create_task":  createTaskInput{},
		"list_tasks":   listTasksInput{},
		"update_task":  updateTaskInput{},
		"task_history": taskHistoryInput{},
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

// schemaFor resolves the reflect.Type behind an input value and asks the SDK's own
// inferencer for its schema — the same call AddTool makes, so this asserts on the
// published contract rather than on a hand-written copy of it.
func schemaFor(t *testing.T, input any) *jsonschema.Schema {
	t.Helper()
	schema, err := jsonschema.ForType(reflect.TypeOf(input), nil)
	if err != nil {
		t.Fatalf("inferring schema: %v", err)
	}
	return schema
}

// A mistyped filter is an argument TYPE error, not a value the tool rounds
// or defaults. The SDK validates the arguments against the published schema
// before the handler runs, and the schema says string — so `op: 123` is
// rejected there, while `limit: 0` reaches the handler and takes the
// default.
//
// Every shape the tool description and the field docs name is pinned here,
// one row each: the docs are the contract the agent reads, and this test is
// what keeps them honest.
//
// Checked against the SDK's own validator rather than asserted from the
// doc, because the doc is what this test is meant to keep honest.
func TestNullIsAnArgumentTypeError(t *testing.T) {
	schema := schemaFor(t, taskHistoryInput{})
	resolved, err := schema.Resolve(nil)
	if err != nil {
		t.Fatalf("resolving schema: %v", err)
	}
	for _, args := range []map[string]any{
		{"limit": nil},
		{"op": nil},
		{"since": nil},
		{"task_id": nil},
		// An array where a string belongs, for the string filters.
		{"op": []any{"create"}},
		{"since": []any{"2026-10-21T00:00:00Z"}},
		{"task_id": []any{"0123456789abcdef01234567"}},
		// A number where a string belongs. The epoch number is the one
		// the docs warn about — a caller who means "since midnight" and
		// sends a number gets an error, not a date.
		{"since": 1760000000},
		{"op": 123},
		{"task_id": 123},
		// A string where the number belongs.
		{"limit": "200"},
		// A number with a fraction where the number belongs.
		{"limit": 200.5},
	} {
		v := map[string]any{}
		for k, val := range args {
			v[k] = val
		}
		if err := resolved.Validate(&v); err == nil {
			t.Errorf("%v: a mistyped filter must be rejected, not coerced", args)
		}
	}
	// And a value the tool CAN round is accepted: zero is a limit, not a
	// null.
	v := map[string]any{"limit": 0}
	if err := resolved.Validate(&v); err != nil {
		t.Errorf("limit 0 must be accepted: %v", err)
	}
	// A WHOLE-number float is an integer as far as JSON goes — 200.0 is
	// not a fraction, and the schema says integer — so it passes and
	// lands as 200. Pinned here so nobody "fixes" it into an error the
	// docs do not promise.
	w := map[string]any{"limit": 200.0}
	if err := resolved.Validate(&w); err != nil {
		t.Errorf("limit 200.0 must be accepted: %v", err)
	}
}
