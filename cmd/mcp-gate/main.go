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
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"agento/internal/approvals"
	"agento/internal/connectors"
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
	// Normalise once, here, not per call: the flag is operator-controlled
	// and every downstream use — policy lookup, refusal check, audit labels,
	// MarkServerInterrupted sweep — exact-matches. A "Gmail" wiring would
	// otherwise gate correctly (normalised lookups) while audit rows grouped
	// as Gmail vs gmail and the sweep missed the other spelling's rows.
	*server = strings.ToLower(strings.TrimSpace(*server))
	if *server == "" {
		fmt.Fprintln(os.Stderr, "usage: mcp-gate --server NAME -- <command> [args...]")
		os.Exit(2)
	}
	if !approvals.KnownServer(*server) {
		// Fail-closed still applies (every call holds), but a typo in
		// mcp.json should be loud at startup, not discovered hold by hold.
		log.Printf("mcp-gate[%s] warning: server is not in the approval policy table; every tool call will hold", *server)
	}
	store := openStore(*server)
	// A relay restart orphans the rows its previous instance was holding: the
	// poll goroutine died with it, so nothing will ever decide them. Expire
	// this server's pending rows before serving, so the app never shows a
	// card whose approval can only be a no-op.
	sweepCtx, cancelSweep := context.WithTimeout(context.Background(), storeTimeout)
	if n, err := store.MarkServerInterrupted(sweepCtx, *server, time.Now()); err != nil {
		log.Printf("mcp-gate[%s] warning: startup sweep failed: %v", *server, err)
	} else if n > 0 {
		log.Printf("mcp-gate[%s] expired %d orphaned approval(s) from a previous relay", *server, n)
	}
	cancelSweep()
	r := newRelay(*server, store, log.New(os.Stderr, "mcp-gate["+*server+"] ", log.LstdFlags))
	// A term signal drives the normal teardown (pumps close, holds cancel, the
	// join runs and expiry rows land). Background() would let SIGTERM kill the
	// process mid-hold, bypassing exactly the join that exists to record it.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	if err := r.run(ctx, os.Stdin, os.Stdout, rest[0], rest[1:]); err != nil {
		r.log.Printf("relay ended: %v", err)
		os.Exit(1)
	}
}

