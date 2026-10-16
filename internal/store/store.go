// Package store maintains durable application records, not cluster inventory.
// WAL requires local storage; DELETE journaling also supports configured BeeGFS.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"kubeflow-admin/internal/model"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("record not found")
var ErrConflict = errors.New("operation conflicts with an existing request or locked target")

// ErrLocked reports that another process already owns the database files.
var ErrLocked = errors.New("database is already owned by another application")

// ErrIntegrity reports durable operation state that cannot be reconciled
// automatically without either releasing a lock without inspecting the cluster
// or inventing audit history. Callers must fail closed and stop.
var ErrIntegrity = errors.New("operation storage integrity error")

// IntegrityError is a safe, actionable description of inconsistent durable
// state. It intentionally carries only a short structural reason, never raw
// record contents, tokens or user data.
type IntegrityError struct {
	Reason string
}

func (e *IntegrityError) Error() string {
	return ErrIntegrity.Error() + ": " + e.Reason + "; stop writes, inspect the cluster and database, and reconcile with an operator before restarting"
}
func (e *IntegrityError) Unwrap() error { return ErrIntegrity }

func integrity(reason string) error { return &IntegrityError{Reason: reason} }

type Store struct {
	db         *sql.DB
	lock       *os.File
	mu         sync.Mutex
	worker     bool
	unverified bool
	closeOnce  sync.Once
	closeErr   error
}

// Open acquires a lifetime, nonblocking exclusive application lock. SQLite's
// WAL locks alone do not prevent two processes from recovering each other's
// in-flight operations. A second application must not open the same database.
func Open(path string) (*Store, error) {
	return OpenWithJournal(path, "wal", false)
}

// OpenWithJournal supports rollback journaling for BeeGFS clients configured
// with global file locks and remote fsync. allowUnverifiedFilesystem must be
// set only after an operator verifies locking and fsync semantics; it unlocks
// storage the application cannot prove local. Only one process may own the DB.
func OpenWithJournal(path, journal string, allowUnverifiedFilesystem bool) (*Store, error) {
	if journal == "" {
		journal = "wal"
	}
	if journal != "wal" && journal != "delete" {
		return nil, errors.New("database journalMode must be wal or delete")
	}
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, errors.New("a durable database file is required")
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	unverified, err := databaseFilesystem(filepath.Dir(path), journal, allowUnverifiedFilesystem)
	if err != nil {
		return nil, err
	}
	lock, err := privateFile(path + ".lock")
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, ErrLocked
	}
	fail := func(err error) (*Store, error) { lock.Close(); return nil, err }
	f, err := privateFile(path)
	if err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		return fail(err)
	}
	// A pre-existing WAL/SHM/journal sidecar is opened by SQLite with the same
	// initial flags as the main file, so a planted symlink or a sidecar that is
	// not private could redirect or expose writes. SQLite creates new sidecars
	// with the main file's 0600 permissions (verified by the permissive-umask
	// regression test), so only pre-existing files need rejection here. The
	// library must never change the process-global umask; main() sets 0077.
	if err := verifyExistingSidecars(path); err != nil {
		return fail(err)
	}
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	// Pragmas are applied to every new connection, including replacement
	// connections after cancellation. Immediate transactions avoid lock upgrades.
	// EXTRA also syncs the directory after unlinking a rollback journal.
	synchronous := "FULL"
	if journal == "delete" {
		synchronous = "EXTRA"
	}
	for _, p := range []string{"busy_timeout(5000)", "synchronous(" + synchronous + ")", "foreign_keys(ON)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return fail(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db, lock: lock, unverified: unverified}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode="+journal).Scan(&mode); err != nil || mode != journal {
		s.Close()
		return nil, errors.New("cannot enable requested SQLite journal mode: " + journal)
	}
	for _, query := range []string{
		`CREATE TABLE IF NOT EXISTS records(bucket TEXT NOT NULL,key TEXT NOT NULL,value BLOB NOT NULL,updated INTEGER NOT NULL,PRIMARY KEY(bucket,key))`,
		`CREATE TABLE IF NOT EXISTS operation_claims(id TEXT PRIMARY KEY,actor TEXT NOT NULL,idem TEXT NOT NULL,plan_id TEXT NOT NULL,target TEXT NOT NULL,active INTEGER NOT NULL DEFAULT 1,UNIQUE(actor,idem),UNIQUE(plan_id))`,
		`CREATE UNIQUE INDEX IF NOT EXISTS active_target ON operation_claims(target) WHERE active=1`,
		`CREATE TABLE IF NOT EXISTS operation_targets(operation_id TEXT NOT NULL,target TEXT NOT NULL,active INTEGER NOT NULL DEFAULT 1,PRIMARY KEY(operation_id,target))`,
		`CREATE UNIQUE INDEX IF NOT EXISTS locked_target ON operation_targets(target) WHERE active=1`,
		`INSERT OR IGNORE INTO operation_targets(operation_id,target,active) SELECT id,target,active FROM operation_claims`,
		`PRAGMA user_version=2`,
	} {
		if _, err := db.ExecContext(ctx, query); err != nil {
			s.Close()
			return nil, err
		}
	}
	return s, nil
}

