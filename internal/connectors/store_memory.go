package connectors

import (
	"context"
	"strings"
	"sync"
	"time"
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
	// ids are NotFound.
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
	out := make([]Connector, 0, len(ListIDs))
	for _, id := range ListIDs {
		if c, ok := s.rows[id]; ok {
			out = append(out, c)
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
		return c, nil
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
	c.Status = st
	c.LastError = strings.TrimSpace(lastError)
	c.UpdatedAt = now
	if st == StatusConnected {
		c.ConnectedAt = now
	}
	s.rows[id] = c
	return c, nil
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
	c.Status = StatusDisconnected
	c.LastError = ""
	c.UpdatedAt = time.Now().UnixMilli()
	// No token field in this shape to clear; the Mongo backend clears its
	// token columns here (see refreshTokenField/accessTokenField). Documented
	// so the two backends cannot diverge silently: a future field must be
	// cleared in both.
	s.rows[id] = c
	return c, nil
}
