package connectors

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
)

// Server auth for the connector MCP servers (gmail/calendar binaries).
//
// The servers never mint consent URLs — they only call Google with the grant
// the OAuth layer already sealed into the store (see oauth.go). So they need
// the client credentials + envelope key, but NOT the redirect URL: requiring
// it here would 500 a working server over a value it never uses.

// ServerOAuthConfig builds the OAuth client for the connector MCP servers.
// ANY missing piece is an error (nil config, fail closed): without the
// client pair Google cannot refresh, and without the key the stored grant
// cannot be decrypted — a half-open server would fail every call anyway,
// so it refuses to start instead.
func ServerOAuthConfig() (*OAuthConfig, error) {
	clientID := strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("GOOGLE_CLIENT_SECRET"))
	if clientID == "" || clientSecret == "" {
		return nil, errors.New("connectors: GOOGLE_CLIENT_ID/GOOGLE_CLIENT_SECRET must both be set for the connector servers")
	}
	key, err := KeyFromEnv()
	if err != nil {
		return nil, err
	}
	return &OAuthConfig{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Key:          key,
		AuthURL:      googleAuthURL,
		TokenURL:     googleTokenURL,
	}, nil
}

// GrantClient returns an *http.Client that calls Google as the connector:
// the stored grant is decrypted, refreshed on expiry, and the refreshed
// tokens are sealed back into the store (lazy refresh-on-use — there is no
// background refresher, so the first call after expiry pays one refresh).
//
// No grant, undecryptable grant, or failed refresh is a reconnect error,
// never a secrets prompt: the handler surfaces it and the app banners
// Settings. The store holds ciphertext only; plaintext lives in this
// process just long enough to build the token source.
//
// logf (may be nil, same contract as Exchange) receives the provider-side
// detail the agent must never see: error codes for operator triage in
// container logs, never tokens.
func GrantClient(ctx context.Context, store Store, cfg *OAuthConfig, id string, logf func(string, ...any)) (*http.Client, error) {
	src, err := GrantTokenSource(ctx, store, cfg, id, logf)
	if err != nil {
		return nil, err
	}
	return oauth2.NewClient(ctx, src), nil
}

