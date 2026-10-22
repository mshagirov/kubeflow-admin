package kube

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	ktesting "k8s.io/client-go/testing"

	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
)

func securityPVCreateAction() model.Action {
	return model.Action{
		Kind: "pv.create.static", Name: "new-pv",
		Template: "nfs-reg", StorageClass: "nfs-static",
		NFSPath: "/exports/kubeflow/alice/new",
	}
}

func securityPVCDeleteAction(pvc *corev1.PersistentVolumeClaim) model.Action {
	return model.Action{
		Kind: "pvc.delete", Namespace: pvc.Namespace, Name: pvc.Name,
		ConfirmName: pvc.Name, Reason: "security regression",
		TargetUID: string(pvc.UID), TargetResourceVersion: pvc.ResourceVersion,
	}
}

func securityBoundCreateAction(pv *corev1.PersistentVolume) model.Action {
	a := boundNFSAction("bound", pv)
	a.PodDefault = nil
	return a
}

func securityPVCPair() (*corev1.PersistentVolume, *corev1.PersistentVolumeClaim) {
	pv := boundRegistrationPV("data-pv", "nfs-static", "/exports/kubeflow/alice/data", "2400Gi", "alice", "data")
	pvc := boundRegistrationPVC("data", "alice", "data-pv", "pvc-data")
	return pv, pvc
}

func securityDiscovery(f *registrationFixture) *fakediscovery.FakeDiscovery {
	return f.client.Discovery().(*fakediscovery.FakeDiscovery)
}

func securityPodDefaultResource(kind string) metav1.APIResource {
	return metav1.APIResource{
		Name: "poddefaults", Namespaced: true, Kind: kind,
		Verbs: []string{"get", "list", "create", "patch", "delete"},
	}
}

func TestSecurityRegressionInventoryPreservesDependencyWarnings(t *testing.T) {
	f := newRegistrationFixture(t, registrationConfig(), nil, nil)
	securityDiscovery(f).PrependReactor("get", "resource", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "kubeflow.org", Resource: "poddefaults"}, "", errors.New("discovery denied"))
	})
	inventory, err := f.svc.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	warnings := strings.Join(inventory.Warnings, " ")
	if !strings.Contains(warnings, "PodDefault inventory is unavailable") || !strings.Contains(warnings, "Filesystem usage") {
		t.Fatal("inventory lost either the dependency warning or the base safety warning")
	}
}

