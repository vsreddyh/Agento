// Command cookbook is the recipe + ingredient book MCP server (#600).
// Storage: MongoDB (cookbook_ingredients, cookbook_recipes). Both books
// hold name + note (+tags on recipes) — no quantities, servings, macros
// or cook log. Permanent — never pruned. Runs over stdio
// for MCP clients.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"agento/internal/cookbook"
	"agento/internal/mcpschema"

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
// the same types `AddTool` publishes.
//
// Two kinds of optionality in this server, and the difference is the whole story:
//
//   - **the query is filtered** — every `list_*` argument is documented as
//     "optionally filtered by…", and a required filter makes "show me
//     everything" a call that cannot be made.
//
// `add_*` take the full record (name required, rest optional);
// `update_*` are patch-shaped ("Empty/zero args are left alone"), so absent
// and zero mean the same thing there and every field but the identifier is
// optional.
type addIngredientInput struct {
	// store.AddIngredient rejects an empty name.
	Name string `json:"name"`
	Note string `json:"note,omitempty"`
}

type getIngredientInput struct {
	NameOrID string `json:"name_or_id"`
}

type listIngredientsInput struct {
	// "optionally filtered by substring"
	Search string `json:"search,omitempty"`
	// Order, two enums: sort_by names the field (name|updatedAt, default
	// name), sort the direction (asc|desc, default asc). Both echoed back.
	SortBy string `json:"sort_by,omitempty"`
	Sort   string `json:"sort,omitempty"`
}

type updateIngredientInput struct {
	NameOrID string `json:"name_or_id"`
	// Patch semantics throughout: "Empty/zero args are left alone".
	Name string `json:"name,omitempty"`
	Note string `json:"note,omitempty"`
}

type deleteIngredientInput struct {
	// The ingredient is identified by name or id.
	NameOrID string `json:"name_or_id"`
}

type addRecipeInput struct {
	// store.AddRecipe rejects an empty name.
	Name string   `json:"name"`
	Note string   `json:"note,omitempty"`
	Tags []string `json:"tags,omitempty"`
}

type getRecipeInput struct {
	NameOrID string `json:"name_or_id"`
}

type listRecipesInput struct {
	// "optionally filtered by name substring or tag" — both, so listing
	// nothing-filtered has to stay expressible.
	Search string `json:"search,omitempty"`
	Tag    string `json:"tag,omitempty"`
	// Order, two enums: sort_by names the field (name|updatedAt, default
	// name), sort the direction (asc|desc, default asc). Both echoed back.
	SortBy string `json:"sort_by,omitempty"`
	Sort   string `json:"sort,omitempty"`
}

type updateRecipeInput struct {
	NameOrID string `json:"name_or_id"`
	// Patch semantics throughout: "Empty/zero args are left alone".
	Name string   `json:"name,omitempty"`
	Note string   `json:"note,omitempty"`
	Tags []string `json:"tags,omitempty"`
}

type deleteRecipeInput struct {
	NameOrID string `json:"name_or_id"`
}

