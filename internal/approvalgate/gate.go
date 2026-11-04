// Package approvalgate is the in-process approval gate (ADR-0011 Stage 2).
//
// It replaces cmd/mcp-gate's relay process with a wrapper applied where each
// MCP server registers its tools: the same approvals.NeedsApproval / audit /
// hold-poll logic, minus the JSON-RPC re-parsing. One deployment serves one
// client and one turn at a time, so the hold blocks the handler goroutine —
// no pump pair, no pending map, no teardown join budget.
//
// Behaviour matches the relay decision-for-decision: draft-only refusal,
// fail-closed unknown server/tool, allow-path audit, hold → poll →
// approve/deny/expire with the same audit dispositions and reply wording.
// What is gone is process-only: stdio plumbing, frame pairing, eviction,
// and the memory-fallback meeting problem (without a shared store the gate
// still denies, since the gateway could never decide that hold either).
// The startup sweep moved into each server main instead: same per-server
// MarkServerInterrupted call before serving, because a crash between
// CreateApproval and decision still orphans the Mongo row.
package approvalgate

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"agento/internal/approvals"
	"agento/internal/connectors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	pollInterval = 2 * time.Second
	storeTimeout = 5 * time.Second
)

// AddTool registers h on s with the approval gate in front. server is the
// --server name the relay used (lowercase id from pi/mcp.json); store must be
// shared (Mongo) for holds to be decidable from the gateway.
func AddTool[In, Out any](s *mcp.Server, server string, store approvals.Store, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	name := strings.ToLower(strings.TrimSpace(t.Name))
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		args := argMap(in)
		return run(ctx, server, t.Name, name, args, approvals.Redact(args), store,
			func() (*mcp.CallToolResult, Out, error) { return h(ctx, req, in) })
	})
}

// run is the gate decision: draft-only refusal, allow, or hold → poll. It
// audits exactly one row per call and returns the handler result (or the
// denial). waitFor/pollFor are parameters so tests run on milliseconds.
func run[Out any](ctx context.Context, server, tool, name string, args, redacted map[string]any, store approvals.Store, exec func() (*mcp.CallToolResult, Out, error)) (*mcp.CallToolResult, Out, error) {
	return decide(ctx, server, tool, name, args, redacted, store, approvals.WaitTimeout, pollInterval, exec)
}

func decide[Out any](ctx context.Context, server, tool, name string, args, redacted map[string]any, store approvals.Store, waitFor, pollFor time.Duration, exec func() (*mcp.CallToolResult, Out, error)) (*mcp.CallToolResult, Out, error) {
	var zero Out

	// Draft-only refusal, before the approval check (same order as the relay).
	if connectors.IsDraftOnlySend(server, name) {
		audit(store, server, tool, redacted, approvals.DispositionDenied,
			"connector_draft_only: "+connectors.RefusalMessage, "")
		return nil, zero, errors.New(connectors.RefusalMessage)
	}

	// Allow path: run, then audit with the tool's own error if it fails.
	if !approvals.NeedsApproval(server, name, args) {
		res, out, err := exec()
		msg := ""
		if err != nil {
			msg = approvals.ScrubError(err.Error())
		}
		audit(store, server, tool, redacted, approvals.DispositionAllow, msg, "")
		return res, out, err
	}

	// Gated but no shared store: the gateway could never decide this hold,
	// so deny instead of parking it (relay_unavailable).
	if !store.Shared() {
		audit(store, server, tool, redacted, approvals.DispositionDenied,
			"gate_unavailable: approvals unavailable (no shared store)", "")
		return nil, zero, errors.New("approvals unavailable (no shared store): failing closed")
	}

	// Hold: park the approval, poll until decided or the wait elapses.
	a := approvals.Approval{
		ID:     approvals.NewID(),
		Server: server,
		Tool:   tool,
		Args:   redacted,
		Status: approvals.StatusPending,
	}
	opCtx, cancelOp := context.WithTimeout(context.Background(), storeTimeout)
	cerr := store.CreateApproval(opCtx, a)
	cancelOp()
	if cerr != nil {
		audit(store, server, tool, redacted, approvals.DispositionDenied,
			"gate_store: approval store unavailable: "+cerr.Error(), "")
		return nil, zero, errors.New("approval store unavailable: failing closed")
	}

	deadline := time.Now().Add(waitFor)
	ticker := time.NewTicker(pollFor)
	defer ticker.Stop()
	pollFails := 0
	for {
		select {
		case <-ctx.Done():
			expire(store, server, tool, redacted, a.ID, "abandoned at teardown")
			return nil, zero, errors.New("approval " + a.ID + " abandoned at teardown; failing closed")
		case <-ticker.C:
			if time.Now().After(deadline) {
				expire(store, server, tool, redacted, a.ID, "expired with no decision")
				return nil, zero, errors.New("approval " + a.ID + " expired before a decision; failing closed")
			}
			pollCtx, cancelPoll := context.WithTimeout(context.Background(), storeTimeout)
			got, gerr := store.GetApproval(pollCtx, a.ID)
			cancelPoll()
			if gerr != nil {
				pollFails++
				if pollFails%30 == 1 {
					log.Printf("approvalgate[%s] approval %s poll failed %d times: %v", server, a.ID, pollFails, gerr)
				}
				continue
			}
			switch got.Status {
			case approvals.StatusApproved:
				res, out, err := exec()
				msg := ""
				if err != nil {
					msg = approvals.ScrubError(err.Error())
				}
				audit(store, server, tool, redacted, approvals.DispositionApproved, msg, a.ID)
				return res, out, err
			case approvals.StatusDenied, approvals.StatusExpired:
				// Expired is not a denial: nobody said no. Attribute
				// separately for the 90-day audit row, like the relay.
				disp, reason, reply := approvals.DispositionDenied, "denied by user",
					"denied by user (approval "+a.ID+")"
				if got.Status == approvals.StatusExpired {
					disp, reason, reply = approvals.DispositionExpired,
						"expired before a decision",
						"approval "+a.ID+" expired before a decision; failing closed"
				}
				audit(store, server, tool, redacted, disp, reason, a.ID)
				return nil, zero, errors.New(reply)
			}
		}
	}
}

// expire flips a still-pending approval expired and records it. No-op when
// the row already decided — called after the wait, win or lose, like the relay.
func expire(store approvals.Store, server, tool string, redacted map[string]any, id, reason string) {
	opCtx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	_ = store.TimeoutApproval(opCtx, id, time.Now())
	cancel()
	audit(store, server, tool, redacted, approvals.DispositionExpired, reason, id)
}

// audit writes one row per call. Best-effort like the relay: a wedged store
// must not take the tool call down with it.
func audit(store approvals.Store, server, tool string, redacted map[string]any, disp approvals.Disposition, errMsg, approvalID string) {
	opCtx, cancel := context.WithTimeout(context.Background(), storeTimeout)
	defer cancel()
	if err := store.Audit(opCtx, approvals.AuditEntry{
		ID:          approvals.NewID(),
		Server:      server,
		Tool:        tool,
		Args:        redacted,
		Disposition: disp,
		Error:       errMsg,
		ApprovalID:  approvalID,
	}); err != nil {
		log.Printf("approvalgate[%s] audit failed: %v", server, err)
	}
}

// argMap renders the typed tool input as a plain map for the policy check,
// the redactor, and the stored approval row. Unmarshalable input yields an
// empty map: the policy still fails closed on it.
func argMap[In any](in In) map[string]any {
	raw, err := json.Marshal(in)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{}
	}
	if m == nil {
		return map[string]any{}
	}
	return m
}
