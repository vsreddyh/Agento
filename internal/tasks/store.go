// Package tasks: MongoDB-backed personal task manager for the agent.
//
// Collection: tasks. One doc per task with name, description, due date/time,
// estimated minutes, a recurrence (see Repeat) and a parallelable flag
// (true = can run alongside other tasks). There is NO status
// field: a task is open while completedAt is null and done once it is set.
//
// Completed tasks are retained 3 days via expiresAt TTL
// (= completedAt + RetentionDays); open tasks carry no expiresAt and never
// expire. A Repeat is never interpreted here — advancing to the next
// occurrence is purely the agent's job (see pi/skills/task-manager/SKILL.md);
// complete_task only echoes the rule back so the caller can't miss it.
package tasks

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"agento/internal/mongostore"
	"agento/internal/validate"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	coll = "tasks"
	// RetentionDays drives the TTL target for completed tasks.
	RetentionDays = 3
)

var timeRE = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

// StoreError is the domain error: servers catch it and return {ok: false, error}.
type StoreError struct {
	Msg      string
	Conflict bool
	// Mismatch means an idempotency key was REUSED with a different payload
	// (#176). It is neither missing nor a lost revision race, so it gets its own
	// flag rather than being smuggled in as a message prefix — a caller has to be
	// able to distinguish "your key is wrong" from "your fields are wrong", and
	// only one of those is fixed by changing the request.
	Mismatch bool
}

func (e *StoreError) Error() string { return e.Msg }

// conflict reports a lost optimistic-concurrency race. It is a distinct
// shape (not a validation failure) so the HTTP layer can answer 409 and
// the MCP layer can tell the agent to re-read rather than re-send.
func conflict(format string, args ...any) *StoreError {
	return &StoreError{Msg: "conflict: " + fmt.Sprintf(format, args...), Conflict: true}
}

func fail(format string, args ...any) *StoreError {
	return &StoreError{Msg: fmt.Sprintf(format, args...)}
}

// mismatch reports an idempotency key reused with a different payload.
func mismatch(key string) *StoreError {
	return &StoreError{
		Msg: fmt.Sprintf("idempotency key %q was already used with a different request body; "+
			"send a NEW key for a different task, or resend the original body to replay it", key),
		Mismatch: true,
	}
}

// Store is the MongoDB backend for tasks.
type Store struct {
	client *mongoDrv.Client
	db     *mongoDrv.Database
	tasks  *mongoDrv.Collection
}

