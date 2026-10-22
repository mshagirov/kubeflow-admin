package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
)

func boolp(b bool) *bool { return &b }
func i32(n int32) *int32 { return &n }
func i64(n int64) *int64 { return &n }
func owner(uid types.UID, kind, name string) []metav1.OwnerReference {
	return []metav1.OwnerReference{{APIVersion: "kubeflow.org/v1", Kind: kind, Name: name, UID: uid, Controller: boolp(true)}}
}
func fixture(t *testing.T) *unstructured.Unstructured {
	t.Helper()
	b, err := os.ReadFile("testdata/notebook-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	n := &unstructured.Unstructured{}
	if err = json.Unmarshal(b, &n.Object); err != nil {
		t.Fatal(err)
	}
	return n
}

// This small HTTP API fixture deliberately honors dryRun and JSON-Patch tests;
// client-go's dynamic fake drops PatchOptions and cannot test this contract.
type crAPI struct {
	mu      sync.Mutex
	objects map[string]*unstructured.Unstructured
	writes  []string
	patches [][]patchOp
	dryRuns int
	after   func(*unstructured.Unstructured)
	// Optional failure injection for the live PATCH path. The server dry-run
	// is never affected, so a plan can still be prepared against a fixture
	// whose live patch fails.
	patchStatus  int
	patchReason  metav1.StatusReason
	patchMessage string
	patchRaw     string
	// Optional failure injection for the live create path.
	createStatus  int
	createReason  metav1.StatusReason
	createMessage string
	createRaw     string
	// getHook may replace the object served for a read. It proves that a plan
	// is built from one consistent read instead of two.
	getHook func(resource string, n *unstructured.Unstructured) *unstructured.Unstructured
}

func key(resource, ns, n string) string { return resource + "/" + ns + "/" + n }
func (c *crAPI) put(r string, n *unstructured.Unstructured) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.objects[key(r, n.GetNamespace(), n.GetName())] = n.DeepCopy()
}
func (c *crAPI) remove(r, ns, n string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.objects, key(r, ns, n))
}
func (c *crAPI) get(r, ns, n string) *unstructured.Unstructured {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.objects[key(r, ns, n)].DeepCopy()
}
func (c *crAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	defer c.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	ns := ""
	resourceName := ""
	name := ""
	if len(parts) >= 4 {
		if parts[3] == "namespaces" && len(parts) >= 6 {
			ns = parts[4]
			resourceName = parts[5]
			if len(parts) > 6 {
				name = parts[6]
			}
		} else {
			resourceName = parts[3]
			if len(parts) > 4 {
				name = parts[4]
			}
		}
	}
	k := key(resourceName, ns, name)
	status := func(code int, reason metav1.StatusReason, msg string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Code: int32(code), Reason: reason, Message: msg})
	}
	if r.Method == http.MethodGet {
		if name == "" {
			items := []any{}
			for _, obj := range c.objects {
				if (ns == "" || obj.GetNamespace() == ns) && strings.EqualFold(obj.GetKind()+"s", resourceName) {
					items = append(items, obj.Object)
				}
			}
			kind := "NotebookList"
			if resourceName == "profiles" {
				kind = "ProfileList"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "kubeflow.org/v1", "kind": kind, "items": items})
			return
		}
		n := c.objects[k]
		if n == nil {
			status(404, metav1.StatusReasonNotFound, "missing")
			return
		}
		if c.getHook != nil {
			if replaced := c.getHook(resourceName, n); replaced != nil {
				n = replaced
			}
		}
		_ = json.NewEncoder(w).Encode(n.Object)
		return
	}
	var obj *unstructured.Unstructured
	var ops []patchOp
	if r.Method == http.MethodPatch {
		if r.URL.Query().Get("dryRun") != metav1.DryRunAll {
			if c.patchRaw != "" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(c.patchRaw))
				return
			}
			if c.patchStatus != 0 {
				status(c.patchStatus, c.patchReason, c.patchMessage)
				return
			}
		}
		old := c.objects[k]
		if old == nil {
			status(404, metav1.StatusReasonNotFound, "missing")
			return
		}
		b, _ := io.ReadAll(r.Body)
		p, err := jsonpatch.DecodePatch(b)
		if err != nil {
			status(400, metav1.StatusReasonBadRequest, "invalid patch")
			return
		}
		oldBytes, _ := json.Marshal(old.Object)
		patched, err := p.Apply(oldBytes)
		if err != nil {
			status(422, metav1.StatusReasonInvalid, "JSON test operation failed")
			return
		}
		obj = &unstructured.Unstructured{}
		if err = json.Unmarshal(patched, &obj.Object); err != nil {
			status(400, metav1.StatusReasonBadRequest, "invalid object")
			return
		}
		_ = json.Unmarshal(b, &ops)
	} else if r.Method == http.MethodPost {
		if r.URL.Query().Get("dryRun") != metav1.DryRunAll {
			if c.createRaw != "" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(c.createRaw))
				return
			}
			if c.createStatus != 0 {
				status(c.createStatus, c.createReason, c.createMessage)
				return
			}
		}
		obj = &unstructured.Unstructured{}
		if err := json.NewDecoder(r.Body).Decode(&obj.Object); err != nil {
			status(400, metav1.StatusReasonBadRequest, "invalid object")
			return
		}
		name = obj.GetName()
		k = key(resourceName, ns, name)
		if c.objects[k] != nil {
			status(409, metav1.StatusReasonAlreadyExists, "exists")
			return
		}
		obj.SetUID(types.UID("created-" + name))
		obj.SetResourceVersion("1")
	} else {
		status(405, metav1.StatusReasonMethodNotAllowed, "unsupported")
		return
	}
	if r.URL.Query().Get("dryRun") == metav1.DryRunAll {
		c.dryRuns++
		_ = json.NewEncoder(w).Encode(obj.Object)
		return
	}
	rv, _ := strconv.Atoi(obj.GetResourceVersion())
	obj.SetResourceVersion(strconv.Itoa(rv + 1))
	c.objects[k] = obj.DeepCopy()
	c.writes = append(c.writes, r.Method+" "+resourceName+"/"+name)
	c.patches = append(c.patches, ops)
	if c.after != nil {
		c.after(obj.DeepCopy())
	}
	_ = json.NewEncoder(w).Encode(obj.Object)
}
func testService(t *testing.T) (*Service, *crAPI, *fake.Clientset) {
	t.Helper()
	p := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubeflow.org/v1", "kind": "Profile", "metadata": map[string]any{"name": "alice", "uid": "profile-1", "resourceVersion": "1"}, "spec": map[string]any{"owner": map[string]any{"kind": "User", "name": "alice@example.test"}}}}
	api := &crAPI{objects: map[string]*unstructured.Unstructured{key("profiles", "", "alice"): p, key("notebooks", "alice", "research"): fixture(t)}}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	d, err := dynamic.NewForConfig(&rest.Config{Host: server.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "alice", UID: "namespace-1", Annotations: map[string]string{"owner": "alice@example.test"}, OwnerReferences: owner("profile-1", "Profile", "alice")}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}}
	sc := &storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "approved", UID: "class-1"}, Provisioner: "csi.example.vendor", AllowVolumeExpansion: boolp(true)}
	objects := []runtime.Object{ns, sc}
	for _, n := range []string{"home", "scratch"} {
		v := boundPV(n)
		objects = append(objects, v, boundPVC(n))
	}
	typed := fake.NewSimpleClientset(objects...)
	if fd, ok := typed.Discovery().(*fakediscovery.FakeDiscovery); ok {
		// The application requires kubeflow.org/v1 Profiles and Notebooks in
		// production. Expose that group in the fake so a missing
		// kubeflow.org/v1alpha1 PodDefault resource is explicit absence rather
		// than an unverifiable nil discovery result.
		fd.Resources = []*metav1.APIResourceList{{
			GroupVersion: "kubeflow.org/v1",
			APIResources: []metav1.APIResource{
				{Name: "profiles", Namespaced: false, Kind: "Profile"},
				{Name: "notebooks", Namespaced: true, Kind: "Notebook"},
			},
		}}
	}
	if fd, ok := typed.Discovery().(*fakediscovery.FakeDiscovery); ok {
		fd.Resources = append(fd.Resources,
			&metav1.APIResourceList{GroupVersion: "storage.k8s.io/v1", APIResources: []metav1.APIResource{{Name: "storageclasses", Namespaced: false, Kind: "StorageClass"}}},
		)
	}
	// The typed fake does not assign UID/resourceVersion on create. Real API
	// responses always do, and creation identity checks depend on it.
	typed.PrependReactor("create", "persistentvolumes", func(a ktesting.Action) (bool, runtime.Object, error) {
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
		if stored.UID == "" {
			stored.UID = types.UID("fake-pv-" + stored.Name)
		}
		if stored.ResourceVersion == "" {
			stored.ResourceVersion = "1"
		}
		if err := typed.Tracker().Create(pvGVR, stored, ""); err != nil {
			return true, nil, err
		}
		return true, stored, nil
	})
	cfg := config.Config{Features: config.Features{WritesEnabled: true, StaticStorage: true, PVCExpansion: true}, Storage: config.Storage{AllowedStorageClasses: []string{"approved"}, NFSTemplates: []config.NFSTemplate{{Name: "nfs-template", Server: "nfs.example.test", BasePath: "/prepared", StorageClass: "approved", AccessModes: []string{"ReadWriteMany"}, MountOptions: []string{"nfsvers=3"}}}, CSITemplates: []config.CSITemplate{{Name: "csi-template", Driver: "csi.example.vendor", VolumeHandle: "example://mgmt/prepared/operator-approved", StorageClass: "approved", AccessModes: []string{"ReadWriteMany"}}}}, Notebooks: config.Notebooks{ControllerContractConfirmed: true, AllowedImages: []string{"approved:v1", "approved:v2"}}}
	return &Service{Config: cfg, Typed: typed, Dynamic: d, PollInterval: time.Millisecond}, api, typed
}
func boundPVC(n string) *corev1.PersistentVolumeClaim {
	cl := "approved"
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: n, Namespace: "alice", UID: types.UID("pvc-" + n), ResourceVersion: "1"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: n, StorageClassName: &cl, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}}}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}}}
}
func boundPV(n string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: n, UID: types.UID("pv-" + n), ResourceVersion: "1"}, Spec: corev1.PersistentVolumeSpec{StorageClassName: "approved", PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}, ClaimRef: &corev1.ObjectReference{Name: n, Namespace: "alice", UID: types.UID("pvc-" + n)}, PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "csi.example.vendor", VolumeHandle: "existing-" + n}}}, Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound}}
}

