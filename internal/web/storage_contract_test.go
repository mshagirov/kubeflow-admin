package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
)

// These are exact, static messages authored by internal/kube/storage_edit.go.
// Only these reviewed strings may surface as validation errors; a prefix, a
// regex or arbitrary backend text stays a generic 503.
var reviewedStoragePolicyMessages = []string{
	"storage YAML is invalid; use the server-provided projection and approved fields only",
	"PersistentVolume is cluster scoped; namespace must be empty",
	"PersistentVolumeClaim has no stable UID/resourceVersion",
	"PersistentVolumeClaim is terminating",
	"an existing kubeflow-admin.io/ label is invalid; an operator must correct it before editing",
	"only valid labels under the kubeflow-admin.io/ prefix are editable",
	"too many kubeflow-admin.io/ labels",
	"kubeflow-admin.io/ labels exceed the allowed size",
	"storage projection exceeds the allowed size",
	"unsupported storage kind",
	"unsupported storage action kind",
	"storage action accepts no other fields",
	"storage editing does not accept force or a delete confirmation",
	"storage editing requires the exact observed targetUID and targetResourceVersion",
	"a bounded storage projection is required",
	"storage deletion does not accept force or an edit document",
	"storage deletion requires the exact observed targetUID and targetResourceVersion",
	"a nonempty reason of at most 500 characters is required for storage deletion",
	"typed confirmation must exactly match the storage object name",
	"storage projection does not match the action target",
	"PersistentVolume projection must not carry a namespace",
	"PersistentVolume editing accepts only the reclaim policy and console-owned labels",
	"PersistentVolume reclaim policy must be Retain or Delete",
	"no approved PersistentVolume change was requested",
	"PersistentVolumeClaim projection namespace does not match the action",
	"PersistentVolumeClaim editing does not accept a reclaim policy",
	"spec.resources.requests.storage must be a positive quantity when present",
	"PersistentVolumeClaim has no observed storage request to compare",
	"PersistentVolumeClaim storage cannot be decreased",
	"storage increase requires a bound nonterminating PVC with explicit class and volume",
	"storage increase requires a verified CSI binding and matching driver",
	"no approved PersistentVolumeClaim change was requested",
	"PersistentVolume has no stable UID/resourceVersion",
	"PersistentVolume is terminating",
	"PersistentVolume must be Available; bound, released, pending or failed volumes are refused",
	"PersistentVolume is reserved or bound by a claimRef",
	"PersistentVolume is referenced by a PersistentVolumeClaim",
	"controller-owned PersistentVolumeClaims are refused to prevent automatic recreation",
	"bound PersistentVolumeClaim has no volumeName",
	"a pending PersistentVolumeClaim with a volumeName is binding and cannot be deleted here",
	"PersistentVolumeClaim must be Bound or Pending; lost or unknown claims are refused",
	"bound PersistentVolume is not a stable Bound volume",
	"bound PersistentVolume does not name this exact claim",
	"bound PersistentVolume StorageClass is not approved",
	"bound PersistentVolume reclaim policy is unknown",
	"PersistentVolumeClaim is reserved by a PersistentVolume",
	"bound PersistentVolumeClaim does not have exactly one matching PersistentVolume reservation",
	"PersistentVolumeClaim is referenced by an observed Pod",
	"PersistentVolumeClaim collides with an observed inline ephemeral claim",
	"PersistentVolumeClaim is referenced by an observed Notebook",
	"storage deletion requires UID and resourceVersion preconditions",
	"namespace is outside configured scope",
	"protected namespace",
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder) apiError {
	t.Helper()
	var response struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response.Error
}

