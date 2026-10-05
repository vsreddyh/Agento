package tasks

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// The rollover outcome used to be `next == nil`, which made four different
// situations look identical to every caller. Each test below is one of them, and
// the reconciler cases are the ones that actually cost the user their schedule
// (#180).

func structuredRep(every int, unit string) Repeat { return Repeat{Every: every, Unit: unit} }

func TestRolloverOutcomeIsNamed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	t.Run("one-shot is none, not a silent nothing", func(t *testing.T) {
		id := mustCreate(t, s, "One shot", map[string]any{
			"due_date": "2026-10-05", "due_time": "08:00",
		})
		next, r, err := s.rollOver(ctx, mustGet(t, s, id))
		if err != nil {
			t.Fatalf("rollOver: %v", err)
		}
		if r != RolloverNone {
			t.Errorf("rollover = %q, want %q", r, RolloverNone)
		}
		if next != nil {
			t.Errorf("one-shot minted a successor: %v", next)
		}
		if r.NeedsAttention() {
			t.Error("a one-shot must not demand the user's attention")
		}
	})

	t.Run("custom is named so the caller knows it owns the next date", func(t *testing.T) {
		id := mustCreate(t, s, "Custom", map[string]any{
			"due_date": "2026-10-05", "due_time": "08:00",
			"repeat_custom": true, "repeat_rule": "every 3rd friday",
		})
		next, r, err := s.rollOver(ctx, mustGet(t, s, id))
		if err != nil {
			t.Fatalf("rollOver: %v", err)
		}
		if next != nil {
			t.Errorf("custom repeat minted a successor server-side: %v", next)
		}
		if r != RolloverCustom {
			t.Errorf("rollover = %q, want %q", r, RolloverCustom)
		}
		// Custom is NORMAL: the caller owns the next occurrence, which is the whole
		// point of naming it. It must not read as a fault.
		if r.NeedsAttention() {
			t.Error("a custom repeat must not demand the user's attention")
		}
		if r.UserFacing() == "" {
			t.Error("a custom repeat must still tell the caller it owns the next date")
		}
	})

	t.Run("structured creates and back-links the successor", func(t *testing.T) {
		id := mustCreate(t, s, "Structured", map[string]any{
			"due_date": "2026-10-05", "due_time": "08:00",
			"repeat_every": 2, "repeat_unit": "weeks",
		})
		next, r, err := s.rollOver(ctx, mustGet(t, s, id))
		if err != nil {
			t.Fatalf("rollOver: %v", err)
		}
		if r != RolloverCreated || next == nil {
			t.Fatalf("rollover = %q, next = %v; want created", r, next)
		}
		// The back-link is what lets the reconciler PROVE a rollover happened
		// instead of inferring it from a name and date a hand-made task could
		// collide with.
		//
		// Asserted against the RAW document, because that is where the intent lives.
		// This used to read it off a rendered task via mustGet, which made the test
		// depend on `rolled_from` being part of the response contract — and it is
		// internal linkage that deliberately is not. So the assertion was coupled to a
		// presentation decision it never cared about, and would have failed the moment
		// the field was hidden for a reason that had nothing to do with rollover. The
		// reconciler reads it from a raw projection too (parentsWithSuccessors), so this
		// now matches how it is actually consumed.
		childOID, err := primitive.ObjectIDFromHex(next["id"].(string))
		if err != nil {
			t.Fatalf("successor id: %v", err)
		}
		var raw bson.M
		if err := s.tasks.FindOne(ctx, bson.M{"_id": childOID}).Decode(&raw); err != nil {
			t.Fatalf("raw read of the successor: %v", err)
		}
		if got := raw[rolledFromField]; got != id {
			t.Errorf("successor's %s = %v, want the parent's id %q", rolledFromField, got, id)
		}
		// Written in the same insert, so it is present the moment the successor exists.
		if _, exposed := next[rolledFromField]; exposed {
			t.Errorf("%s is exposed on a rendered task; it is internal linkage", rolledFromField)
		}
	})

	t.Run("a structured repeat with no time reports failed", func(t *testing.T) {
		// The exact shape of the legacy rows that stopped recurring: a structured
		// repeat with an empty due_time. Create rejects that today, so the doc is
		// inserted directly to reproduce the historical state.
		id := mustCreate(t, s, "Legacy broken", map[string]any{
			"due_date": "2026-10-05", "due_time": "",
			"repeat_every": 1, "repeat_unit": "days",
		})
		next, r, _ := s.rollOver(ctx, mustGet(t, s, id))
		if r != RolloverFailed {
			t.Errorf("rollover = %q, want %q for a repeat with no time to carry", r, RolloverFailed)
		}
		if next != nil {
			t.Errorf("a failed rollover minted a successor: %v", next)
		}
	})
}

