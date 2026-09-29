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
// occurrence is purely the agent's job (see skills/task-manager/SKILL.md);
// complete_task only echoes the rule back so the caller can't miss it.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agento/internal/mongo"
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

// Repeat is a task's recurrence, in one of two mutually exclusive modes:
//
//	structured — Every N Unit ("every 3 days"), the machine-usable case;
//	custom     — the user's own words in Text, verbatim.
//
// An all-zero Repeat is a one-shot task. The server stores the recurrence
// but never advances it: creating the next occurrence is the caller's job
// (see skills/task-manager/SKILL.md).
type Repeat struct {
	// Every is the count, RepeatEveryMin..RepeatEveryMax; 0 = unset.
	Every int
	// Unit is one of RepeatUnits; "" = unset.
	Unit string
	// Custom selects the Text mode.
	Custom bool
	// Text is the custom condition, stored verbatim.
	Text string
}

const (
	// RepeatEveryMin/Max bound the structured count. The upper bound is
	// the largest interval a person tracks by eye (a 4-weekly cycle);
	// anything longer wants a custom condition instead.
	RepeatEveryMin = 1
	RepeatEveryMax = 28
)

// RepeatUnits lists the accepted Unit values, shortest first.
func RepeatUnits() []string { return []string{"days", "weeks", "months", "years"} }

func isRepeatUnit(u string) bool {
	for _, v := range RepeatUnits() {
		if u == v {
			return true
		}
	}
	return false
}

// IsZero reports the one-shot case: no cadence and no custom text.
func (r Repeat) IsZero() bool {
	return r.Every == 0 && r.Unit == "" && !r.Custom && r.Text == ""
}

// Normalize infers the mode flag from what the caller actually sent, so
// clients that predate Repeat.Custom (and the 4.6 migration, which only
// knew the free-text rule) keep working unchanged: text with no
// structured cadence means a custom condition.
func (r Repeat) Normalize() Repeat {
	if r.Text != "" && !r.Custom && r.Every == 0 && r.Unit == "" {
		r.Custom = true
	}
	return r
}

// Validate rejects the shapes that would leave the recurrence ambiguous.
// The one leniency is Normalize's: text alone is a custom condition.
func (r Repeat) Validate() error {
	r = r.Normalize()
	if r.Custom {
		if strings.TrimSpace(r.Text) == "" {
			return fail("repeat_custom is set but repeat_rule is empty — a custom condition needs the words")
		}
		if r.Every != 0 || r.Unit != "" {
			return fail("set either repeat_every/repeat_unit or a custom condition, not both")
		}
		return nil
	}
	if r.Every == 0 && r.Unit == "" {
		if strings.TrimSpace(r.Text) != "" {
			return fail("repeat_rule text needs repeat_custom = true")
		}
		return nil
	}
	if r.Every < RepeatEveryMin || r.Every > RepeatEveryMax {
		return fail("repeat_every must be %d-%d, got %d", RepeatEveryMin, RepeatEveryMax, r.Every)
	}
	if !isRepeatUnit(r.Unit) {
		return fail("repeat_unit must be one of %s, got '%s'",
			strings.Join(RepeatUnits(), "/"), r.Unit)
	}
	if strings.TrimSpace(r.Text) != "" {
		return fail("repeat_rule text needs repeat_custom = true")
	}
	return nil
}

// String renders the recurrence for humans: "Every 3 days", the custom
// words verbatim, or "" for a one-shot task.
func (r Repeat) String() string {
	r = r.Normalize()
	if r.Custom || (r.Every == 0 && r.Unit == "") {
		return r.Text
	}
	singular := strings.TrimSuffix(r.Unit, "s")
	if r.Every == 1 {
		return "Every " + singular
	}
	return "Every " + strconv.Itoa(r.Every) + " " + r.Unit
}

// docs renders the stored shape. Kept in one place so Create, Update and
// the migration can never disagree on the key names.
func (r Repeat) docs() bson.M {
	r = r.Normalize()
	return bson.M{
		"repeat_every":  r.Every,
		"repeat_unit":   r.Unit,
		"repeat_custom": r.Custom,
		"repeat_rule":   r.Text,
	}
}

// repeatFromDoc reads a doc's recurrence, tolerating pre-4.6 docs that
// only ever had the free-text repeat_rule string.
func repeatFromDoc(doc bson.M) Repeat {
	r := Repeat{
		Every:  0,
		Unit:   "",
		Custom: false,
		Text:   "",
	}
	if v, ok := doc["repeat_rule"].(string); ok {
		r.Text = v
	}
	if n, ok := toInt(doc["repeat_every"]); ok {
		r.Every = n
	}
	if u, ok := doc["repeat_unit"].(string); ok {
		r.Unit = u
	}
	if b, ok := toBool(doc["repeat_custom"]); ok {
		r.Custom = b
	}
	return r.Normalize()
}

