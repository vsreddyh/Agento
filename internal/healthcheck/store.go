// Package healthcheck implements MongoDB storage for the health-check MCP:
// hc_food_log (flat per-item rows), hc_weight (one row per date),
// hc_days (sync scalars), hc_sleep (one row per session) and hc_workouts
// (one row per workout).
//
// All five collections are permanent: no TTL index, no prune tool, no
// retention job touches them. Deletes are hard deletes by id (food, sleep
// entries, workouts) or by date (weight), and only on explicit request —
// dry_run reports first, like miser-money's delete_transactions.
//
// Conventions mirror internal/money: StoreError/fail for domain errors,
// trimmed inputs, friendly duplicate-key errors, _id as hex, createdAt as
// BSON DateTime UTC, derived values computed on read and never stored
// (food totals, sleep hours, daily summaries), named indexes in
// EnsureSchema with legacy cleanup.
//
// One structural lesson is baked in: dedup hashes live on top-level
// documents with scalar unique indexes, never on array paths. Unique
// multikey indexes do not enforce on this cluster (probed: duplicate
// array-element values insert cleanly), while scalar unique indexes
// return E11000 — so sessions and workouts are collections, not subdocs.
package healthcheck

import (
	"context"
	"crypto/sha1"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"agento/internal/mongo"
	"agento/internal/validate"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	mongoDrv "go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

const (
	foodColl     = "hc_food_log"
	weightColl   = "hc_weight"
	daysColl     = "hc_days"
	sleepColl    = "hc_sleep"
	workoutsColl = "hc_workouts"
	// listCap bounds every list_* result. Food volume never justifies
	// miser-style paging (limit/offset); a full page means narrow the dates.
	listCap = 500
)

// Sort directions. Both list enums (sort_by per list, sort here) reject
// unknown values with fail() naming the allowed set — never a silent
// default — and the applied values are echoed in every list response.
const (
	SortAsc  = "asc"
	SortDesc = "desc"
)

// Sort fields per list. Each list_* names only its own fields.
var (
	FoodSortFields    = []string{"datetime", "kcal"}
	WeightSortFields  = []string{"date", "kg"}
	SleepSortFields   = []string{"date", "hours"}
	WorkoutSortFields = []string{"date", "minutes", "kcal"}
)

// MacroKeys is the 5-macro set shared by food rows.
var MacroKeys = []string{"kcal", "protein", "carbs", "fat", "fiber"}

// StoreError is the domain error.
type StoreError struct{ Msg string }

func (e *StoreError) Error() string { return e.Msg }

func fail(format string, args ...any) *StoreError {
	return &StoreError{Msg: fmt.Sprintf(format, args...)}
}

// Store binds the five health collections.
type Store struct {
	db       *mongoDrv.Database
	food     *mongoDrv.Collection
	weight   *mongoDrv.Collection
	days     *mongoDrv.Collection
	sleep    *mongoDrv.Collection
	workouts *mongoDrv.Collection
}

func New() (*Store, error) {
	d, err := mongo.DB()
	if err != nil {
		return nil, err
	}
	s := &Store{db: d, food: d.Collection(foodColl), weight: d.Collection(weightColl),
		days: d.Collection(daysColl), sleep: d.Collection(sleepColl), workouts: d.Collection(workoutsColl)}
	if err := s.EnsureSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

// FromEnv builds a Store from env (kept for symmetry; New reads env itself).
func FromEnv() (*Store, error) { return New() }

// EnsureSchema creates the named indexes and removes the machinery the
// meals era left behind: the hc_meals collection and the unnamed date_1
// indexes (superseded by the uniq_date names below). Idempotent — reruns
// are cheap no-ops.
func (s *Store) EnsureSchema(ctx context.Context) error {
	if err := s.dropCollectionIfExists(ctx, "hc_meals"); err != nil {
		return err
	}
	// Legacy unnamed indexes first: the meals era created date_1 on the
	// same keys uniq_date now owns, and creating a second name errors.
	for _, col := range []*mongoDrv.Collection{s.weight, s.days} {
		if err := dropIndexIfNamed(ctx, col, "date_1"); err != nil {
			return err
		}
	}
	for _, c := range []struct {
		col  *mongoDrv.Collection
		keys bson.D
		opts *options.IndexOptions
	}{
		{s.food, bson.D{{Key: "day", Value: 1}}, options.Index().SetName("food_day")},
		{s.food, bson.D{{Key: "datetime", Value: 1}}, options.Index().SetName("food_datetime")},
		{s.food, bson.D{{Key: "what_lc", Value: 1}}, options.Index().SetName("food_whatlc")},
		{s.weight, bson.D{{Key: "date", Value: 1}}, options.Index().SetUnique(true).SetName("uniq_date")},
		{s.days, bson.D{{Key: "date", Value: 1}}, options.Index().SetUnique(true).SetName("uniq_date")},
		{s.sleep, bson.D{{Key: "day", Value: 1}}, options.Index().SetName("sleep_day")},
		{s.sleep, bson.D{{Key: "hash", Value: 1}}, options.Index().SetUnique(true).SetName("uniq_hash")},
		{s.workouts, bson.D{{Key: "day", Value: 1}}, options.Index().SetName("workout_day")},
		{s.workouts, bson.D{{Key: "hash", Value: 1}}, options.Index().SetUnique(true).SetName("uniq_hash")},
	} {
		if _, err := c.col.Indexes().CreateOne(ctx, mongoDrv.IndexModel{Keys: c.keys, Options: c.opts}); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) dropCollectionIfExists(ctx context.Context, name string) error {
	names, err := s.db.ListCollectionNames(ctx, bson.M{"name": name})
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	return s.db.Collection(name).Drop(ctx)
}

func dropIndexIfNamed(ctx context.Context, col *mongoDrv.Collection, name string) error {
	cur, err := col.Indexes().List(ctx)
	if err != nil {
		return err
	}
	defer cur.Close(ctx)
	for cur.Next(ctx) {
		var spec bson.M
		if err := cur.Decode(&spec); err != nil {
			return err
		}
		if n, _ := spec["name"].(string); n == name {
			_, err := col.Indexes().DropOne(ctx, name)
			return err
		}
	}
	return cur.Err()
}

// checkSort validates a list's sort pair. Unknown values fail naming the
// allowed set — the schema enum stops them first, this is the second layer
// for direct store callers.
func checkSort(tool, sortBy string, allowed []string, dir string) (bool, error) {
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
		return false, fail("%s sort_by must be one of %v, got '%s'", tool, allowed, sortBy)
	}
	switch dir {
	case "", SortAsc:
		return true, nil
	case SortDesc:
		return false, nil
	default:
		return false, fail("%s sort must be one of [asc desc], got '%s'", tool, dir)
	}
}

func sortDir(asc bool) int {
	if asc {
		return 1
	}
	return -1
}

func mustDay(day string) (string, error) {
	d, err := validate.CheckDay(day)
	if err != nil {
		return "", &StoreError{Msg: err.Error()}
	}
	return d, nil
}

// hexID renders an _id as hex, like miser-money's oidHex.
// IST is the day-bucketing zone every writer shares: calendar dates are
// IST midnights, matching the app's localDate. Fixed offset, always
// available — LoadLocation depends on tzdata the minimal container may
// not have, which once bucketed the same instant to different days.
func IST() *time.Location {
	return time.FixedZone("IST", 5*3600+30*60)
}

func hexID(v any) string {
	if o, ok := v.(primitive.ObjectID); ok {
		return o.Hex()
	}
	return fmt.Sprint(v)
}

// contentHash is the deterministic dedup key for a sleep session or
// workout entry. Same content re-sent (a re-sync, a double-tap) hashes
// identically and hits the scalar unique index instead of inserting
// twice. Entry ids are separate random ObjectIDs: stable identity for
// correct/delete even when a correction changes the content.
func contentHash(parts ...string) string {
	sum := sha1.Sum([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf("%x", sum)[:16]
}

// round1 rounds to one decimal, like validate.Round1.
func round1(f float64) float64 { return validate.Round1(f) }

// checkMacros validates provided macros: each present key must be numeric
// and >= 0, rounded to one decimal. Absent keys mean "not provided", never
// zero — food macros are optional per #601.
func checkMacros(macros map[string]float64) (map[string]float64, error) {
	out := map[string]float64{}
	for k, v := range macros {
		known := false
		for _, m := range MacroKeys {
			if k == m {
				known = true
				break
			}
		}
		if !known {
			return nil, fail("unknown macro '%s' — use one of %v", k, MacroKeys)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fail("macro '%s' must be a number", k)
		}
		if v < 0 {
			return nil, fail("macro '%s' must be >= 0", k)
		}
		out[k] = round1(v)
	}
	return out, nil
}

// sumTenths adds values in integer tenths so a day's totals can't drift
// (0.1+0.2 stays 0.3) — the miser int64-paise rule, one decimal up.
func sumTenths(vals []float64) float64 {
	var t int64
	for _, v := range vals {
		t += int64(math.Round(v * 10))
	}
	return float64(t) / 10
}

// foodTotals derives a day's macro totals from its rows. Totals live only
// in responses, never in storage — a correction shows up on the next read.
func (s *Store) foodTotals(ctx context.Context, day string) (map[string]float64, int64, error) {
	cur, err := s.food.Find(ctx, bson.M{"day": day})
	if err != nil {
		return nil, 0, err
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return nil, 0, err
	}
	totals := map[string]float64{}
	for _, m := range MacroKeys {
		vals := make([]float64, 0, len(rows))
		for _, r := range rows {
			if v, ok := r[m]; ok {
				if f, err := validate.ToFloat(v); err == nil {
					vals = append(vals, f)
				}
			}
		}
		totals[m] = sumTenths(vals)
	}
	return totals, int64(len(rows)), nil
}

// LogFood records one flat food entry. Macros are optional (#601); absent
// means unknown, never zero. Two identical rows can both be real (ate it
// twice), so food has no dedup hash — unlike sleep/workouts below.
func (s *Store) LogFood(ctx context.Context, day string, dt time.Time, what, qty string, macros map[string]float64, note string) (bson.M, error) {
	day, err := mustDay(day)
	if err != nil {
		return nil, err
	}
	what = strings.TrimSpace(what)
	if what == "" {
		return nil, fail("log_food needs what you ate")
	}
	checked, err := checkMacros(macros)
	if err != nil {
		return nil, err
	}
	doc := bson.M{"datetime": primitive.NewDateTimeFromTime(dt.UTC()), "day": day,
		"what": what, "what_lc": strings.ToLower(what),
		"qty": strings.TrimSpace(qty), "createdAt": primitive.NewDateTimeFromTime(time.Now().UTC())}
	for k, v := range checked {
		doc[k] = v
	}
	if note = strings.TrimSpace(note); note != "" {
		doc["note"] = note
	}
	res, err := s.food.InsertOne(ctx, doc)
	if err != nil {
		return nil, err
	}
	doc["_id"] = hexID(res.InsertedID)
	return foodOut(doc), nil
}

func foodOut(doc bson.M) bson.M {
	out := bson.M{"_id": hexID(doc["_id"]), "day": doc["day"], "what": doc["what"], "qty": doc["qty"]}
	if v, ok := doc["datetime"]; ok {
		out["datetime"] = fmt.Sprint(v)
	}
	for _, m := range MacroKeys {
		if v, ok := doc[m]; ok {
			out[m] = v
		}
	}
	if v, ok := doc["note"]; ok {
		out["note"] = v
	}
	if v, ok := doc["createdAt"]; ok {
		out["createdAt"] = fmt.Sprint(v)
	}
	return out
}

// GetFood fetches one entry by id hex. Unknown ids fail — an id addresses
// one row, absence is a wrong address, not an empty filter.
func (s *Store) GetFood(ctx context.Context, id string) (bson.M, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("'%s' is not a food id", id)
	}
	var doc bson.M
	if err := s.food.FindOne(ctx, bson.M{"_id": oid}).Decode(&doc); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return nil, fail("unknown food '%s'", id)
		}
		return nil, err
	}
	return foodOut(doc), nil
}

// ListFood scans a day range, datetime-ordered by default. The what filter
// is exact match on the lowercased copy — equal-or-nothing, never
// substring. A filter matching nothing returns the empty page, not an
// error; only the enums and the dates can fail.
func (s *Store) ListFood(ctx context.Context, start, end, sortBy, dir, what string) ([]bson.M, error) {
	st, err := mustDay(start)
	if err != nil {
		return nil, err
	}
	en, err := mustDay(end)
	if err != nil {
		return nil, err
	}
	asc, err := checkSort("list_food", sortBy, FoodSortFields, dir)
	if err != nil {
		return nil, err
	}
	if sortBy == "" {
		sortBy = FoodSortFields[0]
	}
	filt := bson.M{"day": bson.M{"$gte": st, "$lte": en}}
	if w := strings.TrimSpace(what); w != "" {
		filt["what_lc"] = strings.ToLower(w)
	}
	key := "datetime"
	if sortBy == "kcal" {
		key = "kcal"
	}
	cur, err := s.food.Find(ctx, filt, options.Find().
		SetSort(bson.D{{Key: key, Value: sortDir(asc)}, {Key: "_id", Value: sortDir(asc)}}).
		SetLimit(listCap))
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
	out := make([]bson.M, 0, len(rows))
	for _, r := range rows {
		out = append(out, foodOut(r))
	}
	return out, nil
}

// CorrectFood patches one entry by id: supplied fields replace, omitted
// fields keep. A macro set to nil clears it; what also refreshes what_lc;
// datetime also refreshes day.
func (s *Store) CorrectFood(ctx context.Context, id string, what, qty, note *string, macros map[string]*float64, dt *time.Time) (bson.M, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("'%s' is not a food id", id)
	}
	upd := bson.M{}
	unset := bson.M{}
	if what != nil {
		if w := strings.TrimSpace(*what); w == "" {
			return nil, fail("what cannot be cleared to empty")
		} else {
			upd["what"] = w
			upd["what_lc"] = strings.ToLower(w)
		}
	}
	if qty != nil {
		upd["qty"] = strings.TrimSpace(*qty)
	}
	if note != nil {
		if n := strings.TrimSpace(*note); n != "" {
			upd["note"] = n
		} else {
			unset["note"] = ""
		}
	}
	for k, v := range macros {
		known := false
		for _, m := range MacroKeys {
			if k == m {
				known = true
				break
			}
		}
		if !known {
			return nil, fail("unknown macro '%s' — use one of %v", k, MacroKeys)
		}
		if v == nil {
			unset[k] = ""
			continue
		}
		if math.IsNaN(*v) || math.IsInf(*v, 0) {
			return nil, fail("macro '%s' must be a number", k)
		}
		if *v < 0 {
			return nil, fail("macro '%s' must be >= 0", k)
		}
		upd[k] = round1(*v)
	}
	if dt != nil {
		upd["datetime"] = primitive.NewDateTimeFromTime(dt.UTC())
		upd["day"] = dt.In(IST()).Format("2006-01-02")
	}
	if len(upd) == 0 && len(unset) == 0 {
		return nil, fail("correct_food needs at least one field to change")
	}
	update := bson.M{"$set": upd}
	if len(unset) > 0 {
		update["$unset"] = unset
	}
	res, err := s.food.UpdateOne(ctx, bson.M{"_id": oid}, update)
	if err != nil {
		return nil, err
	}
	if res.MatchedCount == 0 {
		return nil, fail("unknown food '%s'", id)
	}
	return s.GetFood(ctx, id)
}

