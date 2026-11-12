package agento_test

// Pin: opencode.json `ask` rules are now the ONLY confirmation gate
// (#503 deleted the in-process policy table and the approval gate).
// Two properties are pinned, both fail-closed:
//
//  1. TestAskSetPinned pins the exact `ask` set: adding, removing or
//     respelling a pattern fails here until the expected set below is
//     updated in the same change.
//  2. TestEveryToolClassified enumerates every tool registered in
//     cmd/*/main.go and requires each to be either `ask`-covered or in
//     the explicit freeTools list. A new tool that matches neither fails
//     CI instead of running default-allow unprompted — the author must
//     consciously classify it (add an `ask` pattern or a free entry).
//
// Sensitivity record (what prompts): money writes, account lifecycle
// (`create_account`/`archive_account`), every `delete_*`, destructive
// rewrites (`fix_last_*`), prunes that delete. The harness cannot
// see arguments, so even a counting prune with dry_run=true prompts.
//
// Servers registered under `cmd/*` are not the whole agent surface: the
// `workspace-mcp` entry in opencode.json runs an external Python server
// (#515) whose tools this file cannot enumerate. Its surface is pinned by
// TestWorkspaceMCPSurfaceStaysReadOnly instead — read-only Gmail only, so
// no write/send tool exists to gate. That is the reason it adds no `ask`
// rule here, and the reason enabling a write means editing that launcher
// and this file together.

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"strings"
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

// freeTools names every registered tool that runs free by policy. A tool
// here must NOT match any `ask` pattern (that would prompt a tool the
// record calls free); a tool in neither list fails the coverage test.
// Update this in the same change as any tool add/rename.
var freeTools = map[string][]string{
	"miser-money": {"list_accounts", "get_balances", "query_transactions", "summarize"},
	"cookbook": {"add_ingredient", "list_ingredients", "add_recipe", "get_recipe",
		"list_recipes", "update_recipe", "scale_recipe", "log_cook", "list_cooks"},
	"health-check": {"log_meal", "query_meals", "log_weight", "log_sleep",
		"log_workout", "daily_summary"},
	"task-manager": {"create_task", "list_tasks", "get_task", "update_task",
		"complete_task", "skip_task", "reopen_task", "task_history"},
	"project-manager": {"create_project", "list_projects", "get_project", "update_project"},
	"reports":         {"create_report", "get_report", "list_reports"},
}

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
	sort.Strings(out)
	return out
}

func TestAskSetPinned(t *testing.T) {
	got := askPatterns(t)
	if len(got) != len(expectedAsk) {
		t.Fatalf("ask set = %q, want %q", got, expectedAsk)
	}
	for i := range got {
		if got[i] != expectedAsk[i] {
			t.Fatalf("ask set = %q, want %q", got, expectedAsk)
		}
	}
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
	return []string{
		tool,
		server + "_" + tool, flat + "_" + tool,
		"mcp_" + server + "_" + tool, "mcp_" + flat + "_" + tool,
		"mcp__" + server + "__" + tool, "mcp__" + flat + "__" + tool,
	}
}

