package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agento/internal/pi"
)

// Two concurrent turns on one profile would race Pi's single session and
// interleave their deltas, because Subscribe broadcasts to every subscriber. The
// second must be refused, not allowed to interleave.
func TestConcurrentTurnsOnOneProfileAreRefused(t *testing.T) {
	agent := newFakeAgent()
	// Hold the first turn open so it is provably still in flight.
	release := make(chan struct{})
	agent.onPrompt = func() { <-release }

	srv := newTestServer(t, agent, Config{})

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- post(t, srv, "/v1/chat/completions", "test-token", validBody) }()

	// Wait for the first turn to be inside the handler.
	waitFor(t, func() bool { return agent.sawCommand("prompt") }, "first turn to reach the agent")

	second := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	if second.Code != http.StatusTooManyRequests {
		t.Fatalf("concurrent turn: got %d, want 429. body=%s", second.Code, second.Body)
	}
	if !strings.Contains(second.Body.String(), "already answering") {
		t.Fatalf("429 should explain why, got %s", second.Body)
	}
	if second.Header().Get("Retry-After") == "" {
		t.Fatal("429 should carry Retry-After")
	}

	close(release)
	if rec := <-first; rec.Code != http.StatusOK {
		t.Fatalf("first turn: got %d, want 200. body=%s", rec.Code, rec.Body)
	}
}

// A different profile has its own agent and its own lock, so it is unaffected.
func TestConcurrentTurnsOnDifferentProfilesAreAllowed(t *testing.T) {
	aAgent, bAgent := newFakeAgent(), newFakeAgent()
	release := make(chan struct{})
	aAgent.onPrompt = func() { <-release }
	bAgent.onPrompt = func() {}

	srv, err := New(Config{Password: "tok"},
		map[string]Agent{"default": aAgent, "story": bAgent})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		first <- rec
	}()
	waitFor(t, func() bool { return aAgent.sawCommand("prompt") }, "default turn to start")

	// The other profile must not be blocked.
	rec := post(t, srv, "/story/v1/chat/completions", "tok", validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("other profile: got %d, want 200. body=%s", rec.Code, rec.Body)
	}

	close(release)
	<-first
}

// The keepalive ticker and the turn loop both write to the same ResponseWriter,
// which is not safe for concurrent use. Under -race this is what would surface.
func TestKeepaliveAndTurnDoNotRaceOnTheWriter(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{KeepaliveInterval: 5 * time.Millisecond})

	// A turn long enough for several keepalives to fire mid-stream.
	agent.events = nil
	agent.noSettle = true

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Drain the stream so the writer is being read while written.
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-token")
		ctx, cancel := context.WithTimeout(req.Context(), 120*time.Millisecond)
		defer cancel()
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req.WithContext(ctx))
		_ = rec
	}()

	<-done

	// The stream must still be well-formed: every frame is a complete line.
	// Interleaved writes would splice a keepalive into the middle of a frame.
	if t.Failed() {
		t.Fatal("race detected during the keepalive/turn test")
	}
}

// Every emitted line must be independently parseable — a torn frame is the
// observable symptom of an unsynchronised writer.
func TestSSEFramesAreNeverTorn(t *testing.T) {
	agent := newFakeAgent()
	agent.noSettle = true
	// Several deltas plus a settle, with keepalives interleaved.
	agent.events = []pi.Record{
		textDelta(t, "a"), textDelta(t, "b"), textDelta(t, "c"),
		{Type: pi.TypeAgentSettled},
	}
	srv := newTestServer(t, agent, Config{KeepaliveInterval: time.Millisecond})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data: ") {
			t.Fatalf("stray line in the stream: %q", line)
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		var probe map[string]any
		if err := json.Unmarshal([]byte(payload), &probe); err != nil {
			t.Fatalf("torn frame %q: %v", payload, err)
		}
	}
}

// A known endpoint reached with the wrong verb is 405, not 404: it tells the
// client to fix the method rather than hunt for a different path.
func TestWrongMethodIs405(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{Password: "tok"})
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		req := httptest.NewRequest(method, "/v1/chat/completions", nil)
		req.Header.Set("Authorization", "Bearer tok")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: got %d, want 405. body=%s", method, rec.Code, rec.Body)
		}
		if rec.Header().Get("Allow") == "" {
			t.Fatalf("%s: 405 should carry Allow", method)
		}
	}
}

// An unknown endpoint is still 404, so the two cases stay distinguishable.
func TestUnknownEndpointIs404(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{Password: "tok"})
	req := httptest.NewRequest(http.MethodPost, "/v1/nope", strings.NewReader(validBody))
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404. body=%s", rec.Code, rec.Body)
	}
}

// The completion id must be bounded: the conversation id is client-supplied, so
// echoing it verbatim would put arbitrary bytes into every frame of the stream.
func TestCompletionIDIsBoundedAndStable(t *testing.T) {
	huge := strings.Repeat("z", 100_000)
	id := completionIDFor(huge)
	if len(id) > 64 {
		t.Fatalf("completion id is %d chars for a 100k-char conversation id", len(id))
	}
	if strings.ContainsAny(id, "z") {
		t.Fatalf("completion id leaked conversation content: %q", id)
	}
	if id != completionIDFor(huge) {
		t.Fatal("completion id must be stable for the same conversation")
	}
	if id == completionIDFor("other") {
		t.Fatal("different conversations must not share a completion id")
	}
	if got := completionIDFor(""); got == "" {
		t.Fatal("empty conversation id should still produce an id")
	}
}

// The streamed id must be the bounded one, not the raw header value.
func TestStreamedCompletionIDIsBounded(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{textDelta(t, "x"), {Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	// Long but legal: the id is length-bounded, so use a value right at the limit.
	// Echoing even this verbatim would put 256 bytes of client input in every frame.
	huge := strings.Repeat("q", maxConversationIDLen)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("X-Hermes-Session-Id", huge)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, huge[:64]) {
		t.Fatal("the raw conversation id was echoed into the stream")
	}
	if !strings.Contains(body, completionIDFor(huge)) {
		t.Fatal("stream did not carry the derived completion id")
	}
}

// A content type that merely contains "json" is not JSON. The previous
// hand-rolled check accepted anything with a "/" followed by a "j".
func TestContentTypeMustReallyBeJSON(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{})

	rejected := []string{
		"garbage with space json",
		"application/x-json-ish/garbage",
		"nonsense/json",
	}
	for _, ct := range rejected {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q: got %d, want 415", ct, rec.Code)
		}
	}

	// An absent Content-Type is accepted: the body is validated as JSON anyway, so
	// a 415 here would only break clients that omit the header without making the
	// request any safer.
	accepted := []string{
		"",
		"application/json",
		"application/json; charset=utf-8",
		"APPLICATION/JSON",
		"text/json",
	}
	for _, ct := range accepted {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("Content-Type %q: got %d, want 200. body=%s", ct, rec.Code, rec.Body)
		}
	}
}

// Two conversations each claiming a session must both succeed, the second
// starting its own.
//
// Sequential on purpose: the per-profile turn lock refuses genuinely concurrent
// turns with 429, so concurrency is no longer a path to the claim conflict. This
// is the real scenario — a second chat tab opened after the first — and the
// check-then-claim guard in resolveSession stays as defence in depth.
func TestSecondFirstSeenConversationGetsItsOwnSession(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	send := func(conversation string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("X-Hermes-Session-Id", conversation)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}

	if got := send("conv-a"); got != http.StatusOK {
		t.Fatalf("first conversation: got %d, want 200", got)
	}
	if got := send("conv-b"); got != http.StatusOK {
		t.Fatalf("second conversation: got %d, want 200", got)
	}

	if agent.commandCount("new_session") == 0 {
		t.Fatal("the second conversation did not start its own session")
	}
	if got := srv.handlers["default"].sessions.Len(); got != 2 {
		t.Fatalf("store holds %d conversations, want 2", got)
	}

	// And the first still resumes onto its own session.
	if got := send("conv-a"); got != http.StatusOK {
		t.Fatalf("first conversation resuming: got %d, want 200", got)
	}
	if !agent.sawCommand("switch_session") {
		t.Fatal("resuming a conversation did not switch Pi back to its session")
	}
}

