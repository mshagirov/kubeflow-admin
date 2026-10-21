package kube

// profile_delete_test.go exercises the specialized Profile deletion path.
//
// The fixture is a deterministic stand-in for the API server, the installed
// Profile controller and the Kubernetes garbage collector. A completed
// deletion is only ever reported when the controller reactor has actually
// removed both fixture objects: the Profile and its namespace.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	ktesting "k8s.io/client-go/testing"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
)

// errDeleteNetwork marks a delete that must fail like a broken transport.
var errDeleteNetwork = errors.New("network")

// profileDeleteNamespaceGVR addresses namespaces in the fixture tracker.
var profileDeleteNamespaceGVR = corev1.SchemeGroupVersion.WithResource("namespaces")

type profileDeleteFixture struct {
	service *Service
	client  *fake.Clientset

	mu        sync.Mutex
	profiles  map[string]*unstructured.Unstructured
	notebooks map[string][]unstructured.Unstructured
	deletes   []metav1.DeleteOptions
	dryRuns   []metav1.DeleteOptions

	// deleteErr is returned instead of accepting a real delete.
	deleteErr error
	// namespaceErr is returned by the namespace read after the delete.
	namespaceErr error
	// deleteStatusNotFound answers the delete request with 404 while the
	// controller reactor still removes the Profile: the object may already be
	// gone for a reason this request did not cause.
	deleteStatusNotFound bool
	// replaceAfterDelete injects a replacement Profile during the first
	// namespace read after the delete.
	replaceAfterDelete bool
	injected           bool
	// namespaceListErr fails the namespace list.
	namespaceListErr error
	// namespaceListContinue makes every namespace list page continue.
	namespaceListContinue string

	// deleted is closed by the controller reactor once the Profile object has
	// actually been removed, so a test can wait for the mutation instead of
	// guessing at a deadline.
	deleted    chan struct{}
	deleteOnce sync.Once

	// The controller reactor decides what the garbage collector removes.
	removeProfile    bool
	removeNamespace  bool
	stripOwnerRef    bool
	replaceNamespace bool
}

// setReactor configures the controller reactor under the fixture lock.
func (f *profileDeleteFixture) setReactor(removeProfile, removeNamespace, stripOwnerRef, replaceNamespace bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeProfile = removeProfile
	f.removeNamespace = removeNamespace
	f.stripOwnerRef = stripOwnerRef
	f.replaceNamespace = replaceNamespace
}

// setNamespaceError makes every namespace read fail once the delete has been
// accepted, so a read failure is exercised after the mutation.
func (f *profileDeleteFixture) setNamespaceError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaceErr = err
}

// setDeleteStatusNotFound makes the delete request answer 404 while the
// controller reactor still removes the Profile.
func (f *profileDeleteFixture) setDeleteStatusNotFound(notFound bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteStatusNotFound = notFound
}

// setReplaceAfterDelete makes the first namespace read after the delete
// inject a replacement Profile.
func (f *profileDeleteFixture) setReplaceAfterDelete(replace bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.replaceAfterDelete = replace
}

// setNamespaceListFailure makes the namespace sweep fail.
func (f *profileDeleteFixture) setNamespaceListFailure(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaceListErr = err
}

// setNamespaceListNeverEnds makes every namespace list page continue.
func (f *profileDeleteFixture) setNamespaceListNeverEnds(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaceListContinue = token
}

// mutateClusterUID points the fixture at another cluster.
func (f *profileDeleteFixture) mutateClusterUID(t *testing.T, uid string) {
	t.Helper()
	ns, err := f.client.CoreV1().Namespaces().Get(context.Background(), profileDeleteClusterNamespace, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ns.UID = types.UID(uid)
	if _, err = f.client.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// addNamespace installs an extra namespace fixture.
func (f *profileDeleteFixture) addNamespace(t *testing.T, ns *corev1.Namespace) {
	t.Helper()
	if _, err := f.client.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func newProfileDeleteFixture(t *testing.T) *profileDeleteFixture {
	t.Helper()
	f := &profileDeleteFixture{
		profiles:        map[string]*unstructured.Unstructured{},
		notebooks:       map[string][]unstructured.Unstructured{},
		deleted:         make(chan struct{}),
		removeProfile:   true,
		removeNamespace: true,
	}
	f.putProfile(profileDeleteProfile("alice", "profile-1", "1"))
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "alice",
			UID:             "namespace-1",
			ResourceVersion: "1",
			Annotations:     map[string]string{"owner": "alice@example.test"},
			OwnerReferences: owner("profile-1", "Profile", "alice"),
		},
		Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}
	f.client = fake.NewSimpleClientset(ns)
	// The cluster identity: the well-known namespace whose UID names the
	// cluster every plan is bound to.
	cluster := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: profileDeleteClusterNamespace, UID: "cluster-1", ResourceVersion: "1"}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}}
	if _, err := f.client.CoreV1().Namespaces().Create(context.Background(), cluster, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// A namespace read failure is injected only after the delete has been
	// accepted, so the "a failed read is not absence" path is exercised after
	// the mutation rather than before it.
	f.client.PrependReactor("get", "namespaces", func(a ktesting.Action) (bool, runtime.Object, error) {
		f.mu.Lock()
		readErr := f.namespaceErr
		inject := f.replaceAfterDelete && len(f.deletes) > 0 && !f.injected
		if inject {
			f.injected = true
		}
		f.mu.Unlock()
		if readErr != nil && len(f.recordedDeletes()) > 0 {
			return true, nil, readErr
		}
		if inject {
			// A replacement Profile appears between the Profile read and the
			// namespace read of one observation round.
			f.putProfile(profileDeleteProfile("alice", "profile-2", "9"))
		}
		return false, nil, nil
	})
	// The namespace sweep can be made to fail or to never finish.
	f.client.PrependReactor("list", "namespaces", func(a ktesting.Action) (bool, runtime.Object, error) {
		f.mu.Lock()
		err := f.namespaceListErr
		token := f.namespaceListContinue
		f.mu.Unlock()
		if err != nil {
			return true, nil, err
		}
		if token != "" {
			return true, &corev1.NamespaceList{ListMeta: metav1.ListMeta{Continue: token}}, nil
		}
		return false, nil, nil
	})
	cfg := config.Config{Features: config.Features{WritesEnabled: true, ProfileDeletion: true}}
	s := &Service{Config: cfg, Typed: f.client, PollInterval: 20 * time.Millisecond}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	d, err := dynamic.NewForConfig(&rest.Config{Host: server.URL, QPS: 5000, Burst: 5000})
	if err != nil {
		t.Fatal(err)
	}
	s.Dynamic = d
	f.service = s
	return f
}

func profileDeleteProfile(name, uid, rv string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubeflow.org/v1",
		"kind":       "Profile",
		"metadata":   map[string]any{"name": name, "uid": uid, "resourceVersion": rv},
		"spec":       map[string]any{"owner": map[string]any{"kind": "User", "name": "alice@example.test"}},
	}}
}

