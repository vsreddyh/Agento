// Command gmail is the Gmail connector MCP server (#316).
// Storage: MongoDB (connectors grant, sealed) + the Gmail API itself.
// Runs over stdio for MCP clients, behind mcp-gate like every server.
//
// What this is for: searching/reading mail and creating drafts. What it is
// NOT for: sending — send_message/send_gmail are registered only so the
// relay refuses them with "create a draft instead" (see
// connectors.IsDraftOnlySend) instead of answering an unknown tool. The
// grant carries compose (drafts), never send: the token is incapable of
// sending even if the relay check vanished.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"

	"agento/internal/connectors"
	mail "agento/internal/gmail"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var client *mail.Client

func fail(err error) (*mcp.CallToolResult, map[string]any, error) {
	return nil, map[string]any{"ok": false, "error": err.Error()}, nil
}

func result(out map[string]any) (*mcp.CallToolResult, map[string]any, error) {
	b, err := json.Marshal(out)
	if err != nil {
		return fail(err)
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
		StructuredContent: out,
	}, out, nil
}

// Input types are named rather than inline so schema_test.go can infer the
// schema from the same types AddTool publishes — the test asserts the
// published contract, not a hand-written copy of it.
type searchGmailInput struct {
	// Gmail query syntax (same as the search box): "from:boss subject:invoice".
	Query string `json:"query"`
	// The handler defaults 0 to 10, caps at 50.
	Limit int `json:"limit,omitempty"`
}

type readGmailInput struct {
	// The id from search_gmail / list_messages. Ids are opaque; subjects
	// are not unique and do not resolve.
	ID string `json:"id"`
}

type listMessagesInput struct {
	// Gmail label ID (uppercase for system labels: INBOX, SENT, ...).
	// Custom labels use their own IDs. Defaults to INBOX.
	Label string `json:"label,omitempty"`
	// The handler defaults 0 to 10, caps at 50.
	Limit int `json:"limit,omitempty"`
}

type createDraftInput struct {
	// Recipient address. A draft with no recipient is a note, not a mail draft.
	To string `json:"to"`
	// Draft subject line.
	Subject string `json:"subject"`
	// Plain-text body. Leading/trailing whitespace is trimmed (same
	// normalisation as calendar drafts); over-cap bodies are refused
	// rather than truncated.
	Body string `json:"body"`
}

type sendMessageInput struct {
	// Accepted and refused: sending is disabled in v1 — create a draft
	// instead and let the user send it. Registered so the refusal names
	// the draft path instead of answering unknown-tool.
	To      string `json:"to,omitempty"`
	Subject string `json:"subject,omitempty"`
	Body    string `json:"body,omitempty"`
}

func main() {
	store, err := connectors.FromEnv()
	if err != nil {
		log.Fatalf("gmail: %v", err)
	}
	cfg, err := connectors.ServerOAuthConfig()
	if err != nil {
		// Unconfigured OAuth is not fatal: the server still boots with a
		// nil cfg so the entrypoint probe passes, and every
		// Google-touching tool answers "not configured" per call until
		// the deploy sets the client pair.
		log.Printf("gmail: running unconfigured: %v", err)
		cfg = nil // boot unconfigured; the per-call guard answers not-configured
	}
	// Boot needs Mongo only — the grant resolves per tool call, so a
	// disconnected deploy still starts and every tool answers reconnect
	// guidance until the user connects.
	client, err = mail.NewClient(store, cfg, "", log.Printf)
	if err != nil {
		log.Fatalf("gmail: %v", err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "gmail", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "search_gmail",
		Description: "Search mail with Gmail query syntax (same as the search box: 'from:boss subject:invoice newer_than:7d'). Returns headers + snippets WITHOUT bodies — call read_gmail for the text. Lists are capped at 50 silently (a full page means narrow the query, not that only 50 exist). Subject and From arrive RFC2047-decoded; Date is raw by design."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in searchGmailInput) (*mcp.CallToolResult, map[string]any, error) {
			res, err := client.Search(ctx, in.Query, in.Limit)
			if err != nil {
				return fail(err)
			}
			out := map[string]any{"ok": true, "count": len(res.Messages), "messages": res.Messages}
			if res.Partial {
				out["partial"] = true
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "read_gmail",
		Description: "Read one message by id (the id from search_gmail / list_messages). Returns the full text/plain body, truncated past 20KB and reported. Ids are opaque — a subject will not resolve. To/Cc are not surfaced (headers available: From/Date); use the snippet and thread for recipient context."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in readGmailInput) (*mcp.CallToolResult, map[string]any, error) {
			m, err := client.Read(ctx, in.ID)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "message": m})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_messages",
		Description: "List recent messages (default INBOX), newest first. Returns headers + snippets WITHOUT bodies — call read_gmail for the text. Lists are capped at 50 silently (a full page means narrow the query with search_gmail, not that only 50 exist). For a filtered view use search_gmail instead."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listMessagesInput) (*mcp.CallToolResult, map[string]any, error) {
			res, err := client.List(ctx, in.Label, in.Limit)
			if err != nil {
				return fail(err)
			}
			out := map[string]any{"ok": true, "count": len(res.Messages), "messages": res.Messages}
			if res.Partial {
				// Some rows failed mid-list (deleted between list and
				// fetch is routine): the subset says so instead of
				// failing the whole call.
				out["partial"] = true
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "create_draft",
		Description: "Create a Gmail draft (compose scope: it can create drafts, never send). To is one ASCII bare address (no display names, no lists); subject caps at 200 runes, body at 20000 (refused, not truncated); leading/trailing whitespace is trimmed. Non-ASCII subjects are Q-encoded. The user sends it from Gmail — this server has no send path."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in createDraftInput) (*mcp.CallToolResult, map[string]any, error) {
			d, err := client.CreateDraft(ctx, in.To, in.Subject, in.Body)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "draft": d})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "send_message",
		Description: "DISABLED in v1: sending is refused, create a draft instead and let the user send it."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ sendMessageInput) (*mcp.CallToolResult, map[string]any, error) {
			// Unreachable through the relay (it refuses first), defense in
			// depth for direct callers. Same message either way.
			return fail(errors.New(connectors.RefusalMessage))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "send_gmail",
		Description: "DISABLED in v1: sending is refused, create a draft instead and let the user send it."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ sendMessageInput) (*mcp.CallToolResult, map[string]any, error) {
			return fail(errors.New(connectors.RefusalMessage))
		})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "gmail:", err)
		os.Exit(1)
	}
}
