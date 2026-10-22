package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"kubeflow-admin/internal/model"
)

func provisioningPlan(id string, a model.Action) model.Plan {
	return model.Plan{ID: id, Actor: "issuer|admin", Prepared: model.Prepared{Action: a}}
}

func provisioningOp(plan model.Plan, id string) model.Operation {
	now := time.Now().UTC()
	return model.Operation{ID: id, PlanID: plan.ID, Actor: plan.Actor, Target: PrimaryTarget(plan.Prepared.Action), Action: plan.Prepared.Action.Kind, State: "queued", Step: "queued", CreatedAt: now, UpdatedAt: now}
}

func acceptedLocks(t *testing.T, s *Store, operationID string) []string {
	t.Helper()
	rows, err := s.db.Query(`SELECT target FROM operation_targets WHERE operation_id=? AND active=1 ORDER BY target`, operationID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			t.Fatal(err)
		}
		out = append(out, target)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func containsTarget(targets []string, want string) bool {
	for _, target := range targets {
		if target == want {
			return true
		}
	}
	return false
}

func podDefaultRequest(name string) *model.PodDefaultRequest {
	return &model.PodDefaultRequest{Name: name, MountPath: "/home/jovyan/data"}
}

func cleanupEvidence(t *testing.T, namespace string, entries ...map[string]string) json.RawMessage {
	t.Helper()
	if namespace != "" {
		for i := range entries {
			if _, exists := entries[i]["namespace"]; !exists {
				entries[i]["namespace"] = namespace
			}
		}
	}
	data, err := json.Marshal(map[string]any{"version": 2, "podDefaults": entries})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func cleanupEvidenceNames(t *testing.T, namespace string, names ...string) json.RawMessage {
	t.Helper()
	entries := make([]map[string]string, 0, len(names))
	for _, name := range names {
		entries = append(entries, map[string]string{"name": name, "uid": "uid-" + name, "resourceVersion": "1"})
	}
	return cleanupEvidence(t, namespace, entries...)
}

func pvcDeleteProvisioningAction(namespace, name string) model.Action {
	return model.Action{Kind: pvcDeleteKind, Namespace: namespace, Name: name, TargetUID: "uid-" + name, TargetResourceVersion: "7", ConfirmName: name, Reason: "verified unused"}
}

// The standalone static PV action is cluster scoped. Its name is the PV name
// and it never acquires a namespace, PVC or second PV lock.
func TestPVCreateStaticIsClusterScopedAndCanonical(t *testing.T) {
	a := model.Action{Kind: "pv.create.static", Name: "static-pv", PVName: "static-pv", Template: "nfs-a", StorageClass: "approved", NFSPath: "/exports/alice/data", DirectoryUID: "1000", DirectoryGID: "1000"}
	if got := PrimaryTarget(a); got != "pv:static-pv" {
		t.Fatalf("primary target = %q", got)
	}
	plan := provisioningPlan("pv-create-plan", a)
	if targets := Targets(plan); len(targets) != 1 || targets[0] != "pv:static-pv" {
		t.Fatalf("static PV targets = %v", targets)
	}
	s := database(t)
	ctx := context.Background()
	op := provisioningOp(plan, "pv-create-op")
	if _, created, err := s.CreateOperation(ctx, op, plan, "pv-create-key"); err != nil || !created {
		t.Fatal(err)
	}
	if locks := acceptedLocks(t, s, op.ID); len(locks) != 1 || locks[0] != "pv:static-pv" {
		t.Fatalf("accepted locks = %v", locks)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("static PV operation failed integrity: %v", err)
	}
	// The same canonical PV name cannot be registered twice.
	second := plan
	second.ID = "pv-create-plan-2"
	secondOp := provisioningOp(second, "pv-create-op-2")
	if _, _, err := s.CreateOperation(ctx, secondOp, second, "pv-create-key-2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate PV registration accepted: %v", err)
	}
}

func TestPVCreateStaticRejectsForeignScopeAndDependencies(t *testing.T) {
	base := model.Action{Kind: "pv.create.static", Name: "static-pv", Template: "nfs-a", StorageClass: "approved"}
	cases := map[string]func(*model.Action){
		"empty name":         func(a *model.Action) { a.Name = "" },
		"invalid name":       func(a *model.Action) { a.Name = "Static/PV" },
		"namespace":          func(a *model.Action) { a.Namespace = "alice" },
		"typed confirmation": func(a *model.Action) { a.ConfirmName = "static-pv" },
		"force":              func(a *model.Action) { a.Force = true },
		"observed uid":       func(a *model.Action) { a.TargetUID = "uid" },
		"observed rv":        func(a *model.Action) { a.TargetResourceVersion = "1" },
		"storage yaml":       func(a *model.Action) { a.StorageYAML = "apiVersion: v1" },
		"other pv name":      func(a *model.Action) { a.PVName = "other-pv" },
		"poddefault":         func(a *model.Action) { a.PodDefault = podDefaultRequest("mount-data") },
		"cleanup ack":        func(a *model.Action) { a.ConfirmPodDefaultCleanup = true },
	}
	for name, edit := range cases {
		t.Run(name, func(t *testing.T) {
			s := database(t)
			ctx := context.Background()
			a := base
			edit(&a)
			plan := provisioningPlan("invalid-"+name, a)
			op := provisioningOp(plan, "invalid-op-"+name)
			if _, _, err := s.CreateOperation(ctx, op, plan, "invalid-key-"+name); !errors.Is(err, ErrInvalidTarget) {
				t.Fatalf("invalid static PV action accepted: %v", err)
			}
			var claims, locks int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_claims`).Scan(&claims); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM operation_targets`).Scan(&locks); err != nil {
				t.Fatal(err)
			}
			if claims != 0 || locks != 0 {
				t.Fatalf("refused action left claims=%d locks=%d", claims, locks)
			}
		})
	}
}

