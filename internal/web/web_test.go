package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubeflow-admin/internal/auth"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/kube"
	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/operations"
	"kubeflow-admin/internal/store"
)

type fakeKube struct {
	result model.Inventory
	err    error
}

func (k *fakeKube) Inventory(context.Context) (model.Inventory, error) { return k.result, k.err }
func (k *fakeKube) Check(context.Context) error                        { return k.err }

type fakeOps struct {
	action         model.Action
	plans, submits int
	key, planID    string
	err            error
	changes        []model.Change
}

func (o *fakeOps) Plan(_ context.Context, _ model.Identity, a model.Action) (model.Plan, error) {
	o.plans++
	o.action = a
	return model.Plan{ID: "server-plan", Prepared: model.Prepared{Action: a, Summary: "server-owned preview", Changes: o.changes, Warnings: []string{"Review interruption risk"}}}, o.err
}
func (o *fakeOps) Submit(_ context.Context, _ model.Identity, id, key string) (model.Operation, error) {
	o.submits++
	o.key = key
	o.planID = id
	return model.Operation{ID: "operation", PlanID: id, State: "queued"}, o.err
}
func (o *fakeOps) Get(context.Context, string) (model.Operation, error) {
	return model.Operation{}, o.err
}
func (o *fakeOps) List(context.Context) ([]model.Operation, error) { return []model.Operation{}, o.err }
func (o *fakeOps) Audit(context.Context) ([]model.Audit, error)    { return []model.Audit{}, o.err }
func fixture() (*server, *fakeKube, *fakeOps) {
	k := &fakeKube{result: model.Inventory{ObservedAt: time.Now(), Resources: []model.Resource{{Kind: "Profile", Name: "team"}, {Kind: "Notebook", Name: "work", Namespace: "team"}, {Kind: "Notebook", Name: "other", Namespace: "other"}}}}
	o := &fakeOps{}
	s := &server{cfg: config.Config{Features: config.Features{WritesEnabled: true}}, kube: k, ops: o, ping: func(context.Context) error { return nil }, identity: func(context.Context) (model.Identity, bool) {
		return model.Identity{Email: "admin@example.org", CSRF: "csrf-test"}, true
	}}
	return s, k, o
}
func handler(s *server) http.Handler {
	noop := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }
	return s.routes(func(h http.Handler) http.Handler { return h }, noop, noop, noop)
}
func request(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestRouteProtectionAndPublicRoutes(t *testing.T) {
	s, _, _ := fixture()
	blocked := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	}
	noop := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }
	h := s.routes(blocked, noop, noop, noop)
	for _, path := range []string{"/", "/profiles", "/storage", "/notebooks", "/operations", "/audit", "/api/v1/me", "/api/v1/inventory", "/api/v1/plans", "/api/v1/operations", "/api/v1/audit", "/unknown", "/logout", "/auth/logout"} {
		method := "GET"
		if path == "/api/v1/plans" || path == "/logout" || path == "/auth/logout" {
			method = "POST"
		}
		if got := request(h, method, path, "").Code; got != 401 {
			t.Errorf("%s: got %d", path, got)
		}
	}
	for _, path := range []string{"/healthz", "/readyz", "/static/app.js", "/static/logic.js", "/static/style.css", "/login", "/callback", "/auth/signed-out", "/signed-out"} {
		if got := request(h, "GET", path, "").Code; got != 200 && got != 204 {
			t.Errorf("public %s: %d", path, got)
		}
	}
}
func TestStrictPlanDecodeAndNoWrites(t *testing.T) {
	s, _, o := fixture()
	h := handler(s)
	for _, body := range []string{`{"kind":"profile.create","name":"team","manifest":{}}`, `{"kind":"profile.create"} {}`, `null`, `{"kind":`, `{"reason":"` + strings.Repeat("x", 70<<10) + `"}`} {
		w := request(h, "POST", "/api/v1/plans", body)
		if w.Code != 400 {
			t.Errorf("invalid request got %d", w.Code)
		}
	}
	// Unknown fields and trailing objects must never reach the manager.
	if o.plans != 0 {
		t.Fatalf("unexpected plans: %d", o.plans)
	}
	s.cfg.Features.WritesEnabled = false
	if w := request(h, "POST", "/api/v1/plans", `{"kind":"profile.create","name":"team"}`); w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestPlanIsPreparedNotExecuted(t *testing.T) {
	s, _, o := fixture()
	w := request(handler(s), "POST", "/api/v1/plans", `{"kind":"notebook.edit","namespace":"team","name":"work","container":"main","cpuRequest":"500m","mounts":[]}`)
	if w.Code != 201 {
		t.Fatal(w.Code, w.Body.String())
	}
	if o.submits != 0 || o.plans != 1 || o.action.Container != "main" || o.action.Mounts == nil {
		t.Fatalf("typed action not preserved: %+v", o)
	}
	var p model.Plan
	if e := json.Unmarshal(w.Body.Bytes(), &p); e != nil {
		t.Fatal(e)
	}
	if p.ID != "server-plan" || p.Prepared.Summary != "server-owned preview" {
		t.Fatal(p)
	}
}
func TestSubmitOnlyPlanAndIdempotencyKey(t *testing.T) {
	s, _, o := fixture()
	h := handler(s)
	for _, body := range []string{`{"planID":"server-plan"}`, `{"planID":"server-plan","idempotencyKey":"1234567890123456","action":{"kind":"notebook.stop"}}`} {
		if w := request(h, "POST", "/api/v1/operations", body); w.Code != 400 {
			t.Fatal(w.Code)
		}
	}
	if o.submits != 0 {
		t.Fatal("invalid submission reached manager")
	}
	w := request(h, "POST", "/api/v1/operations", `{"planID":"server-plan","idempotencyKey":"1234567890123456"}`)
	if w.Code != 202 || o.key != "1234567890123456" || o.planID != "server-plan" {
		t.Fatal(w.Code, o)
	}
}
func TestInventoryFilterAndSafeErrors(t *testing.T) {
	s, k, o := fixture()
	h := handler(s)
	w := request(h, "GET", "/api/v1/notebooks?namespace=team", "")
	var inv model.Inventory
	if e := json.Unmarshal(w.Body.Bytes(), &inv); e != nil {
		t.Fatal(e)
	}
	if len(inv.Resources) != 1 || inv.Resources[0].Name != "work" || inv.ObservedAt.IsZero() {
		t.Fatal(inv)
	}
	k.err = errors.New("Bearer sensitive-secret kubeconfig /private/secret")
	o.err = k.err
	for _, path := range []string{"/readyz", "/api/v1/inventory", "/api/v1/operations", "/api/v1/audit"} {
		w = request(h, "GET", path, "")
		if w.Code < 400 || strings.Contains(w.Body.String(), "sensitive-secret") {
			t.Fatalf("unsafe error %s: %s", path, w.Body.String())
		}
	}
}
func TestEmbeddedPagesAndSecurityHeaders(t *testing.T) {
	s, _, _ := fixture()
	s.identity = func(context.Context) (model.Identity, bool) {
		return model.Identity{Email: `<script>alert(1)</script>`, CSRF: `"><script>bad</script>`}, true
	}
	h := handler(s)
	for _, path := range []string{"/", "/profiles", "/storage", "/notebooks", "/operations", "/audit"} {
		w := request(h, "GET", path, "")
		if w.Code != 200 {
			t.Fatal(path, w.Code)
		}
		if strings.Contains(w.Body.String(), "<script>alert") || strings.Contains(w.Body.String(), "<script>bad") {
			t.Fatal("identity was not escaped")
		}
		if !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Header())
		}
	}
	w := request(h, "GET", "/static/app.js", "")
	if !strings.Contains(w.Body.String(), "crypto.randomUUID()") || strings.Contains(w.Body.String(), "innerHTML") {
		t.Fatal("unsafe or missing local JS")
	}
}