func openStore(server string) approvals.Store {
	if store, err := approvals.FromEnv(); err != nil {
		// Prefixed like every other relay line: six relays share stderr, and
		// this is the line that explains a later split-brain warning.
		log.Printf("mcp-gate[%s] approvals falling back to in-memory: %v", server, err)
		mem := approvals.NewMemoryStore()
		// Same prefix and the same Logf contract as the Mongo branch: the
		// fallback is the split-brain path triage cares about most, and a
		// silent duplicate-create line helps nobody.
		mem.Logf = func(format string, args ...any) {
			log.Printf("mcp-gate["+server+"] "+format, args...)
		}
		return mem
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
	// auditWG tracks notification/malformed audits, which are detached so the
	// pump cannot stall on the store. closing flips when teardown starts:
	// after that auditAsync runs inline, because a WaitGroup Add racing Wait
	// is a panic, and a frame still in the pipe has no row to lose anyway.
	auditMu sync.Mutex
	closing bool
	auditWG sync.WaitGroup

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
	// disp and approvalID carry the call's provenance to the response row, so
	// an approved call is ONE audit row that names its approval, instead of an
	// "approved" row at forward time plus a second anonymous "allowed" row.
	disp       approvals.Disposition
	approvalID string
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
	r.beginClose()
	cancelHolds()
	// Holds record their own expiry after the cancel, and this process exits
	// as soon as run returns — without a join, the teardown audit row (the
	// one case the relay specifically promises to record) is cut off mid-write.
	// Each hold can spend 2x storeTimeout sequentially (timeout + audit), so
	// the budget scales with the number of parked holds, capped so a wedged
	// store cannot hold shutdown open indefinitely. Concurrent holds are rare
	// by design (one turn per profile), but the cap keeps the worst case
	// bounded rather than proportional to traffic. Pre-close detached audits
	// (auditWG) share this budget best-effort: they are bounded by one
	// storeTimeout each, so a burst can push the join into the timeout branch,
	// which logs rather than failing anything that still matters.
	holdsDone := make(chan struct{})
	go func() {
		r.holdWG.Wait()
		r.auditWG.Wait()
		close(holdsDone)
	}()
	budget := 2*storeTimeout + time.Duration(r.holdCount())*2*storeTimeout
	if budget > 30*time.Second {
		budget = 30 * time.Second
	}
	select {
	case <-holdsDone:
	case <-time.After(budget):
		r.log.Printf("shutdown: held calls still recording after %s; continuing", budget)
	}
	stop()
	// Wait's error is expected here — stop() killed the child — so the exit
	// status is not a signal; the prefixed stderr is the record of why.
	_ = child.Wait()
	// The second pump ends because of the teardown above, so it is read
	// without waiting (a Pi-side reader can stay blocked on stdin forever).
	// A non-clean cause is still worth a line: a clean hangup must not hide a
	// child that died dirty, and vice versa. "server process exited" is the
	// expected consequence of stop() and is not logged.
	select {
	case second := <-done:
		if second != nil && !errors.Is(second, errServerExited) {
			r.log.Printf("relay: other direction ended: %v", second)
		}
	default:
	}
	// A cancelled parent is a requested shutdown: the child's exit is the
	// expected consequence, not a crash. Returning it would log "relay ended"
	// and exit 1 on every deploy/container stop.
	if ctx.Err() != nil && errors.Is(err, errServerExited) {
		return nil
	}
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
//
// The audit contract is per tools/call, not per frame: initialize, tools/list
// and any non-call traffic forward untouched without a row. JSON-RPC batches
// (a top-level array) are outside the contract — MCP removed them, and Pi/the
// SDK do not send them — so they forward untouched too, which also means an
// allowed call inside a batch is not paired with its response.
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
			// The relay could not classify this call, so it cannot know
			// whether it is safe. Forwarding it would be the one fail-open
			// path around the gate: a lenient server would run a call the
			// policy never saw. Deny it, audit why, and answer Pi when
			// there is an id to answer.
			// No frame bytes: this is the one path where structured Redact
			// cannot run, and stderr outlives the 90-day audit TTL. Method +
			// shape is enough to triage; the audit row carries the event.
			r.log.Printf("denying unparseable tools/call (id=%t, %d bytes)",
				hasReplyID(env.ID), len(line))
			r.auditAsync("(malformed)", map[string]any{}, approvals.DispositionDenied,
				"relay_protocol: tools/call params could not be parsed; failed closed", "")
			if hasReplyID(env.ID) {
				if werr := r.writePi(rpcError(env.ID, -32602,
					"tools/call params could not be parsed; failed closed")); werr != nil {
					r.log.Printf("write to Pi failed: %v", werr)
				}
			}
			continue
		}
		// Normalise once, here: the tool name is operator-adjacent (Pi sends
		// it, casing is Pi's choice) and every downstream use — refusal
		// check, policy lookup, tracked state, audit rows — exact-matches.
		// Without this, Send_Message and send_message refuse identically but
		// audit as two tool names. The forwarded frame keeps Pi's raw bytes
		// (line is written verbatim): policy and audit see the folded
		// spelling, the server sees exactly what Pi sent. A mixed-case
		// allowed tool therefore gates correctly but may arrive at a
		// case-sensitive MCP server as an unknown method — fail-closed
		// downstream (server errors, Pi sees a tool error), and Pi sends
		// lowercase in practice. Rewriting the frame would break the
		// byte-identical forwarding contract, so the case lives with Pi.
		params.Name = strings.ToLower(strings.TrimSpace(params.Name))
		if connectors.IsDraftOnlySend(r.server, params.Name) {
			// Draft-only v1 (#316): sends never reach the server, gated or
			// not. Refused with a clear message naming drafts, audited as a
			// human-readable deny. A notification (no id) has nobody to
			// answer, so it is only audited.
			r.log.Printf("refusing draft-only send %s/%s", r.server, params.Name)
			r.auditAsync(params.Name, approvals.Redact(params.Arguments),
				approvals.DispositionDenied,
				"connector_draft_only: "+connectors.RefusalMessage, "")
			if hasReplyID(env.ID) {
				if werr := r.writePi(rpcError(env.ID, -32602, connectors.RefusalMessage)); werr != nil {
					r.log.Printf("write to Pi failed: %v", werr)
				}
			}
			continue
		}
		if !approvals.NeedsApproval(r.server, params.Name, params.Arguments) {
			redacted := approvals.Redact(params.Arguments)
			tracked := hasReplyID(env.ID)
			if tracked {
				r.track(env.ID, params.Name, redacted)
			}
			if err := r.writeChild(line); err != nil {
				// The child never received the call, so no response will take
				// the tracked entry and the forward-time row would claim a
				// call that did not happen. Untrack and record the delivery
				// failure, exactly as the approved-forward path does.
				if tracked {
					r.take(env.ID)
				}
				r.log.Printf("write to server failed: %v", err)
				// Async like every other pump-path row: the return below tears
				// the pair down, and auditAsync is closing-aware, so the row is
				// joined by the teardown wait without blocking this pump.
				r.auditAsync(params.Name, redacted, approvals.DispositionAllow,
					"allowed but not delivered to the server: "+err.Error(), "")
				return err
			}
			if !tracked {
				// A call with no id gets no response to observe, so its allow
				// row is written here — after the forward, so it can only
				// describe a call the server actually received.
				r.auditAsync(params.Name, redacted, approvals.DispositionAllow, "", "")
			}
			continue
		}
		if !hasReplyID(env.ID) {
			// Nothing is waiting on a notification: approving it later could
			// not release a turn, and parking it would show an approval card
			// for a call whose result nobody receives. Fail closed now and
			// record why; the allow path above still forwards notifications.
			r.log.Printf("denying gated notification %s/%s: no reply id", r.server, params.Name)
			r.auditAsync(params.Name, approvals.Redact(params.Arguments),
				approvals.DispositionDenied,
				"relay_notification: gated notification has no reply id; failed closed", "")
			continue
		}
		// One goroutine per held call. Unbounded by construction, but bounded
		// by traffic: Pi serialises turns per profile, so a server sees one
		// agent call at a time, and each hold ends at waitFor regardless. A
		// semaphore here would queue sensitive calls behind each other for no
		// gain the turn's own serialisation does not already give.
		// Under the same lock beginClose takes: after closing is set, no Add
		// can land while run() is in holdWG.Wait() (a panic, not a lost row),
		// and the budget snapshot below cannot be undersized by a late hold.
		r.auditMu.Lock()
		if r.closing {
			r.auditMu.Unlock()
			// Teardown: nothing will decide this call, so fail closed now
			// rather than park a row the exiting relay can never release.
			r.log.Printf("denying %s/%s: relay is shutting down", r.server, params.Name)
			// Inline, but still best-effort: the Pi pump is not joined, so
			// process exit can cut this row like any other post-close pump
			// audit. Only hold-path rows are guaranteed by the teardown join.
			r.audit(params.Name, approvals.Redact(params.Arguments),
				approvals.DispositionDenied,
				"relay_closing: relay is shutting down; failed closed", "")
			if hasReplyID(env.ID) {
				if werr := r.writePi(rpcError(env.ID, -32000,
					"relay is shutting down; failed closed")); werr != nil {
					r.log.Printf("write to Pi failed: %v", werr)
				}
			}
			continue
		}
		// Checked under the same lock as closing: no Add has happened, so
		// no Wait can be in flight, and a deny here parks nothing.
		if !r.store.Shared() {
			r.auditMu.Unlock()
			// No shared store means no decision can ever arrive: the
			// gateway holds a different in-process store (or none), so a
			// parked hold would burn the full wait and fail closed with
			// nothing to show for it. Deny now with the reason attached —
			// same verdict, minutes earlier, and audited as its own cause.
			// Sync audit like the closing path above: the row must exist
			// before Pi reads the error that cites it.
			r.log.Printf("denying %s/%s: approval store is not shared; failing closed", r.server, params.Name)
			r.audit(params.Name, approvals.Redact(params.Arguments),
				approvals.DispositionDenied,
				"relay_unavailable: approval store is not shared across processes; failed closed", "")
			if werr := r.writePi(rpcError(env.ID, -32000,
				"approvals unavailable: relay has no shared store; failed closed")); werr != nil {
				r.log.Printf("write to Pi failed: %v", werr)
			}
			continue
		}
		r.holdWG.Add(1)
		r.auditMu.Unlock()
		go func() {
			defer r.holdWG.Done()
			r.holdCall(line, env, params)
		}()
	}
}

