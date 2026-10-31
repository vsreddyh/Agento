package connectors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// mcpJSONPath is the file Pi reads to spawn its MCP servers, relative to this
// package directory (go test runs with the package dir as cwd; same convention
// as internal/skills).
const mcpJSONPath = "../../pi/mcp.json"

// mcpEntry is the shape pi/mcp.json uses for every server: the approval relay
// wrapping the real binary. Asserting it here — not just presence — is what
// makes a re-add safe: an entry without the `mcp-gate --server` prefix connects
// fine but bypasses approval, which is precisely the silent regression a
// presence-only check would wave through.
type mcpEntry struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// TestConnectorServersWiredIffConfigured guards the deliberate unwiring of the
// connector servers while server-side OAuth is unset (#424 Stage 1).
//
// Two directions, and both matter:
//
//   - OAuth unset -> the servers must NOT be in pi/mcp.json. Spawning them costs
//     a process, a Mongo client (3 monitors + up to 6 pooled with the ADR-0011
//     caps, so 6-9 connections) and ~24 MB each, to serve tools that answer
//     "not configured" on every call.
//   - OAuth set -> they MUST be, in the relay shape below. This is the direction
//     that would otherwise rot: re-adding the entry is a manual step documented
//     in a PR, and the failure mode is silent — a connector the app reports as
//     connectable with no agent tools behind it at all.
//
// "Configured" here is exactly what ServerOAuthConfig requires: the client pair
// plus the envelope key. GOOGLE_REDIRECT_URL is deliberately not part of it (the
// servers never mint consent URLs), so this matches what the servers can
// actually do rather than what a full OAuth setup needs.
//
// The check is env-dependent by design: CI runs with no credentials and
// enforces the first direction, a deployment that has configured OAuth enforces
// the second when it runs the suite.
func TestConnectorServersWiredIffConfigured(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(mcpJSONPath))
	if err != nil {
		t.Fatalf("read %s: %v", mcpJSONPath, err)
	}
	var cfg struct {
		MCPServers map[string]mcpEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", mcpJSONPath, err)
	}

	_, oauthErr := ServerOAuthConfig()
	configured := oauthErr == nil

	for _, id := range ListIDs() {
		entry, wired := cfg.MCPServers[id]

		if configured && !wired {
			t.Errorf("%s is missing from %s but server-side OAuth is configured: the connector would advertise tools the agent does not have. Add it as:\n"+
				"%q: {\"command\": %q, \"args\": [\"--server\", %q, \"--\", %q]}  (see #424 Stage 1)",
				id, mcpJSONPath, id, "/usr/local/bin/mcp-gate", id, "/usr/local/bin/"+id)
			continue
		}
		if !configured && wired {
			t.Errorf("%s is wired in %s but server-side OAuth is NOT configured (%v): every tool call pays a process and 6-9 Mongo connections to fail — drop the entry (see #424 Stage 1)",
				id, mcpJSONPath, oauthErr)
			continue
		}
		if !wired {
			continue
		}

		want := mcpEntry{
			Command: "/usr/local/bin/mcp-gate",
			Args:    []string{"--server", id, "--", "/usr/local/bin/" + id},
		}
		if entry.Command != want.Command || !reflect.DeepEqual(entry.Args, want.Args) {
			t.Errorf("%s in %s is wired in the wrong shape: got command=%q args=%v, want %q %v — the relay prefix is what enforces approval",
				id, mcpJSONPath, entry.Command, entry.Args, want.Command, want.Args)
		}
	}
}
