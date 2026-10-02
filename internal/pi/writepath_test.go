package pi

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCallRespectsContextWhileWriting covers the path where Call writes to a child
// that has stopped draining stdin. Send was covered; Call was not, and it is the
// one the gateway uses for every turn.
func TestCallRespectsContextWhileWriting(t *testing.T) {
	c := startFake(t, "sleep 5\n")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := c.Call(ctx, "noop", map[string]any{"padding": strings.Repeat("x", 256<<10)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Call must honour ctx while writing, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("Call blocked %v despite ctx", elapsed)
	}
}

// TestConcurrentWritesDoNotBlockOnMutex covers the queueing half of the same
// hazard: the second caller waits on writeMu, which must also be bounded by ctx.
// Taking the lock on the caller's goroutine made a deadline useless here.
func TestConcurrentWritesDoNotBlockOnMutex(t *testing.T) {
	// Holds stdin open without reading, so the first write fills the pipe and
	// never completes.
	c := startFake(t, "sleep 5\n")

	// Occupy the write path with a write that will block.
	blockedCtx, cancelBlocked := context.WithCancel(context.Background())
	go func() { _ = c.Send(blockedCtx, "filler", map[string]any{"pad": strings.Repeat("y", 512<<10)}) }()
	time.Sleep(200 * time.Millisecond)

	// A second writer must still be able to give up on its own deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := c.Send(ctx, "second", map[string]any{"pad": strings.Repeat("z", 512<<10)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second write queued behind writeMu must honour ctx, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("queued writer waited %v on the mutex", elapsed)
	}
	cancelBlocked()
}

// TestAbandonedWriteIsNotSent checks that a command whose context expired while it
// sat queued is dropped rather than delivered late. Without this, a retry would
// be preceded by the command the caller already abandoned.
func TestAbandonedWriteIsNotSent(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "received")

	// Records every command line it receives, one per line, so the test can prove
	// the abandoned command never arrived.
	script := filepath.Join(dir, "recorder")
	body := `#!/bin/sh
: > ` + marker + `
i=0
while IFS= read -r line; do
  printf '%s\n' "$line" >> ` + marker + `
  i=$((i+1))
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"noop","success":true}\n' "$id"
  if [ $i -ge 20 ]; then exit 0; fi
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write recorder: %v", err)
	}

	c, err := Start(context.Background(), Options{Bin: script})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Cancel before the write can plausibly be scheduled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := c.Send(ctx, "abandoned-command", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}

	// Give any stray background write a chance to land, then confirm it did not.
	time.Sleep(300 * time.Millisecond)
	data, err := os.ReadFile(marker)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read marker: %v", err)
	}
	if strings.Contains(string(data), "abandoned-command") {
		t.Fatalf("a cancelled command was still sent to the child:\n%s", data)
	}
}

// TestSubscribersClosedOnProcessExit covers the crash case: an event loop must see
// its channel close when Pi dies, not hang waiting for records that will never
// come. Close() is not called here — the child simply exits.
func TestSubscribersClosedOnProcessExit(t *testing.T) {
	c := startFake(t, "exit 9\n")

	events, cancel := c.Subscribe()
	defer cancel()

	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("child never exited")
	}

	// Drain to a close. A timeout here is the bug this test exists for.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return // channel closed: correct
			}
		case <-deadline:
			t.Fatal("subscriber channel still open after the child exited")
		}
	}
}

// TestSendCannotSmuggleAnID checks a payload-supplied id is discarded: a response
// to an id nobody registered is dropped as a late reply, so smuggling one in
// silently loses that command's response.
func TestSendCannotSmuggleAnID(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "line")

	script := filepath.Join(dir, "echo-raw")
	body := `#!/bin/sh
