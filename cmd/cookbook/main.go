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

func main() {
	var err error
	store, err = cookbook.FromEnv()
	if err != nil {
		log.Fatalf("cookbook: %v", err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "cookbook", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "add_ingredient",
		Description: "Add an ingredient name (optional note). Names are unique."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Name string `json:"name"`
			Note string `json:"note"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			ing, err := store.AddIngredient(ctx, in.Name, in.Note)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "ingredient": ing})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_ingredients",
		Description: "List ingredient names, optionally filtered by substring."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Search string `json:"search"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			rows, err := store.ListIngredients(ctx, in.Search)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "ingredients": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_ingredient",
		Description: "Delete an ingredient. Refused if any recipe still references it."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			NameOrID string `json:"name_or_id"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			done, err := store.DeleteIngredient(ctx, in.NameOrID)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "deleted": done})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "add_recipe",
		Description: "Save a recipe. ingredient_qtys maps name-or-id -> free qty string. per_serving needs kcal/protein_g/carbs_g/fat_g/fiber_g."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Name           string         `json:"name"`
			IngredientQtys map[string]any `json:"ingredient_qtys"`
			PerServing     map[string]any `json:"per_serving"`
			Servings       float64        `json:"servings"`
			Note           string         `json:"note"`
			Tags           []string       `json:"tags"`
		}) (*mcp.CallToolResult, map[string]any, error) {
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
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			NameOrID string `json:"name_or_id"`
		}) (*mcp.CallToolResult, map[string]any, error) {
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
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Search     string `json:"search"`
			Tag        string `json:"tag"`
			Ingredient string `json:"ingredient"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			rows, err := store.ListRecipes(ctx, in.Search, in.Tag, in.Ingredient)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "recipes": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "update_recipe",
		Description: "Patch a recipe (the ONLY way a recipe changes after a cook — call only when the user approves). Empty/zero args are left unchanged."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			NameOrID      string         `json:"name_or_id"`
			Name          string         `json:"name"`
			Servings      float64        `json:"servings"`
			PerServing    map[string]any `json:"per_serving"`
			IngredientQtys map[string]any `json:"ingredient_qtys"`
			Note          string         `json:"note"`
			Tags          []string       `json:"tags"`
		}) (*mcp.CallToolResult, map[string]any, error) {
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
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			NameOrID string `json:"name_or_id"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			deleted, logs, err := store.DeleteRecipe(ctx, in.NameOrID)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "deleted": deleted, "cook_logs": logs})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "scale_recipe",
		Description: "Scale macros to a target serving count. Pure math — no write."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			NameOrID string  `json:"name_or_id"`
			Servings float64 `json:"servings"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			out, err := store.ScaleRecipe(ctx, in.NameOrID, in.Servings)
			if err != nil {
				return fail(err)
			}
			out["ok"] = true
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "log_cook",
		Description: "Log a cook: what differed (cooking_note) + what to improve (aftertaste_note). Never modifies the recipe."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Recipe         string `json:"recipe"`
			CookingNote    string `json:"cooking_note"`
			AftertasteNote string `json:"aftertaste_note"`
			Date           string `json:"date"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			c, err := store.LogCook(ctx, in.Recipe, in.CookingNote, in.AftertasteNote, in.Date)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "cook": c})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_cooks",
		Description: "List cook-log rows, optionally for one recipe (newest first)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Recipe string `json:"recipe"`
			Limit  int    `json:"limit"`
		}) (*mcp.CallToolResult, map[string]any, error) {
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