// OnUnverifiedFilesystem reports whether the database sits on storage whose
// cross-node locking and durability the application cannot verify (for
// example BeeGFS). Operators should alert on this condition.
func (s *Store) OnUnverifiedFilesystem() bool { return s.unverified }

func privateFile(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, errors.New("database and lock files must be private regular files (0600)")
	}
	return f, nil
}

// sidecarSuffixes are the SQLite journal/WAL shared-memory files that live next
// to the main database. SQLite creates them with the main database's mode, so
// they inherit 0600; only files that already exist before SQLite opens the
// database can have been planted by another user.
var sidecarSuffixes = []string{"-wal", "-shm", "-journal"}

func verifyExistingSidecars(path string) error {
	for _, suffix := range sidecarSuffixes {
		info, err := os.Lstat(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("SQLite %s file must be a private regular file created by the application (0600)", suffix)
		}
	}
	return nil
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
		if s.lock != nil {
			if err := s.lock.Close(); s.closeErr == nil {
				s.closeErr = err
			}
		}
	})
	return s.closeErr
}
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// AcquireWorker prevents a second manager from recovering a live manager's
// operations even when both share the same Store in one process.
func (s *Store) AcquireWorker() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.worker {
		return false
	}
	s.worker = true
	return true
}
func (s *Store) ReleaseWorker() { s.mu.Lock(); s.worker = false; s.mu.Unlock() }

func put(ctx context.Context, e interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, bucket, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = e.ExecContext(ctx, `INSERT INTO records(bucket,key,value,updated) VALUES(?,?,?,?) ON CONFLICT(bucket,key) DO UPDATE SET value=excluded.value,updated=excluded.updated`, bucket, key, b, time.Now().UnixNano())
	return err
}
func (s *Store) Put(ctx context.Context, bucket, key string, value any) error {
	return put(ctx, s.db, bucket, key, value)
}
func insertAudit(ctx context.Context, tx *sql.Tx, audit model.Audit) error {
	b, err := json.Marshal(audit)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO records(bucket,key,value,updated) VALUES('audit',?,?,?)`, audit.ID, b, time.Now().UnixNano())
	return err
}
func (s *Store) Get(ctx context.Context, bucket, key string, out any) error {
	var b []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM records WHERE bucket=? AND key=?`, bucket, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
func (s *Store) Delete(ctx context.Context, bucket, key string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM records WHERE bucket=? AND key=?`, bucket, key)
	return err
}
func (s *Store) List(ctx context.Context, bucket string, limit int) ([]json.RawMessage, error) {
	if limit < 1 || limit > 10000 {
		limit = 1000
	}
	rows, err := s.db.QueryContext(ctx, `SELECT value FROM records WHERE bucket=? ORDER BY updated DESC,key DESC LIMIT ?`, bucket, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var b []byte
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		out = append(out, json.RawMessage(b))
	}
	return out, rows.Err()
}
func (s *Store) Take(ctx context.Context, bucket, key string, out any) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var b []byte
	err = tx.QueryRowContext(ctx, `SELECT value FROM records WHERE bucket=? AND key=?`, bucket, key).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err = json.Unmarshal(b, out); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM records WHERE bucket=? AND key=?`, bucket, key); err != nil {
		return err
	}
	return tx.Commit()
}

