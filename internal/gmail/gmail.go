// Package gmail reads mail and creates drafts through the official Gmail
// API, using the connector grant the OAuth layer sealed into the store
// (#316). It never sees a client secret or a refresh token in plaintext
// beyond the process that decrypts them, and it never sends: send paths
// are refused by the relay before they reach this process (see
// connectors.IsDraftOnlySend), and the send tools below are registered
// only so that refusal names the draft path instead of 404ing.
package gmail

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"

	"agento/internal/connectors"

	"golang.org/x/oauth2"
	"google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"
)

const (
	// DefaultLimit answers "show me recent mail" without a number.
	DefaultLimit = 10
	// MaxLimit bounds one list/search response: at full bodies the worst
	// case is 50 summaries, not 50 downloads — Read fetches one message.
	MaxLimit = 50
	// MaxBody caps one read body. Mail prose is far under this; over-cap
	// bodies are truncated and reported, never silently cut.
	MaxBody = 20000
)

// Client calls the Gmail API as the connected grant. No grant (or a dead
// one) is a reconnect error, never a secrets prompt.
type Client struct {
	svc *gmail.Service
}

// NewClient builds a Client from the stored connector grant, refreshing
// and persisting on expiry (see connectors.GrantClient). Endpoint
// overrides the Gmail API base URL — httptest in tests, empty in prod.
func NewClient(ctx context.Context, store connectors.Store, cfg *connectors.OAuthConfig, endpoint string) (*Client, error) {
	httpClient, err := connectors.GrantClient(ctx, store, cfg, connectors.Gmail)
	if err != nil {
		return nil, err
	}
	opts := []option.ClientOption{option.WithHTTPClient(httpClient)}
	if endpoint != "" {
		opts = append(opts, option.WithEndpoint(endpoint))
	}
	svc, err := gmail.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{svc: svc}, nil
}

// NewClientWithToken builds a Client around an explicit token source (tests
// that drive the Gmail wire without a connector store).
func NewClientWithToken(ctx context.Context, src oauth2.TokenSource, endpoint string) (*Client, error) {
	opts := []option.ClientOption{option.WithTokenSource(src)}
	if endpoint != "" {
		opts = append(opts, option.WithEndpoint(endpoint))
	}
	svc, err := gmail.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{svc: svc}, nil
}

// Message is the agent-facing shape: headers that identify, snippet that
// orients, body on read only. Sizes are capped; ids are Gmail's.
type Message struct {
	ID      string `json:"id"`
	Thread  string `json:"thread_id,omitempty"`
	Subject string `json:"subject,omitempty"`
	From    string `json:"from,omitempty"`
	Date    string `json:"date,omitempty"`
	Snippet string `json:"snippet,omitempty"`
	Body    string `json:"body,omitempty"`
	// Truncated reports an over-cap body cut (same contract as notes:
	// a cut that says so, never silent data loss).
	Truncated bool `json:"truncated,omitempty"`
}

// List returns recent messages (default INBOX), newest first. Bodies are
// NOT fetched here — Snippet orients, Read downloads.
func (c *Client) List(ctx context.Context, label string, limit int) ([]Message, error) {
	limit = boundLimit(limit)
	call := c.svc.Users.Messages.List("me").MaxResults(int64(limit))
	if label = strings.TrimSpace(label); label != "" {
		call.LabelIds(label)
	} else {
		call.LabelIds("INBOX")
	}
	res, err := call.Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("gmail: list messages: %w", err)
	}
	out := make([]Message, 0, len(res.Messages))
	for _, m := range res.Messages {
		full, err := c.read(ctx, m.Id)
		if err != nil {
			return nil, err
		}
		full.Body = ""
		out = append(out, *full)
	}
	return out, nil
}

// Search runs a Gmail query (same syntax as the Gmail search box).
func (c *Client) Search(ctx context.Context, query string, limit int) ([]Message, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("gmail: search needs a query")
	}
	limit = boundLimit(limit)
	res, err := c.svc.Users.Messages.List("me").Q(query).MaxResults(int64(limit)).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("gmail: search messages: %w", err)
	}
	out := make([]Message, 0, len(res.Messages))
	for _, m := range res.Messages {
		full, err := c.read(ctx, m.Id)
		if err != nil {
			return nil, err
		}
		full.Body = ""
		out = append(out, *full)
	}
	return out, nil
}

// Read returns one message with its body (text/plain preferred, HTML
// stripped only by absence — the agent sees no markup here).
func (c *Client) Read(ctx context.Context, id string) (*Message, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("gmail: read needs a message id")
	}
	return c.read(ctx, strings.TrimSpace(id))
}

func (c *Client) read(ctx context.Context, id string) (*Message, error) {
	m, err := c.svc.Users.Messages.Get("me", id).Format("full").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("gmail: read message: %w", err)
	}
	msg := &Message{ID: m.Id, Thread: m.ThreadId, Snippet: m.Snippet}
	for _, h := range m.Payload.Headers {
		switch strings.ToLower(h.Name) {
		case "subject":
			msg.Subject = h.Value
		case "from":
			msg.From = h.Value
		case "date":
			msg.Date = h.Value
		}
	}
	body, _ := plainBody(m.Payload)
	if len(body) > MaxBody {
		body = body[:MaxBody] + "…"
		msg.Truncated = true
	}
	msg.Body = body
	return msg, nil
}

// Draft is the agent-facing shape of a created draft.
type Draft struct {
	ID        string `json:"id"`
	MessageID string `json:"message_id,omitempty"`
}

// CreateDraft stores a compose-needing draft (compose scope creates drafts;
// it cannot send). To, subject, body are all required — a draft with no
// recipient is a note, not a mail draft.
func (c *Client) CreateDraft(ctx context.Context, to, subject, body string) (*Draft, error) {
	to = strings.TrimSpace(to)
	if to == "" || strings.TrimSpace(subject) == "" || strings.TrimSpace(body) == "" {
		return nil, fmt.Errorf("gmail: draft needs to, subject and body")
	}
	var raw strings.Builder
	raw.WriteString("To: " + to + "\r\n")
	raw.WriteString("Subject: " + strings.TrimSpace(subject) + "\r\n")
	raw.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	raw.WriteString(body)
	encoded := base64.URLEncoding.EncodeToString([]byte(raw.String()))
	d, err := c.svc.Users.Drafts.Create("me", &gmail.Draft{
		Message: &gmail.Message{Raw: encoded},
	}).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("gmail: create draft: %w", err)
	}
	return &Draft{ID: d.Id, MessageID: d.Message.Id}, nil
}

func boundLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}

// plainBody walks the payload tree for the first text/plain part (falling
// back to any text/*), decoding base64url bodies. Attachments are skipped:
// the agent reads mail, it does not download files.
func plainBody(p *gmail.MessagePart) (string, bool) {
	if p == nil {
		return "", false
	}
	if strings.HasPrefix(p.MimeType, "text/") && p.Body != nil && p.Body.Data != "" {
		raw, err := base64.URLEncoding.DecodeString(p.Body.Data)
		if err != nil {
			if raw, err = base64.StdEncoding.DecodeString(p.Body.Data); err != nil {
				return "", false
			}
		}
		return string(raw), true
	}
	for _, part := range p.Parts {
		if strings.HasPrefix(part.MimeType, "text/plain") {
			if s, ok := plainBody(part); ok {
				return s, true
			}
		}
	}
	for _, part := range p.Parts {
		if s, ok := plainBody(part); ok {
			return s, true
		}
	}
	return "", false
}
