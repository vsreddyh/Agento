// Command mcp-gate is the approval-gate relay for #313.
//
// Pi spawns its MCP servers itself from mcp.json over stdio, and Pi offers
// no pre-execution tool hook — so enforcement interposes here: mcp.json
// points each server at this binary, which spawns the real server and
// relays newline-delimited JSON-RPC both ways. Reads and routine writes
// forward untouched; sensitive calls (see approvals.NeedsApproval) hold for
// an explicit in-app decision and audit every call either way.
//
//	pi <--NDJSON--> mcp-gate <--NDJSON--> real server
//	                  |  ^
//	          approvals store (Mongo, memory fallback)
//
// A held call blocks only itself: other traffic keeps flowing, and a deny or
// timeout answers Pi with a tool error so the turn continues. Fail closed
// throughout — a doubt resolves to "not executed".
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"agento/internal/approvals"
)

// maxLineBytes caps one JSON-RPC frame. The SDK allows 16 MB lines; the relay
// refuses past double that rather than OOMing on a corrupt stream.
const maxLineBytes = 32 << 20

// pollInterval is how often a held call re-reads its approval.
const pollInterval = 2 * time.Second

// pendingCap bounds unanswered allowed calls retained for auditing. Real
// traffic is one turn at a time per profile, so reaching this means a server
// stopped answering; the cap keeps that failure from becoming a leak.
const pendingCap = 256

// storeTimeout bounds one approvals-store call. Mongo's server-selection
// budget is longer than this on a wedged cluster, and every one of these calls
// sits on a traffic path: a create on a Pi request, a poll between polls, an
// audit on a response. Unbounded, one wedged client stalls a hold past its own
// deadline and makes best-effort audit writes block the relay.
const storeTimeout = 5 * time.Second

// envelope is the JSON-RPC shape the relay routes on. IDs stay raw: numbers
// and strings must round-trip byte-identical.
type envelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type callParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func main() {
	server := flag.String("server", "", "MCP server name (policy key + audit label)")
	flag.Parse()
	rest := flag.Args()
	if *server == "" || len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "usage: mcp-gate --server NAME -- <command> [args...]")
		os.Exit(2)
	}
	store := openStore()
	r := newRelay(*server, store, log.New(os.Stderr, "mcp-gate["+*server+"] ", log.LstdFlags))
	if err := r.run(context.Background(), os.Stdin, os.Stdout, rest[0], rest[1:]); err != nil {
		r.log.Printf("relay ended: %v", err)
		os.Exit(1)
	}
}

func openStore() approvals.Store {
	if store, err := approvals.FromEnv(); err != nil {
		log.Printf("mcp-gate: approvals falling back to in-memory: %v", err)
		return approvals.NewMemoryStore()
	} else {
		return store
	}
}

// newRelay builds a relay with production timings.
func newRelay(server string, store approvals.Store, log *log.Logger) *relay {
	return &relay{
		server: server, store: store, log: log,
		waitFor: approvals.WaitTimeout, pollFor: pollInterval,
	}
}

// relay owns one Pi<->server pair.
type relay struct {
	server string
	store  approvals.Store
	log    *log.Logger
	// waitFor bounds one held call; pollFor re-reads its approval. Fields
	// (not consts) so tests hold for milliseconds instead of minutes.
	waitFor time.Duration
	pollFor time.Duration

	// outMu guards Pi-bound writes, childMu server-bound ones: a slow or
	// dead direction must not head-of-line-block the other behind one lock.
	outMu   sync.Mutex
	childMu sync.Mutex
	out     io.Writer
	childIn io.Writer

	// holdWG tracks detached hold goroutines. run waits on it (bounded) at
	// teardown so an expiry/deny audit write is not cut off by process exit.
	holdWG sync.WaitGroup

	mu sync.Mutex
	// pending holds allowed calls until their response arrives, so the
	// response can be audited with the request's args. Bounded by
	// pendingCap: a server that accepts a call and never answers must not
	// grow this map for the life of the relay (Pi spawns servers once). The
	// eviction is by request time, so the entry lost is the oldest
	// unanswered one — the call least likely to still produce a response.
	pending map[string]*callState
	// holds cancels parked waits at teardown, so a Pi disconnect does not
	// leave approvals pending that nothing will ever decide.
	holds map[string]context.CancelFunc
	// holdCtx is the teardown scope parked waits observe. Set once in run
	// before the pumps start; read-only after.
	holdCtx context.Context
}

