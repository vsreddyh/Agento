package connectors

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

const testHexKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testKey(t *testing.T) [32]byte {
	t.Helper()
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", testHexKey)
	key, err := KeyFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// No grant is a reconnect answer, never a secrets prompt or an oracle.
func TestGrantClientNeedsGrant(t *testing.T) {
	key := testKey(t)
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", Key: key, AuthURL: "https://o.example", TokenURL: "https://t.example"}
	if _, err := GrantClient(context.Background(), NewMemoryStore(), cfg, Gmail, nil); err == nil {
		t.Fatal("grant-less connector should fail")
	} else if !strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("err = %v, want reconnect guidance", err)
	}
	if _, err := GrantClient(context.Background(), NewMemoryStore(), cfg, "slack", nil); err == nil {
		t.Fatal("unknown connector should fail")
	} else if se, ok := err.(*StoreError); !ok || !se.NotFound {
		t.Fatalf("unknown should be NotFound, got %v", err)
	}
	if _, err := GrantClient(context.Background(), NewMemoryStore(), nil, Gmail, nil); err == nil {
		t.Fatal("nil config should fail closed")
	}
}

func TestServerOAuthConfigNeedsClientPair(t *testing.T) {
	testKey(t)
	t.Setenv("GOOGLE_CLIENT_ID", "")
	t.Setenv("GOOGLE_CLIENT_SECRET", "")
	if _, err := ServerOAuthConfig(); err == nil {
		t.Fatal("missing client pair should fail")
	}
	t.Setenv("GOOGLE_CLIENT_ID", "cid")
	t.Setenv("GOOGLE_CLIENT_SECRET", "sec")
	if _, err := ServerOAuthConfig(); err != nil {
		t.Fatalf("full env should build: %v", err)
	}
}

// Expired access refreshes against Google and the new pair lands sealed in
// the store — the next process starts current.
func TestGrantClientRefreshesAndPersists(t *testing.T) {
	key := testKey(t)
	var authed string
	var grant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/t") {
			if err := r.ParseForm(); err != nil {
				t.Error(err)
			}
			grant = r.Form.Get("grant_type")
			fmt.Fprint(w, `{"access_token":"a456","refresh_token":"r789","expires_in":3600,"token_type":"Bearer"}`)
			return
		}
		authed = r.Header.Get("Authorization")
		fmt.Fprint(w, `{}`)
	}))
	defer srv.Close()

	// Sealed refresh only, no access: forces a refresh on first use.
	sealed, err := EncryptToken(key, "r123")
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	if err := store.SetTokens(context.Background(), Gmail, TokenRow{RefreshCipher: sealed}); err != nil {
		t.Fatal(err)
	}
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", Key: key, AuthURL: srv.URL + "/o", TokenURL: srv.URL + "/t"}
	httpClient, err := GrantClient(context.Background(), store, cfg, Gmail, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := httpClient.Get(srv.URL + "/api")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if grant != "refresh_token" {
		t.Fatalf("grant = %q, want refresh_token", grant)
	}
	if authed != "Bearer a456" {
		t.Errorf("authorization = %q, want refreshed bearer", authed)
	}
	row, err := store.GetTokens(context.Background(), Gmail)
	if err != nil {
		t.Fatal(err)
	}
	access, err := DecryptToken(key, row.AccessCipher)
	if err != nil || access != "a456" {
		t.Errorf("stored access = %q, %v — want persisted refresh", access, err)
	}
	refresh, err := DecryptToken(key, row.RefreshCipher)
	if err != nil || refresh != "r789" {
		t.Errorf("stored refresh = %q, %v — want rotated value", refresh, err)
	}
	if row.AccessExpiryUnixMilli <= 0 {
		t.Errorf("stored expiry = %d, want the refreshed expires_in", row.AccessExpiryUnixMilli)
	}
}

