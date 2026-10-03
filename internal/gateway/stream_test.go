package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agento/internal/pi"
)

// sseFrames parses the data lines out of an SSE body, the same way the app does:
// skip ':' comments and 'event:' lines, split the rest on "data: " and stop at
// [DONE].
func sseFrames(t *testing.T, body string) (payloads []string, sawDone bool) {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			return payloads, sawDone
		}
		payloads = append(payloads, data)
	}
	return payloads, sawDone
}

// deltas pulls choices[0].delta.content out of every frame that carries one.
func deltas(t *testing.T, payloads []string) []string {
	t.Helper()
	var out []string
	for _, p := range payloads {
		var frame struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(p), &frame); err != nil {
			t.Fatalf("frame is not JSON: %q (%v)", p, err)
		}
		if len(frame.Choices) == 0 {
			continue
		}
		if c := frame.Choices[0].Delta.Content; c != "" {
			out = append(out, c)
		}
	}
	return out
}

func TestStreamEmitsDeltasThenDone(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{
		textDelta(t, "Hel"),
		textDelta(t, "lo"),
		{Type: pi.TypeAgentSettled},
	}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type=%q, want text/event-stream", ct)
	}
	// nginx buffers proxied responses unless told not to, which would hold the
	// whole turn until it finished.
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Fatal("streaming response must set X-Accel-Buffering: no or nginx will buffer it")
	}

	payloads, sawDone := sseFrames(t, rec.Body.String())
	if !sawDone {
		t.Fatal("stream did not terminate with [DONE]")
	}
	if got := strings.Join(deltas(t, payloads), ""); got != "Hello" {
		t.Fatalf("assembled %q, want %q", got, "Hello")
	}
}