type callState struct {
	name string
	args map[string]any
	at   time.Time
}

func (r *relay) run(
	parent context.Context,
	piIn io.Reader, piOut io.Writer,
	command string, args []string,
) error {
	ctx, stop := context.WithCancel(parent)
	defer stop()
	child := exec.CommandContext(ctx, command, args...)
	childStdin, err := child.StdinPipe()
	if err != nil {
		return err
	}
	childStdout, err := child.StdoutPipe()
	if err != nil {
		return err
	}
	// One process-wide stderr, but each child chunk carries the relay's
	// prefix: six relays share the container log, and a bare child line with
	// no server name is unattributable.
	child.Stderr = &prefixWriter{w: os.Stderr, prefix: "mcp-gate[" + r.server + "] child "}
	if err := child.Start(); err != nil {
		return err
	}
	r.mu.Lock()
	r.out = piOut
	r.childIn = childStdin
	r.pending = map[string]*callState{}
	r.holds = map[string]context.CancelFunc{}
	// Holds die with the pair (see holdCall): captured before the pumps
	// start, so every parked wait observes teardown. Derived from the run
	// scope rather than Background, so a parent cancel reaches parked holds
	// in one step instead of only through the pumps exiting.
	holdCtx, cancelHolds := context.WithCancel(ctx)
	r.holdCtx = holdCtx
	r.mu.Unlock()
	defer cancelHolds()

	done := make(chan error, 2)
	go func() { done <- r.pumpPiToChild(piIn) }()
	go func() { done <- r.pumpChildToPi(childStdout) }()

	err = <-done
	// Either direction ending tears the pair down: a dead child must not
	// leave the Pi-side pump (blocked on stdin) hanging the session, and a
	// closed stdin means Pi is gone. stop() SIGKILLs a live child; the
	// blocked reader is abandoned on the exit path, which is safe because
	// the process is leaving.
	cancelHolds()
	// Holds record their own expiry after the cancel, and this process exits
	// as soon as run returns — without a join, the teardown audit row (the
	// one case the relay specifically promises to record) is cut off mid-write.
	// Bounded: a wedged store must not hold shutdown open forever.
	holdsDone := make(chan struct{})
	go func() {
		r.holdWG.Wait()
		close(holdsDone)
	}()
	select {
	case <-holdsDone:
	case <-time.After(2 * storeTimeout):
		r.log.Printf("shutdown: held calls still recording after %s; continuing", 2*storeTimeout)
	}
	stop()
	child.Wait()
	return err
}

// holdCtxOrBackground returns the teardown scope, or Background when the
// relay runs outside run() (unit tests drive holdCall directly).
func (r *relay) holdCtxOrBackground() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.holdCtx == nil {
		return context.Background()
	}
	return r.holdCtx
}

// pumpPiToChild forwards Pi traffic, holding gated calls.
func (r *relay) pumpPiToChild(piIn io.Reader) error {
	rd := bufio.NewReader(piIn)
	for {
		line, err := readLine(rd)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			if errors.Is(err, errFrameTooLarge) {
				// A framing fault is per-message, not per-connection: the
				// reader has already drained to the next line boundary, so
				// dropping one frame is safer than killing the pair and every
				// hold it owns.
				r.log.Printf("dropping oversized frame from Pi")
				continue
			}
			return err
		}
		var env envelope
		if json.Unmarshal(line, &env) != nil || env.Method != "tools/call" {
			if err := r.writeChild(line); err != nil {
				r.log.Printf("write to server failed: %v", err)
				return err
			}
			continue
		}
		var params callParams
		if json.Unmarshal(env.Params, &params) != nil || params.Name == "" {
			if err := r.writeChild(line); err != nil {
				r.log.Printf("write to server failed: %v", err)
				return err
			}
			continue
		}
		if !approvals.NeedsApproval(r.server, params.Name, params.Arguments) {
			redacted := approvals.Redact(params.Arguments)
			if !hasReplyID(env.ID) {
				// A call with no id gets no response to observe, so its allow
				// row is written here. track() would no-op and the call would
				// forward with no audit entry, which is the one hole in
				// "every proxied call is recorded".
				r.audit(params.Name, redacted, approvals.DispositionAllow, "", "")
			} else {
				r.track(env.ID, params.Name, redacted)
			}
			if err := r.writeChild(line); err != nil {
				r.log.Printf("write to server failed: %v", err)
				return err
			}
			continue
		}
		if !hasReplyID(env.ID) {
			// Nothing is waiting on a notification: approving it later could
			// not release a turn, and parking it would show an approval card
			// for a call whose result nobody receives. Fail closed now and
			// record why; the allow path above still forwards notifications.
			r.log.Printf("denying gated notification %s/%s: no reply id", r.server, params.Name)
			r.audit(params.Name, approvals.Redact(params.Arguments),
				approvals.DispositionDenied,
				"gated notification has no reply id; failed closed", "")
			continue
		}
		// One goroutine per held call. Unbounded by construction, but bounded
		// by traffic: Pi serialises turns per profile, so a server sees one
		// agent call at a time, and each hold ends at waitFor regardless. A
		// semaphore here would queue sensitive calls behind each other for no
		// gain the turn's own serialisation does not already give.
		r.holdWG.Add(1)
		go func() {
			defer r.holdWG.Done()
			r.holdCall(line, env, params)
		}()
	}
}

