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

// This file is the connectors surface (#316: foundation + OAuth grants).
//
// Connectors are deployment-wide (unlike memory/teach): one status per
// connector id, no profile split. The app sees status + scopes only —
// refresh tokens live server-side in Mongo, sealed, and never appear here.
//
//	GET  /api/connectors              list the three v1 rows (disconnected
//	                                 synthesised on fresh deploys)
//	POST /api/connectors/{id}/disconnect   stop future access (tokens + all
//	                                 pending states cleared server-side)
//	POST /api/connectors/{id}/connect      mints state+verifier, returns the
//	                                 Google consent auth_url (501 when OAuth
//	                                 unconfigured — never a secrets prompt)
//	GET  /api/connectors/callback?code=&state=  single registered redirect;
//	                                 consumes the state once, exchanges
//	                                 server-side, seals tokens, flips to
//	                                 connected
//
// Callback is the one route that skips the bearer gate (see route()): a
// Google-redirected browser carries no Authorization header, and the state
// is the capability. An expired row is the reconnect prompt: the app
// banners it and offers the connect flow, never a password field.

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
	// first-connected-at and is retained across disconnect/expire, so
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

// connectorCallbackCtx bounds one full OAuth callback: state consume +
// Google exchange + two sealed writes + status flip, all sequential against
// live Google. The 5s verb budget cannot hold that (Google P99 alone can
// exceed it, and the failure would strand an already-consumed state),
// so the callback gets 30s. Single endpoint, no per-connector fan-out.
func connectorCallbackCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 30*time.Second)
}

// storeTimeoutOrGone maps a store failure onto the timeout contract both
// verbs share: store didn't answer (deadline) → 502; client went away first
// (any other ctx error) → logged distinctly so triage doesn't read a hangup
// as a store failure, then falls through to the domain error (the write
// fails silently on a dead conn either way). Void by design: both branches
// answer, so there is no conditional for callers to branch on — a bool
// would invite a meaningless `if !storeTimeoutOrGone`.
func storeTimeoutOrGone(w http.ResponseWriter, s *Server, ctx context.Context, err error, verb string) {
	if ctx.Err() == context.DeadlineExceeded {
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"the connector store did not answer in time")
		return
	}
	if ctx.Err() != nil {
		// Observe-only: writing a domain error on a dead connection only
		// buys a second log line at the http layer for a hangup. Log and
		// return without a body.
		s.cfg.Logf("connectors: %s aborted, client gone: %v", verb, ctx.Err())
		return
	}
	connectorsError(w, err)
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

// callbackStateReject is the single answer for every dead OAuth attempt —
// unknown, consumed, or expired state, plus unknown profile on the
// callback path (see route()). One string so no branch can drift into an
// oracle that distinguishes them.
const callbackStateReject = "that connect attempt expired or was already used; start over from connect"

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
	// Single callback for all three connectors: Google requires the EXACT
	// registered redirect URI, so one stable spelling (rather than one per
	// connector id) means one URI to register. The connector travels inside
	// the signed-by-opaque state row, not the path — the handler resolves it
	// from ConsumeState and rejects cross-wired states. Checked before the
	// id/action split below so "callback" can never read as a connector id.
	if rest == "api/connectors/callback" {
		if !requireMethod(w, r, http.MethodGet, "/api/connectors/callback") {
			return
		}
		s.handleConnectorCallback(w, r)
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
		s.handleConnectorConnect(w, r, norm)
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

// handleConnectorConnect starts one OAuth attempt: mints state+verifier,
// records the pending attempt, and returns the Google consent URL the app
// opens in a browser. The app never sees the verifier, the code, or any
// token — it opens a URL and polls GET /api/connectors.
//
// Unconfigured (nil ConnectorOAuth) answers 501 exactly like the v1
// foundation: the request is fine, the server side isn't built, and the app
// must never fall back to asking for a secret in chat.
func (s *Server) handleConnectorConnect(w http.ResponseWriter, r *http.Request, id string) {
	if s.cfg.ConnectorOAuth == nil {
		apiError(w, http.StatusNotImplemented, "invalid_request_error", "",
			"connect for "+bounded(approvals.ScrubError(id))+" is not configured on this server yet — never paste a token in chat")
		return
	}
	// Defensive: the router already Known-checks, and CreateState +
	// Exchange check again — this keeps the handler correct for direct
	// callers without depending on any of those layers.
	if !connectors.Known(id) {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown connector "+bounded(approvals.ScrubError(id)))
		return
	}
	state, verifier, err := connectors.NewState()
	if err != nil {
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"could not start the connect flow")
		return
	}
	ctx, cancel := connectorCtx(r)
	defer cancel()
	if err := s.connectors.CreateState(ctx, connectors.OAuthState{
		State:     state,
		Connector: id,
		Verifier:  verifier,
		ExpiresAt: time.Now().Add(connectors.StateTTL),
	}); err != nil {
		connectorsError(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"ok":       true,
		"auth_url": connectors.AuthURL(s.cfg.ConnectorOAuth, id, state, verifier),
	})
}

