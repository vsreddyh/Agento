package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"agento/internal/pi"
)

// fakeAgent implements Agent with scripted behaviour. No Pi binary, no provider:
// the HTTP contract is what is under test.
type fakeAgent struct {
	mu sync.Mutex
	// responses is keyed by command name.
	responses map[string]any
	// levels is what get_available_thinking_levels reports.
	levels []string
	// events are pushed to subscribers when a prompt arrives.
	events []pi.Record
	// sent records the commands this agent received.
	sent []string
	// calls counts commands by name.
	calls map[string]int

	subMu sync.Mutex
	subs  []chan pi.Record
	// closedSubs simulates the agent dying.
	closedSubs bool

	// failCommands makes the named command return an error.
	failCommands map[string]error

	// stderr is what Stderr() returns. A wedged real agent can emit megabytes.
	stderr string

	// hangCommands makes the named commands block until their context is done, which
	// is how a wedged Pi presents: the RPC never answers. Absent a deadline on the
	// caller's side, that blocks forever.
	hangCommands map[string]bool

	// noSettle suppresses the default agent_settled, so a test can exercise the
	// agent-dying path instead of a turn that completes instantly.
	noSettle bool

	// sessionSeq models Pi holding ONE current session per process: new_session
	// moves to a different file, exactly as the real agent does.
	sessionSeq int

	// curFile and curID track the current session explicitly rather than deriving
	// both from sessionSeq, because switch_session does something the previous
	// model could not express: real Pi lands on the REQUESTED file but assigns a
	// FRESH sessionId for it (measured: e07f... -> e2cb... on an unchanged path).
	// Deriving the id from sessionSeq made every switch look like a no-op, which
	// is why stale-state-after-switch went unnoticed until it was probed.
	curFile string
	curID   string

	// modelID is the model reported by get_state. Empty models a fake that
	// reports none at all, which is a shape Pi may legitimately send.
	modelID string

	// modelProvider is the provider reported by get_state alongside modelID.
	modelProvider string

	// models backs get_available_models. Two models on one provider by
	// default: enough for a switch to have somewhere to go, and for an
	// unknown model to be genuinely unknown.
	models []fakeModel

	// levelsFor reports thinking levels per running model id, falling back
	// to levels when the model has no entry — which is how the handler's
	// post-switch validation reads the NEW model's ladder.
	levelsFor map[string][]string

	// prompts records the text of every prompt sent, so a test can assert what the
	// agent actually receives rather than inferring it from the reply.
	prompts []string

	// onPrompt, when set, runs at the moment a prompt is sent. Lets a test hold a
	// turn open so a second request provably overlaps it.
	onPrompt func()
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{
		responses:     map[string]any{},
		levels:        []string{"off", "minimal", "low", "medium", "high"},
		modelID:       "mimo-v2.6-flash",
		modelProvider: "opencode-go",
		models: []fakeModel{
			{id: "mimo-v2.6-flash", provider: "opencode-go"},
			{id: "muse-spark-1.3-contributor", provider: "opencode-go"},
		},
		calls:  map[string]int{},
		events: []pi.Record{},
		// Initialised rather than left nil: a test that assigns a failing command
		// would otherwise panic on the nil map, which reads as a fake bug rather
		// than the test setup it is.
		failCommands: map[string]error{},
	}
}

// fakeModel is one row of get_available_models: just enough to validate a
// selection against.
type fakeModel struct {
	id       string
	provider string
}

