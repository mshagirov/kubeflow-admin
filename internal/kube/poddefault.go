package kube

// PodDefaults are substeps of PVC creation and unused-only PVC deletion.
// There is no standalone PodDefault route, action or browser editor.
//
// The object shape is fixed by the server:
//   - one PersistentVolumeClaim volume and one matching volumeMount;
//   - one selector label derived from the final PodDefault name;
//   - a generated description and provenance annotations;
//   - no env, Secret, container, service, additional PVC or unrelated setting.
//
// An existing object is only patched with an explicit overwrite acknowledgement
// and an exact UID/resourceVersion precondition. The whole spec is replaced, so
// removal of the previous selector/other PVC/injected settings is explicit in
// the reviewed plan; foreign metadata, ownerReferences and finalizers are preserved.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
)

var PodDefaults = schema.GroupVersionResource{Group: "kubeflow.org", Version: "v1alpha1", Resource: "poddefaults"}

const (
	maxPodDefaultCleanup = 32
	// The upstream admission webhook derives "poddefault-<name>" as an
	// annotation name segment. Capping the reviewed name independently keeps
	// that derived key within Kubernetes' 63-character segment bound.
	maxPodDefaultNameBytes = 52
	podDefaultWorkflowKey  = "kubeflow-admin.io/workflow"
	podDefaultPVCUIDKey    = "kubeflow-admin.io/pvc-uid"
)

type podDefaultEvidence struct {
	Name               string   `json:"name"`
	MountPathDigest    string   `json:"mountPathDigest"`
	ClaimName          string   `json:"claimName"`
	Existing           bool     `json:"existing"`
	ExistingUID        string   `json:"existingUID,omitempty"`
	ExistingRV         string   `json:"existingResourceVersion,omitempty"`
	Overwrite          bool     `json:"overwrite"`
	Desired            string   `json:"desired"`
	Shared             bool     `json:"shared,omitempty"`
	ExternalManaged    bool     `json:"externalManaged,omitempty"`
	AdditionalSettings bool     `json:"additionalSettings,omitempty"`
	OtherPVCs          []string `json:"otherPVCs,omitempty"`
}

type podDefaultPlan struct {
	namespace             string
	claim                 string
	name                  string
	mountPath             string
	volumeName            string
	spec                  map[string]any
	existingUID           types.UID
	existingRV            string
	overwrite             bool
	evidence              podDefaultEvidence
	warnings              []string
	changes               []model.Change
	workflow              string
	selectedPV            string
	requireBound          bool
	existingHasAnnotation bool
	pvcUID                types.UID
}

type podDefaultCleanupTarget struct {
	Name               string   `json:"name"`
	Namespace          string   `json:"namespace"`
	UID                string   `json:"uid"`
	ResourceVersion    string   `json:"resourceVersion"`
	OtherPVCs          []string `json:"otherPVCs"`
	Shared             bool     `json:"shared"`
	ExternalManaged    bool     `json:"externalManaged"`
	AdditionalSettings bool     `json:"additionalSettings"`
	Finalizers         int      `json:"finalizers,omitempty"`
}

type podDefaultUsage struct {
	Claims     []string
	Additional bool
	External   bool
}

