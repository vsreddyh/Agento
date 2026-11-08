package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"agento/internal/teach"
)

// These tests pin the #314 taught-skills contract: drafts are born drafts,
// activation needs a recorded test, and foreign ids 404.

func teachServer(t *testing.T, runner TeachRunner) *Server {
	t.Helper()
	cfg := Config{}
	if runner != nil {
		cfg.TeachRunner = runner
	}
	return newTestServer(t, &fakeAgent{}, cfg)
}

func createDraft(t *testing.T, srv *Server, body string) string {
	t.Helper()
	rec := do(t, srv, http.MethodPost, "/api/skills", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	return decodeBody(t, rec)["skill"].(map[string]any)["id"].(string)
}

func TestTeachDraftLifecycle(t *testing.T) {
	srv := teachServer(t, nil)

	id := createDraft(t, srv, `{"name":"weekly","description":"Sends the report","body":"# Weekly\nSend it."}`)
	got := decodeBody(t, do(t, srv, http.MethodGet, "/api/skills", ""))
	if n := len(got["skills"].([]any)); n != 1 {
		t.Fatalf("skills = %d, want 1", n)
	}
	if st := got["skills"].([]any)[0].(map[string]any)["status"]; st != "draft" {
		t.Fatalf("status = %v, want draft", st)
	}

	// Activate without a test fails closed: review/test before run.
	if rec := do(t, srv, http.MethodPost, "/api/skills/"+id+"/activate", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("untested activate: %d, want 400 (%s)", rec.Code, rec.Body)
	}
	// Promote is gone with the scheduler: POST /api/skills/{id}/promote 404s.
	if rec := do(t, srv, http.MethodPost, "/api/skills/"+id+"/promote",
		`{"schedule":"0 9 * * 1"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("promote after removal: %d, want 404 (%s)", rec.Code, rec.Body)
	}

	// Suffix verbs share the id guard: an over-long id 404s on /test as
	// well as on the bare path, never reaching the store.
	if rec := do(t, srv, http.MethodPost, "/api/skills/"+strings.Repeat("x", 65)+"/test", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("long suffix id: %d, want 404", rec.Code)
	}
	// Bad names and empty bodies fail at create, not later.
	for _, tc := range []struct{ name, body string }{
		{"bad name", `{"name":"Weekly Report","description":"x","body":"y"}`},
		{"empty body", `{"name":"ok-name","description":"x","body":"   "}`},
		{"dup name", `{"name":"weekly","description":"x","body":"y"}`},
	} {
		if rec := do(t, srv, http.MethodPost, "/api/skills", tc.body); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d, want 400 (%s)", tc.name, rec.Code, rec.Body)
		}
	}
}

func TestTeachTestFireRecords(t *testing.T) {
	var gotPrompt string
	srv := teachServer(t, func(ctx context.Context, prompt, tag string) (string, error) {
		gotPrompt = prompt
		return "ran fine", nil
	})

	id := createDraft(t, srv, `{"name":"weekly","description":"Sends it","body":"Send the report."}`)
	rec := do(t, srv, http.MethodPost, "/api/skills/"+id+"/test", `{"input":"for January"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("test: %d %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec)
	if body["output"] != "ran fine" {
		t.Fatalf("output = %#v", body)
	}
	sk := body["skill"].(map[string]any)
	if sk["tested_at"] == float64(0) || sk["test_text"] != "ran fine" {
		t.Fatalf("record = %#v", sk)
	}
	if !strings.Contains(gotPrompt, "Send the report.") || !strings.Contains(gotPrompt, "for January") {
		t.Fatalf("prompt = %q", gotPrompt)
	}

	// A recorded test unlocks activation.
	rec = do(t, srv, http.MethodPost, "/api/skills/"+id+"/activate", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("activate: %d %s", rec.Code, rec.Body)
	}
	if st := decodeBody(t, rec)["skill"].(map[string]any)["status"]; st != "active" {
		t.Fatalf("status = %v, want active", st)
	}
}

func TestTeachTestWithoutRunner(t *testing.T) {
	srv := teachServer(t, nil)
	id := createDraft(t, srv, `{"name":"weekly","description":"x","body":"y"}`)
	if rec := do(t, srv, http.MethodPost, "/api/skills/"+id+"/test", ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("runnerless test: %d, want 502 (%s)", rec.Code, rec.Body)
	}
}

func TestTeachIsProfileScoped(t *testing.T) {
	srv := teachServer(t, func(ctx context.Context, prompt, tag string) (string, error) {
		return "ok", nil
	})
	store := srv.teach.(*teach.MemoryStore)
	other, err := store.Create(context.Background(), teach.Skill{
		Profile: "someone-else", Name: "weekly", Description: "x", Body: "y",
	})
	if err != nil {
		t.Fatal(err)
	}
	list := decodeBody(t, do(t, srv, http.MethodGet, "/api/skills", ""))
	if n := len(list["skills"].([]any)); n != 0 {
		t.Fatalf("other profile leaked: %#v", list["skills"])
	}
	if rec := do(t, srv, http.MethodDelete, "/api/skills/"+other.ID, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign delete = %d, want 404 (%s)", rec.Code, rec.Body)
	}
	if rec := do(t, srv, http.MethodPut, "/api/skills/"+other.ID, `{"body":"x"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign edit = %d, want 404 (%s)", rec.Code, rec.Body)
	}
	// The test path Gets the row before running: a foreign id 404s and the
	// model is never prompted.
	if rec := do(t, srv, http.MethodPost, "/api/skills/"+other.ID+"/test", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("foreign test = %d, want 404", rec.Code)
	}
}