while IFS= read -r line; do
  printf '%s\n' "$line" >> ` + out + `
  sleep 0.2
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	c, err := Start(context.Background(), Options{Bin: script})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = c.Close() }()

	if err := c.Send(context.Background(), "noop", map[string]any{"id": "smuggled"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	// The stub sleeps before handling a line, so poll rather than reading once.
	data := waitForFile(t, out)
	if strings.Contains(data, "smuggled") {
		t.Fatalf("payload id reached the wire: %s", strings.TrimSpace(data))
	}
	if !strings.Contains(data, "\"type\":\"noop\"") {
		t.Fatalf("command never arrived at all; test proves nothing: %q", data)
	}
}

// waitForFile polls for path to appear and return non-empty content.
func waitForFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return string(data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
	return ""
}

// TestCallCannotSmuggleAnID is the same guarantee on the Call path, where an id
// IS supplied: the generated one must win.
func TestCallCannotSmuggleAnID(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "line")

	script := filepath.Join(dir, "echo-id")
	body := `#!/bin/sh
while IFS= read -r line; do
  printf '%s\n' "$line" >> ` + out + `
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"noop","success":true}\n' "$id"
done
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}

	c, err := Start(context.Background(), Options{Bin: script})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.Call(context.Background(), "noop", map[string]any{"id": "smuggled"}); err != nil {
		t.Fatalf("call: %v", err)
	}
	data, _ := os.ReadFile(out)
	if strings.Contains(string(data), "smuggled") {
		t.Fatalf("payload id overrode the correlation id: %s", strings.TrimSpace(string(data)))
	}
}

// TestMalformedIsCounted makes protocol skew observable rather than silent.
func TestMalformedIsCounted(t *testing.T) {
	c := startFake(t, `
while IFS= read -r line; do
  printf 'definitely not json\n'
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"noop","success":true}\n' "$id"
done
`)
	if _, err := c.Call(context.Background(), "noop", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if c.Malformed() == 0 {
		t.Fatal("malformed line was dropped silently; it must be counted")
	}
}

// TestEnvWithIsDeterministic pins sorted override output, so the same inputs
// always produce the same environment.
func TestEnvWithIsDeterministic(t *testing.T) {
	base := []string{"A=1"}
	kv := map[string]string{"z": "1", "a": "2", "m": "3"}

	first := EnvWith(base, kv)
	for i := 0; i < 20; i++ {
		got := EnvWith(base, kv)
		if strings.Join(got, "\n") != strings.Join(first, "\n") {
			t.Fatalf("non-deterministic output:\n%v\n%v", first, got)
		}
	}

	overrides := first[len(base):]
	if !sort.StringsAreSorted(overrides) {
		t.Fatalf("overrides not sorted: %v", overrides)
	}
}

// TestEnvWithKeepsMalformedEntries covers an entry with no "=", which cannot be
// matched against an override and must survive untouched.
func TestEnvWithKeepsMalformedEntries(t *testing.T) {
	got := EnvWith([]string{"WEIRD", "A=1"}, map[string]string{"A": "2"})
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "WEIRD") {
		t.Fatalf("entry without '=' was dropped: %v", got)
	}
	if strings.Contains(joined, "A=1") {
		t.Fatalf("override did not apply: %v", got)
	}
}

// TestCloseIsBoundedWhenChildIgnoresSIGKILL is hard to arrange portably, so this
// asserts the weaker but real property: Close returns promptly for a child that
// exits promptly, and never waits the full grace for a healthy shutdown.
func TestCloseReturnsPromptlyForHealthyChild(t *testing.T) {
	c, err := Start(context.Background(), Options{Bin: fakePi(t, echoID)})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	done := make(chan struct{})
	go func() { _ = c.Close(); close(done) }()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close took longer than its grace period for a healthy child")
	}
}

// TestConcurrentCloseAndCall exercises the closed/exit interaction under -race.
func TestConcurrentCloseAndCall(t *testing.T) {
	for i := 0; i < 20; i++ {
		c := startFake(t, echoID)
		var wg sync.WaitGroup
		for j := 0; j < 6; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_, _ = c.Call(ctx, "noop", nil)
			}()
		}
		wg.Add(1)
		go func() { defer wg.Done(); _ = c.Close() }()

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Fatalf("iteration %d deadlocked", i)
		}
	}
}

// TestSubscribeAfterExitReturnsClosedChannel covers registering a listener once
// the child is already gone. Such a channel must be closed, or a consumer that
// subscribes late blocks forever on a set nothing will close again.
func TestSubscribeAfterExitReturnsClosedChannel(t *testing.T) {
	c := startFake(t, "exit 5\n")
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("child never exited")
	}

	events, cancel := c.Subscribe()
	defer cancel()

	select {
	case _, ok := <-events:
		if ok {
			// A buffered record is fine; keep draining until closed.
			deadline := time.After(5 * time.Second)
			for {
				select {
				case _, ok := <-events:
					if !ok {
						return
					}
				case <-deadline:
					t.Fatal("channel not closed")
				}
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Subscribe after exit returned a channel that was never closed")
	}
}

// TestArgsCannotShadowMode guards the protocol: --mode is reserved, because
// letting a caller pass it would put the child in a mode whose output this
// package cannot read.
func TestArgsCannotShadowMode(t *testing.T) {
	for _, arg := range []string{"--mode", "-mode", "--mode=json"} {
		if _, err := Start(context.Background(), Options{
			Bin:  fakePi(t, echoID),
			Args: []string{arg},
		}); err == nil {
			t.Fatalf("Args %q was accepted; --mode must be reserved", arg)
		} else if err != nil && !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("Args %q: want a 'reserved' error, got %v", arg, err)
		}
	}

	// A normal argument must still get through.
	c, err := Start(context.Background(), Options{Bin: fakePi(t, echoID), Args: []string{"--no-session"}})
	if err != nil {
		t.Fatalf("ordinary arg rejected: %v", err)
	}
	_ = c.Close()
}

// TestLateResponsesAreCounted makes a slow turn distinguishable from a lost one.
// The orphan response has to come from the child: the client no longer allows a
// caller to put an arbitrary id on the wire, which is the point of the previous
// test.
func TestLateResponsesAreCounted(t *testing.T) {
	c := startFake(t, `
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  # An orphan: a response for an id no caller is registered under.
  printf '{"id":"nobody-home","type":"response","command":"noop","success":true}\n'
  printf '{"id":"%s","type":"response","command":"noop","success":true}\n' "$id"
