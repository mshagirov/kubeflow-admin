package kube

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"kubeflow-admin/internal/model"
)

var (
	nodesGVR = corev1.SchemeGroupVersion.WithResource("nodes")
	nodesGR  = schema.GroupResource{Resource: "nodes"}
)

type nodeCordonFixture struct {
	service    *Service
	client     *fake.Clientset
	dryRuns    int
	dryPatches [][]byte
	patches    [][]byte
	patchErr   error
}

func newNodeCordonFixture(t *testing.T) *nodeCordonFixture {
	t.Helper()
	s, _, k := testService(t)
	s.Config.Features.NodeCordon = true
	f := &nodeCordonFixture{service: s, client: k}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-1", UID: "node-1", ResourceVersion: "10",
			Labels: map[string]string{"node-role.kubernetes.io/worker": ""},
		},
		Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
	if err := k.Tracker().Create(nodesGVR, node, ""); err != nil {
		t.Fatal(err)
	}
	k.PrependReactor("patch", "nodes", func(action ktesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(ktesting.PatchActionImpl)
		if !ok {
			return false, nil, nil
		}
		if len(pa.GetPatchOptions().DryRun) > 0 {
			f.dryRuns++
			f.dryPatches = append(f.dryPatches, append([]byte(nil), pa.GetPatch()...))
			obj, err := k.Tracker().Get(nodesGVR, "", pa.GetName())
			return true, obj, err
		}
		f.patches = append(f.patches, append([]byte(nil), pa.GetPatch()...))
		if f.patchErr != nil {
			return true, nil, f.patchErr
		}
		return false, nil, nil
	})
	return f
}

func (f *nodeCordonFixture) updateNode(t *testing.T, mutate func(*corev1.Node)) *corev1.Node {
	t.Helper()
	node, err := f.client.CoreV1().Nodes().Get(context.Background(), "worker-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mutate(node)
	node, err = f.client.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return node
}

func (f *nodeCordonFixture) node(t *testing.T) *corev1.Node {
	t.Helper()
	node, err := f.client.CoreV1().Nodes().Get(context.Background(), "worker-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return node
}

func nodeAction(kind string) model.Action {
	return model.Action{Kind: kind, Name: "worker-1", Reason: "scheduled maintenance"}
}

func TestNodeCordonPolicyDigestIncludesFeature(t *testing.T) {
	f := newNodeCordonFixture(t)
	on := f.service.newEvidence().Policy
	f.service.Config.Features.NodeCordon = false
	off := f.service.newEvidence().Policy
	if on == off {
		t.Fatal("policy digest must include Features.NodeCordon")
	}
}

func TestNodeCordonFeatureAndValidationGates(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		change func(*nodeCordonFixture, *model.Action)
		want   string
	}{
		{"feature off", "node.cordon", func(f *nodeCordonFixture, _ *model.Action) {
			f.service.Config.Features.NodeCordon = false
		}, "node cordon is disabled"},
		{"writes disabled", "node.cordon", func(f *nodeCordonFixture, _ *model.Action) {
			f.service.Config.Features.WritesEnabled = false
		}, "writes are disabled"},
		{"empty reason", "node.cordon", func(_ *nodeCordonFixture, a *model.Action) {
			a.Reason = " \t\n"
		}, "a nonempty reason is required for node cordon"},
		{"long reason", "node.cordon", func(_ *nodeCordonFixture, a *model.Action) {
			a.Reason = strings.Repeat("x", 501)
		}, "reason exceeds 500 characters"},
		{"namespace", "node.cordon", func(_ *nodeCordonFixture, a *model.Action) {
			a.Namespace = "alice"
		}, "Node namespace must be empty"},
		{"no uid", "node.cordon", func(f *nodeCordonFixture, _ *model.Action) {
			f.updateNode(t, func(n *corev1.Node) { n.UID = "" })
		}, "Node has no stable UID"},
		{"terminating", "node.cordon", func(f *nodeCordonFixture, _ *model.Action) {
			f.updateNode(t, func(n *corev1.Node) {
				now := metav1.Now()
				n.DeletionTimestamp = &now
			})
		}, "Node is terminating"},
		{"already cordoned", "node.cordon", func(f *nodeCordonFixture, _ *model.Action) {
			f.updateNode(t, func(n *corev1.Node) { n.Spec.Unschedulable = true })
		}, "Node is already cordoned"},
		{"already schedulable", "node.uncordon", func(_ *nodeCordonFixture, _ *model.Action) {},
			"Node is already schedulable"},
		{"control plane", "node.cordon", func(f *nodeCordonFixture, _ *model.Action) {
			f.updateNode(t, func(n *corev1.Node) {
				n.Labels["node-role.kubernetes.io/control-plane"] = ""
			})
		}, "control-plane Node requires force to cordon"},
		{"master", "node.cordon", func(f *nodeCordonFixture, _ *model.Action) {
			f.updateNode(t, func(n *corev1.Node) {
				n.Labels["node-role.kubernetes.io/master"] = ""
			})
		}, "control-plane Node requires force to cordon"},
		{"not ready", "node.cordon", func(f *nodeCordonFixture, _ *model.Action) {
			f.updateNode(t, func(n *corev1.Node) {
				n.Status.Conditions[0].Status = corev1.ConditionFalse
			})
		}, "NotReady Node requires force to cordon"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newNodeCordonFixture(t)
			a := nodeAction(tc.kind)
			tc.change(f, &a)
			_, err := f.service.Prepare(context.Background(), a)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			if len(f.patches) != 0 {
				t.Fatalf("gated action patched: %v", f.patches)
			}
		})
	}
}

