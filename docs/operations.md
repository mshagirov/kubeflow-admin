# Operations and recovery

## Safety and monitoring

Run one application process per database: one replica, `Recreate`, no HPA. Follow [database requirements](database.md) for filesystem, journal mode, permissions and node fencing. Local-PV node loss means downtime and potentially restoration from backup.

Monitor readiness/restarts, database and node free space, failed/`needs-review` operations, OIDC/API permission failures, controller convergence and backup age.

| Signal | Meaning |
| --- | --- |
| `/healthz` | Process liveness |
| `/readyz` | Database and required API discovery; not full RBAC, OIDC or workload admission validation |
| Inventory | Timestamped, non-atomic observations; not mutation preconditions |
| Accepted operation | Durable acceptance, not completion |
| `succeeded` | Workflow-specific convergence passed; not a guarantee of future availability or data recovery |
| `needs-review` | Outcome is uncertain/unfinished; inspect actual objects and retain locks |

There is no metrics endpoint or full-history export. [History](history.md) describes retention, capacity limits and immutable evidence; [logging](logging.md) covers diagnostic output.

### What the app can and cannot clean up

Typed Profile deletion cascades through its owned namespace. PV/PVC deletion is separately enabled and unused-only. PV reclaim removes an eligible stale claimRef without wiping data. All use reviewed identities; none provides arbitrary deletion, forced cleanup or finalizer removal. See [deployment](deployment.md#7-profile-deletion-separate-opt-in-irreversible) and [storage](storage.md).

Notebook stop/edit/restart can lose kernels, processes, writable layers and unsaved files. Persistent storage does not checkpoint memory. Expansion cannot be undone by shrinking. Profile/PVC deletion may destroy backing data according to reclaim policy.

### Inventory latency and freshness

Inventory bulk-lists Pods/StatefulSets and derives Notebook state by namespace/controller UID. Navigation reuses the in-memory snapshot; inventory pages refresh after 30 seconds and visible pages poll. Manual Refresh fetches again. Fetch failures retain old observations with a stale warning. History loads independently.

Inventory is not persisted in browser storage. Only eligible notification-dismissal identifiers are remembered for the session. See [UI usage](ui.md). Static assets revalidate with ETags; reload after upgrades. For latency reports, capture request durations/statuses rather than HAR files, cookies or response bodies.

## Backup (no live-copy shortcut)

Use the complete [backup and restore procedure](backup.md). Freeze new work, account for accepted operations, stop the app and verify the process has exited. Back up the entire stopped directory, including remaining SQLite journals, with permissions/ownership and release/configuration metadata. Encrypt and retain an off-node copy; verify an isolated restore with SQLite integrity checks and a disposable application recovery drill.

Never copy only a live main database or discard journals. The distroless image contains no backup endpoint or SQLite shell. Back up non-secret configuration separately and recover credentials through the secret manager.

## Recovery and upgrades

1. **Record real cluster state.** Disable new writes and, where practical, revoke optional mutation grants. A restored database is older than Kubernetes and cannot undo newer changes.
2. **Stop and preserve.** Keep a forensic copy of the current database directory. Restore a complete, matching snapshot onto compatible storage with runtime UID/GID 65532 and private permissions. Do not mix snapshots or run two owners.
3. **Validate compatibility.** Verify SQLite integrity, application/schema/config compatibility and storage locking. A local-PV node replacement needs an explicit migration/rebinding plan; do not edit bound immutable fields or blindly repoint paths.
4. **Start read-only.** Startup converts incomplete work to `needs-review` without replay. It refuses inconsistent claims, operations, acceptance evidence or target locks; it never fabricates missing evidence or clears locks to repair corruption.
5. **Reconcile exact identities.** Inspect the accepted object UIDs, checkpoints and mutation receipts against current objects. A Notebook may be stopped, a PV reserved, or a PVC created/resizing/deleted. Empty receipts do not prove nothing changed. Never retry an ambiguous mutation, delete lock rows or restore an old snapshot merely to discard unresolved work.
6. **Handle restored authentication state.** Sessions/plans are sensitive stale state. Use a tested invalidation procedure or wait out their bounded lifetimes with writes disabled; rotate upstream credentials if exposed. Local sessions last at most 15 minutes; upstream group revocation is not immediate.

Before upgrades, test a consistent backup, review config/schema changes and scan the exact image. Configuration loads only at startup. Recreate causes downtime. Image-only rollback is unsafe when configuration, schema or operation contracts differ; use a compatible restore and independently reconcile cluster changes. Do not downgrade unresolved Profile/storage operations without verifying their checkpoint and lock contracts.

Only Profile deletion has an offline resolution tool. Other uncertain operations have no generic resume, unlock or rollback endpoint; preserve evidence and investigate through the site's recovery process.

### Offline reconciliation of a Profile deletion

The tool resolves only a `needs-review` `profile.delete` with complete immutable evidence and both Profile/namespace locks. It reads Kubernetes but never retries deletion, changes resources or removes finalizers.

If a crash left work `queued` or `applying`, start the app once with writes disabled to record startup recovery, then stop it again. Recovery preserves the existing pre-delete checkpoint marker; it does not turn a possibly issued deletion into proof of no write.

1. Freeze app and external administrative changes; keep [namespace ownership controls](deployment.md#7-profile-deletion-separate-opt-in-irreversible) in force. Record the operation ID, reviewed identities and observed storage outcomes.
2. Stop the app, verify exit and take a complete backup. Run the tool against the **original application database**, not the backup copy.
3. Use the compatible application binary/config and an explicitly selected read-only kubeconfig for the original cluster. Maintenance containers need the original volume, correct identity and node placement.
4. Run the default dry run:

   ```sh
   kubeflow-admin -config /secure/site/config.yaml \
     -reconcile-profile-delete OPERATION_ID \
     -confirm-profile EXACT_PROFILE_NAME \
     -reason 'Verified outcome and incident reference'
   ```

   It reads the Profile, same-name namespace and `kube-system` twice, comparing cluster/object UIDs with accepted evidence. Missing evidence, replacement objects, read failures or another cluster cause refusal. A database owned by the running app causes exit code 3. Dry run changes no operation outcome or locks.
5. Review the derived outcome:
   - `reconciled-succeeded`: both objects absent in both complete observations; this does not prove a backend wipe.
   - `reconciled-cancelled`: versioned durable evidence proves no delete request started. Present objects alone cannot prove that. A possibly issued, unfinished deletion cannot be cancelled.
6. Rerun the same command with `-apply`. It takes fresh observations and atomically records the terminal outcome, operator reason/audit entry and lock release. Restart one app instance with writes disabled and review the result.

Expired preview cleanup does not remove accepted reconciliation identities. Recovery flags without an operation ID, or combined with `-check-db`/`-hold`, are refused. If reconciliation fails, preserve the database and locks; do not alter records to force acceptance.

## Incident boundaries and acceptance

A failed pilot needs an operator cleanup plan, not forced deletion through the app. Inspect Profile namespace cleanup, retained PVs, snapshots and backend data independently. Disable feature flags and revoke exact optional RBAC bindings separately after the pilot; neither step rolls back completed changes.

Use [live acceptance](ui-workflow-acceptance.md) before production writes. Fake clients and local browser tests cannot establish controller/culling behavior, admission, scheduling, storage mounts/resize, crash durability, OIDC integration, TLS or CNI enforcement.
