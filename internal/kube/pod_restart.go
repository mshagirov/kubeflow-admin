package kube

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"kubeflow-admin/internal/model"
)

const podRestartReasonLimit = 500

// preparePodRestart validates a running Pod controlled by one StatefulSet,
// itself controlled by a running Notebook in the same Profile namespace.
// It never evicts or mutates anything; the live Pod's
// current UID/resourceVersion become the eviction preconditions only in Execute.
func (s *Service) preparePodRestart(ctx context.Context, a model.Action, w *work) error {
	if !s.Config.Features.PodRestarts {
		return errors.New("pod restarts are disabled")
	}
	if !s.Config.Notebooks.ControllerContractConfirmed {
		return errors.New("Pod restarts require a confirmed installed Notebook controller contract")
	}
	if strings.TrimSpace(a.Reason) == "" {
		return errors.New("a nonempty reason is required for pod restart")
	}
	if len(a.Reason) > podRestartReasonLimit {
		return errors.New("reason exceeds 500 characters")
	}
	pod, err := s.Typed.CoreV1().Pods(a.Namespace).Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		return safe(err)
	}
	if pod.UID == "" {
		return errors.New("Pod has no stable UID")
	}
	if pod.DeletionTimestamp != nil {
		return errors.New("Pod is terminating")
	}
	if isMirrorOrStaticPod(pod) {
		return errors.New("mirror and static Pods cannot be restarted")
	}
	if pod.Status.Phase != corev1.PodRunning {
		return errors.New("Pod is not running")
	}
	controller, err := singleControllerOwner(pod.OwnerReferences)
	if err != nil {
		return err
	}
	if controller.Kind != "StatefulSet" || controller.APIVersion != appsv1.SchemeGroupVersion.String() {
		return errors.New("only StatefulSet-managed Notebook Pods are supported")
	}
	if controller.UID == "" {
		return errors.New("Pod owner has no stable UID")
	}
	set, err := s.Typed.AppsV1().StatefulSets(a.Namespace).Get(ctx, controller.Name, metav1.GetOptions{})
	if err != nil {
		return safe(err)
	}
	if set.UID != controller.UID {
		return errors.New("Pod owner chain changed; prepare a new plan")
	}
	if set.DeletionTimestamp != nil {
		return errors.New("StatefulSet is terminating")
	}
	// A zero replica StatefulSet may have no Pod; if one still exists it is
	// terminating and must not be treated as a restart candidate.
	if set.Spec.Replicas == nil || *set.Spec.Replicas < 1 {
		return errors.New("StatefulSet has no desired replica")
	}
	notebook, err := s.restartParentNotebook(ctx, a.Namespace, set)
	if err != nil {
		return err
	}
	node, err := s.restartNode(ctx, pod)
	if err != nil {
		return err
	}
	revision := set.Status.UpdateRevision
	if revision == "" {
		return errors.New("StatefulSet has no desired revision")
	}
	if pod.Labels[appsv1.StatefulSetRevisionLabel] == "" {
		return errors.New("Pod has no controller revision")
	}
	// The Pod may belong to an older revision while the StatefulSet is updating.
	// That is still a valid restart target: convergence requires the replacement
	// to reach the StatefulSet's current desired revision.
	w.pod = pod
	w.podSet = set
	w.podNotebook = notebook
	w.podNode = node
	w.podFingerprint = podRestartFingerprint(pod, set, notebook)
	w.prepared.TargetUID = string(pod.UID)
	w.prepared.ResourceVersion = pod.ResourceVersion
	// Server-owned, never accepted from the browser. The store qualifies this
	// validated Notebook name with the action namespace to derive the secondary
	// notebook:namespace/name lock.
	w.prepared.ParentNotebook = notebook.GetName()
	w.ev.Target = w.podFingerprint
	w.ev.add("StatefulSet", a.Namespace, set.Name, restartSetEvidence(set))
	w.ev.add("Notebook", a.Namespace, notebook.GetName(), struct {
		Fingerprint string
		Stopped     bool
		Deleting    bool
	}{notebookFingerprint(notebook), notebookStopped(notebook), notebook.GetDeletionTimestamp() != nil})
	w.ev.add("Node", "", node.Name, restartNodeEvidence(node))
	w.prepared.Warnings = append(w.prepared.Warnings,
		"Eviction interrupts running kernels, processes, writable container layers and emptyDir contents. Persistent volume data is not deleted.",
		"The API enforces PodDisruptionBudgets. A blocked eviction is reported as failed without force deletion, zero-grace deletion or automatic retry.",
		"Success requires the original Pod UID to disappear and a distinct owned replacement to become Ready at the StatefulSet's desired revision.",
		"Only Notebook-owned StatefulSet Pods are supported; application-level checkpointing and data consistency are not guaranteed.",
	)
	return nil
}

