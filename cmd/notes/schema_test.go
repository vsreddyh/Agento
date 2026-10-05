package main

import (
	"reflect"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// Every optional field carries `omitempty`, because a note is mostly optional
// beyond its title, and a required-but-defaulted field is the agent being asked for
// a value the server is about to invent. The filters must stay optional: a required
// filter makes "show me everything" a call that cannot be made.
func TestToolInputSchemaRequired(t *testing.T) {
	cases := []struct {
		tool     string
		input    any
		required []string
	}{
		// store.AddNote rejects an empty title.
		{"create_note", addNoteInput{}, []string{"title"}},
		// Notes are not addressable by title, so the id is the only identifier and it
		// is required.
		{"get_note", getNoteInput{}, []string{"id"}},
		// "optionally filtered by tag and/or to pinned only"
		{"list_notes", listNotesInput{}, nil},
		// store.SearchNotes rejects empty text; the handler defaults limit to 20.
		{"search_notes", searchNotesInput{}, []string{"text"}},
		// Patch-shaped: "Absent args are left alone".
		{"update_note", updateNoteInput{}, []string{"id"}},
		{"delete_note", deleteNoteInput{}, []string{"id"}},
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

// Optional must not mean invisible. A caller that cannot see `list_notes.tag` has no
// way to ask for notes by tag at all.
func TestOptionalFieldsAreStillDocumented(t *testing.T) {
	optional := map[string][]string{
		"create_note":  {"body", "tags", "pinned"},
		"list_notes":   {"tag", "pinned_only", "limit"},
		"search_notes": {"limit"},
		"update_note":  {"title", "body", "tags", "pinned"},
	}
	inputs := map[string]any{
		"create_note":  addNoteInput{},
		"list_notes":   listNotesInput{},
		"search_notes": searchNotesInput{},
		"update_note":  updateNoteInput{},
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
// makes, so this asserts the published contract rather than a hand-written copy.
func schemaFor(t *testing.T, input any) *jsonschema.Schema {
	t.Helper()
	schema, err := jsonschema.ForType(reflect.TypeOf(input), nil)
	if err != nil {
		t.Fatalf("inferring schema: %v", err)
	}
	return schema
}
