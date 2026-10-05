// Package tasks: MongoDB-backed personal task manager for the agent.
//
// Collection: tasks. One doc per task with name, description, due date/time,
// estimated minutes, a recurrence (see Repeat) and a parallelable flag
// (true = can run alongside other tasks). There is NO status
// field: a task is open while completedAt is null and done once it is set.
//
// Completed tasks are retained 3 days via expiresAt TTL
// (= completedAt + RetentionDays); open tasks carry no expiresAt and never
// expire. A Repeat is never interpreted here — advancing to the next
// occurrence is purely the agent's job (see pi/skills/task-manager/SKILL.md);
// complete_task only echoes the rule back so the caller can't miss it.
package tasks

import (
	"context"
	"fmt"
	"log"
	"math"
	"regexp"
	"strings"
	"time"

	"agento/internal/mongostore"
	"agento/internal/validate"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	coll = "tasks"
	// RetentionDays drives the TTL target for completed tasks.
	RetentionDays = 3
)

var timeRE = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// StoreError is the domain error: servers catch it and return {ok: false, error}.
type StoreError struct {
	Msg      string
	Conflict bool
}

func (e *StoreError) Error() string { return e.Msg }

// conflict reports a lost optimistic-concurrency race. It is a distinct
// shape (not a validation failure) so the HTTP layer can answer 409 and
// the MCP layer can tell the agent to re-read rather than re-send.
func conflict(format string, args ...any) *StoreError {
	return &StoreError{Msg: "conflict: " + fmt.Sprintf(format, args...), Conflict: true}
}

func fail(format string, args ...any) *StoreError {
	return &StoreError{Msg: fmt.Sprintf(format, args...)}
}

// Store is the MongoDB backend for tasks.
type Store struct {
	client *mongoDrv.Client
	db     *mongoDrv.Database
	tasks  *mongoDrv.Collection
}

