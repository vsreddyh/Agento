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
	// pin guards nothing — and it must pass no `environment` at all: the
	// launcher reads the root .env itself, so any key here (an inherited
	// `WORKSPACE_MCP_TOOLS`, `WORKSPACE_MCP_BASE_URI`, `PORT`, ...) would
	// reach the server without passing through the launcher's scrub below.
	var cfg struct {
		MCP struct {
			Servers map[string]struct {
				Command     []string          `json:"command"`
				Environment map[string]string `json:"environment"`
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
	var gotEnv []string
	for k := range server.Environment {
		gotEnv = append(gotEnv, k)
	}
	sort.Strings(gotEnv)
	if len(gotEnv) != 0 {
		t.Fatalf("workspace-mcp environment = %q, want none — the launcher reads the root .env itself", gotEnv)
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

	// The loopback-only claim in documentation.md / vps.md rests on these
	// scrubs, not on the argv: the stdio OAuth callback binds the hostname
	// from WORKSPACE_MCP_BASE_URI, so an inherited 0.0.0.0 would publish the
	// callback (and the attachment route) while the flags above stayed green.
	// Pinned as text because there is nothing else to pin — script scrubs are
	// not observable from a Go test without spawning a listener.
	flat := strings.Join(strings.Fields(strings.ReplaceAll(string(raw), "\\\n", " ")), " ")
	for _, want := range []string{
		"unset WORKSPACE_MCP_HOST WORKSPACE_MCP_BASE_URI WORKSPACE_EXTERNAL_URL",
		"unset WORKSPACE_MCP_HTTP_PORT",
		"unset WORKSPACE_MCP_TOOLS WORKSPACE_MCP_TOOL_TIER WORKSPACE_MCP_READ_ONLY WORKSPACE_MCP_PERMISSIONS",
		// The callback port and its redirect must stay one derived value: an
		// inherited GOOGLE_OAUTH_REDIRECT_URI would point at a port the
		// operator is not forwarding.
		`export GOOGLE_OAUTH_REDIRECT_URI="http://localhost:$WORKSPACE_MCP_PORT/oauth2callback"`,
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("launcher is missing %q — deleting it lets an inherited env var widen the documented loopback-only, Gmail-only surface", want)
		}
	}

	// The fail-fast placeholders are the ones .env.example ships, so a
	// copied-but-unedited .env is caught here rather than at Google's consent
	// screen. Pinned together so changing one side cannot silently disable it.
	rawExample, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"GOOGLE_OAUTH_CLIENT_ID", "GOOGLE_OAUTH_CLIENT_SECRET", "USER_GOOGLE_EMAIL"} {
		placeholder := ""
		for _, line := range strings.Split(string(rawExample), "\n") {
			if rest, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
				placeholder = rest
				break
			}
		}
		if placeholder == "" {
			t.Fatalf(".env.example has no %s= placeholder line", key)
		}
		if !strings.Contains(string(raw), placeholder) {
			t.Errorf("launcher does not reject the .env.example %s placeholder %q — a copied .env would start the server with it", key, placeholder)
		}
	}

	// The pinned upstream version is part of the surface: the flags above were
	// verified against it, so a version change must be deliberate (bump this
	// expectation in the same commit) rather than an env override.
	const installer = "scripts/build-workspace-mcp.sh"
	rawInstaller, err := os.ReadFile(installer)
	if err != nil {
		t.Fatalf("%s: %v", installer, err)
	}
	if !strings.Contains(string(rawInstaller), `WORKSPACE_MCP_VERSION="2.4.2"`) {
		t.Errorf(`%s must pin WORKSPACE_MCP_VERSION="2.4.2" outright — an override installs an unreviewed upstream behind this launcher's flags`, installer)
	}
}

