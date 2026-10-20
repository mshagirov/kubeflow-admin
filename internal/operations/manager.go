package operations

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"kubeflow-admin/internal/auth"
	"kubeflow-admin/internal/config"
	"kubeflow-admin/internal/model"
	"kubeflow-admin/internal/store"
)

// Backend is implemented by *kube.Service. Keeping the boundary structural
// also permits isolated worker tests without a Kubernetes dependency.
type Backend interface {
	Prepare(context.Context, model.Action) (model.Prepared, error)
	Execute(context.Context, model.Prepared, func(string) error) error
}
type task struct {
	op    model.Operation
	plan  model.Plan
	actor model.Identity
}
type Manager struct {
	cfg       config.Config
	s         *store.Store
	k         Backend
	ctx       context.Context
	cancel    context.CancelFunc
	queue     chan task
	wg        sync.WaitGroup
	requests  sync.WaitGroup
	mu        sync.Mutex
	closed    bool
	closeOnce sync.Once

	cleanupMu       sync.Mutex
	lastPlanCleanup time.Time
}

// historyCleanupInterval is frequent enough to keep logical history bounded but
// each pass deletes only small batches. Request paths never wait for it.
const historyCleanupInterval = time.Minute

func New(ctx context.Context, cfg config.Config, s *store.Store, k Backend) (*Manager, error) {
	return newManager(ctx, cfg, s, k)
}
func newManager(ctx context.Context, cfg config.Config, s *store.Store, k Backend) (*Manager, error) {
	if s == nil || k == nil {
		return nil, errors.New("operation storage and backend required")
	}
	if !s.AcquireWorker() {
		return nil, errors.New("an operation worker already owns this database")
	}
	// Direct config.Config values in tests and the operator path use the same
	// conservative retention defaults.
	cfg.History = cfg.History.WithDefaults()
	ctx, cancel := context.WithCancel(ctx)
	m := &Manager{cfg: cfg, s: s, k: k, ctx: ctx, cancel: cancel, queue: make(chan task, 32)}
	fail := func(err error) (*Manager, error) { cancel(); s.ReleaseWorker(); return nil, err }
	recoveryCtx, recoveryCancel := context.WithTimeout(ctx, 30*time.Second)
	defer recoveryCancel()
	// Orphaned active claims or target locks cannot be recovered automatically
	// without releasing locks or inventing audit history. Fail closed so an
	// operator reconciles them instead of silently unlocking.
	if err := s.CheckIntegrity(recoveryCtx); err != nil {
		return fail(storageError(err))
	}
	list, err := s.IncompleteOperations(recoveryCtx)
	if err != nil {
		return fail(storageError(err))
	}
	// Crash recovery never executes a previously accepted action. Uncertain
	// operations retain their target locks until explicit offline reconciliation.
	for _, op := range list {
		if terminal(op.State) {
			continue
		}
		op.State = "needs-review"
		op.Error = "Application restarted before a durable outcome. Inspect actual cluster state; no action was replayed."
		op.UpdatedAt = time.Now().UTC()
		if err := m.finish(recoveryCtx, op); err != nil {
			return fail(storageError(err))
		}
	}
	m.wg.Add(1)
	go m.worker()
	// One background janitor performs bounded retention and is canceled and
	// waited for by Close, like the operation worker.
	m.wg.Add(1)
	go m.janitor()
	return m, nil
}
func terminal(s string) bool { return s == "succeeded" || s == "failed" || s == "needs-review" }
func id() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("secure identifier generation unavailable")
	}
	return hex.EncodeToString(b[:]), nil
}

func (m *Manager) authorize(ctx context.Context, actor model.Identity) error {
	if !m.cfg.Features.WritesEnabled {
		return errors.New("writes are disabled")
	}
	checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return auth.ValidateSession(checkCtx, m.cfg, m.s, actor)
}

