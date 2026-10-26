package kube

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/operations"
	"kubeflow-admin/internal/store"
)

// dryRunDynamic makes client-go's dynamic fake honor server-side dry-run.
// The upstream fake drops CreateOptions/PatchOptions/DeleteOptions entirely,
// so the test harness wraps only the PodDefault resource and answers dry-run
// requests without touching the tracker.
type dryRunDynamic struct {
	dynamic.Interface
	raw *dynamicfake.FakeDynamicClient
}

func (d *dryRunDynamic) Resource(gvr schema.GroupVersionResource) dynamic.NamespaceableResourceInterface {
	original := d.Interface.Resource(gvr)
	if gvr != PodDefaults {
		return original
	}
	return &dryRunNamespaceable{NamespaceableResourceInterface: original}
}

type dryRunNamespaceable struct {
	dynamic.NamespaceableResourceInterface
}

func (d *dryRunNamespaceable) Namespace(ns string) dynamic.ResourceInterface {
	return &dryRunResource{ResourceInterface: d.NamespaceableResourceInterface.Namespace(ns), namespace: ns}
}

type dryRunResource struct {
	dynamic.ResourceInterface
	namespace string
}

func (d *dryRunResource) Create(ctx context.Context, obj *unstructured.Unstructured, opts metav1.CreateOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if len(opts.DryRun) > 0 {
		return obj.DeepCopy(), nil
	}
	return d.ResourceInterface.Create(ctx, obj, opts, subresources...)
}

func (d *dryRunResource) Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*unstructured.Unstructured, error) {
	if len(opts.DryRun) > 0 {
		return d.ResourceInterface.Get(ctx, name, metav1.GetOptions{})
	}
	return d.ResourceInterface.Patch(ctx, name, pt, data, opts, subresources...)
}

func (d *dryRunResource) Delete(ctx context.Context, name string, opts metav1.DeleteOptions, subresources ...string) error {
	if len(opts.DryRun) > 0 {
		return nil
	}
	return d.ResourceInterface.Delete(ctx, name, opts, subresources...)
}

type registrationFixture struct {
	svc             *Service
	client          *fake.Clientset
	dynamic         dynamic.Interface
	raw             *dynamicfake.FakeDynamicClient
	profile         *unstructured.Unstructured
	cfg             config.Config
	failPVC         error
	failPD          error
	failPDDelete    string
	failPDDeleteErr error
	checks          []string
}

func registrationConfig() config.Config {
	return config.Config{
		Features: config.Features{WritesEnabled: true, StaticStorage: true, PodDefaults: true, StorageDeletion: true},
		Policy:   config.Policy{},
		Storage: config.Storage{
			IsolatedStorageClasses: []string{"nfs-static", "beegfs-static"},
			AllowedStorageClasses:  []string{"nfs-static", "beegfs-static", "dynamic-class"},
			RegistrationTemplates: []config.RegistrationTemplate{
				{
					Name: "nfs-reg", Kind: config.RegistrationKindNFS, StorageClass: "nfs-static",
					Capacity: "2400Gi", AccessModes: []string{"ReadWriteMany"},
					PVCRequest: "10Gi", PVCAccessModes: []string{"ReadWriteMany"},
					ReclaimPolicy: "Retain", VolumeMode: "Filesystem",
					NFSServer: "nfs.example.invalid", NFSRoots: []string{"/exports/kubeflow/alice"},
					MountOptions: []string{"nfsvers=3"}, RequireDirectoryUIDGID: true,
					PodDefaultName: "mount-toast", PodDefaultMountPathPrefix: "/home/jovyan",
				},
				{
					Name: "beegfs-reg", Kind: config.RegistrationKindBeeGFS, StorageClass: "beegfs-static",
					Capacity: "500Gi", AccessModes: []string{"ReadWriteMany"},
					PVCRequest: "10Gi", PVCAccessModes: []string{"ReadWriteMany"},
					ReclaimPolicy: "Retain", VolumeMode: "Filesystem",
					BeeGFSDriver: config.BeeGFSDriverName, BeeGFSPrefix: "beegfs://beegfs.example.invalid/home",
					PodDefaultName: "mount-hpc", PodDefaultMountPathPrefix: "/home/jovyan",
				},
			},
		},
		Notebooks: config.Notebooks{ProtectedMountPaths: []string{"/home/jovyan"}},
	}
}

func newRegistrationFixture(t *testing.T, cfg config.Config, typedObjects []runtime.Object, dynamicObjects []runtime.Object) *registrationFixture {
	t.Helper()
	profile := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubeflow.org/v1", "kind": "Profile",
		"metadata": map[string]any{"name": "alice", "uid": "profile-1", "resourceVersion": "1"},
		"spec":     map[string]any{"owner": map[string]any{"kind": "User", "name": "alice@example.test"}},
	}}
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", UID: "namespace-1", Annotations: map[string]string{"owner": "alice@example.test"}, OwnerReferences: owner("profile-1", "Profile", "alice")},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}
	nfsClass := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "nfs-static", UID: "class-nfs"}, Provisioner: "kubernetes.io/no-provisioner"}
	beegfsClass := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "beegfs-static", UID: "class-beegfs"}, Provisioner: config.BeeGFSDriverName}
	wffc := storagev1.VolumeBindingWaitForFirstConsumer
	dynamicClass := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "dynamic-class", UID: "class-dyn"}, Provisioner: "csi.example.vendor", VolumeBindingMode: &wffc}
	typedObjects = append([]runtime.Object{ns, nfsClass, beegfsClass, dynamicClass}, typedObjects...)
	client := fake.NewSimpleClientset(typedObjects...)
	dynObjs := append([]runtime.Object{profile}, dynamicObjects...)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		Profiles:    "ProfileList",
		Notebooks:   "NotebookList",
		PodDefaults: "PodDefaultList",
	}, dynObjs...)
	wrapped := &dryRunDynamic{Interface: dyn, raw: dyn}
	f := &registrationFixture{client: client, dynamic: wrapped, raw: dyn, profile: profile, cfg: cfg}
	installRegistrationReactors(t, f)
	f.svc = &Service{Config: cfg, Typed: client, Dynamic: wrapped, PollInterval: time.Millisecond}
	return f
}

