package store

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"kubeflow-admin/internal/model"
)

// redactAcceptanceChanges removes raw entered paths, backend handles and mount
// paths from the durable acceptance ledger and audit history. The transient
// preview plan keeps the administrator's reviewed values, but durable history
// carries only a stable digest with the rest of the safe, typed change summary.
//
// Safe reviewed dependency identities (resource kind/namespace/name/UID) and
// template/class/capacity facts are not touched.
func redactAcceptanceChanges(changes []model.Change) []model.Change {
	if len(changes) == 0 {
		return changes
	}
	out := make([]model.Change, len(changes))
	copy(out, changes)
	for i := range out {
		if !sensitiveChangeField(out[i].Field) {
			continue
		}
		out[i].Before = redactedChangeValue(out[i].Before)
		out[i].After = redactedChangeValue(out[i].After)
	}
	return out
}

// sensitiveChangeField recognizes only the bounded fields that may carry
// operator-entered filesystem paths or backend volume handles. It never
// redacts a whole manifest because the adapter never puts one in a change.
func sensitiveChangeField(field string) bool {
	field = strings.ToLower(strings.TrimSpace(field))
	switch field {
	case "path", "handle":
		return true
	}
	for _, suffix := range []string{" path", " handle"} {
		if strings.HasSuffix(field, suffix) {
			return true
		}
	}
	for _, fragment := range []string{"mountpath", "volumehandle", "beegfs handle", "backend path"} {
		if strings.Contains(field, fragment) {
			return true
		}
	}
	return false
}

// redactedChangeValue keeps short placeholders such as "(entered)" readable,
// and replaces every other value with a deterministic digest so repeated
// checkpoints and idempotent retries retain the same safe evidence.
func redactedChangeValue(value string) string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return value
	}
	if strings.HasPrefix(trimmed, "(") && strings.HasSuffix(trimmed, ")") {
		return value
	}
	if strings.HasPrefix(trimmed, "sha256:") {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}
