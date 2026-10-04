package gateway

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
)

// The two session routes the app has been calling since #120.
//
// The app asks for `GET /{profile}/api/sessions/{id}/messages` (tool and skill names for
// the latest turn) and `GET /{profile}/api/sessions/{id}` (conversation totals). pi-gateway
// served neither, so both were 404 — and the app handles that by silently returning: the
// per-reply tool and skill chips simply never appeared, with no error anywhere. Hermes
// served these routes before the migration, which is why the app's KDoc still describes
// the response shapes.
//
// The mapping from a conversation to a file already exists and is not new work:
// SessionStore.Lookup answers "which pi session file backs this conversation id".
//
// What lives here is the translation of pi's on-disk session format into the shapes the
// app parses. pi writes JSONL; one record per entry, `type: "message"`, carrying a role,
// a content array, and — on assistant records — a `usage` object. Tool calls are
// `type: "toolCall"` parts holding the tool name and its arguments as a JSON *object*,
// and their results are separate `role: "toolResult"` messages carrying `toolName`.

// sessionBytesCap bounds how much of a session file is read. A conversation that ran for
// weeks would otherwise be read whole on every refresh, and the app only needs the tail
// for chips while totals need a running sum. Reading is sequential and stops at the cap,
// so a truncated read degrades totals rather than failing them — the same direction as
// every other bound in this package.
const sessionBytesCap = 8 << 20 // 8 MiB

// piRecord is one line of a pi session file. Only the fields this package reads are
// declared; the format carries more (sections, toolsAdded, parentId) that nothing here
// needs, and json.Unmarshal ignores the rest.
type piRecord struct {
	Type    string     `json:"type"`
	ModelID string     `json:"modelId"`
	Message *piMessage `json:"message"`
}

type piMessage struct {
	Role     string   `json:"role"`
	Content  []piPart `json:"content"`
	Usage    *piUsage `json:"usage"`
	ToolName string   `json:"toolName"`
	// ToolCallID names the toolCall this result answers, which is what lets a codemode
	// result be matched back to the codemode call that produced it.
	ToolCallID  string         `json:"toolCallId"`
	NestedCalls *piNestedCalls `json:"nestedCalls"`
}

// piNestedCalls is how pi reports the MCP calls made *inside* a codemode snippet.
//
// This matters more than it looks. pi does not expose each MCP tool to the model as its own
// callable; it exposes one tool, `codemode`, whose argument is a JavaScript snippet, and the
// MCP servers appear inside that snippet as `tools.mcp__<server>__<tool>()`. So the toolCall
// record says only "codemode", and a chip built from it would tell the user nothing.
//
// The real names are not lost — they are here, on the result, one entry per call actually
// made, with its own arguments, status and duration. That is strictly more than the row the
// app used to be sent under Hermes, so the translation is worth doing rather than accepting
// `codemode` as the answer.
//
// It is also what makes skill attribution work. The app finds skills by scanning a call's
// arguments for SKILL.md paths, and under codemode those paths sit in the *nested* call's
// arguments rather than inside the JS source of the wrapper.
type piNestedCalls struct {
	Calls []piNestedCall `json:"calls"`
	// Complete is false while pi is still running the snippet. A partial list is still the
	// right answer for a chip refresh, so it is read but not branched on.
	Complete bool `json:"complete"`
}

type piNestedCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Status    string          `json:"status"`
}

type piPart struct {
	Type string `json:"type"`
	Name string `json:"name"`
	// ID is how the result finds its call again, which is what makes the nested-call
	// expansion below possible.
	ID string `json:"id"`
	// Arguments is deliberately json.RawMessage: pi stores a tool call's arguments as an
	// object, and the app scans them as a *string* looking for SKILL.md paths. Re-encoding
	// is what turns one into the other, so the raw bytes are kept and marshalled on the
	// way out rather than decoded into a map here.
	Arguments json.RawMessage `json:"arguments"`
}

// pi's per-message usage is decoded by piUsage, which openai.go already declares for the
// streaming path — reused rather than redeclared, so the two cannot drift on a field name.

// sessionTotals is the subset the app's parser reads. Every field is omitempty because
// the parser treats an absent key as unknown rather than zero, and a hard 0 would be
// indistinguishable from a real measurement.
type sessionTotals struct {
	InputTokens      int64  `json:"input_tokens,omitempty"`
	OutputTokens     int64  `json:"output_tokens,omitempty"`
	CacheReadTokens  int64  `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int64  `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int64  `json:"reasoning_tokens,omitempty"`
	TotalTokens      int64  `json:"total_tokens,omitempty"`
	MessageCount     int64  `json:"message_count,omitempty"`
	Model            string `json:"model,omitempty"`
}

