package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"agento/internal/connectors"
)

// These tests pin the #316 v1 foundation contract: three disconnected rows,
// per-connector disconnect, connect 501s until OAuth lands, secrets never
// appear, and expired rows carry the reconnect hint.

func connectorsServer(t *testing.T) *Server {
	t.Helper()
	return newTestServer(t, &fakeAgent{}, Config{ConnectorStore: connectors.NewMemoryStore()})
}

func TestConnectorsListThreeDisconnected(t *testing.T) {
	srv := connectorsServer(t)
	got := decodeBody(t, do(t, srv, http.MethodGet, "/api/connectors", ""))
	rows, ok := got["connectors"].([]any)
	if !ok || len(rows) != 3 {
		t.Fatalf("connectors = %v", got)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		m := r.(map[string]any)
		seen[m["id"].(string)] = true
		if m["status"] != "disconnected" {
			t.Errorf("%v status = %v, want disconnected", m["id"], m["status"])
		}
		if _, hasToken := m["refresh_token"]; hasToken {
			t.Errorf("%v leaks refresh_token", m["id"])
		}
		if _, hasToken := m["access_token"]; hasToken {
			t.Errorf("%v leaks access_token", m["id"])
		}
		if _, ok := m["scopes"]; !ok {
			t.Errorf("%v has no scopes", m["id"])
		}
	}
	for _, id := range []string{"gmail", "calendar", "drive"} {
		if !seen[id] {
			t.Errorf("missing connector %s", id)
		}
	}
}

func TestConnectorsDisconnectAndUnknown(t *testing.T) {
	srv := connectorsServer(t)
	// Unknown id 404s, never synthesises.
	if rec := do(t, srv, http.MethodPost, "/api/connectors/slack/disconnect", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown disconnect: %d, want 404 (%s)", rec.Code, rec.Body)
	}
	// Wrong verb is 405, not 404: the client fixes its method, not its path.
	if rec := do(t, srv, http.MethodGet, "/api/connectors/gmail/disconnect", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("get disconnect: %d, want 405 (%s)", rec.Code, rec.Body)
	}
	rec := do(t, srv, http.MethodPost, "/api/connectors/gmail/disconnect", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disconnect: %d %s", rec.Code, rec.Body)
	}
	if st := decodeBody(t, rec)["connector"].(map[string]any)["status"]; st != "disconnected" {
		t.Fatalf("status = %v, want disconnected", st)
	}
}

func TestConnectorsConnectIs501NotSecretPrompt(t *testing.T) {
	srv := connectorsServer(t)
	rec := do(t, srv, http.MethodPost, "/api/connectors/gmail/connect", "")
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("connect: %d, want 501 (%s)", rec.Code, rec.Body)
	}
	body := strings.ToLower(rec.Body.String())
	if !strings.Contains(body, "never paste") {
		t.Fatalf("connect 501 should warn against pasting secrets, got %s", rec.Body)
	}
}

func TestConnectorsExpiredCarriesReconnectHint(t *testing.T) {
	srv := connectorsServer(t)
	if _, err := srv.connectors.SetStatus(context.Background(), "gmail", connectors.StatusExpired, "aged out"); err != nil {
		t.Fatal(err)
	}
	got := decodeBody(t, do(t, srv, http.MethodGet, "/api/connectors", ""))
	for _, r := range got["connectors"].([]any) {
		m := r.(map[string]any)
		if m["id"] == "gmail" {
			if m["status"] != "expired" {
				t.Fatalf("gmail status = %v, want expired", m["status"])
			}
			hint, _ := m["reconnect"].(string)
			if !strings.Contains(strings.ToLower(hint), "never paste") {
				t.Fatalf("expired row should carry the no-secrets hint, got %v", m)
			}
		}
	}
}
