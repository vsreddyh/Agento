package teach

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// inferenceURL is the OpenAI-compatible Go surface for direct model
	// calls — NOT the zen agent endpoint, which requires session headers
	// only agent clients maintain.
	inferenceURL = "https://opencode.ai/inference/go/v1/chat/completions"
	// userAgent identifies the caller per the Go docs (own UA, not a
	// generic HTTP-library name).
	userAgent = "agento-teach/1.0"
	// maxTokens bounds one test reply: skill-test prompts are short and so
	// are their answers; an unbounded completion is a runaway cost with
	// nobody watching.
	maxTokens = 1024
	// callTimeout bounds one test call: the skill tab must not hang
	// behind a slow model.
	callTimeout = 3 * time.Minute
)

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model     string        `json:"model"`
	Messages  []chatMessage `json:"messages"`
	Stream    bool          `json:"stream"`
	MaxTokens int           `json:"max_tokens"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// BareClient is a direct model client with no persona and no tools: one
// prompt in, one reply out. HTTP is injectable for tests. It serves skill
// test-fires (same key and model as the retired scheduler, no job store)
// and unit tests.
type BareClient struct {
	APIKey  string
	Model   string
	BaseURL string // inferenceURL unless overridden in tests
	HTTP    *http.Client
}

// Run sends one skill-test prompt. tag is stable per test (agento-teach-<id>)
// so the provider can route and cache by conversation, per the Go docs.
func (c *BareClient) Run(ctx context.Context, prompt, tag string) (string, error) {
	if strings.TrimSpace(c.APIKey) == "" {
		return "", fmt.Errorf("teach: OPENCODE_API_KEY is unset")
	}
	model := strings.TrimSpace(c.Model)
	model = strings.TrimPrefix(model, "opencode-go/")
	if model == "" {
		return "", fmt.Errorf("teach: model is unset")
	}
	url := c.BaseURL
	if url == "" {
		url = inferenceURL
	}
	body, err := json.Marshal(chatRequest{
		Model:     model,
		Messages:  []chatMessage{{Role: "user", Content: prompt}},
		Stream:    false,
		MaxTokens: maxTokens,
	})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(c.APIKey))
	req.Header.Set("x-opencode-session", tag)
	req.Header.Set("User-Agent", userAgent)
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("teach: call failed: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("teach: read failed: %w", err)
	}
	var out chatResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("teach: bad response (HTTP %d): %s", resp.StatusCode, truncate(string(raw), 200))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || out.Error != nil {
		msg := ""
		if out.Error != nil {
			msg = out.Error.Message
		}
		if msg == "" {
			msg = truncate(string(raw), 200)
		}
		return "", fmt.Errorf("teach: HTTP %d: %s", resp.StatusCode, msg)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("teach: empty choices")
	}
	text := strings.TrimSpace(out.Choices[0].Message.Content)
	if text == "" {
		return "", fmt.Errorf("teach: empty reply")
	}
	return text, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
