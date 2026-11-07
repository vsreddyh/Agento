package main

// Connector routes: the Gmail/Calendar OAuth surface over the shared
// connectors collection (#488).
//
// The app used to reach these verbs on pi-gateway (:8643); they live here
// now so connect/list/disconnect keep working with the pi container
// stopped. Same contract, same Mongo rows (internal/connectors owns the
// store and the OAuth helpers — this file is only the HTTP envelope):
//
//	GET  /api/connectors                 list the two v1 rows
//	POST /api/connectors/{id}/connect    mint state+verifier, return the
//	                                     Google consent auth_url (501 when
//	                                     OAuth is unconfigured)
//	POST /api/connectors/{id}/disconnect stop future access
//	GET  /api/connectors/callback        single registered redirect;
//	                                     consumes the state once, exchanges
//	                                     server-side, seals tokens, flips
//	                                     to connected
//
// Everything except the callback sits behind the bearer token like every
// other non-health route. The callback skips it: a Google-redirected
// browser carries no Authorization header, and the state is the
// capability. An expired row is the reconnect prompt: the app banners it
// and offers the connect flow, never a password field.

import (
	"context"
	"errors"
	"html"
	"log"
	"net/http"
	"strings"
	syncpkg "sync"
	"time"
	"unicode/utf8"

	"agento/internal/approvals"
	"agento/internal/connectors"

	"go.mongodb.org/mongo-driver/bson"
)

// connectorVerbBudget bounds the fast verbs (list/disconnect/connect
// validation). The callback gets its own longer budget below: it binds a
// state consume plus a live Google exchange.
const connectorVerbBudget = 5 * time.Second

// connectorCallbackBudget bounds one full OAuth callback: state consume +
// Google exchange + sealed writes + status flip, all sequential against
// live Google.
const connectorCallbackBudget = 30 * time.Second

// connectorDeadAttempt is the single answer for every dead OAuth attempt —
// unknown, consumed, expired, or oversized state. One string so no branch
// can drift into an oracle that distinguishes them.
const connectorDeadAttempt = "that connect attempt expired or was already used; start over from connect"

// connectorStore opens the shared connectors collection (the same rows the
// gmail/calendar MCP servers read grants from). Cached process-wide like
// the other stores — including the memory fallback, so statuses and
// pending states survive between requests and the log stays quiet.
// Without MongoDB it falls back to memory that cannot meet a
// restart — same contract as the gateway: the list still answers, but
// connect answers 501 (nothing durable to store a grant in). A Mongo
// recovery still needs a restart to re-cache; failures are loud in the
// log either way.
var (
	connectorStoreMu syncpkg.Mutex
	connectorCache   connectors.Store
)

func connectorStore() connectors.Store {
	connectorStoreMu.Lock()
	defer connectorStoreMu.Unlock()
	if connectorCache != nil {
		return connectorCache
	}
	var backend connectors.Store
	if ms, err := connectors.FromEnv(); err != nil {
		log.Printf("connectors: falling back to in-process (statuses do not survive restart): %v", err)
		backend = connectors.NewMemoryStore()
	} else {
		backend = ms
	}
	connectorCache = backend
	return backend
}

// connectorBounded caps one echoed string for API bodies, mirroring the
// gateway's bounded(): rune-cut with a visible marker, never silent.
func connectorBounded(s string) string {
	const maxConnectorEcho = 128
	runes := []rune(s)
	if len(runes) <= maxConnectorEcho {
		return s
	}
	return string(runes[:maxConnectorEcho]) + "… (truncated)"
}

// connectorOAuth resolves the Google OAuth config when it can serve one:
// all four values present AND a durable store behind them. Anything less
// is unconfigured and connect/callback answer 501 — a half-open flow
// would strand the user mid-consent.
func connectorOAuth(store connectors.Store) *connectors.OAuthConfig {
	if !store.Durable() {
		return nil
	}
	cfg, err := connectors.OAuthConfigFromEnv()
	if err != nil {
		return nil
	}
	return cfg
}

