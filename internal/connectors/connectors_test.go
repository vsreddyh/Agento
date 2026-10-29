package connectors

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
)

func mongoErrNotFound() error { return mongoDrv.ErrNoDocuments }

func errBoom() error { return errors.New("boom") }

// testVerifier is a valid-length PKCE verifier for tests: 64 chars,
// inside the RFC 7636 43-128 window, so length gates pass and the test
// exercises the guard under test instead of the floor. (NewState mints
// 86; 64 keeps literals short while staying clearly legal.)
var testVerifier = strings.Repeat("v", 64)

// testVerifierBadUTF8 is a valid-length verifier carrying an overlong
// encoding: long enough to pass the length floor, invalid UTF-8 so the
// encoding gate (not the length gate) is what fires.
var testVerifierBadUTF8 = strings.Repeat("v", 60) + "\xc0\x80"

// testState is a valid-length state: 22 chars like NewState mints, inside
// the MinStateLen..MaxStateFieldLen window. Short literals ("s1", "STATE")
// now trip the floor, so success-path fixtures use realistic lengths.
var testState = strings.Repeat("s", 22)
var testState2 = strings.Repeat("t", 22)

func TestListSynthesisesDisconnected(t *testing.T) {
	s := NewMemoryStore()
	list, err := s.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("list = %d, want 2", len(list))
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
		{"calendar", "create_event_draft"},
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
	if len(ListIDs()) != 2 {
		t.Errorf("ListIDs = %v, want 2", ListIDs())
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
	for _, k := range []string{"status", "lastError", "connectedAt", "displayName", "scopes",
		refreshTokenField, accessTokenField, "accessExpiry"} {
		if !seen[k] {
			t.Errorf("probe projection missing %q", k)
		}
	}
	if len(seen) != 9 {
		t.Errorf("probe projection has %d keys %v, want exactly 9", len(seen), seen)
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
		// Token columns are the documented exception: presence-only
		// probe input for the token-parity check, deliberately absent
		// from the display shape (see probeProjection).
		if e.Key == refreshTokenField || e.Key == accessTokenField || e.Key == "accessExpiry" {
			continue
		}
		if !display[e.Key] {
			t.Errorf("probe key %q not in display projection", e.Key)
		}
	}
}

// Envelope round-trips, and every tamper class fails closed.
func TestTokenEnvelope(t *testing.T) {
	key := [32]byte{1, 2, 3}
	sealed, err := EncryptToken(key, "refresh-token-value")
	if err != nil {
		t.Fatal(err)
	}
	if sealed == "refresh-token-value" {
		t.Fatal("sealed must not equal plaintext")
	}
	open, err := DecryptToken(key, sealed)
	if err != nil || open != "refresh-token-value" {
		t.Fatalf("round trip = %q, %v", open, err)
	}
	// Two seals of the same value differ (random nonce).
	second, err := EncryptToken(key, "refresh-token-value")
	if err != nil {
		t.Fatal(err)
	}
	if second == sealed {
		t.Error("deterministic seal: nonce reuse")
	}
	// Wrong key, tampered tail, truncated, non-base64: all fail.
	var other [32]byte
	other[0] = 9
	if _, err := DecryptToken(other, sealed); err == nil {
		t.Error("wrong key opened")
	}
	bad := sealed[:len(sealed)-2] + "AA"
	if _, err := DecryptToken(key, bad); err == nil {
		t.Error("tampered seal opened")
	}
	if _, err := DecryptToken(key, "!!!not-base64!!!"); err == nil {
		t.Error("non-base64 opened")
	}
	if _, err := EncryptToken([32]byte{}, "x"); err == nil {
		t.Error("zero-key seal allowed")
	}
	if _, err := DecryptToken([32]byte{}, sealed); err == nil {
		t.Error("zero-key open allowed")
	}
}

// States are single-use and expiring (memory backend).
func TestOAuthStateLifecycle(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.CreateState(ctx, OAuthState{State: testState, Connector: "gmail", Verifier: testVerifier, ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ConsumeState(ctx, testState)
	if err != nil || got.Connector != "gmail" || got.Verifier != testVerifier {
		t.Fatalf("consume = %+v, %v", got, err)
	}
	// Double consume fails: a code can never be exchanged twice.
	if _, err := s.ConsumeState(ctx, testState); err == nil {
		t.Fatal("second consume should fail")
	} else if se, ok := err.(*StoreError); !ok || !se.NotFound {
		t.Fatalf("second consume should be NotFound, got %v", err)
	}
	// Expired at creation fails fast: an explicitly-expired row can never
	// be consumed, so storing it would only strand a dead attempt.
	if err := s.CreateState(ctx, OAuthState{State: testState2, Connector: "gmail", Verifier: testVerifier, ExpiresAt: time.Now().Add(-time.Minute)}); err == nil {
		t.Fatal("explicitly expired create should fail")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("expired create should be a deterministic 400, got %v", err)
	}
	// Expiry still enforced at consume time.
	s2 := NewMemoryStore()
	fixed := time.Now()
	s2.now = func() time.Time { return fixed }
	if err := s2.CreateState(ctx, OAuthState{State: testState2, Connector: "gmail", Verifier: testVerifier, ExpiresAt: fixed.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	s2.now = func() time.Time { return fixed.Add(2 * time.Minute) }
	if _, err := s2.ConsumeState(ctx, testState2); err == nil {
		t.Fatal("expired consume should fail")
	}
	// Unknown connector rejected at create.
	if err := s.CreateState(ctx, OAuthState{State: "s3", Connector: "slack", Verifier: testVerifier}); err == nil {
		t.Fatal("unknown connector state should fail")
	}
}

// Token grant lifecycle (memory backend): renewability enforced, unknown
// and grant-less both NotFound.
func TestTokenGrantLifecycle(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if _, err := s.GetTokens(ctx, "gmail"); err == nil {
		t.Fatal("grant-less get should fail")
	}
	if err := s.SetTokens(ctx, "gmail", TokenRow{RefreshCipher: "", AccessCipher: "a"}); err == nil {
		t.Fatal("empty refresh cipher should fail")
	}
	if err := s.SetTokens(ctx, "gmail", TokenRow{RefreshCipher: "r", AccessCipher: "a", AccessExpiryUnixMilli: 1}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTokens(ctx, "gmail")
	if err != nil || got.RefreshCipher != "r" {
		t.Fatalf("get = %+v, %v", got, err)
	}
	if _, err := s.GetTokens(ctx, "slack"); err == nil {
		t.Fatal("unknown get should fail")
	}
}

// Auth URL carries the connector scopes, PKCE challenge, and offline access.
func TestAuthURLShape(t *testing.T) {
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example.com/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	u := AuthURL(cfg, Gmail, testState, testVerifier, nil)
	for _, want := range []string{"client_id=cid", "state=" + testState, "code_challenge=", "code_challenge_method=S256", "access_type=offline", "prompt=consent", "gmail.readonly", "gmail.compose"} {
		if !strings.Contains(u, want) {
			t.Errorf("auth URL missing %q:\n%s", want, u)
		}
	}
	if scopes := GoogleScopes("slack"); scopes != nil {
		t.Errorf("unknown scopes = %v, want nil", scopes)
	}
}

// Disconnect clears grants on every path (parity with the Mongo $unset),
// including the pure no-op: a disconnected row must never serve a grant.
func TestDisconnectClearsTokens(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.SetTokens(ctx, "gmail", TokenRow{RefreshCipher: "r", AccessCipher: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Disconnect(ctx, "gmail"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTokens(ctx, "gmail"); err == nil {
		t.Fatal("grant survived disconnect")
	}
	// Repeat disconnect keeps it cleared.
	if _, err := s.Disconnect(ctx, "gmail"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTokens(ctx, "gmail"); err == nil {
		t.Fatal("grant survived repeat disconnect")
	}
}

// Disconnect scrubs pending attempts for that connector: a browser
// completing an old consent afterwards must fail at ConsumeState.
func TestDisconnectScrubsStates(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.CreateState(ctx, OAuthState{State: testState, Connector: "gmail", Verifier: testVerifier, ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Disconnect(ctx, "gmail"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeState(ctx, testState); err == nil {
		t.Fatal("state survived disconnect")
	}
}

// CreateState normalises like every other verb: a padded/cased connector
// stores folded, so the Disconnect scrub comparison later sees the same id.
func TestCreateStateNormalisesConnector(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.CreateState(ctx, OAuthState{State: testState, Connector: "  GMail ", Verifier: testVerifier, ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	st, err := s.ConsumeState(ctx, testState)
	if err != nil {
		t.Fatal(err)
	}
	if st.Connector != "gmail" {
		t.Errorf("stored connector = %q, want folded gmail", st.Connector)
	}
}

// Absurd expiries clamp to the TTL window instead of persisting: a
// far-future oauth_states row would otherwise outlive every backstop.
func TestCreateStateClampsFarFutureExpiry(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	far := time.Now().Add(365 * 24 * time.Hour)
	if err := s.CreateState(ctx, OAuthState{State: testState, Connector: "gmail", Verifier: testVerifier, ExpiresAt: far}); err != nil {
		t.Fatal(err)
	}
	st, err := s.ConsumeState(ctx, testState)
	if err != nil {
		t.Fatal(err)
	}
	if max := time.Now().Add(StateTTL + time.Minute); st.ExpiresAt.After(max) {
		t.Errorf("expiry not clamped: %v", st.ExpiresAt)
	}
}

// An explicitly expired state fails fast at creation, like a missing
// verifier: a row that ConsumeState would immediately reject must never
// be stored. Fail closed (400), not silent, so the caller retries with
// a fresh attempt instead of triaging a dead one.
func TestCreateStateRejectsPastExpiry(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	past := time.Now().Add(-time.Minute)
	if err := s.CreateState(ctx, OAuthState{State: testState, Connector: "gmail", Verifier: testVerifier, ExpiresAt: past}); err == nil {
		t.Fatal("explicitly expired state should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("expired state should be a deterministic 400, got %v", err)
	}
}

// An oversized pinned redirect fails closed at creation, not after consent.
func TestCreateStateRejectsLongRedirect(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	big := strings.Repeat("x", MaxRedirectLen+1)
	if err := s.CreateState(ctx, OAuthState{State: testState, Connector: "gmail", Verifier: testVerifier, RedirectURI: big}); err == nil {
		t.Fatal("oversized redirect should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("long redirect should be a deterministic 400, got %v", err)
	}
}

// Checked == stored: the row holds the canonicalised form, not the raw
// input — trailing slash dropped, host folded. The raw length gate still
// fires first on oversized input; the canonical gate behind it covers
// growth during canonicalisation (e.g. re-bracketing).
func TestCreateStateStoresCanonicalRedirect(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	raw := "https://HOST.EXAMPLE.COM/p/api/connectors/callback/"
	if err := s.CreateState(ctx, OAuthState{State: testState, Connector: "gmail", Verifier: testVerifier, RedirectURI: raw}); err != nil {
		t.Fatalf("canonicalisable redirect should store: %v", err)
	}
	st, err := s.ConsumeState(ctx, testState)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://host.example.com/p/api/connectors/callback"; st.RedirectURI != want {
		t.Errorf("stored redirect = %q, want canonical %q", st.RedirectURI, want)
	}
}

// A misshapen redirect fails at creation, not after consent: the row would
// otherwise supersede the live attempt and die only at Exchange.
func TestCreateStateRejectsBadRedirect(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.CreateState(ctx, OAuthState{State: testState, Connector: "gmail", Verifier: testVerifier, RedirectURI: "not-a-url"}); err == nil {
		t.Fatal("misshapen redirect should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("bad redirect should be a deterministic 400, got %v", err)
	}
}

// Exact-boundary expiry is pinned without sleeping: expiresAt == now
// counts as expired (matches the Mongo TTL, which reaps at expiry, not
// after) — on BOTH sides. Creation rejects it, so the consume-side boundary
// is exercised with a +1ns row instead.
func TestConsumeStateBoundaryTick(t *testing.T) {
	s := NewMemoryStore()
	fixed := time.Now().Truncate(time.Second)
	s.now = func() time.Time { return fixed }
	ctx := context.Background()
	if err := s.CreateState(ctx, OAuthState{State: testState, Connector: "gmail", Verifier: testVerifier, ExpiresAt: fixed}); err == nil {
		t.Fatal("expiresAt==now should fail at creation")
	}
	if err := s.CreateState(ctx, OAuthState{State: testState2, Connector: "gmail", Verifier: testVerifier, ExpiresAt: fixed.Add(time.Nanosecond)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeState(ctx, testState2); err != nil {
		t.Fatalf("expiresAt 1ns out should be live: %v", err)
	}
}

// Folding matches every store verb: padded/cased ids work the same in
// Exchange, AuthURL, and the stores — same logical id, same answer.
func TestOAuthHelpersFoldIDs(t *testing.T) {
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example.com/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	u := AuthURL(cfg, " GMail ", testState, testVerifier, nil)
	if !strings.Contains(u, "gmail.readonly") {
		t.Errorf("unfolded id lost scopes:\n%s", u)
	}
	if _, err := Exchange(context.Background(), cfg, " GMail ", "code", testVerifier, nil); err == nil {
		t.Fatal("expected exchange attempt (folded id must pass the Known check to reach HTTP)")
	} else if se, ok := err.(*StoreError); ok && se.NotFound {
		t.Fatal("folded id should not 404 as unknown")
	}
}

// Same state string twice is a caller bug: 400, not silent overwrite.
func TestCreateStateDuplicateFails(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	st := OAuthState{State: testState, Connector: "gmail", Verifier: testVerifier, ExpiresAt: time.Now().Add(time.Minute)}
	if err := s.CreateState(ctx, st); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateState(ctx, st); err == nil {
		t.Fatal("duplicate state should fail")
	}
	// ...but a *new* state for the same connector supersedes the old one.
	if err := s.CreateState(ctx, OAuthState{State: testState2, Connector: "gmail", Verifier: testVerifier, ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ConsumeState(ctx, testState); err == nil {
		t.Fatal("superseded state should be gone")
	}
}

// Empty plaintext is unsealable by contract: sealed-empty would be a second
// representation of "missing" alongside literal "".
func TestEncryptTokenRefusesEmpty(t *testing.T) {
	if _, err := EncryptToken([32]byte{1}, ""); err == nil {
		t.Fatal("sealing empty token should fail")
	}
}

// Padded plaintext must fail, not seal-with-padding: the sealed bytes
// carry no spaces, so the check and the stored value would diverge.
func TestEncryptTokenRefusesPadded(t *testing.T) {
	if _, err := EncryptToken([32]byte{1}, " secret "); err == nil {
		t.Fatal("sealing padded token should fail")
	}
}

// Controls (and format characters) fail closed at seal time: a
// sealed-with-control plaintext is valid base64url the SetTokens gates
// cannot see, but decrypts to a value Google rejects. Invalid UTF-8
// fails the same way (RuneError, not control).
func TestEncryptTokenRefusesInvalid(t *testing.T) {
	for _, tc := range []struct{ name, plaintext string }{
		{"control", "sec\x7fret"},
		{"zero-width", "sec\u200bret"},
		{"invalid utf8", "sec\xc0\x80ret"},
		{"interior space", "sec ret"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := EncryptToken([32]byte{1}, tc.plaintext); err == nil {
				t.Fatalf("sealing %s token should fail", tc.name)
			}
		})
	}
}

// Oversized plaintext fails before the seal alloc, not after: the cap
// fires here so the store never sees a multi-MB cipher from a direct
// caller. Sealed grants stay well under the bound in prod.
func TestEncryptTokenRefusesOversized(t *testing.T) {
	if _, err := EncryptToken([32]byte{1}, strings.Repeat("x", MaxPlaintextLen+1)); err == nil {
		t.Fatal("sealing oversized token should fail")
	}
	// Oversized and padded reports length first (empty → length → padded
	// → control), matching every sibling leg for the same input pair.
	if _, err := EncryptToken([32]byte{1}, " "+strings.Repeat("x", MaxPlaintextLen+1)+" "); err == nil {
		t.Fatal("sealing oversized padded token should fail")
	} else if !strings.Contains(err.Error(), "oversized") {
		t.Fatalf("oversized+padded should report length, got %q", err)
	}
}

// The bounds round-trip: a max-size plaintext seals to a cipher the store
// accepts and DecryptToken opens. The cipher bound must clear the
// plaintext bound plus seal overhead, or a legitimate large token could
// be minted but never stored.
func TestTokenBoundsRoundTrip(t *testing.T) {
	var key [32]byte
	copy(key[:], "test-envelope-key-0123456789ab")
	sealed, err := EncryptToken(key, strings.Repeat("x", MaxPlaintextLen))
	if err != nil {
		t.Fatalf("max-size plaintext should seal: %v", err)
	}
	if len(sealed) > MaxCipherLen {
		t.Fatalf("sealed max plaintext = %d chars, want <= MaxCipherLen %d", len(sealed), MaxCipherLen)
	}
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.SetTokens(ctx, Gmail, TokenRow{RefreshCipher: sealed}); err != nil {
		t.Fatalf("sealed max plaintext should store: %v", err)
	}
	row, err := s.GetTokens(ctx, Gmail)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecryptToken(key, row.RefreshCipher); err != nil {
		t.Fatalf("stored max cipher should open: %v", err)
	}
}

// Unknown ids fail closed at the helper, not just the gateway.
func TestAuthURLUnknownIDFailsClosed(t *testing.T) {
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example.com/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	if got := AuthURL(cfg, "slack", "s", "v", nil); got != "" {
		t.Errorf("unknown id auth URL = %q, want empty", got)
	}
	if got := AuthURL(nil, "gmail", "s", "v", nil); got != "" {
		t.Errorf("nil cfg auth URL = %q, want empty", got)
	}
	// Hand-built cfgs without endpoints fail closed too: nothing but
	// OAuthConfigFromEnv sets them, and a consent URL against an empty
	// endpoint can only 400 at Google.
	bare := &OAuthConfig{ClientID: "cid", RedirectURL: "https://app.example.com/cb/api/connectors/callback"}
	if got := AuthURL(bare, "gmail", "s", "v", nil); got != "" {
		t.Errorf("endpoint-less auth URL = %q, want empty", got)
	}
	// Hand-built cfgs with a non-matching redirect fail closed too, via
	// the same shape check the env path runs.
	evil := &OAuthConfig{ClientID: "cid", RedirectURL: "https://evil.example/x", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	if got := AuthURL(evil, "gmail", "s", "v", nil); got != "" {
		t.Errorf("wrong-host auth URL = %q, want empty", got)
	}
	barePath := &OAuthConfig{ClientID: "cid", RedirectURL: "https://app.example.com/other", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	if got := AuthURL(barePath, "gmail", "s", "v", nil); got != "" {
		t.Errorf("wrong-path auth URL = %q, want empty", got)
	}
}

// The logf reason names the guard that fired, so triage reads the log,
// not the code, to tell a misconfigured client from a bad redirect.
func TestAuthURLLogsReason(t *testing.T) {
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example.com/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	var logged string
	logf := func(format string, args ...any) { logged = fmt.Sprintf(format, args...) }
	if got := AuthURL(cfg, "slack", testState, testVerifier, logf); got != "" || !strings.Contains(logged, "unknown") {
		t.Errorf("got %q, logged %q; want empty with unknown-connector reason", got, logged)
	}
	// Empty state vs unknown id are distinct log lines: triage must not
	// guess which guard fired.
	logged = ""
	if got := AuthURL(cfg, "gmail", "", "v", logf); got != "" || !strings.Contains(logged, "empty state") {
		t.Errorf("got %q, logged %q; want empty-state reason", got, logged)
	}
}

// Verifier-less attempts fail at creation, not after user consent.
func TestCreateStateRejectsEmptyVerifier(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.CreateState(ctx, OAuthState{State: "nv", Connector: "gmail", Verifier: ""}); err == nil {
		t.Fatal("empty verifier should fail")
	}
	// Empty-before-Known, matching Exchange: unknown id + empty verifier
	// 400s on empty, not 404s on identity.
	if err := s.CreateState(ctx, OAuthState{State: "nv", Connector: "slack", Verifier: "  "}); err == nil {
		t.Fatal("empty verifier should fail even for unknown id")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("unknown+empty should be a deterministic 400, got %v", err)
	}
}

// Control bytes fail closed at creation, not after consent: an embedded
// NUL/CR/LF would otherwise become a DB filter or an outbound request.
func TestCreateStateRejectsControlBytes(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.CreateState(ctx, OAuthState{State: "s\x00tate", Connector: "gmail", Verifier: testVerifier}); err == nil {
		t.Fatal("control-byte state should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("control state should be a deterministic 400, got %v", err)
	}
	if err := s.CreateState(ctx, OAuthState{State: testState, Connector: "gmail", Verifier: "v\ne"}); err == nil {
		t.Fatal("control-byte verifier should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("control verifier should be a deterministic 400, got %v", err)
	}
	// Invalid UTF-8 fails the same gate: overlong sequences decode as
	// RuneError (not control), so the control check alone would miss them.
	if err := s.CreateState(ctx, OAuthState{State: testState2, Connector: "gmail", Verifier: "v\xc0\x80"}); err == nil {
		t.Fatal("invalid-utf8 verifier should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("invalid utf8 should be a deterministic 400, got %v", err)
	}
}

// Durability contract: memory is not durable (OAuth stays off), Mongo is
// (zero value still reports true — durability is a backend property, not
// row state).
func TestDurableContract(t *testing.T) {
	if NewMemoryStore().Durable() {
		t.Error("memory store must not be durable")
	}
	if !(&MongoStore{}).Durable() {
		t.Error("mongo store must be durable")
	}
}

// AuthURL fails closed on empty state/verifier: a consent URL minted that
// way could only die later at ConsumeState, after user consent.
func TestAuthURLRejectsEmptyParts(t *testing.T) {
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example.com/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	if got := AuthURL(cfg, "gmail", "", "v", nil); got != "" {
		t.Errorf("empty state URL = %q, want empty", got)
	}
	if got := AuthURL(cfg, "gmail", "s", "", nil); got != "" {
		t.Errorf("empty verifier URL = %q, want empty", got)
	}
}

// Whitespace-only plaintext is as unsealable as empty: same missing-value
// class, refused at the same gate.
func TestEncryptTokenRefusesWhitespace(t *testing.T) {
	if _, err := EncryptToken([32]byte{1}, "   "); err == nil {
		t.Fatal("whitespace-only seal should fail")
	}
}

// SetStatus(disconnected) clears grants like Disconnect does (belt and
// braces): the off state never serves a previous grant, whichever verb
// wrote it.
func TestSetStatusDisconnectedClearsTokens(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.SetTokens(ctx, "gmail", TokenRow{RefreshCipher: "r", AccessCipher: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetStatus(ctx, "gmail", StatusDisconnected, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTokens(ctx, "gmail"); err == nil {
		t.Fatal("grant survived SetStatus(disconnected)")
	}
}

// Padded inputs normalise instead of failing: the callback trims query
// params, so stored/built values must match the trimmed form.
func TestAuthURLTrimsInputs(t *testing.T) {
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example.com/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	got := AuthURL(cfg, " GMail ", " "+testState+" ", " "+testVerifier+" ", nil)
	if got == "" {
		t.Fatal("padded known id should build")
	}
	if !strings.Contains(got, "state=s&") && !strings.Contains(got, "state=s") {
		t.Errorf("state not trimmed in URL:\n%s", got)
	}
}

// Whitespace-only access cipher is rejected like whitespace refresh: one
// missing representation (""), enforced at the store.
func TestSetTokensRejectsWhitespaceAccess(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.SetTokens(ctx, "gmail", TokenRow{RefreshCipher: "r", AccessCipher: "   "}); err == nil {
		t.Fatal("whitespace access cipher should fail")
	}
}

// A far-future expiry clamps instead of persisting: the refresher must
// renew soon, not in decades. Pinned on memory (DB tests need Atlas).
func TestSetTokensClampsFarFutureExpiry(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	far := time.Now().Add(365 * 24 * time.Hour).UnixMilli()
	if err := s.SetTokens(ctx, "gmail", TokenRow{RefreshCipher: "r", AccessCipher: "a", AccessExpiryUnixMilli: far}); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetTokens(ctx, "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if max := time.Now().Add(MaxAccessLifetime + time.Minute).UnixMilli(); got.AccessExpiryUnixMilli > max {
		t.Errorf("expiry not clamped: %v", got.AccessExpiryUnixMilli)
	}
}

// Expiry without access is a second missing representation: the row would
// read as holding a grant (non-zero expiry) for a token that doesn't
// exist. Fail closed so the single representation (zero expiry, empty
// access) is the only way to say "no access token".
func TestSetTokensRejectsExpiryWithoutAccess(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.SetTokens(ctx, "gmail", TokenRow{RefreshCipher: "r", AccessExpiryUnixMilli: 123}); err == nil {
		t.Fatal("expiry without access should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("expiry-without-access should be a deterministic 400, got %v", err)
	}
}

// Negative expiries are a third missing representation: UnixMilli is never
// negative for a real token. Reject instead of storing a second spelling
// of "already expired".
func TestSetTokensRejectsNegativeExpiry(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.SetTokens(ctx, "gmail", TokenRow{RefreshCipher: "r", AccessCipher: "a", AccessExpiryUnixMilli: -1}); err == nil {
		t.Fatal("negative expiry should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("negative expiry should be a deterministic 400, got %v", err)
	}
}

// Oversized ciphers are a deterministic reject at the store, not an
// unbounded stored doc — MaxCipherLen, sized to clear the plaintext bound
// plus seal overhead (same number DecryptToken enforces at open).
func TestSetTokensRejectsOversizedCipher(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	big := strings.Repeat("x", MaxCipherLen+1)
	if err := s.SetTokens(ctx, "gmail", TokenRow{RefreshCipher: big}); err == nil {
		t.Fatal("oversized refresh cipher should fail")
	}
	if err := s.SetTokens(ctx, "gmail", TokenRow{RefreshCipher: "r", AccessCipher: big}); err == nil {
		t.Fatal("oversized access cipher should fail")
	}
}

// Padded ciphers are rejected, not trimmed-and-stored: the checked string
// and the stored bytes must round-trip byte-exact.
func TestSetTokensRejectsPaddedCipher(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.SetTokens(ctx, "gmail", TokenRow{RefreshCipher: " r "}); err == nil {
		t.Fatal("padded refresh cipher should fail")
	}
	if err := s.SetTokens(ctx, "gmail", TokenRow{RefreshCipher: "r", AccessCipher: " a "}); err == nil {
		t.Fatal("padded access cipher should fail")
	}
	// Shape before identity: padded for an unknown id 400s on shape, not
	// 404s on identity — same as every other length/shape gate.
	if err := s.SetTokens(ctx, "slack", TokenRow{RefreshCipher: " r "}); err == nil {
		t.Fatal("padded unknown-id cipher should fail")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("padded unknown-id should be a deterministic 400, got %v", err)
	}
}

// Interior-space ciphers fail closed at write time, not at decrypt: they
// would otherwise store as present and fail generically at open.
func TestSetTokensRejectsSpacedCipher(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.SetTokens(ctx, "gmail", TokenRow{RefreshCipher: "r 123"}); err == nil {
		t.Fatal("spaced refresh cipher should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("spaced cipher should be a deterministic 400, got %v", err)
	}
	// Blank and padded keep their own messages: whitespace-only is
	// "missing", edge-padded is "padded" — only interior space lands here.
	if err := s.SetTokens(ctx, "gmail", TokenRow{RefreshCipher: "   "}); err == nil {
		t.Fatal("blank cipher should fail")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream || se.Msg != "connector grant needs a refresh token" {
		t.Fatalf("blank cipher should say missing, got %v", err)
	}
}

// Control-bearing ciphers fail closed at write time, not at decrypt:
// they would otherwise store as present and fail generically at open.
func TestSetTokensRejectsControlCipher(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if err := s.SetTokens(ctx, "gmail", TokenRow{RefreshCipher: "r\x00x"}); err == nil {
		t.Fatal("control-byte cipher should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("control cipher should be a deterministic 400, got %v", err)
	}
}

// Unknown ids with oversized input 400 on length, not 404 on identity:
// length is checked before Known so the two inputs are indistinguishable
// from the known-id oversized path.
func TestSetTokensOversizedUnknownIDIs400(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	big := strings.Repeat("x", MaxCipherLen+1)
	err := s.SetTokens(ctx, "slack", TokenRow{RefreshCipher: big})
	if err == nil {
		t.Fatal("oversized unknown-id should fail")
	}
	if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("oversized unknown-id = %+v, want plain 400", err)
	}
}

// grantClear requires every token column empty: a refresh-less row with a
// leftover access cipher (or expiry) still holds a servable grant.
func TestGrantClearNeedsAllColumns(t *testing.T) {
	if !grantClear(probeDoc{}) {
		t.Error("empty row should be grant-clear")
	}
	if grantClear(probeDoc{Access: " sealed-access "}) {
		t.Error("access-only row should not be grant-clear")
	}
	if grantClear(probeDoc{Expiry: 123}) {
		t.Error("expiry-only row should not be grant-clear")
	}
	// Raw-presence semantics: a whitespace-only cipher is still a stored
	// artifact (unservable via GetTokens, but real enough to repair rather
	// than skip as "clear").
	if grantClear(probeDoc{Refresh: "   "}) {
		t.Error("whitespace-only cipher should read as present, not clear")
	}
}

// Overlong state is a deterministic reject at the store, not a Mongo filter.
func TestConsumeStateRejectsOversized(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if _, err := s.ConsumeState(ctx, strings.Repeat("x", MaxStateFieldLen+1)); err == nil {
		t.Fatal("oversized state should fail")
	} else if se, ok := err.(*StoreError); !ok || !se.NotFound {
		t.Fatalf("oversized should be NotFound, got %v", err)
	}
	// Control bytes fail closed on consume too, never reaching the map:
	// unknown, expired, and malformed are the same NotFound.
	if _, err := s.ConsumeState(ctx, "s\x001"); err == nil {
		t.Fatal("control-byte consume should fail")
	} else if se, ok := err.(*StoreError); !ok || !se.NotFound {
		t.Fatalf("control consume should be NotFound, got %v", err)
	}
	// Invalid UTF-8 fails the same way: overlong sequences decode as
	// RuneError (not control), so the control check alone would miss
	// them before the map/DB filter.
	if _, err := s.ConsumeState(ctx, "s\xc0\x801"); err == nil {
		t.Fatal("invalid-utf8 consume should fail")
	} else if se, ok := err.(*StoreError); !ok || !se.NotFound {
		t.Fatalf("invalid utf8 consume should be NotFound, got %v", err)
	}
	if err := s.CreateState(ctx, OAuthState{State: strings.Repeat("y", MaxStateFieldLen+1), Connector: "gmail", Verifier: testVerifier, ExpiresAt: time.Now().Add(time.Minute)}); err == nil {
		t.Fatal("oversized create should fail")
	}
}

// AuthURL trims and length-gates: padded inputs build a working URL,
// overlong ones fail closed.
func TestAuthURLTrimsAndCaps(t *testing.T) {
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example.com/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	if got := AuthURL(cfg, "gmail", " "+testState+" ", " "+testVerifier+" ", nil); got == "" {
		t.Fatal("padded inputs should build")
	} else if strings.Contains(got, " ") {
		t.Errorf("URL contains a space:\n%s", got)
	}
	if got := AuthURL(cfg, "gmail", strings.Repeat("s", MaxStateFieldLen+1), "v", nil); got != "" {
		t.Fatal("oversized state should fail closed")
	}
}

// AuthURL needs a usable client, not just a non-nil one: empty client id or
// redirect builds no URL.
func TestAuthURLRejectsEmptyClient(t *testing.T) {
	if got := AuthURL(&OAuthConfig{ClientID: "", RedirectURL: "https://app.example.com/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}, "gmail", "s", "v", nil); got != "" {
		t.Errorf("empty client id URL = %q, want empty", got)
	}
	if got := AuthURL(&OAuthConfig{ClientID: "cid", ClientSecret: "", RedirectURL: "https://app.example.com/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}, "gmail", "s", "v", nil); got != "" {
		t.Errorf("empty client secret URL = %q, want empty", got)
	}
	if got := AuthURL(&OAuthConfig{ClientID: "cid", RedirectURL: "", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}, "gmail", "s", "v", nil); got != "" {
		t.Errorf("empty redirect URL = %q, want empty", got)
	}
}

// Displayed scopes and granted scopes must correspond: the app shows
// ScopesFor at connect time, but Google enforces GoogleScopes. A widening
// that updates one table without the other ships a consent screen that
// misstates the grant (or a grant the UI never disclosed).
func TestDisplayScopesMatchGrantedScopes(t *testing.T) {
	// Helpers fold like the store verbs: padded/cased ids answer the same
	// as canonical ones, so direct callers cannot tell them apart.
	if DisplayName(" GMail ") != "Gmail" {
		t.Errorf("DisplayName(padded) = %q, want Gmail", DisplayName(" GMail "))
	}
	if got := ScopesFor(" CALENDAR "); len(got) == 0 || got[0] != "calendar.read" {
		t.Errorf("ScopesFor(padded) = %v, want calendar scopes", got)
	}
	if got := GoogleScopes(" GMail "); len(got) == 0 {
		t.Error("GoogleScopes(padded) should grant like canonical")
	}
	// gmail.read displayed => readonly granted; gmail.draft displayed =>
	// compose granted (draft-only: create drafts, never send).
	has := func(hay []string, needle string) bool {
		for _, s := range hay {
			if s == needle {
				return true
			}
		}
		return false
	}
	granted := map[string][]string{}
	for _, id := range []string{Gmail, Calendar} {
		granted[id] = GoogleScopes(id)
		if len(granted[id]) == 0 {
			t.Fatalf("%s has no granted scopes", id)
		}
	}
	if !has(granted[Gmail], "https://www.googleapis.com/auth/gmail.readonly") ||
		!has(granted[Gmail], "https://www.googleapis.com/auth/gmail.compose") {
		t.Errorf("gmail grant = %v, want readonly+compose", granted[Gmail])
	}
	if !has(granted[Calendar], "https://www.googleapis.com/auth/calendar.readonly") {
		t.Errorf("calendar grant = %v, want readonly", granted[Calendar])
	}
	// No full-access scopes anywhere in v1 (exact match — compose and the
	// read-onlys are the bounded ones).
	tooBroad := map[string]bool{
		"https://www.googleapis.com/auth/gmail.send":      true,
		"https://www.googleapis.com/auth/gmail.modify":    true,
		"https://www.googleapis.com/auth/gmail":           true,
		"https://www.googleapis.com/auth/calendar":        true,
		"https://www.googleapis.com/auth/calendar.events": true,
	}
	for id, scopes := range granted {
		for _, s := range scopes {
			if tooBroad[s] {
				t.Errorf("%s grant %q is broader than v1 allows", id, s)
			}
		}
	}
}

// No-op disconnect on an already-disconnected row still clears tokens: a
// corrupt row (status says off, grant says on) must not keep serving after
// a verb that promises "no future access".
func TestDisconnectNoOpClearsTokens(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if _, err := s.SetStatus(ctx, "gmail", StatusDisconnected, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetTokens(ctx, "gmail", TokenRow{RefreshCipher: "r", AccessCipher: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Disconnect(ctx, "gmail"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTokens(ctx, "gmail"); err == nil {
		t.Fatal("grant survived no-op disconnect")
	}
}

// A partial grant (access cipher but no refresh — hand-edit or future code
// path, since SetTokens itself refuses refresh-less rows) must still die
// on a disconnected no-op: the pure no-op predicate checks every token
// column, not just refresh, and the repair path clears them.
func TestSetStatusNoOpClearsAccessOnlyGrant(t *testing.T) {
	s := NewMemoryStore()
	ctx := context.Background()
	if _, err := s.SetStatus(ctx, "gmail", StatusDisconnected, ""); err != nil {
		t.Fatal(err)
	}
	// Hand-inserted: bypasses SetTokens validation on purpose.
	s.mu.Lock()
	s.tokens["gmail"] = TokenRow{AccessCipher: "sealed-access"}
	s.mu.Unlock()
	if _, err := s.SetStatus(ctx, "gmail", StatusDisconnected, ""); err != nil {
		t.Fatal(err)
	}
	s.mu.RLock()
	_, stillThere := s.tokens["gmail"]
	s.mu.RUnlock()
	if stillThere {
		t.Fatal("access-only grant survived a disconnected no-op")
	}
}

// AuthURL with an overlong state fails closed instead of minting a URL
// Google would 414.
func TestAuthURLRejectsOversizedState(t *testing.T) {
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example.com/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	if got := AuthURL(cfg, "gmail", strings.Repeat("s", MaxStateFieldLen+1), "v", nil); got != "" {
		t.Errorf("oversized state URL = %q, want empty", got[:60])
	}
}

// Control bytes fail closed at mint, matching Exchange/CreateState: a
// minted consent URL for a control-bearing verifier could only
// dead-attempt after the user consented.
func TestAuthURLRejectsControlBytes(t *testing.T) {
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example.com/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	var logged string
	logf := func(format string, args ...any) { logged = fmt.Sprintf(format, args...) }
	if got := AuthURL(cfg, "gmail", testState, testVerifier[:60]+"\x00x", logf); got != "" || !strings.Contains(logged, "invalid") {
		t.Errorf("got %q, logged %q; want empty with invalid-input reason", got, logged)
	}
	// A control-bearing id fails the same gate as Exchange: quoting alone
	// is not the enforcement.
	logged = ""
	if got := AuthURL(cfg, "gm\x00ail", testState, testVerifier, logf); got != "" || !strings.Contains(logged, "invalid") {
		t.Errorf("got %q, logged %q; want empty with invalid-id reason", got, logged)
	}
	// Invalid UTF-8 fails the same gate: overlong sequences decode as
	// RuneError (not control), so the control check alone would miss them.
	logged = ""
	if got := AuthURL(cfg, "gmail", testState, strings.Repeat("v", 60)+"\xc0\x80", logf); got != "" || !strings.Contains(logged, "invalid") {
		t.Errorf("got %q, logged %q; want empty with invalid-input reason", got, logged)
	}
	// Trailing zero-width from a console copy-paste is forgiven like
	// whitespace (trimmed at the edges), not rejected as a control:
	// TrimCredential covers Cf where TrimSpace stopped.
	logged = ""
	if got := AuthURL(cfg, "gmail", testState+"\u200b", testVerifier, logf); got == "" {
		t.Errorf("got empty, want trimmed Cf to build; log: %q", logged)
	}
}

// DecryptToken("") errors by design: "" is the documented missing-token
// sentinel and must be checked by the caller before decrypting, not
// collapsed into a successful empty decrypt.
func TestDecryptTokenRejectsEmpty(t *testing.T) {
	if _, err := DecryptToken([32]byte{1}, ""); err == nil {
		t.Fatal("empty sealed input should fail")
	}
}

// Refusal/policy sync: IsDraftOnlySend must name exactly the send paths,
// and the same tools must gate (not refuse-or-run-free) in the relay
// policy. A renamed tool misses the refusal and falls through to the
// gated backstop — safe, but the agent stalls on an approval for the
// exact flow v1 sells ("create a draft instead"). Sync this list with
// the wired servers' real tool names whenever they change.
func TestIsDraftOnlySendMatchesServers(t *testing.T) {
	refused := map[string][]string{
		"gmail":    {"send_message", "send_gmail"},
		"calendar": {"create_event", "send_invite"},
	}
	free := map[string][]string{
		"gmail":    {"search_gmail", "read_gmail", "list_messages", "create_draft"},
		"calendar": {"list_events", "get_event", "create_event_draft"},
	}
	for server, tools := range refused {
		for _, tool := range tools {
			if !IsDraftOnlySend(server, tool) {
				t.Errorf("%s/%s should be refused outright", server, tool)
			}
		}
	}
	for server, tools := range free {
		for _, tool := range tools {
			if IsDraftOnlySend(server, tool) {
				t.Errorf("%s/%s should run, not be refused", server, tool)
			}
		}
	}
	if IsDraftOnlySend("gmail", "unknown_tool_xyz") {
		t.Error("unknown tools must not be refused")
	}
	// Cf-suffixed inputs fold like clean ones: a zero-width-suffixed pair
	// must hit the same refusal, not slip to the gated backstop.
	if !IsDraftOnlySend("gmail⁠", "send_message⁠") {
		t.Error("Cf-suffixed send should still be refused")
	}
	// Hyphen spellings refuse like underscores: a wired send-message is
	// the same tool as send_message, so punctuation cannot drift the
	// refusal list the way the scope test guards against renames.
	if !IsDraftOnlySend("gmail", "send-message") {
		t.Error("hyphenated send should still be refused")
	}
	if !IsDraftOnlySend("calendar", "create-event") {
		t.Error("hyphenated create should still be refused")
	}
	if !IsDraftOnlySend("gmail", "send.message") {
		t.Error("dotted send should still be refused")
	}
	// Oversize defers to the backstop on both parts: an oversized id is
	// unknown by construction, and an oversized tool can never be a real
	// send path — the gated backstop (approval hold) is the honest answer,
	// not a refusal text naming a draft workflow for a tool that cannot
	// exist. A future legit upstream tool longer than the cap flips from
	// gated to gated when the cap widens, never gated to refused.
	if IsDraftOnlySend(strings.Repeat("x", MaxConnectorIDLen+1), "send_message") {
		t.Error("oversized server should fall to the backstop, not refuse")
	}
	if IsDraftOnlySend("gmail", strings.Repeat("x", maxToolNameLen+1)) {
		t.Error("oversized tool should fall to the backstop, not refuse")
	}
}

// Guard order is a contract: empty → length → control → shape → Known
// holds in AuthURL, Exchange, and both CreateStates, so an oversized or
// empty input for an unknown id 400s identically everywhere instead of
// 404ing on identity in one layer and 400ing on shape in another.
// (Mongo mirrors the memory order by construction; it needs Atlas to run,
// so the memory leg pins the contract here.)
func TestGuardOrderMatchesAcrossLayers(t *testing.T) {
	ctx := context.Background()
	bigID := "slack"
	big := strings.Repeat("x", MaxStateFieldLen+1)
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example.com/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	check400 := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: oversized unknown-id should fail", name)
		}
		if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
			t.Fatalf("%s: want deterministic 400, got %v", name, err)
		}
	}
	if got := AuthURL(cfg, bigID, big, testVerifier, nil); got != "" {
		t.Errorf("AuthURL oversized unknown-id = %q, want empty", got)
	}
	check400("Exchange", func() error { _, err := Exchange(ctx, cfg, bigID, big, testVerifier, nil); return err }())
	check400("memory CreateState", NewMemoryStore().CreateState(ctx, OAuthState{State: big, Connector: bigID, Verifier: testVerifier}))
}

// TrimCredential forgives edge Cf like whitespace but never interior
// controls: padded input trims, embedded control still fails closed.
func TestTrimCredential(t *testing.T) {
	// U+200B zero-width space is Cf, not ASCII whitespace:
	// TrimSpace leaves it, TrimCredential forgives it at the edges but
	// never in the middle (interior Cf must still fail HasControl).
	if got := TrimCredential("\u200b\u200bgmail\u200b\u200b"); got != "gmail" {
		t.Errorf("TrimCredential padded = %q, want gmail", got)
	}
	if !HasControl(TrimCredential("a\u200bb")) {
		t.Error("interior Cf must survive trimming and still fail HasControl")
	}
}

// Oversized ids fail closed before any fold/lookup: ids are two short
// fixed strings, so anything over the cap is certainly unknown — no
// normalize alloc, no quoted value, same 400 everywhere.
func TestOversizedIDFailsClosed(t *testing.T) {
	ctx := context.Background()
	big := strings.Repeat("x", MaxConnectorIDLen+1)
	if Known(big) {
		t.Error("oversized id must not be known")
	}
	if DisplayName(big) != "Unknown" {
		t.Errorf("oversized DisplayName = %q, want Unknown", DisplayName(big))
	}
	if ScopesFor(big) != nil {
		t.Errorf("oversized ScopesFor = %v, want nil", ScopesFor(big))
	}
	if GoogleScopes(big) != nil {
		t.Errorf("oversized GoogleScopes = %v, want nil", GoogleScopes(big))
	}
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example.com/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	if got := AuthURL(cfg, big, "s", "v", nil); got != "" {
		t.Errorf("oversized AuthURL id = %q, want empty", got)
	}
	if _, err := Exchange(ctx, cfg, big, "code", testVerifier, nil); err == nil {
		t.Error("oversized Exchange id should fail")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Errorf("oversized Exchange id should be a deterministic 400, got %v", err)
	}
	s := NewMemoryStore()
	for name, err := range map[string]error{
		"Get":         func() error { _, err := s.Get(ctx, big); return err }(),
		"SetStatus":   func() error { _, err := s.SetStatus(ctx, big, StatusConnected, ""); return err }(),
		"Disconnect":  func() error { _, err := s.Disconnect(ctx, big); return err }(),
		"SetTokens":   s.SetTokens(ctx, big, TokenRow{RefreshCipher: "r"}),
		"GetTokens":   func() error { _, err := s.GetTokens(ctx, big); return err }(),
		"CreateState": s.CreateState(ctx, OAuthState{State: "s1", Connector: big, Verifier: testVerifier}),
	} {
		if err == nil {
			t.Errorf("oversized store %s id should fail", name)
		} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
			t.Errorf("oversized store %s id should be a deterministic 400, got %v", name, err)
		}
	}
}

// Control-bearing ids fail closed before Known: the "unknown connector"
// echo must never carry controls into the response body or log line.
func TestInvalidIDFailsClosed(t *testing.T) {
	ctx := context.Background()
	bad := "gm\nail"
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example.com/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	if got := AuthURL(cfg, bad, "s", testVerifier, nil); got != "" {
		t.Errorf("control-id AuthURL = %q, want empty", got)
	}
	if _, err := Exchange(ctx, cfg, bad, "code", testVerifier, nil); err == nil {
		t.Error("control-id Exchange should fail")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Errorf("control-id Exchange should be a deterministic 400, got %v", err)
	}
	s := NewMemoryStore()
	for name, err := range map[string]error{
		"Get":         func() error { _, err := s.Get(ctx, bad); return err }(),
		"Disconnect":  func() error { _, err := s.Disconnect(ctx, bad); return err }(),
		"SetTokens":   s.SetTokens(ctx, bad, TokenRow{RefreshCipher: "r"}),
		"CreateState": s.CreateState(ctx, OAuthState{State: "s1", Connector: bad, Verifier: testVerifier}),
	} {
		if err == nil {
			t.Errorf("control-id store %s should fail", name)
		} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
			t.Errorf("control-id store %s should be a deterministic 400, got %v", name, err)
		} else if strings.Contains(se.Msg, "\n") {
			t.Errorf("control-id store %s echo carries the control: %q", name, se.Msg)
		}
	}
}

// A spaced envelope key fails with the specific discriminant, not the
// generic 64-hex-chars message — same vocabulary as the client-credential
// gates for the same operator-paste class.
func TestKeyFromEnvRejectsInteriorSpace(t *testing.T) {
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", "0123456789abcdef 0123456789abcdef0123456789abcdef0123456789abcdef")
	if _, err := KeyFromEnv(); err == nil {
		t.Fatal("spaced key should fail closed")
	} else if !strings.Contains(err.Error(), "spaces") {
		t.Fatalf("spaced key should name the class, got %q", err)
	}
}

// Interior whitespace in state/verifier fails like controls: server-minted
// base64url never contains spaces, so a spaced input is paste or garbage —
// deterministic 400 at every layer, never a 502 at Google after consent.
func TestRejectsInteriorWhitespace(t *testing.T) {
	ctx := context.Background()
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example.com/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	if got := AuthURL(cfg, "gmail", testState[:10]+" "+testState[11:], testVerifier, nil); got != "" {
		t.Errorf("spaced AuthURL state = %q, want empty", got)
	}
	if _, err := Exchange(ctx, cfg, Gmail, "co dexxxxxxxxxxxxxx", testVerifier, nil); err == nil {
		t.Fatal("spaced Exchange code should fail")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("spaced code should be a deterministic 400, got %v", err)
	}
	s := NewMemoryStore()
	if err := s.CreateState(ctx, OAuthState{State: "s 1", Connector: "gmail", Verifier: testVerifier}); err == nil {
		t.Fatal("spaced CreateState should fail")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("spaced state should be a deterministic 400, got %v", err)
	}
	if _, err := s.ConsumeState(ctx, "s 1"); err == nil {
		t.Fatal("spaced ConsumeState should fail")
	} else if se, ok := err.(*StoreError); !ok || !se.NotFound {
		t.Fatalf("spaced consume should be NotFound, got %v", err)
	}
}

// Bound layering is a contract: the raw pre-filter must never be tighter
// than the contract gate it feeds, or valid input dies before the checks
// that understand it. Same value today, independent names by design — this
// pins the ordering, not the number.
func TestBoundLayering(t *testing.T) {
	if MaxSegmentAllocLen < MaxConnectorIDLen {
		t.Errorf("MaxSegmentAllocLen (%d) must clear MaxConnectorIDLen (%d)", MaxSegmentAllocLen, MaxConnectorIDLen)
	}
	if MaxSegmentAllocLen < MaxStateFieldLen {
		t.Errorf("MaxSegmentAllocLen (%d) must clear MaxStateFieldLen (%d)", MaxSegmentAllocLen, MaxStateFieldLen)
	}
}
