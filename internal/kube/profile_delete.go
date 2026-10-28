package kube

// The path is deliberately narrow. It deletes exactly one Profile object with
// UID and resourceVersion preconditions and Background propagation (the
// kubectl default). It never deletes a namespace, never deletes or edits a
// PersistentVolume, never removes a finalizer, never force-deletes a
// terminating object and never enumerates the namespace contents to decide
// what to remove: the cleanup belongs to the Profile controller and the
// Kubernetes garbage collector.
//
// The specialized path is dispatched from Service.Prepare and Service.Execute.
// It is separate from the generic path because the preview compare cannot express
// "one Profile object plus the namespace it owns" and must never be allowed
// to widen into a namespace or volume delete.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"kubeflow-admin/internal/model"
)

const (
	// profileDeleteReasonLimit is the authored reason cap for this kind.
	profileDeleteReasonLimit = 500
	// profileDeleteObservationLimit bounds every informational list used for
	// the preview counts. The counts are a sample, never a complete census.
	profileDeleteObservationLimit = 100
	// profileDeleteNamespacePageLimit bounds one page of the namespace list
	// that verifies the Profile's ownership blast radius.
	profileDeleteNamespacePageLimit = 200
	// profileDeleteNamespacePageMax bounds how many pages that verification
	// reads. An incomplete sweep is refused, never truncated.
	profileDeleteNamespacePageMax = 20
	// profileDeleteStep is the durable checkpoint step of this workflow.
	profileDeleteStep = "delete-profile"
	// profileDeleteClusterNamespace is the well-known namespace whose UID
	// identifies the cluster a plan was prepared against.
	profileDeleteClusterNamespace = "kube-system"
)

// profileDeleteEvidence is the narrow, stable evidence of one deletion plan.
// It carries identifiers and ownership facts only: the Profile UID and
// resourceVersion that become the delete preconditions, the namespace UID and
// its exact controlling owner reference plus owner identity, and the policy
// digest. The namespace's resourceVersion is deliberately absent: a namespace
// heartbeat must not invalidate an otherwise valid plan, while its identity
// and ownership must. No object bodies, no Secrets, no manifests and no
// observed counts are stored, because those churn with ordinary cluster
// activity and would make an otherwise valid plan look stale.
type profileDeleteEvidence struct {
	Version        int                   `json:"version"`
	Policy         string                `json:"policy"`
	ClusterUID     string                `json:"clusterUID"`
	ProfileName    string                `json:"profileName"`
	ProfileUID     string                `json:"profileUID"`
	ProfileRV      string                `json:"profileRV"`
	ProfileOwner   string                `json:"profileOwner"`
	NamespaceName  string                `json:"namespaceName"`
	NamespaceUID   string                `json:"namespaceUID"`
	NamespaceOwner string                `json:"namespaceOwner"`
	OwnerRef       profileDeleteOwnerRef `json:"namespaceOwnerRef"`
}

// profileDeleteOwnerRef records the exact controlling owner reference the
// namespace carried when the plan was prepared.
type profileDeleteOwnerRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Controller bool   `json:"controller"`
}

// profileDeleteObservation is the bounded, informational view of the namespace
// contents seen while the plan was prepared. It is reported to the
// administrator in the preview and is never part of the plan evidence.
type profileDeleteObservation struct {
	Counted   bool
	Notebooks int
	Pods      int
	Claims    int
	Truncated bool
	PVRetain  int
	PVDelete  int
	PVUnknown int
}

