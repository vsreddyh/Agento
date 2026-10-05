package tasks

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// #209: there was no verb for "not doing this occurrence", so an agent asked to
// skip five chores recorded five completions for work that never happened — and
// because completed rows are TTL-deleted, the false history was erased rather than
// corrected. These tests pin the distinction that makes a skip a skip.

func TestSkipIsNotComplete(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	id := mustCreate(t, s, "Skip me", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
	})
	doc, next, rollover, err := s.Skip(ctx, id, "out of time")
	if err != nil {
		t.Fatalf("Skip: %v", err)
	}

	// The headline: the record says SKIPPED, not done. Everything else here is
	// bookkeeping; this is the fix.
	if doc["skipped"] != true {
		t.Errorf("skipped = %v, want true", doc["skipped"])
	}
	if doc["skipReason"] != "out of time" {
		t.Errorf("skipReason = %q, want the user's words verbatim", doc["skipReason"])
	}
	// completedAt IS set, deliberately: the occurrence is resolved and must leave
	// the open list, or the user is asked again tonight. It is skippedAt that
	// distinguishes it from a completion.
	if doc["completedAt"] == nil {
		t.Error("a skipped occurrence must leave the open list (completedAt set)")
	}
	if doc["skippedAt"] == nil {
		t.Error("skippedAt must be set — it is the marker that makes this a skip")
	}

	// A one-shot has no next occurrence; that is not a fault.
	if next != nil {
		t.Errorf("a one-shot skip minted a successor: %v", next)
	}
	if rollover != RolloverNone {
		t.Errorf("rollover = %q, want %q", rollover, RolloverNone)
	}

	// Open list must not contain it any more.
	rows, _, err := s.List(ctx, "open", false, "", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, r := range rows {
		if r["id"] == id {
			t.Error("a skipped task must not stay on the open list")
		}
	}
	// …but the done list must, and must show it as skipped.
	done, _, err := s.List(ctx, "done", false, "", 0)
	if err != nil {
		t.Fatalf("list done: %v", err)
	}
	found := false
	for _, r := range done {
		if r["id"] == id {
			found = true
			if r["skipped"] != true {
				t.Error("a task in the done list that was actually skipped must say skipped")
			}
		}
	}
	if !found {
		t.Error("a skipped task must be visible in the done list, not vanish")
	}
}

// Skipping an occurrence must not abandon the series. That is what delete is for.
func TestSkipStillAdvancesAStructuredRepeat(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	id := mustCreate(t, s, "Skip repeat", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
		"repeat_every": 1, "repeat_unit": "days",
	})
	_, next, rollover, err := s.Skip(ctx, id, "")
	if err != nil {
		t.Fatalf("Skip: %v", err)
	}
	if rollover != RolloverCreated || next == nil {
		t.Fatalf("rollover = %q, next = %v; a skipped occurrence still advances the cadence", rollover, next)
	}
	if next["due_date"] == "2026-10-05" {
		t.Error("the successor must carry the NEXT due date, not the skipped one")
	}
	// And the successor must be open and unskipped.
	if next["skipped"] != false {
		t.Errorf("the successor is marked skipped: %v", next["skipped"])
	}
}

// Skipping twice must not double-record, and the error must distinguish "already
// skipped" from "already done" — they need different remedies.
func TestSkipTwiceIsRefusedDistinctly(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	skippedID := mustCreate(t, s, "Skip twice", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
	})
	doneID := mustCreate(t, s, "Done then skip", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
	})
	if _, _, _, err := s.Complete(ctx, doneID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if _, _, _, err := s.Skip(ctx, skippedID, ""); err != nil {
		t.Fatalf("first skip: %v", err)
	}
	_, _, _, err := s.Skip(ctx, skippedID, "")
	if err == nil {
		t.Fatal("skipping twice must be refused")
	}
	if !strings.Contains(err.Error(), "already skipped") {
		t.Errorf("error = %q, want it to say the task is already skipped", err)
	}

	_, _, _, err = s.Skip(ctx, doneID, "")
	if err == nil {
		t.Fatal("skipping a completed task must be refused")
	}
	if !strings.Contains(err.Error(), "already completed") {
		t.Errorf("error = %q, want it to say the task is already completed", err)
	}
}

