package cronjobs

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// catchupWindow is how late a scheduled fire may still run. Past it the
// fire is recorded missed instead: firing a morning nudge in the evening
// is worse than saying it was missed.
const catchupWindow = 5 * time.Minute

// JobBackend is the storage the worker ticks against. *Store implements
// it; tests use a fake so no MongoDB is needed.
type JobBackend interface {
	List(ctx context.Context) ([]Job, error)
	Get(ctx context.Context, id string) (Job, error)
	RecordRun(ctx context.Context, id string, run Run, fireInstant int64) error
}

// Worker evaluates due jobs and fires them through the bare runner.
// One process runs one worker: the tick loop is single-threaded and each
// fire is guarded by the in-flight set, so a job can never overlap
// itself (manual /run included).
type Worker struct {
	Store  JobBackend
	Runner *BareClient
	Logf   func(format string, args ...any)

	mu       sync.Mutex
	inflight map[string]bool
}

func (w *Worker) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}

// Tick evaluates every unpaused job once. Fire errors are recorded as
// runs (visible in the history) without advancing LastFire, so the next
// tick retries until the catchup window passes and the fire is recorded
// missed instead.
func (w *Worker) Tick(ctx context.Context, now time.Time) {
	jobs, err := w.Store.List(ctx)
	if err != nil {
		w.logf("cron: list failed: %v", err)
		return
	}
	for _, job := range jobs {
		if job.Paused {
			continue
		}
		sched, err := Parse(job.Schedule)
		if err != nil {
			w.logf("cron: job %s has bad schedule %q: %v", job.ID, job.Schedule, err)
			continue
		}
		fireAt := sched.LatestBefore(now)
		if fireAt.IsZero() || fireAt.UnixMilli() <= job.LastFire {
			continue
		}
		if now.Sub(fireAt) > catchupWindow {
			note := fmt.Sprintf("missed fire at %s (gateway down)",
				fireAt.In(IST).Format("2006-01-02 15:04"))
			if err := w.Store.RecordRun(ctx, job.ID,
				Run{At: now.UnixMilli(), Text: note, Missed: true},
				fireAt.UnixMilli()); err != nil {
				w.logf("cron: miss record failed for %s: %v", job.ID, err)
			} else {
				w.logf("cron: job %s missed fire at %s", job.ID, note)
			}
			continue
		}
		w.fireAsync(ctx, job, fireAt)
	}
}

func (w *Worker) claim(id string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.inflight == nil {
		w.inflight = map[string]bool{}
	}
	if w.inflight[id] {
		return false
	}
	w.inflight[id] = true
	return true
}

func (w *Worker) release(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.inflight, id)
}

func (w *Worker) fireAsync(ctx context.Context, job Job, fireAt time.Time) {
	if !w.claim(job.ID) {
		return
	}
	go func() {
		defer w.release(job.ID)
		text, err := w.Runner.Run(ctx, job.Prompt, "agento-cron-"+job.ID)
		now := time.Now().UnixMilli()
		if err != nil {
			// Recorded but LastFire stays: the next tick retries
			// until the catchup window turns this into a miss.
			w.logf("cron: job %s fire failed: %v", job.ID, err)
			_ = w.Store.RecordRun(ctx, job.ID,
				Run{At: now, Text: "error: " + err.Error()}, job.LastFire)
			return
		}
		if err := w.Store.RecordRun(ctx, job.ID,
			Run{At: now, Text: text}, fireAt.UnixMilli()); err != nil {
			w.logf("cron: run record failed for %s: %v", job.ID, err)
			return
		}
		w.logf("cron: job %s fired (%d chars)", job.ID, len(text))
	}()
}

// FireNow runs one job immediately for the manual /run route, bypassing
// the schedule but not the in-flight guard. Paused jobs may be run by
// hand: the tap is explicit, and the pause only stops the ticker.
func (w *Worker) FireNow(ctx context.Context, id string) (string, error) {
	job, err := w.Store.Get(ctx, id)
	if err != nil {
		return "", err
	}
	if !w.claim(job.ID) {
		return "", fmt.Errorf("job %s is already running", id)
	}
	defer w.release(job.ID)
	text, err := w.Runner.Run(ctx, job.Prompt, "agento-cron-"+job.ID)
	now := time.Now().UnixMilli()
	if err != nil {
		_ = w.Store.RecordRun(ctx, job.ID,
			Run{At: now, Text: "error: " + err.Error()}, job.LastFire)
		return "", err
	}
	// A manual run accounts "now": it must not re-fire the same minute
	// on the next tick, nor resurrect a missed fire.
	if err := w.Store.RecordRun(ctx, job.ID,
		Run{At: now, Text: text}, now); err != nil {
		return "", err
	}
	return text, nil
}
