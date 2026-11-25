// Package money implements the miser-money domain: IST day helpers,
// categories, and shared validation-adjacent helpers.
package money

import (
	"time"
)

// DefaultCategories seeds the user-managed money_categories collection on
// first boot. 'other' is the fallback when nothing matches, and it is
// reserved: it cannot be renamed, deleted, or renamed-to. The live set lives
// in MongoDB (see categories.go) — this slice is only the seed.
var DefaultCategories = []string{
	"groceries", "eating out", "transport", "bills", "rent",
	"shopping", "health", "fun", "salary", "other",
}

// IST is the operator timezone (UTC+5:30, no DST — ever — so a FixedZone is
// exact with no tzdata dependency). Server clocks run UTC; the user-visible
// "today" (default log date) is computed in IST so a log at 00:30 IST lands
// on the user's date, not UTC's yesterday.
// Instants (createdAt) stay UTC.
var IST = time.FixedZone("IST", 5*3600+30*60)

// TodayIST reports the current calendar day for the operator.
func TodayIST() string { return todayIn(IST, time.Now()) }

// todayIn renders a calendar day in the given zone. Split out so tests can
// pin midnight boundaries with fixed instants instead of racing time.Now().
func todayIn(loc *time.Location, t time.Time) string { return t.In(loc).Format("2006-01-02") }
