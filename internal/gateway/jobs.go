package gateway

import (
	"context"
	"net/http"
	"strings"
	"time"

	"agento/internal/cronjobs"
)

// JobStore is the cron job backend. *cronjobs.Store implements it; tests
// use an in-memory fake so no MongoDB is needed.
type JobStore interface {
	List(ctx context.Context) ([]cronjobs.Job, error)
	Create(ctx context.Context, name, prompt, schedule, delivery string) (cronjobs.Job, error)
	Update(ctx context.Context, id string, name, prompt, schedule, delivery *string, paused *bool) (cronjobs.Job, error)
	Delete(ctx context.Context, id string) error
	RecordRun(ctx context.Context, id string, run cronjobs.Run, fireInstant int64) error
}

// CronFire fires one job now (manual /run and the tick worker share it).
type CronFire func(ctx context.Context, id string) (string, error)

// dayTime formats an instant the way the app displays it: numeric and
// locale-free, since the phone renders the string verbatim.
func dayTime(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).In(cronjobs.IST).Format("2006-01-02 15:04")
}

func jobJSON(j cronjobs.Job, now time.Time) map[string]any {
	runs := make([]any, 0, len(j.Runs))
	for _, r := range j.Runs {
		runs = append(runs, map[string]any{
			"at": r.At, "text": r.Text, "missed": r.Missed,
		})
	}
	var lastRun string
	if n := len(j.Runs); n > 0 {
		lastRun = dayTime(j.Runs[n-1].At)
	}
	var nextRun string
	if !j.Paused {
		if sched, err := cronjobs.Parse(j.Schedule); err == nil {
			if next := sched.Next(now); !next.IsZero() {
				nextRun = next.Format("2006-01-02 15:04")
			}
		}
	}
	return map[string]any{
		"id": j.ID, "name": j.Name, "prompt": j.Prompt,
		"schedule": j.Schedule, "paused": j.Paused, "delivery": j.Delivery,
		"last_run": lastRun, "next_run": nextRun, "runs": runs,
	}
}

// handleJobs serves the Scheduler's jobs surface. Reached two ways —
// /{profile}/api/jobs/... and unprefixed /api/jobs/... (splitProfile
// reserves "api") — so the profile segment is irrelevant and jobs are
// global to the single chat profile. Everything here sits behind the
// bearer token like every other non-health route.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request, rest string) {
	if s.cfg.CronJobs == nil || s.cfg.CronFire == nil {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"jobs are not configured on this gateway")
		return
	}
	path := strings.TrimPrefix(rest, "api/jobs")
	path = strings.TrimPrefix(path, "/")

	switch {
	case path == "" && r.Method == http.MethodGet:
		jobs, err := s.cfg.CronJobs.List(r.Context())
		if err != nil {
			apiError(w, http.StatusBadGateway, "upstream_error", "",
				"job list failed: "+bounded(err.Error()))
			return
		}
		now := time.Now()
		out := make([]any, 0, len(jobs))
		for _, j := range jobs {
			out = append(out, jobJSON(j, now))
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"jobs": out})
		return
	case path == "" && r.Method == http.MethodPost:
		var req struct {
			Name     string `json:"name"`
			Prompt   string `json:"prompt"`
			Schedule string `json:"schedule"`
			Deliver  string `json:"deliver"`
			Delivery string `json:"delivery"`
		}
		if err := decodeJSONBody(w, r, &req); err != nil {
			return
		}
		if req.Delivery == "" {
			req.Delivery = req.Deliver
		}
		job, err := s.cfg.CronJobs.Create(r.Context(),
			strings.TrimSpace(req.Name), req.Prompt, req.Schedule, req.Delivery)
		if err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "",
				bounded(err.Error()))
			return
		}
		writeJSONResponse(w, http.StatusOK, jobJSON(job, time.Now()))
		return
	}

	id, action, _ := strings.Cut(path, "/")
	if id == "" {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown path "+bounded(r.URL.Path))
		return
	}

	switch action {
	case "pause", "resume":
		if !requireMethod(w, r, http.MethodPost, "/api/jobs/{id}/"+action) {
			return
		}
		paused := action == "pause"
		job, err := s.cfg.CronJobs.Update(r.Context(), id, nil, nil, nil, nil, &paused)
		if err != nil {
			jobsError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusOK, jobJSON(job, time.Now()))
		return
	case "run":
		if !requireMethod(w, r, http.MethodPost, "/api/jobs/{id}/run") {
			return
		}
		text, err := s.cfg.CronFire(r.Context(), id)
		if err != nil {
			jobsError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "response": text})
		return
	case "":
		switch r.Method {
		case http.MethodPatch:
			var req struct {
				Name     *string `json:"name"`
				Prompt   *string `json:"prompt"`
				Schedule *string `json:"schedule"`
				Deliver  *string `json:"deliver"`
				Delivery *string `json:"delivery"`
				Paused   *bool   `json:"paused"`
			}
			if err := decodeJSONBody(w, r, &req); err != nil {
				return
			}
			delivery := req.Delivery
			if delivery == nil {
				delivery = req.Deliver
			}
			job, err := s.cfg.CronJobs.Update(r.Context(), id,
				req.Name, req.Prompt, req.Schedule, delivery, req.Paused)
			if err != nil {
				jobsError(w, err)
				return
			}
			writeJSONResponse(w, http.StatusOK, jobJSON(job, time.Now()))
			return
		case http.MethodDelete:
			if err := s.cfg.CronJobs.Delete(r.Context(), id); err != nil {
				jobsError(w, err)
				return
			}
			writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
	}
	apiError(w, http.StatusNotFound, "invalid_request_error", "",
		"unknown path "+bounded(r.URL.Path))
}

func jobsError(w http.ResponseWriter, err error) {
	if se, ok := err.(*cronjobs.StoreError); ok && se.NotFound {
		apiError(w, http.StatusNotFound, "invalid_request_error", "", bounded(se.Msg))
		return
	}
	apiError(w, http.StatusBadRequest, "invalid_request_error", "", bounded(err.Error()))
}
