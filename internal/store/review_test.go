package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"kubeflow-admin/internal/model"
)

func TestExclusiveProcessLockAndIdempotentClose(t *testing.T) {
	if path := os.Getenv("STORE_LOCK_TEST_PATH"); path != "" {
		if s, err := Open(path); err == nil {
			s.Close()
			t.Fatal("another process acquired the database")
		}
		return
	}
	path := filepath.Join(t.TempDir(), "data.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := Open(path); err == nil {
		other.Close()
		t.Fatal("second open succeeded")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestExclusiveProcessLockAndIdempotentClose$")
	cmd.Env = append(os.Environ(), "STORE_LOCK_TEST_PATH="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("process lock: %v %s", err, output)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	next, err := Open(path)
	if err != nil {
		t.Fatal("lock not released", err)
	}
	defer next.Close()
	var mode string
	var synchronous int
	if err := next.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if err := next.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" || synchronous != 2 {
		t.Fatalf("durability pragmas %s %d", mode, synchronous)
	}
	// Backups/readers can still take SQLite read connections; the application
	// flock must not turn into SQLite locking_mode=EXCLUSIVE.
	reader, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var count int
	if err := reader.QueryRow("SELECT count(*) FROM records").Scan(&count); err != nil {
		t.Fatal("backup reader blocked", err)
	}
	// Durability pragmas also apply after the pool replaces a connection.
	next.db.SetMaxIdleConns(0)
	if err := next.db.QueryRow("PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatal("replacement connection lost FULL synchronization", synchronous, err)
	}
	// Repeated old Close must not release the new owner's lock.
	s.Close()
	if other, err := Open(path); err == nil {
		other.Close()
		t.Fatal("old close released new lock")
	}
}

func TestOpenFailureReleasesLockAndRejectsUnsafeFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.db")
	if err := os.WriteFile(path, []byte("not sqlite"), 0600); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("opened corrupt database")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal("failure leaked application lock", err)
	}
	s.Close()
	link := filepath.Join(t.TempDir(), "symlink.db")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(link); err == nil {
		s.Close()
		t.Fatal("followed database symlink")
	}
	unsafe := filepath.Join(t.TempDir(), "public.db")
	if err := os.WriteFile(unsafe, nil, 0644); err != nil {
		t.Fatal(err)
	}
	if s, err := Open(unsafe); err == nil {
		s.Close()
		t.Fatal("opened world-readable database")
	}
}

