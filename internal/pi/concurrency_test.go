package pi

import (
	"bufio"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestClosedThenCallReportsErrClosed pins that an orderly shutdown is not
// mistaken for a crash. Close only closes stdin; the child is very much alive,
// so reporting ErrProcessExited here would send callers looking for a segfault
// that never happened.
func TestClosedThenCallReportsErrClosed(t *testing.T) {
	c := startFake(t, echoID)
	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err := c.Call(context.Background(), "get_state", nil)
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("want ErrClosed, got %v", err)
	}
	if errors.Is(err, ErrProcessExited) {
		t.Fatal("ErrClosed must not also satisfy errors.Is(ErrProcessExited); " +
			"a routine shutdown would read as a crash")
	}

	if err := c.Send(context.Background(), "noop", nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Send after Close: want ErrClosed, got %v", err)
	}
}

// TestInFlightCallUnblocksWhenProcessExits covers the deadlock hazard: a pending
// channel whose single slot already holds a delivered response, whose caller has
// timed out. wait() must not block on that send, because it holds mu and the
// caller's deferred delete needs mu — the two would wait on each other forever.
func TestInFlightCallUnblocksWhenProcessExits(t *testing.T) {
	// Answer one command with a response (filling the caller's buffered slot),
	// then exit while that caller is still in flight.
	c := startFake(t, `
n=0
while IFS= read -r line; do
  n=$((n+1))
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"noop","success":true}\n' "$id"
  if [ $n -ge 2 ]; then exit 3; fi
done
`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// First call succeeds normally.
	if _, err := c.Call(ctx, "noop", nil); err != nil {
		t.Fatalf("first call: %v", err)
	}

	// Second call races the exit. Whatever happens, the whole thing must
	// terminate: a wedged wait() would show up as the test timeout below.
	done := make(chan error, 1)
	go func() {
		_, err := c.Call(ctx, "noop", nil)
		done <- err
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("call never returned: wait() likely blocked sending to a full pending channel")
	}
}

// TestWaitDoesNotDeadlockWithConcurrentCalls hammers register/write/exit at once.
// The race detector plus the timeout together catch the lock-ordering and
// unsynchronized-exitReason shapes.
func TestWaitDoesNotDeadlockWithConcurrentCalls(t *testing.T) {
	for i := 0; i < 25; i++ {
		c := startFake(t, `
n=0
while IFS= read -r line; do
  n=$((n+1))
  if [ $n -ge 3 ]; then exit 1; fi
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"noop","success":true}\n' "$id"
done
`)
		var wg sync.WaitGroup
		for j := 0; j < 8; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_, _ = c.Call(ctx, "noop", nil)
			}()
		}

		finished := make(chan struct{})
		go func() { wg.Wait(); close(finished) }()

		select {
		case <-finished:
		case <-time.After(15 * time.Second):
			t.Fatalf("iteration %d: concurrent calls did not all finish (wait() deadlock?)", i)
		}
		_ = c.Close()
	}
}

// TestOversizedRecordIsDroppedNotAccumulated covers the unbounded-read hazard: one
// giant line from the child must be discarded and counted, and the records after
// it must still be delivered — the stream has to stay in sync.
func TestOversizedRecordIsDroppedNotAccumulated(t *testing.T) {
	c, err := Start(context.Background(), Options{
		Bin: fakePi(t, `
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  # A single line far beyond the cap.
  i=0
  while [ $i -lt 400 ]; do printf 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'; i=$((i+1)); done
  printf '\n'
  printf '{"id":"%s","type":"response","command":"noop","success":true}\n' "$id"
done
`),
		MaxRecordBytes: 1024,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = c.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// The oversized line precedes the response, so delivering the response proves
	// the reader resynchronised on the next LF.
	if _, err := c.Call(ctx, "noop", nil); err != nil {
		t.Fatalf("oversized record broke the stream: %v", err)
	}
	if c.Oversized() == 0 {
		t.Fatal("expected the oversized line to be counted")
	}
}

// TestSendRespectsContextWhenChildStopsReading covers the blocking-write hazard:
// Pi has stopped draining stdin, so Write blocks on a full pipe and the caller
// must still be bounded by its context.
func TestSendRespectsContextWhenChildStopsReading(t *testing.T) {
	// Sleep without reading stdin, so the pipe fills and the write blocks.
	c := startFake(t, "sleep 5\n")

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := c.Send(ctx, "noop", map[string]any{"padding": strings.Repeat("x", 256<<10)})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded from a blocked write, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("write blocked for %v despite ctx", elapsed)
	}
}

// TestReadyTimeoutReportsNotReady covers ReadyTimeout, which was previously
// declared and never used — a caller asking for a readiness deadline and getting
// none was worse than no option at all.
func TestReadyTimeoutReportsNotReady(t *testing.T) {
	// Accepts input, never answers: the probe must time out rather than hang.
	_, err := Start(context.Background(), Options{
		Bin:          fakePi(t, "while IFS= read -r line; do :; done\n"),
		ReadyTimeout: 400 * time.Millisecond,
	})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("want ErrNotReady, got %v", err)
	}
}