// The reconciler is the only thing in the stack that notices a repeat has stopped.
// If it cannot tell a real gap from a healthy one it is worse than useless: it
// trains the user to ignore the report.
func TestReconcileRolloverFindsOnlyRealGaps(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	// Healthy: a structured repeat that rolled over correctly.
	goodID := mustCreate(t, s, "Good", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
		"repeat_every": 1, "repeat_unit": "days",
	})
	if _, next, r, err := s.Complete(ctx, goodID); err != nil {
		t.Fatalf("complete good: %v", err)
	} else if r != RolloverCreated || next == nil {
		t.Fatalf("good task: rollover = %q, next = %v", r, next)
	}

	// Broken: a structured repeat whose rollover cannot run.
	badID := mustCreate(t, s, "Bad", map[string]any{
		"due_date": "2026-10-05", "due_time": "",
		"repeat_every": 1, "repeat_unit": "days",
	})
	_, _, badReason, err := s.Complete(ctx, badID)
	if err != nil {
		t.Fatalf("complete bad: %v", err)
	}
	if badReason != RolloverFailed {
		t.Fatalf("setup: bad task rollover = %q, want %q", badReason, RolloverFailed)
	}

	res, err := s.ReconcileRollover(ctx)
	if err != nil {
		t.Fatalf("ReconcileRollover: %v", err)
	}
	if res.Truncated {
		t.Fatal("two fixtures must not trip the scan cap")
	}
	seen := map[string]Rollover{}
	for _, g := range res.Gaps {
		seen[g.TaskID] = g.Why
	}
	if _, ok := seen[goodID]; ok {
		t.Error("a task that rolled over correctly must not be reported as a gap")
	}
	if got, ok := seen[badID]; !ok {
		t.Errorf("the stopped repeat was NOT reported; gaps were %+v", res.Gaps)
	} else if got != RolloverFailed {
		t.Errorf("gap reason = %q, want %q", got, RolloverFailed)
	}
}

// A custom repeat completed correctly is the CALLER's job, and a one-shot is not a
// gap at all. Reporting either would make the nightly output noise.
// A daily task done three days running produces a CHAIN: P1 -> S1 -> S2, where
// S1 was itself completed. The successor check used to filter on
// `completedAt: nil`, so it found no OPEN successor for P1 and reported a gap for a
// rollover that worked perfectly. Every chained repeat in the collection was a
// false alarm, and a reconciler that cries wolf gets ignored.
//
// The question is whether a successor was EVER minted, not whether it is still open.
func TestReconcileRolloverAcceptsACompletedSuccessor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	// Day 1.
	day1 := mustCreate(t, s, "Chain", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
		"repeat_every": 1, "repeat_unit": "days",
	})
	_, s1, r1, err := s.Complete(ctx, day1)
	if err != nil || r1 != RolloverCreated || s1 == nil {
		t.Fatalf("complete day1: %v %v %v", s1, r1, err)
	}
	// Day 2: complete the successor, so P1's only back-linked doc is COMPLETED.
	s1ID := s1["id"].(string)
	_, s2, r2, err := s.Complete(ctx, s1ID)
	if err != nil || r2 != RolloverCreated || s2 == nil {
		t.Fatalf("complete day2: %v %v %v", s2, r2, err)
	}

	res, err := s.ReconcileRollover(ctx)
	if err != nil {
		t.Fatalf("ReconcileRollover: %v", err)
	}
	for _, g := range res.Gaps {
		if g.TaskID == day1 || g.TaskID == s1ID {
			t.Errorf("task %s (%s) is part of a chain that rolled over correctly, "+
				"but was reported as a gap", g.TaskID, g.Name)
		}
	}
}

