// Package cookbook implements MongoDB storage for the cookbook MCP.
// Collections (permanent — never pruned): cookbook_ingredients,
// cookbook_recipes, cookbook_cook_log.
package cookbook

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"agento/internal/mongo"
	"agento/internal/validate"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// RecipeMacros: every recipe stores all 5 per-serving macros.
var RecipeMacros = []string{"kcal", "protein_g", "carbs_g", "fat_g", "fiber_g"}

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
	if v, ok := out["recipe_id"]; ok && v != nil {
		out["recipe_id"] = mongo.IDString(v)
	}
	return out
}

// Store binds the 3 cookbook collections + indexes on first use.
type Store struct {
	ings    *mongoDrv.Collection
	recipes *mongoDrv.Collection
	log     *mongoDrv.Collection
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
		log:     d.Collection("cookbook_cook_log"),
	}
	if _, err := s.ings.Indexes().CreateOne(ctx, mongoDrv.IndexModel{
		Keys: bson.D{{Key: "name", Value: 1}}, Options: options.Index().SetUnique(true)}); err != nil {
		return nil, err
	}
	if _, err := s.recipes.Indexes().CreateOne(ctx, mongoDrv.IndexModel{
		Keys: bson.D{{Key: "name", Value: 1}}, Options: options.Index().SetUnique(true)}); err != nil {
		return nil, err
	}
	if _, err := s.log.Indexes().CreateOne(ctx, mongoDrv.IndexModel{
		Keys: bson.D{{Key: "recipe_id", Value: 1}, {Key: "date", Value: 1}}}); err != nil {
		return nil, err
	}
	return s, nil
}

// FromEnv builds a Store from env (kept for symmetry; New reads env itself).
func FromEnv() (*Store, error) { return New() }

func (s *Store) AddIngredient(ctx context.Context, name, note string) (bson.M, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fail("ingredient name is required")
	}
	if len(note) > 300 {
		note = note[:300]
	}
	doc := bson.M{"name": name, "note": note, "createdAt": primitive.NewDateTimeFromTime(time.Now().UTC())}
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

