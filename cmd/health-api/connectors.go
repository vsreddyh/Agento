package main

// Connector routes: the Gmail/Calendar OAuth surface over the shared
// connectors collection (#488).
//
// The app used to reach these verbs on pi-gateway (:8643); they live here
// now so connect/list/disconnect keep working with the pi container
// stopped. Same status codes and success shapes, same Mongo rows
// (internal/connectors owns the store and the OAuth helpers — this file
// is only the HTTP envelope); error envelopes follow this service
// (`detail`, not the gateway's OpenAI-style error object).
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
	"os"
	"strings"
	syncpkg "sync"
	"time"
	"unicode/utf8"

	"agento/internal/approvals"
	"agento/internal/connectors"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/readpref"
)

// connectorVerbBudget bounds the fast verbs (list/disconnect/connect
// validation). The callback gets its own longer budget below: it binds a
// state consume plus a live Google exchange.
const connectorVerbBudget = 5 * time.Second

// Per-step budgets for the callback pipeline (consume + exchange + writes,
// all sequential against live Google). Each step derives fresh from
// r.Context(), deliberately NOT from one shared parent: a slow Google
// exchange must not starve the store writes into false "store did not
// answer" 502s, and a step's DeadlineExceeded is then attributable to
// THAT step — a store-write timeout genuinely means the store is slow
// (worth a demotion note), never that Google ate a shared budget first.
const (
	connectorConsumeBudget    = 10 * time.Second
	connectorExchangeBudget   = 20 * time.Second
	connectorGrantStoreBudget = 10 * time.Second
)

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
// recovery is picked up automatically within the retry budget below —
// no restart needed for serving or TTL.
var (
	connectorStoreMu syncpkg.Mutex
	connectorCache   connectors.Store
	// connectorCacheAt stamps the last Mongo attempt behind a cached
	// memory fallback. Zero when the cache is durable.
	connectorCacheAt time.Time
	// connectorDialing + connectorDialCh single-flight the slow path
	// (cold-start dial and blip-recovery re-dial): exactly one caller
	// dials while the rest wait on the channel, then all share the
	// swap. Guarded by connectorStoreMu.
	connectorDialing bool
	connectorDialCh  chan struct{}
	// connectorLastErr is the last Mongo open failure, scrubbed at write
	// time. It lets the 501-reason path name "no database" without
	// dialling a fresh client per tap.
	connectorLastErr string
	// connectorUpstreamFails counts store-side failures inside one retry
	// window for demotion (see noteConnectorUpstream).
	// connectorLastProbe stamps the last dial, so sustained failures
	// cannot suppress the probe they should trigger. Guarded by connectorStoreMu.
	connectorUpstreamFails int
	connectorLastFail      time.Time
	connectorLastProbe     time.Time
)

// connectorRetryBudget bounds how often a cached memory fallback
// re-attempts Mongo: one recovery dial per minute max during an outage
// (single-flight — concurrent verbs wait for it instead of dialling).
// Never per request.
const connectorRetryBudget = time.Minute

// connectorPingBudget bounds the reachability probe: the driver dials
// lazily, so FromEnv succeeds with an unreachable URI and only a live
// round-trip demotes. Same budget discipline as the retry path.
const connectorPingBudget = 5 * time.Second

// connectorPing reports whether ms answers. A parseable-but-unreachable
// URI must fall back to memory (501s), not serve 502s on a
// durable-looking cache — the documented contract.
func connectorPing(ms *connectors.MongoStore) bool {
	ctx, cancel := context.WithTimeout(context.Background(), connectorPingBudget)
	defer cancel()
	return ms.DB().Client().Ping(ctx, readpref.Primary()) == nil
}

