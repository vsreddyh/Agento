// Command memory is the memory MCP server (#315).
// Storage: MongoDB (memory_items), shared with the gateway and the app.
// Runs over stdio for MCP clients.
//
// What this is for: letting the agent record a durable thing the user told it
// — a preference, a format rule, a fact about their life — so the next turn
// can act on it instead of being told again. What it is NOT for: notes
// (notes), scheduled work (task-manager), or anything the user has not
// actually said. A memory the model invented is worse than no memory, because
// it is invisible and wrong.
//
// Every item written here starts pending. The gateway only injects confirmed
// items, so a proposal is visible in the app for the user to confirm, edit or
// delete before it can steer an answer. The agent cannot confirm its own
// proposals, and it cannot delete items: deletion is the user's verb.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

	"agento/internal/memory"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var (
	store   memory.Store
	profile string
	// ephemeral is true when no database is configured: saves land in this
	// process only and can never reach the app's Memory screen.
	ephemeral bool
)

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

type saveMemoryInput struct {
	// A short label the user can pause on its own: "work", "health",
	// "relationships", "style". Lowercased and hyphenated by the store.
	Topic string `json:"topic"`
	// One fact, in the user's terms. Quote the preference rather than
	// generalising it: "prefers tables for weekly reports" beats "likes
	// structured output".
	Text string `json:"text"`
}

type listMemoryInput struct {
	// Optional topic filter.
	Topic string `json:"topic,omitempty"`
	// "confirmed" (in effect) or "pending" (waiting for the user). Empty lists both.
	Status string `json:"status,omitempty"`
	// The handler defaults 0 to 50 and caps at the store's list cap.
	Limit int `json:"limit,omitempty"`
}

func main() {
	// The gateway runs one Pi per profile and passes the profile down the
	// process tree, so a shared mcp.json still scopes writes correctly. The
	// fallback only matters for a hand-started server outside the gateway.
	profile = strings.TrimSpace(os.Getenv("AGENTO_PROFILE"))
	if profile == "" {
		// Same default the gateway uses for PI_DEFAULT_PROFILE: a hand-started
		// server outside the gateway must scope to the profile the default
		// screen reads, not a neighbouring one.
		profile = strings.TrimSpace(os.Getenv("PI_DEFAULT_PROFILE"))
		if profile == "" {
			profile = "default"
		}
	}

	// Without Mongo the gateway serves from an in-process store the agent
	// cannot see, so proposals would vanish. Refusing to start would fail the
	// container's pi-mcp-list gate and take all six servers down with it; an
	// in-process store with a loud warning keeps the server connectable while
	// saying plainly that saves here will not reach the app.
	if ms, err := memory.FromEnv(); err != nil {
		log.Printf("memory: no database, using in-process store (saves will not reach the app): %v", err)
		store = memory.NewMemoryStore()
		ephemeral = true
	} else {
		store = ms
	}

	s := mcp.NewServer(&mcp.Implementation{Name: "memory", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "save_memory",
		Description: "Remember ONE durable thing the user explicitly told you: a preference, a format rule, a correction, a fact about their life or work. Write it in their terms (" +
			"\"wants weekly reports as a table\", not \"likes tables\"). The item is saved as PENDING and shown in the app; the user confirms it before it affects answers. " +
			"Call this when they correct you or state a lasting preference, not for passing context or anything they have not said. Do not save secrets, credentials or one-off details."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in saveMemoryInput) (*mcp.CallToolResult, map[string]any, error) {
			if !memory.UsableTopic(in.Topic) {
				return result(map[string]any{"ok": false, "error": memory.TopicError(in.Topic)})
			}
			trimmed := strings.TrimSpace(in.Text)
			text := memory.CleanText(in.Text)
			if text == "" {
				return result(map[string]any{"ok": false, "error": "text is required: one durable thing the user said"})
			}
			// CleanText clips at MaxText runes rather than rejecting: say so,
			// so the agent can rephrase instead of believing a clipped fact
			// stored whole.
			truncated := text != trimmed
			it, err := store.Create(ctx, memory.Item{
				Profile: profile,
				Topic:   memory.NormalizeTopic(in.Topic),
				Text:    text,
				Source:  memory.SourceAgent,
				Status:  memory.StatusPending,
			})
			if err != nil {
				return fail(err)
			}
			note := "Saved as pending; the user confirms it in the app before it is used."
			if ephemeral {
				// The agent must not tell the user "saved": this item lives
				// in the MCP process and the screen will never show it.
				note = "Saved in this process only: no database is configured, so it will not reach the app's Memory screen and will vanish on restart."
			}
			return result(map[string]any{
				"ok":         true,
				"item":       it,
				"status":     string(memory.StatusPending),
				"truncated":  truncated,
				"persistent": !ephemeral,
				"note":       note,
			})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_memory",
		Description: "List what is remembered for this profile, newest first, optionally filtered by topic and/or status. Use it when the user asks what you remember, or before saving something that may already be recorded. Items include status: only confirmed items are applied to answers."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listMemoryInput) (*mcp.CallToolResult, map[string]any, error) {
			// Filters forgive where writes reject (same rule as GET): a
			// filter names what to show, so normalising "---" to general
			// is harmless, while a write names where to store.
			f := memory.Filter{Profile: profile, Topic: in.Topic}
			// Exact lowercase like the HTTP contract: topics normalise,
			// enums do not.
			if st := strings.TrimSpace(in.Status); st != "" {
				status := memory.Status(st)
				if !status.Valid() {
					return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown status %q: use confirmed or pending", st)})
				}
				f.Status = status
			}
			// Negative is a caller error, not the store cap: the HTTP
			// contract 400s it, and the MCP surface must not silently mean
			// 200.
			if in.Limit < 0 {
				return result(map[string]any{"ok": false, "error": "limit must be a non-negative number"})
			}
			f.Limit = in.Limit
			if f.Limit == 0 {
				// Small on purpose: the agent gets a working set, while the
				// screen lists up to ListCap for its topic list.
				f.Limit = 50
			}
			items, err := store.List(ctx, f)
			if err != nil {
				return fail(err)
			}
			if items == nil {
				items = []memory.Item{}
			}
			// Same ephemeral signal as save_memory: on fallback these rows
			// live in this process only, and reading them back must not
			// pass for durable memory.
			return result(map[string]any{"ok": true, "count": len(items), "items": items, "persistent": !ephemeral})
		})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "memory:", err)
		os.Exit(1)
	}
}