func main() {
	var err error
	store, err = cookbook.FromEnv()
	if err != nil {
		log.Fatalf("cookbook: %v", err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "cookbook", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "add_ingredient",
		Description: "Add an ingredient name (optional note). Names are unique and case-sensitive — duplicates error. Names/notes are trimmed (whitespace-only counts as empty); notes over 2000 bytes are truncated on a UTF-8 boundary."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in addIngredientInput) (*mcp.CallToolResult, map[string]any, error) {
			ing, err := store.AddIngredient(ctx, in.Name, in.Note)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "ingredient": ing})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_ingredient",
		Description: "Get one ingredient by exact name (case-sensitive) or id. Unknown targets return ok:false."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in getIngredientInput) (*mcp.CallToolResult, map[string]any, error) {
			ing, err := store.GetIngredient(ctx, in.NameOrID)
			if err != nil {
				return fail(err)
			}
			if ing == nil {
				return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown ingredient '%s'", in.NameOrID)})
			}
			return result(map[string]any{"ok": true, "ingredient": ing})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_ingredients",
		Description: "List ingredients, optionally filtered by case-insensitive substring (trimmed; blank/whitespace-only means absent, unfiltered — up to 500; a full page may hide more — narrow with search). sort_by names the field (name|updatedAt, default name), sort the direction (asc|desc, default asc); unknown values are rejected on both layers, and the applied pair is echoed in the response. Omit a field to default it over MCP — the published schema enum is exact, so a blank string is rejected before the handler; blank/whitespace-only means the defaults for direct store callers. sort_by is case-sensitive (updatedAt), sort case-insensitive.",
		InputSchema: mcpschema.EnumSchema(listIngredientsInput{},
			map[string][]string{"sort_by": cookbook.IngredientSortFields, "sort": {"asc", "desc"}})},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listIngredientsInput) (*mcp.CallToolResult, map[string]any, error) {
			// Trimmed up front so whitespace-only means absent (the default),
			// like every other filter on this server.
			in.SortBy = strings.TrimSpace(in.SortBy)
			in.Sort = strings.TrimSpace(in.Sort)
			rows, err := store.ListIngredients(ctx, in.Search, in.SortBy, in.Sort)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows),
				"sort_by": orDefault(in.SortBy, cookbook.IngredientSortFields[0]), "sort": orDefault(normDir(in.Sort), "asc"),
				"ingredients": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "update_ingredient",
		Description: "Patch an ingredient's name or note (at least one required). Names/notes are trimmed (whitespace-only counts as empty); empty args are left unchanged, notes cannot be cleared to empty. Renames to an existing name error; unknown targets return ok:false. Over-cap notes are truncated UTF-8-safe."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in updateIngredientInput) (*mcp.CallToolResult, map[string]any, error) {
			patch := map[string]any{}
			if in.Name != "" {
				patch["name"] = in.Name
			}
			if in.Note != "" {
				patch["note"] = in.Note
			}
			ing, err := store.UpdateIngredient(ctx, in.NameOrID, patch)
			if err != nil {
				return fail(err)
			}
			if ing == nil {
				return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown ingredient '%s'", in.NameOrID)})
			}
			return result(map[string]any{"ok": true, "ingredient": ing})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_ingredient",
		Description: "Delete an ingredient by exact name (case-sensitive, trimmed) or id. Idempotent: unknown names return ok:true with deleted:false."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deleteIngredientInput) (*mcp.CallToolResult, map[string]any, error) {
			done, err := store.DeleteIngredient(ctx, in.NameOrID)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "deleted": done})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "add_recipe",
		Description: "Save a recipe: a name with an optional note and tags. Names are unique and case-sensitive — duplicates error. Names/notes/tags are trimmed (whitespace-only counts as empty). No quantities, servings or macros — just a named note. Notes over 2000 bytes (tags over 20 entries of 40 bytes) are truncated on a UTF-8 boundary."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in addRecipeInput) (*mcp.CallToolResult, map[string]any, error) {
			r, err := store.AddRecipe(ctx, in.Name, in.Note, in.Tags)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "recipe": r})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_recipe",
		Description: "Get one recipe by exact dish name (case-sensitive) or id. Unknown targets return ok:false."},
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
		Description: "List recipes, optionally filtered by name substring (case-insensitive, trimmed) or tag (exact, case-sensitive, trimmed); blank/whitespace-only means absent, unfiltered. Up to 200; a full page may hide more — narrow with search. sort_by names the field (name|updatedAt, default name), sort the direction (asc|desc, default asc); unknown values are rejected on both layers, and the applied pair is echoed in the response. Omit a field to default it over MCP — the published schema enum is exact, so a blank string is rejected before the handler; blank/whitespace-only means the defaults for direct store callers. sort_by is case-sensitive (updatedAt), sort case-insensitive.",
		InputSchema: mcpschema.EnumSchema(listRecipesInput{},
			map[string][]string{"sort_by": cookbook.RecipeSortFields, "sort": {"asc", "desc"}})},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listRecipesInput) (*mcp.CallToolResult, map[string]any, error) {
			in.SortBy = strings.TrimSpace(in.SortBy)
			in.Sort = strings.TrimSpace(in.Sort)
			rows, err := store.ListRecipes(ctx, in.Search, in.Tag, in.SortBy, in.Sort)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows),
				"sort_by": orDefault(in.SortBy, cookbook.RecipeSortFields[0]), "sort": orDefault(normDir(in.Sort), "asc"),
				"recipes": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "update_recipe",
		Description: "Patch a recipe's name, note or tags (at least one required; tags replace the whole list, not merged). Names/notes/tags are trimmed (whitespace-only counts as empty); empty args are left unchanged, note/tags cannot be cleared to empty. Renames to an existing name error; unknown targets return ok:false. Over-cap values are truncated UTF-8-safe."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in updateRecipeInput) (*mcp.CallToolResult, map[string]any, error) {
			patch := map[string]any{}
			if in.Name != "" {
				patch["name"] = in.Name
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
		Description: "Delete a recipe by exact name (case-sensitive, trimmed) or id. Idempotent: unknown names return ok:true with deleted:false."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deleteRecipeInput) (*mcp.CallToolResult, map[string]any, error) {
			done, err := store.DeleteRecipe(ctx, in.NameOrID)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "deleted": done})
		})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "cookbook:", err)
		os.Exit(1)
	}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// normDir folds an already-trimmed sort direction for the echo, matching the
// store's case-insensitive match. sort_by echoes raw (trimmed): the field
// names are case-sensitive.
func normDir(v string) string { return strings.ToLower(v) }
