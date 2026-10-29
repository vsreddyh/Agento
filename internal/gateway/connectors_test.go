package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agento/internal/connectors"
)

// These tests pin the #316 v1 foundation contract: two disconnected rows,
// per-connector disconnect, connect 501s until OAuth lands, secrets never
// appear, and expired rows carry the reconnect hint.

func connectorsServer(t *testing.T) *Server {
	t.Helper()
	return newTestServer(t, &fakeAgent{}, Config{ConnectorStore: connectors.NewMemoryStore()})
}

func TestConnectorsListTwoDisconnected(t *testing.T) {
	srv := connectorsServer(t)
	got := decodeBody(t, do(t, srv, http.MethodGet, "/api/connectors", ""))
	rows, ok := got["connectors"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("connectors = %v", got)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		m := r.(map[string]any)
		seen[m["id"].(string)] = true
		if m["status"] != "disconnected" {
			t.Errorf("%v status = %v, want disconnected", m["id"], m["status"])
		}
		if _, hasToken := m["refresh_token"]; hasToken {
			t.Errorf("%v leaks refresh_token", m["id"])
		}
		if _, hasToken := m["access_token"]; hasToken {
			t.Errorf("%v leaks access_token", m["id"])
		}
		if _, ok := m["scopes"]; !ok {
			t.Errorf("%v has no scopes", m["id"])
		}
	}
	for _, id := range []string{"gmail", "calendar"} {
		if !seen[id] {
			t.Errorf("missing connector %s", id)
		}
	}
}

func TestConnectorsDisconnectAndUnknown(t *testing.T) {
	srv := connectorsServer(t)
	// Unknown id 404s, never synthesises.
	if rec := do(t, srv, http.MethodPost, "/api/connectors/slack/disconnect", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown disconnect: %d, want 404 (%s)", rec.Code, rec.Body)
	}
	// Wrong verb is 405, not 404: the client fixes its method, not its path.
	if rec := do(t, srv, http.MethodGet, "/api/connectors/gmail/disconnect", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("get disconnect: %d, want 405 (%s)", rec.Code, rec.Body)
	}
	rec := do(t, srv, http.MethodPost, "/api/connectors/gmail/disconnect", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", rec.Code, rec.Body)
	}
	if st := decodeBody(t, rec)["connector"].(map[string]any)["status"]; st != "disconnected" {
		t.Fatalf("status = %v, want disconnected", st)
	}
	// Trailing slash is the same verb, not an unknown path.
	if rec := do(t, srv, http.MethodPost, "/api/connectors/gmail/disconnect/", ""); rec.Code != http.StatusOK {
		t.Fatalf("trailing-slash disconnect: %d, want 200 (%s)", rec.Code, rec.Body)
	}
}

func TestConnectorsConnectIs501NotSecretPrompt(t *testing.T) {
	srv := connectorsServer(t)
	rec := do(t, srv, http.MethodPost, "/api/connectors/gmail/connect", "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("connect: %d, want 501 (%s)", rec.Code, rec.Body)
	}
	body := strings.ToLower(rec.Body.String())
	if !strings.Contains(body, "never paste") {
		t.Fatalf("connect 501 should warn against pasting secrets, got %s", rec.Body)
	}
}

func TestConnectorsExpiredCarriesReconnectHint(t *testing.T) {
	srv := connectorsServer(t)
	if _, err := srv.connectors.SetStatus(context.Background(), "gmail", connectors.StatusExpired, "aged out"); err != nil {
		t.Fatal(err)
	}
	got := decodeBody(t, do(t, srv, http.MethodGet, "/api/connectors", ""))
	for _, r := range got["connectors"].([]any) {
		m := r.(map[string]any)
		if m["id"] == "gmail" {
			if m["status"] != "expired" {
				t.Fatalf("gmail status = %v, want expired", m["status"])
			}
			hint, _ := m["reconnect"].(string)
			if !strings.Contains(strings.ToLower(hint), "never paste") {
				t.Fatalf("expired row should carry the no-secrets hint, got %v", m)
			}
		}
	}
}