// New connects to uri/dbName and ensures indexes.
func New(uri, dbName string) (*Store, error) {
	c, db, err := mongostore.Open(uri, dbName)
	if err != nil {
		return nil, err
	}
	s := &Store{client: c, db: db, tasks: db.Collection(coll)}
	if err := s.EnsureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

// FromEnv builds a Store from MONGODB_URI/MONGODB_DB (single root .env).
func FromEnv() (*Store, error) {
	uri, db := mongostore.Env()
	return New(uri, db)
}

// EnsureSchema creates the TTL + query indexes (idempotent).
func (s *Store) EnsureSchema(ctx context.Context) error {
	_, err := s.tasks.Indexes().CreateMany(ctx, []mongoDrv.IndexModel{
		{Keys: bson.D{{Key: "expiresAt", Value: 1}}, Options: options.Index().SetName("ttl_expiresAt").SetExpireAfterSeconds(0)},
		{Keys: bson.D{{Key: "completedAt", Value: 1}}, Options: options.Index().SetName("completedAt_1")},
		{Keys: bson.D{{Key: "due_date", Value: 1}}, Options: options.Index().SetName("due_date_1")},
		// Read by ReconcileRollover once per completed structured repeat (up to 500
		// per run). Without this it is a collection scan per task, which is the
		// difference between the nightly job being free and being the slowest thing
		// in the stack.
		{Keys: bson.D{{Key: "rolled_from", Value: 1}}, Options: options.Index().SetName("rolled_from_1")},
		// The idempotency guarantee (#176). UNIQUE + SPARSE, and both words matter:
		//   - unique, so the database — not a read-then-write in Go — decides who wins
		//     a concurrent retry. The loser gets a duplicate-key error and is handed
		//     the winner's row.
		//   - sparse, so tasks created WITHOUT a key are not all treated as the same
		//     key. A plain unique index here would let exactly one keyless task exist
		//     and reject every subsequent one, which is a far worse bug than the
		//     duplicate this prevents.
		{Keys: bson.D{{Key: idempotencyKeyField, Value: 1}},
			Options: options.Index().SetName("idempotency_key_uniq").SetUnique(true).SetSparse(true)},
	})
	if err != nil {
		return err
	}
	// The audit collection's indexes live here too, deliberately. They were a
	// separate EnsureAuditIndex that New() never called — so `task_mutations` shipped
	// with NO indexes at all, and the only two things that read it (the 90-day
	// retention prune on `at`, and "what happened to this task" on `task_id`) were
	// collection scans against a collection that only grows. A second function is a
	// second place to forget the call, so there is now one place to add an index and
	// one place that runs.
	_, err = s.db.Collection(auditCollection).Indexes().CreateMany(ctx, []mongoDrv.IndexModel{
		{Keys: bson.D{{Key: "at", Value: 1}}, Options: options.Index().SetName("at_1")},
		{Keys: bson.D{{Key: "task_id", Value: 1}}, Options: options.Index().SetName("task_id_1")},
	})
	return err
}

func checkDue(dueDate, dueTime string) error {
	if dueDate != "" {
		if _, err := validate.CheckDay(dueDate); err != nil {
			return fail("due_date: %s", err.Error())
		}
	}
	if dueTime != "" && !timeRE.MatchString(dueTime) {
		return fail("due_time must be HH:MM (24h), got '%s'", dueTime)
	}
	if dueTime != "" && dueDate == "" {
		return fail("due_time requires due_date")
	}
	return nil
}

// isSkipped is the ONE rule for "was this occurrence skipped, rather than done?".
//
// It used to be written twice with different rules: toDoc derived the `skipped` boolean
// from `skippedAt != nil`, while resolvedStateErr required a primitive.DateTime whose
// value was non-zero. Those disagree on any row where the field is present but is not a
// real timestamp — a zero DateTime, or a value of some other type — so such a task would
// be REPORTED as skipped in every response and simultaneously produce "already
// completed" when you tried to skip or complete it again. A row like that cannot be
// written through the API, which is exactly why the divergence survived: both halves
// were individually reasonable and neither was ever compared against the other.
//
// The stricter rule wins on purpose. A zero DateTime is the epoch, not a moment someone
// decided to skip something, so crediting the occurrence with that decision would be
// inventing it.
func isSkipped(doc map[string]any) bool {
	// Accepts BOTH representations, and it has to: the raw BSON doc holds a
	// primitive.DateTime, while toDoc renders dates to an ISO string for the API. So a
	// caller holding a rendered task and a caller holding a stored one see the same
	// field as different Go types.
	//
	// This is why the check cannot simply be "assert primitive.DateTime and compare to
	// zero", which is what the divergence was originally resolved to. That version is
	// correct for the stored doc and SILENTLY WRONG for the rendered one: every response
	// would carry `skipped: false` even for a genuinely skipped occurrence, and nothing
	// would fail — no compile error, no test touching a raw doc. A client filtering on
	// `skipped` would quietly see no skips at all.
	//
	// The empty string is Reopen's own clearing value (`skippedAt: ""`), so it must read
	// as NOT skipped, exactly as a cleared DateTime must.
	switch v := doc["skippedAt"].(type) {
	case primitive.DateTime:
		return v != 0
	case string:
		return v != ""
	default:
		// Absent, nil, or a type that is neither: not a skip this code wrote.
		return false
	}
}

// TaskContractVersion is the version of the task response contract (#185).
//
// Every task response carries it as `contract_version` (see toDoc), and the app
// asserts it alongside the fields it sent. Bump it whenever the contract changes
// — a renamed key, a new shape, a different rollover envelope — and update the
// app's TASK_CONTRACT_VERSION in the same change, so a stale install on either
// side reads as a version disagreement instead of silent wrong data. That is the
// mechanical form of the review that caught the 4.7.0 complete-response break:
// it must not depend on a reviewer happening to look.
const TaskContractVersion = 1

func toDoc(doc bson.M) map[string]any {
	out := map[string]any{}
	for k, v := range doc {
		if k == "_id" {
			if o, ok := v.(primitive.ObjectID); ok {
				out["id"] = o.Hex()
			}
			continue
		}
		if dt, ok := v.(primitive.DateTime); ok {
			out[k] = dt.Time().UTC().Format(time.RFC3339)
			continue
		}
		out[k] = v
	}
	// `idempotency_key` and `source` are STORED on the doc, so they appear in every
	// response. Deliberate, and worth stating because it looks like a leak:
	//   - the key is client-generated (a UUID), not a secret, and echoing it back is
	//     what lets a client correlate a retry with the task it already created
	//     without keeping its own log;
	//   - `source` is the only record of WHICH caller wrote a row, which is the whole
	//     reason it exists — a task changed by the agent and by the app are otherwise
	//     indistinguishable, because both authenticate with the same password;
	//   - both are needed by the reconciler and by anyone reading `task_mutations`,
	//     and hiding them from the API while leaving them in the database would only
	//     mean the next reader adds a second, inconsistent path to the same facts.
	// The one thing that IS withheld is the bearer token: it never reaches Mongo, and
	// where the limiter needs to distinguish callers it stores only a short hash.

	// The fingerprint is stripped: it is an INTERNAL field of the idempotency
	// mechanism, no client has any use for a digest of the body it just sent, and it
	// is the only one of these three that says nothing about the task.
	//
	// Note what this is NOT coupled to: byIdempotencyKey reads the stored fingerprint
	// from the RAW BSON, not from here. Had it read it from the rendered doc, adding
	// this line would have silently switched off the payload-mismatch check — a
	// cosmetic edit disabling a correctness guarantee. TestCreateWithKeyRejectsADifferentBody
	// is the test that catches that, and it is why the raw read exists.
	if _, present := out[idempotencyFingerprintField]; present {
		delete(out, idempotencyFingerprintField)
	}

	// `skipped` is derived, not stored, so it cannot disagree with `skippedAt`
	// (#209). A skip sets completedAt so the occurrence leaves the open list — which
	// means "is it done?" and "was it done?" are different questions and a client
	// reading only completedAt would report work that never happened as finished.
	// Deriving it here means every response shape carries it, including ones an old
	// client already parses: it ignores a key it does not know.
	out["skipped"] = isSkipped(out)
	// The contract version this response speaks (#185). Additive on purpose: an
	// app that has never heard of it ignores the key, while an app that asserts
	// it can tell "old server" from "genuine zero" instead of defaulting every
	// missing field and rendering a structured cadence as a one-shot.
	out["contract_version"] = TaskContractVersion
	// Backfill keys that pre-mandatory docs lack, so every response
	// speaks the same contract (readers default the same way).
	// due_time "" is the "created before times were required" sentinel:
	// it is never invented here, and such a task becomes valid again the
	// moment it is written with a real time (Update rejects an explicit
	// ""), so legacy rows heal on first edit instead of needing a
	// migration that would guess a user's schedule.
	if _, ok := out["description"]; !ok {
		out["description"] = ""
	}
	if _, ok := out["due_date"]; !ok {
		out["due_date"] = ""
	}
	if _, ok := out["due_time"]; !ok {
		out["due_time"] = ""
	}
	if _, ok := out["estimated_minutes"]; !ok {
		out["estimated_minutes"] = 0
	}
	if _, ok := out["repeat_rule"]; !ok {
		out["repeat_rule"] = ""
	}
	if _, ok := out["revision"]; !ok {
		out["revision"] = 0
	}
	// Recurrence backfill: pre-4.6 docs carry only the free-text
	// repeat_rule, so the mode is inferred (text = custom) and the
	// structured keys come back neutral. Every response speaks the same
	// contract whether or not the 4.6 migration has run.
	if _, ok := out["repeat_every"]; !ok {
		out["repeat_every"] = 0
	}
	if _, ok := out["repeat_unit"]; !ok {
		out["repeat_unit"] = ""
	}
	if _, ok := out["repeat_custom"]; !ok {
		rule, _ := out["repeat_rule"].(string)
		out["repeat_custom"] = strings.TrimSpace(rule) != ""
	}
	if _, ok := out["parallelable"]; !ok {
		out["parallelable"] = false
	}
	// `rolled_from` is stripped: it is INTERNAL linkage between a task and the
	// occurrence it produced, and it lands on exactly the successor docs a user has no
	// reason to be reading. Exposing it invites a client to reconstruct rollover chains
	// from the API and then depend on that — reimplementing the reconciler's job against
	// a field that has no contract.
	//
	// Safe to strip, and deliberately checked before doing so: NOTHING reads this off a
	// rendered doc. The reconciler reads it from a raw projection (parentsWithSuccessors),
	// because that too is linkage it must not infer from a display document.
	// TestReconcileStillWorksWithRolledFromHidden pins that condition, so a future
	// reader who renders it again finds out immediately.
	if _, present := out[rolledFromField]; present {
		delete(out, rolledFromField)
	}
	return out
}

// Create inserts an open task. Every field except the repeat is required
// (an all-zero Repeat = one-shot task); parallelable marks tasks that can
// run alongside other tasks.
//
// A non-empty key makes the insert idempotent (#176): a repeat of the same key
// returns the ORIGINAL task instead of creating a second one.
func (s *Store) Create(ctx context.Context, name, description, dueDate, dueTime string, estimatedMinutes *int, rep Repeat, parallelable *bool) (map[string]any, error) {
	doc, _, err := s.CreateWithKey(ctx, name, description, dueDate, dueTime, estimatedMinutes, rep, parallelable, "", "", "")
	return doc, err
}

// Field names for #176. Declared here rather than inline so the write and the
// unique index below cannot drift apart.
const (
	// idempotencyKeyField is the client's key. Unique and SPARSE: most tasks are
	// created without one (the MCP path does not use it), and a unique index over
	// missing values would treat every one of them as the same key and reject all
	// but the first. Sparse means "absent is not a value".
	idempotencyKeyField = "idempotency_key"
	// sourceField records WHICH caller wrote the row — app vs MCP — so a duplicate
	// or a surprise is traceable. health-api and the agent are indistinguishable at
	// the auth layer (one shared password), which is exactly why this was missing.
	sourceField = "source"
	// idempotencyFingerprintField is a digest of the request payload, written in the
	// SAME insert as the key. A key alone cannot detect reuse-with-different-body:
	// the store would hand back the original task with a 200 and no signal, so the
	// caller would believe it created something it did not. See CreateWithKey.
	idempotencyFingerprintField = "idempotency_fingerprint"
	// IdempotencyKeyMax caps the client key, in BYTES (what BSON and the unique index
	// actually compare). Over the limit is an error, never a truncation — see
	// CreateWithKey.
	IdempotencyKeyMax = 200
)

// CreateWithKey is Create plus a client-supplied idempotency key and the caller's
// identity, both recorded on the doc.
//
// WHY THE KEY IS RESOLVED BY THE DATABASE AND NOT BY A LOOKUP FIRST: a
// read-then-insert has a window between them, and the failure it produces is
// exactly the one this exists to prevent. A client retrying after a timeout is the
// case that matters — the first request may still be in flight, so a lookup finds
// nothing and the retry inserts a second task. `Complete` is already idempotent by
// accident (it filters on completedAt: nil); `Create` was not, so a double-tapped
// button or a flaky connection produced duplicates with no way to tell them apart
// or remove them in bulk.
//
// So the unique index does the work: the loser of a concurrent race gets a
// duplicate-key error from the INSERT and is handed the winner's row. There is no
// window, because the constraint and the insert are the same operation.
//
// `replayed` is returned true only for that second-and-subsequent case, so a
// caller can answer "already saved" rather than implying it created something.
func (s *Store) CreateWithKey(ctx context.Context, name, description, dueDate, dueTime string, estimatedMinutes *int, rep Repeat, parallelable *bool, idemKey, source, fingerprint string) (map[string]any, bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, false, fail("name is required")
	}
	description = strings.TrimSpace(description)
	if description == "" {
		return nil, false, fail("description is required")
	}
	dueDate = strings.TrimSpace(dueDate)
	if dueDate == "" {
		return nil, false, fail("due_date is required (YYYY-MM-DD)")
	}
	dueTime = strings.TrimSpace(dueTime)
	if dueTime == "" {
		return nil, false, fail("due_time is required (HH:MM)")
	}
	if err := checkDue(dueDate, dueTime); err != nil {
		return nil, false, err
	}
	if estimatedMinutes == nil {
		return nil, false, fail("estimated_minutes is required")
	}
	if *estimatedMinutes < 0 {
		return nil, false, fail("estimated_minutes must be >= 0")
	}
	if parallelable == nil {
		return nil, false, fail("parallelable is required")
	}
	rep = rep.Normalize()
	if err := rep.Validate(); err != nil {
		return nil, false, err
	}
	now := primitive.NewDateTimeFromTime(time.Now().UTC())
	doc := bson.M{
		"name": name, "description": description,
		"due_date": dueDate, "due_time": dueTime,
		"estimated_minutes": *estimatedMinutes,
		"parallelable":      *parallelable,
		"revision":          0,
		"completedAt":       nil, "createdAt": now,
	}
	for k, v := range rep.docs() {
		doc[k] = v
	}
	idemKey = strings.TrimSpace(idemKey)
	if idemKey != "" {
		// REJECTED, not truncated. Truncating to 200 bytes makes a 250-byte key equal
		// to the 200-byte key that is its own prefix, so two genuinely different
		// requests collide on the unique index and the second is handed the first's
		// task as a "replay". That is the silent wrong answer this whole mechanism
		// exists to avoid, reached by the fix for a different problem — an overlong key
		// is a client bug, and saying so is far cheaper than inventing a task.
		if len(idemKey) > IdempotencyKeyMax {
			return nil, false, fail("idempotency key is %d bytes, over the %d-byte limit; "+
				"send a shorter key (a UUID is 36)", len(idemKey), IdempotencyKeyMax)
		}
		doc[idempotencyKeyField] = idemKey
		// Same insert as the key, so a task can never carry one without the other.
		// An empty fingerprint disables the check rather than failing the write: a
		// caller that has no body to digest (the MCP path) still gets idempotency.
		if fingerprint = truncateRunes(fingerprint, 64); fingerprint != "" {
			doc[idempotencyFingerprintField] = fingerprint
		}
	}
	if source = strings.TrimSpace(source); source != "" {
		// Truncated rather than rejected, deliberately — the OPPOSITE call from the
		// key above, and the asymmetry is the point. `source` is a human label in an
		// audit log: two long labels colliding degrades a log line and nothing else.
		// `idempotency_key` is an identity that decides whether a request is a replay,
		// so two keys colliding there silently returns the wrong task.
		source = truncateRunes(source, 40)
		doc[sourceField] = source
	}
	out, err := s.insert(ctx, doc)
	if err == nil {
		return out, false, nil
	}
	// The unique index on idempotency_key is what makes this safe: the loser of a
	// concurrent race lands here and is handed the winner's row, instead of
	// creating the duplicate this was added to prevent.
	if idemKey == "" || !mongoDrv.IsDuplicateKeyError(err) {
		return nil, false, err
	}
	found, getErr := s.byIdempotencyKey(ctx, idemKey)
	if getErr != nil || found == nil {
		// The insert reported a duplicate but the row cannot be read back. Returning
		// the original error is more useful than a silent success.
		return nil, false, err
	}
	// Reuse with a DIFFERENT body is not a replay, and answering 200 with the
	// original task would be the worst possible reply: the caller would believe it
	// created a task with the fields it just sent, and the task it gets back has
	// none of them. Refuse instead, and say which of the two mistakes it is.
	//
	// Compared only when BOTH sides have a fingerprint. A row written before this
	// field existed has none, and so does a caller that sent no body to digest;
	// treating "unknown" as "different" would reject every legacy key on its first
	// retry, which is the opposite of what an idempotency key is for.
	stored := found.storedFingerprint
	if fingerprint != "" && stored != "" && stored != fingerprint {
		return nil, false, mismatch(idemKey)
	}
	return found.doc, true, nil
}

