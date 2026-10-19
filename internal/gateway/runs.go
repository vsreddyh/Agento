package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
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

// maxRunIDLen bounds the {id} path segment. Run ids are 24-hex ObjectIDs, so
// anything longer matches nothing; the bound keeps a garbage segment out of
// the store query.
const maxRunIDLen = 128

// runFrame is the snapshot frame. It is the only new wire shape this feature
// adds; everything after it is the existing chat SSE vocabulary.
type runFrame struct {
	Run runSnapshot `json:"agento_run"`
}

type runSnapshot struct {
	ID string `json:"id"`
	// Profile and conversation travel with every snapshot so a client handed
	// a run ID from another conversation (the cross-conversation 429) can
	// correlate what it attached to without a second round trip. Prompt does
	// not: the snapshot is progress, and the preview lives in the record.
	Profile        string `json:"profile,omitempty"`
	ConversationID string `json:"conversation_id,omitempty"`
	Status         string `json:"status"`
	Seq            int64  `json:"seq"`
	Model          string `json:"model,omitempty"`
	Output         string `json:"output,omitempty"`
	Reasoning      string `json:"reasoning,omitempty"`
	// Tools is always present, even when empty, matching runWire: the app
	// parses the snapshot and the list/detail shapes with one helper, and an
	// omitted-vs-empty difference would make that helper branch for nothing.
	Tools      []string `json:"tools"`
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
		ID:             r.ID,
		Profile:        r.Profile,
		ConversationID: r.ConversationID,
		Status:         string(r.Status),
		Seq:            r.Seq,
		Model:          r.Model,
		Output:         r.Output,
		Reasoning:      r.Reasoning,
		Tools:          append([]string(nil), r.Tools...),
		Error:          r.Error,
		CreatedAt:      r.CreatedAt,
		UpdatedAt:      r.UpdatedAt,
		FinishedAt:     r.FinishedAt,
	}
	if r.Usage.NonZero() {
		s.Usage = usageWire(r.Usage)
	}
	if s.Tools == nil {
		// append(nil) stays nil and encodes as `null`, not `[]`. The field
		// is always present by contract (matching runWire), so normalize.
		s.Tools = []string{}
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
	// imagesSig identifies the turn's image set for the reattach comparison.
	// Text equality alone would attach a new turn carrying different images to
	// the old one, showing progress for work that is not running.
	imagesSig string

	// persistCh carries snapshots to the background persist loop, and
	// persistDone closes when that loop has drained. The executor offers each
	// tick's snapshot without blocking (a full channel means the store is
	// slower than the turn, and dropping one interval of detail beats stalling
	// the live stream); finish closes the channel and waits, so the final
	// synchronous write is strictly newer than anything the loop wrote.
	persistCh   chan runs.Run
	persistDone chan struct{}

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
		persistCh:    make(chan runs.Run, 1),
		persistDone:  make(chan struct{}),
	}
}

// imagesSigOf hashes a turn's image set for the reattach comparison. Lengths
// and MIME types are hashed alongside the bytes: same pixels re-encoded under
// a different type are not the same turn for the agent.
func imagesSigOf(images []piImage) string {
	if len(images) == 0 {
		return ""
	}
	h := sha256.New()
	for _, img := range images {
		fmt.Fprintf(h, "%s\x00%d\x00", img.MimeType, len(img.Data))
		h.Write([]byte(img.Data))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

func (lr *liveRun) id() string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.run.ID
}

// profile reports the owning profile without copying the run: the 429 path
// calls it for every live run, and a full snapshot would copy megabytes of
// output to compare one string.
func (lr *liveRun) profile() string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.run.Profile
}

// status reports the lifecycle state without copying the run, for cheap
// pre-filtering before a full snapshot (see list overlay).
func (lr *liveRun) status() runs.Status {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.run.Status
}

// conversation reports the conversation id without copying the run.
func (lr *liveRun) conversation() string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.run.ConversationID
}

