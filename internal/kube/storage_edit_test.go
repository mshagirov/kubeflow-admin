package kube

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	jsonpatch "gopkg.in/evanphx/json-patch.v4"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	"kubeflow-admin/internal/model"
)

func storageService(t *testing.T) (*Service, *crAPI, *fake.Clientset) {
	t.Helper()
	s, api, k := testService(t)
	s.Config.Features.StorageEditing = true
	s.Config.Features.StorageDeletion = true
	installPVPatchReactor(k)
	installStorageDeleteDryRunReactor(k)
	return s, api, k
}

// installPVPatchReactor applies a JSON patch to a fake PersistentVolume and
// honors server-side dry-run. client-go's typed fake does not do either by
// default.
func installPVPatchReactor(k *fake.Clientset) {
	k.PrependReactor("patch", "persistentvolumes", func(a ktesting.Action) (bool, runtime.Object, error) {
		pa, ok := a.(ktesting.PatchActionImpl)
		if !ok || pa.GetPatchType() != types.JSONPatchType {
			return false, nil, nil
		}
		obj, err := k.Tracker().Get(pvGVR, "", pa.GetName())
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
		out := &corev1.PersistentVolume{}
		if err := json.Unmarshal(modified, out); err != nil {
			return true, nil, err
		}
		if err := k.Tracker().Update(pvGVR, out, ""); err != nil {
			return true, nil, err
		}
		return true, out, nil
	})
}

func installStorageDeleteDryRunReactor(k *fake.Clientset) {
	react := func(a ktesting.Action) (bool, runtime.Object, error) {
		da, ok := a.(ktesting.DeleteActionImpl)
		if !ok || len(da.GetDeleteOptions().DryRun) == 0 {
			return false, nil, nil
		}
		return true, nil, nil
	}
	k.PrependReactor("delete", "persistentvolumes", react)
	k.PrependReactor("delete", "persistentvolumeclaims", react)
}

func freePV(name, uid, rv string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, UID: types.UID(uid), ResourceVersion: rv,
			Labels:      map[string]string{"kubeflow-admin.io/note": "console", "team": "hidden"},
			Annotations: map[string]string{"secret-annotation": "hidden"},
		},
		Spec: corev1.PersistentVolumeSpec{
			StorageClassName:              "approved",
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
			Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver: "csi.example.vendor", VolumeHandle: "backend-handle-must-stay-hidden",
				VolumeAttributes: map[string]string{"backendPath": "/must/stay/hidden"},
			}},
		},
		Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable},
	}
}

func pendingPVC(namespace, name, uid string) *corev1.PersistentVolumeClaim {
	class := "approved"
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, UID: types.UID(uid), ResourceVersion: "1",
			Labels:      map[string]string{"kubeflow-admin.io/note": "console", "team": "hidden"},
			Annotations: map[string]string{"owner-note": "hidden"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &class,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimPending},
	}
}

func TestStorageDocumentSafeProjection(t *testing.T) {
	s, _, k := storageService(t)
	ctx := context.Background()
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, freePV("free-pv", "free-uid", "7"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.StorageDocument(ctx, "pv", "", "free-pv")
	if err != nil {
		t.Fatal(err)
	}
	if doc.Kind != storageKindPV || doc.Name != "free-pv" || doc.UID != "free-uid" || doc.ResourceVersion != "7" {
		t.Fatalf("unsafe document identity: %+v", doc)
	}
	for _, forbidden := range []string{"backend-handle-must-stay-hidden", "must/stay/hidden", "secret-annotation", "team:", "capacity", "claimRef", "storageClassName"} {
		if strings.Contains(doc.YAML, forbidden) {
			t.Fatalf("projection leaked %q:\n%s", forbidden, doc.YAML)
		}
	}
	if !strings.Contains(doc.YAML, "kubeflow-admin.io/note: console") || !strings.Contains(doc.YAML, "persistentVolumeReclaimPolicy: Retain") {
		t.Fatalf("projection missing approved fields:\n%s", doc.YAML)
	}

	pvcDoc, err := s.StorageDocument(ctx, "pvc", "alice", "home")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"volumeName", "storageClassName", "team:", "owner-note", "capacity"} {
		if strings.Contains(pvcDoc.YAML, forbidden) {
			t.Fatalf("PVC projection leaked %q:\n%s", forbidden, pvcDoc.YAML)
		}
	}
	if !strings.Contains(pvcDoc.YAML, "storage: 10Gi") {
		t.Fatalf("PVC projection missing storage:\n%s", pvcDoc.YAML)
	}
}

func TestStorageProjectionStrictParser(t *testing.T) {
	base := `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: claim
  namespace: alice
  uid: uid-1
  resourceVersion: "3"
  labels:
    kubeflow-admin.io/note: hello
spec:
  resources:
    requests:
      storage: 10Gi
`
	if _, err := parseStorageProjection(base); err != nil {
		t.Fatalf("valid projection rejected: %v", err)
	}
	cases := map[string]string{
		"unknown field": strings.Replace(base, "spec:\n", "spec:\n  volumeName: hidden\n", 1),
		"unknown secret": `apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: claim, namespace: alice, uid: uid-1, resourceVersion: "3", annotations: {password: SUPERSECRET}}
spec: {resources: {requests: {storage: 10Gi}}}
`,
		"duplicate key": `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: claim
  name: other
  namespace: alice
  uid: uid-1
  resourceVersion: "3"
spec:
  resources:
    requests:
      storage: 10Gi
`,
		"anchor":         strings.Replace(base, "  labels:\n    kubeflow-admin.io/note: hello\n", "  labels: &labels\n    kubeflow-admin.io/note: hello\n", 1),
		"anchored key":   strings.Replace(base, "metadata:\n", "&meta metadata:\n", 1),
		"custom tag key": strings.Replace(base, "metadata:\n", "!custom metadata:\n", 1),
		"alias":          strings.Replace(base, "  labels:\n    kubeflow-admin.io/note: hello\n", "  labels: *labels\n", 1),
		"multi document": base + "---\napiVersion: v1\nkind: PersistentVolumeClaim\nmetadata: {name: other}\n",
		"custom tag":     strings.Replace(base, "storage: 10Gi", "storage: !secret 10Gi", 1),
		"empty":          "",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := parseStorageProjection(raw)
			if err == nil {
				t.Fatal("invalid projection accepted")
			}
			if strings.Contains(err.Error(), "SUPERSECRET") {
				t.Fatalf("error echoed secret: %v", err)
			}
		})
	}
	long := base + strings.Repeat("# padding\n", storageDocumentMaxBytes/8)
	if _, err := parseStorageProjection(long); err == nil {
		t.Fatal("oversized projection accepted")
	}
}

