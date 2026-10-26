package kube

// storage_edit.go implements the two narrow storage workflows added to the
// console:
//
//   - pv.edit / pvc.edit: a restricted, server-normalized YAML projection and
//     patch of approved fields only. PersistentVolumes change only the reclaim
//     policy and console-owned labels while they are Available and unclaimed.
//     PersistentVolumeClaims change only the requested storage (increase) and
//     console-owned labels.
//   - pv.delete / pvc.delete: unused-only deletion with exact UID and
//     resourceVersion preconditions, one request, no automatic replay and no
//     finalizer or force handling.
//
// The full object is never accepted or emitted. Backend volume sources, CSI
// handles and attributes, NFS paths, Secrets, annotations, finalizers, status,
// owner references, StorageClass and claim references stay hidden and are
// preserved by patching only the approved field paths. The projection parser is
// deliberately strict: one document, no aliases/anchors, no duplicate keys, no
// unknown fields and a hard 32 KiB bound.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	"kubeflow-admin/internal/model"
)

const (
	// StorageLabelPrefix is the only label prefix this console may create,
	// change or remove. All other labels and every annotation are preserved.
	StorageLabelPrefix = "kubeflow-admin.io/"

	storageDocumentMaxBytes = 32 << 10
	storageLabelMaxCount    = 32
	storageLabelMaxBytes    = 4 << 10

	storageKindPV  = "PersistentVolume"
	storageKindPVC = "PersistentVolumeClaim"

	storageEditStep    = "apply-storage-edit"
	storageDeleteStep  = "delete-storage"
	storageReclaimStep = "reclaim-volume"
	storageAbsent      = "(absent)"
)

var errStorageDocumentInvalid = errors.New("storage YAML is invalid; use the server-provided projection and approved fields only")

// storageProjection is the complete set of fields accepted from a browser. It
// is also the shape this adapter emits. No field carries a volume source,
// backend handle, Secret reference, status or unapproved metadata.
type storageProjection struct {
	APIVersion string                    `yaml:"apiVersion" json:"apiVersion"`
	Kind       string                    `yaml:"kind" json:"kind"`
	Metadata   storageProjectionMetadata `yaml:"metadata" json:"metadata"`
	Spec       *storageProjectionSpec    `yaml:"spec,omitempty" json:"spec,omitempty"`
}

type storageProjectionMetadata struct {
	Name            string             `yaml:"name" json:"name"`
	Namespace       string             `yaml:"namespace,omitempty" json:"namespace,omitempty"`
	UID             string             `yaml:"uid" json:"uid"`
	ResourceVersion string             `yaml:"resourceVersion" json:"resourceVersion"`
	Labels          *map[string]string `yaml:"labels,omitempty" json:"labels,omitempty"`
}

type storageProjectionSpec struct {
	PersistentVolumeReclaimPolicy string                      `yaml:"persistentVolumeReclaimPolicy,omitempty" json:"persistentVolumeReclaimPolicy,omitempty"`
	Resources                     *storageProjectionResources `yaml:"resources,omitempty" json:"resources,omitempty"`
}

type storageProjectionResources struct {
	Requests storageProjectionRequests `yaml:"requests" json:"requests"`
}

type storageProjectionRequests struct {
	Storage string `yaml:"storage" json:"storage"`
}

// StorageDocument returns the safe projection of one PersistentVolume or
// PersistentVolumeClaim. kind accepts "pv", "PersistentVolume" or the typed
// "pv.edit"/"pv.delete" spelling (and the corresponding PVC spellings); a PV
// must be passed with an empty namespace. The returned YAML contains only the
// API identity, UID, resourceVersion, console-owned labels and the one
// approved editable value. Warnings explain what is hidden and preserved.
func (s *Service) StorageDocument(ctx context.Context, kind, namespace, objectName string) (model.StorageDocument, error) {
	if !s.Config.Features.WritesEnabled || (!s.Config.Features.StorageEditing && !s.Config.Features.StorageDeletion) {
		return model.StorageDocument{}, errors.New("storage editing and deletion are disabled")
	}
	canonical, err := storageKindName(kind)
	if err != nil {
		return model.StorageDocument{}, err
	}
	if err := name(objectName); err != nil {
		return model.StorageDocument{}, err
	}
	if canonical == storageKindPV {
		if namespace != "" {
			return model.StorageDocument{}, errors.New("PersistentVolume is cluster scoped; namespace must be empty")
		}
		ev := s.newEvidence()
		pv, _, err := s.storagePVTarget(ctx, objectName, &ev)
		if err != nil {
			return model.StorageDocument{}, err
		}
		projection, err := storageProjectionFromPV(pv)
		if err != nil {
			return model.StorageDocument{}, err
		}
		doc := storageDocumentProjection(projection)
		y, err := marshalStorageProjection(doc)
		if err != nil {
			return model.StorageDocument{}, err
		}
		return model.StorageDocument{Kind: storageKindPV, Name: pv.Name, UID: string(pv.UID), ResourceVersion: pv.ResourceVersion, YAML: y, Warnings: storagePVWarnings(string(pv.Spec.PersistentVolumeReclaimPolicy), "document")}, nil
	}
	if err := s.namespacePolicy(namespace); err != nil {
		return model.StorageDocument{}, err
	}
	ev := s.newEvidence()
	if err := s.scope(ctx, namespace, &ev); err != nil {
		return model.StorageDocument{}, err
	}
	pvc, err := s.Typed.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, objectName, metav1.GetOptions{})
	if err != nil {
		return model.StorageDocument{}, safe(err)
	}
	if pvc.UID == "" || pvc.ResourceVersion == "" {
		return model.StorageDocument{}, errors.New("PersistentVolumeClaim has no stable UID/resourceVersion")
	}
	if pvc.DeletionTimestamp != nil {
		return model.StorageDocument{}, errors.New("PersistentVolumeClaim is terminating")
	}
	proj, err := storageProjectionFromPVC(pvc)
	if err != nil {
		return model.StorageDocument{}, err
	}
	doc := storageDocumentProjection(proj)
	y, err := marshalStorageProjection(doc)
	if err != nil {
		return model.StorageDocument{}, err
	}
	warnings := []string{
		"Only apiVersion, kind, name, namespace, uid, resourceVersion, kubeflow-admin.io/ labels and spec.resources.requests.storage are shown. Volume sources, StorageClass, annotations, finalizers, owner references, status and all other fields are hidden and preserved.",
		"An omitted or null metadata.labels field leaves existing console labels unchanged. An explicit metadata.labels: {} removes all console labels. A nonempty labels map is the complete desired console-owned set: keys omitted from it are removed. At most 32 labels and 4 KiB of key+value text are accepted.",
		"Storage can be increased only when the bound claim, CSI binding, approved StorageClass with allowVolumeExpansion and observed quota headroom all still hold. Decreasing or repeating the size is refused.",
	}
	if pvc.Spec.VolumeName != "" {
		warnings = append(warnings, "This claim is bound. Deleting it is allowed only while no observed Pod or Notebook references it and the bound PersistentVolume still names this exact claim.")
	}
	return model.StorageDocument{Kind: storageKindPVC, Namespace: pvc.Namespace, Name: pvc.Name, UID: string(pvc.UID), ResourceVersion: pvc.ResourceVersion, YAML: y, Warnings: warnings}, nil
}

// storageDocumentProjection returns a deep copy that carries no field outside
// the approved projection. It is used both for browser reads and for the
// normalized action that is persisted in a plan.
func storageDocumentProjection(p storageProjection) storageProjection {
	out := storageProjection{
		APIVersion: "v1",
		Kind:       p.Kind,
		Metadata: storageProjectionMetadata{
			Name:            p.Metadata.Name,
			Namespace:       p.Metadata.Namespace,
			UID:             p.Metadata.UID,
			ResourceVersion: p.Metadata.ResourceVersion,
			Labels:          copyStorageLabelPtr(p.Metadata.Labels),
		},
	}
	if p.Spec != nil {
		spec := &storageProjectionSpec{PersistentVolumeReclaimPolicy: p.Spec.PersistentVolumeReclaimPolicy}
		if p.Spec.Resources != nil {
			spec.Resources = &storageProjectionResources{Requests: storageProjectionRequests{Storage: p.Spec.Resources.Requests.Storage}}
		}
		out.Spec = spec
	}
	return out
}

func storageProjectionFromPV(pv *corev1.PersistentVolume) (storageProjection, error) {
	labels, err := consoleStorageLabels(pv.Labels)
	if err != nil {
		return storageProjection{}, err
	}
	return storageProjection{
		APIVersion: "v1",
		Kind:       storageKindPV,
		Metadata: storageProjectionMetadata{
			Name:            pv.Name,
			UID:             string(pv.UID),
			ResourceVersion: pv.ResourceVersion,
			Labels:          storageLabelPtr(labels),
		},
		Spec: &storageProjectionSpec{PersistentVolumeReclaimPolicy: string(pv.Spec.PersistentVolumeReclaimPolicy)},
	}, nil
}

func storageProjectionFromPVC(pvc *corev1.PersistentVolumeClaim) (storageProjection, error) {
	labels, err := consoleStorageLabels(pvc.Labels)
	if err != nil {
		return storageProjection{}, err
	}
	out := storageProjection{
		APIVersion: "v1",
		Kind:       storageKindPVC,
		Metadata: storageProjectionMetadata{
			Name:            pvc.Name,
			Namespace:       pvc.Namespace,
			UID:             string(pvc.UID),
			ResourceVersion: pvc.ResourceVersion,
			Labels:          storageLabelPtr(labels),
		},
	}
	if size := pvc.Spec.Resources.Requests.Storage(); size != nil {
		out.Spec = &storageProjectionSpec{Resources: &storageProjectionResources{Requests: storageProjectionRequests{Storage: size.String()}}}
	}
	return out, nil
}

