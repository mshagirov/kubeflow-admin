package kube

import (
	"context"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