func profileDeleteAction() model.Action {
	return model.Action{Kind: "profile.delete", Name: "alice", Reason: "disposable pilot cleanup", ConfirmName: "alice"}
}

func (f *profileDeleteFixture) putProfile(p *unstructured.Unstructured) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.profiles[p.GetName()] = p.DeepCopy()
}

func (f *profileDeleteFixture) profile(name string) *unstructured.Unstructured {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.profiles[name]
	if !ok {
		return nil
	}
	return p.DeepCopy()
}

func (f *profileDeleteFixture) mutateProfile(t *testing.T, name string, fn func(*unstructured.Unstructured)) {
	t.Helper()
	p := f.profile(name)
	if p == nil {
		t.Fatalf("missing Profile fixture %q", name)
	}
	fn(p)
	f.putProfile(p)
}

func (f *profileDeleteFixture) namespace(t *testing.T, name string) *corev1.Namespace {
	t.Helper()
	ns, err := f.client.CoreV1().Namespaces().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return ns
}

func (f *profileDeleteFixture) mutateNamespace(t *testing.T, name string, fn func(*corev1.Namespace)) {
	t.Helper()
	ns, err := f.client.CoreV1().Namespaces().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fn(ns)
	if _, err = f.client.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// deletes returns the recorded real delete requests.
func (f *profileDeleteFixture) recordedDeletes() []metav1.DeleteOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]metav1.DeleteOptions(nil), f.deletes...)
}

// recordedDryRuns returns the recorded dry-run delete requests.
func (f *profileDeleteFixture) recordedDryRuns() []metav1.DeleteOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]metav1.DeleteOptions(nil), f.dryRuns...)
}

// ServeHTTP implements the small part of the API surface this workflow uses.
func (f *profileDeleteFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 5 && parts[0] == "apis" && parts[1] == "kubeflow.org" && parts[2] == "v1" && parts[3] == "profiles":
		switch r.Method {
		case http.MethodGet:
			f.serveProfileGet(w, parts[4])
		case http.MethodDelete:
			f.serveProfileDelete(w, r, parts[4])
		default:
			writeStatus(w, http.StatusMethodNotAllowed, metav1.StatusReasonMethodNotAllowed, "unsupported")
		}
	case len(parts) == 6 && parts[0] == "apis" && parts[1] == "kubeflow.org" && parts[2] == "v1" && parts[3] == "namespaces" && parts[5] == "notebooks":
		f.serveNotebookList(w, parts[4])
	default:
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "missing")
	}
}

func (f *profileDeleteFixture) serveProfileGet(w http.ResponseWriter, name string) {
	p := f.profile(name)
	if p == nil {
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "missing")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(p.Object)
}

func (f *profileDeleteFixture) serveNotebookList(w http.ResponseWriter, ns string) {
	f.mu.Lock()
	items := f.notebooks[ns]
	f.mu.Unlock()
	list := map[string]any{"apiVersion": "kubeflow.org/v1", "kind": "NotebookList", "items": []any{}}
	raw := []any{}
	for _, n := range items {
		raw = append(raw, n.Object)
	}
	list["items"] = raw
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(list)
}

func (f *profileDeleteFixture) serveProfileDelete(w http.ResponseWriter, r *http.Request, name string) {
	opts := metav1.DeleteOptions{}
	if err := json.NewDecoder(r.Body).Decode(&opts); err != nil {
		writeStatus(w, http.StatusBadRequest, metav1.StatusReasonBadRequest, "invalid delete options")
		return
	}
	f.mu.Lock()
	if len(opts.DryRun) > 0 {
		f.dryRuns = append(f.dryRuns, opts)
	} else {
		f.deletes = append(f.deletes, opts)
	}
	f.mu.Unlock()
	if len(opts.DryRun) > 0 {
		// A dry run never persists anything.
		p := f.profile(name)
		if p == nil {
			writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "missing")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p.Object)
		return
	}
	f.mu.Lock()
	err := f.deleteErr
	notFound := f.deleteStatusNotFound
	f.mu.Unlock()
	if err != nil {
		if errors.Is(err, errDeleteNetwork) {
			breakConnection(w)
			return
		}
		writeDeleteError(w, err)
		return
	}
	// The controller reactor: the Profile disappears and the garbage
	// collector removes the namespace it owns.
	f.mu.Lock()
	removeProfile, removeNamespace, stripOwner, replaceNamespace := f.removeProfile, f.removeNamespace, f.stripOwnerRef, f.replaceNamespace
	f.mu.Unlock()
	if removeProfile {
		f.mu.Lock()
		delete(f.profiles, name)
		f.mu.Unlock()
		f.deleteOnce.Do(func() { close(f.deleted) })
	}
	if stripOwner {
		f.mutateNamespaceNoFatal(func(ns *corev1.Namespace) { ns.OwnerReferences = nil })
	}
	if replaceNamespace {
		f.mutateNamespaceNoFatal(func(ns *corev1.Namespace) { ns.UID = "namespace-2"; ns.ResourceVersion = "2" })
	}
	if removeNamespace {
		// The garbage collector removes the namespace. It is deleted straight
		// from the tracker so the recorded action log contains only what the
		// console itself did.
		_ = f.client.Tracker().Delete(profileDeleteNamespaceGVR, "", name)
	}
	if notFound {
		// The Profile is already gone: the request answers 404 even though
		// the object was removed by the controller or by another actor.
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "missing")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"apiVersion": "kubeflow.org/v1", "kind": "Status", "status": "Success"})
}

func (f *profileDeleteFixture) mutateNamespaceNoFatal(fn func(*corev1.Namespace)) {
	ns, err := f.client.CoreV1().Namespaces().Get(context.Background(), "alice", metav1.GetOptions{})
	if err != nil {
		return
	}
	fn(ns)
	_, _ = f.client.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{})
}

func writeStatus(w http.ResponseWriter, code int, reason metav1.StatusReason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"},
		Status:   metav1.StatusFailure,
		Code:     int32(code),
		Reason:   reason,
		Message:  message,
	})
}

func writeDeleteError(w http.ResponseWriter, err error) {
	switch {
	case apierrors.IsForbidden(err):
		writeStatus(w, http.StatusForbidden, metav1.StatusReasonForbidden, "denied")
	case apierrors.IsConflict(err):
		writeStatus(w, http.StatusConflict, metav1.StatusReasonConflict, "changed")
	case apierrors.IsInvalid(err):
		writeStatus(w, http.StatusUnprocessableEntity, metav1.StatusReasonInvalid, "invalid")
	case apierrors.IsTooManyRequests(err):
		writeStatus(w, http.StatusTooManyRequests, metav1.StatusReasonTooManyRequests, "busy")
	case apierrors.IsNotFound(err):
		writeStatus(w, http.StatusNotFound, metav1.StatusReasonNotFound, "missing")
	default:
		writeStatus(w, http.StatusInternalServerError, metav1.StatusReasonInternalError, "failed")
	}
}