func TestReviewedStoragePolicyMessagesAreExactValidation(t *testing.T) {
	for _, message := range reviewedStoragePolicyMessages {
		t.Run(message, func(t *testing.T) {
			w := httptest.NewRecorder()
			backendFailure(w, errors.New(message), true)
			response := decodeError(t, w)
			if w.Code != http.StatusUnprocessableEntity || response.Code != "validation_failed" || response.Retryable {
				t.Fatalf("reviewed storage message was not mapped: %d %+v", w.Code, response)
			}
			if response.Message != message {
				t.Fatalf("reviewed storage message was rewritten: %q", response.Message)
			}
		})
	}
}

func TestStorageClusterAndUnknownFailuresStaySafe(t *testing.T) {
	for _, test := range []struct {
		message   string
		status    int
		code      string
		retryable bool
	}{
		{"Kubernetes API denied the storage request", 503, "cluster_access_denied", false},
		{"Kubernetes API rejected the storage request", 422, "cluster_validation_failed", false},
		{"Kubernetes API storage request failed", 503, "service_unavailable", true},
	} {
		w := httptest.NewRecorder()
		backendFailure(w, errors.New(test.message), true)
		response := decodeError(t, w)
		if w.Code != test.status || response.Code != test.code || response.Retryable != test.retryable {
			t.Fatalf("%s: %d %+v", test.message, w.Code, response)
		}
	}
	// A message is only approved by an exact match. Raw input, a CSI handle, a
	// backend path or a Secret value in an otherwise similar message stays
	// generic and must never be echoed.
	for _, message := range []string{
		"storage YAML is invalid: csi volumeHandle secret-value /private/nfs/export",
		"PersistentVolume is referenced by a PersistentVolumeClaim /private/nfs/export",
		"raw CSI volumeHandle secret-value token=abc",
		"storage projection failed: data: secret-value",
	} {
		w := httptest.NewRecorder()
		backendFailure(w, errors.New(message), true)
		response := decodeError(t, w)
		if w.Code != 503 || response.Code != "service_unavailable" || !response.Retryable {
			t.Fatalf("unknown storage failure was not generic: %d %+v", w.Code, response)
		}
		for _, secret := range []string{"secret-value", "/private/nfs/export", "token=abc"} {
			if strings.Contains(w.Body.String(), secret) {
				t.Fatalf("unknown storage failure leaked %q: %s", secret, w.Body.String())
			}
		}
	}
}

func TestStorageDocumentGETUsesReviewedValidationOnly(t *testing.T) {
	newServer := func(err error) *server {
		k := &fakeStorageKube{fakeKube: &fakeKube{}, documentErr: err}
		return &server{
			cfg:  config.Config{Features: config.Features{WritesEnabled: true, StorageEditing: true, StorageDeletion: true}},
			kube: k, ops: &fakeOps{}, ping: func(context.Context) error { return nil },
			identity: func(context.Context) (model.Identity, bool) { return model.Identity{CSRF: "csrf-test"}, true },
		}
	}
	exact := errors.New("PersistentVolume must be Available; bound, released, pending or failed volumes are refused")
	w := request(handler(newServer(exact)), "GET", "/api/v1/storage-document?kind=PersistentVolume&name=data", "")
	response := decodeError(t, w)
	if w.Code != 422 || response.Code != "validation_failed" || response.Retryable || response.Message != exact.Error() {
		t.Fatalf("reviewed GET refusal was not mapped: %d %+v", w.Code, response)
	}

	unknown := errors.New("raw CSI volumeHandle secret-value /private/nfs/export")
	w = request(handler(newServer(unknown)), "GET", "/api/v1/storage-document?kind=PersistentVolume&name=data", "")
	response = decodeError(t, w)
	if w.Code != 503 || response.Code != "service_unavailable" || !response.Retryable {
		t.Fatalf("unknown GET failure was not generic: %d %+v", w.Code, response)
	}
	if strings.Contains(w.Body.String(), "secret-value") || strings.Contains(w.Body.String(), "/private/nfs/export") {
		t.Fatalf("unknown GET failure leaked backend text: %s", w.Body.String())
	}
}
