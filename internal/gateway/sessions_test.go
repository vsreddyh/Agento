package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSession writes a pi session file from raw JSONL lines and returns its path. Lines
// are given verbatim so a test can include a deliberately corrupt one, which is the state
// a session file is actually in while a turn is streaming.
func writeSession(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}

// The fixture mirrors the shape pi actually writes: one JSON object per line, `type:
// "message"`, tool calls as `toolCall` parts with arguments as an OBJECT, and results as
// separate `role: "toolResult"` messages carrying the tool's name.
var (
	lineSession    = `{"type":"session","id":"s1","cwd":"/workspace","version":1}`
	lineModel      = `{"type":"model_change","modelId":"mimo-v2.6-flash"}`
	lineSystem     = `{"type":"message","message":{"role":"system","content":[]}}`
	lineUser       = `{"type":"message","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`
	lineToolCall   = `{"type":"message","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hmm"},{"type":"toolCall","name":"skill","id":"call_1","arguments":{"name":"podman-management","path":"/opt/pi/skills/podman-management/SKILL.md"}}],"usage":{"input":100,"output":20,"cacheRead":5,"cacheWrite":1,"reasoning":7,"totalTokens":125}}}`
	lineToolResult = `{"type":"message","message":{"role":"toolResult","toolName":"skill","content":[{"type":"text","text":"loaded"}]}}`
	lineAnswer     = `{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"usage":{"input":50,"output":10,"cacheRead":0,"cacheWrite":0,"reasoning":3,"totalTokens":63}}}`
	// A turn mid-write leaves a truncated final line. Skipping it is the whole reason the
	// parser tolerates unparseable lines.
	lineCorrupt = `{"type":"message","message":{"role":"assistant"`
)

func fullSession(t *testing.T) string {
	t.Helper()
	return writeSession(t, lineSession, lineModel, lineSystem, lineUser,
		lineToolCall, lineToolResult, lineAnswer, lineCorrupt)
}

func TestParsePiSession(t *testing.T) {
	messages, totals, err := parsePiSession(fullSession(t))
	if err != nil {
		t.Fatalf("parsePiSession: %v", err)
	}

	// The system record is not a message the app should see, and a tool result must be its
	// own row or the app cannot attribute the tool.
	wantRoles := []string{"user", "assistant", "tool", "assistant"}
	if len(messages) != len(wantRoles) {
		t.Fatalf("got %d messages %+v, want %d", len(messages), messages, len(wantRoles))
	}
	for i, want := range wantRoles {
		if messages[i].Role != want {
			t.Errorf("message[%d].Role = %q, want %q", i, messages[i].Role, want)
		}
	}

	// The tool call, and the reasoning part that must NOT become one.
	if len(messages[1].ToolCalls) != 1 {
		t.Fatalf("assistant row has %d tool_calls, want 1: %+v",
			len(messages[1].ToolCalls), messages[1])
	}
	fn := messages[1].ToolCalls[0].Function
	if fn.Name != "skill" {
		t.Errorf("tool name = %q, want %q", fn.Name, "skill")
	}
	// The app regex-scans arguments as a STRING for SKILL.md paths to attribute skills.
	// If this were an object, or pretty-printed, skill attribution silently yields nothing.
	if !strings.Contains(fn.Arguments, "podman-management/SKILL.md") {
		t.Errorf("arguments = %q, want it to carry the SKILL.md path", fn.Arguments)
	}
	if strings.ContainsAny(fn.Arguments, "\n\t") {
		t.Errorf("arguments = %q, want compact (the app scans this per call)", fn.Arguments)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(fn.Arguments), &decoded); err != nil {
		t.Errorf("arguments %q is not valid JSON: %v", fn.Arguments, err)
	}

	if messages[2].ToolName != "skill" {
		t.Errorf("tool row tool_name = %q, want %q", messages[2].ToolName, "skill")
	}
	if len(messages[3].ToolCalls) != 0 {
		t.Errorf("a text-only reply must carry no tool_calls, got %+v", messages[3].ToolCalls)
	}

	// usage is per assistant message, so totals are the sum across turns.
	if totals.InputTokens != 150 || totals.OutputTokens != 30 {
		t.Errorf("prompt/completion = %d/%d, want 150/30",
			totals.InputTokens, totals.OutputTokens)
	}
	if totals.CacheReadTokens != 5 || totals.CacheWriteTokens != 1 {
		t.Errorf("cache read/write = %d/%d, want 5/1",
			totals.CacheReadTokens, totals.CacheWriteTokens)
	}
	// Reasoning is decoded off the shared piUsage, which the streaming path ignores.
	if totals.ReasoningTokens != 10 {
		t.Errorf("reasoning = %d, want 10", totals.ReasoningTokens)
	}
	if totals.TotalTokens != 188 {
		t.Errorf("total = %d, want 188 (125 + 63)", totals.TotalTokens)
	}
	if totals.MessageCount != 3 {
		t.Errorf("message_count = %d, want 3 (one user row + two assistant rows)",
			totals.MessageCount)
	}
	if totals.Model != "mimo-v2.6-flash" {
		t.Errorf("model = %q, want %q", totals.Model, "mimo-v2.6-flash")
	}
}

