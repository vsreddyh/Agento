package gateway

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agento/internal/memory"
)

// This file is the memory surface (#315).
//
// Memory is per profile, not global: two profiles are two minds and must not
// leak each other's remembered facts. Every route below resolves the profile
// from the URL (default profile when unprefixed) and treats an item belonging
// to another profile as missing, so an id cannot be used to probe or edit
// across profiles.
//
//	GET    /api/memory                items + paused topics + topic list
//	POST   /api/memory                create (user-written, confirmed)
//	PUT    /api/memory/{id}           edit text/topic/status
//	DELETE /api/memory/{id}           delete one item
//	POST   /api/memory/forget         delete every item for the profile
//	PUT    /api/memory/topics/{topic} pause or resume one topic
//
// What these routes show is what the next turn is given: injection reads the
// same store and applies the same confirmed-unpaused rule.

// Memory ids are 24-hex ObjectIDs; the cap only keeps a garbage path from
// becoming a store query. Per-resource on purpose: changing the runs cap must
// not silently change this 404 contract (same rule as approvals).
const maxMemoryIDLen = 64

func memoryWire(it memory.Item) map[string]any {
	return map[string]any{
		"id":         it.ID,
		"profile":    it.Profile,
		"topic":      it.Topic,
		"text":       it.Text,
		"source":     string(it.Source),
		"status":     string(it.Status),
		"created_at": it.CreatedAt,
		"updated_at": it.UpdatedAt,
	}
}

func memoryError(w http.ResponseWriter, err error) {
	if se, ok := err.(*memory.StoreError); ok {
		switch {
		case se.NotFound:
			apiError(w, http.StatusNotFound, "invalid_request_error", "", bounded(se.Msg))
		case se.Upstream:
			apiError(w, http.StatusBadGateway, "upstream_error", "", bounded(se.Msg))
		default:
			apiError(w, http.StatusBadRequest, "invalid_request_error", "", bounded(se.Msg))
		}
		return
	}
	apiError(w, http.StatusBadRequest, "invalid_request_error", "", bounded(err.Error()))
}

// memoryCtx bounds the request's store calls: memory is a local read, not a
// model turn. One deadline covers both calls (list, then paused topics) on
// purpose: separate budgets would only move the same 5s around, and the
// failure modes are already split — a list failure 502s, a paused failure
// degrades with a warning.
func memoryCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 5*time.Second)
}

func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request, rest, profile string) {
	path := strings.TrimPrefix(rest, "api/memory")
	path = strings.TrimPrefix(path, "/")
	// Tolerate one trailing slash, like the runs verbs: /api/memory/forget/
	// is forget, not an id lookup for "forget/".
	path = strings.TrimSuffix(path, "/")

	switch {
	case path == "":
		s.handleMemoryCollection(w, r, profile)
		return
	case path == "forget":
		if !requireMethod(w, r, http.MethodPost, "/api/memory/forget") {
			return
		}
		ctx, cancel := memoryCtx(r)
		defer cancel()
		n, err := s.memory.DeleteAll(ctx, profile)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the memory store did not answer in time")
				return
			}
			memoryError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "deleted": n})
		return
	case strings.HasPrefix(path, "topics/"):
		topic := strings.TrimPrefix(path, "topics/")
		if topic == "" || strings.Contains(topic, "/") {
			apiError(w, http.StatusNotFound, "invalid_request_error", "",
				"unknown path "+bounded(r.URL.Path))
			return
		}
		if !requireMethod(w, r, http.MethodPut, "/api/memory/topics/{topic}") {
			return
		}
		var body struct {
			Paused *bool `json:"paused"`
		}
		if err := decodeJSONBody(w, r, &body); err != nil {
			return
		}
		if body.Paused == nil {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "paused",
				"paused is required (true pauses, false resumes)")
			return
		}
		// A typo must not pause the default topic the user never named:
		// the same UsableTopic rule as item writes.
		if !memory.UsableTopic(topic) {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "topic",
				memory.TopicError(topic))
			return
		}
		ctx, cancel := memoryCtx(r)
		defer cancel()
		t := memory.NormalizeTopic(topic)
		if err := s.memory.SetTopicPaused(ctx, profile, t, *body.Paused); err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the memory store did not answer in time")
				return
			}
			memoryError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{
			"topic": t, "paused": *body.Paused,
		})
		return
	}

	// One item, addressed by id.
	if strings.Contains(path, "/") || len(path) > maxMemoryIDLen || strings.ContainsRune(path, 0) {
		apiError(w, http.StatusNotFound, "invalid_request_error", "", "unknown memory")
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.handleMemoryUpdate(w, r, profile, path)
	case http.MethodDelete:
		ctx, cancel := memoryCtx(r)
		defer cancel()
		// One profile-scoped query authorises and deletes; a foreign id is
		// NotFound, indistinguishable from a missing one.
		if err := s.memory.Delete(ctx, path, profile); err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the memory store did not answer in time")
				return
			}
			memoryError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		apiError(w, http.StatusMethodNotAllowed, "invalid_request_error", "",
			"/api/memory/{id} accepts PUT or DELETE")
	}
}

