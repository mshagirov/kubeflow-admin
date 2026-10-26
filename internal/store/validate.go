package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"

	"kubeflow-admin/internal/model"
)

// decodeAudit validates the fields that the history UI and retention logic rely
// on. Historical rows without actorDisplay remain supported. It never returns
// raw record bytes in its error.
func decodeAudit(value []byte) (model.Audit, error) {
	var audit model.Audit
	if err := json.Unmarshal(value, &audit); err != nil {
		return audit, integrity("audit record is not readable JSON")
	}
	if audit.ID == "" || audit.Actor == "" || audit.Action == "" || audit.Target == "" || audit.Result == "" || audit.Time.IsZero() {
		return audit, integrity("audit record is missing required fields")
	}
	for _, change := range audit.Changes {
		if change.Field == "" {
			return audit, integrity("audit change record is missing required fields")
		}
	}
	return audit, nil
}

// decodeStoredAudit is the compatibility check for generic Store.Get callers.
// Some non-history callers persist an audit-shaped marker with only identity and
// time. History APIs and retention use the strict decodeAudit above.
func decodeStoredAudit(value []byte) (model.Audit, error) {
	var audit model.Audit
	if err := json.Unmarshal(value, &audit); err != nil {
		return audit, integrity("audit record is not readable JSON")
	}
	if audit.ID == "" || audit.Time.IsZero() {
		return audit, integrity("audit record is missing required fields")
	}
	present := 0
	for _, field := range []string{audit.Actor, audit.Action, audit.Target, audit.Result} {
		if field != "" {
			present++
		}
	}
	if present != 0 && present != 4 {
		return audit, integrity("audit record is missing required fields")
	}
	for _, change := range audit.Changes {
		if change.Field == "" {
			return audit, integrity("audit change record is missing required fields")
		}
	}
	return audit, nil
}

// decodeOperation validates operation identity and lifecycle fields before they
// are exposed, retried or used for a checkpoint. ActorDisplay is optional for
// backward compatibility.
func decodeOperation(value []byte) (model.Operation, error) {
	var op model.Operation
	if err := json.Unmarshal(value, &op); err != nil {
		return op, integrity("operation record is not readable JSON")
	}
	if op.ID == "" || op.PlanID == "" || op.Actor == "" || op.Action == "" || op.Target == "" || op.State == "" || op.CreatedAt.IsZero() || op.UpdatedAt.IsZero() {
		return op, integrity("operation record is missing required fields")
	}
	switch op.State {
	case "queued", "applying", "succeeded", "failed", "needs-review":
	default:
		return op, integrity("operation record has an unknown state")
	}
	// Older records have no mutationReceipts field at all; a nil list remains
	// valid. Present proofs must be bounded, structurally safe and bound to the
	// operation's canonical namespaced target where applicable.
	if err := validateOperationMutationReceiptsForTarget(op); err != nil {
		return op, integrity("operation record has invalid mutation receipts")
	}
	return op, nil
}

func decodeChanges(value []byte) ([]model.Change, error) {
	if len(value) == 0 {
		return nil, nil
	}
	var changes []model.Change
	if err := json.Unmarshal(value, &changes); err != nil {
		return nil, integrity("acceptance changes are not readable JSON")
	}
	for _, change := range changes {
		if change.Field == "" {
			return nil, integrity("acceptance change record is missing required fields")
		}
	}
	return changes, nil
}

type sqlQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// validateActiveOperations reads every active claim's operation with typed
// decoding so null/empty/unknown-state/mismatched records fail closed before a
// later raw JSON error reaches a request or recovery path.
func validateActiveOperations(ctx context.Context, q sqlQueryer) error {
	rows, err := q.QueryContext(ctx, `SELECT c.id,c.actor,c.plan_id,c.target,r.key,r.value
		FROM operation_claims c LEFT JOIN records r ON r.bucket='operations' AND r.key=c.id
		WHERE c.active=1`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var claimID, actor, planID, target string
		var recordKey sql.NullString
		var value []byte
		if err := rows.Scan(&claimID, &actor, &planID, &target, &recordKey, &value); err != nil {
			return err
		}
		if !recordKey.Valid {
			return integrity("an active operation claim has no operation record")
		}
		op, err := decodeOperation(value)
		if err != nil {
			return err
		}
		if op.ID != claimID || op.Actor != actor || op.PlanID != planID || op.Target != target {
			return integrity("an active operation record conflicts with its claim")
		}
	}
	return rows.Err()
}

// validatePrimaryTargets ensures every active claim owns an active lock on its
// canonical primary target. Secondary PV locks are validated only through the
// evidence actually present (via the converse ownership check), never invented.
func validatePrimaryTargets(ctx context.Context, q sqlQueryer) error {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_claims c
		LEFT JOIN operation_targets t ON t.operation_id=c.id AND t.target=c.target AND t.active=1
		WHERE c.active=1 AND t.operation_id IS NULL`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return integrity(fmt.Sprintf("%d active operation claim(s) have no active primary target lock", n))
	}
	return nil
}

func validateTargetOwnership(ctx context.Context, q sqlQueryer) error {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_targets t
		LEFT JOIN operation_claims c ON c.id=t.operation_id
		WHERE t.active=1 AND (c.id IS NULL OR c.active!=1)`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return integrity(fmt.Sprintf("%d active target lock(s) have no active operation claim", n))
	}
	return nil
}