// matchesFilter reports whether the run can match f, without copying it.
// Used to reserve overlay room and to pre-filter appends before a full
// snapshot; the final f.Matches on merged rows stays authoritative.
func (lr *liveRun) matchesFilter(f runs.Filter) bool {
	return (f.Profile == "" || lr.profile() == f.Profile) &&
		(f.ConversationID == "" || lr.conversation() == f.ConversationID) &&
		(f.Status == "" || lr.status() == f.Status)
}
func (lr *liveRun) prompt() string {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.promptFull != "" {
		return lr.promptFull
	}
	return lr.run.Prompt
}

// reattachKey returns the prompt text and image signature for the reattach
// comparison in one locked read. Both fields are immutable after begin, so
// separate reads would be harmless — one accessor keeps the pair consistent
// and the locking obvious.
func (lr *liveRun) reattachKey() (prompt, images string) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.promptLocked(), lr.imagesSig
}

func (lr *liveRun) promptLocked() string {
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

// persistState returns the current state, its sequence, and whether anything
// changed since the last acknowledged write. A run parked in a long tool call
// would otherwise rewrite an unchanging document once a second.
//
// Split from the acknowledgement on purpose: the persist loop marks a
// sequence saved only after its store write lands. Marking on offer would let
// a failed Save retire a snapshot that never reached the store, and the next
// tick would report "unchanged" and never retry it.
func (lr *liveRun) persistState() (runs.Run, int64, bool) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if lr.seq == lr.savedSeq {
		return runs.Run{}, 0, false
	}
	return lr.snapshotLocked(), lr.seq, true
}

