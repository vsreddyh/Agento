// Package pi drives a Pi agent process over its RPC mode.
//
// Pi is a stdio program: `pi --mode rpc` reads JSON command records on stdin and
// writes JSON response and event records on stdout, both strictly JSONL. This
// package is the Go side of that protocol — process supervision, framing, and
// command/response correlation — with no HTTP and no model semantics. Keeping it
// separate means the part that must not lose a byte of the stream can be tested
// on its own.
//
// Two framing rules from Pi's protocol docs are load-bearing here:
//
//   - Records are split ONLY on LF. U+2028 and U+2029 are valid inside JSON
//     strings and are not record boundaries, so a reader that treats them as
//     separators corrupts the stream. Go's line splitting does not, but any
//     replacement must.
//   - A preceding CR is stripped so CRLF input is accepted.
//
// Stdout carries protocol records only; diagnostics arrive on stderr and are
// captured as text, never parsed. A caller that stops draining stdout stalls the
// child once the pipe buffer fills, so the read loop runs for the child's whole
// lifetime.
package pi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Record is one JSONL record read from Pi's stdout.
//
// A record is either a command response (Type == TypeResponse) or a session
// event. Event payloads vary widely by type, so Raw always holds the original
// bytes and typed accessors are provided for the common ones.
type Record struct {
	Type string `json:"type"`

	// Set on responses that correlate to a command we sent.
	ID      string `json:"id,omitempty"`
	Command string `json:"command,omitempty"`

	// Success is a pointer because the field is absent on session events, and a
	// bare bool would report absent as false.
	Success *bool  `json:"success,omitempty"`
	Error   string `json:"error,omitempty"`

	Data json.RawMessage `json:"data,omitempty"`

	// Raw is the record exactly as received, for consumers decoding a specific
	// event type.
	Raw json.RawMessage `json:"-"`
}

// TypeResponse is Record.Type for a command response.
const TypeResponse = "response"

// Event types this package treats as significant for stream lifecycle. The full
// set is documented in Pi's json.md; only the ones a client must react to are
// named here.
const (
	// TypeAgentSettled means Pi has no further automatic work for the current
	// session-level run — the correct end-of-turn signal for a client. TypeAgentEnd
	// is NOT: retries, compaction recovery and queued messages can follow it.
	TypeAgentSettled = "agent_settled"
	// TypeAgentEnd closes one low-level agent run.
	TypeAgentEnd = "agent_end"
)

// IsTerminal reports whether r ends the current session-level run.
func (r Record) IsTerminal() bool { return r.Type == TypeAgentSettled }

var (
	// ErrProcessExited reports that the child process is gone. Any command that
	// was in flight when it died fails with this.
	ErrProcessExited = errors.New("pi: process exited")

	// ErrClosed reports that the Client was closed by its owner. The child may
	// well still be alive — Close only closes its stdin — so this is deliberately
	// distinct from ErrProcessExited, which would make a routine shutdown look
	// like a crash.
	ErrClosed = errors.New("pi: client closed")

	// ErrNotReady reports that Start's readiness probe did not answer in time.
	ErrNotReady = errors.New("pi: not ready")

	// ErrRecordTooLarge reports a JSONL record longer than the configured cap.
	// The offending line is dropped and counted; the stream survives.
	ErrRecordTooLarge = errors.New("pi: record too large")
)

// Options configure a client.
type Options struct {
	// Path to the pi executable. Defaults to "pi" on PATH.
	Bin string

	// Dir is the working directory for the child. Pi loads AGENTS.md and skills
	// from here, so this is what selects a profile's personality.
	Dir string

	// Args are passed after the fixed RPC arguments.
	Args []string

	// Env replaces the child's environment when non-nil. When nil the parent
	// environment is inherited.
	//
	// A non-nil Env REPLACES everything, it does not merge: setting it to just
	// {"FOO=bar"} leaves the child with no PATH and exec fails in a way that
	// looks nothing like the cause. Build on the inherited environment instead:
	//
	//	Env: pi.EnvWith(pi.EnvironOrOs(os.Environ()), map[string]string{"FOO": "bar"})
	//
	// or leave it nil when only Pi's own config needs to change.
	Env []string

	// SubscribeBuffer is the per-subscriber event channel depth. Slow
	// subscribers drop records rather than stalling the read loop; dropped count
	// is reported by Dropped. Zero selects the default.
	SubscribeBuffer int

	// StderrLimit caps retained stderr bytes. Older output is dropped. Zero
	// selects the default.
	StderrLimit int

	// MaxRecordBytes caps a single JSONL record. A line beyond this is dropped
	// and counted rather than accumulated, so one malformed or hostile line from
	// the child cannot exhaust gateway memory. Zero selects the default.
	MaxRecordBytes int

	// ReadyTimeout, when > 0, makes Start block until the child answers a
	// `get_state` probe, and fail with ErrNotReady if it does not within the
	// deadline. ctx bounds that probe and nothing else: the child is long-lived,
	// so a per-request context must not take it down when it ends. This is how a caller learns the agent is genuinely usable rather
	// than merely spawned. Zero (the default) skips the probe and returns as soon
	// as the process starts.
	ReadyTimeout time.Duration
}

