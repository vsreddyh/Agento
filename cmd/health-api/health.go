package main

// Health sync + status: payload types, the /health probe and the sync endpoint (#171).

import (
	"agento/internal/crashlog"
	"agento/internal/healthcheck"
	"context"
	"encoding/json"
	"errors"
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

// parseSyncTime parses a Health Connect ISO timestamp: fractional
// seconds accepted, naive read as UTC — one rule, shared with the MCP
// (healthcheck.ParseSyncTime), so a retried payload hashes identically
// whether zoned or naive.
func parseSyncTime(s string) (time.Time, bool) {
	return healthcheck.ParseSyncTime(s)
}

// localDate buckets an ISO timestamp into the user's (IST) calendar date.
// Unparseable input is invalid (""), never a guessed prefix: a corrupt
// timestamp must fail or count invalid downstream, not land on a date.
func localDate(iso string) string {
	s := strings.TrimSpace(iso)
	if t, ok := parseSyncTime(s); ok {
		return t.In(healthcheck.IST()).Format("2006-01-02")
	}
	return ""
}

// minutesBetween returns whole-minute duration; ok=false when unparseable
// or non-positive (an end before/at the start is corrupt data, not a
// 1-minute workout — skipped like unparseable timestamps).
func minutesBetween(startISO, endISO string) (dur int, ok bool) {
	s, ok1 := parseSyncTime(startISO)
	e, ok2 := parseSyncTime(endISO)
	if !ok1 || !ok2 {
		return 0, false
	}
	m := int(e.Sub(s).Minutes())
	if m < 1 {
		return 0, false
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

	tsInvalidSleeps, tsInvalidWorkouts := 0, 0
	for _, s := range p.Sleep {
		// Sessions bucket by wake date but carry their own start, so a
		// night crossing midnight still attributes to the morning —
		// while the dedup hash keys on the real start. Sleep uses the
		// device-reported minutes (awake time excluded), NOT the
		// start/end interval — workouts instead use the interval
		// duration. An unparseable end timestamp skips-and-counts
		// (invalid, not a session on the sync date); an unparseable
		// start still syncs (minutes only), like a manual log_sleep.
		// Naive timestamps parse as UTC.
		if _, ok := parseSyncTime(s.EndISO); !ok {
			log.Printf("skipping sleep with unparseable end timestamp: %q (minutes=%d)", s.EndISO, s.TotalMinutes)
			tsInvalidSleeps++
			continue
		}
		day := localDate(s.EndISO)
		start, ok := parseSyncTime(s.StartISO)
		var startPtr *time.Time
		if ok {
			startPtr = &start
		}
		rec.Sleeps = append(rec.Sleeps, healthcheck.SleepIn{
			Day:     day,
			Start:   startPtr,
			Minutes: s.TotalMinutes,
		})
	}

	for _, wo := range p.Workouts {
		duration, ok := minutesBetween(wo.StartISO, wo.EndISO)
		if !ok {
			log.Printf("skipping workout with unparseable timestamps: %s -> %s", wo.StartISO, wo.EndISO)
			tsInvalidWorkouts++
			continue
		}
		var kcal *float64
		if wo.CaloriesKcal != nil {
			kcal = wo.CaloriesKcal
		}
		// Title is the human name when Type is empty (Health Connect
		// often sends one without the other); the store lowercases it
		// like any other type.
		typ := wo.Type
		if strings.TrimSpace(typ) == "" {
			typ = wo.Title
		}
		rec.Workouts = append(rec.Workouts, healthcheck.WorkoutIn{
			Day:     localDate(wo.StartISO),
			Type:    typ,
			Minutes: float64(duration),
			Kcal:    kcal,
		})
	}

	syncRes, err := hcs.RecordSync(ctx, rec)
	syncRes.SessionsInvalid += tsInvalidSleeps
	syncRes.WorkoutsInvalid += tsInvalidWorkouts
	if err != nil {
		// Domain errors (bad date, implausible minutes/kcal) are the
		// client's malformed payload: 422 like the batch-size guards
		// above, not a 500 server bug. Anything else is ours.
		var se *healthcheck.StoreError
		if errors.As(err, &se) {
			writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": se.Error()})
		} else {
			writeJSON(w, http.StatusInternalServerError, bson.M{"detail": boundedDetail(err)})
		}
		return
	}

	steps, kcal := 0, 0.0
	if p.Steps != nil {
		steps = *p.Steps
	}
	if p.ActiveCaloriesKcal != nil {
		kcal = *p.ActiveCaloriesKcal
	}
	log.Printf("synced device=%s steps=%d calories=%v sleep=%d workouts=%d (+%d sessions +%d workouts, %d+%d skipped, %d+%d invalid)",
		p.Device, steps, kcal, len(p.Sleep), len(p.Workouts),
		syncRes.SessionsAdded, syncRes.WorkoutsAdded, syncRes.SessionsSkipped, syncRes.WorkoutsSkipped,
		syncRes.SessionsInvalid, syncRes.WorkoutsInvalid)
	resp := bson.M{"status": "ok", "synced_at": now.Format(time.RFC3339),
		"sessions_added": syncRes.SessionsAdded, "sessions_skipped": syncRes.SessionsSkipped,
		"sessions_invalid": syncRes.SessionsInvalid, "workouts_added": syncRes.WorkoutsAdded,
		"workouts_skipped": syncRes.WorkoutsSkipped, "workouts_invalid": syncRes.WorkoutsInvalid}
	// The batch date covers only the scalars; entries-only syncs (or a
	// bad batch date) write no scalars, so no date is echoed.
	if syncRes.Date != "" {
		resp["date"] = syncRes.Date
	}
	writeJSON(w, http.StatusOK, resp)
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