func installRegistrationReactors(t *testing.T, f *registrationFixture) {
	t.Helper()
	// Typed dry-run guard. Custom create reactors below handle their own dry
	// run before the tracker can persist it.
	f.client.PrependReactor("*", "persistentvolumes", dryRunReactor(f.client))
	f.client.PrependReactor("*", "persistentvolumeclaims", dryRunReactor(f.client))
	// A deleted Retain-bound claim releases its volume and the console reclaims it.
	installPVController(f.client)
	f.client.PrependReactor("create", "persistentvolumes", func(a ktesting.Action) (bool, runtime.Object, error) {
		ca, ok := a.(ktesting.CreateActionImpl)
		if !ok {
			return false, nil, nil
		}
		pv, ok := ca.GetObject().(*corev1.PersistentVolume)
		if !ok {
			return false, nil, nil
		}
		if len(ca.GetCreateOptions().DryRun) > 0 {
			return true, pv.DeepCopy(), nil
		}
		stored := pv.DeepCopy()
		stored.UID = types.UID("pv-" + stored.Name)
		stored.ResourceVersion = "1"
		stored.Status.Phase = corev1.VolumeAvailable
		if err := f.client.Tracker().Create(pvGVR, stored, ""); err != nil {
			return true, nil, err
		}
		return true, stored, nil
	})
	f.client.PrependReactor("create", "persistentvolumeclaims", func(a ktesting.Action) (bool, runtime.Object, error) {
		ca, ok := a.(ktesting.CreateActionImpl)
		if !ok {
			return false, nil, nil
		}
		pvc, ok := ca.GetObject().(*corev1.PersistentVolumeClaim)
		if !ok {
			return false, nil, nil
		}
		if len(ca.GetCreateOptions().DryRun) > 0 {
			return true, pvc.DeepCopy(), nil
		}
		if f.failPVC != nil {
			return true, nil, f.failPVC
		}
		stored := pvc.DeepCopy()
		stored.UID = types.UID("pvc-" + stored.Name)
		stored.ResourceVersion = "1"
		if stored.Spec.VolumeName != "" {
			obj, err := f.client.Tracker().Get(pvGVR, "", stored.Spec.VolumeName)
			if err != nil {
				return true, nil, err
			}
			pv, ok := obj.(*corev1.PersistentVolume)
			if !ok {
				return true, nil, fmt.Errorf("unexpected PV type")
			}
			pv = pv.DeepCopy()
			if pv.Spec.ClaimRef == nil {
				pv.Spec.ClaimRef = &corev1.ObjectReference{APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: stored.Namespace, Name: stored.Name}
			}
			pv.Spec.ClaimRef.UID = stored.UID
			pv.Status.Phase = corev1.VolumeBound
			pv.ResourceVersion = "2"
			if err := f.client.Tracker().Update(pvGVR, pv, ""); err != nil {
				return true, nil, err
			}
			stored.Status.Phase = corev1.ClaimBound
			stored.Status.Capacity = stored.Spec.Resources.Requests.DeepCopy()
		} else {
			stored.Status.Phase = corev1.ClaimPending
		}
		if err := f.client.Tracker().Create(pvcGVR, stored, stored.Namespace); err != nil {
			return true, nil, err
		}
		return true, stored, nil
	})
	f.client.PrependReactor("delete", "persistentvolumeclaims", func(a ktesting.Action) (bool, runtime.Object, error) {
		da, ok := a.(ktesting.DeleteActionImpl)
		if !ok {
			return false, nil, nil
		}
		if len(da.GetDeleteOptions().DryRun) > 0 {
			return true, nil, nil
		}
		return false, nil, nil
	})

	// PodDefault discovery exists and advertises every reviewed verb.
	discovery := f.client.Discovery().(*fakediscovery.FakeDiscovery)
	discovery.Resources = []*metav1.APIResourceList{
		{
			GroupVersion: "kubeflow.org/v1",
			APIResources: []metav1.APIResource{
				{Name: "profiles", Namespaced: false, Kind: "Profile"},
				{Name: "notebooks", Namespaced: true, Kind: "Notebook"},
			},
		},
		{
			GroupVersion: "kubeflow.org/v1alpha1",
			APIResources: []metav1.APIResource{{
				Name: "poddefaults", Namespaced: true, Kind: "PodDefault",
				Verbs: []string{"get", "list", "create", "patch", "delete"},
			}},
		},
	}

	f.raw.PrependReactor("create", "poddefaults", func(a ktesting.Action) (bool, runtime.Object, error) {
		ca, ok := a.(ktesting.CreateActionImpl)
		if !ok {
			return false, nil, nil
		}
		obj, ok := ca.GetObject().(*unstructured.Unstructured)
		if !ok {
			return false, nil, nil
		}
		if len(ca.GetCreateOptions().DryRun) > 0 {
			return true, obj.DeepCopy(), nil
		}
		if f.failPD != nil {
			return true, nil, f.failPD
		}
		stored := obj.DeepCopy()
		stored.SetUID(types.UID("pd-" + stored.GetNamespace() + "-" + stored.GetName()))
		stored.SetResourceVersion("1")
		if err := f.raw.Tracker().Create(PodDefaults, stored, stored.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, stored, nil
	})
	f.raw.PrependReactor("patch", "poddefaults", func(a ktesting.Action) (bool, runtime.Object, error) {
		pa, ok := a.(ktesting.PatchActionImpl)
		if !ok {
			return false, nil, nil
		}
		if len(pa.GetPatchOptions().DryRun) > 0 {
			obj, err := f.raw.Tracker().Get(PodDefaults, pa.GetNamespace(), pa.GetName())
			return true, obj, err
		}
		obj, err := f.raw.Tracker().Get(PodDefaults, pa.GetNamespace(), pa.GetName())
		if err != nil {
			return true, nil, err
		}
		raw, err := json.Marshal(obj)
		if err != nil {
			return true, nil, err
		}
		patch, err := jsonpatch.DecodePatch(pa.GetPatch())
		if err != nil {
			return true, nil, err
		}
		out, err := patch.Apply(raw)
		if err != nil {
			return true, nil, err
		}
		updated := &unstructured.Unstructured{}
		if err := json.Unmarshal(out, &updated.Object); err != nil {
			return true, nil, err
		}
		rv, _ := strconv.Atoi(updated.GetResourceVersion())
		updated.SetResourceVersion(strconv.Itoa(rv + 1))
		if err := f.raw.Tracker().Update(PodDefaults, updated, updated.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, updated, nil
	})
	f.raw.PrependReactor("delete", "poddefaults", func(a ktesting.Action) (bool, runtime.Object, error) {
		da, ok := a.(ktesting.DeleteActionImpl)
		if !ok {
			return false, nil, nil
		}
		if len(da.GetDeleteOptions().DryRun) > 0 {
			return true, nil, nil
		}
		if f.failPDDelete != "" && da.GetName() == f.failPDDelete {
			return true, nil, f.failPDDeleteErr
		}
		obj, err := f.raw.Tracker().Get(PodDefaults, da.GetNamespace(), da.GetName())
		if err != nil {
			return true, nil, err
		}
		if pre := da.GetDeleteOptions().Preconditions; pre != nil && pre.UID != nil {
			if obj.(*unstructured.Unstructured).GetUID() != *pre.UID {
				return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "kubeflow.org", Resource: "poddefaults"}, da.GetName(), errors.New("uid precondition"))
			}
		}
		if err := f.raw.Tracker().Delete(PodDefaults, da.GetNamespace(), da.GetName()); err != nil {
			return true, nil, err
		}
		return true, nil, nil
	})
}

func registrationPV(name, class, path, capacity string) *corev1.PersistentVolume {
	fs := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("pv-" + name), ResourceVersion: "1"},
		Spec: corev1.PersistentVolumeSpec{
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(capacity)},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			VolumeMode:                    &fs,
			StorageClassName:              class,
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			MountOptions:                  []string{"nfsvers=3"},
			PersistentVolumeSource:        corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: "nfs.example.invalid", Path: path}},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable},
	}
}

func boundRegistrationPVC(name, ns, volumeName, uid string) *corev1.PersistentVolumeClaim {
	class := "nfs-static"
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid), ResourceVersion: "1"},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: &class,
			VolumeName:       volumeName,
			Resources:        corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}},
	}
}

func boundRegistrationPV(name, class, path, capacity, claimNS, claimName string) *corev1.PersistentVolume {
	pv := registrationPV(name, class, path, capacity)
	pv.Spec.ClaimRef = &corev1.ObjectReference{APIVersion: "v1", Kind: "PersistentVolumeClaim", Namespace: claimNS, Name: claimName, UID: types.UID("pvc-" + claimName)}
	pv.Status.Phase = corev1.VolumeBound
	return pv
}

func podDefaultFixture(ns, name string, claims []string, extra, external bool) *unstructured.Unstructured {
	volumes := []any{}
	mounts := []any{}
	for i, claim := range claims {
		vn := fmt.Sprintf("vol-%d", i)
		volumes = append(volumes, map[string]any{"name": vn, "persistentVolumeClaim": map[string]any{"claimName": claim}})
		mounts = append(mounts, map[string]any{"name": vn, "mountPath": fmt.Sprintf("/home/jovyan/%s", claim)})
	}
	spec := map[string]any{
		"desc":         "fixture",
		"selector":     map[string]any{"matchLabels": map[string]any{name: "true"}},
		"volumes":      volumes,
		"volumeMounts": mounts,
	}
	if extra {
		spec["env"] = []any{map[string]any{"name": "SECRET_LITERAL", "value": "DO_NOT_ECHO"}}
	}
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubeflow.org/v1alpha1", "kind": "PodDefault",
		"metadata": map[string]any{"name": name, "namespace": ns, "uid": "pd-uid-" + name, "resourceVersion": "1", "labels": map[string]any{"app.kubernetes.io/managed-by": "helm"}},
		"spec":     spec,
	}}
	if external {
		obj.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "fixture", UID: "deploy-1"}})
		obj.SetFinalizers([]string{"example.test/finalizer"})
	}
	if !extra {
		obj.SetLabels(map[string]string{})
	}
	return obj
}

func boundNFSAction(name string, pv *corev1.PersistentVolume) model.Action {
	return model.Action{
		Kind: "pvc.create.bound", Namespace: "alice", Name: name,
		Template: "nfs-reg", StorageClass: "nfs-static", PVName: pv.Name,
		TargetUID: string(pv.UID), TargetResourceVersion: pv.ResourceVersion,
		DirectoryUID: "1000", DirectoryGID: "2000",
		PodDefault: &model.PodDefaultRequest{Name: "mount-toast", MountPath: "/home/jovyan/" + name},
	}
}

func TestPVCreateStaticNFSRootAndEvidence(t *testing.T) {
	f := newRegistrationFixture(t, registrationConfig(), nil, nil)
	a := model.Action{Kind: "pv.create.static", Name: "alice-pv", Template: "nfs-reg", StorageClass: "nfs-static", NFSPath: "/exports/kubeflow/alice"}
	p, err := f.svc.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(p.Data), "/exports/kubeflow/alice") || strings.Contains(string(p.Data), "nfs.example.invalid") {
		t.Fatalf("durable evidence contains a raw path/server: %s", p.Data)
	}
	for _, change := range p.Changes {
		if strings.Contains(change.After, "/exports/kubeflow/alice") {
			t.Fatalf("durable change echoes the raw path: %+v", change)
		}
	}
	if err := f.svc.Execute(context.Background(), p, func(step string) error { f.checks = append(f.checks, step); return nil }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(f.checks, ","), "create-static-pv") {
		t.Fatalf("missing create checkpoint: %v", f.checks)
	}
	pv, err := f.client.CoreV1().PersistentVolumes().Get(context.Background(), "alice-pv", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Spec.NFS == nil || pv.Spec.NFS.Path != "/exports/kubeflow/alice" || pv.Spec.ClaimRef != nil || pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Fatalf("unexpected created PV: %+v", pv.Spec)
	}
	if pv.Annotations["pv.kubernetes.io/provisioned-by"] != "" {
		t.Fatal("static PV carries a dynamically provisioned-by annotation")
	}
	if pv.Status.Phase != corev1.VolumeAvailable {
		t.Fatalf("PV not verified Available: %v", pv.Status.Phase)
	}
}