// appMessage is one row of the /messages array.
//
// Two shapes the app needs and pi does not use: an assistant row carries `tool_calls`
// with a nested `function` object, and a tool result is a `role: "tool"` row carrying
// `tool_name` on the row itself. `tool_calls` is omitted entirely when empty because the
// app skips a null array, and emitting an empty one for every text reply would be noise.
type appMessage struct {
	Role      string     `json:"role"`
	ToolCalls []toolCall `json:"tool_calls,omitempty"`
	ToolName  string     `json:"tool_name,omitempty"`
}

type toolCall struct {
	Function functionCall `json:"function"`
}

type functionCall struct {
	Name string `json:"name"`
	// A JSON *string* of the arguments. The app regex-scans this for SKILL.md paths to
	// attribute skills, so an object here would silently yield no skills.
	Arguments string `json:"arguments"`
}

// parsePiSession reads a pi session file into the two views the app needs.
//
// Both are returned from one pass because both come from the same records, and the file
// is the expensive part: a chip refresh and a totals refresh are two app calls, and
// reading the file twice for them would be needless.
func parsePiSession(path string) (messages []appMessage, totals sessionTotals, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, totals, err
	}
	defer f.Close()

	messages = []appMessage{}
	// toolCall id -> index of the assistant row carrying it, so a later result can replace
	// a `codemode` chip with the MCP tools the snippet actually ran. Bounded by the number
	// of calls in one session, which is small; a pi transcript holds far more text than
	// call ids.
	pending := map[string]int{}
	// A LimitReader is the bound: hitting it is a clean EOF, so a truncated read needs no
	// error path. The buffer is raised because a single line can be a whole tool result,
	// and bufio.Scanner's default 64 KiB cap would fail on one.
	sc := bufio.NewScanner(io.LimitReader(f, sessionBytesCap))
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var rec piRecord
		if err := json.Unmarshal(line, &rec); err != nil {
			// A line we cannot parse is skipped rather than fatal. A session file is
			// append-only and may end mid-write while a turn is streaming, so the tail
			// can legitimately be unparseable — and a partial transcript is still the
			// right answer for "what did this turn do".
			continue
		}
		if rec.Type == "model_change" && rec.ModelID != "" {
			totals.Model = rec.ModelID
			continue
		}
		if rec.Type != "message" || rec.Message == nil {
			continue
		}
		m := rec.Message

		// usage is per assistant message and represents that turn, so conversation
		// totals are the sum. cacheWrite is carried through because it is a real token
		// cost the app currently has nowhere else to read.
		if m.Usage != nil {
			totals.InputTokens += m.Usage.Input
			totals.OutputTokens += m.Usage.Output
			totals.CacheReadTokens += m.Usage.CacheRead
			totals.CacheWriteTokens += m.Usage.CacheWrite
			totals.ReasoningTokens += m.Usage.Reasoning
			totals.TotalTokens += m.Usage.Total
		}

		switch m.Role {
		case "user":
			messages = append(messages, appMessage{Role: "user"})
			totals.MessageCount++
		case "assistant":
			row := appMessage{Role: "assistant"}
			for _, p := range m.Content {
				// `thinking` parts are the model's own reasoning, not a tool the user
				// would recognise, so they are not calls.
				if p.Type != "toolCall" || p.Name == "" {
					continue
				}
				row.ToolCalls = append(row.ToolCalls, toolCall{
					Function: functionCall{Name: p.Name, Arguments: compactArgs(p.Arguments)},
				})
				// Remembered so the result below can find this call again and replace it
				// with the MCP tools it actually ran.
				if p.ID != "" {
					pending[p.ID] = len(messages)
				}
			}
			messages = append(messages, row)
			totals.MessageCount++
		case "toolResult":
			// A codemode result names the MCP tools it ran; a bash result does not. When the
			// names are there they replace the wrapper, because `codemode` tells the user
			// nothing about which tool ran — and the nested arguments are where SKILL.md
			// paths live, so this is also what makes skill attribution work.
			names := nestedNames(m)
			if len(names) == 0 {
				// No nested calls: the tool name on the result is the real one. A result
				// whose name is missing still counts as a row — dropping it would hide that
				// something ran.
				messages = append(messages, appMessage{Role: "tool", ToolName: m.ToolName})
				continue
			}
			if idx, ok := pending[m.ToolCallID]; ok {
				// Expand in place: one chip per MCP tool actually called, rather than a
				// single chip for the snippet that called them.
				messages[idx].ToolCalls = nil
				for _, n := range names {
					messages[idx].ToolCalls = append(messages[idx].ToolCalls, toolCall{
						Function: functionCall{Name: n.Name, Arguments: compactArgs(n.Arguments)},
					})
				}
			}
			for _, n := range names {
				messages = append(messages, appMessage{Role: "tool", ToolName: n.Name})
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, totals, err
	}
	// Total is what the app displays, and pi's own total can be zero on a record that
	// carries only partial usage. Deriving it is strictly better than reporting 0.
	if totals.TotalTokens == 0 {
		totals.TotalTokens = totals.InputTokens + totals.OutputTokens
	}
	return messages, totals, nil
}

