package connectors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// mcpJSONPath is the file Pi reads to spawn its MCP servers, relative to this
// package directory (go test runs with the package dir as cwd; same convention
// as internal/skills).
const mcpJSONPath = "../../pi/mcp.json"

// TestConnectorServersWiredIffConfigured guards the deliberate unwiring of the
// connector servers while Google OAuth is unset (#424 Stage 1).
//
// Two directions, and both matter:
//
//   - OAuth unset -> the servers must NOT be in pi/mcp.json. Spawning them
//     costs a process, a Mongo client (6 connections) and ~24 MB each, to serve
//     tools that answer "not configured" on every call.
//   - OAuth set -> they MUST be. This is the direction that would otherwise rot:
//     re-adding the entry is a manual step documented in a PR, and the failure
//     mode is silent — a connector the app reports as connectable with no agent
//     tools behind it at all.
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
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", mcpJSONPath, err)
	}

	_, oauthErr := ServerOAuthConfig()
	configured := oauthErr == nil

	for _, id := range ListIDs() {
		_, wired := cfg.MCPServers[id]
		switch {
		case configured && !wired:
			t.Errorf("%s is missing from %s but Google OAuth IS configured: the connector would advertise tools the agent does not have — add the \"%s\" entry back (see #424 Stage 1)",
				id, mcpJSONPath, id)
		case !configured && wired:
			t.Errorf("%s is wired in %s but Google OAuth is NOT configured (%v): every tool call pays a process and 6 Mongo connections to fail — drop the entry (see #424 Stage 1)",
				id, mcpJSONPath, oauthErr)
		}
	}
}
