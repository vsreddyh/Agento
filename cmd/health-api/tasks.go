package main

// Task routes: the Task Manager surface over the shared tasks collection (#171).

import (
	"agento/internal/tasks"
	"context"
	"encoding/json"
	"errors"
	"go.mongodb.org/mongo-driver/bson"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	syncpkg "sync"
	"time"
)

// listTasks serves the app's Task Manager screen: the user's own tasks from
// the shared `tasks` collection (the same rows the agent manages over MCP).
// `?state=open|done|all` (default open).
func listTasks(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "method not allowed"})
		return
	}
	state := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("state")))
	if state == "" {
		state = "open"
	}
	store, ok := taskStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	limit := 0
	// Non-numeric ?limit= falls back to the store default (200) on
	// purpose: a stray query param should never nuke the task list.
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	rows, truncated, err := store.List(ctx, state, false, "", limit)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, bson.M{"tasks": rows, "truncated": truncated})
}

// taskStore opens the shared tasks collection (same rows the agent manages).
// The client is cached process-wide: connecting + ensuring indexes on every
// request would fan a fresh connection out of each list/refresh. Failures
// are not cached, so a bad env recovers without a restart.
var (
	tasksStoreMu syncpkg.Mutex
	tasksCache   *tasks.Store
)

func taskStore(w http.ResponseWriter) (*tasks.Store, bool) {
	tasksStoreMu.Lock()
	defer tasksStoreMu.Unlock()
	if tasksCache != nil {
		return tasksCache, true
	}
	store, err := tasks.FromEnv()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, bson.M{"detail": "MONGODB_URI not set"})
		return nil, false
	}
	tasksCache = store
	return store, true
}

// writeTaskErr maps store domain errors: unknown ids 404, validation 422.
// Anything else is logged with full detail but returns a generic message
// (the endpoint is auth-gated, still no reason to leak DB internals).
func writeTaskErr(w http.ResponseWriter, err error) {
	var se *tasks.StoreError
	if errors.As(err, &se) {
		if strings.HasPrefix(se.Msg, "unknown task") {
			writeJSON(w, http.StatusNotFound, bson.M{"detail": se.Msg})
			return
		}
		// A lost revision race is neither missing nor invalid: 409, with
		// the message the store wrote (it names both revisions and says
		// to reload), so the app can show it as-is.
		if se.Conflict {
			writeJSON(w, http.StatusConflict, bson.M{"detail": se.Msg})
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": se.Msg})
		return
	}
	log.Printf("tasks: internal error: %v", err)
	writeJSON(w, http.StatusInternalServerError, bson.M{"detail": "internal error"})
}

// decodeTaskBody reads a small JSON object body into a field map.
func decodeTaskBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	return decodeTaskBodyOpts(w, r, bodyOpts{})
}

// bodyOpts are the ways a task body differs between routes. They exist as options
// rather than as separate decoders because the skip route had its own inline copy of
// this function — a second place for the 64KB bound, the error shape and the
// type-check rules to drift apart, which is exactly how it ended up unable to reject
// a typo'd field without duplicating that too.
type bodyOpts struct {
	// allowEmpty accepts an absent or empty body. Only skip, where the reason is
	// optional and the action is unambiguous without it; create and update have
	// required fields, so an empty body there is a malformed request.
	allowEmpty bool
	// known, when non-nil, is the complete set of accepted keys and anything else is
	// a 422. Only skip sets it.
	known map[string]bool
}

func decodeTaskBodyOpts(w http.ResponseWriter, r *http.Request, opts bodyOpts) (map[string]any, bool) {
	var fields map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&fields); err != nil {
		if opts.allowEmpty && errors.Is(err, io.EOF) {
			return map[string]any{}, true
		}
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "invalid JSON: " + err.Error()})
		return nil, false
	}
	if fields == nil {
		fields = map[string]any{}
	}
	if opts.known != nil {
		// Checked by hand rather than with json.Decoder.DisallowUnknownFields, which
		// is a NO-OP against a map[string]any target: it only rejects unknown fields
		// when decoding into a STRUCT. Relying on it here would have looked like the
		// typo check and silently accepted every typo.
		var unknown []string
		for k := range fields {
			if !opts.known[k] {
				unknown = append(unknown, k)
			}
		}
		if len(unknown) > 0 {
			sort.Strings(unknown)
			accepted := make([]string, 0, len(opts.known))
			for k := range opts.known {
				accepted = append(accepted, k)
			}
			sort.Strings(accepted)
			writeJSON(w, http.StatusUnprocessableEntity, bson.M{
				"detail": "unknown field(s): " + strings.Join(unknown, ", ") +
					"; this endpoint accepts only " + strings.Join(accepted, ", "),
			})
			return nil, false
		}
	}
	return fields, true
}

