package connectors

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/bson"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
)

func mongoErrNotFound() error { return mongoDrv.ErrNoDocuments }

func errBoom() error { return errors.New("boom") }

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
// Exact key set, not subset: lastError/connectedAt/updatedAt are load-bearing
// too (expired loses its error text, the wire loses timestamps if any drops),
// so any field addition/removal fails loudly instead of silently changing
// the wire.
func TestDisplayProjectionExcludesTokens(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range displayProjection() {
		if seen[e.Key] {
			t.Errorf("duplicate projection key %q", e.Key)
		}
		seen[e.Key] = true
	}
	want := []string{"_id", "displayName", "scopes", "status", "lastError", "connectedAt", "updatedAt"}
	if len(seen) != len(want) {
		t.Fatalf("projection has %d keys %v, want %d %v", len(seen), seen, len(want), want)
	}
	for _, k := range want {
		if !seen[k] {
			t.Errorf("display projection missing %q", k)
		}
	}
	for _, tok := range []string{refreshTokenField, accessTokenField} {
		if seen[tok] {
			t.Errorf("display projection selects token column %q", tok)
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

// The atomic-write invariant: the disconnected update carries the token
// $unset in the SAME document as the status $set, so no edit can split them
// into two writes again. No database needed.
func TestStatusUpdateDocAtomic(t *testing.T) {
	doc := statusUpdateDoc("gmail", StatusDisconnected, "", 123, 123)
	var unset bson.D
	for _, e := range doc {
		if e.Key == "$unset" {
			unset, _ = e.Value.(bson.D)
		}
	}
	if len(unset) == 0 {
		t.Fatal("disconnected update has no $unset")
	}
	keys := map[string]bool{}
	for _, e := range unset {
		keys[e.Key] = true
	}
	if !keys[refreshTokenField] || !keys[accessTokenField] {
		t.Errorf("$unset = %v, want both token columns", unset)
	}
	if doc := statusUpdateDoc("gmail", StatusConnected, "", 123, 77); len(doc) != 1 {
		t.Errorf("connected update should be $set only, got %d ops", len(doc))
	}
	if doc := disconnectUpdateDoc("gmail", 123); len(doc) != 2 {
		t.Errorf("disconnect update should be $set+$unset, got %d ops", len(doc))
	}
}

// Memory writes self-heal display fields like Mongo's $set does.
func TestMemoryWriteRefreshesDisplay(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	got, err := s.SetStatus(ctx, "gmail", StatusConnected, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != DisplayName("gmail") || len(got.Scopes) == 0 {
		t.Errorf("write did not refresh display fields: %+v", got)
	}
}

// ListIDs returns a copy: mutating it must not change the package set.
func TestListIDsReturnsCopy(t *testing.T) {
	a := ListIDs()
	a[0] = "mutated"
	if b := ListIDs(); b[0] == "mutated" {
		t.Error("ListIDs mutation leaked into the package set")
	}
	if len(ListIDs()) != 3 {
		t.Errorf("ListIDs = %v, want 3", ListIDs())
	}
}

// Healthy and off rows carry no error text: only expired does.
func TestSetStatusOnlyExpiredCarriesError(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if got, err := s.SetStatus(ctx, "gmail", StatusConnected, "stale"); err != nil {
		t.Fatal(err)
	} else if got.LastError != "" {
		t.Errorf("connected LastError = %q, want empty", got.LastError)
	}
	if _, err := s.SetStatus(ctx, "gmail", StatusExpired, "aged out"); err != nil {
		t.Fatal(err)
	}
	got, err := s.SetStatus(ctx, "gmail", StatusConnected, "stale")
	if err != nil {
		t.Fatal(err)
	} else if got.LastError != "" {
		t.Errorf("reconnected LastError = %q, want empty", got.LastError)
	}
}

// ConnectedAt is first-connect: re-marking connected preserves it.
func TestSetStatusPreservesConnectedAt(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	first, err := s.SetStatus(ctx, "gmail", StatusConnected, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SetStatus(ctx, "gmail", StatusConnected, "")
	if err != nil {
		t.Fatal(err)
	}
	if second.ConnectedAt != first.ConnectedAt {
		t.Errorf("ConnectedAt moved %d -> %d on re-mark", first.ConnectedAt, second.ConnectedAt)
	}
	// Disconnect→reconnect keeps the first-ever stamp: Disconnect retains
	// ConnectedAt and SetStatus(connected) preserves-if-nonzero, so both
	// backends agree the wire means "since first connect", not "since
	// current streak".
	if _, err := s.Disconnect(ctx, "gmail"); err != nil {
		t.Fatal(err)
	}
	third, err := s.SetStatus(ctx, "gmail", StatusConnected, "")
	if err != nil {
		t.Fatal(err)
	}
	if third.ConnectedAt != first.ConnectedAt {
		t.Errorf("ConnectedAt moved %d -> %d across disconnect/reconnect", first.ConnectedAt, third.ConnectedAt)
	}
}

// Expired retains tokens for the follow-up's silent-refresh attempt: only
// the disconnected path carries $unset. A future edit adding $unset here
// would silently break the refresh path the ADR promises.
func TestStatusUpdateDocExpiredRetainsTokens(t *testing.T) {
	for _, e := range statusUpdateDoc("gmail", StatusExpired, "aged out", 123, 0) {
		if e.Key == "$unset" {
			t.Errorf("expired update must not $unset tokens, got %v", e.Value)
		}
	}
}

// Read-path scrub: a corrupt doc with a token in lastError must not echo it
// verbatim — the exact leak the write-path scrub exists to prevent.
func TestBackfillDisplayScrubsLastError(t *testing.T) {
	got := backfillDisplay(Connector{ID: "gmail", Status: StatusExpired, LastError: "token=sk-live-1234567890"}, "gmail")
	if strings.Contains(got.LastError, "sk-live") {
		t.Errorf("LastError echoes token verbatim: %q", got.LastError)
	}
	if !strings.Contains(got.LastError, "[redacted]") {
		t.Errorf("LastError not scrubbed: %q", got.LastError)
	}
	if got.Status != StatusExpired {
		t.Errorf("status changed by scrub: %q", got.Status)
	}
}

// Repeat disconnect is a no-op success: updated_at stays the last transition,
// not the last verb.
func TestDisconnectRepeatLeavesTimestamps(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	first, err := s.Disconnect(ctx, "gmail")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Disconnect(ctx, "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if second.UpdatedAt != first.UpdatedAt {
		t.Errorf("UpdatedAt moved %d -> %d on repeat disconnect", first.UpdatedAt, second.UpdatedAt)
	}
}

// resolveConnectedAt pins first-ever-connect for both backends with no DB:
// nonzero survives, zero takes now.
func TestResolveConnectedAt(t *testing.T) {
	if got := resolveConnectedAt(42, 99); got != 42 {
		t.Errorf("got %d, want 42", got)
	}
	if got := resolveConnectedAt(0, 99); got != 99 {
		t.Errorf("got %d, want 99", got)
	}
}

// disconnectTransition pins the explicit-off rule without Atlas: absent
// means transition (write; sets explicit-off), stored-disconnected means
// no-op, other statuses mean transition, probe errors write anyway flagged.
func TestDisconnectTransition(t *testing.T) {
	if tr, ok := disconnectTransition(mongoErrNotFound(), StatusDisconnected); !tr || !ok {
		t.Errorf("absent = %v,%v want true,true", tr, ok)
	}
	if tr, ok := disconnectTransition(nil, StatusDisconnected); tr || !ok {
		t.Errorf("stored-disconnected = %v,%v want false,true", tr, ok)
	}
	if tr, ok := disconnectTransition(nil, StatusConnected); !tr || !ok {
		t.Errorf("stored-connected = %v,%v want true,true", tr, ok)
	}
	if tr, ok := disconnectTransition(errBoom(), StatusDisconnected); !tr || ok {
		t.Errorf("probe-error = %v,%v want true,false", tr, ok)
	}
}

// Repeated SetStatus with no state change skips the write: updated_at is
// last transition on every path.
func TestSetStatusNoChangeSkipsWrite(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	first, err := s.SetStatus(ctx, "gmail", StatusExpired, "aged out")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.SetStatus(ctx, "gmail", StatusExpired, "aged out")
	if err != nil {
		t.Fatal(err)
	}
	if second.UpdatedAt != first.UpdatedAt {
		t.Errorf("UpdatedAt moved %d -> %d on no-change re-set", first.UpdatedAt, second.UpdatedAt)
	}
}

// Regression: the transition check must compare BEFORE mutating. A
// connected→disconnected flip with empty error once early-returned a
// mutated-but-unpersisted copy (caller saw disconnected, store kept
// connected). Re-read after flip to pin persistence.
func TestSetStatusFlipPersists(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if _, err := s.SetStatus(ctx, "gmail", StatusConnected, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Disconnect(ctx, "gmail"); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusDisconnected {
		t.Errorf("stored status = %q, want disconnected", got.Status)
	}
}

// Disconnected backfill clears stale error text, matching the write path.
func TestBackfillDisplayClearsDisconnectedError(t *testing.T) {
	got := backfillDisplay(Connector{ID: "gmail", Status: StatusDisconnected, LastError: "boom"}, "gmail")
	if got.LastError != "" {
		t.Errorf("disconnected LastError = %q, want cleared", got.LastError)
	}
}

// repairUpdateDoc must never carry updatedAt/status/connectedAt: a repair
// that bumped updatedAt would silently turn every repeat verb into a
// timestamp bump, breaking the last-transition contract with no test failing.
func TestRepairUpdateDocShape(t *testing.T) {
	for _, doc := range []bson.D{
		repairUpdateDoc("gmail", "", true),
		repairUpdateDoc("gmail", "aged out", false),
	} {
		seen := map[string]bool{}
		var setBody, unsetBody bson.D
		for _, e := range doc {
			seen[e.Key] = true
			if e.Key == "$set" {
				setBody, _ = e.Value.(bson.D)
			}
			if e.Key == "$unset" {
				unsetBody, _ = e.Value.(bson.D)
			}
		}
		if len(doc) > 2 {
			t.Fatalf("repair doc has %d ops %v, want at most $set+$unset", len(doc), seen)
		}
		for _, k := range []string{"updatedAt", "status", "connectedAt"} {
			for _, e := range setBody {
				if e.Key == k {
					t.Errorf("repair $set carries %q", k)
				}
			}
		}
		foundErr := false
		for _, e := range setBody {
			if e.Key == "lastError" {
				foundErr = true
			}
		}
		if !foundErr {
			t.Errorf("repair $set missing lastError: %v", setBody)
		}
		_ = unsetBody
	}
	// Disconnect repair unsets tokens; a non-disconnect repair must not.
	withUnset := repairUpdateDoc("gmail", "", true)
	withoutUnset := repairUpdateDoc("gmail", "aged out", false)
	hasUnset := func(doc bson.D) bool {
		for _, e := range doc {
			if e.Key == "$unset" {
				return true
			}
		}
		return false
	}
	if !hasUnset(withUnset) || hasUnset(withoutUnset) {
		t.Errorf("unset mismatch: disconnect=%v expired=%v", hasUnset(withUnset), hasUnset(withoutUnset))
	}
}

// Same contract on the other healthy state: a corrupt connected row's
// lastError clears on read, matching the write path.
func TestBackfillDisplayClearsConnectedError(t *testing.T) {
	got := backfillDisplay(Connector{ID: "gmail", Status: StatusConnected, LastError: "boom"}, "gmail")
	if got.LastError != "" {
		t.Errorf("connected LastError = %q, want cleared", got.LastError)
	}
}

// Canonical scope order is enforced on read, not just filled when empty.
func TestBackfillDisplayCanonicalisesScopeOrder(t *testing.T) {
	got := backfillDisplay(Connector{ID: "gmail", Status: StatusConnected, Scopes: []string{"gmail.draft", "gmail.read"}}, "gmail")
	want := ScopesFor("gmail")
	if len(got.Scopes) != len(want) || got.Scopes[0] != want[0] || got.Scopes[1] != want[1] {
		t.Errorf("scopes = %v, want canonical %v", got.Scopes, want)
	}
}

// Struct-vs-projection parity: every Connector bson field the app may see
// must be selected by displayProjection, and no token column may be. A new
// struct field forgotten in the projection would otherwise read back zero
// with no test breaking — the exact hole the privacy invariant depends on.
func TestDisplayProjectionMatchesStruct(t *testing.T) {
	type field struct {
		name string
		tag  string
	}
	rt := reflect.TypeOf(Connector{})
	fields := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("bson"), ",")
		if tag == "" || tag == "-" {
			continue
		}
		fields[tag] = true
	}
	var proj []string
	for _, e := range displayProjection() {
		proj = append(proj, e.Key)
	}
	for key := range fields {
		if key == refreshTokenField || key == accessTokenField {
			continue // no such fields on the struct; guarded below
		}
		found := false
		for _, k := range proj {
			if k == key {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("struct bson field %q not selected by displayProjection", key)
		}
	}
	// And the reverse: no projection key the struct doesn't know.
	for _, k := range proj {
		if !fields[k] {
			t.Errorf("projection key %q has no struct field", k)
		}
	}
}

// Struct must never grow token columns: the parity test skips them, so pin
// the skip — a future RefreshToken field would otherwise pass both loops.
func TestConnectorStructHasNoTokenFields(t *testing.T) {
	rt := reflect.TypeOf(Connector{})
	for i := 0; i < rt.NumField(); i++ {
		tag, _, _ := strings.Cut(rt.Field(i).Tag.Get("bson"), ",")
		if tag == refreshTokenField || tag == accessTokenField {
			t.Errorf("Connector carries token column %q; tokens live outside the wire shape", tag)
		}
	}
}

// probeProjection must select exactly the transition-decision fields:
// status, lastError, connectedAt for the stamp resolve, displayName+scopes
// for the heal-vs-skip check — and exclude _id, which the probe decodes
// into nothing. Dropping one compiles fine and silently turns repeat verbs
// into heal-writes (or breaks the stamp resolve) — the no-DB pin in the
// same class as the displayProjection and builder pins.
func TestProbeProjectionShape(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range probeProjection() {
		if seen[e.Key] {
			t.Fatalf("duplicate probe key %q", e.Key)
		}
		seen[e.Key] = true
	}
	for _, k := range []string{"status", "lastError", "connectedAt", "displayName", "scopes"} {
		if !seen[k] {
			t.Errorf("probe projection missing %q", k)
		}
	}
	if len(seen) != 6 {
		t.Errorf("probe projection has %d keys %v, want exactly 6", len(seen), seen)
	}
	for _, e := range probeProjection() {
		if e.Key == "_id" && e.Value != 0 {
			t.Errorf("_id must be excluded (0), got %v", e.Value)
		}
	}
}

// Probe keys must stay within the display keys: the no-op heal-vs-skip
// decision reads off the probe, so a display field missing from the probe
// silently forces heal-writes. Same pin class as the struct and projection
// shape tests.
func TestProbeProjectionWithinDisplay(t *testing.T) {
	display := map[string]bool{}
	for _, e := range displayProjection() {
		display[e.Key] = true
	}
	for _, e := range probeProjection() {
		if !display[e.Key] {
			t.Errorf("probe key %q not in display projection", e.Key)
		}
	}
}
