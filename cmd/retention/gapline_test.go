package main

import (
	"strings"
	"testing"

	"agento/internal/tasks"
)

// A gap line is always a "this needs a human" line, so it must never render without an
// explanation. `UserFacing()` is empty for RolloverNone and RolloverCreated, which is
// correct where else it is used — those outcomes need no explanation — but here an
// empty one produced " - name (id) — " and left the reader with a dangling dash and
// nothing to act on.
func TestGapLineNeverRendersAnEmptyExplanation(t *testing.T) {
	for _, why := range []tasks.Rollover{
		tasks.RolloverNone, tasks.RolloverCreated, tasks.RolloverNone,
	} {
		line := gapLine(tasks.RolloverGap{TaskID: "abc123", Name: "Some task", Why: why})
		if strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), ":")) == "" {
			t.Errorf("reason %q rendered as an empty explanation: %q", why, line)
		}
		if !strings.Contains(line, string(why)) {
			t.Errorf("reason %q rendered without naming it, so the reader cannot tell "+
				"what state produced it: %q", why, line)
		}
	}
}

// With a detail present the detail LEADS: it names the field to fix, while
// UserFacing() only ever described the outcome, and making the reader parse a sentence
// about a category before learning which field was wrong is the wrong order.
func TestGapLineLeadsWithTheDetail(t *testing.T) {
	line := gapLine(tasks.RolloverGap{
		TaskID: "abc", Name: "t", Why: tasks.RolloverFailed,
		Detail: "cannot roll over: due_time is required (HH:MM)",
	})
	if !strings.HasPrefix(line, "cannot roll over: due_time") {
		t.Errorf("detail does not lead the line: %q", line)
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
	if !strings.HasPrefix(line, "the stored repeat") {
		t.Errorf("detail does not lead: %q", line)
	}
	if strings.HasSuffix(strings.TrimSpace(line), ":") {
		t.Errorf("line ends in a dangling colon: %q", line)
	}
	if !strings.Contains(line, string(tasks.RolloverNone)) {
		t.Errorf("line does not name the reason: %q", line)
	}
}