// replayedTask is a task found by its idempotency key, carrying the stored fingerprint
// SEPARATELY from the rendered doc.
type replayedTask struct {
	doc map[string]any
	// storedFingerprint is read from the RAW document, never from `doc`.
	storedFingerprint string
}

// byIdempotencyKey finds a task by its idempotency key. Used ONLY to resolve the
// duplicate-key race above, never to pre-check — a pre-check is exactly the
// read-then-write window this design exists to avoid.
//
// It deliberately does NOT go through toDoc for the fingerprint. The fingerprint is
// an internal field of the idempotency mechanism and is stripped from API responses,
// so reading it back off a rendered doc meant the mismatch check silently stopped
// working the moment anyone tidied that list — a cosmetic change quietly disabling a
// correctness guarantee, with no test failing. Pulling it from the raw BSON here
// means the two concerns cannot be coupled by a future edit.
func (s *Store) byIdempotencyKey(ctx context.Context, key string) (*replayedTask, error) {
	var doc bson.M
	if err := s.tasks.FindOne(ctx, bson.M{idempotencyKeyField: key}).Decode(&doc); err != nil {
		if errors.Is(err, mongoDrv.ErrNoDocuments) {
			return nil, nil
		}
		return nil, err
	}
	stored, _ := doc[idempotencyFingerprintField].(string)
	return &replayedTask{doc: toDoc(doc), storedFingerprint: stored}, nil
}

