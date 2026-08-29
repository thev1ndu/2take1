// Package main implements a minimal MCP-over-HTTP server exposing todo
// tools to the agent-worker's Claude tool-use loop.
//
// The JSON-RPC/MCP transport in this file mirrors agent/mcp-server/server.go
// exactly (same envelope shapes, same three methods) -- only the tool
// catalog and backing implementations differ. Tools are backed by the
// existing api/ service (todo text/done/id, unchanged) plus Redis (todo
// priority, which api/ has no concept of).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/redis/go-redis/v9"
)

const (
	jsonRPCVersion = "2.0"

	// JSON-RPC 2.0 reserved error codes (https://www.jsonrpc.org/specification#error_object).
	codeParseError     = -32700
	codeMethodNotFound = -32601
)

// rpcRequest is a JSON-RPC 2.0 request envelope.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is a JSON-RPC 2.0 response envelope.
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

// tool describes one MCP tool, per the "tools/list" result shape.
type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// toolContent is one block of an MCP tool-call result (we only ever emit
// "text" blocks).
type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolCallResult is the "result" payload of a "tools/call" response. Tool
// errors are reported in-band via IsError, per the MCP spec, rather than as
// a JSON-RPC-level error.
type toolCallResult struct {
	Content []toolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// callToolParams is the "params" payload of a "tools/call" request.
type callToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// availableTools is the fixed tool catalog this server advertises.
func availableTools() []tool {
	return []tool{
		{
			Name:        "list_todos",
			Description: "List all todos, merged with their priority, sorted by priority (descending) then id. Priority defaults to 0 if never set.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			Name:        "add_todo",
			Description: "Create a new todo. Starts at priority 0 until set_priority is called on it.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"text": map[string]any{"type": "string", "description": "The todo's text."},
				},
				"required": []string{"text"},
			},
		},
		{
			Name:        "set_priority",
			Description: "Set a todo's priority, an integer from 0 (lowest) to 100 (highest/most urgent).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":       map[string]any{"type": "integer", "description": "The todo's id."},
					"priority": map[string]any{"type": "integer", "description": "0-100, higher is more urgent."},
				},
				"required": []string{"id", "priority"},
			},
		},
		{
			Name:        "complete_todo",
			Description: "Mark a todo as done.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "integer", "description": "The todo's id."},
				},
				"required": []string{"id"},
			},
		},
		{
			Name:        "delete_todo",
			Description: "Permanently delete a todo.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id": map[string]any{"type": "integer", "description": "The todo's id."},
				},
				"required": []string{"id"},
			},
		},
	}
}

// Server holds the dependencies MCP tool handlers need: an HTTP client
// pointed at the existing api/ service (todo text/done/id, unchanged) and a
// Redis client (todo priority, which api/ has no concept of).
type Server struct {
	apiBaseURL string
	httpClient *http.Client
	redis      *redis.Client
}

// NewMux builds the HTTP handler for mcp-server: /healthz and /readyz for
// kubelet probes, and /mcp for the JSON-RPC 2.0 / MCP protocol.
func NewMux(s *Server) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/mcp", s.handleMCP)
	return mux
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// handleReadyz also checks Redis connectivity, since every tool but
// list_todos needs it.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.redis.Ping(r.Context()).Err(); err != nil {
		http.Error(w, "redis unreachable: "+err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeRPC(w, nil, nil, &rpcError{Code: codeParseError, Message: "failed to read request body"})
		return
	}

	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, nil, nil, &rpcError{Code: codeParseError, Message: "invalid JSON"})
		return
	}

	switch req.Method {
	case "initialize":
		writeRPC(w, req.ID, map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo":      map[string]any{"name": "keda-todo-mcp-server", "version": "0.1.0"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}, nil)
	case "tools/list":
		writeRPC(w, req.ID, map[string]any{"tools": availableTools()}, nil)
	case "tools/call":
		writeRPC(w, req.ID, s.handleToolsCall(r.Context(), req.Params), nil)
	default:
		writeRPC(w, req.ID, nil, &rpcError{Code: codeMethodNotFound, Message: fmt.Sprintf("method not found: %s", req.Method)})
	}
}

func (s *Server) handleToolsCall(ctx context.Context, rawParams json.RawMessage) toolCallResult {
	var params callToolParams
	if err := json.Unmarshal(rawParams, &params); err != nil {
		return toolCallResult{
			IsError: true,
			Content: []toolContent{{Type: "text", Text: "invalid tools/call params: " + err.Error()}},
		}
	}

	var (
		text string
		err  error
	)
	switch params.Name {
	case "list_todos":
		text, err = s.runListTodos(ctx)
	case "add_todo":
		text, err = s.runAddTodo(ctx, params.Arguments)
	case "set_priority":
		text, err = s.runSetPriority(ctx, params.Arguments)
	case "complete_todo":
		text, err = s.runCompleteTodo(ctx, params.Arguments)
	case "delete_todo":
		text, err = s.runDeleteTodo(ctx, params.Arguments)
	default:
		return toolCallResult{
			IsError: true,
			Content: []toolContent{{Type: "text", Text: fmt.Sprintf("unknown tool: %s", params.Name)}},
		}
	}
	if err != nil {
		return toolCallResult{
			IsError: true,
			Content: []toolContent{{Type: "text", Text: err.Error()}},
		}
	}

	return toolCallResult{Content: []toolContent{{Type: "text", Text: text}}}
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rpcErr *rpcError) {
	resp := rpcResponse{JSONRPC: jsonRPCVersion, ID: id, Result: result, Error: rpcErr}
	_ = json.NewEncoder(w).Encode(resp)
}
