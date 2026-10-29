package connectors

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
		return &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example/cb/api/connectors/callback", AuthURL: srv.URL + "/o", TokenURL: srv.URL + "/t"}
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

// The returned token carries the checked (trimmed) values, not the raw
// provider bytes: a future caller sealing tok.RefreshToken must not seal
// padding the refresher can't use.
func TestExchangeTrimsTokens(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"  a1  ","refresh_token":"  r1  ","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer srv.Close()
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example/cb/api/connectors/callback", AuthURL: srv.URL + "/o", TokenURL: srv.URL + "/t"}
	tok, err := Exchange(context.Background(), cfg, Gmail, "code1", "verifier1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "a1" || tok.RefreshToken != "r1" {
		t.Errorf("tokens = %+v, want trimmed", tok)
	}
}

// A control-bearing refresh from the provider fails closed as a
// deterministic 400 — not as a 502 at seal time with the state already
// burned. Same no-refresh branch contract one screen up.
func TestExchangeRejectsControlToken(t *testing.T) {
	// A literal NUL would break JSON parsing itself; \u0001 is valid JSON
	// that decodes to a control character, exercising the token gate.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"a123","refresh_token":"r\u0001123","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer srv.Close()
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example/cb/api/connectors/callback", AuthURL: srv.URL + "/o", TokenURL: srv.URL + "/t"}
	if _, err := Exchange(context.Background(), cfg, Gmail, "code1", "verifier1", nil); err == nil {
		t.Fatal("control-bearing provider token should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("control token should be a deterministic 400, got %v", err)
	}
}

// Empty endpoints fail closed before any network call: a hand-built
// config missing AuthURL/TokenURL must not reach the token endpoint.
func TestExchangeRejectsEmptyEndpoints(t *testing.T) {
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example/cb/api/connectors/callback"}
	if _, err := Exchange(context.Background(), cfg, Gmail, "code", "verifier", nil); err == nil {
		t.Fatal("empty endpoints should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("empty endpoints should be a deterministic 400, got %v", err)
	}
}

// Empty client credentials fail closed before any network call, matching
// AuthURL: a hand-built cfg with blank id/secret can only 400 at Google.
func TestExchangeRejectsEmptyClient(t *testing.T) {
	cfg := &OAuthConfig{ClientID: "   ", ClientSecret: "sec", RedirectURL: "https://app.example/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	if _, err := Exchange(context.Background(), cfg, Gmail, "code", "verifier", nil); err == nil {
		t.Fatal("blank client id should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("blank client should be a deterministic 400, got %v", err)
	}
}

// Control bytes and invalid UTF-8 in code/verifier fail closed before any
// network call: overlong sequences decode as RuneError (not control), so
// the control gate alone would miss them.
func TestExchangeRejectsControlBytes(t *testing.T) {
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	if _, err := Exchange(context.Background(), cfg, Gmail, "co\rde", "verifier", nil); err == nil {
		t.Fatal("control-byte code should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("control code should be a deterministic 400, got %v", err)
	}
	if _, err := Exchange(context.Background(), cfg, Gmail, "code", "v\xc0\x80erifier", nil); err == nil {
		t.Fatal("invalid-utf8 verifier should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("invalid utf8 should be a deterministic 400, got %v", err)
	}
}

// Shape before identity in Exchange too: a bad redirect with an unknown
// id 400s on shape, not 404s on identity — matching AuthURL's order.
func TestExchangeShapeBeforeIdentity(t *testing.T) {
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://evil.example/x", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	if _, err := Exchange(context.Background(), cfg, "slack", "code", "verifier", nil); err == nil {
		t.Fatal("bad redirect should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("bad-redirect unknown-id should be a deterministic 400, got %v", err)
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

// A control-bearing redirect fails closed at validation, not in the
// browser with redirect_uri_mismatch.
func TestCanonicalRedirectRejectsControlBytes(t *testing.T) {
	if _, err := CanonicalRedirectURL("https://host/p/api/connectors/callback\x7f"); err == nil {
		t.Fatal("control-bearing redirect should fail closed")
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

func TestOAuthConfigCanonicalisesNonCanonicalRedirect(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("GOOGLE_CLIENT_ID", "cid")
	t.Setenv("GOOGLE_CLIENT_SECRET", "sec")
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", key)
	// Cleaned, not rejected: validated shape must equal sent shape.
	for _, tc := range []struct{ name, url, want string }{
		{"double slash", "https://host/p//api/connectors/callback", "https://host/p/api/connectors/callback"},
		{"dot segment", "https://host/p/./api/connectors/callback", "https://host/p/api/connectors/callback"},
		{"trailing dot", "https://host./p/api/connectors/callback", "https://host/p/api/connectors/callback"},
		{"trailing dot with port", "https://host.:8443/p/api/connectors/callback", "https://host:8443/p/api/connectors/callback"},
		{"double trailing dot", "https://host../p/api/connectors/callback", "https://host/p/api/connectors/callback"},
		{"ipv6 non-default port", "http://[::1]:8643/api/connectors/callback", "http://[::1]:8643/api/connectors/callback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOOGLE_REDIRECT_URL", tc.url)
			cfg, err := OAuthConfigFromEnv()
			if err != nil {
				t.Fatalf("canonicalisable redirect rejected: %v", err)
			}
			if cfg.RedirectURL != tc.want {
				t.Errorf("RedirectURL = %q, want canonical %q", cfg.RedirectURL, tc.want)
			}
		})
	}
}

func TestOAuthConfigRejectsNonLocalhostHTTP(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("GOOGLE_CLIENT_ID", "cid")
	t.Setenv("GOOGLE_CLIENT_SECRET", "sec")
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", key)
	t.Setenv("GOOGLE_REDIRECT_URL", "http://host/p/api/connectors/callback")
	if _, err := OAuthConfigFromEnv(); err == nil {
		t.Fatal("non-localhost http redirect should fail closed")
	}
	t.Setenv("GOOGLE_REDIRECT_URL", "http://localhost:8643/api/connectors/callback")
	if _, err := OAuthConfigFromEnv(); err != nil {
		t.Fatalf("loopback http redirect should pass: %v", err)
	}
}

// Control bytes in client credentials fail closed at config load, not
// at Google after consent.
func TestOAuthConfigRejectsControlClient(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("GOOGLE_CLIENT_ID", "cid\x7f")
	t.Setenv("GOOGLE_CLIENT_SECRET", "sec")
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", key)
	t.Setenv("GOOGLE_REDIRECT_URL", "https://host/p/api/connectors/callback")
	if _, err := OAuthConfigFromEnv(); err == nil {
		t.Fatal("control-byte client id should fail closed")
	}
	// Invalid UTF-8 fails the same way: bad bytes decode as RuneError
	// (not control), so the control gate alone would miss them.
	t.Setenv("GOOGLE_CLIENT_ID", "ci\xc0\x80d")
	if _, err := OAuthConfigFromEnv(); err == nil {
		t.Fatal("invalid-utf8 client id should fail closed")
	}
}

// A non-numeric port can never match a console registration: fail closed
// at config load, not with redirect_uri_mismatch in the browser.
func TestOAuthConfigRejectsNonNumericPort(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("GOOGLE_CLIENT_ID", "cid")
	t.Setenv("GOOGLE_CLIENT_SECRET", "sec")
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", key)
	t.Setenv("GOOGLE_REDIRECT_URL", "https://host:abc/p/api/connectors/callback")
	if _, err := OAuthConfigFromEnv(); err == nil {
		t.Fatal("non-numeric port should fail closed")
	}
}

// Length is capped before parsing: a multi-MB redirect must not pay the
// parse + Clean + String alloc before the deterministic 400.
func TestCanonicalRedirectRejectsOversized(t *testing.T) {
	if _, err := CanonicalRedirectURL("https://host/" + strings.Repeat("x", MaxStateFieldLen)); err == nil {
		t.Fatal("oversized redirect should fail closed")
	}
}

// Oversized client credentials fail closed at config load, matching the
// state/code/verifier length gates: a multi-MB id would otherwise mint a
// consent URL Google 414s.
func TestOAuthConfigRejectsLongClient(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("GOOGLE_CLIENT_ID", strings.Repeat("x", MaxStateFieldLen+1))
	t.Setenv("GOOGLE_CLIENT_SECRET", "sec")
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", key)
	t.Setenv("GOOGLE_REDIRECT_URL", "https://host/p/api/connectors/callback")
	if _, err := OAuthConfigFromEnv(); err == nil {
		t.Fatal("oversized client id should fail closed")
	}
}

// Oversized helper inputs fail closed the same way: a multi-KB client id
// would otherwise mint a consent URL Google 414s, and a multi-KB provider
// token would 502 at seal time after the state is consumed.
func TestAuthURLRejectsLongClient(t *testing.T) {
	cfg := &OAuthConfig{ClientID: strings.Repeat("x", MaxStateFieldLen+1), ClientSecret: "sec", RedirectURL: "https://app.example/cb/api/connectors/callback", AuthURL: "https://auth.example/o", TokenURL: "https://tok.example/t"}
	if got := AuthURL(cfg, "gmail", "s", "v", nil); got != "" {
		t.Errorf("oversized client id URL = %q, want empty", got)
	}
}

func TestExchangeRejectsOversizedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"access_token":"a123","refresh_token":%q,"expires_in":3600,"token_type":"Bearer"}`, strings.Repeat("y", MaxPlaintextLen+1))
	}))
	defer srv.Close()
	cfg := &OAuthConfig{ClientID: "cid", ClientSecret: "sec", RedirectURL: "https://app.example/cb/api/connectors/callback", AuthURL: srv.URL + "/o", TokenURL: srv.URL + "/t"}
	if _, err := Exchange(context.Background(), cfg, Gmail, "code1", "verifier1", nil); err == nil {
		t.Fatal("oversized provider token should fail closed")
	} else if se, ok := err.(*StoreError); !ok || se.NotFound || se.Upstream {
		t.Fatalf("oversized token should be a deterministic 400, got %v", err)
	}
}

// Hostnames no console registration can hold fail closed: underscores,
// leading/trailing hyphens, empty labels, non-ASCII (punycode at the
// console, never Unicode).
func TestOAuthConfigRejectsBadHostname(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("GOOGLE_CLIENT_ID", "cid")
	t.Setenv("GOOGLE_CLIENT_SECRET", "sec")
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", key)
	for _, tc := range []struct{ name, url string }{
		{"underscore", "https://ho_st/p/api/connectors/callback"},
		{"leading hyphen", "https://-host/p/api/connectors/callback"},
		{"unicode", "https://m\xc3\xbcnchen/p/api/connectors/callback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GOOGLE_REDIRECT_URL", tc.url)
			if _, err := OAuthConfigFromEnv(); err == nil {
				t.Fatalf("bad hostname %q should fail closed", tc.url)
			}
		})
	}
}

// Percent-encoded controls fail closed too: Parse decodes %00 before the
// shape checks, so gating only the raw string would miss them.
func TestOAuthConfigRejectsEncodedControls(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("GOOGLE_CLIENT_ID", "cid")
	t.Setenv("GOOGLE_CLIENT_SECRET", "sec")
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", key)
	t.Setenv("GOOGLE_REDIRECT_URL", "https://host/p/api/connectors/callback%00")
	if _, err := OAuthConfigFromEnv(); err == nil {
		t.Fatal("encoded control should fail closed")
	}
	// Invalid UTF-8 fails the same way: bad bytes decode as RuneError
	// (not control), so the control gate alone would miss them.
	t.Setenv("GOOGLE_REDIRECT_URL", "https://host/p/api/connectors/callback\xff")
	if _, err := OAuthConfigFromEnv(); err == nil {
		t.Fatal("invalid-utf8 redirect should fail closed")
	}
	// Encoded controls in host fail the same way: Parse leaves Host
	// percent-encoded, so the check unescapes before testing.
	t.Setenv("GOOGLE_REDIRECT_URL", "https://h%00st/p/api/connectors/callback")
	if _, err := OAuthConfigFromEnv(); err == nil {
		t.Fatal("encoded control in host should fail closed")
	}
}

// IPv6 literals keep their brackets through canonicalisation: stripping
// the default port must not emit an unbracketed (hence invalid) host,
// or the loopback-http gate that follows misreads it.
func TestOAuthConfigCanonicalisesIPv6Loopback(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("GOOGLE_CLIENT_ID", "cid")
	t.Setenv("GOOGLE_CLIENT_SECRET", "sec")
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", key)
	t.Setenv("GOOGLE_REDIRECT_URL", "http://[::1]:80/api/connectors/callback")
	cfg, err := OAuthConfigFromEnv()
	if err != nil {
		t.Fatalf("IPv6 loopback http should pass: %v", err)
	}
	if want := "http://[::1]/api/connectors/callback"; cfg.RedirectURL != want {
		t.Errorf("RedirectURL = %q, want %q", cfg.RedirectURL, want)
	}
}

// Explicit default ports canonicalise away: the console registration is
// port-less, and Google exact-matches redirect_uri.
func TestOAuthConfigStripsDefaultPort(t *testing.T) {
	const key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("GOOGLE_CLIENT_ID", "cid")
	t.Setenv("GOOGLE_CLIENT_SECRET", "sec")
	t.Setenv("CONNECTORS_ENCRYPTION_KEY", key)
	t.Setenv("GOOGLE_REDIRECT_URL", "https://host:443/p/api/connectors/callback")
	cfg, err := OAuthConfigFromEnv()
	if err != nil {
		t.Fatalf("explicit default port should canonicalise, not fail: %v", err)
	}
	if cfg.RedirectURL != "https://host/p/api/connectors/callback" {
		t.Errorf("RedirectURL = %q, want canonical no-port form", cfg.RedirectURL)
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
