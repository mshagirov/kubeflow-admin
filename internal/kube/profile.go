package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/mail"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"kubeflow-admin/internal/model"
)

// profileName validates a Profile name against the Kubernetes namespace label
// grammar the Profile controller requires: at most 63 characters, only
// lowercase letters, digits and interior hyphens, starting and ending with an
// alphanumeric character. Dots, underscores, uppercase letters, spaces and
// every other special character are rejected here, whatever the console form
// allows, because the Profile name is also the namespace name.
func profileName(v string) error {
	if len(v) > 63 {
		return errors.New("Profile name must be at most 63 characters")
	}
	if len(validation.IsDNS1123Label(v)) > 0 {
		return errors.New("Profile name must be a lowercase DNS label")
	}
	return nil
}

// validateOwnerEmail validates the exact Kubeflow owner identity. The console
// collects an email address, so the backend requires one: the submitted text
// must parse as a single address whose text is exactly the submitted value.
// A display name, an angle-address form, a comment, a second address or
// surrounding whitespace is rejected, so a display name can never be injected
// into spec.owner.name.
func validateOwnerEmail(v string) error {
	if v == "" || len(v) > 253 || strings.TrimSpace(v) != v {
		return errors.New("a valid owner email address is required")
	}
	at := strings.LastIndex(v, "@")
	if at <= 0 || at == len(v)-1 {
		return errors.New("a valid owner email address is required")
	}
	addr, err := mail.ParseAddress(v)
	if err != nil || addr.Name != "" || addr.Address != v {
		return errors.New("a valid owner email address is required")
	}
	return nil
}

// quotaKeys returns the quota resource keys this adapter may author in
// addition to the built-in quota resources.
func (s *Service) quotaKeys() []string {
	keys := []string{}
	for _, p := range s.Config.Notebooks.GPUPresets {
		keys = append(keys, "requests."+p.ResourceKey)
	}
	return keys
}

// prepareProfile plans every Profile-scoped action. The kinds have separate
// contracts: a new Profile and a guarded quota update. The legacy
// profile.quota.change kind is an alias of the guarded update: it keeps its
// name for existing consoles but is no longer an unguarded quota write.
func (s *Service) prepareProfile(ctx context.Context, a model.Action, w *work) error {
	switch a.Kind {
	case "profile.create":
		return s.prepareProfileCreate(ctx, a, w)
	case "profile.update", "profile.quota.change":
		return s.prepareProfileUpdate(ctx, a, w)
	}
	return errors.New("unsupported action kind")
}

// prepareProfileCreate plans a new Profile and its namespace. The name must be
// unique: an existing Profile and an existing unrelated namespace are both
// rejected, because a namespace is never adopted.
func (s *Service) prepareProfileCreate(ctx context.Context, a model.Action, w *work) error {
	if err := profileName(a.Name); err != nil {
		return err
	}
	if err := validateOwnerEmail(a.Owner); err != nil {
		return err
	}
	hard := map[string]any{}
	var err error
	if len(a.Quota) > 0 {
		hard, err = quota(a.Quota, s.quotaKeys())
		if err != nil {
			return err
		}
	}
	if _, err = s.Dynamic.Resource(Profiles).Get(ctx, a.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		if err == nil {
			return ErrConflict
		}
		return safe(err)
	}
	if _, err = s.Typed.CoreV1().Namespaces().Get(ctx, a.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		if err == nil {
			return errors.New("existing namespaces cannot be adopted")
		}
		return safe(err)
	}
	spec := map[string]any{"owner": map[string]any{"kind": "User", "name": a.Owner}}
	if len(hard) > 0 {
		spec["resourceQuotaSpec"] = map[string]any{"hard": hard}
	}
	w.profile = &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubeflow.org/v1", "kind": "Profile", "metadata": map[string]any{"name": a.Name}, "spec": spec}}
	w.prepared.Warnings = append(w.prepared.Warnings, "The exact owner identity is retained; directory existence is not verified.")
	w.prepared.Warnings = append(w.prepared.Warnings, "Setup verification covers the Profile-owned active namespace, owner admin RoleBinding and requested quota; service-account and Istio policy contents are not independently verified.")
	return nil
}

