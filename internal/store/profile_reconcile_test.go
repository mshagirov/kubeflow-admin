package store

// profile_reconcile_test.go covers the offline reconciliation of an uncertain
// Profile deletion.
//
// Every test drives the real store: the real acceptance transaction, the real
// immutable acceptance ledger, the real lock table and the real audit rows.
// No cluster is involved: the reconciliation only ever consumes the
// observations the operator supplies.

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"kubeflow-admin/internal/model"
)

// The reviewed identities of one accepted Profile deletion.
const (
	reconcileProfileUID   = "profile-uid-alice"
	reconcileProfileRV    = "profile-rv-7"
	reconcileNamespaceUID = "namespace-uid-alice"
	reconcileClusterUID   = "cluster-uid-kube-system"
)

// reconcilePlan builds the accepted plan of a Profile deletion whose
// acceptance evidence carries the reviewed identities.
func reconcilePlan(name string, changes []model.Change) model.Plan {
	return model.Plan{
		ID:           "plan-reconcile-" + name,
		Actor:        "issuer|admin",
		ActorDisplay: "Alice",
		Prepared: model.Prepared{
			Action:  model.Action{Kind: profileDeleteKind, Name: name, Reason: "pilotcleanup", ConfirmName: name},
			Changes: changes,
		},
	}
}

// reconcileChanges is the accepted evidence of a current Profile deletion.
func reconcileChanges() []model.Change {
	return []model.Change{
		{Field: "Profile", Before: "alice", After: "deleted"},
		{Field: profileUIDChangeField, Before: reconcileProfileUID, After: "deleted"},
		{Field: profileResourceVersionChangeField, Before: reconcileProfileRV, After: "deleted"},
		{Field: namespaceUIDChangeField, Before: reconcileNamespaceUID, After: "deleted"},
		{Field: clusterUIDChangeField, Before: reconcileClusterUID, After: "unchanged"},
		{Field: profileReconcileVersionChangeField, Before: profileReconcileVersion, After: "unchanged"},
	}
}

// reconcileOperation creates an accepted Profile deletion and, when
// checkpointed is true, records the durable pre-delete checkpoint so the
// operation proves that a delete request may have been issued.
func reconcileOperation(t *testing.T, s *Store, id string, changes []model.Change, checkpointed bool) model.Operation {
	t.Helper()
	ctx := context.Background()
	plan := reconcilePlan("alice", changes)
	now := time.Now().UTC()
	op := model.Operation{ID: id, PlanID: plan.ID, Actor: plan.Actor, ActorDisplay: plan.ActorDisplay, Target: PrimaryTarget(plan.Prepared.Action), Action: plan.Prepared.Action.Kind, State: "queued", Step: "queued", CreatedAt: now, UpdatedAt: now}
	if _, created, err := s.CreateOperation(ctx, op, plan, "reconcile-key-"+id); err != nil || !created {
		t.Fatalf("the Profile deletion was not accepted: %v", err)
	}
	if checkpointed {
		op.State = "applying"
		op.Step = "delete-profile"
		op.ProfileDeleteCheckpointed = true
		if err := s.Checkpoint(ctx, op, model.Audit{ID: id + "-checkpoint"}); err != nil {
			t.Fatalf("the pre-delete checkpoint was not durable: %v", err)
		}
	}
	op.State = "needs-review"
	if err := s.Finish(ctx, op, model.Audit{ID: id + "-review"}); err != nil {
		t.Fatalf("the uncertain outcome was not recorded: %v", err)
	}
	return op
}

