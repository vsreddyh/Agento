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

// memoryCtx bounds one store call: memory is a local read, not a model turn.
func memoryCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 5*time.Second)
}

// ownedMemory reads one item and hides a foreign profile's row behind the
// same 404 as a missing id. Memory ids are not secrets, but "exists but is
// not yours" is a fact the API has no reason to confirm.
func (s *Server) ownedMemory(ctx context.Context, id, profile string) (memory.Item, error) {
	it, err := s.memory.Get(ctx, id)
	if err != nil {
		return memory.Item{}, err
	}
	if it.Profile != profile {
		return memory.Item{}, &memory.StoreError{Msg: "unknown memory " + id, NotFound: true}
	}
	return it, nil
}

func (s *Server) handleMemory(w http.ResponseWriter, r *http.Request, rest, profile string) {
	path := strings.TrimPrefix(rest, "api/memory")
	path = strings.TrimPrefix(path, "/")

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
	if strings.Contains(path, "/") || len(path) > maxRunIDLen || strings.ContainsRune(path, 0) {
		apiError(w, http.StatusNotFound, "invalid_request_error", "", "unknown memory")
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.handleMemoryUpdate(w, r, profile, path)
	case http.MethodDelete:
		ctx, cancel := memoryCtx(r)
		defer cancel()
		if _, err := s.ownedMemory(ctx, path, profile); err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the memory store did not answer in time")
				return
			}
			memoryError(w, err)
			return
		}
		if err := s.memory.Delete(ctx, path); err != nil {
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
		if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
			st := memory.Status(raw)
			if !st.Valid() {
				apiError(w, http.StatusBadRequest, "invalid_request_error", "status",
					"unknown memory status "+bounded(raw))
				return
			}
			f.Status = st
		}
		if raw := strings.TrimSpace(r.URL.Query().Get("topic")); raw != "" {
			f.Topic = memory.NormalizeTopic(raw)
		}
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
		items, err := s.memory.List(ctx, f)
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
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the memory store did not answer in time")
				return
			}
			memoryError(w, err)
			return
		}
		// Topic list is independent of the status filter: pausing a topic with
		// no currently-visible item must still be possible from the screen.
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
		topics := memoryTopics(all, paused)
		out := make([]any, 0, len(items))
		for _, it := range items {
			out = append(out, memoryWire(it))
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{
			"items":  out,
			"paused": paused,
			"topics": topics,
		})
		return
	case http.MethodPost:
		var body struct {
			Topic string `json:"topic"`
			Text  string `json:"text"`
		}
		if err := decodeJSONBody(w, r, &body); err != nil {
			return
		}
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
		writeJSONResponse(w, http.StatusOK, map[string]any{"item": memoryWire(it)})
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
	upd := memory.Update{Topic: body.Topic}
	if body.Text != nil {
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
	if _, err := s.ownedMemory(ctx, id, profile); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			apiError(w, http.StatusBadGateway, "upstream_error", "",
				"the memory store did not answer in time")
			return
		}
		memoryError(w, err)
		return
	}
	it, err := s.memory.Update(ctx, id, upd)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			apiError(w, http.StatusBadGateway, "upstream_error", "",
				"the memory store did not answer in time")
			return
		}
		memoryError(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"item": memoryWire(it)})
}

// memoryTopics unions item topics with paused ones, sorted by first seen.
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
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	items, err := s.memory.List(ctx, memory.Filter{
		Profile: profile,
		Status:  memory.StatusConfirmed,
		Limit:   memory.PromptItems,
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
