package web

import (
	"context"
	"encoding/json"
	"errors"
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
		s.cfg.Features.NodeCordon = enabled
		h := handler(s)
		for _, path := range []string{"/", "/profiles", "/namespaces", "/storage", "/notebooks", "/pods", "/nodes", "/history?tab=audit", "/operations", "/audit"} {
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

func TestProfileDeletionFeatureProjection(t *testing.T) {
	s, _, _ := fixture()
	identity, ok := s.identity(context.Background())
	if !ok {
		t.Fatal("fixture identity missing")
	}
	for _, enabled := range []bool{false, true} {
		s.cfg.Features.ProfileDeletion = enabled
		s.cfg.Features.WritesEnabled = true
		view := buildPageView(identity, s.cfg)
		if view.Config.Features.ProfileDeletion != enabled {
			t.Fatalf("deletion projection=%v for enabled=%v", view.Config.Features.ProfileDeletion, enabled)
		}
		// The capability is never projected while writes are disabled, even
		// when the separate deletion opt-in is set.
		s.cfg.Features.WritesEnabled = false
		if view = buildPageView(identity, s.cfg); view.Config.Features.ProfileDeletion {
			t.Fatal("deletion capability projected while writes are disabled")
		}
		s.cfg.Features.WritesEnabled = true
	}
	// The projection is a boolean capability only: it must not carry the rest
	// of the configuration into the browser.
	view := buildPageView(identity, s.cfg)
	if view.Config.Storage.AllowedStorageClasses != nil || len(view.Config.Notebooks.AllowedImages) != 0 {
		t.Fatal("page view carries unexpected configuration")
	}
}

func TestProfileDeletionMetaProjection(t *testing.T) {
	s, _, _ := fixture()
	// The enabled capability must be projected as true in the template.
	s.cfg.Features.ProfileDeletion = true
	s.cfg.Features.WritesEnabled = true
	body := request(handler(s), "GET", "/", "").Body.String()
	if !strings.Contains(body, `name="profile-deletion" content="true"`) {
		t.Fatal("enabled deletion capability was not projected")
	}
	// A disabled capability is never projected as true, whether the template
	// renders the flag unconditionally or omits it.
	for _, writes := range []bool{true, false} {
		s.cfg.Features.ProfileDeletion = false
		s.cfg.Features.WritesEnabled = writes
		body = request(handler(s), "GET", "/", "").Body.String()
		if strings.Contains(body, `name="profile-deletion" content="true"`) {
			t.Fatalf("deletion capability projected while disabled (writes=%v)", writes)
		}
	}
	// With writes disabled the capability must not be projected even when the
	// deletion opt-in is enabled.
	s.cfg.Features.ProfileDeletion = true
	s.cfg.Features.WritesEnabled = false
	body = request(handler(s), "GET", "/", "").Body.String()
	if strings.Contains(body, `name="profile-deletion" content="true"`) {
		t.Fatal("deletion capability projected while writes are disabled")
	}
}

func TestProfileDeletionAPIGate(t *testing.T) {
	s, _, o := fixture()
	h := handler(s)
	body := `{"kind":"profile.delete","name":"team","reason":"disposable pilot cleanup","confirmName":"team"}`
	// Flag off: refused before any plan is created.
	s.cfg.Features.ProfileDeletion = false
	if w := request(h, "POST", "/api/v1/plans", body); w.Code != 403 {
		t.Fatalf("deletion without the opt-in must be refused: %d", w.Code)
	}
	if o.plans != 0 {
		t.Fatalf("refused deletion reached the plan manager: %d", o.plans)
	}
	// Flag on, writes off: refused by the generic writes gate.
	s.cfg.Features.ProfileDeletion = true
	s.cfg.Features.WritesEnabled = false
	if w := request(h, "POST", "/api/v1/plans", body); w.Code != 403 {
		t.Fatalf("deletion with writes disabled must be refused: %d", w.Code)
	}
	if o.plans != 0 {
		t.Fatalf("refused deletion reached the plan manager: %d", o.plans)
	}
	// Flag on and writes on: the typed action reaches the plan manager with
	// the typed confirmation intact.
	s.cfg.Features.WritesEnabled = true
	if w := request(h, "POST", "/api/v1/plans", body); w.Code != 201 {
		t.Fatalf("gated deletion was not accepted: %d %s", w.Code, w.Body.String())
	}
	if o.plans != 1 || o.action.Kind != "profile.delete" || o.action.ConfirmName != "team" {
		t.Fatalf("typed confirmation did not reach the manager: %+v", o.action)
	}
	// No arbitrary delete endpoint exists: deletion is only reachable through
	// the typed plan/operation pipeline with the reviewed opt-in.
	for _, path := range []string{"/api/v1/delete", "/api/v1/profiles/delete", "/api/v1/namespaces/delete"} {
		if w := request(h, "POST", path, body); w.Code != 404 {
			t.Fatalf("unexpected delete route %s: %d", path, w.Code)
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

func TestNodeCordonValidationMessagesAreAllowlisted(t *testing.T) {
	for _, msg := range []string{
		"node cordon is disabled",
		"a nonempty reason is required for node cordon",
		"Node has no stable UID",
		"Node is terminating",
		"Node namespace must be empty",
		"Node is already cordoned",
		"Node is already schedulable",
		"control-plane Node requires force to cordon",
		"NotReady Node requires force to cordon",
	} {
		w := httptest.NewRecorder()
		backendFailure(w, errors.New(msg), true)
		if w.Code != 422 || !strings.Contains(w.Body.String(), msg) {
			t.Fatalf("message %q was not allowlisted as 422 validation_failed: %d %s", msg, w.Code, w.Body.String())
		}
	}
}
