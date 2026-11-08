package cronjobs

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// OS cron migration (#490).
//
// The in-process Worker is retired: schedules now live in the host crontab
// and are executed by scripts/t3-cron.sh. This file owns the two pieces the
// migration needs to be honest:
//
//   - The generated job cache (OSJob/OSJobsFile), so a fire runs the prompt
//     captured at install time without needing MongoDB at 03:00.
//   - The conversion from an IST wall-clock 5-field schedule (what users
//     entered in the app, what cronjobs.Parse accepts) to the host cron
//     daemon's zone.
//
// The conversion exists because Debian/Ubuntu cron has no per-user
// CRON_TZ (man 5 crontab: "It currently does not support per-user
// timezones"). Emitting the IST expression unchanged would run every job
// 5h30 late on a UTC host. Entries are therefore emitted in the daemon's
// zone, which is fixed at install time. Only fixed-offset host zones are
// accepted: a DST host maps one IST wall clock to two local times, which
// cannot be expressed as one set of 5-field lines, and silently picking one
// would fire jobs an hour early or late for half the year.

const maxPromptBytes = 64 * 1024

// OSJob is one scheduled prompt as generated from the cron_jobs collection.
// The prompt is captured here so an unattended fire never depends on Mongo
// being reachable just to know what to say.
type OSJob struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Prompt   string `json:"prompt"`
	Schedule string `json:"schedule"` // IST wall clock, 5-field
	Enabled  bool   `json:"enabled"`
}

// OSJobsFile is the run/-scoped cache written by `t3-cron install`.
type OSJobsFile struct {
	Version     int     `json:"version"`
	GeneratedAt string  `json:"generatedAt"`
	HostZone    string  `json:"hostZone"`
	Jobs        []OSJob `json:"jobs"`
}

// RenderedEntry is one enabled job and the host-zone expression(s) it
// expands to. A single IST schedule may need several lines when the
// conversion moves a fire across midnight.
type RenderedEntry struct {
	JobID       string
	JobName     string
	ISTSchedule string
	Schedules   []string
}

// SnapshotJobs reads every cron_jobs row into the install-time cache.
// Paused rows are kept as disabled so `list` can show them; rendering skips
// them. A malformed schedule or blank prompt fails the snapshot loudly:
// installing a cron line that can never fire, or that fires an empty
// prompt, is worse than refusing the install.
func SnapshotJobs(jobs []Job) ([]OSJob, error) {
	out := make([]OSJob, 0, len(jobs))
	for _, j := range jobs {
		name := strings.Join(strings.Fields(j.Name), " ")
		if name == "" {
			name = "unnamed job"
		}
		sched := strings.TrimSpace(j.Schedule)
		if _, err := Parse(sched); err != nil {
			return nil, fmt.Errorf("job %s (%q): bad schedule %q: %w", j.ID, name, j.Schedule, err)
		}
		if strings.TrimSpace(j.Prompt) == "" {
			return nil, fmt.Errorf("job %s (%q): prompt is empty", j.ID, name)
		}
		// Prompts travel as one argv element to `opencode run`; keep well
		// under ARG_MAX so a pathological row fails at install with a clear
		// message instead of failing every scheduled fire forever.
		if len(j.Prompt) > maxPromptBytes {
			return nil, fmt.Errorf("job %s (%q): prompt is %d bytes (max %d)", j.ID, name, len(j.Prompt), maxPromptBytes)
		}
		out = append(out, OSJob{
			ID:       j.ID,
			Name:     name,
			Prompt:   j.Prompt,
			Schedule: sched,
			Enabled:  !j.Paused,
		})
	}
	return out, nil
}