func taskStrField(fields map[string]any, key string) string {
	if v, ok := fields[key].(string); ok {
		return v
	}
	return ""
}

// checkTaskFields rejects mistyped values loudly. The store silently
// ignores non-string fields and truncates fractional minutes, which would
// turn client bugs into mystery no-ops — the HTTP layer validates first so
// mistakes come back as 422. JSON null counts as absent (no change).
func checkTaskFields(fields map[string]any) error {
	for _, k := range []string{"name", "description", "due_date", "due_time", "repeat_rule", "repeat_unit"} {
		if v, ok := fields[k]; ok && v != nil {
			if _, ok := v.(string); !ok {
				return &tasks.StoreError{Msg: k + " must be a string"}
			}
		}
	}
	if v, ok := fields["estimated_minutes"]; ok && v != nil {
		if _, ok := taskMinutes(v); !ok {
			return &tasks.StoreError{Msg: "estimated_minutes must be an integer >= 0"}
		}
	}
	if v, ok := fields["repeat_every"]; ok && v != nil {
		// Type-shape only: whether the value is an integer at all is
		// decided here, whether it is in range is the store's call
		// (Repeat.Validate), so the bounds are stated once. Fractionals
		// are deliberately tighter here than the store (which truncates):
		// 3.5 is a caller bug, not a count, and it should fail at the
		// door with this message rather than store as 3.
		if _, ok := taskInt(v); !ok {
			return &tasks.StoreError{Msg: "repeat_every must be an integer"}
		}
	}
	if v, ok := fields["expected_revision"]; ok && v != nil {
		if _, ok := taskInt(v); !ok {
			return &tasks.StoreError{Msg: "expected_revision must be an integer >= 0"}
		}
	}
	for _, k := range []string{"parallelable", "repeat_custom"} {
		if v, ok := fields[k]; ok && v != nil {
			if _, ok := v.(bool); !ok {
				return &tasks.StoreError{Msg: k + " must be a boolean"}
			}
		}
	}
	return nil
}

// taskMinutes converts a JSON number to whole minutes, rejecting
// fractionals, negatives, and non-numbers (store.toInt truncates).
func taskMinutes(v any) (int, bool) { return taskInt(v) }

// taskInt converts a JSON number to a whole non-negative int, rejecting
// fractionals, negatives, and non-numbers.
func taskInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) || n < 0 {
			return 0, false
		}
		return int(n), true
	case int:
		if n < 0 {
			return 0, false
		}
		return n, true
	case int64:
		if n < 0 {
			return 0, false
		}
		return int(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil && i >= 0 {
			return int(i), true
		}
	}
	return 0, false
}

// taskRepeat reads the recurrence out of a request body. Out-of-range
// counts and unknown units are left for the store to reject, so the
// wording lives in one place.
func taskRepeat(fields map[string]any) tasks.Repeat {
	rep := tasks.Repeat{Text: taskStrField(fields, "repeat_rule")}
	if n, ok := taskInt(fields["repeat_every"]); ok {
		rep.Every = n
	}
	rep.Unit = strings.TrimSpace(taskStrField(fields, "repeat_unit"))
	if b, ok := fields["repeat_custom"].(bool); ok {
		rep.Custom = b
	}
	return rep.Normalize()
}

