package store

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"kubeflow-admin/internal/model"
)

// MaxMutationReceipts bounds the durable mutation proof attached to one
// operation. Receipts are small safe identities, never manifests or backend
// settings.
const MaxMutationReceipts = 128

// MaxMutationReceiptToken bounds each identity token a receipt may carry.
const MaxMutationReceiptToken = 256

var mutationReceiptKinds = map[string]bool{
	"PersistentVolume":      true,
	"PersistentVolumeClaim": true,
	"PodDefault":            true,
}

var mutationReceiptOutcomes = map[string]bool{
	"created":     true,
	"reserved":    true,
	"overwritten": true,
	"deleted":     true,
	"available":   true,
	"bound":       true,
}

// ValidateMutationReceipt validates one recorder-supplied receipt against the
// namespace of the reviewed action. A PersistentVolume is cluster scoped and
// must not carry a namespace; every other kind must match the action namespace.
func ValidateMutationReceipt(receipt model.MutationReceipt, namespace string) error {
	if err := validateMutationReceiptShape(receipt); err != nil {
		return err
	}
	if receipt.Kind == "PersistentVolume" {
		if receipt.Namespace != "" {
			return ErrInvalidTarget
		}
		return nil
	}
	if !validDNS1123Label(namespace) || receipt.Namespace != namespace {
		return ErrInvalidTarget
	}
	return nil
}

func validateMutationReceiptShape(receipt model.MutationReceipt) error {
	if !mutationReceiptKinds[receipt.Kind] || !mutationReceiptOutcomes[string(receipt.Outcome)] {
		return ErrInvalidTarget
	}
	if !validTargetName(receipt.Name) {
		return ErrInvalidTarget
	}
	if receipt.Kind == "PersistentVolume" {
		if receipt.Namespace != "" {
			return ErrInvalidTarget
		}
	} else if !validDNS1123Label(receipt.Namespace) {
		return ErrInvalidTarget
	}
	if !validMutationReceiptToken(receipt.UID) || !validMutationReceiptToken(receipt.ResourceVersion) {
		return ErrInvalidTarget
	}
	return nil
}

func validMutationReceiptToken(value string) bool {
	if value == "" || len(value) > MaxMutationReceiptToken || strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// validateOperationMutationReceipts validates the persisted receipt list. It
// permits a missing field for older operation JSON but rejects malformed shapes,
// overflow and exact duplicates.
func validateOperationMutationReceipts(receipts []model.MutationReceipt) error {
	if len(receipts) > MaxMutationReceipts {
		return fmt.Errorf("operation carries more than %d mutation receipts", MaxMutationReceipts)
	}
	seen := make(map[model.MutationReceipt]struct{}, len(receipts))
	for _, receipt := range receipts {
		if err := validateMutationReceiptShape(receipt); err != nil {
			return errors.New("operation carries an invalid mutation receipt")
		}
		if _, exists := seen[receipt]; exists {
			return errors.New("operation repeats a mutation receipt")
		}
		seen[receipt] = struct{}{}
	}
	return nil
}

// validateOperationMutationReceiptsForTarget additionally binds namespaced
// receipts to the canonical PVC target namespace of the operation. A
// PersistentVolume receipt remains cluster scoped.
func validateOperationMutationReceiptsForTarget(op model.Operation) error {
	if err := validateOperationMutationReceipts(op.MutationReceipts); err != nil {
		return err
	}
	namespace, namespaced := pvcNamespaceFromTarget(op.Target)
	for _, receipt := range op.MutationReceipts {
		if receipt.Kind == "PersistentVolume" {
			continue
		}
		if !namespaced || receipt.Namespace != namespace {
			return errors.New("operation carries a namespaced mutation receipt outside its target")
		}
	}
	return nil
}

// mutationReceiptPrefix reports whether candidate extends persisted without
// dropping, reordering or rewriting any earlier receipt.
func mutationReceiptPrefix(persisted, candidate []model.MutationReceipt) bool {
	if len(candidate) < len(persisted) {
		return false
	}
	for i := range persisted {
		if candidate[i] != persisted[i] {
			return false
		}
	}
	return true
}

// mutationReceiptChanges returns safe presentation changes for audit history.
// They are appended per checkpoint in addition to the immutable acceptance
// changes and never contain paths, manifests or arbitrary settings.
func mutationReceiptChanges(receipts []model.MutationReceipt) []model.Change {
	out := make([]model.Change, 0, len(receipts))
	for _, receipt := range receipts {
		identity := receipt.Name
		if receipt.Namespace != "" {
			identity = receipt.Namespace + "/" + receipt.Name
		}
		out = append(out, model.Change{
			Field:  "Mutation receipt " + receipt.Kind,
			Before: identity,
			After:  string(receipt.Outcome) + " (uid " + receipt.UID + ", resourceVersion " + receipt.ResourceVersion + ")",
		})
	}
	return out
}
