package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// These tests exist because the previous round of tests in this package proved the
// validators behave and said nothing about whether the handlers call them. Deleting the
// guard from `summarize` left every unit test green.
//
// So the server is built exactly as main builds it — `registerTools` over an in-memory
// transport — and the tools are called through a real MCP client. `store` stays nil: a
// handler that reaches MongoDB before validating panics, and that panic is exactly the
// failure this file is here to catch.
//
// The gap being closed, in both tools:
//
//   - `query_transactions` with empty bounds built `date >= "" && date <= ""`, matching
//     nothing: the caller saw zero transactions and no reason why.
//   - `summarize` with a lone `start` fell through to the period branch and summarised a
//     window nobody asked for.
func callTool(t *testing.T, name string, args map[string]any) (string, string) {
	t.Helper()

	s := mcp.NewServer(&mcp.Implementation{Name: "miser-money-test", Version: "0"}, nil)
	registerTools(s)

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := s.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connecting server: %v", err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connecting client: %v", err)
	}
	defer session.Close()

	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}

	// `structured` is this package's structured payload: a map with "ok" and "error".
	raw, mErr := json.Marshal(res.StructuredContent)
	if mErr != nil {
		t.Fatalf("marshalling result: %v", mErr)
	}
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshalling result: %v (raw %s)", err, raw)
	}
	text := ""
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text += tc.Text
		}
	}
	return text, out.Error
}

func TestQueryTransactionsRejectsUnusableBounds(t *testing.T) {
	cases := []struct {
		name       string
		start, end string
	}{
		{"both empty — used to return zero rows silently", "", ""},
		{"start empty", "", "2026-03-31"},
		{"end empty", "2026-01-01", ""},
		{"malformed start", "yesterday", "2026-03-31"},
		{"malformed end", "2026-01-01", "soon"},
		{"impossible calendar date", "2026-02-30", "2026-03-31"},
		// Two valid dates whose range is empty — the store would have returned zero rows
		// as a real answer.
		{"reversed range", "2026-03-31", "2026-01-01"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, errMsg := callTool(t, "query_transactions", map[string]any{
				"start": c.start, "end": c.end,
			})
			if errMsg == "" {
				t.Fatalf("query_transactions(%q, %q) was accepted; expected a rejection "+
					"rather than a silently empty result", c.start, c.end)
			}
			if !strings.Contains(errMsg, "query_transactions") {
				t.Errorf("error %q does not name the tool", errMsg)
			}
		})
	}
}

func TestSummarizeRejectsAHalfSuppliedRange(t *testing.T) {
	for _, c := range []struct{ name, start, end string }{
		{"lone start", "2026-01-01", ""},
		{"lone end", "", "2026-01-31"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, errMsg := callTool(t, "summarize", map[string]any{
				"start": c.start, "end": c.end,
			})
			if errMsg == "" {
				t.Fatalf("summarize(start=%q, end=%q) was accepted; it used to ignore the "+
					"bound and summarise the default period instead", c.start, c.end)
			}
			if !strings.Contains(errMsg, "both start and end") {
				t.Errorf("error %q does not explain the either/or constraint", errMsg)
			}
		})
	}
}