// DeleteFood hard-deletes one entry by id. dry_run reports the row and
// deletes nothing; there is no TTL and no restore — permanent means the
// agent confirms first.
func (s *Store) DeleteFood(ctx context.Context, id string, dryRun bool) (bson.M, error) {
	row, err := s.GetFood(ctx, id)
	if err != nil {
		return nil, err
	}
	if dryRun {
		return bson.M{"would_delete": 1, "food": row}, nil
	}
	if _, err := s.food.DeleteOne(ctx, bson.M{"_id": mustOID(id)}); err != nil {
		return nil, err
	}
	return bson.M{"deleted": 1}, nil
}

func mustOID(id string) primitive.ObjectID {
	oid, _ := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	return oid
}

// LogWeight upserts one row per date. Re-logging is the correction — no
// separate correct tool.
func (s *Store) LogWeight(ctx context.Context, day string, kg float64) (bson.M, error) {
	day, err := mustDay(day)
	if err != nil {
		return nil, err
	}
	if kg <= 0 || kg > 500 {
		return nil, fail("implausible weight %v kg", kg)
	}
	if _, err := s.weight.UpdateOne(ctx, bson.M{"date": day},
		bson.M{"$set": bson.M{"kg": kg},
			"$setOnInsert": bson.M{"createdAt": primitive.NewDateTimeFromTime(time.Now().UTC())}},
		options.Update().SetUpsert(true)); err != nil {
		return nil, err
	}
	return bson.M{"date": day, "kg": kg}, nil
}

