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
	"errors"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"agento/internal/mongo"
	"agento/internal/tasks"

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

// exportsRoot is the VPS folder exposed for mobile downloads (#26).
// Override with EXPORTS_DIR; served read-only, subfolders allowed.
func exportsRoot() string {
	if v := strings.TrimSpace(os.Getenv("EXPORTS_DIR")); v != "" {
		return v
	}
	return "/exports"
}

// resolveExport cleans a user-supplied relative path and confines it inside
// the exports root (rejects absolute paths and .. escapes).
func resolveExport(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return exportsRoot(), nil
	}
	// Reject escapes before cleaning (Clean would normalize ".." away).
	if path.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") ||
		strings.Contains(rel, "/../") || strings.HasSuffix(rel, "/..") {
		return "", errBadPath
	}
	full := filepath.Join(exportsRoot(), filepath.FromSlash(path.Clean("/"+rel)))
	root, err := filepath.EvalSymlinks(exportsRoot())
	if err != nil {
		return "", err
	}
	// Allow missing leaf (parent must still resolve inside root).
	target := full
	if _, err := os.Lstat(target); err != nil {
		target = filepath.Dir(target)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		return "", errBadPath
	}
	return full, nil
}

var errBadPath = errText("invalid path")

type errText string

func (e errText) Error() string { return string(e) }

func listFiles(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	full, err := resolveExport(r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, bson.M{"detail": "invalid path"})
		return
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "not found"})
		return
	}
	rel, _ := filepath.Rel(exportsRoot(), full)
	dirs := []bson.M{}
	files := []bson.M{}
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, bson.M{"name": e.Name()})
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, bson.M{
			"name":  e.Name(),
			"size":  info.Size(),
			"mtime": info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	if dirs == nil {
		dirs = []bson.M{}
	}
	if files == nil {
		files = []bson.M{}
	}
	writeJSON(w, http.StatusOK, bson.M{"path": filepath.ToSlash(rel), "dirs": dirs, "files": files})
}

func downloadFile(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	full, err := resolveExport(r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, bson.M{"detail": "invalid path"})
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "not found"})
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\""+info.Name()+"\"")
	http.ServeFile(w, r, full)
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
			"$inc":  bson.M{"sleep_hours": hours},
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

// listTasks serves the app's Task Manager screen: the user's own tasks from
// the shared `tasks` collection (the same rows the agent manages over MCP).
// `?state=open|done|all` (default open).
func listTasks(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "method not allowed"})
		return
	}
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if state == "" {
		state = "open"
	}
	store, ok := taskStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rows, err := store.List(ctx, state, false, "")
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, bson.M{"tasks": rows})
}

// taskStore opens the shared tasks collection (same rows the agent manages).
func taskStore(w http.ResponseWriter) (*tasks.Store, bool) {
	store, err := tasks.FromEnv()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, bson.M{"detail": "MONGODB_URI not set"})
		return nil, false
	}
	return store, true
}

// writeTaskErr maps store domain errors: unknown ids 404, validation 422.
func writeTaskErr(w http.ResponseWriter, err error) {
	var se *tasks.StoreError
	if errors.As(err, &se) {
		if strings.HasPrefix(se.Msg, "unknown task") {
			writeJSON(w, http.StatusNotFound, bson.M{"detail": se.Msg})
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": se.Msg})
		return
	}
	writeJSON(w, http.StatusInternalServerError, bson.M{"detail": err.Error()})
}

// decodeTaskBody reads a small JSON object body into a field map.
func decodeTaskBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var fields map[string]any
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&fields); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "invalid JSON: " + err.Error()})
		return nil, false
	}
	if fields == nil {
		fields = map[string]any{}
	}
	return fields, true
}

func taskStrField(fields map[string]any, key string) string {
	if v, ok := fields[key].(string); ok {
		return v
	}
	return ""
}

func taskIntField(fields map[string]any, key string) (int, bool) {
	switch n := fields[key].(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return int(i), true
		}
	}
	return 0, false
}