// Reopening a skip must put the task back on the OPEN list, not mark it done. That
// is the whole reason skippedAt is separate from completedAt.
func TestReopenASkippedTaskMakesItOpenAgain(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	id := mustCreate(t, s, "Skip then reopen", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
	})
	if _, _, _, err := s.Skip(ctx, id, "later"); err != nil {
		t.Fatalf("skip: %v", err)
	}
	reopened, err := s.Reopen(ctx, id)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if reopened["completedAt"] != nil {
		t.Error("a reopened task must be open, not completed")
	}
	if reopened["skipped"] != false {
		t.Error("reopening must clear the skip marker")
	}
	if reopened["skippedAt"] != nil || reopened["skipReason"] != nil {
		t.Errorf("reopen left the skip marker behind: %v", reopened)
	}
	rows, _, err := s.List(ctx, "open", false, "", 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, r := range rows {
		if r["id"] == id {
			found = true
		}
	}
	if !found {
		t.Error("a reopened skip must be back on the open list")
	}
}

// The reason is free text typed by voice: it must be capped, and the cap must not
// split a rune.
func TestSkipReasonIsCappedRuneSafely(t *testing.T) {
	if got := truncSkipReason(strings.Repeat("a", 500)); len(got) != 200 {
		t.Errorf("ascii reason capped to %d bytes, want 200", len(got))
	}
	emoji := truncSkipReason(strings.Repeat("🥑", 500))
	if !utf8.ValidString(emoji) {
		t.Error("the cap split a rune — the stored reason is not valid UTF-8")
	}
	if len(emoji) > 200 {
		t.Errorf("capped reason is %d bytes, over the cap", len(emoji))
	}
	if got := truncSkipReason(""); got != "" {
		t.Errorf("an absent reason must stay empty, got %q", got)
	}
	if got := truncSkipReason("out of time"); got != "out of time" {
		t.Errorf("a short reason must be kept verbatim, got %q", got)
	}
}

// A task that was never skipped must report skipped:false rather than omitting the
// key, so a client reading `skipped` never has to distinguish absent from false.
func TestSkippedIsAlwaysPresent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	id := mustCreate(t, s, "Not skipped", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
	})
	doc := mustGet(t, s, id)
	if v, ok := doc["skipped"]; !ok {
		t.Error("an untouched task must still carry skipped")
	} else if v != false {
		t.Errorf("skipped = %v, want false", v)
	}
	completed, _, _, err := s.Complete(ctx, id)
	if err != nil {
		t.Fatalf("complete: %v", err)
	}
	if completed["skipped"] != false {
		t.Error("a COMPLETED task must report skipped:false — that is the false record this fixes")
	}
}

// Completing an occurrence that was SKIPPED must not report it as done. That is a
// false record of work that never happened — the exact failure #209 exists to stop,
// reached from the other direction: Skip distinguished its two terminal states while
// Complete did not, so the two paths disagreed about what a resolved task is.
func TestCompleteOnASkippedTaskReportsSkippedNotDone(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	id := mustCreate(t, s, "Skipped then completed", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
	})
	if _, _, _, err := s.Skip(ctx, id, "out of time"); err != nil {
		t.Fatalf("skip: %v", err)
	}
	_, _, _, err := s.Complete(ctx, id)
	if err == nil {
		t.Fatal("completing an already-resolved task must fail")
	}
	if !strings.Contains(err.Error(), "already skipped") {
		t.Errorf("error = %q; it must name the task as skipped, not report it as completed", err)
	}
	if !strings.Contains(err.Error(), "not done") {
		t.Errorf("error = %q does not distinguish skipped from done in words", err)
	}
	if strings.Contains(err.Error(), "already completed") {
		t.Errorf("error = %q reports a skipped occurrence as completed", err)
	}
	// And the remedy has to name the right verb: reopening a skipped occurrence is
	// what returns it to the open list, not completing it again.
	if !strings.Contains(err.Error(), "reopen_task") {
		t.Errorf("error = %q does not point at the verb that can fix it", err)
	}
}