var timeRE = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// StoreError is the domain error: servers catch it and return {ok: false, error}.
type StoreError struct{ Msg string }

func (e *StoreError) Error() string { return e.Msg }

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
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return nil, errors.New("MONGODB_URI is not set — MongoDB is the only backend")
	}
	if strings.TrimSpace(dbName) == "" {
		dbName = "hermes"
	}
	c, err := mongo.ConnectURI(uri)
	if err != nil {
		return nil, err
	}
	db := c.Database(dbName)
	s := &Store{client: c, db: db, tasks: db.Collection(coll)}
	if err := s.EnsureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

// FromEnv builds a Store from MONGODB_URI/MONGODB_DB (single root .env).
func FromEnv() (*Store, error) {
	return New(os.Getenv("MONGODB_URI"), os.Getenv("MONGODB_DB"))
}

// EnsureSchema creates the TTL + query indexes (idempotent).
func (s *Store) EnsureSchema(ctx context.Context) error {
	_, err := s.tasks.Indexes().CreateMany(ctx, []mongoDrv.IndexModel{
		{Keys: bson.D{{Key: "expiresAt", Value: 1}}, Options: options.Index().SetName("ttl_expiresAt").SetExpireAfterSeconds(0)},
		{Keys: bson.D{{Key: "completedAt", Value: 1}}, Options: options.Index().SetName("completedAt_1")},
		{Keys: bson.D{{Key: "due_date", Value: 1}}, Options: options.Index().SetName("due_date_1")},
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
		"completedAt":       nil, "createdAt": now,
	}
	for k, v := range rep.docs() {
		doc[k] = v
	}
	res, err := s.tasks.InsertOne(ctx, doc)
	if err != nil {
		return nil, err
	}
	doc["_id"] = res.InsertedID
	return toDoc(doc), nil
}

// List returns tasks filtered by state. state: "open" (default), "done", "all".
// overdue=true keeps only open tasks with due_date before today.
func (s *Store) List(ctx context.Context, state string, overdue bool, search string) ([]map[string]any, error) {
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
		return nil, fail("state must be open|done|all, got '%s'", state)
	}
	if overdue && state != "open" {
		return nil, fail("overdue only applies to state=open, got state='%s'", state)
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
	cur, err := s.tasks.Find(ctx, filt, options.Find().SetSort(bson.D{{Key: "due_date", Value: 1}, {Key: "createdAt", Value: 1}}))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []map[string]any{}
	for cur.Next(ctx) {
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			return nil, err
		}
		out = append(out, toDoc(doc))
	}
	return out, cur.Err()
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
		n, ok := toInt(v)
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
		b, ok := toBool(v)
		if !ok {
			return nil, fail("parallelable must be a boolean")
		}
		set["parallelable"] = b
	}
	if len(set) == 0 {
		return toDoc(cur), nil
	}
	if _, err := s.tasks.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$set": set}); err != nil {
		return nil, err
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
	// Wrong types fail loudly rather than reading as absent.
	if v, ok := fields["repeat_every"]; ok && v != nil {
		n, ok := toInt(v)
		if !ok {
			return rep, true, fail("repeat_every must be an integer %d-%d", RepeatEveryMin, RepeatEveryMax)
		}
		rep.Every = n
	}
	if str, ok := strField("repeat_unit"); ok {
		rep.Unit = strings.TrimSpace(str)
	}
	if v, ok := fields["repeat_custom"]; ok && v != nil {
		b, ok := toBool(v)
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
// (= completedAt + RetentionDays, TTL target). Returns the doc's
// recurrence verbatim so the caller can roll the next occurrence.
func (s *Store) Complete(ctx context.Context, id string) (map[string]any, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("bad id '%s'", id)
	}
	now := time.Now().UTC()
	res, err := s.tasks.UpdateOne(ctx,
		bson.M{"_id": oid, "completedAt": nil},
		bson.M{"$set": bson.M{
			"completedAt": primitive.NewDateTimeFromTime(now),
			"expiresAt":   primitive.NewDateTimeFromTime(now.AddDate(0, 0, RetentionDays)),
		}})
	if err != nil {
		return nil, err
	}
	if res.MatchedCount == 0 {
		var doc bson.M
		if ferr := s.tasks.FindOne(ctx, bson.M{"_id": oid}).Decode(&doc); ferr != nil {
			return nil, fail("unknown task '%s'", id)
		}
		return nil, fail("task '%s' is already completed", id)
	}
	return s.Get(ctx, id)
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
		bson.M{"$unset": bson.M{"completedAt": "", "expiresAt": ""}})
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

func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int32:
		return int(n), true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	}
	return 0, false
}

// toBool accepts real booleans only — strings like "true" are caller bugs,
// not values (same strictness as checkTaskFields on the HTTP layer).
func toBool(v any) (bool, bool) {
	b, ok := v.(bool)
	return b, ok
}
