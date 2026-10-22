package tasks

// Mutation log (#176).
//
// Before this, nothing recorded that a task changed. The agent and the app are
// the same caller as far as the server is concerned — one shared password, one
// collection, no audit — so when a task changed unexpectedly there was no way to
// answer WHEN, from WHERE, or BY WHOM. Every other store in this stack carries a
// `source` field for the same reason; tasks had none.
//
// Append-only and best-effort. A failed write must NOT fail the mutation that
// succeeded: the user asked for a task change, and refusing it because the audit
// record could not be written would be a worse outcome than a gap in the log. So
// every call here swallows its error deliberately, and says so at each site.

import (
	"context"
	"log"
	"slices"
	"sort"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// MutationOps, as recorded. The set is what the routes can do, not what the store
// can do — the log answers "what did a CALLER change", which is the question that
// has no other answer.
const (
	OpCreate    = "create"
	OpUpdate    = "update"
	OpDelete    = "delete"
	OpComplete  = "complete"
	OpSkip      = "skip"
	OpReopen    = "reopen"
	OpDuplicate = "duplicate"
)

// knownOps is every op the log can hold.
//
// It is the validation set for a filter, so it lives next to the constants
// that define the vocabulary rather than in the reader: a new op is added in
// one place, and that place is where the op is written. A copy of this list
// somewhere else is a list that goes stale, and a stale list rejects a valid
// op — the caller asking "what changed?" gets an error instead of an answer.
//
// The WRITE side consults it too (RecordMutation drops an op it does not
// know), so a new op must be added here in the same change as its constant —
// otherwise the entries for it are dropped on the way in. The constant
// itself must be LOWERCASE: both sides lowercase before comparing, so a
// mixed-case constant would be stored as "foo", never match the "Foo" in
// this list, and ship dead.
//
// Unexported, and handed out as a copy: an exported slice is a slice any
// caller can append to, and one that changes the validation set process-wide
// by accident is a bug with no trace.
//
// READ-ONLY after init. The messages above build their "known ops" list from
// the copy (KnownOps), so nothing here can hand out the list itself — and
// an in-package edit that appended to it "temporarily" would change what
// every caller may ask for, silently.
var knownOps = []string{OpCreate, OpUpdate, OpDelete, OpComplete, OpSkip, OpReopen, OpDuplicate}

// KnownOps returns every op the log can hold.
//
// A copy, not the list itself: the set is the validation rule, and a caller
// that edited it would change what every other caller is allowed to ask for.
func KnownOps() []string {
	return slices.Clone(knownOps)
}

// auditCollection is separate from `tasks` on purpose: it must survive the task.
// A log that goes away with its subject cannot answer "what happened to it".
const auditCollection = "task_mutations"

// RecordMutation appends one entry. Best-effort — see the package comment.
//
// The write is DETACHED from the caller's context, so it costs the CALLER
// up to its own 5s budget on the response path: every verb records its row
// before answering, so a slow Mongo delays an acknowledgement for a change
// that has already committed. The batch pays that once per op; a single
// verb pays it once. The trade is the same in both — the log is durable by
// default, and a change the log does not record is a change nobody can
// explain later.
func (s *Store) RecordMutation(ctx context.Context, op, taskID, source, detail string) {
	// The SENT spelling — trimmed, not lowercased — is what the drop
	// paths below echo, and it is what the transports' 422 echoes: one
	// mistake, one spelling, so an operator correlating the app's error
	// with the server's log finds it. The four drop lines match EACH
	// OTHER for the same reason: they are handed the same arguments.
	sentOp, sentID := strings.TrimSpace(op), strings.TrimSpace(taskID)
	// The id and the op are normalized HERE, on the write side, for the
	// same reason the read side normalizes them.
	//
	// The verbs accept an UPPERCASE hex id — ObjectIDFromHex does — and
	// would go on to write a row whose `task_id` is upper case, which the
	// lowercased read filter can never match. The row would be invisible:
	// the empty history that reads like "this task was never touched",
	// for a task that was very much touched. The op is normalized for the
	// same reason: a direct caller passing "CREATE" would otherwise store
	// a row the filter misses.
	//
	// No BACKFILL, and none is needed: every row written before this
	// normalisation came from a transport, and the transports' ids came
	// from `ObjectID.Hex()` (always lower case) and their ops from the
	// constants (always lower case). A row that a direct caller wrote
	// with an upper-case id before this change is unreachable — it was
	// unreachable then too, because the read side has always lowercased.
	taskID = canonicalID(sentID)
	op = lowerTrim(sentOp)
	// A store that was never connected cannot record anything, and the
	// function is best-effort by contract: it swallows its own failures
	// rather than failing the mutation they describe. The read side says
	// "not connected" out loud; here, the log simply has no entry — and
	// says so, because a silent hole in the log is the one thing a log
	// must not have.
	if s == nil || s.db == nil {
		log.Printf("tasks: mutation log entry dropped (store is not connected, op=%q task_id=%q)",
			TruncateBytes(sentOp, 40), TruncateBytes(sentID, 40))
		return
	}
	if op == "" || taskID == "" {
		// A programmer error, not a caller error: the transports always
		// pass both, and a dropped entry is a hole in the log. Logged, the
		// way the failed write below is — best-effort means the mutation
		// still succeeds, not that the gap is invisible. The values are
		// bounded: a caller passing 10KB of id must not write 10KB of log.
		log.Printf("tasks: mutation log entry dropped (op=%q task_id=%q)",
			TruncateBytes(sentOp, 40), TruncateBytes(sentID, 40))
		return
	}
	// Every row in this collection must be one the READ side can find:
	// a known op, and a hex id the filter accepts. A row that fails either
	// test is visible only in a whole-log read and can never be narrowed
	// to — a record that exists but cannot be asked for. Both checks are
	// programmer errors (the transports validate the id before they get
	// here, and pass op constants), so they are dropped loudly.
	//
	// A NEW op must be added to knownOps in the same change as the
	// constant: the read side refuses what the write side drops, and a
	// writer that invents an op the log does not know would lose the
	// entry entirely.
	if _, err := NormalizeTaskID(taskID); err != nil {
		log.Printf("tasks: mutation log entry dropped (task_id=%q is not a hex id)", TruncateBytes(sentID, 40))
		return
	}
	if err := ValidateOps([]string{op}); err != nil {
		log.Printf("tasks: mutation log entry dropped (op=%q is not a known op)", TruncateBytes(sentOp, 40))
		return
	}
	// The duplicate's detail is an id — the task the copy came from — so
	// it is canonicalised HERE, on the write seam, like the task_id. The
	// batch path already lowercases it, but the seam is where every
	// writer's id passes, and a detail that kept the caller's spelling
	// would not join against a task_id lookup.
	//
	// ONLY the duplicate's detail is id-like, and only it is
	// canonicalised: the other ops' details are field lists and rollover
	// outcomes, which are already lower-case constants. A future op with
	// an id-like detail (a merge source, say) has to route it through
	// NormalizeTaskID the same way, or it will store a value that joins
	// against nothing.
	if op == OpDuplicate {
		if src, nerr := NormalizeTaskID(detail); nerr == nil && src != "" {
			detail = src
		} else if strings.TrimSpace(detail) == "" {
			// No back-link at all: a copy row that says what it was
			// copied from nowhere. Kept — the row itself is findable, and
			// dropping it would lose the record of a copy that really
			// happened — and logged, so an operator looking for join
			// failures is not sent after a value that was never there.
			//
			// The copy's id is echoed, like every other line in this
			// file: a log line that cannot be joined back to a task_id is
			// the one kind that cannot be acted on.
			log.Printf("tasks: mutation log entry kept with a MISSING duplicate detail (no back-link) op=%q task_id=%q",
				TruncateBytes(sentOp, 40), TruncateBytes(sentID, 40))
		} else {
			// A back-link that joins nothing: the detail is not an id, so
			// it cannot be matched against a task_id lookup — the one
			// thing it is for. Kept and logged, for the same reason.
			// Lowercased like every other id-like field on this seam, so
			// the stored value differs from the caller's only in case.
			log.Printf("tasks: mutation log entry kept with an unjoinable duplicate detail (%q) op=%q task_id=%q",
				TruncateBytes(strings.ToLower(strings.TrimSpace(detail)), 40),
				TruncateBytes(sentOp, 40), TruncateBytes(sentID, 40))
			detail = strings.ToLower(strings.TrimSpace(detail))
		}
	}
	entry := bson.M{
		"op":      op,
		"task_id": taskID,
		// Trimmed before it is capped, so the stored label is the label the
		// caller meant: "  app  " and "app" are one caller, not two.
		"source": TruncateBytes(strings.TrimSpace(source), 40),
		"detail": TruncateBytes(strings.TrimSpace(detail), 300),
		"at":     primitive.NewDateTimeFromTime(time.Now().UTC()),
	}
	// The audit write must NOT inherit the caller's cancellation.
	//
	// ctx here is the request context, which Go cancels the moment the client
	// disconnects — and the interesting case is precisely a client that DID: it
	// timed out or hung up, the mutation had already committed, and then the log
	// entry for it was dropped because its context was dead. That inverts the point
	// of the log. "What happened to this task?" is asked most often about the change
	// nobody can explain, and a disconnected client is exactly the sort of change
	// nobody saw happen.
	//
	// WithoutCancel keeps the context's values and drops the cancellation; the short
	// timeout keeps the write bounded so it cannot outlive the process indefinitely.
	// 5s is ample for one indexed insert and, being independent of the request, does
	// not extend the response the client is no longer waiting for.
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), auditWriteBudget)
	defer cancel()
	if _, err := s.db.Collection(auditCollection).InsertOne(auditCtx, entry); err != nil {
		// Still best-effort: the user's mutation succeeded, so a log gap is the lesser
		// failure. But it is logged loudly rather than swallowed, because a silent gap
		// is indistinguishable from "nothing happened".
		// Quoted, like the two drop paths above: op and task_id are
		// constants and hex today, so this is only about a future caller
		// that passes a raw label and splits a log line with it. The SENT
		// spelling, like the drops and the 422s — one mistake, one
		// spelling.
		//
		// The driver's own message is bounded too: a Mongo error can
		// embed the rejected document, and one bad insert should not
		// write kilobytes to the log. Quoted, for the same reason op and
		// task_id are: an embedded document can carry newlines, and one
		// bad insert should not write a multi-line log entry.
		log.Printf("tasks: mutation log write failed (op=%q task_id=%q): %q",
			TruncateBytes(sentOp, 40), TruncateBytes(sentID, 40),
			TruncateBytes(err.Error(), 300))
	}
}

