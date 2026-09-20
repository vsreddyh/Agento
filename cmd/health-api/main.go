// Command health-api is the Health Connect sync endpoint for the health-check bot.
//
// Accepts POSTs from the Agento Android app and persists to the shared
// remote MongoDB collection the health-check bot also reads (hc_days —
// one doc per date, same shape the MCP writes).
//
// Auth: single PASSWORD bearer token (matches the Password field in the
// Agento app Settings; same credential as gateway chat).
//
// Env: MONGODB_URI (required), MONGODB_DB (default: hermes), PASSWORD.
package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"agento/internal/mongo"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// IST is the user timezone (UTC+5:30, no DST) as a fixed offset.
var IST = time.FixedZone("IST", 5*3600+30*60)

type SleepEntry struct {
	StartISO     string         `json:"startIso"`
	EndISO       string         `json:"endIso"`
	TotalMinutes int            `json:"totalMinutes"`
	Stages       map[string]int `json:"stages"`
}

type WorkoutEntry struct {
	StartISO       string   `json:"startIso"`
	EndISO         string   `json:"endIso"`
	Title          string   `json:"title"`
	Type           string   `json:"type"`
	DistanceMeters *float64 `json:"distanceMeters"`
	CaloriesKcal   *float64 `json:"caloriesKcal"`
}

type HealthSyncPayload struct {
	Device             string         `json:"device"`
	SyncedAtISO        string         `json:"syncedAtIso"`
	Steps              *int           `json:"steps"`
	ActiveCaloriesKcal *float64       `json:"activeCaloriesKcal"`
	Sleep              []SleepEntry   `json:"sleep"`
	Workouts           []WorkoutEntry `json:"workouts"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// tokens reads accepted tokens fresh (env may rotate without a restart).
func tokens() map[string]bool {
	out := map[string]bool{}
	for _, t := range strings.Split(os.Getenv("PASSWORD"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			out[t] = true
		}
	}
	return out
}

func authorize(r *http.Request) (int, string) {
	toks := tokens()
	if len(toks) == 0 {
		log.Print("PASSWORD not set — rejecting all requests")
		return http.StatusServiceUnavailable, "server not configured with tokens"
	}
	h := r.Header.Get("Authorization")
	if h == "" || !strings.HasPrefix(h, "Bearer ") {
		return http.StatusUnauthorized, "missing bearer token"
	}
	if !toks[strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))] {
		return http.StatusUnauthorized, "invalid token"
	}
	return 0, ""
}

// localDate buckets an ISO timestamp into the user's (IST) calendar date.
func localDate(iso string) string {
	s := strings.TrimSpace(iso)
	if t, err := time.Parse(time.RFC3339, strings.Replace(s, "Z", "+00:00", 1)); err == nil {
		return t.In(IST).Format("2006-01-02")
	}
	// Naive timestamps are UTC.
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.UTC); err == nil {
		return t.In(IST).Format("2006-01-02")
	}
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// minutesBetween returns whole-minute duration; ok=false when unparseable.
func minutesBetween(startISO, endISO string) (dur int, ok bool) {
	p := func(s string) (time.Time, bool) {
		s = strings.TrimSpace(s)
		if t, err := time.Parse(time.RFC3339, strings.Replace(s, "Z", "+00:00", 1)); err == nil {
			return t, true
		}
		return time.Time{}, false
	}
	s, ok1 := p(startISO)
	e, ok2 := p(endISO)
	if !ok1 || !ok2 {
		return 0, false
	}
	m := int(e.Sub(s).Minutes())
	if m < 1 {
		m = 1
	}
	return m, true
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, bson.M{"status": "ok"})
}

func sync(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "method not allowed"})
		return
	}
	var p HealthSyncPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 5<<20)).Decode(&p); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "invalid JSON: " + err.Error()})
		return
	}
	if len(p.Sleep) > 100 {
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "sleep batch max 100"})
		return
	}
	if len(p.Workouts) > 100 {
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "workouts batch max 100"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	d, err := mongo.DB()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, bson.M{"detail": "MONGODB_URI not set"})
		return
	}
	days := d.Collection("hc_days")
	now := time.Now().UTC().Format(time.RFC3339)
	statsDate := localDate(p.SyncedAtISO)

	update := bson.M{"updatedAt": now}
	if p.Steps != nil {
		update["steps"] = *p.Steps
	}
	if p.ActiveCaloriesKcal != nil {
		update["active_kcal"] = *p.ActiveCaloriesKcal
	}
	if len(update) > 1 {
		if _, err := days.UpdateOne(ctx, bson.M{"date": statsDate}, bson.M{"$set": update}, upsert()); err != nil {
			writeJSON(w, http.StatusInternalServerError, bson.M{"detail": err.Error()})
			return
		}
	}

	for _, s := range p.Sleep {
		wakeDate := localDate(s.EndISO)
		var dup bson.M
		if err := days.FindOne(ctx, bson.M{"date": wakeDate, "sleep_sessions.start": s.StartISO}).Decode(&dup); err == nil {
			continue
		}
		hours := float64(s.TotalMinutes) / 60.0
		hours = float64(int(hours*100+0.5)) / 100
		if _, err := days.UpdateOne(ctx, bson.M{"date": wakeDate}, bson.M{
			"$inc": bson.M{"sleep_hours": hours},
			"$push": bson.M{"sleep_sessions": bson.M{"start": s.StartISO, "minutes": s.TotalMinutes}},
			"$set":  bson.M{"updatedAt": now},
		}, upsert()); err != nil {
			writeJSON(w, http.StatusInternalServerError, bson.M{"detail": err.Error()})
			return
		}
	}

	for _, wo := range p.Workouts {
		duration, ok := minutesBetween(wo.StartISO, wo.EndISO)
		if !ok {
			log.Printf("skipping workout with unparseable timestamps: %s -> %s", wo.StartISO, wo.EndISO)
			continue
		}
		var kcal float64
		if wo.CaloriesKcal != nil {
			kcal = *wo.CaloriesKcal
		}
		kcal = float64(int(kcal*10+0.5)) / 10
		wdoc := bson.M{"type": wo.Type, "minutes": duration, "kcal": kcal}
		var dup bson.M
		if err := days.FindOne(ctx, bson.M{
			"date":     localDate(wo.StartISO),
			"workouts": bson.M{"$elemMatch": wdoc},
		}).Decode(&dup); err == nil {
			continue
		}
		if _, err := days.UpdateOne(ctx, bson.M{"date": localDate(wo.StartISO)}, bson.M{
			"$push": bson.M{"workouts": wdoc},
			"$set":  bson.M{"updatedAt": now},
		}, upsert()); err != nil {
			writeJSON(w, http.StatusInternalServerError, bson.M{"detail": err.Error()})
			return
		}
	}

	steps, kcal := 0, 0.0
	if p.Steps != nil {
		steps = *p.Steps
	}
	if p.ActiveCaloriesKcal != nil {
		kcal = *p.ActiveCaloriesKcal
	}
	log.Printf("synced device=%s steps=%d calories=%v sleep=%d workouts=%d",
		p.Device, steps, kcal, len(p.Sleep), len(p.Workouts))
	writeJSON(w, http.StatusOK, bson.M{"status": "ok", "synced_at": now})
}

func upsert() *options.UpdateOptions {
	return options.Update().SetUpsert(true)
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", health)
	mux.HandleFunc("/api/health/sync", sync)
	log.Print("health-api listening on :8000")
	if err := http.ListenAndServe(":8000", mux); err != nil {
		log.Fatal(err)
	}
}
