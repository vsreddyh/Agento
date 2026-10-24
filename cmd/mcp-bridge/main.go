// Command mcp-bridge is the stdio front for the official Google Workspace
// MCP servers (#316, option A).
//
// Pi spawns MCP servers as stdio subprocesses, but Google's servers are
// remote Streamable-HTTP endpoints with their own OAuth. This bridge is the
// adapter: it speaks MCP over stdio to Pi (behind mcp-gate like every
// server) and forwards tool calls to the official endpoints with the
// vault's access token attached — so the in-app connect flow stays the
// single consent and the sealed-token vault stays the single credential
// home. No grant (or a dead one) answers reconnect guidance, never a
// secrets prompt.
//
// The bridge exposes a read/draft subset and filters the rest: the
// official Gmail server's label tools and the Calendar server's write
// tools are dropped from tools/list, so Pi can never call them. The relay
// refusal (connectors.IsDraftOnlySend) and the gated-true policy rows
// stay as the backstop for any caller that bypasses this filter.
// create_event_draft is bridge-local (validated, sent nowhere — the
// calendar tools it would need are exactly the refused ones).
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"agento/internal/connectors"
)

var (
	serverFlag = flag.String("server", "", "connector server to front (gmail or calendar)")
	httpClient = &http.Client{Timeout: 30 * time.Second}
)

// maxLine bounds one NDJSON frame. Threads can be large; 16MB matches the
// largest body the bridge is willing to relay rather than the smallest the
// client might send — oversized input fails the scan instead of growing
// memory without bound.
const maxLine = 16 << 20

type serverConfig struct {
	connector string
	endpoint  string
	exclude   map[string]bool
}

func bridgeConfig(name string) (serverConfig, error) {
	endpoint := os.Getenv("CONNECTORS_MCP_ENDPOINT")
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "gmail":
		ep := "https://gmailmcp.googleapis.com/mcp/v1"
		if endpoint != "" {
			ep = endpoint
		}
		return serverConfig{connectors.Gmail, ep, map[string]bool{
			// Label writes are outside the read/draft subset: filtered
			// here so Pi never sees them.
			"label_message": true, "label_thread": true,
			"unlabel_message": true, "unlabel_thread": true,
			"create_label": true,
		}}, nil
	case "calendar":
		ep := "https://calendarmcp.googleapis.com/mcp/v1"
		if endpoint != "" {
			ep = endpoint
		}
		return serverConfig{connectors.Calendar, ep, map[string]bool{
			// Real writes on the official server: filtered here,
			// refused at the relay as backstop.
			"create_event": true, "update_event": true,
			"delete_event": true, "respond_to_event": true,
		}}, nil
	}
	return serverConfig{}, fmt.Errorf("unknown bridge server %q", name)
}

// rpcRequest is the minimal JSON-RPC shape this bridge speaks: every
// inbound frame is one line, every reply one line. Notifications (no id)
// get no reply.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type bridge struct {
	cfg   serverConfig
	store connectors.Store
	oauth *connectors.OAuthConfig
	out   *bufio.Writer
}

func main() {
	flag.Parse()
	log.SetPrefix("mcp-bridge: ")
	cfg, err := bridgeConfig(*serverFlag)
	if err != nil {
		log.Fatal(err)
	}
	store, err := connectors.FromEnv()
	if err != nil {
		log.Fatalf("bridge: %v", err)
	}
	oauth, err := connectors.ServerOAuthConfig()
	if err != nil {
		log.Fatalf("bridge: %v", err)
	}
	b := &bridge{cfg: cfg, store: store, oauth: oauth, out: bufio.NewWriter(os.Stdout)}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		b.handleLine(line)
	}
	if err := sc.Err(); err != nil {
		log.Fatalf("bridge: stdin: %v", err)
	}
}

func (b *bridge) handleLine(line []byte) {
	var req rpcRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return // not JSON-RPC; mcp-gate never sends this, drop silently
	}
	if len(req.ID) == 0 {
		return // notification: no reply by contract
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var res rpcResponse
	res.JSONRPC = "2.0"
	res.ID = req.ID
	switch req.Method {
	case "initialize":
		res.Result = map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo":      map[string]any{"name": *serverFlag, "version": "1.0.0"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}
	case "ping":
		res.Result = map[string]any{}
	case "tools/list":
		tools, err := b.listTools(ctx)
		if err != nil {
			res.Error = &rpcError{Code: -32603, Message: err.Error()}
			break
		}
		res.Result = map[string]any{"tools": tools}
	case "tools/call":
		out, err := b.callTool(ctx, req.Params)
		if err != nil {
			res.Error = &rpcError{Code: -32603, Message: err.Error()}
			break
		}
		res.Result = out
	default:
		res.Error = &rpcError{Code: -32601, Message: "unknown method " + req.Method}
	}
	raw, _ := json.Marshal(res)
	b.out.Write(raw)
	b.out.WriteByte('\n')
	b.out.Flush()
}

// toolSpec is the forwarded shape: the bridge passes remote specs through
// untouched (no drift between what Google publishes and what Pi sees),
// dropping only the excluded writes.
type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

func (b *bridge) listTools(ctx context.Context) ([]any, error) {
	remote, err := b.forward(ctx, "tools/list", nil)
	if err != nil {
		return nil, err
	}
	var listed struct {
		Tools []toolSpec `json:"tools"`
	}
	if raw, err := json.Marshal(remote); err != nil {
		return nil, err
	} else if err := json.Unmarshal(raw, &listed); err != nil {
		return nil, fmt.Errorf("bridge: bad tools/list from upstream: %v", err)
	}
	out := make([]any, 0, len(listed.Tools)+1)
	for _, t := range listed.Tools {
		if b.cfg.exclude[t.Name] {
			continue
		}
		out = append(out, t)
	}
	if b.cfg.connector == connectors.Calendar {
		out = append(out, draftToolSpec())
	}
	return out, nil
}

func (b *bridge) callTool(ctx context.Context, params json.RawMessage) (any, error) {
	var call struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments,omitempty"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return nil, fmt.Errorf("bridge: bad tools/call params")
	}
	if call.Name == draftToolName && b.cfg.connector == connectors.Calendar {
		return callDraft(call.Arguments)
	}
	if b.cfg.exclude[call.Name] {
		// Defense in depth: tools/list filters these, so a call means a
		// client that bypassed the listing. Refuse, don't forward.
		return nil, fmt.Errorf("%s", connectors.RefusalMessage)
	}
	return b.forward(ctx, "tools/call", map[string]any{"name": call.Name, "arguments": call.Arguments})
}
