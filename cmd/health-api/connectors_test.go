package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Connectors live on health-api now (#488). These tests pin the HTTP
// envelope without MongoDB: auth/method/shape guards run before any store
// touch, and the memory fallback serves list/disconnect.

func TestConnectorsListGuard(t *testing.T) {
	t.Setenv("PASSWORD", "test-secret-12345678")
	// No token → 401 without touching any store.
	req := httptest.NewRequest(http.MethodGet, "/api/connectors", nil)
	w := httptest.NewRecorder()
	listConnectors(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d", w.Code)
	}
	// Wrong method → 405 without touching any store.
	req = httptest.NewRequest(http.MethodPost, "/api/connectors", nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w = httptest.NewRecorder()
	listConnectors(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post: got %d", w.Code)
	}
}

func TestConnectorsListMemoryFallback(t *testing.T) {
	// No MONGODB_URI in test env → memory fallback lists the two v1 rows
	// as disconnected, never a 500.
	t.Setenv("PASSWORD", "test-secret-12345678")
	req := httptest.NewRequest(http.MethodGet, "/api/connectors", nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w := httptest.NewRecorder()
	listConnectors(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"gmail"`) || !strings.Contains(body, `"calendar"`) {
		t.Fatalf("want both v1 rows, got %s", body)
	}
	if !strings.Contains(body, `"disconnected"`) {
		t.Fatalf("fresh fallback rows read as disconnected, got %s", body)
	}
	// Fresh rows carry no error: last_error omits rather than echoing empty.
	if strings.Contains(body, "last_error") {
		t.Fatalf("fresh rows must omit last_error, got %s", body)
	}
}

func TestConnectorItemGuard(t *testing.T) {
	t.Setenv("PASSWORD", "test-secret-12345678")
	withAuth := func(method, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		req.Header.Set("Authorization", "Bearer test-secret-12345678")
		w := httptest.NewRecorder()
		connectorItem(w, req)
		return w
	}
	// No token → 401.
	req := httptest.NewRequest(http.MethodPost, "/api/connectors/gmail/disconnect", nil)
	w := httptest.NewRecorder()
	connectorItem(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: got %d", w.Code)
	}
	// Unknown id → 404 echoing the normalised id.
	if w := withAuth(http.MethodPost, "/api/connectors/drive/disconnect"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown id: got %d (%s)", w.Code, w.Body.String())
	} else if body := w.Body.String(); !strings.Contains(body, "unknown connector drive") {
		t.Fatalf("unknown id echo: got %s", body)
	}
	// Oversized id → 400 with no echo, before any fold or scan.
	big := strings.Repeat("a", 3000)
	if w := withAuth(http.MethodPost, "/api/connectors/"+big+"/disconnect"); w.Code != http.StatusBadRequest {
		t.Fatalf("oversized id: got %d (%s)", w.Code, w.Body.String())
	}
	// Control-bearing id → bare 404 with no echo.
	if w := withAuth(http.MethodPost, "/api/connectors/gmail%00/disconnect"); w.Code != http.StatusNotFound {
		t.Fatalf("control id: got %d (%s)", w.Code, w.Body.String())
	} else if body := w.Body.String(); strings.Contains(body, "%00") || !strings.Contains(body, `"unknown connector"`) {
		t.Fatalf("control id must answer bare 404, got %s", body)
	}
	// Unknown verb → 404 echoing the normalised action.
	if w := withAuth(http.MethodPost, "/api/connectors/gmail/frobnicate"); w.Code != http.StatusNotFound {
		t.Fatalf("unknown verb: got %d (%s)", w.Code, w.Body.String())
	} else if body := w.Body.String(); !strings.Contains(body, "unknown path frobnicate") {
		t.Fatalf("unknown verb echo: got %s", body)
	}
	if w := withAuth(http.MethodPost, "/api/connectors/gmail/"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("no verb: got %d (%s)", w.Code, w.Body.String())
	} else if allow := w.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("no verb Allow: got %q", allow)
	}
	// Wrong method on a known verb → 405 with Allow.
	if w := withAuth(http.MethodGet, "/api/connectors/gmail/disconnect"); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("get on disconnect: got %d (%s)", w.Code, w.Body.String())
	} else if allow := w.Header().Get("Allow"); allow != http.MethodPost {
		t.Fatalf("disconnect Allow: got %q", allow)
	}
	// List + item verbs answer no-store (the app polls this loop).
	lreq := httptest.NewRequest(http.MethodGet, "/api/connectors", nil)
	lreq.Header.Set("Authorization", "Bearer test-secret-12345678")
	lw := httptest.NewRecorder()
	listConnectors(lw, lreq)
	if cc := lw.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("list Cache-Control: got %q", cc)
	}
}

func TestConnectorDisconnectMemoryFallback(t *testing.T) {
	// Disconnect on a fresh (disconnected) row is a no-op success.
	t.Setenv("PASSWORD", "test-secret-12345678")
	req := httptest.NewRequest(http.MethodPost, "/api/connectors/gmail/disconnect", nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w := httptest.NewRecorder()
	connectorItem(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"disconnected"`) {
		t.Fatalf("want disconnected row, got %s", w.Body.String())
	}
}

func TestConnectorConnectUnconfigured(t *testing.T) {
	// No OAuth env in tests (and no durable store) → 501, never a secrets
	// prompt, without minting anything.
	t.Setenv("PASSWORD", "test-secret-12345678")
	req := httptest.NewRequest(http.MethodPost, "/api/connectors/gmail/connect", nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w := httptest.NewRecorder()
	connectorItem(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("got %d (%s)", w.Code, w.Body.String())
	}
}

func TestConnectorCallbackGuards(t *testing.T) {
	// No OAuth env in tests (and no durable store): the config check runs
	// before param parsing, so a bare callback 501s instead of naming
	// params — same ordering as the gateway.
	req := httptest.NewRequest(http.MethodGet, "/api/connectors/callback?code=abc", nil)
	w := httptest.NewRecorder()
	connectorCallback(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured: got %d (%s)", w.Code, w.Body.String())
	}
	// Wrong method → 405 before anything else.
	req = httptest.NewRequest(http.MethodPost, "/api/connectors/callback?code=a&state=b", nil)
	w = httptest.NewRecorder()
	connectorCallback(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("post: got %d (%s)", w.Code, w.Body.String())
	}
	// Oversized query dead-attempts 400 even while OAuth is unconfigured
	// (the cap carries no config information). Sized past the raw cap
	// (3 × 3 × MaxStateFieldLen + 256, same as the gateway).
	big := strings.Repeat("a", 12000)
	req = httptest.NewRequest(http.MethodGet, "/api/connectors/callback?code="+big+"&state="+big, nil)
	w = httptest.NewRecorder()
	connectorCallback(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("oversized: got %d (%s)", w.Code, w.Body.String())
	}
}

// The callback and both spellings resolve to the connector handlers, not
// to anything else on the mux.
func TestConnectorsRouteRegistration(t *testing.T) {
	mux := http.NewServeMux()
	registerConnectors(mux)
	t.Setenv("PASSWORD", "test-secret-12345678")
	// Exact callback spelling reaches the callback (501 here: OAuth is
	// unconfigured in tests), proving the mux picked the connector
	// handler.
	req := httptest.NewRequest(http.MethodGet, "/api/connectors/callback", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("callback route: got %d (%s)", w.Code, w.Body.String())
	}
	// Trailing-slash collection reaches the list guard (POST → 405).
	req = httptest.NewRequest(http.MethodPost, "/api/connectors/", nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("collection/ route: got %d (%s)", w.Code, w.Body.String())
	}
	// HTML negotiation on the callback answers the page envelope.
	req = httptest.NewRequest(http.MethodGet, "/api/connectors/callback", nil)
	req.Header.Set("Accept", "text/html")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented || !strings.Contains(w.Body.String(), "<html>") {
		t.Fatalf("html callback: got %d (%s)", w.Code, w.Body.String())
	}
	// Wrong method answers the page envelope too, with Allow: GET.
	req = httptest.NewRequest(http.MethodPost, "/api/connectors/callback", nil)
	req.Header.Set("Accept", "text/html")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusMethodNotAllowed || !strings.Contains(w.Body.String(), "<html>") {
		t.Fatalf("html callback 405: got %d (%s)", w.Code, w.Body.String())
	}
	if allow := w.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("callback Allow: got %q", allow)
	}
}
