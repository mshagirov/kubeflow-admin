# Operations and recovery

## Safety and monitoring

Run exactly one application process against the DB. Keep `replicas: 1`, `Recreate`, no HPA. SQLite WAL requires local/block storage. BeeGFS instead uses DELETE journaling with `allowUnverifiedFilesystem: true` and verified client settings; see [database requirements](database.md) for global locks, remote fsync, `umask`-protected files and node fencing. The optional local PV ties the app to a prepared node; scheduling elsewhere cannot recover its files. Node/disk failure means downtime and potentially recovery from backup. Retain is not a backup or an HA mechanism.

Monitor Pod readiness/restarts, DB/PV/node free space and errors, pending/failed/needs-review operations, OIDC failures, Kubernetes permission errors, controller reconciliation and age of backups. `/healthz` checks process liveness; `/readyz` checks DB and required API discovery, not full RBAC, authentication or successful generated Pod admission. No `/metrics` endpoint is promised by this version. History APIs expose bounded recent windows, not a complete archival export. See [History retention](history.md) for the 90-day upper horizon, row/byte budgets, immutable safety ledger and idempotency tombstones. Export using an authorized, reviewed process; protect identity metadata and use platform Kubernetes audit logs for independent attribution. Expired or malformed preview-plan cleanup is separate from background history retention and never removes accepted claims or target locks.

The app's ServiceAccount is a powerful all-user reader, and enabling notebook/PV patches is broader still. No Secret/exec/impersonation privilege does not prevent indirect workload escalation. The optional Pod restart workflow additionally grants normal eviction, never arbitrary or forced deletion; keep it separately disabled unless reviewed. Keep admission policy and gateway/network boundaries in place. Never add `cluster-admin` to fix readiness or a failed workflow.

### Inventory latency and freshness

Inventory lists Pods and StatefulSets once per request and derives notebook
lifecycle summaries from those bulk observations using controller UID and
namespace ownership. Request counts no longer grow with notebook count. These
cross-resource observations are not an atomic cluster snapshot or mutation
preconditions: previews, execution and convergence still perform fresh reads.

Sidebar navigation reuses the current in-memory inventory and renders only the
visible table, without reloading the document. Browser Back/Forward and direct
page links remain supported. Observations older than 30 seconds trigger a fetch
on inventory-page navigation; visible pages also poll every 30 seconds. Refresh
explicitly requests live inventory. Existing observations remain marked with
their original timestamp; on fetch failure a warning says they may be stale.
Inventory is not saved in localStorage/sessionStorage. Operation and audit
requests do not wait for inventory to finish loading.

After updating the image, reload normally: static assets use `no-cache` with ETag
revalidation, so a release should not remain stale for an hour. If a proxy or
browser extension overrides that, clear its cache. In browser Network tools,
compare the initial inventory request duration, then
switch between Notebooks and Storage: switching within the refresh interval
should not trigger another inventory request. Manual Refresh should. Repeat
with a slow/failed request to verify warnings and observation timestamps. Report
durations/statuses only; HAR exports, cookies and response bodies can expose
identities and session data. Fake-client/request-count and JavaScript harness
tests establish behaviour, not a production latency guarantee.

## Backup (no live-copy shortcut)

There is no built-in backup endpoint or SQLite shell in the distroless runtime image. Use the site's protected backup tooling and verify recovery **before** enabling writes. A practical consistent offline backup:

See the [step-by-step backup procedure](backup.md) for stopped-directory commands,
isolated SQLite checks using Python, and a protected off-node SSH/SCP example.

1. Freeze administrative changes, record all active operation IDs/states and current release/config. Disable new writes and wait for completion where possible; interrupting a write may leave a partially completed real change.
2. Operator scales the app to zero and waits for the Pod/process to exit and the volume to be unmounted. Do not attach another process while it is alive. For the node-local PV, work on the recorded prepared node with approved backup tooling; do not exec into the app or grant it backup privileges.
3. Copy/snapshot the **entire stopped DB directory**, including `admin.db` and any `-journal`/`-wal`/`-shm` files that remain, preserving ownership/mode. Alternatively run SQLite's backup API with a compatible trusted tool; never copy only a live main file or manually discard journals. Encrypt backup, restrict access and retain off-node/off-disk copies. Record checksums, time, app/config/schema version and source node/PV.
4. Run SQLite `PRAGMA integrity_check` on an isolated restored copy using approved SQLite tooling, and rehearse app recovery with a **disposable** cluster/API fixture. Then restore the original deployment to one replica and check readiness, operations and login.

