package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// A hung CONNECT tunnel pins two goroutines and two FDs with no idle
// timeout; at human-gated browsing volume that is bounded by traffic, and
// process restart reaps whatever lingers.

// filteringProxy is the SSRF backstop Chromium cannot walk around. checkURL
// validates the first URL, but Chromium re-resolves and follows redirects,
// meta-refreshes and JS navigations with no re-check — so every byte the
// browser sends goes through this proxy, and every outbound connection is
// resolved and checked again here. A redirect to 169.254.169.254 or a DNS
// rebinding from public to private dies at connect time, not at the
// approval sheet (which only ever saw the first, harmless-looking URL).
//
// HTTPS travels as CONNECT tunnels: the proxy sees hostnames, never
// plaintext, so page content is not observable here by construction. Plain
// HTTP is forwarded request/response verbatim.
type filteringProxy struct {
	// check approves one resolved IP. publicIP in production; permissive
	// in tests, where loopback test servers must connect.
	check func(net.IP) bool
	// allowPort approves a destination port. Web ports in production;
	// tests listen on ephemeral ports and open it up.
	allowPort func(port string) bool
}

func webPorts(port string) bool { return port == "80" || port == "443" }

func (p *filteringProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.serveConnect(w, r)
		return
	}
	p.serveForward(w, r)
}

// dialChecked resolves host and dials the first IP the check approves. The
// dial goes to the approved literal, not back through the resolver, so a
// record changing between lookup and connect cannot slip a second,
// unchecked address into the same connection.
func (p *filteringProxy) dialChecked(ctx context.Context, host, port string) (net.Conn, error) {
	if host == "" || port == "" {
		return nil, fmt.Errorf("bad address %q", host+":"+port)
	}
	// Own dial budget: a CONNECT's request ctx has no deadline, so a
	// stalled upstream resolver must not pin the tunnel goroutine. The
	// budget covers lookup + dial only; relayed bytes flow after.
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	defer dialCancel()
	ips, err := net.DefaultResolver.LookupIP(dialCtx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("cannot resolve %q", host)
	}
	// Same port rule as checkURL: the proxy polices redirects and
	// re-resolutions, which never see the first gate.
	allow := p.allowPort
	if allow == nil {
		allow = webPorts
	}
	if !allow(port) {
		return nil, fmt.Errorf("only ports 80 and 443 are allowed, not %q", port)
	}
	for _, ip := range ips {
		if !p.check(ip) {
			continue
		}
		conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, fmt.Errorf("no allowed address for %q", host)
}

func deny(w http.ResponseWriter, msg string) {
	http.Error(w, msg, http.StatusForbidden)
}

func (p *filteringProxy) serveConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || port == "" {
		deny(w, r.Host)
		return
	}
	target, err := p.dialChecked(r.Context(), host, port)
	if err != nil {
		deny(w, err.Error())
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		target.Close()
		deny(w, "tunnel unavailable")
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		target.Close()
		return
	}
	// 200 to the client before relaying: a CONNECT tunnel starts here, and
	// anything else (including the 403 above) ends it.
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		conn.Close()
		target.Close()
		return
	}
	relay(conn, target)
}

func (p *filteringProxy) serveForward(w http.ResponseWriter, r *http.Request) {
	// Forward proxies receive the absolute URI. Anything else is a
	// misdirected request, not something to interpret.
	if !r.URL.IsAbs() || r.URL.Host == "" {
		deny(w, fmt.Sprintf("bad request target %q", r.RequestURI))
		return
	}
	port := r.URL.Port()
	if port == "" {
		if r.URL.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	target, err := p.dialChecked(r.Context(), r.URL.Hostname(), port)
	if err != nil {
		deny(w, err.Error())
		return
	}
	defer target.Close()
	// One deadline for the whole response — write + headers + body copy —
	// not just connect/headers: a slow large origin streaming past 60s
	// truncates mid-copy. Accepted for human-gated v1 (the dial budget above
	// is the SSRF control; this is only liveness). Do not tighten it
	// thinking it bounds headers alone.
	target.SetDeadline(time.Now().Add(60 * time.Second))
	// Rewrite to origin-form for the upstream server, preserving everything
	// else about the client's request — except hop-by-hop headers, which
	// describe this hop, not the next one (RFC 7230 6.1), and proxy
	// credentials, which are ours, not the origin's.
	upstream := r.Clone(r.Context())
	upstream.URL.Scheme, upstream.URL.Host = "", ""
	// Userinfo never forwards: checkURL rejects it on the first URL, so any
	// here arrived via redirect — exactly the path this proxy polices.
	upstream.URL.User = nil
	upstream.RequestURI = ""
	stripHopHeaders(upstream.Header)
	upstream.Header.Del("Proxy-Authorization")
	upstream.Header.Del("Proxy-Authenticate")
	upstream.Header.Del("Authorization")
	if err := upstream.Write(target); err != nil {
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(target), r)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	// Hop-by-hop headers describe the proxy-to-origin hop, not the
	// client-facing one; copying them verbatim confuses keep-alive reuse.
	stripHopHeaders(resp.Header)
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

// stripHopHeaders removes the headers named by Connection plus the fixed
// hop-by-hop set. Transfer-Encoding is deliberately kept: Go's response
// reader needs it to frame a chunked body.
func stripHopHeaders(h http.Header) {
	for _, v := range h["Connection"] {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, k := range []string{"Connection", "Keep-Alive", "Proxy-Connection", "Upgrade"} {
		h.Del(k)
	}
}

func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(a, b); a.Close() }()
	go func() { defer wg.Done(); io.Copy(b, a); b.Close() }()
	wg.Wait()
}
