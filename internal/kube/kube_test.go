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
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
}

func key(resource, ns, n string) string { return resource + "/" + ns + "/" + n }
func (c *crAPI) put(r string, n *unstructured.Unstructured) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.objects[key(r, n.GetNamespace(), n.GetName())] = n.DeepCopy()
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
		_ = json.NewEncoder(w).Encode(n.Object)
		return
	}
	var obj *unstructured.Unstructured
	var ops []patchOp
	if r.Method == http.MethodPatch {
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
