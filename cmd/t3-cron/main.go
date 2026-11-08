// Command t3-cron owns the OS-cron side of scheduled prompts (#490).
//
// The in-process cronjobs.Worker used to tick inside pi-gateway and fire due
// rows from the MongoDB cron_jobs collection. Crons now have to keep firing
// with the pi container stopped, so this binary converts those rows into
// host crontab entries and runs one prompt at a time via `opencode run` from
// the repo root (which is what makes opencode.json's MCP servers available).
//
// Subcommands:
//
//	install   read cron_jobs, write the run/ cache, install the crontab block
//	dry-run   print the exact crontab block, write nothing
//	remove    remove the managed crontab block
//	list      show the generated jobs (from the cache)
//	run <id>  fire one job (called by the crontab lines)
//	check     log fires missed while the host was down
//
// The shell entry point scripts/t3-cron.sh loads the root .env and builds
// this binary; cron calls the shell script, never this file directly.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agento/internal/cronjobs"
)

const (
	cacheVersion = 1
	markerStart  = "# >>> t3-cron"
	markerEnd    = "# <<< t3-cron"
	// catchupWindow is how late a fire may be before `check` calls it a
	// miss. Crontab fires land within seconds; five minutes leaves room for
	// a slow host startup without reporting a fire that is still in flight.
	catchupWindow = 5 * time.Minute
	defaultModel  = "opencode-go/mimo-v2.6-flash"
	defaultLimit  = 10 * time.Minute
)

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func main() {
	os.Exit(run(os.Args[1:]))
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: t3-cron <install|dry-run|remove|list|run <id>|check>

install   read cron_jobs and install the host crontab block (writes run/t3-cron/)
dry-run   print the exact crontab block and exit (no writes)
remove    remove the managed crontab block
list      show the generated jobs
run <id>  fire one generated job via opencode
check     log scheduled fires missed while the host was down
`)
}

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	switch args[0] {
	case "install":
		return cmdInstall(false)
	case "dry-run":
		return cmdInstall(true)
	case "remove":
		return cmdRemove()
	case "list":
		return cmdList()
	case "run":
		if len(args) != 2 {
			usage()
			return 2
		}
		return cmdRun(args[1])
	case "check":
		return cmdCheck()
	case "help", "-h", "--help":
		usage()
		return 0
	default:
		usage()
		return 2
	}
}

func repoRoot() (string, error) {
	if v := strings.TrimSpace(os.Getenv("T3_CRON_REPO")); v != "" {
		return filepath.Abs(v)
	}
	return os.Getwd()
}

// hostZoneLabel names the daemon zone for the generated comment. time.Local
// prints the literal "Local", which hides whether the host is really UTC.
func hostZoneLabel() string {
	name, off := time.Now().Zone()
	if name == "" {
		name = "local"
	}
	sign := "+"
	if off < 0 {
		sign = "-"
		off = -off
	}
	if off == 0 {
		return name
	}
	return fmt.Sprintf("%s UTC%s%02d:%02d", name, sign, off/3600, (off%3600)/60)
}

func runDir(repo string) string    { return filepath.Join(repo, "run", "t3-cron") }
func logPath(repo string) string   { return filepath.Join(repo, "run", "t3-cron.log") }
func cachePath(repo string) string { return filepath.Join(runDir(repo), "jobs.json") }

// ────────────────────────────────────────────────────────────
// INSTALL / DRY-RUN
// ────────────────────────────────────────────────────────────

func cmdInstall(dryRun bool) int {
	repo, err := repoRoot()
	if err != nil {
		return fail("resolve repo root: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := cronjobs.FromEnv()
	if err != nil {
		return fail("open cron_jobs (set MONGODB_URI/MONGODB_DB): %v", err)
	}
	jobs, err := store.List(ctx)
	if err != nil {
		return fail("read cron_jobs: %v", err)
	}
	snapshot, err := cronjobs.SnapshotJobs(jobs)
	if err != nil {
		return fail("%v", err)
	}
	now := time.Now()
	entries, err := cronjobs.RenderJobs(snapshot, time.Local, now)
	if err != nil {
		return fail("%v", err)
	}
	block := renderBlock(entries, repo, now)

	if dryRun {
		fmt.Print(block)
		fmt.Fprintf(os.Stderr, "t3-cron: %d enabled job(s); host zone %s\n", len(entries), hostZoneLabel())
		return 0
	}

	file := cronjobs.OSJobsFile{
		Version:     cacheVersion,
		GeneratedAt: now.In(cronjobs.IST).Format(time.RFC3339),
		HostZone:    hostZoneLabel(),
		Jobs:        snapshot,
	}
	oldFile, _ := loadCache(repo)
	if err := saveCache(cachePath(repo), file); err != nil {
		return fail("write job cache: %v", err)
	}
	// A pause→resume or schedule edit must not report the paused/old-grid
	// interval as "host was down": baseline state for newly-enabled and
	// schedule-changed jobs so the next run/check starts counting from now.
	baselineChangedStates(repo, oldFile.Jobs, snapshot, now)
	if err := installCrontab(repo, block); err != nil {
		return fail("install crontab: %v", err)
	}
	fmt.Printf("t3-cron: installed %d enabled job(s) (host zone %s); log %s\n",
		len(entries), hostZoneLabel(), logPath(repo))
	return 0
}

func renderBlock(entries []cronjobs.RenderedEntry, repo string, now time.Time) string {
	var b strings.Builder
	zoneName := hostZoneLabel()
	fmt.Fprintf(&b, "%s (#490) — managed by scripts/t3-cron.sh; edits are overwritten on install >>>\n", markerStart)
	fmt.Fprintf(&b, "# Schedules are entered as IST wall clock and converted to this host's fixed\n")
	fmt.Fprintf(&b, "# offset (%s): Debian/Ubuntu cron has no per-user CRON_TZ. The 5 crontab\n", zoneName)
	fmt.Fprintf(&b, "# fields below are HOST-local time — not IST. scripts/t3-cron.sh exports\n")
	fmt.Fprintf(&b, "# TZ=Asia/Kolkata for the child so the turn's clock is IST.\n")
	fmt.Fprintf(&b, "# Generated %s\n", now.In(cronjobs.IST).Format("2006-01-02 15:04:05 IST"))
	if len(entries) == 0 {
		b.WriteString("# (no enabled jobs in cron_jobs)\n")
	}
	for _, e := range entries {
		name := strings.Join(strings.Fields(e.JobName), " ")
		if len([]rune(name)) > 160 {
			name = string([]rune(name)[:159]) + "…"
		}
		fmt.Fprintf(&b, "# job: %s (id %s) — %s IST\n", name, e.JobID, e.ISTSchedule)
		for _, sched := range e.Schedules {
			fmt.Fprintf(&b, "%s bash %s/scripts/t3-cron.sh run %s >> %s 2>&1\n",
				sched, repo, e.JobID, logPath(repo))
		}
	}
	fmt.Fprintf(&b, "%s <<<\n", markerEnd)
	return b.String()
}

func saveCache(path string, file cronjobs.OSJobsFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadCache(repo string) (cronjobs.OSJobsFile, error) {
	raw, err := os.ReadFile(cachePath(repo))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return cronjobs.OSJobsFile{}, fmt.Errorf("no generated jobs at %s — run scripts/t3-cron.sh install first", cachePath(repo))
		}
		return cronjobs.OSJobsFile{}, err
	}
	var file cronjobs.OSJobsFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return cronjobs.OSJobsFile{}, fmt.Errorf("parse %s: %w", cachePath(repo), err)
	}
	return file, nil
}

// ────────────────────────────────────────────────────────────
// CRONTAB
// ────────────────────────────────────────────────────────────

func stripBlock(crontab string) string {
	lines := strings.Split(crontab, "\n")
	out := make([]string, 0, len(lines))
	inBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, markerStart):
			inBlock = true
			continue
		case strings.HasPrefix(trimmed, markerEnd):
			inBlock = false
			continue
		}
		if !inBlock {
			out = append(out, line)
		}
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

func currentCrontab() string {
	out, err := exec.Command("crontab", "-l").Output()
	if err != nil {
		// "no crontab for <user>" is the normal first-run case, same as the
		// retention/sysmon installers treating it as empty.
		return ""
	}
	return string(out)
}

func installCrontab(repo, block string) error {
	if _, err := exec.LookPath("crontab"); err != nil {
		return fmt.Errorf("crontab not found — install cron (scripts/hermes.sh init does)")
	}
	if err := os.MkdirAll(filepath.Join(repo, "run"), 0o755); err != nil {
		return err
	}
	lock, err := acquireLock(filepath.Join(repo, "run", "cron.lock"), false)
	if err != nil {
		return err
	}
	if lock == nil {
		return fmt.Errorf("another crontab update is in progress — retry")
	}
	defer releaseLock(lock)

	kept := stripBlock(currentCrontab())
	next := block
	if kept != "" {
		next = kept + "\n" + block
	}
	cmd := exec.Command("crontab", "-")
	cmd.Stdin = strings.NewReader(next)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func cmdRemove() int {
	repo, err := repoRoot()
	if err != nil {
		return fail("resolve repo root: %v", err)
	}
	if _, err := exec.LookPath("crontab"); err != nil {
		// Nothing to remove on a host without cron; clean still succeeds.
		fmt.Fprintln(os.Stderr, "t3-cron: crontab not found; nothing to remove")
		return 0
	}
	if err := os.MkdirAll(filepath.Join(repo, "run"), 0o755); err != nil {
		return fail("%v", err)
	}
	lock, err := acquireLock(filepath.Join(repo, "run", "cron.lock"), false)
	if err != nil {
		return fail("%v", err)
	}
	if lock == nil {
		return fail("another crontab update is in progress — retry")
	}
	defer releaseLock(lock)

	kept := stripBlock(currentCrontab())
	cmd := exec.Command("crontab", "-")
	cmd.Stdin = strings.NewReader(kept + "\n")
	if kept == "" {
		cmd.Stdin = strings.NewReader("")
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fail("remove crontab block: %v: %s", err, strings.TrimSpace(string(out)))
	}
	fmt.Println("t3-cron: crontab block removed")
	return 0
}

// ────────────────────────────────────────────────────────────
// LIST
// ────────────────────────────────────────────────────────────

func cmdList() int {
	repo, err := repoRoot()
	if err != nil {
		return fail("resolve repo root: %v", err)
	}
	file, err := loadCache(repo)
	if err != nil {
		return fail("%v", err)
	}
	if len(file.Jobs) == 0 {
		fmt.Println("t3-cron: no scheduled prompts")
		return 0
	}
	sort.Slice(file.Jobs, func(i, j int) bool { return file.Jobs[i].ID < file.Jobs[j].ID })
	for _, j := range file.Jobs {
		state := "enabled"
		if !j.Enabled {
			state = "paused"
		}
		fmt.Printf("%-7s %-16s %-20s %s\n", state, j.Schedule, j.Name, j.ID)
	}
	return 0
}

// ────────────────────────────────────────────────────────────
// RUN
// ────────────────────────────────────────────────────────────

func cmdRun(id string) int {
	repo, err := repoRoot()
	if err != nil {
		return fail("resolve repo root: %v", err)
	}
	log := logPath(repo)
	if err := os.MkdirAll(runDir(repo), 0o755); err != nil {
		return fail("create run dir: %v", err)
	}
	if !validID.MatchString(id) {
		return runFail(log, id, "", "invalid job id %q", id)
	}
	file, err := loadCache(repo)
	if err != nil {
		return runFail(log, id, "", "%v", err)
	}
	var job *cronjobs.OSJob
	for i := range file.Jobs {
		if file.Jobs[i].ID == id {
			job = &file.Jobs[i]
			break
		}
	}
	if job == nil {
		return runFail(log, id, "", "job %s is not in the generated cache — re-run scripts/t3-cron.sh install", id)
	}
	if !job.Enabled {
		logLine(log, "skip job=%q id=%s status=paused", job.Name, id)
		return 0
	}

	// Resolve the due instant before taking the lock: a fire that loses the
	// lock still has to stamp it, or `check` would later blame the host for
	// being down.
	sched, err := cronjobs.Parse(job.Schedule)
	if err != nil {
		return runFail(log, id, job.Name, "bad schedule %q: %v", job.Schedule, err)
	}
	now := time.Now()
	fireAt := sched.LatestBefore(now)
	if fireAt.IsZero() {
		return runFail(log, id, job.Name, "schedule %q never fires", job.Schedule)
	}
	statePath := filepath.Join(runDir(repo), id+".state")

	lock, err := acquireLock(filepath.Join(runDir(repo), id+".lock"), false)
	if err != nil {
		return runFail(log, id, job.Name, "%v", err)
	}
	if lock == nil {
		// A previous fire is still running. Cron only invokes at the
		// schedule, so this is a doubled entry or a manual run; never let
		// two turns write the same domain concurrently. The running fire
		// owns this instant, so stamp it here as well — otherwise the next
		// `check` counts a skipped instant as a host-down miss.
		if err := writeState(statePath, fireAt); err != nil {
			logLine(log, "warn job=%q id=%s write state after skipped overlapping fire: %v", job.Name, id, err)
		}
		logLine(log, "skip job=%q id=%s status=already-running scheduled=%s",
			job.Name, id, fireAt.In(cronjobs.IST).Format(time.RFC3339))
		return 0
	}
	defer releaseLock(lock)

	last := readState(statePath)
	if !last.IsZero() {
		if first, missed := missedInstants(sched, last, fireAt); missed > 0 {
			logLine(log, "miss job=%q id=%s scheduled=%s count=%d (host was down)",
				job.Name, id, first.In(cronjobs.IST).Format(time.RFC3339), missed)
		}
	}
	// Stamp the attempt before it starts: `check` must never report this
	// instant as missed while the turn is running or after it fails.
	if err := writeState(statePath, fireAt); err != nil {
		return runFail(log, id, job.Name, "write state: %v", err)
	}

	// Intentionally writable — this is a reversal from the old in-process
	// worker, whose SchedulerPrompt restricted turns to read-only tools.
	// #490 requires fires to write tasks/projects (the phone polls them),
	// so each fire runs unattended with --auto: `ask` permissions in
	// opencode.json auto-approve, and only explicit `deny` rules still hold.
	// The guardrail below steers the turn toward the intended writes and
	// away from the destructive ones; a needed delete/prune/archive must be
	// reported in the reply instead of executed.
	prompt := fmt.Sprintf("From Scheduler %s: %s\n\nYou are firing unattended on a schedule: nobody is attached to approve anything. "+
		"You may create and update tasks, projects, reports and notes. Do not delete, prune, or archive anything; "+
		"if one of those is needed, report it in the reply instead.",
		job.Name, strings.TrimSpace(job.Prompt))
	model := strings.TrimSpace(os.Getenv("T3_CRON_MODEL"))
	if model == "" {
		model = defaultModel
	}
	timeout := defaultLimit
	if v := strings.TrimSpace(os.Getenv("T3_CRON_TIMEOUT")); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			timeout = d
		} else {
			logLine(log, "warn job=%q id=%s invalid T3_CRON_TIMEOUT=%q (using %s)", job.Name, id, v, defaultLimit)
		}
	}

	bin, err := opencodePath()
	if err != nil {
		return runFail(log, id, job.Name, "%v", err)
	}
	outPath := filepath.Join(runDir(repo), fmt.Sprintf("%s-%s.log", id, now.In(cronjobs.IST).Format("20060102T150405")))
	out, err := os.Create(outPath)
	if err != nil {
		return runFail(log, id, job.Name, "create per-run log: %v", err)
	}
	defer out.Close()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	args := []string{
		"run",
		"--auto",       // unattended: approve asks; explicit deny rules still hold
		"--archived",   // keep cron sessions out of the interactive session list
		"--standalone", // private server: no dependency on a TUI being open
		"--model", model,
		"--title", "cron: " + job.Name,
		prompt,
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = repo
	cmd.Env = os.Environ()
	cmd.Stdout = out
	cmd.Stderr = out

	started := time.Now()
	err = cmd.Run()
	duration := time.Since(started).Round(time.Millisecond)

	rc := 0
	if err != nil {
		rc = -1
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			rc = ee.ExitCode()
		}
		if ctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("timed out after %s", timeout)
		}
	}
	if err != nil {
		logLine(log, "fire job=%q id=%s scheduled=%s status=FAILED rc=%d duration=%s output=%s error=%q",
			job.Name, id, fireAt.In(cronjobs.IST).Format(time.RFC3339), rc, duration, outPath, err.Error())
		// Loud even when cron's mail is not configured: stderr is captured
		// by the crontab redirect, and the non-zero exit marks the fire.
		fmt.Fprintf(os.Stderr, "t3-cron: job %q (%s) FAILED rc=%d: %v (output: %s)\n", job.Name, id, rc, err, outPath)
		return 1
	}
	logLine(log, "fire job=%q id=%s scheduled=%s status=ok rc=0 duration=%s output=%s",
		job.Name, id, fireAt.In(cronjobs.IST).Format(time.RFC3339), duration, outPath)
	return 0
}

func runFail(log, id, name, format string, args ...any) int {
	msg := fmt.Sprintf(format, args...)
	logLine(log, "fire job=%q id=%s status=FAILED rc=-1 error=%q", name, id, msg)
	fmt.Fprintf(os.Stderr, "t3-cron: %s: %s\n", id, msg)
	return 1
}

func opencodePath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("T3_CRON_OPENCODE")); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("T3_CRON_OPENCODE=%q is not executable", p)
	}
	if p, err := exec.LookPath("opencode"); err == nil {
		return p, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".opencode", "bin", "opencode")
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("opencode CLI not found — run scripts/hermes.sh init or set T3_CRON_OPENCODE")
}

// baselineChangedStates stamps the current instant for jobs whose counting
// must restart: newly-enabled jobs, re-enabled jobs, and jobs whose schedule
// changed. Without this the next run/check would count the paused (or
// old-grid) interval as missed and log a phantom "host was down".
func baselineChangedStates(repo string, old, fresh []cronjobs.OSJob, now time.Time) {
	byID := map[string]cronjobs.OSJob{}
	for _, j := range old {
		byID[j.ID] = j
	}
	for _, j := range fresh {
		if !j.Enabled {
			continue
		}
		prev, ok := byID[j.ID]
		if ok && prev.Enabled && prev.Schedule == j.Schedule {
			continue
		}
		sched, err := cronjobs.Parse(j.Schedule)
		if err != nil {
			continue
		}
		if fireAt := sched.LatestBefore(now); !fireAt.IsZero() {
			_ = writeState(filepath.Join(runDir(repo), j.ID+".state"), fireAt)
		}
	}
}

// ────────────────────────────────────────────────────────────
// CHECK (misses)
// ────────────────────────────────────────────────────────────

func cmdCheck() int {
	repo, err := repoRoot()
	if err != nil {
		return fail("resolve repo root: %v", err)
	}
	file, err := loadCache(repo)
	if err != nil {
		// Before the first install there is nothing to reconcile; start
		// still has to come up, so this is a note, not a failure.
		fmt.Fprintf(os.Stderr, "t3-cron: %v\n", err)
		return 0
	}
	now := time.Now()
	for _, job := range file.Jobs {
		if !job.Enabled {
			continue
		}
		lock, err := acquireLock(filepath.Join(runDir(repo), job.ID+".lock"), false)
		if err != nil || lock == nil {
			continue // busy or unreadable: a running fire owns the state
		}
		sched, err := cronjobs.Parse(job.Schedule)
		if err != nil {
			releaseLock(lock)
			continue
		}
		statePath := filepath.Join(runDir(repo), job.ID+".state")
		last := readState(statePath)
		fireAt := sched.LatestBefore(now)
		if fireAt.IsZero() {
			releaseLock(lock)
			continue
		}
		if last.IsZero() {
			// First install: establish a baseline instead of reporting a
			// pre-migration instant as missed.
			_ = writeState(statePath, fireAt)
			releaseLock(lock)
			continue
		}
		if !fireAt.After(last) || now.Sub(fireAt) < catchupWindow {
			releaseLock(lock)
			continue
		}
		first, missed := missedInstants(sched, last, fireAt)
		if missed == 0 {
			releaseLock(lock)
			continue
		}
		line := fmt.Sprintf("miss job=%q id=%s scheduled=%s count=%d (host was down)",
			job.Name, job.ID, first.In(cronjobs.IST).Format(time.RFC3339), missed)
		logLine(logPath(repo), "%s", line)
		fmt.Println(line)
		_ = writeState(statePath, fireAt)
		releaseLock(lock)
	}
	return 0
}

// ────────────────────────────────────────────────────────────
// STATE / LOCK / LOG
// ────────────────────────────────────────────────────────────

func readState(path string) time.Time {
	raw, err := os.ReadFile(path)
	if err != nil {
		return time.Time{}
	}
	ms, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func writeState(path string, t time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.FormatInt(t.UnixMilli(), 10)+"\n"), 0o644)
}

func acquireLock(path string, block bool) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	how := syscall.LOCK_EX
	if !block {
		how |= syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}

func releaseLock(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

// logLine appends one line to the shared log. If the file cannot be written
// the same line goes to stderr and syslog via logger(1), so an unwritable log
// directory (or a cron mail-less host) cannot swallow a failure.
func logLine(path, format string, args ...any) {
	line := fmt.Sprintf("%s %s\n", time.Now().In(cronjobs.IST).Format(time.RFC3339), fmt.Sprintf(format, args...))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			_, werr := f.WriteString(line)
			f.Close()
			if werr == nil {
				return
			}
		}
	}
	fmt.Fprint(os.Stderr, "t3-cron: "+line)
	if p, err := exec.LookPath("logger"); err == nil {
		_ = exec.Command(p, "-t", "t3-cron", strings.TrimSpace(line)).Run()
	}
}

// missedInstants counts scheduled instants strictly between from (exclusive)
// and to (exclusive), returning the first one. On a normal fire to is simply
// the next instant after from, so this returns zero.
func missedInstants(s *cronjobs.Schedule, from, to time.Time) (time.Time, int) {
	first := s.Next(from)
	if first.IsZero() || !first.Before(to) {
		return time.Time{}, 0
	}
	n := 1
	cur := first
	for i := 0; i < 5000; i++ {
		next := s.Next(cur)
		if next.IsZero() || !next.Before(to) {
			break
		}
		cur = next
		n++
	}
	return first, n
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "t3-cron: "+format+"\n", args...)
	return 1
}
