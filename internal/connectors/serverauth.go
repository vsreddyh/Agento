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
func GrantClient(ctx context.Context, store Store, cfg *OAuthConfig, id string) (*http.Client, error) {
	if cfg == nil {
		return nil, NewStoreError("oauth client is not configured", false)
	}
	id = NormalizedID(id)
	if !Known(id) {
		return nil, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	row, err := store.GetTokens(ctx, id)
	if err != nil {
		// Unknown or grant-less: same reconnect answer either way, so the
		// error never oracles stored-grant existence.
		return nil, NewStoreError(DisplayName(id)+" is not connected; reconnect in Settings — never paste a token in chat", false)
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
	}
	// Zero expiry (or empty access) reads as already expired: the inner
	// source refreshes on first use instead of serving a dead token.
	src := &persistingSource{
		inner:   cfg.oauth2(GoogleScopes(id)).TokenSource(ctx, tok),
		store:   store,
		key:     key,
		id:      id,
		refresh: refresh,
		access:  access,
	}
	return oauth2.NewClient(ctx, src), nil
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
}

func (s *persistingSource) Token() (*oauth2.Token, error) {
	tok, err := s.inner.Token()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if tok.RefreshToken == s.refresh && tok.AccessToken == s.access {
		return tok, nil
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
		return nil, err
	}
	s.refresh = tok.RefreshToken
	s.access = tok.AccessToken
	return tok, nil
}
