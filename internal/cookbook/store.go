// Package cookbook implements MongoDB storage for the cookbook MCP.
// Two permanent books, never pruned: cookbook_ingredients (name + note)
// and cookbook_recipes (name + note + tags). Ultra-simple on purpose —
// no quantities, servings, macros, or cook log (#600).
//
// cookbook_cook_log is retired: the collection stays in Atlas untouched,
// but nothing reads or writes it. Old recipe/ingredient rows keep any
// legacy fields they carry; reads simply ignore them.
package cookbook

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"agento/internal/mongo"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Notes cap at 2000 bytes for both books (#600); tags stay capped at
// 20 entries of 40 bytes each. Byte caps, like money's: a CJK/emoji-heavy
// note hits the cap earlier than a char count would suggest.
const maxNoteLen = 2000
const maxTags = 20
const maxTagLen = 40

// Sort fields per list. Each list_* names only its own fields; sort is
// asc|desc everywhere. The two-enum convention mirrors query_transactions:
// sort_by names the field, sort the direction, blank means the defaults
// (name, asc). Unknown values fail naming the allowed set — the schema enum
// stops them first, this is the second layer for direct store callers.
var (
	IngredientSortFields = []string{"name", "updatedAt"}
	RecipeSortFields     = []string{"name", "updatedAt"}
)

// checkSort validates a list's sort pair and returns the normalized sort
// key plus direction. Unknown values fail naming the allowed set — the
// schema enum stops them first, this is the second layer for direct store
// callers. Whitespace-only counts as absent (the default), not as an error;
// sort_by stays case-exact — the fields are camelCase (updatedAt), so
// lowercasing would corrupt the key — while the direction folds case like
// money's order. Returning the key (instead of validating a copy) keeps
// validation and the issued sort from ever disagreeing.
func checkSort(tool, sortBy string, allowed []string, dir string) (string, bool, error) {
	sortBy = strings.TrimSpace(sortBy)
	dir = strings.ToLower(strings.TrimSpace(dir))
	if sortBy == "" {
		sortBy = allowed[0]
	}
	ok := false
	for _, a := range allowed {
		if sortBy == a {
			ok = true
			break
		}
	}
	if !ok {
		return "", false, fail("%s sort_by must be one of %v, got '%s'", tool, allowed, sortBy)
	}
	switch dir {
	case "", "asc":
		return sortBy, true, nil
	case "desc":
		return sortBy, false, nil
	default:
		return "", false, fail("%s sort must be one of [asc desc], got '%s'", tool, dir)
	}
}

func sortDir(asc bool) int {
	if asc {
		return 1
	}
	return -1
}

// StoreError is the domain error.
type StoreError struct{ Msg string }

func (e *StoreError) Error() string { return e.Msg }

func fail(format string, args ...any) *StoreError {
	return &StoreError{Msg: fmt.Sprintf(format, args...)}
}

func oid(s string) (primitive.ObjectID, error) {
	o, err := primitive.ObjectIDFromHex(s)
	if err != nil {
		return primitive.NilObjectID, fail("bad id '%s'", s)
	}
	return o, nil
}

func docOut(d bson.M) bson.M {
	out := bson.M{}
	for k, v := range d {
		out[k] = v
	}
	out["_id"] = mongo.IDString(d["_id"])
	for _, k := range []string{"createdAt", "updatedAt"} {
		if v, ok := out[k]; ok && v != nil {
			out[k] = fmt.Sprint(v)
		}
	}
	// Retired recipe fields (#600): pre-trim rows still carry servings,
	// macros, quantities and source in Atlas. Reads strip them so callers
	// never see fields the tools can no longer write or clear — an old
	// recipe that still returned `quantities` would invite an update the
	// patch path silently drops.
	for _, k := range []string{"servings", "quantities", "source",
		"kcal", "protein_g", "carbs_g", "fat_g", "fiber_g"} {
		delete(out, k)
	}
	return out
}

// Store binds the 2 cookbook collections + indexes on first use.
type Store struct {
	ings    *mongoDrv.Collection
	recipes *mongoDrv.Collection
}

func New() (*Store, error) {
	d, err := mongo.DB()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s := &Store{
		ings:    d.Collection("cookbook_ingredients"),
		recipes: d.Collection("cookbook_recipes"),
	}
	if _, err := s.ings.Indexes().CreateOne(ctx, mongoDrv.IndexModel{
		Keys: bson.D{{Key: "name", Value: 1}}, Options: options.Index().SetUnique(true)}); err != nil {
		return nil, err
	}
	if _, err := s.recipes.Indexes().CreateOne(ctx, mongoDrv.IndexModel{
		Keys: bson.D{{Key: "name", Value: 1}}, Options: options.Index().SetUnique(true)}); err != nil {
		return nil, err
	}
	return s, nil
}

// FromEnv builds a Store from env (kept for symmetry; New reads env itself).
func FromEnv() (*Store, error) { return New() }

func capTags(tags []string) []string {
	capped := make([]string, 0, len(tags))
	for _, t := range tags {
		// Trimmed and empties dropped: the tag filter trims too, so a
		// stored `"  test  "` or `""` would be unqueryable (#600 review).
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		capped = append(capped, truncateBytes(t, maxTagLen))
		if len(capped) >= maxTags {
			break
		}
	}
	return capped
}

func (s *Store) AddIngredient(ctx context.Context, name, note string) (bson.M, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fail("ingredient name is required")
	}
	// Trimmed like names: a whitespace-only note is empty, not content.
	note = truncateBytes(strings.TrimSpace(note), maxNoteLen)
	now := primitive.NewDateTimeFromTime(time.Now().UTC())
	doc := bson.M{"name": name, "note": note, "createdAt": now, "updatedAt": now}
	res, err := s.ings.InsertOne(ctx, doc)
	if err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			return nil, fail("ingredient '%s' already exists", name)
		}
		return nil, err
	}
	doc["_id"] = res.InsertedID
	return docOut(doc), nil
}

