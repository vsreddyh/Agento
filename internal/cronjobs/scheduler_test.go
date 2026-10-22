package cronjobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

func TestSchedulerPromptLabelsJob(t *testing.T) {
	got := SchedulerPrompt("daily scan", "find new items")
	if !strings.HasPrefix(got, "From Scheduler daily scan: find new items") {
		t.Errorf("missing label prefix: %q", got)
	}
	if !strings.Contains(got, "read-only tools only") {
		t.Errorf("missing unattended policy: %q", got)
	}
}

func TestSchedulerPromptEmptyName(t *testing.T) {
	got := SchedulerPrompt("  ", "do it")
	if !strings.HasPrefix(got, "From Scheduler unnamed job: do it") {
		t.Errorf("empty name should fall back: %q", got)
	}
}

func TestBareFireSendsLabelledPrompt(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody = body
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "bare back"}}},
		})
	}))
	defer srv.Close()
	bare := &BareClient{APIKey: "k", Model: "m", BaseURL: srv.URL, HTTP: srv.Client()}
	res, err := bare.Fire(context.Background(), Job{ID: "j1", Name: "scan", Prompt: "find things"})
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if res.Text != "bare back" {
		t.Errorf("Text = %q", res.Text)
	}
	if res.RunID != "" {
		t.Errorf("bare fire has no run record, RunID = %q", res.RunID)
	}
	var req chatRequest
	if err := json.Unmarshal(gotBody, &req); err != nil {
		t.Fatalf("request is not JSON: %v", err)
	}
	if len(req.Messages) != 1 || !strings.HasPrefix(req.Messages[0].Content, "From Scheduler scan: find things") {
		t.Errorf("prompt not labelled: %+v", req.Messages)
	}
}

func TestTickSkipsBusyAgent(t *testing.T) {
	be := &fakeBackend{jobs: map[string]*Job{
		"a": {ID: "a", Name: "scan", Prompt: "p", Schedule: "* * * * *"},
	}}
	calls := atomic.Int32{}
	w := &Worker{Store: be, Fire: func(_ context.Context, _ Job) (FireResult, error) {
		calls.Add(1)
		return FireResult{}, ErrBusy
	}}
	w.Tick(context.Background(), time.Now())
	deadline := time.Now().Add(2 * time.Second)
	for {
		if calls.Load() > 0 {
			// fireAsync runs in a goroutine; give it a moment, then
			// assert the skip recorded nothing and advanced nothing.
			time.Sleep(100 * time.Millisecond)
			be.mu.Lock()
			defer be.mu.Unlock()
			if len(be.jobs["a"].Runs) != 0 {
				t.Fatalf("busy skip recorded runs: %+v", be.jobs["a"].Runs)
			}
			if be.jobs["a"].LastFire != 0 {
				t.Fatalf("busy skip advanced LastFire to %d", be.jobs["a"].LastFire)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("fire never ran")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTickRecordsProvenance(t *testing.T) {
	be := &fakeBackend{jobs: map[string]*Job{
		"a": {ID: "a", Name: "scan", Prompt: "p", Schedule: "* * * * *"},
	}}
	w := &Worker{Store: be, Fire: func(_ context.Context, j Job) (FireResult, error) {
		if j.ID != "a" {
			t.Errorf("fired job id = %q", j.ID)
		}
		return FireResult{Text: "answer", RunID: "run-1"}, nil
	}}
	w.Tick(context.Background(), time.Now())
	deadline := time.Now().Add(2 * time.Second)
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
	if len(be.jobs["a"].Runs) != 1 {
		t.Fatalf("runs: %+v", be.jobs["a"].Runs)
	}
	r := be.jobs["a"].Runs[0]
	if r.Source != RunSourceScheduler || r.JobName != "scan" || r.RunID != "run-1" || r.Text != "answer" {
		t.Errorf("provenance missing: %+v", r)
	}
}

func TestFireNowBusyRecordsNothing(t *testing.T) {
	be := &fakeBackend{jobs: map[string]*Job{
		"a": {ID: "a", Name: "scan", Prompt: "p", Schedule: "0 0 1 1 *"},
	}}
	w := &Worker{Store: be, Fire: func(_ context.Context, _ Job) (FireResult, error) {
		return FireResult{Text: "x"}, nil
	}}
	if !w.claim("a") {
		t.Fatal("claim failed")
	}
	defer w.release("a")
	if _, err := w.FireNow(context.Background(), "a"); err == nil {
		t.Fatal("in-flight manual run should fail")
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.jobs["a"].Runs) != 0 {
		t.Errorf("busy manual run recorded: %+v", be.jobs["a"].Runs)
	}
}

func TestFireNowBusyAgentSkipsRecord(t *testing.T) {
	be := &fakeBackend{jobs: map[string]*Job{
		"a": {ID: "a", Name: "scan", Prompt: "p", Schedule: "0 0 1 1 *"},
	}}
	wrapped := errors.New("wrapped")
	w := &Worker{Store: be, Fire: func(_ context.Context, _ Job) (FireResult, error) {
		return FireResult{}, errors.Join(wrapped, ErrBusy)
	}}
	if _, err := w.FireNow(context.Background(), "a"); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	be.mu.Lock()
	defer be.mu.Unlock()
	if len(be.jobs["a"].Runs) != 0 {
		t.Errorf("busy manual run recorded: %+v", be.jobs["a"].Runs)
	}
}

func TestRunProvenanceRoundTrip(t *testing.T) {
	j := Job{ID: "a", Name: "scan", Runs: []Run{
		{At: 1, Text: "hi", Source: RunSourceScheduler, JobName: "scan", RunID: "r1"},
		{At: 2, Text: "legacy"},
	}}
	// Through BSON like the real store: fromDoc reads the decoded shapes
	// (bson.A for the array), not toDoc's Go slices.
	raw, err := bson.Marshal(toDoc(j))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m bson.M
	if err := bson.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := fromDoc(m)
	if len(got.Runs) != 2 {
		t.Fatalf("runs: %+v", got.Runs)
	}
	if got.Runs[0].Source != RunSourceScheduler || got.Runs[0].JobName != "scan" || got.Runs[0].RunID != "r1" {
		t.Errorf("provenance lost: %+v", got.Runs[0])
	}
	if got.Runs[1].Source != "" || got.Runs[1].RunID != "" {
		t.Errorf("legacy row gained provenance: %+v", got.Runs[1])
	}
}
