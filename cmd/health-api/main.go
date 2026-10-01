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
	log.Print("health-api listening on :8000")
	if err := http.ListenAndServe(":8000", mux); err != nil {
		log.Fatal(err)
	}
}
