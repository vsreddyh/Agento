package connectors

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Exchange against a stub token endpoint: success stores both tokens, and a
// response without a refresh token fails instead of stranding the row.
func TestExchangeAgainstStub(t *testing.T) {
	var grant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		grant = r.Form.Get("grant_type")
		if r.Form.Get("code_verifier") == "" {
			t.Error("missing PKCE verifier")
		}
		w.Header().Set("Content-Type", "application/json")
		body := `"access_token": "a123", "expires_in": 3600, "token_type": "Bearer"`
		// Refresh keyed off the code, not the query string: this is a POST
		// form endpoint, so RawQuery never carries it.
		if r.Form.Get("code") != "no-refresh" {
			body += `, "refresh_token": "r123"`
		}
		fmt.Fprintf(w, "{%s}", body)
	}))
	defer srv.Close()

	mkcfg := func() *OAuthConfig {
		return &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example/cb", AuthURL: srv.URL + "/o", TokenURL: srv.URL + "/t"}
	}
	tok, err := Exchange(context.Background(), mkcfg(), Gmail, "code1", "verifier1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "a123" || tok.RefreshToken != "r123" {
		t.Fatalf("tokens = %+v", tok)
	}
	if grant != "authorization_code" {
		t.Errorf("grant = %q", grant)
	}
	if tok.Expiry.IsZero() || time.Until(tok.Expiry) > time.Hour {
		t.Errorf("expiry = %v", tok.Expiry)
	}
	if _, err := Exchange(context.Background(), mkcfg(), Gmail, "no-refresh", "v", nil); err == nil {
		t.Fatal("missing refresh token should fail")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		// Deterministic provider answer, not a flaky upstream: 400 so triage
		// reads misconfigured consent, not an outage.
		t.Fatalf("no-refresh should be a non-upstream error, got %v", err)
	}
	if _, err := Exchange(context.Background(), mkcfg(), "slack", "code1", "v", nil); err == nil {
		t.Fatal("unknown connector should fail")
	}
}
