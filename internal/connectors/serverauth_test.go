package connectors

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
	if _, err := GrantClient(context.Background(), NewMemoryStore(), cfg, Gmail); err == nil {
		t.Fatal("grant-less connector should fail")
	} else if !strings.Contains(err.Error(), "reconnect") {
		t.Fatalf("err = %v, want reconnect guidance", err)
	}
	if _, err := GrantClient(context.Background(), NewMemoryStore(), cfg, "slack"); err == nil {
		t.Fatal("unknown connector should fail")
	} else if se, ok := err.(*StoreError); !ok || !se.NotFound {
		t.Fatalf("unknown should be NotFound, got %v", err)
	}
	if _, err := GrantClient(context.Background(), NewMemoryStore(), nil, Gmail); err == nil {
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
	httpClient, err := GrantClient(context.Background(), store, cfg, Gmail)
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
}