func copyStorageLabels(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func copyStorageLabelPtr(in *map[string]string) *map[string]string {
	if in == nil {
		return nil
	}
	c := copyStorageLabels(*in)
	if c == nil {
		c = map[string]string{}
	}
	return &c
}

// storageLabelPtr preserves the presence of a label map. A nil map means the
// YAML field is omitted; a nonnil empty map means the administrator explicitly
// wrote "labels: {}" and wants every console-owned label removed.
func storageLabelPtr(in map[string]string) *map[string]string {
	if in == nil {
		return nil
	}
	c := copyStorageLabels(in)
	if c == nil {
		c = map[string]string{}
	}
	return &c
}

// consoleStorageLabels filters a live label map down to the keys this console
// owns and validates every such key/value before it is projected. A malformed
// console label is refused instead of being silently dropped or rewritten.
func consoleStorageLabels(labels map[string]string) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range labels {
		if !strings.HasPrefix(k, StorageLabelPrefix) {
			continue
		}
		if err := validateStorageLabel(k, v); err != nil {
			return nil, errors.New("an existing kubeflow-admin.io/ label is invalid; an operator must correct it before editing")
		}
		out[k] = v
	}
	if err := validateStorageLabelSet(out); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func validateStorageLabel(key, value string) error {
	if !strings.HasPrefix(key, StorageLabelPrefix) || len(validation.IsQualifiedName(key)) != 0 || len(validation.IsValidLabelValue(value)) != 0 {
		return errors.New("only valid labels under the kubeflow-admin.io/ prefix are editable")
	}
	return nil
}

func validateStorageLabelSet(labels map[string]string) error {
	if len(labels) > storageLabelMaxCount {
		return errors.New("too many kubeflow-admin.io/ labels")
	}
	total := 0
	for k, v := range labels {
		if err := validateStorageLabel(k, v); err != nil {
			return err
		}
		total += len(k) + len(v)
	}
	if total > storageLabelMaxBytes {
		return errors.New("kubeflow-admin.io/ labels exceed the allowed size")
	}
	return nil
}

// marshalStorageProjection renders a deterministic safe projection. yaml.v3
// sorts map keys, so the same accepted document always produces the same plan
// evidence.
func marshalStorageProjection(p storageProjection) (string, error) {
	p = storageDocumentProjection(p)
	b, err := yaml.Marshal(p)
	if err != nil {
		return "", errors.New("storage projection could not be rendered")
	}
	if len(b) > storageDocumentMaxBytes {
		return "", errors.New("storage projection exceeds the allowed size")
	}
	return string(b), nil
}

// parseStorageProjection strictly parses the browser-provided projection. It
// never echoes the document or a decoder value in an error.
func parseStorageProjection(raw string) (storageProjection, error) {
	var out storageProjection
	if strings.TrimSpace(raw) == "" || len(raw) > storageDocumentMaxBytes {
		return out, errStorageDocumentInvalid
	}
	// First pass: exactly one document, no aliases/anchors/custom tags, no
	// duplicate mapping keys anywhere.
	dec := yaml.NewDecoder(bytes.NewReader([]byte(raw)))
	documents := 0
	for {
		var node yaml.Node
		err := dec.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, errStorageDocumentInvalid
		}
		documents++
		if documents > 1 || validateStorageYAMLNode(&node) != nil {
			return out, errStorageDocumentInvalid
		}
	}
	if documents != 1 {
		return out, errStorageDocumentInvalid
	}
	// Second pass: typed decode with unknown-field rejection. This is what
	// refuses spec.volumeName, status, annotations, claimRef and every other
	// field outside the projection.
	typed := yaml.NewDecoder(bytes.NewReader([]byte(raw)))
	typed.KnownFields(true)
	if err := typed.Decode(&out); err != nil {
		return storageProjection{}, errStorageDocumentInvalid
	}
	var extra any
	if err := typed.Decode(&extra); !errors.Is(err, io.EOF) {
		return storageProjection{}, errStorageDocumentInvalid
	}
	if out.APIVersion != "v1" || (out.Kind != storageKindPV && out.Kind != storageKindPVC) {
		return storageProjection{}, errStorageDocumentInvalid
	}
	if out.Metadata.Labels != nil {
		if err := validateStorageLabelSet(*out.Metadata.Labels); err != nil {
			return storageProjection{}, err
		}
	}
	return out, nil
}

// validateStorageYAMLNode walks the raw YAML tree. Anchors, aliases, merge
// keys, custom tags and duplicate keys are all rejected: none of them is needed
// for a small flat projection, and each makes the accepted document ambiguous.
func validateStorageYAMLNode(n *yaml.Node) error {
	if n == nil {
		return errStorageDocumentInvalid
	}
	if n.Kind == yaml.AliasNode || n.Anchor != "" {
		return errStorageDocumentInvalid
	}
	if n.Tag != "" && !strings.HasPrefix(n.Tag, "!!") {
		return errStorageDocumentInvalid
	}
	switch n.Kind {
	case yaml.DocumentNode:
		if len(n.Content) != 1 {
			return errStorageDocumentInvalid
		}
		return validateStorageYAMLNode(n.Content[0])
	case yaml.MappingNode:
		if len(n.Content)%2 != 0 {
			return errStorageDocumentInvalid
		}
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return errStorageDocumentInvalid
			}
			if err := validateStorageYAMLNode(key); err != nil {
				return err
			}
			if key.Value == "<<" {
				return errStorageDocumentInvalid
			}
			if seen[key.Value] {
				return errStorageDocumentInvalid
			}
			seen[key.Value] = true
			if err := validateStorageYAMLNode(n.Content[i+1]); err != nil {
				return err
			}
		}
		return nil
	case yaml.SequenceNode:
		for _, child := range n.Content {
			if err := validateStorageYAMLNode(child); err != nil {
				return err
			}
		}
		return nil
	case yaml.ScalarNode:
		return nil
	default:
		return errStorageDocumentInvalid
	}
}

// storageKindName maps the public kind spellings onto the two canonical
// Kubernetes kinds.
func storageKindName(kind string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "pv", "pv.edit", "pv.delete", "persistentvolume":
		return storageKindPV, nil
	case "pvc", "pvc.edit", "pvc.delete", "persistentvolumeclaim":
		return storageKindPVC, nil
	}
	return "", errors.New("unsupported storage kind")
}

func storageKindForAction(a model.Action) (string, error) {
	switch a.Kind {
	case "pv.edit", "pv.delete":
		return storageKindPV, nil
	case "pvc.edit", "pvc.delete":
		return storageKindPVC, nil
	}
	return "", errors.New("unsupported storage action kind")
}

func storageRef(kind, namespace, name string) string {
	if kind == storageKindPV {
		return name
	}
	return namespace + "/" + name
}

// storageActionFields is a reflective allowlist. A field added to model.Action
// later is rejected by default instead of silently widening these actions.
func storageActionFields(a model.Action, allowed ...string) error {
	ok := map[string]bool{}
	for _, field := range allowed {
		ok[field] = true
	}
	v := reflect.ValueOf(a)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if ok[t.Field(i).Name] {
			continue
		}
		if !v.Field(i).IsZero() {
			return errors.New("storage action accepts no other fields")
		}
	}
	return nil
}

func storageEditActionConstraints(a model.Action) error {
	if a.Force || a.ConfirmName != "" {
		return errors.New("storage editing does not accept force or a delete confirmation")
	}
	if err := storageActionFields(a, "Kind", "Namespace", "Name", "Reason", "StorageYAML", "TargetUID", "TargetResourceVersion"); err != nil {
		return err
	}
	if a.TargetUID == "" || a.TargetResourceVersion == "" {
		return errors.New("storage editing requires the exact observed targetUID and targetResourceVersion")
	}
	if strings.TrimSpace(a.StorageYAML) == "" || len(a.StorageYAML) > storageDocumentMaxBytes {
		return errors.New("a bounded storage projection is required")
	}
	if a.Reason != "" && len([]rune(a.Reason)) > 500 {
		return errors.New("reason exceeds 500 characters")
	}
	kind, err := storageKindForAction(a)
	if err != nil {
		return err
	}
	if kind == storageKindPV {
		if a.Namespace != "" {
			return errors.New("PersistentVolume is cluster scoped; namespace must be empty")
		}
		return name(a.Name)
	}
	if err := name(a.Name); err != nil {
		return err
	}
	return sNamespacePolicyErr(a.Namespace)
}

// sNamespacePolicyErr is a pure grammar check used by action constraints.
// The configured policy is still checked by the cluster-aware preparation.
func sNamespacePolicyErr(namespace string) error {
	if len(validation.IsDNS1123Label(namespace)) != 0 {
		return errors.New("invalid namespace name")
	}
	return nil
}

func storageDeleteActionConstraints(a model.Action) error {
	if a.Force || a.StorageYAML != "" {
		return errors.New("storage deletion does not accept force or an edit document")
	}
	allowed := []string{"Kind", "Namespace", "Name", "Reason", "ConfirmName", "TargetUID", "TargetResourceVersion"}
	if a.Kind == "pvc.delete" {
		// Cleanup acknowledgement exists only on the PVC deletion workflow.
		// A PV deletion carrying it is refused by the reflective allowlist.
		allowed = append(allowed, "ConfirmPodDefaultCleanup", "ConfirmPVReclaim")
	}
	if err := storageActionFields(a, allowed...); err != nil {
		return err
	}
	if a.TargetUID == "" || a.TargetResourceVersion == "" {
		return errors.New("storage deletion requires the exact observed targetUID and targetResourceVersion")
	}
	if strings.TrimSpace(a.Reason) == "" || len([]rune(a.Reason)) > 500 {
		return errors.New("a nonempty reason of at most 500 characters is required for storage deletion")
	}
	if a.ConfirmName != a.Name {
		return errors.New("typed confirmation must exactly match the storage object name")
	}
	kind, err := storageKindForAction(a)
	if err != nil {
		return err
	}
	if kind == storageKindPV {
		if a.Namespace != "" {
			return errors.New("PersistentVolume is cluster scoped; namespace must be empty")
		}
		return name(a.Name)
	}
	if err := name(a.Name); err != nil {
		return err
	}
	return sNamespacePolicyErr(a.Namespace)
}