// A bound PVC locks its canonical claim, the selected PV and the optional
// PodDefault. The same PodDefault name cannot be created by a different claim
// in the same namespace, and a different claim cannot take the same PV.
func TestPVCCreateBoundLocksClaimPVAndPodDefault(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	a := model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "data", PVName: "static-pv", TargetUID: "pv-uid", TargetResourceVersion: "3", Template: "nfs-a", StorageClass: "approved", NFSPath: "/exports/alice/data", DirectoryUID: "1000", DirectoryGID: "1000", PodDefault: podDefaultRequest("mount-data")}
	plan := provisioningPlan("bound-plan", a)
	if err := s.Put(ctx, "plans", plan.ID, plan); err != nil {
		t.Fatal(err)
	}
	targets := Targets(plan)
	for _, want := range []string{"pvc:alice/data", "pv:static-pv", "poddefault:alice/mount-data"} {
		if !containsTarget(targets, want) {
			t.Fatalf("bound targets %v missing %s", targets, want)
		}
	}
	if len(targets) != 3 {
		t.Fatalf("bound targets = %v", targets)
	}
	op := provisioningOp(plan, "bound-op")
	if _, created, err := s.CreateOperation(ctx, op, plan, "bound-key"); err != nil || !created {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("bound operation failed integrity: %v", err)
	}

	conflicts := map[string]model.Action{
		"same poddefault other claim": {Kind: "pvc.create.bound", Namespace: "alice", Name: "other", PVName: "other-pv", TargetUID: "other-pv-uid", TargetResourceVersion: "2", PodDefault: podDefaultRequest("mount-data")},
		"same pv other claim":         {Kind: "pvc.create.bound", Namespace: "alice", Name: "other", PVName: "static-pv", TargetUID: "pv-uid", TargetResourceVersion: "3"},
		"dynamic same poddefault":     {Kind: "pvc.create.dynamic", Namespace: "alice", Name: "other", StorageClass: "approved", Size: "10Gi", AccessMode: "ReadWriteMany", PodDefault: podDefaultRequest("mount-data")},
	}
	for name, action := range conflicts {
		t.Run(name, func(t *testing.T) {
			p := provisioningPlan("conflict-"+strings.ReplaceAll(name, " ", "-"), action)
			o := provisioningOp(p, "conflict-op-"+strings.ReplaceAll(name, " ", "-"))
			if _, _, err := s.CreateOperation(ctx, o, p, "conflict-key-"+strings.ReplaceAll(name, " ", "-")); !errors.Is(err, ErrConflict) {
				t.Fatalf("conflicting dependency accepted: %v", err)
			}
		})
	}
	// The same PodDefault name in another namespace is independent.
	otherNamespace := model.Action{Kind: "pvc.create.bound", Namespace: "bob", Name: "data", PVName: "bob-pv", TargetUID: "bob-pv-uid", TargetResourceVersion: "2", PodDefault: podDefaultRequest("mount-data")}
	p := provisioningPlan("other-namespace-plan", otherNamespace)
	if _, created, err := s.CreateOperation(ctx, provisioningOp(p, "other-namespace-op"), p, "other-namespace-key"); err != nil || !created {
		t.Fatalf("independent PodDefault namespace refused: %v", err)
	}
}