func (m *Manager) Plan(ctx context.Context, actor model.Identity, a model.Action) (model.Plan, error) {
	m.mu.Lock()
	if m.closed || m.ctx.Err() != nil {
		m.mu.Unlock()
		return model.Plan{}, errors.New("server shutting down")
	}
	m.requests.Add(1)
	m.mu.Unlock()
	defer m.requests.Done()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	if err := m.authorize(ctx, actor); err != nil {
		return model.Plan{}, err
	}
	// Bounded-frequency housekeeping: drop expired/malformed preview plans.
	// Accepted work is unaffected because retries resolve
	// through operation_claims, not the plan record. Failure never blocks a
	// preview, and only plans (never operations or audit) are removed.
	m.cleanupExpiredPlans(ctx)
	if len(a.Reason) > 500 {
		return model.Plan{}, errors.New("reason exceeds 500 characters")
	}
	p, err := m.k.Prepare(ctx, a)
	if err != nil {
		// Preserve the backend error's identity and authored message so the HTTP
		// boundary can classify it (409 stale plan, 422 approved validation) with
		// its allowlist. Unknown backend text is not persisted here; the boundary
		// fails closed before exposing it. Replacing this with a fixed message
		// would make every prepared-error classification impossible.
		return model.Plan{}, err
	}
	// A slow preview must not produce an authorized plan after logout/shutdown.
	if ctx.Err() != nil {
		return model.Plan{}, ctx.Err()
	}
	if err := m.authorize(ctx, actor); err != nil {
		return model.Plan{}, err
	}
	planID, err := id()
	if err != nil {
		return model.Plan{}, err
	}
	now := time.Now().UTC()
	plan := model.Plan{ID: planID, Actor: actor.Key(), ActorDisplay: actor.DisplayName(), CreatedAt: now, ExpiresAt: now.Add(5 * time.Minute), Prepared: p}
	if err := m.s.Put(ctx, "plans", plan.ID, plan); err != nil {
		return model.Plan{}, storageError(err)
	}
	return plan, nil
}

const planCleanupInterval = time.Minute

// cleanupExpiredPlans removes expired/malformed preview plans at most once per
// interval. It never removes operations or audit history and never fails the
// caller: stale previews are a hygiene issue, not a write-safety one.
func (m *Manager) cleanupExpiredPlans(ctx context.Context) {
	now := time.Now()
	m.cleanupMu.Lock()
	if now.Sub(m.lastPlanCleanup) < planCleanupInterval {
		m.cleanupMu.Unlock()
		return
	}
	m.lastPlanCleanup = now
	m.cleanupMu.Unlock()
	cleanupCtx, cancel := context.WithTimeout(ctx, store.DefaultCleanupTimeout)
	defer cancel()
	if err := m.s.CleanupPlans(cleanupCtx, now); err != nil {
		m.cleanupMu.Lock()
		m.lastPlanCleanup = time.Time{}
		m.cleanupMu.Unlock()
		// Cleanup is best-effort; do not put raw driver error text in logs.
		slog.Warn("expired plan cleanup failed; retained plans will be retried")
	}
}

func historyPolicy(h config.History) store.HistoryPolicy {
	return store.HistoryPolicy{
		RetentionDays:       h.RetentionDays,
		MaxAuditRecords:     h.MaxAuditRecords,
		MaxAuditBytes:       h.MaxAuditBytes,
		MaxOperationRecords: h.MaxOperationRecords,
	}
}

// janitor owns periodic, bounded history retention. It never runs on a request
// path and is stopped and waited for by Close.
func (m *Manager) janitor() {
	defer m.wg.Done()
	// Run one pass after startup without delaying recovery or serving. It is
	// bounded and transactional, like later passes.
	m.cleanupHistory(m.ctx)
	ticker := time.NewTicker(historyCleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ticker.C:
			m.cleanupHistory(m.ctx)
		}
	}
}

func (m *Manager) cleanupHistory(ctx context.Context) {
	// Audit pruning and operation compaction each get an independent 500ms
	// transaction; the outer span only prevents a hung pass from accumulating.
	cleanupCtx, cancel := context.WithTimeout(ctx, 2*store.DefaultCleanupTimeout)
	defer cancel()
	result, err := m.s.CleanupHistory(cleanupCtx, historyPolicy(m.cfg.History), time.Now())
	if err != nil {
		var integrity *store.IntegrityError
		if errors.As(err, &integrity) {
			slog.Error("history retention stopped on an integrity failure; operator reconciliation required", "reason", integrity.Reason)
			return
		}
		// Best effort, no raw driver/OS text in logs. The next interval retries.
		slog.Warn("bounded history retention did not complete; retained records will be retried")
		return
	}
	if result.PurgedExpired > 0 || result.PurgedUnderPressure > 0 || result.CompactedOperations > 0 || result.RedactedAcceptanceLedger > 0 {
		slog.Info("bounded history retention completed",
			"purgedExpired", result.PurgedExpired,
			"purgedUnderPressure", result.PurgedUnderPressure,
			"compactedOperations", result.CompactedOperations,
			"redactedAcceptanceLedger", result.RedactedAcceptanceLedger)
	}
}

