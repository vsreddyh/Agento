package main

// Batch endpoint: one atomic bulk run for the Task Manager selection (#345).
//
// POST /api/tasks/batch {"ops": [{"op": "complete"|"skip"|"update"|"duplicate",
// "id": "...", "reason": "...", "fields": {...}}]} answers 200 with
// {"results": [{"index": 0, "id": "...", "op": "complete", "task": {...},
// "next": {...}, "rollover": "created", "needs_attention": "..."}]}.
// "id" names the row the op produced: the task itself, except duplicate,
// whose copy is a new row. Any hard failure aborts the whole batch and
// answers with the single-verb status for the failing op (404 unknown id,
// 409 revision conflict, 422 validation, 503 no transactions) plus the
// op's index, id, and op so the client can name it.
//
// Shape validation (JSON types) lives here, beside checkTaskFields; VALUE
// validation (empty batch, over-cap, unknown op) lives in the store, the one
// place both this transport and any future caller share.

import (
	"agento/internal/tasks"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// parseBatchOps reads the ops array out of a decoded batch body. Types only:
// whether an op NAME is valid or the batch fits the cap is the store's call.
func parseBatchOps(fields map[string]any) ([]tasks.BatchOp, error) {
	raw, ok := fields["ops"]
	if !ok || raw == nil {
		return nil, &tasks.StoreError{Msg: "ops is required"}
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, &tasks.StoreError{Msg: "ops must be an array"}
	}
	ops := make([]tasks.BatchOp, 0, len(arr))
	for i, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, &tasks.StoreError{Msg: fmt.Sprintf("ops[%d] must be an object", i)}
		}
		op, _ := obj["op"].(string)
		op = strings.TrimSpace(op)
		if op == "" {
			return nil, &tasks.StoreError{Msg: fmt.Sprintf("ops[%d].op is required", i)}
		}
		id, _ := obj["id"].(string)
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, &tasks.StoreError{Msg: fmt.Sprintf("ops[%d].id is required", i)}
		}
		bop := tasks.BatchOp{Op: op, ID: id}
		if v, present := obj["reason"]; present && v != nil {
			s, ok := v.(string)
			if !ok {
				return nil, &tasks.StoreError{Msg: fmt.Sprintf("ops[%d].reason must be a string", i)}
			}
			bop.Reason = s
		}
		if v, present := obj["fields"]; present && v != nil {
			m, ok := v.(map[string]any)
			if !ok {
				return nil, &tasks.StoreError{Msg: fmt.Sprintf("ops[%d].fields must be an object", i)}
			}
			bop.Fields = m
		}
		ops = append(ops, bop)
	}
	return ops, nil
}

// batchTasks serves POST /api/tasks/batch: one atomic bulk run. Mutations
// are logged AFTER the commit, one row per op, exactly like the single verbs
// — an aborted batch changed nothing, so there is nothing to record.
func batchTasks(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "method not allowed"})
		return
	}
	fields, ok := decodeTaskBody(w, r)
	if !ok {
		return
	}
	ops, err := parseBatchOps(fields)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	store, ok := taskStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	results, err := store.Batch(ctx, ops)
	if err != nil {
		writeBatchErr(w, err)
		return
	}
	source := requestSource(r)
	for _, res := range results {
		switch res.Op {
		case "complete":
			store.RecordMutation(ctx, tasks.OpComplete, res.ID, source, string(res.Rollover))
		case "skip":
			store.RecordMutation(ctx, tasks.OpSkip, res.ID, source, string(res.Rollover))
		case "update":
			// A no-op update wrote nothing: like the single verb, it
			// earns no audit row, so the log stays a record of change.
			if res.Detail != "no fields" {
				store.RecordMutation(ctx, tasks.OpUpdate, res.ID, source, res.Detail)
			}
		case "duplicate":
			store.RecordMutation(ctx, tasks.OpDuplicate, res.ID, source, "")
		}
	}
	out := make([]bson.M, 0, len(results))
	for _, res := range results {
		item := bson.M{
			"index": res.Index, "id": res.ID, "op": res.Op,
			"task": res.Task, "rollover": string(res.Rollover),
		}
		if res.Next != nil {
			item["next"] = res.Next
		}
		if res.Rollover.NeedsAttention() {
			item["needs_attention"] = res.Rollover.Attention(res.Detail)
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, bson.M{"results": out})
}

// writeBatchErr maps a batch failure like its single-verb equivalent (404
// unknown id, 409 conflict, 422 validation, 503 no transactions), naming the
// failing op so the client can report WHICH item failed and keep the
// selection for retry.
func writeBatchErr(w http.ResponseWriter, err error) {
	// No-transaction deployments are a server capability, not a bad
	// envelope: 503, so the client treats it as retry-later rather than
	// fix-your-fields. Served before the op wrapper because this error
	// aborts before any op runs and names no index.
	if errors.Is(err, tasks.ErrNoTransactions) {
		writeJSON(w, http.StatusServiceUnavailable, bson.M{"detail": err.Error()})
		return
	}
	var bop *tasks.BatchOpError
	if errors.As(err, &bop) {
		// Owned here: taskErrBody builds a fresh map per call, so these
		// additions cannot alias anything shared.
		code, body := taskErrBody(bop.Err)
		body["index"] = bop.Index
		body["id"] = bop.ID
		body["op"] = bop.Op
		writeJSON(w, code, body)
		return
	}
	writeTaskErr(w, err)
}
