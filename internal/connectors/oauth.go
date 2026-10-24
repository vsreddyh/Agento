package connectors

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/oauth2"
)

// OAuth exchange for the first connectors (#316, second layer).
//
// v1 foundation answered POST .../connect with 501. This layer wires the
// real Google flow: the app opens the auth URL in a browser, Google
// redirects to the server callback with a code, the server exchanges it and
// stores the ENCRYPTED tokens (see crypto.go), and the connector flips to
// connected. The app polls GET /api/connectors; it never sees a code, a
// verifier, or a token anywhere in the flow.

const (
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"

	// StateTTL bounds one connect attempt: the user has ten minutes from
	// tapping connect to finishing the Google consent screen.
	StateTTL = 10 * time.Minute

	// StatesCollection holds pending connect attempts. Short-lived by TTL;
	// consumed (deleted) on first use, so a code can never be exchanged twice.
	StatesCollection = "oauth_states"
)

// GoogleScopes maps a connector to the Google OAuth scopes its auth URL
// requests. Least privilege per connector, and deliberately NOT the
// full-access variants — but exactly what the official MCP servers
// require, since the bridge (cmd/mcp-bridge) calls THEM, not the REST
// API directly:
//
//   - gmail: readonly + compose. compose creates/updates/deletes DRAFTS
//     only — it cannot send. The exact scope the draft-only rule needs:
//     the token is incapable of sending even if the relay check vanished.
//     Matches the official Gmail MCP server's own scope pair.
//   - calendar: the three scopes the official Calendar MCP server
//     requires (calendar list + freebusy + events read). The grant carries
//     no write bit even though the official server HAS write tools —
//     those tools are filtered at the bridge and refused at the relay, so
//     the token cannot write even if both checks vanished.
//
// Sync obligation (same class as IsDraftOnlySend): if Google changes what
// the official servers require, BOTH this table and the tool policy move
// together, and the ADR records why.
func GoogleScopes(id string) []string {
	switch id {
	case Gmail:
		return []string{
			"https://www.googleapis.com/auth/gmail.readonly",
			"https://www.googleapis.com/auth/gmail.compose",
		}
	case Calendar:
		return []string{
			"https://www.googleapis.com/auth/calendar.calendarlist.readonly",
			"https://www.googleapis.com/auth/calendar.events.freebusy",
			"https://www.googleapis.com/auth/calendar.events.readonly",
		}
	default:
		return nil
	}
}

// OAuthConfig is the server-side OAuth client. Nil *OAuthConfig (the
// gateway's ConnectorOAuth field) means unconfigured: connect 501s exactly
// like the v1 foundation, and no token can be stored.
type OAuthConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string
	Key          [32]byte
	// Endpoint override for tests (httptest token server). Production always
	// uses Google's endpoints below.
	AuthURL  string
	TokenURL string
}

// OAuthConfigFromEnv builds the OAuth client from the environment. ANY
// missing piece is an error (nil config, connect stays 501): a half-open
// flow — auth URL without exchange, exchange without storage key — would
// strand the user mid-consent with no honest error to show.
func OAuthConfigFromEnv() (*OAuthConfig, error) {
	clientID := strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_SECRET"))
	redirectURL := strings.TrimSpace(os.Getenv("GOOGLE_REDIRECT_URL"))
	if clientID == "" || clientSecret == "" || redirectURL == "" {
		return nil, errors.New("connectors: GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET/GOOGLE_REDIRECT_URL must all be set for OAuth")
	}
	// Fail closed on a misregistered redirect: Google matches the callback
	// URL exactly, so a typo'd host/port/scheme or a missing
	// /api/connectors/callback suffix dies in the *browser* with
	// redirect_uri_mismatch and nothing in the server log pointing at it.
	// Validate the shape here; the operator compares the logged value
	// against the Cloud console registration. Trailing slashes are trimmed
	// first: splitProfile Cleans the path server-side, so .../callback/
	// would serve fine — rejecting it here would 501 a working config
	// over punctuation.
	parsed, err := url.Parse(redirectURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, errors.New("connectors: GOOGLE_REDIRECT_URL must be an absolute http(s) URL")
	}
	if parsed.User != nil {
		// Userinfo would be logged verbatim in the startup line below and
		// never match Google's exact-redirect comparison — fail closed
		// rather than storing credentials in the service log.
		return nil, errors.New("connectors: GOOGLE_REDIRECT_URL must not contain userinfo (user:pass@)")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		// Google compares the full redirect_uri exactly: a registered query
		// or fragment would never match what the gateway sends (bare path),
		// so fail here with a pointer at the registration, not in the
		// browser with redirect_uri_mismatch.
		return nil, errors.New("connectors: GOOGLE_REDIRECT_URL must have no query or fragment; register the bare callback URL")
	}
	if !strings.HasSuffix(strings.TrimRight(parsed.Path, "/"), "/api/connectors/callback") {
		return nil, errors.New("connectors: GOOGLE_REDIRECT_URL path must end in /api/connectors/callback (the single registered endpoint)")
	}
	// Canonicalise: store the validated shape with no trailing slash, so
	// the exact string the gateway sends as redirect_uri (and the operator
	// compares in the startup log) is the string validated above. Query
	// and fragment are already rejected, so trimming the string trims the
	// path. Without this a .../callback/ config passes validation but
	// Google exact-matches it against a no-slash registration (or vice
	// versa) and dies with redirect_uri_mismatch in the browser.
	redirectURL = strings.TrimRight(redirectURL, "/")
	key, err := KeyFromEnv()
	if err != nil {
		return nil, err
	}
	return &OAuthConfig{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  redirectURL,
		Key:          key,
		AuthURL:      googleAuthURL,
		TokenURL:     googleTokenURL,
	}, nil
}

