package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"agento/internal/pi"
	"agento/internal/runs"
)

// This file is the resumable-run half of the chat surface (#311).
//
// The important split is between a REQUEST and a RUN. Before this, one handler
// goroutine did both: it held the profile lock, prompted Pi, and wrote frames
// to the HTTP response. A client that went away took the turn with it, because
// the turn's context was the request's context.
//
// Here the executor owns the turn and the request merely subscribes:
//
//	POST /v1/chat/completions   validate, acquire the profile, start a run,
//	                            then stream that run's events
//	GET  /api/runs/{id}/events  subscribe to the same run, starting with a
//	                            snapshot of everything already produced
//
// Both streams carry the frames the app already parses. A reattach starts with
// one `agento_run` snapshot frame (which older clients ignore) and then live
// frames, so it needs no second protocol.

// runPersistInterval is how often a live run's accumulated state is written to
// the store. One write a second is enough for the app's run list to show
// progress, and far cheaper than persisting every delta.
const runPersistInterval = time.Second

// runFrame is the snapshot frame. It is the only new wire shape this feature
// adds; everything after it is the existing chat SSE vocabulary.
type runFrame struct {
	Run runSnapshot `json:"agento_run"`
}

type runSnapshot struct {
	ID         string   `json:"id"`
	Status     string   `json:"status"`
	Seq        int64    `json:"seq"`
	Model      string   `json:"model,omitempty"`
	Output     string   `json:"output,omitempty"`
	Reasoning  string   `json:"reasoning,omitempty"`
	Tools      []string `json:"tools,omitempty"`
	Usage      *usage   `json:"usage,omitempty"`
	Error      string   `json:"error,omitempty"`
	CreatedAt  int64    `json:"created_at,omitempty"`
	UpdatedAt  int64    `json:"updated_at,omitempty"`
	FinishedAt int64    `json:"finished_at,omitempty"`
}

// usageWire converts the stored token counts into the chat stream's spelling,
// so the app parses a run snapshot and a chat chunk with the same helper.
func usageWire(u runs.Usage) *usage {
	return &usage{
		PromptTokens:     u.Prompt,
		CompletionTokens: u.Completion,
		TotalTokens:      u.Total,
		CacheReadTokens:  u.Cached,
		CacheWriteTokens: u.CacheWrite,
	}
}

func snapshotOf(r runs.Run) runSnapshot {
	s := runSnapshot{
		ID:         r.ID,
		Status:     string(r.Status),
		Seq:        r.Seq,
		Model:      r.Model,
		Output:     r.Output,
		Reasoning:  r.Reasoning,
		Tools:      append([]string(nil), r.Tools...),
		Error:      r.Error,
		CreatedAt:  r.CreatedAt,
		UpdatedAt:  r.UpdatedAt,
		FinishedAt: r.FinishedAt,
	}
	if r.Usage.NonZero() {
		s.Usage = usageWire(r.Usage)
	}
	return s
}

// runEvent is one frame queued for subscribers. Seq is assigned in publish
// order; a subscriber that starts from a snapshot drops everything at or below
// the snapshot's Seq, which is what makes "snapshot then live" duplicate-free.
type runEvent struct {
	Seq  int64
	Data string
	Done bool
}

// liveRun is one executing run plus everything needed to observe and steer it.
type liveRun struct {
	mu sync.Mutex

	run    runs.Run
	agent  Agent
	out    strings.Builder
	reason strings.Builder

	// completionID is the id every chat chunk carries; fixed at start.
	completionID string

	seq             int64
	savedSeq        int64
	subs            map[chan runEvent]struct{}
	subsClosed      bool
	cancelRequested bool
	finished        bool
	// promptFull is the user turn exactly as sent. The persisted Prompt is
	// bounded for storage, but a reattach compares the full text: two different
	// prompts that share a 4 KB prefix are not the same turn.
	promptFull string

	// done closes when the executor has finished (after the store write).
	done chan struct{}
}

func newLiveRun(r runs.Run, agent Agent, completionID string) *liveRun {
	return &liveRun{
		run:          r,
		agent:        agent,
		completionID: completionID,
		subs:         map[chan runEvent]struct{}{},
		done:         make(chan struct{}),
	}
}

func (lr *liveRun) id() string { return lr.run.ID }
func (lr *liveRun) prompt() string {
	if lr.promptFull != "" {
		return lr.promptFull
	}
	return lr.run.Prompt
}