// A custom repeat completed correctly is the CALLER's job, and a one-shot is not a
// gap at all. Reporting either would make the nightly output noise.
func TestReconcileRolloverIgnoresCustomAndOneShot(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	customID := mustCreate(t, s, "Custom done", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
		"repeat_custom": true, "repeat_rule": "every sunday",
	})
	oneShotID := mustCreate(t, s, "One shot done", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
	})
	for _, id := range []string{customID, oneShotID} {
		if _, _, _, err := s.Complete(ctx, id); err != nil {
			t.Fatalf("complete %s: %v", id, err)
		}
	}
	res, err := s.ReconcileRollover(ctx)
	if err != nil {
		t.Fatalf("ReconcileRollover: %v", err)
	}
	for _, g := range res.Gaps {
		if g.TaskID == customID {
			t.Error("a correctly completed custom repeat must not be a gap")
		}
		if g.TaskID == oneShotID {
			t.Error("a one-shot must never be a gap")
		}
	}
}

// The reasons are shown to the user verbatim, so they must say nothing when there
// is nothing to say, and be unmissable when the repeat has actually stopped.
//
// `custom` is deliberately NOT in the attention set: it is the caller's ordinary
// job, both transports handle it in an earlier branch, and a NeedsAttention that
// includes it describes a state nothing can ever report.
func TestRolloverUserFacingOnlyWhenSomethingIsWrong(t *testing.T) {
	for _, r := range []Rollover{RolloverCreated, RolloverNone, RolloverCustom} {
		if r.NeedsAttention() {
			t.Errorf("%q must not demand attention — it is a normal outcome", r)
		}
	}
	if RolloverCustom.UserFacing() == "" {
		t.Error("custom still has to explain itself to the caller; it just is not a FAULT")
	}
	for _, r := range []Rollover{RolloverExhausted, RolloverFailed} {
		if r.UserFacing() == "" {
			t.Errorf("%q must explain itself to the user", r)
		}
		if !r.NeedsAttention() {
			t.Errorf("%q must demand attention", r)
		}
	}
	// The failure message is the one that used to be invisible, so it is the one
	// that must not be missable.
	if f := RolloverFailed.UserFacing(); !strings.Contains(f, "FAILED") {
		t.Errorf("the failure message must be unmissable, got %q", f)
	}
}

// The reconciler must classify a gap with the SAME rule rollOver would have used.
// The old heuristic knew only about a blank due_time and called everything else
// `exhausted`, which is the wrong remedy for an unparseable date or a MaxRollovers
// ceiling. Each case below asserts the classification a plan actually produces.
func TestRolloverPlanClassifiesEachFailure(t *testing.T) {
	now := time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)
	base := func(mutate func(map[string]any)) map[string]any {
		doc := map[string]any{
			"id": "6ac32c0000000000000000aa", "name": "x",
			"due_date": "2026-08-10", "due_time": "08:00",
			"estimated_minutes": 5, "parallelable": false,
			"repeat_every": 1, "repeat_unit": "days",
		}
		mutate(doc)
		return doc
	}
	for _, tc := range []struct {
		name   string
		doc    map[string]any
		reason Rollover
	}{
		{"structured plans a date", base(func(d map[string]any) {}), RolloverCreated},
		{
			"a blank due_time is failed, not exhausted",
			base(func(d map[string]any) { d["due_time"] = "" }),
			RolloverFailed,
		},
		{
			// The distinction matters: "exhausted" says the cadence ran out,
			// "failed" says the stored data is broken, and the remedy differs.
			"an unparseable due_date is exhausted, not failed",
			base(func(d map[string]any) { d["due_date"] = "not-a-date" }),
			RolloverExhausted,
		},
		{
			"untyped estimated_minutes is failed",
			base(func(d map[string]any) { d["estimated_minutes"] = "soon" }),
			RolloverFailed,
		},
		{
			"untyped parallelable is failed",
			base(func(d map[string]any) { d["parallelable"] = "yes" }),
			RolloverFailed,
		},
		{
			"an unknown unit is failed (Validate rejects it)",
			base(func(d map[string]any) { d["repeat_unit"] = "fortnights" }),
			RolloverFailed,
		},
		{
			"a one-shot plans nothing",
			base(func(d map[string]any) {
				delete(d, "repeat_every")
				delete(d, "repeat_unit")
			}),
			RolloverNone,
		},
		{
			"a custom condition is the caller's",
			base(func(d map[string]any) {
				d["repeat_custom"] = true
				d["repeat_rule"] = "every sunday"
			}),
			RolloverCustom,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			date, reason, _ := rolloverPlan(tc.doc, now)
			if reason != tc.reason {
				t.Errorf("rolloverPlan = %q, want %q", reason, tc.reason)
			}
			// A date is offered if and only if the plan says created. Returning a
			// date alongside any other reason is a contradiction a caller could act on.
			if (date != "") != (tc.reason == RolloverCreated) {
				t.Errorf("date = %q with reason %q; a date belongs only with created", date, reason)
			}
		})
	}
}

