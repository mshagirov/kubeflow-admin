package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	"kubeflow-admin/internal/model"
)

var (
	podsGVR      = corev1.SchemeGroupVersion.WithResource("pods")
	podsEviction = schema.GroupResource{Group: "", Resource: "pods"}
)

type restartFixture struct {
	service     *Service
	api         *crAPI
	client      *fake.Clientset
	notebook    *unstructured.Unstructured
	set         *appsv1.StatefulSet
	pod         *corev1.Pod
	node        *corev1.Node
	evictions   []policyv1.Eviction
	evictErr    error
	replacement func() *corev1.Pod
	afterEvict  func()
}

func restartPodFromNotebook(t *testing.T, n *unstructured.Unstructured) corev1.PodSpec {
	t.Helper()
	spec, _, _ := unstructured.NestedMap(n.Object, "spec", "template", "spec")
	var pod corev1.PodSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec, &pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

func newRestartFixture(t *testing.T) *restartFixture {
	t.Helper()
	s, api, k := testService(t)
	s.Config.Features.PodRestarts = true
	n := api.get("notebooks", "alice", "research")
	spec := restartPodFromNotebook(t, n)
	spec.NodeName = "node-1"

	revision := "revision-1"
	replicas := int32(1)
	st := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "research", Namespace: "alice", UID: "sts-1", ResourceVersion: "1", Generation: 1, OwnerReferences: owner(n.GetUID(), "Notebook", n.GetName())},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{Spec: spec}},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 1, ReadyReplicas: 1, CurrentReplicas: 1, UpdatedReplicas: 1, CurrentRevision: revision, UpdateRevision: revision},
	}
	if _, err := k.AppsV1().StatefulSets("alice").Create(context.Background(), st, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	statuses := []corev1.ContainerStatus{}
	for _, c := range spec.Containers {
		statuses = append(statuses, corev1.ContainerStatus{Name: c.Name, Ready: true, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}})
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "research-0", Namespace: "alice", UID: "pod-1", ResourceVersion: "10",
			Labels:          map[string]string{appsv1.StatefulSetRevisionLabel: revision},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: st.Name, UID: st.UID, Controller: boolp(true)}},
		},
		Spec:   spec,
		Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: statuses},
	}
	if _, err := k.CoreV1().Pods("alice").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "node-1", UID: "node-1"},
		Status:     corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}},
	}
	if _, err := k.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	f := &restartFixture{service: s, api: api, client: k, notebook: n, set: st, pod: pod, node: node}
	f.replacement = func() *corev1.Pod {
		next := pod.DeepCopy()
		next.UID = "pod-2"
		next.ResourceVersion = "11"
		return next
	}
	k.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(ktesting.CreateActionImpl)
		if !ok || ca.Subresource != "eviction" {
			return false, nil, nil
		}
		eviction, ok := ca.GetObject().(*policyv1.Eviction)
		if !ok {
			return true, nil, fmt.Errorf("unexpected eviction object")
		}
		f.evictions = append(f.evictions, *eviction.DeepCopy())
		if f.evictErr != nil {
			return true, nil, f.evictErr
		}
		if err := k.Tracker().Delete(podsGVR, eviction.Namespace, eviction.Name); err != nil {
			return true, nil, err
		}
		if f.replacement != nil {
			if err := k.Tracker().Create(podsGVR, f.replacement(), eviction.Namespace); err != nil {
				return true, nil, err
			}
		}
		if f.afterEvict != nil {
			f.afterEvict()
		}
		return true, &metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusSuccess}, nil
	})
	return f
}

func restartAction() model.Action {
	return model.Action{Kind: "pod.restart", Namespace: "alice", Name: "research-0", Reason: "replace an unhealthy process"}
}

