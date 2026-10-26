package kube

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"kubeflow-admin/internal/model"
)

func TestRetainPVCDeletionLeavesReleasedUnlessExplicitlyReclaimed(t *testing.T) {
	h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
	a := h.action()
	a.ConfirmPVReclaim = false
	h.s.Config.Features.PVReclaim = false
	p, err := h.s.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.Warnings, " "), "stays Released") {
		t.Fatal("default retention is not disclosed")
	}
	if err := h.s.Execute(context.Background(), p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if h.claimExists() || h.realPatches() != 0 || h.livePV().Spec.ClaimRef == nil {
		t.Fatal("default deletion exposed retained storage")
	}
}

func TestReclaimRequiresSeparateFeatureAndClassIsolation(t *testing.T) {
	for _, which := range []string{"feature", "isolation"} {
		t.Run(which, func(t *testing.T) {
			h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
			if which == "feature" {
				h.s.Config.Features.PVReclaim = false
			} else {
				h.s.Config.Storage.IsolatedStorageClasses = nil
			}
			if _, err := h.s.Prepare(context.Background(), h.action()); err == nil {
				t.Fatal("unsafe reclaim accepted")
			}
			if !h.claimExists() || h.realPatches() != 0 {
				t.Fatal("refusal mutated storage")
			}
		})
	}
}

func TestReclaimRejectsOrdinaryPendingClaimInAnotherNamespace(t *testing.T) {
	h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
	p, err := h.s.Prepare(context.Background(), h.action())
	if err != nil {
		t.Fatal(err)
	}
	err = h.s.Execute(context.Background(), p, func(step string) error {
		if step == storageReclaimStep {
			claim := pendingPVC("another-tenant", "ordinary-pending", "other-uid")
			if _, err := h.k.CoreV1().PersistentVolumeClaims(claim.Namespace).Create(context.Background(), claim, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "unbound PersistentVolumeClaim") || h.realPatches() != 0 {
		t.Fatalf("ordinary pending claim was not fenced: %v", err)
	}
}

func TestUnreservedPVRequiresIsolatedClass(t *testing.T) {
	f := newRegistrationFixture(t, registrationConfig(), nil, nil)
	f.svc.Config.Storage.IsolatedStorageClasses = nil
	a := model.Action{Kind: "pv.create.static", Name: "isolated-test", Template: "nfs-reg", StorageClass: "nfs-static", NFSPath: "/exports/kubeflow/alice/new-user"}
	if _, err := f.svc.Prepare(context.Background(), a); err == nil || !strings.Contains(err.Error(), "isolated StorageClass") {
		t.Fatalf("unisolated registration accepted: %v", err)
	}
}

func TestUnreservedPVRefusesExistingOrdinaryPendingClaim(t *testing.T) {
	f := newRegistrationFixture(t, registrationConfig(), nil, nil)
	claim := pendingPVC("other-tenant", "ordinary", "tenant-uid")
	class := "nfs-static"
	claim.Spec.StorageClassName = &class
	if _, err := f.client.CoreV1().PersistentVolumeClaims(claim.Namespace).Create(context.Background(), claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	a := model.Action{Kind: "pv.create.static", Name: "isolated-test", Template: "nfs-reg", StorageClass: class, NFSPath: "/exports/kubeflow/alice/new-user"}
	if _, err := f.svc.Prepare(context.Background(), a); err == nil || !strings.Contains(err.Error(), "unbound PVC") {
		t.Fatalf("pending tenant claim ignored: %v", err)
	}
	if _, err := f.client.CoreV1().PersistentVolumes().Get(context.Background(), a.Name, metav1.GetOptions{}); err == nil {
		t.Fatal("registration wrote a PV before isolation checks")
	}
}

func TestEffectiveClaimClassHonorsLegacyAnnotationPrecedence(t *testing.T) {
	for _, spec := range []string{"", "ordinary", "approved"} {
		claim := pendingPVC("tenant", "claim", "uid")
		claim.Spec.StorageClassName = &spec
		claim.Annotations = map[string]string{corev1.BetaStorageClassAnnotation: "isolated"}
		if got := effectiveClaimClass(claim); got != "isolated" {
			t.Fatalf("annotation ignored for spec %q: %s", spec, got)
		}
	}
}

func TestBetaAnnotationCannotBypassReclaimClaimCheck(t *testing.T) {
	for _, spec := range []*string{nil, new(string), func() *string { s := "ordinary"; return &s }()} {
		h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
		p, err := h.s.Prepare(context.Background(), h.action())
		if err != nil {
			t.Fatal(err)
		}
		err = h.s.Execute(context.Background(), p, func(step string) error {
			if step == storageReclaimStep {
				claim := pendingPVC("other-tenant", "annotation-bypass", "tenant-uid")
				claim.Spec.StorageClassName = spec
				claim.Annotations = map[string]string{corev1.BetaStorageClassAnnotation: "approved"}
				if _, err := h.k.CoreV1().PersistentVolumeClaims(claim.Namespace).Create(context.Background(), claim, metav1.CreateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "unbound PersistentVolumeClaim") || h.realPatches() != 0 {
			t.Fatalf("annotation bypass succeeded: %v", err)
		}
	}
}

func TestIsolatedClassesCannotBeUsedForDynamicPVCs(t *testing.T) {
	h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
	a := model.Action{Kind: "pvc.create.dynamic", Namespace: "alice", Name: "fresh", StorageClass: "approved", Size: "1Gi", AccessMode: "ReadWriteMany"}
	if _, err := h.s.Prepare(context.Background(), a); err == nil || !strings.Contains(err.Error(), "dynamic PVC creation is forbidden") {
		t.Fatalf("isolated dynamic creation accepted: %v", err)
	}
}

func TestReclaimPlanCannotSurviveDisablingFeature(t *testing.T) {
	h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
	p, err := h.s.Prepare(context.Background(), h.action())
	if err != nil {
		t.Fatal(err)
	}
	h.s.Config.Features.PVReclaim = false
	if err := h.s.Execute(context.Background(), p, func(string) error { return nil }); err == nil {
		t.Fatal("stale reclaim plan bypassed feature gate")
	}
	if !h.claimExists() || h.realPatches() != 0 {
		t.Fatal("stale plan mutated storage")
	}
}

func TestReclaimRefusesPVSideLegacyClassAnnotation(t *testing.T) {
	h := newReclaimHarness(t, corev1.PersistentVolumeReclaimRetain)
	pv := h.livePV().DeepCopy()
	pv.Annotations = map[string]string{corev1.BetaStorageClassAnnotation: "ordinary"}
	if err := h.k.Tracker().Update(pvGVR, pv, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.s.Prepare(context.Background(), h.action()); err == nil || !strings.Contains(err.Error(), "deprecated beta") {
		t.Fatalf("legacy PV alias accepted: %v", err)
	}
	if !h.claimExists() || h.realPatches() != 0 {
		t.Fatal("unsafe PV alias mutated storage")
	}
}
