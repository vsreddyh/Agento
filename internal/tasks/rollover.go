package tasks

import (
	"context"
	"strings"
	"time"

	"agento/internal/mongostore"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Rollover policy: what completing a task does about its own recurrence.
// A structured cadence mints its next occurrence here; a custom condition
// is left to the caller (see pi/skills/task-manager/SKILL.md). Split out of
// store.go (#171); still a Store method, because minting writes.

// Rollover is the outcome of a completion's recurrence step, as a named reason
// rather than an absent field (#180).
//
// It used to be `(next == nil)` versus `(next != nil)`, which collapsed four
// distinct outcomes into two answers. "This repeats but no next occurrence was
// created" was indistinguishable from "this task was a one-shot" — so a caller
// could not tell the user the difference, and a genuinely broken repeat looked
// identical to an ordinary completion. That is how four legacy tasks silently
// stopped recurring: the failure was logged to a line nobody read and the API
// reported success.
type Rollover string

const (
	// RolloverCreated: a structured cadence minted the next occurrence.
	RolloverCreated Rollover = "created"
	// RolloverNone: the task is a one-shot. There was never a next to make.
	RolloverNone Rollover = "none"
	// RolloverCustom: the repeat is a condition only the caller can interpret
	// ("every 3rd friday"). The caller owns creating the next occurrence. This is a
	// normal outcome, not a fault — see NeedsAttention.
	RolloverCustom Rollover = "custom"
	// RolloverExhausted: structured, but no future date is computable — the
	// cadence has run past MaxRollovers, or the stored due_date will not parse.
	RolloverExhausted Rollover = "exhausted"
	// RolloverFailed: minting was attempted and errored. The completion itself
	// succeeded; the recurrence did not. This is the case that needs a human.
	RolloverFailed Rollover = "failed"
)

// UserFacing explains the outcome in words a caller can show without inventing
// its own explanation. Empty for the outcomes that need no explanation.
func (r Rollover) UserFacing() string {
	switch r {
	case RolloverCustom:
		return "this task repeats on a custom condition the server will not interpret; create the next occurrence yourself"
	case RolloverExhausted:
		return "this task repeats, but no future date could be computed from its stored due date or the cadence has run out — check the repeat and re-enter it if it should continue"
	case RolloverFailed:
		return "this task was completed, but creating its next occurrence FAILED — the repeat has stopped until you fix it"
	default:
		return ""
	}
}

// NeedsAttention reports whether the recurrence STOPPED and only a human can
// restart it — `exhausted` and `failed`.
//
// Deliberately NOT `custom`. A custom repeat is the caller's ordinary job: the MCP
// path handles it in an earlier branch (with `follow_up`), and the HTTP path
// excludes it. Both already exclude it, so a `NeedsAttention` that includes it
// describes a state no transport can ever report — a contract that reads as
// "something is wrong" and can never fire. See RolloverCustom's own note.
func (r Rollover) NeedsAttention() bool {
	return r == RolloverExhausted || r == RolloverFailed
}

// rolledFromField links a minted occurrence back to the completion that produced
// it. Without it, "did this repeat roll over?" cannot be answered from data —
// which is the whole reason #180's reconciler needs a marker, and why the marker
// is written on the child rather than inferred from name+date (a name and date
// collide with an unrelated task the user created by hand).
const rolledFromField = "rolled_from"

// rolloverPlan decides what a completed task's recurrence REQUIRES, without
// writing anything. Returns the next due date (only when the reason is
// RolloverCreated), the reason, and an error when the data is unusable.
//
// ONE rule, two callers: rollOver acts on the plan, ReconcileRollover reports the
// reason. That is the whole point. The reconciler originally carried its own
// classification — "failed if due_time is blank, exhausted otherwise" — and so
// mislabelled every other failure (an unparseable due_date, a MaxRollovers
// ceiling, untyped estimated_minutes) as `exhausted`, which is the wrong remedy
// for each. Two copies of a rule are two rules.
func rolloverPlan(done map[string]any, now time.Time) (string, Rollover, error) {
	rep := repeatFromDoc(bson.M(done))
	if !rep.IsStructured() {
		if rep.IsZero() {
			return "", RolloverNone, nil
		}
		return "", RolloverCustom, nil
	}
	if err := rep.Validate(); err != nil {
		return "", RolloverFailed, err
	}
	// The stored shape has to be carryable before a successor is even possible.
	// These are the checks rollOver used to make inline, and they are checks on the
	// PLAN, not on Create's behaviour — so the reconciler sees the same verdict.
	if dueTime, _ := done["due_time"].(string); strings.TrimSpace(dueTime) == "" {
		// The exact shape of the legacy rows that stopped recurring: a structured
		// repeat with no time to carry forward. Create would reject it, but the
		// reason belongs to the recurrence, not to the create.
		return "", RolloverFailed, fail("cannot roll over: due_time is required (HH:MM)")
	}
	if _, ok := mongostore.ToInt(done["estimated_minutes"]); !ok {
		return "", RolloverFailed, fail("cannot roll over: estimated_minutes is not a number")
	}
	if _, ok := mongostore.ToBool(done["parallelable"]); !ok {
		return "", RolloverFailed, fail("cannot roll over: parallelable is not a boolean")
	}
	dueDate, _ := done["due_date"].(string)
	nextDate, ok := rep.NextDueDate(dueDate, now)
	if !ok {
		// Unparseable due_date, or the cadence has run past MaxRollovers.
		return "", RolloverExhausted, nil
	}
	return nextDate, RolloverCreated, nil
}

// rollOver mints the next occurrence for a STRUCTURED cadence, or explains why it
// did not. Every non-error path names itself, so no caller has to infer intent
// from an absence.
func (s *Store) rollOver(ctx context.Context, done map[string]any) (map[string]any, Rollover, error) {
	nextDate, reason, err := rolloverPlan(done, time.Now().UTC())
	if err != nil {
		return nil, reason, err
	}
	if reason != RolloverCreated {
		return nil, reason, nil
	}
	rep := repeatFromDoc(bson.M(done))
	dueTime, _ := done["due_time"].(string)
	name, _ := done["name"].(string)
	description, _ := done["description"].(string)
	// Typed reads, already proven carryable by the plan above; these cannot fail,
	// and a default here would be a lie rather than a fallback.
	mins, _ := mongostore.ToInt(done["estimated_minutes"])
	parallel, _ := mongostore.ToBool(done["parallelable"])
	// The back-link is written in the SAME insert as the successor, not as a
	// follow-up UpdateOne. That closes the crash window: with two writes, a crash
	// in between leaves a successor that exists but is unlinked, so the reconciler
	// reports a gap and sends a human to create a task that is already there.
	//
	// A missing parent id is a real problem and says so. It cannot happen through
	// Complete (Get always renders `id`), but silently minting an unlinked successor
	// is how the reconciler starts crying wolf, and a log line is the only trace.
	parentHex, ok := done["id"].(string)
	if !ok || parentHex == "" {
		return nil, RolloverFailed, fail("cannot roll over: parent id is missing")
	}
	if _, err := primitive.ObjectIDFromHex(parentHex); err != nil {
		return nil, RolloverFailed, fail("cannot roll over: parent id '%s' is not an id", parentHex)
	}
	now := primitive.NewDateTimeFromTime(time.Now().UTC())
	doc := bson.M{
		"name": name, "description": description,
		"due_date": nextDate, "due_time": dueTime,
		"estimated_minutes": mins,
		"parallelable":      parallel,
		"revision":          0,
		"completedAt":       nil, "createdAt": now,
		rolledFromField: parentHex,
	}
	for k, v := range rep.docs() {
		doc[k] = v
	}
	if _, err := s.insert(ctx, doc); err != nil {
		return nil, RolloverFailed, err
	}
	// Checked, not asserted: insert always sets doc["_id"] to an ObjectID, but a
	// bare type assertion here is a panic waiting for a contract change, and this
	// function runs inside a user's completion.
	newID, ok := doc["_id"].(primitive.ObjectID)
	if !ok {
		return nil, RolloverFailed, fail("cannot roll over: inserted task has no id")
	}
	next, err := s.Get(ctx, newID.Hex())
	if err != nil {
		return nil, RolloverFailed, err
	}
	return next, RolloverCreated, nil
}

// parentsWithSuccessors returns the set of parent ids (as hex strings) that have at
// least one successor pointing at them. Read-only, one query, no per-row round trip.
//
// Only the back-link field is projected: the answer is "does any doc name this
// parent", so the successor's contents are never read and never needed.
func (s *Store) parentsWithSuccessors(ctx context.Context, candidates []bson.M) (map[string]bool, error) {
	linked := map[string]bool{}
	ids := make([]string, 0, len(candidates))
	for _, d := range candidates {
		if id, ok := d["_id"].(primitive.ObjectID); ok {
			ids = append(ids, id.Hex())
		}
	}
	if len(ids) == 0 {
		return linked, nil
	}
	cur, err := s.tasks.Find(ctx,
		bson.M{rolledFromField: bson.M{"$in": ids}},
		options.Find().SetProjection(bson.M{rolledFromField: 1, "_id": 0}))
	if err != nil {
		return nil, err
	}
	defer func() { _ = cur.Close(ctx) }()
	var rows []struct {
		RolledFrom string `bson:"rolled_from"`
	}
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	for _, r := range rows {
		linked[r.RolledFrom] = true
	}
	return linked, nil
}

// RolloverGap is one completed structured repeat with no successor — a task that
// has silently stopped recurring.
type RolloverGap struct {
	TaskID string
	Name   string
	Why    Rollover
}

// ReconcileRollover finds completed STRUCTURED repeats that never produced a
// successor (#180). Nothing else catches these: rollover happens as a side effect
// of Complete, so any writer that does not go through it — a script, a migration,
// a future admin path, or a rollover that failed and was logged — stops the task
// recurring with no error anywhere a user would see.
//
// Read-only. Repair is deliberately NOT attempted here: a task that failed to roll
// over usually failed because its stored data is wrong (no due_time, an
// unparseable date), and inventing a successor would paper over the real problem.
// It reports, and a human fixes it.
// ReconcileResult is a reconciliation pass's findings, plus whether it managed to
// look at everything.
type ReconcileResult struct {
	Gaps []RolloverGap
	// Scanned and Truncated: the query reads at most ReconcileLimit rows, newest
	// first. If Truncated is true the oldest completions were NOT examined, so the
	// absence of a gap in this result is not evidence there is none. Silently
	// capping is how a busy day loses its oldest stopped repeats.
	Scanned   int
	Truncated bool
}

// ReconcileLimit caps one pass. 500 is generous for a personal task list; the
// cap exists so one pathological day cannot make the pass unbounded.
const ReconcileLimit = 500

func (s *Store) ReconcileRollover(ctx context.Context) (ReconcileResult, error) {
	// No lower time bound. The TTL index already deletes completed tasks RetentionDays
	// after completion, so "still in the collection" IS the window — and an earlier
	// version used a 1-day cutoff, which quietly skipped any repeat that stopped two
	// days ago and was still sitting there in plain sight. Duplicating the retention
	// constant here would be a third place for it to drift.
	cur, err := s.tasks.Find(ctx, bson.M{
		"completedAt":   bson.M{"$ne": nil, "$exists": true},
		"repeat_every":  bson.M{"$gt": 0},
		"repeat_unit":   bson.M{"$nin": bson.A{"", nil}},
		"repeat_custom": bson.M{"$ne": true},
	}, options.Find().
		SetSort(bson.D{{Key: "completedAt", Value: -1}}).
		// +1 so hitting the cap is DETECTABLE: a query returning exactly the limit
		// might have more rows behind it, and silently dropping the oldest gaps is
		// how a busy day loses the repeat that stopped earliest.
		SetLimit(ReconcileLimit+1))
	if err != nil {
		return ReconcileResult{}, err
	}
	// Closed on every exit path. Negligible for a short-lived binary, but a cursor
	// left to the garbage collector is a cursor whose Close error nobody will ever
	// see — and List closes explicitly, so an unclosed one here would be the odd
	// case a future reader copies.
	defer func() { _ = cur.Close(ctx) }()
	var done []bson.M
	if err := cur.All(ctx, &done); err != nil {
		return ReconcileResult{}, err
	}
	var res ReconcileResult
	if len(done) > ReconcileLimit {
		done = done[:ReconcileLimit]
		res.Truncated = true
	}
	// Scanned counts rows actually EXAMINED, set after the cap is applied. Setting
	// it from len(done) beforehand reported the +1 probe row — 501 against a limit of
	// 500 — so the reconciler told the operator it had scanned more than it is
	// allowed to, in precisely the run where the number is being read to judge
	// whether the report is complete. `Truncated` is what says there was more.
	res.Scanned = len(done)
	// Which of these HAD a successor, resolved in ONE query rather than one per row.
	//
	// It was a FindOne per candidate: 500 sequential round trips at the cap, which is
	// an N+1 against a remote Atlas and a real threat to the 45s budget reconcileTasks
	// gets. Measured at ~70s for a capped run locally — over budget before the first
	// row was reported, which is exactly the failure mode a "reports, never repairs"
	// job must not have, because a timed-out reconciler reports nothing at all.
	//
	// A single $in over the ids is the same question asked once. Batch rather than
	// chunked because ReconcileLimit bounds the $in at 500 values, well inside what
	// the server accepts, so there is nothing to chunk.
	//
	// The successor outlives the parent by construction: both get expiresAt =
	// completedAt + RetentionDays, and the successor is completed no earlier than the
	// parent, so if the parent is still here the back-link is too.
	linked, err := s.parentsWithSuccessors(ctx, done)
	if err != nil {
		return ReconcileResult{}, err
	}

	now := time.Now().UTC()
	var gaps []RolloverGap
	for _, d := range done {
		id, ok := d["_id"].(primitive.ObjectID)
		if !ok {
			// A non-ObjectID _id cannot be back-linked or reported. Ignoring the
			// failed assertion would silently produce the zero ObjectID and report
			// it as a gap, which is a phantom task the user cannot act on.
			continue
		}
		// Was a successor EVER minted? The question is existence, not liveness:
		// filtering on `completedAt: nil` false-positives on a CHAIN — a daily task
		// done three days running has a successor that was itself completed and rolled
		// again, so the parent looks unlinked and every chained task is reported as a
		// gap. A back-link is write-once and never cleared, so any doc carrying it is
		// proof the rollover happened.
		if linked[id.Hex()] {
			continue // rolled over fine
		}
		// The SAME classification rollOver would have produced, not a second guess.
		// The old heuristic here only knew about a blank due_time and called
		// everything else `exhausted`, which is the wrong remedy for an unparseable
		// date, a MaxRollovers ceiling or untyped fields.
		_, why, _ := rolloverPlan(map[string]any(d), now)
		if why == RolloverCreated {
			// The plan says it should have rolled and it did not — a write that
			// failed after planning, or a back-link that never landed. Name it
			// as a failure rather than reporting a gap with no explanation.
			why = RolloverFailed
		}
		name, _ := d["name"].(string)
		gaps = append(gaps, RolloverGap{
			TaskID: id.Hex(),
			Name:   name,
			Why:    why,
		})
	}
	res.Gaps = gaps
	return res, nil
}
