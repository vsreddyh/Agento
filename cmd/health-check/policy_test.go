package main

import (
	"os"
	"regexp"
	"testing"

	"agento/internal/approvals"
)

// The AddTool server literal must be this binary's id and known to the
// policy table: a typo fails closed silently (every tool on this server
// holds), so pin it here rather than relying on the startup warning
// alone. Same pin as the gmail/calendar policy tests.
func TestGateServerLiteralPinned(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	names := regexp.MustCompile(`AddTool\(s,\s*"([^"]+)",\s*gate,\s*&mcp\.Tool\{Name:\s*"([^"]+)"`).FindAllStringSubmatch(string(raw), -1)
	if len(names) == 0 {
		t.Fatal("no gated AddTool registrations found in main.go")
	}
	seen := map[string]bool{}
	for _, m := range names {
		if got := m[1]; got != "health-check" {
			t.Fatalf("AddTool server literal = %q, want \"health-check\"", got)
		}
		if seen[m[2]] {
			t.Errorf("duplicate tool registration %q", m[2])
		}
		seen[m[2]] = true
	}
	if !approvals.KnownServer("health-check") {
		t.Fatalf("health-check is not in the approval policy table")
	}
}
