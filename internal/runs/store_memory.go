package runs

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryStore is the fallback when MongoDB is unavailable and the store tests
// use. It keeps the same contract as the Mongo one, minus durability across
// restarts.
//
// Why a real store rather than a nil check: a gateway without Mongo still has
// to answer /api/runs and still has to keep a turn alive after the request
// that started it, because that property does not depend on persistence. Two
// code paths would make the Mongo-less one the untested one.
type MemoryStore struct {
	mu   sync.Mutex
	byID map[string]Run
	now  func() time.Time
}

// NewMemoryStore builds an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{byID: map[string]Run{}, now: time.Now}
}

// Create stores a copy of r.
func (m *MemoryStore) Create(_ context.Context, r Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.ID == "" {
		r.ID = NewID()
	}
	r = r.Capped()
	m.byID[r.ID] = r
	return nil
}

// Get returns one run.
func (m *MemoryStore) Get(_ context.Context, id string) (Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[id]
	if !ok {
		return Run{}, &StoreError{Msg: "unknown run " + id, NotFound: true}
	}
	return r.Clone(), nil
}

// Save replaces an existing run, or inserts it if it is missing. An empty ID
// is refused rather than stored under "": a Save names a run that Create (or
// NewID) already identified, and inventing one here would fork the record on
// every write.
func (m *MemoryStore) Save(_ context.Context, r Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.ID == "" {
		return NewStoreError("run save needs an id", false)
	}
	m.byID[r.ID] = r.Capped()
	return nil
}

// List returns matching runs, newest first, capped. An empty result is an
// empty slice, never nil: the JSON shape is `{"runs":[]}`, not `null`.
func (m *MemoryStore) List(_ context.Context, f Filter) ([]Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	limit := f.Limit
	if limit <= 0 || limit > ListCap {
		limit = ListCap
	}
	out := make([]Run, 0, len(m.byID))
	for _, r := range m.byID {
		if f.Profile != "" && r.Profile != f.Profile {
			continue
		}
		if f.ConversationID != "" && r.ConversationID != f.ConversationID {
			continue
		}
		if f.Status != "" && r.Status != f.Status {
			continue
		}
		out = append(out, r.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt == out[j].CreatedAt {
			return out[i].ID > out[j].ID
		}
		return out[i].CreatedAt > out[j].CreatedAt
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// MarkInterrupted flips every running record, reporting how many changed.
func (m *MemoryStore) MarkInterrupted(_ context.Context, at time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	now := at.UnixMilli()
	for id, r := range m.byID {
		if r.Status == StatusRunning {
			r.Status = StatusInterrupted
			r.Error = "gateway restarted while this run was in flight"
			r.UpdatedAt = now
			r.FinishedAt = now
			r.ExpiresAt = at.Add(DefaultTTL)
			m.byID[id] = r
			n++
		}
	}
	return n, nil
}
