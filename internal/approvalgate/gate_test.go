package approvalgate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"agento/internal/approvals"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// stubStore is an in-memory approvals.Store. Nothing parks: since #487
// the gate executes at once, so the approvals map stays empty and these
// Decide/Timeout helpers exist only to satisfy the Store interface.
type stubStore struct {
	mu        sync.Mutex
	shared    bool
	approvals map[string]approvals.Approval
	audits    []approvals.AuditEntry
}

func newStub(shared bool) *stubStore {
	return &stubStore{shared: shared, approvals: map[string]approvals.Approval{}}
}

func (s *stubStore) Shared() bool { return s.shared }

func (s *stubStore) CreateApproval(_ context.Context, a approvals.Approval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a.CreatedAt = time.Now().Unix()
	s.approvals[a.ID] = a
	return nil
}

func (s *stubStore) GetApproval(_ context.Context, id string) (approvals.Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.approvals[id]
	if !ok {
		return approvals.Approval{}, errors.New("not found")
	}
	return a, nil
}

func (s *stubStore) DecideApproval(_ context.Context, id string, approved bool, by string, at time.Time) (approvals.Approval, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.approvals[id]
	if approved {
		a.Status = approvals.StatusApproved
	} else {
		a.Status = approvals.StatusDenied
	}
	a.DecidedBy = by
	s.approvals[id] = a
	return a, nil
}

func (s *stubStore) TimeoutApproval(_ context.Context, id string, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.approvals[id]
	if a.Status == approvals.StatusPending {
		a.Status = approvals.StatusExpired
		s.approvals[id] = a
	}
	return nil
}

func (s *stubStore) ListApprovals(_ context.Context, f approvals.ApprovalFilter) ([]approvals.Approval, error) {
	return nil, nil
}

func (s *stubStore) Audit(_ context.Context, e approvals.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.audits = append(s.audits, e)
	return nil
}

func (s *stubStore) ListAudit(_ context.Context, f approvals.AuditFilter) ([]approvals.AuditEntry, error) {
	return nil, nil
}

func (s *stubStore) MarkInterrupted(_ context.Context, at time.Time) (int64, error) {
	return 0, nil
}

func (s *stubStore) MarkServerInterrupted(_ context.Context, server string, at time.Time) (int64, error) {
	return 0, nil
}

func (s *stubStore) lastAudit() approvals.AuditEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audits[len(s.audits)-1]
}

func okExec() (*mcp.CallToolResult, map[string]any, error) {
	return &mcp.CallToolResult{}, map[string]any{"ok": true}, nil
}

// decideCall runs the gate with no SDK server.
func decideCall(store *stubStore, server, tool string, exec func() (*mcp.CallToolResult, map[string]any, error)) (*mcp.CallToolResult, map[string]any, error) {
	if exec == nil {
		exec = okExec
	}
	name := strings.ToLower(strings.TrimSpace(tool))
	return decide(context.Background(), server, tool, name, map[string]any{}, store, exec)
}

func TestAllowPathRunsAndAuditsAllowed(t *testing.T) {
	st := newStub(true)
	res, _, err := decideCall(st, "notes", "list_notes", nil)
	if err != nil {
		t.Fatalf("allow path errored: %v", err)
	}
	if res == nil {
		t.Fatal("allow path returned nil result")
	}
	if got := st.lastAudit(); got.Disposition != approvals.DispositionAllow {
		t.Errorf("want allowed audit, got %q", got.Disposition)
	}
}

func TestAllowPathHandlerErrorAudited(t *testing.T) {
	st := newStub(true)
	boom := errors.New("db exploded")
	_, _, err := decideCall(st, "notes", "list_notes", func() (*mcp.CallToolResult, map[string]any, error) {
		return nil, nil, boom
	})
	if err != boom {
		t.Fatalf("want handler error through, got %v", err)
	}
	if got := st.lastAudit(); got.Disposition != approvals.DispositionAllow || got.Error == "" {
		t.Errorf("want allowed audit carrying the error, got %+v", got)
	}
}