func (m *Manager) Submit(ctx context.Context, actor model.Identity, planID, key string) (model.Operation, error) {
	// Serializing acceptance with Close guarantees every successful acceptance
	// reaches the queue before shutdown begins draining it.
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.ctx.Err() != nil {
		return model.Operation{}, errors.New("server shutting down")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stop := context.AfterFunc(m.ctx, cancel)
	defer stop()
	if err := m.authorize(ctx, actor); err != nil {
		return model.Operation{}, err
	}
	if len(key) < 16 || len(key) > 128 || strings.TrimSpace(key) != key {
		return model.Operation{}, errors.New("idempotency key must be 16..128 characters")
	}
	if prior, err := m.s.OperationForKey(ctx, actor.Key(), key, planID); err == nil {
		return prior, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return model.Operation{}, storageError(err)
	}
	var plan model.Plan
	if err := m.s.Get(ctx, "plans", planID, &plan); err != nil {
		return model.Operation{}, storageError(err)
	}
	if plan.ID != planID || plan.Actor != actor.Key() {
		return model.Operation{}, errors.New("plan belongs to a different actor")
	}
	if !time.Now().Before(plan.ExpiresAt) {
		return model.Operation{}, errors.New("plan expired; preview again")
	}
	if len(m.queue) == cap(m.queue) {
		return model.Operation{}, errors.New("operation queue full; retry later")
	}
	// Admission, including audit row/byte pressure and the exact new accepted row
	// size, is enforced atomically by CreateOperationWithPolicy after idempotent
	// retries have resolved.
	a := plan.Prepared.Action
	target := store.PrimaryTarget(a)
	opID, err := id()
	if err != nil {
		return model.Operation{}, err
	}
	now := time.Now().UTC()
	op := model.Operation{ID: opID, PlanID: plan.ID, Actor: actor.Key(), ActorDisplay: actor.DisplayName(), Target: target, Action: a.Kind, State: "queued", Step: "queued", CreatedAt: now, UpdatedAt: now}
	op, created, err := m.s.CreateOperationWithPolicy(ctx, op, plan, key, historyPolicy(m.cfg.History))
	if err != nil {
		return model.Operation{}, storageError(err)
	}
	if created {
		m.queue <- task{op: op, plan: plan, actor: actor}
	}
	return op, nil
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		// Prefer shutdown over starting another privileged operation.
		if m.ctx.Err() != nil {
			m.drain()
			return
		}
		select {
		case <-m.ctx.Done():
			m.drain()
			return
		case t := <-m.queue:
			if !m.run(t) {
				m.cancel()
			}
		}
	}
}

func (m *Manager) drain() {
	// No new task can be accepted after cancellation; Submit checks m.ctx while
	// holding the same acceptance lock. Wait for an already committing submit.
	m.mu.Lock()
	defer m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		select {
		case t := <-m.queue:
			op := t.op
			op.State = "failed"
			op.Error = "Server stopped before execution; no cluster write was started. Authenticate and preview again."
			op.UpdatedAt = time.Now().UTC()
			err := m.finish(ctx, op)
			if err != nil {
				slog.Error("queued operation outcome not durably recorded; startup review required", "operation", op.ID)
			}
		default:
			return
		}
	}
}

