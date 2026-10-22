package cronjobs

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	// SchedulerConversationID is the one recurring scheduler conversation on
	// the chat agent. Every job fires into this thread (each fire prefixed
	// with its job name), so fires share the conversation's history instead
	// of each carrying its own — and the Scheduler history and the Runs
	// list correlate on this id.
	SchedulerConversationID = "scheduler"

	// RunSourceScheduler marks runs recorded from a scheduler fire, so a
	// reader can tell a scheduled turn from a hand-run one without joining
	// collections.
	RunSourceScheduler = "scheduler"
)

// ErrBusy reports that a fire was skipped because the chat agent was already
// answering a turn. It is a skip, not a failure: the worker records nothing
// and leaves LastFire alone, so the next tick retries until the catchup
// window turns the fire into a miss. Callers compare with errors.Is — the
// chat-backed fire path wraps it.
var ErrBusy = errors.New("scheduler: chat agent is already answering a turn")

// SchedulerPrompt builds the turn text for one fire: the job name as
// provenance (the recurring thread carries many jobs, and the reply must say
// which one asked) plus the unattended tool policy.
//
// The policy is part of the prompt because the gateway cannot enforce it any
// other way: tool calls are mediated by the relay (cmd/mcp-gate), which holds
// approval-gated writes up to 14 minutes waiting for a human. A scheduler
// turn has no human watching, so it must not attempt gated writes at all —
// the TurnTimeout bounds the stall when it does anyway, but the instruction
// is what keeps the common case from stalling. Reads (notes, fetches,
// queries) run free, which is exactly what the motivating use case needs:
// the daily scan reuses applied/inapplicable notes without manual prompt
// edits.
func SchedulerPrompt(jobName, prompt string) string {
	name := strings.TrimSpace(jobName)
	if name == "" {
		name = "unnamed job"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From Scheduler %s: %s", name, strings.TrimSpace(prompt))
	b.WriteString("\n\nYou are firing unattended on a schedule: use read-only tools only " +
		"(notes reads, fetches, queries). Do not perform any write, delete, publish, " +
		"or approval-gated action; if one is needed, report it in the reply instead. " +
		"Nobody is watching to approve it.")
	return b.String()
}

// FireResult is one fire's outcome: the reply text plus the agent run id when
// the fire ran as a chat-agent turn (empty for the bare client, which has no
// run record). The worker stores the run id on the cron run so Scheduler
// history and the Runs list correlate both ways.
type FireResult struct {
	Text  string
	RunID string
}

// FireFunc fires one job. The production implementation runs a chat-agent
// turn (see gateway's FireSchedulerJob); the bare-client adapter below serves
// skill test-fires and unit tests. A busy agent reports ErrBusy, which the
// worker treats as a skip (no record, no LastFire advance) rather than a
// failure.
type FireFunc func(ctx context.Context, job Job) (FireResult, error)

// Fire runs one job through the bare model client: one prompt in, one reply
// out, no persona and no tools. Kept for skill test-fires (which need the
// same key and model but no job store) and unit tests — scheduled production
// fires go through the chat agent instead, so they can read notes and fetch
// live pages at fire time.
func (c *BareClient) Fire(ctx context.Context, job Job) (FireResult, error) {
	text, err := c.Run(ctx, SchedulerPrompt(job.Name, job.Prompt), "agento-cron-"+job.ID)
	if err != nil {
		return FireResult{}, err
	}
	return FireResult{Text: text}, nil
}
