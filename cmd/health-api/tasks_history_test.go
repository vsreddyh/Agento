package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"agento/internal/tasks"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// The change log route must answer without a database for every guard: an
// unauthorised caller, a wrong method and a malformed filter are all decided
// before any MongoDB touch.
func TestTaskMutationsGuard(t *testing.T) {
	t.Setenv("PASSWORD", "test-secret-12345678")
	// No token → 401 without touching MongoDB.
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/mutations", nil)
	w := httptest.NewRecorder()
	taskMutations(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d", w.Code)
	}
	// Wrong method → 405 without touching MongoDB.
	req = httptest.NewRequest(http.MethodPost, "/api/tasks/mutations", nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w = httptest.NewRecorder()
	taskMutations(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post: got %d", w.Code)
	}
	// Deeper than the log → 404, not an answer for a URL that is not it.
	req = httptest.NewRequest(http.MethodGet, "/api/tasks/mutations/anything", nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w = httptest.NewRecorder()
	taskMutations(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("mutations/anything: got %d (%s)", w.Code, w.Body.String())
	}
}

// A malformed filter is 422, and it says which filter was wrong: silently
// widening the answer to "the whole log" would be the wrong data, not merely
// more of it.
func TestTaskMutationsRejectsBadFilters(t *testing.T) {
	t.Setenv("PASSWORD", "test-secret-12345678")
	cases := []struct {
		target string
		want   string
	}{
		{"/api/tasks/mutations?task_id=not-an-id", "task_id"},
		{"/api/tasks/mutations?op=frobnicate", "frobnicate"},
		{"/api/tasks/mutations?since=yesterday", "since"},
		// A filter that names no op is a filter the caller sent: answering
		// it with the whole log is the wrong data, so it is refused.
		{"/api/tasks/mutations?op=,", "names no operation"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(http.MethodGet, tc.target, nil)
		req.Header.Set("Authorization", "Bearer test-secret-12345678")
		w := httptest.NewRecorder()
		taskMutations(w, req)
		if w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: got %d (%s), want 422", tc.target, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s: body must name %q: %s", tc.target, tc.want, w.Body.String())
		}
	}
}

// The route must resolve as its own path, not as a task id: /api/tasks/ is
// the catch-all, and a registration that lost to it would answer "unknown
// task 'mutations'" instead of the log (#350).
func TestTaskMutationsRouteRegistration(t *testing.T) {
	mux := http.NewServeMux()
	registerTasks(mux)
	t.Setenv("PASSWORD", "test-secret-12345678")
	// Authenticated + wrong method reaches the handler, and the handler's own
	// 405 — not the id path's — proves which one the mux picked.
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/mutations", nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("mutations route not resolved: got %d (%s)", w.Code, w.Body.String())
	}
	// The trailing-slash variant must reach the same handler, not the
	// task-id catch-all (which would 422 "unknown task 'mutations'").
	req = httptest.NewRequest(http.MethodPost, "/api/tasks/mutations/", nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("mutations/ route not resolved: got %d (%s)", w.Code, w.Body.String())
	}
}