// ensureConnectorIndexes creates the states TTL (pending OAuth attempts
// reap themselves) best-effort at startup. Without it abandoned states
// linger until superseded — harmless, but the index is what reaps them.
func ensureConnectorIndexes() {
	store, err := connectors.FromEnv()
	if err != nil {
		// Loud, not silent: without Mongo there is no states TTL, and an
		// operator grepping boot logs must see why.
		log.Printf("connectors: no database, skipping index setup (oauth states will not reap): %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := store.EnsureIndexes(ctx); err != nil {
		log.Printf("connectors: WARNING index setup failed, oauth states will not reap: %v", err)
	}
}

// logConnectorOAuth prints the canonical callback URL at startup so the
// operator can compare byte-for-byte against the Cloud console
// registration — the same triage line pi-gateway prints, now from the
// service that actually serves the callback. Storable values log whole;
// compare against this line, never the input spelling.
func logConnectorOAuth() {
	store, err := connectors.FromEnv()
	if err != nil {
		log.Printf("connectors: OAuth unavailable (no database: %v); connect answers 501", err)
		return
	}
	if !store.Durable() {
		log.Printf("connectors: OAuth unavailable (no durable store); connect answers 501")
		return
	}
	cfg, err := connectors.OAuthConfigFromEnv()
	if err != nil {
		log.Printf("connectors: OAuth unavailable (%v); connect answers 501", err)
		return
	}
	canonical, err := connectors.CanonicalRedirectURL(cfg.RedirectURL)
	if err != nil {
		log.Printf("connectors: OAuth redirect invalid (want %q): %v",
			connectors.RedactURL(cfg.RedirectURL), err)
		return
	}
	log.Printf("connectors: OAuth configured for %s (callback %q)",
		strings.Join(connectors.ListIDs(), ", "), canonical)
}

// connectorWire renders one row the way the app reads it: status + scopes
// only. Refresh tokens live server-side, sealed, and never appear here.
func connectorWire(c connectors.Connector) bson.M {
	scopes := c.Scopes
	if scopes == nil {
		scopes = []string{}
	}
	out := bson.M{
		"id":           c.ID,
		"display_name": c.DisplayName,
		"scopes":       scopes,
		"status":       string(c.Status),
	}
	// Last error, scrubbed + bounded like the gateway: the store column
	// can carry provider text, and the app renders this verbatim.
	if c.LastError != "" {
		out["last_error"] = connectorBounded(approvals.ScrubError(c.LastError))
	}
	// The stored stamp is first-connected-at, retained across
	// disconnect/expire: emitting it on an off-state row would read as
	// "connected since X". A zero stamp on a connected row (corrupt)
	// omits too — fail closed rather than stamping now.
	if c.Status == connectors.StatusConnected && c.ConnectedAt != 0 {
		out["connected_at"] = c.ConnectedAt
	}
	if c.UpdatedAt != 0 {
		out["updated_at"] = c.UpdatedAt
	}
	// Reconnect hint: expired rows banner, nothing else does.
	if c.Status == connectors.StatusExpired {
		out["reconnect"] = "this session expired; reconnect in Settings — never paste a password or token in chat"
	}
	return out
}

// writeConnectorErr maps store domain errors: unknown ids 404, transport
// failures 502, anything else 400. The id echo is safe: callers only reach
// it with a normalized known id or the fixed "unknown connector" string.
func writeConnectorErr(w http.ResponseWriter, err error) {
	var se *connectors.StoreError
	if errors.As(err, &se) {
		switch {
		case se.NotFound:
			writeJSON(w, http.StatusNotFound, bson.M{"detail": "unknown connector"})
			return
		case se.Upstream:
			writeJSON(w, http.StatusBadGateway, bson.M{"detail": "the connector store did not answer in time"})
			return
		}
	}
	writeJSON(w, http.StatusBadRequest, bson.M{"detail": "invalid connector request"})
}

// listConnectors serves GET /api/connectors: the two v1 rows, disconnected
// synthesised on fresh deploys.
func listConnectors(w http.ResponseWriter, r *http.Request) {
	// no-store first: the app polls this loop, and a heuristic cache
	// would serve a stale disconnected after the callback flipped to
	// connected.
	w.Header().Set("Cache-Control", "no-store")
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "method not allowed"})
		return
	}
	store := connectorStore()
	ctx, cancel := context.WithTimeout(r.Context(), connectorVerbBudget)
	defer cancel()
	list, err := store.List(ctx)
	if err != nil {
		writeConnectorErr(w, err)
		return
	}
	out := make([]any, 0, len(list))
	for _, c := range list {
		out = append(out, connectorWire(c))
	}
	writeJSON(w, http.StatusOK, bson.M{"connectors": out})
}

