package teach

import (
	"context"
	"errors"
	"strings"
	"time"

	"agento/internal/mongostore"

	"go.mongodb.org/mongo-driver/bson"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoStore is the MongoDB backend for taught skills.
type MongoStore struct {
	skills *mongoDrv.Collection
	// now is the clock every write boundary reads, mirroring the other
	// stores. Nil means time.Now.
	now func() time.Time
	// Logf receives operational notes. Nil discards them.
	Logf func(string, ...any)
}

func (s *MongoStore) clock() time.Time {
	if s.now == nil {
		return time.Now()
	}
	return s.now()
}

// New connects to uri/dbName.
func New(uri, dbName string) (*MongoStore, error) {
	_, db, err := mongostore.Open(uri, dbName)
	if err != nil {
		return nil, err
	}
	return &MongoStore{
		skills: db.Collection(SkillsCollection),
		now:    time.Now,
	}, nil
}

// FromEnv connects from the shared MONGODB_URI/MONGODB_DB pair.
func FromEnv() (*MongoStore, error) {
	uri, db := mongostore.Env()
	return New(uri, db)
}

// EnsureIndexes creates the profile listing index and the per-profile
// name uniqueness the memory backend checks by scan. A name collision is a
// caller bug (400), never an outage.
func (s *MongoStore) EnsureIndexes(ctx context.Context) error {
	if _, err := s.skills.Indexes().CreateOne(ctx, mongoDrv.IndexModel{
		Keys: bson.D{
			{Key: "profile", Value: 1},
			{Key: "createdAt", Value: -1},
		},
		Options: options.Index().SetName("teach_profile_created"),
	}); err != nil {
		return err
	}
	_, err := s.skills.Indexes().CreateOne(ctx, mongoDrv.IndexModel{
		Keys: bson.D{
			{Key: "profile", Value: 1},
			{Key: "name", Value: 1},
		},
		Options: options.Index().SetName("teach_profile_name").SetUnique(true),
	})
	return err
}

// Create stores one draft. Same invariants as the memory backend: drafts are
// born drafts, names are unique per profile, text is cleaned.
func (s *MongoStore) Create(ctx context.Context, in Skill) (Skill, error) {
	in.Profile = strings.TrimSpace(in.Profile)
	if in.Profile == "" {
		return Skill{}, NewStoreError("skill needs a profile", false)
	}
	in.Name = strings.TrimSpace(in.Name)
	if !ValidName(in.Name) {
		return Skill{}, NewStoreError("skill name must be lowercase letters, digits and hyphens", false)
	}
	in.Description = CleanDescription(in.Description)
	if in.Description == "" {
		return Skill{}, NewStoreError("skill needs a description", false)
	}
	in.Body = CleanBody(in.Body)
	if in.Body == "" {
		return Skill{}, NewStoreError("skill needs a body", false)
	}
	in.Source = clipRunes(strings.TrimSpace(in.Source), MaxSource)
	if in.ID == "" {
		in.ID = NewID()
	} else if !ValidID(in.ID) {
		return Skill{}, NewStoreError("skill id must be 1-64 letters, digits, _ or -", false)
	}
	now := s.clock().UnixMilli()
	if in.CreatedAt == 0 || in.CreatedAt > now {
		in.CreatedAt = now
	}
	in.UpdatedAt = now
	in.Status = StatusDraft
	in.TestedAt = 0
	in.TestText = ""
	if _, err := s.skills.InsertOne(ctx, in); err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			// The _id and the {profile,name} unique index share this
			// shape: either way the caller named something taken.
			return Skill{}, NewStoreError("a skill with that id or name already exists", false)
		}
		return Skill{}, NewStoreError("skill create failed: "+err.Error(), true)
	}
	return in, nil
}

// Get returns one skill. Profile-scoped: a foreign id is NotFound.
func (s *MongoStore) Get(ctx context.Context, id, profile string) (Skill, error) {
	profile = strings.TrimSpace(profile)
	var it Skill
	if err := s.skills.FindOne(ctx, bson.M{"_id": id, "profile": profile}).Decode(&it); err != nil {
		if errors.Is(err, mongoDrv.ErrNoDocuments) {
			return Skill{}, &StoreError{Msg: "unknown skill " + id, NotFound: true}
		}
		return Skill{}, NewStoreError("skill read failed: "+err.Error(), true)
	}
	return it, nil
}

// Update applies a partial edit.
func (s *MongoStore) Update(ctx context.Context, id, profile string, u Update) (Skill, error) {
	profile = strings.TrimSpace(profile)
	if u.Name == nil && u.Description == nil && u.Body == nil {
		return Skill{}, NewStoreError("skill update needs name, description or body", false)
	}
	set := bson.M{"updatedAt": s.clock().UnixMilli()}
	if u.Name != nil {
		name := strings.TrimSpace(*u.Name)
		if !ValidName(name) {
			return Skill{}, NewStoreError("skill name must be lowercase letters, digits and hyphens", false)
		}
		set["name"] = name
	}
	if u.Description != nil {
		d := CleanDescription(*u.Description)
		if d == "" {
			return Skill{}, NewStoreError("skill needs a description", false)
		}
		set["description"] = d
	}
	if u.Body != nil {
		b := CleanBody(*u.Body)
		if b == "" {
			return Skill{}, NewStoreError("skill needs a body", false)
		}
		set["body"] = b
	}
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var it Skill
	err := s.skills.FindOneAndUpdate(ctx,
		bson.M{"_id": id, "profile": profile},
		bson.M{"$set": set}, opts).Decode(&it)
	if errors.Is(err, mongoDrv.ErrNoDocuments) {
		return Skill{}, &StoreError{Msg: "unknown skill " + id, NotFound: true}
	}
	if err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			return Skill{}, NewStoreError("a skill with that name already exists", false)
		}
		return Skill{}, NewStoreError("skill update failed: "+err.Error(), true)
	}
	return it, nil
}

