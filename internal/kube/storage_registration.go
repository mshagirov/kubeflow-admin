package kube

// storage_registration.go implements the two standalone, template-driven
// static workflows:
//
//   - pv.create.static: register exactly one new Available PersistentVolume
//     from an operator-approved registration template. No claimRef and no PVC
//     are created.
//   - pvc.create.bound: create exactly one PVC in an eligible Profile namespace
//     and reserve/bind it to one exact, selected Available PersistentVolume.
//     The optional dedicated PodDefault is a substep of this same operation.
//
// Every storage value is resolved from the server-owned registration template.
// The browser supplies only a template name, class, PV name (for binding), an
// approved NFS path or BeeGFS relative suffix, the reviewed NFS directory IDs
// and the optional PodDefault request. Paths, handles and evidence are never
// echoed in errors or logs; Prepared.Data carries digests and safe identities.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
)

const (
	nfsDirectoryUIDAnnotation = "k8s-sigs.io/nfs-directory-uid"
	nfsDirectoryGIDAnnotation = "k8s-sigs.io/nfs-directory-gid"

	maxDirectoryID       = uint64(4294967294)
	maxRegistrationPath  = 4096
	maxBeeGFSRelative    = 4096
	maxBeeGFSHandleBytes = 128
	registrationPVStep   = "create-static-pv"
	registrationBindStep = "reserve-selected-pv"
	registrationPVCStep  = "create-bound-pvc"
)

type storageRegistrationPlan struct {
	prepared   model.Prepared
	ev         evidence
	kind       string
	template   *config.RegistrationTemplate
	pv         *corev1.PersistentVolume
	pvc        *corev1.PersistentVolumeClaim
	ops        []patchOp
	podDefault *podDefaultPlan
	// volumeName is the exact selected PV for pvc.create.bound.
	volumeName string
}

type storageRegistrationEvidence struct {
	Version         int                 `json:"version"`
	Policy          string              `json:"policy"`
	Action          string              `json:"action"`
	Template        string              `json:"template"`
	TemplateKind    string              `json:"templateKind"`
	StorageClass    string              `json:"storageClass"`
	PVName          string              `json:"pvName,omitempty"`
	PVDesired       string              `json:"pvDesired,omitempty"`
	SelectedPV      string              `json:"selectedPV,omitempty"`
	TargetUID       string              `json:"targetUID,omitempty"`
	TargetRV        string              `json:"targetResourceVersion,omitempty"`
	PVCDesired      string              `json:"pvcDesired,omitempty"`
	Reservation     string              `json:"reservation,omitempty"`
	PodDefault      *podDefaultEvidence `json:"podDefault,omitempty"`
	PodDefaultState string              `json:"podDefaultState,omitempty"`
	Scope           string              `json:"scope,omitempty"`
}

func isStorageRegistrationKind(kind string) bool {
	return kind == "pv.create.static" || kind == "pvc.create.bound"
}

func (s *Service) prepareStorageRegistration(ctx context.Context, a model.Action) (model.Prepared, error) {
	plan, err := s.buildStorageRegistration(ctx, a)
	if err != nil {
		return model.Prepared{}, err
	}
	return plan.prepared, nil
}

func (s *Service) buildStorageRegistration(ctx context.Context, a model.Action) (*storageRegistrationPlan, error) {
	if !s.Config.Features.WritesEnabled {
		return nil, errors.New("writes are disabled")
	}
	if !s.Config.Features.StaticStorage {
		return nil, errors.New("static storage is disabled")
	}
	if err := storageRegistrationActionConstraints(a); err != nil {
		return nil, err
	}
	plan := &storageRegistrationPlan{kind: a.Kind}
	switch a.Kind {
	case "pv.create.static":
		if err := s.buildPVCreateStatic(ctx, a, plan); err != nil {
			return nil, err
		}
	case "pvc.create.bound":
		if err := s.buildPVCCreateBound(ctx, a, plan); err != nil {
			return nil, err
		}
	default:
		return nil, errors.New("unsupported storage registration action")
	}
	if err := s.storageRegistrationDryRun(ctx, plan); err != nil {
		return nil, err
	}
	return plan, nil
}

// unsupportedRegistrationFields prevents the new typed fields from being
// silently ignored by an older action kind. An action that does not implement a
// field must reject it, never accept it and do less than the browser requested.
func unsupportedRegistrationFields(a model.Action) error {
	if a.ConfirmPVReclaim && a.Kind != "pvc.delete" {
		return errors.New("confirmPVReclaim is only valid for pvc.delete")
	}
	if a.NFSPath != "" && a.Kind != "pv.create.static" {
		return errors.New("nfsPath is only valid for pv.create.static")
	}
	if a.BeeGFSPath != "" && a.Kind != "pv.create.static" {
		return errors.New("beeGFSPath is only valid for pv.create.static")
	}
	if (a.DirectoryUID != "" || a.DirectoryGID != "") && a.Kind != "pvc.create.bound" {
		return errors.New("directoryUID/directoryGID are only valid for pvc.create.bound")
	}
	switch a.Kind {
	case "pvc.create.dynamic", "pvc.create.bound":
	default:
		if a.PodDefault != nil {
			return errors.New("podDefault is only supported by pvc.create.dynamic or pvc.create.bound")
		}
	}
	if a.ConfirmPodDefaultCleanup && a.Kind != "pvc.delete" {
		return errors.New("confirmPodDefaultCleanup is only valid for pvc.delete")
	}
	return nil
}

func storageRegistrationActionConstraints(a model.Action) error {
	if a.Force || a.ConfirmName != "" || a.StorageYAML != "" {
		return errors.New("storage registration does not accept force, a delete confirmation or an edit document")
	}
	switch a.Kind {
	case "pv.create.static":
		if err := storageActionFields(a, "Kind", "Name", "Reason", "StorageClass", "Template", "NFSPath", "BeeGFSPath", "PVName"); err != nil {
			return err
		}
		if a.Namespace != "" {
			return errors.New("PersistentVolume is cluster scoped; namespace must be empty")
		}
		if err := name(a.Name); err != nil {
			return err
		}
		if a.PVName != "" && a.PVName != a.Name {
			return errors.New("pv.create.static uses name as the new PersistentVolume name")
		}
		if a.Template == "" || a.StorageClass == "" {
			return errors.New("an approved registration template and StorageClass are required")
		}
		if a.Reason != "" && len([]rune(a.Reason)) > 500 {
			return errors.New("reason exceeds 500 characters")
		}
	case "pvc.create.bound":
		if err := storageActionFields(a, "Kind", "Namespace", "Name", "Reason", "StorageClass", "Template", "PVName", "TargetUID", "TargetResourceVersion", "DirectoryUID", "DirectoryGID", "PodDefault"); err != nil {
			return err
		}
		if err := name(a.Name); err != nil {
			return err
		}
		if err := sNamespacePolicyErr(a.Namespace); err != nil {
			return err
		}
		if a.Template == "" || a.StorageClass == "" || a.PVName == "" {
			return errors.New("an approved registration template, StorageClass and selected PersistentVolume are required")
		}
		if err := name(a.PVName); err != nil {
			return err
		}
		if a.TargetUID == "" || a.TargetResourceVersion == "" {
			return errors.New("the selected PersistentVolume requires its exact current UID and resourceVersion")
		}
		if a.Reason != "" && len([]rune(a.Reason)) > 500 {
			return errors.New("reason exceeds 500 characters")
		}
	default:
		return errors.New("unsupported storage registration action")
	}
	return nil
}

