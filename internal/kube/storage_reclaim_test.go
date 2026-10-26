package kube

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"kubeflow-admin/internal/model"
)

// reclaimHarness simulates the two PersistentVolume controller behaviors the
// reclaim depends on: a deleted claim leaves its volume Released with the stale
// claimRef, and a Released volume without a claimRef becomes Available. The
// typed fake does neither by itself.
type reclaimHarness struct {
	t   *testing.T
	s   *Service
	k   *fake.Clientset
	pvc *corev1.PersistentVolumeClaim
	pv  *corev1.PersistentVolume

	mu         sync.Mutex
	realPatch  [][]byte
	dryPatch   int
	afterClaim func(k *fake.Clientset) // runs after the claim is gone, before release
	boundGets  int                     // PV reads that still show Bound after the delete
	failPatch  error                   // injected for the real (non-dry) patch
	dryFail    error                   // injected for the dry-run patch
	afterPatch func(k *fake.Clientset) // runs on the first PV read after a real patch
	deleted    bool
	claimGone  bool
}

func newReclaimHarness(t *testing.T, policy corev1.PersistentVolumeReclaimPolicy) *reclaimHarness {
	t.Helper()
	s, _, k := storageService(t)
	s.Config.Features.PVReclaim = true
	s.Config.Storage.IsolatedStorageClasses = []string{"approved"}
	h := &reclaimHarness{t: t, s: s, k: k}
	pvc := pendingPVC("alice", "data", "data-uid")
	pvc.Spec.VolumeName = "data-pv"
	pvc.Status.Phase = corev1.ClaimBound
	pv := freePV("data-pv", "data-pv-uid", "3")
	pv.Spec.PersistentVolumeReclaimPolicy = policy
	pv.Spec.ClaimRef = &corev1.ObjectReference{Name: "data", Namespace: "alice", UID: "data-uid"}
	pv.Status.Phase = corev1.VolumeBound
	ctx := context.Background()
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.pvc, h.pv = pvc, pv

	// Real claim delete: remove the claim, run the hook, then release the
	// volume the way the PersistentVolume controller does for Retain.
	k.PrependReactor("delete", "persistentvolumeclaims", func(a ktesting.Action) (bool, runtime.Object, error) {
		da, ok := a.(ktesting.DeleteActionImpl)
		if !ok || len(da.GetDeleteOptions().DryRun) > 0 {
			return false, nil, nil
		}
		if err := k.Tracker().Delete(pvcGVR, da.GetNamespace(), da.GetName()); err != nil {
			return true, nil, err
		}
		h.mu.Lock()
		h.claimGone = true
		hook := h.afterClaim
		h.mu.Unlock()
		if hook != nil {
			hook(k)
		}
		return true, nil, nil
	})
	k.PrependReactor("get", "persistentvolumes", func(a ktesting.Action) (bool, runtime.Object, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		obj, err := k.Tracker().Get(pvGVR, "", a.(ktesting.GetAction).GetName())
		if err != nil {
			return false, nil, nil
		}
		live := obj.(*corev1.PersistentVolume).DeepCopy()
		changed := false
		if h.claimGone && live.Status.Phase == corev1.VolumeBound && live.Spec.ClaimRef != nil && live.Spec.ClaimRef.UID == "data-uid" {
			if h.boundGets > 0 {
				h.boundGets--
			} else {
				live.Status.Phase, changed = corev1.VolumeReleased, true
			}
		}
		if live.Status.Phase == corev1.VolumeReleased && live.Spec.ClaimRef == nil {
			live.Status.Phase, changed = corev1.VolumeAvailable, true
		}
		if changed {
			if err := k.Tracker().Update(pvGVR, live, ""); err != nil {
				return true, nil, err
			}
		}
		if h.afterPatch != nil && len(h.realPatch) > 0 {
			// Runs on the first read after the real patch was applied.
			hook := h.afterPatch
			h.afterPatch = nil
			hook(k)
		}
		return false, nil, nil
	})
	k.PrependReactor("patch", "persistentvolumes", func(a ktesting.Action) (bool, runtime.Object, error) {
		pa, ok := a.(ktesting.PatchActionImpl)
		if !ok {
			return false, nil, nil
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		if len(pa.GetPatchOptions().DryRun) > 0 {
			h.dryPatch++
			if h.dryFail != nil {
				return true, nil, h.dryFail
			}
			return false, nil, nil
		}
		h.realPatch = append(h.realPatch, append([]byte(nil), pa.GetPatch()...))
		if h.failPatch != nil {
			return true, nil, h.failPatch
		}
		return false, nil, nil
	})
	return h
}

// installPVController gives a fake clientset the PersistentVolume controller
// behavior a Retain reclaim depends on, for tests that delete a bound PVC and do
// not care about the reclaim itself: a deleted claim releases its Retain volume
// (claimRef kept), the JSON patch is applied and a Released volume without a
// claimRef becomes Available.
func installPVController(k *fake.Clientset) {
	installPVPatchReactor(k)
	k.PrependReactor("delete", "persistentvolumeclaims", func(a ktesting.Action) (bool, runtime.Object, error) {
		da, ok := a.(ktesting.DeleteActionImpl)
		if !ok || len(da.GetDeleteOptions().DryRun) > 0 {
			return false, nil, nil
		}
		obj, err := k.Tracker().Get(pvcGVR, da.GetNamespace(), da.GetName())
		if err != nil {
			return false, nil, nil
		}
		pvc := obj.(*corev1.PersistentVolumeClaim)
		if err := k.Tracker().Delete(pvcGVR, da.GetNamespace(), da.GetName()); err != nil {
			return true, nil, err
		}
		if pvc.Spec.VolumeName != "" {
			if pvObj, err := k.Tracker().Get(pvGVR, "", pvc.Spec.VolumeName); err == nil {
				pv := pvObj.(*corev1.PersistentVolume).DeepCopy()
				if pv.Spec.ClaimRef != nil && pv.Spec.ClaimRef.UID == pvc.UID && pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimRetain {
					pv.Status.Phase = corev1.VolumeReleased
					_ = k.Tracker().Update(pvGVR, pv, "")
				}
			}
		}
		return true, nil, nil
	})
	k.PrependReactor("get", "persistentvolumes", func(a ktesting.Action) (bool, runtime.Object, error) {
		obj, err := k.Tracker().Get(pvGVR, "", a.(ktesting.GetAction).GetName())
		if err != nil {
			return false, nil, nil
		}
		if pv := obj.(*corev1.PersistentVolume); pv.Status.Phase == corev1.VolumeReleased && pv.Spec.ClaimRef == nil {
			pv = pv.DeepCopy()
			pv.Status.Phase = corev1.VolumeAvailable
			_ = k.Tracker().Update(pvGVR, pv, "")
		}
		return false, nil, nil
	})
}

func (h *reclaimHarness) action() model.Action {
	return model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "data", TargetUID: "data-uid", TargetResourceVersion: "1", ConfirmName: "data", Reason: "verified unused", ConfirmPVReclaim: h.pv.Spec.PersistentVolumeReclaimPolicy == corev1.PersistentVolumeReclaimRetain}
}