// waitFor blocks until cond holds, so a test can synchronise on an agent reaching
// a point rather than sleeping a guessed interval.
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// A slow request body must not hold the profile. Taking the turn lock before
// reading the body let one dribbling client 429 every other tab on the profile for
// up to TurnTimeout.
//
// The stalled request never returns — it is blocked reading a body that never
// arrives. What matters is that it does not hold the profile while it waits, so
// the assertion is that a *second* request still gets served.
func TestSlowBodyDoesNotHoldTheProfile(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	// A client that opens the request and sends nothing more.
	pr, _ := io.Pipe()
	stalled := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", pr)
	stalled.Header.Set("Content-Type", "application/json")
	stalled.Header.Set("Authorization", "Bearer test-token")

	stalledDone := make(chan struct{})
	go func() {
		defer close(stalledDone)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, stalled)
	}()

	// Give it a moment to reach the body read.
	time.Sleep(50 * time.Millisecond)
	select {
	case <-stalledDone:
		t.Fatal("the stalled request completed; the test is not exercising a slow body")
	default:
	}

	// A normal request must still be served: the profile lock was never taken.
	if rec := post(t, srv, "/v1/chat/completions", "test-token", validBody); rec.Code != http.StatusOK {
		t.Fatalf("a stalled body blocked the profile: got %d, want 200. body=%s",
			rec.Code, rec.Body)
	}

	// Unblock the stalled request so the test does not leak a goroutine.
	_ = pr.Close()
	select {
	case <-stalledDone:
	case <-time.After(10 * time.Second):
		t.Log("stalled request still unwinding; the assertion above already passed")
	}
}

// An oversized body is 413, distinct from a malformed one, so a client can tell
// "shrink it" from "fix its syntax".
func TestOversizedBodyIs413(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{})

	huge := `{"model_options":{"reasoning_effort":"low"},"messages":[{"role":"user","content":"` +
		strings.Repeat("x", int(maxBodyBytes)+1024) + `"}]}`
	rec := post(t, srv, "/v1/chat/completions", "test-token", huge)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d, want 413. body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "4 MiB") {
		t.Fatalf("413 should state the limit, got %s", rec.Body)
	}

	// A malformed body of a sane size stays 400.
	if rec := post(t, srv, "/v1/chat/completions", "test-token", `{"broken":`); rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed body: got %d, want 400", rec.Code)
	}
}

