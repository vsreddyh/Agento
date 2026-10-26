package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"agento/internal/pi"
	"agento/internal/runs"
)

// This file is the ask_user relay (#373): a mid-turn question the agent asks
// through Pi's extension UI sub-protocol, held by the gateway so it outlives
// the stream, and answered by the app.
//
// Pi blocks the extension's tool call until the client writes an
// extension_ui_response with the request's id on stdin. The gateway sits in
// the middle: it sees the request on the turn's event loop, publishes an
// `agento_question` frame into the live run, holds the pending question
// keyed by profile (the profile lock serialises turns, so one slot is
// enough), and writes the app's answer back to Pi with the request's id.
//
// The pending question outlives any one stream: a client that closed the app
// reattaching to the run is re-emitted the frame on subscribe (see
// streamRun), and the turn keeps waiting. Gateway restart kills the Pi child
// and the turn with it, so losing the question there is consistent —
// nothing is persisted, and the runs store never hears about questions.

// maxQuestionIDLen bounds the {id} segment of /api/questions/{id}. Pi
// request ids are short UUIDs; anything longer matches nothing. 404, not
// 400, like the run-id bound: an {id} names a thing that does not exist,
// and the response is the same as an unknown well-formed id.
const maxQuestionIDLen = 128

// Dialog methods Pi blocks on. Everything else arriving as
// extension_ui_request is either fire-and-forget (below) or a future
// addition answered cancelled so a turn never wedges on it.
var questionDialogMethods = map[string]bool{
	"select":  true,
	"input":   true,
	"confirm": true,
	"editor":  true,
}

// Fire-and-forget methods: Pi emits the request and does not expect a
// response, so the relay ignores them entirely — never held, never
// answered, never published.
var questionFireAndForget = map[string]bool{
	"notify":          true,
	"setStatus":       true,
	"setWidget":       true,
	"setTitle":        true,
	"set_editor_text": true,
}

// questionFrame is the SSE frame: the only new wire shape this feature adds.
// It is published into the live run (so every attached tab sees it) and
// re-emitted on subscribe (so a reattaching client gets it without a
// republish). It never enters the run's output: publish() does not touch
// the output builders, so the stored record and the transcript stay clean.
//
// Deliberately no retraction frame: a successful answer unblocks Pi and the
// turn keeps streaming, so a second attached tab learns the outcome from
// the turn's own continuation (and the run ending clears everything
// anyway). A stale card on a backgrounded tab lives at most until the turn
// settles — no extra protocol for a transient.
type questionFrame struct {
	Question questionWire `json:"agento_question"`
}

type questionWire struct {
	ID             string   `json:"id"`
	ConversationID string   `json:"conversation_id"`
	Method         string   `json:"method"`
	Title          string   `json:"title"`
	Options        []string `json:"options,omitempty"`
	Placeholder    string   `json:"placeholder,omitempty"`
	CreatedAt      int64    `json:"created_at"`
}

// pendingQuestion is one unanswered dialog, held between the request and
// the app's answer (or the turn's end, whichever comes first).
type pendingQuestion struct {
	id             string
	profile        string
	conversationID string
	method         string
	title          string
	options        []string
	placeholder    string
	createdAt      int64
	// answered closes once the app's answer (or the turn-end cancel) is
	// written to Pi. A second answer is a 409, never a second stdin write:
	// Pi already unblocked on the first, and answering into whatever the
	// turn is doing now would be answering a question nobody asked.
	answered bool
}

func (q *pendingQuestion) wire() questionWire {
	return questionWire{
		ID:             q.id,
		ConversationID: q.conversationID,
		Method:         q.method,
		Title:          q.title,
		Options:        append([]string(nil), q.options...),
		Placeholder:    q.placeholder,
		CreatedAt:      q.createdAt,
	}
}

// titleOf reads the card title. Pi's own rpc-types put the prompt in
// `title` for every dialog method (select/input/confirm/editor), but a
// future Pi could carry it in `message` (notify's field) instead — fall
// back rather than render "A question from the agent" for a question Pi
// actually sent.
func titleOf(req uiRequest) string {
	if req.Title != "" {
		return req.Title
	}
	return req.Message
}