// markSaved records that seq reached the store (or the persist queue). Only
// advances: a stale acknowledgement arriving after a newer snapshot can not
// rewind the watermark.
func (lr *liveRun) markSaved(seq int64) {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if seq > lr.savedSeq {
		lr.savedSeq = seq
	}
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
		// Read under the lock like every other seq access: the field is
		// written under lock on every publish, and a bare read here is the
		// exact shape -race flags.
		lr.mu.Lock()
		defer lr.mu.Unlock()
		return lr.seq
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.seq++
	// Progress moved now: a client polling the list sees frames flowing, so
	// updated_at grows with them rather than freezing at begin time.
	lr.run.UpdatedAt = time.Now().UnixMilli()
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
		lr.mu.Lock()
		defer lr.mu.Unlock()
		return lr.seq
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	// Progress moved now: a client polling the list sees output growing, so
	// updated_at must grow with it rather than freezing at begin time.
	lr.run.UpdatedAt = time.Now().UnixMilli()
	// Output is capped live like reasoning, partial-fill rather than dropped
	// at the boundary — but note what the cap means for a reattaching client:
	// past MaxOutput the snapshot carries the head while live deltas keep
	// flowing in full, so the middle is live-only. That matches the stored
	// record, which caps at the same bound: anything past it was never going
	// to be kept. Without this a runaway tool loop holds megabytes in the
	// builder for the whole turn and serves them from GET /api/runs/{id}.
	if remaining := runs.MaxOutput - lr.out.Len(); remaining > 0 {
		lr.out.WriteString(truncateBytes(text, remaining))
	}
	// The live copy is capped like the stored one, but partially rather than
	// by dropping the whole chunk at the boundary: a long reasoning trace
	// still shows its head in the snapshot instead of stopping mid-turn.
	if remaining := runs.MaxReasoning - lr.reason.Len(); remaining > 0 {
		lr.reason.WriteString(truncateBytes(reasoning, remaining))
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
		lr.mu.Lock()
		defer lr.mu.Unlock()
		return lr.seq
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	lr.run.UpdatedAt = time.Now().UnixMilli()
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
	lr.run.UpdatedAt = time.Now().UnixMilli()
	// Usage arrives without a frame of its own, but it still has to reach the
	// store: persistState keys off seq, so an accounting-only tick would
	// otherwise never persist until finish. Bumping seq without publishing is
	// safe — subscribers filter by seq and no event carries this one.
	lr.seq++
}

// peekNextSeq returns the seq the next publish will assign. Only the
// executor publishes, so the peeked value is exact — no other goroutine can
// slip a publish in between. Used to stamp a snapshot with its own delivery
// seq (see finishCancelled).
func (lr *liveRun) peekNextSeq() int64 {
	lr.mu.Lock()
	defer lr.mu.Unlock()
	return lr.seq + 1
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
//
// Lock order: m.mu and lr.mu are never held together. Code that needs both
// takes m.mu, releases it, then takes lr.mu (see activeForProfile, finish).
// Holding both at once would work today and deadlock the day someone reverses
// the order — do not "fix" the separate acquisitions into one critical
// section.
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
	// NUL cannot appear in either half in practice (HTTP header values never
	// carry it), so it cannot collide with content. Do not reuse this for
	// arbitrary strings without the same guarantee.
	return profile + "\x00" + conversationID
}

// begin creates the run record and publishes it to the live map.
//
// The live registration is synchronous — a client reattaching a millisecond
// later must find it — but the store Create runs in the background. The caller
// holds the profile's turnMu here, and a slow database must not hold the
// profile's only turn slot before the first frame. Save upserts, so a Create
// that lands after a persist still converges; a duplicate-key failure from
// that ordering is logged and harmless.
func (m *runManager) begin(r runs.Run, agent Agent, completionID, imagesSig string, now time.Time) *liveRun {
	if r.ID == "" {
		r.ID = runs.NewID()
	}
	r.Status = runs.StatusRunning
	r.CreatedAt = now.UnixMilli()
	r.UpdatedAt = now.UnixMilli()
	r.ExpiresAt = now.Add(runs.DefaultTTL)
	// Trim before the live copy is made: promptFull keeps the full text for
	// the reattach comparison, but lr.run.Prompt is what the live overlay in
	// get/list exposes until finish saves — leaving it full would serve an up
	// to 4 MiB prompt from every snapshot in the meantime.
	promptFull := r.Prompt
	r.Prompt = runs.TrimPrompt(r.Prompt)
	lr := newLiveRun(r, agent, completionID)
	lr.promptFull = promptFull
	lr.imagesSig = imagesSig

	m.mu.Lock()
	m.live[r.ID] = lr
	if r.ConversationID != "" {
		m.byConversation[conversationKey(r.Profile, r.ConversationID)] = r.ID
	}
	m.mu.Unlock()

	go m.persistLoop(lr)

	create := r
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.store.Create(ctx, create); err != nil {
			m.logf("runs: create %s: %v (the run continues, unrecorded)", create.ID, err)
		}
	}()
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
	if !ok {
		return nil, false
	}
	if !lr.isRunning() {
		return nil, false
	}
	return lr, true
}

