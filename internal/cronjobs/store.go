package cronjobs

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agento/internal/mongostore"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	coll = "cron_jobs"
	// RunHistoryCap bounds the stored runs per job (newest last). The app
	// shows this short history per cron chat; older runs are dropped on
	// write, so no retention job is needed.
	RunHistoryCap = 10
	// ListCap bounds one list response.
	ListCap = 100
)

// Run is one firing: when it ran, what the model said (or the miss note),
// and whether the scheduled instant was missed while down.
type Run struct {
	At     int64  `bson:"at" json:"at"`
	Text   string `bson:"text" json:"text"`
	Missed bool   `bson:"missed,omitempty" json:"missed,omitempty"`
}

// Job is one scheduled prompt.
type Job struct {
	ID        string `bson:"_id,omitempty" json:"id"`
	Name      string `bson:"name" json:"name"`
	Prompt    string `bson:"prompt" json:"prompt"`
	Schedule  string `bson:"schedule" json:"schedule"`
	Paused    bool   `bson:"paused" json:"paused"`
	Delivery  string `bson:"delivery" json:"delivery"`
	CreatedAt int64  `bson:"createdAt" json:"createdAt"`
	UpdatedAt int64  `bson:"updatedAt" json:"updatedAt"`
	// LastFire is the latest scheduled instant accounted for (fired or
	// recorded missed), Unix millis. Zero when never evaluated.
	LastFire int64 `bson:"lastFire" json:"lastFire"`
	Runs     []Run `bson:"runs" json:"runs"`
}

// StoreError is the domain error: the HTTP layer answers 400, 404 when
// NotFound, or 502 when Upstream (a transport failure, not the caller's
// mistake).
type StoreError struct {
	Msg      string
	NotFound bool
	Upstream bool
}

func (e *StoreError) Error() string { return e.Msg }

func fail(format string, args ...any) *StoreError {
	return &StoreError{Msg: fmt.Sprintf(format, args...)}
}

func notFound(id string) *StoreError {
	return &StoreError{Msg: "unknown job " + id, NotFound: true}
}

// Store is the MongoDB backend for cron jobs.
type Store struct {
	jobs *mongoDrv.Collection
}

// New connects to uri/dbName.
func New(uri, dbName string) (*Store, error) {
	_, db, err := mongostore.Open(uri, dbName)
	if err != nil {
		return nil, err
	}
	return &Store{jobs: db.Collection(coll)}, nil
}

// FromEnv connects from the shared MONGODB_URI/MONGODB_DB pair.
func FromEnv() (*Store, error) {
	uri, db := mongostore.Env()
	return New(uri, db)
}

func toDoc(j Job) bson.M {
	runs := make([]bson.M, 0, len(j.Runs))
	for _, r := range j.Runs {
		runs = append(runs, bson.M{"at": r.At, "text": r.Text, "missed": r.Missed})
	}
	return bson.M{
		"name": j.Name, "prompt": j.Prompt, "schedule": j.Schedule,
		"paused": j.Paused, "delivery": j.Delivery,
		"createdAt": j.CreatedAt, "updatedAt": j.UpdatedAt,
		"lastFire": j.LastFire, "runs": runs,
	}
}

func strVal(m bson.M, key string) string {
	s, _ := m[key].(string)
	return s
}

func int64Val(m bson.M, key string) int64 {
	switch v := m[key].(type) {
	case int64:
		return v
	case int32:
		return int64(v)
	case int:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
}

func fromDoc(m bson.M) Job {
	j := Job{
		Name:      strVal(m, "name"),
		Prompt:    strVal(m, "prompt"),
		Schedule:  strVal(m, "schedule"),
		Delivery:  strVal(m, "delivery"),
		CreatedAt: int64Val(m, "createdAt"),
		UpdatedAt: int64Val(m, "updatedAt"),
		LastFire:  int64Val(m, "lastFire"),
	}
	if id, ok := m["_id"].(primitive.ObjectID); ok {
		j.ID = id.Hex()
	}
	if pb, ok := m["paused"].(bool); ok {
		j.Paused = pb
	}
	if arr, ok := m["runs"].(bson.A); ok {
		for _, v := range arr {
			rm, ok := v.(bson.M)
			if !ok {
				continue
			}
			r := Run{
				At:   int64Val(rm, "at"),
				Text: strVal(rm, "text"),
			}
			if mb, ok := rm["missed"].(bool); ok {
				r.Missed = mb
			}
			j.Runs = append(j.Runs, r)
		}
	}
	return j
}

func validate(prompt, schedule string) error {
	if strings.TrimSpace(prompt) == "" {
		return fail("prompt is required")
	}
	if _, err := Parse(schedule); err != nil {
		return fail("bad schedule: %v", err)
	}
	return nil
}

// Create stores a job; paused always starts false (pause is explicit).
func (s *Store) Create(ctx context.Context, name, prompt, schedule, delivery string) (Job, error) {
	if err := validate(prompt, schedule); err != nil {
		return Job{}, err
	}
	if strings.TrimSpace(delivery) == "" {
		delivery = "local"
	}
	now := time.Now().UnixMilli()
	j := Job{
		Name: name, Prompt: prompt, Schedule: strings.TrimSpace(schedule),
		Delivery: delivery, CreatedAt: now, UpdatedAt: now, Runs: []Run{},
	}
	res, err := s.jobs.InsertOne(ctx, toDoc(j))
	if err != nil {
		return Job{}, err
	}
	if id, ok := res.InsertedID.(primitive.ObjectID); ok {
		j.ID = id.Hex()
	}
	return j, nil
}

func objectID(id string) (primitive.ObjectID, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return primitive.ObjectID{}, notFound(id)
	}
	return oid, nil
}

