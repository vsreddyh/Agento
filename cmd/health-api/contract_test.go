package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agento/internal/tasks"
)

// The literal bodies the Android app sends (#185).
//
// TasksApi.create builds ONE JSONObject with exactly these ten keys, and
// TasksApi.update sends a subset of the same keys (plus expected_revision).
// These literals are a mirror of that Kotlin, kept key-for-key: when the app
// adds, renames or drops a field, update these bodies in the same change, or
// the next shape change sails through exactly like the 4.6.0 and 4.7.0 ones
// did — parsed with defaults on one side and silently wrong on the other.
//
// What they pin, without a database:
//  1. the app's create body passes this layer's validation (checkTaskFields)
//     and reads back as the Repeat the app meant;
//  2. every key the app sends is covered by the idempotency fingerprint
//     (taskCreateFields), so a retried create cannot replay the wrong task —
//     the repeat_rule omission shipped exactly this bug;
//  3. a partial update body (the keys update actually sends) leaves the
//     recurrence untouched when it carries no repeat keys.
const appCreateBody = `{
	"name": "Take meds",
	"description": "with water",
	"due_date": "2026-10-06",
	"due_time": "09:00",
	"estimated_minutes": 5,
	"parallelable": false,
	"repeat_every": 2,
	"repeat_unit": "weeks",
	"repeat_custom": false,
	"repeat_rule": ""
}`

// One-shot: the app sends the repeat as explicit zeros, never by omission.
const appOneShotBody = `{
	"name": "Take meds",
	"description": "with water",
	"due_date": "2026-10-06",
	"due_time": "09:00",
	"estimated_minutes": 0,
	"parallelable": false,
	"repeat_every": 0,
	"repeat_unit": "",
	"repeat_custom": false,
	"repeat_rule": ""
}`

// A partial edit: only the keys update was given, as update sends them.
const appPartialUpdateBody = `{"name": "Take meds (evening)", "due_time": "20:00"}`

func decodeLiteral(t *testing.T, body string) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks", strings.NewReader(body))
	fields, ok := decodeTaskBody(rec, req)
	if !ok {
		t.Fatalf("the app's own body was rejected (status %d): %s", rec.Code, rec.Body.String())
	}
	return fields
}

func TestAppCreateBodyRoundTrips(t *testing.T) {
	fields := decodeLiteral(t, appCreateBody)
	if err := checkTaskFields(fields); err != nil {
		t.Fatalf("the app's own create body fails validation: %v", err)
	}
	rep := taskRepeat(fields)
	if rep.Every != 2 || rep.Unit != "weeks" || rep.Custom {
		t.Fatalf("the app asked for every 2 weeks and the server read %+v", rep)
	}
	if fp := taskFingerprint(fields); fp == "" {
		t.Fatal("the app's own create body produces no fingerprint; " +
			"the idempotency mismatch check would be off for every app write")
	}
	// Deterministic: a retry serialised differently must still match.
	if taskFingerprint(fields) != taskFingerprint(decodeLiteral(t, appCreateBody)) {
		t.Fatal("the fingerprint is not stable for an unchanged app body")
	}
}

func TestAppOneShotBodyReadsAsOneShot(t *testing.T) {
	fields := decodeLiteral(t, appOneShotBody)
	if err := checkTaskFields(fields); err != nil {
		t.Fatalf("the app's own one-shot body fails validation: %v", err)
	}
	if rep := taskRepeat(fields); rep != (tasks.Repeat{}.Normalize()) {
		t.Fatalf("the app's one-shot body reads as a repeat: %+v", rep)
	}
}

func TestAppCreateBodyKeysAreFingerprinted(t *testing.T) {
	fields := decodeLiteral(t, appCreateBody)
	covered := make(map[string]bool, len(taskCreateFields))
	for _, k := range taskCreateFields {
		covered[k] = true
	}
	for k := range fields {
		if !covered[k] {
			t.Errorf("the app sends %q but the idempotency fingerprint ignores it — "+
				"two creates differing only in that field would replay as one "+
				"(this is how repeat_rule shipped broken)", k)
		}
	}
}

func TestAppPartialUpdateBodyLeavesRepeatAlone(t *testing.T) {
	fields := decodeLiteral(t, appPartialUpdateBody)
	if err := checkTaskFields(fields); err != nil {
		t.Fatalf("the app's own update body fails validation: %v", err)
	}
	// No repeat keys were sent, so there is no recurrence to merge: the store
	// treats that as "leave it alone" (mergeRepeat), and this layer must not
	// invent one from the absence.
	if rep := taskRepeat(fields); rep.Every != 0 || rep.Unit != "" || rep.Custom || rep.Text != "" {
		t.Fatalf("an update carrying no repeat keys reads as a recurrence: %+v", rep)
	}
}