func (s *Server) handleMemoryCollection(w http.ResponseWriter, r *http.Request, profile string) {
	switch r.Method {
	case http.MethodGet:
		f := memory.Filter{Profile: profile}
		// Statuses are exact lowercase: topics normalise, enums do not, so
		// ?status=Confirmed is 400, not "confirmed".
		if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
			st := memory.Status(raw)
			if !st.Valid() {
				apiError(w, http.StatusBadRequest, "invalid_request_error", "status",
					"unknown memory status "+bounded(raw))
				return
			}
			f.Status = st
		}
		// Filters forgive where writes reject: ?topic=--- reads as general
		// with 200, while POST/PUT 400 the same shape. A filter names what
		// to show, so normalising is harmless; a write names where to store,
		// so silence would re-categorise.
		if raw := strings.TrimSpace(r.URL.Query().Get("topic")); raw != "" {
			f.Topic = memory.NormalizeTopic(raw)
		}
		// limit=0 means the default (ListCap here), not "no rows": 0 is the
		// Go zero value for "unspecified", and the store maps <=0 to the cap.
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				apiError(w, http.StatusBadRequest, "invalid_request_error", "limit",
					"limit must be a non-negative number")
				return
			}
			f.Limit = n
		}
		ctx, cancel := memoryCtx(r)
		defer cancel()
		// One list, filtered in memory: the status/topic filter and the
		// screen's topic list are two views of the same rows, and three
		// sequential calls under one deadline can 502 the third after
		// doing the first two. The full ListCap read is by choice, not
		// accident: topics must stay complete under any filter, and at 200
		// rows it is cheap. If the cap ever grows, bound this query first.
		all, err := s.memory.List(ctx, memory.Filter{Profile: profile})
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the memory store did not answer in time")
				return
			}
			memoryError(w, err)
			return
		}
		paused, err := s.memory.PausedTopics(ctx, profile)
		var warnings []string
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the memory store did not answer in time")
				return
			}
			// Display degrades; injection does not. A paused read failure
			// must never steer a turn (memoryPrompt returns ""), but the
			// screen showing items with pauses unknown beats showing
			// nothing, and the warning says which it is.
			paused = []string{}
			warnings = []string{"paused topics unavailable; all topics shown as unpaused"}
			s.cfg.Logf("memory: paused topics degraded for %s: %v", profile, err)
		}
		items := memoryFilter(all, f)
		topics := memoryTopics(all, paused)
		out := make([]any, 0, len(items))
		for _, it := range items {
			out = append(out, memoryWire(it))
		}
		resp := map[string]any{
			"items":  out,
			"paused": paused,
			"topics": topics,
		}
		if warnings != nil {
			resp["warnings"] = warnings
		}
		writeJSONResponse(w, http.StatusOK, resp)
		return
	case http.MethodPost:
		var body struct {
			Topic string `json:"topic"`
			Text  string `json:"text"`
		}
		if err := decodeJSONBody(w, r, &body); err != nil {
			return
		}
		if !memory.UsableTopic(body.Topic) {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "topic",
				memory.TopicError(body.Topic))
			return
		}
		trimmed := strings.TrimSpace(body.Text)
		text := memory.CleanText(body.Text)
		if text == "" {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "text",
				"text is required")
			return
		}
		ctx, cancel := memoryCtx(r)
		defer cancel()
		it, err := s.memory.Create(ctx, memory.Item{
			Profile: profile,
			Topic:   memory.NormalizeTopic(body.Topic),
			Text:    text,
			Source:  memory.SourceUser,
			// A user wrote it: it is confirmed, no approval loop for the
			// person whose memory it is.
			Status: memory.StatusConfirmed,
		})
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the memory store did not answer in time")
				return
			}
			memoryError(w, err)
			return
		}
		// Same contract as save_memory: a pasted paragraph clips at MaxText
		// runes, and the screen should say so rather than show a 200 that
		// quietly stored less.
		writeJSONResponse(w, http.StatusOK, map[string]any{
			"item":      memoryWire(it),
			"truncated": text != trimmed,
		})
		return
	default:
		w.Header().Set("Allow", "GET, POST")
		apiError(w, http.StatusMethodNotAllowed, "invalid_request_error", "",
			"/api/memory accepts GET or POST")
	}
}

