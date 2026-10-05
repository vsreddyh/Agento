// Command notes is the shared notes MCP server.
// Storage: MongoDB (notes).
// Permanent — never pruned. Runs over stdio for MCP clients.
//
// What this is for: things the user keeps but does not schedule — a grocery list,
// a packing list, a half-written idea. What it is NOT for: work items with a due
// date (task-manager) or things that are true about the user's body or money
// (health-check / miser-money). The boundary matters because all of those are
// separate collections with their own contracts; a note that grows a due date is a
// task that grew a wrong home.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"agento/internal/notes"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var store *notes.Store

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

// Input types are named rather than inline so `schema_test.go` can infer the schema
// from the same types `AddTool` publishes — the same call, so the test asserts the
// published contract rather than a hand-written copy of it.
//
// Optionality here has the same two shapes as the cookbook server: the handler
// defaults a zero value (`list_notes.limit` becomes 50, `search_notes.limit` 20), and
// the list tools take filters their own descriptions call optional — a required
// filter makes "show me everything" a call that cannot be made.
type addNoteInput struct {
	// store.AddNote rejects an empty title.
	Title string `json:"title"`
	// Markdown. One list item per line (`- milk`); blank lines and headings are fine.
	Body string `json:"body,omitempty"`
	// store.AddNote lowercases and caps these.
	Tags []string `json:"tags,omitempty"`
	// Pinned notes sort first and can be listed on their own.
	Pinned bool `json:"pinned,omitempty"`
}

type getNoteInput struct {
	// The id returned by list_notes / create_note. Notes are NOT addressable by
	// title — titles are not unique.
	ID string `json:"id"`
}

type listNotesInput struct {
	// "optionally filtered by tag" — all filters optional so an unfiltered list is a
	// call that can be made.
	Tag string `json:"tag,omitempty"`
	// List only pinned notes.
	PinnedOnly bool `json:"pinned_only,omitempty"`
	// The handler defaults 0 to 50.
	Limit int `json:"limit,omitempty"`
}

type searchNotesInput struct {
	// Substring matched against title AND body.
	Text string `json:"text"`
	// The handler defaults 0 to 20.
	Limit int `json:"limit,omitempty"`
}

type updateNoteInput struct {
	ID string `json:"id"`
	// Patch semantics throughout: "Absent args are left alone", per the description.
	// So a change to one line of a list means sending the WHOLE new body — absent and
	// empty mean DIFFERENT things, and only a pointer can say which:
	//
	//   - absent  → leave the field alone
	//   - "" / []  → clear it, on purpose (emptying a list is a real edit)
	//
	// A plain `string` + omitempty collapses those two into one: `body=""` is dropped
	// before the store ever sees it, so "clear this note" is silently a no-op and the
	// agent reports success on an edit that never happened. `*string`/`*[]string` are
	// the only way to publish that contract honestly.
	Title  *string   `json:"title,omitempty"`
	Body   *string   `json:"body,omitempty"`
	Tags   *[]string `json:"tags,omitempty"`
	Pinned *bool     `json:"pinned,omitempty"`
}

type deleteNoteInput struct {
	ID string `json:"id"`
}

func main() {
	var err error
	store, err = notes.FromEnv()
	if err != nil {
		log.Fatalf("notes: %v", err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "notes", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "create_note",
		Description: "Create a note. body is markdown, one list item per line (`- milk`). Tags are lowercased; titles need not be unique. For anything with a due date use the task-manager, not this. Over-cap title/body is stored truncated and reported in `truncated`."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in addNoteInput) (*mcp.CallToolResult, map[string]any, error) {
			n, err := store.AddNote(ctx, in.Title, in.Body, in.Tags, in.Pinned)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "note": n})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_note",
		Description: "Get one note by id (the id from list_notes). Returns the full body. Titles are not unique, so a title will not resolve."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in getNoteInput) (*mcp.CallToolResult, map[string]any, error) {
			n, err := store.GetNote(ctx, in.ID)
			if err != nil {
				return fail(err)
			}
			if n == nil {
				return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown note '%s'", in.ID)})
			}
			return result(map[string]any{"ok": true, "note": n})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_notes",
		Description: "List notes (pinned first, then newest first), optionally filtered by tag and/or to pinned only. Returns summaries WITHOUT bodies — call get_note for the text."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listNotesInput) (*mcp.CallToolResult, map[string]any, error) {
			rows, err := store.ListNotes(ctx, in.Tag, in.PinnedOnly, in.Limit)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "notes": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "search_notes",
		Description: "Substring search over note titles, bodies AND tags, returning full notes. Use when the user names a thing ('milk') without saying which note holds it. Returns full bodies, so limit is capped at 50 — narrow the query rather than asking for more."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in searchNotesInput) (*mcp.CallToolResult, map[string]any, error) {
			limit := in.Limit
			if limit == 0 {
				limit = 20
			}
			rows, err := store.SearchNotes(ctx, in.Text, limit)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "notes": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "update_note",
		Description: "Patch a note. An arg you omit is left alone; body=\"\" empties the note and tags=[] removes every tag (an empty title is refused). The body replaces, it does not append — editing one line means sending the whole new body. Confirm with the user before overwriting a body you did not write."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in updateNoteInput) (*mcp.CallToolResult, map[string]any, error) {
			// Pointers, so absent (leave alone) and empty (clear) stay distinct — see
			// updateNoteInput. Anything that reaches this block is an edit the caller
			// asked for by name, including the zero value.
			patch := map[string]any{}
			if in.Title != nil {
				patch["title"] = *in.Title
			}
			if in.Body != nil {
				patch["body"] = *in.Body
			}
			if in.Tags != nil {
				patch["tags"] = *in.Tags
			}
			if in.Pinned != nil {
				patch["pinned"] = *in.Pinned
			}
			n, err := store.UpdateNote(ctx, in.ID, patch)
			if err != nil {
				return fail(err)
			}
			if n == nil {
				return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown note '%s'", in.ID)})
			}
			return result(map[string]any{"ok": true, "note": n})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_note",
		Description: "Delete a note by id. Irreversible — confirm with the user first. An unknown id returns ok:false, the same as get_note and update_note."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deleteNoteInput) (*mcp.CallToolResult, map[string]any, error) {
			done, err := store.DeleteNote(ctx, in.ID)
			if err != nil {
				return fail(err)
			}
			// Same shape as get_note and update_note for a missing id: ok:false with a
			// reason. `ok:true, deleted:false` was accurate and inconsistent — an agent
			// checking one flag rather than two would read it as success.
			if !done {
				return result(map[string]any{"ok": false,
					"error": fmt.Sprintf("unknown note '%s' — nothing was deleted", in.ID)})
			}
			return result(map[string]any{"ok": true, "deleted": true})
		})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "notes:", err)
		os.Exit(1)
	}
}
