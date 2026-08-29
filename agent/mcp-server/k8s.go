// Real client-go implementations of the k8s_get and k8s_describe MCP tools.
//
// Resource kinds handled here are exactly the ones granted to the
// agent-mcp-reader ClusterRole (agent/manifests/clusterrole-mcp-reader.yaml):
// pods, deployments, replicasets, statefulsets, daemonsets, services,
// endpoints, configmaps, namespaces, nodes, events, jobs, cronjobs,
// ingresses, persistentvolumeclaims.
//
// "secrets" is deliberately NOT supported, even though callers may ask for
// it: the mcp-server never calls the Secrets API. See agent/PLAN.md's RBAC
// section — an LLM-driven agent reading Secret values is a real
// exfiltration risk. This is enforced in code (canonicalKind/errSecrets
// below), independent of and in addition to the RBAC boundary.
package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// errSecretsForbidden is returned verbatim (as a tool error, never a panic
// or a silent empty result) whenever a caller asks for "secrets" in any
// spelling. It deliberately does not echo back the requested name/namespace
// to avoid giving any hint that a particular secret exists.
var errSecretsForbidden = errors.New("access to \"secrets\" is not permitted: mcp-server never reads Secret data, by design (see agent/PLAN.md)")

// errNoClient is returned when the server has no Kubernetes client (e.g.
// running locally outside a cluster with no kubeconfig wired up).
var errNoClient = errors.New("no Kubernetes client available: mcp-server is not running in-cluster")

const maxListItems = 200

// kindAliases maps accepted spellings (singular/plural, and the literal
// resource name) to the canonical plural resource name used everywhere
// else in this file. "secret"/"secrets" are recognized here specifically so
// they can be rejected with a clear message rather than falling through to
// "unknown kind".
var kindAliases = map[string]string{
	"pod": "pods", "pods": "pods",
	"deployment": "deployments", "deployments": "deployments",
	"replicaset": "replicasets", "replicasets": "replicasets",
	"statefulset": "statefulsets", "statefulsets": "statefulsets",
	"daemonset": "daemonsets", "daemonsets": "daemonsets",
	"service": "services", "services": "services",
	"endpoint": "endpoints", "endpoints": "endpoints",
	"configmap": "configmaps", "configmaps": "configmaps",
	"namespace": "namespaces", "namespaces": "namespaces",
	"node": "nodes", "nodes": "nodes",
	"event": "events", "events": "events",
	"job": "jobs", "jobs": "jobs",
	"cronjob": "cronjobs", "cronjobs": "cronjobs",
	"ingress": "ingresses", "ingresses": "ingresses",
	"persistentvolumeclaim": "persistentvolumeclaims", "persistentvolumeclaims": "persistentvolumeclaims", "pvc": "persistentvolumeclaims", "pvcs": "persistentvolumeclaims",
	"secret": "secrets", "secrets": "secrets",
}

// clusterScopedKinds are the canonical kinds that take no namespace.
var clusterScopedKinds = map[string]bool{
	"namespaces": true,
	"nodes":      true,
}

// canonicalToKubeKind maps a canonical resource name to the capitalized
// Kind string Kubernetes Events use in their involvedObject.kind field.
var canonicalToKubeKind = map[string]string{
	"pods":                   "Pod",
	"deployments":            "Deployment",
	"replicasets":            "ReplicaSet",
	"statefulsets":           "StatefulSet",
	"daemonsets":             "DaemonSet",
	"services":               "Service",
	"endpoints":              "Endpoints",
	"configmaps":             "ConfigMap",
	"namespaces":             "Namespace",
	"nodes":                  "Node",
	"events":                 "Event",
	"jobs":                   "Job",
	"cronjobs":               "CronJob",
	"ingresses":              "Ingress",
	"persistentvolumeclaims": "PersistentVolumeClaim",
}

func canonicalKind(kind string) (string, bool) {
	c, ok := kindAliases[strings.ToLower(strings.TrimSpace(kind))]
	return c, ok
}

