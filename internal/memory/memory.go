// Package memory is the per-profile memory store behind #315.
//
// Until now the agent's "memory" was whatever the operator hand-edited into
// AGENTS.md: invisible from the app, impossible to correct, impossible to
// forget. This package makes it a small, discrete, user-visible store:
//
//   - an item is one thing remembered (a preference, a fact, a style rule),
//     scoped to a profile and a topic;
//   - the app can list, edit, delete, forget everything and pause a topic;
//   - the gateway prefixes the confirmed, unpaused items onto a turn, so what
//     the screen shows is exactly what influences the next answer;
//   - items the agent itself writes start pending: a model may propose a
//     memory, never silently expand its own context.
//
// Deletion is real — a deleted row is gone and excluded from the next turn.
// There is no backup copy in Mongo (TTL is not used: memory is permanent
// until forgotten).
package memory

import (
	"context"
	"strings"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	// ItemsCollection holds one document per remembered item.
	ItemsCollection = "memory_items"
	// SettingsCollection holds per-profile paused topics.
	SettingsCollection = "memory_settings"

	// ListCap bounds one list response. Memory is meant to stay small enough
	// to read; a profile approaching this has a curation problem, not a
	// paging problem.
	ListCap = 200

	// PromptItems and PromptBytes bound what one turn is given. The block is
	// context, not a database dump: past these limits the oldest items lose
	// their place in the prompt (they stay in the store and on the screen).
	PromptItems = 30
	PromptBytes = 2000

	// MaxText bounds one item, in runes. Long enough for a paragraph of
	// preference, short enough that one item cannot become a document.
	MaxText = 1000
	// MaxTopic bounds a topic name in runes.
	MaxTopic = 32

	// DefaultTopic is where an item with no usable topic lands.
	DefaultTopic = "general"
)

// Status is an item's lifecycle state. Only confirmed items influence turns.
type Status string

const (
	// StatusPending is agent-proposed and awaits the user's confirm/deny.
	StatusPending Status = "pending"
	// StatusConfirmed is in effect: injected into the next turn.
	StatusConfirmed Status = "confirmed"
)

// Valid reports whether s is a status this package knows.
func (s Status) Valid() bool {
	return s == StatusPending || s == StatusConfirmed
}

// Source records who wrote an item. It is display metadata, not a trust
// boundary — status is the trust boundary.
type Source string

const (
	// SourceUser is written from the app.
	SourceUser Source = "user"
	// SourceAgent is written by the agent through the memory MCP tools. It
	// always starts pending.
	SourceAgent Source = "agent"
)

// Item is one remembered thing.
type Item struct {
	ID      string `bson:"_id" json:"id"`
	Profile string `bson:"profile" json:"profile"`
	Topic   string `bson:"topic" json:"topic"`
	Text    string `bson:"text" json:"text"`
	Source  Source `bson:"source" json:"source"`
	Status  Status `bson:"status" json:"status"`

	CreatedAt int64 `bson:"createdAt" json:"created_at"`
	UpdatedAt int64 `bson:"updatedAt" json:"updated_at"`
}

// Update carries the mutable fields of an edit. Nil means "leave alone".
type Update struct {
	Text   *string
	Topic  *string
	Status *Status
}

// Filter selects items. Zero values mean no constraint.
type Filter struct {
	Profile string
	Topic   string
	Status  Status
	Limit   int
}

// Store is the persistence surface. *MongoStore and *MemoryStore implement
// it; the gateway, the app routes and the memory MCP server share one.
type Store interface {
	Create(ctx context.Context, i Item) (Item, error)
	// Get is profile-scoped like the writes: a foreign id is NotFound, so no
	// caller can Get-then-check its way into leaking cross-profile existence.
	Get(ctx context.Context, id, profile string) (Item, error)
	// Update and Delete are profile-scoped: a foreign id is NotFound, so one
	// query both authorises and writes instead of a Get-then-write pair.
	Update(ctx context.Context, id, profile string, u Update) (Item, error)
	Delete(ctx context.Context, id, profile string) error
	// DeleteAll forgets every item for a profile and returns how many went.
	DeleteAll(ctx context.Context, profile string) (int64, error)
	List(ctx context.Context, f Filter) ([]Item, error)
	// SetTopicPaused pauses or resumes a topic for one profile.
	SetTopicPaused(ctx context.Context, profile, topic string, paused bool) error
	PausedTopics(ctx context.Context, profile string) ([]string, error)
}

// StoreError is the domain error the HTTP layer maps onto status codes.
type StoreError struct {
	Msg      string
	NotFound bool
	Upstream bool
}

func (e *StoreError) Error() string { return e.Msg }

