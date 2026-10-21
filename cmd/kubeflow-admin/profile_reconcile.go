package main

// profile_reconcile.go implements the offline reconciliation of one uncertain
// Profile deletion.
//
// A Profile deletion that reached an ambiguous outcome keeps its Profile and
// namespace locks until an administrator resolves it. This file is the only
// supported way to resolve one, and it is deliberately narrow:
//
//   - It is a CLI mode, never an HTTP endpoint. The web layer does not
//     expose it and the API does not describe it.
//   - The application must be stopped. The store's exclusive process lock
//     refuses to open a database that a running application owns.
//   - It never writes to the cluster. It reads the Profile, the namespace it
//     owns and the kube-system namespace, and nothing else.
//   - It uses the application's own configuration, so the same explicit
//     kubeconfig selection applies. The cluster is never guessed from the
//     current context or from a default.
//   - The default is a dry run. Only -apply changes durable state, and the
//     dry run never releases a lock.
//
// The cluster identity is proved, not assumed. The accepted deletion records
// the UID of the kube-system namespace of the cluster it was prepared in.
// Every observation is taken from the selected cluster and must carry that
// same UID. Without this proof an observation taken against a different
// cluster would report both objects as absent and would falsely complete a
// deletion that never happened.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/kube"
	"kubeflow-admin/internal/store"
)

// clusterIdentityNamespace is the namespace whose UID identifies a cluster.
// Its UID is stable for the life of the cluster and is recorded with every
// accepted Profile deletion.
const clusterIdentityNamespace = "kube-system"

// profileReconcileObservationInterval separates the two observations. Two
// independent rounds keep a single stale, failed or wrong-cluster read from
// resolving an operation.
var profileReconcileObservationInterval = 2 * time.Second

// profileReconcileOptions are the parsed command line values of the
// reconciliation mode.
type profileReconcileOptions struct {
	// OperationID is the needs-review operation to resolve.
	OperationID string
	// ConfirmProfile is the exact Profile name the operator typed back.
	ConfirmProfile string
	// Reason is the offline reconciliation reason recorded in the audit.
	Reason string
	// Apply makes the derived outcome durable. Without it nothing changes.
	Apply bool
}

// reconcileProfileDelete resolves one uncertain Profile deletion.
//
// The database is opened first, and the exclusive process lock refuses to
// open it while the application is running: the reconciliation is only safe
// when nothing else can execute an operation against the same records.
func reconcileProfileDelete(ctx context.Context, cfg config.Config, opts profileReconcileOptions) error {
	if opts.OperationID == "" {
		return errors.New("the operation ID to reconcile is required")
	}
	if opts.ConfirmProfile == "" {
		return errors.New("the exact Profile name must be confirmed")
	}
	if opts.Reason == "" {
		return errors.New("an offline reconciliation reason is required")
	}
	s, err := store.OpenWithJournal(cfg.Database.Path, cfg.Database.JournalMode, cfg.Database.AllowUnverifiedFilesystem)
	if err != nil {
		return err
	}
	defer s.Close()
	plan, err := s.PlanProfileDeleteReconciliation(ctx, opts.OperationID)
	if err != nil {
		return err
	}
	// The cluster comes from the application's own configuration. The
	// reconciliation never guesses a cluster from the current context or
	// from a default.
	k, err := kube.New(cfg)
	if err != nil {
		return err
	}
	return reconcileProfileDeleteWith(ctx, s, k, plan, opts)
}