// insert writes a prepared document and renders it. Split out of Create so that
// fields which must land ATOMICALLY with the insert can be added by callers that
// need them — specifically rollOver's `rolled_from` back-link (#180), which as a
// separate UpdateOne left a crash window where the successor existed but was
// unlinked, so the next nightly reconciler reported a gap and sent a human to
// create a task that already existed.
//
// An extra field added here is part of the same write. There is no second call to
// lose, so nothing has to reconcile afterwards.
func (s *Store) insert(ctx context.Context, doc bson.M) (map[string]any, error) {
	res, err := s.tasks.InsertOne(ctx, doc)
	if err != nil {
		return nil, err
	}
	doc["_id"] = res.InsertedID
	return toDoc(doc), nil
}

// List cap policy, mirroring the projects store: an unbounded Find grows
// with the collection, and every caller of List is a phone, a widget or a
// nag loop. limit <= 0 means the default; anything above the ceiling is
// clamped, never rejected. (#171: this pair of clamps wants one home.)
const (
	DefaultLimit = 200
	MaxLimit     = 500
)

// List returns tasks filtered by state. state: "open" (default), "done", "all".
// overdue=true keeps only open tasks with due_date before today.
// The second return value reports truncation: a +1 probe row is fetched and
// dropped, so callers can tell "exactly N" from "N of many" without a
// second query (#168).
func (s *Store) List(ctx context.Context, state string, overdue bool, search string, limit int) ([]map[string]any, bool, error) {
	if state == "" {
		state = "open"
	}
	filt := bson.M{}
	switch state {
	case "open":
		filt["completedAt"] = nil
	case "done":
		// $exists: reopened tasks have the field $unset (missing), and
		// bare $ne:null matches missing fields — both guards needed.
		filt["completedAt"] = bson.M{"$ne": nil, "$exists": true}
	case "all":
	default:
		return nil, false, fail("state must be open|done|all, got '%s'", state)
	}
	if overdue && state != "open" {
		return nil, false, fail("overdue only applies to state=open, got state='%s'", state)
	}
	if overdue {
		filt["completedAt"] = nil
		filt["due_date"] = bson.M{"$ne": "", "$lt": time.Now().Format("2006-01-02")}
	}
	if search = strings.TrimSpace(search); search != "" {
		// QuoteMeta: raw user input must never reach the regex engine.
		rx := regexp.QuoteMeta(search)
		filt["$or"] = []bson.M{
			{"name": bson.M{"$regex": rx, "$options": "i"}},
			{"description": bson.M{"$regex": rx, "$options": "i"}},
		}
	}
	lim := mongostore.ClampLimit(limit, DefaultLimit, MaxLimit)
	cur, err := s.tasks.Find(ctx, filt, options.Find().SetSort(bson.D{{Key: "due_date", Value: 1}, {Key: "createdAt", Value: 1}}).SetLimit(lim+1))
	if err != nil {
		return nil, false, err
	}
	defer cur.Close(ctx)
	out := []map[string]any{}
	for cur.Next(ctx) {
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			return nil, false, err
		}
		out = append(out, toDoc(doc))
	}
	if err := cur.Err(); err != nil {
		return nil, false, err
	}
	if int64(len(out)) > lim {
		return out[:int(lim)], true, nil
	}
	return out, false, nil
}

