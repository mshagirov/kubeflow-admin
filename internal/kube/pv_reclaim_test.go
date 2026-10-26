package kube

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"kubeflow-admin/internal/model"
)

func oldReleasedPV(t *testing.T) (*reclaimHarness, model.Action) {
	t.Helper()
	h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
	if err := h.k.Tracker().Delete(pvcGVR, "alice", "data"); err != nil {
		t.Fatal(err)
	}
	pv := h.livePV().DeepCopy()
	pv.Status.Phase = corev1.VolumeReleased
	if err := h.k.Tracker().Update(pvGVR, pv, ""); err != nil {
		t.Fatal(err)
	}
	a := model.Action{Kind: "pv.reclaim", Name: pv.Name, TargetUID: string(pv.UID), TargetResourceVersion: pv.ResourceVersion, ConfirmName: pv.Name, Reason: "reviewed retained data and isolated class"}
	return h, a
}

func TestStandalonePVReclaimMakesOldReleasedVolumeAvailable(t *testing.T) {
	h, a := oldReleasedPV(t)
	before := h.livePV().DeepCopy()
	p, err := h.s.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.Warnings, " "), "not a data wipe") || len(p.Changes) < 2 {
		t.Fatal("missing reclaim disclosure")
	}
	var receipts []model.MutationReceipt
	ctx := model.WithMutationReceiptRecorder(context.Background(), func(r model.MutationReceipt) error { receipts = append(receipts, r); return nil })
	var steps []string
	if err := h.s.Execute(ctx, p, func(s string) error { steps = append(steps, s); return nil }); err != nil {
		t.Fatal(err)
	}
	after := h.livePV().DeepCopy()
	if after.Status.Phase != corev1.VolumeAvailable || after.Spec.ClaimRef != nil || h.realPatches() != 1 {
		t.Fatalf("volume was not reclaimed: %+v", after)
	}
	before.Spec.ClaimRef = nil
	if !reflect.DeepEqual(before.Spec, after.Spec) || !reflect.DeepEqual(before.ObjectMeta, after.ObjectMeta) {
		t.Fatal("reclaim changed fields other than claimRef/status")
	}
	if len(receipts) != 1 || receipts[0].Kind != "PersistentVolume" || receipts[0].Outcome != model.MutationOutcomeAvailable || receipts[0].UID != string(before.UID) {
		t.Fatalf("wrong receipt: %+v", receipts)
	}
	if !reflect.DeepEqual(steps, []string{storageReclaimStep}) {
		t.Fatalf("wrong durable checkpoint: %v", steps)
	}
	if err := h.s.Execute(ctx, p, func(string) error { return nil }); err == nil || h.realPatches() != 1 {
		t.Fatal("replayed reclaim issued another patch")
	}
}

func TestStandalonePVReclaimRefusesUnsafeTargets(t *testing.T) {
	for name, mutate := range map[string]func(*reclaimHarness, *model.Action){
		"disabled":           func(h *reclaimHarness, _ *model.Action) { h.s.Config.Features.PVReclaim = false },
		"writes off":         func(h *reclaimHarness, _ *model.Action) { h.s.Config.Features.WritesEnabled = false },
		"no isolation":       func(h *reclaimHarness, _ *model.Action) { h.s.Config.Storage.IsolatedStorageClasses = nil },
		"wrong uid":          func(_ *reclaimHarness, a *model.Action) { a.TargetUID = "replacement" },
		"stale rv":           func(_ *reclaimHarness, a *model.Action) { a.TargetResourceVersion = "old" },
		"wrong confirmation": func(_ *reclaimHarness, a *model.Action) { a.ConfirmName = " data-pv" },
		"no reason":          func(_ *reclaimHarness, a *model.Action) { a.Reason = " " },
		"namespace":          func(_ *reclaimHarness, a *model.Action) { a.Namespace = "alice" },
		"force":              func(_ *reclaimHarness, a *model.Action) { a.Force = true },
		"yaml":               func(_ *reclaimHarness, a *model.Action) { a.StorageYAML = "status: Available" },
		"pvc ack":            func(_ *reclaimHarness, a *model.Action) { a.ConfirmPVReclaim = true },
		"live claim": func(h *reclaimHarness, _ *model.Action) {
			_, err := h.k.CoreV1().PersistentVolumeClaims("alice").Create(context.Background(), pendingPVC("alice", "data", "new-uid"), metav1.CreateOptions{})
			if err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, a := oldReleasedPV(t)
			mutate(h, &a)
			if _, err := h.s.Prepare(context.Background(), a); err == nil {
				t.Fatal("unsafe reclaim accepted")
			}
			if h.realPatches() != 0 {
				t.Fatal("refusal issued patch")
			}
		})
	}
	for name, mutate := range map[string]func(*corev1.PersistentVolume){
		"Bound":     func(p *corev1.PersistentVolume) { p.Status.Phase = corev1.VolumeBound },
		"Available": func(p *corev1.PersistentVolume) { p.Status.Phase = corev1.VolumeAvailable },
		"Pending":   func(p *corev1.PersistentVolume) { p.Status.Phase = corev1.VolumePending },
		"Failed":    func(p *corev1.PersistentVolume) { p.Status.Phase = corev1.VolumeFailed },
		"Delete policy": func(p *corev1.PersistentVolume) {
			p.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
		},
		"no claimRef":  func(p *corev1.PersistentVolume) { p.Spec.ClaimRef = nil },
		"no claim UID": func(p *corev1.PersistentVolume) { p.Spec.ClaimRef.UID = "" },
		"beta class": func(p *corev1.PersistentVolume) {
			p.Annotations = map[string]string{corev1.BetaStorageClassAnnotation: "ordinary"}
		},
		"protected namespace": func(p *corev1.PersistentVolume) { p.Spec.ClaimRef.Namespace = "kube-system" },
	} {
		t.Run(name, func(t *testing.T) {
			h, a := oldReleasedPV(t)
			p := h.livePV().DeepCopy()
			mutate(p)
			if err := h.k.Tracker().Update(pvGVR, p, ""); err != nil {
				t.Fatal(err)
			}
			if _, err := h.s.Prepare(context.Background(), a); err == nil {
				t.Fatal("unsafe PV accepted")
			}
		})
	}
}