func projectionEdit(t *testing.T, doc model.StorageDocument, mutate func(*storageProjection)) (string, string, string) {
	t.Helper()
	proj, err := parseStorageProjection(doc.YAML)
	if err != nil {
		t.Fatal(err)
	}
	if proj.Metadata.Labels == nil {
		empty := map[string]string{}
		proj.Metadata.Labels = &empty
	}
	mutate(&proj)
	y, err := marshalStorageProjection(proj)
	if err != nil {
		t.Fatal(err)
	}
	return y, doc.UID, doc.ResourceVersion
}

func TestPVCStorageEditMetadataOnlyPreservesHiddenFields(t *testing.T) {
	s, _, k := storageService(t)
	fakeController(k, false)
	s.Config.Features.PVCExpansion = false
	ctx := context.Background()
	home, err := k.CoreV1().PersistentVolumeClaims("alice").Get(ctx, "home", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	home.Labels = map[string]string{"kubeflow-admin.io/note": "old", "team": "hidden"}
	home.Annotations = map[string]string{"owner-note": "hidden"}
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Update(ctx, home, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.StorageDocument(ctx, "pvc", "alice", "home")
	if err != nil {
		t.Fatal(err)
	}
	raw, uid, rv := projectionEdit(t, doc, func(p *storageProjection) {
		(*p.Metadata.Labels)["kubeflow-admin.io/note"] = "changed"
	})
	a := model.Action{Kind: "pvc.edit", Namespace: "alice", Name: "home", StorageYAML: raw, TargetUID: uid, TargetResourceVersion: rv}
	p, err := s.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.Action.StorageYAML, "volumeName") || !strings.Contains(p.Action.StorageYAML, "10Gi") {
		t.Fatalf("normalized document is unsafe: %s", p.Action.StorageYAML)
	}
	if err = s.Execute(ctx, p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cur, err := k.CoreV1().PersistentVolumeClaims("alice").Get(ctx, "home", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cur.Labels["kubeflow-admin.io/note"] != "changed" || cur.Labels["team"] != "hidden" || cur.Annotations["owner-note"] != "hidden" {
		t.Fatalf("hidden metadata was not preserved: %#v %#v", cur.Labels, cur.Annotations)
	}
	if cur.Spec.VolumeName != "home" || cur.Spec.Resources.Requests.Storage().Cmp(resource.MustParse("10Gi")) != 0 {
		t.Fatalf("unrelated spec changed: %+v", cur.Spec)
	}

	plain := pendingPVC("alice", "plain", "plain-uid")
	plain.Labels = nil
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, plain, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	plainDoc, err := s.StorageDocument(ctx, "pvc", "alice", "plain")
	if err != nil {
		t.Fatal(err)
	}
	plainRaw, plainUID, plainRV := projectionEdit(t, plainDoc, func(p *storageProjection) {
		(*p.Metadata.Labels)["kubeflow-admin.io/new"] = "value"
	})
	if _, err := s.Prepare(ctx, model.Action{Kind: "pvc.edit", Namespace: "alice", Name: "plain", StorageYAML: plainRaw, TargetUID: plainUID, TargetResourceVersion: plainRV}); err != nil {
		t.Fatalf("adding the first label to an object with no labels map failed: %v", err)
	}
	plainDoc, _ = s.StorageDocument(ctx, "pvc", "alice", "plain")
	plainRaw, plainUID, plainRV = projectionEdit(t, plainDoc, func(p *storageProjection) {
		(*p.Metadata.Labels)["kubeflow-admin.io/new"] = "value"
	})
	plainPrepared, err := s.Prepare(ctx, model.Action{Kind: "pvc.edit", Namespace: "alice", Name: "plain", StorageYAML: plainRaw, TargetUID: plainUID, TargetResourceVersion: plainRV})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Execute(ctx, plainPrepared, func(string) error { return nil }); err != nil {
		t.Fatalf("first-label patch failed: %v", err)
	}
	plainCur, err := k.CoreV1().PersistentVolumeClaims("alice").Get(ctx, "plain", metav1.GetOptions{})
	if err != nil || plainCur.Labels["kubeflow-admin.io/new"] != "value" {
		t.Fatalf("first label was not applied: %v %#v", err, plainCur.Labels)
	}
}

func TestStorageEditClearAllConsoleLabels(t *testing.T) {
	t.Run("PVC", func(t *testing.T) {
		s, _, k := storageService(t)
		fakeController(k, false)
		ctx := context.Background()
		pvc := pendingPVC("alice", "clear", "clear-uid")
		pvc.Labels = map[string]string{"kubeflow-admin.io/old": "value", "team": "hidden"}
		pvc.Annotations = map[string]string{"owner-note": "hidden"}
		if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		doc, err := s.StorageDocument(ctx, "pvc", "alice", "clear")
		if err != nil {
			t.Fatal(err)
		}
		raw, uid, rv := projectionEdit(t, doc, func(p *storageProjection) {
			empty := map[string]string{}
			p.Metadata.Labels = &empty
		})
		if !strings.Contains(raw, "labels: {}") {
			t.Fatalf("explicit empty labels did not serialize as {}:\n%s", raw)
		}
		p, err := s.Prepare(ctx, model.Action{Kind: "pvc.edit", Namespace: "alice", Name: "clear", StorageYAML: raw, TargetUID: uid, TargetResourceVersion: rv})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p.Action.StorageYAML, "labels: {}") {
			t.Fatalf("normalized action lost explicit empty labels:\n%s", p.Action.StorageYAML)
		}
		normalized, err := parseStorageProjection(p.Action.StorageYAML)
		if err != nil || normalized.Metadata.Labels == nil || len(*normalized.Metadata.Labels) != 0 {
			t.Fatalf("normalized action did not round-trip clear-all: %v %#v", err, normalized.Metadata.Labels)
		}
		if err := s.Execute(ctx, p, func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
		cur, err := k.CoreV1().PersistentVolumeClaims("alice").Get(ctx, "clear", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := cur.Labels["kubeflow-admin.io/old"]; exists {
			t.Fatal("console label was not removed")
		}
		if cur.Labels["team"] != "hidden" || cur.Annotations["owner-note"] != "hidden" {
			t.Fatalf("hidden metadata was not preserved: %#v %#v", cur.Labels, cur.Annotations)
		}
	})

	t.Run("PV", func(t *testing.T) {
		s, _, k := storageService(t)
		ctx := context.Background()
		if _, err := k.CoreV1().PersistentVolumes().Create(ctx, freePV("free-pv", "free-uid", "7"), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		doc, err := s.StorageDocument(ctx, "pv", "", "free-pv")
		if err != nil {
			t.Fatal(err)
		}
		raw, uid, rv := projectionEdit(t, doc, func(p *storageProjection) {
			empty := map[string]string{}
			p.Metadata.Labels = &empty
		})
		p, err := s.Prepare(ctx, model.Action{Kind: "pv.edit", Name: "free-pv", StorageYAML: raw, TargetUID: uid, TargetResourceVersion: rv})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(p.Action.StorageYAML, "labels: {}") {
			t.Fatalf("normalized PV action lost explicit empty labels:\n%s", p.Action.StorageYAML)
		}
		if err := s.Execute(ctx, p, func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
		cur, err := k.CoreV1().PersistentVolumes().Get(ctx, "free-pv", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if _, exists := cur.Labels["kubeflow-admin.io/note"]; exists {
			t.Fatal("PV console label was not removed")
		}
		if cur.Labels["team"] != "hidden" || cur.Annotations["secret-annotation"] != "hidden" || cur.Spec.CSI == nil {
			t.Fatalf("hidden PV metadata was not preserved: %#v %#v", cur.Labels, cur.Annotations)
		}
	})

	t.Run("no change when no labels exist", func(t *testing.T) {
		s, _, k := storageService(t)
		ctx := context.Background()
		plain := pendingPVC("alice", "plain", "plain-uid")
		plain.Labels = nil
		if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, plain, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		doc, err := s.StorageDocument(ctx, "pvc", "alice", "plain")
		if err != nil {
			t.Fatal(err)
		}
		raw, uid, rv := projectionEdit(t, doc, func(p *storageProjection) {
			empty := map[string]string{}
			p.Metadata.Labels = &empty
		})
		if _, err := s.Prepare(ctx, model.Action{Kind: "pvc.edit", Namespace: "alice", Name: "plain", StorageYAML: raw, TargetUID: uid, TargetResourceVersion: rv}); err == nil {
			t.Fatal("empty clear-all edit with no existing console labels was accepted as a no-op")
		}
	})
}

func TestPVCStorageEditExpansionAndNoShrinkNoNoop(t *testing.T) {
	s, _, k := storageService(t)
	fakeController(k, false)
	ctx := context.Background()
	doc, err := s.StorageDocument(ctx, "pvc", "alice", "home")
	if err != nil {
		t.Fatal(err)
	}
	raw, uid, rv := projectionEdit(t, doc, func(p *storageProjection) {
		p.Spec.Resources.Requests.Storage = "20Gi"
	})
	a := model.Action{Kind: "pvc.edit", Namespace: "alice", Name: "home", StorageYAML: raw, TargetUID: uid, TargetResourceVersion: rv}
	p, err := s.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Execute(ctx, p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cur, err := k.CoreV1().PersistentVolumeClaims("alice").Get(ctx, "home", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cur.Spec.Resources.Requests.Storage().Cmp(resource.MustParse("20Gi")) != 0 || cur.Status.Capacity.Storage().Cmp(resource.MustParse("20Gi")) != 0 {
		t.Fatalf("expansion did not converge: req=%v status=%v", cur.Spec.Resources.Requests.Storage(), cur.Status.Capacity.Storage())
	}

	doc, _ = s.StorageDocument(ctx, "pvc", "alice", "home")
	shrink, uid, rv := projectionEdit(t, doc, func(p *storageProjection) { p.Spec.Resources.Requests.Storage = "5Gi" })
	if _, err := s.Prepare(ctx, model.Action{Kind: "pvc.edit", Namespace: "alice", Name: "home", StorageYAML: shrink, TargetUID: uid, TargetResourceVersion: rv}); err == nil {
		t.Fatal("shrink accepted")
	}
	doc, _ = s.StorageDocument(ctx, "pvc", "alice", "home")
	noop, uid, rv := projectionEdit(t, doc, func(p *storageProjection) {})
	if _, err := s.Prepare(ctx, model.Action{Kind: "pvc.edit", Namespace: "alice", Name: "home", StorageYAML: noop, TargetUID: uid, TargetResourceVersion: rv}); err == nil {
		t.Fatal("no-op edit accepted")
	}
	s.Config.Features.PVCExpansion = false
	doc, _ = s.StorageDocument(ctx, "pvc", "alice", "home")
	grow, uid, rv := projectionEdit(t, doc, func(p *storageProjection) { p.Spec.Resources.Requests.Storage = "30Gi" })
	if _, err := s.Prepare(ctx, model.Action{Kind: "pvc.edit", Namespace: "alice", Name: "home", StorageYAML: grow, TargetUID: uid, TargetResourceVersion: rv}); err == nil {
		t.Fatal("expansion accepted while pvcExpansion is disabled")
	}
}

func TestPVCEditLabelOnlyDoesNotRequireClass(t *testing.T) {
	s, _, k := storageService(t)
	fakeController(k, false)
	ctx := context.Background()
	pvc := pendingPVC("alice", "no-class", "no-class-uid")
	pvc.Spec.StorageClassName = nil
	pvc.Labels = map[string]string{"kubeflow-admin.io/note": "old"}
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.StorageDocument(ctx, "pvc", "alice", "no-class")
	if err != nil {
		t.Fatal(err)
	}
	raw, uid, rv := projectionEdit(t, doc, func(p *storageProjection) {
		(*p.Metadata.Labels)["kubeflow-admin.io/note"] = "new"
	})
	p, err := s.Prepare(ctx, model.Action{Kind: "pvc.edit", Namespace: "alice", Name: "no-class", StorageYAML: raw, TargetUID: uid, TargetResourceVersion: rv})
	if err != nil {
		t.Fatalf("safe label-only edit unexpectedly required a StorageClass: %v", err)
	}
	if err := s.Execute(ctx, p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cur, err := k.CoreV1().PersistentVolumeClaims("alice").Get(ctx, "no-class", metav1.GetOptions{})
	if err != nil || cur.Labels["kubeflow-admin.io/note"] != "new" {
		t.Fatalf("label-only edit did not apply: %v %#v", err, cur.Labels)
	}
}

func TestStorageEditOmittedLabelsPreserveExisting(t *testing.T) {
	s, _, k := storageService(t)
	ctx := context.Background()
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, freePV("free-pv", "free-uid", "7"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.StorageDocument(ctx, "pv", "", "free-pv")
	if err != nil {
		t.Fatal(err)
	}
	raw, uid, rv := projectionEdit(t, doc, func(p *storageProjection) {
		p.Metadata.Labels = nil
		p.Spec.PersistentVolumeReclaimPolicy = "Delete"
	})
	p, err := s.Prepare(ctx, model.Action{Kind: "pv.edit", Name: "free-pv", StorageYAML: raw, TargetUID: uid, TargetResourceVersion: rv})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Execute(ctx, p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cur, err := k.CoreV1().PersistentVolumes().Get(ctx, "free-pv", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cur.Labels["kubeflow-admin.io/note"] != "console" || cur.Labels["team"] != "hidden" {
		t.Fatalf("omitted labels removed existing labels: %#v", cur.Labels)
	}
}

func TestPVCDeleteNotebookOtherVolumeKindIsNotAReference(t *testing.T) {
	s, api, k := storageService(t)
	ctx := context.Background()
	pvc := pendingPVC("alice", "other-kind", "other-kind-uid")
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	note := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubeflow.org/v1", "kind": "Notebook",
		"metadata": map[string]any{"name": "other-kind-note", "namespace": "alice", "uid": "other-kind-note", "resourceVersion": "1"},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"volumes": []any{
			map[string]any{"name": "config", "configMap": map[string]any{"name": "settings"}},
		}}}},
	}}
	api.put("notebooks", note)
	if _, err := s.Prepare(ctx, model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "other-kind", TargetUID: "other-kind-uid", TargetResourceVersion: "1", ConfirmName: "other-kind", Reason: "test"}); err != nil {
		t.Fatalf("a non-PVC Notebook volume was treated as a reference: %v", err)
	}
}

func TestPVStorageEditChangesOnlyApprovedFields(t *testing.T) {
	s, _, k := storageService(t)
	ctx := context.Background()
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, freePV("free-pv", "free-uid", "7"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.StorageDocument(ctx, "PersistentVolume", "", "free-pv")
	if err != nil {
		t.Fatal(err)
	}
	raw, uid, rv := projectionEdit(t, doc, func(p *storageProjection) {
		p.Spec.PersistentVolumeReclaimPolicy = "Delete"
		(*p.Metadata.Labels)["kubeflow-admin.io/new"] = "value"
	})
	a := model.Action{Kind: "pv.edit", Name: "free-pv", StorageYAML: raw, TargetUID: uid, TargetResourceVersion: rv}
	p, err := s.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.Warnings, "\n"), "destroy the backing data irreversibly") {
		t.Fatalf("missing Delete warning: %v", p.Warnings)
	}
	if err := s.Execute(ctx, p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cur, err := k.CoreV1().PersistentVolumes().Get(ctx, "free-pv", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if cur.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimDelete {
		t.Fatal("reclaim policy not changed")
	}
	if cur.Spec.CSI == nil || cur.Spec.CSI.VolumeHandle != "backend-handle-must-stay-hidden" || cur.Labels["team"] != "hidden" || cur.Annotations["secret-annotation"] != "hidden" {
		t.Fatalf("hidden fields changed: %+v %#v", cur.Spec.CSI, cur.Labels)
	}
	if cur.Labels["kubeflow-admin.io/new"] != "value" || cur.Labels["kubeflow-admin.io/note"] != "console" {
		t.Fatalf("approved labels not applied: %#v", cur.Labels)
	}
}

func TestStorageActionsRequireFeatureGates(t *testing.T) {
	s, _, k := storageService(t)
	ctx := context.Background()
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, freePV("free-pv", "free-uid", "7"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.StorageDocument(ctx, "pv", "", "free-pv")
	if err != nil {
		t.Fatal(err)
	}
	raw, uid, rv := projectionEdit(t, doc, func(p *storageProjection) {
		(*p.Metadata.Labels)["kubeflow-admin.io/note"] = "x"
	})
	s.Config.Features.StorageEditing = false
	if _, err := s.Prepare(ctx, model.Action{Kind: "pv.edit", Name: "free-pv", StorageYAML: raw, TargetUID: uid, TargetResourceVersion: rv}); err == nil {
		t.Fatal("edit accepted while storageEditing is off")
	}
	s.Config.Features.StorageEditing = true
	s.Config.Features.StorageDeletion = false
	if _, err := s.Prepare(ctx, model.Action{Kind: "pv.delete", Name: "free-pv", TargetUID: uid, TargetResourceVersion: rv, ConfirmName: "free-pv", Reason: "unused"}); err == nil {
		t.Fatal("delete accepted while storageDeletion is off")
	}
}

type deleteRecord struct {
	dry  bool
	opts metav1.DeleteOptions
}

func deleteRecords(k *fake.Clientset, resource string) []deleteRecord {
	out := []deleteRecord{}
	for _, a := range k.Actions() {
		if a.GetVerb() != "delete" || a.GetResource().Resource != resource {
			continue
		}
		da, ok := a.(ktesting.DeleteActionImpl)
		if !ok {
			continue
		}
		opts := da.GetDeleteOptions()
		out = append(out, deleteRecord{dry: len(opts.DryRun) > 0, opts: opts})
	}
	return out
}

func TestStorageDeleteDryRunPreconditionsAndOneWrite(t *testing.T) {
	s, _, k := storageService(t)
	ctx := context.Background()
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, freePV("free-pv", "free-uid", "7"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	a := model.Action{Kind: "pv.delete", Name: "free-pv", TargetUID: "free-uid", TargetResourceVersion: "7", ConfirmName: "free-pv", Reason: "verified unused"}
	p, err := s.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Execute(ctx, p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	records := deleteRecords(k, "persistentvolumes")
	dry, real := 0, 0
	for _, record := range records {
		if record.dry {
			dry++
			continue
		}
		real++
		if record.opts.Preconditions == nil || record.opts.Preconditions.UID == nil || *record.opts.Preconditions.UID != "free-uid" || record.opts.Preconditions.ResourceVersion == nil || *record.opts.Preconditions.ResourceVersion != "7" {
			t.Fatalf("missing UID/resourceVersion preconditions: %+v", record.opts.Preconditions)
		}
	}
	if dry < 2 || real != 1 {
		t.Fatalf("expected at least two dry runs and exactly one real delete, got dry=%d real=%d", dry, real)
	}
	if _, err := k.CoreV1().PersistentVolumes().Get(ctx, "free-pv", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("PV was not deleted")
	}
}

func installPVDropEmptyLabelReactor(k *fake.Clientset) {
	k.PrependReactor("patch", "persistentvolumes", func(a ktesting.Action) (bool, runtime.Object, error) {
		pa, ok := a.(ktesting.PatchActionImpl)
		if !ok || pa.GetPatchType() != types.JSONPatchType || len(pa.GetPatchOptions().DryRun) > 0 {
			return false, nil, nil
		}
		obj, err := k.Tracker().Get(pvGVR, "", pa.GetName())
		if err != nil {
			return true, nil, err
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
		out := &corev1.PersistentVolume{}
		if err := json.Unmarshal(modified, out); err != nil {
			return true, nil, err
		}
		for key, value := range out.Labels {
			if strings.HasPrefix(key, StorageLabelPrefix) && value == "" {
				delete(out.Labels, key)
			}
		}
		if err := k.Tracker().Update(pvGVR, out, ""); err != nil {
			return true, nil, err
		}
		return true, out, nil
	})
}

func TestStorageEditEmptyLabelStrippedByAdmissionFailsConvergence(t *testing.T) {
	s, _, k := storageService(t)
	installPVDropEmptyLabelReactor(k)
	ctx := context.Background()
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, freePV("free-pv", "free-uid", "7"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.StorageDocument(ctx, "pv", "", "free-pv")
	if err != nil {
		t.Fatal(err)
	}
	raw, uid, rv := projectionEdit(t, doc, func(p *storageProjection) {
		(*p.Metadata.Labels)["kubeflow-admin.io/empty"] = ""
	})
	p, err := s.Prepare(ctx, model.Action{Kind: "pv.edit", Name: "free-pv", StorageYAML: raw, TargetUID: uid, TargetResourceVersion: rv})
	if err != nil {
		t.Fatal(err)
	}
	err = s.Execute(ctx, p, func(string) error { return nil })
	if err == nil {
		t.Fatal("striped empty label was reported as converged")
	}
	if errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("patch had already been attempted; outcome must stay ambiguous: %v", err)
	}
	cur, err := k.CoreV1().PersistentVolumes().Get(ctx, "free-pv", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := cur.Labels["kubeflow-admin.io/empty"]; exists {
		t.Fatal("fixture did not strip the empty label")
	}
}

func TestStorageEditStaleAtCheckpointNeverWrites(t *testing.T) {
	s, _, k := storageService(t)
	ctx := context.Background()
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, freePV("free-pv", "free-uid", "7"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.StorageDocument(ctx, "pv", "", "free-pv")
	if err != nil {
		t.Fatal(err)
	}
	raw, uid, rv := projectionEdit(t, doc, func(p *storageProjection) {
		(*p.Metadata.Labels)["kubeflow-admin.io/note"] = "changed"
	})
	a := model.Action{Kind: "pv.edit", Name: "free-pv", StorageYAML: raw, TargetUID: uid, TargetResourceVersion: rv}
	p, err := s.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Execute(ctx, p, func(step string) error {
		cur, err := k.CoreV1().PersistentVolumes().Get(ctx, "free-pv", metav1.GetOptions{})
		if err != nil {
			return err
		}
		cur.ResourceVersion = "8"
		_, err = k.CoreV1().PersistentVolumes().Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
	if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("stale edit should fail before mutation: %v", err)
	}
	for _, action := range k.Actions() {
		if action.GetVerb() != "patch" || action.GetResource().Resource != "persistentvolumes" {
			continue
		}
		pa, ok := action.(ktesting.PatchActionImpl)
		if ok && len(pa.GetPatchOptions().DryRun) == 0 {
			t.Fatal("stale edit issued a real patch")
		}
	}
}

func TestStorageDeleteStaleAtCheckpointNeverWrites(t *testing.T) {
	s, _, k := storageService(t)
	ctx := context.Background()
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, freePV("free-pv", "free-uid", "7"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	a := model.Action{Kind: "pv.delete", Name: "free-pv", TargetUID: "free-uid", TargetResourceVersion: "7", ConfirmName: "free-pv", Reason: "verified unused"}
	p, err := s.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Execute(ctx, p, func(step string) error {
		cur, err := k.CoreV1().PersistentVolumes().Get(ctx, "free-pv", metav1.GetOptions{})
		if err != nil {
			return err
		}
		cur.ResourceVersion = "8"
		_, err = k.CoreV1().PersistentVolumes().Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
	if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("stale plan should fail before mutation: %v", err)
	}
	for _, record := range deleteRecords(k, "persistentvolumes") {
		if !record.dry {
			t.Fatal("stale plan issued a real delete")
		}
	}
}

func TestStorageDeleteReplacementIsNotReportedAsGone(t *testing.T) {
	s, _, k := storageService(t)
	ctx := context.Background()
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, freePV("free-pv", "free-uid", "7"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	k.PrependReactor("delete", "persistentvolumes", func(a ktesting.Action) (bool, runtime.Object, error) {
		da, ok := a.(ktesting.DeleteActionImpl)
		if !ok || len(da.GetDeleteOptions().DryRun) > 0 {
			return false, nil, nil
		}
		replacement := freePV("free-pv", "replacement-uid", "9")
		if err := k.Tracker().Update(pvGVR, replacement, ""); err != nil {
			return true, nil, err
		}
		return true, nil, nil
	})
	a := model.Action{Kind: "pv.delete", Name: "free-pv", TargetUID: "free-uid", TargetResourceVersion: "7", ConfirmName: "free-pv", Reason: "verified unused"}
	p, err := s.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Execute(ctx, p, func(string) error { return nil })
	if err == nil || errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("a replacement object must stay unresolved for review: %v", err)
	}
	if !strings.Contains(err.Error(), "replacement") {
		t.Fatalf("unexpected replacement error: %v", err)
	}
}

func TestStorageDeleteUncertainOutcomeIsNotReplayed(t *testing.T) {
	s, _, k := storageService(t)
	ctx := context.Background()
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, freePV("free-pv", "free-uid", "7"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	realDeletes := 0
	k.PrependReactor("delete", "persistentvolumes", func(a ktesting.Action) (bool, runtime.Object, error) {
		da, ok := a.(ktesting.DeleteActionImpl)
		if !ok || len(da.GetDeleteOptions().DryRun) > 0 {
			return false, nil, nil
		}
		mu.Lock()
		realDeletes++
		mu.Unlock()
		return true, nil, apierrors.NewTooManyRequests("throttled", 1)
	})
	a := model.Action{Kind: "pv.delete", Name: "free-pv", TargetUID: "free-uid", TargetResourceVersion: "7", ConfirmName: "free-pv", Reason: "verified unused"}
	p, err := s.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Execute(ctx, p, func(string) error { return nil })
	if err == nil || errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("an uncertain outcome must stay ambiguous: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if realDeletes != 1 {
		t.Fatalf("expected one real delete attempt and no replay, got %d", realDeletes)
	}
}

func TestPVCStorageDeleteUnusedOnlyAndReferences(t *testing.T) {
	s, api, k := storageService(t)
	ctx := context.Background()
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, pendingPVC("alice", "unused", "unused-uid"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "unused", TargetUID: "unused-uid", TargetResourceVersion: "1", ConfirmName: "unused", Reason: "verified unused"}
	p, err := s.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Execute(ctx, p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Get(ctx, "unused", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("unused PVC was not deleted")
	}

	podRef := pendingPVC("alice", "pod-ref", "pod-ref-uid")
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, podRef, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: "alice"}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "pod-ref"}}}}}}
	if _, err := k.CoreV1().Pods("alice").Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "pod-ref", TargetUID: "pod-ref-uid", TargetResourceVersion: "1", ConfirmName: "pod-ref", Reason: "test"}); err == nil {
		t.Fatal("Pod-referenced PVC accepted")
	}

	inline := pendingPVC("alice", "inline-claim", "inline-uid")
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, inline, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	ephemeralPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "inline", Namespace: "alice"}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "claim", VolumeSource: corev1.VolumeSource{Ephemeral: &corev1.EphemeralVolumeSource{VolumeClaimTemplate: &corev1.PersistentVolumeClaimTemplate{}}}}}}}
	if _, err := k.CoreV1().Pods("alice").Create(ctx, ephemeralPod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "inline-claim", TargetUID: "inline-uid", TargetResourceVersion: "1", ConfirmName: "inline-claim", Reason: "test"}); err == nil {
		t.Fatal("inline ephemeral claim collision accepted")
	}

	notePVC := pendingPVC("alice", "note-ref", "note-uid")
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, notePVC, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	note := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubeflow.org/v1", "kind": "Notebook",
		"metadata": map[string]any{"name": "stopped", "namespace": "alice", "uid": "note-uid", "resourceVersion": "1", "annotations": map[string]any{"kubeflow-resource-stopped": "2026-01-01T00:00:00Z"}},
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"volumes": []any{
			map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": "note-ref"}},
		}}}},
	}}
	api.put("notebooks", note)
	if _, err := s.Prepare(ctx, model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "note-ref", TargetUID: "note-uid", TargetResourceVersion: "1", ConfirmName: "note-ref", Reason: "test"}); err == nil {
		t.Fatal("Notebook-referenced PVC accepted")
	}
}