// New connects to uri/dbName and ensures indexes.
func New(uri, dbName string) (*Store, error) {
	c, db, err := mongostore.Open(uri, dbName)
	if err != nil {
		return nil, err
	}
	s := &Store{client: c, db: db, tasks: db.Collection(coll)}
	if err := s.EnsureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

// FromEnv builds a Store from MONGODB_URI/MONGODB_DB (single root .env).
func FromEnv() (*Store, error) {
	uri, db := mongostore.Env()
	return New(uri, db)
}

// EnsureSchema creates the TTL + query indexes (idempotent).
func (s *Store) EnsureSchema(ctx context.Context) error {
	_, err := s.tasks.Indexes().CreateMany(ctx, []mongoDrv.IndexModel{
		{Keys: bson.D{{Key: "expiresAt", Value: 1}}, Options: options.Index().SetName("ttl_expiresAt").SetExpireAfterSeconds(0)},
		{Keys: bson.D{{Key: "completedAt", Value: 1}}, Options: options.Index().SetName("completedAt_1")},
		{Keys: bson.D{{Key: "due_date", Value: 1}}, Options: options.Index().SetName("due_date_1")},
		// Read by ReconcileRollover once per completed structured repeat (up to 500
		// per run). Without this it is a collection scan per task, which is the
		// difference between the nightly job being free and being the slowest thing
		// in the stack.
		{Keys: bson.D{{Key: "rolled_from", Value: 1}}, Options: options.Index().SetName("rolled_from_1")},
	})
	return err
}

func checkDue(dueDate, dueTime string) error {
	if dueDate != "" {
		if _, err := validate.CheckDay(dueDate); err != nil {
			return fail("due_date: %s", err.Error())
		}
	}
	if dueTime != "" && !timeRE.MatchString(dueTime) {
		return fail("due_time must be HH:MM (24h), got '%s'", dueTime)
	}
	if dueTime != "" && dueDate == "" {
		return fail("due_time requires due_date")
	}
	return nil
}

func toDoc(doc bson.M) map[string]any {
	out := map[string]any{}
	for k, v := range doc {
		if k == "_id" {
			if o, ok := v.(primitive.ObjectID); ok {
				out["id"] = o.Hex()
			}
			continue
		}
		if dt, ok := v.(primitive.DateTime); ok {
			out[k] = dt.Time().UTC().Format(time.RFC3339)
			continue
		}
		out[k] = v
	}
	// Backfill keys that pre-mandatory docs lack, so every response
	// speaks the same contract (readers default the same way).
	// due_time "" is the "created before times were required" sentinel:
	// it is never invented here, and such a task becomes valid again the
	// moment it is written with a real time (Update rejects an explicit
	// ""), so legacy rows heal on first edit instead of needing a
	// migration that would guess a user's schedule.
	if _, ok := out["description"]; !ok {
		out["description"] = ""
	}
	if _, ok := out["due_date"]; !ok {
		out["due_date"] = ""
	}
	if _, ok := out["due_time"]; !ok {
		out["due_time"] = ""
	}
	if _, ok := out["estimated_minutes"]; !ok {
		out["estimated_minutes"] = 0
	}
	if _, ok := out["repeat_rule"]; !ok {
		out["repeat_rule"] = ""
	}
	if _, ok := out["revision"]; !ok {
		out["revision"] = 0
	}
	// Recurrence backfill: pre-4.6 docs carry only the free-text
	// repeat_rule, so the mode is inferred (text = custom) and the
	// structured keys come back neutral. Every response speaks the same
	// contract whether or not the 4.6 migration has run.
	if _, ok := out["repeat_every"]; !ok {
		out["repeat_every"] = 0
	}
	if _, ok := out["repeat_unit"]; !ok {
		out["repeat_unit"] = ""
	}
	if _, ok := out["repeat_custom"]; !ok {
		rule, _ := out["repeat_rule"].(string)
		out["repeat_custom"] = strings.TrimSpace(rule) != ""
	}
	if _, ok := out["parallelable"]; !ok {
		out["parallelable"] = false
	}
	// `rolled_from` is stripped: it is INTERNAL linkage between a task and the
	// occurrence it produced, and it lands on exactly the successor docs a user has no
	// reason to be reading. Exposing it invites a client to reconstruct rollover chains
	// from the API and then depend on that — reimplementing the reconciler's job against
	// a field that has no contract.
	//
	// Safe to strip, and deliberately checked before doing so: NOTHING reads this off a
	// rendered doc. The reconciler reads it from a raw projection (parentsWithSuccessors),
	// because that too is linkage it must not infer from a display document.
	// TestReconcileStillWorksWithRolledFromHidden pins that condition, so a future
	// reader who renders it again finds out immediately.
	if _, present := out[rolledFromField]; present {
		delete(out, rolledFromField)
	}
	return out
}

// Create inserts an open task. Every field except the repeat is required
// (an all-zero Repeat = one-shot task); parallelable marks tasks that can
// run alongside other tasks.
func (s *Store) Create(ctx context.Context, name, description, dueDate, dueTime string, estimatedMinutes *int, rep Repeat, parallelable *bool) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fail("name is required")
	}
	description = strings.TrimSpace(description)
	if description == "" {
		return nil, fail("description is required")
	}
	dueDate = strings.TrimSpace(dueDate)
	if dueDate == "" {
		return nil, fail("due_date is required (YYYY-MM-DD)")
	}
	dueTime = strings.TrimSpace(dueTime)
	if dueTime == "" {
		return nil, fail("due_time is required (HH:MM)")
	}
	if err := checkDue(dueDate, dueTime); err != nil {
		return nil, err
	}
	if estimatedMinutes == nil {
		return nil, fail("estimated_minutes is required")
	}
	if *estimatedMinutes < 0 {
		return nil, fail("estimated_minutes must be >= 0")
	}
	if parallelable == nil {
		return nil, fail("parallelable is required")
	}
	rep = rep.Normalize()
	if err := rep.Validate(); err != nil {
		return nil, err
	}
	now := primitive.NewDateTimeFromTime(time.Now().UTC())
	doc := bson.M{
		"name": name, "description": description,
		"due_date": dueDate, "due_time": dueTime,
		"estimated_minutes": *estimatedMinutes,
		"parallelable":      *parallelable,
		"revision":          0,
		"completedAt":       nil, "createdAt": now,
	}
	for k, v := range rep.docs() {
		doc[k] = v
	}
	return s.insert(ctx, doc)
}

