# Verification record

Records what has been **proven locally** and what still requires the operator's live acceptance. Fake Kubernetes clients and local OIDC fixtures do not simulate Kubeflow reconciliation, CSI provisioning, scheduling, admission or Istio.

## Responsive UI / History / Pods work — 20 October 2026

**Local review image published; live cluster acceptance is still pending.** The
older artifact and acceptance records below are historical, not evidence that
this feature set has been deployed or accepted by an operator.

- Planning, implementation and independent review used DeepSeek V4.1 Flash
  through OpenCode Go with **high thinking**. The supervisor reviewed findings,
  added regression fixes and repeated the gates on the exact final tree.
- `make check` passed race tests, vet, API parity/script tests and all seven
  manifest variants. `go test -race -count=2 ./...` passed twice consecutively
  uncached after the final retention change. `go build -mod=readonly ./...` and
  OpenAPI specification validation passed.
  `go run golang.org/x/vuln/cmd/govulncheck@latest ./...` reported no vulnerabilities.
- Actual headless Chrome via Playwright 1.56.1 tested synthetic observations at
  390×844 and 1280×844 after UI remediation: bounded long-table scrolling, no
  page-width overflow, navigation, History tabs, search shortcut/filtering,
  drawer close/Escape, profile drilldown/namespace links, one inventory read
  across sidebar navigation, and warning dismissal surviving refresh. The
  synthetic fixture included 300 Notebooks, 300 Pods, 300 PVCs and a 1,000-event
  audit window. An additional optional browser suite covered the History loading
  race, lazy stage details, pager focus, local-clock freshness, retryable drawer
  errors and unsupported-owner restart gating.
- This browser fixture/harness is external temporary tooling, not part of the
  distributed repository or production image. Node tests in
  `internal/web/testdata/` are repository gates but their stub DOM does not prove
  layout, focus management or browser accessibility. Real-browser layout tests
  complement, rather than replace, keyboard/screen-reader and operator checks.
- Store tests cover schema-3 migration authenticity, strict read-time audit age
  filtering, bounded list/cleanup/admission, immutable unresolved acceptance
  evidence, terminal redaction/tombstones, replay safety and concurrent access.
  Logical data budgets are not whole-SQLite/WAL or backup disk quotas.
- Pod fixtures cover feature/scope/owner/Node refusal, one PDB-aware eviction
  request with UID/RV preconditions, definite rejection versus ambiguity,
  parent locking and replacement convergence. A fake client does not prove live
  API-server enforcement, fencing, controller behavior or data durability.
- Logging tests cover size/count/age rotation, concurrent writes, private files,
  symlink refusal and oversized existing content.

Published review image (20 October 2026):

- `docker.io/muratshagirov/kubeflow-admin:0.3.0`, linux/amd64 only.
- OCI index digest: `sha256:bdf48e5883a0496f5f956f2a8bae5f288d5fcb05cc8e1ed87637e0f87750060d`.
- linux/amd64 manifest: `sha256:6d22631c3bbdc5665a5527bc09b31e524101e4541d1deeab1eecd72f89ed371e`.
- Built with provenance and SBOM attestations from pinned builder/runtime bases;
  verified by digest-pinned pull metadata. Runtime user is `65532:65532`; starting
  without config fails closed.
- Pinned Trivy 0.76 image scan (`vuln,secret`, HIGH/CRITICAL) found **0**
  vulnerabilities and **0** secrets. `govulncheck` found no source vulnerabilities.
  These scans are point-in-time and must be repeated before any later promotion.

Before deployment: take and verify a compatible backup before the schema-3
migration, pin the index digest above in the site overlay, and keep Pod restart
disabled until the operator approves the separately scoped eviction permission and
a disposable pilot. No unfenced force-delete workflow or fabricated
pipeline/metrics integration is included. Live controller, OIDC, storage, network,
retention-under-load and restart acceptance remain operator steps; local fake
fixtures and browser simulation do not establish them.

