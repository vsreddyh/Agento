package gateway

import (
	"context"
	"fmt"
	"sync"
	"time"

	"agento/internal/cronjobs"
	"agento/internal/runs"
)

// schedulerProfileName is the profile scheduler fires prefer. The app's Chat
// tab already addresses this name, so fires share the agent (and its tools)
// the user already talks to. Absent — a single-profile stack, for example —
// fires use the default profile instead of failing.
const schedulerProfileName = "chat"

// FireSchedulerJob runs one job as a turn on the chat agent (#331): the fire
// becomes a labelled conversation turn in the recurring scheduler thread,
// with the agent's tools (notes, fetch) available, instead of a bare
// one-prompt model call with no persona and no tools.
//
// It returns the reply text and the agent run id. The run lives in the Runs
// list under the scheduler conversation, and the worker stores the run id on
// the cron run, so the two histories correlate both ways. (The completion id
// is stable per conversation by construction; the run id — returned here —
// is the correlation unit, and the app keys reattach on the session id.)
//
// Concurrency: one turn per profile, so a fire landing while the profile is
// answering another turn reports ErrBusy (wrapping cronjobs.ErrBusy). The
// worker treats that as a skip — no record, LastFire stays, next tick
// retries — never as a failure.
//
// No model or reasoning selection happens here, deliberately: a scheduled
// fire must not repoint the profile's running model behind the user's back.
// It runs whatever the profile already runs, at its current level.
//
// Unattended tool policy is prompt-level (see cronjobs.SchedulerPrompt): the
// gateway cannot enforce read-only any other way — gated calls are held by
// the relay, which waits on a human that is not there. The TurnTimeout still
// bounds a turn that attempts one anyway.
//
// The context is intentionally unread (named _): the turn detaches like the
// chat path — a scheduler fire must outlive the tick that started it — and
// TurnTimeout is the bound, never the caller's deadline.
func (s *Server) FireSchedulerJob(_ context.Context, job cronjobs.Job) (string, string, error) {
	h, profile := s.schedulerHandler()
	if h == nil {
		return "", "", fmt.Errorf("scheduler: no agent available for job %s", job.ID)
	}
	if !h.turnMu.TryLock() {
		return "", "", fmt.Errorf("scheduler: fire %s on %s skipped: %w", job.ID, profile, cronjobs.ErrBusy)
	}
	// Ownership transfers to executeRun below; every early return releases.
	unlock := sync.OnceFunc(h.turnMu.Unlock)
	started := false
	turnCtx, cancel := context.WithTimeout(context.Background(), s.cfg.TurnTimeout)
	defer func() {
		if !started {
			unlock()
			cancel()
		}
	}()

	setupCtx, setupCancel := context.WithTimeout(turnCtx, setupCallTimeout)
	defer setupCancel()
	agentState, err := h.runner.resolveSession(setupCtx, cronjobs.SchedulerConversationID)
	if err != nil {
		s.cfg.Logf("scheduler: job %s: resolving session: %v", job.ID, err)
		return "", "", fmt.Errorf("scheduler: could not resolve the scheduler conversation: %w", err)
	}
	model := bounded(agentState.Model.ID)
	provider := bounded(agentState.Model.Provider)

	// Subscribe before prompting: the agent can stream before the prompt
	// response arrives, same as the chat path.
	events, unsubscribe := h.runner.agent.Subscribe()

	prompt := cronjobs.SchedulerPrompt(job.Name, job.Prompt)
	lr := s.runs.begin(runs.Run{
		Profile:        profile,
		ConversationID: cronjobs.SchedulerConversationID,
		Prompt:         prompt,
		// Recorded, never selected: a fire runs whatever the profile
		// already runs, but the row must still say what answered, or
		// model-grouped Runs views silently drop scheduler turns.
		Provider: provider,
		Model:    model,
	}, h.runner.agent,
		completionIDFor(cronjobs.SchedulerConversationID), "", time.Now())
	userTurn := &turn{Text: prompt}

	started = true
	// Synchronous: executeRun returns after the turn settles (its defer
	// finishes the run), so the snapshot below is terminal.
	s.executeRun(lr, h, turnCtx, cancel, events, unsubscribe, userTurn, model, func() {}, unlock)

	snap := lr.snapshot()
	s.cfg.Logf("scheduler: job %s fired on %s as run %s (%d chars)",
		job.ID, profile, snap.ID, len(snap.Output))
	switch {
	case snap.Status == runs.StatusCompleted:
		return snap.Output, snap.ID, nil
	case snap.Error != "":
		return "", snap.ID, fmt.Errorf("scheduler: job %s fire failed: %s", job.ID, snap.Error)
	default:
		return "", snap.ID, fmt.Errorf("scheduler: job %s ended as %s", job.ID, snap.Status)
	}
}

// schedulerHandler picks the chat profile when configured, else the default.
// The profile name is resolved per fire rather than cached: profiles are
// fixed for the process, but a lookup is cheap and keeps one code path.
//
// The thread is per-profile, not global: the scheduler conversation id is
// the same string everywhere, but sessions (and their histories) belong to
// the profile's agent. A stack that changes shape across restarts
// (single-profile becomes multi-profile or vice versa) therefore starts a
// fresh scheduler thread on the newly preferred profile rather than
// resuming the old one — accepted, and the job name on every fire still
// says which job asked.
func (s *Server) schedulerHandler() (*agentHandler, string) {
	if h, ok := s.handlers[schedulerProfileName]; ok {
		return h, schedulerProfileName
	}
	h, ok := s.handlers[s.cfg.DefaultProfile]
	if !ok {
		return nil, ""
	}
	return h, s.cfg.DefaultProfile
}