// An unrecognised tool event must still surface the tool as running rather than
// vanish: a Pi release adding an event should not make the tool disappear mid-run.
func TestUnrecognisedToolEventStillReportsRunning(t *testing.T) {
	agent := newFakeAgent()
	novel, err := json.Marshal(map[string]any{
		"type": "tool_execution_something_new", "toolName": "bash",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.events = []pi.Record{
		{Type: "tool_execution_something_new", Raw: novel},
		{Type: pi.TypeAgentSettled},
	}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	payloads, _ := sseFrames(t, rec.Body.String())
	var found string
	for _, p := range payloads {
		var frame toolFrame
		if err := json.Unmarshal([]byte(p), &frame); err != nil {
			continue
		}
		if frame.Tool != "" {
			found = frame.Status
		}
	}
	if found != "running" {
		t.Fatalf("an unrecognised tool event reported status %q, want %q", found, "running")
	}
}

// A structured-suffix media type is JSON.
func TestStructuredSuffixContentTypeAccepted(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	req.Header.Set("Content-Type", "application/vnd.agento.v1+json")
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("+json content type rejected: got %d. body=%s", rec.Code, rec.Body)
	}
}

// Duplicate slashes must not break routing. Asserted on splitProfile directly
// because http.ServeMux answers them with a 307 redirect before the handler ever
// runs — so an end-to-end test would be asserting the mux's behaviour, not ours.
func TestSplitProfileCollapsesDuplicateSlashes(t *testing.T) {
	srv := &Server{cfg: Config{DefaultProfile: "default"}}

	for _, tc := range []struct{ path, profile, rest string }{
		{"/story/v1/chat/completions", "story", "v1/chat/completions"},
		{"/story//v1/chat/completions", "story", "v1/chat/completions"},
		{"/v1/chat/completions", "default", "v1/chat/completions"},
		{"//v1/chat/completions", "default", "v1/chat/completions"},
		{"/v1", "default", "v1"},
		{"/", "default", ""},
	} {
		profile, rest := srv.splitProfile(tc.path)
		if profile != tc.profile || rest != tc.rest {
			t.Fatalf("splitProfile(%q) = (%q, %q), want (%q, %q)",
				tc.path, profile, rest, tc.profile, tc.rest)
		}
	}
}

// An unfamiliar event carrying a tool name renders as running; one without a tool
// name must not be mislabelled as a tool at all.
func TestUnknownEventWithoutToolNameIsNotAToolFrame(t *testing.T) {
	agent := newFakeAgent()
	// A hypothetical future event that happens to have a "tool" field for some
	// unrelated reason, with no toolName.
	decoy, err := json.Marshal(map[string]any{
		"type": "some_future_event", "tool": "not-a-tool-name",
	})
	if err != nil {
		t.Fatal(err)
	}
	named, err := json.Marshal(map[string]any{
		"type": "some_future_tool_event", "toolName": "bash",
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.events = []pi.Record{
		{Type: "some_future_event", Raw: decoy},
		{Type: "some_future_tool_event", Raw: named},
		{Type: pi.TypeAgentSettled},
	}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	payloads, _ := sseFrames(t, rec.Body.String())

	var tools []string
	for _, p := range payloads {
		var frame toolFrame
		if err := json.Unmarshal([]byte(p), &frame); err != nil {
			continue
		}
		if frame.Tool != "" {
			tools = append(tools, frame.Tool+"/"+frame.Status)
		}
	}
	// Only the event that actually named a tool becomes a frame, as running.
	if len(tools) != 1 || tools[0] != "bash/running" {
		t.Fatalf("tool frames = %v, want exactly [bash/running]", tools)
	}
}

// A buffered response carries the same derived completion id as a streamed one, so
// a turn can be correlated from logs whichever mode served it.
func TestBufferedCompletionIDMatchesStreamed(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{textDelta(t, "x"), {Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	const conversation = "conv-buffered-id"
	send := func(stream bool) string {
		body := `{"model_options":{"reasoning_effort":"low"},` +
			`"messages":[{"role":"user","content":"hi"}],"stream":` + map[bool]string{true: "true", false: "false"}[stream] + `}`
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("X-Hermes-Session-Id", conversation)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Body.String()
	}

	want := completionIDFor(conversation)
	if !strings.Contains(send(true), want) {
		t.Fatal("streamed response did not carry the derived completion id")
	}
	if !strings.Contains(send(false), want) {
		t.Fatal("buffered response carried a different completion id")
	}
}

// A HEAD probe must not carry a body.
func TestHealthHeadHasNoBody(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{Password: "secret"})

	req := httptest.NewRequest(http.MethodHead, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("HEAD /healthz: got %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD response carried a %d-byte body: %q", rec.Body.Len(), rec.Body)
	}

	// GET still returns the body.
	req = httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("GET /healthz: got %q", rec.Body)
	}
}

// The store is bounded. A gateway is long-lived, and every fresh
// X-Hermes-Session-Id would otherwise be a permanent entry — unbounded growth
// driven entirely by client-supplied headers.
func TestSessionStoreIsBounded(t *testing.T) {
	store := NewSessionStore()
	cap := store.Cap()

	// Two live entries, then churn well past the cap.
	for i := 0; i < cap+50; i++ {
		if err := store.Remember(fmt.Sprintf("conv-%d", i), fmt.Sprintf("/s/%d.jsonl", i)); err != nil {
			t.Fatalf("remember %d: %v", i, err)
		}
	}

	if got := store.Len(); got > cap {
		t.Fatalf("store holds %d conversations, cap is %d", got, cap)
	}

	// Eviction must release the claim too, or the evicted session file would stay
	// permanently un-adoptable by another conversation.
	var someFile string
	for i := 0; i < cap+50; i++ {
		if _, ok := store.Lookup(fmt.Sprintf("conv-%d", i)); !ok {
			someFile = fmt.Sprintf("/s/%d.jsonl", i)
			break
		}
	}
	if someFile == "" {
		t.Skip("nothing was evicted")
	}
	if err := store.Remember("fresh", someFile); err != nil {
		t.Fatalf("an evicted session file should be adoptable: %v", err)
	}
}

// A recently used conversation must survive eviction; that is the point of LRU
// rather than dropping the oldest entry blindly.
func TestSessionStoreEvictsLeastRecentlyUsed(t *testing.T) {
	store := NewSessionStore()

	// "dropme" is claimed first and never touched again, so it is the least
	// recently used. "keep" is claimed after it and then touched, moving it to the
	// most-recently-used end.
	if err := store.Remember("dropme", "/s/dropme.jsonl"); err != nil {
		t.Fatal(err)
	}
	if err := store.Remember("keep", "/s/keep.jsonl"); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Lookup("keep"); !ok {
		t.Fatal("keep should be present")
	}

	// Add exactly enough to push the store one over the cap, so precisely one
	// entry is evicted — and it must be "dropme".
	for i := 0; i < store.Cap()-1; i++ {
		if err := store.Remember(fmt.Sprintf("filler-%d", i), fmt.Sprintf("/s/f%d.jsonl", i)); err != nil {
			t.Fatalf("filler %d: %v", i, err)
		}
	}

	if _, ok := store.Lookup("dropme"); ok {
		t.Fatal("the least recently used conversation should have been evicted")
	}
	if _, ok := store.Lookup("keep"); !ok {
		t.Fatal("the recently used conversation was evicted")
	}
}

// Forget must also drop the id from the LRU order, or a forgotten id would be
// re-touched on a later claim and skew eviction.
func TestForgetRemovesFromLRUOrder(t *testing.T) {
	store := NewSessionStore()
	if err := store.Remember("gone", "/s/gone.jsonl"); err != nil {
		t.Fatal(err)
	}
	store.Forget("gone")
	if store.Len() != 0 {
		t.Fatalf("Len = %d after Forget", store.Len())
	}
	// Re-claiming the same id must behave as a fresh conversation.
	if err := store.Remember("gone", "/s/other.jsonl"); err != nil {
		t.Fatalf("re-claiming a forgotten id: %v", err)
	}
	if _, ok := store.Lookup("gone"); !ok {
		t.Fatal("re-claimed id is missing")
	}
}

// Usage is decoded from every record, not only message_update. Pi documents usage
// there, but reading it wherever it appears at the top level means a record the
// switch does not handle cannot silently swallow the final counts.
func TestUsageIsHarvestedFromAnyTopLevelRecord(t *testing.T) {
	agent := newFakeAgent()
	// A non-message_update event that still carries top-level usage.
	novel, err := json.Marshal(map[string]any{
		"type":  "some_future_usage_event",
		"usage": map[string]any{"input": 55, "output": 6, "totalTokens": 61},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.events = []pi.Record{{Type: "some_future_usage_event", Raw: novel}, {Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	payloads, _ := sseFrames(t, rec.Body.String())

	var got *usage
	for _, p := range payloads {
		var frame struct {
			Usage *usage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(p), &frame); err != nil {
			continue
		}
		if frame.Usage.nonZero() {
			got = frame.Usage
		}
	}
	if got == nil {
		t.Fatalf("top-level usage on an unhandled record was dropped; frames=%v", payloads)
	}
	if got.PromptTokens != 55 || got.CompletionTokens != 6 {
		t.Fatalf("usage mapped wrongly: %+v", got)
	}
}

// Usage nested under another event is NOT the turn's usage. compaction_end's
// result.usage counts the compaction, and reading it as the turn's totals would
// replace real counts with unrelated ones — so only the top level is read.
func TestNestedUsageDoesNotOverrideTurnTotals(t *testing.T) {
	agent := newFakeAgent()
	compaction, err := json.Marshal(map[string]any{
		"type":   "compaction_end",
		"reason": "threshold",
		"result": map[string]any{
			"usage": map[string]any{"input": 999999, "output": 888888, "totalTokens": 1888887},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent.events = []pi.Record{
		textDelta(t, "hello"), // carries the real turn usage
		{Type: "compaction_end", Raw: compaction},
		{Type: pi.TypeAgentSettled},
	}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	payloads, _ := sseFrames(t, rec.Body.String())

	var got *usage
	for _, p := range payloads {
		var frame struct {
			Usage *usage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(p), &frame); err != nil {
			continue
		}
		if frame.Usage.nonZero() {
			got = frame.Usage
		}
	}
	if got == nil {
		t.Fatalf("the turn's usage was dropped entirely; frames=%v", payloads)
	}
	if got.PromptTokens != 10 || got.CompletionTokens != 2 {
		t.Fatalf("nested compaction usage overwrote the turn totals: %+v", got)
	}
}

// The session id becomes a map key. The entry count is LRU-capped, so the length
// must be bounded or a client can drive memory growth per request regardless.
func TestOversizedSessionIDRejected(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	send := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("X-Hermes-Session-Id", id)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	if rec := send(strings.Repeat("x", maxConversationIDLen+1)); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized session id: got %d, want 400. body=%s", rec.Code, rec.Body)
	}
	if agent.sawCommand("prompt") {
		t.Fatal("a rejected oversized session id still reached the agent")
	}

	// The boundary itself is accepted.
	if rec := send(strings.Repeat("x", maxConversationIDLen)); rec.Code != http.StatusOK {
		t.Fatalf("session id at the limit: got %d, want 200. body=%s", rec.Code, rec.Body)
	}
}

// "/healthz/" is a common way to write a liveness path; a probe should not fail
// on punctuation.
func TestHealthTrailingSlash(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{Password: "secret"})
	for _, path := range []string{"/healthz", "/healthz/"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200", path, rec.Code)
		}
	}
}

// The untracked path deliberately has no "no session file" guard, because it
// stores nothing: the caller discards the returned file entirely. An agent that
// reports no current session must not turn an otherwise working untracked turn
// into a 502 over a value nobody reads.
func TestUntrackedTurnWorksWithNoCurrentSession(t *testing.T) {
	agent := newFakeAgent()
	// An agent that reports no session file at all.
	agent.responses["get_state"] = map[string]any{
		"sessionId":   "",
		"sessionFile": "",
	}
	// A delta as well as the settle, so the stream carries a frame to assert on
	// rather than terminating immediately.
	agent.events = []pi.Record{textDelta(t, "ok"), {Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("untracked turn with no session: got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	payloads, sawDone := sseFrames(t, rec.Body.String())
	if !sawDone {
		t.Fatal("stream did not complete")
	}
	if len(payloads) == 0 {
		t.Fatal("no frames emitted")
	}

	// And nothing was tracked, because there was nothing to track.
	if got := srv.handlers["default"].sessions.Len(); got != 0 {
		t.Fatalf("an untracked turn claimed %d sessions", got)
	}
}

// The tracked path DOES require a session file, because Remember has to store
// one. Without the guard the failure would surface as a confusing store error.
func TestTrackedTurnRequiresASessionFile(t *testing.T) {
	agent := newFakeAgent()
	agent.responses["get_state"] = map[string]any{
		"sessionId":   "",
		"sessionFile": "",
	}
	srv := newTestServer(t, agent, Config{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("X-Hermes-Session-Id", "conv-needs-a-session")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("tracked turn with no session file: got %d, want 502. body=%s", rec.Code, rec.Body)
	}
	if agent.sawCommand("prompt") {
		t.Fatal("a turn with no session still reached the agent")
	}
}

// healthz rejects a wrong verb with Allow, like the chat endpoint does.
func TestHealthMethodNotAllowedCarriesAllow(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{Password: "secret"})
	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /healthz: got %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "GET") {
		t.Fatalf("405 should carry Allow, got %q", allow)
	}
}

// A malformed session id must be rejected on its own merits, not answered 429
// because the profile happens to be busy. The length check used to sit inside the
// profile lock, so an oversized header occupied the profile to produce an error
// that has nothing to do with contention.
func TestOversizedSessionIDIsNotMaskedByContention(t *testing.T) {
	agent := newFakeAgent()
	release := make(chan struct{})
	agent.onPrompt = func() { <-release }
	srv := newTestServer(t, agent, Config{})

	// Occupy the profile with a real turn.
	busy := make(chan *httptest.ResponseRecorder, 1)
	go func() { busy <- post(t, srv, "/v1/chat/completions", "test-token", validBody) }()
	waitFor(t, func() bool { return agent.sawCommand("prompt") }, "first turn to start")

	// An oversized id must still be 400, not 429.
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("X-Hermes-Session-Id", strings.Repeat("x", maxConversationIDLen+1))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized session id while busy: got %d, want 400. body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "256") {
		t.Fatalf("400 should state the limit, got %s", rec.Body)
	}

	close(release)
	<-busy
}

// A malformed session id must not take the profile lock at all, so it cannot make
// a well-formed concurrent request answer 429.
func TestOversizedSessionIDDoesNotHoldTheProfile(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	send := func(id string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("X-Hermes-Session-Id", id)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec.Code
	}

	// Interleave rejections with a valid turn; every valid turn must be served.
	for i := 0; i < 5; i++ {
		if got := send(strings.Repeat("x", maxConversationIDLen+1)); got != http.StatusBadRequest {
			t.Fatalf("iteration %d: oversized id got %d, want 400", i, got)
		}
		if got := send("valid-conv"); got != http.StatusOK {
			t.Fatalf("iteration %d: valid turn got %d, want 200 — the lock leaked", i, got)
		}
	}
}

// A blank TRAILING user message is an error, not a cue to fall back to an
// earlier one. Scanning back and sending "hello" when the user just sent
// whitespace would answer a question they did not ask, with no indication
// anything was wrong — worse than a clear 400. Blanks that are NOT trailing are
// skipped as normal, since the scan walks backwards to the latest real turn.
func TestBlankTrailingMessageIsRejectedNotSkipped(t *testing.T) {
	if got, err := userPrompt([]chatMessage{
		{Role: "user", Content: chatContent{Text: "hello"}},
		{Role: "user", Content: chatContent{Text: "   "}},
	}); err == nil {
		t.Fatalf("a blank trailing message must be an error, got %q", got)
	}

	// A blank message followed by a real one is fine: the real one is the turn.
	if got, err := userPrompt([]chatMessage{
		{Role: "user", Content: chatContent{Text: "  "}},
		{Role: "user", Content: chatContent{Text: "real"}},
	}); err != nil || got != "real" {
		t.Fatalf("got %q err=%v, want %q", got, err, "real")
	}
}

// /healthz must match exactly. The "/healthz/" pattern covers the subtree, so
// without this check any path under it answered 200 and an orchestrator could
// read a typo as a healthy agent.
func TestHealthOnlyMatchesExactPaths(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{Password: "secret"})

	for _, p := range []string{"/healthz", "/healthz/"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200", p, rec.Code)
		}
	}

	// Authenticated, so this exercises routing rather than the auth gate: an
	// unauthenticated request to any path is 401 first, by design.
	// The token must match this server's password ("secret"), or the auth gate
	// answers 401 first and the routing check never runs.
	for _, p := range []string{"/healthz/anything", "/healthz/a/b/c", "/healthzx"} {
		req := httptest.NewRequest(http.MethodGet, p, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: got %d, want 404. body=%s", p, rec.Code, rec.Body)
		}
	}
}

// Client-controlled text reflected into an error body is truncated, so a long path
// segment cannot produce a huge response.
func TestErrorBodiesAreTruncated(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{Password: "tok"})

	huge := strings.Repeat("p", 10_000)
	req := httptest.NewRequest(http.MethodGet, "/"+huge, nil)
	req.Header.Set("Authorization", "Bearer tok")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404", rec.Code)
	}
	if rec.Body.Len() > 512 {
		t.Fatalf("error body is %d bytes for a 10 KB path; it must be truncated", rec.Body.Len())
	}
	if !strings.Contains(rec.Body.String(), "truncated") {
		t.Fatalf("truncated error should say so, got %s", rec.Body)
	}
}

// A level Pi advertises is accepted even if it is not one of the known
// spellings: a new Pi level should work the day it ships.
func TestAdvertisedLevelIsAcceptedWithoutAnAlias(t *testing.T) {
	supported := levelSet{"off": true, "low": true, "ultra": true}
	if got, err := resolveEffort("ultra", supported); err != nil || got != "ultra" {
		t.Fatalf("advertised level: got %q err=%v, want ultra", got, err)
	}
	// Case is still normalised.
	if got, err := resolveEffort("ULTRA", supported); err != nil || got != "ultra" {
		t.Fatalf("case-insensitive advertised level: got %q err=%v", got, err)
	}
	// Unadvertised and unknown is still an error.
	if _, err := resolveEffort("turbo", supported); err == nil {
		t.Fatal("an unknown, unadvertised level must be rejected")
	}
	// A known spelling the model does not advertise is "unsupported", not unknown.
	if _, err := resolveEffort("max", supported); !errors.Is(err, errEffortUnsupported) {
		t.Fatalf("want errEffortUnsupported, got %v", err)
	}
}

// The hint must list every level the model advertises, including ones outside
// the canonical ladder. resolveEffort accepts those, so a hint that omitted them
// would call a level unavailable when it is the one being asked about.
func TestSupportedListIncludesForwardCompatLevels(t *testing.T) {
	got := supportedList(levelSet{"off": true, "high": true, "ultra": true, "turbo": true})
	for _, want := range []string{"off", "high", "ultra", "turbo"} {
		if !strings.Contains(got, want) {
			t.Fatalf("supported list %q is missing %q", got, want)
		}
	}
	// Canonical order first, extras sorted after.
	if !strings.HasPrefix(got, "off, high") {
		t.Fatalf("canonical order not preserved: %q", got)
	}
	if got[len("off, high, "):] != "turbo, ultra" {
		t.Fatalf("extras not sorted after the canonical ones: %q", got)
	}
}

// OpenAI permits content as an array of typed parts; only decoding a bare string
// made the standard shape fail as an unparseable body.
func TestMultipartContentAccepted(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{textDelta(t, "ok"), {Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	body := `{"model_options":{"reasoning_effort":"low"},"messages":[{"role":"user",` +
		`"content":[{"type":"text","text":"what is this?"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,iVBORw0KGgo="}}]}],"stream":true}`

	rec := post(t, srv, "/v1/chat/completions", "test-token", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("multipart content: got %d, want 200. body=%s", rec.Code, rec.Body)
	}
}

// The bare string form must still work — it is what the app sends.
func TestStringContentStillAccepted(t *testing.T) {
	var c chatContent
	if err := json.Unmarshal([]byte(`"hello"`), &c); err != nil {
		t.Fatalf("string form: %v", err)
	}
	if c.Text != "hello" {
		t.Fatalf("string form decoded to %q", c.Text)
	}
}

// A remote image URL would mean the gateway fetching an arbitrary URL on the
// app's behalf. Refused explicitly rather than quietly not fetched.
func TestRemoteImageURLRejected(t *testing.T) {
	var c chatContent
	err := json.Unmarshal([]byte(
		`[{"type":"image_url","image_url":{"url":"https://example.com/x.png"}}]`), &c)
	if err == nil {
		t.Fatal("a remote image URL must be refused, not fetched")
	}
	if !strings.Contains(err.Error(), "remote image URLs") {
		t.Fatalf("error should explain the refusal, got %v", err)
	}
}

// A content part with no type is malformed; dropping it would silently discard
// content the caller believed was sent.
func TestUntypedContentPartRejected(t *testing.T) {
	var c chatContent
	if err := json.Unmarshal([]byte(`[{"text":"orphan"}]`), &c); err == nil {
		t.Fatal("an untyped content part must be an error")
	}
}

// Images reach Pi as its own ImageContent values, not as text.
func TestImagesReachPiPrompt(t *testing.T) {
	userTurn := &turn{
		Text: "what is this?",
		Images: []piImage{
			{Data: "iVBORw0KGgo=", MimeType: "image/png"},
			{Data: "Zm9v", MimeType: "image/jpeg"},
		},
	}
	payload := promptPayload(userTurn)

	images, ok := payload["images"].([]map[string]any)
	if !ok {
		t.Fatalf("images not attached: %#v", payload)
	}
	if len(images) != 2 {
		t.Fatalf("got %d images, want 2", len(images))
	}
	first := images[0]
	if first["type"] != "image" || first["mimeType"] != "image/png" || first["data"] != "iVBORw0KGgo=" {
		t.Fatalf("image not in Pi's shape: %#v", first)
	}
	if payload["message"] != "what is this?" {
		t.Fatalf("text lost: %#v", payload["message"])
	}
}

// No images means no images key at all, rather than an empty array Pi would have
// to interpret.
func TestNoImagesOmittedFromPrompt(t *testing.T) {
	payload := promptPayload(&turn{Text: "hi"})
	if _, present := payload["images"]; present {
		t.Fatalf("images key present with no images: %#v", payload)
	}
}

// System messages are dropped — personality is per-profile — but counted, so a
// future client sending them gets a log line instead of silently ineffective
// instructions.
func TestSystemMessagesAreCountedNotSilentlyDropped(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: chatContent{Text: "you are terse"}},
		{Role: "user", Content: chatContent{Text: "hi"}},
	}
	got, err := lastUserTurn(msgs)
	if err != nil {
		t.Fatalf("lastUserTurn: %v", err)
	}
	if got.SystemMessages != 1 {
		t.Fatalf("SystemMessages = %d, want 1", got.SystemMessages)
	}
	if got.Text != "hi" {
		t.Fatalf("text = %q", got.Text)
	}
}

// userPrompt is the text-only view of lastUserTurn, kept here because that is the
// only place that wants it: production needs the images and the system-message
// count too.
func userPrompt(msgs []chatMessage) (string, error) {
	got, err := lastUserTurn(msgs)
	if err != nil {
		return "", err
	}
	return got.Text, nil
}

// A truncated echo must not cut a multi-byte character in half. Byte slicing left
// a replacement character in the output — the same bug tail() had, repeated in the
// other trimming helper. A path segment can carry multi-byte text, so the case is
// real rather than theoretical.
func TestBoundedSlicesByRuneNotByte(t *testing.T) {
	// "é" is two bytes, so a 128-byte cut lands mid-character.
	long := strings.Repeat("é", 200)

	got := bounded(long)
	if !strings.HasSuffix(got, "… (truncated)") {
		t.Fatalf("bounded did not mark the truncation: %q", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Fatalf("byte slicing split a multi-byte character: %q", got)
	}
	// The visible part is 128 runes, not 128 bytes.
	body := strings.TrimSuffix(got, "… (truncated)")
	if n := len([]rune(body)); n != maxEchoedLen {
		t.Fatalf("truncated to %d runes, want %d", n, maxEchoedLen)
	}

	// Short input is returned unchanged.
	if got := bounded("short"); got != "short" {
		t.Fatalf("short input changed: %q", got)
	}
	// A string at exactly the limit is not marked truncated.
	exact := strings.Repeat("x", maxEchoedLen)
	if got := bounded(exact); got != exact {
		t.Fatalf("input at the limit was altered: %q", got)
	}
}

// Frames must report the model that actually ANSWERED, not the client's claim.
// The gateway never selects on `model` — Pi's model is fixed when its process
// starts — so echoing the claim reports a model that never ran. It was also
// unbounded: the value arrives in a body capped at 4 MiB and was repeated on
// every streamed delta.
func TestFramesReportTheModelThatAnswered(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{
		textDelta(t, "a"), textDelta(t, "b"), textDelta(t, "c"),
		{Type: pi.TypeAgentSettled},
	}
	srv := newTestServer(t, agent, Config{})

	// A wildly wrong and oversized client claim.
	claim := strings.Repeat("not-the-real-model", 200)
	body := fmt.Sprintf(
		`{"model":%q,"model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}],"stream":true}`, claim)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	payloads, _ := sseFrames(t, rec.Body.String())
	if len(payloads) == 0 {
		t.Fatal("no frames emitted")
	}
	if strings.Contains(rec.Body.String(), "not-the-real-model") {
		t.Fatal("the client's model claim was echoed into the stream")
	}
	for _, p := range payloads {
		var frame chunk
		if err := json.Unmarshal([]byte(p), &frame); err != nil {
			continue
		}
		if frame.Model != "" && frame.Model != "mimo-v2.6-flash" {
			t.Fatalf("frame reported model %q, want the agent's own", frame.Model)
		}
	}
}

// The 400 must distinguish "no user turn" from "your latest turn was empty".
// They look identical to the handler but need different client fixes, and the
// review agent was right that the single message left the second unfixable.
func TestPromptErrorsDistinguishTheTwoCases(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{})

	cases := []struct {
		name     string
		messages string
		wantHas  string
	}{
		{
			name:     "no user turn at all",
			messages: `[{"role":"assistant","content":"hi"}]`,
			wantHas:  "must contain a user message",
		},
		{
			name:     "latest user turn is blank",
			messages: `[{"role":"user","content":"hello"},{"role":"user","content":"   "}]`,
			wantHas:  "most recent user message is empty",
		},
	}
	for _, tc := range cases {
		body := fmt.Sprintf(
			`{"model_options":{"reasoning_effort":"low"},"messages":%s,"stream":true}`,
			tc.messages)
		rec := post(t, srv, "/v1/chat/completions", "test-token", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400. body=%s", tc.name, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), tc.wantHas) {
			t.Fatalf("%s: error should say %q, got %s", tc.name, tc.wantHas, rec.Body)
		}
	}
}

// A data URI may carry parameters before ";base64". Taking everything up to the
// marker yields a MIME type of "image/png;charset=utf-8", which Pi rejects as an
// unknown format — a broken image rather than a clear error.
func TestDataURIParametersDoNotLeakIntoMimeType(t *testing.T) {
	for _, tc := range []struct{ uri, want string }{
		{"data:image/png;base64,AAAA", "image/png"},
		{"data:image/png;charset=utf-8;base64,AAAA", "image/png"},
		{"data:image/jpeg;quality=90;base64,AAAA", "image/jpeg"},
	} {
		got, err := parseDataURI(tc.uri)
		if err != nil {
			t.Fatalf("%s: %v", tc.uri, err)
		}
		if got.MimeType != tc.want {
			t.Fatalf("%s: mimeType = %q, want %q", tc.uri, got.MimeType, tc.want)
		}
		if strings.Contains(got.MimeType, ";") {
			t.Fatalf("%s: parameters leaked into the MIME type %q", tc.uri, got.MimeType)
		}
	}
}

// healthz is reachable WITHOUT authentication, so its 404 echo must be bounded
// like every other reflected path — otherwise an anonymous caller can make the
// server emit an arbitrarily large response.
func TestHealthz404EchoIsBounded(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{Password: "secret"})

	// Unauthenticated on purpose: that is the exposure being bounded.
	req := httptest.NewRequest(http.MethodGet, "/healthz/"+strings.Repeat("p", 20_000), nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d, want 404. body=%s", rec.Code, rec.Body)
	}
	if rec.Body.Len() > 512 {
		t.Fatalf("unauthenticated healthz echo is %d bytes; it must be truncated", rec.Body.Len())
	}
}

// A resume that had to switch sessions must return the state of the session it
// switched TO. Real Pi lands on the requested file but assigns a fresh sessionId
// for it (measured against live Pi: e07f... -> e2cb... on an unchanged path), so
// returning the struct read before the switch hands the caller the id of the
// session Pi just left.
func TestResolveSessionReReadsAfterSwitch(t *testing.T) {
	agent := newFakeAgent()
	// Pi is currently sitting on some other conversation's session.
	agent.mu.Lock()
	agent.sessionSeq = 9
	agent.curFile = "/tmp/sessions/sess-9.jsonl"
	agent.curID = "sess-9"
	agent.mu.Unlock()

	srv := newTestServer(t, agent, Config{})
	h := srv.handlers["default"]

	if err := h.runner.sessions.Remember("conv-a", "/tmp/sessions/sess-1.jsonl"); err != nil {
		t.Fatalf("Remember: %v", err)
	}

	st, err := h.runner.resolveSession(context.Background(), "conv-a")
	if err != nil {
		t.Fatalf("resolveSession: %v", err)
	}

	if !agent.sawCommand("switch_session") {
		t.Fatal("resume did not switch sessions, so this test is not exercising the path")
	}
	if st.SessionFile != "/tmp/sessions/sess-1.jsonl" {
		t.Fatalf("SessionFile = %q, want the requested session", st.SessionFile)
	}
	agent.mu.Lock()
	freshID := agent.curID
	agent.mu.Unlock()
	if st.SessionID == "sess-9" {
		t.Fatalf("returned the PRE-switch sessionId %q for the session just resumed", st.SessionID)
	}
	if st.SessionID != freshID {
		t.Fatalf("SessionID = %q, want the post-switch id %q", st.SessionID, freshID)
	}
}

// A bare single segment keeps its name, so a 404 says WHICH namespace was wrong
// instead of reporting the default profile's view of someone else's path.
func TestBareProfileSegmentNamesTheProfile(t *testing.T) {
	agent := newFakeAgent()
	srv, err := New(Config{Password: "test-token"}, map[string]Agent{
		"default": agent,
		"story":   agent,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, tc := range []struct{ path, want string }{
		// "story" IS registered, so this is a profile with no endpoint under it —
		// a distinct problem from the name being wrong.
		{"/story", "profile story has no endpoint"},
		{"/story/", "profile story has no endpoint"},
		// Not registered at all.
		{"/nosuch", "unknown profile nosuch"},
		// The API root is not a profile typo, it is a missing endpoint.
		{"/v1", "unknown endpoint /v1"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)

		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: got %d, want 404. body=%s", tc.path, rec.Code, rec.Body)
		}
		if !strings.Contains(rec.Body.String(), tc.want) {
			t.Fatalf("%s: body should contain %q, got %s", tc.path, tc.want, rec.Body)
		}
	}
}

// When Pi reports no model, the frame's model must not go out blank AND must not
// go out as the raw client claim. Blank is a wire shape no client can act on; the
// raw claim arrives in a 4 MiB-capped body and is repeated on every streamed
// delta. The fallback is the bounded claim.
func TestMissingAgentModelFallsBackToBoundedClaim(t *testing.T) {
	agent := newFakeAgent()
	// A fake that reports no model at all — a field Pi may not send.
	agent.modelID = ""
	agent.events = []pi.Record{textDelta(t, "a"), {Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	claim := strings.Repeat("claimed-model", 5000)
	body := fmt.Sprintf(
		`{"model":%q,"model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}],"stream":true}`, claim)
	rec := post(t, srv, "/v1/chat/completions", "test-token", body)

	payloads, _ := sseFrames(t, rec.Body.String())
	if len(payloads) == 0 {
		t.Fatal("no frames emitted")
	}
	// The full claim must not appear. (Checking for a short repeat would be wrong:
	// the bounded value legitimately begins with many copies of it.)
	if strings.Contains(rec.Body.String(), claim) {
		t.Fatal("the full unbounded client claim was echoed into the stream")
	}
	if rec.Body.Len() > 64*1024 {
		t.Fatalf("stream is %d bytes for a 65 KB claim; it was not bounded", rec.Body.Len())
	}
	// Every frame carrying assistant content must still name a model: bounded,
	// non-empty, and recognisable as the claim it came from.
	//
	// Checked on frames WITH choices rather than on the presence of the JSON key.
	// Model is omitempty, so a blank model omits the field entirely and a
	// "does the key exist" test would pass on exactly the broken case.
	sawText := false
	for _, p := range payloads {
		var frame chunk
		if err := json.Unmarshal([]byte(p), &frame); err != nil {
			continue
		}
		if len(frame.Choices) == 0 {
			continue
		}
		sawText = true
		if frame.Model == "" {
			t.Fatal("frame carrying content had no model at all")
		}
		if len([]rune(frame.Model)) > maxEchoedLen+32 {
			t.Fatalf("frame model is %d runes; it must be bounded",
				len([]rune(frame.Model)))
		}
		if !strings.HasPrefix(frame.Model, "claimed-model") {
			t.Fatalf("frame model %q is not the (bounded) client claim", frame.Model)
		}
	}
	if !sawText {
		t.Fatal("no frame carried assistant content, so nothing was checked")
	}
}

// The agent-reported model is echoed on every frame, so it is bounded like the
// client claim was. "It comes from a local process we started" is not a length
// guarantee, and the amplification is per-delta either way.
func TestAgentReportedModelIsAlsoBounded(t *testing.T) {
	agent := newFakeAgent()
	agent.modelID = strings.Repeat("m", 20_000)
	agent.events = []pi.Record{textDelta(t, "a"), {Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)

	payloads, _ := sseFrames(t, rec.Body.String())
	sawContent := false
	for _, p := range payloads {
		var frame chunk
		if err := json.Unmarshal([]byte(p), &frame); err != nil {
			continue
		}
		if len(frame.Choices) == 0 {
			continue
		}
		sawContent = true
		if n := len([]rune(frame.Model)); n > maxEchoedLen+32 {
			t.Fatalf("frame model is %d runes for a 20k id; it must be bounded", n)
		}
	}
	if !sawContent {
		t.Fatal("no frame carried assistant content, so nothing was checked")
	}
}

// Text parts are joined with a newline, not run together. With an image between
// them, concatenation makes the text either side read as one run and hides where
// the image was.
func TestMultipartTextPartsJoinWithNewline(t *testing.T) {
	// A 1x1 transparent PNG.
	const png = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
	raw := `[{"type":"text","text":"describe this:"},` +
		`{"type":"image_url","image_url":{"url":"` + png + `"}},` +
		`{"type":"text","text":"be brief"}]`

	var c chatContent
	if err := c.UnmarshalJSON([]byte(raw)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if got, want := c.Text, "describe this:\nbe brief"; got != want {
		t.Fatalf("Text = %q, want %q", got, want)
	}
	if len(c.Images) != 1 {
		t.Fatalf("got %d images, want 1", len(c.Images))
	}
}

// Single-part multipart is unaffected by the join.
func TestSingleTextPartIsUnchanged(t *testing.T) {
	var c chatContent
	if err := c.UnmarshalJSON([]byte(`[{"type":"text","text":"hello"}]`)); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}
	if c.Text != "hello" {
		t.Fatalf("Text = %q, want %q", c.Text, "hello")
	}
}

// KeepaliveDisabled must survive withDefaults. It used to test <= 0 and so
// overwrote the one value that means "no keepalive" with the 20s default,
// leaving the documented PI_KEEPALIVE=0 unable to do anything.
func TestKeepaliveDisabledSurvivesDefaults(t *testing.T) {
	t.Run("disabled stays disabled", func(t *testing.T) {
		cfg := Config{KeepaliveInterval: KeepaliveDisabled}
		cfg.withDefaults()
		if cfg.KeepaliveInterval != KeepaliveDisabled {
			t.Fatalf("KeepaliveInterval = %v, want KeepaliveDisabled", cfg.KeepaliveInterval)
		}
	})
	t.Run("unset still gets the default", func(t *testing.T) {
		cfg := Config{}
		cfg.withDefaults()
		if cfg.KeepaliveInterval != 20*time.Second {
			t.Fatalf("KeepaliveInterval = %v, want 20s", cfg.KeepaliveInterval)
		}
	})
	t.Run("startKeepalive emits nothing when disabled", func(t *testing.T) {
		// Each case stops its keepalive goroutine and WAITS for it before reading
		// the recorder. Deferring the stop would leave the goroutine writing while
		// the assertion reads — which the race detector correctly reports, and which
		// is the same unsynchronised access the mutex in sseWriter protects in
		// production, just not in a test.
		rec := httptest.NewRecorder()
		sse := &sseWriter{w: rec, fc: http.NewResponseController(rec)}
		stop := startKeepalive(context.Background(), sse, KeepaliveDisabled)
		time.Sleep(50 * time.Millisecond)
		stop()
		if rec.Body.Len() != 0 {
			t.Fatalf("wrote %d byte(s) of keepalive while disabled: %q",
				rec.Body.Len(), rec.Body.String())
		}

		// Control: the same harness with a live interval does emit, so a passing
		// test above is not just a harness that never writes.
		rec2 := httptest.NewRecorder()
		sse2 := &sseWriter{w: rec2, fc: http.NewResponseController(rec2)}
		stop2 := startKeepalive(context.Background(), sse2, 10*time.Millisecond)
		time.Sleep(80 * time.Millisecond)
		stop2()
		if rec2.Body.Len() == 0 {
			t.Fatal("control: a live keepalive interval wrote nothing, so the harness proves nothing")
		}
	})
}

// A turn's text must reach the agent exactly as the user typed it. Trimming
// decided blankness and was then applied to the text, which strips the
// indentation of pasted code — and an indented Python or YAML block is
// meaningless without it.
func TestPromptWhitespaceIsPreserved(t *testing.T) {
	const code = "def f():\n    return 1\n"
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	body := fmt.Sprintf(
		`{"model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":%s}],"stream":true}`,
		mustJSON(t, code))
	rec := post(t, srv, "/v1/chat/completions", "test-token", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}

	agent.mu.Lock()
	prompts := append([]string(nil), agent.prompts...)
	agent.mu.Unlock()

	if len(prompts) != 1 {
		t.Fatalf("got %d prompts, want 1", len(prompts))
	}
	sent := prompts[0]
	if sent != code {
		t.Fatalf("prompt sent to the agent is %q, want %q", sent, code)
	}
}

// mustJSON is a local JSON-string encoder for building request bodies.
func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal %q: %v", s, err)
	}
	return string(b)
}

// A PASSWORD carrying a trailing newline breaks every request with an opaque 401.
// The presented token is trimmed before comparison, so the configured value has to
// be trimmed too — and a stray newline is exactly what editing .env by hand
// introduces, and it is invisible in most viewers.
func TestConfiguredPasswordIsTrimmed(t *testing.T) {
	agent := newFakeAgent()
	srv, err := New(Config{Password: "  s3cret\n"}, map[string]Agent{"default": agent})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := srv.cfg.Password; got != "s3cret" {
		t.Fatalf("stored password = %q, want the trimmed form", got)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer s3cret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code == http.StatusUnauthorized {
		t.Fatal("the trimmed password did not authenticate")
	}
	// A wrong token must still be refused, so trimming did not weaken the check.
	req2 := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer wrong")
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token got %d, want 401", rec2.Code)
	}
}

// ";BASE64" is as valid as ";base64"; RFC 2397 does not fix the case of the
// extension token. Upper-casing it used to produce a 400 for a legal URI.
func TestDataURIBase64MarkerIsCaseInsensitive(t *testing.T) {
	for _, uri := range []string{
		"data:image/png;base64,AAAA",
		"data:image/png;BASE64,AAAA",
		"data:image/png;Base64,AAAA",
	} {
		got, err := parseDataURI(uri)
		if err != nil {
			t.Fatalf("%s: %v", uri, err)
		}
		if got.MimeType != "image/png" || got.Data != "AAAA" {
			t.Fatalf("%s: got %+v", uri, got)
		}
	}
}

func TestTailKeepsTheEnd(t *testing.T) {
	// A crash explanation lives at the end of the output, so the tail is kept.
	got := Tail("0123456789abcdef", 6)
	if got != "…abcdef" && got != "...abcdef" {
		t.Fatalf("tail = %q, want the last 6 chars with a marker", got)
	}

	// Short input is returned unchanged, trimmed.
	if got := Tail("  hi  ", 10); got != "hi" {
		t.Fatalf("tail = %q, want %q", got, "hi")
	}
	if got := Tail("", 10); got != "" {
		t.Fatalf("tail of empty = %q", got)
	}
}

// Agent stderr is logged on every failed turn. Logged verbatim, one wedged agent
// could emit megabytes per request and grow the log without bound — the same
// bounded-echo rule every other client-influenced string in here already follows.
func TestAgentStderrIsTailedNotLoggedWhole(t *testing.T) {
	var logged strings.Builder
	agent := newFakeAgent()
	agent.stderr = strings.Repeat("wedged stack trace line\n", 20_000)
	// Force the thinking-levels read to fail, which is the path that logs stderr.
	agent.failCommands["get_available_thinking_levels"] = errors.New("no levels")

	srv := newTestServer(t, agent, Config{
		Logf: func(format string, args ...any) {
			logged.WriteString(fmt.Sprintf(format, args...))
			logged.WriteByte('\n')
		},
	})

	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	// 502, not 400: the request was well-formed and it was the agent that could
	// not be asked. Reading it as a client error would send someone to fix a
	// request that was fine.
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502. body=%s", rec.Code, rec.Body)
	}

	out := logged.String()
	if !strings.Contains(out, "thinking levels") {
		t.Fatalf("the failure was not logged at all: %q", out)
	}
	if n := len(out); n > 8*1024 {
		t.Fatalf("log line is %d bytes for a %d-byte stderr; it was not tailed",
			n, len(agent.stderr))
	}
	if !strings.HasPrefix(out[strings.Index(out, "stderr:"):], "stderr: ") {
		t.Fatal("no stderr marker in the log line")
	}
	// The tail keeps the END, which is where the actual error is.
	if !strings.Contains(out, "wedged stack trace line") {
		t.Fatal("the tail dropped the stderr content entirely")
	}
}

// The three inventory routes are what the app's provider picker and its Skills/MCP
// pages are built from. All four of the app's requests currently go to Hermes, so
// these are capability, not convenience — and each is checked against the shape
// ServerApi/ChatApi actually parses rather than the shape that seemed natural.

func TestInventoryModelOptionsMatchesTheAppShape(t *testing.T) {
	agent := newFakeAgent()
	agent.responses["get_available_models"] = map[string]any{"models": []any{
		// Two providers, deliberately out of order, plus a duplicate id and two
		// unusable rows, so grouping, sorting and filtering are all exercised.
		map[string]any{"id": "mimo-v2.6-flash", "name": "MiMo", "provider": "opencode-go", "reasoning": true},
		map[string]any{"id": "muse-spark-1.3-contributor", "name": "Muse", "provider": "opencode-go", "reasoning": true},
		map[string]any{"id": "claude-fable-5", "name": "Fable", "provider": "opencode", "reasoning": true},
		map[string]any{"id": "mimo-v2.6-flash", "name": "dupe", "provider": "opencode-go"},
		map[string]any{"id": "", "provider": "opencode"},
		map[string]any{"id": "no-provider", "provider": ""},
		map[string]any{"id": "x", "provider": "   "},
	}}
	srv := newTestServer(t, agent, Config{})

	rec := get(t, srv, "/api/model/options", "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	// Exactly what ChatApi.fetchCatalog parses.
	var parsed struct {
		Providers []struct {
			Slug   string   `json:"slug"`
			Name   string   `json:"name"`
			Models []string `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("app's parse failed: %v (%s)", err, rec.Body)
	}
	if len(parsed.Providers) != 2 {
		t.Fatalf("got %d providers, want 2: %+v", len(parsed.Providers), parsed.Providers)
	}
	// Sorted by slug, so the dropdown does not reshuffle between refreshes.
	if parsed.Providers[0].Slug != "opencode" || parsed.Providers[1].Slug != "opencode-go" {
		t.Fatalf("providers not sorted by slug: %+v", parsed.Providers)
	}
	// Sorted and de-duplicated within the provider.
	got := parsed.Providers[1].Models
	want := []string{"mimo-v2.6-flash", "muse-spark-1.3-contributor"}
	if len(got) != len(want) {
		t.Fatalf("models = %v, want %v (sorted, deduped)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("models = %v, want %v", got, want)
		}
	}
}

// Skills must come from what Pi reports it loaded, not from a filesystem scan. The
// skill:" prefix Pi adds for /skill:name is a command-namespace detail and would
// render as literal text in the app's list; extension commands are not skills.
func TestInventorySkillsFiltersAndStripsPrefix(t *testing.T) {
	agent := newFakeAgent()
	agent.responses["get_commands"] = map[string]any{"commands": []any{
		map[string]any{
			"name": "skill:note-taking", "description": "Capture notes.",
			"source": "skill",
			"sourceInfo": map[string]any{
				"path": "/opt/pi/skills/note-taking/SKILL.md", "scope": "user",
			},
		},
		map[string]any{
			"name": "skill:apple", "description": "Apple things.",
			"source": "skill",
			"sourceInfo": map[string]any{
				"path": "/opt/pi/skills/apple/SKILL.md", "scope": "user",
			},
		},
		// Not skills: an extension command and a prompt template.
		map[string]any{"name": "mcp", "description": "MCP servers.", "source": "extension"},
		map[string]any{"name": "fix-tests", "description": "Fix.", "source": "prompt"},
		// A skill whose command name is empty after stripping is dropped.
		map[string]any{"name": "skill:", "source": "skill"},
	}}
	srv := newTestServer(t, agent, Config{})

	rec := get(t, srv, "/v1/skills", "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	// The app tries a bare array first, so it must BE a bare array.
	var parsed []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Enabled     bool   `json:"enabled"`
		Path        string `json:"path"`
		Scope       string `json:"scope"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("app's parse failed: %v (%s)", err, rec.Body)
	}
	if len(parsed) != 2 {
		t.Fatalf("got %d skills, want 2 (extensions and prompts excluded): %+v", len(parsed), parsed)
	}
	// Sorted case-insensitively.
	if parsed[0].Name != "apple" || parsed[1].Name != "note-taking" {
		t.Fatalf("not sorted: %+v", parsed)
	}
	for _, s := range parsed {
		if strings.HasPrefix(s.Name, "skill:") {
			t.Fatalf("prefix not stripped: %q", s.Name)
		}
		if !s.Enabled {
			t.Fatalf("%q reported disabled; a loaded skill is available", s.Name)
		}
		if s.Path == "" || s.Scope == "" {
			t.Fatalf("%q missing provenance: %+v", s.Name, s)
		}
	}
}

// An empty inventory is a valid empty page, not an error: the app shows "Nothing
// listed" and only transport failures are errors.
func TestInventoryEmptyIsSuccessNotError(t *testing.T) {
	agent := newFakeAgent()
	agent.responses["get_commands"] = map[string]any{"commands": []any{}}
	agent.responses["get_available_models"] = map[string]any{"models": []any{}}
	srv := newTestServer(t, agent, Config{})

	for _, path := range []string{"/v1/skills", "/api/model/options"} {
		rec := get(t, srv, path, "test-token")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200. body=%s", path, rec.Code, rec.Body)
		}
	}
	// Skills must be an array, not null: rootArray would fail on a null body and the
	// app would report a parse error rather than an empty page.
	if body := get(t, srv, "/v1/skills", "test-token").Body.String(); body != "[]" {
		t.Fatalf("empty skills body = %q, want []", body)
	}
}

// An unreachable agent must not read as "this server offers nothing". The picker
// showing an empty list is indistinguishable from a real answer, so this is a 502.
func TestInventoryUpstreamFailureIsNotAnEmptyList(t *testing.T) {
	agent := newFakeAgent()
	agent.failCommands["get_available_models"] = errors.New("agent gone")
	agent.failCommands["get_commands"] = errors.New("agent gone")
	srv := newTestServer(t, agent, Config{})

	for _, path := range []string{"/api/model/options", "/v1/skills"} {
		rec := get(t, srv, path, "test-token")
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("%s: got %d, want 502. body=%s", path, rec.Code, rec.Body)
		}
	}
}

// "api" is a reserved first segment. Without that, /api/model/options reads "api" as
// a profile name and 404s as "unknown profile api".
func TestAPISegmentIsNotReadAsAProfile(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{})

	profile, rest := srv.splitProfile("/api/model/options")
	if profile != "default" {
		t.Fatalf("profile = %q, want the default", profile)
	}
	if rest != "api/model/options" {
		t.Fatalf("rest = %q, want api/model/options", rest)
	}

	// Both spellings the app uses must reach the handler.
	agent := newFakeAgent()
	agent.responses["get_available_models"] = map[string]any{"models": []any{}}
	srv2, err := New(Config{Password: "test-token"}, map[string]Agent{"default": agent, "story": agent})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, path := range []string{"/api/model/options", "/story/api/model/options"} {
		rec := get(t, srv2, path, "test-token")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200. body=%s", path, rec.Code, rec.Body)
		}
	}
}

// Inventory routes are read-only, so a POST is 405 with Allow rather than a 404 that
// would send a client looking for a different path.
func TestInventoryRoutesRejectTheWrongVerb(t *testing.T) {
	agent := newFakeAgent()
	agent.responses["get_commands"] = map[string]any{"commands": []any{}}
	agent.responses["get_available_models"] = map[string]any{"models": []any{}}
	srv := newTestServer(t, agent, Config{})

	for _, path := range []string{"/v1/skills", "/v1/toolsets", "/api/model/options"} {
		rec := post(t, srv, path, "test-token", "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: got %d, want 405. body=%s", path, rec.Code, rec.Body)
		}
		if allow := rec.Header().Get("Allow"); allow != http.MethodGet {
			t.Fatalf("%s: Allow = %q, want GET", path, allow)
		}
	}
}

// Every inventory route sits behind the bearer token, like chat. These pages list the
// agent's skills and MCP servers, which is not public information.
func TestInventoryRoutesRequireAuth(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{})

	for _, path := range []string{"/v1/skills", "/v1/toolsets", "/api/model/options"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: got %d, want 401", path, rec.Code)
		}
	}
}