// prepareProfileDelete plans the deletion of exactly one Profile.
//
// It requires both the writes opt-in and the separate Profile deletion
// opt-in, a DNS-1123 label name, an empty or matching namespace, an authored
// reason and the exact typed confirmation of the target name. It then
// verifies the live Profile, the live namespace and the exact ownership link
// between them, and proves the delete against the API with a dry run that
// carries the same UID, resourceVersion and propagation policy as the real
// request.
func (s *Service) prepareProfileDelete(ctx context.Context, a model.Action) (model.Prepared, error) {
	if !s.Config.Features.WritesEnabled {
		return model.Prepared{}, errors.New("writes are disabled")
	}
	if !s.Config.Features.ProfileDeletion {
		return model.Prepared{}, errors.New("profile deletion is disabled")
	}
	if err := profileDeleteActionConstraints(a); err != nil {
		return model.Prepared{}, err
	}
	// The scope policy protects the system namespaces and any configured
	// allow/deny list. It is checked before any Profile or namespace read.
	if err := s.namespacePolicy(a.Name); err != nil {
		return model.Prepared{}, err
	}
	profile, err := s.Dynamic.Resource(Profiles).Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		return model.Prepared{}, safe(err)
	}
	if profile.GetAPIVersion() != "kubeflow.org/v1" || profile.GetKind() != "Profile" {
		return model.Prepared{}, errors.New("unsupported Profile API contract")
	}
	if profile.GetUID() == "" {
		return model.Prepared{}, errors.New("Profile has no stable UID")
	}
	if profile.GetDeletionTimestamp() != nil {
		return model.Prepared{}, errors.New("Profile is terminating")
	}
	owner, _, err := profileDeleteOwner(profile)
	if err != nil {
		return model.Prepared{}, err
	}
	ns, err := s.Typed.CoreV1().Namespaces().Get(ctx, a.Name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Fail closed: without the Profile-owned namespace there is no
			// ownership to verify, so this adapter cannot prove what a delete
			// would remove. An orphaned Profile whose namespace is already
			// gone is therefore not deletable here; that case needs an
			// operator procedure, not a broadened console action.
			return model.Prepared{}, errors.New("the Profile namespace does not exist; this deletion requires the Profile-owned namespace")
		}
		return model.Prepared{}, safe(err)
	}
	if ns.UID == "" {
		return model.Prepared{}, errors.New("the Profile namespace has no stable UID")
	}
	ref, err := profileDeleteOwnership(ns, profile, owner)
	if err != nil {
		return model.Prepared{}, err
	}
	// The cluster identity is read once, from a well-known namespace, and it
	// is part of the plan: an offline reconciliation compares the accepted
	// Cluster UID with the cluster it is pointed at, so a plan prepared
	// against another cluster can never be resolved against this one.
	clusterUID, err := s.profileDeleteClusterUID(ctx)
	if err != nil {
		return model.Prepared{}, err
	}
	// The Profile must not own another namespace: the garbage collector would
	// remove that namespace too. The sweep is verified before the dry run and
	// again before every write.
	if err := s.profileDeleteNamespaceScope(ctx, profile.GetUID(), a.Name); err != nil {
		return model.Prepared{}, err
	}
	// The dry run proves the API accepts exactly the request the execution
	// will send: same preconditions, same propagation policy, no persistence.
	if err := s.profileDeleteDryRun(ctx, a.Name, profile.GetUID(), profile.GetResourceVersion()); err != nil {
		return model.Prepared{}, err
	}
	observation := s.observeProfileDeleteContents(ctx, a.Name)
	ev := profileDeleteEvidence{
		Version:        1,
		Policy:         s.newEvidence().Policy,
		ClusterUID:     string(clusterUID),
		ProfileName:    a.Name,
		ProfileUID:     string(profile.GetUID()),
		ProfileRV:      profile.GetResourceVersion(),
		ProfileOwner:   owner,
		NamespaceName:  a.Name,
		NamespaceUID:   string(ns.UID),
		NamespaceOwner: ns.Annotations["owner"],
		OwnerRef:       ref,
	}
	data, err := json.Marshal(ev)
	if err != nil {
		return model.Prepared{}, err
	}
	prepared := model.Prepared{Action: a}
	prepared.Summary = "profile.delete " + a.Name
	prepared.TargetUID = ev.ProfileUID
	prepared.ResourceVersion = ev.ProfileRV
	prepared.Data = data
	prepared.Changes = profileDeleteChanges(a.Name, owner, observation, ev)
	prepared.Warnings = profileDeleteWarnings(observation)
	return prepared, nil
}

// profileDeleteReason validates the authored reason. It is required, bounded
// and never trimmed into a different value.
func profileDeleteReason(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return errors.New("a nonempty reason is required for profile deletion")
	}
	if len([]rune(reason)) > profileDeleteReasonLimit {
		return errors.New("reason exceeds 500 characters")
	}
	return nil
}

// profileDeleteActionFields are the only fields a Profile deletion action may
// carry. Every other field of the typed action must be zero.
var profileDeleteActionFields = map[string]bool{
	"Kind":        true,
	"Namespace":   true,
	"Name":        true,
	"Reason":      true,
	"ConfirmName": true,
}

// profileDeleteNoOtherFields refuses a delete action that carries fields
// belonging to other kinds. A destructive request that also asks for a quota,
// a configuration digest, an image, a mount or a force flag is not the reviewed
// request: refusing it keeps this action a single, unambiguous deletion.
//
// The check is reflective on purpose: a field added to model.Action later is
// rejected here by default instead of silently widening the delete.
func profileDeleteNoOtherFields(a model.Action) error {
	v := reflect.ValueOf(a)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if profileDeleteActionFields[t.Field(i).Name] {
			continue
		}
		if !v.Field(i).IsZero() {
			return errors.New("profile deletion accepts no other action fields")
		}
	}
	return nil
}