// legacyQuotaAction builds the guarded legacy quota action: the observed
// owner identity and the reviewed configuration digest. The legacy kind is
// the same guarded write under its old name.
func legacyQuotaAction(t *testing.T, api *crAPI, name string, quota map[string]string) model.Action {
	t.Helper()
	p := api.get("profiles", "", name)
	if p == nil {
		t.Fatalf("profile %s missing", name)
	}
	owner, _, _ := unstructured.NestedString(p.Object, "spec", "owner", "name")
	return model.Action{Kind: "profile.quota.change", Name: name, Owner: owner, Quota: quota, BaseConfigDigest: profileConfigDigest(p)}
}

func assertNoWrites(t *testing.T, a *crAPI) {
	t.Helper()
	if len(a.writes) != 0 {
		t.Fatalf("unexpected writes: %v", a.writes)
	}
}

func TestPreparePreservesUnknownFieldsAndNoSensitiveEvidence(t *testing.T) {
	s, api, _ := testService(t)
	before := api.get("notebooks", "alice", "research")
	a := model.Action{Kind: "notebook.edit", Namespace: "alice", Name: "research", Container: "research", CPURequest: "1500m", Image: "approved:v2"}
	p, err := s.Prepare(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	assertNoWrites(t, api)
	if api.dryRuns != 1 {
		t.Fatal("missing server dry-run")
	}
	if !same(before.Object, api.get("notebooks", "alice", "research").Object) {
		t.Fatal("preview mutated Notebook")
	}
	for _, sensitive := range []string{"DO_NOT_PERSIST_LITERAL", "env", "futureContainerField", "serviceAccountName", "volumeHandle"} {
		if strings.Contains(string(p.Data), sensitive) {
			t.Fatalf("unsafe Prepared.Data: %s", sensitive)
		}
	}
	w, err := s.build(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(conditional(string(before.GetUID()), before.GetResourceVersion(), w.ops))
	patch, _ := jsonpatch.DecodePatch(b)
	raw, _ := json.Marshal(before.Object)
	edited, err := patch.Apply(raw)
	if err != nil {
		t.Fatal(err)
	}
	after := map[string]any{}
	_ = json.Unmarshal(edited, &after)
	for _, path := range [][]string{{"spec", "futureNotebookField"}, {"spec", "template", "spec", "futurePodField"}, {"spec", "template", "spec", "containers", "0", "env"}, {"spec", "template", "spec", "containers", "0", "command"}, {"spec", "template", "spec", "containers", "0", "futureContainerField"}, {"spec", "template", "spec", "containers", "1"}, {"spec", "template", "spec", "initContainers"}, {"metadata", "annotations"}} {
		old, _ := valueAt(before.Object, path)
		got, _ := valueAt(after, path)
		if !same(old, got) {
			t.Fatalf("changed unrelated field %v", path)
		}
	}
}
func TestScopeAndControllerGate(t *testing.T) {
	for _, ns := range []string{"kube-system", "default", "kubeflow", "not-a-profile"} {
		t.Run(ns, func(t *testing.T) {
			s, api, _ := testService(t)
			_, err := s.Prepare(context.Background(), model.Action{Kind: "notebook.stop", Namespace: ns, Name: "research"})
			if err == nil {
				t.Fatal("allowed out-of-scope write")
			}
			assertNoWrites(t, api)
		})
	}
	s, api, k := testService(t)
	s.Config.Notebooks.ControllerContractConfirmed = false
	if _, err := s.Prepare(context.Background(), model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "research"}); err == nil {
		t.Fatal("ignored controller gate")
	}
	s.Config.Notebooks.ControllerContractConfirmed = true
	ns, _ := k.CoreV1().Namespaces().Get(context.Background(), "alice", metav1.GetOptions{})
	ns.OwnerReferences = nil
	_, _ = k.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{})
	if _, err := s.Prepare(context.Background(), model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "research"}); err == nil {
		t.Fatal("adopted unrelated namespace")
	}
	assertNoWrites(t, api)
}
func TestStalePreviewAndCheckpointFailureNeverWrite(t *testing.T) {
	for _, test := range []string{"uid", "rv", "dependency", "checkpoint"} {
		t.Run(test, func(t *testing.T) {
			s, api, k := testService(t)
			p, err := s.Prepare(context.Background(), model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "research"})
			if err != nil {
				t.Fatal(err)
			}
			switch test {
			case "uid", "rv":
				n := api.get("notebooks", "alice", "research")
				if test == "uid" {
					n.SetUID("replacement")
				} else {
					n.SetResourceVersion("11")
				}
				api.put("notebooks", n)
			case "dependency":
				v, _ := k.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "home", metav1.GetOptions{})
				v.UID = "replacement"
				_, _ = k.CoreV1().PersistentVolumeClaims("alice").Update(context.Background(), v, metav1.UpdateOptions{})
			}
			// stop intentionally needs no PVC read; dependency replacement is namespace.
			if test == "dependency" {
				ns, _ := k.CoreV1().Namespaces().Get(context.Background(), "alice", metav1.GetOptions{})
				ns.UID = "replacement"
				_, _ = k.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{})
			}
			err = s.Execute(context.Background(), p, func(string) error {
				if test == "checkpoint" {
					return errors.New("disk unavailable")
				}
				return nil
			})
			if err == nil {
				t.Fatal("accepted stale plan/failed checkpoint")
			}
			if !errors.Is(err, model.ErrRejectedBeforeMutation) {
				t.Fatalf("known read-only rejection would strand locks: %v", err)
			}
			assertNoWrites(t, api)
		})
	}
}
func TestStartRemovesBothStaleCullingKeys(t *testing.T) {
	n := fixture(t)
	n.SetAnnotations(map[string]string{stoppedKey: "false", activityKey: "old", activityCheckKey: "old", "independent": "retain"})
	ops := stopOps(n, false)
	if len(ops) != 3 {
		t.Fatalf("expected stop + both activity removals: %+v", ops)
	}
	b, _ := json.Marshal(conditional(string(n.GetUID()), n.GetResourceVersion(), ops))
	p, _ := jsonpatch.DecodePatch(b)
	raw, _ := json.Marshal(n.Object)
	b, err := p.Apply(raw)
	if err != nil {
		t.Fatal(err)
	}
	var after unstructured.Unstructured
	if err = json.Unmarshal(b, &after.Object); err != nil {
		t.Fatal(err)
	}
	if !same(after.GetAnnotations(), map[string]string{"independent": "retain"}) {
		t.Fatal(after.GetAnnotations())
	}
	if len(stopOps(&after, false)) != 0 {
		t.Fatal("repeated start changes activity")
	}
	if len(stopOps(n, true)) != 0 {
		t.Fatal("annotation presence must mean stopped regardless of value")
	}
}