func (f *restartFixture) updatePod(t *testing.T, mutate func(*corev1.Pod)) *corev1.Pod {
	t.Helper()
	p, err := f.client.CoreV1().Pods("alice").Get(context.Background(), "research-0", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mutate(p)
	p, err = f.client.CoreV1().Pods("alice").Update(context.Background(), p, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *restartFixture) updateSet(t *testing.T, mutate func(*appsv1.StatefulSet)) {
	t.Helper()
	st, err := f.client.AppsV1().StatefulSets("alice").Get(context.Background(), "research", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mutate(st)
	if _, err = f.client.AppsV1().StatefulSets("alice").Update(context.Background(), st, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func (f *restartFixture) updateNode(t *testing.T, mutate func(*corev1.Node)) {
	t.Helper()
	node, err := f.client.CoreV1().Nodes().Get(context.Background(), "node-1", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mutate(node)
	if _, err = f.client.CoreV1().Nodes().Update(context.Background(), node, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestPodRestartPrepareNeverEvictsAndExposesParentLock(t *testing.T) {
	f := newRestartFixture(t)
	p, err := f.service.Prepare(context.Background(), restartAction())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.evictions) != 0 {
		t.Fatal("Prepare must never evict")
	}
	if p.TargetUID != "pod-1" || p.ParentNotebook != "research" {
		t.Fatalf("wrong server-owned target: %+v", p)
	}
	if len(p.Changes) == 0 || !strings.Contains(fmt.Sprint(p.Changes), "StatefulSet/research") {
		t.Fatalf("missing controller preview: %+v", p.Changes)
	}
	if !strings.Contains(strings.Join(p.Warnings, " "), "PodDisruptionBudget") {
		t.Fatal("missing disruption warning")
	}
	// Env literals must not be persisted as evidence, even indirectly as text.
	for _, sensitive := range []string{"DO_NOT_PERSIST_LITERAL", "SENSITIVE_FIXTURE", "private-pull"} {
		if strings.Contains(string(p.Data), sensitive) {
			t.Fatalf("unsafe restart evidence: %s", sensitive)
		}
	}
}

func TestPodRestartPolicyDigestIncludesFeature(t *testing.T) {
	f := newRestartFixture(t)
	on := f.service.newEvidence().Policy
	f.service.Config.Features.PodRestarts = false
	off := f.service.newEvidence().Policy
	if on == off {
		t.Fatal("policy digest must include Features.PodRestarts")
	}
}

func TestPodRestartFeatureAndReasonGates(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*restartFixture, *model.Action)
		want   string
	}{
		{"feature off", func(f *restartFixture, _ *model.Action) { f.service.Config.Features.PodRestarts = false }, "pod restarts are disabled"},
		{"controller contract", func(f *restartFixture, _ *model.Action) {
			f.service.Config.Notebooks.ControllerContractConfirmed = false
		}, "confirmed installed Notebook controller contract"},
		{"missing reason", func(_ *restartFixture, a *model.Action) { a.Reason = " \t" }, "nonempty reason"},
		{"long reason", func(_ *restartFixture, a *model.Action) { a.Reason = strings.Repeat("x", 501) }, "reason exceeds 500 characters"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestartFixture(t)
			a := restartAction()
			tc.change(f, &a)
			if _, err := f.service.Prepare(context.Background(), a); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			if len(f.evictions) != 0 {
				t.Fatal("gated action evicted")
			}
		})
	}
}

func TestPodRestartRefusesUnsupportedTargets(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*restartFixture)
		want   string
	}{
		{"unmanaged", func(f *restartFixture) {
			f.updatePod(t, func(p *corev1.Pod) { p.OwnerReferences = nil })
		}, "not managed by a controller"},
		{"mirror", func(f *restartFixture) {
			f.updatePod(t, func(p *corev1.Pod) {
				p.Annotations = map[string]string{corev1.MirrorPodAnnotationKey: "mirror"}
			})
		}, "mirror and static Pods"},
		{"direct notebook owner", func(f *restartFixture) {
			f.updatePod(t, func(p *corev1.Pod) {
				p.OwnerReferences = owner(f.notebook.GetUID(), "Notebook", f.notebook.GetName())
			})
		}, "only StatefulSet-managed"},
		{"wrong set owner", func(f *restartFixture) {
			f.updatePod(t, func(p *corev1.Pod) {
				p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "other", UID: "other-set", Controller: boolp(true)}}
			})
		}, "not found"},
		{"set without notebook", func(f *restartFixture) {
			f.updateSet(t, func(st *appsv1.StatefulSet) { st.OwnerReferences = nil })
		}, "not controlled by a Notebook"},
		{"terminating pod", func(f *restartFixture) {
			f.updatePod(t, func(p *corev1.Pod) {
				now := metav1.Now()
				p.DeletionTimestamp = &now
			})
		}, "Pod is terminating"},
		{"parent stopped", func(f *restartFixture) {
			n := f.notebook.DeepCopy()
			n.SetAnnotations(map[string]string{stoppedKey: "2026-01-01T00:00:00Z"})
			f.api.put("notebooks", n)
		}, "parent Notebook is stopped"},
		{"parent deleting", func(f *restartFixture) {
			n := f.notebook.DeepCopy()
			now := metav1.Now()
			n.SetDeletionTimestamp(&now)
			f.api.put("notebooks", n)
		}, "Notebook is terminating"},
		{"node not ready", func(f *restartFixture) {
			f.updateNode(t, func(n *corev1.Node) {
				n.Status.Conditions[0].Status = corev1.ConditionFalse
			})
		}, "assigned Node is not Ready"},
		{"node unreachable", func(f *restartFixture) {
			f.updateNode(t, func(n *corev1.Node) {
				n.Spec.Taints = append(n.Spec.Taints, corev1.Taint{Key: corev1.TaintNodeUnreachable, Effect: corev1.TaintEffectNoExecute})
			})
		}, "assigned Node is unreachable or not ready"},
		{"node unschedulable", func(f *restartFixture) {
			f.updateNode(t, func(n *corev1.Node) { n.Spec.Unschedulable = true })
		}, "assigned Node is unschedulable"},
		{"not running", func(f *restartFixture) {
			f.updatePod(t, func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending })
		}, "Pod is not running"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestartFixture(t)
			tc.mutate(f)
			if _, err := f.service.Prepare(context.Background(), restartAction()); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			if len(f.evictions) != 0 {
				t.Fatal("unsupported target evicted")
			}
		})
	}
}

