# Learning plan — 15 lessons to implement, test and deploy Kubeflow Admin

Use the current code as a reference solution: implement a bounded slice yourself,
test it, then compare. Related topics from the former 22-lesson plan are combined
below; storage safety, authentication, operations and deployment are not omitted.
This guide describes current source, not a claim that a particular cluster or
unpublished release has passed live acceptance.

## How to use the plan

- Finish each lesson's **Done when** checks before moving on. Run the tests for
  the packages you have implemented; once the build/manifests exist, use the
  complete regression gate:
  ```sh
  GOTOOLCHAIN=go1.27.3 make test vet build manifests
  ```
- Use fake APIs/local TLS issuers first. Cluster exercises are operator-run only,
  explicitly context-selected and limited to disposable namespaces and storage.
  A local kind/minikube cluster alone does not supply a Kubeflow Notebook/Profile
  controller, CSI backend, trusted HTTPS or OIDC integration.
- Start read-only. Never point learning code at production, widen RBAC to make a
  test pass, force cleanup or remove finalizers. Rejected unsafe actions are valid
  test results. Accepted operations are not proof of completed changes.
- Keep real site config, identity data, credentials, certificates, databases and
  discovery output outside Git. Review every example placeholder before use.
- Docs-only corrections need no image rebuild/deployment or CI run. Do not apply
  that exception to executable code, tests, config, dependencies or workflows.

## Prerequisites (not an additional lesson)

| Tool/environment | Purpose |
| --- | --- |
| Reviewed Go 1.27.3 toolchain | Current release builds/checks; module minimum Go 1.26 is not a security recommendation |
| Python 3, Kustomize 5, kubeconform | API/manifest checks and strict Kubernetes schema validation |
| Docker with Buildx | Multi-platform builds and local test/scanner containers |
| Node.js; Playwright and Chrome for browser acceptance | Logic/navigation regressions and real rendering checks |
| Git, curl, OpenSSL; kubectl for selected cluster exercises | Source review, HTTPS checks, operator-only observations |
| gh and a GitHub account when exercising CI | Source verification before promotion |

Install pinned Python tooling without changing the system environment:

```sh
python3 -m venv .venv
.venv/bin/python -m pip install --require-hashes -r scripts/requirements.txt
source .venv/bin/activate
```

Use the scanner versions/digests reviewed in `.github/workflows/ci.yaml`, not an
unreviewed `@latest` scanner. `govulncheck` must use the release toolchain; an
older host Go installation can report a vulnerability absent from the pinned
container build. Docker image scans independently check the actual runtime.

A development machine needs enough memory/disk for Docker, caches and fixtures;
16 GB RAM is more comfortable than 8 GB. A disposable two-node environment is
needed only for shared-filesystem lock verification. No GPU is required to test
preset validation; actual GPU scheduling remains a separate operator test.
The base deployment requests 100m CPU/128Mi memory, limits 1 CPU/512Mi and uses a
5Gi database claim. Those are manifest settings, not measured production sizing.

Read [deployment](deployment.md), [database](database.md), [UI guide](ui.md) and
[OpenAPI](../api/openapi.yaml). They supplement the lessons, not replace tests.

## Lesson 1 — Strict configuration and build discipline

**Goal:** a compiling `kubeflow-admin` module that rejects unsafe configuration.

**Implement:** the `cmd/` and `internal/` structure; strict single-document YAML
decoding; config structs matching `internal/config/config.go`; safe defaults and
validation for HTTPS origin, OIDC/admin authorization, timeouts, session lifetime,
database, policy, storage, Notebook presets, history, logging and optional footer.
Keep all write flags and the Notebook controller-contract gate false by default.
Reject unknown fields, URI/in-memory database paths and unsafe combinations.
Add repeatable test/vet/build targets and `gofmt` checks. Build without CGO or VCS
stamping, using trimmed paths and the reviewed Go patch.

**Test/verify:** invalid origins/paths, unknown keys, extra YAML documents and
out-of-range values fail before the service starts. Inspect build flags and
cross-compile a Linux binary; a warm module cache is not an offline guarantee.

**Done when:** config tests and build pass, and invalid input never starts an
insecure fallback service.

## Lesson 2 — Private SQLite records and journal policy

**Goal:** durable local records, private files and explicit filesystem limits.

