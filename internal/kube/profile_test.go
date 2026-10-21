package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
)

// setProfileOwner rewrites the owner identity of the stored Profile.
func setProfileOwner(t *testing.T, api *crAPI, name, owner string) {
	t.Helper()
	p := api.get("profiles", "", name)
	if p == nil {
		t.Fatalf("profile %s missing", name)
	}
	if err := unstructured.SetNestedField(p.Object, owner, "spec", "owner", "name"); err != nil {
		t.Fatal(err)
	}
	api.put("profiles", p)
}

// setNamespaceOwner rewrites the owner annotation the Profile controller
// maintains on the namespace.
func setNamespaceOwner(t *testing.T, k *fake.Clientset, name, owner string) {
	t.Helper()
	ns, err := k.CoreV1().Namespaces().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ns.Annotations == nil {
		ns.Annotations = map[string]string{}
	}
	ns.Annotations["owner"] = owner
	if _, err = k.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// setProfileHard rewrites the declared hard quota map of the stored Profile.
func setProfileHard(t *testing.T, api *crAPI, name string, hard map[string]any) {
	t.Helper()
	p := api.get("profiles", "", name)
	if p == nil {
		t.Fatalf("profile %s missing", name)
	}
	if err := unstructured.SetNestedMap(p.Object, hard, "spec", "resourceQuotaSpec", "hard"); err != nil {
		t.Fatal(err)
	}
	api.put("profiles", p)
}

// profileDigest returns the canonical configuration digest the console would
// observe for a Profile.
func profileDigest(t *testing.T, api *crAPI, name string) string {
	t.Helper()
	p := api.get("profiles", "", name)
	if p == nil {
		t.Fatalf("profile %s missing", name)
	}
	return profileConfigDigest(p)
}

// quotaController emulates the only installed-controller behavior a quota
// update depends on: the declared hard map is mirrored into the Profile-owned
// resource quota. It never changes the owner, the namespace or any role
// binding, because the owner is immutable and this operation changes no
// access.
func quotaController(k *fake.Clientset, api *crAPI) {
	api.after = func(obj *unstructured.Unstructured) {
		if obj.GetKind() != "Profile" {
			return
		}
		ctx := context.Background()
		hard, _, _ := unstructured.NestedMap(obj.Object, "spec", "resourceQuotaSpec", "hard")
		rl := corev1.ResourceList{}
		for key, v := range hard {
			q, err := resource.ParseQuantity(fmt.Sprint(v))
			if err != nil {
				continue
			}
			rl[corev1.ResourceName(key)] = q
		}
		quota := &corev1.ResourceQuota{
			ObjectMeta: metav1.ObjectMeta{Name: "kf-resource-quota", Namespace: obj.GetName(), UID: "quota-1", OwnerReferences: owner(obj.GetUID(), "Profile", obj.GetName())},
			Spec:       corev1.ResourceQuotaSpec{Hard: rl},
			Status:     corev1.ResourceQuotaStatus{Hard: rl},
		}
		if _, err := k.CoreV1().ResourceQuotas(obj.GetName()).Create(ctx, quota, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
			_, _ = k.CoreV1().ResourceQuotas(obj.GetName()).Update(ctx, quota, metav1.UpdateOptions{})
		}
	}
}

// updateAction builds a valid profile.update action for the fixture Profile
// "alice", using the digest the console would have observed.
func updateAction(t *testing.T, api *crAPI, quota map[string]string) model.Action {
	t.Helper()
	return model.Action{Kind: "profile.update", Name: "alice", Owner: "alice@example.test", Quota: quota, BaseConfigDigest: profileDigest(t, api, "alice")}
}

// --- name validation and uniqueness ---

func TestProfileCreateNameBoundaryAndGrammar(t *testing.T) {
	s, api, _ := testService(t)
	// A 63 character label is the longest Kubernetes namespace name.
	long := strings.Repeat("a", 63)
	if _, err := s.build(context.Background(), model.Action{Kind: "profile.create", Name: long, Owner: "owner@example.test"}); err != nil {
		t.Fatalf("63 character name rejected: %v", err)
	}
	tooLong := strings.Repeat("a", 64)
	_, err := s.build(context.Background(), model.Action{Kind: "profile.create", Name: tooLong, Owner: "owner@example.test"})
	if err == nil || !strings.Contains(err.Error(), "at most 63 characters") {
		t.Fatalf("64 character name accepted: %v", err)
	}
	for _, name := range []string{"Team", "team.name", "team_name", "team name", "-team", "team-", "team$name", "team/name", "team@example", "tëam"} {
		t.Run(name, func(t *testing.T) {
			_, err := s.build(context.Background(), model.Action{Kind: "profile.create", Name: name, Owner: "owner@example.test"})
			if err == nil || !strings.Contains(err.Error(), "lowercase DNS label") {
				t.Fatalf("invalid name accepted: %v", err)
			}
		})
	}
	assertNoWrites(t, api)
}

func TestProfileCreateRejectsDuplicateProfileAndNamespace(t *testing.T) {
	s, api, k := testService(t)
	ctx := context.Background()
	// The fixture already owns the Profile "alice".
	_, err := s.build(ctx, model.Action{Kind: "profile.create", Name: "alice", Owner: "owner@example.test"})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate Profile accepted: %v", err)
	}
	// An unrelated namespace is never adopted.
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team", UID: "namespace-team"}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}}
	if _, err = k.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = s.build(ctx, model.Action{Kind: "profile.create", Name: "team", Owner: "owner@example.test"})
	if err == nil || !strings.Contains(err.Error(), "existing namespaces cannot be adopted") {
		t.Fatalf("existing namespace adopted: %v", err)
	}
	assertNoWrites(t, api)
}

// The owner of a new Profile must be an email address. The check is syntax
// only: no identity provider or directory lookup happens here.
func TestProfileCreateRequiresEmailOwnerIdentity(t *testing.T) {
	s, api, _ := testService(t)
	for _, owner := range []string{"", "alice", "alice@example.test ", " Alice <alice@example.test>", "alice@example.test, bob@example.test", "alice@", "@example.test", "alice@example.test (comment)", "\"alice\"@example.test"} {
		t.Run(owner, func(t *testing.T) {
			_, err := s.build(context.Background(), model.Action{Kind: "profile.create", Name: "team", Owner: owner})
			if err == nil || !strings.Contains(err.Error(), "a valid owner email address is required") {
				t.Fatalf("non-email owner accepted: %v", err)
			}
		})
	}
	if _, err := s.build(context.Background(), model.Action{Kind: "profile.create", Name: "team", Owner: "alice@example.test"}); err != nil {
		t.Fatalf("valid email owner rejected: %v", err)
	}
	assertNoWrites(t, api)
}

// --- profile.update: quota only, owner read-only ---

