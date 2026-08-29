package main

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// These tests exercise the same HTTP/JSON-RPC seam as server_test.go
// (tools/call over the mux), but with a fake Kubernetes clientset injected
// via NewMuxWithClient, standing in for a real cluster.

func TestToolsCallK8sGetListsPodsFromCluster(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc123", Namespace: "twotakeone"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	})
	mux := NewMuxWithClient(client)

	out := doRPC(t, mux, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"k8s_get","arguments":{"kind":"pods","namespace":"twotakeone"}}}`)

	result, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("expected result object, got %#v", out)
	}
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("expected success, got error result: %#v", result)
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("expected non-empty content, got %#v", result["content"])
	}
	text, _ := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "web-abc123") {
		t.Fatalf("expected pod name in output, got %q", text)
	}
	if !strings.Contains(text, "Running") {
		t.Fatalf("expected pod status in output, got %q", text)
	}
	if strings.Contains(strings.ToLower(text), "stub") {
		t.Fatalf("expected real cluster data, not stub text, got %q", text)
	}
}

func TestToolsCallK8sGetSinglePodByName(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc123", Namespace: "twotakeone"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	})
	mux := NewMuxWithClient(client)

	out := doRPC(t, mux, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"k8s_get","arguments":{"kind":"pods","namespace":"twotakeone","name":"web-abc123"}}}`)

	result := out["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("expected success, got error result: %#v", result)
	}
	content := result["content"].([]any)
	text, _ := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "web-abc123") || !strings.Contains(text, "twotakeone") {
		t.Fatalf("expected pod detail in output, got %q", text)
	}
}

func TestToolsCallK8sGetRejectsSecrets(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "top-secret", Namespace: "twotakeone"},
		Data:       map[string][]byte{"password": []byte("hunter2")},
	})
	mux := NewMuxWithClient(client)

	out := doRPC(t, mux, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"k8s_get","arguments":{"kind":"secrets","namespace":"twotakeone"}}}`)

	result := out["result"].(map[string]any)
	isErr, _ := result["isError"].(bool)
	if !isErr {
		t.Fatalf("expected isError=true for secrets access, got %#v", result)
	}
	content := result["content"].([]any)
	text, _ := content[0].(map[string]any)["text"].(string)
	if strings.Contains(text, "hunter2") || strings.Contains(text, "top-secret") {
		t.Fatalf("secret data must never appear in tool output, got %q", text)
	}
	if !strings.Contains(strings.ToLower(text), "secret") {
		t.Fatalf("expected error message to mention secrets are forbidden, got %q", text)
	}
}

func TestToolsCallK8sGetRejectsUnknownKind(t *testing.T) {
	mux := NewMuxWithClient(fake.NewSimpleClientset())

	out := doRPC(t, mux, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"k8s_get","arguments":{"kind":"widgets"}}}`)

	result := out["result"].(map[string]any)
	isErr, _ := result["isError"].(bool)
	if !isErr {
		t.Fatalf("expected isError=true for unknown kind, got %#v", result)
	}
}

func TestToolsCallK8sDescribeIncludesRelatedEvents(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc123", Namespace: "twotakeone"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	event := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "web-abc123.evt1", Namespace: "twotakeone"},
		InvolvedObject: corev1.ObjectReference{
			Kind:      "Pod",
			Name:      "web-abc123",
			Namespace: "twotakeone",
		},
		Reason:  "Scheduled",
		Message: "Successfully assigned twotakeone/web-abc123 to node-1",
		Type:    "Normal",
	}
	client := fake.NewSimpleClientset(pod, event)
	mux := NewMuxWithClient(client)

	out := doRPC(t, mux, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"k8s_describe","arguments":{"kind":"pod","namespace":"twotakeone","name":"web-abc123"}}}`)

	result := out["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("expected success, got error result: %#v", result)
	}
	content := result["content"].([]any)
	text, _ := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "web-abc123") {
		t.Fatalf("expected pod name in describe output, got %q", text)
	}
	if !strings.Contains(text, "Scheduled") || !strings.Contains(text, "Successfully assigned") {
		t.Fatalf("expected related event in describe output, got %q", text)
	}
}

func TestToolsCallK8sDescribeRejectsSecrets(t *testing.T) {
	mux := NewMuxWithClient(fake.NewSimpleClientset())

	out := doRPC(t, mux, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"k8s_describe","arguments":{"kind":"secrets","namespace":"twotakeone","name":"top-secret"}}}`)

	result := out["result"].(map[string]any)
	isErr, _ := result["isError"].(bool)
	if !isErr {
		t.Fatalf("expected isError=true for secrets describe, got %#v", result)
	}
}

func TestToolsCallK8sGetClusterScopedNamespace(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "twotakeone"},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	})
	mux := NewMuxWithClient(client)

	out := doRPC(t, mux, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"k8s_get","arguments":{"kind":"namespaces","name":"twotakeone"}}}`)

	result := out["result"].(map[string]any)
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("expected success, got error result: %#v", result)
	}
	content := result["content"].([]any)
	text, _ := content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "twotakeone") || !strings.Contains(text, "Active") {
		t.Fatalf("expected namespace detail in output, got %q", text)
	}
}