**Implement:** pure-Go SQLite; private directory/regular-file checks, `O_NOFOLLOW`,
`umask 0077`; JSON record operations including atomic single-use `Take`. Apply
connection pragmas to every connection, not only the first one. WAL/FULL is the
local/block-storage default; DELETE/EXTRA is the reviewed BeeGFS option with
`allowUnverifiedFilesystem: true`. Use the actual statfs policy table: rejected
filesystems do not become acceptable merely by setting the opt-in. Development
overlayfs is unverified, not evidence of persistent storage.

**Test/verify:** close/reopen preservation, concurrent `Take`, symlink/permission
refusals, connection replacement and journal migration. Check `integrity_check`
with trusted SQLite tooling on a stopped disposable copy.

**Done when:** all filesystem-policy cases are tested and you can distinguish a
configured durability mode from proven backend durability. See [database](database.md).

## Lesson 3 — One writer, ownership and storage durability

**Goal:** one process owns one database; deployment cannot silently bypass it.

**Implement:** nonblocking exclusive lifetime `flock` on the private lock file,
typed lock failure, idempotent Close and one worker lease. Supply `-check-db` and
bounded hold behavior matching the CLI. Run one replica with Recreate, not HPA.
An advisory lock is not a substitute for node fencing or correct remote fsync.

**Test/verify:** second opens in-process and in a child process fail; reopening
after Close/process exit succeeds. For a disposable shared-filesystem claim,
use [the ownership-check procedure](database.md): verify cross-node lock refusal,
crash/reopen integrity, global locks and remote-fsync behavior. Never run these
experiments against the live app database.

**Done when:** the backend passes the reviewed tests or you choose local/block
storage with its explicit node-outage and backup tradeoffs.

## Lesson 4 — Read-only Kubernetes inventory

**Goal:** truthful cross-profile observations for authorized administrators.

**Implement:** in-cluster clients or an explicit kubeconfig, bounded requests and
typed/projected inventory. Include Nodes, Namespaces, Profiles, Notebooks, Pods,
StatefulSets, PVs/PVCs, StorageClasses, quotas, LimitRanges, RoleBindings and
Events according to the shipped read RBAC. Fetch Pods/StatefulSets in bulk and
derive Notebook lifecycle using exact UID ownership. Do not expose full manifests,
environment values, Secret data or storage backend handles/paths.

**Test/verify:** list failures, stale/unknown states, same-name objects of different
kinds/namespaces, owner UID mismatch and bulk request counts. Mutation namespace
policy is not a claim that the administrator can read only their own namespace.
Compare selected metadata with kubectl only in a chosen disposable environment.

**Done when:** observations work with base read RBAC and cannot be mistaken for
atomic mutation evidence, live utilization or controller success.

## Lesson 5 — OIDC login, PKCE and browser binding

**Goal:** authorization-code login without a development-authentication bypass.

**Implement:** discovery/JWKS, PKCE S256, state, nonce and single-use server-side
attempts bound to the initiating browser. Verify issuer/signature/audience,
multi-audience authorized party, expiry and applicable token-hash checks. Use
Secure/HttpOnly/SameSite cookies and the current host-cookie rules. Keep provider
secrets in a read-only file, never config, logs or browser storage.

**Test/verify:** local TLS issuer fixtures reject wrong signature, issuer,
audience, nonce, expiry, replayed state and another browser's callback. Exercise
admin/non-admin identities with a disposable registered local OIDC client.

**Done when:** login failures are closed, bounded and reproducible without a
real directory lookup or password-handling feature.

## Lesson 6 — Sessions, authorization and HTTP hardening

**Goal:** short-lived server-side sessions and a small protected attack surface.

**Implement:** hash opaque session IDs, cap expiry by provider/configured lifetime
and the 15-minute ceiling, validate durable session/local authorization per
request and at mutation checkpoints, and revoke on logout. Do not expose the
session key in JSON. Add same-origin/CSRF checks, bounded bodies, login limiting,
strict headers and safe exact-authored error mapping. Public probes/login/callback,
signed-out and static routes differ from authenticated UI/API routes.

**Test/verify:** expired/forged/revoked sessions, missing CSRF, cross-origin POST,
oversize input, unknown routes and credential redaction. Config is startup-loaded;
editing a file alone is not a live policy reload. Upstream group changes are not
instantly re-queried: document the short session/provider boundary honestly.
Optional footer HTML is sanitized with a strict allowlist, not rendered raw.