func TestPodRestartNamespaceAndProfileScope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ns     string
		policy []string
		want   string
	}{
		{"protected", "kubeflow", nil, "protected namespace"},
		{"outside allowlist", "alice", []string{"somewhere-else"}, "namespace is outside configured scope"},
		{"no profile", "bob", nil, "matching Profile required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestartFixture(t)
			f.service.Config.Policy.AllowedNamespaces = tc.policy
			a := restartAction()
			a.Namespace = tc.ns
			if _, err := f.service.Prepare(context.Background(), a); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			if len(f.evictions) != 0 {
				t.Fatal("out-of-scope restart evicted")
			}
		})
	}
}

func TestPodRestartStatusOnlyRVIsHarmlessAndUsesFreshPreconditions(t *testing.T) {
	f := newRestartFixture(t)
	p, err := f.service.Prepare(context.Background(), restartAction())
	if err != nil {
		t.Fatal(err)
	}
	f.updatePod(t, func(p *corev1.Pod) {
		p.ResourceVersion = "11"
		p.Status.ContainerStatuses[0].RestartCount = 7
	})
	f.client.ClearActions()
	steps := []string{}
	err = f.service.Execute(context.Background(), p, func(step string) error {
		if len(f.evictions) != 0 {
			t.Fatal("eviction happened before checkpoint")
		}
		steps = append(steps, step)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0] != "evict-pod" {
		t.Fatalf("unexpected checkpoints: %v", steps)
	}
	if len(f.evictions) != 1 {
		t.Fatalf("expected one eviction, got %d", len(f.evictions))
	}
	ev := f.evictions[0]
	if ev.DeleteOptions == nil || ev.DeleteOptions.Preconditions == nil || ev.DeleteOptions.Preconditions.UID == nil || ev.DeleteOptions.Preconditions.ResourceVersion == nil {
		t.Fatalf("missing preconditions: %+v", ev.DeleteOptions)
	}
	if *ev.DeleteOptions.Preconditions.UID != "pod-1" || *ev.DeleteOptions.Preconditions.ResourceVersion != "11" {
		t.Fatalf("stale preconditions: %+v", ev.DeleteOptions.Preconditions)
	}
	if ev.DeleteOptions.GracePeriodSeconds != nil {
		t.Fatal("eviction must not force a zero grace period")
	}
	evictionActions := 0
	for _, action := range f.client.Actions() {
		if action.GetVerb() == "delete" {
			t.Fatalf("restart must never delete objects: %v", action)
		}
		if create, ok := action.(ktesting.CreateActionImpl); ok && create.Subresource == "eviction" {
			evictionActions++
		}
	}
	if evictionActions != 1 {
		t.Fatalf("expected one eviction action, got %d", evictionActions)
	}
}

func TestPodRestartUsesPreconditionsRefreshedAfterCheckpoint(t *testing.T) {
	f := newRestartFixture(t)
	p, err := f.service.Prepare(context.Background(), restartAction())
	if err != nil {
		t.Fatal(err)
	}
	err = f.service.Execute(context.Background(), p, func(step string) error {
		if step == "evict-pod" {
			f.updatePod(t, func(pod *corev1.Pod) {
				pod.ResourceVersion = "12"
				pod.Status.ContainerStatuses[0].RestartCount = 9
			})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.evictions) != 1 || f.evictions[0].DeleteOptions == nil || f.evictions[0].DeleteOptions.Preconditions == nil || f.evictions[0].DeleteOptions.Preconditions.ResourceVersion == nil {
		t.Fatalf("missing eviction: %+v", f.evictions)
	}
	if got := *f.evictions[0].DeleteOptions.Preconditions.ResourceVersion; got != "12" {
		t.Fatalf("eviction used stale pre-checkpoint resourceVersion %q", got)
	}
}

func TestPodRestartSpecChangeRejectsBeforeEviction(t *testing.T) {
	f := newRestartFixture(t)
	p, err := f.service.Prepare(context.Background(), restartAction())
	if err != nil {
		t.Fatal(err)
	}
	f.updatePod(t, func(pod *corev1.Pod) {
		pod.ResourceVersion = "11"
		pod.Spec.Containers[0].Image = "unapproved:evil"
	})
	if err = f.service.Execute(context.Background(), p, func(string) error { return nil }); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if len(f.evictions) != 0 {
		t.Fatal("stale spec was evicted")
	}
}

func TestPodRestartParentStopAfterPreviewRejectsBeforeEviction(t *testing.T) {
	f := newRestartFixture(t)
	p, err := f.service.Prepare(context.Background(), restartAction())
	if err != nil {
		t.Fatal(err)
	}
	n := f.notebook.DeepCopy()
	n.SetAnnotations(map[string]string{stoppedKey: "2026-01-01T00:00:00Z"})
	f.api.put("notebooks", n)
	if err = f.service.Execute(context.Background(), p, func(string) error { return nil }); err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("expected a pre-write rejection, got %v", err)
	}
	if len(f.evictions) != 0 {
		t.Fatal("stopped parent was evicted")
	}
}

func TestPodRestartCheckpointFailureNeverEvicts(t *testing.T) {
	f := newRestartFixture(t)
	p, err := f.service.Prepare(context.Background(), restartAction())
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.Execute(context.Background(), p, func(string) error { return errors.New("disk unavailable") }); err == nil {
		t.Fatal("checkpoint failure accepted")
	}
	if len(f.evictions) != 0 {
		t.Fatal("eviction preceded durable checkpoint")
	}
}

func TestPodRestartEvictionRejectionsAreSentinelFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		sentinel bool
		conflict bool
		wantText string
	}{
		{"pdb", apierrors.NewTooManyRequests("blocked", 0), true, false, "PodDisruptionBudget"},
		{"forbidden", apierrors.NewForbidden(podsEviction, "research-0", errors.New("denied")), true, false, "denied the eviction"},
		{"conflict", apierrors.NewConflict(podsEviction, "research-0", errors.New("stale")), true, true, "preconditions"},
		{"notfound", apierrors.NewNotFound(podsEviction, "research-0"), true, false, "not found"},
		{"network", errors.New("dial tcp 10.0.0.1:443: connection reset by peer"), false, false, "outcome is unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestartFixture(t)
			p, err := f.service.Prepare(context.Background(), restartAction())
			if err != nil {
				t.Fatal(err)
			}
			f.evictErr = tc.err
			err = f.service.Execute(context.Background(), p, func(string) error { return nil })
			if err == nil {
				t.Fatal("accepted failed eviction")
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
			if len(f.evictions) != 1 {
				t.Fatalf("expected one eviction attempt, got %d", len(f.evictions))
			}
		})
	}
}

