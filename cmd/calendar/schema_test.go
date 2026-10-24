package main

import (
	"reflect"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// Every tool requires its identifiers: an id-less get and a summary-less
// draft are calls that cannot be answered, so they must not validate.
// Filters stay optional so "what's coming up" is a call that can be made.
func TestToolInputSchemaRequired(t *testing.T) {
	cases := []struct {
		tool     string
		input    any
		required []string
	}{
		{"list_events", listEventsInput{}, nil},
		{"get_event", getEventInput{}, []string{"id"}},
		{"create_event_draft", createEventDraftInput{}, []string{"summary", "start", "end"}},
		{"create_event", createEventInput{}, nil},
		{"send_invite", createEventInput{}, nil},
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
