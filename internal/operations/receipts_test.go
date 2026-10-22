package operations

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/store"
)

func setupAtPath(t *testing.T, path string, b *fakeBackend) (*Manager, *store.Store, model.Identity) {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Auth: config.Auth{IssuerURL: "https://issuer", SessionMinutes: 15, AdminGroups: []string{"admins"}}, Features: config.Features{WritesEnabled: true}}
	m, err := newManager(context.Background(), cfg, s, b)
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	now := time.Now()
	digest := sha256.Sum256([]byte("receipt-test-session"))
	actor := model.Identity{SessionID: base64.RawURLEncoding.EncodeToString(digest[:]), Issuer: cfg.Auth.IssuerURL, Subject: "admin", Groups: []string{"admins"}, AuthenticatedAt: now, ExpiresAt: now.Add(15 * time.Minute)}
	if err := s.Put(context.Background(), "auth.sessions", actor.SessionID, map[string]any{"identity": actor}); err != nil {
		m.Close()
		s.Close()
		t.Fatal(err)
	}
	return m, s, actor
}

func boundReceiptAction() model.Action {
	return model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "data", PVName: "static-pv", TargetUID: "pv-uid", TargetResourceVersion: "3", Template: "nfs-a", StorageClass: "approved", PodDefault: &model.PodDefaultRequest{Name: "mount-data", MountPath: "/home/jovyan/data"}}
}

func pvcCreatedReceipt() model.MutationReceipt {
	return model.MutationReceipt{Kind: "PersistentVolumeClaim", Namespace: "alice", Name: "data", UID: "actual-pvc-uid", ResourceVersion: "42", Outcome: model.MutationOutcomeCreated}
}

// A real PVC identity recorded before a later PodDefault failure remains in the
// operation after transient plan cleanup, needs-review and a process restart.
func TestPVCCreateBoundReceiptSurvivesPlanExpiryReopenNeedsReview(t *testing.T) {
	path := t.TempDir() + "/receipts.db"
	b := &fakeBackend{execute: func(ctx context.Context, p model.Prepared, c func(string) error) error {
		if err := c("create-pvc"); err != nil {
			return err
		}
		if err := model.RecordMutationReceipt(ctx, pvcCreatedReceipt()); err != nil {
			return err
		}
		if err := c("create-poddefault"); err != nil {
			return err
		}
		return errors.New("PodDefault write failed after the PVC was created")
	}}
	m, s, actor := setupAtPath(t, path, b)
	t.Cleanup(func() { m.Close(); s.Close() })
	ctx := context.Background()
	plan, err := m.Plan(ctx, actor, boundReceiptAction())
	if err != nil {
		t.Fatal(err)
	}
	op, err := m.Submit(ctx, actor, plan.ID, "receipt-survive-key")
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, m, op.ID)
	if got.State != "needs-review" {
		t.Fatalf("operation = %+v", got)
	}
	if len(got.MutationReceipts) != 1 || got.MutationReceipts[0] != pvcCreatedReceipt() {
		t.Fatalf("mutation receipts = %+v", got.MutationReceipts)
	}
	// Expire and remove the transient plan, then restart.
	plan.ExpiresAt = time.Now().Add(-time.Hour)
	if err := s.Put(ctx, "plans", plan.ID, plan); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupPlans(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	m.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	replayed := atomic.Bool{}
	b2 := &fakeBackend{execute: func(context.Context, model.Prepared, func(string) error) error {
		replayed.Store(true)
		return nil
	}}
	m2, s2, _ := setupAtPath(t, path, b2)
	t.Cleanup(func() { m2.Close(); s2.Close() })
	got2, err := m2.Get(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.State != "needs-review" || len(got2.MutationReceipts) != 1 || got2.MutationReceipts[0] != pvcCreatedReceipt() {
		t.Fatalf("receipt did not survive reopen: %+v", got2)
	}
	if replayed.Load() {
		t.Fatal("startup replayed a needs-review operation")
	}
	if err := s2.CheckIntegrity(ctx); err != nil {
		t.Fatalf("reopened operation failed integrity: %v", err)
	}
}

// A failed receipt checkpoint refuses the next backend checkpoint/write even
// when the backend ignores the first error, and the operation keeps its locks.
// A later successful Finish retains the known receipt despite the earlier
// checkpoint failure; this never authorizes another cluster write.
func TestReceiptPersistenceFailureStopsNextWriteAndRetainsLocks(t *testing.T) {
	var nextWrite atomic.Bool
	b := &fakeBackend{execute: func(ctx context.Context, p model.Prepared, c func(string) error) error {
		if err := c("create-pvc"); err != nil {
			return err
		}
		receiptErr := model.RecordMutationReceipt(ctx, pvcCreatedReceipt())
		if receiptErr != nil {
			if ctx.Err() == nil {
				t.Error("failed receipt persistence did not cancel further API requests")
			}
			// Deliberately try to continue: the manager must refuse the next
			// checkpoint, so this write must not happen.
			if err := c("create-poddefault"); err == nil {
				nextWrite.Store(true)
			}
			return receiptErr
		}
		return nil
	}}
	m, s, actor := setup(t, b)
	m.persistReceipt = func(context.Context, model.Operation, model.Audit) error {
		return errors.New("simulated receipt storage failure")
	}
	ctx := context.Background()
	plan, err := m.Plan(ctx, actor, boundReceiptAction())
	if err != nil {
		t.Fatal(err)
	}
	op, err := m.Submit(ctx, actor, plan.ID, "receipt-failure-key")
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, m, op.ID)
	if got.State != "needs-review" {
		t.Fatalf("operation = %+v", got)
	}
	if nextWrite.Load() {
		t.Fatal("a cluster write followed failed receipt persistence")
	}
	if len(got.MutationReceipts) != 1 || got.MutationReceipts[0] != pvcCreatedReceipt() {
		t.Fatalf("successful needs-review Finish lost the known API outcome: %+v", got.MutationReceipts)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("failed receipt operation lost integrity/locks: %v", err)
	}
	// The claim, selected PV and PodDefault locks remain held.
	conflictPlan := model.Plan{ID: "receipt-failure-follow-up", Actor: plan.Actor, Prepared: model.Prepared{Action: boundReceiptAction()}}
	conflictPlan.Prepared.Action.Name = "other"
	conflictOp := model.Operation{ID: "receipt-failure-follow-up-op", PlanID: conflictPlan.ID, Actor: conflictPlan.Actor, Target: store.PrimaryTarget(conflictPlan.Prepared.Action), Action: conflictPlan.Prepared.Action.Kind, State: "queued"}
	if _, _, err := s.CreateOperation(ctx, conflictOp, conflictPlan, "receipt-failure-follow-up-key"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("failed receipt persistence released dependency locks: %v", err)
	}
}