// storageEditPlan is the internal, non-persisted execution plan of an edit.
type storageEditPlan struct {
	prepared model.Prepared
	kind     string
	pv       *corev1.PersistentVolume
	pvc      *corev1.PersistentVolumeClaim
	ops      []patchOp
	uid      types.UID
	rv       string
	class    string
	size     *resource.Quantity
	reclaim  string
	labels   map[string]string
	removed  []string
	expand   bool
}

func (s *Service) prepareStorageEdit(ctx context.Context, a model.Action) (model.Prepared, error) {
	plan, err := s.buildStorageEdit(ctx, a)
	if err != nil {
		return model.Prepared{}, err
	}
	return plan.prepared, nil
}

func (s *Service) buildStorageEdit(ctx context.Context, a model.Action) (*storageEditPlan, error) {
	if !s.Config.Features.WritesEnabled {
		return nil, errors.New("writes are disabled")
	}
	if !s.Config.Features.StorageEditing {
		return nil, errors.New("storage editing is disabled")
	}
	if err := storageEditActionConstraints(a); err != nil {
		return nil, err
	}
	kind, _ := storageKindForAction(a)
	proj, err := parseStorageProjection(a.StorageYAML)
	if err != nil {
		return nil, err
	}
	if proj.Kind != kind || proj.Metadata.Name != a.Name {
		return nil, errors.New("storage projection does not match the action target")
	}
	if proj.Metadata.UID == "" || proj.Metadata.UID != a.TargetUID || proj.Metadata.ResourceVersion == "" || proj.Metadata.ResourceVersion != a.TargetResourceVersion {
		return nil, fmt.Errorf("storage projection identity or resourceVersion is stale: %w", ErrConflict)
	}
	if kind == storageKindPV {
		return s.buildPVEdit(ctx, a, proj)
	}
	return s.buildPVCEdit(ctx, a, proj)
}

func (s *Service) buildPVEdit(ctx context.Context, a model.Action, proj storageProjection) (*storageEditPlan, error) {
	if proj.Metadata.Namespace != "" {
		return nil, errors.New("PersistentVolume projection must not carry a namespace")
	}
	if proj.Spec == nil || proj.Spec.Resources != nil {
		return nil, errors.New("PersistentVolume editing accepts only the reclaim policy and console-owned labels")
	}
	if proj.Spec.PersistentVolumeReclaimPolicy != string(corev1.PersistentVolumeReclaimRetain) && proj.Spec.PersistentVolumeReclaimPolicy != string(corev1.PersistentVolumeReclaimDelete) {
		return nil, errors.New("PersistentVolume reclaim policy must be Retain or Delete")
	}
	ev := s.newEvidence()
	pv, class, err := s.storagePVTarget(ctx, a.Name, &ev)
	if err != nil {
		return nil, err
	}
	if string(pv.UID) != a.TargetUID || pv.ResourceVersion != a.TargetResourceVersion {
		return nil, fmt.Errorf("PersistentVolume changed before the edit: %w", ErrConflict)
	}
	currentLabels, err := consoleStorageLabels(pv.Labels)
	if err != nil {
		return nil, err
	}
	explicitLabels := proj.Metadata.Labels != nil
	desiredLabels := currentLabels
	if explicitLabels {
		desiredLabels = *proj.Metadata.Labels
	}
	normalizedLabels := (*map[string]string)(nil)
	if explicitLabels || len(desiredLabels) > 0 {
		normalizedLabels = storageLabelPtr(desiredLabels)
	}
	ops, removed, changes := storageLabelPatch(pv.Labels, desiredLabels)
	reclaim := proj.Spec.PersistentVolumeReclaimPolicy
	if reclaim != string(pv.Spec.PersistentVolumeReclaimPolicy) {
		ops = append(ops, patchOp{Op: "replace", Path: "/spec/persistentVolumeReclaimPolicy", Value: reclaim})
		changes = append(changes, model.Change{Field: "reclaim policy", Before: string(pv.Spec.PersistentVolumeReclaimPolicy), After: reclaim})
	}
	if len(ops) == 0 {
		return nil, errors.New("no approved PersistentVolume change was requested")
	}
	desired := storageDocumentProjection(storageProjection{
		Kind: storageKindPV,
		Metadata: storageProjectionMetadata{
			Name: pv.Name, UID: string(pv.UID), ResourceVersion: pv.ResourceVersion, Labels: normalizedLabels,
		},
		Spec: &storageProjectionSpec{PersistentVolumeReclaimPolicy: reclaim},
	})
	normalized, err := marshalStorageProjection(desired)
	if err != nil {
		return nil, err
	}
	a.StorageYAML = normalized
	evd := storageEditEvidence{
		Version: 1, Policy: s.newEvidence().Policy, Kind: storageKindPV,
		Namespace: "", Name: pv.Name, UID: string(pv.UID), ResourceVersion: pv.ResourceVersion,
		Desired: digest(desired), StorageClass: pv.Spec.StorageClassName, ClassDigest: digest(class),
		ReclaimPolicy: reclaim, Labels: digest(desiredLabels),
	}
	data, err := json.Marshal(evd)
	if err != nil {
		return nil, errors.New("storage edit evidence could not be encoded")
	}
	p := model.Prepared{
		Action: a, TargetUID: string(pv.UID), ResourceVersion: pv.ResourceVersion,
		Summary: "pv.edit " + pv.Name, Data: data, Changes: changes,
	}
	p.Warnings = storageEditWarnings(reclaim, false)
	if err := s.storageEditDryRun(ctx, storageKindPV, "", pv.Name, pv.UID, pv.ResourceVersion, ops); err != nil {
		return nil, err
	}
	return &storageEditPlan{prepared: p, kind: storageKindPV, pv: pv, ops: ops, uid: pv.UID, rv: pv.ResourceVersion, class: pv.Spec.StorageClassName, reclaim: reclaim, labels: desiredLabels, removed: removed}, nil
}

func (s *Service) buildPVCEdit(ctx context.Context, a model.Action, proj storageProjection) (*storageEditPlan, error) {
	if proj.Metadata.Namespace != a.Namespace {
		return nil, errors.New("PersistentVolumeClaim projection namespace does not match the action")
	}
	if proj.Spec != nil && proj.Spec.PersistentVolumeReclaimPolicy != "" {
		return nil, errors.New("PersistentVolumeClaim editing does not accept a reclaim policy")
	}
	ev := s.newEvidence()
	if err := s.scope(ctx, a.Namespace, &ev); err != nil {
		return nil, err
	}
	pvc, err := s.Typed.CoreV1().PersistentVolumeClaims(a.Namespace).Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		return nil, safe(err)
	}
	if pvc.DeletionTimestamp != nil {
		return nil, errors.New("PersistentVolumeClaim is terminating")
	}
	if string(pvc.UID) != a.TargetUID || pvc.ResourceVersion != a.TargetResourceVersion {
		return nil, fmt.Errorf("PersistentVolumeClaim changed before the edit: %w", ErrConflict)
	}
	currentLabels, err := consoleStorageLabels(pvc.Labels)
	if err != nil {
		return nil, err
	}
	explicitLabels := proj.Metadata.Labels != nil
	desiredLabels := currentLabels
	if explicitLabels {
		desiredLabels = *proj.Metadata.Labels
	}
	normalizedLabels := (*map[string]string)(nil)
	if explicitLabels || len(desiredLabels) > 0 {
		normalizedLabels = storageLabelPtr(desiredLabels)
	}
	ops, removed, changes := storageLabelPatch(pvc.Labels, desiredLabels)

	current := pvc.Spec.Resources.Requests.Storage()
	var desiredSize *resource.Quantity
	if proj.Spec != nil && proj.Spec.Resources != nil {
		raw := proj.Spec.Resources.Requests.Storage
		if strings.TrimSpace(raw) == "" {
			return nil, errors.New("spec.resources.requests.storage must be a positive quantity when present")
		}
		q, err := quantity(raw)
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, errors.New("PersistentVolumeClaim has no observed storage request to compare")
		}
		switch q.Cmp(*current) {
		case -1:
			return nil, errors.New("PersistentVolumeClaim storage cannot be decreased")
		case 0:
			// An unchanged size is allowed when the same edit changes an
			// approved label; it is never a storage change by itself.
		default:
			desiredSize = &q
		}
	}
	className := ""
	classDigest := ""
	expand := desiredSize != nil
	reclaim := ""
	if expand {
		if !s.Config.Features.PVCExpansion {
			return nil, errors.New("PVC expansion is disabled")
		}
		if pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" || pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
			return nil, errors.New("storage increase requires a bound nonterminating PVC with explicit class and volume")
		}
		className = *pvc.Spec.StorageClassName
		class, err := s.class(ctx, className, &ev)
		if err != nil {
			return nil, err
		}
		classDigest = digest(class)
		if class.AllowVolumeExpansion == nil || !*class.AllowVolumeExpansion {
			return nil, errors.New("StorageClass does not allow expansion")
		}
		pv, err := s.Typed.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			return nil, safe(err)
		}
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != class.Provisioner || pv.Spec.StorageClassName != className || pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != pvc.UID || pv.Spec.ClaimRef.Name != pvc.Name || pv.Spec.ClaimRef.Namespace != pvc.Namespace {
			return nil, errors.New("storage increase requires a verified CSI binding and matching driver")
		}
		ev.add("PersistentVolume", "", pv.Name, storagePVFingerprint(pv))
		reclaim = string(pv.Spec.PersistentVolumeReclaimPolicy)
		delta := desiredSize.DeepCopy()
		delta.Sub(*current)
		if err := s.storageQuota(ctx, a.Namespace, delta, false); err != nil {
			return nil, err
		}
		ops = append(ops, patchOp{Op: "replace", Path: "/spec/resources/requests/storage", Value: desiredSize.String()})
		changes = append(changes, model.Change{Field: "requested storage", Before: current.String(), After: desiredSize.String()})
	}
	if len(ops) == 0 {
		return nil, errors.New("no approved PersistentVolumeClaim change was requested")
	}
	spec := &storageProjectionSpec{}
	if desiredSize != nil {
		spec.Resources = &storageProjectionResources{Requests: storageProjectionRequests{Storage: desiredSize.String()}}
	} else if current != nil {
		// Keep the observed storage in the normalized document for a
		// metadata-only edit so a re-parse cannot mistake it for a removal.
		spec.Resources = &storageProjectionResources{Requests: storageProjectionRequests{Storage: current.String()}}
	}
	desired := storageDocumentProjection(storageProjection{
		Kind: storageKindPVC,
		Metadata: storageProjectionMetadata{
			Name: pvc.Name, Namespace: pvc.Namespace, UID: string(pvc.UID), ResourceVersion: pvc.ResourceVersion, Labels: normalizedLabels,
		},
		Spec: spec,
	})
	normalized, err := marshalStorageProjection(desired)
	if err != nil {
		return nil, err
	}
	a.StorageYAML = normalized
	requested := ""
	if desiredSize != nil {
		requested = desiredSize.String()
	}
	evd := storageEditEvidence{
		Version: 1, Policy: s.newEvidence().Policy, Kind: storageKindPVC,
		Namespace: pvc.Namespace, Name: pvc.Name, UID: string(pvc.UID), ResourceVersion: pvc.ResourceVersion,
		Desired: digest(desired), StorageClass: className, ClassDigest: classDigest,
		RequestedStorage: requested, ReclaimPolicy: reclaim, Labels: digest(desiredLabels),
		Scope: digest(string(ev.bytes())),
	}
	data, err := json.Marshal(evd)
	if err != nil {
		return nil, errors.New("storage edit evidence could not be encoded")
	}
	p := model.Prepared{
		Action: a, TargetUID: string(pvc.UID), ResourceVersion: pvc.ResourceVersion,
		Summary: "pvc.edit " + storageRef(storageKindPVC, pvc.Namespace, pvc.Name), Data: data, Changes: changes,
	}
	p.Warnings = storageEditWarnings(reclaim, true)
	if expand {
		p.Warnings = append(p.Warnings, "CSI/controller and filesystem resize must complete; driver/backend support requires deployment verification.")
	}
	if err := s.storageEditDryRun(ctx, storageKindPVC, pvc.Namespace, pvc.Name, pvc.UID, pvc.ResourceVersion, ops); err != nil {
		return nil, err
	}
	return &storageEditPlan{prepared: p, kind: storageKindPVC, pvc: pvc, ops: ops, uid: pvc.UID, rv: pvc.ResourceVersion, class: className, size: desiredSize, reclaim: reclaim, labels: desiredLabels, removed: removed, expand: expand}, nil
}