func TestPVCreateStaticBeeGFSAndRejections(t *testing.T) {
	for _, suffix := range []string{"../escape", "/absolute", "a/%2e%2e/b", `a\b`, "a?b", "a#b", "a@b", "a:b", "a//b", strings.Repeat("a", maxBeeGFSHandleBytes)} {
		t.Run("reject-"+suffix, func(t *testing.T) {
			f := newRegistrationFixture(t, registrationConfig(), nil, nil)
			a := model.Action{Kind: "pv.create.static", Name: "bee-pv", Template: "beegfs-reg", StorageClass: "beegfs-static", BeeGFSPath: suffix}
			if _, err := f.svc.Prepare(context.Background(), a); err == nil {
				t.Fatalf("unsafe BeeGFS suffix %q accepted", suffix)
			}
		})
	}
	f := newRegistrationFixture(t, registrationConfig(), nil, nil)
	a := model.Action{Kind: "pv.create.static", Name: "bee-pv", Template: "beegfs-reg", StorageClass: "beegfs-static", BeeGFSPath: "example.user/kf-shared-volume"}
	p, err := f.svc.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(p.Data), "beegfs.example.invalid") || strings.Contains(string(p.Data), "kf-shared-volume") {
		t.Fatalf("durable evidence contains a raw handle: %s", p.Data)
	}
	if err := f.svc.Execute(context.Background(), p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	pv, err := f.client.CoreV1().PersistentVolumes().Get(context.Background(), "bee-pv", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != config.BeeGFSDriverName || pv.Spec.CSI.VolumeHandle != "beegfs://beegfs.example.invalid/home/example.user/kf-shared-volume" {
		t.Fatalf("unexpected BeeGFS PV: %+v", pv.Spec.CSI)
	}
	if pv.Annotations["pv.kubernetes.io/provisioned-by"] != "" {
		t.Fatal("static BeeGFS PV carries a dynamic provisioned-by annotation")
	}
}

func TestPVCreateStaticBeeGFSChecksProvisionerAndOverlap(t *testing.T) {
	cfg := registrationConfig()
	cfg.Storage.RegistrationTemplates[1].StorageClass = "nfs-static"
	f := newRegistrationFixture(t, cfg, nil, nil)
	a := model.Action{Kind: "pv.create.static", Name: "bee-pv", Template: "beegfs-reg", StorageClass: "nfs-static", BeeGFSPath: "example.user/volume"}
	if _, err := f.svc.Prepare(context.Background(), a); err == nil {
		t.Fatal("BeeGFS template accepted a non-BeeGFS provisioner")
	}

	cfg = registrationConfig()
	existing := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "existing", UID: "pv-existing", ResourceVersion: "1"}, Spec: corev1.PersistentVolumeSpec{
		Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("500Gi")}, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
		StorageClassName: "beegfs-static", PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
		PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: config.BeeGFSDriverName, VolumeHandle: "beegfs://beegfs.example.invalid/home/example.user"}},
	}}
	f = newRegistrationFixture(t, cfg, []runtime.Object{existing}, nil)
	a = model.Action{Kind: "pv.create.static", Name: "bee-overlap", Template: "beegfs-reg", StorageClass: "beegfs-static", BeeGFSPath: "example.user/other"}
	if _, err := f.svc.Prepare(context.Background(), a); err == nil {
		t.Fatal("overlapping BeeGFS backing path accepted")
	}
}

func TestPVCreateStaticRejectsNoncanonicalExistingBackendAliases(t *testing.T) {
	t.Run("nfs", func(t *testing.T) {
		existing := registrationPV("existing", "nfs-static", "/exports/kubeflow/alice/x/../workspace", "2400Gi")
		f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{existing}, nil)
		a := model.Action{Kind: "pv.create.static", Name: "new-pv", Template: "nfs-reg", StorageClass: "nfs-static", NFSPath: "/exports/kubeflow/alice/workspace"}
		if _, err := f.svc.Prepare(context.Background(), a); err == nil {
			t.Fatal("noncanonical existing NFS alias was not recognized as overlapping")
		}
	})
	t.Run("beegfs", func(t *testing.T) {
		existing := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "existing", UID: "pv-existing", ResourceVersion: "1"},
			Spec: corev1.PersistentVolumeSpec{
				Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("500Gi")}, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				StorageClassName: "beegfs-static", PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
				PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: config.BeeGFSDriverName, VolumeHandle: "beegfs://beegfs.example.invalid/home/example.user/x/../other"}},
			},
		}
		f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{existing}, nil)
		a := model.Action{Kind: "pv.create.static", Name: "new-bee", Template: "beegfs-reg", StorageClass: "beegfs-static", BeeGFSPath: "example.user/other"}
		if _, err := f.svc.Prepare(context.Background(), a); err == nil {
			t.Fatal("noncanonical existing BeeGFS alias was not recognized as overlapping")
		}
	})
}

func TestPVCCreateBoundNFSWithPodDefault(t *testing.T) {
	pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/workspace", "3000Gi")
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv}, nil)
	a := boundNFSAction("bound", pv)
	p, err := f.svc.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if p.TargetUID != string(pv.UID) || p.ResourceVersion != pv.ResourceVersion {
		t.Fatalf("plan does not bind the selected PV identity: %+v", p)
	}
	if strings.Contains(string(p.Data), "nfs.example.invalid") || strings.Contains(string(p.Data), "/exports/kubeflow") {
		t.Fatalf("durable evidence contains a raw backend path: %s", p.Data)
	}
	var receipts []model.MutationReceipt
	ctx := model.WithMutationReceiptRecorder(context.Background(), func(r model.MutationReceipt) error {
		receipts = append(receipts, r)
		return nil
	})
	if err := f.svc.Execute(ctx, p, func(step string) error { f.checks = append(f.checks, step); return nil }); err != nil {
		t.Fatal(err)
	}
	var createdPD bool
	for _, r := range receipts {
		if r.Kind == mutationKindPodDefault && r.Outcome == model.MutationOutcomeCreated && r.Name == "mount-toast" && r.UID == "pd-alice-mount-toast" && r.ResourceVersion != "" {
			createdPD = true
		}
	}
	if !createdPD {
		t.Fatalf("missing exact created PodDefault receipt: %+v", receipts)
	}
	want := []string{"reserve-selected-pv", "create-bound-pvc", "create-poddefault"}
	for _, step := range want {
		if !strings.Contains(strings.Join(f.checks, ","), step) {
			t.Fatalf("missing checkpoint %s: %v", step, f.checks)
		}
	}
	claim, err := f.client.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "bound", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if claim.Spec.VolumeName != "selected" || claim.Spec.Resources.Requests.Storage().String() != "10Gi" {
		t.Fatalf("unexpected bound PVC: %+v", claim.Spec)
	}
	if len(claim.Annotations) != 2 || claim.Annotations[nfsDirectoryUIDAnnotation] != "1000" || claim.Annotations[nfsDirectoryGIDAnnotation] != "2000" {
		t.Fatalf("unexpected NFS annotations: %v", claim.Annotations)
	}
	selected, err := f.client.CoreV1().PersistentVolumes().Get(context.Background(), "selected", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if selected.Spec.ClaimRef == nil || selected.Spec.ClaimRef.UID != claim.UID || selected.Status.Phase != corev1.VolumeBound {
		t.Fatalf("PV was not reserved/bound to the created claim: %+v", selected)
	}
	pd, err := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), "mount-toast", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pd.GetOwnerReferences()) != 0 || pd.GetAnnotations()[podDefaultWorkflowKey] != "pvc.create.bound" || pd.GetAnnotations()[podDefaultPVCUIDKey] != string(claim.UID) {
		t.Fatalf("unexpected PodDefault provenance: %+v", pd.GetAnnotations())
	}
	spec, _, _ := unstructured.NestedMap(pd.Object, "spec")
	if !same(spec, podDefaultSpec(podDefaultVolumeName("alice", "bound", "mount-toast"), "mount-toast", "bound", "/home/jovyan/bound")) {
		t.Fatalf("unexpected PodDefault spec: %+v", spec)
	}
	selector, _, _ := unstructured.NestedStringMap(pd.Object, "spec", "selector", "matchLabels")
	if len(selector) != 1 || selector["mount-toast"] != "true" {
		t.Fatalf("selector is not independent/dedicated: %v", selector)
	}
}

