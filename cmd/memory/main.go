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
		profile = "chat"
	}

	ms, err := memory.FromEnv()
	if err != nil {
		log.Fatalf("memory: %v", err)
	}
	store = ms

	s := mcp.NewServer(&mcp.Implementation{Name: "memory", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "save_memory",
		Description: "Remember ONE durable thing the user explicitly told you: a preference, a format rule, a correction, a fact about their life or work. Write it in their terms (" +
			"\"wants weekly reports as a table\", not \"likes tables\"). The item is saved as PENDING and shown in the app; the user confirms it before it affects answers. " +
			"Call this when they correct you or state a lasting preference, not for passing context or anything they have not said. Do not save secrets, credentials or one-off details."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in saveMemoryInput) (*mcp.CallToolResult, map[string]any, error) {
			text := memory.CleanText(in.Text)
			if text == "" {
				return result(map[string]any{"ok": false, "error": "text is required: one durable thing the user said"})
			}
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
			return result(map[string]any{
				"ok":     true,
				"item":   it,
				"status": string(memory.StatusPending),
				"note":   "Saved as pending; the user confirms it in the app before it is used.",
			})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_memory",
		Description: "List what is remembered for this profile, newest first, optionally filtered by topic and/or status. Use it when the user asks what you remember, or before saving something that may already be recorded. Items include status: only confirmed items are applied to answers."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listMemoryInput) (*mcp.CallToolResult, map[string]any, error) {
			f := memory.Filter{Profile: profile, Topic: in.Topic}
			if st := strings.TrimSpace(in.Status); st != "" {
				status := memory.Status(st)
				if !status.Valid() {
					return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown status %q: use confirmed or pending", st)})
				}
				f.Status = status
			}
			f.Limit = in.Limit
			if f.Limit == 0 {
				f.Limit = 50
			}
			items, err := store.List(ctx, f)
			if err != nil {
				return fail(err)
			}
			if items == nil {
				items = []memory.Item{}
			}
			return result(map[string]any{"ok": true, "count": len(items), "items": items})
		})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "memory:", err)
		os.Exit(1)
	}
}
