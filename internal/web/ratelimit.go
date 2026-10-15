package web

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"kubeflow-admin/internal/model"
)

// Plans and submissions share a fixed one-minute write budget: 30 per actor,
// 120 globally. Retransmissions use the same budget but retain their original
// idempotency key. A 1024-entry hard cap and whole-window cleanup bound memory;
// the global limit normally restricts the map to at most 120 entries.
const actorWritesPerMinute = 30
const globalWritesPerMinute = 120
const maxWriteActors = 1024

type writeLimiter struct {
	mu      sync.Mutex
	started time.Time
	total   int
	actors  map[string]int
}

func (l *writeLimiter) allow(actor string, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.started.IsZero() || !now.Before(l.started.Add(time.Minute)) {
		l.started = now
		l.total = 0
		l.actors = make(map[string]int)
	}
	retry := int(l.started.Add(time.Minute).Sub(now).Seconds()) + 1
	if retry > 60 {
		retry = 60
	}
	if retry < 1 {
		retry = 1
	}
	if l.total >= globalWritesPerMinute || l.actors[actor] >= actorWritesPerMinute || (len(l.actors) >= maxWriteActors && l.actors[actor] == 0) {
		return false, retry
	}
	l.actors[actor]++
	l.total++
	return true, 0
}

func (s *server) allowWrite(w http.ResponseWriter, actor model.Identity) bool {
	allowed, retry := s.rate.allow(actor.Key(), time.Now())
	if !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		failure(w, http.StatusTooManyRequests, "rate_limited", "Too many plan or submission requests. Wait for Retry-After before retrying; keep the same plan and idempotency key for a submission.", true)
	}
	return allowed
}