func TestStaticNFSPathBoundariesAndMountOptions(t *testing.T) {
	for _, sub := range []string{"..", "../escape", "a/../../escape", "a/../b", "/absolute", ".", "", "a//b", "a\\b"} {
		t.Run(sub, func(t *testing.T) {
			s, api, _ := testService(t)
			_, err := s.build(context.Background(), model.Action{Kind: "storage.create.static", Namespace: "alice", Name: "new", PVName: "new-pv", Template: "nfs-template", Subdirectory: sub, StorageClass: "approved", Size: "10Gi", AccessMode: "ReadWriteMany"})
			if err == nil {
				t.Fatal("unsafe NFS path accepted")
			}
			assertNoWrites(t, api)
		})
	}
	s, _, _ := testService(t)
	w, err := s.build(context.Background(), model.Action{Kind: "storage.create.static", Namespace: "alice", Name: "new", PVName: "new-pv", Template: "nfs-template", Subdirectory: "alice/data", StorageClass: "approved", Size: "10Gi", AccessMode: "ReadWriteMany"})
	if err != nil {
		t.Fatal(err)
	}
	if w.pv.Spec.NFS.Path != "/prepared/alice/data" || !same(w.pv.Spec.MountOptions, []string{"nfsvers=3"}) || w.pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain || w.pv.Spec.ClaimRef.Name != "new" || w.pvc.Spec.VolumeName != "new-pv" {
		t.Fatalf("unsafe static pairing: %+v", w.pv.Spec)
	}
}
func TestReservationRejectsUnsafePV(t *testing.T) {
	for _, change := range []string{"bound", "released", "delete", "reserved", "capacity", "class", "mount-options", "topology"} {
		t.Run(change, func(t *testing.T) {
			s, api, k := testService(t)
			pv := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "available", UID: "available-uid", ResourceVersion: "2"}, Spec: corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, StorageClassName: "approved", MountOptions: []string{"nfsvers=3"}, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}, PersistentVolumeSource: corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: "nfs.example.test", Path: "/prepared/alice/data"}}}, Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable}}
			switch change {
			case "bound":
				pv.Status.Phase = corev1.VolumeBound
			case "released":
				pv.Status.Phase = corev1.VolumeReleased
			case "delete":
				pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
			case "reserved":
				pv.Spec.ClaimRef = &corev1.ObjectReference{Name: "someone-else"}
			case "capacity":
				pv.Spec.Capacity[corev1.ResourceStorage] = resource.MustParse("1Gi")
			case "class":
				pv.Spec.StorageClassName = "other"
			case "mount-options":
				pv.Spec.MountOptions = nil
			case "topology":
				pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{}
			}
			_, _ = k.CoreV1().PersistentVolumes().Create(context.Background(), pv, metav1.CreateOptions{})
			_, err := s.Prepare(context.Background(), model.Action{Kind: "pv.reserve-and-claim", Namespace: "alice", Name: "new", PVName: "available", StorageClass: "approved", Size: "5Gi", AccessMode: "ReadWriteMany"})
			if err == nil {
				t.Fatal("unsafe reservation allowed")
			}
			assertNoWrites(t, api)
		})
	}
}
func TestFixedCSIImportRejectsExistingHandleAndArbitrarySubdirectory(t *testing.T) {
	s, _, k := testService(t)
	a := model.Action{Kind: "storage.create.static", Namespace: "alice", Name: "new", PVName: "new-pv", Template: "csi-template", StorageClass: "approved", Size: "10Gi", AccessMode: "ReadWriteMany"}
	w, err := s.build(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if w.pv.Spec.CSI.VolumeHandle != s.Config.Storage.CSITemplates[0].VolumeHandle {
		t.Fatal("did not use exact approved handle")
	}
	a.Subdirectory = "user-input"
	if _, err = s.build(context.Background(), a); err == nil {
		t.Fatal("accepted free CSI path")
	}
	a.Subdirectory = ""
	existing := w.pv.DeepCopy()
	existing.Name = "already-represented"
	existing.Status.Phase = corev1.VolumeBound
	_, _ = k.CoreV1().PersistentVolumes().Create(context.Background(), existing, metav1.CreateOptions{})
	if _, err = s.build(context.Background(), a); err == nil {
		t.Fatal("reused represented CSI handle")
	}
}

func TestStableQuotaEvidenceButPolicyAndUIDChangesConflict(t *testing.T) {
	s, _, k := testService(t)
	q := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "quota", Namespace: "alice", UID: "quota-1", ResourceVersion: "1"}, Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("10")}}, Status: corev1.ResourceQuotaStatus{Used: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("2")}}}
	_, _ = k.CoreV1().ResourceQuotas("alice").Create(context.Background(), q, metav1.CreateOptions{})
	e1 := s.newEvidence()
	if err := s.scope(context.Background(), "alice", &e1); err != nil {
		t.Fatal(err)
	}
	q.ResourceVersion = "2"
	q.Status.Used[corev1.ResourceRequestsCPU] = resource.MustParse("0")
	_, _ = k.CoreV1().ResourceQuotas("alice").Update(context.Background(), q, metav1.UpdateOptions{})
	e2 := s.newEvidence()
	if err := s.scope(context.Background(), "alice", &e2); err != nil {
		t.Fatal(err)
	}
	if string(e1.bytes()) != string(e2.bytes()) {
		t.Fatal("controller usage churn invalidated policy evidence")
	}
	q.Spec.Hard[corev1.ResourceRequestsCPU] = resource.MustParse("9")
	_, _ = k.CoreV1().ResourceQuotas("alice").Update(context.Background(), q, metav1.UpdateOptions{})
	if err := s.checkEvidence(context.Background(), "alice", e1); !errors.Is(err, ErrConflict) {
		t.Fatalf("policy change not caught: %v", err)
	}
}

func installWorkload(t *testing.T, k *fake.Clientset, n *unstructured.Unstructured, running bool) (*appsv1.StatefulSet, *corev1.Pod) {
	t.Helper()
	spec, _, _ := unstructured.NestedMap(n.Object, "spec", "template", "spec")
	var ps corev1.PodSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(spec, &ps); err != nil {
		t.Fatal(err)
	}
	rep := int32(0)
	status := appsv1.StatefulSetStatus{ObservedGeneration: 1}
	if running {
		rep = 1
		status.Replicas = 1
		status.ReadyReplicas = 1
		status.CurrentReplicas = 1
		status.UpdatedReplicas = 1
		status.CurrentRevision = "revision-new"
		status.UpdateRevision = "revision-new"
	}
	st := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "research", Namespace: "alice", UID: "sts-1", Generation: 1, OwnerReferences: owner(n.GetUID(), "Notebook", n.GetName())}, Spec: appsv1.StatefulSetSpec{Replicas: &rep, Template: corev1.PodTemplateSpec{Spec: ps}}, Status: status}
	if _, err := k.AppsV1().StatefulSets("alice").Create(context.Background(), st, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		_, _ = k.AppsV1().StatefulSets("alice").Update(context.Background(), st, metav1.UpdateOptions{})
	} else if err != nil {
		t.Fatal(err)
	}
	if !running {
		return st, nil
	}
	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "research-0", Namespace: "alice", UID: "pod-new", Labels: map[string]string{appsv1.StatefulSetRevisionLabel: "revision-new"}, OwnerReferences: owner(st.UID, "StatefulSet", st.Name)}, Spec: ps, Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}, ContainerStatuses: []corev1.ContainerStatus{{Name: "research", Ready: true}, {Name: "sidecar", Ready: true}}}}
	if _, err := k.CoreV1().Pods("alice").Create(context.Background(), p, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
		_, _ = k.CoreV1().Pods("alice").Update(context.Background(), p, metav1.UpdateOptions{})
	} else if err != nil {
		t.Fatal(err)
	}
	return st, p
}
func TestConvergenceRejectsStaleReadyPodAndWrongOwnership(t *testing.T) {
	s, api, k := testService(t)
	n := api.get("notebooks", "alice", "research")
	st, p := installWorkload(t, k, n, true)
	ops := []patchOp{{"add", "/spec/template/spec/containers/0/image", "approved:v2"}}
	st.Spec.Template.Spec.Containers[0].Image = "approved:v2"
	_, _ = k.AppsV1().StatefulSets("alice").Update(context.Background(), st, metav1.UpdateOptions{})
	ready, err := s.notebookObserved(context.Background(), n, false, ops)
	if err != nil || ready {
		t.Fatalf("accepted ready old Pod: %v %v", ready, err)
	}
	p.Spec.Containers[0].Image = "approved:v2"
	p.Labels[appsv1.StatefulSetRevisionLabel] = "revision-old"
	_, _ = k.CoreV1().Pods("alice").Update(context.Background(), p, metav1.UpdateOptions{})
	ready, _ = s.notebookObserved(context.Background(), n, false, ops)
	if ready {
		t.Fatal("accepted old revision")
	}
	p.Labels[appsv1.StatefulSetRevisionLabel] = "revision-new"
	_, _ = k.CoreV1().Pods("alice").Update(context.Background(), p, metav1.UpdateOptions{})
	ready, err = s.notebookObserved(context.Background(), n, false, ops)
	if err != nil || !ready {
		t.Fatalf("did not accept current owned Pod: %v %v", ready, err)
	}
	p.OwnerReferences = owner("different-sts", "StatefulSet", "research")
	_, _ = k.CoreV1().Pods("alice").Update(context.Background(), p, metav1.UpdateOptions{})
	ready, _ = s.notebookObserved(context.Background(), n, false, ops)
	if ready {
		t.Fatal("matched Pod by name instead of owner UID")
	}
}

