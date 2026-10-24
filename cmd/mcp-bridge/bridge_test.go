package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agento/internal/connectors"
)

const testHexKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// stubUpstream speaks minimal Streamable-HTTP: tools/list with a mix of
// exposed and filtered tools, and a tools/call echo. mode selects the
// framing (json or sse) and the status code.
func stubUpstream(t *testing.T, mode string, status int, seenAuth *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seenAuth != nil {
			*seenAuth = r.Header.Get("Authorization")
		}
		if r.URL.Path == "/t" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"access_token":"a456","refresh_token":"r789","expires_in":3600,"token_type":"Bearer"}`)
			return
		}
		if status != 0 {
			http.Error(w, "denied", status)
			return
		}
		var in rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			t.Error(err)
		}
		var result any
		switch in.Method {
		case "tools/list":
			result = map[string]any{"tools": []any{
				map[string]any{"name": "search_threads", "description": "search", "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "label_message", "description": "write", "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "create_event", "description": "write", "inputSchema": map[string]any{"type": "object"}},
				map[string]any{"name": "list_events", "description": "read", "inputSchema": map[string]any{"type": "object"}},
			}}
		case "tools/call":
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}
		default:
			t.Errorf("unexpected method %s", in.Method)
		}
		payload, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": result})
		if mode == "sse" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(payload)
	}))
}

func testBridge(t *testing.T, server, endpoint string, grant bool) (*bridge, *bytes.Buffer) {
	t.Helper()
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", testHexKey)
	key, err := connectors.KeyFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	store := connectors.NewMemoryStore()
	if grant {
		sealed, err := connectors.EncryptToken(key, "r123")
		if err != nil {
			t.Fatal(err)
		}
		connector := connectors.Gmail
		if server == "calendar" {
			connector = connectors.Calendar
		}
		if err := store.SetTokens(context.Background(), connector, connectors.TokenRow{RefreshCipher: sealed}); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CONNECTORS_MCP_ENDPOINT", endpoint)
	cfg, err := bridgeConfig(server)
	if err != nil {
		t.Fatal(err)
	}
	oauth := &connectors.OAuthConfig{ClientID: "cid", ClientSecret: "sec", Key: key, TokenURL: endpoint + "/t"}
	var out bytes.Buffer
	return &bridge{cfg: cfg, store: store, oauth: oauth, out: bufio.NewWriter(&out)}, &out
}

func lastResponse(t *testing.T, out *bytes.Buffer) rpcResponse {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var res rpcResponse
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &res); err != nil {
		t.Fatalf("bad reply: %v", err)
	}
	return res
}

func TestListFiltersWrites(t *testing.T) {
	srv := stubUpstream(t, "json", 0, nil)
	defer srv.Close()
	b, out := testBridge(t, "gmail", srv.URL, true)
	b.handleLine([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	b.out.Flush()
	res := lastResponse(t, out)
	if res.Error != nil {
		t.Fatalf("tools/list failed: %+v", res.Error)
	}
	got, _ := json.Marshal(res.Result)
	if strings.Contains(string(got), "label_message") {
		t.Errorf("label write leaked into listing: %s", got)
	}
	if !strings.Contains(string(got), "search_threads") {
		t.Errorf("read tool missing from listing: %s", got)
	}
}

func TestListFiltersCalendarWritesAndAddsDraft(t *testing.T) {
	srv := stubUpstream(t, "json", 0, nil)
	defer srv.Close()
	b, out := testBridge(t, "calendar", srv.URL, true)
	b.handleLine([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`))
	b.out.Flush()
	res := lastResponse(t, out)
	if res.Error != nil {
		t.Fatalf("tools/list failed: %+v", res.Error)
	}
	got, _ := json.Marshal(res.Result)
	for _, want := range []string{"list_events", "create_event_draft"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("listing missing %s: %s", want, got)
		}
	}
	if strings.Contains(string(got), "create_event\"") {
		t.Errorf("create_event write leaked into listing: %s", got)
	}
}

