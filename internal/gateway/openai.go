package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"agento/internal/pi"
)

// Wire types for the chat completions surface. Field names match what the app
// sends and parses; anything the app ignores is omitted rather than sent empty.

// chatRequest is the request body the app posts.
type chatRequest struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`

	// ModelOptions carries the reasoning override. Required: the app is
	// configured to always send it, and a request without one is rejected rather
	// than silently defaulted, because a silent default is indistinguishable from
	// the requested setting having been honoured.
	ModelOptions *modelOptions `json:"model_options"`

	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
}

type modelOptions struct {
	ReasoningEffort string `json:"reasoning_effort"`
}

type chatMessage struct {
	Role string `json:"role"`
	// Content is chatContent, not a string: OpenAI permits either a bare string
	// or an array of typed parts, and decoding straight into a string made the
	// multipart form fail as an unparseable body — a mystery 400 for any client
	// using the more standard shape.
	Content chatContent `json:"content"`
}

// chatContent is the text and images of one message.
//
// Pi's prompt command takes plain text plus separate ImageContent values, which
// is exactly what the two shapes below reduce to, so both OpenAI spellings are
// accepted rather than only the one this app happens to send.
type chatContent struct {
	Text   string
	Images []piImage
}

// piImage mirrors Pi's ImageContent: raw base64 plus a MIME type.
type piImage struct {
	Data     string
	MimeType string
}

// UnmarshalJSON accepts either a bare string or an array of content parts.
func (c *chatContent) UnmarshalJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}

	// Bare string form.
	if trimmed[0] == '"' {
		return json.Unmarshal(trimmed, &c.Text)
	}

	var parts []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		ImageURL struct {
			URL string `json:"url"`
		} `json:"image_url"`
	}
	if err := json.Unmarshal(trimmed, &parts); err != nil {
		return err
	}

	// Text parts are joined with a newline rather than run together. With an image
	// between them, plain concatenation makes the text on either side of it read as
	// one run, and nothing marks where the image was:
	//
	//	["describe this:", image, "be brief"] -> "describe this:be brief"
	//
	// The newline keeps the position of the image legible. The app itself sends a
	// bare string (ChatApi puts content as a plain string), so this only affects
	// clients using the conventional multipart shape.
	var textParts []string
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text", "output_text":
			textParts = append(textParts, part.Text)
		case "image_url", "input_image":
			img, err := parseDataURI(part.ImageURL.URL)
			if err != nil {
				return err
			}
			c.Images = append(c.Images, img)
		case "":
			// A part with no type is malformed; ignoring it would silently drop
			// content the caller believed was sent.
			return fmt.Errorf("gateway: content part has no type")
		default:
			// Fail-closed, unlike unknown JSON fields elsewhere in the body, which
			// are ignored for forward compatibility. The difference is that dropping
			// a field the server does not understand is harmless, while dropping a
			// content part means sending the agent less than the user asked it to,
			// and answering the wrong question as though nothing were wrong. A 400
			// naming the part type is the recoverable version of that.
			//
			// The cost is that a newer client using a part type Pi does not yet carry
			// gets a clear refusal rather than a partial turn. That is the intended
			// tradeoff, not an oversight to be "fixed" by skipping unknown parts.
			return fmt.Errorf("gateway: unsupported content part type %q", part.Type)
		}
	}
	c.Text = strings.Join(textParts, "\n")
	return nil
}

// parseDataURI splits a data: URL into the base64 payload and MIME type Pi wants.
//
// Only data URIs are accepted. A remote http(s) URL would mean the gateway
// fetching an arbitrary URL on the app's behalf, which is a request-forgery
// surface far larger than this migration is for — so it is refused explicitly
// rather than not fetched.
func parseDataURI(url string) (piImage, error) {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
			return piImage{}, fmt.Errorf(
				"gateway: remote image URLs are not supported; send a base64 data: URI")
		}
		return piImage{}, fmt.Errorf("gateway: image_url must be a base64 data: URI")
	}
	rest := url[len(prefix):]
	mime, payload, ok := strings.Cut(rest, ",")
	// ";BASE64" is as valid as ";base64" — RFC 2397 does not fix the case of the
	// extension token, and a client that upper-cases it got a 400 for something
	// that is legal.
	if !ok || !strings.Contains(strings.ToLower(mime), "base64") {
		return piImage{}, fmt.Errorf("gateway: image_url data URI must be base64-encoded")
	}
	// Take the bare type, not everything up to ";base64". A URI like
	// "image/png;charset=utf-8;base64" would otherwise yield the MIME type
	// "image/png;charset=utf-8", which Pi would reject as an unknown format.
	var mimeType string
	mimeType, _, _ = strings.Cut(mime, ";")
	if mimeType == "" {
		return piImage{}, fmt.Errorf("gateway: image_url data URI has no MIME type")
	}
	return piImage{Data: payload, MimeType: mimeType}, nil
}

// SSE frames.

// chunk is one streamed assistant delta. The app reads choices[0].delta.content
// and choices[0].delta.reasoning_content, and tolerates `message` as a fallback.
type chunk struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	Model   string `json:"model,omitempty"`
	// omitempty so the final usage-only frame does not carry `"choices":null`.
	// The app tells a tool frame from an assistant frame by the presence of
	// choices, so an explicit null there is at best noise and at worst reads as a
	// malformed frame.
	Choices []chunkChoice `json:"choices,omitempty"`
	Usage   *usage        `json:"usage,omitempty"`
}

type chunkChoice struct {
	Index int   `json:"index"`
	Delta delta `json:"delta"`
}

// completion is the non-streaming response body.
//
// A SEPARATE type from chunk on purpose. Reusing chunk here put
// choices[0].delta into a non-streamed response, which is the streaming shape:
// a standard OpenAI client sending stream:false reads choices[0].message, finds
// nothing, and gets a 200 it cannot parse. The app always streams and never sees
// this path, so nothing caught it.
//
// The two shapes are not interchangeable, and the difference is the whole point of
// having a buffered mode at all.
type completion struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model,omitempty"`
	Choices []completionChoice `json:"choices"`
	Usage   *usage             `json:"usage,omitempty"`
}

