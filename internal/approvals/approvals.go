// Package approvals is the approval-gate and audit-trail half of #313.
//
// Pi offers no pre-execution tool hook: its tool events are post-hoc
// notifications, and the MCP servers run as Pi-spawned stdio subprocesses.
// So enforcement lives in a relay (cmd/mcp-gate) between Pi and each server:
// every `tools/call` passes through it, reads and routine writes forward
// untouched, and sensitive calls hold for an explicit in-app decision.
//
// The app never sees secrets in this flow: approvals carry the tool, its
// redacted arguments and the time — never credentials — and a decision is a
// verb on an id, not a pasted password.
package approvals

import (
	"context"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

const (
	// ApprovalsCollection holds pending and decided approvals. Short-lived:
	// rows expire ApprovalTTL after creation whether decided or not.
	ApprovalsCollection = "approvals"
	// AuditCollection holds one row per proxied tool call. The audit trail
	// outlives the approval that allowed it.
	AuditCollection = "audit_log"

	// ApprovalTTL bounds how long a call waits for a human. Fifteen minutes
	// is long enough to see a notification and answer, short enough that a
	// forgotten approval does not hold a turn past its budget (TurnTimeout
	// is 960s; the proxy hold is shorter, see WaitTimeout).
	ApprovalTTL = 15 * time.Minute

	// WaitTimeout is how long the relay holds one sensitive call. Slightly
	// under ApprovalTTL so expiry, not the race, decides the outcome.
	WaitTimeout = 14 * time.Minute

	// AuditTTL is the audit retention the issue asks for in days. Ninety
	// days keeps roughly a quarter of dispositions queryable without growing
	// an unbounded second archive; retention wipes older rows by TTL index.
	AuditTTL = 90 * 24 * time.Hour

	// ListCap bounds one list response.
	ListCap = 100
)

// Disposition is what the relay decided for one call.
type Disposition string

const (
	// DispositionAllow means the call ran without asking.
	DispositionAllow Disposition = "allowed"
	// DispositionApproved means it ran after an explicit approval.
	DispositionApproved Disposition = "approved"
	// DispositionDenied means a human refused it; Pi got a tool error.
	DispositionDenied Disposition = "denied"
	// DispositionExpired means nobody answered in time; fail closed.
	DispositionExpired Disposition = "expired"
)

// Status is an approval's lifecycle state.
type Status string

const (
	// StatusPending waits for a human.
	StatusPending Status = "pending"
	// StatusApproved released the call.
	StatusApproved Status = "approved"
	// StatusDenied refused it.
	StatusDenied Status = "denied"
	// StatusExpired timed out waiting.
	StatusExpired Status = "expired"
)

// Terminal reports whether no further decision can happen.
func (s Status) Terminal() bool {
	switch s {
	case StatusApproved, StatusDenied, StatusExpired:
		return true
	}
	return false
}

// Valid reports whether s is a status this package knows.
func (s Status) Valid() bool {
	switch s {
	case StatusPending, StatusApproved, StatusDenied, StatusExpired:
		return true
	}
	return false
}

// Policy is the gate rule for one (server, tool): false runs immediately,
// true holds for approval. Unknown tools hold: a tool added without a rule
// is a write the table never reviewed, and failing closed beats executing
// blind. Pi built-in tools (bash/read/write via codemode) never reach the
// relay at all — see the ADR's threat note.
func NeedsApproval(server, tool string, args map[string]any) bool {
	// Dry runs only count: both prune tools default dry_run=true, and a
	// counting call changes nothing. Scoped to the two servers that have the
	// tool: an unclassified "prune_old" elsewhere must not inherit a free
	// pass from a name match.
	if tool == "prune_old" &&
		(server == "miser-money" || server == "health-check") && isDryRun(args) {
		return false
	}
	gated, ok := gatedTools[server]
	if !ok {
		return true
	}
	// Unknown tool on a known server holds: fail closed. The completeness
	// test below forces an explicit rule the moment a server gains one.
	gate, ok := gated[tool]
	return !ok || gate
}

// KnownServer reports whether the policy table has a row for this server. The
// relay warns at startup when it does not, so a wiring typo in mcp.json is
// visible at boot instead of only as per-hold lines. Execution stays
// fail-closed either way: unknown servers hold every call.
func KnownServer(server string) bool {
	_, ok := gatedTools[server]
	return ok
}

// isDryRun reports whether a prune call only counts. Missing or
// non-boolean dry_run is treated as a real prune: fail closed.
func isDryRun(args map[string]any) bool {
	v, ok := args["dry_run"]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// gatedTools names every known tool with its rule: true holds for approval,
// false runs free. The table is COMPLETE by contract — an unknown tool on a
// known server holds (fail closed), so adding a server tool without a rule
// here gates it until someone classifies it. TestPolicyTableIsComplete pins
// the coverage.
//
// Reads never gate. Routine creates and updates (log_meal, create_task,
// add_recipe, log_cook, ...) run free — gating every write would make the
// agent ask permission to take notes, which is approval fatigue, not safety.
// Gate: money writes, deletes, archives, destructive rewrites (fix_*), and
// prunes that actually delete (dry runs are arg-exempted above).
var gatedTools = map[string]map[string]bool{
	"miser-money": {
		"create_account": true, "list_accounts": false,
		"archive_account": true, "get_balances": false,
		"log_transaction": true, "log_text": true,
		"query_transactions": false, "summarize": false,
		"fix_last_transaction": true, "delete_transactions": true,
		"prune_old": true,
	},
	"cookbook": {
		"add_ingredient": false, "list_ingredients": false,
		"delete_ingredient": true, "add_recipe": false,
		"get_recipe": false, "list_recipes": false,
		"update_recipe": false, "delete_recipe": true,
		"scale_recipe": false, "log_cook": false, "list_cooks": false,
	},
	"health-check": {
		"log_meal": false, "fix_last_meal": true, "query_meals": false,
		"delete_meals": true, "log_weight": false, "log_sleep": false,
		"log_workout": false, "daily_summary": false, "prune_old": true,
	},
	"task-manager": {
		"create_task": false, "list_tasks": false, "get_task": false,
		"update_task": false, "complete_task": false, "skip_task": false,
		"reopen_task": false, "delete_task": true,
	},
	"project-manager": {
		"create_project": false, "list_projects": false, "get_project": false,
		"update_project": false, "delete_project": true,
	},
	"notes": {
		"create_note": false, "get_note": false, "list_notes": false,
		"search_notes": false, "update_note": false, "delete_note": true,
	},
}

// Redact returns a copy of args with secret-shaped values masked, recursing
// into nested objects and arrays — tool arguments nest (a "params" object
// carrying a token must not survive because the secret sits one level down).
// None of today's tools take passwords, but arguments are persisted for 90
// days and rendered in the app, so anything named like a credential is masked
// by rule rather than by audit of each tool's schema.
//
// The recursion covers map[string]any and []any — the shapes encoding/json
// produces for tool arguments. A caller that builds typed maps itself
// (map[string]string, []string) is outside that contract and must convert to
// the JSON shapes first; those values fall through the default branch
// unredacted rather than being guessed at by reflection.
func Redact(args map[string]any) map[string]any {
	if len(args) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(args))
	for k, v := range args {
		if isSecretKey(k) {
			out[k] = "***"
			continue
		}
		out[k] = redactValue(v)
	}
	return out
}

func redactValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return Redact(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redactValue(e)
		}
		return out
	default:
		return v
	}
}