func TestStandalonePVReclaimRechecksAfterCheckpoint(t *testing.T) {
	for _, which := range []string{"new claim", "ordinary pending", "beta pending", "PV replaced", "claimRef changed", "class changed", "list error", "incomplete list", "authorization revoked"} {
		t.Run(which, func(t *testing.T) {
			h, a := oldReleasedPV(t)
			p, err := h.s.Prepare(context.Background(), a)
			if err != nil {
				t.Fatal(err)
			}
			err = h.s.Execute(context.Background(), p, func(string) error {
				switch which {
				case "new claim", "ordinary pending", "beta pending":
					claim := pendingPVC("bob", "other", "other-uid")
					if which == "new claim" {
						claim.Name = "data"
						claim.Namespace = "alice"
					}
					if which == "beta pending" {
						ordinary := "ordinary"
						claim.Spec.StorageClassName = &ordinary
						claim.Annotations = map[string]string{corev1.BetaStorageClassAnnotation: "approved"}
					}
					_, e := h.k.CoreV1().PersistentVolumeClaims(claim.Namespace).Create(context.Background(), claim, metav1.CreateOptions{})
					return e
				case "PV replaced", "claimRef changed", "class changed":
					live := h.livePV().DeepCopy()
					if which == "PV replaced" {
						live.UID = "replacement"
					}
					if which == "claimRef changed" {
						live.Spec.ClaimRef.UID = "other"
					}
					if which == "class changed" {
						live.Spec.StorageClassName = "ordinary"
					}
					return h.k.Tracker().Update(pvGVR, live, "")
				case "authorization revoked":
					return errors.New("session revoked")
				case "list error", "incomplete list":
					h.k.PrependReactor("list", "persistentvolumeclaims", func(ktesting.Action) (bool, runtime.Object, error) {
						if which == "list error" {
							return true, nil, errors.New("denied")
						}
						return true, &corev1.PersistentVolumeClaimList{ListMeta: metav1.ListMeta{Continue: "next"}}, nil
					})
				}
				return nil
			})
			if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) || h.realPatches() != 0 {
				t.Fatalf("unsafe checkpoint outcome: %v", err)
			}
		})
	}
}

func TestStandalonePVReclaimDryRunAndUncertainPatch(t *testing.T) {
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumes"}, "data-pv", errors.New("denied"))
	h, a := oldReleasedPV(t)
	h.dryFail = denied
	if _, err := h.s.Prepare(context.Background(), a); err == nil || h.realPatches() != 0 {
		t.Fatal("dry-run denial did not block reclaim")
	}
	h, a = oldReleasedPV(t)
	p, err := h.s.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	h.failPatch = errors.New("connection lost after request")
	err = h.s.Execute(context.Background(), p, func(string) error { return nil })
	if err == nil || errors.Is(err, model.ErrRejectedBeforeMutation) || h.realPatches() != 1 {
		t.Fatalf("uncertain patch misclassified or retried: %v", err)
	}
}