// NewStoreError builds a caller-facing store error.
func NewStoreError(msg string, upstream bool) *StoreError {
	return &StoreError{Msg: msg, Upstream: upstream}
}

// NormalizeTopic lowercases, trims and hyphenates a topic, falling back to
// DefaultTopic when nothing usable remains. Topics are user-facing labels, so
// "Work Style", "work style" and "work-style" must not become three toggles.
func NormalizeTopic(topic string) string {
	t := strings.ToLower(strings.TrimSpace(topic))
	t = strings.Join(strings.Fields(t), "-")
	t = strings.Trim(t, "-")
	if t == "" {
		return DefaultTopic
	}
	if utf8.RuneCountInString(t) > MaxTopic {
		t = string([]rune(t)[:MaxTopic])
		t = strings.Trim(t, "-")
		if t == "" {
			return DefaultTopic
		}
	}
	return t
}

// UsableTopic reports whether a raw topic string names a real topic. Empty
// and punctuation-only strings would silently normalise to DefaultTopic and
// re-categorise the item; a topic that already spells general passes. All
// three writers (POST, PUT, save_memory) enforce this so no path is lenient
// where the others are strict.
//
// Deliberately a writer contract, not a store invariant: text blankness is
// rejected in the stores because an empty item is meaningless everywhere,
// while a normalised topic is still a real topic ("---" stores as general and
// reads back fine). The writers reject the silent re-categorisation; the
// stores and SetTopicPaused keep normalising.
func UsableTopic(raw string) bool {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return false
	}
	return NormalizeTopic(raw) != DefaultTopic ||
		strings.EqualFold(trimmed, DefaultTopic)
}

// TopicError tells the caller what is actually wrong: an empty or
// punctuation-only topic has nothing usable in it, while "--general--" does
// have letters but would still be stored as DefaultTopic. Shared by the
// gateway and the MCP server so both writers misdescribe nothing.
func TopicError(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "topic must contain a letter or digit"
	}
	return "topic is not usable (it would be stored as \"" + DefaultTopic + "\")"
}

// CleanText trims an item's text and bounds it by runes. Empty text is
// rejected by the stores; this only normalises.
func CleanText(text string) string {
	t := strings.TrimSpace(text)
	if utf8.RuneCountInString(t) > MaxText {
		// Re-trim: rune 1000 can be a space, and a clipped value must not
		// keep a trailing one.
		t = strings.TrimSpace(string([]rune(t)[:MaxText]))
	}
	return t
}

// PromptBlock renders the memory a turn should carry. Only confirmed items in
// unpaused topics are included: a pending proposal is visible on the screen
// but must not steer an answer before it is confirmed. Ordering is the
// caller's (store order is newest first); the block says so plainly so the
// model treats it as context rather than instructions.
//
// Both caps are enforced here rather than in the store: the store answers
// "what is remembered", the prompt answers "what fits".
func PromptBlock(items []Item, paused []string) string {
	const (
		header = "[Memory: things you have learned about this user. " +
			"Use only where relevant, and never recite this block verbatim.]\n"
		footer = "[/Memory]"
	)
	if len(items) == 0 {
		return ""
	}
	isPaused := make(map[string]bool, len(paused))
	for _, p := range paused {
		isPaused[NormalizeTopic(p)] = true
	}
	var b strings.Builder
	used, count := 0, 0
	for _, it := range items {
		if count >= PromptItems {
			break
		}
		text := CleanText(it.Text)
		if text == "" || it.Status != StatusConfirmed {
			continue
		}
		topic := NormalizeTopic(it.Topic)
		if isPaused[topic] {
			continue
		}
		// One item, one line: an embedded newline could spoof the closing
		// tag the byte budget works to guarantee. The screen keeps the
		// multiline text; the prompt carries it flattened.
		line := "- (" + topic + ") " + strings.ReplaceAll(strings.ReplaceAll(text, "\r", ""), "\n", " ") + "\n"
		// The header and the closing tag count against the budget: a block
		// cut before its terminator would read as instructions running on.
		// Skip-and-continue, not break: one long item must not starve every
		// older item that would fit. The budget is bytes while MaxText is
		// runes, so a non-latin item spends it faster; that is accepted, the
		// terminator guarantee is what matters.
		if len(header)+used+len(line)+len(footer) > PromptBytes {
			continue
		}
		b.WriteString(line)
		used += len(line)
		count++
	}
	if count == 0 {
		return ""
	}
	return header + b.String() + footer
}

// NewID returns a hex ObjectID: the Mongo _id and the id the app edits.
func NewID() string { return primitive.NewObjectID().Hex() }