func TestPVCCreateBoundValidationAndOptionalPodDefault(t *testing.T) {
	valid := model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "data", PVName: "static-pv", TargetUID: "pv-uid", TargetResourceVersion: "3"}
	withPodDefault := valid
	withPodDefault.PodDefault = podDefaultRequest("mount-data")
	invalid := map[string]model.Action{
		"missing namespace":  func() model.Action { a := valid; a.Namespace = ""; return a }(),
		"invalid namespace":  func() model.Action { a := valid; a.Namespace = "Alice/Team"; return a }(),
		"missing pv":         func() model.Action { a := valid; a.PVName = ""; return a }(),
		"invalid pv":         func() model.Action { a := valid; a.PVName = "pv/other"; return a }(),
		"force":              func() model.Action { a := valid; a.Force = true; return a }(),
		"confirm name":       func() model.Action { a := valid; a.ConfirmName = "data"; return a }(),
		"missing target uid": func() model.Action { a := valid; a.TargetUID = ""; return a }(),
		"missing target rv":  func() model.Action { a := valid; a.TargetResourceVersion = ""; return a }(),
		"storage yaml":       func() model.Action { a := valid; a.StorageYAML = "apiVersion: v1"; return a }(),
		"cleanup ack":        func() model.Action { a := valid; a.ConfirmPodDefaultCleanup = true; return a }(),
		"empty poddefault": func() model.Action {
			a := valid
			a.PodDefault = &model.PodDefaultRequest{Name: "", MountPath: "/home/jovyan/data"}
			return a
		}(),
		"invalid pd name": func() model.Action { a := valid; a.PodDefault = podDefaultRequest("mount/Data"); return a }(),
	}
	for name, action := range invalid {
		t.Run(name, func(t *testing.T) {
			s := database(t)
			p := provisioningPlan("invalid-bound-"+name, action)
			if _, _, err := s.CreateOperation(context.Background(), provisioningOp(p, "invalid-bound-op-"+name), p, "invalid-bound-key-"+name); !errors.Is(err, ErrInvalidTarget) {
				t.Fatalf("invalid bound action accepted: %v", err)
			}
		})
	}
	// PodDefault is optional, and the legacy dynamic action may use it too.
	for _, a := range []model.Action{valid, withPodDefault} {
		t.Run(a.Kind+"-poddefault="+fmt.Sprint(a.PodDefault != nil), func(t *testing.T) {
			s := database(t)
			p := provisioningPlan("accepted-bound-"+fmt.Sprint(a.PodDefault != nil), a)
			if _, created, err := s.CreateOperation(context.Background(), provisioningOp(p, "accepted-bound-op-"+fmt.Sprint(a.PodDefault != nil)), p, "accepted-bound-key-"+fmt.Sprint(a.PodDefault != nil)); err != nil || !created {
				t.Fatalf("valid bound action refused: %v", err)
			}
		})
	}
	// A PodDefault request on an unrelated action is refused rather than
	// silently ignored and turned into a stale write.
	s := database(t)
	unrelated := model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "lab", PodDefault: podDefaultRequest("mount-data")}
	p := provisioningPlan("unrelated-poddefault", unrelated)
	if _, _, err := s.CreateOperation(context.Background(), provisioningOp(p, "unrelated-poddefault-op"), p, "unrelated-poddefault-key"); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("PodDefault on an unrelated action accepted: %v", err)
	}
}

func TestPVCDeletePodDefaultCleanupLocksAndValidation(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	a := pvcDeleteProvisioningAction("alice", "data")
	a.ConfirmPodDefaultCleanup = true
	plan := provisioningPlan("cleanup-plan", a)
	plan.Prepared.Data = cleanupEvidenceNames(t, "alice", "mount-b", "mount-a")
	if err := s.Put(ctx, "plans", plan.ID, plan); err != nil {
		t.Fatal(err)
	}
	targets := Targets(plan)
	if targets[0] != "pvc:alice/data" {
		t.Fatalf("cleanup primary = %v", targets)
	}
	for _, want := range []string{"poddefault:alice/mount-a", "poddefault:alice/mount-b", "namespace:alice"} {
		if !containsTarget(targets, want) {
			t.Fatalf("cleanup targets %v missing %s", targets, want)
		}
	}
	if len(targets) != 4 {
		t.Fatalf("cleanup targets = %v", targets)
	}
	op := provisioningOp(plan, "cleanup-op")
	if _, created, err := s.CreateOperation(ctx, op, plan, "cleanup-key"); err != nil || !created {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("cleanup operation failed integrity: %v", err)
	}
	// A create of either reviewed PodDefault name is serialized by the durable
	// secondary lock as well as the namespace fence.
	create := model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "new", PVName: "new-pv", TargetUID: "new-pv-uid", TargetResourceVersion: "1", PodDefault: podDefaultRequest("mount-a")}
	createPlan := provisioningPlan("cleanup-conflict", create)
	if _, _, err := s.CreateOperation(ctx, provisioningOp(createPlan, "cleanup-conflict-op"), createPlan, "cleanup-conflict-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("PodDefault create raced cleanup: %v", err)
	}
}

