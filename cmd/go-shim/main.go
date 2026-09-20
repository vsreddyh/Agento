// Command go-shim is a localhost reverse proxy in front of OpenCode Go.
//
// hermes-agent 0.19.0 never sends the x-opencode-session header that Go
// hard-requires (MissingSessionID), and the fixed upstream (PR #101864) has
// no installable release. hermes points its provider base_url here
// (HERMES_BASE_URL=http://127.0.0.1:18081); the shim forwards to upstream
// with a stable session header injected, so Go chat flows today.
//
// Session ID: GO_SHIM_SESSION when set, else sha256(OPENCODE_API_KEY)
// formatted as UUID (stable per key — preserves Go's routing/cache affinity).
// Runs as an s6 service inside the gateway container; localhost-only.
package main

import (
	"crypto/sha256"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

const upstreamHost = "opencode.ai"

func sessionID() string {
	if s := strings.TrimSpace(os.Getenv("GO_SHIM_SESSION")); s != "" {
		return s
	}
	sum := sha256.Sum256([]byte(os.Getenv("OPENCODE_API_KEY")))
	h := fmt.Sprintf("%x", sum)
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

func main() {
	target := &url.URL{Scheme: "https", Host: upstreamHost}
	sid := sessionID()
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Director = func(r *http.Request) {
		r.URL.Scheme = "https"
		r.URL.Host = upstreamHost
		// hermes may or may not include the /v1 prefix (depends on how it
		// joins base_url); normalize so upstream always sees /zen/go/v1/...
		// (NewSingleHostReverseProxy is bypassed here — explicit is safer.)
		p := r.URL.Path
		if strings.HasPrefix(p, "/v1/") {
			r.URL.Path = "/zen/go" + p
		} else {
			r.URL.Path = "/zen/go/v1" + p
		}
		r.Host = upstreamHost
		r.Header.Set("x-opencode-session", sid)
		// Identify honestly (Go asks clients not to use generic SDK names).
		if ua := r.Header.Get("User-Agent"); ua == "" || strings.Contains(ua, "Go-http-client") {
			r.Header.Set("User-Agent", "hermes-go-shim/1.0")
		}
	}
	proxy.Transport = &http.Transport{
		MaxIdleConns:        32,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 15 * time.Second,
	}
	proxy.ErrorLog = log.New(os.Stderr, "go-shim: ", log.LstdFlags)
	log.Printf("go-shim listening on 127.0.0.1:18081 -> https://%s/zen/go/v1 (session %s…)", upstreamHost, sid[:8])
	if err := http.ListenAndServe("127.0.0.1:18081", proxy); err != nil {
		log.Fatal(err)
	}
}
