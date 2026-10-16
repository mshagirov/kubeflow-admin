package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"kubeflow-admin/internal/model"
)

func planFor(kind, ns, name, pv string) model.Plan {
	return model.Plan{ID: "plan-" + name, Actor: "issuer|admin", Prepared: model.Prepared{Action: model.Action{Kind: kind, Namespace: ns, Name: name, PVName: pv}}}
}

func queuedOp(plan model.Plan, id, target string) model.Operation {
	return model.Operation{ID: id, PlanID: plan.ID, Actor: plan.Actor, Target: target, State: "queued", CreatedAt: time.Now()}
}

// A malformed JSON row must never abort cleanup of the remaining records. Before
// this hardening, json_extract raised on the malformed row and the whole delete
// failed, so authentication traffic failed closed for every request forever.
func TestCleanupSurvivesMalformedJSONRows(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	now := time.Now()
	if _, err := s.db.Exec(`INSERT INTO records(bucket,key,value,updated) VALUES('auth.attempts','bad','not json',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO records(bucket,key,value,updated) VALUES('auth.sessions','bad2',X'00ff',2)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO records(bucket,key,value,updated) VALUES('plans','bad3','not json',3)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "auth.attempts", "old", map[string]any{"expiresAt": now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "auth.attempts", "live", map[string]any{"expiresAt": now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "plans", "expired", model.Plan{ID: "expired", ExpiresAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "plans", "live", model.Plan{ID: "live", ExpiresAt: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupAuth(ctx, now); err != nil {
		t.Fatal("malformed auth row aborted authentication cleanup", err)
	}
	if err := s.CleanupPlans(ctx, now); err != nil {
		t.Fatal("malformed plan row aborted plan cleanup", err)
	}
	for _, tc := range []struct {
		bucket, key string
		gone        bool
	}{
		{"auth.attempts", "bad", true},
		{"auth.sessions", "bad2", true},
		{"auth.attempts", "old", true},
		{"auth.attempts", "live", false},
		{"plans", "bad3", true},
		{"plans", "expired", true},
		{"plans", "live", false},
	} {
		var v model.Plan
		err := s.Get(ctx, tc.bucket, tc.key, &v)
		if tc.gone && !errors.Is(err, ErrNotFound) || !tc.gone && err != nil {
			t.Errorf("%s/%s gone=%v: %v", tc.bucket, tc.key, tc.gone, err)
		}
	}
}

func TestCleanupPlansNeverTouchesOperationsOrAudit(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	now := time.Now()
	if err := s.Put(ctx, "operations", "forever", map[string]any{"expiresAt": now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "audit", "forever", map[string]any{"expiresAt": now.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupPlans(ctx, now); err != nil {
		t.Fatal(err)
	}
	for _, bucket := range []string{"operations", "audit"} {
		var v any
		if err := s.Get(ctx, bucket, "forever", &v); err != nil {
			t.Fatal(bucket, "was cleaned", err)
		}
	}
}

// The primary target must be exactly the canonical target of the server-owned
// action; a caller must not be able to lock an arbitrary target.
func TestRejectsNonCanonicalPrimaryTarget(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	plan := planFor("pv.reserve-and-claim", "alice", "claim", "pv-a")
	tampered := queuedOp(plan, "tampered", "pv:victim")
	if _, _, err := s.CreateOperation(ctx, tampered, plan, "idempotency-key-tampered"); !errors.Is(err, ErrConflict) {
		t.Fatal("arbitrary primary target accepted", err)
	}
	canonical := queuedOp(plan, "canonical", "pvc:alice/claim")
	if _, created, err := s.CreateOperation(ctx, canonical, plan, "idempotency-key-canonical"); err != nil || !created {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_targets WHERE operation_id='canonical' AND active=1`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected canonical primary plus reserved PV locks, got %d", n)
	}
}

