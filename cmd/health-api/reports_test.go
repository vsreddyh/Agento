package main

// DB-free paths of the ∃-Mail report routes (#365): auth, validation and
// method dispatch fail before any store is opened, so these hold without
// MONGODB_URI.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func reportReq(t *testing.T, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	t.Setenv("PASSWORD", "test-secret-12345678")
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	r.Header.Set("Authorization", "Bearer test-secret-12345678")
	w := httptest.NewRecorder()
	return serveReportReq(w, r)
}

func serveReportReq(w *httptest.ResponseRecorder, r *http.Request) *httptest.ResponseRecorder {
	if strings.HasPrefix(r.URL.Path, "/api/reports/") {
		reportItem(w, r)
	} else {
		reportsRoot(w, r)
	}
	return w
}

func TestReportsRequireAuth(t *testing.T) {
	t.Setenv("PASSWORD", "test-secret-12345678")
	req := httptest.NewRequest(http.MethodGet, "/api/reports", nil)
	w := httptest.NewRecorder()
	reportsRoot(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", w.Code)
	}
}

func TestReportsRejectBadSort(t *testing.T) {
	w := reportReq(t, http.MethodGet, "/api/reports?sort=oldest", "")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d (%s), want 422", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "newest") {
		t.Fatalf("should name the accepted sort: %s", w.Body.String())
	}
}

func TestReportsRejectBadJSON(t *testing.T) {
	w := reportReq(t, http.MethodPatch, "/api/reports/abc", "{oops")
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422", w.Code)
	}
}

func TestReportsRejectUnknownField(t *testing.T) {
	w := reportReq(t, http.MethodPatch, "/api/reports/abc", `{"title":"nope"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d (%s), want 422", w.Code, w.Body.String())
	}
}

func TestReportsRejectNonBool(t *testing.T) {
	w := reportReq(t, http.MethodPatch, "/api/reports/abc", `{"read":"yes"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("got %d, want 422", w.Code)
	}
}

func TestReportsMethodNotAllowed(t *testing.T) {
	for _, m := range []string{http.MethodPost, http.MethodPut} {
		w := reportReq(t, m, "/api/reports/abc", "")
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: got %d, want 405", m, w.Code)
		}
	}
}
