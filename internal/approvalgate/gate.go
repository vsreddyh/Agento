// Package approvalgate is the in-process tool wrapper (ADR-0011 Stage 2,
// re-owned by #487).
//
// It replaces cmd/mcp-gate's relay process with a wrapper applied where each
// MCP server registers its tools: the same approvals.NeedsApproval / audit
// logging, minus the JSON-RPC re-parsing. One deployment serves one
// client and one turn at a time.
//
// Behaviour as of #487: the gate NEVER parks a hold. Confirmation lives in
// T3/opencode (permission rules in opencode.json), not in an in-process
// wait for an app tap — there is no app attached to decide one, so a hold
// would stall the turn for the full wait and then fail. What remains is
// decision-for-decision with the old allow path plus two fail-closed
// refusals: draft-only sends (connector write paths refused outright with
// the drafts-instead message) and unknown (server, tool) pairs (a tool no
// policy table reviewed never executes blind).
//
// The startup sweep lives in each server main: same per-server
// MarkServerInterrupted call before serving, which now also expires any
// pre-migration pending row no live holder polls.
package approvalgate

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"time"

	"agento/internal/approvals"
	"agento/internal/connectors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	storeTimeout = 5 * time.Second
)

// warned tracks servers already warned about, so a 12-tool binary logs one
// line, not twelve.
var warned sync.Map

// AddTool registers h on s with the tool wrapper in front. server is the
// lowercase code literal each binary registers with (what the KnownServer
// warning and the cmd policy tests pin); store carries the audit trail.
// The wrapper never blocks: confirmation is owned by T3/opencode
// (opencode.json permission rules), so every known tool executes once the
// harness has permitted the call.
func AddTool[In, Out any](s *mcp.Server, server string, store approvals.Store, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	// Unknown server holds every call fail-closed, so say so once at
	// startup: without this a typo in the server literal is silent until
	// a human wonders why every tool on it parks holds.
	if !approvals.KnownServer(server) {
		if _, loaded := warned.LoadOrStore(server, true); !loaded {
			log.Printf("approvalgate: warning: server %q is not in the approval policy table; every tool call will hold", server)
		}
	}
	name := strings.ToLower(strings.TrimSpace(t.Name))
	mcp.AddTool(s, t, func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		args := argMap(in)
		return run(ctx, server, t.Name, name, args, approvals.Redact(args), store,
			func() (*mcp.CallToolResult, Out, error) { return h(ctx, req, in) })
	})
}

// run is the gate decision: draft-only refusal, unknown-tool denial, or
// immediate execution with an audit row. It never parks a hold: there is no
// app attached to decide one (#487), so waiting would stall the turn for
// the full timeout and then fail — the harness asks the human before the
// call runs instead.
func run[Out any](ctx context.Context, server, tool, name string, args, redacted map[string]any, store approvals.Store, exec func() (*mcp.CallToolResult, Out, error)) (*mcp.CallToolResult, Out, error) {
	return decide(ctx, server, tool, name, args, redacted, store, exec)
}

func decide[Out any](ctx context.Context, server, tool, name string, args, redacted map[string]any, store approvals.Store, exec func() (*mcp.CallToolResult, Out, error)) (*mcp.CallToolResult, Out, error) {
	var zero Out

	// Draft-only refusal, before anything else (same order as the relay):
	// connector send paths never reach a server, however permissive the
	// harness permission is.
	if connectors.IsDraftOnlySend(server, name) {
		audit(store, server, tool, redacted, approvals.DispositionDenied,
			"connector_draft_only: "+connectors.RefusalMessage, "")
		return nil, zero, errors.New(connectors.RefusalMessage)
	}

	// Unknown (server, tool) fails closed: a tool no policy table reviewed
	// never executes blind. Unreachable from the wire in practice — the SDK
	// rejects unregistered tools before any handler runs — so this is the
	// backstop for direct callers, pinned by TestUnknownToolDenied.
	if !approvals.KnownServer(server) || !knownTool(server, name) {
		audit(store, server, tool, redacted, approvals.DispositionDenied,
			"unknown tool: no approval policy covers "+server+"/"+name, "")
		return nil, zero, errors.New("unknown tool " + server + "/" + name + ": no approval policy covers it; failing closed")
	}

	// Known tool: run at once, then audit with the tool's own error if it
	// fails. Sensitive or not, the harness — not this process — owns the
	// confirmation (see opencode.json), so there is no allow/gated split
	// left to enforce here. The policy table (approvals.NeedsApproval)
	// stays as the documented sensitivity record the permission rules
	// mirror; it just no longer parks anything.
	res, out, err := exec()
	msg := ""
	if err != nil {
		msg = approvals.ScrubError(err.Error())
	}
	audit(store, server, tool, redacted, approvals.DispositionAllow, msg, "")
	return res, out, err
}

// knownTool reports whether the policy table names this tool for the
// server. Both sides fold: the table keys stay lowercase and the caller
// passes the already-folded tool name.
func knownTool(server, name string) bool {
	for _, t := range approvals.ToolsFor(server) {
		if t == name {
			return true
		}
	}
	return false
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
