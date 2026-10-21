package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"kubeflow-admin/internal/model"
)

const nodeCordonReasonLimit = 500
const nodeCordonPatchAttempts = 3

// nodeCordonState is the heartbeat-safe subset of a Node. Status conditions and
// resourceVersion are deliberately excluded: kubelet updates them every few
// seconds and must not invalidate an otherwise valid plan.
type nodeCordonState struct {
	Name          string
	UID           types.UID
	Unschedulable bool
	Terminating   bool
	ControlPlane  bool
}

func nodeCordonEvidence(n *corev1.Node) nodeCordonState {
	return nodeCordonState{
		Name:          n.Name,
		UID:           n.UID,
		Unschedulable: n.Spec.Unschedulable,
		Terminating:   n.DeletionTimestamp != nil,
		ControlPlane:  isControlPlaneNode(n),
	}
}

// nodeCordonPatch builds the single mutation this workflow may perform. The
// resourceVersion test makes the write atomic with the freshest heartbeat
// observation; executeNodeCordon retries a bounded number of times when only
// kubelet heartbeat churn causes that test to fail.
func nodeCordonPatch(uid types.UID, rv string, unschedulable bool) []byte {
	ops := []map[string]any{
		{"op": "test", "path": "/metadata/uid", "value": string(uid)},
	}
	if rv != "" {
		ops = append(ops, map[string]any{"op": "test", "path": "/metadata/resourceVersion", "value": rv})
	}
	ops = append(ops, map[string]any{"op": "add", "path": "/spec/unschedulable", "value": unschedulable})
	b, _ := json.Marshal(ops)
	return b
}

// prepareNodeCordon validates a cluster-scoped Node action. It never relies on
// Profile scope or namespace ownership, and it records no status-derived state.
func (s *Service) prepareNodeCordon(ctx context.Context, a model.Action, w *work) error {
	if a.Namespace != "" {
		return errors.New("Node namespace must be empty")
	}
	reason := strings.TrimSpace(a.Reason)
	if reason == "" {
		return errors.New("a nonempty reason is required for node cordon")
	}
	if len([]rune(reason)) > nodeCordonReasonLimit {
		return errors.New("reason exceeds 500 characters")
	}
	node, err := s.Typed.CoreV1().Nodes().Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		return safe(err)
	}
	if node.UID == "" {
		return errors.New("Node has no stable UID")
	}
	if node.DeletionTimestamp != nil {
		return errors.New("Node is terminating")
	}
	if a.Kind == "node.cordon" {
		if node.Spec.Unschedulable && !a.Force {
			return errors.New("Node is already cordoned")
		}
		if isControlPlaneNode(node) && !a.Force {
			return errors.New("control-plane Node requires force to cordon")
		}
		if !nodeReady(node) && !a.Force {
			return errors.New("NotReady Node requires force to cordon")
		}
	} else if a.Kind == "node.uncordon" && !node.Spec.Unschedulable && !a.Force {
		return errors.New("Node is already schedulable")
	}
	w.node = node
	w.prepared.TargetUID = string(node.UID)
	// Informational only. Execute compares the stable evidence and never uses
	// this value as a patch precondition.
	w.prepared.ResourceVersion = node.ResourceVersion
	w.ev.Target = digest(nodeCordonEvidence(node))
	w.ev.add("Node", "", node.Name, nodeCordonEvidence(node))
	if a.Kind == "node.cordon" {
		w.prepared.Warnings = append(w.prepared.Warnings,
			"Cordoning sets spec.unschedulable=true and updates status to include SchedulingDisabled. New Pods will not be scheduled on this Node; existing Pods are not evicted.",
			"Changing a Node does not drain workloads or guarantee application availability; review existing Pods separately.",
		)
	} else {
		w.prepared.Warnings = append(w.prepared.Warnings,
			"Uncordoning sets spec.unschedulable=false and allows new Pods to schedule on this Node. Existing Pods are not moved.",
		)
	}
	if a.Force {
		w.prepared.Warnings = append(w.prepared.Warnings, "Force was explicitly enabled for this Node action; verify the Node's health and role before proceeding.")
	}
	if a.Kind == "node.cordon" && isControlPlaneNode(node) {
		w.prepared.Warnings = append(w.prepared.Warnings, "This Node carries a control-plane role; cordoning it can reduce control-plane availability even when the write succeeds.")
	}
	return nil
}

func nodeCordonStep(kind string) string {
	if kind == "node.uncordon" {
		return "uncordon-node"
	}
	return "cordon-node"
}

