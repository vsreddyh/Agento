package gateway

// Inventory endpoints: the read-only views the Android app builds its provider
// picker and its Skills/MCP pages from.
//
// These three are why the app is not simply "one chat endpoint". ChatApi calls
// GET {path}/api/model/options (and the unprefixed /api/model/options) to populate
// the provider and model dropdowns; ServerApi calls GET {path}/v1/skills and
// /v1/toolsets for the Skills and MCP pages. All four requests are parsed
// leniently by the app and all four currently go to the Hermes API server, so
// without these the migration loses three app features.
//
// Sources are chosen to be truthful rather than convenient:
//
//   - Models come from Pi's get_available_models, so the picker lists what the agent
//     can actually switch to rather than a hand-maintained copy.
//   - Skills come from Pi's get_commands, filtered to source == "skill". That reports
//     what Pi really loaded, with the path and scope it came from. A filesystem scan
//     would be easier and would be wrong: Pi discovers skills from the agent
//     directory and from project directories that require trust, so a directory of
//     SKILL.md files is not the same thing as a loaded skill.
//   - Toolsets come from the agent directory's mcp.json. There is no RPC that
//     enumerates tools or MCP servers, and the entrypoint already refuses to boot if
//     any configured server fails to connect, so the configured set is the live set.
//
// Every response is bounded and reflected values are rune-truncated, like the rest of
// this package: these are authenticated but unauthenticated-prober-visible routes.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// modelOption is one provider row in the app's picker.
//
// The app reads `slug`, `name` and a `models` array of plain strings, and skips any
// row whose slug is blank. Models are listed as ids rather than display names because
// the id is what the chat request sends back as `model`.
type modelOption struct {
	Slug   string   `json:"slug"`
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

// modelOptionsResponse is the `{"providers": [...]}` envelope the app expects.
// A bare array would also parse, but the app reads this shape directly.
type modelOptionsResponse struct {
	Providers []modelOption `json:"providers"`
}

// skillInfo is one row in the app's Skills page: name, description and enabled.
// `path` and `scope` are extra — the app ignores unknown fields, and they are the ones
// that make a surprising entry explainable ("why is this skill here?").
//
// No `category`, though the app reads one. It was here and never populated: the Agent
// Skills frontmatter spec has no such field (name, description, license, compatibility,
// metadata, allowed-tools, disable-model-invocation) and get_commands reports none, so
// there was nothing honest to map. Deriving one from sourceInfo.scope would put
// "user"/"project" into a user-facing grouping field.
//
// Worth being precise about why this mattered at all, since omitempty meant it emitted
// identical bytes: it was a dead field in the type and a claim in the comment, not a
// broken response. That is still worth removing — a field nothing populates invites the
// next person to populate it wrongly — and TestSkillsRowShape now asserts the struct,
// not the JSON, because an output assertion passes with the dead field present.
type skillInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
	Path        string `json:"path,omitempty"`
	Scope       string `json:"scope,omitempty"`
}

// toolsetInfo is one row in the app's MCP page. `tools` is omitted rather than
// emptied: the app treats a missing array as "unknown" and an empty one as "this
// server has no tools", and only the first is true — Pi exposes no RPC that enumerates
// a server's tools.
type toolsetInfo struct {
	Name        string `json:"name"`
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
	Configured  bool   `json:"configured"`
}