func breakConnection(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	_ = conn.Close()
}

// typedDeletes returns the delete actions the typed client performed.
func typedDeletes(k *fake.Clientset) []string {
	out := []string{}
	for _, a := range k.Actions() {
		if a.GetVerb() != "delete" {
			continue
		}
		out = append(out, a.GetResource().Resource)
	}
	return out
}

// typedGets counts the read actions the typed client performed for one
// resource.
func typedGets(k *fake.Clientset, resource string) int {
	n := 0
	for _, a := range k.Actions() {
		if a.GetVerb() == "get" && a.GetResource().Resource == resource {
			n++
		}
	}
	return n
}

// --- prepare ---

func TestProfileDeletePrepareUsesUIDRVPreconditionsAndBackgroundPropagation(t *testing.T) {
	f := newProfileDeleteFixture(t)
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	dry := f.recordedDryRuns()
	if len(dry) != 1 {
		t.Fatalf("expected exactly one dry-run delete, got %d", len(dry))
	}
	opts := dry[0]
	if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != "profile-1" {
		t.Fatalf("missing UID precondition: %+v", opts.Preconditions)
	}
	if opts.Preconditions.ResourceVersion == nil || *opts.Preconditions.ResourceVersion != "1" {
		t.Fatalf("missing resourceVersion precondition: %+v", opts.Preconditions)
	}
	if opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationBackground {
		t.Fatalf("propagation policy = %v, want Background", opts.PropagationPolicy)
	}
	if len(opts.DryRun) != 1 || opts.DryRun[0] != metav1.DryRunAll {
		t.Fatalf("dry run = %v", opts.DryRun)
	}
	if len(f.recordedDeletes()) != 0 {
		t.Fatal("prepare issued a real delete")
	}
	if f.profile("alice") == nil {
		t.Fatal("the dry run mutated the Profile")
	}
	if p.TargetUID != "profile-1" || p.ResourceVersion != "1" {
		t.Fatalf("target identity: %+v", p)
	}
	if p.Summary != "profile.delete alice" {
		t.Fatalf("summary: %q", p.Summary)
	}
	// The evidence is narrow: identifiers and ownership facts only.
	var ev profileDeleteEvidence
	if err = json.Unmarshal(p.Data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.ProfileUID != "profile-1" || ev.ProfileRV != "1" || ev.NamespaceUID != "namespace-1" {
		t.Fatalf("evidence: %+v", ev)
	}
	if ev.OwnerRef.Kind != "Profile" || ev.OwnerRef.UID != "profile-1" || !ev.OwnerRef.Controller {
		t.Fatalf("owner reference evidence: %+v", ev.OwnerRef)
	}
	for _, unsafe := range []string{"resourceQuotaSpec", "containers", "spec\":", "volumeHandle", "secret"} {
		if strings.Contains(strings.ToLower(string(p.Data)), strings.ToLower(unsafe)) {
			t.Fatalf("unsafe evidence content %q: %s", unsafe, p.Data)
		}
	}
}

func TestProfileDeletePrepareStatesScopeAndIrreversibility(t *testing.T) {
	f := newProfileDeleteFixture(t)
	// Observed contents: one Notebook, one Pod, one claim with a Retain PV.
	f.mu.Lock()
	f.notebooks["alice"] = []unstructured.Unstructured{{Object: map[string]any{
		"apiVersion": "kubeflow.org/v1",
		"kind":       "Notebook",
		"metadata":   map[string]any{"name": "research", "namespace": "alice"},
	}}}
	f.mu.Unlock()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "research-0", Namespace: "alice", UID: "pod-1"}}
	if _, err := f.client.CoreV1().Pods("alice").Create(context.Background(), pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "home", Namespace: "alice", UID: "pvc-1"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "home-pv"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	if _, err := f.client.CoreV1().PersistentVolumeClaims("alice").Create(context.Background(), claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	volume := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "home-pv", UID: "pv-1"}, Spec: corev1.PersistentVolumeSpec{PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain, ClaimRef: &corev1.ObjectReference{Name: "home", Namespace: "alice", UID: "pvc-1"}}}
	if _, err := f.client.CoreV1().PersistentVolumes().Create(context.Background(), volume, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Join(p.Warnings, " ")
	for _, want := range []string{
		"irreversible",
		"PersistentVolumeClaims",
		"Retain reclaim policy",
		"may remain after the deletion",
		"Remaining is not protection",
		"Delete reclaim policy",
		"Never treat this operation as a verified data wipe",
		"storage stack",
		"VolumeSnapshotContent",
		"cluster scoped",
		"observed while the plan was prepared",
		"backup",
		"never deletes a namespace",
		"never removes a finalizer",
		"never retries or replays a deletion automatically",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing warning %q in %v", want, p.Warnings)
		}
	}
	changes := map[string]string{}
	for _, c := range p.Changes {
		changes[c.Field] = c.Before + " -> " + c.After
	}
	if changes["observed Notebooks"] != "1 observed -> deleted with the namespace" {
		t.Fatalf("notebook count: %v", changes)
	}
	if changes["observed Pods"] != "1 observed -> deleted with the namespace" {
		t.Fatalf("pod count: %v", changes)
	}
	if changes["observed PersistentVolumeClaims"] != "1 observed -> deleted with the namespace; backing data may be irreversibly deleted according to the PV reclaim policy" {
		t.Fatalf("claim count: %v", changes)
	}
	if !strings.Contains(changes["observed PersistentVolume reclaim policies"], "Retain 1") {
		t.Fatalf("reclaim policies: %v", changes)
	}
	// The preview must never promise that a volume survives.
	if strings.Contains(changes["observed PersistentVolume reclaim policies"], "PersistentVolumes are not deleted") {
		t.Fatalf("the preview promises that a volume survives: %v", changes)
	}
	if !strings.Contains(changes["Profile owner"], "identity provider account is not deleted") {
		t.Fatalf("the owner row must not claim the identity was deleted: %v", changes)
	}
}