// TestMCPServersLoadTheirOwnEnv pins the launch path, not just the servers.
// T3/opencode runs from a systemd unit whose environment is empty, so the
// `{env:MONGODB_URI}` / `{env:MONGODB_DB}` blocks this file used to carry
// expanded to "" and the six Go servers exited with "MONGODB_URI is not set"
// (2026-11-14; workspace-mcp reported its Google credentials unset). The repo
// owns its secrets, so the launcher owns them too:
//
//   - each of the six Go servers runs ./scripts/mcp-run.sh <server>, which
//     fills MONGODB_URI/MONGODB_DB from the root .env and execs bin/<server>
//   - workspace-mcp runs ./scripts/workspace-mcp.sh, which reads the three
//     Google credentials plus the callback-port/token-cache overrides the
//     same way (pinned by TestWorkspaceMCPSurfaceStaysReadOnly)
//
// `{env:` must not come back: under T3's empty systemd unit it expands to
// "", which a launcher's `fill` then treats as unset and replaces anyway —
// dead weight for the keys it covers, and for a key no launcher covers the
// empty value is indistinguishable from unset to the Go server: a required
// key fails loudly, an optional one defaults. The ban is about not bypassing
// the launcher that reads .env, not about a rescue.
func TestMCPServersLoadTheirOwnEnv(t *testing.T) {
	raw, err := os.ReadFile("opencode.json")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "{env:") {
		t.Error("opencode.json uses `{env:...}` again — under T3's empty systemd unit it expands to \"\", the launchers fill from .env anyway, and for a key no launcher covers the empty value is indistinguishable from unset (required keys fail loudly, optional ones default); name the launcher instead")
	}

	var cfg struct {
		MCP struct {
			Servers map[string]struct {
				Command     []string          `json:"command"`
				Environment map[string]string `json:"environment"`
			} `json:"servers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"cookbook":        {"./scripts/mcp-run.sh", "cookbook"},
		"health-check":    {"./scripts/mcp-run.sh", "health-check"},
		"miser-money":     {"./scripts/mcp-run.sh", "miser-money"},
		"project-manager": {"./scripts/mcp-run.sh", "project-manager"},
		"reports":         {"./scripts/mcp-run.sh", "reports"},
		"task-manager":    {"./scripts/mcp-run.sh", "task-manager"},
		"workspace-mcp":   {"./scripts/workspace-mcp.sh"},
	}
	if len(cfg.MCP.Servers) != len(want) {
		t.Errorf("opencode.json registers %d servers, want %d — a new server needs a .env-reading launcher here too", len(cfg.MCP.Servers), len(want))
	}
	for name, cmd := range want {
		server, ok := cfg.MCP.Servers[name]
		if !ok {
			t.Errorf("opencode.json has no `%s` server entry", name)
			continue
		}
		if strings.Join(server.Command, " ") != strings.Join(cmd, " ") {
			t.Errorf("%s command = %q, want %q", name, server.Command, cmd)
		}
		if len(server.Environment) != 0 {
			t.Errorf("%s passes environment %v — the launcher reads the root .env, and a key here would bypass it", name, server.Environment)
		}
	}

	// Both launchers must take their values through the shared loader, and
	// name only the keys their server needs (fill never unsets, so an
	// inherited PASSWORD/provider key still passes through `exec`).
	for _, f := range []string{"scripts/mcp-run.sh", "scripts/workspace-mcp.sh"} {
		rawScript, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		flat := strings.Join(strings.Fields(strings.ReplaceAll(string(rawScript), "\\\n", " ")), " ")
		if !strings.Contains(flat, "scripts/lib/common.sh") {
			t.Errorf("%s does not source scripts/lib/common.sh — the .env must be parsed, never `source`d (a pasted value can contain `$(...)`)", f)
		}
		if !strings.Contains(flat, "load_root_env_fill") {
			t.Errorf("%s does not call load_root_env_fill — the server then depends on the environment T3 does not provide", f)
		}
	}

	rawRun, err := os.ReadFile("scripts/mcp-run.sh")
	if err != nil {
		t.Fatal(err)
	}
	// Exact token list, not a substring: `load_root_env_fill MONGODB_URI
	// MONGODB_DB PASSWORD` contains the substring but hands every one of the
	// six servers a key none of them needs.
	var calls [][]string
	var logical string
	for _, l := range strings.Split(string(rawRun), "\n") {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		logical += strings.TrimSuffix(trimmed, "\\") + " "
		if strings.HasSuffix(trimmed, "\\") {
			continue
		}
		if fields := strings.Fields(logical); len(fields) > 0 && fields[0] == "load_root_env_fill" {
			calls = append(calls, fields)
		}
		logical = ""
	}
	wantKeys := []string{"load_root_env_fill", "MONGODB_URI", "MONGODB_DB"}
	if len(calls) != 1 {
		t.Fatalf("scripts/mcp-run.sh has %d load_root_env_fill calls, want exactly 1", len(calls))
	}
	if strings.Join(calls[0], " ") != strings.Join(wantKeys, " ") {
		t.Errorf("scripts/mcp-run.sh loads %q, want exactly %q — widening this leaks PASSWORD/the provider key into every server", calls[0], wantKeys)
	}

	// Same fail-fast discipline as the Gmail launcher: a copied-but-unedited
	// .env must be caught here, not at Atlas.
	rawExample, err := os.ReadFile(".env.example")
	if err != nil {
		t.Fatal(err)
	}
	placeholder := ""
	for _, l := range strings.Split(string(rawExample), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(l), "MONGODB_URI="); ok {
			placeholder = rest
			break
		}
	}
	if placeholder == "" {
		t.Fatal(".env.example has no MONGODB_URI= placeholder line")
	}
	if !strings.Contains(string(rawRun), placeholder) {
		t.Errorf("scripts/mcp-run.sh does not reject the .env.example MONGODB_URI placeholder %q — a copied .env would fail at Atlas instead", placeholder)
	}
}