const (
	defaultSubscribeBuffer = 256
	defaultStderrLimit     = 64 << 10
	// 16 MiB is far above any plausible single record — a long tool result is
	// kilobytes — while still bounding the damage from a runaway line.
	defaultMaxRecordBytes = 16 << 20
	// exitGrace bounds how long a failed write waits for the child's exit to be
	// published, so a pipe error is reported as the exit it almost always is.
	exitGrace = 2 * time.Second
	// closeGrace bounds Close's wait for a graceful exit, and again after SIGKILL.
	closeGrace = 10 * time.Second
	// discardMultiple is how far past MaxRecordBytes the reader will chase an LF
	// before declaring the stream unrecoverable. Generous for a legitimate record
	// with no newline in it, far below an endless run.
	discardMultiple = 16
	// maxRecordCeiling caps a caller-supplied MaxRecordBytes, keeping
	// maxRec*discardMultiple clear of overflow.
	maxRecordCeiling = 64 << 20
)

// Client is a running Pi process driven over RPC.
//
// A Client is safe for concurrent use. Commands may be issued from many
// goroutines; responses are correlated by command id, never by arrival order,
// because Pi handles commands asynchronously.
type Client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	// writeMu serialises record writes so interleaved goroutines cannot splice
	// partial JSON into one line.
	writeMu sync.Mutex

	// mu guards pending, closed and pid.
	mu      sync.Mutex
	pending map[string]chan *Record
	closed  bool

	seq atomic.Uint64

	subsMu sync.Mutex
	subs   map[int]chan Record
	subSeq int
	subBuf int

	stderrMu  sync.Mutex
	stderrBuf *ringBuffer

	// maxRecord bounds a single line off stdout.
	maxRecord int
	// maxDiscard bounds how far the reader will chase a LF before declaring the
	// stream unrecoverable. Derived from maxRecord: a run of bytes this far past
	// the record limit is garbage, not a record.
	maxDiscard int

	done chan struct{}
	// readDone is closed once the stdout reader has drained to EOF, so wait() can
	// dispatch trailing records before closing subscribers.
	readDone chan struct{}

	// dropped counts events discarded because a subscriber was too slow.
	dropped atomic.Uint64
	// oversized counts lines rejected for exceeding maxRecord.
	oversized atomic.Uint64
	// malformed counts lines that were not valid JSON. Non-zero means Pi's
	// output and this parser have diverged, which is otherwise silent.
	malformed atomic.Uint64
	// lateResponses counts responses whose correlation id nobody was waiting on
	// — a caller that timed out, most often. Non-zero is normal under load but
	// a useful signal when diagnosing slow turns.
	lateResponses atomic.Uint64
	// subsClosed records that the subscriber set has been closed, so Subscribe
	// can hand back an already-closed channel instead of registering into a set
	// nothing will ever close again.
	subsClosed bool
	// desync records that the stdout stream became unreadable and the child was
	// killed as a result.
	desync atomic.Bool
	// pid is cached at Start; cmd.Process is not safe to touch after exit.
	pid int

	// exitReason is the child's exit error, written by wait() under mu before it
	// signals any waiter. Guarded by mu rather than by done, because exitErr may
	// be called before the process has exited.
	exitReason error
}