func TestProfileDeletePrepareRefusesUnsafeInputs(t *testing.T) {
	cases := []struct {
		name   string
		change func(*profileDeleteFixture, *model.Action)
		want   string
	}{
		{"deletion disabled", func(f *profileDeleteFixture, _ *model.Action) {
			f.service.Config.Features.ProfileDeletion = false
		}, "profile deletion is disabled"},
		{"writes disabled", func(f *profileDeleteFixture, _ *model.Action) {
			f.service.Config.Features.WritesEnabled = false
		}, "writes are disabled"},
		{"invalid name", func(_ *profileDeleteFixture, a *model.Action) {
			a.Name = "Alice"
			a.ConfirmName = "Alice"
		}, "Profile name must be a lowercase DNS label"},
		{"long name", func(_ *profileDeleteFixture, a *model.Action) {
			a.Name = strings.Repeat("a", 64)
			a.ConfirmName = a.Name
		}, "Profile name must be at most 63 characters"},
		{"namespace mismatch", func(_ *profileDeleteFixture, a *model.Action) {
			a.Namespace = "other"
		}, "Profile namespace must match its name"},
		{"empty reason", func(_ *profileDeleteFixture, a *model.Action) {
			a.Reason = "  \t\n"
		}, "a nonempty reason is required for profile deletion"},
		{"long reason", func(_ *profileDeleteFixture, a *model.Action) {
			a.Reason = strings.Repeat("x", 501)
		}, "reason exceeds 500 characters"},
		{"missing confirmation", func(_ *profileDeleteFixture, a *model.Action) {
			a.ConfirmName = ""
		}, "typed confirmation must exactly match the Profile name"},
		{"partial confirmation", func(_ *profileDeleteFixture, a *model.Action) {
			a.ConfirmName = "alic"
		}, "typed confirmation must exactly match the Profile name"},
		{"cased confirmation", func(_ *profileDeleteFixture, a *model.Action) {
			a.ConfirmName = "Alice"
		}, "typed confirmation must exactly match the Profile name"},
		{"padded confirmation", func(_ *profileDeleteFixture, a *model.Action) {
			a.ConfirmName = " alice "
		}, "typed confirmation must exactly match the Profile name"},
		{"unrelated quota", func(_ *profileDeleteFixture, a *model.Action) {
			a.Quota = map[string]string{"requests.cpu": "2"}
		}, "profile deletion accepts no other action fields"},
		{"unrelated owner", func(_ *profileDeleteFixture, a *model.Action) {
			a.Owner = "alice@example.test"
		}, "profile deletion accepts no other action fields"},
		{"unrelated force", func(_ *profileDeleteFixture, a *model.Action) {
			a.Force = true
		}, "profile deletion accepts no other action fields"},
		{"unrelated image", func(_ *profileDeleteFixture, a *model.Action) {
			a.Image = "approved:v1"
		}, "profile deletion accepts no other action fields"},
		{"unrelated size", func(_ *profileDeleteFixture, a *model.Action) {
			a.Size = "1Gi"
		}, "profile deletion accepts no other action fields"},
		{"unrelated gpu count", func(_ *profileDeleteFixture, a *model.Action) {
			count := int64(1)
			a.GPUCount = &count
		}, "profile deletion accepts no other action fields"},
		{"unrelated mounts", func(_ *profileDeleteFixture, a *model.Action) {
			mounts := []model.Mount{{Name: "home", Claim: "home", Path: "/home"}}
			a.Mounts = &mounts
		}, "profile deletion accepts no other action fields"},
		{"protected namespace", func(_ *profileDeleteFixture, a *model.Action) {
			a.Name = "kubeflow"
			a.ConfirmName = "kubeflow"
		}, "protected namespace"},
		{"namespace outside scope", func(f *profileDeleteFixture, a *model.Action) {
			f.service.Config.Policy.AllowedNamespaces = []string{"other"}
		}, "namespace is outside configured scope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newProfileDeleteFixture(t)
			a := profileDeleteAction()
			tc.change(f, &a)
			_, err := f.service.prepareProfileDelete(context.Background(), a)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			if len(f.recordedDeletes()) != 0 {
				t.Fatal("a refused plan issued a delete")
			}
		})
	}
}

func TestProfileDeletePrepareRefusesUnownedOrMissingNamespace(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, *profileDeleteFixture)
		want   string
	}{
		{"missing namespace", func(t *testing.T, f *profileDeleteFixture) {
			if err := f.client.CoreV1().Namespaces().Delete(context.Background(), "alice", metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		}, "the Profile namespace does not exist"},
		{"foreign owner reference", func(t *testing.T, f *profileDeleteFixture) {
			f.mutateNamespace(t, "alice", func(ns *corev1.Namespace) {
				ns.OwnerReferences = owner("other-profile", "Profile", "alice")
			})
		}, "namespace ownership does not match its Profile"},
		{"owner reference of another kind", func(t *testing.T, f *profileDeleteFixture) {
			f.mutateNamespace(t, "alice", func(ns *corev1.Namespace) {
				ns.OwnerReferences = owner("profile-1", "Deployment", "alice")
			})
		}, "namespace ownership does not match its Profile"},
		{"owner reference of another group", func(t *testing.T, f *profileDeleteFixture) {
			f.mutateNamespace(t, "alice", func(ns *corev1.Namespace) {
				ns.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Profile", Name: "alice", UID: "profile-1", Controller: boolp(true)}}
			})
		}, "namespace ownership does not match its Profile"},
		{"ambiguous owner references", func(t *testing.T, f *profileDeleteFixture) {
			f.mutateNamespace(t, "alice", func(ns *corev1.Namespace) {
				ns.OwnerReferences = append(ns.OwnerReferences, owner("profile-1", "Profile", "alice")[0])
			})
		}, "the Profile namespace has more than one owner reference"},
		{"owner annotation mismatch", func(t *testing.T, f *profileDeleteFixture) {
			f.mutateNamespace(t, "alice", func(ns *corev1.Namespace) {
				ns.Annotations["owner"] = "someone-else@example.test"
			})
		}, "namespace ownership does not match its Profile"},
		{"terminating namespace", func(t *testing.T, f *profileDeleteFixture) {
			f.mutateNamespace(t, "alice", func(ns *corev1.Namespace) {
				now := metav1.Now()
				ns.DeletionTimestamp = &now
			})
		}, "namespace is not active"},
		{"terminating profile", func(t *testing.T, f *profileDeleteFixture) {
			f.mutateProfile(t, "alice", func(p *unstructured.Unstructured) {
				now := metav1.Now()
				p.SetDeletionTimestamp(&now)
			})
		}, "Profile is terminating"},
		{"foreign owner kind", func(t *testing.T, f *profileDeleteFixture) {
			f.mutateProfile(t, "alice", func(p *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(p.Object, "Group", "spec", "owner", "kind")
			})
		}, "unsupported Profile owner contract"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newProfileDeleteFixture(t)
			tc.change(t, f)
			_, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
			if len(f.recordedDeletes()) != 0 {
				t.Fatal("a refused plan issued a delete")
			}
		})
	}
}