func TestEditStopsBeforeSpecAndCheckpointBeforeEveryWrite(t *testing.T) {
	s, api, k := testService(t)
	n := api.get("notebooks", "alice", "research")
	installWorkload(t, k, n, true)
	steps := []string{}
	api.after = func(n *unstructured.Unstructured) {
		_, stop := n.GetAnnotations()[stoppedKey]
		if stop {
			_ = k.CoreV1().Pods("alice").Delete(context.Background(), "research-0", metav1.DeleteOptions{})
		}
		installWorkload(t, k, n, !stop)
	}
	p, err := s.Prepare(context.Background(), model.Action{Kind: "notebook.edit", Namespace: "alice", Name: "research", Container: "research", Image: "approved:v2", Restart: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = s.Execute(ctx, p, func(step string) error {
		if len(api.writes) != len(steps) {
			t.Fatal("write occurred before checkpoint")
		}
		steps = append(steps, step)
		return nil
	})
	if err != nil {
		cur := api.get("notebooks", "alice", "research")
		sets, pods, _ := s.notebookChildren(context.Background(), cur)
		ok, e := s.notebookObserved(context.Background(), cur, true, []patchOp{{"add", "/spec/template/spec/containers/0/image", "approved:v2"}})
		t.Fatalf("%v; writes=%v observed=%v/%v sets=%d pods=%d", err, api.writes, ok, e, len(sets), len(pods))
	}
	if !same(steps, []string{"stop-notebook-before-edit", "edit-stopped-notebook", "restart-edited-notebook"}) || len(api.writes) != 3 {
		t.Fatalf("wrong sequence: %v / %v", steps, api.writes)
	}
	for _, ops := range api.patches {
		if len(ops) < 2 || ops[0].Path != "/metadata/uid" || ops[1].Path != "/metadata/resourceVersion" {
			t.Fatal("missing UID+RV tests")
		}
	}
	if api.patches[0][2].Path != "/metadata/annotations/"+pointer(stoppedKey) {
		t.Fatal("spec edited before stop")
	}
	after := api.get("notebooks", "alice", "research")
	env, _, _ := unstructured.NestedSlice(after.Object, "spec", "template", "spec", "containers")
	if env[0].(map[string]any)["futureContainerField"] == nil {
		t.Fatal("lost unknown fields")
	}
}
func TestStoppedNotebookDoesNotRestart(t *testing.T) {
	s, api, k := testService(t)
	n := api.get("notebooks", "alice", "research")
	a := n.GetAnnotations()
	a[stoppedKey] = "old"
	n.SetAnnotations(a)
	api.put("notebooks", n)
	installWorkload(t, k, n, false)
	api.after = func(n *unstructured.Unstructured) { installWorkload(t, k, n, false) }
	p, err := s.Prepare(context.Background(), model.Action{Kind: "notebook.edit", Namespace: "alice", Name: "research", Container: "research", CPURequest: "1500m", Restart: true})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = s.Execute(ctx, p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(api.writes) != 1 {
		t.Fatalf("unexpected restart: %v", api.writes)
	}
	if _, ok := api.get("notebooks", "alice", "research").GetAnnotations()[stoppedKey]; !ok {
		t.Fatal("restarted originally stopped Notebook")
	}
}
func TestMountPreservesSidecarReferencesAndProtectsHome(t *testing.T) {
	s, api, _ := testService(t)
	n := api.get("notebooks", "alice", "research")
	spec, _, _ := unstructured.NestedMap(n.Object, "spec", "template", "spec")
	mounts := []model.Mount{{Name: "home", Claim: "home", Path: "/home/jovyan"}}
	ms, vs, err := s.mountChanges(model.Action{Mounts: &mounts}, n, spec, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || len(vs) != 3 {
		t.Fatal("removed sidecar/init/unrelated volume")
	}
	mounts = []model.Mount{}
	if _, _, err = s.mountChanges(model.Action{Mounts: &mounts}, n, spec, 0); err == nil {
		t.Fatal("removed protected home")
	}
}

func TestExpansionRejectsShrinkWrongBindingAndFeatureGate(t *testing.T) {
	for _, which := range []string{"shrink", "binding", "feature"} {
		t.Run(which, func(t *testing.T) {
			s, _, k := testService(t)
			a := model.Action{Kind: "pvc.expand", Namespace: "alice", Name: "home", Size: "20Gi"}
			switch which {
			case "shrink":
				a.Size = "1Gi"
			case "feature":
				s.Config.Features.PVCExpansion = false
			case "binding":
				pv, _ := k.CoreV1().PersistentVolumes().Get(context.Background(), "home", metav1.GetOptions{})
				pv.Spec.ClaimRef.UID = "other"
				_, _ = k.CoreV1().PersistentVolumes().Update(context.Background(), pv, metav1.UpdateOptions{})
			}
			if _, err := s.Prepare(context.Background(), a); err == nil {
				t.Fatal("unsafe expansion accepted")
			}
		})
	}
}

func TestGPUFailsClosedAndPreservesIndependentScheduling(t *testing.T) {
	s, api, k := testService(t)
	s.Config.Notebooks.GPUPresets = []config.GPUPreset{{Name: "gpu", ResourceKey: "nvidia.com/gpu", NodeSelector: map[string]string{"gpu.example/type": "a100"}}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "gpu-node", Labels: map[string]string{"gpu.example/type": "a100", "independent.example/pool": "research"}}, Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}}, Allocatable: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")}}}
	_, _ = k.CoreV1().Nodes().Create(context.Background(), node, metav1.CreateOptions{})
	a := model.Action{Kind: "notebook.edit", Namespace: "alice", Name: "research", Container: "research", GPUPreset: "gpu", GPUCount: i64(1)}
	w, err := s.build(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	selectors := map[string]any{}
	for _, op := range w.ops {
		if op.Path == "/spec/template/spec/nodeSelector" {
			selectors = op.Value.(map[string]any)
		}
	}
	if selectors["independent.example/pool"] != "research" || selectors["gpu.example/type"] != "a100" {
		t.Fatal("lost independent selector")
	}
	n := api.get("notebooks", "alice", "research")
	_ = unstructured.SetNestedField(n.Object, map[string]any{"nodeAffinity": map[string]any{}}, "spec", "template", "spec", "affinity")
	api.put("notebooks", n)
	if _, err = s.Prepare(context.Background(), a); err == nil {
		t.Fatal("guessed affinity semantics")
	}
	a.GPUCount = i64(-1)
	if _, err = s.Prepare(context.Background(), a); err == nil {
		t.Fatal("accepted negative GPU")
	}
}

func TestAPIConditionalFailuresAreConflicts(t *testing.T) {
	for _, err := range []error{apierrors.NewConflict(Notebooks.GroupResource(), "x", errors.New("changed")), apierrors.NewBadRequest("test operation failed"), &apierrors.StatusError{ErrStatus: metav1.Status{Reason: metav1.StatusReasonInvalid, Message: "JSON patch test failed", Code: 422}}} {
		if !errors.Is(safe(err), ErrConflict) {
			t.Fatalf("not normalized: %v", err)
		}
	}
}
func TestInventorySanitizesAndPropagatesFailures(t *testing.T) {
	s, _, k := testService(t)
	inv, err := s.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(inv)
	if strings.Contains(string(b), "DO_NOT_PERSIST_LITERAL") || strings.Contains(string(b), "existing-home") {
		t.Fatal("inventory exposed unapproved fields")
	}
	k.PrependReactor("list", "events", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("sensitive arbitrary API error")
	})
	if _, err = s.Inventory(context.Background()); err == nil || strings.Contains(err.Error(), "sensitive") {
		t.Fatal("did not safely propagate inventory error")
	}
}

// --- quota change preservation ---

func TestQuotaChangePreservesExistingUnknownKeys(t *testing.T) {
	s, api, _ := testService(t)
	p := api.get("profiles", "", "alice")
	if err := unstructured.SetNestedMap(p.Object, map[string]any{"requests.cpu": "2", "requests.ephemeral-storage": "5Gi"}, "spec", "resourceQuotaSpec", "hard"); err != nil {
		t.Fatal(err)
	}
	api.put("profiles", p)
	// 5120Mi is the same quantity as 5Gi for the operator-managed key; a
	// round-tripped unchanged value must be accepted and preserved verbatim.
	w, err := s.build(context.Background(), legacyQuotaAction(t, api, "alice", map[string]string{"requests.cpu": "4", "requests.ephemeral-storage": "5120Mi"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.ops) != 1 || w.ops[0].Op != "add" {
		t.Fatalf("unexpected ops: %+v", w.ops)
	}
	hard, ok := w.ops[0].Value.(map[string]any)
	if !ok {
		t.Fatalf("unexpected hard value: %#v", w.ops[0].Value)
	}
	if hard["requests.cpu"] != "4" {
		t.Fatalf("supported key not updated: %v", hard)
	}
	if hard["requests.ephemeral-storage"] != "5Gi" {
		t.Fatalf("unknown key not preserved: %v", hard)
	}
}

func TestQuotaChangeRejectsUnsafeEdits(t *testing.T) {
	cases := []struct {
		name      string
		existing  map[string]any
		requested map[string]string
		want      string
	}{
		{"introduce-unsupported", map[string]any{"requests.cpu": "2"}, map[string]string{"requests.cpu": "2", "example.com/gpu": "1"}, "unsupported quota resource"},
		{"change-unsupported", map[string]any{"requests.cpu": "2", "requests.ephemeral-storage": "5Gi"}, map[string]string{"requests.cpu": "2", "requests.ephemeral-storage": "6Gi"}, "changing an unsupported quota key is unsupported"},
		{"remove-all", map[string]any{"requests.cpu": "2"}, map[string]string{}, "quota removal is unsupported"},
		{"remove-supported", map[string]any{"requests.cpu": "2", "requests.memory": "1Gi"}, map[string]string{"requests.cpu": "3"}, "quota key removal is unsupported by this adapter"},
		{"bad-quantity", map[string]any{"requests.cpu": "2"}, map[string]string{"requests.cpu": "-1"}, "a quota value must be a non-negative Kubernetes quantity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, api, _ := testService(t)
			p := api.get("profiles", "", "alice")
			if err := unstructured.SetNestedMap(p.Object, tc.existing, "spec", "resourceQuotaSpec", "hard"); err != nil {
				t.Fatal(err)
			}
			api.put("profiles", p)
			_, err := s.build(context.Background(), legacyQuotaAction(t, api, "alice", tc.requested))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// --- root NFS base path ---

func TestRootNFSBasePathRejectedAtRuntime(t *testing.T) {
	s, api, _ := testService(t)
	s.Config.Storage.NFSTemplates[0].BasePath = "/"
	_, err := s.build(context.Background(), model.Action{Kind: "storage.create.static", Namespace: "alice", Name: "new", PVName: "new-pv", Template: "nfs-template", Subdirectory: "alice", StorageClass: "approved", Size: "10Gi", AccessMode: "ReadWriteMany"})
	if err == nil || !strings.Contains(err.Error(), "invalid approved NFS template") {
		t.Fatalf("root NFS base path accepted: %v", err)
	}
	assertNoWrites(t, api)
}

// --- defensive mount parsing ---

func mountEdit() model.Action {
	return model.Action{Kind: "notebook.edit", Namespace: "alice", Name: "research", Container: "research", Mounts: &[]model.Mount{{Name: "home", Claim: "home", Path: "/home/jovyan"}}}
}

func setFixtureVolume(t *testing.T, n *unstructured.Unstructured, i int, key string, value any) {
	t.Helper()
	vols, _, err := unstructured.NestedSlice(n.Object, "spec", "template", "spec", "volumes")
	if err != nil || i >= len(vols) {
		t.Fatalf("fixture volume %d: %v", i, err)
	}
	vols[i].(map[string]any)[key] = value
	if err = unstructured.SetNestedSlice(n.Object, vols, "spec", "template", "spec", "volumes"); err != nil {
		t.Fatal(err)
	}
}

func setFixtureMount(t *testing.T, n *unstructured.Unstructured, ci, mi int, value any) {
	t.Helper()
	cs, _, err := unstructured.NestedSlice(n.Object, "spec", "template", "spec", "containers")
	if err != nil || ci >= len(cs) {
		t.Fatalf("fixture container %d: %v", ci, err)
	}
	mounts, ok := cs[ci].(map[string]any)["volumeMounts"].([]any)
	if !ok || mi >= len(mounts) {
		t.Fatalf("fixture mount %d/%d", ci, mi)
	}
	mounts[mi] = value
	cs[ci].(map[string]any)["volumeMounts"] = mounts
	if err = unstructured.SetNestedSlice(n.Object, cs, "spec", "template", "spec", "containers"); err != nil {
		t.Fatal(err)
	}
}

func TestMountEditRejectsMalformedShapesWithoutPanic(t *testing.T) {
	s, api, _ := testService(t)
	ctx := context.Background()
	if _, err := s.build(ctx, mountEdit()); err != nil {
		t.Fatalf("valid mount edit failed: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(t *testing.T, n *unstructured.Unstructured)
	}{
		{"pvc-not-map", func(t *testing.T, n *unstructured.Unstructured) {
			setFixtureVolume(t, n, 0, "persistentVolumeClaim", "not-a-map")
		}},
		{"volume-name-not-string", func(t *testing.T, n *unstructured.Unstructured) {
			setFixtureVolume(t, n, 0, "name", int64(3))
		}},
		{"mount-not-map", func(t *testing.T, n *unstructured.Unstructured) {
			setFixtureMount(t, n, 0, 0, "not-a-map")
		}},
		{"containers-not-list", func(t *testing.T, n *unstructured.Unstructured) {
			if err := unstructured.SetNestedField(n.Object, "not-a-list", "spec", "template", "spec", "containers"); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := fixture(t)
			tc.mutate(t, n)
			api.put("notebooks", n)
			if _, err := s.build(ctx, mountEdit()); err == nil {
				t.Fatal("accepted malformed mount object")
			}
			assertNoWrites(t, api)
		})
	}
}

func TestExecuteRejectsMalformedMountAfterValidPlan(t *testing.T) {
	s, api, _ := testService(t)
	ctx := context.Background()
	p, err := s.Prepare(ctx, mountEdit())
	if err != nil {
		t.Fatal(err)
	}
	n := api.get("notebooks", "alice", "research")
	setFixtureVolume(t, n, 0, "persistentVolumeClaim", "not-a-map")
	api.put("notebooks", n)
	if err = s.Execute(ctx, p, func(string) error { return nil }); err == nil {
		t.Fatal("executed against a malformed live object")
	}
	assertNoWrites(t, api)
}

func TestProtectedMountSurvivesMalformedSibling(t *testing.T) {
	s, api, _ := testService(t)
	n := fixture(t)
	setFixtureVolume(t, n, 1, "persistentVolumeClaim", "not-a-map")
	api.put("notebooks", n)
	spec, _, _ := unstructured.NestedMap(n.Object, "spec", "template", "spec")
	empty := []model.Mount{}
	if _, _, err := s.mountChanges(model.Action{Mounts: &empty}, n, spec, 0); err == nil || !strings.Contains(err.Error(), "protected workspace mount cannot be removed") {
		t.Fatalf("protected mount not preserved: %v", err)
	}
}

// --- deterministic storage-controller fixtures ---

var (
	pvGVR  = corev1.SchemeGroupVersion.WithResource("persistentvolumes")
	pvcGVR = corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims")
	nsGVR  = corev1.SchemeGroupVersion.WithResource("namespaces")
)

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// dryRunReactor makes the typed fake honor server-side dry-run, which client-go's
// ObjectTracker otherwise persists.
func dryRunReactor(k *fake.Clientset) ktesting.ReactionFunc {
	return func(a ktesting.Action) (bool, runtime.Object, error) {
		switch act := a.(type) {
		case ktesting.CreateActionImpl:
			if len(act.GetCreateOptions().DryRun) > 0 {
				return true, act.GetObject(), nil
			}
		case ktesting.UpdateActionImpl:
			if len(act.GetUpdateOptions().DryRun) > 0 {
				return true, act.GetObject(), nil
			}
		case ktesting.PatchActionImpl:
			if len(act.GetPatchOptions().DryRun) > 0 {
				obj, err := k.Tracker().Get(act.GetResource(), act.GetNamespace(), act.GetName())
				return true, obj, err
			}
		}
		return false, nil, nil
	}
}

func bindFakePV(k *fake.Clientset, vol string, pvc *corev1.PersistentVolumeClaim) error {
	existing, err := k.Tracker().Get(pvGVR, "", vol)
	if apierrors.IsNotFound(err) {
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: vol, UID: types.UID("fake-pv-" + vol), ResourceVersion: "1"},
			Spec: corev1.PersistentVolumeSpec{
				StorageClassName:              derefString(pvc.Spec.StorageClassName),
				AccessModes:                   append([]corev1.PersistentVolumeAccessMode(nil), pvc.Spec.AccessModes...),
				VolumeMode:                    pvc.Spec.VolumeMode,
				Capacity:                      pvc.Spec.Resources.Requests.DeepCopy(),
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
				PersistentVolumeSource:        corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "csi.example.vendor", VolumeHandle: "provisioned-" + vol}},
				ClaimRef:                      &corev1.ObjectReference{Name: pvc.Name, Namespace: pvc.Namespace, UID: pvc.UID},
			},
			Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeBound},
		}
		return k.Tracker().Create(pvGVR, pv, "")
	}
	if err != nil {
		return err
	}
	pv, ok := existing.(*corev1.PersistentVolume)
	if !ok {
		return errors.New("unexpected PV type")
	}
	pv = pv.DeepCopy()
	if pv.Spec.ClaimRef == nil {
		pv.Spec.ClaimRef = &corev1.ObjectReference{}
	}
	pv.Spec.ClaimRef.Name = pvc.Name
	pv.Spec.ClaimRef.Namespace = pvc.Namespace
	pv.Spec.ClaimRef.UID = pvc.UID
	pv.Status.Phase = corev1.VolumeBound
	return k.Tracker().Update(pvGVR, pv, "")
}

// fakeController is a deterministic stand-in for the cluster's dry-run behavior,
// the storage controller and a CSI driver completing a resize. It provisions no
// real storage: an Immediate claim is recorded Bound with a matching PV, a
// WaitForFirstConsumer claim stays Pending. Both are creation outcomes only and
// must never be read as a provisioning guarantee.
func fakeController(k *fake.Clientset, wffc bool) {
	k.PrependReactor("create", "persistentvolumeclaims", func(a ktesting.Action) (bool, runtime.Object, error) {
		ca, ok := a.(ktesting.CreateActionImpl)
		if !ok {
			return false, nil, nil
		}
		pvc, ok := ca.GetObject().(*corev1.PersistentVolumeClaim)
		if !ok {
			return false, nil, nil
		}
		pvc = pvc.DeepCopy()
		if len(ca.GetCreateOptions().DryRun) > 0 {
			return true, pvc, nil
		}
		pvc.UID = types.UID("fake-" + pvc.Namespace + "-" + pvc.Name)
		pvc.ResourceVersion = "1"
		if wffc && pvc.Spec.VolumeName == "" {
			pvc.Status.Phase = corev1.ClaimPending
			if err := k.Tracker().Create(pvcGVR, pvc, pvc.Namespace); err != nil {
				return true, nil, err
			}
			return true, pvc, nil
		}
		if pvc.Spec.VolumeName == "" {
			pvc.Spec.VolumeName = "provisioned-" + pvc.Name
		}
		pvc.Status.Phase = corev1.ClaimBound
		pvc.Status.Capacity = pvc.Spec.Resources.Requests.DeepCopy()
		if err := bindFakePV(k, pvc.Spec.VolumeName, pvc); err != nil {
			return true, nil, err
		}
		if err := k.Tracker().Create(pvcGVR, pvc, pvc.Namespace); err != nil {
			return true, nil, err
		}
		return true, pvc, nil
	})
	k.PrependReactor("patch", "persistentvolumeclaims", func(a ktesting.Action) (bool, runtime.Object, error) {
		pa, ok := a.(ktesting.PatchActionImpl)
		if !ok || pa.GetPatchType() != types.JSONPatchType {
			return false, nil, nil
		}
		obj, err := k.Tracker().Get(pvcGVR, pa.GetNamespace(), pa.GetName())
		if err != nil {
			return true, nil, err
		}
		if len(pa.GetPatchOptions().DryRun) > 0 {
			return true, obj, nil
		}
		old, err := json.Marshal(obj)
		if err != nil {
			return true, nil, err
		}
		patch, err := jsonpatch.DecodePatch(pa.GetPatch())
		if err != nil {
			return true, nil, err
		}
		modified, err := patch.Apply(old)
		if err != nil {
			return true, nil, err
		}
		out := &corev1.PersistentVolumeClaim{}
		if err := json.Unmarshal(modified, out); err != nil {
			return true, nil, err
		}
		if req := out.Spec.Resources.Requests.Storage(); req != nil {
			if out.Status.Capacity == nil {
				out.Status.Capacity = corev1.ResourceList{}
			}
			out.Status.Capacity[corev1.ResourceStorage] = req.DeepCopy()
		}
		if err := k.Tracker().Update(pvcGVR, out, out.Namespace); err != nil {
			return true, nil, err
		}
		return true, out, nil
	})
	k.PrependReactor("*", "persistentvolumes", dryRunReactor(k))
	k.PrependReactor("*", "persistentvolumeclaims", dryRunReactor(k))
}

// --- execution coverage ---

func TestExecuteProfileCreateConverges(t *testing.T) {
	s, api, k := testService(t)
	ctx := context.Background()
	action := model.Action{Kind: "profile.create", Name: "bob", Owner: "bob@example.test", Quota: map[string]string{"requests.cpu": "4", "requests.memory": "8Gi"}}
	api.after = func(p *unstructured.Unstructured) {
		if p.GetKind() != "Profile" {
			return
		}
		uid := p.GetUID()
		ownerName, _, _ := unstructured.NestedString(p.Object, "spec", "owner", "name")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.GetName(), UID: types.UID("ns-" + p.GetName()), Annotations: map[string]string{"owner": ownerName}, OwnerReferences: owner(uid, "Profile", p.GetName())}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}}
		if _, err := k.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
			t.Errorf("namespace fixture: %v", err)
		}
		rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "kubeflow-admin", Namespace: p.GetName(), UID: types.UID("rb-" + p.GetName()), OwnerReferences: owner(uid, "Profile", p.GetName())}, Subjects: []rbacv1.Subject{{Kind: "User", Name: ownerName, APIGroup: "rbac.authorization.k8s.io"}}, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "kubeflow-admin", APIGroup: "rbac.authorization.k8s.io"}}
		if _, err := k.RbacV1().RoleBindings(p.GetName()).Create(ctx, rb, metav1.CreateOptions{}); err != nil {
			t.Errorf("rolebinding fixture: %v", err)
		}
		rl := corev1.ResourceList{}
		for key, v := range action.Quota {
			q, err := quantity(v)
			if err != nil {
				t.Errorf("bad quota fixture: %v", err)
			}
			rl[corev1.ResourceName(key)] = q
		}
		q := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "kf-resource-quota", Namespace: p.GetName(), UID: types.UID("q-" + p.GetName()), OwnerReferences: owner(uid, "Profile", p.GetName())}, Spec: corev1.ResourceQuotaSpec{Hard: rl}, Status: corev1.ResourceQuotaStatus{Hard: rl}}
		if _, err := k.CoreV1().ResourceQuotas(p.GetName()).Create(ctx, q, metav1.CreateOptions{}); err != nil {
			t.Errorf("quota fixture: %v", err)
		}
	}
	p, err := s.Prepare(ctx, action)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Execute(ctx, p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if api.get("profiles", "", "bob") == nil {
		t.Fatal("profile not created")
	}
}

