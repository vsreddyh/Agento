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

func TestProviderModelPairIsValidated(t *testing.T) {
	// Both halves exist but never together: provider-a never offered
	// muse-spark, so the 400 must name where it actually lives rather than
	// the composed pair Pi never advertised.
	agent := newFakeAgent()
	agent.models = []fakeModel{
		{id: "mimo-v2.6-flash", provider: "provider-a"},
		{id: "muse-spark-1.3-contributor", provider: "provider-b"},
	}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"provider":"provider-a","model":"muse-spark-1.3-contributor",`+
			`"model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400. body=%s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "provider-b") {
		t.Fatalf("the 400 does not name the model's real provider: %s", rec.Body)
	}
	if agent.sawCommand("set_model") {
		t.Fatal("a mismatched pair still switched the model")
	}
}

func TestRefusedEffortRevertsTheSwitch(t *testing.T) {
	// The switch applies before effort is judged, so a refused effort must
	// put the previous model back: otherwise one bad request silently
	// repoints every following turn.
	agent := newFakeAgent()
	agent.events = []pi.Record{{Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model":"muse-spark-1.3-contributor",`+
			`"model_options":{"reasoning_effort":"ultra"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400. body=%s", rec.Code, rec.Body)
	}
	if got := switchedModel(t, agent); got != "mimo-v2.6-flash" {
		t.Fatalf("refused effort left the agent on %q instead of reverting", got)
	}
}

func TestDashedModelSpellingFindsDottedInventoryID(t *testing.T) {
	// The app normalizes dots/dashes/underscores for lookup; the gateway must
	// match the same way — while sending Pi the inventory's canonical
	// spelling, never the normalized guess.
	agent := newFakeAgent()
	agent.events = []pi.Record{{Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model":"muse-spark-1-3-contributor","model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if got := switchedModel(t, agent); got != "muse-spark-1.3-contributor" {
		t.Fatalf("Pi received %q, want the canonical inventory spelling", got)
	}
}

func TestProviderAloneIsValidated(t *testing.T) {
	// A provider with no model selects nothing — but an unknown one is still
	// refused rather than silently ignored.
	agent := newFakeAgent()
	agent.events = []pi.Record{{Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"provider":"no-such-provider","model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400. body=%s", rec.Code, rec.Body)
	}

	rec = post(t, srv, "/v1/chat/completions", "test-token",
		`{"provider":"opencode-go","model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("known provider alone: got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if agent.sawCommand("set_model") {
		t.Fatal("a provider-only request switched the model")
	}
}

func TestThinkingLevelFailureRevertsTheSwitch(t *testing.T) {
	// Only the effort-400 revert is not enough: any pre-prompt failure after
	// a switch must put the previous model back, or one failed turn repoints
	// every following blank-model turn.
	agent := newFakeAgent()
	agent.failCommands["set_thinking_level"] = errors.New("agent wedged")
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model":"muse-spark-1.3-contributor","model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502. body=%s", rec.Code, rec.Body)
	}
	if got := switchedModel(t, agent); got != "mimo-v2.6-flash" {
		t.Fatalf("failed turn left the agent on %q instead of reverting", got)
	}
}

func TestUnnameablePreviousModelStaysSwitched(t *testing.T) {
	// A running model Pi reports without a provider cannot be composed back
	// into a set_model id, so there is nothing to revert to: the turn still
	// 400s, and the switch stands, documented rather than half-reverted.
	agent := newFakeAgent()
	agent.modelProvider = ""
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model":"muse-spark-1.3-contributor",`+
			`"model_options":{"reasoning_effort":"ultra"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400. body=%s", rec.Code, rec.Body)
	}
	if got := switchedModel(t, agent); got != "muse-spark-1.3-contributor" {
		t.Fatalf("agent runs %q; without a nameable previous model the switch stands", got)
	}
}

func TestProviderMatchingIgnoresCase(t *testing.T) {
	// Provider slugs compare case-insensitively, like model ids do through
	// normalization: {"provider":"OpenCode-Go"} must not 400.
	agent := newFakeAgent()
	agent.events = []pi.Record{{Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"provider":"OpenCode-Go","model":"muse-spark-1.3-contributor",`+
			`"model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if got := switchedModel(t, agent); got != "muse-spark-1.3-contributor" {
		t.Fatalf("agent runs %q after the switch", got)
	}
}

func TestQualifiedModelClaimResolves(t *testing.T) {
	// A provider-qualified model claim splits the way the app's own catalog
	// lookup splits it — while Pi receives the canonical spelling.
	agent := newFakeAgent()
	agent.events = []pi.Record{{Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model":"opencode-go/muse-spark-1.3-contributor",`+
			`"model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if got := switchedModel(t, agent); got != "muse-spark-1.3-contributor" {
		t.Fatalf("Pi received %q, want the canonical inventory spelling", got)
	}
}

func TestMixedCaseInventorySlugStillSelects(t *testing.T) {
	// Slugs are folded at inventory build, so a mixed-case row Pi returns
	// still matches a lowercase request — instead of 400ing every selection.
	agent := newFakeAgent()
	agent.models = []fakeModel{
		{id: "mimo-v2.6-flash", provider: "OpenCode-Go"},
		{id: "muse-spark-1.3-contributor", provider: "OpenCode-Go"},
	}
	agent.events = []pi.Record{{Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"provider":"opencode-go","model":"muse-spark-1.3-contributor",`+
			`"model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if got := switchedModel(t, agent); got != "muse-spark-1.3-contributor" {
		t.Fatalf("agent runs %q after the switch", got)
	}
}

func TestQualifiedClaimPrefixFoldsCase(t *testing.T) {
	// The strip must fold like provider does: an explicit lowercase
	// provider with a mixed-case qualified claim still resolves.
	agent := newFakeAgent()
	agent.events = []pi.Record{{Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"provider":"opencode-go","model":"OpenCode-Go/muse-spark-1.3-contributor",`+
			`"model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if got := switchedModel(t, agent); got != "muse-spark-1.3-contributor" {
		t.Fatalf("agent runs %q after the switch", got)
	}
}

func TestPromptDeliveryFailureRevertsTheSwitch(t *testing.T) {
	// Streaming path: the prompt never reached the agent, so the turn never
	// executed — the switch must not stand.
	agent := newFakeAgent()
	agent.failCommands["prompt"] = errors.New("agent unreachable")
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model":"muse-spark-1.3-contributor","model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	if !strings.Contains(rec.Body.String(), "could not deliver the prompt") {
		t.Fatalf("expected the delivery error frame, got %d: %s", rec.Code, rec.Body)
	}
	if got := switchedModel(t, agent); got != "mimo-v2.6-flash" {
		t.Fatalf("undelivered turn left the agent on %q instead of reverting", got)
	}
}

func TestBufferedPromptDeliveryFailureRevertsTheSwitch(t *testing.T) {
	// Same rule on the buffered path, which carries its own Send call.
	agent := newFakeAgent()
	agent.failCommands["prompt"] = errors.New("agent unreachable")
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"model":"muse-spark-1.3-contributor","model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}],"stream":false}`)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502. body=%s", rec.Code, rec.Body)
	}
	if got := switchedModel(t, agent); got != "mimo-v2.6-flash" {
		t.Fatalf("undelivered turn left the agent on %q instead of reverting", got)
	}
}

func TestMixedCaseRunningProviderIsNoSwitch(t *testing.T) {
	// The fast-path comparison folds the running side too: a mixed-case slug
	// Pi reports must equal the folded target, not force a redundant switch
	// every turn.
	agent := newFakeAgent()
	agent.modelProvider = "OpenCode-Go"
	agent.events = []pi.Record{{Type: pi.TypeAgentSettled}}
	srv := newTestServer(t, agent, Config{})

	rec := post(t, srv, "/v1/chat/completions", "test-token",
		`{"provider":"opencode-go","model":"mimo-v2.6-flash",`+
			`"model_options":{"reasoning_effort":"low"},`+
			`"messages":[{"role":"user","content":"hi"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200. body=%s", rec.Code, rec.Body)
	}
	if agent.sawCommand("set_model") {
		t.Fatal("requesting the running model under a differently-cased provider switched")
	}
}
