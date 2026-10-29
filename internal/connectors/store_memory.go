package connectors

import (
	"context"
	"sync"
	"time"
	"unicode/utf8"

	"agento/internal/approvals"
)

// Store is the persistence surface. *MongoStore and *MemoryStore implement
// it; the gateway owns one. Reads never return secrets (there are none in
// this shape); the Mongo backend keeps refresh tokens in a separate field
// that no read method selects into Connector.
type Store interface {
	// Contexts must be bounded by the caller: the memory backend ignores
	// ctx (no IO to hang), but the Mongo backend runs every verb on the
	// passed ctx verbatim — an unbounded caller ctx plus a hung Atlas
	// hangs the verb forever. The gateway always passes a 5s/30s ctx;
	// direct/future callers must do the same (context.WithTimeout).
	// Deliberately stricter than Exchange, which self-bounds when the
	// caller passes none: an outbound HTTPS round-trip has unbounded
	// latency by nature (hung endpoint either side), while these verbs are
	// local indexed reads whose only unbounded case is a hung database —
	// and a shared in-verb default here would mask a caller that forgot
	// cancellation propagation as well as one that forgot a deadline.
	// Same contract as database/sql: the deadline is the caller's job.
	// List returns the two v1 rows, synthesising disconnected rows for
	// ids with no document yet (a fresh deploy lists two disconnected
	// connectors, not an empty list).
	List(ctx context.Context) ([]Connector, error)
	// Get returns one row, or NotFound for an unknown id.
	Get(ctx context.Context, id string) (Connector, error)
	// SetStatus flips one known id's status (connect/expire path). Unknown
	// ids are NotFound. Only the expired path carries error text (scrubbed);
	// connected and disconnected rows carry none. ConnectedAt is
	// first-connect: re-marking connected preserves it.
	//
	// Ordering contract (landed, see the callback in
	// internal/gateway/connectors.go): tokens land BEFORE the status
	// flips — SetTokens first, SetStatus(connected) second — so a
	// connected-with-no-tokens row is never servable. Two writes, not
	// one UpdateOne: the token write and the status write are separate
	// store verbs, and the crash window between them reads as
	// still-disconnected (never as connected-without-tokens).
	SetStatus(ctx context.Context, id string, st Status, lastError string) (Connector, error)
	// Disconnect stops future access for one id: status disconnected and
	// the token fields cleared server-side (see store_mongo.go). Unknown
	// ids are NotFound; already-disconnected is a no-op success.
	Disconnect(ctx context.Context, id string) (Connector, error)
	// Durable reports whether state survives a process restart (Mongo) or
	// not (memory). The gateway enables OAuth only on a durable store: an
	// in-memory grant store would strand connected rows at the next
	// restart (status says connected, tokens are gone), so connect 501s
	// without one. A method, not a concrete type-assert at the call site —
	// a future wrapper/decorator must consciously choose its answer.
	Durable() bool
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
// tick through this (expiresAt==now counts as expired). Only expiry math
// goes through here: UpdatedAt/ConnectedAt stamps stay on wall-clock
// deliberately (display-only, never an expiry input), so "injectable"
// means the lifetime decisions, not every timestamp the store writes.
func (s *MemoryStore) timeNow() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// Durable reports false: rows die with the process, so the gateway keeps
// OAuth disabled on this backend rather than strand connected rows at the
// next restart.
func (s *MemoryStore) Durable() bool { return false }

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
	// Length before fold and before Known: ids are two short fixed
	// strings, so anything over the cap is certainly unknown — fail
	// without the normalize alloc or a quoted value in the 404.
	if len(id) > maxConnectorIDLen {
		return Connector{}, NewStoreError("oversized connector id", false)
	}
	id = NormalizedID(id)
	// Controls fail closed like every sibling input: a control-bearing
	// id would otherwise ride the "unknown connector" echo below into
	// the JSON body and the scrubbed log line. Checked post-fold so
	// edge Cf forgiven by TrimCredential still folds instead of 400ing.
	if HasControl(id) || !utf8.ValidString(id) {
		return Connector{}, NewStoreError("invalid connector id", false)
	}
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
	// Length before fold and before Known: ids are two short fixed
	// strings, so anything over the cap is certainly unknown — fail
	// without the normalize alloc or a quoted value in the 404.
	if len(id) > maxConnectorIDLen {
		return Connector{}, NewStoreError("oversized connector id", false)
	}
	id = NormalizedID(id)
	// Controls fail closed like every sibling input: a control-bearing
	// id would otherwise ride the "unknown connector" echo below into
	// the JSON body and the scrubbed log line. Checked post-fold so
	// edge Cf forgiven by TrimCredential still folds instead of 400ing.
	if HasControl(id) || !utf8.ValidString(id) {
		return Connector{}, NewStoreError("invalid connector id", false)
	}
	if !Known(id) {
		return Connector{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	if !st.Valid() {
		return Connector{}, NewStoreError("unknown connector status", false)
	}
	s.mu.Lock()
	// No early returns between this Lock and the Unlock below — a future
	// `return` inserted here deadlocks. Keep the single-exit shape:
	// compute everything, copy out, unlock once, then backfill outside
	// the lock. Same copy-out contract as Disconnect (and the reason
	// List backfills outside its RLock): backfillDisplay runs the
	// ScrubError regexes, and a write lock must not span them.
	// Pending attempts die on every disconnecting path, like the tokens
	// and like Disconnect (and the Mongo twin): a browser completing an
	// old consent after this verb must fail at ConsumeState, not silently
	// reconnect without fresh intent.
	if st == StatusDisconnected {
		for state, existing := range s.states {
			if existing.Connector == id {
				delete(s.states, state)
			}
		}
	}
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
	// error matches, no grant to clear — no store touch), repair-only
	// (display/error refresh, no updated_at bump), full transition write.
	// A missing doc always takes the full write.
	// updated_at means last STATUS transition, not last failure reason: an
	// expired error-text change (aged out → revoked) heals the text without
	// moving the timestamp — triage reads failure recency from last_error
	// content, not from updated_at.
	hasGrant := func() bool {
		// Presence-only like the Mongo probe: an over-cap cipher reads as
		// present (repair path), never served — serving happens only
		// through GetTokens/DecryptToken.
		tok, ok := s.tokens[id]
		return ok && (TrimCredential(tok.RefreshCipher) != "" ||
			TrimCredential(tok.AccessCipher) != "" ||
			tok.AccessExpiryUnixMilli != 0)
	}
	var out Connector
	switch {
	case ok && c.Status == st && displayFieldsFresh(id, c.DisplayName, c.Scopes, c.LastError, wantErr) &&
		(st != StatusDisconnected || !hasGrant()):
		// Pure no-op: display fresh, error matches, no grant to clear —
		// no store touch.
		out = c
	case ok && c.Status == st:
		// Repair-only: heal display + error text, leave every timestamp
		// (and, for connected, the stamp) exactly where the last
		// transition put it. Tokens die here too on the disconnecting
		// path — same $unset the Mongo repair carries.
		c.DisplayName = DisplayName(id)
		c.Scopes = ScopesFor(id)
		c.LastError = wantErr
		if st == StatusDisconnected {
			delete(s.tokens, id)
		}
		s.rows[id] = c
		out = c
	default:
		// Same self-heal as the Mongo backend: display fields rewrite from
		// the current constants on every write (transition and no-op
		// alike), so a future scope/name change cannot leave stale values
		// on existing rows. Reads repair per response via backfillDisplay
		// without storing — GETs stay read-only; the next write persists
		// the repair.
		c.DisplayName = DisplayName(id)
		c.Scopes = ScopesFor(id)
		c.Status = st
		c.LastError = wantErr
		c.UpdatedAt = now
		// Tokens die on the disconnecting path here too, not just in
		// Disconnect: SetStatus(disconnected) is the belt-and-braces
		// off-ramp, and leaving a grant behind would keep serving access
		// the audit calls disconnected.
		if st == StatusDisconnected {
			delete(s.tokens, id)
		}
		// First-ever connect, shared with Mongo via resolveConnectedAt
		// (same helper, same semantic on both backends).
		if st == StatusConnected {
			c.ConnectedAt = resolveConnectedAt(c.ConnectedAt, now)
		}
		s.rows[id] = c
		out = c
	}
	s.mu.Unlock()
	return backfillDisplay(out, id), nil
}

func (s *MemoryStore) Disconnect(_ context.Context, id string) (Connector, error) {
	// Length before fold and before Known: ids are two short fixed
	// strings, so anything over the cap is certainly unknown — fail
	// without the normalize alloc or a quoted value in the 404.
	if len(id) > maxConnectorIDLen {
		return Connector{}, NewStoreError("oversized connector id", false)
	}
	id = NormalizedID(id)
	// Controls fail closed like every sibling input: a control-bearing
	// id would otherwise ride the "unknown connector" echo below into
	// the JSON body and the scrubbed log line. Checked post-fold so
	// edge Cf forgiven by TrimCredential still folds instead of 400ing.
	if HasControl(id) || !utf8.ValidString(id) {
		return Connector{}, NewStoreError("invalid connector id", false)
	}
	if !Known(id) {
		return Connector{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	s.mu.Lock()
	// No early returns between this Lock and the Unlock below — a future
	// `return` inserted here deadlocks. Keep the single-exit shape: compute
	// everything, copy out, unlock once, then backfill outside the lock.
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
	//
	// One critical section: the whole read-decide-write happens under a
	// single Lock. Splitting it (unlock to run backfillDisplay, re-lock to
	// write) opens a lost-update window against a concurrent SetStatus —
	// the scrub runs AFTER the unlock, on a copy, instead.
	unchanged := ok && c.Status == StatusDisconnected &&
		displayFieldsFresh(id, c.DisplayName, c.Scopes, c.LastError, "")
	repair := ok && c.Status == StatusDisconnected && !unchanged
	transition := !unchanged && !repair
	if repair {
		c.DisplayName = DisplayName(id)
		c.Scopes = ScopesFor(id)
		c.LastError = ""
		s.rows[id] = c
	}
	if transition {
		c.DisplayName = DisplayName(id)
		c.Scopes = ScopesFor(id)
		c.Status = StatusDisconnected
		c.LastError = ""
		c.UpdatedAt = time.Now().UnixMilli()
		// Token columns cleared at the top of this function (all paths),
		// mirroring the Mongo $unset — see above.
		s.rows[id] = c
	}
	out := c
	s.mu.Unlock()
	return backfillDisplay(out, id), nil
}

// SetTokens stores an encrypted grant. Ciphertext-opaque: the store never
// sees plaintext and enforces only "a stored grant renews" (non-empty
// refresh cipher). Tokens die with the process like every other row here.
func (s *MemoryStore) SetTokens(_ context.Context, id string, tok TokenRow) error {
	// Length before fold (see Get): an oversized id is certainly
	// unknown — fail before the normalize alloc.
	if len(id) > maxConnectorIDLen {
		return NewStoreError("oversized connector id", false)
	}
	id = NormalizedID(id)
	// Controls fail closed like the id verbs above: a control-bearing id
	// would otherwise ride the "unknown connector" echo below.
	if HasControl(id) || !utf8.ValidString(id) {
		return NewStoreError("invalid connector id", false)
	}
	// Length before identity, matching CreateState/ConsumeState/Exchange:
	// an oversized input 400s the same way whether the id is known or
	// not, so the unknown-id path cannot be told apart by which error
	// fires first.
	if len(tok.RefreshCipher) > MaxCipherLen || len(tok.AccessCipher) > MaxCipherLen {
		return NewStoreError("connector grant token too long", false)
	}
	// Controls fail closed like every sibling input: a control-bearing
	// cipher is undecryptable (DecryptToken only fails later, generically),
	// so reject the shape here that the read side cannot name.
	if HasControl(tok.RefreshCipher) || HasControl(tok.AccessCipher) || !utf8.ValidString(tok.RefreshCipher) || !utf8.ValidString(tok.AccessCipher) || HasWhitespace(TrimCredential(tok.RefreshCipher)) || (tok.AccessCipher != "" && HasWhitespace(TrimCredential(tok.AccessCipher))) {
		return NewStoreError("connector grant token is invalid", false)
	}
	// Empty before identity, matching every sibling verb: an empty or
	// blank cipher 400s the same way whether the id is known or not.
	if TrimCredential(tok.RefreshCipher) == "" {
		return NewStoreError("connector grant needs a refresh token", false)
	}
	// Whitespace-only access is neither a sealed grant nor literal "": the
	// refresher keys missing off "", so a padded value would create a
	// second missing representation that reads as present. Reject it here
	// (same gate as refresh) rather than letting it through to storage.
	if tok.AccessCipher != "" && TrimCredential(tok.AccessCipher) == "" {
		return NewStoreError("connector grant access token is blank", false)
	}
	// Padded before identity, like every sibling shape gate: a padded
	// cipher for an unknown id 400s instead of 404ing on identity.
	// Checked == stored — the string the checks see and the bytes the
	// store keeps must be identical, or a direct caller mints an
	// undecryptable "present" grant (same class as the state
	// trim-at-write contract, enforced as a reject here because stored
	// credentials must round-trip byte-exact).
	if tok.RefreshCipher != TrimCredential(tok.RefreshCipher) ||
		(tok.AccessCipher != "" && tok.AccessCipher != TrimCredential(tok.AccessCipher)) {
		return NewStoreError("connector grant token is padded", false)
	}
	if !Known(id) {
		return &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	// Expiry-without-access is a second missing representation: the
	// refresher keys renew-now off zero expiry, and the no-op predicates
	// read a non-zero expiry as grant signal. The gateway zeroes expiry
	// when access is empty (single representation of missing) — the store
	// enforces the same rule instead of relying on the one prod caller.
	if tok.AccessCipher == "" && tok.AccessExpiryUnixMilli != 0 {
		return NewStoreError("connector grant expiry without access token", false)
	}
	// Negative expiries are a third missing representation: UnixMilli is
	// never negative for a real token, and the refresher treats any past
	// value as renew-now — so reject instead of storing a second spelling
	// of "already expired".
	if tok.AccessCipher != "" && tok.AccessExpiryUnixMilli < 0 {
		return NewStoreError("connector grant expiry is negative", false)
	}
	// Clamp absurd futures: a far-future expiry reads as "don't renew for
	// decades". Capped like the state clamp (row still works, renews
	// soon). Memory clock is injectable; mongo is wall-clock by design.
	if max := s.timeNow().Add(MaxAccessLifetime).UnixMilli(); tok.AccessExpiryUnixMilli > max {
		tok.AccessExpiryUnixMilli = max
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[id] = tok
	// Display-default row alongside the tokens, mirroring the Mongo
	// $setOnInsert exactly: insert-only. An existing row keeps its display
	// (even if stale — the next SetStatus heals it, same as Mongo); a
	// crash between SetTokens and SetStatus still reads as explicit-off
	// (updated_at set at insert), not never-touched, on both backends.
	if c, ok := s.rows[id]; !ok || c.ID == "" {
		c := DefaultRow(id)
		// Display-only stamp, wall-clock by design: lifetime ticks use the
		// injectable timeNow above, but updated_at answers "when did a
		// human-visible touch happen" — wall clock, like every other
		// display field. See the timeNow contract, not this line, for
		// expiry-test control.
		c.UpdatedAt = time.Now().UnixMilli()
		s.rows[id] = c
	}
	return nil
}

// GetTokens reads the ciphertext grant. Unknown ids and grant-less rows are
// both NotFound: the caller reconnects either way, and distinguishing them
// would oracle stored-grant existence to any bearer holder.
func (s *MemoryStore) GetTokens(_ context.Context, id string) (TokenRow, error) {
	// Length before fold and before Known: ids are two short fixed
	// strings, so anything over the cap is certainly unknown — fail
	// without the normalize alloc or a quoted value in the 404.
	if len(id) > maxConnectorIDLen {
		return TokenRow{}, NewStoreError("oversized connector id", false)
	}
	id = NormalizedID(id)
	// Controls fail closed like every sibling input: a control-bearing
	// id would otherwise ride the "unknown connector" echo below into
	// the JSON body and the scrubbed log line. Checked post-fold so
	// edge Cf forgiven by TrimCredential still folds instead of 400ing.
	if HasControl(id) || !utf8.ValidString(id) {
		return TokenRow{}, NewStoreError("invalid connector id", false)
	}
	if !Known(id) {
		return TokenRow{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	tok, ok := s.tokens[id]
	if !ok || TrimCredential(tok.RefreshCipher) == "" {
		return TokenRow{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	// Returned verbatim by design: size enforcement lives in DecryptToken
	// (len > MaxCipherLen errors there), so an oversized artifact from
	// before the cap fails at open time with a precise error instead of
	// being masked as NotFound here. Same for expiry freshness: the
	// MaxAccessLifetime clamp is a write-time invariant (gateway + both
	// SetTokens), not re-checked on read — a hand-edited far-future expiry
	// is served as-is until the row is next written.
	return tok, nil
}

// CreateState records a pending OAuth attempt. Zero ExpiresAt defaults to
// now+StateTTL like the Mongo backend (TTL index) — callers pass an
// explicit expiry only for tests; without this, a forgotten expiry field
// would mint an immortal row in a store with no sweeper.
func (s *MemoryStore) CreateState(_ context.Context, st OAuthState) error {
	// Length before fold: an oversized connector is certainly unknown —
	// fail before the normalize alloc (same class as the id verbs).
	if len(st.Connector) > maxConnectorIDLen {
		return NewStoreError("oversized connector id", false)
	}
	st.Connector = NormalizedID(st.Connector)
	// Controls fail closed like the id verbs: a control-bearing connector
	// would otherwise ride the "unknown connector" echo below.
	if HasControl(st.Connector) || !utf8.ValidString(st.Connector) {
		return NewStoreError("invalid connector id", false)
	}
	// Trimmed before storing: the callback trims query params before
	// ConsumeState, so a padded value stored verbatim would never match a
	// lookup — and a padded verifier would break Exchange at Google after
	// the user already consented. Normalise here, not at read time.
	st.State = TrimCredential(st.State)
	st.Verifier = TrimCredential(st.Verifier)
	if st.State == "" {
		return NewStoreError("oauth state needs an id", false)
	}
	// Empty → length → Known, matching Exchange: an empty or oversized
	// input 400s the same way whether the id is known or not, so the
	// unknown-id path cannot be told apart by which error fires first.
	if TrimCredential(st.Verifier) == "" {
		// Fail fast at creation, not after the user completed consent: a
		// verifier-less attempt can never survive Exchange.
		return NewStoreError("oauth state needs a verifier", false)
	}
	if len(st.State) > MaxStateFieldLen || len(st.Verifier) > MaxStateFieldLen {
		return NewStoreError("oauth state or verifier too long", false)
	}
	if HasControl(st.State) || HasControl(st.Verifier) || !utf8.ValidString(st.State) || !utf8.ValidString(st.Verifier) || HasWhitespace(st.State) || HasWhitespace(st.Verifier) {
		return NewStoreError("oauth state or verifier invalid", false)
	}
	if len(st.RedirectURI) > MaxStateFieldLen {
		return NewStoreError("oauth redirect too long", false)
	}
	if !Known(st.Connector) {
		return &StoreError{Msg: "unknown connector " + st.Connector, NotFound: true}
	}
	// Shape-checked like AuthURL/Exchange, not just length-capped: a
	// sub-2048 garbage redirect would otherwise supersede the live
	// attempt and fail only at Exchange — after the user consented.
	// Canonicalised and stored, so stored == sent by construction.
	// Empty means minted before this column existed: allowed, the
	// callback falls back to the live config.
	if st.RedirectURI != "" {
		canonical, err := CanonicalRedirectURL(st.RedirectURI)
		if err != nil {
			return NewStoreError("oauth redirect is invalid", false)
		}
		st.RedirectURI = canonical
		// Checked == stored: canonicalisation can grow the string
		// (IPv6 brackets), so the raw length gate above is not enough —
		// a just-under-cap raw could land just-over-cap canonical.
		if len(st.RedirectURI) > MaxStateFieldLen {
			return NewStoreError("oauth redirect too long", false)
		}
	}
	// Single clock read for the whole verb: default, clamp bound, and
	// past-expiry check must agree on "now", or a tick between reads
	// could mint-then-kill (or keep-then-reap) the same row.
	now := s.timeNow()
	if st.ExpiresAt.IsZero() {
		st.ExpiresAt = now.Add(StateTTL)
	}
	// Clamp absurd futures: a one-year oauth_states row is never a
	// legitimate pending attempt, and the TTL backstop would honour it.
	// Capped, not rejected — the row still works, just not forever.
	if exp := now.Add(StateTTL); st.ExpiresAt.After(exp) {
		st.ExpiresAt = exp
	}
	// Creation and ConsumeState agree on the boundary tick: == now counts
	// as expired on both sides (!After here, !Before there), so a direct
	// caller minting == now cannot supersede the live row with a stillborn
	// one the next callback instantly rejects as dead.
	if !st.ExpiresAt.After(now) {
		return NewStoreError("oauth state already expired", false)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Opportunistic sweep: no TTL thread exists here, so reap expired rows
	// on the write path that creates them. Keeps an abandoned-connect spam
	// from growing the map without bound; same lazy-expiry contract as the
	// Mongo TTL, enforced at write time instead of by the database.
	//
	// Single pass, single critical section, by structural necessity — not
	// performance: the sweep must run before the duplicate check (an
	// expired row with the same state id is reapable, not a collision),
	// and the supersede must run after it (reusing a state string must
	// 400 like the Mongo duplicate-key path, not silently overwrite
	// after deleting the row it collides with). Collect-then-mutate
	// keeps that ordering while unlocking only once.
	var reap, supersede []string
	for state, existing := range s.states {
		// Reap predicate matches the consume rule: zero counts as
		// expired on read, so it must reap on sweep too — otherwise a
		// hand-inserted zero row is dead-on-read yet immortal.
		if existing.ExpiresAt.IsZero() || !now.Before(existing.ExpiresAt) {
			reap = append(reap, state)
		} else if existing.Connector == st.Connector {
			supersede = append(supersede, state)
		}
	}
	for _, state := range reap {
		delete(s.states, state)
	}
	// Duplicate check AFTER the sweep above: reusing the same state
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
	for _, state := range supersede {
		delete(s.states, state)
	}
	s.states[st.State] = st
	return nil
}

// ConsumeState returns and deletes a pending attempt. Unknown and expired
// states are the same NotFound: expiry is lazy (no sweeper, and only two
// connectors mint states), and the caller message is "try connect again"
// either way.
func (s *MemoryStore) ConsumeState(_ context.Context, state string) (OAuthState, error) {
	// Trimmed like the write path and the gateway: padded input resolves
	// instead of 404ing on whitespace no writer could have stored.
	// Length-gated like the gateway: oversized input answers the same
	// NotFound as unknown, never reaching the map. Control-gated like
	// CreateState: a control-bearing id must not reach even a map lookup
	// as anything but NotFound.
	state = TrimCredential(state)
	if len(state) > MaxStateFieldLen {
		return OAuthState{}, &StoreError{Msg: "unknown or expired oauth state", NotFound: true}
	}
	if HasControl(state) || !utf8.ValidString(state) || HasWhitespace(state) {
		return OAuthState{}, &StoreError{Msg: "unknown or expired oauth state", NotFound: true}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.states[state]
	if !ok {
		return OAuthState{}, &StoreError{Msg: "unknown or expired oauth state", NotFound: true}
	}
	delete(s.states, state)
	// Boundary: expiresAt itself counts as expired (matches the Mongo TTL,
	// which reaps at expiry, not after). Zero counts as expired too: no
	// API writer can mint one (CreateState defaults zero), so a zero row
	// is a hand-edit — fail closed, matching the Mongo TTL which reaps a
	// zero date at once. Clock is injectable so tests pin this tick
	// without sleeping.
	if !s.timeNow().Before(st.ExpiresAt) {
		return OAuthState{}, &StoreError{Msg: "unknown or expired oauth state", NotFound: true}
	}
	return st, nil
}