func (lr *liveRun) snapshot() runs.Run {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.snapshotLocked()
}

// persistState returns the current state and whether anything changed since the
// last write. A run parked in a long tool call would otherwise rewrite an
// unchanging document once a second.
func (lr *liveRun) persistState() (runs.Run, bool) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.seq == lr.savedSeq {
		return runs.Run{}, false
	}
	lr.savedSeq = lr.seq
	return lr.snapshotLocked(), true
}

func (lr *liveRun) snapshotLocked() runs.Run {
	r := lr.run.Clone()
	r.Output = lr.out.String()
	r.Reasoning = lr.reason.String()
	r.Seq = lr.seq
	return r
}

func (lr *liveRun) isRunning() bool {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return !lr.finished && lr.run.Status == runs.StatusRunning
}

// requestCancel marks the run cancelled and asks Pi to abort. The turn still
// ends through the normal event loop, so output produced up to the abort is
// kept and the run is closed exactly once.
func (lr *liveRun) requestCancel(ctx context.Context) error {
	lr.mu.Lock()
	if lr.finished || lr.run.Status != runs.StatusRunning {
		lr.mu.Unlock()
		return fmt.Errorf("run is not in flight")
	}
	lr.cancelRequested = true
	lr.mu.Unlock()

	// Send, not Call: abort's response arrives only once the session is idle,
	// which is after the run loop has already observed agent_settled. Waiting
	// here would just duplicate that wait.
	if err := lr.agent.Send(ctx, "abort", nil); err != nil {
		// The abort never reached Pi, so the turn will run to completion. Do
		// not leave the flag set: it would relabel a completed turn cancelled.
		lr.mu.Lock()
		if !lr.finished {
			lr.cancelRequested = false
		}
		lr.mu.Unlock()
		return err
	}
	return nil
}

// steer queues an instruction into the running turn. Pi delivers it after the
// current assistant turn's tool calls, before the next model call.
func (lr *liveRun) steer(ctx context.Context, message string) error {
	if !lr.isRunning() {
		return fmt.Errorf("run is not in flight")
	}
	if _, err := lr.agent.Call(ctx, "steer", map[string]any{"message": message}); err != nil {
		return err
	}
	return nil
}

func (lr *liveRun) subscribe() (<-chan runEvent, func()) {
	ch := make(chan runEvent, 256)
	lr.mu.Lock()
	if lr.finished {
		close(ch)
		lr.mu.Unlock()
		return ch, func() {}
	}
	lr.subs[ch] = struct{}{}
	lr.mu.Unlock()
	return ch, func() {
		lr.mu.Lock()
		defer lr.mu.Unlock()
		if _, ok := lr.subs[ch]; ok {
			delete(lr.subs, ch)
			close(ch)
		}
	}
}