func TestProfileDeleteObservationStaysBoundedWhenTheServerIgnoresTheLimit(t *testing.T) {
	f := newProfileDeleteFixture(t)
	// The API server may ignore the requested list limit, so more items than
	// the bound arrive. The preview must stay capped and the volume reads
	// must stay bounded.
	notebooks := []unstructured.Unstructured{}
	for i := 0; i < 150; i++ {
		notebooks = append(notebooks, unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "kubeflow.org/v1",
			"kind":       "Notebook",
			"metadata":   map[string]any{"name": fmt.Sprintf("lab-%d", i), "namespace": "alice"},
		}})
	}
	f.mu.Lock()
	f.notebooks["alice"] = notebooks
	f.mu.Unlock()
	for i := 0; i < 150; i++ {
		claim := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("claim-%d", i), Namespace: "alice", UID: types.UID(fmt.Sprintf("pvc-%d", i))},
			Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: fmt.Sprintf("pv-%d", i)},
			Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
		}
		if _, err := f.client.CoreV1().PersistentVolumeClaims("alice").Create(context.Background(), claim, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	changes := map[string]string{}
	for _, c := range p.Changes {
		changes[c.Field] = c.Before + " -> " + c.After
	}
	if changes["observed Notebooks"] != "100 observed -> deleted with the namespace" {
		t.Fatalf("the observed Notebook count is not capped: %v", changes)
	}
	if changes["observed PersistentVolumeClaims"] != "100 observed -> deleted with the namespace; backing data may be irreversibly deleted according to the PV reclaim policy" {
		t.Fatalf("the observed claim count is not capped: %v", changes)
	}
	if !strings.Contains(strings.Join(p.Warnings, " "), "bounded sample limit") {
		t.Fatalf("the truncated observation is not disclosed: %v", p.Warnings)
	}
	if reads := typedGets(f.client, "persistentvolumes"); reads > 100 {
		t.Fatalf("the volume reads are not bounded: %d", reads)
	}
}

func TestProfileDeleteForeignVolumeClaimIsUnknownNotReassuring(t *testing.T) {
	f := newProfileDeleteFixture(t)
	claim := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "home", Namespace: "alice", UID: "pvc-1"},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "home-pv"},
		Status:     corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	if _, err := f.client.CoreV1().PersistentVolumeClaims("alice").Create(context.Background(), claim, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// The volume points at another claim: a volume name alone proves
	// nothing, so the reclaim policy must stay unknown.
	volume := &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{Name: "home-pv", UID: "pv-1"}, Spec: corev1.PersistentVolumeSpec{
		PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimRetain,
		ClaimRef:                      &corev1.ObjectReference{Name: "other", Namespace: "alice", UID: "pvc-other"},
	}}
	if _, err := f.client.CoreV1().PersistentVolumes().Create(context.Background(), volume, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	row := ""
	for _, c := range p.Changes {
		if c.Field == "observed PersistentVolume reclaim policies" {
			row = c.Before
		}
	}
	if !strings.Contains(row, "Retain 0") || !strings.Contains(row, "unknown 1") {
		t.Fatalf("a foreign volume was reported as a reassuring Retain: %q", row)
	}
}

func TestProfileDeletePublicDispatchReachesTheSpecializedPath(t *testing.T) {
	f := newProfileDeleteFixture(t)
	// The public API must reach the specialized path: the typed action is
	// prepared and executed without any other write.
	p, err := f.service.Prepare(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	if p.Summary != "profile.delete alice" {
		t.Fatalf("summary: %q", p.Summary)
	}
	if len(f.recordedDryRuns()) != 1 || len(f.recordedDeletes()) != 0 {
		t.Fatalf("prepare requests: dry=%d delete=%d", len(f.recordedDryRuns()), len(f.recordedDeletes()))
	}
	if err = f.service.Execute(context.Background(), p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(f.recordedDeletes()) != 1 {
		t.Fatalf("expected exactly one delete, got %d", len(f.recordedDeletes()))
	}
	for _, resource := range typedDeletes(f.client) {
		t.Fatalf("the console issued a %s delete", resource)
	}
}

func TestProfileDeleteExecuteRefusesADriftedPlan(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*model.Prepared)
	}{
		{"foreign action field", func(p *model.Prepared) { p.Action.BaseConfigDigest = "digest" }},
		{"changed confirmation", func(p *model.Prepared) { p.Action.ConfirmName = "other" }},
		{"padded confirmation", func(p *model.Prepared) { p.Action.ConfirmName = " alice " }},
		{"empty reason", func(p *model.Prepared) { p.Action.Reason = "" }},
		{"long reason", func(p *model.Prepared) { p.Action.Reason = strings.Repeat("x", 501) }},
		{"foreign namespace", func(p *model.Prepared) { p.Action.Namespace = "other" }},
		{"invalid name", func(p *model.Prepared) { p.Action.Name = "Alice"; p.Action.ConfirmName = "Alice" }},
		{"target identity drift", func(p *model.Prepared) { p.TargetUID = "profile-9" }},
		{"resource version drift", func(p *model.Prepared) { p.ResourceVersion = "9" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newProfileDeleteFixture(t)
			p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(&p)
			err = f.service.executeProfileDelete(context.Background(), p, func(string) error { return nil })
			if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) {
				t.Fatalf("expected a pre-mutation rejection, got %v", err)
			}
			if len(f.recordedDeletes()) != 0 {
				t.Fatal("a drifted plan was deleted")
			}
		})
	}
}

func TestProfileDeleteDurableIdentityRowsAreExact(t *testing.T) {
	f := newProfileDeleteFixture(t)
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	after := map[string]string{}
	for _, c := range p.Changes {
		rows[c.Field] = c.Before
		after[c.Field] = c.After
	}
	want := map[string]string{
		"Cluster UID":              "cluster-1",
		"Profile UID":              "profile-1",
		"Profile resource version": "1",
		"Namespace UID":            "namespace-1",
		"Profile deletion reconciliation version": "1",
	}
	for field, value := range want {
		if rows[field] != value {
			t.Fatalf("identity row %q = %q, want %q", field, rows[field], value)
		}
		if after[field] != "accepted identity" {
			t.Fatalf("identity row %q after = %q", field, after[field])
		}
	}
	// The identity rows are not counts and the owner row keeps its meaning.
	if strings.Contains(rows["Profile UID"], "observed") {
		t.Fatal("the identity row must not be a count")
	}
	for _, c := range p.Changes {
		if c.Field == "Profile owner" && !strings.Contains(c.After, "identity provider account is not deleted") {
			t.Fatalf("owner row after = %q", c.After)
		}
	}
	// The evidence carries the cluster identity and no secret.
	var ev profileDeleteEvidence
	if err = json.Unmarshal(p.Data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.ClusterUID != "cluster-1" {
		t.Fatalf("cluster identity in the evidence: %q", ev.ClusterUID)
	}
	for _, unsafe := range []string{"kubeconfig", "token", "bearer", "client_secret", "context:"} {
		if strings.Contains(strings.ToLower(string(p.Data)), unsafe) {
			t.Fatalf("unsafe evidence content %q: %s", unsafe, p.Data)
		}
	}
}

func TestProfileDeleteRefusesADifferentCluster(t *testing.T) {
	f := newProfileDeleteFixture(t)
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	// The context now points at another cluster.
	f.mutateClusterUID(t, "cluster-2")
	err = f.service.executeProfileDelete(context.Background(), p, func(string) error { return nil })
	if err == nil || !errors.Is(err, ErrConflict) || !errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("expected a pre-mutation conflict, got %v", err)
	}
	if len(f.recordedDeletes()) != 0 {
		t.Fatal("a plan prepared for another cluster was deleted")
	}
}

