package connectors

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

// wantRelayEntry is the only correct shape for a connector entry: the relay in
// front, the real binary behind it.
func wantRelayEntry(id string) mcpEntry {
	return mcpEntry{
		Command: "/usr/local/bin/mcp-gate",
		Args:    []string{"--server", id, "--", "/usr/local/bin/" + id},
	}
}

// knownServers is every id pi/mcp.json may legally contain: the eight wired
// domain/browser servers plus the two connectors, which come and go with OAuth
// (#424 Stage 1). checkWiring flags any other key outright: a typo'd extra id
// (e.g. "gmaill" next to a missing "gmail") would otherwise pass whenever the
// two real connectors agree with the env, while pi spawns a bogus server.
var knownServers = map[string]bool{
	"miser-money": true, "cookbook": true, "health-check": true,
	"task-manager": true, "project-manager": true, "notes": true,
	"memory": true, "browser": true,
	Gmail: true, Calendar: true,
}

// checkWiring returns one problem per connector id whose wiring disagrees with
// whether server-side OAuth is configured. Pure function of its two inputs —
// no env, no filesystem — so both directions are testable deterministically
// (TestCheckWiringBothDirections); the ambient-env test below is only the thin
// wrapper that feeds it the real file and the real credentials.
//
// configured and oauthErr travel together today (configured == (oauthErr == nil))
// but the signature keeps both so the message can quote the reason: when
// unconfigured, oauthErr must be non-nil — checkWiring substitutes a generic
// reason for a nil error rather than dereferencing it, so a future caller
// passing (false, nil) gets a report, not a panic.
func checkWiring(raw []byte, configured bool, oauthErr error) []string {
	var cfg struct {
		MCPServers map[string]mcpEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return []string{"parse pi/mcp.json: " + err.Error()}
	}

	var problems []string
	for id := range cfg.MCPServers {
		if !knownServers[id] {
			problems = append(problems, id+" is in "+mcpJSONPath+
				" but is not a known server id — likely a typo; pi would spawn a bogus server")
		}
	}
	for _, id := range ListIDs() {
		entry, wired := cfg.MCPServers[id]

		if configured && !wired {
			problems = append(problems, id+" is missing from "+mcpJSONPath+
				" but server-side OAuth is configured: the connector would advertise tools the agent does not have. Add it as:\n"+
				"\""+id+"\": {\"command\": \"/usr/local/bin/mcp-gate\", \"args\": [\"--server\", \""+id+"\", \"--\", \"/usr/local/bin/"+id+"\"]}  (see #424 Stage 1)")
			continue
		}
		if !configured && wired {
			reason := "unknown"
			if oauthErr != nil {
				reason = oauthErr.Error()
			}
			problems = append(problems, id+" is wired in "+mcpJSONPath+
				" but server-side OAuth is NOT configured ("+reason+"): every tool call pays a process and 6-9 Mongo connections to fail — drop the entry (see #424 Stage 1)")
			continue
		}
		if !wired {
			continue
		}

		if want := wantRelayEntry(id); entry.Command != want.Command || !reflect.DeepEqual(entry.Args, want.Args) {
			problems = append(problems, id+" in "+mcpJSONPath+
				" is wired in the wrong shape: got command="+entry.Command+
				" args="+strings.Join(entry.Args, " ")+", want /usr/local/bin/mcp-gate --server "+id+" -- /usr/local/bin/"+id+
				" — the relay prefix is what enforces approval")
		}
	}
	return problems
}

// TestConnectorServersWiredIffConfigured guards the deliberate unwiring of the
// connector servers while server-side OAuth is unset (#424 Stage 1).
//
// Unwired-while-unset saves a process, a Mongo client (3 monitors + up to 6
// pooled with the ADR-0011 caps, so 6-9 connections) and ~24 MB per connector,
// for tools that would answer "not configured" on every call. The dangerous
// direction is the other one: re-adding OAuth later without re-adding the
// entries leaves a connector the app reports as connectable with no agent tools
// behind it at all.
//
// "Configured" here is exactly what ServerOAuthConfig requires: the client pair
// plus the envelope key. GOOGLE_REDIRECT_URL is deliberately not part of it
// (the servers never mint consent URLs), so this matches what the servers can
// actually do rather than what a full OAuth setup needs.
func TestConnectorServersWiredIffConfigured(t *testing.T) {
	raw, err := os.ReadFile(filepath.FromSlash(mcpJSONPath))
	if err != nil {
		t.Fatalf("read %s: %v", mcpJSONPath, err)
	}
	_, oauthErr := ServerOAuthConfig()
	for _, p := range checkWiring(raw, oauthErr == nil, oauthErr) {
		t.Error(p)
	}
}