**Done when:** every unsafe request and checkpoint requires current local
authorization; logout reaches the public signed-out page without a login loop.

## Lesson 7 — Typed previews and fresh conditional mutations

**Goal:** reviewed plans, never arbitrary manifests or a Kubernetes proxy.

**Implement:** server-owned actor-bound plans with five-minute expiry, canonical
targets and bounded typed inputs. Validate policy, images, GPU presets, mounts,
classes and quotas; dry-run before confirmation. Execute fresh UID/resourceVersion
checks and conditional writes appropriate to each action. Preserve client-go's
no-automatic-write-replay safeguard, including Retry-After responses.

**Test/verify:** stale/replaced objects, changed dependencies, expired plans,
unknown fields and hidden-field preservation. A strict restricted PV/PVC YAML
projection is supported; full resource manifests are not. Create, patch, normal
Pod eviction and the three separately gated delete actions have different
preconditions; do not reduce all writes to one generic patch/delete helper.

**Done when:** preview changes nothing and every mutation revalidates the exact
reviewed target. Dry-run remains validation, not a guarantee of later admission.

## Lesson 8 — Durable operations, locks and bounded audit history

**Goal:** accepted work stays accountable across crashes without blind replay.

**Implement:** durable pre-write checkpoints, immutable acceptance evidence,
idempotency claims, canonical target locks and one sequential worker. Exclusive
namespace fences for Profile/PVC deletion block app mutations in both directions;
static create/reserve also conflict through the shared PV target. Startup marks
incomplete work needs-review and retains locks. Integrity checks fail closed.
Bound presentation History by age/rows/bytes while preserving safety evidence,
active locks and idempotency tombstones. File-log rotation is separate.

**Test/verify:** duplicate submit, target collisions, namespace fences, checkpoint
failure, ambiguous 429/5xx/transport results, restart recovery, bounded cleanup
and shutdown accounting. Inject failures in fixtures, not a production writer.

**Done when:** no Kubernetes mutation precedes its durable checkpoint and no
History cleanup can erase a safety lock. See [history](history.md).

## Lesson 9 — Profile and existing-Notebook management

**Goal:** useful edits without ownership transfers or cross-target mistakes.

**Implement:** DNS-label Profile creation, duplicate Profile/namespace refusal,
email syntax without invented IdP-registration verification. Quota updates echo
the exact unchanged owner and identity-bound configuration digest. Preserve
unmanaged keys/types and existing aliases/raw device keys; do not auto-migrate
device accounting. Manage existing Notebooks only: start/stop and named-container
edits behind writes plus verified controller contract. Blank fields preserve;
mount replacement is explicit, protected paths stay protected.

**Test/verify:** zero quotas, unsupported owner changes, quota evidence drift,
wrong target/container UID, stop/start convergence, quota/LimitRange refusals,
GPU presets and same-namespace PVC mounts. Changing target clears previous edits.
Notebook initial creation uses the normal Kubeflow workflow, not this console.

**Done when:** before/after previews show only approved deltas, and synthetic
tests are not presented as proof of the installed controller's behavior.

## Lesson 10 — Storage, gated deletion and scheduling controls

**Goal:** narrow storage/workload actions with explicit irreversible boundaries.

**Implement:** dynamic PVC creation; standalone template-based static PV
registration and PVC creation bound to a selected existing PV; approved prepared
NFS/CSI legacy registration and PV reservation; CSI-compatible upward-only PVC
expansion. Read-only template values supply PV/PVC capacity, access modes,
reclaim/volume mode, mount options and driver/prefix details **only for the two
new registration kinds**; the legacy combined static, reservation and dynamic
contracts intentionally keep their existing size/access-mode inputs. New NFS
templates accept their approved root or a descendant and require nonzero UID/GID
annotations that are intent-only metadata — prepare and verify the actual
directory/ownership externally. BeeGFS accepts only a canonical relative suffix
under the fixed prefix, and the joined final handle must be at most 128 bytes.
Keep legacy templates and workflows unchanged. Restricted Edit allows console
labels, Available/unclaimed PV reclaim policy and gated PVC growth. Omitted/null
labels preserve; explicit `{}` clears only console labels. PVC creation may
optionally create or explicitly overwrite one dedicated PodDefault when its
false-by-default gate/RBAC is enabled; the final name is capped at 52 bytes and
there is no standalone PodDefault page or mutation endpoint. For dynamic PVCs
the PodDefault is attempted only after the PVC object exists; a
WaitForFirstConsumer claim may still be Pending, and PodDefault acceptance is
not binding/mount readiness. PVC Delete may clean related namespace-local
PodDefaults (at most 32) only after Pod/stopped-Notebook checks, a shared/external
warning review and explicit acknowledgement. Delete requires separate
gates/RBAC, exact confirmation/reason, fresh identity and unused checks including
stopped Notebook references. Profile Delete waits for its owned namespace too.
Pod restart is a PDB-aware normal eviction of a supported Notebook StatefulSet
Pod; Node cordon patches scheduling only, never drains workloads.

