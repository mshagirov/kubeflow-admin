package operations

// profile_delete_test.go is the operations-level regression for the
// destructive profile.delete workflow.
//
// It drives a REAL operations.Manager over a REAL store. The only fake is the
// Kubernetes adapter at the operations.Backend boundary.
//
// Scope, stated plainly: this adapter does not simulate the API server, the
// installed Profile controller or the Kubernetes garbage collector. These
// tests therefore do NOT prove that a Profile or its namespace was really
// removed, and they must never be read as such. That proof belongs to
// internal/kube/profile_delete_test.go, which owns the fake-cluster fixture
// and the delete/GC semantics. What these tests own is everything on this
// side of that boundary: the durable outcome classification, the namespace
// fence, idempotent retry and the audit evidence.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/store"
)

// profileDeleteReason is the required reason recorded with the deletion.
const profileDeleteReason = "pilotcleanup"

// profileDeleteAction is the exact destructive action under test: the Profile
// name alice, the typed confirmation alice and a recorded reason.
func profileDeleteAction() model.Action {
	return model.Action{Kind: "profile.delete", Name: "alice", Reason: profileDeleteReason, ConfirmName: "alice"}
}

// profileDeleteChanges is the safe preview the fake adapter returns. It
// carries only the narrow, reviewed facts a real preview carries.
func profileDeleteChanges() []model.Change {
	return []model.Change{
		{Field: "Profile", Before: "alice", After: "deleted with the namespace"},
		{Field: "observed Pods", Before: "1 observed", After: "deleted with the namespace"},
	}
}

// deleteAdapter is the fake Kubernetes adapter for the profile.delete
// workflow.
//
// It reproduces the failure-classification contract of
// kube.Service.executeProfileDelete: every failure raised before the single
// delete request is a definite pre-mutation rejection
// (model.ErrRejectedBeforeMutation), while a failure raised after that
// request is ambiguous and must be reviewed by an administrator. It never
// simulates the API server or garbage collection.
type deleteAdapter struct {
	mu sync.Mutex
	// executions counts every Execute call, of any action kind.
	executions int
	// profileExecutions counts the Execute calls for profile.delete.
	profileExecutions int
	// profileDeletes counts the delete requests issued for profile.delete.
	profileDeletes int
	// children counts the executions of any other action kind.
	children int

	// beforeDelete fails before the delete request is issued.
	beforeDelete error
	// afterDelete fails after the delete request was issued.
	afterDelete error
	// childError fails an execution of any other action kind after its
	// checkpoint, so the generic guidance can be exercised.
	childError error
	// revokeSession invalidates the administrator session so the next
	// durable checkpoint fails.
	revokeSession func()
}

func (a *deleteAdapter) prepare(_ context.Context, act model.Action) (model.Prepared, error) {
	if act.Kind == "profile.delete" {
		return model.Prepared{
			Action:          act,
			TargetUID:       "profile-uid-alice",
			ResourceVersion: "7",
			Summary:         "profile.delete alice",
			Warnings:        []string{"the deletion is irreversible"},
			Changes:         profileDeleteChanges(),
		}, nil
	}
	return model.Prepared{Action: act, TargetUID: "uid", Summary: "safe diff"}, nil
}

func (a *deleteAdapter) execute(_ context.Context, p model.Prepared, c func(string) error) error {
	a.mu.Lock()
	a.executions++
	a.mu.Unlock()
	if p.Action.Kind != "profile.delete" {
		a.mu.Lock()
		a.children++
		a.mu.Unlock()
		if err := c("before-write"); err != nil {
			return err
		}
		if a.childError != nil {
			return a.childError
		}
		return nil
	}
	a.mu.Lock()
	a.profileExecutions++
	a.mu.Unlock()
	// Everything that fails before the delete request provably changed
	// nothing, so it may release the operation lock.
	if a.beforeDelete != nil {
		return errors.Join(model.ErrRejectedBeforeMutation, a.beforeDelete)
	}
	if a.revokeSession != nil {
		a.revokeSession()
	}
	// The durable checkpoint precedes the delete request. A failure here
	// means no delete was ever issued.
	if err := c("delete-profile"); err != nil {
		return errors.Join(model.ErrRejectedBeforeMutation, err)
	}
	// Exactly one delete request.
	a.mu.Lock()
	a.profileDeletes++
	a.mu.Unlock()
	if a.afterDelete != nil {
		// The request was issued and the outcome is unknown.
		return a.afterDelete
	}
	return nil
}