func TestPVCDeleteMalformedNotebookVolumesFailClosed(t *testing.T) {
	s, api, k := storageService(t)
	ctx := context.Background()
	volumesSpec := func(volumes any) map[string]any {
		return map[string]any{"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"volumes": volumes}}}}
	}
	cases := map[string]map[string]any{
		"volumes wrong type":      volumesSpec("not-a-list"),
		"volume entry wrong type": volumesSpec([]any{"not-a-map"}),
		"pvc source wrong type": volumesSpec([]any{
			map[string]any{"name": "data", "persistentVolumeClaim": "not-a-map"},
		}),
		"claim name wrong type": volumesSpec([]any{
			map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{"claimName": int64(7)}},
		}),
		"missing claim name": volumesSpec([]any{
			map[string]any{"name": "data", "persistentVolumeClaim": map[string]any{}},
		}),
		"template wrong type": {"spec": map[string]any{"template": "not-a-map"}},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			pvcName := "malformed-" + strings.ReplaceAll(name, " ", "-")
			pvc := pendingPVC("alice", pvcName, pvcName+"-uid")
			if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			note := &unstructured.Unstructured{Object: map[string]any{
				"apiVersion": "kubeflow.org/v1", "kind": "Notebook",
				"metadata": map[string]any{"name": "note-" + pvcName, "namespace": "alice", "uid": "note-" + pvcName, "resourceVersion": "1"},
			}}
			for key, value := range body {
				note.Object[key] = value
			}
			api.put("notebooks", note)
			a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: pvcName, TargetUID: pvcName + "-uid", TargetResourceVersion: "1", ConfirmName: pvcName, Reason: "test"}
			if _, err := s.Prepare(ctx, a); err == nil {
				t.Fatal("malformed Notebook volumes accepted; deletion could hide a reference")
			}
		})
	}
	for _, record := range deleteRecords(k, "persistentvolumeclaims") {
		if !record.dry {
			t.Fatal("malformed Notebook reference reached a real delete")
		}
	}
}

