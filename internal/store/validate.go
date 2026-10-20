package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

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
	if err := validateKnownSecondaryPVLocks(ctx, q); err != nil {
		return err
	}
	return validateTargetOwnership(ctx, q)
}

// validateKnownSecondaryPVLocks uses a still-present server-owned plan as
// evidence. A plan may already have expired, in which case no secondary lock is
// invented or required. When the plan is available and names a PV for a PV
// action, the active lock must exist.
func validateKnownSecondaryPVLocks(ctx context.Context, q sqlQueryer) error {
	rows, err := q.QueryContext(ctx, `SELECT c.id,c.plan_id,p.value FROM operation_claims c
		JOIN (SELECT key,value FROM records WHERE bucket='plans' AND json_valid(value)=1) p ON p.key=c.plan_id
		WHERE c.active=1`)
	if err != nil {
		return err
	}
	type pvEvidence struct {
		operationID string
		pvName      string
	}
	var evidence []pvEvidence
	for rows.Next() {
		var operationID, planID string
		var value []byte
		if err := rows.Scan(&operationID, &planID, &value); err != nil {
			rows.Close()
			return err
		}
		var plan model.Plan
		if err := json.Unmarshal(value, &plan); err != nil || plan.ID != planID {
			continue
		}
		kind := plan.Prepared.Action.Kind
		if (kind == "storage.create.static" || kind == "pv.reserve-and-claim") && plan.Prepared.Action.PVName != "" {
			evidence = append(evidence, pvEvidence{operationID: operationID, pvName: plan.Prepared.Action.PVName})
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, e := range evidence {
		var n int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM operation_targets WHERE operation_id=? AND target=? AND active=1`, e.operationID, "pv:"+e.pvName).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return integrity("an active operation is missing a known secondary PV lock")
		}
	}
	return nil
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
