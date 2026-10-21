package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"kubeflow-admin/internal/model"
)

// legacyDatabase writes a schema-v2 database directly, as the previous release
// would have left it. Tests then verify transactional migration/backfill.
func legacyDatabase(t *testing.T, withAcceptedAudit bool) string {
	t.Helper()
	return legacyDatabaseMutated(t, withAcceptedAudit, nil, nil)
}

func legacyDatabaseMutated(t *testing.T, withAcceptedAudit bool, mutateOp func(*model.Operation), mutateAudit func(*model.Audit)) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		`CREATE TABLE records(bucket TEXT NOT NULL,key TEXT NOT NULL,value BLOB NOT NULL,updated INTEGER NOT NULL,PRIMARY KEY(bucket,key))`,
		`CREATE TABLE operation_claims(id TEXT PRIMARY KEY,actor TEXT NOT NULL,idem TEXT NOT NULL,plan_id TEXT NOT NULL,target TEXT NOT NULL,active INTEGER NOT NULL DEFAULT 1,UNIQUE(actor,idem),UNIQUE(plan_id))`,
		`CREATE UNIQUE INDEX active_target ON operation_claims(target) WHERE active=1`,
		`CREATE TABLE operation_targets(operation_id TEXT NOT NULL,target TEXT NOT NULL,active INTEGER NOT NULL DEFAULT 1,PRIMARY KEY(operation_id,target))`,
		`CREATE UNIQUE INDEX locked_target ON operation_targets(target) WHERE active=1`,
		`PRAGMA user_version=2`,
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	op := model.Operation{ID: "legacy-op", PlanID: "legacy-plan", Actor: "https://issuer|admin", ActorDisplay: "Legacy Admin", Target: "notebook:alice/lab", Action: "notebook.stop", State: "queued", Step: "queued", CreatedAt: now, UpdatedAt: now}
	if mutateOp != nil {
		mutateOp(&op)
	}
	value, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO records(bucket,key,value,updated) VALUES('operations',?,?,?)`, op.ID, value, now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO operation_claims(id,actor,idem,plan_id,target,active) VALUES(?,?,?,?,?,1)`, "legacy-op", "https://issuer|admin", "legacy-idempotency-key", "legacy-plan", "notebook:alice/lab"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO operation_targets(operation_id,target,active) VALUES(?,?,1)`, "legacy-op", "notebook:alice/lab"); err != nil {
		t.Fatal(err)
	}
	if withAcceptedAudit {
		audit := model.Audit{ID: op.ID + "-accepted", Actor: op.Actor, ActorDisplay: op.ActorDisplay, Action: op.Action, Target: op.Target, Result: "queued", OperationID: op.ID, Reason: "approved legacy maintenance", Changes: []model.Change{{Field: "spec", Before: "a", After: "b"}}, Time: now}
		if mutateAudit != nil {
			mutateAudit(&audit)
		}
		auditValue, err := json.Marshal(audit)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO records(bucket,key,value,updated) VALUES('audit',?,?,?)`, op.ID+"-accepted", auditValue, now.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMigrationBackfillsAcceptanceLedgerAndKeepsVersion(t *testing.T) {
	path := legacyDatabase(t, true)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatal("schema version not migrated", version, err)
	}
	var reason, display string
	var changes []byte
	if err := s.db.QueryRow(`SELECT reason,actor_display,changes FROM operation_acceptance WHERE operation_id='legacy-op'`).Scan(&reason, &display, &changes); err != nil {
		t.Fatal("authentic accepted audit was not backfilled", err)
	}
	var restored []model.Change
	if err := json.Unmarshal(changes, &restored); err != nil || reason != "approved legacy maintenance" || display != "Legacy Admin" || len(restored) != 1 {
		t.Fatal("backfill lost accepted evidence", reason, display, restored, err)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatal("healthy migrated store rejected", err)
	}
	// Checkpoint now reads the ledger; the visible audit row is not required.
	if err := s.Delete(ctx, "audit", "legacy-op-accepted"); err != nil {
		t.Fatal(err)
	}
	op := model.Operation{ID: "legacy-op", PlanID: "legacy-plan", Actor: "https://issuer|admin", Target: "notebook:alice/lab", Action: "notebook.stop", State: "applying", Step: "validating", UpdatedAt: time.Now()}
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "legacy-checkpoint"}); err != nil {
		t.Fatal("checkpoint after visible-history cleanup", err)
	}
	var audit model.Audit
	if err := s.Get(ctx, "audit", "legacy-checkpoint", &audit); err != nil || audit.Reason != "approved legacy maintenance" || len(audit.Changes) != 1 || audit.ActorDisplay != "Legacy Admin" {
		t.Fatal("checkpoint lost accepted evidence", audit, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening must not reset the version or repeat/harm the backfill.
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatal("unconditional schema reset regression", version, err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_acceptance WHERE operation_id='legacy-op'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("ledger changed on reopen", n, err)
	}
}

func TestMigrationRejectsConflictingAcceptedEvidence(t *testing.T) {
	cases := []struct {
		name string
		op   func(*model.Operation)
		aud  func(*model.Audit)
	}{
		{"target", nil, func(a *model.Audit) { a.Target = "notebook:alice/other" }},
		{"action", nil, func(a *model.Audit) { a.Action = "notebook.edit" }},
		{"operation-id", nil, func(a *model.Audit) { a.OperationID = "other-op" }},
		{"audit-id", nil, func(a *model.Audit) { a.ID = "other-id" }},
		{"actor", nil, func(a *model.Audit) { a.Actor = "https://issuer|other" }},
		{"plan", func(o *model.Operation) { o.PlanID = "other-plan" }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := legacyDatabaseMutated(t, true, tc.op, tc.aud)
			s, err := Open(path)
			if err == nil {
				s.Close()
				t.Fatal("conflicting evidence was committed as a schema migration")
			}
			if !errors.Is(err, ErrIntegrity) {
				t.Fatal("expected typed integrity failure", err)
			}
		})
	}
}

