// Command task-manager is the personal task-manager MCP server.
//
// Storage: MongoDB (tasks collection). No status field — a task is open
// while completedAt is null, done once set. Completed tasks expire via
// TTL 3 days after completion; a repeat is stored but never interpreted
// here (agent-side per skills/task-manager/SKILL.md).
// Runs over stdio for MCP clients.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"

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
	if n, ok := toInt(doc["repeat_every"]); ok {
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

// toInt mirrors the store's number handling for response maps.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
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
	return nil, map[string]any{"ok": false, "error": err.Error()}, nil
}

func result(out map[string]any) (*mcp.CallToolResult, map[string]any, error) {
	b, _ := json.Marshal(out)
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(b)}},
		StructuredContent: out,
	}, out, nil
}

func main() {
	var err error
	store, err = tasks.FromEnv()
	if err != nil {
		log.Fatalf("task-manager: %v", err)
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "task-manager", Version: "1.0.0"}, nil)

	mcp.AddTool(s, &mcp.Tool{Name: "create_task",
		Description: "Create an open task. ALL fields except the repeat are required: name, description, due_date YYYY-MM-DD, due_time HH:MM, estimated_minutes >= 0, parallelable (true = can run alongside other tasks). The repeat is EITHER structured (repeat_every 1-28 with repeat_unit days|weeks|months|years) OR a custom condition (repeat_custom true with repeat_rule = the user's words verbatim) — never both, and all of them empty/0 = one-shot (never interpreted server-side)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Name             string `json:"name"`
			Description      string `json:"description"`
			DueDate          string `json:"due_date"`
			DueTime          string `json:"due_time"`
			EstimatedMinutes *int   `json:"estimated_minutes"`
			RepeatEvery      *int   `json:"repeat_every"`
			RepeatUnit       string `json:"repeat_unit"`
			RepeatCustom     *bool  `json:"repeat_custom"`
			RepeatRule       string `json:"repeat_rule"`
			Parallelable     *bool  `json:"parallelable"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			rep := tasks.Repeat{
				Every:  0,
				Unit:   strings.TrimSpace(in.RepeatUnit),
				Custom: in.RepeatCustom != nil && *in.RepeatCustom,
				Text:   strings.TrimSpace(in.RepeatRule),
			}
			if in.RepeatEvery != nil {
				rep.Every = *in.RepeatEvery
			}
			doc, err := store.Create(ctx, in.Name, in.Description, in.DueDate, in.DueTime, in.EstimatedMinutes, rep.Normalize(), in.Parallelable)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "task": doc})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_tasks",
		Description: "List tasks. state open (default) | done | all; overdue=true keeps open tasks due before today (requires state=open); search matches name/description; limit caps rows (default 200, max 500) and truncated says whether more exist."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			State   string `json:"state"`
			Overdue bool   `json:"overdue"`
			Search  string `json:"search"`
			Limit   int    `json:"limit"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			rows, truncated, err := store.List(ctx, in.State, in.Overdue, in.Search, in.Limit)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "truncated": truncated, "tasks": rows})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "get_task",
		Description: "Fetch one task by id."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			ID string `json:"id"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			doc, err := store.Get(ctx, in.ID)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "task": doc})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "update_task",
		Description: "Edit name/description/due_date/due_time/estimated_minutes/repeat/parallelable. Supplied values must satisfy create_task's mandatory rules (empty description/due fields rejected). The repeat accepts the same four keys (repeat_every/repeat_unit/repeat_custom/repeat_rule) and is validated as a whole; sending repeat_rule \"\" with no structured key clears the whole rule (one-shot). Works on open or done tasks."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			ID               string  `json:"id"`
			Name             string  `json:"name"`
			Description      *string `json:"description"`
			DueDate          *string `json:"due_date"`
			DueTime          *string `json:"due_time"`
			EstimatedMinutes *int    `json:"estimated_minutes"`
			RepeatEvery      *int    `json:"repeat_every"`
			RepeatUnit       *string `json:"repeat_unit"`
			RepeatCustom     *bool   `json:"repeat_custom"`
			RepeatRule       *string `json:"repeat_rule"`
			Parallelable     *bool   `json:"parallelable"`
		}) (*mcp.CallToolResult, map[string]any, error) {
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
			if in.Parallelable != nil {
				fields["parallelable"] = *in.Parallelable
			}
			doc, err := store.Update(ctx, in.ID, fields)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "task": doc})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "complete_task",
		Description: "Mark a task done (retained 3 days, then auto-deleted). A STRUCTURED repeat (repeat_every + repeat_unit) rolls itself over: the next occurrence is created for you and returned as `next` — do not create it yourself. A CUSTOM repeat is yours: the response carries `follow_up` and you MUST create the next occurrence via create_task with the same repeat keys, keeping every field identical (including due_time) and advancing only due_date."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			ID string `json:"id"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			doc, next, err := store.Complete(ctx, in.ID)
			if err != nil {
				return fail(err)
			}
			out := map[string]any{"ok": true, "task": doc}
			rep := repeatOf(doc)
			switch {
			case next != nil:
				// Rolled over server-side: the agent has nothing to do, and
				// must not create a second occurrence.
				out["rolled_over"] = true
				out["next"] = next
			case !rep.IsZero():
				// Custom: only the caller can compute the next date.
				out["repeat_every"] = rep.Every
				out["repeat_unit"] = rep.Unit
				out["repeat_custom"] = rep.Custom
				out["repeat_rule"] = rep.Text
				out["follow_up"] = "this task repeats (" + rep.String() + "), a CUSTOM condition the server will not interpret — create the next occurrence via create_task with the SAME repeat keys (" + repeatHint(rep) + "), keeping name/description/due_time/estimated_minutes/parallelable identical; only due_date advances, to the occurrence you compute; change due_time only if the rule itself names a different time, otherwise a drifting time is a bug; all create_task fields except the repeat are required."
			}
			return result(out)
		})

	mcp.AddTool(s, &mcp.Tool{Name: "reopen_task",
		Description: "Reopen a completed task (clears completion, cancels the 3-day expiry)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			ID string `json:"id"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			doc, err := store.Reopen(ctx, in.ID)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "task": doc})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "delete_task",
		Description: "Permanently delete a task (open or done)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			ID string `json:"id"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			done, err := store.Delete(ctx, in.ID)
			if err != nil {
				return fail(err)
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
