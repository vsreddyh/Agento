package main

import (
	"testing"
	"time"
)

// parseBound accepts the same date kinds as Draft: full timestamps and
// whole days (to=day is inclusive through end of day).
func TestParseBoundAcceptsDates(t *testing.T) {
	from, err := parseBound("2026-10-26", "from")
	if err != nil {
		t.Fatal(err)
	}
	if h := from.Hour(); h != 0 || from.Day() != 26 {
		t.Errorf("from date = %v, want midnight Oct 26", from)
	}
	to, err := parseBound("2026-10-26", "to")
	if err != nil {
		t.Fatal(err)
	}
	if !to.Equal(time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("to date = %v, want end of Oct 26 (next midnight)", to)
	}
	ts, err := parseBound("2026-10-26T09:00:00Z", "from")
	if err != nil || ts.Hour() != 9 {
		t.Errorf("timestamp = %v, %v; want 09:00 passthrough", ts, err)
	}
	if _, err := parseBound("monday", "from"); err == nil {
		t.Error("weekday should fail")
	}
	if _, err := parseBound("2026-02-30", "from"); err == nil {
		t.Error("nonexistent date should fail, not normalize into March")
	}
	if _, err := parseBound("   ", "from"); err != nil {
		t.Errorf("whitespace-only should default empty, got %v", err)
	}
	padded, err := parseBound("  2026-10-26T09:00:00Z  ", "from")
	if err != nil || padded.Hour() != 9 {
		t.Errorf("padded timestamp = %v, %v; want trimmed", padded, err)
	}
}
