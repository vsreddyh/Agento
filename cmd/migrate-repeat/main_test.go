package main

import (
	"testing"

	"agento/internal/tasks"
)

// The classifier decides what a legacy rule becomes, so it is pinned here
// rather than left to whatever the live data happens to contain. The two
// rules that matter: a cadence must be the WHOLE rule (never drop a
// trailing exception), and a count outside 1-28 keeps the words rather
// than being clamped into a cadence that means something else.
func TestClassify(t *testing.T) {
	cases := []struct {
		rule string
		want tasks.Repeat
	}{
		// One-shot.
		{"", tasks.Repeat{}},
		{"   ", tasks.Repeat{}},

		// Adverbs: every one of them.
		{"daily", tasks.Repeat{Every: 1, Unit: "days"}},
		{"weekly", tasks.Repeat{Every: 1, Unit: "weeks"}},
		{"monthly", tasks.Repeat{Every: 1, Unit: "months"}},
		{"yearly", tasks.Repeat{Every: 1, Unit: "years"}},
		{"annually", tasks.Repeat{Every: 1, Unit: "years"}},
		// Case and surrounding space are the user's, not the data's.
		{"  Daily ", tasks.Repeat{Every: 1, Unit: "days"}},

		// Counted cadences, with and without "every".
		{"every 3 days", tasks.Repeat{Every: 3, Unit: "days"}},
		{"3 days", tasks.Repeat{Every: 3, Unit: "days"}},
		{"every 2 weeks", tasks.Repeat{Every: 2, Unit: "weeks"}},
		{"every 6 months", tasks.Repeat{Every: 6, Unit: "months"}},
		{"every 12 days", tasks.Repeat{Every: 12, Unit: "days"}},
		{"every 28 days", tasks.Repeat{Every: 28, Unit: "days"}},
		{"every 1 day", tasks.Repeat{Every: 1, Unit: "days"}},
		{"every 1 year", tasks.Repeat{Every: 1, Unit: "years"}},
		{"every 3 week", tasks.Repeat{Every: 3, Unit: "weeks"}},

		// A cadence with a tail is NOT a cadence: the tail is the rule.
		{"daily, skip Wednesdays", tasks.Repeat{Custom: true, Text: "daily, skip Wednesdays"}},
		{"every 2 weeks unless it rains", tasks.Repeat{Custom: true, Text: "every 2 weeks unless it rains"}},
		{"end of every month", tasks.Repeat{Custom: true, Text: "end of every month"}},
		{"every day at 6am", tasks.Repeat{Custom: true, Text: "every day at 6am"}},

		// Weekday sets, notes, and anything the vocabulary can't express.
		{"mon-fri only", tasks.Repeat{Custom: true, Text: "mon-fri only"}},
		{"ask user when to repeat", tasks.Repeat{Custom: true, Text: "ask user when to repeat"}},
		{"every 2 fortnights", tasks.Repeat{Custom: true, Text: "every 2 fortnights"}},
		{"every other tuesday", tasks.Repeat{Custom: true, Text: "every other tuesday"}},

		// Out of range keeps the words: "every 30 days" clamped to 28 would
		// be a different schedule, silently.
		{"every 29 days", tasks.Repeat{Custom: true, Text: "every 29 days"}},
		{"every 0 days", tasks.Repeat{Custom: true, Text: "every 0 days"}},
	}
	for _, c := range cases {
		if got := classify(c.rule); got != c.want {
			t.Fatalf("classify(%q) = %+v, want %+v", c.rule, got, c.want)
		}
	}
}

// Whatever comes out has to be storable: a classification the store would
// reject would abort the whole run.
func TestClassifyAlwaysValid(t *testing.T) {
	for _, rule := range []string{
		"", "daily", "every 3 days", "mon-fri only", "every 29 days",
		"end of every month", "every 2 weeks unless it rains", "nonsense",
	} {
		if err := classify(rule).Validate(); err != nil {
			t.Fatalf("classify(%q) produced an unstorable recurrence: %v", rule, err)
		}
	}
}
