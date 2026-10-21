package operations

// profile_reconcile_test.go covers the durable pre-delete checkpoint marker.
//
// The marker is the proof that a Profile delete request may have been
// issued. It is written with the durable checkpoint that immediately
// precedes the single delete request, it survives a crash and it can never
// be cleared. Only the marker lets an offline reconciliation cancel an
// uncertain deletion; without it, a deletion that may have been issued stays
// locked for an operator to review.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/store"
)

// markerBackend is the fake Kubernetes adapter for the marker tests. It
// checkpoints exactly like the real Profile deletion does, then either
// reports a definite refusal before the delete request or leaves the outcome
// unknown after it.
type markerBackend struct {
	mu sync.Mutex
	// deleteRequests counts the delete requests the adapter issued.
	deleteRequests int
	// refuse makes the adapter fail before the delete request.
	refuse bool
}

func (b *markerBackend) Prepare(_ context.Context, a model.Action) (model.Prepared, error) {
	// The accepted evidence carries the reviewed identities, exactly like the
	// real deletion preview does.
	return model.Prepared{
		Action:    a,
		TargetUID: "profile-uid-alice",
		Summary:   "profile.delete alice",
		Changes: []model.Change{
			{Field: "Profile UID", Before: "profile-uid-alice", After: "deleted"},
			{Field: "Profile resource version", Before: "profile-rv-7", After: "deleted"},
			{Field: "Namespace UID", Before: "namespace-uid-alice", After: "deleted"},
			{Field: "Cluster UID", Before: "cluster-uid-kube-system", After: "unchanged"},
			{Field: "Profile deletion reconciliation version", Before: "1", After: "unchanged"},
		},
	}, nil
}

func (b *markerBackend) Execute(_ context.Context, p model.Prepared, c func(string) error) error {
	if b.refuse {
		return errors.Join(model.ErrRejectedBeforeMutation, errors.New("Kubernetes API denied the Profile deletion"))
	}
	if err := c("delete-profile"); err != nil {
		return errors.Join(model.ErrRejectedBeforeMutation, err)
	}
	b.mu.Lock()
	b.deleteRequests++
	b.mu.Unlock()
	// The request was issued and the outcome is unknown.
	return errors.New("the Profile deletion outcome is unknown")
}

func (b *markerBackend) deletes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.deleteRequests
}

// The durable marker is written before the delete request is issued.
func TestProfileDeleteCheckpointIsRecordedBeforeTheDeleteRequest(t *testing.T) {
	ctx := context.Background()
	backend := &markerBackend{}
	m, s, actor := setup(t, &fakeBackend{prepare: backend.Prepare, execute: backend.Execute})
	op, err := m.Submit(ctx, actor, planProfileDelete(t, m, actor).ID, "marker-checkpoint-key")
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, m, op.ID)
	if got.State != "needs-review" {
		t.Fatalf("state = %q, want needs-review", got.State)
	}
	if backend.deletes() != 1 {
		t.Fatalf("delete requests = %d, want exactly one", backend.deletes())
	}
	var stored model.Operation
	if err := s.Get(ctx, "operations", op.ID, &stored); err != nil {
		t.Fatal(err)
	}
	if !stored.ProfileDeleteCheckpointed {
		t.Fatal("the pre-delete checkpoint was not recorded as a durable marker")
	}
	// The marker is present in the record, so its absence could never be
	// mistaken for "no write happened".
	plan, err := s.PlanProfileDeleteReconciliation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.MarkerPresent {
		t.Fatal("the durable record does not carry the marker")
	}
	if plan.NoWriteProven {
		t.Fatal("a checkpointed deletion proves that no write was started")
	}
}