// insert writes a prepared document and renders it. Split out of Create so that
// fields which must land ATOMICALLY with the insert can be added by callers that
// need them — specifically rollOver's `rolled_from` back-link (#180), which as a
// separate UpdateOne left a crash window where the successor existed but was
// unlinked, so the next nightly reconciler reported a gap and sent a human to
// create a task that already existed.
//
// An extra field added here is part of the same write. There is no second call to
// lose, so nothing has to reconcile afterwards.
func (s *Store) insert(ctx context.Context, doc bson.M) (map[string]any, error) {
	res, err := s.tasks.InsertOne(ctx, doc)
	if err != nil {
		return nil, err
	}
	doc["_id"] = res.InsertedID
	return toDoc(doc), nil
}

// List cap policy, mirroring the projects store: an unbounded Find grows
// with the collection, and every caller of List is a phone, a widget or a
// nag loop. limit <= 0 means the default; anything above the ceiling is
// clamped, never rejected. (#171: this pair of clamps wants one home.)
const (
	DefaultLimit = 200
	MaxLimit     = 500
)

// List returns tasks filtered by state. state: "open" (default), "done", "all".
// overdue=true keeps only open tasks with due_date before today.
// The second return value reports truncation: a +1 probe row is fetched and
// dropped, so callers can tell "exactly N" from "N of many" without a
// second query (#168).
func (s *Store) List(ctx context.Context, state string, overdue bool, search string, limit int) ([]map[string]any, bool, error) {
	if state == "" {
		state = "open"
	}
	filt := bson.M{}
	switch state {
	case "open":
		filt["completedAt"] = nil
	case "done":
		// $exists: reopened tasks have the field $unset (missing), and
		// bare $ne:null matches missing fields — both guards needed.
		filt["completedAt"] = bson.M{"$ne": nil, "$exists": true}
	case "all":
	default:
		return nil, false, fail("state must be open|done|all, got '%s'", state)
	}
	if overdue && state != "open" {
		return nil, false, fail("overdue only applies to state=open, got state='%s'", state)
	}
	if overdue {
		filt["completedAt"] = nil
		filt["due_date"] = bson.M{"$ne": "", "$lt": time.Now().Format("2006-01-02")}
	}
	if search = strings.TrimSpace(search); search != "" {
		// QuoteMeta: raw user input must never reach the regex engine.
		rx := regexp.QuoteMeta(search)
		filt["$or"] = []bson.M{
			{"name": bson.M{"$regex": rx, "$options": "i"}},
			{"description": bson.M{"$regex": rx, "$options": "i"}},
		}
	}
	lim := mongostore.ClampLimit(limit, DefaultLimit, MaxLimit)
	cur, err := s.tasks.Find(ctx, filt, options.Find().SetSort(bson.D{{Key: "due_date", Value: 1}, {Key: "createdAt", Value: 1}}).SetLimit(lim+1))
	if err != nil {
		return nil, false, err
	}
	defer cur.Close(ctx)
	out := []map[string]any{}
	for cur.Next(ctx) {
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			return nil, false, err
		}
		out = append(out, toDoc(doc))
	}
	if err := cur.Err(); err != nil {
		return nil, false, err
	}
	if int64(len(out)) > lim {
		return out[:int(lim)], true, nil
	}
	return out, false, nil
}

// Get fetches one task by hex id.
func (s *Store) Get(ctx context.Context, id string) (map[string]any, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("bad id '%s'", id)
	}
	var doc bson.M
	if err := s.tasks.FindOne(ctx, bson.M{"_id": oid}).Decode(&doc); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return nil, fail("unknown task '%s'", id)
		}
		return nil, err
	}
	return toDoc(doc), nil
}

