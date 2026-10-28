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
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"kubeflow-admin/internal/model"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("record not found")
var ErrConflict = errors.New("operation conflicts with an existing request or locked target")

// ErrInvalidTarget reports a server-validated action whose derived target is
// malformed. No claim or lock is created.
var ErrInvalidTarget = errors.New("invalid or unsafe operation target")

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

const cleanupBatch = 500

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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var mode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode="+journal).Scan(&mode); err != nil || mode != journal {
		s.Close()
		return nil, errors.New("cannot enable requested SQLite journal mode: " + journal)
	}
	if err := s.migrate(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// schemaVersion is the durable application schema. Migration must be
// transactional and versioned: an older release unconditionally resetting this
// value could otherwise silently downgrade a database written by a newer one.
const schemaVersion = 3

// migrate creates or upgrades the database schema in one transaction. Existing
// records, journals and authentication state are preserved. Accepted operation
// evidence is copied from authentic accepted audit rows into the immutable
// safety ledger; missing evidence is left missing so CheckIntegrity fails closed
// instead of fabricating history.
func (s *Store) migrate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	if err := tx.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than supported version %d", version, schemaVersion)
	}
	var compactionExists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='operation_compaction'`).Scan(&compactionExists); err != nil {
		return err
	}
	for _, query := range []string{
		`CREATE TABLE IF NOT EXISTS records(bucket TEXT NOT NULL,key TEXT NOT NULL,value BLOB NOT NULL,updated INTEGER NOT NULL,PRIMARY KEY(bucket,key))`,
		`CREATE TABLE IF NOT EXISTS operation_claims(id TEXT PRIMARY KEY,actor TEXT NOT NULL,idem TEXT NOT NULL,plan_id TEXT NOT NULL,target TEXT NOT NULL,active INTEGER NOT NULL DEFAULT 1,UNIQUE(actor,idem),UNIQUE(plan_id))`,
		`CREATE UNIQUE INDEX IF NOT EXISTS active_target ON operation_claims(target) WHERE active=1`,
		`CREATE TABLE IF NOT EXISTS operation_targets(operation_id TEXT NOT NULL,target TEXT NOT NULL,active INTEGER NOT NULL DEFAULT 1,PRIMARY KEY(operation_id,target))`,
		`CREATE UNIQUE INDEX IF NOT EXISTS locked_target ON operation_targets(target) WHERE active=1`,
		`INSERT OR IGNORE INTO operation_targets(operation_id,target,active) SELECT id,target,active FROM operation_claims`,
		// The safety ledger is intentionally separate from visible audit history:
		// retention may delete presentation rows, but acceptance evidence must
		// survive for every active/unresolved claim and for idempotent replay.
		`CREATE TABLE IF NOT EXISTS operation_acceptance(operation_id TEXT PRIMARY KEY,actor TEXT NOT NULL,actor_display TEXT NOT NULL,plan_id TEXT NOT NULL,target TEXT NOT NULL,action TEXT NOT NULL,reason TEXT NOT NULL,changes BLOB NOT NULL,created INTEGER NOT NULL)`,
		// Pending compaction is indexed by completion time so cleanup examines only
		// a bounded batch instead of scanning an ever-longer compacted prefix.
		`CREATE TABLE IF NOT EXISTS operation_compaction(operation_id TEXT PRIMARY KEY,updated INTEGER NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS operation_compaction_updated ON operation_compaction(updated)`,
		`CREATE INDEX IF NOT EXISTS records_bucket_updated ON records(bucket,updated)`,
		`CREATE INDEX IF NOT EXISTS operation_claims_active ON operation_claims(active)`,
	} {
		if _, err := tx.ExecContext(ctx, query); err != nil {
			return err
		}
	}
	// Idempotent: fills only missing active ledgers from authentic evidence. This
	// also lets an operator restore a missing accepted row and reopen a schema-v3
	// development database without a version change.
	if err := backfillAcceptance(ctx, tx); err != nil {
		return err
	}
	if compactionExists == 0 {
		if err := backfillCompactionQueue(ctx, tx); err != nil {
			return err
		}
	}
	// Never commit a version bump (or any backfill) over active work whose safety
	// evidence, operation record or primary lock set is inconsistent.
	if err := validateActiveHistory(ctx, tx); err != nil {
		return err
	}
	// A Profile deletion that lost its namespace fence would let another
	// mutation enter the namespace while the cascade is unresolved, so the
	// database must not be opened with such a gap.
	if err := validateNamespaceFenceLocks(ctx, tx); err != nil {
		return err
	}
	if version != schemaVersion {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version=%d`, schemaVersion)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// backfillAcceptance copies only structurally authentic accepted audit rows. It
