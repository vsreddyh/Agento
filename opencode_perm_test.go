package agento_test

// Pin: opencode.json `ask` rules mirror the approval policy table (#487).
//
// Confirmation moved from the in-process hold to T3/opencode permission
// prompts, which leaves two hand-maintained lists — the gated==true set in
// internal/approvals and the `ask` patterns in opencode.json — with drift
// in the unsafe direction: add a gated tool, forget the pattern, and the
// gate runs it at once under opencode default-allow. This test fails CI on
// either drift direction instead.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"agento/internal/approvals"
	"agento/internal/connectors"
)

// askPatterns reads the string-valued `ask` entries of opencode.json's
// permission map (object values like external_directory are boundaries,
// not tool rules).
func askPatterns(t *testing.T) []string {
	t.Helper()
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
	var out []string
	for pattern, effect := range cfg.Permission {
		if s, ok := effect.(string); ok && s == "ask" {
			out = append(out, pattern)
		}
	}
	return out
}

// matchGlob reports the repo's `*`-wildcard match: `*` spans any run,
// everything else is literal. Patterns and tool ids are lowercase by
// construction on both sides.
func matchGlob(pattern, value string) bool {
	if pattern == "*" {
		return true
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == value
	}
	pos := 0
	for i, part := range parts {
		if part == "" {
			continue
		}
		if i == 0 {
			if !strings.HasPrefix(value, part) {
				return false
			}
			pos = len(part)
			continue
		}
		j := strings.Index(value[pos:], part)
		if j < 0 {
			return false
		}
		pos += j + len(part)
	}
	return parts[len(parts)-1] == "" || strings.HasSuffix(value, parts[len(parts)-1])
}

// candidates spells one (server, tool) every way an opencode tool id might
// carry it, so the pin does not depend on the harness's exact prefix
// style — a pattern must hit at least one spelling.
func candidates(server, tool string) []string {
	flat := strings.ReplaceAll(server, "-", "_")
	return []string{tool, server + "_" + tool, flat + "_" + tool}
}

func TestAskMirrorsGated(t *testing.T) {
	patterns := askPatterns(t)
	for _, server := range approvals.Servers() {
		for _, tool := range approvals.ToolsFor(server) {
			// Draft-refused sends can never run however permitted, so
			// they need no `ask` entry (and must not have one — see
			// TestAskHasNoRefusedSends).
			if connectors.IsDraftOnlySend(server, tool) {
				continue
			}
			// Allow-path tools run free by policy; only gated ones must
			// prompt. Empty args: the dry-run prune exemption is an
			// in-gate concern the harness cannot see (patterns match
			// names, not arguments), so the counting prune prompts too.
			if !approvals.NeedsApproval(server, tool, map[string]any{}) {
				continue
			}
			covered := false
			for _, p := range patterns {
				for _, c := range candidates(server, tool) {
					if matchGlob(p, c) {
						covered = true
						break
					}
				}
			}
			if !covered {
				t.Errorf("gated %s/%s has no `ask` pattern in opencode.json", server, tool)
			}
		}
	}
}

func TestAskHasNoOrphansOrRefusals(t *testing.T) {
	patterns := askPatterns(t)
	// Every `ask` pattern must hit at least one gated, runnable tool —
	// otherwise it is a stale entry (or, worse, an entry that prompts for
	// a call the gate refuses outright).
	for _, p := range patterns {
		hit := false
		for _, server := range approvals.Servers() {
			for _, tool := range approvals.ToolsFor(server) {
				if connectors.IsDraftOnlySend(server, tool) {
					continue
				}
				if !approvals.NeedsApproval(server, tool, map[string]any{}) {
					continue
				}
				for _, c := range candidates(server, tool) {
					if matchGlob(p, c) {
						hit = true
						break
					}
				}
			}
		}
		if !hit {
			t.Errorf("`ask` pattern %q matches no gated tool (stale, or prompts for a call the gate refuses)", p)
		}
	}
}