// connectorItem dispatches POST /api/connectors/{id}/connect and
// POST /api/connectors/{id}/disconnect. Auth first; unknown ids and verbs
// 404 without touching OAuth config (disconnect never reads it).
func connectorItem(w http.ResponseWriter, r *http.Request) {
	// no-store like every other verb on this surface: the success body
	// completes a grant, so nothing here may sit in a heuristic cache.
	w.Header().Set("Cache-Control", "no-store")
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/connectors/"), "/")
	id, action, _ := strings.Cut(rest, "/")
	// Alloc guard before any fold or scan: a multi-MB segment answers the
	// same oversized 400 as the contract gate below, without paying the
	// trim/fold/scrub allocs first. No echo — the value is megabytes.
	if len(id) > connectors.MaxSegmentAllocLen {
		writeConnectorErr(w, connectors.NewStoreError("oversized connector id", false))
		return
	}
	if len(action) > connectors.MaxSegmentAllocLen {
		writeConnectorErr(w, connectors.NewStoreError("oversized path segment", false))
		return
	}
	// Trim first, then check: edge whitespace and invisible format
	// characters are forgiven, interior controls fail closed, and
	// empty-after-trim stays on its natural branch (blank id 404s,
	// blank verb hits the no-verb 405).
	id = connectors.TrimCredential(id)
	action = connectors.TrimCredential(action)
	// Length on the trimmed form, before any scan or fold: padding must
	// not turn a known id unknown, and anything over the cap skips the
	// rune walks below.
	if len(id) > connectors.MaxConnectorIDLen {
		writeConnectorErr(w, connectors.NewStoreError("oversized connector id", false))
		return
	}
	if len(action) > connectors.MaxConnectorIDLen {
		writeConnectorErr(w, connectors.NewStoreError("oversized path segment", false))
		return
	}
	if id == "" || connectors.HasControl(id) || !utf8.ValidString(id) {
		// Bare 404, no echo: control bytes and undecodable input never
		// reach a filter or a log line verbatim.
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "unknown connector"})
		return
	}
	if connectors.HasControl(action) || !utf8.ValidString(action) {
		writeJSON(w, http.StatusNotFound, bson.M{
			"detail": "unknown path " + connectorBounded(approvals.ScrubError(action))})
		return
	}
	// Verbs fold like ids: a hand-tested "Disconnect" lands on the verb
	// instead of 404ing as an unknown path. Trimmed above, folded here —
	// the switch below sees only lowercase.
	action = strings.ToLower(action)
	// Normalise before Known: case and spacing fold, while a truly
	// unknown id still 404s. Length re-checked post-fold (folding exotic
	// spellings can grow byte length).
	norm := connectors.NormalizedID(id)
	if len(norm) > connectors.MaxConnectorIDLen {
		writeConnectorErr(w, connectors.NewStoreError("oversized connector id", false))
		return
	}
	if !connectors.Known(norm) {
		// Echo the normalised id (what the store would see), scrubbed +
		// bounded — triage greps the spelling the store saw.
		writeJSON(w, http.StatusNotFound, bson.M{
			"detail": "unknown connector " + connectorBounded(approvals.ScrubError(norm))})
		return
	}
	id = norm
	switch action {
	case "disconnect":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "method not allowed"})
			return
		}
		store := connectorStore()
		ctx, cancel := context.WithTimeout(r.Context(), connectorVerbBudget)
		defer cancel()
		c, err := store.Disconnect(ctx, id)
		if err != nil {
			writeConnectorErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, bson.M{"ok": true, "connector": connectorWire(c)})
	case "connect":
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "method not allowed"})
			return
		}
		connectorConnect(w, r, id)
	case "":
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed,
			bson.M{"detail": "no verb on /api/connectors/{id}; use /disconnect or /connect"})
	default:
		// Echo the normalised action (what the switch saw), scrubbed +
		// bounded — triage greps the spelling the store would have seen.
		writeJSON(w, http.StatusNotFound, bson.M{
			"detail": "unknown path " + connectorBounded(approvals.ScrubError(action))})
	}
}

