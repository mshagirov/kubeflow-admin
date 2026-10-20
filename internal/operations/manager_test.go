package operations

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/store"
	"path/filepath"
	"testing"
	"time"
)

type fakeBackend struct {
	execute func(context.Context, model.Prepared, func(string) error) error
	prepare func(context.Context, model.Action) (model.Prepared, error)
}

func (f *fakeBackend) Prepare(ctx context.Context, a model.Action) (model.Prepared, error) {
	if f.prepare != nil {
		return f.prepare(ctx, a)
	}
	return model.Prepared{Action: a, TargetUID: "uid", Summary: "safe diff"}, nil
}
func (f *fakeBackend) Execute(ctx context.Context, p model.Prepared, c func(string) error) error {
	if f.execute != nil {
		return f.execute(ctx, p, c)
	}
	return c("write-notebook")
}
func setup(t *testing.T, b *fakeBackend) (*Manager, *store.Store, model.Identity) {
	t.Helper()
	s, e := store.Open(filepath.Join(t.TempDir(), "admin.db"))
	if e != nil {
		t.Fatal(e)
	}
	cfg := config.Config{Auth: config.Auth{IssuerURL: "https://issuer", SessionMinutes: 15, AdminGroups: []string{"admins"}}, Features: config.Features{WritesEnabled: true}}
	m, e := newManager(context.Background(), cfg, s, b)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { m.Close(); s.Close() })
	now := time.Now()
	digest := sha256.Sum256([]byte("test-only-session"))
	actor := model.Identity{SessionID: base64.RawURLEncoding.EncodeToString(digest[:]), Issuer: cfg.Auth.IssuerURL, Subject: "admin", Groups: []string{"admins"}, AuthenticatedAt: now, ExpiresAt: now.Add(15 * time.Minute)}
	if err := s.Put(context.Background(), "auth.sessions", actor.SessionID, map[string]any{"identity": actor}); err != nil {
		t.Fatal(err)
	}
	return m, s, actor
}
func TestPlansBindActorAndExpiry(t *testing.T) {
	m, s, actor := setup(t, &fakeBackend{})
	ctx := context.Background()
	p, e := m.Plan(ctx, actor, model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "lab"})
	if e != nil {
		t.Fatal(e)
	}
	different := actor
	different.Subject = "someone-else"
	if _, e = m.Submit(ctx, different, p.ID, "abcdefghijklmnop"); e == nil {
		t.Fatal("other actor accepted")
	}
	p.ExpiresAt = time.Now().Add(-time.Minute)
	if e = s.Put(ctx, "plans", p.ID, p); e != nil {
		t.Fatal(e)
	}
	if _, e = m.Submit(ctx, actor, p.ID, "abcdefghijklmnop"); e == nil {
		t.Fatal("expired plan accepted")
	}
}
func TestCheckpointBeforeWriteAndIdempotency(t *testing.T) {
	b := &fakeBackend{}
	m, s, actor := setup(t, b)
	ctx := context.Background()
	writes := make(chan bool, 1)
	b.execute = func(ctx context.Context, p model.Prepared, c func(string) error) error {
		if e := c("before-write"); e != nil {
			return e
		}
		ops, e := m.List(ctx)
		if e != nil || len(ops) != 1 || ops[0].Step != "before-write" {
			return errors.New("checkpoint not durable")
		}
		writes <- true
		return nil
	}
	p, e := m.Plan(ctx, actor, model.Action{Kind: "notebook.stop", Namespace: "alice", Name: "lab"})
	if e != nil {
		t.Fatal(e)
	}
	op, e := m.Submit(ctx, actor, p.ID, "abcdefghijklmnop")
	if e != nil {
		t.Fatal(e)
	}
	select {
	case <-writes:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not write")
	}
	again, e := m.Submit(ctx, actor, p.ID, "abcdefghijklmnop")
	if e != nil || again.ID != op.ID {
		t.Fatal(again, e)
	}
	// Retrying the same immutable plan/key remains idempotent even after preview expiry.
	p.ExpiresAt = time.Now().Add(-time.Hour)
	if e = s.Put(ctx, "plans", p.ID, p); e != nil {
		t.Fatal(e)
	}
	again, e = m.Submit(ctx, actor, p.ID, "abcdefghijklmnop")
	if e != nil || again.ID != op.ID {
		t.Fatal(again, e)
	}
}
func TestStartupNeverReplaysIncompleteOperations(t *testing.T) {
	s, e := store.Open(filepath.Join(t.TempDir(), "data.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	ctx := context.Background()
	p := model.Plan{ID: "old", Actor: "issuer|admin", Prepared: model.Prepared{Action: model.Action{Kind: "notebook.stop", Namespace: "a", Name: "b"}}}
	op := model.Operation{ID: "old-op", Actor: p.Actor, PlanID: p.ID, Target: "notebook:a/b", Action: p.Prepared.Action.Kind, State: "queued", Step: "queued", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if _, _, e = s.CreateOperation(ctx, op, p, "old-idempotency-key"); e != nil {
		t.Fatal(e)
	}
	op.State = "applying"
	op.Step = "stop-notebook"
	if e = s.Checkpoint(ctx, op, model.Audit{ID: "old-checkpoint"}); e != nil {
		t.Fatal(e)
	}
	b := &fakeBackend{execute: func(context.Context, model.Prepared, func(string) error) error { t.Error("replayed"); return nil }}
	m, e := newManager(ctx, config.Config{}, s, b)
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	got, e := m.Get(ctx, op.ID)
	if e != nil || got.State != "needs-review" {
		t.Fatal(got, e)
	}
}
func TestAuthorizationDeniesStaleClaims(t *testing.T) {
	m, _, a := setup(t, &fakeBackend{})
	a.AuthenticatedAt = time.Now().Add(-16 * time.Minute)
	if _, e := m.Plan(context.Background(), a, model.Action{}); e == nil {
		t.Fatal("accepted stale claims")
	}
}