func TestMigrationRestoreAfterFailedOpenBackfills(t *testing.T) {
	path := legacyDatabase(t, false)
	if s, err := Open(path); err == nil {
		s.Close()
		t.Fatal("missing active acceptance evidence was accepted")
	} else if !errors.Is(err, ErrIntegrity) {
		t.Fatal("expected typed integrity failure", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
		db.Close()
		t.Fatal("failed migration committed a version bump", version, err)
	}
	// The failed transaction rolled back, so restoring authentic evidence lets the
	// next open complete the version bump and backfill.
	now := time.Now().UTC()
	audit := model.Audit{ID: "legacy-op-accepted", Actor: "https://issuer|admin", ActorDisplay: "Legacy Admin", Action: "notebook.stop", Target: "notebook:alice/lab", Result: "queued", OperationID: "legacy-op", Reason: "restored maintenance", Changes: []model.Change{{Field: "spec", Before: "a", After: "b"}}, Time: now}
	value, err := json.Marshal(audit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO records(bucket,key,value,updated) VALUES('audit',?,?,?)`, audit.ID, value, now.UnixNano()); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err != nil {
		t.Fatal("restored authentic evidence did not permit reopen", err)
	}
	defer s.Close()
	var reason string
	if err := s.db.QueryRow(`SELECT reason FROM operation_acceptance WHERE operation_id='legacy-op'`).Scan(&reason); err != nil || reason != "restored maintenance" {
		t.Fatal("restored evidence was not backfilled", reason, err)
	}
}

func TestVersion3MissingLedgerBackfillsOnReopen(t *testing.T) {
	path := legacyDatabase(t, true)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate a version-3 development database that lost its ledger but still
	// has the authentic accepted audit row.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`DELETE FROM operation_acceptance`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal("version-3 database did not heal from authentic evidence", err)
	}
	defer s.Close()
	var version, ledger int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatal("version changed unexpectedly", version, err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_acceptance`).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatal("missing version-3 ledger was not backfilled", ledger, err)
	}
}

func TestMigrationRejectsSilentlyIgnoredTargetConflict(t *testing.T) {
	path := legacyDatabase(t, true)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE operation_targets SET operation_id='ghost' WHERE operation_id='legacy-op'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err == nil {
		s.Close()
		t.Fatal("migration silently accepted a conflicting active target")
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatal("expected typed integrity failure", err)
	}
}

func TestMigrationRejectsMalformedAcceptedEvidence(t *testing.T) {
	path := legacyDatabase(t, false)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO records(bucket,key,value,updated) VALUES('audit','legacy-op-accepted','PRIVATE-MALFORMED-ACCEPTED',?)`, time.Now().UnixNano()); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(path)
	if err == nil {
		s.Close()
		t.Fatal("malformed accepted evidence was accepted")
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatal("expected typed integrity failure", err)
	}
}

func TestMigrationNeverInventsMissingAcceptanceEvidence(t *testing.T) {
	path := legacyDatabase(t, false)
	s, err := Open(path)
	if err == nil {
		s.Close()
		t.Fatal("missing acceptance evidence was accepted")
	}
	if !errors.Is(err, ErrIntegrity) {
		t.Fatal("missing acceptance evidence did not fail closed", err)
	}
}

func createStoreOperation(t *testing.T, s *Store, id, ns, name, display string) (model.Operation, model.Plan) {
	t.Helper()
	p := planFor("notebook.stop", ns, name, "")
	p.ID = "plan-" + id
	p.ActorDisplay = display
	p.Prepared.Action.Reason = "maintenance"
	p.Prepared.Changes = []model.Change{{Field: "spec", Before: "a", After: "b"}}
	op := queuedOp(p, id, PrimaryTarget(p.Prepared.Action))
	if _, _, err := s.CreateOperation(context.Background(), op, p, "idempotency-key-"+id); err != nil {
		t.Fatal(err)
	}
	return op, p
}

func backdate(t *testing.T, s *Store, bucket, key string, when time.Time) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE records SET updated=? WHERE bucket=? AND key=?`, when.UnixNano(), bucket, key); err != nil {
		t.Fatal(err)
	}
}

