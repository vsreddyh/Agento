package cronjobs

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func utcLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("UTC")
	if err != nil {
		t.Fatalf("load UTC: %v", err)
	}
	return loc
}

func mustRender(t *testing.T, sched string, host *time.Location) []string {
	t.Helper()
	jobs := []OSJob{{ID: "x", Name: "job", Prompt: "do it", Schedule: sched, Enabled: true}}
	entries, err := RenderJobs(jobs, host, time.Date(2026, 11, 8, 12, 0, 0, 0, host))
	if err != nil {
		t.Fatalf("render %q: %v", sched, err)
	}
	return entries[0].Schedules
}

func TestRenderDailyISTtoUTC(t *testing.T) {
	got := mustRender(t, "0 9 * * *", utcLoc(t))
	want := []string{"30 3 * * *"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRenderDailyBeforeOffsetWrapsDay(t *testing.T) {
	// 02:00 IST is 20:30 UTC the previous calendar day; with every day in
	// the week the weekday set is identical and collapses back to *.
	got := mustRender(t, "0 2 * * *", utcLoc(t))
	want := []string{"30 20 * * *"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRenderWeeklyMovesWeekday(t *testing.T) {
	// Monday 02:00 IST -> Sunday 20:30 UTC.
	got := mustRender(t, "0 2 * * 1", utcLoc(t))
	want := []string{"30 20 * * 0"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRenderWeeklySameDay(t *testing.T) {
	got := mustRender(t, "0 9 * * 1", utcLoc(t))
	want := []string{"30 3 * * 1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRenderHourlyAndSteps(t *testing.T) {
	if got, want := mustRender(t, "0 * * * *", utcLoc(t)), []string{"30 * * * *"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("hourly: got %v want %v", got, want)
	}
	if got, want := mustRender(t, "*/15 * * * *", utcLoc(t)), []string{"*/15 * * * *"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("steps: got %v want %v", got, want)
	}
}

func TestRenderMonthRestrictedSameDate(t *testing.T) {
	// 09:00 IST stays on the same UTC date, so dom/month survive.
	got := mustRender(t, "0 9 1 * *", utcLoc(t))
	want := []string{"30 3 1 * *"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRenderRejectsDateCrossingForDayRestrictedSchedule(t *testing.T) {
	jobs := []OSJob{{ID: "x", Name: "monthly", Prompt: "p", Schedule: "0 2 1 * *", Enabled: true}}
	_, err := RenderJobs(jobs, utcLoc(t), time.Now())
	if err == nil {
		t.Fatal("expected a date-crossing error")
	}
	if !strings.Contains(err.Error(), "crosses a calendar date") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRenderDateCrossingAdviceMatchesHostZone(t *testing.T) {
	// The suggested threshold must be the host's real one: 05:30 IST only
	// on UTC, earlier on UTC+2 and later on UTC-5.
	cases := []struct {
		zone, want string
	}{
		{"Etc/GMT-2", "03:30 IST or later"}, // UTC+2
		{"Etc/GMT+5", "10:30 IST or later"}, // UTC-5
	}
	for _, c := range cases {
		loc, err := time.LoadLocation(c.zone)
		if err != nil {
			t.Skipf("zoneinfo unavailable: %v", err)
		}
		jobs := []OSJob{{ID: "x", Name: "monthly", Prompt: "p", Schedule: "0 2 1 * *", Enabled: true}}
		_, err = RenderJobs(jobs, loc, time.Date(2026, 11, 8, 12, 0, 0, 0, loc))
		if err == nil {
			t.Fatalf("%s: expected a date-crossing error", c.zone)
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Fatalf("%s: error should offer %q, got: %v", c.zone, c.want, err)
		}
	}
}

func TestRenderMultiMinuteHourGroupsByHour(t *testing.T) {
	// 0,30 9,10 IST is 4 fires (03:30, 04:00, 04:30, 05:00 UTC). A single
	// "0,30 3,4,5" line would invent 03:00 and 04:30 — group by hour instead.
	got := mustRender(t, "0,30 9,10 * * *", utcLoc(t))
	want := []string{"30 3 * * *", "0,30 4 * * *", "0 5 * * *"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRenderMultiMinuteHourDayRestricted(t *testing.T) {
	got := mustRender(t, "0,30 9 1 * *", utcLoc(t))
	want := []string{"30 3 1 * *", "0 4 1 * *"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRenderInISTIsIdentity(t *testing.T) {
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skipf("zoneinfo unavailable: %v", err)
	}
	got := mustRender(t, "0 9 1 * *", ist)
	want := []string{"0 9 1 * *"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestRenderRejectsDSTHost(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("zoneinfo unavailable: %v", err)
	}
	jobs := []OSJob{{ID: "x", Name: "job", Prompt: "p", Schedule: "0 9 * * *", Enabled: true}}
	if _, err := RenderJobs(jobs, ny, time.Now()); err == nil || !strings.Contains(err.Error(), "DST") {
		t.Fatalf("expected a DST rejection, got %v", err)
	}
}

func TestRenderSkipsDisabledAndValidates(t *testing.T) {
	jobs := []OSJob{
		{ID: "p", Name: "paused", Prompt: "p", Schedule: "0 9 * * *", Enabled: false},
		{ID: "b", Name: "bad", Prompt: "p", Schedule: "not cron", Enabled: true},
	}
	if _, err := RenderJobs(jobs, utcLoc(t), time.Now()); err == nil {
		t.Fatal("expected a bad schedule error")
	}
	snap, err := SnapshotJobs([]Job{{ID: "p", Name: "paused", Prompt: "p", Schedule: "0 9 * * *", Paused: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap) != 1 || snap[0].Enabled {
		t.Fatalf("paused row should stay in the snapshot disabled: %+v", snap)
	}
}

func TestSnapshotRejectsEmptyPrompt(t *testing.T) {
	if _, err := SnapshotJobs([]Job{{ID: "e", Name: "empty", Prompt: "  ", Schedule: "0 9 * * *"}}); err == nil {
		t.Fatal("expected an empty-prompt error")
	}
}