// Update edits mutable fields of any task (open or done). Supplied values
// must satisfy the same mandatory rules as Create (empty description /
// due fields are rejected, not cleared); the recurrence stays clearable
// ("" / 0 = one-shot). Absent keys are untouched; due fields and the
// recurrence's four keys are validated together.
//
// expected_revision is the optimistic-concurrency guard (#184): when the
// caller sends the revision it read, a mismatch means someone else wrote
// first and the update is rejected instead of silently overwriting. Absent
// means unchecked, so old callers keep working. Every successful mutation
// bumps the revision, so the number the next reader sees is always fresh.
func (s *Store) Update(ctx context.Context, id string, fields map[string]any) (map[string]any, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("bad id '%s'", id)
	}
	var cur bson.M
	if err := s.tasks.FindOne(ctx, bson.M{"_id": oid}).Decode(&cur); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return nil, fail("unknown task '%s'", id)
		}
		return nil, err
	}
	// The guard is checked twice, on purpose. The comparison here gives
	// the precise message (which revision was wanted, which is stored);
	// the filter below makes it atomic, so two holders of the same number
	// cannot both win and a concurrent $inc cannot slip between.
	stored, _ := mongostore.ToInt(cur["revision"])
	guarded, want := false, 0
	if v, ok := fields["expected_revision"]; ok && v != nil {
		if f, isFloat := v.(float64); isFloat && f != math.Trunc(f) {
			return nil, fail("expected_revision must be an integer >= 0")
		}
		want, ok = mongostore.ToInt(v)
		if !ok || want < 0 {
			return nil, fail("expected_revision must be an integer >= 0")
		}
		if want != stored {
			return nil, conflict("task changed since revision %d (now %d) — reload and retry", want, stored)
		}
		guarded = true
	}
	set := bson.M{}
	strField := func(key string) (string, bool) {
		v, ok := fields[key]
		if !ok {
			return "", false
		}
		str, ok := v.(string)
		if !ok {
			return "", false
		}
		return str, true
	}
	// Wrong types fail loudly instead of becoming mystery no-ops
	// (strField above treats them as absent). JSON null still counts
	// as absent, matching the HTTP layer's convention.
	for _, k := range []string{"description", "due_date", "due_time", "repeat_rule"} {
		if v, ok := fields[k]; ok && v != nil {
			if _, ok := v.(string); !ok {
				return nil, fail("%s must be a string", k)
			}
		}
	}
	if v, ok := fields["name"]; ok && v != nil {
		name, _ := v.(string)
		if strings.TrimSpace(name) == "" {
			return nil, fail("name is required")
		}
		set["name"] = strings.TrimSpace(name)
	}
	if str, ok := strField("description"); ok {
		if strings.TrimSpace(str) == "" {
			return nil, fail("description is required")
		}
		set["description"] = strings.TrimSpace(str)
	}
	// Due fields flow through only when the caller sent them, so a
	// no-change update stays a no-op and the len(set)==0 path can fire.
	dueDate, _ := cur["due_date"].(string)
	dueTime, _ := cur["due_time"].(string)
	if str, ok := strField("due_date"); ok {
		if strings.TrimSpace(str) == "" {
			return nil, fail("due_date is required (YYYY-MM-DD)")
		}
		dueDate = strings.TrimSpace(str)
		set["due_date"] = dueDate
	}
	if str, ok := strField("due_time"); ok {
		if strings.TrimSpace(str) == "" {
			return nil, fail("due_time is required (HH:MM)")
		}
		dueTime = strings.TrimSpace(str)
		set["due_time"] = dueTime
	}
	if err := checkDue(dueDate, dueTime); err != nil {
		return nil, err
	}
	if v, ok := fields["estimated_minutes"]; ok && v != nil {
		n, ok := mongostore.ToInt(v)
		if !ok || n < 0 {
			return nil, fail("estimated_minutes must be >= 0")
		}
		set["estimated_minutes"] = n
	}
	// Recurrence: merge whatever was supplied onto the stored one and
	// validate the result, so the four keys can never end up describing
	// two different rules. Any of the four present means "edit the
	// recurrence"; none of them means leave it alone.
	rep, repTouched, err := mergeRepeat(cur, fields, strField)
	if err != nil {
		return nil, err
	}
	if repTouched {
		for k, v := range rep.docs() {
			set[k] = v
		}
	}
	if v, ok := fields["parallelable"]; ok && v != nil {
		b, ok := mongostore.ToBool(v)
		if !ok {
			return nil, fail("parallelable must be a boolean")
		}
		set["parallelable"] = b
	}
	// Nothing to change means nothing to write: without this the bump
	// below would mint a new revision for a read. Guarded, the revision
	// is still re-verified with a fresh read — otherwise a write landing
	// between our read and this return reports success to a stale editor.
	if len(set) == 0 {
		if !guarded {
			return toDoc(cur), nil
		}
		var latest bson.M
		if ferr := s.tasks.FindOne(ctx, bson.M{"_id": oid}).Decode(&latest); ferr != nil {
			return nil, fail("unknown task '%s'", id)
		}
		if now, _ := mongostore.ToInt(latest["revision"]); now != want {
			return nil, conflict("task changed since revision %d (now %d) — reload and retry", want, now)
		}
		return toDoc(latest), nil
	}
	update := bson.M{"$set": set, "$inc": bson.M{"revision": 1}}
	filt := bson.M{"_id": oid}
	if guarded {
		// A doc that predates revisions has no field but reads as 0
		// (toDoc backfill): matching only revision:0 would false-conflict
		// it forever, unretryably, against its own displayed number.
		if want == 0 {
			filt["$or"] = []bson.M{{"revision": 0}, {"revision": bson.M{"$exists": false}}}
		} else {
			filt["revision"] = want
		}
	}
	res, err := s.tasks.UpdateOne(ctx, filt, update)
	if err != nil {
		return nil, err
	}
	if res.MatchedCount == 0 {
		// Guarded and unmatched: someone won the race after our read.
		// Re-read to name the current revision rather than guessing.
		// (Unguarded cannot miss: the id was just read above.)
		var latest bson.M
		if ferr := s.tasks.FindOne(ctx, bson.M{"_id": oid}).Decode(&latest); ferr != nil {
			return nil, fail("unknown task '%s'", id)
		}
		now, _ := mongostore.ToInt(latest["revision"])
		return nil, conflict("task changed since revision %d (now %d) — reload and retry", want, now)
	}
	return s.Get(ctx, id)
}

