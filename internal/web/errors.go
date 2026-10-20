package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"kubeflow-admin/internal/auth"
	"kubeflow-admin/internal/kube"
	"kubeflow-admin/internal/store"
)

type apiError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func failure(w http.ResponseWriter, status int, code, message string, retryable bool) {
	respond(w, status, struct {
		Error apiError `json:"error"`
	}{apiError{code, message, retryable}})
}

// Backend validation errors are currently untyped. Only exact, reviewed authored
// messages cross this boundary. Unknown errors (including SQL, JSON and client
// internals) fail closed; adding a backend policy needs a corresponding entry.
// Never turn arbitrary err.Error() text into a user-visible response.
func backendFailure(w http.ResponseWriter, err error, validation bool) {
	switch {
	case errors.Is(err, kube.ErrConflict):
		failure(w, 409, "stale_plan", "The resource changed or already exists. Refresh inventory and prepare a new plan.", false)
		return
	case errors.Is(err, store.ErrConflict):
		failure(w, 409, "operation_conflict", "An operation already claims this plan, key or target. Check Operations before preparing another plan.", false)
		return
	case errors.Is(err, store.ErrHistoryBudget):
		failure(w, 503, "history_capacity_exceeded", "History capacity is exhausted. Ask the operator to review retention, disk capacity and unresolved operations before accepting new changes.", false)
		return
	case errors.Is(err, store.ErrInvalidTarget):
		failure(w, 422, "invalid_target", "The prepared action does not have a valid resource and parent target. Refresh and prepare a new plan.", false)
		return
	case errors.Is(err, store.ErrIntegrity):
		failure(w, 503, "operation_integrity_error", "Durable operation evidence is inconsistent. Stop new changes and ask the operator to reconcile the database with actual cluster state.", false)
		return
	case errors.Is(err, store.ErrNotFound):
		failure(w, 404, "not_found", "The requested plan or operation was not found. Refresh the list or prepare a new plan.", false)
		return
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		failure(w, 503, "request_timeout", "The request timed out. Refresh inventory or check Operations before retrying a submission with the same idempotency key.", true)
		return
	case errors.Is(err, auth.ErrSessionInvalid):
		// The worker boundary returns auth.ErrSessionInvalid unchanged; match it
		// by identity so wrapping or message changes cannot misroute this.
		failure(w, 403, "reauthentication_required", "Fresh administrator authentication is required. Sign in again before preparing a new plan.", false)
		return
	}
	message := err.Error()
	switch message {
	case "Kubernetes resource not found", "matching Profile required: Kubernetes resource not found":
		failure(w, 404, "resource_not_found", "The Kubernetes resource was not found. Verify the name and namespace; notebook and storage changes require a matching Profile.", false)
	case "Kubernetes API denied the request", "matching Profile required: Kubernetes API denied the request":
		failure(w, 503, "cluster_access_denied", "Kubernetes denied the request. Ask the operator to check the application's RBAC and cluster admission policy.", false)
	case "Kubernetes API rejected validation or conditional patch":
		failure(w, 422, "cluster_validation_failed", "Kubernetes rejected validation or the conditional patch. Check quantities, admission policy and installed API compatibility, then refresh and prepare again.", false)
	case "plan belongs to a different actor":
		failure(w, 403, "plan_actor_mismatch", "This plan belongs to another administrator. Prepare a plan in your own session.", false)
	case "plan expired; preview again":
		failure(w, 409, "plan_expired", "The plan expired. Refresh inventory and prepare a new plan for review.", false)
	case "operation queue full; retry later":
		failure(w, 503, "queue_full", "The operation queue is full. Check Operations and retry shortly with the same plan and idempotency key.", true)
	default:
		if validation && approvedValidationMessage(message) {
			failure(w, 422, "validation_failed", message, false)
			return
		}
		failure(w, 503, "service_unavailable", "The service could not complete this request. Check readiness and ask the operator to inspect sanitized diagnostics. Check Operations before retrying a submission.", true)
	}
}

func approvedValidationMessage(message string) bool {
	if len(message) > 700 || !utf8.ValidString(message) || strings.IndexFunc(message, unicode.IsControl) >= 0 {
		return false
	}
	lower := strings.ToLower(message)
	for _, sensitive := range []string{"bearer ", "client_secret", "password", "token=", "sqlite", "sql:", "kubeconfig", "-----begin", "eyj"} {
		if strings.Contains(lower, sensitive) {
			return false
		}
	}
	_, ok := validationMessages[message]
	return ok
}

