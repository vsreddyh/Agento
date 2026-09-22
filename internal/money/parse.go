// Package money implements the miser-money domain: free-text parsing plus
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

var categoryHints = map[string][]string{
	"groceries": {"grocer", "vegetable", "veggie", "fruit", "milk", "kirana", "supermarket"},
	"eating out": {"restaurant", "food", "lunch", "dinner", "breakfast", "cafe", "coffee",
		"pizza", "burger", "swiggy", "zomato", "eating out", "dining"},
	"transport": {"uber", "ola", "taxi", "cab", "bus", "train", "metro", "fuel", "petrol",
		"diesel", "parking", "flight", "transport", "travel"},
	"bills": {"electric", "water", "internet", "wifi", "phone", "mobile", "recharge",
		"gas bill", "utility", "bill"},
	"rent":        {"rent", "lease", "landlord"},
	"shopping":    {"amazon", "flipkart", "cloth", "shirt", "shoe", "dress", "shopping", "myntra"},
	"health":      {"doctor", "hospital", "medic", "pharma", "gym", "health"},
	"fun":         {"movie", "game", "party", "concert", "netflix", "fun", "entertainment"},
	"salary":      {"salary", "paycheck", "wage", "income", "credited", "salary credit"},
}

var amountRE = regexp.MustCompile(`(?i)(?:₹|rs\.?\s?|\$)?\s?(\d+(?:,\d+)*(?:\.\d{1,2})?)`)

var incomeWords = []string{"got", "received", "income", "salary", "credited", "earned", "pay", "deposit"}
var expenseWords = []string{"spent", "paid", "bought", "purchase", "expense", "cost", "spend"}

var months = map[string]int{
	"january": 1, "february": 2, "march": 3, "april": 4,
	"may": 5, "june": 6, "july": 7, "august": 8,
	"september": 9, "october": 10, "november": 11, "december": 12,
}

var monthNames = []string{"", "January", "February", "March", "April", "May", "June",
	"July", "August", "September", "October", "November", "December"}

var monthRE = regexp.MustCompile(`(january|february|march|april|may|june|july|august|september|october|november|december)`)
var yearRE = regexp.MustCompile(`(19|20)\d{2}`)
var (
	deleteRE   = regexp.MustCompile(`\b(remove|delete|undo)\b`)
	fixRE      = regexp.MustCompile(`\b(fix|correct|change|update)\b.*\b(to|as)\b`)
	transferRE = regexp.MustCompile(`\b(transfer|move|send)\b`)
	toRE       = regexp.MustCompile(`\bto\s+([A-Za-z][\w ]*)`)
)

// NormalizeCategory matches exact category name first, then keyword hints.
func NormalizeCategory(text string) string {
	t := strings.ToLower(text)
	for _, c := range Categories {
		if strings.Contains(t, c) {
			return c
		}
	}
	for _, c := range Categories {
		for _, h := range categoryHints[c] {
			if strings.Contains(t, h) {
				return c
			}
		}
	}
	return "other"
}

// ExtractAmount strips commas so "5,000" parses; nil when no digits found.
func ExtractAmount(text string) *float64 {
	m := amountRE.FindStringSubmatch(strings.ReplaceAll(text, ",", ""))
	if m == nil {
		return nil
	}
	f, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return nil
	}
	return &f
}

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

// Classification mirrors parse.classify: help → !command → delete/fix →
// question → transfer → income/expense.
type Classification struct {
	Action    string
	Amount    *float64
	Category  *string
	Type      *string
	Start     string
	End       string
	Label     string
	SendingTo *string
	Raw       string
}

func strptr(s string) *string { return &s }

// Classify free-form text into an action.
func Classify(text string) Classification {
	t := strings.TrimSpace(text)
	low := strings.ToLower(t)

	if low == "help" || low == "!help" || low == "commands" {
		return Classification{Action: "help"}
	}
	if strings.HasPrefix(low, "!") {
		return Classify(t[1:])
	}
	if deleteRE.MatchString(low) {
		return Classification{Action: "delete", Amount: ExtractAmount(t),
			Category: strptr(NormalizeCategory(t)), Raw: t}
	}
	if fixRE.MatchString(low) || strings.HasPrefix(low, "fix ") {
		return Classification{Action: "fix", Amount: ExtractAmount(t), Raw: t}
	}
	if hasAnyPrefix(low, "what", "how", "total", "show", "report", "summary", "breakdown", "biggest", "top") ||
		strings.Contains(t, "?") ||
		(strings.Contains(low, "spend") && (strings.Contains(low, "month") || strings.Contains(low, "total") || strings.Contains(low, "much"))) {
		s, e, label := ResolvePeriod(t, time.Now())
		var txType *string
		if strings.Contains(low, "income") || strings.Contains(low, "earn") {
			txType = strptr("income")
		} else if !strings.Contains(low, "income") &&
			(strings.Contains(low, "spend") || strings.Contains(low, "spent") || strings.Contains(low, "expense")) {
			txType = strptr("expense")
		}
		var cat *string
		if strings.Contains(low, " on ") || strings.Contains(low, " for ") {
			cat = strptr(NormalizeCategory(t))
		}
		return Classification{Action: "query", Start: s, End: e, Label: label, Type: txType, Category: cat, Raw: t}
	}

	amount := ExtractAmount(t)
	if transferRE.MatchString(low) && amount != nil {
		var dest *string
		if m := toRE.FindStringSubmatch(t); m != nil {
			d := strings.TrimRight(strings.TrimSpace(m[1]), " .!")
			dest = &d
		}
		return Classification{Action: "log_transfer", Amount: amount, SendingTo: dest, Raw: t}
	}
	if amount != nil {
		txType := "expense"
		if containsAny(low, incomeWords) && !containsAny(low, expenseWords) {
			txType = "income"
		}
		return Classification{Action: "log_" + txType, Amount: amount,
			Category: strptr(NormalizeCategory(t)), Raw: t}
	}
	if strings.Contains(low, "salary credited") || strings.Contains(low, "salary credit") ||
		strings.Contains(low, "income") {
		return Classification{Action: "log_income", Amount: nil,
			Category: strptr(NormalizeCategory(t)), Raw: t}
	}
	return Classification{Action: "unknown", Raw: t}
}

func hasAnyPrefix(s string, prefixes ...string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}