func (m *Manager) run(t task) bool {
	timeoutCtx, timeoutCancel := context.WithTimeout(m.ctx, 25*time.Minute)
	defer timeoutCancel()
	ctx, cancel := context.WithCancelCause(timeoutCtx)
	defer cancel(nil)
	op := t.op
	if err := m.authorize(ctx, t.actor); err != nil || ctx.Err() != nil {
		op.State = "failed"
		op.Error = "Administrator session is no longer authorized or execution was canceled before any write. Authenticate and preview again."
		op.UpdatedAt = time.Now().UTC()
		return m.persistOutcome(op)
	}
	// Revoke long-running waits too, not just requests reaching a write
	// checkpoint. The backend must honor cancellation before each external call.
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := m.authorize(ctx, t.actor); err != nil {
					cancel(errors.New("administrator session revoked or verification unavailable"))
					return
				}
			}
		}
	}()
	checkpoint := func(step string) error {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if err := m.authorize(ctx, t.actor); err != nil {
			return err
		}
		if len(step) > 80 || step == "" {
			return errors.New("invalid operation checkpoint")
		}
		for _, c := range step {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
				return errors.New("invalid operation checkpoint")
			}
		}
		op.State = "applying"
		op.Step = step
		op.UpdatedAt = time.Now().UTC()
		auditID, err := id()
		if err != nil {
			return err
		}
		return m.s.Checkpoint(ctx, op, model.Audit{ID: auditID})
	}
	err := checkpoint("validating")
	if err == nil {
		err = m.k.Execute(ctx, t.plan.Prepared, checkpoint)
	} else {
		// No backend execution or cluster write has started.
		err = errors.Join(model.ErrRejectedBeforeMutation, err)
	}
	// A backend that returns nil after cancellation cannot report success.
	if err == nil && ctx.Err() != nil {
		err = context.Cause(ctx)
	}
	cancel(nil)
	<-monitorDone
	op.UpdatedAt = time.Now().UTC()
	if err == nil {
		op.State = "succeeded"
		op.Step = "verified"
	} else if errors.Is(err, model.ErrRejectedBeforeMutation) {
		// A definite API rejection (for example PDB/403/409 before the eviction or
		// patch request changed anything) is a failed operation. Finishing as
		// failed releases its target locks, so a corrected preview can proceed.
		op.State = "failed"
		op.Step = "rejected"
		op.Error = "The cluster API rejected the request before any change was applied. Nothing was changed; correct the reported precondition, authenticate and preview again."
	} else {
		op.State = "needs-review"
		op.Error = safeError(err) + " Inspect actual cluster state; a notebook may be stopped or storage partially provisioned. No rollback or automatic replay was performed. Targets remain locked pending explicit reconciliation."
	}
	return m.persistOutcome(op)
}

func (m *Manager) persistOutcome(op model.Operation) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := m.finish(ctx, op); err != nil {
		slog.Error("operation outcome not durably recorded; worker stopped and target remains locked", "operation", op.ID)
		return false
	}
	return true
}

// Never persist arbitrary backend/HTTP/OS error text. Substring blocklists
// cannot reliably remove environment values, credentials, or admission bodies.
func safeError(err error) string {
	if errors.Is(err, context.Canceled) {
		return "Operation canceled."
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "Operation timed out."
	}
	if errors.Is(err, auth.ErrSessionInvalid) {
		return "Administrator session is no longer authorized."
	}
	return "Operation could not be completed safely."
}
func storageError(err error) error {
	var integrity *store.IntegrityError
	if errors.As(err, &integrity) {
		// Preserve the typed, actionable integrity failure so operators can act.
		return integrity
	}
	var budget *store.BudgetError
	if errors.As(err, &budget) {
		return budget
	}
	if errors.Is(err, store.ErrNotFound) {
		return store.ErrNotFound
	}
	if errors.Is(err, store.ErrConflict) {
		return store.ErrConflict
	}
	if errors.Is(err, store.ErrInvalidTarget) {
		return store.ErrInvalidTarget
	}
	return errors.New("durable operation storage unavailable")
}
func (m *Manager) finish(ctx context.Context, op model.Operation) error {
	auditID, err := id()
	if err != nil {
		return err
	}
	return m.s.Finish(ctx, op, model.Audit{ID: auditID})
}
func (m *Manager) Get(ctx context.Context, id string) (model.Operation, error) {
	var op model.Operation
	if err := m.s.Get(ctx, "operations", id, &op); err != nil {
		return model.Operation{}, storageError(err)
	}
	return op, nil
}
func (m *Manager) List(ctx context.Context) ([]model.Operation, error) {
	ops, err := m.s.RecentOperations(ctx, store.RecentHistoryLimit)
	if err != nil {
		return nil, storageError(err)
	}
	return ops, nil
}
func (m *Manager) Audit(ctx context.Context) ([]model.Audit, error) {
	audit, err := m.s.RecentAuditWithPolicy(ctx, historyPolicy(m.cfg.History), store.RecentHistoryLimit)
	if err != nil {
		return nil, storageError(err)
	}
	return audit, nil
}

// HistoryStats exposes effective retention limits and the residual safety
// ledger/tombstone usage so HTTP handlers can state truncation and residual
// storage honestly. Logical budgets do not cap physical SQLite/WAL files.
func (m *Manager) HistoryStats(ctx context.Context) (store.HistoryStats, error) {
	stats, err := m.s.HistoryStats(ctx, historyPolicy(m.cfg.History))
	if err != nil {
		return store.HistoryStats{}, storageError(err)
	}
	return stats, nil
}
func (m *Manager) Close() {
	m.closeOnce.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.cancel()
		m.mu.Unlock()
		m.requests.Wait()
		m.wg.Wait()
		m.s.ReleaseWorker()
	})
}
