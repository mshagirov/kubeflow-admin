package operations

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/store"
)

func TestActorDisplayCapturedAtAcceptanceAndKeptByCheckpoints(t *testing.T) {
	m, _, actor := setup(t, &fakeBackend{})
	actor.Username = "Alice Example"
	actor.Email = "alice@example.test"
	ctx := context.Background()
	p, err := m.Plan(ctx, actor, model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "display", Reason: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	if p.ActorDisplay != "Alice Example" {
		t.Fatal("plan did not capture display label", p.ActorDisplay)
	}
	op, err := m.Submit(ctx, actor, p.ID, "display-idempotency-key")
	if err != nil {
		t.Fatal(err)
	}
	if op.ActorDisplay != "Alice Example" {
		t.Fatal("acceptance did not capture current display label", op.ActorDisplay)
	}
	// A later profile change is not retroactively applied to accepted work.
	actor.Username = "Renamed Later"
	got := waitOperation(t, m, op.ID)
	if got.State != "succeeded" || got.ActorDisplay != "Alice Example" {
		t.Fatal("operation display changed", got)
	}
	audit, err := m.Audit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range audit {
		if a.OperationID != op.ID {
			continue
		}
		found = true
		if a.ActorDisplay != "Alice Example" {
			t.Fatal("audit display changed across checkpoint/finish", a)
		}
		// Authority remains the issuer|subject key, never the presentation label.
		if a.Actor != actor.Key() {
			t.Fatal("authority identifier replaced by display label", a)
		}
	}
	if !found {
		t.Fatal("no audit evidence for operation")
	}
}

func TestRejectedBeforeMutationFailsAndReleasesLocks(t *testing.T) {
	b := &fakeBackend{execute: func(ctx context.Context, p model.Prepared, c func(string) error) error {
		if err := c("before-write"); err != nil {
			return err
		}
		return fmt.Errorf("admission detail: %w", model.ErrRejectedBeforeMutation)
	}}
	m, _, actor := setup(t, b)
	op := accept(t, m, actor, "rejected")
	got := waitOperation(t, m, op.ID)
	if got.State != "failed" {
		t.Fatal("definite pre-mutation rejection is not failed", got)
	}
	if strings.Contains(got.Error, "admission detail") || strings.Contains(got.Error, "private") {
		t.Fatal("backend rejection text leaked into durable state", got.Error)
	}
	// The target lock was released: a corrected preview can be accepted.
	next, err := m.Plan(context.Background(), actor, model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "rejected"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Submit(context.Background(), actor, next.ID, "rejected-retry-key"); err != nil {
		t.Fatal("pre-mutation failure did not release the target lock", err)
	}
}

func TestAmbiguousFailureRemainsNeedsReviewAndLocked(t *testing.T) {
	b := &fakeBackend{execute: func(ctx context.Context, p model.Prepared, c func(string) error) error {
		if err := c("before-write"); err != nil {
			return err
		}
		return errors.New("transport timeout; unknown outcome")
	}}
	m, _, actor := setup(t, b)
	op := accept(t, m, actor, "ambiguous")
	got := waitOperation(t, m, op.ID)
	if got.State != "needs-review" {
		t.Fatal("ambiguous failure did not stay needs-review", got)
	}
	next, err := m.Plan(context.Background(), actor, model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "ambiguous"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Submit(context.Background(), actor, next.ID, "ambiguous-retry-key"); !errors.Is(err, store.ErrConflict) {
		t.Fatal("ambiguous failure released target locks", err)
	}
}

func TestHistoryStatsUsesDirectConfigDefaultsAndCloseWaits(t *testing.T) {
	m, _, _ := setup(t, &fakeBackend{})
	stats, err := m.HistoryStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.RetentionDays != 90 || stats.MaxAuditRecords != 50000 || stats.MaxAuditBytes != 64<<20 || stats.MaxOperationRecords != 100000 {
		t.Fatal("direct Config{} history defaults not applied", stats)
	}
	if stats.MaxUnresolved != store.MaxUnresolvedOperations || stats.RecentRecordLimit != store.RecentHistoryLimit {
		t.Fatal("exact list/admission bounds not exposed", stats)
	}
	done := make(chan struct{})
	go func() {
		m.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not wait for worker/janitor shutdown")
	}
}