// publish assigns the next sequence, delivers to every subscriber, and drops a
// subscriber that has stopped reading.
//
// Dropping is deliberate: the run holds the profile's only turn and must not
// stall because one client stopped draining a socket. The dropped client sees
// EOF and can reattach, at which point the snapshot carries everything it
// missed.
func (lr *liveRun) publish(frame any, done bool) int64 {
	data, err := jsonPayload(frame)
	if err != nil {
		return lr.seq
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.seq++
	ev := runEvent{Seq: lr.seq, Data: data, Done: done}
	for ch := range lr.subs {
		select {
		case ch <- ev:
		default:
			delete(lr.subs, ch)
			close(ch)
		}
	}
	return lr.seq
}

// publishDelta applies a text/reasoning increment to the run and emits the
// corresponding chat chunk in one critical section, so a reattaching snapshot
// can never observe output that was not also sent, or vice versa in a way that
// produces a gap.
func (lr *liveRun) publishDelta(text, reasoning string, frame chunk) int64 {
	data, err := jsonPayload(frame)
	if err != nil {
		return 0
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.out.WriteString(text)
	if lr.reason.Len() < runs.MaxReasoning {
		lr.reason.WriteString(reasoning)
	}
	lr.seq++
	ev := runEvent{Seq: lr.seq, Data: data}
	for ch := range lr.subs {
		select {
		case ch <- ev:
		default:
			delete(lr.subs, ch)
			close(ch)
		}
	}
	return lr.seq
}

func (lr *liveRun) publishTool(name string, frame toolFrame) int64 {
	data, err := jsonPayload(frame)
	if err != nil {
		return 0
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if len(lr.run.Tools) < runs.MaxTools && !containsString(lr.run.Tools, name) {
		lr.run.Tools = append(lr.run.Tools, name)
	}
	lr.seq++
	ev := runEvent{Seq: lr.seq, Data: data}
	for ch := range lr.subs {
		select {
		case ch <- ev:
		default:
			delete(lr.subs, ch)
			close(ch)
		}
	}
	return lr.seq
}

func (lr *liveRun) setUsage(u piUsage) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.run.Usage = runs.Usage{
		Prompt:     u.Input,
		Completion: u.Output,
		Total:      u.Total,
		Cached:     u.CacheRead,
		CacheWrite: u.CacheWrite,
	}
}

// closeSubs ends every attached stream. Safe to call more than once: the
// deferred path always calls it after a terminal path may already have.
func (lr *liveRun) closeSubs() {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.subsClosed {
		return
	}
	lr.subsClosed = true
	for ch := range lr.subs {
		delete(lr.subs, ch)
		close(ch)
	}
}

// settle records the terminal state without closing subscriber channels.
//
// State has to become terminal BEFORE the terminator frame goes out. A client
// that reads [DONE] and immediately sends its next turn must not find the
// profile still busy with the turn it just watched finish; that would be a 429
// whose cause is a scheduling gap rather than real concurrency.
func (lr *liveRun) settle(status runs.Status, errMsg string, now time.Time) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.finished {
		return
	}
	lr.run.Status = status
	lr.run.Error = errMsg
	lr.run.UpdatedAt = now.UnixMilli()
	lr.run.FinishedAt = now.UnixMilli()
	lr.run.ExpiresAt = now.Add(runs.DefaultTTL)
	lr.finished = true
}

func (lr *liveRun) cancelled() bool {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.cancelRequested
}

// runManager owns the live runs and the store they are persisted to.
type runManager struct {
	store runs.Store
	logf  func(format string, args ...any)

	mu   sync.Mutex
	live map[string]*liveRun
	// byConversation points at the single in-flight run for a conversation.
	// Pi allows one turn per profile, and a conversation is the unit a client
	// reattaches to, so a second run for the same conversation is a reattach
	// or a conflict, never a queue.
	byConversation map[string]string
}

func newRunManager(store runs.Store, logf func(string, ...any)) *runManager {
	if store == nil {
		store = runs.NewMemoryStore()
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &runManager{store: store, logf: logf, live: map[string]*liveRun{}, byConversation: map[string]string{}}
}

func conversationKey(profile, conversationID string) string {
	return profile + "\x00" + conversationID
}

// begin creates the run record and publishes it to the live map.
//
// Store failure is logged, not fatal: a turn that can still run and be observed
// in this process is better than refusing to answer because the history
// database is down.
func (m *runManager) begin(r runs.Run, agent Agent, completionID string, now time.Time) *liveRun {
	if r.ID == "" {
		r.ID = runs.NewID()
	}
	r.Status = runs.StatusRunning
	r.CreatedAt = now.UnixMilli()
	r.UpdatedAt = now.UnixMilli()
	r.ExpiresAt = now.Add(runs.DefaultTTL)
	lr := newLiveRun(r, agent, completionID)
	lr.promptFull = r.Prompt
	r.Prompt = runs.TrimPrompt(r.Prompt)

	m.mu.Lock()
	m.live[r.ID] = lr
	if r.ConversationID != "" {
		m.byConversation[conversationKey(r.Profile, r.ConversationID)] = r.ID
	}
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.store.Create(ctx, r); err != nil {
		m.logf("runs: create %s: %v (the run continues, unrecorded)", r.ID, err)
	}
	return lr
}

func (m *runManager) byID(id string) (*liveRun, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lr, ok := m.live[id]
	return lr, ok
}

func (m *runManager) activeForConversation(profile, conversationID string) (*liveRun, bool) {
	if conversationID == "" {
		return nil, false
	}
	m.mu.Lock()
	id, ok := m.byConversation[conversationKey(profile, conversationID)]
	m.mu.Unlock()
	if !ok {
		return nil, false
	}
	lr, ok := m.byID(id)
	if !ok || !lr.isRunning() {
		return nil, false
	}
	return lr, true
}

// get returns the live snapshot when the run is executing here, and the stored
// record otherwise.
func (m *runManager) get(ctx context.Context, id string) (runs.Run, error) {
	if lr, ok := m.byID(id); ok {
		return lr.snapshot(), nil
	}
	return m.store.Get(ctx, id)
}

func (m *runManager) list(ctx context.Context, f runs.Filter) ([]runs.Run, error) {
	return m.store.List(ctx, f)
}

// persist writes the current accumulated state. Best-effort: a failed write is
// logged, and the next tick retries with the then-current state, so a short
// outage loses at most one interval of detail rather than the run.
func (m *runManager) persist(lr *liveRun) {
	snap, changed := lr.persistState()
	if !changed {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.store.Save(ctx, snap); err != nil {
		m.logf("runs: persist %s: %v", snap.ID, err)
	}
}

// finish persists the terminal state, removes the run from the live maps, and
// closes done so a buffered caller can answer. Persistence happens BEFORE the
// live entry is dropped, so a client that reattaches in the gap still finds the
// run.
func (m *runManager) finish(lr *liveRun, now time.Time) {
	snap := lr.snapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.store.Save(ctx, snap); err != nil {
		m.logf("runs: final persist %s: %v", snap.ID, err)
	}

	m.mu.Lock()
	delete(m.live, snap.ID)
	if snap.ConversationID != "" {
		key := conversationKey(snap.Profile, snap.ConversationID)
		if m.byConversation[key] == snap.ID {
			delete(m.byConversation, key)
		}
	}
	m.mu.Unlock()

	lr.closeSubs()
	lr.mu.Lock()
	done := lr.done
	lr.mu.Unlock()
	close(done)
}

// jsonPayload marshals one SSE frame with HTML escaping off, matching
// sseWriter.write: model output is full of < and >, and escaping it would make
// snapshots read differently from the deltas they replace.
func jsonPayload(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSuffix(buf.String(), "\n"), nil
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// handleRuns serves /api/runs. Reached with and without a profile segment, like
// /api/jobs: runs are listed globally, with `profile` as an optional filter.
func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request, rest string) {
	path := strings.TrimPrefix(rest, "api/runs")
	path = strings.TrimPrefix(path, "/")

	// Collection route.
	if path == "" {
		if !requireMethod(w, r, http.MethodGet, "/api/runs") {
			return
		}
		f := runs.Filter{
			Profile:        strings.TrimSpace(r.URL.Query().Get("profile")),
			ConversationID: strings.TrimSpace(r.URL.Query().Get("conversation_id")),
		}
		if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
			st := runs.Status(raw)
			if !st.Valid() {
				apiError(w, http.StatusBadRequest, "invalid_request_error", "status",
					"unknown run status "+bounded(raw))
				return
			}
			f.Status = st
		}
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				apiError(w, http.StatusBadRequest, "invalid_request_error", "limit",
					"limit must be a non-negative number")
				return
			}
			f.Limit = n
		}
		list, err := s.runs.list(r.Context(), f)
		if err != nil {
			runsError(w, err)
			return
		}
		out := make([]any, 0, len(list))
		for _, run := range list {
			out = append(out, runWire(run))
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"runs": out})
		return
	}

	id, action, _ := strings.Cut(path, "/")
	if id == "" {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown path "+bounded(r.URL.Path))
		return
	}

	switch action {
	case "events":
		if !requireMethod(w, r, http.MethodGet, "/api/runs/{id}/events") {
			return
		}
		s.handleRunEvents(w, r, id)
		return
	case "steer":
		if !requireMethod(w, r, http.MethodPost, "/api/runs/{id}/steer") {
			return
		}
		var req struct {
			Message string `json:"message"`
		}
		if err := decodeJSONBody(w, r, &req); err != nil {
			return
		}
		msg := strings.TrimSpace(req.Message)
		if msg == "" {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "message",
				"message is required")
			return
		}
		if len(msg) > runs.MaxPrompt {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "message",
				"message is too long")
			return
		}
		lr, ok := s.runs.byID(id)
		if !ok {
			if _, err := s.runs.store.Get(r.Context(), id); err != nil {
				runsError(w, err)
				return
			}
			apiError(w, http.StatusConflict, "invalid_request_error", "",
				"this run is no longer in flight; it cannot be steered")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := lr.steer(ctx, msg); err != nil {
			if !lr.isRunning() {
				apiError(w, http.StatusConflict, "invalid_request_error", "",
					"this run is no longer in flight; it cannot be steered")
				return
			}
			apiError(w, http.StatusBadGateway, "upstream_error", "",
				"could not steer the run: "+bounded(err.Error()))
			return
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "status": "queued"})
		return
	case "cancel":
		if !requireMethod(w, r, http.MethodPost, "/api/runs/{id}/cancel") {
			return
		}
		lr, ok := s.runs.byID(id)
		if !ok {
			run, err := s.runs.store.Get(r.Context(), id)
			if err != nil {
				runsError(w, err)
				return
			}
			writeJSONResponse(w, http.StatusOK, map[string]any{
				"ok": true, "status": string(run.Status),
			})
			return
		}
		if !lr.isRunning() {
			writeJSONResponse(w, http.StatusOK, map[string]any{
				"ok": true, "status": string(lr.snapshot().Status),
			})
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := lr.requestCancel(ctx); err != nil {
			apiError(w, http.StatusBadGateway, "upstream_error", "",
				"could not cancel the run: "+bounded(err.Error()))
			return
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "status": "cancelling"})
		return
	case "":
		if !requireMethod(w, r, http.MethodGet, "/api/runs/{id}") {
			return
		}
		run, err := s.runs.get(r.Context(), id)
		if err != nil {
			runsError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusOK, runWire(run))
		return
	}
	apiError(w, http.StatusNotFound, "invalid_request_error", "",
		"unknown path "+bounded(r.URL.Path))
}