func (c *OAuthConfig) endpoint() oauth2.Endpoint {
	return oauth2.Endpoint{AuthURL: c.AuthURL, TokenURL: c.TokenURL}
}

func (c *OAuthConfig) oauth2(scopes []string) *oauth2.Config {
	return &oauth2.Config{
		ClientID:     c.ClientID,
		ClientSecret: c.ClientSecret,
		RedirectURL:  c.RedirectURL,
		Scopes:       scopes,
		Endpoint:     c.endpoint(),
	}
}

// NewState mints one CSRF state + PKCE verifier pair. State is 128 bits of
// crypto rand, base64url; the verifier is 64 bytes (S256 flow, so the
// exchange needs no client-secret-in-browser — the secret never leaves the
// server, and the app never sees the verifier either).
func NewState() (state, verifier string, err error) {
	rb := make([]byte, 16)
	if _, err := rand.Read(rb); err != nil {
		return "", "", err
	}
	vb := make([]byte, 64)
	if _, err := rand.Read(vb); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(vb)
	return base64.RawURLEncoding.EncodeToString(rb), verifier, nil
}

// MaxStateFieldLen bounds state/verifier/code inputs end to end. States are
// 128-bit base64url (~22 chars); PKCE verifiers ~86; Google codes a few
// hundred chars. Anything past 2048 is not a real credential — it is a
// garbage or hostile input that must 400 before it becomes a DB filter
// or an outbound token request. Enforced at the gateway (fast path) AND the
// store (ConsumeState/CreateState): the store is exported for direct
// callers (future MCP servers), so the gateway cap alone would leave the
// enforcement to one layer.
const MaxStateFieldLen = 2048