// ListWeight scans a day range. sort_by is date|kg.
func (s *Store) ListWeight(ctx context.Context, start, end, sortBy, dir string) ([]bson.M, error) {
	st, err := mustDay(start)
	if err != nil {
		return nil, err
	}
	en, err := mustDay(end)
	if err != nil {
		return nil, err
	}
	asc, err := checkSort("list_weight", sortBy, WeightSortFields, dir)
	if err != nil {
		return nil, err
	}
	if sortBy == "" {
		sortBy = WeightSortFields[0]
	}
	key := "date"
	if sortBy == "kg" {
		key = "kg"
	}
	cur, err := s.weight.Find(ctx, bson.M{"date": bson.M{"$gte": st, "$lte": en}},
		options.Find().SetSort(bson.D{{Key: key, Value: sortDir(asc)}, {Key: "_id", Value: sortDir(asc)}}).SetLimit(listCap))
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
	out := make([]bson.M, 0, len(rows))
	for _, r := range rows {
		out = append(out, bson.M{"date": r["date"], "kg": r["kg"]})
	}
	return out, nil
}

// DeleteWeight hard-deletes one date's row. A valid date with no row is
// the empty result ({deleted: 0}), not an error.
func (s *Store) DeleteWeight(ctx context.Context, day string, dryRun bool) (bson.M, error) {
	day, err := mustDay(day)
	if err != nil {
		return nil, err
	}
	var row bson.M
	err = s.weight.FindOne(ctx, bson.M{"date": day}).Decode(&row)
	if err != nil && err != mongoDrv.ErrNoDocuments {
		return nil, err
	}
	if dryRun {
		if row == nil {
			return bson.M{"would_delete": 0}, nil
		}
		return bson.M{"would_delete": 1, "weight": bson.M{"date": row["date"], "kg": row["kg"]}}, nil
	}
	res, err := s.weight.DeleteOne(ctx, bson.M{"date": day})
	if err != nil {
		return nil, err
	}
	return bson.M{"deleted": int(res.DeletedCount)}, nil
}

