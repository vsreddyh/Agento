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
		if got := m[1]; got != "notes" {
			t.Fatalf("AddTool server literal = %q, want \"notes\"", got)
		}
		if seen[m[2]] {
			t.Errorf("duplicate tool registration %q", m[2])
		}
		seen[m[2]] = true
	}
	if !approvals.KnownServer("notes") {
		t.Fatalf("notes is not in the approval policy table")
	}
	// Registration set must equal the policy-table keys: a registered
	// tool with no rule holds fail-closed 14m in prod, and a rule
	// with no registration is a dead row. Same pin as the
	// gmail/calendar tool-set checks, via approvals.ToolsFor.
	want := map[string]bool{}
	for _, tool := range approvals.ToolsFor("notes") {
		want[tool] = true
	}
	if len(seen) != len(want) {
		t.Fatalf("registered %d tools, policy table has %d", len(seen), len(want))
	}
	for tool := range seen {
		if !want[tool] {
			t.Errorf("registered tool %q has no policy-table rule", tool)
		}
	}
	for tool := range want {
		if !seen[tool] {
			t.Errorf("policy-table tool %q is not registered in main.go", tool)
		}
	}
}
