package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"kubeflow-admin/internal/model"
)

func storeReceiptPVC() model.MutationReceipt {
	return model.MutationReceipt{Kind: "PersistentVolumeClaim", Namespace: "alice", Name: "data", UID: "pvc-uid", ResourceVersion: "42", Outcome: model.MutationOutcomeCreated}
}

func storeReceiptPodDefault() model.MutationReceipt {
	return model.MutationReceipt{Kind: "PodDefault", Namespace: "alice", Name: "mount-data", UID: "pd-uid", ResourceVersion: "7", Outcome: model.MutationOutcomeCreated}
}

func TestMutationReceiptPrefixIsAppendOnly(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	action := model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "data", PVName: "static-pv", TargetUID: "pv-uid", TargetResourceVersion: "3", PodDefault: podDefaultRequest("mount-data")}
	plan := provisioningPlan("receipt-prefix-plan", action)
	if err := s.Put(ctx, "plans", plan.ID, plan); err != nil {
		t.Fatal(err)
	}
	op := provisioningOp(plan, "receipt-prefix-op")
	if _, created, err := s.CreateOperation(ctx, op, plan, "receipt-prefix-key"); err != nil || !created {
		t.Fatal(err)
	}
	op.State = "applying"
	op.Step = "create-pvc"
	op.UpdatedAt = time.Now().UTC()
	first := storeReceiptPVC()
	op.MutationReceipts = []model.MutationReceipt{first}
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "receipt-prefix-cp1"}); err != nil {
		t.Fatal(err)
	}
	// Dropping proof is refused.
	dropped := op
	dropped.MutationReceipts = nil
	if err := s.Checkpoint(ctx, dropped, model.Audit{ID: "receipt-prefix-drop"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("receipt drop accepted: %v", err)
	}
	// Rewriting the same prefix position is refused.
	rewritten := op
	rewritten.MutationReceipts = []model.MutationReceipt{{Kind: "PersistentVolumeClaim", Namespace: "alice", Name: "data", UID: "replacement-uid", ResourceVersion: "99", Outcome: model.MutationOutcomeCreated}}
	if err := s.Checkpoint(ctx, rewritten, model.Audit{ID: "receipt-prefix-rewrite"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("receipt rewrite accepted: %v", err)
	}
	var stored model.Operation
	if err := s.Get(ctx, "operations", op.ID, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.MutationReceipts) != 1 || stored.MutationReceipts[0] != first {
		t.Fatalf("stored receipts changed after refused rewrite: %+v", stored.MutationReceipts)
	}
	// Appending the next receipt is allowed.
	second := storeReceiptPodDefault()
	op.MutationReceipts = []model.MutationReceipt{first, second}
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "receipt-prefix-cp2"}); err != nil {
		t.Fatal(err)
	}
	// A later finish still cannot drop the prefix.
	dropped = op
	dropped.State = "needs-review"
	dropped.MutationReceipts = []model.MutationReceipt{first}
	dropped.UpdatedAt = time.Now().UTC()
	if err := s.Finish(ctx, dropped, model.Audit{ID: "receipt-prefix-finish-drop"}); !errors.Is(err, ErrConflict) {
		t.Fatalf("finish dropped receipt proof: %v", err)
	}
	op.State = "needs-review"
	op.UpdatedAt = time.Now().UTC()
	if err := s.Finish(ctx, op, model.Audit{ID: "receipt-prefix-finish"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Get(ctx, "operations", op.ID, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.MutationReceipts) != 2 || stored.MutationReceipts[0] != first || stored.MutationReceipts[1] != second {
		t.Fatalf("final receipts = %+v", stored.MutationReceipts)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("append-only receipts failed integrity: %v", err)
	}
	// The immutable acceptance ledger is unchanged; receipt details appear only
	// in presentation audit rows.
	for _, change := range acceptanceChanges(t, s, op.ID) {
		if strings.HasPrefix(change.Field, "Mutation receipt") {
			t.Fatalf("acceptance ledger changed by receipt: %+v", change)
		}
	}
	audits, err := s.RecentAudit(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	foundPVC, foundPD := false, false
	for _, audit := range audits {
		if audit.OperationID != op.ID {
			continue
		}
		for _, change := range audit.Changes {
			if change.Field == "Mutation receipt PersistentVolumeClaim" {
				foundPVC = true
			}
			if change.Field == "Mutation receipt PodDefault" {
				foundPD = true
			}
		}
	}
	if !foundPVC || !foundPD {
		t.Fatalf("audit lacks receipt changes: pvc=%v pd=%v", foundPVC, foundPD)
	}
}

func TestOldOperationWithoutMutationReceiptsRemainsValid(t *testing.T) {
	raw := []byte(`{"id":"old-op","planID":"old-plan","actor":"issuer|admin","target":"notebook:alice/lab","action":"notebook.stop","state":"queued","step":"queued","createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}`)
	op, err := decodeOperation(raw)
	if err != nil {
		t.Fatalf("old operation without receipts failed decode: %v", err)
	}
	if len(op.MutationReceipts) != 0 {
		t.Fatalf("missing receipt field decoded as %+v", op.MutationReceipts)
	}
	s := database(t)
	ctx := context.Background()
	plan := provisioningPlan("old-receipt-plan", model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "lab"})
	created := provisioningOp(plan, "old-receipt-op")
	if _, ok, err := s.CreateOperation(ctx, created, plan, "old-receipt-key"); err != nil || !ok {
		t.Fatal(err)
	}
	var value []byte
	if err := s.db.QueryRow(`SELECT value FROM records WHERE bucket='operations' AND key=?`, created.ID).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(value), "mutationReceipts") {
		t.Fatalf("new operation serialized an empty receipt field: %s", value)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("operation without receipts failed integrity: %v", err)
	}
}

