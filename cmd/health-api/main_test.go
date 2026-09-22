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
