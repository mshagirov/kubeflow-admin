// Package auth implements direct OIDC login and short-lived local sessions.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/store"
)

const (
	sessionCookie = "__Host-kfa-session"
	loginCookie   = "__Host-kfa-login"
	sessionBucket = "auth.sessions"
	attemptBucket = "auth.attempts"
	loginTTL      = 5 * time.Minute
)

type identityKey struct{}

// Identity returns the verified local identity, never an identity from headers.
func Identity(ctx context.Context) (model.Identity, bool) {
	i, ok := ctx.Value(identityKey{}).(model.Identity)
	return i, ok
}

type session struct {
	Identity model.Identity `json:"identity"`
}

type attempt struct {
	BrowserHash string    `json:"browserHash"`
	Verifier    string    `json:"verifier"`
	Nonce       string    `json:"nonce"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

type rateEntry struct {
	start time.Time
	count int
}

// Auth is safe for concurrent requests. Configuration is immutable for its
// lifetime; replacing the process/configuration never bypasses allowlist checks
// on sessions recovered from the database.
type Auth struct {
	cfg         config.Config
	store       *store.Store
	oauth       oauth2.Config
	verifier    *oidc.IDTokenVerifier
	client      *http.Client
	origin      string
	mu          sync.Mutex
	rates       map[string]rateEntry
	global      rateEntry
	lastCleanup time.Time
}

// New discovers and validates the provider before accepting any login traffic.
// The context may contain an oauth2 HTTP client (useful for private CA roots).
func New(ctx context.Context, cfg config.Config, s *store.Store) (*Auth, error) {
	if s == nil {
		return nil, errors.New("auth: session store required")
	}
	if cfg.Auth.ClientID == "" || cfg.Auth.ClientSecretFile == "" || cfg.Auth.SessionMinutes < 1 || cfg.Auth.SessionMinutes > 15 || len(cfg.Auth.AdminGroups)+len(cfg.Auth.AdminSubjects) == 0 {
		return nil, errors.New("auth: invalid authentication configuration")
	}
	public, err := secureURL(cfg.Server.PublicURL)
	if err != nil || public.RawQuery != "" || (public.Path != "" && public.Path != "/") {
		return nil, errors.New("auth: public URL must be an HTTPS origin")
	}
	if issuer, err := secureURL(cfg.Auth.IssuerURL); err != nil || issuer.RawQuery != "" {
		return nil, errors.New("auth: issuer must use HTTPS")
	}
	secret, err := os.ReadFile(cfg.Auth.ClientSecretFile)
	if err != nil {
		return nil, fmt.Errorf("auth: read client secret: %w", err)
	}
	secretText := strings.TrimSpace(string(secret))
	if secretText == "" {
		return nil, errors.New("auth: empty client secret")
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if supplied, ok := ctx.Value(oauth2.HTTPClient).(*http.Client); ok && supplied != nil {
		*client = *supplied
	}
	client.Timeout = 10 * time.Second
	// Do not follow redirects with credentials, or from discovery to untrusted hosts.
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return errors.New("auth: provider redirect disallowed") }
	providerCtx, cancel := context.WithTimeout(oidc.ClientContext(ctx, client), 15*time.Second)
	defer cancel()
	provider, err := oidc.NewProvider(providerCtx, cfg.Auth.IssuerURL)
	if err != nil {
		return nil, errors.New("auth: provider discovery failed")
	}
	ep := provider.Endpoint()
	if _, err := secureURL(ep.AuthURL); err != nil {
		return nil, errors.New("auth: insecure authorization endpoint")
	}
	if _, err := secureURL(ep.TokenURL); err != nil {
		return nil, errors.New("auth: insecure token endpoint")
	}
	var metadata struct {
		JWKS string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return nil, errors.New("auth: invalid provider metadata")
	}
	if _, err := secureURL(metadata.JWKS); err != nil {
		return nil, errors.New("auth: insecure signing key endpoint")
	}
	origin := public.Scheme + "://" + public.Host
	scopes := []string{oidc.ScopeOpenID}
	for _, scope := range cfg.Auth.Scopes {
		if scope != "openid" && scope != "" {
			scopes = append(scopes, scope)
		}
	}
	// Explicit basic authentication prevents an auth-style probe from sending a
	// second token exchange, and makes this a confidential client only.
	ep.AuthStyle = oauth2.AuthStyleInHeader
	// Use a non-request context for key refresh: a canceled startup context must
	// not prevent later JWKS rotation.
	keys := oidc.NewRemoteKeySet(oidc.ClientContext(context.Background(), client), metadata.JWKS)
	return &Auth{cfg: cfg, store: s, client: client, origin: origin,
		oauth:    oauth2.Config{ClientID: cfg.Auth.ClientID, ClientSecret: secretText, Endpoint: ep, RedirectURL: origin + "/auth/callback", Scopes: scopes},
		verifier: oidc.NewVerifier(cfg.Auth.IssuerURL, keys, &oidc.Config{ClientID: cfg.Auth.ClientID}), rates: make(map[string]rateEntry)}, nil
}

func secureURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid HTTPS URL")
	}
	return u, nil
}

func random() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func hash(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}
func csrf(raw string) string { return hash("kubeflow-admin csrf\x00" + raw) }
func equal(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
func validOpaque(s string) bool {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == 32
}
func cookie(w http.ResponseWriter, name, value string, expires time.Time) {
	age := int(time.Until(expires).Seconds())
	if value == "" {
		age = -1
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, Expires: expires, MaxAge: age})
}
func clearCookie(w http.ResponseWriter, name string) { cookie(w, name, "", time.Unix(1, 0)) }
func safeError(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, http.StatusText(status), status)
}
func method(w http.ResponseWriter, r *http.Request, want string) bool {
	if r.Method == want {
		return true
	}
	w.Header().Set("Allow", want)
	safeError(w, http.StatusMethodNotAllowed)
	return false
}

func (a *Auth) allowed(i model.Identity) bool {
	if i.Issuer != a.cfg.Auth.IssuerURL || i.Subject == "" {
		return false
	}
	for _, subject := range a.cfg.Auth.AdminSubjects {
		if subject != "" && subject == i.Subject {
			return true
		}
	}
	for _, group := range i.Groups {
		for _, allowed := range a.cfg.Auth.AdminGroups {
			if allowed != "" && group == allowed {
				return true
			}
		}
	}
	return false
}

// Middleware validates the durable session and current local authorization on
// every request. Unsafe methods additionally require same-origin and CSRF.
func (a *Auth) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		cleanupCtx, cleanupCancel := context.WithTimeout(r.Context(), 5*time.Second)
		cleanupErr := a.cleanup(cleanupCtx)
		cleanupCancel()
		if cleanupErr != nil {
			safeError(w, http.StatusServiceUnavailable)
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil || !validOpaque(c.Value) {
			a.unauthorized(w, r)
			return
		}
		var s session
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := a.store.Get(ctx, sessionBucket, hash(c.Value), &s); err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				safeError(w, http.StatusServiceUnavailable)
				return
			}
			a.unauthorized(w, r)
			return
		}
		now := time.Now()
		// Also enforce the current configured duration on recovered sessions.
		if !now.Before(s.Identity.ExpiresAt) || s.Identity.AuthenticatedAt.IsZero() || s.Identity.AuthenticatedAt.After(now) || !now.Before(s.Identity.AuthenticatedAt.Add(time.Duration(a.cfg.Auth.SessionMinutes)*time.Minute)) || !now.Before(s.Identity.AuthenticatedAt.Add(15*time.Minute)) {
			clearCookie(w, sessionCookie)
			a.unauthorized(w, r)
			return
		}
		if !a.allowed(s.Identity) {
			safeError(w, http.StatusForbidden)
			return
		}
		s.Identity.CSRF = csrf(c.Value)
		s.Identity.SessionID = hash(c.Value)
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			if !a.sameOrigin(r) {
				safeError(w, http.StatusForbidden)
				return
			}
			token := r.Header.Get("X-CSRF-Token")
			if token == "" {
				r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
				if err := r.ParseForm(); err != nil {
					safeError(w, http.StatusForbidden)
					return
				}
				token = r.PostForm.Get("csrf")
			}
			if !equal(token, s.Identity.CSRF) {
				safeError(w, http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), identityKey{}, s.Identity)))
	})
}

func (a *Auth) unauthorized(w http.ResponseWriter, r *http.Request) {
	if (r.Method == http.MethodGet || r.Method == http.MethodHead) && !strings.HasPrefix(r.URL.Path, "/api/") && strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
		return
	}
	safeError(w, http.StatusUnauthorized)
}
func (a *Auth) sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" {
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" {
		return origin == a.origin
	}
	ref, err := url.Parse(r.Header.Get("Referer"))
	return err == nil && ref.User == nil && ref.Scheme+"://"+ref.Host == a.origin
}

func (a *Auth) rateAllowed(r *http.Request) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	if now.Sub(a.global.start) >= time.Minute {
		a.global = rateEntry{start: now}
	}
	if a.global.count >= 100 {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	for key, entry := range a.rates {
		if now.Sub(entry.start) >= time.Minute {
			delete(a.rates, key)
		}
	}
	entry, exists := a.rates[host]
	if !exists {
		if len(a.rates) >= 1024 {
			return false
		}
		entry.start = now
	}
	if entry.count >= 10 {
		return false
	}
	entry.count++
	a.rates[host] = entry
	a.global.count++
	return true
}

// Login starts a single-use code flow bound to an independent browser cookie.
func (a *Auth) Login(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !method(w, r, http.MethodGet) {
		return
	}
	if !a.rateAllowed(r) {
		w.Header().Set("Retry-After", "60")
		safeError(w, http.StatusTooManyRequests)
		return
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(r.Context(), 5*time.Second)
	cleanupErr := a.cleanup(cleanupCtx)
	cleanupCancel()
	if cleanupErr != nil {
		safeError(w, http.StatusServiceUnavailable)
		return
	}
	state, err := random()
	if err != nil {
		safeError(w, 500)
		return
	}
	browser, err := random()
	if err != nil {
		safeError(w, 500)
		return
	}
	verifier, err := random()
	if err != nil {
		safeError(w, 500)
		return
	}
	nonce, err := random()
	if err != nil {
		safeError(w, 500)
		return
	}
	expires := time.Now().Add(loginTTL)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := a.store.Put(ctx, attemptBucket, hash(state), attempt{BrowserHash: hash(browser), Verifier: verifier, Nonce: nonce, ExpiresAt: expires}); err != nil {
		safeError(w, 503)
		return
	}
	cookie(w, loginCookie, browser, expires)
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier), oidc.Nonce(nonce)), http.StatusSeeOther)
}

// Callback consumes the login attempt before exchanging the authorization code.
func (a *Auth) Callback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	if !method(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	if !validOpaque(state) || len(q["state"]) != 1 {
		safeError(w, 400)
		return
	}
	browser, err := r.Cookie(loginCookie)
	if err != nil || !validOpaque(browser.Value) {
		safeError(w, 400)
		return
	}
	ctx, cancel := context.WithTimeout(oidc.ClientContext(r.Context(), a.client), 15*time.Second)
	defer cancel()
	var attempt attempt
	if err := a.store.Take(ctx, attemptBucket, hash(state), &attempt); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			safeError(w, 400)
		} else {
			safeError(w, 503)
		}
		return
	}
	clearCookie(w, loginCookie)
	if !equal(attempt.BrowserHash, hash(browser.Value)) || !time.Now().Before(attempt.ExpiresAt) || q.Get("error") != "" || q.Get("code") == "" || len(q["code"]) != 1 {
		safeError(w, 400)
		return
	}
	if iss := q.Get("iss"); iss != "" && iss != a.cfg.Auth.IssuerURL {
		safeError(w, 400)
		return
	}
	token, err := a.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(attempt.Verifier))
	if err != nil {
		safeError(w, 401)
		return
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok || raw == "" {
		safeError(w, 401)
		return
	}
	id, err := a.verifier.Verify(ctx, raw)
	if err != nil || id.Subject == "" || !equal(id.Nonce, attempt.Nonce) || !time.Now().Before(id.Expiry) {
		safeError(w, 401)
		return
	}
	var claims struct {
		Email  string   `json:"email"`
		Groups []string `json:"groups"`
		AZP    string   `json:"azp"`
	}
	if err := id.Claims(&claims); err != nil || (claims.AZP != "" && claims.AZP != a.cfg.Auth.ClientID) || (len(id.Audience) > 1 && claims.AZP != a.cfg.Auth.ClientID) {
		safeError(w, 401)
		return
	}
	if id.AccessTokenHash != "" {
		if err := id.VerifyAccessToken(token.AccessToken); err != nil {
			safeError(w, 401)
			return
		}
	}
	now := time.Now()
	expires := now.Add(time.Duration(a.cfg.Auth.SessionMinutes) * time.Minute)
	if id.Expiry.Before(expires) {
		expires = id.Expiry
	}
	i := model.Identity{Issuer: id.Issuer, Subject: id.Subject, Email: claims.Email, Groups: claims.Groups, AuthenticatedAt: now, ExpiresAt: expires}
	if !a.allowed(i) {
		safeError(w, 403)
		return
	}
	rawSession, err := random()
	if err != nil {
		safeError(w, 500)
		return
	}
	// Rotate any existing browser session rather than adopting a supplied ID.
	if old, err := r.Cookie(sessionCookie); err == nil && validOpaque(old.Value) {
		if err := a.store.Delete(ctx, sessionBucket, hash(old.Value)); err != nil && !errors.Is(err, store.ErrNotFound) {
			safeError(w, 503)
			return
		}
	}
	if err := a.store.Put(ctx, sessionBucket, hash(rawSession), session{Identity: i}); err != nil {
		safeError(w, 503)
		return
	}
	cookie(w, sessionCookie, rawSession, expires)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Logout is independently protected even if a router registers it publicly.
// It is local logout only: the application never stores provider tokens.
func (a *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	if !method(w, r, http.MethodPost) {
		return
	}
	a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			safeError(w, 401)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := a.store.Delete(ctx, sessionBucket, hash(c.Value)); err != nil {
			safeError(w, 503)
			return
		}
		clearCookie(w, sessionCookie)
		clearCookie(w, loginCookie)
		http.Redirect(w, r, "/auth/login", http.StatusSeeOther)
	})).ServeHTTP(w, r)
}