// Get fetches one task by hex id.
func (s *Store) Get(ctx context.Context, id string) (map[string]any, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("bad id '%s'", id)
	}
	var doc bson.M
	if err := s.tasks.FindOne(ctx, bson.M{"_id": oid}).Decode(&doc); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return nil, fail("unknown task '%s'", id)
		}
		return nil, err
	}
	return toDoc(doc), nil
}

// Update edits mutable fields of any task (open or done). Supplied values
// must satisfy the same mandatory rules as Create (empty description /
// due fields are rejected, not cleared); the recurrence stays clearable
// ("" / 0 = one-shot). Absent keys are untouched; due fields and the
// recurrence's four keys are validated together.
//
// expected_revision is the optimistic-concurrency guard (#184): when the
// caller sends the revision it read, a mismatch means someone else wrote
// first and the update is rejected instead of silently overwriting. Absent
// means unchecked, so old callers keep working. Every successful mutation
// bumps the revision, so the number the next reader sees is always fresh.
func (s *Store) Update(ctx context.Context, id string, fields map[string]any) (map[string]any, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("bad id '%s'", id)
	}
	var cur bson.M
	if err := s.tasks.FindOne(ctx, bson.M{"_id": oid}).Decode(&cur); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return nil, fail("unknown task '%s'", id)
		}
		return nil, err
	}
	// The guard is checked twice, on purpose. The comparison here gives
	// the precise message (which revision was wanted, which is stored);
	// the filter below makes it atomic, so two holders of the same number
	// cannot both win and a concurrent $inc cannot slip between.
	stored, _ := mongostore.ToInt(cur["revision"])
	guarded, want := false, 0
	if v, ok := fields["expected_revision"]; ok && v != nil {
		if f, isFloat := v.(float64); isFloat && f != math.Trunc(f) {
			return nil, fail("expected_revision must be an integer >= 0")
		}
		want, ok = mongostore.ToInt(v)
		if !ok || want < 0 {
			return nil, fail("expected_revision must be an integer >= 0")
		}
		if want != stored {
			return nil, conflict("task changed since revision %d (now %d) — reload and retry", want, stored)
		}
		guarded = true
	}
	set := bson.M{}
	strField := func(key string) (string, bool) {
		v, ok := fields[key]
		if !ok {
			return "", false
		}
		str, ok := v.(string)
		if !ok {
			return "", false
		}
		return str, true
	}
	// Wrong types fail loudly instead of becoming mystery no-ops
	// (strField above treats them as absent). JSON null still counts
	// as absent, matching the HTTP layer's convention.
	for _, k := range []string{"description", "due_date", "due_time", "repeat_rule"} {
		if v, ok := fields[k]; ok && v != nil {
			if _, ok := v.(string); !ok {
				return nil, fail("%s must be a string", k)
			}
		}
	}
	if v, ok := fields["name"]; ok && v != nil {
		name, _ := v.(string)
		if strings.TrimSpace(name) == "" {
			return nil, fail("name is required")
		}
		set["name"] = strings.TrimSpace(name)
	}
	if str, ok := strField("description"); ok {
		if strings.TrimSpace(str) == "" {
			return nil, fail("description is required")
		}
		set["description"] = strings.TrimSpace(str)
	}
	// Due fields flow through only when the caller sent them, so a
	// no-change update stays a no-op and the len(set)==0 path can fire.
	dueDate, _ := cur["due_date"].(string)
	dueTime, _ := cur["due_time"].(string)
	if str, ok := strField("due_date"); ok {
		if strings.TrimSpace(str) == "" {
			return nil, fail("due_date is required (YYYY-MM-DD)")
		}
		dueDate = strings.TrimSpace(str)
		set["due_date"] = dueDate
	}
	if str, ok := strField("due_time"); ok {
		if strings.TrimSpace(str) == "" {
			return nil, fail("due_time is required (HH:MM)")
		}
		dueTime = strings.TrimSpace(str)
		set["due_time"] = dueTime
	}
	if err := checkDue(dueDate, dueTime); err != nil {
		return nil, err
	}
	if v, ok := fields["estimated_minutes"]; ok && v != nil {
		n, ok := mongostore.ToInt(v)
		if !ok || n < 0 {
			return nil, fail("estimated_minutes must be >= 0")
		}
		set["estimated_minutes"] = n
	}
	// Recurrence: merge whatever was supplied onto the stored one and
	// validate the result, so the four keys can never end up describing
	// two different rules. Any of the four present means "edit the
	// recurrence"; none of them means leave it alone.
	rep, repTouched, err := mergeRepeat(cur, fields, strField)
	if err != nil {
		return nil, err
	}
	if repTouched {
		for k, v := range rep.docs() {
			set[k] = v
		}
	}
	if v, ok := fields["parallelable"]; ok && v != nil {
		b, ok := mongostore.ToBool(v)
		if !ok {
			return nil, fail("parallelable must be a boolean")
		}
		set["parallelable"] = b
	}
	// Nothing to change means nothing to write: without this the bump
	// below would mint a new revision for a read. Guarded, the revision
	// is still re-verified with a fresh read — otherwise a write landing
	// between our read and this return reports success to a stale editor.
	if len(set) == 0 {
		if !guarded {
			return toDoc(cur), nil
		}
		var latest bson.M
		if ferr := s.tasks.FindOne(ctx, bson.M{"_id": oid}).Decode(&latest); ferr != nil {
			return nil, fail("unknown task '%s'", id)
		}
		if now, _ := mongostore.ToInt(latest["revision"]); now != want {
			return nil, conflict("task changed since revision %d (now %d) — reload and retry", want, now)
		}
		return toDoc(latest), nil
	}
	update := bson.M{"$set": set, "$inc": bson.M{"revision": 1}}
	filt := bson.M{"_id": oid}
	if guarded {
		// A doc that predates revisions has no field but reads as 0
		// (toDoc backfill): matching only revision:0 would false-conflict
		// it forever, unretryably, against its own displayed number.
		if want == 0 {
			filt["$or"] = []bson.M{{"revision": 0}, {"revision": bson.M{"$exists": false}}}
		} else {
			filt["revision"] = want
		}
	}
	res, err := s.tasks.UpdateOne(ctx, filt, update)
	if err != nil {
		return nil, err
	}
	if res.MatchedCount == 0 {
		// Guarded and unmatched: someone won the race after our read.
		// Re-read to name the current revision rather than guessing.
		// (Unguarded cannot miss: the id was just read above.)
		var latest bson.M
		if ferr := s.tasks.FindOne(ctx, bson.M{"_id": oid}).Decode(&latest); ferr != nil {
			return nil, fail("unknown task '%s'", id)
		}
		now, _ := mongostore.ToInt(latest["revision"])
		return nil, conflict("task changed since revision %d (now %d) — reload and retry", want, now)
	}
	return s.Get(ctx, id)
}

