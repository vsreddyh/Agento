package approvals

import (
	"context"
	"sort"
	"sync"
	"time"
)

// MemoryStore is the fallback when MongoDB is unavailable, and what the
// relay and gateway tests use. Same contract as the Mongo backend, minus
// durability: expiry is enforced lazily per read instead of by a TTL monitor.
type MemoryStore struct {
	mu        sync.Mutex
	approvals map[string]Approval
	audit     map[string]AuditEntry
	now       func() time.Time
	// Logf is optional and nil-silent. It exists so a duplicate create reads
	// the same in triage per backend; ids are ObjectIDs, so this is a retry,
	// not a collision.
	Logf func(string, ...any)
}

func (m *MemoryStore) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf(format, args...)
	}
}

// Shared reports false: this store lives in one process only.
func (m *MemoryStore) Shared() bool { return false }

// NewMemoryStore builds empty stores.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		approvals: map[string]Approval{},
		audit:     map[string]AuditEntry{},
		now:       time.Now,
	}
}

func expired(at time.Time, expires time.Time) bool {
	return !expires.IsZero() && !expires.After(at)
}

// CreateApproval stores a pending approval, stamping times when unset.
func (m *MemoryStore) CreateApproval(_ context.Context, a Approval) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if a.ID == "" {
		return NewStoreError("approval create needs an id", false)
	}
	if _, exists := m.approvals[a.ID]; exists {
		m.logf("approvals: ignored duplicate create for %s", a.ID)
		return nil
	}
	// Redact in the store, not only at call sites: redaction is idempotent, and
	// one future caller forgetting it must not write a secret for 90 days.
	a.Args = Redact(a.Args)
	now := m.now()
	if a.CreatedAt == 0 {
		a.CreatedAt = now.UnixMilli()
	}
	a.UpdatedAt = now.UnixMilli()
	if a.Status == "" {
		a.Status = StatusPending
	}
	if a.ExpiresAt.IsZero() {
		a.ExpiresAt = now.Add(ApprovalTTL)
	}
	m.approvals[a.ID] = a.Clone()
	return nil
}

// GetApproval returns one approval; expired rows read as NotFound.
func (m *MemoryStore) GetApproval(_ context.Context, id string) (Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok {
		return Approval{}, &StoreError{Msg: "unknown approval " + id, NotFound: true}
	}
	if expired(m.now(), a.ExpiresAt) {
		delete(m.approvals, id)
		return Approval{}, &StoreError{Msg: "unknown approval " + id, NotFound: true}
	}
	return a.Clone(), nil
}

// DecideApproval flips a pending approval terminal exactly once.
func (m *MemoryStore) DecideApproval(_ context.Context, id string, approved bool, by string, at time.Time) (Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok {
		return Approval{}, &StoreError{Msg: "unknown approval " + id, NotFound: true}
	}
	if expired(at, a.ExpiresAt) {
		delete(m.approvals, id)
		return Approval{}, &StoreError{Msg: "approval " + id + " expired", Conflict: true}
	}
	if a.Status.Terminal() {
		return Approval{}, &StoreError{Msg: "approval " + id + " already decided", Conflict: true}
	}
	if approved {
		a.Status = StatusApproved
	} else {
		a.Status = StatusDenied
	}
	a.DecidedBy = by
	a.DecidedAt = at.UnixMilli()
	a.UpdatedAt = at.UnixMilli()
	m.approvals[id] = a
	return a.Clone(), nil
}

// TimeoutApproval flips a still-pending approval expired; decided rows are
// left alone so a human decision always wins over the timer.
func (m *MemoryStore) TimeoutApproval(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.approvals[id]
	if !ok || a.Status.Terminal() {
		return nil
	}
	a.Status = StatusExpired
	a.UpdatedAt = at.UnixMilli()
	a.DecidedAt = at.UnixMilli()
	m.approvals[id] = a
	return nil
}

// ListApprovals returns matching approvals, newest first, capped. Expired
// rows are dropped lazily, like Get.
func (m *MemoryStore) ListApprovals(_ context.Context, f ApprovalFilter) ([]Approval, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	limit := f.Limit
	if limit <= 0 || limit > ListCap {
		limit = ListCap
	}
	now := m.now()
	out := make([]Approval, 0, len(m.approvals))
	for id, a := range m.approvals {
		if expired(now, a.ExpiresAt) {
			delete(m.approvals, id)
			continue
		}
		if f.Status != "" && a.Status != f.Status {
			continue
		}
		out = append(out, a.Clone())
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

// Audit appends one audit row.
func (m *MemoryStore) Audit(_ context.Context, e AuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.ID == "" {
		return NewStoreError("audit needs an id", false)
	}
	// Same 90-day contract as Mongo: the cap below is a memory bound, not the
	// retention policy.
	// One clock stamps both: otherwise TTL is storeClock+90d minus a caller's
	// At, not exactly the window this store promises.
	now := m.now()
	if e.At == 0 {
		e.At = now.UnixMilli()
	}
	if e.ExpiresAt.IsZero() {
		e.ExpiresAt = now.Add(AuditTTL)
	}
	e.Args = Redact(e.Args)
	m.audit[e.ID] = e
	// Memory is bounded like the runs fallback: oldest-first past cap.
	// The scan is O(n) per insert once full (n = 10*ListCap). This fallback is
	// for tests and the brief window before Mongo is reachable, not the
	// sustained path; a heap would be more code for a store that is not meant
	// to carry production traffic.
	for len(m.audit) > 10*ListCap {
		oldest, oldestAt := "", int64(0)
		first := true
		for id, o := range m.audit {
			if first || o.At < oldestAt || (o.At == oldestAt && id < oldest) {
				oldest, oldestAt, first = id, o.At, false
			}
		}
		if oldest == "" {
			break
		}
		delete(m.audit, oldest)
	}
	return nil
}

// ListAudit returns matching rows, newest first, capped.
func (m *MemoryStore) ListAudit(_ context.Context, f AuditFilter) ([]AuditEntry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	limit := f.Limit
	if limit <= 0 || limit > ListCap {
		limit = ListCap
	}
	now := m.now()
	out := make([]AuditEntry, 0, len(m.audit))
	for id, e := range m.audit {
		// Lazy drop, like approvals: a long-lived fallback must not keep rows
		// its own list refuses to show.
		if expired(now, e.ExpiresAt) {
			delete(m.audit, id)
			continue
		}
		if f.Server != "" && e.Server != f.Server {
			continue
		}
		if f.Tool != "" && e.Tool != f.Tool {
			continue
		}
		out = append(out, e.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].At != out[j].At {
			return out[i].At > out[j].At
		}
		// Same-millis tie-break, matching Mongo's {at,-1,_id,-1}: otherwise two
		// identical requests can return the rows in different orders.
		return out[i].ID > out[j].ID
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// MarkInterrupted expires pending rows that predate the restart: a restart
// orphans the relay that owned their wait, and a "pending" row nobody reads
// would lie until TTL. Rows created at/after `at` belong to this process's
// startup (the single-process case makes that rare, but the scope mirrors
// Mongo so a Create landing mid-sweep is not expired by a dead owner).
func (m *MemoryStore) MarkInterrupted(_ context.Context, at time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for id, a := range m.approvals {
		if a.Status != StatusPending || a.CreatedAt >= at.UnixMilli() {
			continue
		}
		if expired(at, a.ExpiresAt) {
			delete(m.approvals, id)
			continue
		}
		a.Status = StatusExpired
		a.UpdatedAt = at.UnixMilli()
		a.DecidedAt = at.UnixMilli()
		m.approvals[id] = a
		n++
	}
	return n, nil
}