// summary is a concise, LLM-context-friendly stand-in for a full Kubernetes
// object: just enough to identify and triage the resource, never a raw
// object dump (no managedFields, no full spec/status trees).
type summary struct {
	Kind      string
	Name      string
	Namespace string
	Status    string
	Extra     string // pre-formatted "Key: Value\n" lines, detail-level info
}

func (s summary) format() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Kind:      %s\n", s.Kind)
	fmt.Fprintf(&b, "Name:      %s\n", s.Name)
	if s.Namespace != "" {
		fmt.Fprintf(&b, "Namespace: %s\n", s.Namespace)
	}
	if s.Status != "" {
		fmt.Fprintf(&b, "Status:    %s\n", s.Status)
	}
	if s.Extra != "" {
		b.WriteString(s.Extra)
	}
	return b.String()
}

func (s summary) oneLine() string {
	line := "- " + s.Name
	if s.Namespace != "" {
		line += fmt.Sprintf(" (ns: %s)", s.Namespace)
	}
	if s.Status != "" {
		line += fmt.Sprintf(" [%s]", s.Status)
	}
	return line
}

func formatList(canonical string, items []summary) string {
	if len(items) == 0 {
		return fmt.Sprintf("No %s found.", canonical)
	}
	truncated := len(items) > maxListItems
	shown := items
	if truncated {
		shown = items[:maxListItems]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d %s:\n", len(items), canonical)
	for _, it := range shown {
		b.WriteString(it.oneLine())
		b.WriteString("\n")
	}
	if truncated {
		fmt.Fprintf(&b, "... and %d more (truncated)\n", len(items)-maxListItems)
	}
	return b.String()
}

// runK8sGet implements the k8s_get MCP tool: Get when name is set, List
// otherwise.
func runK8sGet(ctx context.Context, client kubernetes.Interface, args map[string]any) (string, error) {
	kind, _ := args["kind"].(string)
	namespace, _ := args["namespace"].(string)
	name, _ := args["name"].(string)

	if kind == "" {
		return "", errors.New("kind is required")
	}
	canonical, ok := canonicalKind(kind)
	if !ok {
		return "", fmt.Errorf("unknown or unsupported resource kind %q", kind)
	}
	if canonical == "secrets" {
		return "", errSecretsForbidden
	}
	if client == nil {
		return "", errNoClient
	}

	if name != "" {
		s, err := fetchOne(ctx, client, canonical, namespace, name)
		if err != nil {
			return "", fmt.Errorf("get %s/%s: %w", canonical, name, err)
		}
		return s.format(), nil
	}

	items, err := fetchList(ctx, client, canonical, namespace)
	if err != nil {
		return "", fmt.Errorf("list %s: %w", canonical, err)
	}
	return formatList(canonical, items), nil
}

// runK8sDescribe implements the k8s_describe MCP tool: Get plus related
// Events, formatted similarly to `kubectl describe`.
func runK8sDescribe(ctx context.Context, client kubernetes.Interface, args map[string]any) (string, error) {
	kind, _ := args["kind"].(string)
	namespace, _ := args["namespace"].(string)
	name, _ := args["name"].(string)

	if kind == "" || name == "" {
		return "", errors.New("kind and name are required")
	}
	canonical, ok := canonicalKind(kind)
	if !ok {
		return "", fmt.Errorf("unknown or unsupported resource kind %q", kind)
	}
	if canonical == "secrets" {
		return "", errSecretsForbidden
	}
	if client == nil {
		return "", errNoClient
	}
	if namespace == "" && !clusterScopedKinds[canonical] {
		return "", fmt.Errorf("namespace is required to describe %s", canonical)
	}

	s, err := fetchOne(ctx, client, canonical, namespace, name)
	if err != nil {
		return "", fmt.Errorf("get %s/%s: %w", canonical, name, err)
	}

	var b strings.Builder
	b.WriteString(s.format())

	events, err := relatedEvents(ctx, client, namespace, canonicalToKubeKind[canonical], name)
	if err != nil {
		// Events are supplementary; don't fail the whole describe if the
		// events list call errors.
		fmt.Fprintf(&b, "\nEvents:    <error fetching events: %v>\n", err)
		return b.String(), nil
	}
	b.WriteString("\nEvents:\n")
	if len(events) == 0 {
		b.WriteString("  <none>\n")
	} else {
		for _, e := range events {
			fmt.Fprintf(&b, "  %s\t%s\t%s\n", e.Type, e.Reason, e.Message)
		}
	}
	return b.String(), nil
}

// relatedEvents lists Events involving the given object. It sets a field
// selector (an optimization real API servers honor) and additionally
// filters client-side, since the fake clientset used in tests does not
// implement field-selector filtering — this keeps behavior correct either
// way.
func relatedEvents(ctx context.Context, client kubernetes.Interface, namespace, kubeKind, name string) ([]corev1.Event, error) {
	eventsClient := client.CoreV1().Events(namespace)
	if namespace == "" {
		eventsClient = client.CoreV1().Events(metav1.NamespaceAll)
	}
	list, err := eventsClient.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}

	var out []corev1.Event
	for _, e := range list.Items {
		if e.InvolvedObject.Name != name {
			continue
		}
		if kubeKind != "" && e.InvolvedObject.Kind != kubeKind {
			continue
		}
		if namespace != "" && e.InvolvedObject.Namespace != namespace {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastTimestamp.Before(&out[j].LastTimestamp)
	})
	return out, nil
}