// profileDeleteActionConstraints validates the pure, request-level contract of
// this kind: the name grammar, the namespace relationship, the authored
// reason and the exact typed confirmation. It reads no cluster state, so both
// the preparation and the execution apply it: the execution re-checks it
// defensively so a drifted adapter invariant can never reach a delete.
func profileDeleteActionConstraints(a model.Action) error {
	if err := profileName(a.Name); err != nil {
		return err
	}
	// The Profile name is also its namespace name. A supplied namespace must
	// therefore be that name; nothing else is ever treated as the target.
	if a.Namespace != "" && a.Namespace != a.Name {
		return errors.New("Profile namespace must match its name")
	}
	if err := profileDeleteReason(a.Reason); err != nil {
		return err
	}
	// The typed confirmation is compared raw: no trimming, no case folding.
	// A partial, padded or differently cased name is a different string and
	// is refused.
	if a.ConfirmName != a.Name {
		return errors.New("typed confirmation must exactly match the Profile name")
	}
	return profileDeleteNoOtherFields(a)
}

// profileDeleteOwner reads the Profile owner contract. Only the installed
// controller's "User" owner kind is supported.
func profileDeleteOwner(profile *unstructured.Unstructured) (string, string, error) {
	owner, _, _ := unstructured.NestedString(profile.Object, "spec", "owner", "name")
	kind, _, _ := unstructured.NestedString(profile.Object, "spec", "owner", "kind")
	if owner == "" || kind != "User" {
		return "", "", errors.New("unsupported Profile owner contract")
	}
	return owner, kind, nil
}

// profileDeleteOwnership proves that the namespace is the one this Profile
// owns. The namespace must carry exactly one owner reference, and that
// reference must be the controlling reference to this Profile, this Profile's
// UID and the kubeflow.org group: a second, live owner reference would keep
// the garbage collector from removing the namespace and would leave the
// ownership of the namespace undecided. The owner annotation must carry the
// Profile owner identity. A namespace that is owned differently, is owned
// more than once, or is not owned at all is never adopted.
func profileDeleteOwnership(ns *corev1.Namespace, profile *unstructured.Unstructured, owner string) (profileDeleteOwnerRef, error) {
	if ns.DeletionTimestamp != nil || ns.Status.Phase != corev1.NamespaceActive {
		return profileDeleteOwnerRef{}, errors.New("namespace is not active")
	}
	if len(ns.OwnerReferences) != 1 {
		return profileDeleteOwnerRef{}, errors.New("the Profile namespace has more than one owner reference")
	}
	ref, ok := profileDeleteControllerRef(ns.OwnerReferences, profile.GetUID(), profile.GetName())
	if !ok {
		return profileDeleteOwnerRef{}, errors.New("namespace ownership does not match its Profile")
	}
	if ns.Annotations["owner"] != owner {
		return profileDeleteOwnerRef{}, errors.New("namespace ownership does not match its Profile")
	}
	return ref, nil
}

// profileDeleteControllerRef returns the single controlling owner reference
// when it identifies the given Profile exactly. The reference list must hold
// exactly one entry: an additional owner reference is a live second owner.
func profileDeleteControllerRef(refs []metav1.OwnerReference, uid types.UID, name string) (profileDeleteOwnerRef, bool) {
	if len(refs) != 1 {
		return profileDeleteOwnerRef{}, false
	}
	r := refs[0]
	if r.Controller == nil || !*r.Controller {
		return profileDeleteOwnerRef{}, false
	}
	if r.UID != uid || r.Kind != "Profile" || r.Name != name {
		return profileDeleteOwnerRef{}, false
	}
	group, _, ok := strings.Cut(r.APIVersion, "/")
	if !ok || group != "kubeflow.org" {
		return profileDeleteOwnerRef{}, false
	}
	return profileDeleteOwnerRef{APIVersion: r.APIVersion, Kind: r.Kind, Name: r.Name, UID: string(r.UID), Controller: true}, true
}

// profileDeleteOwnedByProfile reports whether any owner reference of the
// namespace still points at the deleted Profile. The garbage collector can
// only remove a namespace that is still owned by it.
func profileDeleteOwnedByProfile(refs []metav1.OwnerReference, uid types.UID) bool {
	for _, r := range refs {
		if r.UID == uid {
			return true
		}
	}
	return false
}

// profileDeleteClusterUID reads the stable identity of the cluster this plan
// targets.
//
// It reads one well-known namespace and nothing else: no Secret, no
// kubeconfig, no context name and no environment value. The UID is what an
// offline reconciliation compares against the cluster it is pointed at, so a
// plan prepared against a different cluster can never be resolved against
// this one, and an operator database reused across clusters cannot silently
// complete a deletion in the wrong cluster. A missing or unreadable
// identity is refused: the proof is required, not best effort.
func (s *Service) profileDeleteClusterUID(ctx context.Context) (types.UID, error) {
	ns, err := s.Typed.CoreV1().Namespaces().Get(ctx, profileDeleteClusterNamespace, metav1.GetOptions{})
	if err != nil {
		return "", errors.New("cluster identity could not be verified")
	}
	if ns.UID == "" {
		return "", errors.New("cluster identity could not be verified")
	}
	return ns.UID, nil
}