type storageEditEvidence struct {
	Version          int    `json:"version"`
	Policy           string `json:"policy"`
	Kind             string `json:"kind"`
	Namespace        string `json:"namespace,omitempty"`
	Name             string `json:"name"`
	UID              string `json:"uid"`
	ResourceVersion  string `json:"resourceVersion"`
	Desired          string `json:"desired"`
	StorageClass     string `json:"storageClass,omitempty"`
	ClassDigest      string `json:"classDigest,omitempty"`
	RequestedStorage string `json:"requestedStorage,omitempty"`
	ReclaimPolicy    string `json:"reclaimPolicy,omitempty"`
	Labels           string `json:"labels,omitempty"`
	Scope            string `json:"scope,omitempty"`
}

// storageLabelPatch computes only the console-owned label differences. Every
// other label and annotation is absent from both the projection and the patch,
// so it survives untouched.
func storageLabelPatch(allCurrent, desired map[string]string) ([]patchOp, []string, []model.Change) {
	current := map[string]string{}
	for k, v := range allCurrent {
		if strings.HasPrefix(k, StorageLabelPrefix) {
			current[k] = v
		}
	}
	ops := []patchOp{}
	changes := []model.Change{}
	removed := []string{}
	if len(desired) > 0 && len(allCurrent) == 0 {
		// The object has no metadata.labels map yet. Create an empty map once
		// so the per-key adds below have an existing parent. This never replaces
		// a map that already holds labels, so hidden labels stay untouched.
		ops = append(ops, patchOp{Op: "add", Path: "/metadata/labels", Value: map[string]string{}})
	}
	keys := make([]string, 0, len(desired))
	for k := range desired {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		want := desired[k]
		before, exists := current[k]
		if exists && before == want {
			continue
		}
		ops = append(ops, patchOp{Op: "add", Path: "/metadata/labels/" + pointer(k), Value: want})
		if exists {
			changes = append(changes, model.Change{Field: "label " + k, Before: before, After: want})
		} else {
			changes = append(changes, model.Change{Field: "label " + k, Before: storageAbsent, After: want})
		}
	}
	keys = keys[:0]
	for k := range current {
		if _, exists := desired[k]; !exists {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		ops = append(ops, patchOp{Op: "remove", Path: "/metadata/labels/" + pointer(k)})
		removed = append(removed, k)
		changes = append(changes, model.Change{Field: "label " + k, Before: current[k], After: storageAbsent})
	}
	return ops, removed, changes
}

func storageEditWarnings(reclaim string, pvc bool) []string {
	warnings := []string{
		"Only approved fields are patched with UID and resourceVersion preconditions. All other spec and metadata fields are preserved exactly.",
	}
	if pvc {
		warnings = append(warnings, "Only storage increases and kubeflow-admin.io/ labels are editable. Storage cannot be decreased, and a resize is not proof that the backend or filesystem completed it.")
	}
	if reclaim == string(corev1.PersistentVolumeReclaimDelete) {
		warnings = append(warnings, "The resulting reclaim policy is Delete. That can allow the storage stack to destroy the backing data irreversibly, even when this console only changes the policy and never deletes backend data itself.")
	}
	if !pvc && reclaim == string(corev1.PersistentVolumeReclaimRetain) {
		warnings = append(warnings, "Retain keeps the volume object and backend data after a claim is released in ordinary Kubernetes operation; it is not a backup guarantee and this console does not verify backend retention.")
	}
	return warnings
}

func storagePVWarnings(reclaim, operation string) []string {
	warnings := []string{
		"Only the reclaim policy and kubeflow-admin.io/ labels are shown. Volume source, backend path/handle/attributes, Secrets, annotations, finalizers, owner references, StorageClass and status are hidden and preserved.",
		"An omitted or null metadata.labels field leaves existing console labels unchanged. An explicit metadata.labels: {} removes all console labels. A nonempty labels map is the complete desired console-owned set: keys omitted from it are removed. At most 32 labels and 4 KiB of key+value text are accepted.",
	}
	if reclaim == string(corev1.PersistentVolumeReclaimDelete) {
		warnings = append(warnings, "This PersistentVolume has Delete reclaim policy. Deleting it or a future claim release can destroy backing data irreversibly; this console never touches backend data directly.")
	}
	if operation == "delete" {
		warnings = append(warnings,
			"An unused PersistentVolume may still contain data that no claim currently exposes. Deleting the object is not a verified data wipe; the storage stack, provisioner and backend retention policy determine what happens to the underlying volume.",
			"The unused check is a scoped observation, not an atomic usage fence. A PVC binding or reservation request, a controller action or an external actor can appear between the final recheck and the API delete; verify independently that nothing uses this volume.",
			"A PersistentVolume is deletable only while it is Available, has no claimRef, no PVC in any namespace names it and its StorageClass is approved.")
	}
	return warnings
}

// storageEditDryRun proves the exact conditional patch before any write.
func (s *Service) storageEditDryRun(ctx context.Context, kind, namespace, name string, uid types.UID, rv string, ops []patchOp) error {
	b, err := json.Marshal(conditional(string(uid), rv, ops))
	if err != nil {
		return errors.New("storage patch could not be encoded")
	}
	opts := metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}}
	var e error
	if kind == storageKindPV {
		_, e = s.Typed.CoreV1().PersistentVolumes().Patch(ctx, name, types.JSONPatchType, b, opts)
	} else {
		_, e = s.Typed.CoreV1().PersistentVolumeClaims(namespace).Patch(ctx, name, types.JSONPatchType, b, opts)
	}
	if e == nil {
		return nil
	}
	return storageDryRunError(e, kind)
}

// storagePVTarget enforces the unused-only PersistentVolume precondition:
// stable identity, Available phase, no claimRef, an approved StorageClass with
// a known reclaim policy and no PVC in any namespace naming this volume.
func (s *Service) storagePVTarget(ctx context.Context, volumeName string, ev *evidence) (*corev1.PersistentVolume, *storagev1.StorageClass, error) {
	if err := name(volumeName); err != nil {
		return nil, nil, err
	}
	pv, err := s.Typed.CoreV1().PersistentVolumes().Get(ctx, volumeName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, safe(err)
	}
	if pv.UID == "" || pv.ResourceVersion == "" {
		return nil, nil, errors.New("PersistentVolume has no stable UID/resourceVersion")
	}
	if pv.DeletionTimestamp != nil {
		return nil, nil, errors.New("PersistentVolume is terminating")
	}
	if pv.Status.Phase != corev1.VolumeAvailable {
		return nil, nil, errors.New("PersistentVolume must be Available; bound, released, pending or failed volumes are refused")
	}
	if pv.Spec.ClaimRef != nil {
		return nil, nil, errors.New("PersistentVolume is reserved or bound by a claimRef")
	}
	if pv.Spec.StorageClassName == "" {
		return nil, nil, errors.New("explicit approved StorageClass required")
	}
	class, err := s.class(ctx, pv.Spec.StorageClassName, ev)
	if err != nil {
		return nil, nil, err
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain && pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		return nil, nil, errors.New("PersistentVolume reclaim policy must be Retain or Delete")
	}
	claims, err := s.Typed.CoreV1().PersistentVolumeClaims(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, nil, errors.New("PersistentVolumeClaim reference list could not be verified")
	}
	if claims.GetContinue() != "" {
		return nil, nil, errors.New("PersistentVolumeClaim reference list is incomplete")
	}
	for _, claim := range claims.Items {
		if claim.Spec.VolumeName == volumeName {
			return nil, nil, errors.New("PersistentVolume is referenced by a PersistentVolumeClaim")
		}
	}
	return pv, class, nil
}

