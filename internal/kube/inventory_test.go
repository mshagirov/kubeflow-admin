package kube

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ktesting "k8s.io/client-go/testing"
	"kubeflow-admin/internal/model"
)

func TestInventoryBulkObservationDoesNotGrowRequestsPerNotebook(t *testing.T) {
	ctx := context.Background()
	s, api, client := testService(t)
	n := api.get("notebooks", "alice", "research")
	installWorkload(t, client, n, true)
	for i := range 100 {
		n := n.DeepCopy()
		n.SetName(fmt.Sprintf("stopped-%d", i))
		n.SetUID(types.UID(fmt.Sprintf("nb-%d", i)))
		n.SetAnnotations(map[string]string{stoppedKey: "2026-01-01T00:00:00Z"})
		api.put("notebooks", n)
		_, err := client.AppsV1().StatefulSets("alice").Create(ctx, &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: n.GetName(), Namespace: "alice", UID: types.UID(fmt.Sprintf("stopped-sts-%d", i)), OwnerReferences: owner(n.GetUID(), "Notebook", n.GetName())},
			Spec:       appsv1.StatefulSetSpec{Replicas: i32(0)},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
	}
	client.ClearActions()
	inv, err := s.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]int{}
	for _, r := range inv.Resources {
		if r.Kind == "Notebook" {
			states[r.State]++
		}
	}
	if states["Stopped"] != 100 || states["Running / Ready"] != 1 {
		t.Fatalf("incorrect lifecycle states: %v", states)
	}
	counts := map[string]int{}
	for _, action := range client.Actions() {
		if action.GetVerb() != "list" || action.GetNamespace() != "" {
			t.Fatalf("inventory made a non-bulk request: %v", action)
		}
		counts[action.GetResource().Resource]++
	}
	if counts["pods"] != 1 || counts["statefulsets"] != 1 {
		t.Fatalf("expected exactly one bulk list each, got %v", counts)
	}
	// A failed bulk read must not produce an apparently successful partial view.
	client.PrependReactor("list", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("unavailable")
	})
	if _, err := s.Inventory(ctx); err == nil {
		t.Fatal("failed bulk read was hidden")
	}
}

func TestInventoryPodProjectionIncludesOwnerReadinessAndDeclaredResources(t *testing.T) {
	ctx := context.Background()
	s, api, client := testService(t)
	n := api.get("notebooks", "alice", "research")
	installWorkload(t, client, n, true)
	inv, err := s.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var pod *model.Resource
	for i := range inv.Resources {
		if inv.Resources[i].Kind == "Pod" && inv.Resources[i].Name == "research-0" {
			pod = &inv.Resources[i]
		}
	}
	if pod == nil {
		t.Fatal("Pod missing from inventory")
	}
	if pod.Owner != "StatefulSet/research" || pod.Details["owner"] != pod.Owner {
		t.Fatalf("owner projection missing: %+v", pod)
	}
	if pod.Details["ready"] != "true" || pod.Details["containers"] != "2/2 ready" {
		t.Fatalf("readiness projection missing: %+v", pod.Details)
	}
	if pod.Details["restarts"] != "0" || pod.Details["restarts.research"] != "0" {
		t.Fatalf("restart projection missing: %+v", pod.Details)
	}
	for key, want := range map[string]string{
		"requested.cpu":    "1100m",
		"requested.memory": "1Gi",
		"limit.cpu":        "2",
		"limit.memory":     "2Gi",
	} {
		if pod.Details[key] != want {
			t.Fatalf("%s: got %q, want %q", key, pod.Details[key], want)
		}
	}
	if pod.Details["liveUsage"] != "not available" {
		t.Fatalf("fabricated live usage: %+v", pod.Details)
	}
	// Inventory must not project pod env literals or secret names as a side
	// effect of adding restart-related summaries.
	for key, value := range pod.Details {
		if strings.Contains(key, "SENSITIVE") || strings.Contains(value, "DO_NOT_PERSIST_LITERAL") || strings.Contains(value, "private-pull") {
			t.Fatalf("sensitive projection: %s=%s", key, value)
		}
	}
}