func TestExecuteProfileQuotaChangePreservesUnknownKeys(t *testing.T) {
	s, api, k := testService(t)
	ctx := context.Background()
	p := api.get("profiles", "", "alice")
	if err := unstructured.SetNestedMap(p.Object, map[string]any{"requests.cpu": "2", "requests.ephemeral-storage": "5Gi"}, "spec", "resourceQuotaSpec", "hard"); err != nil {
		t.Fatal(err)
	}
	api.put("profiles", p)
	api.after = func(obj *unstructured.Unstructured) {
		if obj.GetKind() != "Profile" {
			return
		}
		hard, _, _ := unstructured.NestedMap(obj.Object, "spec", "resourceQuotaSpec", "hard")
		rl := corev1.ResourceList{}
		for key, v := range hard {
			q, err := resource.ParseQuantity(fmt.Sprint(v))
			if err != nil {
				continue
			}
			rl[corev1.ResourceName(key)] = q
		}
		q := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "kf-resource-quota", Namespace: obj.GetName(), UID: "quota-1", OwnerReferences: owner(obj.GetUID(), "Profile", obj.GetName())}, Spec: corev1.ResourceQuotaSpec{Hard: rl}, Status: corev1.ResourceQuotaStatus{Hard: rl}}
		if _, err := k.CoreV1().ResourceQuotas(obj.GetName()).Create(ctx, q, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
			_, _ = k.CoreV1().ResourceQuotas(obj.GetName()).Update(ctx, q, metav1.UpdateOptions{})
		}
	}
	prepared, err := s.Prepare(ctx, legacyQuotaAction(t, api, "alice", map[string]string{"requests.cpu": "4"}))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Execute(ctx, prepared, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cur := api.get("profiles", "", "alice")
	hard, _, _ := unstructured.NestedMap(cur.Object, "spec", "resourceQuotaSpec", "hard")
	if hard["requests.ephemeral-storage"] != "5Gi" {
		t.Fatalf("unknown key lost: %v", hard)
	}
	if hard["requests.cpu"] != "4" {
		t.Fatalf("cpu not updated: %v", hard)
	}
}