func TestProfileDeletePrepareRefusesAnUnverifiableClusterIdentity(t *testing.T) {
	f := newProfileDeleteFixture(t)
	if err := f.client.CoreV1().Namespaces().Delete(context.Background(), profileDeleteClusterNamespace, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err == nil || !strings.Contains(err.Error(), "cluster identity could not be verified") {
		t.Fatalf("expected a cluster identity refusal, got %v", err)
	}
	if len(f.recordedDryRuns()) != 0 {
		t.Fatal("a plan without a cluster identity reached the dry run")
	}
}

func TestProfileDeleteRefusesAProfileThatOwnsAnotherNamespace(t *testing.T) {
	cases := []struct {
		name string
		ns   string
		ref  metav1.OwnerReference
	}{
		{"foreign namespace", "bob", metav1.OwnerReference{APIVersion: "kubeflow.org/v1", Kind: "Profile", Name: "alice", UID: "profile-1", Controller: boolp(true)}},
		{"protected namespace", "kubeflow", metav1.OwnerReference{APIVersion: "kubeflow.org/v1", Kind: "Profile", Name: "alice", UID: "profile-1", Controller: boolp(false)}},
		{"other kind owner", "bob", metav1.OwnerReference{APIVersion: "apps/v1", Kind: "Deployment", Name: "alice", UID: "profile-1", Controller: boolp(true)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newProfileDeleteFixture(t)
			f.addNamespace(t, &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{Name: tc.ns, UID: types.UID("ns-" + tc.ns), OwnerReferences: []metav1.OwnerReference{tc.ref}},
				Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
			})
			_, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
			if err == nil || !strings.Contains(err.Error(), "the Profile also owns another namespace") {
				t.Fatalf("expected a blast radius refusal, got %v", err)
			}
			if len(f.recordedDryRuns()) != 0 {
				t.Fatal("a plan with a wider blast radius reached the dry run")
			}
		})
	}
}

func TestProfileDeleteExecuteRefusesAProfileThatOwnsAnotherNamespace(t *testing.T) {
	f := newProfileDeleteFixture(t)
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	// The Profile acquires a second namespace after the plan was prepared.
	f.addNamespace(t, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "bob", UID: "ns-bob", OwnerReferences: []metav1.OwnerReference{{APIVersion: "kubeflow.org/v1", Kind: "Profile", Name: "alice", UID: "profile-1", Controller: boolp(true)}}},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	})
	err = f.service.executeProfileDelete(context.Background(), p, func(string) error { return nil })
	if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("expected a pre-mutation rejection, got %v", err)
	}
	if len(f.recordedDeletes()) != 0 {
		t.Fatal("a plan with a wider blast radius was deleted")
	}
}

func TestProfileDeleteRefusesANamespaceWithASecondOwner(t *testing.T) {
	f := newProfileDeleteFixture(t)
	f.mutateNamespace(t, "alice", func(ns *corev1.Namespace) {
		ns.OwnerReferences = append(ns.OwnerReferences, metav1.OwnerReference{APIVersion: "kubeflow.org/v1", Kind: "Profile", Name: "other", UID: "profile-2", Controller: boolp(true)})
	})
	_, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err == nil || !strings.Contains(err.Error(), "the Profile namespace has more than one owner reference") {
		t.Fatalf("expected a second owner refusal, got %v", err)
	}
}

func TestProfileDeleteRefusesAProfileThatOwnsTheSystemNamespace(t *testing.T) {
	f := newProfileDeleteFixture(t)
	// The system namespace is ordinary and unowned in a healthy cluster, so
	// it must not be skipped by the sweep: a Profile that owns it would take
	// it with it.
	cluster, err := f.client.CoreV1().Namespaces().Get(context.Background(), profileDeleteClusterNamespace, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cluster.OwnerReferences = []metav1.OwnerReference{{APIVersion: "kubeflow.org/v1", Kind: "Profile", Name: "alice", UID: "profile-1", Controller: boolp(true)}}
	if _, err = f.client.CoreV1().Namespaces().Update(context.Background(), cluster, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err == nil || !strings.Contains(err.Error(), "the Profile also owns another namespace") {
		t.Fatalf("expected a blast radius refusal, got %v", err)
	}
}

func TestProfileDeleteUnrelatedNamespaceOwnersAreAllowed(t *testing.T) {
	f := newProfileDeleteFixture(t)
	f.addNamespace(t, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "bob", UID: "ns-bob", OwnerReferences: []metav1.OwnerReference{{APIVersion: "kubeflow.org/v1", Kind: "Profile", Name: "bob", UID: "profile-bob", Controller: boolp(true)}}},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	})
	if _, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction()); err != nil {
		t.Fatalf("an unrelated namespace owner was refused: %v", err)
	}
}

func TestProfileDeleteNamespaceSweepFailsClosed(t *testing.T) {
	t.Run("list error", func(t *testing.T) {
		f := newProfileDeleteFixture(t)
		f.setNamespaceListFailure(errors.New("boom"))
		_, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
		if err == nil || !strings.Contains(err.Error(), "the namespace list could not be verified") {
			t.Fatalf("expected a fail-closed refusal, got %v", err)
		}
		if len(f.recordedDryRuns()) != 0 {
			t.Fatal("an unverified sweep reached the dry run")
		}
	})
	t.Run("incomplete sweep", func(t *testing.T) {
		f := newProfileDeleteFixture(t)
		f.setNamespaceListNeverEnds("page-2")
		_, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
		if err == nil || !strings.Contains(err.Error(), "the namespace list could not be verified") {
			t.Fatalf("expected a fail-closed refusal, got %v", err)
		}
		if len(f.recordedDryRuns()) != 0 {
			t.Fatal("a truncated sweep reached the dry run")
		}
	})
}

func TestProfileDeleteNotFoundFromTheDeleteIsNotARejection(t *testing.T) {
	t.Run("namespace still present", func(t *testing.T) {
		f := newProfileDeleteFixture(t)
		f.setReactor(true, false, false, false)
		f.setDeleteStatusNotFound(true)
		p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
		if err != nil {
			t.Fatal(err)
		}
		// The 404 is not a refusal: the execution waits for the strict
		// both-gone observation, and the fence is not released.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- f.service.executeProfileDelete(ctx, p, func(string) error { return nil }) }()
		select {
		case <-f.deleted:
		case <-time.After(10 * time.Second):
			t.Fatal("the delete was never issued")
		}
		cancel()
		var execErr error
		select {
		case execErr = <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the execution did not finish")
		}
		if execErr == nil {
			t.Fatal("reported success while the namespace was still present")
		}
		if errors.Is(execErr, model.ErrRejectedBeforeMutation) {
			t.Fatalf("a 404 released the fence: %v", execErr)
		}
	})
	t.Run("both gone", func(t *testing.T) {
		f := newProfileDeleteFixture(t)
		f.setReactor(true, true, false, false)
		f.setDeleteStatusNotFound(true)
		p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
		if err != nil {
			t.Fatal(err)
		}
		if err = f.service.executeProfileDelete(context.Background(), p, func(string) error { return nil }); err != nil {
			t.Fatalf("a verified deletion was refused: %v", err)
		}
	})
}

