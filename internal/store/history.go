package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"kubeflow-admin/internal/model"
)

// Retention and presentation bounds. These mirror the conservative config
// defaults, but the store applies them even when a caller passes a zero-valued
// policy (for example a test constructing config.Config directly).
const (
	DefaultRetentionDays             = 90
	DefaultMaxAuditRecords           = 50000
	DefaultMaxAuditBytes       int64 = 64 << 20
	DefaultMaxOperationRecords       = 100000

	// MaxUnresolvedOperations bounds queued/applying/needs-review claims. All of
	// them are returned by the operations list so an old unresolved item cannot
	// fall behind the recent window.
	MaxUnresolvedOperations = 1000

	// RecentHistoryLimit is the hard number of ordinary list rows. Audit is not
	// expanded with every checkpoint of every active operation.
	RecentHistoryLimit = 1000

	// DefaultCleanupTimeout caps one bounded retention transaction. Callers may
	// pass a shorter deadline, never a longer effective one.
	DefaultCleanupTimeout = 500 * time.Millisecond

	retentionBatchAudit = 500
	// Keep the write batch comfortably inside DefaultCleanupTimeout even under
	// race-detector or slow-storage load. A timed-out transaction rolls back, so
	// an oversized batch could otherwise prevent retention progress entirely.
	retentionBatchOperations = 50
)

// ErrHistoryBudget means accepting another mutation would exceed a configured
// safety or history budget. It is separate from a target conflict: the request
// itself may be valid but durable evidence cannot be kept safely.
var ErrHistoryBudget = errors.New("history storage budget exhausted")

// BudgetError is a safe, actionable budget failure. It never includes record
// contents, identities or raw driver text.
type BudgetError struct {
	Reason string
}

func (e *BudgetError) Error() string {
	return ErrHistoryBudget.Error() + ": " + e.Reason + "; reconcile unresolved operations or raise the configured history limit before retrying"
}
func (e *BudgetError) Unwrap() error { return ErrHistoryBudget }

func budget(reason string) error { return &BudgetError{Reason: reason} }

// HistoryPolicy controls logical retention. It does not cap SQLite/WAL file
// size; operators still need disk quotas and backup retention.
type HistoryPolicy struct {
	RetentionDays       int
	MaxAuditRecords     int
	MaxAuditBytes       int64
	MaxOperationRecords int
}

// WithDefaults applies the shipped conservative limits to zero values. A
// low-level caller cannot widen the user-visible retention window beyond 90
// days; configuration validation already enforces the same upper bound.
func (p HistoryPolicy) WithDefaults() HistoryPolicy {
	if p.RetentionDays <= 0 || p.RetentionDays > DefaultRetentionDays {
		p.RetentionDays = DefaultRetentionDays
	}
	if p.MaxAuditRecords <= 0 {
		p.MaxAuditRecords = DefaultMaxAuditRecords
	}
	if p.MaxAuditBytes <= 0 {
		p.MaxAuditBytes = DefaultMaxAuditBytes
	}
	if p.MaxOperationRecords <= 0 {
		p.MaxOperationRecords = DefaultMaxOperationRecords
	}
	return p
}

// HistoryStats is the honest, bounded view of retained state. Logical row and
// byte budgets do not cap physical database/WAL files, and the safety ledger
// and operation tombstones remain after presentation history expires.
type HistoryStats struct {
	RetentionDays       int       `json:"retentionDays"`
	RetentionCutoff     time.Time `json:"retentionCutoff"`
	MaxAuditRecords     int       `json:"maxAuditRecords"`
	MaxAuditBytes       int64     `json:"maxAuditBytes"`
	MaxOperationRecords int       `json:"maxOperationRecords"`
	MaxUnresolved       int       `json:"maxUnresolvedOperations"`
	RecentRecordLimit   int       `json:"recentRecordLimit"`

	AuditRecords           int       `json:"auditRecords"`
	AuditBytes             int64     `json:"auditBytes"`
	UnresolvedAuditRecords int       `json:"unresolvedAuditRecords"`
	UnresolvedAuditBytes   int64     `json:"unresolvedAuditBytes"`
	OldestAudit            time.Time `json:"oldestAudit,omitempty"`
	NewestAudit            time.Time `json:"newestAudit,omitempty"`
	AuditTruncated         bool      `json:"auditTruncated"`

	OperationClaims         int   `json:"operationClaims"`
	OperationRecords        int   `json:"operationRecords"`
	UnresolvedOperations    int   `json:"unresolvedOperations"`
	NeedsReviewOperations   int   `json:"needsReviewOperations"`
	CompactedOperations     int   `json:"compactedOperations"`
	TombstoneBytes          int64 `json:"tombstoneBytes"`
	AcceptanceLedgerRecords int   `json:"acceptanceLedgerRecords"`
	AcceptanceLedgerBytes   int64 `json:"acceptanceLedgerBytes"`
}

