// Command health-api is the Health Connect sync endpoint for the health-check bot.
//
// Accepts POSTs from the Agento Android app and persists to the shared
// remote MongoDB collection the health-check bot also reads (hc_days —
// one doc per date, same shape the MCP writes).
//
// Auth: single PASSWORD bearer token (matches the Password field in the
// Agento app Settings; same credential as gateway chat).
//
// Env: MONGODB_URI (required), MONGODB_DB (default: hermes), PASSWORD.
package main

import (
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// IST is the user timezone (UTC+5:30, no DST) as a fixed offset.
var IST = time.FixedZone("IST", 5*3600+30*60)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// tokens reads accepted tokens fresh (env may rotate without a restart).
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
		log.Print("PASSWORD not set — rejecting all requests")
		return http.StatusServiceUnavailable, "server not configured with tokens"
	}
	h := r.Header.Get("Authorization")
	if h == "" || !strings.HasPrefix(h, "Bearer ") {
		return http.StatusUnauthorized, "missing bearer token"
	}
	if !toks[strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))] {
		return http.StatusUnauthorized, "invalid token"
	}
	return 0, ""
}

func main() {
	mux := http.NewServeMux()
	registerHealth(mux)
	registerFiles(mux)
	registerTasks(mux)
	registerProjects(mux)
	// Rate limiting wraps everything, and lives here because main is the only place
	// that knows the whole handler set (#176). The API is network-reachable and
	// authed by ONE shared password, so without a counter a stuck client retrying in
	// a loop — or anyone holding that password — is an unbounded write load on a
	// shared production MongoDB.
	//
	// Wrapping the mux rather than each handler is deliberate: a per-handler limit is
	// a limit someone forgets to add to the next route.
	h := rateLimit(newLimiter(defaultBurst, defaultRefill), mux)
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
	log.Print("health-api listening on :8000")
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}