// activeForProfile returns the profile's in-flight run for any conversation,
// or false when the profile is idle. Used for the 429: a client on another
// conversation gets a bare refusal otherwise, with no run to reattach to or
// cancel.
func (m *runManager) activeForProfile(profile string) (*liveRun, bool) {
	m.mu.Lock()
	lives := make([]*liveRun, 0, len(m.live))
	for _, lr := range m.live {
		lives = append(lives, lr)
	}
	m.mu.Unlock()
	for _, lr := range lives {
		if lr.profile() != profile {
			continue
		}
		if lr.isRunning() {
			return lr, true
		}
	}
	return nil, false
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
	// Reserve room for the overlay below instead of over-fetching: the store
	// window applies before the merge, so fetching a full page of stored rows
	// would slice a live run straight back out. The merged page holds at most
	// `want` rows with live runs prioritized over the oldest stored ones;
	// past ListCap the window still wins (a hundred newer runs during one
	// turn), and then detail/events serve what the list cannot.
	m.mu.Lock()
	lives := make([]*liveRun, 0, len(m.live))
	for _, lr := range m.live {
		lives = append(lives, lr)
	}
	m.mu.Unlock()
	// Reserve room only for live runs the filter can match: every reserved
	// row displaces a stored one, and runs for other profiles/statuses would
	// shrink the page for rows the overlay would never add.
	room := 0
	for _, lr := range lives {
		if lr.matchesFilter(f) {
			room++
		}
	}
	want := f.Limit
	if want <= 0 || want > runs.ListCap {
		want = runs.ListCap
	}
	var stored []runs.Run
	if fetch := want - room; fetch > 0 {
		f.Limit = fetch
		var err error
		stored, err = m.store.List(ctx, f)
		if err != nil {
			return nil, err
		}
	}
	// Overlay the live runs. The store lags the executor by design now — the
	// Create is background and persists are offered without blocking — so a
	// list taken mid-turn must show the live state, not the last write. A live
	// snapshot replaces its stored row, or joins the list when the Create has
	// not landed yet.
	byID := make(map[string]int, len(stored)+len(lives))
	for i, r := range stored {
		byID[r.ID] = i
	}
	for _, lr := range lives {
		// Refresh stored rows unconditionally, even when the live snapshot no
		// longer matches the filter: a stored row can be staler than the live
		// run (a Create that landed while the terminal Save is still in
		// flight), and skipping the refresh serves the stale row. The final
		// filter below decides what matches. Snapshots copy up to 256 KiB, so
		// they are taken only when either side can match: fresh data for an
		// included row, or a refresh that drops a stale one. When both the
		// stored row and the live state exclude, the row stays excluded
		// without paying for the copy.
		i, inStore := byID[lr.id()]
		liveMatch := lr.matchesFilter(f)
		if inStore {
			if liveMatch || f.Matches(stored[i]) {
				stored[i] = lr.snapshot()
			}
			continue
		}
		if !liveMatch {
			continue
		}
		snap := lr.snapshot()
		if !f.Matches(snap) {
			continue
		}
		byID[snap.ID] = len(stored)
		stored = append(stored, snap)
	}
	sort.Slice(stored, func(i, j int) bool {
		if stored[i].CreatedAt == stored[j].CreatedAt {
			return stored[i].ID > stored[j].ID
		}
		return stored[i].CreatedAt > stored[j].CreatedAt
	})
	// Re-apply the filter to the merged list. A stored row can be refreshed
	// by a newer live snapshot that no longer matches (a run that settled
	// after the store filtered it running), and without this the settled run
	// leaks into a ?status=running list.
	kept := stored[:0]
	for _, r := range stored {
		if f.Matches(r) {
			kept = append(kept, r)
		}
	}
	stored = kept
	limit := want
	if len(stored) > limit {
		stored = stored[:limit]
	}
	return stored, nil
}

// offerPersist queues one snapshot for the background persist loop without
// blocking. A full channel means the store is slower than the turn; the tick
// is dropped and the next one retries, so the live stream never stalls on the
// database.
func (m *runManager) offerPersist(lr *liveRun) {
	snap, _, changed := lr.persistState()
	if !changed {
		return
	}
	select {
	case lr.persistCh <- snap:
	default:
		// The store is slower than the turn; the tick is dropped and the
		// next one retries with the then-current state, so the live stream
		// never stalls on the database.
	}
}

// persistLoop writes offered snapshots until the run finishes. It marks the
// watermark only after a successful store write — never on offer — so a
// failed Save leaves the tick unacknowledged and the next one retries with
// the then-current cumulative snapshot. It is the only writer besides the
// final save in finish, and finish waits for it to drain, so the terminal
// write is strictly the newest.
func (m *runManager) persistLoop(lr *liveRun) {
	defer close(lr.persistDone)
	// Consecutive store failures keep one counter so degraded mode (every
	// tick dropped, converging only at finish) trips the same ERROR grep as
	// a terminal failure instead of passing as routine info.
	drops := 0
	for snap := range lr.persistCh {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := m.store.Save(ctx, snap); err != nil {
			drops++
			m.logf("runs: ERROR persist %s failed %d in a row: %v", snap.ID, drops, err)
		} else {
			drops = 0
			lr.markSaved(snap.Seq)
		}
		cancel()
	}
}

// saveRun writes one snapshot with a single retry on a fresh context. Used
// for the terminal write, which must land or the row lies until restart.
func (m *runManager) saveRun(snap runs.Run) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err := m.store.Save(ctx, snap)
	cancel()
	if err == nil {
		return nil
	}
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.store.Save(ctx, snap)
}

