package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
)

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
}

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
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("cannot resolve %q", host)
	}
	for _, ip := range ips {
		if !p.check(ip) {
			continue
		}
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, fmt.Errorf("no allowed address for %q", host)
}

func deny(w http.ResponseWriter, host string) {
	http.Error(w, fmt.Sprintf("forbidden target %q", host), http.StatusForbidden)
}

func (p *filteringProxy) serveConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil || port == "" {
		deny(w, r.Host)
		return
	}
	target, err := p.dialChecked(r.Context(), host, port)
	if err != nil {
		deny(w, host)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		target.Close()
		deny(w, host)
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
		deny(w, r.RequestURI)
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
		deny(w, r.URL.Hostname())
		return
	}
	defer target.Close()
	// Rewrite to origin-form for the upstream server, preserving everything
	// else about the client's request.
	upstream := r.Clone(r.Context())
	upstream.URL.Scheme, upstream.URL.Host = "", ""
	upstream.RequestURI = ""
	if err := upstream.Write(target); err != nil {
		return
	}
	resp, err := http.ReadResponse(bufio.NewReader(target), r)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(a, b); a.Close() }()
	go func() { defer wg.Done(); io.Copy(b, a); b.Close() }()
	wg.Wait()
}
