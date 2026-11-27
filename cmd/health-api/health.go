package main

// Health sync + status: payload types, the /health probe and the sync endpoint (#171).

import (
	"agento/internal/crashlog"
	"agento/internal/healthcheck"
	"context"
	"encoding/json"
	"go.mongodb.org/mongo-driver/bson"
	"log"
	"net/http"
	"strings"
	stdsync "sync"
	"time"
)

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

func health(w http.ResponseWriter, r *http.Request) {
	// Liveness answers GET and HEAD only, like the gateway's handleHealth:
	// a 200 to POST would misstate the contract when compared across
	// services, and the limiter exempt below stays narrow to the static
	// reply. HEAD carries headers only; net/http suppresses the body.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "GET or HEAD only"})
		return
	}
	writeJSON(w, http.StatusOK, bson.M{"status": "ok"})
}

// exportsRoot is the VPS folder exposed for mobile downloads (#26).
// Override with EXPORTS_DIR; served read-only, subfolders allowed.

// hcStore is the shared healthcheck backend. Both the MCP server and this
// sync endpoint write through it, so hc_days/hc_sleep/hc_workouts have one
// write path and one updatedAt shape (BSON Date, never a string).
var (
	hcStore     *healthcheck.Store
	hcStoreOnce stdsync.Once
	hcStoreErr  error
)

func healthStore() (*healthcheck.Store, error) {
	hcStoreOnce.Do(func() {
		hcStore, hcStoreErr = healthcheck.FromEnv()
	})
	return hcStore, hcStoreErr
}

func sync(w http.ResponseWriter, r *http.Request) {
	// Auth first, like every other authed verb in this binary (list,
	// item, fallback): an unauthenticated mistype 401s instead of
	// leaking the method contract — the Allow below is for authed
	// triage, matching the neighbouring handlers.
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "/api/health/sync accepts POST"})
		return
	}
	var p HealthSyncPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 5<<20)).Decode(&p); err != nil {
		// Scrubbed + bounded: the decode error echoes client body bytes
		// verbatim, and the app renders `detail` as-is.
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "invalid JSON: " + boundedDetail(err)})
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
	hcs, err := healthStore()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, bson.M{"detail": "health store unavailable"})
		return
	}
	now := time.Now().UTC()

	// Steps and active calories overwrite the day's scalars when present;
	// a payload without them (sleep/workout-only backfill) leaves the
	// scalars alone instead of zeroing the day.
	rec := healthcheck.SyncRecord{Date: localDate(p.SyncedAtISO)}
	if p.Steps != nil {
		steps := *p.Steps
		rec.Steps = &steps
	}
	if p.ActiveCaloriesKcal != nil {
		kcal := *p.ActiveCaloriesKcal
		rec.ActiveKcal = &kcal
	}

	for _, s := range p.Sleep {
		// Sessions bucket by wake date but carry their own start, so a
		// night crossing midnight still attributes to the morning —
		// while the dedup hash keys on the real start. An unparseable
		// start still syncs (minutes only), like a manual log_sleep.
		start, perr := time.Parse(time.RFC3339, strings.Replace(s.StartISO, "Z", "+00:00", 1))
		var startPtr *time.Time
		if perr == nil {
			startPtr = &start
		}
		rec.Sleeps = append(rec.Sleeps, healthcheck.SleepIn{
			Day:     localDate(s.EndISO),
			Start:   startPtr,
			Minutes: s.TotalMinutes,
		})
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
		rec.Workouts = append(rec.Workouts, healthcheck.WorkoutIn{
			Day:     localDate(wo.StartISO),
			Type:    wo.Type,
			Minutes: float64(duration),
			Kcal:    kcal,
		})
	}

	syncRes, err := hcs.RecordSync(ctx, rec)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, bson.M{"detail": boundedDetail(err)})
		return
	}

	steps, kcal := 0, 0.0
	if p.Steps != nil {
		steps = *p.Steps
	}
	if p.ActiveCaloriesKcal != nil {
		kcal = *p.ActiveCaloriesKcal
	}
	log.Printf("synced device=%s steps=%d calories=%v sleep=%d workouts=%d (+%d sessions +%d workouts, %d+%d skipped)",
		p.Device, steps, kcal, len(p.Sleep), len(p.Workouts),
		syncRes.SessionsAdded, syncRes.WorkoutsAdded, syncRes.SessionsSkipped, syncRes.WorkoutsSkipped)
	writeJSON(w, http.StatusOK, bson.M{"status": "ok", "synced_at": now.Format(time.RFC3339)})
}

// registerHealth wires this domain's routes. One function per file so main stays a
// list of domains, not a list of paths (#171).
func registerHealth(mux *http.ServeMux) {
	// Both spellings: the probe is hand-curled with and without the
	// slash, and an exact-only registration answers the other with Go's
	// text/plain 404 (no detail envelope). The handler ignores the
	// request, so the alias serves identically.
	mux.HandleFunc("/health", health)
	mux.HandleFunc("/health/", health)
	mux.HandleFunc("/api/health/sync", sync)
}

// boundedDetail scrubs a driver/decode error and caps it for API bodies:
// rune-cut with a visible marker, never silent.
func boundedDetail(err error) string {
	const maxEcho = 128
	s := crashlog.ScrubError(err.Error())
	runes := []rune(s)
	if len(runes) <= maxEcho {
		return s
	}
	return string(runes[:maxEcho]) + "… (truncated)"
}
