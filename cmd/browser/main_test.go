package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	// Trailing whitespace goes, blank runs collapse, but leading
	// indentation survives: code blocks and nested lists keep structure.
	got := collapseLines("  hello  \n\n   \n    indented\nworld\n\n")
	if got != "  hello\n\n    indented\nworld" {
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
		// evidenceDir creates the dir itself (Mkdir + exist-retry): it
		// must exist on disk, so a second save can never land in it and
		// clobber the first. Plant a sentinel and check the next call
		// goes elsewhere — that is the no-overwrite property, not just
		// name uniqueness.
		if st, err := os.Stat(d); err != nil || !st.IsDir() {
			t.Fatalf("evidence dir %q not created: %v", d, err)
		}
		if err := os.WriteFile(d+"/sentinel", []byte("first"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Every planted sentinel survives: no later call reused an earlier dir.
	for d := range seen {
		b, err := os.ReadFile(d + "/sentinel")
		if err != nil || string(b) != "first" {
			t.Fatalf("dir %q clobbered: %q %v", d, b, err)
		}
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

func TestGetEvictIdleConcurrent(t *testing.T) {
	// lastUse was once written under ss.mu alone while evictIdle's spare
	// re-check read it under s.mu alone: -race flags the pair. Hammer
	// both so the race detector watches the exact access pair.
	oldTTL := sessionTTL
	sessionTTL = time.Millisecond
	defer func() { sessionTTL = oldTTL }()
	ss := &sessions{byProfile: map[string]*session{}, agentDir: t.TempDir()}
	// Errors travel on a channel: t.Error is goroutine-safe, but calling
	// it from workers keeps failing after the damage — fail fast instead.
	errs := make(chan error, 4)
	done := make(chan struct{})
	for i := 0; i < 4; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				if _, err := ss.get("chat"); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for i := 0; i < 50; i++ {
		ss.evictIdle()
	}
	for i := 0; i < 4; i++ {
		<-done
	}
	close(errs)
	for err := range errs {
		t.Fatal(err)
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
	// Hermetic: evidenceDir creates on disk now, so the real /exports
	// default would need root. The shape (jailed, profile-prefixed) is
	// what this pins, not the production mount point.
	exports = t.TempDir() + "/browser"
	profile = "chat"
	dir, err := evidenceDir()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(dir, exports+"/chat-") {
		t.Fatalf("dir = %q", dir)
	}
}
