package operations

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"kubeflow-admin/internal/model"
)

func TestStorageDeleteReviewGuidanceIsTypedAndNonReplaying(t *testing.T) {
	for kind, want := range map[string]string{"pv.delete": storageDeleteReviewGuidance, "pvc.delete": pvcDeleteReviewGuidance} {
		got := reviewGuidance(kind)
		if got != want {
			t.Fatalf("%s did not get typed storage guidance: %s", kind, got)
		}
		if !strings.Contains(got, "Do not retry") || !strings.Contains(got, "finalizers") || !strings.Contains(got, "No rollback or automatic replay") {
			t.Fatalf("%s guidance is incomplete: %s", kind, got)
		}
		if strings.Contains(got, genericReviewGuidance) {
			t.Fatalf("%s retained generic notebook wording", kind)
		}
	}
	if reviewGuidance("profile.delete") != profileDeleteReviewGuidance {
		t.Fatal("Profile deletion guidance changed")
	}
	if reviewGuidance("pvc.expand") != genericReviewGuidance {
		t.Fatal("generic guidance changed")
	}
}

func TestStorageDeleteRecoveryNeverResumes(t *testing.T) {
	for _, kind := range []string{"pv.delete", "pvc.delete"} {
		got := recoveryGuidance(kind)
		if !strings.Contains(got, "no action was replayed") || !strings.Contains(got, "nothing will be resumed automatically") {
			t.Fatalf("%s recovery guidance is unsafe: %s", kind, got)
		}
	}
	if recoveryGuidance("pvc.expand") != "Application restarted before a durable outcome. Inspect actual cluster state; no action was replayed." {
		t.Fatal("generic recovery guidance changed")
	}
}

func TestStorageEditUncertainAndRecoveryGuidance(t *testing.T) {
	for _, kind := range []string{"pv.edit", "pvc.edit"} {
		t.Run(kind, func(t *testing.T) {
			if reviewGuidance(kind) != storageEditReviewGuidance {
				t.Fatal("storage edit did not receive typed review guidance")
			}
			for _, guidance := range []string{reviewGuidance(kind), recoveryGuidance(kind)} {
				for _, required := range []string{"UID", "before/after", "policy may now be Delete", "backing data", "resize", "CSI", "filesystem", "Do not retry", "shrink", "finalizers", "replacement object", "remains locked", "No rollback or automatic replay"} {
					if !strings.Contains(guidance, required) {
						t.Fatalf("%s guidance omits %q: %s", kind, required, guidance)
					}
				}
			}
			if !strings.Contains(recoveryGuidance(kind), "nothing will be resumed automatically") {
				t.Fatal("restart guidance must explicitly refuse automatic resume")
			}
		})
	}
	if reviewGuidance("pvc.expand") != genericReviewGuidance || reviewGuidance("profile.delete") != profileDeleteReviewGuidance {
		t.Fatal("existing action guidance changed")
	}
}

// A PVC deletion that already removed the claim but did not finish the Retain
// volume reclaim keeps the proven deletion receipt, ends in needs-review and
// keeps both the namespace and the volume locked.
func TestPVCDeletePartialReclaimKeepsReceiptAndLocks(t *testing.T) {
	deleted := model.MutationReceipt{Kind: "PersistentVolumeClaim", Namespace: "alice", Name: "data", UID: "claim-uid", ResourceVersion: "1", Outcome: model.MutationOutcomeDeleted}
	b := &fakeBackend{
		prepare: func(_ context.Context, a model.Action) (model.Prepared, error) {
			return model.Prepared{Action: a, TargetUID: "claim-uid", ResourceVersion: "1", Summary: "delete claim",
				Data: json.RawMessage(`{"version":1,"reclaimVolume":"retained-pv","reclaimVolumeUID":"pv-uid","podDefaults":[]}`)}, nil
		},
		execute: func(ctx context.Context, p model.Prepared, c func(string) error) error {
			if err := c("delete-storage"); err != nil {
				return err
			}
			if err := model.RecordMutationReceipt(ctx, deleted); err != nil {
				return err
			}
			if err := c("reclaim-volume"); err != nil {
				return err
			}
			return errors.New("PersistentVolume reclaim stopped for review")
		},
	}
	m, s, actor := setup(t, b)
	ctx := context.Background()
	action := model.Action{Kind: "pvc.delete", Namespace: "alice", Name: "data", TargetUID: "claim-uid", TargetResourceVersion: "1", ConfirmName: "data", Reason: "verified unused", ConfirmPVReclaim: true}
	plan, err := m.Plan(ctx, actor, action)
	if err != nil {
		t.Fatal(err)
	}
	op, err := m.Submit(ctx, actor, plan.ID, "partial-reclaim-key")
	if err != nil {
		t.Fatal(err)
	}
	got := waitOperation(t, m, op.ID)
	if got.State != "needs-review" || len(got.MutationReceipts) != 1 || got.MutationReceipts[0] != deleted {
		t.Fatalf("partial reclaim = %+v", got)
	}
	if !strings.Contains(got.Error, "claimRef") || !strings.Contains(got.Error, "No rollback or automatic replay") {
		t.Fatalf("review guidance does not describe the unfinished reclaim: %s", got.Error)
	}
	if err := s.CheckIntegrity(ctx); err != nil {
		t.Fatal(err)
	}
	other := model.Action{Kind: "pv.delete", Name: "retained-pv", TargetUID: "pv-uid", TargetResourceVersion: "3", ConfirmName: "retained-pv", Reason: "unused"}
	otherPlan, err := m.Plan(ctx, actor, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Submit(ctx, actor, otherPlan.ID, "pv-while-partial-key"); err == nil {
		t.Fatal("the retained volume was not kept locked after the partial reclaim")
	}
}