// profileDeleteNamespaceScope verifies the blast radius of the deletion.
//
// A Profile that owns another namespace would make the garbage collector
// remove that namespace too, whatever its name and whatever the scope policy
// says about it. Every namespace is therefore listed and checked: no
// namespace other than the target may carry an owner reference to this
// Profile, whether it is a controlling reference or not.
//
// The sweep is bounded and it fails closed. A list error, an unreadable page
// or a sweep that does not finish is a refusal, never a truncated sweep that
// still sends the delete. This check is not atomic with the delete: the
// platform's admission policy and RBAC remain the only way to forbid a
// cross-namespace owner reference, a reparent or a replacement during the
// deletion.
func (s *Service) profileDeleteNamespaceScope(ctx context.Context, profileUID types.UID, namespaceName string) error {
	continuation := ""
	seen := 0
	for page := 0; page < profileDeleteNamespacePageMax; page++ {
		list, err := s.Typed.CoreV1().Namespaces().List(ctx, metav1.ListOptions{Limit: profileDeleteNamespacePageLimit, Continue: continuation})
		if err != nil {
			return errors.New("the namespace list could not be verified")
		}
		for _, ns := range list.Items {
			seen++
			if seen > profileDeleteNamespacePageMax*profileDeleteNamespacePageLimit {
				return errors.New("the namespace list could not be verified")
			}
			if ns.Name == namespaceName {
				continue
			}
			for _, r := range ns.OwnerReferences {
				if r.UID == profileUID {
					return fmt.Errorf("the Profile also owns another namespace: %w", ErrConflict)
				}
			}
		}
		continuation = list.Continue
		if continuation == "" {
			return nil
		}
	}
	return errors.New("the namespace list could not be verified")
}

// profileDeleteOptions builds the single request shape this workflow uses:
// UID and resourceVersion preconditions plus Background propagation.
//
// Background is the kubectl default: the Profile object is removed as soon as
// its own finalizer is released, and the namespace it owns is then removed by
// the Kubernetes garbage collector. The request never asks the API server to
// wait for the dependents before removing the Profile.
func profileDeleteOptions(uid types.UID, rv string, dry bool) metav1.DeleteOptions {
	propagation := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{
		Preconditions:     &metav1.Preconditions{UID: &uid, ResourceVersion: &rv},
		PropagationPolicy: &propagation,
	}
	if dry {
		opts.DryRun = []string{metav1.DryRunAll}
	}
	return opts
}

// profileDeleteDryRun proves the delete against the API without persisting
// anything. The dry run uses the same preconditions and propagation policy as
// the real request, so a changed object is refused here rather than later.
func (s *Service) profileDeleteDryRun(ctx context.Context, name string, uid types.UID, rv string) error {
	if uid == "" || rv == "" {
		return errors.New("Profile deletion requires UID and resourceVersion preconditions")
	}
	err := s.Dynamic.Resource(Profiles).Delete(ctx, name, profileDeleteOptions(uid, rv, true))
	if err == nil {
		return nil
	}
	// A dry run never persists anything, so every failure here is a plain
	// refusal of the plan and never an ambiguous write outcome.
	return profileDeleteDryRunError(err)
}

// profileDeleteDryRunError maps a failed dry run onto safe, reviewed
// messages. It never carries the pre-mutation sentinel: preparing a plan
// holds no lock and mutates nothing.
func profileDeleteDryRunError(err error) error {
	switch {
	case apierrors.IsForbidden(err), apierrors.IsUnauthorized(err):
		return errors.New("Kubernetes API denied the Profile deletion")
	case apierrors.IsNotFound(err):
		return fmt.Errorf("the Profile no longer exists: %w", ErrConflict)
	case apierrors.IsConflict(err):
		return fmt.Errorf("the Profile changed before the deletion: %w", ErrConflict)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err):
		return errors.New("Kubernetes API rejected the Profile deletion")
	default:
		return errors.New("Kubernetes API Profile deletion request failed")
	}
}

