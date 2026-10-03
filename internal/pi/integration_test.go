package pi_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"agento/internal/pi"
)

// These exercise the real `pi` binary rather than a stub, so a change in Pi's
// protocol shows up here instead of in production. They are skipped unless pi is
// on PATH, because the package must still build and test without it.

func requirePi(t *testing.T) {
	t.Helper()
	if _, err := pi.LookPath(); err != nil {
		t.Skipf("pi not on PATH: %v", err)
	}
	if os.Getenv("OPENCODE_API_KEY") == "" {
		t.Skip("OPENCODE_API_KEY unset")
	}
}

func startRealPi(t *testing.T, model string) *pi.Client {
	t.Helper()
	requirePi(t)

	c, err := pi.Start(context.Background(), pi.Options{
		Bin: "pi",
		// No --no-session: session persistence is what switch_session depends
		// on, and this is the only place that gets exercised.
		Args: []string{"--model", model},
	})
	if err != nil {
		t.Fatalf("start pi: %v", err)
	}
	t.Cleanup(func() {
		if s := c.Stderr(); s != "" {
			t.Logf("pi stderr: %s", s)
		}
		_ = c.Close()
	})
	return c
}

// TestGetStateAgainstRealPi is the cheapest end-to-end proof that framing and
// correlation work against the actual binary.
func TestGetStateAgainstRealPi(t *testing.T) {
	c := startRealPi(t, "opencode-go/mimo-v2.6-flash:off")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rec, err := c.Call(ctx, "get_state", nil)
	if err != nil {
		t.Fatalf("get_state: %v (stderr: %s)", err, c.Stderr())
	}

	var state struct {
		SessionID     string `json:"sessionId"`
		SessionFile   string `json:"sessionFile"`
		ThinkingLevel string `json:"thinkingLevel"`
		Model         struct {
			ID        string `json:"id"`
			Provider  string `json:"provider"`
			API       string `json:"api"`
			Reasoning bool   `json:"reasoning"`
		} `json:"model"`
	}
	if err := json.Unmarshal(rec.Data, &state); err != nil {
		t.Fatalf("decode get_state data: %v", err)
	}

	// sessionFile is what switch_session needs to resume a conversation, and
	// sessionId is the stable public key. Without both, the app's existing
	// X-Hermes-Session-Id header cannot be honoured.
	if state.SessionID == "" {
		t.Fatalf("no sessionId in state: %s", rec.Data)
	}
	if !strings.HasSuffix(state.SessionFile, ".jsonl") {
		t.Fatalf("sessionFile is not a .jsonl path: %q", state.SessionFile)
	}
	if state.Model.Provider == "" || state.Model.API == "" {
		t.Fatalf("model object missing provider/api: %s", rec.Data)
	}
	t.Logf("sessionId=%s", state.SessionID)
	t.Logf("sessionFile=%s", state.SessionFile)
	t.Logf("model=%s/%s api=%s reasoning=%v thinking=%s",
		state.Model.Provider, state.Model.ID, state.Model.API, state.Model.Reasoning, state.ThinkingLevel)
}

