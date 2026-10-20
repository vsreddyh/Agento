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

// prepareSave validates one save_memory call into a store-ready item plus
// the truncation signal, or a caller-facing error. Pure, so the MCP writer's
// mapping stays pinned by tests the way the HTTP contract is — the two
// surfaces have drifted twice before.
func prepareSave(profile string, in saveMemoryInput) (memory.Item, bool, string) {
	if !memory.UsableTopic(in.Topic) {
		return memory.Item{}, false, memory.TopicError(in.Topic)
	}
	text := memory.CleanText(in.Text)
	if text == "" {
		return memory.Item{}, false, "text is required: one durable thing the user said"
	}
	// CleanText clips at MaxText runes rather than rejecting: the caller gets
	// the signal so it can rephrase instead of believing a clipped fact
	// stored whole.
	return memory.Item{
		Profile: profile,
		Topic:   memory.NormalizeTopic(in.Topic),
		Text:    text,
		Source:  memory.SourceAgent,
		Status:  memory.StatusPending,
	}, memory.Clipped(in.Text), ""
}

// prepareList maps one list_memory call into a store filter or a
// caller-facing error. Same contracts as GET: filters forgive where writes
// reject, statuses are exact lowercase, negative limits fail, and 0 means a
// small agent working set while the screen lists up to ListCap.
func prepareList(profile string, in listMemoryInput) (memory.Filter, string) {
	f := memory.Filter{Profile: profile, Topic: in.Topic}
	if st := strings.TrimSpace(in.Status); st != "" {
		status := memory.Status(st)
		if !status.Valid() {
			return memory.Filter{}, fmt.Sprintf("unknown status %q: use confirmed or pending", st)
		}
		f.Status = status
	}
	if in.Limit < 0 {
		return memory.Filter{}, "limit must be a non-negative number"
	}
	f.Limit = in.Limit
	if f.Limit == 0 {
		f.Limit = 50
	}
	return f, ""
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
	// Index ownership lives solely in pi-gateway (EnsureIndexes on startup):
	// a hand-started server must not race it, and an unindexed query still
	// answers, just slower, until the gateway runs once.
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
			item, truncated, errMsg := prepareSave(profile, in)
			if errMsg != "" {
				return result(map[string]any{"ok": false, "error": errMsg})
			}
			it, err := store.Create(ctx, item)
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
			f, errMsg := prepareList(profile, in)
			if errMsg != "" {
				return result(map[string]any{"ok": false, "error": errMsg})
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
