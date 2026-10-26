package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"kubeflow-admin/internal/model"
)

// storageReclaimTarget is the reviewed identity of the Retain PersistentVolume
// whose stale claimRef is removed after its claim is deleted. Kubernetes leaves
// such a volume Released with the old claimRef, which blocks every new binding
// until an administrator clears it. Only the claimRef is ever cleared, and only
// for exactly this volume UID and exactly this deleted claim UID.
type storageReclaimTarget struct {
	name            string
	uid             types.UID
	resourceVersion string
	claimNamespace  string
	claimName       string
	claimUID        types.UID
	className       string
	// specDigest covers the whole reviewed volume spec except claimRef, so a
	// changed backend source, class, capacity or reclaim policy is detected.
	specDigest string
}

func newStorageReclaimTarget(pv *corev1.PersistentVolume, pvc *corev1.PersistentVolumeClaim) *storageReclaimTarget {
	return &storageReclaimTarget{
		name: pv.Name, uid: pv.UID, resourceVersion: pv.ResourceVersion,
		claimNamespace: pvc.Namespace, claimName: pvc.Name, claimUID: pvc.UID,
		className:  pv.Spec.StorageClassName,
		specDigest: reclaimSpecDigest(pv),
	}
}

func reclaimSpecDigest(pv *corev1.PersistentVolume) string {
	spec := pv.Spec.DeepCopy()
	spec.ClaimRef = nil
	return digest(spec)
}

// storageReclaimOps removes only spec.claimRef. The tests make the patch
// self-checking: it fails unless the claimRef still names the deleted claim and
// the volume still has the Retain policy. The caller adds the UID and
// resourceVersion tests with conditional().
func storageReclaimOps(t *storageReclaimTarget) []patchOp {
	return []patchOp{
		{"test", "/spec/claimRef/namespace", t.claimNamespace},
		{"test", "/spec/claimRef/name", t.claimName},
		{"test", "/spec/claimRef/uid", string(t.claimUID)},
		{"test", "/spec/persistentVolumeReclaimPolicy", string(corev1.PersistentVolumeReclaimRetain)},
		{"remove", "/spec/claimRef", nil},
	}
}

func storageReclaimChanges(t *storageReclaimTarget) []model.Change {
	return []model.Change{
		{Field: "Post-delete volume reclaim", Before: "PersistentVolume " + t.name + " (Retain) becomes Released with a stale claimRef to " + t.claimNamespace + "/" + t.claimName, After: "only spec.claimRef is removed so the volume returns to Available"},
		{Field: "Reclaim volume identity", Before: "uid " + string(t.uid), After: "exact UID, deleted-claim UID and current resourceVersion preconditions, re-read after the claim is confirmed gone"},
	}
}

func storageReclaimWarnings(t *storageReclaimTarget) []string {
	return []string{
		"After the claim is confirmed deleted, this console removes only the stale spec.claimRef from PersistentVolume " + t.name + " with a patch conditioned on its UID, the deleted claim and its current resourceVersion, so the volume returns to Available. Available means any matching or pre-bound claim can bind it, and it can then be reused or deleted from this console. The reclaim policy, backend source, status and finalizers are never modified.",
		"The reclaim is not a data wipe. Retain keeps the backend data intact; the next claim that binds this volume can see that data. The reclaim is refused if the volume was replaced or changed, if any claim in any namespace names it, or if a replacement claim with the deleted claim's namespace and name exists. The claim-list check is point-in-time: a claim created between that check and the patch can race the clear, in which case post-patch verification stops for review.",
		"If the reclaim cannot be completed or verified, the claim stays deleted and is never recreated, the volume is not rolled back and its state may be unverified (a patch may have applied before a timeout or server error), and the operation stops as needs-review. There is no rollback, retry or forced cleanup.",
	}
}

