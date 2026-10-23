package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"agento/internal/connectors"
)

// This file is the connectors surface (#316, v1 foundation).
//
// Connectors are deployment-wide (unlike memory/teach): one status per
// connector id, no profile split. The app sees status + scopes only —
// refresh tokens live server-side in Mongo and never appear here.
//
//	GET  /api/connectors              list the three v1 rows
//	POST /api/connectors/{id}/disconnect   stop future access (tokens cleared)
//	POST /api/connectors/{id}/connect      v1: 501 until OAuth lands; the app
//	                                     treats it as "needs setup", never as
//	                                     a prompt to paste a secret in chat
//
// An expired row is the reconnect prompt: the app banners it and offers the
// connect flow, never a password field.

// maxConnectorIDLen bounds the id segment. Ids are short fixed strings;
// anything longer matches nothing. Same no-oracle 404 contract as runs.
const maxConnectorIDLen = 64

func connectorWire(c connectors.Connector) map[string]any {
	scopes := c.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	out := map[string]any{
		"id":           c.ID,
		"display_name": c.DisplayName,
		"scopes":       scopes,
		"status":       string(c.Status),
	}
	if c.LastError != "" {
		out["last_error"] = c.LastError
	}
	// Emitted only while connected: the stored value is last-connected-at
	// and is retained across disconnect/expire, so emitting it on an
	// off-state row would read as "connected since X". Absence of
	// updated_at means never touched (see DefaultRow).
	if c.Status == connectors.StatusConnected && c.ConnectedAt != 0 {
		out["connected_at"] = c.ConnectedAt
	}
	if c.UpdatedAt != 0 {
		out["updated_at"] = c.UpdatedAt
	}
	// Reconnect hint for the app: expired rows banner, nothing else does.
	if c.Status == connectors.StatusExpired {
		out["reconnect"] = "this session expired; reconnect in Settings — never paste a password or token in chat"
	}
	return out
}

func connectorsError(w http.ResponseWriter, err error) {
	var se *connectors.StoreError
	if errors.As(err, &se) {
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

func connectorCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 5*time.Second)
}

// handleConnectors serves /api/connectors. Global like approvals, not
// per-profile like memory.
func (s *Server) handleConnectors(w http.ResponseWriter, r *http.Request, rest string) {
	path := strings.TrimPrefix(rest, "api/connectors")
	path = strings.TrimPrefix(path, "/")
	// Belt-and-braces: splitProfile already ran path.Clean, which strips
	// trailing slashes, so this rarely fires — but a doubly-slashed client
	// path reaching here as "gmail/disconnect/" would otherwise 404 as an
	// unknown action instead of hitting the verb.
	path = strings.TrimSuffix(path, "/")

	if path == "" {
		if !requireMethod(w, r, http.MethodGet, "/api/connectors") {
			return
		}
		ctx, cancel := connectorCtx(r)
		defer cancel()
		list, err := s.connectors.List(ctx)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the connector store did not answer in time")
				return
			}
			connectorsError(w, err)
			return
		}
		out := make([]any, 0, len(list))
		for _, c := range list {
			out = append(out, connectorWire(c))
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"connectors": out})
		return
	}

	id, action, _ := strings.Cut(path, "/")
	if id == "" || strings.ContainsRune(id, 0) {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown connector")
		return
	}
	// Normalise before Known: " GMail " addresses gmail (case and spacing
	// fold), while a truly unknown id still 404s. The length guard measures
	// the normalised form — what Known actually sees — so padding cannot
	// turn a known id unknown.
	norm := connectors.NormalizedID(id)
	if len(norm) > maxConnectorIDLen {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown connector")
		return
	}
	if !connectors.Known(norm) {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown connector "+bounded(norm))
		return
	}

	switch action {
	case "disconnect":
		if !requireMethod(w, r, http.MethodPost, "/api/connectors/{id}/disconnect") {
			return
		}
		ctx, cancel := connectorCtx(r)
		defer cancel()
		c, err := s.connectors.Disconnect(ctx, norm)
		if err != nil {
			if ctx.Err() == context.DeadlineExceeded {
				apiError(w, http.StatusBadGateway, "upstream_error", "",
					"the connector store did not answer in time")
				return
			}
			connectorsError(w, err)
			return
		}
		writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "connector": connectorWire(c)})
		return
	case "connect":
		if !requireMethod(w, r, http.MethodPost, "/api/connectors/{id}/connect") {
			return
		}
		// v1 foundation: OAuth is not wired yet, so connect cannot succeed.
		// 501 (not 400): the request is fine, the server side is not built.
		// The app treats this as "needs setup" and keeps the row
		// disconnected — it must never fall back to asking for a secret in
		// chat.
		apiError(w, http.StatusNotImplemented, "invalid_request_error", "",
			"connect for "+bounded(norm)+" is not configured on this server yet; OAuth lands in a follow-up — never paste a token in chat")
		return
	case "":
		w.Header().Set("Allow", "POST")
		apiError(w, http.StatusMethodNotAllowed, "invalid_request_error", "",
			"no verb on /api/connectors/{id}; use /disconnect or /connect")
		return
	}
	apiError(w, http.StatusNotFound, "invalid_request_error", "",
		"unknown path "+bounded(r.URL.Path))
}