func (u podDefaultUsage) otherPVCs(claim string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, c := range u.Claims {
		if c == claim || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// podDefaultAPI reports whether the PodDefault resource is installed and, when
// mutation is requested, whether discovery advertises every verb this adapter
// uses. One unavailable version is not proof of absence: all advertised
// kubeflow.org versions must be verified first. Discovery failures never become
// an implicit empty reference set.
func (s *Service) podDefaultAPI(ctx context.Context, mutate bool) (bool, error) {
	if s.Typed == nil || s.Dynamic == nil {
		return false, errors.New("Kubernetes clients are not configured")
	}
	discovery := s.Typed.Discovery()
	const expectedVersion = "kubeflow.org/v1alpha1"
	list, err := discovery.ServerResourcesForGroupVersion(expectedVersion)
	if err == nil {
		if list == nil {
			return false, errors.New("PodDefault API discovery returned no resource list; references cannot be verified")
		}
		present, _, err := podDefaultResourceState(list, mutate)
		if err != nil || present {
			return present, err
		}
		// The expected version is served but has no PodDefault resource. A CRD
		// may still be served only at another version, so enumerate the group's
		// advertised versions before concluding genuine absence.
		return s.podDefaultOtherGroupVersions(mutate, expectedVersion)
	}
	if !apierrors.IsNotFound(err) {
		return false, errors.New("PodDefault API discovery failed; references cannot be verified")
	}
	return s.podDefaultOtherGroupVersions(mutate, expectedVersion)
}

// podDefaultOtherGroupVersions resolves genuine group/resource absence versus a
// PodDefault API served only at an unsupported version. Every group and targeted
// group-version discovery call must succeed with a nonnil list; any error or
// partial result fails closed instead of becoming "no references".
func (s *Service) podDefaultOtherGroupVersions(mutate bool, expectedVersion string) (bool, error) {
	discovery := s.Typed.Discovery()
	groups, err := discovery.ServerGroups()
	if err != nil || groups == nil {
		return false, errors.New("PodDefault API group discovery failed; references cannot be verified")
	}
	var versions []string
	for i := range groups.Groups {
		group := &groups.Groups[i]
		if group.Name != PodDefaults.Group {
			continue
		}
		if len(group.Versions) == 0 || len(group.Versions) > 16 {
			return false, errors.New("the installed kubeflow.org discovery is incomplete")
		}
		seen := map[string]bool{}
		for _, version := range group.Versions {
			gv := version.GroupVersion
			if gv == "" {
				gv = PodDefaults.Group + "/" + version.Version
			}
			if version.Version == "" || seen[gv] {
				return false, errors.New("the installed kubeflow.org discovery is incomplete")
			}
			seen[gv] = true
			versions = append(versions, gv)
		}
		break
	}
	hasBaselineV1 := false
	for _, gv := range versions {
		if gv == "kubeflow.org/v1" {
			hasBaselineV1 = true
			break
		}
	}
	if len(versions) == 0 || !hasBaselineV1 {
		// This application requires kubeflow.org/v1 Profiles and Notebooks.
		// An empty or baseline-less group result is therefore not trustworthy
		// positive evidence of PodDefault absence (for example a partial/nil
		// discovery result).
		return false, errors.New("PodDefault API group discovery is incomplete; references cannot be verified")
	}
	foundExpected := false
	foundOther := false
	for _, gv := range versions {
		list, err := discovery.ServerResourcesForGroupVersion(gv)
		if err != nil || list == nil {
			return false, errors.New("PodDefault API version discovery failed; references cannot be verified")
		}
		present, found, err := podDefaultResourceState(list, mutate)
		if err != nil {
			return false, err
		}
		if found && gv == expectedVersion {
			foundExpected = present
		}
		if found && gv != expectedVersion {
			foundOther = true
		}
	}
	if foundOther && !foundExpected {
		return false, errors.New("the installed PodDefault API is served only at an unsupported version")
	}
	return foundExpected, nil
}

// podDefaultResourceState inspects one targeted group-version list. The
// resource must be exactly PodDefault and namespaced; an absent resource in one
// successfully read version is not treated as proof for another version.
func podDefaultResourceState(list *metav1.APIResourceList, mutate bool) (present, found bool, err error) {
	if list == nil || len(list.APIResources) == 0 {
		return false, false, errors.New("PodDefault API discovery returned an empty resource list; references cannot be verified")
	}
	valid := 0
	for i := range list.APIResources {
		resource := &list.APIResources[i]
		if resource.Name != PodDefaults.Resource {
			continue
		}
		found = true
		if !resource.Namespaced || resource.Kind != "PodDefault" {
			return false, true, errors.New("the installed PodDefault API is incompatible with this adapter")
		}
		verbs := map[string]bool{}
		for _, verb := range resource.Verbs {
			verbs[verb] = true
		}
		for _, verb := range []string{"get", "list"} {
			if !verbs[verb] {
				return false, true, errors.New("the installed PodDefault API does not advertise the required read permissions")
			}
		}
		if mutate {
			for _, verb := range []string{"create", "patch", "delete"} {
				if !verbs[verb] {
					return false, true, errors.New("the installed PodDefault API does not advertise the required mutation permissions")
				}
			}
		}
		valid++
	}
	return found && valid > 0, found, nil
}

func (s *Service) requirePodDefaultMutation(ctx context.Context) error {
	present, err := s.podDefaultAPI(ctx, true)
	if err != nil {
		return err
	}
	if !present {
		return errors.New("the PodDefault API is not installed; PodDefault changes are unavailable")
	}
	return nil
}

func (s *Service) buildPodDefaultPlan(ctx context.Context, a model.Action, namespace, claim string, t *config.RegistrationTemplate, ev *evidence) (*podDefaultPlan, string, error) {
	if a.PodDefault == nil {
		return nil, "not-requested", nil
	}
	if !s.Config.Features.PodDefaults {
		return nil, "", errors.New("PodDefault creation is disabled")
	}
	if err := s.requirePodDefaultMutation(ctx); err != nil {
		return nil, "", err
	}
	name := a.PodDefault.Name
	if !validPodDefaultName(name) {
		return nil, "", errors.New("a valid PodDefault name is required")
	}
	if err := s.validPodDefaultMountPath(a.PodDefault.MountPath, t); err != nil {
		return nil, "", err
	}
	volumeName := podDefaultVolumeName(namespace, claim, name)
	spec := podDefaultSpec(volumeName, name, claim, a.PodDefault.MountPath)
	plan := &podDefaultPlan{
		namespace: namespace, claim: claim, name: name, mountPath: a.PodDefault.MountPath,
		volumeName: volumeName, spec: spec, overwrite: a.PodDefault.Overwrite, workflow: a.Kind,
		evidence: podDefaultEvidence{
			Name: name, ClaimName: claim, MountPathDigest: digest(a.PodDefault.MountPath),
			Overwrite: a.PodDefault.Overwrite, Desired: digest(spec),
		},
	}
	existing, err := s.Dynamic.Resource(PodDefaults).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		if existing.GetDeletionTimestamp() != nil {
			return nil, "", errors.New("existing PodDefault is terminating")
		}
		if existing.GetUID() == "" || existing.GetResourceVersion() == "" {
			return nil, "", errors.New("existing PodDefault has no stable UID/resourceVersion")
		}
		if !a.PodDefault.Overwrite {
			return nil, "", errors.New("a PodDefault with this name already exists; explicit overwrite acknowledgement is required")
		}
		usage, err := podDefaultUsageFrom(existing)
		if err != nil {
			return nil, "", errors.New("existing PodDefault could not be safely reviewed")
		}
		plan.existingUID = existing.GetUID()
		plan.existingRV = existing.GetResourceVersion()
		plan.existingHasAnnotation = existing.GetAnnotations() != nil
		plan.evidence.Existing = true
		plan.evidence.ExistingUID = string(existing.GetUID())
		plan.evidence.ExistingRV = existing.GetResourceVersion()
		plan.evidence.Shared = len(usage.otherPVCs(claim)) > 0 || usage.Additional
		plan.evidence.ExternalManaged = usage.External
		plan.evidence.AdditionalSettings = usage.Additional
		plan.evidence.OtherPVCs = usage.otherPVCs(claim)
		plan.changes = append(plan.changes,
			model.Change{Field: "PodDefault " + name, Before: namespace + "/" + name, After: "overwrite dedicated spec on the exact reviewed object"},
			model.Change{Field: "PodDefault " + name + " UID", Before: string(existing.GetUID()), After: "exact overwrite precondition"},
			model.Change{Field: "PodDefault selector", Before: "(existing selector)", After: name + "=true"},
			model.Change{Field: "PodDefault " + name + " resourceVersion", Before: existing.GetResourceVersion(), After: "exact overwrite precondition"},
			model.Change{Field: "PodDefault spec", Before: "(existing injected settings)", After: "replaced by one PVC volume and one mount; other settings are removed"},
			model.Change{Field: "PodDefault provenance", Before: "(existing console keys)", After: "updated to reviewed workflow and new PVC UID; foreign annotations preserved"},
			model.Change{Field: "PodDefault metadata", Before: "preserved", After: "preserved (labels, unrelated annotations, ownerReferences, finalizers)"},
		)
		if len(plan.evidence.OtherPVCs) > 0 {
			plan.changes = append(plan.changes, model.Change{Field: "other PVC references", Before: strings.Join(plan.evidence.OtherPVCs, ","), After: "removed; those PVCs are not deleted"})
		}
		plan.warnings = append(plan.warnings,
			"This overwrites the whole spec of the existing PodDefault named "+name+". Its previous selector, other PVC references and injected environment/settings are removed. Unrelated metadata, ownerReferences and finalizers are preserved.",
		)
		if len(plan.evidence.OtherPVCs) > 0 {
			plan.warnings = append(plan.warnings, "The existing PodDefault also references other PersistentVolumeClaims: "+strings.Join(plan.evidence.OtherPVCs, ", ")+". Overwriting it removes those settings from future matching Pods; those PVCs are never deleted or recreated by this operation.")
		}
		if usage.Additional {
			plan.warnings = append(plan.warnings, "The existing PodDefault carries additional injected settings or additional volumes/mounts beyond the dedicated shape. They are removed by this overwrite and are not reproduced.")
		}
		if usage.External {
			plan.warnings = append(plan.warnings, "The existing PodDefault carries owner references or common external-management metadata. It is not an ownership ban, but another manager such as Helm, Argo CD, Flux or a GitOps controller may reconcile it afterward. This console cannot prove exclusive ownership.")
		}
	case apierrors.IsNotFound(err):
		plan.changes = append(plan.changes,
			model.Change{Field: "PodDefault", Before: "(new)", After: namespace + "/" + name},
			model.Change{Field: "PodDefault selector", Before: "(new)", After: name + "=true"},
			model.Change{Field: "PodDefault volume", Before: "(new)", After: "PVC " + claim},
			model.Change{Field: "PodDefault mount path", Before: "(reviewed input)", After: "entered mount path (digest " + shortDigest(a.PodDefault.MountPath) + ")"},
			model.Change{Field: "PodDefault provenance", Before: "(new)", After: podDefaultWorkflowKey + " and " + podDefaultPVCUIDKey},
		)
		plan.warnings = append(plan.warnings, "Creates one dedicated PodDefault with exactly one PVC volume and one mount, selector "+name+"=true and provenance annotations naming the reviewed PVC UID. It injects no environment, Secret, container, service or additional PVC.")
		if a.PodDefault.Overwrite {
			plan.warnings = append(plan.warnings, "Overwrite was acknowledged, but no existing PodDefault with this name was observed; a new object will be created.")
		}
	default:
		return nil, "", safe(err)
	}
	plan.warnings = append(plan.warnings,
		"Creating or updating a PodDefault does not select it on any Notebook. Users must choose the corresponding configuration/label through the supported Notebook workflow; it applies only to matching Pods admitted afterward, never to existing mounts.",
		"PodDefault API discovery, dry-run and read-back are local evidence of API/schema acceptance only. Installed CRD/controller/webhook behavior, Notebook opt-in and Pod admission/mount success require independent live verification.",
	)
	plan.evidence.Shared = plan.evidence.Shared || len(plan.evidence.OtherPVCs) > 0 || plan.evidence.AdditionalSettings
	if err := s.podDefaultDryRun(ctx, plan); err != nil {
		return nil, "", err
	}
	if ev != nil {
		ev.add("PodDefault", namespace, name, podDefaultFingerprintFromPlan(plan))
	}
	state := "new"
	if plan.evidence.Existing {
		state = "existing"
	}
	return plan, state, nil
}

// attachPodDefault builds the optional dedicated PodDefault substep for a
// dynamic PVC creation. Registration binding uses its own builder so it can
// additionally enforce the selected template's mount-path prefix. Legacy
// combined static/reserve actions intentionally do not accept this substep.
func (s *Service) attachPodDefault(ctx context.Context, a model.Action, w *work) error {
	if a.PodDefault == nil {
		return nil
	}
	plan, _, err := s.buildPodDefaultPlan(ctx, a, a.Namespace, a.Name, nil, &w.ev)
	if err != nil {
		return err
	}
	w.podDefault = plan
	w.prepared.Changes = append(w.prepared.Changes, plan.changes...)
	w.prepared.Warnings = append(w.prepared.Warnings, plan.warnings...)
	w.prepared.Warnings = append(w.prepared.Warnings, "If PVC/PV creation succeeds but the optional PodDefault step fails, the storage object is retained. The operation reports an unfinished PodDefault step; it is never deleted as rollback and nothing is automatically replayed.")
	return nil
}

func validPodDefaultName(name string) bool {
	return name != "" && len(name) <= maxPodDefaultNameBytes && len(validation.IsDNS1123Subdomain(name)) == 0 && len(validation.IsQualifiedName(name)) == 0
}

func (s *Service) validPodDefaultMountPath(raw string, t *config.RegistrationTemplate) error {
	if raw == "" || len(raw) > maxRegistrationPath || !utf8.ValidString(raw) || strings.Contains(raw, "\\") || !safeMountPath(raw) {
		return errors.New("a safe absolute PodDefault mount path is required")
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return errors.New("PodDefault mount path contains control characters")
		}
	}
	if containsAmbiguousEncoding(raw) {
		return errors.New("PodDefault mount path contains an ambiguous encoding")
	}
	if t != nil && t.PodDefaultMountPathPrefix != "" {
		prefix := t.PodDefaultMountPathPrefix
		if raw == prefix || !pathWithin(prefix, raw) {
			return errors.New("PodDefault mount path must remain beneath the selected template's mount prefix")
		}
	}
	// The workspace/home root and its ancestors are always protected, even if
	// a site omits the conventional entry. A safe child such as
	// /home/jovyan/workspace remains allowed.
	baselineHome := "/home/jovyan"
	if raw == baselineHome || strings.HasPrefix(baselineHome, strings.TrimSuffix(raw, "/")+"/") {
		return errors.New("PodDefault mount path cannot replace the workspace home or an ancestor")
	}
	for _, protected := range append([]string{baselineHome}, s.Config.Notebooks.ProtectedMountPaths...) {
		if protected == baselineHome {
			continue
		}
		if raw == protected || strings.HasPrefix(raw, strings.TrimSuffix(protected, "/")+"/") || strings.HasPrefix(protected, strings.TrimSuffix(raw, "/")+"/") {
			return errors.New("PodDefault mount path conflicts with a protected mount path")
		}
	}
	return nil
}

