package gateway

import (
	"net/http"
	"strings"
	"testing"

	"agento/internal/pi"
)

// The defect this exists for: an errored turn produces no content, so before this the
// gateway ended the stream cleanly and the client received an empty completion with
// `finish_reason: "stop"` — indistinguishable from a turn that legitimately said nothing.
// During a live outage that is exactly what it did: pi recorded `stopReason: "error"` in
// every session file while every response reported success.

func TestDecodeTurnError(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{
			name:    "the real outage record",
			raw:     `{"type":"message","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"Cannot convert argument to a ByteString because the character at index 93 has a value of 8212 which is greater than 255."}}`,
			wantErr: true,
		},
		{
			name:    "an errored turn with no detail still fails",
			raw:     `{"type":"message","message":{"role":"assistant","content":[],"stopReason":"error"}}`,
			want:    "the agent reported a failed turn with no detail",
			wantErr: true,
		},
		// Every one of these is a normal completed turn. Treating a non-empty stop
		// reason as failure would turn every reply into an error.
		{name: "completed", raw: `{"type":"message","message":{"role":"assistant","stopReason":"stop","content":[]}}`},
		{name: "tool use", raw: `{"type":"message","message":{"role":"assistant","stopReason":"toolUse","content":[]}}`},
		{name: "no stop reason", raw: `{"type":"message","message":{"role":"assistant","content":[]}}`},
		{name: "user row", raw: `{"type":"message","message":{"role":"user","content":[]}}`},
		{name: "system row", raw: `{"type":"message","message":{"role":"system","content":[]}}`},
		{name: "not a message record", raw: `{"type":"model_change","modelId":"mimo-v2.6-flash"}`},
		{name: "no message key", raw: `{"type":"message"}`},
		{name: "malformed json", raw: `{"type":"message"`},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, failed := decodeTurnError(pi.Record{Raw: []byte(c.raw)})
			if failed != c.wantErr {
				t.Fatalf("failed = %v, want %v (got %q)", failed, c.wantErr, got)
			}
			if c.want != "" && got != c.want {
				t.Errorf("message = %q, want %q", got, c.want)
			}
			if c.wantErr && c.want == "" && !strings.Contains(got, "ByteString") {
				t.Errorf("message = %q, want pi's own text so the cause is visible", got)
			}
		})
	}
}

// Pi's error text reaches a client, so it is bounded like every other reflected value.
func TestDecodeTurnErrorBoundsTheMessage(t *testing.T) {
	long := strings.Repeat("é", 4000)
	raw := `{"type":"message","message":{"role":"assistant","stopReason":"error","errorMessage":"` + long + `"}}`
	got, failed := decodeTurnError(pi.Record{Raw: []byte(raw)})
	if !failed {
		t.Fatal("expected the turn to be reported as failed")
	}
	if len(got) > 1024 {
		t.Errorf("message is %d bytes, want it bounded", len(got))
	}
}

// The end-to-end shape of the bug: an errored turn must not come back as a successful empty completion.
//
// The two paths fail differently and both need checking: a streaming response has already
// sent its 200 by the time the failure is known, so the error can only travel as an SSE
// error frame, while a non-streaming request can still use the status code.
func TestErroredTurnIsNotAnEmptySuccess(t *testing.T) {
	seedFailure := func() *fakeAgent {
		agent := newFakeAgent()
		agent.noSettle = true
		agent.events = []pi.Record{
			{Type: "message", Raw: []byte(
				`{"type":"message","message":{"role":"assistant","content":[],"stopReason":"error","errorMessage":"upstream refused"}}`)},
			{Type: pi.TypeAgentSettled},
		}
		return agent
	}

	t.Run("streaming carries the error as an SSE frame", func(t *testing.T) {
		srv := newTestServer(t, seedFailure(), Config{})
		rec := post(t, srv, "/v1/chat/completions", "test-token", validBody)
		// 200 is unavoidable: the head went out before the failure was known.
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for a stream already opened", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "upstream refused") {
			t.Errorf("stream body = %q, want pi's reason in an error frame", body)
		}
		if strings.Contains(body, `"finish_reason":"stop"`) {
			t.Errorf("stream body = %q, want no successful completion for a failed turn", body)
		}
	})

	t.Run("non-streaming uses the status code", func(t *testing.T) {
		srv := newTestServer(t, seedFailure(), Config{})
		rec := post(t, srv, "/v1/chat/completions", "test-token",
			`{"model":"mimo-v2.6-flash","model_options":{"reasoning_effort":"low"},`+
				`"messages":[{"role":"user","content":"hi"}]}`)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502 so a caller can tell failure from a short answer: %s",
				rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "upstream refused") {
			t.Errorf("body = %s, want pi's reason so the cause is visible", rec.Body.String())
		}
	})
}
