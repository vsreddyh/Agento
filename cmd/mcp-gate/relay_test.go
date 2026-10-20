package main

import (
	"bytes"
	"context"
	"encoding/json"
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

// A gated call holds, and an approval releases the exact bytes to the child.
func TestHoldApproveForwards(t *testing.T) {
	store := approvals.NewMemoryStore()
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
	audit, err := store.ListAudit(context.Background(), approvals.AuditFilter{})
	if err != nil || len(audit) != 1 || audit[0].Disposition != approvals.DispositionApproved {
		t.Fatalf("audit = %+v, %v", audit, err)
	}
}

// A denied call answers Pi with a tool error carrying the call id, and the
// child never sees it.
func TestHoldDenyAnswersPi(t *testing.T) {
	store := approvals.NewMemoryStore()
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
	store := approvals.NewMemoryStore()
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
	store := approvals.NewMemoryStore()
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
	store := approvals.NewMemoryStore()
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
	store := approvals.NewMemoryStore()
	r := testRelay(store)
	r.childIn = &bufWriter{}
	line := `{"jsonrpc":"2.0","method":"tools/call","params":{"name":"list_notes","arguments":{}}}` + "\n"
	if err := r.pumpPiToChild(strings.NewReader(line)); err != nil {
		t.Fatalf("pump: %v", err)
	}
	audit, err := store.ListAudit(context.Background(), approvals.AuditFilter{})
	if err != nil {
		t.Fatal(err)
	}
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
	store := approvals.NewMemoryStore()
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
	audit, _ := store.ListAudit(context.Background(), approvals.AuditFilter{})
	if len(audit) != 1 || audit[0].Disposition != approvals.DispositionDenied {
		t.Fatalf("audit = %+v", audit)
	}
}

// A server may re-encode the id it echoes; pairing must survive that or the
// allowed call's audit row is silently lost.
func TestTakeNormalisesIdEncoding(t *testing.T) {
	r := testRelay(approvals.NewMemoryStore())
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
	r := testRelay(approvals.NewMemoryStore())
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
	store := approvals.NewMemoryStore()
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
