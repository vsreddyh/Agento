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
	"slices"
	"strings"
	"time"

	"agento/internal/mongostore"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

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
	// Ops restricts to a set of operations. Empty means every op.
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
	parts := strings.Split(raw, ",")
	ops := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		ops = append(ops, p)
	}
	if len(ops) == 0 && raw != "" {
		return nil, fail("op %q names no operation; known ops are %s", raw, strings.Join(knownOps, ", "))
	}
	if err := ValidateOps(ops); err != nil {
		return nil, err
	}
	return ops, nil
}

// ValidateOps checks op names against the ones the log can hold.
//
// An unknown op is a caller mistake, and the error names the valid set: an
// empty history that reads like "nothing happened" is the wrong answer to a
// typo, and "you asked for a thing that does not exist" is the right one.
func ValidateOps(ops []string) error {
	for _, op := range ops {
		if !slices.Contains(knownOps, op) {
			return fail("unknown op %q; known ops are %s", op, strings.Join(knownOps, ", "))
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
		return time.Time{}, fail("since must be an RFC3339 timestamp, e.g. 2026-10-21T00:00:00Z")
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
func NormalizeTaskID(id string) (string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return "", nil
	}
	if _, err := primitive.ObjectIDFromHex(id); err != nil {
		return "", fail("task_id must be a 24-character hex id")
	}
	return id, nil
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
	// The ops are lowercased here, on a copy: the stored ops are lowercase,
	// and a caller that reaches the store directly (bypassing ParseOps)
	// must not be told "create" is an unknown op — the same id in the wrong
	// case is the same task, and so is the same op.
	ops := make([]string, len(f.Ops))
	for i, op := range f.Ops {
		ops[i] = strings.ToLower(strings.TrimSpace(op))
	}
	if err := ValidateOps(ops); err != nil {
		return nil, false, err
	}
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
	if s == nil || s.db == nil {
		return nil, false, fail("store is not connected")
	}
	filt := bson.M{}
	if taskID != "" {
		filt["task_id"] = taskID
	}
	if len(ops) > 0 {
		filt["op"] = bson.M{"$in": ops}
	}
	if !f.Since.IsZero() {
		filt["at"] = bson.M{"$gte": f.Since}
	}
	lim := mongostore.ClampLimit(f.Limit, DefaultLimit, MaxLimit)
	// Newest first, with `_id` as the tiebreak: two mutations in the same
	// millisecond would otherwise come back in an order the database chose,
	// and a history that reshuffles itself between two reads of the same
	// window is a history nobody can read.
	cur, err := s.db.Collection(auditCollection).Find(ctx, filt,
		options.Find().
			SetSort(bson.D{{Key: "at", Value: -1}, {Key: "_id", Value: -1}}).
			SetLimit(lim+1))
	if err != nil {
		return nil, false, err
	}
	defer cur.Close(ctx)
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
	if int64(len(rows)) > lim {
		return rows[:int(lim)], true, nil
	}
	return rows, false, nil
}
