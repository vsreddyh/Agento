// Command project-manager is the project board MCP server.
//
// Storage: MongoDB (projects collection). One doc per project with name,
// status (Todo | Ongoing | Paused | Done), note, createdAt, updatedAt.
// Projects are permanent (no TTL) and shared with the app's Projects tab
// over /api/projects (see cmd/health-api/main.go).
// Runs over stdio for MCP clients.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"agento/internal/projects"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var store *projects.Store

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
// the same types `AddTool` publishes — an inline struct can only be checked by reading the
// handler, and in this server the handler and the published schema disagreed: every field
// was advertised as required because nothing carried `omitempty`, including fields the
// handler treats as "absent means untouched".
type createProjectInput struct {
	// The only required field: store.Create rejects an empty name.
	Name string `json:"name"`
	// store.Create defaults an empty status to Todo, so "" is a meaningful value here,
	// not a missing one.
	Status string `json:"status,omitempty"`
	Note   string `json:"note,omitempty"`
}

type listProjectsInput struct {
	// All documented with defaults (status all, limit 200), so none of them should be
	// mandatory — requiring them makes "list everything" a call that cannot be made.
	Status string `json:"status,omitempty"`
	Search string `json:"search,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

type getProjectInput struct {
	ID string `json:"id"`
}

type updateProjectInput struct {
	ID string `json:"id"`
	// Nil means untouched, non-nil (even "") means set. Publishing these as required is
	// what let a partial edit become a full-record overwrite.
	Name   *string `json:"name,omitempty"`
	Status *string `json:"status,omitempty"`
	Note   *string `json:"note,omitempty"`
}

type deleteProjectInput struct {
	ID string `json:"id"`
}

func main() {
	var err error
	store, err = projects.FromEnv()
	if err != nil {
		log.Fatalf("project-manager: %v", err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "project-manager", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "create_project",
		Description: "Create a project. name required; status Todo (default) | Ongoing | Paused | Done; note free-form."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in createProjectInput) (*mcp.CallToolResult, map[string]any, error) {
			doc, err := store.Create(ctx, in.Name, in.Status, in.Note)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "project": doc})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_projects",
		Description: "List projects. status Todo | Ongoing | Paused | Done | all (default all); search matches name/note; limit caps rows (default 200, max 500)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listProjectsInput) (*mcp.CallToolResult, map[string]any, error) {
			rows, err := store.List(ctx, in.Status, in.Search, in.Limit)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "projects": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_project",
		Description: "Fetch one project by id."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in getProjectInput) (*mcp.CallToolResult, map[string]any, error) {
			doc, err := store.Get(ctx, in.ID)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "project": doc})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "update_project",
		Description: "Edit name/status/note (only sent keys change; blank name rejected)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in updateProjectInput) (*mcp.CallToolResult, map[string]any, error) {
			fields := map[string]any{}
			// Pointers tell absent from set: nil = untouched, non-nil
			// (even "") = set (name still rejects blank).
			if in.Name != nil {
				fields["name"] = *in.Name
			}
			if in.Status != nil {
				fields["status"] = *in.Status
			}
			if in.Note != nil {
				fields["note"] = *in.Note
			}
			if len(fields) == 0 {
				// Explicit no-op marker so the agent knows nothing changed.
				doc, err := store.Get(ctx, in.ID)
				if err != nil {
					return fail(err)
				}
				return result(map[string]any{"ok": true, "project": doc, "noop": true})
			}
			doc, err := store.Update(ctx, in.ID, fields)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "project": doc})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_project",
		Description: "Permanently delete a project."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deleteProjectInput) (*mcp.CallToolResult, map[string]any, error) {
			done, err := store.Delete(ctx, in.ID)
			if err != nil {
				return fail(err)
			}
			if !done {
				return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown project '%s'", in.ID)})
			}
			return result(map[string]any{"ok": true, "deleted": in.ID})
		})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "project-manager:", err)
		os.Exit(1)
	}
}
