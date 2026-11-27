package healthcheck

import (
	"context"
	"os"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("MONGODB_URI") == "" {
		t.Skip("MONGODB_URI not set")
	}
	// Its OWN throwaway database, not the shared `miser_test`. `go test ./...` runs
	// packages in parallel and internal/money drops `miser_test` — sharing it made this suite fail.
	t.Setenv("MONGODB_DB", "healthcheck_test")
	s, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestFoodRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	day := "2026-09-01"
	dt, _ := time.Parse(time.RFC3339, "2026-09-01T12:30:00Z")

	if _, err := s.LogFood(ctx, day, dt, "  ", "", nil, ""); err == nil {
		t.Fatal("expected blank-what error")
	}
	if _, err := s.LogFood(ctx, day, dt, "rice", "", map[string]float64{"kcal": -1}, ""); err == nil {
		t.Fatal("expected negative-macro error")
	}
	if _, err := s.LogFood(ctx, day, dt, "rice", "", map[string]float64{"sugar": 1}, ""); err == nil {
		t.Fatal("expected unknown-macro error")
	}
	row, err := s.LogFood(ctx, day, dt, "rice", "1 bowl", map[string]float64{"kcal": 200, "protein": 4}, "lunch")
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	id, _ := row["_id"].(string)
	if id == "" {
		t.Fatalf("no _id: %v", row)
	}
	if row["what_lc"] != nil {
		t.Fatalf("what_lc must not leak: %v", row)
	}
	got, err := s.GetFood(ctx, id)
	if err != nil || got["what"] != "rice" {
		t.Fatalf("get %v %v", got, err)
	}
	if _, err := s.GetFood(ctx, "000000000000000000000000"); err == nil {
		t.Fatal("expected unknown-id error")
	}
	rows, err := s.ListFood(ctx, day, day, "", "", "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("list %v %v", rows, err)
	}
	rows, err = s.ListFood(ctx, day, day, "", "", "noodles")
	if err != nil || len(rows) != 0 {
		t.Fatalf("unmatched filter must be empty, got %v %v", rows, err)
	}
	if _, err := s.ListFood(ctx, day, day, "sugar", "", ""); err == nil {
		t.Fatal("expected unknown sort_by error")
	}
	if _, err := s.ListFood(ctx, day, day, "", "up", ""); err == nil {
		t.Fatal("expected unknown sort error")
	}
	qty := "2 bowls"
	patched, err := s.CorrectFood(ctx, id, nil, &qty, nil, map[string]*float64{"kcal": fptr(210)}, nil)
	if err != nil || patched["qty"] != "2 bowls" {
		t.Fatalf("correct %v %v", patched, err)
	}
	if _, err := s.CorrectFood(ctx, id, nil, nil, nil, nil, nil); err == nil {
		t.Fatal("expected empty-patch error")
	}
	dry, err := s.DeleteFood(ctx, id, true)
	if err != nil || dry["would_delete"] != 1 {
		t.Fatalf("dry run %v %v", dry, err)
	}
	if rows, _ := s.ListFood(ctx, day, day, "", "", ""); len(rows) != 1 {
		t.Fatal("dry run must not delete")
	}
	live, err := s.DeleteFood(ctx, id, false)
	if err != nil || live["deleted"] != 1 {
		t.Fatalf("live delete %v %v", live, err)
	}
	if _, err := s.DeleteFood(ctx, id, false); err == nil {
		t.Fatal("expected unknown-id error on second delete")
	}
	if _, err := s.ListFood(ctx, "bad-day", day, "", "", ""); err == nil {
		t.Fatal("expected bad-date error")
	}
}

func TestWeightRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	day := "2026-09-02"

	if _, err := s.LogWeight(ctx, day, 0); err == nil {
		t.Fatal("expected implausible error")
	}
	if _, err := s.LogWeight(ctx, day, 70.5); err != nil {
		t.Fatalf("weight: %v", err)
	}
	// Upsert is the correction: re-log overwrites.
	if _, err := s.LogWeight(ctx, day, 71); err != nil {
		t.Fatalf("re-log: %v", err)
	}
	rows, err := s.ListWeight(ctx, day, day, "", "")
	if err != nil || len(rows) != 1 || rows[0]["kg"] != 71.0 {
		t.Fatalf("list %v %v", rows, err)
	}
	dry, err := s.DeleteWeight(ctx, day, true)
	if err != nil || dry["would_delete"] != 1 {
		t.Fatalf("dry run %v %v", dry, err)
	}
	live, err := s.DeleteWeight(ctx, day, false)
	if err != nil || live["deleted"] != 1 {
		t.Fatalf("live delete %v %v", live, err)
	}
	// Valid date, no row: empty, not error.
	again, err := s.DeleteWeight(ctx, day, false)
	if err != nil || again["deleted"] != 0 {
		t.Fatalf("repeat delete %v %v", again, err)
	}
}

