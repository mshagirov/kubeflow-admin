package kube

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"kubeflow-admin/internal/model"
)

type pvReclaimPlan struct {
	prepared model.Prepared
	target   *storageReclaimTarget
}

func standalonePVReclaimWarnings(t *storageReclaimTarget) []string {
	return []string{
		"Returns the reviewed Released Retain PersistentVolume " + t.name + " to Available by removing only its stale spec.claimRef. The previous claim is already absent. This action does not delete or create any PVC or PV, and never edits status, reclaim policy, backend source or finalizers. Kubernetes must report Available before success is recorded.",
		"This is not a data wipe. Existing backend data remains and any matching or pre-bound claim in any namespace can bind the Available volume and access that data. Operator-enforced StorageClass isolation must be verified before reclaim; merely listing a class in configuration does not enforce isolation.",
		"Replacement claims, claims naming this volume and unbound matching-class claims block reclaim. Claim observations are point-in-time and cannot atomically fence external writers; coordinate admission and other operators.",
		"If the patch or its convergence cannot be verified, it may already have applied. The operation stops as needs-review with the PV locked for operator inspection. Do not retry or restore claimRef blindly. There is no rollback, automatic replay or forced cleanup.",
	}
}

func pvReclaimActionConstraints(a model.Action) error {
	if a.Kind != "pv.reclaim" {
		return errors.New("unsupported PV reclaim action")
	}
	if err := storageActionFields(a, "Kind", "Name", "Reason", "ConfirmName", "TargetUID", "TargetResourceVersion"); err != nil {
		return err
	}
	if err := name(a.Name); err != nil {
		return err
	}
	if a.TargetUID == "" || a.TargetResourceVersion == "" {
		return errors.New("PV reclaim requires its exact current UID and resourceVersion")
	}
	if a.ConfirmName != a.Name {
		return errors.New("typed confirmation must exactly match the PV name")
	}
	if strings.TrimSpace(a.Reason) == "" || utf8.RuneCountInString(a.Reason) > 500 {
		return errors.New("PV reclaim requires a reason of at most 500 characters")
	}
	return nil
}

func (s *Service) releasedPVReclaimTarget(ctx context.Context, objectName string, ev *evidence) (*corev1.PersistentVolume, *storageReclaimTarget, error) {
	if !s.Config.Features.WritesEnabled || !s.Config.Features.PVReclaim {
		return nil, nil, errors.New("PV reclaim is disabled")
	}
	if err := name(objectName); err != nil {
		return nil, nil, err
	}
	pv, err := s.Typed.CoreV1().PersistentVolumes().Get(ctx, objectName, metav1.GetOptions{})
	if err != nil {
		return nil, nil, safe(err)
	}
	if pv.UID == "" || pv.ResourceVersion == "" || pv.DeletionTimestamp != nil {
		return nil, nil, errors.New("PV reclaim requires a stable nonterminating PersistentVolume")
	}
	if pv.Status.Phase != corev1.VolumeReleased || pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		return nil, nil, errors.New("PV reclaim requires a Released PersistentVolume with Retain policy")
	}
	if _, legacy := pv.Annotations[corev1.BetaStorageClassAnnotation]; legacy {
		return nil, nil, errors.New("PV reclaim refuses deprecated beta StorageClass annotations")
	}
	if !has(s.Config.Storage.IsolatedStorageClasses, pv.Spec.StorageClassName) {
		return nil, nil, errors.New("PV reclaim requires an operator-confirmed isolated StorageClass")
	}
	if _, err := s.class(ctx, pv.Spec.StorageClassName, ev); err != nil {
		return nil, nil, err
	}
	ref := pv.Spec.ClaimRef
	if ref == nil || ref.UID == "" || ref.Namespace == "" || ref.Name == "" || (ref.Kind != "" && ref.Kind != "PersistentVolumeClaim") || (ref.APIVersion != "" && ref.APIVersion != "v1") {
		return nil, nil, errors.New("PV reclaim requires a complete stale PersistentVolumeClaim reference")
	}
	if err := s.namespacePolicy(ref.Namespace); err != nil {
		return nil, nil, err
	}
	if err := name(ref.Name); err != nil {
		return nil, nil, err
	}
	// NotFound is the only positive evidence of absence. A replacement claim
	// under the same name also blocks reclaim, irrespective of its UID/class.
	if _, err := s.Typed.CoreV1().PersistentVolumeClaims(ref.Namespace).Get(ctx, ref.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		if err != nil {
			return nil, nil, safe(err)
		}
		return nil, nil, errors.New("the previous claim name still exists; PV reclaim is refused")
	}
	t := newStorageReclaimTarget(pv, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ref.Namespace, Name: ref.Name, UID: ref.UID}})
	if err := s.rejectReclaimClaims(ctx, t); err != nil {
		return nil, nil, err
	}
	ev.add("PersistentVolume", "", pv.Name, storagePVFingerprint(pv))
	return pv, t, nil
}

