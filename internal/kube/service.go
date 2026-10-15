// Package kube implements the deliberately restricted Kubernetes adapter. It never
// accepts manifests, reads Secrets, deletes objects, or writes child workloads.
package kube

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"kubeflow-admin/internal/config"
)

var Profiles = schema.GroupVersionResource{Group: "kubeflow.org", Version: "v1", Resource: "profiles"}
var Notebooks = schema.GroupVersionResource{Group: "kubeflow.org", Version: "v1", Resource: "notebooks"}

const stoppedKey = "kubeflow-resource-stopped"
const activityKey = "notebooks.kubeflow.org/last-activity"

var ErrConflict = errors.New("conflict: preview is stale; prepare a new plan")

// Clients and polling interval are exported to permit isolated fake-client tests.
// Production configuration is immutable after construction.
type Service struct {
	Config       config.Config
	Typed        kubernetes.Interface
	Dynamic      dynamic.Interface
	PollInterval time.Duration
}

func New(c config.Config) (*Service, error) {
	var r *rest.Config
	var err error
	if c.Kubernetes.Kubeconfig != "" {
		r, err = clientcmd.BuildConfigFromFlags("", c.Kubernetes.Kubeconfig)
	} else {
		r, err = rest.InClusterConfig()
	}
	if err != nil {
		return nil, fmt.Errorf("Kubernetes client configuration: %w", err)
	}
	r.Timeout = c.RequestTimeout()
	r.UserAgent = "kubeflow-admin/1"
	t, err := kubernetes.NewForConfig(r)
	if err != nil {
		return nil, err
	}
	d, err := dynamic.NewForConfig(r)
	if err != nil {
		return nil, err
	}
	return &Service{Config: c, Typed: t, Dynamic: d, PollInterval: time.Second}, nil
}

func (s *Service) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.Typed == nil || s.Dynamic == nil {
		return errors.New("Kubernetes clients are not configured")
	}
	for _, g := range []schema.GroupVersionResource{Profiles, Notebooks} {
		list, err := s.Typed.Discovery().ServerResourcesForGroupVersion(g.GroupVersion().String())
		if err != nil {
			return fmt.Errorf("required API discovery: %w", err)
		}
		found := false
		for _, r := range list.APIResources {
			if r.Name == g.Resource && r.Namespaced == (g == Notebooks) {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("required %s v1 API is unavailable", g.Resource)
		}
	}
	return nil
}

type reference struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Digest    string `json:"digest"`
}
type evidence struct {
	Version      int         `json:"version"`
	Policy       string      `json:"policy"`
	Target       string      `json:"target,omitempty"`
	Dependencies []reference `json:"dependencies,omitempty"`
}