func TestSleepWorkoutDedup(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	day := "2026-09-03"

	if _, err := s.LogSleep(ctx, day, 0); err == nil {
		t.Fatal("expected implausible error")
	}
	if _, err := s.LogSleep(ctx, day, 7.5); err != nil {
		t.Fatalf("sleep: %v", err)
	}
	// Same manual entry twice: duplicate, not a second session.
	if _, err := s.LogSleep(ctx, day, 7.5); err == nil {
		t.Fatal("expected duplicate-sleep error")
	}
	days, err := s.ListSleep(ctx, day, day, "", "")
	if err != nil || len(days) != 1 || days[0]["sleep_hours"] != 7.5 {
		t.Fatalf("list %v %v", days, err)
	}
	sessArr, _ := days[0]["sessions"].([]bson.M)
	if len(sessArr) != 1 {
		t.Fatalf("sessions %v", days[0]["sessions"])
	}
	sessID, _ := sessArr[0]["id"].(string)

	if _, err := s.CorrectSleep(ctx, sessID, 8*60); err != nil {
		t.Fatalf("correct: %v", err)
	}
	days, _ = s.ListSleep(ctx, day, day, "", "")
	if days[0]["sleep_hours"] != 8.0 {
		t.Fatalf("hours %v", days[0]["sleep_hours"])
	}
	if _, err := s.CorrectSleep(ctx, "nonexistent", 60); err == nil {
		t.Fatal("expected unknown-id error")
	}
	dry, err := s.DeleteSleep(ctx, sessID, true)
	if err != nil || dry["would_delete"] != 1 {
		t.Fatalf("dry run %v %v", dry, err)
	}
	if live, err := s.DeleteSleep(ctx, sessID, false); err != nil || live["deleted"] != 1 {
		t.Fatalf("live delete %v %v", live, err)
	}

	// Workouts: manual log, type normalized, duplicate rejected.
	if _, err := s.LogWorkout(ctx, day, "Run", 30, 300); err != nil {
		t.Fatalf("workout: %v", err)
	}
	if _, err := s.LogWorkout(ctx, day, "run", 30, 300); err == nil {
		t.Fatal("expected duplicate-workout error (lowered type)")
	}
	wodays, err := s.ListWorkouts(ctx, day, day, "", "", "RUN")
	if err != nil || len(wodays) != 1 {
		t.Fatalf("type filter %v %v", wodays, err)
	}
	wodays, err = s.ListWorkouts(ctx, day, day, "", "", "swim")
	if err != nil || len(wodays) != 0 {
		t.Fatalf("unmatched type must be empty, got %v %v", wodays, err)
	}
	allDays, _ := s.ListWorkouts(ctx, day, day, "", "", "")
	first := allDays[0]["workouts"].([]bson.M)[0]
	woid, _ := first["id"].(string)
	if _, err := s.CorrectWorkout(ctx, woid, nil, nil, nil); err == nil {
		t.Fatal("expected empty-patch error")
	}
	if _, err := s.CorrectWorkout(ctx, woid, strptr("lift"), nil, nil); err != nil {
		t.Fatalf("correct: %v", err)
	}
	if _, err := s.CorrectWorkout(ctx, "nonexistent", nil, nil, nil); err == nil {
		t.Fatal("expected unknown-id error")
	}
	if _, err := s.DeleteWorkout(ctx, woid, false); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Sync path: steps overwrite, sessions dedup quietly, updatedAt is Date.
	steps := 1000
	kcal := 50.5
	start, _ := time.Parse(time.RFC3339, "2026-09-03T22:00:00Z")
	rec := SyncRecord{Date: day, Steps: &steps, ActiveKcal: &kcal,
		Sleeps:   []SleepIn{{Start: &start, Minutes: 400}},
		Workouts: []WorkoutIn{{Type: "cycle", Minutes: 20, Kcal: 150}}}
	res, err := s.RecordSync(ctx, rec)
	if err != nil || res.SessionsAdded != 1 || res.WorkoutsAdded != 1 {
		t.Fatalf("sync %v %v", res, err)
	}
	res2, err := s.RecordSync(ctx, rec)
	if err != nil || res2.SessionsSkipped != 1 || res2.WorkoutsSkipped != 1 {
		t.Fatalf("resync %v %v", res2, err)
	}
	sum, err := s.DailySummary(ctx, day)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if fval(sum["steps"]) != 1000 || sum["sleep_hours"] != 6.7 {
		t.Fatalf("summary %v", sum)
	}
	if _, ok := sum["net"]; ok {
		t.Fatalf("no single net: %v", sum)
	}
	if sum["workout_kcal"] != 150.0 || sum["active_kcal"] != 50.5 {
		t.Fatalf("split cal-out %v", sum)
	}

	// Cleanup rows created by this test.
	if _, err := s.DeleteWeight(ctx, day, false); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		col *mongoDrv.Collection
		key string
	}{
		{s.days, "date"}, {s.sleep, "day"}, {s.workouts, "day"}, {s.food, "day"},
	} {
		if _, err := c.col.DeleteMany(ctx, bson.M{c.key: day}); err != nil {
			t.Fatal(err)
		}
	}
}

func fptr(f float64) *float64 { return &f }

func strptr(s string) *string { return &s }