func backdateOperationAudit(t *testing.T, s *Store, operationID string, when time.Time) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE records SET updated=? WHERE bucket='audit' AND json_extract(value,'$.operationID')=?`, when.UnixNano(), operationID); err != nil {
		t.Fatal(err)
	}
}

func queuedCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_compaction`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func backdateCompaction(t *testing.T, s *Store, operationID string, when time.Time) {
	t.Helper()
	if _, err := s.db.Exec(`UPDATE operation_compaction SET updated=? WHERE operation_id=?`, when.UnixNano(), operationID); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionCutoffCompactsCompletedAndRetainsActiveLedger(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	old := time.Now().Add(-100 * 24 * time.Hour).UTC()
	active, activePlan := createStoreOperation(t, s, "active", "alice", "active", "Active Admin")
	backdate(t, s, "operations", active.ID, old)
	backdateOperationAudit(t, s, active.ID, old)

	done, donePlan := createStoreOperation(t, s, "done", "alice", "done", "Done Admin")
	done.State = "applying"
	done.Step = "stop-notebook"
	done.UpdatedAt = time.Now().UTC()
	if err := s.Checkpoint(ctx, done, model.Audit{ID: "done-cp"}); err != nil {
		t.Fatal(err)
	}
	done.State = "succeeded"
	done.Step = "verified"
	done.Error = "old detail to compact"
	done.UpdatedAt = time.Now().UTC()
	if err := s.Finish(ctx, done, model.Audit{ID: "done-final"}); err != nil {
		t.Fatal(err)
	}
	backdate(t, s, "operations", done.ID, old)
	backdateOperationAudit(t, s, done.ID, old)
	backdateCompaction(t, s, done.ID, old)

	result, err := s.CleanupHistory(ctx, HistoryPolicy{RetentionDays: 90}, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// Ordinary and unresolved visible audit beyond the window are both removed:
	// the safety ledger, not the visible audit, is authoritative for active work.
	if result.PurgedExpired < 4 {
		t.Fatal("expired audit, including unresolved presentation rows, was not purged", result)
	}
	if result.RedactedAcceptanceLedger != 1 {
		t.Fatal("completed acceptance detail was not redacted", result)
	}
	var activeAudit int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records WHERE bucket='audit' AND key=?`, active.ID+"-accepted").Scan(&activeAudit); err != nil || activeAudit != 0 {
		t.Fatal("unresolved audit exceeded the visible retention window", activeAudit, err)
	}
	var activeReason string
	var activeChanges []byte
	if err := s.db.QueryRow(`SELECT reason,changes FROM operation_acceptance WHERE operation_id=?`, active.ID).Scan(&activeReason, &activeChanges); err != nil || activeReason != "maintenance" || len(activeChanges) <= 2 {
		t.Fatal("active acceptance ledger was damaged", activeReason, string(activeChanges), err)
	}
	var doneAudit int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records WHERE bucket='audit' AND json_extract(value,'$.operationID')=?`, done.ID).Scan(&doneAudit); err != nil || doneAudit != 0 {
		t.Fatal("completed visible audit retained past cutoff", doneAudit, err)
	}
	var ledger int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_acceptance`).Scan(&ledger); err != nil || ledger != 2 {
		t.Fatal("acceptance ledger rows lost", ledger, err)
	}
	var doneReason string
	var doneChanges []byte
	if err := s.db.QueryRow(`SELECT reason,changes FROM operation_acceptance WHERE operation_id=?`, done.ID).Scan(&doneReason, &doneChanges); err != nil || doneReason != "" || string(doneChanges) != "[]" {
		t.Fatal("completed acceptance reason/diff were not redacted", doneReason, string(doneChanges), err)
	}
	var tombstone model.Operation
	if err := s.Get(ctx, "operations", done.ID, &tombstone); err != nil || !tombstone.HistoryExpired || tombstone.State != "succeeded" || tombstone.Error != "" {
		t.Fatal("completed operation was not compacted", tombstone, err)
	}
	if tombstone.ActorDisplay != "Done Admin" || tombstone.PlanID != donePlan.ID {
		t.Fatal("tombstone lost stable identity/display", tombstone)
	}
	// Claims are retained; an idempotent retry still resolves to the compact
	// tombstone after completed ledger detail has been removed.
	if prior, created, err := s.CreateOperation(ctx, queuedOp(donePlan, "retry-should-not-create", PrimaryTarget(donePlan.Prepared.Action)), donePlan, "idempotency-key-done"); err != nil || created || prior.ID != done.ID || !prior.HistoryExpired {
		t.Fatal("compacted retry was not idempotent", prior, created, err)
	}
	// Stats are taken before the post-retention checkpoint adds a new visible row.
	stats, err := s.HistoryStats(ctx, HistoryPolicy{RetentionDays: 90})
	if err != nil {
		t.Fatal(err)
	}
	if stats.AcceptanceLedgerRecords != 2 || stats.CompactedOperations != 1 || stats.UnresolvedOperations != 1 {
		t.Fatal("stats do not report residual safety state", stats)
	}
	if stats.UnresolvedAuditRecords != 0 {
		t.Fatal("stats still report expired unresolved audit as visible", stats)
	}
	if stats.MaxUnresolved != MaxUnresolvedOperations || stats.RecentRecordLimit != RecentHistoryLimit {
		t.Fatal("stats do not expose exact bounds", stats)
	}
	// The active ledger still supplies the accepted reason/diff to checkpoints
	// even though its visible audit row is gone.
	active.State = "applying"
	active.Step = "after-retention"
	active.UpdatedAt = time.Now().UTC()
	if err := s.Checkpoint(ctx, active, model.Audit{ID: "active-after-retention"}); err != nil {
		t.Fatal("active checkpoint lost acceptance evidence", err)
	}
	var checkpoint model.Audit
	if err := s.Get(ctx, "audit", "active-after-retention", &checkpoint); err != nil || checkpoint.Reason != activePlan.Prepared.Action.Reason || len(checkpoint.Changes) != 1 {
		t.Fatal("checkpoint did not use the active ledger", checkpoint, err)
	}
}

func TestCorruptAcceptanceLedgerFailsClosed(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	op, _ := createStoreOperation(t, s, "corrupt", "alice", "corrupt", "Alice")
	if _, err := s.db.Exec(`UPDATE operation_acceptance SET target='notebook:alice/other' WHERE operation_id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); !errors.Is(err, ErrIntegrity) {
		t.Fatal("conflicting acceptance evidence not detected", err)
	}
	op.State = "applying"
	op.Step = "tampered"
	op.UpdatedAt = time.Now().UTC()
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "should-not-commit"}); !errors.Is(err, ErrIntegrity) {
		t.Fatal("checkpoint accepted conflicting acceptance evidence", err)
	}
	var got model.Operation
	if err := s.Get(ctx, "operations", op.ID, &got); err != nil || got.State != "queued" {
		t.Fatal("operation changed despite integrity failure", got, err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records WHERE bucket='audit' AND key='should-not-commit'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("audit fabricated after integrity failure", n, err)
	}
}

func TestCleanupVerifiesActiveLedgerBeforePruning(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	op, _ := createStoreOperation(t, s, "ledger-gate", "alice", "ledger-gate", "Alice")
	if _, err := s.db.Exec(`DELETE FROM operation_acceptance WHERE operation_id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-100 * 24 * time.Hour).UTC()
	backdateOperationAudit(t, s, op.ID, old)
	result, err := s.CleanupHistory(ctx, HistoryPolicy{RetentionDays: 90}, time.Now().UTC())
	if !errors.Is(err, ErrIntegrity) {
		t.Fatal("cleanup pruned visible evidence without active safety evidence", err)
	}
	if result.PurgedExpired != 0 || result.PurgedUnderPressure != 0 {
		t.Fatal("cleanup deleted records despite missing active ledger", result)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records WHERE bucket='audit' AND key=?`, op.ID+"-accepted").Scan(&n); err != nil || n != 1 {
		t.Fatal("audit evidence was removed before ledger verification", n, err)
	}
}

func TestVisibleAuditDeletionDoesNotBreakCheckpoint(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	op, _ := createStoreOperation(t, s, "visible", "alice", "visible", "Alice")
	if err := s.Delete(ctx, "audit", op.ID+"-accepted"); err != nil {
		t.Fatal(err)
	}
	// Visible presentation history is not the safety evidence; integrity still
	// passes because the immutable ledger is present.
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatal("visible audit deletion broke integrity", err)
	}
	op.State = "applying"
	op.Step = "before-write"
	op.UpdatedAt = time.Now().UTC()
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "after-visible-purge"}); err != nil {
		t.Fatal(err)
	}
	var audit model.Audit
	if err := s.Get(ctx, "audit", "after-visible-purge", &audit); err != nil {
		t.Fatal(err)
	}
	if audit.Reason != "maintenance" || audit.ActorDisplay != "Alice" || len(audit.Changes) != 1 {
		t.Fatal("ledger evidence not used for checkpoint", audit)
	}
}

