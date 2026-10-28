package connectors

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"agento/internal/approvals"

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

	// MaxAccessLifetime bounds a stored access-token expiry: provider
	// expires_in is ~1h, so anything past a day is skew, a stubby test
	// token, or a hand-edit — all of which must renew soon, not never.
	// See TokenRow for the stored-side contract; enforced at the gateway
	// write site and in both SetTokens.
	MaxAccessLifetime = 24 * time.Hour

	// StatesCollection holds pending connect attempts. Short-lived by TTL;
	// consumed (deleted) on first use, so a code can never be exchanged twice.
	StatesCollection = "oauth_states"
)

// GoogleScopes maps a connector to the Google OAuth scopes its auth URL
// requests. Least privilege per connector, and deliberately NOT the
// full-access variants:
//
//   - gmail: readonly + compose. compose creates/updates/deletes DRAFTS
//     only — it cannot send. The exact scope the draft-only rule needs:
//     the token is incapable of sending even if the relay check vanished.
//   - calendar: readonly. Draft events are agent-side objects (created
//     locally, sent nowhere) until the approval-gate flow covers writes,
//     so the grant carries no write bit at all.
//
// Follow-up sync obligation (same class as IsDraftOnlySend): if a wired MCP
// server needs a broader scope, BOTH this table and the tool policy move
// together, and the ADR records why. Folds like every store verb: same
// logical id, same answer.
func GoogleScopes(id string) []string {
	id = NormalizedID(id)
	switch id {
	case Gmail:
		return []string{
			"https://www.googleapis.com/auth/gmail.readonly",
			"https://www.googleapis.com/auth/gmail.compose",
		}
	case Calendar:
		return []string{
			"https://www.googleapis.com/auth/calendar.readonly",
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

// CanonicalRedirectURL validates and canonicalises in one step so every
// caller sends the exact registered shape: absolute http(s), https unless
// loopback, no userinfo/query/fragment, path ending in the single
// registered callback endpoint. path.Clean collapses double slashes and
// dot segments before the suffix check, so validated-shape == sent-shape
// by construction — Google exact-matches redirect_uri, and a mismatch
// dies in the browser with nothing in the server log pointing at it.
func CanonicalRedirectURL(raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", errors.New("connectors: redirect URL must be an absolute http(s) URL")
	}
	// Hostnames are case-insensitive: fold before validation so a
	// mixed-case registration compares equal to what Google
	// exact-matches against (ports are unaffected by lowercasing).
	// A trailing-dot FQDN spelling is folded too: the console
	// registration is canonically dot-less, and the dot would
	// otherwise pass validation yet mismatch Google exactly.
	// TrimRight, not TrimSuffix: "host.." must fold fully, not to "host.".
	// (Fold before the empty check: a bare "." host must fail as empty,
	// not validate as dot-less nothing.)
	parsed.Host = strings.TrimRight(strings.ToLower(parsed.Host), ".")
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return "", errors.New("connectors: redirect URL must be an absolute http(s) URL")
	}
	// Strip default ports: the console registration is canonically
	// port-less, while an operator-supplied URL may name :443/:80
	// explicitly — and Google exact-matches redirect_uri. The hostname
	// is re-folded after the split too: "host.:8443" must canonicalise
	// to "host:8443", not pass validation dotted. Brackets go back on
	// IPv6 literals, or the stripped host stops parsing as a URL.
	if h, p, err := net.SplitHostPort(parsed.Host); err == nil {
		h = strings.TrimRight(h, ".")
		if strings.Contains(h, ":") {
			h = "[" + h + "]"
		}
		if p != "" {
			// Ports must be numeric: SplitHostPort doesn't check, and no
			// console registration can hold :abc — a non-numeric port
			// passes validation yet mismatches Google exactly.
			if n, convErr := strconv.Atoi(p); convErr != nil || n < 1 || n > 65535 {
				return "", errors.New("connectors: redirect URL port must be numeric 1-65535")
			}
		}
		if (parsed.Scheme == "https" && p == "443") || (parsed.Scheme == "http" && p == "80") {
			parsed.Host = h
		} else {
			parsed.Host = net.JoinHostPort(h, p)
		}
	}
	if parsed.Scheme == "http" && parsed.Hostname() != "localhost" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "::1" {
		// Google only permits http for loopback: a non-localhost http
		// registration can never complete, and the failure surfaces as
		// redirect_uri_mismatch in the browser, not as a config error —
		// so fail closed here with a pointer at the scheme.
		return "", errors.New("connectors: redirect URL must use https (http loopback-only)")
	}
	if parsed.User != nil {
		// Userinfo would be logged verbatim in the startup line and never
		// match Google's exact-redirect comparison — fail closed rather
		// than storing credentials in the service log.
		return "", errors.New("connectors: redirect URL must not contain userinfo (user:pass@)")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		// Google compares the full redirect_uri exactly: a registered
		// query or fragment would never match what the gateway sends
		// (bare path), so fail here with a pointer at the registration,
		// not in the browser with redirect_uri_mismatch.
		return "", errors.New("connectors: redirect URL must have no query or fragment; register the bare callback URL")
	}
	// Inner controls fail closed like every sibling input (state/code/
	// verifier at every layer): url.Parse/String round-trips them, so a
	// control-bearing path would pass validation yet mismatch Google.
	if hasControl(raw) {
		return "", errors.New("connectors: redirect URL must not contain control characters")
	}
	parsed.Path = strings.TrimRight(path.Clean(parsed.Path), "/")
	if !strings.HasSuffix(parsed.Path, "/api/connectors/callback") {
		return "", errors.New("connectors: redirect URL path must end in /api/connectors/callback (the single registered endpoint)")
	}
	return parsed.String(), nil
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
	parsed, err := CanonicalRedirectURL(redirectURL)
	if err != nil {
		return nil, err
	}
	redirectURL = parsed
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
// store (ConsumeState/CreateState) AND Exchange: every layer is exported
// for direct callers (future MCP servers), so the gateway cap alone would
// leave the enforcement to one layer. Sealed tokens reuse the same bound
// (a stored grant must decrypt to use it, so a multi-MB cipher can never
// become a servable grant) — raise independently if Google ever lengthens
// tokens past what this cap allows.
const MaxStateFieldLen = 2048

// AuthURL builds the Google consent URL for one connector attempt. Offline
// access (a refresh token) + consent prompt every time: without
// prompt=consent Google returns a refresh token only on the FIRST grant,
// and a reconnect that yields no refresh token would silently downgrade the
// row to access-token-only with no renewal path.
//
// Fail-closed builder: nil cfg, unknown id, empty/overlong inputs,
// empty endpoints, or a misshapen redirect return "" — cheaper than a
// panic and cheaper than a consent URL that can only die later at
// ConsumeState. cfg normally comes from OAuthConfigFromEnv (which also
// validates the redirect shape and sets both Google endpoints), but a
// hand-built cfg with a non-matching redirect is rejected here too via
// the same CanonicalRedirectURL check — shape is helper-enforced, not just
// caller-promised. logf (may be nil) names which guard fired for
// server-side triage; the app never sees it. The gateway route, CreateState, and Exchange all
// re-check inputs (defense in depth for direct callers), but this
// helper needs no trusted caller.
func AuthURL(cfg *OAuthConfig, id, state, verifier string, logf func(string, ...any)) string {
	// Folded first, like Exchange and every store verb: the unconfigured
	// log below must observe the same spelling every downstream branch
	// logs — stored == logged == sent by construction.
	id = NormalizedID(id)
	if cfg == nil || strings.TrimSpace(cfg.ClientID) == "" || strings.TrimSpace(cfg.RedirectURL) == "" {
		if logf != nil {
			logf("connectors: auth URL build failed: unconfigured client (%q)", id)
		}
		return ""
	}
	// Work on a trimmed copy from here on: the checks below and the
	// built URL must see identical values. ClientSecret is folded too
	// even though AuthCodeURL never sends it — the copy contract is
	// "everything trimmed", so the next edit copies one pattern.
	trimmed := *cfg
	trimmed.ClientID = strings.TrimSpace(trimmed.ClientID)
	trimmed.ClientSecret = strings.TrimSpace(trimmed.ClientSecret)
	trimmed.RedirectURL = strings.TrimSpace(trimmed.RedirectURL)
	trimmed.AuthURL = strings.TrimSpace(trimmed.AuthURL)
	trimmed.TokenURL = strings.TrimSpace(trimmed.TokenURL)
	if trimmed.AuthURL == "" || trimmed.TokenURL == "" {
		// A hand-built cfg with no endpoints mints a consent URL Google
		// can only 400. OAuthConfigFromEnv always sets both to the
		// Google endpoints, so this guards direct callers only.
		if logf != nil {
			logf("connectors: auth URL build failed for %q: missing endpoint", id)
		}
		return ""
	}
	canonical, err := CanonicalRedirectURL(trimmed.RedirectURL)
	if err != nil {
		// Same shape gate as the env path: a hand-built redirect that
		// would never match a registration fails here, not in the
		// browser with redirect_uri_mismatch.
		if logf != nil {
			logf("connectors: auth URL build failed for %q: %v", id, err)
		}
		return ""
	}
	// Build from the canonicalised shape, not cfg.RedirectURL verbatim,
	// so AuthURL and Exchange agree on the exact string.
	trimmed.RedirectURL = canonical
	// Folded and trimmed like every store verb — unknown or empty inputs
	// fail closed, and overlong inputs fail closed too (same
	// MaxStateFieldLen bound the stores enforce): a multi-KB state or
	// verifier would mint a consent URL Google 414s, with only log spam to
	// show for it. The gateway mints fixed-size values via NewState, so
	// this guards direct callers. (id already folded at the top; the
	// state/verifier trims below are the per-call normalisation.)
	state = strings.TrimSpace(state)
	verifier = strings.TrimSpace(verifier)
	// Empty → length → Known, matching Exchange/CreateState/ConsumeState/
	// SetTokens: an oversized input for an unknown id 400s on length, so
	// the unknown-id path cannot be told apart by which error fires first.
	if state == "" || verifier == "" {
		if logf != nil {
			logf("connectors: auth URL build failed for %q: empty state or verifier", id)
		}
		return ""
	}
	if len(state) > MaxStateFieldLen || len(verifier) > MaxStateFieldLen {
		if logf != nil {
			logf("connectors: auth URL build failed for %q: oversized state or verifier", id)
		}
		return ""
	}
	if hasControl(state) || hasControl(verifier) {
		if logf != nil {
			logf("connectors: auth URL build failed for %q: invalid state or verifier", id)
		}
		return ""
	}
	if !Known(id) {
		if logf != nil {
			logf("connectors: auth URL build failed: unknown connector %q", id)
		}
		return ""
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return trimmed.oauth2(GoogleScopes(id)).AuthCodeURL(state,
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
	if hasControl(code) || hasControl(verifier) {
		return nil, NewStoreError("oauth code or verifier invalid", false)
	}
	// Config-shape before identity, matching AuthURL: a bad redirect or
	// blank client fields 400s on shape the same way whether the id is
	// known or not, so the unknown-id path cannot be told apart by which
	// guard fires first. Canonicalise the redirect the same way AuthURL
	// does: the token endpoint compares redirect_uri exactly, so both
	// helpers must mint and send the same string. Trimmed like every other
	// input: padded client fields are a caller bug, and the checked
	// values must match what is sent.
	redirect, err := CanonicalRedirectURL(cfg.RedirectURL)
	if err != nil {
		return nil, NewStoreError("oauth client redirect URL is invalid", false)
	}
	cc := *cfg
	cc.ClientID = strings.TrimSpace(cc.ClientID)
	cc.ClientSecret = strings.TrimSpace(cc.ClientSecret)
	cc.AuthURL = strings.TrimSpace(cc.AuthURL)
	cc.TokenURL = strings.TrimSpace(cc.TokenURL)
	// Length gate above covers the trimmed values copied here — keep the
	// copy below the gate, or padding could push a real credential over
	// the cap (the padded-push class fixed for the trim-copy contract).
	if cc.ClientID == "" || cc.ClientSecret == "" {
		return nil, NewStoreError("oauth client is not configured", false)
	}
	if cc.AuthURL == "" || cc.TokenURL == "" {
		return nil, NewStoreError("oauth client endpoints are not configured", false)
	}
	// Folded like AuthURL above and every store verb: same logical id,
	// same answer, wherever the caller normalised.
	id = NormalizedID(id)
	if !Known(id) {
		return nil, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	cc.RedirectURL = redirect
	tok, err := cc.oauth2(GoogleScopes(id)).Exchange(ctx, code,
		oauth2.SetAuthURLParam("code_verifier", verifier))
	if err != nil {
		if logf != nil {
			// Scrubbed + capped like the domain mapper below — and
			// quoted, so an inner control byte in provider text cannot
			// split the log line.
			logf("connectors: grant exchange failed for %q: %q", id, approvals.ScrubError(err.Error()))
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
	// Normalise before return: the checked strings and the delivered
	// bytes must be identical, so a future caller that seals
	// tok.RefreshToken raw cannot seal padding the refresher can't use
	// (same class as the gateway's trim-before-seal, enforced here so
	// no caller has to remember it).
	tok.RefreshToken = strings.TrimSpace(tok.RefreshToken)
	tok.AccessToken = strings.TrimSpace(tok.AccessToken)
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
	// A past timestamp means the same as zero (renew immediately) by
	// accident of comparison, not by a separate contract: clock skew or
	// a stubby test token lands here, and the refresher cannot tell it
	// from a genuinely stale row — nor does it need to. Far-future values
	// are clamped to now+MaxAccessLifetime at write time — enforced in
	// three places (gateway connect + both SetTokens), so a stored expiry
	// never reads as "don't renew for decades".
	AccessExpiryUnixMilli int64 `bson:"accessExpiry" json:"-"`
}

// OAuthState is one pending connect attempt: connector + PKCE verifier,
// consumed (deleted) on first callback so a code can never be exchanged twice.
type OAuthState struct {
	State     string    `bson:"_id" json:"-"`
	Connector string    `bson:"connector" json:"-"`
	Verifier  string    `bson:"verifier" json:"-"`
	ExpiresAt time.Time `bson:"expiresAt" json:"-"`
	// RedirectURI pins the exact redirect sent at mint time, so a config
	// rotation mid-consent cannot strand the attempt with a mismatch.
	// Empty means minted before this column existed: fall back to the
	// live config instead of failing an otherwise valid attempt.
	RedirectURI string `bson:"redirectUri,omitempty" json:"-"`
}
