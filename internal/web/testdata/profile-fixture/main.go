// Package main is a synthetic, offline browser fixture for the Kubeflow Admin
// console. It exists only to give the optional real-browser acceptance runner
// a deterministic console to drive.
//
// It is NOT production code and it is NOT a second console:
//
//   - It binds 127.0.0.1 only. It is never reachable from outside the host.
//   - It never reads a kubeconfig, never reads a client secret and never
//     opens a Kubernetes client. It imports nothing from the console and it
//     starts no cluster connection of any kind.
//   - Every observation is a hard coded literal in this file. Every identity
//     is a synthetic one.
//   - A plan or an operation request is recorded in memory and answered with
//     a typed stub. Nothing outside this process is mutated, and the process
//     keeps no state between requests.
//   - It serves the real console template, the real client scripts and the
//     real stylesheet of the repository, so the browser exercises the shipped
//     front end. The page is rendered by Go's html/template with the same
//     minimal configuration projection and identity the real server uses.
//
// The directory is named "testdata" on purpose: the go tool ignores it, so
// "go build ./..." and "go test ./..." never see this program. The console
// binary embeds assets/* only and never imports this package, so a deployed
// console can never gain an unauthenticated endpoint from it.
//
// The runner builds this program into an approved temporary directory and
// starts it on a loopback port. See
// internal/web/testdata/profile_browser_optional.cjs.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------- observations

// resource is one synthetic observation. It mirrors the console inventory
// shape without importing the console model.
type resource struct {
	Kind      string            `json:"kind"`
	Namespace string            `json:"namespace,omitempty"`
	Name      string            `json:"name"`
	UID       string            `json:"uid,omitempty"`
	Owner     string            `json:"owner,omitempty"`
	State     string            `json:"state,omitempty"`
	Desired   string            `json:"desired,omitempty"`
	Details   map[string]string `json:"details,omitempty"`
}

type inventory struct {
	ObservedAt time.Time  `json:"observedAt"`
	Resources  []resource `json:"resources"`
	Warnings   []string   `json:"warnings,omitempty"`
}

// identity is the synthetic administrator of the fixture. It is not a real
// person and it grants no access anywhere.
type identity struct {
	Issuer            string    `json:"issuer"`
	Subject           string    `json:"subject"`
	Email             string    `json:"email"`
	Username          string    `json:"username,omitempty"`
	NotificationScope string    `json:"notificationScope,omitempty"`
	Groups            []string  `json:"groups"`
	AuthenticatedAt   time.Time `json:"authenticatedAt"`
	ExpiresAt         time.Time `json:"expiresAt"`
	CSRF              string    `json:"csrf,omitempty"`
}

// The synthetic sessions and observations are dated against the real UTC
// clock of the fixture process. A hard coded instant would make the fixture
// stale the moment the wall clock moved past it: a plan that was prepared
// against a literal expiry would be expired before anybody read it.
//
// The clock of the process is a reference point: it is read once, so every
// synthetic observation of one run tells the same story. A plan is dated
// again for every request, so every plan is live when it is prepared.
var fixtureClock = time.Now().UTC()

// ago is the instant that is the given duration before the fixture clock. It
// dates a synthetic observation.
func ago(d time.Duration) string {
	return fixtureClock.Add(-d).UTC().Format(time.RFC3339)
}

const (
	// day is the coarse unit the synthetic history is dated in.
	day = 24 * time.Hour
	// sessionLifetime is the lifetime of the synthetic session.
	sessionLifetime = 15 * time.Minute
	// planTTL is the lifetime of a synthetic plan. It matches the real
	// console: a plan is prepared, reviewed and submitted within minutes.
	planTTL = 5 * time.Minute
)

// fixtureIdentity is the synthetic administrator of the fixture. It is not a
// real person and it grants no access anywhere. Its session window is bound
// to the clock of the fixture, so the session is always live while the
// fixture runs.
var fixtureIdentity = identity{
	Issuer:            "https://synthetic.invalid",
	Subject:           "fixture-operator",
	Email:             "operator@example.org",
	Username:          "Synthetic Operator",
	NotificationScope: "synthetic-fixture-session",
	Groups:            []string{"kubeflow-admins"},
	CSRF:              "fixture-csrf-token-0123456789abcdef",
	AuthenticatedAt:   fixtureClock.Add(-time.Minute),
	ExpiresAt:         fixtureClock.Add(sessionLifetime),
}

