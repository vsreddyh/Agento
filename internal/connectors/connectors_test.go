package connectors

import (
	"context"
	"testing"
)

func TestListSynthesisesDisconnected(t *testing.T) {
	s := NewMemoryStore()
	list, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("list = %d, want 3", len(list))
	}
	for _, c := range list {
		if c.Status != StatusDisconnected {
			t.Errorf("%s status = %s, want disconnected", c.ID, c.Status)
		}
		if len(c.Scopes) == 0 {
			t.Errorf("%s has no scopes", c.ID)
		}
	}
}

func TestDisconnectUnknown404(t *testing.T) {
	s := NewMemoryStore()
	// Unknown id is NotFound, never synthesised.
	if _, err := s.Get(context.Background(), "slack"); err == nil {
		t.Fatal("unknown get should fail")
	} else if se, ok := err.(*StoreError); !ok || !se.NotFound {
		t.Fatalf("unknown get should be NotFound, got %v", err)
	}
	if _, err := s.Disconnect(context.Background(), "slack"); err == nil {
		t.Fatal("unknown disconnect should fail")
	}
}

func TestSetStatusAndDisconnect(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	got, err := s.SetStatus(ctx, "gmail", StatusExpired, "token aged out")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusExpired || got.LastError == "" {
		t.Fatalf("set = %+v", got)
	}
	got, err = s.Disconnect(ctx, "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDisconnected || got.LastError != "" {
		t.Fatalf("disconnect = %+v, want disconnected with cleared error", got)
	}
	// Idempotent: disconnecting twice stays disconnected.
	if _, err := s.Disconnect(ctx, "gmail"); err != nil {
		t.Fatal(err)
	}
}

func TestDraftOnlySend(t *testing.T) {
	for _, tc := range []struct{ server, tool string }{
		{"gmail", "send_message"}, {"gmail", "send_gmail"},
		{"calendar", "create_event"}, {"calendar", "send_invite"},
	} {
		if !IsDraftOnlySend(tc.server, tc.tool) {
			t.Errorf("%s/%s should be draft-only send", tc.server, tc.tool)
		}
	}
	for _, tc := range []struct{ server, tool string }{
		{"gmail", "create_draft"}, {"gmail", "search_gmail"},
		{"calendar", "create_event_draft"}, {"drive", "save_file"},
		{"notes", "delete_note"},
	} {
		if IsDraftOnlySend(tc.server, tc.tool) {
			t.Errorf("%s/%s should not be a refused send", tc.server, tc.tool)
		}
	}
	// Case-insensitive: the --server flag is operator-controlled, and a
	// "Gmail" mcp.json entry must not bypass the refusal.
	if !IsDraftOnlySend("Gmail", "Send_Message") {
		t.Error("mixed-case Gmail/Send_Message should still refuse")
	}
	if RefusalMessage == "" {
		t.Error("refusal message must name the draft path")
	}
}

// The display projection is the load-bearing privacy invariant: token
// columns must never be selected into API reads. Unit-pinned without a
// database (DB tests skip without MONGODB_URI; this one always runs).
func TestDisplayProjectionExcludesTokens(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range displayProjection {
		seen[e.Key] = true
	}
	for _, tok := range []string{refreshTokenField, accessTokenField} {
		if seen[tok] {
			t.Errorf("display projection selects token column %q", tok)
		}
	}
	for _, want := range []string{"_id", "displayName", "scopes", "status"} {
		if !seen[want] {
			t.Errorf("display projection missing %q", want)
		}
	}
}

// A corrupt/hand-edited doc with an empty status must read back
// disconnected, never a fourth state the app doesn't expect.
func TestBackfillDisplayRepairsStatus(t *testing.T) {
	got := backfillDisplay(Connector{ID: "gmail"}, "gmail")
	if got.Status != StatusDisconnected {
		t.Errorf("status = %q, want disconnected", got.Status)
	}
	if len(got.Scopes) == 0 || got.DisplayName == "" {
		t.Errorf("backfill dropped scopes/display: %+v", got)
	}
}

// Disconnect clears the error like the off state should: SetStatus to
// disconnected matches Disconnect so the two paths agree.
func TestSetStatusDisconnectedClearsError(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if _, err := s.SetStatus(ctx, "gmail", StatusExpired, "aged out"); err != nil {
		t.Fatal(err)
	}
	got, err := s.SetStatus(ctx, "gmail", StatusDisconnected, "stale")
	if err != nil {
		t.Fatal(err)
	}
	if got.LastError != "" {
		t.Errorf("LastError = %q, want cleared on disconnect", got.LastError)
	}
}
