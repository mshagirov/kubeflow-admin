package operations

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/store"
)

func TestStorageProvisioningGuidanceIsTypedAndNonReplaying(t *testing.T) {
	cases := map[string]string{
		"pv.create.static":      pvCreateStaticReviewGuidance,
		"pvc.create.bound":      pvcCreateBoundReviewGuidance,
		"pvc.create.dynamic":    pvcCreateDynamicReviewGuidance,
		"storage.create.static": storageCreateStaticReviewGuidance,
	}
	for kind, want := range cases {
		t.Run(kind, func(t *testing.T) {
			got := reviewGuidance(kind)
			if got != want {
				t.Fatalf("%s guidance = %q, want typed guidance", kind, got)
			}
			for _, required := range []string{"UID", "Do not retry", "No rollback or automatic replay", "locked pending explicit operator reconciliation"} {
				if !strings.Contains(got, required) {
					t.Fatalf("%s guidance omits %q: %s", kind, required, got)
				}
			}
			if strings.Contains(got, genericReviewGuidance) {
				t.Fatalf("%s retained the generic guidance", kind)
			}
			recovery := recoveryGuidance(kind)
			if !strings.Contains(recovery, "no action was replayed") || !strings.Contains(recovery, "nothing will be resumed automatically") {
				t.Fatalf("%s recovery guidance is unsafe: %s", kind, recovery)
			}
		})
	}
	if reviewGuidance("pvc.expand") != genericReviewGuidance {
		t.Fatal("legacy generic guidance changed")
	}
}

// Manager audit history is written from the store's redacted acceptance
// evidence, not from the raw review preview.
func TestProvisioningAuditRedactsEnteredPaths(t *testing.T) {
	rawPath := "/exports/private-team/alice/secret"
	b := &fakeBackend{prepare: func(ctx context.Context, a model.Action) (model.Prepared, error) {
		return model.Prepared{Action: a, TargetUID: "uid", Summary: "safe static PV plan", Changes: []model.Change{
			{Field: "PersistentVolume", Before: "(new)", After: "static-pv"},
			{Field: "NFS path", Before: "(entered)", After: rawPath},
		}}, nil
	}}
	m, _, actor := setup(t, b)
	ctx := context.Background()
	action := model.Action{Kind: "pv.create.static", Name: "static-pv", Template: "nfs-a", StorageClass: "approved", NFSPath: rawPath}
	plan, err := m.Plan(ctx, actor, action)
	if err != nil {
		t.Fatal(err)
	}
	op, err := m.Submit(ctx, actor, plan.ID, "redacted-audit-key")
	if err != nil {
		t.Fatal(err)
	}
	if got := waitOperation(t, m, op.ID); got.State != "succeeded" {
		t.Fatalf("operation = %+v", got)
	}
	audits, err := m.Audit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(audits)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), rawPath) || strings.Contains(string(encoded), "private-team") {
		t.Fatalf("audit history leaked the entered path: %s", encoded)
	}
	if !strings.Contains(string(encoded), "sha256:") || !strings.Contains(string(encoded), "static-pv") {
		t.Fatalf("audit history lost safe evidence: %s", encoded)
	}
}

func TestPVCCreateBoundPodDefaultPartialFailureKeepsLocksForReview(t *testing.T) {
	sensitive := "PODDEFAULT_PRIVATE_VALUE=/private/backend/path"
	executions := 0
	b := &fakeBackend{execute: func(ctx context.Context, p model.Prepared, c func(string) error) error {
		executions++
		if err := c("create-pvc"); err != nil {
			return err
		}
		if err := c("create-poddefault"); err != nil {
			return err
		}
		return errors.New(sensitive)
	}}
	m, s, actor := setup(t, b)
	ctx := context.Background()
	action := model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "data", PVName: "static-pv", TargetUID: "pv-uid", TargetResourceVersion: "3", Template: "nfs-a", StorageClass: "approved", NFSPath: "/exports/alice/data", DirectoryUID: "1000", DirectoryGID: "1000", PodDefault: &model.PodDefaultRequest{Name: "mount-data", MountPath: "/home/jovyan/data"}}
	plan, err := m.Plan(ctx, actor, action)
	if err != nil {
		t.Fatal(err)
	}
	op, err := m.Submit(ctx, actor, plan.ID, "bound-partial-key")
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, m, op.ID)
	if got.State != "needs-review" {
		t.Fatalf("partial PodDefault outcome = %+v", got)
	}
	if strings.Contains(got.Error, "PRIVATE") || strings.Contains(got.Error, "/private") {
		t.Fatal("outcome echoed backend detail")
	}
	if !strings.Contains(got.Error, "PodDefault") || !strings.Contains(got.Error, "partially applied") {
		t.Fatalf("outcome lacks typed PodDefault guidance: %s", got.Error)
	}
	if executions != 1 {
		t.Fatalf("execution count = %d", executions)
	}
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	// The store keeps the claim, selected PV and PodDefault locks. A fresh
	// operation that collides with all three proves the retained lock set.
	conflictPlan := model.Plan{ID: "follow-up-plan", Actor: plan.Actor, Prepared: model.Prepared{Action: model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "other", PVName: "static-pv", TargetUID: "pv-uid", TargetResourceVersion: "3", PodDefault: &model.PodDefaultRequest{Name: "mount-data", MountPath: "/home/jovyan/data"}}}}
	conflictOp := model.Operation{ID: "follow-up-op", PlanID: conflictPlan.ID, Actor: conflictPlan.Actor, Target: store.PrimaryTarget(conflictPlan.Prepared.Action), Action: conflictPlan.Prepared.Action.Kind, State: "queued"}
	if _, _, err := s.CreateOperation(ctx, conflictOp, conflictPlan, "follow-up-key"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("needs-review did not retain its dependency locks: %v", err)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("needs-review operation failed integrity: %v", err)
	}
	// An idempotent retry resolves to the same accepted operation; it never
	// starts another execution.
	again, err := m.Submit(ctx, actor, plan.ID, "bound-partial-key")
	if err != nil || again.ID != op.ID || again.State != "needs-review" {
		t.Fatalf("retry = %+v, %v", again, err)
	}
	if executions != 1 {
		t.Fatalf("retry re-executed the partial operation: %d", executions)
	}
}