// observeProfileDeleteContents collects the bounded, aggregate counts shown in
// the preview: how many Notebooks, Pods and PersistentVolumeClaims were
// observed in the namespace, and how the volumes bound to those claims are
// configured to be reclaimed. No individual name, path, handle or Secret is
// read or reported, and the result never becomes part of the plan evidence:
// ordinary cluster activity must not make a valid plan look stale.
//
// Every observation is capped. The API server may ignore a list limit, so the
// returned items are capped here as well, and at most
// profileDeleteObservationLimit volumes are read. A denied or failed read is
// not fatal: it only removes the counts.
func (s *Service) observeProfileDeleteContents(ctx context.Context, ns string) profileDeleteObservation {
	obs := profileDeleteObservation{}
	notes, err := s.Dynamic.Resource(Notebooks).Namespace(ns).List(ctx, metav1.ListOptions{Limit: profileDeleteObservationLimit})
	if err != nil {
		return obs
	}
	obs.Notebooks, obs.Truncated = profileDeleteBoundedCount(len(notes.Items), notes.GetContinue() != "")
	pods, err := s.Typed.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{Limit: profileDeleteObservationLimit})
	if err != nil {
		return profileDeleteObservation{}
	}
	count, truncated := profileDeleteBoundedCount(len(pods.Items), pods.Continue != "")
	obs.Pods = count
	obs.Truncated = obs.Truncated || truncated
	claims, err := s.Typed.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{Limit: profileDeleteObservationLimit})
	if err != nil {
		return profileDeleteObservation{}
	}
	count, truncated = profileDeleteBoundedCount(len(claims.Items), claims.Continue != "")
	obs.Claims = count
	obs.Truncated = obs.Truncated || truncated
	for i := 0; i < len(claims.Items) && i < profileDeleteObservationLimit; i++ {
		claim := claims.Items[i]
		// Only a bound claim names a volume at all, and only a volume that
		// claims exactly this claim can be attributed to it. A volume name
		// alone can point at a stale or foreign volume, so it proves
		// nothing.
		if claim.Status.Phase != corev1.ClaimBound || claim.Spec.VolumeName == "" {
			obs.PVUnknown++
			continue
		}
		pv, err := s.Typed.CoreV1().PersistentVolumes().Get(ctx, claim.Spec.VolumeName, metav1.GetOptions{})
		if err != nil {
			obs.PVUnknown++
			continue
		}
		if pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.Namespace != claim.Namespace || pv.Spec.ClaimRef.Name != claim.Name || pv.Spec.ClaimRef.UID != claim.UID {
			obs.PVUnknown++
			continue
		}
		switch pv.Spec.PersistentVolumeReclaimPolicy {
		case corev1.PersistentVolumeReclaimRetain:
			obs.PVRetain++
		case corev1.PersistentVolumeReclaimDelete:
			obs.PVDelete++
		default:
			obs.PVUnknown++
		}
	}
	obs.Counted = true
	return obs
}

// profileDeleteBoundedCount caps an observed item count at the sample limit
// and reports whether the observation was truncated. A server that ignores the
// requested limit must not inflate the preview beyond the bound.
func profileDeleteBoundedCount(observed int, continued bool) (int, bool) {
	truncated := continued || observed >= profileDeleteObservationLimit
	if observed > profileDeleteObservationLimit {
		observed = profileDeleteObservationLimit
	}
	return observed, truncated
}

// profileDeleteChanges renders the reviewed scope of the deletion. It names
// the affected classes and the observed counts, never individual objects, and
// it never promises that a volume survives.
//
// The last rows are the durable identity of the reviewed instances: the exact
// Profile UID, Profile resourceVersion, namespace UID and reconciliation
// version. They are carried in the accepted plan and in the accepted audit
// record, so an offline reconciliation can verify which instances were
// reviewed after the plan itself has expired. They describe the reviewed
// identity and never change it.
func profileDeleteChanges(name, owner string, obs profileDeleteObservation, ev profileDeleteEvidence) []model.Change {
	changes := []model.Change{
		{Field: "Profile", Before: name, After: "deleted"},
		{Field: "Profile namespace", Before: name, After: "deleted by normal garbage collection"},
		{Field: "Profile owner", Before: owner, After: "the Profile's ownership record is removed; the identity provider account is not deleted"},
	}
	if obs.Counted {
		changes = append(changes,
			model.Change{Field: "observed Notebooks", Before: fmt.Sprintf("%d observed", obs.Notebooks), After: "deleted with the namespace"},
			model.Change{Field: "observed Pods", Before: fmt.Sprintf("%d observed", obs.Pods), After: "deleted with the namespace"},
			model.Change{Field: "observed PersistentVolumeClaims", Before: fmt.Sprintf("%d observed", obs.Claims), After: "deleted with the namespace; backing data may be irreversibly deleted according to the PV reclaim policy"},
			model.Change{Field: "observed PersistentVolume reclaim policies", Before: fmt.Sprintf("Retain %d, Delete %d, unknown %d", obs.PVRetain, obs.PVDelete, obs.PVUnknown), After: "not deleted by this console; a Delete-policy volume may still be destroyed by the storage stack"},
		)
	}
	// The durable identity rows are exact and immutable: they name the
	// reviewed cluster and instances, not a count and not a state.
	changes = append(changes,
		model.Change{Field: "Cluster UID", Before: ev.ClusterUID, After: "accepted identity"},
		model.Change{Field: "Profile UID", Before: ev.ProfileUID, After: "accepted identity"},
		model.Change{Field: "Profile resource version", Before: ev.ProfileRV, After: "accepted identity"},
		model.Change{Field: "Namespace UID", Before: ev.NamespaceUID, After: "accepted identity"},
		model.Change{Field: "Profile deletion reconciliation version", Before: "1", After: "accepted identity"},
	)
	return changes
}

