package gateway

import (
	"context"
	"errors"
	"html"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

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

// acceptsHTML reports text/html negotiation without lowercasing an
// unbounded header: unauthenticated callers control Accept, and Go allows
// ~1MB of it — a megabyte value would buy a megabyte alloc before any gate
// runs. Legit browser values are ~100 chars, so anything past 512 answers
// JSON. Guidance-vs-alloc tradeoff, stated so it sticks: HTML here is a
// close-this-tab convenience, JSON is the contract — a long-Accept browser
// (extensions, enterprise headers) gets valid JSON, not a broken flow.
// Length-only log on the skip: a browser tab that got JSON instead of the
// close-this-tab page reads as a regression with nothing to trace — same
// pointer class as the duplicate-params and oversized-scope skips. logf
// may be nil (tests).
func acceptsHTML(r *http.Request, logf func(string, ...any)) bool {
	accept := r.Header.Get("Accept")
	if len(accept) > 512 {
		if logf != nil {
			logf("connectors: Accept header oversized (%d bytes), answering JSON", len(accept))
		}
		return false
	}
	// Boundary-checked, not substring: "text/html" must end the value or be
	// followed by a parameter/list separator — so application/x-text/htmlfoo
	// stays JSON. Browsers send bare "text/html", so the convenience page
	// keeps working while non-HTML ranges cannot trigger it.
	return hasHTMLRange(strings.ToLower(accept))
}

// hasHTMLRange reports a boundary-delimited "text/html" token: end of value
// or followed by space, comma, or semicolon (parameter/list separator).
func hasHTMLRange(lower string) bool {
	for i := 0; i+len("text/html") <= len(lower); {
		j := strings.Index(lower[i:], "text/html")
		if j < 0 {
			return false
		}
		i += j
		after := i + len("text/html")
		if after == len(lower) || lower[after] == ' ' || lower[after] == ',' || lower[after] == ';' || lower[after] == '\t' {
			return true
		}
		i = after
	}
	return false
}

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
		// Scrubbed + bounded like every other echo on this surface: the
		// only writer in this PR passes "", but the store contract does
		// not bound the column — the next writer that sets provider text
		// must not land it in an API body uncapped.
		out["last_error"] = bounded(approvals.ScrubError(c.LastError))
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
		s.cfg.Logf("connectors: %s aborted, request ended (client gone or server stopping): %v", verb, ctx.Err())
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
	// no-store like every other verb on this surface: the app polls this
	// loop, and a heuristic cache would serve a stale disconnected after
	// the callback flipped to connected. No capability here (status +
	// scopes only), so this is staleness, not leak — still no-store.
	w.Header().Set("Cache-Control", "no-store")
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

// isDuplicateStateError reports the already-exists shape both stores
// return when the state id collides with a live row: that row predates the
// call, so the caller must not consume it. Typed via StoreError.Duplicate,
// not the message — reword Msg freely without touching this contract.
func isDuplicateStateError(err error) bool {
	var se *connectors.StoreError
	return err != nil && errors.As(err, &se) && se.Duplicate
}

// cleanupOrphanState best-effort consumes a just-minted state on a detached
// budget (the verb ctx may be dead at exactly this point — cleanup must not
// depend on it). Only real IO failures log: NotFound means no row was ever
// minted (failed create) or the row already burned — both are the desired
// end state, not a failure, and logging them would send triage grepping
// phantom cleanups. Upstream failures log, since the row may have landed
// unseen. Racy by construction against a still-in-flight detached insert
// (the insert may land after this delete): the orphan then lives until
// TTL/next-tap supersede — same accepted class as the disconnect-during-
// exchange window, re-tap recovers. See ADR-0009.
func (s *Server) cleanupOrphanState(id, state string) {
	cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cleanCancel()
	if _, cerr := s.connectors.ConsumeState(cleanCtx, state); cerr != nil {
		var se *connectors.StoreError
		if errors.As(cerr, &se) && se.Upstream {
			// Scrubbed + bounded like every interpolated log on this
			// surface: today's message is static, but the StoreError
			// contract warns Msg may carry caller-influenced text.
			s.cfg.Logf("connectors: orphan state cleanup for %q failed: %q", id, bounded(approvals.ScrubError(cerr.Error())))
		}
	}
}

// canonicalRedirectForMint canonicalises the configured redirect for
// pinning at mint time. FromEnv already enforces canonical shape, so an
// error here means a hand-built cfg — fail fast with a 502 config error
// instead of storing raw and letting the store 400 it as bad state input.
func canonicalRedirectForMint(cfg *connectors.OAuthConfig, id string, logf func(string, ...any)) (string, error) {
	canonical, err := connectors.CanonicalRedirectURL(cfg.RedirectURL)
	if err != nil {
		if logf != nil {
			// Echo the attempted value: redirect is non-secret (logged
			// verbatim on success), and triage needs the mismatch
			// candidate without re-reading .env. Id named like every
			// sibling log on this path, so triage greps single-term.
			// Error quoted: canonicaliser errors are static strings today,
			// but quoting keeps the every-value-quoted contract total if
			// one ever interpolates input. `want` is scrubbed for logging;
			// compare against the success-path `callback %q` line for the
			// exact sent bytes (a scrub-shaped substring would log
			// differently here than what AuthURL/Exchange would send).
			// Single owner: RedactURL already scrubs + bounds + marks.
			// No second bounded() here — that would re-cap a 300-rune
			// value at 128 and stack a second truncation marker onto the
			// first, breaking the byte-for-byte comparison this log exists
			// for. Do not re-wrap. Fidelity note: this failure line caps at
			// ~300 runes while the success-path callback line logs the full
			// string — diff a truncated failure `want` against the console
			// only up to its marker, or fix-then-read the success line.
			logf("connectors: connect redirect invalid for %q (want %q): %q", id, connectors.RedactURL(cfg.RedirectURL), err.Error())
		}
		return "", err
	}
	return canonical, nil
}

// writeCallbackPage answers a browser tab with the close-this-tab page:
// no-store + nosniff + status + guidance body. Single place so the two
// representation helpers cannot drift apart when the next one is added —
// the "same status, same message, two envelopes" contract stays structural,
// not disciplined. body must already be display-safe (static text or
// html.EscapeString at the call site), like the apiError path below.
// no-store lives here AND at each caller: the callers cover the JSON
// envelope on the same path, this covers the HTML one — the 405 lesson
// (a bypassed first-line header) applied to both envelopes, so neither
// envelope trusts the other.
func writeCallbackPage(w http.ResponseWriter, code int, title, body string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	// Both envelopes escaped inside the helper: title was already owned
	// here, and body is now too — every caller passes raw text (static
	// guidance or a scrubbed domain message) and the helper escapes it,
	// so a future branch interpolating a connector id or provider string
	// lands escaped in a browser-parsed page. Safe by construction, not
	// by discipline; static strings escape to themselves.
	_, _ = w.Write([]byte(`<!doctype html><html><head><title>` + html.EscapeString(title) + `</title></head><body><p>` + html.EscapeString(body) + `</p></body></html>`))
}

// callbackDeadAttempt answers a dead OAuth attempt (unknown, consumed, or
// expired state) with the same 400 meaning as every other failure on this
// unauthenticated path — one string, never an oracle on id validity. A
// browser tab gets the close-this-tab page (the operator just consented
// and needs guidance, not JSON); API clients get the existing JSON shape
// unchanged. no-store is already set at the top of the handler.
func callbackDeadAttempt(w http.ResponseWriter, r *http.Request, logf func(string, ...any)) {
	// Single ownership: every answer on this path carries a single-use
	// capability URL, so the helpers set no-store themselves instead of
	// relying on each caller (the 405 branch bypasses the handler's
	// first-line header).
	w.Header().Set("Cache-Control", "no-store")
	if acceptsHTML(r, logf) {
		writeCallbackPage(w, http.StatusBadRequest, "Agento connect expired",
			"This connect attempt expired or was already used. Close this tab and tap connect again in the Agento app.")
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
func callbackError(w http.ResponseWriter, r *http.Request, code int, typ, msg string, logf func(string, ...any)) {
	// Single ownership like callbackDeadAttempt: no-store lives here, not
	// at each call site, so the next branch added beside the 405 check
	// cannot miss it.
	w.Header().Set("Cache-Control", "no-store")
	if acceptsHTML(r, logf) {
		writeCallbackPage(w, code, "Agento connect",
			msg+" Close this tab and return to the Agento app.")
		return
	}
	apiError(w, code, typ, "", msg)
}

// missingScopes reports requested scopes absent from a space-delimited
// granted set (the ?scope= Google appends to the callback). Space-delimited
// by spec (RFC 6749): commas are not separators, so a comma-joined value
// would false-warn — Google sends spaces, and anything else is treated
// literally (a "missing" warning, never a grant failure). Set compare:
// order and repeats don't matter, only coverage. Empty granted means
// Google sent none — treated as "nothing to compare", not a total miss,
// since some flows omit the param on full grants.
func missingScopes(want []string, granted string) []string {
	// Nothing to compare: GoogleScopes is non-nil on every path that
	// reaches here today (hand-inserted unknown rows dead-attempt
	// earlier), but an empty want must not pay the map alloc on the
	// unauthenticated fast path — or rely on the caller to guarantee it.
	if len(want) == 0 {
		return nil
	}
	fields := strings.Fields(granted)
	have := make(map[string]bool, len(fields))
	for _, s := range fields {
		have[s] = true
	}
	var missing []string
	for _, s := range want {
		if !have[s] {
			missing = append(missing, s)
		}
	}
	return missing
}

// callbackTimeoutOrGone is storeTimeoutOrGone for the callback path: same
// timeout/client-gone split, but the domain fallthrough answers in both
// representations (see callbackError) instead of JSON-only.
func callbackTimeoutOrGone(w http.ResponseWriter, s *Server, r *http.Request, ctx context.Context, err error, verb string) {
	if ctx.Err() == context.DeadlineExceeded {
		callbackError(w, r, http.StatusBadGateway, "upstream_error",
			"the connector store did not answer in time", s.cfg.Logf)
		return
	}
	if ctx.Err() != nil {
		s.cfg.Logf("connectors: %s aborted, request ended (client gone or server stopping): %v", verb, ctx.Err())
		return
	}
	code, typ, msg := connectorsErrorMapped(err)
	callbackError(w, r, code, typ, msg, s.cfg.Logf)
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
	// First line, before every branch: no answer on this surface may sit
	// in a heuristic cache — success bodies complete grants, and error
	// bodies carry single-use capability URLs. Single ownership here (not
	// per-branch) so the next branch added beside these cannot miss it
	// the way the callback 405 once did.
	w.Header().Set("Cache-Control", "no-store")
	// Exact match for the collection, prefix-with-slash for members:
	// "api/connectorsfoo" is an unknown path, not connector "foo" — the
	// prefix form without a slash boundary would mislabel the resource and
	// send triage after a bad id instead of a bad path.
	// Case-sensitive by design: paths are lowercase-canonical. Only the
	// callback bypass folds case (EqualFold above and in route()), because
	// unauthenticated callers must not be distinguishable by casing; a
	// miscased authenticated member 404s here, which is a client bug, not
	// an oracle.
	if rest == "api/connectors" {
		s.serveConnectorsList(w, r)
		return
	}
	// Single callback for both connectors: Google requires the EXACT
	// registered redirect URI, so one stable spelling (rather than one per
	// connector id) means one URI to register. The connector travels inside
	// the signed-by-opaque state row, not the path — the handler resolves it
	// from ConsumeState and rejects cross-wired states. Checked before the
	// id/action split below so "callback" can never read as a connector id.
	// Case-insensitive like the server bypass above, and checked before the
	// prefix gate: path casing must not distinguish the same
	// unauthenticated caller, and the HasPrefix below is case-sensitive.
	// ("api/connectorsfoo" still 404s — it is not the callback.)
	if strings.EqualFold(rest, "api/connectors/callback") {
		if r.Method != http.MethodGet {
			// Method errors on this path answer in both
			// representations like every other callback branch:
			// a browser tab that POSTs (prefetch, form replay,
			// extension) gets the guidance page, not raw JSON.
			// Allow header preserved from requireMethod's contract.
			w.Header().Set("Allow", http.MethodGet)
			callbackError(w, r, http.StatusMethodNotAllowed, "invalid_request_error",
				"/api/connectors/callback accepts GET", s.cfg.Logf)
			return
		}
		s.handleConnectorCallback(w, r)
		return
	}
	if !strings.HasPrefix(rest, "api/connectors/") {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown path "+bounded(approvals.ScrubError(rest)))
		return
	}
	path := strings.TrimPrefix(rest, "api/connectors/")
	// Unreachable via splitProfile (path.Clean strips trailing slashes
	// before dispatch), kept as defense in depth: without it a
	// doubly-slashed client path reaching here as "gmail/disconnect/"
	// would 404 as an unknown action instead of hitting the verb.
	path = strings.TrimSuffix(path, "/")

	if path == "" {
		// "api/connectors/" with trailing slash: the collection, same as
		// the exact form above — one list shape, one helper.
		s.serveConnectorsList(w, r)
		return
	}

	id, action, _ := strings.Cut(path, "/")
	// Alloc guard before any fold or scan: a multi-MB segment answers the
	// same oversized 400 as every sibling gate without the TrimFunc/
	// ToLower/HasControl rune walks below. No echo — the value is
	// megabytes. Semantic caps stay on the normalised forms further down
	// (padding must not turn a known id unknown), so this tier only kills
	// multi-MB; the trimmed gate below is the contract.
	// Oversized answers 400, not 404 — matching every store verb: length
	// fails before identity on all layers, so the same input reports the
	// same code wherever it is checked. No echo (the value is megabytes).
	if len(id) > connectors.MaxSegmentAllocLen {
		connectorsError(w, connectors.NewStoreError("oversized connector id", false))
		return
	}
	if len(action) > connectors.MaxSegmentAllocLen {
		connectorsError(w, connectors.NewStoreError("oversized path segment", false))
		return
	}
	// Trim first, then check — matching the code/state legs and every store
	// verb: edge whitespace and invisible Cf (zero-width, BOM) are forgiven
	// at the edges, so a zero-width-prefixed id folds to gmail here instead
	// of 404ing what the store would accept. Interior controls still fail closed, and
	// empty-after-trim stays on its natural branch (blank id 404s below,
	// blank verb hits the same no-verb 405 as a missing verb) — blank ==
	// absent on both segments, so no branch distinguishes them.
	id = connectors.TrimCredential(id)
	action = connectors.TrimCredential(action)
	// Length on the trimmed form, before any scan or fold: the trimmed form
	// is what Known actually sees, so padding still cannot turn a known id
	// unknown — and anything over the cap skips the HasControl rune walks
	// and the ToLower alloc below. (The raw pre-filter above only kills
	// multi-MB; this is the contract gate, same 64 as every sibling.)
	if len(id) > connectors.MaxConnectorIDLen {
		connectorsError(w, connectors.NewStoreError("oversized connector id", false))
		return
	}
	if len(action) > connectors.MaxConnectorIDLen {
		connectorsError(w, connectors.NewStoreError("oversized path segment", false))
		return
	}
	if id == "" || connectors.HasControl(id) || !utf8.ValidString(id) {
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown connector")
		return
	}
	if connectors.HasControl(action) || !utf8.ValidString(action) {
		// Honest noun: the connector is fine, the verb is garbage. Same
		// 404 with no echo and no oracle — only the noun changes, so the
		// next triage grep for a bad-verb 404 lands on the verb, not the
		// connector. Echoes the normalised action (what the switch below
		// sees), not the raw path — stored == logged by construction.
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown path "+bounded(approvals.ScrubError(action)))
		return
	}
	// Verbs fold like ids: "Gmail/Disconnect" from a hand test lands on the
	// verb instead of 404ing as an unknown path. Already trimmed above — a
	// padded verb is client noise, not a different resource. Length-capped
	// like the id: bounded() keeps any echo safe and the server caps total
	// header size anyway, but there is no reason to pay rune-scan + alloc
	// on a garbage path before the inevitable 404.
	action = strings.ToLower(action)
	if len(action) > connectors.MaxConnectorIDLen {
		// Same length-before-identity as the id legs: 400, not 404.
		// Kept as a separate gate even though the trimmed check above
		// usually fires first: ToLower can grow byte length for exotic
		// spellings, so the post-fold length is what the switch below
		// actually sees — check it, don't assume the pre-fold check held.
		connectorsError(w, connectors.NewStoreError("oversized path segment", false))
		return
	}
	// Normalise before Known: " GMail " addresses gmail (case and spacing
	// fold), while a truly unknown id still 404s. The length guard measures
	// the normalised form — what Known actually sees — so padding cannot
	// turn a known id unknown. Single trim: id was already trimmed above,
	// so fold directly instead of re-trimming via NormalizedID (whose trim
	// would be a no-op here).
	norm := strings.ToLower(id)
	if norm == "" || connectors.HasControl(norm) || !utf8.ValidString(norm) {
		// Empty after folding (" "): same bare 404 as the NUL path above,
		// not the trailing-space echo the unknown branch would emit.
		// Controls checked post-fold too: ToLower never introduces them,
		// so a control-bearing norm is caller input, not a fold artifact
		// — and the echo below must never carry it. Checked == logged:
		// the Known branch beneath echoes this same norm.
		apiError(w, http.StatusNotFound, "invalid_request_error", "",
			"unknown connector")
		return
	}
	if len(norm) > connectors.MaxConnectorIDLen {
		// Length before identity, matching every store verb and helper:
		// the same oversized input 400s in the stores, so it 400s here
		// too instead of reading as unknown. No echo — the value is
		// megabytes-scale by construction of this branch.
		connectorsError(w, connectors.NewStoreError("oversized connector id", false))
		return
	}
	if !connectors.Known(norm) {
		// Identity-first at dispatch, by design: this gate is shared by
		// every member verb, and disconnect never reads OAuth config — a
		// config 502 here would be wrong for it. So a routed unknown id
		// 404s here regardless of config health, while the connect verb
		// itself is shape-first internally (canonical before Known) for
		// direct callers and the known-id path — same split as AuthURL/
		// Exchange, which have no dispatch layer to resolve first.
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
	// Fallthrough: echo the normalised action (what the switch saw), not
	// the raw URL (which still carries the profile prefix and
	// percent-encoding). Stored == logged: triage greps the spelling the
	// store would have seen. Note the gateway-wide decoding underneath:
	// splitProfile reads r.URL.Path (decoded), so %2F folds to / before the
	// Cut — fail-closed either way (both parts are validated), consistent
	// with every other route in this server.
	apiError(w, http.StatusNotFound, "invalid_request_error", "",
		"unknown path "+bounded(approvals.ScrubError(action)))
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
	// Config shape before identity, matching AuthURL/Exchange (shape-first
	// since round 42): a misconfigured server 502s before identity or
	// randomness or budgets — the same (unknown-id + bad-config) pair
	// answers config-first whichever layer it reaches. FromEnv enforces
	// canonical shape, so this fires only for a hand-built cfg — exactly
	// when triage needs the discriminant. Ordering rationale in the file's
	// own terms: config fails fast with no dead attempt minted.
	pinnedRedirect, err := canonicalRedirectForMint(s.cfg.ConnectorOAuth, id, s.cfg.Logf)
	if err != nil {
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"connect is not configured correctly on this server")
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
	// Verb budget opens before use, not after mint: the budget should cover
	// the whole attempt the app waits on — validation, mint, and the
	// CreateState call below — so a slow start (depleted entropy, cold
	// container) debits the same 5s the detached IO is sized against
	// instead of running off-budget. (The mint itself is ~µs of rand and
	// needs no deadline; the composition does.) Validation + mint only:
	// CreateState ignores the verb ctx (detached quickCtx inside the
	// store), so this 5s is not an IO budget — the ctx.Err() checks below
	// still need a verb clock to observe.
	ctx, cancel := connectorCtx(r)
	defer cancel()
	state, verifier, err := connectors.NewState()
	if err != nil {
		// Logged like every other server-side failure on this path, as a
		// static string: rand carries no discriminant (and no caller
		// input), so there is nothing to quote or scrub. Deliberate, not
		// an omission — do not "restore" a %v here.
		s.cfg.Logf("connectors: could not mint connect state")
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"could not start the connect flow")
		return
	}
	// Capability-bearing response (auth_url completes the grant): never
	// heuristically cacheable by proxy or browser. Header set at the top
	// of this handler so every branch carries it.
	// Persist first, build the URL second: a down store fails fast with no
	// dead attempt minted. The empty-auth_url guard below stays the only
	// post-store branch, and it best-effort consumes the just-minted row
	// so a failed build leaves nothing observable behind.
	// No ExpiresAt: the store owns the default (memory injectable clock,
	// Mongo TTL-indexed). Passing wall-clock time here would bypass the
	// injectable default and pin a boundary no test covers — single clock
	// owner, set at the store.
	// Budget composition note: the 5s verb budget covers validation only.
	// CreateState's dup-check, delete, and insert share one detached 5s
	// quickCtx inside the store — sequential, so one tap costs ~5s
	// worst-case wall. Detached IO does not stop the verb clock, so the
	// no-split handling below answers the detached Upstream as-is (not
	// the timeout wording). That is honest either way — slow means
	// retry, and re-tap supersedes. Every mint has a cleanup: the two
	// failure branches below plus the gone-client-before-200 branch share
	// the same detached-budget ConsumeState (see cleanupOrphanState) — grep
	// all three before assuming a path mints without cover.
	// Verb ctx covers validation + mint only here: CreateState ignores it
	// for IO (detached quickCtx inside the store, `_` param) — so the verb
	// deadline below can only fire on validation/mint, never on slow Atlas.
	// Matches the store's own budget note; re-tap recovers either way.
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
		// No timeout split here (unlike list/disconnect/callback): the
		// verb ctx never reaches IO — delete + insert run on detached
		// budgets, one of which always outlives the 5s verb on slow Atlas.
		// Branching on the verb deadline would answer the timeout
		// contract for detached-IO failures the verb clock never
		// bounded. Detached IO errors already carry their own taxonomy
		// (upstream 502 via the domain mapper below) — that is the
		// honest answer. Gone clients still short-circuit: writing on a
		// dead conn buys only a second log line for a hangup — but the
		// row may already have landed on detached IO, so clean it up
		// best-effort like the empty-auth_url branch below (same detached
		// budget, same single-use delete). A failed or abandoned build
		// mints nothing observable either way. Duplicate-key failure is
		// the exception: the colliding row predates this call (same
		// 128-bit state already live from an earlier tap), so consuming
		// it would burn a legitimate attempt this call never minted —
		// skip cleanup on the already-exists shape, answer the error. The DeadlineExceeded leg
		// below is nearly dead by construction: CreateState runs on
		// detached opCtx (validation + mint are the only verb-clock work,
		// ~µs), so only a hung client or depleted entropy trips it — a
		// slow Atlas surfaces via the detached Upstream branch above, not
		// here. Both legs clean up + answer honestly regardless.
		if ctx.Err() != nil {
			// Log first: triage distinguishes hangup/shutdown from a slow
			// Atlas verb deadline below by this line, not the cleanup
			// outcome — the row may already have landed on detached IO in
			// either case, so both branches clean up best-effort on a
			// detached budget (same single-use delete as the empty-auth_url
			// branch). A failed or abandoned build mints nothing observable.
			s.cfg.Logf("connectors: connect aborted, request ended (client gone, server stopping, or verb deadline): %v", ctx.Err())
			if !isDuplicateStateError(err) {
				s.cleanupOrphanState(id, state)
			}
			if ctx.Err() != context.DeadlineExceeded {
				return
			}
			// Verb-deadline answers the single timeout 502 like every
			// sibling verb (storeTimeoutOrGone), not the detached generic:
			// one grep term for one fault, even though this leg is nearly
			// dead by construction (see above).
			storeTimeoutOrGone(w, s, ctx, err, "connect")
			return
		}
		// Generic failure below the gone-client split (validation 400s
		// already returned above; this is the detached-IO Upstream leg):
		// the row may have landed after a supersede-delete, so clean up
		// best-effort like the sibling branches — an
		// insert-timeout-after-landing must not strand a live row no
		// client holds. Skipped only for the duplicate shape, which
		// predates this call (same guard as the deadline leg above).
		if !isDuplicateStateError(err) {
			s.cleanupOrphanState(id, state)
		}
		connectorsError(w, err)
		return
	}
	// Detached-Upstream answers the generic 502 here (not the timeout
	// contract): CreateState runs detached by design, so a slow-Atlas
	// failure is not a verb-deadline fault and must not borrow its
	// wording — triage greps this path by "create failed", the verb path
	// by "did not answer in time", and the log line names the op either
	// way. Intentional divergence from the sibling verbs; re-tap recovers.
	// Fail-closed builder: empty means a helper refused (nil cfg, unknown
	// id — both pre-checked above, so this is defense in depth, not a live
	// branch). Never hand the app a 200 with an empty auth_url it would
	// open as nothing with no error to triage.
	authURL := connectors.AuthURL(s.cfg.ConnectorOAuth, id, state, verifier, s.cfg.Logf)
	if authURL == "" {
		// No log here: the helper already logged which guard fired.
		// A second line would split triage across two entries for one
		// failure. Best-effort cleanup of the just-minted row: an
		// unreachable live state would otherwise sit until TTL/next tap
		// — harmless, but the failed build must mint nothing observable.
		// Same single-use delete the callback uses; failure only logs.
		// Detached budget, not the verb ctx: CreateState's own delete +
		// insert run detached, so the verb ctx can be near-deadline at
		// exactly this point — a fired deadline must not turn the
		// cleanup into a certain failure.
		s.cleanupOrphanState(id, state)
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"connect is not configured correctly on this server")
		return
	}
	// Gone client before the 200: the row landed on detached IO above, but
	// the app hung up mid-build — writing auth_url to a dead conn mints a
	// live 10-minute attempt nobody will ever complete. Clean it up
	// best-effort like the failure branches (same detached budget, same
	// single-use delete), so a successful build observed by nobody mints
	// nothing either.
	if ctx.Err() != nil {
		s.cfg.Logf("connectors: connect aborted, request ended (client gone, server stopping, or verb deadline): %v", ctx.Err())
		s.cleanupOrphanState(id, state)
		if ctx.Err() == context.DeadlineExceeded {
			// Same timeout 502 as the CreateState-failure branch: the row
			// is already cleaned up above, so only the status is left to
			// answer — never an empty 200 the app cannot triage. Typed
			// error, not nil: storeTimeoutOrGone must never receive a nil
			// err it could deref on a non-deadline fallthrough.
			storeTimeoutOrGone(w, s, ctx, connectors.NewStoreError("connect aborted", false), "connect")
		}
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
	// Bound the raw query before anything else — including the unconfigured
	// 501 below. The cap carries no config information (same 400 shape
	// configured or not), so ordering it first removes the config oracle a
	// huge query would otherwise add, and unconfigured callers get the same
	// parse protection as configured ones. Sized for escaping expansion:
	// raw counts encoded bytes (up to 3x decoded: %00 is 3 raw bytes for 1
	// decoded byte), so three max-size values at full expansion plus slack —
	// never tighter than the decoded per-value gates below, which stay the
	// real contract for legitimately large inputs. Single-value assumption:
	// repeated tiny params (?scope=a&scope=a&…& — ~7 bytes raw per decoded
	// byte) can trip the raw cap first — same 400 either way, no oracle,
	// just a coarser fast path for a shape Google never sends. Budgets all three
	// attacker-influenced values (code + state + scope) at full expansion,
	// so a triple at each per-value cap still reaches the per-value gates.
	// DoS hygiene first: a junk-suffixed but otherwise valid attempt
	// (?code=<valid>&x=<10k>) dead-attempts here instead of reaching those
	// gates — same 400 shape, so no oracle, but the fast-path gate is
	// deliberately coarser than the per-value contract.
	if len(r.URL.RawQuery) > 3*(3*connectors.MaxStateFieldLen)+256 {
		callbackDeadAttempt(w, r, s.cfg.Logf)
		return
	}
	if s.cfg.ConnectorOAuth == nil {
		callbackError(w, r, http.StatusNotImplemented, "invalid_request_error",
			"callback is not configured on this server yet — never paste a token in chat", s.cfg.Logf)
		return
	}
	// Parse the query once and reuse it: each Query() call re-parses and
	// re-allocates the full map, and this unauthenticated entry point
	// pays that before any fast-path reject runs. First value wins:
	// duplicates (?code=a&code=b) are not rejected — the first value is
	// exchanged and the rest dropped, deterministically and safely
	// (single-use state burns either way, the second value never reaches
	// Google), so do not "fix" this into a 400 that changes the
	// dead-attempt shape.
	// (Raw length already gated above: url.Query() allocates the full map
	// up front, and raw counts encoded bytes — percent-encoding expands,
	// so %00-heavy payloads hit the raw 400 before the decoded per-value
	// gates below ever run. Query() also discards parse errors: %zz-style
	// malformed encoding yields empty values and the missing-params 400,
	// not the dead-attempt shape — both are 400 by design, and the split
	// is intentional, so do not "fix" it into one branch.)
	query := r.URL.Query()
	code := connectors.TrimCredential(query.Get("code"))
	state := connectors.TrimCredential(query.Get("state"))
	// Duplicates first-win by design (see above), but a junk-suffixed
	// duplicate leaves no trace of the second value — log lengths only
	// (never values), same pointer class as the oversized-scope skip.
	if len(query["code"]) > 1 || len(query["state"]) > 1 {
		s.cfg.Logf("connectors: callback with duplicate params (code=%d, state=%d values), using first", len(query["code"]), len(query["state"]))
	}
	// Absent and blank collapse to one branch: a whitespace-only value
	// carries no credential, exactly like a missing one — and the message
	// names what's missing rather than crying a dead attempt that never
	// existed. One 400 body for both, so no branch distinguishes them.
	if code == "" || state == "" {
		callbackError(w, r, http.StatusBadRequest, "invalid_request_error",
			"callback needs ?code= and ?state=; start over from connect", s.cfg.Logf)
		return
	}
	// Length-gated before the store/Google calls (shared
	// connectors.MaxStateFieldLen — the store enforces the same bound, so
	// the gateway cap is the fast path, not the only enforcement): states
	// are ~22-char base64url, codes a few hundred chars in practice. A
	// multi-MB value would become an oversized DB filter / outbound token
	// request. Same 400 shape as every other callback failure, so no
	// oracle is added.
	// All pre-consume shape rejects dead-attempt like the consume failures
	// below: same 400, same JSON (callbackStateReject), one HTML guidance
	// page ("tap connect again") instead of two wordings that the next
	// reader would "unify" anyway. No oracle: oversized/short is
	// self-revealing input, and the message never varies by validity.
	if len(code) > connectors.MaxStateFieldLen || len(state) > connectors.MaxStateFieldLen {
		callbackDeadAttempt(w, r, s.cfg.Logf)
		return
	}
	// Floor on state only: states are server-minted (22-char 128-bit), so a
	// short state is hand-minted junk — dead-attempt before it can burn the
	// single-use row on a Google round-trip. Codes carry no minimum
	// (Google defines no floor): a short code reaches ConsumeState/Exchange
	// and fails there with its own discriminant instead of burning as an
	// "unknown attempt".
	if len(state) < connectors.MinStateLen {
		callbackDeadAttempt(w, r, s.cfg.Logf)
		return
	}
	// Control bytes fail closed here, not at Google: an embedded NUL/CR/LF
	// would otherwise become a Mongo filter and an outbound token request
	// before failing. Same dead-attempt shape as the oversized branch —
	// the values are server-minted or Google-issued, so a control byte is
	// never a legitimate credential.
	if connectors.HasControl(code) || connectors.HasControl(state) || connectors.HasWhitespace(code) || connectors.HasWhitespace(state) {
		callbackDeadAttempt(w, r, s.cfg.Logf)
		return
	}
	// Invalid UTF-8 fails here too: overlong/invalid byte sequences decode
	// as RuneError (not control), so the control gate alone would miss
	// them — and a BSON filter built from them fails downstream as a 502
	// instead of here as a 400. Same dead-attempt shape, no oracle.
	if !utf8.ValidString(code) || !utf8.ValidString(state) {
		callbackDeadAttempt(w, r, s.cfg.Logf)
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
				"the connector store did not answer in time", s.cfg.Logf)
			return
		}
		if ctx.Err() != nil {
			s.cfg.Logf("connectors: callback aborted, request ended (client gone or server stopping): %v", ctx.Err())
			return
		}
		// Unknown, consumed, or expired: same dead-attempt answer, "try
		// connect again". The store already refuses to distinguish; the
		// gateway must not add an oracle on top by varying the message.
		// Layer discriminant note: short states answer NotFound at the
		// store ("unknown or expired") but dead-attempt here — same 400
		// meaning on the wire either way, since this pre-check runs first.
		// Only direct store callers ever see the store string.
		// Upstream store failures are the exception: those 502 via the
		// domain mapper (transient, retryable) instead of burning the
		// user's live state with a "start over" that would supersede it.
		// Browser tabs get the close-this-tab page, API clients the JSON
		// shape — same 400 meaning either way.
		var se *connectors.StoreError
		if errors.As(err, &se) && se.Upstream {
			status, typ, msg := connectorsErrorMapped(err)
			callbackError(w, r, status, typ, msg, s.cfg.Logf)
			return
		}
		callbackDeadAttempt(w, r, s.cfg.Logf)
		return
	}
	id := connectors.NormalizedID(st.Connector)
	// Folded here, not just downstream: Exchange/SetTokens/SetStatus all
	// re-fold, but the log lines below must observe the same spelling the
	// store sees — stored == logged == sent by construction.
	// Verifier gated like code/state above: server-minted base64url in
	// prod, but a hand-inserted row could carry controls into an outbound
	// request and a log line. Same dead-attempt shape, no oracle. The row
	// is already consumed at this point (verifier lives inside it, so the
	// check cannot run first) — burning a hand-inserted row is intended;
	// prod rows pass CreateState's own controls gate and never reach here.
	// All legs burn: controls, invalid UTF-8, whitespace, and length alike
	// (only code/state fail pre-consume; the verifier can only be checked
	// after the read).
	if connectors.HasControl(st.Verifier) || !utf8.ValidString(st.Verifier) || connectors.HasWhitespace(st.Verifier) || len(st.Verifier) > connectors.MaxVerifierLen || len(st.Verifier) < connectors.MinVerifierLen {
		callbackDeadAttempt(w, r, s.cfg.Logf)
		return
	}
	// Pinned-redirect misshape burns the same way, in the same place:
	// CreateState shape-checks at mint, so only a hand-inserted row
	// carries a bad redirect here — and the row is already consumed, so
	// this dead-attempts exactly like the verifier leg above instead of
	// failing later inside Exchange after the Google round-trip.
	if st.RedirectURI != "" {
		// Re-canonicalise and send the output, not the verbatim row:
		// CreateState canonicalise-and-stores today, but a row minted by
		// an older build (or a canonicaliser change) could hold a value
		// that validates yet is not byte-identical to fresh output — and
		// Google exact-matches the sent bytes. Stored == sent ==
		// validated by construction on every path, not just mint.
		redirect, rerr := connectors.CanonicalRedirectURL(st.RedirectURI)
		if rerr != nil {
			callbackDeadAttempt(w, r, s.cfg.Logf)
			return
		}
		st.RedirectURI = redirect
	}
	// Send the pinned redirect from mint time, not the live config: a
	// rotation mid-consent would otherwise exact-mismatch Google.
	// Empty means minted before this column existed: fall back to the
	// live config rather than failing an otherwise valid attempt. That
	// fallback is best-effort, not a pin: after a rotation the minted
	// (old) redirect_uri can never equal the fallback (new config), so a
	// pre-column row may pay a doomed Google round-trip instead of
	// dead-attempting fast. Narrow window (deploy + rotation inside 10
	// min, pre-column rows only); re-tap recovers either way — triage
	// reads that 502 as rotation fallout, not a helper bug.
	exCfg := s.cfg.ConnectorOAuth
	if st.RedirectURI != "" {
		pinned := *exCfg
		pinned.RedirectURL = st.RedirectURI
		exCfg = &pinned
		// Log the divergence when pinned != live: an operator rotation
		// gets confirmation which string was sent, and a hand-inserted
		// row carrying a foreign redirect is visible here instead of
		// silently sent to Google. Same warn-pattern as the subset-scope
		// line below (mint requires bearer, so not an anonymous vector).
		if st.RedirectURI != s.cfg.ConnectorOAuth.RedirectURL {
			s.cfg.Logf("connectors: callback for %q uses pinned redirect %q (live config differs; rotation or hand-inserted row)", id, connectors.RedactURL(st.RedirectURI))
		}
	} else {
		// Fallback, not a pin: pre-column rows have no stored redirect, so
		// the live config is the only candidate. Auditable like the pinned
		// path above — after a rotation this sends the NEW string for a row
		// minted under the OLD one, so the 502 it may produce reads as
		// rotation fallout in the log, not a helper bug. Keyword for the
		// post-rotation grep: rotation suspected.
		s.cfg.Logf("connectors: callback for %q uses live redirect %q (no pinned value; pre-column row or fallback — rotation suspected if recently rotated)", id, connectors.RedactURL(s.cfg.ConnectorOAuth.RedirectURL))
	}
	// Granted-scope subset warns, not fails: Google appends the granted
	// scopes (?scope=, space-delimited) and granular consent or admin
	// restriction can return fewer than requested. The grant still stores
	// and flips (read paths keep working on a subset); a later draft 403
	// traces back to this line, not to the relay. Enforcement belongs to
	// the consumers that need the missing scope (follow-up MCP servers),
	// not to the mint path, which cannot know which calls the app will make.
	// Length-gated like code/state: the value is attacker-influenced and
	// only feeds a log line plus set compare, so oversized input skips the
	// check instead of paying the split on megabytes. The skip logs: a
	// junk-suffixed scope that suppresses the subset warning must leave
	// its own trace, or a later draft 403 has no line to trace back to.
	// Trimmed first like code/state: whitespace-only is absent, not a
	// subset — blank == absent on every leg, so "  " never warns.
	// Total gate implies the per-value gate: joined form only shrinks
	// under TrimCredential, so anything clearing the total necessarily
	// clears the length check below — the two gates cannot disagree no
	// matter how the separator changes. Only clean, in-budget values are
	// compared. All
	// values joined first: a duplicated ?scope= compares the union, not
	// just the first, so a junk second value cannot suppress the warning
	// (same pointer class as the duplicate-params length log above).
	// Length before the join (same bound): sum the parts plus separators
	// so a junk-suffixed multi-value scope skips without paying the
	// Join + Fields alloc — matching the raw-query fast-path intent above.
	scopeTotal := 0
	for _, v := range query["scope"] {
		scopeTotal += len(v) + 1
	}
	if scopeTotal > 0 {
		scopeTotal-- // separators join n values with n-1 spaces
	}
	if scopeTotal > connectors.MaxStateFieldLen+1 {
		s.cfg.Logf("connectors: grant for %q scope params oversized (%d bytes), subset check skipped (grant stored; drafts may 403)", id, scopeTotal)
	} else if granted := connectors.TrimCredential(strings.Join(query["scope"], " ")); granted != "" && len(granted) <= connectors.MaxStateFieldLen && !connectors.HasControl(granted) && utf8.ValidString(granted) {
		if missing := missingScopes(connectors.GoogleScopes(id), granted); len(missing) > 0 {
			// Bounded like every interpolated value on this path: missing
			// holds server-side scope constants today, but the next scope
			// string edit must not inherit an uncapped pattern.
			s.cfg.Logf("connectors: grant for %q missing requested scopes %q (granted %q; subset granted, drafts may 403)", id, bounded(strings.Join(missing, " ")), bounded(approvals.ScrubError(granted)))
		}
		// Clean path logs nothing by design: no missing scopes means the
		// grant was full — or the param was absent, which reads as assumed
		// full (Google omits scope on some full grants, so absence cannot
		// fail). A subset delivered without the param is therefore invisible
		// by design: no log line distinguishes it from a true full grant,
		// and a later draft 403 on such a row traces to a downstream
		// restriction, not to a skipped check. Only the skip branch below
		// is silent-comparison, and it logs.
	} else if len(granted) > 0 {
		// Oversized, control-bearing, or undecodable: skip the compare
		// like the oversized leg, never the grant. Same blank==absent
		// contract as code/state — only non-empty values reach here, and
		// only clean ones are compared. Logged length is the post-trim
		// value the compare would have seen, not the raw sum — so triage
		// sizes the attacker input from what mattered, not the padding.
		s.cfg.Logf("connectors: grant for %q scope param unusable (%d bytes trimmed), subset check skipped (grant stored; drafts may 403)", id, len(granted))
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
				"the grant exchange did not answer in time", s.cfg.Logf)
			return
		}
		if ctx.Err() != nil {
			s.cfg.Logf("connectors: callback aborted, request ended (client gone or server stopping): %v", ctx.Err())
			return
		}
		var se *connectors.StoreError
		if errors.As(err, &se) && se.NotFound {
			// Unknown connector id inside a consumed state (hand-inserted
			// row — CreateState rejects unknown ids, so no live flow gets
			// here): same dead-attempt 400 as every other failure on this
			// unauthenticated path, never a 404 oracle on id validity.
			callbackDeadAttempt(w, r, s.cfg.Logf)
			return
		}
		status, typ, msg := connectorsErrorMapped(err)
		callbackError(w, r, status, typ, msg, s.cfg.Logf)
		return
	}
	cfg := s.cfg.ConnectorOAuth
	// Trimmed copies are what get sealed: a padded value would otherwise
	// pass the non-empty checks and seal with padding the refresher cannot
	// use (same class as the whitespace cases fixed earlier — checked and
	// stored bytes must be the same string). Exchange already normalises
	// on return; this trim is the backstop, not the owner — do not remove
	// either side without checking the other still holds.
	refreshToken := connectors.TrimCredential(tok.RefreshToken)
	accessToken := connectors.TrimCredential(tok.AccessToken)
	refreshSealed, err := connectors.EncryptToken(cfg.Key, refreshToken)
	if err != nil {
		// Logged, not answered in detail: seal failures are server-side
		// (rand, zero key) and carry nothing the client can act on.
		s.cfg.Logf("connectors: token seal failed for %q: %q", id, bounded(approvals.ScrubError(err.Error())))
		callbackError(w, r, http.StatusBadGateway, "upstream_error",
			"could not store the grant", s.cfg.Logf)
		return
	}
	// Empty access tokens stay empty: sealing "" would create a second
	// representation of "missing" that the refresher must also recognise.
	// The TokenRow contract names literal "" as the missing form.
	var accessSealed string
	if accessToken != "" {
		accessSealed, err = connectors.EncryptToken(cfg.Key, accessToken)
		if err != nil {
			s.cfg.Logf("connectors: token seal failed for %q: %q", id, bounded(approvals.ScrubError(err.Error())))
			callbackError(w, r, http.StatusBadGateway, "upstream_error",
				"could not store the grant", s.cfg.Logf)
			return
		}
	}
	var expiry int64
	if accessToken != "" && !tok.Expiry.IsZero() {
		expiry = tok.Expiry.UnixMilli()
		// Clamp absurd futures: a far-future expires_in (hand-built
		// token, clock skew) would otherwise read as "don't renew for
		// decades". Same unbounded-lifetime class as the state clamp;
		// the stores re-enforce the same bound and win: with a stepped
		// fake clock the store clamp can only shrink this wall-clock max
		// (into the past at worst), and the refresher treats past as
		// renew-now per TokenRow — never as "don't renew". Wall-clock
		// deliberately: this is a display-adjacent bound, not an expiry
		// input — the injectable clock (see connectors timeNow) owns
		// state-expiry ticks, and the store re-clamps on its own clock at
		// write time, so a stepped fake clock and this gate never have
		// to agree; agreement is not required because the store is the
		// single owner and this gate is advisory.
		expiry = connectors.ClampAccessExpiry(expiry, time.Now())
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
	// Known race, accepted (see ADR-0009): a disconnect landing during the
	// Exchange above clears tokens, then this write flips the row back to
	// connected — re-tap disconnect is the recovery, and triage must read
	// a resurrect as that race, not a scrub regression.
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
	// Same envelope helper as every other branch on this path, so the
	// success page cannot drift from the dead-attempt page.
	if acceptsHTML(r, s.cfg.Logf) {
		writeCallbackPage(w, http.StatusOK, "Agento connected",
			"Connected. You can close this tab and return to the Agento app.")
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{"ok": true, "connector": connectorWire(c)})
}
