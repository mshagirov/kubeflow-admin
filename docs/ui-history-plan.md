# Responsive administration and bounded history — delivery plan

> Historical delivery plan, not the current UI/API contract. Later releases added
> Namespaces/Nodes navigation, simplified Pod/Node columns, consolidated Workspace
> Health, compact notification stacks, gated storage Edit/Delete and updated panel
> layouts/labels. Use [the current UI guide](ui.md), [live acceptance](ui-workflow-acceptance.md),
> [History](history.md) and [OpenAPI](../api/openapi.yaml) for implemented behavior.

## Scope and boundaries

This release improves navigation, tables, feedback, resource inspection and
history. It adds a separately authorized managed-pod restart workflow. Preserve
the existing logout fix, bulk inventory reads, UID-based ownership checks,
server-owned previews, CSRF, per-checkpoint authorization and fail-closed recovery.
GPU selection and additional notebook PVC editing are separate follow-up work.
No real environment identifiers, credentials or deployment output belong here.

## 1. Shared table/viewer component

- All long resource/history lists use a bounded-height viewport with horizontal
  and vertical scrolling, sticky headers and keyboard-accessible scrolling.
  On small screens controls wrap and content does not expand the whole page.
- Each table has local search, namespace filtering where applicable, result
  counts and sortable descriptive columns. Sorting is stable and compares
  dates, Kubernetes quantities (capacity/CPU/memory), names/IDs and lifecycle or
  severity values appropriately. Missing values sort last. Do not sort numeric
  quantities lexicographically or confuse requested resources with live usage.
- Place the active namespace/search controls immediately above the visible
  table. `/` focuses search unless typing in an input, editing content, or in a
  modal. Keep sort/search/filter state across in-document navigation.
- Avoid building hidden tables, persistent browser inventory caches and
  additional whole-cluster queries for each sidebar click. Mutation evidence
  still comes from live Kubernetes reads, never from table observations.

## 2. Layout and resource management

- Sidebar order: Overview, Profiles, Storage, Notebooks, Pods, History.
- Manage Profile/Storage/Notebook become buttons opening accessible right-side
  modal drawers, with close/Escape, focus management and bounded scroll regions.
  Keep plan confirmation a separate explicit step; preserve typed inputs and
  server validation. Close a management drawer before opening the review dialog.
- Profiles lists only actual Profiles and their associated namespaces, not
  every cluster namespace. Selecting a Profile opens a summary modal with
  non-zero resource counts and navigation links scoped to its namespace.
- Count only resources actually observed by the integration. Notebook, Pod,
  PVC and other already-supported Kubernetes resources can be counted locally.
  KFP pipeline/run counts need a real KFP API integration; do not infer pipelines
  from arbitrary Pods, treat missing integration as zero, or silently grant
  new broad API permissions. State unavailable integrations clearly.
- Storage gains a sortable Resource Type column immediately after name/ID.

## 3. Feedback and warnings

- Non-modal accessible notification region: users can continue working while
  reading messages. Success/informational notices expire after 10 seconds.
  Errors/warnings remain until explicitly dismissed; include severity and close
  controls and do not obscure confirmations or keyboard focus.
- Deduplicate warnings. Remember dismissed messages in sessionStorage only for
  the current authenticated session, using a non-secret per-session scope.
  Do not store tokens, identities, inventory or message contents persistently;
  store dismissal identifiers only. A new login/session reminds the user again.
- Keep detailed warnings inside change previews as well: dismissing a display
  warning must never acknowledge or bypass a mutation precondition.

## 4. Unified History

- History has Operations and Audit tabs using the shared table/search/sort
  viewer. Preserve old operations/audit links as compatible entry points.
- Show human-readable administrator username (verified OIDC preferred_username,
  then email, then a safe subject fallback); retain immutable issuer/subject
  identifiers internally for authorization and attribution. Do not make email
  the authorization key or invent historical usernames.
- Audit columns: time, actor, resource kind, namespace, resource name, action,
  outcome, actual administrator reason. Operation identifiers belong in their
  own details/ID field, not concatenated into the reason.