// configDigest is the synthetic configuration precondition of a Profile. It
// is an opaque non-empty value, exactly like the digest the real console
// observes and sends back unchanged.
func configDigest(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// observations returns the whole synthetic observation set. Nothing here comes
// from a cluster: every value is a literal in this file.
func observations() []resource {
	return []resource{
		{
			Kind: "Profile", Name: "team-alpha", Owner: "admin@example.org", State: "Active",
			Details: map[string]string{
				"apiVersion":            "kubeflow.org/v1",
				"ownerKind":             "User",
				"quota.requests.cpu":    "4",
				"quota.requests.memory": "8Gi",
				"quota.pods":            "0",
				// A quota key the console does not manage. It has to stay
				// read-only and is returned unchanged in an update.
				"quota.requests.ephemeral-storage": "5Gi",
				"configDigest":                     configDigest("synthetic-config-team-alpha"),
				"createdAt":                        ago(50 * day),
			},
		},
		{
			Kind: "Profile", Name: "team-beta", Owner: "beta.owner@example.org", State: "Active",
			Details: map[string]string{
				"apiVersion":                       "kubeflow.org/v1",
				"ownerKind":                        "User",
				"quota.requests.cpu":               "1",
				"quota.requests.memory":            "2Gi",
				"quota.pods":                       "3",
				"quota.requests.ephemeral-storage": "5Gi",
				"configDigest":                     configDigest("synthetic-config-team-beta"),
				"createdAt":                        ago(49 * day),
			},
		},
		{Kind: "Namespace", Name: "team-alpha", State: "Active", Details: map[string]string{"createdAt": ago(50 * day)}},
		{Kind: "Namespace", Name: "team-beta", State: "Active", Details: map[string]string{"createdAt": ago(49 * day)}},
		// A namespace without a Profile. Its name is reserved: a new Profile
		// can never adopt an existing namespace.
		{Kind: "Namespace", Name: "team-delta", State: "Active", Details: map[string]string{"createdAt": ago(48 * day)}},
		{
			Kind: "Notebook", Namespace: "team-alpha", Name: "team-alpha-notebook-0", Owner: "admin@example.org", State: "Running",
			Details: map[string]string{
				"requested": "4 CPU / 8Gi", "capacity": "4 CPU / 8Gi", "storageClass": "standard",
				"node": "node-a", "volume": "team-alpha-claim", "condition.Ready": "True",
				"image.notebook": "kubeflownotebookswg/jupyter:1.9.0", "image.sidecar": "istio/proxyv2",
				"createdAt": ago(20 * day),
			},
		},
		{
			Kind: "Notebook", Namespace: "team-beta", Name: "team-beta-notebook-0", Owner: "beta.owner@example.org", State: "Stopped",
			Details: map[string]string{
				"requested": "1 CPU / 2Gi", "capacity": "1 CPU / 2Gi", "storageClass": "standard",
				"node": "node-b", "volume": "team-beta-claim", "condition.Ready": "False",
				"image.notebook": "kubeflownotebookswg/jupyter:1.9.0",
				"createdAt":      ago(19 * day),
			},
		},
		{
			Kind: "PersistentVolumeClaim", Namespace: "team-alpha", Name: "team-alpha-claim", State: "Bound",
			Details: map[string]string{
				"requested": "50Gi", "capacity": "50Gi", "storageClass": "standard",
				"volume": "pvc-team-alpha-claim", "claim": "team-alpha/team-alpha-claim",
				"createdAt": ago(20*day + 5*time.Minute),
			},
		},
		{
			Kind: "PersistentVolumeClaim", Namespace: "team-beta", Name: "team-beta-claim", State: "Bound",
			Details: map[string]string{
				"requested": "20Gi", "capacity": "20Gi", "storageClass": "standard",
				"volume": "pvc-team-beta-claim", "claim": "team-beta/team-beta-claim",
				"createdAt": ago(19*day + 5*time.Minute),
			},
		},
		{
			// A claim and a volume share the name "data", and a storage class
			// shares it as well: a row is only identified by its kind and its
			// name, never by its name alone.
			Kind: "PersistentVolumeClaim", Namespace: "team-alpha", Name: "data", State: "Bound",
			Details: map[string]string{
				"requested": "10Gi", "capacity": "10Gi", "storageClass": "standard",
				"volume": "pvc-team-alpha-data", "claim": "team-alpha/data",
				"createdAt": ago(20*day + 3*time.Minute),
			},
		},
		{
			Kind: "PersistentVolume", Name: "data", State: "Bound",
			Details: map[string]string{
				"capacity": "10Gi", "storageClass": "standard", "reclaimPolicy": "Retain",
				"claim": "team-alpha/data", "createdAt": ago(20*day + 2*time.Minute),
			},
		},
		{
			Kind: "StorageClass", Name: "data", State: "Active",
			Details: map[string]string{"provisioner": "kubeflow.io/data", "reclaimPolicy": "Delete"},
		},
		{
			Kind: "PersistentVolume", Name: "pvc-team-alpha-claim", State: "Bound",
			Details: map[string]string{
				"capacity": "50Gi", "storageClass": "standard", "reclaimPolicy": "Retain",
				"claim": "team-alpha/team-alpha-claim", "createdAt": ago(20*day + 10*time.Minute),
			},
		},
		{
			Kind: "StorageClass", Name: "standard", State: "Active",
			Details: map[string]string{"provisioner": "kubeflow.io/standard", "reclaimPolicy": "Delete"},
		},
		{
			Kind: "Pod", Namespace: "team-alpha", Name: "team-alpha-notebook-0-0", Owner: "StatefulSet/team-alpha-notebook-0", State: "Running",
			Details: map[string]string{
				"ready": "2/2", "containers": "notebook,istio-proxy", "restarts": "0", "node": "node-a",
				"cpuRequest": "4", "memoryRequest": "8Gi", "cpuLimit": "4", "memoryLimit": "8Gi",
				"createdAt": ago(20 * day),
			},
		},
		{
			Kind: "Pod", Namespace: "team-beta", Name: "team-beta-notebook-0-0", Owner: "StatefulSet/team-beta-notebook-0", State: "Running",
			Details: map[string]string{
				"ready": "1/2", "containers": "notebook", "restarts": "3", "node": "node-b",
				"cpuRequest": "1", "memoryRequest": "2Gi",
				"createdAt": ago(19 * day),
			},
		},
		{
			// The same Pod name in two namespaces: a row is identified by its
			// namespace and its name, never by its name alone.
			Kind: "Pod", Namespace: "team-alpha", Name: "shared-worker", Owner: "ReplicaSet/shared-worker", State: "Running",
			Details: map[string]string{
				"ready": "1/1", "containers": "worker", "restarts": "0", "node": "node-a",
				"createdAt": ago(20 * day),
			},
		},
		{
			Kind: "Pod", Namespace: "team-beta", Name: "shared-worker", Owner: "ReplicaSet/shared-worker", State: "Running",
			Details: map[string]string{
				"ready": "1/1", "containers": "worker", "restarts": "1", "node": "node-b",
				"createdAt": ago(19 * day),
			},
		},
		{
			Kind: "StatefulSet", Namespace: "team-alpha", Name: "team-alpha-notebook-0", State: "Running",
			Details: map[string]string{"replicas": "1", "ready": "1", "service": "team-alpha-notebook-0", "createdAt": ago(20 * day)},
		},
		{
			Kind: "ResourceQuota", Namespace: "team-alpha", Name: "team-alpha-quota", State: "Active",
			Details: map[string]string{"requests.cpu": "4", "requests.memory": "8Gi", "pods": "0"},
		},
		{
			// An event and a resource quota share the name "scheduled".
			Kind: "ResourceQuota", Namespace: "team-alpha", Name: "scheduled", State: "Active",
			Details: map[string]string{"requests.cpu": "4", "requests.memory": "8Gi", "pods": "0"},
		},
		{
			Kind: "Event", Namespace: "team-alpha", Name: "scheduled", State: "Normal",
			Details: map[string]string{"reason": "Scheduled", "message": "Successfully assigned synthetic pod."},
		},
		{
			Kind: "LimitRange", Namespace: "team-alpha", Name: "team-alpha-limits", State: "Active",
			Details: map[string]string{"type": "Container"},
		},
		{
			Kind: "RoleBinding", Namespace: "team-alpha", Name: "team-alpha-viewers", State: "Active",
			Details: map[string]string{"role": "view", "subjects": "group:team-alpha-viewers"},
		},
		{
			Kind: "Node", Name: "node-a", State: "Ready",
			Details: map[string]string{
				"roles": "control-plane", "version": "v1.31.4", "internalIP": "10.0.0.10",
				"osImage": "Ubuntu 24.04 LTS", "kernelVersion": "6.8.0", "containerRuntime": "containerd",
				"createdAt": ago(80 * day),
			},
		},
		{
			Kind: "Node", Name: "node-b", State: "Ready",
			Details: map[string]string{
				"roles": "worker", "version": "v1.31.4", "internalIP": "10.0.0.11",
				"osImage": "Ubuntu 24.04 LTS", "kernelVersion": "6.8.0", "containerRuntime": "containerd",
				"createdAt": ago(80 * day),
			},
		},
	}
}

// ---------------------------------------------------------------- projection

// The projection is the minimal configuration the console template consumes.
// It mirrors the real server: feature switches, GPU preset names and
// resource keys, allowed images, approved storage class and template names
// and the optional footer text.
// No database path, no kubeconfig location and no secret ever reaches it.
func projection(writes, deletion bool) map[string]any {
	return map[string]any{
		"Identity": map[string]any{
			"Email":             fixtureIdentity.Email,
			"Subject":           fixtureIdentity.Subject,
			"CSRF":              fixtureIdentity.CSRF,
			"DisplayName":       fixtureIdentity.Username,
			"NotificationScope": fixtureIdentity.NotificationScope,
		},
		"Config": map[string]any{
			"Features": map[string]any{
				"WritesEnabled":   writes,
				"StaticStorage":   true,
				"PVCExpansion":    true,
				"PodRestarts":     true,
				"NodeCordon":      true,
				"ProfileDeletion": deletion && writes,
			},
			"Notebooks": map[string]any{
				"ControllerContractConfirmed": true,
				"GPUPresets":                  []map[string]string{{"Name": "L4", "ResourceKey": "nvidia.com/gpu"}},
				"AllowedImages":               []string{"kubeflownotebookswg/jupyter:1.9.0"},
			},
			"Storage": map[string]any{
				"AllowedStorageClasses": []string{"standard", "nfs-csi"},
				"NFSTemplates":          []map[string]string{{"Name": "nfs-shared", "StorageClass": "nfs-csi"}},
				"CSITemplates":          []map[string]string{{"Name": "csi-shared", "StorageClass": "nfs-csi"}},
			},
			"UI": map[string]any{
				// The footer is deployment configuration. The synthetic
				// fixture sets none, exactly like the shipped default, so
				// the console renders no footer. The real server projects
				// the sanitized footer as template.HTML: the fixture keeps
				// the same type so the template renders it the same way.
				"FooterText": template.HTML(""),
			},
		},
	}
}

// projectionGap matches a template reference such as .Config.Features.X.
var projectionGap = regexp.MustCompile(`\.(Config|Identity)((?:\.[A-Za-z0-9_]+)+)`)

// missingProjectionFields reports template references the projection does
// not cover. A gap means the console template grew a field and the fixture
// projection has to follow. It is reported, never silently ignored.
func missingProjectionFields(tmpl *template.Template, view map[string]any) []string {
	seen := map[string]bool{}
	var gaps []string
	for _, match := range projectionGap.FindAllStringSubmatch(tmpl.Tree.Root.String(), -1) {
		root, path := match[1], strings.TrimPrefix(match[2], ".")
		full := root + "." + path
		if seen[full] {
			continue
		}
		seen[full] = true
		current := any(view)
		ok := true
		for _, part := range append([]string{root}, strings.Split(path, ".")...) {
			node, isMap := current.(map[string]any)
			if !isMap {
				ok = false
				break
			}
			value, exists := node[part]
			if !exists {
				ok = false
				break
			}
			current = value
		}
		if !ok {
			gaps = append(gaps, full)
		}
	}
	sort.Strings(gaps)
	return gaps
}

// ------------------------------------------------------------------- server

type capture struct {
	Kind    string            `json:"kind"`
	Body    map[string]any    `json:"body"`
	Raw     string            `json:"raw"`
	Headers map[string]string `json:"headers"`
	At      time.Time         `json:"at"`
}

type server struct {
	mu       sync.Mutex
	captures []capture
	warnings []string
	writes   bool
	deletion bool
	tmpl     *template.Template
	assets   string
}

func main() {
	repoRoot := os.Getenv("KF_ADMIN_REPO_ROOT")
	if repoRoot == "" {
		log.Fatal("fixture: KF_ADMIN_REPO_ROOT must point at the console repository")
	}
	assets := filepath.Join(repoRoot, "internal", "web", "assets")
	page, err := os.ReadFile(filepath.Join(assets, "index.html"))
	if err != nil {
		log.Fatalf("fixture: the console template is missing at %s: %v", assets, err)
	}
	writes := os.Getenv("KF_ADMIN_WRITES") != "0"
	deletion := os.Getenv("KF_ADMIN_PROFILE_DELETION") != "0"
	s := &server{
		writes:   writes,
		deletion: deletion,
		tmpl:     template.Must(template.New("index.html").Parse(string(page))),
		assets:   assets,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /__fixture", s.handleState)
	mux.HandleFunc("GET /__fixture/captures", s.handleCaptures)
	mux.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, fixtureIdentity) })
	mux.HandleFunc("GET /api/v1/inventory", s.handleInventory)
	for _, list := range []string{"overview", "profiles", "namespaces", "pvs", "pvcs", "notebooks", "storage-classes", "pods", "events", "quotas", "limit-ranges", "role-bindings", "statefulsets", "nodes"} {
		mux.HandleFunc("GET /api/v1/"+list, s.handleInventory)
	}
	mux.HandleFunc("GET /api/v1/operations", func(w http.ResponseWriter, r *http.Request) {
		historyHeaders(w, "recent-plus-unresolved")
		writeJSON(w, 200, []any{})
	})
	mux.HandleFunc("GET /api/v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 404, map[string]any{"error": map[string]any{"code": "not_found", "message": "Not found."}})
	})
	mux.HandleFunc("GET /api/v1/audit", func(w http.ResponseWriter, r *http.Request) {
		historyHeaders(w, "recent-audit-events")
		writeJSON(w, 200, []any{})
	})
	mux.HandleFunc("POST /api/v1/plans", s.handlePlan)
	mux.HandleFunc("POST /api/v1/operations", s.handleSubmit)
	for _, name := range []string{"app.js", "logic.js", "style.css"} {
		mux.HandleFunc("GET /static/"+name, s.handleStatic(name))
	}
	for _, path := range []string{"/", "/profiles", "/namespaces", "/storage", "/notebooks", "/pods", "/nodes", "/history", "/operations", "/audit"} {
		mux.HandleFunc("GET "+path, s.handlePage)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:"+port())
	if err != nil {
		log.Fatalf("fixture: listen on the loopback interface: %v", err)
	}
	base := "http://" + listener.Addr().String()
	fmt.Printf("FIXTURE_READY %s writes=%v deletion=%v\n", base, writes, deletion && writes)
	_ = os.Stdout.Sync()
	log.Fatal(http.Serve(listener, mux))
}