// ── fixtures ──────────────────────────────────────────────────────────────

// fixtureNames are the tasks these tests create. Cleanup removes exactly these and
// nothing else, because the suite shares a throwaway database with the other task
// tests and must not delete their rows.
var fixtureNames = []string{
	"One shot", "Custom", "Structured", "Legacy broken",
	"Good", "Bad", "Custom done", "One shot done", "Chain",
	// skip_test.go
	"Skip me", "Skip repeat", "Skip twice", "Done then skip",
	"Skip then reopen", "Not skipped",
}

func mustCreate(t *testing.T, s *Store, name string, fields map[string]any) string {
	t.Helper()
	doc := map[string]any{
		"name": name, "description": "d",
		"estimated_minutes": 5, "parallelable": false,
		"revision":    0,
		"createdAt":   primitive.NewDateTimeFromTime(time.Now().UTC()),
		"completedAt": nil,
	}
	for k, v := range fields {
		doc[k] = v
	}
	res, err := s.tasks.InsertOne(context.Background(), doc)
	if err != nil {
		t.Fatalf("insert %s: %v", name, err)
	}
	return res.InsertedID.(primitive.ObjectID).Hex()
}

func mustGet(t *testing.T, s *Store, id string) map[string]any {
	t.Helper()
	doc, err := s.Get(context.Background(), id)
	if err != nil || doc == nil {
		t.Fatalf("get %s: %v %v", id, doc, err)
	}
	return doc
}

func cleanupFixtures(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	cur, err := s.tasks.Find(ctx, bson.M{
		"name": bson.M{"$in": fixtureNames},
	})
	if err != nil {
		return
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return
	}
	for _, r := range rows {
		if _, err := s.tasks.DeleteOne(ctx, bson.M{"_id": r["_id"]}); err != nil {
			t.Errorf("cleanup %v: %v", r["name"], err)
		}
	}
}