func (s *Service) registrationTemplate(templateName, kind string) (*config.RegistrationTemplate, error) {
	var found *config.RegistrationTemplate
	for i := range s.Config.Storage.RegistrationTemplates {
		t := &s.Config.Storage.RegistrationTemplates[i]
		if t.Name != templateName {
			continue
		}
		if found != nil {
			return nil, errors.New("ambiguous registration template name")
		}
		found = t
	}
	if found == nil {
		return nil, errors.New("approved registration template required")
	}
	if kind != "" && found.Kind != kind {
		return nil, errors.New("registration template backend does not match the selected path")
	}
	return found, nil
}

func (s *Service) buildPVCreateStatic(ctx context.Context, a model.Action, plan *storageRegistrationPlan) error {
	if !has(s.Config.Storage.IsolatedStorageClasses, a.StorageClass) {
		return errors.New("unreserved PV creation requires an operator-confirmed isolated StorageClass")
	}
	if err := s.rejectUnboundClassClaims(ctx, a.StorageClass); err != nil {
		return err
	}
	t, err := s.registrationTemplate(a.Template, "")
	if err != nil {
		return err
	}
	if t.StorageClass != a.StorageClass {
		return errors.New("registration template StorageClass does not match the selected class")
	}
	ev := s.newEvidence()
	class, err := s.class(ctx, a.StorageClass, &ev)
	if err != nil {
		return err
	}
	if class.DeletionTimestamp != nil {
		return errors.New("StorageClass is terminating")
	}
	if t.Kind == config.RegistrationKindBeeGFS && class.Provisioner != t.BeeGFSDriver {
		return errors.New("BeeGFS registration StorageClass provisioner does not match the fixed driver")
	}
	pvCapacity, err := quantity(t.Capacity)
	if err != nil {
		return err
	}
	pvModes, err := registrationAccessModes(t.AccessModes)
	if err != nil {
		return err
	}
	fs := corev1.PersistentVolumeFilesystem
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: a.Name},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: pvCapacity},
			AccessModes:                   pvModes,
			VolumeMode:                    &fs,
			StorageClassName:              t.StorageClass,
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
		},
	}
	changes := []model.Change{
		{Field: "PersistentVolume", Before: "(new)", After: a.Name},
		{Field: "registration template", Before: "(selected)", After: t.Name},
		{Field: "StorageClass", Before: "(selected)", After: t.StorageClass},
		{Field: "PV capacity", Before: "(template)", After: t.Capacity},
		{Field: "PV access modes", Before: "(template)", After: strings.Join(t.AccessModes, ",")},
		{Field: "reclaim policy", Before: "(template)", After: string(corev1.PersistentVolumeReclaimRetain)},
		{Field: "volume mode", Before: "(template)", After: string(corev1.PersistentVolumeFilesystem)},
		{Field: "claimRef", Before: "(none)", After: "(remains unclaimed; no PVC is created)"},
	}
	switch t.Kind {
	case config.RegistrationKindNFS:
		if a.BeeGFSPath != "" {
			return errors.New("an NFS registration template accepts only nfsPath")
		}
		nfsPath, err := validateNFSRegistrationPath(a.NFSPath, t.NFSRoots)
		if err != nil {
			return err
		}
		pv.Spec.NFS = &corev1.NFSVolumeSource{Server: t.NFSServer, Path: nfsPath}
		pv.Spec.MountOptions = append([]string(nil), t.MountOptions...)
		changes = append(changes, model.Change{Field: "NFS path", Before: "(entered)", After: "entered path (digest " + shortDigest(nfsPath) + ")"})
	case config.RegistrationKindBeeGFS:
		if a.NFSPath != "" {
			return errors.New("a BeeGFS registration template accepts only beeGFSPath")
		}
		handle, err := joinBeeGFSPath(t.BeeGFSPrefix, a.BeeGFSPath)
		if err != nil {
			return err
		}
		pv.Spec.CSI = &corev1.CSIPersistentVolumeSource{Driver: t.BeeGFSDriver, VolumeHandle: handle}
		changes = append(changes, model.Change{Field: "BeeGFS path", Before: "(fixed prefix + entered suffix)", After: "entered suffix (digest " + shortDigest(handle) + ")"})
	default:
		return errors.New("unsupported registration template backend")
	}

	if _, err := s.Typed.CoreV1().PersistentVolumes().Get(ctx, a.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		if err == nil {
			return ErrConflict
		}
		return safe(err)
	}
	all, err := s.Typed.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return safe(err)
	}
	if all.GetContinue() != "" {
		return errors.New("PersistentVolume list is incomplete; backend uniqueness cannot be verified")
	}
	for i := range all.Items {
		overlap, err := sameBackendChecked(pv, &all.Items[i])
		if err != nil {
			return err
		}
		if overlap {
			return errors.New("backing storage is already represented by a PV")
		}
	}
	ev.add("PersistentVolumeList", "", "", all.Items)
	ev.Target = digest(pv.Spec)

	p := model.Prepared{
		Action:  a,
		Summary: "pv.create.static " + a.Name,
		Changes: changes,
		Warnings: []string{
			"This volume is unreserved: any matching claim in any namespace can bind it and read or modify the backend data. The configured isolated StorageClass requires independently verified admission restricting tenant PVC creation; an empty claimRef, a template or the class provisioner does not provide tenant isolation.",
			"Registers existing storage only; it does not create directories, set permissions, allocate backend space or move files. Declared capacity is not an enforced directory quota.",
			"The new PersistentVolume is created with no claimRef and no PVC. Its observed Available phase and exact identity/spec are verified, but backend readiness, export permissions and mount correctness require independent operator verification.",
			"Backend uniqueness is a scoped observation, not an atomic cluster-wide guarantee. Coordinate every privileged writer and admission policy; two writers can still race after this list check.",
		},
	}
	plan.template = t
	plan.pv = pv
	plan.ev = ev
	plan.prepared = p
	p.Data = storageRegistrationEvidenceBytes(plan)
	plan.prepared = p
	return nil
}

