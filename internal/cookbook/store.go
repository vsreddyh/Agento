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
	"fmt"
	"regexp"
	"strings"
	"time"

	"agento/internal/mongo"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// Notes cap at 2000 chars for both books (#600); tags stay capped at
// 20 entries of 40 chars each, like before the trim.
const maxNoteLen = 2000
const maxTags = 20
const maxTagLen = 40

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
		if len(t) > maxTagLen {
			t = t[:maxTagLen]
		}
		capped = append(capped, t)
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
	if len(note) > maxNoteLen {
		note = note[:maxNoteLen]
	}
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
		return nil, nil
	}
	return docOut(ing), nil
}

func (s *Store) ListIngredients(ctx context.Context, search string) ([]bson.M, error) {
	filt := bson.M{}
	if strings.TrimSpace(search) != "" {
		filt = bson.M{"name": bson.M{"$regex": regexp.QuoteMeta(strings.TrimSpace(search)), "$options": "i"}}
	}
	cur, err := s.ings.Find(ctx, filt, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}).SetLimit(500))
	if err != nil {
		return nil, err
	}
	return allOut(ctx, cur)
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
	if v, ok := patch["note"].(string); ok && v != "" {
		if len(v) > maxNoteLen {
			v = v[:maxNoteLen]
		}
		upd["note"] = v
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
	res, err := s.ings.DeleteOne(ctx, bson.M{"_id": ing["_id"]})
	if err != nil {
		return false, err
	}
	return res.DeletedCount > 0, nil
}

func (s *Store) findIng(ctx context.Context, nameOrID string) (bson.M, error) {
	if o, err := oid(nameOrID); err == nil {
		var ing bson.M
		if err := s.ings.FindOne(ctx, bson.M{"_id": o}).Decode(&ing); err == nil {
			return ing, nil
		}
	}
	var ing bson.M
	if err := s.ings.FindOne(ctx, bson.M{"name": strings.TrimSpace(nameOrID)}).Decode(&ing); err != nil {
		return nil, fail("unknown ingredient '%s'", nameOrID)
	}
	return ing, nil
}

func (s *Store) AddRecipe(ctx context.Context, name, note string, tags []string) (bson.M, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fail("recipe name is required")
	}
	if len(note) > maxNoteLen {
		note = note[:maxNoteLen]
	}
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
	if o, err := oid(nameOrID); err == nil {
		if err := s.recipes.FindOne(ctx, bson.M{"_id": o}).Decode(&r); err == nil {
			return docOut(r), nil
		}
	}
	if err := s.recipes.FindOne(ctx, bson.M{"name": strings.TrimSpace(nameOrID)}).Decode(&r); err != nil {
		return nil, nil
	}
	return docOut(r), nil
}

func (s *Store) ListRecipes(ctx context.Context, search, tag string) ([]bson.M, error) {
	filt := bson.M{}
	if strings.TrimSpace(search) != "" {
		filt["name"] = bson.M{"$regex": regexp.QuoteMeta(strings.TrimSpace(search)), "$options": "i"}
	}
	if strings.TrimSpace(tag) != "" {
		filt["tags"] = strings.TrimSpace(tag)
	}
	cur, err := s.recipes.Find(ctx, filt, options.Find().SetSort(bson.D{{Key: "name", Value: 1}}).SetLimit(200))
	if err != nil {
		return nil, err
	}
	return allOut(ctx, cur)
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
	if v, ok := patch["note"].(string); ok && v != "" {
		if len(v) > maxNoteLen {
			v = v[:maxNoteLen]
		}
		upd["note"] = v
	}
	if v, ok := patch["tags"].([]any); ok && len(v) > 0 {
		capped := []string{}
		for _, t := range v {
			ts := fmt.Sprint(t)
			if len(ts) > maxTagLen {
				ts = ts[:maxTagLen]
			}
			capped = append(capped, ts)
			if len(capped) >= maxTags {
				break
			}
		}
		upd["tags"] = capped
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
