package tasks

import (
	"strconv"
	"strings"
	"time"

	"agento/internal/mongostore"
	"go.mongodb.org/mongo-driver/bson"
)

// Recurrence rules: the Repeat type, its validation, its date arithmetic,
// and reading one back off a stored doc. Pure logic — no Store, no I/O —
// split out of store.go so the rule outlives the file it grew in (#171).
//
// The server stores the recurrence but never advances it: creating the
// next occurrence is the caller's job (see pi/skills/task-manager/SKILL.md).

// Repeat is a task's recurrence, in one of two mutually exclusive modes:
//
//	structured — Every N Unit ("every 3 days"), the machine-usable case;
//	custom     — the user's own words in Text, verbatim.
//
// An all-zero Repeat is a one-shot task. The server stores the recurrence
// but never advances it: creating the next occurrence is the caller's job
// (see pi/skills/task-manager/SKILL.md).
type Repeat struct {
	// Every is the count, RepeatEveryMin..RepeatEveryMax; 0 = unset.
	Every int
	// Unit is one of RepeatUnits; "" = unset.
	Unit string
	// Custom selects the Text mode.
	Custom bool
	// Text is the custom condition, stored verbatim.
	Text string
}

const (
	// RepeatEveryMin/Max bound the structured count. The upper bound is
	// the largest interval a person tracks by eye (a 4-weekly cycle);
	// anything longer wants a custom condition instead.
	RepeatEveryMin = 1
	RepeatEveryMax = 28
)

// RepeatUnits lists the accepted Unit values, shortest first.
func RepeatUnits() []string { return []string{"days", "weeks", "months", "years"} }

func isRepeatUnit(u string) bool {
	for _, v := range RepeatUnits() {
		if u == v {
			return true
		}
	}
	return false
}

// IsZero reports the one-shot case: no cadence and no custom text.
func (r Repeat) IsZero() bool {
	return r.Every == 0 && r.Unit == "" && !r.Custom && r.Text == ""
}

// Normalize infers the mode flag from what the caller actually sent, so
// clients that predate Repeat.Custom (and the 4.6 migration, which only
// knew the free-text rule) keep working unchanged: text with no
// structured cadence means a custom condition.
func (r Repeat) Normalize() Repeat {
	if r.Text != "" && !r.Custom && r.Every == 0 && r.Unit == "" {
		r.Custom = true
	}
	return r
}

// Validate rejects the shapes that would leave the recurrence ambiguous.
// The one leniency is Normalize's: text alone is a custom condition.
func (r Repeat) Validate() error {
	r = r.Normalize()
	if r.Custom {
		if strings.TrimSpace(r.Text) == "" {
			return fail("repeat_custom is set but repeat_rule is empty — a custom condition needs the words")
		}
		if r.Every != 0 || r.Unit != "" {
			return fail("set either repeat_every/repeat_unit or a custom condition, not both")
		}
		return nil
	}
	if r.Every == 0 && r.Unit == "" {
		if strings.TrimSpace(r.Text) != "" {
			return fail("repeat_rule text needs repeat_custom = true")
		}
		return nil
	}
	if r.Every < RepeatEveryMin || r.Every > RepeatEveryMax {
		return fail("repeat_every must be %d-%d, got %d", RepeatEveryMin, RepeatEveryMax, r.Every)
	}
	if !isRepeatUnit(r.Unit) {
		return fail("repeat_unit must be one of %s, got '%s'",
			strings.Join(RepeatUnits(), "/"), r.Unit)
	}
	if strings.TrimSpace(r.Text) != "" {
		return fail("repeat_rule text needs repeat_custom = true")
	}
	return nil
}

// IsStructured reports the machine-usable mode: a cadence with a count
// and a unit, and no custom text. Only a structured repeat can be rolled
// over automatically — a custom condition is the caller's to interpret.
func (r Repeat) IsStructured() bool {
	n := r.Normalize()
	return n.Every > 0 && n.Unit != "" && !n.Custom
}