done
`)

	if _, err := c.Call(context.Background(), "noop", nil); err != nil {
		t.Fatalf("call: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if c.LateResponses() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("late response was dropped without being counted")
}

// TestModeFlagGuardIsNotBypassable closes the "-mode=x" hole the earlier
// literal-form check left open.
func TestModeFlagGuardIsNotBypassable(t *testing.T) {
	// Deliberately not --MODE: CLI flags are case-sensitive, so that is a
	// different (unknown) flag rather than a way to reach mode selection.
	for _, arg := range []string{
		"--mode", "-mode", "--mode=rpc", "-mode=rpc", "--mode=json", "-mode=json",
		"---mode",
	} {
		_, err := Start(context.Background(), Options{Bin: fakePi(t, echoID), Args: []string{arg}})
		if err == nil {
			t.Fatalf("Args %q was accepted; --mode must be reserved in every form", arg)
		}
		if !strings.Contains(err.Error(), "reserved") {
			t.Fatalf("Args %q: want a 'reserved' error, got %v", arg, err)
		}
	}

	// A similarly-named but unrelated flag must still pass.
	c, err := Start(context.Background(), Options{
		Bin:  fakePi(t, echoID),
		Args: []string{"--modelfile", "--session-dir", "/tmp"},
	})
	if err != nil {
		t.Fatalf("unrelated args rejected: %v", err)
	}
	_ = c.Close()
}

// TestWriteAfterCloseReportsErrClosed covers Close winning the race against a
// write that had already passed the closed check: the child is still alive, so
// this is a shutdown, not a crash.
func TestWriteAfterCloseReportsErrClosed(t *testing.T) {
	c, err := Start(context.Background(), Options{Bin: fakePi(t, echoID)})
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// Close concurrently with a burst of writes; every write must report a clean
	// shutdown rather than a raw pipe error.
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				err := c.Send(context.Background(), "noop", nil)
				if err == nil {
					continue
				}
				if !errors.Is(err, ErrClosed) && !errors.Is(err, ErrProcessExited) {
					t.Errorf("write during close reported %v; want ErrClosed or ErrProcessExited", err)
				}
				return
			}
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); time.Sleep(20 * time.Millisecond); _ = c.Close() }()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("deadlock between Close and concurrent writes")
	}
}

// TestUnrecoverableDesyncKillsChild covers the LF-less flood. The reader cannot
// realign, so the correct outcome is a loud teardown — the child is killed and
// callers get ErrProcessExited — rather than a read loop that is alive but will
// never deliver another record.
func TestUnrecoverableDesyncKillsChild(t *testing.T) {
	// Emit an endless run with no LF, then sleep so the kill is what ends us.
	c, err := Start(context.Background(), Options{
		Bin: fakePi(t, `
printf 'no-newline-forever'
i=0
while [ $i -lt 100000 ]; do printf 'XXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX'; i=$((i+1)); done
sleep 30
`),
		// Small caps so the desync bound is reached quickly in the test.
		MaxRecordBytes: 4096,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = c.Close() }()

	// A command cannot get an answer, and must not hang forever.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	_, callErr := c.Call(ctx, "noop", nil)
	if !errors.Is(callErr, ErrProcessExited) {
		t.Fatalf("want ErrProcessExited after desync, got %v", callErr)
	}
	if !c.Desynchronised() {
		t.Fatal("Desynchronised() should report the unrecoverable stream")
	}
}

// TestAbandonedResponseIsCountedAsLate covers the gap where a waiter has timed
// out but not yet run its deferred delete, so the response hits the non-blocking
// send's default branch. That is an abandoned response, not a silent drop.
func TestAbandonedResponseIsCountedAsLate(t *testing.T) {
	// Delay the reply long enough for the caller to time out, then reply anyway.
	c := startFake(t, `