// RenderJobs converts every enabled job to host-zone cron expressions.
// now anchors the DST check and is injectable for tests.
func RenderJobs(jobs []OSJob, host *time.Location, now time.Time) ([]RenderedEntry, error) {
	shift, err := zoneShiftMinutes(host, now)
	if err != nil {
		return nil, err
	}
	out := make([]RenderedEntry, 0, len(jobs))
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		s, err := Parse(j.Schedule)
		if err != nil {
			return nil, fmt.Errorf("job %s (%q): bad schedule %q: %w", j.ID, j.Name, j.Schedule, err)
		}
		exprs, err := hostExpressions(s, shift)
		if err != nil {
			return nil, fmt.Errorf("job %s (%q): %w", j.ID, j.Name, err)
		}
		out = append(out, RenderedEntry{
			JobID:       j.ID,
			JobName:     j.Name,
			ISTSchedule: j.Schedule,
			Schedules:   exprs,
		})
	}
	return out, nil
}

// zoneShiftMinutes returns how many minutes host wall clock is behind IST
// (positive on a UTC host). Fixed-offset zones only; see the file comment.
func zoneShiftMinutes(host *time.Location, now time.Time) (int, error) {
	if host == nil {
		host = time.Local
	}
	jan := time.Date(now.Year(), time.January, 15, 12, 0, 0, 0, host)
	jul := time.Date(now.Year(), time.July, 15, 12, 0, 0, 0, host)
	_, janOff := jan.Zone()
	_, julOff := jul.Zone()
	if janOff != julOff {
		return 0, fmt.Errorf("host time zone %q observes DST; OS cron cannot express a fixed IST schedule there — run the daemon in UTC (or another fixed-offset zone) and re-run install", host.String())
	}
	// IST is a fixed zone (cronjobs.IST). Offsets are in seconds and every
	// real zone offset is a whole number of minutes.
	const istOff = (5*3600 + 30*60)
	diff := istOff - janOff
	if diff%60 != 0 {
		return 0, fmt.Errorf("host time zone %q has a sub-minute offset from IST", host.String())
	}
	return diff / 60, nil
}

// hostExpressions converts one parsed IST schedule using shift (minutes
// behind IST). Day-of-month/month restricted schedules that cross midnight
// have no equivalent 5-field expression and are rejected with an
// actionable message rather than silently firing on the wrong date.
func hostExpressions(s *Schedule, shift int) ([]string, error) {
	minutes := sortedInts(s.minute)
	hours := sortedInts(s.hour)
	domFull := len(s.dom) == 31
	monthFull := len(s.month) == 12
	dowFull := len(s.dow) == 8

	dows := make([]int, 0, 7)
	for d := 0; d <= 6; d++ {
		if s.dow[d] || s.dow[7] {
			dows = append(dows, d)
		}
	}

	if domFull && monthFull {
		return dailyExpressions(minutes, hours, dows, dowFull, shift)
	}

	// Day-restricted schedules: shifting is only faithful while the fire
	// stays on the same IST calendar date. Otherwise dom/month would have
	// to move with the calendar, which 5-field cron cannot express.
	for _, h := range hours {
		for _, m := range minutes {
			total := h*60 + m - shift
			if total < 0 || total >= 24*60 {
				return nil, fmt.Errorf("schedule %q crosses a calendar date when converted to the host zone; move it to 05:30 IST or later (or express it as a daily/weekly job)", s.String())
			}
		}
	}

	shiftedMinutes := map[int]bool{}
	shiftedHours := map[int]bool{}
	for _, h := range hours {
		for _, m := range minutes {
			total := h*60 + m - shift
			shiftedMinutes[total%60] = true
			shiftedHours[total/60] = true
		}
	}
	dom := "*"
	if !domFull {
		dom = numList(sortedInts(s.dom), 1, 31)
	}
	month := "*"
	if !monthFull {
		month = numList(sortedInts(s.month), 1, 12)
	}
	dow := "*"
	if !dowFull {
		dow = numList(dows, 0, 6)
	}
	return []string{
		fmt.Sprintf("%s %s %s %s %s",
			numList(sortedInts(shiftedMinutes), 0, 59),
			numList(sortedInts(shiftedHours), 0, 23), dom, month, dow),
	}, nil
}

type osClock struct{ m, h int }