func podDefaultSpec(volumeName, name, claim, mountPath string) map[string]any {
	return map[string]any{
		"desc": "Mounts PersistentVolumeClaim " + claim + " at " + mountPath + " (managed by kubeflow-admin).",
		"selector": map[string]any{
			"matchLabels": map[string]any{name: "true"},
		},
		"volumes": []any{
			map[string]any{
				"name":                  volumeName,
				"persistentVolumeClaim": map[string]any{"claimName": claim},
			},
		},
		"volumeMounts": []any{
			map[string]any{"name": volumeName, "mountPath": mountPath},
		},
	}
}

func podDefaultVolumeName(namespace, claim, podDefaultName string) string {
	h := sha256.Sum256([]byte(namespace + "\x00" + claim + "\x00" + podDefaultName))
	return "pvc-" + hex.EncodeToString(h[:6])
}

func (s *Service) podDefaultObject(namespace, workflow string, plan *podDefaultPlan, pvcUID types.UID) *unstructured.Unstructured {
	annotations := map[string]string{podDefaultWorkflowKey: workflow}
	if pvcUID != "" {
		annotations[podDefaultPVCUIDKey] = string(pvcUID)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubeflow.org/v1alpha1",
		"kind":       "PodDefault",
		"metadata":   map[string]any{"name": plan.name, "namespace": namespace, "annotations": stringMapAny(annotations)},
		"spec":       runtime.DeepCopyJSON(plan.spec),
	}}
}