func TestProfileUpdateChangesOnlyTheQuota(t *testing.T) {
	s, api, _ := testService(t)
	ctx := context.Background()
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2", "requests.ephemeral-storage": "5Gi"})
	w, err := s.build(ctx, updateAction(t, api, map[string]string{"requests.cpu": "4", "requests.ephemeral-storage": "5Gi"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.ops) != 1 || w.ops[0].Path != "/spec/resourceQuotaSpec/hard" {
		t.Fatalf("unexpected ops: %+v", w.ops)
	}
	for _, op := range w.ops {
		if strings.Contains(op.Path, "owner") {
			t.Fatalf("owner patch emitted: %+v", w.ops)
		}
	}
	hard, ok := w.ops[0].Value.(map[string]any)
	if !ok {
		t.Fatalf("unexpected hard value: %#v", w.ops[0].Value)
	}
	if hard["requests.cpu"] != "4" {
		t.Fatalf("supported key not updated: %v", hard)
	}
	if hard["requests.ephemeral-storage"] != "5Gi" {
		t.Fatalf("unmanaged key not preserved exactly: %v", hard)
	}
	assertNoWrites(t, api)
}

// The owner of an existing Profile is immutable: the installed controller
// refuses an owner transfer, so the adapter refuses it before any mutation.
func TestProfileUpdateRefusesOwnerTransfer(t *testing.T) {
	s, api, k := testService(t)
	ctx := context.Background()
	action := updateAction(t, api, map[string]string{"requests.cpu": "4"})
	action.Owner = "attacker@example.test"
	_, err := s.build(ctx, action)
	if err == nil || !strings.Contains(err.Error(), "Profile owner transfer is unsupported by the installed controller contract") {
		t.Fatalf("owner transfer accepted: %v", err)
	}
	// A legacy username owner that is left unchanged is still accepted, so an
	// existing Profile with a configured username can have its quota edited.
	setProfileOwner(t, api, "alice", "CaseSensitive-Username")
	setNamespaceOwner(t, k, "alice", "CaseSensitive-Username")
	legacy := model.Action{Kind: "profile.update", Name: "alice", Owner: "CaseSensitive-Username", Quota: map[string]string{"requests.cpu": "4"}, BaseConfigDigest: profileDigest(t, api, "alice")}
	if _, err = s.build(ctx, legacy); err != nil {
		t.Fatalf("unchanged legacy owner refused: %v", err)
	}
	assertNoWrites(t, api)
}

func TestProfileUpdateRejectsQuotaRemovalByOmission(t *testing.T) {
	s, api, _ := testService(t)
	ctx := context.Background()
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2", "requests.ephemeral-storage": "5Gi"})
	// An existing managed key must be supplied: omitting it never removes it.
	action := updateAction(t, api, map[string]string{"requests.ephemeral-storage": "5Gi"})
	_, err := s.build(ctx, action)
	if err == nil || !strings.Contains(err.Error(), "quota key removal is unsupported by this adapter") {
		t.Fatalf("omitted quota key accepted: %v", err)
	}
	// An unmanaged key may only be round-tripped with an identical value.
	action = updateAction(t, api, map[string]string{"requests.cpu": "2", "requests.ephemeral-storage": "6Gi"})
	_, err = s.build(ctx, action)
	if err == nil || !strings.Contains(err.Error(), "changing an unsupported quota key is unsupported") {
		t.Fatalf("unmanaged quota key changed: %v", err)
	}
	// An unsupported key that does not exist yet is never introduced.
	action = updateAction(t, api, map[string]string{"requests.cpu": "2", "example.com/gpu": "1"})
	_, err = s.build(ctx, action)
	if err == nil || !strings.Contains(err.Error(), "unsupported quota resource") {
		t.Fatalf("unsupported quota key accepted: %v", err)
	}
	// An empty quota is never a removal request and never a silent no-op.
	action = updateAction(t, api, nil)
	_, err = s.build(ctx, action)
	if err == nil || !strings.Contains(err.Error(), "quota removal is unsupported") {
		t.Fatalf("empty quota accepted: %v", err)
	}
	assertNoWrites(t, api)
}

// A semantically identical result is not a change and is refused instead of
// producing a pointless write and a misleading success.
func TestProfileUpdateRefusesNoOp(t *testing.T) {
	s, api, _ := testService(t)
	ctx := context.Background()
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2", "requests.ephemeral-storage": "5120Mi"})
	// The same values, and the same quantity in another spelling, are no-ops.
	for _, quota := range []map[string]string{
		{"requests.cpu": "2", "requests.ephemeral-storage": "5120Mi"},
		{"requests.cpu": "2", "requests.ephemeral-storage": "5Gi"},
	} {
		action := updateAction(t, api, quota)
		_, err := s.build(ctx, action)
		if err == nil || !strings.Contains(err.Error(), "Profile quota already matches the requested values") {
			t.Fatalf("no-op update accepted: %v", err)
		}
	}
	assertNoWrites(t, api)
}

// The reviewed configuration digest is the precondition of a quota update: a
// form filled from an older observation must not overwrite a concurrent change.
func TestProfileUpdateRejectsStaleConfigDigest(t *testing.T) {
	s, api, _ := testService(t)
	ctx := context.Background()
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2"})
	action := updateAction(t, api, map[string]string{"requests.cpu": "4"})
	// The observed digest is stale: the quota changed after the form was
	// filled.
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "8"})
	_, err := s.build(ctx, action)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("stale digest accepted: %v", err)
	}
	// A missing digest is refused as well.
	missing := updateAction(t, api, map[string]string{"requests.cpu": "16"})
	missing.BaseConfigDigest = ""
	_, err = s.build(ctx, missing)
	if err == nil || !strings.Contains(err.Error(), "the reviewed Profile configuration digest is required") {
		t.Fatalf("missing digest accepted: %v", err)
	}
	assertNoWrites(t, api)
}

// The canonical configuration digest covers the owner identity and the
// declared quota only. It is stable across resource version and metadata
// churn and across quantity spellings, and it changes when the owner or the
// declared quota changes.
func TestProfileConfigDigestIsStableAndSensitive(t *testing.T) {
	_, api, _ := testService(t)
	base := api.get("profiles", "", "alice")
	first := profileConfigDigest(base)
	// Resource version, creation timestamp, labels and annotations churn.
	churn := base.DeepCopy()
	churn.SetResourceVersion("99")
	churn.SetCreationTimestamp(metav1.NewTime(time.Now().UTC()))
	churn.SetLabels(map[string]string{"team": "research"})
	churn.SetAnnotations(map[string]string{"note": "churn"})
	if profileConfigDigest(churn) != first {
		t.Fatal("digest changed with resource version or metadata churn")
	}
	// The same quantity in another spelling is the same configuration.
	withQuota := base.DeepCopy()
	if err := unstructured.SetNestedMap(withQuota.Object, map[string]any{"requests.cpu": "2"}, "spec", "resourceQuotaSpec", "hard"); err != nil {
		t.Fatal(err)
	}
	equivalent := withQuota.DeepCopy()
	if err := unstructured.SetNestedMap(equivalent.Object, map[string]any{"requests.cpu": "2000m"}, "spec", "resourceQuotaSpec", "hard"); err != nil {
		t.Fatal(err)
	}
	if profileConfigDigest(equivalent) != profileConfigDigest(withQuota) {
		t.Fatal("digest changed with a quantity spelling")
	}
	// A changed owner changes the digest.
	owner := withQuota.DeepCopy()
	if err := unstructured.SetNestedField(owner.Object, "other@example.test", "spec", "owner", "name"); err != nil {
		t.Fatal(err)
	}
	if profileConfigDigest(owner) == profileConfigDigest(withQuota) {
		t.Fatal("digest did not change with the owner")
	}
	// A changed quota changes the digest.
	quota := withQuota.DeepCopy()
	if err := unstructured.SetNestedMap(quota.Object, map[string]any{"requests.cpu": "4"}, "spec", "resourceQuotaSpec", "hard"); err != nil {
		t.Fatal(err)
	}
	if profileConfigDigest(quota) == profileConfigDigest(withQuota) {
		t.Fatal("digest did not change with the declared quota")
	}
}

