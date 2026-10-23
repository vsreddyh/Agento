package connectors

import (
	"context"
	"errors"
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

// Note: no EnsureIndexes method exists on purpose. Three fixed rows keyed
// by _id need no secondary index, and an uncalled no-op hook would bit-rot
// silently (signature drift, no callers, no test). The OAuth follow-up adds
// the real index method alongside the first query that needs one.

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
	// Cleanup on its own budget, not the verb's: a slow Atlas read must not
	// make the cursor close eat the same expired deadline.
	defer cur.Close(context.Background())
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
		// Raw, deliberately NOT folded: the storage contract is lowercase
		// _ids (every write normalises), so a case-variant doc ("Gmail")
		// stays stray + logged rather than served. Folding here would make
		// List serve a row Get(FindOne, exact match) cannot see — List
		// saying "connected" while Get says "never touched" for the same
		// logical id. Unfolded, both synthesise DefaultRow and triage gets
		// the log line. (The OAuth follow-up must write lowercase _ids.)
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
	// Three tiers, one probe (status+error+stamp+display). Absent doc always
	// takes the full write. Unexpected probe failures log once, here.
	pctx, pcancel := quickCtx()
	var cur probeDoc
	probeErr := s.coll.FindOne(pctx, bson.D{{Key: "_id", Value: id}},
		options.FindOne().SetProjection(bson.D{{Key: "status", Value: 1}, {Key: "lastError", Value: 1}, {Key: "connectedAt", Value: 1}, {Key: "displayName", Value: 1}, {Key: "scopes", Value: 1}})).Decode(&cur)
	pcancel()
	probeUnexpected := probeErr != nil && !errors.Is(probeErr, mongoDrv.ErrNoDocuments)
	if probeUnexpected {
		s.logf("connectors: SetStatus pre-read %s failed: %v", id, probeErr)
	}
	if probeErr == nil && cur.Status == st && cur.LastError == lastErr {
		if displayFresh(cur, id, lastErr) {
			// Pure no-op: nothing to heal, no write at all.
			return s.readBack(id)
		}
		// Repair-only: display and/or cleared error, no updated_at bump —
		// updated_at stays last transition. $unset rides along on the
		// disconnecting state so a corrupt row's tokens die in the same
		// write that would otherwise leave them.
		if _, derr := s.coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}},
			repairUpdateDoc(id, lastErr, st == StatusDisconnected)); derr != nil {
			s.logf("connectors: display refresh %s failed: %v", id, derr)
			return Connector{}, NewStoreError("connector write failed", true)
		}
		return s.readBack(id)
	}
	// ConnectedAt is first-ever connect, not last-write and not
	// last-reconnect: re-marking an already connected row (e.g. a future
	// idempotent token-refresh write) must not move the timestamp, and a
	// disconnect→reconnect cycle keeps the original stamp (Disconnect
	// retains it, both backends). Single-writer assumption: no two
	// SetStatus(connected) run concurrently in v1 (gateway verbs only; the
	// OAuth follow-up must keep token writes on one serialised path, not
	// copy this read for non-idempotent work). Resolved only on the
	// connected path — other paths pass 0, which the builder ignores.
	connectedAt := int64(0)
	if st == StatusConnected {
		if probeUnexpected {
			return Connector{}, NewStoreError("connector read failed", true)
		}
		connectedAt = resolveConnectedAt(cur.ConnectedAt, now)
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
	// Transition is defined against the STORED row, not Get's synthesised
	// view: Get returns DefaultRow(disconnected) for a missing doc, which
	// would read a never-touched id as "already off" and skip the write —
	// losing the explicit-off signal (updated_at set). ErrNoDocuments
	// therefore means transition (see disconnectTransition).
	pctx, pcancel := quickCtx()
	var probe probeDoc
	probeErr := s.coll.FindOne(pctx, bson.D{{Key: "_id", Value: id}},
		options.FindOne().SetProjection(bson.D{{Key: "status", Value: 1}, {Key: "lastError", Value: 1}, {Key: "displayName", Value: 1}, {Key: "scopes", Value: 1}})).Decode(&probe)
	pcancel()
	transition, preReadOK := disconnectTransition(probeErr, probe.Status)
	// Idempotent no-op: already disconnected means updated_at is the last
	// transition, not the last verb. Pure no-op (display fresh, error
	// already clear) skips the write entirely; stale display/error takes
	// the repair shape — display + clear, no updated_at bump, tokens
	// unset in the same write.
	if !transition {
		if displayFresh(probe, id, "") {
			return s.readBack(id)
		}
		if _, derr := s.coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}},
			repairUpdateDoc(id, "", true)); derr != nil {
			s.logf("connectors: display refresh %s failed: %v", id, derr)
			return Connector{}, NewStoreError("connector disconnect failed", true)
		}
		return s.readBack(id)
	}
	// On probe error the write lands anyway (fail-loud) but updated_at then
	// means "last verb attempted", not "last transition" — the
	// (pre-read unavailable) log label marks exactly this case.
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

// resolveConnectedAt is the first-ever-connect rule both backends share:
// a nonzero stored stamp survives re-marks and disconnect→reconnect cycles,
// zero takes now. Pure, so the Mongo probe-then-write sequence and the memory
// write pin the same semantic with one no-DB test.
func resolveConnectedAt(stamp, now int64) int64 {
	if stamp != 0 {
		return stamp
	}
	return now
}