func TestNodeCordonForceAllowsGuardedTargets(t *testing.T) {
	cases := []struct {
		name   string
		kind   string
		change func(*nodeCordonFixture)
		want   bool
	}{
		{"already cordoned", "node.cordon", func(f *nodeCordonFixture) {
			f.updateNode(t, func(n *corev1.Node) { n.Spec.Unschedulable = true })
		}, true},
		{"control plane", "node.cordon", func(f *nodeCordonFixture) {
			f.updateNode(t, func(n *corev1.Node) {
				n.Labels["node-role.kubernetes.io/control-plane"] = ""
			})
		}, true},
		{"not ready", "node.cordon", func(f *nodeCordonFixture) {
			f.updateNode(t, func(n *corev1.Node) {
				n.Status.Conditions[0].Status = corev1.ConditionUnknown
			})
		}, true},
		{"already schedulable uncordon", "node.uncordon", func(f *nodeCordonFixture) {}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newNodeCordonFixture(t)
			tc.change(f)
			a := nodeAction(tc.kind)
			a.Force = true
			p, err := f.service.Prepare(context.Background(), a)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(strings.Join(p.Warnings, " "), "Force was explicitly enabled") {
				t.Fatalf("missing force warning: %v", p.Warnings)
			}
			steps := []string{}
			if err = f.service.Execute(context.Background(), p, func(step string) error {
				steps = append(steps, step)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if wantStep := nodeCordonStep(tc.kind); len(steps) != 1 || steps[0] != wantStep {
				t.Fatalf("checkpoint sequence: %v", steps)
			}
			if got := f.node(t).Spec.Unschedulable; got != tc.want {
				t.Fatalf("unschedulable = %v, want %v", got, tc.want)
			}
			if len(f.patches) != 1 {
				t.Fatalf("expected exactly one patch, got %d", len(f.patches))
			}
		})
	}
}

func TestNodeCordonPreviewAndSummary(t *testing.T) {
	f := newNodeCordonFixture(t)
	p, err := f.service.Prepare(context.Background(), nodeAction("node.cordon"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Summary != "node.cordon worker-1" {
		t.Fatalf("summary: %q", p.Summary)
	}
	changes := map[string]model.Change{}
	for _, c := range p.Changes {
		changes[c.Field] = c
	}
	if c, ok := changes["spec.unschedulable"]; !ok || c.Before != "false" || c.After != "true" {
		t.Fatalf("missing unschedulable preview: %+v", p.Changes)
	}
	if c, ok := changes["status"]; !ok || c.Before != "Ready" || c.After != "Ready,SchedulingDisabled" {
		t.Fatalf("missing status preview: %+v", p.Changes)
	}
	if len(f.patches) != 0 || f.dryRuns != 1 {
		t.Fatalf("prepare writes: patches=%d dryRuns=%d", len(f.patches), f.dryRuns)
	}
	for _, raw := range f.dryPatches {
		var ops []map[string]any
		if err := json.Unmarshal(raw, &ops); err != nil {
			t.Fatal(err)
		}
		if len(ops) != 3 || ops[0]["path"] != "/metadata/uid" || ops[1]["path"] != "/metadata/resourceVersion" || ops[1]["value"] != "10" || ops[2]["path"] != "/spec/unschedulable" {
			t.Fatalf("dry-run must test UID and current resourceVersion: %s", raw)
		}
	}
}

func TestNodeCordonHeartbeatResourceVersionChurnStillExecutes(t *testing.T) {
	f := newNodeCordonFixture(t)
	a := nodeAction("node.cordon")
	p, err := f.service.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	// Kubelet heartbeats change resourceVersion and status timestamps every few
	// seconds while the Node remains Ready.
	f.updateNode(t, func(n *corev1.Node) {
		n.ResourceVersion = "5000"
		n.Status.Conditions[0] = corev1.NodeCondition{Type: corev1.NodeReady, Status: corev1.ConditionTrue, LastHeartbeatTime: metav1.Now(), LastTransitionTime: metav1.Now()}
		n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.0.0.55"}}
	})
	steps := []string{}
	if err = f.service.Execute(context.Background(), p, func(step string) error {
		steps = append(steps, step)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0] != "cordon-node" {
		t.Fatalf("checkpoint sequence: %v", steps)
	}
	if !f.node(t).Spec.Unschedulable {
		t.Fatal("Node was not cordoned")
	}
	if len(f.patches) != 1 {
		t.Fatalf("expected one patch, got %d", len(f.patches))
	}
	var ops []map[string]any
	if err = json.Unmarshal(f.patches[0], &ops); err != nil {
		t.Fatal(err)
	}
	if len(ops) != 3 {
		t.Fatalf("unexpected patch: %s", f.patches[0])
	}
	if ops[0]["op"] != "test" || ops[0]["path"] != "/metadata/uid" || ops[0]["value"] != "node-1" {
		t.Fatalf("missing UID test: %s", f.patches[0])
	}
	if ops[1]["op"] != "test" || ops[1]["path"] != "/metadata/resourceVersion" || ops[1]["value"] != "5000" {
		t.Fatalf("missing fresh resourceVersion test: %s", f.patches[0])
	}
	if ops[2]["op"] != "add" || ops[2]["path"] != "/spec/unschedulable" || ops[2]["value"] != true {
		t.Fatalf("wrong mutation: %s", f.patches[0])
	}
}

func TestNodeCordonBecomingNotReadyBeforeExecuteRequiresForce(t *testing.T) {
	f := newNodeCordonFixture(t)
	p, err := f.service.Prepare(context.Background(), nodeAction("node.cordon"))
	if err != nil {
		t.Fatal(err)
	}
	f.updateNode(t, func(n *corev1.Node) {
		n.ResourceVersion = "11"
		n.Status.Conditions[0].Status = corev1.ConditionFalse
	})
	err = f.service.Execute(context.Background(), p, func(string) error { return nil })
	if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("expected a pre-mutation rejection, got %v", err)
	}
	if !strings.Contains(err.Error(), "NotReady Node requires force to cordon") {
		t.Fatalf("missing readiness gate: %v", err)
	}
	if errors.Is(err, ErrConflict) {
		t.Fatalf("readiness gate must be an explicit rejection, not a conflict: %v", err)
	}
	if len(f.patches) != 0 {
		t.Fatalf("NotReady Node was patched without force: %v", f.patches)
	}
}

func TestNodeCordonRetriesHeartbeatConflictWithFreshResourceVersion(t *testing.T) {
	f := newNodeCordonFixture(t)
	p, err := f.service.Prepare(context.Background(), nodeAction("node.cordon"))
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	f.client.PrependReactor("patch", "nodes", func(action ktesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(ktesting.PatchActionImpl)
		if !ok || len(pa.GetPatchOptions().DryRun) > 0 {
			return false, nil, nil
		}
		attempts++
		if attempts > 1 {
			return false, nil, nil
		}
		obj, err := f.client.Tracker().Get(nodesGVR, "", "worker-1")
		if err != nil {
			return true, nil, err
		}
		node, ok := obj.(*corev1.Node)
		if !ok {
			return true, nil, errors.New("unexpected Node type")
		}
		node = node.DeepCopy()
		node.ResourceVersion = "11"
		if err = f.client.Tracker().Update(nodesGVR, node, ""); err != nil {
			return true, nil, err
		}
		return true, nil, apierrors.NewConflict(nodesGR, "worker-1", errors.New("heartbeat"))
	})
	if err = f.service.Execute(context.Background(), p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("expected one retry, got %d attempts", attempts)
	}
	if !f.node(t).Spec.Unschedulable {
		t.Fatal("retried cordon did not converge")
	}
	if len(f.patches) != 1 {
		t.Fatalf("expected the retry to be the only recorded mutation, got %d", len(f.patches))
	}
	var ops []map[string]any
	if err = json.Unmarshal(f.patches[0], &ops); err != nil {
		t.Fatal(err)
	}
	if ops[1]["path"] != "/metadata/resourceVersion" || ops[1]["value"] != "11" {
		t.Fatalf("retry did not use the fresh resourceVersion: %s", f.patches[0])
	}
}

func TestNodeCordonConcurrentChangeRejectsBeforePatch(t *testing.T) {
	cases := []struct {
		name         string
		change       func(*nodeCordonFixture)
		wantConflict bool
	}{
		{"uid replaced", func(f *nodeCordonFixture) {
			f.updateNode(t, func(n *corev1.Node) { n.UID = "node-2" })
		}, true},
		{"concurrent cordon", func(f *nodeCordonFixture) {
			f.updateNode(t, func(n *corev1.Node) { n.Spec.Unschedulable = true })
		}, true},
		{"terminating", func(f *nodeCordonFixture) {
			f.updateNode(t, func(n *corev1.Node) {
				now := metav1.Now()
				n.DeletionTimestamp = &now
			})
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newNodeCordonFixture(t)
			p, err := f.service.Prepare(context.Background(), nodeAction("node.cordon"))
			if err != nil {
				t.Fatal(err)
			}
			tc.change(f)
			err = f.service.Execute(context.Background(), p, func(string) error { return nil })
			if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) {
				t.Fatalf("expected a pre-mutation rejection, got %v", err)
			}
			if errors.Is(err, ErrConflict) != tc.wantConflict {
				t.Fatalf("conflict classification wrong: %v", err)
			}
			if len(f.patches) != 0 {
				t.Fatalf("stale plan was patched: %v", f.patches)
			}
		})
	}
}

func TestNodeCordonConcurrentChangeAfterCheckpointRejectsBeforePatch(t *testing.T) {
	f := newNodeCordonFixture(t)
	p, err := f.service.Prepare(context.Background(), nodeAction("node.cordon"))
	if err != nil {
		t.Fatal(err)
	}
	err = f.service.Execute(context.Background(), p, func(step string) error {
		if step != "cordon-node" {
			t.Fatalf("unexpected checkpoint %q", step)
		}
		f.updateNode(t, func(n *corev1.Node) { n.Spec.Unschedulable = true })
		return nil
	})
	if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) || !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict before mutation, got %v", err)
	}
	if len(f.patches) != 0 {
		t.Fatalf("post-checkpoint stale plan was patched: %v", f.patches)
	}
}

