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
	"errors"
	"fmt"
	"mime"
	"strings"
	"time"
	"unicode"

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
	// MaxSubject caps a draft subject line. Outbound cuts refuse rather
	// than truncate: silently shortening an email the user will send is
	// data loss, and the error names our bound, not Google's. Gmail
	// accepts MBs, so a legit long draft errors today; the raise path is
	// chunking or an explicit user confirm, not a silent lift.
	//
	// The cap counts decoded runes, not wire bytes: a 200-rune non-ASCII
	// subject Q-encodes to ~600 unfolded bytes. MaxSubject bounds the
	// pre-image the user wrote; the wire form is longer by encoding.
	MaxSubject = 200
)

// Client calls the Gmail API as the connected grant. The grant resolves
// per call, not at construction: a fresh deploy with no grant yet must
// still boot (the entrypoint probes every server), and every tool answers
// reconnect guidance until the user connects. No grant (or a dead one) is
// a reconnect error, never a secrets prompt.
type Client struct {
	store    connectors.Store
	cfg      *connectors.OAuthConfig
	endpoint string
	// logf carries provider-side refresh failure detail to container
	// logs (nil = silent). Test-only callers leave it nil.
	logf func(string, ...any)
	// static bypasses the vault: test-only, set by NewClientWithToken.
	static *gmail.Service
}

// NewClient binds store + config + endpoint. It needs no grant and no
// OAuth client pair: Mongo is the only boot requirement, so construction
// fails only on programmer error (nil store), never on a disconnected user
// and never on a deploy without Google OAuth — a nil cfg boots fine and
// every Google-touching call answers "not configured" per call (see
// connectors.GrantTokenSource) until the deploy is configured.
func NewClient(store connectors.Store, cfg *connectors.OAuthConfig, endpoint string, logf func(string, ...any)) (*Client, error) {
	if store == nil {
		return nil, fmt.Errorf("gmail: client needs a store")
	}
	return &Client{store: store, cfg: cfg, endpoint: endpoint, logf: logf}, nil
}

// NewClientWithToken builds a Client around an explicit token source.
// Test-only: production always resolves the vault grant per call.
func NewClientWithToken(ctx context.Context, src oauth2.TokenSource, endpoint string) (*Client, error) {
	opts := []option.ClientOption{option.WithTokenSource(src)}
	if endpoint != "" {
		opts = append(opts, option.WithEndpoint(endpoint))
	}
	svc, err := gmail.NewService(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &Client{static: svc}, nil
}

// service resolves the Gmail API client for one call: the vault grant
// (refreshing and persisting on expiry), or the static test service.
// Grant errors surface here — per tool call, never at boot.
// callCtx bounds one Google API call: Pi issues no deadline of its own,
// so an unbounded call would hang the tool (and the agent turn) on a hung
// upstream. 20s covers refresh + call together: the OAuth refresh inside
// the transport runs under the same deadline as the list/get itself. A
// slow refresh therefore starves a slow call (fail-closed timeout, never
// a hang) — splitting the budget would trade one sharp edge for two.
func callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 20*time.Second)
}

func (c *Client) service(ctx context.Context) (*gmail.Service, error) {
	if c.static != nil {
		return c.static, nil
	}
	httpClient, err := connectors.GrantClient(ctx, c.store, c.cfg, connectors.Gmail, c.logf)
	if err != nil {
		return nil, err
	}
	opts := []option.ClientOption{option.WithHTTPClient(httpClient)}
	if c.endpoint != "" {
		opts = append(opts, option.WithEndpoint(c.endpoint))
	}
	return gmail.NewService(ctx, opts...)
}

// Message is the agent-facing shape: headers that identify, snippet that
// orients, body on read only. Sizes are capped; ids are Gmail's.
// Body is "" with Truncated:false when no text/plain part exists
// (attachment-only or empty-text mail) — "no readable text", not "fetch
// failed". Snippet still orients those; Read never errors on them.
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
	// IsHTML reports markup-as-body: no text/plain part existed, so the
	// fallback returned HTML. The agent must not read tags as prose.
	IsHTML bool `json:"is_html,omitempty"`
}