// registeredTools enumerates (server, tool) pairs from the cmd mains by
// reading what the servers actually register — not a hand-written list,
// so a tool added without classification cannot hide from the test.
func registeredTools(t *testing.T) [][2]string {
	t.Helper()
	var out [][2]string
	entries, err := os.ReadDir("cmd")
	if err != nil {
		t.Fatal(err)
	}
	serverRe := regexp.MustCompile(`mcp\.NewServer\(&mcp\.Implementation\{Name:\s*"([^"]+)"`)
	toolRe := regexp.MustCompile(`mcp\.AddTool\(s,\s*&mcp\.Tool\{Name:\s*"([^"]+)"`)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile("cmd/" + e.Name() + "/main.go")
		if err != nil {
			continue
		}
		sm := serverRe.FindSubmatch(raw)
		if sm == nil {
			continue
		}
		server := string(sm[1])
		seen := map[string]bool{}
		for _, m := range toolRe.FindAllSubmatch(raw, -1) {
			tool := string(m[1])
			if !seen[tool] {
				seen[tool] = true
				out = append(out, [2]string{server, tool})
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("no registered tools found under cmd/*/main.go")
	}
	return out
}

func coveredByAsk(patterns []string, server, tool string) bool {
	for _, p := range patterns {
		for _, c := range candidates(server, tool) {
			if matchGlob(p, c) {
				return true
			}
		}
	}
	return false
}

func TestEveryToolClassified(t *testing.T) {
	patterns := askPatterns(t)
	free := map[string]map[string]bool{}
	for server, tools := range freeTools {
		free[server] = map[string]bool{}
		for _, tool := range tools {
			free[server][tool] = true
		}
	}
	n := 0
	for _, st := range registeredTools(t) {
		server, tool := st[0], st[1]
		n++
		covered := coveredByAsk(patterns, server, tool)
		isFree := free[server][tool]
		if covered && isFree {
			t.Errorf("tool %s/%s is ask-covered but listed free — pick one", server, tool)
		}
		if !covered && !isFree {
			t.Errorf("tool %s/%s is neither ask-covered nor listed free — classify it (ask pattern or freeTools entry)", server, tool)
		}
	}
	// Every `ask` pattern must hit at least one non-free (gated) tool —
	// otherwise it is a stale entry matching nothing that prompts.
	for _, p := range patterns {
		hit := false
		for _, st := range registeredTools(t) {
			if free[st[0]][st[1]] {
				continue
			}
			for _, c := range candidates(st[0], st[1]) {
				if matchGlob(p, c) {
					hit = true
					break
				}
			}
		}
		if !hit {
			t.Errorf("`ask` pattern %q matches no gated tool (stale, or prompts for a call that runs free)", p)
		}
	}
	t.Logf("classified %d registered tools", n)
}

// TestWorkspaceMCPSurfaceStaysReadOnly pins the one server whose tools
// registeredTools cannot see: the external Python `workspace-mcp` (#515).
// Its narrow, write-free surface is a property of the launcher's argv, so
// the launcher is the thing under test:
//
//   - read-only Gmail only (`--single-user --read-only --tools gmail
//     --tool-tier core` over stdio) means no send, delete, draft or label
//     tool exists, which is why it contributes no `ask` pattern above.
//   - Enabling a write/send tool therefore requires editing this launcher.
//     The test fails until the author states the change here and, in the
//     same commit, adds the matching `ask` rules to opencode.json (and
//     expectedAsk above).
//
// Only the exec argv is inspected, never the comments around it: comments
// explain these flags and must not be able to satisfy the test.
func TestWorkspaceMCPSurfaceStaysReadOnly(t *testing.T) {
	const launcher = "scripts/workspace-mcp.sh"
	raw, err := os.ReadFile(launcher)
	if err != nil {
		t.Fatalf("%s: %v — opencode.json's `workspace-mcp` entry runs this launcher", launcher, err)
	}

	// opencode.json must still point at the launcher this test pins, or the
	// pin guards nothing.
	var cfg struct {
		MCP struct {
			Servers map[string]struct {
				Command []string `json:"command"`
			} `json:"servers"`
		} `json:"mcp"`
	}
	rawCfg, err := os.ReadFile("opencode.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawCfg, &cfg); err != nil {
		t.Fatal(err)
	}
	server, ok := cfg.MCP.Servers["workspace-mcp"]
	if !ok {
		t.Fatalf("opencode.json has no `workspace-mcp` server entry")
	}
	if len(server.Command) != 1 || server.Command[0] != "./"+launcher {
		t.Fatalf("workspace-mcp command = %q, want [\"./%s\"]", server.Command, launcher)
	}

	var argv []string
	for _, line := range strings.Split(string(raw), "\n") {
		if argv != nil || strings.Contains(line, `exec "$MCP_BIN"`) {
			argv = append(argv, strings.TrimSuffix(strings.TrimSpace(line), `\`))
		}
	}
	if len(argv) == 0 {
		t.Fatalf(`%s: no exec "$MCP_BIN" line found`, launcher)
	}
	fields := strings.Fields(strings.Join(argv, " "))

	// Exact sequence, not substring: `--tools gmail drive` would satisfy a
	// Contains check for "--tools gmail" but must not pass.
	want := []string{
		`exec`, `"$MCP_BIN"`,
		"--transport", "stdio",
		"--single-user",
		"--read-only",
		"--tools", "gmail",
		"--tool-tier", "core",
	}
	if len(fields) != len(want) {
		t.Fatalf("launcher argv = %q, want exactly %q", fields, want)
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Fatalf("launcher argv = %q, want exactly %q", fields, want)
		}
	}
}