// handleRunEvents streams a run from its current position, live or from the
// store when it has finished.
func (s *Server) handleRunEvents(w http.ResponseWriter, r *http.Request, id string) {
	if lr, ok := s.runs.byID(id); ok {
		s.streamLiveRun(w, r, lr)
		return
	}
	run, err := s.runs.store.Get(r.Context(), id)
	if err != nil {
		runsError(w, err)
		return
	}
	sse := newSSEWriter(w)
	sse.headers()
	w.WriteHeader(http.StatusOK)
	if err := sse.write(runFrame{Run: snapshotOf(run)}); err != nil {
		return
	}
	// Reattaching to a finished run is how the app retrieves output after it
	// was killed: one snapshot, then the same terminator a live turn ends with.
	s.replayTerminal(sse, run)
}

// replayTerminal ends a stream whose run settled before the caller subscribed.
//
// The snapshot already carries the whole output, and a client that understands
// `agento_run` treats it as authoritative. The extra delta is for the clients
// that predate this feature: without it, a turn that finishes in the window
// between the prompt and the subscribe would answer an old client with an empty
// stream, which is a regression no error message would reveal.
//
// A new client must ignore `choices` deltas once the snapshot says the run is
// terminal — the snapshot is the complete answer, the replay is a courtesy.
func (s *Server) replayTerminal(sse *sseWriter, run runs.Run) {
	if run.Output != "" || run.Reasoning != "" {
		_ = sse.write(newChunk(completionIDFor(run.ConversationID), run.Model,
			run.Output, run.Reasoning))
	}
	switch run.Status {
	case runs.StatusFailed, runs.StatusInterrupted:
		_ = sse.write(errorFrame{Error: run.Error})
		return
	}
	if run.Status == runs.StatusCompleted && run.Usage.NonZero() {
		_ = sse.write(chunk{
			ID:      completionIDFor(run.ConversationID),
			Object:  "chat.completion.chunk",
			Created: nowUnix(),
			Model:   run.Model,
			Usage:   usageWire(run.Usage),
		})
	}
	_ = sse.done()
}