while IFS= read -r line; do
  sleep 1
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"noop","success":true}\n' "$id"
done
`)

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err := c.Call(ctx, "noop", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if c.LateResponses() > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("response to a timed-out caller was discarded without being counted")
}

// TestRingBufferReleasesOldArray checks the buffer bounds retained memory, not
// just length. Reslicing forward would keep len() at the limit while the whole
// original allocation stayed reachable, so this measures live heap rather than
// trusting cap — cap hides exactly this bug.
//
// The chunk is allocated AFTER the baseline is read; including it beforehand makes
// the unsigned delta underflow.
func TestRingBufferReleasesOldArray(t *testing.T) {
	if testing.Short() {
		t.Skip("writes ~64 MiB to measure retained heap")
	}

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	chunk := make([]byte, 16<<20) // 16 MiB
	for i := range chunk {
		chunk[i] = 'x'
	}

	r := newRingBuffer(64 << 10)
	for i := 0; i < 4; i++ { // 64 MiB total
		r.Write(chunk)
	}

	runtime.GC()
	runtime.ReadMemStats(&after)

	if len(r.buf) != r.limit {
		t.Fatalf("len=%d want %d", len(r.buf), r.limit)
	}
	if after.HeapAlloc < before.HeapAlloc {
		t.Skip("heap measurement unstable in this environment")
	}

	// Reslicing keeps an INTERIOR pointer into the last array, and Go's GC pins
	// the whole object for an interior pointer. Measured on this tree:
	//   reslice    -> heap grows 16.0 MiB (the pinned array)
	//   copy-trim  -> heap grows  0.0 MiB
	// The threshold sits between the two. A looser bound (24 MiB was tried first)
	// passes against the buggy code and guards nothing.
	const allowed = 4 << 20
	if grew := after.HeapAlloc - before.HeapAlloc; grew > allowed {
		t.Fatalf("retained %d bytes for a %d-byte buffer; old backing array is still reachable",
			grew, r.limit)
	}
}

// TestMaxRecordBytesCeilingPreventsOverflow pins the clamp. Without it a caller
// passing a value near MaxInt makes maxRec*discardMultiple wrap negative, which
// silently disables the desync bound rather than failing loudly.
func TestMaxRecordBytesCeilingPreventsOverflow(t *testing.T) {
	for _, in := range []int{math.MaxInt, math.MaxInt / 2, math.MaxInt / 8} {
		c, err := Start(context.Background(), Options{
			Bin:            fakePi(t, echoID),
			MaxRecordBytes: in,
		})
		if err != nil {
			t.Fatalf("start with MaxRecordBytes=%d: %v", in, err)
		}
		if c.maxRecord <= 0 || c.maxRecord > maxRecordCeiling {
			t.Fatalf("MaxRecordBytes=%d left maxRecord=%d, outside (0, %d]",
				in, c.maxRecord, maxRecordCeiling)
		}
		if c.maxDiscard <= 0 {
			t.Fatalf("MaxRecordBytes=%d produced maxDiscard=%d; a non-positive "+
				"bound disables the desync guard silently", in, c.maxDiscard)
		}
		_ = c.Close()
	}

	// A sane value must be honoured rather than clamped away.
	c, err := Start(context.Background(), Options{
		Bin:            fakePi(t, echoID),
		MaxRecordBytes: 1 << 20,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = c.Close() }()
	if c.maxRecord != 1<<20 {
		t.Fatalf("a valid MaxRecordBytes was altered: %d", c.maxRecord)
	}
}

// TestExitErrorIsNilWhileRunning pins the honest reading: a health check on a
// healthy client must not be handed a sentinel that reads as a crash.
func TestExitErrorIsNilWhileRunning(t *testing.T) {
	c := startFake(t, echoID)

	if err := c.ExitError(); err != nil {
		t.Fatalf("ExitError on a running client = %v, want nil", err)
	}
	if _, err := c.Call(context.Background(), "get_state", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if err := c.ExitError(); err != nil {
		t.Fatalf("ExitError after a successful call = %v, want nil", err)
	}

	// After exit it must report the reason.
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("never exited")
	}
	if err := c.ExitError(); err == nil {
		t.Fatal("ExitError after exit = nil, want the exit condition")
	}
}

// TestNilContextDoesNotPanic guards a footgun that would otherwise only surface in
// production: every exported entry point dereferences ctx.
func TestNilContextDoesNotPanic(t *testing.T) {
	c := startFake(t, echoID)
	defer func() { _ = c.Close() }()

	//nolint:staticcheck // deliberately passing nil to prove it is handled
	if _, err := c.Call(nil, "get_state", nil); err != nil {
		t.Fatalf("Call with nil ctx: %v", err)
	}
	//nolint:staticcheck // deliberately passing nil
	if err := c.Send(nil, "noop", nil); err != nil {
		t.Fatalf("Send with nil ctx: %v", err)
	}
}

// TestOptionsEnvReplacesRatherThanMerges documents the footgun in a test so the
// behaviour is pinned: Env is a replacement, and the safe pattern is EnvWith over
// the inherited environment.
func TestOptionsEnvReplacesRatherThanMerges(t *testing.T) {
	// A minimal Env: the child still runs, because the script path was absolute,
	// but PATH is genuinely absent from its environment.
	c, err := Start(context.Background(), Options{
		Bin: fakePi(t, echoID),
		Env: []string{"ONLY_THIS=1"},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.Call(context.Background(), "get_state", nil); err != nil {
		t.Fatalf("call: %v", err)
	}

	// The documented pattern keeps the parent environment and adds to it.
	safe := EnvWith(EnvironOrOs(nil), map[string]string{"ONLY_THIS": "1"})
	found := false
	for _, e := range safe {
		if strings.HasPrefix(e, "PATH=") {
			found = true
		}
	}
	if !found {
		t.Fatal("EnvWith over os.Environ dropped PATH; the documented pattern is unsafe")
	}
}

// TestStartNilContextDoesNotPanic covers the readiness probe path, which fed ctx
// straight into context.WithTimeout while Call/Send had been guarded.
func TestStartNilContextDoesNotPanic(t *testing.T) {
	//nolint:staticcheck // deliberately passing nil to prove it is handled
	if _, err := Start(nil, Options{
		Bin:          fakePi(t, echoID),
		ReadyTimeout: 5 * time.Second,
	}); err != nil {
		t.Fatalf("Start(nil, ReadyTimeout>0): %v", err)
	}

	// And without the probe.
	c, err := Start(nil, Options{Bin: fakePi(t, echoID)})
	if err != nil {
		t.Fatalf("Start(nil): %v", err)
	}
	_ = c.Close()
}

// TestEOFHonoursRecordCap covers the end-of-stream path, which returned the
// accumulated bytes unchecked: a child dumping a huge unterminated tail on exit
// would be buffered in full and then handed to the JSON parser.
func TestEOFHonoursRecordCap(t *testing.T) {
	// A child that exits after writing a run well past the cap, with no trailing
	// LF. The cap is set small so the payload is unambiguously over it.
	c, err := Start(context.Background(), Options{
		Bin: fakePi(t, `
