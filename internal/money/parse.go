// Package money implements the miser-money domain: MongoDB storage with
// transactional balance maintenance.
package money

import (
	"time"
)

// Categories; 'other' is the fallback when nothing matches.
var Categories = []string{
	"groceries", "eating out", "transport", "bills", "rent",
	"shopping", "health", "fun", "salary", "other",
}

// IST is the operator timezone (UTC+5:30, no DST — ever — so a FixedZone is
// exact with no tzdata dependency). Server clocks run UTC; the user-visible
// "today" (default log date) is computed in IST so a log at 00:30 IST lands
// on the user's date, not UTC's yesterday.
// Instants (createdAt) stay UTC. Retention math (the TTL `expiresAt` instant,
// mirrored by the retention container's `date` cutoff) also uses the same UTC
// frame as the TTL index (never IST) — boundary note: the container keeps
// `date == cutoff` while TTL expires that same row at 00:00 UTC, so the frame
// matches but the instants do not.
var IST = time.FixedZone("IST", 5*3600+30*60)

// TodayIST reports the current calendar day for the operator.
func TodayIST() string { return todayIn(IST, time.Now()) }

// todayIn renders a calendar day in the given zone. Split out so tests can
// pin midnight boundaries with fixed instants instead of racing time.Now().
func todayIn(loc *time.Location, t time.Time) string { return t.In(loc).Format("2006-01-02") }
