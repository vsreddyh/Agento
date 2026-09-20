// Package healthcheck implements MongoDB storage for the health-check MCP:
// hc_meals, hc_weight (never pruned), hc_days.
package healthcheck

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agento/internal/mongo"
	"agento/internal/validate"

	"go.mongodb.org/mongo-driver/bson"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// StoreError is the domain error.
type StoreError struct{ Msg string }

func (e *StoreError) Error() string { return e.Msg }

func fail(format string, args ...any) *StoreError {
	return &StoreError{Msg: fmt.Sprintf(format, args...)}
}

// Store binds hc_meals/hc_weight/hc_days + unique date indexes for upserts.
type Store struct {
	meals  *mongoDrv.Collection
	weight *mongoDrv.Collection
	days   *mongoDrv.Collection
}

func New() (*Store, error) {
	d, err := mongo.DB()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	s := &Store{
		meals:  d.Collection("hc_meals"),
		weight: d.Collection("hc_weight"),
		days:   d.Collection("hc_days"),
	}
	for _, c := range []struct {
		col  *mongoDrv.Collection
		keys bson.D
		opts *options.IndexOptions
	}{
		{s.weight, bson.D{{Key: "date", Value: 1}}, options.Index().SetUnique(true)},
		{s.days, bson.D{{Key: "date", Value: 1}}, options.Index().SetUnique(true)},
		{s.meals, bson.D{{Key: "date", Value: 1}}, nil},
	} {
		if _, err := c.col.Indexes().CreateOne(ctx, mongoDrv.IndexModel{Keys: c.keys, Options: c.opts}); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// FromEnv builds a Store from env (kept for symmetry; New reads env itself).
func FromEnv() (*Store, error) { return New() }

func checkItems(items []map[string]any) ([]map[string]any, error) {
	if len(items) == 0 {
		return nil, fail("log_meal needs at least one item with kcal/protein/carbs/fat/fiber")
	}
	out := make([]map[string]any, 0, len(items))
	for i, it := range items {
		name, _ := it["name"].(string)
		if strings.TrimSpace(name) == "" {
			return nil, fail("item[%d] needs a name", i)
		}
		conv := map[string]any{}
		for k, v := range it {
			conv[k] = v
		}
		if err := validate.CheckMacros(conv, fmt.Sprintf("item[%d] '%s'", i, name)); err != nil {
			return nil, &StoreError{Msg: err.Error()}
		}
		qty, _ := it["qty"].(string)
		if len(qty) > 100 {
			qty = qty[:100]
		}
		out = append(out, map[string]any{"name": strings.TrimSpace(name), "qty": qty,
			"kcal": fval(it["kcal"]), "protein": fval(it["protein"]), "carbs": fval(it["carbs"]),
			"fat": fval(it["fat"]), "fiber": fval(it["fiber"])})
	}
	return out, nil
}

func (s *Store) LogMeal(ctx context.Context, day, description string, items []map[string]any) (bson.M, error) {
	day, err := mustDay(day)
	if err != nil {
		return nil, err
	}
	checked, err := checkItems(items)
	if err != nil {
		return nil, err
	}
	doc := bson.M{"date": day, "items": checked, "totals": validate.SumTotals(checked),
		"createdAt": time.Now().UTC().Format(time.RFC3339)}
	res, err := s.meals.InsertOne(ctx, doc)
	if err != nil {
		return nil, err
	}
	doc["_id"] = mongo.IDString(res.InsertedID)
	return doc, nil
}

func (s *Store) QueryMeals(ctx context.Context, start, end string) ([]bson.M, error) {
	st, err := mustDay(start)
	if err != nil {
		return nil, err
	}
	en, err := mustDay(end)
	if err != nil {
		return nil, err
	}
	cur, err := s.meals.Find(ctx, bson.M{"date": bson.M{"$gte": st, "$lte": en}},
		options.Find().SetSort(bson.D{{Key: "date", Value: 1}}).SetLimit(500))
	if err != nil {
		return nil, err
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	if rows == nil {
		rows = []bson.M{}
	}
	for _, r := range rows {
		r["_id"] = mongo.IDString(r["_id"])
		if c, ok := r["createdAt"]; ok {
			r["createdAt"] = mongo.IDString(c)
		} else {
			r["createdAt"] = ""
		}
	}
	return rows, nil
}

func (s *Store) FixLastMeal(ctx context.Context, description string, items []map[string]any) (bson.M, error) {
	cur, err := s.meals.Find(ctx, bson.M{},
		options.Find().SetSort(bson.D{{Key: "createdAt", Value: -1}}).SetLimit(1))
	if err != nil {
		return nil, err
	}
	var last []bson.M
	if err := cur.All(ctx, &last); err != nil {
		return nil, err
	}
	if len(last) == 0 {
		return nil, nil
	}
	checked, err := checkItems(items)
	if err != nil {
		return nil, err
	}
	totals := validate.SumTotals(checked)
	if _, err := s.meals.UpdateOne(ctx, bson.M{"_id": last[0]["_id"]},
		bson.M{"$set": bson.M{"items": checked, "totals": totals}}); err != nil {
		return nil, err
	}
	return bson.M{"date": last[0]["date"], "totals": totals, "items": checked}, nil
}

func (s *Store) DeleteMeals(ctx context.Context, day string) (int64, error) {
	day, err := mustDay(day)
	if err != nil {
		return 0, err
	}
	res, err := s.meals.DeleteMany(ctx, bson.M{"date": day})
	if err != nil {
		return 0, err
	}
	return res.DeletedCount, nil
}

func (s *Store) LogWeight(ctx context.Context, day string, kg float64) (bson.M, error) {
	day, err := mustDay(day)
	if err != nil {
		return nil, err
	}
	if kg <= 0 || kg > 500 {
		return nil, fail("implausible weight %v kg", kg)
	}
	if _, err := s.weight.UpdateOne(ctx, bson.M{"date": day},
		bson.M{"$set": bson.M{"kg": kg, "createdAt": time.Now().UTC().Format(time.RFC3339)}},
		options.Update().SetUpsert(true)); err != nil {
		return nil, err
	}
	return bson.M{"date": day, "kg": kg}, nil
}

func (s *Store) LogSleep(ctx context.Context, day string, hours float64) (bson.M, error) {
	day, err := mustDay(day)
	if err != nil {
		return nil, err
	}
	if hours <= 0 || hours > 24 {
		return nil, fail("implausible sleep %v h", hours)
	}
	if _, err := s.days.UpdateOne(ctx, bson.M{"date": day},
		bson.M{"$set": bson.M{"sleep_hours": hours, "updatedAt": time.Now().UTC().Format(time.RFC3339)}},
		options.Update().SetUpsert(true)); err != nil {
		return nil, err
	}
	return bson.M{"date": day, "sleep_hours": hours}, nil
}

func (s *Store) LogWorkout(ctx context.Context, day, typ string, minutes, kcal float64) (bson.M, error) {
	day, err := mustDay(day)
	if err != nil {
		return nil, err
	}
	if minutes <= 0 {
		return nil, fail("workout minutes must be > 0")
	}
	if kcal < 0 {
		return nil, fail("workout kcal must be >= 0")
	}
	if len(typ) > 60 {
		typ = typ[:60]
	}
	if typ == "" {
		typ = "workout"
	}
	w := bson.M{"type": typ, "minutes": minutes, "kcal": kcal}
	if _, err := s.days.UpdateOne(ctx, bson.M{"date": day},
		bson.M{"$push": bson.M{"workouts": w}, "$set": bson.M{"updatedAt": time.Now().UTC().Format(time.RFC3339)}},
		options.Update().SetUpsert(true)); err != nil {
		return nil, err
	}
	out := bson.M{"date": day}
	for k, v := range w {
		out[k] = v
	}
	return out, nil
}

func (s *Store) DailySummary(ctx context.Context, day string) (bson.M, error) {
	day, err := mustDay(day)
	if err != nil {
		return nil, err
	}
	cur, err := s.meals.Find(ctx, bson.M{"date": day}, options.Find().SetProjection(bson.M{"_id": 0}))
	if err != nil {
		return nil, err
	}
	var meals []bson.M
	if err := cur.All(ctx, &meals); err != nil {
		return nil, err
	}
	items := []map[string]any{}
	for _, m := range meals {
		if arr, ok := m["items"].(bson.A); ok {
			for _, e := range arr {
				if mm, ok := e.(bson.M); ok {
					conv := map[string]any{}
					for k, v := range mm {
						conv[k] = v
					}
					items = append(items, conv)
				}
			}
		}
	}
	totals := validate.SumTotals(items)
	var w, d bson.M
	_ = s.weight.FindOne(ctx, bson.M{"date": day}, options.FindOne().SetProjection(bson.M{"_id": 0})).Decode(&w)
	_ = s.days.FindOne(ctx, bson.M{"date": day}, options.FindOne().SetProjection(bson.M{"_id": 0})).Decode(&d)
	burn := 0.0
	var wo []any
	if d != nil {
		if arr, ok := d["workouts"].(bson.A); ok {
			for _, e := range arr {
				wo = append(wo, e)
				if mm, ok := e.(bson.M); ok {
					burn += fval(mm["kcal"])
				}
			}
		}
		if a, ok := d["active_kcal"]; ok {
			burn += fval(a)
		}
	}
	if wo == nil {
		wo = []any{}
	}
	burn = float64(int(burn*10+0.5)) / 10
	in := totals["kcal"]
	return bson.M{"date": day, "meals": len(meals), "totals": totals,
		"weight": w, "day": d, "workouts": wo,
		"cal_in": in, "cal_out": burn,
		"net": float64(int((in-burn)*10+0.5)) / 10}, nil
}

func (s *Store) Prune(ctx context.Context, days int, dryRun bool) (bson.M, error) {
	cutoff := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	filt := bson.M{"date": bson.M{"$lt": cutoff}}
	mc, err := s.meals.CountDocuments(ctx, filt)
	if err != nil {
		return nil, err
	}
	dc, err := s.days.CountDocuments(ctx, filt)
	if err != nil {
		return nil, err
	}
	if !dryRun {
		if _, err := s.meals.DeleteMany(ctx, filt); err != nil {
			return nil, err
		}
		if _, err := s.days.DeleteMany(ctx, filt); err != nil {
			return nil, err
		}
	}
	return bson.M{"cutoff": cutoff, "meals": mc, "days": dc}, nil
}

func mustDay(day string) (string, error) {
	d, err := validate.CheckDay(day)
	if err != nil {
		return "", &StoreError{Msg: err.Error()}
	}
	return d, nil
}

func fval(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int32:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}