// profileConfig binds the canonical configuration to the observed Profile's
// immutable identity, owner and declared hard quota. It never carries the resource
// version, metadata, status or any other field, so its digest is stable
// across unrelated churn and changes when the target identity, owner or
// declared quota changes.
type profileConfig struct {
	Name      string            `json:"name"`
	UID       types.UID         `json:"uid"`
	OwnerKind string            `json:"ownerKind"`
	OwnerName string            `json:"ownerName"`
	Hard      map[string]string `json:"hard"`
}

// profileConfigOf reads the canonical configuration of a Profile. Quantities
// are normalized, so two spellings of the same quantity produce the same
// configuration and the same digest.
func profileConfigOf(p *unstructured.Unstructured) profileConfig {
	kind, _, _ := unstructured.NestedString(p.Object, "spec", "owner", "kind")
	name, _, _ := unstructured.NestedString(p.Object, "spec", "owner", "name")
	hard, _, _ := unstructured.NestedMap(p.Object, "spec", "resourceQuotaSpec", "hard")
	out := profileConfig{Name: p.GetName(), UID: p.GetUID(), OwnerKind: kind, OwnerName: name, Hard: map[string]string{}}
	for k, v := range hard {
		out.Hard[k] = normalizedQuantity(v)
	}
	return out
}

// profileConfigDigest is the canonical digest of a Profile configuration. It
// is projected in the inventory and echoed back by the console as the
// precondition of a quota update.
func profileConfigDigest(p *unstructured.Unstructured) string {
	return digest(profileConfigOf(p))
}

// normalizedQuantity renders a quota value as its canonical Kubernetes
// quantity. Two representations of the same quantity, an unquoted integer and
// its string spelling, produce the same canonical text and therefore the same
// digest. A value that is not a declared quantity still has to change the
// digest when it changes, and it must be stable across observations.
func normalizedQuantity(v any) string {
	if q, ok := canonicalQuantity(v); ok {
		return q
	}
	return "invalid:" + digest(v)
}

// quotaValue extracts the declared text of a quota value. The official
// Profile CRD declares resourceQuotaSpec.hard as an int-or-string map, so a
// deployed value may be a JSON string or a JSON integer. Anything else, a
// map, an arbitrary text, a non-finite number or a fractional number, is not
// a declared quota value and is refused.
func quotaValue(v any) (string, bool) {
	switch value := v.(type) {
	case string:
		text := strings.TrimSpace(value)
		if text == "" {
			return "", false
		}
		if _, err := resource.ParseQuantity(text); err != nil {
			return "", false
		}
		return text, true
	case int:
		return strconv.Itoa(value), true
	case int32:
		return strconv.FormatInt(int64(value), 10), true
	case int64:
		return strconv.FormatInt(value, 10), true
	case json.Number:
		text := value.String()
		if _, err := strconv.ParseInt(text, 10, 64); err != nil {
			return "", false
		}
		return text, true
	case float32:
		return floatQuotaValue(float64(value))
	case float64:
		return floatQuotaValue(value)
	}
	return "", false
}

// floatQuotaValue accepts a whole finite number as an integer value. A
// fraction or a non-finite number is not a value the int-or-string CRD field
// accepts and is refused.
func floatQuotaValue(v float64) (string, bool) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v != math.Trunc(v) {
		return "", false
	}
	return strconv.FormatInt(int64(v), 10), true
}

// canonicalQuantity renders a declared quota value as its canonical
// Kubernetes quantity, so an integer and its string spelling, or two
// spellings of one quantity, produce the same text.
func canonicalQuantity(v any) (string, bool) {
	text, ok := quotaValue(v)
	if !ok {
		return "", false
	}
	q, err := resource.ParseQuantity(text)
	if err != nil {
		return "", false
	}
	return q.String(), true
}

