package teach

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// MemoryStore is the in-process backend used when no Mongo URI is
// configured. Like every other fallback here it is honest about being
// local: items die with the process, and the materialized files are the
// only thing Pi ever reads.
type MemoryStore struct {
	mu     sync.RWMutex
	skills map[string]Skill
	// now is the clock every write boundary reads, mirroring MongoStore so
	// time-dependent behavior pins on both backends. Nil means time.Now.
	now func() time.Time
}

func (s *MemoryStore) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// NewMemoryStore builds an empty store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{skills: make(map[string]Skill)}
}

func cleanCreate(in Skill, now int64) (Skill, error) {
	in.Profile = strings.TrimSpace(in.Profile)
	if in.Profile == "" {
		return Skill{}, NewStoreError("skill needs a profile", false)
	}
	in.Name = strings.TrimSpace(in.Name)
	if !ValidName(in.Name) {
		return Skill{}, NewStoreError("skill name must be lowercase letters, digits and hyphens", false)
	}
	in.Description = CleanDescription(in.Description)
	if in.Description == "" {
		return Skill{}, NewStoreError("skill needs a description", false)
	}
	in.Body = CleanBody(in.Body)
	if in.Body == "" {
		return Skill{}, NewStoreError("skill needs a body", false)
	}
	// Source is client-supplied free text like the rest: trim and clip
	// instead of riding unbounded.
	in.Source = clipRunes(strings.TrimSpace(cleanFree(in.Source)), MaxSource)
	// CreatedAt is always stamped: a client-supplied past instant would
	// backdate sort order, and nothing legitimate sets it.
	in.CreatedAt = now
	in.UpdatedAt = now
	// Drafts are born drafts: only review + test promote, never the create
	// call itself.
	in.Status = StatusDraft
	in.TestedAt = 0
	in.TestText = ""
	return in, nil
}

// Create stores one draft. The status is always draft: activation is a
// separate, tested step. Validation runs before duplicate checks (same order
// as Mongo): a request that is both malformed and colliding fails as
// malformed on either backend.
func (s *MemoryStore) Create(_ context.Context, in Skill) (Skill, error) {
	if in.ID != "" && !ValidID(in.ID) {
		return Skill{}, NewStoreError("skill id must be 1-64 letters, digits, _ or -", false)
	}
	in, err := cleanCreate(in, s.clock().UnixMilli())
	if err != nil {
		return Skill{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if in.ID == "" {
		in.ID = NewID()
	}
	if _, exists := s.skills[in.ID]; exists {
		return Skill{}, NewStoreError("skill id already exists", false)
	}
	for _, it := range s.skills {
		if it.Profile == in.Profile && it.Name == in.Name {
			return Skill{}, NewStoreError("a skill named "+in.Name+" already exists", false)
		}
	}
	s.skills[in.ID] = in
	return in, nil
}

// Get returns one skill. Profile-scoped: a foreign id is NotFound.
func (s *MemoryStore) Get(_ context.Context, id, profile string) (Skill, error) {
	profile = strings.TrimSpace(profile)
	s.mu.RLock()
	defer s.mu.RUnlock()
	it, ok := s.skills[id]
	if !ok || it.Profile != profile {
		return Skill{}, &StoreError{Msg: "unknown skill " + id, NotFound: true}
	}
	return it, nil
}

// Update applies a partial edit. A rename collides like a create.
func (s *MemoryStore) Update(_ context.Context, id, profile string, u Update) (Skill, error) {
	profile = strings.TrimSpace(profile)
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.skills[id]
	if !ok || it.Profile != profile {
		return Skill{}, &StoreError{Msg: "unknown skill " + id, NotFound: true}
	}
	if u.Name == nil && u.Description == nil && u.Body == nil {
		return Skill{}, NewStoreError("skill update needs name, description or body", false)
	}
	if u.Name != nil {
		name := strings.TrimSpace(*u.Name)
		if !ValidName(name) {
			return Skill{}, NewStoreError("skill name must be lowercase letters, digits and hyphens", false)
		}
		for otherID, other := range s.skills {
			if otherID != id && other.Profile == it.Profile && other.Name == name {
				return Skill{}, NewStoreError("a skill named "+name+" already exists", false)
			}
		}
		it.Name = name
	}
	if u.Description != nil {
		d := CleanDescription(*u.Description)
		if d == "" {
			return Skill{}, NewStoreError("skill needs a description", false)
		}
		it.Description = d
	}
	if u.Body != nil {
		b := CleanBody(*u.Body)
		if b == "" {
			return Skill{}, NewStoreError("skill needs a body", false)
		}
		it.Body = b
	}
	it.UpdatedAt = s.clock().UnixMilli()
	s.skills[id] = it
	return it, nil
}

// Delete removes one skill. The materializer drops its file on the next
// reconcile; there is no un-delete.
func (s *MemoryStore) Delete(_ context.Context, id, profile string) error {
	profile = strings.TrimSpace(profile)
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.skills[id]
	if !ok || it.Profile != profile {
		return &StoreError{Msg: "unknown skill " + id, NotFound: true}
	}
	delete(s.skills, id)
	return nil
}

// List returns one profile's skills, newest first.
func (s *MemoryStore) List(_ context.Context, profile string) ([]Skill, error) {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return nil, NewStoreError("skill list needs a profile", false)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Skill, 0, 8)
	for _, it := range s.skills {
		if it.Profile == profile {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].CreatedAt != out[b].CreatedAt {
			return out[a].CreatedAt > out[b].CreatedAt
		}
		return out[a].ID > out[b].ID
	})
	// Same cap as Mongo: the Store contract is one contract, even though a
	// hundred taught skills per profile is nowhere in sight.
	if len(out) > ListCap {
		out = out[:ListCap]
	}
	return out, nil
}

// SetStatus moves one skill along its lifecycle.
func (s *MemoryStore) SetStatus(_ context.Context, id, profile string, st Status) (Skill, error) {
	profile = strings.TrimSpace(profile)
	if !st.Valid() {
		return Skill{}, NewStoreError("unknown skill status", false)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.skills[id]
	if !ok || it.Profile != profile {
		return Skill{}, &StoreError{Msg: "unknown skill " + id, NotFound: true}
	}
	if st == StatusActive && it.TestedAt == 0 {
		return Skill{}, NewStoreError("test the skill before activating it", false)
	}
	it.Status = st
	it.UpdatedAt = s.clock().UnixMilli()
	s.skills[id] = it
	return it, nil
}

// RecordTest stores a test transcript and stamps the skill tested.
func (s *MemoryStore) RecordTest(_ context.Context, id, profile string, at int64, text string) (Skill, error) {
	profile = strings.TrimSpace(profile)
	s.mu.Lock()
	defer s.mu.Unlock()
	it, ok := s.skills[id]
	if !ok || it.Profile != profile {
		return Skill{}, &StoreError{Msg: "unknown skill " + id, NotFound: true}
	}
	it.TestedAt = at
	it.TestText = clipRunes(strings.TrimSpace(strings.ReplaceAll(text, "\x00", "")), MaxTestText)
	it.UpdatedAt = s.clock().UnixMilli()
	s.skills[id] = it
	return it, nil
}