func (s *Service) restartParentNotebook(ctx context.Context, ns string, set *appsv1.StatefulSet) (*unstructured.Unstructured, error) {
	owner, err := singleControllerOwner(set.OwnerReferences)
	if err != nil {
		return nil, errors.New("StatefulSet is not controlled by a Notebook")
	}
	if owner.Kind != "Notebook" || owner.APIVersion != "kubeflow.org/v1" || owner.UID == "" {
		return nil, errors.New("StatefulSet is not controlled by a Notebook")
	}
	n, err := s.Dynamic.Resource(Notebooks).Namespace(ns).Get(ctx, owner.Name, metav1.GetOptions{})
	if err != nil {
		return nil, safe(err)
	}
	if n.GetUID() != owner.UID {
		return nil, errors.New("Pod owner chain changed; prepare a new plan")
	}
	if n.GetDeletionTimestamp() != nil {
		return nil, errors.New("Notebook is terminating")
	}
	if n.GetAPIVersion() != "kubeflow.org/v1" || n.GetKind() != "Notebook" {
		return nil, errors.New("unsupported Notebook API contract")
	}
	if notebookStopped(n) {
		return nil, errors.New("parent Notebook is stopped; start it before restarting its Pod")
	}
	return n, nil
}

func notebookStopped(n *unstructured.Unstructured) bool {
	_, stopped := n.GetAnnotations()[stoppedKey]
	return stopped
}

func (s *Service) restartNode(ctx context.Context, pod *corev1.Pod) (*corev1.Node, error) {
	if pod.Spec.NodeName == "" {
		return nil, errors.New("Pod is not assigned to a Node")
	}
	node, err := s.Typed.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	if err != nil {
		return nil, safe(err)
	}
	if err = validateRestartNode(node); err != nil {
		return nil, err
	}
	return node, nil
}

// validateRestartNode deliberately refuses every state that can leave the
// replacement unschedulable or make the original process's stop unobservable.
// A zero-grace delete is never used as a fallback for an unreachable node.
func validateRestartNode(node *corev1.Node) error {
	if node.DeletionTimestamp != nil {
		return errors.New("assigned Node is terminating")
	}
	ready := false
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			ready = c.Status == corev1.ConditionTrue
			break
		}
	}
	if !ready {
		return errors.New("assigned Node is not Ready")
	}
	if node.Spec.Unschedulable {
		return errors.New("assigned Node is unschedulable")
	}
	for _, taint := range node.Spec.Taints {
		if taint.Key == corev1.TaintNodeUnreachable || taint.Key == corev1.TaintNodeNotReady {
			return errors.New("assigned Node is unreachable or not ready")
		}
	}
	return nil
}

func isMirrorOrStaticPod(p *corev1.Pod) bool {
	_, ok := p.Annotations[corev1.MirrorPodAnnotationKey]
	source := p.Annotations["kubernetes.io/config.source"]
	return ok || source != "" && source != "api"
}

func singleControllerOwner(refs []metav1.OwnerReference) (metav1.OwnerReference, error) {
	var found *metav1.OwnerReference
	for i := range refs {
		if refs[i].Controller == nil || !*refs[i].Controller {
			continue
		}
		if found != nil {
			return metav1.OwnerReference{}, errors.New("Pod has an ambiguous controller owner chain")
		}
		found = &refs[i]
	}
	if found == nil {
		return metav1.OwnerReference{}, errors.New("Pod is not managed by a controller")
	}
	return *found, nil
}

type ownerIdentity struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	UID        types.UID `json:"uid"`
}

