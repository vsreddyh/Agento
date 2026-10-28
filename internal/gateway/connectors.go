package gateway

import (
	"context"
	"errors"
	"html"
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
//	GET  /api/connectors              list the two v1 rows (disconnected
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
	code, typ, msg := connectorsErrorMapped(err)
	apiError(w, code, typ, "", msg)
}

// connectorsErrorMapped is the pure domain→status mapping behind
// connectorsError. The callback path reuses it so its browser HTML and
// API JSON answers carry the same status, type, and scrubbed message.
func connectorsErrorMapped(err error) (code int, typ, msg string) {
	var se *connectors.StoreError
	if errors.As(err, &se) {
		// Scrubbed in every branch, including NotFound: that message
		// carries caller-influenced text ("unknown connector "+id), so it
		// is the one branch that most needs it. Contract for future
		// StoreError construction: Msg must be safe OR get scrubbed here —
		// this switch scrubs, so callers don't have to.
		msg = bounded(approvals.ScrubError(se.Msg))
		switch {
		case se.NotFound:
			return http.StatusNotFound, "invalid_request_error", msg
		case se.Upstream:
			return http.StatusBadGateway, "upstream_error", msg
		default:
			return http.StatusBadRequest, "invalid_request_error", msg
		}
	}
	return http.StatusBadRequest, "invalid_request_error", bounded(approvals.ScrubError(err.Error()))
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
// alias): the two v1 rows, disconnected synthesised on fresh deploys.
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
// unknown, consumed, expired, or oversized state. One string so no branch
// can drift into an oracle that distinguishes them. (Profile routing is
// bypassed entirely for the callback — see route() — so there is no
// profile branch left to mirror.)
const callbackStateReject = "that connect attempt expired or was already used; start over from connect"

// canonicalRedirectForMint canonicalises the configured redirect for
// pinning at mint time. FromEnv already enforces canonical shape, so an
// error here means a hand-built cfg — fail fast with a 502 config error
// instead of storing raw and letting the store 400 it as bad state input.
func canonicalRedirectForMint(cfg *connectors.OAuthConfig, logf func(string, ...any)) (string, error) {
	canonical, err := connectors.CanonicalRedirectURL(cfg.RedirectURL)
	if err != nil {
		if logf != nil {
			logf("connectors: connect redirect invalid: %v", err)
		}
		return "", err
	}
	return canonical, nil
}

// callbackDeadAttempt answers a dead OAuth attempt (unknown, consumed, or
// expired state) with the same 400 meaning as every other failure on this
// unauthenticated path — one string, never an oracle on id validity. A
// browser tab gets the close-this-tab page (the operator just consented
// and needs guidance, not JSON); API clients get the existing JSON shape
// unchanged. no-store is already set at the top of the handler.
func callbackDeadAttempt(w http.ResponseWriter, r *http.Request) {
	// Single ownership: every answer on this path carries a single-use
	// capability URL, so the helpers set no-store themselves instead of
	// relying on each caller (the 405 branch bypasses the handler's
	// first-line header).
	w.Header().Set("Cache-Control", "no-store")
	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Agento connect expired</title></head>` +
			`<body><p>This connect attempt expired or was already used. Close this tab and tap connect again in the Agento app.</p></body></html>`))
		return
	}
	apiError(w, http.StatusBadRequest, "invalid_request_error", "",
		callbackStateReject)
}

// callbackError answers every other callback failure in both
// representations: JSON for API clients (the app polls
// GET /api/connectors instead of reading error bodies), close-this-tab
// HTML for the Google-redirected browser tab that just consented. Same
// status, same message — only the envelope differs, so no branch can
// drift into a guidance gap or an oracle. Message text is HTML-escaped:
// every caller passes a static string or a scrubbed domain-mapped one,
// and the escape keeps that contract even if a caller slips.
func callbackError(w http.ResponseWriter, r *http.Request, code int, typ, msg string) {
	// Single ownership like callbackDeadAttempt: no-store lives here, not
	// at each call site, so the next branch added beside the 405 check
	// cannot miss it.
	w.Header().Set("Cache-Control", "no-store")
	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(code)
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Agento connect</title></head><body><p>` +
			html.EscapeString(msg) + ` Close this tab and return to the Agento app.</p></body></html>`))
		return
	}
	apiError(w, code, typ, "", msg)
}