func TestExecuteDynamicPVCCreationOutcomes(t *testing.T) {
	cases := []struct {
		name      string
		binding   storagev1.VolumeBindingMode
		wffc      bool
		wantPhase corev1.PersistentVolumeClaimPhase
	}{
		{"immediate-binds", storagev1.VolumeBindingImmediate, false, corev1.ClaimBound},
		{"wait-for-consumer-stays-pending", storagev1.VolumeBindingWaitForFirstConsumer, true, corev1.ClaimPending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, k := testService(t)
			sc, err := k.StorageV1().StorageClasses().Get(context.Background(), "approved", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			sc.VolumeBindingMode = &tc.binding
			if _, err = k.StorageV1().StorageClasses().Update(context.Background(), sc, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			fakeController(k, tc.wffc)
			action := model.Action{Kind: "pvc.create.dynamic", Namespace: "alice", Name: "dyn", StorageClass: "approved", Size: "5Gi", AccessMode: "ReadWriteMany"}
			p, err := s.Prepare(context.Background(), action)
			if err != nil {
				t.Fatal(err)
			}
			if tc.wffc {
				if !strings.Contains(strings.Join(p.Warnings, " "), "not evidence that storage will be provisioned") {
					t.Fatalf("missing provisioning caveat: %v", p.Warnings)
				}
			}
			if err = s.Execute(context.Background(), p, func(string) error { return nil }); err != nil {
				t.Fatal(err)
			}
			cur, err := k.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "dyn", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if cur.Status.Phase != tc.wantPhase {
				t.Fatalf("phase = %v, want %v", cur.Status.Phase, tc.wantPhase)
			}
		})
	}
}

func TestExecuteStaticCreateAndReserve(t *testing.T) {
	t.Run("static", func(t *testing.T) {
		s, _, k := testService(t)
		fakeController(k, false)
		action := model.Action{Kind: "storage.create.static", Namespace: "alice", Name: "new", PVName: "new-pv", Template: "nfs-template", Subdirectory: "alice/data", StorageClass: "approved", Size: "10Gi", AccessMode: "ReadWriteMany"}
		p, err := s.Prepare(context.Background(), action)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Execute(context.Background(), p, func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
		pv, err := k.CoreV1().PersistentVolumes().Get(context.Background(), "new-pv", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain || pv.Status.Phase != corev1.VolumeBound {
			t.Fatalf("unexpected PV: %+v", pv)
		}
	})
	t.Run("reserve", func(t *testing.T) {
		s, _, k := testService(t)
		fakeController(k, false)
		available := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "available-pv", UID: "available-uid", ResourceVersion: "2"}, Spec: corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, StorageClassName: "approved", MountOptions: []string{"nfsvers=3"}, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}, PersistentVolumeSource: corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: "nfs.example.test", Path: "/prepared/alice/data"}}}, Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable}}
		if _, err := k.CoreV1().PersistentVolumes().Create(context.Background(), available, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		action := model.Action{Kind: "pv.reserve-and-claim", Namespace: "alice", Name: "reserved", PVName: "available-pv", StorageClass: "approved", Size: "5Gi", AccessMode: "ReadWriteMany"}
		p, err := s.Prepare(context.Background(), action)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Execute(context.Background(), p, func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
		cur, err := k.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "reserved", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if cur.Status.Phase != corev1.ClaimBound || cur.Spec.VolumeName != "available-pv" {
			t.Fatalf("reservation did not bind: %+v", cur.Status)
		}
	})
}