func TestPodRestartReplayDoesNotEvictTwice(t *testing.T) {
	f := newRestartFixture(t)
	p, err := f.service.Prepare(context.Background(), restartAction())
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.Execute(context.Background(), p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	// Both a replay of the accepted plan and a re-prepare see a different UID.
	if err = f.service.Execute(context.Background(), p, func(string) error { return nil }); err == nil {
		t.Fatal("replay of a completed plan was accepted")
	}
	if len(f.evictions) != 1 {
		t.Fatalf("double eviction: %d", len(f.evictions))
	}
}

func TestPodRestartConvergenceRequiresDistinctReadyReplacement(t *testing.T) {
	// Leave enough time for race-instrumented preflight reads to reach the
	// eviction. These cases exercise post-eviction convergence, not preflight
	// cancellation on a busy test host.
	for _, tc := range []struct {
		name         string
		replacement  func(pod *corev1.Pod, st *appsv1.StatefulSet) *corev1.Pod
		timeout      time.Duration
		wantSentinel bool
		wantConflict bool
	}{
		{"original never disappears", func(pod *corev1.Pod, _ *appsv1.StatefulSet) *corev1.Pod { return nil }, time.Second, false, false},
		{"wrong owner", func(pod *corev1.Pod, _ *appsv1.StatefulSet) *corev1.Pod {
			next := pod.DeepCopy()
			next.UID = "pod-2"
			next.OwnerReferences = owner("foreign-sts", "StatefulSet", "research")
			return next
		}, time.Second, false, true},
		{"wrong revision", func(pod *corev1.Pod, _ *appsv1.StatefulSet) *corev1.Pod {
			next := pod.DeepCopy()
			next.UID = "pod-2"
			next.Labels[appsv1.StatefulSetRevisionLabel] = "old-revision"
			return next
		}, time.Second, false, false},
		{"container not ready", func(pod *corev1.Pod, _ *appsv1.StatefulSet) *corev1.Pod {
			next := pod.DeepCopy()
			next.UID = "pod-2"
			next.Status.ContainerStatuses[0].Ready = false
			return next
		}, time.Second, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRestartFixture(t)
			p, err := f.service.Prepare(context.Background(), restartAction())
			if err != nil {
				t.Fatal(err)
			}
			f.replacement = nil
			if next := tc.replacement(f.pod, f.set); next != nil {
				f.replacement = func() *corev1.Pod { return next }
			}
			ctx, cancel := context.WithTimeout(context.Background(), tc.timeout)
			defer cancel()
			err = f.service.Execute(ctx, p, func(string) error { return nil })
			if err == nil {
				t.Fatal("accepted unconverged replacement")
			}
			if errors.Is(err, model.ErrRejectedBeforeMutation) != tc.wantSentinel {
				t.Fatalf("sentinel classification wrong: %v", err)
			}
			if errors.Is(err, ErrConflict) != tc.wantConflict {
				t.Fatalf("conflict classification wrong: %v", err)
			}
			if len(f.evictions) != 1 {
				t.Fatalf("expected one eviction, got %d", len(f.evictions))
			}
		})
	}
}

