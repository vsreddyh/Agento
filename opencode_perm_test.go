package agento_test

// Pin: opencode.json `ask` rules are now the ONLY confirmation gate
// (#503 deleted the in-process policy table and the approval gate).
// There is no second list to drift against, so this test pins the exact
// `ask` set instead: adding, removing or respelling a pattern fails here
// until the expected set below is updated in the same change, which keeps
// the widening/narrowing reviewable instead of silent.
//
// What the patterns mean (sensitivity record, formerly the policy table):
// money writes (log_transaction/log_text), account lifecycle
// (create/archive), deletes, destructive rewrites (fix_*) and prunes that
// delete all prompt; reads and routine writes run free. The harness cannot
// see arguments, so even a counting prune with dry_run=true prompts.

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// expectedAsk is the full `ask` set in opencode.json's permission map.
// Object values like external_directory are boundaries, not tool rules.
var expectedAsk = []string{
	"*archive_account*",
	"*create_account*",
	"*delete*",
	"*fix_last*",
	"*log_text*",
	"*log_transaction*",
	"*prune_old*",
}

func TestAskSetPinned(t *testing.T) {
	raw, err := os.ReadFile("opencode.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Permission map[string]any `json:"permission"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	var got []string
	for pattern, effect := range cfg.Permission {
		if s, ok := effect.(string); ok && s == "ask" {
			got = append(got, pattern)
		}
	}
	sort.Strings(got)
	if len(got) != len(expectedAsk) {
		t.Fatalf("ask set = %q, want %q", got, expectedAsk)
	}
	for i := range got {
		if got[i] != expectedAsk[i] {
			t.Fatalf("ask set = %q, want %q", got, expectedAsk)
		}
	}
}