// SleepIn is one sleep session: sync sessions carry a start, manual
// log_sleep entries carry minutes only. sleep_hours is always derived
// from session minutes — never stored, never set directly.
type SleepIn struct {
	Day     string
	Start   *time.Time
	Minutes int
}

// insertSleepSession inserts one session row; duplicate content hits the
// scalar unique hash index. Sync calls tolerate that (re-sent session =
// already there, skip quietly); manual calls surface it as a friendly
// duplicate error (double log = user feedback).
func (s *Store) insertSleepSession(ctx context.Context, day string, start *time.Time, minutes int, skipDup bool) (bson.M, error) {
	day, err := mustDay(day)
	if err != nil {
		return nil, err
	}
	if minutes <= 0 {
		return nil, fail("sleep session minutes must be > 0")
	}
	hashBase := day + "|manual"
	doc := bson.M{"day": day, "minutes": minutes,
		"createdAt": primitive.NewDateTimeFromTime(time.Now().UTC())}
	if start != nil {
		doc["start"] = primitive.NewDateTimeFromTime(start.UTC())
		hashBase = day + "|" + start.UTC().Format(time.RFC3339)
	}
	doc["hash"] = contentHash(hashBase, fmt.Sprint(minutes))
	res, err := s.sleep.InsertOne(ctx, doc)
	if err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			if skipDup {
				return bson.M{"skipped": true}, nil
			}
			return nil, fail("that sleep is already recorded for %s", day)
		}
		return nil, err
	}
	doc["_id"] = hexID(res.InsertedID)
	return sleepOut(doc), nil
}

