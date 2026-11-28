package main

import (
	"reflect"
	"slices"
	"testing"

	"agento/internal/cookbook"
	"agento/internal/mcpschema"

	"github.com/google/jsonschema-go/jsonschema"
)

// Ten tools, two books (#600): every tool takes its record (add_*) or an
// identifier (everything else). The only optionality left is the query
// filters — a required filter makes "show me everything" a call that
// cannot be made — plus the patch-shaped updates ("Empty/zero args are
// left alone").
func TestToolInputSchemaRequired(t *testing.T) {
	cases := []struct {
		tool     string
		input    any
		required []string
	}{
		{"add_ingredient", addIngredientInput{}, []string{"name"}},
		{"get_ingredient", getIngredientInput{}, []string{"name_or_id"}},
		{"list_ingredients", listIngredientsInput{}, nil},
		{"update_ingredient", updateIngredientInput{}, []string{"name_or_id"}},
		{"delete_ingredient", deleteIngredientInput{}, []string{"name_or_id"}},
		// store.AddRecipe rejects an empty name; note and tags are optional.
		{"add_recipe", addRecipeInput{}, []string{"name"}},
		{"get_recipe", getRecipeInput{}, []string{"name_or_id"}},
		// "optionally filtered by name substring or tag"
		{"list_recipes", listRecipesInput{}, nil},
		// Patch-shaped: "Empty/zero args are left alone".
		{"update_recipe", updateRecipeInput{}, []string{"name_or_id"}},
		{"delete_recipe", deleteRecipeInput{}, []string{"name_or_id"}},
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
				// A pointer field is emitted as a union — `["null","object"]` — so Type
				// is empty and Types is populated. Checking Type alone would fail every
				// optional-typed field in this file.
				if prop.Type == "" && len(prop.Types) == 0 && prop.Ref == "" {
					t.Errorf("required field %q has no type (Type=%q Types=%v Ref=%q)",
						name, prop.Type, prop.Types, prop.Ref)
				}
			}
		})
	}
}

// Optional must not mean invisible. The filters matter most: a caller that cannot see
// `list_recipes.tag` has no way to ask for recipes by tag at all.
func TestOptionalFieldsAreStillDocumented(t *testing.T) {
	optional := map[string][]string{
		"add_ingredient":    {"note"},
		"list_ingredients":  {"search", "sort_by", "sort"},
		"update_ingredient": {"name", "note"},
		"add_recipe":        {"note", "tags"},
		"list_recipes":      {"search", "tag", "sort_by", "sort"},
		"update_recipe":     {"name", "note", "tags"},
	}
	inputs := map[string]any{
		"add_ingredient":    addIngredientInput{},
		"list_ingredients":  listIngredientsInput{},
		"update_ingredient": updateIngredientInput{},
		"add_recipe":        addRecipeInput{},
		"list_recipes":      listRecipesInput{},
		"update_recipe":     updateRecipeInput{},
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

// Enums are published in the schema (not just validated server-side) so
// unknown values throw before the handler. Each list's sort_by names only
// its own fields; sort is asc|desc everywhere — the same two-enum convention
// as query_transactions.
func TestSortEnumsPublished(t *testing.T) {
	cases := []struct {
		tool   string
		input  any
		sortBy []string
	}{
		{"list_ingredients", listIngredientsInput{}, cookbook.IngredientSortFields},
		{"list_recipes", listRecipesInput{}, cookbook.RecipeSortFields},
	}
	for _, c := range cases {
		t.Run(c.tool, func(t *testing.T) {
			schema := mcpschema.EnumSchema(c.input, map[string][]string{
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
