// Command cookbook is the reusable recipe library MCP server.
// Storage: MongoDB (cookbook_ingredients, cookbook_recipes, cookbook_cook_log).
// Permanent — never pruned. Runs over stdio for MCP clients.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"agento/internal/cookbook"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var store *cookbook.Store

func fail(err error) (*mcp.CallToolResult, map[string]any, error) {
	return nil, map[string]any{"ok": false, "error": err.Error()}, nil
}

func result(out map[string]any) (*mcp.CallToolResult, map[string]any, error) {
	b, _ := json.Marshal(out)
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
		StructuredContent: out,
	}, out, nil
}

// Input types are named rather than inline so `schema_test.go` can infer the schema from
// the same types `AddTool` publishes. Nothing here carried `omitempty`, so all twenty-five
// fields across eleven tools were advertised as required.
//
// Two kinds of optionality in this server, and the difference is the whole story:
//
//   - **the handler defaults it** — `add_recipe.servings` becomes 1, `list_cooks.limit`
//     becomes 50. The zero value IS the default, so requiring it asked the caller for a
//     value the server was about to invent.
//   - **the tool is a query with filters** — every `list_*` argument is documented as
//     "optionally filtered by…", and a required filter makes "show me everything" a call
//     that cannot be made.
//
// `update_recipe` is patch-shaped ("Empty/zero args are left alone"), so absent and zero
// mean the same thing there and every field but the identifier is optional.
type addIngredientInput struct {
	// store.AddIngredient rejects an empty name.
	Name string `json:"name"`
	Note string `json:"note,omitempty"`
}

type listIngredientsInput struct {
	// "optionally filtered by substring"
	Search string `json:"search,omitempty"`
}

type deleteIngredientInput struct {
	// The ingredient is identified by name or id.
	NameOrID string `json:"name_or_id"`
}

type addRecipeInput struct {
	// store.AddRecipe rejects an empty name.
	Name string `json:"name"`
	// "recipe needs at least one ingredient quantity"
	IngredientQtys map[string]any `json:"ingredient_qtys"`
	// "per_serving missing '%s' — ask the user for it": every macro key is required by
	// the store, so the map itself is not optional.
	PerServing map[string]any `json:"per_serving"`
	// The handler defaults 0 to 1.
	Servings float64  `json:"servings,omitempty"`
	Note     string   `json:"note,omitempty"`
	Tags     []string `json:"tags,omitempty"`
}

type getRecipeInput struct {
	NameOrID string `json:"name_or_id"`
}

type listRecipesInput struct {
	// "optionally filtered by name substring, tag, or ingredient name" — all three, so
	// listing nothing-filtered has to stay expressible.
	Search     string `json:"search,omitempty"`
	Tag        string `json:"tag,omitempty"`
	Ingredient string `json:"ingredient,omitempty"`
}

type updateRecipeInput struct {
	NameOrID string `json:"name_or_id"`
	// Patch semantics throughout: "Empty/zero args are left alone", per the description.
	// This tool is also the only way a recipe changes after a cook, so a field wrongly
	// marked required here is a field the agent is pushed to resend — which is how an
	// approved edit becomes a whole-record overwrite.
	Name           string         `json:"name,omitempty"`
	Servings       float64        `json:"servings,omitempty"`
	PerServing     map[string]any `json:"per_serving,omitempty"`
	IngredientQtys map[string]any `json:"ingredient_qtys,omitempty"`
	Note           string         `json:"note,omitempty"`
	Tags           []string       `json:"tags,omitempty"`
}

type deleteRecipeInput struct {
	NameOrID string `json:"name_or_id"`
}

type scaleRecipeInput struct {
	NameOrID string `json:"name_or_id"`
	// store.ScaleRecipe rejects servings <= 0, and unlike add_recipe nothing defaults it.
	Servings float64 `json:"servings"`
}

type logCookInput struct {
	// store.LogCook fails on an unknown recipe.
	Recipe string `json:"recipe"`
	// An empty date becomes today in the store.
	CookingNote    string `json:"cooking_note,omitempty"`
	AftertasteNote string `json:"aftertaste_note,omitempty"`
	Date           string `json:"date,omitempty"`
}

type listCooksInput struct {
	Recipe string `json:"recipe,omitempty"`
	// The handler defaults 0 to 50.
	Limit int `json:"limit,omitempty"`
}

