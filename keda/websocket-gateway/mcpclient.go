package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// callTool speaks the same minimal MCP-over-HTTP JSON-RPC shape that
// keda/mcp-server implements. websocket-gateway only ever calls list_todos
// (no arguments needed), so this stays a single small function rather than
// a full client type.
func callTool(ctx context.Context, mcpURL, name string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": map[string]any{}},
	})
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling mcp-server: %w", err)
	}
	defer resp.Body.Close()

	var rpcResp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return "", fmt.Errorf("decoding mcp-server response: %w", err)
	}
	if rpcResp.Error != nil {
		return "", fmt.Errorf("mcp-server: %s", rpcResp.Error.Message)
	}

	var text string
	for _, c := range rpcResp.Result.Content {
		text += c.Text
	}
	if rpcResp.Result.IsError {
		return "", fmt.Errorf("mcp-server tool error: %s", text)
	}
	return text, nil
}