**Test/verify:** forged read-only template fields, NFS root/descendant and BeeGFS
escape/encoding/final-handle-bound cases, nonzero UID/GID, wrong class/owner,
incompatible or replacement PV, referenced/malformed observations, missing CRD
versus denied/error discovery, stale PodDefault UID/resourceVersion, shared
warnings, the 52-byte PodDefault name and 32-object cleanup bounds, PDB rejection
and ambiguous writes never force or replay. Verify dynamic PodDefault work after
the PVC object with a Pending/WFFC claim, that a PodDefault failure leaves the
actual PVC/PV in place with a truthful partial outcome, and that completion
requires manual operator `kubectl` review with no app repair/replay endpoint.
Verify cleanup never recreates PodDefaults or deletes another PVC, and that
namespace fences/data-loss warnings are correct. Storage unused checks do not
atomically fence external workloads or binders.

**Done when:** each capability has its own tested gates and exact permissions;
the base read role can detect PodDefault references while mutation stays behind
the separate overlay; there is no standalone PodDefault API, forced deletion,
finalizer removal or generic cleanup. See [storage](storage.md), [Pods](pods.md),
[Nodes](nodes.md) and [acceptance](ui-workflow-acceptance.md).

## Lesson 11 — API, current dashboard and browser regressions

**Goal:** the implemented OpenAPI contract and accessible, truthful navigation.

**Implement:** embedded same-origin UI and safe API handlers; preview → explicit
confirmation → durable result. Follow [the current UI guide](ui.md): Workspace
Health only, simplified Pods/Nodes tables with full safe Resource Details,
Profile information above its table, stable right-centered Notebook action,
Node-to-Running-Pods URL filters, visible filter reset and Back/Forward behavior.
Storage forms keep template-derived capacity/modes/reclaim/server/prefix values
read-only, expose the PV NFS/BeeGFS path only as a typed input, and expose bound
NFS UID/GID only as typed nonzero inputs. Offer the optional PodDefault switch
only inside PVC creation. There is no standalone PodDefault page/button; cleanup
is a reviewed part of PVC Delete. Use compact expandable notifications: errors
first, newest-first within groups, deduplication, safe dismissal IDs and
preserved preview warnings.

**Test/verify:** handler auth/CSRF/error boundaries, Node logic/navigation harnesses
and configured real-Chrome synthetic fixtures. Check 390px/1280px overflow, table
cell/header alignment, button geometry, focus, disabled gates, late responses and
cross-target form clearing. A missing browser dependency is a skip, not a pass.

**Done when:** regression tests and actual browser rendering agree; observations,
accepted operations and workflow convergence are visibly distinct.

## Lesson 12 — Images, security gates, CI and publication

**Goal:** scan and publish exactly the reviewed non-root multi-platform release.

**Implement:** digest-pinned Go 1.27.3 builder/distroless runtime, CGO-disabled
trimmed build, UID/GID 65532 and `/LICENSE`. Keep .dockerignore's explicit source/
LICENSE allowlist; no site config, databases or credentials in images. CI uses
SHA-pinned actions, contents-read permission, test/vet/build/manifests, no-push
multi-architecture compilation and reviewed Go/Trivy scans.

**Test/verify:** full race suite, API parity, strict schemas, source vulnerability/
secret/misconfiguration checks, both runtime scans, revision labels/license and
read-only-root/network-disabled smoke tests. Confirm successful source CI, then
promote the scanned platform artifacts with provenance/SPDX SBOM. `make image-push
IMAGE=repository:VERSION` publishes VERSION and latest from one build; it does
not itself enforce the prior verification gates. Check both tags resolve to the
verified index and platforms match scanned artifacts. Do not rebuild historical
versions as latest by accident. Pin deployment digests, not mutable tags.