// ListResult is one list/search answer. Partial reports skipped rows: a
// deleted message mid-list degrades to a subset that says so, never an
// all-or-nothing failure that voids the 49 good rows.
type ListResult struct {
	Messages []Message `json:"messages"`
	Partial  bool      `json:"partial,omitempty"`
}

// List returns recent messages (default INBOX), newest first. Rows carry
// headers + snippet only (metadata format — one cheap fetch each, no
// bodies); Read downloads. Rows that fail to fetch are skipped with
// Partial set, not fatal.
func (c *Client) List(ctx context.Context, label string, limit int) (*ListResult, error) {
	limit = boundLimit(limit)
	ctx, cancel := callCtx(ctx)
	defer cancel()
	svc, err := c.service(ctx)
	if err != nil {
		return nil, err
	}
	call := svc.Users.Messages.List("me").MaxResults(int64(limit))
	if label = strings.TrimSpace(label); label != "" {
		call.LabelIds(label)
	} else {
		call.LabelIds("INBOX")
	}
	res, err := call.Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("gmail: list messages: %w", err)
	}
	if res == nil {
		return nil, fmt.Errorf("gmail: list messages: empty response")
	}
	return c.rows(ctx, svc, res.Messages)
}

// Search runs a Gmail query (same syntax as the Gmail search box). Same
// row shape and partial contract as List.
func (c *Client) Search(ctx context.Context, query string, limit int) (*ListResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("gmail: search needs a query")
	}
	limit = boundLimit(limit)
	ctx, cancel := callCtx(ctx)
	defer cancel()
	svc, err := c.service(ctx)
	if err != nil {
		return nil, err
	}
	res, err := svc.Users.Messages.List("me").Q(strings.TrimSpace(query)).MaxResults(int64(limit)).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("gmail: search messages: %w", err)
	}
	if res == nil {
		return nil, fmt.Errorf("gmail: search messages: empty response")
	}
	return c.rows(ctx, svc, res.Messages)
}

func (c *Client) rows(ctx context.Context, svc *gmail.Service, ids []*gmail.Message) (*ListResult, error) {
	out := &ListResult{Messages: make([]Message, 0, len(ids))}
	for _, m := range ids {
		// Fresh budget per row, independent of the list-level ctx: the
		// list already paid for refresh + list against that budget, and
		// a child WithTimeout never outlives its parent — deriving from
		// ctx would cap every row by the list's remaining time. Half an
		// exception, recorded honestly: the API HTTP gets the fresh
		// 20s, but the OAuth refresh inside the transport still sees
		// the list-level deadline. Harmless in practice (list just
		// refreshed, token is fresh for rows). The tradeoff (no
		// client-cancel propagation into row fetches) is deliberate:
		// each row is bounded at 20s, so a hung row fails loud instead
		// of hanging the turn, and the MCP layer is request/response
		// over stdio with nothing to forward a hangup to anyway.
		if m == nil || strings.TrimSpace(m.Id) == "" {
			// Never returned in practice, but a nil or id-less entry
			// must not panic the whole list or burn a round-trip on a
			// GET with an empty id (which 404s into a misleading
			// Partial): same skip-and-report contract as a deleted
			// message.
			out.Partial = true
			continue
		}
		rctx, cancel := callCtx(context.WithoutCancel(ctx))
		full, err := readMessage(rctx, svc, strings.TrimSpace(m.Id), "metadata")
		cancel()
		if err != nil {
			// A row that timed out is a hung upstream, not a deleted
			// message: fail the call instead of hiding it in Partial.
			if errors.Is(err, context.DeadlineExceeded) {
				return nil, fmt.Errorf("gmail: list timed out: %w", err)
			}
			// A message deleted between list and fetch is routine, not
			// fatal: skip it and say the result is partial.
			out.Partial = true
			continue
		}
		out.Messages = append(out.Messages, *full)
	}
	return out, nil
}

