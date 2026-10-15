package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"kubeflow-admin/internal/auth"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/kube"
	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/operations"
	"kubeflow-admin/internal/store"
	"kubeflow-admin/internal/web"
)

// This is an actual HTTP/auth/store/operations/adapter integration with TLS OIDC
// fixtures and fake Kubernetes APIs. It does NOT simulate controllers or CSI.
func TestAuthenticatedApplicationWorkflow(t *testing.T) {
	ctx := context.Background()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var nonce string
	var nonceMu sync.Mutex
	var issuer string
	idp := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}, "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"}})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "test", "alg": "RS256", "use": "sig", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
		case "/token":
			clientID, secret, ok := r.BasicAuth()
			if !ok || clientID != "kubeflow-admin" || secret != "test-secret" {
				w.WriteHeader(401)
				return
			}
			nonceMu.Lock()
			n := nonce
			nonceMu.Unlock()
			header, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "test"})
			body, _ := json.Marshal(map[string]any{"iss": issuer, "sub": "test-admin", "aud": "kubeflow-admin", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": n, "groups": []string{"platform-admins"}, "email": "admin@example.org"})
			raw := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(body)
			h := sha256.Sum256([]byte(raw))
			sig, e := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
			if e != nil {
				t.Error(e)
				w.WriteHeader(500)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "not-persisted", "token_type": "Bearer", "expires_in": 3600, "id_token": raw + "." + base64.RawURLEncoding.EncodeToString(sig)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer idp.Close()
	issuer = idp.URL
	app := httptest.NewTLSServer(http.NotFoundHandler())
	defer app.Close()
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "oidc-secret")
	if err = os.WriteFile(secretPath, []byte("test-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Server: config.Server{PublicURL: app.URL}, Auth: config.Auth{IssuerURL: issuer, ClientID: "kubeflow-admin", ClientSecretFile: secretPath, AdminGroups: []string{"platform-admins"}, SessionMinutes: 15, Scopes: []string{"openid", "groups", "email"}}, Notebooks: config.Notebooks{ControllerContractConfirmed: true}, Features: config.Features{WritesEnabled: true}}
	s, err := store.Open(filepath.Join(dir, "admin.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := auth.New(context.WithValue(ctx, oauth2.HTTPClient, idp.Client()), cfg, s)
	if err != nil {
		t.Fatal(err)
	}
	controlled := true
	ref := func(kind, name string, uid types.UID) []metav1.OwnerReference {
		return []metav1.OwnerReference{{APIVersion: "kubeflow.org/v1", Kind: kind, Name: name, UID: uid, Controller: &controlled}}
	}
	profile := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubeflow.org/v1", "kind": "Profile", "metadata": map[string]any{"name": "pilot", "uid": "profile-uid", "resourceVersion": "1"}, "spec": map[string]any{"owner": map[string]any{"kind": "User", "name": "researcher@example.org"}}}}
	notebook := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubeflow.org/v1", "kind": "Notebook", "metadata": map[string]any{"name": "lab", "namespace": "pilot", "uid": "notebook-uid", "resourceVersion": "1", "annotations": map[string]any{"kubeflow-resource-stopped": "2026-10-15T00:00:00Z"}}, "spec": map[string]any{"template": map[string]any{"spec": map[string]any{"containers": []any{map[string]any{"name": "lab", "image": "example.org/notebook:v1"}}}}}}}
	replicas := int32(0)
	typed := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pilot", UID: "namespace-uid", Annotations: map[string]string{"owner": "researcher@example.org"}, OwnerReferences: ref("Profile", "pilot", "profile-uid")}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}}, &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "pilot", UID: "sts-uid", Generation: 1, OwnerReferences: ref("Notebook", "lab", "notebook-uid")}, Spec: appsv1.StatefulSetSpec{Replicas: &replicas}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 1}})
	typed.Discovery().(*fakediscovery.FakeDiscovery).Resources = []*metav1.APIResourceList{{GroupVersion: "kubeflow.org/v1", APIResources: []metav1.APIResource{{Name: "profiles", Kind: "Profile"}, {Name: "notebooks", Kind: "Notebook", Namespaced: true}}}}
	dynamic := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{kube.Profiles: "ProfileList", kube.Notebooks: "NotebookList"}, profile, notebook)
	k := &kube.Service{Config: cfg, Typed: typed, Dynamic: dynamic, PollInterval: time.Millisecond}
	m, err := operations.New(ctx, cfg, s, k)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	app.Config.Handler = web.New(cfg, a, k, m, s)
	client := app.Client()
	client.Jar, _ = cookiejar.New(nil)
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	request := func(method, path string, body any, csrf string) *http.Response {
		t.Helper()
		var reader io.Reader
		if body != nil {
			b, e := json.Marshal(body)
			if e != nil {
				t.Fatal(e)
			}
			reader = bytes.NewReader(b)
		}
		r, e := http.NewRequest(method, app.URL+path, reader)
		if e != nil {
			t.Fatal(e)
		}
		if body != nil {
			r.Header.Set("Content-Type", "application/json")
		}
		if method == "POST" {
			r.Header.Set("Origin", app.URL)
			r.Header.Set("X-CSRF-Token", csrf)
		}
		response, e := client.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { response.Body.Close() })
		return response
	}
	if response := request("GET", "/api/v1/me", nil, ""); response.StatusCode != 401 {
		t.Fatal("anonymous access", response.StatusCode)
	}
	login := request("GET", "/auth/login", nil, "")
	if login.StatusCode != 303 {
		t.Fatal("login", login.StatusCode)
	}
	location, e := url.Parse(login.Header.Get("Location"))
	if e != nil {
		t.Fatal(e)
	}
	q := location.Query()
	if q.Get("code_challenge_method") != "S256" {
		t.Fatal("missing PKCE")
	}
	nonceMu.Lock()
	nonce = q.Get("nonce")
	nonceMu.Unlock()
	callback := request("GET", "/auth/callback?state="+url.QueryEscape(q.Get("state"))+"&code=fixture", nil, "")
	if callback.StatusCode != 303 {
		b, _ := io.ReadAll(callback.Body)
		t.Fatalf("callback %d: %s", callback.StatusCode, b)
	}
	var identity model.Identity
	me := request("GET", "/api/v1/me", nil, "")
	if err = json.NewDecoder(me.Body).Decode(&identity); err != nil || me.StatusCode != 200 || identity.CSRF == "" {
		t.Fatal("identity", me.StatusCode, err)
	}
	readiness := request("GET", "/readyz", nil, "")
	if readiness.StatusCode != 200 {
		t.Fatal("readiness", readiness.StatusCode)
	}
	page := request("GET", "/notebooks", nil, "")
	if page.StatusCode != 200 {
		t.Fatal("page", page.StatusCode)
	}
	bad := request("POST", "/api/v1/plans", model.Action{Kind: "notebook.stop", Namespace: "pilot", Name: "lab"}, "")
	if bad.StatusCode != 403 {
		t.Fatal("missing CSRF accepted", bad.StatusCode)
	}
	response := request("POST", "/api/v1/plans", model.Action{Kind: "notebook.stop", Namespace: "pilot", Name: "lab", Reason: "Integration test"}, identity.CSRF)
	var plan model.Plan
	if err = json.NewDecoder(response.Body).Decode(&plan); err != nil || response.StatusCode != 201 {
		t.Fatal("plan", response.StatusCode, err, plan)
	}
	response = request("POST", "/api/v1/operations", map[string]string{"planID": plan.ID, "idempotencyKey": "integration-operation-key"}, identity.CSRF)
	var op model.Operation
	if err = json.NewDecoder(response.Body).Decode(&op); err != nil || response.StatusCode != 202 {
		t.Fatal("submit", response.StatusCode, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		op, err = m.Get(ctx, op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if op.State == "succeeded" {
			break
		}
		if op.State == "needs-review" || op.State == "failed" || time.Now().After(deadline) {
			t.Fatalf("operation did not complete: %+v", op)
		}
		time.Sleep(time.Millisecond)
	}
	logout := request("POST", "/auth/logout", map[string]string{}, identity.CSRF)
	if logout.StatusCode != 303 {
		t.Fatal("logout", logout.StatusCode)
	}
	if response = request("GET", "/api/v1/me", nil, ""); response.StatusCode != 401 {
		t.Fatal("logout failed", response.StatusCode)
	}
}
