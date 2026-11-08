package main

import (
	"strings"
	"testing"
	"time"

	"agento/internal/cronjobs"
)

func TestRenderBlockExactEntries(t *testing.T) {
	loc, err := time.LoadLocation("UTC")
	if err != nil {
		t.Skipf("zoneinfo unavailable: %v", err)
	}
	now := time.Date(2026, 11, 8, 9, 0, 0, 0, cronjobs.IST)
	entries, err := cronjobs.RenderJobs([]cronjobs.OSJob{{
		ID: "68abc", Name: "Morning nudge", Prompt: "log breakfast",
		Schedule: "0 9 * * *", Enabled: true,
	}}, loc, now)
	if err != nil {
		t.Fatal(err)
	}
	block := renderBlock(entries, "/repo", now)
	want := "30 3 * * * TZ=Asia/Kolkata bash /repo/scripts/t3-cron.sh run 68abc >> /repo/run/t3-cron.log 2>&1"
	if !strings.Contains(block, want) {
		t.Fatalf("block missing %q:\n%s", want, block)
	}
	if !strings.Contains(block, "# job: Morning nudge (id 68abc) — 0 9 * * * IST") {
		t.Fatalf("block missing provenance comment:\n%s", block)
	}
	if !strings.HasPrefix(block, markerStart) || !strings.HasSuffix(strings.TrimSpace(block), strings.TrimSpace(markerEnd)+" <<<") {
		t.Fatalf("block markers malformed:\n%s", block)
	}
}

func TestRenderBlockNoJobs(t *testing.T) {
	block := renderBlock(nil, "/repo", time.Now())
	if !strings.Contains(block, "# (no enabled jobs in cron_jobs)") {
		t.Fatalf("empty block should say so:\n%s", block)
	}
}

func TestStripBlockKeepsOtherEntries(t *testing.T) {
	in := strings.Join([]string{
		"MAILTO=me@example.com",
		"0 3 * * * bash /repo/scripts/retention.sh run",
		markerStart + " (#490) — managed >>>",
		"30 3 * * * bash /repo/scripts/t3-cron.sh run abc",
		markerEnd + " <<<",
		"* * * * * bash /repo/scripts/sysmon.sh record",
		"",
	}, "\n")
	got := stripBlock(in)
	for _, want := range []string{
		"MAILTO=me@example.com",
		"retention.sh",
		"sysmon.sh record",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("lost %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "t3-cron") {
		t.Fatalf("block survived:\n%s", got)
	}
}

func TestMissedInstants(t *testing.T) {
	s, err := cronjobs.Parse("0 9 * * *")
	if err != nil {
		t.Fatal(err)
	}
	day1 := time.Date(2026, 11, 6, 9, 0, 0, 0, cronjobs.IST)
	day2 := time.Date(2026, 11, 7, 9, 0, 0, 0, cronjobs.IST)
	day3 := time.Date(2026, 11, 8, 9, 0, 0, 0, cronjobs.IST)

	if first, n := missedInstants(s, day1, day2); n != 0 {
		t.Fatalf("normal consecutive fire reported %d miss(es) at %v", n, first)
	}
	first, n := missedInstants(s, day1, day3)
	if n != 1 || !first.Equal(day2) {
		t.Fatalf("got first=%v n=%d, want %v n=1", first, n, day2)
	}
}