func TestMalformedMutationReceiptsFailDecodeAndIntegrity(t *testing.T) {
	base := model.Operation{ID: "receipt-shape", PlanID: "plan", Actor: "issuer|admin", Target: "notebook:alice/lab", Action: "notebook.stop", State: "queued", Step: "queued", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	cases := map[string][]model.MutationReceipt{
		"invalid outcome": {{Kind: "PersistentVolumeClaim", Namespace: "alice", Name: "data", UID: "uid", ResourceVersion: "1", Outcome: "done"}},
		"invalid kind":    {{Kind: "Secret", Namespace: "alice", Name: "data", UID: "uid", ResourceVersion: "1", Outcome: model.MutationOutcomeCreated}},
		"missing uid":     {{Kind: "PersistentVolumeClaim", Namespace: "alice", Name: "data", ResourceVersion: "1", Outcome: model.MutationOutcomeCreated}},
		"missing rv":      {{Kind: "PersistentVolumeClaim", Namespace: "alice", Name: "data", UID: "uid", Outcome: model.MutationOutcomeCreated}},
		"pv namespace":    {{Kind: "PersistentVolume", Namespace: "alice", Name: "pv", UID: "uid", ResourceVersion: "1", Outcome: model.MutationOutcomeCreated}},
		"bad pvc ns":      {{Kind: "PersistentVolumeClaim", Namespace: "Bad/NS", Name: "data", UID: "uid", ResourceVersion: "1", Outcome: model.MutationOutcomeCreated}},
		"control token":   {{Kind: "PersistentVolumeClaim", Namespace: "alice", Name: "data", UID: "u\x00id", ResourceVersion: "1", Outcome: model.MutationOutcomeCreated}},
		"duplicate":       {storeReceiptPVC(), storeReceiptPVC()},
	}
	for name, receipts := range cases {
		t.Run(name, func(t *testing.T) {
			op := base
			op.MutationReceipts = receipts
			raw, err := json.Marshal(op)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := decodeOperation(raw); err == nil {
				t.Fatal("malformed receipts decoded")
			}
		})
	}
	// A valid-shaped receipt must still bind to the operation's canonical PVC
	// target namespace.
	wrongNamespace := base
	wrongNamespace.Target = "pvc:alice/data"
	wrongNamespace.Action = "pvc.create.bound"
	wrongNamespace.MutationReceipts = []model.MutationReceipt{{Kind: "PersistentVolumeClaim", Namespace: "bob", Name: "data", UID: "uid", ResourceVersion: "1", Outcome: model.MutationOutcomeCreated}}
	raw, err := json.Marshal(wrongNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeOperation(raw); err == nil {
		t.Fatal("receipt outside the operation target namespace decoded")
	}
	// Persisted malformed proof also fails closed through CheckIntegrity.
	s := database(t)
	ctx := context.Background()
	plan := provisioningPlan("bad-receipt-plan", model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "lab"})
	op := provisioningOp(plan, "bad-receipt-op")
	if _, created, err := s.CreateOperation(ctx, op, plan, "bad-receipt-key"); err != nil || !created {
		t.Fatal(err)
	}
	bad := op
	bad.MutationReceipts = cases["duplicate"]
	if err := s.Put(ctx, "operations", bad.ID, bad); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("malformed persisted receipts passed integrity: %v", err)
	}
}

