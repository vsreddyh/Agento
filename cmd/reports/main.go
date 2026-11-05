// Command reports is the ∃-Mail writer MCP server (#365).
// Storage: MongoDB (reports).
// Seven-day conditional retention — NOT permanent: read+unstarred reports older
// than 7 days are pruned; unread and starred survive. See cmd/retention.
//
// What this is for: agents filing reports the user reads in the app's ∃-Mail
// inbox — investigation findings, comparisons, trip research. What it is NOT
// for: anything the user edits (notes), schedules (task-manager), or anything
// with two-way messaging — there is no reply, no thread, no send. The inbox is
// read-only; this server only files.
//
// Math: body_md may carry inline `$..$` and block `$$..$$` LaTeX. It is stored
// verbatim and compiled server-side at READ time (internal/reports), so the
// writer never renders and the app never compiles.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"agento/internal/approvalgate"
	"agento/internal/approvals"
	"agento/internal/mongo"
	"agento/internal/reports"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var store *reports.Store

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

// Input types are named rather than inline so `schema_test.go` can infer the
// schema from the same types `AddTool` publishes — the same call, so the test
// asserts the published contract rather than a hand-written copy.
type createReportInput struct {
	// store.CreateReport rejects an empty title.
	Title string `json:"title"`
	// Markdown with inline ($..$) and block ($$..$$) LaTeX math. Stored
	// verbatim; compiled server-side on read. `$` inside code spans stays
	// literal. Over-cap bodies are stored truncated and reported.
	BodyMD string `json:"body_md,omitempty"`
	// Which skill/agent files this, e.g. "t3-research". Display-only.
	Source string `json:"source,omitempty"`
}

type getReportInput struct {
	// The id returned by list_reports / create_report. Reports are NOT
	// addressable by title — titles are not unique.
	ID string `json:"id"`
}

type listReportsInput struct {
	// Newest-first. The handler defaults 0 to 50.
	Limit int `json:"limit,omitempty"`
}

func main() {
	var err error
	store, err = reports.FromEnv()
	if err != nil {
		log.Fatalf("reports: %v", err)
	}
	// Gate shares the domain store's client: mongo.DB() is process-cached and
	// reports.New built on it, so this adds zero clients.
	db, err := mongo.DB()
	if err != nil {
		log.Fatalf("reports: %v", err)
	}
	gate := approvals.NewOn(db)
	s := mcp.NewServer(&mcp.Implementation{Name: "reports", Version: "1.0.0"}, nil)

	approvalgate.AddTool(s, "reports", gate, &mcp.Tool{Name: "create_report",
		Description: "File a report in the user's ∃-Mail inbox. body_md is markdown with inline $..$ and block $$..$$ LaTeX math (stored verbatim, compiled server-side on read). The inbox is read-only for the user — no reply, no thread, no send. Over-cap title/body is stored truncated and reported in `truncated`."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in createReportInput) (*mcp.CallToolResult, map[string]any, error) {
			r, err := store.CreateReport(ctx, in.Title, in.BodyMD, in.Source)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "report": r})
		})

	approvalgate.AddTool(s, "reports", gate, &mcp.Tool{Name: "get_report",
		Description: "Get one report by id (the id from list_reports). Returns the full body_md with math stored verbatim — pre-rendered segments are served by the read API, not here. Titles are not unique, so a title will not resolve."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in getReportInput) (*mcp.CallToolResult, map[string]any, error) {
			r, err := store.GetReport(ctx, in.ID)
			if err != nil {
				return fail(err)
			}
			if r == nil {
				return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown report '%s'", reports.ShortID(in.ID))})
			}
			return result(map[string]any{"ok": true, "report": r})
		})

	approvalgate.AddTool(s, "reports", gate, &mcp.Tool{Name: "list_reports",
		Description: "List filed reports, newest first. Returns summaries WITHOUT bodies — call get_report for the text. There is no search, no archive, no folders: the inbox is newest-first only."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listReportsInput) (*mcp.CallToolResult, map[string]any, error) {
			rows, err := store.ListReports(ctx, in.Limit)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "reports": rows})
		})

	// Sweep this server's orphaned holds: a crash between CreateApproval and
	// decision leaves a pending row no live holder polls. Scoped by server,
	// like the other servers; best-effort.
	sweepCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, serr := gate.MarkServerInterrupted(sweepCtx, "reports", time.Now()); serr != nil {
		log.Printf("reports: startup sweep failed: %v", serr)
	}
	cancel()
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "reports:", err)
		os.Exit(1)
	}
}
