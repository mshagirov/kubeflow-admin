# Learning plan — reimplement, test and deploy Kubeflow Admin by hand

This plan breaks the project into ordered lessons. Each lesson is written so you
can implement the behaviour yourself, test it, and verify it by hand before
moving on. This repository's working code is the reference solution:
implement first, then compare.

## How to use this plan

- Take one lesson at a time; finish its **Done when** checks before continuing.
- After every lesson, run the regression gate:
  ```sh
  make test vet build && make manifests
  ```
- Keep every experiment disposable: scratch cluster, temporary namespaces,
  throwaway volumes. Never point a learning environment at production.
- Start read-only. The app ships with all feature flags off; keep them off
  until the deployment lessons are complete.
- Replace every `REPLACE_*` and `*.example.invalid` placeholder. Never commit
  real hostnames, IPs, storage class names, user identities, certificates or
  secrets. Keep a personal inventory file **locally** (it is git-ignored) —
  the public repository stays generic.
- When a lesson says "verify by hand", actually do it: observe the failure
  case, not only the success case.

## Requirements (lesson 0)

### Software — required

| Tool | Purpose |
| --- | --- |
| Go 1.26+ (the module minimum; the container pins a newer patch) | build and test |
| Kustomize 5 | render manifests |
| Python 3 + PyYAML (`scripts/requirements.txt` works) | manifest checker |
| Docker with buildx | image build; also runs the local OIDC provider and scanners |
| `kubectl` | cluster lessons |
| Git, `curl`, `openssl`, a web browser | version control, by-hand verification, local certificates |

### Software — recommended