func TestPVCCreateBoundPartialOutcomesDoNotLookPreMutation(t *testing.T) {
	for name, failure := range map[string]error{
		"pvc-403-after-reservation": apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, "bound", errors.New("denied")),
		"pvc-409-after-reservation": apierrors.NewConflict(schema.GroupResource{Resource: "persistentvolumeclaims"}, "bound", errors.New("conflict")),
		"pvc-422-after-reservation": apierrors.NewInvalid(schema.GroupKind{Group: "", Kind: "PersistentVolumeClaim"}, "bound", nil),
	} {
		t.Run(name, func(t *testing.T) {
			pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/workspace", "2400Gi")
			f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv}, nil)
			f.failPVC = failure
			a := boundNFSAction("bound", pv)
			a.PodDefault = nil
			p, err := f.svc.Prepare(context.Background(), a)
			if err != nil {
				t.Fatal(err)
			}
			err = f.svc.Execute(context.Background(), p, func(string) error { return nil })
			if err == nil || errors.Is(err, model.ErrRejectedBeforeMutation) {
				t.Fatalf("post-reservation PVC failure was misclassified: %v", err)
			}
			selected, getErr := f.client.CoreV1().PersistentVolumes().Get(context.Background(), "selected", metav1.GetOptions{})
			if getErr != nil || selected.Spec.ClaimRef == nil || selected.Spec.ClaimRef.Name != "bound" {
				t.Fatalf("PV reservation must remain for review: %+v %v", selected, getErr)
			}
			if _, getErr := f.client.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "bound", metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
				t.Fatalf("unexpected PVC after failure: %v", getErr)
			}
		})
	}
	t.Run("poddefault-fails-after-pvc", func(t *testing.T) {
		pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/workspace", "2400Gi")
		f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv}, nil)
		f.failPD = apierrors.NewForbidden(schema.GroupResource{Group: "kubeflow.org", Resource: "poddefaults"}, "mount-toast", errors.New("denied"))
		a := boundNFSAction("bound", pv)
		p, err := f.svc.Prepare(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		err = f.svc.Execute(context.Background(), p, func(string) error { return nil })
		if err == nil || errors.Is(err, model.ErrRejectedBeforeMutation) {
			t.Fatalf("post-PVC PodDefault failure was misclassified: %v", err)
		}
		if _, getErr := f.client.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "bound", metav1.GetOptions{}); getErr != nil {
			t.Fatalf("PVC was not retained after PodDefault failure: %v", getErr)
		}
	})
}

// managerRegistration runs the real kube backend through the durable
// operations manager. It proves the manager's needs-review decision and the
// persisted checkpoint stage, not only a direct Execute error.
func managerRegistration(t *testing.T, f *registrationFixture) (*operations.Manager, *store.Store, model.Identity) {
	t.Helper()
	cfg := f.cfg
	cfg.Auth = config.Auth{IssuerURL: "https://issuer.example", SessionMinutes: 15, AdminGroups: []string{"admins"}}
	st, err := store.Open(filepath.Join(t.TempDir(), "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := operations.New(context.Background(), cfg, st, f.svc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close(); st.Close() })
	now := time.Now()
	sum := sha256.Sum256([]byte("registration-test-session"))
	actor := model.Identity{
		SessionID: base64.RawURLEncoding.EncodeToString(sum[:]), Issuer: cfg.Auth.IssuerURL,
		Subject: "admin", Groups: []string{"admins"}, AuthenticatedAt: now, ExpiresAt: now.Add(15 * time.Minute),
	}
	if err := st.Put(context.Background(), "auth.sessions", actor.SessionID, map[string]any{"identity": actor}); err != nil {
		t.Fatal(err)
	}
	return m, st, actor
}

func waitRegistrationOperation(t *testing.T, m *operations.Manager, id string) model.Operation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		op, err := m.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if op.State == "succeeded" || op.State == "failed" || op.State == "needs-review" {
			return op
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("operation did not reach a terminal state")
	return model.Operation{}
}

func submitRegistration(t *testing.T, m *operations.Manager, actor model.Identity, a model.Action, key string) model.Operation {
	t.Helper()
	plan, err := m.Plan(context.Background(), actor, a)
	if err != nil {
		t.Fatal(err)
	}
	op, err := m.Submit(context.Background(), actor, plan.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	return waitRegistrationOperation(t, m, op.ID)
}

func TestMutationReceiptsExactReservedPVAndCreatedPVCBeforePodDefaultFailure(t *testing.T) {
	pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/workspace", "2400Gi")
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv}, nil)
	f.failPD = apierrors.NewForbidden(schema.GroupResource{Group: "kubeflow.org", Resource: "poddefaults"}, "mount-toast", errors.New("denied"))
	p, err := f.svc.Prepare(context.Background(), boundNFSAction("bound", pv))
	if err != nil {
		t.Fatal(err)
	}
	var receipts []model.MutationReceipt
	ctx := model.WithMutationReceiptRecorder(context.Background(), func(r model.MutationReceipt) error {
		receipts = append(receipts, r)
		return nil
	})
	err = f.svc.Execute(ctx, p, func(string) error { return nil })
	if err == nil || errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("partial PodDefault failure misclassified: %v", err)
	}
	var reservedPV, createdPVC *model.MutationReceipt
	for i := range receipts {
		switch {
		case receipts[i].Kind == mutationKindPersistentVolume && receipts[i].Outcome == model.MutationOutcomeReserved:
			reservedPV = &receipts[i]
		case receipts[i].Kind == mutationKindPersistentVolumeClaim && receipts[i].Outcome == model.MutationOutcomeCreated:
			createdPVC = &receipts[i]
		}
	}
	if reservedPV == nil || reservedPV.Name != "selected" || reservedPV.UID != string(pv.UID) || reservedPV.ResourceVersion == "" {
		t.Fatalf("missing exact reserved PV receipt: %+v", receipts)
	}
	if createdPVC == nil || createdPVC.Namespace != "alice" || createdPVC.Name != "bound" || createdPVC.UID != "pvc-bound" || createdPVC.ResourceVersion == "" {
		t.Fatalf("missing exact created PVC receipt: %+v", receipts)
	}
	for _, r := range receipts {
		if r.Kind == mutationKindPodDefault {
			t.Fatalf("false PodDefault receipt after failed write: %+v", r)
		}
	}
}

func TestMutationReceiptsDeletedExactReviewedIdentity(t *testing.T) {
	pv := boundRegistrationPV("data-pv", "nfs-static", "/exports/kubeflow/alice/data", "2400Gi", "alice", "data")
	pvc := boundRegistrationPVC("data", "alice", "data-pv", "pvc-data")
	pd := podDefaultFixture("alice", "mount-a", []string{"data"}, false, false)
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv, pvc}, []runtime.Object{pd})
	a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "data", ConfirmName: "data", Reason: "test", TargetUID: string(pvc.UID), TargetResourceVersion: pvc.ResourceVersion, ConfirmPodDefaultCleanup: true}
	p, err := f.svc.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	var receipts []model.MutationReceipt
	ctx := model.WithMutationReceiptRecorder(context.Background(), func(r model.MutationReceipt) error {
		receipts = append(receipts, r)
		return nil
	})
	if err := f.svc.Execute(ctx, p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var deletedPD, deletedPVC bool
	for _, r := range receipts {
		if r.Outcome != model.MutationOutcomeDeleted {
			continue
		}
		if r.Kind == mutationKindPodDefault && r.Name == "mount-a" && r.UID == "pd-uid-mount-a" && r.ResourceVersion == "1" && r.Namespace == "alice" {
			deletedPD = true
		}
		if r.Kind == mutationKindPersistentVolumeClaim && r.Name == "data" && r.UID == string(pvc.UID) && r.ResourceVersion == pvc.ResourceVersion {
			deletedPVC = true
		}
	}
	if !deletedPD || !deletedPVC {
		t.Fatalf("missing exact deletion receipts: %+v", receipts)
	}
}

func TestMutationReceiptRecorderFailureStopsAfterWriteWithoutSentinel(t *testing.T) {
	pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/workspace", "2400Gi")
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv}, nil)
	p, err := f.svc.Prepare(context.Background(), boundNFSAction("bound", pv))
	if err != nil {
		t.Fatal(err)
	}
	ctx := model.WithMutationReceiptRecorder(context.Background(), func(r model.MutationReceipt) error {
		if r.Kind == mutationKindPersistentVolume && r.Outcome == model.MutationOutcomeReserved {
			return errors.New("durable receipt store unavailable")
		}
		return nil
	})
	err = f.svc.Execute(ctx, p, func(string) error { return nil })
	if err == nil || errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("recorder failure after reservation was misclassified: %v", err)
	}
	selected, getErr := f.client.CoreV1().PersistentVolumes().Get(context.Background(), "selected", metav1.GetOptions{})
	if getErr != nil || selected.Spec.ClaimRef == nil || selected.Spec.ClaimRef.Name != "bound" {
		t.Fatalf("reservation should remain for review: %+v %v", selected, getErr)
	}
	if _, getErr := f.client.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "bound", metav1.GetOptions{}); !apierrors.IsNotFound(getErr) {
		t.Fatalf("execution continued to PVC create after receipt failure: %v", getErr)
	}
}

