# Operations and recovery

## Safety and monitoring

Run exactly one application process against the DB. Keep `replicas: 1`, `Recreate`, no HPA. SQLite WAL requires local/block storage. BeeGFS instead uses DELETE journaling with `allowUnverifiedFilesystem: true` and verified client settings; see [database requirements](database.md) for global locks, remote fsync, `umask`-protected files and node fencing. The optional local PV ties the app to a prepared node; scheduling elsewhere cannot recover its files. Node/disk failure means downtime and potentially recovery from backup. Retain is not a backup or an HA mechanism.

Monitor Pod readiness/restarts, DB/PV/node free space and errors, pending/failed/needs-review operations, OIDC failures, Kubernetes permission errors, controller reconciliation and age of backups. `/healthz` checks process liveness; `/readyz` checks DB and required API discovery, not full RBAC, authentication or successful generated Pod admission. No `/metrics` endpoint is promised by this version. History APIs expose bounded recent windows, not a complete archival export. See [History retention](history.md) for the 90-day upper horizon, row/byte budgets, immutable safety ledger and idempotency tombstones. Export using an authorized, reviewed process; protect identity metadata and use platform Kubernetes audit logs for independent attribution. Expired or malformed preview-plan cleanup is separate from background history retention and never removes accepted claims or target locks.

The app's ServiceAccount is a powerful all-user reader, and enabling notebook/PV patches is broader still. No Secret/exec/impersonation privilege does not prevent indirect workload escalation. The optional Pod restart workflow additionally grants normal eviction, never arbitrary or forced deletion; keep it separately disabled unless reviewed. The optional Node cordon workflow grants cluster-wide `patch` on Nodes, which can affect scheduling for the whole cluster; keep it disabled unless reviewed and force-gated. The optional Profile deletion workflow grants cluster-scoped `delete` on `kubeflow.org/profiles` only; keep it disabled unless reviewed, and narrow it with `resourceNames` to the disposable pilot Profiles. Restricted storage YAML editing and unused-only storage deletion have independent feature flags and patch/delete RBAC grants; leave both disabled unless reviewed. An application's field/state checks do not constrain another privileged writer or a compromised service account: use admission and coordination too. Keep admission policy and gateway/network boundaries in place. Never add `cluster-admin` to fix readiness or a failed workflow.

### What the app can and cannot clean up