type completionChoice struct {
	Index   int    `json:"index"`
	Message delta  `json:"message"`
	Finish  string `json:"finish_reason"`
}

// delta is one streamed increment. It doubles as the buffered message body,
// where the field names happen to be identical — OpenAI uses the same content and
// reasoning_content keys in both shapes, which is the only reason sharing a type
// is not misleading.
type delta struct {
	Content          string `json:"content,omitempty"`
	ReasoningContent string `json:"reasoning_content,omitempty"`
}

// toolFrame is a tool-progress frame. It deliberately has no choices array: the
// app keys on the presence of `tool` to route these separately from assistant
// text, and a frame carrying both would be misread.
// toolFrame is a tool-progress frame. No `label`: Pi does not send a per-call
// label for tool events, and a field that is always absent is wire surface that
// only ever rots. The app reads label optionally.
type toolFrame struct {
	Tool   string `json:"tool"`
	Status string `json:"status,omitempty"`
}

// errorFrame is terminal. The app treats it as the end of the turn: no [DONE]
// follows, exactly as for an HTTP error.
type errorFrame struct {
	Error string `json:"error"`
}

// usage mirrors the token counts the app reads, accepting the names Pi reports
// plus the OpenAI spellings.
type usage struct {
	PromptTokens     int64 `json:"prompt_tokens,omitempty"`
	CompletionTokens int64 `json:"completion_tokens,omitempty"`
	TotalTokens      int64 `json:"total_tokens,omitempty"`
	CacheReadTokens  int64 `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
}

// piUsage is the shape Pi reports usage in: a top-level `usage` on
// message_update.
//
// Only the top level is decoded. Other events nest a `usage` of their own —
// compaction_end carries result.usage, which counts the COMPACTION — and reading
// those as the turn's totals would replace real counts with unrelated ones. The
// gateway does decode the top level from every record rather than only from
// message_update, so a future event carrying turn-level usage cannot lose it.
type piUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	// Reasoning is decoded but not sent on the streaming path: toWire below deliberately
	// omits it, because a turn's reasoning tokens are already inside Output for every
	// provider that reports them separately, and publishing both would double-count. The
	// session-totals route reads it, where the app wants the split.
	Reasoning int64 `json:"reasoning"`
	Total     int64 `json:"totalTokens"`
}

func (u piUsage) toWire() *usage {
	return &usage{
		PromptTokens:     u.Input,
		CompletionTokens: u.Output,
		TotalTokens:      u.Total,
		CacheReadTokens:  u.CacheRead,
		CacheWriteTokens: u.CacheWrite,
	}
}

// nonZero reports whether any count is set, so an all-zero usage frame is omitted
// rather than sent as a misleading zero-token turn.
func (u *usage) nonZero() bool {
	return u != nil && (u.PromptTokens != 0 || u.CompletionTokens != 0 ||
		u.TotalTokens != 0 || u.CacheReadTokens != 0 || u.CacheWriteTokens != 0)
}

// errorResponse is the OpenAI error envelope. The app shows the raw body text for
// a non-2xx, so the message is what the user actually sees — it has to explain
// the fix, not just name the field.
type errorResponse struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Param   string `json:"param,omitempty"`
		Code    string `json:"code,omitempty"`
	} `json:"error"`
}

// sseWriter emits the `data: <json>` lines the app's hand-rolled parser expects.
//
// Each frame is written and flushed individually: the app reads a line at a time
// and renders deltas live, so buffering would defeat streaming entirely. `:`
// lines are used for keepalive because the app explicitly skips them.
type sseWriter struct {
	w  http.ResponseWriter
	fc *http.ResponseController
	// mu guards every write. http.ResponseWriter is explicitly not safe for
	// concurrent use, and two goroutines write here: the handler emits turn
	// frames while the keepalive ticker emits comments. Without this, a turn
	// longer than the keepalive interval interleaves the two mid-frame.
	mu sync.Mutex
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	return &sseWriter{w: w, fc: http.NewResponseController(w)}
}

func (s *sseWriter) headers() {
	s.w.Header().Set("Content-Type", "text/event-stream")
	s.w.Header().Set("Cache-Control", "no-cache")
	s.w.Header().Set("Connection", "keep-alive")
	// Nginx buffers proxied responses by default, which would hold the whole turn
	// until it completed. This disables it for the streaming route.
	s.w.Header().Set("X-Accel-Buffering", "no")
}

// write emits one JSON payload as an SSE data line. The trailing newline matters:
// the app splits on it.
func (s *sseWriter) write(payload any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// SetEscapeHTML(false) keeps reasoning text with < and > readable instead of
	// \u003c-escaped, which matters because model output is full of them.
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return err
	}
	return s.emit("data: " + buf.String())
}

// comment writes a keepalive the app ignores (it skips lines starting with ':').
func (s *sseWriter) comment(text string) error {
	return s.emit(": " + text + "\n")
}

// done writes the terminator the app breaks on.
func (s *sseWriter) done() error {
	return s.emit("data: [DONE]\n")
}

// emit writes and flushes one line under the lock. Write and Flush are a single
// unit on purpose: flushing between two writers' writes is how frames tear.
func (s *sseWriter) emit(line string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write([]byte(line)); err != nil {
		return err
	}
	return s.fc.Flush()
}

// nowUnix is time.Now().Unix, injected so tests can assert stable ids.
var nowUnix = func() int64 { return time.Now().Unix() }

// completionIDFor derives a bounded completion id from a conversation id.
//
// The conversation id is client-supplied and unbounded, so it is hashed rather
// than interpolated. Echoing it verbatim would put arbitrary client-controlled
// bytes into every frame of the stream; hashing keeps the id stable within a
// conversation (which is all the app uses it for) and short.
// completionIDFor hashes the conversation id rather than echoing it, so a
// client-supplied string cannot inject wire structure into an id field.
//
// The hash is deliberately STABLE, including for the empty id that every
// untracked turn shares. Two untracked turns therefore carry the same completion
// id, which looks like a collision and is not: nothing correlates completions
// server-side, the app keys its own state on X-Hermes-Session-Id, and a
// per-request id would only add a counter to keep in sync. The stable hash is
// the intended behaviour, not an oversight.
func completionIDFor(conversationID string) string {
	sum := sha256.Sum256([]byte(conversationID))
	return "chatcmpl-" + hex.EncodeToString(sum[:8])
}

// maxEchoedLen bounds client-controlled text reflected into an error body. The
// value is JSON-encoded and the caller is authenticated, but a 10 KB path segment
// would still produce a 10 KB error body; reflecting a slice of the input says
// everything the caller needs to know.
const maxEchoedLen = 128

// bounded trims s for safe inclusion in a response.
//
// Sliced by rune, not byte, for the same reason tail() is: a byte cut through a
// multi-byte character leaves a replacement character in the output. Path segments
// can carry one, and the point of a truncated echo is that it stays readable.
func bounded(s string) string {
	runes := []rune(s)
	if len(runes) <= maxEchoedLen {
		return s
	}
	return string(runes[:maxEchoedLen]) + "… (truncated)"
}

// newChunk builds a text or reasoning delta frame.
func newChunk(id, model string, content, reasoning string) chunk {
	return chunk{
		ID:      id,
		Object:  "chat.completion.chunk",
		Created: nowUnix(),
		Model:   model,
		Choices: []chunkChoice{{
			Index: 0,
			Delta: delta{Content: content, ReasoningContent: reasoning},
		}},
	}
}

// apiError writes the OpenAI error envelope with a status.
func apiError(w http.ResponseWriter, status int, kind, param, message string) {
	var resp errorResponse
	resp.Error.Message = message
	resp.Error.Type = kind
	resp.Error.Param = param
	resp.Error.Code = strconv.Itoa(status)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// toolStatus maps Pi's tool lifecycle events onto the status strings the app
// displays.
//
// An event type we do not recognise still reports "running", rather than being
// dropped: a Pi release that adds an event would otherwise make the tool vanish
// from the UI mid-execution. Callers must still require a real tool name — the
// app skips a progress frame it cannot attribute, and inventing a name is worse
// than silence.
func toolStatus(eventType string) string {
	switch eventType {
	case "tool_execution_start", "bash_execution_start":
		return "start"
	case "tool_execution_end", "bash_execution_end":
		return "end"
	case "tool_execution_update":
		return "update"
	}
	return "running"
}

// isToolEvent reports whether a record is a tool-progress event at all, so an
// unrelated event is never rendered as a tool.
func isToolEvent(eventType string) bool {
	switch eventType {
	case "tool_execution_start", "tool_execution_update", "tool_execution_end",
		"bash_execution_start", "bash_execution_end":
		return true
	}
	return false
}

// decodeMessageUpdate pulls the assistant message event out of a Pi
// message_update record, returning the delta text and its kind.
//
// Pi streams deltas only — cumulative snapshots are deliberately omitted from the
// wire — so text is accumulated from text_delta events alone.
func decodeMessageUpdate(rec pi.Record) (text, reasoning string, isDelta bool, ok bool) {
	var payload struct {
		AssistantMessageEvent struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		} `json:"assistantMessageEvent"`
	}
	if err := json.Unmarshal(rec.Raw, &payload); err != nil {
		return "", "", false, false
	}
	ev := payload.AssistantMessageEvent
	switch ev.Type {
	case "text_delta":
		return ev.Delta, "", true, true
	case "thinking_delta":
		return "", ev.Delta, true, true
	}
	return "", "", false, false
}

// decodeToolEvent extracts the tool name from a Pi tool event.
//
// Only `toolName` counts. Pi spells it that way on every tool event, and
// accepting a bare `tool` field as well would let any future event that happens
// to include one be rendered as a tool frame — attributing a tool the agent never
// ran. Better to show nothing than to show the wrong tool.
func decodeToolEvent(rec pi.Record) (name string, ok bool) {
	var payload struct {
		ToolName string `json:"toolName"`
	}
	if err := json.Unmarshal(rec.Raw, &payload); err != nil {
		return "", false
	}
	if payload.ToolName == "" {
		return "", false
	}
	return payload.ToolName, true
}

// decodeUsage extracts the cumulative usage Pi reports on message_update.
func decodeUsage(rec pi.Record) (piUsage, bool) {
	var payload struct {
		Usage *piUsage `json:"usage"`
	}
	if err := json.Unmarshal(rec.Raw, &payload); err != nil {
		return piUsage{}, false
	}
	if payload.Usage == nil {
		return piUsage{}, false
	}
	return *payload.Usage, true
}

// turn is the user content a single request contributes to Pi.
type turn struct {
	Text   string
	Images []piImage
	// SystemMessages counts system-role messages that were NOT applied.
	//
	// Only the trailing user turn is sent: Pi keeps conversation history in its own
	// session file, so replaying the whole transcript on every request would
	// duplicate context and cost tokens for nothing. System messages are dropped
	// along with everything else — personality lives in each profile's AGENTS.md,
	// not per request. They are counted rather than silently swallowed, so a
	// future client that starts sending them produces a log line instead of
	// quietly ineffective instructions.
	SystemMessages int
}

// lastUserTurn picks the message to send.
func lastUserTurn(msgs []chatMessage) (*turn, error) {
	out := &turn{}
	for _, m := range msgs {
		if strings.EqualFold(m.Role, "system") {
			out.SystemMessages++
		}
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		if !strings.EqualFold(msgs[i].Role, "user") {
			continue
		}
		// Trim only to decide BLANKNESS, and send the original text. Sending the
		// trimmed form would strip the leading indentation of pasted code and the
		// trailing newline a client sends after a multi-line paste, so the agent
		// would receive text the user never typed — and a Python or YAML block is
		// meaningless without its indentation.
		raw := msgs[i].Content.Text
		if strings.TrimSpace(raw) == "" && len(msgs[i].Content.Images) == 0 {
			return nil, errBlankTurn
		}
		out.Text = raw
		out.Images = msgs[i].Content.Images
		return out, nil
	}
	// No user turn anywhere in the request — a different problem from a blank one.
	return nil, errNoUserTurn
}