// nestedNames returns the MCP tools a codemode result reports having called.
//
// Nil for anything else — a bash result has no nestedCalls, and neither does a plain tool —
// which is what keeps the wrapper name in place for tools that really are called directly.
func nestedNames(m *piMessage) []piNestedCall {
	if m.NestedCalls == nil || len(m.NestedCalls.Calls) == 0 {
		return nil
	}
	out := make([]piNestedCall, 0, len(m.NestedCalls.Calls))
	for _, c := range m.NestedCalls.Calls {
		if c.Name != "" {
			out = append(out, c)
		}
	}
	return out
}

// compactArgs renders a call's arguments as the compact JSON *string* the app scans.
//
// "{}" for anything unparseable rather than an error: the app treats a missing or odd
// argument string as "no arguments", and a chip is not worth failing a refresh over.
func compactArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return "{}"
	}
	return buf.String()
}

// handleSession serves both session routes, which differ only in what they return.
//
// An unknown conversation id is NOT an error. The app treats a missing session as
// "nothing to show" and falls back to its own device-side sums, so answering 404 here
// would turn a normal state — a thread id the gateway has not seen, a session evicted by
// the store's LRU bound — into a failure the app logs. The distinction the app does draw
// is transport/parse failure versus absent data, so absent data is a 200 with an empty
// result.
func (s *Server) handleSession(w http.ResponseWriter, h *agentHandler, rest string) {
	id, wantMessages, ok := sessionIDFromPath(rest)
	if !ok {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"expected /api/sessions/{id} or /api/sessions/{id}/messages")
		return
	}

	// The store is per profile, so a conversation id is only meaningful within the profile
	// that owns it — which is why this takes the handler rather than the server.
	file, found := h.sessions.Lookup(id)
	if !found {
		if wantMessages {
			// An empty array, not null: the app's rootArray helper treats a null body as
			// "unexpected response shape" and raises.
			writeJSONResponse(w, http.StatusOK, []appMessage{})
			return
		}
		// The app reads a non-empty `error` as "absent, not fatal" and falls back to
		// device sums — which is exactly right for an unknown conversation.
		writeJSONResponse(w, http.StatusOK, map[string]any{
			"error": "unknown session " + bounded(id),
		})
		return
	}

	messages, totals, err := parsePiSession(file)
	if err != nil {
		// A session file that cannot be read is a real failure — unlike an unknown id —
		// so it is reported as one rather than as absent data.
		apiError(w, http.StatusInternalServerError, "api_error", "",
			"reading session: "+bounded(err.Error()))
		return
	}

	if wantMessages {
		writeJSONResponse(w, http.StatusOK, messages)
		return
	}
	// Wrapped in `session`, with the bare object as the app's documented fallback. The
	// wrapper is what the app's KDoc records as the Hermes shape.
	writeJSONResponse(w, http.StatusOK, map[string]any{"session": totals})
}

// sessionIDFromPath extracts the conversation id from `api/sessions/{id}` (totals) or
// `api/sessions/{id}/messages` (the current turn's tool and skill names).
//
// The second return is which of the two was asked for, rather than a bool meaning "ok":
// deciding it here keeps the route match and the parse from disagreeing about the same
// string.
func sessionIDFromPath(rest string) (id string, wantMessages, ok bool) {
	const prefix = "api/sessions/"
	if !strings.HasPrefix(rest, prefix) {
		return "", false, false
	}
	tail := strings.TrimPrefix(rest, prefix)
	tail = strings.TrimSuffix(tail, "/")
	id, sub, _ := strings.Cut(tail, "/")
	// "." and ".." are refused because the id is joined onto a directory to reach the
	// file; a traversal there would read outside the session store.
	if id == "" || id == "." || id == ".." || strings.Contains(id, "/") {
		return "", false, false
	}
	// A further segment is not one of the two routes. Rejecting it keeps a typo'd URL
	// from being answered with a session the caller did not ask for.
	if sub != "" && sub != "messages" {
		return "", false, false
	}
	return id, sub == "messages", true
}