// callbackTimeoutOrGone is storeTimeoutOrGone for the callback path: same
// timeout/client-gone split, but the domain fallthrough answers in both
// representations (see callbackError) instead of JSON-only.
func callbackTimeoutOrGone(w http.ResponseWriter, s *Server, r *http.Request, ctx context.Context, err error, verb string) {
	if ctx.Err() == context.DeadlineExceeded {
		callbackError(w, r, http.StatusBadGateway, "upstream_error",
			"the connector store did not answer in time")
		return
	}
	if ctx.Err() != nil {
		s.cfg.Logf("connectors: %s aborted, client gone: %v", verb, ctx.Err())
		return
	}
	code, typ, msg := connectorsErrorMapped(err)
	callbackError(w, r, code, typ, msg)
}

// handleConnectors serves /api/connectors. Global like approvals, not
// per-profile like memory.
//
// Budget note: verbs fan out to quickCtx-budgeted pre-reads + readBack
// around one verb-ctx write (~15s worst case, no overall deadline,
// detached from request cancel by design — see quickCtx). The callback
// path is the exception this note warned about: it binds the whole
// grant pipeline to one verb-level deadline (connectorCallbackCtx, 30s)
// instead of stacking per-call budgets.
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
	// Single callback for both connectors: Google requires the EXACT
	// registered redirect URI, so one stable spelling (rather than one per
	// connector id) means one URI to register. The connector travels inside
	// the signed-by-opaque state row, not the path — the handler resolves it
	// from ConsumeState and rejects cross-wired states. Checked before the
	// id/action split below so "callback" can never read as a connector id.
	if rest == "api/connectors/callback" {
		if r.Method != http.MethodGet {
			// Method errors on this path answer in both
			// representations like every other callback branch:
			// a browser tab that POSTs (prefetch, form replay,
			// extension) gets the guidance page, not raw JSON.
			// Allow header preserved from requireMethod's contract.
			w.Header().Set("Allow", http.MethodGet)
			callbackError(w, r, http.StatusMethodNotAllowed, "invalid_request_error",
				"/api/connectors/callback accepts GET")
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
	// First line, before every branch — same ordering trap as the
	// callback: the success answer carries the capability (auth_url), and
	// a future branch must not miss the header.
	w.Header().Set("Cache-Control", "no-store")
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
		// Logged like every other server-side failure on this path:
		// without it a (near-impossible) rand failure is a silent 502
		// indistinguishable from the AuthURL-empty branch below.
		s.cfg.Logf("connectors: could not mint connect state: %v", err)
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"could not start the connect flow")
		return
	}
	// Capability-bearing response (auth_url completes the grant): never
	// heuristically cacheable by proxy or browser. Header set at the top
	// of this handler so every branch carries it.
	// Persist first, build the URL second: a down store fails fast with no
	// dead attempt minted, and the empty-auth_url guard below stays the
	// only post-store branch.
	ctx, cancel := connectorCtx(r)
	defer cancel()
	// No ExpiresAt: the store owns the default (memory injectable clock,
	// Mongo TTL-indexed). Passing wall-clock time here would bypass the
	// injectable default and pin a boundary no test covers — single clock
	// owner, set at the store.
	// Budget composition note: the 5s verb budget covers the whole
	// CreateState call, including the advisory supersede delete (which
	// runs on its own detached 5s quickCtx inside the store). Worst case
	// is sequential — ~5s of delete plus a 502d insert if Atlas is that
	// slow — and a failed create mints nothing, so re-tap is a clean
	// retry (same idempotency story as every other verb here).
	//
	// Fail fast on a misshapen configured redirect: storing raw and letting
	// the store 400 it would misread a server config error as bad state
	// input. FromEnv enforces canonical shape, so this fires only for a
	// hand-built cfg — exactly when triage needs the discriminant.
	pinnedRedirect, err := canonicalRedirectForMint(s.cfg.ConnectorOAuth, s.cfg.Logf)
	if err != nil {
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"connect is not configured correctly on this server")
		return
	}
	if err := s.connectors.CreateState(ctx, connectors.OAuthState{
		State:     state,
		Connector: id,
		Verifier:  verifier,
		// Pin the exact redirect_uri minted for this attempt: a config
		// rotation mid-consent must not strand it with a mismatch.
		// Canonicalised here (not raw cfg) so stored == sent == logged
		// by construction — CreateState re-checks shape, but the
		// canonical form is what both helpers send.
		RedirectURI: pinnedRedirect,
	}); err != nil {
		// Same timeout/client-gone split as list/disconnect and every
		// callback store call: the domain mapper below never sees a
		// deadline, and a gone client never pays for the AuthURL build
		// + 200 write that follows.
		storeTimeoutOrGone(w, s, ctx, err, "connect")
		return
	}
	// Fail-closed builder: empty means a helper refused (nil cfg, unknown
	// id — both pre-checked above, so this is defense in depth, not a live
	// branch). Never hand the app a 200 with an empty auth_url it would
	// open as nothing with no error to triage.
	authURL := connectors.AuthURL(s.cfg.ConnectorOAuth, id, state, verifier, s.cfg.Logf)
	if authURL == "" {
		// No log here: the helper already logged which guard fired.
		// A second line would split triage across two entries for one
		// failure.
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"could not start the connect flow")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{
		"ok":       true,
		"auth_url": authURL,
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
	// First line, before every branch: every answer on this path carries a
	// single-use capability URL (?code=&state=), so nothing here may sit in
	// a heuristic cache — success JSON and 400s alike.
	w.Header().Set("Cache-Control", "no-store")
	if s.cfg.ConnectorOAuth == nil {
		callbackError(w, r, http.StatusNotImplemented, "invalid_request_error",
			"connect is not configured on this server yet — never paste a token in chat")
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if code == "" || state == "" {
		callbackError(w, r, http.StatusBadRequest, "invalid_request_error",
			"callback needs ?code= and ?state=; start over from connect")
		return
	}
	// Length-gated before the store/Google calls (shared
	// connectors.MaxStateFieldLen — the store enforces the same bound, so
	// the gateway cap is the fast path, not the only enforcement): states
	// are ~22-char base64url, codes a few hundred chars in practice. A
	// multi-MB value would become an oversized DB filter / outbound token
	// request. Same 400 shape as every other callback failure, so no
	// oracle is added.
	if len(code) > connectors.MaxStateFieldLen || len(state) > connectors.MaxStateFieldLen {
		callbackError(w, r, http.StatusBadRequest, "invalid_request_error",
			callbackStateReject)
		return
	}
	ctx, cancel := connectorCallbackCtx(r)
	defer cancel()
	st, err := s.connectors.ConsumeState(ctx, state)
	if err != nil {
		// Timeout/client-gone split first (same contract as list and the
		// verbs below): a deadline answers 502 with the timeout wording,
		// a gone client logs and returns without writing to a dead conn.
		if ctx.Err() == context.DeadlineExceeded {
			callbackError(w, r, http.StatusBadGateway, "upstream_error",
				"the connector store did not answer in time")
			return
		}
		if ctx.Err() != nil {
			s.cfg.Logf("connectors: callback aborted, client gone: %v", ctx.Err())
			return
		}
		// Unknown, consumed, or expired: same dead-attempt answer, "try
		// connect again". The store already refuses to distinguish; the
		// gateway must not add an oracle on top by varying the message.
		// Upstream store failures are the exception: those 502 via the
		// domain mapper (transient, retryable) instead of burning the
		// user's live state with a "start over" that would supersede it.
		// Browser tabs get the close-this-tab page, API clients the JSON
		// shape — same 400 meaning either way.
		var se *connectors.StoreError
		if errors.As(err, &se) && se.Upstream {
			status, typ, msg := connectorsErrorMapped(err)
			callbackError(w, r, status, typ, msg)
			return
		}
		callbackDeadAttempt(w, r)
		return
	}
	id := st.Connector
	// Send the pinned redirect from mint time, not the live config: a
	// rotation mid-consent would otherwise exact-mismatch Google.
	// Empty means minted before this column existed: fall back to the
	// live config rather than failing an otherwise valid attempt.
	exCfg := s.cfg.ConnectorOAuth
	if st.RedirectURI != "" {
		pinned := *exCfg
		pinned.RedirectURL = st.RedirectURI
		exCfg = &pinned
	}
	tok, err := connectors.Exchange(ctx, exCfg, id, code, st.Verifier, s.cfg.Logf)
	if err != nil {
		// Same timeout/client-gone split as ConsumeState above, but the
		// deadline wording names the exchange, not the store: the 30s
		// budget expired during the Google token round-trip
		// (oauth2.Exchange), not a FindOneAndDelete/SetTokens store
		// call. A gone browser logs without writing to a dead conn.
		// Only then the domain map below.
		if ctx.Err() == context.DeadlineExceeded {
			callbackError(w, r, http.StatusBadGateway, "upstream_error",
				"the grant exchange did not answer in time")
			return
		}
		if ctx.Err() != nil {
			s.cfg.Logf("connectors: callback aborted, client gone: %v", ctx.Err())
			return
		}
		var se *connectors.StoreError
		if errors.As(err, &se) && se.NotFound {
			// Unknown connector id inside a consumed state (hand-inserted
			// row — CreateState rejects unknown ids, so no live flow gets
			// here): same dead-attempt 400 as every other failure on this
			// unauthenticated path, never a 404 oracle on id validity.
			callbackDeadAttempt(w, r)
			return
		}
		status, typ, msg := connectorsErrorMapped(err)
		callbackError(w, r, status, typ, msg)
		return
	}
	cfg := s.cfg.ConnectorOAuth
	// Trimmed copies are what get sealed: a padded value would otherwise
	// pass the non-empty checks and seal with padding the refresher cannot
	// use (same class as the whitespace cases fixed earlier — check and
	// stored bytes must be the same string).
	refreshToken := strings.TrimSpace(tok.RefreshToken)
	accessToken := strings.TrimSpace(tok.AccessToken)
	refreshSealed, err := connectors.EncryptToken(cfg.Key, refreshToken)
	if err != nil {
		// Logged, not answered in detail: seal failures are server-side
		// (rand, zero key) and carry nothing the client can act on.
		s.cfg.Logf("connectors: token seal failed for %s: %v", id, err)
		callbackError(w, r, http.StatusBadGateway, "upstream_error",
			"could not store the grant")
		return
	}
	// Empty access tokens stay empty: sealing "" would create a second
	// representation of "missing" that the refresher must also recognise.
	// The TokenRow contract names literal "" as the missing form.
	var accessSealed string
	if accessToken != "" {
		accessSealed, err = connectors.EncryptToken(cfg.Key, accessToken)
		if err != nil {
			s.cfg.Logf("connectors: token seal failed for %s: %v", id, err)
			callbackError(w, r, http.StatusBadGateway, "upstream_error",
				"could not store the grant")
			return
		}
	}
	var expiry int64
	if accessToken != "" && !tok.Expiry.IsZero() {
		expiry = tok.Expiry.UnixMilli()
	}
	if err := s.connectors.SetTokens(ctx, id, connectors.TokenRow{
		RefreshCipher:         refreshSealed,
		AccessCipher:          accessSealed,
		AccessExpiryUnixMilli: expiry,
	}); err != nil {
		// Same timeout/client-gone split as list/disconnect, answered
		// in both representations (see callbackError).
		callbackTimeoutOrGone(w, s, r, ctx, err, "callback tokens")
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
		callbackTimeoutOrGone(w, s, r, ctx, err, "callback status")
		return
	}
	// A browser tab (Accept: text/html) gets a close-this-tab page; API
	// clients (the app polls GET /api/connectors instead of reading this
	// body) get the existing JSON shape unchanged. no-store was already set
	// at the top of the handler for every branch on this path.
	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Agento connected</title></head>` +
			`<body><p>Connected. You can close this tab and return to the Agento app.</p></body></html>`))
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "connector": connectorWire(c)})
}