// LogSleep records a manual sleep (hours, no start time). Sync sessions
// arrive through RecordSync; both land as rows in hc_sleep.
func (s *Store) LogSleep(ctx context.Context, day string, hours float64) (bson.M, error) {
	day, err := mustDay(day)
	if err != nil {
		return nil, err
	}
	if hours <= 0 || hours > 24 {
		return nil, fail("implausible sleep %v h", hours)
	}
	minutes := int(math.Round(hours * 60))
	entry, err := s.insertSleepSession(ctx, day, nil, minutes, false)
	if err != nil {
		return nil, err
	}
	return bson.M{"date": day, "sleep_hours": round1(float64(minutes) / 60), "session": entry}, nil
}

func sleepOut(doc bson.M) bson.M {
	out := bson.M{"id": hexID(doc["_id"]), "minutes": doc["minutes"]}
	if v, ok := doc["day"]; ok {
		out["day"] = v
	}
	if v, ok := doc["start"]; ok {
		out["start"] = fmt.Sprint(v)
	}
	return out
}

// ListSleep returns per-day docs with derived hours and full sessions.
// sort_by is date|hours (the day's derived total).
func (s *Store) ListSleep(ctx context.Context, start, end, sortBy, dir string) ([]bson.M, error) {
	st, err := mustDay(start)
	if err != nil {
		return nil, err
	}
	en, err := mustDay(end)
	if err != nil {
		return nil, err
	}
	asc, err := checkSort("list_sleep", sortBy, SleepSortFields, dir)
	if err != nil {
		return nil, err
	}
	if sortBy == "" {
		sortBy = SleepSortFields[0]
	}
	cur, err := s.sleep.Find(ctx, bson.M{"day": bson.M{"$gte": st, "$lte": en}},
		options.Find().SetSort(bson.D{{Key: "day", Value: sortDir(asc)}, {Key: "_id", Value: sortDir(asc)}}).SetLimit(listCap))
	if err != nil {
		return nil, err
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	byDay := map[string][]bson.M{}
	order := []string{}
	for _, r := range rows {
		day, _ := r["day"].(string)
		if _, ok := byDay[day]; !ok {
			order = append(order, day)
		}
		byDay[day] = append(byDay[day], r)
	}
	if !asc {
		for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
			order[i], order[j] = order[j], order[i]
		}
	}
	out := []bson.M{}
	for _, day := range order {
		sessions := make([]bson.M, 0, len(byDay[day]))
		for _, se := range byDay[day] {
			sessions = append(sessions, sleepOut(se))
		}
		out = append(out, bson.M{"date": day, "sleep_hours": sleepHours(sessions), "sessions": sessions})
	}
	if sortBy == "hours" {
		sortDaysByFloat(out, "sleep_hours", asc)
	}
	return out, nil
}

// sortDaysByFloat orders day rows by a numeric key, same direction as the
// requested sort. Ties keep date order.
func sortDaysByFloat(rows []bson.M, key string, asc bool) {
	sort.SliceStable(rows, func(i, j int) bool {
		fi, _ := validate.ToFloat(rows[i][key])
		fj, _ := validate.ToFloat(rows[j][key])
		if fi == fj {
			di, _ := rows[i]["date"].(string)
			dj, _ := rows[j]["date"].(string)
			if asc {
				return di < dj
			}
			return di > dj
		}
		if asc {
			return fi < fj
		}
		return fi > fj
	})
}

func sleepMinutes(sessions []bson.M) int {
	total := 0
	for _, se := range sessions {
		if f, err := validate.ToFloat(se["minutes"]); err == nil {
			total += int(f)
		}
	}
	return total
}

func sleepHours(sessions []bson.M) float64 {
	return round1(float64(sleepMinutes(sessions)) / 60)
}

// CorrectSleep replaces one session's minutes by entry id. The hash is
// recomputed; colliding with a sibling session fails instead of merging.
func (s *Store) CorrectSleep(ctx context.Context, id string, minutes int) (bson.M, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("'%s' is not a sleep id", id)
	}
	if minutes <= 0 {
		return nil, fail("sleep session minutes must be > 0")
	}
	var doc bson.M
	if err := s.sleep.FindOne(ctx, bson.M{"_id": oid}).Decode(&doc); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return nil, fail("unknown sleep '%s'", id)
		}
		return nil, err
	}
	day, _ := doc["day"].(string)
	hashBase := day + "|manual"
	// Start may be a BSON Date (current writes) or a string (pre-v2
	// rows): handle both, so the recomputed hash keys on the real start
	// instead of falling back to manual.
	switch v := doc["start"].(type) {
	case primitive.DateTime:
		t := v.Time()
		hashBase = day + "|" + t.UTC().Format(time.RFC3339)
	case string:
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			hashBase = day + "|" + t.UTC().Format(time.RFC3339)
		}
	}
	_, err = s.sleep.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$set": bson.M{
		"minutes": minutes, "hash": contentHash(hashBase, fmt.Sprint(minutes))}})
	if err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			return nil, fail("that sleep is already recorded for %s", day)
		}
		return nil, err
	}
	return bson.M{"date": day, "id": id, "minutes": minutes}, nil
}