// streamLiveRun subscribes first, then snapshots, then relays. That order is
// what closes the gap between "what has already happened" and "what happens
// next": every event published after the snapshot has a higher Seq and is
// relayed, and every event that landed before it is already inside the
// snapshot and is dropped.
func (s *Server) streamLiveRun(w http.ResponseWriter, r *http.Request, lr *liveRun) {
	events, unsubscribe := lr.subscribe()
	defer unsubscribe()
	s.streamRun(w, r, lr, events, lr.snapshot())
}

// streamRun relays a run to one client. The subscription and the snapshot are
// supplied by the caller so the POST path can take both BEFORE the executor
// starts: a turn that finishes in microseconds then still streams, and does not
// collapse into a single terminal snapshot.
func (s *Server) streamRun(
	w http.ResponseWriter,
	r *http.Request,
	lr *liveRun,
	events <-chan runEvent,
	snap runs.Run,
) {
	sse := newSSEWriter(w)
	sse.headers()
	w.WriteHeader(http.StatusOK)
	if err := sse.write(runFrame{Run: snapshotOf(snap)}); err != nil {
		return
	}

	if snap.Status.Terminal() {
		// It finished while the client was connecting or reconnecting. The
		// snapshot is complete; replay it as a delta for older clients and
		// close the same way the live path does.
		s.replayTerminal(sse, snap)
		return
	}

	keepalive := startKeepalive(r.Context(), sse, s.cfg.KeepaliveInterval)
	defer keepalive()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev, open := <-events:
			if !open {
				return
			}
			if ev.Seq <= snap.Seq {
				continue
			}
			if ev.Done {
				_ = sse.done()
				return
			}
			if err := sse.emit("data: " + ev.Data + "\n"); err != nil {
				return
			}
		}
	}
}