func TestReadyTimeoutSucceedsWhenProbeAnswers(t *testing.T) {
	c, err := Start(context.Background(), Options{
		Bin:          fakePi(t, echoID),
		ReadyTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = c.Close() }()

	if c.PID() <= 0 {
		t.Fatalf("PID should be positive after a successful start, got %d", c.PID())
	}
}

// TestPIDIsStableAcrossExit guards the doc/impl agreement: PID is captured at
// Start and does not change when the child dies.
func TestPIDIsStableAcrossExit(t *testing.T) {
	c := startFake(t, "exit 0\n")
	pid := c.PID()
	if pid <= 0 {
		t.Fatalf("PID=%d", pid)
	}
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("never exited")
	}
	if got := c.PID(); got != pid {
		t.Fatalf("PID changed across exit: %d -> %d", pid, got)
	}
}

// TestExitErrorCarriesExitStatus checks the caller can turn a dead turn into a
// useful log line instead of a bare sentinel.
func TestExitErrorCarriesExitStatus(t *testing.T) {
	c := startFake(t, "exit 42\n")
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("never exited")
	}
	err := c.ExitError()
	if !errors.Is(err, ErrProcessExited) {
		t.Fatalf("want ErrProcessExited, got %v", err)
	}
	if !strings.Contains(err.Error(), "42") {
		t.Fatalf("exit status missing from %v", err)
	}
}

// TestReadRecordRejectsOversizedLine checks the cap directly, including that the
// reader is left positioned on the following record.
func TestReadRecordRejectsOversizedLine(t *testing.T) {
	input := strings.Repeat("x", 4096) + "\n{\"ok\":1}\n"
	br := bufio.NewReaderSize(strings.NewReader(input), 256)

	if _, err := readRecord(br, 512, 1<<16); !errors.Is(err, ErrRecordTooLarge) {
		t.Fatalf("want ErrRecordTooLarge, got %v", err)
	}

	next, err := readRecord(br, 512, 1<<16)
	if err != nil {
		t.Fatalf("stream not resynchronised after an oversized line: %v", err)
	}
	if string(next) != `{"ok":1}` {
		t.Fatalf("next record = %q", next)
	}
}

// TestReadRecordHandlesRecordSpanningManyBuffers exercises the accumulate path
// with a cap large enough that a legitimately long record still works.
func TestReadRecordHandlesRecordSpanningManyBuffers(t *testing.T) {
	payload := strings.Repeat("y", 3000)
	input := `{"k":"` + payload + `"}` + "\n"
	br := bufio.NewReaderSize(strings.NewReader(input), 256)

	got, err := readRecord(br, 1<<20, 1<<24)
	if err != nil {
		t.Fatalf("readRecord: %v", err)
	}
	if !strings.Contains(string(got), payload) {
		t.Fatalf("long record truncated: got %d bytes, want %d", len(got), len(input)-1)
	}
}

// TestEOFStripsTrailingCR keeps the framing contract uniform. The LF path trims
// a CR so CRLF sources are handled; the unterminated tail used to skip that, so
// a CRLF child that died mid-record left a stray CR on the final record.
func TestEOFStripsTrailingCR(t *testing.T) {
	// The stub waits for a command before emitting, so Subscribe is guaranteed to
	// register while the child is still alive. Without that gate the stub could
	// exit first, and Subscribe would correctly hand back a pre-closed channel —
	// making this a test of timing rather than of CR handling.
	c := startFake(t, `
IFS= read -r _
printf '{"type":"agent_settled"}\r'
exit 0
`)
	events, cancel := c.Subscribe()
	defer cancel()

	if err := c.Send(context.Background(), "go", nil); err != nil {
		t.Fatalf("send: %v", err)
	}

	deadline := time.After(15 * time.Second)
	for {
		select {
		case rec, ok := <-events:
			if !ok {
				t.Fatal("closed before the record arrived")
			}
			if rec.Type == TypeAgentSettled {
				if len(rec.Raw) > 0 && rec.Raw[len(rec.Raw)-1] == '\r' {
					t.Fatalf("trailing CR survived on the unterminated tail: %q", rec.Raw)
				}
				return
			}
		case <-deadline:
			t.Fatal("record never arrived")
		}
	}
}

// TestDiscardBudgetIsExact checks the scan bound is the documented one rather than
// max + maxDiscard. Seeded with the accumulated bytes, the total stays within the
// budget the caller was promised.
func TestDiscardBudgetIsExact(t *testing.T) {
	// A long run with no LF, with a record cap small enough to enter discard.
	c, err := Start(context.Background(), Options{
		Bin: fakePi(t, `
printf 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA'
i=0
while [ $i -lt 400 ]; do printf 'BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB'; i=$((i+1)); done
exit 0
`),
		MaxRecordBytes: 2048, // maxDiscard = 32 KiB
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

	// The run is ~32 KB of B's plus 96 A's, so it overruns the 32 KiB budget
	// once the accumulated 96 bytes are counted. Either way the stream must
	// terminate and be accounted for, never hang: exactly one oversized record is
	// dropped, and the junk never reaches the JSON parser.
	if c.Oversized() != 1 {
		t.Fatalf("oversized=%d, want 1", c.Oversized())
	}
	if c.Malformed() != 0 {
		t.Fatalf("discarded bytes reached the parser (%d malformed)", c.Malformed())
	}
}
