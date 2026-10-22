package store

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"kubeflow-admin/internal/model"
)

// The durable acceptance ledger and every later audit checkpoint must carry
// only digests for entered paths/handles and mount paths, while retaining the
// safe reviewed dependency identities the operator needs for reconciliation.
func TestAcceptanceRedactsPathsHandlesAndMountPaths(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	rawNFS := "/exports/private-team/alice/secret"
	rawBeeGFS := "beegfs://private.example.invalid/home/alice/secret"
	rawMount := "/home/jovyan/alice-private"
	rawHandle := "private-volume-handle-value"
	plan := provisioningPlan("redact-plan", model.Action{Kind: "pv.create.static", Name: "secret-pv", Template: "nfs-a", StorageClass: "approved"})
	plan.Prepared.Changes = []model.Change{
		{Field: "PersistentVolume", Before: "(new)", After: "secret-pv"},
		{Field: "NFS path", Before: "(entered)", After: rawNFS},
		{Field: "BeeGFS path", Before: "(prefix+entered suffix)", After: rawBeeGFS},
		{Field: "PodDefault mount path", Before: "(reviewed input)", After: rawMount},
		{Field: "volume handle", Before: "(old)", After: rawHandle},
		{Field: "NFS directory UID", Before: "(required)", After: "1000"},
		{Field: "related PodDefault", Before: "alice/mount-a (uid-a)", After: "delete with exact UID/resourceVersion before the PVC"},
	}
	if err := s.Put(ctx, "plans", plan.ID, plan); err != nil {
		t.Fatal(err)
	}
	op := provisioningOp(plan, "redact-op")
	if _, created, err := s.CreateOperation(ctx, op, plan, "redact-key"); err != nil || !created {
		t.Fatal(err)
	}
	// The transient preview plan still carries the reviewed values.
	var preview model.Plan
	if err := s.Get(ctx, "plans", plan.ID, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Prepared.Changes[1].After != rawNFS {
		t.Fatal("transient preview was redacted")
	}
	changes := acceptanceChanges(t, s, op.ID)
	assertRedactedChanges(t, changes, rawNFS, rawBeeGFS, rawMount, rawHandle)
	if changes[0].After != "secret-pv" || changes[5].After != "1000" || changes[6].Before != "alice/mount-a (uid-a)" {
		t.Fatalf("safe reviewed facts were redacted: %+v", changes)
	}
	// The accepted audit row uses the same redacted evidence.
	assertAuditRedacted(t, s, rawNFS, rawBeeGFS, rawMount, rawHandle)
	// A later checkpoint also reads the immutable redacted ledger rather than
	// reintroducing the plan's raw values.
	op.State = "applying"
	op.Step = "create-static-pv"
	op.UpdatedAt = time.Now().UTC()
	if err := s.Checkpoint(ctx, op, model.Audit{ID: "redact-checkpoint"}); err != nil {
		t.Fatal(err)
	}
	assertAuditRedacted(t, s, rawNFS, rawBeeGFS, rawMount, rawHandle)
	// The redaction is deterministic and idempotent across checkpoints.
	again := acceptanceChanges(t, s, op.ID)
	for i := range changes {
		if changes[i] != again[i] {
			t.Fatalf("acceptance evidence changed after checkpoint: %+v vs %+v", changes[i], again[i])
		}
	}
}

// Cleanup audit keeps the safe reviewed dependency identities (namespace,
// name and UID) and other affected PVC names, while no PodDefault setting or
// path is copied into the durable record.
func TestCleanupAcceptanceRetainsReviewedDependencyIdentities(t *testing.T) {
	s := database(t)
	ctx := context.Background()
	a := pvcDeleteProvisioningAction("alice", "data")
	a.ConfirmPodDefaultCleanup = true
	plan := provisioningPlan("cleanup-redact-plan", a)
	plan.Prepared.Data = cleanupEvidenceNames(t, "alice", "mount-toast")
	plan.Prepared.Changes = []model.Change{
		{Field: "PersistentVolumeClaim", Before: "alice/data", After: "deleted"},
		{Field: "target UID", Before: "pvc-uid", After: "exact precondition"},
		{Field: "related PodDefault", Before: "alice/mount-toast (uid-mount-toast)", After: "delete with exact UID/resourceVersion before the PVC"},
		{Field: "shared PodDefault mount-toast", Before: "alice/other-claim", After: "settings removed from future matching Pods; other PVCs untouched"},
	}
	op := provisioningOp(plan, "cleanup-redact-op")
	if _, created, err := s.CreateOperation(ctx, op, plan, "cleanup-redact-key"); err != nil || !created {
		t.Fatal(err)
	}
	changes := acceptanceChanges(t, s, op.ID)
	encoded, err := json.Marshal(changes)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"alice/mount-toast (uid-mount-toast)", "alice/other-claim", "PersistentVolumeClaim"} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("safe cleanup identity %q missing: %s", want, encoded)
		}
	}
	if strings.Contains(string(encoded), "env") || strings.Contains(string(encoded), "secret") {
		t.Fatalf("cleanup change dumped PodDefault settings: %s", encoded)
	}
}

func acceptanceChanges(t *testing.T, s *Store, operationID string) []model.Change {
	t.Helper()
	var raw []byte
	if err := s.db.QueryRow(`SELECT changes FROM operation_acceptance WHERE operation_id=?`, operationID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var changes []model.Change
	if err := json.Unmarshal(raw, &changes); err != nil {
		t.Fatal(err)
	}
	return changes
}

func assertRedactedChanges(t *testing.T, changes []model.Change, secrets ...string) {
	t.Helper()
	encoded, err := json.Marshal(changes)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("durable changes leaked %q: %s", secret, encoded)
		}
	}
	sensitive := map[string]bool{"NFS path": true, "BeeGFS path": true, "PodDefault mount path": true, "volume handle": true}
	for _, change := range changes {
		if sensitive[change.Field] {
			if !strings.HasPrefix(change.After, "sha256:") {
				t.Fatalf("%s was not digests: %+v", change.Field, change)
			}
		}
	}
	if !strings.Contains(string(encoded), `"before":"(entered)"`) {
		t.Fatalf("safe placeholder was replaced: %s", encoded)
	}
}

func assertAuditRedacted(t *testing.T, s *Store, secrets ...string) {
	t.Helper()
	audits, err := s.RecentAudit(context.Background(), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) == 0 {
		t.Fatal("no audit history")
	}
	for _, audit := range audits {
		encoded, err := json.Marshal(audit)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range secrets {
			if strings.Contains(string(encoded), secret) {
				t.Fatalf("audit history leaked %q: %s", secret, encoded)
			}
		}
	}
}