// cleanupConnectorOrphanState best-effort consumes a just-minted state on
// a detached budget (the verb ctx may be dead at exactly this point).
// Only real IO failures log: NotFound means no row was ever minted, which
// is the desired end state — not a failure. A failed build must mint
// nothing observable.
func cleanupConnectorOrphanState(id, state string) {
	cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cleanCancel()
	if _, cerr := connectorStore().ConsumeState(cleanCtx, state); cerr != nil {
		var se *connectors.StoreError
		if errors.As(cerr, &se) && se.Upstream {
			log.Printf("connectors: orphan state cleanup for %q failed: %q", id, connectorBounded(approvals.ScrubError(cerr.Error())))
		}
	}
}

// connectorConnect starts one OAuth attempt: mints state+verifier, records
// the pending attempt, and returns the Google consent URL the app opens in
// a browser. The app never sees the verifier, the code, or any token.
func connectorConnect(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Cache-Control", "no-store")
	store := connectorStore()
	oauth := connectorOAuth(store)
	if oauth == nil {
		log.Print("connectors: connect attempted while OAuth is unconfigured (missing values or no durable store)")
		writeJSON(w, http.StatusNotImplemented,
			bson.M{"detail": "connect is not configured on this server yet — never paste a token in chat"})
		return
	}
	pinnedRedirect, err := connectors.CanonicalRedirectURL(oauth.RedirectURL)
	if err != nil {
		// Echo the attempted redirect (non-secret): triage compares it
		// against the console registration without re-reading .env.
		log.Printf("connectors: connect redirect invalid (want %q): %v",
			connectors.RedactURL(oauth.RedirectURL), err)
		writeJSON(w, http.StatusBadGateway, bson.M{"detail": "connect is not configured correctly on this server"})
		return
	}
	state, verifier, err := connectors.NewState()
	if err != nil {
		log.Print("connectors: could not mint connect state")
		writeJSON(w, http.StatusBadGateway, bson.M{"detail": "could not start the connect flow"})
		return
	}
	// Persist first, build the URL second: a down store fails fast with no
	// dead attempt minted. The pinned redirect travels with the row so a
	// config rotation mid-consent cannot strand it with a mismatch. The
	// verb budget covers the whole attempt the app waits on.
	ctx, cancel := context.WithTimeout(r.Context(), connectorVerbBudget)
	defer cancel()
	if err := store.CreateState(ctx, connectors.OAuthState{
		State:       state,
		Connector:   id,
		Verifier:    verifier,
		RedirectURI: pinnedRedirect,
	}); err != nil {
		writeConnectorErr(w, err)
		return
	}
	authURL := connectors.AuthURL(oauth, id, state, verifier, log.Printf)
	if authURL == "" {
		// A failed build mints nothing observable: consume the just-minted
		// row best-effort so no live attempt lingers that no client holds.
		cleanupConnectorOrphanState(id, state)
		writeJSON(w, http.StatusBadGateway, bson.M{"detail": "connect is not configured correctly on this server"})
		return
	}
	writeJSON(w, http.StatusOK, bson.M{"ok": true, "auth_url": authURL})
}

// connectorCallbackPage answers a browser tab with the close-this-tab
// page. Both envelopes escape inside the helper: callers pass raw text
// (static guidance or a scrubbed domain message) and the helper escapes
// it, so a future branch interpolating a connector id or provider string
// lands escaped in a browser-parsed page. Safe by construction, not by
// discipline; static strings escape to themselves.
func connectorCallbackPage(w http.ResponseWriter, code int, title, body string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)
	_, _ = w.Write([]byte(`<!doctype html><html><head><title>` + html.EscapeString(title) +
		`</title></head><body><p>` + html.EscapeString(body) + `</p></body></html>`))
}