func (s *Service) buildPVCCreateBound(ctx context.Context, a model.Action, plan *storageRegistrationPlan) error {
	if err := s.namespacePolicy(a.Namespace); err != nil {
		return err
	}
	ev := s.newEvidence()
	if err := s.scope(ctx, a.Namespace, &ev); err != nil {
		return err
	}
	t, err := s.registrationTemplate(a.Template, "")
	if err != nil {
		return err
	}
	if t.StorageClass != a.StorageClass {
		return errors.New("registration template StorageClass does not match the selected class")
	}
	class, err := s.class(ctx, a.StorageClass, &ev)
	if err != nil {
		return err
	}
	if class.DeletionTimestamp != nil {
		return errors.New("StorageClass is terminating")
	}
	if t.Kind == config.RegistrationKindBeeGFS && class.Provisioner != t.BeeGFSDriver {
		return errors.New("BeeGFS registration StorageClass provisioner does not match the fixed driver")
	}
	pv, selectedClass, err := s.storagePVTarget(ctx, a.PVName, &ev)
	if err != nil {
		return err
	}
	if string(pv.UID) != a.TargetUID || pv.ResourceVersion != a.TargetResourceVersion {
		return fmt.Errorf("selected PersistentVolume changed before the plan: %w", ErrConflict)
	}
	if !s.pvMatchesRegistrationTemplate(pv, t) {
		return errors.New("selected PersistentVolume does not match the approved registration template")
	}
	if selectedClass.Name != a.StorageClass || pv.Spec.StorageClassName != a.StorageClass {
		return errors.New("selected PersistentVolume, StorageClass and template do not match")
	}
	pvcRequest, err := quantity(t.PVCRequest)
	if err != nil {
		return err
	}
	cap := pv.Spec.Capacity.Storage()
	if cap == nil || cap.Cmp(pvcRequest) < 0 {
		return errors.New("insufficient selected PersistentVolume capacity")
	}
	pvcModes, err := registrationAccessModes(t.PVCAccessModes)
	if err != nil {
		return err
	}
	for _, mode := range pvcModes {
		if !hasAccessMode(pv.Spec.AccessModes, mode) {
			return errors.New("selected PersistentVolume access mode mismatch")
		}
	}
	fs := corev1.PersistentVolumeFilesystem
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: a.Name, Namespace: a.Namespace},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      pvcModes,
			VolumeMode:       &fs,
			StorageClassName: &t.StorageClass,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: pvcRequest},
			},
			VolumeName: pv.Name,
		},
	}
	changes := []model.Change{
		{Field: "PersistentVolumeClaim", Before: "(new)", After: a.Namespace + "/" + a.Name},
		{Field: "selected PersistentVolume", Before: "(selected)", After: pv.Name},
		{Field: "selected PV UID", Before: a.TargetUID, After: "exact precondition"},
		{Field: "selected PV resourceVersion", Before: a.TargetResourceVersion, After: "exact precondition"},
		{Field: "registration template", Before: "(selected)", After: t.Name},
		{Field: "StorageClass", Before: "(selected)", After: t.StorageClass},
		{Field: "PVC request", Before: "(template)", After: t.PVCRequest},
		{Field: "PVC access modes", Before: "(template)", After: strings.Join(t.PVCAccessModes, ",")},
		{Field: "PV capacity", Before: "(observed)", After: cap.String()},
		{Field: "binding", Before: "(unclaimed PV)", After: "claimRef reserved for " + a.Namespace + "/" + a.Name},
	}
	switch t.Kind {
	case config.RegistrationKindNFS:
		uid, gid, err := requiredDirectoryIDs(a.DirectoryUID, a.DirectoryGID)
		if err != nil {
			return err
		}
		pvc.Annotations = map[string]string{nfsDirectoryUIDAnnotation: uid, nfsDirectoryGIDAnnotation: gid}
		changes = append(changes, model.Change{Field: "NFS directory UID", Before: "(required)", After: uid})
		changes = append(changes, model.Change{Field: "NFS directory GID", Before: "(required)", After: gid})
	case config.RegistrationKindBeeGFS:
		if a.DirectoryUID != "" || a.DirectoryGID != "" {
			return errors.New("BeeGFS registration does not accept NFS directory IDs")
		}
	default:
		return errors.New("unsupported registration template backend")
	}

	if _, err := s.Typed.CoreV1().PersistentVolumeClaims(a.Namespace).Get(ctx, a.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		if err == nil {
			return ErrConflict
		}
		return safe(err)
	}
	if err := s.storageQuota(ctx, a.Namespace, pvcRequest, true); err != nil {
		return err
	}
	ev.add("PersistentVolume", "", pv.Name, storagePVFingerprint(pv))
	ev.Target = digest(pv.Spec)
	ops := []patchOp{{"add", "/spec/claimRef", map[string]any{"apiVersion": "v1", "kind": "PersistentVolumeClaim", "name": a.Name, "namespace": a.Namespace}}}

	var podPlan *podDefaultPlan
	podState := "not-requested"
	if a.PodDefault != nil {
		podPlan, podState, err = s.buildPodDefaultPlan(ctx, a, a.Namespace, a.Name, t, &ev)
		if err != nil {
			return err
		}
		podPlan.selectedPV = pv.Name
		podPlan.requireBound = true
	}
	warnings := []string{
		"The PVC is created with an exact volumeName and the selected PV is reserved by a UID/resourceVersion-conditioned claimRef. The stored UID/resourceVersion is the reviewed PV identity, not a name-only match.",
		"StorageClass, PV and PVC access/volume modes and capacity are rechecked before every write. A claim request may be smaller than the PV capacity; declared capacity is not an enforced directory quota.",
		"The unused PVC/PV checks are scoped observations, not an atomic usage fence. External writers, the PV controller and admission remain outside the console's locks.",
	}
	if t.Kind == config.RegistrationKindNFS {
		warnings = append(warnings, "The two approved NFS directory annotations declare intent only. The console does not create, chown or secure the backend directory, and workload UID/GID permissions remain an operator responsibility.")
	}
	if podPlan != nil {
		warnings = append(warnings, podPlan.warnings...)
		warnings = append(warnings, "If PVC creation succeeds but the optional PodDefault step fails, the PVC and its binding are retained. The operation reports an unfinished PodDefault step; the PVC is never deleted as rollback and nothing is automatically replayed.")
		changes = append(changes, podPlan.changes...)
	}
	p := model.Prepared{
		Action:          a,
		TargetUID:       string(pv.UID),
		ResourceVersion: pv.ResourceVersion,
		Summary:         "pvc.create.bound " + a.Namespace + "/" + a.Name + " -> " + pv.Name,
		Changes:         changes,
		Warnings:        warnings,
	}
	plan.template = t
	plan.pv = pv
	plan.pvc = pvc
	plan.ops = ops
	plan.podDefault = podPlan
	plan.volumeName = pv.Name
	plan.ev = ev
	plan.prepared = p
	evd := storageRegistrationEvidence{
		Version: 1, Policy: ev.Policy, Action: a.Kind, Template: t.Name, TemplateKind: t.Kind,
		StorageClass: t.StorageClass, PVName: pv.Name, SelectedPV: digest(storagePVFingerprint(pv)),
		TargetUID: string(pv.UID), TargetRV: pv.ResourceVersion,
		PVCDesired: digest(struct {
			Spec        corev1.PersistentVolumeClaimSpec
			Annotations map[string]string
		}{pvc.Spec, pvc.Annotations}),
		Reservation: digest(ops), PodDefaultState: podState, Scope: digest(string(ev.bytes())),
	}
	if podPlan != nil {
		evd.PodDefault = &podPlan.evidence
	}
	b, err := json.Marshal(evd)
	if err != nil {
		return errors.New("storage registration evidence could not be encoded")
	}
	p.Data = b
	plan.prepared = p
	return nil
}

