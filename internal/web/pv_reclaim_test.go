package web

import (
	"encoding/json"
	"errors"
	"kubeflow-admin/internal/model"
	"strings"
	"testing"
)

func TestReclaimPolicyRefusalsAreNonRetryableAtPlanAndDocumentBoundaries(t *testing.T) {
	for _, message := range []string{
		"the deleted PersistentVolumeClaim reappeared; reclaim stopped for review",
		"a PersistentVolumeClaim references the retained PersistentVolume; reclaim stopped for review",
		"a replacement PersistentVolumeClaim with the deleted claim's namespace and name exists and could bind the retained PersistentVolume; reclaim stopped for review",
		"an unbound PersistentVolumeClaim can bind the retained PersistentVolume; reclaim stopped for review",
	} {
		s, _, ops := fixture()
		s.cfg.Features.PVReclaim = true
		s.cfg.Features.StorageDeletion = true
		ops.err = errors.New(message)
		s.kube = &fakeStorageKube{fakeKube: &fakeKube{}, documentErr: errors.New(message)}
		for _, route := range []struct{ method, path, body string }{
			{"POST", "/api/v1/plans", `{"kind":"pv.reclaim","name":"old-pv","targetUID":"uid","targetResourceVersion":"7","confirmName":"old-pv","reason":"reviewed"}`},
			{"GET", "/api/v1/storage-document?kind=pv.reclaim&name=old-pv", ""},
		} {
			w := request(handler(s), route.method, route.path, route.body)
			var response struct {
				Error struct {
					Code      string `json:"code"`
					Retryable bool   `json:"retryable"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if w.Code != 422 || response.Error.Retryable || response.Error.Code != "validation_failed" {
				t.Fatalf("policy refusal became retryable/unavailable: %s %d %s", route.path, w.Code, w.Body.String())
			}
		}
	}
}

func TestStandalonePVReclaimHTTPGatesAndDocumentDispatch(t *testing.T) {
	s, _, ops := fixture()
	s.cfg.Features.StorageEditing = false
	s.cfg.Features.StorageDeletion = true
	body := `{"kind":"pv.reclaim","name":"old-pv","targetUID":"pv-uid","targetResourceVersion":"7","confirmName":"old-pv","reason":"reviewed retention"}`
	docPath := "/api/v1/storage-document?kind=pv.reclaim&name=old-pv"
	if w := request(handler(s), "POST", "/api/v1/plans", body); w.Code != 403 || ops.plans != 0 {
		t.Fatal("disabled reclaim reached plan manager", w.Code)
	}
	if w := request(handler(s), "GET", docPath, ""); w.Code != 403 {
		t.Fatal("disabled reclaim document allowed", w.Code)
	}
	s.cfg.Features.PVReclaim = true
	k := &fakeStorageKube{fakeKube: &fakeKube{}, document: model.StorageDocument{Kind: "PersistentVolume", Name: "old-pv", UID: "pv-uid", ResourceVersion: "7", YAML: "# No editable YAML\n"}}
	s.kube = k
	if w := request(handler(s), "GET", docPath, ""); w.Code != 200 || k.documentKind != "pv.reclaim" || k.documentName != "old-pv" {
		t.Fatalf("reclaim read fell back to Available-only document: %d %+v", w.Code, k)
	}
	if w := request(handler(s), "GET", docPath+"&namespace=alice", ""); w.Code != 400 {
		t.Fatal("namespaced PV reclaim allowed", w.Code)
	}
	if w := request(handler(s), "POST", "/api/v1/plans", body); w.Code != 201 || ops.action.Kind != "pv.reclaim" || ops.action.ConfirmName != "old-pv" {
		t.Fatal("enabled typed reclaim failed", w.Code, ops.action)
	}
	page := request(handler(s), "GET", "/storage", "").Body.String()
	if !strings.Contains(page, `id="storage-reclaim-drawer" data-enabled="true"`) {
		t.Fatal("reclaim capability missing")
	}
	s.cfg.Features.WritesEnabled = false
	if w := request(handler(s), "POST", "/api/v1/plans", body); w.Code != 403 {
		t.Fatal("reclaim bypassed writes gate", w.Code)
	}
}