// never fills a missing row, never overwrites existing evidence and never
// treats a malformed, mismatched or incomplete row as success. Work is bounded
// to batches so restoring evidence on a large database remains responsive.
func backfillAcceptance(ctx context.Context, tx *sql.Tx) error {
	last := ""
	for {
		rows, err := tx.QueryContext(ctx, `SELECT c.id,c.actor,c.plan_id,c.target,a.value,a.updated,r.value
			FROM operation_claims c
			JOIN (SELECT key,value,updated FROM records WHERE bucket='audit' AND json_valid(value)=1) a ON a.key=c.id||'-accepted'
			JOIN (SELECT key,value FROM records WHERE bucket='operations' AND json_valid(value)=1) r ON r.key=c.id
			WHERE NOT EXISTS (SELECT 1 FROM operation_acceptance x WHERE x.operation_id=c.id)
			AND c.id>? ORDER BY c.id LIMIT 500`, last)
		if err != nil {
			return err
		}
		type candidate struct {
			id, actor, planID, target string
			auditValue                []byte
			auditUpdated              int64
			operationValue            []byte
		}
		var candidates []candidate
		for rows.Next() {
			var c candidate
			if err := rows.Scan(&c.id, &c.actor, &c.planID, &c.target, &c.auditValue, &c.auditUpdated, &c.operationValue); err != nil {
				rows.Close()
				return err
			}
			candidates = append(candidates, c)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(candidates) == 0 {
			return nil
		}
		for _, c := range candidates {
			last = c.id
			audit, err := decodeAudit(c.auditValue)
			if err != nil || audit.Result != "queued" {
				continue
			}
			op, err := decodeOperation(c.operationValue)
			if err != nil {
				continue
			}
			if audit.ID != c.id+"-accepted" || audit.OperationID != c.id || audit.Actor != c.actor ||
				audit.Target != c.target || audit.Action != op.Action ||
				op.ID != c.id || op.Actor != c.actor || op.PlanID != c.planID || op.Target != c.target {
				continue
			}
			changes, err := json.Marshal(audit.Changes)
			if err != nil {
				return err
			}
			if audit.Changes == nil {
				changes = []byte("[]")
			}
			created := audit.Time.UnixNano()
			if audit.Time.IsZero() {
				created = c.auditUpdated
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO operation_acceptance(operation_id,actor,actor_display,plan_id,target,action,reason,changes,created) VALUES(?,?,?,?,?,?,?,?,?)`,
				c.id, c.actor, audit.ActorDisplay, c.planID, c.target, op.Action, audit.Reason, changes, created); err != nil {
				return err
			}
		}
		if len(candidates) < 500 {
			return nil
		}
	}
}

func backfillCompactionQueue(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO operation_compaction(operation_id,updated)
		SELECT c.id,r.updated FROM operation_claims c
		JOIN (SELECT key,value,updated FROM records WHERE bucket='operations' AND json_valid(value)=1) r ON r.key=c.id
		WHERE c.active=0 AND json_extract(r.value,'$.state') IN ('succeeded','failed')`)
	return err
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
	switch target := out.(type) {
	case *model.Operation:
		op, err := decodeOperation(b)
		if err != nil {
			return err
		}
		*target = op
		return nil
	case *model.Audit:
		audit, err := decodeStoredAudit(b)
		if err != nil {
			return err
		}
		*target = audit
		return nil
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
// closed forever. Audit and operation records are untouched. Each call performs
// at most one bounded batch per bucket within the cleanup context budget.
func (s *Store) CleanupAuth(ctx context.Context, now time.Time) error {
	stamp := now.UTC().Format(time.RFC3339Nano)
	return s.cleanupRecords(ctx, []string{"auth.attempts", "auth.sessions"},
		cleanupDelete{
			bucket:    "auth.attempts",
			predicate: `json_valid(value)=1 AND COALESCE(julianday(json_extract(value,'$.expiresAt')),0)<=julianday(?)`,
			args:      []any{stamp},
		},
		cleanupDelete{
			bucket:    "auth.sessions",
			predicate: `json_valid(value)=1 AND (COALESCE(julianday(json_extract(value,'$.identity.expiresAt')),0)<=julianday(?) OR COALESCE(julianday(json_extract(value,'$.identity.authenticatedAt')),0)+15.0/1440<=julianday(?))`,
			args:      []any{stamp, stamp},
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
			bucket:    "plans",
			predicate: `json_valid(value)=1 AND COALESCE(julianday(json_extract(value,'$.expiresAt')),0)<=julianday(?)`,
			args:      []any{stamp},
		},
	)
}

type cleanupDelete struct {
	bucket    string
	predicate string
	args      []any
}

// cleanupRecords deletes malformed rows (json_valid=0) in buckets and then runs
// the bounded expiry deletes, all in one short transaction. Every DELETE is
// limited to cleanupBatch records by rowid, so request-path housekeeping cannot
// hold the single SQLite connection for an unbounded scan.
func (s *Store) cleanupRecords(ctx context.Context, buckets []string, deletes ...cleanupDelete) error {
	if len(buckets) == 0 {
		return nil
	}
	ctx, cancel := cleanupContext(ctx)
	defer cancel()
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
	if _, err = tx.ExecContext(ctx, `DELETE FROM records WHERE rowid IN (SELECT rowid FROM records WHERE bucket IN (`+placeholders+`) AND json_valid(value)=0 LIMIT ?)`, append(args, cleanupBatch)...); err != nil {
		return err
	}
	for _, d := range deletes {
		query := `DELETE FROM records WHERE rowid IN (SELECT rowid FROM records WHERE bucket=? AND ` + d.predicate + ` LIMIT ?)`
		callArgs := append(append([]any{}, d.args...), cleanupBatch)
		if _, err = tx.ExecContext(ctx, query, append([]any{d.bucket}, callArgs...)...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// profileDeleteKind is the typed destructive Profile action. It is the only
// action that cascades into a namespace: there is no namespace delete action.
// Deleting a Profile removes the namespace the Profile controller owns, so the
// whole namespace is fenced while the deletion is unresolved.
const profileDeleteKind = "profile.delete"

// pvcDeleteKind is the unused-only PersistentVolumeClaim deletion. It is not a
// namespace cascade, but it acquires the same exclusive namespace fence so it
// cannot race a Notebook mount edit, a claim expansion or a PV reservation that
// would add a reference to the same claim while the deletion is unresolved.
const pvcDeleteKind = "pvc.delete"

// storageYAMLMaxBytes is the server-side bound of the restricted storage
// projection. The HTTP request limit is separate and must also be enforced at
// the API boundary; the backend never parses an unbounded document.
const storageYAMLMaxBytes = 32 << 10

// PrimaryTarget is the canonical lock name derived from the server-owned action.
// It is the only primary target a caller may claim; accepting an arbitrary
// caller primary would let a plan lock unrelated PVC/PV/object targets.
//
// A Profile action keeps the profile:<name> primary target, including the
// destructive profile.delete action. The namespace the cascade reaches is a
// separate, additional lock target (see Targets).
func PrimaryTarget(a model.Action) string {
	switch {
	case a.Kind == "pod.restart":
		return "pod:" + a.Namespace + "/" + a.Name
	case a.Kind == "node.cordon" || a.Kind == "node.uncordon":
		return "node:" + a.Name
	case strings.HasPrefix(a.Kind, "notebook."):
		return "notebook:" + a.Namespace + "/" + a.Name
	case strings.HasPrefix(a.Kind, "profile."):
		return "profile:" + a.Name
	case a.Kind == "pv.create.static" || a.Kind == "pv.edit" || a.Kind == "pv.delete" || a.Kind == "pv.reclaim":
		// PersistentVolumes are cluster scoped: the canonical target never
		// carries a namespace, even when another field does.
		return "pv:" + a.Name
	default:
		return "pvc:" + a.Namespace + "/" + a.Name
	}
}

// validTargetName accepts a Kubernetes object name but never a namespace,
// path, wildcard or empty value. A restart must not acquire a lock on an
// arbitrary or surprising parent object.
func validTargetName(name string) bool {
	if name == "" || len(name) > 253 || strings.Contains(name, "/") || strings.Contains(name, "..") {
		return false
	}
	for i, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.'
		if !ok || (r == '.' || r == '-') && (i == 0 || i == len(name)-1) {
			return false
		}
	}
	return true
}

// validDNS1123Label reports whether v is a DNS-1123 label: at most 63
// lowercase alphanumeric characters or interior hyphens, starting and ending
// with an alphanumeric character. A Profile name is also the name of the
// namespace the Profile controller creates for it, so a Profile name must
// never be a path, a wildcard or any other namespace selector.
func validDNS1123Label(v string) bool {
	if v == "" || len(v) > 63 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-':
			if i == 0 || i == len(v)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// actionNamespace returns the namespace an action mutates, or "" for a
// cluster-scoped action. A Profile action mutates the namespace that carries
// the Profile name, because the Profile controller creates and owns exactly
// that namespace; the namespace is never taken from a field that is not the
// server-owned action.
func actionNamespace(a model.Action) string {
	if strings.HasPrefix(a.Kind, "profile.") {
		return a.Name
	}
	return a.Namespace
}

// fenceNamespace returns the namespace whose deletion fence an action must
// respect, or "" when the action is cluster scoped or names an unusable
// namespace. A malformed namespace never becomes a lock target: it cannot
// match a real namespace fence and it cannot widen one.
func fenceNamespace(a model.Action) string {
	ns := actionNamespace(a)
	if ns == "" || !validDNS1123Label(ns) {
		return ""
	}
	return ns
}

// profileNamespaceFromTarget returns the Profile name of a canonical Profile
// target ("profile:<name>").
func profileNamespaceFromTarget(target string) (string, bool) {
	name, ok := strings.CutPrefix(target, "profile:")
	if !ok || !validDNS1123Label(name) {
		return "", false
	}
	return name, true
}

// pvcNamespaceFromTarget returns the namespace of a canonical namespaced PVC
// target ("pvc:<namespace>/<name>"). A malformed namespace or name never
// identifies a fence.
func pvcNamespaceFromTarget(target string) (string, bool) {
	rest, ok := strings.CutPrefix(target, "pvc:")
	if !ok {
		return "", false
	}
	namespace, claimName, ok := strings.Cut(rest, "/")
	if !ok || !validDNS1123Label(namespace) || !validTargetName(claimName) {
		return "", false
	}
	return namespace, true
}

// validateProfileDelete enforces the typed contract of the destructive Profile
// action: the exact Profile name, the exact typed confirmation, no foreign
// namespace and a recorded reason. A deletion whose target cannot be fenced is
// refused before any claim or lock exists. Irrelevant fields (PVName,
// ParentNotebook) are never turned into lock targets for this kind.
func validateProfileDelete(a model.Action) error {
	if !validDNS1123Label(a.Name) {
		return ErrInvalidTarget
	}
	if a.ConfirmName != a.Name {
		return ErrInvalidTarget
	}
	if a.Namespace != "" && a.Namespace != a.Name {
		return ErrInvalidTarget
	}
	if strings.TrimSpace(a.Reason) == "" {
		return ErrInvalidTarget
	}
	// The reason limit counts Unicode code points, like the API's documented
	// maxLength and the backend's own limit.
	if utf8.RuneCountInString(a.Reason) > 500 {
		return ErrInvalidTarget
	}
	return nil
}

// validateStorageTarget validates the target scope shared by the new storage
// edit/delete actions. A PV is cluster scoped and must never carry a namespace;
// a PVC is namespaced and must carry exactly one usable namespace label. A
// force flag is never accepted: this adapter does not implement forced deletion.
func validateStorageTarget(a model.Action) error {
	if !validTargetName(a.Name) || a.Force || a.TargetUID == "" || a.TargetResourceVersion == "" {
		return ErrInvalidTarget
	}
	switch a.Kind {
	case "pv.edit", "pv.delete", "pv.reclaim":
		if a.Namespace != "" {
			return ErrInvalidTarget
		}
		return nil
	case "pvc.edit", "pvc.delete":
		if !validDNS1123Label(a.Namespace) {
			return ErrInvalidTarget
		}
		return nil
	default:
		return ErrInvalidTarget
	}
}

// validateStorageEdit enforces the typed contract of pv.edit and pvc.edit. The
// action carries the exact observed UID/resourceVersion and a bounded,
// restricted projection; it never carries a typed delete confirmation.
func validateStorageEdit(a model.Action) error {
	if err := validateStorageTarget(a); err != nil {
		return err
	}
	if a.ConfirmName != "" || strings.TrimSpace(a.StorageYAML) == "" || len(a.StorageYAML) > storageYAMLMaxBytes {
		return ErrInvalidTarget
	}
	if a.Reason != "" && utf8.RuneCountInString(a.Reason) > 500 {
		return ErrInvalidTarget
	}
	return nil
}

// validateStorageDelete enforces the typed contract of pv.delete and pvc.delete:
// exact target identity, an authored reason, the exact typed confirmation, and
// no edit document or force flag.
func validateStorageDelete(a model.Action) error {
	if err := validateStorageTarget(a); err != nil {
		return err
	}
	if a.StorageYAML != "" || a.ConfirmName != a.Name {
		return ErrInvalidTarget
	}
	if strings.TrimSpace(a.Reason) == "" || utf8.RuneCountInString(a.Reason) > 500 {
		return ErrInvalidTarget
	}
	return nil
}

// Targets locks the canonical primary plus its selected PV; using only PVName
// would allow two queued operations to mutate the same PVC using different PV
// reservations. A restart also locks its validated parent Notebook so it cannot
// race an edit or stop of that parent.
//
// A Profile deletion additionally locks the namespace the Profile owns. The
// cascade removes every object in that namespace, so the whole namespace is
// fenced for as long as the deletion is unresolved. The fence is derived only
// from the validated Profile name: no other field can add a lock target.
//
// An unused-only PVC deletion also acquires the same exclusive namespace fence.
// A Notebook edit that adds a PVC mount currently carries only a Notebook
// target, and widening every historical Notebook plan with new PVC locks would
// invalidate active legacy operations. The namespace fence is the conservative
// compatible alternative: it serializes the PVC delete against every ordinary
// namespace-scoped operation (including Notebook edits that add the same claim)
// without changing the persisted lock shape of existing actions. A PV deletion
// is already serialized by the existing pv:<name> lock used by reservations.
func Targets(p model.Plan) []string {
	out, err := targets(p)
	if err == nil {
		return out
	}
	// Direct callers of Targets keep the historical fallback: malformed
	// dependency evidence yields only the base targets. Admission and integrity
	// never use this fallback; they call targets and fail closed on the error.
	return baseTargets(p)
}

// targets is the strict lock-set builder used by admission and integrity.
func targets(p model.Plan) ([]string, error) {
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
	// Secondary dependencies are derived only from the validated action and its
	// safe prepared evidence: the selected PV of a binding, the optional
	// dedicated PodDefault of a PVC creation, and the exact reviewed PodDefaults
	// of a cleanup. An error is a real admission/integrity failure, never an
	// empty dependency set.
	deps, err := plannedDependencyTargets(p.Prepared)
	if err != nil {
		return nil, err
	}
	for _, target := range deps {
		add(target)
	}
	if a.Kind == "pod.restart" && validTargetName(a.Namespace) && validTargetName(p.Prepared.ParentNotebook) {
		add("notebook:" + a.Namespace + "/" + p.Prepared.ParentNotebook)
	}
	if a.Kind == profileDeleteKind || a.Kind == pvcDeleteKind {
		if ns := fenceNamespace(a); ns != "" {
			add("namespace:" + ns)
		}
	}
	return out, nil
}

// baseTargets returns only targets that never depend on parsed evidence.
func baseTargets(p model.Plan) []string {
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
	if a.Kind == "pod.restart" && validTargetName(a.Namespace) && validTargetName(p.Prepared.ParentNotebook) {
		add("notebook:" + a.Namespace + "/" + p.Prepared.ParentNotebook)
	}
	if a.Kind == profileDeleteKind || a.Kind == pvcDeleteKind {
		if ns := fenceNamespace(a); ns != "" {
			add("namespace:" + ns)
		}
	}
	return out
}

// checkNamespaceFence enforces the namespace fence in both directions for one
// acceptance transaction.
//
// Direction one: an active namespace:<ns> deletion lock blocks every other
// mutation that touches that namespace, whatever its own target is.
//
// Direction two: an exclusive namespace action (a Profile deletion or an
// unused-only PVC deletion) is refused while any other active mutation still
// targets the same namespace. The compared targets are exactly the ordinary
// namespace-scoped primary targets (notebook:<ns>/..., pod:<ns>/... and
// pvc:<ns>/...), plus a secondary poddefault:<ns>/... dependency, the Profile
// target (profile:<ns>) and the namespace fence target itself
// (namespace:<ns>). Cluster-scoped targets such as pv:<name> and node:<name>
// are deliberately not compared: they are not namespace scoped.
//
// The caller has already resolved an idempotent retry before this check, so a
// retry of an accepted deletion keeps resolving to the same operation even
// while the namespace stays fenced.
func checkNamespaceFence(ctx context.Context, tx *sql.Tx, a model.Action, ns, operationID string) error {
	var found int
	// Direction one: a pending exclusive namespace deletion owns the namespace.
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM operation_targets WHERE target=? AND active=1 LIMIT 1`, "namespace:"+ns).Scan(&found)
	if err == nil {
		return ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if a.Kind != profileDeleteKind && a.Kind != pvcDeleteKind {
		return nil
	}
	// Direction two: an exclusive namespace deletion may not start while the
	// namespace is still busy. The prefixes are compared with substr, never
	// with LIKE, so a namespace can never act as a wildcard.
	notebookPrefix := "notebook:" + ns + "/"
	podPrefix := "pod:" + ns + "/"
	pvcPrefix := "pvc:" + ns + "/"
	podDefaultPrefix := "poddefault:" + ns + "/"
	err = tx.QueryRowContext(ctx, `SELECT 1 FROM operation_targets
		WHERE active=1 AND operation_id<>? AND (
			target=? OR target=?
			OR substr(target,1,?)=? OR substr(target,1,?)=? OR substr(target,1,?)=? OR substr(target,1,?)=?)
		LIMIT 1`,
		operationID,
		"namespace:"+ns, "profile:"+ns,
		len(notebookPrefix), notebookPrefix,
		len(podPrefix), podPrefix,
		len(pvcPrefix), pvcPrefix,
		len(podDefaultPrefix), podDefaultPrefix,
	).Scan(&found)
	if err == nil {
		return ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

// CreateOperation atomically claims actor+idempotency key, immutable plan, all
// affected targets, and writes the accepted operation plus audit record. It uses
// the shipped operation-budget defaults.
func (s *Store) CreateOperation(ctx context.Context, op model.Operation, plan model.Plan, key string) (model.Operation, bool, error) {
	return s.createOperation(ctx, op, plan, key, HistoryPolicy{})
}

// CreateOperationWithPolicy is the admission path for a configured deployment:
// it enforces the same total-operation and unresolved-operation budgets inside
// the acceptance transaction, after idempotent retries have resolved.
func (s *Store) CreateOperationWithPolicy(ctx context.Context, op model.Operation, plan model.Plan, key string, policy HistoryPolicy) (model.Operation, bool, error) {
	return s.createOperation(ctx, op, plan, key, policy)
}

func (s *Store) createOperation(ctx context.Context, op model.Operation, plan model.Plan, key string, policy HistoryPolicy) (model.Operation, bool, error) {
	p := policy.WithDefaults()
	if op.CreatedAt.IsZero() {
		op.CreatedAt = time.Now().UTC()
	}
	if op.UpdatedAt.IsZero() {
		op.UpdatedAt = op.CreatedAt
	}
	if op.ID == "" || plan.ID == "" || op.PlanID != plan.ID || op.Actor == "" || op.Actor != plan.Actor || op.Target == "" || op.Action == "" || op.Action != plan.Prepared.Action.Kind || key == "" || op.State != "queued" {
		return model.Operation{}, false, ErrConflict
	}
	// A freshly queued operation cannot already carry mutation proof. Receipts
	// are appended only through durable checkpoints after a real write.
	if len(op.MutationReceipts) != 0 {
		return model.Operation{}, false, ErrConflict
	}
	// A restart's secondary lock is derived only from the server-validated parent
	// Notebook. Never accept a namespace/path/wildcard there.
	if plan.Prepared.Action.Kind == "pod.restart" {
		if !validTargetName(plan.Prepared.Action.Namespace) || !validTargetName(plan.Prepared.Action.Name) || !validTargetName(plan.Prepared.ParentNotebook) {
			return model.Operation{}, false, ErrInvalidTarget
		}
	}
	// The typed Profile deletion is the only destructive Profile action. Its
	// target must be exact, because the acceptance fences a whole namespace
	// under that name.
	if plan.Prepared.Action.Kind == profileDeleteKind {
		if err := validateProfileDelete(plan.Prepared.Action); err != nil {
			return model.Operation{}, false, err
		}
	}
	// The restricted storage edit and unused-only storage delete are typed
	// destructive/privileged actions. Validate their target scope and exact
	// identity before any claim or lock is written.
	switch plan.Prepared.Action.Kind {
	case "pv.create.static":
		if err := validatePVCreateStatic(plan.Prepared.Action); err != nil {
			return model.Operation{}, false, err
		}
	case "pvc.create.bound":
		if err := validatePVCCreateBound(plan.Prepared.Action); err != nil {
			return model.Operation{}, false, err
		}
	case "pv.edit", "pvc.edit":
		if err := validateStorageEdit(plan.Prepared.Action); err != nil {
			return model.Operation{}, false, err
		}
	case "pv.delete", "pvc.delete", "pv.reclaim":
		if err := validateStorageDelete(plan.Prepared.Action); err != nil {
			return model.Operation{}, false, err
		}
	}
	// The optional PodDefault dependency fields are typed and only meaningful
	// on a PVC creation or the reviewed PVC cleanup. Validate them before any
	// claim or lock is written.
	if err := validatePodDefaultContract(plan.Prepared); err != nil {
		return model.Operation{}, false, err
	}
	// Admission must fail closed on malformed dependency evidence instead of
	// relying on Targets' historical no-op fallback.
	if _, err := plannedDependencyTargets(plan.Prepared); err != nil {
		return model.Operation{}, false, ErrInvalidTarget
	}
	lockedTargets, err := targets(plan)
	if err != nil {
		return model.Operation{}, false, ErrInvalidTarget
	}
	// There is no namespace delete action. The Profile cascade owns the
	// namespace it removes, so a direct namespace deletion is refused outright
	// instead of being derived into an unrelated PVC-style lock target.
	if plan.Prepared.Action.Kind == "namespace.delete" {
		return model.Operation{}, false, ErrInvalidTarget
	}
	// The primary target must be exactly the canonical target of the server-owned
	// plan action. Derived targets (for example a reserved PV) never substitute an
	// arbitrary caller-supplied primary.
	if op.Target != PrimaryTarget(plan.Prepared.Action) {
		return model.Operation{}, false, ErrConflict
	}
	// The display label is a presentation claim captured at acceptance. It never
	// substitutes for the issuer|subject authority key above. The manager supplies
	// the current verified identity at acceptance; direct callers may fall back to
	// the immutable plan label.
	display := op.ActorDisplay
	if display == "" {
		display = plan.ActorDisplay
	}
	op.ActorDisplay = display
	// Build the exact accepted audit row before admission so its serialized size
	// is included in the byte budget inside the acceptance transaction.
	// Persist the exact reviewed PodDefault identities independently of backend
	// presentation changes, so the immutable acceptance evidence remains
	// complete after the transient plan is cleaned up. Durable audit/acceptance
	// evidence never carries raw paths or handles.
	acceptedChanges := plan.Prepared.Changes
	if identityChange, ok, err := reviewedPodDefaultIdentityChange(plan.Prepared); err != nil {
		return model.Operation{}, false, ErrInvalidTarget
	} else if ok {
		// The store owns this reserved field. Drop any server-supplied value
		// with the same name so the immutable snapshot is unambiguous.
		filtered := make([]model.Change, 0, len(acceptedChanges)+1)
		for _, change := range acceptedChanges {
			if change.Field == reviewedPodDefaultIdentitiesField {
				continue
			}
			filtered = append(filtered, change)
		}
		acceptedChanges = append(filtered, identityChange)
	}
	acceptedChanges = redactAcceptanceChanges(acceptedChanges)
	acceptedAudit := model.Audit{ID: op.ID + "-accepted", Actor: op.Actor, ActorDisplay: op.ActorDisplay, Action: op.Action, Target: op.Target, Result: "queued", OperationID: op.ID, Reason: plan.Prepared.Action.Reason, Changes: acceptedChanges, Time: op.CreatedAt}
	acceptedAuditValue, err := json.Marshal(acceptedAudit)
	if err != nil {
		return op, false, err
	}
	// Every statement of the acceptance runs on this transaction, never on the
	// pool: the store keeps a single SQLite connection, so a query issued
	// outside the transaction while this transaction holds that connection
	// would wait for it instead of running. Each statement is a single row
	// query that is fully consumed before the next one starts, so no result
	// set is ever open while another statement runs.
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
		op, err = decodeOperation(b)
		return op, false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return op, false, err
	}
	// A retry above resolves through its existing claim even when admission is
	// full. A genuinely new claim must not grow total or unresolved work without
	// bound. These checks are inside the same transaction that inserts the claim,
	// so concurrent callers cannot both consume the last slot.
	var claims, unresolved int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(active),0) FROM operation_claims`).Scan(&claims, &unresolved); err != nil {
		return op, false, err
	}
	if claims >= p.MaxOperationRecords {
		return op, false, budget(fmt.Sprintf("retained operation records reached the configured maximum of %d", p.MaxOperationRecords))
	}
	if unresolved >= MaxUnresolvedOperations {
		return op, false, budget(fmt.Sprintf("unresolved operations reached the safety limit of %d", MaxUnresolvedOperations))
	}
	// Audit usage is checked in the same transaction, including the exact new
	// accepted row. Older audit is now prunable after ledger verification, so a
	// full budget rejects the new mutation until bounded cleanup catches up; it
	// never blocks an already accepted checkpoint.
	var auditCount int
	var auditBytes int64
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(LENGTH(value)),0) FROM records WHERE bucket='audit'`).Scan(&auditCount, &auditBytes); err != nil {
		return op, false, err
	}
	if auditCount+1 > p.MaxAuditRecords || auditBytes+int64(len(acceptedAuditValue)) > p.MaxAuditBytes {
		return op, false, budget("audit history budget is full; bounded background retention must prune before another mutation is accepted")
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
	for _, target := range lockedTargets {
		err = tx.QueryRowContext(ctx, `SELECT 1 FROM operation_targets WHERE target=? AND active=1`, target).Scan(&exists)
		if err == nil {
			return op, false, ErrConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return op, false, err
		}
	}
	// Namespace fence. A pending Profile deletion owns the whole namespace it
	// cascades into, so every other mutation in that namespace is refused; and
	// a Profile deletion is refused while another mutation in that namespace is
	// still active. Both directions are evaluated inside this acceptance
	// transaction, so two concurrent requests cannot both pass a stale read of
	// the lock table.
	if ns := fenceNamespace(plan.Prepared.Action); ns != "" {
		if err := checkNamespaceFence(ctx, tx, plan.Prepared.Action, ns, op.ID); err != nil {
			return op, false, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO operation_claims(id,actor,idem,plan_id,target) VALUES(?,?,?,?,?)`, op.ID, op.Actor, key, plan.ID, op.Target); err != nil {
		return op, false, err
	}
	for _, target := range lockedTargets {
		if _, err = tx.ExecContext(ctx, `INSERT INTO operation_targets(operation_id,target) VALUES(?,?)`, op.ID, target); err != nil {
			return op, false, err
		}
	}
	if err = put(ctx, tx, "operations", op.ID, op); err != nil {
		return op, false, err
	}
	changes, err := json.Marshal(acceptedChanges)
	if err != nil {
		return op, false, err
	}
	if acceptedChanges == nil {
		changes = []byte("[]")
	}
	// The ledger is written in the same transaction as the claim, locks,
	// operation and visible accepted audit. It is the evidence checkpoints use,
	// so later presentation-history cleanup cannot break a live operation.
	if _, err = tx.ExecContext(ctx, `INSERT INTO operation_acceptance(operation_id,actor,actor_display,plan_id,target,action,reason,changes,created) VALUES(?,?,?,?,?,?,?,?,?)`,
		op.ID, op.Actor, op.ActorDisplay, op.PlanID, op.Target, op.Action, plan.Prepared.Action.Reason, changes, op.CreatedAt.UnixNano()); err != nil {
		return op, false, err
	}
	audit := acceptedAudit
	if err = insertAudit(ctx, tx, audit); err != nil {
		return op, false, err
	}
	if err = tx.Commit(); err != nil {
		return op, false, err
	}
	return op, true, nil
}

func (s *Store) Checkpoint(ctx context.Context, op model.Operation, audit model.Audit) error {
	return s.updateOperation(ctx, op, audit, false, nil)
}
func (s *Store) Finish(ctx context.Context, op model.Operation, audit model.Audit) error {
	return s.updateOperation(ctx, op, audit, true, nil)
}

// updateOperation applies one durable operation update in its own transaction.
func (s *Store) updateOperation(ctx context.Context, op model.Operation, audit model.Audit, finish bool, reconcile *profileDeleteReconciliation) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.updateOperationTx(ctx, tx, op, audit, finish, reconcile); err != nil {
		return err
	}
	return tx.Commit()
}

// updateOperationTx applies one durable operation update inside a caller's
// transaction, so an offline reconciliation can read its evidence and write
// its outcome atomically.
//
// reconcile is set only by the offline Profile deletion reconciliation. It
// selects a private transition rule and adds the operator attribution to the
// audit row. An ordinary Checkpoint or Finish can never resolve an uncertain
// operation, and a reconciliation can never resolve anything except a
// needs-review Profile deletion.
func (s *Store) updateOperationTx(ctx context.Context, tx *sql.Tx, op model.Operation, audit model.Audit, finish bool, reconcile *profileDeleteReconciliation) error {
	if audit.ID == "" {
		return ErrConflict
	}
	if op.UpdatedAt.IsZero() {
		op.UpdatedAt = time.Now().UTC()
	}
	if finish {
		if op.State != "succeeded" && op.State != "failed" && op.State != "needs-review" {
			return ErrConflict
		}
	} else if op.State != "applying" {
		return ErrConflict
	}
	var actor, planID, target string
	var active int
	err := tx.QueryRowContext(ctx, `SELECT actor,plan_id,target,active FROM operation_claims WHERE id=?`, op.ID).Scan(&actor, &planID, &target, &active)
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
	if err = tx.QueryRowContext(ctx, `SELECT value FROM records WHERE bucket='operations' AND key=?`, op.ID).Scan(&currentBytes); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return integrity("active operation claim has no operation record")
		}
		return err
	}
	var current model.Operation
	if current, err = decodeOperation(currentBytes); err != nil {
		return err
	}
	// Mutation receipts are append-only proof. A checkpoint, finish or recovery
	// update may extend the persisted prefix but can never drop, reorder or
	// rewrite an earlier receipt.
	if err := validateOperationMutationReceiptsForTarget(op); err != nil {
		return ErrConflict
	}
	if !mutationReceiptPrefix(current.MutationReceipts, op.MutationReceipts) {
		return ErrConflict
	}
	// Only real lifecycle transitions are accepted. In particular a fresh
	// queued operation cannot be reported succeeded without an applying
	// checkpoint, while shutdown (queued->failed) and crash recovery
	// (queued/applying->needs-review) remain valid.
	if reconcile != nil {
		// The offline reconciliation has its own private rule. It never widens
		// the transitions an ordinary Finish may make.
		if !reconciledTransition(current.State, op.State) {
			return ErrConflict
		}
	} else if !allowedTransition(current.State, op.State, finish) {
		return ErrConflict
	}
	// The pre-delete checkpoint marker is monotonic: once a Profile delete
	// request may have been issued, no later update may clear that proof.
	op.ProfileDeleteCheckpointed = current.ProfileDeleteCheckpointed || op.ProfileDeleteCheckpointed
	// Identity/action metadata belongs to the original claim, not caller input.
	op.Action = current.Action
	op.CreatedAt = current.CreatedAt
	op.HistoryExpired = current.HistoryExpired
	// Read the immutable safety ledger, not visible audit history: retention may
	// have deleted the accepted presentation row while the operation was still
	// active, and that must never turn into missing evidence or a fabricated
	// reason/diff.
	var acceptedActor, acceptedDisplay, acceptedPlan, acceptedAction, acceptedTarget, acceptedReason string
	var acceptedChanges []byte
	err = tx.QueryRowContext(ctx, `SELECT actor,actor_display,plan_id,action,target,reason,changes FROM operation_acceptance WHERE operation_id=?`, op.ID).Scan(&acceptedActor, &acceptedDisplay, &acceptedPlan, &acceptedAction, &acceptedTarget, &acceptedReason, &acceptedChanges)
	if errors.Is(err, sql.ErrNoRows) {
		// Never fabricate the immutable accepted reason/diff. Fail closed so an
		// operator can restore or reconcile the exact history.
		return integrity("operation " + op.ID + " is missing its immutable acceptance evidence")
	}
	if err != nil {
		return err
	}
	changes, err := decodeChanges(acceptedChanges)
	if err != nil {
		return err
	}
	if acceptedActor != actor || acceptedPlan != planID || acceptedAction != current.Action || acceptedTarget != current.Target {
		return integrity("acceptance evidence conflicts with the operation record")
	}
	op.ActorDisplay = acceptedDisplay
	audit.Actor = op.Actor
	audit.ActorDisplay = acceptedDisplay
	audit.Action = op.Action
	audit.Target = op.Target
	audit.Result = op.State
	audit.OperationID = op.ID
	audit.Time = op.UpdatedAt
	audit.Reason = acceptedReason
	// Acceptance changes stay immutable. Receipt proof is appended to visible
	// audit descriptions only, so audit pruning or plan expiry cannot remove the
	// durable operation receipts.
	audit.Changes = append(append([]model.Change(nil), changes...), mutationReceiptChanges(op.MutationReceipts)...)
	if reconcile != nil {
		// The reconciliation is a distinct, operator-attributed audit row. The
		// accepted actor, display and reason stay exactly as they were
		// accepted; the offline operator and the reconciliation reason are
		// appended as a reviewed change of their own.
		audit.Result = reconcile.Result
		audit.Changes = append(audit.Changes, model.Change{
			Field:  "Offline reconciliation",
			Before: "accepted by " + op.Actor + " with reason: " + acceptedReason,
			After:  "reconciled by " + reconcile.Operator + ": " + reconcile.Reason,
		})
	}
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
		if op.State == "succeeded" || op.State == "failed" {
			// Indexed pending-compaction marker: cleanup never scans an ever-growing
			// compacted prefix. needs-review is intentionally not queued.
			if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO operation_compaction(operation_id,updated) VALUES(?,?)`, op.ID, op.UpdatedAt.UnixNano()); err != nil {
				return err
			}
		}
	}
	return nil
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
	rows, err := s.db.QueryContext(ctx, `SELECT c.id,c.actor,c.plan_id,c.target,r.value FROM records r JOIN operation_claims c ON c.id=r.key WHERE r.bucket='operations' AND c.active=1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Operation{}
	for rows.Next() {
		var claimID, actor, planID, target string
		var b []byte
		if err = rows.Scan(&claimID, &actor, &planID, &target, &b); err != nil {
			return nil, err
		}
		op, err := decodeOperation(b)
		if err != nil {
			return nil, err
		}
		if op.ID != claimID || op.Actor != actor || op.PlanID != planID || op.Target != target {
			return nil, integrity("an active operation record conflicts with its claim")
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

// validateNamespaceFenceLocks verifies that every active exclusive namespace
// deletion (Profile deletion or unused-only PVC deletion) still holds the
// namespace fence lock that protects its namespace, and that no other active
// action holds a namespace fence. The fence is the only thing that keeps a
// concurrent mutation out of a namespace whose deletion is unresolved, so a
// missing or forged fence is an integrity error: the store fails closed instead
// of accepting a mutation into that namespace.
//
// The evidence is the immutable acceptance ledger, which records the accepted
// action and its canonical target for every active claim. A claim without
// ledger evidence is reported by the acceptance evidence check instead.
//
// The check is deliberately split into bounded queries over a slice: no
// statement is started inside an open result set on the store's single
// connection.
func validateNamespaceFenceLocks(ctx context.Context, q sqlQueryer) error {
	rows, err := q.QueryContext(ctx, `SELECT c.id,a.action,a.target FROM operation_claims c
		JOIN operation_acceptance a ON a.operation_id=c.id
		WHERE c.active=1`)
	if err != nil {
		return err
	}
	type exclusive struct {
		operationID string
		action      string
		target      string
		namespace   string
	}
	var deletions []exclusive
	for rows.Next() {
		var d exclusive
		if err := rows.Scan(&d.operationID, &d.action, &d.target); err != nil {
			rows.Close()
			return err
		}
		switch d.action {
		case profileDeleteKind:
			ns, ok := profileNamespaceFromTarget(d.target)
			if !ok {
				rows.Close()
				return integrity("an active Profile deletion does not have a canonical Profile target")
			}
			d.namespace = ns
			deletions = append(deletions, d)
		case pvcDeleteKind:
			ns, ok := pvcNamespaceFromTarget(d.target)
			if !ok {
				rows.Close()
				return integrity("an active PVC deletion does not have a canonical namespaced claim target")
			}
			d.namespace = ns
			deletions = append(deletions, d)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, d := range deletions {
		var n int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_targets WHERE operation_id=? AND target=? AND active=1`, d.operationID, "namespace:"+d.namespace).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return integrity("an active exclusive namespace deletion is missing its namespace fence lock")
		}
	}
	// A namespace fence must never be an incidental lock of any other action,
	// and a reviewed deletion may not fence a namespace other than its own.
	// Accepting either would invent an exclusive namespace capability outside
	// the two reviewed deletion kinds.
	rows, err = q.QueryContext(ctx, `SELECT a.action,a.target,t.target FROM operation_targets t
		JOIN operation_claims c ON c.id=t.operation_id AND c.active=1
		JOIN operation_acceptance a ON a.operation_id=c.id
		WHERE t.active=1 AND substr(t.target,1,10)='namespace:'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var action, actionTarget, fence string
		if err := rows.Scan(&action, &actionTarget, &fence); err != nil {
			return err
		}
		expected := ""
		switch action {
		case profileDeleteKind:
			if ns, ok := profileNamespaceFromTarget(actionTarget); ok {
				expected = "namespace:" + ns
			}
		case pvcDeleteKind:
			if ns, ok := pvcNamespaceFromTarget(actionTarget); ok {
				expected = "namespace:" + ns
			}
		}
		if expected == "" || fence != expected {
			return integrity("an active action holds a namespace fence that is reserved for its own exclusive deletion")
		}
	}
	return rows.Err()
}

// CheckIntegrity fails closed when durable operation state is internally
// inconsistent: an active claim without its operation record, an active claim
// whose operation is already terminal, an active claim without its immutable
// accepted audit, an active target lock with no active claim, or an active
// Profile deletion without its namespace fence lock. Such state cannot be
// repaired automatically without silently releasing locks or inventing audit
// history, so the caller must stop for operator reconciliation.
// Locks are never released and no records are synthesized here.
func (s *Store) CheckIntegrity(ctx context.Context) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_claims WHERE active=1`).Scan(&n); err != nil {
		return err
	}
	if n > MaxUnresolvedOperations {
		return integrity(fmt.Sprintf("%d active operation claims exceed the safety limit of %d", n, MaxUnresolvedOperations))
	}
	if err := validateActiveHistory(ctx, s.db); err != nil {
		return err
	}
	if err := validateNamespaceFenceLocks(ctx, s.db); err != nil {
		return err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_claims c JOIN records r ON r.bucket='operations' AND r.key=c.id WHERE c.active=1 AND json_extract(r.value,'$.state') IN ('succeeded','failed')`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return integrity(fmt.Sprintf("%d active operation claim(s) conflict with a terminal operation state", n))
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
	return decodeOperation(b)
}

// ErrReconcileUnsupported reports that an offline reconciliation cannot be
// derived from the available evidence. It never unlocks anything and never
// changes durable state.
var ErrReconcileUnsupported = errors.New("operation cannot be reconciled from the available evidence")

// The accepted deletion evidence carries the reviewed identities. They are
// read from the immutable acceptance ledger, never from live cluster state,
// so a replacement object can never be adopted as the accepted one.
const (
	profileUIDChangeField              = "Profile UID"
	profileResourceVersionChangeField  = "Profile resource version"
	namespaceUIDChangeField            = "Namespace UID"
	clusterUIDChangeField              = "Cluster UID"
	profileReconcileVersionChangeField = "Profile deletion reconciliation version"
	// profileReconcileVersion is the current accepted-evidence version. The
	// cancellation path requires it: an acceptance record that predates the
	// contract cannot prove that no write was ever started.
	profileReconcileVersion = "1"
)

// ProfileDeleteReconcileOutcome enumerates the only supported derived
// outcomes of an offline Profile deletion reconciliation.
type ProfileDeleteReconcileOutcome string

const (
	// ProfileDeleteReconciledSucceeded is derived when two complete
	// observations show that the Profile and its namespace are both gone.
	ProfileDeleteReconciledSucceeded ProfileDeleteReconcileOutcome = "reconciled-succeeded"
	// ProfileDeleteReconciledCancelled is derived when the durable evidence
	// proves that no Profile delete request was ever issued, so the
	// operation changed nothing and its locks may be released.
	ProfileDeleteReconciledCancelled ProfileDeleteReconcileOutcome = "reconciled-cancelled"
)

// ProfileDeleteObservation is one complete observation of the two objects a
// Profile deletion cascades into. An incomplete observation proves nothing
// and is refused.
type ProfileDeleteObservation struct {
	// Complete reports that the Profile and the namespace were both read and
	// that each read returned a definite answer.
	Complete bool
	// ProfileUID is the Profile's UID, empty when the Profile is absent.
	ProfileUID string
	// NamespaceUID is the namespace's UID, empty when the namespace is
	// absent.
	NamespaceUID string
	// ClusterUID is the UID of the kube-system namespace of the cluster the
	// observation was taken from. It must equal the accepted cluster
	// identity: an observation taken against another cluster would report
	// both objects as absent and would falsely complete an old deletion.
	ClusterUID string
}

// ProfileDeleteReconcilePlan is the read-only evidence about one uncertain
// Profile deletion. It is what the offline CLI shows the operator before any
// change is proposed.
type ProfileDeleteReconcilePlan struct {
	// OperationID is the needs-review operation the evidence belongs to.
	OperationID string
	// Operation is the durable operation record.
	Operation model.Operation
	// ProfileName is the Profile the accepted deletion targeted.
	ProfileName string
	// ProfileUID, ProfileResourceVersion and NamespaceUID are the accepted
	// identities read from the immutable acceptance evidence.
	ProfileUID             string
	ProfileResourceVersion string
	NamespaceUID           string
	// ClusterUID is the accepted identity of the cluster the deletion was
	// prepared in: the UID of its kube-system namespace. Every observation
	// must come from the same cluster, or the deletion of an old cluster
	// could be reported as complete.
	ClusterUID string
	// ReconciliationVersion is the accepted-evidence version of the
	// reconciliation contract. It is empty when the accepted evidence
	// predates the contract.
	ReconciliationVersion string
	// MarkerPresent reports whether the durable operation record explicitly
	// carries the pre-delete checkpoint marker. A record written before the
	// marker existed decodes to false, so the absence of the field must
	// never be read as "no write happened".
	MarkerPresent bool
	// NoWriteProven reports the durable proof that no Profile delete request
	// was ever issued: the marker is present, it is false, and the accepted
	// evidence carries the current reconciliation version.
	NoWriteProven bool
	// ActiveTargets are the lock targets the operation still holds.
	ActiveTargets []string
	// AcceptedActor, AcceptedActorDisplay and AcceptedReason are the
	// immutable accepted identity and reason.
	AcceptedActor        string
	AcceptedActorDisplay string
	AcceptedReason       string
}

// ProfileDeleteReconciliation is the structured evidence an offline operator
// supplies to resolve one uncertain Profile deletion.
type ProfileDeleteReconciliation struct {
	// OperationID is the needs-review operation to resolve.
	OperationID string
	// Observations are the operator's complete observations of the Profile
	// and the namespace it owns.
	Observations []ProfileDeleteObservation
	// Reason is the operator's reconciliation reason. It is recorded in the
	// audit row.
	Reason string
	// Operator is the offline operator identity. It is recorded in the audit
	// row.
	Operator string
}

// profileDeleteReconciliation is the private operator attribution of one
// offline reconciliation. It never replaces the accepted evidence.
type profileDeleteReconciliation struct {
	// Result is the derived audit result of the reconciliation.
	Result string
	// Operator is the offline operator identity.
	Operator string
	// Reason is the operator's reconciliation reason.
	Reason string
}

// reconciledTransition reports whether a validated offline reconciliation may
// move an operation to its derived terminal state.
//
// It is deliberately separate from allowedTransition: an ordinary Finish or
// Checkpoint must never be able to resolve an uncertain operation, and a
// reconciliation must never resolve anything but a needs-review Profile
// deletion.
func reconciledTransition(from, to string) bool {
	if from != "needs-review" {
		return false
	}
	return to == "succeeded" || to == "failed"
}

// operationHasProfileDeleteMarker reports whether the stored operation record
// explicitly carries the pre-delete checkpoint marker.
//
// The marker is always serialized for a new operation, so a record without
// the key was written before the contract existed. Such a record can never
// prove that no write was started.
func operationHasProfileDeleteMarker(raw []byte) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false
	}
	_, ok := fields["profileDeleteCheckpointed"]
	return ok
}

// PlanProfileDeleteReconciliation reads the durable evidence about one
// uncertain Profile deletion. It never changes anything.
func (s *Store) PlanProfileDeleteReconciliation(ctx context.Context, operationID string) (ProfileDeleteReconcilePlan, error) {
	return planProfileDeleteReconciliation(ctx, s.db, operationID)
}

// planProfileDeleteReconciliation reads the durable evidence about one
// uncertain Profile deletion inside a caller's transaction.
func planProfileDeleteReconciliation(ctx context.Context, q sqlQueryer, operationID string) (ProfileDeleteReconcilePlan, error) {
	plan := ProfileDeleteReconcilePlan{OperationID: operationID}
	if operationID == "" {
		return plan, ErrNotFound
	}
	var raw []byte
	err := q.QueryRowContext(ctx, `SELECT value FROM records WHERE bucket='operations' AND key=?`, operationID).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return plan, ErrNotFound
	}
	if err != nil {
		return plan, err
	}
	op, err := decodeOperation(raw)
	if err != nil {
		return plan, err
	}
	plan.Operation = op
	plan.MarkerPresent = operationHasProfileDeleteMarker(raw)
	// An already resolved operation is not reconcilable: its claim and locks
	// are gone.
	var active int
	err = q.QueryRowContext(ctx, `SELECT active FROM operation_claims WHERE id=?`, operationID).Scan(&active)
	if errors.Is(err, sql.ErrNoRows) {
		return plan, ErrNotFound
	}
	if err != nil {
		return plan, err
	}
	if active != 1 {
		return plan, fmt.Errorf("%w: the operation no longer holds an active claim", ErrReconcileUnsupported)
	}
	rows, err := q.QueryContext(ctx, `SELECT target FROM operation_targets WHERE operation_id=? AND active=1`, operationID)
	if err != nil {
		return plan, err
	}
	// The targets are collected and the rows are closed before the next
	// statement runs. The store holds a single connection, so an open row
	// set would block the next query on the same connection.
	for rows.Next() {
		var target string
		if err = rows.Scan(&target); err != nil {
			rows.Close()
			return plan, err
		}
		plan.ActiveTargets = append(plan.ActiveTargets, target)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return plan, err
	}
	rows.Close()
	// The immutable acceptance evidence is the only source of the reviewed
	// identities.
	var acceptedActor, acceptedDisplay, acceptedAction, acceptedTarget, acceptedReason string
	var acceptedChanges []byte
	err = q.QueryRowContext(ctx, `SELECT actor,actor_display,action,target,reason,changes FROM operation_acceptance WHERE operation_id=?`, operationID).Scan(&acceptedActor, &acceptedDisplay, &acceptedAction, &acceptedTarget, &acceptedReason, &acceptedChanges)
	if errors.Is(err, sql.ErrNoRows) {
		return plan, fmt.Errorf("%w: the accepted evidence is missing", ErrReconcileUnsupported)
	}
	if err != nil {
		return plan, err
	}
	plan.AcceptedActor = acceptedActor
	plan.AcceptedActorDisplay = acceptedDisplay
	plan.AcceptedReason = acceptedReason
	if acceptedAction != profileDeleteKind {
		return plan, fmt.Errorf("%w: only a Profile deletion can be reconciled", ErrReconcileUnsupported)
	}
	name, ok := strings.CutPrefix(acceptedTarget, "profile:")
	if !ok || name == "" {
		return plan, fmt.Errorf("%w: the accepted target is not a Profile", ErrReconcileUnsupported)
	}
	plan.ProfileName = name
	changes, err := decodeChanges(acceptedChanges)
	if err != nil {
		return plan, err
	}
	uid, resourceVersion, namespaceUID, clusterUID, version, err := acceptedProfileDeleteIdentities(changes)
	if err != nil {
		return plan, err
	}
	plan.ProfileUID = uid
	plan.ProfileResourceVersion = resourceVersion
	plan.NamespaceUID = namespaceUID
	plan.ClusterUID = clusterUID
	plan.ReconciliationVersion = version
	// The durable proof that no write was ever started. It needs all three
	// parts: the field must be present, it must be false, and the accepted
	// evidence must carry the current contract version.
	plan.NoWriteProven = plan.MarkerPresent && !op.ProfileDeleteCheckpointed && plan.ReconciliationVersion == profileReconcileVersion
	return plan, nil
}

// acceptedProfileDeleteIdentities reads the reviewed identities from the
// immutable acceptance evidence.
//
// A missing or malformed identity fails closed. There is no fallback to live
// cluster state: an identity that was not reviewed can never unlock anything.
// The accepted cluster identity is required as well, because an observation
// taken against another cluster reports both objects as absent and would
// otherwise complete a deletion that never happened.
func acceptedProfileDeleteIdentities(changes []model.Change) (uid, resourceVersion, namespaceUID, clusterUID, version string, err error) {
	for _, c := range changes {
		switch c.Field {
		case profileUIDChangeField:
			uid = strings.TrimSpace(c.Before)
		case profileResourceVersionChangeField:
			resourceVersion = strings.TrimSpace(c.Before)
		case namespaceUIDChangeField:
			namespaceUID = strings.TrimSpace(c.Before)
		case clusterUIDChangeField:
			clusterUID = strings.TrimSpace(c.Before)
		case profileReconcileVersionChangeField:
			version = strings.TrimSpace(c.Before)
		}
	}
	if uid == "" || resourceVersion == "" || namespaceUID == "" {
		return "", "", "", "", "", fmt.Errorf("%w: the accepted evidence does not carry the reviewed Profile identities", ErrReconcileUnsupported)
	}
	if clusterUID == "" {
		return "", "", "", "", "", fmt.Errorf("%w: the accepted evidence does not carry the reviewed cluster identity", ErrReconcileUnsupported)
	}
	return uid, resourceVersion, namespaceUID, clusterUID, version, nil
}

// DeriveProfileDeleteOutcome decides the only supported derived outcome from
// the accepted evidence and the operator's complete observations.
//
// It refuses every case it cannot prove:
//
//   - An incomplete observation proves nothing and is refused.
//   - An object whose UID differs from the accepted UID is a different
//     object, so the deletion cannot be reconciled as this one.
//   - Observations that disagree with each other are refused.
//   - A completed deletion needs two complete observations that both show
//     the Profile and its namespace gone.
//   - A cancellation needs the durable proof that no delete request was ever
//     issued. Objects that are still present never prove that on their own.
func DeriveProfileDeleteOutcome(plan ProfileDeleteReconcilePlan, observations []ProfileDeleteObservation) (ProfileDeleteReconcileOutcome, error) {
	absent, present := 0, 0
	for _, o := range observations {
		if !o.Complete {
			return "", fmt.Errorf("%w: an observation is incomplete", ErrReconcileUnsupported)
		}
		// Every observation must come from the accepted cluster. An
		// observation taken against another cluster reports both objects as
		// absent and would falsely complete an old deletion.
		if o.ClusterUID == "" || o.ClusterUID != plan.ClusterUID {
			return "", fmt.Errorf("%w: the observation was taken against another cluster", ErrReconcileUnsupported)
		}
		if o.ProfileUID != "" && o.ProfileUID != plan.ProfileUID {
			return "", fmt.Errorf("%w: the Profile was replaced", ErrReconcileUnsupported)
		}
		if o.NamespaceUID != "" && o.NamespaceUID != plan.NamespaceUID {
			return "", fmt.Errorf("%w: the namespace was replaced", ErrReconcileUnsupported)
		}
		if o.ProfileUID == "" && o.NamespaceUID == "" {
			absent++
			continue
		}
		present++
	}
	if absent+present == 0 {
		return "", fmt.Errorf("%w: no complete observation was supplied", ErrReconcileUnsupported)
	}
	if absent > 0 && present > 0 {
		return "", fmt.Errorf("%w: the observations disagree", ErrReconcileUnsupported)
	}
	if absent > 0 {
		// The deletion completed. Two complete observations are required so
		// a single failed or stale read cannot resolve an operation.
		if absent < 2 {
			return "", fmt.Errorf("%w: two complete observations are required", ErrReconcileUnsupported)
		}
		return ProfileDeleteReconciledSucceeded, nil
	}
	// The objects are still there. Only the durable marker may prove that no
	// write was ever started; objects that merely look active never can.
	if !plan.NoWriteProven {
		return "", fmt.Errorf("%w: a Profile delete request may have been issued", ErrReconcileUnsupported)
	}
	return ProfileDeleteReconciledCancelled, nil
}

// ReconcileProfileDelete resolves one uncertain Profile deletion from the
// operator's structured evidence.
//
// It performs no cluster write of any kind: the caller supplies the
// observations, and this method revalidates them against the immutable
// acceptance evidence before anything changes. The evidence read and the
// outcome write happen in one transaction, so the derived outcome always
// matches the evidence it was derived from.
//
// The transition is private on purpose. A normal Finish still cannot resolve
// an uncertain operation, and this method still cannot resolve anything
// except a needs-review Profile deletion that holds both of its locks.
func (s *Store) ReconcileProfileDelete(ctx context.Context, req ProfileDeleteReconciliation) (model.Operation, error) {
	if req.OperationID == "" {
		return model.Operation{}, ErrNotFound
	}
	operator := strings.TrimSpace(req.Operator)
	reason := strings.TrimSpace(req.Reason)
	if operator == "" || reason == "" {
		return model.Operation{}, fmt.Errorf("%w: an offline operator identity and a reconciliation reason are required", ErrReconcileUnsupported)
	}
	// The reason limit counts Unicode code points, like the API's documented
	// maxLength and the backend's own limit. The reason contents are never
	// echoed into an error.
	if utf8.RuneCountInString(reason) > 500 {
		return model.Operation{}, fmt.Errorf("%w: the reconciliation reason exceeds 500 characters", ErrReconcileUnsupported)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Operation{}, err
	}
	defer tx.Rollback()
	plan, err := planProfileDeleteReconciliation(ctx, tx, req.OperationID)
	if err != nil {
		return model.Operation{}, err
	}
	if plan.Operation.State != "needs-review" {
		return model.Operation{}, fmt.Errorf("%w: only a needs-review operation can be reconciled", ErrReconcileUnsupported)
	}
	if plan.Operation.Action != profileDeleteKind {
		return model.Operation{}, fmt.Errorf("%w: only a Profile deletion can be reconciled", ErrReconcileUnsupported)
	}
	// Both the Profile and the namespace fence must still be held. A missing
	// fence is not something this path may repair.
	if !hasTarget(plan.ActiveTargets, "profile:"+plan.ProfileName) || !hasTarget(plan.ActiveTargets, "namespace:"+plan.ProfileName) {
		return model.Operation{}, fmt.Errorf("%w: the Profile and namespace locks are not both held", ErrReconcileUnsupported)
	}
	outcome, err := DeriveProfileDeleteOutcome(plan, req.Observations)
	if err != nil {
		return model.Operation{}, err
	}
	op := plan.Operation
	op.State = "succeeded"
	op.Step = string(ProfileDeleteReconciledSucceeded)
	op.Error = ""
	if outcome == ProfileDeleteReconciledCancelled {
		op.State = "failed"
		op.Step = string(ProfileDeleteReconciledCancelled)
		op.Error = "Offline reconciliation: no Profile deletion request was ever issued, so this operation changed nothing in the cluster."
	}
	op.UpdatedAt = time.Now().UTC()
	reconcile := &profileDeleteReconciliation{Result: string(outcome), Operator: operator, Reason: reason}
	if err = s.updateOperationTx(ctx, tx, op, model.Audit{ID: op.ID + "-reconciled", Time: op.UpdatedAt}, true, reconcile); err != nil {
		return model.Operation{}, err
	}
	if err = tx.Commit(); err != nil {
		return model.Operation{}, err
	}
	return op, nil
}

// hasTarget reports whether the exact target is held.
func hasTarget(targets []string, want string) bool {
	for _, t := range targets {
		if t == want {
			return true
		}
	}
	return false
}