// createTask inserts one open task. Every field except repeat_rule is
// required (same rules as the agent's create_task); missing keys 422
// before any store touch.
func createTask(w http.ResponseWriter, r *http.Request) {
	fields, ok := decodeTaskBody(w, r)
	if !ok {
		return
	}
	if err := checkTaskFields(fields); err != nil {
		writeTaskErr(w, err)
		return
	}
	for _, k := range []string{"description", "due_date", "due_time", "estimated_minutes", "parallelable"} {
		if v, present := fields[k]; !present || v == nil {
			writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": k + " is required"})
			return
		}
	}
	for _, k := range []string{"description", "due_date", "due_time"} {
		if s, ok := fields[k].(string); !ok || strings.TrimSpace(s) == "" {
			writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": k + " is required"})
			return
		}
	}
	store, ok := taskStore(w)
	if !ok {
		return
	}
	minutes, ok := taskMinutes(fields["estimated_minutes"])
	if !ok {
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "estimated_minutes must be an integer >= 0"})
		return
	}
	parallelable, _ := fields["parallelable"].(bool)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	doc, err := store.Create(ctx,
		taskStrField(fields, "name"),
		taskStrField(fields, "description"),
		taskStrField(fields, "due_date"),
		taskStrField(fields, "due_time"),
		&minutes,
		taskRepeat(fields),
		&parallelable,
	)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, doc)
}

// taskItem dispatches /api/tasks/{id}[/{action}]: get + patch + delete on
// the id, post on complete/reopen. Auth first; unknown shapes 404.
func taskItem(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/tasks/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		// Trailing slash with no id behaves like the collection root.
		tasksRoot(w, r)
		return
	}
	id, action := parts[0], ""
	if len(parts) > 2 {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "not found"})
		return
	}
	if len(parts) == 2 {
		action = parts[1]
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		getTask(w, r, id)
	case action == "" && r.Method == http.MethodPatch:
		updateTask(w, r, id)
	case action == "" && r.Method == http.MethodDelete:
		deleteTask(w, r, id)
	case action == "complete" && r.Method == http.MethodPost:
		completeTask(w, r, id)
	case action == "skip" && r.Method == http.MethodPost:
		skipTask(w, r, id)
	case action == "reopen" && r.Method == http.MethodPost:
		reopenTask(w, r, id)
	default:
		if action != "" && action != "complete" && action != "skip" && action != "reopen" {
			writeJSON(w, http.StatusNotFound, bson.M{"detail": "not found"})
			return
		}
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "method not allowed"})
	}
}

func getTask(w http.ResponseWriter, r *http.Request, id string) {
	store, ok := taskStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	doc, err := store.Get(ctx, id)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// updateTask applies a partial edit (only sent keys change; empty
// repeat_rule clears the rule — same semantics as the agent's update_task).
// Omit all four repeat keys to leave the recurrence alone; the rule is
// never inferred from an empty value. expected_revision is the
// optimistic-concurrency guard: when present it must match the stored
// revision or the edit is rejected with 409 instead of overwriting.
func updateTask(w http.ResponseWriter, r *http.Request, id string) {
	fields, ok := decodeTaskBody(w, r)
	if !ok {
		return
	}
	if err := checkTaskFields(fields); err != nil {
		writeTaskErr(w, err)
		return
	}
	store, ok := taskStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	doc, err := store.Update(ctx, id, fields)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func deleteTask(w http.ResponseWriter, r *http.Request, id string) {
	store, ok := taskStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	found, err := store.Delete(ctx, id)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "unknown task '" + id + "'"})
		return
	}
	writeJSON(w, http.StatusOK, bson.M{"deleted": true})
}

// completeTask marks a task done (3-day retention starts server-side).
func completeTask(w http.ResponseWriter, r *http.Request, id string) {
	store, ok := taskStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	doc, next, rollover, rolloverDetail, err := store.CompleteDetail(ctx, id)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	// The task stays at the top level: a 4.6.0 app parses the response as
	// the bare doc, so nesting it under "task" would break completing a task
	// for anyone who hasn't updated. "next" is the only addition, and it is
	// present only when a structured cadence rolled itself over.
	out := bson.M{}
	for k, v := range doc {
		out[k] = v
	}
	if next != nil {
		out["next"] = next
	}
	// Additive, and the field the 4.7.0 review should have added (#180). An app
	// that does not know this key ignores it; an app that does can tell "one-shot"
	// from "repeats, and the rollover failed" instead of inferring from an absent
	// `next` — which is how a repeat that had stopped recurring still looked like a
	// successful completion.
	out["rollover"] = string(rollover)
	if rollover.NeedsAttention() {
		out["needs_attention"] = rollover.Attention(rolloverDetail)
	}
	writeJSON(w, http.StatusOK, out)
}