// holdCall parks one sensitive call until a human decides, then forwards it
// or answers Pi with a tool error. Only this call waits; the pumps flow on.
func (r *relay) holdCall(line []byte, env envelope, params callParams) {
	redacted := approvals.Redact(params.Arguments)
	a := approvals.Approval{
		ID:     approvals.NewID(),
		Server: r.server,
		Tool:   params.Name,
		Args:   redacted,
		Status: approvals.StatusPending,
	}
	// Derived from the hold scope, not Background: a create still in flight
	// when the pair tears down must abort rather than insert a row after
	// cancelHolds has run, where nothing would ever expire it.
	createCtx, cancelCreate := context.WithTimeout(r.holdCtxOrBackground(), storeTimeout)
	err := r.store.CreateApproval(createCtx, a)
	cancelCreate()
	if err != nil {
		r.log.Printf("create approval: %v (failing closed)", err)
		if hasReplyID(env.ID) {
			if werr := r.writePi(rpcError(env.ID, -32000, "approval store unavailable; failing closed")); werr != nil {
				r.log.Printf("write to Pi failed: %v", werr)
			}
		}
		r.audit(params.Name, redacted, approvals.DispositionDenied, "approval store unavailable", "")
		return
	}
	if !r.store.Shared() {
		// Split-brain guard: this store is process-local, so no gateway
		// will ever decide this row. Loud, so a Mongo-less deployment
		// reads as what it is (gated calls expire by design) rather than a
		// hung turn nobody can explain.
		r.log.Printf("ERROR holding %s/%s with no shared store: configure MONGODB_URI or every gated call expires", r.server, params.Name)
	}
	r.log.Printf("holding %s/%s for approval %s", r.server, params.Name, a.ID)

	holdCtx, unhold := context.WithCancel(r.holdCtxOrBackground())
	r.mu.Lock()
	r.holds[a.ID] = unhold
	r.mu.Unlock()
	defer func() {
		// unhold releases this hold's child context on the normal path; the
		// map entry is what teardown cancels.
		unhold()
		r.mu.Lock()
		delete(r.holds, a.ID)
		r.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(holdCtx, r.waitFor)
	defer cancel()
	ticker := time.NewTicker(r.pollFor)
	defer ticker.Stop()
	expire := func(reason string) {
		r.log.Printf("approval %s %s", a.ID, reason)
		expireCtx, cancelExpire := context.WithTimeout(context.Background(), storeTimeout)
		if err := r.store.TimeoutApproval(expireCtx, a.ID, time.Now()); err != nil {
			r.log.Printf("timeout approval %s: %v", a.ID, err)
		}
		cancelExpire()
		if hasReplyID(env.ID) {
			if werr := r.writePi(rpcError(env.ID, -32000, "approval "+a.ID+" "+reason+"; failing closed")); werr != nil {
				r.log.Printf("write to Pi failed: %v", werr)
			}
		}
		r.audit(params.Name, redacted, approvals.DispositionExpired, reason, a.ID)
	}
	pollFails := 0
	for {
		select {
		case <-ctx.Done():
			// Teardown (Pi gone) and timeout share the expiry: either way
			// nobody will decide, so the row must not stay pending. The
			// message says which, for the audit reader.
			if holdCtx.Err() != nil {
				expire("abandoned at teardown")
			} else {
				expire("expired with no decision")
			}
			return
		case <-ticker.C:
			// The poll observes teardown; expire below deliberately does
			// NOT — it runs because teardown happened and must be able to
			// write that fact.
			pollCtx, cancelPoll := context.WithTimeout(holdCtx, storeTimeout)
			got, err := r.store.GetApproval(pollCtx, a.ID)
			cancelPoll()
			if err != nil {
				// A store outage spins here until expiry; log sparsely so
				// triage sees it without one line per poll.
				pollFails++
				if pollFails%30 == 1 {
					r.log.Printf("approval %s poll failed %d times: %v", a.ID, pollFails, err)
				}
				continue
			}
			switch got.Status {
			case approvals.StatusApproved:
				// Track redacted: the pending map lives until the response
				// arrives, and raw args must not sit in it. Audit redacts
				// again — same output, safe in-memory copy.
				r.track(env.ID, params.Name, redacted)
				if werr := r.writeChild(line); werr != nil {
					// Approved but undeliverable: the audit row must say so,
					// or a reader sees "approved" while Pi never got an
					// answer. The hold is over either way — the turn's
					// failure is the stream's to report.
					r.log.Printf("approval %s: write to server failed: %v", a.ID, werr)
					r.audit(params.Name, redacted, approvals.DispositionApproved,
						"approved but not delivered to the server: "+werr.Error(), a.ID)
					return
				}
				r.audit(params.Name, redacted, approvals.DispositionApproved, "", a.ID)
				return
			case approvals.StatusDenied, approvals.StatusExpired:
				r.log.Printf("approval %s %s", a.ID, got.Status)
				if hasReplyID(env.ID) {
					if werr := r.writePi(rpcError(env.ID, -32000, "denied by user (approval "+a.ID+")")); werr != nil {
						r.log.Printf("write to Pi failed: %v", werr)
					}
				}
				r.audit(params.Name, redacted, approvals.DispositionDenied, "denied by user", a.ID)
				return
			}
		}
	}
}

// pumpChildToPi forwards server traffic, auditing tracked calls.
func (r *relay) pumpChildToPi(childOut io.Reader) error {
	rd := bufio.NewReader(childOut)
	for {
		line, err := readLine(rd)
		if err != nil {
			if err == io.EOF {
				return fmt.Errorf("server process exited")
			}
			if errors.Is(err, errFrameTooLarge) {
				r.log.Printf("dropping oversized frame from server")
				continue
			}
			return err
		}
		var env envelope
		// hasReplyID, not len(ID) > 0: "null" is four bytes and take() would
		// reject it anyway, so the null contract stays in one helper.
		if json.Unmarshal(line, &env) == nil && hasReplyID(env.ID) {
			if st := r.take(env.ID); st != nil {
				r.auditResponse(st, line)
			}
		}
		if err := r.writePi(line); err != nil {
			r.log.Printf("write to Pi failed: %v", err)
			return err
		}
	}
}

// auditResponse records one completed call. Best-effort: an audit failure is
// logged, never fatal to the traffic it describes.
func (r *relay) auditResponse(st *callState, line []byte) {
	var env envelope
	disp := approvals.DispositionAllow
	errMsg := ""
	if json.Unmarshal(line, &env) == nil {
		var res struct {
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &res) == nil && res.Error != nil {
			errMsg = res.Error.Message
		}
	}
	r.audit(st.name, st.args, disp, errMsg, "")
}

// audit writes one audit row, redacted and best-effort.
func (r *relay) audit(tool string, args map[string]any, disp approvals.Disposition, errMsg, approvalID string) {
	now := time.Now()
	e := approvals.AuditEntry{
		ID:          approvals.NewID(),
		Server:      r.server,
		Tool:        tool,
		Args:        approvals.Redact(args),
		Disposition: disp,
		Error:       errMsg,
		ApprovalID:  approvalID,
		At:          now.UnixMilli(),
		ExpiresAt:   now.Add(approvals.AuditTTL),
	}
	ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err := r.store.Audit(ctx, e); err != nil {
		r.log.Printf("audit write failed: %v", err)
	}
}

// idKey canonicalises a JSON-RPC id for pairing a call with its response.
// Raw bytes are not enough: a server may re-encode what it echoed ("1" as
// "1.0", whitespace around it), and byte-equality would then miss the
// response and silently drop its audit row.
//
// Numbers are canonicalised as exact rationals, not through float64: two ids
// above 2^53 (9007199254740993 and 9007199254740992) both round to the same
// float and would share one pending entry, pairing one call's response with
// another's args. UseNumber plus big.Rat keeps integer ids exact and still
// makes "1", "1.0" and "1e0" the same key. Strings are re-marshalled for
// whitespace/escape normalisation. An unparseable id falls back to its
// trimmed bytes rather than vanishing.
func idKey(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return ""
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return s
	}
	switch t := v.(type) {
	case string:
		return "s:" + t
	case json.Number:
		if r, ok := new(big.Rat).SetString(t.String()); ok {
			return "n:" + r.RatString()
		}
		return "n:" + t.String()
	default:
		// JSON-RPC ids are string, number or null. Anything else (a bool, an
		// object) is malformed: fail closed by not pairing or replying to it.
		return ""
	}
}

