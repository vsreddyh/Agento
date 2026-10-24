package connectors

import (
	"context"
	"strings"
	"sync"
	"time"

	"agento/internal/approvals"
)

// Store is the persistence surface. *MongoStore and *MemoryStore implement
// it; the gateway owns one. Reads never return secrets (there are none in
// this shape); the Mongo backend keeps refresh tokens in a separate field
// that no read method selects into Connector.
type Store interface {
	// List returns the three v1 rows, synthesising disconnected rows for
	// ids with no document yet (a fresh deploy lists three disconnected
	// connectors, not an empty list).
	List(ctx context.Context) ([]Connector, error)
	// Get returns one row, or NotFound for an unknown id.
	Get(ctx context.Context, id string) (Connector, error)
	// SetStatus flips one known id's status (connect/expire path). Unknown
	// ids are NotFound. Only the expired path carries error text (scrubbed);
	// connected and disconnected rows carry none. ConnectedAt is
	// first-connect: re-marking connected preserves it.
	//
	// OAuth follow-up note: nothing sets connected in v1 (connect 501s), and
	// when it does it must write tokens + status in ONE UpdateOne — a
	// connected-with-no-tokens row would otherwise be servable between two
	// writes. That needs a NEW builder (e.g. connectUpdateDoc carrying
	// tokens+status); do not extend statusUpdateDoc, which is status-only by
	// contract and pinned as such.
	SetStatus(ctx context.Context, id string, st Status, lastError string) (Connector, error)
	// Disconnect stops future access for one id: status disconnected and
	// the token fields cleared server-side (see store_mongo.go). Unknown
	// ids are NotFound; already-disconnected is a no-op success.
	Disconnect(ctx context.Context, id string) (Connector, error)
	// SetTokens persists an encrypted grant (see TokenRow: the store holds
	// ciphertext opaquely and needs no key). Unknown ids are NotFound;
	// empty refresh cipher is rejected (a stored grant must always renew).
	SetTokens(ctx context.Context, id string, tok TokenRow) error
	// GetTokens reads back the ciphertext grant for renewal. Unknown ids
	// and rows with no grant are NotFound — callers treat both as
	// "reconnect".
	GetTokens(ctx context.Context, id string) (TokenRow, error)
	// CreateState records a pending OAuth attempt.
	CreateState(ctx context.Context, st OAuthState) error
	// ConsumeState returns and deletes a pending attempt. Unknown, already
	// consumed, or expired states are NotFound — a code can never be
	// exchanged twice, and a stale tap fails as "try connect again".
	ConsumeState(ctx context.Context, state string) (OAuthState, error)
}

// MemoryStore is the in-process backend: rows die with the process, like
// every other memory fallback. The gateway still serves the routes, which
// keeps a development stack usable without pretending tokens survive.
type MemoryStore struct {
	mu     sync.RWMutex
	rows   map[string]Connector
	tokens map[string]TokenRow
	states map[string]OAuthState
	// now is the clock for state expiry (ConsumeState boundary). Nil means
	// time.Now; tests pin the exact boundary tick without sleeping. Mongo
	// has no equivalent hook (wall-clock inline, untestable without live
	// Atlas) — same !Before rule, so behavior matches; only the pin
	// differs, and DB tests need Atlas anyway.
	now func() time.Time
}

// timeNow resolves the store clock. Tests pin the exact expiry boundary
// tick through this (expiresAt==now counts as expired).
func (s *MemoryStore) timeNow() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// NewMemoryStore builds a store with no rows (List synthesises disconnected).
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		rows:   make(map[string]Connector),
		tokens: make(map[string]TokenRow),
		states: make(map[string]OAuthState),
	}
}

// DefaultRow synthesises the disconnected row for an id with no document.
// UpdatedAt stays zero (and the wire omits it): absence means never touched,
// which is exactly the fresh-deploy state worth distinguishing while
// debugging "was this ever written". Note the companion case: Disconnect on
// a never-touched id upserts with UpdatedAt set, so an explicitly-off row
// carries updated_at where a never-touched one omits it — same status,
// different shape, and the difference is the signal (explicit-off vs never
// touched), not an inconsistency.
func DefaultRow(id string) Connector {
	return Connector{
		ID:          id,
		DisplayName: DisplayName(id),
		Scopes:      ScopesFor(id),
		Status:      StatusDisconnected,
	}
}