func (s *Server) handleMemoryUpdate(w http.ResponseWriter, r *http.Request, profile, id string) {
	var body struct {
		Topic  *string `json:"topic"`
		Text   *string `json:"text"`
		Status *string `json:"status"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		return
	}
	upd := memory.Update{}
	if body.Topic != nil {
		if !memory.UsableTopic(*body.Topic) {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "topic",
				memory.TopicError(*body.Topic))
			return
		}
		upd.Topic = body.Topic
	}
	// trimmedText feeds the truncation signal in the response: an edit
	// pasting past MaxText clips like a create, and the writer should know.
	trimmedText := ""
	if body.Text != nil {
		trimmedText = strings.TrimSpace(*body.Text)
		text := memory.CleanText(*body.Text)
		if text == "" {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "text",
				"text must not be empty")
			return
		}
		upd.Text = &text
	}
	if body.Status != nil {
		st := memory.Status(*body.Status)
		if !st.Valid() {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "status",
				"unknown memory status "+bounded(*body.Status))
			return
		}
		upd.Status = &st
	}
	if upd.Text == nil && upd.Topic == nil && upd.Status == nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "",
			"nothing to update: send text, topic or status")
		return
	}
	ctx, cancel := memoryCtx(r)
	defer cancel()
	it, err := s.memory.Update(ctx, id, profile, upd)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			apiError(w, http.StatusBadGateway, "upstream_error", "",
				"the memory store did not answer in time")
			return
		}
		memoryError(w, err)
		return
	}
	// Same truncated contract as POST and save_memory.
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"item":      memoryWire(it),
		"truncated": upd.Text != nil && it.Text != trimmedText,
	})
}

// memoryFilter applies the collection's status/topic/limit views to one
// store list. Limit <= 0 means the store cap, matching List.
func memoryFilter(all []memory.Item, f memory.Filter) []memory.Item {
	limit := f.Limit
	if limit <= 0 || limit > memory.ListCap {
		limit = memory.ListCap
	}
	out := make([]memory.Item, 0, len(all))
	for _, it := range all {
		if t := strings.TrimSpace(f.Topic); t != "" && it.Topic != memory.NormalizeTopic(t) {
			continue
		}
		if f.Status != "" && it.Status != f.Status {
			continue
		}
		out = append(out, it)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// memoryTopics unions item topics with paused ones: paused first, then the
// items newest-first. Paused-first is deliberate — the screen lists what it
// can pause — not alphabetical, so do not "fix" the order.
// The screen shows every topic it can pause, including one whose items were
// just filtered out.
func memoryTopics(items []memory.Item, paused []string) []string {
	seen := make(map[string]bool, len(items)+len(paused))
	out := make([]string, 0, len(items)+len(paused))
	add := func(t string) {
		t = memory.NormalizeTopic(t)
		if seen[t] {
			return
		}
		seen[t] = true
		out = append(out, t)
	}
	for _, t := range paused {
		add(t)
	}
	for _, it := range items {
		add(it.Topic)
	}
	return out
}

// memoryPrompt returns the block a turn should carry, or "" when there is
// none. A store failure is logged and treated as "no memory": a context
// lookup must never fail the user's turn. A paused-topic read failure also
// returns "": injecting a paused topic would override an explicit choice,
// while skipping the block for one turn is recoverable.
//
// The timeout is short on purpose. This runs before Pi is prompted, so it is
// on the turn's critical path; memory is context, not a prerequisite.
func (s *Server) memoryPrompt(ctx context.Context, profile string) string {
	if s.memory == nil || strings.TrimSpace(profile) == "" {
		return ""
	}
	// One 2s deadline covers both calls on purpose, like memoryCtx: a slow
	// List starves the paused read and the turn carries no block, which is
	// the fail-closed outcome ADR-0003 requires (never inject past a pause).
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	// ListCap, not PromptItems: the pause filter runs after the list, so a
	// limit applied first would let 30 paused items starve unpaused ones.
	// PromptBlock enforces its own item and byte caps.
	items, err := s.memory.List(ctx, memory.Filter{
		Profile: profile,
		Status:  memory.StatusConfirmed,
		Limit:   memory.ListCap,
	})
	if err != nil {
		s.cfg.Logf("memory: prompt load failed for %s: %v", profile, err)
		return ""
	}
	if len(items) == 0 {
		return ""
	}
	paused, err := s.memory.PausedTopics(ctx, profile)
	if err != nil {
		s.cfg.Logf("memory: paused topics load failed for %s: %v", profile, err)
		return ""
	}
	return memory.PromptBlock(items, paused)
}