// HistoryResult reports what one bounded cleanup actually removed or redacted.
// It deliberately omits whole-database statistics: those belong on a read-only
// stats path, not inside the short retention transaction.
type HistoryResult struct {
	PurgedExpired            int `json:"purgedExpired"`
	PurgedUnderPressure      int `json:"purgedUnderPressure"`
	CompactedOperations      int `json:"compactedOperations"`
	RedactedAcceptanceLedger int `json:"redactedAcceptanceLedger"`
}

// CheckAdmission is an early, non-authoritative check for callers that want to
// reject obviously full budgets before preparing a plan. The acceptance
// transaction (CreateOperationWithPolicy) repeats all checks, including the
// serialized size of the new audit row, so races cannot overrun a budget.
func (s *Store) CheckAdmission(ctx context.Context, policy HistoryPolicy) error {
	p := policy.WithDefaults()
	var claims, active int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(active),0) FROM operation_claims`).Scan(&claims, &active); err != nil {
		return err
	}
	if claims >= p.MaxOperationRecords {
		return budget(fmt.Sprintf("retained operation records reached the configured maximum of %d", p.MaxOperationRecords))
	}
	if active >= MaxUnresolvedOperations {
		return budget(fmt.Sprintf("unresolved operations reached the safety limit of %d", MaxUnresolvedOperations))
	}
	var auditCount int
	var auditBytes int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(LENGTH(value)),0) FROM records WHERE bucket='audit'`).Scan(&auditCount, &auditBytes); err != nil {
		return err
	}
	if auditCount >= p.MaxAuditRecords || auditBytes >= p.MaxAuditBytes {
		return budget("audit history budget is full; bounded background retention must prune before another mutation is accepted")
	}
	return nil
}

// CleanupHistory runs two independent, bounded transactions: presentation-audit
// pruning and completed-operation compaction. A failure or timeout in compaction
// cannot roll back audit progress (and vice versa). Each transaction is capped at
// DefaultCleanupTimeout.
func (s *Store) CleanupHistory(ctx context.Context, policy HistoryPolicy, now time.Time) (HistoryResult, error) {
	p := policy.WithDefaults()
	now = now.UTC()
	auditCtx, auditCancel := cleanupContext(ctx)
	auditResult, auditErr := s.cleanupAudit(auditCtx, p, now)
	auditCancel()
	opCtx, opCancel := cleanupContext(ctx)
	opResult, opErr := s.cleanupOperations(opCtx, p, now)
	opCancel()
	return HistoryResult{
		PurgedExpired:            auditResult.PurgedExpired,
		PurgedUnderPressure:      auditResult.PurgedUnderPressure,
		CompactedOperations:      opResult.CompactedOperations,
		RedactedAcceptanceLedger: opResult.RedactedAcceptanceLedger,
	}, errors.Join(auditErr, opErr)
}

func (s *Store) cleanupAudit(ctx context.Context, p HistoryPolicy, now time.Time) (HistoryResult, error) {
	result := HistoryResult{}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	// Safety evidence must exist for every active/unresolved claim before its
	// visible audit presentation rows can be discarded.
	if err := checkActiveAcceptance(ctx, tx); err != nil {
		return result, err
	}
	cutoff := now.Add(-time.Duration(p.RetentionDays) * 24 * time.Hour)
	cutoffNano := cutoff.UnixNano()
	expired, err := selectExpiredAudit(ctx, tx, cutoffNano, retentionBatchAudit)
	if err != nil {
		return result, err
	}
	if len(expired) > 0 {
		n, err := deleteAuditRows(ctx, tx, expired)
		if err != nil {
			return result, err
		}
		result.PurgedExpired = int(n)
	} else {
		count, bytes, err := auditUsage(ctx, tx)
		if err != nil {
			return result, err
		}
		targetRecords, targetBytes := auditPressureTargets(p)
		if count > targetRecords || bytes > targetBytes {
			rows, err := selectPressureAudit(ctx, tx, count, bytes, targetRecords, targetBytes, retentionBatchAudit)
			if err != nil {
				return result, err
			}
			n, err := deleteAuditRows(ctx, tx, rows)
			if err != nil {
				return result, err
			}
			result.PurgedUnderPressure = int(n)
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}

func cleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) <= DefaultCleanupTimeout {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, DefaultCleanupTimeout)
}

