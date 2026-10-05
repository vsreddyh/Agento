package main

// Task routes: the Task Manager surface over the shared tasks collection (#171).

import (
	"agento/internal/tasks"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
		// A reused Idempotency-Key with a different body. 422 like any other
		// validation failure, but named separately: the fix is a NEW key, not
		// different fields, and the message has to say which.
		if se.Mismatch {
			writeJSON(w, http.StatusUnprocessableEntity, bson.M{
				"detail": se.Msg,
				"hint":   "idempotency keys are single-use per distinct request body",
			})
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
	// Idempotency (#176). The header is the contract: the same key plus the same
	// body returns the SAME task, not a second one. Absent means the old
	// behaviour, so every existing client is unaffected.
	//
	// `replayed` comes back from the STORE, not from a lookup here — the unique
	// index decides, so a retry racing the original request cannot slip through a
	// read-then-write window and create the duplicate this prevents.
	idemKey := r.Header.Get("Idempotency-Key")
	source := requestSource(r)
	doc, replayed, err := store.CreateWithKey(ctx,
		taskStrField(fields, "name"),
		taskStrField(fields, "description"),
		taskStrField(fields, "due_date"),
		taskStrField(fields, "due_time"),
		&minutes,
		taskRepeat(fields),
		&parallelable,
		idemKey,
		source,
		taskFingerprint(fields),
	)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	store.RecordMutation(ctx, tasks.OpCreate, fmt.Sprint(doc["id"]), source, "http POST /api/tasks")
	if replayed {
		// 200, not 201: nothing was created this time. A client counting 201s to
		// learn what it created would otherwise be told it made a new task.
		if idemKey != "" {
			w.Header().Set("Idempotent-Replay", "true")
		}
		writeJSON(w, http.StatusOK, doc)
		return
	}
	writeJSON(w, http.StatusCreated, doc)
}

// changedKeys names which fields a PATCH touched, for the audit log. "PATCH
// /api/tasks" alone does not say whether the user moved a due date or fixed a
// typo, and that is the whole reason the log exists.
func changedKeys(fields map[string]any) string {
	if len(fields) == 0 {
		return "no fields"
	}
	keys := make([]string, 0, len(fields))
	for k := range fields {
		if k == "expected_revision" {
			continue // concurrency plumbing, not a change to the task
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "no fields"
	}
	return strings.Join(keys, ",")
}

// zeroLike returns the zero value of whatever type this field takes elsewhere, so an
// absent key and an explicit zero hash the same. Type is taken from the OTHER keys
// present in the same body where possible, because the digest has no schema of its own;
// the fallbacks match what taskStrField / taskMinutes / the bool read would produce.
func zeroLike(fields map[string]any, key string) any {
	switch key {
	case "estimated_minutes", "repeat_every":
		for _, probe := range []string{"estimated_minutes", "repeat_every"} {
			switch fields[probe].(type) {
			case float64:
				return float64(0)
			case int:
				return 0
			case json.Number:
				return json.Number("0")
			}
		}
		return float64(0)
	case "parallelable", "repeat_custom":
		for _, probe := range []string{"parallelable", "repeat_custom"} {
			if _, ok := fields[probe].(bool); ok {
				return false
			}
		}
		return false
	default:
		return ""
	}
}

// taskCreateFields is every body key the create path READS, and therefore every key
// the idempotency fingerprint must cover.
//
// It is a named list rather than something derived because the consumers are spread
// across taskStrField / taskMinutes / taskRepeat and nothing enforces that a new
// consumed field is added here. That gap was real and shipped: `repeat_rule` is
// consumed into Repeat.Text, validated, and STORED (repeat.go), yet was missing from
// this list — so two creates differing only in their custom repeat text ("3rd Friday"
// vs "end of month") hashed identically and the second was handed the first's task as
// a replay. The silent wrong answer, reached through the mechanism built to prevent it.
//
// `TestFingerprintCoversEveryConsumedField` walks this list and asserts each field
// changes the digest, which is what makes the next omission a test failure rather than
// a review finding. When a field is added to the create path, add it here too.
var taskCreateFields = []string{
	"name", "description", "due_date", "due_time",
	"estimated_minutes", "parallelable",
	"repeat_every", "repeat_unit", "repeat_custom", "repeat_rule",
}

// trimmedForFingerprint is the subset of taskCreateFields the create path TrimSpaces
// before storing. It must mirror that list exactly: a field trimmed by the store but
// hashed verbatim makes the digest stricter than reality and 422s a legitimate retry,
// and a field trimmed by the digest but stored verbatim makes it looser — two
// genuinely different requests would replay as one.
//
// Kept as its own map rather than derived, because the trimming is spread across two
// layers (the store trims the four core fields, taskRepeat trims the unit) and there is
// nothing to derive from. TestFingerprintAgreesWithWhatTheStoreTrims round-trips
// through the store to keep the two in step.
var trimmedForFingerprint = map[string]bool{
	"name": true, "description": true,
	"due_date": true, "due_time": true,
	"repeat_unit": true,
	// repeat_rule is here because Repeat.Normalize now trims Text, and the digest
	// mirrors what the store writes. It was deliberately EXCLUDED while create stored
	// the text raw — and that exclusion was correct then. It became wrong the moment
	// the store started trimming, which is the argument for deriving this list from the
	// store rather than restating it here: the exclusion was right about the old
	// behaviour and silently became a bug when the behaviour changed.
	"repeat_rule": true,
}

// taskFingerprint digests a create body so a reused Idempotency-Key can be told
// apart from a genuine retry.
//
// Derived from the DECODED body, not the raw bytes: encoding/json emits map keys in
// sorted order, so this is canonical, and a client that re-serialises its retry with
// different key order or indentation still matches. Hashing the bytes would 422 a
// legitimate retry for a cosmetic difference.
//
// Only the fields the create actually consumes, so a client adding an unknown key
// to a NEW version of its payload is not rejected for it.
func taskFingerprint(fields map[string]any) string {
	if len(fields) == 0 {
		return ""
	}
	// Every key is present in the digest, defaulting to its zero value when absent.
	//
	// The store reads an absent key and a zero-valued one IDENTICALLY —
	// taskStrField returns "" either way, and the bool/number reads yield false/0 —
	// so the digest must too. Skipping absent keys (the obvious implementation) makes
	// a body that omits `repeat_rule` hash differently from the same body sending
	// `"repeat_rule": ""`, and since the mismatch check only fires on a replay, that
	// turns a client merely upgrading to an explicit empty value into a 422 on its own
	// retry of a request that succeeded. A false rejection of a legitimate retry is
	// the same class of silent-wrong-answer as the one this guards against.
	// Trimmed exactly where the create path trims, and NOWHERE else.
	//
	// The store TrimSpaces name, description, due_date and due_time, and taskRepeat
	// (this layer) TrimSpaces repeat_unit — so " foo " and "foo" create the SAME task.
	// Hashing them verbatim made the digest disagree with the store about what "the
	// same request" means, so a retry differing only in surrounding whitespace got a
	// 422 Mismatch instead of a replay. Same class as the repeat_rule omission: the
	// digest is only correct if it agrees with what actually gets stored.
	//
	// repeat_rule is deliberately NOT trimmed, because create stores it verbatim —
	// measured, not assumed. ("  3rd Friday  " and "3rd Friday" really are two
	// different stored values, so digesting them differently is correct. Note the
	// update path does trim it, so the two disagree; that is the store's business, not
	// the digest's, and the digest only ever covers create.)
	//
	// Verified by round-tripping through the store rather than by reading it: a body
	// padded on all five fields must digest the same as the trimmed body AND produce
	// the same stored task.
	trimmed := trimmedForFingerprint
	relevant := make(map[string]any, len(taskCreateFields))
	for _, k := range taskCreateFields {
		v, ok := fields[k]
		if !ok {
			relevant[k] = zeroLike(fields, k)
			continue
		}
		if trimmed[k] {
			if str, isStr := v.(string); isStr {
				v = strings.TrimSpace(str)
			}
		}
		relevant[k] = v
	}
	if len(relevant) == 0 {
		return ""
	}
	raw, err := json.Marshal(relevant)
	if err != nil {
		// Unencodable means the body holds a type the create will reject anyway.
		// Returning "" disables the mismatch check rather than failing the write.
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// requestSource names the caller for the audit log (#176). The app sends
// X-Agento-Source; the header is trusted only as a LABEL — it decides what the log
// says, never what the caller may do, which authorize() already settled.
func requestSource(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("X-Agento-Source")); v != "" {
		return v
	}
	return "http"
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
	store.RecordMutation(ctx, tasks.OpUpdate, id, requestSource(r), changedKeys(fields))
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
	// Logged AFTER the delete, so there is no window where the log claims a delete
	// that was then rolled back by a failed response.
	store.RecordMutation(ctx, tasks.OpDelete, id, requestSource(r), "http DELETE")
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
	doc, next, rollover, err := store.Complete(ctx, id)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	store.RecordMutation(ctx, tasks.OpComplete, id, requestSource(r), fmt.Sprint(rollover))
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
		out["needs_attention"] = rollover.UserFacing()
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
	store.RecordMutation(ctx, tasks.OpSkip, id, requestSource(r), fmt.Sprint(rollover))
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
	store.RecordMutation(ctx, tasks.OpReopen, id, requestSource(r), "")
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