// Delete removes one skill.
func (s *MongoStore) Delete(ctx context.Context, id, profile string) error {
	profile = strings.TrimSpace(profile)
	res, err := s.skills.DeleteOne(ctx, bson.M{"_id": id, "profile": profile})
	if err != nil {
		return NewStoreError("skill delete failed: "+err.Error(), true)
	}
	if res.DeletedCount == 0 {
		return &StoreError{Msg: "unknown skill " + id, NotFound: true}
	}
	return nil
}

// List returns one profile's skills, newest first.
func (s *MongoStore) List(ctx context.Context, profile string) ([]Skill, error) {
	profile = strings.TrimSpace(profile)
	if profile == "" {
		return nil, NewStoreError("skill list needs a profile", false)
	}
	cur, err := s.skills.Find(ctx, bson.M{"profile": profile},
		options.Find().
			SetSort(bson.D{{Key: "createdAt", Value: -1}, {Key: "_id", Value: -1}}).
			SetLimit(ListCap))
	if err != nil {
		return nil, NewStoreError("skill list failed: "+err.Error(), true)
	}
	defer cur.Close(ctx)
	out := make([]Skill, 0, 8)
	if err := cur.All(ctx, &out); err != nil {
		return nil, NewStoreError("skill list failed: "+err.Error(), true)
	}
	return out, nil
}

// SetStatus moves one skill along its lifecycle. Only the transition to
// active is gated (testedAt), and the gate reads the row being decided, so
// one operation sees one instant.
func (s *MongoStore) SetStatus(ctx context.Context, id, profile string, st Status) (Skill, error) {
	profile = strings.TrimSpace(profile)
	if !st.Valid() {
		return Skill{}, NewStoreError("unknown skill status", false)
	}
	if st != StatusActive {
		opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
		var it Skill
		err := s.skills.FindOneAndUpdate(ctx,
			bson.M{"_id": id, "profile": profile},
			bson.M{"$set": bson.M{"status": st, "updatedAt": s.clock().UnixMilli()}},
			opts).Decode(&it)
		if errors.Is(err, mongoDrv.ErrNoDocuments) {
			return Skill{}, &StoreError{Msg: "unknown skill " + id, NotFound: true}
		}
		if err != nil {
			return Skill{}, NewStoreError("skill status failed: "+err.Error(), true)
		}
		return it, nil
	}
	// Activate is one atomic update: the filter itself requires a recorded
	// test, so no concurrent edit/test can interleave between a read and a
	// write. A miss is either unknown or untested; the probe tells which so
	// the error stays honest.
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var it Skill
	err := s.skills.FindOneAndUpdate(ctx,
		bson.M{"_id": id, "profile": profile, "testedAt": bson.M{"$ne": 0}},
		bson.M{"$set": bson.M{"status": st, "updatedAt": s.clock().UnixMilli()}},
		opts).Decode(&it)
	if errors.Is(err, mongoDrv.ErrNoDocuments) {
		var probe Skill
		if perr := s.skills.FindOne(ctx, bson.M{"_id": id, "profile": profile}).Decode(&probe); perr != nil {
			if errors.Is(perr, mongoDrv.ErrNoDocuments) {
				return Skill{}, &StoreError{Msg: "unknown skill " + id, NotFound: true}
			}
			return Skill{}, NewStoreError("skill read failed: "+perr.Error(), true)
		}
		return Skill{}, NewStoreError("test the skill before activating it", false)
	}
	if err != nil {
		return Skill{}, NewStoreError("skill status failed: "+err.Error(), true)
	}
	return it, nil
}

// RecordTest stores a test transcript and stamps the skill tested.
func (s *MongoStore) RecordTest(ctx context.Context, id, profile string, at int64, text string) (Skill, error) {
	profile = strings.TrimSpace(profile)
	text = clipRunes(strings.TrimSpace(strings.ReplaceAll(text, "\x00", "")), MaxTestText)
	opts := options.FindOneAndUpdate().SetReturnDocument(options.After)
	var it Skill
	err := s.skills.FindOneAndUpdate(ctx,
		bson.M{"_id": id, "profile": profile},
		bson.M{"$set": bson.M{
			"testedAt": at, "testText": text, "updatedAt": s.clock().UnixMilli(),
		}},
		opts).Decode(&it)
	if errors.Is(err, mongoDrv.ErrNoDocuments) {
		return Skill{}, &StoreError{Msg: "unknown skill " + id, NotFound: true}
	}
	if err != nil {
		return Skill{}, NewStoreError("skill test record failed: "+err.Error(), true)
	}
	return it, nil
}