// The same task, completed for real first, must still say "completed" — the fix must
// not swing the message the other way and invent a skip that never happened.
func TestCompleteOnACompletedTaskStillSaysCompleted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	id := mustCreate(t, s, "Completed twice", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
	})
	if _, _, _, err := s.Complete(ctx, id); err != nil {
		t.Fatalf("complete: %v", err)
	}
	_, _, _, err := s.Complete(ctx, id)
	if err == nil {
		t.Fatal("completing twice must fail")
	}
	if !strings.Contains(err.Error(), "already completed") {
		t.Errorf("error = %q, want it to say already completed", err)
	}
	if strings.Contains(err.Error(), "SKIPPED") {
		t.Errorf("error = %q claims a skip that never happened", err)
	}
}

// The symmetry, from the other direction: skipping an already-COMPLETED task must not
// claim it was skipped. Same helper, so this is now true by construction — the test
// is what makes "by construction" checkable rather than merely asserted.
func TestSkipOnACompletedTaskStillSaysCompleted(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	id := mustCreate(t, s, "Completed then skipped", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
	})
	if _, _, _, err := s.Complete(ctx, id); err != nil {
		t.Fatalf("complete: %v", err)
	}
	_, _, _, err := s.Skip(ctx, id, "changed my mind")
	if err == nil {
		t.Fatal("skipping an already-resolved task must fail")
	}
	if !strings.Contains(err.Error(), "already completed") {
		t.Errorf("error = %q, want it to say already completed", err)
	}
}

// An unknown id is still "unknown", from BOTH verbs — the shared fetch must not have
// turned a missing task into a "already completed" claim.
func TestResolveVerbsStillReportUnknownTasks(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	oid := primitive.NewObjectID().Hex()
	if _, _, _, err := s.Complete(ctx, oid); err == nil ||
		!strings.Contains(err.Error(), "unknown task") {
		t.Errorf("complete on an unknown id: %v, want an unknown-task error", err)
	}
	if _, _, _, err := s.Skip(ctx, oid, "r"); err == nil ||
		!strings.Contains(err.Error(), "unknown task") {
		t.Errorf("skip on an unknown id: %v, want an unknown-task error", err)
	}
}

// "Was this skipped?" must be ONE rule, not two. It used to be written twice: toDoc
// derived the response boolean from `skippedAt != nil`, while the miss-path error
// required a non-zero primitive.DateTime. Any row where the field is present but is not
// a real timestamp — a zero DateTime, or another type entirely — therefore read as
// `skipped: true` in every response AND produced "already completed" when you tried to
// act on it. Such a row cannot be written through the API, which is exactly why the
// divergence survived.
func TestIsSkippedIsTheSameRuleEverywhere(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	// A real skip: both agree it is skipped, and the doc says so.
	real := mustCreate(t, s, "Real skip", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
	})
	if _, _, _, err := s.Skip(ctx, real, "out of time"); err != nil {
		t.Fatalf("skip: %v", err)
	}
	if !isSkipped(mustGet(t, s, real)) {
		t.Error("isSkipped says a genuinely skipped task is not skipped")
	}
	if got := mustGet(t, s, real)["skipped"]; got != true {
		t.Errorf("response says skipped=%v for a skipped task", got)
	}
	if _, _, _, err := s.Complete(ctx, real); err == nil ||
		!strings.Contains(err.Error(), "already skipped") {
		t.Errorf("completing a skipped task: %v, want the ALREADY SKIPPED error", err)
	}

	// The divergent shapes: skippedAt present, but not a real timestamp.
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"zero DateTime", primitive.DateTime(0)},
		{"string", "2026-10-05"},
	} {
		id := mustCreate(t, s, "Drifted "+tc.name, map[string]any{
			"due_date": "2026-10-05", "due_time": "08:00",
		})
		if _, err := s.tasks.UpdateOne(ctx, bson.M{"_id": mustObjectID(t, id)},
			bson.M{"$set": bson.M{"skippedAt": tc.value}}); err != nil {
			t.Fatalf("set skippedAt: %v", err)
		}
		doc := mustGet(t, s, id)
		derived, _ := doc["skipped"].(bool)
		byRule := isSkipped(doc)
		if derived != byRule {
			t.Errorf("%s: the response boolean is %v but isSkipped says %v — the two "+
				"must not disagree, or the task reads as skipped and then errors as "+
				"completed", tc.name, derived, byRule)
		}
	}
}