func storageRegistrationEvidenceBytes(plan *storageRegistrationPlan) []byte {
	evd := storageRegistrationEvidence{
		Version: 1, Policy: plan.ev.Policy, Action: plan.prepared.Action.Kind,
		Template: plan.template.Name, TemplateKind: plan.template.Kind,
		StorageClass: plan.template.StorageClass, PVName: plan.pv.Name,
		PVDesired: digest(plan.pv.Spec), Scope: digest(string(plan.ev.bytes())),
	}
	b, _ := json.Marshal(evd)
	return b
}

func (s *Service) storageRegistrationDryRun(ctx context.Context, plan *storageRegistrationPlan) error {
	o := metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}}
	switch plan.prepared.Action.Kind {
	case "pv.create.static":
		if _, err := s.Typed.CoreV1().PersistentVolumes().Create(ctx, plan.pv, o); err != nil {
			return safe(err)
		}
		return nil
	case "pvc.create.bound":
		b, err := json.Marshal(conditional(string(plan.pv.UID), plan.pv.ResourceVersion, plan.ops))
		if err != nil {
			return errors.New("selected PersistentVolume reservation could not be encoded")
		}
		if _, err := s.Typed.CoreV1().PersistentVolumes().Patch(ctx, plan.pv.Name, types.JSONPatchType, b, metav1.PatchOptions{DryRun: o.DryRun}); err != nil {
			return safe(err)
		}
		if _, err := s.Typed.CoreV1().PersistentVolumeClaims(plan.pvc.Namespace).Create(ctx, plan.pvc, o); err != nil {
			return safe(err)
		}
		// PodDefault dry-runs are performed while the optional substep is
		// built, before any real write in this plan.
		return nil
	}
	return errors.New("unsupported storage registration action")
}

