# Kubeflow Admin — final consolidated implementation plan

**Date:** 15 October 2026. **Delivery:** buildable application, container recipe, Kubernetes deployment packaging, tests, and operator instructions. The user deploys and performs live acceptance tests. No cluster credentials are requested or used by the implementation agents.

This file consolidates and supersedes the earlier draft plans (v1–v3). It is a delivery contract, not evidence that live acceptance tests have passed. Build/test evidence and remaining site configuration are in [docs/verification.md](docs/verification.md).

**Delivery status (15 October 2026):** implemented and built. Agents used OpenCode subagent sessions on `openai/gpt-6.1-sol` at default thinking level, as requested. All seven work packages in section 5 are complete: the application compiles, all tests pass (including `-race`), `govulncheck` is clean, the Linux amd64 binary and container image build, and all manifest variants render. The operator still performs deployment and live acceptance; nothing was applied to the cluster.

## 1. Review and resolved conflicts

| Earlier proposal | Final decision |
|---|---|
| Trust `X-Auth-Request-Email` or `kubeflow-userid` | Direct OIDC verification with a dedicated Dex client. Ignore identity headers. No insecure development login or automatic proxy fallback. |
| `kubeflow.org/notebook-stopped: true` in v1 | Incorrect controller contract. Use `kubeflow-resource-stopped` with RFC3339 timestamp to stop; remove the key to start. Enable only after controller contract confirmation. |
| Strategic merge patch for Notebook CRDs; replace container index 0 | UID/resourceVersion conditional JSON patches; select containers by name and retain sidecars, unknown fields, env, probes, other resources and volumes. |
| CRUD/delete/owner transfer and broad namespace/Pod writes | No deletion, owner transfer, raw YAML, exec, arbitrary mounts, RBAC writes, Secret reads, impersonation, or direct StatefulSet scaling. |
| Stateless app or optional audit | Kubernetes remains authoritative; SQLite persists login/session state, previews, operations and audit. Database failure blocks writes. |
| Browser stores ID/refresh tokens | Opaque host-only Secure/HttpOnly session cookie; only hashed session identifiers persisted. No raw access/ID/refresh tokens stored. |
| CDN frontend | Embedded local HTML/CSS/JavaScript. A small native script replaces HTMX where sufficient; no frontend dependency/build pipeline or remote assets. |
| Generic inferred API/version/storage support | Use confirmed site inventory, discovery and explicit approved templates; unsupported capabilities fail closed. |
| General PostgreSQL/HA and multiple packaging formats | One Go binary, one replica, Kustomize; node-local SQLite filesystem in this cluster. PostgreSQL/HA is future work, not a claimed implementation. |

The most important v3 safeguards are retained. Complexity that does not protect the first release (bulk actions, metrics integrations, directory lookup, general controller adapters, automatic rollback) stays out of scope. Differences between this implementable release and v3's broader proposed future scope must be explicit in the verification record.

## 2. Discovery inputs and unresolved deployment decisions

A read-only inventory of the target cluster is required before enabling writes. Record it locally; do not commit site-specific values into this repository. The relevant categories are:

* Kubernetes/RKE2 version and architecture; served Profile/Notebook API versions and installed controller digests (do not infer controller versions from the platform version).
* Identity provider (Dex/OIDC) issuer, gateway/ingress integration point, and the Kubeflow user identifier used for Profile owners.
* StorageClasses in use, their backend, reclaim policy, binding mode, access modes and real expansion support; any existing static PVs.
* GPU resource keys and the node selectors/taints/tolerations/RuntimeClasses a valid scheduling preset requires.
* Whether notebooks are idle-culled, and the installed admission policy.
* Suitable local/block storage (WAL), or verified BeeGFS with DELETE journaling and the explicit opt-in, for the SQLite database.

**SQLite placement:** WAL requires local/block storage. For BeeGFS, the reported default StorageClass, the base PVC omits `storageClassName` and the site sets DELETE journaling plus `allowUnverifiedFilesystem: true` after verifying client locking/fsync. See [database requirements](docs/database.md). The node-local PV remains an option. Do not install a new CSI driver.

Still configured by the operator: public Dex issuer, exact admin group or subject allowlist, dedicated HTTPS admin hostname, TLS/routing/auth-policy integration, registry/image credentials, selected database node/path, approved user classes, storage backend import fixtures, culling behavior and GPU scheduling presets. These are configuration/acceptance gates, not reasons to leave application code unfinished. No guessed identity mapping, guessed CSI volume-handle format, or unrestricted PV source is enabled.

