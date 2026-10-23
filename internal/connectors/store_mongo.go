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

// EnsureIndexes creates the status index List never strictly needs at three
// rows but keeps the read path covered if the set ever grows beyond v1.
func (s *MongoStore) EnsureIndexes(ctx context.Context) error {
	_, err := s.coll.Indexes().CreateOne(ctx, mongoDrv.IndexModel{
		Keys:    bson.D{{Key: "status", Value: 1}},
		Options: options.Index().SetName("connectors_status"),
	})
	return err
}

// Token columns live in this collection alongside the display fields but
// are never projected into API reads (see displayProjection). The field
// names are defined once here so SetStatus/Disconnect and the future OAuth
// exchange cannot drift apart.
const (
	refreshTokenField = "refreshToken"
	accessTokenField  = "accessToken"
)

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
		s.logf("connectors: list failed: %v", err)
		return nil, NewStoreError("connector list failed", true)
	}
	defer cur.Close(ctx)
	byID := make(map[string]Connector, 3)
	for cur.Next(ctx) {
		var c Connector
		if err := cur.Decode(&c); err != nil {
			s.logf("connectors: list decode failed: %v", err)
			return nil, NewStoreError("connector list failed", true)
		}
		byID[c.ID] = c
	}
	if err := cur.Err(); err != nil {
		s.logf("connectors: list cursor failed: %v", err)
		return nil, NewStoreError("connector list failed", true)
	}
	out := make([]Connector, 0, len(listIDs))
	for _, id := range listIDs {
		if c, ok := byID[id]; ok {
			c = backfillDisplay(c, id)
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
		s.logf("connectors: read %s failed: %v", id, err)
		return Connector{}, NewStoreError("connector read failed", true)
	}
	return backfillDisplay(c, id), nil
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
	// Disconnect clears lastError in both backends; SetStatus matches it on
	// the disconnected path so the two routes to the same state agree.
	lastErr := strings.TrimSpace(lastError)
	if st == StatusDisconnected {
		lastErr = ""
	}
	// One write, never two: when marking disconnected the token $unset rides
	// in the same UpdateOne as the status $set (mirroring Disconnect). A
	// split $set-then-$unset could land the first and lose the second (Atlas
	// hiccup, deadline) and leave status: disconnected with tokens present —
	// the reported state and the safe state diverging in exactly the failure
	// case that matters. Disconnect stays the documented off-path; this is
	// the belt-and-braces for any caller that marks disconnected via
	// SetStatus.
	_, err := s.coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}},
		statusUpdateDoc(id, st, lastErr, now), options.Update().SetUpsert(true))
	if err != nil {
		s.logf("connectors: write %s failed: %v", id, err)
		return Connector{}, NewStoreError("connector write failed", true)
	}
	return s.readBack(id)
}

func (s *MongoStore) Disconnect(ctx context.Context, id string) (Connector, error) {
	id = NormalizedID(id)
	if !Known(id) {
		return Connector{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	now := time.Now().UnixMilli()
	_, err := s.coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}},
		disconnectUpdateDoc(id, now), options.Update().SetUpsert(true))
	if err != nil {
		s.logf("connectors: disconnect %s failed: %v", id, err)
		return Connector{}, NewStoreError("connector disconnect failed", true)
	}
	s.logf("connectors: %s disconnected (tokens cleared)", id)
	return s.readBack(id)
}

// statusUpdateDoc builds the single UpdateOne body for SetStatus: $set with
// the display fields plus status, $unset of the token columns on the
// disconnected path. One document, one write — a future edit must not split
// it again (see the atomicity note on SetStatus). lastErr must already be
// normalised (cleared on the disconnected path).
func statusUpdateDoc(id string, st Status, lastErr string, now int64) bson.D {
	set := bson.D{
		{Key: "displayName", Value: DisplayName(id)},
		{Key: "scopes", Value: ScopesFor(id)},
		{Key: "status", Value: string(st)},
		{Key: "lastError", Value: lastErr},
		{Key: "updatedAt", Value: now},
	}
	if st == StatusConnected {
		set = append(set, bson.E{Key: "connectedAt", Value: now})
	}
	update := bson.D{{Key: "$set", Value: set}}
	if st == StatusDisconnected {
		update = append(update, bson.E{Key: "$unset", Value: bson.D{
			{Key: refreshTokenField, Value: ""},
			{Key: accessTokenField, Value: ""},
		}})
	}
	return update
}

// disconnectUpdateDoc builds the single UpdateOne body for Disconnect. Same
// atomicity contract as statusUpdateDoc: tokens die in the same write that
// flips the status. History (chat, saved files) is untouched by design.
func disconnectUpdateDoc(id string, now int64) bson.D {
	return bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "displayName", Value: DisplayName(id)},
			{Key: "scopes", Value: ScopesFor(id)},
			{Key: "status", Value: string(StatusDisconnected)},
			{Key: "lastError", Value: ""},
			{Key: "updatedAt", Value: now},
		}},
		{Key: "$unset", Value: bson.D{
			{Key: refreshTokenField, Value: ""},
			{Key: accessTokenField, Value: ""},
		}},
	}
}

// backfillDisplay repairs a present-but-thin document the way List's
// DefaultRow synthesis covers absent ones: scopes, display name, and — for
// a corrupt/hand-edited doc with a missing status — the status itself,
// which otherwise decodes to "" and would emit a fourth state the app never
// expects and the reconnect hint never matches. Fail-closed to disconnected.
func backfillDisplay(c Connector, id string) Connector {
	if len(c.Scopes) == 0 {
		c.Scopes = ScopesFor(id)
	}
	if c.DisplayName == "" {
		c.DisplayName = DisplayName(id)
	}
	if !c.Status.Valid() {
		c.Status = StatusDisconnected
	}
	return c
}

// readBack re-reads one row on a fresh budget. The gateway hands one 5s ctx
// for the whole verb; a slow Atlas write could consume it and turn a landed
// write into a 502 on the confirming read. The read is cheap and idempotent,
// so it gets its own 5s rather than the write's leftovers.
//
// Deliberately detached from the request: a client disconnect mid-verb still
// pays for one cheap idempotent read. Correct at three rows; do not copy
// this pattern for non-idempotent work (the OAuth follow-up's heavier
// writes must stay on the request ctx).
func (s *MongoStore) readBack(id string) (Connector, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.Get(ctx, id)
}