func TestManagerPostReservationPVCFailureKeepsReservationStage(t *testing.T) {
	pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/workspace", "2400Gi")
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv}, nil)
	f.failPVC = apierrors.NewForbidden(schema.GroupResource{Resource: "persistentvolumeclaims"}, "bound", errors.New("denied"))
	m, _, actor := managerRegistration(t, f)
	a := boundNFSAction("bound", pv)
	a.PodDefault = nil
	op := submitRegistration(t, m, actor, a, "manager-reservation-failure")
	if op.State != "needs-review" {
		t.Fatalf("post-reservation PVC failure state = %+v", op)
	}
	if op.Step != "create-bound-pvc" {
		t.Fatalf("durable checkpoint does not retain the post-reservation stage: %+v", op)
	}
	selected, err := f.client.CoreV1().PersistentVolumes().Get(context.Background(), "selected", metav1.GetOptions{})
	if err != nil || selected.Spec.ClaimRef == nil || selected.Spec.ClaimRef.Name != "bound" {
		t.Fatalf("reservation receipt not observable on the cluster: %+v %v", selected, err)
	}
}

func TestManagerBoundPodDefaultFailureKeepsPVCStage(t *testing.T) {
	pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/workspace", "2400Gi")
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv}, nil)
	f.failPD = apierrors.NewForbidden(schema.GroupResource{Group: "kubeflow.org", Resource: "poddefaults"}, "mount-toast", errors.New("denied"))
	m, _, actor := managerRegistration(t, f)
	op := submitRegistration(t, m, actor, boundNFSAction("bound", pv), "manager-poddefault-failure")
	if op.State != "needs-review" {
		t.Fatalf("partial PodDefault state = %+v", op)
	}
	if op.Step != "create-poddefault" {
		t.Fatalf("durable checkpoint does not identify the unfinished PodDefault step: %+v", op)
	}
	if _, err := f.client.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "bound", metav1.GetOptions{}); err != nil {
		t.Fatalf("manager lost the completed PVC: %v", err)
	}
}

func TestManagerMutationReceiptsPersistExactCreatedAndReservedIdentities(t *testing.T) {
	pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/workspace", "2400Gi")
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv}, nil)
	f.failPD = apierrors.NewForbidden(schema.GroupResource{Group: "kubeflow.org", Resource: "poddefaults"}, "mount-toast", errors.New("denied"))
	m, st, actor := managerRegistration(t, f)
	plan, err := m.Plan(context.Background(), actor, boundNFSAction("bound", pv))
	if err != nil {
		t.Fatal(err)
	}
	submitted, err := m.Submit(context.Background(), actor, plan.ID, "manager-mutation-receipts")
	if err != nil {
		t.Fatal(err)
	}
	op := waitRegistrationOperation(t, m, submitted.ID)
	if op.State != "needs-review" {
		t.Fatalf("partial PodDefault state = %+v", op)
	}
	// Receipts live on the operation, not the ephemeral preview plan.
	if err := st.Delete(context.Background(), "plans", plan.ID); err != nil {
		t.Fatal(err)
	}
	op, err = m.Get(context.Background(), op.ID)
	if err != nil || op.State != "needs-review" {
		t.Fatalf("operation after plan expiry = %+v, %v", op, err)
	}
	var reservedPV, createdPVC bool
	for _, r := range op.MutationReceipts {
		if r.Kind == mutationKindPersistentVolume && r.Outcome == model.MutationOutcomeReserved && r.Name == "selected" && r.UID == string(pv.UID) && r.ResourceVersion != "" {
			reservedPV = true
		}
		if r.Kind == mutationKindPersistentVolumeClaim && r.Outcome == model.MutationOutcomeCreated && r.Namespace == "alice" && r.Name == "bound" && r.UID == "pvc-bound" && r.ResourceVersion != "" {
			createdPVC = true
		}
	}
	if !reservedPV || !createdPVC {
		t.Fatalf("operation receipts lost exact identities: %+v", op.MutationReceipts)
	}
}

func TestManagerCleanupFailureCheckpointMapsReviewedIndex(t *testing.T) {
	pv := boundRegistrationPV("data-pv", "nfs-static", "/exports/kubeflow/alice/data", "2400Gi", "alice", "data")
	pvc := boundRegistrationPVC("data", "alice", "data-pv", "pvc-data")
	aPD := podDefaultFixture("alice", "mount-a", []string{"data"}, false, false)
	bPD := podDefaultFixture("alice", "mount-b", []string{"data"}, false, false)
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv, pvc}, []runtime.Object{aPD, bPD})
	f.failPDDelete = "mount-b"
	f.failPDDeleteErr = apierrors.NewInternalError(errors.New("ambiguous"))
	m, _, actor := managerRegistration(t, f)
	a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "data", ConfirmName: "data", Reason: "test", TargetUID: string(pvc.UID), TargetResourceVersion: pvc.ResourceVersion, ConfirmPodDefaultCleanup: true}
	op := submitRegistration(t, m, actor, a, "manager-cleanup-partial")
	if op.State != "needs-review" {
		t.Fatalf("partial cleanup state = %+v", op)
	}
	// Data.podDefaults is sorted by name, so index 2 is mount-b; reaching
	// this pre-write checkpoint proves mount-a's completion checkpoint ran.
	if op.Step != "delete-poddefault-2" {
		t.Fatalf("durable checkpoint does not map the reviewed cleanup index: %+v", op)
	}
	if _, err := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), "mount-a", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("first reviewed dependency was not completed before the failure: %v", err)
	}
	if _, err := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), "mount-b", metav1.GetOptions{}); err != nil {
		t.Fatalf("failed reviewed dependency must remain for review: %v", err)
	}
	if _, err := f.client.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "data", metav1.GetOptions{}); err != nil {
		t.Fatalf("PVC must remain after partial cleanup: %v", err)
	}
}

func TestPVCCreateBoundRejectsClientOverridesAndIdentities(t *testing.T) {
	pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/workspace", "2400Gi")
	cases := map[string]func(*model.Action){
		"nfsPath on bound":         func(a *model.Action) { a.NFSPath = "/exports/kubeflow/alice/workspace" },
		"beeGFSPath on bound":      func(a *model.Action) { a.BeeGFSPath = "user/path" },
		"missing uid":              func(a *model.Action) { a.DirectoryUID = "" },
		"zero uid":                 func(a *model.Action) { a.DirectoryUID = "0" },
		"leading zero":             func(a *model.Action) { a.DirectoryUID = "0100" },
		"signed id":                func(a *model.Action) { a.DirectoryGID = "+1" },
		"overflow id":              func(a *model.Action) { a.DirectoryGID = "4294967295" },
		"stale target":             func(a *model.Action) { a.TargetUID = "other" },
		"wrong class":              func(a *model.Action) { a.StorageClass = "beegfs-static" },
		"poddefault name 53":       func(a *model.Action) { a.PodDefault.Name = strings.Repeat("a", 53) },
		"poddefault home exact":    func(a *model.Action) { a.PodDefault.MountPath = "/home/jovyan" },
		"poddefault home ancestor": func(a *model.Action) { a.PodDefault.MountPath = "/home" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv.DeepCopy()}, nil)
			a := boundNFSAction("bound", pv)
			mutate(&a)
			if _, err := f.svc.Prepare(context.Background(), a); err == nil {
				t.Fatal("unsafe bound action accepted")
			}
		})
	}
}

func TestPVCCreateBoundSelectedPVEligibility(t *testing.T) {
	now := metav1.Now()
	cases := map[string]func(*corev1.PersistentVolume){
		"capacity-below-request": func(pv *corev1.PersistentVolume) {
			pv.Spec.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")}
		},
		"not-available": func(pv *corev1.PersistentVolume) { pv.Status.Phase = corev1.VolumeBound },
		"already-reserved": func(pv *corev1.PersistentVolume) {
			pv.Spec.ClaimRef = &corev1.ObjectReference{Namespace: "alice", Name: "other"}
		},
		"topology-constrained": func(pv *corev1.PersistentVolume) {
			pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{}}
		},
		"extra-access-mode": func(pv *corev1.PersistentVolume) {
			pv.Spec.AccessModes = append(pv.Spec.AccessModes, corev1.ReadWriteOnce)
		},
		"path-outside-root": func(pv *corev1.PersistentVolume) { pv.Spec.NFS.Path = "/exports/kubeflow/bob/workspace" },
		"terminating":       func(pv *corev1.PersistentVolume) { pv.DeletionTimestamp = &now },
		"reclaim-delete": func(pv *corev1.PersistentVolume) {
			pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/workspace", "2400Gi")
			mutate(pv)
			f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv}, nil)
			if _, err := f.svc.Prepare(context.Background(), boundNFSAction("bound", pv)); err == nil {
				t.Fatal("ineligible selected PV accepted")
			}
		})
	}
}