// TestCheckWiringBothDirections pins both halves of the iff deterministically.
// The ambient test above flips on ambient env by design (CI has no credentials
// and enforces unwired; a dev shell with secrets enforces wired), so without
// this, adding OAuth secrets to CI — e.g. for a live-grant test — would
// silently flip what the suite enforces. These subtests own their env
// (t.Setenv) and their input bytes, so they hold in every environment.
func TestCheckWiringBothDirections(t *testing.T) {
	writeMCP := func(t *testing.T, entries map[string]mcpEntry) []byte {
		t.Helper()
		raw, err := json.Marshal(map[string]any{"mcpServers": entries})
		if err != nil {
			t.Fatalf("marshal test mcp.json: %v", err)
		}
		return raw
	}

	setOAuth := func(t *testing.T, configured bool) (bool, error) {
		t.Helper()
		if configured {
			t.Setenv("GOOGLE_CLIENT_ID", "test-client-id")
			t.Setenv("GOOGLE_CLIENT_SECRET", "test-client-secret")
			t.Setenv("CONNECTORS_ENCRYPTION_KEY", strings.Repeat("ab", 32))
		} else {
			t.Setenv("GOOGLE_CLIENT_ID", "")
			t.Setenv("GOOGLE_CLIENT_SECRET", "")
			t.Setenv("CONNECTORS_ENCRYPTION_KEY", "")
		}
		_, err := ServerOAuthConfig()
		return err == nil, err
	}

	both := map[string]mcpEntry{
		Gmail:    wantRelayEntry(Gmail),
		Calendar: wantRelayEntry(Calendar),
	}

	t.Run("unconfigured+unwired passes", func(t *testing.T) {
		configured, oauthErr := setOAuth(t, false)
		if configured {
			t.Fatal("test setup broken: expected unconfigured")
		}
		if ps := checkWiring(writeMCP(t, nil), configured, oauthErr); len(ps) != 0 {
			t.Errorf("want no problems, got %v", ps)
		}
	})

	t.Run("configured+wired passes", func(t *testing.T) {
		configured, oauthErr := setOAuth(t, true)
		if !configured {
			t.Fatalf("test setup broken: %v", oauthErr)
		}
		if ps := checkWiring(writeMCP(t, both), configured, oauthErr); len(ps) != 0 {
			t.Errorf("want no problems, got %v", ps)
		}
	})

	t.Run("unconfigured+wired fails", func(t *testing.T) {
		configured, oauthErr := setOAuth(t, false)
		ps := checkWiring(writeMCP(t, both), configured, oauthErr)
		if len(ps) != 2 {
			t.Errorf("want one problem per connector, got %v", ps)
		}
	})

	t.Run("configured+unwired fails", func(t *testing.T) {
		configured, oauthErr := setOAuth(t, true)
		if !configured {
			t.Fatalf("test setup broken: %v", oauthErr)
		}
		ps := checkWiring(writeMCP(t, nil), configured, oauthErr)
		if len(ps) != 2 {
			t.Errorf("want one problem per connector, got %v", ps)
		}
	})

	t.Run("wrong shape fails even when wired+configured", func(t *testing.T) {
		configured, oauthErr := setOAuth(t, true)
		if !configured {
			t.Fatalf("test setup broken: %v", oauthErr)
		}
		bad := map[string]mcpEntry{
			Gmail:    {Command: "/usr/local/bin/gmail"},
			Calendar: wantRelayEntry(Calendar),
		}
		ps := checkWiring(writeMCP(t, bad), configured, oauthErr)
		if len(ps) != 1 || !strings.Contains(ps[0], "wrong shape") {
			t.Errorf("want the gmail shape problem, got %v", ps)
		}
	})

	t.Run("stray id fails", func(t *testing.T) {
		configured, oauthErr := setOAuth(t, false)
		stray := map[string]mcpEntry{"gmaill": wantRelayEntry(Gmail)}
		ps := checkWiring(writeMCP(t, stray), configured, oauthErr)
		if len(ps) != 1 || !strings.Contains(ps[0], "not a known server id") {
			t.Errorf("want the stray-id problem, got %v", ps)
		}
	})
}
