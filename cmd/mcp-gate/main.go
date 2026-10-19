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
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"

	"agento/internal/approvals"
)

// maxLineBytes caps one JSON-RPC frame. The SDK allows 16 MB lines; the relay
// refuses past double that rather than OOMing on a corrupt stream.
const maxLineBytes = 32 << 20

// pollInterval is how often a held call re-reads its approval.
const pollInterval = 2 * time.Second

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

	mu      sync.Mutex
	out     io.Writer
	childIn io.Writer
	// pending tracks forwarded tools/call ids so responses can be audited.
	// Held calls are NOT in here until approved and forwarded.
	pending map[string]*callState
}

type callState struct {
	name string
	args map[string]any
	sent time.Time
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
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		return err
	}
	r.mu.Lock()
	r.out = piOut
	r.childIn = childStdin
	r.pending = map[string]*callState{}
	r.mu.Unlock()

	done := make(chan error, 2)
	go func() { done <- r.pumpPiToChild(piIn) }()
	go func() { done <- r.pumpChildToPi(childStdout) }()

	err = <-done
	// Either direction ending tears the pair down: a dead child must not
	// leave the Pi-side pump (blocked on stdin) hanging the session, and a
	// closed stdin means Pi is gone. stop() SIGKILLs a live child; the
	// blocked reader is abandoned on the exit path, which is safe because
	// the process is leaving.
	stop()
	child.Wait()
	return err
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
			return err
		}
		var env envelope
		if json.Unmarshal(line, &env) != nil || env.Method != "tools/call" {
			r.writeChild(line)
			continue
		}
		var params callParams
		if json.Unmarshal(env.Params, &params) != nil || params.Name == "" {
			r.writeChild(line)
			continue
		}
		if !approvals.NeedsApproval(r.server, params.Name, params.Arguments) {
			r.track(string(env.ID), params.Name, params.Arguments)
			r.writeChild(line)
			continue
		}
		go r.holdCall(line, env, params)
	}
}

// holdCall parks one sensitive call until a human decides, then forwards it
// or answers Pi with a tool error. Only this call waits; the pumps flow on.
func (r *relay) holdCall(line []byte, env envelope, params callParams) {
	id := string(env.ID)
	redacted := approvals.Redact(params.Arguments)
	a := approvals.Approval{
		ID:     approvals.NewID(),
		Server: r.server,
		Tool:   params.Name,
		Args:   redacted,
		Status: approvals.StatusPending,
	}
	if err := r.store.CreateApproval(context.Background(), a); err != nil {
		r.log.Printf("create approval: %v (failing closed)", err)
		r.writePi(rpcError(env.ID, -32000, "approval store unavailable; failing closed"))
		r.audit(params.Name, redacted, approvals.DispositionDenied, "approval store unavailable", "")
		return
	}
	r.log.Printf("holding %s/%s for approval %s", r.server, params.Name, a.ID)

	ctx, cancel := context.WithTimeout(context.Background(), r.waitFor)
	defer cancel()
	ticker := time.NewTicker(r.pollFor)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			r.log.Printf("approval %s expired with no decision", a.ID)
			r.store.TimeoutApproval(context.Background(), a.ID, time.Now())
			r.writePi(rpcError(env.ID, -32000, "approval "+a.ID+" expired with no decision; failing closed"))
			r.audit(params.Name, redacted, approvals.DispositionExpired, "no decision in time", a.ID)
			return
		case <-ticker.C:
			got, err := r.store.GetApproval(context.Background(), a.ID)
			if err != nil {
				continue
			}
			switch got.Status {
			case approvals.StatusApproved:
				r.track(id, params.Name, params.Arguments)
				r.writeChild(line)
				r.audit(params.Name, redacted, approvals.DispositionApproved, "", a.ID)
				return
			case approvals.StatusDenied, approvals.StatusExpired:
				r.log.Printf("approval %s %s", a.ID, got.Status)
				r.writePi(rpcError(env.ID, -32000, "denied by user (approval "+a.ID+")"))
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
			return err
		}
		var env envelope
		if json.Unmarshal(line, &env) == nil && len(env.ID) > 0 {
			if st := r.take(string(env.ID)); st != nil {
				r.auditResponse(st, line)
			}
		}
		r.writePi(line)
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
	if err := r.store.Audit(context.Background(), e); err != nil {
		r.log.Printf("audit write failed: %v", err)
	}
}

func (r *relay) track(id, name string, args map[string]any) {
	if id == "" || id == "null" {
		return
	}
	r.mu.Lock()
	r.pending[id] = &callState{name: name, args: args, sent: time.Now()}
	r.mu.Unlock()
}

func (r *relay) take(id string) *callState {
	if id == "" || id == "null" {
		return nil
	}
	r.mu.Lock()
	st := r.pending[id]
	delete(r.pending, id)
	r.mu.Unlock()
	return st
}

func (r *relay) writeChild(line []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.childIn.Write(line)
	r.childIn.Write([]byte("\n"))
}

func (r *relay) writePi(line []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.out.Write(line)
	r.out.Write([]byte("\n"))
}

// readLine reads one LF-terminated frame, bounded. The trailing newline is
// stripped; the returned bytes never include it.
func readLine(rd *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		chunk, isPrefix, err := rd.ReadLine()
		if err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		if len(out) > maxLineBytes {
			return nil, fmt.Errorf("frame exceeds %d bytes", maxLineBytes)
		}
		if !isPrefix {
			return out, nil
		}
	}
}

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
