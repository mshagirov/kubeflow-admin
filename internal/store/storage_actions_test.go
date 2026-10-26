package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"kubeflow-admin/internal/model"
)

func storageDeletePlan(id string, a model.Action) model.Plan {
	return model.Plan{ID: id, Actor: "issuer|admin", Prepared: model.Prepared{Action: a}}
}

func storageDeleteOp(plan model.Plan, id string) model.Operation {
	return model.Operation{ID: id, PlanID: plan.ID, Actor: plan.Actor, Target: PrimaryTarget(plan.Prepared.Action), Action: plan.Prepared.Action.Kind, State: "queued", CreatedAt: time.Now(), UpdatedAt: time.Now()}
}

func TestStorageTargetGrammarAndValidation(t *testing.T) {
	if got := PrimaryTarget(model.Action{Kind: "pv.edit", Name: "vol", Namespace: "ignored"}); got != "pv:vol" {
		t.Fatalf("PV target is not canonical: %s", got)
	}
	if got := PrimaryTarget(model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "claim"}); got != "pvc:alice/claim" {
		t.Fatalf("PVC target is not canonical: %s", got)
	}
	s := database(t)
	ctx := context.Background()

	validPV := model.Action{Kind: "pv.edit", Name: "vol", StorageYAML: "apiVersion: v1", TargetUID: "uid", TargetResourceVersion: "1"}
	plan := storageDeletePlan("pv-plan", validPV)
	if _, created, err := s.CreateOperation(ctx, storageDeleteOp(plan, "pv-op"), plan, "pv-edit-key"); err != nil || !created {
		t.Fatalf("valid PV edit target rejected: %v", err)
	}

	for name, action := range map[string]model.Action{
		"missing yaml":     {Kind: "pv.edit", Name: "vol", TargetUID: "uid", TargetResourceVersion: "1"},
		"missing uid":      {Kind: "pv.edit", Name: "vol", StorageYAML: "apiVersion: v1", TargetResourceVersion: "1"},
		"force":            {Kind: "pvc.edit", Namespace: "alice", Name: "claim", StorageYAML: "apiVersion: v1", TargetUID: "uid", TargetResourceVersion: "1", Force: true},
		"pv namespace":     {Kind: "pv.delete", Namespace: "alice", Name: "vol", TargetUID: "uid", TargetResourceVersion: "1", ConfirmName: "vol", Reason: "unused"},
		"missing reason":   {Kind: "pvc.delete", Namespace: "alice", Name: "claim", TargetUID: "uid", TargetResourceVersion: "1", ConfirmName: "claim"},
		"wrong confirm":    {Kind: "pvc.delete", Namespace: "alice", Name: "claim", TargetUID: "uid", TargetResourceVersion: "1", ConfirmName: "other", Reason: "unused"},
		"delete with yaml": {Kind: "pvc.delete", Namespace: "alice", Name: "claim", TargetUID: "uid", TargetResourceVersion: "1", ConfirmName: "claim", Reason: "unused", StorageYAML: "apiVersion: v1"},
	} {
		t.Run(name, func(t *testing.T) {
			p := storageDeletePlan("plan-"+name, action)
			if _, _, err := s.CreateOperation(ctx, storageDeleteOp(p, "op-"+name), p, "key-"+name); !errors.Is(err, ErrInvalidTarget) {
				t.Fatalf("invalid action accepted: %v", err)
			}
		})
	}
}

func TestPVCDeleteExclusiveNamespaceFence(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	deleteAction := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "claim", TargetUID: "claim-uid", TargetResourceVersion: "1", ConfirmName: "claim", Reason: "verified unused"}
	plan := storageDeletePlan("delete-plan", deleteAction)
	targets := Targets(plan)
	foundNamespace := false
	for _, target := range targets {
		if target == "namespace:alice" {
			foundNamespace = true
		}
	}
	if !foundNamespace {
		t.Fatalf("PVC delete did not acquire the namespace fence: %v", targets)
	}
	if _, created, err := s.CreateOperation(ctx, storageDeleteOp(plan, "delete-op"), plan, "delete-key"); err != nil || !created {
		t.Fatal(err)
	}

	notebook := model.Plan{ID: "notebook-plan", Actor: "issuer|admin", Prepared: model.Prepared{Action: model.Action{Kind: "notebook.edit", Namespace: "alice", Name: "note"}}}
	if _, _, err := s.CreateOperation(ctx, storageDeleteOp(notebook, "notebook-op"), notebook, "notebook-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Notebook edit raced a PVC deletion: %v", err)
	}

	// Release the deletion and prove the fence is not permanent.
	op := storageDeleteOp(plan, "delete-op")
	op.State = "failed"
	op.UpdatedAt = time.Now()
	if err := s.Finish(ctx, op, model.Audit{ID: "delete-finish"}); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.CreateOperation(ctx, storageDeleteOp(notebook, "notebook-op"), notebook, "notebook-key"); err != nil || !created {
		t.Fatalf("fence was not released: %v", err)
	}

	// The reverse direction: a PVC delete may not start while another
	// namespace-scoped mutation is active.
	second := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "other", TargetUID: "other-uid", TargetResourceVersion: "1", ConfirmName: "other", Reason: "verified unused"}
	secondPlan := storageDeletePlan("second-plan", second)
	if _, _, err := s.CreateOperation(ctx, storageDeleteOp(secondPlan, "second-op"), secondPlan, "second-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("PVC delete raced an active Notebook edit: %v", err)
	}
}

