package connectors

import (
	"context"
	"sort"
	"time"

	"agento/internal/approvals"
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

// EnsureIndexes is a no-op: three fixed rows keyed by _id need no secondary
// index. List scans all three and Get/writes key on _id (already indexed),
// so a status index would be dead weight that merely looks load-bearing.
// Kept as an explicit step so main's startup sequence matches the sibling
// stores, and so the OAuth follow-up has a place to add the index its
// queries actually need.
func (s *MongoStore) EnsureIndexes(_ context.Context) error { return nil }

// Token columns live in this collection alongside the display fields but
// are never projected into API reads (see displayProjection). The field
// names are defined once here so SetStatus/Disconnect and the future OAuth
// exchange cannot drift apart.
const (
	refreshTokenField = "refreshToken"
	accessTokenField  = "accessToken"
)

// displayProjection returns the display-only read shape: every API-visible
// field, never the token columns. A fresh copy per call (same class as
// ListIDs): the token-exclusion invariant TestDisplayProjectionExcludesTokens
// pins would otherwise rest on no in-package edit ever appending a field.
func displayProjection() bson.D {
	return bson.D{
		{Key: "_id", Value: 1},
		{Key: "displayName", Value: 1},
		{Key: "scopes", Value: 1},
		{Key: "status", Value: 1},
		{Key: "lastError", Value: 1},
		{Key: "connectedAt", Value: 1},
		{Key: "updatedAt", Value: 1},
	}
}

func findOpts() *options.FindOptions {
	return options.Find().SetProjection(displayProjection())
}

func findOneOpts() *options.FindOneOptions {
	return options.FindOne().SetProjection(displayProjection())
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
	var ignored []string
	for id := range byID {
		if !Known(id) {
			ignored = append(ignored, id)
		}
	}
	// Fixed-set v1: stray docs (typo'd id from a future OAuth bug,
	// hand-edit) are excluded from the shape, but must not vanish silently
	// — triage would otherwise see "3 disconnected rows" while Mongo holds
	// 4 docs. Sorted so two log lines for the same stray id diff clean.
	sort.Strings(ignored)
	if len(ignored) > 0 {
		s.logf("connectors: ignoring %d unknown row(s): %q", len(ignored), ignored)
	}
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
	// Only the expired path carries error text: connected (healthy) and
	// disconnected (off) rows carry none, mirroring each other, so a stale
	// or empty error can never render as failure text on a healthy row.
	// Scrubbed (approvals.ScrubError): lastError is app-visible and the
	// OAuth follow-up will store provider errors here.
	lastErr := ""
	if st == StatusExpired {
		lastErr = approvals.ScrubError(lastError)
	}
	// ConnectedAt is first-ever connect, not last-write and not
	// last-reconnect: re-marking an already connected row (e.g. a future
	// idempotent token-refresh write) must not move the timestamp, and a
	// disconnect→reconnect cycle keeps the original stamp (Disconnect
	// retains it, both backends). Read-then-decide on this path only — 3
	// rows, and the alternative (pipeline conditional) buys nothing at this
	// size. Single-writer assumption: no two SetStatus(connected) run
	// concurrently in v1 (gateway verbs only; the OAuth follow-up must keep
	// token writes on one serialised path, not copy this read for
	// non-idempotent work).
	connectedAt := now
	if st == StatusConnected {
		cur, gerr := s.Get(ctx, id)
		if gerr != nil {
			// Fail the write, don't guess the stamp: falling through with
			// now would silently reset first-ever-connect on a transient
			// read failure while reporting success. The caller retries; the
			// stamp survives.
			s.logf("connectors: pre-read %s failed: %v", id, gerr)
			return Connector{}, NewStoreError("connector read failed", true)
		}
		if cur.ConnectedAt != 0 {
			connectedAt = cur.ConnectedAt
		}
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
		statusUpdateDoc(id, st, lastErr, now, connectedAt), options.Update().SetUpsert(true))
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
	// Log only on transition: the upsert is idempotent by design, so a
	// repeat disconnect would otherwise read as "tokens cleared" when
	// nothing changed. Best-effort pre-read; on read failure the write
	// still lands but the line says the pre-read was unavailable rather
	// than claiming a transition that may not have happened (fail-loud
	// over fail-silent, honestly labeled).
	transition := true
	preReadOK := true
	if cur, gerr := s.Get(ctx, id); gerr == nil {
		transition = cur.Status != StatusDisconnected
	} else {
		preReadOK = false
	}
	_, err := s.coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}},
		disconnectUpdateDoc(id, now), options.Update().SetUpsert(true))
	if err != nil {
		s.logf("connectors: disconnect %s failed: %v", id, err)
		return Connector{}, NewStoreError("connector disconnect failed", true)
	}
	if transition {
		if preReadOK {
			s.logf("connectors: %s disconnected (tokens cleared)", id)
		} else {
			s.logf("connectors: %s disconnected (tokens cleared, pre-read unavailable)", id)
		}
	}
	return s.readBack(id)
}

// statusUpdateDoc builds the single UpdateOne body for SetStatus: $set with
// the display fields plus status, $unset of the token columns on the
// disconnected path. One document, one write — a future edit must not split
// it again (see the atomicity note on SetStatus). lastErr must already be
// normalised (only the expired path carries text); connectedAt must already
// be resolved (first-connect preserved by the caller).
func statusUpdateDoc(id string, st Status, lastErr string, now, connectedAt int64) bson.D {
	set := bson.D{
		{Key: "displayName", Value: DisplayName(id)},
		{Key: "scopes", Value: ScopesFor(id)},
		{Key: "status", Value: string(st)},
		{Key: "lastError", Value: lastErr},
		{Key: "updatedAt", Value: now},
	}
	if st == StatusConnected {
		set = append(set, bson.E{Key: "connectedAt", Value: connectedAt})
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
//
// LastError is scrubbed here too, not only on the SetStatus write path: a
// corrupt/hand-edited doc with a token in lastError would otherwise emit it
// verbatim on the next read — the exact leak the write scrub exists to
// prevent. ScrubError is idempotent on already-scrubbed text.
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
	c.LastError = approvals.ScrubError(c.LastError)
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
