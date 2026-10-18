package tasks

import (
	"sort"
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
// The server stores the recurrence and rolls a STRUCTURED one (interval or
// selected days) over itself on completion; only a custom condition is the
// caller's to interpret (see pi/skills/task-manager/SKILL.md).

// Repeat is a task's recurrence, in one of three mutually exclusive modes:
//
//	structured interval — Every N Unit ("every 3 days"), the machine-usable case;
//	structured weekly   — Days, the selected weekdays ("Mon, Wed, Fri"), also
//	                      machine-usable and ALSO owned by the server (#304);
//	custom              — the user's own words in Text, verbatim.
//
// An all-zero Repeat is a one-shot task. The server advances a structured
// recurrence (interval or weekly) itself; a custom condition is the caller's
// to interpret (see pi/skills/task-manager/SKILL.md).
type Repeat struct {
	// Every is the count, RepeatEveryMin..RepeatEveryMax; 0 = unset.
	Every int
	// Unit is one of RepeatUnits; "" = unset.
	Unit string
	// Days is the selected-days mode: the weekdays this task applies to, each
	// one of RepeatDays(). Empty = not this mode. ALWAYS weekly by design
	// (#304): "every other Monday" is a cadence the interval mode already
	// spells as weeks, and allowing both would leave one task with two
	// cadences and no rule for which one the date arithmetic means.
	Days []string
	// Custom selects the Text mode.
	Custom bool
	// Text is the custom condition, stored verbatim.
	Text string
}

// RepeatDays lists the accepted weekday values, Monday first — the order
// Normalize sorts into, so one canonical spelling is stored for one logical
// selection regardless of the order the caller sent or the app's chip order.
func RepeatDays() []string {
	return []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}
}

func isRepeatDay(d string) bool {
	for _, v := range RepeatDays() {
		if d == v {
			return true
		}
	}
	return false
}

// DayLabel renders one weekday for humans: "mon" -> "Mon". An unrecognised
// value is returned unchanged rather than dropped, so an error message about a
// mistyped day can still name what was actually stored.
func DayLabel(day string) string {
	if day == "" {
		return day
	}
	// By RUNE, not by byte: canonicalisation keeps an unrecognised token so
	// the error can name it, and a token that starts with a multi-byte rune
	// would otherwise be split into invalid UTF-8 on its way into the message.
	r := []rune(day)
	return strings.ToUpper(string(r[0])) + string(r[1:])
}

// CanonicalRepeatDays trims, lowercases, de-duplicates and sorts a selection
// into the ONE spelling stored for it.
//
// Unknown tokens are KEPT, deliberately: dropping one would turn a typo into
// "no selection", i.e. silently into a one-shot task — the silent-wrong-answer
// class this package keeps having to close. Validate names them instead.
// A selection that canonicalises to nothing comes back nil, not an empty
// slice, so "absent" and "no days" are the same value everywhere (and an
// explicit [] from a client reads as one-shot, like every other zero).
func CanonicalRepeatDays(days []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(days))
	for _, d := range days {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	if len(out) == 0 {
		return nil
	}
	order := map[string]int{}
	for i, d := range RepeatDays() {
		order[d] = i
	}
	sort.SliceStable(out, func(i, j int) bool {
		oi, iok := order[out[i]]
		oj, jok := order[out[j]]
		switch {
		case iok && jok:
			return oi < oj
		// A recognised day always sorts before an unrecognised token, so the
		// stored order of a valid selection never depends on the junk.
		case iok != jok:
			return iok
		default:
			return out[i] < out[j]
		}
	})
	return out
}