func TestProfileUpdateRequiresAnExistingProfile(t *testing.T) {
	s, api, _ := testService(t)
	_, err := s.build(context.Background(), model.Action{Kind: "profile.update", Name: "missing", Owner: "alice@example.test", BaseConfigDigest: "x"})
	if err == nil || !strings.Contains(err.Error(), "matching Profile required") {
		t.Fatalf("missing Profile accepted: %v", err)
	}
	assertNoWrites(t, api)
}

// A replaced namespace is never adopted, neither between preview and
// execution nor during the convergence wait.
func TestProfileUpdateRefusesNamespaceReplacement(t *testing.T) {
	s, api, k := testService(t)
	ctx := context.Background()
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2"})
	prepared, err := s.Prepare(ctx, updateAction(t, api, map[string]string{"requests.cpu": "4"}))
	if err != nil {
		t.Fatal(err)
	}
	// The work reviewed against the original namespace.
	w, err := s.build(ctx, updateAction(t, api, map[string]string{"requests.cpu": "4"}))
	if err != nil {
		t.Fatal(err)
	}
	// The namespace is replaced by one that looks complete but is a different
	// object.
	if err = k.CoreV1().Namespaces().Delete(ctx, "alice", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	replacement := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "alice", UID: types.UID("namespace-replaced"), Annotations: map[string]string{"owner": "alice@example.test"}, OwnerReferences: owner("profile-1", "Profile", "alice")},
		Status:     corev1.NamespaceStatus{Phase: corev1.NamespaceActive},
	}
	if _, err = k.CoreV1().Namespaces().Create(ctx, replacement, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// The reviewed namespace identity is refused before any write.
	if err = s.revalidateProfileUpdate(ctx, w); !errors.Is(err, ErrConflict) {
		t.Fatalf("replaced namespace passed revalidation: %v", err)
	}
	// The plan prepared against the original namespace is stale.
	err = s.Execute(ctx, prepared, func(string) error { return nil })
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("replaced namespace was not refused: %v", err)
	}
	if !errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("replaced namespace was not a definite rejection: %v", err)
	}
	assertNoWrites(t, api)
}

// The update must not report success before the installed controller has
// reconciled the declared quota into the Profile-owned resource quota.
func TestProfileUpdateWaitsForQuotaConvergence(t *testing.T) {
	s, api, _ := testService(t)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2"})
	prepared, err := s.Prepare(ctx, updateAction(t, api, map[string]string{"requests.cpu": "4"}))
	if err != nil {
		t.Fatal(err)
	}
	// No controller: the resource quota is never reconciled.
	err = s.Execute(ctx, prepared, func(string) error { return nil })
	if err == nil {
		t.Fatal("an unreconciled quota reported success")
	}
	// The patch was applied, so the outcome is unknown and the operation is
	// held for review instead of being called a definite rejection.
	if errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("a convergence timeout was reported as a definite rejection: %v", err)
	}
	cur := api.get("profiles", "", "alice")
	hard, _, _ := unstructured.NestedMap(cur.Object, "spec", "resourceQuotaSpec", "hard")
	if hard["requests.cpu"] != "4" {
		t.Fatalf("the reviewed quota was not applied: %v", hard)
	}
}

func TestExecuteProfileUpdateConverges(t *testing.T) {
	s, api, k := testService(t)
	ctx := context.Background()
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2"})
	quotaController(k, api)
	prepared, err := s.Prepare(ctx, updateAction(t, api, map[string]string{"requests.cpu": "4"}))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Execute(ctx, prepared, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	cur := api.get("profiles", "", "alice")
	owner, _, _ := unstructured.NestedString(cur.Object, "spec", "owner", "name")
	if owner != "alice@example.test" {
		t.Fatalf("owner changed: %q", owner)
	}
	hard, _, _ := unstructured.NestedMap(cur.Object, "spec", "resourceQuotaSpec", "hard")
	if hard["requests.cpu"] != "4" {
		t.Fatalf("quota not updated: %v", hard)
	}
}

// A failed durable checkpoint and a failed read are definite no-mutation
// rejections: the operation fails and its target lock is released.
func TestProfileUpdatePreMutationFailuresAreDefiniteRejections(t *testing.T) {
	s, api, _ := testService(t)
	ctx := context.Background()
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2"})
	prepared, err := s.Prepare(ctx, updateAction(t, api, map[string]string{"requests.cpu": "4"}))
	if err != nil {
		t.Fatal(err)
	}
	// A failed durable checkpoint stops the operation before any write.
	err = s.Execute(ctx, prepared, func(string) error { return errors.New("disk unavailable") })
	if err == nil {
		t.Fatal("failed checkpoint accepted")
	}
	if !errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("checkpoint failure was not rejected before mutation: %v", err)
	}
	assertNoWrites(t, api)
	// A Profile that cannot be read is refused before any write as well.
	api.remove("profiles", "", "alice")
	err = s.Execute(ctx, prepared, func(string) error { return nil })
	if err == nil {
		t.Fatal("unreadable Profile accepted")
	}
	if !errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("read failure was not rejected before mutation: %v", err)
	}
	assertNoWrites(t, api)
}

// A change injected while the durable checkpoint ran is refused: the Profile
// is revalidated after the checkpoint and before the patch.
func TestProfileUpdatePostCheckpointStateChangeIsRefused(t *testing.T) {
	s, api, _ := testService(t)
	ctx := context.Background()
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2"})
	prepared, err := s.Prepare(ctx, updateAction(t, api, map[string]string{"requests.cpu": "4"}))
	if err != nil {
		t.Fatal(err)
	}
	err = s.Execute(ctx, prepared, func(string) error {
		// A concurrent quota edit lands while the checkpoint runs.
		setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "16"})
		return nil
	})
	if err == nil {
		t.Fatal("a concurrent change was written over")
	}
	if !errors.Is(err, model.ErrRejectedBeforeMutation) {
		t.Fatalf("concurrent change was not a definite rejection: %v", err)
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent change was not reported as a conflict: %v", err)
	}
	assertNoWrites(t, api)
}

// A definite API rejection changed nothing and is reported as rejected before
// mutation. An ambiguous transport failure stays unknown and is held for
// review.
func TestProfileUpdatePatchFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		reason    metav1.StatusReason
		raw       string
		rejected  bool
		ambiguous bool
	}{
		{name: "forbidden", status: 403, reason: metav1.StatusReasonForbidden, rejected: true},
		{name: "not-found", status: 404, reason: metav1.StatusReasonNotFound, rejected: true},
		{name: "conflict", status: 409, reason: metav1.StatusReasonConflict, rejected: true},
		{name: "invalid", status: 422, reason: metav1.StatusReasonInvalid, rejected: true},
		{name: "throttled", status: 429, reason: metav1.StatusReasonTooManyRequests, rejected: true},
		{name: "transport", raw: "PRIVATE_UPSTREAM_DETAIL", ambiguous: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, api, k := testService(t)
			ctx := context.Background()
			setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2"})
			quotaController(k, api)
			prepared, err := s.Prepare(ctx, updateAction(t, api, map[string]string{"requests.cpu": "4"}))
			if err != nil {
				t.Fatal(err)
			}
			api.patchStatus = tc.status
			api.patchReason = tc.reason
			api.patchMessage = "rejected"
			api.patchRaw = tc.raw
			err = s.Execute(ctx, prepared, func(string) error { return nil })
			if err == nil {
				t.Fatal("failed patch reported success")
			}
			if tc.rejected && !errors.Is(err, model.ErrRejectedBeforeMutation) {
				t.Fatalf("definite rejection was not classified as rejected before mutation: %v", err)
			}
			if tc.ambiguous && errors.Is(err, model.ErrRejectedBeforeMutation) {
				t.Fatalf("ambiguous failure was classified as a definite rejection: %v", err)
			}
			if strings.Contains(err.Error(), "PRIVATE_UPSTREAM_DETAIL") {
				t.Fatalf("raw upstream detail leaked: %v", err)
			}
		})
	}
}

