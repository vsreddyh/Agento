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
	//
	// Reserved: nothing validates user conversation ids against it, so a
	// user thread with this id on the chat profile would merge with the
	// scheduler thread. Low risk today (ids are app-generated), but do not
	// reuse this string for anything else.
	SchedulerConversationID = "scheduler"

	// RunSourceScheduler marks runs recorded from a scheduler fire, so a
	// reader can tell a scheduled turn from a hand-run one without joining
	// collections.
	RunSourceScheduler = "scheduler"

	// maxSchedulerName caps the job-name label in runes. The prompt carries
	// the name, not the name's formatting (see SchedulerPrompt).
	maxSchedulerName = 80
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
// The name is sanitized, not interpolated verbatim: job names are
// user-controlled (POST /api/jobs), and a newline in one would break the
// label line and spoof the thread. Whitespace runs collapse to one space
// and the label caps at 80 runes — the prompt carries the name, not the
// name's formatting.
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
	name := strings.Join(strings.Fields(jobName), " ")
	switch {
	case name == "":
		name = "unnamed job"
	case len([]rune(name)) > maxSchedulerName:
		// An ellipsis marks the cut: two names differing only past the
		// cap would otherwise label identically in the model-visible
		// thread (storage still disambiguates via RunID).
		name = truncateRunes(name, maxSchedulerName-1) + "…"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "From Scheduler %s: %s", name, strings.TrimSpace(prompt))
	b.WriteString("\n\nYou are firing unattended on a schedule: use read-only tools only " +
		"(notes reads, fetches, queries). Do not perform any write, delete, publish, " +
		"or approval-gated action; if one is needed, report it in the reply instead. " +
		"Nobody is watching to approve it.")
	return b.String()
}

// truncateRunes caps s at limit runes without splitting one.
func truncateRunes(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return string(runes[:limit])
}

// FireResult is one fire's outcome: the reply text plus the agent run id.
// RunID is set whenever a run began — including on failure, so the error
// row links to the turn that failed — and empty only when the fire never
// became a turn (busy skip, bare client). The worker stores the run id on
// the cron run so Scheduler history and the Runs list correlate both ways.
type FireResult struct {
	Text  string
	RunID string
}

// FireFunc fires one job. The production implementation runs a chat-agent
// turn (see gateway's FireSchedulerJob); tests use the bare-client adapter
// (FireForTest) or inline stubs. A busy agent reports ErrBusy, which the
// worker treats as a skip (no record, no LastFire advance) rather than a
// failure.
//
// Detach contract: implementations must bound the turn themselves and must
// NOT tie its lifetime to ctx — the worker passes the tick / request
// context, which can die (shutdown, client disconnect) while the turn
// legitimately runs on. Callers likewise must not assume cancellation
// propagates through Fire.
type FireFunc func(ctx context.Context, job Job) (FireResult, error)

// FireForTest runs one job through the bare model client: one prompt in,
// one reply out, no persona and no tools. Test-only by name: production
// scheduler fires go through the chat agent, and skill test-fires use Run
// directly (unlabeled — a test procedure is not a scheduler fire, so it
// must not carry the From Scheduler prefix or the unattended policy).
// Wiring a skill test through this would silently mislabel it.
func (c *BareClient) FireForTest(ctx context.Context, job Job) (FireResult, error) {
	text, err := c.Run(ctx, SchedulerPrompt(job.Name, job.Prompt), "agento-cron-"+job.ID)
	if err != nil {
		return FireResult{}, err
	}
	return FireResult{Text: text}, nil
}