// DeleteSleep hard-deletes one session by entry id. Unknown ids fail;
// dry_run reports first.
func (s *Store) DeleteSleep(ctx context.Context, id string, dryRun bool) (bson.M, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("'%s' is not a sleep id", id)
	}
	var doc bson.M
	if err := s.sleep.FindOne(ctx, bson.M{"_id": oid}).Decode(&doc); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return nil, fail("unknown sleep '%s'", id)
		}
		return nil, err
	}
	if dryRun {
		return bson.M{"would_delete": 1, "sleep": sleepOut(doc)}, nil
	}
	if _, err := s.sleep.DeleteOne(ctx, bson.M{"_id": oid}); err != nil {
		return nil, err
	}
	return bson.M{"deleted": 1}, nil
}

// WorkoutIn is one workout, from either writer. Type is free text,
// lowercased+trimmed on write so "Run" and "run" are one type.
type WorkoutIn struct {
	Day     string
	Type    string
	Minutes float64
	Kcal    float64
}

func normWorkoutType(t string) string {
	if t = strings.ToLower(strings.TrimSpace(t)); t == "" {
		return "other"
	}
	if len(t) > 60 {
		t = t[:60]
	}
	return t
}

func insertWorkout(ctx context.Context, col *mongoDrv.Collection, w WorkoutIn, skipDup bool) (bson.M, error) {
	day, err := mustDay(w.Day)
	if err != nil {
		return nil, err
	}
	typ := normWorkoutType(w.Type)
	if w.Minutes <= 0 {
		return nil, fail("workout minutes must be > 0")
	}
	if w.Kcal < 0 {
		return nil, fail("workout kcal must be >= 0")
	}
	minutes := round1(w.Minutes)
	kcal := round1(w.Kcal)
	doc := bson.M{"day": day, "type": typ, "minutes": minutes, "kcal": kcal,
		"hash":      contentHash(day, typ, fmt.Sprint(minutes), fmt.Sprint(kcal)),
		"createdAt": primitive.NewDateTimeFromTime(time.Now().UTC())}
	res, err := col.InsertOne(ctx, doc)
	if err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			if skipDup {
				return bson.M{"skipped": true}, nil
			}
			return nil, fail("that workout is already recorded for %s", day)
		}
		return nil, err
	}
	doc["_id"] = hexID(res.InsertedID)
	return workoutOut(doc), nil
}

// LogWorkout records one manual workout.
func (s *Store) LogWorkout(ctx context.Context, day, typ string, minutes, kcal float64) (bson.M, error) {
	return insertWorkout(ctx, s.workouts, WorkoutIn{Day: day, Type: typ, Minutes: minutes, Kcal: kcal}, false)
}

func workoutOut(doc bson.M) bson.M {
	out := bson.M{"id": hexID(doc["_id"]), "type": doc["type"], "minutes": doc["minutes"], "kcal": doc["kcal"]}
	if v, ok := doc["day"]; ok {
		out["day"] = v
	}
	return out
}

func workoutKcal(wos []bson.M) float64 {
	var vals []float64
	for _, w := range wos {
		if f, err := validate.ToFloat(w["kcal"]); err == nil {
			vals = append(vals, f)
		}
	}
	return sumTenths(vals)
}

func workoutMinutes(wos []bson.M) float64 {
	var vals []float64
	for _, w := range wos {
		if f, err := validate.ToFloat(w["minutes"]); err == nil {
			vals = append(vals, f)
		}
	}
	return sumTenths(vals)
}

// ListWorkouts returns per-day docs. sort_by is date|minutes|kcal, where
// minutes/kcal sort by the day's derived totals. The type filter is exact
// match on the lowered type.
func (s *Store) ListWorkouts(ctx context.Context, start, end, sortBy, dir, typ string) ([]bson.M, error) {
	st, err := mustDay(start)
	if err != nil {
		return nil, err
	}
	en, err := mustDay(end)
	if err != nil {
		return nil, err
	}
	asc, err := checkSort("list_workouts", sortBy, WorkoutSortFields, dir)
	if err != nil {
		return nil, err
	}
	if sortBy == "" {
		sortBy = WorkoutSortFields[0]
	}
	filt := bson.M{"day": bson.M{"$gte": st, "$lte": en}}
	if t := strings.TrimSpace(typ); t != "" {
		filt["type"] = normWorkoutType(t)
	}
	cur, err := s.workouts.Find(ctx, filt, options.Find().
		SetSort(bson.D{{Key: "day", Value: sortDir(asc)}, {Key: "_id", Value: sortDir(asc)}}).SetLimit(listCap))
	if err != nil {
		return nil, err
	}
	var rows []bson.M
	if err := cur.All(ctx, &rows); err != nil {
		return nil, err
	}
	byDay := map[string][]bson.M{}
	order := []string{}
	for _, r := range rows {
		day, _ := r["day"].(string)
		if _, ok := byDay[day]; !ok {
			order = append(order, day)
		}
		byDay[day] = append(byDay[day], r)
	}
	if !asc {
		for i, j := 0, len(order)-1; i < j; i, j = i+1, j-1 {
			order[i], order[j] = order[j], order[i]
		}
	}
	out := []bson.M{}
	for _, day := range order {
		wos := make([]bson.M, 0, len(byDay[day]))
		for _, w := range byDay[day] {
			wos = append(wos, workoutOut(w))
		}
		if len(wos) == 0 {
			continue
		}
		row := bson.M{"date": day, "workouts": wos}
		if sortBy == "minutes" {
			row["_sort"] = workoutMinutes(wos)
		} else if sortBy == "kcal" {
			row["_sort"] = workoutKcal(wos)
		}
		out = append(out, row)
	}
	if sortBy != "date" {
		sortDaysByFloat(out, "_sort", asc)
		for _, r := range out {
			delete(r, "_sort")
		}
	}
	return out, nil
}

