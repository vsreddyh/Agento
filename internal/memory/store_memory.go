package memory

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryStore is the in-process backend used when no Mongo URI is
// configured. It is honest about being local:
//
//   - items live and die with the process, so a restart forgets everything
//     between the app's write and the next read;
//   - the memory MCP server is a different process and cannot see this store
//     at all, so agent-proposed items only work with Mongo.
//
// The gateway still serves the routes and injects what it has, which keeps a
// development stack usable without pretending it is durable.
type MemoryStore struct {
	// RWMutex: List/Get/PausedTopics are the read-heavy paths (every turn,
	// every screen load) and take the read lock; writes take it exclusive.
	mu     sync.RWMutex
	items  map[string]Item
	paused map[string]map[string]bool
}

// NewMemoryStore builds an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		items:  make(map[string]Item),
		paused: make(map[string]map[string]bool),
	}
}

// Create stores one item. Text is cleaned and empty text rejected here, not
// only at the callers: MaxText is a store invariant, and a future caller that
// forgets CleanText must not be able to store a 10KB item or "".
func (s *MemoryStore) Create(_ context.Context, i Item) (Item, error) {
	// Trimmed on write so " chat " cannot fork an isolated namespace that
	// reads under "chat" 404; the check and the stored value must agree.
	i.Profile = strings.TrimSpace(i.Profile)
	if i.Profile == "" {
		return Item{}, NewStoreError("memory item needs a profile", false)
	}
	i.Text = CleanText(i.Text)
	if i.Text == "" {
		return Item{}, NewStoreError("memory item needs text", false)
	}
	// Same 200-item enforcement as Mongo: the 201st create fails instead
	// of writing a row neither the screen nor the prompt can reach.
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for _, it := range s.items {
		if it.Profile == i.Profile {
			n++
		}
	}
	if n >= ListCap {
		return Item{}, NewStoreError("memory is full for this profile: delete or forget items first", false)
	}
	if i.ID == "" {
		i.ID = NewID()
	}
	// A caller-supplied ID that already exists is rejected, not overwritten:
	// Mongo fails the same insert on duplicate key, and silently re-homing
	// another profile's row would be the worse mismatch.
	if _, exists := s.items[i.ID]; exists {
		return Item{}, NewStoreError("memory id already exists", false)
	}
	now := time.Now().UnixMilli()
	if i.CreatedAt == 0 {
		i.CreatedAt = now
	}
	i.UpdatedAt = now
	i.Topic = NormalizeTopic(i.Topic)
	// An unknown status is a caller bug, not pending: Update and List reject
	// it, and Create must not be the lenient path.
	if !i.Status.Valid() {
		return Item{}, NewStoreError("unknown memory status", false)
	}
	// Source stays display-only passthrough (see its declaration): anything
	// but a known writer label stores verbatim, because status — not source
	// — is the trust boundary. Empty means the app wrote it.
	if i.Source == "" {
		i.Source = SourceUser
	}
	s.items[i.ID] = i
	return i, nil
}

// Get returns one item. Profile-scoped: a foreign id is NotFound.
func (s *MemoryStore) Get(_ context.Context, id, profile string) (Item, error) {
	profile = strings.TrimSpace(profile)
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := s.items[id]
	if !ok || it.Profile != profile {
		return Item{}, &StoreError{Msg: "unknown memory " + id, NotFound: true}
	}
	return it, nil
}

// Update applies a partial edit. A foreign profile's id is NotFound, the
// same 404 the gateway returns for a missing id.
//
// UpdatedAt means last write, not last change: a same-value edit still bumps
// it. Sorting is by CreatedAt, so the bump never re-floats an item.

