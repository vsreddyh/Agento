// Package projects: MongoDB-backed project board for the agent.
//
// Collection: projects. One doc per project with name, status (Todo |
// Ongoing | Testing | Blocked | Paused | Done), note, position, createdAt,
// updatedAt. Projects are permanent (no TTL, no retention pruning) and
// shared with the app's Projects tab over /api/projects (see
// cmd/health-api/main.go) and the project-manager MCP server (see
// cmd/project-manager/main.go).
//
// position is the manual board order (#519): ascending, spaced
// positionStep apart so rows can be re-slotted without rewriting the whole
// board. Rows created before #519 are backfilled with positions at startup
// (EnsureSchema), so every row carries one in steady state; a row that
// somehow lacks it sorts last instead of leading the board.
package projects

import (
	"agento/internal/mongostore"
	"context"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const coll = "projects"

// positionStep separates consecutive manual positions so a reorder can
// rewrite a section without renumbering the whole board.
const positionStep = int64(1000)

// DefaultLimit caps List rows when the caller passes limit <= 0;
// MaxLimit clamps explicit requests (one unbounded Find would grow with
// the board forever).
const (
	DefaultLimit = 200
	MaxLimit     = 500
)

// Statuses is the fixed project lifecycle (same set as the app board).
var Statuses = []string{"Todo", "Ongoing", "Testing", "Blocked", "Paused", "Done"}

// StoreError is the domain error: servers catch it and return {ok: false, error}.
type StoreError struct{ Msg string }

func (e *StoreError) Error() string { return e.Msg }

func fail(format string, args ...any) *StoreError {
	return &StoreError{Msg: fmt.Sprintf(format, args...)}
}

// Store is the MongoDB backend for projects.
type Store struct {
	client   *mongoDrv.Client
	db       *mongoDrv.Database
	projects *mongoDrv.Collection
}

// New connects to uri/dbName and ensures indexes.
func New(uri, dbName string) (*Store, error) {
	c, db, err := mongostore.Open(uri, dbName)
	if err != nil {
		return nil, err
	}
	s := &Store{client: c, db: db, projects: db.Collection(coll)}
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

// EnsureSchema creates the query indexes (idempotent) and backfills
// positions for pre-#519 rows. No TTL: projects are permanent.
func (s *Store) EnsureSchema(ctx context.Context) error {
	_, err := s.projects.Indexes().CreateMany(ctx, []mongoDrv.IndexModel{
		{Keys: bson.D{{Key: "status", Value: 1}}, Options: options.Index().SetName("status_1")},
		{Keys: bson.D{{Key: "updatedAt", Value: -1}}, Options: options.Index().SetName("updatedAt_-1")},
		{Keys: bson.D{{Key: "position", Value: 1}}, Options: options.Index().SetName("position_1")},
	})
	if err != nil {
		return err
	}
	return s.backfillPositions(ctx)
}

// CheckStatus rejects values outside the fixed set (empty = default Todo).
func CheckStatus(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "Todo", nil
	}
	for _, s := range Statuses {
		if strings.EqualFold(strings.TrimSpace(raw), s) {
			return s, nil
		}
	}
	return "", fail("status must be Todo|Ongoing|Testing|Blocked|Paused|Done, got '%s'", raw)
}

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
	return out
}

// Create inserts a project. Name required; empty status defaults to Todo.
// New rows get the highest position, which renders as the end of their
// status section.
func (s *Store) Create(ctx context.Context, name, status, note string) (map[string]any, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fail("name is required")
	}
	status, err := CheckStatus(status)
	if err != nil {
		return nil, err
	}
	position, err := s.nextPosition(ctx)
	if err != nil {
		return nil, err
	}
	now := primitive.NewDateTimeFromTime(time.Now().UTC())
	doc := bson.M{
		"name": name, "status": status, "note": strings.TrimSpace(note),
		"position": position, "createdAt": now, "updatedAt": now,
	}
	res, err := s.projects.InsertOne(ctx, doc)
	if err != nil {
		return nil, err
	}
	doc["_id"] = res.InsertedID
	return toDoc(doc), nil
}

// highestPosition returns the current maximum position (0 on an empty
// board).
func (s *Store) highestPosition(ctx context.Context) (int64, error) {
	var last struct {
		Position int64 `bson:"position"`
	}
	err := s.projects.FindOne(ctx,
		bson.M{"position": bson.M{"$exists": true}},
		options.FindOne().
			SetSort(bson.D{{Key: "position", Value: -1}}).
			SetProjection(bson.M{"position": 1}),
	).Decode(&last)
	if err != nil && err != mongoDrv.ErrNoDocuments {
		return 0, err
	}
	return last.Position, nil
}

// nextPosition returns one step past the current highest position (1000 on
// an empty board). Two concurrent creates can pick the same value; the
// updatedAt tiebreak keeps the list deterministic until the next reorder.
func (s *Store) nextPosition(ctx context.Context) (int64, error) {
	max, err := s.highestPosition(ctx)
	if err != nil {
		return 0, err
	}
	return max + positionStep, nil
}

