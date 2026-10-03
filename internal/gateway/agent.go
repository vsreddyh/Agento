// Package gateway serves the OpenAI-compatible HTTP surface the Agento Android
// app already speaks, backed by a Pi agent process.
//
// The app is not modified by this migration. It builds requests as
// "$serverUrl$profilePath/v1/...", sends `Authorization: Bearer <PASSWORD>` and
// `X-Hermes-Session-Id`, and parses a hand-rolled SSE stream. Everything here
// exists to satisfy that contract exactly:
//
//	POST /{profile}/v1/chat/completions   text/event-stream, [DONE] terminated
//	GET  /healthz                          liveness, unauthenticated
//
// The profile is the first path segment because the proxy strips the /p/pi prefix
// before forwarding; the app's per-tab profile path is what selects the agent.
//
// SSE frame shapes the app understands, all as `data: <json>` lines:
//
//	{"choices":[{"delta":{"content":"...","reasoning_content":"..."}}]}
//	{"tool":"bash","label":"...","status":"start"}       tool progress
//	{"usage":{...}}                                     token counts, last wins
//	{"error":"..."}                                     terminal, no [DONE]
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"agento/internal/pi"
)

// Agent is the subset of the Pi RPC client this package needs.
//
// Narrowed to an interface so the HTTP surface can be tested against a fake with
// no Pi binary and no model provider. Anything that needs the process lifecycle
// itself (readiness, exit diagnostics) is the caller's problem, not this one's.
type Agent interface {
	// Call sends a command and waits for its correlated response.
	Call(ctx context.Context, command string, payload map[string]any) (*pi.Record, error)
	// Send writes a command without waiting for a response.
	Send(ctx context.Context, command string, payload map[string]any) error
	// Subscribe returns a stream of session events plus a cancel function.
	Subscribe() (<-chan pi.Record, func())
	// Stderr returns diagnostics from the agent process.
	Stderr() string
}

// SessionStore remembers which Pi session file backs each conversation id.
//
// The app already sends a conversation id in X-Hermes-Session-Id and expects a
// conversation to resume when it repeats one. Pi identifies sessions by file, so
// the mapping between the two is this type's whole job.
type SessionStore struct {
	mu sync.Mutex
	// byID maps a conversation id to the Pi session file backing it.
	byID map[string]string
	// claimed guards against two conversation ids claiming the same Pi session,
	// which would interleave two conversations into one transcript.
	claimed map[string]string
	// order is the least-recently-used end first.
	order []string
	// cap bounds the store. A gateway is long-lived and every fresh
	// X-Hermes-Session-Id would otherwise be a permanent entry — unbounded growth
	// driven entirely by client-supplied headers.
	cap int
}

// defaultSessionCap bounds how many conversations one profile tracks. The app has
// a handful of tabs; a client cycling random ids must not be able to grow this
// without limit.
const defaultSessionCap = 256

func NewSessionStore() *SessionStore {
	return &SessionStore{
		byID:    make(map[string]string),
		claimed: make(map[string]string),
		cap:     defaultSessionCap,
	}
}

// Lookup returns the Pi session file for a conversation id, marking it as most
// recently used.
func (s *SessionStore) Lookup(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, ok := s.byID[id]
	if ok {
		s.touch(id)
	}
	return f, ok
}

