package store

import (
	"context"
	"errors"
	"kubeflow-admin/internal/model"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func database(t *testing.T) *Store {
	t.Helper()
	s, e := Open(filepath.Join(t.TempDir(), "data.db"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func TestDurableRecordsAndSingleUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.db")
	s, e := Open(path)
	if e != nil {
		t.Fatal(e)
	}
	ctx := context.Background()
	if e = s.Put(ctx, "login", "attempt", map[string]string{"nonce": "random"}); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(path)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	var v map[string]string
	if e = s.Take(ctx, "login", "attempt", &v); e != nil || v["nonce"] != "random" {
		t.Fatal(v, e)
	}
	if e = s.Take(ctx, "login", "attempt", &v); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}
}
func TestConcurrentClaimsAndIdempotency(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	plan := model.Plan{ID: "plan", Actor: "issuer|subject"}
	op := model.Operation{ID: "operation", Actor: plan.Actor, PlanID: plan.ID, Target: "notebook:alice/work", State: "queued", CreatedAt: time.Now()}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _, e := s.CreateOperation(ctx, op, plan, "same-idempotency-key")
			if e != nil || got.ID != op.ID {
				t.Errorf("%+v %v", got, e)
			}
		}()
	}
	wg.Wait()
	next := op
	next.ID = "other"
	otherPlan := plan
	otherPlan.ID = "other-plan"
	next.PlanID = otherPlan.ID
	if _, _, e := s.CreateOperation(ctx, next, otherPlan, "different-key-123"); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if _, _, e := s.CreateOperation(ctx, next, otherPlan, "same-idempotency-key"); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	op.State = "succeeded"
	if e := s.Finish(ctx, op, model.Audit{ID: "done", Result: op.State}); e != nil {
		t.Fatal(e)
	}
	if _, created, e := s.CreateOperation(ctx, next, otherPlan, "different-key-123"); e != nil || !created {
		t.Fatal(created, e)
	}
	if rows, e := s.List(ctx, "audit", 10); e != nil || len(rows) != 3 {
		t.Fatal(len(rows), e)
	}
}
func TestTakeOnlyOnceAcrossGoroutines(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	if e := s.Put(ctx, "login", "a", "value"); e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	success := make(chan bool, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var v string
			e := s.Take(ctx, "login", "a", &v)
			if e == nil {
				success <- true
			} else if !errors.Is(e, ErrNotFound) {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	close(success)
	if len(success) != 1 {
		t.Fatal(len(success))
	}
}