func TestProfileQuotaChangeCompatibilityIsPreserved(t *testing.T) {
	s, api, _ := testService(t)
	ctx := context.Background()
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2", "requests.ephemeral-storage": "5Gi"})
	w, err := s.build(ctx, legacyQuotaAction(t, api, "alice", map[string]string{"requests.cpu": "4", "requests.ephemeral-storage": "5Gi"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(w.ops) != 1 || w.ops[0].Path != "/spec/resourceQuotaSpec/hard" {
		t.Fatalf("unexpected ops: %+v", w.ops)
	}
	hard, _ := w.ops[0].Value.(map[string]any)
	if hard["requests.ephemeral-storage"] != "5Gi" || hard["requests.cpu"] != "4" {
		t.Fatalf("unexpected hard map: %v", hard)
	}
	assertNoWrites(t, api)
}

// --- inventory projection ---

func TestInventoryProjectsSafeProfileDetails(t *testing.T) {
	s, api, _ := testService(t)
	p := api.get("profiles", "", "alice")
	p.SetCreationTimestamp(metav1.NewTime(time.Now().UTC()))
	p.SetResourceVersion("7")
	// A managed key, an unmanaged key, an unexpected key shape and a value
	// that is not a quantity.
	if err := unstructured.SetNestedMap(p.Object, map[string]any{
		"requests.cpu":               "2",
		"requests.ephemeral-storage": "5Gi",
		"evil key":                   "1",
		"requests.secret":            "not-a-quantity",
	}, "spec", "resourceQuotaSpec", "hard"); err != nil {
		t.Fatal(err)
	}
	api.put("profiles", p)
	inv, err := s.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var profile *model.Resource
	for i := range inv.Resources {
		if inv.Resources[i].Kind == "Profile" && inv.Resources[i].Name == "alice" {
			profile = &inv.Resources[i]
		}
	}
	if profile == nil {
		t.Fatal("Profile missing from inventory")
	}
	if profile.Owner != "alice@example.test" {
		t.Fatalf("owner projection: %q", profile.Owner)
	}
	for key, want := range map[string]string{
		"apiVersion":                       "kubeflow.org/v1",
		"ownerKind":                        "User",
		"quota.requests.cpu":               "2",
		"quota.requests.ephemeral-storage": "5Gi",
		"resourceVersion":                  "7",
	} {
		if profile.Details[key] != want {
			t.Fatalf("Profile detail %s: got %q, want %q", key, profile.Details[key], want)
		}
	}
	if profile.Details["createdAt"] == "" {
		t.Fatalf("missing createdAt: %+v", profile.Details)
	}
	if profile.Details["configDigest"] != profileConfigDigest(p) {
		t.Fatalf("configDigest projection: %+v", profile.Details)
	}
	for key, value := range profile.Details {
		if strings.Contains(key, "evil") || strings.Contains(value, "not-a-quantity") {
			t.Fatalf("unsafe projection: %s=%s", key, value)
		}
	}
}

// The inventory digest is the precondition the console echoes back, so it must
// be the same value the server recomputes at prepare time.
func TestInventoryConfigDigestMatchesThePreparedPrecondition(t *testing.T) {
	s, api, _ := testService(t)
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2"})
	inv, err := s.Inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var digest string
	for _, r := range inv.Resources {
		if r.Kind == "Profile" && r.Name == "alice" {
			digest = r.Details["configDigest"]
		}
	}
	if digest == "" {
		t.Fatal("missing configDigest projection")
	}
	if _, err = s.build(context.Background(), model.Action{Kind: "profile.update", Name: "alice", Owner: "alice@example.test", Quota: map[string]string{"requests.cpu": "4"}, BaseConfigDigest: digest}); err != nil {
		t.Fatalf("observed digest was refused: %v", err)
	}
}

// A deployed zero quota is a legitimate declaration: it must round-trip
// unchanged so editing another quota key is never blocked by it, and it must
// never be silently raised.
func TestProfileZeroQuotaRoundTrips(t *testing.T) {
	s, api, k := testService(t)
	ctx := context.Background()
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2", "requests.nvidia.com/gpu": "0", "pods": "0"})
	// The inventory projects the zero values as they are declared.
	inv, err := s.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var details map[string]string
	for _, r := range inv.Resources {
		if r.Kind == "Profile" && r.Name == "alice" {
			details = r.Details
		}
	}
	if details["quota.requests.nvidia.com/gpu"] != "0" || details["quota.pods"] != "0" {
		t.Fatalf("zero quota values were not projected: %+v", details)
	}
	// The canonical digest covers the zero values and is stable.
	digest := details["configDigest"]
	if digest == "" || digest != profileDigest(t, api, "alice") {
		t.Fatalf("config digest does not cover the declared quota: %+v", details)
	}
	// Editing a different key keeps the zero values exactly as declared.
	w, err := s.build(ctx, updateAction(t, api, map[string]string{"requests.cpu": "4", "requests.nvidia.com/gpu": "0", "pods": "0"}))
	if err != nil {
		t.Fatal(err)
	}
	hard, _ := w.ops[0].Value.(map[string]any)
	if hard["requests.nvidia.com/gpu"] != "0" || hard["pods"] != "0" {
		t.Fatalf("zero quota value was changed: %v", hard)
	}
	if hard["requests.cpu"] != "4" {
		t.Fatalf("requested quota key was not applied: %v", hard)
	}
	// A zero value for a managed key is accepted as a request too.
	w, err = s.build(ctx, updateAction(t, api, map[string]string{"requests.cpu": "0", "requests.nvidia.com/gpu": "0", "pods": "0"}))
	if err != nil {
		t.Fatalf("a zero quota value was refused: %v", err)
	}
	hard, _ = w.ops[0].Value.(map[string]any)
	if hard["requests.cpu"] != "0" {
		t.Fatalf("zero quota value was not applied: %v", hard)
	}
	// A negative quota value is never valid.
	negative := updateAction(t, api, map[string]string{"requests.cpu": "-1", "requests.nvidia.com/gpu": "0", "pods": "0"})
	if _, err = s.build(ctx, negative); err == nil || !strings.Contains(err.Error(), "a quota value must be a non-negative Kubernetes quantity") {
		t.Fatalf("negative quota value accepted: %v", err)
	}
	// A new Profile may declare a zero quota as well.
	if _, err = s.build(ctx, model.Action{Kind: "profile.create", Name: "team", Owner: "owner@example.test", Quota: map[string]string{"pods": "0"}}); err != nil {
		t.Fatalf("zero quota refused for a new Profile: %v", err)
	}
	assertNoWrites(t, api)
	_ = k
}

// Requested storage sizes keep the stricter positive rule: a zero or negative
// size is still refused, and only the declared quota values are non-negative.
func TestStorageSizeRemainsStrictlyPositive(t *testing.T) {
	s, api, _ := testService(t)
	for _, size := range []string{"0", "-1Gi"} {
		_, err := s.build(context.Background(), model.Action{Kind: "pvc.create.dynamic", Namespace: "alice", Name: "dyn", StorageClass: "approved", Size: size, AccessMode: "ReadWriteMany"})
		if err == nil || !strings.Contains(err.Error(), "a positive Kubernetes quantity is required") {
			t.Fatalf("storage size %q accepted: %v", size, err)
		}
	}
	assertNoWrites(t, api)
}

// mutateNamespace applies one mutation to the fixture namespace while keeping
// its identity: the UID never changes, so the change must be refused on its
// own merits and not because the object was replaced.
func mutateNamespace(t *testing.T, k *fake.Clientset, mutate func(*corev1.Namespace)) {
	t.Helper()
	ns, err := k.CoreV1().Namespaces().Get(context.Background(), "alice", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	mutate(ns)
	if _, err = k.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// A namespace can keep its identity and still stop being the active,
// Profile-owned namespace the review covered. Every such change must be
// refused before the quota patch is written, with the operation reported as a
// definite rejection and no write at all.
func TestProfileUpdateRefusesNamespaceChangedDuringCheckpoint(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*corev1.Namespace)
		want   string
	}{
		{"terminating", func(ns *corev1.Namespace) {
			now := metav1.NewTime(time.Now())
			ns.DeletionTimestamp = &now
		}, "namespace is not active"},
		{"phase-terminating", func(ns *corev1.Namespace) {
			ns.Status.Phase = corev1.NamespaceTerminating
		}, "namespace is not active"},
		{"controller-reference-removed", func(ns *corev1.Namespace) {
			ns.OwnerReferences = nil
		}, "namespace ownership does not match its Profile"},
		{"owner-annotation-changed", func(ns *corev1.Namespace) {
			ns.Annotations["owner"] = "someone-else@example.test"
		}, "namespace ownership does not match its Profile"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, api, k := testService(t)
			// A bounded context keeps a regression that lets the patch through
			// failing fast instead of waiting for the convergence timeout.
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2"})
			prepared, err := s.Prepare(ctx, updateAction(t, api, map[string]string{"requests.cpu": "4"}))
			if err != nil {
				t.Fatal(err)
			}
			err = s.Execute(ctx, prepared, func(string) error {
				mutateNamespace(t, k, tc.mutate)
				return nil
			})
			if err == nil {
				t.Fatal("a changed namespace was written over")
			}
			// The refusal happens before the patch: the operation is a definite
			// rejection and its target lock is released.
			if !errors.Is(err, model.ErrRejectedBeforeMutation) {
				t.Fatalf("the change was not a definite pre-mutation rejection: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("missing authored reason %q in %v", tc.want, err)
			}
			assertNoWrites(t, api)
		})
	}
}

// The preview shows the change the patch actually writes. A key this adapter
// preserves verbatim is never shown as changed, and no owner or internal
// digest row is ever rendered.
func TestProfileUpdatePreviewShowsOnlyActualQuotaChanges(t *testing.T) {
	s, api, _ := testService(t)
	ctx := context.Background()
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2", "requests.ephemeral-storage": "5120Mi"})
	prepared, err := s.Prepare(ctx, updateAction(t, api, map[string]string{"requests.cpu": "4", "requests.ephemeral-storage": "5Gi"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Changes) != 1 {
		t.Fatalf("unexpected preview changes: %+v", prepared.Changes)
	}
	change := prepared.Changes[0]
	if change.Field != "quota.requests.cpu" || change.Before != "2" || change.After != "4" {
		t.Fatalf("unexpected preview change: %+v", change)
	}
	for _, c := range prepared.Changes {
		if c.Field != "quota.requests.cpu" {
			t.Fatalf("a preserved key was previewed as changed: %+v", prepared.Changes)
		}
		if strings.Contains(strings.ToLower(c.Field), "owner") || strings.Contains(c.Field, "digest") {
			t.Fatalf("an internal or owner row was previewed: %+v", prepared.Changes)
		}
	}
	// The written map still keeps the unmanaged key exactly as declared.
	w, err := s.build(ctx, updateAction(t, api, map[string]string{"requests.cpu": "4", "requests.ephemeral-storage": "5Gi"}))
	if err != nil {
		t.Fatal(err)
	}
	hard, _ := w.ops[0].Value.(map[string]any)
	if hard["requests.ephemeral-storage"] != "5120Mi" {
		t.Fatalf("the unmanaged key was not preserved literally: %v", hard)
	}
	if hard["requests.cpu"] != "4" {
		t.Fatalf("the requested key was not applied: %v", hard)
	}
	assertNoWrites(t, api)
}

// The official Profile CRD declares resourceQuotaSpec.hard as an int-or-string
// map, so a deployed Profile may carry unquoted integer quota values. They
// must be projected, round-tripped and compared by quantity, and an
// operator-managed integer key must be preserved with its original value and
// type.
func TestProfileIntegerQuotaValuesRoundTrip(t *testing.T) {
	s, api, _ := testService(t)
	ctx := context.Background()
	setProfileHard(t, api, "alice", map[string]any{
		"requests.cpu":               int64(2),
		"pods":                       int64(0),
		"requests.ephemeral-storage": int64(1024),
	})
	// The inventory projects every declared value, integer or string.
	inv, err := s.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var details map[string]string
	for _, r := range inv.Resources {
		if r.Kind == "Profile" && r.Name == "alice" {
			details = r.Details
		}
	}
	for key, want := range map[string]string{
		"quota.requests.cpu":               "2",
		"quota.pods":                       "0",
		"quota.requests.ephemeral-storage": "1024",
	} {
		if details[key] != want {
			t.Fatalf("Profile detail %s: got %q, want %q", key, details[key], want)
		}
	}
	// Editing another key keeps the operator-managed integer exactly as it is
	// declared, including its type.
	w, err := s.build(ctx, updateAction(t, api, map[string]string{"requests.cpu": "4", "pods": "0", "requests.ephemeral-storage": "1024"}))
	if err != nil {
		t.Fatal(err)
	}
	hard, _ := w.ops[0].Value.(map[string]any)
	if hard["requests.cpu"] != "4" {
		t.Fatalf("the requested key was not applied: %#v", hard)
	}
	preserved, ok := hard["requests.ephemeral-storage"].(int64)
	if !ok || preserved != 1024 {
		t.Fatalf("the operator-managed integer was not preserved exactly: %#v", hard["requests.ephemeral-storage"])
	}
	// A number and its string spelling are the same value: the update is a
	// no-op and is refused.
	action := updateAction(t, api, map[string]string{"requests.cpu": "2", "pods": "0", "requests.ephemeral-storage": "1024"})
	if _, err = s.build(ctx, action); err == nil || !strings.Contains(err.Error(), "Profile quota already matches the requested values") {
		t.Fatalf("an equivalent number and string update was accepted: %v", err)
	}
	assertNoWrites(t, api)
}

// The canonical digest treats an unquoted integer and its string spelling as
// the same configuration, stays stable across metadata churn and remains
// bound to the observed Profile identity.
func TestProfileConfigDigestTreatsIntegersAndStringsAlike(t *testing.T) {
	_, api, _ := testService(t)
	base := api.get("profiles", "", "alice")
	if err := unstructured.SetNestedMap(base.Object, map[string]any{"requests.cpu": int64(2), "pods": int64(0)}, "spec", "resourceQuotaSpec", "hard"); err != nil {
		t.Fatal(err)
	}
	asInteger := profileConfigDigest(base)
	asString := base.DeepCopy()
	if err := unstructured.SetNestedMap(asString.Object, map[string]any{"requests.cpu": "2", "pods": "0"}, "spec", "resourceQuotaSpec", "hard"); err != nil {
		t.Fatal(err)
	}
	if profileConfigDigest(asString) != asInteger {
		t.Fatal("an integer and its string spelling produced different digests")
	}
	churn := asString.DeepCopy()
	churn.SetResourceVersion("99")
	churn.SetCreationTimestamp(metav1.NewTime(time.Now().UTC()))
	churn.SetLabels(map[string]string{"team": "research"})
	churn.SetAnnotations(map[string]string{"note": "churn"})
	if profileConfigDigest(churn) != asInteger {
		t.Fatal("the digest changed with resource version or metadata churn")
	}
	replacement := asString.DeepCopy()
	replacement.SetUID(types.UID("replacement-profile"))
	if profileConfigDigest(replacement) == asInteger {
		t.Fatal("the digest did not bind the observed Profile identity")
	}
}

// A value that is not a declared quantity is never projected and never
// compared as a quantity.
func TestProfileNonQuantityValuesAreRefused(t *testing.T) {
	for _, value := range []any{map[string]any{"nested": "1"}, []any{"1"}, "not-a-quantity", "", 2.5, math.NaN(), math.Inf(1)} {
		if _, ok := quotaValue(value); ok {
			t.Fatalf("a non-quantity value was accepted: %#v", value)
		}
		if _, ok := canonicalQuantity(value); ok {
			t.Fatalf("a non-quantity value was canonicalized: %#v", value)
		}
	}
	for _, value := range []any{"2", int64(2), int(2), int32(2), json.Number("2"), float64(2)} {
		text, ok := quotaValue(value)
		if !ok || text != "2" {
			t.Fatalf("a legitimate integer value was refused: %#v", value)
		}
	}
}

// The legacy quota kind is the same guarded write under its old name. A
// request that carries no reviewed configuration, which is what an old
// cached console sends, is refused instead of being applied.
func TestLegacyQuotaChangeWithoutReviewedConfigurationIsRefused(t *testing.T) {
	s, api, _ := testService(t)
	ctx := context.Background()
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2"})
	for _, action := range []model.Action{
		{Kind: "profile.quota.change", Name: "alice", Quota: map[string]string{"requests.cpu": "4"}},
		{Kind: "profile.quota.change", Name: "alice", Owner: "alice@example.test", Quota: map[string]string{"requests.cpu": "4"}},
		{Kind: "profile.quota.change", Name: "alice", Quota: map[string]string{"requests.cpu": "4"}, BaseConfigDigest: "stale"},
	} {
		_, err := s.build(ctx, action)
		if err == nil || !strings.Contains(err.Error(), profileReloadMessage) {
			t.Fatalf("an unguarded legacy request was accepted: %v", err)
		}
	}
	// The guarded legacy request is accepted.
	if _, err := s.build(ctx, legacyQuotaAction(t, api, "alice", map[string]string{"requests.cpu": "4"})); err != nil {
		t.Fatalf("a guarded legacy request was refused: %v", err)
	}
	assertNoWrites(t, api)
}

// The plan and the recorded evidence must describe the same Profile. One
// read cannot mix the evidence of two different objects.
func TestProfilePlanUsesOneConsistentProfileRead(t *testing.T) {
	s, api, _ := testService(t)
	ctx := context.Background()
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2"})
	reads := 0
	api.getHook = func(r string, n *unstructured.Unstructured) *unstructured.Unstructured {
		if r != "profiles" {
			return nil
		}
		reads++
		if reads == 1 {
			return nil
		}
		changed := n.DeepCopy()
		if err := unstructured.SetNestedMap(changed.Object, map[string]any{"requests.cpu": "99"}, "spec", "resourceQuotaSpec", "hard"); err != nil {
			t.Fatal(err)
		}
		return changed
	}
	w, err := s.build(ctx, updateAction(t, api, map[string]string{"requests.cpu": "4"}))
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Fatalf("the plan read the Profile %d times", reads)
	}
	dep, ok := evidenceDep(w.ev, "Profile", "", "alice")
	if !ok {
		t.Fatal("missing Profile evidence")
	}
	want := digest(struct {
		UID  types.UID
		Spec any
	}{w.profile.GetUID(), w.profile.Object["spec"]})
	if dep.Digest != want {
		t.Fatal("the plan and the recorded evidence describe different Profiles")
	}
}

// The convergence contract is the merged result that was written. An
// operator-managed key that disappears from the Profile after the patch is a
// conflict, never a converged quota, and the observed resource quota must
// carry the whole merged map.
func TestProfileUpdateConvergenceRequiresTheWholeMergedMap(t *testing.T) {
	// The Profile loses the preserved key after the patch.
	t.Run("profile-map-drift", func(t *testing.T) {
		s, api, k := testService(t)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2", "requests.ephemeral-storage": "5Gi"})
		// Once the patch has landed, every read serves a Profile whose
		// operator-managed key has disappeared.
		api.getHook = func(r string, n *unstructured.Unstructured) *unstructured.Unstructured {
			if r != "profiles" {
				return nil
			}
			hard, _, _ := unstructured.NestedMap(n.Object, "spec", "resourceQuotaSpec", "hard")
			if hard["requests.cpu"] != "4" {
				return nil
			}
			changed := n.DeepCopy()
			if err := unstructured.SetNestedMap(changed.Object, map[string]any{"requests.cpu": "4"}, "spec", "resourceQuotaSpec", "hard"); err != nil {
				t.Fatal(err)
			}
			return changed
		}
		prepared, err := s.Prepare(ctx, updateAction(t, api, map[string]string{"requests.cpu": "4", "requests.ephemeral-storage": "5Gi"}))
		if err != nil {
			t.Fatal(err)
		}
		err = s.Execute(ctx, prepared, func(string) error { return nil })
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("a drifted Profile map was not a conflict: %v", err)
		}
		if errors.Is(err, model.ErrRejectedBeforeMutation) {
			t.Fatalf("a post-write drift was reported as a definite rejection: %v", err)
		}
		_ = k
	})
	// The resource quota never carries the preserved key.
	t.Run("resource-quota-missing-key", func(t *testing.T) {
		s, api, k := testService(t)
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2", "requests.ephemeral-storage": "5Gi"})
		api.after = func(obj *unstructured.Unstructured) {
			if obj.GetKind() != "Profile" {
				return
			}
			hard, _, _ := unstructured.NestedMap(obj.Object, "spec", "resourceQuotaSpec", "hard")
			rl := corev1.ResourceList{}
			for key, v := range hard {
				if key == "requests.ephemeral-storage" {
					continue
				}
				q, err := resource.ParseQuantity(fmt.Sprint(v))
				if err != nil {
					continue
				}
				rl[corev1.ResourceName(key)] = q
			}
			quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "kf-resource-quota", Namespace: obj.GetName(), UID: "quota-1", OwnerReferences: owner(obj.GetUID(), "Profile", obj.GetName())}, Spec: corev1.ResourceQuotaSpec{Hard: rl}, Status: corev1.ResourceQuotaStatus{Hard: rl}}
			if _, err := k.CoreV1().ResourceQuotas(obj.GetName()).Create(context.Background(), quota, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
				_, _ = k.CoreV1().ResourceQuotas(obj.GetName()).Update(context.Background(), quota, metav1.UpdateOptions{})
			}
		}
		prepared, err := s.Prepare(ctx, updateAction(t, api, map[string]string{"requests.cpu": "4", "requests.ephemeral-storage": "5Gi"}))
		if err != nil {
			t.Fatal(err)
		}
		err = s.Execute(ctx, prepared, func(string) error { return nil })
		if err == nil {
			t.Fatal("an incomplete observed quota reported success")
		}
		if errors.Is(err, model.ErrRejectedBeforeMutation) {
			t.Fatalf("a convergence timeout was reported as a definite rejection: %v", err)
		}
	})
}

// A create is refused when the target name is taken while the durable
// checkpoint runs, and a definite API refusal is classified as a rejection
// before any mutation.
func TestProfileCreateRefusesATakenTargetAfterTheCheckpoint(t *testing.T) {
	t.Run("profile-appears", func(t *testing.T) {
		s, api, _ := testService(t)
		ctx := context.Background()
		prepared, err := s.Prepare(ctx, model.Action{Kind: "profile.create", Name: "bob", Owner: "bob@example.test"})
		if err != nil {
			t.Fatal(err)
		}
		err = s.Execute(ctx, prepared, func(string) error {
			// A Profile with the same name appears while the checkpoint runs.
			existing := api.get("profiles", "", "bob")
			if existing == nil {
				existing = &unstructured.Unstructured{Object: map[string]any{"apiVersion": "kubeflow.org/v1", "kind": "Profile", "metadata": map[string]any{"name": "bob", "uid": "other-profile", "resourceVersion": "1"}, "spec": map[string]any{"owner": map[string]any{"kind": "User", "name": "someone@example.test"}}}}
			}
			api.put("profiles", existing)
			return nil
		})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("a taken target was not refused: %v", err)
		}
		if !errors.Is(err, model.ErrRejectedBeforeMutation) {
			t.Fatalf("the refusal was not a definite rejection: %v", err)
		}
	})
	t.Run("namespace-appears", func(t *testing.T) {
		s, api, k := testService(t)
		ctx := context.Background()
		prepared, err := s.Prepare(ctx, model.Action{Kind: "profile.create", Name: "bob", Owner: "bob@example.test"})
		if err != nil {
			t.Fatal(err)
		}
		err = s.Execute(ctx, prepared, func(string) error {
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "bob", UID: "namespace-bob"}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}}
			if _, err := k.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
				t.Errorf("namespace fixture: %v", err)
			}
			return nil
		})
		if err == nil || !strings.Contains(err.Error(), "existing namespaces cannot be adopted") {
			t.Fatalf("an appearing namespace was not refused: %v", err)
		}
		if !errors.Is(err, model.ErrRejectedBeforeMutation) {
			t.Fatalf("the refusal was not a definite rejection: %v", err)
		}
		if api.get("profiles", "", "bob") != nil {
			t.Fatal("an orphan Profile was created")
		}
	})
	for _, tc := range []struct {
		name     string
		status   int
		reason   metav1.StatusReason
		rejected bool
	}{
		{"forbidden", 403, metav1.StatusReasonForbidden, true},
		{"invalid", 422, metav1.StatusReasonInvalid, true},
		{"throttled", 429, metav1.StatusReasonTooManyRequests, true},
		{"transport", 0, metav1.StatusReasonUnknown, false},
	} {
		t.Run("create-"+tc.name, func(t *testing.T) {
			s, api, _ := testService(t)
			ctx := context.Background()
			prepared, err := s.Prepare(ctx, model.Action{Kind: "profile.create", Name: "bob", Owner: "bob@example.test"})
			if err != nil {
				t.Fatal(err)
			}
			api.createStatus = tc.status
			api.createReason = tc.reason
			api.createMessage = "rejected"
			if tc.status == 0 {
				api.createRaw = "PRIVATE_UPSTREAM_DETAIL"
			}
			err = s.Execute(ctx, prepared, func(string) error { return nil })
			if err == nil {
				t.Fatal("a refused create reported success")
			}
			if tc.rejected && !errors.Is(err, model.ErrRejectedBeforeMutation) {
				t.Fatalf("a definite refusal was not classified as rejected before mutation: %v", err)
			}
			if !tc.rejected && errors.Is(err, model.ErrRejectedBeforeMutation) {
				t.Fatalf("an ambiguous failure was classified as a definite rejection: %v", err)
			}
			if strings.Contains(err.Error(), "PRIVATE_UPSTREAM_DETAIL") {
				t.Fatalf("raw upstream detail leaked: %v", err)
			}
		})
	}
}