// deleteCounts is a snapshot of what the fake Kubernetes adapter was asked to
// do.
type deleteCounts struct {
	// executions is every Execute call, of any action kind.
	executions int
	// profileExecutions is the Execute calls for profile.delete.
	profileExecutions int
	// profileDeletes is the delete requests actually issued.
	profileDeletes int
	// children is the Execute calls for any other action kind.
	children int
}

func (a *deleteAdapter) counts() deleteCounts {
	a.mu.Lock()
	defer a.mu.Unlock()
	return deleteCounts{a.executions, a.profileExecutions, a.profileDeletes, a.children}
}

// planProfileDelete previews the destructive deletion through the real
// manager and returns the accepted plan.
func planProfileDelete(t *testing.T, m *Manager, actor model.Identity) model.Plan {
	t.Helper()
	p, err := m.Plan(context.Background(), actor, profileDeleteAction())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// childMutation submits an ordinary namespaced mutation in the Profile's own
// namespace. While the Profile deletion is unresolved that namespace is
// fenced, so the submission must be refused; once the deletion reached a
// definite outcome the fence is released and the submission is accepted.
func childMutation(t *testing.T, m *Manager, actor model.Identity, key string) (model.Operation, error) {
	t.Helper()
	p, err := m.Plan(context.Background(), actor, model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "lab", Reason: "routine change"})
	if err != nil {
		t.Fatal(err)
	}
	return m.Submit(context.Background(), actor, p.ID, key)
}

// An ambiguous outcome after the delete request was issued is never a success
// and never a plain failure: it needs an explicit review, and the Profile and
// namespace locks stay held so no child mutation can enter the namespace
// while the outcome is unknown.
func TestProfileDeleteAmbiguousOutcomeNeedsReviewAndKeepsTheNamespaceFenced(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"convergence-timeout", fmt.Errorf("observed convergence not established: %w", context.DeadlineExceeded), "Operation timed out."},
		{"ambiguous-transport", errors.New("ADMISSION_ENV_VALUE=totally-private-value the Profile deletion outcome is unknown"), "Operation could not be completed safely."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			a := &deleteAdapter{afterDelete: tc.err}
			b := &fakeBackend{prepare: a.prepare, execute: a.execute}
			m, s, actor := setup(t, b)
			plan := planProfileDelete(t, m, actor)
			key := "profile-delete-idempotency-key"
			op, err := m.Submit(ctx, actor, plan.ID, key)
			if err != nil {
				t.Fatal(err)
			}
			got := waitOperation(t, m, op.ID)
			if got.State != "needs-review" {
				t.Fatalf("state = %q, want needs-review (%+v)", got.State, got)
			}
			// The durable checkpoint preceded the delete request.
			if got.Step != "delete-profile" {
				t.Fatalf("step = %q, want delete-profile", got.Step)
			}
			if !strings.Contains(got.Error, tc.want) {
				t.Fatalf("error = %q, want it to contain %q", got.Error, tc.want)
			}
			// The backend's raw text never reaches the durable record.
			for _, secret := range []string{"ADMISSION", "private", "totally-private-value"} {
				if strings.Contains(got.Error, secret) {
					t.Fatalf("raw backend text %q leaked into the outcome: %q", secret, got.Error)
				}
			}
			// The review guidance is specific to the Profile deletion: it
			// points the operator at the Profile and the namespace it owns,
			// and it keeps the lock and replay warnings.
			for _, want := range []string{
				"Profile",
				"namespace",
				"finalizer",
				"PersistentVolumeClaim",
				"deleted or retained",
				"may already have been accepted",
				"Do not retry",
				"do not force",
				"remain locked",
				"explicit operator reconciliation",
				"No rollback or automatic replay",
			} {
				if !strings.Contains(got.Error, want) {
					t.Fatalf("review guidance %q does not mention %q", got.Error, want)
				}
			}
			// The generic notebook and storage wording is not used for a
			// Profile deletion.
			for _, unwanted := range []string{"notebook may be stopped", "storage partially provisioned"} {
				if strings.Contains(got.Error, unwanted) {
					t.Fatalf("the generic guidance %q leaked into the Profile deletion review", unwanted)
				}
			}
			// The durable checkpoint preceded the delete request: the
			// operation was checkpointed once before the backend ran and
			// again before the delete request was issued.
			audit, err := m.Audit(ctx)
			if err != nil {
				t.Fatal(err)
			}
			checkpoints := 0
			for _, row := range audit {
				if row.OperationID == op.ID && row.Result == "applying" {
					checkpoints++
				}
			}
			if checkpoints < 2 {
				t.Fatalf("durable checkpoint rows = %d, want the pre-write and the pre-delete checkpoint", checkpoints)
			}
			// The delete was requested exactly once and never replayed.
			counts := a.counts()
			if counts.profileExecutions != 1 || counts.profileDeletes != 1 {
				t.Fatalf("executions = %d, delete requests = %d; want exactly one of each", counts.profileExecutions, counts.profileDeletes)
			}
			// The durable lock set is the Profile and the namespace the
			// cascade reaches.
			targets := store.Targets(plan)
			if len(targets) != 2 || targets[0] != "profile:alice" || targets[1] != "namespace:alice" {
				t.Fatalf("lock targets = %v, want the Profile and its namespace", targets)
			}
			// The retained fence is intact, so the store still agrees.
			if err := s.CheckIntegrity(ctx); err != nil {
				t.Fatalf("the retained fence failed the integrity check: %v", err)
			}
			// A later child mutation in the fenced namespace is refused.
			if _, err := childMutation(t, m, actor, "child-mutation-key-fenced"); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("a child mutation in the fenced namespace was accepted: %v", err)
			}
			// A second deletion of the same Profile is refused too.
			second := planProfileDelete(t, m, actor)
			if _, err := m.Submit(ctx, actor, second.ID, "second-profile-delete-key"); !errors.Is(err, store.ErrConflict) {
				t.Fatalf("a second deletion of the same Profile was accepted: %v", err)
			}
			// The idempotent retry resolves to the same operation and never
			// executes the deletion again.
			again, err := m.Submit(ctx, actor, plan.ID, key)
			if err != nil || again.ID != op.ID {
				t.Fatalf("idempotent retry = %v %v, want the same operation", again, err)
			}
			if again.State != "needs-review" {
				t.Fatalf("retry state = %q, want needs-review", again.State)
			}
			if counts := a.counts(); counts.profileExecutions != 1 || counts.profileDeletes != 1 {
				t.Fatalf("the retry executed the deletion again: executions = %d, delete requests = %d", counts.profileExecutions, counts.profileDeletes)
			}
		})
	}
}

