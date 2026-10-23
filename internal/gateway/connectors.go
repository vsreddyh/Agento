package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode"

	"agento/internal/approvals"
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

// hasControl reports whether s contains any control byte. NUL, \n, \r and
// friends are never valid in an id or verb; rejecting the whole class (not
// just NUL) keeps echoed 404s and log lines break-free.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, unicode.IsControl)
}

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
	// Emitted only while connected with a valid stamp: the stored value is
	// last-connected-at and is retained across disconnect/expire, so
	// emitting it on an off-state row would read as "connected since X".
	// A zero stamp on a connected row (corrupt) omits too — fail-closed
	// rather than a per-response now that drifts without persisting.
	// Absence of updated_at means never touched (see DefaultRow).
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
		// Scrubbed in every branch, including NotFound: that message
		// carries caller-influenced text ("unknown connector "+id), so it
		// is the one branch that most needs it. Contract for future
		// StoreError construction: Msg must be safe OR get scrubbed here —
		// this switch scrubs, so callers don't have to.
		msg := bounded(approvals.ScrubError(se.Msg))
		switch {
		case se.NotFound:
			apiError(w, http.StatusNotFound, "invalid_request_error", "", msg)
		case se.Upstream:
			apiError(w, http.StatusBadGateway, "upstream_error", "", msg)
		default:
			apiError(w, http.StatusBadRequest, "invalid_request_error", "", msg)
		}
		return
	}
	apiError(w, http.StatusBadRequest, "invalid_request_error", "", bounded(approvals.ScrubError(err.Error())))
}

func connectorCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 5*time.Second)
}

// storeTimeoutOrGone maps a store failure onto the timeout contract both
// verbs share: store didn't answer (deadline) → 502; client went away first
// (any other ctx error) → logged distinctly so triage doesn't read a hangup
// as a store failure, then falls through to the domain error (the write
// fails silently on a dead conn either way). Returns true when it answered.
func storeTimeoutOrGone(w http.ResponseWriter, s *Server, ctx context.Context, err error, verb string) bool {
	if ctx.Err() == context.DeadlineExceeded {
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"the connector store did not answer in time")
		return true
	}
	if ctx.Err() != nil {
		// Observe-only: writing a domain error on a dead connection only
		// buys a second log line at the http layer for a hangup. Log and
		// return without a body.
		s.cfg.Logf("connectors: %s aborted, client gone: %v", verb, ctx.Err())
		return true
	}
	connectorsError(w, err)
	return true
}

// serveConnectorsList answers GET /api/connectors (and the trailing-slash
// alias): the three v1 rows, disconnected synthesised on fresh deploys.
// One helper so the two spellings cannot drift apart.
func (s *Server) serveConnectorsList(w http.ResponseWriter, r *http.Request) {
	if !requireMethod(w, r, http.MethodGet, "/api/connectors") {
		return
	}
	ctx, cancel := connectorCtx(r)
	defer cancel()
	list, err := s.connectors.List(ctx)
	if err != nil {
		storeTimeoutOrGone(w, s, ctx, err, "list")
		return
	}
	out := make([]any, 0, len(list))
	for _, c := range list {
		out = append(out, connectorWire(c))
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"connectors": out})
}

// handleConnectors serves /api/connectors. Global like approvals, not
// per-profile like memory.
//
// Budget note for the next writer (OAuth follow-up starts here): verbs fan
// out to quickCtx-budgeted pre-reads + readBack around one verb-ctx write
// (~15s worst case, no overall deadline, detached from request cancel by
// design — see quickCtx). Do NOT copy that stacking for heavier writes or
// the real token exchange; bind those to one verb-level deadline instead.
func (s *Server) handleConnectors(w http.ResponseWriter, r *http.Request, rest string) {
	// Exact match for the collection, prefix-with-slash for members:
	// "api/connectorsfoo" is an unknown path, not connector "foo" — the
	// prefix form without a slash boundary would mislabel the resource and
	// send triage after a bad id instead of a bad path.
	if rest == "api/connectors" {
		s.serveConnectorsList(w, r)
		return
	}
	if !strings.HasPrefix(rest, "api/connectors/") {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown path "+bounded(approvals.ScrubError(rest)))
		return
	}
	path := strings.TrimPrefix(rest, "api/connectors/")
	// Belt-and-braces: splitProfile already ran path.Clean, which strips
	// trailing slashes, so this rarely fires — but a doubly-slashed client
	// path reaching here as "gmail/disconnect/" would otherwise 404 as an
	// unknown action instead of hitting the verb.
	path = strings.TrimSuffix(path, "/")

	if path == "" {
		// "api/connectors/" with trailing slash: the collection, same as
		// the exact form above — one list shape, one helper.
		s.serveConnectorsList(w, r)
		return
	}

	id, action, _ := strings.Cut(path, "/")
	// Control bytes checked on the RAW segments, before any trim: TrimSpace
	// strips \n/\r/\t, so checking after folding would turn " \n " into ""
	// (no-verb 405) while the id path rejects the same bytes with a bare
	// 404. Same bare echoless 404 for both — JSON-escaping keeps the app
	// safe, but line-oriented log tailing must never see an injected break,
	// and no valid id or verb contains one.
	if id == "" || hasControl(id) || hasControl(action) {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown connector")
		return
	}
	// Verbs fold like ids: "Gmail/Disconnect" from a hand test lands on the
	// verb instead of 404ing as an unknown path. Trimmed like the id — a
	// padded verb is client noise, not a different resource. Length-capped
	// like the id: bounded() keeps any echo safe and the server caps total
	// header size anyway, but there is no reason to pay rune-scan + alloc
	// on a garbage path before the inevitable 404.
	action = strings.ToLower(strings.TrimSpace(action))
	if len(action) > maxConnectorIDLen {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown path "+bounded(approvals.ScrubError(path)))
		return
	}
	// Normalise before Known: " GMail " addresses gmail (case and spacing
	// fold), while a truly unknown id still 404s. The length guard measures
	// the normalised form — what Known actually sees — so padding cannot
	// turn a known id unknown.
	norm := connectors.NormalizedID(id)
	if norm == "" {
		// Empty after folding (" "): same bare 404 as the NUL path above,
		// not the trailing-space echo the unknown branch would emit.
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown connector")
		return
	}
	if len(norm) > maxConnectorIDLen {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown connector")
		return
	}
	if !connectors.Known(norm) {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown connector "+bounded(approvals.ScrubError(norm)))
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
			storeTimeoutOrGone(w, s, ctx, err, "disconnect")
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
			"connect for "+bounded(approvals.ScrubError(norm))+" is not configured on this server yet; OAuth lands in a follow-up — never paste a token in chat")
		return
	case "":
		w.Header().Set("Allow", "POST")
		apiError(w, http.StatusMethodNotAllowed, "invalid_request_error", "",
			"no verb on /api/connectors/{id}; use /disconnect or /connect")
		return
	}
	// Fallthrough: echo the cleaned connector segment the router rejected,
	// not the raw URL (which still carries the profile prefix and
	// percent-encoding). Note the gateway-wide decoding underneath:
	// splitProfile reads r.URL.Path (decoded), so %2F folds to / before the
	// Cut — fail-closed either way (both parts are validated), consistent
	// with every other route in this server.
	apiError(w, http.StatusNotFound, "invalid_request_error", "",
		"unknown path "+bounded(approvals.ScrubError(path)))
}