func TestStandaloneReclaimDocumentIsIdentityOnly(t *testing.T) {
	h, a := oldReleasedPV(t)
	doc, err := h.s.StorageDocument(context.Background(), "pv.reclaim", "", a.Name)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Kind != "PersistentVolume" || doc.UID != a.TargetUID || doc.ResourceVersion != a.TargetResourceVersion || len(doc.Warnings) == 0 {
		t.Fatalf("wrong document: %+v", doc)
	}
	if strings.Contains(doc.YAML, "spec:") || strings.Contains(doc.YAML, "nfs:") || strings.Contains(doc.YAML, "annotations:") {
		t.Fatal("reclaim document exposes editable or backend fields")
	}
	if _, err := h.s.StorageDocument(context.Background(), "pv.reclaim", "alice", a.Name); err == nil {
		t.Fatal("PV reclaim document accepted namespace")
	}
}

func TestStandaloneReclaimDoesNotReportSuccessAfterRebindOrReceiptFailure(t *testing.T) {
	for _, which := range []string{"rebound", "receipt failure"} {
		t.Run(which, func(t *testing.T) {
			h, a := oldReleasedPV(t)
			p, err := h.s.Prepare(context.Background(), a)
			if err != nil {
				t.Fatal(err)
			}
			if which == "rebound" {
				h.afterPatch = func(k *fake.Clientset) {
					obj, e := k.Tracker().Get(pvGVR, "", a.Name)
					if e != nil {
						t.Fatal(e)
					}
					pv := obj.(*corev1.PersistentVolume).DeepCopy()
					pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: "other", Name: "claim", UID: "replacement-claim"}
					pv.Status.Phase = corev1.VolumeBound
					if e := k.Tracker().Update(pvGVR, pv, ""); e != nil {
						t.Fatal(e)
					}
				}
			}
			var receipts []model.MutationReceipt
			ctx := model.WithMutationReceiptRecorder(context.Background(), func(r model.MutationReceipt) error {
				if which == "receipt failure" {
					return errors.New("receipt persistence failed")
				}
				receipts = append(receipts, r)
				return nil
			})
			err = h.s.Execute(ctx, p, func(string) error { return nil })
			if err == nil || errors.Is(err, model.ErrRejectedBeforeMutation) || len(receipts) != 0 || h.realPatches() != 1 {
				t.Fatalf("post-write failure falsely succeeded or replayed: err=%v receipts=%v patches=%d", err, receipts, h.realPatches())
			}
		})
	}
}

func TestStandaloneReclaimRejectsMetadataRVDriftAtFinalRead(t *testing.T) {
	h, a := oldReleasedPV(t)
	p, err := h.s.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	err = h.s.Execute(context.Background(), p, func(string) error {
		gets := 0
		h.k.PrependReactor("get", "persistentvolumes", func(action ktesting.Action) (bool, runtime.Object, error) {
			gets++
			// First read is the post-checkpoint full build; the second is the
			// final inspection. Only metadata changes: the spec digest is equal.
			if gets == 2 {
				pv := h.livePV().DeepCopy()
				pv.ResourceVersion = "4"
				pv.Finalizers = append(pv.Finalizers, "example.org/review-required")
				if e := h.k.Tracker().Update(pvGVR, pv, ""); e != nil {
					return true, nil, e
				}
			}
			return false, nil, nil
		})
		return nil
	})
	if err == nil || !errors.Is(err, ErrConflict) || !errors.Is(err, model.ErrRejectedBeforeMutation) || h.realPatches() != 0 {
		t.Fatalf("metadata/RV drift was silently accepted: %v", err)
	}
}

func TestStandaloneReclaimWarningsDoNotClaimToDeleteTheClaim(t *testing.T) {
	h, a := oldReleasedPV(t)
	p, err := h.s.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	warnings := strings.Join(p.Warnings, " ")
	for _, bad := range []string{"claim stays deleted", "After the claim is confirmed deleted", "Post-delete"} {
		if strings.Contains(warnings, bad) {
			t.Fatalf("standalone warnings imply PVC deletion: %s", warnings)
		}
	}
	if !strings.Contains(warnings, "previous claim is already absent") || !strings.Contains(warnings, "does not delete or create any PVC or PV") {
		t.Fatal("standalone operation scope is not disclosed")
	}
}

func TestStandaloneReclaimDefinitePatchRejectionsRemainConservative(t *testing.T) {
	resource := schema.GroupResource{Resource: "persistentvolumes"}
	for _, rejection := range []error{
		apierrors.NewForbidden(resource, "data-pv", errors.New("denied")),
		apierrors.NewConflict(resource, "data-pv", errors.New("drift")),
		apierrors.NewBadRequest("rejected"),
		apierrors.NewInvalid(schema.GroupKind{Kind: "PersistentVolume"}, "data-pv", nil),
	} {
		h, a := oldReleasedPV(t)
		p, err := h.s.Prepare(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		h.failPatch = rejection
		err = h.s.Execute(context.Background(), p, func(string) error { return nil })
		if err == nil || errors.Is(err, model.ErrRejectedBeforeMutation) || h.realPatches() != 1 {
			t.Fatalf("write refusal unexpectedly released evidence or retried: %v", err)
		}
	}
}