// mergeRepeat folds the supplied recurrence fields onto the task's stored
// recurrence and validates the result. touched is false when the caller
// sent none of the four keys, so an unrelated edit leaves the rule alone.
//
// One compatibility rule lives here: a client that sends repeat_rule ""
// with no structured keys is asking to clear the rule (the only way a
// pre-4.6 client can express that), so the whole recurrence goes — not
// just the text, which would leave a cadence the client cannot see.
func mergeRepeat(cur bson.M, fields map[string]any, strField func(string) (string, bool)) (Repeat, bool, error) {
	present := func(keys ...string) bool {
		for _, k := range keys {
			if v, ok := fields[k]; ok && v != nil {
				return true
			}
		}
		return false
	}
	structuredSent := present("repeat_every", "repeat_unit", "repeat_custom")
	if !present("repeat_rule", "repeat_every", "repeat_unit", "repeat_custom") {
		return Repeat{}, false, nil
	}
	rep := repeatFromDoc(cur)
	// Wrong types fail loudly rather than reading as absent. Fractionals
	// are rejected, not truncated: 3.5 is a caller bug, and the store is
	// the last layer that can say so (same guard as expected_revision).
	if v, ok := fields["repeat_every"]; ok && v != nil {
		if f, isFloat := v.(float64); isFloat && f != math.Trunc(f) {
			return rep, true, fail("repeat_every must be an integer %d-%d", RepeatEveryMin, RepeatEveryMax)
		}
		n, ok := mongostore.ToInt(v)
		if !ok {
			return rep, true, fail("repeat_every must be an integer %d-%d", RepeatEveryMin, RepeatEveryMax)
		}
		rep.Every = n
	}
	if str, ok := strField("repeat_unit"); ok {
		rep.Unit = strings.TrimSpace(str)
	}
	if v, ok := fields["repeat_custom"]; ok && v != nil {
		b, ok := mongostore.ToBool(v)
		if !ok {
			return rep, true, fail("repeat_custom must be a boolean")
		}
		rep.Custom = b
	}
	if str, ok := strField("repeat_rule"); ok {
		rep.Text = strings.TrimSpace(str)
	}
	// "No text, no structured keys" from an older client = clear it all.
	if rep.Text == "" && !structuredSent && (rep.Every != 0 || rep.Unit != "" || rep.Custom) {
		rep = Repeat{}
	}
	rep = rep.Normalize()
	if err := rep.Validate(); err != nil {
		return rep, true, err
	}
	return rep, true, nil
}