// profileDeleteWarnings states plainly what the deletion removes and what it
// does not. It never promises that a volume or its data survives: what
// happens to a volume depends on the reclaim policy and on the storage
// stack, not on this console.
func profileDeleteWarnings(obs profileDeleteObservation) []string {
	warnings := []string{
		"Deleting this Profile removes the Profile and the namespace it owns. Everything in that namespace is removed by normal Kubernetes garbage collection: Notebooks, Pods, PersistentVolumeClaims, VolumeSnapshots and the other namespaced objects in it. The deletion is irreversible: this console has no undo, no recovery path and no forced cleanup.",
		"Removing PersistentVolumeClaims removes the claims to their storage. A Delete reclaim policy allows storage controllers or provisioners to delete the backing volume and its data irreversibly in the storage stack, not directly in this console. Data destruction is not guaranteed by deleting a claim: Retain-policy volumes and external data may remain. Never treat this operation as a verified data wipe.",
		"A volume with the Retain reclaim policy usually keeps its data: the PersistentVolume object and the backend data - an NFS export directory, CSI volume content, snapshots or other copies - may remain after the deletion. Remaining is not protection: the backend's own snapshot and retention lifecycle, a later operator delete, or a subsequent claim can still destroy that data.",
		"VolumeSnapshot objects live in the namespace and disappear with it. Their VolumeSnapshotContent objects and the backend snapshots they point at are cluster scoped and follow their own deletion and retention policy, so a backend snapshot may outlive this deletion.",
		"This Profile deletion never deletes a namespace directly, never deletes or edits a PersistentVolume, never removes a finalizer and never force-deletes a terminating object. A namespace that stays Terminating needs an operator plan.",
		"The deletion is complete only when both this Profile and its namespace are gone. A namespace that remains, an object that reappears under the same name, or a cleanup that does not finish leaves the operation for review instead of reporting success.",
		"This console never retries or replays a deletion automatically. An unfinished or ambiguous deletion stays unresolved for review and needs an operator plan; it is never resolved by deleting something else.",
		"Take a backup of the workspace data you need before confirming. This console never creates one.",
	}
	if obs.Counted {
		warnings = append(warnings, "The counts in this preview were observed while the plan was prepared, from a bounded sample. They are not a frozen inventory and not part of this plan's identity: the namespace can change before the deletion, and the deletion removes whatever is there at that moment.")
	} else {
		warnings = append(warnings, "The namespace contents could not be counted with the configured read permissions, so this preview states no counts. The deletion still removes everything in the namespace; verify the contents with your own read-only tooling first.")
	}
	if obs.Truncated {
		warnings = append(warnings, "The observed counts reached the bounded sample limit, so the namespace may contain more objects than this preview shows.")
	}
	return warnings
}