// A count quota is a whole number of objects. A fraction of a Pod, of a claim
// or of a configured device is refused before anything is written.
func TestQuotaCountsMustBeWholeNumbers(t *testing.T) {
	s, api, _ := testService(t)
	ctx := context.Background()
	s.Config.Notebooks.GPUPresets = []config.GPUPreset{{Name: "a100", ResourceKey: "nvidia.com/gpu"}}
	setProfileHard(t, api, "alice", map[string]any{"requests.cpu": "2", "requests.nvidia.com/gpu": "1"})
	for _, quota := range []map[string]string{
		{"requests.cpu": "4", "pods": "1.5"},
		{"requests.cpu": "4", "persistentvolumeclaims": "0.5"},
		{"requests.cpu": "4", "requests.nvidia.com/gpu": "1.5"},
	} {
		action := updateAction(t, api, quota)
		_, err := s.build(ctx, action)
		if err == nil || !strings.Contains(err.Error(), "a quota count must be a whole number of objects") {
			t.Fatalf("a fractional count was accepted: %v", err)
		}
	}
	// Whole counts, including zero, are accepted.
	action := updateAction(t, api, map[string]string{"requests.cpu": "4", "pods": "0", "requests.nvidia.com/gpu": "2"})
	if _, err := s.build(ctx, action); err != nil {
		t.Fatalf("a whole count was refused: %v", err)
	}
	// A resource measurement is a normal quantity and may be fractional.
	action = updateAction(t, api, map[string]string{"requests.cpu": "1.5", "pods": "0", "requests.nvidia.com/gpu": "2"})
	if _, err := s.build(ctx, action); err != nil {
		t.Fatalf("a fractional resource quantity was refused: %v", err)
	}
	// A new Profile may not declare a fractional count either.
	_, err := s.build(ctx, model.Action{Kind: "profile.create", Name: "team", Owner: "owner@example.test", Quota: map[string]string{"pods": "1.5"}})
	if err == nil || !strings.Contains(err.Error(), "a quota count must be a whole number of objects") {
		t.Fatalf("a fractional count was accepted for a new Profile: %v", err)
	}
	assertNoWrites(t, api)
}