func TestApprovedTemplatesExposeNamesOnly(t *testing.T) {
	s, _, _ := fixture()
	s.cfg.Storage.CSITemplates = []config.CSITemplate{{Name: "approved-csi", Driver: "private-driver", VolumeHandle: "secret-volume-handle", VolumeAttributes: map[string]string{"private": "secret-attribute"}}}
	s.cfg.Storage.NFSTemplates = []config.NFSTemplate{{Name: "approved-nfs", Server: "private-nfs-server", BasePath: "/private/backend"}}
	w := request(handler(s), "GET", "/storage", "")
	for _, text := range []string{"approved-csi", "approved-nfs", "Expansion disabled by policy", "Static storage disabled by policy"} {
		if !strings.Contains(w.Body.String(), text) {
			t.Errorf("missing %q", text)
		}
	}
	for _, text := range []string{"secret-volume-handle", "private-driver", "secret-attribute", "private-nfs-server", "/private/backend"} {
		if strings.Contains(w.Body.String(), text) {
			t.Errorf("leaked %q", text)
		}
	}
}

func TestJSONContentTypeAndHeaderKeyAgreement(t *testing.T) {
	s, _, o := fixture()
	h := handler(s)
	r := httptest.NewRequest("POST", "/api/v1/plans", strings.NewReader(`{"kind":"profile.create","name":"team"}`))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnsupportedMediaType || o.plans != 0 {
		t.Fatal(w.Code, o.plans)
	}
	r = httptest.NewRequest("POST", "/api/v1/operations", strings.NewReader(`{"planID":"plan","idempotencyKey":"abcdefghijklmnop"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Idempotency-Key", "different-key-1234")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || o.submits != 0 {
		t.Fatal(w.Code, o.submits)
	}
}

func TestStructuredBackendErrors(t *testing.T) {
	for _, test := range []struct {
		name          string
		err           error
		status        int
		code, message string
		retryable     bool
	}{
		{"stale", fmt.Errorf("stale: %w", kube.ErrConflict), 409, "stale_plan", "prepare a new plan", false},
		{"locked", store.ErrConflict, 409, "operation_conflict", "Check Operations", false},
		{"missing", fmt.Errorf("lookup: %w", store.ErrNotFound), 404, "not_found", "not found", false},
		{"quota", errors.New("Notebook resource total exceeds namespace quota"), 422, "validation_failed", "exceeds namespace quota", false},
		{"operator-managed quota", errors.New("changing an unsupported quota key is unsupported"), 422, "validation_failed", "unsupported quota key", false},
		{"gpu", errors.New("approved GPU preset required"), 422, "validation_failed", "approved GPU preset required", false},
		{"mount", errors.New("protected workspace mount cannot be removed"), 422, "validation_failed", "cannot be removed", false},
		{"storage", errors.New("template class/access mismatch"), 422, "validation_failed", "template class/access mismatch", false},
		{"rbac", errors.New("Kubernetes API denied the request"), 503, "cluster_access_denied", "RBAC", false},
		{"missing cluster target", errors.New("Kubernetes resource not found"), 404, "resource_not_found", "name and namespace", false},
		{"admission", errors.New("Kubernetes API rejected validation or conditional patch"), 422, "cluster_validation_failed", "admission policy", false},
		{"timeout", context.DeadlineExceeded, 503, "request_timeout", "same idempotency key", true},
		{"queue", errors.New("operation queue full; retry later"), 503, "queue_full", "queue is full", true},
		{"expired", errors.New("plan expired; preview again"), 409, "plan_expired", "expired", false},
		{"session invalid", auth.ErrSessionInvalid, 403, "reauthentication_required", "Sign in again", false},
		{"session invalid wrapped", fmt.Errorf("authorize: %w", auth.ErrSessionInvalid), 403, "reauthentication_required", "Sign in again", false},
		{"sql", errors.New("SQLITE_BUSY: SELECT secret FROM records /private/internal.db"), 503, "service_unavailable", "sanitized diagnostics", true},
		{"unexpected", errors.New("unexpected internal error secret-value"), 503, "service_unavailable", "sanitized diagnostics", true},
		{"token appended", errors.New("protected namespace Bearer secret-value"), 503, "service_unavailable", "sanitized diagnostics", true},
		{"unbounded", errors.New(strings.Repeat("x", 10000)), 503, "service_unavailable", "sanitized diagnostics", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, _, o := fixture()
			o.err = test.err
			w := request(handler(s), "POST", "/api/v1/plans", `{"kind":"notebook.edit","namespace":"team","name":"work"}`)
			var response struct {
				Error apiError `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if w.Code != test.status || response.Error.Code != test.code || response.Error.Retryable != test.retryable || !strings.Contains(response.Error.Message, test.message) {
				t.Fatalf("%d: %+v", w.Code, response)
			}
			if len(response.Error.Message) > 700 || strings.Contains(w.Body.String(), "secret-value") || strings.Contains(w.Body.String(), "SELECT secret") || strings.Contains(w.Body.String(), "/private/internal.db") {
				t.Fatal("unsafe response", w.Body.String())
			}
		})
	}
}

