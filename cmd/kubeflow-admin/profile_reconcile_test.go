package main

// profile_reconcile_test.go covers the offline reconciliation CLI.
//
// The cluster is a fake API server with reactors that refuse every request
// that is not a read, so the test proves that the reconciliation never
// mutates anything. The database is a real store, so the locks, the
// acceptance ledger and the audit rows are the real ones.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/kube"
	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/store"
)

const (
	cliProfileUID   = "profile-uid-alice"
	cliProfileRV    = "profile-rv-7"
	cliNamespaceUID = "namespace-uid-alice"
	cliClusterUID   = "cluster-uid-kube-system"
)

// cliStore builds a real store with one uncertain Profile deletion.
func cliStore(t *testing.T, checkpointed bool) (*store.Store, string) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	ctx := context.Background()
	plan := model.Plan{ID: "plan-cli-reconcile", Actor: "issuer|admin", ActorDisplay: "Alice", Prepared: model.Prepared{
		Action: model.Action{Kind: "profile.delete", Name: "alice", Reason: "pilotcleanup", ConfirmName: "alice"},
		Changes: []model.Change{
			{Field: "Profile", Before: "alice", After: "deleted"},
			{Field: "Profile UID", Before: cliProfileUID, After: "deleted"},
			{Field: "Profile resource version", Before: cliProfileRV, After: "deleted"},
			{Field: "Namespace UID", Before: cliNamespaceUID, After: "deleted"},
			{Field: "Cluster UID", Before: cliClusterUID, After: "unchanged"},
			{Field: "Profile deletion reconciliation version", Before: "1", After: "unchanged"},
		},
	}}
	now := time.Now().UTC()
	op := model.Operation{ID: "op-cli-reconcile", PlanID: plan.ID, Actor: plan.Actor, ActorDisplay: plan.ActorDisplay, Target: "profile:alice", Action: plan.Prepared.Action.Kind, State: "queued", Step: "queued", CreatedAt: now, UpdatedAt: now}
	if _, created, err := s.CreateOperation(ctx, op, plan, "cli-reconcile-acceptance-key"); err != nil || !created {
		t.Fatalf("the Profile deletion was not accepted: %v", err)
	}
	if checkpointed {
		op.State = "applying"
		op.Step = "delete-profile"
		op.ProfileDeleteCheckpointed = true
		if err := s.Checkpoint(ctx, op, model.Audit{ID: "op-cli-reconcile-checkpoint"}); err != nil {
			t.Fatal(err)
		}
	}
	op.State = "needs-review"
	if err := s.Finish(ctx, op, model.Audit{ID: "op-cli-reconcile-review"}); err != nil {
		t.Fatal(err)
	}
	return s, op.ID
}

// cliCluster is a fake cluster. The typed client and the Profile endpoint
// both refuse every request that is not a read, so the test proves the
// reconciliation never mutates the cluster.
type cliCluster struct {
	service *kube.Service
	client  *fake.Clientset
}