func TestPodRestartCancellationAfterEvictionNeedsReview(t *testing.T) {
	f := newRestartFixture(t)
	p, err := f.service.Prepare(context.Background(), restartAction())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.replacement = nil
	f.afterEvict = cancel
	err = f.service.Execute(ctx, p, func(string) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation ambiguity, got %v", err)
	}
	if errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatal("post-eviction cancellation must not be classified as a pre-mutation rejection")
	}
	if len(f.evictions) != 1 {
		t.Fatalf("expected one eviction, got %d", len(f.evictions))
	}
}

func TestEvictPodHTTPPreconditions(t *testing.T) {
	var got policyv1.Eviction
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/namespaces/alice/pods/research-0/eviction" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"Status","status":"Success"}`))
	}))
	defer server.Close()
	client, err := kubernetes.NewForConfig(&rest.Config{Host: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "research-0", Namespace: "alice", UID: types.UID("pod-1"), ResourceVersion: "17"}}
	if err = evictPod(context.Background(), client.CoreV1().Pods("alice"), pod); err != nil {
		t.Fatal(err)
	}
	if got.APIVersion != "policy/v1" || got.Kind != "Eviction" {
		t.Fatalf("wrong eviction type: %s %s", got.APIVersion, got.Kind)
	}
	if got.DeleteOptions == nil || got.DeleteOptions.Preconditions == nil {
		t.Fatalf("missing delete preconditions: %+v", got.DeleteOptions)
	}
	if got.DeleteOptions.Preconditions.UID == nil || *got.DeleteOptions.Preconditions.UID != "pod-1" {
		t.Fatalf("wrong UID precondition: %+v", got.DeleteOptions.Preconditions)
	}
	if got.DeleteOptions.Preconditions.ResourceVersion == nil || *got.DeleteOptions.Preconditions.ResourceVersion != "17" {
		t.Fatalf("wrong resourceVersion precondition: %+v", got.DeleteOptions.Preconditions)
	}
	if got.DeleteOptions.GracePeriodSeconds != nil {
		t.Fatal("no grace-period override is allowed")
	}
}