// The trap in the obvious fix. Unifying on "assert primitive.DateTime and compare to
// zero" is correct for a STORED doc and silently wrong for a RENDERED one, because
// toDoc renders dates to ISO strings for the API. Every response would then carry
// `skipped: false` even for a genuinely skipped occurrence — no compile error, no
// failing test, and a client filtering on the flag would quietly see no skips at all.
//
// So this pins BOTH representations against the same rule.
func TestIsSkippedHandlesBothRepresentations(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want bool
	}{
		{"stored, real timestamp", primitive.DateTime(1791197697928), true},
		{"stored, zero DateTime", primitive.DateTime(0), false},
		{"stored, Reopen's clearing value", "", false},
		{"stored, absent", nil, false},
		{"rendered ISO string", "2026-10-05T10:54:57Z", true},
		{"rendered empty string", "", false},
		{"a type that is neither", int64(5), false},
	}
	for _, c := range cases {
		doc := map[string]any{}
		if c.val != nil {
			doc["skippedAt"] = c.val
		}
		if got := isSkipped(doc); got != c.want {
			t.Errorf("%s: isSkipped = %v, want %v", c.name, got, c.want)
		}
	}
}

// And the end-to-end consequence: a skipped task must actually report skipped=true
// through the API, which is what the representation bug would have broken.
func TestSkippedTaskReportsSkippedTrueThroughTheApi(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	id := mustCreate(t, s, "Api shape", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
	})
	if _, _, _, err := s.Skip(ctx, id, "why"); err != nil {
		t.Fatalf("skip: %v", err)
	}
	if got := mustGet(t, s, id)["skipped"]; got != true {
		t.Errorf("a skipped task reports skipped=%v through the API", got)
	}
	// And after reopening it must flip back, or the flag is stuck.
	if _, err := s.Reopen(ctx, id); err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := mustGet(t, s, id)["skipped"]; got != false {
		t.Errorf("after reopen, skipped=%v, want false", got)
	}
}

func mustObjectID(t *testing.T, id string) primitive.ObjectID {
	t.Helper()
	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		t.Fatalf("bad id %q: %v", id, err)
	}
	return oid
}

// The store half of the invariant the audit guard relies on: a field set that resolves
// to nothing really does leave the revision alone.
//
// The handler suppresses its audit entry when ChangedFieldNames reports "no fields", on
// the reasoning that this is exactly when Update takes its no-op path. That reasoning is
// only sound because of what this test pins — if Update began bumping the revision on a
// no-op, the handler would be dropping audit entries for real writes instead, silently.
func TestNoOpUpdateDoesNotBumpRevision(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupFixtures(t, s, ctx)

	id := mustCreate(t, s, "No-op update", map[string]any{
		"due_date": "2026-10-05", "due_time": "08:00",
	})
	before := mustGet(t, s, id)["revision"]

	// The shapes that resolve to an empty `set`: nothing at all, and the concurrency
	// guard on its own. A JSON null resolves to nothing too, since every field read in
	// the store treats null as absent.
	for name, fields := range map[string]map[string]any{
		"empty":            {},
		"guard only":       {"expected_revision": 0},
		"null field":       {"name": nil},
		"guard and a null": {"expected_revision": 0, "description": nil},
	} {
		doc, err := s.Update(ctx, id, fields)
		if err != nil {
			t.Fatalf("%s: update: %v", name, err)
		}
		if doc["revision"] != before {
			t.Errorf("%s: revision moved %v -> %v. A no-op that bumps the revision "+
				"means the audit guard's assumption is wrong — it would now suppress "+
				"entries for real writes", name, before, doc["revision"])
		}
		if got := ChangedFieldNames(fields); got != "no fields" {
			t.Errorf("%s: ChangedFieldNames = %q, so the handler would suppress an "+
				"entry for a write that DID happen", name, got)
		}
	}

	// The control: a real change must bump the revision, or none of the above means
	// anything.
	doc, err := s.Update(ctx, id, map[string]any{"name": "actually changed"})
	if err != nil {
		t.Fatalf("real update: %v", err)
	}
	if doc["revision"] == before {
		t.Error("a real update did not bump the revision, so the no-op assertions " +
			"above are vacuous")
	}
}
