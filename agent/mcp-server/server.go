// Package main implements a minimal MCP-over-HTTP server for the Klyde
// agent's privileged cluster-reading pod.
//
// The JSON-RPC/MCP plumbing in this file is transport-only; the two tools
// (k8s_get, k8s_describe) are backed by real client-go calls implemented in
// k8s.go, against whatever kubernetes.Interface the Server is constructed
// with (a real clientset in production, a fake one in tests).
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"k8s.io/client-go/kubernetes"
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
// errors (e.g. unknown tool name) are reported in-band via IsError, per the
// MCP spec, rather than as a JSON-RPC-level error.
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
			Name:        "k8s_get",
			Description: "Get or list a Kubernetes resource. Provide name to get a single resource, omit it to list. Never returns secrets.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind":      map[string]any{"type": "string", "description": "Resource kind, e.g. pods, deployments, services."},
					"namespace": map[string]any{"type": "string", "description": "Namespace to query; omit to list across namespaces."},
					"name":      map[string]any{"type": "string", "description": "Specific resource name; omit to list."},
				},
				"required": []string{"kind"},
			},
		},
		{
			Name:        "k8s_describe",
			Description: "Describe a Kubernetes resource, kubectl-describe style: object detail plus related Events. Never returns secrets.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"kind":      map[string]any{"type": "string", "description": "Resource kind, e.g. pod, deployment."},
					"namespace": map[string]any{"type": "string", "description": "Namespace the resource lives in."},
					"name":      map[string]any{"type": "string", "description": "Resource name."},
				},
				"required": []string{"kind", "namespace", "name"},
			},
		},
	}
}

// Server holds the dependencies MCP tool handlers need. client is the
// Kubernetes API access point; it is a real clientset in production and a
// fake clientset (k8s.io/client-go/kubernetes/fake) in tests.
type Server struct {
	client kubernetes.Interface
}

// NewMux builds the HTTP handler for the mcp-server using a Kubernetes
// client resolved from the ambient environment (see newInClusterClient).
func NewMux() *http.ServeMux {
	mux, _ := newMuxForEnvironment()
	return mux
}

// newMuxForEnvironment is like NewMux but also reports whether a real
// Kubernetes client was found, so main() can log it.
func newMuxForEnvironment() (*http.ServeMux, bool) {
	client := newInClusterClient()
	return NewMuxWithClient(client), client != nil
}

// NewMuxWithClient builds the HTTP handler for the mcp-server: /healthz and
// /readyz for kubelet probes, and /mcp for the JSON-RPC 2.0 / MCP protocol.
// client may be nil (e.g. no in-cluster config found); in that case
// k8s_get/k8s_describe calls fail with a clear in-band tool error instead of
// panicking.
func NewMuxWithClient(client kubernetes.Interface) *http.ServeMux {
	s := &Server{client: client}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealthz)
	mux.HandleFunc("/readyz", handleHealthz)
	mux.HandleFunc("/mcp", s.handleMCP)
	return mux
}

func handleHealthz(w http.ResponseWriter, _ *http.Request) {
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
			"serverInfo":      map[string]any{"name": "klyde-mcp-server", "version": "0.2.0-milestone2"},
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
	case "k8s_get":
		text, err = runK8sGet(ctx, s.client, params.Arguments)
	case "k8s_describe":
		text, err = runK8sDescribe(ctx, s.client, params.Arguments)
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