func TestPVCDeletePodDefaultCleanupEvidenceValidation(t *testing.T) {
	action := pvcDeleteProvisioningAction("alice", "data")
	action.ConfirmPodDefaultCleanup = true
	cases := map[string]json.RawMessage{
		"missing evidence":         nil,
		"not json":                 json.RawMessage("not-json"),
		"not an object":            json.RawMessage(`[]`),
		"unrelated object":         json.RawMessage(`{"kind":"PersistentVolumeClaim","name":"data"}`),
		"null list":                json.RawMessage(`{"podDefaults":null}`),
		"not a list":               json.RawMessage(`{"podDefaults":{"name":"mount-a","uid":"uid-a","resourceVersion":"1"}}`),
		"bare name entry":          json.RawMessage(`{"podDefaults":["mount-a"]}`),
		"alias key":                json.RawMessage(`{"podDefaultDependencies":[{"name":"mount-a","uid":"uid-a","resourceVersion":"1"}]}`),
		"wrapper form":             json.RawMessage(`{"podDefaultCleanup":{"podDefaults":[{"name":"mount-a","uid":"uid-a","resourceVersion":"1"}]}}`),
		"case variant key":         json.RawMessage(`{"PodDefaults":[{"name":"mount-a","uid":"uid-a","resourceVersion":"1"}]}`),
		"duplicate canonical key":  json.RawMessage(`{"podDefaults":[{"name":"mount-a","uid":"uid-a","resourceVersion":"1"}],"podDefaults":[{"name":"mount-b","uid":"uid-b","resourceVersion":"1"}]}`),
		"trailing content":         json.RawMessage(`{"podDefaults":[{"name":"mount-a","uid":"uid-a","resourceVersion":"1"}]} {}`),
		"empty name":               json.RawMessage(`{"podDefaults":[{"name":"","uid":"uid-a","resourceVersion":"1"}]}`),
		"invalid name":             json.RawMessage(`{"podDefaults":[{"name":"Mount/A","uid":"uid-a","resourceVersion":"1"}]}`),
		"missing uid":              json.RawMessage(`{"podDefaults":[{"name":"mount-a","resourceVersion":"1"}]}`),
		"empty uid":                json.RawMessage(`{"podDefaults":[{"name":"mount-a","uid":"","resourceVersion":"1"}]}`),
		"non-string uid":           json.RawMessage(`{"podDefaults":[{"name":"mount-a","uid":7,"resourceVersion":"1"}]}`),
		"missing resource version": json.RawMessage(`{"podDefaults":[{"name":"mount-a","uid":"uid-a"}]}`),
		"foreign namespace":        json.RawMessage(`{"podDefaults":[{"name":"mount-a","namespace":"bob","uid":"uid-a","resourceVersion":"1"}]}`),
		"invalid namespace":        json.RawMessage(`{"podDefaults":[{"name":"mount-a","namespace":"Bad/NS","uid":"uid-a","resourceVersion":"1"}]}`),
		"duplicate name":           json.RawMessage(`{"podDefaults":[{"name":"mount-a","uid":"uid-a","resourceVersion":"1"},{"name":"mount-a","uid":"uid-b","resourceVersion":"2"}]}`),
		"duplicate entry field":    json.RawMessage(`{"podDefaults":[{"name":"mount-a","name":"mount-b","uid":"uid-a","resourceVersion":"1"}]}`),
		"malformed entry":          json.RawMessage(`{"podDefaults":[42]}`),
		"oversized cleanup set":    oversizedCleanupEvidence(t),
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			s := database(t)
			plan := provisioningPlan("cleanup-invalid-"+name, action)
			plan.Prepared.Data = data
			if _, _, err := s.CreateOperation(context.Background(), provisioningOp(plan, "cleanup-invalid-op-"+name), plan, "cleanup-invalid-key-"+name); !errors.Is(err, ErrInvalidTarget) {
				t.Fatalf("invalid cleanup evidence accepted: %v", err)
			}
		})
	}
	// An explicitly empty reviewed set is valid and adds only the PVC lock plus
	// the namespace fence.
	s := database(t)
	plan := provisioningPlan("cleanup-empty", action)
	plan.Prepared.Data = json.RawMessage(`{"podDefaults":[]}`)
	if _, created, err := s.CreateOperation(context.Background(), provisioningOp(plan, "cleanup-empty-op"), plan, "cleanup-empty-key"); err != nil || !created {
		t.Fatalf("explicit empty cleanup set refused: %v", err)
	}
	if locks := acceptedLocks(t, s, "cleanup-empty-op"); len(locks) != 2 || !containsTarget(locks, "pvc:alice/data") || !containsTarget(locks, "namespace:alice") {
		t.Fatalf("empty cleanup locks = %v", locks)
	}
	// Server-owned extra safe fields on an entry and at the top level are
	// allowed; they never change the canonical lock derivation.
	extras := database(t)
	extrasPlan := provisioningPlan("cleanup-extras", action)
	extrasPlan.Prepared.Data = json.RawMessage(`{"version":1,"podDefaultAPI":"present","podDefaults":[{"name":"mount-a","namespace":"alice","uid":"uid-a","resourceVersion":"1","shared":true,"otherPVCs":["alice/other"],"additionalSettings":true,"finalizers":2}]}`)
	if _, created, err := extras.CreateOperation(context.Background(), provisioningOp(extrasPlan, "cleanup-extras-op"), extrasPlan, "cleanup-extras-key"); err != nil || !created {
		t.Fatalf("canonical evidence with safe extra fields refused: %v", err)
	}
	if locks := acceptedLocks(t, extras, "cleanup-extras-op"); !containsTarget(locks, "poddefault:alice/mount-a") || !containsTarget(locks, "namespace:alice") {
		t.Fatalf("canonical extras derived wrong locks: %v", locks)
	}
	// A non-empty reviewed set can never be submitted without the explicit
	// cleanup acknowledgement.
	noAck := action
	noAck.ConfirmPodDefaultCleanup = false
	noAckPlan := provisioningPlan("cleanup-without-ack", noAck)
	noAckPlan.Prepared.Data = cleanupEvidenceNames(t, "alice", "mount-a")
	if _, _, err := s.CreateOperation(context.Background(), provisioningOp(noAckPlan, "cleanup-without-ack-op"), noAckPlan, "cleanup-without-ack-key"); !errors.Is(err, ErrInvalidTarget) {
		t.Fatalf("non-empty cleanup set accepted without acknowledgement: %v", err)
	}
	// Known cleanup-shaped noncanonical keys are refused even with a false
	// acknowledgement. They are never downgraded to "true legacy no evidence".
	for name, evidence := range map[string]string{
		"alias dependencies false ack": `{"podDefaultDependencies":[]}`,
		"alias names false ack":        `{"podDefaultNames":[]}`,
		"alias targets false ack":      `{"podDefaultCleanupTargets":[]}`,
		"wrapper false ack":            `{"podDefaultCleanup":[]}`,
		"case variant false ack":       `{"PodDefaults":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			invalid := database(t)
			invalidAction := pvcDeleteProvisioningAction("alice", "data")
			invalidPlan := provisioningPlan("cleanup-false-ack-"+strings.ReplaceAll(name, " ", "-"), invalidAction)
			invalidPlan.Prepared.Data = json.RawMessage(evidence)
			if _, _, err := invalid.CreateOperation(context.Background(), provisioningOp(invalidPlan, "cleanup-false-ack-op"), invalidPlan, "cleanup-false-ack-key"); !errors.Is(err, ErrInvalidTarget) {
				t.Fatalf("noncanonical cleanup key accepted with false ack: %v", err)
			}
		})
	}
	// The canonical empty set emitted by the current backend is still a true
	// no-cleanup plan when the acknowledgement is false.
	canonicalEmpty := database(t)
	canonicalAction := pvcDeleteProvisioningAction("alice", "data")
	canonicalPlan := provisioningPlan("cleanup-canonical-empty-no-ack", canonicalAction)
	canonicalPlan.Prepared.Data = json.RawMessage(`{"podDefaults":[]}`)
	if _, created, err := canonicalEmpty.CreateOperation(context.Background(), provisioningOp(canonicalPlan, "cleanup-canonical-empty-op"), canonicalPlan, "cleanup-canonical-empty-key"); err != nil || !created {
		t.Fatalf("canonical empty no-ack plan refused: %v", err)
	}
	// A legacy cleanup plan without the new fields remains compatible, including
	// its old flat delete evidence that has no PodDefault list.
	legacyAction := pvcDeleteProvisioningAction("bob", "legacy")
	legacyPlan := provisioningPlan("cleanup-legacy", legacyAction)
	legacyPlan.Prepared.Data = json.RawMessage(`{"version":1,"kind":"PersistentVolumeClaim","namespace":"bob","name":"legacy","uid":"uid-legacy","resourceVersion":"7"}`)
	if _, created, err := s.CreateOperation(context.Background(), provisioningOp(legacyPlan, "cleanup-legacy-op"), legacyPlan, "cleanup-legacy-key"); err != nil || !created {
		t.Fatalf("legacy PVC deletion refused: %v", err)
	}
}

func oversizedCleanupEvidence(t *testing.T) json.RawMessage {
	t.Helper()
	entries := make([]map[string]string, MaxPodDefaultCleanup+1)
	for i := range entries {
		entries[i] = map[string]string{"name": fmt.Sprintf("mount-%03d", i), "uid": fmt.Sprintf("uid-%03d", i), "resourceVersion": "1"}
	}
	data, err := json.Marshal(map[string]any{"podDefaults": entries})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPVCDeleteCleanupPartialOutcomeKeepsAllLocks(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	a := pvcDeleteProvisioningAction("alice", "data")
	a.ConfirmPodDefaultCleanup = true
	plan := provisioningPlan("cleanup-review-plan", a)
	plan.Prepared.Data = cleanupEvidenceNames(t, "alice", "mount-a", "mount-b")
	if err := s.Put(ctx, "plans", plan.ID, plan); err != nil {
		t.Fatal(err)
	}
	op := provisioningOp(plan, "cleanup-review-op")
	if _, created, err := s.CreateOperation(ctx, op, plan, "cleanup-review-key"); err != nil || !created {
		t.Fatal(err)
	}
	op.State = "applying"
	op.Step = "delete-poddefaults"
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "cleanup-review-checkpoint"}); err != nil {
		t.Fatal(err)
	}
	op.State = "needs-review"
	op.UpdatedAt = time.Now().UTC()
	if err := s.Finish(ctx, op, model.Audit{ID: "cleanup-review-finish"}); err != nil {
		t.Fatal(err)
	}
	locks := acceptedLocks(t, s, op.ID)
	if len(locks) != 4 {
		t.Fatalf("needs-review cleanup released locks: %v", locks)
	}
	for _, want := range []string{"poddefault:alice/mount-a", "poddefault:alice/mount-b", "namespace:alice"} {
		if !containsTarget(locks, want) {
			t.Fatalf("needs-review cleanup missing %s: %v", want, locks)
		}
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("needs-review cleanup failed integrity: %v", err)
	}
	// No fresh mutation may enter the fenced namespace.
	create := model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "new", PVName: "new-pv", TargetUID: "new-pv-uid", TargetResourceVersion: "1", PodDefault: podDefaultRequest("mount-a")}
	createPlan := provisioningPlan("cleanup-review-conflict", create)
	if _, _, err := s.CreateOperation(ctx, provisioningOp(createPlan, "cleanup-review-conflict-op"), createPlan, "cleanup-review-conflict-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("needs-review cleanup released the namespace: %v", err)
	}
	// The accepted idempotent retry still resolves without releasing anything.
	// The manager always builds a fresh queued operation for a retry.
	retry := provisioningOp(plan, "cleanup-review-op")
	prior, created, err := s.CreateOperation(ctx, retry, plan, "cleanup-review-key")
	if err != nil || created || prior.ID != op.ID || prior.State != "needs-review" {
		t.Fatalf("idempotent retry did not resolve: %v %v %v", prior, created, err)
	}
}

func TestPVCCreateBoundPersistedDependenciesSurviveReopen(t *testing.T) {
	path := t.TempDir() + "/provisioning.db"
	ctx := context.Background()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a := model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "data", PVName: "static-pv", TargetUID: "pv-uid", TargetResourceVersion: "3", PodDefault: podDefaultRequest("mount-data")}
	plan := provisioningPlan("persisted-bound-plan", a)
	op := provisioningOp(plan, "persisted-bound-op")
	if err := s.Put(ctx, "plans", plan.ID, plan); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.CreateOperation(ctx, op, plan, "persisted-bound-key"); err != nil || !created {
		t.Fatal(err)
	}
	op.State = "applying"
	op.Step = "create-pvc"
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "persisted-bound-checkpoint"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("reopened dependency locks failed integrity: %v", err)
	}
	if got, err := s.OperationForKey(ctx, plan.Actor, "persisted-bound-key", plan.ID); err != nil || got.ID != op.ID || got.State != "applying" {
		t.Fatalf("reopened operation = %+v %v", got, err)
	}
	locks := acceptedLocks(t, s, op.ID)
	for _, want := range []string{"pvc:alice/data", "pv:static-pv", "poddefault:alice/mount-data"} {
		if !containsTarget(locks, want) {
			t.Fatalf("reopened locks %v missing %s", locks, want)
		}
	}
	// A replacement claim with the same PodDefault name is still refused.
	second := provisioningPlan("persisted-bound-plan-2", model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "other", PVName: "other-pv", TargetUID: "other-pv-uid", TargetResourceVersion: "1", PodDefault: podDefaultRequest("mount-data")})
	if _, _, err := s.CreateOperation(ctx, provisioningOp(second, "persisted-bound-op-2"), second, "persisted-bound-key-2"); !errors.Is(err, ErrConflict) {
		t.Fatalf("persisted PodDefault lock was lost: %v", err)
	}
}

// Old persisted plans and actions predate the new fields; a zero value must
// keep the legacy combined static behavior and its exact lock shape.
func TestLegacyStorageActionCompatibility(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	a := model.Action{Kind: "storage.create.static", Namespace: "alice", Name: "claim", PVName: "legacy-pv", Template: "nfs-a", StorageClass: "approved", Size: "10Gi", AccessMode: "ReadWriteMany"}
	plan := provisioningPlan("legacy-static-plan", a)
	targets := Targets(plan)
	if len(targets) != 2 || !containsTarget(targets, "pvc:alice/claim") || !containsTarget(targets, "pv:legacy-pv") {
		t.Fatalf("legacy targets changed: %v", targets)
	}
	if _, created, err := s.CreateOperation(ctx, provisioningOp(plan, "legacy-static-op"), plan, "legacy-static-key"); err != nil || !created {
		t.Fatalf("legacy combined static action refused: %v", err)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("legacy static operation failed integrity: %v", err)
	}
}

// New namespace-scoped provisioning participates in the existing Profile/PVC
// deletion fences in both directions. The standalone PV action is cluster
// scoped and must remain allowed while a namespace deletion is unresolved.
func TestProvisioningRespectsProfileDeletionFences(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	deletion := profileDeletePlan("alice")
	if _, created, err := s.CreateOperation(ctx, profileDeleteOp(deletion, "profile-fence-op"), deletion, "profile-fence-key"); err != nil || !created {
		t.Fatal(err)
	}
	bound := model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "data", PVName: "static-pv", TargetUID: "pv-uid", TargetResourceVersion: "3", PodDefault: podDefaultRequest("mount-data")}
	boundPlan := provisioningPlan("profile-fence-bound", bound)
	if _, _, err := s.CreateOperation(ctx, provisioningOp(boundPlan, "profile-fence-bound-op"), boundPlan, "profile-fence-bound-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("bound PVC creation entered a Profile-fenced namespace: %v", err)
	}
	// A standalone static PV has no namespace target and is not fenced.
	static := model.Action{Kind: "pv.create.static", Name: "cluster-pv", Template: "nfs-a", StorageClass: "approved", NFSPath: "/exports/alice/data"}
	staticPlan := provisioningPlan("profile-fence-static", static)
	if _, created, err := s.CreateOperation(ctx, provisioningOp(staticPlan, "profile-fence-static-op"), staticPlan, "profile-fence-static-key"); err != nil || !created {
		t.Fatalf("cluster-scoped PV creation was fenced: %v", err)
	}
}

func TestProvisioningCleanupRefusesWhileBoundCreationIsActive(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	bound := model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "data", PVName: "static-pv", TargetUID: "pv-uid", TargetResourceVersion: "3", PodDefault: podDefaultRequest("mount-data")}
	boundPlan := provisioningPlan("reverse-fence-bound", bound)
	if _, created, err := s.CreateOperation(ctx, provisioningOp(boundPlan, "reverse-fence-bound-op"), boundPlan, "reverse-fence-bound-key"); err != nil || !created {
		t.Fatal(err)
	}
	cleanup := pvcDeleteProvisioningAction("alice", "other")
	cleanup.ConfirmPodDefaultCleanup = true
	cleanupPlan := provisioningPlan("reverse-fence-cleanup", cleanup)
	cleanupPlan.Prepared.Data = cleanupEvidenceNames(t, "alice", "mount-data")
	if _, _, err := s.CreateOperation(ctx, provisioningOp(cleanupPlan, "reverse-fence-cleanup-op"), cleanupPlan, "reverse-fence-cleanup-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("PVC cleanup started while a PodDefault creation was active: %v", err)
	}
	// A Profile deletion is refused in the same direction.
	deletion := profileDeletePlan("alice")
	if _, _, err := s.CreateOperation(ctx, profileDeleteOp(deletion, "reverse-fence-profile-op"), deletion, "reverse-fence-profile-key"); !errors.Is(err, ErrConflict) {
		t.Fatalf("Profile deletion started while provisioning was active: %v", err)
	}
}

func TestPodDefaultSecondaryLockIntegrity(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	a := model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "data", PVName: "static-pv", TargetUID: "pv-uid", TargetResourceVersion: "3", PodDefault: podDefaultRequest("mount-data")}
	plan := provisioningPlan("integrity-bound-plan", a)
	op := provisioningOp(plan, "integrity-bound-op")
	if err := s.Put(ctx, "plans", plan.ID, plan); err != nil {
		t.Fatal(err)
	}
	if _, created, err := s.CreateOperation(ctx, op, plan, "integrity-bound-key"); err != nil || !created {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("valid PodDefault lock failed integrity: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE operation_targets SET active=0 WHERE operation_id=? AND target='poddefault:alice/mount-data'`, op.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); err == nil {
		t.Fatal("missing PodDefault dependency lock passed integrity")
	}
}