func TestCleanupIsBoundedAndDoesNotStarveConcurrentStoreWork(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; i++ {
		id := fmt.Sprintf("bounded-%04d", i)
		audit := model.Audit{ID: id, Actor: "issuer|admin", Action: "notebook.stop", Target: "notebook:alice/lab", Result: "succeeded", OperationID: fmt.Sprintf("op-%d", i), Time: old, Reason: strings.Repeat("r", 32)}
		value, err := json.Marshal(audit)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO records(bucket,key,value,updated) VALUES('audit',?,?,?)`, id, value, old.UnixNano()); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	op, _ := createStoreOperation(t, s, "concurrent", "alice", "concurrent", "Alice")
	op.State = "applying"
	op.Step = "before-write"
	op.UpdatedAt = now
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "concurrent-cp"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	readErr := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			var session map[string]any
			if err := s.Get(ctx, "auth.sessions", "missing", &session); err != nil && !errors.Is(err, ErrNotFound) {
				readErr <- err
				return
			}
			var current model.Operation
			if err := s.Get(ctx, "operations", op.ID, &current); err != nil {
				readErr <- err
				return
			}
		}
	}()
	start := time.Now()
	finishErr := make(chan error, 1)
	go func() {
		finished := op
		finished.State = "succeeded"
		finished.Step = "verified"
		finished.UpdatedAt = time.Now().UTC()
		finishErr <- s.Finish(ctx, finished, model.Audit{ID: "concurrent-final"})
	}()
	result, err := s.CleanupHistory(ctx, HistoryPolicy{RetentionDays: 90}, now)
	elapsed := time.Since(start)
	if ferr := <-finishErr; ferr != nil {
		t.Fatal("finish was starved by retention", ferr)
	}
	close(done)
	wg.Wait()
	select {
	case err := <-readErr:
		t.Fatal("concurrent store read failed", err)
	default:
	}
	if err != nil {
		t.Fatal("bounded cleanup failed", err)
	}
	if elapsed > 3*DefaultCleanupTimeout {
		t.Fatalf("cleanup exceeded its bounded budget: %s", elapsed)
	}
	if result.PurgedExpired == 0 || result.PurgedExpired > retentionBatchAudit {
		t.Fatal("cleanup did not process exactly one bounded batch", result)
	}
	var final model.Operation
	if err := s.Get(ctx, "operations", op.ID, &final); err != nil || final.State != "succeeded" {
		t.Fatal("concurrent finish outcome lost", final, err)
	}
}

func TestCleanupCancellationLeavesState(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.Put(ctx, "audit", "old", model.Audit{ID: "old", Actor: "issuer|admin", Action: "notebook.stop", Target: "notebook:alice/lab", Result: "succeeded", Time: now.Add(-100 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE records SET updated=? WHERE bucket='audit' AND key='old'`, now.Add(-100*24*time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	result, err := s.CleanupHistory(canceled, HistoryPolicy{RetentionDays: 90}, now)
	if err == nil {
		t.Fatal("canceled cleanup reported success")
	}
	if result.PurgedExpired != 0 || result.PurgedUnderPressure != 0 || result.CompactedOperations != 0 {
		t.Fatal("canceled cleanup changed state", result)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records WHERE bucket='audit' AND key='old'`).Scan(&n); err != nil || n != 1 {
		t.Fatal("canceled cleanup deleted evidence", n, err)
	}
}

func TestHistoryPolicyCannotWidenVisibleWindow(t *testing.T) {
	p := HistoryPolicy{RetentionDays: 365}.WithDefaults()
	if p.RetentionDays != DefaultRetentionDays {
		t.Fatal("low-level policy widened user-visible retention", p.RetentionDays)
	}
	p = HistoryPolicy{RetentionDays: 7}.WithDefaults()
	if p.RetentionDays != 7 {
		t.Fatal("shorter configured retention was not honored", p.RetentionDays)
	}
}

// Authentication and preview housekeeping must also be batch-bounded so they do
// not hold the single SQLite connection for an unbounded DELETE.
func TestAuthAndPlanCleanupDoNotStarveFinish(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	op, _ := createStoreOperation(t, s, "cleanup-concurrent", "alice", "cleanup", "Alice")
	op.State = "applying"
	op.Step = "before-write"
	op.UpdatedAt = now
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "cleanup-cp"}); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	old := now.Add(-time.Hour)
	stamp := old.Format(time.RFC3339Nano)
	for i := 0; i < 600; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO records(bucket,key,value,updated) VALUES('auth.attempts',?,?,?)`, fmt.Sprintf("expired-auth-%d", i), fmt.Sprintf(`{"expiresAt":%q}`, stamp), now.UnixNano()); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO records(bucket,key,value,updated) VALUES('plans',?,?,?)`, fmt.Sprintf("expired-plan-%d", i), fmt.Sprintf(`{"id":"expired-plan-%d","expiresAt":%q}`, i, stamp), now.UnixNano()); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	errs := make(chan error, 3)
	go func() { errs <- s.CleanupAuth(ctx, now) }()
	go func() { errs <- s.CleanupPlans(ctx, now) }()
	go func() {
		op.State = "succeeded"
		op.Step = "verified"
		op.UpdatedAt = time.Now().UTC()
		errs <- s.Finish(ctx, op, model.Audit{ID: "cleanup-final"})
	}()
	for i := 0; i < 3; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(start); elapsed > 3*DefaultCleanupTimeout {
		t.Fatalf("auth/plan cleanup starved finish: %s", elapsed)
	}
}

// Authentication and preview housekeeping must also be batch-bounded so they do
// not hold the single SQLite connection for an unbounded DELETE.
func TestAuthAndPlanCleanupAreBounded(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	now := time.Now()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 600; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO records(bucket,key,value,updated) VALUES('auth.attempts',?,?,?)`, fmt.Sprintf("bad-auth-%d", i), "not-json", now.UnixNano()); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO records(bucket,key,value,updated) VALUES('plans',?,?,?)`, fmt.Sprintf("bad-plan-%d", i), "not-json", now.UnixNano()); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupAuth(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupPlans(ctx, now); err != nil {
		t.Fatal(err)
	}
	var attempts, plans int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records WHERE bucket='auth.attempts'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records WHERE bucket='plans'`).Scan(&plans); err != nil {
		t.Fatal(err)
	}
	if attempts != 100 || plans != 100 {
		t.Fatal("auth/plan cleanup was not batch-bounded", attempts, plans)
	}
	if err := s.CleanupAuth(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupPlans(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records WHERE bucket IN ('auth.attempts','plans')`).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatal("bounded cleanup did not finish on the next pass", attempts, err)
	}
}

// The primary target lock is part of active-operation integrity, not just the
// converse target-without-claim check.
func TestCheckIntegrityRequiresActivePrimaryTarget(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	op, _ := createStoreOperation(t, s, "primary-lock", "alice", "primary", "Alice")
	if _, err := s.db.Exec(`UPDATE operation_targets SET active=0 WHERE operation_id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); !errors.Is(err, ErrIntegrity) {
		t.Fatal("missing active primary target was not detected", err)
	}
}

func TestCheckIntegrityValidatesKnownSecondaryPVLock(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	plan := planFor("pv.reserve-and-claim", "alice", "claim", "pv-known")
	plan.ActorDisplay = "Alice"
	plan.Prepared.Action.Reason = "maintenance"
	if err := s.Put(ctx, "plans", plan.ID, plan); err != nil {
		t.Fatal(err)
	}
	op := queuedOp(plan, "known-pv-op", PrimaryTarget(plan.Prepared.Action))
	if _, _, err := s.CreateOperation(ctx, op, plan, "known-pv-key"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatal("healthy known PV lock rejected", err)
	}
	if _, err := s.db.Exec(`UPDATE operation_targets SET active=0 WHERE operation_id=? AND target='pv:pv-known'`, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); !errors.Is(err, ErrIntegrity) {
		t.Fatal("missing known secondary PV lock was not detected", err)
	}
}

func TestCheckIntegrityRejectsMalformedActiveOperations(t *testing.T) {
	cases := map[string]string{
		"null":          "null",
		"empty-object":  "{}",
		"unknown-state": `{"id":"corrupt-op","planID":"plan-corrupt","actor":"issuer|admin","target":"notebook:alice/corrupt","action":"notebook.stop","state":"future-state","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`,
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			s := database(t)
			ctx := context.Background()
			op, _ := createStoreOperation(t, s, "corrupt-op", "alice", "corrupt", "Alice")
			if _, err := s.db.Exec(`UPDATE records SET value=? WHERE bucket='operations' AND key=?`, value, op.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.CheckIntegrity(ctx); !errors.Is(err, ErrIntegrity) {
				t.Fatal("malformed active operation was not a typed integrity failure", err)
			}
			var got model.Operation
			if err := s.Get(ctx, "operations", op.ID, &got); !errors.Is(err, ErrIntegrity) {
				t.Fatal("Get exposed malformed active operation", err)
			}
			if _, err := s.OperationForKey(ctx, op.Actor, "idempotency-key-corrupt-op", op.PlanID); !errors.Is(err, ErrIntegrity) {
				t.Fatal("idempotent retry exposed malformed active operation", err)
			}
		})
	}
}