// AuthURL builds the Google consent URL for one connector attempt. Offline
// access (a refresh token) + consent prompt every time: without
// prompt=consent Google returns a refresh token only on the FIRST grant,
// and a reconnect that yields no refresh token would silently downgrade the
// row to access-token-only with no renewal path.
//
// Fail-closed builder: nil cfg, unknown id, or empty/overlong inputs
// return "" — cheaper than a panic and cheaper than a consent URL that can
// only die later at ConsumeState. cfg itself must come from
// OAuthConfigFromEnv, which is the single place that validates the
// redirect shape (absolute http(s), no userinfo/query/fragment, correct
// path suffix); a hand-built cfg with a non-matching redirect mints a URL
// Google will reject at consent time. The gateway route, CreateState, and
// Exchange all re-check inputs (defense in depth for direct callers), but
// this helper needs no trusted caller.
func AuthURL(cfg *OAuthConfig, id, state, verifier string) string {
	if cfg == nil || strings.TrimSpace(cfg.ClientID) == "" || strings.TrimSpace(cfg.RedirectURL) == "" {
		return ""
	}
	// Folded and trimmed like every store verb — unknown or empty inputs
	// fail closed, and overlong inputs fail closed too (same
	// MaxStateFieldLen bound the stores enforce): a multi-KB state or
	// verifier would mint a consent URL Google 414s, with only log spam to
	// show for it. The gateway mints fixed-size values via NewState, so
	// this guards direct callers.
	id = NormalizedID(id)
	state = strings.TrimSpace(state)
	verifier = strings.TrimSpace(verifier)
	if !Known(id) || state == "" || verifier == "" {
		return ""
	}
	if len(state) > MaxStateFieldLen || len(verifier) > MaxStateFieldLen {
		return ""
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return cfg.oauth2(GoogleScopes(id)).AuthCodeURL(state,
		oauth2.SetAuthURLParam("code_challenge", challenge),
		oauth2.SetAuthURLParam("code_challenge_method", "S256"),
		oauth2.SetAuthURLParam("access_type", "offline"),
		oauth2.SetAuthURLParam("prompt", "consent"),
	)
}

// Exchange swaps a code (+ PKCE verifier) for tokens against the configured
// token endpoint. cfg may be nil — it fails closed with a 400
// ("not configured"), never a panic. HTTP errors surface as a generic 502
// ("grant exchange failed") with the provider's error_description only in
// the log — oauth2 errors echo the token endpoint verbatim, and the client
// needs a retry signal, not provider internals. logf may be nil (tests).
func Exchange(ctx context.Context, cfg *OAuthConfig, id, code, verifier string, logf func(string, ...any)) (*oauth2.Token, error) {
	if cfg == nil {
		return nil, NewStoreError("oauth client is not configured", false)
	}
	// Bounded before the token endpoint, on trimmed values: codes/verifiers
	// are short by construction, and a multi-MB value would become an
	// oversized outbound request. Trimmed first so padding can't push a
	// real credential over the cap (or slip whitespace through to Google).
	// Deterministic input reject (400), not a flaky upstream — same shape
	// as the missing-refresh branch below.
	code = strings.TrimSpace(code)
	verifier = strings.TrimSpace(verifier)
	if code == "" || verifier == "" {
		// Deterministic caller error, not an upstream failure: nothing was
		// sent anywhere. Same 400 shape as the length and no-refresh
		// branches.
		return nil, NewStoreError("oauth code or verifier is empty", false)
	}
	if len(code) > MaxStateFieldLen || len(verifier) > MaxStateFieldLen {
		return nil, NewStoreError("oauth code or verifier too long", false)
	}
	// Folded like AuthURL above and every store verb: same logical id,
	// same answer, wherever the caller normalised.
	id = NormalizedID(id)
	if !Known(id) {
		return nil, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	tok, err := cfg.oauth2(GoogleScopes(id)).Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", verifier))
	if err != nil {
		if logf != nil {
			logf("connectors: grant exchange failed for %s: %v", id, err)
		}
		return nil, NewStoreError("grant exchange failed", true)
	}
	if strings.TrimSpace(tok.RefreshToken) == "" {
		// No refresh token means no renewal path: storing this row as
		// connected would strand it at the first access-token expiry.
		// Fail the connect loudly instead of a slow-burn disconnect.
		// Deterministic provider answer (not a flaky upstream), so 400 —
		// retrying the same consumed code cannot change the outcome, and
		// triage must read this as misconfigured consent, not an outage.
		// Trimmed like SetTokens: same definition of "missing" on both
		// sides of the store boundary.
		return nil, NewStoreError("grant exchange returned no refresh token; reconnect and approve offline access", false)
	}
	return tok, nil
}

// TokenRow is the ciphertext half of a connector grant. The store holds
// these strings opaquely — it never sees plaintext and needs no key.
// Encryption lives with the OAuth layer (EncryptToken/DecryptToken) and the
// future MCP servers, which share the key from the environment.
type TokenRow struct {
	// RefreshCipher is base64(nonce||ciphertext) of the OAuth refresh token.
	// Never empty on a stored row: Exchange refuses token responses without
	// one, so a stored grant always renews.
	RefreshCipher string `bson:"refreshToken" json:"-"`
	// AccessCipher is base64(nonce||ciphertext) of the short-lived access token.
	// May be empty ("" literal, never a sealed empty — EncryptToken refuses
	// those): the refresher treats empty as "renew now". The refresh cipher
	// above must never be empty — same rule the store enforces — or the row
	// strands at the first access-token expiry.
	AccessCipher string `bson:"accessToken" json:"-"`
	// AccessExpiryUnixMilli is when the access token stops working; the
	// refresher renews before this, not after it fails. Zero means unknown
	// (provider omitted expires_in) — treat as already expired (renew
	// immediately), never as "never expires". Unknown is fail-closed.
	AccessExpiryUnixMilli int64 `bson:"accessExpiry" json:"-"`
}

// OAuthState is one pending connect attempt: connector + PKCE verifier,
// consumed (deleted) on first callback so a code can never be exchanged twice.
type OAuthState struct {
	State     string    `bson:"_id" json:"-"`
	Connector string    `bson:"connector" json:"-"`
	Verifier  string    `bson:"verifier" json:"-"`
	ExpiresAt time.Time `bson:"expiresAt" json:"-"`
}
