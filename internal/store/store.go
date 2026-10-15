// Package store maintains durable application records, not cluster inventory.
// WAL requires local storage; DELETE journaling also supports configured BeeGFS.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

// CleanupAuth removes only expired/invalid authentication records. SQLite
// performs the bounded-bucket scan directly; it cannot starve old records
// behind List's newest-first limit. Audit and operation records are untouched.
func (s *Store) CleanupAuth(ctx context.Context, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM records WHERE
	 (bucket='auth.attempts' AND COALESCE(julianday(json_extract(value,'$.expiresAt')),0)<=julianday(?)) OR
	 (bucket='auth.sessions' AND (COALESCE(julianday(json_extract(value,'$.identity.expiresAt')),0)<=julianday(?) OR COALESCE(julianday(json_extract(value,'$.identity.authenticatedAt')),0)+15.0/1440<=julianday(?)))`, now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano))
	return err
}

// Targets locks both a PVC and its selected PV; using only PVName would allow
// two queued operations to mutate the same PVC using different PV reservations.
func Targets(p model.Plan, primary string) []string {
	a := p.Prepared.Action
	out := []string{primary}
	add := func(v string) {
		for _, old := range out {
			if old == v {
				return
			}
		}
		out = append(out, v)
	}
	switch {
	case strings.HasPrefix(a.Kind, "notebook."):
		add("notebook:" + a.Namespace + "/" + a.Name)
	case strings.HasPrefix(a.Kind, "profile."):
		add("profile:" + a.Name)
	default:
		if a.Name != "" {
			add("pvc:" + a.Namespace + "/" + a.Name)
		}
	}
	if a.PVName != "" {
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
	for _, target := range Targets(plan, op.Target) {
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
	for _, target := range Targets(plan, op.Target) {
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
		return err
	}
	var current model.Operation
	if err := json.Unmarshal(currentBytes, &current); err != nil {
		return err
	}
	if current.State == "succeeded" || current.State == "failed" || current.State == "needs-review" {
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
		return err
	}
	var accepted model.Audit
	if err := json.Unmarshal(acceptedBytes, &accepted); err != nil {
		return err
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
