package gateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agento/internal/pi"
	"agento/internal/runs"
)

// errTestWrite simulates a dead child stdin for the failure-path test.
var errTestWrite = errors.New("test: stdin gone")

// testServer wraps httptest so streaming tests can hold a turn open while
// answering mid-turn: post() alone cannot, because the handler only returns
// when the stream ends.
func newServer(t *testing.T, srv *Server) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts
}

func jsonRequest(t *testing.T, method, url, body string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	return req
}

// streamTap is a background SSE reader for one live stream. sawQuestion
// flips when an agento_question frame arrives; stop ends the read.
type streamTap struct {
	stop     chan struct{}
	mu       sync.Mutex
	question bool
}

func (s *streamTap) sawQuestion() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.question
}

func sseReader(t *testing.T, ts *httptest.Server, path string, headers map[string]string) *streamTap {
	t.Helper()
	tap := &streamTap{stop: make(chan struct{})}
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, strings.NewReader(validBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		// Frames are single lines; the cap is generous, not a bound.
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			select {
			case <-tap.stop:
				return
			default:
			}
			line := strings.TrimSpace(sc.Text())
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var frame struct {
				Question *questionWire `json:"agento_question"`
			}
			if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &frame); err != nil {
				continue
			}
			if frame.Question != nil {
				tap.mu.Lock()
				tap.question = true
				tap.mu.Unlock()
			}
		}
	}()
	return tap
}

// getEvents attaches to a live run and reads until the first question
// frame, then closes: the stream itself never ends while the run is live.
func getEvents(t *testing.T, ts *httptest.Server, runID string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/runs/"+runID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET events: got %d, want 200", resp.StatusCode)
	}
	var sb strings.Builder
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	deadline := time.Now().Add(10 * time.Second)
	for sc.Scan() {
		line := sc.Text()
		sb.WriteString(line + "\n")
		if strings.Contains(line, `"agento_question"`) || time.Now().After(deadline) {
			break
		}
	}
	return sb.String()
}

// newRunForTest builds the minimal Run a liveRun needs for relay tests that
// never prompt: profile + conversation only.
func newRunForTest(profile, conversation string) runs.Run {
	return runs.Run{Profile: profile, ConversationID: conversation, Prompt: "hi"}
}

// uiRequest builds an extension_ui_request record the way Pi emits it:
// method + dialog fields on the envelope, id echoed for the response.
func uiRecord(t *testing.T, id, method string, extra map[string]any) pi.Record {
	t.Helper()
	payload := map[string]any{"id": id, "method": method}
	for k, v := range extra {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	// Pi sends the envelope type at top level; the body repeats the id.
	env, err := json.Marshal(map[string]any{"type": "extension_ui_request"})
	if err != nil {
		t.Fatal(err)
	}
	merged := map[string]any{}
	if err := json.Unmarshal(env, &merged); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &merged); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	return pi.Record{Type: "extension_ui_request", Raw: raw}
}

func questionFrames(t *testing.T, payloads []string) []questionWire {
	t.Helper()
	var out []questionWire
	for _, p := range payloads {
		var frame struct {
			Question *questionWire `json:"agento_question"`
		}
		if err := json.Unmarshal([]byte(p), &frame); err != nil {
			t.Fatalf("frame is not JSON: %q (%v)", p, err)
		}
		if frame.Question != nil {
			out = append(out, *frame.Question)
		}
	}
	return out
}

// Pi's own rpc-types put the prompt in `title` for every dialog method,
// so a blank title means a future shape — fall back to `message` rather
// than render "A question from the agent" for a question Pi sent.
func TestQuestionTitleFallsBackToMessage(t *testing.T) {
	req, ok := decodeExtensionUI(uiRecord(t, "q-m", "input", map[string]any{
		"message": "What fuel?",
	}))
	if !ok {
		t.Fatal("message-only request did not decode")
	}
	if got := titleOf(req); got != "What fuel?" {
		t.Fatalf("title = %q, want the message fallback", got)
	}
	req, ok = decodeExtensionUI(uiRecord(t, "q-t", "input", map[string]any{
		"title": "Pick one", "message": "ignored",
	}))
	if !ok {
		t.Fatal("titled request did not decode")
	}
	if got := titleOf(req); got != "Pick one" {
		t.Fatalf("title = %q, want title to win over message", got)
	}
}