// connectorCallbackJSON answers API clients (the app polls
// GET /api/connectors instead of reading error bodies).
func connectorCallbackJSON(w http.ResponseWriter, code int, detail string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, code, bson.M{"detail": detail})
}

// connectorCallbackError answers a callback failure in both
// representations: JSON for API clients, the close-this-tab page for the
// Google-redirected browser tab. Same status, same message — only the
// envelope differs, so no branch can drift into a guidance gap.
func connectorCallbackError(w http.ResponseWriter, r *http.Request, code int, detail string) {
	w.Header().Set("Cache-Control", "no-store")
	if connectorWantsHTML(r) {
		connectorCallbackPage(w, code, "Agento connect",
			detail+" Close this tab and return to the Agento app.")
		return
	}
	connectorCallbackJSON(w, code, detail)
}

// connectorCallbackAborted logs a dead client and reports whether the
// caller should return without writing a body: answering a domain error
// on a dead connection buys only a second log line for a hangup. A fired
// deadline is NOT gone — callers answer it with the timeout wording
// instead, so check DeadlineExceeded first.
func connectorCallbackAborted(ctx context.Context, verb string) bool {
	if ctx.Err() == nil || ctx.Err() == context.DeadlineExceeded {
		return false
	}
	log.Printf("connectors: %s aborted, request ended (client gone or server stopping): %v", verb, ctx.Err())
	return true
}