func (r *relay) track(id json.RawMessage, name string, args map[string]any) {
	key := idKey(id)
	if key == "" {
		return
	}
	var evicted *callState
	var evictedKey string
	r.mu.Lock()
	if len(r.pending) >= pendingCap {
		var oldest time.Time
		for k, st := range r.pending {
			if evictedKey == "" || st.at.Before(oldest) {
				evictedKey, oldest = k, st.at
			}
		}
		evicted = r.pending[evictedKey]
		delete(r.pending, evictedKey)
	}
	r.pending[key] = &callState{name: name, args: args, at: time.Now()}
	r.mu.Unlock()
	// Outside the lock: the logger is I/O, and the audit can block on a store
	// call. Losing the entry must not also lose the record of the call, so
	// the eviction writes its own allow row with the reason.
	if evicted != nil {
		r.log.Printf("pending call table full: evicted unanswered id %s", evictedKey)
		// Synchronous, like every other audit: the 5s store bound caps the
		// stall, and a fire-and-forget row is the one most likely to be lost
		// to teardown — the exact hole this row exists to close.
		r.audit(evicted.name, evicted.args, approvals.DispositionAllow,
			"relay_evicted: response not observed (pending cap)", "")
	}
}

func (r *relay) take(id json.RawMessage) *callState {
	key := idKey(id)
	if key == "" {
		return nil
	}
	r.mu.Lock()
	st := r.pending[key]
	delete(r.pending, key)
	r.mu.Unlock()
	return st
}