// cliNewCluster builds the fake cluster. A nil profile means the Profile is
// absent; a nil namespace means the Profile's namespace is absent.
func cliNewCluster(t *testing.T, clusterUID string, profile *unstructured.Unstructured, namespace *corev1.Namespace) *cliCluster {
	t.Helper()
	objects := []runtime.Object{&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kube-system", UID: types.UID(clusterUID)}}}
	if namespace != nil {
		objects = append(objects, namespace)
	}
	client := fake.NewSimpleClientset(objects...)
	mux := http.NewServeMux()
	mux.HandleFunc("/apis/kubeflow.org/v1/profiles/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("the reconciliation issued a %s request on the Profile endpoint", r.Method)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if profile == nil {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(metav1.Status{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Status"}, Status: metav1.StatusFailure, Code: http.StatusNotFound, Reason: metav1.StatusReasonNotFound, Message: "not found"})
			return
		}
		_ = json.NewEncoder(w).Encode(profile.Object)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	d, err := dynamic.NewForConfig(&rest.Config{Host: server.URL, QPS: 1000, Burst: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return &cliCluster{service: &kube.Service{Config: config.Config{}, Typed: client, Dynamic: d, PollInterval: time.Millisecond}, client: client}
}

// assertReadOnly proves the reconciliation only ever read.
func (c *cliCluster) assertReadOnly(t *testing.T) {
	t.Helper()
	for _, a := range c.client.Actions() {
		if a.GetVerb() != "get" {
			t.Errorf("the reconciliation issued a %s request on the cluster", a.GetVerb())
		}
	}
}

// cliProfileObject is the Profile the fake cluster reports.
func cliProfileObject(uid string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubeflow.org/v1",
		"kind":       "Profile",
		"metadata":   map[string]any{"name": "alice", "uid": uid, "resourceVersion": cliProfileRV},
		"spec":       map[string]any{"owner": map[string]any{"kind": "User", "name": "alice@example.test"}},
	}}
}

// cliNamespaceObject is the Profile's namespace the fake cluster reports.
func cliNamespaceObject(uid string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "alice", UID: types.UID(uid)}}
}

// cliLocks counts the locks the operation still holds.
func cliLocks(t *testing.T, s *store.Store, operationID string) int {
	t.Helper()
	plan, err := s.PlanProfileDeleteReconciliation(context.Background(), operationID)
	if err != nil {
		return 0
	}
	return len(plan.ActiveTargets)
}

// cliOptions is the parsed command line of a dry run.
func cliOptions(operationID string) profileReconcileOptions {
	return profileReconcileOptions{OperationID: operationID, ConfirmProfile: "alice", Reason: "operator verified the cluster state offline"}
}