// Toolsets come from the agent directory's mcp.json: the configured set, which the
// entrypoint has already proven connectable. Disabled servers and entries with
// neither a command nor a URL are not live toolsets and must not be listed.
func TestInventoryToolsetsReadsMCPConfig(t *testing.T) {
	dir := t.TempDir()
	body := `{"mcpServers":{
      "cookbook":{"command":"/usr/local/bin/cookbook"},
      "miser-money":{"command":"/usr/local/bin/miser-money"},
      "off-server":{"command":"/usr/local/bin/x","disabled":true},
      "broken":{"description":"no command or url"},
      "remote":{"url":"https://example.test/mcp"}
    }}`
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write mcp.json: %v", err)
	}
	agent := newFakeAgent()
	srv, err := New(Config{Password: "test-token", AgentDir: dir},
		map[string]Agent{"default": agent})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := get(t, srv, "/v1/toolsets", "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	var parsed []struct {
		Name       string `json:"name"`
		Enabled    bool   `json:"enabled"`
		Configured bool   `json:"configured"`
		Tools      []any  `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("parse: %v (%s)", err, rec.Body)
	}
	// cookbook, miser-money, remote — sorted. NOT off-server, NOT broken.
	if len(parsed) != 3 {
		t.Fatalf("got %d toolsets, want 3: %+v", len(parsed), parsed)
	}
	want := []string{"cookbook", "miser-money", "remote"}
	for i, w := range want {
		if parsed[i].Name != w {
			t.Fatalf("toolsets = %+v, want %v", parsed, want)
		}
	}
	for _, ts := range parsed {
		if !ts.Enabled || !ts.Configured {
			t.Fatalf("%q reported not enabled/configured: %+v", ts.Name, ts)
		}
		// Pi exposes no RPC that enumerates a server's tools, so this must be absent
		// rather than an empty array: the app reads a missing array as "unknown" and
		// an empty one as "this server has no tools", and only the first is true.
		if ts.Tools != nil {
			t.Fatalf("%q advertised a tools array we cannot populate: %+v", ts.Name, ts)
		}
	}
}

// A missing or unreadable mcp.json is a legitimate empty state, not a 5xx: the page
// should render empty. The log line is what distinguishes it from a real empty list.
func TestInventoryToolsetsToleratesMissingMCPConfig(t *testing.T) {
	var logged strings.Builder
	agent := newFakeAgent()
	srv, err := New(Config{
		Password: "test-token",
		AgentDir: t.TempDir(), // exists but has no mcp.json
		Logf:     func(f string, a ...any) { logged.WriteString(f) },
	}, map[string]Agent{"default": agent})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := get(t, srv, "/v1/toolsets", "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if body := rec.Body.String(); body != "[]" {
		t.Fatalf("body = %q, want []", body)
	}
	if !strings.Contains(logged.String(), "mcp.json") {
		t.Fatalf("no log line distinguished this from a deliberate empty list: %q", logged.String())
	}

	// No agent directory configured at all is also fine.
	srv2, err := New(Config{Password: "test-token"}, map[string]Agent{"default": agent})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if rec := get(t, srv2, "/v1/toolsets", "test-token"); rec.Code != http.StatusOK {
		t.Fatalf("no AgentDir: got %d, want 200. body=%s", rec.Code, rec.Body)
	}
}

// A malformed mcp.json must not take the page down either, and must not be silently
// reported as "no servers".
func TestInventoryToolsetsReportsMalformedConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var logged strings.Builder
	agent := newFakeAgent()
	srv, err := New(Config{
		Password: "test-token", AgentDir: dir,
		Logf: func(f string, a ...any) { logged.WriteString(f) },
	}, map[string]Agent{"default": agent})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rec := get(t, srv, "/v1/toolsets", "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(logged.String(), "mcp.json") {
		t.Fatalf("malformed config was silent: %q", logged.String())
	}
}