// backfillPositions slots rows written before #519 after every positioned
// row, in their previous visible order (newest first). Without it a new
// project would jump above the legacy rows in its section instead of
// appending. No-op once every row has a position; idempotent.
func (s *Store) backfillPositions(ctx context.Context) error {
	cur, err := s.projects.Find(ctx,
		bson.M{"position": bson.M{"$exists": false}},
		options.Find().
			SetSort(bson.D{{Key: "updatedAt", Value: -1}}).
			SetProjection(bson.M{"_id": 1}))
	if err != nil {
		return err
	}
	defer cur.Close(ctx)
	var ids []primitive.ObjectID
	for cur.Next(ctx) {
		var doc struct {
			ID primitive.ObjectID `bson:"_id"`
		}
		if err := cur.Decode(&doc); err != nil {
			return err
		}
		ids = append(ids, doc.ID)
	}
	if err := cur.Err(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	base, err := s.highestPosition(ctx)
	if err != nil {
		return err
	}
	for i, oid := range ids {
		if _, err := s.projects.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{
			"$set": bson.M{"position": base + int64(i+1)*positionStep},
		}); err != nil {
			return err
		}
	}
	return nil
}

// List returns projects filtered by status ("" or "all" = everything) with
// optional case-insensitive name/note search, in the board order the app
// renders: status rank (Ongoing, Testing, Blocked, Paused, Todo, Done),
// then position ascending, then most recently updated. A row with no
// position (only reachable mid-backfill) sorts after every positioned row
// in its section. limit caps rows after ordering: <=0 defaults to
// DefaultLimit, above MaxLimit clamps down.
func (s *Store) List(ctx context.Context, status, search string, limit int) ([]map[string]any, error) {
	filt := bson.M{}
	if status = strings.TrimSpace(status); status != "" && !strings.EqualFold(status, "all") {
		st, err := CheckStatus(status)
		if err != nil {
			return nil, err
		}
		filt["status"] = st
	}
	if search = strings.TrimSpace(search); search != "" {
		// QuoteMeta: raw user input must never reach the regex engine.
		rx := bson.M{"$regex": regexp.QuoteMeta(search), "$options": "i"}
		filt["$or"] = []bson.M{{"name": rx}, {"note": rx}}
	}
	// Board order is the one the app renders: status sections in the fixed
	// order (Ongoing → Testing → Blocked → Paused → Todo → Done), then the
	// stored position, then newest-updated. $ifNull makes a missing
	// position sort last (MaxInt64) instead of first, where MongoDB puts a
	// missing field in an ascending sort. limit has to run after the sort,
	// hence an aggregation.
	cur, err := s.projects.Aggregate(ctx, mongoDrv.Pipeline{
		{{Key: "$match", Value: filt}},
		{{Key: "$addFields", Value: bson.M{
			"_statusRank": bson.M{"$switch": bson.M{
				"branches": bson.A{
					bson.M{"case": bson.M{"$eq": bson.A{"$status", "Ongoing"}}, "then": 0},
					bson.M{"case": bson.M{"$eq": bson.A{"$status", "Testing"}}, "then": 1},
					bson.M{"case": bson.M{"$eq": bson.A{"$status", "Blocked"}}, "then": 2},
					bson.M{"case": bson.M{"$eq": bson.A{"$status", "Paused"}}, "then": 3},
					bson.M{"case": bson.M{"$eq": bson.A{"$status", "Done"}}, "then": 5},
				},
				"default": 4,
			}},
			"_manualOrder": bson.M{
				"$ifNull": bson.A{"$position", int64(math.MaxInt64)},
			},
		}}},
		{{Key: "$sort", Value: bson.D{
			{Key: "_statusRank", Value: 1},
			{Key: "_manualOrder", Value: 1},
			{Key: "updatedAt", Value: -1},
		}}},
		{{Key: "$limit", Value: int64(mongostore.ClampLimit(limit, DefaultLimit, MaxLimit))}},
		{{Key: "$unset", Value: bson.A{"_statusRank", "_manualOrder"}}},
	})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	out := []map[string]any{}
	for cur.Next(ctx) {
		var doc bson.M
		if err := cur.Decode(&doc); err != nil {
			return nil, err
		}
		out = append(out, toDoc(doc))
	}
	return out, cur.Err()
}

// Get fetches one project by hex id.
func (s *Store) Get(ctx context.Context, id string) (map[string]any, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("bad id '%s'", id)
	}
	var doc bson.M
	if err := s.projects.FindOne(ctx, bson.M{"_id": oid}).Decode(&doc); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return nil, fail("unknown project '%s'", id)
		}
		return nil, err
	}
	return toDoc(doc), nil
}