// storagePVFingerprint is the stable binding identity used as digest-only plan
// evidence. It includes the UID and the complete spec so a replaced volume, a
// changed claimRef, reclaim policy, class or backend source invalidates the
// plan. Status/resourceVersion heartbeat churn is deliberately excluded; the
// plan still re-reads the live volume and rechecks Bound phase immediately
// before the write. Only the digest is stored, never this value or any backend
// source.
type storagePVBindingFingerprint struct {
	UID  types.UID
	Spec corev1.PersistentVolumeSpec
}

func storagePVFingerprint(pv *corev1.PersistentVolume) storagePVBindingFingerprint {
	return storagePVBindingFingerprint{UID: pv.UID, Spec: *pv.Spec.DeepCopy()}
}

// storageDeletePlan is the internal execution plan of a storage delete.
type storageDeletePlan struct {
	prepared    model.Prepared
	kind        string
	namespace   string
	name        string
	uid         types.UID
	rv          string
	podDefaults []podDefaultCleanupTarget
	// reclaim is non-nil only for a PVC whose bound PersistentVolume uses the
	// Retain reclaim policy. It names the exact reviewed volume whose stale
	// claimRef is removed after the claim is confirmed deleted.
	reclaim *storageReclaimTarget
	ev      evidence
}

func (s *Service) prepareStorageDelete(ctx context.Context, a model.Action) (model.Prepared, error) {
	plan, err := s.buildStorageDelete(ctx, a)
	if err != nil {
		return model.Prepared{}, err
	}
	return plan.prepared, nil
}

func (s *Service) buildStorageDelete(ctx context.Context, a model.Action) (*storageDeletePlan, error) {
	if !s.Config.Features.WritesEnabled {
		return nil, errors.New("writes are disabled")
	}
	if !s.Config.Features.StorageDeletion {
		return nil, errors.New("storage deletion is disabled")
	}
	if err := storageDeleteActionConstraints(a); err != nil {
		return nil, err
	}
	kind, _ := storageKindForAction(a)
	ev := s.newEvidence()
	var uid types.UID
	var rv, className, classDigest, reclaim, namespace, targetName string
	podDefaults := []podDefaultCleanupTarget{}
	podDefaultAPI := ""
	var reclaimTarget *storageReclaimTarget
	switch kind {
	case storageKindPV:
		pv, class, err := s.storagePVTarget(ctx, a.Name, &ev)
		if err != nil {
			return nil, err
		}
		if string(pv.UID) != a.TargetUID || pv.ResourceVersion != a.TargetResourceVersion {
			return nil, fmt.Errorf("PersistentVolume changed before the deletion: %w", ErrConflict)
		}
		uid, rv, targetName = pv.UID, pv.ResourceVersion, pv.Name
		className, classDigest = pv.Spec.StorageClassName, digest(class)
		reclaim = string(pv.Spec.PersistentVolumeReclaimPolicy)
	case storageKindPVC:
		if err := s.namespacePolicy(a.Namespace); err != nil {
			return nil, err
		}
		if err := s.scope(ctx, a.Namespace, &ev); err != nil {
			return nil, err
		}
		pvc, class, classDigestValue, reclaimValue, boundPV, err := s.storagePVCDeleteTarget(ctx, a.Namespace, a.Name, &ev)
		if err != nil {
			return nil, err
		}
		if string(pvc.UID) != a.TargetUID || pvc.ResourceVersion != a.TargetResourceVersion {
			return nil, fmt.Errorf("PersistentVolumeClaim changed before the deletion: %w", ErrConflict)
		}
		uid, rv, targetName, namespace = pvc.UID, pvc.ResourceVersion, pvc.Name, pvc.Namespace
		className, classDigest, reclaim = class, classDigestValue, reclaimValue
		// A Retain volume is left Released with a stale claimRef once this claim
		// is gone. The reviewed volume identity is carried into the plan so the
		// post-delete reclaim can only ever act on this exact PersistentVolume.
		if a.ConfirmPVReclaim && (!s.Config.Features.PVReclaim || reclaimValue != string(corev1.PersistentVolumeReclaimRetain) || boundPV == nil) {
			return nil, errors.New("PV reclaim requires its separate feature and a bound Retain volume")
		}
		if a.ConfirmPVReclaim && boundPV != nil {
			if _, legacy := boundPV.Annotations[corev1.BetaStorageClassAnnotation]; legacy {
				return nil, errors.New("PV reclaim refuses deprecated beta StorageClass annotations")
			}
			if !has(s.Config.Storage.IsolatedStorageClasses, class) {
				return nil, errors.New("PV reclaim requires an operator-confirmed isolated StorageClass")
			}
			reclaimTarget = newStorageReclaimTarget(boundPV, pvc)
		}
		// PodDefault dependencies are inspected after the Pod/Notebook unused
		// checks above. Genuine CRD absence is explicit; every discovery, list,
		// read or permission failure is a refusal. The helper also enforces the
		// feature/acknowledgement gates and dry-runs every reviewed deletion.
		var state string
		podDefaults, state, err = s.podDefaultDependencies(ctx, namespace, targetName, a, &ev)
		if err != nil {
			return nil, err
		}
		podDefaultAPI = state
	}
	evd := storageDeleteEvidence{
		Version: 1, Policy: s.newEvidence().Policy, Kind: kind, Namespace: namespace,
		Name: targetName, UID: string(uid), ResourceVersion: rv,
		StorageClass: className, ClassDigest: classDigest, ReclaimPolicy: reclaim,
		PodDefaultAPI: podDefaultAPI, PodDefaults: podDefaults,
		Scope: digest(string(ev.bytes())),
	}
	if reclaimTarget != nil {
		evd.ReclaimVolume, evd.ReclaimVolumeUID = reclaimTarget.name, string(reclaimTarget.uid)
	}
	data, err := json.Marshal(evd)
	if err != nil {
		return nil, errors.New("storage delete evidence could not be encoded")
	}
	p := model.Prepared{
		Action: a, TargetUID: string(uid), ResourceVersion: rv,
		Summary: a.Kind + " " + storageRef(kind, namespace, targetName), Data: data,
		Changes: storageDeleteChanges(kind, namespace, targetName, className, reclaim, string(uid), rv),
	}
	if kind == storageKindPV {
		p.Warnings = append(storagePVWarnings(reclaim, "delete"), "Finalizers are never removed. If the delete remains pending for ten minutes the operation stops for review; it is never retried or forced.")
	} else {
		p.Warnings = storagePVCDeleteWarnings(reclaim)
		if reclaim == string(corev1.PersistentVolumeReclaimRetain) && reclaimTarget == nil {
			p.Warnings = append(p.Warnings, "The Retain volume stays Released with its claimRef after deletion. Returning it to Available requires a separate enabled and explicitly acknowledged reclaim with operator-confirmed class isolation.")
		}
		if reclaimTarget != nil {
			p.Changes = append(p.Changes, storageReclaimChanges(reclaimTarget)...)
			p.Warnings = append(p.Warnings, storageReclaimWarnings(reclaimTarget)...)
		}
		p.Changes = append(p.Changes, podDefaultCleanupChanges(podDefaults)...)
		p.Warnings = append(p.Warnings, podDefaultCleanupWarnings(podDefaults, targetName, a.ConfirmPodDefaultCleanup)...)
	}
	if err := s.storageDeleteDryRun(ctx, kind, namespace, targetName, uid, rv); err != nil {
		return nil, err
	}
	if reclaimTarget != nil {
		// Prove the exact conditional claimRef removal is permitted before the
		// claim is deleted, so missing patch RBAC is caught while nothing has
		// been changed. The dry run validates the patch against the still-bound
		// volume; the real patch is built from the volume re-read after deletion.
		if err := s.storageEditDryRun(ctx, storageKindPV, "", reclaimTarget.name, reclaimTarget.uid, reclaimTarget.resourceVersion, storageReclaimOps(reclaimTarget)); err != nil {
			return nil, err
		}
	}
	return &storageDeletePlan{prepared: p, kind: kind, namespace: namespace, name: targetName, uid: uid, rv: rv, podDefaults: podDefaults, reclaim: reclaimTarget, ev: ev}, nil
}

type storageDeleteEvidence struct {
	Version          int                       `json:"version"`
	Policy           string                    `json:"policy"`
	Kind             string                    `json:"kind"`
	Namespace        string                    `json:"namespace,omitempty"`
	Name             string                    `json:"name"`
	UID              string                    `json:"uid"`
	ResourceVersion  string                    `json:"resourceVersion"`
	StorageClass     string                    `json:"storageClass,omitempty"`
	ClassDigest      string                    `json:"classDigest,omitempty"`
	ReclaimPolicy    string                    `json:"reclaimPolicy,omitempty"`
	ReclaimVolume    string                    `json:"reclaimVolume,omitempty"`
	ReclaimVolumeUID string                    `json:"reclaimVolumeUID,omitempty"`
	PodDefaultAPI    string                    `json:"podDefaultAPI,omitempty"`
	PodDefaults      []podDefaultCleanupTarget `json:"podDefaults"`
	Scope            string                    `json:"scope,omitempty"`
}

