package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Seam under test: the HTTP handler mux returned by NewMux(). This is the
// same boundary the backend (milestone 4) will call over ClusterIP, and the
// same one kubelet probes hit. Tests only observe HTTP request/response —
// never internal helpers.

func doRPC(t *testing.T, mux http.Handler, body string) map[string]any {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not valid JSON: %v\nbody: %s", err, rec.Body.String())
	}
	return out
}

func TestHealthz(t *testing.T) {
	mux := NewMux()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestReadyz(t *testing.T) {
	mux := NewMux()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestToolsListReturnsK8sGetAndK8sDescribe(t *testing.T) {
	mux := NewMux()
	out := doRPC(t, mux, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)

	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result object, got %#v", out)
	}
	tools, ok := result["tools"].([]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("expected 2 tools, got %#v", result["tools"])
	}

	names := map[string]bool{}
	for _, tl := range tools {
		tm := tl.(map[string]any)
		names[tm["name"].(string)] = true
	}
	if !names["k8s_get"] || !names["k8s_describe"] {
		t.Fatalf("expected tools k8s_get and k8s_describe, got %v", names)
	}
}

// TestToolsCallK8sGetReturnsLiveClusterContent supersedes milestone 1's stub
// test: k8s_get now returns real client-go data (from a fake clientset
// here), not hardcoded stub text. Kind-specific behavior is covered more
// thoroughly in k8s_test.go; this test just re-confirms the tools/call wire
// shape milestone 1 established still holds.
func TestToolsCallK8sGetReturnsLiveClusterContent(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "example-pod", Namespace: "twotakeone"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	})
	mux := NewMuxWithClient(client)
	out := doRPC(t, mux, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"k8s_get","arguments":{"kind":"pods","namespace":"twotakeone"}}}`)

	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result object, got %#v", out)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("expected non-empty content, got %#v", result["content"])
	}
	first := content[0].(map[string]any)
	text, _ := first["text"].(string)
	if strings.Contains(strings.ToLower(text), "stub") {
		t.Fatalf("expected live cluster data, not stub text, got %q", text)
	}
	if !strings.Contains(text, "example-pod") {
		t.Fatalf("expected pod data, got %q", text)
	}
}

// TestToolsCallK8sDescribeReturnsLiveClusterContent supersedes milestone 1's
// stub test; see TestToolsCallK8sGetReturnsLiveClusterContent above.
func TestToolsCallK8sDescribeReturnsLiveClusterContent(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "example", Namespace: "twotakeone"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	})
	mux := NewMuxWithClient(client)
	out := doRPC(t, mux, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"k8s_describe","arguments":{"kind":"pod","namespace":"twotakeone","name":"example"}}}`)

	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result object, got %#v", out)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("expected non-empty content, got %#v", result["content"])
	}
	first := content[0].(map[string]any)
	text, _ := first["text"].(string)
	if strings.Contains(strings.ToLower(text), "stub") {
		t.Fatalf("expected live cluster data, not stub text, got %q", text)
	}
	if !strings.Contains(text, "example") || !strings.Contains(text, "Events") {
		t.Fatalf("expected describe output with events section, got %q", text)
	}
}

func TestToolsCallUnknownToolReturnsToolError(t *testing.T) {
	mux := NewMux()
	out := doRPC(t, mux, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"does_not_exist","arguments":{}}}`)

	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result object (MCP tool errors are in-band), got %#v", out)
	}
	isErr, _ := result["isError"].(bool)
	if !isErr {
		t.Fatalf("expected isError=true for unknown tool, got %#v", result)
	}
}

func TestUnknownMethodReturnsJSONRPCMethodNotFound(t *testing.T) {
	mux := NewMux()
	out := doRPC(t, mux, `{"jsonrpc":"2.0","id":5,"method":"nope"}`)

	errObj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %#v", out)
	}
	code, _ := errObj["code"].(float64)
	if code != -32601 {
		t.Fatalf("expected JSON-RPC method-not-found code -32601, got %v", errObj["code"])
	}
}

func TestMalformedJSONReturnsParseError(t *testing.T) {
	mux := NewMux()
	out := doRPC(t, mux, `{not json`)

	errObj, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("expected error object, got %#v", out)
	}
	code, _ := errObj["code"].(float64)
	if code != -32700 {
		t.Fatalf("expected JSON-RPC parse error code -32700, got %v", errObj["code"])
	}
}