// A definite API rejection before any change is applied is a failed
// operation, not an uncertain one, so the namespace fence is released and a
// corrected preview can proceed.
func TestProfileDeleteRejectedBeforeMutationFailsAndReleasesTheNamespaceFence(t *testing.T) {
	ctx := context.Background()
	a := &deleteAdapter{beforeDelete: errors.New("Kubernetes API denied the Profile deletion")}
	b := &fakeBackend{prepare: a.prepare, execute: a.execute}
	m, _, actor := setup(t, b)
	plan := planProfileDelete(t, m, actor)
	key := "profile-delete-rejected-key"
	op, err := m.Submit(ctx, actor, plan.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, m, op.ID)
	if got.State != "failed" {
		t.Fatalf("state = %q, want failed (%+v)", got.State, got)
	}
	if got.Step != "rejected" {
		t.Fatalf("step = %q, want rejected", got.Step)
	}
	// No delete request was ever issued.
	if counts := a.counts(); counts.profileDeletes != 0 {
		t.Fatalf("%d delete requests were issued after a definite rejection", counts.profileDeletes)
	}
	// The outcome is the authored explanation, never the backend's raw text.
	if strings.Contains(got.Error, "denied") {
		t.Fatalf("raw backend text leaked into the outcome: %q", got.Error)
	}
	if !strings.Contains(got.Error, "before any change was applied") {
		t.Fatalf("outcome = %q", got.Error)
	}
	// The fence is released: a later child mutation is accepted.
	child, err := childMutation(t, m, actor, "child-mutation-key-after-rejection")
	if err != nil {
		t.Fatalf("a child mutation was refused after a definite rejection: %v", err)
	}
	waitOperation(t, m, child.ID)
	// The idempotent retry resolves to the same failed operation and never
	// executes the deletion again.
	again, err := m.Submit(ctx, actor, plan.ID, key)
	if err != nil || again.ID != op.ID || again.State != "failed" {
		t.Fatalf("idempotent retry = %v %v, want the same failed operation", again, err)
	}
	if counts := a.counts(); counts.profileExecutions != 1 || counts.profileDeletes != 0 {
		t.Fatalf("the retry executed again: executions = %d, delete requests = %d", counts.profileExecutions, counts.profileDeletes)
	}
}

