package tasks

import (
	"context"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// The scan cap (#180). The reconciler reads the most recent ReconcileLimit
// completions and fetches one EXTRA row purely to detect that more exist.
//
// That probe row is where the arithmetic went wrong: `Scanned` was assigned from
// the pre-truncation length, so a capped run reported 501 against a limit of 500.
// That is the one number an operator reads to judge whether the night's report is
// complete, so being wrong by one in the truncated case — the only case where
// completeness is in question — is worse than it looks.

// buildCompletedRepeats bulk-inserts n completed structured repeats. InsertMany
// rather than mustCreate n times: this is the one test that needs hundreds of
// rows, and a per-insert round trip would dominate the suite's runtime.
func buildCompletedRepeats(t *testing.T, s *Store, n int) {
	t.Helper()
	base := time.Now().UTC()
	rows := make([]any, 0, n)
	for i := 0; i < n; i++ {
		at := primitive.NewDateTimeFromTime(base.Add(-time.Duration(i) * time.Minute))
		rows = append(rows, bson.M{
			// A name prefix, so cleanup is a prefix delete rather than a 500-entry
			// $in list that would have to be rebuilt on every run.
			"name":              bulkFixtureName,
			"description":       "d",
			"estimated_minutes": 5,
			"parallelable":      false,
			"revision":          0,
			"createdAt":         at,
			"completedAt":       at,
			"due_date":          "2026-10-05",
			"due_time":          "", // blank due_time => RolloverFailed, so each is a gap
			"repeat_every":      1,
			"repeat_unit":       "days",
			"repeat_custom":     false,
		})
	}
	if _, err := s.tasks.InsertMany(context.Background(), rows); err != nil {
		t.Fatalf("bulk insert: %v", err)
	}
}

const bulkFixtureName = "BULKSCAN fixture"

// A capped run must report the number of rows it ACTUALLY EXAMINED, never the
// number it fetched. The distinction is only observable past the cap, which is
// why the fixtures are ReconcileLimit+1.
func TestReconcileRolloverScannedExcludesTheProbeRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupBulkFixtures(t, s, ctx)

	buildCompletedRepeats(t, s, ReconcileLimit+1)

	res, err := s.ReconcileRollover(ctx)
	if err != nil {
		t.Fatalf("ReconcileRollover: %v", err)
	}
	if !res.Truncated {
		t.Fatalf("%d fixtures must trip the scan cap", ReconcileLimit+1)
	}
	if res.Scanned != ReconcileLimit {
		t.Errorf("Scanned = %d, want %d — the +1 probe row must not be counted as "+
			"examined (it was fetched but never inspected)", res.Scanned, ReconcileLimit)
	}
	// And the invariant, stated so it holds for an UNCAPPED run too.
	if res.Scanned > ReconcileLimit {
		t.Errorf("Scanned = %d exceeds the limit of %d", res.Scanned, ReconcileLimit)
	}
}

// Below the cap, Scanned counts every eligible row and nothing is truncated. The
// pairing matters: a fix that simply clamped Scanned to the limit would pass the
// test above and wrongly report a small run as truncated-adjacent, so the
// under-cap case is asserted as its own fact.
func TestReconcileRolloverScannedCountsEveryRowWhenUncapped(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	defer cleanupBulkFixtures(t, s, ctx)

	const n = 5
	buildCompletedRepeats(t, s, n)

	res, err := s.ReconcileRollover(ctx)
	if err != nil {
		t.Fatalf("ReconcileRollover: %v", err)
	}
	if res.Truncated {
		t.Errorf("%d fixtures tripped the cap of %d", n, ReconcileLimit)
	}
	if res.Scanned < n {
		t.Errorf("Scanned = %d, want at least %d", res.Scanned, n)
	}
	if res.Scanned > ReconcileLimit {
		t.Errorf("Scanned = %d exceeds the limit even uncapped", res.Scanned)
	}
	// Every fixture has a blank due_time, so each must be reported — a run that
	// scanned rows without examining them would return none.
	if len(res.Gaps) < n {
		t.Errorf("gaps = %d, want at least %d — the scanned rows were not examined",
			len(res.Gaps), n)
	}
}

func cleanupBulkFixtures(t *testing.T, s *Store, ctx context.Context) {
	t.Helper()
	if _, err := s.tasks.DeleteMany(ctx, bson.M{"name": bulkFixtureName}); err != nil {
		t.Errorf("cleanup bulk fixtures: %v", err)
	}
	// Belt and braces: this fixture is deliberately a completed structured repeat,
	// so if cleanup ever silently fails the NEXT test in the package inherits 500
	// phantom gaps. Say so loudly rather than letting it present as a real failure.
	if n, err := s.tasks.CountDocuments(ctx, bson.M{"name": bulkFixtureName}); err == nil && n > 0 {
		t.Errorf("%d bulk fixtures survived cleanup — later tests will see phantom gaps", n)
	}
}