// connectorMissingScopes reports requested scopes absent from a
// space-delimited granted set (the ?scope= Google appends). Set compare:
// order and repeats do not matter, only coverage. Empty granted means
// Google sent none — treated as "nothing to compare", not a total miss.
func connectorMissingScopes(want []string, granted string) []string {
	if len(want) == 0 {
		return nil
	}
	have := map[string]bool{}
	for _, s := range strings.Fields(granted) {
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

// connectorCallbackDead answers a dead OAuth attempt (unknown, consumed,
// or expired state) with the same 400 meaning in both envelopes.
func connectorCallbackDead(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if connectorWantsHTML(r) {
		connectorCallbackPage(w, http.StatusBadRequest, "Agento connect expired",
			"This connect attempt expired or was already used. Close this tab and tap connect again in the Agento app.")
		return
	}
	connectorCallbackJSON(w, http.StatusBadRequest, connectorDeadAttempt)
}

// connectorWantsHTML reports text/html negotiation without lowercasing an
// unbounded header: anything past 512 bytes answers JSON (legit browser
// values are ~100 chars).
func connectorWantsHTML(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	if len(accept) > 512 {
		return false
	}
	lower := strings.ToLower(accept)
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

// connectorCallback finishes one OAuth attempt: the browser lands here
// from Google with ?code=&state=. No bearer check — the state is the
// capability. The state is consumed single-use, the code is exchanged
// server-side, the tokens are sealed with the envelope key, and the
// connector flips to connected. Tokens land BEFORE the status flips, so a
// connected-with-no-tokens row is never servable.
func connectorCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		connectorCallbackError(w, r, http.StatusMethodNotAllowed, "/api/connectors/callback accepts GET")
		return
	}
	// Bound the raw query before anything else, including the unconfigured
	// 501 below: the cap carries no config information, so a huge query
	// answers the same 400 configured or not.
	if len(r.URL.RawQuery) > 3*(3*connectors.MaxStateFieldLen)+256 {
		connectorCallbackDead(w, r)
		return
	}
	store := connectorStore()
	oauth := connectorOAuth(store)
	if oauth == nil {
		connectorCallbackError(w, r, http.StatusNotImplemented,
			"callback is not configured on this server yet — never paste a token in chat")
		return
	}
	query := r.URL.Query()
	code := connectors.TrimCredential(query.Get("code"))
	state := connectors.TrimCredential(query.Get("state"))
	// Duplicates first-win by design (single-use state burns either way),
	// but a junk-suffixed duplicate leaves no trace of the second value —
	// log lengths only (never values).
	if len(query["code"]) > 1 || len(query["state"]) > 1 {
		log.Printf("connectors: callback with duplicate params (code=%d, state=%d values), using first",
			len(query["code"]), len(query["state"]))
	}
	if code == "" || state == "" {
		connectorCallbackError(w, r, http.StatusBadRequest,
			"callback needs ?code= and ?state=; start over from connect")
		return
	}
	if len(code) > connectors.MaxStateFieldLen || len(state) > connectors.MaxStateFieldLen ||
		len(state) < connectors.MinStateLen ||
		connectors.HasControl(code) || connectors.HasControl(state) ||
		connectors.HasWhitespace(code) || connectors.HasWhitespace(state) ||
		!utf8.ValidString(code) || !utf8.ValidString(state) {
		connectorCallbackDead(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), connectorCallbackBudget)
	defer cancel()
	st, err := store.ConsumeState(ctx, state)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			connectorCallbackError(w, r, http.StatusBadGateway, "the connector store did not answer in time")
			return
		}
		if connectorCallbackAborted(ctx, "callback consume") {
			return
		}
		var se *connectors.StoreError
		if errors.As(err, &se) && se.Upstream {
			connectorCallbackError(w, r, http.StatusBadGateway, "the connector store did not answer in time")
			return
		}
		connectorCallbackDead(w, r)
		return
	}
	id := connectors.NormalizedID(st.Connector)
	// The verifier lives inside the consumed row, so it can only be
	// checked after the read — burning a hand-inserted bad row is
	// intended; minted rows always pass.
	if connectors.HasControl(st.Verifier) || !utf8.ValidString(st.Verifier) ||
		connectors.HasWhitespace(st.Verifier) ||
		len(st.Verifier) > connectors.MaxVerifierLen || len(st.Verifier) < connectors.MinVerifierLen {
		connectorCallbackDead(w, r)
		return
	}
	// Send the pinned redirect from mint time, not the live config, so a
	// rotation mid-consent cannot exact-mismatch Google. Pre-column rows
	// (no stored redirect) fall back to the live config.
	exCfg := oauth
	if st.RedirectURI != "" {
		redirect, rerr := connectors.CanonicalRedirectURL(st.RedirectURI)
		if rerr != nil {
			connectorCallbackDead(w, r)
			return
		}
		pinned := *oauth
		pinned.RedirectURL = redirect
		exCfg = &pinned
		// Log the divergence when pinned != live: an operator rotation
		// gets confirmation which string was sent, and a hand-inserted
		// row carrying a foreign redirect is visible here instead of
		// silently sent. Redacted (non-secret) like every logged URL.
		if st.RedirectURI != oauth.RedirectURL {
			log.Printf("connectors: callback uses pinned redirect %q (live config differs; rotation or hand-inserted row)",
				connectors.RedactURL(st.RedirectURI))
		}
	} else {
		// Pre-column rows carry no pinned redirect: the live config is
		// the only candidate. Auditable — after a rotation this sends the
		// NEW string for a row minted under the OLD one, so a mismatch
		// here reads as rotation fallout, not a helper bug.
		log.Printf("connectors: callback uses live redirect (no pinned value; pre-column row — rotation suspected if recently rotated)")
	}
	// Granted-scope subset warns, not fails: granular consent can return
	// fewer scopes than requested, and the grant still stores. A later
	// draft 403 traces back to this line. Unusable values (oversized,
	// control-bearing, undecodable) skip the compare, never the grant —
	// lengths only in the log, never values.
	var scopes []string
	for _, v := range query["scope"] {
		if t := connectors.TrimCredential(v); t != "" {
			scopes = append(scopes, t)
		}
	}
	if granted := strings.Join(scopes, " "); granted != "" &&
		len(granted) <= connectors.MaxStateFieldLen &&
		!connectors.HasControl(granted) && utf8.ValidString(granted) {
		if missing := connectorMissingScopes(connectors.GoogleScopes(id), granted); len(missing) > 0 {
			log.Printf("connectors: grant for %q missing requested scopes %q (subset granted, drafts may 403)",
				id, connectorBounded(strings.Join(missing, " ")))
		}
	} else if len(granted) > 0 {
		log.Printf("connectors: grant for %q scope param unusable (%d bytes trimmed), subset check skipped",
			id, len(granted))
	}
	tok, err := connectors.Exchange(ctx, exCfg, id, code, st.Verifier, log.Printf)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			connectorCallbackError(w, r, http.StatusBadGateway, "the grant exchange did not answer in time")
			return
		}
		if connectorCallbackAborted(ctx, "callback exchange") {
			return
		}
		var se *connectors.StoreError
		if errors.As(err, &se) && se.NotFound {
			connectorCallbackDead(w, r)
			return
		}
		connectorCallbackError(w, r, http.StatusBadGateway, "the grant exchange failed; start over from connect")
		return
	}
	// Trimmed copies are what get sealed: a padded value would seal with
	// padding the refresher cannot use.
	refreshToken := connectors.TrimCredential(tok.RefreshToken)
	accessToken := connectors.TrimCredential(tok.AccessToken)
	refreshSealed, err := connectors.EncryptToken(oauth.Key, refreshToken)
	if err != nil {
		log.Printf("connectors: token seal failed for %q: %q", id, connectorBounded(approvals.ScrubError(err.Error())))
		connectorCallbackError(w, r, http.StatusBadGateway, "could not store the grant")
		return
	}
	var accessSealed string
	if accessToken != "" {
		accessSealed, err = connectors.EncryptToken(oauth.Key, accessToken)
		if err != nil {
			log.Printf("connectors: token seal failed for %q: %q", id, connectorBounded(approvals.ScrubError(err.Error())))
			connectorCallbackError(w, r, http.StatusBadGateway, "could not store the grant")
			return
		}
	}
	var expiry int64
	if accessToken != "" && !tok.Expiry.IsZero() {
		expiry = connectors.ClampAccessExpiry(tok.Expiry.UnixMilli(), time.Now())
	}
	if err := store.SetTokens(ctx, id, connectors.TokenRow{
		RefreshCipher:         refreshSealed,
		AccessCipher:          accessSealed,
		AccessExpiryUnixMilli: expiry,
	}); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			connectorCallbackError(w, r, http.StatusBadGateway, "the connector store did not answer in time")
			return
		}
		if connectorCallbackAborted(ctx, "callback tokens") {
			return
		}
		connectorCallbackError(w, r, http.StatusBadGateway, "could not store the grant")
		return
	}
	c, err := store.SetStatus(ctx, id, connectors.StatusConnected, "")
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			connectorCallbackError(w, r, http.StatusBadGateway, "the connector store did not answer in time")
			return
		}
		if connectorCallbackAborted(ctx, "callback status") {
			return
		}
		connectorCallbackError(w, r, http.StatusBadGateway, "could not store the grant")
		return
	}
	// A browser tab gets the close-this-tab page; API clients get JSON.
	// The app polls GET /api/connectors instead of reading this body.
	if connectorWantsHTML(r) {
		connectorCallbackPage(w, http.StatusOK, "Agento connected",
			"Connected. You can close this tab and return to the Agento app.")
		return
	}
	writeJSON(w, http.StatusOK, bson.M{"ok": true, "connector": connectorWire(c)})
}

// registerConnectors wires this domain's routes. One function per file so
// main stays a list of domains, not a list of paths.
func registerConnectors(mux *http.ServeMux) {
	mux.HandleFunc("/api/connectors", listConnectors)
	mux.HandleFunc("/api/connectors/", connectorsSlash)
	// Exact callback spelling: the registered Google redirect URI has no
	// trailing slash, and without this it would 404 (the subtree above
	// only catches paths WITH the slash).
	mux.HandleFunc("/api/connectors/callback", connectorCallback)
}

// connectorsSlash dispatches under /api/connectors/: the callback (no
// bearer — the state is the capability), the collection with a trailing
// slash, and per-connector verbs. Anything deeper 404s.
func connectorsSlash(w http.ResponseWriter, r *http.Request) {
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api/connectors/"), "/")
	if rest == "" {
		listConnectors(w, r)
		return
	}
	if strings.EqualFold(rest, "callback") {
		connectorCallback(w, r)
		return
	}
	connectorItem(w, r)
}