The review follow-up used DeepSeek V4.1 Flash through OpenCode Go with default thinking for four implementation streams and three separate read-only review scopes (auth/UI/deployment, Kubernetes, and operations/store). The final independent reviews found no blocking issues. Lead review also corrected exact PV identity/checkpoint timing and action-scoped secondary PV locks before the final checks below.

## Proven locally (16 October 2026)

| Check | Command | Result |
| --- | --- | --- |
| Compile all packages | `go build ./...` | Pass |
| Unit/integration tests | `go test -mod=readonly -race -count=1 -cover ./...` | Pass, including review regression tests; kube package statement coverage 69.1% |
| Linux race detector | Pinned Go 1.27.2 Linux/arm64 container, `go test -mod=readonly -race -count=1 ./...` | All packages pass; network disabled and source/module mounts read-only |
| Repeated shuffled tests | `go test -mod=readonly -shuffle=on -count=10 ./...` | All packages pass |
| Vet | `go vet ./...` | Pass |
| Dependency vulnerabilities | `govulncheck ./...` on host and Linux/amd64 source; `govulncheck -mode=binary` on the new image binary | No vulnerabilities found |
| Manifest render + invariants | `make manifests` | 6 variants OK; RBAC, single-replica/`Recreate`, hardened Pod, read-only shipped config, journal-mode values; API route/error-shape parity against `api/openapi.yaml` |
| Linux binaries | Dockerfile build for amd64; `CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build` | Both compile; new amd64 binary checksum below |
| Local review image | `docker buildx build --platform linux/amd64 --load --provenance=false -t kubeflow-admin:security-review .` | Built locally from pinned bases; not published or promoted |
| Source security scan | Pinned Trivy 0.76 container, `fs --scanners vuln,secret,misconfig --severity HIGH,CRITICAL` | No HIGH/CRITICAL findings; ignored local binaries, virtualenv, Git data and private site notes excluded |
| Image security scan | Same pinned Trivy container, `image --input` on a saved local image, vulnerability/secret scanners, HIGH/CRITICAL | No HIGH/CRITICAL findings; no Docker socket or network access given to scanner |
| Hardened image smoke | New Linux/amd64 image, UID/GID 65532, read-only root and network disabled | Missing config fails closed; disposable-volume `-check-db` opens DB; competing container exits 3 while holder owns it |
| CI supply chain | `.github/workflows/ci.yaml` | Actions pinned by SHA; Trivy pinned by digest; Python tooling installed with `--require-hashes` (`scripts/requirements.txt`) |

Evidence of behavior (not a cluster pass):

- `cmd/kubeflow-admin/integration_test.go`: full HTTPS flow through real HTTP handlers, real OIDC code+PKCE verification against a fake TLS issuer/JWKS, real SQLite store, real operation worker, and fake Kubernetes APIs. Covers anonymous 401, login, callback, `/readyz`, page render, missing-CSRF 403, preview → submit → observed success, correct policy/not-found/conflict/RBAC error classification through the real manager, sanitization of unknown backend errors, and logout invalidation without automatic re-login. `-check-db` ownership reporting is tested, including the exit-3 path (`store.ErrLocked`).
- `internal/auth`/`internal/web`: PKCE/state/nonce, issuer/audience/signature/expiry rejection, single-use attempts, hashed sessions, CSRF/same-origin, proxy-aware limiter modes, bounded global/source budgets, public script-free signed-out landing, and a narrow template view-model without server/backend secrets.
- `internal/kube`: namespace/Profile scope, conditional patches, container/volume preservation, protected workspace mounts, non-root storage path boundaries, PV/PVC binding and GPU preset removal. Deterministic fake-controller tests now exercise Profile create/quota changes, dynamic Immediate/WFFC claims, static create/reserve and expansion. Regression tests cover unknown quota preservation, malformed mount shapes, exact reviewed PV identity, changed dependencies and overlap injected during write checkpoints. These fixtures provision nothing and are not live controller evidence.
- `internal/operations`: durable checkpoint before every write, idempotency, one active operation per target, no restart replay, session revoke halts queued and running work, shutdown accounts for every accepted operation.
- `internal/store`: durable records, single-use `Take`, concurrent-claim uniqueness, canonical action-scoped targets, state transitions, exclusive single-writer lock, WAL/DELETE migration, connection replacement synchronization, malformed-auth cleanup, expired-plan cleanup preserving operations/audit/idempotency, fail-closed `CheckIntegrity`, unsafe pre-existing-sidecar rejection, and SQLite 0600 sidecar inheritance under permissive umask in an isolated subprocess. overlayfs is reported unverified but accepted for CI/dev. Linux tests cover the BeeGFS policy, not a live BeeGFS mount.

