package operations

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/store"
)

func waitOperation(t *testing.T, m *Manager, id string) model.Operation {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		op, err := m.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if terminal(op.State) {
			return op
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("operation did not finish")
	return model.Operation{}
}
func accept(t *testing.T, m *Manager, actor model.Identity, name string) model.Operation {
	t.Helper()
	p, err := m.Plan(context.Background(), actor, model.Action{Kind: "notebook.stop", Namespace: "alice", Name: name, Reason: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	op, err := m.Submit(context.Background(), actor, p.ID, "idempotency-key-"+name)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func TestLogoutRevokesQueuedAndNextWrite(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var writes atomic.Int32
	b := &fakeBackend{execute: func(ctx context.Context, p model.Prepared, c func(string) error) error {
		if p.Action.Name == "first" {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := c("stop-notebook-before-edit"); err != nil {
			return err
		}
		writes.Add(1)
		return nil
	}}
	m, s, actor := setup(t, b)
	first := accept(t, m, actor, "first")
	<-started
	second := accept(t, m, actor, "second")
	if err := s.Delete(context.Background(), "auth.sessions", actor.SessionID); err != nil {
		t.Fatal(err)
	}
	close(release)
	if op := waitOperation(t, m, first.ID); op.State != "needs-review" {
		t.Fatal(op)
	}
	if op := waitOperation(t, m, second.ID); op.State != "failed" {
		t.Fatal(op)
	}
	if writes.Load() != 0 {
		t.Fatal("write after session logout")
	}
	if _, err := m.Plan(context.Background(), actor, model.Action{}); err == nil {
		t.Fatal("cached actor permitted preview after logout")
	}
}

func TestLongWaitNoticesRevocation(t *testing.T) {
	started := make(chan struct{})
	b := &fakeBackend{execute: func(ctx context.Context, p model.Prepared, c func(string) error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	m, s, actor := setup(t, b)
	op := accept(t, m, actor, "long")
	<-started
	// Removing current persisted groups must revoke even though actor.Groups
	// still contains the original admin membership.
	revoked := actor
	revoked.Groups = []string{"not-admins"}
	if err := s.Put(context.Background(), "auth.sessions", actor.SessionID, map[string]any{"identity": revoked}); err != nil {
		t.Fatal(err)
	}
	if got := waitOperation(t, m, op.ID); got.State != "needs-review" {
		t.Fatal(got)
	}
}

func TestShutdownAccountsForEveryAcceptedOperation(t *testing.T) {
	started := make(chan struct{})
	var executions atomic.Int32
	b := &fakeBackend{execute: func(ctx context.Context, p model.Prepared, c func(string) error) error {
		executions.Add(1)
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}}
	m, s, actor := setup(t, b)
	first := accept(t, m, actor, "first")
	<-started
	var queued []model.Operation
	for _, name := range []string{"second", "third", "fourth"} {
		queued = append(queued, accept(t, m, actor, name))
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() { defer wg.Done(); m.Close() }()
	}
	wg.Wait()
	if executions.Load() != 1 {
		t.Fatal("queued operation ran during shutdown")
	}
	if got := waitOperation(t, m, first.ID); got.State != "needs-review" {
		t.Fatal(got)
	}
	for _, op := range queued {
		if got := waitOperation(t, m, op.ID); got.State != "failed" {
			t.Fatal("accepted operation left queued", got)
		}
	}
	audit, err := m.Audit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range append(queued, first) {
		found := false
		for _, a := range audit {
			if a.OperationID == op.ID && terminal(a.Result) {
				found = true
			}
		}
		if !found {
			t.Fatal("accepted operation lacks outcome audit", op.ID)
		}
	}
	if _, err := m.Submit(context.Background(), actor, "anything", "idempotency-key-1"); err == nil {
		t.Fatal("accepted after shutdown")
	}
	// A new worker sees terminal outcomes only; it never replays queued work.
	next, err := newManager(context.Background(), m.cfg, s, b)
	if err != nil {
		t.Fatal(err)
	}
	next.Close()
	if executions.Load() != 1 {
		t.Fatal("startup replayed an accepted task")
	}
}

func TestConcurrentAcceptanceExecutesOnce(t *testing.T) {
	var executions atomic.Int32
	b := &fakeBackend{execute: func(ctx context.Context, p model.Prepared, c func(string) error) error {
		if err := c("stop-notebook-before-edit"); err != nil {
			return err
		}
		executions.Add(1)
		return nil
	}}
	m, _, actor := setup(t, b)
	p, err := m.Plan(context.Background(), actor, model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "lab"})
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan string, 20)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			op, err := m.Submit(context.Background(), actor, p.ID, "same-idempotency-key")
			if err != nil {
				t.Error(err)
				return
			}
			results <- op.ID
		}()
	}
	wg.Wait()
	close(results)
	var operationID string
	for got := range results {
		if operationID == "" {
			operationID = got
		} else if got != operationID {
			t.Fatal("multiple IDs for one acceptance")
		}
	}
	if op := waitOperation(t, m, operationID); op.State != "succeeded" {
		t.Fatal(op)
	}
	if executions.Load() != 1 {
		t.Fatal("duplicate execution", executions.Load())
	}
}

func TestPersistedErrorsNeverIncludeBackendDetails(t *testing.T) {
	secret := "ADMISSION_ENV_VALUE=totally-private-value /private/config/provider-error"
	b := &fakeBackend{prepare: func(context.Context, model.Action) (model.Prepared, error) {
		return model.Prepared{}, errors.New(secret)
	}}
	m, _, actor := setup(t, b)
	if _, err := m.Plan(context.Background(), actor, model.Action{}); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("preview leaked backend detail", err)
	}
	b.prepare = nil
	b.execute = func(context.Context, model.Prepared, func(string) error) error { return errors.New(secret) }
	op := accept(t, m, actor, "error")
	got := waitOperation(t, m, op.ID)
	if got.State != "needs-review" || strings.Contains(got.Error, "private") || strings.Contains(got.Error, "ADMISSION") {
		t.Fatal("unsafe outcome", got)
	}
	audit, err := m.Audit(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(audit)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), actor.SessionID) {
		t.Fatal("audit leaks backend detail/session credential")
	}
}

func TestSecondManagerCannotRecoverLiveWorker(t *testing.T) {
	m, s, _ := setup(t, &fakeBackend{})
	if other, err := newManager(context.Background(), m.cfg, s, &fakeBackend{}); err == nil {
		other.Close()
		t.Fatal("second manager acquired live database")
	}
}

func TestStartupQueuedAcceptanceIsReviewedAndLocked(t *testing.T) {
	m, s, actor := setup(t, &fakeBackend{})
	m.Close()
	p := model.Plan{ID: "durable-plan", Actor: actor.Key(), Prepared: model.Prepared{Action: model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "lab"}}}
	op := model.Operation{ID: "durable-op", PlanID: p.ID, Actor: p.Actor, Target: "notebook:alice/lab", State: "queued"}
	if _, _, err := s.CreateOperation(context.Background(), op, p, "durable-key"); err != nil {
		t.Fatal(err)
	}
	var executed atomic.Bool
	b := &fakeBackend{execute: func(context.Context, model.Prepared, func(string) error) error { executed.Store(true); return nil }}
	next, err := newManager(context.Background(), m.cfg, s, b)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if got := waitOperation(t, next, op.ID); got.State != "needs-review" {
		t.Fatal(got)
	}
	if executed.Load() {
		t.Fatal("startup executed queued privileged action")
	}
	otherPlan := p
	otherPlan.ID = "new-plan"
	other := op
	other.ID = "new-op"
	other.PlanID = otherPlan.ID
	if _, _, err := s.CreateOperation(context.Background(), other, otherPlan, "new-key"); !errors.Is(err, store.ErrConflict) {
		t.Fatal("recovery unlocked uncertain target", err)
	}
}