// skipTask resolves ONE occurrence as skipped rather than done (#209). The route
// is additive, so an app that has never heard of it is unaffected.
//
// The response is the same top-level shape as completeTask — the task, `next` and
// `rollover` — and only ONE field is genuinely new: `skipReason`. The `skipped`
// boolean is NOT added by this route; it is derived from `skippedAt` in toDoc and
// so has been present on every task response since #209, as `false` on anything
// completed or open. It reads as "added" only if you compare against a build from
// before the derivation existed.
//
// `skipReason` is camel case, matching the sibling server-set fields
// (`completedAt`, `skippedAt`); snake_case is reserved for the fields the client
// SENDS (`due_date`, `repeat_every`). An earlier version of this comment called
// both fields "added" and spelled the second one `skip_reason` — both wrong.
func skipTask(w http.ResponseWriter, r *http.Request, id string) {
	// Only `reason` is accepted, and a typo is a 422 rather than a silent empty
	// reason. The reason is the entire point of a skip — "out of time", "doing it
	// tomorrow" — and it is the part a human reads weeks later. A misspelt key that
	// decoded to an empty reason would leave the user believing they had recorded
	// why, with nothing recorded, which is the quiet disappearance #209 exists to
	// distinguish from a skip.
	//
	// Strictness is cheap HERE and expensive later, and this is the only moment it is
	// cheap: the route is new in #209 and has never shipped, so there is no client to
	// break. Once an app version is in the wild, adding a field here becomes a
	// breaking change, and the next reader should reopen that deliberately.
	fields, ok := decodeTaskBodyOpts(w, r, bodyOpts{
		allowEmpty: true,
		known:      map[string]bool{"reason": true},
	})
	if !ok {
		return
	}
	// `{"reason": null}` is treated as an omitted reason, on purpose, and deliberately
	// NOT rejected. JSON null counting as absent is the convention this file already
	// uses for every other field (see checkTaskFields: "JSON null counts as absent (no
	// change)"), and a null that meant "clear this" would be a different contract from
	// the one every other field already has. Rejecting it here would make `reason` the
	// one field where null is an error while `description`, `due_time` and the rest
	// accept it silently — a caller clearing optional values would get a 422 from one
	// endpoint and success from the others.
	//
	// Stated here because the ambiguity is real otherwise: null and omitted both mean
	// "no reason given", which is the same outcome either way.
	if raw, ok := fields["reason"]; ok && raw != nil {
		if _, isStr := raw.(string); !isStr {
			writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "reason must be a string"})
			return
		}
	}
	store, ok := taskStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	doc, next, rollover, err := store.Skip(ctx, id, taskStrField(fields, "reason"))
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	out := bson.M{}
	for k, v := range doc {
		out[k] = v
	}
	if next != nil {
		out["next"] = next
	}
	out["rollover"] = string(rollover)
	if rollover.NeedsAttention() {
		out["needs_attention"] = rollover.UserFacing()
	}
	writeJSON(w, http.StatusOK, out)
}

// reopenTask clears completion, making a done task open again.
func reopenTask(w http.ResponseWriter, r *http.Request, id string) {
	store, ok := taskStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	doc, err := store.Reopen(ctx, id)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// tasksRoot serves the collection endpoint: GET lists, POST creates.
func tasksRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if code, detail := authorize(r); code != 0 {
			writeJSON(w, code, bson.M{"detail": detail})
			return
		}
		createTask(w, r)
		return
	}
	listTasks(w, r)
}

// registerTasks wires this domain's routes. One function per file so main stays a
// list of domains, not a list of paths (#171).
func registerTasks(mux *http.ServeMux) {
	mux.HandleFunc("/api/tasks", tasksRoot)
	mux.HandleFunc("/api/tasks/", taskItem)
}