// MaxAuditError bounds one persisted tool-error message, in runes.
const MaxAuditError = 300

// secretValue matches credential-shaped text that a server may echo back in an
// error message. Arguments are redacted by key; an error is free text, so it
// needs a value-shape scrub: "invalid token sk-live-..." must not become a
// 90-day audit row just because it arrived in the wrong field.
var secretValue = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:sk|pk|ghp|gho|ghs|ghu|xox[abprs])[-_][A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{8,}`),
	// The key set mirrors isSecretKey so an upstream `passwd=...` is masked as
	// surely as a `password` arg. Optional quotes on both sides of the
	// separator: an error often echoes JSON ({"token":"sk-..."}).
	regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|passcode|secret|token|2fa|otp|api[_-]?key|apikey|private[_-]?key|credential|auth|bearer|cookie|session|access[_-]?key|accesskey|key)["']?\s*[:=]\s*"[^"]*"`),
	regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|passcode|secret|token|2fa|otp|api[_-]?key|apikey|private[_-]?key|credential|auth|bearer|cookie|session|access[_-]?key|accesskey|key)["']?\s*[:=]\s*'[^']*'`),
	regexp.MustCompile(`(?i)\b(?:password|passwd|pwd|passcode|secret|token|2fa|otp|api[_-]?key|apikey|private[_-]?key|credential|auth|bearer|cookie|session|access[_-]?key|accesskey|key)["']?\s*[:=]\s*["']?[^"',}\s]+`),
}