**Done when:** publication has a reproducible receipt for source, checks and both
platforms; docs-only receipts can use `[skip ci]` without changing binaries.

## Lesson 13 — Discovery, manifests and read-only rollout

**Goal:** deploy the configured reader with no accidental mutation privileges.

**Implement:** context-selected read-only discovery; one-base site Kustomization,
single Recreate replica, hardened Pod, probes, ingress NetworkPolicy, durable
database and a read-only OIDC Secret mount. Choose/verify DB storage from lessons
2–3. Review each optional RBAC overlay independently; permissions do not set
flags. Keep every flag/controller-contract gate false for the first rollout.
Before adding `storage.registrationTemplates` or `features.podDefaults`, back up
the stopped DB/journal set and the old config. Old binaries reject the new keys
under strict `KnownFields`; the store schema stays version 3 with no new columns,
but never roll back the image alone. New-action operations cannot be replayed by
an old app; use reviewed restore/reconciliation instead.

**Test/verify:** Python helper tests/API parity, all rendered variants (including
the separate `poddefaults` overlay) and strict Kubernetes schemas. Replace
placeholders in local site files only; pin the verified image index. Review
render/diff/server dry-run, then apply only in the approved disposable context.
Readiness does not prove full RBAC, login or controller/storage compatibility.
Inspect exact ServiceAccount capabilities, not merely `can-i create` without a
resource.

**Done when:** read-only inventory matches selected observations, optional write
bindings are absent/revoked, and one base supplies no duplicate resources.

## Lesson 14 — HTTPS, identity and disposable live workflow acceptance

**Goal:** a trusted login path and independently verified, narrow write pilot.

**Implement:** a dedicated HTTPS origin/certificate, exact OIDC client callback,
registered disposable owner identity and local secret file. Use the reviewed
ingress example without weakening a shared gateway. Confirm installed Profile/
Notebook controller ownership, annotations and convergence before enabling writes.
Restrict the pilot namespace, then add only the required reviewed RBAC/flags.

**Test/verify:** trusted TLS, non-admin denial, logout/expiry, observed inventory,
fresh preview and one confirmed disposable operation at a time. Follow
[the complete acceptance checklist](ui-workflow-acceptance.md) for Profile,
Notebook, storage, Pod and Node actions. Observe real UID/lifecycle, mounted data,
CSI/filesystem outcomes and quota enforcement independently. Stopping a Notebook
does not remove its PVC references. Test deletion only with a reviewed data/reclaim
plan; Retain is not a backup/wipe and Delete may destroy backing data.

**Done when:** evidence records both successful/refused cases and every unproven
integration. Turning flags off/revoking bindings is not rollback of prior writes.

## Lesson 15 — Backups, incident recovery and final handover

**Goal:** recover deliberately without discarding safety evidence or user data.

**Implement:** stopped-directory backup of the complete database/journal set,
checksums, encryption, off-node retention and rehearsed restore on a disposable
copy. Back up the matching configuration too: a failed new-config rollout cannot
be fixed by restoring an old image alone because old binaries reject the new
keys, and new-action operations cannot be replayed by them. Record
release/config/schema and actual cluster state. Preserve needs-review locks;
never replay a stale DB as Kubernetes rollback. Only Profile deletion has the
narrow stopped-app reconciliation tool; storage has no generic unlock or force
cleanup. Bound private file logging separately from database History.

**Test/verify:** restore/integrity drills, node-loss procedure, compatible schema,
session/plan handling, corrupted-copy refusal and no second writer. Rerun release
checks and selected operator acceptance from a fresh documented environment.
Keep observation records private; [verification](verification.md) distinguishes
local/synthetic proof from live acceptance.

**Done when:** a colleague can repeat the safe build/deploy/backup procedure and
explain every remaining risk without guessing. See [backup](backup.md),
[database](database.md), [operations](operations.md) and [logging](logging.md).

## Limits and optional exercises

No local suite certifies a site's admission webhooks, CSI behavior, filesystem
locking/fsync, IdP claims, gateway policies, GPU placement or production load.
Keep unverified features off. Optional separate exercises include image signing,
an authenticated metrics integration or evaluating a client-server database;
these are not implemented capabilities or additional required lessons.
