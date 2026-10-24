package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agento/internal/approvals"
	"agento/internal/connectors"
)

// forward proxies one JSON-RPC call to the official MCP endpoint with the
// vault's access token attached. Transport failures are upstream errors
// (502-shaped at the relay); 401/403 means the grant is dead on Google's
// side, so the answer is reconnect guidance, never provider text.
func (b *bridge) forward(ctx context.Context, method string, params any) (any, error) {
	src, err := connectors.GrantTokenSource(ctx, b.store, b.oauth, b.cfg.connector)
	if err != nil {
		return nil, err
	}
	tok, err := src.Token()
	if err != nil {
		// Refresh failed against Google: the stored grant no longer
		// renews. Reconnect, don't retry with a dead token.
		return nil, fmt.Errorf("%s is not connected; reconnect in Settings — never paste a token in chat", connectors.DisplayName(b.cfg.connector))
	}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.cfg.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	res, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bridge: upstream %s unreachable: %v", b.cfg.connector, err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, maxLine))
	if err != nil {
		return nil, fmt.Errorf("bridge: upstream %s unreadable: %v", b.cfg.connector, err)
	}
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("%s grant was rejected; reconnect in Settings — never paste a token in chat", connectors.DisplayName(b.cfg.connector))
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bridge: upstream %s answered %d", b.cfg.connector, res.StatusCode)
	}
	return decodeUpstream(res.Header.Get("Content-Type"), raw)
}

// decodeUpstream reads a Streamable-HTTP reply: plain JSON, or an SSE
// stream of data: frames (take the last complete JSON-RPC response).
func decodeUpstream(contentType string, raw []byte) (any, error) {
	if strings.Contains(contentType, "text/event-stream") {
		var last json.RawMessage
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			payload, ok := strings.CutPrefix(line, "data:")
			if !ok {
				continue
			}
			payload = strings.TrimSpace(payload)
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var probe struct {
				Result json.RawMessage `json:"result"`
				Error  *rpcError       `json:"error"`
			}
			if err := json.Unmarshal([]byte(payload), &probe); err != nil {
				continue
			}
			if len(probe.Result) > 0 || probe.Error != nil {
				last = []byte(payload)
			}
		}
		if len(last) == 0 {
			return nil, fmt.Errorf("bridge: upstream stream held no response")
		}
		raw = last
	}
	var res rpcResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("bridge: bad upstream response: %v", approvals.ScrubError(string(raw)))
	}
	if res.Error != nil {
		return nil, fmt.Errorf("bridge: upstream error %d: %s", res.Error.Code, approvals.ScrubError(res.Error.Message))
	}
	if res.Result == nil {
		return nil, fmt.Errorf("bridge: upstream answered without a result")
	}
	return res.Result, nil
}