Current local review artifacts (not a published release):

- Image: `kubeflow-admin:security-review`, Linux/amd64, built and scanned locally only.
- Extracted binary SHA-256: `591020a136a39b08ea646a7896c8552cad4de7c5514fb9d659e310b4dbfc7af9`.
- Trivy image: `aquasec/trivy@sha256:2416d1f74045b57274df1a340eee6b510cacd73c66a6ccb8da092b117eb7d222`.

Historical published `0.1.0` artifacts (do not contain these review fixes):

- Binary: `bin/kubeflow-admin-linux-amd64`
- SHA-256: `4bd887eb9afac295cf1a878cb97c170f4db9326f903b057d9a1660b6f283fb60` (extracted from the published image)
- Published image: `docker.io/muratshagirov/kubeflow-admin:0.1.0` (linux/amd64)
  - OCI index digest: `sha256:19f3eba52e65952c6812a24c061e0e0bce2e38a4308e48e9cd9ca1549a25ef7d`
  - linux/amd64 manifest digest: `sha256:4615de6e8c0a8f00485c20cc25921892704621ffefb8b5c502ac5b513b87f303`
  - Republished with the fail-closed filesystem policy (explicit `allowUnverifiedFilesystem` opt-in), `umask 0077` file protection, DELETE journaling support, the `-check-db` ownership test mode, provenance and SBOM; verified by digest-pinned `docker pull --platform linux/amd64` and container runs for startup failure without config and `-check-db` flag presence.

**The published digest above is historical, not the fixed source.** A new image was built and checked locally, but no registry image was pushed or republished and no new pullable registry digest is asserted. Before deployment, publish the reviewed source with the release provenance/SBOM workflow, rescan it and pin its fresh registry digest in the site overlay. The recorded `0.1.0` digest must not be treated as representing these fixes. Re-run checks for the exact revision you promote.

## Dependency security notes

Two transitive dependency advisories were found and fixed during implementation:

- `github.com/go-jose/go-jose/v4` upgraded `4.1.3 → 4.1.4` (GO-2026-4945).
- `golang.org/x/net` and `golang.org/x/text` upgraded to fixed versions (GO-2026-6603/6610/6611/6612/6617, GO-2026-5970, GO-2026-5026, GO-2026-4918).

`govulncheck ./...` now reports no vulnerabilities. Re-run before every promotion.

## Requires live cluster acceptance (not yet performed)

- Installed Notebook/Profile controller digests, lifecycle annotation and culling behavior on a disposable notebook.
- Real OIDC login against the site Dex issuer, exact admin group claim, and callback routing behind the chosen HTTPS route.
- Admission/RBAC behavior under installed policy; forged-header and direct-Pod access tests.
- Dynamic PVC binding, static NFS/CSI import, PV reservation, and supported expansion against your actual storage backend.
- Whether the configured database filesystem and backup arrangement meet durability requirements; for BeeGFS, verify effective global locks, remote fsync, cross-node owner exclusion and interrupted-write recovery.
- Notebook edit convergence including image pull, GPU scheduling, quota headroom and data persistence across stop/start.

These are listed as operator steps in [deployment](deployment.md) and [operations](operations.md). Until they pass, run read-only (`features.writesEnabled: false`).
