package main

import (
	"strings"
	"testing"

	"agento/internal/tasks"
)

// gapSeparator is the one in main.go — deliberately not redeclared here, so the test
// and the renderer cannot disagree about the format they are both asserting.
//
// humanPart returns everything after the separator, failing the test rather than
// slicing blindly if the separator is absent.
//
// The byte length is computed, never written as a literal. An earlier version used
// `Index(...) + 3`, which is right for a 1-byte separator and WRONG here: the em dash
// is three bytes in UTF-8, so the slice landed two bytes inside it and the assertion
// compared against a string starting with a stray \x94. It failed, which is the only
// reason it was caught — the same +3 against a plain "-" would have silently worked
// and then broken the day someone swapped the dash.
func humanPart(t *testing.T, line string) string {
	t.Helper()
	i := strings.Index(line, gapSeparator)
	if i < 0 {
		t.Fatalf("line has no %q separator: %q", gapSeparator, line)
	}
	return line[i+len(gapSeparator):]
}

// A gap line is always a "this needs a human" line, so it must never render without an
// explanation. `UserFacing()` is empty for RolloverNone and RolloverCreated, which is
// correct where else it is used — those outcomes need no explanation — but here an
// empty one produced " - name (id) — " and left the reader with a dangling dash and
// nothing to act on.
// ALL SIX reasons, not a sample. This originally listed
// {None, Created, None} — a duplicate where a third case belonged — so it exercised
// two of the six and missed Custom, Exhausted and Failed entirely. The test's whole
// claim is "no reason renders without an explanation", and that claim is only worth
// something if every reason is in the loop: a new reason with an empty UserFacing()
// would have passed a sampled list without anyone noticing.
func TestGapLineNeverRendersAnEmptyExplanation(t *testing.T) {
	all := []tasks.Rollover{
		tasks.RolloverNone, tasks.RolloverCreated, tasks.RolloverCustom,
		tasks.RolloverExhausted, tasks.RolloverFailed, tasks.Rollover(""),
	}
	seen := map[tasks.Rollover]bool{}
	for _, why := range all {
		if seen[why] {
			t.Errorf("reason %q appears twice in the case list; a duplicate is a "+
				"missing case", why)
		}
		seen[why] = true

		line := gapLine(tasks.RolloverGap{TaskID: "abc123", Name: "Some task", Why: why})
		if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), ":")) == "" {
			t.Errorf("reason %q rendered as an empty explanation: %q", why, line)
		}
		if !strings.Contains(line, string(why)) {
			t.Errorf("reason %q rendered without naming it, so the reader cannot tell "+
				"what state produced it: %q", why, line)
		}
		if !strings.Contains(line, gapSeparator) {
			t.Errorf("reason %q rendered without the separator, so it does not have the "+
				"documented shape: %q", why, line)
		}
	}
	// Guard against the list itself rotting: every Rollover the store can produce must
	// be covered. RolloverNone and RolloverCreated are the two with an empty
	// UserFacing(); the rest have text and are here to prove the loop is exhaustive.
	if len(seen) < 6 {
		t.Errorf("covered %d reasons, want all six", len(seen))
	}
}

// The line is "<reason> — <detail>: <outcome>". Within the human part the Detail LEADS:
// it names the field to fix, while UserFacing() only ever described the outcome, and
// making the reader parse a sentence about a category before learning which field was
// wrong is the wrong order.
func TestGapLineLeadsWithTheDetail(t *testing.T) {
	line := gapLine(tasks.RolloverGap{
		TaskID: "abc", Name: "t", Why: tasks.RolloverFailed,
		Detail: "cannot roll over: due_time is required (HH:MM)",
	})
	if !strings.HasPrefix(line, string(tasks.RolloverFailed)) {
		t.Errorf("line does not lead with the machine reason: %q", line)
	}
	human := humanPart(t, line)
	if !strings.HasPrefix(human, "cannot roll over: due_time") {
		t.Errorf("detail does not lead the human part: %q", line)
	}
	if !strings.Contains(line, tasks.RolloverFailed.UserFacing()) {
		t.Errorf("the outcome sentence is missing: %q", line)
	}
}

// A detail with no outcome text still prints the detail, and does not append a colon
// followed by nothing.
func TestGapLineWithDetailAndNoOutcomeIsClean(t *testing.T) {
	line := gapLine(tasks.RolloverGap{
		TaskID: "abc", Name: "t", Why: tasks.RolloverNone,
		Detail: "the stored repeat no longer parses as structured",
	})
	if !strings.HasPrefix(line, string(tasks.RolloverNone)) {
		t.Errorf("line does not lead with the machine reason: %q", line)
	}
	human := humanPart(t, line)
	if !strings.HasPrefix(human, "the stored repeat") {
		t.Errorf("detail does not lead the human part: %q", line)
	}
	if strings.HasSuffix(strings.TrimSpace(line), ":") {
		t.Errorf("line ends in a dangling colon: %q", line)
	}
	if !strings.Contains(line, string(tasks.RolloverNone)) {
		t.Errorf("line does not name the reason: %q", line)
	}
}
