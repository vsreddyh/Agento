package main

import (
	"os"
	"regexp"
	"testing"

	"agento/internal/approvals"
	"agento/internal/connectors"
)

// TestPublishedToolsMatchPolicy pins the publisher side of the rename-sync
// obligation: every tool name this binary registers in AddTool must land
// in exactly one policy bucket — free (runs ungated, never refused) or
// refused (gate-refused + sensitive-true backstop). A typo'd AddTool name
// ("search_gmial") denies at once instead of failing loudly; this test
// reads the registered names out of main.go so the typo fails here, in CI,
// instead of silently denying in prod.
//
// TestConnectorPolicySync (connectors package) pins the other direction:
// policy tables stay in agreement with each other. Together they close the
// loop publisher → policy → gate.
func TestPublishedToolsMatchPolicy(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	names := regexp.MustCompile(`AddTool\(s,(?:\s*"([^"]+)",\s*gate,\s*)?&mcp\.Tool\{Name:\s*"([^"]+)"`).FindAllStringSubmatch(string(raw), -1)
	if len(names) == 0 {
		t.Fatal("no AddTool names found in main.go")
	}
	// The server literal must be this binary's id and known to the
	// policy table: a typo fails closed silently (every tool is denied), so
	// pin it here rather than relying on the startup warning alone.
	for _, m := range names {
		if got := m[1]; got == "" || got != "gmail" {
			t.Fatalf("AddTool server literal = %q, want \"gmail\"", got)
		}
	}
	if !approvals.KnownServer("gmail") {
		t.Fatalf("gmail is not in the approval policy table")
	}
	sweeps := regexp.MustCompile(`MarkServerInterrupted\(sweepCtx,\s*"([^"]+)"`).FindAllStringSubmatch(string(raw), -1)
	if len(sweeps) != 1 || sweeps[0][1] != "gmail" {
		t.Fatalf("startup sweep literal = %v, want exactly one for \"gmail\"", sweeps)
	}
	// Presence, not just bucket-membership: a deleted registration
	// leaves fewer names, all still correctly bucketed, and the refusal
	// for direct callers is gone with no test failing. Assert the exact
	// canonical set from connectors.ExposedTools — the single source of truth both this
	// test and TestConnectorPolicySync read — so add/rename/delete
	// fail loudly in both places, not just the edited one.
	want := map[string]bool{}
	for _, tool := range connectors.ExposedTools("gmail") {
		want[tool] = connectors.IsDraftOnlySend("gmail", tool)
	}
	if len(names) != len(want) {
		t.Fatalf("registered %d tools, want %d %v", len(names), len(want), want)
	}
	seen := map[string]bool{}
	for _, m := range names {
		name := m[2]
		if seen[name] {
			t.Errorf("duplicate tool registration %q", name)
		}
		seen[name] = true
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected tool registration %q", name)
		}
		refused := connectors.IsDraftOnlySend("gmail", name)
		sensitive := approvals.NeedsApproval("gmail", name, nil)
		switch {
		case refused && sensitive:
			// Refused write with sensitive-true backstop: correct.
		case !refused && !sensitive:
			// Free read/draft: correct.
		case refused && !sensitive:
			t.Errorf("tool %q refused by gate but not sensitive: bypass runs it free", name)
		default:
			t.Errorf("tool %q is sensitive but is not a refused write: rename sync gap", name)
		}
	}
	// Both directions: the count check catches add/delete with a changed
	// total, but a dup+delete pair keeps the length — name the absent
	// tool too, so the failure says what's missing, not just what's
	// duplicated.
	for tool := range want {
		if !seen[tool] {
			t.Errorf("missing tool registration %q", tool)
		}
	}
}