// reclaimRetainedVolume runs after the claim deletion is proven. Every failure
// here is a partial outcome: the claim is already gone, so the error is never a
// pre-mutation rejection and nothing is rolled back.
func (s *Service) reclaimRetainedVolume(ctx context.Context, plan *storageDeletePlan, cp func(string) error) error {
	t := plan.reclaim
	// Observation only: wait for the PersistentVolume controller to release the
	// volume. Any state other than Bound-to-the-deleted-claim fails immediately.
	if err := s.wait(ctx, func() (bool, error) {
		pv, pending, err := s.inspectReclaimVolume(ctx, t)
		return pv != nil && !pending, err
	}); err != nil {
		return err
	}
	if err := checkpoint(cp, storageReclaimStep); err != nil {
		return err
	}
	// Final validation immediately before the one write: re-read the exact
	// volume, require Released with the deleted claim's claimRef and refuse if
	// any claim names the volume.
	pv, pending, err := s.inspectReclaimVolume(ctx, t)
	if err != nil {
		return err
	}
	if pending || pv == nil {
		return errors.New("the retained PersistentVolume is not Released; reclaim stopped for review")
	}
	if err := s.rejectReclaimClaims(ctx, t); err != nil {
		return err
	}
	return s.patchAndVerifyReclaim(ctx, t, pv)
}

// Shared by explicit reclaim of an old Released PV and optional post-delete
// reclaim. Callers must durably checkpoint and revalidate before entering;
// every error here is conservatively treated as a possibly applied mutation.
func (s *Service) patchAndVerifyReclaim(ctx context.Context, t *storageReclaimTarget, pv *corev1.PersistentVolume) error {
	body, err := json.Marshal(conditional(string(pv.UID), pv.ResourceVersion, storageReclaimOps(t)))
	if err != nil {
		return errors.New("PersistentVolume reclaim patch could not be encoded")
	}
	patched, err := s.Typed.CoreV1().PersistentVolumes().Patch(ctx, t.name, types.JSONPatchType, body, metav1.PatchOptions{})
	if err != nil {
		return storagePostMutationError(err, storageKindPV, "reclaim")
	}
	if patched == nil || patched.UID != t.uid {
		return fmt.Errorf("PersistentVolume identity changed during the reclaim: %w", ErrConflict)
	}
	var available *corev1.PersistentVolume
	if err := s.wait(ctx, func() (bool, error) {
		live, err := s.Typed.CoreV1().PersistentVolumes().Get(ctx, t.name, metav1.GetOptions{})
		if err != nil {
			return false, errors.New("reclaimed PersistentVolume could not be verified")
		}
		if live.UID != t.uid {
			return false, fmt.Errorf("PersistentVolume was replaced during the reclaim: %w", ErrConflict)
		}
		if live.DeletionTimestamp != nil {
			return false, errors.New("reclaimed PersistentVolume is terminating")
		}
		if live.Spec.ClaimRef != nil {
			return false, errors.New("a claim references the reclaimed PersistentVolume; reclaim stopped for review")
		}
		if _, legacy := live.Annotations[corev1.BetaStorageClassAnnotation]; legacy {
			return false, errors.New("reclaimed PersistentVolume has a deprecated beta StorageClass annotation")
		}
		if live.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain || reclaimSpecDigest(live) != t.specDigest {
			return false, errors.New("reclaimed PersistentVolume does not match the reviewed volume")
		}
		switch live.Status.Phase {
		case corev1.VolumeAvailable:
			available = live
			return true, nil
		case corev1.VolumeReleased:
			// The controller has not yet processed the removed claimRef.
			return false, nil
		default:
			return false, errors.New("reclaimed PersistentVolume is not Available; reclaim stopped for review")
		}
	}); err != nil {
		return err
	}
	return recordMutationReceipt(ctx, mutationKindPersistentVolume, "", available.Name, available.UID, available.ResourceVersion, model.MutationOutcomeAvailable)
}

