package main

// GET /api/tasks/mutations — the change log (#350).
//
// The log has been written since #176 (task_mutations: op, task, source, time)
// and had no read path at all. It matters because a completed task is deleted
// three days after it is completed: the task row is gone long before the
// log's 90-day window ends, so this is the only place left that says what
// happened to it.
//
// Every entry carries the four facts the log exists for — what changed
// (`op`), which task (`task_id`), who changed it (`source`, self-attested) and
// when (`at`).

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agento/internal/tasks"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// taskMutations serves the change log.
//
// Filters: `task_id` (one task), `op` (comma-separated, e.g. "complete,skip"),
// `since` (an inclusive RFC3339 lower bound on `at`) and `limit`.
//
// The filter params are STRICT and the limit is LENIENT, and the difference
// is deliberate: a malformed `since` or `op` would silently widen the answer
// to "the whole log", which is the wrong data rather than merely more of it,
// so it is a 422 that names the mistake. A malformed `limit` can only make
// the page bigger, which is the same answer the caller already had, so it
// falls back to the default exactly like ?limit= on the task list.
func taskMutations(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	// The trailing-slash registration is a subtree pattern, so anything under
	// /api/tasks/mutations/ reaches this handler. Only the two exact paths
	// are the log; the rest 404 rather than answering for a URL that is not
	// the log.
	if r.URL.Path != "/api/tasks/mutations" && r.URL.Path != "/api/tasks/mutations/" {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "not found"})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "method not allowed"})
		return
	}
	q := r.URL.Query()
	// The log stores the task's hex id, so a malformed one can only ever
	// match nothing. Saying so beats an empty history that reads like "this
	// task was never touched".
	taskID := strings.TrimSpace(q.Get("task_id"))
	if taskID != "" {
		if _, err := primitive.ObjectIDFromHex(taskID); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity,
				bson.M{"detail": "task_id must be a 24-character hex id"})
			return
		}
	}
	ops, err := tasks.ParseOps(q.Get("op"))
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	var since time.Time
	if raw := strings.TrimSpace(q.Get("since")); raw != "" {
		t, perr := time.Parse(time.RFC3339, raw)
		if perr != nil {
			writeJSON(w, http.StatusUnprocessableEntity,
				bson.M{"detail": "since must be an RFC3339 timestamp, e.g. 2026-10-21T00:00:00Z"})
			return
		}
		since = t
	}
	limit := 0
	// Non-numeric ?limit= falls back to the store default on purpose: a
	// stray query param should never nuke the answer.
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		if n, cerr := strconv.Atoi(raw); cerr == nil {
			limit = n
		}
	}
	store, ok := taskStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rows, truncated, err := store.ListMutations(ctx, tasks.MutationFilter{
		TaskID: taskID,
		Ops:    ops,
		Since:  since,
		Limit:  limit,
	})
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	if rows == nil {
		rows = []tasks.Mutation{}
	}
	writeJSON(w, http.StatusOK, bson.M{"mutations": rows, "truncated": truncated})
}
