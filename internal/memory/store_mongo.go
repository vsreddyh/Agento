package memory

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"agento/internal/mongostore"

	"go.mongodb.org/mongo-driver/bson"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoStore is the MongoDB backend for items and paused topics.
type MongoStore struct {
	items    *mongoDrv.Collection
	settings *mongoDrv.Collection
	// Logf receives operational notes. Nil discards them.
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
	return &MongoStore{
		items:    db.Collection(ItemsCollection),
		settings: db.Collection(SettingsCollection),
	}, nil
}

// FromEnv connects from the shared MONGODB_URI/MONGODB_DB pair.
func FromEnv() (*MongoStore, error) {
	uri, db := mongostore.Env()
	return New(uri, db)
}

// EnsureIndexes creates the profile listing index and the settings lookup.
// Memory has no TTL: an item lives until it is deleted or forgotten.
// The compound serves the List sort; topic/status filters scan the profile's
// rows, which is fine at ListCap=200 — if the cap ever grows, extend this
// index before the per-turn/per-load reads.
func (s *MongoStore) EnsureIndexes(ctx context.Context) error {
	_, err := s.items.Indexes().CreateOne(ctx, mongoDrv.IndexModel{
		Keys: bson.D{
			{Key: "profile", Value: 1},
			{Key: "createdAt", Value: -1},
		},
		Options: options.Index().SetName("memory_profile_created"),
	})
	return err
}

// Create inserts one item. Empty timestamps are filled here so every path
// (app, agent tool) gets the same clock, and text is cleaned and empty text
// rejected here for the same reason as the memory store: the invariant lives
// in the store, not in each caller.
func (s *MongoStore) Create(ctx context.Context, i Item) (Item, error) {
	if i.ID == "" {
		i.ID = NewID()
	}
	// Trimmed on write; see MemoryStore.Create.
	i.Profile = strings.TrimSpace(i.Profile)
	if i.Profile == "" {
		return Item{}, NewStoreError("memory item needs a profile", false)
	}
	i.Text = CleanText(i.Text)
	if i.Text == "" {
		return Item{}, NewStoreError("memory item needs text", false)
	}
	// The 200-item cap is enforced, not just read: beyond it the oldest rows
	// are invisible to both the screen and the prompt, so the 201st create
	// fails instead of writing an unreachable row.
	if n, err := s.items.CountDocuments(ctx, bson.M{"profile": i.Profile}); err != nil {
		return Item{}, NewStoreError("memory count failed: "+err.Error(), true)
	} else if n >= ListCap {
		return Item{}, NewStoreError("memory is full for this profile: delete or forget items first", false)
	}
	now := time.Now().UnixMilli()
	if i.CreatedAt == 0 {
		i.CreatedAt = now
	}
	i.UpdatedAt = now
	i.Topic = NormalizeTopic(i.Topic)
	if !i.Status.Valid() {
		i.Status = StatusPending
	}
	if i.Source == "" {
		i.Source = SourceUser
	}
	if _, err := s.items.InsertOne(ctx, i); err != nil {
		return Item{}, NewStoreError("memory create failed: "+err.Error(), true)
	}
	return i, nil
}

// Get returns one item. Profile-scoped like the writes.
func (s *MongoStore) Get(ctx context.Context, id, profile string) (Item, error) {
	profile = strings.TrimSpace(profile)
	var it Item
	if err := s.items.FindOne(ctx, bson.M{"_id": id, "profile": profile}).Decode(&it); err != nil {
		if errors.Is(err, mongoDrv.ErrNoDocuments) {
			return Item{}, &StoreError{Msg: "unknown memory " + id, NotFound: true}
		}
		return Item{}, NewStoreError("memory read failed: "+err.Error(), true)
	}
	return it, nil
}

// Update applies a partial edit and returns the stored row. The filter
// carries the profile: one query authorises and writes, and a foreign id
// reads as NotFound instead of costing a Get-then-write pair.
func (s *MongoStore) Update(ctx context.Context, id, profile string, u Update) (Item, error) {
	profile = strings.TrimSpace(profile)
	if u.Text == nil && u.Topic == nil && u.Status == nil {
		return Item{}, NewStoreError("memory update needs text, topic or status", false)
	}
	set := bson.M{"updatedAt": time.Now().UnixMilli()}
	if u.Text != nil {
		// Same invariant as Create; see MemoryStore.Update.
		text := CleanText(*u.Text)
		if text == "" {
			return Item{}, NewStoreError("memory item needs text", false)
		}
		set["text"] = text
	}
	if u.Topic != nil {
		set["topic"] = NormalizeTopic(*u.Topic)
	}
	if u.Status != nil {
		if !u.Status.Valid() {
			return Item{}, NewStoreError("unknown memory status", false)
		}
		set["status"] = *u.Status
	}
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var it Item
	err := s.items.FindOneAndUpdate(ctx, bson.M{"_id": id, "profile": profile}, bson.M{"$set": set}, opts).Decode(&it)
	if errors.Is(err, mongoDrv.ErrNoDocuments) {
		return Item{}, &StoreError{Msg: "unknown memory " + id, NotFound: true}
	}
	if err != nil {
		return Item{}, NewStoreError("memory update failed: "+err.Error(), true)
	}
	return it, nil
}