func (s *Service) executeStorageRegistration(ctx context.Context, p model.Prepared, cp func(string) error) (err error) {
	mutationAttempted := false
	defer func() {
		if err != nil && !mutationAttempted {
			err = errors.Join(model.ErrRejectedBeforeMutation, err)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if !isStorageRegistrationKind(p.Action.Kind) {
		return errors.New("unsupported storage registration action")
	}
	plan, err := s.buildStorageRegistration(ctx, p.Action)
	if err != nil {
		return err
	}
	if !storageRegistrationPreparedMatches(p, plan.prepared) {
		return ErrConflict
	}
	switch p.Action.Kind {
	case "pv.create.static":
		if err := checkpoint(cp, registrationPVStep); err != nil {
			return err
		}
		fresh, err := s.buildStorageRegistration(ctx, p.Action)
		if err != nil {
			return err
		}
		if !storageRegistrationPreparedMatches(p, fresh.prepared) {
			return ErrConflict
		}
		plan = fresh
		work := &work{prepared: plan.prepared, ev: plan.ev, pv: plan.pv}
		if err := s.revalidateClass(ctx, plan.template.StorageClass, work); err != nil {
			return err
		}
		if err := s.assertNoOverlap(ctx, plan.pv, plan.pv.Name); err != nil {
			return err
		}
		mutationAttempted = true
		v, err := s.Typed.CoreV1().PersistentVolumes().Create(ctx, plan.pv, metav1.CreateOptions{})
		if err != nil {
			return storageMutationError(err, storageKindPV, "create")
		}
		if v == nil || v.UID == "" {
			return errors.New("created PersistentVolume identity was not observed")
		}
		// Emit the exact created identity before any further work. A recorder
		// failure stops here, retains locks and is never a pre-mutation refusal.
		if err := recordMutationReceipt(ctx, mutationKindPersistentVolume, "", v.Name, v.UID, v.ResourceVersion, model.MutationOutcomeCreated); err != nil {
			return err
		}
		// The create receipt is durable before the convergence wait, so a
		// failed status verification still records that the object was created.
		if err := checkpoint(cp, "static-pv-created"); err != nil {
			return err
		}
		available, err := s.verifyCreatedPV(ctx, plan.pv, v.UID)
		if err != nil {
			return err
		}
		if err := recordMutationReceipt(ctx, mutationKindPersistentVolume, "", available.Name, available.UID, available.ResourceVersion, model.MutationOutcomeAvailable); err != nil {
			return err
		}
		// Completion receipt: this checkpoint can only exist after the exact
		// reviewed PV was observed with a stable UID and Available phase.
		return checkpoint(cp, "static-pv-verified")
	case "pvc.create.bound":
		return s.executePVCCreateBound(ctx, p, plan, cp, &mutationAttempted)
	default:
		return errors.New("unsupported storage registration action")
	}
}

func (s *Service) executePVCCreateBound(ctx context.Context, p model.Prepared, plan *storageRegistrationPlan, cp func(string) error, mutationAttempted *bool) error {
	if err := checkpoint(cp, registrationBindStep); err != nil {
		return err
	}
	fresh, err := s.buildStorageRegistration(ctx, p.Action)
	if err != nil {
		return err
	}
	if !storageRegistrationPreparedMatches(p, fresh.prepared) {
		return ErrConflict
	}
	plan = fresh
	w := &work{prepared: plan.prepared, ev: plan.ev, pv: plan.pv, pvc: plan.pvc, ops: plan.ops}
	if err := s.checkEvidence(ctx, plan.prepared.Action.Namespace, plan.ev); err != nil {
		return err
	}
	if err := s.revalidateClass(ctx, plan.template.StorageClass, w); err != nil {
		return err
	}
	b, err := json.Marshal(conditional(string(plan.pv.UID), plan.pv.ResourceVersion, plan.ops))
	if err != nil {
		return errors.New("selected PersistentVolume reservation could not be encoded")
	}
	*mutationAttempted = true
	reserved, err := s.Typed.CoreV1().PersistentVolumes().Patch(ctx, plan.pv.Name, types.JSONPatchType, b, metav1.PatchOptions{})
	if err != nil {
		return storageMutationError(err, storageKindPV, "reserve")
	}
	if reserved == nil || reserved.UID != plan.pv.UID {
		return fmt.Errorf("selected PersistentVolume identity changed during reservation: %w", ErrConflict)
	}
	if err := recordMutationReceipt(ctx, mutationKindPersistentVolume, "", reserved.Name, reserved.UID, reserved.ResourceVersion, model.MutationOutcomeReserved); err != nil {
		return err
	}
	if err := s.verifyReservedPV(ctx, plan.prepared.Action, w, reserved); err != nil {
		return err
	}
	// The selected PV is now durably reserved for this reviewed claim name.
	if err := checkpoint(cp, "selected-pv-reserved"); err != nil {
		return err
	}
	if err := checkpoint(cp, registrationPVCStep); err != nil {
		return err
	}
	if err := s.checkEvidence(ctx, plan.prepared.Action.Namespace, plan.ev); err != nil {
		return err
	}
	if err := s.revalidateClass(ctx, plan.template.StorageClass, w); err != nil {
		return err
	}
	if err := s.verifyReservedPV(ctx, plan.prepared.Action, w, reserved); err != nil {
		return err
	}
	created, err := s.Typed.CoreV1().PersistentVolumeClaims(plan.pvc.Namespace).Create(ctx, plan.pvc, metav1.CreateOptions{})
	if err != nil {
		// The selected PV has already been reserved by this operation. Even a
		// definite PVC 403/409/422 is therefore a partial outcome: it must not
		// carry ErrRejectedBeforeMutation, which would release the locks and
		// hide the reservation that remains on the cluster.
		return boundPVCMutationError(err)
	}
	if created == nil || created.UID == "" {
		return errors.New("created PersistentVolumeClaim identity was not observed")
	}
	if err := recordMutationReceipt(ctx, mutationKindPersistentVolumeClaim, created.Namespace, created.Name, created.UID, created.ResourceVersion, model.MutationOutcomeCreated); err != nil {
		return err
	}
	// The object-created receipt is durable before the binding wait. It does
	// not claim that binding or backend provisioning completed.
	if err := checkpoint(cp, "bound-pvc-created"); err != nil {
		return err
	}
	bound, err := s.waitForBoundPVC(ctx, plan, created.UID, reserved.UID)
	if err != nil {
		return err
	}
	if err := recordMutationReceipt(ctx, mutationKindPersistentVolumeClaim, bound.Namespace, bound.Name, bound.UID, bound.ResourceVersion, model.MutationOutcomeBound); err != nil {
		return err
	}
	if err := checkpoint(cp, "bound-pvc-bound"); err != nil {
		return err
	}
	if plan.podDefault == nil {
		return nil
	}
	step := "create-poddefault"
	if plan.podDefault.overwrite {
		step = "overwrite-poddefault"
	}
	if err := checkpoint(cp, step); err != nil {
		return err
	}
	if err := s.checkEvidence(ctx, plan.prepared.Action.Namespace, plan.ev); err != nil {
		return err
	}
	if err := s.revalidatePodDefaultBeforeWrite(ctx, plan.podDefault, created); err != nil {
		return err
	}
	if err := s.writePodDefault(ctx, plan.podDefault, created); err != nil {
		return err
	}
	if err := s.verifyPodDefault(ctx, plan.podDefault); err != nil {
		return err
	}
	completed := "poddefault-created"
	if plan.podDefault.overwrite {
		completed = "poddefault-overwritten"
	}
	return checkpoint(cp, completed)
}

func storageRegistrationPreparedMatches(want, got model.Prepared) bool {
	if want.TargetUID != got.TargetUID || want.ResourceVersion != got.ResourceVersion || string(want.Data) != string(got.Data) {
		return false
	}
	a, b := want.Action, got.Action
	if a.Kind != b.Kind || a.Namespace != b.Namespace || a.Name != b.Name || a.Template != b.Template || a.StorageClass != b.StorageClass ||
		a.PVName != b.PVName || a.TargetUID != b.TargetUID || a.TargetResourceVersion != b.TargetResourceVersion ||
		a.NFSPath != b.NFSPath || a.BeeGFSPath != b.BeeGFSPath || a.DirectoryUID != b.DirectoryUID || a.DirectoryGID != b.DirectoryGID {
		return false
	}
	return same(a.PodDefault, b.PodDefault)
}

func (s *Service) verifyCreatedPV(ctx context.Context, planned *corev1.PersistentVolume, uid types.UID) (*corev1.PersistentVolume, error) {
	var observed *corev1.PersistentVolume
	err := s.wait(ctx, func() (bool, error) {
		v, err := s.Typed.CoreV1().PersistentVolumes().Get(ctx, planned.Name, metav1.GetOptions{})
		if err != nil {
			return false, safe(err)
		}
		if v.UID != uid || v.DeletionTimestamp != nil {
			return false, ErrConflict
		}
		if !pvMatchesPlan(v, planned) || v.Spec.ClaimRef != nil {
			return false, ErrConflict
		}
		if v.Status.Phase != corev1.VolumeAvailable {
			return false, nil
		}
		observed = v
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return observed, nil
}

func (s *Service) waitForBoundPVC(ctx context.Context, plan *storageRegistrationPlan, pvcUID, pvUID types.UID) (*corev1.PersistentVolumeClaim, error) {
	var bound *corev1.PersistentVolumeClaim
	err := s.wait(ctx, func() (bool, error) {
		cur, err := s.Typed.CoreV1().PersistentVolumeClaims(plan.pvc.Namespace).Get(ctx, plan.pvc.Name, metav1.GetOptions{})
		if err != nil {
			return false, safe(err)
		}
		if cur.UID != pvcUID || cur.DeletionTimestamp != nil {
			return false, ErrConflict
		}
		if !pvcMatchesPlan(cur, plan.pvc) {
			return false, ErrConflict
		}
		if cur.Status.Phase != corev1.ClaimBound || cur.Spec.VolumeName != plan.volumeName {
			return false, nil
		}
		v, err := s.Typed.CoreV1().PersistentVolumes().Get(ctx, cur.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			return false, safe(err)
		}
		if v.UID != pvUID {
			return false, ErrConflict
		}
		r := v.Spec.ClaimRef
		if r == nil || r.UID != cur.UID || r.Namespace != cur.Namespace || r.Name != cur.Name {
			return false, errors.New("PVC/PV binding identity mismatch")
		}
		if v.Status.Phase != corev1.VolumeBound {
			return false, nil
		}
		bound = cur
		return true, nil
	})
	if err != nil {
		return nil, err
	}
	return bound, nil
}

// boundPVCMutationError deliberately never wraps ErrRejectedBeforeMutation:
// the PV reservation write has already succeeded, so the PVC step is a partial
// outcome even when the API gives a definite 403/409/422 response.
func boundPVCMutationError(err error) error {
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return errors.New("Kubernetes API rejected the bound PersistentVolumeClaim creation after PV reservation; the reservation remains for review")
	case apierrors.IsConflict(err):
		return fmt.Errorf("the bound PersistentVolumeClaim conflicted after PV reservation; the reservation remains for review: %w", ErrConflict)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return errors.New("Kubernetes API rejected the bound PersistentVolumeClaim validation after PV reservation; the reservation remains for review")
	default:
		return errors.New("Kubernetes API bound PersistentVolumeClaim creation outcome is unknown; the PV reservation remains for review")
	}
}

func pvcMatchesPlan(actual, planned *corev1.PersistentVolumeClaim) bool {
	if actual == nil || planned == nil || actual.Spec.VolumeName != planned.Spec.VolumeName || actual.Namespace != planned.Namespace || actual.Name != planned.Name {
		return false
	}
	if !sameModes(actual.Spec.AccessModes, planned.Spec.AccessModes) {
		return false
	}
	if (actual.Spec.VolumeMode == nil) != (planned.Spec.VolumeMode == nil) || actual.Spec.VolumeMode != nil && *actual.Spec.VolumeMode != *planned.Spec.VolumeMode {
		return false
	}
	if (actual.Spec.StorageClassName == nil) != (planned.Spec.StorageClassName == nil) || actual.Spec.StorageClassName != nil && *actual.Spec.StorageClassName != *planned.Spec.StorageClassName {
		return false
	}
	if actual.Spec.Selector != nil || actual.Spec.DataSource != nil || actual.Spec.DataSourceRef != nil || actual.Spec.VolumeAttributesClassName != nil || len(actual.Spec.Resources.Limits) != 0 {
		return false
	}
	a, p := actual.Spec.Resources.Requests.Storage(), planned.Spec.Resources.Requests.Storage()
	if a == nil || p == nil || a.Cmp(*p) != 0 {
		return false
	}
	for k, v := range planned.Annotations {
		if actual.Annotations[k] != v {
			return false
		}
	}
	for _, key := range []string{nfsDirectoryUIDAnnotation, nfsDirectoryGIDAnnotation} {
		if _, wanted := planned.Annotations[key]; !wanted {
			if _, present := actual.Annotations[key]; present {
				return false
			}
		}
	}
	return true
}

func registrationAccessModes(modes []string) ([]corev1.PersistentVolumeAccessMode, error) {
	out := make([]corev1.PersistentVolumeAccessMode, 0, len(modes))
	for _, mode := range modes {
		m, err := accessMode(mode)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if len(out) == 0 {
		return nil, errors.New("at least one access mode is required")
	}
	return out, nil
}

func hasAccessMode(modes []corev1.PersistentVolumeAccessMode, want corev1.PersistentVolumeAccessMode) bool {
	for _, mode := range modes {
		if mode == want {
			return true
		}
	}
	return false
}

// pvMatchesRegistrationTemplate is intentionally strict: a registration PV is
// only selectable when its class, access modes, reclaim/volume mode and backend
// source match one configured template, with sufficient capacity for the claim.
// This keeps an unrelated hand-built PV out of the reviewed binding workflow.
func (s *Service) pvMatchesRegistrationTemplate(pv *corev1.PersistentVolume, t *config.RegistrationTemplate) bool {
	if pv == nil || t == nil || pv.DeletionTimestamp != nil || pv.UID == "" {
		return false
	}
	if pv.Spec.StorageClassName != t.StorageClass || pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		return false
	}
	if pv.Spec.VolumeMode != nil && *pv.Spec.VolumeMode != corev1.PersistentVolumeFilesystem {
		return false
	}
	if pv.Spec.NodeAffinity != nil || pv.Spec.VolumeAttributesClassName != nil {
		return false
	}
	// A selected existing PV may be larger than the template default. The
	// template's exact capacity is used only when this adapter creates a new
	// PV; binding separately requires capacity >= the reviewed PVC request.
	expectedModes, err := registrationAccessModes(t.AccessModes)
	if err != nil || !sameModes(pv.Spec.AccessModes, expectedModes) {
		return false
	}
	switch t.Kind {
	case config.RegistrationKindNFS:
		if pv.Spec.NFS == nil || pv.Spec.CSI != nil {
			return false
		}
		if pv.Spec.NFS.Server != t.NFSServer || pv.Spec.NFS.ReadOnly {
			return false
		}
		if pv.Spec.NFS.Path == "" || !path.IsAbs(pv.Spec.NFS.Path) || path.Clean(pv.Spec.NFS.Path) != pv.Spec.NFS.Path || strings.ContainsAny(pv.Spec.NFS.Path, "\\") || containsAmbiguousEncoding(pv.Spec.NFS.Path) {
			return false
		}
		for _, r := range pv.Spec.NFS.Path {
			if unicode.IsControl(r) {
				return false
			}
		}
		p := pv.Spec.NFS.Path
		under := false
		for _, root := range t.NFSRoots {
			if root == "" || root == "/" || !path.IsAbs(root) || path.Clean(root) != root || strings.Contains(root, "\\") {
				continue
			}
			if p == path.Clean(root) || strings.HasPrefix(p, strings.TrimSuffix(path.Clean(root), "/")+"/") {
				under = true
				break
			}
		}
		if !under || !equalStrings(pv.Spec.MountOptions, t.MountOptions) {
			return false
		}
		return true
	case config.RegistrationKindBeeGFS:
		if pv.Spec.CSI == nil || pv.Spec.NFS != nil || pv.Spec.CSI.Driver != t.BeeGFSDriver {
			return false
		}
		if len(pv.Spec.CSI.VolumeAttributes) != 0 || pv.Spec.CSI.FSType != "" || pv.Spec.CSI.ReadOnly || len(pv.Spec.MountOptions) != 0 {
			return false
		}
		if pv.Spec.CSI.ControllerPublishSecretRef != nil || pv.Spec.CSI.NodeStageSecretRef != nil || pv.Spec.CSI.NodePublishSecretRef != nil || pv.Spec.CSI.ControllerExpandSecretRef != nil || pv.Spec.CSI.NodeExpandSecretRef != nil {
			return false
		}
		return beeGFSHandleUnderPrefix(pv.Spec.CSI.VolumeHandle, t.BeeGFSPrefix)
	}
	return false
}

func sameModes(a, b []corev1.PersistentVolumeAccessMode) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[corev1.PersistentVolumeAccessMode]bool{}
	for _, mode := range a {
		seen[mode] = true
	}
	for _, mode := range b {
		if !seen[mode] {
			return false
		}
	}
	return true
}

// validateNFSRegistrationPath accepts the approved root itself or any
// canonical descendant. The legacy strict-descendant rule is not changed.
func validateNFSRegistrationPath(raw string, roots []string) (string, error) {
	if raw == "" || len(raw) > maxRegistrationPath || !utf8.ValidString(raw) || strings.Contains(raw, "\\") || !path.IsAbs(raw) || path.Clean(raw) != raw {
		return "", errors.New("canonical absolute NFS path required")
	}
	if raw == "/" || strings.HasSuffix(raw, "/") || strings.Contains(raw, "//") || containsAmbiguousEncoding(raw) {
		return "", errors.New("canonical non-root NFS path required")
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return "", errors.New("NFS path contains control characters")
		}
	}
	for _, root := range roots {
		if root == "" || root == "/" || !path.IsAbs(root) || path.Clean(root) != root || strings.Contains(root, "\\") {
			continue
		}
		root = path.Clean(root)
		if raw == root || strings.HasPrefix(raw, strings.TrimSuffix(root, "/")+"/") {
			return raw, nil
		}
	}
	return "", errors.New("NFS path is outside every approved root")
}

func joinBeeGFSPath(prefix, suffix string) (string, error) {
	if suffix == "" || len(suffix) > maxBeeGFSRelative || !utf8.ValidString(suffix) {
		return "", errors.New("canonical nonempty BeeGFS relative path required")
	}
	if path.IsAbs(suffix) || path.Clean(suffix) != suffix || suffix == "." || suffix == ".." || strings.HasPrefix(suffix, "../") {
		return "", errors.New("BeeGFS path must be a canonical relative suffix")
	}
	// The BeeGFS driver decodes url.Path and joins it. A percent escape,
	// backslash, query, fragment, credentials marker or double separator can
	// change meaning after decoding, so only plain canonical segments are
	// accepted. Encoded traversal is therefore refused by refusing every "%".
	if strings.ContainsAny(suffix, "%\\?#@:") || strings.Contains(suffix, "//") {
		return "", errors.New("BeeGFS path contains an unsafe or ambiguous segment")
	}
	for _, r := range suffix {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", errors.New("BeeGFS path contains control or whitespace characters")
		}
	}
	u, err := url.Parse(prefix)
	if err != nil || u.Scheme != "beegfs" || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid fixed BeeGFS prefix")
	}
	if u.Path == "" || !path.IsAbs(u.Path) || path.Clean(u.Path) != u.Path || strings.HasSuffix(u.Path, "/") {
		return "", errors.New("invalid fixed BeeGFS prefix")
	}
	base := strings.TrimSuffix(prefix, "/")
	joined := base + "/" + suffix
	// The installed driver joins the decoded path; enforce the fixed authority
	// and the exact prefix with no meaning-changing normalization.
	parsed, err := url.Parse(joined)
	if err != nil || parsed.Scheme != u.Scheme || parsed.Host != u.Host || parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("BeeGFS suffix escapes the fixed prefix")
	}
	if parsed.Path != u.Path+"/"+suffix || !strings.HasPrefix(parsed.Path, u.Path+"/") || path.Clean(parsed.Path) != parsed.Path {
		return "", errors.New("BeeGFS suffix escapes the fixed prefix")
	}
	if len(joined) > maxBeeGFSHandleBytes {
		return "", errors.New("BeeGFS handle exceeds the supported byte bound")
	}
	return joined, nil
}