printf 'no-trailing-newline'
i=0
while [ $i -lt 200 ]; do printf 'YYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYYY'; i=$((i+1)); done
exit 0
`),
		MaxRecordBytes: 1024,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = c.Close() }()

	// Wait for the child to finish so the EOF path is the one exercised.
	select {
	case <-c.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("child never exited")
	}

	if c.Oversized() == 0 {
		t.Fatal("unterminated oversized tail at EOF was not counted")
	}
	// The cap is what makes this safe: a tail beyond it must never be parsed.
	if c.Malformed() != 0 {
		t.Fatalf("the oversized tail reached the JSON parser (%d malformed)", c.Malformed())
	}
}

// TestEOFWithinCapIsStillParsed is the other half: a legitimate unterminated
// final record must still be delivered rather than dropped by the new cap.
func TestEOFWithinCapIsStillParsed(t *testing.T) {
	c := startFake(t, `
printf '{"type":"agent_settled"}'
exit 0
`)
	events, cancel := c.Subscribe()
	defer cancel()

	deadline := time.After(15 * time.Second)
	for {
		select {
		case rec, ok := <-events:
			if !ok {
				t.Fatal("channel closed before the unterminated record arrived")
			}
			if rec.Type == TypeAgentSettled {
				if c.Oversized() != 0 {
					t.Fatalf("a small record was counted as oversized")
				}
				return
			}
		case <-deadline:
			t.Fatal("unterminated final record within the cap was dropped")
		}
	}
}

// TestFileDescriptorsAreReleased counts /proc/self/fd across many start/close
// cycles. These pipes are self-managed, so nothing but the readers releases them —
// a leak here would accumulate silently for the lifetime of a long-running
// gateway, one client per profile per restart.
func TestFileDescriptorsAreReleased(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("counts /proc/self/fd")
	}
	if testing.Short() {
		t.Skip("counts file descriptors")
	}

	countFDs := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Skipf("cannot read /proc/self/fd: %v", err)
		}
		return len(entries)
	}

	// Warm up so one-off allocations are not counted as growth.
	for i := 0; i < 5; i++ {
		c := startFake(t, echoID)
		_ = c.Close()
	}
	base := countFDs()

	const cycles = 40
	for i := 0; i < cycles; i++ {
		c := startFake(t, echoID)
		if _, err := c.Call(context.Background(), "get_state", nil); err != nil {
			t.Fatalf("cycle %d call: %v", i, err)
		}
		if err := c.Close(); err != nil {
			t.Fatalf("cycle %d close: %v", i, err)
		}
		<-c.Done()
	}

	// A little slack for runtime bookkeeping, but nothing like 3 per cycle.
	if grew := countFDs() - base; grew > cycles {
		t.Fatalf("descriptor count grew by %d over %d start/close cycles; "+
			"pipes are leaking", grew, cycles)
	}
}

// TestCancelledCallDrainsAndCounts covers the window where a response is delivered
// non-blockingly after the caller gave up but before its id was removed from the
// pending map. That record used to be neither delivered nor counted.
func TestCancelledCallDrainsAndCounts(t *testing.T) {
	// Reply only after the caller has certainly timed out, so the response lands
	// in the abandoned window.
	c := startFake(t, `