// Read returns one message with its body (text/plain preferred; when no
// text/plain part exists the fallback may be HTML — the agent sees markup
// only in that case, never by preference).
func (c *Client) Read(ctx context.Context, id string) (*Message, error) {
	if strings.TrimSpace(id) == "" {
		return nil, fmt.Errorf("gmail: read needs a message id")
	}
	ctx, cancel := callCtx(ctx)
	defer cancel()
	svc, err := c.service(ctx)
	if err != nil {
		return nil, err
	}
	return readMessage(ctx, svc, strings.TrimSpace(id), "full")
}

func readMessage(ctx context.Context, svc *gmail.Service, id, format string) (*Message, error) {
	call := svc.Users.Messages.Get("me", id)
	if format == "metadata" {
		call.Format("metadata").MetadataHeaders("Subject", "From", "Date")
	} else {
		call.Format("full")
	}
	m, err := call.Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("gmail: read message: %w", err)
	}
	if m == nil {
		return nil, fmt.Errorf("gmail: read message: empty response")
	}
	msg := &Message{ID: m.Id, Thread: m.ThreadId, Snippet: m.Snippet}
	if m.Payload != nil {
		var dec mime.WordDecoder
		for _, h := range m.Payload.Headers {
			if h == nil {
				continue
			}
			// Wire subjects/addresses may arrive RFC2047-encoded
			// (non-ASCII); decode for the agent, keep raw on failure.
			// This mirrors the compose side, which Q-encodes on the
			// way out — what the agent reads matches what it writes.
			val := h.Value
			if decoded, err := dec.DecodeHeader(val); err == nil {
				val = decoded
			}
			switch strings.ToLower(h.Name) {
			case "subject":
				msg.Subject = val
			case "from":
				msg.From = val
			case "date":
				// Raw by design: dates are always ASCII and never
				// RFC2047, so decoding could only rewrite a malformed
				// date into something less faithful.
				msg.Date = h.Value
			}
		}
		if format == "full" {
			body, isHTML, _ := plainBody(m.Payload)
			if t, cut := truncateRunes(body, MaxBody); cut {
				body, msg.Truncated = t, true
			}
			msg.Body = body
			msg.IsHTML = isHTML && body != ""
		}
	}
	return msg, nil
}

// Draft is the agent-facing shape of a created draft.
type Draft struct {
	ID        string `json:"id"`
	MessageID string `json:"message_id,omitempty"`
}

