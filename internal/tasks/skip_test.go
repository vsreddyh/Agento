package tasks

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
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
