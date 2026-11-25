package main

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Category and account-lifecycle tools. Reads run free; every write prompts
// (create/update via their own ask patterns, delete via *delete*).

type editAccountInput struct {
	// A name or id hex (trimmed, like archive_account); the stored name is
	// echoed back, never the raw input.
	Name string `json:"name"`
	// New name (trimmed, must be unique). Absent means no rename.
	NewName string `json:"new_name,omitempty"`
	// New type (cash|bank|card|wallet|other, case-insensitive). Absent means
	// no retype.
	Type string `json:"type,omitempty"`
	// Corrected opening balance in paise (int64, may be negative like the
	// create seed). Absent means the seed is untouched. The derived balance
	// moves by exactly the delta and no transaction row is written.
	OpeningBalancePaise *int64 `json:"opening_balance_paise,omitempty"`
}

type createCategoryInput struct {
	// Trimmed and lowercased (Cash Groceries and groceries are the same
	// category); must be unique. 'other' is reserved and already seeded.
	Name string `json:"name"`
}

type listCategoriesInput struct{}

type updateCategoryInput struct {
	// The category to rename (trimmed, case-insensitive).
	Name string `json:"name"`
	// The new name (trimmed, lowercased, unique). 'other' can be neither
	// renamed nor renamed to.
	NewName string `json:"new_name"`
}

type deleteCategoryInput struct {
	// The category to delete (trimmed, case-insensitive). 'other' cannot be
	// deleted.
	Name string `json:"name"`
	// When references remain, move them here instead of rejecting. Must name
	// another existing category, different from the deleted one.
	ReassignTo string `json:"reassign_to,omitempty"`
	// A pointer because nil is meaningful: nil means dry run, which is the
	// safe default for anything that deletes.
	DryRun *bool `json:"dry_run,omitempty"`
}

func registerAccountLifecycleTools(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "edit_account",
		Description: "Fix an account without faking a transaction: rename (name stays unique, rows follow by id so nothing relinks), retype (cash|bank|card|wallet|other, case-insensitive), and/or correct the opening-balance seed (int64 paise, may be negative; the derived balance moves by exactly the delta and no transaction row is written). `name` accepts a name or id hex (trimmed). At least one of new_name/type/opening_balance_paise must differ from the stored row or the call is rejected. Works on archived accounts too."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in editAccountInput) (*mcp.CallToolResult, map[string]any, error) {
			in.Name = strings.TrimSpace(in.Name)
			if in.Name == "" {
				return failMsg("edit_account name is required.")
			}
			var name, typ *string
			if strings.TrimSpace(in.NewName) != "" {
				n := strings.TrimSpace(in.NewName)
				name = &n
			}
			if strings.TrimSpace(in.Type) != "" {
				t := in.Type
				typ = &t
			}
			if name == nil && typ == nil && in.OpeningBalancePaise == nil {
				return failMsg("provide at least one of new_name, type, opening_balance_paise.")
			}
			acct, err := store.EditAccount(ctx, in.Name, name, typ, in.OpeningBalancePaise)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "account": acct})
		})
}

func registerCategoryTools(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "create_category",
		Description: "Add a spending category (trimmed, lowercased, unique — names are case-insensitive). 'other' is the reserved blank fallback and cannot be created. The new name is immediately valid everywhere a category is accepted (log, query, analyses, budgets)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in createCategoryInput) (*mcp.CallToolResult, map[string]any, error) {
			if strings.TrimSpace(in.Name) == "" {
				return failMsg("create_category name is required.")
			}
			c, err := store.CreateCategory(ctx, in.Name)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "category": c})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_categories",
		Description: "List the user-managed category set (sorted). These are the only names every category-accepting tool validates against; 'other' is the blank fallback."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listCategoriesInput) (*mcp.CallToolResult, map[string]any, error) {
			cats, err := store.ListCategories(ctx)
			if err != nil {
				return fail(err)
			}
			rows := make([]any, 0, len(cats))
			for _, c := range cats {
				rows = append(rows, c)
			}
			return result(map[string]any{"ok": true, "categories": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "update_category",
		Description: "Rename a category everywhere: the registry row plus every transaction and budget row carrying the old name (counts echoed back). 'other' can be neither renamed nor renamed to. Renaming to a taken name or to the same name is rejected."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in updateCategoryInput) (*mcp.CallToolResult, map[string]any, error) {
			if strings.TrimSpace(in.Name) == "" || strings.TrimSpace(in.NewName) == "" {
				return failMsg("update_category name and new_name are required.")
			}
			to, ntxn, nbud, err := store.UpdateCategory(ctx, in.Name, in.NewName)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "category": to,
				"transactions_renamed": ntxn, "budgets_renamed": nbud})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_category",
		Description: "Delete an unused category. dry_run=true (default) reports how many transaction and budget rows reference it and deletes nothing. With dry_run=false the delete is rejected while references remain, unless reassign_to names another existing category to move those rows to. 'other' cannot be deleted."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deleteCategoryInput) (*mcp.CallToolResult, map[string]any, error) {
			if strings.TrimSpace(in.Name) == "" {
				return failMsg("delete_category category is required.")
			}
			dry := true
			if in.DryRun != nil {
				dry = *in.DryRun
			}
			c, ntxn, nbud, err := store.DeleteCategory(ctx, in.Name, in.ReassignTo, dry)
			if err != nil {
				return fail(err)
			}
			out := map[string]any{"ok": true, "dry_run": dry, "category": c,
				"transactions": ntxn, "budgets": nbud}
			if !dry {
				out["deleted"] = 1
			}
			return result(out)
		})
}