func TestSecurityRegressionPodDefaultDiscoveryFailsClosed(t *testing.T) {
	ctx := context.Background()
	base := func(t *testing.T) (*registrationFixture, model.Action) {
		pv, pvc := securityPVCPair()
		f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv, pvc}, nil)
		return f, securityPVCDeleteAction(pvc)
	}

	t.Run("only-other-version", func(t *testing.T) {
		f, a := base(t)
		if _, err := f.svc.Prepare(ctx, a); err != nil {
			t.Fatalf("baseline delete must prepare: %v", err)
		}
		securityDiscovery(f).Resources = []*metav1.APIResourceList{{
			GroupVersion: "kubeflow.org/v1beta1",
			APIResources: []metav1.APIResource{securityPodDefaultResource("PodDefault")},
		}}
		if _, err := f.svc.Prepare(ctx, a); err == nil {
			t.Fatal("PVC delete accepted while PodDefault was served only under another API version")
		}
	})

	t.Run("nil-discovery", func(t *testing.T) {
		f, a := base(t)
		if _, err := f.svc.Prepare(ctx, a); err != nil {
			t.Fatalf("baseline delete must prepare: %v", err)
		}
		securityDiscovery(f).Resources = nil
		if _, err := f.svc.Prepare(ctx, a); err == nil {
			t.Fatal("PVC delete accepted with nil discovery data; absence must be explicit")
		}
	})

	t.Run("nil-resource-list", func(t *testing.T) {
		f, a := base(t)
		if _, err := f.svc.Prepare(ctx, a); err != nil {
			t.Fatalf("baseline delete must prepare: %v", err)
		}
		d := securityDiscovery(f)
		d.Resources = nil
		d.PrependReactor("get", "resource", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, nil
		})
		if _, err := f.svc.Prepare(ctx, a); err == nil {
			t.Fatal("PVC delete accepted a nil discovery resource list; nil must fail closed")
		}
	})

	t.Run("empty-resource-list", func(t *testing.T) {
		f, a := base(t)
		securityDiscovery(f).Resources = []*metav1.APIResourceList{
			{GroupVersion: "kubeflow.org/v1alpha1", APIResources: []metav1.APIResource{}},
			{GroupVersion: "kubeflow.org/v1", APIResources: []metav1.APIResource{
				{Name: "profiles", Namespaced: false, Kind: "Profile", Verbs: []string{"get", "list"}},
			}},
		}
		if _, err := f.svc.Prepare(ctx, a); err == nil {
			t.Fatal("PVC delete accepted an empty discovery resource list as verified absence")
		}
	})

	for _, kind := range []string{"NotPodDefault", ""} {
		kind := kind
		t.Run("wrong-kind-"+kind, func(t *testing.T) {
			f, a := base(t)
			if _, err := f.svc.Prepare(ctx, a); err != nil {
				t.Fatalf("baseline delete must prepare: %v", err)
			}
			securityDiscovery(f).Resources = []*metav1.APIResourceList{{
				GroupVersion: "kubeflow.org/v1alpha1",
				APIResources: []metav1.APIResource{securityPodDefaultResource(kind)},
			}}
			if _, err := f.svc.Prepare(ctx, a); err == nil {
				t.Fatal("PVC delete accepted an incompatible PodDefault discovery entry")
			}
		})
	}

	t.Run("discovery-error", func(t *testing.T) {
		f, a := base(t)
		if _, err := f.svc.Prepare(ctx, a); err != nil {
			t.Fatalf("baseline delete must prepare: %v", err)
		}
		securityDiscovery(f).PrependReactor("get", "resource", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: "kubeflow.org", Resource: "poddefaults"}, "",
				errors.New("discovery denied"),
			)
		})
		if _, err := f.svc.Prepare(ctx, a); err == nil {
			t.Fatal("PVC delete accepted a failed PodDefault discovery")
		}
	})

	t.Run("genuine-absence", func(t *testing.T) {
		f, a := base(t)
		securityDiscovery(f).Resources = []*metav1.APIResourceList{
			{
				GroupVersion: "kubeflow.org/v1alpha1",
				APIResources: []metav1.APIResource{{
					Name: "profiles", Namespaced: false, Kind: "Profile", Verbs: []string{"get", "list"},
				}},
			},
			{
				GroupVersion: "kubeflow.org/v1",
				APIResources: []metav1.APIResource{{
					Name: "profiles", Namespaced: false, Kind: "Profile", Verbs: []string{"get", "list"},
				}},
			},
		}
		p, err := f.svc.Prepare(ctx, a)
		if err != nil {
			t.Fatalf("explicit discovery without PodDefault must permit deletion: %v", err)
		}
		var evidence struct {
			PodDefaultAPI string `json:"podDefaultAPI"`
		}
		if err := json.Unmarshal(p.Data, &evidence); err != nil {
			t.Fatal(err)
		}
		if evidence.PodDefaultAPI != "absent" {
			t.Fatalf("explicit absence was not recorded: %q", evidence.PodDefaultAPI)
		}
	})
}

func TestSecurityRegressionFinalPVCDeleteAfterCleanupIsNotPreMutation(t *testing.T) {
	ctx := context.Background()
	pv, pvc := securityPVCPair()
	pd := podDefaultFixture("alice", "mount-a", []string{"data"}, false, false)
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv, pvc}, []runtime.Object{pd})
	a := securityPVCDeleteAction(pvc)
	a.ConfirmPodDefaultCleanup = true
	p, err := f.svc.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}

	f.client.PrependReactor("delete", "persistentvolumeclaims", func(action ktesting.Action) (bool, runtime.Object, error) {
		da, ok := action.(ktesting.DeleteActionImpl)
		if !ok || len(da.GetDeleteOptions().DryRun) > 0 {
			return false, nil, nil
		}
		if da.GetName() == "data" {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Resource: "persistentvolumeclaims"}, "data",
				errors.New("denied after PodDefault cleanup"),
			)
		}
		return false, nil, nil
	})

	err = f.svc.Execute(ctx, p, func(string) error { return nil })
	if err == nil {
		t.Fatal("final PVC rejection after PodDefault cleanup unexpectedly succeeded")
	}
	if errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("final PVC rejection after earlier PodDefault deletion was marked pre-mutation: %v", err)
	}
	if _, getErr := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(ctx, "mount-a", metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Fatalf("reviewed PodDefault was not deleted before the final PVC rejection: %v", getErr)
	}
	if _, getErr := f.client.CoreV1().PersistentVolumeClaims("alice").Get(ctx, "data", metav1.GetOptions{}); getErr != nil {
		t.Fatalf("PVC must remain for review after the final rejection: %v", getErr)
	}
}

