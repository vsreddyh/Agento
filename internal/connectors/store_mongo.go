package connectors

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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
// Disconnect clears the token columns. Already-fetched data (chat history)
// is NOT wiped: disconnect stops future access, it does not
// rewrite the past.
type MongoStore struct {
	coll *mongoDrv.Collection
	// states holds pending OAuth attempts (see StatesCollection).
	states *mongoDrv.Collection
	// Logf receives operational notes. Nil discards them.
	Logf func(string, ...any)
}

func (s *MongoStore) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// scrubErr scrubs + bounds a store-internal error for log lines: driver
// errors can embed the offending filter/value, so a raw %v would leak
// caller-influenced text into container logs. Same contract as the
// gateway's bounded(ScrubError(...)) on the read path.
func scrubErr(err error) string {
	if err == nil {
		return ""
	}
	return approvals.ScrubError(err.Error())
}

// New connects to uri/dbName.
func New(uri, dbName string) (*MongoStore, error) {
	_, db, err := mongostore.Open(uri, dbName)
	if err != nil {
		return nil, err
	}
	return &MongoStore{
		coll:   db.Collection(Collection),
		states: db.Collection(StatesCollection),
	}, nil
}

// Durable reports true: Mongo outlives the process, so grants stored
// here survive restarts and the gateway may enable OAuth on top.
func (s *MongoStore) Durable() bool { return true }

// FromEnv connects from the shared MONGODB_URI/MONGODB_DB pair.
func FromEnv() (*MongoStore, error) {
	uri, db := mongostore.Env()
	return New(uri, db)
}

// EnsureIndexes creates the states TTL (pending attempts die on their own;
// the relay-style sweep other stores need does not exist here by design)
// plus the connector lookup the supersede/scrub deletes filter on. The
// connectors collection needs none: two fixed rows keyed by _id.
func (s *MongoStore) EnsureIndexes(ctx context.Context) error {
	if _, err := s.states.Indexes().CreateOne(ctx, mongoDrv.IndexModel{
		Keys:    bson.D{{Key: "expiresAt", Value: 1}},
		Options: options.Index().SetName("oauth_states_ttl").SetExpireAfterSeconds(0),
	}); err != nil {
		return err
	}
	_, err := s.states.Indexes().CreateOne(ctx, mongoDrv.IndexModel{
		Keys:    bson.D{{Key: "connector", Value: 1}},
		Options: options.Index().SetName("oauth_states_connector"),
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
		s.logf("connectors: list failed: %s", scrubErr(err))
		return nil, NewStoreError("connector list failed", true)
	}
	// Cleanup on its own budget, not the verb's: a slow Atlas read must not
	// make the cursor close eat the same expired deadline.
	defer cur.Close(context.Background())
	byID := make(map[string]Connector, 2)
	for cur.Next(ctx) {
		var c Connector
		if err := cur.Decode(&c); err != nil {
			s.logf("connectors: list decode failed: %s", scrubErr(err))
			return nil, NewStoreError("connector list failed", true)
		}
		byID[c.ID] = c
	}
	if err := cur.Err(); err != nil {
		s.logf("connectors: list cursor failed: %s", scrubErr(err))
		return nil, NewStoreError("connector list failed", true)
	}
	out := make([]Connector, 0, len(listIDs))
	var ignored []string
	for id := range byID {
		// Raw, deliberately NOT folded: the storage contract is lowercase
		// _ids (every write normalises — CreateState, SetTokens,
		// Disconnect, SetStatus all fold first), so a case-variant doc
		// ("Gmail") stays stray + logged rather than served. Folding here
		// would make List serve a row Get(FindOne, exact match) cannot
		// see — List saying "connected" while Get says "never touched"
		// for the same logical id. Unfolded, both synthesise DefaultRow
		// and triage gets the log line.
		if !Known(id) {
			ignored = append(ignored, id)
		}
	}
	// Fixed-set v1: stray docs (typo'd id from a hand-edit or a writer bug
	// that skipped the normalise step) are excluded from the shape, but
	// must not vanish silently — triage would otherwise see "2
	// disconnected rows" while Mongo holds 3 docs. Sorted so two log lines
	// for the same stray id diff clean.
	sort.Strings(ignored)
	if len(ignored) > 0 {
		// Capped like the gateway's bounded(): a bad migration writing
		// thousands of typo'd docs must not turn one list call into a
		// multi-MB log line. First 10 quoted, then a count.
		const maxStrayLogged = 10
		quoted := make([]string, 0, maxStrayLogged)
		for i, id := range ignored {
			if i >= maxStrayLogged {
				break
			}
			quoted = append(quoted, fmt.Sprintf("%q", id))
		}
		suffix := ""
		if extra := len(ignored) - len(quoted); extra > 0 {
			suffix = fmt.Sprintf(" …and %d more", extra)
		}
		s.logf("connectors: ignoring %d unknown row(s): %s%s", len(ignored), strings.Join(quoted, ", "), suffix)
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
	// Length before fold and before Known: ids are two short fixed
	// strings, so anything over the cap is certainly unknown — fail
	// without the normalize alloc or a quoted value in the 404.
	if len(id) > MaxConnectorIDLen {
		return Connector{}, NewStoreError("oversized connector id", false)
	}
	id = NormalizedID(id)
	// Controls fail closed like every sibling input: a control-bearing
	// id would otherwise ride the "unknown connector" echo below into
	// the JSON body and the scrubbed log line. Checked post-fold so
	// edge Cf forgiven by TrimCredential still folds instead of 400ing.
	if HasControl(id) || !utf8.ValidString(id) || HasWhitespace(id) {
		return Connector{}, NewStoreError("invalid connector id", false)
	}
	if !Known(id) {
		return Connector{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	var c Connector
	if err := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}}, findOneOpts()).Decode(&c); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return DefaultRow(id), nil
		}
		s.logf("connectors: read %q failed: %s", id, scrubErr(err))
		return Connector{}, NewStoreError("connector read failed", true)
	}
	return backfillDisplay(c, id), nil
}

