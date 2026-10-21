package kube

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"kubeflow-admin/internal/model"
)

type work struct {
	prepared model.Prepared
	ev       evidence
	notebook *unstructured.Unstructured
	profile  *unstructured.Unstructured
	pvc      *corev1.PersistentVolumeClaim
	pv       *corev1.PersistentVolume
	ops      []patchOp
	delayed  bool

	// profileOwner and profileQuota capture the exact owner identity and hard
	// quota map reviewed for a Profile update, so the live object can be
	// revalidated against the plan immediately before the write.
	profileOwner string
	profileQuota map[string]any
	// profileNamespace is the UID of the namespace reviewed for a Profile
	// update. A namespace replaced between preview and execution is refused
	// instead of adopted.
	profileNamespace types.UID

	// Pod restart targets are kept separate from Notebook edit targets: the
	// restart path needs the validated owner chain, node and stable fingerprint
	// without acquiring Notebook mutation semantics.
	pod            *corev1.Pod
	podSet         *appsv1.StatefulSet
	podNotebook    *unstructured.Unstructured
	podNode        *corev1.Node
	podFingerprint string

	// Node cordon targets are cluster-scoped and intentionally bypass Profile
	// scope. The Node is re-read in Execute; only stable fields are evidence.
	node *corev1.Node
}