type reclaimRun struct {
	err         error
	steps       []string
	receipts    []model.MutationReceipt
	prepared    model.Prepared
	prepareErr  error
	executeCall bool
}

func (h *reclaimHarness) run() reclaimRun {
	h.t.Helper()
	var out reclaimRun
	ctx := context.Background()
	out.prepared, out.prepareErr = h.s.Prepare(ctx, h.action())
	if out.prepareErr != nil {
		return out
	}
	ctx = model.WithMutationReceiptRecorder(ctx, func(r model.MutationReceipt) error {
		out.receipts = append(out.receipts, r)
		return nil
	})
	out.executeCall = true
	out.err = h.s.Execute(ctx, out.prepared, func(step string) error {
		out.steps = append(out.steps, step)
		return nil
	})
	return out
}

func (h *reclaimHarness) livePV() *corev1.PersistentVolume {
	h.t.Helper()
	pv, err := h.k.Tracker().Get(pvGVR, "", "data-pv")
	if err != nil {
		h.t.Fatal(err)
	}
	return pv.(*corev1.PersistentVolume)
}

func (h *reclaimHarness) claimExists() bool {
	_, err := h.k.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "data", metav1.GetOptions{})
	return err == nil
}

func (h *reclaimHarness) realPatches() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.realPatch)
}

