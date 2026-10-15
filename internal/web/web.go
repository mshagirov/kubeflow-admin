// Package web serves the administrative console and its deliberately narrow API.
package web

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"kubeflow-admin/internal/auth"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/kube"
	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/operations"
	"kubeflow-admin/internal/store"
)

//go:embed assets/*
var assets embed.FS

type inventoryService interface {
	Inventory(context.Context) (model.Inventory, error)
	Check(context.Context) error
}
type operationService interface {
	Plan(context.Context, model.Identity, model.Action) (model.Plan, error)
	Submit(context.Context, model.Identity, string, string) (model.Operation, error)
	Get(context.Context, string) (model.Operation, error)
	List(context.Context) ([]model.Operation, error)
	Audit(context.Context) ([]model.Audit, error)
}
type server struct {
	rate     writeLimiter
	cfg      config.Config
	kube     inventoryService
	ops      operationService
	ping     func(context.Context) error
	identity func(context.Context) (model.Identity, bool)
}

// New registers all routes. Authentication and unsafe-request CSRF enforcement
// are applied as one boundary to every non-public route.
func New(cfg config.Config, a *auth.Auth, k *kube.Service, o *operations.Manager, s *store.Store) http.Handler {
	v := &server{cfg: cfg, kube: k, ops: o, ping: s.Ping, identity: auth.Identity}
	return v.routes(a.Middleware, a.Login, a.Callback, a.Logout)
}

func (s *server) routes(protect func(http.Handler) http.Handler, login, callback, logout http.HandlerFunc) http.Handler {
	private := http.NewServeMux()
	private.HandleFunc("GET /api/v1/me", func(w http.ResponseWriter, r *http.Request) {
		i, ok := s.identity(r.Context())
		if !ok {
			fail(w, 401, "Sign in is required.")
			return
		}
		respond(w, 200, i)
	})
	for _, name := range []string{"inventory", "overview", "profiles", "namespaces", "pvs", "pvcs", "notebooks", "storage-classes", "pods", "events", "quotas", "limit-ranges", "role-bindings", "statefulsets", "nodes"} {
		private.HandleFunc("GET /api/v1/"+name, s.inventory(name))
	}
	private.HandleFunc("POST /api/v1/plans", s.plan)
	private.HandleFunc("POST /api/v1/operations", s.submit)
	private.HandleFunc("GET /api/v1/operations", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.ops.List(r.Context())
		if e != nil {
			backendFailure(w, e, false)
			return
		}
		respond(w, 200, v)
	})
	private.HandleFunc("GET /api/v1/operations/{id}", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.ops.Get(r.Context(), r.PathValue("id"))
		if e != nil {
			backendFailure(w, e, false)
			return
		}
		respond(w, 200, v)
	})
	private.HandleFunc("GET /api/v1/audit", func(w http.ResponseWriter, r *http.Request) {
		v, e := s.ops.Audit(r.Context())
		if e != nil {
			backendFailure(w, e, false)
			return
		}
		respond(w, 200, v)
	})
	private.HandleFunc("POST /logout", logout)
	private.HandleFunc("POST /auth/logout", logout)
	tmpl := template.Must(template.ParseFS(assets, "assets/index.html"))
	for _, path := range []string{"/{$}", "/profiles", "/storage", "/notebooks", "/operations", "/audit"} {
		private.HandleFunc("GET "+path, func(w http.ResponseWriter, r *http.Request) {
			identity, ok := s.identity(r.Context())
			if !ok {
				fail(w, 401, "Sign in is required.")
				return
			}
			var page bytes.Buffer
			if err := tmpl.Execute(&page, struct {
				Identity model.Identity
				Config   config.Config
			}{identity, s.cfg}); err != nil {
				fail(w, 500, "The console could not be rendered.")
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(page.Bytes())
		})
	}
	root := http.NewServeMux()
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { respond(w, 200, map[string]string{"status": "ok"}) })
	root.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		if s.ping(ctx) != nil || s.kube.Check(ctx) != nil {
			fail(w, 503, "Not ready.")
			return
		}
		respond(w, 200, map[string]string{"status": "ready"})
	})
	root.HandleFunc("GET /login", login)
	root.HandleFunc("GET /callback", callback)
	root.HandleFunc("GET /auth/login", login)
	root.HandleFunc("GET /auth/callback", callback)
	for _, name := range []string{"app.js", "logic.js", "style.css"} {
		name := name
		root.HandleFunc("GET /static/"+name, func(w http.ResponseWriter, r *http.Request) {
			b, e := assets.ReadFile("assets/" + name)
			if e != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", mime.TypeByExtension("."+strings.Split(name, ".")[1]))
			w.Header().Set("Cache-Control", "public, max-age=3600")
			_, _ = w.Write(b)
		})
	}
	root.Handle("/", protect(private))
	return security(root)
}

