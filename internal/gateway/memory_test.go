package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agento/internal/memory"
)

// These tests pin the #315 memory contract: per-profile scoping, user writes
// confirmed, agent writes pending, real deletes, topic pauses, and the same
// fail-closed prompt rule the screen implies (pending and paused items are
// shown but not injected).

func do(t *testing.T, srv *Server, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	}
	r.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, r)
	return rec
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
	return out
}

func TestMemoryCRUD(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{}, Config{})

	create := do(t, srv, http.MethodPost, "/api/memory",
		`{"topic":"Work Style","text":"Prefers tables for weekly reports."}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create: %d %s", create.Code, create.Body)
	}
	item := decodeBody(t, create)["item"].(map[string]any)
	id := item["id"].(string)
	if item["status"] != "confirmed" || item["source"] != "user" ||
		item["topic"] != "work-style" {
		t.Fatalf("created item = %+v", item)
	}

	list := do(t, srv, http.MethodGet, "/api/memory", "")
	if list.Code != http.StatusOK {
		t.Fatalf("list: %d %s", list.Code, list.Body)
	}
	body := decodeBody(t, list)
	if n := len(body["items"].([]any)); n != 1 {
		t.Fatalf("items = %d, want 1", n)
	}
	topics := body["topics"].([]any)
	if len(topics) != 1 || topics[0] != "work-style" {
		t.Fatalf("topics = %+v", topics)
	}

	// Edit text and confirm a pending row through the same route.
	upd := do(t, srv, http.MethodPut, "/api/memory/"+id,
		`{"text":"Prefers weekly reports as a table.","status":"confirmed"}`)
	if upd.Code != http.StatusOK {
		t.Fatalf("update: %d %s", upd.Code, upd.Body)
	}
	if got := decodeBody(t, upd)["item"].(map[string]any)["text"]; got != "Prefers weekly reports as a table." {
		t.Fatalf("updated text = %v", got)
	}

	if del := do(t, srv, http.MethodDelete, "/api/memory/"+id, ""); del.Code != http.StatusOK {
		t.Fatalf("delete: %d %s", del.Code, del.Body)
	}
	if again := do(t, srv, http.MethodDelete, "/api/memory/"+id, ""); again.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d, want 404", again.Code)
	}
}

func TestMemoryIsProfileScoped(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{}, Config{})
	store := srv.memory.(*memory.MemoryStore)
	other, err := store.Create(context.Background(), memory.Item{
		Profile: "someone-else", Topic: "health", Text: "has a bad knee",
		Source: memory.SourceUser, Status: memory.StatusConfirmed,
	})
	if err != nil {
		t.Fatal(err)
	}

	list := decodeBody(t, do(t, srv, http.MethodGet, "/api/memory", ""))
	if n := len(list["items"].([]any)); n != 0 {
		t.Fatalf("other profile leaked into list: %+v", list["items"])
	}
	// An id from another profile is a 404, not a permission error: the API
	// does not confirm that a foreign row exists.
	if got := do(t, srv, http.MethodDelete, "/api/memory/"+other.ID, ""); got.Code != http.StatusNotFound {
		t.Fatalf("foreign delete = %d, want 404", got.Code)
	}
	if got := do(t, srv, http.MethodPut, "/api/memory/"+other.ID, `{"text":"x"}`); got.Code != http.StatusNotFound {
		t.Fatalf("foreign edit = %d, want 404", got.Code)
	}
	if it, err := store.Get(context.Background(), other.ID); err != nil || it.Text != "has a bad knee" {
		t.Fatalf("foreign item changed: %+v %v", it, err)
	}
}

func TestMemoryForgetAndPausedTopics(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{}, Config{})
	for _, text := range []string{"likes tea", "runs on Tuesdays"} {
		if rec := do(t, srv, http.MethodPost, "/api/memory", `{"topic":"health","text":"`+text+`"}`); rec.Code != http.StatusOK {
			t.Fatalf("create: %d %s", rec.Code, rec.Body)
		}
	}

	pause := do(t, srv, http.MethodPut, "/api/memory/topics/Health", `{"paused":true}`)
	if pause.Code != http.StatusOK {
		t.Fatalf("pause: %d %s", pause.Code, pause.Body)
	}
	body := decodeBody(t, do(t, srv, http.MethodGet, "/api/memory", ""))
	if paused := body["paused"].([]any); len(paused) != 1 || paused[0] != "health" {
		t.Fatalf("paused = %+v", body["paused"])
	}
	if n := len(body["items"].([]any)); n != 2 {
		t.Fatalf("pausing deleted items: %d left", n)
	}
	if got := srv.memoryPrompt(context.Background(), "default"); got != "" {
		t.Fatalf("paused-only memory was injected: %q", got)
	}

	forget := do(t, srv, http.MethodPost, "/api/memory/forget", "")
	if forget.Code != http.StatusOK {
		t.Fatalf("forget: %d %s", forget.Code, forget.Body)
	}
	if n := decodeBody(t, forget)["deleted"].(float64); n != 2 {
		t.Fatalf("deleted = %v, want 2", n)
	}
	// Pausing is a learning policy, not a memory: it survives a forget.
	body = decodeBody(t, do(t, srv, http.MethodGet, "/api/memory", ""))
	if n := len(body["items"].([]any)); n != 0 {
		t.Fatalf("items after forget = %d", n)
	}
	if paused := body["paused"].([]any); len(paused) != 1 {
		t.Fatalf("paused after forget = %+v", body["paused"])
	}
}

func TestMemoryValidationAndMethods(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{}, Config{})

	cases := []struct {
		name, method, path, body string
		want                     int
	}{
		{"empty text", http.MethodPost, "/api/memory", `{"text":"   "}`, http.StatusBadRequest},
		{"bad status", http.MethodGet, "/api/memory?status=maybe", "", http.StatusBadRequest},
		{"bad limit", http.MethodGet, "/api/memory?limit=-1", "", http.StatusBadRequest},
		{"pause without flag", http.MethodPut, "/api/memory/topics/work", `{}`, http.StatusBadRequest},
		{"bad update status", http.MethodPut, "/api/memory/abc", `{"status":"maybe"}`, http.StatusBadRequest},
		{"empty update", http.MethodPut, "/api/memory/abc", `{}`, http.StatusBadRequest},
		{"unknown route", http.MethodGet, "/api/memory/a/b", "", http.StatusNotFound},
		{"collection method", http.MethodPatch, "/api/memory", "", http.StatusMethodNotAllowed},
		{"id method", http.MethodGet, "/api/memory/abc", "", http.StatusMethodNotAllowed},
	}
	for _, tc := range cases {
		if got := do(t, srv, tc.method, tc.path, tc.body); got.Code != tc.want {
			t.Errorf("%s: %d, want %d (%s)", tc.name, got.Code, tc.want, got.Body)
		}
	}
}

func TestMemoryPromptInjectsOnlyConfirmedUnpaused(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{}, Config{})
	ctx := context.Background()
	store := srv.memory.(*memory.MemoryStore)
	mustCreate := func(it memory.Item) {
		it.Profile = "default"
		if _, err := store.Create(ctx, it); err != nil {
			t.Fatal(err)
		}
	}
	mustCreate(memory.Item{Topic: "style", Text: "Prefers tables for reports.", Status: memory.StatusConfirmed})
	mustCreate(memory.Item{Topic: "health", Text: "Training for a half marathon.", Status: memory.StatusConfirmed})
	mustCreate(memory.Item{Topic: "fact", Text: "Proposed, not confirmed.", Status: memory.StatusPending})
	if err := store.SetTopicPaused(ctx, "default", "health", true); err != nil {
		t.Fatal(err)
	}

	block := srv.memoryPrompt(ctx, "default")
	if !strings.Contains(block, "Prefers tables") {
		t.Fatalf("confirmed item missing: %q", block)
	}
	if strings.Contains(block, "half marathon") {
		t.Fatalf("paused topic injected: %q", block)
	}
	if strings.Contains(block, "Proposed") {
		t.Fatalf("pending item injected: %q", block)
	}
	if !strings.HasPrefix(block, "[Memory:") || !strings.HasSuffix(block, "[/Memory]") {
		t.Fatalf("block not delimited: %q", block)
	}
}
