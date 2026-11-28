package main

import (
	"testing"
	"time"

	"agento/internal/healthcheck"
)

// Whitespace-only counts as absent (today), like every other optional date
// in this PR — never a "bad date ”" rejection.
func TestDayOfTrimsToAbsent(t *testing.T) {
	want := time.Now().In(healthcheck.IST()).Format("2006-01-02")
	for _, in := range []string{"", "   ", "\t\n "} {
		if got := dayOf(in); got != want {
			t.Errorf("dayOf(%q) = %q, want today %q", in, got, want)
		}
	}
	if got := dayOf("2026-09-03"); got != "2026-09-03" {
		t.Errorf("dayOf explicit = %q, want 2026-09-03", got)
	}
}