type auditRow struct {
	key   string
	value []byte
	size  int64
}

func selectExpiredAudit(ctx context.Context, tx *sql.Tx, before int64, limit int) ([]auditRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT key,value,LENGTH(value) FROM records WHERE bucket='audit' AND updated<? ORDER BY updated ASC,key ASC LIMIT ?`, before, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.key, &r.value, &r.size); err != nil {
			return nil, err
		}
		if err := validateAuditRow(r.value); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func selectPressureAudit(ctx context.Context, tx *sql.Tx, count int, bytes int64, maxRecords int, maxBytes int64, limit int) ([]auditRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT key,value,LENGTH(value) FROM records WHERE bucket='audit' ORDER BY updated ASC,key ASC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []auditRow
	removeBytes := int64(0)
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.key, &r.value, &r.size); err != nil {
			return nil, err
		}
		if err := validateAuditRow(r.value); err != nil {
			return nil, err
		}
		out = append(out, r)
		removeBytes += r.size
		if count-len(out) <= maxRecords && bytes-removeBytes <= maxBytes {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func validateAuditRow(value []byte) error {
	_, err := decodeAudit(value)
	return err
}

func deleteAuditRows(ctx context.Context, tx *sql.Tx, rows []auditRow) (int64, error) {
	if len(rows) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(rows)), ",")
	args := make([]any, len(rows))
	for i, row := range rows {
		args[i] = row.key
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM records WHERE bucket='audit' AND key IN (`+placeholders+`)`, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// auditPressureTargets reserves a small admission headroom below the configured
// maximum. Without it, a cleanup that stops exactly at the budget would reject
// every new mutation forever because the next accepted audit row needs one more
// slot. The byte reserve is capped for the small test-sized budgets.
func auditPressureTargets(p HistoryPolicy) (int, int64) {
	records := p.MaxAuditRecords
	if records > 1 {
		records--
	}
	bytes := p.MaxAuditBytes
	reserve := int64(1 << 20)
	if max := bytes / 4; max < reserve {
		reserve = max
	}
	if bytes > reserve {
		bytes -= reserve
	} else if bytes > 1 {
		bytes = bytes * 3 / 4
	}
	return records, bytes
}

func auditUsage(ctx context.Context, tx *sql.Tx) (int, int64, error) {
	var count int
	var bytes int64
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(LENGTH(value)),0) FROM records WHERE bucket='audit'`).Scan(&count, &bytes)
	return count, bytes, err
}

// cleanupOperations processes one bounded batch from the indexed compaction
// queue. It never scans a compacted prefix and never touches an active or
// needs-review claim. Invalid rows fail closed for that row while other valid
// queued operations in the batch can still make progress; the transaction is
// independent from audit pruning.
func (s *Store) cleanupOperations(ctx context.Context, p HistoryPolicy, now time.Time) (HistoryResult, error) {
	result := HistoryResult{}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	cutoff := now.Add(-time.Duration(p.RetentionDays) * 24 * time.Hour).UnixNano()
	rows, err := tx.QueryContext(ctx, `SELECT q.operation_id,q.updated,c.active,r.key,r.value
		FROM operation_compaction q
		LEFT JOIN operation_claims c ON c.id=q.operation_id
		LEFT JOIN records r ON r.bucket='operations' AND r.key=q.operation_id
		WHERE q.updated<=?
		ORDER BY q.updated,q.operation_id LIMIT ?`, cutoff, retentionBatchOperations)
	if err != nil {
		return result, err
	}
	type queued struct {
		id     string
		active sql.NullInt64
		key    sql.NullString
		value  []byte
	}
	var selected []queued
	for rows.Next() {
		var q queued
		var updated int64
		if err := rows.Scan(&q.id, &updated, &q.active, &q.key, &q.value); err != nil {
			rows.Close()
			return result, err
		}
		selected = append(selected, q)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return result, err
	}
	rows.Close()
	var firstErr error
	for _, q := range selected {
		if !q.active.Valid || q.active.Int64 != 0 {
			if firstErr == nil {
				firstErr = integrity("compaction queue references a non-terminal operation")
			}
			continue
		}
		if !q.key.Valid {
			if firstErr == nil {
				firstErr = integrity("compaction queue references a missing operation record")
			}
			continue
		}
		op, err := decodeOperation(q.value)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if op.ID != q.id {
			if firstErr == nil {
				firstErr = integrity("compaction queue record identity does not match its operation")
			}
			continue
		}
		// Only definitively completed work may be compacted/redacted. A
		// needs-review operation retains its full evidence even if its claim is
		// unexpectedly inactive.
		if op.State != "succeeded" && op.State != "failed" {
			if firstErr == nil {
				firstErr = integrity("compaction queue contains an operation that must remain protected")
			}
			continue
		}
		if !op.HistoryExpired {
			compact := model.Operation{
				ID:               op.ID,
				PlanID:           op.PlanID,
				Actor:            op.Actor,
				ActorDisplay:     op.ActorDisplay,
				HistoryExpired:   true,
				Target:           op.Target,
				Action:           op.Action,
				State:            op.State,
				MutationReceipts: append([]model.MutationReceipt(nil), op.MutationReceipts...),
				CreatedAt:        op.CreatedAt,
				UpdatedAt:        op.UpdatedAt,
			}
			value, err := json.Marshal(compact)
			if err != nil {
				return result, err
			}
			// Preserve the original updated timestamp: compaction is not a new
			// event and must not move a tombstone into the recent window.
			if _, err := tx.ExecContext(ctx, `UPDATE records SET value=? WHERE bucket='operations' AND key=?`, value, q.id); err != nil {
				return result, err
			}
			result.CompactedOperations++
		}
		res, err := tx.ExecContext(ctx, `UPDATE operation_acceptance SET reason='',changes='[]' WHERE operation_id=?`, q.id)
		if err != nil {
			return result, err
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			result.RedactedAcceptanceLedger++
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM operation_compaction WHERE operation_id=?`, q.id); err != nil {
			return result, err
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, firstErr
}