func port() string {
	if value := strings.TrimSpace(os.Getenv("KF_ADMIN_PORT")); value != "" {
		return value
	}
	return "0"
}

// handlePage renders the real console template with the projection.
func (s *server) handlePage(w http.ResponseWriter, r *http.Request) {
	view := projection(s.writes, s.deletion)
	if gaps := missingProjectionFields(s.tmpl, view); len(gaps) > 0 {
		warning := "projection gap: " + strings.Join(gaps, ", ")
		s.mu.Lock()
		if !contains(s.warnings, warning) {
			s.warnings = append(s.warnings, warning)
		}
		s.mu.Unlock()
		log.Printf("FIXTURE WARNING %s", warning)
	}
	var page strings.Builder
	if err := s.tmpl.Execute(&page, view); err != nil {
		http.Error(w, "fixture template error: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(page.String()))
}

// handleStatic serves the real client scripts and stylesheet of the
// repository.
func (s *server) handleStatic(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := os.ReadFile(filepath.Join(s.assets, name))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		switch filepath.Ext(name) {
		case ".js":
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		case ".css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
		}
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(body)
	}
}

// handleInventory answers the observation list. The list is filtered the same
// way the real console filters it.
func (s *server) handleInventory(w http.ResponseWriter, r *http.Request) {
	list := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	all := observations()
	resources := make([]resource, 0, len(all))
	for _, item := range all {
		if list != "inventory" && list != "overview" && resourceList(item.Kind) != list {
			continue
		}
		resources = append(resources, item)
	}
	writeJSON(w, 200, inventory{
		// The observation is dated against the real clock of the fixture.
		ObservedAt: time.Now().UTC(),
		Resources:  resources,
	})
}

// allowedActionFields are the typed fields a plan request may carry. A field
// outside this list is refused, so no arbitrary manifest field can travel.
var allowedActionFields = map[string]bool{
	"kind": true, "namespace": true, "name": true, "reason": true, "owner": true,
	"confirmName": true, "baseConfigDigest": true,
	"quota": true, "storageClass": true, "size": true, "accessMode": true, "pvName": true,
	"template": true, "subdirectory": true, "container": true, "image": true,
	"cpuRequest": true, "cpuLimit": true, "memoryRequest": true, "memoryLimit": true,
	"gpuPreset": true, "gpuCount": true, "mounts": true, "restart": true, "force": true,
}

var quotaKeyPattern = regexp.MustCompile(`^[A-Za-z0-9]+([./-][A-Za-z0-9]+)*$`)

// handlePlan records a typed plan request in memory and answers with a typed
// stub. Nothing outside this process is mutated.
func (s *server) handlePlan(w http.ResponseWriter, r *http.Request) {
	body, err := decodeStrict(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "invalid_request", "message": err.Error()}})
		return
	}
	for key := range body {
		if !allowedActionFields[key] {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "invalid_request", "message": "Unsupported action field: " + key}})
			return
		}
	}
	if quota, ok := body["quota"].(map[string]any); ok {
		for key := range quota {
			if !quotaKeyPattern.MatchString(key) {
				writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "invalid_request", "message": "Unsupported quota key: " + key}})
				return
			}
		}
	}
	// An update must carry the base configuration digest of the observed
	// Profile, so a stale update is refused before anything is planned.
	if body["kind"] == "profile.update" {
		name, _ := body["name"].(string)
		digest, _ := body["baseConfigDigest"].(string)
		want := profileConfigDigest(name)
		switch {
		case digest == "":
			writeJSON(w, 422, map[string]any{"error": map[string]any{"code": "validation_failed", "message": "A profile update must carry the base configuration digest."}})
			return
		case want == "":
			writeJSON(w, 404, map[string]any{"error": map[string]any{"code": "not_found", "message": "The profile is not observed."}})
			return
		case digest != want:
			writeJSON(w, 409, map[string]any{"error": map[string]any{"code": "conflict", "message": "The profile configuration changed; reload it before updating."}})
			return
		}
	}
	s.record("plan", body, r)

	// The plan is dated against the real clock of the fixture, for every
	// request: a plan that is prepared is a plan that is still live. The
	// console refuses to submit a plan whose expiry has passed, so a fixture
	// that answered with a stale instant would block the whole workflow.
	now := time.Now().UTC()
	prepared := map[string]any{
		"action":          body,
		"targetUID":       "synthetic-uid-0001",
		"resourceVersion": "4242",
		"summary":         "Synthetic plan captured in memory by the offline fixture.",
		"warnings":        []string{"Synthetic fixture plan: no cluster was contacted and nothing was mutated."},
	}
	if changes := quotaChanges(body); len(changes) > 0 {
		prepared["changes"] = changes
	}
	writeJSON(w, 201, map[string]any{
		"id":           "plan-synthetic-0001",
		"actor":        "https://synthetic.invalid|fixture-operator",
		"actorDisplay": fixtureIdentity.Username,
		"createdAt":    now,
		"expiresAt":    now.Add(planTTL),
		"prepared":     prepared,
	})
}