func stringMapAny(in map[string]string) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (s *Service) podDefaultDryRun(ctx context.Context, plan *podDefaultPlan) error {
	opts := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	if plan.evidence.Existing {
		ops := podDefaultOverwriteOps(plan, "pending")
		b, err := json.Marshal(conditional(string(plan.existingUID), plan.existingRV, ops))
		if err != nil {
			return errors.New("PodDefault patch could not be encoded")
		}
		if _, err := s.Dynamic.Resource(PodDefaults).Namespace(plan.namespace).Patch(ctx, plan.name, types.JSONPatchType, b, metav1.PatchOptions{DryRun: opts.DryRun}); err != nil {
			return podDefaultDryRunError(err, "overwrite")
		}
		return nil
	}
	object := s.podDefaultObject(plan.namespace, plan.workflow, plan, "")
	if _, err := s.Dynamic.Resource(PodDefaults).Namespace(plan.namespace).Create(ctx, object, opts); err != nil {
		return podDefaultDryRunError(err, "create")
	}
	return nil
}

func podDefaultDryRunError(err error, operation string) error {
	switch {
	case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		return fmt.Errorf("the reviewed PodDefault changed before the %s dry-run; prepare a new plan", operation)
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return errors.New("Kubernetes API denied the PodDefault dry-run")
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return errors.New("the installed PodDefault API rejected the dedicated object schema")
	default:
		return errors.New("PodDefault dry-run could not be verified")
	}
}

func podDefaultOverwriteOps(plan *podDefaultPlan, pvcUID string) []patchOp {
	ops := []patchOp{}
	if plan.existingHasAnnotation {
		ops = append(ops,
			patchOp{"add", "/metadata/annotations/" + pointer(podDefaultWorkflowKey), plan.workflow},
			patchOp{"add", "/metadata/annotations/" + pointer(podDefaultPVCUIDKey), pvcUID},
		)
	} else {
		ops = append(ops, patchOp{"add", "/metadata/annotations", map[string]string{
			podDefaultWorkflowKey: plan.workflow,
			podDefaultPVCUIDKey:   pvcUID,
		}})
	}
	return append(ops, patchOp{"add", "/spec", plan.spec})
}