// finish persists the terminal state, removes the run from the live maps, and
// closes done so a buffered caller can answer. The persist loop is drained
// first, so the terminal write is the newest one; persistence still happens
// BEFORE the live entry is dropped, so a client that reattaches in the gap
// still finds the run.
func (m *runManager) finish(lr *liveRun, now time.Time) {
	close(lr.persistCh)
	<-lr.persistDone
	snap := lr.snapshot()
	// The terminal write must land: without it the stored row stays running
	// forever (only the next restart would correct it via MarkInterrupted).
	// One retry on a fresh context, then a loud log — teardown is bounded,
	// and the subscribers already have their terminator either way.
	if err := m.saveRun(snap); err != nil {
		m.logf("runs: ERROR final persist %s failed twice, record stays stale until restart: %v", snap.ID, err)
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
	// Single caller by construction (the executeRun defer): a second closer
	// would panic, so any future caller must take over — not add one.
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

// storeCtx bounds a store read so a wedged database stalls the route for
// seconds, not until the client goes away. Callers map a timeout to 502.
func storeCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 5*time.Second)
}

// storeTimeout reports whether err is the bound above firing.
func storeTimeout(ctx context.Context, err error) bool {
	return err != nil && ctx.Err() == context.DeadlineExceeded
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// truncateBytes bounds s to at most limit bytes without cutting a UTF-8 rune
// in half. Unlike runs.CapText it adds no marker: it trims a live buffer that
// is still growing, not a stored value a reader might mistake for complete.
// A non-positive limit means nothing fits, so it yields "".
func truncateBytes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut]
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
		// The filters go straight into the Mongo query: bound them to the same
		// shared client-id bound so a megabyte of query string cannot become
		// a full equality scan. Each names its own field — trimming profile
		// for a conversation_id overflow would send the app the wrong way.
		if len(f.Profile) > maxFilterLen {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "profile",
				"profile filter must be at most 256 characters")
			return
		}
		if len(f.ConversationID) > maxFilterLen {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "conversation_id",
				"conversation_id filter must be at most 256 characters")
			return
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
		// An empty or absent limit means the default (ListCap); only a
		// present non-numeric or negative value is malformed.
		listCtx, listCancel := storeCtx(r)
		list, err := s.runs.list(listCtx, f)
		listCancel()
		if err != nil {
			if storeTimeout(listCtx, err) {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the run store did not answer in time")
				return
			}
			runsError(w, err)
			return
		}
		out := make([]any, 0, len(list))
		for _, run := range list {
			out = append(out, runListWire(run))
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
	// Run ids are 24-hex ObjectIDs; a longer segment matches nothing. Refuse
	// it before the store round trip rather than querying Mongo for megabytes
	// of path. Intentionally 404, not 400 like the overlong list filters: an
	// {id} names a thing that does not exist (no oracle — the response is the
	// same as an unknown well-formed id), while a filter is a malformed
	// request.
	if len(id) > maxRunIDLen {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown run")
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
		if len([]rune(msg)) > runs.MaxSteer {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "message",
				"message is too long (at most "+strconv.Itoa(runs.MaxSteer)+" characters)")
			return
		}
		lr, ok := s.runs.byID(id)
		if !ok {
			// Bounded: a slow store must not stall the verb until the client
			// goes away. A timeout here is an upstream failure, not a 400.
			getCtx, getCancel := storeCtx(r)
			_, err := s.runs.store.Get(getCtx, id)
			getCancel()
			if err != nil {
				if storeTimeout(getCtx, err) {
					apiError(w, http.StatusBadGateway, "upstream_error", "",
						"the run store did not answer in time")
					return
				}
				runsError(w, err)
				return
			}
			apiError(w, http.StatusConflict, "invalid_request_error", "",
				"this run is no longer in flight; it cannot be steered")
			return
		}
		// Detached like the turn context itself: a client that steers and
		// drops must not cancel the steer RPC — the instruction belongs to
		// the run, not the connection.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
		// The run can settle between the Call and this read: Pi accepted the
		// instruction but the turn is over, so it steered nothing. Report the
		// race instead of a false success — the client resends as a new turn,
		// and a duplicated instruction in the worst case is benign.
		if !lr.isRunning() {
			s.cfg.Logf("runs: steer landed as run %s settled", lr.id())
			apiError(w, http.StatusConflict, "invalid_request_error", "",
				"this run is no longer in flight; it cannot be steered")
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
			// Same bound as steer: distinguish finished from unknown without
			// letting a slow store stall the verb.
			getCtx, getCancel := storeCtx(r)
			run, err := s.runs.store.Get(getCtx, id)
			getCancel()
			if err != nil {
				if storeTimeout(getCtx, err) {
					apiError(w, http.StatusBadGateway, "upstream_error", "",
						"the run store did not answer in time")
					return
				}
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
		// Detached like the turn context itself: a client that cancels and
		// drops would otherwise abort the abort — requestCancel resets its
		// flag when the Send fails, and the turn would run on.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := lr.requestCancel(ctx); err != nil {
			// The turn can settle between the isRunning check above and the
			// abort: that is a race-free cancel of a finished run, not a
			// failure — answer its terminal status like the settled path.
			if snap := lr.snapshot(); snap.Status.Terminal() {
				writeJSONResponse(w, http.StatusOK, map[string]any{
					"ok": true, "status": string(snap.Status),
				})
				return
			}
			apiError(w, http.StatusBadGateway, "upstream_error", "",
				"could not cancel the run: "+bounded(err.Error()))
			return
		}
		// Mirror of the steer re-check: abort accepted just as the turn
		// settled aborts nothing (a no-op on idle, unlike a steer which
		// would inject into the next turn). Answer the terminal status so
		// the client does not poll a finished run as "cancelling".
		if snap := lr.snapshot(); snap.Status.Terminal() {
			s.cfg.Logf("runs: cancel landed as run %s settled (%s)", lr.id(), snap.Status)
			writeJSONResponse(w, http.StatusOK, map[string]any{
				"ok": true, "status": string(snap.Status),
			})
		} else {
			writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "status": "cancelling"})
		}
		return
	case "":
		if !requireMethod(w, r, http.MethodGet, "/api/runs/{id}") {
			return
		}
		getCtx, getCancel := storeCtx(r)
		run, err := s.runs.get(getCtx, id)
		getCancel()
		if err != nil {
			if storeTimeout(getCtx, err) {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the run store did not answer in time")
				return
			}
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
		// Every entry to a run names it, like both chat paths do: the client
		// has the ID from the URL today, but the invariant lets the app rely
		// on the header instead of the URL it built.
		w.Header().Set("X-Agento-Run-Id", id)
		s.streamLiveRun(w, r, lr)
		return
	}
	evCtx, evCancel := storeCtx(r)
	run, err := s.runs.store.Get(evCtx, id)
	evCancel()
	if err != nil {
		if storeTimeout(evCtx, err) {
			apiError(w, http.StatusBadGateway, "upstream_error", "",
				"the run store did not answer in time")
			return
		}
		runsError(w, err)
		return
	}
	w.Header().Set("X-Agento-Run-Id", id)
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
	// Usage rides along for every terminal state that has any, not just
	// completions: a failed or cancelled turn still spent tokens, and an old
	// client that only reads chat chunks would otherwise lose the accounting
	// the snapshot carries.
	if run.Usage.NonZero() {
		_ = sse.write(chunk{
			ID:      completionIDFor(run.ConversationID),
			Object:  "chat.completion.chunk",
			Created: nowUnix(),
			Model:   run.Model,
			Usage:   usageWire(run.Usage),
		})
	}
	switch run.Status {
	case runs.StatusFailed, runs.StatusInterrupted:
		_ = sse.write(errorFrame{Error: run.Error})
		return
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
//
// Holding the connection this long is safe for the same reason the old
// buffered handler was: the timeout chain is ordered outermost-first —
// whatever nginx's read timeout, PI_TURN_TIMEOUT and the delegation ceiling
// are configured to (deployed: 990s idle, 960s, 900s+30s; see
// docker/docker-compose.yml PI_TURN_TIMEOUT and docker/proxy/nginx.conf
// proxy_read_timeout), the layer that fires is the innermost one with an
// answer, never a bare proxy 504.
// Worst case is TurnTimeout plus the post-settle bound (5s persist drain, 10s
// terminal retry ≈ 15s): still inside nginx as deployed, but only just —
// widening either post-settle budget or PI_TURN_TIMEOUT without widening the
// proxy inverts the chain.
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
		// Param run_status (not a message match) tells "user stopped it"
		// apart from "gateway restarted" below. The app branches on status /
		// param; both messages are runs.InterruptedError / the cancel string,
		// documented stable in docs/adr/0001-resumable-runs.md.
		apiError(w, http.StatusConflict, "invalid_request_error", "run_status",
			"the run was cancelled")
		return
	case runs.StatusInterrupted:
		// A restart is not an upstream failure: the agent did nothing wrong,
		// and the client should not retry as if Pi had failed. 409 names the
		// cause so the app reports it instead of retrying it.
		apiError(w, http.StatusConflict, "invalid_request_error", "run_status",
			runs.InterruptedError)
		return
	default:
		// Defensive: finish runs only after settle, so snap is always
		// terminal once done closes. If this ever fires, the run ended
		// without settling — a bug, reported as upstream failure.
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

// runListWire is the run list shape. Same fields as the detail, but output
// and reasoning are previews: a full list at ListCap with 256 KiB turns would
// be tens of megabytes in one response, and the list shows what a turn was
// about, not a second copy of it. The detail route keeps the whole text.
func runListWire(r runs.Run) map[string]any {
	r.Output = runs.CapText(r.Output, runs.ListPreviewOutput)
	r.Reasoning = runs.CapText(r.Reasoning, runs.ListPreviewReasoning)
	return runWire(r)
}

// runWire is the run detail shape. It reuses the chat stream's usage
// spelling so the app parses both with one helper. Always present: id,
// profile, status, output, error, seq, created_at, updated_at, finished_at
// (0 means "not finished"), tools (possibly []). Present only when set:
// conversation_id, prompt, provider, model, effort, reasoning, usage. The
// snapshot frame omits empty output/reasoning via omitempty (absent means
// "nothing yet"); a parser must tolerate both spellings.
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

	// promptSent tracks whether the turn reached the agent. The panic recovery
	// below reverts the sticky model switch only when it did not: every other
	// pre-prompt failure path reverts because the turn never executed, but a
	// turn that ran (even partially) under the new model keeps it — reverting
	// that would unswitch a session the agent already moved.
	promptSent := false

	defer func() {
		// A panic in the turn loop must not leave the profile locked forever:
		// that would make every later turn 429 until the process restarts.
		if rec := recover(); rec != nil {
			s.cfg.Logf("runs: panic in run %s: %v", lr.id(), rec)
			if !promptSent {
				revert()
			}
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
	promptSent = true

	for {
		select {
		case <-ctx.Done():
			// Settled-first: select picks randomly when the deadline and a
			// queued agent_settled are both ready, which would mark a
			// completed turn failed. Drain whatever the agent already queued
			// — without blocking — before declaring the timeout, so only a
			// settle that arrives after the drain loses the race (a residual
			// millisecond window, loud as a timeout, not silent).
		drain:
			for {
				select {
				case rec, open := <-events:
					if !open {
						const msg = "the agent stopped before finishing this turn"
						lr.settle(runs.StatusFailed, msg, time.Now())
						unlock()
						lr.publish(errorFrame{Error: msg}, false)
						return
					}
					if s.stepRun(lr, req, rec, unlock) {
						return
					}
				default:
					break drain
				}
			}
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
			s.runs.offerPersist(lr)

		case rec, open := <-events:
			if !open {
				const msg = "the agent stopped before finishing this turn"
				lr.settle(runs.StatusFailed, msg, time.Now())
				unlock()
				lr.publish(errorFrame{Error: msg}, false)
				return
			}
			if s.stepRun(lr, req, rec, unlock) {
				return
			}
		}
	}
}

// stepRun folds one agent record into the run and reports whether the turn
// ended. Shared by the event loop and the timeout drain, so a settle queued
// beside the deadline is completed, not failed.
func (s *Server) stepRun(lr *liveRun, req chatRequest, rec pi.Record, unlock func()) bool {
	// Usage is cumulative and reported on message_update today, but it is
	// decoded for EVERY record rather than inside one case: Pi also nests it
	// under other events (compaction_end carries result.usage), and a record
	// whose final counts were never read would silently report zero tokens
	// for a completed turn.
	if u, ok := decodeUsage(rec); ok {
		lr.setUsage(u)
	}
	// A turn Pi could not complete is reported as an error, not as an empty
	// success: an errored turn emits no content, so without this the stream
	// would end cleanly and the client would see a finished run with no text.
	if msg, failed := decodeTurnError(rec); failed {
		if lr.cancelled() {
			s.finishCancelled(lr, unlock)
		} else {
			lr.settle(runs.StatusFailed, msg, time.Now())
			unlock()
			lr.publish(errorFrame{Error: msg}, false)
		}
		return true
	}

	switch rec.Type {
	case "message_update":
		text, reasoning, isDelta, ok := decodeMessageUpdate(rec)
		if !ok || !isDelta || (text == "" && reasoning == "") {
			return false
		}
		lr.publishDelta(text, reasoning, newChunk(lr.completionID, req.Model, text, reasoning))
		return false

	default:
		// Tool progress. A known lifecycle event always renders; an
		// unfamiliar one renders only if it names a tool, so a future Pi
		// event about a tool does not vanish mid-execution while an unrelated
		// event is not mislabelled as one.
		name, named := decodeToolEvent(rec)
		if !named {
			return false
		}
		status := toolStatus(rec.Type)
		if !isToolEvent(rec.Type) {
			// Unfamiliar type that still names a tool: running, not a start
			// or end we did not observe.
			status = "running"
		}
		lr.publishTool(name, toolFrame{Tool: name, Status: status})
		return false

	case pi.TypeAgentSettled:
		// The end signal for the turn. Usage goes on its own frame so the
		// app's last-seen-wins parsing picks it up before [DONE].
		if lr.cancelled() {
			s.finishCancelled(lr, unlock)
			return true
		}
		lr.settle(runs.StatusCompleted, "", time.Now())
		unlock()
		if snap := lr.snapshot(); snap.Usage.NonZero() {
			// The usage chunk carries the run's model, not the
			// request's: both come from the same resolved selection
			// today, but the snapshot is the authority a reattached
			// client already read — one run must not name two models.
			lr.publish(chunk{
				ID:      lr.completionID,
				Object:  "chat.completion.chunk",
				Created: nowUnix(),
				Model:   snap.Model,
				Usage:   usageWire(snap.Usage),
			}, false)
		}
		lr.publish(nil, true)
		return true
	}
}

// finishCancelled emits the explicit status frame before the terminator, so a
// client watching from a second device can tell a deliberate stop from a
// normal completion. The app uses it to mark the reply interrupted-by-user
// rather than completed.
func (s *Server) finishCancelled(lr *liveRun, unlock func()) {
	lr.settle(runs.StatusCancelled, "", time.Now())
	unlock()
	// The status frame carries the real snapshot with the status overridden,
	// not a hand-built stub: a client tracking agento_run.seq as a cursor
	// must not see a regression to 0 on cancel. The inner seq is stamped with
	// the delivery seq peeked up front, so cursor and stream agree — the
	// executor is the sole publisher, so publish assigns exactly that seq.
	snap := snapshotOf(lr.snapshot())
	snap.Status = string(runs.StatusCancelled)
	snap.Seq = lr.peekNextSeq()
	lr.publish(runFrame{Run: snap}, false)
	lr.publish(nil, true)
}