// reconcileProfileDeleteWith resolves one uncertain Profile deletion against
// an already open store and an already selected cluster.
//
// The store and the cluster are passed in so the reads can be exercised
// without a cluster. The exclusive process lock is taken by the caller: the
// reconciliation is only safe while nothing else can execute an operation
// against the same records.
func reconcileProfileDeleteWith(ctx context.Context, s *store.Store, k *kube.Service, plan store.ProfileDeleteReconcilePlan, opts profileReconcileOptions) error {
	// The operator confirms the exact Profile the accepted deletion
	// targeted. A mismatch is refused before anything is read or proposed.
	if opts.ConfirmProfile != plan.ProfileName {
		return errors.New("the confirmed Profile name does not match the accepted deletion target")
	}
	if plan.Operation.State != "needs-review" {
		return errors.New("only a needs-review operation can be reconciled")
	}
	observations := make([]store.ProfileDeleteObservation, 0, 2)
	for round := 0; round < 2; round++ {
		if round > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(profileReconcileObservationInterval):
			}
		}
		observation, err := observeProfileDelete(ctx, k, plan)
		if err != nil {
			return err
		}
		observations = append(observations, observation)
	}
	// The decision is derived from the evidence, never proposed by the
	// operator. A cluster mismatch, a replaced object, an incomplete read or
	// an unresolved write all fail closed here, before anything is proposed
	// and long before anything is applied.
	outcome, err := store.DeriveProfileDeleteOutcome(plan, observations)
	if err != nil {
		return err
	}
	slog.Info("profile deletion reconciliation decision",
		"operationID", plan.OperationID,
		"profile", plan.ProfileName,
		"outcome", string(outcome),
		"clusterUID", plan.ClusterUID,
		"observedClusterUID", observations[0].ClusterUID,
		"profileUID", plan.ProfileUID,
		"profileResourceVersion", plan.ProfileResourceVersion,
		"namespaceUID", plan.NamespaceUID,
		"observations", len(observations),
		"apply", opts.Apply)
	if !opts.Apply {
		slog.Info("dry run only: no durable state was changed and the Profile and namespace locks are still held; re-run with -apply to record the derived outcome")
		return nil
	}
	op, err := s.ReconcileProfileDelete(ctx, store.ProfileDeleteReconciliation{
		OperationID:  plan.OperationID,
		Observations: observations,
		Reason:       opts.Reason,
		Operator:     offlineOperatorIdentity(),
	})
	if err != nil {
		return err
	}
	slog.Info("profile deletion reconciliation recorded",
		"operationID", op.ID,
		"state", op.State,
		"step", op.Step,
		"outcome", string(outcome))
	return nil
}

// observeProfileDelete reads the Profile, the namespace it owns and the
// cluster identity exactly once.
//
// It issues GET requests only. This path never mutates the cluster and never
// deletes anything: a read that fails for any reason other than "absent"
// makes the observation incomplete, which fails the whole reconciliation. A
// successful read that returns no UID is incomplete as well: an object
// without an identity is never encoded as absent.
func observeProfileDelete(ctx context.Context, k *kube.Service, plan store.ProfileDeleteReconcilePlan) (store.ProfileDeleteObservation, error) {
	observation := store.ProfileDeleteObservation{}
	// The cluster identity is read first. An observation taken against
	// another cluster would report both objects as absent.
	cluster, err := k.Typed.CoreV1().Namespaces().Get(ctx, clusterIdentityNamespace, metav1.GetOptions{})
	if err != nil {
		return observation, fmt.Errorf("the cluster identity could not be read: %s", safeReadError(err))
	}
	observation.ClusterUID = string(cluster.UID)
	profile, err := k.Dynamic.Resource(kube.Profiles).Get(ctx, plan.ProfileName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return observation, fmt.Errorf("the Profile could not be read: %s", safeReadError(err))
		}
	} else {
		uid := string(profile.GetUID())
		if uid == "" {
			// A successful read without a UID is not an absent Profile: it is
			// an incomplete observation. Encoding it as absent would let a
			// broken API or proxy prove a deletion that never happened.
			return observation, errors.New("the Profile was read without a UID: the observation is incomplete")
		}
		observation.ProfileUID = uid
	}
	ns, err := k.Typed.CoreV1().Namespaces().Get(ctx, plan.ProfileName, metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return observation, fmt.Errorf("the Profile namespace could not be read: %s", safeReadError(err))
		}
	} else {
		uid := string(ns.UID)
		if uid == "" {
			// The same rule as for the Profile: an object without an
			// identity is never encoded as absent.
			return observation, errors.New("the Profile namespace was read without a UID: the observation is incomplete")
		}
		observation.NamespaceUID = uid
	}
	observation.Complete = true
	return observation, nil
}

// safeReadError describes a failed read without ever echoing the raw client
// error, which may carry request or server detail that is not the operator's
// to publish.
func safeReadError(err error) string {
	switch {
	case err == nil:
		return "unknown read failure"
	case apierrors.IsNotFound(err):
		return "the object is absent"
	case apierrors.IsForbidden(err):
		return "the API denied the read"
	case apierrors.IsUnauthorized(err):
		return "the API rejected the credentials"
	case apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err):
		return "the read timed out"
	case errors.Is(err, context.DeadlineExceeded):
		return "the read timed out"
	case errors.Is(err, context.Canceled):
		return "the read was canceled"
	default:
		return "the read failed"
	}
}

// offlineOperatorIdentity names the offline operator in the audit row. It is
// deliberately coarse: the audit records who reconciled the operation, and
// the operator's own reason explains why.
func offlineOperatorIdentity() string {
	return "offline-operator"
}