func TestNodeCordonPatchRejectionsAreSentinelFailures(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		sentinel bool
		conflict bool
		attempts int
		wantText string
	}{
		{"forbidden", apierrors.NewForbidden(nodesGR, "worker-1", errors.New("denied")), true, false, 1, "denied the Node patch"},
		{"conflict", apierrors.NewConflict(nodesGR, "worker-1", errors.New("stale")), true, true, nodeCordonPatchAttempts, "changed before the patch"},
		{"notfound", apierrors.NewNotFound(nodesGR, "worker-1"), true, false, 1, "not found"},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Group: "", Kind: "Node"}, "worker-1", nil), true, false, 1, "rejected the Node patch"},
		{"badrequest", apierrors.NewBadRequest("bad patch"), true, false, 1, "rejected the Node patch"},
		{"unauthorized", apierrors.NewUnauthorized("no"), true, false, 1, "rejected the Node patch"},
		{"throttled", apierrors.NewTooManyRequests("busy", 1), true, false, 1, "throttled"},
		{"network", errors.New("dial tcp 10.0.0.1:443: connection reset by peer"), false, false, 1, "outcome is unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newNodeCordonFixture(t)
			p, err := f.service.Prepare(context.Background(), nodeAction("node.cordon"))
			if err != nil {
				t.Fatal(err)
			}
			f.patchErr = tc.err
			err = f.service.Execute(context.Background(), p, func(string) error { return nil })
			if err == nil {
				t.Fatal("accepted failed patch")
			}
			if errors.Is(err, model.ErrRejectedBeforeMutation) != tc.sentinel {
				t.Fatalf("sentinel classification wrong for %v: %v", tc.err, err)
			}
			if errors.Is(err, ErrConflict) != tc.conflict {
				t.Fatalf("conflict classification wrong for %v: %v", tc.err, err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("missing safe explanation %q in %v", tc.wantText, err)
			}
			if strings.Contains(err.Error(), "10.0.0.1") || strings.Contains(err.Error(), "connection reset") {
				t.Fatalf("transport details leaked: %v", err)
			}
			if len(f.patches) != tc.attempts {
				t.Fatalf("expected %d patch attempts, got %d", tc.attempts, len(f.patches))
			}
		})
	}
}

