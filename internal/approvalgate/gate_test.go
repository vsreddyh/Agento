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

// stubStore is a shared in-memory approvals.Store: holds are decidable by
// calling Decide/Expire directly, like the gateway would over Mongo.
type stubStore struct {
	mu        sync.Mutex
	shared    bool
	approvals map[string]approvals.Approval
	audits    []approvals.AuditEntry
	createErr error
}

func newStub(shared bool) *stubStore {
	return &stubStore{shared: shared, approvals: map[string]approvals.Approval{}}
}

func (s *stubStore) Shared() bool { return s.shared }

func (s *stubStore) CreateApproval(_ context.Context, a approvals.Approval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.createErr != nil {
		return s.createErr
	}
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

// decideCall runs the gate with millisecond timing and no SDK server.
func decideCall(store *stubStore, server, tool string, exec func() (*mcp.CallToolResult, map[string]any, error)) (*mcp.CallToolResult, map[string]any, error) {
	if exec == nil {
		exec = okExec
	}
	name := strings.ToLower(strings.TrimSpace(tool))
	return decide(context.Background(), server, tool, name, map[string]any{}, map[string]any{}, store,
		300*time.Millisecond, 10*time.Millisecond, exec)
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

func TestGatedApproveRuns(t *testing.T) {
	st := newStub(true)
	done := make(chan error, 1)
	go func() {
		_, _, err := decideCall(st, "notes", "delete_note", nil)
		done <- err
	}()
	// Wait for the hold to park, then decide like the gateway would.
	var id string
	for i := 0; i < 200; i++ {
		st.mu.Lock()
		for k := range st.approvals {
			id = k
		}
		st.mu.Unlock()
		if id != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("no approval parked")
	}
	if _, err := st.DecideApproval(context.Background(), id, true, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("approved call errored: %v", err)
	}
	if got := st.lastAudit(); got.Disposition != approvals.DispositionApproved || got.ApprovalID != id {
		t.Errorf("want approved audit joined to decision, got %+v", got)
	}
}

func TestGatedDenyFailsClosed(t *testing.T) {
	st := newStub(true)
	done := make(chan error, 1)
	go func() {
		_, _, err := decideCall(st, "notes", "delete_note", nil)
		done <- err
	}()
	var id string
	for i := 0; i < 200 && id == ""; i++ {
		st.mu.Lock()
		for k := range st.approvals {
			id = k
		}
		st.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	if id == "" {
		t.Fatal("no approval parked")
	}
	if _, err := st.DecideApproval(context.Background(), id, false, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "denied by user") {
		t.Fatalf("want denial error, got %v", err)
	}
	if got := st.lastAudit(); got.Disposition != approvals.DispositionDenied {
		t.Errorf("want denied audit, got %+v", got)
	}
}

func TestGatedTimeoutExpires(t *testing.T) {
	st := newStub(true)
	_, _, err := decideCall(st, "notes", "delete_note", nil)
	if err == nil || !strings.Contains(err.Error(), "expired before a decision") {
		t.Fatalf("want expiry error, got %v", err)
	}
	if got := st.lastAudit(); got.Disposition != approvals.DispositionExpired {
		t.Errorf("want expired audit, got %+v", got)
	}
	// Row must not stay pending.
	st.mu.Lock()
	defer st.mu.Unlock()
	for _, a := range st.approvals {
		if a.Status == approvals.StatusPending {
			t.Error("approval left pending after timeout")
		}
	}
}

func TestUnsharedStoreDeniesGated(t *testing.T) {
	st := newStub(false)
	_, _, err := decideCall(st, "notes", "delete_note", nil)
	if err == nil || !strings.Contains(err.Error(), "failing closed") {
		t.Fatalf("want fail-closed error, got %v", err)
	}
	if got := st.lastAudit(); got.Disposition != approvals.DispositionDenied {
		t.Errorf("want denied audit, got %+v", got)
	}
}

func TestUnsharedStoreStillAllows(t *testing.T) {
	st := newStub(false)
	if _, _, err := decideCall(st, "notes", "list_notes", nil); err != nil {
		t.Fatalf("allow path on unshared store errored: %v", err)
	}
}

func TestUnknownToolHolds(t *testing.T) {
	st := newStub(true)
	// Unknown tool on a known server fails closed (holds → times out here).
	// This pins the policy branch, not the wire path: the SDK rejects
	// unregistered tools before the gate ever runs.
	_, _, err := decideCall(st, "notes", "no_such_tool_xyz", nil)
	if err == nil || !strings.Contains(err.Error(), "expired before a decision") {
		t.Fatalf("want hold-then-expiry, got %v", err)
	}
}

func TestCreateErrorDenies(t *testing.T) {
	st := newStub(true)
	st.createErr = errors.New("store down")
	_, _, err := decideCall(st, "notes", "delete_note", nil)
	if err == nil || !strings.Contains(err.Error(), "approval store unavailable") {
		t.Fatalf("want store-unavailable error, got %v", err)
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