func TestExecutePVCExpansionConverges(t *testing.T) {
	s, _, k := testService(t)
	fakeController(k, false)
	p, err := s.Prepare(context.Background(), model.Action{Kind: "pvc.expand", Namespace: "alice", Name: "home", Size: "20Gi"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Execute(context.Background(), p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cur, err := k.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "home", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cur.Spec.Resources.Requests.Storage().Cmp(resource.MustParse("20Gi")) != 0 || cur.Status.Capacity.Storage().Cmp(resource.MustParse("20Gi")) != 0 {
		t.Fatalf("expansion not converged: req=%v status=%v", cur.Spec.Resources.Requests.Storage(), cur.Status.Capacity.Storage())
	}
}

func staticCreateAction() model.Action {
	return model.Action{Kind: "storage.create.static", Namespace: "alice", Name: "new", PVName: "new-pv", Template: "nfs-template", Subdirectory: "alice/data", StorageClass: "approved", Size: "10Gi", AccessMode: "ReadWriteMany"}
}

func csiStaticCreateAction() model.Action {
	return model.Action{Kind: "storage.create.static", Namespace: "alice", Name: "csi", PVName: "csi-pv", Template: "csi-template", StorageClass: "approved", Size: "10Gi", AccessMode: "ReadWriteMany"}
}

func TestExecuteStorageRaceDetection(t *testing.T) {
	t.Run("overlap-injected-at-checkpoint", func(t *testing.T) {
		s, _, k := testService(t)
		fakeController(k, false)
		p, err := s.Prepare(context.Background(), staticCreateAction())
		if err != nil {
			t.Fatal(err)
		}
		err = s.Execute(context.Background(), p, func(step string) error {
			if step == "create-reserved-pv" {
				competitor := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "competitor-pv", UID: "competitor-uid"}, Spec: corev1.PersistentVolumeSpec{StorageClassName: "approved", PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}, PersistentVolumeSource: corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: "nfs.example.test", Path: "/prepared/alice/data/sub"}}}, Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable}}
				if _, cerr := k.CoreV1().PersistentVolumes().Create(context.Background(), competitor, metav1.CreateOptions{}); cerr != nil {
					t.Fatal(cerr)
				}
			}
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "already represented") {
			t.Fatalf("overlap not detected: %v", err)
		}
		if _, err := k.CoreV1().PersistentVolumes().Get(context.Background(), "new-pv", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatal("wrote a PV despite detected overlap")
		}
		if _, err := k.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "new", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatal("wrote a PVC despite detected overlap")
		}
	})
	t.Run("dependency-changed-between-writes", func(t *testing.T) {
		s, _, k := testService(t)
		fakeController(k, false)
		k.PrependReactor("create", "persistentvolumes", func(a ktesting.Action) (bool, runtime.Object, error) {
			ca, ok := a.(ktesting.CreateActionImpl)
			if !ok || len(ca.GetCreateOptions().DryRun) > 0 {
				return false, nil, nil
			}
			pv, ok := ca.GetObject().(*corev1.PersistentVolume)
			if !ok {
				return false, nil, nil
			}
			stored := pv.DeepCopy()
			stored.UID = types.UID("created-" + stored.Name)
			stored.ResourceVersion = "1"
			if err := k.Tracker().Create(pvGVR, stored, ""); err != nil {
				return true, nil, err
			}
			// Change the namespace identity after the PV write and before the
			// PVC write. Mutate through the tracker because the fake holds its
			// reaction lock while a reactor runs.
			nsObj, err := k.Tracker().Get(nsGVR, "", "alice")
			if err != nil {
				return true, nil, err
			}
			ns := nsObj.(*corev1.Namespace).DeepCopy()
			ns.UID = "namespace-changed"
			if err = k.Tracker().Update(nsGVR, ns, ""); err != nil {
				return true, nil, err
			}
			return true, stored, nil
		})
		p, err := s.Prepare(context.Background(), staticCreateAction())
		if err != nil {
			t.Fatal(err)
		}
		err = s.Execute(context.Background(), p, func(string) error { return nil })
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("dependency change not detected: %v", err)
		}
		if _, err := k.CoreV1().PersistentVolumes().Get(context.Background(), "new-pv", metav1.GetOptions{}); err != nil {
			t.Fatal("expected the partial PV to remain for review")
		}
		if _, err := k.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "new", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatal("created a PVC after a dependency change")
		}
	})
	t.Run("pv-mutated-after-create", func(t *testing.T) {
		s, _, k := testService(t)
		fakeController(k, false)
		k.PrependReactor("create", "persistentvolumes", func(a ktesting.Action) (bool, runtime.Object, error) {
			ca, ok := a.(ktesting.CreateActionImpl)
			if !ok || len(ca.GetCreateOptions().DryRun) > 0 {
				return false, nil, nil
			}
			pv, ok := ca.GetObject().(*corev1.PersistentVolume)
			if !ok {
				return false, nil, nil
			}
			stored := pv.DeepCopy()
			stored.UID = types.UID("created-" + stored.Name)
			stored.ResourceVersion = "1"
			stored.Spec.Capacity[corev1.ResourceStorage] = resource.MustParse("1Gi")
			if err := k.Tracker().Create(pvGVR, stored, ""); err != nil {
				return true, nil, err
			}
			return true, stored, nil
		})
		p, err := s.Prepare(context.Background(), staticCreateAction())
		if err != nil {
			t.Fatal(err)
		}
		err = s.Execute(context.Background(), p, func(string) error { return nil })
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("PV mutation not detected: %v", err)
		}
		if _, err := k.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), "new", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Fatal("created a PVC against a mutated PV")
		}
	})
}

// --- exact PV identity ---

func plannedNFSPV(path string) *corev1.PersistentVolume {
	fs := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv", UID: "uid-1", ResourceVersion: "1"},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName:              "approved",
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			VolumeMode:                    &fs,
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			MountOptions:                  []string{"nfsvers=3"},
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			PersistentVolumeSource:        corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: "nfs.example.test", Path: path}},
			ClaimRef:                      &corev1.ObjectReference{Name: "claim", Namespace: "alice"},
		},
	}
}

func plannedCSIPV() *corev1.PersistentVolume {
	fs := corev1.PersistentVolumeFilesystem
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv", UID: "uid-1", ResourceVersion: "1"},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName:              "approved",
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			VolumeMode:                    &fs,
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: "csi.example.vendor", VolumeHandle: "handle-1",
				VolumeAttributes: map[string]string{"a": "b"}, FSType: "ext4",
			}},
			ClaimRef: &corev1.ObjectReference{Name: "claim", Namespace: "alice"},
		},
	}
}

