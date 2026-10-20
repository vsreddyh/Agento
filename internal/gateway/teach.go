package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"agento/internal/teach"
)

// This file is the taught-skills surface (#314).
//
// A skill is a procedure the user taught the agent: draft → review/edit →
// test-fire → activate. Only active skills materialize to SKILL.md files for
// Pi's --skill flag, and only tested skills may activate or promote — the
// store enforces both, so the gate is not a UI hint. Every route resolves
// the profile from the URL like memory and treats a foreign id as missing
// (404, not 403).
//
//	GET    /api/skills                drafts + active + disabled for the profile
//	POST   /api/skills                create a draft (always draft)
//	PUT    /api/skills/{id}           edit name/description/body
//	DELETE /api/skills/{id}           delete (file drops on next reconcile)
//	POST   /api/skills/{id}/test      run the procedure once, record transcript
//	POST   /api/skills/{id}/activate  draft/disabled → active (needs a test)
//	POST   /api/skills/{id}/disable   active/draft → disabled
//	POST   /api/skills/{id}/promote   create a cron job running the procedure
//
// What the Skills tab shows is what Pi loads: v1/skills merges these rows
// with Pi's own skill list, and the materialized files are byte-identical to
// the rendered body the app reviewed.

// TeachRunner runs one prompt through the bare model client and returns its
// text. The cron worker's runner backs it: a test fire is the same shape as
// a scheduled fire, minus the schedule. Nil means no LLM runner is
// configured, and the test route answers 502 rather than faking a run.
type TeachRunner func(ctx context.Context, prompt, tag string) (string, error)

func teachWire(s teach.Skill) map[string]any {
	return map[string]any{
		"id":          s.ID,
		"profile":     s.Profile,
		"name":        s.Name,
		"description": s.Description,
		"body":        s.Body,
		"status":      string(s.Status),
		"source":      s.Source,
		"tested_at":   s.TestedAt,
		"test_text":   s.TestText,
		"created_at":  s.CreatedAt,
		"updated_at":  s.UpdatedAt,
	}
}

func teachError(w http.ResponseWriter, err error) {
	if se, ok := err.(*teach.StoreError); ok {
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

// teachCtx bounds one store call. The test route uses its own longer budget:
// it runs a model call, not a local read.
func teachCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 5*time.Second)
}

// maxSkillIDLen caps an id path the way runs and memory cap theirs:
// per-resource, so changing one never changes another's 404 contract.
const maxSkillIDLen = 64

// materialize reconciles the profile's runtime skill dir with its active
// skills. Best-effort and logged: a failed write must not fail the API call
// that triggered it, or a read-only filesystem would mask every skill edit
// as a 502. The next mutation (or gateway restart) retries.
func (s *Server) materialize(profile string) {
	if s.cfg.TeachMaterialize == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	skills, err := s.teach.List(ctx, profile)
	if err != nil {
		s.cfg.Logf("teach: list for materialize failed for %s: %v", profile, err)
		return
	}
	if _, err := s.cfg.TeachMaterialize(profile, skills); err != nil {
		s.cfg.Logf("teach: materialize failed for %s: %v", profile, err)
	}
}

