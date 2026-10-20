package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The proxy tests inject a permissive check so loopback test servers can
// connect; the default publicIP path is pinned separately by the SSRF tests
// (a 403 for loopback proves the production check engages).
func testProxy() *filteringProxy {
	return &filteringProxy{
		check:     func(net.IP) bool { return true },
		allowPort: func(string) bool { return true },
	}
}

func TestProxyConnectTunnels(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "behind-the-tunnel")
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)

	proxy := httptest.NewServer(testProxy())
	defer proxy.Close()

	// CONNECT through the proxy: dial the proxy, ask for the backend.
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxy.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", u.Host, u.Host)
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(buf[:n]), "200") {
		t.Fatalf("CONNECT status = %q", buf[:n])
	}
	// The tunnel is up: a plain GET through it reaches the backend.
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", u.Host)
	out, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "behind-the-tunnel") {
		t.Fatalf("tunneled body = %q", out)
	}
}

func TestProxyRefusesLoopbackByDefault(t *testing.T) {
	// Permissive ports so the 403 proves the IP gate fired, not the port
	// rule (the ephemeral test port would fail that first).
	proxy := &filteringProxy{check: publicIP, allowPort: func(string) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)
	psrv := httptest.NewServer(proxy)
	defer psrv.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(psrv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// httptest binds loopback: the production check must refuse it.
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", u.Host, u.Host)
	buf := make([]byte, 64)
	n, _ := conn.Read(buf)
	if !strings.Contains(string(buf[:n]), "403") {
		t.Fatalf("loopback CONNECT status = %q, want 403", buf[:n])
	}
}

func TestProxyForwardRefusesLoopbackByDefault(t *testing.T) {
	// The plain-HTTP forward path must enforce the production IP check
	// too: its response bytes are observable, unlike CONNECT tunnels.
	proxy := &filteringProxy{check: publicIP, allowPort: func(string) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "must-not-reach")
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)
	psrv := httptest.NewServer(proxy)
	defer psrv.Close()
	pu, _ := url.Parse(psrv.URL)
	fwd := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	resp, err := fwd.Get(u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("forward to loopback = %d %q, want 403", resp.StatusCode, body)
	}
}

func TestProxyPortRule(t *testing.T) {
	// Permissive IPs but default ports: an ephemeral test port must 403.
	proxy := &filteringProxy{check: func(net.IP) bool { return true }}
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)
	psrv := httptest.NewServer(proxy)
	defer psrv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(psrv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", u.Host, u.Host)
	conn.(*net.TCPConn).SetReadDeadline(time.Now().Add(2 * time.Second))
	out, _ := io.ReadAll(conn)
	if got := string(out); !strings.Contains(got, "403") || !strings.Contains(got, "ports") {
		t.Fatalf("ephemeral-port CONNECT = %q, want a 403 naming ports", got)
	}
}

func TestProxyForwardsPlainHTTP(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "plain-http")
	}))
	defer backend.Close()
	u, _ := url.Parse(backend.URL)

	// Exercise serveForward via a proxy-configured client.
	p := httptest.NewServer(testProxy())
	defer p.Close()
	pu, _ := url.Parse(p.URL)
	fwd := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(pu)}}
	resp, err := fwd.Get(u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "plain-http" {
		t.Fatalf("forwarded body = %q", body)
	}
}