// sameQuotaValue reports whether two declared quota values are the same
// quantity. An unquoted integer and its string spelling are the same value.
func sameQuotaValue(a, b any) bool {
	aq, aok := canonicalQuantity(a)
	bq, bok := canonicalQuantity(b)
	if aok && bok {
		return quantityEqual(aq, bq)
	}
	if aok != bok {
		return false
	}
	return same(a, b)
}

// profileReloadMessage is the authored refusal for a request that carries no
// reviewed Profile configuration. The legacy console sent neither the owner
// nor the configuration digest, so its quota write is refused instead of being
// applied to a configuration nobody reviewed.
const profileReloadMessage = "Reload the console and review the current Profile configuration before changing quotas"

// prepareProfileUpdate plans a guarded quota update of an existing Profile.
//
// The owner identity is immutable. The installed Profile controller refuses an
// owner change on an existing namespace (upstream kubeflow issue 4890), so
// this adapter never patches spec.owner, never patches the namespace and never
// patches a role binding. The requested owner must equal the current owner
// exactly; any difference is refused as an unsupported contract.
//
// The reviewed configuration digest must match the live Profile, so a console
// form filled from an older observation cannot overwrite a concurrent quota
// change. The legacy profile.quota.change kind is routed here as well: it is
// the same guarded write under its old name.
func (s *Service) prepareProfileUpdate(ctx context.Context, a model.Action, w *work) error {
	legacy := a.Kind == "profile.quota.change"
	if err := profileName(a.Name); err != nil {
		return err
	}
	// One read: the Profile the scope validated is the Profile the plan is
	// built from, so the recorded evidence and the plan can never describe two
	// different objects.
	state, p, err := s.scopeState(ctx, a.Name, &w.ev)
	if err != nil {
		return err
	}
	if p.GetDeletionTimestamp() != nil {
		return errors.New("Profile is terminating")
	}
	owner, _, _ := unstructured.NestedString(p.Object, "spec", "owner", "name")
	kind, _, _ := unstructured.NestedString(p.Object, "spec", "owner", "kind")
	if owner == "" || kind != "User" {
		return errors.New("unsupported Profile owner contract")
	}
	// The owner is read-only. A legacy Profile whose owner is a configured
	// username keeps that exact owner; only a different value is refused.
	if a.Owner == "" {
		if legacy {
			return errors.New(profileReloadMessage)
		}
		return errors.New("exact nonempty owner identity required")
	}
	if a.Owner != owner {
		return errors.New("Profile owner transfer is unsupported by the installed controller contract")
	}
	// The configuration the administrator reviewed must still be the live one.
	if a.BaseConfigDigest == "" {
		if legacy {
			return errors.New(profileReloadMessage)
		}
		return errors.New("the reviewed Profile configuration digest is required")
	}
	if a.BaseConfigDigest != profileConfigDigest(p) {
		return ErrConflict
	}
	old, _, err := unstructured.NestedMap(p.Object, "spec", "resourceQuotaSpec", "hard")
	if err != nil {
		return errors.New("unsupported Profile quota shape")
	}
	if len(a.Quota) == 0 {
		return errors.New("quota removal is unsupported")
	}
	hard, err := mergeQuota(old, a.Quota, s.quotaKeys())
	if err != nil {
		return err
	}
	// A semantically identical result is not a change. Refusing it keeps a
	// pointless write and a misleading success out of the operation history.
	if sameQuota(hard, old) {
		return errors.New("Profile quota already matches the requested values")
	}
	ops := []patchOp{}
	if _, found, _ := unstructured.NestedMap(p.Object, "spec", "resourceQuotaSpec"); !found {
		ops = append(ops, patchOp{"add", "/spec/resourceQuotaSpec", map[string]any{"hard": hard}})
	} else {
		// Only the hard map is replaced. Scopes, scope selectors and every
		// other field of the resource quota spec are left untouched.
		ops = append(ops, patchOp{"add", "/spec/resourceQuotaSpec/hard", hard})
	}
	w.profile = p
	w.profileOwner = owner
	w.profileQuota = old
	w.profileNamespace = state.UID
	w.prepared.TargetUID = string(p.GetUID())
	w.prepared.ResourceVersion = p.GetResourceVersion()
	w.ev.Target = digest(p.Object["spec"])
	// The exact owner and quota evidence reviewed for this plan is recorded
	// separately so a concurrent change to either is detected, not merged.
	w.ev.add("ProfileOwner", "", a.Name, struct {
		UID  types.UID
		Kind string
		Name string
	}{p.GetUID(), kind, owner})
	w.ev.add("ProfileQuota", "", a.Name, old)
	w.ops = ops
	w.prepared.Warnings = append(w.prepared.Warnings, "The Profile owner identity is read-only: the installed Profile controller does not support transferring a Profile to another owner, so this operation changes the declared quota only.")
	w.prepared.Warnings = append(w.prepared.Warnings, "This operation changes no access: it does not modify the owner identity, the namespace or any role binding, and it grants or revokes no access in the identity provider or anywhere else.")
	w.prepared.Warnings = append(w.prepared.Warnings, "Quota keys this console does not manage are preserved exactly as they are declared. Removing a quota key is unsupported.")
	return nil
}