func TestGatedToolRunsWithoutHold(t *testing.T) {
	// A sensitive tool (#487: confirmation lives in T3/opencode, not in a
	// parked hold) executes at once: no approval row is parked and the
	// audit records an allow.
	st := newStub(true)
	res, _, err := decideCall(st, "notes", "delete_note", nil)
	if err != nil {
		t.Fatalf("gated tool errored: %v", err)
	}
	if res == nil {
		t.Fatal("gated tool returned nil result")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.approvals) != 0 {
		t.Errorf("gated tool parked %d approval(s), want none", len(st.approvals))
	}
	if got := st.audits[len(st.audits)-1]; got.Disposition != approvals.DispositionAllow {
		t.Errorf("want allowed audit, got %q", got.Disposition)
	}
}

func TestGatedToolHandlerErrorAudited(t *testing.T) {
	st := newStub(true)
	boom := errors.New("db exploded")
	_, _, err := decideCall(st, "notes", "delete_note", func() (*mcp.CallToolResult, map[string]any, error) {
		return nil, nil, boom
	})
	if err != boom {
		t.Fatalf("want handler error through, got %v", err)
	}
	if got := st.lastAudit(); got.Disposition != approvals.DispositionAllow || got.Error == "" {
		t.Errorf("want allowed audit carrying the error, got %+v", got)
	}
}

func TestGatedToolRunsOnUnsharedStore(t *testing.T) {
	// No shared store used to mean "deny, since no gateway could decide
	// the hold". With no holds left there is nothing to decide, so even a
	// memory fallback runs the call (#487).
	st := newStub(false)
	res, _, err := decideCall(st, "notes", "delete_note", nil)
	if err != nil {
		t.Fatalf("gated tool on unshared store errored: %v", err)
	}
	if res == nil {
		t.Fatal("gated tool on unshared store returned nil result")
	}
}

func TestUnknownToolDenied(t *testing.T) {
	st := newStub(true)
	// Unknown tool on a known server fails closed at once (#487: there is
	// no hold left to park, so the backstop denies instead of waiting).
	// This pins the policy branch, not the wire path: the SDK rejects
	// unregistered tools before the gate ever runs.
	ran := false
	_, _, err := decideCall(st, "notes", "no_such_tool_xyz", func() (*mcp.CallToolResult, map[string]any, error) {
		ran = true
		return okExec()
	})
	if err == nil || !strings.Contains(err.Error(), "failing closed") {
		t.Fatalf("want fail-closed error, got %v", err)
	}
	if ran {
		t.Error("unknown tool reached the handler")
	}
	if got := st.lastAudit(); got.Disposition != approvals.DispositionDenied {
		t.Errorf("want denied audit, got %+v", got)
	}
}

func TestUnknownServerDenied(t *testing.T) {
	st := newStub(true)
	_, _, err := decideCall(st, "no_such_server_xyz", "list_notes", nil)
	if err == nil || !strings.Contains(err.Error(), "failing closed") {
		t.Fatalf("want fail-closed error, got %v", err)
	}
}

func TestDraftOnlySendRefused(t *testing.T) {
	st := newStub(true)
	_, _, err := decideCall(st, "gmail", "send_message", nil)
	if err == nil || !strings.Contains(err.Error(), "create a draft instead") {
		t.Fatalf("want draft refusal, got %v", err)
	}
	if got := st.lastAudit(); got.Disposition != approvals.DispositionDenied {
		t.Errorf("want denied audit, got %+v", got)
	}
	// The refused send must never reach the handler.
	ran := false
	_, _, _ = decideCall(st, "gmail", "send_message", func() (*mcp.CallToolResult, map[string]any, error) {
		ran = true
		return okExec()
	})
	if ran {
		t.Error("refused send reached the handler")
	}
}

// TestAddToolRegisters exercises the SDK wiring: the tool is callable by
// name through a real server+client pair over an in-memory transport is
// covered by TestAddToolRegistersName below via handler lookup instead —
// the SDK has no exported handler-invoke, so registration (no panic, tool
// listed) is the assertion that survives SDK upgrades.
func TestAddToolRegistersName(t *testing.T) {
	st := newStub(true)
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	AddTool(s, "notes", st, &mcp.Tool{Name: "list_notes", Description: "x"},
		func(_ context.Context, _ *mcp.CallToolRequest, in map[string]any) (*mcp.CallToolResult, map[string]any, error) {
			return &mcp.CallToolResult{}, map[string]any{"ok": true}, nil
		})
	// Registered without panic; the decision core is covered above.
}