// Start launches pi in RPC mode and returns a client bound to it.
//
// By default Start returns as soon as the process is spawned. Set
// Options.ReadyTimeout to wait for the agent to actually answer a command first.
func Start(ctx context.Context, opts Options) (*Client, error) {
	// Same guard as Call/Send: the readiness probe below feeds ctx straight into
	// context.WithTimeout, which panics on nil.
	if ctx == nil {
		ctx = context.Background()
	}
	// ctx otherwise bounds only the readiness probe — the child is long-lived and
	// outlives any single request — but an already-cancelled context should not
	// spawn a process at all.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("pi: start: %w", err)
	}
	bin := opts.Bin
	if bin == "" {
		bin = "pi"
	}
	// --mode is reserved. Allowing it in Args would let a caller silently switch
	// the child out of RPC mode, after which every record read here is nonsense.
	// Normalised first so "-mode", "--mode" and either "=" form are all caught;
	// matching the literal forms left "-mode=x" through.
	for _, a := range opts.Args {
		// Only flags can select a mode. Without the leading-dash check a bare
		// positional "mode" was rejected as though it were the flag.
		if !strings.HasPrefix(a, "-") {
			continue
		}
		flag := strings.TrimLeft(a, "-")
		if flag == "mode" || strings.HasPrefix(flag, "mode=") {
			return nil, fmt.Errorf("pi: %q is reserved by this client; it is always rpc", a)
		}
	}
	args := append([]string{"--mode", "rpc"}, opts.Args...)

	cmd := exec.Command(bin, args...)
	cmd.Dir = opts.Dir
	if opts.Env != nil {
		cmd.Env = opts.Env
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pi: stdin pipe: %w", err)
	}

	// stdout/stderr are self-managed pipes rather than cmd.StdoutPipe/StderrPipe
	// on purpose. exec closes those on Wait, and the documented rule is that it
	// is "incorrect to call Wait before all reads from the pipe have completed" —
	// but Wait is how we learn the child exited. Owning the pipe means Wait never
	// touches our read end, so the reader drains to EOF and no trailing record is
	// lost when the child dies mid-turn.
	outR, outW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("pi: stdout pipe: %w", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return nil, fmt.Errorf("pi: stderr pipe: %w", err)
	}
	cmd.Stdout = outW
	cmd.Stderr = errW

	if err := cmd.Start(); err != nil {
		// StdinPipe allocated its own pipe, so it has to be released here too —
		// it is not one of the two os.Pipe pairs above.
		_ = stdin.Close()
		_ = outR.Close()
		_ = outW.Close()
		_ = errR.Close()
		_ = errW.Close()
		return nil, fmt.Errorf("pi: start %s: %w", bin, err)
	}
	// The child holds the write ends; drop ours so EOF is seen when it exits.
	outW.Close()
	errW.Close()
	stdout, stderr := outR, errR

	limit := opts.StderrLimit
	if limit <= 0 {
		limit = defaultStderrLimit
	}
	subBuf := opts.SubscribeBuffer
	if subBuf <= 0 {
		subBuf = defaultSubscribeBuffer
	}
	// Clamped, not reset: MaxRecordBytes is caller-supplied, and maxRec*16 wraps
	// for a value near MaxInt, which would leave the discard budget negative and
	// silently disable the desync bound. A caller asking for more than the ceiling
	// gets the ceiling rather than the default — silently handing back LESS than
	// requested would make a legitimate large-record setting fail closed with no
	// indication why.
	maxRec := opts.MaxRecordBytes
	if maxRec <= 0 {
		maxRec = defaultMaxRecordBytes
	}
	if maxRec > maxRecordCeiling {
		maxRec = maxRecordCeiling
	}

	c := &Client{
		cmd:        cmd,
		stdin:      stdin,
		stdout:     stdout,
		stderr:     stderr,
		pending:    make(map[string]chan *Record),
		subs:       make(map[int]chan Record),
		subBuf:     subBuf,
		stderrBuf:  newRingBuffer(limit),
		maxRecord:  maxRec,
		maxDiscard: maxRec * discardMultiple,
		done:       make(chan struct{}),
		readDone:   make(chan struct{}),
		pid:        cmd.Process.Pid,
	}

	go c.readLoop(stdout)
	go c.drainStderr(stderr)
	go c.wait()

	if opts.ReadyTimeout > 0 {
		readyCtx, cancel := context.WithTimeout(ctx, opts.ReadyTimeout)
		defer cancel()
		if _, err := c.Call(readyCtx, "get_state", nil); err != nil {
			// Snapshot BEFORE Close: Close waits for the child to exit, not for
			// drainStderr to see EOF, so reading stderr afterwards can miss the
			// trailing diagnostics — which are the ones that explain the failure.
			stderr := c.Stderr()
			_ = c.Close()
			return nil, fmt.Errorf("%w: %w (stderr: %s)", ErrNotReady, err, stderr)
		}
	}

	return c, nil
}

