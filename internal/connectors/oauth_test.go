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

// Redirect validation fails closed: typo'd host/scheme or a missing
// callback suffix must break startup config, not die in the browser.
func TestOAuthConfigValidatesRedirect(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("GOOGLE_CLIENT_ID", "cid")
	t.Setenv("GOOGLE_CLIENT_SECRET", "sec")
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", key)
	for _, tc := range []struct {
		name string
		url  string
		ok   bool
	}{
		{"good", "https://host:8080/p/api/connectors/callback", true},
		{"http ok", "http://localhost:8643/api/connectors/callback", true},
		{"trailing slash ok", "https://host:8080/p/api/connectors/callback/", true},
		{"wrong suffix", "https://host/p/api/connectors/gmail/callback", false},
		{"no scheme", "host/api/connectors/callback", false},
		{"ftp", "ftp://host/api/connectors/callback", false},
		{"query rejected", "https://host/p/api/connectors/callback?x=1", false},
		{"fragment rejected", "https://host/p/api/connectors/callback#y", false},
		{"missing", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOOGLE_REDIRECT_URL", tc.url)
			_, err := OAuthConfigFromEnv()
			if tc.ok && err != nil {
				t.Fatalf("valid redirect rejected: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("invalid redirect accepted")
			}
		})
	}
}

func TestOAuthConfigRejectsUserinfo(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("GOOGLE_CLIENT_ID", "cid")
	t.Setenv("GOOGLE_CLIENT_SECRET", "sec")
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", key)
	t.Setenv("GOOGLE_REDIRECT_URL", "https://user:pass@host/p/api/connectors/callback")
	if _, err := OAuthConfigFromEnv(); err == nil {
		t.Fatal("userinfo in redirect URL should fail")
	}
}

// The validated shape and the sent shape must be the same string: Google
// exact-matches redirect_uri, so a stored trailing slash against a
// no-slash registration (or vice versa) dies with redirect_uri_mismatch.
func TestOAuthConfigCanonicalisesRedirect(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("GOOGLE_CLIENT_ID", "cid")
	t.Setenv("GOOGLE_CLIENT_SECRET", "sec")
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", key)
	t.Setenv("GOOGLE_REDIRECT_URL", "https://host:8080/p/api/connectors/callback/")
	cfg, err := OAuthConfigFromEnv()
	if err != nil {
		t.Fatalf("valid redirect rejected: %v", err)
	}
	if want := "https://host:8080/p/api/connectors/callback"; cfg.RedirectURL != want {
		t.Fatalf("RedirectURL = %q, want canonical %q", cfg.RedirectURL, want)
	}
}
