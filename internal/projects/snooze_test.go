package projects

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestParseSnoozeAt(t *testing.T) {
	// Empty clears, and is not an error: one spelling serves set and clear.
	at, err := ParseSnoozeAt("  ")
	if err != nil || at != nil {
		t.Fatalf("empty: got %v %v", at, err)
	}
	// A date alone or a clock time alone must not parse: snooze is an
	// instant, and both halves are required.
	for _, raw := range []string{"2026-11-19", "18:00", "2026-11-19 18:00"} {
		if _, err := ParseSnoozeAt(raw); err == nil {
			t.Fatalf("%q: expected an error", raw)
		}
	}
	got, err := ParseSnoozeAt("2026-11-19T18:00:00+05:30")
	if err != nil {
		t.Fatalf("valid RFC3339: %v", err)
	}
	if got == nil || got.UTC().Format(time.RFC3339) != "2026-11-19T12:30:00Z" {
		t.Fatalf("valid RFC3339: got %v", got)
	}
}

func TestCheckSnooze(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	later := now.Add(time.Hour)
	// No snooze is always fine, whatever the status.
	if err := checkSnooze("Todo", nil, now, false); err != nil {
		t.Fatalf("nil snooze: %v", err)
	}
	// A snooze is a Paused-only field.
	if err := checkSnooze("Todo", &later, now, false); err == nil ||
		!strings.Contains(err.Error(), "Paused") {
		t.Fatalf("non-Paused: got %v", err)
	}
	// And it must be far enough ahead that the ticker cannot flip it instantly.
	if err := checkSnooze("Paused", &later, now, false); err != nil {
		t.Fatalf("future Paused: %v", err)
	}
	soon := now.Add(SnoozeLead)
	if err := checkSnooze("Paused", &soon, now, false); err == nil {
		t.Fatal("snooze exactly at the lead should be rejected")
	}
	// ...but an unchanged instant must survive an unrelated edit near expiry.
	if err := checkSnooze("Paused", &soon, now, true); err != nil {
		t.Fatalf("unchanged instant should bypass the lead: %v", err)
	}
	past := now.Add(-time.Minute)
	if err := checkSnooze("Paused", &past, now, false); err == nil {
		t.Fatal("past snooze should be rejected")
	}
}

func TestSnoozeRoundTripAndWake(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	// Snooze needs Paused, on create too.
	if _, err := s.CreateWithSnooze(ctx, "nope", "Todo", "", ptrTime(now.Add(time.Hour))); err == nil {
		t.Fatal("create with snooze on Todo should fail")
	}
	due := now.Add(2 * time.Hour)
	doc, err := s.CreateWithSnooze(ctx, "snoozed", "Paused", "", &due)
	if err != nil {
		t.Fatalf("CreateWithSnooze: %v", err)
	}
	id, _ := doc["id"].(string)
	if got, _ := doc["snoozeAt"].(string); got == "" {
		t.Fatalf("snoozeAt missing after create: %v", doc)
	}

	// Before the lead: nothing wakes.
	moved, err := s.WakeDue(ctx, due.Add(-2*SnoozeLead))
	if err != nil || len(moved) != 0 {
		t.Fatalf("early wake: %v %v", moved, err)
	}

	// At snoozeAt-SnoozeLead the row flips, clears the snooze and lands in
	// the Ongoing section.
	moved, err = s.WakeDue(ctx, due.Add(-SnoozeLead))
	if err != nil || len(moved) != 1 {
		t.Fatalf("wake: %v %v", moved, err)
	}
	if moved[0]["status"] != "Ongoing" {
		t.Fatalf("status after wake: %v", moved[0])
	}
	if _, ok := moved[0]["snoozeAt"]; ok {
		t.Fatalf("snoozeAt should be cleared: %v", moved[0])
	}
	// The wake marker names the instant that fired, so a client can compare
	// it with the snooze it cached and tell this from a manual move.
	if got, _ := moved[0]["snoozeFiredAt"].(string); got != due.Format(time.RFC3339) {
		t.Fatalf("snoozeFiredAt = %q, want %q", got, due.Format(time.RFC3339))
	}
	got, err := s.Get(ctx, id)
	if err != nil || got["status"] != "Ongoing" {
		t.Fatalf("Get after wake: %v %v", got, err)
	}

	// Idempotent: a second ticker finds nothing.
	moved, err = s.WakeDue(ctx, due.Add(-SnoozeLead))
	if err != nil || len(moved) != 0 {
		t.Fatalf("second wake: %v %v", moved, err)
	}
}

func TestSnoozeUpdateRules(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	doc, err := s.Create(ctx, "paused", "Paused", "")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	id, _ := doc["id"].(string)
	// A snooze on a non-Paused row is rejected before any write.
	if _, err := s.Update(ctx, id, map[string]any{"status": "Todo", "snoozeAt": ptrTime(now.Add(time.Hour))}); err == nil {
		t.Fatal("snooze with status Todo should fail")
	}
	// Setting it on Paused succeeds...
	due := now.Add(time.Hour)
	got, err := s.Update(ctx, id, map[string]any{"snoozeAt": &due})
	if err != nil || got["snoozeAt"] == "" {
		t.Fatalf("set snooze: %v %v", got, err)
	}
	// Echoing the stored instant alongside an unrelated edit is accepted:
	// the value has not changed, so the 1-minute lead is not re-applied.
	echo, err := time.Parse(time.RFC3339, got["snoozeAt"].(string))
	if err != nil {
		t.Fatalf("stored snoozeAt: %v", err)
	}
	if _, err := s.Update(ctx, id, map[string]any{"note": "typo fix", "snoozeAt": echo}); err != nil {
		t.Fatalf("unchanged snooze with a note edit: %v", err)
	}
	// ...and leaving Paused clears it, so a stale instant can never sit on
	// an Ongoing row. The echoed instant must not be treated as an attempt
	// to arm a snooze on a non-Paused row: that shape is exactly what a
	// client sending its whole cached row looks like.
	got, err = s.Update(ctx, id, map[string]any{"status": "Ongoing", "snoozeAt": echo})
	if err != nil {
		t.Fatalf("leave Paused with echoed snooze: %v", err)
	}
	if _, ok := got["snoozeAt"]; ok {
		t.Fatalf("leaving Paused should clear snooze: %v", got)
	}
	// Explicit clear uses a typed nil pointer.
	got, err = s.Update(ctx, id, map[string]any{"status": "Paused"})
	if err != nil {
		t.Fatalf("back to Paused: %v", err)
	}
	got, err = s.Update(ctx, id, map[string]any{"snoozeAt": &due})
	if err != nil {
		t.Fatalf("set again: %v", err)
	}
	got, err = s.Update(ctx, id, map[string]any{"snoozeAt": (*time.Time)(nil)})
	if err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, ok := got["snoozeAt"]; ok {
		t.Fatalf("explicit clear should remove snooze: %v", got)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
