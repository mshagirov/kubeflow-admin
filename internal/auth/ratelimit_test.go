package auth

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"golang.org/x/oauth2"
)

func loginReq(peer, xff string) *http.Request {
	r := httptest.NewRequest("GET", "https://admin.example/auth/login", nil)
	r.RemoteAddr = peer
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	return r
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The shipped deployment funnels every client through one reverse proxy. The
// default limiter must not let one client exhaust a shared per-peer bucket: it
// applies only the bounded global budget and never trusts forwarded headers.
func TestDefaultLoginLimitIsGlobalAndIgnoresForwardedHeaders(t *testing.T) {
	f := setup(t)
	for n := 0; n < 11; n++ {
		w := httptest.NewRecorder()
		f.a.Login(w, loginReq("192.0.2.1:1234", fmt.Sprintf("203.0.113.%d", n+1)))
		if w.Code != 303 {
			t.Fatalf("shared peer denied at request %d: %d", n, w.Code)
		}
	}
	if len(f.a.rates) != 0 {
		t.Fatalf("default mode created per-source buckets: %v", f.a.rates)
	}
}

func TestGlobalLoginBudgetIsBounded(t *testing.T) {
	f := setup(t)
	r := loginReq("192.0.2.1:1234", "203.0.113.1")
	for n := 0; n < globalLoginPerMinute; n++ {
		if !f.a.rateAllowed(r) {
			t.Fatalf("denied before the global budget at %d", n)
		}
	}
	if f.a.rateAllowed(r) {
		t.Fatal("global login budget not enforced")
	}
	if len(f.a.rates) != 0 {
		t.Fatalf("global-only mode must not create source buckets: %d", len(f.a.rates))
	}
}

func TestGlobalLoginBudgetBoundsDistinctSourceBuckets(t *testing.T) {
	f := setup(t)
	f.a.cfg.Auth.LoginSourceLimit = "peer"
	for n := 0; n < globalLoginPerMinute; n++ {
		if !f.a.rateAllowed(loginReq(fmt.Sprintf("203.0.113.%d:1234", n+1), "")) {
			t.Fatalf("distinct source denied before global budget at %d", n)
		}
	}
	if len(f.a.rates) != globalLoginPerMinute {
		t.Fatalf("expected %d distinct buckets, got %d", globalLoginPerMinute, len(f.a.rates))
	}
	if f.a.rateAllowed(loginReq("198.51.100.1:1234", "")) || len(f.a.rates) != globalLoginPerMinute {
		t.Fatal("exhausted global budget must reject new sources without allocating a bucket")
	}
}

func TestPeerModeIgnoresForwardedHeaders(t *testing.T) {
	f := setup(t)
	f.a.cfg.Auth.LoginSourceLimit = "peer"
	for n := 0; n < sourceLoginPerMinute; n++ {
		w := httptest.NewRecorder()
		f.a.Login(w, loginReq("192.0.2.1:1234", fmt.Sprintf("203.0.113.%d", n+1)))
		if w.Code != 303 {
			t.Fatalf("peer request %d: %d", n, w.Code)
		}
	}
	final := httptest.NewRecorder()
	f.a.Login(final, loginReq("192.0.2.1:1234", "203.0.113.99"))
	if final.Code != 429 {
		t.Fatal("peer limit not enforced despite rotated X-Forwarded-For")
	}
}

func TestForwardedModeValidatesChainRightToLeft(t *testing.T) {
	f := setup(t)
	f.a.cfg.Auth.LoginSourceLimit = "forwarded"
	f.a.trusted = []*net.IPNet{mustCIDR(t, "192.0.2.0/24")}
	for _, tc := range []struct {
		name, peer, xff, want string
		ok                    bool
	}{
		{"direct client", "192.0.2.1:1234", "203.0.113.7", "203.0.113.7", true},
		{"spoofed leftmost ignored", "192.0.2.1:1234", "6.6.6.6, 203.0.113.9", "203.0.113.9", true},
		{"trusted hop skipped", "192.0.2.1:1234", "192.0.2.9, 203.0.113.11", "203.0.113.11", true},
		{"malformed chain", "192.0.2.1:1234", "not-an-ip", "", false},
		{"all hops trusted", "192.0.2.1:1234", "192.0.2.5", "", false},
		{"missing chain", "192.0.2.1:1234", "", "", false},
		{"untrusted immediate peer", "198.51.100.9:1234", "203.0.113.12", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := f.a.loginSource(loginReq(tc.peer, tc.xff))
			if ok != tc.ok || got != tc.want {
				t.Fatalf("got (%q,%v) want (%q,%v)", got, ok, tc.want, tc.ok)
			}
		})
	}
	// Each validated client gets an independent budget; a spoofed prefix does
	// not create a new bucket.
	r := loginReq("192.0.2.1:1234", "203.0.113.7")
	for n := 0; n < sourceLoginPerMinute; n++ {
		if !f.a.rateAllowed(r) {
			t.Fatalf("source denied at %d", n)
		}
	}
	if f.a.rateAllowed(r) {
		t.Fatal("per-source limit not enforced")
	}
	if !f.a.rateAllowed(loginReq("192.0.2.1:1234", "203.0.113.8")) {
		t.Fatal("independent client blocked by another client's budget")
	}
	if !f.a.rateAllowed(loginReq("192.0.2.1:1234", "192.0.2.5")) {
		t.Fatal("untrusted/malformed chain should fall back to the global budget")
	}
}

func TestNewAcceptsForwardedTrustedProxyConfig(t *testing.T) {
	f := setup(t)
	cfg := f.a.cfg
	cfg.Auth.LoginSourceLimit = "forwarded"
	cfg.Auth.TrustedProxyCIDRs = []string{"192.0.2.0/24", "2001:db8::/32"}
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, f.provider.Client())
	a, err := New(ctx, cfg, f.s)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.trusted) != 2 {
		t.Fatalf("trusted networks: %d", len(a.trusted))
	}
}