func ownerIdentities(refs []metav1.OwnerReference) []ownerIdentity {
	out := []ownerIdentity{}
	for _, r := range refs {
		if r.Controller == nil || !*r.Controller {
			continue
		}
		out = append(out, ownerIdentity{r.APIVersion, r.Kind, r.Name, r.UID})
	}
	sort.Slice(out, func(i, j int) bool { return string(out[i].UID) < string(out[j].UID) })
	return out
}

// restartPodSpec removes values that must never be persisted in evidence (env
// literals, envFrom, and opaque volume payloads) while keeping the structural
// fields that determine the target, controller revision and restart identity.
func restartPodSpec(spec corev1.PodSpec) string {
	copySpec := *spec.DeepCopy()
	redact := func(cs []corev1.Container) {
		for i := range cs {
			env := []corev1.EnvVar{}
			for _, e := range cs[i].Env {
				env = append(env, corev1.EnvVar{Name: e.Name})
			}
			cs[i].Env = env
			cs[i].EnvFrom = nil
		}
	}
	redact(copySpec.Containers)
	redact(copySpec.InitContainers)
	for i := range copySpec.EphemeralContainers {
		env := []corev1.EnvVar{}
		for _, e := range copySpec.EphemeralContainers[i].Env {
			env = append(env, corev1.EnvVar{Name: e.Name})
		}
		copySpec.EphemeralContainers[i].Env = env
		copySpec.EphemeralContainers[i].EnvFrom = nil
	}
	for i := range copySpec.Volumes {
		v := &copySpec.Volumes[i]
		if v.Projected != nil {
			v.Projected = &corev1.ProjectedVolumeSource{}
		}
		if v.CSI != nil {
			v.CSI.VolumeAttributes = nil
		}
	}
	return digest(copySpec)
}

func podRestartFingerprint(p *corev1.Pod, set *appsv1.StatefulSet, notebook *unstructured.Unstructured) string {
	return digest(struct {
		PodUID              types.UID
		Namespace           string
		Name                string
		PodOwners           []ownerIdentity
		PodLabels           map[string]string
		PodSpec             string
		SetUID              types.UID
		SetOwners           []ownerIdentity
		SetGeneration       int64
		NotebookUID         types.UID
		NotebookFingerprint string
	}{
		PodUID:              p.UID,
		Namespace:           p.Namespace,
		Name:                p.Name,
		PodOwners:           ownerIdentities(p.OwnerReferences),
		PodLabels:           p.Labels,
		PodSpec:             restartPodSpec(p.Spec),
		SetUID:              set.UID,
		SetOwners:           ownerIdentities(set.OwnerReferences),
		SetGeneration:       set.Generation,
		NotebookUID:         notebook.GetUID(),
		NotebookFingerprint: notebookFingerprint(notebook),
	})
}

func restartSetEvidence(set *appsv1.StatefulSet) any {
	return struct {
		UID            types.UID
		Generation     int64
		Owners         []ownerIdentity
		UpdateRevision string
		Replicas       *int32
	}{set.UID, set.Generation, ownerIdentities(set.OwnerReferences), set.Status.UpdateRevision, set.Spec.Replicas}
}

func restartNodeEvidence(node *corev1.Node) any {
	ready := false
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			ready = c.Status == corev1.ConditionTrue
		}
	}
	return struct {
		UID           types.UID
		Ready         bool
		Unschedulable bool
		Taints        []corev1.Taint
		Terminating   bool
	}{node.UID, ready, node.Spec.Unschedulable, node.Spec.Taints, node.DeletionTimestamp != nil}
}