// fetchOne fetches a single object of the given canonical kind and
// summarizes it.
func fetchOne(ctx context.Context, client kubernetes.Interface, canonical, namespace, name string) (summary, error) {
	switch canonical {
	case "pods":
		o, err := client.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return summary{}, err
		}
		return summarizePod(o), nil
	case "deployments":
		o, err := client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return summary{}, err
		}
		return summarizeDeployment(o), nil
	case "replicasets":
		o, err := client.AppsV1().ReplicaSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return summary{}, err
		}
		return summarizeReplicaSet(o), nil
	case "statefulsets":
		o, err := client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return summary{}, err
		}
		return summarizeStatefulSet(o), nil
	case "daemonsets":
		o, err := client.AppsV1().DaemonSets(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return summary{}, err
		}
		return summarizeDaemonSet(o), nil
	case "services":
		o, err := client.CoreV1().Services(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return summary{}, err
		}
		return summarizeService(o), nil
	case "endpoints":
		o, err := client.CoreV1().Endpoints(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return summary{}, err
		}
		return summarizeEndpoints(o), nil
	case "configmaps":
		o, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return summary{}, err
		}
		return summarizeConfigMap(o), nil
	case "namespaces":
		o, err := client.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return summary{}, err
		}
		return summarizeNamespace(o), nil
	case "nodes":
		o, err := client.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return summary{}, err
		}
		return summarizeNode(o), nil
	case "events":
		o, err := client.CoreV1().Events(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return summary{}, err
		}
		return summarizeEvent(o), nil
	case "jobs":
		o, err := client.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return summary{}, err
		}
		return summarizeJob(o), nil
	case "cronjobs":
		o, err := client.BatchV1().CronJobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return summary{}, err
		}
		return summarizeCronJob(o), nil
	case "ingresses":
		o, err := client.NetworkingV1().Ingresses(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return summary{}, err
		}
		return summarizeIngress(o), nil
	case "persistentvolumeclaims":
		o, err := client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return summary{}, err
		}
		return summarizePVC(o), nil
	default:
		return summary{}, fmt.Errorf("unsupported kind: %s", canonical)
	}
}