// createConvergenceFixture emulates the installed controller for a created
// Profile. The namespace and the owner role binding are always created; the
// resource quota is created only when a hard map is given.
func createConvergenceFixture(k *fake.Clientset, quota map[string]any) func(*unstructured.Unstructured) {
	return func(p *unstructured.Unstructured) {
		if p.GetKind() != "Profile" {
			return
		}
		ctx := context.Background()
		uid := p.GetUID()
		ownerName, _, _ := unstructured.NestedString(p.Object, "spec", "owner", "name")
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.GetName(), UID: types.UID("ns-" + p.GetName()), Annotations: map[string]string{"owner": ownerName}, OwnerReferences: owner(uid, "Profile", p.GetName())}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}}
		if _, err := k.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
			return
		}
		rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "kubeflow-admin", Namespace: p.GetName(), UID: types.UID("rb-" + p.GetName()), OwnerReferences: owner(uid, "Profile", p.GetName())}, Subjects: []rbacv1.Subject{{Kind: "User", Name: ownerName, APIGroup: "rbac.authorization.k8s.io"}}, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "kubeflow-admin", APIGroup: "rbac.authorization.k8s.io"}}
		if _, err := k.RbacV1().RoleBindings(p.GetName()).Create(ctx, rb, metav1.CreateOptions{}); err != nil {
			return
		}
		if quota == nil {
			return
		}
		rl := corev1.ResourceList{}
		for key, v := range quota {
			q, err := resource.ParseQuantity(fmt.Sprint(v))
			if err != nil {
				continue
			}
			rl[corev1.ResourceName(key)] = q
		}
		q := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "kf-resource-quota", Namespace: p.GetName(), UID: types.UID("q-" + p.GetName()), OwnerReferences: owner(uid, "Profile", p.GetName())}, Spec: corev1.ResourceQuotaSpec{Hard: rl}, Status: corev1.ResourceQuotaStatus{Hard: rl}}
		if _, err := k.CoreV1().ResourceQuotas(p.GetName()).Create(ctx, q, metav1.CreateOptions{}); apierrors.IsAlreadyExists(err) {
			_, _ = k.CoreV1().ResourceQuotas(p.GetName()).Update(ctx, q, metav1.UpdateOptions{})
		}
	}
}

