package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealth(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	health(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"status":"ok"`) {
		t.Fatalf("got %s", w.Body.String())
	}
}

func TestAuthorize(t *testing.T) {
	t.Setenv("PASSWORD", "test-secret-12345678")
	req := httptest.NewRequest(http.MethodPost, "/api/health/sync", nil)
	if code, _ := authorize(req); code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d", code)
	}
	req.Header.Set("Authorization", "Bearer wrong")
	if code, _ := authorize(req); code != http.StatusUnauthorized {
		t.Fatalf("wrong token: got %d", code)
	}
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	if code, _ := authorize(req); code != 0 {
		t.Fatalf("valid token: got %d", code)
	}
}

func TestLocalDate(t *testing.T) {
	// 2026-09-20T02:00:00Z = 07:30 IST same date.
	if d := localDate("2026-09-20T02:00:00Z"); d != "2026-09-20" {
		t.Fatalf("got %s", d)
	}
	// 2026-09-20T20:00:00Z = 01:30 IST next day.
	if d := localDate("2026-09-20T20:00:00Z"); d != "2026-09-21" {
		t.Fatalf("got %s", d)
	}
	if d := localDate("not-a-date"); d != "not-a-date"[:10] {
		t.Fatalf("got %s", d)
	}
}

func TestMinutesBetween(t *testing.T) {
	m, ok := minutesBetween("2026-09-20T08:00:00+05:30", "2026-09-20T08:30:00+05:30")
	if !ok || m != 30 {
		t.Fatalf("got %d %v", m, ok)
	}
	if _, ok := minutesBetween("bogus", "2026-09-20T08:30:00+05:30"); ok {
		t.Fatal("expected failure")
	}
}

func TestListTasksGuard(t *testing.T) {
	t.Setenv("PASSWORD", "test-secret-12345678")
	// No token → 401 without touching MongoDB.
	req := httptest.NewRequest(http.MethodGet, "/api/tasks?state=open", nil)
	w := httptest.NewRecorder()
	listTasks(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d", w.Code)
	}
	// Wrong method → 405 without touching MongoDB.
	req = httptest.NewRequest(http.MethodPost, "/api/tasks", nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w = httptest.NewRecorder()
	listTasks(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post: got %d", w.Code)
	}
}

func TestTaskItemGuard(t *testing.T) {
	t.Setenv("PASSWORD", "test-secret-12345678")
	// No token → 401 without touching MongoDB.
	for _, target := range []string{"/api/tasks/abc", "/api/tasks/abc/complete", "/api/tasks"} {
		req := httptest.NewRequest(http.MethodPost, target, nil)
		w := httptest.NewRecorder()
		if strings.Contains(target, "/abc") {
			taskItem(w, req)
		} else {
			tasksRoot(w, req)
		}
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s missing token: got %d", target, w.Code)
		}
	}
	// Unknown action → 404 without touching MongoDB.
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/abc/frobnicate", nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w := httptest.NewRecorder()
	taskItem(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown action: got %d", w.Code)
	}
	// Wrong method on a known action → 405 without touching MongoDB.
	req = httptest.NewRequest(http.MethodGet, "/api/tasks/abc/complete", nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w = httptest.NewRecorder()
	taskItem(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("get on complete: got %d", w.Code)
	}
	// Invalid JSON body → 422 without touching MongoDB.
	req = httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader("{oops"))
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w = httptest.NewRecorder()
	tasksRoot(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad json: got %d", w.Code)
	}
}

func TestTaskFieldValidation(t *testing.T) {
	t.Setenv("PASSWORD", "test-secret-12345678")
	post := func(target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer test-secret-12345678")
		w := httptest.NewRecorder()
		if strings.HasPrefix(target, "/api/tasks/") {
			taskItem(w, req)
		} else {
			tasksRoot(w, req)
		}
		return w
	}
	// Mistyped / fractional values fail before any MongoDB touch.
	for _, body := range []string{
		`{"name":"x","estimated_minutes":"lots"}`,
		`{"name":"x","estimated_minutes":1.5}`,
		`{"name":"x","estimated_minutes":-3}`,
		`{"name":"x","description":42}`,
		`{"name":"x","due_date":20260926}`,
	} {
		if w := post("/api/tasks", body); w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s: got %d (%s)", body, w.Code, w.Body.String())
		}
	}
	// Trailing slash with no id behaves like the collection root: GET is
	// auth-gated (tested) and would list; POST create validates first.
	if w := post("/api/tasks/", `{"estimated_minutes":"lots"}`); w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("trailing slash create: got %d", w.Code)
	}
	// PATCH validation also runs before any MongoDB touch.
	req := httptest.NewRequest(http.MethodPatch, "/api/tasks/abc", strings.NewReader(`{"estimated_minutes":2.5}`))
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w := httptest.NewRecorder()
	taskItem(w, req)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("patch fractional: got %d (%s)", w.Code, w.Body.String())
	}
}
