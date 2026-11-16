// Package projects: MongoDB-backed project board for the agent.
//
// Collection: projects. One doc per project with name, status (Todo |
// Ongoing | Testing | Paused | Done), note, position, createdAt,
// updatedAt. Projects are permanent (no TTL, no retention pruning) and
// shared with the app's Projects tab over /api/projects (see
// cmd/health-api/projects.go) and the project-manager MCP server (see
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
	"sort"
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
var Statuses = []string{"Todo", "Ongoing", "Testing", "Paused", "Done"}

// StoreError is the domain error: servers catch it and return {ok: false, error}.
type StoreError struct{ Msg string }

func (e *StoreError) Error() string { return e.Msg }

func fail(format string, args ...any) *StoreError {
	return &StoreError{Msg: fmt.Sprintf(format, args...)}
}

// counters holds single-doc atomic counters (one doc per _id). New code
// must not compute a position from a read-then-write: two concurrent
// creates read the same max and store the same next position (#527).
const countersColl = "counters"

// positionCounterID is the counters doc handing out project positions.
const positionCounterID = "projects_position"

// Store is the MongoDB backend for projects.
type Store struct {
	client   *mongoDrv.Client
	db       *mongoDrv.Database
	projects *mongoDrv.Collection
	counters *mongoDrv.Collection
}

// New connects to uri/dbName and ensures indexes.
func New(uri, dbName string) (*Store, error) {
	c, db, err := mongostore.Open(uri, dbName)
	if err != nil {
		return nil, err
	}
	s := &Store{client: c, db: db, projects: db.Collection(coll), counters: db.Collection(countersColl)}
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

// EnsureSchema creates the query indexes (idempotent), backfills
// positions for pre-#519 rows, and seeds the atomic position counter at
// the current max so the first handed-out position lands past every row.
// No TTL: projects are permanent.
func (s *Store) EnsureSchema(ctx context.Context) error {
	_, err := s.projects.Indexes().CreateMany(ctx, []mongoDrv.IndexModel{
		{Keys: bson.D{{Key: "status", Value: 1}}, Options: options.Index().SetName("status_1")},
		{Keys: bson.D{{Key: "updatedAt", Value: -1}}, Options: options.Index().SetName("updatedAt_-1")},
		{Keys: bson.D{{Key: "position", Value: 1}}, Options: options.Index().SetName("position_1")},
	})
	if err != nil {
		return err
	}
	if err := s.backfillPositions(ctx); err != nil {
		return err
	}
	return s.seedPositionCounter(ctx)
}

// seedPositionCounter raises the counter to at least the current highest
// position ($max never lowers it, upsert creates it), so positions handed
// out after a backfill or a compaction still append past every row.
func (s *Store) seedPositionCounter(ctx context.Context) error {
	max, err := s.highestPosition(ctx)
	if err != nil {
		return err
	}
	_, err = s.counters.UpdateOne(ctx,
		bson.M{"_id": positionCounterID},
		bson.M{"$max": bson.M{"seq": max}},
		options.Update().SetUpsert(true),
	)
	return err
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
	return "", fail("status must be Todo|Ongoing|Testing|Paused|Done, got '%s'", raw)
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

// nextPosition hands out one step past the counter (1000 on an empty
// board). The $inc is atomic server-side, so concurrent creates never
// share a position (#527); the old read-max-then-+1000 did.
func (s *Store) nextPosition(ctx context.Context) (int64, error) {
	var doc struct {
		Seq int64 `bson:"seq"`
	}
	err := s.counters.FindOneAndUpdate(ctx,
		bson.M{"_id": positionCounterID},
		bson.M{"$inc": bson.M{"seq": positionStep}},
		options.FindOneAndUpdate().
			SetUpsert(true).
			SetReturnDocument(options.After),
	).Decode(&doc)
	if err != nil {
		return 0, err
	}
	return doc.Seq, nil
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
// renders: status rank (Ongoing, Testing, Paused, Todo, Done),
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
	// order (Ongoing → Testing → Paused → Todo → Done), then the
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
					bson.M{"case": bson.M{"$eq": bson.A{"$status", "Paused"}}, "then": 2},
					bson.M{"case": bson.M{"$eq": bson.A{"$status", "Done"}}, "then": 4},
				},
				"default": 3,
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
			return nil, fail("status must be Todo|Ongoing|Testing|Paused|Done")
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
	if err := s.writePositions(ctx, status, oids); err != nil {
		return err
	}
	return nil
}

// writePositions assigns (i+1)*positionStep to oids in order — the full
// rewrite Reorder used to expose directly. The status filter is repeated
// on every write, so a row edited to another status mid-write aborts
// instead of silently taking a new position in a section it left.
// Earlier rows keep their rewritten positions, which the next list shows
// as-is (callers reload on failure), so a partial rewrite is visible
// rather than hidden. updatedAt is deliberately untouched.
func (s *Store) writePositions(ctx context.Context, status string, oids []primitive.ObjectID) error {
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

// slotPosition picks a sparse position between prev and next (nil = open
// end). int64 throughout to match the stored field. ok=false means the
// integer gap is exhausted and the caller must compact the section
// before retrying.
func slotPosition(prev, next *int64, step int64) (pos int64, ok bool) {
	switch {
	case prev == nil && next == nil:
		return step, true
	case prev == nil:
		return *next - step, true
	case next == nil:
		return *prev + step, true
	case *next-*prev < 2:
		return 0, false
	default:
		return *prev + (*next-*prev)/2, true
	}
}

// sectionOrder returns one status section as (id, position) pairs in board
// order: position ascending, rows missing a position (only reachable
// mid-backfill) last, _id hex as the deterministic tiebreak.
func (s *Store) sectionOrder(ctx context.Context, status string) ([]primitive.ObjectID, []int64, error) {
	cur, err := s.projects.Find(ctx,
		bson.M{"status": status},
		options.Find().SetProjection(bson.M{"_id": 1, "position": 1}))
	if err != nil {
		return nil, nil, err
	}
	defer cur.Close(ctx)
	type row struct {
		id  primitive.ObjectID
		pos *int64
	}
	var rows []row
	for cur.Next(ctx) {
		var doc struct {
			ID  primitive.ObjectID `bson:"_id"`
			Pos *int64             `bson:"position"`
		}
		if err := cur.Decode(&doc); err != nil {
			return nil, nil, err
		}
		rows = append(rows, row{id: doc.ID, pos: doc.Pos})
	}
	if err := cur.Err(); err != nil {
		return nil, nil, err
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].pos == nil && rows[j].pos == nil {
			return rows[i].id.Hex() < rows[j].id.Hex()
		}
		if rows[i].pos == nil {
			return false
		}
		if rows[j].pos == nil {
			return true
		}
		if *rows[i].pos != *rows[j].pos {
			return *rows[i].pos < *rows[j].pos
		}
		return rows[i].id.Hex() < rows[j].id.Hex()
	})
	oids := make([]primitive.ObjectID, 0, len(rows))
	positions := make([]int64, 0, len(rows))
	for _, r := range rows {
		oids = append(oids, r.id)
		if r.pos == nil {
			positions = append(positions, int64(math.MaxInt64))
		} else {
			positions = append(positions, *r.pos)
		}
	}
	return oids, positions, nil
}

