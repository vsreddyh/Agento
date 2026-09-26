// Package tasks: MongoDB-backed personal task manager for the agent.
//
// Collection: tasks. One doc per task with name, description, due date/time,
// estimated minutes, and a free-form repeat_rule string. There is NO status
// field: a task is open while completedAt is null and done once it is set.
//
// Completed tasks are retained 3 days via expiresAt TTL
// (= completedAt + RetentionDays); open tasks carry no expiresAt and never
// expire. repeat_rule is stored verbatim and never interpreted here —
// interpretation + next-occurrence creation is purely the agent's job
// (see skills/task-manager/SKILL.md); complete_task only echoes the rule
// back so the caller can't miss it.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
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
	return out
}

// Create inserts an open task. Name required; repeat_rule stored verbatim.
func (s *Store) Create(ctx context.Context, name, description, dueDate, dueTime string, estimatedMinutes int, repeatRule string) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fail("name is required")
	}
	if err := checkDue(dueDate, dueTime); err != nil {
		return nil, err
	}
	if estimatedMinutes < 0 {
		return nil, fail("estimated_minutes must be >= 0")
	}
	now := primitive.NewDateTimeFromTime(time.Now().UTC())
	doc := bson.M{
		"name": name, "description": description,
		"due_date": dueDate, "due_time": dueTime,
		"estimated_minutes": estimatedMinutes,
		"repeat_rule":       strings.TrimSpace(repeatRule),
		"completedAt":       nil, "createdAt": now,
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

// Update edits mutable fields of any task (open or done). Empty repeat_rule
// clears the rule; due fields validated together.
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
	if v, ok := fields["name"]; ok {
		name, _ := v.(string)
		if strings.TrimSpace(name) == "" {
			return nil, fail("name is required")
		}
		set["name"] = strings.TrimSpace(name)
	}
	if str, ok := strField("description"); ok {
		set["description"] = str
	}
	// Due fields flow through only when the caller sent them, so a
	// no-change update stays a no-op and the len(set)==0 path can fire.
	dueDate, _ := cur["due_date"].(string)
	dueTime, _ := cur["due_time"].(string)
	if str, ok := strField("due_date"); ok {
		dueDate = str
		set["due_date"] = str
	}
	if str, ok := strField("due_time"); ok {
		dueTime = str
		set["due_time"] = str
	}
	if err := checkDue(dueDate, dueTime); err != nil {
		return nil, err
	}
	if v, ok := fields["estimated_minutes"]; ok {
		n, ok := toInt(v)
		if !ok || n < 0 {
			return nil, fail("estimated_minutes must be >= 0")
		}
		set["estimated_minutes"] = n
	}
	if str, ok := strField("repeat_rule"); ok {
		set["repeat_rule"] = strings.TrimSpace(str)
	}
	if len(set) == 0 {
		return toDoc(cur), nil
	}
	if _, err := s.tasks.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$set": set}); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Complete marks a task done: sets completedAt + expiresAt
// (= completedAt + RetentionDays, TTL target). Returns the doc's
// repeat_rule verbatim so the caller can roll the next occurrence.
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