// mergeRepeat folds the supplied recurrence fields onto the task's stored
// recurrence and validates the result. touched is false when the caller
// sent none of the four keys, so an unrelated edit leaves the rule alone.
//
// One compatibility rule lives here: a client that sends repeat_rule ""
// with no structured keys is asking to clear the rule (the only way a
// pre-4.6 client can express that), so the whole recurrence goes — not
// just the text, which would leave a cadence the client cannot see.
func mergeRepeat(cur bson.M, fields map[string]any, strField func(string) (string, bool)) (Repeat, bool, error) {
	present := func(keys ...string) bool {
		for _, k := range keys {
			if v, ok := fields[k]; ok && v != nil {
				return true
			}
		}
		return false
	}
	structuredSent := present("repeat_every", "repeat_unit", "repeat_custom")
	if !present("repeat_rule", "repeat_every", "repeat_unit", "repeat_custom") {
		return Repeat{}, false, nil
	}
	rep := repeatFromDoc(cur)
	// Wrong types fail loudly rather than reading as absent. Fractionals
	// are rejected, not truncated: 3.5 is a caller bug, and the store is
	// the last layer that can say so (same guard as expected_revision).
	if v, ok := fields["repeat_every"]; ok && v != nil {
		if f, isFloat := v.(float64); isFloat && f != math.Trunc(f) {
			return rep, true, fail("repeat_every must be an integer %d-%d", RepeatEveryMin, RepeatEveryMax)
		}
		n, ok := mongostore.ToInt(v)
		if !ok {
			return rep, true, fail("repeat_every must be an integer %d-%d", RepeatEveryMin, RepeatEveryMax)
		}
		rep.Every = n
	}
	if str, ok := strField("repeat_unit"); ok {
		rep.Unit = strings.TrimSpace(str)
	}
	if v, ok := fields["repeat_custom"]; ok && v != nil {
		b, ok := mongostore.ToBool(v)
		if !ok {
			return rep, true, fail("repeat_custom must be a boolean")
		}
		rep.Custom = b
	}
	if str, ok := strField("repeat_rule"); ok {
		rep.Text = strings.TrimSpace(str)
	}
	// "No text, no structured keys" from an older client = clear it all.
	if rep.Text == "" && !structuredSent && (rep.Every != 0 || rep.Unit != "" || rep.Custom) {
		rep = Repeat{}
	}
	rep = rep.Normalize()
	if err := rep.Validate(); err != nil {
		return rep, true, err
	}
	return rep, true, nil
}