func connectorStore(ctx context.Context) connectors.Store {
	// Fast path under lock: cache hit (durable with no recent upstream
	// failures, or non-durable still inside its retry budget) returns
	// without any dial.
	connectorStoreMu.Lock()
	if connectorCache != nil && connectorStoreFreshLocked() {
		defer connectorStoreMu.Unlock()
		return connectorCache
	}
	if connectorDialing {
		// A dial is already in flight (cold-start burst or blip
		// recovery): wait for it instead of opening a second one
		// (single-flight). Cancellable: a hung-up caller stops waiting
		// instead of holding its goroutine for the full dial, then
		// serving a response nobody reads.
		ch := connectorDialCh
		connectorStoreMu.Unlock()
		select {
		case <-ch:
			connectorStoreMu.Lock()
			defer connectorStoreMu.Unlock()
			return connectorCache
		case <-ctx.Done():
			connectorStoreMu.Lock()
			stale := connectorCache
			connectorStoreMu.Unlock()
			if stale != nil {
				return stale
			}
			// Cold start with nobody cached yet: a transient memory
			// store, deliberately UNCACHED so the in-flight dial's swap
			// still lands for everyone else. This response is discarded
			// anyway (the client is gone).
			return connectors.NewMemoryStore()
		}
	}
	// Slow path: first boot, a non-durable cache past its retry budget,
	// or a durable cache with enough consecutive upstream failures to
	// suspect a post-boot drop. This caller owns the dial; concurrent
	// verbs wait above. The dial happens OUTSIDE the lock, so serving
	// (and the waiters' wakeup) never queues behind it — the swap is the
	// only critical section.
	// A Mongo recovery must not wait for a restart to take effect: a
	// restart during an Atlas blip otherwise pins memory until a second
	// restart, with 501s the whole while. Symmetrically, a post-boot drop
	// demotes instead of 502ing forever on a stale "configured" line.
	connectorDialing = true
	connectorDialCh = make(chan struct{})
	connectorStoreMu.Unlock()
	ms, err := connectors.FromEnv()
	// The driver dials lazily: FromEnv succeeds with an unreachable URI,
	// so only a live round-trip promotes. Without this a typo'd-but-
	// parseable URI caches "durable" and serves 502s where the docs
	// promise memory + 501s.
	pingOK := err == nil && ms != nil && connectorPing(ms)
	connectorStoreMu.Lock()
	defer connectorStoreMu.Unlock()
	connectorDialing = false
	close(connectorDialCh)
	// If/else, not a typed-nil assignment: a failed FromEnv yields a nil
	// *MongoStore, which as an interface value is non-nil and would cache
	// a panic. The else branch only ever holds a live store.
	var backend connectors.Store
	if err != nil || !pingOK {
		if err != nil {
			log.Printf("connectors: falling back to in-process (statuses do not survive restart): %s",
				connectorBounded(approvals.ScrubError(err.Error())))
			connectorLastErr = connectorBounded(approvals.ScrubError(err.Error()))
		} else {
			log.Print("connectors: MongoDB unreachable (dial ok, ping failed) — falling back to in-process")
			connectorLastErr = "unreachable (ping failed)"
			// The failed client holds pool goroutines and a monitor:
			// disconnect it or every probe leaks one for the outage.
			func() {
				dctx, dcancel := context.WithTimeout(context.Background(), connectorPingBudget)
				defer dcancel()
				_ = ms.DB().Client().Disconnect(dctx)
			}()
		}
		backend, connectorCacheAt = connectors.NewMemoryStore(), time.Now()
	} else {
		if connectorCache != nil && !connectorCache.Durable() {
			log.Print("connectors: MongoDB recovered, serving durable store")
		}
		backend, connectorCacheAt = ms, time.Now()
		connectorLastErr = ""
		connectorUpstreamFails = 0
		// The recovered store never got its TTL: index it now,
		// best-effort on a detached budget, or pending states
		// accumulate until the next restart. (Boot-time indexing lives
		// in initConnectors; this covers the blip-recovery path only.
		// ms is *MongoStore by construction — no assert needed.)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := ms.EnsureIndexes(ctx); err != nil {
				log.Printf("connectors: WARNING recovered-store index setup failed: %s",
					connectorBounded(approvals.ScrubError(err.Error())))
			}
		}()
	}
	connectorCache = backend
	// Every slow-path dial stamps the probe time whatever the outcome:
	// the durable freshness throttle keys on it, so a failed probe
	// (demote) still buys a full budget before the next one.
	connectorLastProbe = time.Now()
	return backend
}