// holdCall parks one sensitive call until a human decides, then forwards it
// or answers Pi with a tool error. Only this call waits; the pumps flow on.
//
// The raw frame is retained for the whole hold on purpose: approve must
// forward the exact bytes Pi sent (ids included), and re-encoding happens
// nowhere else in the relay. A multi-MB arguments blob is therefore pinned for
// up to waitFor; traffic is one turn per profile, so the bound is a single
// frame, not a queue.
//
// Both parsed views of the arguments (params.Arguments and the envelope's
// raw params) are nil'd immediately after redaction, so the hold pins ONE
// unmasked copy — the bytes it must forward — rather than three. Everything
// after this point reads `redacted`.
func (r *relay) holdCall(line []byte, env envelope, params callParams) {
	redacted := approvals.Redact(params.Arguments)
	params.Arguments = nil
	env.Params = nil
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
		r.auditAsync(params.Name, redacted, approvals.DispositionDenied,
			"relay_store: approval store unavailable; failed closed", "")
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
				// The response completes this story: trackWith carries the
				// approval id and disposition so auditResponse writes ONE
				// approved row, joined to its decision, with the tool's own
				// error if it fails.
				r.trackWith(env.ID, params.Name, redacted,
					approvals.DispositionApproved, a.ID)
				if werr := r.writeChild(line); werr != nil {
					// No response will arrive, so remove the tracked entry or
					// it would leak, and record the delivery failure now.
					r.take(env.ID)
					r.log.Printf("approval %s: write to server failed: %v", a.ID, werr)
					r.audit(params.Name, redacted, approvals.DispositionApproved,
						"approved but not delivered to the server: "+werr.Error(), a.ID)
					return
				}
				return
			case approvals.StatusDenied, approvals.StatusExpired:
				// Expired is not a denial: nobody said no. A row that expired
				// elsewhere (another relay, MarkInterrupted) must not be
				// attributed to the user in a 90-day audit row.
				disp, reason, reply := approvals.DispositionDenied, "denied by user",
					"denied by user (approval "+a.ID+")"
				if got.Status == approvals.StatusExpired {
					disp, reason, reply = approvals.DispositionExpired,
						"expired before a decision",
						"approval "+a.ID+" expired before a decision; failing closed"
				}
				r.log.Printf("approval %s %s", a.ID, got.Status)
				if hasReplyID(env.ID) {
					if werr := r.writePi(rpcError(env.ID, -32000, reply)); werr != nil {
						r.log.Printf("write to Pi failed: %v", werr)
					}
				}
				r.audit(params.Name, redacted, disp, reason, a.ID)
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
				return errServerExited
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
	disp := st.disp
	if disp == "" {
		disp = approvals.DispositionAllow
	}
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
	r.audit(st.name, st.args, disp, errMsg, st.approvalID)
}