// executeNodeCordon is a specialized path. Every read and the durable checkpoint
// happen before the bounded patch attempts; failures before a write are
// classified as pre-mutation so the store can release the Node lock.
func (s *Service) executeNodeCordon(ctx context.Context, p model.Prepared, cp func(string) error) (err error) {
	mutationAttempted := false
	defer func() {
		if err != nil && !mutationAttempted {
			err = errors.Join(model.ErrRejectedBeforeMutation, err)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if !s.Config.Features.WritesEnabled {
		return errors.New("writes are disabled")
	}
	if !s.Config.Features.NodeCordon {
		return errors.New("node cordon is disabled")
	}
	node, err := s.revalidateNodeCordon(ctx, p)
	if err != nil {
		return err
	}
	if err = checkpoint(cp, nodeCordonStep(p.Action.Kind)); err != nil {
		return err
	}
	want := p.Action.Kind == "node.cordon"
	var patchErr error
	for attempt := 0; attempt < nodeCordonPatchAttempts; attempt++ {
		if attempt > 0 {
			// A 409 from the JSON-patch test rejected the write before mutation.
			// Allow a failed revalidation on retry to be classified as such.
			mutationAttempted = false
		}
		// Re-read before every attempt so the patch carries the freshest
		// resourceVersion while UID, scheduling state, role and readiness gates
		// are still revalidated. Status-only heartbeat churn is acceptable.
		if node, err = s.revalidateNodeCordon(ctx, p); err != nil {
			return err
		}
		mutationAttempted = true
		_, err = s.Typed.CoreV1().Nodes().Patch(ctx, node.Name, types.JSONPatchType, nodeCordonPatch(node.UID, node.ResourceVersion, want), metav1.PatchOptions{})
		if err == nil {
			patchErr = nil
			break
		}
		patchErr = err
		if !apierrors.IsConflict(err) {
			break
		}
	}
	if patchErr != nil {
		return nodeCordonPatchError(patchErr)
	}
	return s.wait(ctx, func() (bool, error) {
		cur, err := s.Typed.CoreV1().Nodes().Get(ctx, node.Name, metav1.GetOptions{})
		if err != nil {
			return false, safe(err)
		}
		if cur.UID != node.UID {
			return false, fmt.Errorf("Node identity changed during cordon: %w", ErrConflict)
		}
		if cur.DeletionTimestamp != nil {
			return false, fmt.Errorf("Node became terminating during cordon: %w", ErrConflict)
		}
		return cur.Spec.Unschedulable == want, nil
	})
}

func (s *Service) revalidateNodeCordon(ctx context.Context, p model.Prepared) (*corev1.Node, error) {
	node, err := s.Typed.CoreV1().Nodes().Get(ctx, p.Action.Name, metav1.GetOptions{})
	if err != nil {
		return nil, safe(err)
	}
	if node.UID == "" || string(node.UID) != p.TargetUID {
		return nil, fmt.Errorf("Node identity changed before cordon: %w", ErrConflict)
	}
	if node.DeletionTimestamp != nil {
		return nil, errors.New("Node is terminating")
	}
	// Readiness is not part of the heartbeat-stable evidence, but a forced
	// cordon must still be re-confirmed when the Node has become NotReady.
	if p.Action.Kind == "node.cordon" && !p.Action.Force && !nodeReady(node) {
		return nil, errors.New("NotReady Node requires force to cordon")
	}
	var stored evidence
	if len(p.Data) == 0 {
		return nil, fmt.Errorf("prepared Node evidence is missing: %w", ErrConflict)
	}
	if err = json.Unmarshal(p.Data, &stored); err != nil {
		return nil, fmt.Errorf("prepared Node evidence is invalid: %w", ErrConflict)
	}
	if stored.Policy != s.newEvidence().Policy {
		return nil, fmt.Errorf("Node policy changed before cordon: %w", ErrConflict)
	}
	if stored.Target == "" || stored.Target != digest(nodeCordonEvidence(node)) {
		return nil, fmt.Errorf("Node state changed before cordon: %w", ErrConflict)
	}
	return node, nil
}

// nodeCordonPatchError distinguishes definite API rejections (no mutation) from
// transport ambiguity. Only the former may release the Node lock automatically.
func nodeCordonPatchError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, model.ErrRejectedBeforeMutation):
		return err
	case apierrors.IsForbidden(err):
		return fmt.Errorf("%w: Kubernetes API denied the Node patch", model.ErrRejectedBeforeMutation)
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w: Kubernetes Node not found", model.ErrRejectedBeforeMutation)
	case apierrors.IsConflict(err):
		return fmt.Errorf("%w: %w: Node changed before the patch was applied", ErrConflict, model.ErrRejectedBeforeMutation)
	case apierrors.IsTooManyRequests(err):
		return fmt.Errorf("%w: Kubernetes API throttled the Node patch", model.ErrRejectedBeforeMutation)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err), apierrors.IsUnauthorized(err), apierrors.IsMethodNotSupported(err), apierrors.IsRequestEntityTooLargeError(err), apierrors.IsUnsupportedMediaType(err):
		return fmt.Errorf("%w: Kubernetes API rejected the Node patch", model.ErrRejectedBeforeMutation)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		// The request may have reached the API server. Keep this ambiguous so
		// the Node lock remains held for review; never call it rejected.
		return errors.New("Kubernetes API Node patch outcome is unknown")
	}
}