func (s *Server) handleTeach(w http.ResponseWriter, r *http.Request, rest, profile string) {
	path := strings.TrimPrefix(rest, "api/skills")
	path = strings.TrimPrefix(path, "/")
	// Tolerate one trailing slash. Production routing already Cleaned it
	// (splitProfile), so this only fires for direct callers and tests.
	path = strings.TrimSuffix(path, "/")

	switch {
	case path == "":
		s.handleSkillCollection(w, r, profile)
		return
	case strings.HasSuffix(path, "/test"):
		s.handleSkillTest(w, r, profile, strings.TrimSuffix(path, "/test"))
		return
	case strings.HasSuffix(path, "/activate"):
		s.handleSkillStatus(w, r, profile, strings.TrimSuffix(path, "/activate"), teach.StatusActive, "activate")
		return
	case strings.HasSuffix(path, "/disable"):
		s.handleSkillStatus(w, r, profile, strings.TrimSuffix(path, "/disable"), teach.StatusDisabled, "disable")
		return
	case strings.HasSuffix(path, "/promote"):
		s.handleSkillPromote(w, r, profile, strings.TrimSuffix(path, "/promote"))
		return
	}

	// One skill, addressed by id.
	if strings.Contains(path, "/") || len(path) > maxSkillIDLen || strings.ContainsRune(path, 0) {
		apiError(w, http.StatusNotFound, "invalid_request_error", "", "unknown skill")
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.handleSkillUpdate(w, r, profile, path)
	case http.MethodDelete:
		ctx, cancel := teachCtx(r)
		defer cancel()
		if err := s.teach.Delete(ctx, path, profile); err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the skill store did not answer in time")
				return
			}
			teachError(w, err)
			return
		}
		s.materialize(profile)
		writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		apiError(w, http.StatusMethodNotAllowed, "invalid_request_error", "",
			"/api/skills/{id} accepts PUT or DELETE")
	}
}

func (s *Server) handleSkillCollection(w http.ResponseWriter, r *http.Request, profile string) {
	switch r.Method {
	case http.MethodGet:
		ctx, cancel := teachCtx(r)
		defer cancel()
		skills, err := s.teach.List(ctx, profile)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the skill store did not answer in time")
				return
			}
			teachError(w, err)
			return
		}
		out := make([]any, 0, len(skills))
		for _, sk := range skills {
			out = append(out, teachWire(sk))
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"skills": out})
		return
	case http.MethodPost:
		var body struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Body        string `json:"body"`
			Source      string `json:"source"`
		}
		if err := decodeJSONBody(w, r, &body); err != nil {
			return
		}
		ctx, cancel := teachCtx(r)
		defer cancel()
		sk, err := s.teach.Create(ctx, teach.Skill{
			Profile:     profile,
			Name:        body.Name,
			Description: body.Description,
			Body:        body.Body,
			Source:      strings.TrimSpace(body.Source),
		})
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the skill store did not answer in time")
				return
			}
			teachError(w, err)
			return
		}
		// Drafts do not materialize, but reconcile anyway: a rename race or
		// a failed earlier write converges here for free.
		s.materialize(profile)
		writeJSONResponse(w, http.StatusOK, map[string]any{"skill": teachWire(sk)})
		return
	default:
		w.Header().Set("Allow", "GET, POST")
		apiError(w, http.StatusMethodNotAllowed, "invalid_request_error", "",
			"/api/skills accepts GET or POST")
	}
}