// ScrubError masks credential-shaped text in an upstream error and bounds the
// result. Over-redaction is the accepted direction: a mangled message is
// cheaper than a leaked key in a row the app renders.
func ScrubError(msg string) string {
	msg = strings.TrimSpace(msg)
	for _, re := range secretValue {
		msg = re.ReplaceAllString(msg, "[redacted]")
	}
	if utf8.RuneCountInString(msg) > MaxAuditError {
		// Keep the marker INSIDE the cap and make the cut visible: a silently
		// truncated message reads as a complete one to the app.
		msg = string([]rune(msg)[:MaxAuditError-1]) + "…"
	}
	return msg
}

// isSecretKey reports whether an argument key names a credential.
//
// Substring matching is deliberate and deliberately over-broad: a
// hypothetical "tokenizer" arg is masked too. Over-redacting an audit row is
// the safe direction; missing a credential is not, and the cost of a false
// positive is one unreadable argument name.
func isSecretKey(k string) bool {
	k = strings.ToLower(k)
	for _, sub := range []string{
		"password", "passwd", "pwd", "passcode", "secret", "token",
		"2fa", "otp", "api_key", "apikey", "private_key", "credential",
		"auth", "bearer", "cookie", "session", "access_key", "accesskey",
		// Bare "key" too: over-matching "monkey" is the accepted cost, and a
		// {"key": "sk-..."} arg must not sit in Mongo for 90 days.
		"key",
	} {
		if strings.Contains(k, sub) {
			return true
		}
	}
	return false
}

// Approval is one held tool call awaiting a human.
type Approval struct {
	ID        string         `bson:"_id" json:"id"`
	Server    string         `bson:"server" json:"server"`
	Tool      string         `bson:"tool" json:"tool"`
	Args      map[string]any `bson:"args" json:"args"`
	Status    Status         `bson:"status" json:"status"`
	DecidedBy string         `bson:"decidedBy,omitempty" json:"decided_by,omitempty"`

	CreatedAt int64     `bson:"createdAt" json:"created_at"`
	UpdatedAt int64     `bson:"updatedAt" json:"updated_at"`
	DecidedAt int64     `bson:"decidedAt,omitempty" json:"decided_at,omitempty"`
	ExpiresAt time.Time `bson:"expiresAt" json:"-"`
}