// The dry run proposes the decision and changes nothing: the Profile and the
// namespace stay locked.
func TestProfileReconcileDryRunChangesNothing(t *testing.T) {
	defer cliShortInterval()()
	ctx := context.Background()
	s, operationID := cliStore(t, false)
	cluster := cliNewCluster(t, cliClusterUID, nil, nil)
	plan, err := s.PlanProfileDeleteReconciliation(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconcileProfileDeleteWith(ctx, s, cluster.service, plan, cliOptions(operationID)); err != nil {
		t.Fatal(err)
	}
	cluster.assertReadOnly(t)
	if locks := cliLocks(t, s, operationID); locks != 2 {
		t.Fatalf("%d locks are held after a dry run, want both", locks)
	}
	var stored model.Operation
	if err := s.Get(ctx, "operations", operationID, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.State != "needs-review" {
		t.Fatalf("the dry run changed the state to %q", stored.State)
	}
}

// The applied reconciliation of a completed deletion releases both locks.
func TestProfileReconcileApplyRecordsTheDerivedOutcome(t *testing.T) {
	defer cliShortInterval()()
	ctx := context.Background()
	s, operationID := cliStore(t, true)
	cluster := cliNewCluster(t, cliClusterUID, nil, nil)
	plan, err := s.PlanProfileDeleteReconciliation(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	opts := cliOptions(operationID)
	opts.Apply = true
	if err := reconcileProfileDeleteWith(ctx, s, cluster.service, plan, opts); err != nil {
		t.Fatal(err)
	}
	cluster.assertReadOnly(t)
	var stored model.Operation
	if err := s.Get(ctx, "operations", operationID, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.State != "succeeded" || stored.Step != "reconciled-succeeded" {
		t.Fatalf("the derived outcome is wrong: %+v", stored)
	}
	if locks := cliLocks(t, s, operationID); locks != 0 {
		t.Fatalf("%d locks are held after the reconciliation", locks)
	}
}

// The critical case: both objects are absent, but the cluster is not the
// accepted one. The reconciliation must refuse, propose nothing and keep
// every lock.
func TestProfileReconcileRefusesAnotherCluster(t *testing.T) {
	defer cliShortInterval()()
	ctx := context.Background()
	s, operationID := cliStore(t, false)
	// Another cluster: both objects are absent and the kube-system identity
	// is different.
	cluster := cliNewCluster(t, "cluster-uid-somewhere-else", nil, nil)
	plan, err := s.PlanProfileDeleteReconciliation(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	opts := cliOptions(operationID)
	opts.Apply = true
	err = reconcileProfileDeleteWith(ctx, s, cluster.service, plan, opts)
	if err == nil {
		t.Fatal("an observation from another cluster was accepted")
	}
	if !errors.Is(err, store.ErrReconcileUnsupported) {
		t.Fatalf("unexpected error: %v", err)
	}
	cluster.assertReadOnly(t)
	if locks := cliLocks(t, s, operationID); locks != 2 {
		t.Fatalf("%d locks are held after a refused reconciliation, want both", locks)
	}
	var stored model.Operation
	if err := s.Get(ctx, "operations", operationID, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.State != "needs-review" {
		t.Fatalf("a refused reconciliation changed the state to %q", stored.State)
	}
}

// A deletion whose request may have been issued is never cancelled, even
// though the objects are still there.
func TestProfileReconcileRefusesToCancelAnIssuedDeletion(t *testing.T) {
	defer cliShortInterval()()
	ctx := context.Background()
	s, operationID := cliStore(t, true)
	cluster := cliNewCluster(t, cliClusterUID, cliProfileObject(cliProfileUID), cliNamespaceObject(cliNamespaceUID))
	plan, err := s.PlanProfileDeleteReconciliation(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	opts := cliOptions(operationID)
	opts.Apply = true
	if err := reconcileProfileDeleteWith(ctx, s, cluster.service, plan, opts); !errors.Is(err, store.ErrReconcileUnsupported) {
		t.Fatalf("a deletion that may have been issued was cancelled: %v", err)
	}
	cluster.assertReadOnly(t)
	if locks := cliLocks(t, s, operationID); locks != 2 {
		t.Fatalf("%d locks are held after a refused reconciliation, want both", locks)
	}
}

// A replaced object is refused: it is a different object than the accepted
// one.
func TestProfileReconcileRefusesAReplacedObject(t *testing.T) {
	defer cliShortInterval()()
	ctx := context.Background()
	s, operationID := cliStore(t, false)
	cluster := cliNewCluster(t, cliClusterUID, cliProfileObject("profile-uid-bob"), cliNamespaceObject(cliNamespaceUID))
	plan, err := s.PlanProfileDeleteReconciliation(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	opts := cliOptions(operationID)
	opts.Apply = true
	if err := reconcileProfileDeleteWith(ctx, s, cluster.service, plan, opts); !errors.Is(err, store.ErrReconcileUnsupported) {
		t.Fatalf("a replaced Profile was reconciled: %v", err)
	}
	cluster.assertReadOnly(t)
	if locks := cliLocks(t, s, operationID); locks != 2 {
		t.Fatalf("%d locks are held after a refused reconciliation, want both", locks)
	}
}

// The operator must confirm the exact Profile the accepted deletion
// targeted.
func TestProfileReconcileRefusesAMismatchedConfirmation(t *testing.T) {
	defer cliShortInterval()()
	ctx := context.Background()
	s, operationID := cliStore(t, false)
	cluster := cliNewCluster(t, cliClusterUID, nil, nil)
	plan, err := s.PlanProfileDeleteReconciliation(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	opts := cliOptions(operationID)
	opts.ConfirmProfile = "bob"
	opts.Apply = true
	if err := reconcileProfileDeleteWith(ctx, s, cluster.service, plan, opts); err == nil {
		t.Fatal("a mismatched confirmation was accepted")
	}
	cluster.assertReadOnly(t)
	if locks := cliLocks(t, s, operationID); locks != 2 {
		t.Fatalf("%d locks are held after a refused reconciliation, want both", locks)
	}
}

// The application must be stopped: the exclusive process lock refuses a
// database that a running application owns.
func TestProfileReconcileRefusesWhileTheApplicationOwnsTheDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	owner, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	cfg := config.Config{Database: config.Database{Path: path, JournalMode: "wal"}}
	err = reconcileProfileDelete(context.Background(), cfg, profileReconcileOptions{OperationID: "op-cli-reconcile", ConfirmProfile: "alice", Reason: "operator verified the cluster state offline"})
	if !errors.Is(err, store.ErrLocked) {
		t.Fatalf("the reconciliation ran while the application owned the database: %v", err)
	}
}

// A read that succeeds without a UID is an incomplete observation, not an
// absent object. It must fail closed instead of proving a deletion that
// never happened.
func TestProfileReconcileRefusesAnObservationWithoutAProfileUID(t *testing.T) {
	defer cliShortInterval()()
	ctx := context.Background()
	s, operationID := cliStore(t, false)
	cluster := cliNewCluster(t, cliClusterUID, cliProfileObject(""), cliNamespaceObject(cliNamespaceUID))
	plan, err := s.PlanProfileDeleteReconciliation(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	opts := cliOptions(operationID)
	opts.Apply = true
	if err := reconcileProfileDeleteWith(ctx, s, cluster.service, plan, opts); err == nil {
		t.Fatal("a Profile read without a UID was accepted")
	}
	cluster.assertReadOnly(t)
	if locks := cliLocks(t, s, operationID); locks != 2 {
		t.Fatalf("%d locks are held after a refused reconciliation, want both", locks)
	}
	var stored model.Operation
	if err := s.Get(ctx, "operations", operationID, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.State != "needs-review" {
		t.Fatalf("a refused reconciliation changed the state to %q", stored.State)
	}
}

// The same rule applies to the Profile's namespace.
func TestProfileReconcileRefusesAnObservationWithoutANamespaceUID(t *testing.T) {
	defer cliShortInterval()()
	ctx := context.Background()
	s, operationID := cliStore(t, false)
	cluster := cliNewCluster(t, cliClusterUID, cliProfileObject(cliProfileUID), cliNamespaceObject(""))
	plan, err := s.PlanProfileDeleteReconciliation(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	opts := cliOptions(operationID)
	opts.Apply = true
	if err := reconcileProfileDeleteWith(ctx, s, cluster.service, plan, opts); err == nil {
		t.Fatal("a namespace read without a UID was accepted")
	}
	cluster.assertReadOnly(t)
	if locks := cliLocks(t, s, operationID); locks != 2 {
		t.Fatalf("%d locks are held after a refused reconciliation, want both", locks)
	}
}

// The cluster identity without a UID cannot prove the cluster either. The
// derived outcome refuses it, so no lock is released.
func TestProfileReconcileRefusesAnObservationWithoutAClusterIdentity(t *testing.T) {
	defer cliShortInterval()()
	ctx := context.Background()
	s, operationID := cliStore(t, false)
	cluster := cliNewCluster(t, "", nil, nil)
	plan, err := s.PlanProfileDeleteReconciliation(ctx, operationID)
	if err != nil {
		t.Fatal(err)
	}
	opts := cliOptions(operationID)
	opts.Apply = true
	if err := reconcileProfileDeleteWith(ctx, s, cluster.service, plan, opts); err == nil {
		t.Fatal("a cluster identity without a UID was accepted")
	}
	cluster.assertReadOnly(t)
	if locks := cliLocks(t, s, operationID); locks != 2 {
		t.Fatalf("%d locks are held after a refused reconciliation, want both", locks)
	}
}

// cliShortInterval shrinks the wait between the two observations and
// restores it afterwards.
func cliShortInterval() func() {
	previous := profileReconcileObservationInterval
	profileReconcileObservationInterval = time.Millisecond
	return func() { profileReconcileObservationInterval = previous }
}