// PID reports the child process id, captured at Start. It does not change when
// the process exits.
func (c *Client) PID() int { return c.pid }

// Done is closed when the child process exits.
func (c *Client) Done() <-chan struct{} { return c.done }

// Dropped reports how many event DELIVERIES were discarded because a subscriber
// was not keeping up — one drop per subscriber that missed a given record, so
// three slow subscribers cost three. Non-zero means a consumer is missing events.
func (c *Client) Dropped() uint64 { return c.dropped.Load() }

// Oversized reports how many stdout lines were dropped for exceeding
// MaxRecordBytes. Non-zero means the child emitted something unparseably large.
func (c *Client) Oversized() uint64 { return c.oversized.Load() }

// Malformed reports how many stdout lines were not valid JSON. Non-zero means Pi's
// output and this parser have diverged.
func (c *Client) Malformed() uint64 { return c.malformed.Load() }

// LateResponses reports how many responses arrived for a correlation id nobody
// was waiting on, i.e. the caller had already timed out.
func (c *Client) LateResponses() uint64 { return c.lateResponses.Load() }

// Desynchronised reports whether the stdout stream became unreadable (an
// unrecoverable run with no LF), in which case the child was killed and every
// in-flight call failed with ErrProcessExited.
func (c *Client) Desynchronised() bool { return c.desync.Load() }

// Call sends a command and waits for its correlated response.
//
// A response with Success false is returned as an error, so a caller that only
// cares whether the command worked can ignore the record.
//
// On context.DeadlineExceeded or Canceled the command may still have been
// delivered: once a record is on the pipe it cannot be recalled, and a select
// racing two ready channels may report the cancellation even though the write
// completed. A caller that retries should treat a cancelled Call as "possibly
// applied" rather than "not applied" — retrying a prompt would otherwise double
// it.
func (c *Client) Call(ctx context.Context, command string, payload map[string]any) (*Record, error) {
	// A nil context would panic on ctx.Done(). Defaulting is friendlier than
	// documenting "must be non-nil", because a future call site will get it wrong.
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.closedErr(); err != nil {
		return nil, err
	}

	id := fmt.Sprintf("pi-%d", c.seq.Add(1))
	ch := make(chan *Record, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.pending[id] = ch
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
	}()

	if err := c.write(ctx, command, id, payload); err != nil {
		return nil, err
	}

	select {
	case r := <-ch:
		// A nil record is how wait() signals the child died with this command
		// still in flight. It must not read as success.
		if r == nil {
			return nil, c.exitErr()
		}
		if r.Success != nil && !*r.Success {
			return r, fmt.Errorf("pi: %s: %s", command, r.Error)
		}
		return r, nil
	case <-ctx.Done():
		// Go picks randomly among ready select cases, so a deadline firing in the
		// same instant a response arrives can win. Stop accepting a response for
		// this id, then look at what is already queued: deliverResponse sends
		// non-blockingly, so a response can already be sitting here.
		//
		// If it is, the command genuinely completed, so hand it back rather than
		// reporting a failure the caller would then retry — a retried prompt
		// double-sends. Only a response that arrives after this point is late.
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		select {
		case rec := <-ch:
			if rec != nil {
				if rec.Success != nil && !*rec.Success {
					return rec, fmt.Errorf("pi: %s: %s", command, rec.Error)
				}
				return rec, nil
			}
		default:
		}
		return nil, ctx.Err()
	case <-c.done:
		// Same reasoning as the ctx branch: Go picks randomly among ready cases,
		// so a response queued just before the child exited can lose the race and
		// be reported as a crash even though the command succeeded.
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		select {
		case rec := <-ch:
			if rec != nil {
				if rec.Success != nil && !*rec.Success {
					return rec, fmt.Errorf("pi: %s: %s", command, rec.Error)
				}
				return rec, nil
			}
		default:
		}
		return nil, c.exitErr()
	}
}

// Send writes a command without waiting for its response. Use for commands whose
// outcome arrives as events rather than a response.
//
// The write can block if the child has stopped draining stdin, so ctx bounds the
// wait for pipe space; a cancelled ctx returns without writing — unless the write
// had already started, in which case it is delivered and a retry would duplicate
// it. Same caveat as Call.
func (c *Client) Send(ctx context.Context, command string, payload map[string]any) error {
	return c.writeCtx(ctx, command, "", payload)
}