func (s *MongoStore) SetStatus(ctx context.Context, id string, st Status, lastError string) (Connector, error) {
	// Length before fold and before Known: ids are two short fixed
	// strings, so anything over the cap is certainly unknown — fail
	// without the normalize alloc or a quoted value in the 404.
	if len(id) > MaxConnectorIDLen {
		return Connector{}, NewStoreError("oversized connector id", false)
	}
	id = NormalizedID(id)
	// Controls fail closed like every sibling input: a control-bearing
	// id would otherwise ride the "unknown connector" echo below into
	// the JSON body and the scrubbed log line. Checked post-fold so
	// edge Cf forgiven by TrimCredential still folds instead of 400ing.
	if HasControl(id) || !utf8.ValidString(id) || HasWhitespace(id) {
		return Connector{}, NewStoreError("invalid connector id", false)
	}
	if !Known(id) {
		return Connector{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	if !st.Valid() {
		return Connector{}, NewStoreError("unknown connector status", false)
	}
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
	//
	// Pending attempts die on every disconnecting path too (like the
	// tokens, and like Disconnect): a browser completing an old consent
	// after this verb must fail at ConsumeState, not silently reconnect
	// without fresh intent. Advisory on its own detached budget (TTL is
	// the backstop); a failed scrub only logs, never fails the verb.
	if st == StatusDisconnected {
		srubCtx, scrubCancel := quickCtx()
		_, serr := s.states.DeleteMany(srubCtx, bson.D{{Key: "connector", Value: id}})
		scrubCancel()
		if serr != nil {
			s.logf("connectors: state scrub %q failed: %s", id, scrubErr(serr))
		}
	}
	pctx, pcancel := quickCtx()
	var cur probeDoc
	probeErr := s.coll.FindOne(pctx, bson.D{{Key: "_id", Value: id}},
		options.FindOne().SetProjection(probeProjection())).Decode(&cur)
	pcancel()
	probeUnexpected := probeErr != nil && !errors.Is(probeErr, mongoDrv.ErrNoDocuments)
	if probeUnexpected {
		s.logf("connectors: SetStatus pre-read %q failed: %s", id, scrubErr(probeErr))
	}
	if probeErr == nil && cur.Status == st && cur.LastError == lastErr {
		// Pure no-op only when there is also no grant to clear: a
		// disconnected row that somehow still holds tokens must take the
		// repair write below (which unsets them), never a skipped write
		// that would keep serving the grant.
		if displayFresh(cur, id, lastErr) && (st != StatusDisconnected || grantClear(cur)) {
			// Pure no-op: nothing to heal, no write at all.
			return s.readBack(id)
		}
		// Repair-only: display and/or cleared error, no updated_at bump —
		// updated_at stays last transition. $unset rides along on the
		// disconnecting state so a corrupt row's tokens die in the same
		// write that would otherwise leave them.
		if _, derr := s.coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}},
			repairUpdateDoc(id, lastErr, st == StatusDisconnected)); derr != nil {
			s.logf("connectors: display refresh %q failed: %s", id, scrubErr(derr))
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
	// Sampled AFTER the probe: the stamp must reflect the write, not the
	// pre-read start — a slow Atlas read would otherwise backdate updatedAt
	// by up to 5s. Same below in Disconnect.
	now := time.Now().UnixMilli()
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
		s.logf("connectors: write %q failed: %s", id, scrubErr(err))
		return Connector{}, NewStoreError("connector write failed", true)
	}
	return s.readBack(id)
}

