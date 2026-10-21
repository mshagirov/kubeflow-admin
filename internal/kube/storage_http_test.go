package kube

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
)

// storageHTTPAPI is a real HTTP API fixture for one unused PersistentVolume and
// one approved StorageClass. It proves the request shape (UID/resourceVersion
// preconditions, dry run and exactly one real DELETE) without a cluster.
type storageHTTPAPI struct {
	mu           sync.Mutex
	deleted      bool
	dryDeletes   int
	realDeletes  int
	failReal     int
	deleteBodies []metav1.DeleteOptions
	requests     []string
}

func (a *storageHTTPAPI) status(w http.ResponseWriter, code int, reason metav1.StatusReason) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Failure", Code: int32(code), Reason: reason})
}

func (a *storageHTTPAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	a.requests = append(a.requests, r.Method+" "+r.URL.RequestURI()+" "+string(body))
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/apis/storage.k8s.io/v1/storageclasses/approved":
		_ = json.NewEncoder(w).Encode(storagev1.StorageClass{
			TypeMeta:    metav1.TypeMeta{APIVersion: "storage.k8s.io/v1", Kind: "StorageClass"},
			ObjectMeta:  metav1.ObjectMeta{Name: "approved", UID: "class-uid", ResourceVersion: "4"},
			Provisioner: "csi.example.vendor",
		})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/persistentvolumes/free-pv":
		if a.deleted {
			a.status(w, http.StatusNotFound, metav1.StatusReasonNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(corev1.PersistentVolume{
			TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolume"},
			ObjectMeta: metav1.ObjectMeta{Name: "free-pv", UID: "free-uid", ResourceVersion: "7"},
			Spec: corev1.PersistentVolumeSpec{
				StorageClassName:              "approved",
				PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
				Capacity:                      corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
				AccessModes:                   []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
				PersistentVolumeSource:        corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{Driver: "csi.example.vendor", VolumeHandle: "hidden"}},
			},
			Status: corev1.PersistentVolumeStatus{Phase: corev1.VolumeAvailable},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/persistentvolumeclaims":
		_ = json.NewEncoder(w).Encode(corev1.PersistentVolumeClaimList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PersistentVolumeClaimList"}})
	case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/persistentvolumes/free-pv":
		var opts metav1.DeleteOptions
		if err := json.Unmarshal(body, &opts); err != nil {
			a.status(w, http.StatusBadRequest, metav1.StatusReasonBadRequest)
			return
		}
		if len(opts.DryRun) > 0 {
			a.dryDeletes++
		} else {
			a.realDeletes++
			if a.failReal > 0 {
				a.failReal--
				w.Header().Set("Retry-After", "1")
				a.status(w, http.StatusInternalServerError, metav1.StatusReasonInternalError)
				return
			}
			a.deleteBodies = append(a.deleteBodies, opts)
			a.deleted = true
		}
		_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: "Success", Code: 200})
	default:
		a.status(w, http.StatusNotFound, metav1.StatusReasonNotFound)
	}
}

func TestStorageDeleteRealHTTPOneConditionedWrite(t *testing.T) {
	api := &storageHTTPAPI{}
	server := httptest.NewServer(api)
	defer server.Close()
	restConfig := &rest.Config{Host: server.URL, QPS: 100, Burst: 100}
	restConfig.ContentType = "application/json"
	restConfig.AcceptContentTypes = "application/json"
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{
		Config: config.Config{Features: config.Features{WritesEnabled: true, StorageDeletion: true}, Storage: config.Storage{AllowedStorageClasses: []string{"approved"}}},
		Typed:  client, PollInterval: time.Millisecond,
	}
	ctx := context.Background()
	a := model.Action{Kind: "pv.delete", Name: "free-pv", TargetUID: "free-uid", TargetResourceVersion: "7", ConfirmName: "free-pv", Reason: "verified unused"}
	p, err := s.Prepare(ctx, a)
	if err != nil {
		api.mu.Lock()
		t.Logf("requests: %v", api.requests)
		api.mu.Unlock()
		t.Fatal(err)
	}
	if err := s.Execute(ctx, p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.realDeletes != 1 || len(api.deleteBodies) != 1 {
		t.Fatalf("expected exactly one real delete, got %d", api.realDeletes)
	}
	if api.dryDeletes < 2 {
		t.Fatalf("expected a dry run in prepare and execution, got %d", api.dryDeletes)
	}
	opts := api.deleteBodies[0]
	if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != types.UID("free-uid") || opts.Preconditions.ResourceVersion == nil || *opts.Preconditions.ResourceVersion != "7" {
		t.Fatalf("delete did not carry exact UID/resourceVersion preconditions: %+v", opts.Preconditions)
	}
	if opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationBackground {
		t.Fatalf("unexpected propagation policy: %+v", opts.PropagationPolicy)
	}
	if strings.Contains(string(mustJSON(t, opts)), "force") {
		t.Fatal("delete options carried an unsupported force field")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestStorageDeleteRealHTTPAmbiguous5xxIsNotReplayed(t *testing.T) {
	api := &storageHTTPAPI{failReal: 1}
	server := httptest.NewServer(api)
	defer server.Close()
	restConfig := &rest.Config{Host: server.URL, QPS: 100, Burst: 100}
	restConfig.ContentType = "application/json"
	restConfig.AcceptContentTypes = "application/json"
	restConfig.WrapTransport = disableWriteAutoRetry(nil)
	client, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{
		Config: config.Config{Features: config.Features{WritesEnabled: true, StorageDeletion: true}, Storage: config.Storage{AllowedStorageClasses: []string{"approved"}}},
		Typed:  client, PollInterval: time.Millisecond,
	}
	ctx := context.Background()
	a := model.Action{Kind: "pv.delete", Name: "free-pv", TargetUID: "free-uid", TargetResourceVersion: "7", ConfirmName: "free-pv", Reason: "verified unused"}
	p, err := s.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Execute(ctx, p, func(string) error { return nil })
	if err == nil || errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("a 5xx outcome must be ambiguous and stay needs-review: %v", err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	if api.realDeletes != 1 {
		t.Fatalf("the ambiguous delete was replayed: %d real attempts", api.realDeletes)
	}
	if api.deleted {
		t.Fatal("fixture applied a delete after the scripted 5xx")
	}
}