// uiRequest is Pi's extension_ui_request record. Decoded from Raw like the
// other Pi event decoders: Data is empty on session events.
type uiRequest struct {
	ID          string
	Method      string
	Title       string
	Options     []string
	Message     string
	Placeholder string
}

func decodeExtensionUI(rec pi.Record) (uiRequest, bool) {
	var payload struct {
		ID          string   `json:"id"`
		Method      string   `json:"method"`
		Title       string   `json:"title"`
		Options     []string `json:"options"`
		Message     string   `json:"message"`
		Placeholder string   `json:"placeholder"`
	}
	if err := json.Unmarshal(rec.Raw, &payload); err != nil {
		return uiRequest{}, false
	}
	payload.ID = strings.TrimSpace(payload.ID)
	payload.Method = strings.TrimSpace(payload.Method)
	payload.Title = strings.TrimSpace(payload.Title)
	payload.Message = strings.TrimSpace(payload.Message)
	payload.Placeholder = strings.TrimSpace(payload.Placeholder)
	if payload.ID == "" || payload.Method == "" {
		return uiRequest{}, false
	}
	if len(payload.ID) > maxQuestionIDLen {
		return uiRequest{}, false
	}
	// Trimmed like every other dialog field: the select membership check
	// compares the app's trimmed value, so stored options must be trimmed
	// the same way or a card-offered value 400s on whitespace.
	for i, o := range payload.Options {
		payload.Options[i] = strings.TrimSpace(o)
	}
	return uiRequest{
		ID:          payload.ID,
		Method:      payload.Method,
		Title:       payload.Title,
		Options:     payload.Options,
		Message:     payload.Message,
		Placeholder: payload.Placeholder,
	}, true
}

// questionsOn registers the pending-question slot on the Server. Called from
// New alongside the runs registry.
type questionRegistry struct {
	mu      sync.Mutex
	pending map[string]*pendingQuestion
}

func newQuestionRegistry() *questionRegistry {
	return &questionRegistry{pending: map[string]*pendingQuestion{}}
}

// handleExtensionUI folds one extension_ui_request record into the turn.
// Called from the turn's event loop (stepRun), so it runs once per record.
func (s *Server) handleExtensionUI(lr *liveRun, rec pi.Record) {
	req, ok := decodeExtensionUI(rec)
	if !ok {
		// No id to answer with — nothing to hold, nothing to cancel.
		// Pi-side timeouts (when the extension sets one) bound this; the
		// turn's own budget bounds the rest.
		s.cfg.Logf("gateway: dropping undecodable extension_ui_request")
		return
	}
	if questionFireAndForget[req.Method] {
		// notify, setStatus, setWidget, setTitle, set_editor_text: Pi
		// does not wait on these, so there is nothing to hold and
		// nothing to answer — ignore them entirely.
		return
	}
	// Dialog methods the relay does not know fail fast rather than wedge
	// the turn on a card the app cannot render. The app renders select as
	// options and everything else (input, plus confirm/editor if a future
	// extension ever asks them) as a free-text field — see hasOptions.
	if !questionDialogMethods[req.Method] {
		s.respondCancelled(lr, req.ID)
		return
	}
	profile, conversation := lr.runProfile(), lr.runConversation()
	if lr.subscriberCount() == 0 {
		// No stream attached: nobody can see the card, so blocking would
		// burn the whole TurnTimeout. Scheduler fires land here (they never
		// subscribe) and fail the tool fast instead.
		s.cfg.Logf("gateway: cancelling %s question %s with no subscriber", req.Method, req.ID)
		s.respondCancelled(lr, req.ID)
		return
	}
	q := &pendingQuestion{
		id:             req.ID,
		profile:        profile,
		conversationID: conversation,
		method:         req.Method,
		title:          titleOf(req),
		options:        append([]string(nil), req.Options...),
		placeholder:    strings.TrimSpace(req.Placeholder),
		createdAt:      time.Now().UnixMilli(),
	}
	// One slot per profile: the lock serialises turns, so a second
	// registration means the previous turn leaked (or a future Pi asks
	// concurrently). Cancel the old slot first — silently overwriting
	// would orphan its id (answers 404 while Pi stays blocked on it).
	// The cancel runs unlocked: holding the registry lock across a Pi
	// write would wedge every other question path behind a slow child,
	// and the transient window (a POST for the new id 404s) is covered
	// by the app's restore-and-retry.
	s.questions.mu.Lock()
	if old, dupe := s.questions.pending[profile]; dupe && !old.answered {
		s.cfg.Logf("gateway: replacing unanswered %s question %s", old.method, old.id)
		old.answered = true
		s.questions.mu.Unlock()
		s.respondCancelled(lr, old.id)
		s.questions.mu.Lock()
	}
	s.questions.pending[profile] = q
	s.questions.mu.Unlock()
	lr.publish(questionFrame{Question: q.wire()}, false)
}