func TestPVCDeleteCleanupPartialFailureKeepsDependencyLocks(t *testing.T) {
	executions := 0
	b := &fakeBackend{execute: func(ctx context.Context, p model.Prepared, c func(string) error) error {
		executions++
		if err := c("delete-poddefaults"); err != nil {
			return err
		}
		return errors.New("ambiguous transport after PodDefault deletions")
	}}
	b.prepare = func(ctx context.Context, a model.Action) (model.Prepared, error) {
		data, err := json.Marshal(map[string]any{"version": 2, "podDefaults": []map[string]string{{"name": "mount-a", "uid": "uid-a", "resourceVersion": "1"}, {"name": "mount-b", "uid": "uid-b", "resourceVersion": "1"}}})
		if err != nil {
			return model.Prepared{}, err
		}
		return model.Prepared{Action: a, TargetUID: a.TargetUID, ResourceVersion: a.TargetResourceVersion, Summary: "safe cleanup diff", Data: data}, nil
	}
	m, s, actor := setup(t, b)
	ctx := context.Background()
	action := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "data", TargetUID: "pvc-uid", TargetResourceVersion: "9", ConfirmName: "data", Reason: "verified unused", ConfirmPodDefaultCleanup: true}
	plan, err := m.Plan(ctx, actor, action)
	if err != nil {
		t.Fatal(err)
	}
	op, err := m.Submit(ctx, actor, plan.ID, "cleanup-partial-key")
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, m, op.ID)
	if got.State != "needs-review" {
		t.Fatalf("partial cleanup outcome = %+v", got)
	}
	if !strings.Contains(got.Error, "PodDefault") || !strings.Contains(got.Error, "namespace fence") {
		t.Fatalf("cleanup guidance is not typed: %s", got.Error)
	}
	if executions != 1 {
		t.Fatalf("cleanup executions = %d", executions)
	}
	// The namespace fence and both reviewed PodDefault locks remain durable.
	createPlan := model.Plan{ID: "cleanup-follow-up", Actor: plan.Actor, Prepared: model.Prepared{Action: model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "new", PVName: "new-pv", TargetUID: "new-pv-uid", TargetResourceVersion: "1"}}}
	createOp := model.Operation{ID: "cleanup-follow-up-op", PlanID: createPlan.ID, Actor: createPlan.Actor, Target: store.PrimaryTarget(createPlan.Prepared.Action), Action: createPlan.Prepared.Action.Kind, State: "queued"}
	if _, _, err := s.CreateOperation(ctx, createOp, createPlan, "cleanup-follow-up-key"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("cleanup released the namespace fence: %v", err)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("partial cleanup failed integrity: %v", err)
	}
	again, err := m.Submit(ctx, actor, plan.ID, "cleanup-partial-key")
	if err != nil || again.ID != op.ID || again.State != "needs-review" {
		t.Fatalf("cleanup retry = %+v, %v", again, err)
	}
	if executions != 1 {
		t.Fatalf("cleanup retry re-executed: %d", executions)
	}
}

func TestPVCreateStaticPartialFailureUsesTypedOutcome(t *testing.T) {
	b := &fakeBackend{execute: func(ctx context.Context, p model.Prepared, c func(string) error) error {
		if err := c("create-pv"); err != nil {
			return err
		}
		return errors.New("ambiguous create response")
	}}
	m, s, actor := setup(t, b)
	ctx := context.Background()
	action := model.Action{Kind: "pv.create.static", Name: "static-pv", Template: "nfs-a", StorageClass: "approved", NFSPath: "/exports/alice/data"}
	plan, err := m.Plan(ctx, actor, action)
	if err != nil {
		t.Fatal(err)
	}
	op, err := m.Submit(ctx, actor, plan.ID, "pv-static-partial-key")
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, m, op.ID)
	if got.State != "needs-review" || !strings.Contains(got.Error, "PersistentVolume") {
		t.Fatalf("static PV outcome = %+v", got)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("static PV partial operation failed integrity: %v", err)
	}
	conflict := model.Plan{ID: "pv-static-follow-up", Actor: plan.Actor, Prepared: model.Prepared{Action: model.Action{Kind: "pv.create.static", Name: "static-pv", Template: "nfs-a", StorageClass: "approved"}}}
	conflictOp := model.Operation{ID: "pv-static-follow-up-op", PlanID: conflict.ID, Actor: conflict.Actor, Target: store.PrimaryTarget(conflict.Prepared.Action), Action: conflict.Prepared.Action.Kind, State: "queued"}
	if _, _, err := s.CreateOperation(ctx, conflictOp, conflict, "pv-static-follow-up-key"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("partial PV creation released its name lock: %v", err)
	}
}