func (s *Service) buildPVReclaim(ctx context.Context, a model.Action) (*pvReclaimPlan, error) {
	if err := pvReclaimActionConstraints(a); err != nil {
		return nil, err
	}
	ev := s.newEvidence()
	pv, target, err := s.releasedPVReclaimTarget(ctx, a.Name, &ev)
	if err != nil {
		return nil, err
	}
	if string(pv.UID) != a.TargetUID || pv.ResourceVersion != a.TargetResourceVersion {
		return nil, fmt.Errorf("PersistentVolume changed before reclaim: %w", ErrConflict)
	}
	if err := s.storageEditDryRun(ctx, storageKindPV, "", pv.Name, pv.UID, pv.ResourceVersion, storageReclaimOps(target)); err != nil {
		return nil, err
	}
	p := model.Prepared{Action: a, TargetUID: string(pv.UID), ResourceVersion: pv.ResourceVersion,
		Summary: "pv.reclaim " + pv.Name, Data: ev.bytes(), Warnings: standalonePVReclaimWarnings(target),
		Changes: []model.Change{
			{Field: "PV availability", Before: "Released", After: "Available after stale claimRef removal and controller convergence"},
			{Field: "Previous claim", Before: target.claimNamespace + "/" + target.claimName + " (UID " + string(target.claimUID) + ")", After: "verified absent; only spec.claimRef is removed"},
		}}
	return &pvReclaimPlan{prepared: p, target: target}, nil
}

func (s *Service) preparePVReclaim(ctx context.Context, a model.Action) (model.Prepared, error) {
	p, err := s.buildPVReclaim(ctx, a)
	if err != nil {
		return model.Prepared{}, err
	}
	return p.prepared, nil
}

func (s *Service) executePVReclaim(ctx context.Context, p model.Prepared, cp func(string) error) (err error) {
	attempted := false
	defer func() {
		if err != nil && !attempted {
			err = errors.Join(model.ErrRejectedBeforeMutation, err)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	plan, err := s.buildPVReclaim(ctx, p.Action)
	if err != nil {
		return err
	}
	if !storagePreparedMatches(p, plan.prepared) {
		return ErrConflict
	}
	if err := checkpoint(cp, storageReclaimStep); err != nil {
		return err
	}
	fresh, err := s.buildPVReclaim(ctx, p.Action)
	if err != nil {
		return err
	}
	if !storagePreparedMatches(p, fresh.prepared) {
		return ErrConflict
	}
	pv, pending, err := s.inspectReclaimVolume(ctx, fresh.target)
	if err != nil {
		return err
	}
	if pending || pv == nil {
		return errors.New("PV reclaim requires a Released PersistentVolume with Retain policy")
	}
	if string(pv.UID) != p.Action.TargetUID || pv.ResourceVersion != p.Action.TargetResourceVersion {
		return fmt.Errorf("PersistentVolume changed at the final reclaim check: %w", ErrConflict)
	}
	if err := s.rejectReclaimClaims(ctx, fresh.target); err != nil {
		return err
	}
	attempted = true
	return s.patchAndVerifyReclaim(ctx, fresh.target, pv)
}

func (s *Service) pvReclaimDocument(ctx context.Context, objectName string) (model.StorageDocument, error) {
	ev := s.newEvidence()
	pv, target, err := s.releasedPVReclaimTarget(ctx, objectName, &ev)
	if err != nil {
		return model.StorageDocument{}, err
	}
	// The drawer needs identity and warnings only, not backend paths or YAML.
	return model.StorageDocument{Kind: storageKindPV, Name: pv.Name, UID: string(pv.UID), ResourceVersion: pv.ResourceVersion, YAML: "# Reclaim removes only the reviewed stale claimRef. No YAML is editable.\n", Warnings: standalonePVReclaimWarnings(target)}, nil
}
