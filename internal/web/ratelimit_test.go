package web

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	"kubeflow-admin/internal/model"
)

func TestWriteRateBeforePrepareAndSubmit(t *testing.T) {
	s, _, o := fixture()
	h := handler(s)
	for n := 0; n < actorWritesPerMinute; n++ {
		if w := request(h, "POST", "/api/v1/plans", `{"kind":"profile.create","name":"team"}`); w.Code != 201 {
			t.Fatal(n, w.Code)
		}
	}
	for _, path := range []string{"/api/v1/plans", "/api/v1/operations"} {
		w := request(h, "POST", path, `{"planID":"server-plan","idempotencyKey":"1234567890123456"}`)
		retry, err := strconv.Atoi(w.Header().Get("Retry-After"))
		if w.Code != 429 || err != nil || retry < 1 || retry > 60 {
			t.Fatal(w.Code, w.Header(), w.Body.String())
		}
	}
	if o.plans != actorWritesPerMinute || o.submits != 0 {
		t.Fatal(o.plans, o.submits)
	}
}

func TestGlobalWriteBudgetAndWindowCleanup(t *testing.T) {
	var limiter writeLimiter
	now := time.Now()
	for n := 0; n < globalWritesPerMinute; n++ {
		if ok, _ := limiter.allow(fmt.Sprintf("actor-%d", n), now); !ok {
			t.Fatal(n)
		}
	}
	if ok, retry := limiter.allow("new-actor", now); ok || retry != 60 {
		t.Fatal(ok, retry)
	}
	if len(limiter.actors) > maxWriteActors {
		t.Fatal(len(limiter.actors))
	}
	if ok, _ := limiter.allow("new-actor", now.Add(time.Minute)); !ok {
		t.Fatal("window did not reset")
	}
	if len(limiter.actors) != 1 || limiter.total != 1 {
		t.Fatal("old actors retained", len(limiter.actors), limiter.total)
	}
}

func TestGlobalWriteRateHTTP(t *testing.T) {
	s, _, o := fixture()
	actor := 0
	s.identity = func(context.Context) (model.Identity, bool) {
		return model.Identity{Issuer: "issuer", Subject: fmt.Sprintf("actor-%d", actor)}, true
	}
	h := handler(s)
	for n := 0; n < globalWritesPerMinute; n++ {
		actor = n
		if w := request(h, "POST", "/api/v1/plans", `{"kind":"profile.create","name":"team"}`); w.Code != 201 {
			t.Fatal(n, w.Code)
		}
	}
	actor++
	if w := request(h, "POST", "/api/v1/operations", `{"planID":"server-plan","idempotencyKey":"1234567890123456"}`); w.Code != 429 {
		t.Fatal(w.Code)
	}
	if o.plans != globalWritesPerMinute || o.submits != 0 {
		t.Fatal(o.plans, o.submits)
	}
}

func TestWriteLimiterConcurrentAndAfterIdentity(t *testing.T) {
	var limiter writeLimiter
	now := time.Now()
	var wg sync.WaitGroup
	for n := 0; n < 500; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); limiter.allow("actor", now) }()
	}
	wg.Wait()
	if limiter.total != actorWritesPerMinute {
		t.Fatal(limiter.total)
	}
	s, _, o := fixture()
	s.identity = func(context.Context) (model.Identity, bool) { return model.Identity{}, false }
	if w := request(handler(s), "POST", "/api/v1/plans", `{}`); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if s.rate.total != 0 || o.plans != 0 {
		t.Fatal("unauthenticated request consumed write budget")
	}
}

func TestSubmissionRetransmissionsKeepKeyWithinBudget(t *testing.T) {
	s, _, o := fixture()
	h := handler(s)
	for n := 0; n < 3; n++ {
		if w := request(h, "POST", "/api/v1/operations", `{"planID":"server-plan","idempotencyKey":"1234567890123456"}`); w.Code != 202 {
			t.Fatal(w.Code)
		}
	}
	if o.submits != 3 || o.key != "1234567890123456" {
		t.Fatal(o.submits, o.key)
	}
}
