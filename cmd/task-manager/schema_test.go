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
		{"reopen_task", reopenTaskInput{}, []string{"id"}},
		{"delete_task", deleteTaskInput{}, []string{"id"}},
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
	}

	inputs := map[string]any{
		"create_task": createTaskInput{},
		"list_tasks":  listTasksInput{},
		"update_task": updateTaskInput{},
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