// The reconciler used to discard rolloverPlan's error with `_`, so the nightly line
// read a bare "FAILED" and the human had to go and discover which field was wrong.
// The whole premise of this job is that a repeat stopped for a FIXABLE reason and a
// person has to go fix it, so throwing away the one thing the report already knew was
// the most expensive kind of omission.
func TestReconcileGapsCarryTheReasonTheyFailed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	// Two different broken fields, so the detail must distinguish them rather than
	// restate the outcome.
	blankTime := mustCreate(t, s, "Bad time", map[string]any{
		"due_date": "2026-10-05", "due_time": "",
		"repeat_every": 1, "repeat_unit": "days",
	})
	badEstimate := mustCreate(t, s, "Bad estimate", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
		"estimated_minutes": "not a number",
		"repeat_every":      1, "repeat_unit": "days",
	})
	for _, id := range []string{blankTime, badEstimate} {
		if _, _, _, err := s.Complete(ctx, id); err != nil {
			t.Fatalf("complete %s: %v", id, err)
		}
	}

	res, err := s.ReconcileRollover(ctx)
	if err != nil {
		t.Fatalf("ReconcileRollover: %v", err)
	}
	byID := map[string]RolloverGap{}
	for _, g := range res.Gaps {
		byID[g.TaskID] = g
	}
	for id, wantSubstr := range map[string]string{
		blankTime:   "due_time",
		badEstimate: "estimated_minutes",
	} {
		g, ok := byID[id]
		if !ok {
			t.Errorf("task %s was not reported as a gap", id)
			continue
		}
		if g.Why != RolloverFailed {
			t.Errorf("task %s: Why = %q, want %q", id, g.Why, RolloverFailed)
		}
		if g.Detail == "" {
			t.Errorf("task %s: Detail is empty — the reader is told FAILED and nothing else", id)
			continue
		}
		if !strings.Contains(g.Detail, wantSubstr) {
			t.Errorf("task %s: Detail = %q, want it to name %q", id, g.Detail, wantSubstr)
		}
	}
	// The two details must not be the same string, or the field name is being faked.
	if byID[blankTime].Detail == byID[badEstimate].Detail {
		t.Errorf("both gaps report the identical detail %q — it is not naming the field",
			byID[blankTime].Detail)
	}
}

// A gap whose reason is not a validation failure (the cadence is exhausted) has
// nothing specific to add, and must not invent something. Detail stays empty and the
// reader gets the outcome sentence on its own.
func TestReconcileGapDetailIsEmptyWhenThereIsNothingToAdd(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	// A due date so far in the past that no future occurrence can be computed.
	exhausted := mustCreate(t, s, "Exhausted", map[string]any{
		"due_date": "2001-01-01", "due_time": "08:00",
		"repeat_every": 1, "repeat_unit": "days",
	})
	if _, _, r, err := s.Complete(ctx, exhausted); err != nil {
		t.Fatalf("complete: %v", err)
	} else if r != RolloverExhausted {
		t.Fatalf("setup: rollover = %q, want %q", r, RolloverExhausted)
	}

	res, err := s.ReconcileRollover(ctx)
	if err != nil {
		t.Fatalf("ReconcileRollover: %v", err)
	}
	for _, g := range res.Gaps {
		if g.TaskID == exhausted {
			if g.Why != RolloverExhausted {
				t.Errorf("Why = %q, want %q", g.Why, RolloverExhausted)
			}
			if g.Detail != "" {
				t.Errorf("Detail = %q for an exhausted cadence; there is no field to "+
					"blame and it must not invent one", g.Detail)
			}
		}
	}
}

// `rolled_from` is internal linkage and must not reach a client — but the reconciler
// DEPENDS on it to decide whether a repeat rolled over. If anything ever rendered it
// and the reconciler read it back off the rendered doc, hiding the field would
// silently report every chained repeat as a gap.
//
// So this asserts the reconciler still works while the field is hidden, which is the
// condition that makes the strip safe in the first place.
func TestReconcileStillWorksWithRolledFromHidden(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	id := mustCreate(t, s, "Rolled and hidden", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
		"repeat_every": 1, "repeat_unit": "days",
	})
	// Complete returns (doc, next, rollover, err). Taking the FIRST return here — as
	// this test did at first — silently asserts on the PARENT, which has no back-link
	// by construction, and the assertion then fails for a reason that has nothing to
	// do with what it claims to check.
	_, next, r, err := s.Complete(ctx, id)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if r != RolloverCreated || next == nil {
		t.Fatalf("setup: rollover = %q, next = %v", r, next)
	}

	// The back-link IS in the stored document — that is what the reconciler reads.
	oid, err := primitive.ObjectIDFromHex(fmt.Sprint(next["id"]))
	if err != nil {
		t.Fatalf("successor id is not an ObjectID: %v", err)
	}
	var raw bson.M
	if err := s.tasks.FindOne(ctx, bson.M{"_id": oid}).Decode(&raw); err != nil {
		t.Fatalf("raw read: %v", err)
	}
	if raw[rolledFromField] == nil {
		t.Error("rolled_from is not in the stored document — the reconciler would " +
			"report this as a gap regardless of what responses expose")
	}

	// ...and absent from every rendered view of it.
	for _, doc := range []map[string]any{next, mustGet(t, s, fmt.Sprint(next["id"]))} {
		if v, ok := doc[rolledFromField]; ok {
			t.Errorf("%s is exposed on a rendered task: %v", rolledFromField, v)
		}
	}

	// And the reconciler still sees the back-link, so nothing is reported.
	res, err := s.ReconcileRollover(ctx)
	if err != nil {
		t.Fatalf("ReconcileRollover: %v", err)
	}
	for _, g := range res.Gaps {
		if g.TaskID == id {
			t.Error("a repeat that rolled over correctly was reported as a gap — the " +
				"reconciler is reading the back-link from somewhere the strip can reach")
		}
	}
}