func TestOnlyPVActionsAcquireSecondaryPVLocks(t *testing.T) {
	for _, kind := range []string{"notebook.stop", "notebook.edit", "profile.create", "profile.quota.change", "pvc.create.dynamic", "pvc.expand", "storage.create.static", "pv.reserve-and-claim"} {
		t.Run(kind, func(t *testing.T) {
			s := database(t)
			plan := planFor(kind, "alice", "alice", "unrelated")
			op := queuedOp(plan, "op", PrimaryTarget(plan.Prepared.Action))
			if _, _, err := s.CreateOperation(context.Background(), op, plan, "test-pv-target-key"); err != nil {
				t.Fatal(err)
			}
			var pvLocks int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_targets WHERE operation_id='op' AND target='pv:unrelated' AND active=1`).Scan(&pvLocks); err != nil {
				t.Fatal(err)
			}
			want := 0
			if kind == "storage.create.static" || kind == "pv.reserve-and-claim" {
				want = 1
			}
			if pvLocks != want {
				t.Fatalf("%s acquired %d PV locks, want %d", kind, pvLocks, want)
			}
		})
	}
}

func TestRejectsInvalidStateTransitions(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	plan := planFor("notebook.stop", "alice", "lab", "")
	op := queuedOp(plan, "op-trans", "notebook:alice/lab")
	if _, _, err := s.CreateOperation(ctx, op, plan, "idempotency-key-trans"); err != nil {
		t.Fatal(err)
	}
	// A never-started operation cannot report success.
	skipped := op
	skipped.State = "succeeded"
	if err := s.Finish(ctx, skipped, model.Audit{ID: "skipped"}); !errors.Is(err, ErrConflict) {
		t.Fatal("queued->succeeded accepted", err)
	}
	// queued -> applying -> succeeded is the valid path.
	op.State = "applying"
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "cp"}); err != nil {
		t.Fatal(err)
	}
	op.State = "succeeded"
	if err := s.Finish(ctx, op, model.Audit{ID: "done"}); err != nil {
		t.Fatal(err)
	}
	// Terminal operations cannot move again.
	op.State = "applying"
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "late"}); !errors.Is(err, ErrConflict) {
		t.Fatal("terminal checkpoint accepted", err)
	}
	// queued -> failed (shutdown) and queued/applying -> needs-review (recovery)
	// must keep working.
	shutdownPlan := planFor("notebook.stop", "alice", "shutdown", "")
	shutdown := queuedOp(shutdownPlan, "op-shutdown", "notebook:alice/shutdown")
	if _, _, err := s.CreateOperation(ctx, shutdown, shutdownPlan, "idempotency-key-shutdown"); err != nil {
		t.Fatal(err)
	}
	shutdown.State = "failed"
	if err := s.Finish(ctx, shutdown, model.Audit{ID: "shutdown-failed"}); err != nil {
		t.Fatal("queued->failed broken", err)
	}
	recoveredPlan := planFor("notebook.stop", "alice", "recovered", "")
	recovered := queuedOp(recoveredPlan, "op-recovered", "notebook:alice/recovered")
	if _, _, err := s.CreateOperation(ctx, recovered, recoveredPlan, "idempotency-key-recovered"); err != nil {
		t.Fatal(err)
	}
	recovered.State = "needs-review"
	if err := s.Finish(ctx, recovered, model.Audit{ID: "recovered"}); err != nil {
		t.Fatal("queued->needs-review broken", err)
	}
	applyingPlan := planFor("notebook.stop", "alice", "applying", "")
	applying := queuedOp(applyingPlan, "op-applying", "notebook:alice/applying")
	if _, _, err := s.CreateOperation(ctx, applying, applyingPlan, "idempotency-key-applying"); err != nil {
		t.Fatal(err)
	}
	applying.State = "applying"
	if err := s.Checkpoint(ctx, applying, model.Audit{ID: "applying-cp"}); err != nil {
		t.Fatal(err)
	}
	applying.State = "needs-review"
	if err := s.Finish(ctx, applying, model.Audit{ID: "applying-review"}); err != nil {
		t.Fatal("applying->needs-review broken", err)
	}
}

func TestMissingAcceptedAuditFailsClosed(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	plan := planFor("notebook.stop", "alice", "lab", "")
	op := queuedOp(plan, "op-audit", "notebook:alice/lab")
	if _, _, err := s.CreateOperation(ctx, op, plan, "idempotency-key-audit"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "audit", op.ID+"-accepted"); err != nil {
		t.Fatal(err)
	}
	op.State = "applying"
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "cp"}); !errors.Is(err, ErrIntegrity) {
		t.Fatal("missing accepted audit not a typed integrity error", err)
	}
	// Fail closed: the operation is unchanged and no audit was fabricated.
	var got model.Operation
	if err := s.Get(ctx, "operations", op.ID, &got); err != nil || got.State != "queued" {
		t.Fatal("operation changed despite integrity failure", got, err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records WHERE bucket='audit'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("audit row fabricated")
	}
}

func TestCheckIntegrityDetectsOrphansAndFailsClosed(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatal("healthy store rejected", err)
	}
	// Active claim with no operation record.
	if _, err := s.db.Exec(`INSERT INTO operation_claims(id,actor,idem,plan_id,target,active) VALUES('orphan','issuer|admin','key','plan','pvc:alice/x',1)`); err != nil {
		t.Fatal(err)
	}
	err := s.CheckIntegrity(ctx)
	if !errors.Is(err, ErrIntegrity) {
		t.Fatal("orphan active claim not detected", err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_claims WHERE id='orphan' AND active=1`).Scan(&count); err != nil || count != 1 {
		t.Fatal("integrity check altered the orphan claim", count, err)
	}
	if _, err := s.db.Exec(`DELETE FROM operation_claims WHERE id='orphan'`); err != nil {
		t.Fatal(err)
	}
	// Active target lock with no active claim.
	if _, err := s.db.Exec(`INSERT INTO operation_targets(operation_id,target,active) VALUES('ghost','pv:orphan',1)`); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); !errors.Is(err, ErrIntegrity) {
		t.Fatal("orphan target lock not detected", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_targets WHERE operation_id='ghost' AND active=1`).Scan(&count); err != nil || count != 1 {
		t.Fatal("integrity check silently unlocked the orphan target", count, err)
	}
	// Active claim whose operation is already terminal.
	if _, err := s.db.Exec(`UPDATE operation_targets SET active=0 WHERE operation_id='ghost'`); err != nil {
		t.Fatal(err)
	}
	plan := planFor("notebook.stop", "alice", "lab", "")
	op := queuedOp(plan, "op-terminal", "notebook:alice/lab")
	if _, _, err := s.CreateOperation(ctx, op, plan, "idempotency-key-terminal"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE records SET value=? WHERE bucket='operations' AND key='op-terminal'`, []byte(`{"id":"op-terminal","planID":"plan-lab","actor":"issuer|admin","target":"notebook:alice/lab","state":"succeeded"}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); !errors.Is(err, ErrIntegrity) {
		t.Fatal("active claim with terminal operation not detected", err)
	}
}

// New sidecars must inherit the private main database mode even under a
// permissive umask, and pre-existing unsafe sidecars must be rejected. The
// umask is changed only in a child process, never in the library.
func TestSidecarFilesStayPrivateUnderPermissiveUmask(t *testing.T) {
	if path := os.Getenv("STORE_UMASK_TEST_PATH"); path != "" {
		syscall.Umask(0)
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		ctx := context.Background()
		for i := 0; i < 64; i++ {
			if err := s.Put(ctx, "probe", fmt.Sprint(i), "value"); err != nil {
				t.Fatal(err)
			}
		}
		var report strings.Builder
		for _, p := range []string{path, path + "-wal", path + "-shm"} {
			info, err := os.Lstat(p)
			if err != nil {
				report.WriteString(filepath.Base(p) + " MISSING\n")
				continue
			}
			fmt.Fprintf(&report, "%s %04o regular=%v\n", filepath.Base(p), info.Mode().Perm(), info.Mode().IsRegular())
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		// The rollback journal is transient; hold a write transaction open to
		// observe its inherited mode before rolling back.
		deletePath := path + ".delete.db"
		sd, err := OpenWithJournal(deletePath, "delete", false)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := sd.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO records(bucket,key,value,updated) VALUES('probe','journal','x',1)`); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Lstat(deletePath + "-journal"); err != nil {
			report.WriteString("journal MISSING\n")
		} else {
			fmt.Fprintf(&report, "journal %04o regular=%v\n", info.Mode().Perm(), info.Mode().IsRegular())
		}
		if err := tx.Rollback(); err != nil {
			t.Fatal(err)
		}
		if err := sd.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path+".report", []byte(report.String()), 0600); err != nil {
			t.Fatal(err)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "data.db")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSidecarFilesStayPrivateUnderPermissiveUmask$")
	cmd.Env = append(os.Environ(), "STORE_UMASK_TEST_PATH="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("permissive-umask child: %v %s", err, output)
	}
	report, err := os.ReadFile(path + ".report")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(report)), "\n")
	if len(lines) < 2 {
		t.Fatalf("unexpected report: %q", report)
	}
	for _, line := range lines {
		if strings.Contains(line, "MISSING") {
			t.Fatalf("expected WAL-mode sidecar missing: %s", line)
		}
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[1] != "0600" || fields[2] != "regular=true" {
			t.Fatalf("sidecar not private: %s", line)
		}
	}
}

func TestRejectsUnsafeExistingSidecars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.db")
	target := filepath.Join(dir, "outside")
	if err := os.WriteFile(target, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range sidecarSuffixes {
		link := path + suffix
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		if s, err := Open(path); err == nil {
			s.Close()
			t.Fatalf("followed %s symlink", suffix)
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(link, nil, 0644); err != nil {
			t.Fatal(err)
		}
		if s, err := Open(path); err == nil {
			s.Close()
			t.Fatalf("accepted world-readable %s", suffix)
		}
		if err := os.Remove(link); err != nil {
			t.Fatal(err)
		}
	}
}
