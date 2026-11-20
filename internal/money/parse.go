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

var monthRE = regexp.MustCompile(`\b(january|february|march|april|may|june|july|august|september|october|november|december)\b`)
var yearRE = regexp.MustCompile(`(19|20)\d{2}`)

func monthRange(year, month int) (string, string) {
	first := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1)
	return first.Format("2006-01-02"), last.Format("2006-01-02")
}

// ResolvePeriod returns (start, end, label); defaults to the current month.
func ResolvePeriod(text string, today time.Time) (string, string, string) {
	t := strings.ToLower(text)
	if strings.Contains(t, "today") {
		s := today.Format("2006-01-02")
		return s, s, "today"
	}
	if strings.Contains(t, "yesterday") {
		y := today.AddDate(0, 0, -1).Format("2006-01-02")
		return y, y, "yesterday"
	}
	if strings.Contains(t, "this week") || strings.Contains(t, "thisweek") {
		wd := (int(today.Weekday()) + 6) % 7 // Monday-first
		s := today.AddDate(0, 0, -wd).Format("2006-01-02")
		return s, today.Format("2006-01-02"), "this week"
	}
	if strings.Contains(t, "last month") {
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
	if strings.Contains(t, "this month") || strings.Contains(t, "thismonth") ||
		strings.Contains(t, "so far") || strings.Contains(t, "month") {
		s, e := monthRange(today.Year(), int(today.Month()))
		return s, e, today.Format("January 2006")
	}
	s, e := monthRange(today.Year(), int(today.Month()))
	return s, e, today.Format("January 2006")
}
