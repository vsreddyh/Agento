package main

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"agento/internal/tasks"
)

// The literal bodies the Android app sends (#185).
//
// TasksApi.create builds ONE JSONObject with exactly these keys, and
// TasksApi.update sends a subset of the same keys (plus expected_revision).
// These literals mirror the bodies the app builds from TASK_CREATE_KEYS, and
// the CI drift step asserts that key set equals taskCreateFields — so when the
// app adds, renames or drops a field, these bodies move in the same change or
// the key-set check fails first. What THEY pin, without a database:
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
	"repeat_days": [],
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
	"repeat_days": [],
	"repeat_custom": false,
	"repeat_rule": ""
}`

// Selected days (#304): the app's "Select days" picker sends the interval keys
// zeroed and the selection filled — the same body shape every other mode uses.
const appSelectedDaysBody = `{
	"name": "Gym",
	"description": "leg day",
	"due_date": "2026-10-07",
	"due_time": "18:00",
	"estimated_minutes": 60,
	"parallelable": false,
	"repeat_every": 0,
	"repeat_unit": "",
	"repeat_days": ["Mon", "wed", "MON"],
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
	if rep := taskRepeat(fields); !reflect.DeepEqual(rep, tasks.Repeat{}.Normalize()) {
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

// The selected-days body the app sends must read as THAT mode, with no residue
// of the interval keys — and it must pass this layer's type checks, since a
// `repeat_days` mistake here is a 422 on a form the user filled in correctly.
func TestAppSelectedDaysBodyReadsAsDays(t *testing.T) {
	fields := decodeLiteral(t, appSelectedDaysBody)
	if err := checkTaskFields(fields); err != nil {
		t.Fatalf("the app's own selected-days body fails validation: %v", err)
	}
	rep := taskRepeat(fields)
	want := []string{"mon", "wed"}
	if !reflect.DeepEqual(rep.Days, want) {
		t.Fatalf("the app asked for %v and the server read %v", want, rep.Days)
	}
	if rep.Every != 0 || rep.Unit != "" || rep.Custom || rep.Text != "" {
		t.Fatalf("the days body reads as another mode too: %+v", rep)
	}
	if !rep.IsStructured() {
		t.Fatal("a selected-days repeat must be structured, so the server rolls it over")
	}
	// The whole point of the mode: the date the app sent is one of the days, and
	// the same request is accepted by the store's own rule.
	if err := rep.Validate(); err != nil {
		t.Fatalf("the app's selection must validate: %v", err)
	}
	if err := rep.CheckDueDate(taskStrField(fields, "due_date")); err != nil {
		t.Fatalf("the app's date must be one of its selected days: %v", err)
	}
	if fp := taskFingerprint(fields); fp == "" {
		t.Fatal("the app's selected-days body produces no fingerprint")
	}
	// And a caller that sends the selection as a bare string gets a 422 here
	// rather than a task that silently stopped repeating.
	bad := decodeLiteral(t, `{"repeat_days": "mon,wed"}`)
	if err := checkTaskFields(bad); err == nil {
		t.Fatal("a string repeat_days must be rejected, not read as no selection")
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