// createTask inserts one open task. Name required; due/estimate/repeat
// validated by the store (same rules as the agent's create_task).
func createTask(w http.ResponseWriter, r *http.Request) {
	fields, ok := decodeTaskBody(w, r)
	if !ok {
		return
	}
	store, ok := taskStore(w)
	if !ok {
		return
	}
	minutes := 0
	if _, present := fields["estimated_minutes"]; present {
		n, ok := taskIntField(fields, "estimated_minutes")
		if !ok {
			writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "estimated_minutes must be a number >= 0"})
			return
		}
		minutes = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	doc, err := store.Create(ctx,
		taskStrField(fields, "name"),
		taskStrField(fields, "description"),
		taskStrField(fields, "due_date"),
		taskStrField(fields, "due_time"),
		minutes,
		taskStrField(fields, "repeat_rule"),
	)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, doc)
}

// taskItem dispatches /api/tasks/{id}[/{action}]: get + patch + delete on
// the id, post on complete/reopen. Auth first; unknown shapes 404.
func taskItem(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/tasks/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "not found"})
		return
	}
	id, action := parts[0], ""
	if len(parts) > 2 {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "not found"})
		return
	}
	if len(parts) == 2 {
		action = parts[1]
	}
	switch {
	case action == "" && r.Method == http.MethodGet:
		getTask(w, r, id)
	case action == "" && r.Method == http.MethodPatch:
		updateTask(w, r, id)
	case action == "" && r.Method == http.MethodDelete:
		deleteTask(w, r, id)
	case action == "complete" && r.Method == http.MethodPost:
		completeTask(w, r, id)
	case action == "reopen" && r.Method == http.MethodPost:
		reopenTask(w, r, id)
	default:
		if action != "" && action != "complete" && action != "reopen" {
			writeJSON(w, http.StatusNotFound, bson.M{"detail": "not found"})
			return
		}
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "method not allowed"})
	}
}

func getTask(w http.ResponseWriter, r *http.Request, id string) {
	store, ok := taskStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	doc, err := store.Get(ctx, id)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// updateTask applies a partial edit (only sent keys change; empty
// repeat_rule clears the rule — same semantics as the agent's update_task).
func updateTask(w http.ResponseWriter, r *http.Request, id string) {
	fields, ok := decodeTaskBody(w, r)
	if !ok {
		return
	}
	store, ok := taskStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	doc, err := store.Update(ctx, id, fields)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func deleteTask(w http.ResponseWriter, r *http.Request, id string) {
	store, ok := taskStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	found, err := store.Delete(ctx, id)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "unknown task '" + id + "'"})
		return
	}
	writeJSON(w, http.StatusOK, bson.M{"deleted": true})
}

// completeTask marks a task done (3-day retention starts server-side).
func completeTask(w http.ResponseWriter, r *http.Request, id string) {
	store, ok := taskStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	doc, err := store.Complete(ctx, id)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// reopenTask clears completion, making a done task open again.
func reopenTask(w http.ResponseWriter, r *http.Request, id string) {
	store, ok := taskStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	doc, err := store.Reopen(ctx, id)
	if err != nil {
		writeTaskErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// tasksRoot serves the collection endpoint: GET lists, POST creates.
func tasksRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if code, detail := authorize(r); code != 0 {
			writeJSON(w, code, bson.M{"detail": detail})
			return
		}
		createTask(w, r)
		return
	}
	listTasks(w, r)
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", health)
	mux.HandleFunc("/api/health/sync", sync)
	mux.HandleFunc("/api/files", listFiles)
	mux.HandleFunc("/api/files/download", downloadFile)
	mux.HandleFunc("/api/tasks", tasksRoot)
	mux.HandleFunc("/api/tasks/", taskItem)
	log.Print("health-api listening on :8000")
	if err := http.ListenAndServe(":8000", mux); err != nil {
		log.Fatal(err)
	}
}