func TestPVCStorageDeleteBoundAndControllerOwned(t *testing.T) {
	s, _, k := storageService(t)
	ctx := context.Background()
	// A bound claim whose PV names it exactly and with no workload reference is
	// unused under this workflow's rule.
	pvc := pendingPVC("alice", "idle", "idle-uid")
	pvc.Spec.VolumeName = "idle-pv"
	pvc.Status.Phase = corev1.ClaimBound
	pv := freePV("idle-pv", "idle-pv-uid", "3")
	pv.Spec.ClaimRef = &corev1.ObjectReference{Name: "idle", Namespace: "alice", UID: "idle-uid"}
	pv.Spec.PersistentVolumeReclaimPolicy = corev1.PersistentVolumeReclaimDelete
	pv.Status.Phase = corev1.VolumeBound
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "idle", TargetUID: "idle-uid", TargetResourceVersion: "1", ConfirmName: "idle", Reason: "verified unused"}
	p, err := s.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(p.Warnings, "\n"), "Delete reclaim policy") {
		t.Fatalf("missing irreversible data warning: %v", p.Warnings)
	}
	if err := s.Execute(ctx, p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}

	controllerOwned := pendingPVC("alice", "owned", "owned-uid")
	controller := true
	controllerOwned.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "sts", UID: "sts-uid", Controller: &controller}}
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, controllerOwned, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "owned", TargetUID: "owned-uid", TargetResourceVersion: "1", ConfirmName: "owned", Reason: "test"}); err == nil {
		t.Fatal("controller-owned PVC accepted")
	}
}

