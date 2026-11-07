// Command health-api is the Health Connect sync endpoint for the health-check bot.
//
// Accepts POSTs from the Agento Android app and persists to the shared
// remote MongoDB collection the health-check bot also reads (hc_days —
// one doc per date, same shape the MCP writes).
//
// Auth: bearer token(s) from PASSWORD (comma-separated = multi-token;
// matches the Password field in the Agento app Settings, same credential
// as gateway chat).
//
// Env: MONGODB_URI (required), MONGODB_DB (default: hermes), PASSWORD.
package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	syncpkg "sync"
	"time"

	"agento/internal/crashlog"
)

// passwordWarnOnce keeps the empty-PASSWORD line to one emission: without
// it the app's poll loop turns a config error into steady log spam.
var passwordWarnOnce syncpkg.Once

// IST is the user timezone (UTC+5:30, no DST) as a fixed offset.
var IST = time.FixedZone("IST", 5*3600+30*60)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// nosniff on every JSON body: error text is scrubbed + bounded, but a
	// sniffed content-type on an error page is still a MIME-confusion
	// vector for browser-parsed responses.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Challenge on every 401: generic clients key off it, and authorize
	// below is the only 401 producer in this binary.
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="agento"`)
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// tokens reads accepted tokens fresh (env may rotate without a restart
// in-process — under compose the root .env is baked at container creation,
// so a PASSWORD edit needs `./scripts/hermes.sh restart` to recreate;
// without it the app keeps 401ing on the old value).
func tokens() map[string]bool {
	out := map[string]bool{}
	for _, t := range strings.Split(os.Getenv("PASSWORD"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			out[t] = true
		}
	}
	return out
}

func authorize(r *http.Request) (int, string) {
	toks := tokens()
	if len(toks) == 0 {
		// Once, not per request: an empty PASSWORD plus the app's poll
		// loop would otherwise fill the log with one identical line per
		// allowed poll, burying the single boot-time signal. A green
		// /health does NOT contradict this: liveness is exempt from auth
		// and throttling by design, and the OAuth callback bypasses auth
		// (the state is the capability) — both still serve. Probe an
		// auth-bearing route (not /health) for serving readiness.
		passwordWarnOnce.Do(func() {
			log.Print("PASSWORD not set — rejecting auth-bearing requests (/health and the OAuth callback still serve)")
		})
		return http.StatusServiceUnavailable, "server not configured with tokens"
	}
	// Deliberate divergences from the gateway's authenticate, both
	// health-api-wins and both predating the connector move: an empty
	// PASSWORD fails closed here (503) instead of opening up, and
	// comma-separated rotation tokens all work here instead of one.
	// Both retire with the gateway in #494 — until then the two surfaces
	// answer differently on empty/multi-token setups, and this side is
	// the safe one.
	h := r.Header.Get("Authorization")
	const bearerPrefix = "Bearer "
	// Scheme case-insensitive per RFC 7235 (and like the gateway): a
	// lowercase `bearer` authorizes the same token instead of 401ing a
	// working credential on hand-tests and generic clients.
	if len(h) <= len(bearerPrefix) || !strings.EqualFold(h[:len(bearerPrefix)], bearerPrefix) {
		return http.StatusUnauthorized, "missing bearer token"
	}
	got := strings.TrimSpace(h[len(bearerPrefix):])
	// Constant-time bearer comparison (#416): map lookup short-circuits
	// on the first differing byte (timing oracle), so compare against
	// every accepted token with subtle.ConstantTimeCompare and accumulate
	// with OR — no early return. Unequal-length candidates are skipped, so
	// token length is not fully hidden (0 vs 1 compares); accepted as
	// low-risk per #416 (shared secret(s) over a noisy network, and the
	// multi-token case only widens the length signal, not the content
	// oracle this fix removes). An empty PASSWORD still fails closed
	// above (503) before reaching here.
	match := 0
	for t := range toks {
		if len(got) == len(t) {
			match |= subtle.ConstantTimeCompare([]byte(got), []byte(t))
		}
	}
	if match != 1 {
		return http.StatusUnauthorized, "invalid token"
	}
	return 0, ""
}

// recoverCrash turns a handler panic into a 500 plus a stack trace with
// the same detail as the file logs. health-api has no log bind mount, so
// crashes go to stdout — podman logs — instead of a file (#290). Only the
// method and path are recorded, never headers or the query; the stack is
// redacted the same way before printing.
func recoverCrash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				// Decoded Path, not EscapedPath: dispatch, rate limiting,
				// and every serving log key on it, so triage greps match.
				// (An encoded spelling lands here exactly as the router
				// saw it.)
				route := r.Method + " " + r.URL.Path
				secrets := []string{}
				for _, t := range strings.Split(os.Getenv("PASSWORD"), ",") {
					if t = strings.TrimSpace(t); t != "" {
						secrets = append(secrets, t)
					}
				}
				secrets = append(secrets, os.Getenv("MONGODB_URI"))
				// OAuth values live in this binary too (connect/exchange/
				// seal paths): a panic carrying one must not land verbatim
				// in `logs health-api`. Redaction is pattern-based — see
				// crashlog.Redact — so append raw and let it scrub.
				for _, k := range []string{"GOOGLE_CLIENT_ID", "GOOGLE_CLIENT_SECRET", "GOOGLE_REDIRECT_URL", "CONNECTORS_ENCRYPTION_KEY"} {
					if v := strings.TrimSpace(os.Getenv(k)); v != "" {
						secrets = append(secrets, v)
					}
				}
				// Both halves redacted: the stack goes through Redact, and
				// so must the panic value — a config echo or exchange error
				// in `rec` would otherwise bypass the list just built.
				fmt.Fprintf(os.Stdout, "crash route=%s panic=%v\nstack:\n%s\n",
					route, crashlog.Redact(fmt.Sprintf("%v", rec), secrets...), crashlog.Redact(string(debug.Stack()), secrets...))
				// `detail` like every other error in this binary (the app
				// reads `detail`, never `error`, on this service).
				writeJSON(w, http.StatusInternalServerError,
					map[string]string{"detail": "internal server error"})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func main() {
	mux := http.NewServeMux()
	registerHealth(mux)
	registerFiles(mux)
	registerTasks(mux)
	registerProjects(mux)
	registerReports(mux)
	registerConnectors(mux)
	// Rate limiting wraps everything, and lives here because main is the only place
	// that knows the whole handler set (#176). The API is network-reachable and
	// authed by shared password(s), so without a counter a stuck client retrying in
	// a loop — or anyone holding that password — is an unbounded write load on a
	// shared production MongoDB.
	//
	// Wrapping the mux rather than each handler is deliberate: a per-handler limit is
	// a limit someone forgets to add to the next route.
	h := rateLimit(newLimiter(defaultBurst, defaultRefill), mux)
	h = recoverCrash(h)
	// Timeouts, for the same "one choke point" reason. A slow client on a read must
	// not hold a connection open indefinitely, and without these a handler that
	// blocks keeps its goroutine forever.
	srv := &http.Server{
		Addr:              ":8000",
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		// Bounds the whole request READ, headers and body together, so a client that
		// dribbles a slow body cannot pin a handler goroutine indefinitely. Bodies here
		// are small JSON (MaxBytesReader caps them at 64KB), so this cannot cut off a
		// legitimate write.
		ReadTimeout: 30 * time.Second,
		IdleTimeout: 120 * time.Second,
		// NO WriteTimeout, deliberately. /exports serves files of unbounded size from
		// the host, and a global WriteTimeout would abort a large download partway
		// through — turning a slow client into a corrupt file for every caller behind
		// it. The per-handler read budget above covers the abuse case (a stalled
		// request body) without touching the response.
	}
	// Connector boot work is detached: initConnectors() runs in the
	// background so boot (and /health) never waits on Mongo — a 10s index
	// budget during an Atlas blip must not hold readiness. Serving uses
	// the same cached store every verb (lazy per-verb fallback until the
	// cache lands, then a 60s-budgeted Mongo retry while non-durable),
	// and the boot lines below may lag "listening" by seconds — poll the
	// log, don't grep it once.
	go initConnectors()
	log.Print("connectors: boot check started (OAuth status line follows)")
	log.Print("health-api listening on :8000")
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