// CorrectWorkout patches one workout by id: supplied fields replace,
// omitted keep. Changing type/minutes/kcal recomputes the hash; colliding
// with a sibling fails instead of merging.
func (s *Store) CorrectWorkout(ctx context.Context, id string, typ *string, minutes, kcal *float64) (bson.M, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("'%s' is not a workout id", id)
	}
	var doc bson.M
	if err := s.workouts.FindOne(ctx, bson.M{"_id": oid}).Decode(&doc); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return nil, fail("unknown workout '%s'", id)
		}
		return nil, err
	}
	day, _ := doc["day"].(string)
	set := bson.M{}
	newType, _ := doc["type"].(string)
	newMin, _ := validate.ToFloat(doc["minutes"])
	newKcal, _ := validate.ToFloat(doc["kcal"])
	if typ != nil {
		newType = normWorkoutType(*typ)
		set["type"] = newType
	}
	if minutes != nil {
		if *minutes <= 0 {
			return nil, fail("workout minutes must be > 0")
		}
		newMin = round1(*minutes)
		set["minutes"] = newMin
	}
	if kcal != nil {
		if *kcal < 0 {
			return nil, fail("workout kcal must be >= 0")
		}
		newKcal = round1(*kcal)
		set["kcal"] = newKcal
	}
	if len(set) == 0 {
		return nil, fail("correct_workout needs at least one field to change")
	}
	set["hash"] = contentHash(day, newType, fmt.Sprint(newMin), fmt.Sprint(newKcal))
	_, err = s.workouts.UpdateOne(ctx, bson.M{"_id": oid}, bson.M{"$set": set})
	if err != nil {
		if mongoDrv.IsDuplicateKeyError(err) {
			return nil, fail("that workout is already recorded for %s", day)
		}
		return nil, err
	}
	return bson.M{"date": day, "id": id, "type": newType, "minutes": newMin, "kcal": newKcal}, nil
}

// DeleteWorkout hard-deletes one workout by id. Unknown ids fail;
// dry_run reports first.
func (s *Store) DeleteWorkout(ctx context.Context, id string, dryRun bool) (bson.M, error) {
	oid, err := primitive.ObjectIDFromHex(strings.TrimSpace(id))
	if err != nil {
		return nil, fail("'%s' is not a workout id", id)
	}
	var doc bson.M
	if err := s.workouts.FindOne(ctx, bson.M{"_id": oid}).Decode(&doc); err != nil {
		if err == mongoDrv.ErrNoDocuments {
			return nil, fail("unknown workout '%s'", id)
		}
		return nil, err
	}
	if dryRun {
		return bson.M{"would_delete": 1, "workout": workoutOut(doc)}, nil
	}
	if _, err := s.workouts.DeleteOne(ctx, bson.M{"_id": oid}); err != nil {
		return nil, err
	}
	return bson.M{"deleted": 1}, nil
}

// SyncRecord is one health-api sync payload, already bucketed to dates.
type SyncRecord struct {
	Date       string
	Steps      *int
	ActiveKcal *float64
	Sleeps     []SleepIn
	Workouts   []WorkoutIn
}

// SyncResult reports what one sync wrote. Re-sent sessions/workouts are
// skipped quietly (already there), not errors — syncs are cumulative.
type SyncResult struct {
	Date            string `json:"date"`
	Steps           *int   `json:"steps,omitempty"`
	SessionsAdded   int    `json:"sessions_added"`
	SessionsSkipped int    `json:"sessions_skipped"`
	WorkoutsAdded   int    `json:"workouts_added"`
	WorkoutsSkipped int    `json:"workouts_skipped"`
}

