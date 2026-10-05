package healthcheck

import (
	"context"
	"os"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("MONGODB_URI") == "" {
		t.Skip("MONGODB_URI not set")
	}
	// Its OWN throwaway database, not the shared `miser_test`. `go test ./...` runs
	// packages in parallel and internal/money calls db.Drop(ctx) on `miser_test` — a
	// wholesale drop, not a scoped delete. Sharing it made this suite fail whenever it
	// happened to run alongside money: the collection was dropped between a write and
	// the read that asserted it, and the test reported empty results with no error
	// anywhere. A test that fails based on which package the scheduler paired it with is
	// not a test. internal/tasks, internal/projects and internal/notes already have
	// their own; this closes the last two.
	t.Setenv("MONGODB_DB", "healthcheck_test")
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestMealWeightRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	day := "2026-09-01"

	if _, err := s.LogMeal(ctx, day, "test", nil); err == nil {
		t.Fatal("expected empty-items error")
	}
	meal, err := s.LogMeal(ctx, day, "lunch", []map[string]any{
		{"name": "rice", "kcal": 200.0, "protein": 4.0, "carbs": 40.0, "fat": 1.0, "fiber": 2.0},
	})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	if meal["date"] != day {
		t.Fatalf("got %v", meal)
	}
	rows, err := s.QueryMeals(ctx, day, day)
	if err != nil || len(rows) != 1 {
		t.Fatalf("got %v %v", rows, err)
	}
	if _, err := s.LogWeight(ctx, day, 70.5); err != nil {
		t.Fatalf("weight: %v", err)
	}
	if _, err := s.LogWeight(ctx, day, 0); err == nil {
		t.Fatal("expected implausible error")
	}
	if _, err := s.LogSleep(ctx, day, 7.5); err != nil {
		t.Fatalf("sleep: %v", err)
	}
	if _, err := s.LogWorkout(ctx, day, "run", 30, 300); err != nil {
		t.Fatalf("workout: %v", err)
	}
	sum, err := s.DailySummary(ctx, day)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if sum["meals"] != 1 || sum["cal_in"] != 200.0 {
		t.Fatalf("got %v", sum)
	}
	if n, err := s.DeleteMeals(ctx, day); err != nil || n != 1 {
		t.Fatalf("got %d %v", n, err)
	}
	// Cleanup weight/day rows created by this test.
	if _, err := s.weight.DeleteMany(ctx, map[string]any{"date": day}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.days.DeleteMany(ctx, map[string]any{"date": day}); err != nil {
		t.Fatal(err)
	}
}
