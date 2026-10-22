package tasks

// Change log, read side (#350).
//
// The write side is audit.go: every create, update, complete, skip, reopen
// and delete appends one row to `task_mutations` — the op, the task, the
// caller, the time. That log has been written since #176 with no way to READ
// it: no HTTP route, no MCP tool. The record of "who changed this, and when"
// existed only for whoever opened a Mongo shell.
//
// It matters more than it used to. A completed task is DELETED three days
// after it is completed (the `expiresAt` TTL), so the task row — its name,
// its due date, everything a person would recognise it by — is gone long
// before the log's 90-day window ends. The log outlives its subject on
// purpose (see audit.go); this is the door into it.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"agento/internal/mongostore"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// RequestBudget bounds one task request, at either door.
//
// One number for the task domain: the list, the change log, the single
// verbs, the batch. They share it because they share a failure — a
// request that outlives its budget is a caller waiting on a hung Mongo
// — and a domain with two spellings of one budget is a domain whose
// budgets drift.
//
// The MCP change-log tool takes it for the CHANGE LOG, the read the
// HTTP route bounds the same way; the MCP write tools keep the
// caller's context, because a write cut short mid-flight is a
// different failure from a read that hangs.
//
// It bounds the QUERY, not the whole handler: releasing the cursor
// afterwards has its own budget (closeCursor, 5s), so a handler whose
// read timed out can still take that long to let go of the cursor. The
// extra wait delays only a response the client has already given up
// on.
const RequestBudget = 30 * time.Second

// Mutation is one row of the change log.
//
// The four fields are the whole contract, and each answers one of the
// questions the log exists for: `op` is WHAT happened, `task_id` is WHICH
// task, `source` is WHO did it, `at` is WHEN. `detail` carries the
// op-specific extra — the changed field names for an update, the rollover
// outcome for a completion — and is empty for ops that have nothing to add.
//
// `source` is a LABEL, not an identity: it is self-attested by the caller
// (see mcps/task-manager/README.md), so it answers "which client made this
// change" well enough to explain an accident and badly enough to be evidence
// against anyone.
type Mutation struct {
	TaskID string    `json:"task_id" bson:"task_id"`
	Op     string    `json:"op" bson:"op"`
	Source string    `json:"source" bson:"source"`
	Detail string    `json:"detail" bson:"detail"`
	At     time.Time `json:"at" bson:"at"`
}

// MutationFilter narrows a change-log query. Every field is optional: the
// zero filter is the whole log.
type MutationFilter struct {
	// TaskID restricts to one task. Empty means every task.
	TaskID string
	// Ops restricts to a set of operations. Empty means every op. A
	// non-empty list that normalizes to nothing ([""]) is REJECTED, like
	// a filter of nothing is by ParseOps — a filter that was sent and
	// names no op is a mistake, not a request for the whole log.
	Ops []string
	// Since is an INCLUSIVE lower bound on `at`. The zero time means no
	// bound. Inclusive because a caller that has seen an entry at time T and
	// asks for "since T" means "from T onwards" — dropping that entry would
	// make it miss the very change it was polling for.
	Since time.Time
	// Limit caps the rows returned. Zero or less means DefaultLimit, and
	// anything above MaxLimit is clamped rather than rejected.
	Limit int
}

// normalizeOps lowercases, trims, drops empties and removes repeats.
//
// One rule, two callers: the transport parses a comma-separated string
// through it, and the store seam applies it to the ops a direct caller
// passed. So "complete, " and "COMPLETE" mean the same filter in both,
// and neither can end up asking the database about an empty op.
func normalizeOps(ops []string) []string {
	out := make([]string, 0, len(ops))
	seen := map[string]bool{}
	for _, op := range ops {
		op = lowerTrim(op)
		if op == "" || seen[op] {
			continue
		}
		seen[op] = true
		out = append(out, op)
	}
	return out
}

// namesNoOperation is the ONE message for a filter that named nothing,
// shared by the parser and the validator: the same filter, read through
// either door, gets the same answer.
//
// ONE message, not a branch on the echo: a filter that names nothing is
// all blanks, so whether the echo comes out empty or as a row of commas
// depends on how MANY blanks arrived — and the answer must not depend on
// that. The value is not echoed either: it is blanks, and there is
// nothing there to show.
//
// The COUNT is echoed, though: a filter of one blank and a filter of ten
// thousand blanks are the same mistake at very different scales, and the
// count is the one bounded diagnostic that tells them apart.
//
// The count is per ENTRY POINT, and cannot be otherwise: the parser
// counts the blank tokens a comma-separated string split into, while the
// validator counts the blank elements of the slice it was handed — and a
// slice element is ONE op, never a group (see ValidateOps). So the same
// filter sent to each door in its own shape reports its own count: " , "
// is two blank tokens to the parser and one blank element to the
// validator. Both are the same mistake, told at the scale the caller
// sent it.
func namesNoOperation(count int) *StoreError {
	return fail("op filter names no operation (%d blank element%s); known ops are %s",
		count, plural(count), strings.Join(KnownOps(), ", "))
}