func TestPVCCreateBoundPodDefaultFeatureGate(t *testing.T) {
	pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/workspace", "2400Gi")
	cfg := registrationConfig()
	cfg.Features.PodDefaults = false
	f := newRegistrationFixture(t, cfg, []runtime.Object{pv}, nil)
	if _, err := f.svc.Prepare(context.Background(), boundNFSAction("bound", pv)); err == nil {
		t.Fatal("PodDefault request accepted while features.podDefaults is false")
	}
}

func TestPVCCreateBoundPodDefaultOverwritePreservesMetadata(t *testing.T) {
	pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/workspace", "2400Gi")
	existing := podDefaultFixture("alice", "mount-toast", []string{"other-claim"}, true, true)
	existing.SetAnnotations(map[string]string{
		"example.test/foreign": "preserve",
		podDefaultWorkflowKey:  "pvc.create.dynamic",
		podDefaultPVCUIDKey:    "stale-pvc-uid",
	})
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv, boundRegistrationPVC("other-claim", "alice", "", "pvc-other")}, []runtime.Object{existing})
	a := boundNFSAction("bound", pv)
	a.PodDefault.Overwrite = false
	if _, err := f.svc.Prepare(context.Background(), a); err == nil {
		t.Fatal("existing PodDefault was silently accepted without overwrite")
	}
	a.PodDefault.Overwrite = true
	p, err := f.svc.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(p.Data), "DO_NOT_ECHO") || strings.Contains(strings.Join(p.Warnings, " "), "DO_NOT_ECHO") {
		t.Fatal("secret-bearing PodDefault value leaked into evidence/warnings")
	}
	if !strings.Contains(strings.Join(p.Warnings, " "), "external-management") {
		t.Fatalf("missing external-management warning: %v", p.Warnings)
	}
	var receipts []model.MutationReceipt
	ctx := model.WithMutationReceiptRecorder(context.Background(), func(r model.MutationReceipt) error {
		receipts = append(receipts, r)
		return nil
	})
	if err := f.svc.Execute(ctx, p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var overwritten bool
	for _, r := range receipts {
		if r.Kind == mutationKindPodDefault && r.Outcome == model.MutationOutcomeOverwritten && r.Name == "mount-toast" && r.UID == "pd-uid-mount-toast" && r.ResourceVersion != "" {
			overwritten = true
		}
	}
	if !overwritten {
		t.Fatalf("missing exact PodDefault overwrite receipt: %+v", receipts)
	}
	pd, err := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), "mount-toast", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	spec, _, _ := unstructured.NestedMap(pd.Object, "spec")
	if !same(spec, podDefaultSpec(podDefaultVolumeName("alice", "bound", "mount-toast"), "mount-toast", "bound", "/home/jovyan/bound")) {
		t.Fatalf("overwrite did not replace the whole dedicated spec: %+v", spec)
	}
	if pd.GetLabels()["app.kubernetes.io/managed-by"] != "helm" || len(pd.GetOwnerReferences()) != 1 || len(pd.GetFinalizers()) != 1 {
		t.Fatalf("metadata was not preserved: labels=%v owners=%v finalizers=%v", pd.GetLabels(), pd.GetOwnerReferences(), pd.GetFinalizers())
	}
	if pd.GetAnnotations()["example.test/foreign"] != "preserve" || pd.GetAnnotations()[podDefaultWorkflowKey] != "pvc.create.bound" || pd.GetAnnotations()[podDefaultPVCUIDKey] != "pvc-bound" {
		t.Fatalf("console provenance was not updated while foreign annotations were preserved: %v", pd.GetAnnotations())
	}
}

func TestLegacyStaticAndReserveRejectOptionalPodDefault(t *testing.T) {
	cfg := registrationConfig()
	cfg.Storage.NFSTemplates = []config.NFSTemplate{{
		Name: "legacy-nfs", Server: "nfs.example.invalid", BasePath: "/prepared",
		StorageClass: "nfs-static", AccessModes: []string{"ReadWriteMany"}, MountOptions: []string{"nfsvers=3"},
	}}
	f := newRegistrationFixture(t, cfg, nil, nil)
	pd := &model.PodDefaultRequest{Name: "mount-legacy", MountPath: "/home/jovyan/legacy-claim"}
	actions := []model.Action{
		{Kind: "storage.create.static", Namespace: "alice", Name: "legacy-claim", PVName: "legacy-pv", Template: "legacy-nfs", Subdirectory: "alice/data", StorageClass: "nfs-static", Size: "10Gi", AccessMode: "ReadWriteMany", PodDefault: pd},
		{Kind: "pv.reserve-and-claim", Namespace: "alice", Name: "reserved", PVName: "ancient-pv", StorageClass: "nfs-static", Size: "10Gi", AccessMode: "ReadWriteMany", PodDefault: pd},
	}
	for _, a := range actions {
		if _, err := f.svc.Prepare(context.Background(), a); err == nil {
			t.Fatalf("legacy action %s accepted an optional PodDefault", a.Kind)
		}
	}
}

func TestPodDefaultOverwriteAddsProvenanceWhenAnnotationsAbsent(t *testing.T) {
	pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/workspace", "2400Gi")
	existing := podDefaultFixture("alice", "mount-toast", []string{"other-claim"}, false, false)
	existing.SetAnnotations(nil)
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
	if pd.GetAnnotations()[podDefaultWorkflowKey] != "pvc.create.bound" || pd.GetAnnotations()[podDefaultPVCUIDKey] != "pvc-bound" {
		t.Fatalf("provenance not added when annotations were absent: %v", pd.GetAnnotations())
	}
}

func TestDynamicPVCOptionalPodDefaultWhilePending(t *testing.T) {
	f := newRegistrationFixture(t, registrationConfig(), nil, nil)
	a := model.Action{
		Kind: "pvc.create.dynamic", Namespace: "alice", Name: "dynamic", StorageClass: "dynamic-class",
		Size: "5Gi", AccessMode: "ReadWriteMany", Reason: "test",
		PodDefault: &model.PodDefaultRequest{Name: "mount-dynamic", MountPath: "/home/jovyan/dynamic"},
	}
	p, err := f.svc.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.Warnings, " "), "not storage provisioning") {
		t.Fatalf("missing pending/PodDefault caveat: %v", p.Warnings)
	}
	if err := f.svc.Execute(context.Background(), p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	claim, err := f.client.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "dynamic", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if claim.Status.Phase != corev1.ClaimPending {
		t.Fatalf("fixture expected a Pending WFFC claim, got %v", claim.Status.Phase)
	}
	if _, err := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), "mount-dynamic", metav1.GetOptions{}); err != nil {
		t.Fatalf("optional PodDefault was not created for the Pending claim: %v", err)
	}
}

func TestPVDeleteCleanupWorkflow(t *testing.T) {
	pv := boundRegistrationPV("data-pv", "nfs-static", "/exports/kubeflow/alice/data", "2400Gi", "alice", "data")
	pvc := boundRegistrationPVC("data", "alice", "data-pv", "pvc-data")
	other := boundRegistrationPVC("other", "alice", "", "pvc-other")
	other2 := boundRegistrationPVC("other2", "alice", "", "pvc-other2")
	shared := podDefaultFixture("alice", "mount-a", []string{"data", "other"}, true, true)
	dedicated := podDefaultFixture("alice", "mount-b", []string{"data"}, false, false)
	unrelated := podDefaultFixture("alice", "mount-elsewhere", []string{"other2"}, false, false)
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv, pvc, other, other2}, []runtime.Object{shared, dedicated, unrelated})
	a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "data", ConfirmName: "data", Reason: "test cleanup", TargetUID: string(pvc.UID), TargetResourceVersion: pvc.ResourceVersion, ConfirmPodDefaultCleanup: true}
	p, err := f.svc.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	var evidence map[string]any
	if err := json.Unmarshal(p.Data, &evidence); err != nil {
		t.Fatal(err)
	}
	list, ok := evidence["podDefaults"].([]any)
	if !ok || len(list) != 2 {
		t.Fatalf("cleanup evidence is not the canonical bounded list: %s", p.Data)
	}
	for _, raw := range list {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("cleanup entry is not an object: %s", p.Data)
		}
		for _, key := range []string{"name", "namespace", "uid", "resourceVersion"} {
			if entry[key] == nil || entry[key] == "" {
				t.Fatalf("cleanup entry lacks %s: %v", key, entry)
			}
		}
		if _, ok := entry["shared"]; !ok {
			t.Fatalf("cleanup entry lacks shared: %v", entry)
		}
		if _, ok := entry["externalManaged"]; !ok {
			t.Fatalf("cleanup entry lacks externalManaged: %v", entry)
		}
		if _, ok := entry["additionalSettings"]; !ok {
			t.Fatalf("cleanup entry lacks additionalSettings: %v", entry)
		}
	}
	if evidence["podDefaultAPI"] != "present" {
		t.Fatalf("podDefaultAPI state missing: %s", p.Data)
	}
	if !strings.Contains(strings.Join(p.Warnings, " "), "other PVCs") || !strings.Contains(strings.Join(p.Warnings, " "), "external-management") {
		t.Fatalf("shared/external warning missing: %v", p.Warnings)
	}
	if err := f.svc.Execute(context.Background(), p, func(step string) error { f.checks = append(f.checks, step); return nil }); err != nil {
		t.Fatal(err)
	}
	steps := strings.Join(f.checks, ",")
	if !strings.Contains(steps, "delete-poddefault-1") || !strings.Contains(steps, "delete-poddefault-2") || !strings.Contains(steps, "delete-pvc") {
		t.Fatalf("missing per-write checkpoints: %v", f.checks)
	}
	for _, name := range []string{"mount-a", "mount-b"} {
		if _, err := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatalf("related PodDefault %s was not deleted: %v", name, err)
		}
	}
	if _, err := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), "mount-elsewhere", metav1.GetOptions{}); err != nil {
		t.Fatalf("unrelated PodDefault was touched: %v", err)
	}
	if _, err := f.client.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "data", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("target PVC was not deleted: %v", err)
	}
	for _, name := range []string{"other", "other2"} {
		if _, err := f.client.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), name, metav1.GetOptions{}); err != nil {
			t.Fatalf("other PVC %s was touched: %v", name, err)
		}
	}
	if _, err := f.client.CoreV1().PersistentVolumes().Get(context.Background(), "data-pv", metav1.GetOptions{}); err != nil {
		t.Fatalf("bound PV was deleted by PVC cleanup: %v", err)
	}
}