The app has **no arbitrary or forced cleanup workflow** and no HTTP `DELETE`
route or Kubernetes proxy. Separately gated `profile.delete` requests deletion
of one reviewed Profile and its normal namespace cascade; `pv.delete` and
`pvc.delete` request deletion of one approved, observed-unused storage object.
Each requires a reason, exact typed confirmation, reviewed plan and fresh
identity checks. Backend reclamation, removing a stuck namespace or finalizer,
and clearing an export directory remain operator-run procedures with their own
reviewed data/reclaim plan. See [storage management](storage.md#restricted-yaml-editing-and-unused-only-deletion).

Deletion details worth repeating during an incident:

- The delete uses **background propagation**, matching `kubectl delete`; the Profile and its owned namespace disappear asynchronously, and the console waits for **both** to be gone before reporting success. A same-named replacement object or a read error is **not** gone.
- The console issues **no** Namespace `DELETE`. The namespace is removed only as the Profile's owned child, by the installed Profile controller and Kubernetes garbage collection.
- The Profile deletion action never directly deletes PVs/PVCs. Namespace cleanup deletes PVCs: a bound PV's Delete reclaim policy can irreversibly destroy backing data, while Retain-policy PVs and external data may remain. Separate unused-only PV/PVC deletion requires explicit storage gates and RBAC; it never forces cleanup or removes finalizers. Retain is not protection against backend lifecycle or later operator deletion. Namespaced VolumeSnapshots disappear; their underlying content follows its own deletion policy. Profile deletion is not a verified data wipe.
- A namespace stuck in `Terminating` on a finalizer that never clears leaves the operation in `needs-review`. There is no force, no finalizer removal and no automatic retry; reconcile it as an operator.
- An unresolved deletion retains exclusive Profile/namespace locks and blocks other app mutations there. Success releases those locks; missing Profile/namespace scope still rejects child mutations. A later recreated workspace requires fresh observations and plans.

Profile ownership is immutable in the installed Profile controller, so `profile.update` changes quotas only; an owner transfer is refused as an unsupported contract and never applied as a broken write.

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

Notifications use a compact stack: one card is visible by default, with a count
button to expand/collapse the rest. Errors are first; within the error and
non-error groups, newest arrival is first. Repeated visible notices are
deduplicated and update recency. Info/success notices expire after ten seconds;
errors/warnings remain until dismissed. Only eligible inventory/policy dismissal
identifiers are remembered for the current session, never inventory or message
contents. An action error is not persistently suppressed by dismissing its toast.
Collapsed or dismissed notifications do not acknowledge preview warnings, prove
convergence or replace History and operator inspection.

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
- Incomplete operations recover as `needs-review`; no automatic replay. Fresh-read actual Profile/Notebook/PV/PVC UIDs and desired/observed state with independent authorized tooling. A notebook may be left stopped, a PV reserved, a PVC created/resizing, or a storage edit/deletion already applied. A changed PV reclaim policy may now allow later data deletion. Inspect the reviewed UID and accepted before/after changes, not a replacement under the same name. Preserve remaining resources; never shrink, retry blindly, remove finalizers or force cleanup. Storage edits/deletes retain their target locks; unresolved PVC deletion also fences its namespace. Acknowledging an incident does **not** release retained locks, so a fresh preview cannot bypass them. Only Profile deletion has the narrow offline reconciliation procedure below; there is no general resume, unlock or rollback endpoint.
- Review restored sessions/login attempts and plans as sensitive stale state. Invalidate them using a tested site-approved database/session procedure or wait for the bounded session and plan lifetime in a write-disabled state; rotate upstream client credentials if exposure is suspected. Local logout is not upstream LDAP/Dex logout. Group revocation follows the provider and short (at most 15-minute) local authorization lifetime; do not promise immediate upstream revocation.
- Before upgrade, take and test a consistent backup, inspect dependency/security scan findings, pin the image digest and review config/schema changes. Recreate incurs downtime. Restoring the prior image alone is not safe if DB/config schema changed; test downgrade compatibility or restore a matching backup while reconciling newer Kubernetes changes manually. Never autoscale for availability.

Do not downgrade while a Profile or storage mutation is unresolved unless
compatibility of its action, checkpoint and target/namespace fences has been
explicitly reviewed. Older binaries may not understand these safety contracts,
even if they can open the SQLite schema. Only Profile deletion has the narrow
reconciliation tool below; there is no corresponding generic storage unlock.
Never restore an old DB snapshot to discard a pending mutation or its locks.

### Offline reconciliation of a Profile deletion

This is **not** a cleanup command or an HTTP endpoint. It never writes to
Kubernetes, removes finalizers, retries a delete or blindly unlocks a target.
It can resolve only a `needs-review` **`profile.delete`** operation with complete
immutable acceptance evidence and both its Profile and namespace locks intact.

The command resolves only a `needs-review` operation. After a crash that left
an operation `queued` or `applying`, start the application **once** with
writes disabled: its startup recovery records the `needs-review` transition
and persists the checkpoint marker as false, which is the durable evidence the
command needs. Stop the application again, take and verify the stopped-directory
backup, and only then run the command against the **original** application DB.
Never coerce a `queued` or `applying` record into `needs-review` by hand, and
never edit the database to make the command accept it.

Any recovery flag given without the ID of one operation (`-reconcile-profile-delete`,
`-confirm-profile`, `-reason`, `-apply`) is refused before the configuration is
read, so the command can never fall back to starting the writable server. The
reconciliation mode is also refused together with `-check-db`/`-hold`.

1. Freeze app and external administrative changes. Record the operation ID,
   reviewed Profile/namespace identities and actual storage outcomes. Keep the
   namespace ownership/admission controls from [deployment section 7](deployment.md#7-profile-deletion--separate-opt-in-irreversible)
   in force. Do not recreate or reparent the workspace during reconciliation.
2. Stop the application and verify the process has exited. Take and verify a
   complete stopped-directory backup as above. Preserve it separately; run
   reconciliation against the **original application DB**, not a backup copy.
3. Use the reviewed application binary and config, its original DB directory,
   and an explicitly selected read-only kubeconfig for the same cluster. In a
   maintenance container use the site's approved volume mounts, service
   identity and node placement. No second server process may own that DB.
4. Run the default **dry run**, substituting reviewed values:

   ```sh
   kubeflow-admin -config /secure/site/config.yaml \
     -reconcile-profile-delete OPERATION_ID \
     -confirm-profile EXACT_PROFILE_NAME \
     -reason 'Explain the verified outcome and reference the incident record'
   ```

   The tool reads only the Profile, same-name namespace and `kube-system`
   namespace. It compares the cluster UID and object UIDs with the immutable
   accepted record, and takes two observations. Read failures, replacement
   objects, another cluster or missing evidence cause refusal. A running app
   holding the DB lock causes exit code 3. A dry run does not release locks.
5. Review the reported, **derived** outcome. The only supported outcomes are:
   - `reconciled-succeeded`: both Profile and namespace are absent in both
     complete observations. This does not prove backend data was wiped.
   - `reconciled-cancelled`: the durable, versioned checkpoint marker proves
     no Profile DELETE request could have started. An apparently active
     namespace alone never proves this. If a request may have started and
     cleanup remains unfinished, the command refuses; there is no force option.
6. Only after reviewing that decision, rerun the same command with **`-apply`**.
   It takes fresh observations, records a distinct offline reconciliation audit
   entry with the reason, and atomically releases the operation's locks with
   its terminal outcome. Review the record, then restart exactly one app
   instance with writes disabled initially. Any later action needs a new plan.

Expired preview-plan cleanup does not remove the accepted identities needed by
this command. If the tool refuses, retain the DB and locks and investigate;
never hand-edit SQLite records or delete lock rows to force completion.

## Incident boundaries and acceptance

Notebook stop/edit can lose kernels, processes, writable layers and unpersisted files; persistent volumes do not checkpoint memory. Expansion cannot be undone by shrinking, and restoring Notebook spec does not restore data. Never force-delete child Pods, delete data or remove finalizers as app recovery.

Profile deletion is irreversible and is not an undo path for anything else. It cannot restore a workspace or provide a verified backend wipe. Treat a `needs-review` deletion as an open incident: inspect the real Profile and namespace state with independent tooling, confirm what the controller and garbage collection actually did, and reconcile storage outcomes with the storage operator. Do not force a stuck namespace through the app or use deletion to tidy a failed pilot. Notebook edits, storage changes and Pod restarts can also cause irreversible loss of volatile state; deletion is not the only data-loss risk.

Local unit/fake-client tests and offline Kustomize checks do not establish live controller/culling, scheduling/GPU, admission, PVC binding, real resize, crash durability, OIDC claims/callback, TLS or CNI enforcement. Only the operator runs discovery/deployment and a disposable write pilot. Capture those results as a separate compatibility/acceptance record before production use.