// executeProfileDelete performs the reviewed deletion.
//
// Every read and the durable checkpoint happen before the single delete
// request, and the plan is revalidated again after the checkpoint. A failure
// before that request is a pre-mutation rejection; a failure after it is
// ambiguous or unfinished and must be reviewed.
func (s *Service) executeProfileDelete(ctx context.Context, p model.Prepared, cp func(string) error) (err error) {
	mutationAttempted := false
	defer func() {
		// Only a failure that provably happened before the delete request may
		// release the operation lock.
		if err != nil && !mutationAttempted {
			err = errors.Join(model.ErrRejectedBeforeMutation, err)
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	if !s.Config.Features.WritesEnabled {
		return errors.New("writes are disabled")
	}
	if !s.Config.Features.ProfileDeletion {
		return errors.New("profile deletion is disabled")
	}
	if p.Action.Kind != "profile.delete" {
		return errors.New("unsupported action kind")
	}
	// The request-level contract is re-checked here, not only at preparation.
	// The plan is server owned, but an adapter invariant that drifted must
	// never reach a delete: the name grammar, the namespace relationship, the
	// authored reason, the exact typed confirmation and the absence of every
	// other action field are all verified again, together with the namespace
	// scope policy.
	if err := profileDeleteActionConstraints(p.Action); err != nil {
		return err
	}
	if err := s.namespacePolicy(p.Action.Name); err != nil {
		return err
	}
	ev, err := profileDeleteEvidenceFrom(p.Data)
	if err != nil {
		return err
	}
	// The prepared identity and the reviewed evidence must describe the same
	// object, and they must be the ones the delete preconditions use.
	if p.TargetUID != ev.ProfileUID || p.ResourceVersion != ev.ProfileRV {
		return fmt.Errorf("prepared Profile deletion identity does not match the plan: %w", ErrConflict)
	}
	if err = s.revalidateProfileDelete(ctx, p, ev); err != nil {
		return err
	}
	if err = checkpoint(cp, profileDeleteStep); err != nil {
		return err
	}
	// Re-read after the durable checkpoint so that every ownership fact is
	// still current when the delete is issued. The request keeps the reviewed
	// UID and resourceVersion as its preconditions: the plan is only valid
	// for the exact object it reviewed.
	if err = s.revalidateProfileDelete(ctx, p, ev); err != nil {
		return err
	}
	// One delete request, issued once. This workflow never replays it and the
	// client library is configured not to retry a write, so this is the only
	// delete this console sends. A transport library may still replay a
	// request that never reached the API server, so a transport failure
	// after this point is ambiguous and is never retried automatically.
	mutationAttempted = true
	deleteErr := s.Dynamic.Resource(Profiles).Delete(ctx, ev.ProfileName, profileDeleteOptions(types.UID(ev.ProfileUID), ev.ProfileRV, false))
	if deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
		return profileDeleteError(deleteErr)
	}
	// A 404 is not a definite refusal and never releases the fence: the
	// Profile may already be gone because the controller or another actor
	// removed it, or because an earlier request's response was rewritten. The
	// only authority on the outcome is the strict both-gone observation.
	return s.waitForProfileDeletion(ctx, ev)
}

// profileDeleteEvidenceFrom reads the stored evidence of a deletion plan.
func profileDeleteEvidenceFrom(raw json.RawMessage) (profileDeleteEvidence, error) {
	var ev profileDeleteEvidence
	if len(raw) == 0 {
		return ev, fmt.Errorf("prepared Profile deletion evidence is missing: %w", ErrConflict)
	}
	if err := json.Unmarshal(raw, &ev); err != nil {
		return ev, fmt.Errorf("prepared Profile deletion evidence is invalid: %w", ErrConflict)
	}
	if ev.Version != 1 || ev.ClusterUID == "" || ev.ProfileName == "" || ev.ProfileUID == "" || ev.ProfileRV == "" || ev.NamespaceName == "" || ev.NamespaceUID == "" {
		return ev, fmt.Errorf("prepared Profile deletion evidence is incomplete: %w", ErrConflict)
	}
	return ev, nil
}

// revalidateProfileDelete re-reads the Profile and its namespace and
// confirms that the plan still describes exactly them.
//
// The Profile identity and resourceVersion must match: they are the delete
// preconditions. The namespace identity and ownership must match: a namespace
// heartbeat or resourceVersion churn is irrelevant, but a replaced namespace
// or a changed owner reference invalidates the plan, because the ownership
// link is what proves what would be removed.
func (s *Service) revalidateProfileDelete(ctx context.Context, p model.Prepared, ev profileDeleteEvidence) error {
	if ev.Policy != s.newEvidence().Policy {
		return fmt.Errorf("Profile deletion policy changed before the deletion: %w", ErrConflict)
	}
	// The plan must still target the same cluster. A context that now points
	// at another cluster is refused before any write: the objects of that
	// other cluster are not the ones the administrator reviewed.
	clusterUID, err := s.profileDeleteClusterUID(ctx)
	if err != nil {
		return err
	}
	if string(clusterUID) != ev.ClusterUID {
		return fmt.Errorf("the cluster identity changed before the deletion: %w", ErrConflict)
	}
	if ev.ProfileName != p.Action.Name || ev.NamespaceName != p.Action.Name {
		return fmt.Errorf("prepared Profile deletion evidence does not match the action: %w", ErrConflict)
	}
	profile, err := s.Dynamic.Resource(Profiles).Get(ctx, ev.ProfileName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("the Profile no longer exists: %w", ErrConflict)
		}
		return safe(err)
	}
	if profile.GetAPIVersion() != "kubeflow.org/v1" || profile.GetKind() != "Profile" {
		return errors.New("unsupported Profile API contract")
	}
	if profile.GetUID() != types.UID(ev.ProfileUID) {
		return fmt.Errorf("Profile identity changed before the deletion: %w", ErrConflict)
	}
	if profile.GetResourceVersion() != ev.ProfileRV {
		return fmt.Errorf("Profile changed before the deletion: %w", ErrConflict)
	}
	if profile.GetDeletionTimestamp() != nil {
		return errors.New("Profile is terminating")
	}
	owner, kind, err := profileDeleteOwner(profile)
	if err != nil {
		return err
	}
	if kind != "User" || owner != ev.ProfileOwner {
		return fmt.Errorf("Profile owner changed before the deletion: %w", ErrConflict)
	}
	ns, err := s.Typed.CoreV1().Namespaces().Get(ctx, ev.NamespaceName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("the Profile namespace disappeared before the deletion: %w", ErrConflict)
		}
		return safe(err)
	}
	if ns.UID != types.UID(ev.NamespaceUID) {
		return fmt.Errorf("the Profile namespace was replaced before the deletion: %w", ErrConflict)
	}
	if ns.DeletionTimestamp != nil || ns.Status.Phase != corev1.NamespaceActive {
		return fmt.Errorf("namespace is not active: %w", ErrConflict)
	}
	ref, ok := profileDeleteControllerRef(ns.OwnerReferences, types.UID(ev.ProfileUID), ev.ProfileName)
	if !ok || ref != ev.OwnerRef {
		return fmt.Errorf("namespace ownership changed before the deletion: %w", ErrConflict)
	}
	if ns.Annotations["owner"] != ev.NamespaceOwner {
		return fmt.Errorf("namespace ownership changed before the deletion: %w", ErrConflict)
	}
	// The blast radius is verified again here: a Profile that acquired
	// another namespace since the plan would make the garbage collector
	// remove that namespace too. The sweep fails closed.
	if err := s.profileDeleteNamespaceScope(ctx, types.UID(ev.ProfileUID), ev.NamespaceName); err != nil {
		return err
	}
	return nil
}