// Complete marks a task done: sets completedAt + expiresAt
// (= completedAt + RetentionDays, TTL target).
//
// A task with a **structured** cadence ("every 3 days") rolls itself over
// here: the next occurrence is created with the date advanced and every
// other field carried over, so nobody is asked to pick a date the server
// can already compute. A **custom** condition is left to the caller —
// "every 3rd Friday" and "end of every month" need the user or the agent,
// and the server will not guess. One-shot tasks roll over to nothing.
//
// Returns the completed doc, the new task when one was created, and WHY one was
// not. That third value is the point of #180: the caller used to see `next == nil`
// and could not distinguish "one-shot, nothing to do" from "repeats, but the
// rollover failed", so a task that had silently stopped recurring was reported as
// an ordinary completion.
//
// It DISCARDS the field-specific reason a rollover failed. Use CompleteDetail if you need
// to tell the user WHICH field to fix — the two transports do, and a caller that picks
// this one by accident gets a `RolloverFailed` with no explanation attached.
func (s *Store) Complete(ctx context.Context, id string) (map[string]any, map[string]any, Rollover, error) {
	doc, next, reason, _, err := s.CompleteDetail(ctx, id)
	return doc, next, reason, err
}

// CompleteDetail is Complete plus the FIELD-SPECIFIC reason a rollover failed, e.g.
// "cannot roll over: due_time is required (HH:MM)".
//
// It exists because the detail was being thrown away on the live path: Complete logged
// the error and returned only RolloverFailed, so `needs_attention` reached the user as
// the generic "this task was marked done or skipped, but creating its next occurrence
// FAILED" — with no indication of which field to open. The nightly reconciler already
// carried the detail (it is what made its gaps actionable), so the person who fixed it
// in the morning had to wait for the next run to learn what the error already knew.
//
// A separate method rather than a fifth return value: four production callers want the
// detail, but roughly twenty test call sites discard three of the four values already,
// and widening the signature would churn every one of them to serve a need only the
// transports have.
func (s *Store) CompleteDetail(ctx context.Context, id string) (map[string]any, map[string]any, Rollover, string, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, nil, "", "", fail("bad id '%s'", id)
	}
	now := time.Now().UTC()
	res, err := s.tasks.UpdateOne(ctx,
		bson.M{"_id": oid, "completedAt": nil},
		bson.M{
			"$set": bson.M{
				"completedAt": primitive.NewDateTimeFromTime(now),
				"expiresAt":   primitive.NewDateTimeFromTime(now.AddDate(0, 0, RetentionDays)),
			},
			"$inc": bson.M{"revision": 1},
		})
	if err != nil {
		return nil, nil, "", "", err
	}
	if res.MatchedCount == 0 {
		// HEAD wins: stateOnMiss/resolvedStateErr (#209) name the task's ACTUAL state,
		// where the incoming side is the older inline version that only knew "already
		// completed" — the same defect #209 fixed. Returns widened to CompleteDetail's
		// five values.
		doc, serr := s.stateOnMiss(ctx, oid, id)
		if serr != nil {
			return nil, nil, "", "", serr
		}
		return nil, nil, "", "", resolvedStateErr(doc, id)
	}
	done, err := s.Get(ctx, id)
	if err != nil {
		return nil, nil, "", "", err
	}
	next, reason, rerr := s.rollOver(ctx, done)
	if rerr != nil {
		// The task is already marked done, so a failed rollover must not
		// make the completion look like it failed. It is reported as
		// RolloverFailed rather than swallowed, because that reason is what
		// tells the caller — and now the reconciler — that the repeat has
		// stopped. Logged too, because the reconciler runs nightly and the
		// user may act sooner.
		log.Printf("task %s completed but rollover failed: %v", id, rerr)
		return done, nil, RolloverFailed, rerr.Error(), nil
	}
	return done, next, reason, "", nil
}

// stateOnMiss fetches a task that a resolve verb could not claim, because the
// update matched no rows. Returns the stored doc so the caller can say WHICH state it
// is actually in, or an error only when the task does not exist at all.
func (s *Store) stateOnMiss(ctx context.Context, oid primitive.ObjectID, id string) (bson.M, *StoreError) {
	var doc bson.M
	if err := s.tasks.FindOne(ctx, bson.M{"_id": oid}).Decode(&doc); err != nil {
		return nil, fail("unknown task '%s'", id)
	}
	return doc, nil
}

// resolvedStateErr names the state a task is ACTUALLY in when complete_task or
// skip_task finds it already resolved.
//
// Both verbs land here, which is the point. Skip used to distinguish "already
// skipped" from "already completed" while Complete did not, so completing a skipped
// occurrence reported it as DONE — a false record of work that never happened, which
// is the exact thing #209 exists to stop, reached from the other direction. And the
// two states need different remedies, so the message that names the wrong one sends
// the reader to the wrong verb.
//
// Kept as one function rather than a check copied into each miss path: a rule written
// twice is a rule that will be updated once.
func resolvedStateErr(doc bson.M, id string) *StoreError {
	if isSkipped(doc) {
		return fail("task '%s' is already skipped, not done — reopen_task returns it to "+
			"the open list if you now mean to do it", id)
	}
	return fail("task '%s' is already completed", id)
}