func securitySetProfileOwner(t *testing.T, f *registrationFixture) {
	t.Helper()
	profile, err := f.dynamic.Resource(Profiles).Get(context.Background(), "alice", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(profile.Object, "mallory@example.test", "spec", "owner", "name"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dynamic.Resource(Profiles).Update(context.Background(), profile, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func securitySetNamespaceOwner(t *testing.T, f *registrationFixture) {
	t.Helper()
	ns, err := f.client.CoreV1().Namespaces().Get(context.Background(), "alice", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ns.Annotations == nil {
		ns.Annotations = map[string]string{}
	}
	ns.Annotations["owner"] = "mallory@example.test"
	if _, err := f.client.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityRegressionScopeChangeBeforePodDefaultWrite(t *testing.T) {
	mutations := map[string]func(*testing.T, *registrationFixture){
		"profile-owner":   securitySetProfileOwner,
		"namespace-owner": securitySetNamespaceOwner,
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/workspace", "2400Gi")
			f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv}, nil)
			p, err := f.svc.Prepare(context.Background(), boundNFSAction("bound", pv))
			if err != nil {
				t.Fatal(err)
			}
			err = f.svc.Execute(context.Background(), p, func(step string) error {
				if step == "create-poddefault" {
					mutate(t, f)
				}
				return nil
			})
			if err == nil {
				t.Fatal("scope change at post-binding PodDefault checkpoint was ignored")
			}
			if _, getErr := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), "mount-toast", metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
				t.Fatalf("PodDefault was written after the namespace/Profile scope changed: %v", getErr)
			}
		})
	}
}

func TestSecurityRegressionScopeChangeBeforeFinalPVCDelete(t *testing.T) {
	mutations := map[string]func(*testing.T, *registrationFixture){
		"profile-owner":   securitySetProfileOwner,
		"namespace-owner": securitySetNamespaceOwner,
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			pv, pvc := securityPVCPair()
			pd := podDefaultFixture("alice", "mount-a", []string{"data"}, false, false)
			f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv, pvc}, []runtime.Object{pd})
			a := securityPVCDeleteAction(pvc)
			a.ConfirmPodDefaultCleanup = true
			p, err := f.svc.Prepare(context.Background(), a)
			if err != nil {
				t.Fatal(err)
			}
			err = f.svc.Execute(context.Background(), p, func(step string) error {
				if step == "delete-pvc" {
					mutate(t, f)
				}
				return nil
			})
			if err == nil {
				t.Fatal("scope change at final PVC delete checkpoint was ignored")
			}
			if _, getErr := f.client.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "data", metav1.GetOptions{}); getErr != nil {
				t.Fatalf("PVC was deleted after the namespace/Profile scope changed: %v", getErr)
			}
			if _, getErr := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), "mount-a", metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
				t.Fatalf("reviewed PodDefault should already be deleted before the final PVC check: %v", getErr)
			}
		})
	}
}

