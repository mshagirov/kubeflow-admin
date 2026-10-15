package auth

import (
	"context"
	"errors"
	"testing"
	"time"

	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/store"
)

func TestWorkerSessionValidationUsesDurableCurrentIdentity(t *testing.T) {
	f := setup(t)
	c := findCookie(t, f.callback(f.start(t)), sessionCookie)
	w, actor := f.request("GET", "/", c, "", "", "")
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	ctx := context.Background()
	if err := ValidateSession(ctx, f.a.cfg, f.s, actor); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*model.Identity){
		func(i *model.Identity) { i.SessionID = "" },
		func(i *model.Identity) { i.Issuer = "https://foreign.example" },
		func(i *model.Identity) { i.Subject = "different" },
		func(i *model.Identity) { i.ExpiresAt = i.ExpiresAt.Add(time.Hour) },
		func(i *model.Identity) { i.AuthenticatedAt = i.AuthenticatedAt.Add(-time.Hour) },
	} {
		copy := actor
		mutate(&copy)
		if err := ValidateSession(ctx, f.a.cfg, f.s, copy); !errors.Is(err, ErrSessionInvalid) {
			t.Fatal("invalid worker identity accepted", err)
		}
	}
	for _, mutate := range []func(*model.Identity){
		func(i *model.Identity) { i.Groups = []string{"not-admins"} },
		func(i *model.Identity) { i.Issuer = "https://foreign.example" },
		func(i *model.Identity) { i.Subject = "different" },
		func(i *model.Identity) { i.ExpiresAt = time.Now().Add(-time.Second) },
		func(i *model.Identity) { i.AuthenticatedAt = time.Now().Add(time.Minute) },
	} {
		copy := actor
		mutate(&copy)
		if err := f.s.Put(ctx, sessionBucket, actor.SessionID, session{Identity: copy}); err != nil {
			t.Fatal(err)
		}
		if err := ValidateSession(ctx, f.a.cfg, f.s, actor); !errors.Is(err, ErrSessionInvalid) {
			t.Fatal("revoked/changed session accepted", err)
		}
	}
	if err := f.s.Delete(ctx, sessionBucket, actor.SessionID); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSession(ctx, f.a.cfg, f.s, actor); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("logout not noticed", err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSession(ctx, f.a.cfg, f.s, actor); err == nil || err.Error() != "session verification unavailable" {
		t.Fatal("unsafe session store failure", err)
	}
}

func TestTrafficCleanupDeletesExpiredAuthenticationRecordsOnly(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Hour)
	if err := f.s.Put(ctx, attemptBucket, "expired", attempt{ExpiresAt: past}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Put(ctx, sessionBucket, "expired", session{Identity: model.Identity{AuthenticatedAt: past, ExpiresAt: past}}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Put(ctx, "audit", "preserved", model.Audit{ID: "preserved", Time: past}); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	for _, bucket := range []string{attemptBucket, sessionBucket} {
		var data any
		if err := f.s.Get(ctx, bucket, "expired", &data); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("expired authentication record retained", bucket, err)
		}
	}
	var audit model.Audit
	if err := f.s.Get(ctx, "audit", "preserved", &audit); err != nil {
		t.Fatal("audit was cleaned", err)
	}
}
