package connectors

import (
	"context"
	"errors"
	"sort"
	"strings"
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
// the relay-style sweep other stores need does not exist here by design).
// The connectors collection needs none: two fixed rows keyed by _id.
func (s *MongoStore) EnsureIndexes(ctx context.Context) error {
	_, err := s.states.Indexes().CreateOne(ctx, mongoDrv.IndexModel{
		Keys:    bson.D{{Key: "expiresAt", Value: 1}},
		Options: options.Index().SetName("oauth_states_ttl").SetExpireAfterSeconds(0),
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
		s.logf("connectors: list failed: %v", err)
		return nil, NewStoreError("connector list failed", true)
	}
	// Cleanup on its own budget, not the verb's: a slow Atlas read must not
	// make the cursor close eat the same expired deadline.
	defer cur.Close(context.Background())
	byID := make(map[string]Connector, 2)
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
			s.logf("connectors: state scrub %s failed: %v", id, serr)
		}
	}
	pctx, pcancel := quickCtx()
	var cur probeDoc
	probeErr := s.coll.FindOne(pctx, bson.D{{Key: "_id", Value: id}},
		options.FindOne().SetProjection(probeProjection())).Decode(&cur)
	pcancel()
	probeUnexpected := probeErr != nil && !errors.Is(probeErr, mongoDrv.ErrNoDocuments)
	if probeUnexpected {
		s.logf("connectors: SetStatus pre-read %s failed: %v", id, probeErr)
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
		s.logf("connectors: state scrub %s failed: %v", id, serr)
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

// probeProjection selects the transition-decision fields: status (+lastError
// where compared, +connectedAt where the stamp resolves, +display for the
// no-op heal-vs-skip, +token-column presence for the token-parity check).
// A fresh copy per call, like displayProjection: the no-op path decides
// heal-vs-skip off these keys, so dropping one (e.g. scopes) silently
// turns every repeat verb into a heal-write. The token columns are the
// one deliberate exception to "never the token columns": only their
// presence is read (never returned), and the no-op paths need all three
// to avoid stranding a partial grant behind a skipped write.
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
// Every column counts: a row with an empty refresh but a leftover access
// cipher (hand-edit, partial clear, future refresher bug) must take the
// repair write, never the pure no-op that would keep serving it.
func grantClear(cur probeDoc) bool {
	return strings.TrimSpace(cur.Refresh) == "" &&
		strings.TrimSpace(cur.Access) == "" &&
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
		update = append(update, bson.E{Key: "$unset", Value: tokenUnset()})
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
// deliberately — one cheap idempotent read at two rows. Do not copy this for
// non-idempotent work (the OAuth follow-up's token writes stay on the
// request ctx).
//
// Budget composition, stated so the follow-up doesn't copy the stacking:
// worst case per verb is 5s pre-read + 5s write (verb ctx) + 5s readBack,
// sequential (~15s) with no overall gateway deadline. All three are cheap
// indexed reads/writes at two rows; heavier OAuth writes and the real token
// exchange need one verb-level bound instead of stacked per-call budgets.
func quickCtx() (context.Context, context.CancelFunc) {
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
	id = NormalizedID(id)
	// Length before identity, matching CreateState/ConsumeState/Exchange:
	// an oversized input 400s the same way whether the id is known or
	// not, so the unknown-id path cannot be told apart by which error
	// fires first. Same order as the memory twin.
	if len(tok.RefreshCipher) > MaxStateFieldLen || len(tok.AccessCipher) > MaxStateFieldLen {
		return NewStoreError("connector grant token too long", false)
	}
	if !Known(id) {
		return &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	if strings.TrimSpace(tok.RefreshCipher) == "" {
		return NewStoreError("connector grant needs a refresh token", false)
	}
	// Whitespace-only access is neither a sealed grant nor literal "": the
	// refresher keys missing off "", so a padded value would create a
	// second missing representation that reads as present. Reject it here
	// (same gate as refresh) rather than letting it through to storage.
	if tok.AccessCipher != "" && strings.TrimSpace(tok.AccessCipher) == "" {
		return NewStoreError("connector grant access token is blank", false)
	}
	// Expiry-without-access is a second missing representation: the
	// refresher keys renew-now off zero expiry, and the no-op predicates
	// read a non-zero expiry as grant signal. The gateway zeroes expiry
	// when access is empty (single representation of missing) — the store
	// enforces the same rule instead of relying on the one prod caller.
	if tok.AccessCipher == "" && tok.AccessExpiryUnixMilli != 0 {
		return NewStoreError("connector grant expiry without access token", false)
	}
	// Reject padded ciphers instead of trimming-and-storing: the string
	// the checks see and the bytes the store keeps must be identical,
	// or a direct caller mints an undecryptable "present" grant (same
	// class as the state trim-at-write contract, enforced as a reject
	// here because stored credentials must round-trip byte-exact).
	if tok.RefreshCipher != strings.TrimSpace(tok.RefreshCipher) ||
		(tok.AccessCipher != "" && tok.AccessCipher != strings.TrimSpace(tok.AccessCipher)) {
		return NewStoreError("connector grant token is padded", false)
	}
	// Display defaults on insert only: a token write must never invent
	// status (backfill synthesises disconnected for a status-less doc
	// on read), and without these a crash between SetTokens and
	// SetStatus would leave a display-less row. updatedAt rides along so
	// the crash-window row reads as explicit-off (something happened here)
	// rather than never-touched. Reads project only the display shape, so
	// these columns are inert until the transition write lands the real row.
	now := time.Now().UnixMilli()
	_, err := s.coll.UpdateOne(ctx, bson.D{{Key: "_id", Value: id}}, bson.D{
		{Key: "$set", Value: bson.D{
			{Key: refreshTokenField, Value: tok.RefreshCipher},
			{Key: accessTokenField, Value: tok.AccessCipher},
			{Key: "accessExpiry", Value: tok.AccessExpiryUnixMilli},
		}},
		{Key: "$setOnInsert", Value: bson.D{
			{Key: "displayName", Value: DisplayName(id)},
			{Key: "scopes", Value: ScopesFor(id)},
			{Key: "updatedAt", Value: now},
		}},
	}, options.Update().SetUpsert(true))
	if err != nil {
		s.logf("connectors: token write %s failed: %v", id, err)
		return NewStoreError("connector token write failed", true)
	}
	return nil
}

// GetTokens reads the ciphertext grant. Unknown ids and grant-less rows are
// both NotFound: the caller reconnects either way, and distinguishing them
// would oracle stored-grant existence.
func (s *MongoStore) GetTokens(ctx context.Context, id string) (TokenRow, error) {
	id = NormalizedID(id)
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
		s.logf("connectors: token read %s failed: %v", id, err)
		return TokenRow{}, NewStoreError("connector token read failed", true)
	}
	if strings.TrimSpace(row.Refresh) == "" {
		return TokenRow{}, &StoreError{Msg: "unknown connector " + id, NotFound: true}
	}
	// Returned verbatim by design: size enforcement lives in DecryptToken
	// (len > MaxStateFieldLen errors there), so an oversized artifact from
	// before the cap fails at open time with a precise error instead of
	// being masked as NotFound here.
	return TokenRow{RefreshCipher: row.Refresh, AccessCipher: row.Access, AccessExpiryUnixMilli: row.Expiry}, nil
}

// CreateState records a pending OAuth attempt. States are single-use by
// contract (see ConsumeState); an id collision is a caller bug (400), never
// an overwrite.
func (s *MongoStore) CreateState(ctx context.Context, st OAuthState) error {
	// Normalised like every other verb: SetTokens/GetTokens/Disconnect all
	// fold case+spacing first, and a non-normalised stored row would miss
	// both the Known check here and the Disconnect scrub comparison later.
	st.Connector = NormalizedID(st.Connector)
	// Trimmed before storing: the callback trims query params before
	// ConsumeState, so a padded value stored verbatim would never match a
	// lookup — and a padded verifier would break Exchange at Google after
	// the user already consented. Normalise here, not at read time.
	st.State = strings.TrimSpace(st.State)
	st.Verifier = strings.TrimSpace(st.Verifier)
	if st.State == "" {
		return NewStoreError("oauth state needs an id", false)
	}
	if len(st.State) > MaxStateFieldLen || len(st.Verifier) > MaxStateFieldLen {
		return NewStoreError("oauth state or verifier too long", false)
	}
	if len(st.RedirectURI) > MaxStateFieldLen {
		return NewStoreError("oauth redirect too long", false)
	}
	// Shape-checked like AuthURL/Exchange, not just length-capped: a
	// sub-2048 garbage redirect would otherwise supersede the live
	// attempt and fail only at Exchange — after the user consented.
	// Canonicalised and stored, so stored == sent by construction.
	// Empty means minted before this column existed: allowed, the
	// callback falls back to the live config.
	if st.RedirectURI != "" {
		canonical, err := CanonicalRedirectURL(st.RedirectURI)
		if err != nil {
			return NewStoreError("oauth redirect is invalid", false)
		}
		st.RedirectURI = canonical
	}
	if !Known(st.Connector) {
		return &StoreError{Msg: "unknown connector " + st.Connector, NotFound: true}
	}
	if strings.TrimSpace(st.Verifier) == "" {
		// Fail fast at creation, not after the user completed consent: a
		// verifier-less attempt can never survive Exchange.
		return NewStoreError("oauth state needs a verifier", false)
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
	// create over a cleanup write.
	supCtx, supCancel := quickCtx()
	_, serr := s.states.DeleteMany(supCtx, bson.D{{Key: "connector", Value: st.Connector}})
	supCancel()
	if serr != nil {
		s.logf("connectors: state supersede %s failed: %v", st.Connector, serr)
	}
	_, err := s.states.InsertOne(ctx, st)
	if err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			return NewStoreError("oauth state already exists", false)
		}
		s.logf("connectors: state create failed: %v", err)
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
	// Trimmed like the write path and the gateway: padded input resolves
	// instead of 404ing on whitespace no writer could have stored.
	// Length-gated like the gateway: oversized input answers the same
	// NotFound as unknown, never reaching Mongo as a filter.
	state = strings.TrimSpace(state)
	if len(state) > MaxStateFieldLen {
		return OAuthState{}, &StoreError{Msg: "unknown or expired oauth state", NotFound: true}
	}
	var st OAuthState
	err := s.states.FindOneAndDelete(ctx, bson.D{{Key: "_id", Value: state}}).Decode(&st)
	if err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return OAuthState{}, &StoreError{Msg: "unknown or expired oauth state", NotFound: true}
		}
		s.logf("connectors: state consume failed: %v", err)
		return OAuthState{}, NewStoreError("oauth state consume failed", true)
	}
	if !st.ExpiresAt.IsZero() && !time.Now().Before(st.ExpiresAt) {
		// Boundary == now counts as expired, matching the memory side —
		// pinned there by the injectable-clock test (memory pinned,
		// Mongo wall-clock by design; DB tests need Atlas anyway).
		return OAuthState{}, &StoreError{Msg: "unknown or expired oauth state", NotFound: true}
	}
	return st, nil
}
