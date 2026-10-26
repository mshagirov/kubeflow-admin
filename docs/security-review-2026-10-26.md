# Security review — 26 October 2026

Scope: repository authentication/session/CSRF and authorization boundaries,
browser rendering and typed request handling, Kubernetes mutation identities,
storage reclaim/binding, durable receipts and locks, configuration, deployment
permissions, build inputs and dependencies. Claude CLI with Opus 5.5/high effort
performed read-only reviews; the implementation agent made the fixes. This is
not a security guarantee or evidence of live-cluster acceptance.

The final Opus 5.5 recheck returned **PASS**, with no remaining verified
security finding. Its optional PV-side legacy-class alias guard was also added:
reclaim refuses such annotations before deletion and during convergence.

## Findings addressed

- **Cross-namespace retained-data exposure:** PVC deletion no longer makes a
  Retain PV Available implicitly. Reclaim requires the separate `pvReclaim`
  feature, per-plan `confirmPVReclaim`, isolated-class attestation and patch
  grant. Missing gates refuse the request before deletion; normal deletion
  leaves Retain volumes Released. Reclaim checks exact deleted-claim and PV
  identities, current resourceVersion and unchanged spec, refuses pending and
  replacement claims, patches only claimRef and verifies Available. Partial
  failures preserve deletion receipts and locks with no rollback/replay.
- **Unreserved static registration:** requires dedicated classes listed in
  `storage.isolatedStorageClasses` and refuses existing unbound claims. The
  list defaults empty and is explicitly an operator attestation, not admission
  enforcement. Install and test the supplied or equivalent policy before
  enabling it. Dynamic PVC creation in those classes is refused: use exact-PV
  binding instead.
- **Deprecated storage-class annotation bypass:** claim checks use the PV
  controller's annotation-first class semantics. The admission example denies
  `volume.beta.kubernetes.io/storage-class` outright, including missing, empty
  and conflicting spec-class combinations. Ordinary and defaulted tenant
  claims must also be tested for denial. Never make these classes default.
- **Permission separation:** storage deletion grants delete only. The new
  `pv-reclaim` overlay separately grants PV patch. RBAC cannot restrict fields;
  protect the identity and use admission for additional field restrictions.
- **Disposable database checks:** unnecessary service-account token mounts are
  disabled and CPU/memory requests and limits are specified.

## Local verification and limits

- Go 1.27.3 race tests (`-p 1`), ordinary tests and vet; Node logic/navigation
  regressions, Python/API parity tests and OpenAPI validation.
- Go vulnerability scan with the fixed Go 1.27.3 toolchain: no vulnerabilities.
  The workstation default Go 1.27.2 was flagged for GO-2026-6809; the Dockerfile
  and CI already pin Go 1.27.3. Do not build a release with the older toolchain.
- Manifest invariant checks include 13 variants, default-off reclaim, the
  separate permission grant and bounded/tokenless database-check examples.
- Kubernetes 1.32 schema validation passed for the admission and database-check
  examples. An isolated temporary CEL harness compiled the actual policy's two
  validation expressions and passed 11 tenant/app/controller decision cases.
  This does not replace server-side admission testing in the target cluster.
- Pinned Trivy scans found no HIGH/CRITICAL source dependency, secret or
  manifest findings. Lower-severity Docker HEALTHCHECK and base-namespace
  flags are handled by Kubernetes probes and Kustomize respectively. Registry
  trust is site policy, not established by a repository name or image tag.
- All Kubernetes resource observations are point-in-time, not a cross-object
  atomic usage fence. External claim creation must be controlled by admission;
  available retained data is not erased and remains sensitive. Cluster policy,
  tenant denial, storage permissions, actual controller behavior and mounts
  still require disposable operator-run acceptance. No live cluster was used.

The historical UI changes already in the working tree were preserved. The new
presentation request changes the storage-delete heading to the exact resource
name via `textContent`; no HTML interpolation is used.