func hasReceipt(receipts []model.MutationReceipt, kind string, outcome model.MutationOutcome) bool {
	for _, r := range receipts {
		if r.Kind == kind && r.Outcome == outcome {
			return true
		}
	}
	return false
}

func TestPVCDeleteReclaimsRetainedVolumeToAvailable(t *testing.T) {
	h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
	h.boundGets = 3 // the controller is slow to release; the reclaim must wait
	before := h.livePV().DeepCopy()
	r := h.run()
	if r.prepareErr != nil || r.err != nil {
		t.Fatalf("prepare=%v execute=%v", r.prepareErr, r.err)
	}
	preview := strings.Join(r.prepared.Warnings, "\n")
	for _, want := range []string{"spec.claimRef", "Available", "not a data wipe", "Retain keeps the backend data intact", "never recreated"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview does not disclose %q: %v", want, r.prepared.Warnings)
		}
	}
	changes := ""
	for _, c := range r.prepared.Changes {
		changes += c.Field + "|" + c.Before + "|" + c.After + "\n"
	}
	if !strings.Contains(changes, "Post-delete volume reclaim") || !strings.Contains(changes, "alice/data") {
		t.Fatalf("plan does not list the reclaim: %s", changes)
	}
	var evidence storageDeleteEvidence
	if err := json.Unmarshal(r.prepared.Data, &evidence); err != nil || evidence.ReclaimVolume != "data-pv" || evidence.ReclaimVolumeUID != "data-pv-uid" {
		t.Fatalf("evidence does not name the reclaimed volume: %v %+v", err, evidence)
	}
	if h.claimExists() {
		t.Fatal("claim not deleted")
	}
	pv := h.livePV()
	if pv.Spec.ClaimRef != nil || pv.Status.Phase != corev1.VolumeAvailable {
		t.Fatalf("volume not reclaimed: claimRef=%v phase=%s", pv.Spec.ClaimRef, pv.Status.Phase)
	}
	// Only claimRef changed: identity, labels, policy, source and finalizers stay.
	if pv.UID != before.UID || pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain ||
		reclaimSpecDigest(pv) != reclaimSpecDigest(before) || len(pv.Finalizers) != len(before.Finalizers) ||
		pv.Labels["team"] != "hidden" || pv.Annotations["secret-annotation"] != "hidden" {
		t.Fatalf("reclaim changed more than claimRef: %+v", pv)
	}
	// Exactly one real patch whose only mutation is remove /spec/claimRef,
	// guarded by UID, current resourceVersion, the deleted claim and Retain.
	if h.realPatches() != 1 || h.dryPatch < 2 {
		t.Fatalf("real patches=%d dry-run patches=%d", h.realPatches(), h.dryPatch)
	}
	var ops []patchOp
	if err := json.Unmarshal(h.realPatch[0], &ops); err != nil {
		t.Fatal(err)
	}
	removes, tests := 0, map[string]any{}
	for _, op := range ops {
		switch op.Op {
		case "remove":
			removes++
			if op.Path != "/spec/claimRef" {
				t.Fatalf("patch removes %s", op.Path)
			}
		case "test":
			tests[op.Path] = op.Value
		default:
			t.Fatalf("unexpected patch op %+v", op)
		}
	}
	if removes != 1 || tests["/metadata/uid"] != "data-pv-uid" || tests["/metadata/resourceVersion"] == nil ||
		tests["/spec/claimRef/uid"] != "data-uid" || tests["/spec/claimRef/name"] != "data" || tests["/spec/claimRef/namespace"] != "alice" ||
		tests["/spec/persistentVolumeReclaimPolicy"] != "Retain" {
		t.Fatalf("patch is not the exact conditional claimRef removal: %s", h.realPatch[0])
	}
	if !hasReceipt(r.receipts, mutationKindPersistentVolumeClaim, model.MutationOutcomeDeleted) ||
		!hasReceipt(r.receipts, mutationKindPersistentVolume, model.MutationOutcomeAvailable) {
		t.Fatalf("missing receipts: %+v", r.receipts)
	}
	if got := strings.Join(r.steps, ","); got != "delete-storage,reclaim-volume" {
		t.Fatalf("checkpoints = %s", got)
	}
	// The deleted receipt is durable before the reclaim receipt.
	if r.receipts[0].Outcome != model.MutationOutcomeDeleted {
		t.Fatalf("receipt order: %+v", r.receipts)
	}
}