func TestPVDeleteCleanupFailClosed(t *testing.T) {
	newFixture := func(t *testing.T, cfg config.Config, pds ...runtime.Object) (*registrationFixture, model.Action) {
		pv := boundRegistrationPV("data-pv", "nfs-static", "/exports/kubeflow/alice/data", "2400Gi", "alice", "data")
		pvc := boundRegistrationPVC("data", "alice", "data-pv", "pvc-data")
		f := newRegistrationFixture(t, cfg, []runtime.Object{pv, pvc}, pds)
		return f, model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "data", ConfirmName: "data", Reason: "test", TargetUID: string(pvc.UID), TargetResourceVersion: pvc.ResourceVersion, ConfirmPodDefaultCleanup: true}
	}
	t.Run("feature-disabled", func(t *testing.T) {
		cfg := registrationConfig()
		cfg.Features.PodDefaults = false
		f, a := newFixture(t, cfg, podDefaultFixture("alice", "mount-a", []string{"data"}, false, false))
		if _, err := f.svc.Prepare(context.Background(), a); err == nil {
			t.Fatal("cleanup accepted while podDefaults feature is disabled")
		}
	})
	t.Run("acknowledgement-missing", func(t *testing.T) {
		f, a := newFixture(t, registrationConfig(), podDefaultFixture("alice", "mount-a", []string{"data"}, false, false))
		a.ConfirmPodDefaultCleanup = false
		if _, err := f.svc.Prepare(context.Background(), a); err == nil {
			t.Fatal("cleanup accepted without acknowledgement")
		}
	})
	t.Run("bound-limit", func(t *testing.T) {
		pds := make([]runtime.Object, 0, maxPodDefaultCleanup+1)
		for i := 0; i < maxPodDefaultCleanup+1; i++ {
			pds = append(pds, podDefaultFixture("alice", fmt.Sprintf("mount-%02d", i), []string{"data"}, false, false))
		}
		f, a := newFixture(t, registrationConfig(), pds...)
		if _, err := f.svc.Prepare(context.Background(), a); err == nil {
			t.Fatal("oversized cleanup set was truncated or accepted")
		}
	})
	t.Run("discovery-denied", func(t *testing.T) {
		f, a := newFixture(t, registrationConfig(), podDefaultFixture("alice", "mount-a", []string{"data"}, false, false))
		discovery := f.client.Discovery().(*fakediscovery.FakeDiscovery)
		discovery.PrependReactor("get", "resource", func(ktesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "kubeflow.org", Resource: "poddefaults"}, "", errors.New("denied"))
		})
		if _, err := f.svc.Prepare(context.Background(), a); err == nil {
			t.Fatal("discovery denial was treated as no references")
		}
	})
	t.Run("malformed-structure", func(t *testing.T) {
		bad := podDefaultFixture("alice", "mount-bad", []string{"data"}, false, false)
		bad.Object["spec"].(map[string]any)["volumes"] = "not-a-list"
		f, a := newFixture(t, registrationConfig(), bad)
		if _, err := f.svc.Prepare(context.Background(), a); err == nil {
			t.Fatal("malformed PodDefault structure was treated as no reference")
		}
	})
	t.Run("pod-reference-blocks-before-cleanup", func(t *testing.T) {
		pv := boundRegistrationPV("data-pv", "nfs-static", "/exports/kubeflow/alice/data", "2400Gi", "alice", "data")
		pvc := boundRegistrationPVC("data", "alice", "data-pv", "pvc-data")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "uses-data", Namespace: "alice"},
			Spec: corev1.PodSpec{Volumes: []corev1.Volume{{
				Name:         "data",
				VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data"}},
			}}},
		}
		f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv, pvc, pod}, []runtime.Object{podDefaultFixture("alice", "mount-a", []string{"data"}, false, false)})
		a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "data", ConfirmName: "data", Reason: "test", TargetUID: string(pvc.UID), TargetResourceVersion: pvc.ResourceVersion, ConfirmPodDefaultCleanup: true}
		if _, err := f.svc.Prepare(context.Background(), a); err == nil {
			t.Fatal("referenced PVC accepted before PodDefault cleanup")
		}
		if _, err := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), "mount-a", metav1.GetOptions{}); err != nil {
			t.Fatal("PodDefault was deleted before the Pod reference refusal")
		}
	})
	t.Run("late-unreviewed-reference", func(t *testing.T) {
		f, a := newFixture(t, registrationConfig(), podDefaultFixture("alice", "mount-a", []string{"data"}, false, false))
		p, err := f.svc.Prepare(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		late := podDefaultFixture("alice", "mount-late", []string{"data"}, false, false)
		if err := f.raw.Tracker().Create(PodDefaults, late, "alice"); err != nil {
			t.Fatal(err)
		}
		if err := f.svc.Execute(context.Background(), p, func(string) error { return nil }); !errors.Is(err, ErrConflict) {
			t.Fatalf("late reference was not refused: %v", err)
		}
		for _, name := range []string{"mount-a", "mount-late"} {
			if _, err := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), name, metav1.GetOptions{}); err != nil {
				t.Fatalf("PodDefault %s was deleted despite stale preview: %v", name, err)
			}
		}
	})
}

func TestPVDeleteCleanupAllowsLongExistingPodDefaultName(t *testing.T) {
	pv := boundRegistrationPV("data-pv", "nfs-static", "/exports/kubeflow/alice/data", "2400Gi", "alice", "data")
	pvc := boundRegistrationPVC("data", "alice", "data-pv", "pvc-data")
	// The creation-only 52-byte cap must not reject or skip an existing valid
	// Kubernetes object name during cleanup. This name is also longer than a
	// label-key name segment, so the old selector is reviewed as additional
	// configuration while the PVC reference still makes the object related.
	longName := strings.Repeat("a", 70)
	pd := podDefaultFixture("alice", longName, []string{"data"}, false, false)
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv, pvc}, []runtime.Object{pd})
	a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "data", ConfirmName: "data", Reason: "test long dependency", TargetUID: string(pvc.UID), TargetResourceVersion: pvc.ResourceVersion, ConfirmPodDefaultCleanup: true}
	p, err := f.svc.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(p.Data), longName) {
		t.Fatalf("long existing PodDefault was not reviewed: %s", p.Data)
	}
	if err := f.svc.Execute(context.Background(), p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), longName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("long-named reviewed PodDefault was not deleted: %v", err)
	}
	if _, err := f.client.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "data", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("PVC was not deleted after long-name cleanup: %v", err)
	}
}

func TestPVDeleteGenuinePodDefaultCRDAbsenceIsExplicit(t *testing.T) {
	pv := boundRegistrationPV("data-pv", "nfs-static", "/exports/kubeflow/alice/data", "2400Gi", "alice", "data")
	pvc := boundRegistrationPVC("data", "alice", "data-pv", "pvc-data")
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv, pvc}, nil)
	discovery := f.client.Discovery().(*fakediscovery.FakeDiscovery)
	// Explicit absence of only the PodDefault resource: kubeflow.org/v1 is
	// served (as required for Profiles/Notebooks) but has no poddefaults entry,
	// and no other kubeflow.org version advertises one.
	discovery.Resources = []*metav1.APIResourceList{{
		GroupVersion: "kubeflow.org/v1",
		APIResources: []metav1.APIResource{
			{Name: "profiles", Namespaced: false, Kind: "Profile"},
			{Name: "notebooks", Namespaced: true, Kind: "Notebook"},
		},
	}}
	a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "data", ConfirmName: "data", Reason: "test", TargetUID: string(pvc.UID), TargetResourceVersion: pvc.ResourceVersion}
	p, err := f.svc.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	var evidence map[string]any
	if err := json.Unmarshal(p.Data, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence["podDefaultAPI"] != "absent" {
		t.Fatalf("genuine CRD absence is not explicit: %s", p.Data)
	}
	if _, ok := evidence["podDefaults"].([]any); !ok {
		t.Fatalf("cleanup dependency list is not explicitly present: %s", p.Data)
	}
}