func (s *Store) GetIngredient(ctx context.Context, nameOrID string) (bson.M, error) {
	ing, err := s.findIng(ctx, nameOrID)
	if err != nil {
		// Unknown reads back as missing; a real DB failure propagates
		// instead of masquerading as not-found (#600 review).
		var se *StoreError
		if errors.As(err, &se) {
			return nil, nil
		}
		return nil, err
	}
	return docOut(ing), nil
}

func (s *Store) ListIngredients(ctx context.Context, search, sortBy, dir string) ([]bson.M, int64, error) {
	// The key comes back normalized from checkSort: validation and the
	// issued sort cannot disagree (whitespace-only is the default key).
	sortBy, asc, err := checkSort("list_ingredients", sortBy, IngredientSortFields, dir)
	if err != nil {
		return nil, 0, err
	}
	filt := bson.M{}
	if strings.TrimSpace(search) != "" {
		filt = bson.M{"name": bson.M{"$regex": regexp.QuoteMeta(strings.TrimSpace(search)), "$options": "i"}}
	}
	// The _id tiebreak follows the direction so updatedAt ties page
	// deterministically (names are unique, but updatedAt is not).
	total, err := s.ings.CountDocuments(ctx, filt)
	if err != nil {
		return nil, 0, err
	}
	cur, err := s.ings.Find(ctx, filt, options.Find().SetSort(bson.D{{Key: sortBy, Value: sortDir(asc)}, {Key: "_id", Value: sortDir(asc)}}).SetLimit(500))
	if err != nil {
		return nil, 0, err
	}
	rows, err := allOut(ctx, cur)
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

func (s *Store) UpdateIngredient(ctx context.Context, nameOrID string, patch map[string]any) (bson.M, error) {
	ing, err := s.GetIngredient(ctx, nameOrID)
	if err != nil || ing == nil {
		return nil, err
	}
	upd := bson.M{}
	if v, ok := patch["name"].(string); ok && strings.TrimSpace(v) != "" {
		upd["name"] = strings.TrimSpace(v)
	}
	if v, ok := patch["note"].(string); ok {
		if v = strings.TrimSpace(v); v != "" {
			upd["note"] = truncateBytes(v, maxNoteLen)
		}
	}
	if len(upd) == 0 {
		return nil, fail("nothing to update — pass a new name or note")
	}
	upd["updatedAt"] = primitive.NewDateTimeFromTime(time.Now().UTC())
	rawID := idstr(ing["_id"])
	oid, _ := primitive.ObjectIDFromHex(rawID)
	if _, err := s.ings.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$set": upd}); err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			return nil, fail("ingredient '%v' already exists", upd["name"])
		}
		return nil, err
	}
	return s.GetIngredient(ctx, rawID)
}

func (s *Store) DeleteIngredient(ctx context.Context, nameOrID string) (bool, error) {
	// Unknown reads back as not-deleted, like DeleteRecipe: deletes are
	// idempotent, while gets/updates report unknown targets as ok:false.
	ing, err := s.GetIngredient(ctx, nameOrID)
	if err != nil || ing == nil {
		return false, err
	}
	oid, _ := primitive.ObjectIDFromHex(idstr(ing["_id"]))
	res, err := s.ings.DeleteOne(ctx, bson.M{"_id": oid})
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}

func (s *Store) findIng(ctx context.Context, nameOrID string) (bson.M, error) {
	// IDs are trimmed like names: a copy-pasted id with surrounding
	// whitespace must resolve the same as the bare id (#600 review).
	if o, err := oid(strings.TrimSpace(nameOrID)); err == nil {
		var ing bson.M
		if err := s.ings.FindOne(ctx, bson.M{"_id": o}).Decode(&ing); err == nil {
			return ing, nil
		} else if err != mongoDrv.ErrNoDocuments {
			return nil, err
		}
	}
	var ing bson.M
	if err := s.ings.FindOne(ctx, bson.M{"name": strings.TrimSpace(nameOrID)}).Decode(&ing); err != nil {
		// Only a genuine miss reads as unknown: a transient Atlas
		// failure must surface as an error, never as ok:false/ok:true
		// deleted:false (#600 review).
		if err == mongoDrv.ErrNoDocuments {
			return nil, fail("unknown ingredient '%s'", nameOrID)
		}
		return nil, err
	}
	return ing, nil
}