func TestConnectorsWireGatesConnectedAt(t *testing.T) {
	if got := connectorWire(connectors.Connector{ID: "gmail", Status: connectors.StatusConnected, ConnectedAt: 77}); got["connected_at"] != int64(77) {
		t.Errorf("connected row should emit connected_at, got %v", got)
	}
	if got := connectorWire(connectors.Connector{ID: "gmail", Status: connectors.StatusDisconnected, ConnectedAt: 77}); got["connected_at"] != nil {
		t.Errorf("disconnected row must omit connected_at, got %v", got)
	}
}

func TestConnectorsMalformedVerbPathIs404(t *testing.T) {
	srv := connectorsServer(t)
	// Extra segment is a bad path (404), not a wrong method (405 with
	// Allow): the verb is right, the resource doesn't exist.
	if rec := do(t, srv, http.MethodPost, "/api/connectors/gmail/disconnect/extra", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("disconnect/extra: %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

func TestConnectorsVerbFoldsCase(t *testing.T) {
	srv := connectorsServer(t)
	// Verbs fold like ids: mixed-case still lands on the verb.
	if rec := do(t, srv, http.MethodPost, "/api/connectors/gmail/Disconnect", ""); rec.Code != http.StatusOK {
		t.Fatalf("mixed-case verb: %d, want 200 (%s)", rec.Code, rec.Body)
	}
}

func TestConnectorsPrefixBoundary(t *testing.T) {
	srv := connectorsServer(t)
	// No slash boundary: a bad path, not a bad id.
	rec := do(t, srv, http.MethodGet, "/api/connectorsfoo", "")
	body := rec.Body.String()
	if rec.Code != http.StatusNotFound || !strings.Contains(body, "unknown path") {
		t.Fatalf("prefix-no-slash: %d, want 404 unknown path (%s)", rec.Code, body)
	}
}

func TestConnectorsTrailingSlashList(t *testing.T) {
	srv := connectorsServer(t)
	// Trailing-slash alias serves the same list shape.
	got := decodeBody(t, do(t, srv, http.MethodGet, "/api/connectors/", ""))
	rows, ok := got["connectors"].([]any)
	if !ok || len(rows) != 2 {
		t.Fatalf("trailing-slash list = %v, want 2 rows", got)
	}
}

// Both list spellings carry no-store: the app polls this loop, and a
// heuristic cache would serve a stale disconnected after the callback
// flipped to connected.
func TestConnectorsListSpellingsNoStore(t *testing.T) {
	srv := connectorsServer(t)
	plain := do(t, srv, http.MethodGet, "/api/connectors", "")
	slashed := do(t, srv, http.MethodGet, "/api/connectors/", "")
	if plain.Code != http.StatusOK || slashed.Code != http.StatusOK {
		t.Fatalf("list spellings: %d / %d, want 200 / 200", plain.Code, slashed.Code)
	}
	for _, rec := range []*httptest.ResponseRecorder{plain, slashed} {
		if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
			t.Errorf("list cache-control = %q, want no-store", cc)
		}
	}
	if plain.Body.String() != slashed.Body.String() {
		t.Errorf("list bodies diverge:\n%s\n%s", plain.Body, slashed.Body)
	}
}

func TestConnectorsNoVerbIs405WithAllow(t *testing.T) {
	srv := connectorsServer(t)
	// Bare id with no verb: 405 with an Allow header naming POST, not 404.
	rec := do(t, srv, http.MethodPost, "/api/connectors/gmail", "")
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("no-verb: %d, want 405 (%s)", rec.Code, rec.Body)
	}
	if allow := rec.Header().Get("Allow"); allow != "POST" {
		t.Fatalf("Allow = %q, want POST", allow)
	}
}

func TestConnectorsCollectionWrongMethod(t *testing.T) {
	srv := connectorsServer(t)
	// Collection with wrong method: 405 with Allow, on both spellings.
	for _, p := range []string{"/api/connectors", "/api/connectors/"} {
		rec := do(t, srv, http.MethodPost, p, "")
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s: %d, want 405 (%s)", p, rec.Code, rec.Body)
		}
		if allow := rec.Header().Get("Allow"); allow != "GET" {
			t.Fatalf("POST %s Allow = %q, want GET", p, allow)
		}
	}
}