// The initial validating checkpoint is legitimate pre-mutation state: a clean
// rejection without a receipt must still finish failed and release locks.
func TestCleanPreMutationRejectionStillReleasesLocks(t *testing.T) {
	b := &fakeBackend{execute: func(context.Context, model.Prepared, func(string) error) error {
		return errors.Join(model.ErrRejectedBeforeMutation, errors.New("rejected by API precondition"))
	}}
	m, _, actor := setup(t, b)
	ctx := context.Background()
	plan, err := m.Plan(ctx, actor, boundReceiptAction())
	if err != nil {
		t.Fatal(err)
	}
	op, err := m.Submit(ctx, actor, plan.ID, "clean-rejection-key")
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, m, op.ID)
	if got.State != "failed" || len(got.MutationReceipts) != 0 {
		t.Fatalf("clean rejection = %+v", got)
	}
}

func TestReceiptProofPreventsCleanRejectionClassification(t *testing.T) {
	b := &fakeBackend{execute: func(ctx context.Context, p model.Prepared, c func(string) error) error {
		if err := c("create-pvc"); err != nil {
			return err
		}
		if err := model.RecordMutationReceipt(ctx, pvcCreatedReceipt()); err != nil {
			return err
		}
		return errors.Join(model.ErrRejectedBeforeMutation, errors.New("backend misclassified the write"))
	}}
	m, s, actor := setup(t, b)
	ctx := context.Background()
	plan, err := m.Plan(ctx, actor, boundReceiptAction())
	if err != nil {
		t.Fatal(err)
	}
	op, err := m.Submit(ctx, actor, plan.ID, "receipt-sentinel-key")
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, m, op.ID)
	if got.State != "needs-review" {
		t.Fatalf("receipt-backed sentinel was treated as a clean rejection: %+v", got)
	}
	if len(got.MutationReceipts) != 1 {
		t.Fatalf("receipt proof lost: %+v", got.MutationReceipts)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("receipt-backed rejection failed integrity: %v", err)
	}
}

func TestReceiptBackedSuccessfulOperationRemainsSucceeded(t *testing.T) {
	b := &fakeBackend{execute: func(ctx context.Context, p model.Prepared, c func(string) error) error {
		if err := c("create-pvc"); err != nil {
			return err
		}
		return model.RecordMutationReceipt(ctx, pvcCreatedReceipt())
	}}
	m, _, actor := setup(t, b)
	ctx := context.Background()
	plan, err := m.Plan(ctx, actor, boundReceiptAction())
	if err != nil {
		t.Fatal(err)
	}
	op, err := m.Submit(ctx, actor, plan.ID, "receipt-success-key")
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, m, op.ID)
	if got.State != "succeeded" || len(got.MutationReceipts) != 1 || got.MutationReceipts[0] != pvcCreatedReceipt() {
		t.Fatalf("receipt-backed success = %+v", got)
	}
}