func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (e *evidence) add(kind, ns, name string, v any) {
	for i, r := range e.Dependencies {
		if r.Kind == kind && r.Namespace == ns && r.Name == name {
			e.Dependencies[i].Digest = digest(v)
			return
		}
	}
	e.Dependencies = append(e.Dependencies, reference{kind, ns, name, digest(v)})
}
func (s *Service) newEvidence() evidence {
	return evidence{Version: 1, Policy: digest(struct {
		Policy    config.Policy
		Storage   config.Storage
		Notebooks config.Notebooks
		Features  config.Features
	}{s.Config.Policy, s.Config.Storage, s.Config.Notebooks, s.Config.Features})}
}
func (e *evidence) bytes() []byte {
	sort.Slice(e.Dependencies, func(i, j int) bool {
		a, b := e.Dependencies[i], e.Dependencies[j]
		return a.Kind+a.Namespace+a.Name < b.Kind+b.Namespace+b.Name
	})
	b, _ := json.Marshal(e)
	return b
}
func has(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
func name(v string) error {
	if len(validation.IsDNS1123Subdomain(v)) > 0 {
		return errors.New("invalid Kubernetes object name")
	}
	return nil
}
func (s *Service) namespacePolicy(ns string) error {
	if len(validation.IsDNS1123Label(ns)) > 0 {
		return errors.New("invalid namespace name")
	}
	if has([]string{"default", "kubeflow", "istio-system", "cert-manager", "kubeflow-admin", "auth", "oauth2-proxy", "kubeflow-system", "gpu-operator", "knative-serving", "knative-eventing"}, ns) || strings.HasPrefix(ns, "kube-") || has(s.Config.Policy.ProtectedNamespaces, ns) {
		return errors.New("protected namespace")
	}
	if len(s.Config.Policy.AllowedNamespaces) > 0 && !has(s.Config.Policy.AllowedNamespaces, ns) {
		return errors.New("namespace is outside configured scope")
	}
	return nil
}
func (s *Service) scope(ctx context.Context, ns string, e *evidence) error {
	if err := s.namespacePolicy(ns); err != nil {
		return err
	}
	p, err := s.Dynamic.Resource(Profiles).Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("matching Profile required: %w", safe(err))
	}
	if p.GetDeletionTimestamp() != nil {
		return errors.New("Profile is terminating")
	}
	owner, _, _ := unstructured.NestedString(p.Object, "spec", "owner", "name")
	kind, _, _ := unstructured.NestedString(p.Object, "spec", "owner", "kind")
	if owner == "" || kind != "User" {
		return errors.New("unsupported Profile owner contract")
	}
	n, err := s.Typed.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		return safe(err)
	}
	if n.DeletionTimestamp != nil || n.Status.Phase != corev1.NamespaceActive {
		return errors.New("namespace is not active")
	}
	if p.GetUID() == "" || !controlledBy(n.OwnerReferences, p.GetUID()) || n.Annotations["owner"] != owner {
		return errors.New("namespace ownership does not match its Profile")
	}
	e.add("Profile", "", ns, struct {
		UID  types.UID
		Spec any
	}{p.GetUID(), p.Object["spec"]})
	e.add("Namespace", "", ns, struct {
		UID    types.UID
		Phase  corev1.NamespacePhase
		Owners []metav1.OwnerReference
		Owner  string
	}{n.UID, n.Status.Phase, n.OwnerReferences, n.Annotations["owner"]})
	q, err := s.Typed.CoreV1().ResourceQuotas(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return safe(err)
	}
	type quotaPolicy struct {
		Name string
		UID  types.UID
		Spec corev1.ResourceQuotaSpec
	}
	qp := []quotaPolicy{}
	for _, v := range q.Items {
		qp = append(qp, quotaPolicy{v.Name, v.UID, v.Spec})
	}
	sort.Slice(qp, func(i, j int) bool { return qp[i].Name < qp[j].Name })
	e.add("ResourceQuotaList", ns, "", qp)
	l, err := s.Typed.CoreV1().LimitRanges(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return safe(err)
	}
	type limitPolicy struct {
		Name string
		UID  types.UID
		Spec corev1.LimitRangeSpec
	}
	lp := []limitPolicy{}
	for _, v := range l.Items {
		lp = append(lp, limitPolicy{v.Name, v.UID, v.Spec})
	}
	sort.Slice(lp, func(i, j int) bool { return lp[i].Name < lp[j].Name })
	e.add("LimitRangeList", ns, "", lp)
	return nil
}

type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

func conditional(uid, rv string, ops []patchOp) []patchOp {
	return append([]patchOp{{"test", "/metadata/uid", uid}, {"test", "/metadata/resourceVersion", rv}}, ops...)
}
func pointer(s string) string { return strings.ReplaceAll(strings.ReplaceAll(s, "~", "~0"), "/", "~1") }
func safe(err error) error {
	if err == nil {
		return nil
	}
	if apierrors.IsConflict(err) || (apierrors.IsInvalid(err) || apierrors.IsBadRequest(err)) && strings.Contains(strings.ToLower(err.Error()), "test") {
		return ErrConflict
	}
	if apierrors.IsNotFound(err) {
		return errors.New("Kubernetes resource not found")
	}
	if apierrors.IsAlreadyExists(err) {
		return ErrConflict
	}
	if apierrors.IsForbidden(err) {
		return errors.New("Kubernetes API denied the request")
	}
	if apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return errors.New("Kubernetes API rejected validation or conditional patch")
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return errors.New("Kubernetes API request failed")
}
func (s *Service) patchNotebook(ctx context.Context, n *unstructured.Unstructured, ops []patchOp, dry bool) (*unstructured.Unstructured, error) {
	b, err := json.Marshal(conditional(string(n.GetUID()), n.GetResourceVersion(), ops))
	if err != nil {
		return nil, err
	}
	o := metav1.PatchOptions{}
	if dry {
		o.DryRun = []string{metav1.DryRunAll}
	}
	r, err := s.Dynamic.Resource(Notebooks).Namespace(n.GetNamespace()).Patch(ctx, n.GetName(), types.JSONPatchType, b, o)
	return r, safe(err)
}
func checkpoint(fn func(string) error, step string) error {
	if fn == nil {
		return errors.New("durable checkpoint callback is required")
	}
	return fn(step)
}
func (s *Service) wait(ctx context.Context, fn func() (bool, error)) error {
	d := s.PollInterval
	if d <= 0 {
		d = time.Second
	}
	for {
		ok, err := fn()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("observed convergence not established: %w", ctx.Err())
		case <-t.C:
		}
	}
}
func same(a, b any) bool { return reflect.DeepEqual(a, b) }
