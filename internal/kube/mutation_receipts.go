package kube

import (
	"context"
	"errors"

	"k8s.io/apimachinery/pkg/types"

	"kubeflow-admin/internal/model"
)

const (
	mutationKindPersistentVolume      = "PersistentVolume"
	mutationKindPersistentVolumeClaim = "PersistentVolumeClaim"
	mutationKindPodDefault            = "PodDefault"
)

// recordMutationReceipt emits one fixed-outcome identity receipt after a write
// response or an observed completion. It refuses an empty UID/resourceVersion:
// a false receipt is worse than a needs-review outcome. Recorder failures are
// returned unchanged; callers must stop before the next write and must not mark
// the operation as rejected-before-mutation.
func recordMutationReceipt(ctx context.Context, kind, namespace, name string, uid types.UID, resourceVersion string, outcome model.MutationOutcome) error {
	if kind != mutationKindPersistentVolume && kind != mutationKindPersistentVolumeClaim && kind != mutationKindPodDefault {
		return errors.New("unsupported mutation receipt kind")
	}
	if name == "" || uid == "" || resourceVersion == "" {
		return errors.New("mutation receipt requires a stable name, UID and resourceVersion")
	}
	switch outcome {
	case model.MutationOutcomeCreated, model.MutationOutcomeReserved, model.MutationOutcomeOverwritten,
		model.MutationOutcomeDeleted, model.MutationOutcomeAvailable, model.MutationOutcomeBound:
	default:
		return errors.New("unsupported mutation receipt outcome")
	}
	return model.RecordMutationReceipt(ctx, model.MutationReceipt{
		Kind: kind, Namespace: namespace, Name: name, UID: string(uid),
		ResourceVersion: resourceVersion, Outcome: outcome,
	})
}