// revalidateProfileUpdate re-reads the Profile and confirms that the exact
// owner and quota evidence reviewed in the plan is still current, and that the
// reviewed namespace has not been replaced. A changed owner, a changed quota,
// a replaced object, a newer resourceVersion or a replaced namespace is a
// conflict: the plan is stale and a new plan must be prepared.
func (s *Service) revalidateProfileUpdate(ctx context.Context, w *work) error {
	cur, err := s.Dynamic.Resource(Profiles).Get(ctx, w.prepared.Action.Name, metav1.GetOptions{})
	if err != nil {
		return safe(err)
	}
	if cur.GetUID() != w.profile.GetUID() {
		return ErrConflict
	}
	if cur.GetDeletionTimestamp() != nil {
		return errors.New("Profile is terminating")
	}
	if cur.GetResourceVersion() != w.profile.GetResourceVersion() {
		return ErrConflict
	}
	owner, _, _ := unstructured.NestedString(cur.Object, "spec", "owner", "name")
	kind, _, _ := unstructured.NestedString(cur.Object, "spec", "owner", "kind")
	if kind != "User" || owner != w.profileOwner {
		return ErrConflict
	}
	hard, _, err := unstructured.NestedMap(cur.Object, "spec", "resourceQuotaSpec", "hard")
	if err != nil {
		return errors.New("unsupported Profile quota shape")
	}
	if !sameQuota(hard, w.profileQuota) {
		return ErrConflict
	}
	// A namespace replaced between preview and execution is never adopted,
	// even when it looks complete and owned.
	state, err := s.stableNamespace(ctx, w.prepared.Action.Name)
	if err != nil {
		return err
	}
	if state.UID != w.profileNamespace {
		return ErrConflict
	}
	// The namespace must still be the active, Profile-owned namespace the
	// review covered. An external actor can keep the same identity and still
	// terminate it, move its phase, drop its controller reference or repoint
	// its owner annotation. Such a change must be refused before the patch is
	// written, not discovered by the later convergence wait.
	if state.Terminating || state.Phase != corev1.NamespaceActive {
		return errors.New("namespace is not active")
	}
	if !controlledBy(state.Owners, w.profile.GetUID()) {
		return errors.New("namespace ownership does not match its Profile")
	}
	if state.Owner != w.profileOwner {
		return errors.New("namespace ownership does not match its Profile")
	}
	return nil
}

