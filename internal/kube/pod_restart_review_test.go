package kube

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"kubeflow-admin/internal/model"
)

func TestRestartRechecksPhaseAfterDurableCheckpoint(t *testing.T) {
	for _, phase := range []corev1.PodPhase{corev1.PodFailed, corev1.PodSucceeded, corev1.PodPending} {
		t.Run(string(phase), func(t *testing.T) {
			f := newRestartFixture(t)
			p, err := f.service.Prepare(context.Background(), restartAction())
			if err != nil {
				t.Fatal(err)
			}
			err = f.service.Execute(context.Background(), p, func(string) error {
				f.updatePod(t, func(pod *corev1.Pod) { pod.Status.Phase = phase })
				return nil
			})
			if !errors.Is(err, model.ErrRejectedBeforeMutation) || !errors.Is(err, ErrConflict) || len(f.evictions) != 0 {
				t.Fatalf("terminal/nonrunning Pod must not be evicted or strand locks: %v", err)
			}
		})
	}
}

func TestRestartConvergenceRejectsChangedParentOwnership(t *testing.T) {
	f := newRestartFixture(t)
	p, err := f.service.Prepare(context.Background(), restartAction())
	if err != nil {
		t.Fatal(err)
	}
	f.afterEvict = func() {
		// A reactor already holds Fake's invocation mutex. Mutate the tracker
		// directly rather than recursively entering the client from its callback.
		set := f.set.DeepCopy()
		set.OwnerReferences = nil
		if err := f.client.Tracker().Update(schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}, set, "alice"); err != nil {
			t.Fatal(err)
		}
	}
	err = f.service.Execute(context.Background(), p, func(string) error { return nil })
	if !errors.Is(err, ErrConflict) || errors.Is(err, model.ErrRejectedBeforeMutation) || len(f.evictions) != 1 {
		t.Fatalf("ownership changed after eviction must require review, not claim success: %v", err)
	}
}

func TestRestartRejectsStaticSourceWithoutMirrorAnnotation(t *testing.T) {
	f := newRestartFixture(t)
	f.updatePod(t, func(pod *corev1.Pod) { pod.Annotations = map[string]string{"kubernetes.io/config.source": "file"} })
	if _, err := f.service.Prepare(context.Background(), restartAction()); err == nil {
		t.Fatal("static Pod without mirror annotation was accepted")
	}
}