func validateActiveHistory(ctx context.Context, q sqlQueryer) error {
	if err := validateActiveOperations(ctx, q); err != nil {
		return err
	}
	if err := checkActiveAcceptance(ctx, q); err != nil {
		return err
	}
	if err := validatePrimaryTargets(ctx, q); err != nil {
		return err
	}
	if err := validateKnownDependencyLocks(ctx, q); err != nil {
		return err
	}
	return validateTargetOwnership(ctx, q)
}

// validateKnownDependencyLocks validates the declared secondary lock set of
// every active operation whose plan record still exists. A plan may already
// have expired; then no secondary lock is invented or required. A present but
// unreadable/malformed plan is an integrity failure, never an empty set.
//
// New provisioning actions and reviewed cleanup use an exact declared set,
// including the namespace fence, so an extra stale PV/PodDefault lock is also
// detected. Legacy actions keep the historical subset rule so their legitimate
// old lock shapes remain compatible.
func validateKnownDependencyLocks(ctx context.Context, q sqlQueryer) error {
	rows, err := q.QueryContext(ctx, `SELECT c.id,c.plan_id,p.value FROM operation_claims c
		LEFT JOIN records p ON p.bucket='plans' AND p.key=c.plan_id
		WHERE c.active=1`)
	if err != nil {
		return err
	}
	type planEvidence struct {
		operationID string
		plan        model.Plan
	}
	var evidence []planEvidence
	for rows.Next() {
		var operationID, planID string
		var value []byte
		if err := rows.Scan(&operationID, &planID, &value); err != nil {
			rows.Close()
			return err
		}
		if value == nil {
			// The preview plan was cleaned up; durable locks remain, but the
			// declared dependency set is no longer available to re-derive.
			continue
		}
		var plan model.Plan
		if err := json.Unmarshal(value, &plan); err != nil || plan.ID != planID {
			rows.Close()
			return integrity("an active operation has an unreadable plan record")
		}
		evidence = append(evidence, planEvidence{operationID: operationID, plan: plan})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, e := range evidence {
		expected, err := targets(e.plan)
		if err != nil {
			return integrity("an active operation plan carries unsafe dependency evidence")
		}
		if strictDependencyLocks(e.plan.Prepared.Action) {
			actual, err := activeOperationTargets(ctx, q, e.operationID)
			if err != nil {
				return err
			}
			if !sameTargetSet(actual, expected) {
				return integrity("an active operation holds a secondary dependency lock set that does not match its plan")
			}
			continue
		}
		// Legacy actions preserve their historical shape: every declared lock
		// must exist, but an unrelated extra lock is not newly rejected.
		for _, target := range expected {
			var n int
			if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_targets WHERE operation_id=? AND target=? AND active=1`, e.operationID, target).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return integrity("an active operation is missing a known secondary dependency lock")
			}
		}
	}
	return nil
}

// strictDependencyLocks reports whether the plan's complete target set is
// declared and must match exactly. Legacy actions without new dependency
// fields keep the subset rule.
func strictDependencyLocks(a model.Action) bool {
	switch a.Kind {
	case "pv.create.static", "pvc.create.bound", "pv.reclaim":
		return true
	case "pvc.create.dynamic":
		return a.PodDefault != nil
	case pvcDeleteKind:
		return a.ConfirmPodDefaultCleanup || a.ConfirmPVReclaim
	}
	return false
}

func activeOperationTargets(ctx context.Context, q sqlQueryer, operationID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT target FROM operation_targets WHERE operation_id=? AND active=1 ORDER BY target`, operationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return nil, err
		}
		out = append(out, target)
	}
	return out, rows.Err()
}

func sameTargetSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	sortedA := append([]string(nil), a...)
	sortedB := append([]string(nil), b...)
	sort.Strings(sortedA)
	sort.Strings(sortedB)
	for i := range sortedA {
		if sortedA[i] != sortedB[i] {
			return false
		}
	}
	return true
}

// checkActiveAcceptance verifies that every active claim has a ledger row whose
// authority metadata matches the claim and operation.
func checkActiveAcceptance(ctx context.Context, q sqlQueryer) error {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_claims c LEFT JOIN operation_acceptance a ON a.operation_id=c.id WHERE c.active=1 AND a.operation_id IS NULL`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return integrity(fmt.Sprintf("%d active operation claim(s) are missing their immutable acceptance evidence", n))
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_claims c JOIN operation_acceptance a ON a.operation_id=c.id WHERE c.active=1 AND (a.actor!=c.actor OR a.plan_id!=c.plan_id OR a.target!=c.target)`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return integrity(fmt.Sprintf("%d active operation claim(s) have acceptance evidence that conflicts with the claim", n))
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_claims c JOIN operation_acceptance a ON a.operation_id=c.id JOIN (SELECT key,value FROM records WHERE bucket='operations' AND json_valid(value)=1) r ON r.key=c.id WHERE c.active=1 AND (a.action!=json_extract(r.value,'$.action') OR a.target!=json_extract(r.value,'$.target'))`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return integrity(fmt.Sprintf("%d active operation claim(s) have acceptance evidence that conflicts with the operation record", n))
	}
	return nil
}