// handleSubmit records a typed operation request in memory.
func (s *server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	body, err := decodeStrict(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "invalid_request", "message": err.Error()}})
		return
	}
	s.record("operation", body, r)
	now := time.Now().UTC()
	writeJSON(w, 202, map[string]any{
		"id":        "operation-synthetic-0001",
		"planID":    stringField(body, "planID"),
		"actor":     "https://synthetic.invalid|fixture-operator",
		"target":    "profile:team-alpha",
		"action":    "profile.create",
		"state":     "Succeeded",
		"step":      "complete",
		"createdAt": now,
		"updatedAt": now,
	})
}

// handleState exposes the synthetic configuration of the fixture.
func (s *server) handleState(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"identity": map[string]any{
			"email": fixtureIdentity.Email, "subject": fixtureIdentity.Subject,
			"displayName": fixtureIdentity.Username, "csrf": fixtureIdentity.CSRF,
			"notificationScope": fixtureIdentity.NotificationScope,
		},
		"features": map[string]any{
			"writesEnabled":   s.writes,
			"staticStorage":   true,
			"pvcExpansion":    true,
			"podRestarts":     true,
			"nodeCordon":      true,
			"profileDeletion": s.deletion && s.writes,
		},
		"forbiddenStrings": []string{
			// Values the projection must never carry. They are set here only
			// so the runner can prove that they do not reach the page.
			"/nonexistent/synthetic-fixture-kubeconfig",
			"/nonexistent/synthetic-fixture-admin.db",
		},
		"captures": len(s.captures),
		"warnings": s.warnings,
		// The clock of the fixture. The acceptance runner compares it with
		// the clock of the browser: a plan is only live when its expiry is
		// in the future of the clock the console runs on.
		"clock": map[string]any{
			"now":            time.Now().UTC(),
			"planTTLSeconds": int(planTTL / time.Second),
			"sessionSeconds": int(sessionLifetime / time.Second),
		},
	})
}