func cleanupSnapshot(t *testing.T, changes []model.Change) podDefaultIdentitySnapshot {
	t.Helper()
	var snapshot podDefaultIdentitySnapshot
	found := false
	for _, change := range changes {
		if change.Field != reviewedPodDefaultIdentitiesField {
			continue
		}
		if found {
			t.Fatalf("acceptance evidence repeats the reviewed identity snapshot")
		}
		found = true
		if err := json.Unmarshal([]byte(change.After), &snapshot); err != nil {
			t.Fatalf("snapshot is not readable JSON: %v", err)
		}
	}
	if !found {
		t.Fatal("acceptance evidence has no reviewed identity snapshot")
	}
	return snapshot
}

func assertCleanupSnapshot(t *testing.T, snapshot podDefaultIdentitySnapshot) {
	t.Helper()
	var safe strings.Builder
	for _, identity := range snapshot.PodDefaults {
		safe.WriteString(identity.Name + "|" + identity.Namespace + "|" + identity.UID + "|" + identity.ResourceVersion + "\n")
	}
	if snapshot.Version != 1 || len(snapshot.PodDefaults) != 2 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	want := "mount-a|alice|uid-mount-a|1\nmount-b|alice|uid-mount-b|1\n"
	if safe.String() != want {
		t.Fatalf("snapshot identities = %q, want %q", safe.String(), want)
	}
	for _, forbidden := range []string{"/exports", "/home/jovyan", "secret", "spec", "env"} {
		if strings.Contains(safe.String(), forbidden) {
			t.Fatalf("snapshot leaked %q: %s", forbidden, safe.String())
		}
	}
}