// CreateDraft stores a compose-needing draft (compose scope creates drafts;
// it cannot send). To, subject, body are all required — a draft with no
// recipient is a note, not a mail draft. Header fields reject control
// bytes (a \r\n in subject would inject a MIME header), and To must be
// email-shaped.
func (c *Client) CreateDraft(ctx context.Context, to, subject, body string) (*Draft, error) {
	to = strings.TrimSpace(to)
	subject = strings.TrimSpace(subject)
	body = strings.TrimSpace(body)
	if to == "" || subject == "" || body == "" {
		return nil, fmt.Errorf("gmail: draft needs to, subject and body")
	}
	if strings.ContainsAny(to+subject, "\r\n") {
		return nil, fmt.Errorf("gmail: draft headers must not contain line breaks")
	}
	if !strings.Contains(to, "@") {
		return nil, fmt.Errorf("gmail: draft recipient must be an email address")
	}
	if strings.ContainsAny(to, ",;") {
		return nil, fmt.Errorf("gmail: draft takes one recipient address, not a list")
	}
	if strings.ContainsAny(to, " \t<>") {
		// Singular bare address by contract: no display names
		// ("John Doe <a@x>") and no space-separated pairs. ASCII
		// display names would actually send, but the tool promises
		// one address — pass "John Doe" as the subject/body if the
		// name matters, or extend the contract deliberately.
		return nil, fmt.Errorf("gmail: draft takes one bare email address")
	}
	if !isASCII(to) {
		// Display names ("café <a@x>") go on the wire raw like subjects
		// did before QEncoding: reject for v1 instead of garbling.
		// Gmail validates on create; ASCII-only is the contract until
		// display-name encoding lands next to subject encoding.
		return nil, fmt.Errorf("gmail: draft recipient must be ASCII")
	}
	// Outbound caps (fail-closed, not truncated): silently cutting an
	// email body the user will send is data loss. Gmail validates size
	// on create, but the error should name our bound, not Google's.
	if n := len([]rune(subject)); n > MaxSubject {
		return nil, fmt.Errorf("gmail: draft subject too long (%d runes, max %d)", n, MaxSubject)
	}
	if n := len([]rune(body)); n > MaxBody {
		return nil, fmt.Errorf("gmail: draft body too long (%d runes, max %d)", n, MaxBody)
	}
	// Validated first, budgeted second: the whole 20s goes to refresh +
	// the Create call, none of it burned on checks that need no I/O.
	// Symmetric with calendar List/Get (validate before service()).
	ctx, cancel := callCtx(ctx)
	defer cancel()
	if !isASCII(subject) {
		// Raw UTF-8 is not valid RFC2822 in a header (needs RFC2047
		// encoded-word); Gmail accepts it in practice, but a café
		// subject is where it garbles. Note the whole header is
		// encoded when ANY rune is non-ASCII (QEncoding has no partial
		// mode), so "Re: café" goes on the wire fully encoded —
		// correct, just less human-readable than the ASCII-only path
		// this branch skips. Long encoded-words are not folded at 76
		// chars (MaxSubject bounds the damage; Gmail accepts unfolded
		// headers in practice).
		subject = mime.QEncoding.Encode("utf-8", subject)
	}
	svc, err := c.service(ctx)
	if err != nil {
		return nil, err
	}
	var raw strings.Builder
	// Minimal headers Gmail accepts: To/Subject/Content-Type. From
	// defaults to the grant identity and MIME-Version is not required in
	// practice — if a future interop bug appears, these two headers are
	// the first place to look.
	raw.WriteString("To: " + to + "\r\n")
	raw.WriteString("Subject: " + subject + "\r\n")
	// v1: UTF-8 raw body with no Content-Transfer-Encoding. Gmail accepts
	// raw 8-bit here in practice; if non-ASCII bodies ever corrupt, the
	// fix is CTE + base64 body encoding at this exact spot.
	raw.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	raw.WriteString(body)
	// Raw base64url per the Gmail API: unpadded, URL-safe.
	encoded := base64.RawURLEncoding.EncodeToString([]byte(raw.String()))
	d, err := svc.Users.Drafts.Create("me", &gmail.Draft{
		Message: &gmail.Message{Raw: encoded},
	}).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("gmail: create draft: %w", err)
	}
	// The API normally returns message.id, but a minimal response must
	// fail the call, never panic the server process.
	if d == nil || d.Message == nil {
		return nil, fmt.Errorf("gmail: create draft: upstream answered without a draft")
	}
	return &Draft{ID: d.Id, MessageID: d.Message.Id}, nil
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// boundLimit caps one list/search page. The cap is silent by design
// (documented on the tool surface, not a wire flag): 50 summaries is the
// most an agent turn can use, and a capped page reads complete — narrow
// the query rather than asking for more.
func boundLimit(limit int) int {
	if limit <= 0 {
		return DefaultLimit
	}
	if limit > MaxLimit {
		return MaxLimit
	}
	return limit
}