// ExitError reports why the child exited, or nil while it is still running.
//
// Deliberately honest about the live case: a health check calling this on a
// healthy client must get nil, not a sentinel that reads as a crash.
func (c *Client) ExitError() error {
	select {
	case <-c.done:
	default:
		return nil
	}
	return c.exitErr()
}

func (c *Client) closedErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	return nil
}

// writeCtx marshals a command and writes it as one LF-terminated line.
//
// Everything that can block happens on a helper goroutine so ctx bounds all of
// it: both the write itself and the wait for writeMu. Taking writeMu on the
// caller's goroutine would defeat the deadline entirely — a second caller would
// sit on the mutex behind a stalled first write with no way to abandon it.
//
// A write that has not started by the time ctx fires is abandoned rather than
// sent. It cannot be un-sent once it reaches the pipe, but skipping it covers the
// common case of a command queued behind another write, where a retry would
// otherwise be preceded by a command the caller already gave up on.
//
// Goroutines: one helper goroutine per in-flight write, blocked on writeMu or on
// the pipe. A child that stops draining therefore accumulates at most one
// goroutine per outstanding Call or Send, all released when it exits or when
// Close closes stdin. That is bounded by concurrent requests, which is what a
// gateway should already be limiting — a retry storm with no in-flight cap would
// turn into a goroutine pile-up here.
func (c *Client) writeCtx(ctx context.Context, command, id string, payload map[string]any) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.closedErr(); err != nil {
		return err
	}

	rec := make(map[string]any, len(payload)+2)
	for k, v := range payload {
		rec[k] = v
	}
	rec["type"] = command
	if id != "" {
		rec["id"] = id
	} else {
		// Never let a caller's payload smuggle in a correlation id: a response
		// carrying an id nobody registered would be dropped as a late reply.
		delete(rec, "id")
	}

	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("pi: marshal %s: %w", command, err)
	}
	line = append(line, '\n')

	type result struct{ err error }
	done := make(chan result, 1)

	var abandoned atomic.Bool
	go func() {
		c.writeMu.Lock()
		defer c.writeMu.Unlock()

		if abandoned.Load() {
			// The caller went away while this write was queued. Sending now would
			// put a cancelled command on the wire ahead of whatever it retries.
			done <- result{}
			return
		}
		_, err := c.stdin.Write(line)
		done <- result{err}
	}()

	select {
	case <-ctx.Done():
		// Go picks randomly among ready cases, so a write that completed in the
		// same instant can lose the race and be reported as a failure it is not.
		// A delivered command that reads as failed invites a retry, and retrying a
		// prompt double-sends it — so check for the result before giving up.
		select {
		case res := <-done:
			abandoned.Store(true)
			return res.err
		default:
		}
		abandoned.Store(true)
		return ctx.Err()
	case <-c.done:
		select {
		case res := <-done:
			abandoned.Store(true)
			return res.err
		default:
		}
		return c.exitErr()
	case res := <-done:
		if res.err != nil {
			// A write to the child's stdin fails as soon as the child exits, which
			// is slightly BEFORE wait() closes done. Reporting the raw pipe error
			// ("file already closed" / EPIPE) would bury the actual cause, so give
			// the exit a brief moment to be published and prefer that.
			select {
			case <-c.done:
				return c.exitErr()
			case <-time.After(exitGrace):
			}
			// Close can win the race against a write that already passed the
			// closedErr check, closing stdin while the child is still alive. That
			// is a shutdown, not a crash, so it must not surface as a pipe error.
			if err := c.closedErr(); err != nil {
				return err
			}
			return fmt.Errorf("pi: write %s: %w", command, res.err)
		}
		return nil
	}
}

func (c *Client) write(ctx context.Context, command, id string, payload map[string]any) error {
	return c.writeCtx(ctx, command, id, payload)
}