// A dead refresh token (invalid_grant) is a reconnect answer, not provider
// gibberish: no retry will revive the grant.
func TestGrantClientDeadRefreshIsReconnect(t *testing.T) {
	key := testKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`)
	}))
	defer srv.Close()
	sealed, err := EncryptToken(key, "dead-refresh")
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	if err := store.SetTokens(context.Background(), Gmail, TokenRow{RefreshCipher: sealed}); err != nil {
		t.Fatal(err)
	}
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", Key: key, AuthURL: srv.URL + "/o", TokenURL: srv.URL + "/t"}
	var logged []string
	logf := func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}
	src, err := GrantTokenSource(context.Background(), store, cfg, Gmail, logf)
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.Token()
	if err == nil || !strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("dead grant = %v, want reconnect guidance", err)
	}
	if se, ok := err.(*StoreError); !ok || se.Upstream {
		t.Fatalf("dead grant = %+v, want non-upstream StoreError", err)
	}
	// The agent-safe message hides the cause; the operator log must not.
	found := false
	for _, line := range logged {
		if strings.Contains(line, "gmail") && strings.Contains(line, "invalid_grant") && !strings.Contains(line, "dead-refresh") {
			found = true
		}
	}
	if !found {
		t.Errorf("operator log = %v, want id + invalid_grant without the secret", logged)
	}
}

// Zero stored expiry reads as already-expired (fail-closed), even with a
// live access token: oauth2 treats zero Expiry as never-expired, so
// without backdating the grant would serve forever and never refresh.
func TestGrantClientZeroExpiryForcesRefresh(t *testing.T) {
	key := testKey(t)
	var grants int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("grant_type") == "refresh_token" {
			grants++
		}
		fmt.Fprint(w, `{"access_token":"a000","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer srv.Close()
	refreshSealed, err := EncryptToken(key, "r123")
	if err != nil {
		t.Fatal(err)
	}
	accessSealed, err := EncryptToken(key, "stale-access")
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	// Sealed access, no expiry recorded: must refresh on first use.
	if err := store.SetTokens(context.Background(), Gmail, TokenRow{RefreshCipher: refreshSealed, AccessCipher: accessSealed}); err != nil {
		t.Fatal(err)
	}
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", Key: key, AuthURL: srv.URL + "/o", TokenURL: srv.URL + "/t"}
	src, err := GrantTokenSource(context.Background(), store, cfg, Gmail, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Token(); err != nil {
		t.Fatal(err)
	}
	if grants != 1 {
		t.Fatalf("zero-expiry access served without refresh (%d refresh calls)", grants)
	}
}

// Same pair with an extended expiry still persists: without the expiry in
// the comparison the stored AccessExpiryUnixMilli goes stale and the next
// process refreshes one cycle early.
func TestPersistingSourceUpdatesStaleExpiry(t *testing.T) {
	key := testKey(t)
	sealed, err := EncryptToken(key, "r123")
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	ctx := context.Background()
	if err := store.SetTokens(ctx, Gmail, TokenRow{RefreshCipher: sealed}); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Hour).Round(0)
	src := &persistingSource{
		inner:   staticSource{tok: &oauth2.Token{RefreshToken: "r123", AccessToken: "a", Expiry: later}},
		store:   store,
		key:     key,
		id:      Gmail,
		refresh: "r123",
		access:  "a",
	}
	if _, err := src.Token(); err != nil {
		t.Fatal(err)
	}
	row, err := store.GetTokens(ctx, Gmail)
	if err != nil {
		t.Fatal(err)
	}
	if row.AccessExpiryUnixMilli != later.UnixMilli() {
		t.Errorf("stored expiry = %d, want %d (extended)", row.AccessExpiryUnixMilli, later.UnixMilli())
	}
}

type staticSource struct{ tok *oauth2.Token }

func (s staticSource) Token() (*oauth2.Token, error) { return s.tok, nil }

// Access-only rotation keeps the stored refresh: Google normally omits
// refresh_token on re-issue, and the inner source surfaces that omission
// verbatim. Sealing "" would fail (or worse, overwrite) — the old value
// persists with the new access side.
func TestGrantClientKeepsRefreshOnAccessOnlyRotation(t *testing.T) {
	key := testKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"a999","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer srv.Close()
	sealed, err := EncryptToken(key, "r123")
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryStore()
	if err := store.SetTokens(context.Background(), Gmail, TokenRow{RefreshCipher: sealed}); err != nil {
		t.Fatal(err)
	}
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", Key: key, AuthURL: srv.URL + "/o", TokenURL: srv.URL + "/t"}
	src, err := GrantTokenSource(context.Background(), store, cfg, Gmail, nil)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := src.Token()
	if err != nil {
		t.Fatalf("access-only refresh should succeed: %v", err)
	}
	if tok.AccessToken != "a999" || tok.RefreshToken != "r123" {
		t.Fatalf("token = %+v, want new access with kept refresh", tok)
	}
	row, err := store.GetTokens(context.Background(), Gmail)
	if err != nil {
		t.Fatal(err)
	}
	refresh, err := DecryptToken(key, row.RefreshCipher)
	if err != nil || refresh != "r123" {
		t.Errorf("stored refresh = %q, %v — must survive access-only rotation", refresh, err)
	}
}

// A down store is upstream, not "reconnect": conflating them sends triage
// after the wrong layer.
func TestGrantClientStoreOutageIsUpstream(t *testing.T) {
	key := testKey(t)
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", Key: key}
	_, err := GrantClient(context.Background(), errStore{}, cfg, Gmail, nil)
	if err == nil {
		t.Fatal("store outage should fail")
	}
	se, ok := err.(*StoreError)
	if !ok || !se.Upstream || se.NotFound {
		t.Fatalf("outage = %+v, want upstream non-NotFound", err)
	}
	if strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("outage must not read as reconnect: %v", err)
	}
}

// errStore fails every read with a transport error (no NotFound shape).
type errStore struct{ *MemoryStore }

func (errStore) GetTokens(context.Context, string) (TokenRow, error) {
	return TokenRow{}, NewStoreError("connector store unavailable", true)
}