func TestOperationLookupDoesNotMaskStorageFailureAsNotFound(t *testing.T) {
	s, _, o := fixture()
	o.err = errors.New("SQLITE_CORRUPT private database")
	w := request(handler(s), "GET", "/api/v1/operations/id", "")
	if w.Code != 503 || strings.Contains(w.Body.String(), "SQLITE") {
		t.Fatal(w.Code, w.Body.String())
	}
	o.err = store.ErrNotFound
	if w = request(handler(s), "GET", "/api/v1/operations/id", ""); w.Code != 404 {
		t.Fatal(w.Code)
	}
}

func TestStorageNamespaceKeepsRelatedClusterResources(t *testing.T) {
	s, k, _ := fixture()
	k.result.Resources = append(k.result.Resources,
		model.Resource{Kind: "PersistentVolumeClaim", Name: "data", Namespace: "team", Details: map[string]string{"volume": "bound-team", "storageClass": "team-class"}},
		model.Resource{Kind: "PersistentVolume", Name: "bound-team", State: "Bound"},
		model.Resource{Kind: "PersistentVolume", Name: "claim-team", State: "Bound", Details: map[string]string{"claim": "team/data"}},
		model.Resource{Kind: "PersistentVolume", Name: "outside", State: "Bound", Details: map[string]string{"claim": "other/data"}},
		model.Resource{Kind: "PersistentVolume", Name: "available", State: "Available"},
		model.Resource{Kind: "StorageClass", Name: "team-class"},
	)
	for _, scope := range []string{"namespace", "profile"} {
		w := request(handler(s), "GET", "/api/v1/inventory?"+scope+"=team", "")
		var inv model.Inventory
		if err := json.Unmarshal(w.Body.Bytes(), &inv); err != nil {
			t.Fatal(err)
		}
		found := map[string]bool{}
		for _, r := range inv.Resources {
			found[r.Name] = true
		}
		for _, name := range []string{"bound-team", "claim-team", "available", "team-class", "data", "work", "team"} {
			if !found[name] {
				t.Errorf("%s missing %s", scope, name)
			}
		}
		if found["outside"] || found["other"] {
			t.Fatal(found)
		}
	}
}