// Subscribe registers a listener for session events and returns the channel plus
// a cancel function.
//
// Session events carry no command id, so they are broadcast to every subscriber.
// If a subscriber's buffer fills, further records are dropped for that subscriber
// rather than blocking the read loop — a stalled HTTP client must not be able to
// wedge the agent. Dropped records are counted by Dropped.
func (c *Client) Subscribe() (<-chan Record, func()) {
	ch := make(chan Record, c.subBuf)

	c.subsMu.Lock()
	if c.subsClosed {
		// The child is gone. Returning a pre-closed channel means a consumer
		// ranging over it finishes immediately instead of waiting forever on a
		// set that will never be closed again.
		c.subsMu.Unlock()
		close(ch)
		return ch, func() {}
	}
	id := c.subSeq
	c.subSeq++
	c.subs[id] = ch
	c.subsMu.Unlock()

	return ch, func() {
		c.subsMu.Lock()
		if existing, ok := c.subs[id]; ok {
			delete(c.subs, id)
			close(existing)
		}
		c.subsMu.Unlock()
	}
}

// Stderr returns the tail of the child's stderr. Diagnostics only — this is
// never protocol data.
func (c *Client) Stderr() string {
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	return c.stderrBuf.String()
}

// Close shuts the client down: closes stdin so Pi can dispose its runtime and
// exit, then waits briefly for it to do so, escalating to a kill if it does not.
// Close is idempotent.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	_ = c.stdin.Close()

	select {
	case <-c.done:
	case <-time.After(closeGrace):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		// Bounded even after SIGKILL: an unkillable child (D-state on an
		// uninterruptible mount, say) must not hang Close forever.
		select {
		case <-c.done:
		case <-time.After(closeGrace):
		}
	}

	// Backstop for the readers: if the child died without them seeing EOF, their
	// descriptors would otherwise linger until the finalizer runs.
	if c.stdout != nil {
		_ = c.stdout.Close()
	}
	if c.stderr != nil {
		_ = c.stderr.Close()
	}

	c.closeSubscribers()
	return nil
}

// readLoop consumes stdout for the child's lifetime. It must not exit early: a
// reader that stops drains-nothing fills the pipe buffer and stalls Pi.
func (c *Client) readLoop(r io.Reader) {
	// Close the read end as soon as it is done with. These are self-managed
	// pipes, so nothing else ever closes them: without this the descriptors sit
	// open until the finalizer runs.
	defer func() {
		if c.stdout != nil {
			_ = c.stdout.Close()
		}
		close(c.readDone)
	}()

	br := bufio.NewReaderSize(r, 64<<10)

	for {
		line, err := readRecord(br, c.maxRecord, c.maxDiscard)
		switch {
		case errors.Is(err, ErrRecordTooLarge):
			// The oversized line was already resynchronised past its LF, so the
			// stream is intact — count it and keep reading.
			c.oversized.Add(1)
			continue
		case errors.Is(err, errStreamDesync):
			// Unbounded run with no LF. Bailing here would leave the reader inside
			// the bad run and corrupt everything after it, so the stream cannot be
			// recovered: kill the child. wait() then fires, in-flight calls fail
			// with ErrProcessExited and subscribers close. A loud teardown beats a
			// read loop that is technically alive but will never deliver again.
			c.desync.Store(true)
			c.kill()
			return
		case len(line) > 0:
			c.dispatch(line)
		}
		if err != nil {
			return
		}
	}
}

// readRecord reads one LF-terminated record and strips a preceding CR.
//
// A non-nil error means the stream ended; any bytes returned alongside it are a
// final unterminated record. ErrRecordTooLarge means the line was discarded, and
// the stream is still positioned at the next one.
func readRecord(br *bufio.Reader, max, maxDiscard int) ([]byte, error) {
	var acc []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(acc)+len(chunk) > max {
				// Discard until the next LF so the stream stays in sync.
				discarded, derr := discardLine(br, maxDiscard-len(acc))
				// A budget overrun means the stream cannot be realigned at all, so
				// it must keep its own identity: reported as ErrRecordTooLarge the
				// read loop would count it and carry on reading a desynchronised
				// stream instead of tearing the child down.
				if errors.Is(derr, errStreamDesync) {
					return nil, derr
				}
				if !discarded && derr != nil {
					// Nothing was consumed and the stream simply ended.
					return nil, derr
				}
				// Bytes were discarded, so this oversized line counts even if the
				// stream then ended.
				return nil, ErrRecordTooLarge
			}
			acc = append(acc, chunk...)
			continue
		}
		if err != nil {
			// End of stream. The cap has to be applied here too: a child that
			// dumps an enormous unterminated tail on the way out would otherwise
			// be buffered in full and handed to the JSON parser.
			if len(acc)+len(chunk) > max {
				return nil, ErrRecordTooLarge
			}
			// Unterminated tail: no LF to trim, but a CRLF source may still have
			// left a CR. Stripped for the same reason as on the LF path — the
			// framing contract should not have one exception.
			acc = append(acc, chunk...)
			acc = bytes.TrimSuffix(acc, []byte("\r"))
			return acc, err
		}
		acc = append(acc, chunk...)
		if len(acc) > max {
			return nil, ErrRecordTooLarge
		}
		acc = bytes.TrimSuffix(acc, []byte("\n"))
		acc = bytes.TrimSuffix(acc, []byte("\r"))
		return acc, nil
	}
}

