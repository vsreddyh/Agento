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

// These tests pin the #316 v1 foundation contract: three disconnected rows,
// per-connector disconnect, connect 501s until OAuth lands, secrets never
// appear, and expired rows carry the reconnect hint.

func connectorsServer(t *testing.T) *Server {
	t.Helper()
	return newTestServer(t, &fakeAgent{}, Config{ConnectorStore: connectors.NewMemoryStore()})
}

func TestConnectorsListThreeDisconnected(t *testing.T) {
	srv := connectorsServer(t)
	got := decodeBody(t, do(t, srv, http.MethodGet, "/api/connectors", ""))
	rows, ok := got["connectors"].([]any)
	if !ok || len(rows) != 3 {
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
	for _, id := range []string{"gmail", "calendar", "drive"} {
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
	if !ok || len(rows) != 3 {
		t.Fatalf("trailing-slash list = %v, want 3 rows", got)
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
		ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example/cb",
		Key: key, AuthURL: srv.URL + "/o", TokenURL: srv.URL + "/t",
	}
	return cfg, srv.Close
}

func oauthServer(t *testing.T, cfg *connectors.OAuthConfig) *Server {
	t.Helper()
	return newTestServer(t, &fakeAgent{}, Config{ConnectorStore: connectors.NewMemoryStore(), ConnectorOAuth: cfg})
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
	// serves all three. A leftover per-id path 404s as unknown.
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
