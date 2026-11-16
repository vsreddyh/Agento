package main

// Project routes: the Projects surface over the shared projects collection (#171).

import (
	"agento/internal/projects"
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

// listProjects serves the app's Projects screen: the user's projects from
// the shared `projects` collection (the same rows the agent manages over
// the project-manager MCP). `?status=Todo|Ongoing|Testing|Paused|Done|all`
// (default all), `?search=` matches name/note, `?limit=` caps rows
// (default 200, max 500).
func listProjects(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "method not allowed"})
		return
	}
	store, ok := projectStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	limit := 0
	// Non-numeric ?limit= falls back to the store default (200) on
	// purpose: a stray query param should never nuke the board list.
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	rows, err := store.List(ctx,
		r.URL.Query().Get("status"),
		r.URL.Query().Get("search"),
		limit,
	)
	if err != nil {
		writeProjectErr(w, err)
		return
	}
	if rows == nil {
		rows = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, bson.M{"projects": rows})
}

// projectStore opens the shared projects collection (same rows the agent
// manages). Cached process-wide like the task store; failures are not
// cached, so a bad env recovers without a restart.
var (
	projectsStoreMu syncpkg.Mutex
	projectsCache   *projects.Store
)

func projectStore(w http.ResponseWriter) (*projects.Store, bool) {
	projectsStoreMu.Lock()
	defer projectsStoreMu.Unlock()
	if projectsCache != nil {
		return projectsCache, true
	}
	store, err := projects.FromEnv()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, bson.M{"detail": "MONGODB_URI not set"})
		return nil, false
	}
	projectsCache = store
	return store, true
}

// writeProjectErr maps store domain errors: unknown ids 404, validation
// 422. Anything else is logged with full detail but returns a generic
// message (the endpoint is auth-gated, still no reason to leak DB
// internals).
func writeProjectErr(w http.ResponseWriter, err error) {
	var se *projects.StoreError
	if errors.As(err, &se) {
		if strings.HasPrefix(se.Msg, "unknown project") {
			writeJSON(w, http.StatusNotFound, bson.M{"detail": se.Msg})
			return
		}
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": se.Msg})
		return
	}
	log.Printf("projects: internal error: %v", err)
	writeJSON(w, http.StatusInternalServerError, bson.M{"detail": "internal error"})
}

// decodeProjectBody reads a small JSON object body into a field map.
func decodeProjectBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
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

// checkProjectFields rejects mistyped values loudly so client bugs come
// back as 422 instead of mystery no-ops. JSON null counts as absent.
func checkProjectFields(fields map[string]any) error {
	for _, k := range []string{"name", "status", "note"} {
		if v, ok := fields[k]; ok && v != nil {
			if _, ok := v.(string); !ok {
				return &projects.StoreError{Msg: k + " must be a string"}
			}
		}
	}
	return nil
}

// createProject inserts one project. Name required; blank status defaults
// to Todo (same rules as the agent's create_project).
func createProject(w http.ResponseWriter, r *http.Request) {
	fields, ok := decodeProjectBody(w, r)
	if !ok {
		return
	}
	if err := checkProjectFields(fields); err != nil {
		writeProjectErr(w, err)
		return
	}
	store, ok := projectStore(w)
	if !ok {
		return
	}
	strField := func(key string) string {
		if v, ok := fields[key].(string); ok {
			return v
		}
		return ""
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	doc, err := store.Create(ctx, strField("name"), strField("status"), strField("note"))
	if err != nil {
		writeProjectErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, doc)
}

// projectItem dispatches /api/projects/{id}: get + patch + delete on the
// id. Auth first; unknown shapes 404.
func projectItem(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/projects/"), "/")
	parts := strings.Split(rest, "/")
	if len(parts) == 0 || parts[0] == "" {
		// Trailing slash with no id behaves like the collection root.
		projectsRoot(w, r)
		return
	}
	if len(parts) > 1 {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "not found"})
		return
	}
	// /api/projects/reorder/ and /api/projects/move/ normalize to the ids
	// "reorder" and "move"; they are the collection verbs, not projects.
	if parts[0] == "reorder" {
		reorderProjects(w, r)
		return
	}
	if parts[0] == "move" {
		moveProjects(w, r)
		return
	}
	id := parts[0]
	switch r.Method {
	case http.MethodGet:
		getProject(w, r, id)
	case http.MethodPatch:
		updateProject(w, r, id)
	case http.MethodDelete:
		deleteProject(w, r, id)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "method not allowed"})
	}
}