func (r *relay) writeChild(line []byte) error {
	r.childMu.Lock()
	defer r.childMu.Unlock()
	return writeFrame(r.childIn, line)
}

func (r *relay) writePi(line []byte) error {
	r.outMu.Lock()
	defer r.outMu.Unlock()
	return writeFrame(r.out, line)
}

// prefixWriter labels another process's stderr with the relay's server name.
// It is deliberately not line-buffered: a child that writes a partial line
// still gets the prefix on that chunk, which is what makes the first chunk
// attributable when a server crashes mid-line.
type prefixWriter struct {
	w      io.Writer
	prefix string
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	if _, err := p.w.Write(append([]byte(p.prefix), b...)); err != nil {
		return 0, err
	}
	return len(b), nil
}

// writeFrame writes one newline-terminated frame with a single Write. Two
// writes under the same lock were safe against interleaving but could leave a
// half frame if the second failed, and a partial frame is unrecoverable for
// the reader.
func writeFrame(w io.Writer, line []byte) error {
	frame := make([]byte, len(line)+1)
	copy(frame, line)
	frame[len(line)] = '\n'
	_, err := w.Write(frame)
	return err
}

// readLine reads one LF-terminated frame, bounded. The trailing newline is
// stripped; the returned bytes never include it.
var errFrameTooLarge = errors.New("frame exceeds the size cap")

func readLine(rd *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, isPrefix, err := rd.ReadLine()
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if len(out) > maxLineBytes {
			// Drain to the next newline so the caller can keep reading
			// frames: stopping mid-line would make the rest of this frame
			// parse as a new one.
			for isPrefix {
				_, isPrefix, err = rd.ReadLine()
				if err != nil {
					return nil, err
				}
			}
			return nil, errFrameTooLarge
		}
		if !isPrefix {
			return out, nil
		}
	}
}

// hasReplyID reports whether Pi is waiting for a response. A tools/call
// notification carries no id (or explicit null) and gets no reply by
// JSON-RPC spec: writing one back is a stray frame the client never asked
// for. The audit row is written either way.
func hasReplyID(id json.RawMessage) bool { return idKey(id) != "" }

// rpcError builds a JSON-RPC error reply preserving the caller's id bytes.
func rpcError(id json.RawMessage, code int, message string) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error":   map[string]any{"code": code, "message": message},
	})
	return b
}