// storagePVCDeleteTarget proves the unused-only PVC precondition. It allows a
// Pending claim with no volume, or a Bound claim whose PV names this exact
// claim; in both cases no observed Pod or Notebook may reference it, the PVC
// may not be controller-owned and any StorageClass must be approved.
func (s *Service) storagePVCDeleteTarget(ctx context.Context, namespace, claimName string, ev *evidence) (*corev1.PersistentVolumeClaim, string, string, string, *corev1.PersistentVolume, error) {
	pvc, err := s.Typed.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, claimName, metav1.GetOptions{})
	if err != nil {
		return nil, "", "", "", nil, safe(err)
	}
	if pvc.UID == "" || pvc.ResourceVersion == "" {
		return nil, "", "", "", nil, errors.New("PersistentVolumeClaim has no stable UID/resourceVersion")
	}
	if pvc.DeletionTimestamp != nil {
		return nil, "", "", "", nil, errors.New("PersistentVolumeClaim is terminating")
	}
	for _, ref := range pvc.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return nil, "", "", "", nil, errors.New("controller-owned PersistentVolumeClaims are refused to prevent automatic recreation")
		}
	}
	switch pvc.Status.Phase {
	case corev1.ClaimBound:
		if pvc.Spec.VolumeName == "" {
			return nil, "", "", "", nil, errors.New("bound PersistentVolumeClaim has no volumeName")
		}
	case corev1.ClaimPending:
		if pvc.Spec.VolumeName != "" {
			return nil, "", "", "", nil, errors.New("a pending PersistentVolumeClaim with a volumeName is binding and cannot be deleted here")
		}
	default:
		return nil, "", "", "", nil, errors.New("PersistentVolumeClaim must be Bound or Pending; lost or unknown claims are refused")
	}
	className := ""
	classDigest := ""
	// Deletion is bounded by an explicit class even for a Pending claim. A nil
	// class would otherwise make an unapproved or default-assigned claim
	// deletable outside the reviewed storage scope. This requirement is
	// intentionally delete-only: a safe label-only PVC edit is already bounded
	// by the Profile ownership and namespace policy and does not need a class.
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName == "" {
		return nil, "", "", "", nil, errors.New("explicit approved StorageClass required")
	}
	className = *pvc.Spec.StorageClassName
	class, err := s.class(ctx, className, ev)
	if err != nil {
		return nil, "", "", "", nil, err
	}
	classDigest = digest(class)
	reclaim := ""
	var boundPV *corev1.PersistentVolume
	if pvc.Spec.VolumeName != "" {
		pv, err := s.Typed.CoreV1().PersistentVolumes().Get(ctx, pvc.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			return nil, "", "", "", nil, errors.New("bound PersistentVolume could not be verified")
		}
		if pv.UID == "" || pv.DeletionTimestamp != nil || pv.Status.Phase != corev1.VolumeBound {
			return nil, "", "", "", nil, errors.New("bound PersistentVolume is not a stable Bound volume")
		}
		ref := pv.Spec.ClaimRef
		if ref == nil || ref.UID != pvc.UID || ref.Namespace != pvc.Namespace || ref.Name != pvc.Name {
			return nil, "", "", "", nil, errors.New("bound PersistentVolume does not name this exact claim")
		}
		if pv.Spec.StorageClassName == "" || !has(s.Config.Storage.AllowedStorageClasses, pv.Spec.StorageClassName) {
			return nil, "", "", "", nil, errors.New("bound PersistentVolume StorageClass is not approved")
		}
		if className != pv.Spec.StorageClassName {
			return nil, "", "", "", nil, errors.New("PersistentVolumeClaim and PersistentVolume class mismatch")
		}
		className = pv.Spec.StorageClassName
		class, err := s.class(ctx, className, ev)
		if err != nil {
			return nil, "", "", "", nil, err
		}
		classDigest = digest(class)
		switch pv.Spec.PersistentVolumeReclaimPolicy {
		case corev1.PersistentVolumeReclaimRetain, corev1.PersistentVolumeReclaimDelete:
			reclaim = string(pv.Spec.PersistentVolumeReclaimPolicy)
		default:
			return nil, "", "", "", nil, errors.New("bound PersistentVolume reclaim policy is unknown")
		}
		ev.add("PersistentVolume", "", pv.Name, storagePVFingerprint(pv))
		boundPV = pv
	}
	if err := s.rejectPVCReferences(ctx, pvc.Namespace, pvc.Name); err != nil {
		return nil, "", "", "", nil, err
	}
	if err := s.rejectPVCReservation(ctx, pvc); err != nil {
		return nil, "", "", "", nil, err
	}
	return pvc, className, classDigest, reclaim, boundPV, nil
}

// rejectPVCReservation fails closed when a PersistentVolume already reserves
// this exact claim. A Pending pre-bound claim with no volumeName can still be
// the target of a PV claimRef; deleting it would leave that reservation behind.
// A Bound claim must have exactly the one PV verified above, never a second
// reserved volume under the same claim identity.
func (s *Service) rejectPVCReservation(ctx context.Context, pvc *corev1.PersistentVolumeClaim) error {
	volumes, err := s.Typed.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return errors.New("PersistentVolume reservation list could not be verified")
	}
	if volumes.GetContinue() != "" {
		return errors.New("PersistentVolume reservation list is incomplete")
	}
	matches := 0
	for _, pv := range volumes.Items {
		ref := pv.Spec.ClaimRef
		if ref == nil || ref.Namespace != pvc.Namespace || ref.Name != pvc.Name {
			continue
		}
		if ref.UID != "" && ref.UID != pvc.UID {
			continue
		}
		matches++
		if pvc.Spec.VolumeName == "" || pv.Name != pvc.Spec.VolumeName {
			return errors.New("PersistentVolumeClaim is reserved by a PersistentVolume")
		}
	}
	if pvc.Spec.VolumeName != "" && matches != 1 {
		return errors.New("bound PersistentVolumeClaim does not have exactly one matching PersistentVolume reservation")
	}
	return nil
}

// rejectPVCReferences fails closed on any observed Pod or Notebook reference.
// It includes every Pod phase, inline ephemeral claim name collisions and
// stopped Notebooks. This is not an atomic usage fence: a Pod created after
// this read and before the API delete is outside its scope, which the preview
// states explicitly.
func (s *Service) rejectPVCReferences(ctx context.Context, namespace, name string) error {
	pods, err := s.Typed.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errors.New("Pod reference list could not be verified")
	}
	if pods.GetContinue() != "" {
		return errors.New("Pod reference list is incomplete")
	}
	for _, pod := range pods.Items {
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == name {
				return errors.New("PersistentVolumeClaim is referenced by an observed Pod")
			}
			if volume.Ephemeral != nil && volume.Ephemeral.VolumeClaimTemplate != nil && pod.Name+"-"+volume.Name == name {
				return errors.New("PersistentVolumeClaim collides with an observed inline ephemeral claim")
			}
		}
	}
	if s.Dynamic == nil {
		return errors.New("Notebook reference list could not be verified")
	}
	notes, err := s.Dynamic.Resource(Notebooks).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errors.New("Notebook reference list could not be verified")
	}
	if notes.GetContinue() != "" {
		return errors.New("Notebook reference list is incomplete")
	}
	for i := range notes.Items {
		used, err := notebookReferencesPVC(&notes.Items[i], name)
		if err != nil {
			// A present but unreadable volume template must fail closed: a
			// malformed Notebook could hide the reference this check exists to
			// find. No raw value is echoed.
			return errors.New("Notebook reference list could not be verified")
		}
		if used {
			return errors.New("PersistentVolumeClaim is referenced by an observed Notebook")
		}
	}
	return nil
}

// notebookReferencesPVC inspects the official unstructured volume shape. An
// absent volumes field and volumes of another kind are legitimate. A present
// field of the wrong type, a non-map volume entry, a present
// persistentVolumeClaim of the wrong type, or a present claim without a
// nonempty string claimName is malformed and fails closed.
func notebookReferencesPVC(note *unstructured.Unstructured, name string) (bool, error) {
	rawVolumes, found, err := unstructured.NestedFieldNoCopy(note.Object, "spec", "template", "spec", "volumes")
	if err != nil {
		return false, err
	}
	if !found || rawVolumes == nil {
		return false, nil
	}
	volumes, ok := rawVolumes.([]any)
	if !ok {
		return false, errors.New("malformed Notebook volumes")
	}
	for _, rawVolume := range volumes {
		volume, ok := rawVolume.(map[string]any)
		if !ok {
			return false, errors.New("malformed Notebook volume entry")
		}
		rawClaim, present, err := unstructured.NestedFieldNoCopy(volume, "persistentVolumeClaim")
		if err != nil {
			return false, err
		}
		if !present || rawClaim == nil {
			// Another volume kind, or an explicit empty source: no PVC reference.
			continue
		}
		if _, ok := rawClaim.(map[string]any); !ok {
			return false, errors.New("malformed Notebook persistentVolumeClaim")
		}
		claim, present, err := unstructured.NestedString(volume, "persistentVolumeClaim", "claimName")
		if err != nil {
			return false, err
		}
		if !present || claim == "" {
			return false, errors.New("malformed Notebook claimName")
		}
		if claim == name {
			return true, nil
		}
	}
	return false, nil
}

func storageDeleteChanges(kind, namespace, name, class, reclaim, uid, rv string) []model.Change {
	object := "PersistentVolume"
	if kind == storageKindPVC {
		object = "PersistentVolumeClaim"
	}
	changes := []model.Change{
		{Field: object, Before: storageRef(kind, namespace, name), After: "deleted"},
		{Field: "target UID", Before: uid, After: "exact precondition"},
		{Field: "target resource version", Before: rv, After: "exact precondition"},
		{Field: "unused check", Before: "no observed Pod, Notebook or PVC reference", After: "rechecked immediately before the single delete"},
	}
	if class != "" {
		changes = append(changes, model.Change{Field: "StorageClass", Before: class, After: "verified against the approved class"})
	}
	if reclaim != "" {
		changes = append(changes, model.Change{Field: "reclaim policy", Before: reclaim, After: "not modified by this console; may determine whether backend data is retained"})
	}
	return changes
}

