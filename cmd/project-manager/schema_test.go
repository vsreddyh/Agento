package main

import (
	"reflect"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// The published input schema is what an agent reads before calling, so a field the
// handler treats as optional but the schema calls required is a field the agent must
// invent a value for. In `update_project` that is not cosmetic: the handler is nil-safe,
// so an agent forced to resend all three fields overwrites whatever wrote since — the
// lost update on the one tool whose job is changing one field.
//
// These required sets come from the handler and from store.Create's validation, not from
// the schema, because the schema is what this test checks.
func TestToolInputSchemaRequired(t *testing.T) {
	cases := []struct {
		tool     string
		input    any
		required []string
	}{
		// store.Create rejects an empty name and nothing else; an empty status
		// defaults to Todo by design.
		{"create_project", createProjectInput{}, []string{"name"}},
		// Every argument has a documented default.
		{"list_projects", listProjectsInput{}, nil},
		{"get_project", getProjectInput{}, []string{"id"}},
		// Nil pointer = untouched, per the handler.
		{"update_project", updateProjectInput{}, []string{"id"}},
		{"delete_project", deleteProjectInput{}, []string{"id"}},
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
				// A pointer field is emitted as a union — `["null","string"]` — so
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

// Optional must not mean invisible: a field dropped from the schema entirely is
// undiscoverable, which is a different bug from the one this file exists to prevent.
func TestOptionalFieldsAreStillDocumented(t *testing.T) {
	optional := map[string][]string{
		"create_project": {"status", "note"},
		"list_projects":  {"status", "search", "limit"},
		"update_project": {"name", "status", "note"},
	}
	inputs := map[string]any{
		"create_project": createProjectInput{},
		"list_projects":  listProjectsInput{},
		"update_project": updateProjectInput{},
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
