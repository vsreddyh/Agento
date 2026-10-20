package gateway

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"agento/internal/approvals"
)

// maxApprovalIDLen bounds the id segment of /api/approvals/{id}. Approvals are
// ObjectIDs (24 hex); anything longer matches nothing. Separate from
// maxRunIDLen even though both are 64 today: they bound different resources,
// and changing one must not silently change the other's 404 contract.
const maxApprovalIDLen = 64

// This file is the approval-gate half of the chat surface (#313).
//
// The relay (cmd/mcp-gate) holds sensitive tool calls as pending approvals;
// this API is how the app lists them, decides them, and reads the audit
// trail. The gateway keeps no live state here — decisions are store writes
// the relay polls for, so any gateway process serves them identically.
//
//	GET  /api/approvals            list, filterable by status
//	POST /api/approvals/{id}/approve | deny
//	GET  /api/audit                list, filterable by server/tool

func approvalWire(a approvals.Approval) map[string]any {
	out := map[string]any{
		"id":         a.ID,
		"server":     a.Server,
		"tool":       a.Tool,
		"status":     string(a.Status),
		"created_at": a.CreatedAt,
		"updated_at": a.UpdatedAt,
	}
	if len(a.Args) > 0 {
		out["args"] = a.Args
	} else {
		out["args"] = map[string]any{}
	}
	if a.DecidedBy != "" {
		out["decided_by"] = a.DecidedBy
	}
	if a.DecidedAt != 0 {
		out["decided_at"] = a.DecidedAt
	}
	return out
}

func auditWire(e approvals.AuditEntry) map[string]any {
	out := map[string]any{
		"id":          e.ID,
		"server":      e.Server,
		"tool":        e.Tool,
		"disposition": string(e.Disposition),
		"at":          e.At,
	}
	if len(e.Args) > 0 {
		out["args"] = e.Args
	} else {
		out["args"] = map[string]any{}
	}
	if e.Error != "" {
		out["error"] = e.Error
	}
	if e.ApprovalID != "" {
		out["approval_id"] = e.ApprovalID
	}
	return out
}

func approvalsError(w http.ResponseWriter, err error) {
	if se, ok := err.(*approvals.StoreError); ok {
		switch {
		case se.NotFound:
			apiError(w, http.StatusNotFound, "invalid_request_error", "", bounded(se.Msg))
		case se.Conflict:
			apiError(w, http.StatusConflict, "invalid_request_error", "", bounded(se.Msg))
		case se.Upstream:
			apiError(w, http.StatusBadGateway, "upstream_error", "", bounded(se.Msg))
		default:
			apiError(w, http.StatusBadRequest, "invalid_request_error", "", bounded(se.Msg))
		}
		return
	}
	apiError(w, http.StatusBadRequest, "invalid_request_error", "", bounded(err.Error()))
}

// handleApprovals serves /api/approvals. Reached with and without a profile
// segment, like /api/runs: approvals are global, with no per-profile split.
func (s *Server) handleApprovals(w http.ResponseWriter, r *http.Request, rest string) {
	path := strings.TrimPrefix(rest, "api/approvals")
	path = strings.TrimPrefix(path, "/")

	if path == "" {
		if !requireMethod(w, r, http.MethodGet, "/api/approvals") {
			return
		}
		f := approvals.ApprovalFilter{}
		if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
			st := approvals.Status(raw)
			if !st.Valid() {
				apiError(w, http.StatusBadRequest, "invalid_request_error", "status",
					"unknown approval status "+bounded(raw))
				return
			}
			f.Status = st
		}
		// limit=0 is accepted and means the store's cap, exactly like
		// omitting it: both stores read a non-positive limit that way, so
		// rejecting 0 here would be the API disagreeing with its backends.
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 {
				apiError(w, http.StatusBadRequest, "invalid_request_error", "limit",
					"limit must be a non-negative number (0 means the cap)")
				return
			}
			f.Limit = n
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		list, err := s.approvals.ListApprovals(ctx, f)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the approval store did not answer in time")
				return
			}
			approvalsError(w, err)
			return
		}
		out := make([]any, 0, len(list))
		for _, a := range list {
			out = append(out, approvalWire(a))
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"approvals": out})
		return
	}

	id, action, _ := strings.Cut(path, "/")
	if id == "" {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown path "+bounded(r.URL.Path))
		return
	}
	// Approval ids are ObjectIDs; anything longer matches nothing. Same
	// no-oracle 404 contract as the runs routes.
	if len(id) > maxApprovalIDLen || strings.ContainsRune(id, 0) {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown approval")
		return
	}

	switch action {
	case "approve", "deny":
		if !requireMethod(w, r, http.MethodPost, "/api/approvals/{id}/"+action) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		a, err := s.approvals.DecideApproval(ctx, id, action == "approve", "app", time.Now())
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the approval store did not answer in time")
				return
			}
			approvalsError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "status": string(a.Status)})
		return
	case "":
		apiError(w, http.StatusMethodNotAllowed, "invalid_request_error", "",
			"no verb on /api/approvals/{id}; decide via /api/approvals/{id}/approve or /deny")
		return
	}
	apiError(w, http.StatusNotFound, "invalid_request_error", "",
		"unknown path "+bounded(r.URL.Path))
}

// handleAudit serves /api/audit: the read-only trail of every proxied call.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request, rest string) {
	path := strings.TrimPrefix(rest, "api/audit")
	path = strings.TrimPrefix(path, "/")
	if path != "" {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown path "+bounded(r.URL.Path))
		return
	}
	if !requireMethod(w, r, http.MethodGet, "/api/audit") {
		return
	}
	filter := approvals.AuditFilter{
		Server: strings.TrimSpace(r.URL.Query().Get("server")),
		Tool:   strings.TrimSpace(r.URL.Query().Get("tool")),
	}
	// Same contract as /api/approvals: 0 means the store's cap.
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			apiError(w, http.StatusBadRequest, "invalid_request_error", "limit",
				"limit must be a non-negative number (0 means the cap)")
			return
		}
		filter.Limit = n
	}
	if len([]rune(filter.Server)) > maxFilterLen || len([]rune(filter.Tool)) > maxFilterLen {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "server",
			"filter values must be at most 256 characters")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	list, err := s.approvals.ListAudit(ctx, filter)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			apiError(w, http.StatusBadGateway, "upstream_error", "",
				"the approval store did not answer in time")
			return
		}
		approvalsError(w, err)
		return
	}
	out := make([]any, 0, len(list))
	for _, e := range list {
		out = append(out, auditWire(e))
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"entries": out})
}
