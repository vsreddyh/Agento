package main

// Batch endpoint: one atomic bulk run for the Task Manager selection (#345).
//
// POST /api/tasks/batch {"ops": [{"op": "complete"|"skip"|"update"|"duplicate",
// "id": "...", "reason": "...", "fields": {...}}]} answers 200 with
// {"results": [{"index": 0, "id": "...", "op": "complete", "task": {...},
// "next": {...}, "rollover": "created", "needs_attention": "..."}]}.
// Op names are lowercase-exact (`Complete` 422s in the store); reason rides
// only on skip and fields only on update (verified above, before any write).
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
	"sort"
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
		// No silent extras: a misspelled key (`reasons`, `dueDate` at the
		// wrong level) would otherwise commit without what the caller
		// meant — the same silent-drop shape the misplaced reason/fields
		// rejection guards one level down. Sorted before reporting, so two
		// unknown keys name a deterministic one instead of a random map
		// iteration.
		var unknown []string
		for key := range obj {
			switch key {
			case "op", "id", "reason", "fields":
			default:
				unknown = append(unknown, key)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			return nil, &tasks.StoreError{Msg: fmt.Sprintf("ops[%d].%s is not a batch field; want op, id, reason, or fields", i, unknown[0])}
		}
		// Missing and mistyped stay distinct: absent means "send it",
		// wrong-typed means "fix its type", and blank-after-trim reads as
		// missing — there is no value to type-check.
		opRaw, opPresent := obj["op"]
		opStr, opIsStr := opRaw.(string)
		if !opPresent || opRaw == nil {
			return nil, &tasks.StoreError{Msg: fmt.Sprintf("ops[%d].op is required", i)}
		}
		if !opIsStr {
			return nil, &tasks.StoreError{Msg: fmt.Sprintf("ops[%d].op must be a string", i)}
		}
		op := strings.TrimSpace(opStr)
		if op == "" {
			return nil, &tasks.StoreError{Msg: fmt.Sprintf("ops[%d].op is required", i)}
		}
		idRaw, idPresent := obj["id"]
		idStr, idIsStr := idRaw.(string)
		if !idPresent || idRaw == nil {
			return nil, &tasks.StoreError{Msg: fmt.Sprintf("ops[%d].id is required", i)}
		}
		if !idIsStr {
			return nil, &tasks.StoreError{Msg: fmt.Sprintf("ops[%d].id must be a string", i)}
		}
		id := strings.TrimSpace(idStr)
		if id == "" {
			return nil, &tasks.StoreError{Msg: fmt.Sprintf("ops[%d].id is required", i)}
		}
		bop := tasks.BatchOp{Op: op, ID: id}
		if v, present := obj["reason"]; present && v != nil {
			s, ok := v.(string)
			if !ok {
				return nil, &tasks.StoreError{Msg: fmt.Sprintf("ops[%d].reason must be a string", i)}
			}
			// Trimmed like op/id: a whitespace reason is no reason, and
			// the store would carry the padding onto the skip record.
			bop.Reason = strings.TrimSpace(s)
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
	// The audit writes are DETACHED from the request (each carries its own
	// 5s budget), so the 30s request timeout does not bound this loop: on
	// a slow Mongo the acknowledgement waits for up to N×5s of durability
	// work — 50 ops is ~250s.
	//
	// That is the trade this feature makes on purpose: the log is
	// best-effort but durable-by-default, and a change the log does not
	// record is a change nobody can explain later. The fix, if batch
	// sizes ever grow, is one bulk insert for the batch's rows.
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
			// The row is about the COPY (its id is the copy's), and the
			// detail names the SOURCE it was duplicated from — carried in
			// the result by the store, so the transport does not have to
			// match the result back to its op to know what it was about.
			store.RecordMutation(ctx, tasks.OpDuplicate, res.ID, source, res.Detail)
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
		// Bounded and trimmed, like every other caller-controlled echo: a
		// batch id comes from a JSON body, so it is naturally longer than
		// a path segment, and it is not worth answering at length. The
		// trim is done HERE, not inherited from the parser — the echo
		// site owns the spelling, the same as every other echo.
		body["id"] = tasks.TruncateBytes(strings.TrimSpace(bop.ID), 40)
		body["op"] = tasks.TruncateBytes(strings.TrimSpace(bop.Op), 40)
		writeJSON(w, code, body)
		return
	}
	writeTaskErr(w, err)
}