func TestProfileDeleteReplacementAfterTheFirstAbsentRoundIsNotSuccess(t *testing.T) {
	f := newProfileDeleteFixture(t)
	f.setReplaceAfterDelete(true)
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	err = f.service.executeProfileDelete(context.Background(), p, func(string) error { return nil })
	if err == nil || !errors.Is(err, ErrConflict) {
		t.Fatalf("expected a conflict, got %v", err)
	}
	if errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("the delete was already issued: %v", err)
	}
	if !strings.Contains(err.Error(), "replacement Profile") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --- execute ---

func TestProfileDeleteExecuteDeletesTheProfileOnceAndWaitsForBoth(t *testing.T) {
	f := newProfileDeleteFixture(t)
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	steps := []string{}
	if err = f.service.executeProfileDelete(context.Background(), p, func(step string) error {
		steps = append(steps, step)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0] != profileDeleteStep {
		t.Fatalf("checkpoint sequence: %v", steps)
	}
	deletes := f.recordedDeletes()
	if len(deletes) != 1 {
		t.Fatalf("expected exactly one delete, got %d", len(deletes))
	}
	opts := deletes[0]
	if opts.Preconditions == nil || opts.Preconditions.UID == nil || *opts.Preconditions.UID != "profile-1" {
		t.Fatalf("missing UID precondition: %+v", opts.Preconditions)
	}
	if opts.Preconditions.ResourceVersion == nil || *opts.Preconditions.ResourceVersion != "1" {
		t.Fatalf("missing resourceVersion precondition: %+v", opts.Preconditions)
	}
	if opts.PropagationPolicy == nil || *opts.PropagationPolicy != metav1.DeletePropagationBackground {
		t.Fatalf("propagation policy = %v, want Background", opts.PropagationPolicy)
	}
	if len(opts.DryRun) != 0 {
		t.Fatalf("the real delete must not be a dry run: %v", opts.DryRun)
	}
	if f.profile("alice") != nil {
		t.Fatal("the Profile was not deleted")
	}
	if _, err = f.client.CoreV1().Namespaces().Get(context.Background(), "alice", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the namespace was not removed: %v", err)
	}
	// The console never deletes the namespace or any volume itself.
	for _, resource := range typedDeletes(f.client) {
		t.Fatalf("the console issued a %s delete", resource)
	}
}

func TestProfileDeleteExecuteRefusesDriftBeforeMutation(t *testing.T) {
	cases := []struct {
		name   string
		change func(*testing.T, *profileDeleteFixture)
	}{
		{"profile replaced", func(t *testing.T, f *profileDeleteFixture) {
			f.mutateProfile(t, "alice", func(p *unstructured.Unstructured) { p.SetUID("profile-2") })
		}},
		{"profile changed", func(t *testing.T, f *profileDeleteFixture) {
			f.mutateProfile(t, "alice", func(p *unstructured.Unstructured) { p.SetResourceVersion("7") })
		}},
		{"profile owner changed", func(t *testing.T, f *profileDeleteFixture) {
			f.mutateProfile(t, "alice", func(p *unstructured.Unstructured) {
				_ = unstructured.SetNestedField(p.Object, "other@example.test", "spec", "owner", "name")
			})
		}},
		{"namespace replaced", func(t *testing.T, f *profileDeleteFixture) {
			f.mutateNamespace(t, "alice", func(ns *corev1.Namespace) { ns.UID = "namespace-2" })
		}},
		{"namespace ownership changed", func(t *testing.T, f *profileDeleteFixture) {
			f.mutateNamespace(t, "alice", func(ns *corev1.Namespace) {
				ns.OwnerReferences = owner("other-profile", "Profile", "alice")
			})
		}},
		{"namespace owner annotation changed", func(t *testing.T, f *profileDeleteFixture) {
			f.mutateNamespace(t, "alice", func(ns *corev1.Namespace) {
				ns.Annotations["owner"] = "someone-else@example.test"
			})
		}},
		{"namespace became terminating", func(t *testing.T, f *profileDeleteFixture) {
			f.mutateNamespace(t, "alice", func(ns *corev1.Namespace) {
				now := metav1.Now()
				ns.DeletionTimestamp = &now
			})
		}},
		{"policy changed", func(t *testing.T, f *profileDeleteFixture) {
			f.service.Config.Policy.ProtectedNamespaces = []string{"other"}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newProfileDeleteFixture(t)
			p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
			if err != nil {
				t.Fatal(err)
			}
			tc.change(t, f)
			err = f.service.executeProfileDelete(context.Background(), p, func(string) error { return nil })
			if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) {
				t.Fatalf("expected a pre-mutation rejection, got %v", err)
			}
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("expected a conflict, got %v", err)
			}
			if len(f.recordedDeletes()) != 0 {
				t.Fatal("a stale plan was deleted")
			}
		})
	}
}

func TestProfileDeleteExecuteRefusesDriftAfterCheckpoint(t *testing.T) {
	f := newProfileDeleteFixture(t)
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	err = f.service.executeProfileDelete(context.Background(), p, func(step string) error {
		if step != profileDeleteStep {
			t.Fatalf("unexpected checkpoint %q", step)
		}
		f.mutateProfile(t, "alice", func(p *unstructured.Unstructured) { p.SetResourceVersion("9") })
		return nil
	})
	if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) || !errors.Is(err, ErrConflict) {
		t.Fatalf("expected a conflict before the delete, got %v", err)
	}
	if len(f.recordedDeletes()) != 0 {
		t.Fatal("a plan that drifted after the checkpoint was deleted")
	}
}

func TestProfileDeleteCheckpointFailureNeverDeletes(t *testing.T) {
	f := newProfileDeleteFixture(t)
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	steps := []string{}
	err = f.service.executeProfileDelete(context.Background(), p, func(step string) error {
		steps = append(steps, step)
		return errors.New("disk unavailable")
	})
	if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("checkpoint failure would strand the operation lock: %v", err)
	}
	if len(steps) != 1 || steps[0] != profileDeleteStep {
		t.Fatalf("checkpoint sequence: %v", steps)
	}
	if len(f.recordedDeletes()) != 0 {
		t.Fatal("the delete preceded the durable checkpoint")
	}
}

