package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"agento/internal/approvals"
)

// sharedStore behaves like Mongo for split-brain purposes: decisions made
// elsewhere are visible, so gated calls park instead of failing fast. Tests
// that hold use it; the fail-fast path is pinned separately with a plain
// memory store.
type sharedStore struct {
	*approvals.MemoryStore
}

func (sharedStore) Shared() bool { return true }

func testStore() sharedStore { return sharedStore{approvals.NewMemoryStore()} }

func testRelay(store approvals.Store) *relay {
	return &relay{
		server:  "notes",
		store:   store,
		log:     testLogger(),
		waitFor: 300 * time.Millisecond,
		pollFor: 10 * time.Millisecond,
		pending: map[string]*callState{},
		holds:   map[string]context.CancelFunc{},
	}
}

type bufWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *bufWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *bufWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func callLine(id, tool, args string) ([]byte, envelope, callParams) {
	raw := `{"jsonrpc":"2.0","id":` + id + `,"method":"tools/call","params":{"name":"` + tool + `","arguments":` + args + `}}`
	var env envelope
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		panic(err)
	}
	var params callParams
	if err := json.Unmarshal(env.Params, &params); err != nil {
		panic(err)
	}
	return []byte(raw), env, params
}

func waitForApproval(t *testing.T, store approvals.Store) approvals.Approval {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		list, err := store.ListApprovals(context.Background(), approvals.ApprovalFilter{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range list {
			if a.Status == approvals.StatusPending {
				return a
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no pending approval appeared")
	return approvals.Approval{}
}

// waitForAudit polls for n rows: notification-path audits are deliberately
// async (the pump must not stall on the store), so they land shortly after the
// frame is handled.
func waitForAudit(t *testing.T, store approvals.Store, n int) []approvals.AuditEntry {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		audit, err := store.ListAudit(context.Background(), approvals.AuditFilter{})
		if err == nil && len(audit) >= n {
			return audit
		}
		if time.Now().After(deadline) {
			audit, _ := store.ListAudit(context.Background(), approvals.AuditFilter{})
			t.Fatalf("audit rows = %d, want %d", len(audit), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A gated call holds, and an approval releases the exact bytes to the child.
func TestHoldApproveForwards(t *testing.T) {
	store := testStore()
	out, child := &bufWriter{}, &bufWriter{}
	r := testRelay(store)
	r.out = out
	r.childIn = child

	line, env, params := callLine(`1`, "delete_note", `{"id":"abc"}`)
	done := make(chan struct{})
	go func() { defer close(done); r.holdCall(line, env, params) }()

	a := waitForApproval(t, store)
	if a.Tool != "delete_note" || a.Server != "notes" {
		t.Fatalf("approval = %+v", a)
	}
	if _, err := store.DecideApproval(context.Background(), a.ID, true, "app", time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hold did not release after approval")
	}
	if got := child.String(); !strings.Contains(got, `"delete_note"`) {
		t.Fatalf("child never received the call: %q", got)
	}
	// The response completes the audit story: one approved row, joined to the
	// decision, not a forward-time row plus an anonymous allow row.
	st := r.take(json.RawMessage(`1`))
	if st == nil {
		t.Fatal("approved call was not tracked for its response")
	}
	r.auditResponse(st, []byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	audit, err := store.ListAudit(context.Background(), approvals.AuditFilter{})
	if err != nil || len(audit) != 1 {
		t.Fatalf("audit = %+v, %v", audit, err)
	}
	if audit[0].Disposition != approvals.DispositionApproved || audit[0].ApprovalID != a.ID {
		t.Fatalf("audit = %+v, want approved/%s", audit[0], a.ID)
	}
}

// A denied call answers Pi with a tool error carrying the call id, and the
// child never sees it.
func TestHoldDenyAnswersPi(t *testing.T) {
	store := testStore()
	out, child := &bufWriter{}, &bufWriter{}
	r := testRelay(store)
	r.out = out
	r.childIn = child

	line, env, params := callLine(`7`, "delete_note", `{"id":"abc"}`)
	done := make(chan struct{})
	go func() { defer close(done); r.holdCall(line, env, params) }()

	a := waitForApproval(t, store)
	if _, err := store.DecideApproval(context.Background(), a.ID, false, "app", time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hold did not end after deny")
	}
	if child.String() != "" {
		t.Fatalf("denied call reached the child: %q", child.String())
	}
	var reply struct {
		ID    json.RawMessage `json:"id"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	dec := json.NewDecoder(strings.NewReader(out.String()))
	if err := dec.Decode(&reply); err != nil {
		t.Fatalf("pi got no error frame: %q (%v)", out.String(), err)
	}
	if string(reply.ID) != "7" || reply.Error.Message == "" {
		t.Fatalf("bad error frame: %+v", reply)
	}
}

// No decision in time fails closed: error to Pi, expiry recorded.
func TestHoldTimeoutFailsClosed(t *testing.T) {
	store := testStore()
	out, child := &bufWriter{}, &bufWriter{}
	r := testRelay(store)
	r.out = out
	r.childIn = child
	r.waitFor = 60 * time.Millisecond
	r.pollFor = 10 * time.Millisecond

	line, env, params := callLine(`9`, "delete_note", `{"id":"abc"}`)
	done := make(chan struct{})
	go func() { defer close(done); r.holdCall(line, env, params) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hold did not time out")
	}
	if child.String() != "" {
		t.Fatalf("expired call reached the child: %q", child.String())
	}
	if !strings.Contains(out.String(), "expired") {
		t.Fatalf("pi got no expiry error: %q", out.String())
	}
	audit, _ := store.ListAudit(context.Background(), approvals.AuditFilter{})
	if len(audit) != 1 || audit[0].Disposition != approvals.DispositionExpired {
		t.Fatalf("audit = %+v", audit)
	}
}

// Allowed calls forward untouched and audit as allowed.
func TestAllowForwardsAndAudits(t *testing.T) {
	store := testStore()
	if approvals.NeedsApproval("notes", "list_notes", nil) {
		t.Fatal("list_notes should run free")
	}
	r := testRelay(store)
	r.out = &bufWriter{}
	r.childIn = &bufWriter{}
	line, env, params := callLine(`3`, "list_notes", `{}`)
	// Forward path of pumpPiToChild, minus the pump: track + write.
	r.track(env.ID, params.Name, params.Arguments)
	r.writeChild(line)
	// A canned result coming back audits clean.
	r.take(env.ID)
	r.audit(params.Name, params.Arguments, approvals.DispositionAllow, "", "")
	audit, _ := store.ListAudit(context.Background(), approvals.AuditFilter{})
	if len(audit) != 1 || audit[0].Disposition != approvals.DispositionAllow {
		t.Fatalf("audit = %+v", audit)
	}
}

// End to end through real pipes with cat(1) standing in for a server: an
// allowed call round-trips, and the relay exits when Pi hangs up.
func TestRelayRoundTrip(t *testing.T) {
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("no cat binary")
	}
	store := testStore()
	r := testRelay(store)
	piInR, piInW := ioPipe(t)
	piOut := &bufWriter{}
	done := make(chan error, 1)
	go func() { done <- r.run(context.Background(), piInR, piOut, "cat", nil) }()
	line, _, _ := callLine(`11`, "list_notes", `{}`)
	piInW.Write(append(line, '\n'))
	deadline := time.Now().Add(5 * time.Second)
	for {
		if strings.Contains(piOut.String(), `"list_notes"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no round trip: %q", piOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	piInW.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relay did not exit after hangup")
	}
}

func ioPipe(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close(); w.Close() })
	return r, w
}

func testLogger() *log.Logger {
	return log.New(io.Discard, "", 0)
}

// A tools/call notification (no id) has no response to observe, so the allow
// path audits it inline. Without that, "every proxied call is audited" was
// false for exactly the calls the relay cannot track.
func TestAllowNotificationIsAudited(t *testing.T) {
	store := testStore()
	r := testRelay(store)
	r.childIn = &bufWriter{}
	line := `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"list_notes","arguments":{}}}` + "\n"
	if err := r.pumpPiToChild(strings.NewReader(line)); err != nil {
		t.Fatalf("pump: %v", err)
	}
	audit := waitForAudit(t, store, 1)
	if len(audit) != 1 {
		t.Fatalf("audit rows = %d, want 1", len(audit))
	}
	if audit[0].Tool != "list_notes" || audit[0].Disposition != approvals.DispositionAllow {
		t.Fatalf("audit = %+v", audit[0])
	}
}

// A gated notification (tools/call with no id) cannot release anything, so
// it fails closed inline: no hold, no approval card, no reply frame, and a
// denied audit row saying why.
func TestGatedNotificationFailsClosed(t *testing.T) {
	store := testStore()
	out, child := &bufWriter{}, &bufWriter{}
	r := testRelay(store)
	r.out = out
	r.childIn = child

	line := `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"delete_note","arguments":{"id":"abc"}}}` + "\n"
	if err := r.pumpPiToChild(strings.NewReader(line)); err != nil {
		t.Fatalf("pump: %v", err)
	}
	if out.String() != "" {
		t.Fatalf("notification got a reply: %q", out.String())
	}
	if child.String() != "" {
		t.Fatalf("gated notification reached the child: %q", child.String())
	}
	if list, _ := store.ListApprovals(context.Background(), approvals.ApprovalFilter{}); len(list) != 0 {
		t.Fatalf("notification parked an approval card: %+v", list)
	}
	audit := waitForAudit(t, store, 1)
	if len(audit) != 1 || audit[0].Disposition != approvals.DispositionDenied {
		t.Fatalf("audit = %+v", audit)
	}
}

// A server may re-encode the id it echoes; pairing must survive that or the
// allowed call's audit row is silently lost.
func TestTakeNormalisesIdEncoding(t *testing.T) {
	r := testRelay(testStore())
	r.track(json.RawMessage(`1`), "list_notes", map[string]any{})
	if st := r.take(json.RawMessage(`1.0`)); st == nil {
		t.Fatal("1 and 1.0 did not pair")
	}
	r.track(json.RawMessage(` "abc" `), "list_notes", map[string]any{})
	if st := r.take(json.RawMessage(`"abc"`)); st == nil {
		t.Fatal("whitespace-padded id did not pair")
	}
	r.track(json.RawMessage(`1e3`), "list_notes", map[string]any{})
	if st := r.take(json.RawMessage(`1000`)); st == nil {
		t.Fatal("1e3 and 1000 did not pair")
	}
	// Above 2^53 a float64 round-trip would merge these two ids; exact
	// rationals must keep them apart.
	r.track(json.RawMessage(`9007199254740993`), "list_notes", map[string]any{})
	if st := r.take(json.RawMessage(`9007199254740992`)); st != nil {
		t.Fatal("distinct large ids paired with each other")
	}
	if st := r.take(json.RawMessage(`9007199254740993`)); st == nil {
		t.Fatal("large id did not pair with itself")
	}
}

// The pending table is bounded even when a server never answers.
func TestPendingTableIsBounded(t *testing.T) {
	r := testRelay(testStore())
	for i := 0; i < pendingCap+50; i++ {
		r.track(json.RawMessage(strconv.Itoa(i)), "list_notes", map[string]any{})
	}
	r.mu.Lock()
	n := len(r.pending)
	r.mu.Unlock()
	if n != pendingCap {
		t.Fatalf("pending = %d, want %d", n, pendingCap)
	}
}

// Teardown mid-hold is not a timeout: the row must expire with the
// "abandoned at teardown" reason, and Pi gets a failure for the call it is
// waiting on. This branch only runs when holdCtx is wired, which the other
// tests leave nil.
func TestHoldAbandonedAtTeardown(t *testing.T) {
	store := testStore()
	out, child := &bufWriter{}, &bufWriter{}
	r := testRelay(store)
	r.out = out
	r.childIn = child
	r.waitFor = time.Minute
	r.pollFor = 10 * time.Millisecond

	holdCtx, cancelHolds := context.WithCancel(context.Background())
	defer cancelHolds()
	r.holdCtx = holdCtx

	line, env, params := callLine(`11`, "delete_note", `{"id":"abc"}`)
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.holdCall(line, env, params)
	}()
	waitForApproval(t, store)
	cancelHolds()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hold did not end at teardown")
	}
	if child.String() != "" {
		t.Fatalf("abandoned call reached the child: %q", child.String())
	}
	if !strings.Contains(out.String(), "abandoned at teardown") {
		t.Fatalf("Pi got no teardown error: %q", out.String())
	}
	audit, _ := store.ListAudit(context.Background(), approvals.AuditFilter{})
	if len(audit) != 1 || audit[0].Disposition != approvals.DispositionExpired ||
		!strings.Contains(audit[0].Error, "abandoned at teardown") {
		t.Fatalf("audit = %+v", audit)
	}
}

// A tools/call the relay cannot parse is denied, not forwarded: the gate must
// not be bypassable by sending params it cannot classify.
func TestMalformedToolsCallFailsClosed(t *testing.T) {
	store := testStore()
	out, child := &bufWriter{}, &bufWriter{}
	r := testRelay(store)
	r.out = out
	r.childIn = child
	line := `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":"nonsense"}` + "\n"
	if err := r.pumpPiToChild(strings.NewReader(line)); err != nil {
		t.Fatalf("pump: %v", err)
	}
	if child.String() != "" {
		t.Fatalf("malformed call reached the child: %q", child.String())
	}
	if !strings.Contains(out.String(), "could not be parsed") {
		t.Fatalf("Pi got no error: %q", out.String())
	}
	audit := waitForAudit(t, store, 1)
	if len(audit) != 1 || audit[0].Disposition != approvals.DispositionDenied ||
		audit[0].Tool != "(malformed)" {
		t.Fatalf("audit = %+v", audit)
	}
}

// A row that expired elsewhere is reported as expired, never as a denial by
// the user: the audit trail must not attribute an action nobody took.
func TestPollObservedExpiryIsNotADenial(t *testing.T) {
	store := testStore()
	out, child := &bufWriter{}, &bufWriter{}
	r := testRelay(store)
	r.out = out
	r.childIn = child
	r.waitFor = time.Minute
	r.pollFor = 10 * time.Millisecond

	line, env, params := callLine(`12`, "delete_note", `{"id":"abc"}`)
	done := make(chan struct{})
	go func() { defer close(done); r.holdCall(line, env, params) }()
	a := waitForApproval(t, store)
	// MarkInterrupted's shape: pending -> expired without a human decision.
	if err := store.TimeoutApproval(context.Background(), a.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hold did not observe the expiry")
	}
	if child.String() != "" {
		t.Fatalf("expired call reached the child: %q", child.String())
	}
	audit, _ := store.ListAudit(context.Background(), approvals.AuditFilter{})
	if len(audit) != 1 || audit[0].Disposition != approvals.DispositionExpired ||
		!strings.Contains(audit[0].Error, "expired before a decision") {
		t.Fatalf("audit = %+v", audit)
	}
}

// Once teardown starts, auditAsync writes inline: the row survives the join
// instead of racing process exit, and no Add can race the Wait.
func TestAuditAsyncIsInlineAfterClose(t *testing.T) {
	store := testStore()
	r := testRelay(store)
	r.beginClose()
	r.auditAsync("list_notes", nil, approvals.DispositionAllow, "", "")
	audit, err := store.ListAudit(context.Background(), approvals.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != 1 || audit[0].Tool != "list_notes" {
		t.Fatalf("audit = %+v, want the row written synchronously", audit)
	}
}

// A gated call arriving after teardown starts is denied inline: it cannot be
// parked (the relay is exiting) and it must not Add to a WaitGroup already
// being waited on.
func TestGatedCallAfterCloseFailsClosed(t *testing.T) {
	store := testStore()
	out, child := &bufWriter{}, &bufWriter{}
	r := testRelay(store)
	r.out = out
	r.childIn = child
	r.beginClose()

	line := `{"jsonrpc":"2.0","id":21,"method":"tools/call","params":{"name":"delete_note","arguments":{"id":"abc"}}}` + "\n"
	if err := r.pumpPiToChild(strings.NewReader(line)); err != nil {
		t.Fatalf("pump: %v", err)
	}
	if child.String() != "" {
		t.Fatalf("closing relay forwarded the call: %q", child.String())
	}
	if !strings.Contains(out.String(), "shutting down") {
		t.Fatalf("Pi got no shutdown error: %q", out.String())
	}
	if list, _ := store.ListApprovals(context.Background(), approvals.ApprovalFilter{}); len(list) != 0 {
		t.Fatalf("closing relay parked an approval: %+v", list)
	}
	audit, _ := store.ListAudit(context.Background(), approvals.AuditFilter{})
	if len(audit) != 1 || audit[0].Disposition != approvals.DispositionDenied ||
		!strings.Contains(audit[0].Error, "relay_closing") {
		t.Fatalf("audit = %+v", audit)
	}
}

// rpcError's fallback is defensive (hasReplyID means the id decoded), but a
// malformed RawMessage must still produce a valid frame with a null id rather
// than a bare newline the client reads as a protocol violation.
func TestRPCErrorFallsBackToNullID(t *testing.T) {
	got := rpcError(json.RawMessage(`{`), -32602, "bad params")
	var frame map[string]any
	if err := json.Unmarshal(got, &frame); err != nil {
		t.Fatalf("rpcError produced invalid JSON: %q", got)
	}
	if id, ok := frame["id"]; !ok || id != nil {
		t.Fatalf("id = %#v, want null", frame["id"])
	}
	if _, ok := frame["error"]; !ok {
		t.Fatalf("frame = %q, want an error object", got)
	}
}

// failWriter simulates the child's stdin being gone: the call never reaches
// the server.
type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// A forwarded-but-undelivered call must not stay tracked and must not be
// recorded as a clean allow: no response is coming to complete it.
func TestAllowDeliveryFailureUntracksAndAudits(t *testing.T) {
	store := testStore()
	r := testRelay(store)
	r.out = &bufWriter{}
	r.childIn = failWriter{}

	line, _, _ := callLine("7", "list_notes", `{}`)
	if err := r.pumpPiToChild(strings.NewReader(string(line) + "\n")); err == nil {
		t.Fatal("pump returned nil after a failed forward")
	}
	r.mu.Lock()
	pending := len(r.pending)
	r.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending entries = %d after failed delivery, want 0", pending)
	}
	audit := waitForAudit(t, store, 1)
	if len(audit) != 1 || audit[0].Disposition != approvals.DispositionAllow ||
		!strings.Contains(audit[0].Error, "not delivered") {
		t.Fatalf("audit = %+v, want one allow row recording the delivery failure", audit)
	}
}

// Without a shared store no decision can ever arrive, so a gated call fails
// fast instead of burning the full hold: same verdict (denied, failed
// closed), minutes earlier, audited as its own cause.
func TestUnsharedStoreFailsFast(t *testing.T) {
	store := approvals.NewMemoryStore()
	r := testRelay(store)
	r.out = &bufWriter{}
	r.childIn = &bufWriter{}

	line, _, _ := callLine("9", "delete_note", `{"id":"abc"}`)
	if err := r.pumpPiToChild(strings.NewReader(string(line) + "\n")); err != nil {
		t.Fatalf("pump: %v", err)
	}
	if got := r.childIn.(*bufWriter).String(); got != "" {
		t.Fatalf("unshared relay forwarded the call: %q", got)
	}
	if out := r.out.(*bufWriter).String(); !strings.Contains(out, "no shared store") {
		t.Fatalf("Pi got no unavailable error: %q", out)
	}
	if list, _ := store.ListApprovals(context.Background(), approvals.ApprovalFilter{}); len(list) != 0 {
		t.Fatalf("unshared relay parked an approval: %+v", list)
	}
	audit, _ := store.ListAudit(context.Background(), approvals.AuditFilter{})
	if len(audit) != 1 || audit[0].Disposition != approvals.DispositionDenied ||
		!strings.Contains(audit[0].Error, "relay_unavailable") {
		t.Fatalf("audit = %+v, want one relay_unavailable deny", audit)
	}
}

// Draft-only v1 (#316): sends never reach the server, gated or not. The
// relay refuses inline with the draft message, audits a deny, and parks no
// approval card.
func TestDraftOnlySendIsRefused(t *testing.T) {
	store := testStore()
	r := testRelay(store)
	r.server = "gmail"
	r.out = &bufWriter{}
	r.childIn = &bufWriter{}

	line, _, _ := callLine("7", "send_message", `{"to":"a@b.c"}`)
	if err := r.pumpPiToChild(strings.NewReader(string(line) + "\n")); err != nil {
		t.Fatalf("pump: %v", err)
	}
	if got := r.childIn.(*bufWriter).String(); got != "" {
		t.Fatalf("draft-only send reached the child: %q", got)
	}
	if out := r.out.(*bufWriter).String(); !strings.Contains(out, "draft") {
		t.Fatalf("Pi got no draft refusal: %q", out)
	}
	if list, _ := store.ListApprovals(context.Background(), approvals.ApprovalFilter{}); len(list) != 0 {
		t.Fatalf("refused send parked an approval: %+v", list)
	}
	audit := waitForAudit(t, store, 1)
	if len(audit) != 1 || audit[0].Disposition != approvals.DispositionDenied {
		t.Fatalf("audit = %+v, want one denied row", audit)
	}
	if !strings.Contains(audit[0].Error, "connector_draft_only") {
		t.Fatalf("audit error should name the draft-only cause, got %q", audit[0].Error)
	}
}
