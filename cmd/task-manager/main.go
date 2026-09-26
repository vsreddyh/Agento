// Command task-manager is the personal task-manager MCP server.
//
// Storage: MongoDB (tasks collection). No status field — a task is open
// while completedAt is null, done once set. Completed tasks expire via
// TTL 3 days after completion; repeat_rule is stored verbatim and never
// interpreted here (agent-side per skills/task-manager/SKILL.md).
// Runs over stdio for MCP clients.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"agento/internal/tasks"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var store *tasks.Store

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
		Description: "Create an open task. name required; due_date YYYY-MM-DD, due_time HH:MM (needs due_date), estimated_minutes >= 0, repeat_rule free-form (empty = one-shot, never interpreted server-side)."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			Name             string `json:"name"`
			Description      string `json:"description"`
			DueDate          string `json:"due_date"`
			DueTime          string `json:"due_time"`
			EstimatedMinutes int    `json:"estimated_minutes"`
			RepeatRule       string `json:"repeat_rule"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			doc, err := store.Create(ctx, in.Name, in.Description, in.DueDate, in.DueTime, in.EstimatedMinutes, in.RepeatRule)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "task": doc})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "list_tasks",
		Description: "List tasks. state open (default) | done | all; overdue=true keeps open tasks due before today; search matches name/description."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			State   string `json:"state"`
			Overdue bool   `json:"overdue"`
			Search  string `json:"search"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			rows, err := store.List(ctx, in.State, in.Overdue, in.Search)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "count": len(rows), "tasks": rows})
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
		Description: "Edit name/description/due_date/due_time/estimated_minutes/repeat_rule (empty repeat_rule clears the rule). Works on open or done tasks."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			ID               string  `json:"id"`
			Name             string  `json:"name"`
			Description      *string `json:"description"`
			DueDate          *string `json:"due_date"`
			DueTime          *string `json:"due_time"`
			EstimatedMinutes *int    `json:"estimated_minutes"`
			RepeatRule       *string `json:"repeat_rule"`
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
			if in.RepeatRule != nil {
				fields["repeat_rule"] = *in.RepeatRule
			}
			doc, err := store.Update(ctx, in.ID, fields)
			if err != nil {
				return fail(err)
			}
			return result(map[string]any{"ok": true, "task": doc})
		})

	mcp.AddTool(s, &mcp.Tool{Name: "complete_task",
		Description: "Mark a task done (retained 3 days, then auto-deleted). If the task has a repeat_rule, you MUST create the next occurrence via create_task (same rule, next due date you compute) — the server never does this."},
		func(ctx context.Context, _ *mcp.CallToolRequest, in struct {
			ID string `json:"id"`
		}) (*mcp.CallToolResult, map[string]any, error) {
			doc, err := store.Complete(ctx, in.ID)
			if err != nil {
				return fail(err)
			}
			out := map[string]any{"ok": true, "task": doc}
			if rule, _ := doc["repeat_rule"].(string); rule != "" {
				out["repeat_rule"] = rule
				out["follow_up"] = "repeat_rule is '" + rule + "' — add task with '" + rule + "' repeat rule via create_task (same name/description/estimate, next due date you compute)."
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