// plural is the "s" of a count, kept beside the one message that uses it.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ParseOps parses a comma-separated op filter ("complete,skip") into the ops
// it names.
//
// An empty string means EVERY op, not none: the whole log is the default
// reading of the question, and "op=" is a caller who asked for the log.
//
// A non-blank filter that names NO op ("op=,") is an error, not the whole
// log: the caller sent a filter, and answering it with the whole log is the
// "wrong data, not merely more of it" this filter is strict about. A stray
// comma beside a real op ("complete,") is tolerated — it names one op and
// adds nothing.
//
// Repeats are dropped, so "create,create" is one op and not two.
func ParseOps(s string) ([]string, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		// No filter at all. Checked BEFORE the split, because splitting
		// an empty string yields one empty token — and a list of one
		// blank is a filter that names nothing, which is a 422.
		return nil, nil
	}
	// The INPUT is bounded before it is split. Every echo in this file is
	// bounded, but the echo is the small end of it: a filter is
	// caller-controlled text, and `?op=` with a megabyte of commas would
	// build a megabyte of tokens before anything could refuse it. The
	// split happens only on a string small enough to split.
	//
	// The bound is on the string that is SPLIT — the one as sent, not
	// the trimmed copy. The split runs on what was sent, so that is what
	// has to fit: padding is part of the string the split will walk.
	if len(s) > maxOpsFilterBytes {
		return nil, fail("op filter is too long (%d bytes; at most %d)",
			len(s), maxOpsFilterBytes)
	}
	// Split the SENT string, padding and all: the validator must echo
	// the token the caller typed, exactly as it does when it is called
	// directly. The trimmed copy is only used to decide whether there is
	// a filter at all.
	//
	// Three passes over a list of at most a few elements, deliberately:
	// this one for the emptiness check, the validator's own for the
	// names it knows, and the return value's. Folding them into one
	// would lose the distinction between "no filter" and "a filter that
	// names nothing" — the branch the strict rule exists for.
	tokens := strings.Split(s, ",")
	ops := normalizeOps(tokens)
	if len(ops) == 0 {
		return nil, namesNoOperation(len(tokens))
	}
	if err := ValidateOps(tokens); err != nil {
		return nil, err
	}
	return ops, nil
}

// maxOpsFilterBytes bounds the op filter BEFORE it is split.
//
// The filter names a set drawn from seven known ops, so a real one is
// well under a hundred bytes and no legitimate filter meets this cap.
//
// It exists because this filter is SPLIT. The other two are handled in
// one piece — a timestamp is parsed, a hex id is checked — and neither
// builds a list out of what it is given, so neither has a split to
// bound. This one would build a token per comma: a megabyte of commas
// is a megabyte of tokens, built before anything could refuse it.
//
// The bound is on the INPUT, not only on the echo: every echo in this
// file was already bounded, and the echo is the small end of it.
const maxOpsFilterBytes = 256