Operator commands (run only after the above review):

```sh
kubectl --context YOUR_CONTEXT -n kubeflow-admin scale deployment/kubeflow-admin --replicas=0
kubectl --context YOUR_CONTEXT -n kubeflow-admin wait --for=delete pod -l app.kubernetes.io/name=kubeflow-admin --timeout=180s
# Execute the site's stopped-directory backup procedure on the prepared node.
kubectl --context YOUR_CONTEXT -n kubeflow-admin scale deployment/kubeflow-admin --replicas=1
kubectl --context YOUR_CONTEXT -n kubeflow-admin rollout status deployment/kubeflow-admin --timeout=180s
```

## Recovery and upgrades

- Durable-state integrity: the application fails closed at startup when operation state is internally inconsistent — an active claim with no operation record, an active claim whose operation is already `succeeded` or `failed`, an active claim missing its immutable acceptance-ledger evidence, or an active target lock with no active claim. `needs-review` deliberately keeps its locks and is not an inconsistency. The check never releases locks or synthesizes records. If startup reports an integrity error, stop and inspect the actual cluster state and the database, then reconcile with an operator before restarting; do not hand-edit the database or delete lock rows to bypass the check.
- Start recovery with new writes disabled and optional mutation RBAC revoked by the operator where practical. Record actual cluster state first: a restored DB is older than Kubernetes and cannot roll it back. Never replay completed or ambiguous operations solely because the database says they are pending.
- Stop the app; preserve a forensic copy of the damaged/current directory. Restore to compatible storage for the configured journal mode with UID/GID 65532 and restrictive permissions, using the backed-up complete SQLite file set. Do not mix files from different snapshots. Validate integrity and compatible application/schema versions before starting one instance.
- On node replacement, provision a new safe directory/disk and new local PV with the replacement node's affinity; explicitly migrate/rebind through an operator-reviewed PV/PVC plan. Do not edit bound immutable fields, remove finalizers, repoint the old path blindly or assume Retain makes reuse safe. No generic delete/recreate shortcut is prescribed.
- Incomplete operations recover as `needs-review`; no automatic replay. Fresh-read the actual Profile/Notebook/PV/PVC UIDs and desired/observed state with independent authorized tooling. A notebook may be left stopped, a PV reserved or a PVC already created. Preserve those resources. After a human resolves/acknowledges the outcome, use a **fresh** authorized preview if a retry is still required. This version has no general operation-resume/rollback endpoint.
- Review restored sessions/login attempts and plans as sensitive stale state. Invalidate them using a tested site-approved database/session procedure or wait for the bounded session and plan lifetime in a write-disabled state; rotate upstream client credentials if exposure is suspected. Local logout is not upstream LDAP/Dex logout. Group revocation follows the provider and short (at most 15-minute) local authorization lifetime; do not promise immediate upstream revocation.
- Before upgrade, take and test a consistent backup, inspect dependency/security scan findings, pin the image digest and review config/schema changes. Recreate incurs downtime. Restoring the prior image alone is not safe if DB/config schema changed; test downgrade compatibility or restore a matching backup while reconciling newer Kubernetes changes manually. Never autoscale for availability.

## Incident boundaries and acceptance

Notebook stop/edit can lose kernels, processes, writable layers and unpersisted files; persistent volumes do not checkpoint memory. Expansion cannot be undone by shrinking, and restoring Notebook spec does not restore data. Never force-delete child Pods, delete data or remove finalizers as app recovery.

Local unit/fake-client tests and offline Kustomize checks do not establish live controller/culling, scheduling/GPU, admission, PVC binding, real resize, crash durability, OIDC claims/callback, TLS or CNI enforcement. Only the operator runs discovery/deployment and a disposable write pilot. Capture those results as a separate compatibility/acceptance record before production use.
