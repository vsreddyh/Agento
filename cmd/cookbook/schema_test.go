package main

import (
	"reflect"
	"slices"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
)

// Nothing in this file carried `omitempty`, so all twenty-five fields across eleven tools
// were advertised as required. The two shapes of optionality here are the handler
// defaulting a zero value (`add_recipe.servings` → 1, `list_cooks.limit` → 50) and the
// query tools taking filters their own descriptions call optional — and a required filter
// makes "show me everything" a call that cannot be made.
func TestToolInputSchemaRequired(t *testing.T) {
	cases := []struct {
		tool     string
		input    any
		required []string
	}{
		{"add_ingredient", addIngredientInput{}, []string{"name"}},
		{"list_ingredients", listIngredientsInput{}, nil},
		{"delete_ingredient", deleteIngredientInput{}, []string{"name_or_id"}},
		// store.AddRecipe rejects an empty name, an empty ingredient list, and a
		// per_serving missing any macro key. servings is NOT in this list: the handler
		// turns 0 into 1.
		{"add_recipe", addRecipeInput{}, []string{"name", "ingredient_qtys", "per_serving"}},
		{"get_recipe", getRecipeInput{}, []string{"name_or_id"}},
		// "optionally filtered by name substring, tag, or ingredient name"
		{"list_recipes", listRecipesInput{}, nil},
		// Patch-shaped: "Empty/zero args are left alone".
		{"update_recipe", updateRecipeInput{}, []string{"name_or_id"}},
		{"delete_recipe", deleteRecipeInput{}, []string{"name_or_id"}},
		// store.ScaleRecipe rejects servings <= 0 and, unlike add_recipe, nothing
		// defaults it first.
		{"scale_recipe", scaleRecipeInput{}, []string{"name_or_id", "servings"}},
		// store.LogCook fails on an unknown recipe; an empty date becomes today.
		{"log_cook", logCookInput{}, []string{"recipe"}},
		{"list_cooks", listCooksInput{}, nil},
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
		"add_ingredient":   {"note"},
		"list_ingredients": {"search"},
		"add_recipe":       {"servings", "note", "tags"},
		"list_recipes":     {"search", "tag", "ingredient"},
		"update_recipe":    {"name", "servings", "per_serving", "ingredient_qtys", "note", "tags"},
		"log_cook":         {"cooking_note", "aftertaste_note", "date"},
		"list_cooks":       {"recipe", "limit"},
	}
	inputs := map[string]any{
		"add_ingredient":   addIngredientInput{},
		"list_ingredients": listIngredientsInput{},
		"add_recipe":       addRecipeInput{},
		"list_recipes":     listRecipesInput{},
		"update_recipe":    updateRecipeInput{},
		"log_cook":         logCookInput{},
		"list_cooks":       listCooksInput{},
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