func TestProfileDeleteAPIRejectionIsDistinctFromTransportAmbiguity(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		sentinel bool
		conflict bool
		wantText string
	}{
		{"forbidden", apierrors.NewForbidden(Profiles.GroupResource(), "alice", errors.New("denied")), true, false, "denied the Profile deletion"},
		{"conflict", apierrors.NewConflict(Profiles.GroupResource(), "alice", errors.New("stale")), true, true, "changed before the deletion"},
		{"invalid", apierrors.NewInvalid(schema.GroupKind{Group: "kubeflow.org", Kind: "Profile"}, "alice", nil), true, false, "rejected the Profile deletion"},
		{"throttled", apierrors.NewTooManyRequests("busy", 1), true, false, "throttled the Profile deletion"},
		{"network", errDeleteNetwork, false, false, "outcome is unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newProfileDeleteFixture(t)
			p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
			if err != nil {
				t.Fatal(err)
			}
			f.mu.Lock()
			f.deleteErr = tc.err
			f.mu.Unlock()
			err = f.service.executeProfileDelete(context.Background(), p, func(string) error { return nil })
			if err == nil {
				t.Fatal("accepted a failed delete")
			}
			if errors.Is(err, model.ErrRejectedBeforeMutation) != tc.sentinel {
				t.Fatalf("sentinel classification wrong for %v: %v", tc.err, err)
			}
			if errors.Is(err, ErrConflict) != tc.conflict {
				t.Fatalf("conflict classification wrong for %v: %v", tc.err, err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("missing safe explanation %q in %v", tc.wantText, err)
			}
			if tc.name == "network" && (strings.Contains(err.Error(), "boom") || strings.Contains(err.Error(), "dial")) {
				t.Fatalf("transport details leaked: %v", err)
			}
			if len(f.recordedDeletes()) != 1 {
				t.Fatalf("expected exactly one delete attempt, got %d", len(f.recordedDeletes()))
			}
		})
	}
}

func TestProfileDeleteProfileGoneNamespacePresentIsNotSuccess(t *testing.T) {
	f := newProfileDeleteFixture(t)
	f.setReactor(true, false, false, false)
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	// The delete is driven to completion deterministically: the controller
	// reactor signals that the Profile object is really gone, and only the
	// still-present namespace keeps the operation unfinished. The bounded
	// context ends the wait.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.service.executeProfileDelete(ctx, p, func(string) error { return nil }) }()
	select {
	case <-f.deleted:
	case <-time.After(10 * time.Second):
		t.Fatal("the delete was never issued")
	}
	var execErr error
	select {
	case execErr = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the execution did not finish")
	}
	if execErr == nil {
		t.Fatal("reported success while the namespace was still present")
	}
	if errors.Is(execErr, model.ErrRejectedBeforeMutation) {
		t.Fatalf("the delete was already issued: %v", execErr)
	}
	if f.profile("alice") != nil {
		t.Fatal("the Profile was not deleted")
	}
	// The console never deletes the namespace or a volume itself.
	for _, resource := range typedDeletes(f.client) {
		t.Fatalf("the console issued a %s delete", resource)
	}
}

func TestProfileDeleteNamespaceThatOutlivedItsProfileNeedsReview(t *testing.T) {
	f := newProfileDeleteFixture(t)
	f.setReactor(true, false, true, false)
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	err = f.service.executeProfileDelete(context.Background(), p, func(string) error { return nil })
	if err == nil || !errors.Is(err, ErrConflict) {
		t.Fatalf("expected a conflict, got %v", err)
	}
	if errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("the delete was already issued: %v", err)
	}
	if !strings.Contains(err.Error(), "outlived its Profile") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProfileDeleteReplacementObjectsNeedReview(t *testing.T) {
	f := newProfileDeleteFixture(t)
	f.setReactor(true, false, false, true)
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	err = f.service.executeProfileDelete(context.Background(), p, func(string) error { return nil })
	if err == nil || !errors.Is(err, ErrConflict) {
		t.Fatalf("expected a conflict, got %v", err)
	}
	if errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("the delete was already issued: %v", err)
	}
	if !strings.Contains(err.Error(), "replacement namespace") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestProfileDeleteReadErrorIsNotAbsence(t *testing.T) {
	f := newProfileDeleteFixture(t)
	f.setReactor(true, false, false, false)
	f.setNamespaceError(apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "alice", errors.New("denied")))
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err = f.service.executeProfileDelete(ctx, p, func(string) error { return nil })
	if err == nil {
		t.Fatal("a failed read was treated as absence")
	}
	if errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("the delete was already issued: %v", err)
	}
	if !strings.Contains(err.Error(), "denied the request") {
		t.Fatalf("the failed read was not reported: %v", err)
	}
	if f.profile("alice") != nil {
		t.Fatal("the Profile was not deleted")
	}
}

func TestProfileDeleteExecuteRejectsForeignActionKinds(t *testing.T) {
	f := newProfileDeleteFixture(t)
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	p.Action.Kind = "notebook.stop"
	if err = f.service.executeProfileDelete(context.Background(), p, func(string) error { return nil }); err == nil {
		t.Fatal("executed a foreign action kind")
	}
	p.Action.Kind = "profile.delete"
	p.Action.ConfirmName = "other"
	if err = f.service.executeProfileDelete(context.Background(), p, func(string) error { return nil }); err == nil {
		t.Fatal("executed without the typed confirmation")
	}
	if len(f.recordedDeletes()) != 0 {
		t.Fatal("a refused action was deleted")
	}
}

func TestProfileDeleteExecuteRejectsMissingEvidence(t *testing.T) {
	f := newProfileDeleteFixture(t)
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	p.Data = nil
	err = f.service.executeProfileDelete(context.Background(), p, func(string) error { return nil })
	if err == nil || !errors.Is(err, model.ErrRejectedBeforeMutation) || !errors.Is(err, ErrConflict) {
		t.Fatalf("expected a pre-mutation conflict, got %v", err)
	}
	if len(f.recordedDeletes()) != 0 {
		t.Fatal("a plan without evidence was deleted")
	}
}

func TestProfileDeletePolicyDigestTracksTheDeletionFeature(t *testing.T) {
	f := newProfileDeleteFixture(t)
	on := f.service.newEvidence().Policy
	f.service.Config.Features.ProfileDeletion = false
	off := f.service.newEvidence().Policy
	if on == off {
		t.Fatal("the policy digest must include Features.ProfileDeletion")
	}
}

// The fixture must never let a test claim a completed deletion without both
// objects actually being removed.
func TestProfileDeleteFixtureRemovesBothObjects(t *testing.T) {
	f := newProfileDeleteFixture(t)
	p, err := f.service.prepareProfileDelete(context.Background(), profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.executeProfileDelete(context.Background(), p, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if f.profile("alice") != nil {
		t.Fatal("the Profile fixture was not removed")
	}
	if _, err = f.client.CoreV1().Namespaces().Get(context.Background(), "alice", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the namespace fixture was not removed: %v", err)
	}
}