// errStreamDesync reports a run of bytes with no LF that is too long to skip
// safely. The reader cannot be realigned, so the stream is over.
var errStreamDesync = errors.New("pi: stream desynchronised")

// discardLine consumes bytes up to and including the next LF, throwing them away.
//
// Discarded chunks are not accumulated, so the scan costs time rather than memory.
// It is still bounded: a child emitting an endless LF-less run would otherwise
// livelock the read loop — memory-safe, but silently dead, with no further records
// and no counter. Bounding it and reporting errStreamDesync lets the caller tear
// down loudly instead.
//
// budget counts the bytes still to be scanned, including those the caller already
// accumulated toward the record limit, so the total scan stays within the
// documented bound instead of quietly exceeding it by the record size.
func discardLine(br *bufio.Reader, budget int) (discarded bool, err error) {
	if budget < 0 {
		budget = 0
	}
	consumed := 0
	for {
		chunk, err := br.ReadSlice('\n')
		consumed += len(chunk)
		if err == nil {
			return true, nil
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			// EOF (or a read error) part-way through the run. Bytes were consumed,
			// so the oversized line was still seen — report that to the caller so
			// it is counted, rather than letting a truncated record vanish from
			// every counter.
			return consumed > 0, err
		}
		if consumed > budget {
			return true, errStreamDesync
		}
	}
}

// kill terminates the child, used when the stream can no longer be trusted.
func (c *Client) kill() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

func (c *Client) dispatch(line []byte) {
	var rec Record
	if err := json.Unmarshal(line, &rec); err != nil {
		// A malformed line has no id to correlate, so it cannot be attributed
		// to a caller. Counted rather than dropped in silence, because a
		// non-zero value means protocol skew worth investigating.
		c.malformed.Add(1)
		return
	}
	rec.Raw = append(json.RawMessage(nil), line...)

	if rec.Type == TypeResponse {
		c.deliverResponse(&rec)
		return
	}
	c.broadcast(rec)
}

func (c *Client) deliverResponse(rec *Record) {
	if rec.ID == "" {
		// Pi reports malformed commands without an id. Nobody is waiting on it;
		// surface it to event subscribers rather than dropping it silently.
		c.broadcast(*rec)
		return
	}
	// The send happens under mu, which is what closes the window: a caller that
	// has already removed its id cannot drain the channel between this lookup and
	// this send, so a response is either found by that drain or counted late. It
	// cannot land in an orphaned buffer that nobody reads and no counter sees.
	// Safe to hold mu because the send is non-blocking.
	c.mu.Lock()
	ch, ok := c.pending[rec.ID]
	if ok {
		select {
		case ch <- rec:
		default:
			// The waiter already moved on (context expired) or already has a
			// record queued. Either way this one is late, and counted so a slow
			// turn can be told apart from a lost one.
			ok = false
		}
	}
	c.mu.Unlock()

	if !ok {
		c.lateResponses.Add(1)
	}
}

func (c *Client) broadcast(rec Record) {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	for _, ch := range c.subs {
		select {
		case ch <- rec:
		default:
			c.dropped.Add(1)
		}
	}
}

func (c *Client) closeSubscribers() {
	c.subsMu.Lock()
	defer c.subsMu.Unlock()
	c.subsClosed = true
	for id, ch := range c.subs {
		delete(c.subs, id)
		close(ch)
	}
}