// probeDoc is the minimal read for transition decisions: status (+lastError
// where compared, +connectedAt where the stamp resolves, +display where the
// no-op decides heal-vs-skip). Never the token columns.
type probeDoc struct {
	Status      Status   `bson:"status"`
	LastError   string   `bson:"lastError"`
	ConnectedAt int64    `bson:"connectedAt"`
	DisplayName string   `bson:"displayName"`
	Scopes      []string `bson:"scopes"`
}

// displayFresh reports whether the stored display already matches the
// constants (and the error matches the caller's intent): nothing to heal,
// so the no-op can skip the $set entirely rather than healing nothing at
// the cost of two round trips.
func displayFresh(cur probeDoc, id, lastErr string) bool {
	if cur.LastError != lastErr || cur.DisplayName != DisplayName(id) {
		return false
	}
	want := ScopesFor(id)
	if len(cur.Scopes) != len(want) {
		return false
	}
	for i := range want {
		if cur.Scopes[i] != want[i] {
			return false
		}
	}
	return true
}

// repairUpdateDoc builds the repair-only write: display + error refresh with
// NO updatedAt/status/connectedAt keys, so a repair can never move the
// last-transition timestamp. Pinned by test — a future edit adding
// updatedAt here silently turns every repeat verb into a bump.
func repairUpdateDoc(id, lastErr string, unsetTokens bool) bson.D {
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "displayName", Value: DisplayName(id)},
		{Key: "scopes", Value: ScopesFor(id)},
		{Key: "lastError", Value: lastErr},
	}}}
	if unsetTokens {
		update = append(update, bson.E{Key: "$unset", Value: bson.D{
			{Key: refreshTokenField, Value: ""},
			{Key: accessTokenField, Value: ""},
		}})
	}
	return update
}

// disconnectTransition decides the Disconnect write from the raw status
// probe. Pure so the explicit-off rule is pinned without Atlas:
//   - absent doc (ErrNoDocuments) → transition (write; sets explicit-off)
//   - stored-disconnected → no-op (skip; updated_at stays last transition)
//   - any other status → transition (write)
//   - probe error → write anyway, flagged (fail-loud; see preReadOK)
func disconnectTransition(probeErr error, status Status) (transition, preReadOK bool) {
	switch {
	case probeErr == nil:
		return status != StatusDisconnected, true
	case errors.Is(probeErr, mongoDrv.ErrNoDocuments):
		return true, true
	default:
		return true, false
	}
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
// DefaultRow synthesis covers absent ones: scopes rewrite to canonical
// order (not just fill-when-empty, so a hand-edited reversed order never
// reaches the wire — matching displayFresh's order-sensitive heal check),
// display name always rewrites for the same reason, and status defaults to
// disconnected. A connected row with a zero stamp keeps 0 (no per-response
// now-fill — that would drift on every read without persisting); the wire
// omits connected_at and the store heals on the next transition.
// Only the expired path carries error text anywhere in this package
// (write paths enforce it, no-op refreshes preserve the clear): a corrupt
// doc's stale error on a healthy (connected) or off (disconnected) row is
// cleared here rather than rendered, matching the empty-status repair
// below. Fail-closed display repair — never invents state, only removes
// text healthy rows must not show.
//
// The scrub stays for the expired path: a token in an expired row's
// lastError would otherwise emit verbatim on read — the exact leak the
// write scrub exists to prevent. ScrubError is idempotent on
// already-scrubbed text.
func backfillDisplay(c Connector, id string) Connector {
	// Display is canonical, not best-effort: scopes always rewrite from the
	// constants (order included) and so does the display name, so a
	// hand-edited reversed order or label never survives to the wire —
	// consistent with displayFresh, which treats any display mismatch as
	// heal-worthy, and with every write path.
	c.Scopes = ScopesFor(id)
	c.DisplayName = DisplayName(id)
	if !c.Status.Valid() {
		c.Status = StatusDisconnected
	}
	// No stamp repair here, deliberately: a connected row with a zero stamp
	// (hand-edit, OAuth bug writing status without it) keeps 0 in the
	// response and the wire omits connected_at (fail-closed — "emitted only
	// while connected with a valid stamp"). Repairing to now per response
	// would show a moving timestamp that never persists; the store heals on
	// the next real transition write (resolveConnectedAt maps 0 → now).
	if c.Status == StatusExpired {
		c.LastError = approvals.ScrubError(c.LastError)
	} else {
		c.LastError = ""
	}
	return c
}

// readBack re-reads one row on a fresh budget. The gateway hands one 5s ctx
// for the whole verb; a slow Atlas write could consume it and turn a landed
// write into a 502 on the confirming read (retry-safe: a second verb
// succeeds idempotently, but triage shouldn't have to know that).
// quickCtx gives pre-reads and the confirming read their own short budget
// instead of spending the verb's deadline: a slow read must never starve a
// would-be-successful write into a 502. Detached from request cancellation
// deliberately — one cheap idempotent read at 3 rows. Do not copy this for
// non-idempotent work (the OAuth follow-up's token writes stay on the
// request ctx).
//
// Budget composition, stated so the follow-up doesn't copy the stacking:
// worst case per verb is 5s pre-read + 5s write (verb ctx) + 5s readBack,
// sequential (~15s) with no overall gateway deadline. All three are cheap
// indexed reads/writes at 3 rows; heavier OAuth writes and the real token
// exchange need one verb-level bound instead of stacked per-call budgets.
func quickCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func (s *MongoStore) readBack(id string) (Connector, error) {
	ctx, cancel := quickCtx()
	defer cancel()
	return s.Get(ctx, id)
}
