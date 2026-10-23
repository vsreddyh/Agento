// Package cronjobs: MongoDB-backed scheduled prompts ("cron chats").
//
// Collection: cron_jobs. One doc per job: name, prompt, cron schedule,
// paused flag, delivery target, last fire instant, and the short run
// history (capped, newest last).
//
// Fires run as turns on the chat agent (#331), not as bare model calls: one
// recurring scheduler conversation carries every job's fires (each prefixed
// with its job name), so a fire can read notes and fetch live pages at fire
// time. The bare client remains for skill test-fires and unit tests.
package cronjobs

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// IST is the evaluation zone: schedules are entered as wall-clock IST,
// like every other user-visible time in this stack.
var IST = time.FixedZone("IST", 5*3600+30*60)

// fieldSpec bounds one cron field.
type fieldSpec struct {
	min, max int
	names    map[string]int
}

// Schedule is a parsed 5-field cron expression: minute hour dom month dow.
// Each field is the flat set of matching values.
type Schedule struct {
	minute, hour, dom, month, dow map[int]bool
	domStar, dowStar              bool
}

var specs = []fieldSpec{
	{0, 59, nil},
	{0, 23, nil},
	{1, 31, nil},
	{1, 12, map[string]int{
		"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6,
		"jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12,
	}},
	{0, 7, map[string]int{
		"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6,
	}},
}

// Parse accepts a 5-field cron expression (minute hour dom month dow)
// plus @hourly, @daily, @weekly and @monthly. Fields allow *, */n, a-b,
// a-b/n and comma lists; month/dow accept 3-letter English names. Sunday
// is 0 or 7. Anything else is a validation error, reported to the caller
// rather than stored.
func Parse(expr string) (*Schedule, error) {
	switch strings.ToLower(strings.TrimSpace(expr)) {
	case "@hourly":
		expr = "0 * * * *"
	case "@daily":
		expr = "0 0 * * *"
	case "@weekly":
		expr = "0 0 * * 0"
	case "@monthly":
		expr = "0 0 1 * *"
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return nil, fmt.Errorf("schedule must have 5 fields (minute hour dom month dow), got %d", len(fields))
	}
	s := &Schedule{}
	sets := make([]map[int]bool, 5)
	for i, f := range fields {
		set, err := parseField(f, specs[i])
		if err != nil {
			return nil, fmt.Errorf("field %d (%q): %w", i+1, f, err)
		}
		sets[i] = set
	}
	s.minute, s.hour, s.dom, s.month, s.dow = sets[0], sets[1], sets[2], sets[3], sets[4]
	// Star detection drives the day rule below: an explicitly listed full
	// range reads the same as *, which is exactly what standard cron does.
	s.domStar = len(s.dom) == 31
	s.dowStar = len(s.dow) == 8
	return s, nil
}

func parseField(field string, spec fieldSpec) (map[int]bool, error) {
	out := map[int]bool{}
	for _, part := range strings.Split(field, ",") {
		step := 1
		base := part
		if cut := strings.Index(part, "/"); cut >= 0 {
			base = part[:cut]
			n, err := strconv.Atoi(part[cut+1:])
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("bad step in %q", part)
			}
			step = n
		}
		var lo, hi int
		switch {
		case base == "" || base == "*":
			lo, hi = spec.min, spec.max
		case strings.Contains(base, "-"):
			cut := strings.Index(base, "-")
			a, err1 := parseValue(base[:cut], spec)
			b, err2 := parseValue(base[cut+1:], spec)
			if err1 != nil || err2 != nil {
				return nil, fmt.Errorf("bad range in %q", part)
			}
			lo, hi = a, b
		default:
			v, err := parseValue(base, spec)
			if err != nil {
				return nil, fmt.Errorf("bad value in %q", part)
			}
			// A bare value with a step ("5/15") ranges to the max,
			// like standard cron; without a step it is one minute.
			lo, hi = v, v
			if strings.Contains(part, "/") {
				hi = spec.max
			}
		}
		if lo < spec.min || hi > spec.max || lo > hi {
			return nil, fmt.Errorf("out of range in %q", part)
		}
		for v := lo; v <= hi; v += step {
			out[v] = true
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty field %q", field)
	}
	return out, nil
}

func parseValue(s string, spec fieldSpec) (int, error) {
	if v, err := strconv.Atoi(s); err == nil {
		return v, nil
	}
	if v, ok := spec.names[strings.ToLower(s)]; ok {
		return v, nil
	}
	return 0, fmt.Errorf("bad value %q", s)
}

// Matches reports whether t (read in IST) is a fire minute.
func (s *Schedule) Matches(t time.Time) bool {
	l := t.In(IST)
	if !s.minute[l.Minute()] || !s.hour[l.Hour()] || !s.month[int(l.Month())] {
		return false
	}
	// Standard cron day rule: when both dom and dow are restricted, either
	// may match; when one is *, the other decides.
	domMatch := s.dom[l.Day()]
	dow := int(l.Weekday())
	dowMatch := s.dow[dow] || (dow == 0 && s.dow[7])
	switch {
	case s.domStar && s.dowStar:
		return true
	case s.domStar:
		return dowMatch
	case s.dowStar:
		return domMatch
	default:
		return domMatch || dowMatch
	}
}

// LatestBefore returns the latest fire minute <= t, scanning back at most
// 366 days. Zero time when nothing matches (a schedule that can never
// fire — rejected at a higher layer, but never loop forever here).
func (s *Schedule) LatestBefore(t time.Time) time.Time {
	cand := t.Truncate(time.Minute)
	for i := 0; i < 366*24*60; i++ {
		if s.Matches(cand) {
			return cand
		}
		cand = cand.Add(-time.Minute)
	}
	return time.Time{}
}

// Next returns the next fire minute strictly after t.
func (s *Schedule) Next(t time.Time) time.Time {
	cand := t.Truncate(time.Minute).Add(time.Minute)
	for i := 0; i < 366*24*60; i++ {
		if s.Matches(cand) {
			return cand
		}
		cand = cand.Add(time.Minute)
	}
	return time.Time{}
}