// handleModelOptions serves GET /api/model/options and
// GET /{profile}/api/model/options.
func (s *Server) handleModelOptions(w http.ResponseWriter, r *http.Request, h *agentHandler) {
	// Bounded, unlike a chat turn: see Config.InventoryTimeout. The raw request
	// context here would let a wedged Pi hold this goroutine — and the app's Settings
	// screen — until the client or a proxy gave up, on a call that carries no work
	// worth waiting for.
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.InventoryTimeout)
	defer cancel()

	rec, err := h.runner.agent.Call(ctx, "get_available_models", nil)
	if err != nil {
		// One unreachable agent must not empty the picker for every profile, so this
		// reports the failure instead of an empty list. An empty providers array is
		// indistinguishable from "this server offers nothing".
		s.cfg.Logf("gateway: reading available models: %v", err)
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"could not read the agent's model list")
		return
	}

	var payload struct {
		Models []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Provider string `json:"provider"`
			// Reasoning reports whether the model supports a thinking level at all.
			// Recorded per model rather than surfaced, because the picker has no field
			// for it and the chat request's reasoning_effort is validated against the
			// levels Pi reports for the model actually in use.
			Reasoning bool `json:"reasoning"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Data, &payload); err != nil {
		s.cfg.Logf("gateway: decoding model list: %v", err)
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"could not decode the agent's model list")
		return
	}

	// Grouped by provider, which is the axis the app's dropdown is built on. Order is
	// fixed rather than map-random so the picker does not reshuffle between refreshes.
	order := make([]string, 0, len(payload.Models))
	byProvider := map[string]*modelOption{}
	for _, m := range payload.Models {
		// Bounded at ingestion, before grouping and sorting, so dedup compares what
		// will actually be rendered: two ids differing only past the bound would
		// otherwise collapse into one row on screen but stay two rows here.
		slug := bounded(strings.TrimSpace(m.Provider))
		id := bounded(strings.TrimSpace(m.ID))
		if slug == "" || id == "" {
			// A model with no provider cannot be requested back, and one with no id
			// cannot be named; either would produce a row the app shows but cannot use.
			continue
		}
		row, ok := byProvider[slug]
		if !ok {
			row = &modelOption{Slug: slug, Name: slug} // both bounded above
			byProvider[slug] = row
			order = append(order, slug)
		}
		row.Models = append(row.Models, id)
	}
	sort.Strings(order)

	out := modelOptionsResponse{Providers: make([]modelOption, 0, len(order))}
	for _, slug := range order {
		row := byProvider[slug]
		// Sorted so the model list is stable, and de-duplicated: the same id can appear
		// twice in the registry and a duplicated dropdown entry is just noise.
		sort.Strings(row.Models)
		row.Models = distinctStrings(row.Models)
		out.Providers = append(out.Providers, *row)
	}

	writeJSONResponse(w, http.StatusOK, out)
}

// handleSkills serves GET /{profile}/v1/skills.
func (s *Server) handleSkills(w http.ResponseWriter, r *http.Request, h *agentHandler) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.InventoryTimeout)
	defer cancel()

	rec, err := h.runner.agent.Call(ctx, "get_commands", nil)
	if err != nil {
		s.cfg.Logf("gateway: reading commands: %v", err)
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"could not read the agent's skills")
		return
	}

	var payload struct {
		Commands []struct {
			Name        string `json:"name"`
			Description string `json:"description"`
			Source      string `json:"source"`
			SourceInfo  struct {
				Path  string `json:"path"`
				Scope string `json:"scope"`
			} `json:"sourceInfo"`
		} `json:"commands"`
	}
	if err := json.Unmarshal(rec.Data, &payload); err != nil {
		s.cfg.Logf("gateway: decoding commands: %v", err)
		apiError(w, http.StatusBadGateway, "upstream_error", "",
			"could not decode the agent's skills")
		return
	}

	skills := make([]skillInfo, 0, len(payload.Commands))
	for _, c := range payload.Commands {
		// Only skills. Extension commands (/llama, /mcp) and prompt templates are not
		// skills, and listing them on a Skills page would be wrong rather than
		// merely noisy.
		if c.Source != "skill" {
			continue
		}
		// Pi prefixes a skill's command name with "skill:" so it can be invoked as
		// /skill:name. That prefix is a command-namespace detail and would render as
		// literal text in the app's list.
		name := strings.TrimSpace(strings.TrimPrefix(c.Name, "skill:"))
		if name == "" {
			continue
		}
		skills = append(skills, skillInfo{
			Name:        bounded(name),
			Description: bounded(c.Description),
			Enabled:     true,
			Path:        bounded(c.SourceInfo.Path),
			Scope:       bounded(c.SourceInfo.Scope),
		})
	}
	// Case-insensitive, with the raw name as tiebreak. Comparing only the folded
	// form returns false both ways for "Apple" and "apple", which leaves their order
	// down to sort.Slice's instability — so the app's list would reshuffle between
	// refreshes for reasons no one can see.
	sort.Slice(skills, func(i, j int) bool {
		li, lj := strings.ToLower(skills[i].Name), strings.ToLower(skills[j].Name)
		if li != lj {
			return li < lj
		}
		return skills[i].Name < skills[j].Name
	})

	// A bare array, which is the first shape the app's rootArray tries. An empty
	// result is a valid empty page, not an error — the app shows "Nothing listed".
	writeJSONResponse(w, http.StatusOK, skills)
}

// handleToolsets serves GET /{profile}/v1/toolsets.
func (s *Server) handleToolsets(w http.ResponseWriter, r *http.Request, h *agentHandler) {
	// Per-agent-directory, not per-profile: Pi loads one agent directory, so every
	// profile is served the same server set. h is unused for that reason.
	_ = h

	out := make([]toolsetInfo, 0, len(s.mcpServers))
	for _, name := range s.mcpServers {
		out = append(out, toolsetInfo{
			Name:       name,
			Label:      name,
			Enabled:    true,
			Configured: true,
		})
	}
	writeJSONResponse(w, http.StatusOK, out)
}

// mcpConfigLimit caps mcp.json. The shipped file is well under a kilobyte; this
// exists so a corrupt or hostile file cannot be read into memory whole. It is read
// once at startup, so this is a startup-bounded read, not a per-request one.
const mcpConfigLimit = 1 << 20 // 1 MiB

// readMCPServers lists the MCP server names from <agent-dir>/mcp.json.
//
// Called once from New. mcp.json is the configured set, which the entrypoint has
// already proven connectable: docker/pi/entrypoint.sh runs `pi mcp list` at boot and
// refuses to start if any server fails. So a server listed here is one Pi connected to.
//
// A failure is logged and yields no servers rather than propagating. The caller is
// startup, where an unreadable mcp.json should cost the toolsets page and nothing
// else.
func readMCPServers(dir string, logf func(string, ...any)) []string {
	if dir == "" {
		return nil
	}
	f, err := os.Open(filepath.Join(dir, "mcp.json"))
	if err != nil {
		// Both cases are logged, and both render as an empty page to the app. They are
		// still worth telling apart at startup: "no MCP configured" is a decision,
		// while "mcp.json is missing" is usually a mount that did not happen. Logged
		// once here rather than per request, which is the reason this read moved.
		if os.IsNotExist(err) {
			logf("gateway: no mcp.json in %s; the toolsets list will be empty", dir)
		} else {
			logf("gateway: reading mcp.json: %v", err)
		}
		return nil
	}
	defer func() { _ = f.Close() }()

	// Read one byte past the limit so an oversized file is detectable rather than
	// silently truncated into invalid JSON that happens to parse.
	raw, err := io.ReadAll(io.LimitReader(f, mcpConfigLimit+1))
	if err != nil {
		logf("gateway: reading mcp.json: %v", err)
		return nil
	}
	if len(raw) > mcpConfigLimit {
		logf("gateway: mcp.json exceeds %d bytes; ignoring it", mcpConfigLimit)
		return nil
	}
	var doc struct {
		Servers map[string]struct {
			Command string `json:"command"`
			URL     string `json:"url"`
			// Disabled servers are configured but not connected, and listing them as
			// enabled would be a lie the app has no way to detect.
			Disabled bool `json:"disabled"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		logf("gateway: parsing mcp.json: %v", err)
		return nil
	}

	names := make([]string, 0, len(doc.Servers))
	for name, cfg := range doc.Servers {
		if cfg.Disabled {
			continue
		}
		// A server with neither a command nor a URL cannot start; it is a config
		// error rather than a toolset, and listing it as enabled would be a lie the
		// app cannot detect.
		//
		// Logged rather than skipped in silence, because the usual cause is a typo in
		// the key ("path" instead of "command", say). Without this, the symptom is an
		// MCP page quietly missing a server the operator can see in mcp.json, with
		// nothing anywhere saying why. That is the same shape as the profile-skip
		// silence fixed in the entrypoint.
		if strings.TrimSpace(cfg.Command) == "" && strings.TrimSpace(cfg.URL) == "" {
			logf("gateway: ignoring MCP server %q: neither \"command\" nor \"url\" is set", name)
			continue
		}
		names = append(names, bounded(name))
	}
	sort.Strings(names)
	return names
}

// writeJSONResponse writes a JSON body with the content type set.
//
// Separate from the existing writeJSON because that one is called after a
// WriteHeader on paths that already committed a status; this sets both together, so
// an encoder failure cannot leave a 200 with no body.
func writeJSONResponse(w http.ResponseWriter, status int, payload any) {
	body, err := json.Marshal(payload)
	if err != nil {
		apiError(w, http.StatusInternalServerError, "internal_error", "",
			"could not encode the response")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func distinctStrings(in []string) []string {
	out := in[:0:0]
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
