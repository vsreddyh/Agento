package runs

import (
	"context"
	"errors"
	"time"

	"agento/internal/mongostore"

	"go.mongodb.org/mongo-driver/bson"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoStore is the MongoDB backend for runs.
type MongoStore struct {
	runs *mongoDrv.Collection
	// Logf receives operational notes. Nil discards them; main sets it so a
	// skipped corrupt row is visible instead of silent.
	Logf func(string, ...any)
}

func (s *MongoStore) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// New connects to uri/dbName.
func New(uri, dbName string) (*MongoStore, error) {
	_, db, err := mongostore.Open(uri, dbName)
	if err != nil {
		return nil, err
	}
	return &MongoStore{runs: db.Collection(Collection)}, nil
}

// FromEnv connects from the shared MONGODB_URI/MONGODB_DB pair.
func FromEnv() (*MongoStore, error) {
	uri, db := mongostore.Env()
	return New(uri, db)
}

// EnsureIndexes creates the two indexes the routes depend on: a TTL index so a
// finished run's record disappears without a retention job, and a lookup for
// the conversation reattach query.
//
// Best-effort by design at the call site: a gateway that cannot create an index
// (a restricted Atlas user, say) still serves runs; it just keeps records
// longer. That failure is logged loudly by main rather than taking chat down.
func (s *MongoStore) EnsureIndexes(ctx context.Context) error {
	// TTL indexes are ignored on a field that is missing (ExpiresAt is
	// always set by the gateway, so this is belt-and-braces).
	_, err := s.runs.Indexes().CreateMany(ctx, []mongoDrv.IndexModel{
		{
			Keys:    bson.D{{Key: "expiresAt", Value: 1}},
			Options: options.Index().SetExpireAfterSeconds(0).SetName("runs_ttl"),
		},
		{
			Keys: bson.D{
				{Key: "conversationId", Value: 1},
				{Key: "createdAt", Value: -1},
			},
			Options: options.Index().SetName("runs_conversation"),
		},
	})
	return err
}

// Create inserts a run. The caller supplies the id so the HTTP response can
// carry it before the write lands. An empty id is refused like in Save:
// silently inserting a "" _id document would fork a record no route can
// address. A duplicate key means the terminal Save already wrote the record
// (the Create runs in the background); that converges to the newer state, so
// it is ignored rather than reported.
func (s *MongoStore) Create(ctx context.Context, r Run) error {
	if r.ID == "" {
		return NewStoreError("run create needs an id", false)
	}
	r = r.Capped()
	_, err := s.runs.InsertOne(ctx, r)
	if err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			return nil
		}
		return NewStoreError("run create failed: "+err.Error(), true)
	}
	return nil
}

// Get returns one run; a missing document is NotFound, a transport failure is
// Upstream (so an outage is not reported as a 404).
func (s *MongoStore) Get(ctx context.Context, id string) (Run, error) {
	var r Run
	if err := s.runs.FindOne(ctx, bson.M{"_id": id}).Decode(&r); err != nil {
		if errors.Is(err, mongoDrv.ErrNoDocuments) {
			return Run{}, &StoreError{Msg: "unknown run " + id, NotFound: true}
		}
		return Run{}, NewStoreError("run read failed: "+err.Error(), true)
	}
	return r.Clone(), nil
}

// Save replaces a run's persisted state.
//
// Upsert, not a plain replace: the TTL index could in principle have removed a
// record whose ExpiresAt was never refreshed (a clock jump, a run that outlived
// its TTL because persistence was failing). Losing the record mid-run is worse
// than recreating it.
func (s *MongoStore) Save(ctx context.Context, r Run) error {
	if r.ID == "" {
		return NewStoreError("run save needs an id", false)
	}
	r = r.Capped()
	_, err := s.runs.ReplaceOne(ctx, bson.M{"_id": r.ID}, r, options.Replace().SetUpsert(true))
	if err != nil {
		return NewStoreError("run save failed: "+err.Error(), true)
	}
	return nil
}

// List returns matching runs, newest first, capped.
func (s *MongoStore) List(ctx context.Context, f Filter) ([]Run, error) {
	limit := f.Limit
	if limit <= 0 || limit > ListCap {
		limit = ListCap
	}
	filter := bson.M{}
	if f.Profile != "" {
		filter["profile"] = f.Profile
	}
	if f.ConversationID != "" {
		filter["conversationId"] = f.ConversationID
	}
	if f.Status != "" {
		filter["status"] = string(f.Status)
	}
	cur, err := s.runs.Find(ctx, filter,
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, NewStoreError("run list failed: "+err.Error(), true)
	}
	defer cur.Close(ctx)
	out := make([]Run, 0)
	// A corrupt or schema-drifted row is logged and skipped, never fatal to
	// the whole list — but never silent either, or the list would quietly
	// omit a run the detail route errors on.
	skipped := 0
	for cur.Next(ctx) {
		var r Run
		if err := cur.Decode(&r); err != nil {
			skipped++
			continue
		}
		out = append(out, r.Clone())
	}
	if skipped > 0 {
		s.logf("run history: skipped %d undecodable row(s) in agent_runs", skipped)
	}
	if err := cur.Err(); err != nil {
		return nil, NewStoreError("run list failed: "+err.Error(), true)
	}
	return out, nil
}

// MarkInterrupted flips every still-running record at startup.
//
// Execution cannot survive the process, so this is the honest correction: the
// output produced before the restart stays readable, and the status stops
// claiming work is in flight when only a row remains.
func (s *MongoStore) MarkInterrupted(ctx context.Context, at time.Time) (int64, error) {
	// Expired rows are deleted outright, matching the memory backend: leaving
	// them for the TTL thread would serve a stale `running` row for up to a
	// minute after the restart that memory 404s on. A transient delete
	// failure must not skip the flip below — log and continue, so the worst
	// case is the old TTL window, not every in-flight row lying running.
	if _, err := s.runs.DeleteMany(ctx, bson.M{
		"status":    string(StatusRunning),
		"expiresAt": bson.M{"$lte": at},
	}); err != nil {
		s.logf("run history: ERROR could not delete expired runs at startup: %v", err)
	}
	// Unexpired rows flip; rows without an expiry (pre-TTL documents) flip too.
	res, err := s.runs.UpdateMany(ctx,
		bson.M{
			"status": string(StatusRunning),
			"$or": []bson.M{
				{"expiresAt": bson.M{"$gt": at}},
				{"expiresAt": bson.M{"$exists": false}},
			},
		},
		bson.M{"$set": bson.M{
			"status":     string(StatusInterrupted),
			"error":      InterruptedError,
			"updatedAt":  at.UnixMilli(),
			"finishedAt": at.UnixMilli(),
			"expiresAt":  at.Add(DefaultTTL),
		}})
	if err != nil {
		return 0, NewStoreError("mark interrupted: "+err.Error(), true)
	}
	return res.ModifiedCount, nil
}
