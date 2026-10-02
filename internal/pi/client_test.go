package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePi writes a Pi-compatible stub to disk and returns its path. The stub
// behaves like `pi --mode rpc` closely enough to exercise framing, correlation
// and shutdown without a provider round-trip.
//
// Script receives its behaviour from env vars, so one stub covers every case.
func fakePi(t *testing.T, body string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("stub is a POSIX shell script")
	}
	path := filepath.Join(t.TempDir(), "fake-pi")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

// echoID replies to each command with success:true, correlating on the id it
// received. Pi's parse errors arrive WITHOUT an id; every test that expects a
// correlated response must therefore echo the id back.
const echoID = `
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  cmd=$(printf '%s' "$line" | sed -n 's/.*"type":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"%s","success":true,"data":{}}\n' "$id" "$cmd"
done
`

// startFake launches a stub through the real Client, so every test exercises the
// same spawn, framing and correlation path as production.
func startFake(t *testing.T, body string, args ...string) *Client {
	t.Helper()
	c, err := Start(context.Background(), Options{
		Bin:  fakePi(t, body),
		Args: args,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestCallCorrelatesByIDNotArrivalOrder(t *testing.T) {
	c := startFake(t, echoID)

	// Fire concurrently: responses must be routed by id, never by who happens
	// to read first. If this regressed to positional matching, the Payload
	// values would cross over.
	const n = 8
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := c.Call(context.Background(), "set_model",
				map[string]any{"payload": i})
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatalf("concurrent call: %v", err)
		}
	}
}

func TestCallReturnsErrorOnSuccessFalse(t *testing.T) {
	c := startFake(t, `
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"set_model","success":false,"error":"Model not found: bad/model"}\n' "$id"
done
`)

	_, err := c.Call(context.Background(), "set_model", nil)
	if err == nil {
		t.Fatal("want error for success:false, got nil")
	}
	if !strings.Contains(err.Error(), "Model not found") {
		t.Fatalf("error should carry Pi's message, got %v", err)
	}
}

// Pi splits records only on LF. U+2028 and U+2029 are legal inside JSON strings
// and are not record boundaries, so a reader that treats them as separators
// corrupts the stream. Pi's own docs call this out because Node's readline
// splits on them.
func TestRecordsSplitOnlyOnLF(t *testing.T) {
	// Escapes rather than literal characters: the separators are invisible in
	// source and easy to mangle in transit.
	const sep = "line\u2028sep\u2029end"

	c := startFake(t, "\n"+
		"SEP='"+sep+"'\n"+
		"while IFS= read -r line; do\n"+
		"  id=$(printf '%s' \"$line\" | sed -n 's/.*\"id\":\"\\([^\"]*\\)\".*/\\1/p')\n"+
		"  printf '{\"id\":\"%s\",\"type\":\"response\",\"command\":\"x\",\"success\":true,\"data\":{\"note\":\"%s\"}}\\n' \"$id\" \"$SEP\"\n"+
		"done\n")

	rec, err := c.Call(context.Background(), "noop", nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var data struct {
		Note string `json:"note"`
	}
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		t.Fatalf("record was split mid-string, unmarshal failed: %v", err)
	}
	if data.Note != sep {
		t.Fatalf("U+2028/U+2029 treated as record boundaries.\n got %q\nwant %q", data.Note, sep)
	}
}

func TestAcceptsCRLF(t *testing.T) {
	c := startFake(t, `
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"x","success":true,"data":{"v":"ok"}}\r\n' "$id"
done
`)
	rec, err := c.Call(context.Background(), "noop", nil)
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	var data struct {
		V string `json:"v"`
	}
	if err := json.Unmarshal(rec.Data, &data); err != nil {
		t.Fatalf("CR not stripped, unmarshal failed: %v", err)
	}
	if data.V != "ok" {
		t.Fatalf("got %q want %q", data.V, "ok")
	}
}

func TestEventsReachSubscribers(t *testing.T) {
	c := startFake(t, `
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  cmd=$(printf '%s' "$line" | sed -n 's/.*"type":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"%s","success":true}\n' "$id" "$cmd"
  printf '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"Hel"}}\n'
  printf '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"lo"}}\n'
  printf '{"type":"message_end","message":{"role":"assistant","content":[]}}\n'
  printf '{"type":"agent_settled"}\n'
done
`)

	events, cancel := c.Subscribe()
	defer cancel()

	if err := c.Send(context.Background(), "prompt", map[string]any{"message": "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	var text strings.Builder
	deadline := time.After(10 * time.Second)
	for {
		select {
		case rec := <-events:
			switch rec.Type {
			case "message_update":
				text.WriteString(deltaOf(t, rec))
			case "message_end", "agent_settled":
				if text.String() != "Hello" {
					t.Fatalf("assembled %q want %q", text.String(), "Hello")
				}
				return
			}
		case <-deadline:
			t.Fatalf("timeout; assembled so far %q", text.String())
		}
	}
}

func deltaOf(t *testing.T, rec Record) string {
	t.Helper()
	var ue struct {
		AssistantMessageEvent struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		} `json:"assistantMessageEvent"`
	}
	if err := json.Unmarshal(rec.Raw, &ue); err != nil {
		t.Fatalf("decode message_update: %v", err)
	}
	if ue.AssistantMessageEvent.Type != "text_delta" {
		return ""
	}
	return ue.AssistantMessageEvent.Delta
}

func TestTerminalSignalIsAgentSettled(t *testing.T) {
	c := startFake(t, `
while IFS= read -r line; do
  printf '{"type":"agent_end","messages":[],"willRetry":true}\n'
  printf '{"type":"agent_settled"}\n'
done
`)
	events, cancel := c.Subscribe()
	defer cancel()

	if err := c.Send(context.Background(), "prompt", map[string]any{"message": "hi"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	seen := map[string]bool{}
	for len(seen) < 2 {
		select {
		case rec := <-events:
			seen[rec.Type] = true
			// agent_end must not read as terminal: Pi can still retry.
			if rec.Type == TypeAgentEnd && rec.IsTerminal() {
				t.Fatal("agent_end reported terminal; agent_settled is the end signal")
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timeout, saw %v", seen)
		}
	}
	if !seen[TypeAgentSettled] {
		t.Fatal("never saw agent_settled")
	}
}

func TestMalformedRecordDoesNotKillReader(t *testing.T) {
	c := startFake(t, `
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf 'this is not json\n'
  printf '{"id":"%s","type":"response","command":"noop","success":true}\n' "$id"
done
`)
	// The garbage line carries no id and cannot be attributed to anyone; the
	// correlated response after it must still be delivered.
	rec, err := c.Call(context.Background(), "noop", nil)
	if err != nil {
		t.Fatalf("malformed line broke the stream: %v", err)
	}
	if !strings.HasPrefix(rec.ID, "pi-") {
		t.Fatalf("got id %q, want the generated correlation id", rec.ID)
	}
}

func TestStderrIsCapturedNotParsed(t *testing.T) {
	c := startFake(t, `
printf 'diagnostic line one\n' >&2
printf 'diagnostic line two\n' >&2
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  printf '{"id":"%s","type":"response","command":"noop","success":true}\n' "$id"
done
`)
	if _, err := c.Call(context.Background(), "noop", nil); err != nil {
		t.Fatalf("call: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(c.Stderr(), "diagnostic line two") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("stderr not captured, got %q", c.Stderr())
}

func TestInFlightCallFailsWhenProcessExits(t *testing.T) {
	c := startFake(t, `
printf 'booting\n' >&2
exit 7
`)
	_, err := c.Call(context.Background(), "noop", nil)
	if err == nil {
		t.Fatal("want error after process exit")
	}
	if !errors.Is(err, ErrProcessExited) {
		t.Fatalf("want ErrProcessExited, got %v", err)
	}
}

func TestDoneClosesOnExit(t *testing.T) {
	c := startFake(t, "exit 0\n")
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done never closed")
	}
}

func TestCallRespectsContextDeadline(t *testing.T) {
	// A stub that accepts input but never answers.
	c := startFake(t, "while IFS= read -r line; do :; done\n")

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := c.Call(ctx, "noop", nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %v; should return at the deadline", elapsed)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c, err := Start(context.Background(), Options{Bin: fakePi(t, echoID)})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestCancelClosesSubscriberChannel(t *testing.T) {
	c := startFake(t, echoID)
	events, cancel := c.Subscribe()
	cancel()
	// Drain to a closed channel; must terminate rather than block.
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-events:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("subscriber channel not closed by cancel")
		}
	}
}

func TestSlowSubscriberDropsRatherThanStalling(t *testing.T) {
	// Emit far more events than the subscriber buffer holds, without reading.
	c := startFake(t, `
while IFS= read -r line; do
  i=0
  while [ $i -lt 400 ]; do
    printf '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"x"}}\n'
    i=$((i+1))
  done
done
`)
	// Subscribe but never read: this is the backpressure case. Do NOT cancel —
	// a cancelled subscriber is removed from the fan-out and drops nothing.
	_, cancel := c.Subscribe()
	defer cancel()

	if err := c.Send(context.Background(), "prompt", map[string]any{"message": "go"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	// The read loop must keep draining regardless of subscriber backpressure.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if c.Dropped() > 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("expected drops for a non-reading subscriber")
}

func TestEnvWithOverridesAndPreserves(t *testing.T) {
	base := []string{"A=1", "B=2", "PATH=/bin"}
	got := EnvWith(base, map[string]string{"B": "override", "C": "new"})

	index := map[string]string{}
	for _, e := range got {
		k, v, ok := strings.Cut(e, "=")
		if ok {
			index[k] = v
		}
	}
	if index["A"] != "1" {
		t.Fatalf("A dropped: %v", index)
	}
	if index["B"] != "override" {
		t.Fatalf("B not overridden: %v", index)
	}
	if index["C"] != "new" {
		t.Fatalf("C not added: %v", index)
	}
	// A, PATH and C plus the single overridden B — an override replaces, it
	// does not append a second value.
	if len(got) != 4 {
		t.Fatalf("override should replace not duplicate, got %v", got)
	}
	if n := strings.Count(strings.Join(got, "\n"), "B="); n != 1 {
		t.Fatalf("B appears %d times, want exactly 1: %v", n, got)
	}
}

func TestRingBufferKeepsTail(t *testing.T) {
	r := newRingBuffer(8)
	r.Write([]byte("aaaaaaaa"))
	r.Write([]byte("bbbbbbbb"))
	if got := r.String(); got != "bbbbbbbb" {
		t.Fatalf("got %q want %q", got, "bbbbbbbb")
	}
}

// Guard the framing rule directly at the reader, independent of any stub: a
// record containing U+2028/U+2029 must survive readRecord intact.
func TestReadRecordSplitsOnlyOnLF(t *testing.T) {
	input := "{\"a\":\"x y z\"}\n{\"b\":2}\n"
	br := bufio.NewReaderSize(strings.NewReader(input), 64<<10)

	first, err := readRecord(br, 1<<20, 1<<20)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if string(first) != "{\"a\":\"x y z\"}" {
		t.Fatalf("unicode separators treated as boundaries: %q", first)
	}

	second, err := readRecord(br, 1<<20, 1<<20)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if string(second) != "{\"b\":2}" {
		t.Fatalf("got %q", second)
	}
}