func (s *Service) Prepare(ctx context.Context, a model.Action) (model.Prepared, error) {
	if a.Kind == "profile.delete" {
		return s.prepareProfileDelete(ctx, a)
	}
	w, err := s.build(ctx, a)
	if err != nil {
		return model.Prepared{}, err
	}
	if err = s.dryRun(ctx, w); err != nil {
		return model.Prepared{}, err
	}
	return w.prepared, nil
}
func (s *Service) build(ctx context.Context, a model.Action) (*work, error) {
	if !s.Config.Features.WritesEnabled {
		return nil, errors.New("writes are disabled")
	}
	// A Profile name is also its namespace name and has its own grammar. It is
	// checked first so an over-long or otherwise invalid Profile name is
	// reported with its own reason, not as a generic object name failure.
	if a.Kind == "profile.create" || a.Kind == "profile.update" {
		if err := profileName(a.Name); err != nil {
			return nil, err
		}
	}
	if err := name(a.Name); err != nil {
		return nil, err
	}
	if len(a.Reason) > 1024 {
		return nil, errors.New("reason is too long")
	}
	if a.Kind == "pod.restart" && !s.Config.Features.PodRestarts {
		return nil, errors.New("pod restarts are disabled")
	}
	if (a.Kind == "node.cordon" || a.Kind == "node.uncordon") && !s.Config.Features.NodeCordon {
		return nil, errors.New("node cordon is disabled")
	}
	w := &work{ev: s.newEvidence()}
	w.prepared.Action = a
	w.prepared.Summary = a.Kind + " " + a.Namespace + "/" + a.Name
	if a.Kind != "pod.restart" {
		w.prepared.Warnings = []string{"API dry-run does not prove controller reconciliation or available storage/compute capacity."}
	}
	if a.Kind == "node.cordon" || a.Kind == "node.uncordon" {
		// Nodes are cluster-scoped: never require or acquire a Profile scope.
		w.prepared.Summary = a.Kind + " " + a.Name
		if err := s.prepareNodeCordon(ctx, a, w); err != nil {
			return nil, err
		}
	} else if a.Kind == "profile.create" || a.Kind == "profile.quota.change" || a.Kind == "profile.update" {
		if a.Namespace != "" && a.Namespace != a.Name {
			return nil, errors.New("Profile namespace must match its name")
		}
		// The Profile namespace is the Profile name and must satisfy the
		// namespace scope policy before any Profile write is planned.
		if err := s.namespacePolicy(a.Name); err != nil {
			return nil, err
		}
		if err := s.prepareProfile(ctx, a, w); err != nil {
			return nil, err
		}
	} else {
		if err := s.scope(ctx, a.Namespace, &w.ev); err != nil {
			return nil, err
		}
		switch a.Kind {
		case "pvc.create.dynamic", "storage.create.static", "pv.reserve-and-claim", "pvc.expand":
			if err := s.prepareStorage(ctx, a, w); err != nil {
				return nil, err
			}
		case "notebook.start", "notebook.stop", "notebook.edit":
			if !s.Config.Notebooks.ControllerContractConfirmed {
				return nil, errors.New("Notebook writes require a confirmed installed controller contract")
			}
			n, err := s.Dynamic.Resource(Notebooks).Namespace(a.Namespace).Get(ctx, a.Name, metav1.GetOptions{})
			if err != nil {
				return nil, safe(err)
			}
			if n.GetDeletionTimestamp() != nil {
				return nil, errors.New("Notebook is terminating")
			}
			if n.GetAPIVersion() != "kubeflow.org/v1" || n.GetKind() != "Notebook" {
				return nil, errors.New("unsupported Notebook API contract")
			}
			w.notebook = n
			w.prepared.TargetUID = string(n.GetUID())
			w.prepared.ResourceVersion = n.GetResourceVersion()
			w.ev.Target = notebookFingerprint(n)
			ops, err := s.notebookChanges(ctx, a, n, &w.ev)
			if err != nil {
				return nil, err
			}
			w.ops = ops
			w.prepared.Warnings = append(w.prepared.Warnings, "Stopping loses running kernels, processes, writable container layers and emptyDir contents. PVC data is not deleted.")
		case "pod.restart":
			if err := s.preparePodRestart(ctx, a, w); err != nil {
				return nil, err
			}
		default:
			return nil, errors.New("unsupported action kind")
		}
	}
	w.prepared.Data = w.ev.bytes()
	s.previews(w)
	if w.notebook != nil {
		w.prepared.Warnings = append(w.prepared.Warnings, "Preflight models ordinary init-container maxima, overhead and unambiguous LimitRange defaults, not admission-injected sidecars. Quota headroom is rechecked after stop; capacity is not reserved.")
	}
	return w, nil
}
func (s *Service) dryRun(ctx context.Context, w *work) error {
	o := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	switch w.prepared.Action.Kind {
	case "profile.create":
		_, err := s.Dynamic.Resource(Profiles).Create(ctx, w.profile, o)
		return safe(err)
	case "profile.quota.change", "profile.update":
		b, _ := json.Marshal(conditional(string(w.profile.GetUID()), w.profile.GetResourceVersion(), w.ops))
		_, err := s.Dynamic.Resource(Profiles).Patch(ctx, w.profile.GetName(), types.JSONPatchType, b, metav1.PatchOptions{DryRun: o.DryRun})
		return safe(err)
	case "pvc.create.dynamic":
		_, err := s.Typed.CoreV1().PersistentVolumeClaims(w.pvc.Namespace).Create(ctx, w.pvc, o)
		return safe(err)
	case "storage.create.static":
		if _, err := s.Typed.CoreV1().PersistentVolumes().Create(ctx, w.pv, o); err != nil {
			return safe(err)
		}
		_, err := s.Typed.CoreV1().PersistentVolumeClaims(w.pvc.Namespace).Create(ctx, w.pvc, o)
		return safe(err)
	case "pv.reserve-and-claim":
		b, _ := json.Marshal(conditional(string(w.pv.UID), w.pv.ResourceVersion, w.ops))
		if _, err := s.Typed.CoreV1().PersistentVolumes().Patch(ctx, w.pv.Name, types.JSONPatchType, b, metav1.PatchOptions{DryRun: o.DryRun}); err != nil {
			return safe(err)
		}
		_, err := s.Typed.CoreV1().PersistentVolumeClaims(w.pvc.Namespace).Create(ctx, w.pvc, o)
		return safe(err)
	case "pvc.expand":
		b, _ := json.Marshal(conditional(string(w.pvc.UID), w.pvc.ResourceVersion, w.ops))
		_, err := s.Typed.CoreV1().PersistentVolumeClaims(w.pvc.Namespace).Patch(ctx, w.pvc.Name, types.JSONPatchType, b, metav1.PatchOptions{DryRun: o.DryRun})
		return safe(err)
	case "node.cordon", "node.uncordon":
		// The resourceVersion test makes the dry-run atomic with the observed
		// heartbeat; Execute refreshes and retries it a bounded number of times.
		_, err := s.Typed.CoreV1().Nodes().Patch(ctx, w.node.Name, types.JSONPatchType, nodeCordonPatch(w.node.UID, w.node.ResourceVersion, w.prepared.Action.Kind == "node.cordon"), metav1.PatchOptions{DryRun: o.DryRun})
		return safe(err)
	default:
		if len(w.ops) == 0 {
			return nil
		}
		_, err := s.patchNotebook(ctx, w.notebook, w.ops, true)
		return err
	}
}
func quantity(s string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil || q.Sign() <= 0 {
		return resource.Quantity{}, errors.New("a positive Kubernetes quantity is required")
	}
	return q, nil
}