// Delete removes one item. Deleting an already-deleted id is NotFound: the
// app should reload rather than believe a second delete did something.
// Profile-scoped like Update.
func (s *MongoStore) Delete(ctx context.Context, id, profile string) error {
	profile = strings.TrimSpace(profile)
	res, err := s.items.DeleteOne(ctx, bson.M{"_id": id, "profile": profile})
	if err != nil {
		return NewStoreError("memory delete failed: "+err.Error(), true)
	}
	if res.DeletedCount == 0 {
		return &StoreError{Msg: "unknown memory " + id, NotFound: true}
	}
	return nil
}

// DeleteAll forgets every item for one profile. Paused topics survive: they
// are the user's learning policy, not something they remembered.
func (s *MongoStore) DeleteAll(ctx context.Context, profile string) (int64, error) {
	profile = strings.TrimSpace(profile)
	res, err := s.items.DeleteMany(ctx, bson.M{"profile": profile})
	if err != nil {
		return 0, NewStoreError("memory forget failed: "+err.Error(), true)
	}
	return res.DeletedCount, nil
}

// List returns matching items, newest-created first (stable order; see
// MemoryStore.List).
func (s *MongoStore) List(ctx context.Context, f Filter) ([]Item, error) {
	// An unknown status is a caller bug, not an empty list: both writers
	// validate first, but the store is the backstop, like the text invariant.
	if f.Status != "" && !f.Status.Valid() {
		return nil, NewStoreError("unknown memory status", false)
	}
	// 0 is the Go zero value for "unspecified", not "no rows".
	q := bson.M{}
	if p := strings.TrimSpace(f.Profile); p != "" {
		q["profile"] = p
	}
	if t := strings.TrimSpace(f.Topic); t != "" {
		q["topic"] = NormalizeTopic(t)
	}
	if f.Status != "" {
		q["status"] = f.Status
	}
	// 0 is the Go zero value for "unspecified", not "no rows".
	limit := f.Limit
	if limit <= 0 || limit > ListCap {
		limit = ListCap
	}
	// createdAt desc, _id desc: the memory backend breaks same-millisecond
	// ties the same way, so Atlas and dev return one ordering contract.
	cur, err := s.items.Find(ctx, q,
		options.Find().
			SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}}).
			SetLimit(int64(limit)))
	if err != nil {
		return nil, NewStoreError("memory list failed: "+err.Error(), true)
	}
	defer cur.Close(ctx)
	items := make([]Item, 0, 16)
	if err := cur.All(ctx, &items); err != nil {
		return nil, NewStoreError("memory list failed: "+err.Error(), true)
	}
	return items, nil
}

// SetTopicPaused pauses or resumes one topic for one profile.
func (s *MongoStore) SetTopicPaused(ctx context.Context, profile, topic string, paused bool) error {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return NewStoreError("memory topic needs a profile", false)
	}
	t := NormalizeTopic(topic)
	var update bson.M
	if paused {
		update = bson.M{"$addToSet": bson.M{"paused": t}}
	} else {
		update = bson.M{"$pull": bson.M{"paused": t}}
	}
	_, err := s.settings.UpdateOne(ctx, bson.M{"_id": profile}, update,
		options.Update().SetUpsert(true))
	if err != nil {
		return NewStoreError("memory topic update failed: "+err.Error(), true)
	}
	return nil
}

// PausedTopics returns the paused topics for a profile, sorted: the memory
// backend sorts too, so the screen sees one deterministic order per backend.
func (s *MongoStore) PausedTopics(ctx context.Context, profile string) ([]string, error) {
	profile = strings.TrimSpace(profile)
	var doc struct {
		Paused []string `bson:"paused"`
	}
	if err := s.settings.FindOne(ctx, bson.M{"_id": profile}).Decode(&doc); err != nil {
		if errors.Is(err, mongoDrv.ErrNoDocuments) {
			return []string{}, nil
		}
		return nil, NewStoreError("memory topic read failed: "+err.Error(), true)
	}
	if doc.Paused == nil {
		return []string{}, nil
	}
	sort.Strings(doc.Paused)
	return doc.Paused, nil
}