func TestTooManyMutationReceiptsFailClosed(t *testing.T) {
	receipts := make([]model.MutationReceipt, MaxMutationReceipts+1)
	for i := range receipts {
		receipts[i] = model.MutationReceipt{Kind: "PodDefault", Namespace: "alice", Name: "mount-" + string(rune('a'+i%26)), UID: "uid", ResourceVersion: "1", Outcome: model.MutationOutcomeCreated}
	}
	if err := validateOperationMutationReceipts(receipts); err == nil {
		t.Fatal("oversized receipt list passed validation")
	}
}

func TestMutationReceiptsSurviveAuditPruneAndCompaction(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	action := model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "data", PVName: "static-pv", TargetUID: "pv-uid", TargetResourceVersion: "3"}
	plan := provisioningPlan("receipt-retention-plan", action)
	op := provisioningOp(plan, "receipt-retention-op")
	if _, created, err := s.CreateOperation(ctx, op, plan, "receipt-retention-key"); err != nil || !created {
		t.Fatal(err)
	}
	op.State = "applying"
	op.Step = "create-pvc"
	receipt := storeReceiptPVC()
	op.MutationReceipts = []model.MutationReceipt{receipt}
	op.UpdatedAt = time.Now().UTC()
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "receipt-retention-cp"}); err != nil {
		t.Fatal(err)
	}
	op.State = "succeeded"
	op.Step = "verified"
	op.UpdatedAt = time.Now().UTC()
	if err := s.Finish(ctx, op, model.Audit{ID: "receipt-retention-final"}); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-100 * 24 * time.Hour).UTC()
	backdate(t, s, "operations", op.ID, old)
	backdateOperationAudit(t, s, op.ID, old)
	backdateCompaction(t, s, op.ID, old)
	if _, err := s.CleanupHistory(ctx, HistoryPolicy{RetentionDays: 90}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var tombstone model.Operation
	if err := s.Get(ctx, "operations", op.ID, &tombstone); err != nil {
		t.Fatal(err)
	}
	if !tombstone.HistoryExpired || len(tombstone.MutationReceipts) != 1 || tombstone.MutationReceipts[0] != receipt {
		t.Fatalf("receipt did not survive audit prune/compaction: %+v", tombstone)
	}
	var auditRows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM records WHERE bucket='audit' AND json_extract(value,'$.operationID')=?`, op.ID).Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if auditRows != 0 {
		t.Fatalf("expired audit rows remain: %d", auditRows)
	}
	var changes []byte
	if err := s.db.QueryRow(`SELECT changes FROM operation_acceptance WHERE operation_id=?`, op.ID).Scan(&changes); err != nil {
		t.Fatal(err)
	}
	if string(changes) != "[]" {
		t.Fatalf("compacted acceptance was not redacted: %s", changes)
	}
}