- `gh` and a GitHub account (only for lesson 15's CI)
- Trivy (the container image is fine) and `govulncheck`
  (`go run golang.org/x/vuln/cmd/govulncheck@latest ./...`) for scans
- `sqlite3` CLI to inspect databases by hand
- kind or minikube for the scratch cluster (lessons 6–15)
- A local Dex container as the OIDC provider for lessons 8 and 18

### Hardware

| Environment | Minimum | Notes |
| --- | --- | --- |
| Developer machine | 4 cores, 8 GB RAM, ~5 GB free disk | 16 GB comfortable with Docker Desktop plus a local cluster; macOS, Linux or Windows + WSL2 |
| Scratch cluster | 1 node, 2 vCPU, 4 GB RAM, ~10 GB disk | enough for the app and a disposable notebook |
| Two-node cluster | 2 nodes | only lesson 17's cross-node lock test; the local-storage option avoids it |

No GPU is required anywhere in the plan; GPU handling is configured and
validated, never exercised. Multi-architecture images build through emulation —
no special hardware.

### Deployed footprint (what the cluster must host)

- 1 replica: requests `100m` CPU / `128Mi` memory, limits `1` CPU / `512Mi`
- a 5 Gi ReadWriteOnce volume for SQLite
- image ~72 MB compressed; egress to the OIDC issuer and the Kubernetes API

### Not required

A real identity provider, paid services, managed databases, a GPU, or an
internet-facing cluster: a local OIDC provider, a locally trusted certificate
and a hosts-file entry are enough for every lesson.

Read `README.md`, `docs/deployment.md`, `docs/database.md` and `api/openapi.yaml`
once now. You will re-derive most of their content in the lessons.

---

# Part 1 — Foundations

## Lesson 1 — Module skeleton and strict configuration

**Goal:** a compiling module named `kubeflow-admin` with a configuration loader
that refuses to run on ambiguous or unsafe input.

**Implement**
- `go mod init kubeflow-admin`; directories `cmd/kubeflow-admin`, `internal/...`.
- `internal/config`: struct for server, auth, kubernetes, policy, storage,
  notebooks, database and features; YAML strict decoding (unknown fields are
  errors); defaults (listen port, timeouts, session length); `Validate()` that
  rejects HTTP origins, URL paths, placeholder admin groups and out-of-range
  session lifetimes.
- Refuse `:memory:`, `file:` URIs and empty database paths.

**Write tests:** defaults applied; unknown field rejected; multiple YAML
documents rejected; every unsafe value in `Validate()` rejected.

**Verify by hand:** feed the loader a deliberately wrong file (unknown key,
`http://` origin, 60-minute session) and confirm each one aborts with a clear
message before any network or file work happens.

**Done when:** `make test vet` passes and `cmd/kubeflow-admin` can load a
minimal valid config file and exit, printing a validation error for a bad one.

## Lesson 2 — Build discipline

**Goal:** one-command repeatable build and test.

**Implement:** a `Makefile` with `test` (race detector), `vet`, `build` (static,
`CGO_ENABLED=0`, `-trimpath`, no VCS stamping), `manifests` and `check` targets.
Add an `.editorconfig`-style formatting step (`gofmt -l`) to your routine.

**Verify by hand:** builds succeed from a clean checkout with no network
(module cache warm); the binary runs on linux/amd64 —
`CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/kubeflow-admin`.

**Done when:** `make check` is green and you understand every flag in the build.

---

# Part 2 — Durable state

## Lesson 3 — SQLite record store

**Goal:** a small key/value store on SQLite with private files and durable
writes.

**Implement**
- Open the database with `modernc.org/sqlite` (pure Go).
- Create the directory `0700`; create/verify the database and lock files as
  private regular files (`0600`, `O_NOFOLLOW`).
- Table `records(bucket,key,value,updated)`; `Put`, `Get`, `Take` (single-use
  read that deletes atomically), plus operation/audit support tables you will
  need later.
- Apply pragmas to **every** connection via the connection string
  (`busy_timeout`, `synchronous`, `foreign_keys`); use immediate transactions.

**Write tests:** data survives close/reopen; `Take` succeeds exactly once;
concurrent claims hit a unique constraint instead of corrupting state.

**Verify by hand:** after a run, `ls -l` the directory: directory `0700`,
files `0600`. `sqlite3` the file and confirm `PRAGMA journal_mode` and
`PRAGMA integrity_check` answer as expected.

**Done when:** tests pass and the failure modes you tried (bad path, existing
directory with world permissions) stop the app with a clear message.

## Lesson 4 — One writer, guaranteed

**Goal:** two processes must never own the database.

**Implement:** a non-blocking exclusive `flock` on `<db>.lock` held for the
lifetime of the process; a typed sentinel error (e.g. `ErrLocked`); release on
idempotent `Close`.

**Write tests:** second open in the same process fails; a child process (test
binary re-exec) fails; after `Close`, a new open succeeds.

**Verify by hand:** start the app twice against the same database directory;
the second must exit immediately with the ownership error. Kill the first with
`kill -9`; the next start must succeed.

**Done when:** you can explain why an advisory lock written by the application
is still not enough on a shared filesystem (lesson 17 returns to this).

## Lesson 5 — Journal modes and the filesystem policy

**Goal:** WAL by default on local disks; controlled support for one reviewed
network filesystem; everything else fails closed.

**Implement**
- `database.journalMode`: `wal` (default) or `delete`; reject anything else.
- `database.allowUnverifiedFilesystem` (default false): an explicit operator
  acknowledgement required for storage the app cannot prove local.
- Filesystem check by `statfs` magic: accept known local filesystems; always
  reject NFS/SMB/CIFS/FUSE/tmpfs/ramfs/Lustre/CephFS/GPFS/OCFS2; for the
  configured network filesystem (BeeGFS) require `delete` **and** the opt-in;
  unknown filesystems require the opt-in too.
- `synchronous=FULL` for WAL, `synchronous=EXTRA` for DELETE; `umask 0077` at
  startup so journal/`-shm` files are private.
- Log a structured warning whenever the database runs on unverified storage.

**Write tests:** policy table tests (every magic, both journals, both opt-in
values); migration between journal modes preserves data; pragmas survive
connection replacement; a `-check-db` style mode that opens the store, prints
journal mode, and exits 0/3 for owner/stolen states.

**Verify by hand:** run against a temp directory and against a mounted network
path if you have one; observe the exact refusal messages.

**Done when:** the policy table is fully covered by tests and the app refuses
network storage unless the operator opted in.

---

# Part 3 — The Kubernetes read model

## Lesson 6 — Typed inventory

**Goal:** read-only views of the cluster with a minimal permission surface.

**Implement**
- Kubernetes client from in-cluster config (fallback: an explicit kubeconfig
  path); request timeouts; no exec, no proxy, no secret reads.
- Inventory: namespaces, profiles, notebooks, persistent volumes and claims,
  storage classes, resource quotas, limit ranges, pods, events, stateful sets,
  role bindings — `get`/`list` only.
- Redact or omit anything not needed by the UI (no container environment, no
  secret references).

**Write tests:** fake clientset — scope rules (a user may only see objects in
their profile namespaces), pagination/timeout behaviour, error mapping.

**Verify by hand:** against your scratch cluster, compare each inventory page
with `kubectl get` output; they must agree.

**Done when:** the app renders an inventory with a read-only ServiceAccount
and no additional verbs.

## Lesson 7 — Plans, validation and conditional patches

**Goal:** every mutation is a typed, validated plan; execution is conditional.

**Implement**
- Plan model: actor, target, action, parameters, expiry, ID.
- Validation per action: allowed images, GPU presets, workspace-mount
  protection, storage class allowlist, namespace allowlist, path boundaries.
- Execution: JSON patch carrying `metadata.uid`/`metadata.resourceVersion`
  preconditions, so stale plans fail instead of overwriting new state.
- Never generate raw YAML for users; never delete resources; never write to
  workloads directly.

**Write tests:** container/volume preservation when editing notebooks; rejected
mutations (protected mounts, disallowed images, forbidden namespaces); stale
`resourceVersion` produces a clean conflict.

**Verify by hand:** prepare a plan, change the object with `kubectl`, submit the
plan; it must fail with a conflict and change nothing.

**Done when:** every write path in your code goes through plan → validate →
conditional patch.

---

# Part 4 — Security core

## Lesson 8 — OIDC login with PKCE

**Goal:** authorization-code flow with PKCE and single-use browser binding.

**Implement:** discovery + JWKS; `state`, `nonce`, PKCE S256; the attempt is
stored server-side keyed by a hash of `state` and consumed exactly once; the
callback verifies issuer, signature, audience (and `azp` for multi-audience
tokens), nonce, expiry and `at_hash` when present. Secure cookies:
`__Host-` prefix where possible, `Secure`, `HttpOnly`, `SameSite=Lax`.

**Write tests:** a fake TLS issuer; wrong signature/audience/expiry/nonce/
issuer rejected; replayed `state` rejected; a login attempt issued for one
browser cannot complete in another.

**Verify by hand:** run a local Dex (container) with a test client, sign in,
and confirm login success and rejection of a second non-admin account.

**Done when:** the whole flow is reproducible locally without touching a real
identity provider.

## Lesson 9 — Sessions and authorization

**Goal:** short-lived, server-side, revocable sessions with per-request
re-validation.

**Implement:** opaque random session IDs; store only a hash; expiry bounded by
both the configured session minutes and the ID-token expiry (and never more
than a hard ceiling such as 15 minutes); validate issuer and subject on every
request; re-check the admin group/subject allowlist so removing an admin takes
effect immediately; logout revokes; periodic cleanup never touches audit rows.

**Write tests:** expired/forged/revoked sessions rejected; policy change
disables a live session; cleanup runs at most once per minute.

**Verify by hand:** log in, then edit the configured admin group; the next
request must stop working and force a fresh login.

**Done when:** authorization happens against durable state on every request,
not against a cookie claim.

## Lesson 10 — Web hardening

**Goal:** a small attack surface around the authenticated routes.

**Implement:** per-request CSRF token derived from the session and checked for
every unsafe method; same-origin enforcement (`Sec-Fetch-Site`, `Origin`,
`Referer`); login rate limiting; strict security headers (`CSP default-src
'none'` with only `self` allowances, `X-Content-Type-Options: nosniff`,
`X-Frame-Options: DENY`, `Cache-Control: no-store`); error responses that never
echo internals; body size limits.

**Write tests:** missing/incorrect CSRF → 403; cross-origin POST → 403; huge
body rejected; unknown routes 404 without stack traces.

**Verify by hand:** `curl` the API without a session (401), with a session but
no CSRF header (403), with both (success). Inspect headers with
`curl -I`.

**Done when:** you can demonstrate each protection failing closed.

---

# Part 5 — Operations, audit and the UI

## Lesson 11 — Durable operations and audit

**Goal:** multi-step changes survive crashes with a truthful record.

**Implement**
- Durable checkpoints written **before** every Kubernetes write.
- Idempotency keys; one active operation per target (enforced by a unique
  index, not by memory).
- Recovery: on startup, incomplete operations become "needs review" instead of
  blind replay; locks stay held until an operator resolves them.
- Append-only audit entries: actor, plan, target, result, timestamps.

**Write tests:** duplicated submissions return the same operation; concurrent
operations on one target collide cleanly; a simulated crash mid-operation
leaves a reviewable record; shutdown accounts for every accepted operation.

**Verify by hand:** during a long operation, `kill -9` the app; restart and
confirm the UI reports the operation as needing review and nothing was replayed.

**Done when:** there is no code path that writes to Kubernetes without a prior
durable checkpoint.

## Lesson 12 — HTTP API and dashboard

**Goal:** the API in `api/openapi.yaml` plus a single embedded page.

**Implement:** router with public (`/healthz`, `/readyz`, `/auth/login`,
`/auth/callback`) and protected (everything else) groups; JSON endpoints for
inventory, plans, operations, audit; assets embedded with `html/template`
(auto-escaping); a page that renders preview → confirm → result using only
same-origin requests.

**Write tests:** an integration test through real handlers with a fake issuer
and fake Kubernetes API — anonymous 401, login, ready, page render, missing
CSRF 403, plan → submit → observed success, logout invalidation.

**Verify by hand:** click through the whole flow in a browser; confirm every
mutation first shows a reviewable preview.

**Done when:** the browser flow and the integration test cover the same paths.

---

# Part 6 — Ship it

## Lesson 13 — The container image

**Goal:** a minimal, reproducible, non-root image that contains no site data.

**Implement:** multi-stage build; digest-pinned builder and distroless runtime;
`CGO_ENABLED=0`, `-trimpath`, stripped symbols; run as UID/GID 65532; a
`.dockerignore` allow-list that admits only `go.mod`, `go.sum`, `cmd/`,
`internal/`.

**Verify by hand:** export the image filesystem and list it — only the binary
and base-OS files may appear. Run `strings` on the binary for any known
sensitive word. Scan with Trivy (`vuln,secret`) and confirm zero findings.
Run the image with no config: it must fail closed, not start.

**Done when:** the image passes all three checks and you could hand it to a
stranger without leaking your environment.

## Lesson 14 — Manifests

**Goal:** base + overlays that render deterministically and fail safe.

**Implement:** Kustomize base (namespace, ServiceAccount, read-only ClusterRole,
Deployment with `replicas: 1`, `Recreate`, hardened `securityContext`, probes,
resources, NetworkPolicy restricted to the ingress workload) and overlays for
writes, static storage, PVC expansion and local storage. Keep generated config
in a ConfigMap; keep the OIDC client secret in a Secret mounted read-only.

**Write tests:** extend `scripts/check-manifests.py` — render every variant and
assert: the shipped config stays read-only, RBAC verbs stay within the
allowlist, Pod hardening fields are present, single replica/`Recreate`, and
journal-mode values are valid. Keep `scripts/check-api-parity.py` green so every
registered `/api/` route and the structured error envelope stay documented in
`api/openapi.yaml`.

**Verify by hand:** render each overlay and grep the result for `REPLACE_`
and `.invalid`; the base must not be deployable until replaced.

**Done when:** `make manifests` covers every overlay and would catch a
regression to a mutable default.

## Lesson 15 — CI

**Goal:** a pipeline that reruns everything you did by hand.

**Implement:** GitHub Actions with SHA-pinned actions, least-privilege
`permissions: contents: read`; jobs: (a) test/vet/build + manifest checks +
multi-arch image build (no push), (b) vulnerability scans (`govulncheck`,
Trivy pinned by digest) and secret scan. Install Python tooling with
`--require-hashes` from a compiled `requirements.txt`.

**Verify by hand:** run each job's commands locally; deliberately introduce a
failing case (e.g. an un-allowed RBAC verb) and confirm the checker fails.

**Done when:** CI would block the exact regressions this repo's history went
through.

---

# Part 7 — Deploy by hand

## Lesson 16 — Discover the cluster (read-only)

**Goal:** know exactly what you are deploying onto.

**Implement:** use `scripts/discover.sh <context> > discovery.txt` (kept local,
git-ignored) or equivalent read-only `kubectl` queries to record: Kubernetes
and distribution versions, Profile/Notebook API versions and controller
versions, the ingress gateway type, the StorageClasses your database and user
volumes may use, namespace list, and RBAC you can obtain.

**Verify by hand:** every fact in your inventory comes from a command you can
re-run; nothing is assumed.

**Done when:** you can answer "which storage class will hold the database, and
what does its backend guarantee?" without guessing.

## Lesson 17 — Choose and verify database storage

**Goal:** a database location whose durability and locking you trust.

**Implement:** decide between
1. **Local/block storage** (node-local PV): WAL, best behaviour, app pinned to
   one node; or
2. **A reviewed network storage class** with DELETE journaling and the explicit
   `allowUnverifiedFilesystem` opt-in.

**Verify by hand (required for option 2):**
- run `deploy/examples/db-ownership-check.yaml` (disposable RWX claim, two
  nodes): the attempt pod must exit **3**; anything else means locks do not
  propagate — use option 1.
- crash test on a disposable database: write continuously, `kill -9`, reopen,
  `PRAGMA integrity_check` must be `ok`.
- confirm with the storage operator that global file locks and remote fsync
  are effective for the CSI mounts (settings live on nodes, not in the class).

**Done when:** both tests pass on the backend you intend to use, or you chose
option 1.

## Lesson 18 — Identity and HTTPS

**Goal:** a real login over a dedicated HTTPS origin.

**Implement:** register a confidential OIDC client (code flow + PKCE) with the
exact callback `https://REPLACE_WITH_ADMIN_HOST/auth/callback`; create the
client-secret Secret from a file (never shell history); create the HTTPS route
using `deploy/examples/istio-https.yaml` as a template (or your own ingress);
DNS and certificate must exist before login can work.

**Verify by hand:** `curl -sI https://REPLACE_WITH_ADMIN_HOST/healthz` returns
200 over a trusted certificate; sign-in works; a non-admin account is refused.

**Done when:** the anonymous request is 401, the non-admin 403, the admin gets
the dashboard — all from outside the cluster.

## Lesson 19 — Read-only rollout

**Goal:** the app in the cluster observing, but unable to change anything.

**Implement:** `deploy/site/` overlay generated from `deploy/base` with your
real host, issuer, group/subject and the image pinned by digest; keep all
features false and `controllerContractConfirmed: false`; render and review;
apply; watch rollout.

**Verify by hand:** compare each inventory page with `kubectl`; test logout,
session expiry and a second browser; confirm the pod's ServiceAccount cannot
create anything (`kubectl auth can-i --as=system:serviceaccount:...`).

**Done when:** inventory matches `kubectl` exactly and no write verb is
available to the deployed identity.

## Lesson 20 — Enable writes for one pilot namespace

**Goal:** mutation capability without blast radius.

**Implement:** apply the writes RBAC overlay and set
`allowedNamespaces: [REPLACE_WITH_PILOT_NAMESPACE]`; keep static storage and
expansion disabled; confirm `controllerContractConfirmed` only after testing a
notebook stop/start on a disposable notebook.

**Verify by hand:** stop a pilot notebook → observe the annotation the
controller expects; start it again; edit the image or resources and confirm
convergence; check quota headroom; confirm a plan for a non-pilot namespace is
refused.

**Done when:** you can describe, from observation, exactly what each write
does, and rollback means disabling flags/RBAC, not deleting anything.

## Lesson 21 — Operations: backup, recovery, incidents

**Goal:** be able to lose the node and recover by your own procedure.

**Implement:** a backup of the **stopped** database directory including any
`-journal`/`-wal`/`-shm` files, checksummed and stored off-node; a written
restore procedure; a plan for "needs review" operations and for sessions after
an incident.

**Verify by hand:** perform a restore drill into a scratch namespace and log
in with the restored database. Deliberately corrupt a disposable copy and
confirm your procedure detects it.

**Done when:** you have executed the restore once by hand, not only written it
down.

## Lesson 22 — Final acceptance and retest

**Goal:** a reproducible final pass over everything.

**Verify by hand, in one sitting:**
1. `make check` on a fresh clone; the manifest checker renders every overlay.
2. Rebuild the image; verify: no extra files, zero Trivy findings, fails
   closed without config.
3. Re-run the two database tests (ownership, crash recovery) on the chosen
   storage.
4. Re-run the deployment checks: login, non-admin refusal, inventory parity,
   read-only identity, pilot write, logout.
5. Confirm every non-obvious behaviour above has a test in the repository —
   and that no test requires a real cluster.

**Done when:** a colleague can repeat the whole plan from this document alone.

---

## What this plan cannot prove

Local tests and scratch clusters cannot validate a specific cluster's Istio
policies, CSI provisioning, admission webhooks, identity-provider claim
mapping or production load. Those remain operator acceptance items; the app
should stay read-only until each is observed working, and every mutation
should remain behind a preview-and-confirm step.

## Optional stretch lessons

- Sign the image (cosign) and enable immutable tags in the registry.
- Add metrics (Prometheus) behind authentication, keeping audit data private.
- Study client-server databases (PostgreSQL) as an alternative for the
  durable store; list everything that would change (backup, auth surface,
  an extra workload) before writing code.
- Add a third reviewer scenario: two admins racing the same notebook, to
  observe idempotency and target locking in the audit trail.