func (s *MongoStore) Disconnect(ctx context.Context, id string) (Connector, error) {
	// Length before fold and before Known: ids are two short fixed
	// strings, so anything over the cap is certainly unknown — fail
	// without the normalize alloc or a quoted value in the 404.
	if len(id) > MaxConnectorIDLen {
		return Connector{}, NewStoreError("oversized connector id", false)
	}
	id = NormalizedID(id)
	// Controls fail closed like every sibling input: a control-bearing
	// id would otherwise ride the "unknown connector" echo below into
	// the JSON body and the scrubbed log line. Checked post-fold so
	// edge Cf forgiven by TrimCredential still folds instead of 400ing.
	if HasControl(id) || !utf8.ValidString(id) || HasWhitespace(id) {
		return Connector{}, NewStoreError("invalid connector id", false)
	}
	if !Known(id) {
		return Connector{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	// Transition is defined against the STORED row, not Get's synthesised
	// view: Get returns DefaultRow(disconnected) for a missing doc, which
	// would read a never-touched id as "already off" and skip the write —
	// losing the explicit-off signal (updated_at set). ErrNoDocuments
	// therefore means transition (see disconnectTransition).
	pctx, pcancel := quickCtx()
	var probe probeDoc
	probeErr := s.coll.FindOne(pctx, bson.D{{Key: "_id", Value: id}},
		options.FindOne().SetProjection(probeProjection())).Decode(&probe)
	pcancel()
	// Sampled AFTER the probe, like SetStatus: the stamp reflects the write,
	// not the pre-read start — a slow Atlas read would otherwise backdate
	// updatedAt by up to 5s on the one verb v1 actually serves.
	now := time.Now().UnixMilli()
	// Pending attempts die on every disconnect path, like the tokens: a
	// browser completing an old consent after this disconnect must fail at
	// ConsumeState, not silently reconnect without fresh intent. Runs on
	// all paths (including the pure no-op below) on its own detached
	// budget — same quickCtx rationale as readBack: a slow Atlas scrub
	// must not consume the verb budget and turn the write into a 502,
	// and its result is advisory anyway (TTL is the backstop).
	srubCtx, scrubCancel := quickCtx()
	_, serr := s.states.DeleteMany(srubCtx, bson.D{{Key: "connector", Value: id}})
	scrubCancel()
	if serr != nil {
		s.logf("connectors: state scrub %q failed: %s", id, scrubErr(serr))
	}
	transition, preReadOK := disconnectTransition(probeErr, probe.Status)
	// Idempotent no-op: already disconnected means updated_at is the last
	// transition, not the last verb. Pure no-op (display fresh, error
	// already clear) skips the write entirely; stale display/error takes
	// the repair shape — display + clear, no updated_at bump, tokens
	// unset in the same write.
	if !transition {
		// Pure no-op only with no grant to clear: a disconnected row that
		// somehow still holds tokens takes the repair write below (which
		// unsets them) instead of a skipped write that would keep serving
		// the grant.
		if displayFresh(probe, id, "") && grantClear(probe) {
			return s.readBack(id)
		}
		if _, derr := s.coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}},
			repairUpdateDoc(id, "", true)); derr != nil {
			s.logf("connectors: display refresh %q failed: %s", id, scrubErr(derr))
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
		s.logf("connectors: disconnect %q failed: %s", id, scrubErr(err))
		return Connector{}, NewStoreError("connector disconnect failed", true)
	}
	if transition {
		if preReadOK {
			s.logf("connectors: %q disconnected (tokens cleared)", id)
		} else {
			s.logf("connectors: %q disconnected (tokens cleared, pre-read unavailable)", id)
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

// probeProjection selects the transition-decision fields: status (+lastError
// where compared, +connectedAt where the stamp resolves, +display for the
// no-op heal-vs-skip, +token-column presence for the token-parity check).
// A fresh copy per call, like displayProjection: the no-op path decides
// heal-vs-skip off these keys, so dropping one (e.g. scopes) silently
// turns every repeat verb into a heal-write. The token columns are the
// one deliberate exception to "never the token columns": only their
// presence is read (never returned), and the no-op paths need all three
// to avoid stranding a partial grant behind a skipped write. Presence is
// unbounded by design: an over-cap cipher reads as present (repair path),
// never served — serving happens only through GetTokens/DecryptToken, so
// the driver decode alloc is the whole exposure and there is no second
// read-side cap to add here.
func probeProjection() bson.D {
	return bson.D{
		{Key: "_id", Value: 0},
		{Key: "status", Value: 1},
		{Key: "lastError", Value: 1},
		{Key: "connectedAt", Value: 1},
		{Key: "displayName", Value: 1},
		{Key: "scopes", Value: 1},
		{Key: refreshTokenField, Value: 1},
		{Key: accessTokenField, Value: 1},
		{Key: "accessExpiry", Value: 1},
	}
}

// probeDoc is the minimal read for transition decisions. Token columns are
// presence-only (never returned, never logged); see probeProjection.
type probeDoc struct {
	Status      Status   `bson:"status"`
	LastError   string   `bson:"lastError"`
	ConnectedAt int64    `bson:"connectedAt"`
	DisplayName string   `bson:"displayName"`
	Scopes      []string `bson:"scopes"`
	Refresh     string   `bson:"refreshToken"`
	Access      string   `bson:"accessToken"`
	Expiry      int64    `bson:"accessExpiry"`
}

// grantClear reports whether a probed row holds no grant in any column.
// Every column counts, verbatim: a row with an empty refresh but a
// leftover access cipher (hand-edit, partial clear, future refresher bug)
// must take the repair write, never the pure no-op that would keep serving
// it. Presence is raw (`!= ""`), not trimmed: a whitespace-only cipher is
// still a stored artifact — unservable (GetTokens keeps its own
// whitespace gate), but real enough to repair rather than skip.
func grantClear(cur probeDoc) bool {
	return cur.Refresh == "" &&
		cur.Access == "" &&
		cur.Expiry == 0
}

// displayFresh is the probeDoc-shaped entry to displayFieldsFresh for the
// Mongo no-op paths.
func displayFresh(cur probeDoc, id, lastErr string) bool {
	return displayFieldsFresh(id, cur.DisplayName, cur.Scopes, cur.LastError, lastErr)
}

// displayFieldsFresh is the shared no-op heal check behind displayFresh
// (Mongo) and the memory store's inline compare: same fields, same order
// sensitivity, one definition. Both backends call this so a new display
// field cannot heal on one path and skip on the other.
func displayFieldsFresh(id, displayName string, scopes []string, lastErr, wantErr string) bool {
	if lastErr != wantErr || displayName != DisplayName(id) {
		return false
	}
	want := ScopesFor(id)
	if len(scopes) != len(want) {
		return false
	}
	for i := range want {
		if scopes[i] != want[i] {
			return false
		}
	}
	return true
}

// tokenUnset clears every token-shaped column in one place. All three
// disconnect paths (repair, status, full) must unset ALL of them —
// forgetting accessExpiry leaves stale PII-adjacent data with no tokens,
// and forgetting a future column breaks the parity silently. One list.
func tokenUnset() bson.D {
	return bson.D{
		{Key: refreshTokenField, Value: ""},
		{Key: accessTokenField, Value: ""},
		{Key: "accessExpiry", Value: ""},
	}
}

// repairUpdateDoc builds the repair-only write: display + error refresh with
// NO updatedAt/status/connectedAt keys, so a repair can never move the
// last-transition timestamp. Pinned by test — a future edit adding
// updatedAt here silently turns every repeat verb into a bump.
//
// Repairs never upsert: both call sites guarantee the doc exists (SetStatus
// repair requires a matching probe, Disconnect no-op requires a stored
// disconnected row). A future delete path (the ADR leaves the door open for
// a reaper on abandoned-expired rows) must preserve that guarantee — a
// repair matching 0 docs reads back synthesised DefaultRow, and the caller
// would see healed state over a missing doc with no error.
func repairUpdateDoc(id, lastErr string, unsetTokens bool) bson.D {
	update := bson.D{{Key: "$set", Value: bson.D{
		{Key: "displayName", Value: DisplayName(id)},
		{Key: "scopes", Value: ScopesFor(id)},
		{Key: "lastError", Value: lastErr},
	}}}
	if unsetTokens {
		update = append(update, bson.E{Key: "$unset", Value: tokenUnset()})
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
// be resolved (first-connect preserved by the caller). Upserted: a missing
// row is created by this write, so the tokens-before-status ordering in the
// callback assumes no concurrent doc delete between SetTokens and this call
// — nothing deletes connectors docs today (no API deletes, retention never
// prunes them), which is exactly what makes that assumption hold.
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
		update = append(update, bson.E{Key: "$unset", Value: tokenUnset()})
	}
	return update
}

// disconnectUpdateDoc builds the single UpdateOne body for Disconnect. Same
// atomicity contract as statusUpdateDoc: tokens die in the same write that
// flips the status. History (chat) is untouched by design.
func disconnectUpdateDoc(id string, now int64) bson.D {
	return bson.D{
		{Key: "$set", Value: bson.D{
			{Key: "displayName", Value: DisplayName(id)},
			{Key: "scopes", Value: ScopesFor(id)},
			{Key: "status", Value: string(StatusDisconnected)},
			{Key: "lastError", Value: ""},
			{Key: "updatedAt", Value: now},
		}},
		{Key: "$unset", Value: tokenUnset()},
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
// by design — no tracing in this stack to lose, and budgets are short so
// orphaned IO from a hung-up client stays bounded at 5s. Accepted
// consequence: a hung-up client still blocks on detached IO (the gateway's
// gone-client check after a detached call can only skip the work that
// hasn't started). Do not copy this for
// non-idempotent work: the OAuth token writes on the callback path stay
// on the 30s request ctx for exactly this reason.
//
// Budget composition, stated so the follow-up doesn't copy the stacking:
// worst case per verb is 5s pre-read + 5s scrub + 5s write (verb ctx) +
// 5s readBack, sequential (~15-20s wall) with no overall gateway deadline.
// Each step is cheap indexed reads/writes at two rows; heavier OAuth writes
// and the real token exchange need one verb-level bound instead of stacked
// per-call budgets. The verb ctx still bounds the write itself — only the
// advisory pre-work runs detached.
func quickCtx() (context.Context, context.CancelFunc) {
	// Background-rooted by design, not by leak: detaching from the verb
	// ctx keeps slow Atlas off the verb deadline (validation-only there),
	// but it also detaches from shutdown drain — a SIGTERM mid-verb can
	// wait out the sequential detached budgets (~5s each) before the
	// process exits. Accepted: shutdown delay, never lost work, and the
	// verb ctx still bounds the write itself.
	return context.WithTimeout(context.Background(), 5*time.Second)
}

func (s *MongoStore) readBack(id string) (Connector, error) {
	ctx, cancel := quickCtx()
	defer cancel()
	return s.Get(ctx, id)
}

// SetTokens persists an encrypted grant. Ciphertext-opaque: the store never
// sees plaintext (see TokenRow) and enforces only the renewal invariant —
// an empty refresh cipher is rejected, because a stored grant without one
// strands the row at the first access-token expiry.
func (s *MongoStore) SetTokens(ctx context.Context, id string, tok TokenRow) error {
	// Length before fold (see Get): an oversized id is certainly
	// unknown — fail before the normalize alloc.
	if len(id) > MaxConnectorIDLen {
		return NewStoreError("oversized connector id", false)
	}
	id = NormalizedID(id)
	// Controls fail closed like the id verbs above: a control-bearing id
	// would otherwise ride the "unknown connector" echo below.
	if HasControl(id) || !utf8.ValidString(id) || HasWhitespace(id) {
		return NewStoreError("invalid connector id", false)
	}
	// Length before identity, matching CreateState/ConsumeState/Exchange:
	// an oversized input 400s the same way whether the id is known or
	// not, so the unknown-id path cannot be told apart by which error
	// fires first. Same order as the memory twin.
	if len(tok.RefreshCipher) > MaxCipherLen || len(tok.AccessCipher) > MaxCipherLen {
		return NewStoreError("connector grant token too long", false)
	}
	// Controls fail closed like every sibling input: a control-bearing
	// cipher is undecryptable (DecryptToken only fails later, generically),
	// so reject the shape here that the read side cannot name.
	// TrimCredential inside HasWhitespace is the interior-only idiom, not
	// double-handling: edges are already owned by the blank gate below
	// and the padded gate further down, so only interior whitespace can
	// fire here. Do not "simplify" the wrap away — raw HasWhitespace
	// would re-label blank/padded inputs with this gate's message.
	if HasControl(tok.RefreshCipher) || HasControl(tok.AccessCipher) || !utf8.ValidString(tok.RefreshCipher) || !utf8.ValidString(tok.AccessCipher) || HasWhitespace(TrimCredential(tok.RefreshCipher)) || (tok.AccessCipher != "" && HasWhitespace(TrimCredential(tok.AccessCipher))) {
		return NewStoreError("connector grant token is invalid", false)
	}
	// Empty before identity, matching every sibling verb: an empty or
	// blank cipher 400s the same way whether the id is known or not.
	if TrimCredential(tok.RefreshCipher) == "" {
		return NewStoreError("connector grant needs a refresh token", false)
	}
	// Whitespace-only access is neither a sealed grant nor literal "": the
	// refresher keys missing off "", so a padded value would create a
	// second missing representation that reads as present. Reject it here
	// (same gate as refresh) rather than letting it through to storage.
	if tok.AccessCipher != "" && TrimCredential(tok.AccessCipher) == "" {
		return NewStoreError("connector grant access token is blank", false)
	}
	// Padded before identity, like every sibling shape gate: a padded
	// cipher for an unknown id 400s instead of 404ing on identity.
	// Checked == stored — the string the checks see and the bytes the
	// store keeps must be identical, or a direct caller mints an
	// undecryptable "present" grant (same class as the state
	// trim-at-write contract, enforced as a reject here because stored
	// credentials must round-trip byte-exact).
	if tok.RefreshCipher != TrimCredential(tok.RefreshCipher) ||
		(tok.AccessCipher != "" && tok.AccessCipher != TrimCredential(tok.AccessCipher)) {
		return NewStoreError("connector grant token is padded", false)
	}
	if !Known(id) {
		return &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	// Expiry-without-access is a second missing representation: the
	// refresher keys renew-now off zero expiry, and the no-op predicates
	// read a non-zero expiry as grant signal. The gateway zeroes expiry
	// when access is empty (single representation of missing) — the store
	// enforces the same rule instead of relying on the one prod caller.
	if tok.AccessCipher == "" && tok.AccessExpiryUnixMilli != 0 {
		return NewStoreError("connector grant expiry without access token", false)
	}
	// Clamp absurd futures: a far-future expiry reads as "don't renew for
	// decades". Capped like the state clamp (row still works, renews
	// soon). Wall-clock by design; the injectable clock lives on the
	// memory side, where state-expiry ticks are pinned. Single read for
	// both the clamp and the backfill stamp below: two time.Now() calls
	// can straddle a millisecond tick under the same single-now rationale
	// as CreateState.
	now := time.Now()
	tok.AccessExpiryUnixMilli = ClampAccessExpiry(tok.AccessExpiryUnixMilli, now)
	// Display defaults on insert only: a token write must never invent
	// status (backfill synthesises disconnected for a status-less doc
	// on read), and without these a crash between SetTokens and
	// SetStatus would leave a display-less row. updatedAt rides along so
	// the crash-window row reads as explicit-off (something happened here)
	// rather than never-touched. Wall-clock by design like the memory
	// twin: display-only stamp, not a lifetime tick (injectable timeNow
	// owns state-expiry ticks). Reads project only the display shape, so
	// these columns are inert until the transition write lands the real row.
	nowMillis := now.UnixMilli()
	_, err := s.coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{
		{Key: "$set", Value: bson.D{
			{Key: refreshTokenField, Value: tok.RefreshCipher},
			{Key: accessTokenField, Value: tok.AccessCipher},
			{Key: "accessExpiry", Value: tok.AccessExpiryUnixMilli},
		}},
		{Key: "$setOnInsert", Value: bson.D{
			{Key: "displayName", Value: DisplayName(id)},
			{Key: "scopes", Value: ScopesFor(id)},
			// Status rides along so the crash-window row reads exactly
			// like the memory backend's insert-only DefaultRow (which
			// carries Status disconnected): same interleaving, same
			// repair/no-bump path on the next SetStatus(disconnected).
			{Key: "status", Value: string(StatusDisconnected)},
			{Key: "updatedAt", Value: nowMillis},
		}},
	}, options.Update().SetUpsert(true))
	if err != nil {
		s.logf("connectors: token write %q failed: %s", id, scrubErr(err))
		return NewStoreError("connector token write failed", true)
	}
	return nil
}

// GetTokens reads the ciphertext grant. Unknown ids and grant-less rows are
// both NotFound: the caller reconnects either way, and distinguishing them
// would oracle stored-grant existence.
func (s *MongoStore) GetTokens(ctx context.Context, id string) (TokenRow, error) {
	// Length before fold and before Known: ids are two short fixed
	// strings, so anything over the cap is certainly unknown — fail
	// without the normalize alloc or a quoted value in the 404.
	if len(id) > MaxConnectorIDLen {
		return TokenRow{}, NewStoreError("oversized connector id", false)
	}
	id = NormalizedID(id)
	// Controls fail closed like every sibling input: a control-bearing
	// id would otherwise ride the "unknown connector" echo below into
	// the JSON body and the scrubbed log line. Checked post-fold so
	// edge Cf forgiven by TrimCredential still folds instead of 400ing.
	if HasControl(id) || !utf8.ValidString(id) || HasWhitespace(id) {
		return TokenRow{}, NewStoreError("invalid connector id", false)
	}
	if !Known(id) {
		return TokenRow{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	var row struct {
		Refresh string `bson:"refreshToken"`
		Access  string `bson:"accessToken"`
		Expiry  int64  `bson:"accessExpiry"`
	}
	err := s.coll.FindOne(ctx, bson.D{{Key: "_id", Value: id}},
		options.FindOne().SetProjection(bson.D{
			{Key: refreshTokenField, Value: 1},
			{Key: accessTokenField, Value: 1},
			{Key: "accessExpiry", Value: 1},
		})).Decode(&row)
	if err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return TokenRow{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
		}
		s.logf("connectors: token read %q failed: %s", id, scrubErr(err))
		return TokenRow{}, NewStoreError("connector token read failed", true)
	}
	if TrimCredential(row.Refresh) == "" {
		return TokenRow{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	// Returned verbatim by design: size enforcement lives in DecryptToken
	// (len > MaxCipherLen errors there), so an oversized artifact from
	// before the cap fails at open time with a precise error instead of
	// being masked as NotFound here. Same for expiry freshness: the
	// MaxAccessLifetime clamp is a write-time invariant (gateway + both
	// SetTokens), not re-checked on read — a hand-edited far-future expiry
	// is served as-is until the row is next written.
	return TokenRow{RefreshCipher: row.Refresh, AccessCipher: row.Access, AccessExpiryUnixMilli: row.Expiry}, nil
}

// CreateState records a pending OAuth attempt. States are single-use by
// contract (see ConsumeState); an id collision is a caller bug (400), never
// an overwrite.
func (s *MongoStore) CreateState(_ context.Context, st OAuthState) error {
	// ctx is intentionally unused: delete + insert each run on their own
	// detached quickCtx below (a slow Atlas must not eat the verb budget
	// and turn the create into a 502). Named _ so the next reader doesn't
	// assume the verb deadline bounds this write — see the budget note at
	// the insert.
	// Normalised like every other verb: SetTokens/GetTokens/Disconnect all
	// fold case+spacing first, and a non-normalised stored row would miss
	// both the Known check here and the Disconnect scrub comparison later.
	// Length before fold: an oversized connector is certainly unknown —
	// fail before the normalize alloc (same class as the id verbs).
	if len(st.Connector) > MaxConnectorIDLen {
		return NewStoreError("oversized connector id", false)
	}
	st.Connector = NormalizedID(st.Connector)
	// Controls fail closed like the id verbs: a control-bearing connector
	// would otherwise ride the "unknown connector" echo below.
	if HasControl(st.Connector) || !utf8.ValidString(st.Connector) || HasWhitespace(st.Connector) {
		return NewStoreError("invalid connector id", false)
	}
	// Trimmed before storing: the callback trims query params before
	// ConsumeState, so a padded value stored verbatim would never match a
	// lookup — and a padded verifier would break Exchange at Google after
	// the user already consented. Normalise here, not at read time.
	st.State = TrimCredential(st.State)
	st.Verifier = TrimCredential(st.Verifier)
	if st.State == "" {
		return NewStoreError("oauth state needs an id", false)
	}
	// Empty → length → Known, matching Exchange: an empty or oversized
	// input 400s the same way whether the id is known or not, so the
	// unknown-id path cannot be told apart by which error fires first.
	if TrimCredential(st.Verifier) == "" {
		// Fail fast at creation, not after the user completed consent: a
		// verifier-less attempt can never survive Exchange.
		return NewStoreError("oauth state needs a verifier", false)
	}
	if len(st.State) > MaxStateFieldLen || len(st.Verifier) > MaxVerifierLen {
		return NewStoreError("oauth state or verifier too long", false)
	}
	if len(st.State) < MinStateLen {
		return NewStoreError("oauth state too short", false)
	}
	if len(st.Verifier) < MinVerifierLen {
		return NewStoreError("oauth verifier too short", false)
	}
	if HasControl(st.State) || HasControl(st.Verifier) || !utf8.ValidString(st.State) || !utf8.ValidString(st.Verifier) || HasWhitespace(st.State) || HasWhitespace(st.Verifier) {
		return NewStoreError("oauth state or verifier invalid", false)
	}
	if len(st.RedirectURI) > MaxRedirectLen {
		return NewStoreError("oauth redirect too long", false)
	}
	// Shape before identity, matching Exchange/AuthURL: an unknown id with
	// a bad redirect 400s on shape in every helper, so the same input pair
	// answers identically wherever it is checked. Canonicalised and stored,
	// so stored == sent by construction. Empty means minted before this
	// column existed: allowed, the callback falls back to the live config.
	// Callers must still pin (the gateway always does): an unpinned row silently
	// downgrades to rotation-fragile, exact-mismatching after any redirect rotation.
	// Empty is pre-column compat, not a supported mint shape for new callers.
	// (A sub-2048 garbage redirect would otherwise supersede the live
	// attempt and fail only at Exchange — after the user consented.)
	if st.RedirectURI != "" {
		canonical, err := CanonicalRedirectURL(st.RedirectURI)
		if err != nil {
			return NewStoreError("oauth redirect is invalid", false)
		}
		st.RedirectURI = canonical
		// Checked == stored: canonicalisation can grow the string
		// (IPv6 brackets), so the raw length gate above is not enough —
		// a just-under-cap raw could land just-over-cap canonical.
		if len(st.RedirectURI) > MaxRedirectLen {
			return NewStoreError("oauth redirect too long", false)
		}
	}
	if !Known(st.Connector) {
		return &StoreError{Msg: "unknown connector " + st.Connector, NotFound: true}
	}
	// Single clock read for the whole verb: default, clamp bound, and
	// past-expiry check must agree on "now", or a tick between reads
	// could mint-then-kill (or keep-then-reap) the same row.
	now := time.Now()
	if st.ExpiresAt.IsZero() {
		st.ExpiresAt = now.Add(StateTTL)
	}
	// Clamp absurd futures: a one-year oauth_states row is never a
	// legitimate pending attempt, and the TTL backstop would honour it.
	// Capped, not rejected — the row still works, just not forever.
	if exp := now.Add(StateTTL); st.ExpiresAt.After(exp) {
		st.ExpiresAt = exp
	}
	// Creation and ConsumeState agree on the boundary tick: == now counts
	// as expired on both sides (!After here, !Before there), so a direct
	// caller minting == now cannot supersede the live row with a stillborn
	// one the next callback instantly rejects as dead.
	if !st.ExpiresAt.After(now) {
		return NewStoreError("oauth state already expired", false)
	}
	// One pending attempt per connector (mirrors the memory cap): a new tap
	// supersedes the old one, bounding live rows to one per connector
	// regardless of tap spam — not "3 max", which would silently break
	// when a fourth connector lands. The TTL still reaps whatever the next
	// tap doesn't supersede.
	// Non-atomic by design (delete-then-insert): two concurrent taps for the
	// same connector may briefly leave two live rows, and both are valid —
	// either browser tab can complete its own consent. The bound holds for
	// sequential taps (the only rate that matters: one bearer holder, one
	// browser); no unique index, because failing a legitimate concurrent
	// tap with a duplicate-key error would be worse than an extra row the
	// next tap supersedes. And if the insert itself fails after the delete,
	// the old tab is dead too — re-tap is the recovery, not the old code.
	// The supersede runs on its own detached budget — same quickCtx
	// rationale as the Disconnect scrub: a slow Atlas delete must not
	// consume the verb budget and turn the insert into a 502 that reads
	// as "connect failed". Advisory either way, like that scrub: log and
	// continue on failure (TTL is the backstop, and a leftover old row
	// stays a valid attempt for its own browser tab), never fail the
	// create over a cleanup write. Note the consequence: the gateway's
	// verb-ctx timeout split (storeTimeoutOrGone) cannot see these
	// detached IO timeouts — a fired insCtx with a live verb ctx surfaces
	// as generic 502, not the timeout wording. Both clocks are 5s from a
	// near-simultaneous start, so the mistriage window is milliseconds,
	// and re-tap is the recovery either way.
	// One shared detached budget for dup-check + delete + insert: the
	// three run sequentially under a single 5s quickCtx instead of three
	// stacked ones (~15s wall before). Deliberately tighter than the sum:
	// the verb clock bounds the user-visible wall at ~5s regardless (see
	// the gateway budget note), so detached work that outlives the verb
	// only ever lands-then-cleans-up — a wider budget would burn more
	// slow-Atlas time for no user benefit. A slow Atlas exhausts the
	// shared budget and 502s faster than separate budgets would, but the
	// verb ctx never bounded this IO anyway (validation only) — re-tap
	// recovers either way. Duplicate-key tolerant below.
	// Order matters: the duplicate check runs BEFORE the supersede, so a
	// reused state id 400s instead of silently overwriting after the
	// delete removes the row it collides with.
	opCtx, opCancel := quickCtx()
	defer opCancel()
	var dup OAuthState
	dupErr := s.states.FindOne(opCtx, bson.D{{Key: "_id", Value: st.State}}).Decode(&dup)
	if dupErr == nil {
		return NewStoreError("oauth state already exists", false)
	}
	if dupErr != mongoDrv.ErrNoDocuments {
		// Connector id, not the state value: the state is a live
		// single-use capability and must never land in logs, even quoted.
		s.logf("connectors: state duplicate check for %q failed: %s", st.Connector, scrubErr(dupErr))
		return NewStoreError("oauth state create failed", true)
	}
	_, serr := s.states.DeleteMany(opCtx, bson.D{{Key: "connector", Value: st.Connector}})
	if serr != nil {
		s.logf("connectors: state supersede %q failed: %s", st.Connector, scrubErr(serr))
	}
	_, err := s.states.InsertOne(opCtx, st)
	if err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			return NewStoreError("oauth state already exists", false)
		}
		s.logf("connectors: state create failed: %s", scrubErr(err))
		return NewStoreError("oauth state create failed", true)
	}
	return nil
}

// ConsumeState atomically takes and deletes a pending attempt
// (FindOneAndDelete): a code can never be exchanged twice, even across two
// racing callbacks. Unknown, already-consumed, and expired states are the
// same NotFound — the app message is "try connect again" in all three, and
// distinguishing them would oracle live state ids. Expiry is enforced
// eagerly here AND lazily by the TTL index: the index reaps what this check
// already refused.
func (s *MongoStore) ConsumeState(ctx context.Context, state string) (OAuthState, error) {
	// Caller ctx by design, unlike CreateState's detached opCtx: consume is
	// a single indexed delete with no advisory pre-work, so the caller's
	// budget (5s verbs, 30s callback) is the right bound — there is nothing
	// to detach from. Trimmed like the write path and the gateway: padded
	// input resolves instead of 404ing on whitespace no writer could have stored.
	// Length-gated like the gateway: oversized input answers the same
	// NotFound as unknown, never reaching Mongo as a filter. Control-gated
	// like CreateState: a control-bearing id must not reach the filter.
	state = TrimCredential(state)
	if len(state) > MaxStateFieldLen {
		return OAuthState{}, &StoreError{Msg: "unknown or expired oauth state", NotFound: true}
	}
	if len(state) < MinStateLen {
		// Floor like every sibling leg: a short state is unmintable
		// (NewState mints 22 chars), so it answers the same NotFound as
		// unknown/expired instead of reaching the store as a filter.
		return OAuthState{}, &StoreError{Msg: "unknown or expired oauth state", NotFound: true}
	}
	if HasControl(state) || !utf8.ValidString(state) || HasWhitespace(state) {
		return OAuthState{}, &StoreError{Msg: "unknown or expired oauth state", NotFound: true}
	}
	var st OAuthState
	err := s.states.FindOneAndDelete(ctx, bson.D{{Key: "_id", Value: state}}).Decode(&st)
	if err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return OAuthState{}, &StoreError{Msg: "unknown or expired oauth state", NotFound: true}
		}
		s.logf("connectors: state consume failed: %s", scrubErr(err))
		return OAuthState{}, NewStoreError("oauth state consume failed", true)
	}
	// Zero counts as expired, matching the memory side (no IsZero carve-out
	// on either backend): no API writer can mint one, so a zero row is a
	// hand-edit — fail closed. The TTL reaps such rows eventually; this
	// refuses to serve one as live meanwhile. Boundary == now counts as
	// expired too, matching the memory side — pinned there by the
	// injectable-clock test (memory pinned, Mongo wall-clock by design;
	// DB tests need Atlas anyway).
	if st.ExpiresAt.IsZero() || !time.Now().Before(st.ExpiresAt) {
		return OAuthState{}, &StoreError{Msg: "unknown or expired oauth state", NotFound: true}
	}
	return st, nil
}