// A create is only successful when the observed resource quota carries the
// reviewed hard map. A missing or a different quota never reports success.
func TestProfileCreateRequiresTheObservedQuota(t *testing.T) {
	action := model.Action{Kind: "profile.create", Name: "bob", Owner: "bob@example.test", Quota: map[string]string{"requests.cpu": "4", "pods": "0"}}
	// The controller creates the namespace and the role binding but no
	// resource quota.
	t.Run("no-resource-quota", func(t *testing.T) {
		s, api, k := testService(t)
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		api.after = createConvergenceFixture(k, nil)
		prepared, err := s.Prepare(ctx, action)
		if err != nil {
			t.Fatal(err)
		}
		err = s.Execute(ctx, prepared, func(string) error { return nil })
		if err == nil {
			t.Fatal("a create without an observed quota reported success")
		}
		// The Profile was written, so the outcome is unknown and the
		// operation is held for review.
		if errors.Is(err, model.ErrRejectedBeforeMutation) {
			t.Fatalf("the post-write failure was reported as a definite rejection: %v", err)
		}
	})
	// The controller creates a resource quota with different values.
	t.Run("wrong-resource-quota", func(t *testing.T) {
		s, api, k := testService(t)
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		defer cancel()
		api.after = createConvergenceFixture(k, map[string]any{"requests.cpu": "8", "pods": "0"})
		prepared, err := s.Prepare(ctx, action)
		if err != nil {
			t.Fatal(err)
		}
		err = s.Execute(ctx, prepared, func(string) error { return nil })
		if err == nil {
			t.Fatal("a create with a different observed quota reported success")
		}
		if errors.Is(err, model.ErrRejectedBeforeMutation) {
			t.Fatalf("the post-write failure was reported as a definite rejection: %v", err)
		}
	})
	// The declared hard map changes after the create.
	t.Run("mutated-profile-hard-map", func(t *testing.T) {
		s, api, k := testService(t)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		api.after = createConvergenceFixture(k, map[string]any{"requests.cpu": "4", "pods": "0"})
		api.getHook = func(r string, n *unstructured.Unstructured) *unstructured.Unstructured {
			if r != "profiles" {
				return nil
			}
			hard, _, _ := unstructured.NestedMap(n.Object, "spec", "resourceQuotaSpec", "hard")
			if hard["requests.cpu"] != "4" {
				return nil
			}
			changed := n.DeepCopy()
			if err := unstructured.SetNestedMap(changed.Object, map[string]any{"requests.cpu": "16", "pods": "0"}, "spec", "resourceQuotaSpec", "hard"); err != nil {
				t.Fatal(err)
			}
			return changed
		}
		prepared, err := s.Prepare(ctx, action)
		if err != nil {
			t.Fatal(err)
		}
		err = s.Execute(ctx, prepared, func(string) error { return nil })
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("a mutated hard map was not refused: %v", err)
		}
	})
	// The controller reconciles the reviewed quota: the create converges.
	t.Run("converged", func(t *testing.T) {
		s, api, k := testService(t)
		ctx := context.Background()
		api.after = createConvergenceFixture(k, map[string]any{"requests.cpu": "4", "pods": "0"})
		prepared, err := s.Prepare(ctx, action)
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Execute(ctx, prepared, func(string) error { return nil }); err != nil {
			t.Fatal(err)
		}
		if api.get("profiles", "", "bob") == nil {
			t.Fatal("profile not created")
		}
	})
}