// plainBody walks the payload tree with a global preference: any
// text/plain part anywhere beats any text/html anywhere. The two passes
// are separate walkers (not filter-then-recurse per container), because
// recursing into full plainBody per container lets an HTML-only
// container earlier in document order win over a later plain sibling —
// the exact misflag the IsHTML contract exists to prevent.
// Bodies are base64url; parts carrying a filename or an attachment id
// are attachments, not bodies, and are skipped: the agent reads mail,
// it does not download files.
//
// Bodies are decoded as UTF-8 without consulting charset=: most mail is
// UTF-8 and the API gives no transcoding; a non-UTF-8 part will mojibake
// rather than fail. Revisit with charset handling if that ever surfaces.
func plainBody(p *gmail.MessagePart) (body string, isHTML bool, ok bool) {
	if s, ok := plainText(p); ok {
		return s, false, true
	}
	if s, ok := htmlText(p); ok {
		return s, true, true
	}
	return "", false, false
}

// plainText finds the first text/plain part depth-first, descending into
// any container part (multipart/*, message/rfc822 forwarded bodies —
// Gmail nests forwarded mail a layer deeper, and matching only
// multipart/* would stop the walk exactly where readable mail lives).
// Never returns HTML. MIME types match exactly per the Gmail API
// contract (bare lowercase types, no parameters); a future
// "text/plain; charset=..." or case variant would miss here and surface
// as empty — the exact-match keeps the IsHTML promise airtight today,
// and that hypothetical is where to add parameter-stripping.
func plainText(p *gmail.MessagePart) (string, bool) {
	if p == nil || isAttachment(p) {
		return "", false
	}
	if p.MimeType == "text/plain" && p.Body != nil && p.Body.Data != "" {
		return decodeB64(p.Body.Data)
	}
	for _, part := range p.Parts {
		if s, ok := plainText(part); ok {
			return s, true
		}
	}
	return "", false
}

// htmlText finds the first text/html part depth-first, descending into
// any container part for the same reason. Only the fallback may return
// HTML; the plain pass above never does.
func htmlText(p *gmail.MessagePart) (string, bool) {
	if p == nil || isAttachment(p) {
		return "", false
	}
	if p.MimeType == "text/html" && p.Body != nil && p.Body.Data != "" {
		return decodeB64(p.Body.Data)
	}
	for _, part := range p.Parts {
		if s, ok := htmlText(part); ok {
			return s, true
		}
	}
	return "", false
}

// decodeB64 decodes Gmail base64url (unpadded first, padded and std as
// fallback). All whitespace is stripped first — payloads occasionally
// wrap lines, and Go's decoders reject \n anywhere, not just at the
// ends. An all-whitespace payload decodes to nothing, not to an empty
// success: stripping first and then finding nothing is missing data,
// and found-empty would misreport it as a body.
func decodeB64(data string) (string, bool) {
	data = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, data)
	if data == "" {
		return "", false
	}
	for _, enc := range []*base64.Encoding{
		base64.RawURLEncoding, base64.URLEncoding,
		base64.RawStdEncoding, base64.StdEncoding,
	} {
		if raw, err := enc.DecodeString(data); err == nil {
			return string(raw), true
		}
	}
	return "", false
}

func isAttachment(p *gmail.MessagePart) bool {
	// Half-exception, named honestly: Gmail puts large inline bodies
	// behind messages.attachments.get (Body.Data == "" with an
	// AttachmentId set, no filename) — same shape as a real attachment.
	// Those mails read empty today (use the snippet); fetching them is
	// the follow-up, not a silent misflag as missing. But a part
	// carrying BOTH readable Data and an id (large-inline continuation
	// shape) still decodes: only the Data-less id form is skipped.
	if p == nil {
		return false
	}
	return p.Filename != "" || (p.Body != nil && p.Body.Data == "" && p.Body.AttachmentId != "")
}

// truncateRunes cuts s to max runes (not bytes — a byte cut can split a
// multi-byte rune mid-sequence), reporting whether it cut. The cut keeps
// max content runes with the marker appended past them, so the result is
// max+1 runes and the cut is visible.
func truncateRunes(s string, max int) (string, bool) {
	runes := []rune(s)
	if len(runes) <= max {
		return s, false
	}
	return string(runes[:max]) + "…", true
}
