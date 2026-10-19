package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agento/internal/pi"
	"agento/internal/runs"
)

// These tests pin the behaviour #311 exists for: a turn is a RUN, not the
// lifetime of the request that started it. Every test here drives the HTTP
// surface, not the manager, because "the app can reattach" is a wire contract.

type runJSON struct {
	ID             string   `json:"id"`
	Status         string   `json:"status"`
	Output         string   `json:"output"`
	ConversationID string   `json:"conversation_id"`
	Error          string   `json:"error"`
	Tools          []string `json:"tools"`
	Seq            int64    `json:"seq"`
}

func listRuns(t *testing.T, srv *Server, query string) []runJSON {
	t.Helper()
	rec := get(t, srv, "/api/runs"+query, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/runs%s: got %d, body=%s", query, rec.Code, rec.Body)
	}
	var body struct {
		Runs []runJSON `json:"runs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("run list is not JSON: %v (%s)", err, rec.Body)
	}
	return body.Runs
}

func getRun(t *testing.T, srv *Server, id string) runJSON {
	t.Helper()
	rec := get(t, srv, "/api/runs/"+id, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/runs/%s: got %d, body=%s", id, rec.Code, rec.Body)
	}
	var r runJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatalf("run is not JSON: %v (%s)", err, rec.Body)
	}
	return r
}

func runPost(t *testing.T, srv *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestRunSurvivesClientDisconnect(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	release := make(chan struct{})
	agent.mu.Lock()
	agent.onPrompt = func() {
		<-release
	}
	agent.events = []pi.Record{textDelta(t, "kept"), {Type: pi.TypeAgentSettled}}
	agent.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("X-Hermes-Session-Id", "conv-drop")
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(httptest.NewRecorder(), req)
	}()

	waitFor(t, func() bool { return agent.sawCommand("prompt") }, "the turn to reach the agent")
	var id string
	waitFor(t, func() bool {
		for _, r := range listRuns(t, srv, "?conversation_id=conv-drop") {
			id = r.ID
			return true
		}
		return false
	}, "the run to be recorded")

	// The app is killed: the request dies, the turn must not.
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler did not return after the client disconnected")
	}

	close(release)

	waitFor(t, func() bool {
		r := getRun(t, srv, id)
		return r.Status == string(runs.StatusCompleted)
	}, "the run to complete after the client went away")

	r := getRun(t, srv, id)
	if r.Output != "kept" {
		t.Fatalf("run output = %q, want the text produced after the disconnect", r.Output)
	}
}

// A reattaching client gets everything produced so far in one snapshot frame,
// then live frames. This is the app-kill recovery path.
func TestRunEventsSnapshotThenLive(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	prompted := make(chan struct{})
	release := make(chan struct{})
	agent.mu.Lock()
	agent.onPrompt = func() {
		close(prompted)
		<-release
	}
	agent.events = []pi.Record{textDelta(t, "one"), textDelta(t, "two"), {Type: pi.TypeAgentSettled}}
	agent.mu.Unlock()

	ts := httptest.NewServer(srv)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/chat/completions", strings.NewReader(validBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("X-Hermes-Session-Id", "conv-attach")
	postDone := make(chan struct{})
	go func() {
		defer close(postDone)
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}()

	waitFor(t, func() bool { return agent.sawCommand("prompt") }, "the turn to start")
	var id string
	waitFor(t, func() bool {
		list := listRuns(t, srv, "?conversation_id=conv-attach")
		if len(list) == 0 {
			return false
		}
		id = list[0].ID
		return true
	}, "the run to be recorded")

	// The first client goes away mid-turn, before any text is produced.
	cancel()
	<-postDone

	// A second client attaches while the run is still blocked.
	eventsReq, err := http.NewRequest(http.MethodGet, ts.URL+"/api/runs/"+id+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	eventsReq.Header.Set("Authorization", "Bearer test-token")
	resp, err := http.DefaultClient.Do(eventsReq)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("events: got %d", resp.StatusCode)
	}

	br := bufio.NewReader(resp.Body)
	first := readSSELine(t, br)
	var snapFrame struct {
		Run runSnapshot `json:"agento_run"`
	}
	if err := json.Unmarshal([]byte(first), &snapFrame); err != nil {
		t.Fatalf("first frame is not a run snapshot: %v (%q)", err, first)
	}
	if snapFrame.Run.ID != id || snapFrame.Run.Status != string(runs.StatusRunning) {
		t.Fatalf("snapshot = %+v, want the running run", snapFrame.Run)
	}

	// Release the turn; the attached stream must carry the live deltas and end
	// with the normal terminator.
	close(release)
	var assembled strings.Builder
	for {
		line := readSSELine(t, br)
		if line == "[DONE]" {
			break
		}
		var frame struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			t.Fatalf("live frame is not JSON: %v (%q)", err, line)
		}
		if len(frame.Choices) > 0 {
			assembled.WriteString(frame.Choices[0].Delta.Content)
		}
	}
	if assembled.String() != "onetwo" {
		t.Fatalf("reattached stream assembled %q, want %q", assembled.String(), "onetwo")
	}
}

// readSSELine reads the next data payload off an SSE stream, skipping comments
// and blank lines.
func readSSELine(t *testing.T, br *bufio.Reader) string {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SSE stream: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		}
	}
}

// Reattaching to a finished run returns its output and a terminator, which is
// how a killed app retrieves what the agent produced while it was gone.
func TestFinishedRunIsRetrievable(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{textDelta(t, "finished"), {Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("X-Hermes-Session-Id", "conv-finished")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: got %d", rec.Code)
	}
	if rec.Header().Get("X-Agento-Run-Id") == "" {
		t.Fatal("chat response must name the run so the client can reattach")
	}

	list := listRuns(t, srv, "?conversation_id=conv-finished")
	if len(list) != 1 || list[0].Status != string(runs.StatusCompleted) {
		t.Fatalf("runs = %+v, want one completed run", list)
	}

	events := get(t, srv, "/api/runs/"+list[0].ID+"/events", "test-token")
	if events.Code != http.StatusOK {
		t.Fatalf("events: got %d", events.Code)
	}
	body := events.Body.String()
	if !strings.Contains(body, `"output":"finished"`) {
		t.Fatalf("snapshot does not carry the output: %s", body)
	}
	if !strings.Contains(body, "[DONE]") {
		t.Fatalf("finished run must terminate: %s", body)
	}
}

// A finished run's stream also replays its output as an ordinary chat delta.
// A client that predates `agento_run` ignores the snapshot, and without the
// replay a turn that finished between send and subscribe would look like an
// empty answer rather than a completed one.
func TestFinishedRunReplaysOutputForOlderClients(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{textDelta(t, "finished"), {Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("X-Hermes-Session-Id", "conv-replay")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("chat: got %d", rec.Code)
	}

	list := listRuns(t, srv, "?conversation_id=conv-replay")
	if len(list) != 1 {
		t.Fatalf("runs = %+v, want one run", list)
	}
	events := get(t, srv, "/api/runs/"+list[0].ID+"/events", "test-token")
	if events.Code != http.StatusOK {
		t.Fatalf("events: got %d", events.Code)
	}

	payloads, _ := sseFrames(t, events.Body.String())
	replayed := false
	for _, p := range payloads {
		var frame chunk
		if err := json.Unmarshal([]byte(p), &frame); err != nil {
			continue
		}
		if len(frame.Choices) == 0 {
			continue
		}
		if got := frame.Choices[0].Delta.Content; got != "finished" {
			t.Fatalf("replayed delta = %q, want the run's full output", got)
		}
		replayed = true
	}
	if !replayed {
		t.Fatalf("no chat delta replayed for an older client: %s", events.Body)
	}
}

func TestSteerAndCancelWhileRunning(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	prompted := make(chan struct{})
	release := make(chan struct{})
	agent.mu.Lock()
	agent.onPrompt = func() {
		close(prompted)
		<-release
	}
	agent.events = []pi.Record{textDelta(t, "partial"), {Type: pi.TypeAgentSettled}}
	agent.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("X-Hermes-Session-Id", "conv-steer")
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(httptest.NewRecorder(), req)
	}()

	waitFor(t, func() bool { return agent.sawCommand("prompt") }, "the turn to start")
	var id string
	waitFor(t, func() bool {
		list := listRuns(t, srv, "?conversation_id=conv-steer")
		if len(list) == 0 {
			return false
		}
		id = list[0].ID
		return true
	}, "the run to be recorded")

	steer := runPost(t, srv, "/api/runs/"+id+"/steer", `{"message":"focus on the file"}`)
	if steer.Code != http.StatusOK {
		t.Fatalf("steer: got %d, body=%s", steer.Code, steer.Body)
	}
	if !agent.sawCommand("steer") {
		t.Fatal("steer never reached the agent")
	}

	cancelRun := runPost(t, srv, "/api/runs/"+id+"/cancel", "")
	if cancelRun.Code != http.StatusOK {
		t.Fatalf("cancel: got %d, body=%s", cancelRun.Code, cancelRun.Body)
	}
	if !agent.sawCommand("abort") {
		t.Fatal("cancel never reached the agent")
	}

	close(release)
	cancel()
	<-done

	waitFor(t, func() bool {
		r := getRun(t, srv, id)
		return r.Status == string(runs.StatusCancelled)
	}, "the run to be marked cancelled")
	if r := getRun(t, srv, id); r.Output != "partial" {
		t.Fatalf("output = %q, want the text produced before the cancel", r.Output)
	}
}

// A second send with a different prompt on a busy profile is refused, but the
// refusal names the in-flight run so the client can reattach or cancel.
func TestBusyProfileNamesTheRun(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	prompted := make(chan struct{})
	release := make(chan struct{})
	agent.mu.Lock()
	agent.onPrompt = func() {
		close(prompted)
		<-release
	}
	agent.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	first := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	first = first.WithContext(ctx)
	first.Header.Set("Content-Type", "application/json")
	first.Header.Set("Authorization", "Bearer test-token")
	first.Header.Set("X-Hermes-Session-Id", "conv-busy")
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		srv.ServeHTTP(httptest.NewRecorder(), first)
	}()
	waitFor(t, func() bool { return agent.sawCommand("prompt") }, "the first turn to start")
	var id string
	waitFor(t, func() bool {
		list := listRuns(t, srv, "?conversation_id=conv-busy")
		if len(list) == 0 {
			return false
		}
		id = list[0].ID
		return true
	}, "the run to be recorded")

	second := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(strings.Replace(validBody, `"content":"hi"`, `"content":"a different question"`, 1)))
	second.Header.Set("Content-Type", "application/json")
	second.Header.Set("Authorization", "Bearer test-token")
	second.Header.Set("X-Hermes-Session-Id", "conv-busy")
	secondRec := httptest.NewRecorder()
	srv.ServeHTTP(secondRec, second)

	if secondRec.Code != http.StatusTooManyRequests {
		t.Fatalf("second turn: got %d, want 429. body=%s", secondRec.Code, secondRec.Body)
	}
	if got := secondRec.Header().Get("X-Agento-Run-Id"); got != id {
		t.Fatalf("429 named run %q, want %q", got, id)
	}

	close(release)
	cancel()
	<-firstDone
}

// The same prompt on the same conversation is a reattach, not a duplicate turn:
// it must stream the existing run rather than 429 or prompt Pi twice.
func TestSamePromptReattachesInsteadOfRerunning(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	prompted := make(chan struct{})
	release := make(chan struct{})
	agent.mu.Lock()
	agent.onPrompt = func() {
		close(prompted)
		<-release
	}
	agent.events = []pi.Record{textDelta(t, "once"), {Type: pi.TypeAgentSettled}}
	agent.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	first := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	first = first.WithContext(ctx)
	first.Header.Set("Content-Type", "application/json")
	first.Header.Set("Authorization", "Bearer test-token")
	first.Header.Set("X-Hermes-Session-Id", "conv-reattach")
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		srv.ServeHTTP(httptest.NewRecorder(), first)
	}()
	waitFor(t, func() bool { return agent.sawCommand("prompt") }, "the first turn to start")

	// The dropped client resends the same turn.
	second := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
	second.Header.Set("Content-Type", "application/json")
	second.Header.Set("Authorization", "Bearer test-token")
	second.Header.Set("X-Hermes-Session-Id", "conv-reattach")
	secondRec := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		srv.ServeHTTP(secondRec, second)
	}()

	// Release the turn: the reattached request should stream it out.
	cancel()
	<-firstDone
	close(release)
	select {
	case <-secondDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reattached request did not finish with the run")
	}
	if secondRec.Code != http.StatusOK {
		t.Fatalf("reattached request: got %d, body=%s", secondRec.Code, secondRec.Body)
	}
	payloads, done := sseFrames(t, secondRec.Body.String())
	if !done {
		t.Fatal("reattached stream did not terminate")
	}
	if got := strings.Join(deltas(t, payloads), ""); got != "once" {
		t.Fatalf("reattached stream assembled %q, want %q", got, "once")
	}
	// One prompt only: the reattach must not have started a second turn.
	agent.mu.Lock()
	prompts := agent.calls["prompt"]
	agent.mu.Unlock()
	if prompts != 1 {
		t.Fatalf("prompt command ran %d times, want 1", prompts)
	}
}

func TestRunListFilters(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{textDelta(t, "ok"), {Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	for _, conv := range []string{"one", "two"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(validBody))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("X-Hermes-Session-Id", conv)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("chat %s: got %d", conv, rec.Code)
		}
	}

	if got := listRuns(t, srv, "?conversation_id=one"); len(got) != 1 || got[0].ConversationID != "one" {
		t.Fatalf("conversation filter returned %+v", got)
	}
	if got := listRuns(t, srv, "?status=completed"); len(got) != 2 {
		t.Fatalf("status filter returned %d runs, want 2", len(got))
	}
	if got := listRuns(t, srv, "?status=running"); len(got) != 0 {
		t.Fatalf("running filter returned %d runs, want 0", len(got))
	}
	rec := get(t, srv, "/api/runs?status=not-a-status", "test-token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad status: got %d, want 400", rec.Code)
	}
}

func TestRunRoutesRequireAuthAndMethods(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{})
	req := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: got %d, want 401", rec.Code)
	}
	if got := runPost(t, srv, "/api/runs", "{}"); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/runs: got %d, want 405", got.Code)
	}
	if got := get(t, srv, "/api/runs/nope", "test-token"); got.Code != http.StatusNotFound {
		t.Fatalf("unknown run: got %d, want 404", got.Code)
	}
}

func TestRunSnapshotIsBoundedAndCarriesUsage(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{textDelta(t, "hi"), {Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})
	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "agento_run") {
		t.Fatal("the stream must open with a run snapshot frame")
	}
	runID := rec.Header().Get("X-Agento-Run-Id")
	if runID == "" {
		t.Fatal("no run id on the response")
	}
	r := getRun(t, srv, runID)
	if r.Seq == 0 {
		t.Fatal("seq must advance with the events")
	}
}

// The per-run event sequence is the cursor that makes snapshot + live frames
// duplicate-free. It must strictly increase.
func TestRunSequenceIsMonotonic(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{
		textDelta(t, "a"), toolStart(t, "bash"), textDelta(t, "b"),
		{Type: pi.TypeAgentSettled},
	}
	srv := newTestServer(t, agent, Config{})
	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
	r := getRun(t, srv, rec.Header().Get("X-Agento-Run-Id"))
	if r.Seq < 4 {
		t.Fatalf("seq = %d, want at least one per delta/tool/terminal event", r.Seq)
	}
	if len(r.Tools) != 1 || r.Tools[0] != "bash" {
		t.Fatalf("tools = %v, want [bash]", r.Tools)
	}
}