// The exact reviewed PodDefault identities are accepted as immutable evidence
// and survive transient plan cleanup, a needs-review outcome and a restart.
func TestCleanupIdentitySnapshotSurvivesPlanCleanupAndReopen(t *testing.T) {
	path := t.TempDir() + "/cleanup-snapshot.db"
	ctx := context.Background()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	a := pvcDeleteProvisioningAction("alice", "data")
	a.ConfirmPodDefaultCleanup = true
	plan := provisioningPlan("snapshot-plan", a)
	plan.ExpiresAt = time.Now().Add(time.Hour)
	plan.Prepared.Data = cleanupEvidenceNames(t, "alice", "mount-b", "mount-a")
	if err := s.Put(ctx, "plans", plan.ID, plan); err != nil {
		t.Fatal(err)
	}
	op := provisioningOp(plan, "snapshot-op")
	if _, created, err := s.CreateOperation(ctx, op, plan, "snapshot-key"); err != nil || !created {
		t.Fatal(err)
	}
	assertCleanupSnapshot(t, cleanupSnapshot(t, acceptanceChanges(t, s, op.ID)))
	op.State = "applying"
	op.Step = "delete-poddefaults"
	op.UpdatedAt = time.Now().UTC()
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "snapshot-checkpoint"}); err != nil {
		t.Fatal(err)
	}
	op.State = "needs-review"
	op.UpdatedAt = time.Now().UTC()
	if err := s.Finish(ctx, op, model.Audit{ID: "snapshot-finish"}); err != nil {
		t.Fatal(err)
	}
	// Expire and remove the transient plan exactly as CleanupPlans does.
	plan.ExpiresAt = time.Now().Add(-time.Hour)
	if err := s.Put(ctx, "plans", plan.ID, plan); err != nil {
		t.Fatal(err)
	}
	if err := s.CleanupPlans(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	var gone model.Plan
	if err := s.Get(ctx, "plans", plan.ID, &gone); !errors.Is(err, ErrNotFound) {
		t.Fatalf("transient plan was not cleaned: %v", err)
	}
	audits, err := s.RecentAudit(ctx, 20)
	if err != nil {
		t.Fatal(err)
	}
	auditSnapshot := false
	for _, audit := range audits {
		if audit.OperationID != op.ID {
			continue
		}
		for _, change := range audit.Changes {
			if change.Field == reviewedPodDefaultIdentitiesField {
				auditSnapshot = true
				var snapshot podDefaultIdentitySnapshot
				if err := json.Unmarshal([]byte(change.After), &snapshot); err != nil {
					t.Fatal(err)
				}
				assertCleanupSnapshot(t, snapshot)
			}
		}
	}
	if !auditSnapshot {
		t.Fatal("needs-review audit lacks the reviewed identity snapshot")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("reopened needs-review cleanup failed integrity: %v", err)
	}
	got, err := s.OperationForKey(ctx, plan.Actor, "snapshot-key", plan.ID)
	if err != nil || got.ID != op.ID || got.State != "needs-review" {
		t.Fatalf("reopened operation = %+v %v", got, err)
	}
	assertCleanupSnapshot(t, cleanupSnapshot(t, acceptanceChanges(t, s, op.ID)))
	locks := acceptedLocks(t, s, op.ID)
	if len(locks) != 4 {
		t.Fatalf("reopened snapshot operation locks = %v", locks)
	}
}