while IFS= read -r line; do
  sleep 0.4
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"noop","success":true}\n' "$id"
done
`)

	for i := 0; i < 10; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		_, err := c.Call(ctx, "noop", nil)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("iteration %d: want DeadlineExceeded, got %v", i, err)
		}
		// Nothing should have been counted yet — the reply has not been read.
		time.Sleep(600 * time.Millisecond)
	}

	// Every cancelled call should have had its late response accounted for,
	// rather than any being silently absorbed.
	got := c.LateResponses()
	if got == 0 {
		t.Fatal("responses to abandoned calls were absorbed without being counted")
	}
	t.Logf("lateResponses=%d", got)
}

// TestMaxRecordBytesClampsToCeilingNotDefault checks an explicit request above the
// ceiling is honoured up to it, rather than silently dropped to the default.
func TestMaxRecordBytesClampsToCeilingNotDefault(t *testing.T) {
	c, err := Start(context.Background(), Options{
		Bin:            fakePi(t, echoID),
		MaxRecordBytes: 128 << 20, // above the 64 MiB ceiling, above the 16 MiB default
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = c.Close() }()

	if c.maxRecord != maxRecordCeiling {
		t.Fatalf("maxRecord=%d; an explicit request above the ceiling should clamp to "+
			"the ceiling (%d), not fall back to the default (%d)",
			c.maxRecord, maxRecordCeiling, defaultMaxRecordBytes)
	}
	if c.maxDiscard <= 0 {
		t.Fatalf("discard budget must stay positive, got %d", c.maxDiscard)
	}
}

// TestStartRefusesCancelledContext checks Start does not spawn a process for an
// already-cancelled context.
func TestStartRefusesCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Start(ctx, Options{Bin: fakePi(t, echoID)}); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

// TestDeadlineRaceReturnsQueuedResponse sweeps deadlines across the response time
// so the ctx.Done() and response cases genuinely race.
//
// The invariant is stated globally, not per call: lateResponses is cumulative and
// incremented asynchronously by the read loop, so bracketing one Call cannot
// attribute a count reliably. What must hold is that a response delivered
// successfully is never also counted late — so the total late count can never
// exceed the number of calls that actually failed.
func TestDeadlineRaceReturnsQueuedResponse(t *testing.T) {
	c := startFake(t, `
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"noop","success":true}\n' "$id"
done
`)

	delivered, timedOut := 0, 0
	for _, d := range []time.Duration{
		time.Microsecond, 50 * time.Microsecond, 200 * time.Microsecond,
		500 * time.Microsecond, time.Millisecond, 2 * time.Millisecond,
		5 * time.Millisecond, 20 * time.Millisecond, 50 * time.Millisecond,
		100 * time.Millisecond, 250 * time.Millisecond,
	} {
		for i := 0; i < 15; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), d)
			_, err := c.Call(ctx, "noop", nil)
			cancel()

			switch {
			case err == nil:
				delivered++
			case errors.Is(err, context.DeadlineExceeded):
				timedOut++
			default:
				t.Fatalf("deadline %v: unexpected error %v", d, err)
			}
		}
	}

	if delivered+timedOut != 15*11 {
		t.Fatalf("accounted for %d of %d outcomes", delivered+timedOut, 15*11)
	}
	if delivered == 0 || timedOut == 0 {
		t.Fatalf("sweep did not race both ways: delivered=%d timedOut=%d", delivered, timedOut)
	}

	// Each failed call can account for at most one late response. Anything more
	// means a delivered response was double-reported as lost.
	if got, max := c.LateResponses(), uint64(timedOut); got > max {
		t.Fatalf("lateResponses=%d exceeds the %d failed calls; a delivered response "+
			"was counted late", got, max)
	}
	t.Logf("delivered=%d deadline=%d lateResponses=%d", delivered, timedOut, c.LateResponses())
}

// TestResponseQueuedAtDeadlineIsNotCountedLate verifies a response handed back on
// the ctx.Done() path is a success, not a late drop.
func TestResponseQueuedAtDeadlineIsNotCountedLate(t *testing.T) {
	c := startFake(t, `
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"noop","success":true}\n' "$id"
done
`)

	// Long deadline so the fast stub always wins the race and we exercise the
	// "queued response" branch deterministically.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.Call(ctx, "noop", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	if c.LateResponses() != 0 {
		t.Fatalf("a delivered response was counted late: %d", c.LateResponses())
	}
}

// TestBarePositionalModeArgIsAllowed guards the over-match: a positional argument
// that happens to read "mode" is not the --mode flag and must not be rejected.
func TestBarePositionalModeArgIsAllowed(t *testing.T) {
	for _, arg := range []string{"mode", "mode=rpc", "models", "modex"} {
		c, err := Start(context.Background(), Options{
			Bin:  fakePi(t, echoID),
			Args: []string{arg},
		})
		if err != nil {
			t.Fatalf("positional arg %q was rejected: %v", arg, err)
		}
		_ = c.Close()
	}

	// The flag in every dash form is still reserved.
	for _, arg := range []string{"--mode", "-mode", "--mode=rpc", "-mode=rpc"} {
		if _, err := Start(context.Background(), Options{
			Bin:  fakePi(t, echoID),
			Args: []string{arg},
		}); err == nil {
			t.Fatalf("flag %q was accepted", arg)
		}
	}
}

// TestFailedStartReleasesDescriptors covers the Start error path: StdinPipe
// allocates its own pipe, so it must be closed alongside the two os.Pipe pairs or
// every failed start leaks a descriptor.
func TestFailedStartReleasesDescriptors(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("counts /proc/self/fd")
	}
	if testing.Short() {
		t.Skip("counts file descriptors")
	}

	countFDs := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Skipf("cannot read /proc/self/fd: %v", err)
		}
		return len(entries)
	}

	// A directory that exists but holds no executable: exec fails at Start.
	missing := t.TempDir()
	base := countFDs()

	const attempts = 40
	for i := 0; i < attempts; i++ {
		if _, err := Start(context.Background(), Options{Bin: missing}); err == nil {
			t.Fatal("Start on a non-executable path should fail")
		}
	}

	if grew := countFDs() - base; grew > attempts/2 {
		t.Fatalf("descriptor count grew by %d over %d failed starts; the stdin pipe "+
			"is leaking on the error path", grew, attempts)
	}
}

// TestQueuedResponseSurvivesProcessExit covers the done branch: a response already
// queued when the child dies must be delivered, not reported as a crash. The stub
// exits after one command, so each iteration needs its own client — reusing one
// just measures a dead process.
func TestQueuedResponseSurvivesProcessExit(t *testing.T) {
	const stub = `
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"noop","success":true}\n' "$id"
  exit 0