// audit writes one audit row, redacted and best-effort.
func (r *relay) audit(tool string, args map[string]any, disp approvals.Disposition, errMsg, approvalID string) {
	e := approvals.AuditEntry{
		ID:          approvals.NewID(),
		Server:      r.server,
		Tool:        tool,
		Args:        approvals.Redact(args),
		Disposition: disp,
		Error:       approvals.ScrubError(errMsg),
		ApprovalID:  approvalID,
		// At and ExpiresAt stay zero: the store stamps both from one clock, so
		// the retention window is exactly 90 days by its own reckoning and TTL
		// behaviour is pinned in one place. The relay holds no clock rule.
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
		// Garbage is not a replyable id: only well-formed strings and numbers
		// pair. Tracking raw bytes here would let malformed input create
		// pending entries the normal contract rejects.
		return ""
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

// auditAsync keeps the frame pump moving. These paths are notifications and
// malformed calls with nothing waiting on a response: a wedged store must not
// head-of-line-block the next frame for the 5s store bound. Audit failures are
// logged inside audit(), so a lost row is never silent.
func (r *relay) auditAsync(tool string, args map[string]any,
	disp approvals.Disposition, errMsg, approvalID string) {
	r.auditMu.Lock()
	if r.closing {
		r.auditMu.Unlock()
		// Teardown has begun: run() is about to wait, so write inline.
		r.audit(tool, args, disp, errMsg, approvalID)
		return
	}
	r.auditWG.Add(1)
	r.auditMu.Unlock()
	go func() {
		defer r.auditWG.Done()
		r.audit(tool, args, disp, errMsg, approvalID)
	}()
}

// beginClose stops new detached audits; later ones run inline. Inline is not
// a survival guarantee for a pump that is still alive: only hold-path rows are
// joined, and a frame the Pi pump handles during the teardown budget still
// races process exit. It is best-effort by construction — and correct, because
// such a frame arrived after the pair had already ended.
func (r *relay) beginClose() {
	r.auditMu.Lock()
	r.closing = true
	r.auditMu.Unlock()
}

// holdCount reports parked holds, for sizing the teardown budget.
func (r *relay) holdCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.holds)
}

