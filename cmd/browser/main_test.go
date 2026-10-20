package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicIPRejectsNonPublic(t *testing.T) {
	for _, raw := range []string{
		"127.0.0.1", "::1", "10.0.0.1", "172.16.0.1", "192.168.1.1",
		"169.254.169.254", "224.0.0.1", "0.0.0.0", "::",
		"fc00::1", "fe80::1",
	} {
		if publicIP(net.ParseIP(raw)) {
			t.Fatalf("publicIP(%s) = true", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "1.1.1.1", "93.184.216.34", "2606:4700:4700::1111"} {
		if !publicIP(net.ParseIP(raw)) {
			t.Fatalf("publicIP(%s) = false", raw)
		}
	}
	// Version-proof deny ranges: CGNAT, TEST-NET and reserved must not
	// depend on the Go release's IsPrivate table.
	for _, raw := range []string{
		"100.64.0.1", "192.0.2.1", "198.51.100.1", "203.0.113.1",
		"192.0.0.1", "240.0.0.1",
	} {
		if publicIP(net.ParseIP(raw)) {
			t.Fatalf("publicIP(%s) = true", raw)
		}
	}
}

func TestCheckURLShapes(t *testing.T) {
	for _, tc := range []struct {
		name, url string
		ok        bool
	}{
		// Literal IPs, not example.com: the positive cases must not need
		// live DNS (offline CI, sandboxed runners, hijacked resolvers).
		{"https", "https://93.184.216.34/", true},
		{"http", "http://93.184.216.34/a?b=c", true},
		{"uppercase scheme", "HTTPS://93.184.216.34/", true},
		{"empty", "", false},
		{"no scheme", "example.com", false},
		{"file", "file:///etc/passwd", false},
		{"ftp", "ftp://example.com/", false},
		{"javascript", "javascript:alert(1)", false},
		{"credentials", "https://user:pass@example.com/", false},
		{"no host", "https:///x", false},
		{"loopback", "http://127.0.0.1/", false},
		{"private", "http://192.168.0.1/", false},
		{"unresolvable", "http://nonexistent.invalid/", false},
	} {
		_, err := checkURL(tc.url)
		if tc.ok && err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("%s: accepted", tc.name)
		}
	}
}

func TestJailPath(t *testing.T) {
	if _, err := jailPath("/exports/browser", "chat-20260101-000000"); err != nil {
		t.Fatalf("valid name rejected: %v", err)
	}
	for _, bad := range []string{"../x", "a/b", "", ".", "a b"} {
		if _, err := jailPath("/exports/browser", bad); err == nil {
			t.Fatalf("unsafe name %q accepted", bad)
		}
	}
}

func TestCollapseLines(t *testing.T) {
	got := collapseLines("  hello  \n\n   \nworld\n\n")
	if got != "hello\n\nworld" {
		t.Fatalf("collapse = %q", got)
	}
}

func TestEvidenceDirUnique(t *testing.T) {
	oldExports, oldProfile := exports, profile
	defer func() { exports, profile = oldExports, oldProfile }()
	exports = t.TempDir() + "/browser"
	profile = "chat"
	seen := map[string]bool{}
	for i := 0; i < 5; i++ {
		d, err := evidenceDir()
		if err != nil {
			t.Fatal(err)
		}
		if seen[d] {
			t.Fatalf("duplicate evidence dir %q", d)
		}
		seen[d] = true
	}
}

func TestSessionsRejectUnsafeProfile(t *testing.T) {
	ss := &sessions{byProfile: map[string]*session{}}
	if _, err := ss.get("../x"); err == nil {
		t.Fatal("unsafe profile accepted")
	}
	if _, err := ss.get("chat"); err != nil {
		t.Fatalf("valid profile rejected: %v", err)
	}
}

func TestEvictIdleTearsDown(t *testing.T) {
	ss := &sessions{byProfile: map[string]*session{}}
	s := &session{dir: t.TempDir()}
	ss.byProfile["old"] = s
	// lastUse zero value is ancient: eviction must take it.
	ss.evictIdle()
	if _, ok := ss.byProfile["old"]; ok {
		t.Fatal("idle session survived eviction")
	}
	if _, err := os.Stat(s.dir); !os.IsNotExist(err) {
		t.Fatalf("idle session dir survived: %v", err)
	}
}

func TestEvidenceDirLongProfile(t *testing.T) {
	oldExports, oldProfile := exports, profile
	defer func() { exports, profile = oldExports, oldProfile }()
	exports = t.TempDir() + "/browser"
	// 64 chars: legal, but timestamp alone would overflow the 64-char jail.
	// The hash-qualified truncation must still pass it.
	profile = strings.Repeat("p", 64)
	dir, err := evidenceDir()
	if err != nil {
		t.Fatalf("long profile rejected: %v", err)
	}
	if !strings.HasPrefix(dir, exports+"/") || len(filepath.Base(dir)) > 64 {
		t.Fatalf("dir = %q", dir)
	}
}

func TestEvidenceDirShape(t *testing.T) {
	// Globals restored: tests share the process, and a later test must not
	// inherit these values.
	oldExports, oldProfile := exports, profile
	defer func() { exports, profile = oldExports, oldProfile }()
	exports = "/exports/browser"
	profile = "chat"
	dir, err := evidenceDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, "/exports/browser/chat-") {
		t.Fatalf("dir = %q", dir)
	}
}