func TestNodeCordonCheckpointFailureNeverPatches(t *testing.T) {
	for _, kind := range []string{"node.cordon", "node.uncordon"} {
		t.Run(kind, func(t *testing.T) {
			f := newNodeCordonFixture(t)
			if kind == "node.uncordon" {
				f.updateNode(t, func(n *corev1.Node) { n.Spec.Unschedulable = true })
			}
			p, err := f.service.Prepare(context.Background(), nodeAction(kind))
			if err != nil {
				t.Fatal(err)
			}
			steps := []string{}
			err = f.service.Execute(context.Background(), p, func(step string) error {
				steps = append(steps, step)
				return errors.New("disk unavailable")
			})
			if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) {
				t.Fatalf("checkpoint failure would strand the Node lock: %v", err)
			}
			if len(steps) != 1 || steps[0] != nodeCordonStep(kind) {
				t.Fatalf("checkpoint sequence: %v", steps)
			}
			if len(f.patches) != 0 {
				t.Fatalf("patch preceded the durable checkpoint: %v", f.patches)
			}
		})
	}
}

func TestNodeCordonAndUncordonConverge(t *testing.T) {
	t.Run("cordon", func(t *testing.T) {
		f := newNodeCordonFixture(t)
		p, err := f.service.Prepare(context.Background(), nodeAction("node.cordon"))
		if err != nil {
			t.Fatal(err)
		}
		if err = f.service.Execute(context.Background(), p, func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if !f.node(t).Spec.Unschedulable {
			t.Fatal("cordon did not converge")
		}
	})
	t.Run("uncordon", func(t *testing.T) {
		f := newNodeCordonFixture(t)
		f.updateNode(t, func(n *corev1.Node) { n.Spec.Unschedulable = true })
		p, err := f.service.Prepare(context.Background(), nodeAction("node.uncordon"))
		if err != nil {
			t.Fatal(err)
		}
		changes := map[string]model.Change{}
		for _, c := range p.Changes {
			changes[c.Field] = c
		}
		if c, ok := changes["spec.unschedulable"]; !ok || c.Before != "true" || c.After != "false" {
			t.Fatalf("missing unschedulable preview: %+v", p.Changes)
		}
		if c, ok := changes["status"]; !ok || c.Before != "Ready,SchedulingDisabled" || c.After != "Ready" {
			t.Fatalf("missing status preview: %+v", p.Changes)
		}
		if err = f.service.Execute(context.Background(), p, func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if f.node(t).Spec.Unschedulable {
			t.Fatal("uncordon did not converge")
		}
	})
}

func TestNodeCordonConvergenceTimeoutIsNotRejectedBeforeMutation(t *testing.T) {
	f := newNodeCordonFixture(t)
	p, err := f.service.Prepare(context.Background(), nodeAction("node.cordon"))
	if err != nil {
		t.Fatal(err)
	}
	// Revert the mutation after the API accepts it: Execute must keep waiting and
	// must not report the post-mutation timeout as a pre-mutation rejection.
	f.client.PrependReactor("patch", "nodes", func(action ktesting.Action) (bool, runtime.Object, error) {
		pa, ok := action.(ktesting.PatchActionImpl)
		if !ok || len(pa.GetPatchOptions().DryRun) > 0 {
			return false, nil, nil
		}
		obj, err := f.client.Tracker().Get(nodesGVR, "", "worker-1")
		if err != nil {
			return true, nil, err
		}
		node, ok := obj.(*corev1.Node)
		if !ok {
			return true, nil, errors.New("unexpected Node type")
		}
		node = node.DeepCopy()
		node.Spec.Unschedulable = true
		if err = f.client.Tracker().Update(nodesGVR, node, ""); err != nil {
			return true, nil, err
		}
		node.Spec.Unschedulable = false
		if err = f.client.Tracker().Update(nodesGVR, node, ""); err != nil {
			return true, nil, err
		}
		return true, node, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err = f.service.Execute(ctx, p, func(string) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected convergence timeout, got %v", err)
	}
	if errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatal("post-mutation timeout must keep the Node lock")
	}
}