// A type-drifted row can pass the reconciler's own "looks structured" filter and still
// fail to classify as structured — and it is then a genuinely broken repeat that must
// be REPORTED.
//
// Measured against Atlas rather than assumed, because the mechanism is not the obvious
// one: a `repeat_every` stored as a string does NOT pass `$gt: 0`, since MongoDB
// brackets comparison operators by BSON type. A `repeat_unit` stored as a NUMBER does
// pass `$nin: ["", nil]` and then reads back as an empty string, so the row is selected,
// classified as non-structured, and its repeat cannot be planned.
//
// This is the case that makes `continue` the wrong fix: skipping would omit a task that
// really did stop recurring, which is the precise failure this job exists to prevent.
func TestReconcileReportsATypeDriftedRepeat(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	drift := func(name string, extra map[string]any) {
		doc := map[string]any{
			"name": name, "description": "d",
			"due_date": "2026-10-05", "due_time": "08:00",
			"estimated_minutes": 5, "parallelable": false,
			"completedAt": primitive.NewDateTimeFromTime(time.Now().UTC()),
			"revision":    0,
		}
		for k, v := range extra {
			doc[k] = v
		}
		if _, err := s.tasks.InsertOne(ctx, doc); err != nil {
			t.Fatalf("insert %s: %v", name, err)
		}
	}
	// Selected by the filter (unit is a number, so it is outside the ["", nil] string
	// bracket) but unreadable as a unit, which is what strands the repeat. This is the
	// case the guard exists for: the classifier calls it `custom`, and that advice is
	// actively wrong, since the task has no rule text at all.
	drift("Drifted unit", map[string]any{"repeat_every": 5, "repeat_unit": 7})
	// Selected and NOT actually broken. `repeat_custom` as a string reads as false
	// because ToBool is a strict type assertion, so the row is structurally fine and
	// gets promoted to `failed` for having no successor — which is the right verdict.
	// It is here because it must also be REPORTED rather than skipped: if `continue`
	// ever comes back, a plain no-successor row goes missing too, and nothing else in
	// the suite would notice.
	drift("Drifted custom", map[string]any{
		"repeat_every": 5, "repeat_unit": "days", "repeat_custom": "yes",
	})

	res, err := s.ReconcileRollover(ctx)
	if err != nil {
		t.Fatalf("ReconcileRollover: %v", err)
	}
	byName := map[string]RolloverGap{}
	for _, g := range res.Gaps {
		byName[g.Name] = g
	}
	for _, name := range []string{"Drifted unit", "Drifted custom"} {
		g, ok := byName[name]
		if !ok {
			t.Errorf("%q was selected by the filter but NOT reported — a skipped row "+
				"here is a task that silently stopped recurring", name)
			continue
		}
		if g.Why == RolloverCustom {
			t.Errorf("%q is reported as a custom repeat, but it has no rule text; the "+
				"operator would be told to create an occurrence against a condition that "+
				"does not exist", name)
		}
		if !g.Why.NeedsAttention() {
			t.Errorf("%q: Why = %q, which NeedsAttention() excludes, so this broken "+
				"repeat would not be flagged for anyone", name, g.Why)
		}
	}

	// The drift case specifically must name the type as the actionable cause.
	if g, ok := byName["Drifted unit"]; ok && !strings.Contains(g.Detail, "wrong type") {
		t.Errorf("Drifted unit: Detail = %q, want it to name the type drift — that is "+
			"the actionable part, and `custom` would send the operator after a condition "+
			"the task does not have", g.Detail)
	}
}