func TestCreateOperationBindsActionToPlan(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	plan := planFor("notebook.stop", "alice", "bound", "")
	op := queuedOp(plan, "bound-op", PrimaryTarget(plan.Prepared.Action))
	op.Action = "pvc.expand"
	if _, _, err := s.CreateOperation(ctx, op, plan, "bound-idempotency-key"); !errors.Is(err, ErrConflict) {
		t.Fatal("store accepted operation action unrelated to the plan", err)
	}
}

func TestAuditPressurePurgesOldestWithinRowAndByteBounds(t *testing.T) {
	now := time.Now().UTC()
	t.Run("rows", func(t *testing.T) {
		s := database(t)
		ctx := context.Background()
		for i := 0; i < 20; i++ {
			id := fmt.Sprintf("audit-%02d", i)
			if err := s.Put(ctx, "audit", id, model.Audit{ID: id, Actor: "issuer|admin", Action: "notebook.stop", Target: "notebook:alice/lab", Result: "succeeded", Time: now.Add(-time.Duration(i) * time.Minute), Reason: strings.Repeat("r", 200)}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE records SET updated=? WHERE bucket='audit' AND key=?`, now.Add(-time.Duration(i)*time.Minute).UnixNano(), id); err != nil {
				t.Fatal(err)
			}
		}
		result, err := s.CleanupHistory(ctx, HistoryPolicy{RetentionDays: 90, MaxAuditRecords: 5}, now)
		if err != nil {
			t.Fatal(err)
		}
		if result.PurgedUnderPressure != 16 {
			t.Fatal("pressure purge not bounded to the oldest rows", result)
		}
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM records WHERE bucket='audit'`).Scan(&count); err != nil || count != 4 {
			t.Fatal("row budget with admission headroom not enforced", count, err)
		}
		var newest int
		if err := s.Get(ctx, "audit", "audit-00", &model.Audit{}); err == nil {
			newest = 1
		}
		if newest != 1 {
			t.Fatal("newest audit row was not retained")
		}
		var oldest model.Audit
		if err := s.Get(ctx, "audit", "audit-19", &oldest); !errors.Is(err, ErrNotFound) {
			t.Fatal("oldest audit row survived pressure", err)
		}
	})
	t.Run("bytes", func(t *testing.T) {
		s := database(t)
		ctx := context.Background()
		for i := 0; i < 10; i++ {
			id := fmt.Sprintf("bytes-%02d", i)
			if err := s.Put(ctx, "audit", id, model.Audit{ID: id, Actor: "issuer|admin", Action: "notebook.stop", Target: "notebook:alice/lab", Result: "succeeded", Time: now.Add(-time.Duration(i) * time.Minute), Reason: strings.Repeat("x", 400)}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(`UPDATE records SET updated=? WHERE bucket='audit' AND key=?`, now.Add(-time.Duration(i)*time.Minute).UnixNano(), id); err != nil {
				t.Fatal(err)
			}
		}
		result, err := s.CleanupHistory(ctx, HistoryPolicy{RetentionDays: 90, MaxAuditBytes: 1200}, now)
		if err != nil {
			t.Fatal(err)
		}
		if result.PurgedUnderPressure == 0 {
			t.Fatal("serialized byte pressure did not purge")
		}
		var bytes int64
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(LENGTH(value)),0) FROM records WHERE bucket='audit'`).Scan(&count, &bytes); err != nil {
			t.Fatal(err)
		}
		if bytes > 1200 {
			t.Fatal("serialized byte budget exceeded", count, bytes)
		}
		if count == 0 {
			t.Fatal("byte pressure removed all history instead of the oldest eligible rows")
		}
	})
}

func TestMalformedAuditFailsClosedWithoutRawLeak(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.Put(ctx, "audit", "valid", model.Audit{ID: "valid", Actor: "issuer|admin", Action: "notebook.stop", Target: "notebook:alice/lab", Result: "succeeded", Time: now.Add(-100 * 24 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO records(bucket,key,value,updated) VALUES('audit','broken','PRIVATE-MALFORMED-BYTES',?)`, now.Add(-100*24*time.Hour).UnixNano()); err != nil {
		t.Fatal(err)
	}
	result, err := s.CleanupHistory(ctx, HistoryPolicy{RetentionDays: 90}, now)
	if !errors.Is(err, ErrIntegrity) {
		t.Fatal("malformed audit did not fail closed", err)
	}
	if strings.Contains(err.Error(), "PRIVATE-MALFORMED") {
		t.Fatal("raw malformed bytes leaked through the error")
	}
	if result.PurgedExpired != 0 || result.PurgedUnderPressure != 0 {
		t.Fatal("cleanup deleted evidence despite malformed history", result)
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records WHERE bucket='audit'`).Scan(&count); err != nil || count != 2 {
		t.Fatal("malformed cleanup silently deleted records", count, err)
	}
	// A malformed row inside the visible window must fail closed, without
	// exposing its bytes through the error. Expired malformed evidence was
	// already rejected by cleanup above and is never user-visible.
	if _, err := s.db.Exec(`INSERT INTO records(bucket,key,value,updated) VALUES('audit','broken-recent','PRIVATE-RECENT-MALFORMED',?)`, now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	_, err = s.RecentAuditWithPolicy(ctx, HistoryPolicy{RetentionDays: 90}, RecentHistoryLimit)
	if !errors.Is(err, ErrIntegrity) {
		t.Fatal("malformed recent audit was silently skipped", err)
	}
	if strings.Contains(err.Error(), "PRIVATE-RECENT-MALFORMED") {
		t.Fatal("raw malformed bytes leaked through the list error")
	}
}

func TestAdmissionBudgetsRejectNewMutations(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	createStoreOperation(t, s, "one", "alice", "one", "Alice")
	createStoreOperation(t, s, "two", "alice", "two", "Alice")
	checkErr := s.CheckAdmission(ctx, HistoryPolicy{MaxOperationRecords: 2})
	if !errors.Is(checkErr, ErrHistoryBudget) {
		t.Fatal("total operation budget not enforced", checkErr)
	}
	var budget *BudgetError
	if !errors.As(checkErr, &budget) {
		t.Fatal("budget error is not typed/actionable")
	}
	// The same budget is enforced atomically inside acceptance, but an existing
	// idempotent retry still resolves.
	newPlan := planFor("notebook.stop", "alice", "three", "")
	newPlan.ID = "plan-three"
	blocked := queuedOp(newPlan, "three", PrimaryTarget(newPlan.Prepared.Action))
	if _, _, err := s.CreateOperationWithPolicy(ctx, blocked, newPlan, "idempotency-key-three", HistoryPolicy{MaxOperationRecords: 2}); !errors.Is(err, ErrHistoryBudget) {
		t.Fatal("atomic acceptance budget not enforced", err)
	}
	onePlan := planFor("notebook.stop", "alice", "one", "")
	onePlan.ID = "plan-one"
	retry := queuedOp(onePlan, "another-id", PrimaryTarget(onePlan.Prepared.Action))
	if prior, created, err := s.CreateOperationWithPolicy(ctx, retry, onePlan, "idempotency-key-one", HistoryPolicy{MaxOperationRecords: 2}); err != nil || created || prior.ID != "one" {
		t.Fatal("idempotent retry rejected at budget", prior, created, err)
	}

	// Audit usage, including already accepted checkpoint rows, also refuses a new
	// mutation; cleanup may later prune older presentation rows.
	op, _ := createStoreOperation(t, s, "protected", "alice", "protected", "Alice")
	op.State = "applying"
	op.UpdatedAt = time.Now().UTC()
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "protected-cp"}); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckAdmission(ctx, HistoryPolicy{MaxAuditRecords: 2, MaxOperationRecords: 100000}); !errors.Is(err, ErrHistoryBudget) {
		t.Fatal("audit usage budget not enforced", err)
	}
}

func TestCleanupOperationsUsesQueueIndexAndProgressesPastCompactedPrefix(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		id := fmt.Sprintf("tombstone-%04d", i)
		op := model.Operation{ID: id, PlanID: "plan-" + id, Actor: "issuer|admin", ActorDisplay: "Alice", HistoryExpired: true, Target: "notebook:alice/old", Action: "notebook.stop", State: "succeeded", CreatedAt: old, UpdatedAt: old}
		value, err := json.Marshal(op)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO records(bucket,key,value,updated) VALUES('operations',?,?,?)`, id, value, old.UnixNano()); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO operation_claims(id,actor,idem,plan_id,target,active) VALUES(?,?,?,?,?,0)`, id, "issuer|admin", "idem-"+id, "plan-"+id, "notebook:alice/old"); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO operation_compaction(operation_id,updated) VALUES(?,?)`, id, old.UnixNano()); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	fresh, _ := createStoreOperation(t, s, "fresh-compaction", "alice", "fresh", "Alice")
	fresh.State = "applying"
	fresh.Step = "before-write"
	fresh.UpdatedAt = now
	if err := s.Checkpoint(ctx, fresh, model.Audit{ID: "fresh-cp"}); err != nil {
		t.Fatal(err)
	}
	fresh.State = "succeeded"
	fresh.Step = "verified"
	fresh.UpdatedAt = now
	if err := s.Finish(ctx, fresh, model.Audit{ID: "fresh-final"}); err != nil {
		t.Fatal(err)
	}
	backdate(t, s, "operations", fresh.ID, old.Add(time.Nanosecond))
	backdateCompaction(t, s, fresh.ID, old.Add(time.Nanosecond))

	planRows, err := s.db.Query(`EXPLAIN QUERY PLAN SELECT q.operation_id,q.updated,c.active,r.key,r.value FROM operation_compaction q LEFT JOIN operation_claims c ON c.id=q.operation_id LEFT JOIN records r ON r.bucket='operations' AND r.key=q.operation_id WHERE q.updated<=? ORDER BY q.updated,q.operation_id LIMIT ?`, old.UnixNano(), retentionBatchOperations)
	if err != nil {
		t.Fatal(err)
	}
	var plan strings.Builder
	for planRows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := planRows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			planRows.Close()
			t.Fatal(err)
		}
		plan.WriteString(detail + "\n")
	}
	if err := planRows.Err(); err != nil {
		planRows.Close()
		t.Fatal(err)
	}
	planRows.Close()
	if !strings.Contains(plan.String(), "operation_compaction_updated") {
		t.Fatalf("compaction queue is not using its time index: %s", plan.String())
	}
	// Each bounded pass removes at most one batch from the prefix; the freshly
	// completed operation eventually becomes eligible without an unbounded scan.
	// Allow enough passes for the smaller progress-preserving batch under
	// race-detector load.
	for i := 0; i < 30 && queuedCount(t, s) > 0; i++ {
		if _, err := s.CleanupHistory(ctx, HistoryPolicy{RetentionDays: 90}, now); err != nil {
			t.Fatal(err)
		}
	}
	if queued := queuedCount(t, s); queued != 0 {
		t.Fatal("compaction queue did not drain in bounded batches", queued)
	}
	var got model.Operation
	if err := s.Get(ctx, "operations", fresh.ID, &got); err != nil || !got.HistoryExpired {
		t.Fatal("new eligible operation did not progress past the compacted prefix", got, err)
	}
}