func TestCallForwardsWithBearer(t *testing.T) {
	var auth string
	srv := stubUpstream(t, "json", 0, &auth)
	defer srv.Close()
	b, out := testBridge(t, "gmail", srv.URL, true)
	b.handleLine([]byte(`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"search_threads","arguments":{"q":"x"}}}`))
	b.out.Flush()
	res := lastResponse(t, out)
	if res.Error != nil {
		t.Fatalf("tools/call failed: %+v", res.Error)
	}
	// Sealed refresh only: the bridge refreshed at the stub token
	// endpoint and attached the live access token upstream.
	if auth != "Bearer a456" {
		t.Errorf("authorization = %q, want refreshed bearer", auth)
	}
	got, _ := json.Marshal(res.Result)
	if !strings.Contains(string(got), "ok") {
		t.Errorf("forwarded result = %s", got)
	}
}

func TestCallNeedsGrant(t *testing.T) {
	srv := stubUpstream(t, "json", 0, nil)
	defer srv.Close()
	b, out := testBridge(t, "gmail", srv.URL, false)
	b.handleLine([]byte(`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"search_threads","arguments":{}}}`))
	b.out.Flush()
	res := lastResponse(t, out)
	if res.Error == nil || !strings.Contains(res.Error.Message, "reconnect") {
		t.Fatalf("grant-less call = %+v, want reconnect error", res.Error)
	}
}

func TestExcludedCallRefuses(t *testing.T) {
	srv := stubUpstream(t, "json", 0, nil)
	defer srv.Close()
	b, out := testBridge(t, "calendar", srv.URL, true)
	b.handleLine([]byte(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"delete_event","arguments":{"id":"e"}}}`))
	b.out.Flush()
	res := lastResponse(t, out)
	if res.Error == nil {
		t.Fatal("filtered write call should refuse, not forward")
	}
}

func TestSSEFraming(t *testing.T) {
	srv := stubUpstream(t, "sse", 0, nil)
	defer srv.Close()
	b, out := testBridge(t, "gmail", srv.URL, true)
	b.handleLine([]byte(`{"jsonrpc":"2.0","id":6,"method":"tools/list"}`))
	b.out.Flush()
	res := lastResponse(t, out)
	if res.Error != nil {
		t.Fatalf("sse tools/list failed: %+v", res.Error)
	}
}

func TestUpstream401IsReconnect(t *testing.T) {
	srv := stubUpstream(t, "json", http.StatusUnauthorized, nil)
	defer srv.Close()
	b, out := testBridge(t, "gmail", srv.URL, true)
	// The stub refresh succeeds (live bearer), then the MCP endpoint
	// itself 401s: the grant is dead on Google's side.
	b.handleLine([]byte(`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"search_threads","arguments":{}}}`))
	b.out.Flush()
	res := lastResponse(t, out)
	if res.Error == nil || !strings.Contains(res.Error.Message, "reconnect") {
		t.Fatalf("401 = %+v, want reconnect error", res.Error)
	}
}

func TestDraftValidation(t *testing.T) {
	for _, tc := range []struct {
		name                string
		summary, start, end string
		wantErr             string
	}{
		{"timed", "1:1", "2026-10-26T09:00:00Z", "2026-10-26T09:30:00Z", ""},
		{"allday", "Off", "2026-10-27", "2026-10-27", ""},
		{"missing summary", "", "2026-10-26T09:00:00Z", "2026-10-26T10:00:00Z", "summary"},
		{"missing start", "1:1", "", "2026-10-26T10:00:00Z", "start and an end"},
		{"bad start", "1:1", "monday", "2026-10-26T10:00:00Z", "RFC3339"},
		{"end before start", "1:1", "2026-10-26T10:00:00Z", "2026-10-26T09:00:00Z", "after start"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := buildDraft(tc.summary, "", "", tc.start, tc.end, nil)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDraftCallIsLocal(t *testing.T) {
	srv := stubUpstream(t, "json", 0, nil)
	defer srv.Close()
	b, out := testBridge(t, "calendar", srv.URL, false)
	// No grant needed: drafts never touch Google.
	b.handleLine([]byte(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"create_event_draft","arguments":{"summary":"1:1","start":"2026-10-26T09:00:00Z","end":"2026-10-26T09:30:00Z"}}}`))
	b.out.Flush()
	res := lastResponse(t, out)
	if res.Error != nil {
		t.Fatalf("draft failed: %+v", res.Error)
	}
	got, _ := json.Marshal(res.Result)
	if !strings.Contains(string(got), "1:1") || !strings.Contains(string(got), "draft only") {
		t.Errorf("draft result = %s", got)
	}
}