## 3. Release scope

1. Admin login against existing LDAP-backed Dex with code flow, PKCE, state and nonce; validated token issuer/audience/signature/expiry and exact group/subject authorization.
2. All-user inventory of existing Profiles, Notebooks, PV/PVC bindings, StorageClasses and diagnostic workload state; no migration or relabeling.
3. Profile creation with exact Kubeflow owner identity; approved quota editing through the Profile CR. Reject reserved names and existing unrelated namespaces. Profile creation does not create/check LDAP accounts.
4. Approved StorageClass PVC provisioning; existing eligible PV reservation/claim; static NFS and fixed approved CSI import templates; upward-only optional expansion. Static registration does not prepare storage or permissions.
5. Notebook start/stop, and controlled named-container CPU/memory/GPU/image/PVC-mount edits. Preserve unrelated spec fields, require safe preset scheduling, protect home mounts, stop before spec changes, and restore running state only if confirmed.
6. Server-owned expiring preview → explicit confirmation → bounded asynchronous operation → observed result. Persist action attribution, checkpoints, idempotency and audit. Interrupted operations become `needs-review`, never blindly replayed.
7. Embedded usable UI, same-origin JSON API and OpenAPI contract. No user Kubernetes credentials reach the browser.

Not included: deletion or finalizer removal; new notebook creation; LDAP account/group edits; ownership/contributor changes; arbitrary YAML/env/ServiceAccount/security-context edits; process state recovery; file browsing; GPU utilization metrics; high availability; automatic spec/data rollback; unreviewed external storage imports.

## 4. Architecture and security contract

`Browser → existing HTTPS Istio gateway → Go application → Kubernetes API/controllers`

The app also talks to Dex and its local SQLite database. It uses Go stdlib HTTP/templates, typed and dynamic `client-go`, established OIDC/OAuth2 libraries and embedded assets. Kubernetes dependency modules are pinned consistently to the discovered 1.32 line. One sequential operation worker limits write concurrency; HTTP requests never own long-lived watches.

* Default deployment is read-only. Write RBAC and feature switches are separately explicit.
* Every mutation requires a current authorized admin, CSRF token, same origin, unexpired actor-bound server plan, idempotency key, scope policy, fresh target/dependencies and API dry-run where applicable.
* Hard-deny system/application namespaces even if request paths are forged. Other notebook/PVC mutations require an active matching Profile/namespace. Include existing CLI-created user objects.
* Conditional UID/resourceVersion checks prevent stale patches. Application locks do not stop `kubectl`, GitOps or controller races. A conflict requires a new preview, not silent overwrite.
* Notebook API acceptance does not prove Pod admission or readiness. Wait for actual controller-owned StatefulSet/Pod observations; child mapping uses owner UIDs. Surface pending/error/stale versus desired state.
* Mounts use same-namespace filesystem PVCs; removal never deletes claims. GPU counts use approved extended-resource keys with valid integer request/limit relationships. No capacity promise based on free-resource estimates.
* Static volumes use Retain and intended claim reservation; reject Bound/Released/reserved/terminating or incompatible PVs and duplicate configured backend handles. No generic hostPath UI.
* Dedicated ServiceAccount gets exact read verbs plus only enabled mutation resources. No `delete`, wildcard, Secret, impersonation, RBAC-write, exec or StatefulSet-write privileges.
* Kubernetes RBAC does not constrain patched fields or Profile namespaces. Operator-enforced admission restrictions remain necessary against stolen ServiceAccount credentials. App validation is not an admission boundary.
* Audit/checkpoints must commit before writes. Never store whole notebook manifests, secret/env values or tokens. No automatic compensating storage deletion or replay on crash/database restore.
* Secure host-only cookies and <=15-minute sessions bound claim age. Expired admin authorization blocks subsequent write steps. Local logout does not revoke upstream SSO. Protect TLS/private CA trust; never disable verification.

## 5. Agent work packages and integration order

All agents use OpenCode subagent sessions with discovered `openai/gpt-6.1-sol`, default thinking level (no variant override). It is available, so the requested fallback is unnecessary. Shared interfaces: `docs/agent-contract.md`.