// Get returns one job by id. A missing doc is NotFound; anything else
// (transport failure) is Upstream rather than a 404 for an outage.
func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	oid, err := objectID(id)
	if err != nil {
		return Job{}, err
	}
	var m bson.M
	if err := s.jobs.FindOne(ctx, bson.M{"_id": oid}).Decode(&m); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return Job{}, notFound(id)
		}
		return Job{}, &StoreError{Msg: "job read failed: " + err.Error(), Upstream: true}
	}
	j := fromDoc(m)
	j.ID = oid.Hex()
	return j, nil
}

// List returns all jobs, oldest first, bounded.
func (s *Store) List(ctx context.Context) ([]Job, error) {
	cur, err := s.jobs.Find(ctx, bson.M{},
		options.Find().SetSort(bson.M{"createdAt": 1}).SetLimit(ListCap))
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []Job
	for cur.Next(ctx) {
		var m bson.M
		if err := cur.Decode(&m); err != nil {
			continue
		}
		j := fromDoc(m)
		if id, ok := m["_id"].(primitive.ObjectID); ok {
			j.ID = id.Hex()
		}
		out = append(out, j)
	}
	if out == nil {
		out = []Job{}
	}
	return out, cur.Err()
}

// Update applies the non-nil fields; nil means "leave alone". Schedule is
// re-validated when given. Returns the updated job.
func (s *Store) Update(ctx context.Context, id string, name, prompt, schedule, delivery *string, paused *bool) (Job, error) {
	oid, err := objectID(id)
	if err != nil {
		return Job{}, err
	}
	set := bson.M{"updatedAt": time.Now().UnixMilli()}
	if name != nil {
		set["name"] = *name
	}
	if prompt != nil {
		if strings.TrimSpace(*prompt) == "" {
			return Job{}, fail("prompt is required")
		}
		set["prompt"] = *prompt
	}
	if schedule != nil {
		if _, err := Parse(*schedule); err != nil {
			return Job{}, fail("bad schedule: %v", err)
		}
		set["schedule"] = strings.TrimSpace(*schedule)
	}
	if delivery != nil {
		set["delivery"] = *delivery
	}
	if paused != nil {
		set["paused"] = *paused
	}
	res := s.jobs.FindOneAndUpdate(ctx, bson.M{"_id": oid},
		bson.M{"$set": set}, options.FindOneAndUpdate().SetReturnDocument(options.After))
	var m bson.M
	if err := res.Decode(&m); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return Job{}, notFound(id)
		}
		return Job{}, &StoreError{Msg: "job update failed: " + err.Error(), Upstream: true}
	}
	j := fromDoc(m)
	j.ID = oid.Hex()
	return j, nil
}

// Delete removes a job; unknown ids are NotFound.
func (s *Store) Delete(ctx context.Context, id string) error {
	oid, err := objectID(id)
	if err != nil {
		return err
	}
	res, err := s.jobs.DeleteOne(ctx, bson.M{"_id": oid})
	if err != nil {
		return err
	}
	if res.DeletedCount == 0 {
		return notFound(id)
	}
	return nil
}

// RecordRun appends a run (fired or missed), advances LastFire to the
// accounted scheduled instant, and trims history to the cap. Runs the
// app has already seen stay put — trimming drops only the oldest.
func (s *Store) RecordRun(ctx context.Context, id string, run Run, fireInstant int64) error {
	oid, err := objectID(id)
	if err != nil {
		return err
	}
	res, err := s.jobs.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{
		"$push": bson.M{"runs": bson.M{
			"$each":  []bson.M{{"at": run.At, "text": run.Text, "missed": run.Missed}},
			"$slice": -RunHistoryCap,
		}},
		"$set": bson.M{"lastFire": fireInstant, "updatedAt": time.Now().UnixMilli()},
	})
	if err != nil {
		return &StoreError{Msg: "run record failed: " + err.Error(), Upstream: true}
	}
	if res.MatchedCount == 0 {
		return notFound(id)
	}
	return nil
}