// ValidateOps checks op names against the ones the log can hold.
//
// An unknown op is a caller mistake, and the error names the valid set: an
// empty history that reads like "nothing happened" is the wrong answer to a
// typo, and "you asked for a thing that does not exist" is the right one.
//
// The names are normalized first, by the same rule the parser and the store
// seam use — so this validator, used on its own, agrees with both. An
// exported check that disagreed with the thing it checks would be a second
// rule, and a second rule is where this stack keeps growing surprises.
//
// The error echoes the token the caller SENT, not the normalized copy of it:
// " FROBNICATE " is what the caller can go and fix, and the normalized
// spelling would have them hunting for the wrong string.
//
// A filter that was sent and names NOTHING is refused too — the same rule
// the transport's parser applies, so this validator and the seam cannot
// answer differently about the same input.
//
// One asymmetry, stated here: a BLANK STRING is no filter at all (that is
// ParseOps's rule — "?op=" means every op), while a non-empty LIST that
// normalizes to nothing is refused. A direct caller that splits a blank
// string into a one-element list therefore gets a 422 where the transport
// gets a 200 — the list says "here is a filter", and the filter is empty.
//
// A list element is ONE op, never a comma-separated group: ParseOps is
// where a string is split, and a slice that arrives here has already been
// split. So a slice element that still contains a comma is simply an op
// the log does not hold.
func ValidateOps(ops []string) error {
	// The copy, not the package var: the check and the messages read the
	// same set, so nothing here can see a different list than the one the
	// error names.
	known := KnownOps()
	if len(normalizeOps(ops)) == 0 && len(ops) > 0 {
		// The FIRST element, bounded: the list named nothing, so every
		// element is blank, and the first is the one a caller would look
		// at. Joined, it would build the whole list first — the
		// "never answer a huge input with itself" rule, one layer up.
		return namesNoOperation(len(ops))
	}
	for _, op := range ops {
		norm := lowerTrim(op)
		if norm == "" {
			continue
		}
		if !slices.Contains(known, norm) {
			// The offending token is echoed, bounded and trimmed: the CASE
			// is kept (it is what the caller typed, and the fix is in the
			// case), the padding is not (it is not what the mistake is,
			// and the id echo trims too — one caller typo, one spelling
			// across the filters). Bounded, because a 10KB query parameter
			// answered with itself is a log line nobody wants.
			return fail("unknown op %q; known ops are %s",
				TruncateBytes(strings.TrimSpace(op), 40), strings.Join(known, ", "))
		}
	}
	return nil
}

// ParseSince parses the `since` filter: an INCLUSIVE lower bound on when the
// change happened.
//
// An empty string means no bound. The layout is RFC3339Nano because the
// `at` this filter is fed back from carries milliseconds — a caller that
// echoes a returned timestamp back must not be told it is malformed — and
// Nano accepts plain RFC3339 too.
//
// The error is a StoreError, so both transports answer it the same way: a
// 422 on HTTP, a plain message over MCP.
func ParseSince(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		// The offending value is echoed, bounded: an app author who sent
		// "2026-10-21 00:00:00" should not have to guess what the server
		// was looking at.
		return time.Time{}, fail("since must be an RFC3339 timestamp, e.g. 2026-10-21T00:00:00Z (got %q)", TruncateBytes(s, 40))
	}
	return t, nil
}

// NormalizeTaskID validates a task id and returns it normalized.
//
// The log stores the task's hex id, and Mongo writes those LOWERCASE, so a
// caller sending the same id in upper case must be answered with the same
// history — not with an empty one. The id is therefore lowercased here, not
// merely checked: `ObjectIDFromHex` accepts upper case, so a check alone
// would pass an id that can never match a stored row.
//
// A malformed id is refused outright: it could only ever match nothing, and
// an empty history reads like "this task was never touched" — the wrong
// answer to a typo. Both transports validate through here, so the agent and
// the app are told the same thing.
//
// A BLANK id is no filter at all, at any length: it means the whole log, so
// there is nothing to echo and nothing to bound — a query string of spaces
// is one trim, not a fan-out. (The op filter is the one that is SPLIT, and
// it is the one that bounds its input; see ParseOps.)
func NormalizeTaskID(id string) (string, error) {
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return "", nil
	}
	lower := canonicalID(id)
	if _, err := primitive.ObjectIDFromHex(lower); err != nil {
		// The SENT token, trimmed: the same spelling the lookup path's
		// "bad id" answer uses, so one bad call reads alike wherever it
		// is caught. The lowercased form would have the caller hunting for
		// the wrong string.
		return "", fail("task_id must be a 24-character hex id (got %q)", TruncateBytes(trimmed, 40))
	}
	return lower, nil
}