func (f *fakeAgent) Call(ctx context.Context, command string, payload map[string]any) (*pi.Record, error) {
	f.mu.Lock()
	f.calls[command]++
	f.sent = append(f.sent, command)
	resp, hasResp := f.responses[command]
	err := f.failCommands[command]
	f.mu.Unlock()

	if err != nil {
		return nil, err
	}
	if f.hangCommands[command] {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if !hasResp {
		switch command {
		case "get_available_thinking_levels":
			f.mu.Lock()
			if lv, ok := f.levelsFor[f.modelID]; ok {
				resp = map[string]any{"levels": lv}
			} else {
				resp = map[string]any{"levels": f.levels}
			}
			f.mu.Unlock()
		case "get_available_models":
			f.mu.Lock()
			list := make([]map[string]any, 0, len(f.models))
			for _, m := range f.models {
				list = append(list, map[string]any{
					"id": m.id, "provider": m.provider, "reasoning": true,
				})
			}
			resp = map[string]any{"models": list}
			f.mu.Unlock()
		case "set_model":
			// Models the state change, not Pi's validation: unknown values
			// are rejected by the handler's inventory gate before this is
			// ever called, and a Pi-side refusal is scripted with
			// failCommands. "provider/id" splits; a bare id keeps the
			// current provider, matching the handler which always composes
			// the pair itself.
			target, _ := payload["model"].(string)
			id, provider := target, ""
			if i := strings.Index(target, "/"); i >= 0 {
				provider, id = target[:i], target[i+1:]
			}
			f.mu.Lock()
			f.modelID = id
			if provider != "" {
				f.modelProvider = provider
			}
			f.mu.Unlock()
			resp = map[string]any{}
		case "get_state":
			f.mu.Lock()
			if f.curFile == "" {
				f.sessionSeq++
				f.curFile = fmt.Sprintf("/tmp/sessions/sess-%d.jsonl", f.sessionSeq)
				f.curID = fmt.Sprintf("sess-%d", f.sessionSeq)
			}
			resp = map[string]any{
				"sessionId":     f.curID,
				"sessionFile":   f.curFile,
				"thinkingLevel": "off",
				"model": map[string]any{
					"id":       f.modelID,
					"provider": f.modelProvider,
					"api":      "openai-completions",
				},
			}
			f.mu.Unlock()
		case "new_session":
			f.mu.Lock()
			f.sessionSeq++
			f.curFile = fmt.Sprintf("/tmp/sessions/sess-%d.jsonl", f.sessionSeq)
			f.curID = fmt.Sprintf("sess-%d", f.sessionSeq)
			f.mu.Unlock()
			resp = map[string]any{"cancelled": false}
		case "switch_session":
			target, _ := payload["sessionPath"].(string)
			f.mu.Lock()
			// Land on the requested file, but with a new id — which is what Pi
			// does, and the behaviour under test.
			f.curFile = target
			f.sessionSeq++
			f.curID = fmt.Sprintf("sess-%d", f.sessionSeq)
			f.mu.Unlock()
			resp = map[string]any{"cancelled": false}
		default:
			resp = map[string]any{}
		}
	}
	data, mErr := json.Marshal(resp)
	if mErr != nil {
		return nil, mErr
	}
	success := true
	return &pi.Record{ID: "fake", Command: command, Success: &success, Data: data}, nil
}

func (f *fakeAgent) Send(ctx context.Context, command string, payload map[string]any) error {
	f.mu.Lock()
	f.sent = append(f.sent, command)
	f.calls[command]++
	pushing := command == "prompt"
	if pushing {
		f.prompts = append(f.prompts, promptText(payload))
	}
	events := append([]pi.Record(nil), f.events...)
	settles := len(events) == 0 && !f.noSettle
	f.mu.Unlock()

	if pushing && f.onPrompt != nil {
		f.onPrompt()
	}
	if pushing {
		if settles {
			// Default to a turn that completes immediately. A fake that never
			// settles makes the gateway wait out its full turn timeout, which is
			// correct behaviour but useless as a test default.
			events = []pi.Record{{Type: pi.TypeAgentSettled}}
		}
		f.deliver(events)
	}
	return nil
}

// promptText extracts the text the gateway sent to the agent. promptPayload
// puts it under "message", with images alongside; there is no other text key, so
// anything else means the payload shape changed and this should report nothing
// rather than guess.
func promptText(payload map[string]any) string {
	s, _ := payload["message"].(string)
	return s
}

func (f *fakeAgent) Subscribe() (<-chan pi.Record, func()) {
	ch := make(chan pi.Record, 256)
	f.subMu.Lock()
	if f.closedSubs {
		close(ch)
		f.subMu.Unlock()
		return ch, func() {}
	}
	f.subs = append(f.subs, ch)
	f.subMu.Unlock()
	return ch, func() { f.subMu.Lock(); defer f.subMu.Unlock() }
}

func (f *fakeAgent) deliver(events []pi.Record) {
	f.subMu.Lock()
	pending := append([]chan pi.Record(nil), f.subs...)
	f.subMu.Unlock()
	for _, ch := range pending {
		for _, ev := range events {
			ch <- ev
		}
	}
}

// die closes every subscriber channel, as the real client does on process exit.
func (f *fakeAgent) die() {
	f.subMu.Lock()
	defer f.subMu.Unlock()
	f.closedSubs = true
	for _, ch := range f.subs {
		close(ch)
	}
	f.subs = nil
}

// stderr is what Stderr() reports, so a test can model a noisy agent.
func (f *fakeAgent) Stderr() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stderr
}