// HistoryStats reports current logical usage and the residual safety evidence.
func (s *Store) HistoryStats(ctx context.Context, policy HistoryPolicy) (HistoryStats, error) {
	p := policy.WithDefaults()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return HistoryStats{}, err
	}
	defer tx.Rollback()
	stats, err := historyStats(ctx, tx, p, time.Now().UTC())
	if err != nil {
		return HistoryStats{}, err
	}
	return stats, tx.Commit()
}

func historyStats(ctx context.Context, tx *sql.Tx, p HistoryPolicy, now time.Time) (HistoryStats, error) {
	stats := HistoryStats{
		RetentionDays:       p.RetentionDays,
		RetentionCutoff:     now.Add(-time.Duration(p.RetentionDays) * 24 * time.Hour),
		MaxAuditRecords:     p.MaxAuditRecords,
		MaxAuditBytes:       p.MaxAuditBytes,
		MaxOperationRecords: p.MaxOperationRecords,
		MaxUnresolved:       MaxUnresolvedOperations,
		RecentRecordLimit:   RecentHistoryLimit,
	}
	var oldest, newest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(LENGTH(value)),0),MIN(updated),MAX(updated) FROM records WHERE bucket='audit'`).Scan(&stats.AuditRecords, &stats.AuditBytes, &oldest, &newest); err != nil {
		return stats, err
	}
	if oldest.Valid {
		stats.OldestAudit = time.Unix(0, oldest.Int64).UTC()
	}
	if newest.Valid {
		stats.NewestAudit = time.Unix(0, newest.Int64).UTC()
	}
	stats.AuditTruncated = stats.AuditRecords > RecentHistoryLimit || oldest.Valid && stats.OldestAudit.Before(stats.RetentionCutoff)
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(LENGTH(a.value)),0) FROM (SELECT key,value FROM records WHERE bucket='audit' AND json_valid(value)=1) a JOIN operation_claims c ON c.active=1 AND (c.id=json_extract(a.value,'$.operationID') OR c.id||'-accepted'=a.key)`).Scan(&stats.UnresolvedAuditRecords, &stats.UnresolvedAuditBytes); err != nil {
		return stats, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(active),0) FROM operation_claims`).Scan(&stats.OperationClaims, &stats.UnresolvedOperations); err != nil {
		return stats, err
	}
	var operationBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(LENGTH(value)),0) FROM records WHERE bucket='operations'`).Scan(&stats.OperationRecords, &operationBytes); err != nil {
		return stats, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM records WHERE bucket='operations' AND json_valid(value)=1 AND COALESCE(json_extract(value,'$.historyExpired'),0)=1`).Scan(&stats.CompactedOperations); err != nil {
		return stats, err
	}
	// TombstoneBytes reports only compact tombstones, not all operation detail.
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(LENGTH(value)),0) FROM records WHERE bucket='operations' AND json_valid(value)=1 AND COALESCE(json_extract(value,'$.historyExpired'),0)=1`).Scan(&stats.TombstoneBytes); err != nil {
		return stats, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_claims c JOIN records r ON r.bucket='operations' AND r.key=c.id WHERE c.active=1 AND json_valid(r.value)=1 AND json_extract(r.value,'$.state')='needs-review'`).Scan(&stats.NeedsReviewOperations); err != nil {
		return stats, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(LENGTH(actor)+LENGTH(actor_display)+LENGTH(plan_id)+LENGTH(target)+LENGTH(action)+LENGTH(reason)+LENGTH(changes)),0) FROM operation_acceptance`).Scan(&stats.AcceptanceLedgerRecords, &stats.AcceptanceLedgerBytes); err != nil {
		return stats, err
	}
	return stats, nil
}

