package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kubeflow-admin/internal/model"
)

// profileDeletePlan builds the server-owned plan of a destructive Profile
// deletion: the exact Profile name, the exact typed confirmation, the
// Profile's own namespace left empty and a recorded reason. The Profile name
// is also the name of the namespace the Profile controller created, so the
// deletion cascades into it.
func profileDeletePlan(name string) model.Plan {
	return model.Plan{
		ID:           "plan-delete-" + name,
		Actor:        "issuer|admin",
		ActorDisplay: "Alice",
		Prepared: model.Prepared{
			Action: model.Action{
				Kind:        profileDeleteKind,
				Name:        name,
				ConfirmName: name,
				Reason:      "approved cleanup of a disposable pilot profile",
			},
			TargetUID: "profile-uid-" + name,
			Summary:   "Delete the Profile and the namespace it owns",
		},
	}
}

// profileDeleteOp is the queued operation of an accepted Profile deletion.
func profileDeleteOp(plan model.Plan, id string) model.Operation {
	now := time.Now().UTC()
	return model.Operation{
		ID:        id,
		PlanID:    plan.ID,
		Actor:     plan.Actor,
		Target:    PrimaryTarget(plan.Prepared.Action),
		Action:    plan.Prepared.Action.Kind,
		State:     "queued",
		Step:      "queued",
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// namespacedMutation builds an ordinary namespaced mutation plan.
func namespacedMutation(kind, ns, name string) model.Plan {
	plan := model.Plan{
		ID:           "plan-" + kind + "-" + ns + "-" + name,
		Actor:        "issuer|admin",
		ActorDisplay: "Alice",
		Prepared: model.Prepared{
			Action: model.Action{Kind: kind, Namespace: ns, Name: name, Reason: "routine change"},
		},
	}
	if kind == "pod.restart" {
		plan.Prepared.ParentNotebook = "lab"
	}
	return plan
}

// profileMutation builds an ordinary Profile mutation plan. A Profile action
// addresses its Profile by name, because the Profile name is also the
// namespace the Profile owns.
func profileMutation(kind, name string) model.Plan {
	return model.Plan{
		ID:           "plan-" + kind + "-" + name,
		Actor:        "issuer|admin",
		ActorDisplay: "Alice",
		Prepared: model.Prepared{
			Action: model.Action{Kind: kind, Name: name, Reason: "routine change"},
		},
	}
}

// queuedMutationOp is the queued operation of an ordinary mutation plan.
func queuedMutationOp(plan model.Plan, id string) model.Operation {
	now := time.Now().UTC()
	return model.Operation{
		ID:        id,
		PlanID:    plan.ID,
		Actor:     plan.Actor,
		Target:    PrimaryTarget(plan.Prepared.Action),
		Action:    plan.Prepared.Action.Kind,
		State:     "queued",
		Step:      "queued",
		CreatedAt: now,
		UpdatedAt: now,
	}
}

// countLocks reports the active locks an operation holds.
func countLocks(t *testing.T, s *Store, operationID string) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_targets WHERE operation_id=? AND active=1`, operationID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The Profile target stays canonical and the namespace the cascade reaches is
// an additional lock. Irrelevant fields never add a lock target.
func TestProfileDeleteTargetsFenceTheNamespace(t *testing.T) {
	plan := profileDeletePlan("alice")
	if got := PrimaryTarget(plan.Prepared.Action); got != "profile:alice" {
		t.Fatalf("profile.delete primary target = %q", got)
	}
	// Every Profile action keeps the profile:<name> primary target.
	for _, kind := range []string{"profile.create", "profile.update", "profile.quota.change", "profile.delete"} {
		action := model.Action{Kind: kind, Name: "alice"}
		if got := PrimaryTarget(action); got != "profile:alice" {
			t.Fatalf("%s primary target = %q", kind, got)
		}
	}
	targets := Targets(plan)
	if len(targets) != 2 || targets[0] != "profile:alice" || targets[1] != "namespace:alice" {
		t.Fatalf("profile.delete targets = %v", targets)
	}
	// A reserved PV name and a parent notebook are irrelevant to a Profile
	// deletion: they must not create a PV or Notebook lock.
	plan.Prepared.Action.PVName = "pv-a"
	plan.Prepared.ParentNotebook = "lab"
	if targets := Targets(plan); len(targets) != 2 {
		t.Fatalf("profile.delete targets with irrelevant fields = %v", targets)
	}
	// An ordinary Profile action keeps only its Profile target: it does not
	// fence the namespace it lives in.
	if targets := Targets(profileMutation("profile.update", "alice")); len(targets) != 1 || targets[0] != "profile:alice" {
		t.Fatalf("profile.update targets = %v", targets)
	}

	s := database(t)
	ctx := context.Background()
	op := profileDeleteOp(plan, "delete-op")
	if _, created, err := s.CreateOperation(ctx, op, plan, "profile-delete-target-key"); err != nil || !created {
		t.Fatal(err)
	}
	if locks := countLocks(t, s, op.ID); locks != 2 {
		t.Fatalf("profile deletion holds %d locks, want the Profile and the namespace", locks)
	}
	var fence string
	if err := s.db.QueryRow(`SELECT target FROM operation_targets WHERE operation_id=? AND active=1 AND target LIKE 'namespace:%'`, op.ID).Scan(&fence); err != nil {
		t.Fatal(err)
	}
	if fence != "namespace:alice" {
		t.Fatalf("namespace fence target = %q", fence)
	}
	// No PV or Notebook lock was derived from the irrelevant fields.
	var irrelevant int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_targets WHERE operation_id=? AND (target LIKE 'pv:%' OR target LIKE 'notebook:%')`, op.ID).Scan(&irrelevant); err != nil {
		t.Fatal(err)
	}
	if irrelevant != 0 {
		t.Fatalf("irrelevant fields created %d extra locks", irrelevant)
	}
}