func (s *Service) revalidatePodDefaultBeforeWrite(ctx context.Context, plan *podDefaultPlan, created *corev1.PersistentVolumeClaim) error {
	if err := s.requirePodDefaultMutation(ctx); err != nil {
		return err
	}
	cur, err := s.Typed.CoreV1().PersistentVolumeClaims(plan.namespace).Get(ctx, plan.claim, metav1.GetOptions{})
	if err != nil {
		return errors.New("the created PersistentVolumeClaim could not be verified before the PodDefault step")
	}
	if cur.UID != created.UID || cur.DeletionTimestamp != nil {
		return ErrConflict
	}
	if plan.requireBound {
		if cur.Status.Phase != corev1.ClaimBound || cur.Spec.VolumeName != plan.selectedPV {
			return ErrConflict
		}
	} else if cur.Status.Phase == corev1.ClaimLost {
		return ErrConflict
	}
	existing, err := s.Dynamic.Resource(PodDefaults).Namespace(plan.namespace).Get(ctx, plan.name, metav1.GetOptions{})
	switch {
	case plan.evidence.Existing:
		if err != nil {
			return fmt.Errorf("the reviewed PodDefault changed before the overwrite: %w", ErrConflict)
		}
		if existing.GetUID() != plan.existingUID || existing.GetResourceVersion() != plan.existingRV || existing.GetDeletionTimestamp() != nil {
			return fmt.Errorf("the reviewed PodDefault changed before the overwrite: %w", ErrConflict)
		}
		if _, err := podDefaultUsageFrom(existing); err != nil {
			return errors.New("the reviewed PodDefault could not be safely re-read")
		}
	case apierrors.IsNotFound(err):
		// Still absent, as reviewed.
	case err != nil:
		return safe(err)
	default:
		return fmt.Errorf("a PodDefault with the reviewed new name appeared before creation: %w", ErrConflict)
	}
	return nil
}

func (s *Service) writePodDefault(ctx context.Context, plan *podDefaultPlan, created *corev1.PersistentVolumeClaim) error {
	plan.pvcUID = created.UID
	if plan.evidence.Existing {
		ops := podDefaultOverwriteOps(plan, string(created.UID))
		b, err := json.Marshal(conditional(string(plan.existingUID), plan.existingRV, ops))
		if err != nil {
			return errors.New("PodDefault patch could not be encoded")
		}
		r, err := s.Dynamic.Resource(PodDefaults).Namespace(plan.namespace).Patch(ctx, plan.name, types.JSONPatchType, b, metav1.PatchOptions{})
		if err != nil {
			return podDefaultMutationError(err, "overwrite")
		}
		if r == nil || r.GetUID() != plan.existingUID {
			return fmt.Errorf("PodDefault identity changed during the overwrite: %w", ErrConflict)
		}
		if err := recordMutationReceipt(ctx, mutationKindPodDefault, plan.namespace, plan.name, r.GetUID(), r.GetResourceVersion(), model.MutationOutcomeOverwritten); err != nil {
			return err
		}
		return nil
	}
	object := s.podDefaultObject(plan.namespace, plan.workflow, plan, created.UID)
	r, err := s.Dynamic.Resource(PodDefaults).Namespace(plan.namespace).Create(ctx, object, metav1.CreateOptions{})
	if err != nil {
		return podDefaultMutationError(err, "create")
	}
	if r == nil || r.GetUID() == "" {
		return errors.New("created PodDefault identity was not observed")
	}
	plan.existingUID = r.GetUID()
	plan.existingRV = r.GetResourceVersion()
	if err := recordMutationReceipt(ctx, mutationKindPodDefault, plan.namespace, plan.name, r.GetUID(), r.GetResourceVersion(), model.MutationOutcomeCreated); err != nil {
		return err
	}
	return nil
}

func podDefaultMutationError(err error, operation string) error {
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err), apierrors.IsConflict(err), apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return fmt.Errorf("Kubernetes API rejected the PodDefault %s step", operation)
	case apierrors.IsTooManyRequests(err), apierrors.IsServerTimeout(err), apierrors.IsTimeout(err), apierrors.IsServiceUnavailable(err), apierrors.IsInternalError(err):
		return fmt.Errorf("Kubernetes API PodDefault %s outcome is unknown", operation)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return fmt.Errorf("Kubernetes API PodDefault %s outcome is unknown", operation)
	}
}

func (s *Service) verifyPodDefault(ctx context.Context, plan *podDefaultPlan) error {
	cur, err := s.Dynamic.Resource(PodDefaults).Namespace(plan.namespace).Get(ctx, plan.name, metav1.GetOptions{})
	if err != nil {
		return errors.New("created or updated PodDefault could not be verified")
	}
	if plan.evidence.Existing {
		if cur.GetUID() != plan.existingUID {
			return fmt.Errorf("PodDefault was replaced during the overwrite: %w", ErrConflict)
		}
	} else if cur.GetUID() == "" || cur.GetUID() != plan.existingUID {
		return errors.New("PodDefault identity was not observed after creation")
	}
	spec, found, err := unstructured.NestedMap(cur.Object, "spec")
	if err != nil || !found || !reflect.DeepEqual(spec, plan.spec) {
		return errors.New("observed PodDefault spec does not match the reviewed dedicated shape")
	}
	annotations := cur.GetAnnotations()
	if annotations[podDefaultWorkflowKey] != plan.workflow || annotations[podDefaultPVCUIDKey] != string(plan.pvcUID) || plan.pvcUID == "" {
		return errors.New("observed PodDefault provenance annotations do not match the plan")
	}
	return nil
}