func (s *Store) DeleteIngredient(ctx context.Context, nameOrID string) (bool, error) {
	ing, err := s.findIng(ctx, nameOrID)
	if err != nil {
		return false, err
	}
	var ref bson.M
	if err := s.recipes.FindOne(ctx,
		bson.M{"quantities.ingredient_id": mongo.IDString(ing["_id"])},
		options.FindOne().SetProjection(bson.M{"_id": 0, "name": 1})).Decode(&ref); err == nil {
		return false, fail("ingredient '%v' is used by recipe '%v' — edit the recipe first", ing["name"], ref["name"])
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

func (s *Store) resolveIngs(ctx context.Context, qtys map[string]any) ([]bson.M, error) {
	out := []bson.M{}
	for key, qty := range qtys {
		ing, err := s.findIng(ctx, key)
		if err != nil {
			return nil, fail("unknown ingredient '%s' — add it with add_ingredient first", key)
		}
		q := fmt.Sprint(qty)
		if len(q) > 100 {
			q = q[:100]
		}
		out = append(out, bson.M{"ingredient_id": mongo.IDString(ing["_id"]), "name": ing["name"], "qty": q})
	}
	if len(out) == 0 {
		return nil, fail("recipe needs at least one ingredient quantity")
	}
	return out, nil
}

func checkMacros(perServing map[string]any) (map[string]float64, error) {
	out := map[string]float64{}
	for _, k := range RecipeMacros {
		v, ok := perServing[k]
		if !ok {
			return nil, fail("per_serving missing '%s' — ask the user for it", k)
		}
		f, err := toFloat(v)
		if err != nil {
			return nil, fail("per_serving '%s' must be a number", k)
		}
		if f < 0 {
			return nil, fail("per_serving '%s' must be >= 0", k)
		}
		out[k] = validate.Round1(f)
	}
	return out, nil
}

func (s *Store) AddRecipe(ctx context.Context, name string, ingredientQtys map[string]any, perServing map[string]any, servings float64, note string, tags []string, source string) (bson.M, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fail("recipe name is required")
	}
	if servings <= 0 {
		return nil, fail("servings must be > 0")
	}
	macros, err := checkMacros(perServing)
	if err != nil {
		return nil, err
	}
	quantities, err := s.resolveIngs(ctx, ingredientQtys)
	if err != nil {
		return nil, err
	}
	if len(note) > 300 {
		note = note[:300]
	}
	capped := make([]string, 0, len(tags))
	for _, t := range tags {
		if len(t) > 40 {
			t = t[:40]
		}
		capped = append(capped, t)
		if len(capped) >= 20 {
			break
		}
	}
	if source == "" {
		source = "mcp"
	}
	if len(source) > 64 {
		source = source[:64]
	}
	now := primitive.NewDateTimeFromTime(time.Now().UTC())
	doc := bson.M{"name": name, "servings": servings, "quantities": quantities,
		"note": note, "tags": capped, "source": source, "createdAt": now, "updatedAt": now}
	for k, v := range macros {
		doc[k] = v
	}
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

func (s *Store) ListRecipes(ctx context.Context, search, tag, ingredient string) ([]bson.M, error) {
	filt := bson.M{}
	if strings.TrimSpace(search) != "" {
		filt["name"] = bson.M{"$regex": regexp.QuoteMeta(strings.TrimSpace(search)), "$options": "i"}
	}
	if strings.TrimSpace(tag) != "" {
		filt["tags"] = strings.TrimSpace(tag)
	}
	if strings.TrimSpace(ingredient) != "" {
		filt["quantities.name"] = bson.M{"$regex": regexp.QuoteMeta(strings.TrimSpace(ingredient)), "$options": "i"}
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
	upd := bson.M{"updatedAt": primitive.NewDateTimeFromTime(time.Now().UTC())}
	if v, ok := patch["name"].(string); ok && v != "" {
		upd["name"] = strings.TrimSpace(v)
	}
	if v, ok := patch["servings"]; ok && v != nil {
		f, err := toFloat(v)
		if err != nil || f <= 0 {
			return nil, fail("servings must be > 0")
		}
		upd["servings"] = f
	}
	if v, ok := patch["per_serving"].(map[string]any); ok && v != nil {
		macros, err := checkMacros(v)
		if err != nil {
			return nil, err
		}
		for k, f := range macros {
			upd[k] = f
		}
	}
	if v, ok := patch["ingredient_qtys"].(map[string]any); ok && v != nil {
		q, err := s.resolveIngs(ctx, v)
		if err != nil {
			return nil, err
		}
		upd["quantities"] = q
	}
	if v, ok := patch["note"].(string); ok {
		if len(v) > 300 {
			v = v[:300]
		}
		upd["note"] = v
	}
	if v, ok := patch["tags"].([]any); ok {
		capped := []string{}
		for _, t := range v {
			ts := fmt.Sprint(t)
			if len(ts) > 40 {
				ts = ts[:40]
			}
			capped = append(capped, ts)
			if len(capped) >= 20 {
				break
			}
		}
		upd["tags"] = capped
	}
	if v, ok := patch["source"].(string); ok {
		if len(v) > 64 {
			v = v[:64]
		}
		upd["source"] = v
	}
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

func (s *Store) DeleteRecipe(ctx context.Context, nameOrID string) (bool, int64, error) {
	r, err := s.GetRecipe(ctx, nameOrID)
	if err != nil || r == nil {
		return false, 0, err
	}
	oid, _ := primitive.ObjectIDFromHex(idstr(r["_id"]))
	logs, err := s.log.DeleteMany(ctx, bson.M{"recipe_id": oid})
	if err != nil {
		return false, 0, err
	}
	if _, err := s.recipes.DeleteOne(ctx, bson.M{"_id": oid}); err != nil {
		return false, 0, err
	}
	return true, logs.DeletedCount, nil
}

func (s *Store) ScaleRecipe(ctx context.Context, nameOrID string, servings float64) (bson.M, error) {
	r, err := s.GetRecipe(ctx, nameOrID)
	if err != nil || r == nil {
		return nil, fail("unknown recipe '%s'", nameOrID)
	}
	if servings <= 0 {
		return nil, fail("servings must be > 0")
	}
	base, _ := validate.ToFloat(r["servings"])
	if base == 0 {
		return nil, fail("recipe has zero servings — cannot scale")
	}
	f := servings / base
	get := func(k string) float64 { v, _ := validate.ToFloat(r[k]); return v }
	scaled, total, per := bson.M{}, bson.M{}, bson.M{}
	for _, k := range RecipeMacros {
		v := get(k)
		scaled[k] = float64(int(v*f*10+0.5)) / 10
		total[k] = float64(int(v*servings*10+0.5)) / 10
		per[k] = v
	}
	return bson.M{"name": r["name"], "base_servings": base, "target_servings": servings,
		"factor": float64(int(f*1000+0.5)) / 1000, "per_serving": per,
		"scaled_per_serving": scaled, "total": total, "quantities": r["quantities"],
		"qty_note": "qty strings are free text (e.g. '2 spoons') — not scaled"}, nil
}

func (s *Store) LogCook(ctx context.Context, recipe, cookingNote, aftertasteNote, date string) (bson.M, error) {
	r, err := s.GetRecipe(ctx, recipe)
	if err != nil || r == nil {
		return nil, fail("unknown recipe '%s'", recipe)
	}
	day := strings.TrimSpace(date)
	if day == "" {
		day = time.Now().Format("2006-01-02")
	} else if _, err := validate.CheckDay(day); err != nil {
		return nil, &StoreError{Msg: err.Error()}
	}
	oid, _ := primitive.ObjectIDFromHex(idstr(r["_id"]))
	doc := bson.M{"recipe_id": oid, "recipe_name": r["name"], "date": day,
		"cooking_note": trunc(cookingNote, 500), "aftertaste_note": trunc(aftertasteNote, 500),
		"createdAt": primitive.NewDateTimeFromTime(time.Now().UTC())}
	res, err := s.log.InsertOne(ctx, doc)
	if err != nil {
		return nil, err
	}
	doc["_id"] = res.InsertedID
	return docOut(doc), nil
}

func (s *Store) ListCooks(ctx context.Context, recipe string, limit int) ([]bson.M, error) {
	filt := bson.M{}
	if strings.TrimSpace(recipe) != "" {
		r, err := s.GetRecipe(ctx, strings.TrimSpace(recipe))
		if err != nil || r == nil {
			return nil, fail("unknown recipe '%s'", recipe)
		}
		oid, _ := primitive.ObjectIDFromHex(idstr(r["_id"]))
		filt["recipe_id"] = oid
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	cur, err := s.log.Find(ctx, filt,
		options.Find().SetSort(bson.D{{Key: "date", Value: -1}}).SetLimit(int64(limit)))
	if err != nil {
		return nil, err
	}
	return allOut(ctx, cur)
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

func toFloat(v any) (float64, error) { return validate.ToFloat(v) }

func idstr(v any) string { return mongo.IDString(v) }

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