func getProject(w http.ResponseWriter, r *http.Request, id string) {
	store, ok := projectStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	doc, err := store.Get(ctx, id)
	if err != nil {
		writeProjectErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// updateProject applies a partial edit (only sent keys change — same
// semantics as the agent's update_project).
func updateProject(w http.ResponseWriter, r *http.Request, id string) {
	fields, ok := decodeProjectBody(w, r)
	if !ok {
		return
	}
	if err := checkProjectFields(fields); err != nil {
		writeProjectErr(w, err)
		return
	}
	// JSON null counts as absent (checkProjectFields lets it through), but
	// the store type-asserts strings — strip nils so explicit nulls read
	// as "untouched" instead of 422ing after passing the guard.
	for k, v := range fields {
		if v == nil {
			delete(fields, k)
		}
	}
	store, ok := projectStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	doc, err := store.Update(ctx, id, fields)
	if err != nil {
		writeProjectErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func deleteProject(w http.ResponseWriter, r *http.Request, id string) {
	store, ok := projectStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	found, err := store.Delete(ctx, id)
	if err != nil {
		writeProjectErr(w, err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "unknown project '" + id + "'"})
		return
	}
	writeJSON(w, http.StatusOK, bson.M{"deleted": true})
}

// reorderProjects rewrites the manual order of one status section (#519):
// body {"status":"Todo","ids":["<id>",…]}. The store rejects duplicates,
// unknown ids and ids whose status changed under the request.
func reorderProjects(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "/api/projects/reorder accepts POST"})
		return
	}
	fields, ok := decodeProjectBody(w, r)
	if !ok {
		return
	}
	// Unlike create/update, reorder has no sensible "default section":
	// an omitted/typo'd status must 422, never silently rewrite Todo.
	status, ok := fields["status"].(string)
	if !ok || strings.TrimSpace(status) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "status is required"})
		return
	}
	rawIDs, ok := fields["ids"].([]any)
	if !ok {
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "ids must be an array of project ids"})
		return
	}
	ids := make([]string, 0, len(rawIDs))
	for _, v := range rawIDs {
		s, ok := v.(string)
		if !ok {
			writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "ids must be an array of project ids"})
			return
		}
		ids = append(ids, s)
	}
	store, ok := projectStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := store.Reorder(ctx, status, ids); err != nil {
		writeProjectErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bson.M{"ok": true})
}

// projectsRoot serves the collection endpoint: GET lists, POST creates.
func projectsRoot(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		if code, detail := authorize(r); code != 0 {
			writeJSON(w, code, bson.M{"detail": detail})
			return
		}
		createProject(w, r)
		return
	}
	listProjects(w, r)
}

// moveProjects relocates one project inside its status section (#527):
// body {"status":"Todo","id":"<id>","before":"<anchor>"} or
// {"status":"Todo","id":"<id>","after":"<anchor>"} — exactly one anchor.
// Callers never send the whole section (the server still reads the section
// to slot the row, but nothing over the wire grows with it), so sections
// over the 500-row reorder cap stay movable. updatedAt is untouched, like
// reorder.
func moveProjects(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "/api/projects/move accepts POST"})
		return
	}
	fields, ok := decodeProjectBody(w, r)
	if !ok {
		return
	}
	// Like reorder, move has no sensible default section: an omitted or
	// typo'd status must 422, never silently move inside Todo.
	status, _ := fields["status"].(string)
	if strings.TrimSpace(status) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "status is required"})
		return
	}
	id, _ := fields["id"].(string)
	if strings.TrimSpace(id) == "" {
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "id is required"})
		return
	}
	before, _ := fields["before"].(string)
	after, _ := fields["after"].(string)
	if (strings.TrimSpace(before) == "") == (strings.TrimSpace(after) == "") {
		writeJSON(w, http.StatusUnprocessableEntity, bson.M{"detail": "exactly one of before/after is required"})
		return
	}
	store, ok := projectStore(w)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	anchor := before
	isAfter := false
	if strings.TrimSpace(after) != "" {
		anchor = after
		isAfter = true
	}
	if err := store.Move(ctx, status, id, anchor, isAfter); err != nil {
		writeProjectErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bson.M{"ok": true})
}

// registerProjects wires this domain's routes. One function per file so main stays a
// list of domains, not a list of paths (#171).
func registerProjects(mux *http.ServeMux) {
	mux.HandleFunc("/api/projects", projectsRoot)
	// Exact patterns win over the /api/projects/ subtree: reorder and move
	// are collection verbs, never project ids. projectItem also dispatches
	// the trailing-slash forms, which land there as those ids.
	mux.HandleFunc("/api/projects/reorder", reorderProjects)
	mux.HandleFunc("/api/projects/move", moveProjects)
	mux.HandleFunc("/api/projects/", projectItem)
}