func (s *server) handleCaptures(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, 200, s.captures)
}

func (s *server) record(kind string, body map[string]any, r *http.Request) {
	headers := map[string]string{}
	for _, name := range []string{"Content-Type", "X-CSRF-Token", "Idempotency-Key"} {
		if value := r.Header.Get(name); value != "" {
			headers[name] = value
		}
	}
	raw, _ := json.Marshal(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captures = append(s.captures, capture{Kind: kind, Body: body, Raw: string(raw), Headers: headers, At: time.Now().UTC()})
}

// ------------------------------------------------------------------ helpers

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func historyHeaders(w http.ResponseWriter, window string) {
	w.Header().Set("X-History-Limit", "1000")
	w.Header().Set("X-History-Window", window)
	w.Header().Set("X-History-Retention-Days", "60")
}

func decodeStrict(r *http.Request) (map[string]any, error) {
	decoder := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	var body map[string]any
	if err := decoder.Decode(&body); err != nil {
		return nil, fmt.Errorf("invalid JSON body: %v", err)
	}
	return body, nil
}

func stringField(body map[string]any, key string) string {
	if value, ok := body[key].(string); ok {
		return value
	}
	return ""
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func resourceList(kind string) string {
	switch strings.ToLower(kind) {
	case "profile":
		return "profiles"
	case "namespace":
		return "namespaces"
	case "persistentvolume", "pv":
		return "pvs"
	case "persistentvolumeclaim", "pvc":
		return "pvcs"
	case "storageclass":
		return "storage-classes"
	case "notebook":
		return "notebooks"
	case "pod":
		return "pods"
	case "event":
		return "events"
	case "resourcequota":
		return "quotas"
	case "limitrange":
		return "limit-ranges"
	case "rolebinding":
		return "role-bindings"
	case "statefulset":
		return "statefulsets"
	case "node":
		return "nodes"
	}
	return ""
}

// profileConfigDigest returns the observed configuration digest of a Profile.
func profileConfigDigest(name string) string {
	for _, item := range observations() {
		if item.Kind == "Profile" && item.Name == name {
			return item.Details["configDigest"]
		}
	}
	return ""
}

// quotaChanges returns the server side before and after evidence of a quota
// update, so the console can render the validated difference.
func quotaChanges(body map[string]any) []map[string]string {
	changes := []map[string]string{}
	quota, ok := body["quota"].(map[string]any)
	if !ok {
		return changes
	}
	name, _ := body["name"].(string)
	observed := map[string]string{}
	for _, item := range observations() {
		if item.Kind != "Profile" || item.Name != name {
			continue
		}
		for key, value := range item.Details {
			if strings.HasPrefix(key, "quota.") {
				observed[strings.TrimPrefix(key, "quota.")] = value
			}
		}
	}
	keys := make([]string, 0, len(quota))
	for key := range quota {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value, _ := quota[key].(string)
		if observed[key] == value {
			continue
		}
		changes = append(changes, map[string]string{"field": "quota." + key, "before": observed[key], "after": value})
	}
	return changes
}