// auditWriteBudget bounds one audit insert, independently of the request that
// triggered it. See RecordMutation.
//
// The same value as the tasks package's cursorCloseBudget, and NOT shared
// with it: that one bounds a detached CLOSE, this one a detached INSERT.
// Coupling them would mean tuning a write's durability against a read's
// cleanup.
const auditWriteBudget = 5 * time.Second

// ChangedFieldNames names which fields an update touched, for the audit log's `detail`.
//
// It names EVERY supplied key (except `expected_revision` and nulls), so it
// answers "what did the caller send?". That is the right question for
// VALIDATION — an unknown key has to be caught there, not ignored.
//
// For "what did the write actually change?", the answer is
// UpdateFieldNames, which names only the fields the update path knows.
// The two deliberately disagree about an unknown key, and each is used
// where its own question is the right one: the transports' audit guard
// and the batch's detail use UpdateFieldNames, the batch's extras
// rejection uses this one. Unifying them would reopen either the
// silent-drop (an unknown update key logged as a change) or the
// silent-allow (an unknown field on a non-update op slipping past).
//
// It lives here, in the package that owns the log, because BOTH callers
// had their own copy — cmd/health-api had `changedKeys` and cmd/task-manager had
// `changedFieldNames`, identical apart from a comment and the order of two lines. Two
// copies of one rule is the defect shape this stack has produced repeatedly, and it
// matters more here than usual: the two callers are the HTTP API and the agent, so a
// divergence would show up as the app and the agent describing the same edit differently
// in the one log meant to reconcile them.
//
// Sorted so the string is stable — Go randomises map iteration, and
// an unstable detail would make two identical updates look different in the log.
func ChangedFieldNames(fields map[string]any) string {
	keys := make([]string, 0, len(fields))
	for k, v := range fields {
		// expected_revision is the optimistic-concurrency guard, not a change to the
		// task. A JSON null is treated as ABSENT by every field read in this store, so
		// naming it here would claim an edit that Update never makes — the caller uses
		// this to decide whether to write an audit entry at all, and a null-valued field
		// takes Update's no-op path.
		if k == "expected_revision" || v == nil {
			continue
		}
		keys = append(keys, k)
	}
	if len(keys) == 0 {
		return "no fields"
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
