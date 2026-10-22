package cronjobs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type fakeBackend struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

func (f *fakeBackend) List(_ context.Context) ([]Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Job
	for _, j := range f.jobs {
		out = append(out, *j)
	}
	return out, nil
}

func (f *fakeBackend) Get(_ context.Context, id string) (Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		return Job{}, &StoreError{Msg: "unknown job " + id, NotFound: true}
	}
	return *j, nil
}

func (f *fakeBackend) RecordRun(_ context.Context, id string, run Run, fire int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.jobs[id]
	j.Runs = append(j.Runs, run)
	if len(j.Runs) > RunHistoryCap {
		j.Runs = j.Runs[len(j.Runs)-RunHistoryCap:]
	}
	j.LastFire = fire
	return nil
}

func replyServer(t *testing.T, text string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": text}}},
		})
	}))
}

func TestTickFiresDueJob(t *testing.T) {
	srv := replyServer(t, "nudge back")
	defer srv.Close()
	be := &fakeBackend{jobs: map[string]*Job{
		"a": {ID: "a", Prompt: "p", Schedule: "* * * * *"},
		"b": {ID: "b", Prompt: "p", Schedule: "* * * * *", Paused: true},
	}}
	bare := &BareClient{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTP: srv.Client()}
	w := &Worker{Store: be, Fire: bare.Fire}
	w.Tick(context.Background(), time.Now())
	deadline := time.Now().Add(5 * time.Second)
	for {
		be.mu.Lock()
		n := len(be.jobs["a"].Runs)
		be.mu.Unlock()
		if n > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.jobs["a"].Runs) != 1 || be.jobs["a"].Runs[0].Text != "nudge back" {
		t.Errorf("job a runs: %+v", be.jobs["a"].Runs)
	}
	if len(be.jobs["b"].Runs) != 0 {
		t.Errorf("paused job fired: %+v", be.jobs["b"].Runs)
	}
	if be.jobs["a"].LastFire <= 0 {
		t.Error("LastFire not advanced")
	}
}

func TestTickRecordsMiss(t *testing.T) {
	srv := replyServer(t, "late")
	defer srv.Close()
	// Fire instant seven minutes ago: past the catchup window, so the
	// tick records a miss instead of calling.
	be := &fakeBackend{jobs: map[string]*Job{
		"a": {ID: "a", Prompt: "p", Schedule: "0 11 * * *"},
	}}
	bare := &BareClient{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTP: srv.Client()}
	w := &Worker{Store: be, Fire: bare.Fire}
	now := time.Date(2026, time.October, 14, 11, 7, 0, 0, IST)
	w.Tick(context.Background(), now)
	be.mu.Lock()
	defer be.mu.Unlock()
	runs := be.jobs["a"].Runs
	if len(runs) != 1 || !runs[0].Missed {
		t.Fatalf("expected one missed run, got %+v", runs)
	}
	// LastFire advanced past the accounted instant: the next tick moves on.
	if be.jobs["a"].LastFire != time.Date(2026, time.October, 14, 11, 0, 0, 0, IST).UnixMilli() {
		t.Errorf("LastFire = %d", be.jobs["a"].LastFire)
	}
}

func TestFireNowManual(t *testing.T) {
	srv := replyServer(t, "manual back")
	defer srv.Close()
	be := &fakeBackend{jobs: map[string]*Job{
		"a": {ID: "a", Prompt: "p", Schedule: "0 0 1 1 *", Paused: true},
	}}
	bare := &BareClient{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTP: srv.Client()}
	w := &Worker{Store: be, Fire: bare.Fire}
	text, err := w.FireNow(context.Background(), "a")
	if err != nil || text != "manual back" {
		t.Fatalf("FireNow = %q, %v", text, err)
	}
	if _, err := w.FireNow(context.Background(), "nope"); err == nil {
		t.Error("unknown job should fail")
	}
}