func TestPVCDeleteDeletePolicyVolumeIsNeverReclaimed(t *testing.T) {
	h := newReclaimHarness(t, corev1.PersistentVolumeReclaimDelete)
	r := h.run()
	if r.prepareErr != nil || r.err != nil {
		t.Fatalf("prepare=%v execute=%v", r.prepareErr, r.err)
	}
	if h.dryPatch != 0 || h.realPatches() != 0 {
		t.Fatalf("a Delete-policy volume was patched: dry=%d real=%d", h.dryPatch, h.realPatches())
	}
	var evidence storageDeleteEvidence
	if err := json.Unmarshal(r.prepared.Data, &evidence); err != nil || evidence.ReclaimVolume != "" || evidence.ReclaimVolumeUID != "" {
		t.Fatalf("Delete-policy evidence names a reclaim volume: %v %+v", err, evidence)
	}
	if strings.Contains(strings.Join(r.prepared.Warnings, "\n"), "spec.claimRef") {
		t.Fatalf("Delete-policy preview announces a reclaim: %v", r.prepared.Warnings)
	}
	if hasReceipt(r.receipts, mutationKindPersistentVolume, model.MutationOutcomeAvailable) || len(r.steps) != 1 {
		t.Fatalf("unexpected reclaim activity: steps=%v receipts=%+v", r.steps, r.receipts)
	}
	// Normal controller behavior is untouched: the claimRef is still there.
	if pv := h.livePV(); pv.Spec.ClaimRef == nil {
		t.Fatal("Delete-policy volume lost its claimRef")
	}
}

func TestPVCDeletePendingClaimHasNoReclaim(t *testing.T) {
	s, _, k := storageService(t)
	ctx := context.Background()
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, pendingPVC("alice", "unused", "unused-uid"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	p, err := s.Prepare(ctx, model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "unused", TargetUID: "unused-uid", TargetResourceVersion: "1", ConfirmName: "unused", Reason: "verified unused"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(p.Warnings, "\n"), "spec.claimRef") || strings.Contains(string(p.Data), "reclaimVolume") {
		t.Fatalf("a claim with no volume announces a reclaim: %v %s", p.Warnings, p.Data)
	}
}

// A volume that is replaced, re-pointed or altered after the claim is gone must
// never have its claimRef cleared. The claim stays deleted and the outcome is a
// partial one, never a pre-mutation rejection.
func TestPVCDeleteReclaimRefusesChangedVolume(t *testing.T) {
	mutations := map[string]func(k *fake.Clientset){
		"replaced volume": func(k *fake.Clientset) {
			pv := freePV("data-pv", "replacement-uid", "9")
			pv.Spec.ClaimRef = &corev1.ObjectReference{Name: "data", Namespace: "alice", UID: "data-uid"}
			pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimRetain
			pv.Status.Phase = corev1.VolumeReleased
			if err := k.Tracker().Update(pvGVR, pv, ""); err != nil {
				panic(err)
			}
		},
		"claimRef names another claim": func(k *fake.Clientset) {
			mutateTrackedPV(k, func(pv *corev1.PersistentVolume) {
				pv.Spec.ClaimRef = &corev1.ObjectReference{Name: "other", Namespace: "bob", UID: "other-uid"}
			})
		},
		"claimRef has another UID": func(k *fake.Clientset) {
			mutateTrackedPV(k, func(pv *corev1.PersistentVolume) { pv.Spec.ClaimRef.UID = "recreated-claim-uid" })
		},
		"claimRef already removed": func(k *fake.Clientset) {
			mutateTrackedPV(k, func(pv *corev1.PersistentVolume) {
				pv.Spec.ClaimRef = nil
				pv.Status.Phase = corev1.VolumeAvailable
			})
		},
		"reclaim policy changed": func(k *fake.Clientset) {
			mutateTrackedPV(k, func(pv *corev1.PersistentVolume) {
				pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
			})
		},
		"backend source changed": func(k *fake.Clientset) {
			mutateTrackedPV(k, func(pv *corev1.PersistentVolume) { pv.Spec.CSI.VolumeHandle = "another-backend" })
		},
		"volume terminating": func(k *fake.Clientset) {
			mutateTrackedPV(k, func(pv *corev1.PersistentVolume) {
				now := metav1.Now()
				pv.DeletionTimestamp = &now
			})
		},
		"volume failed": func(k *fake.Clientset) {
			mutateTrackedPV(k, func(pv *corev1.PersistentVolume) { pv.Status.Phase = corev1.VolumeFailed })
		},
		"volume gone": func(k *fake.Clientset) {
			if err := k.Tracker().Delete(pvGVR, "", "data-pv"); err != nil {
				panic(err)
			}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
			h.afterClaim = mutate
			r := h.run()
			if r.prepareErr != nil {
				t.Fatal(r.prepareErr)
			}
			if r.err == nil || errors.Is(r.err, model.ErrRejectedBeforeMutation) {
				t.Fatalf("a refused reclaim must be a partial outcome, got %v", r.err)
			}
			if h.realPatches() != 0 {
				t.Fatalf("claimRef was cleared on a changed volume: %s", h.realPatch[0])
			}
			if h.claimExists() {
				t.Fatal("the claim must stay deleted and is never recreated")
			}
			if !hasReceipt(r.receipts, mutationKindPersistentVolumeClaim, model.MutationOutcomeDeleted) {
				t.Fatalf("completed claim deletion evidence is missing: %+v", r.receipts)
			}
			if hasReceipt(r.receipts, mutationKindPersistentVolume, model.MutationOutcomeAvailable) {
				t.Fatalf("an Available receipt was recorded without a reclaim: %+v", r.receipts)
			}
			if claimCreatedAfterDelete(h.k) {
				t.Fatal("the claim was recreated")
			}
		})
	}
}