// ListMutations returns change-log entries, newest first.
//
// The second return value reports truncation: a +1 probe row is fetched and
// dropped, so a caller can tell "exactly N" from "N of many" without a second
// query — the same contract List has.
func (s *Store) ListMutations(ctx context.Context, f MutationFilter) ([]Mutation, bool, error) {
	// Validated BEFORE any store use, so an unknown op costs no database
	// round trip to discover — and so this rule is testable without one.
	//
	// The ops are normalized here, on a copy, by the SAME rule the
	// transport's parser uses: the stored ops are lowercase, and a caller
	// that reached the store directly must not be told "create" is an
	// unknown op, nor asked about an empty one.
	//
	// The validator sees the caller's OWN list, not the normalized copy:
	// it is the one that can tell "no filter" (an empty list) from "a
	// filter that names nothing" (a list of blanks).
	if err := ValidateOps(f.Ops); err != nil {
		return nil, false, err
	}
	// Normalized a second time here, on purpose: ValidateOps normalizes
	// internally, and folding the two passes into one would lose the
	// distinction it makes — "no filter" (an empty list) from "a filter
	// that names nothing" (a list of blanks). Two passes over a list of
	// at most seven ops is not a cost worth that.
	ops := normalizeOps(f.Ops)
	// The id is normalized here too, not only in the transports: the store
	// is the seam, and a caller that reached it directly must not be able to
	// ask about "not-an-id" or about an id in the wrong case and get an
	// empty history that reads like "never touched".
	taskID, err := NormalizeTaskID(f.TaskID)
	if err != nil {
		return nil, false, err
	}
	// A store that was never connected is a programmer error, and a
	// programmer error that reads as a nil-pointer panic costs the reader
	// the stack that says where. The message says it instead.
	//
	// A plain error, NOT a StoreError: the domain error is what the HTTP
	// layer maps to 422 "your request was wrong", and this is not that —
	// it is the server's fault, and it belongs in the 500 branch.
	if s == nil || s.db == nil {
		return nil, false, errors.New("tasks: store is not connected")
	}
	filt := bson.M{}
	if taskID != "" {
		filt["task_id"] = taskID
	}
	if len(ops) == 1 {
		// One op is an equality, not a set: it reads clearer in a query
		// log and plans the same. The common narrowed read — one task,
		// one kind of change — lands here.
		filt["op"] = ops[0]
	} else if len(ops) > 1 {
		filt["op"] = bson.M{"$in": ops}
	}
	if !f.Since.IsZero() {
		filt["at"] = bson.M{"$gte": f.Since}
	}
	// The limit is an int: ClampLimit answers one, and the one cast that
	// the driver's int64 limit needs sits at that call.
	lim := mongostore.ClampLimit(f.Limit, DefaultLimit, MaxLimit)
	// Newest first, with `_id` as the tiebreak: two mutations in the same
	// millisecond would otherwise come back in an order the database
	// chose, and a history that reshuffles itself between two reads of
	// the same window is a history nobody can read.
	//
	// The tiebreak buys STABILITY, not causality: ObjectID's counter is
	// per-process, so two writers racing the same millisecond are ordered
	// by `_id` without that order being the order they happened in.
	cur, err := s.db.Collection(auditCollection).Find(ctx, filt,
		options.Find().
			SetSort(bson.D{{Key: "at", Value: -1}, {Key: "_id", Value: -1}}).
			SetLimit(int64(lim)+1))
	if err != nil {
		return nil, false, err
	}
	// The cursor is closed WITHOUT the request's deadline: a read that
	// timed out must still release the server-side cursor, and a Close
	// that inherits the expired context would leave it to the driver's own
	// expiry instead. The same reason the audit write detaches its
	// context (audit.go).
	//
	// BOUNDED, like the audit write: WithoutCancel drops the deadline as
	// well as the cancellation, so an unbounded Close on a hung Mongo
	// would hold this goroutine long past the request that started it.
	defer closeCursor(ctx, cur)
	var rows []Mutation
	if err := cur.All(ctx, &rows); err != nil {
		return nil, false, err
	}
	// An empty result is an empty LIST, never a nil slice: the transports
	// both promise `[]` on the wire, and a direct store caller marshalling
	// this slice must not get `null` for the same answer.
	if rows == nil {
		rows = []Mutation{}
	}
	if len(rows) > lim {
		return rows[:lim], true, nil
	}
	return rows, false, nil
}

// closeCursor releases a cursor with a deadline of its own.
//
// It detaches from the caller's context (so a read that TIMED OUT still
// releases the server-side cursor) and bounds the wait (so a Close on a
// hung Mongo cannot hold the goroutine forever — WithoutCancel drops the
// deadline as well as the cancellation).
func closeCursor(ctx context.Context, cur *mongoDrv.Cursor) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cursorCloseBudget)
	defer cancel()
	_ = cur.Close(closeCtx)
}

// cursorCloseBudget bounds one cursor close, independently of the request
// that opened it.
//
// The same value as auditWriteBudget, and NOT shared with it: that one
// bounds a detached INSERT, this one a detached CLOSE. Coupling them would
// mean tuning a read's cleanup against a write's durability.
const cursorCloseBudget = 5 * time.Second