// A deletion that was refused before the delete request never sets the
// marker: no write was ever started.
func TestProfileDeleteWithoutAWriteKeepsTheMarkerFalse(t *testing.T) {
	ctx := context.Background()
	backend := &markerBackend{refuse: true}
	m, s, actor := setup(t, &fakeBackend{prepare: backend.Prepare, execute: backend.Execute})
	op, err := m.Submit(ctx, actor, planProfileDelete(t, m, actor).ID, "marker-none-written-key")
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, m, op.ID)
	if got.State != "failed" {
		t.Fatalf("state = %q, want failed", got.State)
	}
	if backend.deletes() != 0 {
		t.Fatalf("%d delete requests were issued after a definite rejection", backend.deletes())
	}
	var stored model.Operation
	if err := s.Get(ctx, "operations", op.ID, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.ProfileDeleteCheckpointed {
		t.Fatal("a deletion that never issued a delete request set the marker")
	}
}

// The marker survives a crash. An operation that was interrupted after the
// pre-delete checkpoint is recovered as uncertain, and an uncertain operation
// whose delete request may have been issued can never be cancelled.
func TestProfileDeleteCheckpointMarkerSurvivesRecovery(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir() + "/data.db")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// The accepted deletion, checkpointed exactly like the manager's
	// pre-delete checkpoint writes it, and then abandoned by a crash.
	plan := model.Plan{ID: "plan-marker-recovery", Actor: "issuer|admin", ActorDisplay: "Alice", Prepared: model.Prepared{
		Action: model.Action{Kind: "profile.delete", Name: "alice", Reason: "pilotcleanup", ConfirmName: "alice"},
		Changes: []model.Change{
			{Field: "Profile UID", Before: "profile-uid-alice", After: "deleted"},
			{Field: "Profile resource version", Before: "profile-rv-7", After: "deleted"},
			{Field: "Namespace UID", Before: "namespace-uid-alice", After: "deleted"},
			{Field: "Cluster UID", Before: "cluster-uid-kube-system", After: "unchanged"},
			{Field: "Profile deletion reconciliation version", Before: "1", After: "unchanged"},
		},
	}}
	now := time.Now().UTC()
	op := model.Operation{ID: "op-marker-recovery", PlanID: plan.ID, Actor: plan.Actor, ActorDisplay: plan.ActorDisplay, Target: store.PrimaryTarget(plan.Prepared.Action), Action: plan.Prepared.Action.Kind, State: "queued", Step: "queued", CreatedAt: now, UpdatedAt: now}
	if _, created, err := s.CreateOperation(ctx, op, plan, "marker-recovery-key"); err != nil || !created {
		t.Fatal(err)
	}
	op.State = "applying"
	op.Step = "delete-profile"
	op.ProfileDeleteCheckpointed = true
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "marker-recovery-checkpoint"}); err != nil {
		t.Fatal(err)
	}
	// A fresh worker recovers the interrupted operation.
	m, err := newManager(ctx, config.Config{Auth: config.Auth{IssuerURL: "https://issuer", SessionMinutes: 15, AdminGroups: []string{"admins"}}, Features: config.Features{WritesEnabled: true}}, s, &fakeBackend{execute: func(context.Context, model.Prepared, func(string) error) error {
		t.Error("the recovery executed the deletion again")
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	var stored model.Operation
	if err := s.Get(ctx, "operations", op.ID, &stored); err != nil {
		t.Fatal(err)
	}
	if !stored.ProfileDeleteCheckpointed {
		t.Fatal("the marker did not survive the recovery")
	}
	if stored.State != "needs-review" {
		t.Fatalf("the recovered state is %q, want needs-review", stored.State)
	}
	evidence, err := s.PlanProfileDeleteReconciliation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.NoWriteProven {
		t.Fatal("a recovered deletion with the marker set proves that no write was started")
	}
	// The uncertainty is bounded: the locks are still held.
	mutation := model.Plan{ID: "plan-child-after-recovery", Actor: plan.Actor, Prepared: model.Prepared{Action: model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "lab", Reason: "routine change"}}}
	child := model.Operation{ID: "child-after-recovery", PlanID: mutation.ID, Actor: mutation.Actor, Target: store.PrimaryTarget(mutation.Prepared.Action), Action: mutation.Prepared.Action.Kind, State: "queued", Step: "queued"}
	if _, _, err := s.CreateOperation(ctx, child, mutation, "child-after-recovery-key"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("the namespace was unfenced by the recovery: %v", err)
	}
}