// A Profile deletion must name the Profile exactly, confirm the exact name,
// stay inside the Profile's own namespace and carry a reason. A malformed
// deletion creates no claim, no lock and no operation.
func TestProfileDeleteAdmissionRequiresExactTarget(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	cases := []struct {
		name string
		edit func(*model.Action)
	}{
		{"empty-name", func(a *model.Action) { a.Name, a.ConfirmName = "", "" }},
		{"uppercase-name", func(a *model.Action) { a.Name, a.ConfirmName = "Alice", "Alice" }},
		{"path-name", func(a *model.Action) { a.Name, a.ConfirmName = "alice/team", "alice/team" }},
		{"wildcard-name", func(a *model.Action) { a.Name, a.ConfirmName = "*", "*" }},
		{"dotted-name", func(a *model.Action) { a.Name, a.ConfirmName = "alice.team", "alice.team" }},
		{"long-name", func(a *model.Action) { a.Name, a.ConfirmName = strings.Repeat("a", 64), strings.Repeat("a", 64) }},
		{"missing-confirmation", func(a *model.Action) { a.ConfirmName = "" }},
		{"mismatched-confirmation", func(a *model.Action) { a.ConfirmName = "alice2" }},
		{"case-mismatched-confirmation", func(a *model.Action) { a.ConfirmName = "Alice" }},
		{"foreign-namespace", func(a *model.Action) { a.Namespace = "bob" }},
		{"missing-reason", func(a *model.Action) { a.Reason = "" }},
		{"blank-reason", func(a *model.Action) { a.Reason = "   " }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan := profileDeletePlan("alice")
			tc.edit(&plan.Prepared.Action)
			plan.ID = "plan-" + tc.name
			op := profileDeleteOp(plan, "op-"+tc.name)
			if _, _, err := s.CreateOperation(ctx, op, plan, "delete-key-"+tc.name); !errors.Is(err, ErrInvalidTarget) {
				t.Fatalf("malformed Profile deletion accepted: %v", err)
			}
			var claims, locks, operations int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_claims`).Scan(&claims); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_targets`).Scan(&locks); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM records WHERE bucket='operations'`).Scan(&operations); err != nil {
				t.Fatal(err)
			}
			if claims != 0 || locks != 0 || operations != 0 {
				t.Fatalf("malformed deletion left state behind: claims=%d locks=%d operations=%d", claims, locks, operations)
			}
		})
	}
}

// The exact deletion is accepted. The Profile's own namespace may be stated
// explicitly as long as it is the Profile name.
func TestProfileDeleteAdmissionAcceptsExactTarget(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	plan := profileDeletePlan("alice")
	plan.Prepared.Action.Namespace = "alice"
	op := profileDeleteOp(plan, "delete-op")
	if _, created, err := s.CreateOperation(ctx, op, plan, "profile-delete-accept-key"); err != nil || !created {
		t.Fatal(err)
	}
	if locks := countLocks(t, s, op.ID); locks != 2 {
		t.Fatalf("accepted deletion holds %d locks, want 2", locks)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatal("accepted deletion failed the integrity check", err)
	}
}

// A Profile deletion is refused while any other mutation of the same
// namespace is still active.
func TestProfileDeleteRefusedWhileNamespaceMutationIsActive(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	cases := []struct {
		kind string
		plan func(ns string) model.Plan
	}{
		{"notebook.stop", func(ns string) model.Plan { return namespacedMutation("notebook.stop", ns, "lab") }},
		{"notebook.edit", func(ns string) model.Plan { return namespacedMutation("notebook.edit", ns, "lab") }},
		{"pvc.create.dynamic", func(ns string) model.Plan { return namespacedMutation("pvc.create.dynamic", ns, "data") }},
		{"pvc.expand", func(ns string) model.Plan { return namespacedMutation("pvc.expand", ns, "data") }},
		{"pod.restart", func(ns string) model.Plan { return namespacedMutation("pod.restart", ns, "lab-0") }},
		{"profile.update", func(ns string) model.Plan { return profileMutation("profile.update", ns) }},
		{"profile.quota.change", func(ns string) model.Plan { return profileMutation("profile.quota.change", ns) }},
	}
	for i, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			// Each case owns its namespace, so one database can host them all.
			ns := fmt.Sprintf("team-%d", i)
			mutation := tc.plan(ns)
			op := queuedMutationOp(mutation, "mutation-op-"+ns)
			if _, created, err := s.CreateOperation(ctx, op, mutation, "mutation-key-"+ns); err != nil || !created {
				t.Fatal(err)
			}
			del := profileDeletePlan(ns)
			delOp := profileDeleteOp(del, "delete-op-"+ns)
			if _, _, err := s.CreateOperation(ctx, delOp, del, "delete-key-"+ns); !errors.Is(err, ErrConflict) {
				t.Fatalf("Profile deletion accepted while %s was active: %v", tc.kind, err)
			}
			// The refused deletion left nothing behind.
			if locks := countLocks(t, s, delOp.ID); locks != 0 {
				t.Fatalf("refused deletion left %d locks", locks)
			}
		})
	}
}

// An accepted Profile deletion fences its whole namespace: no other mutation
// may start there while the deletion is unresolved. A different namespace and
// a cluster-scoped action are unaffected.
func TestProfileDeletionFencesEveryNamespaceMutation(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	del := profileDeletePlan("alice")
	delOp := profileDeleteOp(del, "delete-op")
	if _, created, err := s.CreateOperation(ctx, delOp, del, "profile-delete-fence-key"); err != nil || !created {
		t.Fatal(err)
	}
	blocked := []model.Plan{
		namespacedMutation("notebook.stop", "alice", "lab"),
		namespacedMutation("notebook.edit", "alice", "lab"),
		namespacedMutation("pvc.create.dynamic", "alice", "data"),
		namespacedMutation("pvc.expand", "alice", "data"),
		namespacedMutation("storage.create.static", "alice", "data"),
		namespacedMutation("pod.restart", "alice", "lab-0"),
		profileMutation("profile.update", "alice"),
	}
	for _, mutation := range blocked {
		op := queuedMutationOp(mutation, "mutation-op-"+mutation.ID)
		if _, _, err := s.CreateOperation(ctx, op, mutation, "mutation-key-"+mutation.ID); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s accepted while a Profile deletion was active: %v", mutation.Prepared.Action.Kind, err)
		}
	}
	// Another namespace is not fenced.
	other := namespacedMutation("notebook.stop", "bob", "lab")
	if _, created, err := s.CreateOperation(ctx, queuedMutationOp(other, "other-op"), other, "other-namespace-key"); err != nil || !created {
		t.Fatalf("mutation in an unrelated namespace refused: %v", err)
	}
	// A cluster-scoped action is not fenced.
	node := model.Plan{ID: "plan-node", Actor: "issuer|admin", Prepared: model.Prepared{Action: model.Action{Kind: "node.cordon", Name: "worker-1", Reason: "maintenance"}}}
	if _, created, err := s.CreateOperation(ctx, queuedMutationOp(node, "node-op"), node, "node-cordon-key"); err != nil || !created {
		t.Fatalf("cluster-scoped mutation refused: %v", err)
	}
}

// An idempotent retry resolves to the accepted operation before any new
// conflict is considered, even while the namespace stays fenced.
func TestProfileDeleteIdempotentRetryResolvesBeforeTheFence(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	del := profileDeletePlan("alice")
	op := profileDeleteOp(del, "delete-op")
	if _, created, err := s.CreateOperation(ctx, op, del, "profile-delete-retry-key"); err != nil || !created {
		t.Fatal(err)
	}
	mutation := namespacedMutation("notebook.stop", "alice", "lab")
	if _, _, err := s.CreateOperation(ctx, queuedMutationOp(mutation, "mutation-op"), mutation, "mutation-retry-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("mutation accepted while a Profile deletion was active: %v", err)
	}
	prior, created, err := s.CreateOperation(ctx, op, del, "profile-delete-retry-key")
	if err != nil || created || prior.ID != op.ID {
		t.Fatalf("idempotent retry did not resolve to the accepted operation: %v %v %v", prior, created, err)
	}
	if locks := countLocks(t, s, op.ID); locks != 2 {
		t.Fatalf("retry changed the accepted locks: %d", locks)
	}
}

// An uncertain outcome keeps the namespace fence: an administrator must
// reconcile it explicitly before the namespace is used again.
func TestProfileDeleteNeedsReviewRetainsTheNamespaceFence(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	del := profileDeletePlan("alice")
	op := profileDeleteOp(del, "delete-op")
	if _, created, err := s.CreateOperation(ctx, op, del, "profile-delete-review-key"); err != nil || !created {
		t.Fatal(err)
	}
	op.State = "applying"
	op.Step = "delete-profile"
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "delete-checkpoint"}); err != nil {
		t.Fatal(err)
	}
	op.State = "needs-review"
	if err := s.Finish(ctx, op, model.Audit{ID: "delete-review"}); err != nil {
		t.Fatal(err)
	}
	if locks := countLocks(t, s, op.ID); locks != 2 {
		t.Fatalf("needs-review released the namespace fence: %d locks", locks)
	}
	mutation := namespacedMutation("notebook.stop", "alice", "lab")
	if _, _, err := s.CreateOperation(ctx, queuedMutationOp(mutation, "mutation-op"), mutation, "mutation-review-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("mutation accepted while a Profile deletion needs review: %v", err)
	}
	// A second deletion of the same Profile is refused while the first one is
	// unresolved.
	second := profileDeletePlan("alice")
	second.ID = "plan-delete-alice-2"
	if _, _, err := s.CreateOperation(ctx, profileDeleteOp(second, "delete-op-2"), second, "profile-delete-second-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("second Profile deletion accepted while the first is unresolved: %v", err)
	}
	// The retry still resolves to the same accepted operation.
	prior, created, err := s.CreateOperation(ctx, profileDeleteOp(del, "delete-op"), del, "profile-delete-review-key")
	if err != nil || created || prior.ID != op.ID || prior.State != "needs-review" {
		t.Fatalf("idempotent retry did not resolve to the accepted operation: %v %v %v", prior, created, err)
	}
}

// A terminal outcome releases the namespace fence: the deletion either
// happened or was definitively refused before any mutation.
func TestProfileDeleteTerminalOutcomeReleasesTheNamespaceFence(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	for i, tc := range []struct {
		name  string
		state string
	}{
		{"succeeded", "succeeded"},
		{"failed", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := fmt.Sprintf("team-%d", i)
			del := profileDeletePlan(ns)
			op := profileDeleteOp(del, "delete-op-"+ns)
			if _, created, err := s.CreateOperation(ctx, op, del, "profile-delete-terminal-key-"+ns); err != nil || !created {
				t.Fatal(err)
			}
			if tc.state == "succeeded" {
				op.State = "applying"
				op.Step = "delete-profile"
				if err := s.Checkpoint(ctx, op, model.Audit{ID: "delete-checkpoint-" + ns}); err != nil {
					t.Fatal(err)
				}
			}
			op.State = tc.state
			if err := s.Finish(ctx, op, model.Audit{ID: "delete-outcome-" + ns}); err != nil {
				t.Fatal(err)
			}
			if locks := countLocks(t, s, op.ID); locks != 0 {
				t.Fatalf("terminal outcome kept %d locks", locks)
			}
			mutation := namespacedMutation("notebook.stop", ns, "lab")
			if _, created, err := s.CreateOperation(ctx, queuedMutationOp(mutation, "mutation-op-"+ns), mutation, "mutation-terminal-key-"+ns); err != nil || !created {
				t.Fatalf("mutation refused after a terminal deletion: %v", err)
			}
		})
	}
}

// The fence is deletion exclusivity only. Ordinary mutations in one namespace
// keep their own target locks and are never serialized against each other.
func TestNamespaceFenceDoesNotSerializeOrdinaryMutations(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	first := namespacedMutation("notebook.stop", "alice", "lab")
	second := namespacedMutation("pvc.create.dynamic", "alice", "data")
	if _, created, err := s.CreateOperation(ctx, queuedMutationOp(first, "first-op"), first, "first-mutation-key"); err != nil || !created {
		t.Fatal(err)
	}
	if _, created, err := s.CreateOperation(ctx, queuedMutationOp(second, "second-op"), second, "second-mutation-key"); err != nil || !created {
		t.Fatalf("ordinary mutations in one namespace were serialized against each other: %v", err)
	}
}

// The integrity check and the acceptance path share the single SQLite
// connection of the store. Neither may starve the other: a statement started
// outside the acceptance transaction while that transaction holds the
// connection would wait for it instead of running. The deadline turns such a
// regression into a failure instead of a hanging suite.
func TestProfileDeleteIntegrityCheckDoesNotStarveAcceptance(t *testing.T) {
	s := database(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	del := profileDeletePlan("alice")
	if _, created, err := s.CreateOperation(ctx, profileDeleteOp(del, "delete-op"), del, "profile-delete-starvation-key"); err != nil || !created {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if err := s.CheckIntegrity(ctx); err != nil {
				errs <- err
			}
		}()
		go func() {
			defer wg.Done()
			mutation := namespacedMutation("notebook.stop", "bob", fmt.Sprintf("lab-%d", i))
			mutation.ID = fmt.Sprintf("plan-mutation-%d", i)
			op := queuedMutationOp(mutation, fmt.Sprintf("mutation-op-%d", i))
			if _, _, err := s.CreateOperation(ctx, op, mutation, fmt.Sprintf("mutation-starvation-key-%d", i)); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent integrity check and acceptance failed: %v", err)
	}
}

// Two concurrent requests for the same namespace cannot both be accepted:
// exactly one wins and the other is refused.
func TestConcurrentProfileDeleteAndNamespaceMutationSerialize(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	for round := 0; round < 8; round++ {
		ns := fmt.Sprintf("team-%d", round)
		del := profileDeletePlan(ns)
		delOp := profileDeleteOp(del, "delete-op-"+ns)
		mutation := namespacedMutation("notebook.stop", ns, "lab")
		mutOp := queuedMutationOp(mutation, "mutation-op-"+ns)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var delCreated, mutCreated bool
		var delErr, mutErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, delCreated, delErr = s.CreateOperation(ctx, delOp, del, "delete-key-"+ns+"-round")
		}()
		go func() {
			defer wg.Done()
			<-start
			_, mutCreated, mutErr = s.CreateOperation(ctx, mutOp, mutation, "mutation-key-"+ns+"-round")
		}()
		close(start)
		wg.Wait()
		if delCreated == mutCreated {
			t.Fatalf("round %d: both requests were accepted or both were refused", round)
		}
		if !delCreated && !errors.Is(delErr, ErrConflict) {
			t.Fatalf("round %d: refused deletion reported %v", round, delErr)
		}
		if !mutCreated && !errors.Is(mutErr, ErrConflict) {
			t.Fatalf("round %d: refused mutation reported %v", round, mutErr)
		}
	}
}

// Two concurrent deletions of the same Profile cannot both be accepted.
func TestConcurrentProfileDeletionsSerialize(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	del := profileDeletePlan("alice")
	second := profileDeletePlan("alice")
	second.ID = "plan-delete-alice-2"
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan bool, 2)
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		_, created, err := s.CreateOperation(ctx, profileDeleteOp(del, "delete-op-1"), del, "delete-key-one-round")
		results <- created
		errs <- err
	}()
	go func() {
		defer wg.Done()
		<-start
		_, created, err := s.CreateOperation(ctx, profileDeleteOp(second, "delete-op-2"), second, "delete-key-two-round")
		results <- created
		errs <- err
	}()
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	accepted := 0
	for created := range results {
		if created {
			accepted++
		}
	}
	if accepted != 1 {
		t.Fatalf("%d concurrent deletions were accepted, want exactly 1", accepted)
	}
	for err := range errs {
		if err != nil && !errors.Is(err, ErrConflict) {
			t.Fatalf("refused deletion reported %v", err)
		}
	}
}

// A Profile deletion that lost its namespace fence is durable corruption: the
// store fails closed instead of letting another mutation into the namespace.
func TestProfileDeleteMissingNamespaceLockFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	del := profileDeletePlan("alice")
	op := profileDeleteOp(del, "delete-op")
	if _, created, err := s.CreateOperation(ctx, op, del, "profile-delete-integrity-key"); err != nil || !created {
		s.Close()
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE operation_targets SET active=0 WHERE operation_id=? AND target='namespace:alice'`, op.ID); err != nil {
		s.Close()
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); !errors.Is(err, ErrIntegrity) {
		s.Close()
		t.Fatalf("missing namespace fence was not a typed integrity failure: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if reopened != nil {
		reopened.Close()
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("database reopened without the namespace fence: %v", err)
	}
}