func storagePVCDeleteWarnings(reclaim string) []string {
	warnings := []string{
		"Deleting a PersistentVolumeClaim removes the claim object. This console does not delete the bound PersistentVolume, does not modify its reclaim policy, does not remove finalizers and cannot force completion.",
		"The unused check is a scoped observation, not an atomic usage fence. It refuses any observed Pod in any phase and any Notebook template volume that names this claim, including stopped Notebooks, and it fails closed if a list cannot be read. Only those observed references are checked: a suspended controller, a workload whose Pod is not currently observed, or a Pod created between the final check and the API delete is not detected. Verify independently that nothing uses the claim.",
		"Finalizers are never removed. If the claim remains terminating for ten minutes the operation stops for review and is never retried automatically.",
	}
	switch reclaim {
	case string(corev1.PersistentVolumeReclaimDelete):
		warnings = append(warnings, "The bound PersistentVolume uses Delete reclaim policy. After the claim is deleted, the storage stack may destroy the backing volume and its data irreversibly. This console never performs that backend deletion itself.")
	case string(corev1.PersistentVolumeReclaimRetain):
		warnings = append(warnings, "The bound PersistentVolume uses Retain. Data may remain after the claim is deleted, but retention is not a backup guarantee and is not verified by this console.")
	}
	return warnings
}

func storageDeleteOptions(uid types.UID, rv string, dry bool) metav1.DeleteOptions {
	propagation := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{
		Preconditions:     &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
		PropagationPolicy: &propagation,
	}
	if dry {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	return opts
}

func (s *Service) storageDeleteDryRun(ctx context.Context, kind, namespace, name string, uid types.UID, rv string) error {
	if uid == "" || rv == "" {
		return errors.New("storage deletion requires UID and resourceVersion preconditions")
	}
	opts := storageDeleteOptions(uid, rv, true)
	var err error
	if kind == storageKindPV {
		err = s.Typed.CoreV1().PersistentVolumes().Delete(ctx, name, opts)
	} else {
		err = s.Typed.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, name, opts)
	}
	if err == nil {
		return nil
	}
	return storageDryRunError(err, kind)
}

func storageDryRunError(err error, kind string) error {
	object := "PersistentVolume"
	if kind == storageKindPVC {
		object = "PersistentVolumeClaim"
	}
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return errors.New("Kubernetes API denied the storage request")
	case apierrors.IsNotFound(err):
		return fmt.Errorf("the %s no longer exists: %w", object, ErrConflict)
	case apierrors.IsConflict(err):
		return fmt.Errorf("the %s changed before the request: %w", object, ErrConflict)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return errors.New("Kubernetes API rejected the storage request")
	default:
		return errors.New("Kubernetes API storage request failed")
	}
}