done
`

	delivered := 0
	for i := 0; i < 40; i++ {
		c := startFake(t, stub)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := c.Call(ctx, "noop", nil)
		cancel()

		switch {
		case err == nil:
			delivered++
		case errors.Is(err, ErrProcessExited):
			// Legitimate only if the response genuinely never arrived. The stub
			// writes it before exiting, so this branch is a narrow window.
		default:
			t.Fatalf("iteration %d: unexpected error %v", i, err)
		}
		_ = c.Close()
	}

	if delivered == 0 {
		t.Fatal("the stub always responded before exiting; no response was ever delivered")
	}
	t.Logf("delivered=%d of 40", delivered)
}

// TestCloseAfterKillIsReportedAsShutdown checks an exit the caller asked for is
// not reported as a crash. Close's own SIGKILL of a hung child produces a non-nil
// exit reason, and an operator reading ErrProcessExited would go hunting a fault
// that is not there.
func TestCloseAfterKillIsReportedAsShutdown(t *testing.T) {
	// Ignore stdin and outlive Close's grace period, so Close has to kill it.
	c := startFake(t, "sleep 30\n")

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	<-c.Done()

	err := c.ExitError()
	if err == nil {
		t.Fatal("ExitError after Close should describe the shutdown")
	}
	if errors.Is(err, ErrProcessExited) && !errors.Is(err, ErrClosed) {
		t.Fatalf("a caller-initiated shutdown reported as a crash: %v", err)
	}
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed after Close, got %v", err)
	}
	t.Logf("reported as: %v", err)
}

// TestCrashIsStillReportedAsCrash is the other half: an exit the caller did NOT
// ask for must still read as a crash.
func TestCrashIsStillReportedAsCrash(t *testing.T) {
	c := startFake(t, "exit 3\n")
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("never exited")
	}

	err := c.ExitError()
	if !errors.Is(err, ErrProcessExited) {
		t.Fatalf("an unexplained exit must report ErrProcessExited, got %v", err)
	}
	if errors.Is(err, ErrClosed) {
		t.Fatalf("a crash must not report ErrClosed: %v", err)
	}
	if !strings.Contains(err.Error(), "3") {
		t.Fatalf("exit status missing from %v", err)
	}
}

// TestAbandonedResponseIsAlwaysAccounted closes the remaining delivery window: a
// response must be either delivered to its waiter or counted late, never absorbed
// into an orphaned channel.
//
// Calls are paced so each stub reply lands well after that call's drain. Firing
// them all at once would leave the stub still working through its backlog when the
// test finished, and only a fraction of the responses would ever arrive to be
// accounted for — which is how an earlier version of this test passed while
// checking almost nothing.
func TestAbandonedResponseIsAlwaysAccounted(t *testing.T) {
	const stub = `