- Backend list responses remain bounded. Clearly state truncation/retention
  so an incomplete view is not described as a full historical export.

## 5. Retention and storage bounds

- User-visible audit history retains at most 90 days. Add configurable row and
  serialized-byte budgets with conservative defaults (target: 50,000 rows and
  64 MiB). Purge expired records and oldest eligible history under pressure.
- SQLite audit records are application data, not slog output: rotating log
  files alone cannot implement audit retention. Use transactional SQL cleanup
  with indexes, bounded batches, periodic housekeeping and explicit tests.
- Never prune active/needs-review operation locks, idempotency claims or their
  durable acceptance evidence. If acceptance evidence must outlive visible
  audit history, keep it in a separate safety ledger and migrate existing
  records transactionally. Do not silently turn integrity failures into success.
- Bound retained completed operation detail too where safe, and keep safety
  tombstones separate from the UI history. If protected state exhausts a
  configured budget, stop accepting new mutations with an actionable error,
  rather than dropping essential evidence. State any residual physical-storage
  limits honestly: logical row budgets do not cap all SQLite/WAL files.
- Do not run disruptive full VACUUM on request paths. Use controlled cleanup,
  page reuse/checkpointing and deployment disk monitoring/quotas for whole-PVC
  capacity. Retention also applies to backup policy, not just the live database.
- Go slog has no built-in rotation. Keep stdout as the default (Kubernetes
  kubelet handles container-log rotation); optionally offer a size-bounded,
  age-bounded file handler with explicit configuration and private files.
  Never log tokens, secret values or full resource manifests. File rotation is
  independent of database retention and must not require a new logging server.

## 6. Pods and safe restart

- Pods table displays observed namespace, identity, state/readiness, owner and
  useful non-secret resource summaries; namespace/search/sort work as elsewhere.
- Restart is a separately disabled-by-default feature and optional RBAC grant.
  Use the existing plan/confirm/operation/audit pipeline, not a direct browser
  DELETE endpoint. Preview actual target UID, controller and disruption warning.
- Prefer a normal, disruption-budget-aware eviction with UID/resourceVersion
  preconditions. Refuse unmanaged/static/mirror Pods, protected namespaces,
  terminating targets, unsafe node state and unsupported owner chains. Avoid
  controllers that do not guarantee a replacement; begin with supported
  Notebook StatefulSets and validate the parent Notebook remains running.
- Only succeed after the original UID is gone and a distinct owned replacement
  is Ready with the desired revision. Timeout or ambiguous interruption becomes
  needs-review, never blind replay. Share target locks with notebook operations
  so restart cannot race an edit/stop on its parent.
- No automatic force-delete fallback. Removing the API object with zero grace
  does not prove the original process stopped and can create simultaneous data
  writers on an unreachable node. Show a clear operator-only explanation until
  a separate fencing-aware force workflow is designed and approved.
- Do not broaden the current pilot Role automatically. Operator explicitly
  reviews and installs the new permission and enables the feature for a
  disposable pilot; all existing protections remain on by default.

## Work packages and integration contract

1. **UI agent:** owns `internal/web/assets/` and frontend testdata. Implement
   shared viewers, drawers, profile summary, notifications, Pods and History.
   Backend adds optional `actorDisplay` to operations/audit and
   `username`/non-secret `notificationScope` to `/me`; use backward-compatible
   fallbacks. Pod action is `pod.restart`; server feature projection is
   `Features.PodRestarts`, HTML meta `pod-restarts`. No independent mutations.
2. **History agent:** owns store/history changes, operation actor projection,
   retention housekeeping and optional slog file rotation. Coordinate model and
   config fields with supervisor; test existing database migration, active-state
   preservation, budget pressure, retention cutoff and bounded responses.
3. **Pods agent:** owns Kubernetes restart backend and its tests plus a new
   optional RBAC overlay. Action `pod.restart`, feature `PodRestarts`. Preserve
   live revalidation and locking; no force fallback or broad deletion grants.
   Do not modify frontend, store, model, config or API specification directly;
   report exact supervisor integration needs.