// The app renders reasoning as a collapsible section, read from
// choices[0].delta.reasoning_content.
func TestStreamCarriesReasoningDeltas(t *testing.T) {
	agent := newFakeAgent()
	reasoning, err := json.Marshal(map[string]any{
		"type":                  "message_update",
		"assistantMessageEvent": map[string]any{"type": "thinking_delta", "contentIndex": 0, "delta": "pondering"},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.events = []pi.Record{
		{Type: "message_update", Raw: reasoning},
		textDelta(t, "answer"),
		{Type: pi.TypeAgentSettled},
	}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	payloads, _ := sseFrames(t, rec.Body.String())

	var reasoningSeen string
	for _, p := range payloads {
		var frame struct {
			Choices []struct {
				Delta struct {
					ReasoningContent string `json:"reasoning_content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(p), &frame); err != nil {
			t.Fatalf("frame not JSON: %q", p)
		}
		if len(frame.Choices) > 0 {
			reasoningSeen += frame.Choices[0].Delta.ReasoningContent
		}
	}
	if reasoningSeen != "pondering" {
		t.Fatalf("reasoning delta = %q, want %q", reasoningSeen, "pondering")
	}
}

// Tool frames carry `tool` and NO choices array; the app keys on that to route
// them separately from assistant text.
func TestStreamEmitsToolProgressFrames(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{
		toolStart(t, "bash"),
		{Type: pi.TypeAgentSettled},
	}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	payloads, _ := sseFrames(t, rec.Body.String())

	found := false
	for _, p := range payloads {
		var frame struct {
			Tool    string          `json:"tool"`
			Status  string          `json:"status"`
			Choices json.RawMessage `json:"choices"`
		}
		if err := json.Unmarshal([]byte(p), &frame); err != nil {
			t.Fatalf("frame not JSON: %q", p)
		}
		if frame.Tool != "" {
			found = true
			if frame.Choices != nil {
				t.Fatalf("a tool frame must not carry choices, got %q", frame.Choices)
			}
			if frame.Status != "start" {
				t.Fatalf("tool status = %q, want start", frame.Status)
			}
		}
	}
	if !found {
		t.Fatalf("no tool frame emitted: %v", payloads)
	}
}

// A tool event with no tool name cannot be attributed, and the app skips those —
// so the gateway must not invent a name.
func TestToolFrameWithoutNameIsSkipped(t *testing.T) {
	agent := newFakeAgent()
	nameless, err := json.Marshal(map[string]any{"type": "tool_execution_start"})
	if err != nil {
		t.Fatal(err)
	}
	agent.events = []pi.Record{
		{Type: "tool_execution_start", Raw: nameless},
		{Type: pi.TypeAgentSettled},
	}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	frames, _ := sseFrames(t, rec.Body.String())
	for _, p := range frames {
		if strings.Contains(p, `"tool"`) {
			t.Fatalf("emitted a tool frame with no tool name: %s", p)
		}
	}
}

// Usage goes on its own final frame before [DONE]; the app takes the last frame
// that carries counts, so placement is flexible but it must be present.
func TestStreamCarriesUsageBeforeDone(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{
		textDelta(t, "hi"),
		{Type: pi.TypeAgentSettled},
	}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	body := rec.Body.String()
	payloads, sawDone := sseFrames(t, body)
	if !sawDone {
		t.Fatal("no [DONE]")
	}

	// textDelta carries usage {input:10, output:2, totalTokens:12}.
	var found bool
	for _, p := range payloads {
		var frame struct {
			Usage *usage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(p), &frame); err != nil {
			continue
		}
		if frame.Usage.nonZero() {
			found = true
			if frame.Usage.PromptTokens != 10 || frame.Usage.CompletionTokens != 2 {
				t.Fatalf("usage mapped wrongly: %+v", frame.Usage)
			}
		}
	}
	if !found {
		t.Fatalf("no usage frame emitted; frames=%v", payloads)
	}
}

// An all-zero usage frame is omitted: reporting zero tokens for a completed turn
// is worse than reporting nothing.
func TestZeroUsageIsOmitted(t *testing.T) {
	zero, err := json.Marshal(map[string]any{
		"type":                  "message_update",
		"usage":                 map[string]any{"input": 0, "output": 0, "totalTokens": 0},
		"assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := newFakeAgent()
	agent.events = []pi.Record{{Type: "message_update", Raw: zero}, {Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	frames, _ := sseFrames(t, rec.Body.String())
	for _, p := range frames {
		if strings.Contains(p, `"usage"`) {
			t.Fatalf("emitted a zero usage frame: %s", p)
		}
	}
}

// The agent dying mid-turn must produce a terminal error frame and no [DONE],
// matching what the app does for an HTTP error.
func TestAgentDeathMidTurnEmitsTerminalError(t *testing.T) {
	agent := newFakeAgent()
	// No agent_settled: this test is about the agent disappearing mid-turn, so the
	// fake must not quietly complete the turn first.
	agent.noSettle = true
	srv := newTestServer(t, agent, Config{})

	// Drive the turn on a goroutine and kill the agent once it is subscribed.
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- post(t, srv, "/v1/chat/completions", "test-token", validBody) }()

	// Let the request reach the subscribe point.
	time.Sleep(50 * time.Millisecond)
	agent.die()

	select {
	case rec := <-done:
		body := rec.Body.String()
		_, sawDone := sseFrames(t, body)
		if sawDone {
			t.Fatalf("a failed turn must not end with [DONE]: %s", body)
		}
		if !strings.Contains(body, `"error"`) {
			t.Fatalf("a failed turn must emit an error frame: %s", body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("request did not return after the agent died")
	}
}

func TestBufferedResponse(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{
		textDelta(t, "one "),
		textDelta(t, "two"),
		{Type: pi.TypeAgentSettled},
	}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model_options":{"reasoning_effort":"low"},"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	var body completion
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not a completion object: %v (%s)", err, rec.Body)
	}
	if len(body.Choices) != 1 || body.Choices[0].Message.Content != "one two" {
		t.Fatalf("buffered content = %+v", body.Choices)
	}
	if body.Object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", body.Object)
	}
	if body.Choices[0].Finish != "stop" {
		t.Fatalf("finish_reason = %q, want stop", body.Choices[0].Finish)
	}
}

// A non-streamed response must speak the shape a standard OpenAI client reads:
// choices[0].message. It used to reuse the chunk type and emit choices[0].delta,
// so such a client got a 200 with no message in it. The app always streams and
// never reaches this path, which is exactly why nothing caught it.
//
// Asserted on the raw JSON rather than only through the struct: unmarshalling into
// a struct would silently accept the wrong shape and leave the field empty, which
// is the failure itself.
func TestBufferedUsesMessageShapeNotDelta(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{textDelta(t, "hello"), {Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model_options":{"reasoning_effort":"low"},"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}

	raw := rec.Body.String()
	if !strings.Contains(raw, `"message"`) {
		t.Fatalf("non-streamed response has no message field: %s", raw)
	}
	if strings.Contains(raw, `"delta"`) {
		t.Fatalf("non-streamed response used the streaming delta shape: %s", raw)
	}

	// What a standard client's parser sees.
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("a standard client's parse failed: %v (%s)", err, raw)
	}
	if len(parsed.Choices) != 1 || parsed.Choices[0].Message.Content != "hello" {
		t.Fatalf("standard client read %+v", parsed.Choices)
	}
}

func TestProfileRouting(t *testing.T) {
	defaultAgent := newFakeAgent()
	storyAgent := newFakeAgent()
	srv, err := New(Config{Password: "tok"},
		map[string]Agent{"default": defaultAgent, "story": storyAgent})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, tc := range []struct {
		path string
		want *fakeAgent
	}{
		{"/v1/chat/completions", defaultAgent},         // no profile -> default
		{"/default/v1/chat/completions", defaultAgent}, // explicit default
		{"/story/v1/chat/completions", storyAgent},     // explicit profile
	} {
		before := tc.want.commandCount("prompt")
		other := defaultAgent
		if tc.want == defaultAgent {
			other = storyAgent
		}
		otherBefore := other.commandCount("prompt")

		rec := post(t, srv, tc.path, "tok", validBody)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200. body=%s", tc.path, rec.Code, rec.Body)
		}
		if got := tc.want.commandCount("prompt"); got != before+1 {
			t.Fatalf("%s: routed to the wrong agent (prompt count %d -> %d)", tc.path, before, got)
		}
		if got := other.commandCount("prompt"); got != otherBefore {
			t.Fatalf("%s: the other agent was prompted too", tc.path)
		}
	}
}

func TestUnknownProfileIs404(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{})
	rec := post(t, srv, "/nosuchprofile/v1/chat/completions", "test-token", validBody)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404. body=%s", rec.Code, rec.Body)
	}
}

// Two conversations must not be able to claim the same Pi session, or their
// transcripts would interleave.
func TestSessionCannotBeSharedBetweenConversations(t *testing.T) {
	store := NewSessionStore()
	if err := store.Remember("conv-1", "/s/1.jsonl"); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if err := store.Remember("conv-2", "/s/1.jsonl"); err == nil {
		t.Fatal("a second conversation claimed a session already owned by another")
	}
	if err := store.Remember("conv-1", "/s/2.jsonl"); err != nil {
		t.Fatalf("re-binding a conversation to a new session should be allowed: %v", err)
	}
	if _, ok := store.Lookup("conv-1"); !ok {
		t.Fatal("conversation lost after re-binding")
	}
	// The old file was released, so another conversation may now adopt it.
	if err := store.Remember("conv-3", "/s/1.jsonl"); err != nil {
		t.Fatalf("released session should be adoptable: %v", err)
	}
}

// The app sends its conversation id on every turn; a repeat must resume the same
// Pi session rather than starting fresh.
func TestConversationResumesSameSession(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	send := func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("X-Hermes-Session-Id", "conv-42")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
		}
	}

	send()
	send()
	send()

	// Three turns in one conversation, so exactly one session file was claimed.
	handler := srv.handlers["default"]
	if got := handler.sessions.Len(); got != 1 {
		t.Fatalf("conversation mapped to %d sessions, want 1", got)
	}
	// Having claimed it, subsequent turns should be checking rather than claiming.
	if agent.commandCount("get_state") < 3 {
		t.Fatalf("expected a state check per turn, got %d", agent.commandCount("get_state"))
	}
}

// Only the trailing user turn is sent: Pi holds history in its own session file,
// so replaying the transcript would duplicate context and cost tokens for nothing.
func TestOnlyLastUserMessageIsSent(t *testing.T) {
	prompt, err := userPrompt([]chatMessage{
		{Role: "user", Content: chatContent{Text: "first"}},
		{Role: "assistant", Content: chatContent{Text: "reply"}},
		{Role: "user", Content: chatContent{Text: "second"}},
	})
	if err != nil {
		t.Fatalf("userPrompt: %v", err)
	}
	if prompt != "second" {
		t.Fatalf("prompt = %q, want %q", prompt, "second")
	}

	// No user turn at all is an error, not an empty prompt.
	if _, err := userPrompt([]chatMessage{{Role: "assistant", Content: chatContent{Text: "x"}}}); err == nil {
		t.Fatal("expected an error when no user message is present")
	}
	if _, err := userPrompt([]chatMessage{{Role: "user", Content: chatContent{Text: "   "}}}); err == nil {
		t.Fatal("expected an error for a blank user message")
	}
}

func TestMalformedBodyRejected(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{})
	for _, body := range []string{`not json`, `{"messages":[]}{"extra":1}`, `{}`} {
		rec := post(t, srv, "/v1/chat/completions", "test-token", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: got %d, want 400", body, rec.Code)
		}
	}
}

func TestEmptyMessagesRejected(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{})
	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model_options":{"reasoning_effort":"low"},"messages":[],"stream":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400. body=%s", rec.Code, rec.Body)
	}
}

func TestUnsupportedContentTypeRejected(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("got %d, want 415. body=%s", rec.Code, rec.Body)
	}
}

func TestAgentCommandFailureIsSurfaced(t *testing.T) {
	agent := newFakeAgent()
	agent.failCommands = map[string]error{"get_available_thinking_levels": fmt.Errorf("agent is down")}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502. body=%s", rec.Code, rec.Body)
	}
}

// Two conversations against one agent must get separate Pi sessions. Pi holds a
// single current session per process, so a second conversation arriving while the
// first owns it must start a fresh one rather than 502 or, worse, interleave into
// the first conversation's transcript.
func TestSecondConversationStartsItsOwnSession(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	send := func(conversation string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("X-Hermes-Session-Id", conversation)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	if rec := send("conv-A"); rec.Code != http.StatusOK {
		t.Fatalf("first conversation: got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	// The second conversation must not fail because the live session is taken.
	if rec := send("conv-B"); rec.Code != http.StatusOK {
		t.Fatalf("second conversation: got %d, want 200. body=%s", rec.Code, rec.Body)
	}

	if !agent.sawCommand("new_session") {
		t.Fatal("a new conversation did not start its own Pi session")
	}

	store := srv.handlers["default"].sessions
	if got := store.Len(); got != 2 {
		t.Fatalf("store holds %d conversations, want 2", got)
	}

	// And the first conversation still resumes onto its own session afterwards.
	if rec := send("conv-A"); rec.Code != http.StatusOK {
		t.Fatalf("first conversation resuming: got %d, want 200", rec.Code)
	}
	if !agent.sawCommand("switch_session") {
		t.Fatal("resuming a conversation did not switch Pi back to its session")
	}
}

// A conversation with no id must not be tracked: it shares Pi's live session and
// would otherwise squat on it, pushing the next real conversation onto a new one.
func TestUntrackedRequestDoesNotClaimTheSession(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if got := srv.handlers["default"].sessions.Len(); got != 0 {
		t.Fatalf("an untracked request claimed %d sessions; it should claim none", got)
	}
	if agent.sawCommand("new_session") {
		t.Fatal("an untracked request started a new session")
	}
}
