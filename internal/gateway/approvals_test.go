package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agento/internal/approvals"
)

// These tests pin the #313 gate contract: approvals list, decide-once,
// audit visibility, and the same auth/method discipline as the other
// inventory routes. They drive the HTTP surface with an in-memory store,
// because "the app can approve" is a wire contract.

func approvePost(t *testing.T, srv *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func seedApproval(t *testing.T, srv *Server, tool string) string {
	t.Helper()
	a := approvals.Approval{
		ID: approvals.NewID(), Server: "notes", Tool: tool,
		Args: map[string]any{"id": "abc"}, Status: approvals.StatusPending,
	}
	if err := srv.approvals.(*approvals.MemoryStore).CreateApproval(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	return a.ID
}

func listApprovals(t *testing.T, srv *Server, query string) []map[string]any {
	t.Helper()
	rec := get(t, srv, "/api/approvals"+query, "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/approvals%s: got %d, body=%s", query, rec.Code, rec.Body)
	}
	var body struct {
		Approvals []map[string]any `json:"approvals"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("approvals list is not JSON: %v (%s)", err, rec.Body)
	}
	return body.Approvals
}

func TestApprovalsListDecideOnce(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{})
	id := seedApproval(t, srv, "delete_note")

	list := listApprovals(t, srv, "?status=pending")
	if len(list) != 1 {
		t.Fatalf("pending approvals = %d, want 1", len(list))
	}
	if list[0]["tool"] != "delete_note" || list[0]["status"] != "pending" {
		t.Fatalf("approval = %+v", list[0])
	}

	rec := approvePost(t, srv, "/api/approvals/"+id+"/deny")
	if rec.Code != http.StatusOK {
		t.Fatalf("deny: got %d, body=%s", rec.Code, rec.Body)
	}
	var decided struct {
		Ok     bool   `json:"ok"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decided); err != nil || !decided.Ok {
		t.Fatalf("deny body = %s, %v", rec.Body, err)
	}
	if decided.Status != string(approvals.StatusDenied) {
		t.Fatalf("status = %q, want denied", decided.Status)
	}

	// A decision lands exactly once: the rematch 409s, it does not flip.
	rec = approvePost(t, srv, "/api/approvals/"+id+"/approve")
	if rec.Code != http.StatusConflict {
		t.Fatalf("second decision: got %d, want 409. body=%s", rec.Code, rec.Body)
	}
	if got := listApprovals(t, srv, "?status=pending"); len(got) != 0 {
		t.Fatalf("pending after decide = %d, want 0", len(got))
	}
}

func TestApprovalsUnknownAndBadStatus(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{})
	// Missing reads 404; decided reads 409 (see TestApprovalsListDecideOnce).
	if got := approvePost(t, srv, "/api/approvals/nope/approve"); got.Code != http.StatusNotFound {
		t.Fatalf("unknown approve: got %d, want 404", got.Code)
	}
	rec := get(t, srv, "/api/approvals?status=bogus", "test-token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad status: got %d, want 400", rec.Code)
	}
	// Overlong id never reaches the store.
	long := "/api/approvals/" + strings.Repeat("x", 65) + "/approve"
	if got := approvePost(t, srv, long); got.Code != http.StatusNotFound {
		t.Fatalf("long id: got %d, want 404", got.Code)
	}
}

func TestAuditListsRows(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{})
	mem := srv.approvals.(*approvals.MemoryStore)
	now := time.Now().UnixMilli()
	for i, tool := range []string{"list_notes", "delete_note"} {
		disp := approvals.DispositionAllow
		if i == 1 {
			disp = approvals.DispositionDenied
		}
		if err := mem.Audit(context.Background(), approvals.AuditEntry{
			ID: approvals.NewID(), Server: "notes", Tool: tool,
			Args: map[string]any{}, Disposition: disp, At: now + int64(i),
		}); err != nil {
			t.Fatal(err)
		}
	}

	rec := get(t, srv, "/api/audit", "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("audit: got %d", rec.Code)
	}
	var body struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(body.Entries))
	}
	rec = get(t, srv, "/api/audit?tool=delete_note", "test-token")
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Entries) != 1 {
		t.Fatalf("tool filter = %+v, %v", body, err)
	}
}

func TestApprovalRoutesRequireAuthAndMethods(t *testing.T) {
	srv := newTestServer(t, newFakeAgent(), Config{})
	req := httptest.NewRequest(http.MethodGet, "/api/approvals", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: got %d, want 401", rec.Code)
	}
	if got := approvePost(t, srv, "/api/approvals"); got.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/approvals: got %d, want 405", got.Code)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/audit", nil)
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("audit unauthenticated: got %d, want 401", rec.Code)
	}
}
