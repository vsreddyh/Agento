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
// refused (relay-refused + gated-true backstop). A typo'd AddTool name
// ("create_even") holds for approval instead of failing loudly; this test
// reads the registered names out of main.go so the typo fails here, in CI,
// instead of stalling the agent in prod.
//
// TestConnectorPolicySync (connectors package) pins the other direction:
// policy tables stay in agreement with each other. Together they close the
// loop publisher → policy → relay.
func TestPublishedToolsMatchPolicy(t *testing.T) {
	raw, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	names := regexp.MustCompile(`AddTool\(s,\s*&mcp\.Tool\{Name:\s*"([^"]+)"`).FindAllStringSubmatch(string(raw), -1)
	if len(names) == 0 {
		t.Fatal("no AddTool names found in main.go")
	}
	// Presence, not just bucket-membership: a deleted registration
	// leaves fewer names, all still correctly bucketed, and the refusal
	// for direct callers is gone with no test failing. Assert the exact
	// canonical set from connectors.ExposedTools — the single source of
	// truth both this test and TestConnectorPolicySync read — so
	// add/rename/delete fail loudly in both places, not just the
	// edited one.
	want := map[string]bool{}
	for _, tool := range connectors.ExposedTools("calendar") {
		want[tool] = connectors.IsDraftOnlySend("calendar", tool)
	}
	if len(names) != len(want) {
		t.Fatalf("registered %d tools, want %d %v", len(names), len(want), want)
	}
	seen := map[string]bool{}
	for _, m := range names {
		name := m[1]
		if seen[name] {
			t.Errorf("duplicate tool registration %q", name)
		}
		seen[name] = true
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected tool registration %q", name)
		}
		refused := connectors.IsDraftOnlySend("calendar", name)
		holds := approvals.NeedsApproval("calendar", name, nil)
		switch {
		case refused && holds:
			// Refused write with gated-true backstop: correct.
		case !refused && !holds:
			// Free read/draft: correct.
		case refused && !holds:
			t.Errorf("tool %q refused by relay but not gated: bypass runs it free", name)
		default:
			t.Errorf("tool %q holds for approval but is not a refused write: rename sync gap", name)
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