// AuditEntry is one proxied call and what happened to it.
type AuditEntry struct {
	ID          string         `bson:"_id" json:"id"`
	Server      string         `bson:"server" json:"server"`
	Tool        string         `bson:"tool" json:"tool"`
	Args        map[string]any `bson:"args" json:"args"`
	Disposition Disposition    `bson:"disposition" json:"disposition"`
	// Error carries the tool error for a denied/expired call (what Pi saw),
	// or the upstream failure when the call itself broke.
	Error string `bson:"error,omitempty" json:"error,omitempty"`
	// ApprovalID links back when a human decided; empty for direct allows.
	ApprovalID string `bson:"approvalId,omitempty" json:"approval_id,omitempty"`

	At        int64     `bson:"at" json:"at"`
	ExpiresAt time.Time `bson:"expiresAt" json:"-"`
}

// Clone returns a top-level copy: Args is a fresh map, but nested values
// (map[string]any, []any) are shared. That is safe for the current contract —
// Redact builds new values and no caller mutates a returned row — but it is
// not a deep copy, so a future in-place edit must copy the nested value too.
func (a Approval) Clone() Approval {
	if a.Args != nil {
		cp := make(map[string]any, len(a.Args))
		for k, v := range a.Args {
			cp[k] = v
		}
		a.Args = cp
	}
	return a
}

// Clone returns a top-level copy of the audit row: Args is a fresh map, but
// nested values are shared for the same reason as Approval.Clone.
func (e AuditEntry) Clone() AuditEntry {
	if e.Args != nil {
		args := make(map[string]any, len(e.Args))
		for k, v := range e.Args {
			args[k] = v
		}
		e.Args = args
	}
	return e
}

// NewID returns a hex ObjectID: the Mongo _id and the id the app decides on.
func NewID() string { return primitive.NewObjectID().Hex() }

// Store is the persistence surface. *MongoStore and *MemoryStore implement
// it; the relay and the gateway share one.
//
// Shared reports whether this store is visible to other processes. Only a
// shared (Mongo) store lets a relay-held call be decided from the gateway:
// two memory fallbacks in two processes can never meet, so without Mongo
// every gated call waits out its hold and expires by design.
type Store interface {
	Shared() bool
	CreateApproval(ctx context.Context, a Approval) error
	GetApproval(ctx context.Context, id string) (Approval, error)
	// DecideApproval flips a pending approval terminal. Anything else is an
	// error: a decision lands exactly once, and races after expiry lose.
	DecideApproval(ctx context.Context, id string, approved bool, by string, at time.Time) (Approval, error)
	// TimeoutApproval flips a still-pending approval expired. No-op when it
	// already decided — the relay calls this after its wait, win or lose.
	TimeoutApproval(ctx context.Context, id string, at time.Time) error
	ListApprovals(ctx context.Context, f ApprovalFilter) ([]Approval, error)
	Audit(ctx context.Context, e AuditEntry) error
	ListAudit(ctx context.Context, f AuditFilter) ([]AuditEntry, error)
	// MarkInterrupted flips pending rows expired at startup: a restart must
	// not leave a "pending" row the relay that owned it will never read.
	MarkInterrupted(ctx context.Context, at time.Time) (int64, error)
	// MarkServerInterrupted flips one server's pending rows expired. A relay
	// restart owns that server's queue outright (one relay per server), so
	// the rows it finds at startup belong to a process that will never
	// decide them. Scoped by server so six relays do not expire each other.
	MarkServerInterrupted(ctx context.Context, server string, at time.Time) (int64, error)
}

// ApprovalFilter selects approvals. Zero values mean no constraint.
type ApprovalFilter struct {
	Status Status
	Limit  int
}

// AuditFilter selects audit rows.
type AuditFilter struct {
	Server string
	Tool   string
	Limit  int
}

// StoreError is the domain error the HTTP layer maps onto status codes.
type StoreError struct {
	Msg      string
	NotFound bool
	Upstream bool
	Conflict bool
}

func (e *StoreError) Error() string { return e.Msg }

// NewStoreError builds a caller-facing store error.
func NewStoreError(msg string, upstream bool) *StoreError {
	return &StoreError{Msg: msg, Upstream: upstream}
}