// The ordinary outcome: the delete was requested once and the workflow
// reported success, so the namespace fence is released.
func TestProfileDeleteSuccessReleasesTheNamespaceFence(t *testing.T) {
	ctx := context.Background()
	a := &deleteAdapter{}
	b := &fakeBackend{prepare: a.prepare, execute: a.execute}
	m, _, actor := setup(t, b)
	plan := planProfileDelete(t, m, actor)
	key := "profile-delete-success-key"
	op, err := m.Submit(ctx, actor, plan.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, m, op.ID)
	if got.State != "succeeded" {
		t.Fatalf("state = %q, want succeeded (%+v)", got.State, got)
	}
	if got.Step != "verified" {
		t.Fatalf("step = %q, want verified", got.Step)
	}
	if counts := a.counts(); counts.profileDeletes != 1 {
		t.Fatalf("delete requests = %d, want exactly one", counts.profileDeletes)
	}
	// The fence is released: a later child mutation is accepted.
	child, err := childMutation(t, m, actor, "child-mutation-key-after-success")
	if err != nil {
		t.Fatalf("a child mutation was refused after a successful deletion: %v", err)
	}
	waitOperation(t, m, child.ID)
}

// An administrator session that is revoked before the write checkpoint
// cancels the operation before any delete request is issued, and the locks
// are released because nothing was mutated.
func TestProfileDeleteSessionRevokedBeforeTheWriteCheckpointIssuesNoDelete(t *testing.T) {
	ctx := context.Background()
	a := &deleteAdapter{}
	b := &fakeBackend{prepare: a.prepare, execute: a.execute}
	m, s, actor := setup(t, b)
	a.revokeSession = func() {
		if err := s.Delete(ctx, "auth.sessions", actor.SessionID); err != nil {
			t.Errorf("revoke the administrator session: %v", err)
		}
	}
	key := "profile-delete-revoked-key"
	op, err := m.Submit(ctx, actor, planProfileDelete(t, m, actor).ID, key)
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, m, op.ID)
	if got.State != "failed" {
		t.Fatalf("state = %q, want failed (%+v)", got.State, got)
	}
	if got.Step != "rejected" {
		t.Fatalf("step = %q, want rejected", got.Step)
	}
	if counts := a.counts(); counts.profileDeletes != 0 {
		t.Fatalf("%d delete requests were issued after the checkpoint failed", counts.profileDeletes)
	}
	// A fresh administrator session may work in the namespace again: nothing
	// was deleted, so the fence was released.
	if err := s.Put(ctx, "auth.sessions", actor.SessionID, map[string]any{"identity": actor}); err != nil {
		t.Fatal(err)
	}
	child, err := childMutation(t, m, actor, "child-mutation-key-after-revocation")
	if err != nil {
		t.Fatalf("a child mutation was refused after a cancelled deletion: %v", err)
	}
	waitOperation(t, m, child.ID)
}