func (s *MemoryStore) List(_ context.Context) ([]Connector, error) {
	// Copy under the read lock, repair outside it: backfillDisplay runs
	// ScrubError regexes, and holding RLock across regex scan blocks the
	// Lock that SetStatus/Disconnect need. Same return value, no
	// lock-held scrub.
	s.mu.RLock()
	rows := make([]Connector, 0, len(listIDs))
	ids := make([]string, 0, len(listIDs))
	for _, id := range listIDs {
		ids = append(ids, id)
		if c, ok := s.rows[id]; ok {
			rows = append(rows, c)
		} else {
			rows = append(rows, DefaultRow(id))
		}
	}
	s.mu.RUnlock()
	out := make([]Connector, 0, len(rows))
	for i, c := range rows {
		out = append(out, backfillDisplay(c, ids[i]))
	}
	return out, nil
}

func (s *MemoryStore) Get(_ context.Context, id string) (Connector, error) {
	id = NormalizedID(id)
	if !Known(id) {
		return Connector{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	s.mu.RLock()
	c, ok := s.rows[id]
	s.mu.RUnlock()
	if ok {
		return backfillDisplay(c, id), nil
	}
	return DefaultRow(id), nil
}

func (s *MemoryStore) SetStatus(_ context.Context, id string, st Status, lastError string) (Connector, error) {
	id = NormalizedID(id)
	if !Known(id) {
		return Connector{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	if !st.Valid() {
		return Connector{}, NewStoreError("unknown connector status", false)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.rows[id]
	if !ok {
		c = DefaultRow(id)
	}
	now := time.Now().UnixMilli()
	// Only the expired path carries error text: connected (healthy) and
	// disconnected (off) rows carry none, so a stale error can never render
	// as failure text on a healthy row. Scrubbed (approvals.ScrubError):
	// lastError is app-visible and the OAuth follow-up will store provider
	// errors here.
	wantErr := ""
	if st == StatusExpired {
		wantErr = approvals.ScrubError(lastError)
	}
	// Transition-aware like Disconnect: compare BEFORE mutating — the old
	// code assigned c.Status first, so every same-error flip early-returned
	// a mutated-but-unpersisted copy (caller saw the new status, the store
	// kept the old). Three tiers like Mongo: pure no-op (display fresh,
	// error matches — no store touch), repair-only (display/error refresh,
	// no updated_at bump), full transition write. A missing doc always
	// takes the full write.
	// updated_at means last STATUS transition, not last failure reason: an
	// expired error-text change (aged out → revoked) heals the text without
	// moving the timestamp — triage reads failure recency from last_error
	// content, not from updated_at.
	if ok && c.Status == st && displayFieldsFresh(id, c.DisplayName, c.Scopes, c.LastError, wantErr) {
		return backfillDisplay(c, id), nil
	}
	if ok && c.Status == st {
		// Repair-only: heal display + error text, leave every timestamp
		// (and, for connected, the stamp) exactly where the last
		// transition put it.
		c.DisplayName = DisplayName(id)
		c.Scopes = ScopesFor(id)
		c.LastError = wantErr
		s.rows[id] = c
		return backfillDisplay(c, id), nil
	}
	// Same self-heal as the Mongo backend: display fields rewrite from the
	// current constants on every write (transition and no-op alike), so a
	// future scope/name change cannot leave stale values on existing rows.
	// Reads repair per response via backfillDisplay without storing — GETs
	// stay read-only; the next write persists the repair.
	c.DisplayName = DisplayName(id)
	c.Scopes = ScopesFor(id)
	c.Status = st
	c.LastError = wantErr
	c.UpdatedAt = now
	// First-ever connect, shared with Mongo via resolveConnectedAt (same
	// helper, same semantic on both backends).
	if st == StatusConnected {
		c.ConnectedAt = resolveConnectedAt(c.ConnectedAt, now)
	}
	s.rows[id] = c
	return backfillDisplay(c, id), nil
}

func (s *MemoryStore) Disconnect(_ context.Context, id string) (Connector, error) {
	id = NormalizedID(id)
	if !Known(id) {
		return Connector{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Tokens die on every disconnect path — including the pure no-op below.
	// Disconnect means "no future access", and a stored grant on a
	// disconnected row (hand-edit, bug) must not survive it. Same $unset
	// the Mongo backend applies; updated_at untouched by the delete itself.
	delete(s.tokens, id)
	// Pending attempts die with it: a browser completing an old consent
	// after this disconnect must fail at ConsumeState ("try connect again"),
	// not silently reconnect without fresh intent (see the Mongo twin).
	for state, st := range s.states {
		if st.Connector == id {
			delete(s.states, state)
		}
	}
	c, ok := s.rows[id]
	if !ok {
		c = DefaultRow(id)
	}
	// Idempotent no-op like the Mongo backend: already disconnected refreshes
	// display AND clears lastError without moving updated_at (stays the last
	// transition), still success — so a scope change lands and a corrupt
	// error clears even on rows that never transition again. Pure no-op when
	// there is nothing to heal: no store touch at all.
	if ok && c.Status == StatusDisconnected &&
		displayFieldsFresh(id, c.DisplayName, c.Scopes, c.LastError, "") {
		return backfillDisplay(c, id), nil
	}
	if ok && c.Status == StatusDisconnected {
		c.DisplayName = DisplayName(id)
		c.Scopes = ScopesFor(id)
		c.LastError = ""
		s.rows[id] = c
		return backfillDisplay(c, id), nil
	}
	c.DisplayName = DisplayName(id)
	c.Scopes = ScopesFor(id)
	c.Status = StatusDisconnected
	c.LastError = ""
	c.UpdatedAt = time.Now().UnixMilli()
	// Token columns cleared at the top of this function (all paths),
	// mirroring the Mongo $unset — see above.
	s.rows[id] = c
	return backfillDisplay(c, id), nil
}

// SetTokens stores an encrypted grant. Ciphertext-opaque: the store never
// sees plaintext and enforces only "a stored grant renews" (non-empty
// refresh cipher). Tokens die with the process like every other row here.
func (s *MemoryStore) SetTokens(_ context.Context, id string, tok TokenRow) error {
	id = NormalizedID(id)
	if !Known(id) {
		return &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	if strings.TrimSpace(tok.RefreshCipher) == "" {
		return NewStoreError("connector grant needs a refresh token", false)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[id] = tok
	return nil
}

// GetTokens reads the ciphertext grant. Unknown ids and grant-less rows are
// both NotFound: the caller reconnects either way, and distinguishing them
// would oracle stored-grant existence to any bearer holder.
func (s *MemoryStore) GetTokens(_ context.Context, id string) (TokenRow, error) {
	id = NormalizedID(id)
	if !Known(id) {
		return TokenRow{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	tok, ok := s.tokens[id]
	if !ok || strings.TrimSpace(tok.RefreshCipher) == "" {
		return TokenRow{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	return tok, nil
}

// CreateState records a pending OAuth attempt. Zero ExpiresAt defaults to
// now+StateTTL like the Mongo backend (TTL index) — callers pass an
// explicit expiry only for tests; without this, a forgotten expiry field
// would mint an immortal row in a store with no sweeper.
func (s *MemoryStore) CreateState(_ context.Context, st OAuthState) error {
	st.Connector = NormalizedID(st.Connector)
	if strings.TrimSpace(st.State) == "" {
		return NewStoreError("oauth state needs an id", false)
	}
	if !Known(st.Connector) {
		return &StoreError{Msg: "unknown connector " + st.Connector, NotFound: true}
	}
	if st.ExpiresAt.IsZero() {
		st.ExpiresAt = s.timeNow().Add(StateTTL)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Opportunistic sweep: no TTL thread exists here, so reap expired rows
	// on the write path that creates them. Keeps an abandoned-connect spam
	// from growing the map without bound; same lazy-expiry contract as the
	// Mongo TTL, enforced at write time instead of by the database.
	now := s.timeNow()
	for state, existing := range s.states {
		if !existing.ExpiresAt.IsZero() && !now.Before(existing.ExpiresAt) {
			delete(s.states, state)
		}
	}
	// Duplicate check BEFORE the supersede below: reusing the same state
	// string for the same connector (caller bug — states are 128-bit rand)
	// must 400 like the Mongo duplicate-key path, not silently overwrite
	// after deleting the row it collides with.
	if _, exists := s.states[st.State]; exists {
		return NewStoreError("oauth state already exists", false)
	}
	// One pending attempt per connector: a new tap supersedes the old one
	// (its browser tab fails at ConsumeState with "try connect again").
	// Bounds live rows to one per connector regardless of tap spam — not a
	// hardcoded count, so a fourth connector doesn't invalidate it. The
	// sweep above only reaps expired rows, so without this a bearer holder
	// could mint unlimited live states inside the 10-minute window.
	for state, existing := range s.states {
		if existing.Connector == st.Connector {
			delete(s.states, state)
		}
	}
	s.states[st.State] = st
	return nil
}

// ConsumeState returns and deletes a pending attempt. Unknown and expired
// states are the same NotFound: expiry is lazy (no sweeper at 3 rows/hour),
// and the caller message is "try connect again" either way.
func (s *MemoryStore) ConsumeState(_ context.Context, state string) (OAuthState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[state]
	if !ok {
		return OAuthState{}, &StoreError{Msg: "unknown or expired oauth state", NotFound: true}
	}
	delete(s.states, state)
	// Boundary: expiresAt itself counts as expired (matches the Mongo TTL,
	// which reaps at expiry, not after). Clock is injectable so tests pin
	// this tick without sleeping.
	if !st.ExpiresAt.IsZero() && !s.timeNow().Before(st.ExpiresAt) {
		return OAuthState{}, &StoreError{Msg: "unknown or expired oauth state", NotFound: true}
	}
	return st, nil
}