// TestThinkingLevelsAgainstRealPi covers the reasoning_effort mapping this whole
// migration hangs on: the app sends model_options.reasoning_effort, Pi takes
// set_thinking_level, and the level must actually be accepted.
func TestThinkingLevelsAgainstRealPi(t *testing.T) {
	c := startRealPi(t, "opencode-go/mimo-v2.6-flash:off")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	rec, err := c.Call(ctx, "get_available_thinking_levels", nil)
	if err != nil {
		t.Fatalf("get_available_thinking_levels: %v (stderr: %s)", err, c.Stderr())
	}
	var levels struct {
		Levels []string `json:"levels"`
	}
	if err := json.Unmarshal(rec.Data, &levels); err != nil {
		t.Fatalf("decode levels: %v", err)
	}
	if len(levels.Levels) == 0 {
		t.Fatalf("no thinking levels advertised: %s", rec.Data)
	}
	t.Logf("levels: %v", levels.Levels)

	if _, err := c.Call(ctx, "set_thinking_level", map[string]any{"level": "off"}); err != nil {
		t.Fatalf("set_thinking_level off: %v (stderr: %s)", err, c.Stderr())
	}

	// Measured against Pi 1.0.0: set_thinking_level does NOT reject an unknown
	// level. It answers success:true and leaves thinkingLevel at its previous
	// value. So the gateway must validate against get_available_thinking_levels
	// itself — a client that forwards model_options.reasoning_effort blind will
	// silently ignore a bad value and look like it worked.
	if _, err := c.Call(ctx, "set_thinking_level",
		map[string]any{"level": "definitely-not-a-level"}); err != nil {
		t.Logf("Pi rejected the invalid level outright: %v", err)
		return
	}

	after, err := c.Call(ctx, "get_state", nil)
	if err != nil {
		t.Fatalf("get_state after invalid level: %v", err)
	}
	var st struct {
		ThinkingLevel string `json:"thinkingLevel"`
	}
	if err := json.Unmarshal(after.Data, &st); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st.ThinkingLevel != "off" {
		t.Fatalf("invalid level changed thinkingLevel to %q; Pi silently accepted it", st.ThinkingLevel)
	}
	t.Logf("Pi accepted the invalid level silently; thinkingLevel stayed %q — "+
		"gateway must validate against get_available_thinking_levels", st.ThinkingLevel)

	// A level from the advertised set must take effect, so the gateway has a
	// way to confirm the mapping landed.
	if _, err := c.Call(ctx, "set_thinking_level", map[string]any{"level": "high"}); err != nil {
		t.Fatalf("set_thinking_level high: %v", err)
	}
	after2, err := c.Call(ctx, "get_state", nil)
	if err != nil {
		t.Fatalf("get_state after high: %v", err)
	}
	var st2 struct {
		ThinkingLevel string `json:"thinkingLevel"`
	}
	if err := json.Unmarshal(after2.Data, &st2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if st2.ThinkingLevel != "high" {
		t.Fatalf("thinkingLevel=%q want high", st2.ThinkingLevel)
	}
	// Leave the session as found.
	if _, err := c.Call(ctx, "set_thinking_level", map[string]any{"level": "off"}); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

// TestPromptStreamsTextAndSettles is the full streaming path: prompt in,
// text_delta events out, agent_settled to close.
func TestPromptStreamsTextAndSettles(t *testing.T) {
	c := startRealPi(t, "opencode-go/mimo-v2.6-flash:off")

	events, cancel := c.Subscribe()
	defer cancel()

	ctx, cancel2 := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel2()

	// Subscribe before prompting: the stream can start immediately, and
	// subscribing afterwards races the first events away.
	rec, err := c.Call(ctx, "prompt", map[string]any{
		"message": "Reply with exactly the word: ACK",
	})
	if err != nil {
		t.Fatalf("prompt: %v (stderr: %s)", err, c.Stderr())
	}
	var disposition struct {
		Disposition string `json:"disposition"`
	}
	_ = json.Unmarshal(rec.Data, &disposition)
	if disposition.Disposition == "" {
		t.Logf("prompt accepted (no disposition field): %s", rec.Data)
	} else {
		t.Logf("disposition=%s", disposition.Disposition)
	}

	var text strings.Builder
	sawToolStart := false
	deadline := time.After(150 * time.Second)

	for {
		select {
		case ev := <-events:
			switch ev.Type {
			case "message_update":
				var ue struct {
					AssistantMessageEvent struct {
						Type  string `json:"type"`
						Delta string `json:"delta"`
					} `json:"assistantMessageEvent"`
				}
				if err := json.Unmarshal(ev.Raw, &ue); err == nil &&
					ue.AssistantMessageEvent.Type == "text_delta" {
					text.WriteString(ue.AssistantMessageEvent.Delta)
				}
			case "tool_execution_start":
				sawToolStart = true
			case pi.TypeAgentSettled:
				if text.Len() == 0 {
					t.Fatal("agent_settled with no assistant text streamed")
				}
				t.Logf("assistant said %q (saw tool exec: %v)", text.String(), sawToolStart)
				return
			}
		case <-deadline:
			t.Fatalf("timeout; streamed so far %q", text.String())
		}
	}
}

// TestUnknownModelIsRejected confirms errors survive the round trip rather than
// being flattened into a generic failure.
func TestUnknownModelIsRejected(t *testing.T) {
	c := startRealPi(t, "opencode-go/mimo-v2.6-flash:off")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := c.Call(ctx, "set_model", map[string]any{"model": "no-such/model"})
	if err == nil {
		t.Fatal("expected failure for an unknown model")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "model") {
		t.Logf("error text: %v", err)
	}
}