// Skip resolves ONE occurrence of a task as skipped rather than done (#209).
//
// The reason this is a verb and not a note: asked to "skip the skippable tasks
// for tonight", an agent with only complete/reopen/delete has no correct action,
// and the nearest available mutation is `complete` — so skips were being recorded
// as completions. Five of them, once, on work that never happened. Because those
// rows TTL away after RetentionDays, the false history was erased rather than
// corrected and the user never got a chance to notice.
//
// What skip does, precisely:
//   - sets `skippedAt`, so the record says SKIPPED and not done — that is the
//     whole point, and it is what `skipped` in the response reads;
//   - also sets `completedAt`, because the occurrence is RESOLVED: it must leave
//     the open list or the user is asked again tonight. Skipping is not "leave it
//     open" — that is what a task the user intends to do today already is, and
//     conflating the two would put skipped chores back on tonight's list;
//   - advances a STRUCTURED cadence, exactly as Complete does. Skipping tonight is
//     not skipping the habit.
//
// A one-shot or a custom repeat has no next occurrence to advance to; `reason`
// says which, and the caller owns the custom case (same contract as Complete).
//
// `reason` is the user's words, kept verbatim and capped — "out of time", "doing it
// tomorrow". It is the difference between a skip and a quiet disappearance, and it
// is the only part of this the user would miss.
//
// `skipReason` is set UNCONDITIONALLY, so a skip with no reason stores "" rather
// than leaving the field out. That is the point: its presence alongside the
// timestamp is what marks the occurrence as skipped, and it is `$set` in the SAME
// atomic update as `skippedAt`, so the two can never disagree. A task that was
// never skipped has no `skipReason` key at all, which is the other half of the
// distinction. Omitting the empty case would save a few bytes per row and make a
// skipped-without-reason task indistinguishable from a completed one in that field
// alone.
func (s *Store) Skip(ctx context.Context, id, reason string) (map[string]any, map[string]any, Rollover, error) {
	doc, next, r, _, err := s.SkipDetail(ctx, id, reason)
	return doc, next, r, err
}

// SkipDetail is Skip plus the field-specific reason a rollover failed. Same rationale as
// CompleteDetail: the detail was logged and discarded, so a skip that stopped a repeat
// reached the agent as a generic "FAILED" with no field named.
func (s *Store) SkipDetail(ctx context.Context, id, reason string) (map[string]any, map[string]any, Rollover, string, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, nil, "", "", fail("bad id '%s'", id)
	}
	now := time.Now().UTC()
	res, err := s.tasks.UpdateOne(ctx,
		bson.M{"_id": oid, "completedAt": nil},
		bson.M{
			"$set": bson.M{
				"completedAt": primitive.NewDateTimeFromTime(now),
				"skippedAt":   primitive.NewDateTimeFromTime(now),
				"skipReason":  truncSkipReason(reason),
				"expiresAt":   primitive.NewDateTimeFromTime(now.AddDate(0, 0, RetentionDays)),
			},
			"$inc": bson.M{"revision": 1},
		})
	if err != nil {
		return nil, nil, "", "", err
	}
	if res.MatchedCount == 0 {
		doc, serr := s.stateOnMiss(ctx, oid, id)
		if serr != nil {
			return nil, nil, "", "", serr
		}
		return nil, nil, "", "", resolvedStateErr(doc, id)
	}
	skipped, err := s.Get(ctx, id)
	if err != nil {
		return nil, nil, "", "", err
	}
	// The cadence advances exactly as on Complete. Skipping an occurrence is not
	// abandoning the series — that is what delete_task is for.
	next, rollover, rerr := s.rollOver(ctx, skipped)
	if rerr != nil {
		log.Printf("task %s skipped but rollover failed: %v", id, rerr)
		return skipped, nil, RolloverFailed, rerr.Error(), nil
	}
	return skipped, next, rollover, "", nil
}

// truncSkipReason caps the user's own words, rune-safely: this is free text typed
// by voice, and a 40 KB paste would otherwise ride along in every list response.
func truncSkipReason(s string) string {
	const max = 200
	return truncateRunes(s, max)
}

// truncateRunes caps a string at max BYTES without splitting a UTF-8 rune, so a
// stored value never ends in the middle of a character.
//
// There were three hand-rolled versions of this (truncSkipReason, truncateField
// and two inline `s[:n]` clamps) and only one of them was rune-safe — the skip
// reason, added later, was; the idempotency key and the audit fields were not. So a
// non-ASCII source label or a key with a multi-byte character could be stored as
// invalid UTF-8, which BSON accepts and every reader then has to defend against.
// Same rule, one place, applied to every capped field.
func truncateRunes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Reopen clears completion (completedAt + expiresAt + the skip marker), making it
// open again. A skipped task reopens to OPEN, not to "done" — the user is putting
// it back on the list, which is the opposite of what a skip recorded.
// Errors on unknown ids and on tasks that are already open.
func (s *Store) Reopen(ctx context.Context, id string) (map[string]any, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("bad id '%s'", id)
	}
	res, err := s.tasks.UpdateOne(ctx,
		bson.M{"_id": oid, "completedAt": bson.M{"$ne": nil, "$exists": true}},
		bson.M{
			"$unset": bson.M{
				"completedAt": "", "expiresAt": "",
				"skippedAt": "", "skipReason": "",
			},
			"$inc": bson.M{"revision": 1},
		})
	if err != nil {
		return nil, err
	}
	if res.MatchedCount == 0 {
		var doc bson.M
		if ferr := s.tasks.FindOne(ctx, bson.M{"_id": oid}).Decode(&doc); ferr != nil {
			return nil, fail("unknown task '%s'", id)
		}
		return nil, fail("task '%s' is already open", id)
	}
	return s.Get(ctx, id)
}

// Delete removes a task regardless of completion.
func (s *Store) Delete(ctx context.Context, id string) (bool, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return false, fail("bad id '%s'", id)
	}
	res, err := s.tasks.DeleteOne(ctx, bson.M{"_id": oid})
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}