func podDefaultFingerprintFromPlan(plan *podDefaultPlan) any {
	return struct {
		Name    string
		UID     types.UID
		RV      string
		Spec    map[string]any
		Exists  bool
		Desired string
	}{plan.name, plan.existingUID, plan.existingRV, plan.spec, plan.evidence.Existing, plan.evidence.Desired}
}

// podDefaultUsageFrom inspects only the structure needed to identify PVC
// references and additional injected settings. It never returns or logs an
// environment value, Secret reference or arbitrary text. A malformed relevant
// structure is an error, never an empty reference set.
func podDefaultUsageFrom(obj *unstructured.Unstructured) (podDefaultUsage, error) {
	var usage podDefaultUsage
	if obj == nil {
		return usage, errors.New("missing PodDefault")
	}
	if kind := obj.GetKind(); kind != "" && kind != "PodDefault" {
		return usage, errors.New("unexpected object kind")
	}
	if version := obj.GetAPIVersion(); version != "" && version != "kubeflow.org/v1alpha1" {
		return usage, errors.New("unexpected API version")
	}
	spec, found, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil {
		return usage, errors.New("malformed PodDefault spec")
	}
	if !found || spec == nil {
		return usage, nil
	}
	for key := range spec {
		switch key {
		case "desc", "selector", "volumes", "volumeMounts":
		default:
			usage.Additional = true
		}
	}
	if raw, present := spec["desc"]; present {
		if _, ok := raw.(string); !ok {
			return usage, errors.New("malformed PodDefault description")
		}
	}
	if raw, present := spec["selector"]; present && raw != nil {
		selector, ok := raw.(map[string]any)
		if !ok {
			return usage, errors.New("malformed PodDefault selector")
		}
		for key := range selector {
			if key != "matchLabels" {
				usage.Additional = true
			}
		}
		labelsRaw, present := selector["matchLabels"]
		if present && labelsRaw != nil {
			labels, ok := labelsRaw.(map[string]any)
			if !ok {
				return usage, errors.New("malformed PodDefault selector labels")
			}
			dedicated := len(labels) == 1
			for k, v := range labels {
				if k != obj.GetName() {
					dedicated = false
				}
				s, ok := v.(string)
				if !ok || s != "true" {
					dedicated = false
				}
			}
			if !dedicated {
				usage.Additional = true
			}
		} else {
			usage.Additional = true
		}
	} else {
		usage.Additional = true
	}
	claims := map[string]bool{}
	volumeCount := 0
	pvcVolumeCount := 0
	volumeNames := map[string]bool{}
	if raw, present := spec["volumes"]; present && raw != nil {
		volumes, ok := raw.([]any)
		if !ok {
			return usage, errors.New("malformed PodDefault volumes")
		}
		volumeCount = len(volumes)
		for _, entry := range volumes {
			volume, ok := entry.(map[string]any)
			if !ok {
				return usage, errors.New("malformed PodDefault volume entry")
			}
			for key := range volume {
				if key != "name" && key != "persistentVolumeClaim" {
					usage.Additional = true
				}
			}
			if name, ok := volume["name"].(string); ok && name != "" {
				volumeNames[name] = true
			}
			rawClaim, present := volume["persistentVolumeClaim"]
			if !present || rawClaim == nil {
				usage.Additional = true
				continue
			}
			claim, ok := rawClaim.(map[string]any)
			if !ok {
				return usage, errors.New("malformed PodDefault persistentVolumeClaim")
			}
			for key := range claim {
				if key != "claimName" {
					usage.Additional = true
				}
			}
			claimName, ok := claim["claimName"].(string)
			if !ok || claimName == "" {
				return usage, errors.New("malformed PodDefault claimName")
			}
			pvcVolumeCount++
			claims[claimName] = true
		}
	} else {
		usage.Additional = true
	}
	mountCount := 0
	if raw, present := spec["volumeMounts"]; present && raw != nil {
		mounts, ok := raw.([]any)
		if !ok {
			return usage, errors.New("malformed PodDefault volumeMounts")
		}
		mountCount = len(mounts)
		for _, entry := range mounts {
			mount, ok := entry.(map[string]any)
			if !ok {
				return usage, errors.New("malformed PodDefault volumeMount entry")
			}
			for key := range mount {
				if key != "name" && key != "mountPath" && key != "readOnly" {
					usage.Additional = true
				}
			}
			if rawReadOnly, present := mount["readOnly"]; present {
				readOnly, ok := rawReadOnly.(bool)
				if !ok {
					return usage, errors.New("malformed PodDefault readOnly")
				}
				if readOnly {
					usage.Additional = true
				}
			}
			name, _ := mount["name"].(string)
			mountPath, _ := mount["mountPath"].(string)
			if name == "" || mountPath == "" || !volumeNames[name] {
				return usage, errors.New("malformed PodDefault volumeMount identity")
			}
		}
	} else {
		usage.Additional = true
	}
	if volumeCount != 1 || pvcVolumeCount != 1 || mountCount != 1 {
		usage.Additional = true
	}
	for claim := range claims {
		usage.Claims = append(usage.Claims, claim)
	}
	sort.Strings(usage.Claims)
	for _, key := range []string{
		"app.kubernetes.io/managed-by", "meta.helm.sh/release-name", "meta.helm.sh/release-namespace",
		"argocd.argoproj.io/instance", "kustomize.toolkit.fluxcd.io/name", "kustomize.toolkit.fluxcd.io/namespace",
		"kubectl.kubernetes.io/last-applied-configuration",
	} {
		if _, ok := obj.GetLabels()[key]; ok {
			usage.External = true
		}
		if _, ok := obj.GetAnnotations()[key]; ok {
			usage.External = true
		}
	}
	if len(obj.GetOwnerReferences()) > 0 {
		usage.External = true
	}
	return usage, nil
}