while IFS= read -r line; do
  sleep 0.2
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"noop","success":true}\n' "$id"
done
`

	c := startFake(t, stub)

	const calls = 10
	cancelled := 0
	for i := 0; i < calls; i++ {
		// Deadline well inside the stub's 200ms reply latency.
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		_, err := c.Call(ctx, "noop", nil)
		cancel()

		if errors.Is(err, context.DeadlineExceeded) {
			cancelled++
		} else if err != nil {
			t.Fatalf("call %d: unexpected %v", i, err)
		}
		// Let the stub drain this one before queuing the next.
		time.Sleep(220 * time.Millisecond)
	}

	// Every reply has now landed, after its call already gave up.
	if cancelled != calls {
		t.Skipf("only %d of %d calls were cancelled; nothing to account for", cancelled, calls)
	}
	if got, want := c.LateResponses(), uint64(calls); got != want {
		t.Fatalf("lateResponses=%d, want %d: responses must be delivered or counted, "+
			"never absorbed into an orphaned channel", got, want)
	}
	t.Logf("cancelled=%d lateResponses=%d (all accounted for)", cancelled, c.LateResponses())
}

// TestTruncatedOversizedLineIsCounted covers a child that dies part-way through a
// huge line. discardLine consumed bytes before hitting EOF, so the oversized
// record was definitely seen — it must not vanish from every counter.
func TestTruncatedOversizedLineIsCounted(t *testing.T) {
	c, err := Start(context.Background(), Options{
		Bin: fakePi(t, `
printf 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'
i=0
while [ $i -lt 200 ]; do printf 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'; i=$((i+1)); done
exit 0
`),
		MaxRecordBytes: 1024, // maxDiscard = 16 KiB; the run is ~16 KB
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = c.Close() }()

	select {
	case <-c.Done():
	case <-time.After(20 * time.Second):
		t.Fatal("child never exited")
	}

	if c.Oversized() == 0 {
		t.Fatal("a truncated oversized line was seen but counted nowhere")
	}
	if c.Malformed() != 0 {
		t.Fatalf("discarded bytes reached the parser (%d malformed)", c.Malformed())
	}
	t.Logf("oversized=%d malformed=%d desync=%v", c.Oversized(), c.Malformed(), c.Desynchronised())
}

// TestReadyTimeoutErrorIncludesStderr checks the diagnostic is captured. Stderr is
// snapshotted before Close, since Close waits for child exit rather than for the
// stderr reader to drain.
func TestReadyTimeoutErrorIncludesStderr(t *testing.T) {
	_, err := Start(context.Background(), Options{
		Bin: fakePi(t, `
printf 'boot diagnostic on stderr\n' >&2
sleep 2
`),
		ReadyTimeout: 400 * time.Millisecond,
	})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("want ErrNotReady, got %v", err)
	}
	if !strings.Contains(err.Error(), "boot diagnostic on stderr") {
		t.Fatalf("ErrNotReady lost the child's stderr diagnostic: %v", err)
	}
}