func TestAuditPruneCommitsIndependentlyOfCompactionFailure(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)
	if err := s.Put(ctx, "audit", "old-audit", model.Audit{ID: "old-audit", Actor: "issuer|admin", Action: "notebook.stop", Target: "notebook:alice/old", Result: "succeeded", Time: old}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE records SET updated=? WHERE bucket='audit' AND key='old-audit'`, old.UnixNano()); err != nil {
		t.Fatal(err)
	}
	// A malformed inactive operation with an old pending marker must not roll
	// back the audit batch that already committed.
	if _, err := s.db.Exec(`INSERT INTO records(bucket,key,value,updated) VALUES('operations','broken-inactive','{not-json',?)`, old.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO operation_claims(id,actor,idem,plan_id,target,active) VALUES('broken-inactive','issuer|admin','broken-idem','broken-plan','notebook:alice/broken',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO operation_compaction(operation_id,updated) VALUES('broken-inactive',?)`, old.UnixNano()); err != nil {
		t.Fatal(err)
	}
	result, err := s.CleanupHistory(ctx, HistoryPolicy{RetentionDays: 90}, now)
	if !errors.Is(err, ErrIntegrity) {
		t.Fatal("malformed inactive operation did not report typed integrity", err)
	}
	if result.PurgedExpired != 1 {
		t.Fatal("audit batch did not commit independently", result)
	}
	var audit int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records WHERE bucket='audit' AND key='old-audit'`).Scan(&audit); err != nil || audit != 0 {
		t.Fatal("audit progress was rolled back by compaction failure", audit, err)
	}
}

func TestInactiveNeedsReviewEvidenceIsNeverRedacted(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	op, _ := createStoreOperation(t, s, "review-protected", "alice", "review", "Alice")
	op.State = "needs-review"
	op.Step = "uncertain"
	op.UpdatedAt = time.Now().UTC()
	value, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE records SET value=? WHERE bucket='operations' AND key=?`, value, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE operation_claims SET active=0 WHERE id=?`, op.ID); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-100 * 24 * time.Hour).UTC()
	if _, err := s.db.Exec(`INSERT INTO operation_compaction(operation_id,updated) VALUES(?,?)`, op.ID, old.UnixNano()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CleanupHistory(ctx, HistoryPolicy{RetentionDays: 90}, time.Now().UTC()); !errors.Is(err, ErrIntegrity) {
		t.Fatal("inactive needs-review operation was treated as compactable", err)
	}
	var reason string
	var changes []byte
	if err := s.db.QueryRow(`SELECT reason,changes FROM operation_acceptance WHERE operation_id=?`, op.ID).Scan(&reason, &changes); err != nil || reason != "maintenance" || string(changes) == "[]" {
		t.Fatal("needs-review evidence was redacted", reason, string(changes), err)
	}
}

func TestCheckpointsContinueAndAdmissionRecoversAfterPruning(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	now := time.Now().UTC()
	op, _ := createStoreOperation(t, s, "checkpoint", "alice", "checkpoint", "Alice")
	for i := 0; i < 5; i++ {
		if err := s.Put(ctx, "audit", fmt.Sprintf("budget-%d", i), model.Audit{ID: fmt.Sprintf("budget-%d", i), Actor: "issuer|admin", Action: "notebook.stop", Target: "notebook:alice/budget", Result: "succeeded", Time: now.Add(-time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.db.Exec(`UPDATE records SET updated=? WHERE bucket='audit' AND key=?`, now.Add(-time.Duration(i)*time.Minute).UnixNano(), fmt.Sprintf("budget-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	// Checkpoints are never blocked by presentation-history pressure.
	op.State = "applying"
	op.Step = "still-progresses"
	op.UpdatedAt = now
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "checkpoint-at-budget"}); err != nil {
		t.Fatal("accepted checkpoint was blocked by audit pressure", err)
	}
	// New admission is rejected until bounded cleanup makes room.
	plan := planFor("notebook.stop", "alice", "blocked", "")
	plan.ID = "plan-blocked"
	blocked := queuedOp(plan, "blocked", PrimaryTarget(plan.Prepared.Action))
	if _, _, err := s.CreateOperationWithPolicy(ctx, blocked, plan, "blocked-key", HistoryPolicy{MaxAuditRecords: 4}); !errors.Is(err, ErrHistoryBudget) {
		t.Fatal("admission over audit budget was accepted", err)
	}
	if _, err := s.CleanupHistory(ctx, HistoryPolicy{RetentionDays: 90, MaxAuditRecords: 4}, now); err != nil {
		t.Fatal(err)
	}
	// The janitor is asynchronous; one bounded pass may need more batches. Run
	// bounded passes until the row count fits, exactly as periodic housekeeping.
	for i := 0; i < 8; i++ {
		var count int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM records WHERE bucket='audit'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count <= 4 {
			break
		}
		if _, err := s.CleanupHistory(ctx, HistoryPolicy{RetentionDays: 90, MaxAuditRecords: 4}, now); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := s.CreateOperationWithPolicy(ctx, blocked, plan, "blocked-key", HistoryPolicy{MaxAuditRecords: 4}); err != nil {
		t.Fatal("admission did not recover after bounded pruning", err)
	}
}

func TestAtomicAdmissionIncludesAuditUsageAndNewRowSize(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	createStoreOperation(t, s, "existing", "alice", "existing", "Alice")
	var bytes int64
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(LENGTH(value)),0) FROM records WHERE bucket='audit'`).Scan(&bytes); err != nil {
		t.Fatal(err)
	}
	plan := planFor("notebook.stop", "alice", "next", "")
	plan.ID = "plan-next"
	op := queuedOp(plan, "next", PrimaryTarget(plan.Prepared.Action))
	if _, _, err := s.CreateOperationWithPolicy(ctx, op, plan, "idempotency-key-next", HistoryPolicy{MaxAuditBytes: bytes + 1}); !errors.Is(err, ErrHistoryBudget) {
		t.Fatal("serialized size of the new audit row was not included in admission", err)
	}
	var claims int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_claims WHERE id='next'`).Scan(&claims); err != nil || claims != 0 {
		t.Fatal("rejected admission left partial state", claims, err)
	}
}

func TestUnresolvedAdmissionLimit(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxUnresolvedOperations; i++ {
		if _, err := tx.ExecContext(ctx, `INSERT INTO operation_claims(id,actor,idem,plan_id,target,active) VALUES(?,?,?,?,?,1)`, fmt.Sprintf("u-%d", i), "issuer|admin", fmt.Sprintf("key-%d", i), fmt.Sprintf("plan-%d", i), fmt.Sprintf("pvc:alice/%d", i)); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckAdmission(ctx, HistoryPolicy{}); !errors.Is(err, ErrHistoryBudget) {
		t.Fatal("unresolved operation limit not enforced", err)
	}
}

func TestLegacyExcessUnresolvedClaimsFailClosedAndListBound(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := 0; i <= MaxUnresolvedOperations; i++ {
		id := fmt.Sprintf("legacy-unresolved-%d", i)
		op := model.Operation{ID: id, PlanID: fmt.Sprintf("plan-%d", i), Actor: "issuer|admin", Target: fmt.Sprintf("notebook:alice/n%d", i), Action: "notebook.stop", State: "applying", CreatedAt: now, UpdatedAt: now}
		value, err := json.Marshal(op)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO records(bucket,key,value,updated) VALUES('operations',?,?,?)`, id, value, now.UnixNano()); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO operation_claims(id,actor,idem,plan_id,target,active) VALUES(?,?,?,?,?,1)`, id, op.Actor, fmt.Sprintf("key-%d", i), op.PlanID, op.Target); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); !errors.Is(err, ErrIntegrity) {
		t.Fatal("legacy excess unresolved claims did not fail closed at startup", err)
	}
	if _, err := s.RecentOperations(ctx, RecentHistoryLimit); !errors.Is(err, ErrHistoryBudget) {
		t.Fatal("unresolved list read was not hard bounded", err)
	}
}

func TestRecentListsIncludeOldUnresolvedButAuditStaysWindowed(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	active, _ := createStoreOperation(t, s, "old-active", "alice", "old-active", "Alice")
	old := time.Now().Add(-365 * 24 * time.Hour)
	backdate(t, s, "operations", active.ID, old)
	backdateOperationAudit(t, s, active.ID, old)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := 0; i < RecentHistoryLimit; i++ {
		id := fmt.Sprintf("recent-%04d", i)
		op := model.Operation{ID: id, PlanID: "plan-" + id, Actor: "issuer|admin", Target: "notebook:alice/recent", Action: "notebook.stop", State: "succeeded", CreatedAt: now, UpdatedAt: now}
		value, err := json.Marshal(op)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO records(bucket,key,value,updated) VALUES('operations',?,?,?)`, id, value, now.UnixNano()); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
		audit := model.Audit{ID: id + "-a", Actor: "issuer|admin", Action: "notebook.stop", Target: "notebook:alice/recent", Result: "succeeded", OperationID: id, Time: now}
		auditValue, err := json.Marshal(audit)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO records(bucket,key,value,updated) VALUES('audit',?,?,?)`, audit.ID, auditValue, now.UnixNano()); err != nil {
			tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ops, err := s.RecentOperations(ctx, RecentHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != RecentHistoryLimit+1 {
		t.Fatal("unresolved operation disappeared behind the recent window", len(ops))
	}
	found := false
	for _, op := range ops {
		if op.ID == active.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("old unresolved operation missing")
	}
	// Audit is hard bounded at 1000 and old unresolved checkpoints are not
	// re-appended: the ledger, not the UI history, preserves safety evidence.
	audit, err := s.RecentAuditWithPolicy(ctx, HistoryPolicy{RetentionDays: 90}, RecentHistoryLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(audit) != RecentHistoryLimit {
		t.Fatal("audit window is not hard bounded", len(audit))
	}
	for _, a := range audit {
		if a.ID == active.ID+"-accepted" {
			t.Fatal("expired unresolved audit leaked past the read-time cutoff")
		}
	}
}

func TestPodRestartTargetsAndValidation(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	plan := model.Plan{ID: "restart-plan", Actor: "issuer|admin", ActorDisplay: "Alice", Prepared: model.Prepared{Action: model.Action{Kind: "pod.restart", Namespace: "alice", Name: "lab-0"}, ParentNotebook: "lab"}}
	if got := PrimaryTarget(plan.Prepared.Action); got != "pod:alice/lab-0" {
		t.Fatal("restart primary target", got)
	}
	targets := Targets(plan)
	if len(targets) != 2 || targets[0] != "pod:alice/lab-0" || targets[1] != "notebook:alice/lab" {
		t.Fatal("restart secondary lock", targets)
	}
	op := queuedOp(plan, "restart-op", "pod:alice/lab-0")
	if _, created, err := s.CreateOperation(ctx, op, plan, "restart-idempotency-key"); err != nil || !created {
		t.Fatal(err)
	}
	var locks int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_targets WHERE operation_id='restart-op' AND active=1`).Scan(&locks); err != nil || locks != 2 {
		t.Fatal("restart locks", locks, err)
	}
	for _, parent := range []string{"", "../lab", "alice/lab", "Lab"} {
		bad := plan
		bad.ID = "bad-" + parent
		bad.Prepared.ParentNotebook = parent
		badOp := queuedOp(bad, "bad-op-"+parent, "pod:alice/lab-0")
		if _, _, err := s.CreateOperation(ctx, badOp, bad, "bad-key-"+parent); !errors.Is(err, ErrInvalidTarget) {
			t.Fatal("invalid parent notebook accepted", parent, err)
		}
	}
	// Other actions never gain a parent lock merely from carrying the field.
	other := planFor("notebook.stop", "alice", "lab", "")
	other.Prepared.ParentNotebook = "lab"
	if targets := Targets(other); len(targets) != 1 || targets[0] != "notebook:alice/lab" {
		t.Fatal("unexpected parent lock", targets)
	}
	for _, kind := range []string{"node.cordon", "node.uncordon"} {
		nodePlan := model.Plan{ID: "node-" + kind, Actor: "issuer|admin", ActorDisplay: "Alice", Prepared: model.Prepared{Action: model.Action{Kind: kind, Name: "worker-1", Reason: "maintenance"}}}
		if got := PrimaryTarget(nodePlan.Prepared.Action); got != "node:worker-1" {
			t.Fatal("node primary target", kind, got)
		}
		if targets := Targets(nodePlan); len(targets) != 1 || targets[0] != "node:worker-1" {
			t.Fatal("node targets", kind, targets)
		}
	}
}