func TestPVStorageDeleteRefusesNonUnused(t *testing.T) {
	s, _, k := storageService(t)
	ctx := context.Background()
	bound := freePV("bound-pv", "bound-uid", "2")
	bound.Status.Phase = corev1.VolumeBound
	bound.Spec.ClaimRef = &corev1.ObjectReference{Name: "home", Namespace: "alice", UID: "pvc-home"}
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, bound, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, model.Action{Kind: "pv.delete", Name: "bound-pv", TargetUID: "bound-uid", TargetResourceVersion: "2", ConfirmName: "bound-pv", Reason: "test"}); err == nil {
		t.Fatal("bound PV accepted")
	}

	reserved := freePV("reserved-pv", "reserved-uid", "2")
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, reserved, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	claim := pendingPVC("other", "reserving", "reserving-uid")
	claim.Spec.VolumeName = "reserved-pv"
	if _, err := k.CoreV1().PersistentVolumeClaims("other").Create(ctx, claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, model.Action{Kind: "pv.delete", Name: "reserved-pv", TargetUID: "reserved-uid", TargetResourceVersion: "2", ConfirmName: "reserved-pv", Reason: "test"}); err == nil {
		t.Fatal("PVC-referenced PV accepted")
	}

	preboundClaim := pendingPVC("alice", "prebound", "prebound-uid")
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, preboundClaim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	preserved := freePV("prebound-pv", "prebound-pv-uid", "2")
	preserved.Spec.ClaimRef = &corev1.ObjectReference{Name: "prebound", Namespace: "alice"}
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, preserved, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "prebound", TargetUID: "prebound-uid", TargetResourceVersion: "1", ConfirmName: "prebound", Reason: "test"}); err == nil {
		t.Fatal("PVC reserved by a PV accepted")
	}
}

