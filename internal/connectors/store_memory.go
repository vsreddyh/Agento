package connectors

import (
	"context"
	"slices"
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
}

// MemoryStore is the in-process backend: rows die with the process, like
// every other memory fallback. The gateway still serves the routes, which
// keeps a development stack usable without pretending tokens survive.
type MemoryStore struct {
	mu   sync.RWMutex
	rows map[string]Connector
}

// NewMemoryStore builds a store with no rows (List synthesises disconnected).
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{rows: make(map[string]Connector)}
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Connector, 0, len(listIDs))
	for _, id := range listIDs {
		if c, ok := s.rows[id]; ok {
			out = append(out, backfillDisplay(c, id))
		} else {
			out = append(out, DefaultRow(id))
		}
	}
	return out, nil
}

func (s *MemoryStore) Get(_ context.Context, id string) (Connector, error) {
	id = NormalizedID(id)
	if !Known(id) {
		return Connector{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if c, ok := s.rows[id]; ok {
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
	if ok && c.Status == st && c.LastError == wantErr &&
		c.DisplayName == DisplayName(id) && slices.Equal(c.Scopes, ScopesFor(id)) {
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
		c.LastError == "" && c.DisplayName == DisplayName(id) &&
		slices.Equal(c.Scopes, ScopesFor(id)) {
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
	// No token field in this shape to clear; the Mongo backend clears its
	// token columns here (see refreshTokenField/accessTokenField). Documented
	// so the two backends cannot diverge silently: a future field must be
	// cleared in both.
	s.rows[id] = c
	return backfillDisplay(c, id), nil
}