func beeGFSHandleUnderPrefix(handle, prefix string) bool {
	if handle == "" || len(handle) > maxBeeGFSHandleBytes || strings.ContainsAny(handle, "%\\?#@") {
		return false
	}
	for _, r := range handle {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return false
		}
	}
	hu, err := url.Parse(handle)
	if err != nil || hu.Scheme != "beegfs" || hu.Host == "" || hu.User != nil || hu.Opaque != "" || hu.RawQuery != "" || hu.Fragment != "" || hu.Path == "" || path.Clean(hu.Path) != hu.Path {
		return false
	}
	pu, err := url.Parse(prefix)
	if err != nil || pu.Scheme != hu.Scheme || pu.Host != hu.Host || pu.User != nil || pu.Opaque != "" || pu.RawQuery != "" || pu.Fragment != "" {
		return false
	}
	base := strings.TrimSuffix(path.Clean(pu.Path), "/")
	return hu.Path != base && strings.HasPrefix(hu.Path, base+"/")
}

// sameBackendChecked is the strict uniqueness comparator used before every
// static PV create/reserve. It never silently treats an ambiguous existing
// handle as a different backend: malformed relevant sources fail the plan.
func sameBackendChecked(a, b *corev1.PersistentVolume) (bool, error) {
	if a.Spec.NFS != nil && b.Spec.NFS != nil {
		return sameNFSBackendChecked(a.Spec.NFS, b.Spec.NFS)
	}
	if a.Spec.CSI != nil && b.Spec.CSI != nil {
		if a.Spec.CSI.Driver != b.Spec.CSI.Driver {
			return false, nil
		}
		if a.Spec.CSI.VolumeHandle == b.Spec.CSI.VolumeHandle {
			return true, nil
		}
		if a.Spec.CSI.Driver == config.BeeGFSDriverName {
			return sameBeeGFSBackendChecked(a.Spec.CSI.VolumeHandle, b.Spec.CSI.VolumeHandle)
		}
		return false, nil
	}
	return false, nil
}