// inspectReclaimVolume re-reads the reviewed volume. pending is true only while
// it is still Bound to the deleted claim, which means the PersistentVolume
// controller has not released it yet. A replaced volume, a missing or different
// claimRef, a changed spec, a changed reclaim policy or any other phase is an
// error: the stale claimRef is only ever cleared when the state is exactly the
// expected Released one.
func (s *Service) inspectReclaimVolume(ctx context.Context, t *storageReclaimTarget) (pv *corev1.PersistentVolume, pending bool, err error) {
	pv, err = s.Typed.CoreV1().PersistentVolumes().Get(ctx, t.name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, false, errors.New("the retained PersistentVolume no longer exists; reclaim stopped for review")
	}
	if err != nil {
		return nil, false, errors.New("the retained PersistentVolume could not be verified")
	}
	if pv.UID != t.uid {
		return nil, false, fmt.Errorf("the retained PersistentVolume was replaced after the claim deletion: %w", ErrConflict)
	}
	if _, legacy := pv.Annotations[corev1.BetaStorageClassAnnotation]; legacy {
		return nil, false, errors.New("retained PersistentVolume has a deprecated beta StorageClass annotation; reclaim stopped for review")
	}
	if pv.DeletionTimestamp != nil || pv.ResourceVersion == "" {
		return nil, false, errors.New("the retained PersistentVolume is not stable; reclaim stopped for review")
	}
	ref := pv.Spec.ClaimRef
	if ref == nil {
		return nil, false, errors.New("the retained PersistentVolume no longer has a claimRef; reclaim stopped for review")
	}
	if ref.Namespace != t.claimNamespace || ref.Name != t.claimName || ref.UID != t.claimUID {
		return nil, false, fmt.Errorf("the retained PersistentVolume now names a different claim: %w", ErrConflict)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain || reclaimSpecDigest(pv) != t.specDigest {
		return nil, false, fmt.Errorf("the retained PersistentVolume changed after the claim deletion: %w", ErrConflict)
	}
	switch pv.Status.Phase {
	case corev1.VolumeReleased:
		return pv, false, nil
	case corev1.VolumeBound:
		return nil, true, nil
	default:
		return nil, false, errors.New("the retained PersistentVolume is neither Released nor Bound; reclaim stopped for review")
	}
}

// rejectReclaimClaims fails closed when any claim in any namespace names the
// volume, when the deleted claim still exists, when a replacement claim with the
// deleted claim's namespace and name exists (whatever its UID or volumeName), or
// when the list cannot be proven complete. Removing the claimRef would otherwise
// let such a claim bind the retained data.
func (s *Service) rejectReclaimClaims(ctx context.Context, t *storageReclaimTarget) error {
	claims, err := s.Typed.CoreV1().PersistentVolumeClaims(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return errors.New("PersistentVolumeClaim reference list could not be verified")
	}
	if claims.GetContinue() != "" {
		return errors.New("PersistentVolumeClaim reference list is incomplete")
	}
	for _, claim := range claims.Items {
		if claim.UID == t.claimUID {
			return errors.New("the deleted PersistentVolumeClaim reappeared; reclaim stopped for review")
		}
		if claim.Spec.VolumeName == t.name {
			return errors.New("a PersistentVolumeClaim references the retained PersistentVolume; reclaim stopped for review")
		}
		if claim.Namespace == t.claimNamespace && claim.Name == t.claimName {
			return errors.New("a replacement PersistentVolumeClaim with the deleted claim's namespace and name exists and could bind the retained PersistentVolume; reclaim stopped for review")
		}
		if effectiveClaimClass(&claim) == t.className && claim.Status.Phase != corev1.ClaimBound {
			return errors.New("an unbound PersistentVolumeClaim can bind the retained PersistentVolume; reclaim stopped for review")
		}
	}
	return nil
}

// Admission isolation is operator-managed; also refuse pre-existing tenant or
// controller-created unbound claims rather than trusting a configuration claim.
func (s *Service) rejectUnboundClassClaims(ctx context.Context, class string) error {
	claims, err := s.Typed.CoreV1().PersistentVolumeClaims(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil || claims.GetContinue() != "" {
		return errors.New("complete PVC reference list required before exposing an unreserved PV")
	}
	for _, claim := range claims.Items {
		if effectiveClaimClass(&claim) == class && claim.Status.Phase != corev1.ClaimBound {
			return errors.New("an unbound PVC already uses this isolated StorageClass")
		}
	}
	return nil
}

// Match the PV controller: the deprecated beta annotation takes precedence
// over spec.storageClassName, including when the annotation is explicitly empty.
func effectiveClaimClass(claim *corev1.PersistentVolumeClaim) string {
	if class, present := claim.Annotations[corev1.BetaStorageClassAnnotation]; present {
		return class
	}
	if claim.Spec.StorageClassName != nil {
		return *claim.Spec.StorageClassName
	}
	return ""
}