func TestInventoryNamespaceAndNodeWideDetails(t *testing.T) {
	ctx := context.Background()
	s, _, k := testService(t)
	created := metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "namespace-team", CreationTimestamp: created}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}}
	if _, err := k.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	worker := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-1", UID: "node-worker-1", CreationTimestamp: created,
			Labels: map[string]string{
				"node-role.kubernetes.io/worker": "",
				"node-role.kubernetes.io/gpu":    "true",
				"kubernetes.io/role":             "edge",
			},
		},
		Spec: corev1.NodeSpec{Unschedulable: true},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			NodeInfo: corev1.NodeSystemInfo{
				KubeletVersion:          "v1.31.0",
				OSImage:                 "Ubuntu 24.04",
				KernelVersion:           "6.8.0-45-generic",
				ContainerRuntimeVersion: "containerd://2.0.0",
			},
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.5"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.5"},
			},
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")},
		},
	}
	controlPlane := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-1", UID: "node-cp-1", Labels: map[string]string{"node-role.kubernetes.io/control-plane": ""}},
		Spec:       corev1.NodeSpec{Unschedulable: true},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionFalse}},
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.9"}},
		},
	}
	bare := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "bare-1", UID: "node-bare-1"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
	for _, n := range []*corev1.Node{worker, controlPlane, bare} {
		if _, err := k.CoreV1().Nodes().Create(ctx, n, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	inv, err := s.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	resources := map[string]model.Resource{}
	for _, r := range inv.Resources {
		resources[r.Kind+"/"+r.Name] = r
	}
	team, ok := resources["Namespace/team"]
	if !ok {
		t.Fatal("Namespace missing from inventory")
	}
	if team.Details["createdAt"] != created.UTC().Format(time.RFC3339) {
		t.Fatalf("Namespace createdAt: %+v", team.Details)
	}
	node, ok := resources["Node/worker-1"]
	if !ok {
		t.Fatal("Node missing from inventory")
	}
	if node.State != "Ready,SchedulingDisabled" {
		t.Fatalf("Node state: %q", node.State)
	}
	for key, want := range map[string]string{
		"roles":                      "edge,gpu,worker",
		"createdAt":                  created.UTC().Format(time.RFC3339),
		"version":                    "v1.31.0",
		"internalIP":                 "10.0.0.5",
		"externalIP":                 "203.0.113.5",
		"osImage":                    "Ubuntu 24.04",
		"kernelVersion":              "6.8.0-45-generic",
		"containerRuntime":           "containerd://2.0.0",
		"unschedulable":              "true",
		"controlPlane":               "false",
		"allocatable.nvidia.com/gpu": "4",
	} {
		if node.Details[key] != want {
			t.Fatalf("Node detail %s: got %q, want %q", key, node.Details[key], want)
		}
	}
	cp, ok := resources["Node/cp-1"]
	if !ok {
		t.Fatal("control-plane Node missing from inventory")
	}
	if cp.State != "NotReady,SchedulingDisabled" || cp.Details["roles"] != "control-plane" || cp.Details["controlPlane"] != "true" {
		t.Fatalf("control-plane projection: %+v", cp)
	}
	if cp.Details["externalIP"] != "<none>" || cp.Details["unschedulable"] != "true" {
		t.Fatalf("control-plane address/unschedulable projection: %+v", cp.Details)
	}
	bareNode, ok := resources["Node/bare-1"]
	if !ok {
		t.Fatal("address-less Node missing from inventory")
	}
	if bareNode.Details["internalIP"] != "<none>" || bareNode.Details["externalIP"] != "<none>" || bareNode.Details["roles"] != "<none>" {
		t.Fatalf("address-less Node projection: %+v", bareNode.Details)
	}
}

func TestBulkNotebookChildrenRequireNamespaceAndControllerUID(t *testing.T) {
	n := fixture(t)
	sets := []appsv1.StatefulSet{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "alice", UID: "owned", OwnerReferences: owner(n.GetUID(), "Notebook", n.GetName())}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "other", UID: "wrong-ns", OwnerReferences: owner(n.GetUID(), "Notebook", n.GetName())}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "alice", UID: "foreign", OwnerReferences: owner("foreign-nb", "Notebook", n.GetName())}},
	}
	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Namespace: "alice", Name: "child", OwnerReferences: owner("owned", "StatefulSet", "work")}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "alice", Name: "direct", OwnerReferences: owner(n.GetUID(), "Notebook", n.GetName())}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "other", Name: "wrong-ns", OwnerReferences: owner("owned", "StatefulSet", "work")}},
		{ObjectMeta: metav1.ObjectMeta{Namespace: "alice", Name: "foreign", OwnerReferences: owner("foreign", "StatefulSet", "work")}},
	}
	owned, children := notebookChildrenFrom(n, sets, pods)
	if len(owned) != 1 || owned[0].UID != "owned" || len(children) != 2 {
		t.Fatalf("incorrect ownership: %v %v", owned, children)
	}
}