func (f *fakeAgent) commandCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeAgent) sawCommand(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.sent {
		if s == name {
			return true
		}
	}
	return false
}

// helpers to build Pi-shaped event records.

func eventRecord(t *testing.T, typ, body string) pi.Record {
	t.Helper()
	return pi.Record{Type: typ, Raw: json.RawMessage(body)}
}

func textDelta(t *testing.T, text string) pi.Record {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"type":                  "message_update",
		"usage":                 map[string]any{"input": 10, "output": 2, "totalTokens": 12},
		"assistantMessageEvent": map[string]any{"type": "text_delta", "contentIndex": 0, "delta": text},
	})
	if err != nil {
		t.Fatal(err)
	}
	return pi.Record{Type: "message_update", Raw: body}
}

func toolStart(t *testing.T, name string) pi.Record {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"type": "tool_execution_start", "toolName": name,
	})
	if err != nil {
		t.Fatal(err)
	}
	return pi.Record{Type: "tool_execution_start", Raw: body}
}

// newTestServer builds a Server over a fake agent, ready to exercise the HTTP
// contract end to end.
func newTestServer(t *testing.T, agent *fakeAgent, cfg Config) *Server {
	t.Helper()
	if cfg.Password == "" {
		cfg.Password = "test-token"
	}
	srv, err := New(cfg, map[string]Agent{"default": agent})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func post(t *testing.T, srv *Server, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// get is post's read-only counterpart, for the inventory routes. No Content-Type:
// these routes take no body, and setting one would suggest they do.
func get(t *testing.T, srv *Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

const validBody = `{"model":"mimo-v2.6-flash","model_options":{"reasoning_effort":"low"},` +
	`"messages":[{"role":"user","content":"hi"}],"stream":true}`

func TestMissingReasoningEffortIsRejected(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	// No model_options at all.
	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing model_options: got %d, want 400. body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "reasoning_effort") {
		t.Fatalf("error should name the field, got %s", rec.Body)
	}

	// Present but empty.
	rec = post(t, srv, "/v1/chat/completions", "test-token",
		`{"model_options":{"reasoning_effort":"  "},"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("blank reasoning_effort: got %d, want 400", rec.Code)
	}

	// The agent must never have been prompted for a rejected request.
	if agent.sawCommand("prompt") {
		t.Fatal("a rejected request still reached the agent")
	}
	if agent.sawCommand("set_thinking_level") {
		t.Fatal("a rejected request still mutated the agent's reasoning level")
	}
}

func TestUnknownReasoningEffortIsRejected(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	for _, effort := range []string{"turbo", "maximum", "yes"} {
		body := fmt.Sprintf(
			`{"model_options":{"reasoning_effort":%q},"messages":[{"role":"user","content":"hi"}]}`, effort)
		rec := post(t, srv, "/v1/chat/completions", "test-token", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("effort %q: got %d, want 400. body=%s", effort, rec.Code, rec.Body)
		}
	}
	if agent.sawCommand("prompt") {
		t.Fatal("an unrecognised effort still reached the agent")
	}
}

// A level Pi supports but this model does not must be refused, naming what is
// valid. Pi would have accepted it silently.
func TestUnsupportedReasoningEffortIsRejectedWithValidList(t *testing.T) {
	agent := newFakeAgent()
	agent.levels = []string{"off", "low", "high"} // no "medium"
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model_options":{"reasoning_effort":"medium"},"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400. body=%s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"off", "low", "high"} {
		if !strings.Contains(body, want) {
			t.Fatalf("error should list %q as supported, got %s", want, body)
		}
	}
}

// "none" is the app's spelling for "no reasoning"; it must map onto Pi's "off"
// rather than being rejected as unknown.
func TestNoneMapsToOff(t *testing.T) {
	agent := newFakeAgent()
	agent.levels = []string{"off", "low"}
	agent.events = []pi.Record{textDelta(t, "ok"), {Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model_options":{"reasoning_effort":"none"},"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "ok") {
		t.Fatalf("stream should carry the text, got %s", rec.Body)
	}
}

func TestValidEffortAppliesTheRightLevel(t *testing.T) {
	// "off" and "none" both mean "no reasoning" — the first is Pi's own
	// spelling, the second is what the app's older settings send. Rejecting
	// either is a bug, and "off" is the value a caller is most likely to try.
	for _, tc := range []struct{ effort, want string }{
		{"off", "off"},
		{"none", "off"},
		{"OFF", "off"},
		{"low", "low"},
		{"high", "high"},
		{"HIGH", "high"}, // case-insensitive
		{" medium ", "medium"},
	} {
		agent := newFakeAgent()
		agent.levels = []string{"off", "minimal", "low", "medium", "high"}
		agent.events = []pi.Record{{Type: pi.TypeAgentSettled}}
		srv := newTestServer(t, agent, Config{})

		body := fmt.Sprintf(
			`{"model_options":{"reasoning_effort":%q},"messages":[{"role":"user","content":"hi"}]}`, tc.effort)
		if rec := post(t, srv, "/v1/chat/completions", "test-token", body); rec.Code != http.StatusOK {
			t.Fatalf("effort %q: got %d, want 200. body=%s", tc.effort, rec.Code, rec.Body)
		}
		if !agent.sawCommand("set_thinking_level") {
			t.Fatalf("effort %q did not set a thinking level", tc.effort)
		}
	}
}

func TestAuthRequired(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{Password: "secret"})

	cases := []struct {
		name, header string
		want         int
	}{
		{"no header", "", http.StatusUnauthorized},
		{"wrong token", "Bearer nope", http.StatusUnauthorized},
		{"not bearer", "Basic secret", http.StatusUnauthorized},
		{"empty bearer", "Bearer ", http.StatusUnauthorized},
		{"correct", "Bearer secret", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
				strings.NewReader(validBody))
			req.Header.Set("Content-Type", "application/json")
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			srv.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("got %d, want %d. body=%s", rec.Code, tc.want, rec.Body)
			}
		})
	}
}

func TestHealthIsUnauthenticated(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{Password: "secret"})

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz should not require auth, got %d", rec.Code)
	}
}

func TestEmptyPasswordRefusedUnlessAllowed(t *testing.T) {
	agent := newFakeAgent()
	if _, err := New(Config{}, map[string]Agent{"default": agent}); err == nil {
		t.Fatal("an empty password must be refused by default")
	}
	if _, err := New(Config{AllowNoAuth: true}, map[string]Agent{"default": agent}); err != nil {
		t.Fatalf("AllowNoAuth should permit it, got %v", err)
	}
}
