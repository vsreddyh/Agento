// Package runs stores conversation-attached agent runs: one turn of work that
// outlives the HTTP request that started it.
//
// This is deliberately a SIBLING of cronjobs, not a reuse of it. Cron jobs are
// fire-and-forget, identified by schedule and prompt, and carry a short capped
// history of replies. A run is a single execution with a lifecycle the client
// can observe, reattach to, steer and cancel; its output is the turn itself,
// not a notification about one. Sharing the cron collection would mean one
// document shape doing two jobs, and /api/jobs would grow a second meaning.
//
// State lives in MongoDB (see store_mongo.go) so a run survives the app being
// killed, a network drop, and a gateway restart (with the caveat recorded in
// docs/adr/0001-resumable-runs.md: a restart cannot resume execution, only the
// record). The in-memory store exists so tests and a Mongo-less gateway still
// have one code path, not a second behaviour.
package runs

import (
	"context"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	// Collection is the MongoDB collection. The "_" prefix is not used: this
	// is durable domain data, not runtime scratch.
	Collection = "agent_runs"

	// DefaultTTL is how long a finished run's record is kept. Seven days is
	// long enough to reopen the app the next morning and see what happened,
	// short enough that stored turn text does not become an unmanaged second
	// transcript archive.
	DefaultTTL = 7 * 24 * time.Hour

	// ListCap bounds one list response.
	ListCap = 100

	// ListPreviewOutput bounds the per-run output shown by the LIST route.
	// A full list at ListCap with 256 KiB turns would be tens of megabytes in
	// one response; the list shows what each turn was about, and the detail
	// route carries the whole text.
	ListPreviewOutput = 2000

	// ListPreviewReasoning is the same preview bound for reasoning traces.
	ListPreviewReasoning = 1000

	// MaxSteer bounds one steer message. Same value as MaxPrompt today — a
	// steer is one instruction, not a conversation — but a separate name so
	// tuning the prompt preview never silently moves the steer limit.
	MaxSteer = MaxPrompt

	// MaxOutput bounds the output kept per run — persisted and live. The phone
	// renders a turn, not a log; a runaway tool loop that produced megabytes
	// must not become a megabyte Mongo document written once a second, held
	// in the builder for the whole turn, or served from GET /api/runs/{id}.
	// Past the cap the live snapshot carries the head while live deltas keep
	// flowing in full; the stored record keeps the same head.
	MaxOutput = 256 << 10

	// MaxReasoning bounds the stored reasoning trace. The app already caps
	// its own display at REASONING_CAP (20k); this is headroom, not a
	// duplicate limit.
	MaxReasoning = 64 << 10

	// MaxPrompt bounds the persisted prompt preview. The run list shows what
	// the turn was about, not a second copy of the conversation.
	MaxPrompt = 4000

	// MaxTools bounds the distinct tool names recorded for a run.
	MaxTools = 32
)

// Status is a run's lifecycle state.
type Status string

const (
	// StatusRunning means an agent turn is executing in this process.
	StatusRunning Status = "running"
	// StatusCompleted means the agent settled normally.
	StatusCompleted Status = "completed"
	// StatusFailed means the turn ended with an error.
	StatusFailed Status = "failed"
	// StatusCancelled means the user (or a client) cancelled it.
	StatusCancelled Status = "cancelled"
	// StatusInterrupted means the gateway restarted while it was running.
	// The record survives; the execution does not.
	StatusInterrupted Status = "interrupted"
)

// InterruptedError is the error recorded on runs in flight across a gateway
// restart, and the message of the buffered 409 that reports one. One string
// in both places so nobody branches on free text: the app tells states apart
// by status / param run_status, and both wordings below stay stable.
const InterruptedError = "the gateway restarted while this run was in flight; the partial output is preserved"

// Terminal reports whether no further execution will happen.
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted:
		return true
	}
	return false
}

// Valid reports whether s is a status this package knows.
func (s Status) Valid() bool {
	switch s {
	case StatusRunning, StatusCompleted, StatusFailed, StatusCancelled, StatusInterrupted:
		return true
	}
	return false
}

// Usage is the token accounting for one run, in the shape the app already
// reads from the chat stream (`prompt_tokens` et al).
type Usage struct {
	Prompt     int64 `bson:"prompt" json:"prompt"`
	Completion int64 `bson:"completion" json:"completion"`
	Total      int64 `bson:"total" json:"total"`
	Cached     int64 `bson:"cached" json:"cached"`
	CacheWrite int64 `bson:"cache_write" json:"cache_write"`
}

// NonZero reports whether any count is set. An all-zero Usage is omitted from
// JSON rather than reported as a zero-token turn.
func (u Usage) NonZero() bool {
	return u.Prompt != 0 || u.Completion != 0 || u.Total != 0 ||
		u.Cached != 0 || u.CacheWrite != 0
}