// executePodRestart is a specialized path. The generic Execute compares the
// preview resourceVersion, which deliberately changes when only Pod status
// changes. Here the stable fingerprint is authoritative, while the live Pod UID
// and resourceVersion immediately before eviction become the API preconditions.
func (s *Service) executePodRestart(ctx context.Context, p model.Prepared, cp func(string) error) (err error) {
	mutationAttempted := false
	defer func() {
		// This path only reads until the eviction call. Revalidation, policy or
		// checkpoint rejection before that call must not strand parent locks.
		if err != nil && !mutationAttempted {
			err = errors.Join(model.ErrRejectedBeforeMutation, err)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	w, err := s.build(ctx, p.Action)
	if err != nil {
		return err
	}
	if w.prepared.TargetUID != p.TargetUID || w.prepared.ParentNotebook != p.ParentNotebook || string(w.prepared.Data) != string(p.Data) {
		return ErrConflict
	}
	target, err := s.revalidatePodRestartTarget(ctx, w)
	if err != nil {
		return err
	}
	if err = checkpoint(cp, "evict-pod"); err != nil {
		return err
	}
	// Re-read after the durable checkpoint so the eviction uses the freshest UID
	// and resourceVersion while still rechecking every relevant precondition.
	if target, err = s.revalidatePodRestartTarget(ctx, w); err != nil {
		return err
	}
	// Exactly one eviction request. A transport failure after this point is
	// ambiguous and must be reviewed; it is never retried automatically.
	mutationAttempted = true
	if err = evictPod(ctx, s.Typed.CoreV1().Pods(target.Namespace), target); err != nil {
		return evictionError(err)
	}
	return s.waitForRestartReplacement(ctx, w)
}

// revalidatePodRestartTarget refreshes the whole owner chain immediately before
// the write. Status-only Pod churn is allowed (and a new resourceVersion is
// used), but any spec, owner-chain, parent or node-safety change is a conflict.
func (s *Service) revalidatePodRestartTarget(ctx context.Context, w *work) (*corev1.Pod, error) {
	pod, err := s.Typed.CoreV1().Pods(w.pod.Namespace).Get(ctx, w.pod.Name, metav1.GetOptions{})
	if err != nil {
		return nil, safe(err)
	}
	if pod.UID != w.pod.UID || pod.DeletionTimestamp != nil {
		return nil, fmt.Errorf("target Pod changed before eviction: %w", ErrConflict)
	}
	if pod.Status.Phase != corev1.PodRunning || isMirrorOrStaticPod(pod) {
		return nil, fmt.Errorf("target Pod is no longer restartable before eviction: %w", ErrConflict)
	}
	set, err := s.Typed.AppsV1().StatefulSets(pod.Namespace).Get(ctx, w.podSet.Name, metav1.GetOptions{})
	if err != nil {
		return nil, safe(err)
	}
	if set.UID != w.podSet.UID || set.Generation != w.podSet.Generation || set.Status.UpdateRevision != w.podSet.Status.UpdateRevision || set.DeletionTimestamp != nil {
		return nil, fmt.Errorf("StatefulSet changed before eviction: %w", ErrConflict)
	}
	notebook, err := s.restartParentNotebook(ctx, pod.Namespace, set)
	if err != nil {
		return nil, fmt.Errorf("parent Notebook changed before eviction: %w", ErrConflict)
	}
	if pod.Spec.NodeName == "" {
		return nil, fmt.Errorf("target Pod lost its node assignment before eviction: %w", ErrConflict)
	}
	node, err := s.Typed.CoreV1().Nodes().Get(ctx, pod.Spec.NodeName, metav1.GetOptions{})
	if err != nil {
		return nil, safe(err)
	}
	if err = validateRestartNode(node); err != nil {
		return nil, fmt.Errorf("assigned Node is no longer safe: %w: %v", ErrConflict, err)
	}
	if podRestartFingerprint(pod, set, notebook) != w.podFingerprint {
		return nil, fmt.Errorf("target Pod identity changed before eviction: %w", ErrConflict)
	}
	return pod, nil
}

func evictPod(ctx context.Context, pods corev1client.PodInterface, pod *corev1.Pod) error {
	uid, rv := pod.UID, pod.ResourceVersion
	if uid == "" || rv == "" {
		return fmt.Errorf("%w: eviction requires UID and resourceVersion preconditions", model.ErrRejectedBeforeMutation)
	}
	// No GracePeriodSeconds override: never translate this into a zero-grace
	// forced delete. policy/v1 eviction remains the only mutation.
	return pods.EvictV1(ctx, &policyv1.Eviction{
		ObjectMeta:    metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace},
		DeleteOptions: &metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid, ResourceVersion: &rv}},
	})
}