// quotaQuantity validates a declared quota value. A ResourceQuota hard value
// may legitimately be zero: it declares that the resource is not allowed at
// all, and a deployed zero must round-trip unchanged so editing another quota
// key is never blocked by it. A negative value is never valid. Requested
// storage sizes keep the stricter positive rule of quantity().
func quotaQuantity(s string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(s)
	if err != nil || q.Sign() < 0 {
		return resource.Quantity{}, errors.New("a quota value must be a non-negative Kubernetes quantity")
	}
	return q, nil
}

// countQuotaKey reports whether a quota key counts objects instead of
// measuring a resource. A Pod count, a claim count and a configured extended
// device count are whole numbers: a fraction of a Pod is not a quota.
func countQuotaKey(key string, presets []string) bool {
	switch key {
	case "pods", "persistentvolumeclaims":
		return true
	}
	// The approved presets are the configured extended device resources, which
	// are counted per device.
	return has(presets, key)
}

// wholeQuantity reports whether a quota quantity is a whole number of objects.
func wholeQuantity(q resource.Quantity) bool {
	v, ok := q.AsInt64()
	if !ok {
		return false
	}
	return q.Cmp(*resource.NewQuantity(v, q.Format)) == 0
}

// quotaCountQuantity validates a declared count quota value. It is a whole,
// non-negative number of objects.
func quotaCountQuantity(key, raw string, presets []string) (resource.Quantity, error) {
	q, err := quotaQuantity(raw)
	if err != nil {
		return resource.Quantity{}, err
	}
	if !wholeQuantity(q) {
		return resource.Quantity{}, errors.New("a quota count must be a whole number of objects")
	}
	return q, nil
}

var builtinQuotaResources = []string{"requests.cpu", "limits.cpu", "requests.memory", "limits.memory", "requests.storage", "persistentvolumeclaims", "pods"}

// supportedQuotaResource reports whether this adapter may author the hard key.
// Anything else is operator/controller managed and must never be introduced or
// changed by a quota plan.
func supportedQuotaResource(k string, presets []string) bool {
	return has(builtinQuotaResources, k) || has(presets, k)
}

// quota validates the complete desired set for a new Profile. Introducing an
// unsupported key here would create a Profile owned quota the adapter cannot
// later reconcile, so it is rejected.
func quota(a map[string]string, presets []string) (map[string]any, error) {
	if len(a) == 0 {
		return nil, errors.New("nonempty quota required; removing quotas is unsupported")
	}
	out := map[string]any{}
	for k, v := range a {
		if !supportedQuotaResource(k, presets) {
			return nil, errors.New("unsupported quota resource")
		}
		if countQuotaKey(k, presets) {
			q, err := quotaCountQuantity(k, v, presets)
			if err != nil {
				return nil, err
			}
			out[k] = q.String()
			continue
		}
		q, err := quotaQuantity(v)
		if err != nil {
			return nil, err
		}
		out[k] = q.String()
	}
	return out, nil
}

// mergeQuota reconciles a quota change against the Profile's existing hard map.
// Every existing key is retained: unsupported (operator/controller managed) keys
// are preserved verbatim and may only be round-tripped with an identical value,
// while supported keys must be supplied as positive quantities so that a changed
// value is intentional. Dropping an existing supported key stays a no-op error,
// preserving the no-removal guard. The returned map replaces the whole hard map,
// so preserved keys survive the patch.
func mergeQuota(existing map[string]any, requested map[string]string, presets []string) (map[string]any, error) {
	if len(requested) == 0 {
		return nil, errors.New("quota removal is unsupported")
	}
	out := map[string]any{}
	for k, v := range existing {
		out[k] = v
	}
	for k, raw := range requested {
		if !supportedQuotaResource(k, presets) {
			old, ok := existing[k]
			if !ok {
				return nil, errors.New("unsupported quota resource")
			}
			// The declared value may be a string or an unquoted integer: the
			// Profile CRD declares the hard map as int-or-string. An equivalent
			// round-trip is preserved exactly, including its original value and
			// type, so an operator-managed key is never rewritten.
			oldText, ok := quotaValue(old)
			if !ok {
				return nil, errors.New("unsupported Profile quota shape")
			}
			if !quantityEqual(oldText, raw) {
				return nil, errors.New("changing an unsupported quota key is unsupported")
			}
			out[k] = old
			continue
		}
		q, err := quotaQuantity(raw)
		if err != nil {
			return nil, err
		}
		if countQuotaKey(k, presets) {
			q, err = quotaCountQuantity(k, raw, presets)
			if err != nil {
				return nil, err
			}
		}
		out[k] = q.String()
	}
	for k := range existing {
		if supportedQuotaResource(k, presets) {
			if _, ok := requested[k]; !ok {
				return nil, errors.New("quota key removal is unsupported by this adapter")
			}
		}
	}
	return out, nil
}

