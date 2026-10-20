package teach

import (
	"context"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// A user-taught skill (#314): a procedure saved from the app, reviewed and
// test-run before it may steer turns or schedules.
//
// Storage model (see docs/adr/0004-taught-skills.md): Mongo
// (taught_skills) is the source of truth; the gateway materializes ACTIVE
// skills as SKILL.md files under a runtime dir for Pi's --skill flag. Git
// never sees them: committed pi/skills/* and taught skills meet only in the
// v1/skills list and on the model via separate --skill roots.

const (
	// SkillsCollection holds taught skills.
	SkillsCollection = "taught_skills"
	// ListCap bounds one list response.
	ListCap = 100
	// MaxDescription bounds a skill description in runes (frontmatter line).
	MaxDescription = 500
	// MaxBody bounds a skill body in runes. Procedures need room; the file
	// is still small next to a prompt.
	MaxBody = 20000
	// MaxInput bounds a test-fire input in runes: it rides verbatim into the
	// model prompt, so it clips like every other free-text field.
	MaxInput = 2000
	// MaxTestText bounds a stored test transcript in runes.
	MaxTestText = 4000
)

// Status is a skill's lifecycle. Drafts are inert: review, edit and test
// happen before activate, and only active skills materialize or promote.
type Status string

const (
	StatusDraft    Status = "draft"
	StatusActive   Status = "active"
	StatusDisabled Status = "disabled"
)

// Valid reports whether s is a status this package knows.
func (s Status) Valid() bool {
	return s == StatusDraft || s == StatusActive || s == StatusDisabled
}

// Skill is one taught procedure.
type Skill struct {
	ID          string `bson:"_id" json:"id"`
	Profile     string `bson:"profile" json:"profile"`
	Name        string `bson:"name" json:"name"`
	Description string `bson:"description" json:"description"`
	Body        string `bson:"body" json:"body"`
	Status      Status `bson:"status" json:"status"`
	// Source names where the draft came from (e.g. a thread id), if anywhere.
	Source   string `bson:"source" json:"source"`
	TestedAt int64  `bson:"testedAt" json:"tested_at"`
	// TestText is the last test transcript, clipped. The writer saw the full
	// output at test time; this is the record, not the signal.
	TestText  string `bson:"testText" json:"test_text"`
	CreatedAt int64  `bson:"createdAt" json:"created_at"`
	UpdatedAt int64  `bson:"updatedAt" json:"updated_at"`
}

// Update carries the mutable fields of an edit. Nil means "leave alone".
// Source is deliberately absent: provenance is set at create and immutable
// after, so no edit can re-attribute a procedure to another writer.
type Update struct {
	Name        *string
	Description *string
	Body        *string
}

// Store is the persistence surface. *MongoStore and *MemoryStore implement
// it; the gateway routes and pi-gateway wiring share one.
type Store interface {
	Create(ctx context.Context, s Skill) (Skill, error)
	Get(ctx context.Context, id, profile string) (Skill, error)
	Update(ctx context.Context, id, profile string, u Update) (Skill, error)
	Delete(ctx context.Context, id, profile string) error
	List(ctx context.Context, profile string) ([]Skill, error)
	// SetStatus moves one skill along its lifecycle. Activating an untested
	// skill fails: review/test before run is the contract, not a UI hint.
	SetStatus(ctx context.Context, id, profile string, st Status) (Skill, error)
	// RecordTest stores a test transcript and stamps the skill tested.
	RecordTest(ctx context.Context, id, profile string, at int64, text string) (Skill, error)
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

// MaxLabel bounds short display labels (promoted job names) in runes.
const MaxLabel = 200

// ClipLabel trims and bounds a short label.
func ClipLabel(s string) string {
	return clipRunes(strings.TrimSpace(strings.ReplaceAll(s, "\x00", "")), MaxLabel)
}

// ClipInput trims and bounds a test input.
func ClipInput(s string) string {
	return clipRunes(strings.TrimSpace(strings.ReplaceAll(s, "\x00", "")), MaxInput)
}

// nameRe is the skill-name rule: filesystem-safe (it becomes a directory),
// Pi-command-safe, lowercase. Checked in the stores and the gateway alike.
var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// idRe bounds caller-supplied ids: routing caps at 64 chars and rejects
// slashes, so an over-long id would create an unreachable row. Generated
// ids are 24-hex and always pass.
var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ValidID reports whether a caller-supplied id is storable and routable.
func ValidID(id string) bool {
	return idRe.MatchString(id)
}

// ValidName reports whether a raw name is a usable skill name.
func ValidName(raw string) bool {
	return nameRe.MatchString(strings.TrimSpace(raw))
}

// CleanDescription trims a description and bounds it by runes.
func CleanDescription(d string) string {
	t := collapseWS(d)
	if utf8.RuneCountInString(t) > MaxDescription {
		t = strings.TrimSpace(string([]rune(t)[:MaxDescription]))
	}
	return t
}

// CleanBody trims a body and bounds it by runes.
func CleanBody(b string) string {
	t := strings.TrimSpace(strings.ReplaceAll(b, "\x00", ""))
	if utf8.RuneCountInString(t) > MaxBody {
		t = strings.TrimSpace(string([]rune(t)[:MaxBody]))
	}
	return t
}

// MaxSource bounds the source label in runes: client-supplied like the
// rest, so it clips like the rest instead of riding unbounded.
const MaxSource = 200

// clipRunes bounds free text stored alongside a record (test transcripts).
func clipRunes(s string, max int) string {
	if utf8.RuneCountInString(s) > max {
		return strings.TrimSpace(string([]rune(s)[:max]))
	}
	return s
}

func collapseWS(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// SafeName returns the name if it is valid, else a fixed fallback: a
// hand-edited row must never inject YAML at render time or raw text into a
// cron prompt. Used everywhere a stored name crosses a trust boundary.
func SafeName(s Skill) string {
	if ValidName(s.Name) {
		return s.Name
	}
	return "invalid-name"
}

// RenderSKILL renders the SKILL.md Pi loads: frontmatter in the same shape
// as the committed skills, then the procedure body verbatim.
func RenderSKILL(s Skill) string {
	name := SafeName(s)
	var b strings.Builder
	b.WriteString("---\nname: " + name + "\n")
	b.WriteString("description: \"" + yamlEscape(s.Description) + "\"\n")
	b.WriteString("---\n\n")
	b.WriteString(s.Body + "\n")
	return b.String()
}

func yamlEscape(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	return strings.ReplaceAll(s, "\"", "\\\"")
}

// NewID returns a hex ObjectID: the Mongo _id and the id the app edits.
func NewID() string {
	return primitive.NewObjectID().Hex()
}

// nowMillis is the clock, one call per operation.
func nowMillis() int64 { return time.Now().UnixMilli() }