// evictionError distinguishes explicit API rejections (no mutation) from
// transport or post-acceptance ambiguity. Only the sentinel causes the manager
// to release locks as a failed operation.
func evictionError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, model.ErrRejectedBeforeMutation):
		return err
	case apierrors.IsForbidden(err):
		return fmt.Errorf("%w: Kubernetes API denied the eviction request", model.ErrRejectedBeforeMutation)
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w: Kubernetes resource not found", model.ErrRejectedBeforeMutation)
	case apierrors.IsConflict(err):
		return fmt.Errorf("%w: %w: eviction preconditions no longer match", ErrConflict, model.ErrRejectedBeforeMutation)
	case apierrors.IsTooManyRequests(err):
		return fmt.Errorf("%w: eviction blocked by PodDisruptionBudget or API throttling; no force fallback exists", model.ErrRejectedBeforeMutation)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return fmt.Errorf("%w: Kubernetes API rejected eviction validation", model.ErrRejectedBeforeMutation)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		// The request may have reached the API server. Keep this ambiguous so
		// target locks remain held for review; never call it rejected.
		return errors.New("Kubernetes API eviction outcome is unknown")
	}
}

func (s *Service) waitForRestartReplacement(ctx context.Context, w *work) error {
	return s.wait(ctx, func() (bool, error) {
		set, err := s.Typed.AppsV1().StatefulSets(w.pod.Namespace).Get(ctx, w.podSet.Name, metav1.GetOptions{})
		if err != nil {
			return false, safe(err)
		}
		if set.UID != w.podSet.UID || set.Status.UpdateRevision != w.podSet.Status.UpdateRevision || set.DeletionTimestamp != nil {
			return false, fmt.Errorf("StatefulSet changed during restart: %w", ErrConflict)
		}
		if digest(ownerIdentities(set.OwnerReferences)) != digest(ownerIdentities(w.podSet.OwnerReferences)) {
			return false, fmt.Errorf("StatefulSet parent ownership changed during restart: %w", ErrConflict)
		}
		notebook, err := s.restartParentNotebook(ctx, w.pod.Namespace, set)
		if err != nil {
			return false, err
		}
		if notebookFingerprint(notebook) != notebookFingerprint(w.podNotebook) {
			return false, fmt.Errorf("parent Notebook changed during restart: %w", ErrConflict)
		}
		current, err := s.Typed.CoreV1().Pods(w.pod.Namespace).Get(ctx, w.pod.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, safe(err)
		}
		if current.UID == w.pod.UID {
			return false, nil
		}
		return restartReplacementReady(current, set)
	})
}

// restartReplacementReady requires a distinct, StatefulSet-owned Pod that is
// Ready at the controller's desired revision. PodReady alone is not enough: the
// original UID must already be absent (the caller observes the same name) and
// every declared container must report ready.
func restartReplacementReady(p *corev1.Pod, set *appsv1.StatefulSet) (bool, error) {
	if p.DeletionTimestamp != nil || p.Status.Phase != corev1.PodRunning {
		return false, nil
	}
	controller, err := singleControllerOwner(p.OwnerReferences)
	if err != nil || controller.Kind != "StatefulSet" || controller.UID != set.UID || controller.Name != set.Name {
		return false, fmt.Errorf("replacement Pod is not controlled by the approved StatefulSet: %w", ErrConflict)
	}
	if set.DeletionTimestamp != nil || set.Status.ObservedGeneration < set.Generation {
		return false, nil
	}
	if set.Spec.Replicas == nil || *set.Spec.Replicas < 1 || set.Status.ReadyReplicas < *set.Spec.Replicas {
		return false, nil
	}
	revision := set.Status.UpdateRevision
	if revision == "" || set.Status.CurrentRevision != revision {
		return false, nil
	}
	if p.Labels[appsv1.StatefulSetRevisionLabel] != revision {
		return false, nil
	}
	ready := false
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	if !ready {
		return false, nil
	}
	for _, c := range p.Spec.Containers {
		found := false
		for _, status := range p.Status.ContainerStatuses {
			if status.Name == c.Name && status.Ready {
				found = true
			}
		}
		if !found {
			return false, nil
		}
	}
	return true, nil
}