| Step / owner | File boundary | Deliverables and gates |
|---|---|---|
| 1 — Lead: plan and interfaces | final plan, `internal/config`, `internal/model` | Resolve plan contradictions; strict typed configuration and action/view/plan/operation models; record discovery. |
| 2A — Kubernetes agent | `internal/kube` | Typed/dynamic clients, read inventory, namespace policy, validated/dry-run previews, conditional writes, static/dynamic storage and notebook workflows, convergence and preservation tests. |
| 2B — Authentication agent | `internal/auth` | Direct OIDC, single-use browser-bound login, durable hashed sessions, admin authorization, same-origin/CSRF, security tests. |
| 2C — Web agent | `internal/web` | Embedded dashboard and typed forms, preview/confirmation, asynchronous progress, safe JSON API, status/errors, route/security tests. |
| 2D — Packaging agent | `deploy`, `scripts`, deployment/runbooks, Dockerfile/Makefile/CI, `api` | Reduced RBAC base and opt-in write/static/expand components, persistent hardened Pod, existing Istio routing, discovery script and concise operator guide. |
| 3 — Lead: durability/runtime | `internal/store`, `internal/operations`, `cmd` | SQLite transactions and operation/idempotency locks, pre-write checkpoints/audit, conservative restart recovery, graceful shutdown, strict startup. |
| 4 — Lead and review agents | integration/tests/docs | Resolve interface/config/OpenAPI drift; build Linux amd64 executable; tests, vet, race checks where supported; manifest rendering and security review. Fix failures before handoff. |
| 5 — User: deployment and live acceptance | user cluster | Configure OIDC/hostname/local storage/templates; build/push image and apply read-only deployment; then disposable pilot and enable reviewed writes. |

Steps 2A–2D run in parallel after step 1. Step 3 proceeds concurrently under stable shared interfaces. No agent is authorized to deploy to the cluster, collect Secrets, or overwrite original plans.

## 6. Local delivery gates

* Application compiles; tests and `go vet` pass. Produce a Linux amd64 binary/checksum and reproducible multi-stage Docker build instructions. An image build is only claimed if a working Docker daemon permits it.
* Tests exercise unauthorized/CSRF requests; nonce/state/token validation; namespace and field policy; correct stop annotation; named-container preservation; storage validation and races; conditional patches; idempotency, durable locking and crash recovery.
* Every Kustomize mode renders. RBAC has no excluded privileges. Config examples load under the actual strict schema once documented site placeholders are replaced.
* UI/API route, model, backend action names, feature gates, RBAC, and documentation agree. Fail unsupported modes closed rather than expose nonfunctional buttons.
* Final verification record separates locally proven behavior from live controller/CSI/GPU/Istio acceptance. No synthetic/fake-client test is called an end-to-end cluster pass.

## 7. User deployment and live acceptance

Follow `docs/deployment.md` (README provides quick start): set exact Dex client/callback/admin authorization and dedicated TLS hostname; prepare database local PV; configure image and approved storage; deploy read-only base; verify inventory and denial of a normal user; grant only required write components and enable one disposable namespace; then test create Profile/PVC, start/stop, CPU/GPU/mount edit and data persistence. Check culling-resume, CLI conflict, app restart and partial storage recovery. Only then expand scope.

Back up SQLite consistently or while the single app is stopped (include WAL if using filesystem-level procedures as documented). Preserve existing user PV/PVC data. On unsafe behavior disable new writes/revoke mutation RBAC; inspect in-flight state before stopping the app. A failed edit may leave a notebook stopped. Rollback is a reviewed new plan/manual operator procedure, not deletion or blind replay.

## 8. Reference authority

The original v3 source list remains useful background, but actual supplied cluster inventory governs compatibility. Key official references: [Kubeflow notebook API](https://www.kubeflow.org/docs/components/notebooks/api-reference/notebook-v1/), [Kubeflow Profiles](https://www.kubeflow.org/docs/components/central-dash/profiles/), [Dex OIDC clients](https://dexidp.io/docs/guides/using-dex/), [Kubernetes API concurrency/dry-run](https://kubernetes.io/docs/reference/using-api/api-concepts/), [persistent volumes](https://kubernetes.io/docs/concepts/storage/persistent-volumes/), [GPU resources](https://kubernetes.io/docs/tasks/manage-gpus/scheduling-gpus/), [RBAC good practices](https://kubernetes.io/docs/concepts/security/rbac-good-practices/), [SQLite WAL filesystem constraint](https://sqlite.org/wal.html). These links are reference material, not a claim of live testing or renewed source verification.
