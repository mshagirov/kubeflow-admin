package operations

import (
	"strings"
	"testing"
)

func TestStorageDeleteReviewGuidanceIsTypedAndNonReplaying(t *testing.T) {
	for _, kind := range []string{"pv.delete", "pvc.delete"} {
		got := reviewGuidance(kind)
		if got != storageDeleteReviewGuidance {
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