// executeProfileUpdate applies the reviewed quota patch and then waits for the
// Profile contract to converge: the owner identity, the namespace identity
// and ownership, and the requested quota. A patched quota is never a
// successful outcome on its own.
func (s *Service) executeProfileUpdate(ctx context.Context, w *work, cp func(string) error) (err error) {
	mutationAttempted := false
	defer func() {
		// Every failure before the patch request is a definite no-mutation
		// rejection: the operation is failed and its target lock is released.
		if err != nil && !mutationAttempted {
			err = errors.Join(model.ErrRejectedBeforeMutation, err)
		}
	}()
	a := w.prepared.Action
	if err = s.revalidateProfileUpdate(ctx, w); err != nil {
		return err
	}
	if err = checkpoint(cp, "update-profile"); err != nil {
		return err
	}
	// The checkpoint performs durable storage and authorization work. Revalidate
	// the reviewed owner, quota and namespace identity after it, so a change
	// injected while the checkpoint ran is refused instead of written over.
	if err = s.revalidateProfileUpdate(ctx, w); err != nil {
		return err
	}
	b, _ := json.Marshal(conditional(string(w.profile.GetUID()), w.profile.GetResourceVersion(), w.ops))
	mutationAttempted = true
	if _, err = s.Dynamic.Resource(Profiles).Patch(ctx, a.Name, types.JSONPatchType, b, metav1.PatchOptions{}); err != nil {
		return profilePatchError(err)
	}
	// The convergence contract is the merged result that was actually
	// written, not the requested values: an operator-managed key the adapter
	// preserved is part of the declared quota and must still be there.
	merged := mergedHardQuota(w)
	mergedText := mergedQuotaText(merged)
	return s.wait(ctx, func() (bool, error) {
		cur, err := s.Dynamic.Resource(Profiles).Get(ctx, a.Name, metav1.GetOptions{})
		if err != nil {
			return false, safe(err)
		}
		if cur.GetUID() != w.profile.GetUID() || cur.GetDeletionTimestamp() != nil {
			return false, ErrConflict
		}
		// The owner is immutable: it must still be exactly the reviewed owner.
		converged, err := profileOwnerObserved(cur, w.profileOwner)
		if err != nil || !converged {
			return false, err
		}
		state, err := s.stableNamespace(ctx, a.Name)
		if err != nil {
			return false, err
		}
		if state.UID != w.profileNamespace {
			return false, fmt.Errorf("namespace identity changed during the Profile update: %w", ErrConflict)
		}
		if state.Terminating || state.Phase != corev1.NamespaceActive {
			return false, nil
		}
		if !controlledBy(state.Owners, w.profile.GetUID()) {
			return false, ErrConflict
		}
		if state.Owner != w.profileOwner {
			// The namespace owner annotation must still describe the Profile
			// owner: an annotation that names somebody else is not a
			// converged Profile.
			return false, nil
		}
		// The declared hard map must still be exactly the merged result that
		// was written. A map that drifted after the patch, for example an
		// operator-managed key that admission or another actor removed, is a
		// conflict and never a converged quota.
		declared, _, err := unstructured.NestedMap(cur.Object, "spec", "resourceQuotaSpec", "hard")
		if err != nil {
			return false, errors.New("unsupported Profile quota shape")
		}
		if !sameQuota(declared, merged) {
			return false, fmt.Errorf("Profile quota changed after the update: %w", ErrConflict)
		}
		// The observed resource quota must carry the whole merged map, the
		// operator-managed keys included.
		return s.quotaObserved(ctx, a.Name, mergedText, w.profile.GetUID())
	})
}

// mergedQuotaText renders the merged hard map as canonical quantity text for
// the observed resource quota comparison. A value that is not a declared
// quantity is left out: it can never be observed as a quantity.
func mergedQuotaText(hard map[string]any) map[string]string {
	out := map[string]string{}
	for k, v := range hard {
		if q, ok := canonicalQuantity(v); ok {
			out[k] = q
		}
	}
	return out
}