// RecentOperations returns at most limit ordinary operations plus every
// unresolved (active) operation, deduplicated and newest first. The unresolved
// read is explicitly limited to one row beyond the safety bound; a malformed
// legacy database with thousands of active claims fails closed instead of
// allocating an unbounded response.
func (s *Store) RecentOperations(ctx context.Context, limit int) ([]model.Operation, error) {
	if limit < 1 || limit > RecentHistoryLimit {
		limit = RecentHistoryLimit
	}
	recent, err := readOperations(ctx, s.db, `SELECT value FROM records WHERE bucket='operations' ORDER BY updated DESC,key DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	active, err := readOperations(ctx, s.db, `SELECT r.value FROM records r JOIN operation_claims c ON c.id=r.key WHERE r.bucket='operations' AND c.active=1 ORDER BY r.updated DESC,r.key DESC LIMIT ?`, MaxUnresolvedOperations+1)
	if err != nil {
		return nil, err
	}
	if len(active) > MaxUnresolvedOperations {
		return nil, budget(fmt.Sprintf("unresolved operations exceed the safety limit of %d", MaxUnresolvedOperations))
	}
	return mergeOperations(recent, active), nil
}

// RecentAudit returns at most limit audit rows inside the default retention
// window. It never appends every checkpoint of active operations.
func (s *Store) RecentAudit(ctx context.Context, limit int) ([]model.Audit, error) {
	return s.RecentAuditWithPolicy(ctx, HistoryPolicy{}, limit)
}

// RecentAuditWithPolicy applies the configured retention cutoff at read time, so
// janitor lag cannot expose audit rows older than the user-visible window. The
// result is hard-bounded by limit (at most RecentHistoryLimit).
func (s *Store) RecentAuditWithPolicy(ctx context.Context, policy HistoryPolicy, limit int) ([]model.Audit, error) {
	return s.recentAudit(ctx, policy, time.Now().UTC(), limit)
}

func (s *Store) recentAudit(ctx context.Context, policy HistoryPolicy, now time.Time, limit int) ([]model.Audit, error) {
	if limit < 1 || limit > RecentHistoryLimit {
		limit = RecentHistoryLimit
	}
	p := policy.WithDefaults()
	cutoff := now.UTC().Add(-time.Duration(p.RetentionDays) * 24 * time.Hour).UnixNano()
	// Apply the durable write-time window at read time so janitor lag cannot
	// expose old rows. Selected malformed rows still fail closed in readAudit.
	return readAudit(ctx, s.db, `SELECT value FROM records WHERE bucket='audit' AND updated>=? ORDER BY updated DESC,key DESC LIMIT ?`, cutoff, limit)
}

func readOperations(ctx context.Context, db *sql.DB, query string, args ...any) ([]model.Operation, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Operation{}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var op model.Operation
		if op, err = decodeOperation(b); err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}

func readAudit(ctx context.Context, db *sql.DB, query string, args ...any) ([]model.Audit, error) {
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []model.Audit{}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var a model.Audit
		if a, err = decodeAudit(b); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func mergeOperations(recent, active []model.Operation) []model.Operation {
	seen := make(map[string]bool, len(recent)+len(active))
	out := make([]model.Operation, 0, len(recent)+len(active))
	for _, list := range [][]model.Operation{recent, active} {
		for _, op := range list {
			if seen[op.ID] {
				continue
			}
			seen[op.ID] = true
			out = append(out, op)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].UpdatedAt.Equal(out[j].UpdatedAt) {
			return out[i].ID > out[j].ID
		}
		return out[i].UpdatedAt.After(out[j].UpdatedAt)
	})
	return out
}
