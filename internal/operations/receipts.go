package operations

import (
	"context"
	"errors"
	"strings"
	"time"

	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/store"
)

// receiptPersistTimeout bounds the durable checkpoint that records one mutation
// receipt. It deliberately uses a fresh background context: a receipt is
// recorded after a real write has already happened, so a canceled session may
// stop the next write but must not prevent preserving the proof of this one.
const receiptPersistTimeout = 5 * time.Second

// recordMutationReceipt validates, deduplicates and durably appends one
// backend-reported mutation receipt to the operation record. The caller holds
// the operation lock. No authorization is performed here and no cluster write
// is started.
func (m *Manager) recordMutationReceipt(op *model.Operation, action model.Action, receipt model.MutationReceipt) error {
	if err := store.ValidateMutationReceipt(receipt, receiptNamespace(action)); err != nil {
		return errors.New("mutation receipt rejected")
	}
	for _, existing := range op.MutationReceipts {
		if existing == receipt {
			return nil
		}
	}
	if len(op.MutationReceipts) >= store.MaxMutationReceipts {
		return errors.New("mutation receipt limit reached")
	}
	if op.State != "applying" {
		return errors.New("mutation receipt requested outside an applying operation")
	}
	auditID, err := id()
	if err != nil {
		return errors.New("mutation receipt could not be recorded")
	}
	candidate := *op
	candidate.MutationReceipts = append(append([]model.MutationReceipt(nil), op.MutationReceipts...), receipt)
	candidate.UpdatedAt = time.Now().UTC()
	// Keep the known API outcome in memory even if this checkpoint fails. The
	// failure guard still forbids the next write, but a successful needs-review
	// Finish can durably retain this exact UID instead of discarding known proof.
	// No caller observes it as durable until a Store transaction succeeds.
	*op = candidate
	persistCtx, cancel := context.WithTimeout(context.Background(), receiptPersistTimeout)
	defer cancel()
	persist := m.s.Checkpoint
	if m.persistReceipt != nil {
		persist = m.persistReceipt
	}
	if err := persist(persistCtx, candidate, model.Audit{ID: auditID}); err != nil {
		return errors.New("mutation receipt could not be persisted before the next cluster write")
	}
	return nil
}

// receiptNamespace is the namespace a namespaced receipt must match. Profile
// actions address their Profile through Name; ordinary actions use Namespace.
func receiptNamespace(action model.Action) string {
	if strings.HasPrefix(action.Kind, "profile.") {
		return action.Name
	}
	return action.Namespace
}
