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

// Every body key the create path reads must change the idempotency fingerprint.
//
// This is the drift guard for taskCreateFields. `repeat_rule` was genuinely missing —
// consumed into Repeat.Text, validated, and stored, yet absent from the digest — so
// two creates differing only in their custom repeat text hashed identically and the
// second was handed the first's task as a "replay". That is the silent wrong answer,
// reached through the mechanism built to prevent it.
//
// The test walks the list and mutates one key at a time, so the next field someone
// adds to the create path without adding it here fails here rather than in review.
func TestFingerprintCoversEveryConsumedField(t *testing.T) {
	base := map[string]any{
		"name": "task", "description": "d",
		"due_date": "2026-10-05", "due_time": "08:00",
		"estimated_minutes": 5, "parallelable": false,
		"repeat_every": 1, "repeat_unit": "days",
		"repeat_custom": false, "repeat_rule": "",
	}
	baseFP := taskFingerprint(base)
	if baseFP == "" {
		t.Fatal("a complete body produced no fingerprint; the mismatch check would be off")
	}

	// A different value per key. repeat_custom/repeat_every/repeat_unit have typed
	// expectations, so use values of the right shape rather than one string for all.
	differ := map[string]any{
		"name": "other", "description": "other",
		"due_date": "2026-10-06", "due_time": "09:00",
		"estimated_minutes": 30, "parallelable": true,
		"repeat_every": 2, "repeat_unit": "weeks",
		"repeat_custom": true, "repeat_rule": "3rd Friday",
	}
	for _, k := range taskCreateFields {
		if _, ok := differ[k]; !ok {
			t.Errorf("taskCreateFields lists %q but the test has no differing value for it", k)
			continue
		}
		mutated := make(map[string]any, len(base))
		for kk, vv := range base {
			mutated[kk] = vv
		}
		mutated[k] = differ[k]
		if taskFingerprint(mutated) == baseFP {
			t.Errorf("changing %q does NOT change the fingerprint — a request differing "+
				"only in that field would replay as identical instead of 422", k)
		}
	}
}

// The specific defect, pinned directly: two custom repeats whose text differs must
// not share a digest. This is the case that shipped broken.
func TestFingerprintDistinguishesCustomRepeatText(t *testing.T) {
	body := func(rule string) map[string]any {
		return map[string]any{
			"name": "monthly-ish", "description": "d",
			"due_date": "2026-10-05", "due_time": "08:00",
			"estimated_minutes": 5, "parallelable": false,
			"repeat_custom": true, "repeat_rule": rule,
		}
	}
	a := taskFingerprint(body("3rd Friday"))
	b := taskFingerprint(body("end of month"))
	if a == "" || b == "" {
		t.Fatalf("a custom-only body produced no fingerprint (%q, %q); the mismatch "+
			"check would be disabled for every custom repeat", a, b)
	}
	if a == b {
		t.Error("two different custom repeat rules hash identically — the second create " +
			"would replay the first's task")
	}
	// And an identical body must still match, or every genuine retry would 422.
	if taskFingerprint(body("3rd Friday")) != a {
		t.Error("the fingerprint is not stable for an unchanged body")
	}
}

// An absent key and a present-but-empty key are different bodies for the digest's
// purposes only if the create treats them differently — it does not, so they must
// hash the same or a client that starts sending an explicit empty value would 422 its
// own retries.
func TestFingerprintTreatsAbsentAndEmptyAlike(t *testing.T) {
	withEmpty := map[string]any{
		"name": "t", "description": "d",
		"due_date": "2026-10-05", "due_time": "08:00",
		"estimated_minutes": 5, "parallelable": false,
		"repeat_custom": false, "repeat_rule": "",
	}
	without := map[string]any{
		"name": "t", "description": "d",
		"due_date": "2026-10-05", "due_time": "08:00",
		"estimated_minutes": 5, "parallelable": false,
		"repeat_custom": false,
	}
	if taskFingerprint(withEmpty) != taskFingerprint(without) {
		t.Error("an explicit empty repeat_rule changed the digest; the store treats " +
			"absent and empty alike, so the digest must too")
	}
}