// respondBuffered waits for the run to finish and answers with one JSON body.
// The wait is on the run, not on the request: a client that disconnects does
// not cancel the executor, so the work still completes and remains retrievable
// through /api/runs.
func (s *Server) respondBuffered(w http.ResponseWriter, r *http.Request, lr *liveRun) {
	select {
	case <-lr.done:
	case <-r.Context().Done():
		return
	}
	snap := lr.snapshot()
	switch snap.Status {
	case runs.StatusCompleted:
	case runs.StatusFailed:
		apiError(w, http.StatusBadGateway, "upstream_error", "", snap.Error)
		return
	case runs.StatusCancelled:
		apiError(w, http.StatusConflict, "invalid_request_error", "",
			"the run was cancelled")
		return
	default:
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"the run ended without finishing")
		return
	}
	body := completion{
		ID:      lr.completionID,
		Object:  "chat.completion",
		Created: nowUnix(),
		Model:   snap.Model,
		Choices: []completionChoice{{
			Message: delta{
				Content:          snap.Output,
				ReasoningContent: snap.Reasoning,
			},
			Finish: "stop",
		}},
	}
	if snap.Usage.NonZero() {
		body.Usage = usageWire(snap.Usage)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = writeJSON(w, body)
}

// runWire is the run list/detail shape. It reuses the chat stream's usage
// spelling so the app parses both with one helper.
func runWire(r runs.Run) map[string]any {
	out := map[string]any{
		"id":          r.ID,
		"profile":     r.Profile,
		"status":      string(r.Status),
		"output":      r.Output,
		"error":       r.Error,
		"seq":         r.Seq,
		"created_at":  r.CreatedAt,
		"updated_at":  r.UpdatedAt,
		"finished_at": r.FinishedAt,
	}
	if r.ConversationID != "" {
		out["conversation_id"] = r.ConversationID
	}
	if r.Prompt != "" {
		out["prompt"] = r.Prompt
	}
	if r.Provider != "" {
		out["provider"] = r.Provider
	}
	if r.Model != "" {
		out["model"] = r.Model
	}
	if r.Effort != "" {
		out["effort"] = r.Effort
	}
	if r.Reasoning != "" {
		out["reasoning"] = r.Reasoning
	}
	if len(r.Tools) > 0 {
		out["tools"] = r.Tools
	} else {
		out["tools"] = []string{}
	}
	if r.Usage.NonZero() {
		out["usage"] = map[string]any{
			"prompt_tokens":      r.Usage.Prompt,
			"completion_tokens":  r.Usage.Completion,
			"total_tokens":       r.Usage.Total,
			"cache_read_tokens":  r.Usage.Cached,
			"cache_write_tokens": r.Usage.CacheWrite,
		}
	}
	return out
}

func runsError(w http.ResponseWriter, err error) {
	if se, ok := err.(*runs.StoreError); ok {
		switch {
		case se.NotFound:
			apiError(w, http.StatusNotFound, "invalid_request_error", "", bounded(se.Msg))
		case se.Upstream:
			apiError(w, http.StatusBadGateway, "upstream_error", "", bounded(se.Msg))
		default:
			apiError(w, http.StatusBadRequest, "invalid_request_error", "", bounded(se.Msg))
		}
		return
	}
	apiError(w, http.StatusBadRequest, "invalid_request_error", "", bounded(err.Error()))
}