// reconcileLocks counts the active locks the operation still holds.
func reconcileLocks(t *testing.T, s *Store, operationID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_targets WHERE operation_id=? AND active=1`, operationID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// reconcileAbsent is a complete observation in which both objects are gone.
func reconcileAbsent() ProfileDeleteObservation {
	return ProfileDeleteObservation{Complete: true, ClusterUID: reconcileClusterUID}
}

// reconcilePresent is a complete observation in which both objects are still
// there with their accepted identities.
func reconcilePresent() ProfileDeleteObservation {
	return ProfileDeleteObservation{Complete: true, ProfileUID: reconcileProfileUID, NamespaceUID: reconcileNamespaceUID, ClusterUID: reconcileClusterUID}
}

// A deletion that never issued a delete request is cancelled: the durable
// marker proves no write was ever started, the locks are released and the
// namespace becomes usable again.
func TestProfileDeleteReconcileCancelledWhenNoWriteWasEverStarted(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	op := reconcileOperation(t, s, "reconcile-cancel", reconcileChanges(), false)
	plan, err := s.PlanProfileDeleteReconciliation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.MarkerPresent || !plan.NoWriteProven {
		t.Fatalf("the durable no-write proof is missing: %+v", plan)
	}
	if plan.ProfileUID != reconcileProfileUID || plan.NamespaceUID != reconcileNamespaceUID || plan.ClusterUID != reconcileClusterUID {
		t.Fatalf("the accepted identities are wrong: %+v", plan)
	}
	resolved, err := s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{
		OperationID:  op.ID,
		Observations: []ProfileDeleteObservation{reconcilePresent()},
		Reason:       "operator verified that no delete request was ever issued",
		Operator:     "offline-operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != "failed" || resolved.Step != string(ProfileDeleteReconciledCancelled) {
		t.Fatalf("the derived outcome is wrong: %+v", resolved)
	}
	if locks := reconcileLocks(t, s, op.ID); locks != 0 {
		t.Fatalf("%d locks were retained after a reconciled cancellation", locks)
	}
	// The claim is released, so the namespace is usable again.
	mutation := reconcilePlan("alice", nil)
	mutation.Prepared.Action = model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "lab", Reason: "routine change"}
	mutation.ID = "plan-child-after-cancel"
	child := model.Operation{ID: "child-after-cancel", PlanID: mutation.ID, Actor: mutation.Actor, Target: PrimaryTarget(mutation.Prepared.Action), Action: mutation.Prepared.Action.Kind, State: "queued", Step: "queued"}
	if _, created, err := s.CreateOperation(ctx, child, mutation, "child-after-cancel-key"); err != nil || !created {
		t.Fatalf("the namespace stayed fenced after a reconciled cancellation: %v", err)
	}
	// The audit row is distinct and operator attributed, and the accepted
	// identity and reason are unchanged.
	audit, err := s.RecentAuditWithPolicy(ctx, HistoryPolicy{}.WithDefaults(), RecentHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range audit {
		if a.ID != op.ID+"-reconciled" {
			continue
		}
		found = true
		if a.Result != string(ProfileDeleteReconciledCancelled) {
			t.Fatalf("the audit result is %q", a.Result)
		}
		if a.Actor != "issuer|admin" || a.ActorDisplay != "Alice" {
			t.Fatalf("the accepted identity was changed: %+v", a)
		}
		if a.Reason != "pilotcleanup" {
			t.Fatalf("the accepted reason was changed: %q", a.Reason)
		}
		joined := ""
		for _, c := range a.Changes {
			joined += c.Field + "|" + c.Before + "|" + c.After + ";"
		}
		if !strings.Contains(joined, "offline-operator") || !strings.Contains(joined, "operator verified") {
			t.Fatalf("the audit does not record the operator and the reason: %v", a.Changes)
		}
		if !strings.Contains(joined, "issuer|admin") || !strings.Contains(joined, "pilotcleanup") {
			t.Fatalf("the audit lost the accepted evidence: %v", a.Changes)
		}
	}
	if !found {
		t.Fatal("the reconciliation wrote no audit row")
	}
}

// A completed deletion is reconciled as succeeded when two complete
// observations show that the Profile and its namespace are both gone.
func TestProfileDeleteReconcileSucceededWhenBothObjectsAreGone(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	op := reconcileOperation(t, s, "reconcile-success", reconcileChanges(), true)
	resolved, err := s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{
		OperationID:  op.ID,
		Observations: []ProfileDeleteObservation{reconcileAbsent(), reconcileAbsent()},
		Reason:       "operator verified that the Profile and its namespace are gone",
		Operator:     "offline-operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != "succeeded" || resolved.Step != string(ProfileDeleteReconciledSucceeded) {
		t.Fatalf("the derived outcome is wrong: %+v", resolved)
	}
	if locks := reconcileLocks(t, s, op.ID); locks != 0 {
		t.Fatalf("%d locks were retained after a reconciled success", locks)
	}
	// The accepted marker is preserved: a delete request was issued.
	var stored model.Operation
	if err := s.Get(ctx, "operations", op.ID, &stored); err != nil {
		t.Fatal(err)
	}
	if !stored.ProfileDeleteCheckpointed {
		t.Fatal("the reconciliation cleared the pre-delete checkpoint marker")
	}
}

// An operation whose delete request may have been issued is never cancelled,
// however the cluster currently looks. The operator has to review it and let
// the normal cleanup path finish.
func TestProfileDeleteReconcileRefusesToCancelAfterAMayHaveBeenIssuedDelete(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	op := reconcileOperation(t, s, "reconcile-ambiguous", reconcileChanges(), true)
	plan, err := s.PlanProfileDeleteReconciliation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.NoWriteProven {
		t.Fatal("a checkpointed deletion claims that no write was started")
	}
	if _, err = s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{
		OperationID:  op.ID,
		Observations: []ProfileDeleteObservation{reconcilePresent()},
		Reason:       "operator looked at the cluster",
		Operator:     "offline-operator",
	}); !errors.Is(err, ErrReconcileUnsupported) {
		t.Fatalf("a deletion that may have been issued was cancelled: %v", err)
	}
	if locks := reconcileLocks(t, s, op.ID); locks != 2 {
		t.Fatalf("%d locks were released by a refused reconciliation", locks)
	}
	var stored model.Operation
	if err := s.Get(ctx, "operations", op.ID, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.State != "needs-review" {
		t.Fatalf("a refused reconciliation changed the state to %q", stored.State)
	}
}

// Every observation that cannot be proven is refused: a replaced object, an
// observation from another cluster, an incomplete read, observations that
// disagree, and a single observation of an absent pair.
func TestProfileDeleteReconcileRefusesUnprovenObservations(t *testing.T) {
	replacedProfile := reconcileAbsent()
	replacedProfile.ProfileUID = "profile-uid-bob"
	replacedNamespace := reconcileAbsent()
	replacedNamespace.NamespaceUID = "namespace-uid-bob"
	wrongCluster := reconcileAbsent()
	wrongCluster.ClusterUID = "cluster-uid-other"
	incomplete := reconcileAbsent()
	incomplete.Complete = false
	present := reconcilePresent()
	cases := []struct {
		name         string
		observations []ProfileDeleteObservation
	}{
		{"replaced-profile", []ProfileDeleteObservation{replacedProfile, replacedProfile}},
		{"replaced-namespace", []ProfileDeleteObservation{replacedNamespace, replacedNamespace}},
		{"another-cluster", []ProfileDeleteObservation{wrongCluster, wrongCluster}},
		{"incomplete", []ProfileDeleteObservation{incomplete, incomplete}},
		{"disagreeing", []ProfileDeleteObservation{reconcileAbsent(), present}},
		{"single-observation", []ProfileDeleteObservation{reconcileAbsent()}},
		{"no-observation", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := database(t)
			ctx := context.Background()
			op := reconcileOperation(t, s, "reconcile-"+tc.name, reconcileChanges(), false)
			if _, err := s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{
				OperationID:  op.ID,
				Observations: tc.observations,
				Reason:       "operator looked at the cluster",
				Operator:     "offline-operator",
			}); !errors.Is(err, ErrReconcileUnsupported) {
				t.Fatalf("an unproven observation was accepted: %v", err)
			}
			if locks := reconcileLocks(t, s, op.ID); locks != 2 {
				t.Fatalf("%d locks were released by a refused reconciliation", locks)
			}
			var stored model.Operation
			if err := s.Get(ctx, "operations", op.ID, &stored); err != nil {
				t.Fatal(err)
			}
			if stored.State != "needs-review" {
				t.Fatalf("a refused reconciliation changed the state to %q", stored.State)
			}
		})
	}
}

// Accepted evidence that never carried the reviewed identities can never
// unlock anything.
func TestProfileDeleteReconcileRefusesIncompleteEvidence(t *testing.T) {
	noCluster := reconcileChanges()
	noCluster = append(noCluster[:4:4], noCluster[5:]...)
	noProfileUID := reconcileChanges()
	noProfileUID = append(noProfileUID[:1:1], noProfileUID[2:]...)
	cases := []struct {
		name    string
		changes []model.Change
	}{
		{"without-cluster-identity", noCluster},
		{"without-profile-identity", noProfileUID},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := database(t)
			ctx := context.Background()
			op := reconcileOperation(t, s, "reconcile-legacy", tc.changes, false)
			if _, err := s.PlanProfileDeleteReconciliation(ctx, op.ID); !errors.Is(err, ErrReconcileUnsupported) {
				t.Fatalf("incomplete evidence was accepted: %v", err)
			}
			if _, err := s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{
				OperationID:  op.ID,
				Observations: []ProfileDeleteObservation{reconcilePresent()},
				Reason:       "operator looked at the cluster",
				Operator:     "offline-operator",
			}); !errors.Is(err, ErrReconcileUnsupported) {
				t.Fatalf("incomplete evidence unlocked an operation: %v", err)
			}
			if locks := reconcileLocks(t, s, op.ID); locks != 2 {
				t.Fatalf("%d locks were released by a refused reconciliation", locks)
			}
		})
	}
}

// Accepted evidence that predates the reconciliation contract cannot prove
// that no write was ever started, so the cancellation is refused even though
// the accepted identities are complete.
func TestProfileDeleteReconcileRefusesCancellationWithoutTheContractVersion(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	noVersion := reconcileChanges()
	noVersion = append(noVersion[:5:5], noVersion[6:]...)
	op := reconcileOperation(t, s, "reconcile-no-version", noVersion, false)
	plan, err := s.PlanProfileDeleteReconciliation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.ReconciliationVersion != "" {
		t.Fatalf("the reconciliation version is %q", plan.ReconciliationVersion)
	}
	if plan.NoWriteProven {
		t.Fatal("evidence without the contract version proves that no write was started")
	}
	if _, err := s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{
		OperationID:  op.ID,
		Observations: []ProfileDeleteObservation{reconcilePresent()},
		Reason:       "operator looked at the cluster",
		Operator:     "offline-operator",
	}); !errors.Is(err, ErrReconcileUnsupported) {
		t.Fatalf("evidence without the contract version unlocked an operation: %v", err)
	}
	if locks := reconcileLocks(t, s, op.ID); locks != 2 {
		t.Fatalf("%d locks were released by a refused reconciliation", locks)
	}
}

// Only a needs-review Profile deletion that holds both of its locks can be
// reconciled. Nothing else is reachable.
func TestProfileDeleteReconcileRefusesEverythingElse(t *testing.T) {
	ctx := context.Background()
	t.Run("another-action", func(t *testing.T) {
		s := database(t)
		plan := reconcilePlan("alice", nil)
		plan.ID = "plan-notebook"
		plan.Prepared.Action = model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "lab", Reason: "routine change"}
		op := model.Operation{ID: "op-notebook", PlanID: plan.ID, Actor: plan.Actor, Target: PrimaryTarget(plan.Prepared.Action), Action: plan.Prepared.Action.Kind, State: "queued", Step: "queued"}
		if _, created, err := s.CreateOperation(ctx, op, plan, "reconcile-notebook-key"); err != nil || !created {
			t.Fatal(err)
		}
		op.State = "needs-review"
		if err := s.Finish(ctx, op, model.Audit{ID: "op-notebook-review"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{OperationID: op.ID, Observations: []ProfileDeleteObservation{reconcilePresent()}, Reason: "operator looked", Operator: "offline-operator"}); !errors.Is(err, ErrReconcileUnsupported) {
			t.Fatalf("a non Profile deletion was reconciled: %v", err)
		}
	})
	t.Run("missing-namespace-fence", func(t *testing.T) {
		s := database(t)
		op := reconcileOperation(t, s, "reconcile-unfenced", reconcileChanges(), false)
		if _, err := s.db.Exec(`UPDATE operation_targets SET active=0 WHERE operation_id=? AND target='namespace:alice'`, op.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{OperationID: op.ID, Observations: []ProfileDeleteObservation{reconcilePresent()}, Reason: "operator looked", Operator: "offline-operator"}); !errors.Is(err, ErrReconcileUnsupported) {
			t.Fatalf("an unfenced deletion was reconciled: %v", err)
		}
	})
	t.Run("already-terminal", func(t *testing.T) {
		s := database(t)
		op := reconcileOperation(t, s, "reconcile-terminal", reconcileChanges(), false)
		if _, err := s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{OperationID: op.ID, Observations: []ProfileDeleteObservation{reconcilePresent()}, Reason: "operator verified that no delete request was ever issued", Operator: "offline-operator"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{OperationID: op.ID, Observations: []ProfileDeleteObservation{reconcilePresent()}, Reason: "operator looked", Operator: "offline-operator"}); !errors.Is(err, ErrReconcileUnsupported) {
			t.Fatalf("an already resolved deletion was reconciled again: %v", err)
		}
	})
	t.Run("unknown-operation", func(t *testing.T) {
		s := database(t)
		if _, err := s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{OperationID: "no-such-operation", Observations: []ProfileDeleteObservation{reconcilePresent()}, Reason: "operator looked", Operator: "offline-operator"}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("an unknown operation was reconciled: %v", err)
		}
	})
}

// A normal Finish still cannot resolve an uncertain operation, and the
// reconciliation is the only path that can.
func TestProfileDeleteReconcileIsNotReachableThroughFinish(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	op := reconcileOperation(t, s, "reconcile-finish", reconcileChanges(), false)
	var stored model.Operation
	if err := s.Get(ctx, "operations", op.ID, &stored); err != nil {
		t.Fatal(err)
	}
	stored.State = "succeeded"
	stored.Step = "verified"
	if err := s.Finish(ctx, stored, model.Audit{ID: "reconcile-finish-manual"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a normal Finish resolved an uncertain operation: %v", err)
	}
	if locks := reconcileLocks(t, s, op.ID); locks != 2 {
		t.Fatalf("%d locks were released by a refused Finish", locks)
	}
}

// A reconciliation whose audit row cannot be written changes nothing: the
// whole decision is one transaction, so the fence is still held.
func TestProfileDeleteReconcileRollsBackWhenTheAuditCannotBeWritten(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	op := reconcileOperation(t, s, "reconcile-audit-fail", reconcileChanges(), false)
	// Occupy the reconciliation audit row so the insert cannot succeed.
	if err := s.Put(ctx, "audit", op.ID+"-reconciled", model.Audit{ID: op.ID + "-reconciled", Actor: "issuer|admin", Action: profileDeleteKind, Target: "profile:alice", Result: "occupied", Time: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{
		OperationID:  op.ID,
		Observations: []ProfileDeleteObservation{reconcilePresent()},
		Reason:       "operator verified that no delete request was ever issued",
		Operator:     "offline-operator",
	}); err == nil {
		t.Fatal("a reconciliation whose audit row could not be written succeeded")
	}
	if locks := reconcileLocks(t, s, op.ID); locks != 2 {
		t.Fatalf("%d locks were released by a rolled back reconciliation", locks)
	}
	var stored model.Operation
	if err := s.Get(ctx, "operations", op.ID, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.State != "needs-review" {
		t.Fatalf("a rolled back reconciliation changed the state to %q", stored.State)
	}
}

// The reviewed identities survive the bounded plan cleanup: the acceptance
// ledger, not the plan row, is the evidence.
func TestProfileDeleteReconcileSurvivesPlanCleanup(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	op := reconcileOperation(t, s, "reconcile-plan-cleanup", reconcileChanges(), false)
	if err := s.CleanupPlans(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var plan model.Plan
	if err := s.Get(ctx, "plans", "plan-reconcile-alice", &plan); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the plan row survived the cleanup: %v", err)
	}
	resolved, err := s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{
		OperationID:  op.ID,
		Observations: []ProfileDeleteObservation{reconcilePresent()},
		Reason:       "operator verified that no delete request was ever issued",
		Operator:     "offline-operator",
	})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != "failed" || resolved.Step != string(ProfileDeleteReconciledCancelled) {
		t.Fatalf("the derived outcome is wrong: %+v", resolved)
	}
}

// The pre-delete checkpoint marker is monotonic. Once a delete request may
// have been issued, no later update may clear that proof.
func TestProfileDeleteCheckpointMarkerIsMonotonic(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	plan := reconcilePlan("alice", reconcileChanges())
	now := time.Now().UTC()
	op := model.Operation{ID: "reconcile-monotonic", PlanID: plan.ID, Actor: plan.Actor, ActorDisplay: plan.ActorDisplay, Target: PrimaryTarget(plan.Prepared.Action), Action: plan.Prepared.Action.Kind, State: "queued", Step: "queued", CreatedAt: now, UpdatedAt: now}
	if _, created, err := s.CreateOperation(ctx, op, plan, "reconcile-monotonic-key"); err != nil || !created {
		t.Fatal(err)
	}
	op.State = "applying"
	op.Step = "delete-profile"
	op.ProfileDeleteCheckpointed = true
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "reconcile-monotonic-checkpoint"}); err != nil {
		t.Fatal(err)
	}
	var stored model.Operation
	if err := s.Get(ctx, "operations", op.ID, &stored); err != nil {
		t.Fatal(err)
	}
	if !stored.ProfileDeleteCheckpointed {
		t.Fatal("the marker was not recorded")
	}
	// Try to clear it with a later durable update.
	stored.ProfileDeleteCheckpointed = false
	stored.State = "applying"
	stored.Step = "delete-profile"
	if err := s.Checkpoint(ctx, stored, model.Audit{ID: "reconcile-monotonic-clear"}); err != nil {
		t.Fatal(err)
	}
	var again model.Operation
	if err := s.Get(ctx, "operations", op.ID, &again); err != nil {
		t.Fatal(err)
	}
	if !again.ProfileDeleteCheckpointed {
		t.Fatal("the marker was cleared by a later update")
	}
	// The operation then reaches an uncertain outcome and cannot be
	// cancelled, because a delete request may have been issued.
	again.State = "needs-review"
	if err := s.Finish(ctx, again, model.Audit{ID: "reconcile-monotonic-review"}); err != nil {
		t.Fatal(err)
	}
	plan2, err := s.PlanProfileDeleteReconciliation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan2.NoWriteProven {
		t.Fatal("a cleared marker still proves that no write was started")
	}
}

// The reconciliation reason is required, bounded like every other reason, and
// its contents never reach an error.
func TestProfileDeleteReconcileReasonIsRequiredAndBounded(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	op := reconcileOperation(t, s, "reconcile-reason", reconcileChanges(), false)
	multibyte := strings.Repeat("é", 500)
	secret := "REASON_SECRET_VALUE"
	cases := []struct {
		name     string
		reason   string
		operator string
		wantErr  bool
	}{
		{"empty-reason", "", "offline-operator", true},
		{"blank-reason", "   ", "offline-operator", true},
		{"empty-operator", multibyte, "", true},
		{"multibyte-500-accepted", multibyte, "offline-operator", false},
		{"multibyte-501-rejected", strings.Repeat("é", 501), "offline-operator", true},
		{"secret-not-echoed", secret, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{
				OperationID:  op.ID,
				Observations: []ProfileDeleteObservation{reconcilePresent()},
				Reason:       tc.reason,
				Operator:     tc.operator,
			})
			if tc.wantErr {
				if err == nil {
					t.Fatal("an unacceptable reconciliation reason was accepted")
				}
				if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), tc.reason) && tc.reason == secret {
					t.Fatalf("the reason was echoed into the error: %v", err)
				}
				if tc.reason == multibyte && strings.Contains(err.Error(), multibyte) {
					t.Fatal("the reason was echoed into the error")
				}
				return
			}
			if err != nil {
				t.Fatalf("a 500 character multibyte reason was rejected: %v", err)
			}
		})
	}
}

// The reconciliation runs entirely inside one transaction on the store's
// single connection. A statement that reached outside the transaction while
// it was open would block forever, so the whole decision is bounded by a
// context: the test fails with a deadline instead of hanging.
func TestProfileDeleteReconcileCompletesUnderABoundedContext(t *testing.T) {
	s := database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	op := reconcileOperation(t, s, "reconcile-bounded", reconcileChanges(), false)
	// The bounded plan read is bounded as well.
	plan, err := s.PlanProfileDeleteReconciliation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Operation.ID != op.ID {
		t.Fatalf("the plan is wrong: %+v", plan)
	}
	resolved, err := s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{
		OperationID:  op.ID,
		Observations: []ProfileDeleteObservation{reconcilePresent()},
		Reason:       "operator verified that no delete request was ever issued",
		Operator:     "offline-operator",
	})
	if err != nil {
		t.Fatalf("the reconciliation did not complete: %v", err)
	}
	if resolved.State != "failed" {
		t.Fatalf("the derived outcome is wrong: %+v", resolved)
	}
}

// The durable store keeps working after a reconciliation: the integrity check
// passes and the namespace is no longer fenced.
func TestProfileDeleteReconcileLeavesTheStoreConsistent(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	op := reconcileOperation(t, s, "reconcile-consistent", reconcileChanges(), false)
	if _, err := s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{
		OperationID:  op.ID,
		Observations: []ProfileDeleteObservation{reconcilePresent()},
		Reason:       "operator verified that no delete request was ever issued",
		Operator:     "offline-operator",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("the store is inconsistent after a reconciliation: %v", err)
	}
	if _, err := s.IncompleteOperations(ctx); err != nil {
		t.Fatal(err)
	}
	incomplete, err := s.IncompleteOperations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range incomplete {
		if o.ID == op.ID {
			t.Fatalf("the reconciled operation is still incomplete: %+v", o)
		}
	}
}

// The store is opened with the exclusive process lock, so a reconciliation
// cannot run while the application owns the database.
func TestProfileDeleteReconcileRefusesWhileTheDatabaseIsOwned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	owner, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err := OpenWithJournal(path, "wal", false); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second process opened the owned database: %v", err)
	}
}

// The reconciliation never reports a cluster identity it did not read.
func TestProfileDeleteReconcileRequiresTheAcceptedClusterIdentity(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	plan := reconcilePlan("alice", reconcileChanges())
	carries := false
	for _, c := range plan.Prepared.Changes {
		if c.Field == clusterUIDChangeField {
			carries = true
		}
	}
	if !carries {
		t.Fatalf("the fixture does not carry the cluster identity: %v", plan.Prepared.Changes)
	}
	op := reconcileOperation(t, s, "reconcile-cluster", reconcileChanges(), false)
	evidence, err := s.PlanProfileDeleteReconciliation(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.ClusterUID != reconcileClusterUID {
		t.Fatalf("the accepted cluster identity is %q", evidence.ClusterUID)
	}
	// An observation from another cluster is refused even when both objects
	// are absent.
	other := reconcileAbsent()
	other.ClusterUID = "cluster-uid-other"
	if _, err = s.ReconcileProfileDelete(ctx, ProfileDeleteReconciliation{
		OperationID:  op.ID,
		Observations: []ProfileDeleteObservation{other, other},
		Reason:       "operator looked at another cluster",
		Operator:     "offline-operator",
	}); !errors.Is(err, ErrReconcileUnsupported) {
		t.Fatalf("an observation from another cluster was accepted: %v", err)
	}
	if locks := reconcileLocks(t, s, op.ID); locks != 2 {
		t.Fatalf("%d locks were released by a refused reconciliation", locks)
	}
	_ = fmt.Sprint(plan.ID)
}