// RecordSync is the single write path for hc_days, hc_sleep and
// hc_workouts, shared by the MCP tools and health-api/sync. Steps and
// active_kcal overwrite when provided; sessions and workouts append with
// atomic scalar-unique hash dedup. updatedAt is always a BSON Date — the
// old string-vs-Date drift is gone by construction.
//
// Each entry keeps its own bucketed day (a backfill's nights stay on
// their mornings); rec.Date covers only the scalars. Input is validated
// before anything is written, so a bad session fails the sync before the
// scalars land — but later write-time failures (duplicate hash, DB error)
// can still land partial rows after the scalars. Best-effort, not atomic:
// the sync is idempotent, so a retry converges instead of duplicating.
func (s *Store) RecordSync(ctx context.Context, rec SyncRecord) (SyncResult, error) {
	day, err := mustDay(rec.Date)
	if err != nil {
		return SyncResult{}, err
	}
	for i := range rec.Sleeps {
		if rec.Sleeps[i].Day == "" {
			rec.Sleeps[i].Day = day
		}
		if err := checkSleepIn(rec.Sleeps[i]); err != nil {
			return SyncResult{}, err
		}
	}
	for i := range rec.Workouts {
		if rec.Workouts[i].Day == "" {
			rec.Workouts[i].Day = day
		}
		if err := checkWorkoutIn(rec.Workouts[i]); err != nil {
			return SyncResult{}, err
		}
	}
	res := SyncResult{Date: day}
	set := bson.M{"updatedAt": primitive.NewDateTimeFromTime(time.Now().UTC())}
	if rec.Steps != nil {
		set["steps"] = *rec.Steps
		res.Steps = rec.Steps
	}
	if rec.ActiveKcal != nil {
		set["active_kcal"] = round1(*rec.ActiveKcal)
	}
	if _, err := s.days.UpdateOne(ctx, bson.M{"date": day}, bson.M{"$set": set},
		options.Update().SetUpsert(true)); err != nil {
		return SyncResult{}, err
	}
	for _, sl := range rec.Sleeps {
		out, err := s.insertSleepSession(ctx, sl.Day, sl.Start, sl.Minutes, true)
		if err != nil {
			return SyncResult{}, err
		}
		if sk, _ := out["skipped"].(bool); sk {
			res.SessionsSkipped++
		} else {
			res.SessionsAdded++
		}
	}
	for _, w := range rec.Workouts {
		out, err := insertWorkout(ctx, s.workouts, w, true)
		if err != nil {
			return SyncResult{}, err
		}
		if sk, _ := out["skipped"].(bool); sk {
			res.WorkoutsSkipped++
		} else {
			res.WorkoutsAdded++
		}
	}
	return res, nil
}

// checkSleepIn / checkWorkoutIn validate an entry without writing it, so
// RecordSync fails whole-sync on bad input.
func checkSleepIn(sl SleepIn) error {
	if _, err := mustDay(sl.Day); err != nil {
		return err
	}
	if sl.Minutes <= 0 {
		return fail("sleep session minutes must be > 0")
	}
	return nil
}

func checkWorkoutIn(w WorkoutIn) error {
	if _, err := mustDay(w.Day); err != nil {
		return err
	}
	if w.Minutes <= 0 {
		return fail("workout minutes must be > 0")
	}
	if w.Kcal < 0 {
		return fail("workout kcal must be >= 0")
	}
	return nil
}

// DailySummary is the cross-entity read: food totals derived from
// hc_food_log by day, sleep hours derived from sessions, cal-out split
// into workout_kcal + active_kcal lines. No single net — Health Connect
// active calories already include exercise burn, so a net would
// double-count every workout.
func (s *Store) DailySummary(ctx context.Context, day string) (bson.M, error) {
	day, err := mustDay(day)
	if err != nil {
		return nil, err
	}
	totals, foodCount, err := s.foodTotals(ctx, day)
	if err != nil {
		return nil, err
	}
	var w bson.M
	_ = s.weight.FindOne(ctx, bson.M{"date": day}).Decode(&w)
	var d bson.M
	_ = s.days.FindOne(ctx, bson.M{"date": day}).Decode(&d)

	sleeps, err := s.ListSleep(ctx, day, day, "", "")
	if err != nil {
		return nil, err
	}
	workouts, err := s.ListWorkouts(ctx, day, day, "", "", "")
	if err != nil {
		return nil, err
	}
	var sessions []bson.M
	var sleepHours float64
	if len(sleeps) > 0 {
		sessions, _ = sleeps[0]["sessions"].([]bson.M)
		sleepHours, _ = sleeps[0]["sleep_hours"].(float64)
	}
	var wos []bson.M
	if len(workouts) > 0 {
		wos, _ = workouts[0]["workouts"].([]bson.M)
	}
	if sessions == nil {
		sessions = []bson.M{}
	}
	if wos == nil {
		wos = []bson.M{}
	}
	var steps any
	var activeKcal float64
	if d != nil {
		steps = d["steps"]
		if v, ok := d["active_kcal"]; ok {
			if f, err := validate.ToFloat(v); err == nil {
				activeKcal = round1(f)
			}
		}
	}
	workoutKcal := workoutKcal(wos)
	out := bson.M{"date": day, "foods": foodCount, "totals": totals,
		"sleep_hours": sleepHours, "sessions": sessions, "workouts": wos,
		"steps": steps, "active_kcal": activeKcal,
		"cal_in": totals["kcal"], "workout_kcal": workoutKcal}
	if w != nil {
		out["weight"] = bson.M{"date": w["date"], "kg": w["kg"]}
	}
	return out, nil
}

func fval(v any) float64 {
	f, _ := validate.ToFloat(v)
	return f
}