// WeekdayOf maps a YYYY-MM-DD date to its canonical weekday value.
func WeekdayOf(date string) (string, bool) {
	d, err := time.Parse("2006-01-02", strings.TrimSpace(date))
	if err != nil {
		return "", false
	}
	return RepeatDays()[int(d.Weekday()+6)%7], true
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

// IsZero reports the one-shot case: no cadence, no selected days and no
// custom text.
func (r Repeat) IsZero() bool {
	return r.Every == 0 && r.Unit == "" && !r.Custom && r.Text == "" && len(r.Days) == 0
}

// Normalize infers the mode flag from what the caller actually sent, so
// clients that predate Repeat.Custom (and the 4.6 migration, which only
// knew the free-text rule) keep working unchanged: text with no
// structured cadence means a custom condition.
// Normalize canonicalises a Repeat so every caller stores the same thing for the same
// logical input.
//
// Text is trimmed HERE, not by the callers. It used to be trimmed by the MCP create
// path and by the update path but NOT by the HTTP create path, so the same custom
// condition arrived stored two different ways depending on which door it came through
// — and the idempotency digest, which mirrors what the store writes, then disagreed
// with the store about whether two bodies were the same request. Every create path
// already calls Normalize, so putting the rule here makes it the one place it can live
// and makes it impossible for a future fourth caller to skip it.
func (r Repeat) Normalize() Repeat {
	r.Text = strings.TrimSpace(r.Text)
	r.Days = CanonicalRepeatDays(r.Days)
	// The days mode is a cadence in its own right, so text alongside it is not
	// a custom condition that merely lacks the flag — inferring Custom there
	// made Validate report "set either a cadence or a custom condition, not
	// both" for a body whose real fault was a stray repeat_rule, and named the
	// wrong field to fix.
	if r.Text != "" && !r.Custom && r.Every == 0 && r.Unit == "" && len(r.Days) == 0 {
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
		if r.Every != 0 || r.Unit != "" || len(r.Days) > 0 {
			return fail("set either repeat_every/repeat_unit, repeat_days or a custom condition, not both")
		}
		return nil
	}
	if len(r.Days) > 0 {
		if r.Every != 0 || r.Unit != "" {
			return fail("set either repeat_every/repeat_unit or repeat_days, not both")
		}
		if strings.TrimSpace(r.Text) != "" {
			return fail("repeat_rule text needs repeat_custom = true")
		}
		for _, d := range r.Days {
			if !isRepeatDay(d) {
				return fail("repeat_days must be one of %s, got '%s'",
					strings.Join(RepeatDays(), "/"), d)
			}
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

// CheckDueDate enforces the selected-days rule: the due date the caller picked
// IS the first occurrence, so it has to be one of the days.
//
// Enforced here rather than in the date arithmetic because it is a property of
// the REQUEST (the two fields together), and the request is the only place the
// contradiction can be fixed. A "Mon, Wed, Fri" task due on a Thursday is not
// a detail the user would ever see until the second occurrence landed on a day
// they never picked — the failure is silent, which is why it is rejected
// loudly at the door. A no-op for every other mode.
func (r Repeat) CheckDueDate(dueDate string) error {
	r = r.Normalize()
	if len(r.Days) == 0 {
		return nil
	}
	day, ok := WeekdayOf(dueDate)
	if !ok {
		return fail("due_date %q is not a YYYY-MM-DD date", dueDate)
	}
	for _, d := range r.Days {
		if d == day {
			return nil
		}
	}
	return fail("due_date must be one of the selected days (%s), but %s is a %s",
		strings.Join(r.Days, ", "), dueDate, DayLabel(day))
}

// IsStructured reports the machine-usable modes: an interval cadence, or a
// selected-days cadence. Only a structured repeat can be rolled over
// automatically — a custom condition is the caller's to interpret.
func (r Repeat) IsStructured() bool {
	n := r.Normalize()
	if n.Custom {
		return false
	}
	return (n.Every > 0 && n.Unit != "") || len(n.Days) > 0
}

// MaxRollovers bounds the catch-up loop in NextDueDate.
const MaxRollovers = 1500

// NextDueDate advances a YYYY-MM-DD date to the next occurrence and returns
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
//
// The selected-days cadence steps to the NEXT selected weekday after the
// current one, strictly (the current date is an occurrence that has just
// happened). Its catch-up loop counts occurrences, exactly like the interval
// mode, so MaxRollovers bounds both the same way — an all-days task left
// untouched for over four years reports the same `exhausted` a daily one does.
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
		if len(step.Days) > 0 {
			stepped, ok := nextSelectedDay(next, step.Days)
			if !ok {
				// Unreachable through Validate (a non-empty selection always
				// matches within seven days); refusing is still the honest
				// answer for a stored selection that reached here some other
				// way, rather than looping on the same date.
				return "", false
			}
			next = stepped
		} else {
			next = advanceDate(next, step.Every, step.Unit)
		}
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

// nextSelectedDay returns the first day after d whose weekday is in days.
func nextSelectedDay(d time.Time, days []string) (time.Time, bool) {
	for i := 1; i <= 7; i++ {
		cand := d.AddDate(0, 0, i)
		day := RepeatDays()[int(cand.Weekday()+6)%7]
		for _, want := range days {
			if day == want {
				return cand, true
			}
		}
	}
	return d, false
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

// String renders the recurrence for humans: "Every 3 days", the selected days
// as "Mon, Wed, Fri", the custom words verbatim, or "" for a one-shot task.
func (r Repeat) String() string {
	r = r.Normalize()
	if len(r.Days) > 0 {
		labels := make([]string, 0, len(r.Days))
		for _, d := range r.Days {
			labels = append(labels, DayLabel(d))
		}
		return strings.Join(labels, ", ")
	}
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
	// An explicit empty array, never a nil one: a nil Go slice marshals to
	// BSON null, so the same one-shot task would store repeat_days as null on
	// one path and [] on another — and the app's `repeat_days` read would have
	// to special-case the null it was never meant to see.
	days := bson.A{}
	for _, d := range r.Days {
		days = append(days, d)
	}
	return bson.M{
		"repeat_every":  r.Every,
		"repeat_unit":   r.Unit,
		"repeat_days":   days,
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
	r.Days, _ = ParseRepeatDays(doc["repeat_days"])
	return r.Normalize()
}

// ParseRepeatDays reads a submitted or stored weekday selection.
//
// It accepts ALL the shapes the same value travels in: the `[]any` a JSON body
// decodes to, the `bson.A` a stored doc decodes to, and a `[]string` from a
// Go caller. ok is false when the field is present but is not an array of
// strings — a `repeat_days: "mon,wed"` or `[1,2]` is a caller bug, and callers
// reject that loudly rather than reading it as "no selection", which would turn
// it into a one-shot task. An ABSENT key (nil) is ok with no days: that is
// "this mode is not in play", which is a different answer from "mistyped".
func ParseRepeatDays(v any) ([]string, bool) {
	switch t := v.(type) {
	case nil:
		return nil, true
	case []string:
		return CanonicalRepeatDays(t), true
	case bson.A:
		return daysFromAny(t)
	case []any:
		return daysFromAny(t)
	default:
		return nil, false
	}
}

func daysFromAny(in []any) ([]string, bool) {
	out := make([]string, 0, len(in))
	for _, e := range in {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return CanonicalRepeatDays(out), true
}