func TestOptionalFieldsKeepNilVersusIntentionalEmpty(t *testing.T) {
	s, _, o := fixture()
	h := handler(s)
	for _, test := range []struct {
		body     string
		explicit bool
	}{
		{`{"kind":"notebook.edit","name":"work","namespace":"team","container":"main","cpuLimit":"2"}`, false},
		{`{"kind":"notebook.edit","name":"work","namespace":"team","container":"main","gpuCount":0,"mounts":[]}`, true},
	} {
		w := request(h, "POST", "/api/v1/plans", test.body)
		if w.Code != 201 {
			t.Fatal(w.Code, w.Body.String())
		}
		if (o.action.GPUCount != nil) != test.explicit || (o.action.Mounts != nil) != test.explicit {
			t.Fatalf("presence lost: %+v", o.action)
		}
		if test.explicit && (*o.action.GPUCount != 0 || len(*o.action.Mounts) != 0) {
			t.Fatal(o.action)
		}
	}
}

func TestProtectedLogoutAliases(t *testing.T) {
	s, _, _ := fixture()
	calls := 0
	logout := func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(204) }
	protect := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-CSRF-Token") != "csrf-test" {
				w.WriteHeader(403)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	h := s.routes(protect, logout, logout, logout)
	for _, path := range []string{"/logout", "/auth/logout"} {
		if w := request(h, "POST", path, ""); w.Code != 403 {
			t.Fatal(path, w.Code)
		}
		r := httptest.NewRequest("POST", path, nil)
		r.Header.Set("X-CSRF-Token", "csrf-test")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 204 {
			t.Fatal(path, w.Code)
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
}

func TestBrowserPayloadAndPreviewLogic(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("Node is required for the local browser-logic test")
	}
	for _, script := range []string{"testdata/logic_test.cjs", "testdata/navigation_test.cjs"} {
		if output, err := exec.Command(node, script).CombinedOutput(); err != nil {
			t.Fatalf("browser logic %s: %v\n%s", script, err, output)
		}
	}
}

