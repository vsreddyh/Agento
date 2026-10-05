package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The skip endpoint's body decoding (#209).
//
// Two properties, and the second is the one that was actually broken:
//
//  1. an EMPTY body is valid, because the reason is optional;
//  2. an UNKNOWN key is a 422, because `reason` is the whole point of a skip and a
//     misspelt key used to decode into an empty reason — leaving the user believing
//     they had recorded why, with nothing recorded.

func skipDecode(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(http.MethodPost, "/api/tasks/x/skip", nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, "/api/tasks/x/skip", strings.NewReader(body))
	}
	fields, ok := decodeTaskBodyOpts(rec, req, bodyOpts{
		allowEmpty: true,
		known:      map[string]bool{"reason": true},
	})
	_ = fields
	if !ok {
		return rec
	}
	rec.Code = http.StatusOK
	rec.Body.Reset()
	rec.Body.WriteString("accepted")
	return rec
}

func TestSkipBodyAcceptsAnEmptyBody(t *testing.T) {
	for _, body := range []string{"", "{}", "   ", "\n"} {
		if rec := skipDecode(t, body); rec.Code != http.StatusOK {
			t.Errorf("body %q rejected with %d: %s — the reason is optional",
				body, rec.Code, rec.Body.String())
		}
	}
}

func TestSkipBodyRejectsATypoedReason(t *testing.T) {
	rec := skipDecode(t, `{"reson": "out of time"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a typo'd key was accepted (status %d) — the reason is silently lost", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "reson") {
		t.Errorf("the 422 does not name the offending field: %s", rec.Body.String())
	}
	// It must also say what IS accepted, or the client is left guessing.
	if !strings.Contains(rec.Body.String(), "reason") {
		t.Errorf("the 422 does not list the accepted fields: %s", rec.Body.String())
	}
}

// The check must be a real allow-list, not a heuristic: every plausible misspelling
// and near-miss is rejected, and the right one is accepted.
func TestSkipBodyAllowListIsExact(t *testing.T) {
	bad := []string{
		`{"reson":"x"}`, `{"reasn":"x"}`, `{"Reason":"x"}`, `{"reason ":"x"}`,
		`{"skip_reason":"x"}`, `{"reason":"x","extra":1}`,
	}
	for _, body := range bad {
		if rec := skipDecode(t, body); rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("body %s accepted (status %d); unknown keys must be rejected", body, rec.Code)
		}
	}
	if rec := skipDecode(t, `{"reason":"out of time"}`); rec.Code != http.StatusOK {
		t.Errorf("the one valid body was rejected: %d %s", rec.Code, rec.Body.String())
	}
}

// The message must be DETERMINISTIC. Go randomises map iteration, so an unsorted
// multi-key message would flap between test runs — and flap in production logs, which
// is worse than being terse.
func TestSkipBodyUnknownFieldMessageIsStable(t *testing.T) {
	body := `{"zeta":1,"alpha":2,"mid":3}`
	first := skipDecode(t, body).Body.String()
	for i := 0; i < 50; i++ {
		if got := skipDecode(t, body).Body.String(); got != first {
			t.Fatalf("message is not stable:\n %s\n %s", first, got)
		}
	}
	if !strings.Contains(first, "alpha, mid, zeta") {
		t.Errorf("fields are not sorted in the message: %s", first)
	}
}

// The other two callers must be UNAFFECTED: create and update accept a wide field set
// and must keep ignoring keys they do not recognise, because their bodies legitimately
// carry fields this handler does not read.
func TestDefaultDecodeStillAcceptsUnknownKeys(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks",
		strings.NewReader(`{"name":"n","something_new":"v"}`))
	fields, ok := decodeTaskBody(rec, req)
	if !ok {
		t.Fatalf("create-style decode rejected an unknown key (status %d): %s",
			rec.Code, rec.Body.String())
	}
	if fields["something_new"] != "v" {
		t.Errorf("the unknown key was dropped rather than passed through: %v", fields)
	}
}

// And an empty body is still a 422 for those callers — only skip made it optional.
func TestDefaultDecodeStillRejectsAnEmptyBody(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks", nil)
	if _, ok := decodeTaskBody(rec, req); ok {
		t.Error("create-style decode accepted an empty body; only skip may do that")
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}
}