func TestExtraDependencyLockFailsIntegrity(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	a := model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "data", PVName: "static-pv", TargetUID: "pv-uid", TargetResourceVersion: "3", PodDefault: podDefaultRequest("mount-data")}
	plan := provisioningPlan("extra-lock-plan", a)
	if err := s.Put(ctx, "plans", plan.ID, plan); err != nil {
		t.Fatal(err)
	}
	op := provisioningOp(plan, "extra-lock-op")
	if _, created, err := s.CreateOperation(ctx, op, plan, "extra-lock-key"); err != nil || !created {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("valid dependency set failed integrity: %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO operation_targets(operation_id,target,active) VALUES(?,?,1)`, op.ID, "pv:stale-volume"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("extra PV dependency lock passed integrity: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM operation_targets WHERE operation_id=? AND target='pv:stale-volume'`, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO operation_targets(operation_id,target,active) VALUES(?,?,1)`, op.ID, "poddefault:alice/stale"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("extra PodDefault dependency lock passed integrity: %v", err)
	}
}

func TestMalformedPresentPlanFailsIntegrity(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	a := model.Action{Kind: "pvc.create.bound", Namespace: "alice", Name: "data", PVName: "static-pv", TargetUID: "pv-uid", TargetResourceVersion: "3", PodDefault: podDefaultRequest("mount-data")}
	plan := provisioningPlan("malformed-plan", a)
	if err := s.Put(ctx, "plans", plan.ID, plan); err != nil {
		t.Fatal(err)
	}
	op := provisioningOp(plan, "malformed-plan-op")
	if _, created, err := s.CreateOperation(ctx, op, plan, "malformed-plan-key"); err != nil || !created {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE records SET value='not-json' WHERE bucket='plans' AND key=?`, plan.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("malformed present plan passed integrity: %v", err)
	}
}

func TestLegacyExtraDependencyLockRemainsCompatible(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	a := model.Action{Kind: "storage.create.static", Namespace: "alice", Name: "claim", PVName: "legacy-pv", Template: "nfs-a", StorageClass: "approved"}
	plan := provisioningPlan("legacy-extra-lock-plan", a)
	if err := s.Put(ctx, "plans", plan.ID, plan); err != nil {
		t.Fatal(err)
	}
	op := provisioningOp(plan, "legacy-extra-lock-op")
	if _, created, err := s.CreateOperation(ctx, op, plan, "legacy-extra-lock-key"); err != nil || !created {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO operation_targets(operation_id,target,active) VALUES(?,?,1)`, op.ID, "pv:legacy-extra"); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatalf("legacy extra dependency lock was newly rejected: %v", err)
	}
}