func securityInstallIncompleteList(t *testing.T, f *registrationFixture, resourceName string) {
	t.Helper()
	continueList := func() metav1.ListMeta { return metav1.ListMeta{Continue: "more"} }
	switch resourceName {
	case "persistentvolumes":
		f.client.PrependReactor("list", resourceName, func(ktesting.Action) (bool, runtime.Object, error) {
			return true, &corev1.PersistentVolumeList{ListMeta: continueList()}, nil
		})
	case "persistentvolumeclaims":
		f.client.PrependReactor("list", resourceName, func(ktesting.Action) (bool, runtime.Object, error) {
			return true, &corev1.PersistentVolumeClaimList{ListMeta: continueList()}, nil
		})
	case "pods":
		f.client.PrependReactor("list", resourceName, func(ktesting.Action) (bool, runtime.Object, error) {
			return true, &corev1.PodList{ListMeta: continueList()}, nil
		})
	case "resourcequotas":
		f.client.PrependReactor("list", resourceName, func(ktesting.Action) (bool, runtime.Object, error) {
			return true, &corev1.ResourceQuotaList{ListMeta: continueList()}, nil
		})
	case "notebooks":
		f.raw.PrependReactor("list", resourceName, func(ktesting.Action) (bool, runtime.Object, error) {
			return true, &unstructured.UnstructuredList{Object: map[string]any{
				"apiVersion": "kubeflow.org/v1",
				"kind":       "NotebookList",
				"metadata":   map[string]any{"continue": "more"},
			}}, nil
		})
	default:
		t.Fatalf("unsupported incomplete-list fixture %q", resourceName)
	}
}

func TestSecurityRegressionIncompleteListsRefusePrepare(t *testing.T) {
	ctx := context.Background()
	newPVCDelete := func(t *testing.T) (*registrationFixture, model.Action) {
		pv, pvc := securityPVCPair()
		return newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv, pvc}, nil), securityPVCDeleteAction(pvc)
	}
	newPVCreate := func(t *testing.T) (*registrationFixture, model.Action) {
		return newRegistrationFixture(t, registrationConfig(), nil, nil), securityPVCreateAction()
	}
	newBoundCreate := func(t *testing.T) (*registrationFixture, model.Action) {
		pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/selected", "2400Gi")
		return newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv}, nil), securityBoundCreateAction(pv)
	}

	cases := []struct {
		name           string
		incompleteList string
		build          func(*testing.T) (*registrationFixture, model.Action)
	}{
		{"pv-create-pv-list", "persistentvolumes", newPVCreate},
		{"bound-create-pvc-list", "persistentvolumeclaims", newBoundCreate},
		{"pvc-delete-pv-list", "persistentvolumes", newPVCDelete},
		{"pvc-delete-pod-list", "pods", newPVCDelete},
		{"pvc-delete-notebook-list", "notebooks", newPVCDelete},
		{"bound-create-quota-list", "resourcequotas", newBoundCreate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseline, baselineAction := tc.build(t)
			if _, err := baseline.svc.Prepare(ctx, baselineAction); err != nil {
				t.Fatalf("baseline action must prepare without the incomplete list: %v", err)
			}
			f, a := tc.build(t)
			securityInstallIncompleteList(t, f, tc.incompleteList)
			if _, err := f.svc.Prepare(ctx, a); err == nil {
				t.Fatalf("prepare accepted an incomplete %s list as complete", tc.incompleteList)
			}
		})
	}
}

func securityBeeGFSPV(handle string) *corev1.PersistentVolume {
	fs := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "existing", UID: "pv-existing", ResourceVersion: "1"},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("500Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			VolumeMode:                    &fs,
			StorageClassName:              "beegfs-static",
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: config.BeeGFSDriverName, VolumeHandle: handle,
			}},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable},
	}
}

func securityNFSPV(path, server string) *corev1.PersistentVolume {
	pv := registrationPV("existing", "nfs-static", path, "2400Gi")
	pv.Spec.NFS.Server = server
	return pv
}

func securityBeeGFSCreateAction(suffix string) model.Action {
	return model.Action{
		Kind: "pv.create.static", Name: "new-bee", Template: "beegfs-reg",
		StorageClass: "beegfs-static", BeeGFSPath: suffix,
	}
}

