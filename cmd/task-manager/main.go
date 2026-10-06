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
	return fmt.Sprintf("repeat_every: %d, repeat_unit: %q", rep.Every, rep.Unit)
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
	Description      string `json:"description"`
	DueDate          string `json:"due_date"`
	DueTime          string `json:"due_time"`
	EstimatedMinutes *int   `json:"estimated_minutes"`
	// The repeat is optional as a block: no keys means one-shot. Each key is
	// individually optional too, since Normalize treats a missing key as zero.
	RepeatEvery  *int   `json:"repeat_every,omitempty"`
	RepeatUnit   string `json:"repeat_unit,omitempty"`
	RepeatCustom *bool  `json:"repeat_custom,omitempty"`
	RepeatRule   string `json:"repeat_rule,omitempty"`
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
	Name             string  `json:"name,omitempty"`
	Description      *string `json:"description,omitempty"`
	DueDate          *string `json:"due_date,omitempty"`
	DueTime          *string `json:"due_time,omitempty"`
	EstimatedMinutes *int    `json:"estimated_minutes,omitempty"`
	RepeatEvery      *int    `json:"repeat_every,omitempty"`
	RepeatUnit       *string `json:"repeat_unit,omitempty"`
	RepeatCustom     *bool   `json:"repeat_custom,omitempty"`
	RepeatRule       *string `json:"repeat_rule,omitempty"`
	ExpectedRevision *int    `json:"expected_revision,omitempty"`
	Parallelable     *bool   `json:"parallelable,omitempty"`
}

type completeTaskInput struct {
	ID string `json:"id"`
}

type reopenTaskInput struct {
	ID string `json:"id"`
}

type deleteTaskInput struct {
	ID string `json:"id"`
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
		"", mcpSource, "")
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

func main() {
	var err error
	store, err = tasks.FromEnv()
	if err != nil {
		log.Fatalf("task-manager: %v", err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "task-manager", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "create_task",
		Description: "Create an open task. ALL fields except the repeat are required: name, description, due_date YYYY-MM-DD, due_time HH:MM, estimated_minutes >= 0, parallelable (true = can run alongside other tasks). The repeat is EITHER structured (repeat_every 1-28 with repeat_unit days|weeks|months|years) OR a custom condition (repeat_custom true with repeat_rule = the user's words verbatim) — never both, and all of them empty/0 = one-shot (never interpreted server-side)."},
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
		Description: "Edit name/description/due_date/due_time/estimated_minutes/repeat/parallelable. Supplied values must satisfy create_task's mandatory rules (empty description/due fields rejected). The repeat accepts the same four keys (repeat_every/repeat_unit/repeat_custom/repeat_rule) and is validated as a whole; OMIT ALL FOUR repeat keys to leave the recurrence alone; sending repeat_rule \"\" with no structured key clears the whole rule (one-shot). expected_revision: pass the revision the task showed when read; a mismatch means someone else wrote first and the edit is rejected, so re-read and retry. Works on open or done tasks."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in updateTaskInput) (*mcp.CallToolResult, map[string]any, error) {
			fields := map[string]any{}
			// Plain strings can't tell "" from absent, so only name (which
			// can never be cleared) is non-pointer; nil pointer = untouched,
			// non-nil (even "") = set/clear.
			if in.Name != "" {
				fields["name"] = in.Name
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
		Description: "Mark a task done (retained 3 days, then auto-deleted). Call ONLY when the work actually happened — never to clear something not done; if it did not happen, leave it open or push due_date/due_time via update_task. A STRUCTURED repeat (repeat_every + repeat_unit) rolls itself over: the next occurrence is created for you and returned as `next` — do not create it yourself. A CUSTOM repeat is yours: the response carries `follow_up` and you MUST create the next occurrence via create_task with the same repeat keys, keeping every field identical (including due_time) and advancing only due_date."},
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
		Description: "Reopen a completed task (clears completion, cancels the 3-day expiry)."},
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
				return result(map[string]any{"ok": false, "error": fmt.Sprintf("unknown task '%s'", in.ID)})
			}
			return result(map[string]any{"ok": true, "deleted": in.ID})
		})

	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, "task-manager:", err)
		os.Exit(1)
	}
}
