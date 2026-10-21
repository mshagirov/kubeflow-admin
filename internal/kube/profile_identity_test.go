package kube

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"kubeflow-admin/internal/model"
)

func TestProfileConfigurationDigestBindsObservedIdentity(t *testing.T) {
	s, api, client := testService(t)
	profile := api.get("profiles", "", "alice")
	reviewed := profileConfigDigest(profile)
	otherName := profile.DeepCopy()
	otherName.SetName("another-profile")
	if profileConfigDigest(otherName) == reviewed {
		t.Fatal("identical configuration on another target must not reuse the precondition")
	}
	replacement := profile.DeepCopy()
	replacement.SetUID(types.UID("replacement-profile"))
	if profileConfigDigest(replacement) == reviewed {
		t.Fatal("a replacement Profile must invalidate the old form")
	}
	// Simulate a fully reconciled replacement under the reused name. Ordinary
	// scope validation now passes, but the old form must still be refused.
	api.put("profiles", replacement)
	namespace, err := client.CoreV1().Namespaces().Get(context.Background(), "alice", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	namespace.UID = "replacement-namespace"
	for i := range namespace.OwnerReferences {
		if namespace.OwnerReferences[i].Controller != nil && *namespace.OwnerReferences[i].Controller {
			namespace.OwnerReferences[i].UID = replacement.GetUID()
		}
	}
	if _, err = client.CoreV1().Namespaces().Update(context.Background(), namespace, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Prepare(context.Background(), model.Action{
		Kind: "profile.update", Name: "alice", Owner: "alice@example.test",
		BaseConfigDigest: reviewed, Quota: map[string]string{"requests.cpu": "4"},
	})
	if err == nil {
		t.Fatal("a stale form prepared an update against a replacement Profile")
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("unexpected identity refusal: %v", err)
	}
	assertNoWrites(t, api)
}