func TestRestartReplacementReadyRequiresAllContainersAndRevision(t *testing.T) {
	replicas := int32(1)
	st := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "research", Namespace: "alice", UID: "sts-1", Generation: 1},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 1, ReadyReplicas: 1, CurrentRevision: "rev", UpdateRevision: "rev"},
	}
	p := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "research-0", Namespace: "alice", UID: "pod-2", Labels: map[string]string{appsv1.StatefulSetRevisionLabel: "rev"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "research", UID: "sts-1", Controller: boolp(true)}}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "a"}, {Name: "b"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "a", Ready: true}, {Name: "b", Ready: true}}},
	}
	ok, err := restartReplacementReady(p, st)
	if err != nil || !ok {
		t.Fatalf("ready replacement rejected: %v %v", ok, err)
	}
	p.Status.ContainerStatuses[1].Ready = false
	if ok, _ = restartReplacementReady(p, st); ok {
		t.Fatal("accepted not-ready container")
	}
	p.Status.ContainerStatuses[1].Ready = true
	p.Labels[appsv1.StatefulSetRevisionLabel] = "old"
	if ok, _ = restartReplacementReady(p, st); ok {
		t.Fatal("accepted old revision")
	}
}

func TestRestartFingerprintRedactsSecretValuesButTracksSpec(t *testing.T) {
	f := newRestartFixture(t)
	one := podRestartFingerprint(f.pod, f.set, f.notebook)
	f.updatePod(t, func(p *corev1.Pod) {
		p.Spec.Containers[0].Env[0].Value = "DIFFERENT_VALUE"
	})
	cur, _ := f.client.CoreV1().Pods("alice").Get(context.Background(), "research-0", metav1.GetOptions{})
	if one != podRestartFingerprint(cur, f.set, f.notebook) {
		t.Fatal("env value change must be redacted from the fingerprint")
	}
	f.updatePod(t, func(p *corev1.Pod) { p.Spec.Containers[0].Image = "changed:v2" })
	cur, _ = f.client.CoreV1().Pods("alice").Get(context.Background(), "research-0", metav1.GetOptions{})
	if one == podRestartFingerprint(cur, f.set, f.notebook) {
		t.Fatal("spec change did not alter the fingerprint")
	}
}