func (s *Store) AddRecipe(ctx context.Context, name, note string, tags []string) (bson.M, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fail("recipe name is required")
	}
	note = truncateBytes(strings.TrimSpace(note), maxNoteLen)
	now := primitive.NewDateTimeFromTime(time.Now().UTC())
	doc := bson.M{"name": name, "note": note, "tags": capTags(tags),
		"createdAt": now, "updatedAt": now}
	res, err := s.recipes.InsertOne(ctx, doc)
	if err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			return nil, fail("recipe '%s' already exists", name)
		}
		return nil, err
	}
	doc["_id"] = res.InsertedID
	return docOut(doc), nil
}

func (s *Store) GetRecipe(ctx context.Context, nameOrID string) (bson.M, error) {
	var r bson.M
	if o, err := oid(strings.TrimSpace(nameOrID)); err == nil {
		if err := s.recipes.FindOne(ctx, bson.M{"_id": o}).Decode(&r); err == nil {
			return docOut(r), nil
		} else if err != mongoDrv.ErrNoDocuments {
			return nil, err
		}
	}
	if err := s.recipes.FindOne(ctx, bson.M{"name": strings.TrimSpace(nameOrID)}).Decode(&r); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return nil, nil
		}
		return nil, err
	}
	return docOut(r), nil
}

func (s *Store) ListRecipes(ctx context.Context, search, tag, sortBy, dir string) ([]bson.M, int64, error) {
	sortBy, asc, err := checkSort("list_recipes", sortBy, RecipeSortFields, dir)
	if err != nil {
		return nil, 0, err
	}
	filt := bson.M{}
	if strings.TrimSpace(search) != "" {
		filt["name"] = bson.M{"$regex": regexp.QuoteMeta(strings.TrimSpace(search)), "$options": "i"}
	}
	if strings.TrimSpace(tag) != "" {
		filt["tags"] = strings.TrimSpace(tag)
	}
	total, err := s.recipes.CountDocuments(ctx, filt)
	if err != nil {
		return nil, 0, err
	}
	cur, err := s.recipes.Find(ctx, filt, options.Find().SetSort(bson.D{{Key: sortBy, Value: sortDir(asc)}, {Key: "_id", Value: sortDir(asc)}}).SetLimit(200))
	if err != nil {
		return nil, 0, err
	}
	rows, err := allOut(ctx, cur)
	if err != nil {
		return nil, 0, err
	}
	return rows, total, nil
}

func (s *Store) UpdateRecipe(ctx context.Context, nameOrID string, patch map[string]any) (bson.M, error) {
	r, err := s.GetRecipe(ctx, nameOrID)
	if err != nil || r == nil {
		return nil, err
	}
	upd := bson.M{}
	if v, ok := patch["name"].(string); ok && strings.TrimSpace(v) != "" {
		upd["name"] = strings.TrimSpace(v)
	}
	if v, ok := patch["note"].(string); ok {
		if v = strings.TrimSpace(v); v != "" {
			upd["note"] = truncateBytes(v, maxNoteLen)
		}
	}
	if v, ok := patch["tags"].([]any); ok && len(v) > 0 {
		str := make([]string, 0, len(v))
		for _, t := range v {
			str = append(str, fmt.Sprint(t))
		}
		if capped := capTags(str); len(capped) > 0 {
			upd["tags"] = capped
		}
	}
	if len(upd) == 0 {
		return nil, fail("nothing to update — pass a new name, note or tags")
	}
	upd["updatedAt"] = primitive.NewDateTimeFromTime(time.Now().UTC())
	rawID := idstr(r["_id"])
	oid, _ := primitive.ObjectIDFromHex(rawID)
	if _, err := s.recipes.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$set": upd}); err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			return nil, fail("recipe '%v' already exists", upd["name"])
		}
		return nil, err
	}
	return s.GetRecipe(ctx, rawID)
}

func (s *Store) DeleteRecipe(ctx context.Context, nameOrID string) (bool, error) {
	r, err := s.GetRecipe(ctx, nameOrID)
	if err != nil || r == nil {
		return false, err
	}
	oid, _ := primitive.ObjectIDFromHex(idstr(r["_id"]))
	res, err := s.recipes.DeleteOne(ctx, bson.M{"_id": oid})
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}

func allOut(ctx context.Context, cur *mongoDrv.Cursor) ([]bson.M, error) {
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	if rows == nil {
		return []bson.M{}, nil
	}
	out := make([]bson.M, 0, len(rows))
	for _, r := range rows {
		out = append(out, docOut(r))
	}
	return out, nil
}

func idstr(v any) string { return mongo.IDString(v) }

// truncateBytes caps s at n bytes without splitting a UTF-8 rune: a naive
// s[:n] can cut a multi-byte character mid-sequence and store invalid bytes.
// Same shape as money's — the caps below are byte caps.
func truncateBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