// claimCreatedAfterDelete reports a PersistentVolumeClaim create issued after
// the real claim delete; the reclaim must never recreate the claim.
func claimCreatedAfterDelete(k *fake.Clientset) bool {
	deleted := false
	for _, a := range k.Actions() {
		if a.GetResource().Resource != "persistentvolumeclaims" {
			continue
		}
		if da, ok := a.(ktesting.DeleteActionImpl); ok && len(da.GetDeleteOptions().DryRun) == 0 {
			deleted = true
		}
		if deleted && a.GetVerb() == "create" {
			return true
		}
	}
	return false
}

func mutateTrackedPV(k *fake.Clientset, fn func(*corev1.PersistentVolume)) {
	obj, err := k.Tracker().Get(pvGVR, "", "data-pv")
	if err != nil {
		panic(err)
	}
	pv := obj.(*corev1.PersistentVolume).DeepCopy()
	fn(pv)
	if err := k.Tracker().Update(pvGVR, pv, ""); err != nil {
		panic(err)
	}
}

// Any claim in any namespace naming the volume blocks the reclaim: removing the
// claimRef would let that claim bind the retained data.
func TestPVCDeleteReclaimBlockedByNewClaimReference(t *testing.T) {
	h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
	h.afterClaim = func(k *fake.Clientset) {
		stealer := pendingPVC("bob", "stealer", "stealer-uid")
		stealer.Spec.VolumeName = "data-pv"
		if err := k.Tracker().Create(pvcGVR, stealer, "bob"); err != nil {
			panic(err)
		}
	}
	r := h.run()
	if r.prepareErr != nil {
		t.Fatal(r.prepareErr)
	}
	if r.err == nil || errors.Is(r.err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("expected a partial outcome, got %v", r.err)
	}
	if h.realPatches() != 0 {
		t.Fatal("claimRef was cleared while another claim names the volume")
	}
	if pv := h.livePV(); pv.Spec.ClaimRef == nil || pv.Status.Phase != corev1.VolumeReleased {
		t.Fatalf("volume was altered: %+v %s", pv.Spec.ClaimRef, pv.Status.Phase)
	}
	if !hasReceipt(r.receipts, mutationKindPersistentVolumeClaim, model.MutationOutcomeDeleted) {
		t.Fatalf("completed claim deletion evidence is missing: %+v", r.receipts)
	}
}