func TestTransactionalOperationAuditAndLocks(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	p := model.Plan{ID: "plan", Actor: "issuer|admin", Prepared: model.Prepared{Action: model.Action{Kind: "pvc.create.dynamic", Namespace: "alice", Name: "data"}}}
	p.Prepared.Action.Reason = "approved maintenance"
	p.Prepared.Changes = []model.Change{{Field: "spec.resources.requests.storage", Before: "1Gi", After: "2Gi"}}
	op := model.Operation{ID: "op", PlanID: p.ID, Actor: p.Actor, Target: "pvc:alice/data", Action: p.Prepared.Action.Kind, State: "queued", CreatedAt: time.Now()}
	// Acceptance must roll back claims AND operation when audit cannot commit.
	if _, err := s.db.Exec(`CREATE TRIGGER reject_audit BEFORE INSERT ON records WHEN NEW.bucket='audit' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateOperation(ctx, op, p, "idempotency-key-1"); err == nil {
		t.Fatal("accepted without audit")
	}
	var got model.Operation
	if err := s.Get(ctx, "operations", op.ID, &got); !errors.Is(err, ErrNotFound) {
		t.Fatal("partial acceptance", err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_audit`); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.CreateOperation(ctx, op, p, "idempotency-key-1"); err != nil || !created {
		t.Fatal("claim was not rolled back", err)
	}
	op.State = "applying"
	op.Step = "create-pvc"
	op.UpdatedAt = time.Now()
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "checkpoint"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_audit BEFORE INSERT ON records WHEN NEW.bucket='audit' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	op.State = "succeeded"
	if err := s.Finish(ctx, op, model.Audit{ID: "outcome"}); err == nil {
		t.Fatal("finished without audit")
	}
	if err := s.Get(ctx, "operations", op.ID, &got); err != nil || got.State != "applying" {
		t.Fatal("outcome was not rolled back", got, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_audit`); err != nil {
		t.Fatal(err)
	}
	otherPlan := p
	otherPlan.ID = "plan-2"
	other := op
	other.ID = "op-2"
	other.PlanID = otherPlan.ID
	other.State = "queued"
	if _, _, err := s.CreateOperation(ctx, other, otherPlan, "idempotency-key-2"); !errors.Is(err, ErrConflict) {
		t.Fatal("failed outcome released lock", err)
	}
	// A duplicate audit ID is immutable, not an upsert over history.
	if err := s.Finish(ctx, op, model.Audit{ID: "checkpoint"}); err == nil {
		t.Fatal("overwrote an audit record")
	}
	if err := s.Finish(ctx, op, model.Audit{ID: "outcome"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{op.ID + "-accepted", "checkpoint", "outcome"} {
		var a model.Audit
		if err := s.Get(ctx, "audit", id, &a); err != nil {
			t.Fatal(err)
		}
		if a.Reason != p.Prepared.Action.Reason || len(a.Changes) != 1 || a.Changes[0] != p.Prepared.Changes[0] {
			t.Fatal("audit lost server-owned reason/diff", a)
		}
	}
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "late"}); err == nil {
		t.Fatal("terminal operation was overwritten")
	}
	if _, _, err := s.CreateOperation(ctx, other, otherPlan, "idempotency-key-2"); err != nil {
		t.Fatal("success did not release lock", err)
	}
}

func TestMultiTargetLockAndReviewRetention(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	makeOp := func(id, pvc, pv string) (model.Operation, model.Plan) {
		p := model.Plan{ID: id, Actor: "issuer|admin", Prepared: model.Prepared{Action: model.Action{Kind: "pv.reserve-and-claim", Namespace: "alice", Name: pvc, PVName: pv}}}
		return model.Operation{ID: "op-" + id, PlanID: id, Actor: p.Actor, Target: "pvc:alice/" + pvc, Action: p.Prepared.Action.Kind, State: "queued"}, p
	}
	op, p := makeOp("first", "claim", "pv-a")
	if _, _, err := s.CreateOperation(ctx, op, p, "first-key"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ id, pvc, pv string }{{"same-pvc", "claim", "pv-b"}, {"same-pv", "other", "pv-a"}} {
		next, plan := makeOp(tc.id, tc.pvc, tc.pv)
		if _, _, err := s.CreateOperation(ctx, next, plan, tc.id); !errors.Is(err, ErrConflict) {
			t.Fatal("conflicting target accepted", tc, err)
		}
	}
	op.State = "needs-review"
	op.UpdatedAt = time.Now()
	if err := s.Finish(ctx, op, model.Audit{ID: "review"}); err != nil {
		t.Fatal(err)
	}
	next, plan := makeOp("after-review", "other", "pv-a")
	if _, _, err := s.CreateOperation(ctx, next, plan, "next-key"); !errors.Is(err, ErrConflict) {
		t.Fatal("review released PV lock", err)
	}
	if prior, created, err := s.CreateOperation(ctx, model.Operation{ID: "retry", PlanID: p.ID, Actor: p.Actor, Target: op.Target, Action: p.Prepared.Action.Kind, State: "queued"}, p, "first-key"); err != nil || created || prior.ID != op.ID || prior.State != "needs-review" {
		t.Fatal("review retry not idempotent", prior, created, err)
	}
}

func TestCleanupAuthenticationOnlyIncludingOldRecords(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	now := time.Now()
	put := func(bucket, key string, v any) {
		t.Helper()
		if err := s.Put(ctx, bucket, key, v); err != nil {
			t.Fatal(err)
		}
	}
	put("auth.attempts", "old", map[string]any{"expiresAt": now.Add(-time.Minute)})
	put("auth.attempts", "live", map[string]any{"expiresAt": now.Add(time.Minute)})
	put("auth.sessions", "old", map[string]any{"identity": model.Identity{AuthenticatedAt: now.Add(-10 * time.Minute), ExpiresAt: now.Add(-time.Minute)}})
	put("auth.sessions", "overlong", map[string]any{"identity": model.Identity{AuthenticatedAt: now.Add(-16 * time.Minute), ExpiresAt: now.Add(time.Hour)}})
	put("auth.sessions", "live", map[string]any{"identity": model.Identity{AuthenticatedAt: now, ExpiresAt: now.Add(time.Minute)}})
	put("audit", "forever", map[string]any{"expiresAt": now.Add(-time.Hour)})
	put("operations", "forever", map[string]any{"expiresAt": now.Add(-time.Hour)})
	if err := s.CleanupAuth(ctx, now); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		bucket, key string
		missing     bool
	}{{"auth.attempts", "old", true}, {"auth.attempts", "live", false}, {"auth.sessions", "old", true}, {"auth.sessions", "overlong", true}, {"auth.sessions", "live", false}, {"audit", "forever", false}, {"operations", "forever", false}} {
		var v any
		err := s.Get(ctx, tc.bucket, tc.key, &v)
		if tc.missing && !errors.Is(err, ErrNotFound) || !tc.missing && err != nil {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}