// --- create convergence ---

// The owner binding of the installed Profile controller may omit the API
// group of its User subject. Both spellings must converge.
func TestProfileCreateConvergesWithEitherRoleBindingSubjectAPIGroup(t *testing.T) {
	for _, group := range []string{"rbac.authorization.k8s.io", ""} {
		t.Run(group, func(t *testing.T) {
			s, api, k := testService(t)
			ctx := context.Background()
			action := model.Action{Kind: "profile.create", Name: "bob", Owner: "bob@example.test", Quota: map[string]string{"requests.cpu": "4"}}
			api.after = func(p *unstructured.Unstructured) {
				if p.GetKind() != "Profile" {
					return
				}
				uid := p.GetUID()
				ownerName, _, _ := unstructured.NestedString(p.Object, "spec", "owner", "name")
				ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: p.GetName(), UID: types.UID("ns-" + p.GetName()), Annotations: map[string]string{"owner": ownerName}, OwnerReferences: owner(uid, "Profile", p.GetName())}, Status: corev1.NamespaceStatus{Phase: corev1.NamespaceActive}}
				if _, err := k.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
					t.Errorf("namespace fixture: %v", err)
				}
				rb := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "kubeflow-admin", Namespace: p.GetName(), UID: types.UID("rb-" + p.GetName()), OwnerReferences: owner(uid, "Profile", p.GetName())}, Subjects: []rbacv1.Subject{{Kind: "User", Name: ownerName, APIGroup: group}}, RoleRef: rbacv1.RoleRef{Kind: "ClusterRole", Name: "kubeflow-admin", APIGroup: "rbac.authorization.k8s.io"}}
				if _, err := k.RbacV1().RoleBindings(p.GetName()).Create(ctx, rb, metav1.CreateOptions{}); err != nil {
					t.Errorf("rolebinding fixture: %v", err)
				}
				rl := corev1.ResourceList{}
				for key, v := range action.Quota {
					q, err := quantity(v)
					if err != nil {
						t.Errorf("bad quota fixture: %v", err)
					}
					rl[corev1.ResourceName(key)] = q
				}
				q := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "kf-resource-quota", Namespace: p.GetName(), UID: types.UID("q-" + p.GetName()), OwnerReferences: owner(uid, "Profile", p.GetName())}, Spec: corev1.ResourceQuotaSpec{Hard: rl}, Status: corev1.ResourceQuotaStatus{Hard: rl}}
				if _, err := k.CoreV1().ResourceQuotas(p.GetName()).Create(ctx, q, metav1.CreateOptions{}); err != nil {
					t.Errorf("quota fixture: %v", err)
				}
			}
			p, err := s.Prepare(ctx, action)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Execute(ctx, p, func(string) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if api.get("profiles", "", "bob") == nil {
				t.Fatal("profile not created")
			}
		})
	}
}
