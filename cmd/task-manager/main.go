// Command task-manager is the personal task-manager MCP server.
//
// Storage: MongoDB (tasks collection). No status field — a task is open
// while completedAt is null, done once set. Completed tasks expire via
// TTL 3 days after completion; a repeat is stored but never interpreted
// here (agent-side per pi/skills/task-manager/SKILL.md).
// Runs over stdio for MCP clients.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"agento/internal/mongostore"
	"agento/internal/tasks"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var store *tasks.Store

// repeatOf reads a task doc's recurrence back out of a response map.
func repeatOf(doc map[string]any) tasks.Repeat {
	rep := tasks.Repeat{}
	if s, ok := doc["repeat_rule"].(string); ok {
		rep.Text = s
	}
	if n, ok := mongostore.ToInt(doc["repeat_every"]); ok {
		rep.Every = n
	}
	if u, ok := doc["repeat_unit"].(string); ok {
		rep.Unit = u
	}
	if b, ok := doc["repeat_custom"].(bool); ok {
		rep.Custom = b
	}
	// The selection, leniently — a doc is read for display (rep.String()) and
	// for the follow_up hint, so an unreadable shape must not fail the call.
	rep.Days, _ = tasks.ParseRepeatDays(doc["repeat_days"])
	return rep.Normalize()
}

// repeatHint spells out the exact create_task keys that reproduce a
// recurrence, so the agent copies a cadence instead of re-deriving it.
func repeatHint(rep tasks.Repeat) string {
	if rep.Custom {
		// %q, not quotes: a custom condition may itself contain an
		// apostrophe, and this string is copied by the agent.
		return fmt.Sprintf("repeat_custom: true, repeat_rule: %q", rep.Text)
	}
	if len(rep.Days) > 0 {
		return fmt.Sprintf("repeat_days: %s", daysHint(rep.Days))
	}
	return fmt.Sprintf("repeat_every: %d, repeat_unit: %q", rep.Every, rep.Unit)
}