func TestExactOwnerIdentityAndSafeServerChanges(t *testing.T) {
	s, _, o := fixture()
	s.identity = func(context.Context) (model.Identity, bool) {
		return model.Identity{Subject: "Exact-Kubeflow-Username", CSRF: "csrf-test"}, true
	}
	w := request(handler(s), "GET", "/profiles", "")
	// The create form asks for the registered email address; the update form
	// shows the observed owner read-only. Neither field is a free-text owner
	// identity any more.
	for _, text := range []string{"Exact-Kubeflow-Username", "Email (registered with identity provider, e.g. user email from LDAP)", `name="profileCreateOwner" type="email"`, `name="profileOwner" type="text" readonly`, "Owner transfer is unsupported by the installed Profile controller", "every existing quota key must be supplied with its current value"} {
		if !strings.Contains(w.Body.String(), text) {
			t.Errorf("missing %q", text)
		}
	}
	if strings.Contains(w.Body.String(), `name="owner" type="text"`) {
		t.Fatal("the owner field is no longer a free-text identity field")
	}
	// The legacy API passthrough is unchanged: the typed owner is still sent
	// exactly as entered, including case.
	o.changes = []model.Change{{Field: "container.main.cpu.request", Before: "500m", After: "2"}, {Field: "effective run state", Before: "Running", After: "Stopped"}}
	w = request(handler(s), "POST", "/api/v1/plans", `{"kind":"profile.create","name":"team","owner":"CaseSensitive-Username"}`)
	if w.Code != 201 || o.action.Owner != "CaseSensitive-Username" {
		t.Fatal(w.Code, o.action.Owner)
	}
	var plan model.Plan
	if err := json.Unmarshal(w.Body.Bytes(), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Prepared.Changes) != 2 || plan.Prepared.Changes[1].After != "Stopped" || len(plan.Prepared.Warnings) != 1 {
		t.Fatal(plan.Prepared)
	}
	js := request(handler(s), "GET", "/static/app.js", "").Body.String()
	if !strings.Contains(js, "plan.prepared.changes.map") || strings.Contains(js, "innerHTML") || strings.Contains(js, "plan.prepared.data") {
		t.Fatal("unsafe or missing change preview")
	}
}

type planBackend struct{}

func (planBackend) Prepare(context.Context, model.Action) (model.Prepared, error) {
	return model.Prepared{}, nil
}
func (planBackend) Execute(context.Context, model.Prepared, func(string) error) error { return nil }

