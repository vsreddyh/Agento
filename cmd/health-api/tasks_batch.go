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
	"log"
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
	// The domain's one request budget (tasks.RequestBudget), the same one
	// the read routes and the MCP change log use — one number for the task
	// domain, so a timeout bump cannot leave one route behind.
	ctx, cancel := context.WithTimeout(r.Context(), tasks.RequestBudget)
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
	// The wait is the SERVER's, and it outlives the client: the request
	// context expires at 30s, so a caller that has already been told
	// "timed out" is still waiting on rows being written here. That
	// asymmetry is what the detached write buys — a client that gave up
	// must not take the log with it — and it is why the response can be
	// lost while the rows are not.
	//
	// That is the trade this feature makes on purpose: the log is
	// best-effort but durable-by-default, and a change the log does not
	// record is a change nobody can explain later. The fix, if batch
	// sizes ever grow, is one bulk insert for the batch's rows.
	//
	// And the consequence for a caller: a timed-out batch has an UNKNOWN
	// outcome, because the commit happened before the timeout did. The
	// answer is never a blind resend — `duplicate` is the one verb here
	// that is not idempotent, so a retry copies the task a second time.
	// Read the change log (or the changes marker) first and resend only
	// what did not land.
	//
	// The loop is timed, and it says so when it OUTLIVES the request: a
	// timeout-vs-commit investigation starts at the client's retry, and
	// this is the server-side anchor for what the server was doing all
	// that time. A loop that finished inside the request says nothing —
	// that is the ordinary case and it needs no line in the log.
	//
	// ATTEMPTED, not "wrote": RecordMutation is best-effort by contract
	// and returns nothing, so a dropped insert still counts here. The
	// line is an anchor for the timeline, not a claim about the log.
	started := time.Now()
	recordBatchMutations(ctx, store, results, source)
	if ctx.Err() != nil {
		log.Printf("tasks: batch audit ATTEMPTED %d row(s) (%s during audit writes; the request deadline was exceeded)",
			len(results), time.Since(started).Round(time.Millisecond))
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

// recordBatchMutations writes one audit row per op the batch actually
// applied.
//
// It is a function of its own so the rules are testable without a
// request: which ops earn a row, and what each row must be able to
// name.
func recordBatchMutations(ctx context.Context, store *tasks.Store, results []tasks.BatchItemResult, source string) {
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
			//
			// BOTH ids must be known: a row that names the source as
			// the copy is a duplicate that never happened, and it reads
			// exactly like one that did. The store refuses to produce
			// such a result (batch.go), so this is the second line — the
			// row is written only when it can name the copy AND the
			// source.
			//
			// And the case is OURS to log, not the seam's: the seam logs
			// the row it KEEPS with a missing back-link, so the transport
			// that DROPS one has to say so too — otherwise the loud
			// signal the seam was given to emit is silenced by the door
			// in front of it.
			if res.Detail == "" {
				log.Printf("tasks: batch duplicate row skipped (the result names no source) task_id=%q",
					tasks.TruncateBytes(strings.TrimSpace(res.ID), 40))
				continue
			}
			store.RecordMutation(ctx, tasks.OpDuplicate, res.ID, source, res.Detail)
		}
	}
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
		//
		// An ENVELOPE-level rejection (index -1: an empty batch, one over
		// the cap) names no op and no id, so the keys are omitted rather
		// than sent empty: a key that promises a culprit the error does
		// not have is worse than no key.
		if bop.Index >= 0 {
			body["id"] = tasks.TruncateBytes(strings.TrimSpace(bop.ID), 40)
			body["op"] = tasks.TruncateBytes(strings.TrimSpace(bop.Op), 40)
		}
		writeJSON(w, code, body)
		return
	}
	writeTaskErr(w, err)
}
