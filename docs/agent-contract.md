# Implementation boundaries

The module is `kubeflow-admin`, using Go and the standard `net/http` router. See [go.mod](../go.mod) for the language minimum and [verification](verification.md) for the supported build/check procedure. Function signatures and schema definitions in source are authoritative.

## Package responsibilities

| Path | Responsibility |
| --- | --- |
| `internal/config`, `internal/model` | Strict configuration, shared typed actions, plans and operation records |
| `internal/kube` | Inventory, policy validation, dry runs, fresh evidence checks, conditional mutations and observed convergence |
| `internal/auth` | OIDC, short-lived sessions, local authorization, same-origin and CSRF checks |
| `internal/store` | SQLite ownership, transactional acceptance/checkpoints/outcomes, locks, immutable evidence and retention |
| `internal/operations` | Server-owned previews, idempotent submission, sequential worker, checkpoint authorization and recovery |
| `internal/web` | Routes, embedded UI/assets, safe projections and API responses |
| `cmd/kubeflow-admin` | Startup, shutdown, database ownership check and offline Profile reconciliation |
| `deploy`, `scripts`, `api` | Minimal RBAC, optional grants, offline checks and OpenAPI contract |

## Authentication and execution

- Authenticate protected routes; ignore forwarded identity headers. Unsafe methods require same-origin and CSRF validation. Sessions last at most 15 minutes; do not store raw access tokens.
- `Identity.SessionID` is a server-only hashed key, excluded from JSON and durable plans/operations/audit. Workers revalidate the persisted session and current local authorization before execution and every write checkpoint.
- Previews are server-owned, actor-bound and expire after five minutes. Dry runs do not prove generated-Pod admission or controller/storage convergence.
- Revalidate exact object identities and relevant evidence before each conditional write. Persist checkpoints before writes and mutation receipts before subsequent writes; wait for workflow-specific convergence.
- Propagate errors with safe messages. Never log credentials, full manifests, backend paths/handles or sensitive diffs. Do not provide a development authentication bypass, arbitrary Kubernetes proxy or unrestricted manifests/deletion.

## Persistence and recovery

One lifetime process file lock, one manager lease and one sequential worker own a database. [Database requirements](database.md) define supported filesystems, journal modes and private permissions.

Acceptance atomically records the plan/key claim, operation, audit evidence and derived target/dependency locks. `Targets` and storage dependency validation in `internal/store` are authoritative: operations can lock related PVs, PVCs, PodDefaults and Notebook dependencies, not only the primary resource. Profile deletion and PVC deletion take an exclusive namespace fence against other app mutations. These locks do not constrain external Kubernetes writers.

Startup converts incomplete work to `needs-review` without replay and retains locks. Integrity failures refuse startup rather than fabricating evidence or unlocking. Shutdown accounts durably for accepted queued work. Missing mutation receipts do not prove no write occurred. [History retention](history.md) distinguishes prunable presentation records from retained acceptance evidence and idempotency tombstones.

Only [offline Profile-deletion reconciliation](operations.md#offline-reconciliation-of-a-profile-deletion) can resolve its supported uncertain outcome. It defaults to dry-run, requires a stopped app and immutable cluster/object evidence, and performs Kubernetes reads only. There is no generic unlock, replay or HTTP recovery endpoint.

## Mutation contracts

- **Profiles:** updates change quotas only, with unchanged owner and identity-bound `baseConfigDigest`. Deletion requires its separate flag, reason and exact raw confirmation; conditionally delete the reviewed Profile and observe it and its owned namespace disappear. Never directly delete Namespaces or remove finalizers.
- **Storage:** restricted YAML permits console labels, PV reclaim policy and gated upward-only PVC growth. Unused-only deletion requires exact identity and confirmation. Reclaim only clears an eligible stale claimRef under separate isolation/permission gates; it never wipes backend data. PodDefaults exist only as reviewed PVC substeps. Preserve partial outcomes without rollback or replay. See [storage contracts](storage.md).
- **Notebooks/Pods/Nodes:** honor the verified Notebook controller contract, protected mounts, approved images/GPU presets and quota checks. Pod restart uses guarded eviction, never forced deletion. Node cordon changes scheduling only. See [compatibility](compatibility.md), [Pods](pods.md) and [Nodes](nodes.md).

Keep [OpenAPI](../api/openapi.yaml), configuration, UI and deployment grants aligned. Use focused local tests and the release checks; fake clients and browser fixtures cannot prove live controller, admission, storage or network behavior.