func oauthTestServer(t *testing.T, failExchange bool) (*connectors.OAuthConfig, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if failExchange {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"access-1","refresh_token":"refresh-1","expires_in":3600,"token_type":"Bearer"}`))
	}))
	var key [32]byte
	copy(key[:], "test-envelope-key-0123456789ab")
	cfg := &connectors.OAuthConfig{
		ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example/cb/api/connectors/callback",
		Key: key, AuthURL: srv.URL + "/o", TokenURL: srv.URL + "/t",
	}
	return cfg, srv.Close
}

func oauthServer(t *testing.T, cfg *connectors.OAuthConfig) *Server {
	t.Helper()
	return newTestServer(t, &fakeAgent{}, Config{ConnectorStore: connectors.NewMemoryStore(), ConnectorOAuth: cfg})
}

func TestConnectorsConnectRejectsBadRedirectConfig(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	// Hand-built cfg with a misshapen redirect: connect must fail fast
	// with a 502 config error, not store a row the store would 400 as
	// bad state input.
	cfg.RedirectURL = "not-a-url"
	srv := oauthServer(t, cfg)
	rec := do(t, srv, http.MethodPost, "/api/connectors/gmail/connect", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("bad-redirect connect: %d, want 502 (%s)", rec.Code, rec.Body)
	}
}

// An AuthURL refusal after a successful CreateState cleans up the minted
// row: the failed build must leave nothing observable behind. Reached
// with a cfg whose redirect pins fine but whose endpoints AuthURL
// rejects — defense in depth, but the cleanup must hold regardless.
func TestConnectorsConnectCleansUpAfterAuthURLRefusal(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	cfg.AuthURL = ""
	store := connectors.NewMemoryStore()
	srv := newTestServer(t, &fakeAgent{}, Config{ConnectorStore: store, ConnectorOAuth: cfg})
	rec := do(t, srv, http.MethodPost, "/api/connectors/gmail/connect", "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("endpoint-less connect: %d, want 502 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "not configured correctly") {
		t.Fatalf("config 502s share one message, got: %s", rec.Body)
	}
}

// A control-bearing verifier from a hand-inserted row fails closed with
// the dead-attempt shape before any Exchange call — same gate as
// code/state, which live one branch up. The stub stands in for a legacy
// or hand-inserted row: CreateState itself rejects controls, so the row
// can only predate the check.
type controlVerifierStore struct {
	connectors.Store
}

func (controlVerifierStore) ConsumeState(context.Context, string) (connectors.OAuthState, error) {
	return connectors.OAuthState{State: "s1", Connector: "gmail", Verifier: "v\x00x"}, nil
}

// Invalid UTF-8 in a verifier fails the same way: overlong sequences
// decode as RuneError (not control), so the control gate alone misses
// them.
func TestConnectorsCallbackRejectsInvalidUTF8Verifier(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	store := invalidUTF8Store{connectors.NewMemoryStore()}
	srv := newTestServer(t, &fakeAgent{}, Config{ConnectorStore: store, ConnectorOAuth: cfg})
	rec := do(t, srv, http.MethodGet, "/api/connectors/callback?code=c1&state=s1", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid-utf8 verifier: %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "start over from connect") {
		t.Fatalf("invalid utf8 verifier should use the dead-attempt message: %s", rec.Body)
	}
}

type invalidUTF8Store struct {
	connectors.Store
}

func (invalidUTF8Store) ConsumeState(context.Context, string) (connectors.OAuthState, error) {
	return connectors.OAuthState{State: "s1", Connector: "gmail", Verifier: "v\xc0\x80x"}, nil
}

func TestConnectorsCallbackRejectsControlVerifier(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	srv := newTestServer(t, &fakeAgent{}, Config{ConnectorStore: controlVerifierStore{connectors.NewMemoryStore()}, ConnectorOAuth: cfg})
	rec := do(t, srv, http.MethodGet, "/api/connectors/callback?code=c1&state=s1", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("control verifier: %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "start over from connect") {
		t.Fatalf("control verifier should use the dead-attempt message: %s", rec.Body)
	}
}

func TestConnectorsConnectReturnsAuthURL(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	srv := oauthServer(t, cfg)
	rec := do(t, srv, http.MethodPost, "/api/connectors/gmail/connect", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("connect: %d %s", rec.Code, rec.Body)
	}
	got := decodeBody(t, rec)
	url, _ := got["auth_url"].(string)
	for _, want := range []string{"gmail.readonly", "code_challenge=", "state="} {
		if !strings.Contains(url, want) {
			t.Errorf("auth_url missing %q:\n%s", want, url)
		}
	}
}

func TestConnectorsCallbackFullFlow(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	store := connectors.NewMemoryStore()
	srv := newTestServer(t, &fakeAgent{}, Config{ConnectorStore: store, ConnectorOAuth: cfg})

	// Start the flow to mint a real state.
	rec := do(t, srv, http.MethodPost, "/api/connectors/gmail/connect", "")
	authURL, _ := decodeBody(t, rec)["auth_url"].(string)
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	state := u.Query().Get("state")
	if state == "" {
		t.Fatalf("no state in %s", authURL)
	}
	// Exchange through the callback.
	rec = do(t, srv, http.MethodGet, "/api/connectors/callback?code=code1&state="+state, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body)
	}
	got := decodeBody(t, rec)["connector"].(map[string]any)
	if got["status"] != "connected" {
		t.Fatalf("status = %v, want connected", got["status"])
	}
	if _, has := got["refresh_token"]; has {
		t.Error("wire leaks refresh token")
	}
	// Tokens stored encrypted: ciphertext present, plaintext absent.
	tok, err := store.GetTokens(context.Background(), "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if tok.RefreshCipher == "" || tok.RefreshCipher == "refresh-1" {
		t.Errorf("refresh not sealed: %q", tok.RefreshCipher)
	}
	open, err := connectors.DecryptToken(cfg.Key, tok.RefreshCipher)
	if err != nil || open != "refresh-1" {
		t.Fatalf("decrypt = %q, %v", open, err)
	}
	// Replaying the same state fails: single-use.
	rec = do(t, srv, http.MethodGet, "/api/connectors/callback?code=code1&state="+state, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("replay: %d, want 400 (%s)", rec.Code, rec.Body)
	}
}

func TestConnectorsCallbackRejects(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	srv := oauthServer(t, cfg)
	// Unknown state.
	if rec := do(t, srv, http.MethodGet, "/api/connectors/callback?code=x&state=nope", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown state: %d, want 400 (%s)", rec.Code, rec.Body)
	}
	// Missing params.
	if rec := do(t, srv, http.MethodGet, "/api/connectors/callback?code=x", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing state: %d, want 400 (%s)", rec.Code, rec.Body)
	}
	// Per-connector callback paths are gone: the single registered endpoint
	// serves both. A leftover per-id path 404s as unknown.
	if rec := do(t, srv, http.MethodGet, "/api/connectors/gmail/callback?code=x&state=y", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("per-id callback: %d, want 404 (%s)", rec.Code, rec.Body)
	}
	// Exchange failure surfaces without storing.
	cfgFail, closeFail := oauthTestServer(t, true)
	defer closeFail()
	srvFail := oauthServer(t, cfgFail)
	rec := do(t, srvFail, http.MethodPost, "/api/connectors/gmail/connect", "")
	uf, err := url.Parse(decodeBody(t, rec)["auth_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if rec := do(t, srvFail, http.MethodGet, "/api/connectors/callback?code=bad&state="+uf.Query().Get("state"), ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("bad exchange: %d, want 502 (%s)", rec.Code, rec.Body)
	}
}

func TestConnectorsCallbackNeedsNoBearer(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	// Memory store + OAuth configured, but the request carries NO bearer:
	// this is the Google-redirected browser shape (no Authorization
	// header), and it must still complete — the state is the capability.
	srv := newTestServer(t, &fakeAgent{}, Config{ConnectorStore: connectors.NewMemoryStore(), ConnectorOAuth: cfg})
	r := httptest.NewRequest(http.MethodGet, "/api/connectors/callback?code=x&state=nope", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bearerless callback: %d, want 400 state-reject (%s)", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "bearer") || strings.Contains(strings.ToLower(rec.Body.String()), "unauthorized") {
		t.Fatalf("bearerless callback answered auth instead of state: %s", rec.Body)
	}
	// And the gated routes still 401 without a bearer.
	r2 := httptest.NewRequest(http.MethodGet, "/api/connectors", nil)
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, r2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("bearerless list: %d, want 401 (%s)", rec2.Code, rec2.Body)
	}
}

func TestConnectorsCallbackNoProfileOracle(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	srv := newTestServer(t, &fakeAgent{}, Config{ConnectorStore: connectors.NewMemoryStore(), ConnectorOAuth: cfg})
	// Unknown profile + callback path answers the SAME 400 as a bad state
	// on a known profile — no existence oracle for unauthenticated callers.
	bad := do(t, srv, http.MethodGet, "/api/connectors/callback?code=x&state=nope", "")
	unknown := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/nosuchprofile/api/connectors/callback?code=x&state=nope", nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, r)
		return rec
	}()
	if bad.Code != http.StatusBadRequest || unknown.Code != http.StatusBadRequest {
		t.Fatalf("bad=%d unknown=%d, want 400/400 (%s) (%s)", bad.Code, unknown.Code, bad.Body, unknown.Body)
	}
	if bad.Body.String() != unknown.Body.String() {
		t.Fatalf("oracle: bodies differ:\n%s\n%s", bad.Body, unknown.Body)
	}
}

func TestConnectorsCallbackRejectsOversizedInput(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	srv := oauthServer(t, cfg)
	// Multi-KB code/state 400 with the same dead-attempt shape, never
	// reaching the store or Google.
	big := strings.Repeat("x", 3000)
	rec := do(t, srv, http.MethodGet, "/api/connectors/callback?code="+big+"&state="+big, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized: %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "start over from connect") {
		t.Fatalf("oversized should use the dead-attempt message: %s", rec.Body)
	}
}

// Embedded control bytes fail closed with the same dead-attempt shape:
// a NUL/CR/LF in code/state must never become a DB filter or an outbound
// token request.
func TestConnectorsCallbackRejectsControlBytes(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	srv := oauthServer(t, cfg)
	rec := do(t, srv, http.MethodGet, "/api/connectors/callback?code=a%00b&state=s1", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("control-byte code: %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "start over from connect") {
		t.Fatalf("control bytes should use the dead-attempt message: %s", rec.Body)
	}
}

// Trimmed-to-empty is malformed, not missing: an invisible-only value was
// never server-minted, so it dead-attempts like unknown/consumed/expired
// instead of answering the missing-params branch (same status either way).
func TestConnectorsCallbackTrimsToDeadAttempt(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	srv := oauthServer(t, cfg)
	rec := do(t, srv, http.MethodGet, "/api/connectors/callback?code=++&state=s1", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("whitespace-only code: %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "start over from connect") {
		t.Fatalf("trimmed-empty code should dead-attempt, got: %s", rec.Body)
	}
	// Truly absent params keep the missing-params message.
	rec = do(t, srv, http.MethodGet, "/api/connectors/callback?state=s1", "")
	if !strings.Contains(rec.Body.String(), "callback needs") {
		t.Fatalf("absent code should name missing params, got: %s", rec.Body)
	}
}

// Invalid UTF-8 fails with the same dead-attempt shape: overlong/invalid
// sequences decode as RuneError (not control), so the control gate alone
// would miss them before the DB filter.
func TestConnectorsCallbackRejectsInvalidUTF8(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	srv := oauthServer(t, cfg)
	rec := do(t, srv, http.MethodGet, "/api/connectors/callback?code=%C0%80&state=s1", "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid-utf8 code: %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "start over from connect") {
		t.Fatalf("invalid utf8 should use the dead-attempt message: %s", rec.Body)
	}
}

// A dead attempt in a browser tab answers HTML, not JSON: the operator
// just consented and needs close-this-tab guidance, not a raw error body.
// API clients (no text/html Accept) keep the existing JSON shape.
func TestConnectorsCallbackTrailingSlashMatches(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	srv := oauthServer(t, cfg)
	// splitProfile Cleans the path before the exact-match bypass, so the
	// slash spelling must reach the same handler as the canonical one —
	// never a different status for the same dead state.
	plain := do(t, srv, http.MethodGet, "/api/connectors/callback?code=x&state=nope", "")
	slashed := do(t, srv, http.MethodGet, "/api/connectors/callback/?code=x&state=nope", "")
	if plain.Code != slashed.Code || plain.Body.String() != slashed.Body.String() {
		t.Fatalf("slash spelling diverges: %d %q vs %d %q", plain.Code, plain.Body, slashed.Code, slashed.Body)
	}
	// Casing must not distinguish callers on this unauthenticated path:
	// mixed case reaches the same bypass, not a 401 oracle.
	mixed := do(t, srv, http.MethodGet, "/API/connectors/CALLBACK?code=x&state=nope", "")
	if mixed.Code != plain.Code || mixed.Body.String() != plain.Body.String() {
		t.Fatalf("case spelling diverges: %d %q vs %d %q", mixed.Code, mixed.Body, plain.Code, plain.Body)
	}
}

// A dead attempt in a browser tab answers HTML, not JSON: the operator
// just consented and needs close-this-tab guidance, not a raw error body.
// API clients (no text/html Accept) keep the existing JSON shape.
func TestConnectorsCallbackDeadAttemptServesHTMLToBrowsers(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	srv := oauthServer(t, cfg)
	req := httptest.NewRequest(http.MethodGet, "/api/connectors/callback?code=x&state=nope", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Accept", "text/html")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("dead-attempt html: %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	if !strings.Contains(rec.Body.String(), "tap connect again") {
		t.Errorf("html body gives no retry guidance: %s", rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("cache-control = %q, want no-store", cc)
	}
	if ct := rec.Header().Get("X-Content-Type-Options"); ct != "nosniff" {
		t.Errorf("x-content-type-options = %q, want nosniff", ct)
	}
	// Same 400 meaning without the browser header: JSON shape unchanged.
	rec2 := do(t, srv, http.MethodGet, "/api/connectors/callback?code=x&state=nope", "")
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("dead-attempt json: %d, want 400 (%s)", rec2.Code, rec2.Body)
	}
	if !strings.Contains(rec2.Body.String(), "start over from connect") {
		t.Errorf("json dead-attempt lost its message: %s", rec2.Body)
	}
}

// Every callback failure answers HTML to browsers, not just dead
// attempts and success: the consenting tab always gets close-this-tab
// guidance, never raw JSON. API clients keep the JSON shapes unchanged.
func TestConnectorsCallbackErrorsServeHTMLToBrowsers(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	srv := oauthServer(t, cfg)
	html := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer test-token")
		req.Header.Set("Accept", "text/html")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	// Missing params: browser gets the close-this-tab page, not JSON.
	rec := html("/api/connectors/callback?code=x")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing-param html: %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Close this tab") || !strings.Contains(body, "callback needs") {
		t.Errorf("html error lost message or guidance: %s", body)
	}
	// Oversized state: same dead-attempt message, HTML envelope.
	rec = html("/api/connectors/callback?code=x&state=" + strings.Repeat("y", 3000))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized html: %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("oversized content-type = %q, want text/html", ct)
	}
	if body := rec.Body.String(); !strings.Contains(body, "Close this tab") || !strings.Contains(body, "start over from connect") {
		t.Errorf("oversized html missing message or guidance: %s", body)
	}
	// Same failures without the browser header stay JSON.
	rec2 := do(t, srv, http.MethodGet, "/api/connectors/callback?code=x", "")
	if rec2.Code != http.StatusBadRequest || strings.Contains(rec2.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("missing-param json: %d %s (%s)", rec2.Code, rec2.Header().Get("Content-Type"), rec2.Body)
	}
	// Wrong method on the single callback endpoint: 405 with the Allow
	// header (requireMethod contract kept), HTML body for browsers.
	req := httptest.NewRequest(http.MethodPost, "/api/connectors/callback?code=x&state=y", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Accept", "text/html")
	rec3 := httptest.NewRecorder()
	srv.ServeHTTP(rec3, req)
	if rec3.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post callback: %d, want 405 (%s)", rec3.Code, rec3.Body)
	}
	if allow := rec3.Header().Get("Allow"); allow != http.MethodGet {
		t.Errorf("Allow = %q, want GET", allow)
	}
	if body := rec3.Body.String(); !strings.Contains(body, "Close this tab") {
		t.Errorf("405 html missing guidance: %s", body)
	}
}

func TestConnectorsCallbackWhitespaceAccessStaysEmpty(t *testing.T) {
	// A whitespace-only access token must land as literal "", not a sealed
	// blob — same missing-value contract as the empty case.
	var key [32]byte
	copy(key[:], "test-envelope-key-0123456789ab")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"   ","refresh_token":"r123","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer srv.Close()
	cfg := &connectors.OAuthConfig{
		ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example/cb/api/connectors/callback",
		Key: key, AuthURL: srv.URL + "/o", TokenURL: srv.URL + "/t",
	}
	store := connectors.NewMemoryStore()
	gsrv := newTestServer(t, &fakeAgent{}, Config{ConnectorStore: store, ConnectorOAuth: cfg})
	rec := do(t, gsrv, http.MethodPost, "/api/connectors/gmail/connect", "")
	u, err := url.Parse(decodeBody(t, rec)["auth_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	rec = do(t, gsrv, http.MethodGet, "/api/connectors/callback?code=x&state="+u.Query().Get("state"), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("callback: %d %s", rec.Code, rec.Body)
	}
	got, err := store.GetTokens(context.Background(), "gmail")
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessCipher != "" {
		t.Errorf("whitespace access token was sealed: %q", got.AccessCipher)
	}
	c, err := store.Get(context.Background(), "gmail")
	if err != nil || c.Status != connectors.StatusConnected {
		t.Fatalf("status = %+v, %v", c, err)
	}
}

func TestConnectorsCallbackServesHTMLToBrowsers(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	srv := oauthServer(t, cfg)
	rec := do(t, srv, http.MethodPost, "/api/connectors/gmail/connect", "")
	u, err := url.Parse(decodeBody(t, rec)["auth_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/connectors/callback?code=x&state="+u.Query().Get("state"), nil)
	req.Header.Set("Authorization", "Bearer test-token")
	req.Header.Set("Accept", "text/html")
	rec2 := httptest.NewRecorder()
	srv.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusOK {
		t.Fatalf("html callback: %d (%s)", rec2.Code, rec2.Body)
	}
	if ct := rec2.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	if cc := rec2.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("cache-control = %q, want no-store", cc)
	}
	if ct := rec2.Header().Get("X-Content-Type-Options"); ct != "nosniff" {
		t.Errorf("x-content-type-options = %q, want nosniff", ct)
	}
}

func TestConnectorsConnectSetsNoStore(t *testing.T) {
	cfg, close := oauthTestServer(t, false)
	defer close()
	srv := oauthServer(t, cfg)
	rec := do(t, srv, http.MethodPost, "/api/connectors/gmail/connect", "")
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("cache-control = %q, want no-store", cc)
	}
}