func TestPVCDeleteUncertainOutcomeKeepsNamespaceFence(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "claim", TargetUID: "claim-uid", TargetResourceVersion: "1", ConfirmName: "claim", Reason: "verified unused"}
	plan := storageDeletePlan("uncertain-plan", a)
	op := storageDeleteOp(plan, "uncertain-op")
	if _, created, err := s.CreateOperation(ctx, op, plan, "uncertain-key"); err != nil || !created {
		t.Fatal(err)
	}
	op.State = "needs-review"
	op.UpdatedAt = time.Now()
	if err := s.Finish(ctx, op, model.Audit{ID: "uncertain-finish"}); err != nil {
		t.Fatal(err)
	}
	notebook := model.Plan{ID: "after-uncertain", Actor: "issuer|admin", Prepared: model.Prepared{Action: model.Action{Kind: "notebook.edit", Namespace: "alice", Name: "note"}}}
	if _, _, err := s.CreateOperation(ctx, storageDeleteOp(notebook, "after-uncertain-op"), notebook, "after-uncertain-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("uncertain PVC deletion released its namespace fence: %v", err)
	}
}

func TestNamespaceFenceIntegrityRequiresExclusiveDeletion(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "claim", TargetUID: "claim-uid", TargetResourceVersion: "1", ConfirmName: "claim", Reason: "verified unused"}
	plan := storageDeletePlan("integrity-plan", a)
	if _, created, err := s.CreateOperation(ctx, storageDeleteOp(plan, "integrity-op"), plan, "integrity-key"); err != nil || !created {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("valid PVC delete fence failed integrity: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE operation_targets SET active=0 WHERE operation_id='integrity-op' AND target='namespace:alice'`); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); err == nil {
		t.Fatal("missing PVC delete namespace fence passed integrity")
	}
}

// A PVC delete that reclaims a Retain volume after deletion fences that PV like
// any other PV write, so a concurrent PV operation cannot race the claimRef
// removal. Evidence without a reclaim volume keeps the historical lock shape.
func TestPVCDeleteReclaimVolumeIsFenced(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	a := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "claim", TargetUID: "claim-uid", TargetResourceVersion: "1", ConfirmName: "claim", Reason: "verified unused"}
	plan := storageDeletePlan("reclaim-plan", a)
	if targets := Targets(plan); len(targets) != 2 {
		t.Fatalf("legacy evidence changed the lock shape: %v", targets)
	}
	plan.Prepared.Data = json.RawMessage(`{"version":1,"reclaimVolume":"retained-pv","reclaimVolumeUID":"pv-uid","podDefaults":[]}`)
	plan.Prepared.Action.ConfirmPVReclaim = true
	found := false
	for _, target := range Targets(plan) {
		found = found || target == "pv:retained-pv"
	}
	if !found {
		t.Fatalf("reclaim volume was not locked: %v", Targets(plan))
	}
	if _, created, err := s.CreateOperation(ctx, storageDeleteOp(plan, "reclaim-op"), plan, "reclaim-key"); err != nil || !created {
		t.Fatal(err)
	}
	edit := storageDeletePlan("pv-edit-plan", model.Action{Kind: "pv.delete", Name: "retained-pv", TargetUID: "pv-uid", TargetResourceVersion: "1", ConfirmName: "retained-pv", Reason: "unused"})
	if _, _, err := s.CreateOperation(ctx, storageDeleteOp(edit, "pv-op"), edit, "pv-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("a PV operation raced the reclaim: %v", err)
	}
	for name, data := range map[string]string{
		"invalid name":   `{"reclaimVolume":"Bad/Name","reclaimVolumeUID":"pv-uid"}`,
		"missing uid":    `{"reclaimVolume":"retained-pv"}`,
		"uid only":       `{"reclaimVolumeUID":"pv-uid"}`,
		"non-string pv":  `{"reclaimVolume":7,"reclaimVolumeUID":"pv-uid"}`,
		"empty uid":      `{"reclaimVolume":"retained-pv","reclaimVolumeUID":""}`,
		"empty pv value": `{"reclaimVolume":"","reclaimVolumeUID":"pv-uid"}`,
	} {
		t.Run(name, func(t *testing.T) {
			bad := storageDeletePlan("bad-"+name, a)
			bad.Prepared.Data = json.RawMessage(data)
			if _, err := targets(bad); err == nil {
				t.Fatal("malformed reclaim evidence produced a lock set")
			}
		})
	}
}