// Move relocates one project inside its status section (#527): id lands
// immediately before anchor (after=false) or after it (after=true). Only
// the moved row is written — callers never send the whole section —
// slotted into the sparse gap between its new neighbors. When repeated
// moves exhaust an integer gap the section is compacted first (the old
// full rewrite, now an internal step rather than the API). updatedAt is
// deliberately untouched, like Reorder.
func (s *Store) Move(ctx context.Context, status, id, anchor string, after bool) error {
	status, err := CheckStatus(status)
	if err != nil {
		return err
	}
	id = strings.TrimSpace(id)
	anchor = strings.TrimSpace(anchor)
	if id == "" || anchor == "" {
		return fail("id and anchor are required")
	}
	if strings.EqualFold(id, anchor) {
		return nil // idempotent no-op
	}
	oid, err := primitive.ObjectIDFromHex(id)
	if err != nil {
		return fail("bad id '%s'", id)
	}
	anchorOID, err := primitive.ObjectIDFromHex(anchor)
	if err != nil {
		return fail("bad anchor id '%s'", anchor)
	}
	for _, pair := range []struct {
		kind string
		raw  string
		oid  primitive.ObjectID
	}{{"id", id, oid}, {"anchor", anchor, anchorOID}} {
		var doc struct {
			Status string `bson:"status"`
		}
		if err := s.projects.FindOne(ctx, bson.M{"_id": pair.oid}, options.FindOne().
			SetProjection(bson.M{"status": 1})).Decode(&doc); err != nil {
			if err == mongoDrv.ErrNoDocuments {
				return fail("unknown project '%s'", pair.raw)
			}
			return err
		}
		current, err := CheckStatus(doc.Status)
		if err != nil {
			return err
		}
		if !strings.EqualFold(current, status) {
			return fail("project '%s' is %s, not %s", pair.raw, current, status)
		}
	}
	// Order without the moving row, then slot it at the anchor. The
	// guarded writes below re-prove membership, so a status edit between
	// the checks and the writes aborts instead of corrupting the order.
	oids, positions, err := s.sectionOrder(ctx, status)
	if err != nil {
		return err
	}
	rest := make([]primitive.ObjectID, 0, len(oids))
	restPos := make([]int64, 0, len(positions))
	for i, o := range oids {
		if o == oid {
			continue
		}
		rest = append(rest, o)
		restPos = append(restPos, positions[i])
	}
	at := -1
	for i, o := range rest {
		if o == anchorOID {
			at = i
			break
		}
	}
	if at < 0 {
		return fail("project '%s' changed status during move", anchor)
	}
	var prev, next *int64
	if after {
		prev = &restPos[at]
		if at+1 < len(restPos) {
			next = &restPos[at+1]
		}
	} else {
		next = &restPos[at]
		if at-1 >= 0 {
			prev = &restPos[at-1]
		}
	}
	if pos, ok := slotPosition(prev, next, positionStep); ok {
		res, err := s.projects.UpdateOne(ctx,
			bson.M{"_id": oid, "status": status},
			bson.M{"$set": bson.M{"position": pos}},
		)
		if err != nil {
			return err
		}
		if res.MatchedCount == 0 {
			return fail("project '%s' changed status during move", id)
		}
		return nil
	}
	// Gap exhausted: compact the section with the row already in its new
	// slot, so the move and the compaction land together.
	ordered := make([]primitive.ObjectID, 0, len(rest)+1)
	if after {
		ordered = append(ordered, rest[:at+1]...)
		ordered = append(ordered, oid)
		ordered = append(ordered, rest[at+1:]...)
	} else {
		ordered = append(ordered, rest[:at]...)
		ordered = append(ordered, oid)
		ordered = append(ordered, rest[at:]...)
	}
	return s.writePositions(ctx, status, ordered)
}