// CleanupAuth removes only expired/invalid authentication records. Malformed
// rows are removed first because json_extract raises on invalid JSON and would
// otherwise abort the whole delete, making every authenticated request fail
// closed forever. Audit and operation records are untouched.
func (s *Store) CleanupAuth(ctx context.Context, now time.Time) error {
	stamp := now.UTC().Format(time.RFC3339Nano)
	return s.cleanupRecords(ctx, []string{"auth.attempts", "auth.sessions"},
		cleanupDelete{
			query: `DELETE FROM records WHERE bucket='auth.attempts' AND json_valid(value)=1 AND COALESCE(julianday(json_extract(value,'$.expiresAt')),0)<=julianday(?)`,
			args:  []any{stamp},
		},
		cleanupDelete{
			query: `DELETE FROM records WHERE bucket='auth.sessions' AND json_valid(value)=1 AND (COALESCE(julianday(json_extract(value,'$.identity.expiresAt')),0)<=julianday(?) OR COALESCE(julianday(json_extract(value,'$.identity.authenticatedAt')),0)+15.0/1440<=julianday(?))`,
			args:  []any{stamp, stamp},
		},
	)
}

// CleanupPlans removes expired or malformed preview plans. Plans are ephemeral
// server-owned previews; operations, audit history and accepted idempotency
// claims are never touched. Accepted retries resolve through operation_claims
// (OperationForKey), so deleting an expired plan cannot lose an accepted task.
func (s *Store) CleanupPlans(ctx context.Context, now time.Time) error {
	stamp := now.UTC().Format(time.RFC3339Nano)
	return s.cleanupRecords(ctx, []string{"plans"},
		cleanupDelete{
			query: `DELETE FROM records WHERE bucket='plans' AND json_valid(value)=1 AND COALESCE(julianday(json_extract(value,'$.expiresAt')),0)<=julianday(?)`,
			args:  []any{stamp},
		},
	)
}

type cleanupDelete struct {
	query string
	args  []any
}

// cleanupRecords deletes malformed rows (json_valid=0) in buckets and then runs
// the bounded expiry deletes, all in one transaction. A malformed row can never
// abort cleanup of the remaining records.
func (s *Store) cleanupRecords(ctx context.Context, buckets []string, deletes ...cleanupDelete) error {
	if len(buckets) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(buckets)), ",")
	args := make([]any, 0, len(buckets))
	for _, b := range buckets {
		args = append(args, b)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `DELETE FROM records WHERE bucket IN (`+placeholders+`) AND json_valid(value)=0`, args...); err != nil {
		return err
	}
	for _, d := range deletes {
		if _, err = tx.ExecContext(ctx, d.query, d.args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PrimaryTarget is the canonical lock name derived from the server-owned action.
// It is the only primary target a caller may claim; accepting an arbitrary
// caller primary would let a plan lock unrelated PVC/PV/object targets.
func PrimaryTarget(a model.Action) string {
	switch {
	case strings.HasPrefix(a.Kind, "notebook."):
		return "notebook:" + a.Namespace + "/" + a.Name
	case strings.HasPrefix(a.Kind, "profile."):
		return "profile:" + a.Name
	default:
		return "pvc:" + a.Namespace + "/" + a.Name
	}
}

// Targets locks the canonical primary plus its selected PV; using only PVName
// would allow two queued operations to mutate the same PVC using different PV
// reservations.
func Targets(p model.Plan) []string {
	a := p.Prepared.Action
	out := []string{PrimaryTarget(a)}
	add := func(v string) {
		for _, old := range out {
			if old == v {
				return
			}
		}
		out = append(out, v)
	}
	// PVName is meaningful only for these two actions. Other adapters ignore it;
	// carrying an irrelevant field in an otherwise valid action must not acquire
	// a lock on an unrelated PV (especially a needs-review lock).
	if (a.Kind == "storage.create.static" || a.Kind == "pv.reserve-and-claim") && a.PVName != "" {
		add("pv:" + a.PVName)
	}
	return out
}

// CreateOperation atomically claims actor+idempotency key, immutable plan, all
// affected targets, and writes the accepted operation plus audit record.
func (s *Store) CreateOperation(ctx context.Context, op model.Operation, plan model.Plan, key string) (model.Operation, bool, error) {
	if op.ID == "" || plan.ID == "" || op.PlanID != plan.ID || op.Actor == "" || op.Actor != plan.Actor || op.Target == "" || key == "" || op.State != "queued" {
		return model.Operation{}, false, ErrConflict
	}
	// The primary target must be exactly the canonical target of the server-owned
	// plan action. Derived targets (for example a reserved PV) never substitute an
	// arbitrary caller-supplied primary.
	if op.Target != PrimaryTarget(plan.Prepared.Action) {
		return model.Operation{}, false, ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return op, false, err
	}
	defer tx.Rollback()
	var priorID, priorPlan string
	err = tx.QueryRowContext(ctx, `SELECT id,plan_id FROM operation_claims WHERE actor=? AND idem=?`, op.Actor, key).Scan(&priorID, &priorPlan)
	if err == nil {
		if priorPlan != plan.ID {
			return op, false, ErrConflict
		}
		var b []byte
		if err = tx.QueryRowContext(ctx, `SELECT value FROM records WHERE bucket='operations' AND key=?`, priorID).Scan(&b); err != nil {
			return op, false, err
		}
		err = json.Unmarshal(b, &op)
		return op, false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return op, false, err
	}
	// Check conflicts separately so disk/I/O/constraint corruption errors are not
	// mislabeled as a harmless competing request.
	var exists int
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM operation_claims WHERE plan_id=? OR id=? OR (target=? AND active=1) LIMIT 1`, plan.ID, op.ID, op.Target).Scan(&exists)
	if err == nil {
		return op, false, ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return op, false, err
	}
	for _, target := range Targets(plan) {
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM operation_targets WHERE target=? AND active=1`, target).Scan(&exists)
		if err == nil {
			return op, false, ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return op, false, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO operation_claims(id,actor,idem,plan_id,target) VALUES(?,?,?,?,?)`, op.ID, op.Actor, key, plan.ID, op.Target); err != nil {
		return op, false, err
	}
	for _, target := range Targets(plan) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO operation_targets(operation_id,target) VALUES(?,?)`, op.ID, target); err != nil {
			return op, false, err
		}
	}
	if err = put(ctx, tx, "operations", op.ID, op); err != nil {
		return op, false, err
	}
	audit := model.Audit{ID: op.ID + "-accepted", Actor: op.Actor, Action: op.Action, Target: op.Target, Result: "queued", OperationID: op.ID, Reason: plan.Prepared.Action.Reason, Changes: plan.Prepared.Changes, Time: op.CreatedAt}
	if err = insertAudit(ctx, tx, audit); err != nil {
		return op, false, err
	}
	if err = tx.Commit(); err != nil {
		return op, false, err
	}
	return op, true, nil
}