// profileOwnerObserved reports whether the live Profile carries exactly the
// reviewed owner identity. The owner kind must remain User: a Profile owned by
// anything else is outside this adapter's contract.
func profileOwnerObserved(cur *unstructured.Unstructured, owner string) (bool, error) {
	got, _, _ := unstructured.NestedString(cur.Object, "spec", "owner", "name")
	kind, _, _ := unstructured.NestedString(cur.Object, "spec", "owner", "kind")
	if kind != "User" {
		return false, ErrConflict
	}
	return got == owner, nil
}

// grantsUser reports whether a role binding grants its role to an exact User
// subject. The API group of a User subject is optional: the installed Profile
// controller may omit it in the owner binding.
func grantsUser(r *rbacv1.RoleBinding, user string) bool {
	for _, sub := range r.Subjects {
		if sub.Kind != "User" || sub.Name != user {
			continue
		}
		if sub.APIGroup == "" || sub.APIGroup == "rbac.authorization.k8s.io" {
			return true
		}
	}
	return false
}

// executeProfileCreate creates the reviewed Profile and waits for the
// installed controller to converge the owned namespace, the owner admin role
// binding and the declared quota.
//
// The target name is re-read immediately after the durable checkpoint and
// immediately before the create: an existing Profile or namespace is refused
// rather than adopted, so no orphan Profile is left behind. The check and the
// create are two separate API calls and cannot be atomic against an external
// privileged writer; a Profile created concurrently under the same name is
// refused by the API server itself and no second object is written.
func (s *Service) executeProfileCreate(ctx context.Context, w *work, cp func(string) error) (err error) {
	mutationAttempted := false
	defer func() {
		// Every failure before the create request is a definite no-mutation
		// rejection: the operation is failed and its target lock is released.
		if err != nil && !mutationAttempted {
			err = errors.Join(model.ErrRejectedBeforeMutation, err)
		}
	}()
	a := w.prepared.Action
	if err = checkpoint(cp, "create-profile"); err != nil {
		return err
	}
	// The checkpoint performs durable storage and authorization work. The
	// target name must still be free immediately before the create.
	if _, err = s.Dynamic.Resource(Profiles).Get(ctx, a.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		if err == nil {
			return ErrConflict
		}
		return safe(err)
	}
	if _, err = s.Typed.CoreV1().Namespaces().Get(ctx, a.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		if err == nil {
			return errors.New("existing namespaces cannot be adopted")
		}
		return safe(err)
	}
	mutationAttempted = true
	p, err := s.Dynamic.Resource(Profiles).Create(ctx, w.profile, metav1.CreateOptions{})
	if err != nil {
		return profilePatchError(err)
	}
	// The reviewed hard map is the validated map the prepared Profile carries.
	// A create has no patch operation, so it is never derived from one.
	reviewed := preparedHardQuota(w)
	reviewedText := mergedQuotaText(reviewed)
	return s.wait(ctx, func() (bool, error) {
		cur, err := s.Dynamic.Resource(Profiles).Get(ctx, a.Name, metav1.GetOptions{})
		if err != nil {
			return false, safe(err)
		}
		ownerName, _, _ := unstructured.NestedString(cur.Object, "spec", "owner", "name")
		ownerKind, _, _ := unstructured.NestedString(cur.Object, "spec", "owner", "kind")
		if cur.GetUID() != p.GetUID() || cur.GetDeletionTimestamp() != nil || ownerName != a.Owner || ownerKind != "User" {
			return false, ErrConflict
		}
		// The declared hard map must still be exactly the reviewed one.
		declared, _, err := unstructured.NestedMap(cur.Object, "spec", "resourceQuotaSpec", "hard")
		if err != nil {
			return false, errors.New("unsupported Profile quota shape")
		}
		if !sameQuota(declared, reviewed) {
			return false, fmt.Errorf("Profile quota changed after the create: %w", ErrConflict)
		}
		ns, err := s.Typed.CoreV1().Namespaces().Get(ctx, a.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		if err != nil {
			return false, safe(err)
		}
		if ns.Status.Phase != corev1.NamespaceActive || ns.DeletionTimestamp != nil {
			return false, nil
		}
		if !controlledBy(ns.OwnerReferences, p.GetUID()) || ns.Annotations["owner"] != a.Owner {
			return false, ErrConflict
		}
		rb, err := s.Typed.RbacV1().RoleBindings(a.Name).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false, safe(err)
		}
		access := false
		for i := range rb.Items {
			r := &rb.Items[i]
			if r.RoleRef.Kind != "ClusterRole" || r.RoleRef.Name != "kubeflow-admin" || r.RoleRef.APIGroup != "rbac.authorization.k8s.io" || r.DeletionTimestamp != nil || !controlledBy(r.OwnerReferences, p.GetUID()) {
				continue
			}
			// The API group of a User subject is optional: the installed
			// Profile controller may omit it in the owner binding.
			if grantsUser(r, a.Owner) {
				access = true
			}
		}
		if !access {
			return false, nil
		}
		return s.quotaObserved(ctx, a.Name, reviewedText, p.GetUID())
	})
}

