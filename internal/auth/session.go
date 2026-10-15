package auth

import (
	"context"
	"errors"
	"time"

	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/store"
)

// ErrSessionInvalid intentionally does not distinguish logout, revocation,
// expiration, or a changed local administrator allowlist.
var ErrSessionInvalid = errors.New("fresh administrator session required")

// ValidateSession is the worker's authorization boundary. It never trusts a
// cached group's membership: the persisted session and current local policy
// must still authorize this issuer/subject at every write checkpoint.
func ValidateSession(ctx context.Context, cfg config.Config, s *store.Store, actor model.Identity) error {
	if !validOpaque(actor.SessionID) || actor.Subject == "" || actor.Issuer != cfg.Auth.IssuerURL {
		return ErrSessionInvalid
	}
	var current session
	if err := s.Get(ctx, sessionBucket, actor.SessionID, &current); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrSessionInvalid
		}
		return errors.New("session verification unavailable")
	}
	i := current.Identity
	now := time.Now()
	if i.Issuer != actor.Issuer || i.Subject != actor.Subject || !i.AuthenticatedAt.Equal(actor.AuthenticatedAt) || !i.ExpiresAt.Equal(actor.ExpiresAt) || i.AuthenticatedAt.IsZero() || i.AuthenticatedAt.After(now) || !now.Before(i.ExpiresAt) || !now.Before(i.AuthenticatedAt.Add(15*time.Minute)) || cfg.Auth.SessionMinutes < 1 || cfg.Auth.SessionMinutes > 15 || !now.Before(i.AuthenticatedAt.Add(time.Duration(cfg.Auth.SessionMinutes)*time.Minute)) {
		return ErrSessionInvalid
	}
	checker := Auth{cfg: cfg}
	if !checker.allowed(i) {
		return ErrSessionInvalid
	}
	return nil
}

// cleanup is driven by traffic, not a background goroutine requiring another
// shutdown API. It runs at most once per minute and never touches audit data.
func (a *Auth) cleanup(ctx context.Context) error {
	now := time.Now()
	a.mu.Lock()
	if now.Sub(a.lastCleanup) < time.Minute {
		a.mu.Unlock()
		return nil
	}
	a.lastCleanup = now
	a.mu.Unlock()
	if err := a.store.CleanupAuth(ctx, now); err != nil {
		a.mu.Lock()
		a.lastCleanup = time.Time{}
		a.mu.Unlock()
		return errors.New("authentication storage unavailable")
	}
	return nil
}