func podDefaultFingerprint(obj *unstructured.Unstructured) any {
	spec, _, _ := unstructured.NestedMap(obj.Object, "spec")
	return struct {
		UID  types.UID
		RV   string
		Spec map[string]any
	}{obj.GetUID(), obj.GetResourceVersion(), spec}
}

// podDefaultDependencies lists the exact namespace-local PodDefaults that
// reference the target claim. Genuine CRD absence returns an explicit "absent"
// state; every discovery/list/read error fails closed. A set larger than the
// cleanup bound is refused, never truncated.
func (s *Service) podDefaultDependencies(ctx context.Context, ns, claim string, a model.Action, ev *evidence) ([]podDefaultCleanupTarget, string, error) {
	present, err := s.podDefaultAPI(ctx, false)
	if err != nil {
		return nil, "", err
	}
	if !present {
		if ev != nil {
			ev.add("PodDefaultAPI", "", "", "absent")
		}
		return []podDefaultCleanupTarget{}, "absent", nil
	}
	if ev != nil {
		ev.add("PodDefaultAPI", "", "", "present")
	}
	list, err := s.Dynamic.Resource(PodDefaults).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, "", errors.New("PodDefault reference list could not be verified")
	}
	if list.GetContinue() != "" {
		return nil, "", errors.New("PodDefault reference list is incomplete")
	}
	targets := []podDefaultCleanupTarget{}
	for i := range list.Items {
		obj := &list.Items[i]
		usage, err := podDefaultUsageFrom(obj)
		if err != nil {
			return nil, "", errors.New("a PodDefault reference structure could not be verified")
		}
		if !containsString(usage.Claims, claim) {
			continue
		}
		if obj.GetDeletionTimestamp() != nil {
			return nil, "", errors.New("a related PodDefault is terminating; prepare a new plan after it is gone")
		}
		if obj.GetUID() == "" || obj.GetResourceVersion() == "" {
			return nil, "", errors.New("a related PodDefault has no stable UID/resourceVersion")
		}
		other := usage.otherPVCs(claim)
		t := podDefaultCleanupTarget{
			Name: obj.GetName(), Namespace: ns, UID: string(obj.GetUID()), ResourceVersion: obj.GetResourceVersion(),
			OtherPVCs: other, Shared: len(other) > 0 || usage.Additional, ExternalManaged: usage.External,
			AdditionalSettings: usage.Additional, Finalizers: len(obj.GetFinalizers()),
		}
		targets = append(targets, t)
		if ev != nil {
			ev.add("PodDefault", ns, obj.GetName(), podDefaultFingerprint(obj))
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Name < targets[j].Name })
	if len(targets) > maxPodDefaultCleanup {
		return nil, "", fmt.Errorf("the related PodDefault cleanup set exceeds the bound of %d; no objects were changed", maxPodDefaultCleanup)
	}
	if len(targets) == 0 {
		return targets, "present", nil
	}
	if !s.Config.Features.PodDefaults {
		return nil, "", errors.New("related PodDefaults exist but PodDefault cleanup is disabled")
	}
	if err := s.requirePodDefaultMutation(ctx); err != nil {
		return nil, "", err
	}
	if !a.ConfirmPodDefaultCleanup {
		return nil, "", errors.New("related PodDefaults require explicit cleanup acknowledgement")
	}
	for _, target := range targets {
		if err := s.podDefaultDeleteDryRun(ctx, target); err != nil {
			return nil, "", err
		}
	}
	return targets, "present", nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func podDefaultCleanupWarnings(targets []podDefaultCleanupTarget, claim string, acknowledged bool) []string {
	if len(targets) == 0 {
		if acknowledged {
			return []string{"No related PodDefault was observed; the cleanup acknowledgement had no effect."}
		}
		return nil
	}
	warnings := []string{
		"The reviewed namespace-local PodDefaults are deleted before the PersistentVolumeClaim. This is not a multi-object transaction: existing Pod mounts and Notebook template references are not changed, and an external writer or admission may interleave. If cleanup partially completes, it is not automatically replayed or rolled back.",
		"Removing a PodDefault only affects future matching Pods. It does not unmount existing Pods or remove Notebook template references.",
		"PodDefault deletion uses orphan propagation with exact UID/resourceVersion preconditions. Only the reviewed PodDefault object is deleted; any dependent objects are not cascade-deleted by this console. Other PVCs are never deleted or recreated.",
	}
	for _, t := range targets {
		warnings = append(warnings, "Related PodDefault "+t.Namespace+"/"+t.Name+" (UID "+t.UID+") injects PVC "+claim+" and will be deleted with exact UID/resourceVersion preconditions.")
		if len(t.OtherPVCs) > 0 {
			warnings = append(warnings, "Shared PodDefault "+t.Name+" also references other PVCs: "+strings.Join(t.OtherPVCs, ", ")+". Deleting it removes those injected mounts/settings from future matching Pods. Those other PVCs are never deleted or recreated.")
		}
		if t.AdditionalSettings {
			warnings = append(warnings, "PodDefault "+t.Name+" carries additional injected settings beyond the dedicated single-PVC shape. Deleting it removes them as well.")
		}
		if t.ExternalManaged {
			warnings = append(warnings, "PodDefault "+t.Name+" carries owner references or common external-management metadata. Another manager may recreate or reconcile it; deletion does not prove exclusive ownership.")
		}
		if t.Finalizers > 0 {
			warnings = append(warnings, "PodDefault "+t.Name+" has "+itoa(t.Finalizers)+" finalizer(s). If it remains terminating, the operation stops for review; finalizers are never removed and deletion is never forced.")
		}
	}
	return warnings
}