func (c *Client) drainStderr(r io.Reader) {
	if c.stderr != nil {
		defer func() { _ = c.stderr.Close() }()
	}

	buf := make([]byte, 8<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			c.stderrMu.Lock()
			c.stderrBuf.Write(buf[:n])
			c.stderrMu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (c *Client) wait() {
	err := c.cmd.Wait()

	// Drain stdout before touching subscribers. The child is gone, so its write
	// end is closed and the reader will hit EOF; anything Pi emitted on the way
	// out — the last text delta of a turn — is still in the pipe and must be
	// dispatched, not dropped on the floor by a premature close.
	select {
	case <-c.readDone:
	case <-time.After(closeGrace):
	}

	c.mu.Lock()
	// Published before the pending channels are signalled: a caller woken by the
	// nil immediately reads the exit reason, and it must already be visible.
	c.exitReason = err
	// Non-blocking: a pending channel may already hold a delivered response
	// while its caller has timed out but not yet run its deferred delete.
	// Blocking here would wedge this goroutine holding mu, and the caller's
	// deferred delete needs mu to unblock in turn.
	for id, ch := range c.pending {
		select {
		case ch <- nil:
		default:
		}
		delete(c.pending, id)
	}
	c.mu.Unlock()

	// Subscribers must be closed here too, not only in Close: an event loop
	// over a crashed Pi would otherwise wait on a channel nothing will ever write
	// to again, and hang until its own deadline instead of seeing the close.
	c.closeSubscribers()

	close(c.done)
}

// exitErr reports the process exit condition, distinguishing a crash from an exit
// code. Safe to call speculatively: exitReason is only ever written by wait()
// under mu, and is published before any waiter is signalled.
func (c *Client) exitErr() error {
	c.mu.Lock()
	reason := c.exitReason
	closed := c.closed
	c.mu.Unlock()

	// Close wins over the exit reason. When the owner called Close, any exit —
	// including Close's own SIGKILL of a hung child — is a shutdown it asked for,
	// and reporting it as a crash sends an operator hunting a fault that is not
	// there. The reason is still worth having, so it is folded into the message.
	if closed {
		if reason != nil {
			return fmt.Errorf("%w (%v)", ErrClosed, reason)
		}
		return ErrClosed
	}
	if reason != nil {
		return fmt.Errorf("%w: %v", ErrProcessExited, reason)
	}
	return ErrProcessExited
}

// ringBuffer is a fixed-capacity byte buffer that keeps the most recent writes.
// Used for stderr, where only the tail matters.
type ringBuffer struct {
	buf   []byte
	limit int
}

func newRingBuffer(limit int) *ringBuffer {
	if limit <= 0 {
		limit = defaultStderrLimit
	}
	return &ringBuffer{buf: make([]byte, 0, limit), limit: limit}
}

// Write appends p, keeping only the most recent limit bytes.
//
// The trim copies rather than reslicing. Reslicing forward would keep a pointer
// into the middle of the old backing array, so the whole allocation stays
// reachable by the GC while len() cheerfully reports the cap — a chatty child
// emitting 100 MiB of stderr would pin 100 MiB for a 64 KiB buffer.
func (r *ringBuffer) Write(p []byte) {
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.limit {
		trimmed := make([]byte, r.limit)
		copy(trimmed, r.buf[len(r.buf)-r.limit:])
		r.buf = trimmed
	}
}

func (r *ringBuffer) String() string { return string(r.buf) }

// LookPath resolves a pi executable, for callers that want to fail early with a
// clear message rather than at spawn time.
func LookPath() (string, error) {
	p, err := exec.LookPath("pi")
	if err != nil {
		return "", fmt.Errorf("pi: executable not on PATH: %w", err)
	}
	return p, nil
}

// EnvWith returns a copy of the base environment with the given overrides applied.
// Used by callers that need a specific model or provider key per profile.
func EnvWith(base []string, kv map[string]string) []string {
	out := make([]string, 0, len(base)+len(kv))
	for _, e := range base {
		// An entry with no "=" is malformed and cannot be overridden; keep it as
		// found rather than guessing at a key.
		k, _, ok := strings.Cut(e, "=")
		if !ok {
			out = append(out, e)
			continue
		}
		if _, overridden := kv[k]; overridden {
			continue
		}
		out = append(out, e)
	}
	// Sorted so the resulting environment is deterministic: map iteration order
	// would otherwise make this function's output vary between calls, which is
	// miserable to debug in a diff or a test.
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+kv[k])
	}
	return out
}

// EnvironOrOs returns the process environment, or the OS environment when base is
// nil. os.Environ is always non-empty, so the nil case is only for tests.
func EnvironOrOs(base []string) []string {
	if base == nil {
		return os.Environ()
	}
	return base
}