func sameNFSBackendChecked(a, b *corev1.NFSVolumeSource) (bool, error) {
	as, err := canonicalNFSHost(a.Server)
	if err != nil {
		return false, err
	}
	bs, err := canonicalNFSHost(b.Server)
	if err != nil {
		return false, err
	}
	if as != bs {
		return false, nil
	}
	ap, err := canonicalExistingNFSPath(a.Path)
	if err != nil {
		return false, err
	}
	bp, err := canonicalExistingNFSPath(b.Path)
	if err != nil {
		return false, err
	}
	return pathWithin(ap, bp) || pathWithin(bp, ap), nil
}

func canonicalNFSHost(raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", errors.New("an existing NFS PersistentVolume has an ambiguous server identity; review manually before creating storage")
	}
	host := strings.ToLower(strings.TrimSuffix(raw, "."))
	if host == "" || strings.ContainsAny(host, "/\\") {
		return "", errors.New("an existing NFS PersistentVolume has an ambiguous server identity; review manually before creating storage")
	}
	for _, r := range host {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return "", errors.New("an existing NFS PersistentVolume has an ambiguous server identity; review manually before creating storage")
		}
	}
	return host, nil
}

func canonicalExistingNFSPath(raw string) (string, error) {
	if raw == "" || len(raw) > maxRegistrationPath || !utf8.ValidString(raw) || strings.Contains(raw, "\\") || !path.IsAbs(raw) || path.Clean(raw) != raw || strings.Contains(raw, "//") || strings.HasSuffix(raw, "/") || containsAmbiguousEncoding(raw) {
		return "", errors.New("an existing NFS PersistentVolume has an ambiguous backend path; review manually before creating storage")
	}
	for _, r := range raw {
		if unicode.IsControl(r) {
			return "", errors.New("an existing NFS PersistentVolume has an ambiguous backend path; review manually before creating storage")
		}
	}
	return raw, nil
}