// fetchList lists objects of the given canonical kind and summarizes each.
// namespace == "" lists across all namespaces for namespaced kinds.
func fetchList(ctx context.Context, client kubernetes.Interface, canonical, namespace string) ([]summary, error) {
	var out []summary
	switch canonical {
	case "pods":
		l, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, summarizePod(&l.Items[i]))
		}
	case "deployments":
		l, err := client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, summarizeDeployment(&l.Items[i]))
		}
	case "replicasets":
		l, err := client.AppsV1().ReplicaSets(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, summarizeReplicaSet(&l.Items[i]))
		}
	case "statefulsets":
		l, err := client.AppsV1().StatefulSets(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, summarizeStatefulSet(&l.Items[i]))
		}
	case "daemonsets":
		l, err := client.AppsV1().DaemonSets(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, summarizeDaemonSet(&l.Items[i]))
		}
	case "services":
		l, err := client.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, summarizeService(&l.Items[i]))
		}
	case "endpoints":
		l, err := client.CoreV1().Endpoints(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, summarizeEndpoints(&l.Items[i]))
		}
	case "configmaps":
		l, err := client.CoreV1().ConfigMaps(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, summarizeConfigMap(&l.Items[i]))
		}
	case "namespaces":
		l, err := client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, summarizeNamespace(&l.Items[i]))
		}
	case "nodes":
		l, err := client.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, summarizeNode(&l.Items[i]))
		}
	case "events":
		l, err := client.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, summarizeEvent(&l.Items[i]))
		}
	case "jobs":
		l, err := client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, summarizeJob(&l.Items[i]))
		}
	case "cronjobs":
		l, err := client.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, summarizeCronJob(&l.Items[i]))
		}
	case "ingresses":
		l, err := client.NetworkingV1().Ingresses(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, summarizeIngress(&l.Items[i]))
		}
	case "persistentvolumeclaims":
		l, err := client.CoreV1().PersistentVolumeClaims(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		for i := range l.Items {
			out = append(out, summarizePVC(&l.Items[i]))
		}
	default:
		return nil, fmt.Errorf("unsupported kind: %s", canonical)
	}
	return out, nil
}

// --- summarize* functions: typed object -> concise summary. Deliberately
// avoid dumping full spec/status (no managedFields, no raw object) since
// this feeds an LLM context window. ---

func summarizePod(p *corev1.Pod) summary {
	ready := 0
	total := len(p.Status.ContainerStatuses)
	restarts := int32(0)
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Ready {
			ready++
		}
		restarts += cs.RestartCount
	}
	extra := fmt.Sprintf("Ready:     %d/%d\nRestarts:  %d\nNode:      %s\nPodIP:     %s\n",
		ready, total, restarts, p.Spec.NodeName, p.Status.PodIP)
	return summary{Kind: "Pod", Name: p.Name, Namespace: p.Namespace, Status: string(p.Status.Phase), Extra: extra}
}

func summarizeDeployment(d *appsv1.Deployment) summary {
	status := fmt.Sprintf("%d/%d ready", d.Status.ReadyReplicas, d.Status.Replicas)
	extra := fmt.Sprintf("Replicas:  desired=%d updated=%d available=%d\n",
		derefInt32(d.Spec.Replicas), d.Status.UpdatedReplicas, d.Status.AvailableReplicas)
	return summary{Kind: "Deployment", Name: d.Name, Namespace: d.Namespace, Status: status, Extra: extra}
}

func summarizeReplicaSet(r *appsv1.ReplicaSet) summary {
	status := fmt.Sprintf("%d/%d ready", r.Status.ReadyReplicas, r.Status.Replicas)
	return summary{Kind: "ReplicaSet", Name: r.Name, Namespace: r.Namespace, Status: status}
}

func summarizeStatefulSet(s *appsv1.StatefulSet) summary {
	status := fmt.Sprintf("%d/%d ready", s.Status.ReadyReplicas, s.Status.Replicas)
	return summary{Kind: "StatefulSet", Name: s.Name, Namespace: s.Namespace, Status: status}
}