func TestStorageDeleteEvidenceBoundPVAndScopeAreDigested(t *testing.T) {
	s, _, k := storageService(t)
	ctx := context.Background()
	pvc := pendingPVC("alice", "idle", "idle-uid")
	pvc.Spec.VolumeName = "idle-pv"
	pvc.Status.Phase = corev1.ClaimBound
	pv := freePV("idle-pv", "idle-pv-uid", "3")
	pv.Spec.ClaimRef = &corev1.ObjectReference{Name: "idle", Namespace: "alice", UID: "idle-uid"}
	pv.Status.Phase = corev1.VolumeBound
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "idle", TargetUID: "idle-uid", TargetResourceVersion: "1", ConfirmName: "idle", Reason: "verified unused"}
	p, err := s.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(p.Data), "backend-handle-must-stay-hidden") || strings.Contains(string(p.Data), "must/stay/hidden") || strings.Contains(string(p.Data), "hidden") {
		t.Fatalf("delete evidence persisted a backend value: %s", p.Data)
	}
	if !strings.Contains(string(p.Data), `"scope"`) {
		t.Fatalf("delete evidence has no digested scope: %s", p.Data)
	}

	for name, mutate := range map[string]func(*corev1.PersistentVolume){
		"replaced bound PV": func(pv *corev1.PersistentVolume) {
			pv.UID = "replacement-pv-uid"
			pv.ResourceVersion = "4"
		},
		"changed bound PV source": func(pv *corev1.PersistentVolume) {
			pv.Spec.CSI.VolumeHandle = "changed-backend-handle"
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, _, k := storageService(t)
			pvc := pendingPVC("alice", "idle", "idle-uid")
			pvc.Spec.VolumeName = "idle-pv"
			pvc.Status.Phase = corev1.ClaimBound
			pv := freePV("idle-pv", "idle-pv-uid", "3")
			pv.Spec.ClaimRef = &corev1.ObjectReference{Name: "idle", Namespace: "alice", UID: "idle-uid"}
			pv.Status.Phase = corev1.VolumeBound
			if _, err := k.CoreV1().PersistentVolumes().Create(ctx, pv, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
			p, err := s.Prepare(ctx, a)
			if err != nil {
				t.Fatal(err)
			}
			err = s.Execute(ctx, p, func(step string) error {
				cur, err := k.CoreV1().PersistentVolumes().Get(ctx, "idle-pv", metav1.GetOptions{})
				if err != nil {
					return err
				}
				mutate(cur)
				_, err = k.CoreV1().PersistentVolumes().Update(ctx, cur, metav1.UpdateOptions{})
				return err
			})
			if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) {
				t.Fatalf("stale bound PV dependency was not refused before delete: %v", err)
			}
			for _, record := range deleteRecords(k, "persistentvolumeclaims") {
				if !record.dry {
					t.Fatal("real delete issued after a changed bound PV dependency")
				}
			}
		})
	}
}