// GrantTokenSource is the refresh-and-persist token source behind
// GrantClient, exposed for callers that need the bearer token itself
// rather than an HTTP client. Same reconnect-error contract as GrantClient.
func GrantTokenSource(ctx context.Context, store Store, cfg *OAuthConfig, id string, logf func(string, ...any)) (oauth2.TokenSource, error) {
	if cfg == nil {
		return nil, NewStoreError("oauth client is not configured", false)
	}
	id = NormalizedID(id)
	if !Known(id) {
		return nil, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	row, err := store.GetTokens(ctx, id)
	if err != nil {
		// Grant-less (NotFound) is a reconnect answer; a down store is an
		// upstream failure. Conflating the two would render a Mongo
		// outage as "reconnect in Settings" and send triage after the
		// wrong layer. Unknown ids 404 ("unknown connector") while
		// grant-less rows 400 with reconnect guidance — different
		// messages, but neither oracles stored-grant existence: ids are
		// a fixed public pair and these callers pass constants, so
		// there is nothing to probe.
		var se *StoreError
		if errors.As(err, &se) && se.NotFound {
			return nil, NewStoreError(DisplayName(id)+" is not connected; reconnect in Settings — never paste a token in chat", false)
		}
		if se != nil {
			return nil, se
		}
		return nil, NewStoreError("connector store unavailable", true)
	}
	key := cfg.Key
	refresh, err := DecryptToken(key, row.RefreshCipher)
	if err != nil {
		return nil, NewStoreError(DisplayName(id)+" grant is unreadable; reconnect in Settings — never paste a token in chat", false)
	}
	var access string
	if row.AccessCipher != "" {
		access, err = DecryptToken(key, row.AccessCipher)
		if err != nil {
			return nil, NewStoreError(DisplayName(id)+" grant is unreadable; reconnect in Settings — never paste a token in chat", false)
		}
	}
	tok := &oauth2.Token{RefreshToken: refresh, AccessToken: access}
	if row.AccessExpiryUnixMilli != 0 {
		tok.Expiry = time.UnixMilli(row.AccessExpiryUnixMilli)
	} else if access != "" {
		// Zero stored expiry reads as NEVER expired to oauth2
		// (expired() returns false on IsZero), so a live access token
		// with no recorded expiry would be served forever without
		// refresh. Fail closed: backdate it so the first use refreshes
		// and persists a real expiry. (Empty access needs no help:
		// invalid tokens always refresh.)
		tok.Expiry = time.Now().Add(-time.Minute)
	}
	// Zero expiry AND empty access reads as already expired: the inner
	// source refreshes on first use instead of serving a dead token.
	// Note the failure-mode asymmetry this creates: a provider that
	// routinely omits expires_in turns lazy refresh-on-use into
	// refresh-on-EVERY-use (one token round-trip + one sealed store
	// write per tool call). Correct (fail-closed beats serving a
	// possibly-dead token) and fine at this cardinality; if a provider
	// ever omits expiry routinely, the fix is a minimum cache interval
	// here, not callers working around it.
	return &persistingSource{
		inner:   cfg.oauth2(GoogleScopes(id)).TokenSource(ctx, tok),
		store:   store,
		key:     key,
		id:      id,
		refresh: refresh,
		access:  access,
		expiry:  tok.Expiry,
		logf:    logf,
	}, nil
}

// persistingSource wraps the oauth2 token source with sealed write-back:
// the first Token() after expiry refreshes against Google, and the new
// pair is sealed into the store so the next process (or the next row read)
// starts current. Comparison is on plaintext — ciphertext carries a fresh
// random nonce per seal, so sealed bytes never equal each other.
type persistingSource struct {
	inner   oauth2.TokenSource
	store   Store
	key     [32]byte
	id      string
	mu      sync.Mutex
	refresh string
	access  string
	expiry  time.Time
	// logf carries provider-side failure detail to container logs
	// (nil = silent, same contract as Exchange). Never tokens: only
	// error codes and ids cross this boundary.
	logf func(string, ...any)
}

func (s *persistingSource) Token() (*oauth2.Token, error) {
	tok, err := s.inner.Token()
	if err != nil {
		// Refresh failed. invalid_grant means the grant itself is dead
		// (revoked, rotated out, wrong client) — no retry will revive
		// it, so answer the same reconnect guidance as the grant-less
		// path instead of leaking provider error text to the agent.
		// Anything else (network blip, Google 5xx) is transient
		// upstream: retryable, not a reconnect. Either way the
		// provider code is logged server-side (id + code, never
		// tokens) so the operator has a triage signal the agent is
		// deliberately not given — same split as Exchange's logf.
		// Deterministic config errors (invalid_client, mismatched
		// registration) land in the upstream bucket by design: the
		// log line above is their operator signal, not a distinct
		// agent message.
		var re *oauth2.RetrieveError
		code := ""
		if errors.As(err, &re) {
			code = re.ErrorCode
		}
		if s.logf != nil {
			s.logf("connectors: token refresh for %s failed (%s): %v", s.id, code, err)
		}
		if code == "invalid_grant" {
			return nil, NewStoreError(DisplayName(s.id)+" is not connected; reconnect in Settings — never paste a token in chat", false)
		}
		return nil, NewStoreError("connector token refresh failed", true)
	}
	// Single copy-on-write up front: the inner source may hand back a
	// shared/cached pointer, and the access-only branch below mutates
	// before returning. Never hand the inner cache back to the caller.
	// (The unexported raw-claims map stays shared — nobody mutates it
	// on this path, so field-copy is the documented boundary, not a
	// deep clone.)
	cp := *tok
	tok = &cp
	s.mu.Lock()
	defer s.mu.Unlock()
	// Expiry participates in the comparison: same pair with an extended
	// expiry must still persist, or the stored AccessExpiryUnixMilli
	// goes stale and the next process refreshes one cycle early.
	if tok.RefreshToken == s.refresh && tok.AccessToken == s.access && tok.Expiry.Equal(s.expiry) {
		return tok, nil
	}
	if strings.TrimSpace(tok.RefreshToken) == "" {
		// Access-only rotation: Google normally omits refresh_token on
		// re-issue, and the inner source surfaces that omission verbatim.
		// Sealing "" would fail (EncryptToken refuses empty) or worse —
		// overwrite the good stored refresh. Keep it, persist the new
		// access side only (the copy above already detached us from the
		// inner cache, so this field write is safe).
		tok.RefreshToken = s.refresh
	}
	refreshSealed, err := EncryptToken(s.key, tok.RefreshToken)
	if err != nil {
		return nil, err
	}
	var accessSealed string
	if strings.TrimSpace(tok.AccessToken) != "" {
		accessSealed, err = EncryptToken(s.key, tok.AccessToken)
		if err != nil {
			return nil, err
		}
	}
	var expiry int64
	if !tok.Expiry.IsZero() {
		expiry = tok.Expiry.UnixMilli()
	}
	// Best effort is wrong here: silently dropping the refreshed pair
	// strands the NEXT process on the old (possibly rotated-out) refresh
	// token. Fail the call so the agent retries instead of running on a
	// grant the store no longer matches. Context is detached from the
	// request: the write must land even if the caller gave up waiting.
	wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.store.SetTokens(wctx, s.id, TokenRow{
		RefreshCipher:         refreshSealed,
		AccessCipher:          accessSealed,
		AccessExpiryUnixMilli: expiry,
	}); err != nil {
		// Curated upstream error, not the raw driver: every other
		// store failure on this path answers the reconnect-vs-upstream
		// taxonomy (which is also the HTTP status contract), and the
		// MCP handler surfaces err.Error() verbatim to the agent.
		// The write failure itself is logged server-side (id only,
		// never tokens) so a Mongo outage names the store, not
		// Google, in container logs.
		if s.logf != nil {
			s.logf("connectors: token write for %s failed", s.id)
		}
		return nil, NewStoreError("connector token write failed", true)
	}
	s.refresh = tok.RefreshToken
	s.access = tok.AccessToken
	s.expiry = tok.Expiry
	return tok, nil
}
