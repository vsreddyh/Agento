package main

// Report routes: the ∃-Mail inbox surface over the shared reports collection
// (#365). Agents file via the reports MCP server; the app reads here.

import (
	"agento/internal/reports"
	"context"
	"encoding/json"
	"errors"
	"go.mongodb.org/mongo-driver/bson"
	"log"
	"net/http"
	"strconv"
	"strings"
	syncpkg "sync"
	"time"
)

// listReports serves the app's ∃-Mail screen: newest-first summaries (no
// bodies — the 7-day retention keeps this small, but a list is still not the
// place for 50 KB bodies with pre-rendered math). `?sort=` accepts only
// `newest` (the only order v1 has); `?limit=` caps rows (default 50, max 200).
func listReports(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "method not allowed"})
		return
	}
	if sort := strings.TrimSpace(r.URL.Query().Get("sort")); sort != "" && sort != "newest" {
		writeReportErr(w, &reports.StoreError{Msg: "unknown sort '" + sort + "' (only newest)"})
		return
	}
	store, ok := reportStore(w)
	if !ok {
		return
	}
	limit := 0
	// Non-numeric ?limit= falls back to the store default (50) on purpose: a
	// stray query param should never nuke the inbox list.
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	rows, err := store.ListReports(ctx, limit)
	if err != nil {
		writeReportErr(w, err)
		return
	}
	if rows == nil {
		rows = []bson.M{}
	}
	writeJSON(w, http.StatusOK, bson.M{"reports": rows})
}

// reportStore opens the shared reports collection (the same rows the agent
// files over the reports MCP). Cached process-wide like the other stores;
// failures are not cached, so a bad env recovers without a restart.
var (
	reportsStoreMu syncpkg.Mutex
	reportsCache   *reports.Store
)

func reportStore(w http.ResponseWriter) (*reports.Store, bool) {
	reportsStoreMu.Lock()
	defer reportsStoreMu.Unlock()
	if reportsCache != nil {
		return reportsCache, true
	}
	store, err := reports.FromEnv()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, bson.M{"detail": "MONGODB_URI not set"})
		return nil, false
	}
	reportsCache = store
	return store, true
}

// writeReportErr maps store domain errors: unknown ids 404, validation 422.
// Anything else is logged with full detail but returns a generic message.
func writeReportErr(w http.ResponseWriter, err error) {
	var se *reports.StoreError
	if errors.As(err, &se) {
		if strings.HasPrefix(se.Msg, "unknown report") {
			writeJSON(w, http.StatusNotFound, bson.M{"detail": se.Msg})
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": se.Msg})
		return
	}
	log.Printf("reports: internal error: %v", err)
	writeJSON(w, http.StatusInternalServerError, bson.M{"detail": "internal error"})
}

// decodeReportBody reads a small JSON object body into a field map.
func decodeReportBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
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

// checkReportFields rejects mistyped values loudly so client bugs come back
// as 422 instead of mystery no-ops, and refuses unknown keys: v1 patches only
// read/starred, so a misspelled key is a lost update, not an ignored one.
// JSON null counts as absent.
func checkReportFields(fields map[string]any) error {
	for k, v := range fields {
		if v == nil {
			continue
		}
		if k != "read" && k != "starred" {
			return &reports.StoreError{Msg: "unknown field '" + k + "' (only read and starred are patchable)"}
		}
		if _, ok := v.(bool); !ok {
			return &reports.StoreError{Msg: k + " must be a boolean"}
		}
	}
	return nil
}

// reportItem dispatches /api/reports/{id}: get + patch + delete on the id.
// Auth first; unknown shapes 404.
func reportItem(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/reports/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		// Trailing slash with no id behaves like the collection root.
		reportsRoot(w, r)
		return
	}
	if len(parts) > 1 {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "not found"})
		return
	}
	id := parts[0]
	switch r.Method {
	case http.MethodGet:
		getReport(w, r, id)
	case http.MethodPatch:
		updateReport(w, r, id)
	case http.MethodDelete:
		deleteReport(w, r, id)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "method not allowed"})
	}
}

// getReport returns the full report plus server-rendered math segments: `text`
// segments carry markdown for the client's renderer, `math` segments carry
// pre-rendered HTML the app displays verbatim (LaTeX compiles on the server,
// #365). math_engine names the renderer whose static CSS the HTML expects.
func getReport(w http.ResponseWriter, r *http.Request, id string) {
	store, ok := reportStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	doc, err := store.GetReport(ctx, id)
	if err != nil {
		writeReportErr(w, err)
		return
	}
	if doc == nil {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "unknown report '" + reports.ShortID(id) + "'"})
		return
	}
	body, _ := doc["body_md"].(string)
	doc["segments"] = reports.Segments(body)
	doc["math_engine"] = reports.MathEngine
	writeJSON(w, http.StatusOK, doc)
}

// updateReport applies the reader's verbs: opening marks read, the star
// toggles. Only read/starred change; content is immutable after filing.
func updateReport(w http.ResponseWriter, r *http.Request, id string) {
	fields, ok := decodeReportBody(w, r)
	if !ok {
		return
	}
	if err := checkReportFields(fields); err != nil {
		writeReportErr(w, err)
		return
	}
	// JSON null counts as absent (checkReportFields lets it through), but the
	// store type-asserts bools — strip nulls so explicit nulls read as
	// "untouched" instead of 422ing after passing the guard.
	for k, v := range fields {
		if v == nil {
			delete(fields, k)
		}
	}
	store, ok := reportStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	doc, err := store.UpdateReport(ctx, id, fields)
	if err != nil {
		writeReportErr(w, err)
		return
	}
	if doc == nil {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "unknown report '" + reports.ShortID(id) + "'"})
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// deleteReport hard-deletes one report (confirm on the client — there is no
// undo, no trash, no archive in v1).
func deleteReport(w http.ResponseWriter, r *http.Request, id string) {
	store, ok := reportStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	found, err := store.DeleteReport(ctx, id)
	if err != nil {
		writeReportErr(w, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "unknown report '" + reports.ShortID(id) + "'"})
		return
	}
	writeJSON(w, http.StatusOK, bson.M{"deleted": true})
}

// reportsRoot serves the collection endpoint: GET lists. There is deliberately
// no POST — agents file over the reports MCP server, and one writer path
// means one validation path.
func reportsRoot(w http.ResponseWriter, r *http.Request) {
	listReports(w, r)
}

// registerReports wires this domain's routes. One function per file so main
// stays a list of domains, not a list of paths.
func registerReports(mux *http.ServeMux) {
	mux.HandleFunc("/api/reports", reportsRoot)
	mux.HandleFunc("/api/reports/", reportItem)
}