// Update edits name/status/note (only sent keys change) and bumps
// updatedAt. Empty update is a no-op returning the current doc.
func (s *Store) Update(ctx context.Context, id string, fields map[string]any) (map[string]any, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("bad id '%s'", id)
	}
	var cur bson.M
	if err := s.projects.FindOne(ctx, bson.M{"_id": oid}).Decode(&cur); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return nil, fail("unknown project '%s'", id)
		}
		return nil, err
	}
	set := bson.M{}
	if v, ok := fields["name"]; ok {
		name, ok := v.(string)
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fail("name is required")
		}
		set["name"] = strings.TrimSpace(name)
	}
	if v, ok := fields["status"]; ok {
		st, ok := v.(string)
		if !ok {
			return nil, fail("status must be Todo|Ongoing|Testing|Blocked|Paused|Done")
		}
		st, err := CheckStatus(st)
		if err != nil {
			return nil, err
		}
		set["status"] = st
	}
	if v, ok := fields["note"]; ok {
		note, ok := v.(string)
		if !ok {
			return nil, fail("note must be a string")
		}
		set["note"] = strings.TrimSpace(note)
	}
	if len(set) == 0 {
		return toDoc(cur), nil
	}
	// A status change moves the row to a new section: give it a fresh
	// position so it lands at the end of that section instead of
	// inheriting the old section's slot.
	if newStatus, ok := set["status"].(string); ok {
		oldStatus, _ := cur["status"].(string)
		if !strings.EqualFold(newStatus, oldStatus) {
			position, err := s.nextPosition(ctx)
			if err != nil {
				return nil, err
			}
			set["position"] = position
		}
	}
	set["updatedAt"] = primitive.NewDateTimeFromTime(time.Now().UTC())
	if _, err := s.projects.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$set": set}); err != nil {
		return nil, err
	}
	return s.Get(ctx, id)
}

// Delete removes a project permanently.
func (s *Store) Delete(ctx context.Context, id string) (bool, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return false, fail("bad id '%s'", id)
	}
	res, err := s.projects.DeleteOne(ctx, bson.M{"_id": oid})
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}

// Reorder rewrites the manual order for one status section (#519): ids must
// list the complete section, every id must exist and still carry status
// (checked per id and again in each write's filter, so a concurrent status
// edit is an error rather than a silent overwrite), and positions become
// (i+1)*positionStep in the given order. updatedAt is deliberately
// untouched — reordering is not an edit.
func (s *Store) Reorder(ctx context.Context, status string, ids []string) error {
	status, err := CheckStatus(status)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return fail("ids is required")
	}
	if len(ids) > MaxLimit {
		return fail("too many ids (max %d)", MaxLimit)
	}
	// A partial list would leave omitted rows at their old positions and
	// create duplicates with the rewritten ones, so the whole section has
	// to be listed. The per-id status checks below then prove set equality
	// (distinct ids all in this section + same count as the section).
	total, err := s.projects.CountDocuments(ctx, bson.M{"status": status})
	if err != nil {
		return err
	}
	if int(total) != len(ids) {
		return fail("ids must list all %d %s projects, got %d", total, status, len(ids))
	}
	oids := make([]primitive.ObjectID, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			return fail("ids must not contain blank values")
		}
		oid, err := primitive.ObjectIDFromHex(id)
		if err != nil {
			return fail("bad id '%s'", id)
		}
		// ObjectID hex is case-insensitive, so the same project can arrive
		// twice in different case: key on the canonical form, or the
		// duplicate slips past the guard, defeats the set-equality proof
		// and writes the same position to two rows.
		key := oid.Hex()
		if seen[key] {
			return fail("duplicate id '%s'", id)
		}
		seen[key] = true
		var doc struct {
			Status string `bson:"status"`
		}
		if err := s.projects.FindOne(ctx, bson.M{"_id": oid}, options.FindOne().
			SetProjection(bson.M{"status": 1})).Decode(&doc); err != nil {
			if err == mongoDrv.ErrNoDocuments {
				return fail("unknown project '%s'", id)
			}
			return err
		}
		current, err := CheckStatus(doc.Status)
		if err != nil {
			return err
		}
		if !strings.EqualFold(current, status) {
			return fail("project '%s' is %s, not %s", id, current, status)
		}
		oids = append(oids, oid)
	}
	// The status filter is repeated on every write: a row edited to another
	// status between the check above and its write must abort the reorder
	// instead of silently taking a new position in a section it left.
	// Earlier rows keep their rewritten positions, which the next list
	// shows as-is (the app reloads on failure), so a partial rewrite is
	// visible rather than hidden.
	changed := ""
	for i, oid := range oids {
		res, err := s.projects.UpdateOne(ctx,
			bson.M{"_id": oid, "status": status},
			bson.M{"$set": bson.M{"position": int64(i+1) * positionStep}},
		)
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			changed = oid.Hex()
			break
		}
	}
	if changed != "" {
		return fail("project '%s' changed status during reorder", changed)
	}
	return nil
}