// A same-namespace/name replacement claim with a new UID and no volumeName,
// created after the deletion was confirmed, is not yet bound to anything, but it could bind the volume as soon as the
// claimRef is cleared and see the retained data, so it blocks the reclaim.
func TestPVCDeleteReclaimBlockedBySameNameReplacementClaim(t *testing.T) {
	h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
	// The replacement appears after the deletion was confirmed (two absent
	// observations), so only the final claim list before the patch can see it.
	created := false
	h.k.PrependReactor("list", "persistentvolumeclaims", func(a ktesting.Action) (bool, runtime.Object, error) {
		h.mu.Lock()
		inject := h.claimGone && !created
		h.mu.Unlock()
		if inject {
			created = true
			if err := h.k.Tracker().Create(pvcGVR, pendingPVC("alice", "data", "replacement-uid"), "alice"); err != nil {
				panic(err)
			}
		}
		return false, nil, nil
	})
	r := h.run()
	if r.prepareErr != nil {
		t.Fatal(r.prepareErr)
	}
	if r.err == nil || errors.Is(r.err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("expected a partial outcome, got %v", r.err)
	}
	if !created || !strings.Contains(r.err.Error(), "replacement PersistentVolumeClaim") {
		t.Fatalf("unexpected error wording: %v", r.err)
	}
	if h.realPatches() != 0 {
		t.Fatal("claimRef was cleared while a same-name replacement claim exists")
	}
	if pv := h.livePV(); pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != "data-uid" || pv.Status.Phase != corev1.VolumeReleased {
		t.Fatalf("volume was altered: %+v %s", pv.Spec.ClaimRef, pv.Status.Phase)
	}
	// The original claim stays deleted: only the replacement exists, and the
	// reclaim neither recreated nor removed anything.
	live, err := h.k.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "data", metav1.GetOptions{})
	if err != nil || live.UID != "replacement-uid" {
		t.Fatalf("expected only the replacement claim to exist: %v %+v", err, live)
	}
	if claimCreatedAfterDelete(h.k) {
		t.Fatal("the claim was recreated")
	}
	if !hasReceipt(r.receipts, mutationKindPersistentVolumeClaim, model.MutationOutcomeDeleted) {
		t.Fatalf("completed claim deletion evidence is missing: %+v", r.receipts)
	}
	if hasReceipt(r.receipts, mutationKindPersistentVolume, model.MutationOutcomeAvailable) {
		t.Fatal("an Available receipt was recorded for a refused reclaim")
	}
}

func TestPVCDeleteReclaimIncompleteClaimListFailsClosed(t *testing.T) {
	h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
	h.k.PrependReactor("list", "persistentvolumeclaims", func(a ktesting.Action) (bool, runtime.Object, error) {
		if !h.claimGone {
			return false, nil, nil
		}
		return true, &corev1.PersistentVolumeClaimList{ListMeta: metav1.ListMeta{Continue: "more"}}, nil
	})
	r := h.run()
	if r.err == nil || errors.Is(r.err, model.ErrRejectedBeforeMutation) || h.realPatches() != 0 {
		t.Fatalf("incomplete list must fail closed without a patch: err=%v patches=%d", r.err, h.realPatches())
	}
}