var validationMessages = func() map[string]struct{} {
	m := make(map[string]struct{})
	for _, message := range strings.Split(strings.TrimSpace(`
invalid Kubernetes object name
invalid namespace name
protected namespace
namespace is outside configured scope
Profile is terminating
unsupported Profile owner contract
namespace is not active
namespace ownership does not match its Profile
writes are disabled
reason is too long
reason exceeds 500 characters
Profile namespace must match its name
Notebook writes require a confirmed installed controller contract
Notebook is terminating
unsupported Notebook API contract
unsupported action kind
pod restarts are disabled
Pod restarts require a confirmed installed Notebook controller contract
a nonempty reason is required for pod restart
Pod has no stable UID
Pod is terminating
mirror and static Pods cannot be restarted
Pod is not running
Pod is not managed by a controller
Pod has an ambiguous controller owner chain
only StatefulSet-managed Notebook Pods are supported
Pod owner has no stable UID
Pod owner chain changed; prepare a new plan
StatefulSet is terminating
StatefulSet has no desired replica
StatefulSet has no desired revision
Pod has no controller revision
StatefulSet is not controlled by a Notebook
parent Notebook is stopped; start it before restarting its Pod
Pod is not assigned to a Node
assigned Node is terminating
assigned Node is not Ready
assigned Node is unschedulable
assigned Node is unreachable or not ready
a positive Kubernetes quantity is required
nonempty quota required; removing quotas is unsupported
unsupported quota resource
changing an unsupported quota key is unsupported
exact nonempty owner identity required
Profile must have a namespace-compatible name
existing namespaces cannot be adopted
quota removal is unsupported
unsupported Profile quota shape
quota key removal is unsupported by this adapter
explicit approved StorageClass required
StorageClass is terminating
explicit supported access mode required
existing PV reservation requires Retain reclaim policy
PV must be Available, unreserved, and nonterminating; Released volumes require external data review
topology-constrained static imports are unsupported
block-mode volumes are unsupported
PV and PVC class mismatch
insufficient PV capacity
PV access mode mismatch
only approved network filesystem volumes can be reserved
storage request exceeds observed ResourceQuota headroom
PVC expansion is disabled
expansion requires a bound nonterminating PVC with explicit class
PVC expansion must increase requested size
StorageClass does not allow expansion
expansion requires a verified CSI binding and matching driver
class has no dynamic provisioner
static storage is disabled
unsupported storage action
ambiguous template name
template class/access mismatch
canonical nonempty relative NFS subdirectory required
invalid approved NFS template
NFS path must remain strictly beneath approved base
invalid or incompatible fixed CSI template
approved storage template required
CSI template driver does not match class
backing storage is already represented by a PV
PV backing source is not in an approved template
PVC/PV binding identity mismatch
unsupported container shape
select an existing container by name
unsupported Notebook Pod template
unsupported container list
unsupported container identity
image must exactly match an approved image
unsupported resources shape
unsupported resource map
no supported Notebook edit was requested
unsupported volumes shape
unsupported mounts shape
unsupported volume shape
unsupported volume identity
duplicate existing volume names
unsupported mount shape
mount references a missing volume
advanced PVC subPath edits are unsupported
invalid mount/claim name
duplicate mount name or path
unsafe mount path
cannot replace an unrelated volume
unsupported PVC volume
swapping a claim under an existing volume name is unsupported; use a new volume name
protected workspace mount cannot be replaced
protected workspace read-only mode cannot change
protected workspace mount cannot be removed
mount editing ephemeral-container templates is unsupported
unsupported mount identity
GPU edit requires a nonnegative bounded integer count
ambiguous GPU preset
previous owned GPU preset is no longer configured
approved GPU preset required
GPU edits with GPU-allocated sidecars are unsupported
unsupported nodeSelector shape
owned GPU selector was independently changed
owned GPU RuntimeClass was independently changed
unsupported GPU resource shape
existing GPU allocation has no verified preset ownership
GPU preset requires a valid extended-resource key
GPU preset would overwrite an independent node selector
GPU preset would overwrite an independent RuntimeClass
GPU preset edits with affinity require a separately tested scheduling adapter
invalid GPU quantity
GPU scheduling shape is unsupported
unsupported Pod scheduling shape
RuntimeClass-added scheduling requires a separately tested adapter
no Ready node satisfies GPU preset, independent selectors, taints and advertised allocation
unsupported Pod template shape
Notebook requires containers
negative resource request
resource request exceeds limit
negative resource limit
extended resources must use integer quantities
extended resource request must equal limit
Pod-wide LimitRange checks require a tested sidecar-aware adapter
resource request below LimitRange minimum
resource limit exceeds LimitRange maximum
resource ratio violates LimitRange
Notebook resource total exceeds namespace quota
duplicate volume name
referenced PVC is terminating or Lost
unbound PVC without explicit StorageClass
PVC is not bound and is not waiting for first consumer
bound PVC lacks volume identity
mounting topology-constrained PVs requires a tested scheduling adapter
read-only claim requires a read-only mount
ReadWriteOncePod claim already has a consumer; stop it before planning
container mount references missing volume
invalid container resources
idempotency key must be 16..128 characters
`), "\n") {
		m[message] = struct{}{}
	}
	return m
}()