// handleConnectorCallback finishes one OAuth attempt: the browser lands here
// from Google with ?code=&state= on the single registered redirect URI. The
// connector id travels inside the state row (resolved via ConsumeState), not
// the path — there is exactly one callback spelling to register, and a
// state minted for gmail can never land anywhere else. The state is consumed
// (single-use: a code can never be exchanged twice), the code is exchanged
// server-side, the tokens are sealed with the envelope key, and the
// connector flips to connected. Store and exchange errors surface as 502;
// bad states answer 400 with "try connect again" semantics — codes,
// verifiers, and token errors never reach the response body verbatim.
func (s *Server) handleConnectorCallback(w http.ResponseWriter, r *http.Request) {
	if s.cfg.ConnectorOAuth == nil {
		apiError(w, http.StatusNotImplemented, "invalid_request_error", "",
			"connect is not configured on this server yet — never paste a token in chat")
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if code == "" || state == "" {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "",
			"callback needs ?code= and ?state=; start over from connect")
		return
	}
	ctx, cancel := connectorCallbackCtx(r)
	defer cancel()
	st, err := s.connectors.ConsumeState(ctx, state)
	if err != nil {
		// Unknown, consumed, or expired: same answer, "try connect again".
		// The store already refuses to distinguish; the gateway must not
		// add an oracle on top by varying the message. Same string as the
		// unknown-profile callback reject in route().
		apiError(w, http.StatusBadRequest, "invalid_request_error", "",
			callbackStateReject)
		return
	}
	id := st.Connector
	tok, err := connectors.Exchange(ctx, s.cfg.ConnectorOAuth, id, code, st.Verifier, s.cfg.Logf)
	if err != nil {
		connectorsError(w, err)
		return
	}
	cfg := s.cfg.ConnectorOAuth
	refreshSealed, err := connectors.EncryptToken(cfg.Key, tok.RefreshToken)
	if err != nil {
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"could not store the grant")
		return
	}
	accessSealed, err := connectors.EncryptToken(cfg.Key, tok.AccessToken)
	if err != nil {
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"could not store the grant")
		return
	}
	var expiry int64
	if !tok.Expiry.IsZero() {
		expiry = tok.Expiry.UnixMilli()
	}
	if err := s.connectors.SetTokens(ctx, id, connectors.TokenRow{
		RefreshCipher:         refreshSealed,
		AccessCipher:          accessSealed,
		AccessExpiryUnixMilli: expiry,
	}); err != nil {
		connectorsError(w, err)
		return
	}
	// Status last, tokens first: a connected row without tokens is the one
	// servable-but-useless state this stack refuses to invent (see
	// statusUpdateDoc's atomicity note). SetStatus(connected) carries no
	// error text by contract.
	// Triage note: if the process dies between SetTokens and SetStatus, the
	// row holds ciphertext with status still disconnected — not a leak
	// (ciphertext only, unreadable without the envelope key), and the next
	// connect overwrites it. No compensating delete: it could race a
	// concurrent legitimate connect.
	c, err := s.connectors.SetStatus(ctx, id, connectors.StatusConnected, "")
	if err != nil {
		connectorsError(w, err)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "connector": connectorWire(c)})
}
