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
	b, _ := json.Marshal(out)
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
	// Gmail label id (INBOX, SENT, ...). Defaults to INBOX.
	Label string `json:"label,omitempty"`
	// The handler defaults 0 to 10, caps at 50.
	Limit int `json:"limit,omitempty"`
}

type createDraftInput struct {
	// Recipient address. A draft with no recipient is a note, not a mail draft.
	To string `json:"to"`
	// Draft subject line.
	Subject string `json:"subject"`
	// Plain-text body.
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
		log.Fatalf("gmail: %v", err)
	}
	ctx := context.Background()
	client, err = mail.NewClient(ctx, store, cfg, "")
	if err != nil {
		log.Fatalf("gmail: %v", err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "gmail", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "search_gmail",
		Description: "Search mail with Gmail query syntax (same as the search box: 'from:boss subject:invoice newer_than:7d'). Returns headers + snippets WITHOUT bodies — call read_gmail for the text. Lists are capped at 50; narrow the query rather than asking for more."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in searchGmailInput) (*mcp.CallToolResult, map[string]any, error) {
			rows, err := client.Search(ctx, in.Query, in.Limit)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "messages": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "read_gmail",
		Description: "Read one message by id (the id from search_gmail / list_messages). Returns the full text/plain body, truncated past 20KB and reported. Ids are opaque — a subject will not resolve."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in readGmailInput) (*mcp.CallToolResult, map[string]any, error) {
			m, err := client.Read(ctx, in.ID)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "message": m})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_messages",
		Description: "List recent messages (default INBOX), newest first. Returns headers + snippets WITHOUT bodies — call read_gmail for the text. For a filtered view use search_gmail instead."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listMessagesInput) (*mcp.CallToolResult, map[string]any, error) {
			rows, err := client.List(ctx, in.Label, in.Limit)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "messages": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "create_draft",
		Description: "Create a Gmail draft (compose scope: it can create drafts, never send). To, subject and body are all required. The user sends it from Gmail — this server has no send path."},
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
			return fail(fmt.Errorf("%s", connectors.RefusalMessage))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "send_gmail",
		Description: "DISABLED in v1: sending is refused, create a draft instead and let the user send it."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ sendMessageInput) (*mcp.CallToolResult, map[string]any, error) {
			return fail(fmt.Errorf("%s", connectors.RefusalMessage))
		})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "gmail:", err)
		os.Exit(1)
	}
}