// Complete marks a task done: sets completedAt + expiresAt
// (= completedAt + RetentionDays, TTL target).
//
// A task with a **structured** cadence ("every 3 days") rolls itself over
// here: the next occurrence is created with the date advanced and every
// other field carried over, so nobody is asked to pick a date the server
// can already compute. A **custom** condition is left to the caller —
// "every 3rd Friday" and "end of every month" need the user or the agent,
// and the server will not guess. One-shot tasks roll over to nothing.
//
// Returns the completed doc, the new task when one was created, and WHY one was
// not. That third value is the point of #180: the caller used to see `next == nil`
// and could not distinguish "one-shot, nothing to do" from "repeats, but the
// rollover failed", so a task that had silently stopped recurring was reported as
// an ordinary completion.
func (s *Store) Complete(ctx context.Context, id string) (map[string]any, map[string]any, Rollover, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, nil, "", fail("bad id '%s'", id)
	}
	now := time.Now().UTC()
	res, err := s.tasks.UpdateOne(ctx,
		bson.M{"_id": oid, "completedAt": nil},
		bson.M{
			"$set": bson.M{
				"completedAt": primitive.NewDateTimeFromTime(now),
				"expiresAt":   primitive.NewDateTimeFromTime(now.AddDate(0, 0, RetentionDays)),
			},
			"$inc": bson.M{"revision": 1},
		})
	if err != nil {
		return nil, nil, "", err
	}
	if res.MatchedCount == 0 {
		var doc bson.M
		if ferr := s.tasks.FindOne(ctx, bson.M{"_id": oid}).Decode(&doc); ferr != nil {
			return nil, nil, "", fail("unknown task '%s'", id)
		}
		return nil, nil, "", fail("task '%s' is already completed", id)
	}
	done, err := s.Get(ctx, id)
	if err != nil {
		return nil, nil, "", err
	}
	next, reason, rerr := s.rollOver(ctx, done)
	if rerr != nil {
		// The task is already marked done, so a failed rollover must not
		// make the completion look like it failed. It is reported as
		// RolloverFailed rather than swallowed, because that reason is what
		// tells the caller — and now the reconciler — that the repeat has
		// stopped. Logged too, because the reconciler runs nightly and the
		// user may act sooner.
		log.Printf("task %s completed but rollover failed: %v", id, rerr)
		return done, nil, RolloverFailed, nil
	}
	return done, next, reason, nil
}

// Reopen clears completion (completedAt + expiresAt), making it open again.
// Errors on unknown ids and on tasks that are already open.
func (s *Store) Reopen(ctx context.Context, id string) (map[string]any, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("bad id '%s'", id)
	}
	res, err := s.tasks.UpdateOne(ctx,
		bson.M{"_id": oid, "completedAt": bson.M{"$ne": nil, "$exists": true}},
		bson.M{
			"$unset": bson.M{"completedAt": "", "expiresAt": ""},
			"$inc":   bson.M{"revision": 1},
		})
	if err != nil {
		return nil, err
	}
	if res.MatchedCount == 0 {
		var doc bson.M
		if ferr := s.tasks.FindOne(ctx, bson.M{"_id": oid}).Decode(&doc); ferr != nil {
			return nil, fail("unknown task '%s'", id)
		}
		return nil, fail("task '%s' is already open", id)
	}
	return s.Get(ctx, id)
}

// Delete removes a task regardless of completion.
func (s *Store) Delete(ctx context.Context, id string) (bool, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return false, fail("bad id '%s'", id)
	}
	res, err := s.tasks.DeleteOne(ctx, bson.M{"_id": oid})
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}