// preparedHardQuota returns the hard quota map of the prepared Profile: the
// validated values the plan carries. A create has no patch operation, so its
// reviewed quota is read from the prepared object and never from a patch.
func preparedHardQuota(w *work) map[string]any {
	if w.profile == nil {
		return map[string]any{}
	}
	hard, _, _ := unstructured.NestedMap(w.profile.Object, "spec", "resourceQuotaSpec", "hard")
	return hard
}

// profilePatchError classifies the outcome of the Profile patch. A definite
// API rejection changed nothing and is reported as rejected before mutation.
// Every other failure is ambiguous: the request may have reached the API
// server, so the outcome stays unknown and the Profile lock is held for
// review. The message never repeats the raw API text.
func profilePatchError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case errors.Is(err, model.ErrRejectedBeforeMutation):
		return err
	case apierrors.IsForbidden(err):
		return fmt.Errorf("%w: Kubernetes API denied the Profile patch", model.ErrRejectedBeforeMutation)
	case apierrors.IsUnauthorized(err):
		return fmt.Errorf("%w: Kubernetes API rejected the Profile patch", model.ErrRejectedBeforeMutation)
	case apierrors.IsAlreadyExists(err):
		return fmt.Errorf("%w: %w: Profile already exists", ErrConflict, model.ErrRejectedBeforeMutation)
	case apierrors.IsNotFound(err):
		return fmt.Errorf("%w: Kubernetes Profile not found", model.ErrRejectedBeforeMutation)
	case apierrors.IsConflict(err):
		return fmt.Errorf("%w: %w: Profile changed before the patch was applied", ErrConflict, model.ErrRejectedBeforeMutation)
	case apierrors.IsTooManyRequests(err):
		return fmt.Errorf("%w: Kubernetes API throttled the Profile patch", model.ErrRejectedBeforeMutation)
	case apierrors.IsInvalid(err), apierrors.IsBadRequest(err), apierrors.IsMethodNotSupported(err), apierrors.IsRequestEntityTooLargeError(err), apierrors.IsUnsupportedMediaType(err):
		return fmt.Errorf("%w: Kubernetes API rejected the Profile patch", model.ErrRejectedBeforeMutation)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	default:
		// The request may have reached the API server. Keep this ambiguous so
		// the Profile lock remains held for review; never call it rejected.
		return errors.New("Kubernetes API Profile patch outcome is unknown")
	}
}

// sameQuota compares two hard quota maps by quantity value so a different
// spelling, or an unquoted integer and its string spelling, is not mistaken
// for a change, while a genuinely different or missing key is.
func sameQuota(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok {
			return false
		}
		if !sameQuotaValue(av, bv) {
			return false
		}
	}
	return true
}
