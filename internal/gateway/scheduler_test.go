package gateway

import (
	"context"
	"errors"
	"strings"
	"testing"

	"agento/internal/cronjobs"
	"agento/internal/pi"
)

// newSchedulerServer builds a server with only the named profiles, so tests
// can pin whether fires prefer "chat" or fall back to the default.
func newSchedulerServer(t *testing.T, agents map[string]*fakeAgent) *Server {
	t.Helper()
	all := make(map[string]Agent, len(agents))
	def := "default"
	if _, ok := agents[def]; !ok {
		for name := range agents {
			def = name
			break
		}
	}
	for name, a := range agents {
		all[name] = a
	}
	srv, err := New(Config{Password: "test-token", DefaultProfile: def}, all)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return srv
}

func TestFireSchedulerJobRunsChatTurn(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{textDelta(t, "scan says hi"), {Type: pi.TypeAgentSettled}}
	srv := newSchedulerServer(t, map[string]*fakeAgent{"chat": agent})

	text, runID, err := srv.FireSchedulerJob(context.Background(),
		cronjobs.Job{ID: "job-1", Name: "daily scan", Prompt: "find new items"})
	if err != nil {
		t.Fatalf("FireSchedulerJob: %v", err)
	}
	if text != "scan says hi" {
		t.Errorf("text = %q", text)
	}
	if runID == "" {
		t.Fatal("run id is empty: the fire left no Runs record to correlate")
	}

	agent.mu.Lock()
	prompts := append([]string(nil), agent.prompts...)
	agent.mu.Unlock()
	if len(prompts) != 1 {
		t.Fatalf("prompts = %d, want 1", len(prompts))
	}
	if !strings.HasPrefix(prompts[0], "From Scheduler daily scan: find new items") {
		t.Errorf("fire not labelled by job name: %q", prompts[0])
	}
	if !strings.Contains(prompts[0], "read-only tools only") {
		t.Errorf("fire missing unattended policy: %q", prompts[0])
	}

	// The run is retrievable and lives in the recurring scheduler thread on
	// the chat profile — that is what makes Scheduler history and the Runs
	// list correlate.
	run, err := srv.runs.store.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("run %s not stored: %v", runID, err)
	}
	if run.ConversationID != cronjobs.SchedulerConversationID {
		t.Errorf("conversation = %q, want %q", run.ConversationID, cronjobs.SchedulerConversationID)
	}
	if run.Profile != "chat" {
		t.Errorf("profile = %q, want chat", run.Profile)
	}

	// The scheduler conversation resumes: the session store bound it.
	h := srv.handlers["chat"]
	if _, ok := h.sessions.Lookup(cronjobs.SchedulerConversationID); !ok {
		t.Error("scheduler conversation not bound in the session store")
	}
}

func TestFireSchedulerJobPrefersChat(t *testing.T) {
	chat := newFakeAgent()
	chat.events = []pi.Record{textDelta(t, "chat back"), {Type: pi.TypeAgentSettled}}
	def := newFakeAgent()
	srv := newSchedulerServer(t, map[string]*fakeAgent{"chat": chat, "default": def})

	text, _, err := srv.FireSchedulerJob(context.Background(),
		cronjobs.Job{ID: "job-1", Name: "scan", Prompt: "p"})
	if err != nil {
		t.Fatalf("FireSchedulerJob: %v", err)
	}
	if text != "chat back" {
		t.Errorf("text = %q, want the chat profile's reply", text)
	}
	if n := def.commandCount("prompt"); n != 0 {
		t.Errorf("default profile prompted %d times, want 0", n)
	}
}

func TestFireSchedulerJobBusyIsSkip(t *testing.T) {
	agent := newFakeAgent()
	srv := newSchedulerServer(t, map[string]*fakeAgent{"chat": agent})

	h := srv.handlers["chat"]
	h.turnMu.Lock()
	defer h.turnMu.Unlock()
	if _, _, err := srv.FireSchedulerJob(context.Background(),
		cronjobs.Job{ID: "job-1", Name: "scan", Prompt: "p"}); !errors.Is(err, cronjobs.ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if n := agent.commandCount("prompt"); n != 0 {
		t.Errorf("busy fire prompted the agent %d times", n)
	}
}

func TestFireSchedulerJobFallsBackToDefault(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{textDelta(t, "default back"), {Type: pi.TypeAgentSettled}}
	srv := newSchedulerServer(t, map[string]*fakeAgent{"default": agent})

	text, runID, err := srv.FireSchedulerJob(context.Background(),
		cronjobs.Job{ID: "job-1", Name: "scan", Prompt: "p"})
	if err != nil {
		t.Fatalf("FireSchedulerJob: %v", err)
	}
	if text != "default back" || runID == "" {
		t.Errorf("text = %q, runID = %q", text, runID)
	}
	run, err := srv.runs.store.Get(context.Background(), runID)
	if err != nil {
		t.Fatalf("run not stored: %v", err)
	}
	if run.Profile != "default" {
		t.Errorf("profile = %q, want default", run.Profile)
	}
}