func (s *MemoryStore) Update(_ context.Context, id, profile string, u Update) (Item, error) {
	profile = strings.TrimSpace(profile)
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok || it.Profile != profile {
		return Item{}, &StoreError{Msg: "unknown memory " + id, NotFound: true}
	}
	// A no-op edit is rejected, not timestamped: UpdatedAt must mean
	// something changed, and the gateway 400s {} first, but the store is the
	// backstop for direct callers.
	if u.Text == nil && u.Topic == nil && u.Status == nil {
		return Item{}, NewStoreError("memory update needs text, topic or status", false)
	}
	if u.Text != nil {
		// Same invariant as Create: an edit must not blank an item. The
		// gateway 400s this first, but the store is the backstop.
		text := CleanText(*u.Text)
		if text == "" {
			return Item{}, NewStoreError("memory item needs text", false)
		}
		it.Text = text
	}
	if u.Topic != nil {
		it.Topic = NormalizeTopic(*u.Topic)
	}
	if u.Status != nil {
		if !u.Status.Valid() {
			return Item{}, NewStoreError("unknown memory status", false)
		}
		it.Status = *u.Status
	}
	it.UpdatedAt = time.Now().UnixMilli()
	s.items[id] = it
	return it, nil
}

// Delete removes one item. Profile-scoped like Update.
func (s *MemoryStore) Delete(_ context.Context, id, profile string) error {
	profile = strings.TrimSpace(profile)
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.items[id]
	if !ok || it.Profile != profile {
		return &StoreError{Msg: "unknown memory " + id, NotFound: true}
	}
	delete(s.items, id)
	return nil
}

// DeleteAll forgets one profile's items, keeping its paused topics.
func (s *MemoryStore) DeleteAll(_ context.Context, profile string) (int64, error) {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return 0, NewStoreError("memory forget needs a profile", false)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int64
	for id, it := range s.items {
		if it.Profile == profile {
			delete(s.items, id)
			n++
		}
	}
	return n, nil
}

// List returns matching items, newest-created first. Creation order is
// stable on purpose: an edit bumps UpdatedAt but does not re-float the item,
// so the screen and the prompt see the same standing order.
func (s *MemoryStore) List(_ context.Context, f Filter) ([]Item, error) {
	f.Profile = strings.TrimSpace(f.Profile)
	// Empty means nothing here, not "all profiles": every other method is
	// profile-scoped or rejects "", and the one read that fans out across
	// profiles would break the per-profile contract.
	if f.Profile == "" {
		return nil, NewStoreError("memory list needs a profile", false)
	}
	if f.Status != "" && !f.Status.Valid() {
		return nil, NewStoreError("unknown memory status", false)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	// 0 is the Go zero value for "unspecified", not "no rows".
	limit := f.Limit
	if limit <= 0 || limit > ListCap {
		limit = ListCap
	}
	out := make([]Item, 0, 16)
	for _, it := range s.items {
		// f.Profile is trimmed and non-empty (rejected above); it.Profile
		// was trimmed on write. Direct comparison, no re-trim.
		if it.Profile != f.Profile {
			continue
		}
		if t := strings.TrimSpace(f.Topic); t != "" && it.Topic != NormalizeTopic(t) {
			continue
		}
		if f.Status != "" && it.Status != f.Status {
			continue
		}
		out = append(out, it)
	}
	// Newest first; the id tiebreak keeps equal-millisecond writes stable so
	// a list does not reshuffle between two identical requests.
	sort.Slice(out, func(a, b int) bool {
		if out[a].CreatedAt != out[b].CreatedAt {
			return out[a].CreatedAt > out[b].CreatedAt
		}
		return out[a].ID > out[b].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SetTopicPaused pauses or resumes one topic.
func (s *MemoryStore) SetTopicPaused(_ context.Context, profile, topic string, paused bool) error {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return NewStoreError("memory topic needs a profile", false)
	}
	t := NormalizeTopic(topic)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paused[profile] == nil {
		s.paused[profile] = make(map[string]bool)
	}
	if paused {
		s.paused[profile][t] = true
	} else {
		delete(s.paused[profile], t)
	}
	return nil
}

// PausedTopics returns the paused topics for a profile. Never nil: the app
// screen reads paused as a list, and null is not a list.
func (s *MemoryStore) PausedTopics(_ context.Context, profile string) ([]string, error) {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return nil, NewStoreError("memory topics need a profile", false)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []string{}
	for t := range s.paused[profile] {
		out = append(out, t)
	}
	sort.Strings(out)
	return out, nil
}