func summarizeDaemonSet(d *appsv1.DaemonSet) summary {
	status := fmt.Sprintf("%d/%d ready", d.Status.NumberReady, d.Status.DesiredNumberScheduled)
	return summary{Kind: "DaemonSet", Name: d.Name, Namespace: d.Namespace, Status: status}
}

func summarizeService(s *corev1.Service) summary {
	extra := fmt.Sprintf("ClusterIP: %s\nType:      %s\n", s.Spec.ClusterIP, s.Spec.Type)
	return summary{Kind: "Service", Name: s.Name, Namespace: s.Namespace, Extra: extra}
}

func summarizeEndpoints(e *corev1.Endpoints) summary {
	addrs := 0
	for _, subset := range e.Subsets {
		addrs += len(subset.Addresses)
	}
	return summary{Kind: "Endpoints", Name: e.Name, Namespace: e.Namespace, Status: fmt.Sprintf("%d addresses", addrs)}
}

func summarizeConfigMap(c *corev1.ConfigMap) summary {
	return summary{Kind: "ConfigMap", Name: c.Name, Namespace: c.Namespace, Status: fmt.Sprintf("%d keys", len(c.Data))}
}

func summarizeNamespace(n *corev1.Namespace) summary {
	return summary{Kind: "Namespace", Name: n.Name, Status: string(n.Status.Phase)}
}

func summarizeNode(n *corev1.Node) summary {
	status := "Unknown"
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			if c.Status == corev1.ConditionTrue {
				status = "Ready"
			} else {
				status = "NotReady"
			}
		}
	}
	return summary{Kind: "Node", Name: n.Name, Status: status}
}

func summarizeEvent(e *corev1.Event) summary {
	status := fmt.Sprintf("%s: %s", e.Reason, e.Message)
	return summary{Kind: "Event", Name: e.Name, Namespace: e.Namespace, Status: status}
}

func summarizeJob(j *batchv1.Job) summary {
	status := fmt.Sprintf("active=%d succeeded=%d failed=%d", j.Status.Active, j.Status.Succeeded, j.Status.Failed)
	return summary{Kind: "Job", Name: j.Name, Namespace: j.Namespace, Status: status}
}

func summarizeCronJob(c *batchv1.CronJob) summary {
	extra := fmt.Sprintf("Schedule:  %s\nSuspended: %v\n", c.Spec.Schedule, derefBool(c.Spec.Suspend))
	return summary{Kind: "CronJob", Name: c.Name, Namespace: c.Namespace, Extra: extra}
}

func summarizeIngress(i *networkingv1.Ingress) summary {
	hosts := make([]string, 0, len(i.Spec.Rules))
	for _, r := range i.Spec.Rules {
		if r.Host != "" {
			hosts = append(hosts, r.Host)
		}
	}
	return summary{Kind: "Ingress", Name: i.Name, Namespace: i.Namespace, Status: strings.Join(hosts, ",")}
}

func summarizePVC(p *corev1.PersistentVolumeClaim) summary {
	extra := fmt.Sprintf("Capacity:  %s\n", p.Status.Capacity.Storage())
	return summary{Kind: "PersistentVolumeClaim", Name: p.Name, Namespace: p.Namespace, Status: string(p.Status.Phase), Extra: extra}
}

func derefInt32(p *int32) int32 {
	if p == nil {
		return 0
	}
	return *p
}

func derefBool(p *bool) bool {
	if p == nil {
		return false
	}
	return *p
}

// newInClusterClient returns a real Kubernetes clientset when running
// in-cluster (the production case: the pod's ServiceAccount token and
// service DNS are used automatically). It returns nil, not an error, when
// no in-cluster config is found — e.g. `go build`/`go test` on a laptop —
// so the server still starts and only k8s_get/k8s_describe calls fail
// (with errNoClient) until deployed. Local development against a real
// cluster would need kubeconfig-file loading (clientcmd), which is
// deliberately not added here to keep this dependency-light; tests use the
// fake clientset instead (see NewMuxWithClient).
func newInClusterClient() kubernetes.Interface {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil
	}
	return client
}
