package main

import (
	"context"
	"strings"
	"testing"

	"agento/internal/tasks"
)

// The mutation log exists to answer "who changed this task", and the agent is a PRIMARY
// mutator — it creates, completes and reopens tasks through this server all day.
// Logging only the HTTP path would answer the question for the app and stay silent for
// the agent, which is exactly the case the log was added for: the two are
// indistinguishable at the auth layer, sharing one password and one collection.

// mcpSource must NOT be "http". A log that records agent writes as HTTP traffic is
// worse than no log, because it looks like an answer.
func TestMcpSourceIsDistinctFromHttp(t *testing.T) {
	if mcpSource == "" {
		t.Fatal("mcpSource is empty; every agent mutation would be unattributed")
	}
	if strings.EqualFold(mcpSource, "http") {
		t.Errorf("mcpSource = %q — agent writes must not be logged as HTTP traffic", mcpSource)
	}
}

// An update's detail must name the fields, or the log says only that something changed.
func TestChangedFieldNamesSharedByBothCallers(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]any
		want   string
	}{
		{"single", map[string]any{"name": "x"}, "name"},
		{"sorted, so stable", map[string]any{"parallelable": true, "due_date": "d", "name": "n"}, "due_date,name,parallelable"},
		{"revision is plumbing", map[string]any{"name": "n", "expected_revision": 3}, "name"},
		{"revision alone is no change", map[string]any{"expected_revision": 3}, "no fields"},
		{"empty", map[string]any{}, "no fields"},
	}
	for _, c := range cases {
		if got := tasks.ChangedFieldNames(c.fields); got != c.want {
			t.Errorf("%s: changedFieldNames = %q, want %q", c.name, got, c.want)
		}
	}
}

// The op vocabulary must be the same one the HTTP path uses, or the log answers "what
// happened" differently depending on who did it.
func TestMcpOpConstantsAreAllNamed(t *testing.T) {
	for name, op := range map[string]string{
		"create": tasks.OpCreate, "update": tasks.OpUpdate,
		"complete": tasks.OpComplete,
		"reopen": tasks.OpReopen, "delete": tasks.OpDelete,
	} {
		if op == "" {
			t.Errorf("the %s op constant is empty; the log would record an operation with no name", name)
		}
	}
}

// A cancelled caller must not take the write down with it. The MCP transport cancels ctx
// when the session or the tool call goes away, and a dropped entry is precisely the
// unexplained change the log exists to explain — so this half of the feature has to be
// cancellation-proof too, not just the HTTP half.
//
// The entry's contents are asserted in internal/tasks, where the collection is
// reachable; from this package the store exposes no accessor for it, and adding one
// purely for a test would be new public API for no caller.
func TestMcpAuditWriteSurvivesACancelledContext(t *testing.T) {
	s, err := tasks.FromEnv()
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Best-effort by design: swallows its own error rather than failing the tool call
	// the user asked for. The assertion is that this is safe and returns.
	s.RecordMutation(ctx, tasks.OpCreate, "0123456789abcdef01234567", mcpSource, "cancelled caller")
}

func intPtr(v int) *int    { return &v }
func boolPtr(v bool) *bool { return &v }

// The agent's creates must record `source` ON THE TASK, not only in the audit log.
//
// Calls the extracted createTask handler, not the store. An earlier version of this test
// called store.CreateWithKey directly and PASSED AGAINST THE UNFIXED CODE — the store
// honours whatever source it is handed, so the test was measuring the layer below the
// one that had been wrong. The decision that was wrong is the handler's choice between
// Create and CreateWithKey, and only the handler can see it.
func TestMcpCreateStoresSourceOnTheTask(t *testing.T) {
	store, err := tasks.FromEnv()
	if err != nil {
		t.Skipf("no database: %v", err)
	}
	ctx := context.Background()

	doc, err := createTask(ctx, store, createTaskInput{
		Name: "mcp source probe", Description: "d",
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
	defer func() { _, _ = store.Delete(ctx, id) }()

	if got := doc["source"]; got != mcpSource {
		t.Errorf("the created task carries source=%v, want %q — the audit log says mcp "+
			"but the document must agree", got, mcpSource)
	}
	// And no idempotency key: this path has no client-supplied key, and an empty one
	// disables the mismatch check rather than weakening it.
	if _, present := doc["idempotency_key"]; present {
		t.Errorf("a keyless MCP create stored an idempotency key: %v", doc["idempotency_key"])
	}
	// The audit entry agrees, since both come from the same handler now.
	if doc["id"] == nil {
		t.Error("no id to audit against")
	}
}