// executeRun owns the turn, the profile lock and the run lifecycle.
func (s *Server) executeRun(
	lr *liveRun,
	h *agentHandler,
	ctx context.Context,
	cancel context.CancelFunc,
	events <-chan pi.Record,
	unsubscribe func(),
	userTurn *turn,
	req chatRequest,
	revert func(),
) {
	// The profile lock is the scarce resource, not the stream. Release it as
	// soon as the turn has stopped talking to Pi, before the terminator frame
	// reaches the client: a client that reads [DONE] and immediately sends the
	// next turn must not be answered with a 429 for a turn it just watched
	// finish. sync.OnceFunc keeps the deferred release from double-unlocking.
	unlock := sync.OnceFunc(h.turnMu.Unlock)

	defer func() {
		// A panic in the turn loop must not leave the profile locked forever:
		// that would make every later turn 429 until the process restarts.
		if rec := recover(); rec != nil {
			s.cfg.Logf("runs: panic in run %s: %v", lr.id(), rec)
			lr.settle(runs.StatusFailed,
				"the gateway failed while running this turn", time.Now())
			unlock()
			lr.publish(errorFrame{Error: "the gateway failed while running this turn"}, false)
		}
		unsubscribe()
		cancel()
		unlock()
		s.runs.finish(lr, time.Now())
	}()

	ticker := time.NewTicker(runPersistInterval)
	defer ticker.Stop()

	if err := h.runner.agent.Send(ctx, "prompt", promptPayload(userTurn)); err != nil {
		revert()
		const msg = "could not deliver the prompt to the agent"
		lr.settle(runs.StatusFailed, msg, time.Now())
		unlock()
		lr.publish(errorFrame{Error: msg}, false)
		return
	}

	for {
		select {
		case <-ctx.Done():
			if lr.cancelled() {
				s.finishCancelled(lr, unlock)
			} else {
				const msg = "the agent did not finish this turn in time"
				lr.settle(runs.StatusFailed, msg, time.Now())
				unlock()
				lr.publish(errorFrame{Error: msg}, false)
			}
			return
		case <-ticker.C:
			s.runs.persist(lr)

		case rec, open := <-events:
			if !open {
				const msg = "the agent stopped before finishing this turn"
				lr.settle(runs.StatusFailed, msg, time.Now())
				unlock()
				lr.publish(errorFrame{Error: msg}, false)
				return
			}
			if u, ok := decodeUsage(rec); ok {
				lr.setUsage(u)
			}
			if msg, failed := decodeTurnError(rec); failed {
				if lr.cancelled() {
					s.finishCancelled(lr, unlock)
				} else {
					lr.settle(runs.StatusFailed, msg, time.Now())
					unlock()
					lr.publish(errorFrame{Error: msg}, false)
				}
				return
			}

			switch rec.Type {
			case "message_update":
				text, reasoning, isDelta, ok := decodeMessageUpdate(rec)
				if !ok || !isDelta || (text == "" && reasoning == "") {
					continue
				}
				lr.publishDelta(text, reasoning, newChunk(lr.completionID, req.Model, text, reasoning))

			default:
				name, named := decodeToolEvent(rec)
				if !named {
					break
				}
				status := toolStatus(rec.Type)
				if !isToolEvent(rec.Type) {
					status = "running"
				}
				lr.publishTool(name, toolFrame{Tool: name, Status: status})

			case pi.TypeAgentSettled:
				if lr.cancelled() {
					s.finishCancelled(lr, unlock)
					return
				}
				lr.settle(runs.StatusCompleted, "", time.Now())
				unlock()
				if u := lr.snapshot().Usage; u.NonZero() {
					lr.publish(chunk{
						ID:      lr.completionID,
						Object:  "chat.completion.chunk",
						Created: nowUnix(),
						Model:   req.Model,
						Usage:   usageWire(u),
					}, false)
				}
				lr.publish(nil, true)
				return
			}
		}
	}
}

// finishCancelled emits the explicit status frame before the terminator, so a
// client watching from a second device can tell a deliberate stop from a
// normal completion. The app uses it to mark the reply interrupted-by-user
// rather than completed.
func (s *Server) finishCancelled(lr *liveRun, unlock func()) {
	lr.settle(runs.StatusCancelled, "", time.Now())
	unlock()
	lr.publish(runFrame{Run: runSnapshot{
		ID:     lr.id(),
		Status: string(runs.StatusCancelled),
	}}, false)
	lr.publish(nil, true)
}
