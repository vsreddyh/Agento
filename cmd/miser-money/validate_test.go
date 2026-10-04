package main

import (
	"strings"
	"testing"
)

// The gap these tests close: both tools used to accept date bounds they could not honour,
// and neither returned an error when it did.
//
//   - `query_transactions` with unusable bounds built `date >= "" && date <= ""` and
//     matched nothing, so the caller saw zero transactions and no reason why.
//   - `summarize` with a lone `start` fell through to the period branch and summarised a
//     different window entirely.
//
// `required` in the input schema does not fix any of this. It is a client-side hint, and a
// caller can send `"start": ""` explicitly and pass validation.
func TestNormalizeDayRange(t *testing.T) {
	cases := []struct {
		name       string
		start, end string
		wantErr    bool
	}{
		{"both valid", "2026-01-01", "2026-03-31", false},
		{"same day", "2026-01-01", "2026-01-01", false},
		// The silent-empty-result cases.
		{"both empty", "", "", true},
		{"start empty", "", "2026-03-31", true},
		{"end empty", "2026-01-01", "", true},
		{"start not a date", "yesterday", "2026-03-31", true},
		{"end not a date", "2026-01-01", "soon", true},
		// A real calendar check, not just the YYYY-MM-DD shape: 2026-02-30 is not a date.
		{"impossible day", "2026-02-30", "2026-03-31", true},
		// Two individually valid dates whose range is empty. Every one of the cases above
		// is caught by validating a bound; this one is caught only by comparing them, and
		// it used to return zero rows as a real answer.
		{"reversed range", "2026-03-31", "2026-01-01", true},
		{"reversed across a year", "2026-01-01", "2025-12-31", true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := normalizeDayRange("query_transactions", c.start, c.end)
			if c.wantErr != (err != nil) {
				t.Errorf("normalizeDayRange(%q, %q) = %v, wantErr %v", c.start, c.end, err, c.wantErr)
			}
		})
	}
}

// Whitespace is the case that made this function return values instead of just an error:
// `validate.CheckDay` trims, and `store.Query` compares raw strings. Validating a trimmed
// copy and forwarding the untrimmed original is the bug — `" 2026-01-01 "` passes and then
// matches nothing.
func TestNormalizeDayRangeTrims(t *testing.T) {
	start, end, err := normalizeDayRange("query_transactions", " 2026-01-01 ", "\t2026-03-31\n")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if start != "2026-01-01" || end != "2026-03-31" {
		t.Errorf("got (%q, %q), want the normalized (2026-01-01, 2026-03-31)", start, end)
	}
}

// A half-supplied pair is the case a caller is most likely to send by accident, and the one
// `summarize` silently got wrong: it fell through to the period branch and reported on a
// window nobody asked for.
func TestNormalizeDayRangePair(t *testing.T) {
	cases := []struct {
		name       string
		start, end string
		wantEmpty  bool // both bounds absent: use the period phrase
		wantErr    bool
	}{
		{name: "both absent — use the period phrase", wantEmpty: true},
		{name: "both present", start: "2026-01-01", end: "2026-01-31"},
		{name: "lone start", start: "2026-01-01", wantErr: true},
		{name: "lone end", end: "2026-01-31", wantErr: true},
		{name: "both present but malformed", start: "2026-01-01", end: "nope", wantErr: true},
		{name: "reversed pair", start: "2026-01-31", end: "2026-01-01", wantErr: true},
		// Whitespace-only counts as ABSENT, not as half a pair: before trimming, "   "
		// took the "present" path and produced a confusing validation error instead.
		{name: "whitespace-only is absent", start: "   ", end: "\t", wantEmpty: true},
		{name: "whitespace start with a real end", start: "  ", end: "2026-01-31", wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start, end, err := normalizeDayRangePair("summarize", c.start, c.end)
			if c.wantErr != (err != nil) {
				t.Fatalf("normalizeDayRangePair(%q, %q) = %v, wantErr %v", c.start, c.end, err, c.wantErr)
			}
			if c.wantEmpty && (start != "" || end != "") {
				t.Errorf("got (%q, %q), want both empty so the period phrase applies", start, end)
			}
		})
	}
}

// The error has to name the tool and the field. An agent reading "bad date" has to guess
// which of six date arguments it passed wrongly, and the tool name is what tells it which
// call to redo.
func TestDayRangeErrorsAreActionable(t *testing.T) {
	_, _, err := normalizeDayRange("query_transactions", "", "2026-01-31")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"query_transactions", "start"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}

	_, _, err = normalizeDayRangePair("summarize", "2026-01-01", "")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"summarize", "start", "end"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}

	// A reversed range is the one whose cause is not visible in the offending value, so the
	// message has to say what is wrong rather than just which field.
	_, _, err = normalizeDayRange("query_transactions", "2026-03-31", "2026-01-01")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "after end") {
		t.Errorf("error %q does not explain that the range is reversed", err.Error())
	}
}
