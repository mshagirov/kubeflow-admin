package auth

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/store"
)

type fixture struct {
	a            *Auth
	s            *store.Store
	provider     *httptest.Server
	key          *rsa.PrivateKey
	mu           sync.Mutex
	claims       map[string]any
	challenge    string
	exchanges    int
	badSignature bool
	dbPath       string
}

func setup(t *testing.T) *fixture {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{key: key}
	mux := http.NewServeMux()
	f.provider = httptest.NewTLSServer(mux)
	t.Cleanup(f.provider.Close)
	mux.HandleFunc("GET /.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"issuer": f.provider.URL, "authorization_endpoint": f.provider.URL + "/authorize", "token_endpoint": f.provider.URL + "/token", "jwks_uri": f.provider.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
	})
	mux.HandleFunc("GET /keys", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": "test", "use": "sig", "alg": "RS256", "n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())}}})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.exchanges++
		id, secret, ok := r.BasicAuth()
		if err := r.ParseForm(); err != nil || !ok || id != "client" || secret != "secret" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "good-code" || r.Form.Get("redirect_uri") != "https://admin.example/auth/callback" || hash(r.Form.Get("code_verifier")) != f.challenge {
			t.Errorf("invalid confidential code+PKCE exchange")
			http.Error(w, "bad exchange", 400)
			return
		}
		header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"test"}`))
		body, err := json.Marshal(f.claims)
		if err != nil {
			t.Error(err)
			return
		}
		input := header + "." + base64.RawURLEncoding.EncodeToString(body)
		h := sha256.Sum256([]byte(input))
		sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
		if err != nil {
			t.Error(err)
			return
		}
		if f.badSignature {
			sig[0] ^= 1
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"access_token": "never-persist-access", "token_type": "Bearer", "id_token": input + "." + base64.RawURLEncoding.EncodeToString(sig)})
	})
	path := filepath.Join(t.TempDir(), "auth.db")
	f.dbPath = path
	f.s, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.s.Close() })
	secret := filepath.Join(t.TempDir(), "client-secret")
	if err := os.WriteFile(secret, []byte("secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Server: config.Server{PublicURL: "https://admin.example"}, Auth: config.Auth{IssuerURL: f.provider.URL, ClientID: "client", ClientSecretFile: secret, SessionMinutes: 15, AdminGroups: []string{"admins"}}}
	f.a, err = New(context.WithValue(context.Background(), oauth2.HTTPClient, f.provider.Client()), cfg, f.s)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

type loginFlow struct {
	callback string
	browser  *http.Cookie
	state    string
}

func (f *fixture) start(t *testing.T) loginFlow {
	t.Helper()
	r := httptest.NewRequest("GET", "https://admin.example/auth/login", nil)
	w := httptest.NewRecorder()
	f.a.Login(w, r)
	if w.Code != 303 {
		t.Fatalf("login status %d: %s", w.Code, w.Body.String())
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("response_type") != "code" || q.Get("client_id") != "client" || q.Get("code_challenge_method") != "S256" || !validOpaque(q.Get("nonce")) || !validOpaque(q.Get("state")) {
		t.Fatal("insecure authorization request", q)
	}
	f.mu.Lock()
	f.challenge = q.Get("code_challenge")
	f.claims = map[string]any{"iss": f.provider.URL, "sub": "alice", "aud": "client", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": q.Get("nonce"), "groups": []string{"admins"}, "email": "alice@example"}
	f.mu.Unlock()
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing login cookie")
	}
	checkCookie(t, cookies[0])
	return loginFlow{callback: "https://admin.example/auth/callback?code=good-code&state=" + q.Get("state"), browser: cookies[0], state: q.Get("state")}
}
func (f *fixture) callback(flow loginFlow) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", flow.callback, nil)
	if flow.browser != nil {
		r.AddCookie(flow.browser)
	}
	w := httptest.NewRecorder()
	f.a.Callback(w, r)
	return w
}
func checkCookie(t *testing.T, c *http.Cookie) {
	t.Helper()
	if !c.Secure || !c.HttpOnly || c.Domain != "" || c.Path != "/" || c.SameSite != http.SameSiteLaxMode || !strings.HasPrefix(c.Name, "__Host-") {
		t.Fatalf("insecure cookie %+v", c)
	}
}
func findCookie(t *testing.T, w *httptest.ResponseRecorder, name string) *http.Cookie {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("missing %s cookie (status %d)", name, w.Code)
	return nil
}
func (f *fixture) request(method, path string, c *http.Cookie, origin, token, body string) (*httptest.ResponseRecorder, model.Identity) {
	r := httptest.NewRequest(method, "https://admin.example"+path, strings.NewReader(body))
	if c != nil {
		r.AddCookie(c)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if token != "" {
		r.Header.Set("X-CSRF-Token", token)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	var i model.Identity
	w := httptest.NewRecorder()
	f.a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { i, _ = Identity(r.Context()); w.WriteHeader(204) })).ServeHTTP(w, r)
	return w, i
}

func TestOIDCFlowDurableHashedSessionAndCSRF(t *testing.T) {
	f := setup(t)
	flow := f.start(t)
	w := f.callback(flow)
	if w.Code != 303 || w.Header().Get("Location") != "/" {
		t.Fatalf("callback %d %s", w.Code, w.Body.String())
	}
	c := findCookie(t, w, sessionCookie)
	checkCookie(t, c)
	if c.MaxAge > 900 || c.MaxAge < 895 {
		t.Fatal("bad max age", c.MaxAge)
	}
	w, i := f.request("GET", "/", c, "", "", "")
	if w.Code != 204 || i.Subject != "alice" || i.Issuer != f.provider.URL || i.CSRF == "" {
		t.Fatal("identity not available", w.Code, i)
	}
	if i.SessionID != hash(c.Value) {
		t.Fatal("missing server-only session credential")
	}
	encodedIdentity, err := json.Marshal(i)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encodedIdentity), i.SessionID) || strings.Contains(string(encodedIdentity), c.Value) {
		t.Fatal("browser identity exposes session credential")
	}
	var persisted session
	if err := f.s.Get(context.Background(), sessionBucket, hash(c.Value), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Identity.CSRF != "" {
		t.Fatal("CSRF persisted")
	}
	var raw session
	if err := f.s.Get(context.Background(), sessionBucket, c.Value, &raw); err != store.ErrNotFound {
		t.Fatal("raw session persisted", err)
	}
	for _, bucket := range []string{sessionBucket, attemptBucket} {
		rows, err := f.s.List(context.Background(), bucket, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if strings.Contains(string(row), "never-persist-access") || strings.Contains(string(row), "id_token") || strings.Contains(string(row), c.Value) {
				t.Fatal("token persisted")
			}
		}
	}
	if again := f.callback(flow); again.Code != 400 {
		t.Fatal("callback replay succeeded", again.Code)
	}
	for _, tc := range []struct {
		method, origin, token, body string
		want                        int
	}{
		{"POST", "https://admin.example", i.CSRF, "", 204},
		{"PUT", "https://admin.example", i.CSRF, "", 204},
		{"DELETE", "https://admin.example", i.CSRF, "", 204},
		{"PATCH", "https://admin.example", i.CSRF, "", 204},
		{"POST", "https://evil.example", i.CSRF, "", 403},
		{"POST", "", i.CSRF, "", 403},
		{"POST", "https://admin.example", "wrong", "", 403},
		{"POST", "https://admin.example", "", "", 403},
		{"POST", "https://admin.example", "", "csrf=" + url.QueryEscape(i.CSRF), 204},
	} {
		w, _ := f.request(tc.method, "/api/v1/plans", c, tc.origin, tc.token, tc.body)
		if w.Code != tc.want {
			t.Errorf("%+v got %d", tc, w.Code)
		}
	}
	// Database-backed sessions work with a freshly initialized Auth instance.
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	f.s = reopened
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, f.provider.Client())
	newAuth, err := New(ctx, f.a.cfg, f.s)
	if err != nil {
		t.Fatal(err)
	}
	f.a = newAuth
	if w, _ := f.request("GET", "/", c, "", "", ""); w.Code != 204 {
		t.Fatal("session did not survive reinitialization")
	}
	// Revocation/configuration changes are applied without trusting old claims.
	f.a.cfg.Auth.AdminGroups = []string{"other"}
	if w, _ := f.request("GET", "/", c, "", "", ""); w.Code != 403 {
		t.Fatal("allowlist change ignored")
	}
	f.a.cfg.Auth.AdminSubjects = []string{"alice"}
	if w, _ := f.request("GET", "/", c, "", "", ""); w.Code != 204 {
		t.Fatal("exact subject not authorized")
	}
	f.a.cfg.Auth.IssuerURL = "https://different.example"
	if w, _ := f.request("GET", "/", c, "", "", ""); w.Code != 403 {
		t.Fatal("foreign issuer authorized by matching subject")
	}
}

func TestTokenRejections(t *testing.T) {
	f := setup(t)
	for _, tc := range []struct {
		name      string
		mutate    func(map[string]any)
		signature bool
		want      int
	}{
		{"nonce", func(c map[string]any) { c["nonce"] = "wrong" }, false, 401},
		{"issuer", func(c map[string]any) { c["iss"] = "https://evil.example" }, false, 401},
		{"audience", func(c map[string]any) { c["aud"] = "other" }, false, 401},
		{"expired", func(c map[string]any) { c["exp"] = time.Now().Add(-time.Second).Unix() }, false, 401},
		{"subject", func(c map[string]any) { c["sub"] = "" }, false, 401},
		{"signature", func(c map[string]any) {}, true, 401},
		{"group-prefix", func(c map[string]any) { c["groups"] = []string{"admins-extra"} }, false, 403},
		{"no-groups", func(c map[string]any) { delete(c, "groups") }, false, 403},
		{"malformed-groups", func(c map[string]any) { c["groups"] = "admins" }, false, 401},
		{"azp", func(c map[string]any) { c["azp"] = "other" }, false, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			flow := f.start(t)
			f.mu.Lock()
			tc.mutate(f.claims)
			f.badSignature = tc.signature
			f.mu.Unlock()
			w := f.callback(flow)
			if w.Code != tc.want {
				t.Fatalf("got %d want %d", w.Code, tc.want)
			}
			for _, c := range w.Result().Cookies() {
				if c.Name == sessionCookie && c.Value != "" {
					t.Fatal("issued session after rejected token")
				}
			}
		})
	}
}

func TestLoginAttemptBindingAndExpiry(t *testing.T) {
	f := setup(t)
	for _, kind := range []string{"missing-cookie", "wrong-cookie", "expired", "wrong-state", "provider-error"} {
		t.Run(kind, func(t *testing.T) {
			flow := f.start(t)
			switch kind {
			case "missing-cookie":
				flow.browser = nil
			case "wrong-cookie":
				raw, _ := random()
				flow.browser = &http.Cookie{Name: loginCookie, Value: raw}
			case "expired":
				var a attempt
				if err := f.s.Get(context.Background(), attemptBucket, hash(flow.state), &a); err != nil {
					t.Fatal(err)
				}
				a.ExpiresAt = time.Now().Add(-time.Second)
				if err := f.s.Put(context.Background(), attemptBucket, hash(flow.state), a); err != nil {
					t.Fatal(err)
				}
			case "wrong-state":
				raw, _ := random()
				flow.callback = "https://admin.example/auth/callback?code=good-code&state=" + raw
			case "provider-error":
				flow.callback += "&error=access_denied"
			}
			if w := f.callback(flow); w.Code != 400 {
				t.Fatal("accepted bad attempt", w.Code)
			}
		})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exchanges != 0 {
		t.Fatal("exchanged invalid attempt")
	}
}

func TestIDExpiryBoundsSession(t *testing.T) {
	f := setup(t)
	flow := f.start(t)
	exp := time.Now().Add(40 * time.Second).Unix()
	f.mu.Lock()
	f.claims["exp"] = exp
	f.mu.Unlock()
	w := f.callback(flow)
	c := findCookie(t, w, sessionCookie)
	var s session
	if err := f.s.Get(context.Background(), sessionBucket, hash(c.Value), &s); err != nil {
		t.Fatal(err)
	}
	if s.Identity.ExpiresAt.Unix() != exp || c.MaxAge > 40 {
		t.Fatal("ID expiry exceeded")
	}
	s.Identity.ExpiresAt = time.Now().Add(-time.Second)
	if err := f.s.Put(context.Background(), sessionBucket, hash(c.Value), s); err != nil {
		t.Fatal(err)
	}
	if w, _ := f.request("GET", "/", c, "", "", ""); w.Code != 401 {
		t.Fatal("expired session accepted")
	}
}

func TestLogoutProtectedAndRevokes(t *testing.T) {
	f := setup(t)
	w := f.callback(f.start(t))
	c := findCookie(t, w, sessionCookie)
	for _, tc := range []struct {
		method, origin, token string
		want                  int
	}{
		{"GET", "", "", 405}, {"POST", "https://evil.example", csrf(c.Value), 403}, {"POST", "https://admin.example", "", 403}, {"POST", "https://admin.example", csrf(c.Value), 303},
	} {
		r := httptest.NewRequest(tc.method, "https://admin.example/logout", nil)
		r.AddCookie(c)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-CSRF-Token", tc.token)
		w := httptest.NewRecorder()
		f.a.Logout(w, r)
		if w.Code != tc.want {
			t.Errorf("logout %+v: %d", tc, w.Code)
		}
	}
	if w, _ := f.request("GET", "/", c, "", "", ""); w.Code != 401 {
		t.Fatal("logout did not revoke")
	}
}

func TestUnauthenticatedAndRateLimit(t *testing.T) {
	f := setup(t)
	for _, tc := range []struct {
		path, accept string
		want         int
	}{{"/", "text/html", 303}, {"/api/v1/me", "text/html", 401}, {"/", "application/json", 401}} {
		r := httptest.NewRequest("GET", "https://admin.example"+tc.path, nil)
		r.Header.Set("Accept", tc.accept)
		r.Header.Set("X-Auth-Request-User", "alice")
		r.Header.Set("X-Forwarded-Groups", "admins")
		r.Header.Set("Authorization", "Bearer fake")
		w := httptest.NewRecorder()
		f.a.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("spoofed headers accepted") })).ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatal(tc, w.Code)
		}
	}
	for n := 0; n < 11; n++ {
		r := httptest.NewRequest("GET", "https://admin.example/auth/login", nil)
		r.Header.Set("X-Forwarded-For", fmt.Sprint(n))
		w := httptest.NewRecorder()
		f.a.Login(w, r)
		want := 303
		if n == 10 {
			want = 429
		}
		if w.Code != want {
			t.Fatalf("rate limit request %d got %d", n, w.Code)
		}
	}
}

func TestConcurrentCallbackIsSingleUse(t *testing.T) {
	f := setup(t)
	flow := f.start(t)
	results := make(chan int, 2)
	for n := 0; n < 2; n++ {
		go func() { results <- f.callback(flow).Code }()
	}
	first, second := <-results, <-results
	if !((first == 303 && second == 400) || (first == 400 && second == 303)) {
		t.Fatalf("callback results %d, %d", first, second)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.exchanges != 1 {
		t.Fatalf("exchanged %d times", f.exchanges)
	}
}

func TestSameOriginAndCurrentLifetime(t *testing.T) {
	f := setup(t)
	w := f.callback(f.start(t))
	c := findCookie(t, w, sessionCookie)
	for _, tc := range []struct {
		origin, referrer, site string
		want                   int
	}{
		{"", "https://admin.example/page", "same-origin", 204},
		{"", "https://admin.example.evil/page", "", 403},
		{"null", "https://admin.example/page", "", 403},
		{"https://evil.example", "https://admin.example/page", "", 403},
		{"https://admin.example", "", "cross-site", 403},
		{"https://admin.example", "", "same-site", 403},
	} {
		r := httptest.NewRequest("POST", "https://admin.example/action", nil)
		r.AddCookie(c)
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("Referer", tc.referrer)
		r.Header.Set("Sec-Fetch-Site", tc.site)
		r.Header.Set("X-CSRF-Token", csrf(c.Value))
		w := httptest.NewRecorder()
		f.a.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%+v: got %d", tc, w.Code)
		}
	}
	var s session
	if err := f.s.Get(context.Background(), sessionBucket, hash(c.Value), &s); err != nil {
		t.Fatal(err)
	}
	s.Identity.AuthenticatedAt = time.Now().Add(-2 * time.Minute)
	if err := f.s.Put(context.Background(), sessionBucket, hash(c.Value), s); err != nil {
		t.Fatal(err)
	}
	f.a.cfg.Auth.SessionMinutes = 1
	if w, _ := f.request("GET", "/", c, "", "", ""); w.Code != 401 {
		t.Fatal("shorter configured duration not enforced")
	}
	f.a.cfg.Auth.SessionMinutes = 15
	s.Identity.AuthenticatedAt = time.Now().Add(-16 * time.Minute)
	if err := f.s.Put(context.Background(), sessionBucket, hash(c.Value), s); err != nil {
		t.Fatal(err)
	}
	if w, _ := f.request("GET", "/", c, "", "", ""); w.Code != 401 {
		t.Fatal("absolute 15 minute lifetime not enforced")
	}
}

func TestRotationInvalidatesExistingSession(t *testing.T) {
	f := setup(t)
	old := findCookie(t, f.callback(f.start(t)), sessionCookie)
	flow := f.start(t)
	r := httptest.NewRequest("GET", flow.callback, nil)
	r.AddCookie(flow.browser)
	r.AddCookie(old)
	w := httptest.NewRecorder()
	f.a.Callback(w, r)
	current := findCookie(t, w, sessionCookie)
	if old.Value == current.Value {
		t.Fatal("session not rotated")
	}
	if w, _ := f.request("GET", "/", old, "", "", ""); w.Code != 401 {
		t.Fatal("old session still valid")
	}
	if w, _ := f.request("GET", "/", current, "", "", ""); w.Code != 204 {
		t.Fatal("new session invalid")
	}
}

func TestMethodsAndInitializationFailClosed(t *testing.T) {
	f := setup(t)
	for _, handler := range []http.HandlerFunc{f.a.Login, f.a.Callback} {
		r := httptest.NewRequest("POST", "https://admin.example/auth/login", nil)
		w := httptest.NewRecorder()
		handler(w, r)
		if w.Code != 405 {
			t.Fatal("unsafe login method accepted")
		}
	}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, f.provider.Client())
	if _, err := New(ctx, f.a.cfg, nil); err == nil {
		t.Fatal("accepted nil store")
	}
	for _, mutate := range []func(*config.Config){
		func(c *config.Config) { c.Auth.SessionMinutes = 16 },
		func(c *config.Config) { c.Auth.SessionMinutes = 0 },
		func(c *config.Config) { c.Auth.AdminGroups = nil; c.Auth.AdminSubjects = nil },
		func(c *config.Config) { c.Auth.IssuerURL = "http://provider.example" },
		func(c *config.Config) { c.Server.PublicURL = "http://admin.example" },
		func(c *config.Config) { c.Server.PublicURL = "https://admin.example/prefix" },
		func(c *config.Config) { c.Server.PublicURL = "https://admin.example?query" },
		func(c *config.Config) { c.Auth.ClientSecretFile = "" },
	} {
		cfg := f.a.cfg
		mutate(&cfg)
		if _, err := New(ctx, cfg, f.s); err == nil {
			t.Fatal("accepted invalid configuration")
		}
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "https://admin.example/auth/login", nil)
	w := httptest.NewRecorder()
	f.a.Login(w, r)
	if w.Code != 503 || strings.Contains(w.Body.String(), "sql") {
		t.Fatal("unsafe database error", w.Code, w.Body.String())
	}
}