func security(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Strict-Transport-Security", "max-age=31536000")
		w.Header().Set("Cache-Control", "no-store")
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		next.ServeHTTP(w, r)
	})
}
func respond(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func fail(w http.ResponseWriter, status int, message string) {
	code := map[int]string{400: "invalid_request", 401: "authentication_required", 403: "policy_denied", 404: "not_found", 409: "conflict", 415: "unsupported_media_type", 422: "validation_failed", 500: "internal_error", 503: "service_unavailable"}[status]
	failure(w, status, code, message, status == 503)
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	media, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || media != "application/json" {
		fail(w, 415, "Use application/json.")
		return false
	}
	d := json.NewDecoder(r.Body)
	var raw json.RawMessage
	if e = d.Decode(&raw); e != nil {
		fail(w, 400, "Invalid request. Check the typed fields and request size.")
		return false
	}
	if e = d.Decode(new(any)); !errors.Is(e, io.EOF) {
		fail(w, 400, "Send exactly one JSON object.")
		return false
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' {
		fail(w, 400, "Send one JSON object, not a null value or array.")
		return false
	}
	strict := json.NewDecoder(bytes.NewReader(raw))
	strict.DisallowUnknownFields()
	if e = strict.Decode(v); e != nil {
		fail(w, 400, "Invalid request. Use only the supported typed fields.")
		return false
	}
	return true
}
func (s *server) inventory(list string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, e := s.kube.Inventory(r.Context())
		if e != nil {
			backendFailure(w, e, false)
			return
		}
		resources := make([]model.Resource, 0, len(v.Resources))
		ns := r.URL.Query().Get("namespace")
		if ns == "" {
			ns = r.URL.Query().Get("profile")
		}
		for _, item := range v.Resources {
			if !matchesNamespace(item, ns, v.Resources) {
				continue
			}
			if list == "inventory" || list == "overview" || resourceList(item.Kind) == list {
				resources = append(resources, item)
			}
		}
		v.Resources = resources
		respond(w, 200, v)
	}
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
func (s *server) plan(w http.ResponseWriter, r *http.Request) {
	i, ok := s.identity(r.Context())
	if !ok {
		fail(w, 401, "Sign in is required.")
		return
	}
	if !s.allowWrite(w, i) {
		return
	}
	var a model.Action
	if !decode(w, r, &a) {
		return
	}
	if !s.cfg.Features.WritesEnabled {
		fail(w, 403, "Changes are disabled by server policy.")
		return
	}
	v, e := s.ops.Plan(r.Context(), i, a)
	if e != nil {
		backendFailure(w, e, true)
		return
	}
	respond(w, 201, v)
}
func (s *server) submit(w http.ResponseWriter, r *http.Request) {
	i, ok := s.identity(r.Context())
	if !ok {
		fail(w, 401, "Sign in is required.")
		return
	}
	if !s.allowWrite(w, i) {
		return
	}
	var input struct {
		PlanID         string `json:"planID"`
		IdempotencyKey string `json:"idempotencyKey"`
	}
	if !decode(w, r, &input) {
		return
	}
	if !s.cfg.Features.WritesEnabled {
		fail(w, 403, "Changes are disabled by server policy.")
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		key = input.IdempotencyKey
	} else if input.IdempotencyKey != "" && key != input.IdempotencyKey {
		fail(w, 400, "Idempotency keys must match.")
		return
	}
	if input.PlanID == "" || len(input.PlanID) > 256 || len(key) < 16 || len(key) > 128 {
		fail(w, 400, "A plan ID and a 16–128 character idempotency key are required.")
		return
	}
	v, e := s.ops.Submit(r.Context(), i, input.PlanID, key)
	if e != nil {
		backendFailure(w, e, true)
		return
	}
	respond(w, 202, v)
}
