package tasks

import (
	"context"
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
		if !r.NeedsAttention() {
			t.Error("a custom repeat needs the caller's attention")
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
		if got := mustGet(t, s, next["id"].(string))[rolledFromField]; got != id {
			t.Errorf("successor's %s = %v, want the parent's id %q", rolledFromField, got, id)
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

	gaps, err := s.ReconcileRollover(ctx)
	if err != nil {
		t.Fatalf("ReconcileRollover: %v", err)
	}
	seen := map[string]Rollover{}
	for _, g := range gaps {
		seen[g.TaskID] = g.Why
	}
	if _, ok := seen[goodID]; ok {
		t.Error("a task that rolled over correctly must not be reported as a gap")
	}
	if got, ok := seen[badID]; !ok {
		t.Errorf("the stopped repeat was NOT reported; gaps were %+v", gaps)
	} else if got != RolloverFailed {
		t.Errorf("gap reason = %q, want %q", got, RolloverFailed)
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
	gaps, err := s.ReconcileRollover(ctx)
	if err != nil {
		t.Fatalf("ReconcileRollover: %v", err)
	}
	for _, g := range gaps {
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
func TestRolloverUserFacingOnlyWhenSomethingIsWrong(t *testing.T) {
	for _, r := range []Rollover{RolloverCreated, RolloverNone} {
		if r.UserFacing() != "" {
			t.Errorf("%q must not explain itself, got %q", r, r.UserFacing())
		}
		if r.NeedsAttention() {
			t.Errorf("%q must not demand attention", r)
		}
	}
	for _, r := range []Rollover{RolloverCustom, RolloverExhausted, RolloverFailed} {
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

// ── fixtures ──────────────────────────────────────────────────────────────

// fixtureNames are the tasks these tests create. Cleanup removes exactly these and
// nothing else, because the suite shares a throwaway database with the other task
// tests and must not delete their rows.
var fixtureNames = []string{
	"One shot", "Custom", "Structured", "Legacy broken",
	"Good", "Bad", "Custom done", "One shot done",
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