// The patch itself failing after the claim is gone is a partial outcome: the
// claim is not recreated, the patch is not replayed and the volume stays as the
// controller left it.
func TestPVCDeleteReclaimPatchFailureIsPartialWithoutRollback(t *testing.T) {
	for name, injected := range map[string]error{
		"forbidden":   apierrors.NewForbidden(corev1.Resource("persistentvolumes"), "data-pv", errors.New("no patch")),
		"conflict":    apierrors.NewConflict(corev1.Resource("persistentvolumes"), "data-pv", errors.New("changed")),
		"unavailable": apierrors.NewServiceUnavailable("down"),
		"throttled":   apierrors.NewTooManyRequests("slow", 1),
	} {
		t.Run(name, func(t *testing.T) {
			h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
			h.failPatch = injected
			r := h.run()
			if r.prepareErr != nil {
				t.Fatal(r.prepareErr)
			}
			if r.err == nil || errors.Is(r.err, model.ErrRejectedBeforeMutation) {
				t.Fatalf("a reclaim failure after the delete must stay needs-review: %v", r.err)
			}
			if h.realPatches() != 1 {
				t.Fatalf("the reclaim must not be replayed: %d patches", h.realPatches())
			}
			if h.claimExists() {
				t.Fatal("claim must stay deleted")
			}
			if pv := h.livePV(); pv.Spec.ClaimRef == nil || pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
				t.Fatalf("volume was rolled back or altered: %+v", pv.Spec)
			}
			if !hasReceipt(r.receipts, mutationKindPersistentVolumeClaim, model.MutationOutcomeDeleted) ||
				hasReceipt(r.receipts, mutationKindPersistentVolume, model.MutationOutcomeAvailable) {
				t.Fatalf("receipts must record the deletion only: %+v", r.receipts)
			}
			if got := strings.Join(r.steps, ","); got != "delete-storage,reclaim-volume" {
				t.Fatalf("checkpoints = %s", got)
			}
		})
	}
}

// Another claim binding the volume right after the claimRef is removed is
// reported for review, never as a successful reclaim.
func TestPVCDeleteReclaimRaceWithNewBindingNeedsReview(t *testing.T) {
	h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
	h.afterPatch = func(k *fake.Clientset) {
		mutateTrackedPV(k, func(pv *corev1.PersistentVolume) {
			pv.Spec.ClaimRef = &corev1.ObjectReference{Name: "new", Namespace: "bob", UID: "new-uid"}
			pv.Status.Phase = corev1.VolumeBound
		})
	}
	r := h.run()
	if r.err == nil || errors.Is(r.err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("expected needs-review, got %v", r.err)
	}
	if hasReceipt(r.receipts, mutationKindPersistentVolume, model.MutationOutcomeAvailable) {
		t.Fatalf("success receipt recorded for a volume that was rebound: %+v", r.receipts)
	}
}

func TestPVCDeleteReclaimNotAvailableAfterPatchNeedsReview(t *testing.T) {
	h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
	h.afterPatch = func(k *fake.Clientset) {
		mutateTrackedPV(k, func(pv *corev1.PersistentVolume) { pv.Status.Phase = corev1.VolumeFailed })
	}
	r := h.run()
	if r.err == nil || hasReceipt(r.receipts, mutationKindPersistentVolume, model.MutationOutcomeAvailable) {
		t.Fatalf("a Failed volume must not be reported reclaimed: err=%v receipts=%+v", r.err, r.receipts)
	}
}

// Missing patch permission is caught while planning, before the claim is
// deleted.
func TestPVCDeleteReclaimDryRunRBACFailureBlocksBeforeDelete(t *testing.T) {
	h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
	h.dryFail = apierrors.NewForbidden(corev1.Resource("persistentvolumes"), "data-pv", errors.New("patch denied"))
	r := h.run()
	if r.prepareErr == nil || r.prepareErr.Error() != "Kubernetes API denied the storage request" {
		t.Fatalf("expected the planned reclaim to be denied, got %v", r.prepareErr)
	}
	for _, record := range deleteRecords(h.k, "persistentvolumeclaims") {
		if !record.dry {
			t.Fatal("the claim was deleted although the reclaim patch is not permitted")
		}
	}
	if !h.claimExists() || h.realPatches() != 0 {
		t.Fatal("state changed during a refused plan")
	}
}

// The reclaim is planned from a stale view: the dry run uses the reviewed
// volume UID, so a replaced volume invalidates the plan before any write.
func TestPVCDeleteReclaimStalePlanVolumeReplacedBeforeExecute(t *testing.T) {
	h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
	ctx := context.Background()
	prepared, err := h.s.Prepare(ctx, h.action())
	if err != nil {
		t.Fatal(err)
	}
	mutateTrackedPV(h.k, func(pv *corev1.PersistentVolume) {
		pv.UID = types.UID("swapped-uid")
		pv.ResourceVersion = "4"
	})
	err = h.s.Execute(ctx, prepared, func(string) error { return nil })
	if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("a replaced volume must reject before any mutation: %v", err)
	}
	if !h.claimExists() || h.realPatches() != 0 {
		t.Fatal("state changed on a stale plan")
	}
}