// The uncertain-outcome guidance is action specific for the destructive
// Profile deletion, and byte for byte unchanged for every other action kind.
func TestProfileDeleteReviewGuidanceIsActionSpecific(t *testing.T) {
	ctx := context.Background()

	t.Run("profile-delete", func(t *testing.T) {
		a := &deleteAdapter{afterDelete: errors.New("ADMISSION_ENV_VALUE=totally-private-value the Profile deletion outcome is unknown")}
		b := &fakeBackend{prepare: a.prepare, execute: a.execute}
		m, _, actor := setup(t, b)
		op, err := m.Submit(ctx, actor, planProfileDelete(t, m, actor).ID, "profile-delete-guidance-key")
		if err != nil {
			t.Fatal(err)
		}
		got := waitOperation(t, m, op.ID)
		if got.State != "needs-review" {
			t.Fatalf("state = %q, want needs-review", got.State)
		}
		// The sanitized prefix is kept: only the guidance is action specific.
		if !strings.HasPrefix(got.Error, "Operation could not be completed safely. ") {
			t.Fatalf("review guidance %q does not keep the sanitized prefix", got.Error)
		}
		for _, want := range []string{
			"Profile",
			"namespace",
			"finalizer",
			"PersistentVolumeClaim",
			"deleted or retained",
			"may already have been accepted",
			"Do not retry",
			"do not force",
			"remain locked",
			"explicit operator reconciliation",
			"No rollback or automatic replay",
		} {
			if !strings.Contains(got.Error, want) {
				t.Fatalf("review guidance %q does not mention %q", got.Error, want)
			}
		}
		for _, unwanted := range []string{"notebook may be stopped", "storage partially provisioned", "ADMISSION", "private"} {
			if strings.Contains(got.Error, unwanted) {
				t.Fatalf("the review guidance contains %q: %q", unwanted, got.Error)
			}
		}
	})

	t.Run("other-action-keeps-the-generic-guidance", func(t *testing.T) {
		a := &deleteAdapter{childError: errors.New("the outcome is unknown")}
		b := &fakeBackend{prepare: a.prepare, execute: a.execute}
		m, _, actor := setup(t, b)
		p, err := m.Plan(ctx, actor, model.Action{Kind: "notebook.stop", Namespace: "bob", Name: "lab", Reason: "routine change"})
		if err != nil {
			t.Fatal(err)
		}
		op, err := m.Submit(ctx, actor, p.ID, "notebook-stop-guidance-key")
		if err != nil {
			t.Fatal(err)
		}
		got := waitOperation(t, m, op.ID)
		if got.State != "needs-review" {
			t.Fatalf("state = %q, want needs-review", got.State)
		}
		want := "Operation could not be completed safely. Inspect actual cluster state; a notebook may be stopped or storage partially provisioned. No rollback or automatic replay was performed. Targets remain locked pending explicit reconciliation."
		if got.Error != want {
			t.Fatalf("the generic guidance changed:\n got %q\nwant %q", got.Error, want)
		}
	})
}

// The accepted identity, the required reason and the safe preview changes
// survive into the durable audit evidence, and no raw credential material is
// stored.
func TestProfileDeletePreservesActorReasonAndSafeAuditEvidence(t *testing.T) {
	ctx := context.Background()
	a := &deleteAdapter{}
	b := &fakeBackend{prepare: a.prepare, execute: a.execute}
	m, _, actor := setup(t, b)
	plan := planProfileDelete(t, m, actor)
	op, err := m.Submit(ctx, actor, plan.ID, "profile-delete-audit-key")
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, m, op.ID)
	if got.State != "succeeded" {
		t.Fatalf("state = %q, want succeeded (%+v)", got.State, got)
	}
	// The operation keeps the accepted identity and the canonical target.
	if got.Actor != actor.Key() {
		t.Fatalf("actor = %q, want %q", got.Actor, actor.Key())
	}
	if got.ActorDisplay != actor.DisplayName() {
		t.Fatalf("actor display = %q, want %q", got.ActorDisplay, actor.DisplayName())
	}
	if got.Action != "profile.delete" || got.Target != "profile:alice" {
		t.Fatalf("action/target = %q/%q", got.Action, got.Target)
	}
	audit, err := m.Audit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var accepted, outcome *model.Audit
	for i := range audit {
		if audit[i].OperationID != op.ID {
			continue
		}
		if audit[i].Result == "queued" {
			accepted = &audit[i]
		}
		if audit[i].Result == got.State {
			outcome = &audit[i]
		}
	}
	if accepted == nil {
		t.Fatal("the accepted operation has no accepted audit row")
	}
	if accepted.Reason != profileDeleteReason {
		t.Fatalf("accepted audit reason = %q, want %q", accepted.Reason, profileDeleteReason)
	}
	want := profileDeleteChanges()
	if len(accepted.Changes) != len(want) {
		t.Fatalf("accepted audit changes = %v, want %v", accepted.Changes, want)
	}
	for i := range want {
		if accepted.Changes[i] != want[i] {
			t.Fatalf("accepted audit changes = %v, want %v", accepted.Changes, want)
		}
	}
	if outcome == nil {
		t.Fatal("the accepted operation has no outcome audit row")
	}
	if outcome.Reason != profileDeleteReason {
		t.Fatalf("outcome audit reason = %q, want %q", outcome.Reason, profileDeleteReason)
	}
	// The session credential never reaches the audit or the operation.
	encoded, err := json.Marshal(audit)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), actor.SessionID) {
		t.Fatal("the audit leaked the session credential")
	}
	encodedOp, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedOp), actor.SessionID) {
		t.Fatal("the operation leaked the session credential")
	}
}
