// Command calendar is the Calendar connector MCP server (#316).
// Storage: MongoDB (connectors grant, sealed) + the Calendar API itself.
// Runs over stdio for MCP clients, behind mcp-gate like every server.
//
// What this is for: listing upcoming events and building draft events. What
// it is NOT for: creating events or sending invites — create_event /
// send_invite are registered only so the relay refuses them with "create a
// draft instead" (see connectors.IsDraftOnlySend) instead of answering an
// unknown tool. The grant carries readonly: the token is incapable of
// writing even if the relay check vanished.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	cal "agento/internal/calendar"
	"agento/internal/connectors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var client *cal.Client

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
type listEventsInput struct {
	// Calendar id. Defaults to "primary".
	Calendar string `json:"calendar,omitempty"`
	// RFC3339 date-time or YYYY-MM-DD date. Defaults to now.
	From string `json:"from,omitempty"`
	// RFC3339 date-time or YYYY-MM-DD date (whole day, inclusive).
	// Defaults to 30 days out. Date bounds are UTC-day bounds:
	// from=day starts at 00:00 UTC, to=day runs through end of day UTC.
	// Non-UTC users get a window off by hours from their calendar day.
	To string `json:"to,omitempty"`
	// The handler defaults 0 to 10, caps at 50.
	Limit int `json:"limit,omitempty"`
}

type getEventInput struct {
	// Calendar id. Defaults to "primary".
	Calendar string `json:"calendar,omitempty"`
	// The id from list_events.
	ID string `json:"id"`
}

type createEventDraftInput struct {
	// Event title. Required.
	Summary string `json:"summary"`
	// Longer description. Leading/trailing whitespace is trimmed;
	// over-cap text is truncated and reported.
	Description string `json:"description,omitempty"`
	// Where it happens.
	Location string `json:"location,omitempty"`
	// RFC3339 date-time or YYYY-MM-DD date (both ends must match in kind).
	Start string `json:"start"`
	// RFC3339 date-time or YYYY-MM-DD date. Must be after start.
	End string `json:"end"`
	// Invitees. Stored on the draft only — nothing is sent anywhere.
	Attendees []string `json:"attendees,omitempty"`
}

type createEventInput struct {
	// Accepted and refused: creating events is disabled in v1 — build a
	// draft instead and let the user confirm it. Registered so the refusal
	// names the draft path instead of answering unknown-tool.
	Summary string `json:"summary,omitempty"`
	Start   string `json:"start,omitempty"`
	End     string `json:"end,omitempty"`
}

func parseBound(raw, name string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t, nil
	}
	// Date-only bounds, same kinds Draft accepts: from=day means that
	// day 00:00 UTC, to=day means the END of that day (next midnight),
	// so a whole-day window is expressible without timestamps.
	// Round-trip check, same as isDate: time.Parse normalizes Feb 30
	// into March, which would silently move the window. The name match
	// is case-insensitive: internal callers pass lowercase today, but a
	// future "To" must not silently get from-semantics (midnight instead
	// of end-of-day).
	if d, err := time.Parse("2006-01-02", raw); err == nil && d.Format("2006-01-02") == raw {
		if strings.EqualFold(name, "to") {
			return d.Add(24 * time.Hour), nil
		}
		return d, nil
	}
	return time.Time{}, fmt.Errorf("calendar: %s must be RFC3339 or YYYY-MM-DD", name)
}

func main() {
	store, err := connectors.FromEnv()
	if err != nil {
		log.Fatalf("calendar: %v", err)
	}
	cfg, err := connectors.ServerOAuthConfig()
	if err != nil {
		log.Fatalf("calendar: %v", err)
	}
	// Boot needs Mongo + the client pair only — the grant resolves per
	// tool call, so a disconnected deploy still starts and every tool
	// answers reconnect guidance until the user connects.
	client, err = cal.NewClient(store, cfg, "", log.Printf)
	if err != nil {
		log.Fatalf("calendar: %v", err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "calendar", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "list_events",
		Description: "List upcoming events (default calendar 'primary', default window the next 30 days), soonest first. Returns when + what, no sync metadata. Lists are capped at 50 silently (a full page means narrow the window, not that only 50 exist)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listEventsInput) (*mcp.CallToolResult, map[string]any, error) {
			from, err := parseBound(in.From, "from")
			if err != nil {
				return fail(err)
			}
			to, err := parseBound(in.To, "to")
			if err != nil {
				return fail(err)
			}
			rows, err := client.List(ctx, in.Calendar, from, to, in.Limit)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "events": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_event",
		Description: "Get one event by id (the id from list_events)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in getEventInput) (*mcp.CallToolResult, map[string]any, error) {
			e, err := client.Get(ctx, in.Calendar, in.ID)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "event": e})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "create_event_draft",
		Description: "Build a draft event (validated, sent nowhere — the grant is readonly). Attendees are single bare ASCII addresses (count uncapped in v1); summary caps at 200 runes, location at 500 (refused, not truncated); description is trimmed and capped at 5000 with a truncated flag. The agent presents the draft to the user; actual creation waits for the approval-gate flow that covers calendar writes. Start/end are RFC3339 date-times or YYYY-MM-DD dates (both ends must match in kind); end must be after start."},
		func(_ context.Context, _ *mcp.CallToolRequest, in createEventDraftInput) (*mcp.CallToolResult, map[string]any, error) {
			d, err := cal.Draft(in.Summary, in.Description, in.Location, in.Start, in.End, in.Attendees)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "draft": d,
				"note": "draft only — nothing was created or sent; present it to the user"})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "create_event",
		Description: "DISABLED in v1: creating events is refused, build a draft instead and let the user confirm it."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ createEventInput) (*mcp.CallToolResult, map[string]any, error) {
			// Unreachable through the relay (it refuses first), defense in
			// depth for direct callers. Same message either way.
			return fail(errors.New(connectors.RefusalMessage))
		})
	mcp.AddTool(s, &mcp.Tool{Name: "send_invite",
		Description: "DISABLED in v1: sending invites is refused, build a draft instead and let the user confirm it."},
		func(_ context.Context, _ *mcp.CallToolRequest, _ createEventInput) (*mcp.CallToolResult, map[string]any, error) {
			return fail(errors.New(connectors.RefusalMessage))
		})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "calendar:", err)
		os.Exit(1)
	}
}