// touch moves id to the most-recently-used end. Caller holds mu.
func (s *SessionStore) touch(id string) {
	for i, existing := range s.order {
		if existing == id {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
	s.order = append(s.order, id)
}

// evictLocked drops the least-recently-used conversations until the store is
// within cap. Caller holds mu.
//
// Evicting only forgets the mapping; the Pi session file itself is untouched, so
// a conversation the client returns to starts a fresh session rather than
// resuming. That is the right trade: bounding memory matters more than resuming
// a conversation older than the cap, and the client can still send a fresh id.
func (s *SessionStore) evictLocked() {
	for len(s.byID) > s.cap && len(s.order) > 0 {
		victim := s.order[0]
		s.order = s.order[1:]
		if file, ok := s.byID[victim]; ok {
			delete(s.claimed, file)
			delete(s.byID, victim)
		}
	}
}

// Remember binds a conversation id to a Pi session file.
//
// Re-binding an id to a new session is allowed — the app starts a new
// conversation and reuses nothing — but claiming a file already owned by a
// different id is refused, because two conversations sharing a transcript would
// corrupt both.
func (s *SessionStore) Remember(id, sessionFile string) error {
	if id == "" || sessionFile == "" {
		return fmt.Errorf("gateway: session id and file are both required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if owner, taken := s.claimed[sessionFile]; taken && owner != id {
		return fmt.Errorf("gateway: session %s already belongs to conversation %s", sessionFile, owner)
	}
	if prev, ok := s.byID[id]; ok && prev != sessionFile {
		// The id is moving to a new session; release the old claim so a later
		// conversation can adopt it rather than being refused forever.
		delete(s.claimed, prev)
	}
	s.byID[id] = sessionFile
	s.claimed[sessionFile] = id
	s.touch(id)
	s.evictLocked()
	return nil
}

// ClaimedBy reports which conversation owns a Pi session file.
func (s *SessionStore) ClaimedBy(sessionFile string) (string, bool) {
	if sessionFile == "" {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.claimed[sessionFile]
	return id, ok
}

// Forget drops a conversation, used when its Pi session dies.
func (s *SessionStore) Forget(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f, ok := s.byID[id]; ok {
		delete(s.claimed, f)
		delete(s.byID, id)
		for i, existing := range s.order {
			if existing == id {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
}

// state is the subset of Pi's get_state response this package reads.
//
// The field names are Pi's, not ours: get_state returns sessionId and
// sessionFile, and that pair is what makes the app's session header work.
type state struct {
	SessionID     string `json:"sessionId"`
	SessionFile   string `json:"sessionFile"`
	ThinkingLevel string `json:"thinkingLevel"`
	Model         struct {
		ID       string `json:"id"`
		Provider string `json:"provider"`
		API      string `json:"api"`
	} `json:"model"`
}

func (a agentRunner) getState(ctx context.Context) (*state, error) {
	rec, err := a.agent.Call(ctx, "get_state", nil)
	if err != nil {
		return nil, err
	}
	var st state
	if len(rec.Data) > 0 {
		if err := json.Unmarshal(rec.Data, &st); err != nil {
			return nil, fmt.Errorf("gateway: decode get_state: %w", err)
		}
	}
	return &st, nil
}

// agentRunner couples an Agent with its session store.
type agentRunner struct {
	agent    Agent
	sessions *SessionStore
}

// resolveSession makes the Pi session match the conversation the app asked for.
//
// Pi sessions are files, and switching is explicit. On a first request the
// current session is claimed for the conversation; on a repeat request the stored
// file is switched back to, which is what makes a conversation resume with its
// history intact.
// It also returns the agent state it read along the way, so a caller can report
// the model that is actually answering without a second RPC. That matters because
// the client's own `model` field is only a claim: the gateway never selects on it,
// Pi's model is fixed when the process starts. Echoing the claim would be both
// unbounded and wrong.
func (a agentRunner) resolveSession(ctx context.Context, conversationID string) (*state, error) {
	// No conversation id means an untracked turn. Use whatever session is current.
	//
	// Deliberately no "no session file" guard here, unlike the tracked path below.
	// The tracked path needs a non-empty file because Remember() has to store one;
	// an untracked turn stores nothing, so an empty file is inert. The caller
	// discards this return value entirely. Adding the guard for symmetry would
	// turn a working untracked turn into a 502 over a value nothing reads.
	if conversationID == "" {
		st, err := a.getState(ctx)
		if err != nil {
			return nil, err
		}
		return st, nil
	}

	if file, ok := a.sessions.Lookup(conversationID); ok {
		// Already ours; make sure Pi is actually on it, since another request may
		// have switched the shared process in the meantime.
		st, err := a.getState(ctx)
		if err != nil {
			return nil, err
		}
		if st.SessionFile == file {
			return st, nil
		}
		if _, err := a.agent.Call(ctx, "switch_session", map[string]any{"sessionPath": file}); err != nil {
			a.sessions.Forget(conversationID)
			return nil, fmt.Errorf("gateway: resume conversation: %w", err)
		}
		// The state read above now describes the session Pi has LEFT, not the one
		// it switched to. This is not a theoretical staleness: switch_session
		// assigns a FRESH sessionId even for the file just requested (measured
		// against real Pi: e07f... -> e2cb... on an unchanged path), so returning
		// the pre-switch struct would hand the caller another session's id.
		//
		// One extra local IPC round trip on a resumed turn, which is every turn
		// after the first in a conversation. It costs no model call and no tokens,
		// so correctness wins outright over avoiding it.
		resumed, err := a.getState(ctx)
		if err != nil {
			// The switch succeeded, so dropping the mapping here would strand a
			// conversation that is in fact resumable. Keep it; the next turn retries.
			return nil, fmt.Errorf("gateway: read state after resume: %w", err)
		}
		return resumed, nil
	}

	// First sighting of this conversation. Pi holds ONE current session per
	// process, and it belongs to whichever conversation claimed it first. Two
	// conversations sharing a transcript would interleave, so when the live
	// session is already owned this starts a fresh one rather than stealing it.
	st, err := a.getState(ctx)
	if err != nil {
		return nil, err
	}
	if st.SessionFile == "" {
		return nil, fmt.Errorf("gateway: agent reported no session file")
	}

	if owner, taken := a.sessions.ClaimedBy(st.SessionFile); taken && owner != conversationID {
		st, err = a.startFreshSession(ctx, st.SessionFile)
		if err != nil {
			return nil, err
		}
	}

	if err := a.sessions.Remember(conversationID, st.SessionFile); err != nil {
		// Check-then-claim is a race: another first-seen conversation can claim
		// this session between ClaimedBy and Remember. Per-profile turns are
		// serialised, but two conversations still arrive on separate requests, so
		// rather than surfacing the conflict as a 502, start a fresh session and
		// claim that.
		st, err = a.startFreshSession(ctx, st.SessionFile)
		if err != nil {
			return nil, err
		}
		if err := a.sessions.Remember(conversationID, st.SessionFile); err != nil {
			return nil, err
		}
	}
	return st, nil
}

// startFreshSession asks Pi for a new session and returns its state, verifying the
// session actually changed — a no-op new_session must not be mistaken for success,
// or the caller would re-claim the session it was trying to escape.
func (a agentRunner) startFreshSession(ctx context.Context, previous string) (*state, error) {
	if _, err := a.agent.Call(ctx, "new_session", nil); err != nil {
		return nil, fmt.Errorf("gateway: start a session for a new conversation: %w", err)
	}
	fresh, err := a.getState(ctx)
	if err != nil {
		return nil, err
	}
	if fresh.SessionFile == "" || fresh.SessionFile == previous {
		return nil, fmt.Errorf("gateway: agent did not start a new session")
	}
	return fresh, nil
}

// levels is the set of thinking levels a model supports, learned from Pi rather
// than hardcoded.
type levelSet map[string]bool

func (l levelSet) has(level string) bool { return l[level] }

func (a agentRunner) thinkingLevels(ctx context.Context) (levelSet, error) {
	rec, err := a.agent.Call(ctx, "get_available_thinking_levels", nil)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Levels []string `json:"levels"`
	}
	if len(rec.Data) > 0 {
		if err := json.Unmarshal(rec.Data, &payload); err != nil {
			return nil, fmt.Errorf("gateway: decode thinking levels: %w", err)
		}
	}
	set := make(levelSet, len(payload.Levels))
	for _, s := range payload.Levels {
		set[s] = true
	}
	return set, nil
}

// effortAliases maps the spellings the app and older configs use onto Pi's own
// level names. Anything not listed must appear in the model's advertised levels
// or the request is refused — Pi itself accepts an unknown level silently, which
// would make a typo look like it worked.
var effortAliases = map[string]string{
	// "none" is what older app settings call it; "off" is Pi's own name for the
	// same level. Both must be accepted — rejecting Pi's own spelling would fail
	// the most obvious value a caller could send.
	"off":     "off",
	"none":    "off",
	"minimal": "minimal",
	"low":     "low",
	"medium":  "medium",
	"high":    "high",
	"xhigh":   "xhigh",
	"max":     "max",
}

// Effort validation failures, as sentinels.
//
// The caller needs to tell "you typed something we do not recognise" from "that
// level is not available on this model", because only the second can list what
// IS valid. Comparing error strings for that distinction would break silently the
// first time either message is reworded.
var (
	errEffortEmpty       = errors.New("reasoning_effort is empty")
	errEffortUnknown     = errors.New("reasoning_effort is not a recognised level")
	errEffortUnsupported = errors.New("reasoning_effort is not supported by this model")
)

// resolveEffort validates model_options.reasoning_effort against the levels the
// current model actually supports, returning the Pi level name.
//
// A recognised level the model does not advertise is refused rather than clamped:
// clamping would silently run at a different reasoning depth than requested, which
// is the failure mode this whole check exists to prevent.
func resolveEffort(effort string, supported levelSet) (string, error) {
	normalised := strings.ToLower(strings.TrimSpace(effort))
	if normalised == "" {
		return "", errEffortEmpty
	}
	candidate, ok := effortAliases[normalised]
	if !ok {
		// Not one of the known spellings, but a level the model advertises is a
		// level Pi will accept. Forward compatibility matters more than a closed
		// list: a new Pi level should work the day it ships, not the day someone
		// remembers to add it here.
		if supported.has(normalised) {
			return normalised, nil
		}
		return "", errEffortUnknown
	}
	if !supported.has(candidate) {
		return "", errEffortUnsupported
	}
	return candidate, nil
}

// supportedList renders a level set for error messages.
//
// Canonical levels come first, in Pi's ladder order, then anything else the model
// advertised — resolveEffort accepts those too, so a hint that omitted them would
// tell the caller a level is unavailable when it is exactly the one being asked
// about.
func supportedList(set levelSet) string {
	order := []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}
	var out []string
	seen := make(map[string]bool, len(set))
	for _, l := range order {
		if set.has(l) {
			out = append(out, l)
			seen[l] = true
		}
	}
	extras := make([]string, 0)
	for l := range set {
		if !seen[l] {
			extras = append(extras, l)
		}
	}
	sort.Strings(extras)
	return strings.Join(append(out, extras...), ", ")
}

// StderrTailLimit caps how much agent stderr reaches the log.
//
// An agent that has wedged can emit megabytes, and this is logged per failed turn,
// so an unbounded tail turns one bad agent into an unbounded log. 2000 characters
// is enough to see the last error and not enough to matter.
const StderrTailLimit = 2000

// Tail returns the END of s, which is where an error message is.
//
// The opposite end from bounded(): for stderr the recent lines are the useful ones,
// whereas bounded() keeps the start of a reflected path where the caller is. Sliced
// by rune for the same reason — a byte cut through a multi-byte character leaves a
// replacement character, and stderr routinely carries non-ASCII from tool output.
func Tail(s string, limit int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= limit {
		return s
	}
	return "..." + string(runes[len(runes)-limit:])
}