func TestPVMatchesPlanExactIdentity(t *testing.T) {
	cases := []struct {
		name    string
		planned *corev1.PersistentVolume
		actual  func(*corev1.PersistentVolume)
		want    bool
	}{
		{"nfs-identical", plannedNFSPV("/prepared/alice/data"), func(*corev1.PersistentVolume) {}, true},
		{"nfs-ancestor", plannedNFSPV("/prepared/alice/data"), func(pv *corev1.PersistentVolume) { pv.Spec.NFS.Path = "/prepared/alice" }, false},
		{"nfs-descendant", plannedNFSPV("/prepared/alice/data"), func(pv *corev1.PersistentVolume) { pv.Spec.NFS.Path = "/prepared/alice/data/sub" }, false},
		{"nfs-other-server", plannedNFSPV("/prepared/alice/data"), func(pv *corev1.PersistentVolume) { pv.Spec.NFS.Server = "evil.example.test" }, false},
		{"capacity", plannedNFSPV("/prepared/alice/data"), func(pv *corev1.PersistentVolume) {
			pv.Spec.Capacity[corev1.ResourceStorage] = resource.MustParse("1Gi")
		}, false},
		{"mount-options", plannedNFSPV("/prepared/alice/data"), func(pv *corev1.PersistentVolume) { pv.Spec.MountOptions = []string{"nolock"} }, false},
		{"class", plannedNFSPV("/prepared/alice/data"), func(pv *corev1.PersistentVolume) { pv.Spec.StorageClassName = "other" }, false},
		{"volume-mode", plannedNFSPV("/prepared/alice/data"), func(pv *corev1.PersistentVolume) { pv.Spec.VolumeMode = nil }, false},
		{"reclaim-policy", plannedNFSPV("/prepared/alice/data"), func(pv *corev1.PersistentVolume) {
			pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
		}, false},
		// A mutation to scheduling must be rejected even with an identical UID and reservation.
		{"node-affinity", plannedNFSPV("/prepared/alice/data"), func(pv *corev1.PersistentVolume) {
			pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{}}
		}, false},
		// ClaimRef is the reservation identity, verified separately, so a different value is not a spec mismatch.
		{"claim-ref-excluded", plannedNFSPV("/prepared/alice/data"), func(pv *corev1.PersistentVolume) { pv.Spec.ClaimRef = nil }, true},
		{"csi-identical", plannedCSIPV(), func(*corev1.PersistentVolume) {}, true},
		{"csi-attrs", plannedCSIPV(), func(pv *corev1.PersistentVolume) { pv.Spec.CSI.VolumeAttributes = map[string]string{"a": "evil"} }, false},
		{"csi-fstype", plannedCSIPV(), func(pv *corev1.PersistentVolume) { pv.Spec.CSI.FSType = "xfs" }, false},
		{"csi-readonly", plannedCSIPV(), func(pv *corev1.PersistentVolume) { pv.Spec.CSI.ReadOnly = true }, false},
		{"csi-node-publish-secret", plannedCSIPV(), func(pv *corev1.PersistentVolume) {
			pv.Spec.CSI.NodePublishSecretRef = &corev1.SecretReference{Name: "s"}
		}, false},
		{"csi-node-stage-secret", plannedCSIPV(), func(pv *corev1.PersistentVolume) { pv.Spec.CSI.NodeStageSecretRef = &corev1.SecretReference{Name: "s"} }, false},
		{"csi-handle", plannedCSIPV(), func(pv *corev1.PersistentVolume) { pv.Spec.CSI.VolumeHandle = "handle-2" }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			actual := tc.planned.DeepCopy()
			tc.actual(actual)
			if got := pvMatchesPlan(actual, tc.planned); got != tc.want {
				t.Fatalf("pvMatchesPlan = %v, want %v", got, tc.want)
			}
		})
	}
}

// --- post-create PV mutation must leave a partial PV and no PVC ---

func TestExecuteStaticCreateRejectsPostCreateNFSMutation(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(pv *corev1.PersistentVolume)
	}{
		{"nfs-ancestor", func(pv *corev1.PersistentVolume) { pv.Spec.NFS.Path = "/prepared/alice" }},
		{"nfs-descendant", func(pv *corev1.PersistentVolume) { pv.Spec.NFS.Path = "/prepared/alice/data/sub" }},
		{"node-affinity", func(pv *corev1.PersistentVolume) {
			pv.Spec.NodeAffinity = &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{}}
		}},
		{"mount-options", func(pv *corev1.PersistentVolume) { pv.Spec.MountOptions = []string{"nolock"} }},
		{"capacity", func(pv *corev1.PersistentVolume) {
			pv.Spec.Capacity[corev1.ResourceStorage] = resource.MustParse("1Gi")
		}},
		{"class", func(pv *corev1.PersistentVolume) { pv.Spec.StorageClassName = "other" }},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			s, _, k := testService(t)
			fakeController(k, false)
			k.PrependReactor("create", "persistentvolumes", mutatingPVReactor(k, tc.mutate))
			p, err := s.Prepare(context.Background(), staticCreateAction())
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Execute(context.Background(), p, func(string) error { return nil }); !errors.Is(err, ErrConflict) {
				t.Fatalf("mutation not detected: %v", err)
			}
			assertPartialPVNoPVC(t, k, "new-pv", "new")
		})
	}
}

func TestExecuteStaticCreateRejectsPostCreateCSIMutation(t *testing.T) {
	mutations := []struct {
		name   string
		mutate func(pv *corev1.PersistentVolume)
	}{
		{"attrs", func(pv *corev1.PersistentVolume) { pv.Spec.CSI.VolumeAttributes = map[string]string{"a": "evil"} }},
		{"fstype", func(pv *corev1.PersistentVolume) { pv.Spec.CSI.FSType = "xfs" }},
		{"readonly", func(pv *corev1.PersistentVolume) { pv.Spec.CSI.ReadOnly = true }},
		{"node-publish-secret", func(pv *corev1.PersistentVolume) {
			pv.Spec.CSI.NodePublishSecretRef = &corev1.SecretReference{Name: "s"}
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			s, _, k := testService(t)
			fakeController(k, false)
			k.PrependReactor("create", "persistentvolumes", mutatingPVReactor(k, tc.mutate))
			p, err := s.Prepare(context.Background(), csiStaticCreateAction())
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Execute(context.Background(), p, func(string) error { return nil }); !errors.Is(err, ErrConflict) {
				t.Fatalf("CSI mutation not detected: %v", err)
			}
			assertPartialPVNoPVC(t, k, "csi-pv", "csi")
		})
	}
}

func mutatingPVReactor(k *fake.Clientset, mutate func(*corev1.PersistentVolume)) ktesting.ReactionFunc {
	return func(a ktesting.Action) (bool, runtime.Object, error) {
		ca, ok := a.(ktesting.CreateActionImpl)
		if !ok || len(ca.GetCreateOptions().DryRun) > 0 {
			return false, nil, nil
		}
		pv, ok := ca.GetObject().(*corev1.PersistentVolume)
		if !ok {
			return false, nil, nil
		}
		stored := pv.DeepCopy()
		stored.UID = types.UID("created-" + stored.Name)
		stored.ResourceVersion = "1"
		mutate(stored)
		if err := k.Tracker().Create(pvGVR, stored, ""); err != nil {
			return true, nil, err
		}
		return true, stored, nil
	}
}

func assertPartialPVNoPVC(t *testing.T, k *fake.Clientset, pvName, pvcName string) {
	t.Helper()
	if _, err := k.CoreV1().PersistentVolumes().Get(context.Background(), pvName, metav1.GetOptions{}); err != nil {
		t.Fatalf("expected the partial PV %s to remain for review: %v", pvName, err)
	}
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Get(context.Background(), pvcName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected no PVC %s after rejected PV, got %v", pvcName, err)
	}
}

// --- mutations injected inside the create-pvc checkpoint must be caught ---

func TestExecuteStaticCreateRevalidatesInsideCreatePVCCheckpoint(t *testing.T) {
	t.Run("pv-mutated", func(t *testing.T) {
		s, _, k := testService(t)
		fakeController(k, false)
		p, err := s.Prepare(context.Background(), staticCreateAction())
		if err != nil {
			t.Fatal(err)
		}
		err = s.Execute(context.Background(), p, func(step string) error {
			if step != "create-pvc" {
				return nil
			}
			obj, gerr := k.Tracker().Get(pvGVR, "", "new-pv")
			if gerr != nil {
				return gerr
			}
			pv := obj.(*corev1.PersistentVolume).DeepCopy()
			pv.Spec.NFS.Path = "/prepared/alice"
			return k.Tracker().Update(pvGVR, pv, "")
		})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("checkpoint-injected PV mutation not detected: %v", err)
		}
		assertPartialPVNoPVC(t, k, "new-pv", "new")
	})
	t.Run("overlap-injected", func(t *testing.T) {
		s, _, k := testService(t)
		fakeController(k, false)
		p, err := s.Prepare(context.Background(), staticCreateAction())
		if err != nil {
			t.Fatal(err)
		}
		err = s.Execute(context.Background(), p, func(step string) error {
			if step != "create-pvc" {
				return nil
			}
			competitor := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "competitor-pv", UID: "competitor-uid"}, Spec: corev1.PersistentVolumeSpec{StorageClassName: "approved", PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, Capacity: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}, PersistentVolumeSource: corev1.PersistentVolumeSource{NFS: &corev1.NFSVolumeSource{Server: "nfs.example.test", Path: "/prepared/alice/data/sub"}}}, Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable}}
			_, cerr := k.CoreV1().PersistentVolumes().Create(context.Background(), competitor, metav1.CreateOptions{})
			return cerr
		})
		if err == nil || !strings.Contains(err.Error(), "already represented") {
			t.Fatalf("checkpoint-injected overlap not detected: %v", err)
		}
		assertPartialPVNoPVC(t, k, "new-pv", "new")
	})
}

// An empty CSI VolumeAttributes map is dropped by the apiserver on read-back
// (omitempty), so the planned PV must not emit one or post-create identity
// verification would falsely reject a correct create.
func TestStaticCSITemplateWithoutAttributesSurvivesAPIServerRoundTrip(t *testing.T) {
	s, _, _ := testService(t)
	w, err := s.build(context.Background(), csiStaticCreateAction())
	if err != nil {
		t.Fatal(err)
	}
	if w.pv.Spec.CSI.VolumeAttributes != nil {
		t.Fatalf("empty attributes must be nil, got %#v", w.pv.Spec.CSI.VolumeAttributes)
	}
	raw, err := json.Marshal(w.pv)
	if err != nil {
		t.Fatal(err)
	}
	actual := &corev1.PersistentVolume{}
	if err = json.Unmarshal(raw, actual); err != nil {
		t.Fatal(err)
	}
	if !pvMatchesPlan(actual, w.pv) {
		t.Fatal("round-tripped PV no longer matches the reviewed plan")
	}
}
