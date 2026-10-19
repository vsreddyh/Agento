package tasks

// Change marker tests (#178).
//
// These assert the property the app depends on, not the implementation: after
// any write, the revision a poller holds is stale — including after a write
// this package makes on its own behalf (the rollover successor), which is the
// case two earlier tracking mechanisms missed.

import (
	"context"
	"testing"
)

func TestChangeMarkerAdvancesOnEveryWrite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// A fresh database has no marker: revision 0, no time.
	if rev, at, err := s.ChangeRevision(ctx); err != nil || rev != 0 || !at.IsZero() {
		t.Fatalf("fresh marker: rev=%d at=%v err=%v", rev, at, err)
	}
	first, err := s.Create(ctx, "Marker probe", "probe for #178", "2026-10-12", "09:00", intP(10), Repeat{}, boolP(false), "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id, _ := first["id"].(string)

	rev, at, err := s.ChangeRevision(ctx)
	if err != nil || rev != 1 || at.IsZero() {
		t.Fatalf("after create: rev=%d at_zero=%v err=%v", rev, at.IsZero(), err)
	}

	// Each subsequent write advances it by exactly one.
	for i, step := range []struct {
		name string
		run  func() error
	}{
		{"update", func() error {
			_, err := s.Update(ctx, id, map[string]any{"name": "Marker probe v2"})
			return err
		}},
		{"complete", func() error {
			_, _, _, _, err := s.CompleteDetail(ctx, id)
			return err
		}},
		{"reopen", func() error {
			_, err := s.Reopen(ctx, id)
			return err
		}},
	} {
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		want := int64(i + 2)
		if rev, _, err := s.ChangeRevision(ctx); err != nil || rev != want {
			t.Fatalf("after %s: rev=%d want=%d err=%v", step.name, rev, want, err)
		}
	}
}

// The rollover successor is written by insert inside Complete, which is
// precisely where the audit log lost it (#180). A completion therefore bumps
// twice — once for the completion, once for the successor it minted.
func TestChangeMarkerCountsRolloverSuccessor(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	doc, err := s.Create(ctx, "Weekly standup", "repeats every week", "2026-10-12", "09:00",
		intP(30), structuredRep(1, "weeks"), boolP(false), "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id, _ := doc["id"].(string)
	before, _, _ := s.ChangeRevision(ctx)

	if _, _, _, _, err := s.CompleteDetail(ctx, id); err != nil {
		t.Fatalf("complete: %v", err)
	}
	after, _, err := s.ChangeRevision(ctx)
	if err != nil {
		t.Fatalf("ChangeRevision: %v", err)
	}
	if after != before+2 {
		t.Fatalf("rollover successor not counted: before=%d after=%d", before, after)
	}
}

// A write that changed nothing must not move the marker: bumping for a
// no-op would wake every poller for a screen that is already correct.
func TestChangeMarkerIgnoresNoOpWrites(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	doc, err := s.Create(ctx, "No-op probe", "probe for #178 no-ops", "2026-10-12", "09:00", intP(10), Repeat{}, boolP(false), "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id, _ := doc["id"].(string)

	// Deleting a task that is already gone matched nothing.
	if deleted, err := s.Delete(ctx, "000000000000000000000000"); err != nil || deleted {
		t.Fatalf("delete unknown: deleted=%v err=%v", deleted, err)
	}
	// An empty patch takes Update's no-op path.
	if _, err := s.Update(ctx, id, map[string]any{}); err != nil {
		t.Fatalf("empty update: %v", err)
	}
	after, _, err := s.ChangeRevision(ctx)
	if err != nil {
		t.Fatalf("ChangeRevision: %v", err)
	}
	if after != 1 {
		t.Fatalf("no-op writes moved the marker: rev=%d want=1", after)
	}
	// And the real delete still does.
	if deleted, err := s.Delete(ctx, id); err != nil || !deleted {
		t.Fatalf("delete: deleted=%v err=%v", deleted, err)
	}
	if rev, _, _ := s.ChangeRevision(ctx); rev != 2 {
		t.Fatalf("delete did not bump: rev=%d want=2", rev)
	}
}
