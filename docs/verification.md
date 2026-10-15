# Verification record

Records what has been **proven locally** and what still requires the operator's live acceptance. Fake Kubernetes clients and local OIDC fixtures do not simulate Kubeflow reconciliation, CSI provisioning, scheduling, admission or Istio.

## Proven locally (16 October 2026)

| Check | Command | Result |
| --- | --- | --- |
| Compile all packages | `go build ./...` | Pass |
| Unit/integration tests | `go test -race ./...` | Pass, including journal migration, filesystem policy and opt-in tests |
| Race detector | `go test -race ./...` | Pass |
| Vet | `go vet ./...` | Pass |
| Dependency vulnerabilities | `govulncheck ./...` | No vulnerabilities found |
| Manifest render + invariants | `make manifests` | 6 variants OK; RBAC, single-replica/`Recreate`, hardened Pod, read-only shipped config, journal-mode values |
| Linux amd64 binary | `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build` | Produced, checksum recorded below |
| Container image | `docker buildx build --platform linux/amd64` | Built from digest-pinned distroless base; Trivy image scan: 0 vulnerabilities, 0 secrets |
| CI supply chain | `.github/workflows/ci.yaml` | Actions pinned by SHA; Trivy pinned by digest; Python tooling installed with `--require-hashes` (`scripts/requirements.txt`) |

Evidence of behavior (not a cluster pass):

- `cmd/kubeflow-admin/integration_test.go`: full HTTPS flow through real HTTP handlers, real OIDC code+PKCE verification against a fake TLS issuer/JWKS, real SQLite store, real operation worker, and fake Kubernetes APIs. Covers anonymous 401, login, callback, `/readyz`, page render, missing-CSRF 403, preview → submit → observed success, and logout invalidation. `-check-db` ownership reporting is tested, including the exit-3 path (`store.ErrLocked`).
- `internal/auth`: PKCE/state/nonce, issuer/audience/signature/expiry rejection, single-use login attempts, hashed sessions, CSRF/same-origin, logout revocation.
- `internal/kube`: namespace/Profile scope, conditional patches, container/volume preservation, protected workspace mount, storage path boundaries, RPC/claim binding, GPU preset removal.
- `internal/operations`: durable checkpoint before every write, idempotency, one active operation per target, no restart replay, session revoke halts queued and running work, shutdown accounts for every accepted operation.
- `internal/store`: durable records, single-use `Take`, concurrent-claim uniqueness, exclusive single-writer lock, WAL/DELETE migration, connection replacement synchronization, integrity checks. Linux store tests also passed in a linux/amd64 container, including the BeeGFS filesystem policy; this does not test a live BeeGFS mount.

Artifacts:

- Binary: `bin/kubeflow-admin-linux-amd64`
- SHA-256: `4bd887eb9afac295cf1a878cb97c170f4db9326f903b057d9a1660b6f283fb60` (extracted from the published image)
- Published image: `docker.io/muratshagirov/kubeflow-admin:0.1.0` (linux/amd64)
  - OCI index digest: `sha256:19f3eba52e65952c6812a24c061e0e0bce2e38a4308e48e9cd9ca1549a25ef7d`
  - linux/amd64 manifest digest: `sha256:4615de6e8c0a8f00485c20cc25921892704621ffefb8b5c502ac5b513b87f303`
  - Republished with the fail-closed filesystem policy (explicit `allowUnverifiedFilesystem` opt-in), `umask 0077` file protection, DELETE journaling support, the `-check-db` ownership test mode, provenance and SBOM; verified by digest-pinned `docker pull --platform linux/amd64` and container runs for startup failure without config and `-check-db` flag presence.

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