// connectorStoreFreshLocked reports whether the cached store needs no
// probe: durable caches stay fresh until consecutive upstream failures
// suggest a post-boot drop (then the next caller re-probes, at most once
// per budget); non-durable caches stay fresh inside the retry budget.
// Throttle keys on the last PROBE, not the last failure — sustained 502s
// refresh LastFail every tap, and keying on it would livelock: fresh
// forever while traffic continues, demoting only when nobody needs it.
// Caller holds the lock.
func connectorStoreFreshLocked() bool {
	if connectorCache == nil {
		return false
	}
	if !connectorCache.Durable() {
		return time.Since(connectorCacheAt) <= connectorRetryBudget
	}
	return connectorUpstreamFails < connectorDemoteAfter ||
		time.Since(connectorLastProbe) <= connectorRetryBudget
}

// connectorDemoteAfter bounds how many upstream failures inside one
// retry window a durable cache absorbs before the next caller re-probes
// (and demotes on a failed probe): enough to ride out one blip, few
// enough that a real drop flips to memory + 501s instead of 502ing
// forever.
const connectorDemoteAfter = 3

// noteConnectorUpstream records one store-side failure (502-boundary:
// deadline or upstream, never client-gone). Failures count inside one
// retry window only: a failure arriving more than a budget after the
// previous one restarts the count at 1, so two isolated blips weeks
// apart (with thousands of successes between) never trip the demotion
// probe — only sustained failure does. Call it wherever a verb answers
// for the store so a post-boot drop demotes instead of pinning a stale
// "configured" cache.
func noteConnectorUpstream() {
	connectorStoreMu.Lock()
	defer connectorStoreMu.Unlock()
	if time.Since(connectorLastFail) > connectorRetryBudget {
		connectorUpstreamFails = 1
	} else {
		connectorUpstreamFails++
	}
	connectorLastFail = time.Now()
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

// connectorOAuthDown explains why OAuth is unavailable: the triage
// pointer the live 501s reference, mirroring logConnectorOAuth's boot
// branches. Reads cached boot state only — never dials, so a 501 storm
// during an Atlas blip cannot churn the cluster. Call with the store
// freshly in hand (it is already locked or local at every call site).
func connectorOAuthDown(store connectors.Store) string {
	connectorStoreMu.Lock()
	lastErr := connectorLastErr
	connectorStoreMu.Unlock()
	if !store.Durable() {
		if lastErr != "" {
			return "no database (" + lastErr + ")"
		}
		return "no durable store"
	}
	// Redirect typos need unpicking: OAuthConfigFromEnv already runs
	// CanonicalRedirectURL, so its error surfaces as "missing values"
	// below — validate the raw value directly so a typo reads as one.
	if raw := strings.TrimSpace(os.Getenv("GOOGLE_REDIRECT_URL")); raw != "" {
		if _, cerr := connectors.CanonicalRedirectURL(raw); cerr != nil {
			return "redirect invalid (" + connectorBounded(approvals.ScrubError(cerr.Error())) + ")"
		}
	}
	if _, err := connectors.OAuthConfigFromEnv(); err != nil {
		return "missing values (" + connectorBounded(approvals.ScrubError(err.Error())) + ")"
	}
	return "redirect invalid"
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

// initConnectors opens the store ONCE at boot and shares the snapshot
// between index setup and the OAuth boot log, so the two lines cannot
// disagree with each other. Serving joins the same cache once populated
// (or falls back lazily per verb until then — a blip exactly at boot can
// still serve a different snapshot than the boot lines, and the live
// connectorOAuthDown reason on a 501 stays authoritative over both).
func initConnectors() {
	store := connectorStore(context.Background())
	if ms, ok := store.(*connectors.MongoStore); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := ms.EnsureIndexes(ctx); err != nil {
			log.Printf("connectors: WARNING index setup failed, oauth states will not reap: %s",
				connectorBounded(approvals.ScrubError(err.Error())))
		}
	} else {
		// Loud, not silent: without Mongo there is no states TTL, and an
		// operator grepping boot logs must see why.
		log.Print("connectors: no database, skipping index setup (oauth states will not reap)")
	}
	if !store.Durable() {
		log.Print("connectors: OAuth unavailable (no durable store); connect answers 501")
		return
	}
	// Redirect typos need unpicking here too: the loader below already
	// canonicalises, so without this a typo would log as "missing values"
	// instead of the `redirect invalid` line the runbook greps for.
	if raw := strings.TrimSpace(os.Getenv("GOOGLE_REDIRECT_URL")); raw != "" {
		if _, cerr := connectors.CanonicalRedirectURL(raw); cerr != nil {
			log.Printf("connectors: OAuth redirect invalid (want %q): %q",
				connectors.RedactURL(raw), connectorBounded(approvals.ScrubError(cerr.Error())))
			return
		}
	}
	cfg, err := connectors.OAuthConfigFromEnv()
	if err != nil {
		log.Printf("connectors: OAuth unavailable (%s); connect answers 501",
			connectorBounded(approvals.ScrubError(err.Error())))
		return
	}
	canonical, err := connectors.CanonicalRedirectURL(cfg.RedirectURL)
	if err != nil {
		log.Printf("connectors: OAuth redirect invalid (want %q): %q",
			connectors.RedactURL(cfg.RedirectURL), connectorBounded(approvals.ScrubError(err.Error())))
		return
	}
	// Storable values log whole: compare this line byte-for-byte against
	// the Cloud console registration, never the input spelling.
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
// failures 502, anything else 400. Domain messages are echoed scrubbed +
// bounded (triage greps them); the envelope stays health-api `detail`
// (not the gateway's OpenAI-style error object) like every other route
// in this service — status codes and success shapes match the gateway,
// error envelopes follow this service.
func writeConnectorErr(w http.ResponseWriter, err error) {
	var se *connectors.StoreError
	if errors.As(err, &se) {
		switch {
		case se.NotFound:
			// Echo the store message (always "unknown connector <id>")
			// scrubbed + bounded, like the gateway: a store-level
			// NotFound (disconnect race, direct future verbs) must grep
			// the same as a dispatch-level one.
			writeJSON(w, http.StatusNotFound, bson.M{"detail": connectorBounded(approvals.ScrubError(se.Msg))})
			return
		case se.Upstream:
			noteConnectorUpstream()
			writeJSON(w, http.StatusBadGateway, bson.M{"detail": connectorBounded(approvals.ScrubError(se.Msg))})
			return
		default:
			writeJSON(w, http.StatusBadRequest, bson.M{"detail": connectorBounded(approvals.ScrubError(se.Msg))})
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
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "/api/connectors accepts GET"})
		return
	}
	store := connectorStore(r.Context())
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
			writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "/api/connectors/{id}/disconnect accepts POST"})
			return
		}
		store := connectorStore(r.Context())
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
			writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "/api/connectors/{id}/connect accepts POST"})
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
// Takes the already-resolved store: re-resolving could land on a
// different backend (blip recovery between the two calls) and leak the
// row the caller just minted. Only real IO failures log: NotFound means
// no row was ever minted, which is the desired end state — not a
// failure. A failed build must mint nothing observable.
func cleanupConnectorOrphanState(store connectors.Store, id, state string) {
	cleanCtx, cleanCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cleanCancel()
	if _, cerr := store.ConsumeState(cleanCtx, state); cerr != nil {
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
	store := connectorStore(r.Context())
	oauth := connectorOAuth(store)
	if oauth == nil {
		log.Printf("connectors: connect for %q attempted while OAuth is unconfigured (%s)", id, connectorOAuthDown(store))
		writeJSON(w, http.StatusNotImplemented,
			bson.M{"detail": "connect is not configured on this server yet — never paste a token in chat"})
		return
	}
	pinnedRedirect, err := connectors.CanonicalRedirectURL(oauth.RedirectURL)
	if err != nil {
		// Echo the attempted redirect (non-secret) with the connector id:
		// triage compares it against the console registration without
		// re-reading .env. Scrubbed + bounded like every logged value.
		log.Printf("connectors: connect redirect invalid for %q (want %q): %q", id,
			connectors.RedactURL(oauth.RedirectURL), connectorBounded(approvals.ScrubError(err.Error())))
		writeJSON(w, http.StatusBadGateway, bson.M{"detail": "connect is not configured correctly on this server"})
		return
	}
	state, verifier, err := connectors.NewState()
	if err != nil {
		log.Printf("connectors: could not mint connect state for %q", id)
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
		log.Printf("connectors: connect state store failed for %q: %s", id,
			connectorBounded(approvals.ScrubError(err.Error())))
		writeConnectorErr(w, err)
		return
	}
	authURL := connectors.AuthURL(oauth, id, state, verifier, log.Printf)
	if authURL == "" {
		// A failed build mints nothing observable: consume the just-minted
		// row best-effort so no live attempt lingers that no client holds.
		cleanupConnectorOrphanState(store, id, state)
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
// instead, so check DeadlineExceeded first. Scrubbed + bounded like every
// other interpolated value on this surface (ctx.Err is safe today, but
// the next edit must not inherit a raw-%v pattern here).
func connectorCallbackAborted(ctx context.Context, verb string) bool {
	if ctx.Err() == nil || ctx.Err() == context.DeadlineExceeded {
		return false
	}
	log.Printf("connectors: %s aborted, request ended (client gone or server stopping): %s",
		verb, connectorBounded(approvals.ScrubError(ctx.Err().Error())))
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
		// Length-only log: a browser tab that got JSON instead of the
		// close-this-tab page reads as a regression with nothing to
		// trace — same pointer class as the duplicate-params skip.
		log.Printf("connectors: Accept header oversized (%d bytes), answering JSON", len(accept))
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
		// Length-only log (never the value): an oversized query and a
		// genuinely expired state answer the identical dead-attempt body,
		// and without this line triage cannot tell flood from expiry.
		log.Printf("connectors: callback query oversized (%d bytes raw), answering dead-attempt", len(r.URL.RawQuery))
		connectorCallbackDead(w, r)
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
	// Missing and malformed params answer before the unconfigured 501
	// below: both are stateless input verdicts that must not vary with
	// server config, or an unauthenticated probe distinguishes
	// configured-vs-not (and the app gets "re-tap" vs "fix server" mixed).
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
		// Lengths only, never values: an oversized/hand-minted code or
		// state answers the identical dead-attempt body as an expired
		// one, and without this line triage cannot tell flood from expiry.
		log.Printf("connectors: callback code/state failed shape gate (code=%d, state=%d bytes), answering dead-attempt",
			len(code), len(state))
		connectorCallbackDead(w, r)
		return
	}
	store := connectorStore(r.Context())
	oauth := connectorOAuth(store)
	if oauth == nil {
		log.Printf("connectors: callback attempted while OAuth is unconfigured (%s)", connectorOAuthDown(store))
		connectorCallbackError(w, r, http.StatusNotImplemented,
			"callback is not configured on this server yet — never paste a token in chat")
		return
	}
	consumeCtx, consumeCancel := context.WithTimeout(r.Context(), connectorConsumeBudget)
	defer consumeCancel()
	st, err := store.ConsumeState(consumeCtx, state)
	if err != nil {
		if consumeCtx.Err() == context.DeadlineExceeded {
			noteConnectorUpstream()
			connectorCallbackError(w, r, http.StatusBadGateway, "the connector store did not answer in time")
			return
		}
		if connectorCallbackAborted(consumeCtx, "callback consume") {
			return
		}
		var se *connectors.StoreError
		if errors.As(err, &se) && se.Upstream {
			noteConnectorUpstream()
			log.Printf("connectors: callback consume failed: %s",
				connectorBounded(approvals.ScrubError(err.Error())))
			connectorCallbackError(w, r, http.StatusBadGateway, "the connector store did not answer in time")
			return
		}
		connectorCallbackDead(w, r)
		return
	}
	id := connectors.NormalizedID(st.Connector)
	// Unknown connector in a consumed row (hand-inserted or typo'd state)
	// dead-attempts like every other dead state — before Exchange, so a
	// bad row never pays a Google round-trip only to 502 at SetTokens.
	// The burn already happened (single-use by design); the 400 names the
	// recovery (re-tap connect), unlike a 502 store-failure message.
	if !connectors.Known(id) {
		log.Printf("connectors: callback consumed state for unknown connector %q (burned, answering dead-attempt)",
			connectorBounded(approvals.ScrubError(id)))
		connectorCallbackDead(w, r)
		return
	}
	// The verifier lives inside the consumed row, so it can only be
	// checked after the read — burning a hand-inserted bad row is
	// intended; minted rows always pass.
	if connectors.HasControl(st.Verifier) || !utf8.ValidString(st.Verifier) ||
		connectors.HasWhitespace(st.Verifier) ||
		len(st.Verifier) > connectors.MaxVerifierLen || len(st.Verifier) < connectors.MinVerifierLen {
		log.Printf("connectors: callback verifier failed shape gate for %q (len=%d, burned row), answering dead-attempt",
			connectorBounded(approvals.ScrubError(id)), len(st.Verifier))
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
			// Length-only, never the value: a corrupt/hand-inserted row
			// burns its single-use state here, and without this line its
			// dead-attempt body is indistinguishable from genuine expiry.
			// Id included like every neighbouring dead branch.
			log.Printf("connectors: callback pinned redirect misshapen for %q (%d bytes stored), burning row and answering dead-attempt",
				connectorBounded(approvals.ScrubError(id)), len(st.RedirectURI))
			connectorCallbackDead(w, r)
			return
		}
		pinned := *oauth
		pinned.RedirectURL = redirect
		exCfg = &pinned
		// Compare canonical-vs-canonical: the stored row is already
		// canonical, so diffing against the raw env string would log
		// rotation on spelling-only differences (case, :443, dot) whose
		// sent bytes are identical.
		live := oauth.RedirectURL
		if canonical, cerr := connectors.CanonicalRedirectURL(oauth.RedirectURL); cerr == nil {
			live = canonical
		}
		// Log the divergence when pinned != live: an operator rotation
		// gets confirmation which string was sent, and a hand-inserted
		// row carrying a foreign redirect is visible here instead of
		// silently sent. Redacted (non-secret) like every logged URL.
		if st.RedirectURI != live {
			log.Printf("connectors: callback for %q uses pinned redirect %q (live config differs; rotation or hand-inserted row)",
				id, connectors.RedactURL(st.RedirectURI))
		}
	} else {
		// Pre-column rows carry no pinned redirect: the live config is
		// the only candidate. Auditable — after a rotation this sends the
		// NEW string for a row minted under the OLD one, so a mismatch
		// here reads as rotation fallout, not a helper bug.
		log.Printf("connectors: callback for %q uses live redirect %q (no pinned value; pre-column row — rotation suspected if recently rotated)",
			id, connectors.RedactURL(oauth.RedirectURL))
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
			log.Printf("connectors: grant for %q missing requested scopes %q (granted %q; subset granted, drafts may 403)",
				id, connectorBounded(strings.Join(missing, " ")), connectorBounded(approvals.ScrubError(granted)))
		}
	} else if len(granted) > 0 {
		log.Printf("connectors: grant for %q scope param unusable (%d bytes trimmed), subset check skipped",
			id, len(granted))
	}
	exchangeCtx, exchangeCancel := context.WithTimeout(r.Context(), connectorExchangeBudget)
	defer exchangeCancel()
	tok, err := connectors.Exchange(exchangeCtx, exCfg, id, code, st.Verifier, log.Printf)
	if err != nil {
		if exchangeCtx.Err() == context.DeadlineExceeded {
			connectorCallbackError(w, r, http.StatusBadGateway, "the grant exchange did not answer in time")
			return
		}
		if connectorCallbackAborted(exchangeCtx, "callback exchange") {
			return
		}
		var se *connectors.StoreError
		if errors.As(err, &se) && se.NotFound {
			connectorCallbackDead(w, r)
			return
		}
		if errors.As(err, &se) && !se.Upstream {
			// Deterministic provider answer (no refresh token, invalid
			// or oversized token): misconfigured consent, not an outage
			// — 400 with the scrubbed reason so re-tap (not retry) is
			// the signal. Upstream failures stay 502 below.
			connectorCallbackError(w, r, http.StatusBadRequest,
				connectorBounded(approvals.ScrubError(se.Msg)))
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
	// Fresh store budget, not the exchange's leftovers: Google slowness
	// must not starve these writes into false store timeouts (and false
	// demotion notes against a healthy Mongo).
	storeCtx, storeCancel := context.WithTimeout(r.Context(), connectorGrantStoreBudget)
	defer storeCancel()
	if err := store.SetTokens(storeCtx, id, connectors.TokenRow{
		RefreshCipher:         refreshSealed,
		AccessCipher:          accessSealed,
		AccessExpiryUnixMilli: expiry,
	}); err != nil {
		if storeCtx.Err() == context.DeadlineExceeded {
			noteConnectorUpstream()
			connectorCallbackError(w, r, http.StatusBadGateway, "the connector store did not answer in time")
			return
		}
		if connectorCallbackAborted(storeCtx, "callback tokens") {
			return
		}
		noteConnectorUpstream()
		log.Printf("connectors: callback token store failed for %q: %s", id,
			connectorBounded(approvals.ScrubError(err.Error())))
		connectorCallbackError(w, r, http.StatusBadGateway, "could not store the grant")
		return
	}
	c, err := store.SetStatus(storeCtx, id, connectors.StatusConnected, "")
	if err != nil {
		if storeCtx.Err() == context.DeadlineExceeded {
			noteConnectorUpstream()
			connectorCallbackError(w, r, http.StatusBadGateway, "the connector store did not answer in time")
			return
		}
		if connectorCallbackAborted(storeCtx, "callback status") {
			return
		}
		noteConnectorUpstream()
		log.Printf("connectors: callback status store failed for %q: %s", id,
			connectorBounded(approvals.ScrubError(err.Error())))
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

// connectorCallbackPath reports whether p reaches the OAuth callback
// after the same trim/fold the dispatcher applies (case-insensitive last
// segment, slash-collapsed). One predicate for both the dispatcher and
// the rate limiter, so a spelling serving in one is enveloped in the
// other.
func connectorCallbackPath(p string) bool {
	if !strings.HasPrefix(p, "/api/connectors/") {
		return false
	}
	rest := strings.Trim(strings.TrimPrefix(p, "/api/connectors/"), "/")
	return strings.EqualFold(rest, "callback")
}

// connectorsFallback answers /api/* paths no domain claimed — including
// the no-slash-boundary misses (/api/connectorsfoo, /api/CONNECTORS)
// that share the connectors prefix without the trailing-slash subtree.
// Without it Go's default mux answers text/plain 404 with no envelope,
// while the gateway answers the same spellings as JSON unknown-path with
// no-store. Auth first like every other verb here (the gateway 401s
// these before its unknown-path 404), then the service `detail` envelope
// with no-store so triage greps and the app's detail branch keep working.
// Registered as /api/ + /api so it only sees what longer patterns missed.
func connectorsFallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	writeJSON(w, http.StatusNotFound, bson.M{
		"detail": "unknown path " + connectorBounded(approvals.ScrubError(r.URL.Path))})
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
	// Near-miss fallback: /api/connectorsfoo, /api/CONNECTORS, and any
	// other /api/* spelling no longer pattern claimed. Longest-match wins,
	// so every registered route above (and every other domain's /api/*
	// routes, registered elsewhere) still takes precedence — this only
	// converts the default-mux text/plain 404 into the service envelope.
	mux.HandleFunc("/api/", connectorsFallback)
	mux.HandleFunc("/api", connectorsFallback)
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
	if connectorCallbackPath(r.URL.Path) {
		connectorCallback(w, r)
		return
	}
	connectorItem(w, r)
}