// pendingFor returns the profile's held question for a conversation, if
// any, as a wire-ready value copy: the caller (streamRun) renders off-lock,
// and a copy makes that obviously safe rather than reasoning about which
// fields mutate (only `answered` does, and `wire()` never reads it).
func (s *Server) pendingFor(profile, conversationID string) (questionWire, bool) {
	s.questions.mu.Lock()
	defer s.questions.mu.Unlock()
	q, ok := s.questions.pending[profile]
	if !ok || q.answered || q.conversationID != conversationID {
		return questionWire{}, false
	}
	return q.wire(), true
}

// respondCancelled answers a dialog with cancelled:true so Pi unwinds the
// blocked extension call. Best-effort: at turn teardown the child may
// already be gone, and the error would only replace the real one.
func (s *Server) respondCancelled(lr *liveRun, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := lr.agent.Respond(ctx, id, map[string]any{"cancelled": true}); err != nil {
		s.cfg.Logf("gateway: extension_ui_response cancelled for %s failed: %v", id, err)
	}
}

// cancelPendingQuestion clears the profile's held question at turn end and
// unblocks Pi. Called from executeRun's teardown: every exit (settle,
// timeout, abort, panic) funnels through it, so a question can never
// outlive its turn — and a later turn never inherits an older one's card.
func (s *Server) cancelPendingQuestion(lr *liveRun) {
	profile := lr.runProfile()
	// Everything under one lock: answerQuestion sets `answered` under the
	// same mutex, and a split read-then-write here would race an answer
	// landing between the lookup and the cancel (double stdin write, or a
	// cancel that resurrects into pendingFor). The Respond below runs
	// unlocked — only the map decision is critical.
	s.questions.mu.Lock()
	q, ok := s.questions.pending[profile]
	// Only the false→true transition responds: an answered question was
	// already unblocked by its answer, and a second write (cancelled)
	// would land in whatever the turn is doing next.
	shouldRespond := ok && !q.answered
	if ok {
		delete(s.questions.pending, profile)
		q.answered = true
	}
	s.questions.mu.Unlock()
	if !shouldRespond {
		return
	}
	s.respondCancelled(lr, q.id)
}