// Run is one conversation-attached turn.
//
// Output is the authoritative accumulated assistant text, which is what makes
// reattaching possible: a client that lost its own buffer replaces it with
// this, then follows live deltas.
type Run struct {
	ID             string   `bson:"_id" json:"id"`
	Profile        string   `bson:"profile" json:"profile"`
	ConversationID string   `bson:"conversationId,omitempty" json:"conversation_id,omitempty"`
	Prompt         string   `bson:"prompt" json:"prompt,omitempty"`
	Provider       string   `bson:"provider,omitempty" json:"provider,omitempty"`
	Model          string   `bson:"model,omitempty" json:"model,omitempty"`
	Effort         string   `bson:"effort,omitempty" json:"effort,omitempty"`
	Status         Status   `bson:"status" json:"status"`
	Output         string   `bson:"output,omitempty" json:"output,omitempty"`
	Reasoning      string   `bson:"reasoning,omitempty" json:"reasoning,omitempty"`
	Tools          []string `bson:"tools,omitempty" json:"tools,omitempty"`
	Usage          Usage    `bson:"usage" json:"usage,omitempty"`
	Error          string   `bson:"error,omitempty" json:"error,omitempty"`

	// Seq is the highest event sequence applied. A reattaching stream starts
	// from a snapshot at this sequence and ignores anything at or below it,
	// which is how a snapshot plus live deltas avoid duplicating text.
	Seq int64 `bson:"seq" json:"seq"`

	CreatedAt  int64 `bson:"createdAt" json:"created_at"`
	UpdatedAt  int64 `bson:"updatedAt" json:"updated_at"`
	FinishedAt int64 `bson:"finishedAt,omitempty" json:"finished_at,omitempty"`

	// ExpiresAt drives the Mongo TTL index. Refreshed when the run finishes,
	// so a long run is not deleted mid-flight.
	ExpiresAt time.Time `bson:"expiresAt" json:"-"`
}

// Clone returns a copy safe to hand to another goroutine. Slices are copied so
// a caller cannot mutate the stored run's tool list.
func (r Run) Clone() Run {
	if len(r.Tools) > 0 {
		r.Tools = append([]string(nil), r.Tools...)
	}
	return r
}

// NewID returns a hex ObjectID, which is both the Mongo _id and the id the app
// reattaches with.
func NewID() string { return primitive.NewObjectID().Hex() }

// CapText bounds s to at most limit bytes without cutting a UTF-8 rune in half.
// A truncated value ends with a marker so a reader can tell it is not the whole
// output. The stored document overshoots the limit by the marker (~14 bytes);
// the bound is on the content kept, not the final field size.
//
// Idempotent: a value that already carries the marker and fits is returned
// unchanged, so a prompt trimmed by TrimPrompt and then stored through Capped
// is not cut and marked twice. Length-aware, so text that merely ends with the
// marker but still exceeds the limit is cut once and marked once.
func CapText(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	if strings.HasSuffix(s, truncMarker) {
		if core := strings.TrimSuffix(s, truncMarker); len(core) <= limit {
			return s
		}
		s = strings.TrimSuffix(s, truncMarker)
	}
	// Walk back to a rune boundary. At most 3 bytes of damage, and the result
	// stays valid UTF-8.
	cut := limit
	for cut > 0 && (s[cut]&0xC0) == 0x80 {
		cut--
	}
	return s[:cut] + truncMarker
}

const truncMarker = "… (truncated)"

// Capped returns a copy with the size-bounded fields trimmed for persistence.
func (r Run) Capped() Run {
	r.Prompt = CapText(r.Prompt, MaxPrompt)
	r.Output = CapText(r.Output, MaxOutput)
	r.Reasoning = CapText(r.Reasoning, MaxReasoning)
	if len(r.Tools) > MaxTools {
		r.Tools = append([]string(nil), r.Tools[:MaxTools]...)
	}
	return r
}

// Store is the persistence surface. *Store (Mongo) and *MemoryStore implement
// it; tests use the in-memory one.
type Store interface {
	Create(ctx context.Context, r Run) error
	Get(ctx context.Context, id string) (Run, error)
	Save(ctx context.Context, r Run) error
	List(ctx context.Context, f Filter) ([]Run, error)
	// MarkInterrupted turns every still-running record into StatusInterrupted.
	// Called once at startup: execution cannot outlive the process, and a
	// "running" row left behind would make the list lie forever.
	MarkInterrupted(ctx context.Context, at time.Time) (int64, error)
}

// Filter selects runs for List. Zero values mean "no constraint"; Limit is
// clamped to ListCap, and a Limit of 0 means the default (ListCap), not "no
// rows" — callers that want an empty list do not ask for one.
type Filter struct {
	Profile        string
	ConversationID string
	Status         Status
	Limit          int
}

// Matches reports whether r satisfies the filter. The gateway's live overlay
// refreshes stored rows with newer snapshots after the store already filtered,
// so it re-applies the filter to the merged list rather than trusting it.
func (f Filter) Matches(r Run) bool {
	if f.Profile != "" && r.Profile != f.Profile {
		return false
	}
	if f.ConversationID != "" && r.ConversationID != f.ConversationID {
		return false
	}
	if f.Status != "" && r.Status != f.Status {
		return false
	}
	return true
}

// StoreError is the domain error the HTTP layer maps onto status codes, the
// same shape cronjobs uses.
type StoreError struct {
	Msg      string
	NotFound bool
	Upstream bool
}

func (e *StoreError) Error() string { return e.Msg }

// NotFound reports whether err is a missing run.
func NotFound(err error) bool {
	se, ok := err.(*StoreError)
	return ok && se.NotFound
}

// NewStoreError builds a caller-facing store error.
func NewStoreError(msg string, upstream bool) *StoreError {
	return &StoreError{Msg: msg, Upstream: upstream}
}

// trimPrompt keeps the stored prompt a preview rather than a copy of the
// conversation. Applied by callers building a Run; exported so the gateway and
// tests agree on the rule.
func TrimPrompt(p string) string { return CapText(strings.TrimSpace(p), MaxPrompt) }