// The response contract: one entry per change, and every entry carrying the
// four facts the log exists for — what, which task, who, when. This is the
// shape the app reads, so it is pinned here rather than left to whichever
// field names the struct happens to have.
func TestTaskMutationsResponseShape(t *testing.T) {
	uri := os.Getenv("MONGODB_URI")
	if uri == "" {
		t.Skip("MONGODB_URI not set")
	}
	const dbName = "task_history_http_test"
	store, err := tasks.New(uri, dbName)
	if err != nil {
		t.Skipf("mongo unreachable: %v", err)
	}
	// The store opened here is the test's own; the store the handler uses
	// comes from the process-wide cache and lives as long as the process,
	// exactly as it does in the server.
	defer store.Close(context.Background())
	ctx := context.Background()
	// The store is cached process-wide, and the cache is filled from the
	// environment — so the test points the environment at its own database
	// and empties the cache, or it would read (and write) the live one.
	t.Setenv("MONGODB_DB", dbName)
	tasksStoreMu.Lock()
	tasksCache = nil
	tasksStoreMu.Unlock()
	t.Cleanup(func() {
		tasksStoreMu.Lock()
		defer tasksStoreMu.Unlock()
		// The cached store is process-lifetime in the server — one client
		// for the life of the process. In a test process that would be a
		// leaked client per run, so it is closed as the cache is emptied.
		if tasksCache != nil {
			_ = tasksCache.Close(context.Background())
			tasksCache = nil
		}
	})

	// A fresh id per run: this database is not dropped between runs, so a
	// fixed id would collect the previous run's rows and the count
	// assertions would be measuring history instead of the contract.
	id := primitive.NewObjectID().Hex()
	store.RecordMutation(ctx, tasks.OpComplete, id, "mcp", "none")
	// The two writes must land in DIFFERENT milliseconds: `at` is stored at
	// millisecond precision, and two entries sharing one timestamp would
	// make the inclusive `since` below match both — the test would then
	// measure the clock, not the contract.
	time.Sleep(2 * time.Millisecond)
	store.RecordMutation(ctx, tasks.OpCreate, id, "http", "http POST /api/tasks")

	t.Setenv("PASSWORD", "test-secret-12345678")
	req := httptest.NewRequest(http.MethodGet, "/api/tasks/mutations?task_id="+id, nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w := httptest.NewRecorder()
	taskMutations(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", w.Code, w.Body.String())
	}
	var body struct {
		Mutations []struct {
			TaskID string `json:"task_id"`
			Op     string `json:"op"`
			Source string `json:"source"`
			Detail string `json:"detail"`
			At     string `json:"at"`
		} `json:"mutations"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %s: %v", w.Body.String(), err)
	}
	if len(body.Mutations) != 2 {
		t.Fatalf("mutations = %d, want 2: %s", len(body.Mutations), w.Body.String())
	}
	if body.Truncated {
		t.Error("two rows must not be truncated")
	}
	// Newest first: the create was recorded last, so it leads.
	if body.Mutations[0].Op != tasks.OpCreate {
		t.Errorf("first op = %q, want %q", body.Mutations[0].Op, tasks.OpCreate)
	}
	first := body.Mutations[0]
	if first.TaskID != id {
		t.Errorf("task_id = %q, want %q", first.TaskID, id)
	}
	if first.Source != "http" {
		t.Errorf("source = %q, want http", first.Source)
	}
	if first.Detail != "http POST /api/tasks" {
		t.Errorf("detail = %q, want the create detail", first.Detail)
	}
	// The timestamp must be RFC3339Nano: the app parses it, and the
	// filter it feeds accepts that shape. RFC3339Nano is the layout the
	// `since` filter uses, so the two agree on the format by construction
	// rather than by luck.
	if _, err := time.Parse(time.RFC3339Nano, first.At); err != nil {
		t.Errorf("at = %q, want RFC3339Nano: %v", first.At, err)
	}
	// The older entry is still there, with its own source — the two writers
	// are distinguishable, which is the point of the field.
	older := body.Mutations[1]
	if older.Op != tasks.OpComplete {
		t.Errorf("second op = %q, want %q", older.Op, tasks.OpComplete)
	}
	if older.Source != "mcp" {
		t.Errorf("second source = %q, want mcp", older.Source)
	}

	// The `at` a caller is given must be usable as the `since` it sends
	// back: the timestamp carries milliseconds, and a filter that rejected
	// the format the read path hands out would 422 a client doing exactly
	// what it was told.
	req = httptest.NewRequest(http.MethodGet, "/api/tasks/mutations?task_id="+id+"&since="+url.QueryEscape(first.At), nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w = httptest.NewRecorder()
	taskMutations(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("since=%s: got %d (%s)", first.At, w.Code, w.Body.String())
	}
	var round struct {
		Mutations []struct {
			Op string `json:"op"`
		} `json:"mutations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &round); err != nil {
		t.Fatalf("decoding %s: %v", w.Body.String(), err)
	}
	// Inclusive: the entry whose `at` was echoed back is still in the
	// window.
	if len(round.Mutations) != 1 || round.Mutations[0].Op != tasks.OpCreate {
		t.Fatalf("round-tripped since = %d rows (%s), want the echoed entry", len(round.Mutations), w.Body.String())
	}

	// A malformed limit is LENIENT, unlike the filters above: it can only
	// make the page bigger, which is the answer the caller already had.
	// A stray query param must never nuke the answer.
	req = httptest.NewRequest(http.MethodGet, "/api/tasks/mutations?task_id="+id+"&limit=garbage", nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w = httptest.NewRecorder()
	taskMutations(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("limit=garbage: got %d (%s)", w.Code, w.Body.String())
	}
	var lenient struct {
		Mutations []struct {
			Op string `json:"op"`
		} `json:"mutations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &lenient); err != nil {
		t.Fatalf("decoding %s: %v", w.Body.String(), err)
	}
	if len(lenient.Mutations) != 2 {
		t.Fatalf("limit=garbage returned %d rows (%s), want the default page of 2",
			len(lenient.Mutations), w.Body.String())
	}
}
