package cronjobs

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// catchupWindow is how late a scheduled fire may still run. Past it the
// fire is recorded missed instead: firing a morning nudge in the evening
// is worse than saying it was missed.
const catchupWindow = 5 * time.Minute

// recordTimeout bounds one history write. Records ride a detached context
// (see recordCtx), so this is the only deadline on them.
const recordTimeout = 10 * time.Second

// JobBackend is the storage the worker ticks against. *Store implements
// it; tests use a fake so no MongoDB is needed.
type JobBackend interface {
	List(ctx context.Context) ([]Job, error)
	Get(ctx context.Context, id string) (Job, error)
	RecordRun(ctx context.Context, id string, run Run, fireInstant int64) error
}

// Worker evaluates due jobs and fires them as chat-agent turns (#331).
// One process runs one worker: the tick loop is single-threaded and each
// fire is guarded by the in-flight set, so a job can never overlap
// itself (manual /run included).
//
// Throughput limit, stated plainly: all fires on one profile serialize on
// the chat turn, so one tick fires at most one job per profile — the rest
// skip as busy and retry next tick. Past the 5-minute catchup window the
// leftovers become misses. Fine for one or two jobs; five jobs due the
// same minute will systematically miss. Queueing (instead of fail-fast)
// would fix that at the cost of a late fire running many minutes behind
// its schedule — the skip keeps every fire near its minute.
//
// Fire-while-busy (the chat profile is answering another turn) is a skip,
// not a failure and not a miss: nothing is recorded and LastFire stays, so
// the next tick retries until the catchup window turns the fire into a miss.
// Recording every skipped tick would flood the capped history with "busy"
// rows and evict the answers the history exists to keep.
type Worker struct {
	Store JobBackend
	// Fire runs one job as a chat-agent turn in the recurring scheduler
	// conversation. Required: a nil Fire never fires, loudly.
	Fire FireFunc
	Logf func(format string, args ...any)

	mu       sync.Mutex
	inflight map[string]bool
}

func (w *Worker) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}

// recordCtx detaches one history write from the tick / request context with
// a short timeout. A client disconnecting mid-manual-run, or shutdown
// mid-tick, must not orphan the cron row for a turn the Runs list keeps —
// the record is the history, and it outlives whoever asked.
func recordCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), recordTimeout)
}

// Tick evaluates every unpaused job once. Fire errors are recorded as
// runs (visible in the history) without advancing LastFire, so the next
// tick retries until the catchup window passes and the fire is recorded
// missed instead. A busy chat agent (ErrBusy) records nothing at all, for
// the reason the Worker doc states.
func (w *Worker) Tick(ctx context.Context, now time.Time) {
	if w.Fire == nil {
		w.logf("cron: no fire function configured; skipping tick")
		return
	}
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
			// One detached context per write (not one per tick): N jobs
			// missing the same tick must not share a single 10s budget.
			rctx, rcancel := recordCtx()
			err := w.Store.RecordRun(rctx, job.ID,
				Run{At: now.UnixMilli(), Text: note, Missed: true,
					Source: RunSourceScheduler, JobName: job.Name},
				fireAt.UnixMilli())
			rcancel()
			if err != nil {
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
		rctx, rcancel := recordCtx()
		defer rcancel()
		res, err := w.Fire(ctx, job)
		now := time.Now().UnixMilli()
		if err != nil {
			if errors.Is(err, ErrBusy) {
				// Skip, not failure: no record, LastFire stays, the
				// next tick retries (see the Worker doc).
				w.logf("cron: job %s fire skipped, chat agent busy", job.ID)
				return
			}
			// Recorded but LastFire stays: the next tick retries
			// until the catchup window turns this into a miss. The
			// run id travels even on failure: a failed chat turn
			// still exists in the Runs list.
			w.logf("cron: job %s fire failed: %v", job.ID, err)
			if rerr := w.Store.RecordRun(rctx, job.ID,
				Run{At: now, Text: "error: " + err.Error(),
					Source: RunSourceScheduler, JobName: job.Name, RunID: res.RunID}, job.LastFire); rerr != nil {
				w.logf("cron: error record failed for %s: %v", job.ID, rerr)
			}
			return
		}
		if err := w.Store.RecordRun(rctx, job.ID,
			Run{At: now, Text: res.Text,
				Source: RunSourceScheduler, JobName: job.Name, RunID: res.RunID},
			fireAt.UnixMilli()); err != nil {
			w.logf("cron: run record failed for %s: %v", job.ID, err)
			return
		}
		w.logf("cron: job %s fired (%d chars)", job.ID, len(res.Text))
	}()
}

// FireNow runs one job immediately for the manual /run route, bypassing
// the schedule but not the in-flight guard. Paused jobs may be run by
// hand: the tap is explicit, and the pause only stops the ticker.
//
// runID names the agent run that answered (empty when the fire never became
// a turn), even on failure: a failed chat turn still exists in the Runs
// list, and the caller links to it rather than orphaning the error row.
func (w *Worker) FireNow(ctx context.Context, id string) (text, runID string, err error) {
	job, err := w.Store.Get(ctx, id)
	if err != nil {
		return "", "", err
	}
	if w.Fire == nil {
		return "", "", fmt.Errorf("cron: no fire function configured")
	}
	if !w.claim(job.ID) {
		return "", "", fmt.Errorf("job %s is already running", id)
	}
	defer w.release(job.ID)
	rctx, rcancel := recordCtx()
	defer rcancel()
	res, err := w.Fire(ctx, job)
	now := time.Now().UnixMilli()
	if err != nil {
		if !errors.Is(err, ErrBusy) {
			if rerr := w.Store.RecordRun(rctx, job.ID,
				Run{At: now, Text: "error: " + err.Error(),
					Source: RunSourceScheduler, JobName: job.Name, RunID: res.RunID}, job.LastFire); rerr != nil {
				w.logf("cron: error record failed for %s: %v", job.ID, rerr)
			}
		}
		return "", res.RunID, err
	}
	// A manual run accounts "now": it must not re-fire the same minute
	// on the next tick, nor resurrect a missed fire.
	if err := w.Store.RecordRun(rctx, job.ID,
		Run{At: now, Text: res.Text,
			Source: RunSourceScheduler, JobName: job.Name, RunID: res.RunID},
		now); err != nil {
		return "", res.RunID, err
	}
	return res.Text, res.RunID, nil
}