func (s *Server) handleSkillUpdate(w http.ResponseWriter, r *http.Request, profile, id string) {
	var body struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Body        *string `json:"body"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		return
	}
	if body.Name == nil && body.Description == nil && body.Body == nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "",
			"nothing to update: send name, description or body")
		return
	}
	ctx, cancel := teachCtx(r)
	defer cancel()
	sk, err := s.teach.Update(ctx, id, profile, teach.Update{
		Name: body.Name, Description: body.Description, Body: body.Body,
	})
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			apiError(w, http.StatusBadGateway, "upstream_error", "",
				"the skill store did not answer in time")
			return
		}
		teachError(w, err)
		return
	}
	// An edit to an active skill changes what Pi loads: re-materialize.
	s.materialize(profile)
	writeJSONResponse(w, http.StatusOK, map[string]any{"skill": teachWire(sk)})
}

func (s *Server) handleSkillStatus(w http.ResponseWriter, r *http.Request, profile, id string, st teach.Status, verb string) {
	if !requireMethod(w, r, http.MethodPost, "/api/skills/{id}/"+verb) {
		return
	}
	ctx, cancel := teachCtx(r)
	defer cancel()
	sk, err := s.teach.SetStatus(ctx, id, profile, st)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			apiError(w, http.StatusBadGateway, "upstream_error", "",
				"the skill store did not answer in time")
			return
		}
		teachError(w, err)
		return
	}
	s.materialize(profile)
	writeJSONResponse(w, http.StatusOK, map[string]any{"skill": teachWire(sk)})
}

// handleSkillTest runs the skill's procedure once through the bare model
// client and records the transcript. It is a smoke test, honestly scoped:
// the runner has no MCP tools, so a pass means "the procedure reads
// coherently and the model follows it", not "it ran against real data".
// The transcript returns inline (the writer saw it) and stored clipped.
func (s *Server) handleSkillTest(w http.ResponseWriter, r *http.Request, profile, id string) {
	if !requireMethod(w, r, http.MethodPost, "/api/skills/{id}/test") {
		return
	}
	if s.cfg.TeachRunner == nil {
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"no model runner is configured for skill tests")
		return
	}
	// The input is optional, but a present-but-malformed one must not run
	// the model: read the body rather than trusting ContentLength, which
	// chunked requests declare as -1.
	var body struct {
		Input string `json:"input"`
	}
	if raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes)); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "",
			"could not read the request body")
		return
	} else if len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "",
				"could not parse the request body as JSON")
			return
		}
	}
	ctx, cancel := teachCtx(r)
	defer cancel()
	sk, err := s.teach.Get(ctx, id, profile)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			apiError(w, http.StatusBadGateway, "upstream_error", "",
				"the skill store did not answer in time")
			return
		}
		teachError(w, err)
		return
	}
	prompt := "Follow this procedure exactly. If a step needs data you do not have, say what is missing instead of inventing it.\n\n" +
		teach.RenderSKILL(sk)
	if input := strings.TrimSpace(body.Input); input != "" {
		prompt += "\n\nTest input:\n" + input
	}
	// The model call gets its own budget: 5s covers the store, not an LLM.
	runCtx, runCancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer runCancel()
	text, err := s.cfg.TeachRunner(runCtx, prompt, "agento-teach-"+sk.ID)
	if err != nil {
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"the test run failed: "+bounded(err.Error()))
		return
	}
	// Fresh budget, not the expired request ctx: the 120s model call above
	// outlived teachCtx's 5s, and reusing it would 502 every Mongo record
	// while memory (which ignores ctx) stayed green in tests.
	recCtx, recCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer recCancel()
	sk, err = s.teach.RecordTest(recCtx, id, profile, time.Now().UnixMilli(), text)
	if err != nil {
		teachError(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"skill": teachWire(sk), "output": text})
}

// handleSkillPromote creates a cron job running the skill's procedure on a
// schedule. Only tested, active skills promote: a draft has no reviewed
// procedure and a disabled one was deliberately stopped. The job's own
// pause and 10-run history then govern it like any other routine.
func (s *Server) handleSkillPromote(w http.ResponseWriter, r *http.Request, profile, id string) {
	if !requireMethod(w, r, http.MethodPost, "/api/skills/{id}/promote") {
		return
	}
	var body struct {
		Name     string `json:"name"`
		Schedule string `json:"schedule"`
		Delivery string `json:"delivery"`
	}
	if err := decodeJSONBody(w, r, &body); err != nil {
		return
	}
	ctx, cancel := teachCtx(r)
	defer cancel()
	// The skill gate first: whether the row may run on schedule does not
	// depend on gateway wiring, and a draft must 400 even where jobs are
	// unconfigured.
	sk, err := s.teach.Get(ctx, id, profile)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			apiError(w, http.StatusBadGateway, "upstream_error", "",
				"the skill store did not answer in time")
			return
		}
		teachError(w, err)
		return
	}
	if sk.Status != teach.StatusActive || sk.TestedAt == 0 {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "",
			"only a tested, active skill can run on schedule: review and test it first")
		return
	}
	if s.cfg.CronJobs == nil {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"jobs are not configured on this gateway")
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		name = sk.Name
	}
	prompt := "Routine \"" + sk.Name + "\" (taught skill, profile " + profile + "). " +
		"Follow the procedure below.\n\n" + teach.RenderSKILL(sk)
	job, err := s.cfg.CronJobs.Create(ctx, name, prompt, body.Schedule, body.Delivery)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "",
			bounded(err.Error()))
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"job": map[string]any{"id": job.ID, "name": job.Name, "schedule": job.Schedule},
	})
}