// dailyExpressions handles schedules whose dom and month are unrestricted:
// every fire is expressible as weekday + clock time, so a -330 minute shift
// only moves the weekday. Identical weekday sets collapse back to "*".
func dailyExpressions(minutes, hours, dows []int, dowFull bool, shift int) ([]string, error) {
	byDow := map[int]map[osClock]bool{}
	for _, d := range dows {
		for _, h := range hours {
			for _, m := range minutes {
				total := h*60 + m - shift
				ud := d
				for total < 0 {
					total += 24 * 60
					ud = (ud + 6) % 7
				}
				for total >= 24*60 {
					total -= 24 * 60
					ud = (ud + 1) % 7
				}
				if byDow[ud] == nil {
					byDow[ud] = map[osClock]bool{}
				}
				byDow[ud][osClock{total % 60, total / 60}] = true
			}
		}
	}

	out := []string{}
	if dowFull && len(byDow) == 7 {
		want := clockSignature(byDow[0])
		same := true
		for d := 1; d < 7; d++ {
			if clockSignature(byDow[d]) != want {
				same = false
				break
			}
		}
		if same {
			return []string{clockExpression(byDow[0]) + " *"}, nil
		}
	}
	for d := 0; d < 7; d++ {
		set, ok := byDow[d]
		if !ok {
			continue
		}
		out = append(out, fmt.Sprintf("%s %d", clockExpression(set), d))
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("schedule never fires")
	}
	return out, nil
}

func clockSignature(set map[osClock]bool) string {
	clocks := make([]osClock, 0, len(set))
	for c := range set {
		clocks = append(clocks, c)
	}
	sort.Slice(clocks, func(i, j int) bool {
		if clocks[i].h != clocks[j].h {
			return clocks[i].h < clocks[j].h
		}
		return clocks[i].m < clocks[j].m
	})
	var b strings.Builder
	for _, c := range clocks {
		fmt.Fprintf(&b, "%d:%d,", c.m, c.h)
	}
	return b.String()
}

func clockExpression(set map[osClock]bool) string {
	minutes := map[int]bool{}
	hours := map[int]bool{}
	for c := range set {
		minutes[c.m] = true
		hours[c.h] = true
	}
	return fmt.Sprintf("%s %s * *",
		numList(sortedInts(minutes), 0, 59),
		numList(sortedInts(hours), 0, 23))
}

func sortedInts(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
}

// numList renders a cron field, collapsing full ranges to * and arithmetic
// runs to a-b/n. min/max bound the field (0-59 minute, 0-23 hour, 1-31 dom,
// 1-12 month, 0-6 dow after Sunday=7 is normalised).
func numList(vals []int, min, max int) string {
	if len(vals) == 0 {
		return "*"
	}
	if len(vals) == max-min+1 {
		return "*"
	}
	step := 1
	if len(vals) > 1 {
		step = vals[1] - vals[0]
		for i := 2; i < len(vals); i++ {
			if vals[i]-vals[i-1] != step {
				step = 0
				break
			}
		}
	}
	if step > 1 && len(vals) >= 3 {
		if vals[0] == min {
			return fmt.Sprintf("*/%d", step)
		}
		return fmt.Sprintf("%d-%d/%d", vals[0], vals[len(vals)-1], step)
	}
	parts := make([]string, len(vals))
	for i, v := range vals {
		parts[i] = fmt.Sprintf("%d", v)
	}
	return strings.Join(parts, ",")
}

// String renders the schedule's fields for error messages.
func (s *Schedule) String() string {
	dows := make([]int, 0, 7)
	for d := 0; d <= 6; d++ {
		if s.dow[d] || s.dow[7] {
			dows = append(dows, d)
		}
	}
	return strings.Join([]string{
		numList(sortedInts(s.minute), 0, 59),
		numList(sortedInts(s.hour), 0, 23),
		numList(sortedInts(s.dom), 1, 31),
		numList(sortedInts(s.month), 1, 12),
		numList(dows, 0, 6),
	}, " ")
}