func TestPVCExpandRejectsStaleBoundPVDigest(t *testing.T) {
	s, _, k := storageService(t)
	fakeController(k, false)
	ctx := context.Background()
	doc, err := s.StorageDocument(ctx, "pvc", "alice", "home")
	if err != nil {
		t.Fatal(err)
	}
	raw, uid, rv := projectionEdit(t, doc, func(p *storageProjection) {
		p.Spec.Resources.Requests.Storage = "20Gi"
	})
	a := model.Action{Kind: "pvc.edit", Namespace: "alice", Name: "home", StorageYAML: raw, TargetUID: uid, TargetResourceVersion: rv}
	p, err := s.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(p.Data), "existing-home") || strings.Contains(string(p.Data), "backend") {
		t.Fatalf("edit evidence persisted a backend value: %s", p.Data)
	}
	err = s.Execute(ctx, p, func(step string) error {
		cur, err := k.CoreV1().PersistentVolumes().Get(ctx, "home", metav1.GetOptions{})
		if err != nil {
			return err
		}
		cur.Spec.CSI.VolumeHandle = "changed-backend-handle"
		_, err = k.CoreV1().PersistentVolumes().Update(ctx, cur, metav1.UpdateOptions{})
		return err
	})
	if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("stale bound PV source was not refused before expansion: %v", err)
	}
	for _, action := range k.Actions() {
		if action.GetVerb() != "patch" || action.GetResource().Resource != "persistentvolumeclaims" {
			continue
		}
		pa, ok := action.(ktesting.PatchActionImpl)
		if ok && len(pa.GetPatchOptions().DryRun) == 0 {
			t.Fatal("real expansion patch issued after a changed bound PV")
		}
	}
}