// executeStorageEdit re-validates the complete plan before and after the
// durable checkpoint, then issues exactly one conditional patch.
func (s *Service) executeStorageEdit(ctx context.Context, p model.Prepared, cp func(string) error) (err error) {
	mutationAttempted := false
	defer func() {
		if err != nil && !mutationAttempted {
			err = errors.Join(model.ErrRejectedBeforeMutation, err)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if p.Action.Kind != "pv.edit" && p.Action.Kind != "pvc.edit" {
		return errors.New("unsupported action kind")
	}
	plan, err := s.buildStorageEdit(ctx, p.Action)
	if err != nil {
		return err
	}
	if !storagePreparedMatches(p, plan.prepared) {
		return ErrConflict
	}
	if err = checkpoint(cp, storageEditStep); err != nil {
		return err
	}
	fresh, err := s.buildStorageEdit(ctx, p.Action)
	if err != nil {
		return err
	}
	if !storagePreparedMatches(p, fresh.prepared) {
		return ErrConflict
	}
	plan = fresh
	b, err := json.Marshal(conditional(string(plan.uid), plan.rv, plan.ops))
	if err != nil {
		return errors.New("storage patch could not be encoded")
	}
	mutationAttempted = true
	var patched *corev1.PersistentVolume
	var patchedClaim *corev1.PersistentVolumeClaim
	if plan.kind == storageKindPV {
		patched, err = s.Typed.CoreV1().PersistentVolumes().Patch(ctx, plan.pv.Name, types.JSONPatchType, b, metav1.PatchOptions{})
	} else {
		patchedClaim, err = s.Typed.CoreV1().PersistentVolumeClaims(plan.pvc.Namespace).Patch(ctx, plan.pvc.Name, types.JSONPatchType, b, metav1.PatchOptions{})
	}
	if err != nil {
		return storageMutationError(err, plan.kind, "edit")
	}
	if plan.kind == storageKindPV {
		if patched == nil || patched.UID != plan.uid {
			return fmt.Errorf("PersistentVolume identity changed during the edit: %w", ErrConflict)
		}
	} else if patchedClaim == nil || patchedClaim.UID != plan.uid {
		return fmt.Errorf("PersistentVolumeClaim identity changed during the edit: %w", ErrConflict)
	}
	if err = s.verifyStorageEdit(ctx, plan); err != nil {
		return err
	}
	if plan.expand {
		return s.waitForPVCExpansion(ctx, plan.pvc.Namespace, plan.pvc.Name, plan.uid, *plan.size)
	}
	return nil
}

func storagePreparedMatches(want, got model.Prepared) bool {
	return want.TargetUID == got.TargetUID &&
		want.ResourceVersion == got.ResourceVersion &&
		string(want.Data) == string(got.Data) &&
		want.Action.Kind == got.Action.Kind &&
		want.Action.Namespace == got.Action.Namespace &&
		want.Action.Name == got.Action.Name &&
		want.Action.TargetUID == got.Action.TargetUID &&
		want.Action.TargetResourceVersion == got.Action.TargetResourceVersion &&
		want.Action.StorageYAML == got.Action.StorageYAML &&
		want.Action.ConfirmPodDefaultCleanup == got.Action.ConfirmPodDefaultCleanup &&
		want.Action.ConfirmPVReclaim == got.Action.ConfirmPVReclaim
}

func (s *Service) verifyStorageEdit(ctx context.Context, plan *storageEditPlan) error {
	if plan.kind == storageKindPV {
		pv, err := s.Typed.CoreV1().PersistentVolumes().Get(ctx, plan.pv.Name, metav1.GetOptions{})
		if err != nil {
			return errors.New("edited PersistentVolume could not be verified")
		}
		if pv.UID != plan.uid {
			return fmt.Errorf("PersistentVolume was replaced during the edit: %w", ErrConflict)
		}
		if plan.reclaim != "" && string(pv.Spec.PersistentVolumeReclaimPolicy) != plan.reclaim {
			return errors.New("observed PersistentVolume reclaim policy does not match the plan")
		}
		return verifyStorageLabels(pv.Labels, plan.labels, plan.removed)
	}
	pvc, err := s.Typed.CoreV1().PersistentVolumeClaims(plan.pvc.Namespace).Get(ctx, plan.pvc.Name, metav1.GetOptions{})
	if err != nil {
		return errors.New("edited PersistentVolumeClaim could not be verified")
	}
	if pvc.UID != plan.uid {
		return fmt.Errorf("PersistentVolumeClaim was replaced during the edit: %w", ErrConflict)
	}
	if plan.size != nil && (pvc.Spec.Resources.Requests.Storage() == nil || pvc.Spec.Resources.Requests.Storage().Cmp(*plan.size) != 0) {
		return errors.New("observed PersistentVolumeClaim storage does not match the plan")
	}
	return verifyStorageLabels(pvc.Labels, plan.labels, plan.removed)
}

func verifyStorageLabels(live, desired map[string]string, removed []string) error {
	for k, v := range desired {
		got, exists := live[k]
		if !exists || got != v {
			return errors.New("observed console label does not match the plan")
		}
	}
	for _, k := range removed {
		if _, exists := live[k]; exists {
			return errors.New("a removed console label is still present")
		}
	}
	return nil
}

// executeStorageDelete issues exactly one conditioned delete after two complete
// validations. Any failure after the delete request is intentionally not marked
// as a pre-mutation rejection, so the operation stays in needs-review instead of
// being retried or silently released.
func (s *Service) executeStorageDelete(ctx context.Context, p model.Prepared, cp func(string) error) (err error) {
	mutationAttempted := false
	defer func() {
		if err != nil && !mutationAttempted {
			err = errors.Join(model.ErrRejectedBeforeMutation, err)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if p.Action.Kind != "pv.delete" && p.Action.Kind != "pvc.delete" {
		return errors.New("unsupported action kind")
	}
	if err := storageDeleteActionConstraints(p.Action); err != nil {
		return err
	}
	plan, err := s.buildStorageDelete(ctx, p.Action)
	if err != nil {
		return err
	}
	if !storagePreparedMatches(p, plan.prepared) {
		return ErrConflict
	}
	if len(plan.podDefaults) == 0 {
		// Preserve the established single-write path for a PV deletion and a
		// PVC deletion with no reviewed PodDefault cleanup.
		if err = checkpoint(cp, storageDeleteStep); err != nil {
			return err
		}
		fresh, err := s.buildStorageDelete(ctx, p.Action)
		if err != nil {
			return err
		}
		if !storagePreparedMatches(p, fresh.prepared) {
			return ErrConflict
		}
		plan = fresh
		opts := storageDeleteOptions(plan.uid, plan.rv, false)
		mutationAttempted = true
		if plan.kind == storageKindPV {
			err = s.Typed.CoreV1().PersistentVolumes().Delete(ctx, plan.name, opts)
		} else {
			err = s.Typed.CoreV1().PersistentVolumeClaims(plan.namespace).Delete(ctx, plan.name, opts)
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return storageMutationError(err, plan.kind, "delete")
		}
		if err := s.waitForStorageDeletion(ctx, plan.kind, plan.namespace, plan.name, plan.uid); err != nil {
			return err
		}
		return s.completeStorageDelete(ctx, plan, cp)
	}
	// A reviewed cleanup is a sequence of independent writes. One checkpoint
	// precedes every PodDefault deletion and another precedes the PVC deletion.
	// There is no rollback, replay or automatic discovery-and-delete.
	fresh, err := s.buildStorageDelete(ctx, p.Action)
	if err != nil {
		return err
	}
	if !storagePreparedMatches(p, fresh.prepared) {
		return ErrConflict
	}
	plan = fresh
	remaining := append([]podDefaultCleanupTarget(nil), plan.podDefaults...)
	for i, target := range plan.podDefaults {
		step := fmt.Sprintf("delete-poddefault-%d", i+1)
		if err = checkpoint(cp, step); err != nil {
			return err
		}
		// Profile ownership, namespace identity and policy/quota configuration
		// are re-read after every checkpoint. Quota status usage is deliberately
		// not compared: this operation's own earlier deletions are expected
		// self-deltas, and headroom was checked before the first allocation.
		if err = s.checkEvidence(ctx, plan.namespace, plan.ev); err != nil {
			return err
		}
		// Re-list after the checkpoint and require exactly the remaining
		// reviewed set. A new reference requires a fresh plan; a changed or
		// replaced reviewed object is refused before it is touched.
		if err = s.assertOnlyPlannedPodDefaults(ctx, plan.namespace, plan.name, remaining); err != nil {
			return err
		}
		if err = s.recheckPodDefaultTarget(ctx, target, plan.name); err != nil {
			return err
		}
		mutationAttempted = true
		if err = s.deletePodDefault(ctx, target); err != nil {
			return err
		}
		remaining = remaining[1:]
		// Completion receipt for exactly this reviewed index. The review order
		// is deterministic (sorted by name) and the acceptance Changes carry the
		// exact name/namespace/UID/resourceVersion snapshot for that index.
		if err = checkpoint(cp, fmt.Sprintf("delete-poddefault-%d-completed", i+1)); err != nil {
			return err
		}
	}
	if err = s.assertOnlyPlannedPodDefaults(ctx, plan.namespace, plan.name, nil); err != nil {
		return err
	}
	if err = checkpoint(cp, "delete-pvc"); err != nil {
		return err
	}
	if err = s.checkEvidence(ctx, plan.namespace, plan.ev); err != nil {
		return err
	}
	// Revalidate the complete PVC deletion contract after the PodDefault
	// writes and after the checkpoint. A new Pod/Notebook reference blocks the
	// PVC; the already-deleted PodDefaults stay deleted and are never recreated.
	target, err := s.Typed.CoreV1().PersistentVolumeClaims(plan.namespace).Get(ctx, plan.name, metav1.GetOptions{})
	if err != nil {
		return errors.New("the PersistentVolumeClaim could not be verified after PodDefault cleanup")
	}
	if target.UID != plan.uid || target.ResourceVersion != plan.rv || target.DeletionTimestamp != nil {
		return fmt.Errorf("the PersistentVolumeClaim changed after PodDefault cleanup: %w", ErrConflict)
	}
	var cleanupEv evidence
	_, className, classDigest, _, _, err := s.storagePVCDeleteTarget(ctx, plan.namespace, plan.name, &cleanupEv)
	if err != nil {
		return err
	}
	// The reviewed class and bound PV must still be the same objects that were
	// accepted. PodDefault cleanup may already have succeeded, so a change here
	// is a partial outcome that must stop before the PVC write.
	if want, ok := evidenceDep(plan.ev, "StorageClass", "", className); !ok || want.Digest != classDigest {
		return fmt.Errorf("the bound StorageClass changed after PodDefault cleanup: %w", ErrConflict)
	}
	if vol := target.Spec.VolumeName; vol != "" {
		want, ok := evidenceDep(plan.ev, "PersistentVolume", "", vol)
		got, current := evidenceDep(cleanupEv, "PersistentVolume", "", vol)
		if !ok || !current || want.Digest != got.Digest {
			return fmt.Errorf("the bound PersistentVolume changed after PodDefault cleanup: %w", ErrConflict)
		}
	}
	// The PodDefault dependency set is checked again after the PVC checkpoint
	// and every other dependency read, immediately before the one PVC delete.
	if err = s.assertOnlyPlannedPodDefaults(ctx, plan.namespace, plan.name, nil); err != nil {
		return err
	}
	opts := storageDeleteOptions(plan.uid, plan.rv, false)
	mutationAttempted = true
	if err = s.Typed.CoreV1().PersistentVolumeClaims(plan.namespace).Delete(ctx, plan.name, opts); err != nil && !apierrors.IsNotFound(err) {
		// Reviewed PodDefaults may already be deleted. Even a definite 403/409/
		// 422 is a partial outcome here, never a pre-mutation refusal.
		return storagePostMutationError(err, plan.kind, "delete")
	}
	if err := s.waitForStorageDeletion(ctx, plan.kind, plan.namespace, plan.name, plan.uid); err != nil {
		return err
	}
	return s.completeStorageDelete(ctx, plan, cp)
}

// completeStorageDelete records the completed-deletion receipt and then, for a
// claim that was bound to a Retain volume, reclaims that volume. The receipt is
// durable before the reclaim so a reclaim failure never hides the proven
// deletion. There is no rollback: the claim is never recreated.
func (s *Service) completeStorageDelete(ctx context.Context, plan *storageDeletePlan, cp func(string) error) error {
	if err := s.recordStorageDeletionReceipt(ctx, plan); err != nil {
		return err
	}
	if plan.reclaim == nil {
		return nil
	}
	return s.reclaimRetainedVolume(ctx, plan, cp)
}

func (s *Service) recordStorageDeletionReceipt(ctx context.Context, plan *storageDeletePlan) error {
	kind := mutationKindPersistentVolumeClaim
	if plan.kind == storageKindPV {
		kind = mutationKindPersistentVolume
	}
	return recordMutationReceipt(ctx, kind, plan.namespace, plan.name, plan.uid, plan.rv, model.MutationOutcomeDeleted)
}

func (s *Service) recheckPodDefaultTarget(ctx context.Context, target podDefaultCleanupTarget, claim string) error {
	present, err := s.podDefaultAPI(ctx, false)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("the reviewed PodDefault API disappeared before cleanup: %w", ErrConflict)
	}
	obj, err := s.Dynamic.Resource(PodDefaults).Namespace(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("the reviewed PodDefault could not be re-read: %w", ErrConflict)
	}
	if string(obj.GetUID()) != target.UID || obj.GetResourceVersion() != target.ResourceVersion || obj.GetDeletionTimestamp() != nil {
		return fmt.Errorf("the reviewed PodDefault changed before deletion: %w", ErrConflict)
	}
	usage, err := podDefaultUsageFrom(obj)
	if err != nil || !containsString(usage.Claims, claim) {
		return fmt.Errorf("the reviewed PodDefault no longer matches the reviewed dependency: %w", ErrConflict)
	}
	return nil
}

// storageMutationError classifies a write failure. Only an explicit API refusal
// that proves no mutation was applied may release the target lock. A 429, a 5xx,
// a timeout or a transport failure is ambiguous and stays in needs-review.
func storagePostMutationError(err error, kind, operation string) error {
	object := "PersistentVolume"
	if kind == storageKindPVC {
		object = "PersistentVolumeClaim"
	}
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return fmt.Errorf("Kubernetes API rejected the %s %s after a prior mutation; partial outcome requires review", object, operation)
	case apierrors.IsConflict(err), apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return fmt.Errorf("%s %s was rejected after a prior mutation; partial outcome requires review: %w", object, operation, ErrConflict)
	case apierrors.IsTooManyRequests(err), apierrors.IsServerTimeout(err), apierrors.IsTimeout(err), apierrors.IsServiceUnavailable(err), apierrors.IsInternalError(err):
		return fmt.Errorf("Kubernetes API %s %s outcome is unknown after a prior mutation", object, operation)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return fmt.Errorf("Kubernetes API %s %s outcome is unknown after a prior mutation", object, operation)
	}
}

func storageMutationError(err error, kind, operation string) error {
	object := "PersistentVolume"
	if kind == storageKindPVC {
		object = "PersistentVolumeClaim"
	}
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err), apierrors.IsConflict(err), apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return fmt.Errorf("%w: Kubernetes API rejected the %s %s", model.ErrRejectedBeforeMutation, object, operation)
	case apierrors.IsTooManyRequests(err), apierrors.IsServerTimeout(err), apierrors.IsTimeout(err), apierrors.IsServiceUnavailable(err), apierrors.IsInternalError(err):
		return fmt.Errorf("Kubernetes API %s %s outcome is unknown", object, operation)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		return fmt.Errorf("Kubernetes API %s %s outcome is unknown", object, operation)
	}
}

// waitForStorageDeletion reports success only when the exact reviewed UID is
// absent in two consecutive observations. A replacement under the same name is
// never accepted as the reviewed object being gone.
func (s *Service) waitForStorageDeletion(ctx context.Context, kind, namespace, name string, uid types.UID) error {
	absentRounds := 0
	return s.wait(ctx, func() (bool, error) {
		var object metav1.Object
		var err error
		if kind == storageKindPV {
			var pv *corev1.PersistentVolume
			pv, err = s.Typed.CoreV1().PersistentVolumes().Get(ctx, name, metav1.GetOptions{})
			if pv != nil {
				object = pv
			}
		} else {
			var pvc *corev1.PersistentVolumeClaim
			pvc, err = s.Typed.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
			if pvc != nil {
				object = pvc
			}
		}
		if apierrors.IsNotFound(err) {
			absentRounds++
			return absentRounds >= 2, nil
		}
		if err != nil {
			return false, errors.New("storage deletion state could not be verified")
		}
		absentRounds = 0
		if object.GetUID() != uid {
			return false, fmt.Errorf("a replacement storage object appeared during the deletion: %w", ErrConflict)
		}
		return false, nil
	})
}
