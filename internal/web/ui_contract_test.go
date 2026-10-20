package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"kubeflow-admin/internal/store"
)

func TestDashboardRoutesAndFeatureProjection(t *testing.T) {
	s, _, _ := fixture()
	for _, enabled := range []bool{false, true} {
		s.cfg.Features.PodRestarts = enabled
		h := handler(s)
		for _, path := range []string{"/", "/profiles", "/storage", "/notebooks", "/pods", "/history?tab=audit", "/operations", "/audit"} {
			response := request(h, "GET", path, "")
			if response.Code != 200 {
				t.Fatalf("feature=%v route=%s status=%d", enabled, path, response.Code)
			}
			if response.Header().Get("Referrer-Policy") != "same-origin" {
				t.Fatal("native logout form origin protection regressed")
			}
			if !strings.Contains(response.Body.String(), `name="notification-scope"`) || !strings.Contains(response.Body.String(), `name="pod-restarts"`) {
				t.Fatal("missing safe UI feature/session projections")
			}
		}
	}
}

func TestHistoryFailuresAreActionableAndDoNotLeakBackendText(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{&store.BudgetError{Reason: "PRIVATE_BACKEND_DETAIL"}, 503, "history_capacity_exceeded"},
		{&store.IntegrityError{Reason: "PRIVATE_BACKEND_DETAIL"}, 503, "operation_integrity_error"},
		{store.ErrInvalidTarget, 422, "invalid_target"},
	} {
		w := httptest.NewRecorder()
		backendFailure(w, tc.err, true)
		var out struct {
			Error apiError `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if w.Code != tc.status || out.Error.Code != tc.code || strings.Contains(w.Body.String(), "PRIVATE_BACKEND_DETAIL") {
			t.Fatalf("unsafe error classification: %d %s", w.Code, w.Body.String())
		}
	}
}

func TestStaticAssetsRevalidateAcrossReleases(t *testing.T) {
	s, _, _ := fixture()
	h := handler(s)
	for _, asset := range []string{"app.js", "logic.js", "style.css"} {
		url := "/static/" + asset
		first := request(h, "GET", url, "")
		etag := first.Header().Get("ETag")
		if first.Code != 200 || first.Header().Get("Cache-Control") != "no-cache" || etag == "" {
			t.Fatalf("asset must revalidate: %s", asset)
		}
		r := httptest.NewRequest(http.MethodGet, url, nil)
		r.Header.Set("If-None-Match", etag)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
			t.Fatalf("unchanged asset should return 304: %s status=%d", asset, w.Code)
		}
	}
}

func TestHistoryWindowMetadata(t *testing.T) {
	s, _, _ := fixture()
	s.cfg.History.RetentionDays = 60
	for _, path := range []string{"/api/v1/operations", "/api/v1/audit"} {
		w := request(handler(s), "GET", path, "")
		if w.Code != 200 || w.Header().Get("X-History-Limit") != "1000" || w.Header().Get("X-History-Retention-Days") != "60" || w.Header().Get("X-History-Window") == "" {
			t.Fatalf("missing bounded history contract: %s %v", path, w.Header())
		}
	}
}
