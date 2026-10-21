package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"agento/internal/tasks"
)

// The agent is a primary mutator, so the history it asks for must include
// the agent's OWN writes — otherwise the tool answers "nothing happened"
// about a change the agent made minutes ago.
//
// It reads through the same store the HTTP endpoint uses, so the agent and
// the app see one history rather than two.
func TestTaskHistoryReadsTheAgentsOwnWrites(t *testing.T) {
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		t.Skip("MONGODB_URI not set")
	}
	store, err := tasks.New(uri, "task_manager_history_test")
	if err != nil {
		t.Skipf("mongo unreachable: %v", err)
	}
	defer store.Close(context.Background())
	ctx := context.Background()

	doc, err := createTask(ctx, store, createTaskInput{
		Name: "history probe", Description: "d",
		DueDate: "2026-10-05", DueTime: "08:00",
		EstimatedMinutes: intPtr(5), Parallelable: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("createTask: %v", err)
	}
	id, _ := doc["id"].(string)
	if id == "" {
		t.Fatalf("no id returned: %v", doc)
	}
	// The cleanup deletes the task WITHOUT writing a log row: only the
	// transports record mutations, not the store's Delete. That is what
	// keeps the counts above exact — if Delete ever started recording, a
	// row would appear here and break them.
	defer func() { _, _ = store.Delete(ctx, id) }()

	out, err := taskHistory(ctx, store, taskHistoryInput{TaskID: id})
	if err != nil {
		t.Fatalf("taskHistory: %v", err)
	}
	if out["ok"] != true {
		t.Fatalf("ok = %v, want true", out["ok"])
	}
	rows, _ := out["mutations"].([]tasks.Mutation)
	if len(rows) != 1 {
		t.Fatalf("mutations = %d, want 1 (the create)", len(rows))
	}
	row := rows[0]
	if row.Op != tasks.OpCreate {
		t.Errorf("op = %q, want %q", row.Op, tasks.OpCreate)
	}
	if row.TaskID != id {
		t.Errorf("task_id = %q, want %q", row.TaskID, id)
	}
	// The WHO part of the question: the agent's writes must be attributed to
	// the agent, not to "http".
	if row.Source != mcpSource {
		t.Errorf("source = %q, want %q", row.Source, mcpSource)
	}
	// The WHEN part: a timestamp the caller can sort on.
	if row.At.IsZero() {
		t.Error("at must be set")
	}
}

// The filters are the tool's whole value beyond "dump the log": one task's
// history, or one kind of change.
func TestTaskHistoryFilters(t *testing.T) {
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		t.Skip("MONGODB_URI not set")
	}
	store, err := tasks.New(uri, "task_manager_history_test")
	if err != nil {
		t.Skipf("mongo unreachable: %v", err)
	}
	defer store.Close(context.Background())
	ctx := context.Background()

	doc, err := createTask(ctx, store, createTaskInput{
		Name: "history filter probe", Description: "d",
		DueDate: "2026-10-06", DueTime: "08:00",
		EstimatedMinutes: intPtr(5), Parallelable: boolPtr(false),
	})
	if err != nil {
		t.Fatalf("createTask: %v", err)
	}
	id, _ := doc["id"].(string)
	// The cleanup deletes the task WITHOUT writing a log row: only the
	// transports record mutations, not the store's Delete. That is what
	// keeps the counts above exact — if Delete ever started recording, a
	// row would appear here and break them.
	defer func() { _, _ = store.Delete(ctx, id) }()

	// An op the task never had: the filter must exclude it rather than
	// return the task's other entries.
	out, err := taskHistory(ctx, store, taskHistoryInput{TaskID: id, Op: "delete"})
	if err != nil {
		t.Fatalf("taskHistory: %v", err)
	}
	rows, _ := out["mutations"].([]tasks.Mutation)
	if len(rows) != 0 {
		t.Fatalf("mutations = %d, want 0", len(rows))
	}

	// The op the task does have.
	out, err = taskHistory(ctx, store, taskHistoryInput{TaskID: id, Op: "create"})
	if err != nil {
		t.Fatalf("taskHistory: %v", err)
	}
	rows, _ = out["mutations"].([]tasks.Mutation)
	if len(rows) != 1 {
		t.Fatalf("mutations = %d, want 1", len(rows))
	}
}

// An op the log does not hold is a caller mistake, and the tool says so
// rather than returning an empty history that reads like "nothing happened".
func TestTaskHistoryRejectsUnknownOp(t *testing.T) {
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		t.Skip("MONGODB_URI not set")
	}
	store, err := tasks.New(uri, "task_manager_history_test")
	if err != nil {
		t.Skipf("mongo unreachable: %v", err)
	}
	defer store.Close(context.Background())
	_, err = taskHistory(context.Background(), store, taskHistoryInput{Op: "frobnicate"})
	if err == nil {
		t.Fatal("unknown op must be rejected")
	}
}

// The agent and the app read the same log through different doors, so a
// typo'd task id must be refused by BOTH. The HTTP route already refuses it;
// a tool that answered with an empty history would be the wrong answer the
// route exists to avoid.
func TestTaskHistoryRejectsMalformedTaskID(t *testing.T) {
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		t.Skip("MONGODB_URI not set")
	}
	store, err := tasks.New(uri, "task_manager_history_test")
	if err != nil {
		t.Skipf("mongo unreachable: %v", err)
	}
	defer store.Close(context.Background())
	_, err = taskHistory(context.Background(), store, taskHistoryInput{TaskID: "not-an-id"})
	if err == nil {
		t.Fatal("a malformed task id must be rejected")
	}
	if !strings.Contains(err.Error(), "task_id") {
		t.Errorf("the error must name the field: %v", err)
	}
}