func TestPVDeleteCleanupPartialProgress(t *testing.T) {
	pv := boundRegistrationPV("data-pv", "nfs-static", "/exports/kubeflow/alice/data", "2400Gi", "alice", "data")
	pvc := boundRegistrationPVC("data", "alice", "data-pv", "pvc-data")
	aPD := podDefaultFixture("alice", "mount-a", []string{"data"}, false, false)
	bPD := podDefaultFixture("alice", "mount-b", []string{"data"}, false, false)
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv, pvc}, []runtime.Object{aPD, bPD})
	f.failPDDelete = "mount-b"
	f.failPDDeleteErr = apierrors.NewInternalError(errors.New("ambiguous"))
	a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "data", ConfirmName: "data", Reason: "test", TargetUID: string(pvc.UID), TargetResourceVersion: pvc.ResourceVersion, ConfirmPodDefaultCleanup: true}
	p, err := f.svc.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	err = f.svc.Execute(context.Background(), p, func(step string) error { f.checks = append(f.checks, step); return nil })
	if err == nil || errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("partial cleanup was misclassified: %v", err)
	}
	if _, err := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), "mount-a", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("first reviewed PodDefault should have been deleted before the failure: %v", err)
	}
	if _, err := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), "mount-b", metav1.GetOptions{}); err != nil {
		t.Fatalf("failed PodDefault must remain for review: %v", err)
	}
	if _, err := f.client.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "data", metav1.GetOptions{}); err != nil {
		t.Fatalf("PVC must remain after partial PodDefault cleanup: %v", err)
	}
	steps := strings.Join(f.checks, ",")
	if !strings.Contains(steps, "delete-poddefault-1") || !strings.Contains(steps, "delete-poddefault-2") {
		t.Fatalf("per-write checkpoints missing: %v", f.checks)
	}
}

func TestPVDeleteStoppedNotebookBlocksBeforePodDefaultCleanup(t *testing.T) {
	pv := boundRegistrationPV("data-pv", "nfs-static", "/exports/kubeflow/alice/data", "2400Gi", "alice", "data")
	pvc := boundRegistrationPVC("data", "alice", "data-pv", "pvc-data")
	notebook := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubeflow.org/v1", "kind": "Notebook",
		"metadata": map[string]any{"name": "stopped", "namespace": "alice", "uid": "nb-1", "resourceVersion": "1", "annotations": map[string]any{stoppedKey: "2026-01-01T00:00:00Z"}},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{
			"volumes": []any{map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": "data"}}},
		}}},
	}}
	pd := podDefaultFixture("alice", "mount-a", []string{"data"}, false, false)
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv, pvc}, []runtime.Object{notebook, pd})
	a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "data", ConfirmName: "data", Reason: "test", TargetUID: string(pvc.UID), TargetResourceVersion: pvc.ResourceVersion, ConfirmPodDefaultCleanup: true}
	if _, err := f.svc.Prepare(context.Background(), a); err == nil {
		t.Fatal("a stopped Notebook reference did not block deletion before PodDefault cleanup")
	}
	if _, err := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), "mount-a", metav1.GetOptions{}); err != nil {
		t.Fatal("PodDefault was removed despite the Notebook reference")
	}
}

func TestPVDeleteChangedReviewedPodDefaultRefusedBeforeWrite(t *testing.T) {
	pv := boundRegistrationPV("data-pv", "nfs-static", "/exports/kubeflow/alice/data", "2400Gi", "alice", "data")
	pvc := boundRegistrationPVC("data", "alice", "data-pv", "pvc-data")
	pd := podDefaultFixture("alice", "mount-a", []string{"data"}, false, false)
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv, pvc}, []runtime.Object{pd})
	a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "data", ConfirmName: "data", Reason: "test", TargetUID: string(pvc.UID), TargetResourceVersion: pvc.ResourceVersion, ConfirmPodDefaultCleanup: true}
	p, err := f.svc.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a changed object between preview and execution.
	obj, err := f.raw.Tracker().Get(PodDefaults, "alice", "mount-a")
	if err != nil {
		t.Fatal(err)
	}
	changed := obj.(*unstructured.Unstructured).DeepCopy()
	changed.SetResourceVersion("2")
	if err := f.raw.Tracker().Update(PodDefaults, changed, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Execute(context.Background(), p, func(string) error { return nil }); err == nil {
		t.Fatal("changed reviewed PodDefault was accepted")
	}
	if _, err := f.dynamic.Resource(PodDefaults).Namespace("alice").Get(context.Background(), "mount-a", metav1.GetOptions{}); err != nil {
		t.Fatal("changed PodDefault was touched")
	}
	if _, err := f.client.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "data", metav1.GetOptions{}); err != nil {
		t.Fatal("PVC was deleted despite the stale PodDefault")
	}
}

func TestPVCCreateBoundBeeGFSHasNoNFSAnnotations(t *testing.T) {
	fs := corev1.PersistentVolumeFilesystem
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "bee-selected", UID: "pv-bee", ResourceVersion: "1"},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("600Gi")}, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			VolumeMode: &fs, StorageClassName: "beegfs-static", PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: config.BeeGFSDriverName, VolumeHandle: "beegfs://beegfs.example.invalid/home/example.user/volume"}},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable},
	}
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv}, nil)
	a := model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "bee-claim", Template: "beegfs-reg", StorageClass: "beegfs-static", PVName: pv.Name, TargetUID: string(pv.UID), TargetResourceVersion: pv.ResourceVersion}
	p, err := f.svc.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.Execute(context.Background(), p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	claim, err := f.client.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "bee-claim", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(claim.Annotations) != 0 {
		t.Fatalf("BeeGFS PVC received NFS annotations: %v", claim.Annotations)
	}
}

func TestPVCCreateBoundRejectsUncleanExistingNFSPath(t *testing.T) {
	// An existing PV whose path is not canonical must not be approved as a
	// template source, even though the class/capacity would otherwise match.
	pv := registrationPV("unclean", "nfs-static", "/exports/kubeflow/alice/../other", "2400Gi")
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv}, nil)
	a := boundNFSAction("bound", pv)
	if _, err := f.svc.Prepare(context.Background(), a); err == nil {
		t.Fatal("existing PV with an unclean NFS path was approved")
	}
}

func TestRegistrationInventoryProjectionRespectsStaticGate(t *testing.T) {
	cfg := registrationConfig()
	cfg.Features.StaticStorage = false
	f := newRegistrationFixture(t, cfg, nil, nil)
	inv, err := f.svc.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.StorageTemplates) != 0 {
		t.Fatalf("registration templates projected while static storage is disabled: %+v", inv.StorageTemplates)
	}
}

func TestRegistrationInventoryProjection(t *testing.T) {
	pv := registrationPV("selected", "nfs-static", "/exports/kubeflow/alice/workspace", "3000Gi")
	pd := podDefaultFixture("alice", "mount-toast", []string{"other"}, false, false)
	f := newRegistrationFixture(t, registrationConfig(), []runtime.Object{pv}, []runtime.Object{pd})
	inv, err := f.svc.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.StorageTemplates) != 2 {
		t.Fatalf("missing safe registration template projection: %+v", inv.StorageTemplates)
	}
	for _, resource := range inv.Resources {
		if resource.Kind == "PersistentVolume" && resource.Name == "selected" {
			if resource.Details["resourceVersion"] != "1" || resource.Details["registrationTemplate"] != "nfs-reg" {
				t.Fatalf("missing selection metadata: %+v", resource.Details)
			}
			if _, ok := resource.Details["path"]; ok {
				t.Fatal("inventory leaked a backend path")
			}
		}
		if resource.Kind == "PodDefault" && resource.Name == "mount-toast" {
			if resource.Details["claims"] != "other" || resource.Details["shared"] != "false" {
				t.Fatalf("unexpected PodDefault inventory details: %+v", resource.Details)
			}
		}
	}
	for _, resource := range inv.Resources {
		if resource.Kind != "PersistentVolume" {
			continue
		}
		raw, _ := json.Marshal(resource)
		if strings.Contains(string(raw), "/exports/kubeflow") || strings.Contains(string(raw), "nfs.example.invalid") {
			t.Fatalf("inventory PV resource leaked backend details: %s", raw)
		}
	}
}
