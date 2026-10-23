package connectors

import (
	"context"
	"strings"
	"time"

	"agento/internal/mongostore"

	"go.mongodb.org/mongo-driver/bson"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoStore is the durable backend. Refresh tokens live server-side in the
// same collection (refreshToken, encrypted at rest by the deployment — see
// the ADR), in columns no read path selects: List/Get project only the
// display shape, so a token can never reach the app even if one is stored.
//
// Disconnect clears the token columns. Already-fetched data (chat history,
// saved files) is NOT wiped: disconnect stops future access, it does not
// rewrite the past.
type MongoStore struct {
	coll *mongoDrv.Collection
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
	return &MongoStore{coll: db.Collection(Collection)}, nil
}

// FromEnv connects from the shared MONGODB_URI/MONGODB_DB pair.
func FromEnv() (*MongoStore, error) {
	uri, db := mongostore.Env()
	return New(uri, db)
}

// EnsureIndexes creates the id lookup (implicit via _id; kept as an explicit
// step so main's startup sequence matches the sibling stores).
func (s *MongoStore) EnsureIndexes(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateOne(ctx, mongoDrv.IndexModel{
		Keys:    bson.D{{Key: "status", Value: 1}},
		Options: options.Index().SetName("connectors_status"),
	})
	return err
}

// rowIn is the stored shape: Connector plus server-side token columns that
// never leave the process.
type rowIn struct {
	Connector `bson:",inline"`
	// RefreshToken holds the OAuth refresh token server-side. Never
	// selected into API reads (see projection below) and never logged.
	RefreshToken string `bson:"refreshToken,omitempty"`
	// AccessToken is short-lived and kept only to avoid re-exchanging on
	// every turn; same secrecy as the refresh token.
	AccessToken string `bson:"accessToken,omitempty"`
}

var displayProjection = bson.D{
	{Key: "_id", Value: 1},
	{Key: "displayName", Value: 1},
	{Key: "scopes", Value: 1},
	{Key: "status", Value: 1},
	{Key: "lastError", Value: 1},
	{Key: "connectedAt", Value: 1},
	{Key: "updatedAt", Value: 1},
}

func findOpts() *options.FindOptions {
	return options.Find().SetProjection(displayProjection)
}

func findOneOpts() *options.FindOneOptions {
	return options.FindOne().SetProjection(displayProjection)
}

func (s *MongoStore) List(ctx context.Context) ([]Connector, error) {
	cur, err := s.coll.Find(ctx, bson.D{}, findOpts())
	if err != nil {
		return nil, NewStoreError("connector list failed", true)
	}
	defer cur.Close(ctx)
	byID := make(map[string]Connector, 3)
	for cur.Next(ctx) {
		var c Connector
		if err := cur.Decode(&c); err != nil {
			return nil, NewStoreError("connector list failed", true)
		}
		byID[c.ID] = c
	}
	if err := cur.Err(); err != nil {
		return nil, NewStoreError("connector list failed", true)
	}
	out := make([]Connector, 0, len(ListIDs))
	for _, id := range ListIDs {
		if c, ok := byID[id]; ok {
			if len(c.Scopes) == 0 {
				c.Scopes = ScopesFor(id)
			}
			if c.DisplayName == "" {
				c.DisplayName = DisplayName(id)
			}
			out = append(out, c)
		} else {
			out = append(out, DefaultRow(id))
		}
	}
	return out, nil
}

func (s *MongoStore) Get(ctx context.Context, id string) (Connector, error) {
	id = NormalizedID(id)
	if !Known(id) {
		return Connector{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	var c Connector
	if err := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}}, findOneOpts()).Decode(&c); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return DefaultRow(id), nil
		}
		return Connector{}, NewStoreError("connector read failed", true)
	}
	if len(c.Scopes) == 0 {
		c.Scopes = ScopesFor(id)
	}
	if c.DisplayName == "" {
		c.DisplayName = DisplayName(id)
	}
	return c, nil
}

func (s *MongoStore) SetStatus(ctx context.Context, id string, st Status, lastError string) (Connector, error) {
	id = NormalizedID(id)
	if !Known(id) {
		return Connector{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	if !st.Valid() {
		return Connector{}, NewStoreError("unknown connector status", false)
	}
	now := time.Now().UnixMilli()
	set := bson.D{
		{Key: "displayName", Value: DisplayName(id)},
		{Key: "scopes", Value: ScopesFor(id)},
		{Key: "status", Value: string(st)},
		{Key: "lastError", Value: strings.TrimSpace(lastError)},
		{Key: "updatedAt", Value: now},
	}
	if st == StatusConnected {
		set = append(set, bson.E{Key: "connectedAt", Value: now})
	}
	_, err := s.coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}},
		bson.D{{Key: "$set", Value: set}}, options.Update().SetUpsert(true))
	if err != nil {
		return Connector{}, NewStoreError("connector write failed", true)
	}
	return s.Get(ctx, id)
}

func (s *MongoStore) Disconnect(ctx context.Context, id string) (Connector, error) {
	id = NormalizedID(id)
	if !Known(id) {
		return Connector{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	now := time.Now().UnixMilli()
	_, err := s.coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "displayName", Value: DisplayName(id)},
			{Key: "scopes", Value: ScopesFor(id)},
			{Key: "status", Value: string(StatusDisconnected)},
			{Key: "lastError", Value: ""},
			{Key: "updatedAt", Value: now},
		}},
		// Tokens die here: future access stops at the next read. History
		// (chat, saved files) is untouched by design.
		{Key: "$unset", Value: bson.D{
			{Key: "refreshToken", Value: ""},
			{Key: "accessToken", Value: ""},
		}},
	}, options.Update().SetUpsert(true))
	if err != nil {
		return Connector{}, NewStoreError("connector disconnect failed", true)
	}
	s.logf("connectors: %s disconnected (tokens cleared)", id)
	return s.Get(ctx, id)
}
