package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"agento/internal/cronjobs"
	"agento/internal/teach"
)

// These tests pin the #314 taught-skills contract: drafts are born drafts,
// activation needs a recorded test, foreign ids 404, and only tested active
// skills promote to cron jobs.

func teachServer(t *testing.T, runner TeachRunner, jobs JobStore) *Server {
	t.Helper()
	cfg := Config{}
	if runner != nil {
		cfg.TeachRunner = runner
	}
	if jobs != nil {
		cfg.CronJobs = jobs
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
	srv := teachServer(t, nil, nil)

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
	// Promote without test+active fails the same way.
	if rec := do(t, srv, http.MethodPost, "/api/skills/"+id+"/promote",
		`{"schedule":"0 9 * * 1"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unreviewed promote: %d, want 400 (%s)", rec.Code, rec.Body)
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
	}, nil)

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
	srv := teachServer(t, nil, nil)
	id := createDraft(t, srv, `{"name":"weekly","description":"x","body":"y"}`)
	if rec := do(t, srv, http.MethodPost, "/api/skills/"+id+"/test", ""); rec.Code != http.StatusBadGateway {
		t.Fatalf("runnerless test: %d, want 502 (%s)", rec.Code, rec.Body)
	}
}

func TestTeachPromote(t *testing.T) {
	jobs := &fakeJobs{jobs: map[string]cronjobs.Job{}}
	srv := teachServer(t, func(ctx context.Context, prompt, tag string) (string, error) {
		return "ok", nil
	}, jobs)

	id := createDraft(t, srv, `{"name":"weekly","description":"Sends it","body":"Send the report."}`)
	do(t, srv, http.MethodPost, "/api/skills/"+id+"/test", "")
	do(t, srv, http.MethodPost, "/api/skills/"+id+"/activate", "")

	rec := do(t, srv, http.MethodPost, "/api/skills/"+id+"/promote",
		`{"schedule":"0 9 * * 1","delivery":"chat"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("promote: %d %s", rec.Code, rec.Body)
	}
	job := decodeBody(t, rec)["job"].(map[string]any)
	if job["schedule"] != "0 9 * * 1" {
		t.Fatalf("job = %#v", job)
	}
	jobID := decodeBody(t, rec)["job"].(map[string]any)["id"].(string)
	jobs.mu.Lock()
	stored, ok := jobs.jobs[jobID]
	jobs.mu.Unlock()
	if !ok {
		t.Fatalf("promoted job %s not stored", jobID)
	}
	if !strings.Contains(stored.Prompt, "Send the report.") || !strings.Contains(stored.Prompt, "weekly") {
		t.Fatalf("job prompt = %q", stored.Prompt)
	}

	// A bad schedule fails at promote, not at 9am Monday.
	if rec := do(t, srv, http.MethodPost, "/api/skills/"+id+"/promote",
		`{"schedule":"whenever"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad schedule: %d, want 400 (%s)", rec.Code, rec.Body)
	}
}

func TestTeachIsProfileScoped(t *testing.T) {
	srv := teachServer(t, func(ctx context.Context, prompt, tag string) (string, error) {
		return "ok", nil
	}, nil)
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
