package healthcheck

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"agento/internal/mongo"

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
	cleanDay(t, s, ctx, day)
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
	id, _ := row["id"].(string)
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
	rows, total, err := s.ListFood(ctx, day, day, "", "", "")
	if err != nil || len(rows) != 1 || total != 1 {
		t.Fatalf("list %v total=%d %v", rows, err, total)
	}
	// Round-trip: the listed datetime feeds straight back into correct.
	dtStr, _ := rows[0]["datetime"].(string)
	dtRT, err := time.Parse(time.RFC3339Nano, dtStr)
	if err != nil {
		t.Fatalf("listed datetime %q does not parse: %v", dtStr, err)
	}
	if _, err := s.CorrectFood(ctx, id, nil, nil, nil, nil, nil, &dtRT); err != nil {
		t.Fatalf("round-trip correct %v", err)
	}
	// Desc comes back newest-first.
	rows, _, err = s.ListFood(ctx, day, day, "", "desc", "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("desc list %v %v", rows, err)
	}
	rows, total, err = s.ListFood(ctx, day, day, "", "", "noodles")
	if err != nil || len(rows) != 0 {
		t.Fatalf("unmatched filter must be empty, got %v %v", rows, err)
	}
	if _, _, err := s.ListFood(ctx, day, day, "sugar", "", ""); err == nil {
		t.Fatal("expected unknown sort_by error")
	}
	if _, _, err := s.ListFood(ctx, day, day, "", "up", ""); err == nil {
		t.Fatal("expected unknown sort error")
	}
	qty := "2 bowls"
	patched, err := s.CorrectFood(ctx, id, nil, &qty, nil, map[string]*float64{"kcal": fptr(210)}, nil, nil)
	if err != nil || patched["qty"] != "2 bowls" {
		t.Fatalf("correct %v %v", patched, err)
	}
	cleared, err := s.CorrectFood(ctx, id, nil, nil, nil, nil, []string{"kcal", "protein"}, nil)
	if err != nil {
		t.Fatalf("clear %v", err)
	}
	if _, ok := cleared["kcal"]; ok {
		t.Fatalf("kcal not cleared: %v", cleared)
	}
	if _, err := s.CorrectFood(ctx, id, nil, nil, nil, nil, []string{"sugar"}, nil); err == nil {
		t.Fatal("expected unknown-macro error on clear")
	}
	if _, err := s.CorrectFood(ctx, id, nil, nil, nil,
		map[string]*float64{"kcal": fptr(1)}, []string{"kcal"}, nil); err == nil {
		t.Fatal("expected set+clear overlap error")
	}
	if _, err := s.CorrectFood(ctx, id, nil, nil, nil, nil, nil, nil); err == nil {
		t.Fatal("expected empty-patch error")
	}
	dry, err := s.DeleteFood(ctx, id, true)
	if err != nil || dry["would_delete"] != 1 {
		t.Fatalf("dry run %v %v", dry, err)
	}
	if rows, _, _ := s.ListFood(ctx, day, day, "", "", ""); len(rows) != 1 {
		t.Fatal("dry run must not delete")
	}
	live, err := s.DeleteFood(ctx, id, false)
	if err != nil || live["deleted"] != 1 {
		t.Fatalf("live delete %v %v", live, err)
	}
	if _, err := s.DeleteFood(ctx, id, false); err == nil {
		t.Fatal("expected unknown-id error on second delete")
	}
	if _, _, err := s.ListFood(ctx, "bad-day", day, "", "", ""); err == nil {
		t.Fatal("expected bad-date error")
	}
}

func TestWeightRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	day := "2026-09-02"
	cleanDay(t, s, ctx, day)

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
	rows, total, err := s.ListWeight(ctx, day, day, "", "")
	if err != nil || len(rows) != 1 || rows[0]["kg"] != 71.0 || total != 1 {
		t.Fatalf("list %v total=%d %v", rows, total, err)
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
	cleanDay(t, s, ctx, day)

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
	days, total, err := s.ListSleep(ctx, day, day, "", "")
	if err != nil || len(days) != 1 || days[0]["sleep_hours"] != 7.5 || total != 1 {
		t.Fatalf("list %v total=%d %v", days, total, err)
	}
	sessArr, _ := days[0]["sessions"].([]bson.M)
	if len(sessArr) != 1 {
		t.Fatalf("sessions %v", days[0]["sessions"])
	}
	sessID, _ := sessArr[0]["id"].(string)

	if _, err := s.CorrectSleep(ctx, sessID, 8*60); err != nil {
		t.Fatalf("correct: %v", err)
	}
	days, _, _ = s.ListSleep(ctx, day, day, "", "")
	if days[0]["sleep_hours"] != 8.0 {
		t.Fatalf("hours %v", days[0]["sleep_hours"])
	}
	if _, err := s.CorrectSleep(ctx, "nonexistent", 60); err == nil {
		t.Fatal("expected unknown-id error")
	}
	if _, err := s.CorrectSleep(ctx, sessID, 25*60); err == nil {
		t.Fatal("expected over-24h error")
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
	if _, err := s.LogWorkout(ctx, day, strings.Repeat("x", 61), 30, 300); err == nil {
		t.Fatal("expected over-60 type error")
	}
	// Rune count, not bytes: 60 multibyte chars pass, 61 fail.
	// kcal 0 keeps the passing row out of the later totals.
	if _, err := s.LogWorkout(ctx, day, strings.Repeat("é", 60), 30, 0); err != nil {
		t.Fatalf("60-char type should pass: %v", err)
	}
	if _, err := s.LogWorkout(ctx, day, strings.Repeat("é", 61), 30, 0); err == nil {
		t.Fatal("expected over-61 type error")
	}
	if _, err := s.LogWorkout(ctx, day, "run", 30, 300); err == nil {
		t.Fatal("expected duplicate-workout error (lowered type)")
	}
	wodays, wtotal, err := s.ListWorkouts(ctx, day, day, "", "", "RUN")
	if err != nil || len(wodays) != 1 || wtotal != 1 {
		t.Fatalf("type filter %v total=%d %v", wodays, wtotal, err)
	}
	wodays, _, err = s.ListWorkouts(ctx, day, day, "", "", "swim")
	if err != nil || len(wodays) != 0 {
		t.Fatalf("unmatched type must be empty, got %v %v", wodays, err)
	}
	allDays, _, _ := s.ListWorkouts(ctx, day, day, "", "", "")
	first := allDays[0]["workouts"].([]bson.M)[0]
	woid, _ := first["id"].(string)
	if _, err := s.CorrectWorkout(ctx, woid, nil, nil, nil); err == nil {
		t.Fatal("expected empty-patch error")
	}
	empty := ""
	if _, err := s.CorrectWorkout(ctx, woid, &empty, nil, nil); err == nil {
		t.Fatal("expected empty-type error")
	}
	huge := 2000.0
	if _, err := s.CorrectWorkout(ctx, woid, nil, &huge, nil); err == nil {
		t.Fatal("expected over-24h error")
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
		Sleeps:   []SleepIn{{Day: day, Start: &start, Minutes: 400}},
		Workouts: []WorkoutIn{{Day: day, Type: "cycle", Minutes: 20, Kcal: 150}}}
	res, err := s.RecordSync(ctx, rec)
	if err != nil || res.SessionsAdded != 1 || res.WorkoutsAdded != 1 {
		t.Fatalf("sync %v %v", res, err)
	}
	// Re-sent entries skip quietly (same content hashes) — Day is set by
	// the caller, never defaulted by the store.
	rec2 := SyncRecord{Date: day, Steps: &steps, ActiveKcal: &kcal,
		Sleeps:   []SleepIn{{Day: day, Start: &start, Minutes: 400}},
		Workouts: []WorkoutIn{{Day: day, Type: "cycle", Minutes: 20, Kcal: 150}}}
	res2, err := s.RecordSync(ctx, rec2)
	if err != nil || res2.SessionsSkipped != 1 || res2.WorkoutsSkipped != 1 {
		t.Fatalf("resync %v %v", res2, err)
	}
	// Sync validation tiers: bad scalars fail the batch (422, not 500);
	// bad entries skip-and-count without blocking the rest.
	negSteps := -5
	if _, err := s.RecordSync(ctx, SyncRecord{Date: day, Steps: &negSteps}); err == nil {
		t.Fatal("expected negative-steps error")
	}
	negKcal := -1.0
	if _, err := s.RecordSync(ctx, SyncRecord{Date: day, ActiveKcal: &negKcal}); err == nil {
		t.Fatal("expected negative-kcal error")
	}
	skipRes, err := s.RecordSync(ctx, SyncRecord{Date: day,
		Sleeps: []SleepIn{{Day: day, Minutes: 25 * 60}}})
	if err != nil || skipRes.SessionsInvalid != 1 || skipRes.SessionsAdded != 0 {
		t.Fatalf("invalid entry should skip, not fail: %v %v", skipRes, err)
	}
	// Empty Day is invalid, never silently re-bucketed onto the sync date.
	emptyDay, err := s.RecordSync(ctx, SyncRecord{Date: day,
		Sleeps: []SleepIn{{Minutes: 60}}})
	if err != nil || emptyDay.SessionsInvalid != 1 || emptyDay.SessionsAdded != 0 {
		t.Fatalf("empty Day should count invalid: %v %v", emptyDay, err)
	}
	// Bad batch date with no scalars: entries still write on their own
	// days instead of 422ing the whole payload.
	badDate, err := s.RecordSync(ctx, SyncRecord{Date: "not-a-day",
		Sleeps: []SleepIn{{Day: "2026-09-10", Minutes: 60}}})
	if err != nil || badDate.SessionsAdded != 1 {
		t.Fatalf("entries-only bad-date sync %v %v", badDate, err)
	}
	// Bad batch date WITH scalars stays a 422 (scalars are homeless).
	if _, err := s.RecordSync(ctx, SyncRecord{Date: "not-a-day",
		Steps: &steps}); err == nil {
		t.Fatal("expected bad-date error with scalars")
	}
	// Empty sync writes nothing — not even a bare day doc.
	if _, err := s.RecordSync(ctx, SyncRecord{Date: "2026-09-13"}); err != nil {
		t.Fatal(err)
	}
	if n := countDay(t, ctx, s.days, "date", "2026-09-13"); n != 0 {
		t.Fatal("empty sync created a day doc")
	}
	// Missing day: all scalars null (missing sync indistinguishable from
	// nothing measured — never a fake zero on one line only).
	ghost, err := s.DailySummary(ctx, "2026-09-30")
	if err != nil {
		t.Fatalf("ghost summary: %v", err)
	}
	if ghost["steps"] != nil || ghost["active_kcal"] != nil || ghost["weight"] != nil {
		t.Fatalf("ghost scalars: %v", ghost)
	}
	if ghost["sleep_hours"] != nil {
		t.Fatalf("ghost sleep: %v", ghost)
	}
	// Partial macros: one row with kcal, one without — totals are lower
	// bounds, and macro_counts says so.
	dt, _ := time.Parse(time.RFC3339, "2026-09-03T08:00:00Z")
	if _, err := s.LogFood(ctx, day, dt, "rice", "", map[string]float64{"kcal": 200}, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LogFood(ctx, day, dt, "tea", "", nil, ""); err != nil {
		t.Fatal(err)
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
	mc, _ := sum["macro_counts"].(map[string]int64)
	if mc["kcal"] != 1 || mc["protein"] != 0 {
		t.Fatalf("macro_counts %v (rice had kcal only)", mc)
	}

	cleanDay(t, s, ctx, day)
}

func fptr(f float64) *float64 { return &f }

func strptr(s string) *string { return &s }

func countDay(t *testing.T, ctx context.Context, col *mongoDrv.Collection, key, day string) int {
	t.Helper()
	n, err := col.CountDocuments(ctx, bson.M{key: day})
	if err != nil {
		t.Fatal(err)
	}
	return int(n)
}

// cleanDay wipes one test day from all four collections, before and
// after. Aborted runs leave rows behind; without this a rerun trips the
// very dedup the suite is asserting.
func cleanDay(t *testing.T, s *Store, ctx context.Context, day string) {
	t.Helper()
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

// Non-empty legacy hc_meals fails boot (fail-closed) instead of being
// dropped; an empty one is cleaned quietly.
func TestLegacyMealsGate(t *testing.T) {
	if os.Getenv("MONGODB_URI") == "" {
		t.Skip("MONGODB_URI not set")
	}
	t.Setenv("MONGODB_DB", "healthcheck_test")
	d, err := mongo.DB()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := d.Collection("hc_meals").InsertOne(ctx, bson.M{"date": "2020-01-01"}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(); err == nil {
		t.Fatal("expected refusal with non-empty hc_meals")
	} else if got := err.Error(); !strings.Contains(got, "hc_meals still holds") {
		t.Fatalf("wrong error: %v", err)
	}
	if _, err := d.Collection("hc_meals").DeleteMany(ctx, bson.M{}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(); err != nil {
		t.Fatalf("empty hc_meals should boot: %v", err)
	}
}

// A backfill's nights stay on their mornings: per-entry days survive
// RecordSync, rec.Date covers only the scalars.
func TestRecordSyncPreservesEntryDays(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	for _, d := range []string{"2026-09-10", "2026-09-11", "2026-09-12", "2026-09-30"} {
		cleanDay(t, s, ctx, d)
	}
	start1, _ := time.Parse(time.RFC3339, "2026-09-10T22:00:00Z")
	start2, _ := time.Parse(time.RFC3339, "2026-09-11T22:00:00Z")
	steps := 100
	rec := SyncRecord{Date: "2026-09-12", Steps: &steps,
		Sleeps: []SleepIn{
			{Day: "2026-09-11", Start: &start1, Minutes: 400},
			{Day: "2026-09-12", Start: &start2, Minutes: 300},
		},
		Workouts: []WorkoutIn{{Day: "2026-09-10", Type: "run", Minutes: 30, Kcal: 300}},
	}
	res, err := s.RecordSync(ctx, rec)
	if err != nil || res.SessionsAdded != 2 || res.WorkoutsAdded != 1 {
		t.Fatalf("sync %v %v", res, err)
	}
	if n := countDay(t, ctx, s.sleep, "day", "2026-09-11"); n != 1 {
		t.Fatalf("2026-09-11 sessions: %d", n)
	}
	if n := countDay(t, ctx, s.sleep, "day", "2026-09-12"); n != 1 {
		t.Fatalf("2026-09-12 sessions: %d", n)
	}
	if n := countDay(t, ctx, s.workouts, "day", "2026-09-10"); n != 1 {
		t.Fatalf("2026-09-10 workouts: %d", n)
	}
	// Scalars land on rec.Date only.
	var d bson.M
	if err := s.days.FindOne(ctx, bson.M{"date": "2026-09-12"}).Decode(&d); err != nil {
		t.Fatal(err)
	}
	if fval(d["steps"]) != 100 {
		t.Fatalf("steps %v (%T)", d["steps"], d["steps"])
	}
	// CorrectFood datetime refresh moves the day.
	dt, _ := time.Parse(time.RFC3339, "2026-09-10T08:00:00Z")
	row, err := s.LogFood(ctx, "2026-09-11", dt, "oats", "", map[string]float64{"kcal": 150}, "")
	if err != nil {
		t.Fatal(err)
	}
	id, _ := row["id"].(string)
	newDT, _ := time.Parse(time.RFC3339, "2026-09-12T08:00:00+05:30")
	patched, err := s.CorrectFood(ctx, id, nil, nil, nil, nil, nil, &newDT)
	if err != nil {
		t.Fatal(err)
	}
	if patched["day"] != "2026-09-12" {
		t.Fatalf("day not refreshed: %v", patched)
	}
	// Cleanup.
	for _, c := range []struct {
		col *mongoDrv.Collection
		key string
	}{
		{s.days, "date"}, {s.sleep, "day"}, {s.workouts, "day"}, {s.food, "day"},
	} {
		if _, err := c.col.DeleteMany(ctx, bson.M{c.key: bson.M{"$gte": "2026-09-10", "$lte": "2026-09-12"}}); err != nil {
			t.Fatal(err)
		}
	}
}