// waitForProfileDeletion waits until both the original Profile and its
// namespace are gone.
//
// The Profile disappearing on its own is not success: the namespace may still
// exist, still be Terminating on its finalizers, or may have outlived the
// Profile without an owner reference, in which case garbage collection will
// never remove it and the operation is finished for review instead. A
// replacement object under the same name and a failed read are both handled
// as unfinished work, never as absence.
//
// Two complete both-absent observations, separated by a poll interval, are
// required before the deletion is reported as complete: a single observation
// can be taken between the removal of the Profile and the creation of a
// replacement. This is not an atomic guarantee. A new object created after
// the final observation is outside the scope of this adapter: the console
// cannot bind several objects atomically, and the platform's RBAC and
// admission policy remain the fence against a reparent, a replacement or a
// cross-namespace owner reference during the deletion.
func (s *Service) waitForProfileDeletion(ctx context.Context, ev profileDeleteEvidence) error {
	absentRounds := 0
	return s.wait(ctx, func() (bool, error) {
		profile, err := s.Dynamic.Resource(Profiles).Get(ctx, ev.ProfileName, metav1.GetOptions{})
		profileAbsent := false
		if apierrors.IsNotFound(err) {
			profileAbsent = true
		} else if err != nil {
			return false, safe(err)
		} else if profile.GetUID() != types.UID(ev.ProfileUID) {
			return false, fmt.Errorf("a replacement Profile appeared during the deletion: %w", ErrConflict)
		}
		ns, nsErr := s.Typed.CoreV1().Namespaces().Get(ctx, ev.NamespaceName, metav1.GetOptions{})
		nsAbsent := false
		if apierrors.IsNotFound(nsErr) {
			nsAbsent = true
		} else if nsErr != nil {
			return false, safe(nsErr)
		} else if ns.UID != types.UID(ev.NamespaceUID) {
			return false, fmt.Errorf("a replacement namespace appeared during the deletion: %w", ErrConflict)
		}
		if !profileAbsent || !nsAbsent {
			// Anything that is still present resets the streak of complete
			// absence observations.
			absentRounds = 0
			if profileAbsent && ns != nil && !profileDeleteOwnedByProfile(ns.OwnerReferences, types.UID(ev.ProfileUID)) {
				// The namespace is still there and is no longer owned by the
				// deleted Profile, so garbage collection will never remove
				// it. That needs an operator plan.
				return false, fmt.Errorf("the Profile namespace outlived its Profile and is no longer owned by it: %w", ErrConflict)
			}
			return false, nil
		}
		absentRounds++
		if absentRounds < 2 {
			// One more complete observation, separated by a poll interval, is
			// required before the deletion is reported as complete.
			return false, nil
		}
		return true, nil
	})
}

// profileDeleteError distinguishes an explicit API refusal, which proves that
// nothing was deleted, from a transport failure, whose outcome is unknown.
// Only the explicit refusal may release the operation lock automatically.
func profileDeleteError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, model.ErrRejectedBeforeMutation):
		return err
	case apierrors.IsForbidden(err):
		return fmt.Errorf("%w: Kubernetes API denied the Profile deletion", model.ErrRejectedBeforeMutation)
	case apierrors.IsNotFound(err):
		// Not a definite refusal: the Profile may already be gone, so the
		// outcome stays unresolved and the fence is not released.
		return fmt.Errorf("%w: the Profile disappeared before the deletion was confirmed", ErrConflict)
	case apierrors.IsConflict(err):
		return fmt.Errorf("%w: %w: the Profile changed before the deletion was applied", ErrConflict, model.ErrRejectedBeforeMutation)
	case apierrors.IsTooManyRequests(err):
		return fmt.Errorf("%w: Kubernetes API throttled the Profile deletion", model.ErrRejectedBeforeMutation)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err), apierrors.IsUnauthorized(err), apierrors.IsMethodNotSupported(err), apierrors.IsRequestEntityTooLargeError(err), apierrors.IsUnsupportedMediaType(err):
		return fmt.Errorf("%w: Kubernetes API rejected the Profile deletion", model.ErrRejectedBeforeMutation)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		// The request may have reached the API server. Keep this ambiguous so
		// the operation stays open for review; never call it rejected.
		return errors.New("Kubernetes API Profile deletion outcome is unknown")
	}
}