func podDefaultCleanupChanges(targets []podDefaultCleanupTarget) []model.Change {
	out := make([]model.Change, 0, len(targets))
	for _, t := range targets {
		out = append(out,
			model.Change{Field: "related PodDefault " + t.Name, Before: t.Namespace + "/" + t.Name, After: "delete before the PVC"},
			model.Change{Field: "related PodDefault " + t.Name + " UID", Before: t.UID, After: "exact delete precondition"},
			model.Change{Field: "related PodDefault " + t.Name + " resourceVersion", Before: t.ResourceVersion, After: "exact delete precondition"},
		)
		if len(t.OtherPVCs) > 0 {
			out = append(out, model.Change{Field: "shared PodDefault " + t.Name, Before: strings.Join(t.OtherPVCs, ","), After: "settings removed from future matching Pods; other PVCs untouched"})
		}
	}
	return out
}

func podDefaultDeleteOptions(uid types.UID, rv string, dry bool) metav1.DeleteOptions {
	orphan := metav1.DeletePropagationOrphan
	opts := metav1.DeleteOptions{
		Preconditions:     &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
		PropagationPolicy: &orphan,
	}
	if dry {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	return opts
}

func (s *Service) podDefaultDeleteDryRun(ctx context.Context, target podDefaultCleanupTarget) error {
	uid, rv := types.UID(target.UID), target.ResourceVersion
	if uid == "" || rv == "" {
		return errors.New("PodDefault deletion requires UID and resourceVersion preconditions")
	}
	err := s.Dynamic.Resource(PodDefaults).Namespace(target.Namespace).Delete(ctx, target.Name, podDefaultDeleteOptions(uid, rv, true))
	if err == nil {
		return nil
	}
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return errors.New("Kubernetes API denied the PodDefault cleanup dry-run")
	case apierrors.IsNotFound(err), apierrors.IsConflict(err):
		return fmt.Errorf("the reviewed PodDefault changed before cleanup: %w", ErrConflict)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return errors.New("Kubernetes API rejected the PodDefault cleanup dry-run")
	default:
		return errors.New("PodDefault cleanup dry-run could not be verified")
	}
}

func (s *Service) deletePodDefault(ctx context.Context, target podDefaultCleanupTarget) error {
	uid, rv := types.UID(target.UID), target.ResourceVersion
	if uid == "" || rv == "" {
		return errors.New("PodDefault deletion requires UID and resourceVersion preconditions")
	}
	if err := s.Dynamic.Resource(PodDefaults).Namespace(target.Namespace).Delete(ctx, target.Name, podDefaultDeleteOptions(uid, rv, false)); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("the reviewed PodDefault disappeared before deletion: %w", ErrConflict)
		}
		return podDefaultMutationError(err, "delete")
	}
	if err := s.waitForPodDefaultDeletion(ctx, target); err != nil {
		return err
	}
	return recordMutationReceipt(ctx, mutationKindPodDefault, target.Namespace, target.Name, types.UID(target.UID), target.ResourceVersion, model.MutationOutcomeDeleted)
}

func (s *Service) waitForPodDefaultDeletion(ctx context.Context, target podDefaultCleanupTarget) error {
	absent := 0
	return s.wait(ctx, func() (bool, error) {
		obj, err := s.Dynamic.Resource(PodDefaults).Namespace(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			absent++
			return absent >= 2, nil
		}
		if err != nil {
			return false, errors.New("PodDefault deletion state could not be verified")
		}
		absent = 0
		if string(obj.GetUID()) != target.UID {
			return false, fmt.Errorf("a replacement PodDefault appeared during cleanup: %w", ErrConflict)
		}
		return false, nil
	})
}

// assertOnlyPlannedPodDefaults re-lists related PodDefaults and requires the
// observed set to equal the remaining reviewed set, by name, UID and
// resourceVersion. A new reference or a changed/replaced reviewed object is a
// conflict; it is never discovered-and-deleted during execution.
func (s *Service) assertOnlyPlannedPodDefaults(ctx context.Context, ns, claim string, remaining []podDefaultCleanupTarget) error {
	present, err := s.podDefaultAPI(ctx, false)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("the reviewed PodDefault API disappeared before cleanup: %w", ErrConflict)
	}
	list, err := s.Dynamic.Resource(PodDefaults).Namespace(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errors.New("PodDefault reference list could not be verified before cleanup")
	}
	if list.GetContinue() != "" {
		return errors.New("PodDefault reference list is incomplete before cleanup")
	}
	expected := map[string]podDefaultCleanupTarget{}
	for _, target := range remaining {
		expected[target.Name] = target
	}
	seen := map[string]bool{}
	for i := range list.Items {
		obj := &list.Items[i]
		usage, err := podDefaultUsageFrom(obj)
		if err != nil {
			return errors.New("a PodDefault reference structure could not be verified before cleanup")
		}
		if !containsString(usage.Claims, claim) {
			continue
		}
		want, ok := expected[obj.GetName()]
		if !ok {
			return fmt.Errorf("an unreviewed PodDefault reference appeared after preview: %w", ErrConflict)
		}
		if string(obj.GetUID()) != want.UID || obj.GetResourceVersion() != want.ResourceVersion {
			return fmt.Errorf("a reviewed PodDefault changed after preview: %w", ErrConflict)
		}
		seen[obj.GetName()] = true
	}
	for name := range expected {
		if !seen[name] {
			return fmt.Errorf("a reviewed PodDefault disappeared after preview: %w", ErrConflict)
		}
	}
	return nil
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}