func (s *Store) Checkpoint(ctx context.Context, op model.Operation, audit model.Audit) error {
	return s.updateOperation(ctx, op, audit, false)
}
func (s *Store) Finish(ctx context.Context, op model.Operation, audit model.Audit) error {
	return s.updateOperation(ctx, op, audit, true)
}
func (s *Store) updateOperation(ctx context.Context, op model.Operation, audit model.Audit, finish bool) error {
	if audit.ID == "" {
		return ErrConflict
	}
	if finish {
		if op.State != "succeeded" && op.State != "failed" && op.State != "needs-review" {
			return ErrConflict
		}
	} else if op.State != "applying" {
		return ErrConflict
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var actor, planID, target string
	var active int
	err = tx.QueryRowContext(ctx, `SELECT actor,plan_id,target,active FROM operation_claims WHERE id=?`, op.ID).Scan(&actor, &planID, &target, &active)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if active != 1 || actor != op.Actor || planID != op.PlanID || target != op.Target {
		return ErrConflict
	}
	var currentBytes []byte
	if err := tx.QueryRowContext(ctx, `SELECT value FROM records WHERE bucket='operations' AND key=?`, op.ID).Scan(&currentBytes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return integrity("active operation claim has no operation record")
		}
		return err
	}
	var current model.Operation
	if err := json.Unmarshal(currentBytes, &current); err != nil {
		return integrity("operation record is not readable JSON")
	}
	// Only real lifecycle transitions are accepted. In particular a fresh
	// queued operation cannot be reported succeeded without an applying
	// checkpoint, while shutdown (queued->failed) and crash recovery
	// (queued/applying->needs-review) remain valid.
	if !allowedTransition(current.State, op.State, finish) {
		return ErrConflict
	}
	// Identity/action metadata belongs to the original claim, not caller input.
	op.Action = current.Action
	op.CreatedAt = current.CreatedAt
	audit.Actor = op.Actor
	audit.Action = op.Action
	audit.Target = op.Target
	audit.Result = op.State
	audit.OperationID = op.ID
	audit.Time = op.UpdatedAt
	var acceptedBytes []byte
	if err := tx.QueryRowContext(ctx, `SELECT value FROM records WHERE bucket='audit' AND key=?`, op.ID+"-accepted").Scan(&acceptedBytes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// Never fabricate the immutable accepted reason/diff. Fail closed so an
			// operator can restore or reconcile the exact history.
			return integrity("operation " + op.ID + " is missing its immutable accepted audit record")
		}
		return err
	}
	var accepted model.Audit
	if err := json.Unmarshal(acceptedBytes, &accepted); err != nil {
		return integrity("accepted audit record is not readable JSON")
	}
	audit.Reason = accepted.Reason
	audit.Changes = accepted.Changes
	if err = put(ctx, tx, "operations", op.ID, op); err != nil {
		return err
	}
	if err = insertAudit(ctx, tx, audit); err != nil {
		return err
	}
	// Uncertain outcomes retain every target lock. An administrator must inspect
	// and explicitly reconcile them; a new request is not evidence of recovery.
	if finish && op.State != "needs-review" {
		if _, err = tx.ExecContext(ctx, `UPDATE operation_claims SET active=0 WHERE id=?`, op.ID); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `UPDATE operation_targets SET active=0 WHERE operation_id=?`, op.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// allowedTransition reports whether a store update may move an operation from
// its durable state to the requested state.
func allowedTransition(from, to string, finish bool) bool {
	if !finish {
		// Checkpoints only advance a non-terminal operation to applying; repeats
		// let a long operation record further verified steps.
		return to == "applying" && (from == "queued" || from == "applying")
	}
	switch to {
	case "succeeded":
		// Success requires evidence that execution started and was checkpointed.
		return from == "applying"
	case "failed":
		// Shutdown may account for accepted-but-unstarted work.
		return from == "queued" || from == "applying"
	case "needs-review":
		// Crash recovery and uncertain execution retain locks for reconciliation.
		return from == "queued" || from == "applying"
	}
	return false
}

func (s *Store) IncompleteOperations(ctx context.Context) ([]model.Operation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.value FROM records r JOIN operation_claims c ON c.id=r.key WHERE r.bucket='operations' AND c.active=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Operation{}
	for rows.Next() {
		var b []byte
		var op model.Operation
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &op); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// CheckIntegrity fails closed when durable operation state is internally
// inconsistent: an active claim without its operation record, an active claim
// whose operation is already terminal, an active claim without its immutable
// accepted audit, or an active target lock with no active claim. Such state
// cannot be repaired automatically without silently releasing locks or
// inventing audit history, so the caller must stop for operator reconciliation.
// Locks are never released and no records are synthesized here.
func (s *Store) CheckIntegrity(ctx context.Context) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_claims c LEFT JOIN records r ON r.bucket='operations' AND r.key=c.id WHERE c.active=1 AND r.key IS NULL`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return integrity(fmt.Sprintf("%d active operation claim(s) have no operation record", n))
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_claims c JOIN records r ON r.bucket='operations' AND r.key=c.id WHERE c.active=1 AND json_valid(r.value)=1 AND json_extract(r.value,'$.state') IN ('succeeded','failed')`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return integrity(fmt.Sprintf("%d active operation claim(s) conflict with a terminal operation state", n))
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_claims c LEFT JOIN records a ON a.bucket='audit' AND a.key=c.id||'-accepted' WHERE c.active=1 AND a.key IS NULL`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return integrity(fmt.Sprintf("%d active operation claim(s) are missing their accepted audit record", n))
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_targets t LEFT JOIN operation_claims c ON c.id=t.operation_id WHERE t.active=1 AND (c.id IS NULL OR c.active!=1)`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return integrity(fmt.Sprintf("%d active target lock(s) have no active operation claim", n))
	}
	return nil
}
func (s *Store) OperationForKey(ctx context.Context, actor, key, planID string) (model.Operation, error) {
	var b []byte
	var foundPlan string
	var op model.Operation
	err := s.db.QueryRowContext(ctx, `SELECT r.value,c.plan_id FROM operation_claims c JOIN records r ON r.bucket='operations' AND r.key=c.id WHERE c.actor=? AND c.idem=?`, actor, key).Scan(&b, &foundPlan)
	if errors.Is(err, sql.ErrNoRows) {
		return op, ErrNotFound
	}
	if err != nil {
		return op, err
	}
	if foundPlan != planID {
		return op, ErrConflict
	}
	err = json.Unmarshal(b, &op)
	return op, err
}