// A failed stdin write is a 502 with the slot left answerable — not a
// 404, and not a 409 on retry. A transient failure must not make the
// question unanswerable for the rest of the turn.
func TestQuestionAnswerFailureKeepsSlot(t *testing.T) {
	agent := newFakeAgent()
	agent.noSettle = true
	agent.onPrompt = func() {
		agent.deliver([]pi.Record{uiRecord(t, "q-f", "select", map[string]any{
			"title": "Which?", "options": []string{"Joint", "Other"},
		})})
	}
	agent.failCommands["extension_ui_response"] = errTestWrite
	srv := newTestServer(t, agent, Config{})
	ts := newServer(t, srv)

	stream := sseReader(t, ts, "/v1/chat/completions", map[string]string{"X-Hermes-Session-Id": "conv-f"})
	waitFor(t, stream.sawQuestion, "the question frame")
	close(stream.stop)

	answer := func(body string) int {
		req := jsonRequest(t, http.MethodPost, ts.URL+"/default/api/questions/q-f", body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := answer(`{"value":"Joint"}`); code != http.StatusBadGateway {
		t.Fatalf("failed write: got %d, want 502", code)
	}
	// The slot is still answerable: clear the fault and retry the same id.
	agent.mu.Lock()
	delete(agent.failCommands, "extension_ui_response")
	agent.mu.Unlock()
	if code := answer(`{"value":"Joint"}`); code != http.StatusOK {
		t.Fatalf("retry after failed write: got %d, want 200", code)
	}
	if len(agent.responds) != 1 || agent.responds[0].id != "q-f" {
		t.Fatalf("responds = %+v, want the single retried answer", agent.responds)
	}
	agent.deliver([]pi.Record{{Type: pi.TypeAgentSettled}})
}

// A select answer must be one of the offered options: anything else is
// a 400 that leaves the slot answerable (unlike 404/409).
func TestQuestionSelectValidatesOptions(t *testing.T) {
	agent := newFakeAgent()
	agent.noSettle = true
	agent.onPrompt = func() {
		agent.deliver([]pi.Record{uiRecord(t, "q-v", "select", map[string]any{
			"title": "Which?", "options": []string{"a", "b"},
		})})
	}
	srv := newTestServer(t, agent, Config{})
	ts := newServer(t, srv)

	stream := sseReader(t, ts, "/v1/chat/completions", map[string]string{"X-Hermes-Session-Id": "conv-v"})
	waitFor(t, stream.sawQuestion, "the question frame")
	close(stream.stop)

	answer := func(body string) int {
		req := jsonRequest(t, http.MethodPost, ts.URL+"/default/api/questions/q-v", body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := answer(`{"value":"c"}`); code != http.StatusBadRequest {
		t.Fatalf("off-menu value: got %d, want 400", code)
	}
	if len(agent.responds) != 0 {
		t.Fatalf("rejected answer wrote to Pi: %+v", agent.responds)
	}
	// The slot is still open: the offered value resolves.
	if code := answer(`{"value":"a"}`); code != http.StatusOK {
		t.Fatalf("offered value after 400: got %d, want 200", code)
	}
	if len(agent.responds) != 1 || agent.responds[0].payload["value"] != "a" {
		t.Fatalf("responds = %+v, want the single valid answer", agent.responds)
	}
	agent.deliver([]pi.Record{{Type: pi.TypeAgentSettled}})
}

// Stored options are trimmed at decode, so a padded option still matches
// the trimmed value the app sends back.
func TestQuestionOptionsTrimmedAtDecode(t *testing.T) {
	req, ok := decodeExtensionUI(uiRecord(t, "q-t", "select", map[string]any{
		"title": "Which?", "options": []string{"  Joint  ", "Other"},
	}))
	if !ok {
		t.Fatal("request did not decode")
	}
	if len(req.Options) != 2 || req.Options[0] != "Joint" {
		t.Fatalf("options = %q, want trimmed", req.Options)
	}
}

// A select dialog mid-turn publishes one agento_question frame carrying the
// request's id, and the question never lands in the stored output.
func TestQuestionFramePublished(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{
		uiRecord(t, "q-1", "select", map[string]any{
			"title": "Which account?", "options": []string{"Joint", "Personal"},
		}),
		textDelta(t, "answer"),
		{Type: pi.TypeAgentSettled},
	}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	payloads, sawDone := sseFrames(t, rec.Body.String())
	if !sawDone {
		t.Fatal("stream did not terminate with [DONE]")
	}
	frames := questionFrames(t, payloads)
	if len(frames) != 1 {
		t.Fatalf("got %d question frames, want 1", len(frames))
	}
	q := frames[0]
	if q.ID != "q-1" || q.Method != "select" || q.Title != "Which account?" {
		t.Fatalf("question frame = %+v, want id q-1 select 'Which account?'", q)
	}
	if len(q.Options) != 2 || q.Options[0] != "Joint" || q.Options[1] != "Personal" {
		t.Fatalf("question options = %v, want [Joint Personal]", q.Options)
	}
	if q.CreatedAt <= 0 {
		t.Fatal("question frame must carry created_at")
	}
	// The turn ended with the question unanswered: teardown cancels it, so
	// Pi unwinds instead of sitting blocked past the settle.
	if len(agent.responds) != 1 {
		t.Fatalf("got %d extension_ui_response writes, want 1 (turn-end cancel)", len(agent.responds))
	}
	if agent.responds[0].id != "q-1" {
		t.Fatalf("cancel answered id %q, want the request id q-1", agent.responds[0].id)
	}
	if agent.responds[0].payload["cancelled"] != true {
		t.Fatalf("cancel payload = %v, want {cancelled:true}", agent.responds[0].payload)
	}
	// The question is a frame, not output: the stored record stays clean.
	runs := listRuns(t, srv, "?conversation_id=")
	if len(runs) == 0 {
		t.Fatal("no runs recorded")
	}
	for _, r := range runs {
		if strings.Contains(r.Output, "Which account?") || strings.Contains(r.Output, "agento_question") {
			t.Fatalf("question leaked into stored output: %q", r.Output)
		}
	}
}

// Answering posts the value back to Pi under the REQUEST's id; a second
// answer is a 409 and never a second stdin write.
func TestQuestionAnswerResolves(t *testing.T) {
	agent := newFakeAgent()
	agent.noSettle = true
	// Deliver the dialog when the prompt lands and hold the turn open: the
	// test answers mid-turn, then settles it by hand.
	agent.onPrompt = func() {
		agent.deliver([]pi.Record{uiRecord(t, "q-9", "select", map[string]any{
			"title": "Which account?", "options": []string{"Joint", "Personal"},
		})})
	}
	srv := newTestServer(t, agent, Config{})
	ts := newServer(t, srv)

	// Stream in the background: the answer lands mid-turn.
	stream := sseReader(t, ts, "/v1/chat/completions", map[string]string{"X-Hermes-Session-Id": "conv-q"})
	waitFor(t, stream.sawQuestion, "the question frame")
	close(stream.stop)

	answer := func(body string) int {
		req := jsonRequest(t, http.MethodPost, ts.URL+"/default/api/questions/q-9", body)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if code := answer(`{"value":"Joint"}`); code != http.StatusOK {
		t.Fatalf("answer: got %d, want 200", code)
	}
	if len(agent.responds) != 1 {
		t.Fatalf("got %d extension_ui_response writes, want 1", len(agent.responds))
	}
	if agent.responds[0].id != "q-9" {
		t.Fatalf("answered id %q, want the request id q-9", agent.responds[0].id)
	}
	if agent.responds[0].payload["value"] != "Joint" {
		t.Fatalf("answer payload = %v, want {value:Joint}", agent.responds[0].payload)
	}
	// Second answer: 409, and still exactly one stdin write.
	if code := answer(`{"value":"Personal"}`); code != http.StatusConflict {
		t.Fatalf("repeat answer: got %d, want 409", code)
	}
	if len(agent.responds) != 1 {
		t.Fatalf("repeat answer wrote to Pi again: %d writes", len(agent.responds))
	}
	// Unknown id: 404.
	unknown := jsonRequest(t, http.MethodPost, ts.URL+"/default/api/questions/nope", `{"value":"x"}`)
	if resp, err := http.DefaultClient.Do(unknown); err != nil {
		t.Fatal(err)
	} else {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown question: got %d, want 404", resp.StatusCode)
		}
	}
	// Settling the turn clears the slot without a second write: the answer
	// already unblocked Pi, so teardown must not answer again.
	agent.deliver([]pi.Record{{Type: pi.TypeAgentSettled}})
	waitFor(t, func() bool {
		for _, r := range listRuns(t, srv, "?conversation_id=conv-q") {
			if r.Status != "" && r.Status != "running" {
				return true
			}
		}
		return false
	}, "the turn to settle")
	if len(agent.responds) != 1 {
		t.Fatalf("teardown wrote again: %d writes, want exactly the answer", len(agent.responds))
	}
}

// A question asked while the app is closed is re-emitted on reattach: the
// second stream gets the card right after its snapshot.
func TestQuestionReemittedOnReattach(t *testing.T) {
	agent := newFakeAgent()
	agent.noSettle = true
	agent.onPrompt = func() {
		agent.deliver([]pi.Record{uiRecord(t, "q-r", "input", map[string]any{
			"title": "What fuel?",
		})})
	}
	srv := newTestServer(t, agent, Config{})
	ts := newServer(t, srv)

	first := sseReader(t, ts, "/v1/chat/completions", map[string]string{"X-Hermes-Session-Id": "conv-r"})
	waitFor(t, first.sawQuestion, "the question frame on the first stream")
	close(first.stop)

	// Reattach on a fresh stream: snapshot first, then the pending card.
	var runID string
	waitFor(t, func() bool {
		for _, r := range listRuns(t, srv, "?conversation_id=conv-r") {
			runID = r.ID
			return true
		}
		return false
	}, "the live run to be recorded")
	rec := getEvents(t, ts, runID)
	payloads, _ := sseFrames(t, rec)
	frames := questionFrames(t, payloads)
	if len(frames) != 1 || frames[0].ID != "q-r" || frames[0].Method != "input" {
		t.Fatalf("reattach frames = %+v, want one input card q-r", frames)
	}
	if frames[0].Title != "What fuel?" {
		t.Fatalf("reattach title = %q, want 'What fuel?'", frames[0].Title)
	}
	agent.deliver([]pi.Record{{Type: pi.TypeAgentSettled}})
}

// No stream attached (scheduler fires never subscribe): the dialog fails
// fast instead of burning the whole TurnTimeout.
func TestQuestionUnattendedCancels(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})
	lr := srv.runs.begin(newRunForTest("chat", "conv-u"), agent, "c", "", time.Now())
	defer srv.runs.finish(lr, time.Now())
	srv.handleExtensionUI(lr, uiRecord(t, "q-u", "select", map[string]any{
		"title": "Which?", "options": []string{"a", "b"},
	}))
	if len(agent.responds) != 1 || agent.responds[0].payload["cancelled"] != true {
		t.Fatalf("responds = %+v, want one cancelled write", agent.responds)
	}
	if qw, ok := srv.pendingFor("chat", "conv-u"); ok {
		t.Fatalf("unattended question held: %+v", qw)
	}
}

// Unknown dialog methods fail fast; fire-and-forget methods are ignored.
func TestQuestionUnknownAndFireForget(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{
		uiRecord(t, "q-w", "teleport", nil),
		uiRecord(t, "q-n", "notify", map[string]any{"message": "hi"}),
		{Type: pi.TypeAgentSettled},
	}
	srv := newTestServer(t, agent, Config{})
	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	payloads, _ := sseFrames(t, rec.Body.String())
	if frames := questionFrames(t, payloads); len(frames) != 0 {
		t.Fatalf("got %d question frames, want none", len(frames))
	}
	// Exactly one write: the unknown dialog's cancel. The notify and the
	// turn-end teardown (nothing held) write nothing.
	if len(agent.responds) != 1 {
		t.Fatalf("got %d writes, want 1 (unknown-dialog cancel)", len(agent.responds))
	}
	if agent.responds[0].id != "q-w" || agent.responds[0].payload["cancelled"] != true {
		t.Fatalf("writes = %+v, want cancelled q-w", agent.responds)
	}
}

// The answer endpoint speaks the gateway's auth/bound dialect: 401 without
// a token, 405 on GET, 400 on empty or conflicting bodies.
func TestQuestionEndpointGuards(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	if rec := get(t, srv, "/default/api/questions/q-1", "test-token"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: got %d, want 405", rec.Code)
	}
	bad := post(t, srv, "/default/api/questions/q-1", "bad-token", `{"value":"x"}`)
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: got %d, want 401", bad.Code)
	}
	empty := post(t, srv, "/default/api/questions/q-1", "test-token", `{"value":"  "}`)
	if empty.Code != http.StatusBadRequest {
		t.Fatalf("empty value: got %d, want 400", empty.Code)
	}
	conflict := post(t, srv, "/default/api/questions/q-1", "test-token", `{"value":"x","cancelled":true}`)
	if conflict.Code != http.StatusBadRequest {
		t.Fatalf("value+cancelled: got %d, want 400", conflict.Code)
	}
	long := post(t, srv, "/default/api/questions/q-1", "test-token",
		`{"value":"`+strings.Repeat("y", 60000)+`"}`)
	if long.Code != http.StatusBadRequest {
		t.Fatalf("overlong value: got %d, want 400", long.Code)
	}
	missing := post(t, srv, "/default/api/questions/q-1", "test-token", `{"value":"x"}`)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("no pending question: got %d, want 404", missing.Code)
	}
}