// daysHint renders a selection as the JSON-shaped literal an agent can copy,
// quoted like every other value in this instruction.
func daysHint(days []string) string {
	parts := make([]string, 0, len(days))
	for _, d := range days {
		parts = append(parts, fmt.Sprintf("%q", d))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func fail(err error) (*mcp.CallToolResult, map[string]any, error) {
	out := map[string]any{"ok": false, "error": err.Error()}
	// A lost revision race is actionable, not just reportable: the agent
	// re-reads the task and retries, rather than re-sending the same edit.
	// Structured (not parsed out of the message) so the branch is exact.
	var se *tasks.StoreError
	if errors.As(err, &se) && se.Conflict {
		out["conflict"] = true
	}
	return nil, out, nil
}

func result(out map[string]any) (*mcp.CallToolResult, map[string]any, error) {
	b, _ := json.Marshal(out)
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
		StructuredContent: out,
	}, out, nil
}

// Input types are named rather than inline so that `internal/skills`-style assertions
// and `cmd/task-manager/schema_test.go` can read the SAME tags the SDK infers the wire
// schema from. An inline struct can only be checked by reading the handler, and the
// handler and the published schema disagreed for the whole life of this file.
//
// Optionality here is not cosmetic. The SDK treats a field as required unless its tag
// carries `omitempty`, so a pointer field without it is advertised as mandatory — and an
// agent that believes it must send `due_date` cannot make the partial edit that avoids
// clobbering a field it did not mean to touch.
type createTaskInput struct {
	Name             string `json:"name"`
	SpeakName        string `json:"speak_name,omitempty"`
	Description      string `json:"description"`
	DueDate          string `json:"due_date"`
	DueTime          string `json:"due_time"`
	EstimatedMinutes *int   `json:"estimated_minutes"`
	// The repeat is optional as a block: no keys means one-shot. Each key is
	// individually optional too, since Normalize treats a missing key as zero.
	RepeatEvery  *int     `json:"repeat_every,omitempty"`
	RepeatUnit   string   `json:"repeat_unit,omitempty"`
	RepeatDays   []string `json:"repeat_days,omitempty"`
	RepeatCustom *bool    `json:"repeat_custom,omitempty"`
	RepeatRule   string   `json:"repeat_rule,omitempty"`
	// Required by store.Create, which rejects a task with no estimate and one with no
	// parallelable flag — so it stays required in the schema despite being a pointer.
	Parallelable *bool `json:"parallelable"`
}

type listTasksInput struct {
	State   string `json:"state,omitempty"`
	Overdue bool   `json:"overdue,omitempty"`
	Search  string `json:"search,omitempty"`
	Limit   int    `json:"limit,omitempty"`
}

type getTaskInput struct {
	ID string `json:"id"`
}

type updateTaskInput struct {
	ID string `json:"id"`
	// Every field below is optional: nil means untouched. Name is a plain string
	// because it can never be cleared, so "" is how "untouched" is spelled.
	Name             string    `json:"name,omitempty"`
	SpeakName        *string   `json:"speak_name,omitempty"`
	Description      *string   `json:"description,omitempty"`
	DueDate          *string   `json:"due_date,omitempty"`
	DueTime          *string   `json:"due_time,omitempty"`
	EstimatedMinutes *int      `json:"estimated_minutes,omitempty"`
	RepeatEvery      *int      `json:"repeat_every,omitempty"`
	RepeatUnit       *string   `json:"repeat_unit,omitempty"`
	RepeatDays       *[]string `json:"repeat_days,omitempty"`
	RepeatCustom     *bool     `json:"repeat_custom,omitempty"`
	RepeatRule       *string   `json:"repeat_rule,omitempty"`
	ExpectedRevision *int      `json:"expected_revision,omitempty"`
	Parallelable     *bool     `json:"parallelable,omitempty"`
}

type completeTaskInput struct {
	ID string `json:"id"`
}

type skipTaskInput struct {
	ID string `json:"id"`
	// Optional free text for why this occurrence was skipped.
	Reason string `json:"reason,omitempty"`
}

type reopenTaskInput struct {
	ID string `json:"id"`
}

type deleteTaskInput struct {
	ID string `json:"id"`
}

// taskHistoryInput filters the change log. Every field is optional: the whole
// log is the default reading of the question, and a caller that wants one
// task's history sends task_id.
type taskHistoryInput struct {
	// TaskID restricts to one task (the hex id from create_task). A blank
	// id means every task; a non-blank one that is not a hex id is refused
	// rather than answered with an empty history.
	TaskID string `json:"task_id,omitempty"`
	// Op restricts to a set of ops, ONE comma-separated string (not an
	// array — an array is an argument type error, not a filter): create,
	// update, delete, complete, skip, reopen, duplicate. A filter that
	// names no op (only commas and spaces) is refused rather than read as
	// the whole log. `duplicate` appears only for duplicates made through
	// a batch.
	Op string `json:"op,omitempty"`
	// Since is an INCLUSIVE RFC3339 lower bound on when the change
	// happened — a string, not an epoch number (a number is an argument
	// type error, not a filter). Fractional seconds are accepted.
	Since string `json:"since,omitempty"`
	// Limit caps the rows returned (default 200, max 500). Zero or
	// negative gives the default, a huge one is clamped to the max. A
	// non-integer number is an argument type error, not a value that gets
	// rounded.
	Limit int `json:"limit,omitempty"`
}

// createTask is the create_task handler's body, named so it can be called directly.
//
// It is extracted for testability, not style: a handler closed over `mcp.AddTool` cannot
// be reached from a test, and the obvious alternative — calling the store directly —
// tests the layer BELOW the one that matters. An earlier version of the source-on-doc
// test did exactly that and passed against the unfixed code, because the store honours
// whatever source it is handed; the decision that had been wrong was the handler's
// choice between Create and CreateWithKey, and only a test of the handler can see it.
func createTask(ctx context.Context, store *tasks.Store, in createTaskInput) (map[string]any, error) {
	rep := tasks.Repeat{
		Every:  0,
		Unit:   strings.TrimSpace(in.RepeatUnit),
		Days:   in.RepeatDays,
		Custom: in.RepeatCustom != nil && *in.RepeatCustom,
		// Not trimmed here: Repeat.Normalize owns that, and it is the only place it needs
		// to live now that both create paths go through it.
		Text: in.RepeatRule,
	}
	if in.RepeatEvery != nil {
		rep.Every = *in.RepeatEvery
	}
	// CreateWithKey, not Create, so the DOC carries `source` and not just the audit
	// entry. Wiring the audit log for the agent was half the job: a task the agent created
	// recorded `mcp` in task_mutations and nothing on the task itself, so the field that
	// exists to answer "which caller wrote this row" was blank for exactly the caller that
	// writes most rows. The two records are the same fact and they disagreed.
	//
	// No idempotency key here: this path has no client-supplied key to replay, and an
	// empty one disables the payload-mismatch check rather than weakening it — there is
	// nothing to compare a retry against.
	doc, _, err := store.CreateWithKey(ctx, in.Name, in.Description, in.DueDate,
		in.DueTime, in.EstimatedMinutes, rep.Normalize(), in.Parallelable,
		in.SpeakName, "", mcpSource, "")
	if err != nil {
		return nil, err
	}
	store.RecordMutation(ctx, tasks.OpCreate, fmt.Sprint(doc["id"]), mcpSource, "mcp create_task")
	return doc, nil
}

// mcpSource labels every mutation made through this server in the audit log.
//
// It has to be a distinct value, not a copy of the HTTP one: the whole reason the
// mutation log exists (#176) is that the agent and the app are indistinguishable at the
// auth layer — one shared password, one collection — and `source` is the only record of
// which caller wrote a row. Logging agent writes under "http" would make the field
// actively misleading, which is worse than not having it.
const mcpSource = "mcp"

// taskHistory reads the change log (#350).
//
// It is the same reader the HTTP endpoint uses, so the agent and the app see
// one history rather than two — and the agent's own writes are the ones it
// most often needs to explain ("what did you do with that task?").
//
// Every entry carries op, task_id, source and at. `source` is a LABEL, not an
// identity: "mcp" is this server, the app labels its own calls, and either
// could claim either. It answers "which client made this change" well enough
// to explain an accident, and badly enough to be evidence against anyone.
func taskHistory(ctx context.Context, store *tasks.Store, in taskHistoryInput) (map[string]any, error) {
	ops, err := tasks.ParseOps(in.Op)
	if err != nil {
		return nil, err
	}
	since, err := tasks.ParseSince(in.Since)
	if err != nil {
		return nil, err
	}
	// The same validation the HTTP route applies: a typo'd id would match
	// nothing, and an empty history reads like "this task was never
	// touched" — the wrong answer, and the same wrong answer the app would
	// get. One helper, so the two transports cannot disagree.
	taskID, err := tasks.NormalizeTaskID(in.TaskID)
	if err != nil {
		return nil, err
	}
	rows, truncated, err := store.ListMutations(ctx, tasks.MutationFilter{
		TaskID: taskID,
		Ops:    ops,
		Since:  since,
		Limit:  in.Limit,
	})
	if err != nil {
		return nil, err
	}
	if rows == nil {
		// Belt and braces: the store returns an empty list, never a nil
		// slice, so this never fires today. It is here so the tool's
		// `mutations` is `[]` and not `null` whatever the seam does next.
		rows = []tasks.Mutation{}
	}
	return map[string]any{
		"ok":        true,
		"count":     len(rows),
		"truncated": truncated,
		"mutations": rows,
	}, nil
}

func main() {
	var err error
	store, err = tasks.FromEnv()
	if err != nil {
		log.Fatalf("task-manager: %v", err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "task-manager", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "create_task",
		Description: "Create an open task. ALL fields except the repeat are required: name, description, due_date YYYY-MM-DD, due_time HH:MM, estimated_minutes >= 0, parallelable (true = can run alongside other tasks). Optional: speak_name, a verb phrase for voice reminders (e.g. name 'Power Bill' → speak_name 'pay the power bill'); blank falls back to name. The repeat is ONE of three mutually exclusive shapes — never combine them: structured interval (repeat_every 1-28 with repeat_unit days|weeks|months|years), structured selected days (repeat_days = an array of weekdays from mon,tue,wed,thu,fri,sat,sun, always WEEKLY, and due_date MUST be one of them), or a custom condition (repeat_custom true with repeat_rule = the user's words verbatim). All of them empty/0/[] = one-shot. Prefer the structured shapes: the server rolls those over itself; only a custom condition is yours to advance."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in createTaskInput) (*mcp.CallToolResult, map[string]any, error) {
			doc, err := createTask(ctx, store, in)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "task": doc})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_tasks",
		Description: "List tasks. state open (default) | done | all; overdue=true keeps open tasks due before today (requires state=open); search matches name/description; limit caps rows (default 200, max 500) and truncated says whether more exist."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in listTasksInput) (*mcp.CallToolResult, map[string]any, error) {
			rows, truncated, err := store.List(ctx, in.State, in.Overdue, in.Search, in.Limit)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "truncated": truncated, "tasks": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_task",
		Description: "Fetch one task by id."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in getTaskInput) (*mcp.CallToolResult, map[string]any, error) {
			doc, err := store.Get(ctx, in.ID)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "task": doc})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "update_task",
		Description: "Edit name/speak_name/description/due_date/due_time/estimated_minutes/repeat/parallelable. Omit a key to leave it alone; send speak_name \"\" to clear it back to the name fallback. Supplied values must satisfy create_task's mandatory rules (empty description/due fields rejected). The repeat accepts the same five keys (repeat_every/repeat_unit/repeat_days/repeat_custom/repeat_rule) and is validated as a whole; OMIT ALL FIVE repeat keys to leave the recurrence alone; sending repeat_rule \"\" with no structured key clears the whole rule (one-shot). Switching shapes means sending the new one and zeroing the old (e.g. moving to repeat_days: send repeat_every 0, repeat_unit \"\"). Repeat_days is an array of weekdays (mon..sun), always weekly, and due_date must be one of them. expected_revision: pass the revision the task showed when read; a mismatch means someone else wrote first and the edit is rejected, so re-read and retry. Works on open or done tasks."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in updateTaskInput) (*mcp.CallToolResult, map[string]any, error) {
			fields := map[string]any{}
			// Plain strings can't tell "" from absent, so only name (which
			// can never be cleared) is non-pointer; nil pointer = untouched,
			// non-nil (even "") = set/clear.
			if in.Name != "" {
				fields["name"] = in.Name
			}
			if in.SpeakName != nil {
				fields["speak_name"] = *in.SpeakName
			}
			if in.Description != nil {
				fields["description"] = *in.Description
			}
			if in.DueDate != nil {
				fields["due_date"] = *in.DueDate
			}
			if in.DueTime != nil {
				fields["due_time"] = *in.DueTime
			}
			if in.EstimatedMinutes != nil {
				fields["estimated_minutes"] = *in.EstimatedMinutes
			}
			if in.RepeatEvery != nil {
				fields["repeat_every"] = *in.RepeatEvery
			}
			if in.RepeatUnit != nil {
				fields["repeat_unit"] = strings.TrimSpace(*in.RepeatUnit)
			}
			if in.RepeatDays != nil {
				fields["repeat_days"] = *in.RepeatDays
			}
			if in.RepeatCustom != nil {
				fields["repeat_custom"] = *in.RepeatCustom
			}
			if in.RepeatRule != nil {
				fields["repeat_rule"] = strings.TrimSpace(*in.RepeatRule)
			}
			if in.ExpectedRevision != nil {
				fields["expected_revision"] = *in.ExpectedRevision
			}
			if in.Parallelable != nil {
				fields["parallelable"] = *in.Parallelable
			}
			doc, err := store.Update(ctx, in.ID, fields)
			if err != nil {
				return fail(err)
			}
			// Only when something was actually written — see the same guard in
			// health-api's updateTask: a PATCH carrying no field the store will set takes
			// the no-op path, and an audit entry for it would claim a change that did not
			// happen.
			if names := tasks.ChangedFieldNames(fields); names != "no fields" {
				store.RecordMutation(ctx, tasks.OpUpdate, in.ID, mcpSource, names)
			}
			return result(map[string]any{"ok": true, "task": doc})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "complete_task",
		Description: "Mark a task done (retained 3 days, then auto-deleted). Call ONLY when the work actually happened — never to clear something not done; if it did not happen, call skip_task instead. A STRUCTURED repeat (repeat_every + repeat_unit, or repeat_days) rolls itself over: the next occurrence is created for you and returned as `next` — do not create it yourself. A CUSTOM repeat is yours: the response carries `follow_up` and you MUST create the next occurrence via create_task with the same repeat keys, keeping every field identical (including due_time) and advancing only due_date."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in completeTaskInput) (*mcp.CallToolResult, map[string]any, error) {
			doc, next, rollover, rolloverDetail, err := store.CompleteDetail(ctx, in.ID)
			if err != nil {
				return fail(err)
			}
			store.RecordMutation(ctx, tasks.OpComplete, in.ID, mcpSource, fmt.Sprint(rollover))
			out := map[string]any{"ok": true, "task": doc}
			// Always name the rollover outcome, including the ones where there was
			// nothing to do (#180). `rolled_over` alone cannot distinguish a one-shot
			// from a repeat that silently stopped — and the silent stop is the one
			// that cost four tasks their schedule before anyone noticed.
			out["rollover"] = string(rollover)
			rep := repeatOf(doc)
			switch {
			case next != nil:
				// Rolled over server-side: the agent has nothing to do, and
				// must not create a second occurrence.
				out["rolled_over"] = true
				out["next"] = next
			case rollover == tasks.RolloverCustom:
				// Custom: only the caller can compute the next date.
				out["repeat_every"] = rep.Every
				out["repeat_unit"] = rep.Unit
				out["repeat_days"] = rep.Days
				out["repeat_custom"] = rep.Custom
				out["repeat_rule"] = rep.Text
				out["follow_up"] = "this task repeats (" + rep.String() + "), a CUSTOM condition the server will not interpret — create the next occurrence via create_task with the SAME repeat keys (" + repeatHint(rep) + "), keeping name/description/due_time/estimated_minutes/parallelable identical; only due_date advances, to the occurrence you compute; change due_time only if the rule itself names a different time, otherwise a drifting time is a bug; all create_task fields except the repeat are required."
			case rollover.NeedsAttention():
				// Say it in the response instead of leaving it in a log line: the
				// repeat has stopped, and only the user can restart it.
				out["needs_attention"] = rollover.Attention(rolloverDetail)
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "skip_task",
		Description: "Skip the current occurrence WITHOUT claiming the work was done (retained 3 days like a completion, then auto-deleted; `reopen_task` restores it). Call when the work did NOT happen: a STRUCTURED repeat (repeat_every + repeat_unit, or repeat_days) rolls itself over exactly like complete_task and the next occurrence is returned as `next` — do not create it yourself. A CUSTOM repeat is yours: the response carries `follow_up` and you MUST create the next occurrence via create_task the same way as after a custom completion."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in skipTaskInput) (*mcp.CallToolResult, map[string]any, error) {
			doc, next, rollover, rolloverDetail, err := store.SkipDetail(ctx, in.ID, in.Reason)
			if err != nil {
				return fail(err)
			}
			store.RecordMutation(ctx, tasks.OpSkip, in.ID, mcpSource, fmt.Sprint(rollover))
			out := map[string]any{"ok": true, "task": doc}
			// Same rollover reporting as complete_task (#180): `rolled_over`
			// alone cannot distinguish a one-shot from a repeat that
			// silently stopped.
			out["rollover"] = string(rollover)
			rep := repeatOf(doc)
			switch {
			case next != nil:
				// Rolled over server-side: the agent has nothing to do, and
				// must not create a second occurrence.
				out["rolled_over"] = true
				out["next"] = next
			case rollover == tasks.RolloverCustom:
				// Custom: only the caller can compute the next date.
				out["repeat_every"] = rep.Every
				out["repeat_unit"] = rep.Unit
				out["repeat_days"] = rep.Days
				out["repeat_custom"] = rep.Custom
				out["repeat_rule"] = rep.Text
				out["follow_up"] = "this task repeats (" + rep.String() + "), a CUSTOM condition the server will not interpret — create the next occurrence via create_task with the SAME repeat keys (" + repeatHint(rep) + "), keeping name/description/due_time/estimated_minutes/parallelable identical; only due_date advances, to the occurrence you compute; change due_time only if the rule itself names a different time, otherwise a drifting time is a bug; all create_task fields except the repeat are required."
			case rollover.NeedsAttention():
				// Say it in the response instead of leaving it in a log line: the
				// repeat has stopped, and only the user can restart it.
				out["needs_attention"] = rollover.Attention(rolloverDetail)
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "reopen_task",
		Description: "Reopen a completed or skipped task (clears completion and the skip marker, cancels the 3-day expiry)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in reopenTaskInput) (*mcp.CallToolResult, map[string]any, error) {
			doc, err := store.Reopen(ctx, in.ID)
			if err != nil {
				return fail(err)
			}
			store.RecordMutation(ctx, tasks.OpReopen, in.ID, mcpSource, "mcp reopen_task")
			return result(map[string]any{"ok": true, "task": doc})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_task",
		Description: "Permanently delete a task (open or done)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in deleteTaskInput) (*mcp.CallToolResult, map[string]any, error) {
			done, err := store.Delete(ctx, in.ID)
			if err != nil {
				return fail(err)
			}
			if done {
				// After the delete, so the log cannot claim one that then failed.
				store.RecordMutation(ctx, tasks.OpDelete, in.ID, mcpSource, "mcp delete_task")
			}
			if !done {
				// Bounded, like every other echo: the id came from the
				// caller and is not worth answering at length.
				return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown task '%s'", tasks.TruncateBytes(in.ID, 40))})
			}
			// The CANONICAL id, not the caller's spelling: the change log
			// stores the normalized id, so a caller comparing this answer
			// with a history row must not miss on case.
			deleted := in.ID
			if id, nerr := tasks.NormalizeTaskID(in.ID); nerr == nil {
				deleted = id
			}
			return result(map[string]any{"ok": true, "deleted": deleted})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "task_history",
		Description: "Read the change log: every create, update, complete, skip, reopen and delete, with the op, the task id, who made it (source) and when (at). The log is kept 90 days, so it still answers for a task that was completed and auto-deleted after 3 days — use it when the user asks what happened to a task, or when a task changed without the agent doing it. Filters: task_id (one task — the 24-character hex id create_task returned), op (a single comma-separated string, not an array: create, update, delete, complete, skip, reopen, duplicate — duplicate appears only for duplicates made through a batch), since (an INCLUSIVE RFC3339 timestamp string, fractional seconds accepted, e.g. 2026-10-21T00:00:00Z), limit (default 200, max 500; zero or negative gives the default, a huge one is clamped, a non-integer is an argument type error). Newest first. A malformed task_id, op or since is refused rather than answered with an empty history. `source` is a self-attested LABEL ('mcp' for this server, the app's own label otherwise), not an authenticated identity."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in taskHistoryInput) (*mcp.CallToolResult, map[string]any, error) {
			out, err := taskHistory(ctx, store, in)
			if err != nil {
				return fail(err)
			}
			return result(out)
		})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "task-manager:", err)
		os.Exit(1)
	}
}