// End-to-end regression: the real Manager returns auth.ErrSessionInvalid
// unchanged and the web layer must map it by error identity, not message text.
func TestRealManagerSessionInvalidIsMappedByErrorIdentity(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := config.Config{
		Features: config.Features{WritesEnabled: true},
		Auth:     config.Auth{IssuerURL: "https://issuer.example", SessionMinutes: 15, AdminGroups: []string{"admins"}},
	}
	m, err := operations.New(context.Background(), cfg, st, planBackend{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	s := &server{cfg: cfg, ops: m, ping: st.Ping, identity: func(context.Context) (model.Identity, bool) {
		// A syntactically invalid session key fails ValidateSession before any
		// store lookup, exercising the real worker authorization boundary.
		return model.Identity{Issuer: "https://issuer.example", Subject: "alice", SessionID: "not-a-session-key"}, true
	}}
	w := request(handler(s), "POST", "/api/v1/plans", `{"kind":"profile.create","name":"team"}`)
	var response struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if w.Code != 403 || response.Error.Code != "reauthentication_required" || !strings.Contains(response.Error.Message, "Sign in again") {
		t.Fatalf("got %d %+v", w.Code, response)
	}
}

func TestSignedOutPageIsPublicStaticAndDoesNotAutoLogin(t *testing.T) {
	s, _, _ := fixture()
	h := handler(s)
	for _, path := range []string{"/auth/signed-out", "/signed-out"} {
		w := request(h, "GET", path, "")
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, `href="/auth/login"`) {
			t.Fatalf("%s: missing explicit sign-in link", path)
		}
		for _, forbidden := range []string{`http-equiv="refresh"`, "<script", "location.href", "window.location"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("%s: auto-login construct %q", path, forbidden)
			}
		}
		if w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Header().Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Fatalf("%s: insecure headers %v", path, w.Header())
		}
	}
}

// The template must receive an explicit projection of config, never the whole
// struct, so sensitive fields cannot leak by accident.
func TestPageViewExposesOnlySafeConfigProjection(t *testing.T) {
	s, _, _ := fixture()
	s.cfg = config.Config{
		Server:     config.Server{PublicURL: "https://private-public-url.example", Listen: "127.0.0.1:9999"},
		Auth:       config.Auth{IssuerURL: "https://private-issuer.example", ClientID: "private-client", ClientSecretFile: "/run/private/secret", AdminGroups: []string{"private-admins"}},
		Kubernetes: config.Kubernetes{Kubeconfig: "/private/kubeconfig"},
		Database:   config.Database{Path: "/private/internal.db"},
		Features:   config.Features{WritesEnabled: true, StaticStorage: true, PVCExpansion: true},
		Notebooks: config.Notebooks{
			ControllerContractConfirmed: true,
			AllowedImages:               []string{"registry.example/allowed-image:1"},
			ProtectedMountPaths:         []string{"/private/protected-mount"},
			GPUPresets:                  []config.GPUPreset{{Name: "private-a100", ResourceKey: "nvidia.com/private-gpu", NodeSelector: map[string]string{"private": "selector"}, RuntimeClassName: "private-runtime"}},
		},
		Storage: config.Storage{
			AllowedStorageClasses: []string{"allowed-class"},
			NFSTemplates:          []config.NFSTemplate{{Name: "allowed-nfs", Server: "private-nfs-server", BasePath: "/private/nfs", StorageClass: "allowed-class"}},
			CSITemplates:          []config.CSITemplate{{Name: "allowed-csi", Driver: "private-driver", VolumeHandle: "private-handle", StorageClass: "allowed-class"}},
		},
	}
	s.identity = func(context.Context) (model.Identity, bool) {
		return model.Identity{Subject: "admin@example.org", CSRF: "csrf-token"}, true
	}
	h := handler(s)
	for path, wants := range map[string][]string{
		"/storage":   {"allowed-class", "allowed-nfs", "allowed-csi"},
		"/profiles":  {"nvidia.com/private-gpu"},
		"/notebooks": {"private-a100", "registry.example/allowed-image:1"},
	} {
		w := request(h, "GET", path, "")
		if w.Code != 200 {
			t.Fatalf("%s: %d", path, w.Code)
		}
		body := w.Body.String()
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s missing %q", path, want)
			}
		}
		for _, secret := range []string{
			"private-public-url.example", "127.0.0.1:9999", "private-issuer.example", "private-client",
			"/run/private/secret", "private-admins", "/private/kubeconfig", "/private/internal.db",
			"/private/protected-mount", "private-selector", "private-runtime", "private-nfs-server",
			"/private/nfs", "private-driver", "private-handle",
		} {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaked %q", path, secret)
			}
		}
	}
}