// MaxRollovers bounds the catch-up loop in NextDueDate.
const MaxRollovers = 1500

// NextDueDate advances a YYYY-MM-DD date by the cadence and returns
// YYYY-MM-DD. ok is false for a custom/empty cadence or an unparseable
// date.
//
// Two details that are easy to get wrong:
//   - Month/year arithmetic clamps to the last day of the target month
//     instead of overflowing: the 31st of a 30-day month becomes the 28th
//     (29th in a leap year), and 31 Jan + 1 month is 28/29 Feb, not 3 Mar.
//   - A task that has been left overdue for months would otherwise roll
//     straight back into the past and nag immediately, so the date is
//     advanced until it is on/after today.
func (r Repeat) NextDueDate(dueDate string, today time.Time) (string, bool) {
	if !r.IsStructured() {
		return "", false
	}
	d, err := time.Parse("2006-01-02", strings.TrimSpace(dueDate))
	if err != nil {
		return "", false
	}
	step := r.Normalize()
	cutoff := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	next := d
	for i := 0; i < MaxRollovers; i++ {
		next = advanceDate(next, step.Every, step.Unit)
		if !next.Before(cutoff) {
			return next.Format("2006-01-02"), true
		}
	}
	// Only reachable for a cadence whose every step lands in the past
	// (a daily task untouched for over four years). Refusing is the honest
	// answer: the caller creates the next occurrence itself rather than
	// receiving a date that is already overdue.
	return "", false
}

// advanceDate adds count units, clamping the day to the target month's
// length. Go's AddDate normalizes overflow (31 Jan + 1 month = 3 Mar),
// which is never what a repeating task means.
func advanceDate(d time.Time, count int, unit string) time.Time {
	switch unit {
	case "days":
		return d.AddDate(0, 0, count)
	case "weeks":
		return d.AddDate(0, 0, 7*count)
	case "months":
		month := int(d.Month()) - 1 + count
		year := d.Year() + month/12
		month = month%12 + 1
		if month < 1 {
			month += 12
			year--
		}
		m := time.Month(month)
		return time.Date(year, m, min(d.Day(), daysInMonth(year, m)), 0, 0, 0, 0, time.UTC)
	case "years":
		year := d.Year() + count
		return time.Date(year, d.Month(), min(d.Day(), daysInMonth(year, d.Month())), 0, 0, 0, 0, time.UTC)
	}
	// Unknown unit can't reach here (Validate rejects it), but never spin.
	return d
}

func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// String renders the recurrence for humans: "Every 3 days", the custom
// words verbatim, or "" for a one-shot task.
func (r Repeat) String() string {
	r = r.Normalize()
	if r.Custom || (r.Every == 0 && r.Unit == "") {
		return r.Text
	}
	singular := strings.TrimSuffix(r.Unit, "s")
	if r.Every == 1 {
		return "Every " + singular
	}
	return "Every " + strconv.Itoa(r.Every) + " " + r.Unit
}

// docs renders the stored shape. Kept in one place so Create, Update and
// the migration can never disagree on the key names.
func (r Repeat) docs() bson.M {
	r = r.Normalize()
	return bson.M{
		"repeat_every":  r.Every,
		"repeat_unit":   r.Unit,
		"repeat_custom": r.Custom,
		"repeat_rule":   r.Text,
	}
}

// repeatFromDoc reads a doc's recurrence, tolerating pre-4.6 docs that
// only ever had the free-text repeat_rule string.
func repeatFromDoc(doc bson.M) Repeat {
	r := Repeat{
		Every:  0,
		Unit:   "",
		Custom: false,
		Text:   "",
	}
	if v, ok := doc["repeat_rule"].(string); ok {
		r.Text = v
	}
	if n, ok := mongostore.ToInt(doc["repeat_every"]); ok {
		r.Every = n
	}
	if u, ok := doc["repeat_unit"].(string); ok {
		r.Unit = u
	}
	if b, ok := mongostore.ToBool(doc["repeat_custom"]); ok {
		r.Custom = b
	}
	return r.Normalize()
}