func TestSecurityRegressionNoncanonicalExistingBackendsFailClosed(t *testing.T) {
	const beeHost = "beegfs://beegfs.example.invalid/home/example.user"
	cases := []struct {
		name     string
		existing *corev1.PersistentVolume
		action   model.Action
	}{
		{"nfs-server-case", securityNFSPV("/exports/kubeflow/alice/workspace", "NFS.EXAMPLE.INVALID"), model.Action{Kind: "pv.create.static", Name: "new-nfs", Template: "nfs-reg", StorageClass: "nfs-static", NFSPath: "/exports/kubeflow/alice/workspace"}},
		{"nfs-server-trailing-dot", securityNFSPV("/exports/kubeflow/alice/workspace", "nfs.example.invalid."), model.Action{Kind: "pv.create.static", Name: "new-nfs", Template: "nfs-reg", StorageClass: "nfs-static", NFSPath: "/exports/kubeflow/alice/workspace"}},
		{"nfs-relative-path", securityNFSPV("exports/kubeflow/alice/workspace", "nfs.example.invalid"), model.Action{Kind: "pv.create.static", Name: "new-nfs", Template: "nfs-reg", StorageClass: "nfs-static", NFSPath: "/exports/kubeflow/alice/workspace"}},
		{"nfs-empty-path", securityNFSPV("", "nfs.example.invalid"), model.Action{Kind: "pv.create.static", Name: "new-nfs", Template: "nfs-reg", StorageClass: "nfs-static", NFSPath: "/exports/kubeflow/alice/workspace"}},
		{"nfs-double-slash", securityNFSPV("/exports/kubeflow/alice//workspace", "nfs.example.invalid"), model.Action{Kind: "pv.create.static", Name: "new-nfs", Template: "nfs-reg", StorageClass: "nfs-static", NFSPath: "/exports/kubeflow/alice/workspace"}},
		{"beegfs-query", securityBeeGFSPV(beeHost + "?x=1"), securityBeeGFSCreateAction("example.user/child")},
		{"beegfs-fragment", securityBeeGFSPV(beeHost + "#frag"), securityBeeGFSCreateAction("example.user/child")},
		{"beegfs-userinfo", securityBeeGFSPV("beegfs://user@" + beeHost[len("beegfs://"):]), securityBeeGFSCreateAction("example.user/child")},
		{"beegfs-percent", securityBeeGFSPV(beeHost + "%2Fchild"), securityBeeGFSCreateAction("example.user/child")},
		{"beegfs-dotdot", securityBeeGFSPV(beeHost + "/../example.user"), securityBeeGFSCreateAction("example.user/child")},
		{"beegfs-double-slash", securityBeeGFSPV("beegfs://beegfs.example.invalid/home//example.user"), securityBeeGFSCreateAction("example.user/child")},
		{"beegfs-trailing-slash", securityBeeGFSPV(beeHost + "/"), securityBeeGFSCreateAction("example.user/child")},
		{"beegfs-host-case", securityBeeGFSPV("beegfs://BEEGFS.EXAMPLE.INVALID/home/example.user"), securityBeeGFSCreateAction("example.user/child")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{tc.existing}, nil)
			if _, err := f.svc.Prepare(context.Background(), tc.action); err == nil {
				t.Fatal("new PV accepted a duplicate/noncanonical existing backend identity")
			}
		})
	}
}

func TestSecurityRegressionOverwriteUpdatesProvenanceAndPreservesForeignMetadata(t *testing.T) {
	pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/workspace", "2400Gi")
	existing := podDefaultFixture("alice", "mount-toast", []string{"other-claim"}, false, false)
	existing.SetAnnotations(map[string]string{
		podDefaultWorkflowKey: "pvc.create.dynamic",
		podDefaultPVCUIDKey:   "pvc-old",
		"example.test/keep":   "yes",
	})
	existing.SetLabels(map[string]string{
		"example.test/keep":            "yes",
		"app.kubernetes.io/managed-by": "helm",
	})
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv}, []runtime.Object{existing})
	a := boundNFSAction("bound", pv)
	a.PodDefault.Overwrite = true
	p, err := f.svc.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Execute(context.Background(), p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	pd, err := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), "mount-toast", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := pd.GetAnnotations()[podDefaultWorkflowKey]; got != "pvc.create.bound" {
		t.Fatalf("overwrite did not update provenance workflow: %q", got)
	}
	if got := pd.GetAnnotations()[podDefaultPVCUIDKey]; got != "pvc-bound" {
		t.Fatalf("overwrite did not bind provenance to the new PVC UID: %q", got)
	}
	if got := pd.GetAnnotations()["example.test/keep"]; got != "yes" {
		t.Fatalf("overwrite removed a foreign annotation: %q", got)
	}
	if got := pd.GetLabels()["example.test/keep"]; got != "yes" {
		t.Fatalf("overwrite removed a foreign label: %q", got)
	}
}
