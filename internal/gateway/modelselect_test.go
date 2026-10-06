package gateway

// The gateway applies the client's provider/model selection per request
// (#251). Before this the pickers were decorative: Pi ran its startup model
// no matter what was asked, and the response echoed the running model while
// the app stamped the requested one, so the substitution was invisible end
// to end. These tests pin the switch and its boundaries through the HTTP
// contract, on a fake whose inventory holds two models on one provider.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"agento/internal/pi"
)

func switchedModel(t *testing.T, agent *fakeAgent) string {
	t.Helper()
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.modelID
}

func TestRequestedModelIsApplied(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{{Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"provider":"opencode-go","model":"muse-spark-1.3-contributor",`+
			`"model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if !agent.sawCommand("set_model") {
		t.Fatal("the requested model was never applied to the agent")
	}
	if got := switchedModel(t, agent); got != "muse-spark-1.3-contributor" {
		t.Fatalf("agent runs %q after the switch", got)
	}
	var body completion
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not a completion object: %v (%s)", err, rec.Body)
	}
	if body.Model != "muse-spark-1.3-contributor" {
		t.Fatalf("response echoes %q, not the applied model", body.Model)
	}
}

func TestSameModelDoesNotSwitch(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{{Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	// The fake runs mimo-v2.6-flash: asking for it must not re-apply it.
	rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if agent.sawCommand("set_model") {
		t.Fatal("requesting the running model still called set_model")
	}
}

func TestBlankModelKeepsRunning(t *testing.T) {
	agent := newFakeAgent()
	agent.events = []pi.Record{{Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	// Legacy callers name no model: behaviour is exactly today's.
	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if agent.sawCommand("set_model") {
		t.Fatal("a model-less request switched the model")
	}
}

func TestUnknownModelIs400(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model":"no-such-model","model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400. body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "no-such-model") {
		t.Fatalf("the 400 does not name the model: %s", rec.Body)
	}
	if agent.sawCommand("prompt") || agent.sawCommand("set_thinking_level") {
		t.Fatal("a rejected selection still reached the agent")
	}
}

func TestUnknownProviderIs400(t *testing.T) {
	agent := newFakeAgent()
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"provider":"no-such-provider","model":"mimo-v2.6-flash",`+
			`"model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400. body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "no-such-provider") {
		t.Fatalf("the 400 does not name the provider: %s", rec.Body)
	}
	if agent.sawCommand("prompt") {
		t.Fatal("a rejected selection still reached the agent")
	}
}

func TestEffortIsValidatedAgainstTheAppliedModel(t *testing.T) {
	// xhigh is outside the default ladder but inside muse-spark's: asking
	// for muse-spark + xhigh must pass, because validation reads the NEW
	// model's levels. Validating against the running (mimo) ladder would
	// 400 this — the wrong-model refusal #251 was written about.
	agent := newFakeAgent()
	agent.levelsFor = map[string][]string{
		"mimo-v2.6-flash":            {"off", "minimal"},
		"muse-spark-1.3-contributor": {"off", "minimal", "low", "medium", "high", "xhigh", "max"},
	}
	agent.events = []pi.Record{{Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model":"muse-spark-1.3-contributor",`+
			`"model_options":{"reasoning_effort":"xhigh"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("xhigh on muse-spark: got %d, want 200. body=%s", rec.Code, rec.Body)
	}

	// And the reverse: a level the new model lacks is refused with the NEW
	// model's list, not the old one's.
	agent2 := newFakeAgent()
	agent2.levelsFor = map[string][]string{
		"mimo-v2.6-flash":            {"off", "minimal", "low", "medium", "high"},
		"muse-spark-1.3-contributor": {"off", "minimal"},
	}
	agent2.events = []pi.Record{{Type: pi.TypeAgentSettled}}
	srv2 := newTestServer(t, agent2, Config{})

	rec = post(t, srv2, "/v1/chat/completions", "test-token",
		`{"model":"muse-spark-1.3-contributor",`+
			`"model_options":{"reasoning_effort":"high"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("high on minimal-only muse-spark: got %d, want 400. body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "off, minimal") {
		t.Fatalf("the 400 does not list the applied model's levels: %s", rec.Body)
	}
}

func TestPiRefusalMidSwitchReadsAsUnknown(t *testing.T) {
	// The inventory listed the model but Pi refused it anyway (registry moved
	// between the two calls): Pi names the model in that refusal, so it reads
	// as unknown rather than wedged.
	agent := newFakeAgent()
	agent.failCommands["set_model"] = errors.New("Model not found: opencode-go/gone")
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model":"muse-spark-1.3-contributor","model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400. body=%s", rec.Code, rec.Body)
	}
	if agent.sawCommand("prompt") {
		t.Fatal("a refused switch still prompted")
	}
}

func TestPiFailureMidSwitchIs502(t *testing.T) {
	// A refusal that does NOT name a model is a broken agent, not a bad value.
	agent := newFakeAgent()
	agent.failCommands["set_model"] = errors.New("connection reset")
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model":"muse-spark-1.3-contributor","model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502. body=%s", rec.Code, rec.Body)
	}
}

func TestAmbiguousModelNamesItsProviders(t *testing.T) {
	// The same id under two providers with no provider named: the 400 must
	// carry the "name one" hint, not a bare unknown-model.
	agent := newFakeAgent()
	agent.models = []fakeModel{
		{id: "shared-model", provider: "provider-a"},
		{id: "shared-model", provider: "provider-b"},
	}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model":"shared-model","model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400. body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "name one") {
		t.Fatalf("the 400 dropped the disambiguation hint: %s", rec.Body)
	}
	if agent.sawCommand("set_model") {
		t.Fatal("an ambiguous selection still switched the model")
	}
}

func TestModelSubstringInUnrelatedErrorIs502(t *testing.T) {
	// "remodel" contains "model" but names no model: matching it would 400 a
	// wedged agent instead of 502ing it.
	agent := newFakeAgent()
	agent.failCommands["set_model"] = errors.New("remodel failed: out of memory")
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model":"muse-spark-1.3-contributor","model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502. body=%s", rec.Code, rec.Body)
	}
}
