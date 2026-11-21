// Package money implements the miser-money domain: period resolution plus
// MongoDB storage with transactional balance maintenance.
package money

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Categories; 'other' is the fallback when nothing matches.
var Categories = []string{
	"groceries", "eating out", "transport", "bills", "rent",
	"shopping", "health", "fun", "salary", "other",
}

var months = map[string]int{
	"january": 1, "february": 2, "march": 3, "april": 4,
	"may": 5, "june": 6, "july": 7, "august": 8,
	"september": 9, "october": 10, "november": 11, "december": 12,
}

var monthNames = []string{"", "January", "February", "March", "April", "May", "June",
	"July", "August", "September", "October", "November", "December"}

// IST is the operator timezone (UTC+5:30, no DST — ever — so a FixedZone is
// exact with no tzdata dependency). Server clocks run UTC; every user-visible
// "today" (default log date, period resolution, prune cutoff) is computed in
// IST so a log at 00:30 IST lands on the user's date, not UTC's yesterday.
// Instants (createdAt) stay UTC — only calendar-day boundaries use IST.
var IST = time.FixedZone("IST", 5*3600+30*60)

// TodayIST reports the current calendar day for the operator.
func TodayIST() string { return time.Now().In(IST).Format("2006-01-02") }

// NowIST reports the current instant in the operator zone, for period
// resolution and date-arithmetic cutoffs.
func NowIST() time.Time { return time.Now().In(IST) }

var monthRE = regexp.MustCompile(`\b(january|february|march|april|may|june|july|august|september|october|november|december)\b`)

// Year matches whole 1000-2999 numbers, so "June 2100" resolves to 2100
// instead of silently falling back to the current June (the old (19|20)
// prefix only covered 1900-2099).
var yearRE = regexp.MustCompile(`\b([12]\d{3})\b`)

// Period keywords match on whole words, like month names do: bare
// strings.Contains would resolve "todays"→today, "last monthly report"→last
// month and "monthly"/"trimonthly"→this month via substring, while "mayhem"
// correctly falls back — same class of input, opposite rules. Spaceless forms
// ("thisweek", "thismonth", "lastmonth") all work via \s*.
var (
	todayRE     = regexp.MustCompile(`\btoday\b`)
	yesterdayRE = regexp.MustCompile(`\byesterday\b`)
	thisWeekRE  = regexp.MustCompile(`\bthis\s*week\b`)
	lastMonthRE = regexp.MustCompile(`\blast\s*month\b`)
	thisMonthRE = regexp.MustCompile(`\bthis\s*month\b|\bso\s+far\b|\bmonth\b`)
)

func monthRange(year, month int) (string, string) {
	first := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1)
	return first.Format("2006-01-02"), last.Format("2006-01-02")
}

// ResolvePeriod returns (start, end, label); defaults to the current month.
//
// Supported phrases: `today`, `yesterday`, `this week` (Monday-first),
// `last month` (spaceless `lastmonth` works, like `thisweek`/`thismonth`),
// whole-word month names with an optional 1000-2999 year (`June`,
// `June 2025`, `June 2100` — a bare year alone is ignored), and `this month`
// / `so far` / bare `month`. Matching is on whole words throughout, so `maybe`/`mayhem`
// do not mean May and `todays`/`monthly` do not hit the day/month branches.
// Anything else falls through to the current month with a this-month label —
// callers that need to distinguish should enumerate, not probe.
func ResolvePeriod(text string, today time.Time) (string, string, string) {
	t := strings.ToLower(text)
	if todayRE.MatchString(t) {
		s := today.Format("2006-01-02")
		return s, s, "today"
	}
	if yesterdayRE.MatchString(t) {
		y := today.AddDate(0, 0, -1).Format("2006-01-02")
		return y, y, "yesterday"
	}
	if thisWeekRE.MatchString(t) {
		wd := (int(today.Weekday()) + 6) % 7 // Monday-first
		s := today.AddDate(0, 0, -wd).Format("2006-01-02")
		return s, today.Format("2006-01-02"), "this week"
	}
	if lastMonthRE.MatchString(t) {
		firstThis := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
		lastPrev := firstThis.AddDate(0, 0, -1)
		s, e := monthRange(lastPrev.Year(), int(lastPrev.Month()))
		return s, e, lastPrev.Format("January 2006")
	}
	if m := monthRE.FindStringSubmatch(t); m != nil {
		month := months[m[1]]
		year := today.Year()
		if y := yearRE.FindString(t); y != "" {
			year, _ = strconv.Atoi(y)
		} else if month > int(today.Month()) {
			year-- // "June" in January means last June
		}
		s, e := monthRange(year, month)
		return s, e, monthNames[month] + " " + strconv.Itoa(year)
	}
	if thisMonthRE.MatchString(t) {
		s, e := monthRange(today.Year(), int(today.Month()))
		return s, e, today.Format("January 2006")
	}
	s, e := monthRange(today.Year(), int(today.Month()))
	return s, e, today.Format("January 2006")
}