func TestPVCDeleteRejectsProfileIdentityDrift(t *testing.T) {
	s, api, k := storageService(t)
	ctx := context.Background()
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, pendingPVC("alice", "unused", "unused-uid"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "unused", TargetUID: "unused-uid", TargetResourceVersion: "1", ConfirmName: "unused", Reason: "verified unused"}
	p, err := s.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Execute(ctx, p, func(step string) error {
		profile := api.get("profiles", "", "alice")
		if profile == nil {
			return errors.New("profile fixture missing")
		}
		profile.SetUID("profile-2")
		api.put("profiles", profile)
		ns, err := k.CoreV1().Namespaces().Get(ctx, "alice", metav1.GetOptions{})
		if err != nil {
			return err
		}
		ns.OwnerReferences = owner("profile-2", "Profile", "alice")
		_, err = k.CoreV1().Namespaces().Update(ctx, ns, metav1.UpdateOptions{})
		return err
	})
	if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("changed Profile/namespace identity was not refused before delete: %v", err)
	}
	for _, record := range deleteRecords(k, "persistentvolumeclaims") {
		if !record.dry {
			t.Fatal("real delete issued after Profile identity drift")
		}
	}
}

func TestPVCStorageDeleteRejectsNewPodAtCheckpoint(t *testing.T) {
	s, _, k := storageService(t)
	ctx := context.Background()
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, pendingPVC("alice", "unused", "unused-uid"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "unused", TargetUID: "unused-uid", TargetResourceVersion: "1", ConfirmName: "unused", Reason: "verified unused"}
	p, err := s.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Execute(ctx, p, func(step string) error {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "late", Namespace: "alice"}, Spec: corev1.PodSpec{Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "unused"}}}}}}
		_, err := k.CoreV1().Pods("alice").Create(ctx, pod, metav1.CreateOptions{})
		return err
	})
	if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("late Pod reference should fail before mutation: %v", err)
	}
	for _, record := range deleteRecords(k, "persistentvolumeclaims") {
		if !record.dry {
			t.Fatal("real delete issued after a newly observed reference")
		}
	}
}

func TestStorageActionsRespectScopeAndClassPolicy(t *testing.T) {
	s, _, k := storageService(t)
	ctx := context.Background()
	s.Config.Policy.AllowedNamespaces = []string{"other"}
	if _, err := s.Prepare(ctx, model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "home", TargetUID: "pvc-home", TargetResourceVersion: "1", ConfirmName: "home", Reason: "test"}); err == nil {
		t.Fatal("PVC delete outside allowedNamespaces accepted")
	}
	if _, err := s.StorageDocument(ctx, "pvc", "alice", "home"); err == nil {
		t.Fatal("PVC document outside allowedNamespaces returned")
	}
	s.Config.Policy.AllowedNamespaces = nil
	if _, err := s.Prepare(ctx, model.Action{Kind: "pvc.delete", Namespace: "default", Name: "home", TargetUID: "uid", TargetResourceVersion: "1", ConfirmName: "home", Reason: "test"}); err == nil {
		t.Fatal("PVC delete in a protected namespace accepted")
	}

	otherClass := pendingPVC("alice", "other-class", "other-class-uid")
	class := "not-approved"
	otherClass.Spec.StorageClassName = &class
	if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, otherClass, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "other-class", TargetUID: "other-class-uid", TargetResourceVersion: "1", ConfirmName: "other-class", Reason: "test"}); err == nil {
		t.Fatal("PVC delete with an unapproved StorageClass accepted")
	}

	for name, mutate := range map[string]func(*corev1.PersistentVolumeClaim){
		"nil class":   func(p *corev1.PersistentVolumeClaim) { p.Spec.StorageClassName = nil },
		"empty class": func(p *corev1.PersistentVolumeClaim) { empty := ""; p.Spec.StorageClassName = &empty },
	} {
		pvc := pendingPVC("alice", "class-"+name[:3], "class-"+name[:3]+"-uid")
		mutate(pvc)
		if _, err := k.CoreV1().PersistentVolumeClaims("alice").Create(ctx, pvc, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Prepare(ctx, model.Action{Kind: "pvc.delete", Namespace: "alice", Name: pvc.Name, TargetUID: string(pvc.UID), TargetResourceVersion: "1", ConfirmName: pvc.Name, Reason: "test"}); err == nil {
			t.Fatalf("PVC delete with %s accepted", name)
		}
	}
}

func TestStorageProjectionTimeBound(t *testing.T) {
	s, _, k := storageService(t)
	ctx := context.Background()
	if _, err := k.CoreV1().PersistentVolumes().Create(ctx, freePV("free-pv", "free-uid", "7"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	doc, err := s.StorageDocument(ctx, "pv", "", "free-pv")
	if err != nil {
		t.Fatal(err)
	}
	// The document carries a stable identity and is deterministic.
	again, err := s.StorageDocument(ctx, "pv", "", "free-pv")
	if err != nil || again.YAML != doc.YAML {
		t.Fatalf("projection not deterministic: %v", err)
	}
	start := time.Now()
	if _, err := parseStorageProjection(doc.YAML); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("bounded projection parse took too long")
	}
}
