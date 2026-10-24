package connectors

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
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
// full-access variants:
//
//   - gmail: readonly + compose. compose creates/updates/deletes DRAFTS
//     only — it cannot send. The exact scope the draft-only rule needs:
//     the token is incapable of sending even if the relay check vanished.
//   - calendar: readonly. Draft events are agent-side objects (created
//     locally, sent nowhere) until the approval-gate flow covers writes,
//     so the grant carries no write bit at all.
//   - drive: readonly + file. readonly covers list/read; file covers
//     "files this app created" — the save-agent-produced-file path —
//     without granting the whole Drive.
//
// Follow-up sync obligation (same class as IsDraftOnlySend): if a wired MCP
// server needs a broader scope, BOTH this table and the tool policy move
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
			"https://www.googleapis.com/auth/calendar.readonly",
		}
	case Drive:
		return []string{
			"https://www.googleapis.com/auth/drive.readonly",
			"https://www.googleapis.com/auth/drive.file",
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

// AuthURL builds the Google consent URL for one connector attempt. Offline
// access (a refresh token) + consent prompt every time: without
// prompt=consent Google returns a refresh token only on the FIRST grant,
// and a reconnect that yields no refresh token would silently downgrade the
// row to access-token-only with no renewal path.
//
// Caller must Known-check the id first: unknown ids yield a scope-less URL
// the token endpoint would still answer. A nil cfg fails closed with "" —
// cheaper than a panic in a server process. The gateway route, CreateState,
// and Exchange all check; this helper trusts its caller by contract so the
// check lives in exactly the layers that own the failure modes.
func AuthURL(cfg *OAuthConfig, id, state, verifier string) string {
	if cfg == nil {
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
// token endpoint. cfg must be non-nil (gateway's ConnectorOAuth nil-check
// is the enforcement point; see AuthURL's contract note) — a nil cfg fails
// closed here rather than panicking in the server process. HTTP errors
// surface as a generic 502 ("grant exchange failed") with the provider's
// error_description only in the log — oauth2 errors echo the token
// endpoint verbatim, and the client needs a retry signal, not provider
// internals. logf may be nil (tests).
func Exchange(ctx context.Context, cfg *OAuthConfig, id, code, verifier string, logf func(string, ...any)) (*oauth2.Token, error) {
	if cfg == nil {
		return nil, NewStoreError("oauth client is not configured", false)
	}
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
	if tok.RefreshToken == "" {
		// No refresh token means no renewal path: storing this row as
		// connected would strand it at the first access-token expiry.
		// Fail the connect loudly instead of a slow-burn disconnect.
		return nil, NewStoreError("oauth exchange returned no refresh token; reconnect and approve offline access", true)
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
	// May be empty: the refresher tolerates a missing access token (first
	// use, or an offline-only rotation) and renews from the refresh token.
	// The refresh cipher above must never be empty — same rule the store
	// enforces — or the row strands at the first access-token expiry.
	AccessCipher string `bson:"accessToken" json:"-"`
	// AccessExpiryUnixMilli is when the access token stops working; the
	// refresher renews before this, not after it fails.
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