// answerQuestion writes the app's answer to Pi. Exactly once: a repeat is
// a 409, and a question gone with its turn is a 404.
func (s *Server) answerQuestion(profile, id, value string, cancelled bool) (status int) {
	s.questions.mu.Lock()
	q, ok := s.questions.pending[profile]
	if !ok || q.id != id || q.answered {
		// Distinguish "already answered" (409) from "unknown or gone"
		// (404): the former is a client bug worth naming, the latter is
		// the normal turn-ended case.
		if ok && q.id == id && q.answered {
			s.questions.mu.Unlock()
			return http.StatusConflict
		}
		s.questions.mu.Unlock()
		return http.StatusNotFound
	}
	// A select answer must be one of the offered options: the card only
	// ever sends those, so anything else is a buggy or hostile client
	// answering a question nobody asked with a value nobody offered.
	// Checked before marking answered, so a 400 leaves the slot exactly
	// as it was — retryable, unlike the answered states.
	if !cancelled && q.method == "select" && !slices.Contains(q.options, value) {
		s.questions.mu.Unlock()
		return http.StatusBadRequest
	}
	q.answered = true
	// Kept, not deleted: a repeat answer must 409 (not 404), and only
	// turn teardown clears the slot. pendingFor skips answered entries,
	// so the card never re-emits after an answer.
	s.questions.mu.Unlock()

	payload := map[string]any{}
	if cancelled {
		payload["cancelled"] = true
	} else {
		payload["value"] = value
	}
	agent := s.agentFor(profile)
	if agent == nil {
		// Defensive only (route() 404s unknown profiles): reset the
		// slot like a failed write, or every retry 409s forever.
		s.questions.mu.Lock()
		if q, ok := s.questions.pending[profile]; ok && q.id == id {
			q.answered = false
		}
		s.questions.mu.Unlock()
		return http.StatusNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := agent.Respond(ctx, id, payload); err != nil {
		s.cfg.Logf("gateway: extension_ui_response for %s failed: %v", id, err)
		// A failed write wrote nothing, so "exactly once" still holds:
		// reopen the slot under lock so the app can retry. Without this
		// the slot stays answered=true and every retry 409s forever.
		s.questions.mu.Lock()
		if q, ok := s.questions.pending[profile]; ok && q.id == id {
			q.answered = false
		}
		s.questions.mu.Unlock()
		return http.StatusBadGateway
	}
	return http.StatusOK
}

// agentFor returns the profile's agent for out-of-turn writes (answers).
// The route already 404s unknown profiles, so the handler always exists
// here; the lookup is what keeps the answer on the same agent the turn
// runs on. The ok-check is belt-and-braces against a future caller that
// does not come through route().
func (s *Server) agentFor(profile string) Agent {
	h, ok := s.handlers[profile]
	if !ok {
		return nil
	}
	return h.runner.agent
}

// handleQuestions serves POST /{profile}/api/questions/{id}.
func (s *Server) handleQuestions(w http.ResponseWriter, r *http.Request, rest, profile string) {
	path := strings.TrimPrefix(rest, "api/questions")
	path = strings.TrimPrefix(path, "/")
	id, extra, _ := strings.Cut(path, "/")
	extra = strings.TrimSuffix(extra, "/")
	if id == "" || extra != "" {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown path "+bounded(r.URL.Path))
		return
	}
	if len(id) > maxQuestionIDLen || strings.ContainsRune(id, 0) {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown question")
		return
	}
	if !requireMethod(w, r, http.MethodPost, "/api/questions/{id}") {
		return
	}
	var req struct {
		Value     string `json:"value"`
		Cancelled *bool  `json:"cancelled"`
	}
	if err := decodeJSONBody(w, r, &req); err != nil {
		return
	}
	cancelled := req.Cancelled != nil && *req.Cancelled
	value := strings.TrimSpace(req.Value)
	if !cancelled && value == "" {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "value",
			"value is required (or cancelled:true)")
		return
	}
	if cancelled && value != "" {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "value",
			"value and cancelled:true are mutually exclusive")
		return
	}
	if len([]rune(value)) > runs.MaxSteer {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "value",
			"value is too long (at most "+strconv.Itoa(runs.MaxSteer)+" characters)")
		return
	}
	switch s.answerQuestion(profile, id, value, cancelled) {
	case http.StatusOK:
		writeJSONResponse(w, http.StatusOK, map[string]any{"resolved": true})
	case http.StatusConflict:
		// 409 while a first write is still in flight: the loser drops
		// silently and the winner's outcome stands. Deliberately not a
		// retryable code — a retry would be a second stdin write, which is
		// exactly what "exactly once" forbids.
		apiError(w, http.StatusConflict, "invalid_request_error", "",
			"this question was already answered")
	case http.StatusBadRequest:
		apiError(w, http.StatusBadRequest, "invalid_request_error", "value",
			"not one of the offered options")
	case http.StatusBadGateway:
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"the agent did not answer in time")
	default:
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown question")
	}
}