// The zero-defaulting must be complete: absent vs explicit zero must agree for EVERY
// consumed field, not just the string one that the repeat_rule bug exposed. Each of
// these is a body a client could send either way, and a mismatch on any of them is a
// 422 on a legitimate retry.
func TestAbsentAndExplicitZeroAgreeForEveryField(t *testing.T) {
	full := map[string]any{
		"name": "t", "description": "d",
		"due_date": "2026-10-05", "due_time": "08:00",
		"estimated_minutes": 5, "parallelable": true,
		"repeat_every": 2, "repeat_unit": "weeks",
		"repeat_custom": true, "repeat_rule": "3rd Friday",
	}
	// For each key, the zero value of its type as the store would read it.
	zeros := map[string]any{
		"description": "", "due_time": "", "repeat_unit": "", "repeat_rule": "",
		"estimated_minutes": 0, "repeat_every": 0,
		"parallelable": false, "repeat_custom": false,
	}
	for k, zero := range zeros {
		mutated := make(map[string]any, len(full))
		for kk, vv := range full {
			mutated[kk] = vv
		}
		mutated[k] = zero
		dropped := make(map[string]any, len(full))
		for kk, vv := range full {
			dropped[kk] = vv
		}
		delete(dropped, k)
		if taskFingerprint(mutated) != taskFingerprint(dropped) {
			t.Errorf("omitting %q and sending it as its zero value hash differently; "+
				"the store reads them alike, so the digest must too", k)
		}
	}
}

// The digest must agree with the STORE about what "the same request" means. The store
// TrimSpaces name/description/due_date/due_time and taskRepeat TrimSpaces repeat_unit,
// so a body padded with whitespace creates the same task as the trimmed one. Hashing it
// verbatim made the digest stricter than reality, and a retry differing only in padding
// got 422 Mismatch instead of a replay.
//
// Same class as the repeat_rule omission: the digest is only correct if it matches what
// actually gets stored.
func TestFingerprintAgreesWithWhatTheStoreTrims(t *testing.T) {
	trimmed := map[string]any{
		"name": "padded name", "description": "padded desc",
		"due_date": "2026-10-05", "due_time": "08:00",
		"estimated_minutes": 5, "parallelable": false,
		"repeat_every": 2, "repeat_unit": "weeks",
		"repeat_custom": false, "repeat_rule": "",
	}
	padded := map[string]any{}
	for k, v := range trimmed {
		padded[k] = v
	}
	// Pad every field the store trims. NOT repeat_rule: create stores that verbatim,
	// so "  a  " and "a" really are different stored values and must digest
	// differently.
	for k := range trimmedForFingerprint {
		if str, ok := padded[k].(string); ok {
			padded[k] = "  " + str + "  "
		}
	}
	if taskFingerprint(trimmed) != taskFingerprint(padded) {
		t.Error("whitespace-padded fields digest differently even though the store trims " +
			"them, so a legitimate retry would 422 instead of replaying")
	}
	// The reverse direction must still hold: a field the store does NOT trim has to
	// keep distinguishing, or two genuinely different requests would replay as one.
	realRule := map[string]any{}
	for k, v := range trimmed {
		realRule[k] = v
	}
	realRule["repeat_rule"] = " 3rd Friday "
	plainRule := map[string]any{}
	for k, v := range trimmed {
		plainRule[k] = v
	}
	plainRule["repeat_rule"] = "3rd Friday"
	if taskFingerprint(realRule) == taskFingerprint(plainRule) {
		t.Error("padded repeat_rule digests the same as the plain one, but create stores " +
			"it verbatim — two different stored tasks would replay as identical")
	}
}

// The liveness probe must never be throttled: compose healthchecks /health with
// `curl -f`, so a 429 under load fails the probe and can mark the container unhealthy
// exactly when it is already struggling. /health touches no database, so throttling it
// protects nothing.
func TestHealthProbeIsNeverThrottled(t *testing.T) {
	l, _ := newLimiterWithClock(1, 1.0)
	probed, limited := 0, 0
	h := rateLimit(l, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	for i := 0; i < 25; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		req.RemoteAddr = "203.0.113.9:5555"
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("probe %d was throttled with %d; the container would flap under load", i+1, rec.Code)
		}
		probed++
	}
	// And the exemption is narrow: a path that merely STARTS with /health must still be
	// limited, or the wrong route would be exempt.
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/health/sync", nil)
		req.RemoteAddr = "203.0.113.9:5555"
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			limited++
		}
	}
	if limited == 0 {
		t.Errorf("/api/health/sync was never throttled — the exemption is too broad (%d/%d limited)", limited, 5)
	}
}
