package main

import (
	"reflect"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// create_report's only required field is the title; everything else the
// server defaults (body "", source "", read/starred false). The list filter
// stays optional: a required filter makes "show me everything" uncallable.
func TestToolInputSchemaRequired(t *testing.T) {
	cases := []struct {
		tool     string
		input    any
		required []string
	}{
		// store.CreateReport rejects an empty title.
		{"create_report", createReportInput{}, []string{"title"}},
		// Reports are not addressable by title, so the id is required.
		{"get_report", getReportInput{}, []string{"id"}},
		// Newest-first, no filters: v1 has no search.
		{"list_reports", listReportsInput{}, nil},
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

// Optional must not mean invisible: a caller that cannot see
// `create_report.body_md` has no way to file a body at all.
func TestOptionalFieldsAreStillDocumented(t *testing.T) {
	optional := map[string][]string{
		"create_report": {"body_md", "source"},
		"list_reports":  {"limit"},
	}
	inputs := map[string]any{
		"create_report": createReportInput{},
		"list_reports":  listReportsInput{},
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

// schemaFor asks the SDK's own inferencer for a type's schema — the same call
// AddTool makes, so this asserts the published contract rather than a
// hand-written copy.
func schemaFor(t *testing.T, input any) *jsonschema.Schema {
	t.Helper()
	schema, err := jsonschema.ForType(reflect.TypeOf(input), nil)
	if err != nil {
		t.Fatalf("inferring schema: %v", err)
	}
	return schema
}