func sameBeeGFSBackendChecked(a, b string) (bool, error) {
	as, ah, ap, err := canonicalBeeGFSHandlePath(a)
	if err != nil {
		return false, err
	}
	bs, bh, bp, err := canonicalBeeGFSHandlePath(b)
	if err != nil {
		return false, err
	}
	if as != bs || ah != bh {
		return false, nil
	}
	// Dot segments, duplicate/trailing separators and percent escapes are
	// canonicalized for overlap detection. The strict creation validator still
	// refuses to create a malformed new handle.
	return pathWithin(ap, bp) || pathWithin(bp, ap), nil
}

func canonicalBeeGFSHandlePath(raw string) (scheme, host, cleaned string, err error) {
	if raw == "" || !utf8.ValidString(raw) {
		return "", "", "", errors.New("an existing BeeGFS PersistentVolume has an ambiguous handle; review manually before creating storage")
	}
	u, parseErr := url.Parse(raw)
	if parseErr != nil || !strings.EqualFold(u.Scheme, "beegfs") || u.Host == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.Path == "" || !path.IsAbs(u.Path) {
		return "", "", "", errors.New("an existing BeeGFS PersistentVolume has an ambiguous handle; review manually before creating storage")
	}
	if strings.Contains(u.Path, "\\") {
		return "", "", "", errors.New("an existing BeeGFS PersistentVolume has an ambiguous handle; review manually before creating storage")
	}
	for _, r := range u.Path {
		if unicode.IsControl(r) {
			return "", "", "", errors.New("an existing BeeGFS PersistentVolume has an ambiguous handle; review manually before creating storage")
		}
	}
	host = strings.ToLower(strings.TrimSuffix(u.Host, "."))
	if host == "" {
		return "", "", "", errors.New("an existing BeeGFS PersistentVolume has an ambiguous handle; review manually before creating storage")
	}
	return strings.ToLower(u.Scheme), host, path.Clean(u.Path), nil
}

func requiredDirectoryIDs(uid, gid string) (string, string, error) {
	u, err := canonicalDirectoryID(uid)
	if err != nil {
		return "", "", errors.New("a required canonical nonzero NFS directory UID is missing or invalid")
	}
	g, err := canonicalDirectoryID(gid)
	if err != nil {
		return "", "", errors.New("a required canonical nonzero NFS directory GID is missing or invalid")
	}
	return u, g, nil
}

// canonicalDirectoryID accepts only a canonical nonzero decimal ID. Leading
// zeros, signs, whitespace, control characters and overflow are refused.
func canonicalDirectoryID(raw string) (string, error) {
	if raw == "" || len(raw) > 10 || raw[0] == '0' {
		return "", errors.New("invalid directory ID")
	}
	for _, c := range raw {
		if c < '0' || c > '9' {
			return "", errors.New("invalid directory ID")
		}
	}
	v, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || v == 0 || v > maxDirectoryID {
		return "", errors.New("invalid directory ID")
	}
	return strconv.FormatUint(v, 10), nil
}

func containsAmbiguousEncoding(value string) bool {
	lower := strings.ToLower(value)
	return strings.Contains(lower, "%2e") || strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c")
}

func shortDigest(value string) string {
	d := digest(value)
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

func registrationTemplateView(t config.RegistrationTemplate) model.StorageRegistrationTemplate {
	return model.StorageRegistrationTemplate{
		Name: t.Name, Kind: t.Kind, StorageClass: t.StorageClass,
		PVCapacity: t.Capacity, PVAccessModes: append([]string(nil), t.AccessModes...),
		PVCRequest: t.PVCRequest, PVCAccessModes: append([]string(nil), t.PVCAccessModes...),
		ReclaimPolicy: string(corev1.PersistentVolumeReclaimRetain), VolumeMode: string(corev1.PersistentVolumeFilesystem),
		NFSServer: t.NFSServer, NFSRoots: append([]string(nil), t.NFSRoots...), NFSMountOptions: append([]string(nil), t.MountOptions...),
		RequireDirectoryUIDGID: t.RequireDirectoryUIDGID, BeeGFSDriver: t.BeeGFSDriver, BeeGFSPrefix: t.BeeGFSPrefix,
		PodDefaultName: t.PodDefaultName, PodDefaultMountPathPrefix: t.PodDefaultMountPathPrefix,
	}
}

// StorageRegistrationTemplates returns the approved browser-safe projection.
// It is empty unless writes and static storage are both enabled; the backend
// remains authoritative even if a browser forges a template name.
func (s *Service) StorageRegistrationTemplates() []model.StorageRegistrationTemplate {
	if !s.Config.Features.WritesEnabled || !s.Config.Features.StaticStorage {
		return nil
	}
	out := make([]model.StorageRegistrationTemplate, 0, len(s.Config.Storage.RegistrationTemplates))
	for _, t := range s.Config.Storage.RegistrationTemplates {
		out = append(out, registrationTemplateView(t))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// registrationTemplateForPV returns the unique matching template name for an
// existing PV, or no match. It exposes no backend path or handle; only the
// template name is projected to inventory.
func (s *Service) registrationTemplateForPV(pv *corev1.PersistentVolume) (string, bool) {
	found := ""
	for i := range s.Config.Storage.RegistrationTemplates {
		t := &s.Config.Storage.RegistrationTemplates[i]
		if !s.pvMatchesRegistrationTemplate(pv, t) {
			continue
		}
		if found != "" {
			return "", false
		}
		found = t.Name
	}
	return found, found != ""
}