func TestConcurrentFinishAndRetention(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	op, _ := createStoreOperation(t, s, "race", "alice", "race", "Alice")
	op.State = "applying"
	op.Step = "before-write"
	op.UpdatedAt = time.Now().UTC()
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "race-cp"}); err != nil {
		t.Fatal(err)
	}
	backdate(t, s, "operations", op.ID, time.Now().Add(-100*24*time.Hour))

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				if _, err := s.CleanupHistory(ctx, HistoryPolicy{RetentionDays: 90, MaxAuditRecords: 5, MaxAuditBytes: 1 << 20}, time.Now()); err != nil {
					errs <- err
					return
				}
				if _, err := s.RecentOperations(ctx, RecentHistoryLimit); err != nil {
					errs <- err
					return
				}
				if _, err := s.RecentAudit(ctx, RecentHistoryLimit); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	op.State = "succeeded"
	op.Step = "verified"
	op.UpdatedAt = time.Now().UTC()
	if err := s.Finish(ctx, op, model.Audit{ID: "race-final"}); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var got model.Operation
	if err := s.Get(ctx, "operations", op.ID, &got); err != nil || got.State != "succeeded" {
		t.Fatal("finish lost in concurrent prune", got, err)
	}
	var active int
	if err := s.db.QueryRow(`SELECT active FROM operation_claims WHERE id=?`, op.ID).Scan(&active); err != nil || active != 0 {
		t.Fatal("completed claim not released", active, err)
	}
	var ledger int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_acceptance WHERE operation_id=?`, op.ID).Scan(&ledger); err != nil || ledger != 1 {
		t.Fatal("acceptance evidence lost in concurrent prune", ledger, err)
	}
}