func (r *relay) track(id json.RawMessage, name string, args map[string]any) {
	r.trackWith(id, name, args, approvals.DispositionAllow, "")
}

func (r *relay) trackWith(id json.RawMessage, name string, args map[string]any,
	disp approvals.Disposition, approvalID string) {
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
	// at is wall clock on purpose: it orders eviction only (oldest unanswered
	// first past pendingCap), never correctness, and the store's clock is not
	// reachable from this in-memory table.
	r.pending[key] = &callState{
		name: name, args: args, at: time.Now(),
		disp: disp, approvalID: approvalID,
	}
	r.mu.Unlock()
	// Outside the lock: the logger is I/O, and the audit can block on a store
	// call. Losing the entry must not also lose the record of the call, so
	// the eviction writes its own allow row with the reason.
	if evicted != nil {
		r.log.Printf("pending call table full: evicted unanswered id %s", evictedKey)
		// Async like the other notification-path rows, and closing-aware:
		// once teardown starts auditAsync writes inline, so the row the
		// eviction exists to preserve is not lost at exit.
		r.auditAsync(evicted.name, evicted.args, evicted.disp,
			"relay_evicted: response not observed (pending cap)", evicted.approvalID)
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
	n, err := p.w.Write(append([]byte(p.prefix), b...))
	if err != nil {
		return 0, err
	}
	// Report only the caller's bytes: the prefix was not part of b. Anything
	// short of the whole prefixed frame is a short write, never a silent
	// success the caller might spin on.
	if n != len(p.prefix)+len(b) {
		return 0, io.ErrShortWrite
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
	n, err := w.Write(frame)
	if err != nil {
		return err
	}
	if n != len(frame) {
		return io.ErrShortWrite
	}
	return nil
}

// readLine reads one LF-terminated frame, bounded. The trailing newline is
// stripped; the returned bytes never include it.
var (
	errFrameTooLarge = errors.New("frame exceeds the size cap")
	// errServerExited is returned when the child closes stdout. It is the
	// expected consequence of teardown, so the second-pump log filters it by
	// sentinel rather than by matching text a rename could invalidate.
	errServerExited = errors.New("server process exited")
)

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
	out := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error":   map[string]any{"code": code, "message": message},
	}
	b, err := json.Marshal(out)
	if err != nil {
		// Invalid id bytes: answer with a null id rather than a bare newline,
		// which the client would read as a protocol violation.
		b, _ = json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      nil,
			"error":   map[string]any{"code": code, "message": message},
		})
	}
	return b
}