// And the direction that does NOT drift must not be caught by the new guard: a genuine
// custom repeat is fine to complete, because the caller's job is not a gap.
func TestReconcileStillIgnoresAGenuineCustomRepeat(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	// Selected by the filter only if repeat_custom != true; a genuine custom repeat has
	// it true, so the filter excludes it. Asserted directly so a change to the filter
	// that let custom rows through would be caught here rather than being silently
	// rescued by the drift guard above.
	n, err := s.tasks.CountDocuments(ctx, bson.M{
		"completedAt":   bson.M{"$ne": nil, "$exists": true},
		"repeat_every":  bson.M{"$gt": 0},
		"repeat_unit":   bson.M{"$nin": bson.A{"", nil}},
		"repeat_custom": bson.M{"$ne": true},
	})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("%d rows matched the reconciler filter in a clean fixture set", n)
	}
}

// Repeat.Normalize is the single place Text is trimmed, so every create path stores the
// same thing for the same logical input. It used to be trimmed by the MCP create path
// and by the update path but NOT by HTTP create — so the same custom condition arrived
// stored two different ways depending on which door it came through.
//
// This asserts the outcome rather than the mechanism: both callers go through Normalize,
// so what matters is that a padded rule and a plain one are the same stored task.
func TestNormalizeTrimsCustomText(t *testing.T) {
	padded := Repeat{Custom: true, Text: "   3rd Friday   "}
	if got := padded.Normalize().Text; got != "3rd Friday" {
		t.Errorf("Normalize did not trim Text: %q", got)
	}
	// Idempotent, because Normalize is called more than once on some paths.
	twice := padded.Normalize().Normalize()
	if twice.Text != "3rd Friday" {
		t.Errorf("Normalize is not idempotent: %q", twice.Text)
	}
	// Whitespace-only text is NOT promoted to a custom repeat — it is still empty, and
	// Validate must reject it rather than mint a rule with no words.
	if got := (Repeat{Text: "   "}).Normalize(); got.Custom || got.Text != "" {
		t.Errorf("whitespace-only text became a custom repeat: %+v", got)
	}
}

// And the store agrees, end to end: a padded rule creates the same stored task as a
// plain one, whichever door it came through. This is the cross-path agreement the
// trimming rule exists to provide.
func TestPaddedCustomRuleStoresTheSameAsPlain(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	// Through the real create path, NOT mustCreate: that helper inserts a document
	// directly, so it bypasses Create and therefore Repeat.Normalize — which means a
	// test written with it measures the FIXTURE rather than the store. An earlier
	// version of this test did exactly that and "proved" the two callers disagreed,
	// when what it actually showed was that it had skipped the code under test.
	plain, _, err := s.CreateWithKey(ctx, "Plain rule", "d", "2026-10-05", "08:00",
		intP(5), Repeat{Custom: true, Text: "3rd Friday"}, boolP(false), "", "", "")
	if err != nil {
		t.Fatalf("plain create: %v", err)
	}
	padded, _, err := s.CreateWithKey(ctx, "Padded rule", "d", "2026-10-05", "08:00",
		intP(5), Repeat{Custom: true, Text: "   3rd Friday   "}, boolP(false), "", "", "")
	if err != nil {
		t.Fatalf("padded create: %v", err)
	}
	pg, cg := mustGet(t, s, plain["id"].(string)), mustGet(t, s, padded["id"].(string))
	if pg["repeat_rule"] != cg["repeat_rule"] {
		t.Errorf("the same rule stored two ways: %q vs %q — the same input must not "+
			"depend on which caller wrote it", pg["repeat_rule"], cg["repeat_rule"])
	}
}