func main() {
	var err error
	store, err = cookbook.FromEnv()
	if err != nil {
		log.Fatalf("cookbook: %v", err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "cookbook", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "add_ingredient",
		Description: "Add an ingredient name (optional note). Names are unique."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in addIngredientInput) (*mcp.CallToolResult, map[string]any, error) {
			ing, err := store.AddIngredient(ctx, in.Name, in.Note)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "ingredient": ing})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_ingredients",
		Description: "List ingredient names, optionally filtered by substring."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listIngredientsInput) (*mcp.CallToolResult, map[string]any, error) {
			rows, err := store.ListIngredients(ctx, in.Search)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "ingredients": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_ingredient",
		Description: "Delete an ingredient. Refused if any recipe still references it."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deleteIngredientInput) (*mcp.CallToolResult, map[string]any, error) {
			done, err := store.DeleteIngredient(ctx, in.NameOrID)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "deleted": done})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "add_recipe",
		Description: "Save a recipe. ingredient_qtys maps name-or-id -> free qty string. per_serving needs kcal/protein_g/carbs_g/fat_g/fiber_g."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in addRecipeInput) (*mcp.CallToolResult, map[string]any, error) {
			servings := in.Servings
			if servings == 0 {
				servings = 1
			}
			r, err := store.AddRecipe(ctx, in.Name, in.IngredientQtys, in.PerServing, servings, in.Note, in.Tags, "mcp")
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "recipe": r})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_recipe",
		Description: "Get one recipe by dish name or id."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in getRecipeInput) (*mcp.CallToolResult, map[string]any, error) {
			r, err := store.GetRecipe(ctx, in.NameOrID)
			if err != nil {
				return fail(err)
			}
			if r == nil {
				return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown recipe '%s'", in.NameOrID)})
			}
			return result(map[string]any{"ok": true, "recipe": r})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_recipes",
		Description: "List recipes, optionally filtered by name substring, tag, or ingredient name."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listRecipesInput) (*mcp.CallToolResult, map[string]any, error) {
			rows, err := store.ListRecipes(ctx, in.Search, in.Tag, in.Ingredient)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "recipes": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "update_recipe",
		Description: "Patch a recipe (the ONLY way a recipe changes after a cook — call only when the user approves). Empty/zero args are left unchanged."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in updateRecipeInput) (*mcp.CallToolResult, map[string]any, error) {
			patch := map[string]any{}
			if in.Name != "" {
				patch["name"] = in.Name
			}
			if in.Servings != 0 {
				patch["servings"] = in.Servings
			}
			if len(in.PerServing) > 0 {
				patch["per_serving"] = in.PerServing
			}
			if len(in.IngredientQtys) > 0 {
				patch["ingredient_qtys"] = in.IngredientQtys
			}
			if in.Note != "" {
				patch["note"] = in.Note
			}
			if len(in.Tags) > 0 {
				anyTags := make([]any, 0, len(in.Tags))
				for _, t := range in.Tags {
					anyTags = append(anyTags, t)
				}
				patch["tags"] = anyTags
			}
			r, err := store.UpdateRecipe(ctx, in.NameOrID, patch)
			if err != nil {
				return fail(err)
			}
			if r == nil {
				return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown recipe '%s'", in.NameOrID)})
			}
			return result(map[string]any{"ok": true, "recipe": r})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_recipe",
		Description: "Delete a recipe plus its cook-log rows."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deleteRecipeInput) (*mcp.CallToolResult, map[string]any, error) {
			deleted, logs, err := store.DeleteRecipe(ctx, in.NameOrID)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "deleted": deleted, "cook_logs": logs})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "scale_recipe",
		Description: "Scale macros to a target serving count. Pure math — no write."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in scaleRecipeInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := store.ScaleRecipe(ctx, in.NameOrID, in.Servings)
			if err != nil {
				return fail(err)
			}
			out["ok"] = true
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "log_cook",
		Description: "Log a cook: what differed (cooking_note) + what to improve (aftertaste_note). Never modifies the recipe."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in logCookInput) (*mcp.CallToolResult, map[string]any, error) {
			c, err := store.LogCook(ctx, in.Recipe, in.CookingNote, in.AftertasteNote, in.Date)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "cook": c})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_cooks",
		Description: "List cook-log rows, optionally for one recipe (newest first)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listCooksInput) (*mcp.CallToolResult, map[string]any, error) {
			limit := in.Limit
			if limit == 0 {
				limit = 50
			}
			rows, err := store.ListCooks(ctx, in.Recipe, limit)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "cooks": rows})
		})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "cookbook:", err)
		os.Exit(1)
	}
}