// quantityEqual compares two quota values, accepting different spellings of the
// same quantity so a round-tripped unknown key is not mistaken for a change.
func quantityEqual(a, b string) bool {
	if a == b {
		return true
	}
	qa, ea := resource.ParseQuantity(a)
	qb, eb := resource.ParseQuantity(b)
	return ea == nil && eb == nil && qa.Cmp(qb) == 0
}

// Execute only accepts a server-owned Prepared record. Preview evidence is
// checked against a complete fresh validation and server dry-run before writes.
func (s *Service) Execute(ctx context.Context, p model.Prepared, cp func(string) error) error {
	if p.Action.Kind == "profile.delete" {
		return s.executeProfileDelete(ctx, p, cp)
	}
	if p.Action.Kind == "pod.restart" {
		return s.executePodRestart(ctx, p, cp)
	}
	if p.Action.Kind == "node.cordon" || p.Action.Kind == "node.uncordon" {
		return s.executeNodeCordon(ctx, p, cp)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	fresh, err := s.Prepare(ctx, p.Action)
	if err != nil {
		return errors.Join(model.ErrRejectedBeforeMutation, err)
	}
	if fresh.TargetUID != p.TargetUID || fresh.ResourceVersion != p.ResourceVersion || string(fresh.Data) != string(p.Data) {
		return errors.Join(model.ErrRejectedBeforeMutation, ErrConflict)
	}
	w, err := s.build(ctx, p.Action)
	if err != nil {
		return errors.Join(model.ErrRejectedBeforeMutation, err)
	}
	if w.prepared.TargetUID != p.TargetUID || w.prepared.ResourceVersion != p.ResourceVersion || string(w.prepared.Data) != string(p.Data) {
		return errors.Join(model.ErrRejectedBeforeMutation, ErrConflict)
	}
	firstCheckpoint := true
	guardedCheckpoint := func(step string) error {
		err := checkpoint(cp, step)
		if firstCheckpoint && err != nil {
			return errors.Join(model.ErrRejectedBeforeMutation, err)
		}
		firstCheckpoint = false
		return err
	}
	if strings.HasPrefix(p.Action.Kind, "notebook.") {
		return s.executeNotebook(ctx, w, guardedCheckpoint)
	}
	if p.Action.Kind == "profile.update" || p.Action.Kind == "profile.quota.change" {
		return s.executeProfileUpdate(ctx, w, guardedCheckpoint)
	}
	if p.Action.Kind == "profile.create" {
		return s.executeProfileCreate(ctx, w, guardedCheckpoint)
	}
	return s.executeStorageOrProfile(ctx, w, guardedCheckpoint)
}

func (s *Service) checkEvidence(ctx context.Context, ns string, old evidence) error {
	e := s.newEvidence()
	if err := s.scope(ctx, ns, &e); err != nil {
		return err
	}
	// Recheck the namespace/Profile/quota policy at multi-write boundaries. Storage
	// identities are separately checked by the reservation/binding paths.
	for _, r := range old.Dependencies {
		if r.Kind != "Profile" && r.Kind != "Namespace" && r.Kind != "ResourceQuotaList" && r.Kind != "LimitRangeList" {
			continue
		}
		found := false
		for _, n := range e.Dependencies {
			if n.Kind == r.Kind && n.Name == r.Name && n.Namespace == r.Namespace {
				found = true
				if n.Digest != r.Digest {
					return ErrConflict
				}
			}
		}
		if !found {
			return ErrConflict
		}
	}
	return nil
}
