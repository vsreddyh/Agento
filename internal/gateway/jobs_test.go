package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"agento/internal/cronjobs"
)

var nextRunRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2} \d{2}:\d{2}$`)

type fakeJobs struct {
	mu   sync.Mutex
	jobs map[string]cronjobs.Job
	seq  int
}

func (f *fakeJobs) List(_ context.Context) ([]cronjobs.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []cronjobs.Job
	for _, j := range f.jobs {
		out = append(out, j)
	}
	return out, nil
}

func (f *fakeJobs) Create(_ context.Context, name, prompt, schedule, delivery string) (cronjobs.Job, error) {
	if strings.TrimSpace(prompt) == "" {
		return cronjobs.Job{}, &cronjobs.StoreError{Msg: "prompt is required"}
	}
	if _, err := cronjobs.Parse(schedule); err != nil {
		return cronjobs.Job{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	j := cronjobs.Job{ID: string(rune('a' + f.seq)), Name: name, Prompt: prompt, Schedule: schedule, Delivery: delivery}
	f.jobs[j.ID] = j
	return j, nil
}

func (f *fakeJobs) Update(_ context.Context, id string, name, prompt, schedule, delivery *string, paused *bool) (cronjobs.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return cronjobs.Job{}, &cronjobs.StoreError{Msg: "unknown job " + id, NotFound: true}
	}
	if schedule != nil {
		if _, err := cronjobs.Parse(*schedule); err != nil {
			return cronjobs.Job{}, &cronjobs.StoreError{Msg: "bad schedule: " + err.Error()}
		}
		j.Schedule = *schedule
	}
	if paused != nil {
		j.Paused = *paused
	}
	f.jobs[id] = j
	return j, nil
}

func (f *fakeJobs) Get(_ context.Context, id string) (cronjobs.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return cronjobs.Job{}, &cronjobs.StoreError{Msg: "unknown job " + id, NotFound: true}
	}
	return j, nil
}

func (f *fakeJobs) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.jobs[id]; !ok {
		return &cronjobs.StoreError{Msg: "unknown job " + id, NotFound: true}
	}
	delete(f.jobs, id)
	return nil
}

func (f *fakeJobs) RecordRun(_ context.Context, id string, run cronjobs.Run, fire int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[id]
	j.Runs = append(j.Runs, run)
	j.LastFire = fire
	f.jobs[id] = j
	return nil
}

func jobsServer(t *testing.T) (*Server, *fakeJobs) {
	t.Helper()
	fj := &fakeJobs{jobs: map[string]cronjobs.Job{}}
	fire := func(ctx context.Context, id string) (string, string, error) {
		if _, err := fj.Get(ctx, id); err != nil {
			return "", "", err
		}
		return "fired", "run-9", nil
	}
	srv := newTestServer(t, &fakeAgent{}, Config{CronJobs: fj, CronFire: fire})
	return srv, fj
}

func patchJob(t *testing.T, srv *Server, path, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPatch, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func deleteJob(t *testing.T, srv *Server, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodDelete, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestJobsUnconfigured(t *testing.T) {
	srv := newTestServer(t, &fakeAgent{}, Config{})
	rec := get(t, srv, "/default/api/jobs", "test-token")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestJobsListEmpty(t *testing.T) {
	srv, _ := jobsServer(t)
	rec := get(t, srv, "/default/api/jobs", "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Jobs []map[string]any `json:"jobs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Jobs == nil {
		t.Fatal("jobs should serialize as [], not null")
	}
}

func TestJobsUnprefixed(t *testing.T) {
	srv, _ := jobsServer(t)
	rec := get(t, srv, "/api/jobs", "test-token")
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d", rec.Code)
	}
}

func TestJobsCRUD(t *testing.T) {
	srv, _ := jobsServer(t)
	// Bad schedule rejected.
	rec := post(t, srv, "/default/api/jobs", "test-token", `{"prompt":"hi","schedule":"nope"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad schedule: got %d", rec.Code)
	}
	// Missing prompt rejected.
	rec = post(t, srv, "/default/api/jobs", "test-token", `{"schedule":"0 9 * * *"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing prompt: got %d", rec.Code)
	}
	// Create.
	rec = post(t, srv, "/default/api/jobs", "test-token",
		`{"name":"standup","prompt":"summarize","schedule":"0 9 * * *","deliver":"local"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: got %d: %s", rec.Code, rec.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id, _ := created["id"].(string)
	if id == "" || created["paused"] != false {
		t.Fatalf("bad create shape: %v", created)
	}
	if nextRun, _ := created["next_run"].(string); !nextRunRe.MatchString(nextRun) {
		t.Fatalf("next_run not an IST datetime: %v", created)
	}
	if _, ok := created["runs"].([]any); !ok {
		t.Fatalf("runs missing: %v", created)
	}
	// Pause + resume.
	rec = post(t, srv, "/default/api/jobs/"+id+"/pause", "test-token", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("pause: got %d", rec.Code)
	}
	var paused map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &paused)
	if paused["paused"] != true {
		t.Fatalf("not paused: %v", paused)
	}
	rec = post(t, srv, "/default/api/jobs/"+id+"/resume", "test-token", `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("resume: got %d", rec.Code)
	}
	// Manual run returns the fired text plus the run id for deep-linking.
	rec = post(t, srv, "/default/api/jobs/"+id+"/run", "test-token", `{}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fired") {
		t.Fatalf("run: got %d: %s", rec.Code, rec.Body.String())
	}
	var ran map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &ran)
	if ran["run_id"] != "run-9" {
		t.Fatalf("run_id missing: %v", ran)
	}
	// Unknown id 404s, on run and on delete.
	rec = post(t, srv, "/default/api/jobs/zzz/run", "test-token", `{}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("run unknown: got %d", rec.Code)
	}
	if rec := deleteJob(t, srv, "/default/api/jobs/zzz", "test-token"); rec.Code != http.StatusNotFound {
		t.Fatalf("delete unknown: got %d", rec.Code)
	}
	// Delete then list empty.
	if rec := deleteJob(t, srv, "/default/api/jobs/"+id, "test-token"); rec.Code != http.StatusOK {
		t.Fatalf("delete: got %d", rec.Code)
	}
}

func TestJobsPatchValidation(t *testing.T) {
	srv, fj := jobsServer(t)
	fj.jobs["q"] = cronjobs.Job{ID: "q", Prompt: "p", Schedule: "0 9 * * *"}
	rec := patchJob(t, srv, "/default/api/jobs/q", "test-token", `{"schedule":"bad"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad schedule patch: got %d", rec.Code)
	}
}