// pi does not give the model each MCP tool as its own callable. It gives it one tool,
// `codemode`, whose argument is a JS snippet, and the MCP tools appear inside the snippet as
// tools.mcp__<server>__<tool>(). The real names are only on the result, under nestedCalls.
//
// A chip built from the toolCall alone therefore reads "codemode", which tells the user
// nothing, and its arguments are the snippet rather than the MCP call's own arguments —
// which is where SKILL.md paths live, so skill attribution breaks too.
func TestParsePiSessionExpandsCodemodeIntoRealMCPToolNames(t *testing.T) {
	path := writeSession(t,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"balances?"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[`+
			`{"type":"toolCall","id":"call_1","name":"codemode","arguments":{"code":"const r = await tools.mcp__miser_money__get_balances({});\ntext(r);"}}`+
			`]}}`,
		`{"type":"message","message":{"role":"toolResult","toolName":"codemode","toolCallId":"call_1",`+
			`"nestedCalls":{"complete":true,"calls":[`+
			`{"name":"mcp__miser_money__get_balances","arguments":{},"status":"ok","durationMs":187},`+
			`{"name":"mcp__cookbook__search","arguments":{"q":"dal"},"status":"ok"}]}}}`)

	messages, _, err := parsePiSession(path)
	if err != nil {
		t.Fatalf("parsePiSession: %v", err)
	}
	if len(messages) != 4 {
		t.Fatalf("got %d rows %+v, want 4 (user, assistant, tool, tool)", len(messages), messages)
	}

	// One chip per MCP tool actually called — not one chip for the snippet.
	calls := messages[1].ToolCalls
	if len(calls) != 2 {
		t.Fatalf("assistant row has %d tool_calls, want 2: %+v", len(calls), calls)
	}
	for i, want := range []string{"mcp__miser_money__get_balances", "mcp__cookbook__search"} {
		if got := calls[i].Function.Name; got != want {
			t.Errorf("tool_calls[%d].name = %q, want %q", i, got, want)
		}
	}
	// The nested call's own arguments, not the JS wrapper: this is what the app scans for
	// SKILL.md paths, so the snippet would attribute nothing.
	if got := calls[1].Function.Arguments; !strings.Contains(got, "dal") {
		t.Errorf("tool_calls[1].arguments = %q, want the nested call's arguments", got)
	}
	for _, r := range messages[2:] {
		if r.Role != "tool" || r.ToolName == "codemode" {
			t.Errorf("tool row = %+v, want the real MCP name", r)
		}
	}
	if messages[2].ToolName != "mcp__miser_money__get_balances" {
		t.Errorf("tool row 0 = %q, want get_balances", messages[2].ToolName)
	}
}

// A tool with no nested calls keeps its own name. `bash` is called directly by the model and
// has no nestedCalls, so expanding on name alone would be wrong.
func TestParsePiSessionKeepsDirectToolNames(t *testing.T) {
	path := writeSession(t,
		`{"type":"message","message":{"role":"user","content":[{"type":"text","text":"date?"}]}}`,
		`{"type":"message","message":{"role":"assistant","content":[`+
			`{"type":"toolCall","id":"c9","name":"bash","arguments":{"command":"date -u +%F"}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolName":"bash","toolCallId":"c9","isError":false}}`)

	messages, _, err := parsePiSession(path)
	if err != nil {
		t.Fatalf("parsePiSession: %v", err)
	}
	if len(messages[1].ToolCalls) != 1 || messages[1].ToolCalls[0].Function.Name != "bash" {
		t.Errorf("tool_calls = %+v, want the single bash call preserved", messages[1].ToolCalls)
	}
	if messages[2].Role != "tool" || messages[2].ToolName != "bash" {
		t.Errorf("tool row = %+v, want tool_name bash", messages[2])
	}
}

// A codemode result whose calls are all unnamed is not evidence of anything, so the wrapper
// name stands rather than the chip disappearing.
func TestParsePiSessionFallsBackWhenNestedNamesAreEmpty(t *testing.T) {
	path := writeSession(t,
		`{"type":"message","message":{"role":"assistant","content":[`+
			`{"type":"toolCall","id":"c1","name":"codemode","arguments":{"code":"x"}}]}}`,
		`{"type":"message","message":{"role":"toolResult","toolName":"codemode","toolCallId":"c1",`+
			`"nestedCalls":{"complete":false,"calls":[]}}}`)

	messages, _, err := parsePiSession(path)
	if err != nil {
		t.Fatalf("parsePiSession: %v", err)
	}
	if len(messages[0].ToolCalls) != 1 || messages[0].ToolCalls[0].Function.Name != "codemode" {
		t.Errorf("tool_calls = %+v, want codemode preserved", messages[0].ToolCalls)
	}
	if len(messages) != 2 || messages[1].ToolName != "codemode" {
		t.Errorf("rows = %+v, want the wrapper tool row kept", messages)
	}
}

// Total is derived when pi reports none, because the app displays it and a hard 0 would be
// indistinguishable from a real measurement.
func TestParsePiSessionDerivesTotalWhenAbsent(t *testing.T) {
	path := writeSession(t, lineSession,
		`{"type":"message","message":{"role":"assistant","content":[],"usage":{"input":7,"output":3}}}`)
	_, totals, err := parsePiSession(path)
	if err != nil {
		t.Fatalf("parsePiSession: %v", err)
	}
	if totals.TotalTokens != 10 {
		t.Errorf("total = %d, want 10 derived from prompt+completion", totals.TotalTokens)
	}
}

func TestSessionIDFromPath(t *testing.T) {
	cases := []struct {
		path         string
		wantID       string
		wantMessages bool
		wantOK       bool
	}{
		{"api/sessions/abc", "abc", false, true},
		{"api/sessions/abc/messages", "abc", true, true},
		{"api/sessions/abc/", "abc", false, true},
		{"api/sessions/abc/messages/", "abc", true, true},
		// A traversal in the id would be joined onto a directory to reach the file.
		{"api/sessions/..", "", false, false},
		{"api/sessions/.", "", false, false},
		{"api/sessions/", "", false, false},
		// A further segment is a typo, not a route.
		{"api/sessions/abc/tools", "", false, false},
		{"v1/chat/completions", "", false, false},
		{"api/model/options", "", false, false},
		// Must not match mid-path.
		{"v1/api/sessions/abc", "", false, false},
	}
	for _, c := range cases {
		id, msgs, ok := sessionIDFromPath(c.path)
		if ok != c.wantOK || id != c.wantID || msgs != c.wantMessages {
			t.Errorf("sessionIDFromPath(%q) = (%q, %v, %v), want (%q, %v, %v)",
				c.path, id, msgs, ok, c.wantID, c.wantMessages, c.wantOK)
		}
	}
}

// An unknown conversation is a normal state — a thread id the gateway has not seen, or one
// evicted by the store's LRU bound — and the app falls back to its own device sums. So it
// is a 200 with an empty result, not a 404 that the app would log as a failure.
func TestSessionRoutesUnknownConversation(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{}, Config{})

	rec := get(t, srv, "/api/sessions/never-seen/messages", "test-token")
	if rec.Code != http.StatusOK {
		t.Errorf("messages status = %d, want 200", rec.Code)
	}
	// An empty array, not null: the app's rootArray helper raises on a null body.
	if got := strings.TrimSpace(rec.Body.String()); got != "[]" {
		t.Errorf("messages body = %q, want %q", got, "[]")
	}

	rec = get(t, srv, "/api/sessions/never-seen", "test-token")
	if rec.Code != http.StatusOK {
		t.Errorf("totals status = %d, want 200", rec.Code)
	}
	// The app reads a non-empty `error` as "absent, not fatal".
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("totals body %q: %v", rec.Body.String(), err)
	}
	if e, _ := body["error"].(string); strings.TrimSpace(e) == "" {
		t.Errorf("totals body = %v, want a non-empty error the app reads as absent", body)
	}
}

func TestSessionRoutesServeAKnownConversation(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{}, Config{})
	h := srv.handlers["default"]
	if h == nil {
		t.Fatal("no handler for the default profile")
	}
	if err := h.sessions.Remember("conv-1", fullSession(t)); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	rec := get(t, srv, "/api/sessions/conv-1/messages", "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("messages status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var msgs []appMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &msgs); err != nil {
		t.Fatalf("messages body %q: %v", rec.Body.String(), err)
	}
	if len(msgs) != 4 {
		t.Fatalf("got %d messages, want 4: %+v", len(msgs), msgs)
	}
	if msgs[2].Role != "tool" || msgs[2].ToolName != "skill" {
		t.Errorf("row 2 = %+v, want a tool row named skill", msgs[2])
	}

	rec = get(t, srv, "/api/sessions/conv-1", "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("totals status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var wrapped struct {
		Session *sessionTotals `json:"session"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wrapped); err != nil {
		t.Fatalf("totals body %q: %v", rec.Body.String(), err)
	}
	if wrapped.Session == nil {
		t.Fatalf("totals body has no `session` object: %s", rec.Body.String())
	}
	if wrapped.Session.InputTokens != 150 || wrapped.Session.CacheReadTokens != 5 {
		t.Errorf("totals = %+v, want input 150 and cache_read 5", *wrapped.Session)
	}
}

// Both routes are read-only. Saying so with 405 rather than 404 tells a client whether to
// fix its verb or its path.
func TestSessionRoutesRejectPost(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{}, Config{})
	rec := post(t, srv, "/api/sessions/conv-1", "test-token", "{}")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
		t.Errorf("Allow = %q, want GET", allow)
	}
}

// Both routes sit behind the bearer token like everything else: a session transcript is
// the conversation itself.
func TestSessionRoutesRequireAuth(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{}, Config{})
	req := httptest.NewRequest(http.MethodGet, "/api/sessions/conv-1/messages", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}