4. **Supervisor:** owns model/config/auth/web/API integration, security review,
   documentation and manifests checks. Review every agent diff, resolve shared
   interfaces, rerun tests, exercise feature-disabled and denied paths and scan
   artifacts before publication. Preserve unrelated uncommitted work.

## Gates and deployment

### Independent plan-review corrections

The corrections in this section supersede earlier provisional implementation
details. In particular, visible Audit events always obey the age horizon and a
hard 1,000-event response bound, even for unresolved operations. Only Operations
adds unresolved rows to its recent window. Required unresolved acceptance
evidence stays in the separate safety ledger, not in an unbounded Audit response.
Completed acceptance reasons/diffs expire when terminal history is compacted;
compact operation/claim tombstones preserve retry identity without retaining
those full payloads forever. Cleanup transactions must be short enough not to
starve authentication and durable checkpoints on the single SQLite connection.

- Freeze model/config/page contracts before agents edit dependent templates.
  `actorDisplay` is captured at acceptance on plans/operations/audit; it is a
  presentation claim, never an authorization identifier. Render a random,
  non-secret `notificationScope` meta value from the current session so warning
  dismissal does not need another API round trip.
- Keep History API arrays backward compatible. Display a clearly labeled recent
  window of at most 1,000 normal records, plus unresolved operations (bounded
  separately). Search/sort apply to loaded records, not all 90 days. Expose window
  and retention headers; do not download all retained history into the browser.
  Retention is an upper bound, not a promise to retain 90 days under byte pressure.
- Migrate the acceptance ledger before pruning audit rows. Preserve compact
  completed operation/idempotency tombstones for replay. Bound total operation
  admission (default 100,000 records) and unresolved work (target 1,000); refuse
  new changes if a safety budget is exhausted. Do not prune a claim whose retry
  would otherwise execute a historical action again. Remove the unconditional
  schema-version reset; migration/restart tests must cover this.
- Pod primary target is `pod:namespace/name`; secondary lock is the validated
  parent `notebook:namespace/name`. Ignore status-only Pod resource-version
  churn in previews, revalidate immutable UID/spec/owner evidence live, and use
  current UID/RV preconditions at the actual eviction request. PDB/403/409
  rejection known to occur before mutation is `failed` (locks released); network
  uncertainty or timeout after eviction remains `needs-review` (locks retained).
- History route is `/history?tab=operations|audit`; legacy paths preselect their
  corresponding tab. Namespace context is shared; query parameter `namespace`
  initializes it and profile-summary links update it. Search/sort are per table.
  Paginate rendered rows (default 50) in addition to bounded scroll viewports.
- Pod inventory explicitly projects owner/readiness/restarts and requested/limit
  resources; real metrics remain unavailable without a metrics integration.
  Profile association follows the Profile-name/namespace-name convention and
  includes only real Profiles. Group audit rows by operation for the summary,
  keeping all stages accessible as details; show actual input reason only.
- Static assets must revalidate on release (`no-cache` with validators or
  fingerprinted URLs), not remain stale for an hour. Update API parity, schema,
  manifest checks and historical docs that previously promised no audit TTL.

1. Review this plan for scope, access boundaries, retained-state integrity and
   unsupported integrations before implementation.
2. All agents use DeepSeek V4.1 Flash with high thinking, isolated file ownership,
   no git history rewrites, no publication and no cluster mutations.
3. Run race tests/vet/build, API parity, all manifest variants, Node table/UI
   tests, retention/migration/rotation tests and restart cancellation/PDB/ownership
   tests. Review changes independently; no site data in source, fixtures or docs.
4. Publish a new version/digest only after integration gates pass. Keep existing
   deployment functional and backup before database migration. New pod restart
   stays disabled until its RBAC and disposable pilot are approved.
5. Operator deploys the image; test small screens, long lists, all sorting/search,
   `/` shortcut, drawers/focus, notification dismissal/relogin, readable history,
   retention limits, profile drilldown, namespace navigation and unchanged
   logout. Confirm latency remains comparable and no extra inventory scans occur.
6. Review real deployment results before widening rollout. Record limitations:
   no fabricated pipeline counts/live usage and no unfenced force deletion.
