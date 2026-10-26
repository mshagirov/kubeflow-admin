# Verification record

Records what has been **proven locally** and what still requires the operator's live acceptance. Fake Kubernetes clients and local OIDC fixtures do not simulate Kubeflow reconciliation, CSI provisioning, scheduling, admission or Istio.

## 1.0.0 — standalone Released-PV reclaim and bound-PVC field order (26 October 2026)

**Published and registry-verified:** `1.0.0` and `latest` resolve to the same
multi-platform index below. Source commit and tag are pushed. No live cluster
mutation or application rollout is performed by these checks.

- Storage details offer a separately gated **Reclaim PV** action for old
  Released PVs. The typed `pv.reclaim` plan requires a reason, exact PV-name
  confirmation, current UID/resourceVersion, Retain policy and absent old claim.
  The drawer also requires a retained-data exposure acknowledgement.
- Existing writes/`pvReclaim`, isolated-class attestation and PV patch RBAC
  remain required. No safety gate is bypassed for shared tenant classes.
  Current/replacement/referencing and pending matching claims (including beta
  class aliases) block reclaim; protected old claim namespaces are refused.
- After an authorized checkpoint, the exact PV and dependencies are re-read.
  Final metadata-only RV drift is refused before the one conditioned claimRef
  removal. Kubernetes updates status; success requires observing Available and
  recording its receipt. No status/backend/finalizer edits or resource deletion.
- Policy refusals return nonretryable validation errors; incomplete/failed
  reference checks return an explicit nonretryable unavailable response. The
  standalone warnings do not imply PVC deletion. Uncertain writes retain the
  PV lock, with no rollback/replay and typed restart guidance.
- The optional post-delete reclaim reuses the same patch/convergence helper
  without changing its opt-ins. The new operation shares the `pv:<name>` lock
  with existing edit/delete/binding operations.

Local verification includes Go 1.27.3 tests/race/vet/build and vulnerability
checks, Node logic/navigation regressions, 16 Python/API parity tests, OpenAPI
validation and 13 manifest variants. All 37 real Chrome baseline acceptance
checks passed after updating the synthetic fixture's feature projection; the
new reclaim drawer/payload is covered by dedicated navigation and HTTP tests,
not by a claim of real-cluster or full real-browser reclaim acceptance.
Claude Opus 5.5/high effort reviewed the diff; its refusal-classification,
final-RV and standalone-warning findings were fixed with regression tests.
The final Opus 5.5/high recheck returned **PASS** with no remaining security or
correctness blocker. Definite single-patch API rejections deliberately remain
conservative needs-review outcomes, with no automatic retry or lock release.

### 1.0.0 publication receipt

- Add the bound-PVC field order: **Registration template** → **Storage
  class** → **Selected PV**. Other storage actions retain their prior order.
- Source commit: `062f1deda728f659f7c2695036d6d6b6862a9afd`, pushed to `main`.
  Annotated tag object `52108a91d535b6475490d6824eadddde9557f87d` resolves to
  that commit. The GitHub `1.0.0` release is marked **Latest**.
- Source CI: [40291628729](https://github.com/mshagirov/kubeflow-admin/actions/runs/40291628729)
  for `main` and
  [40292571143](https://github.com/mshagirov/kubeflow-admin/actions/runs/40292571143)
  for `1.0.0`. Security and test jobs passed on both.
- Built once from the exact `git archive 1.0.0` source with Go 1.27.3, SPDX SBOM
  and SLSA provenance. `1.0.0` and `latest` were pushed to the same index by
  the same build, not rebuilt.
- Source CI passed pinned Trivy 0.76.0 HIGH/CRITICAL dependency, secret and
  misconfiguration gates. Each published runtime manifest was then separately
  selected and scanned with the same pinned Trivy image; both passed the
  vulnerability and secret gates with no HIGH/CRITICAL findings.
- Both architectures have UID/GID `65532:65532`, source/version labels
  `062f1de` / `1.0.0`, and the byte-identical MIT LICENSE
  (`sha256:77ffde667034153e72bcaaa23279029510bf5142d3a7a7f210ff3e4025d993b6`).
  A network-disabled, read-only-root, dropped-capability, no-new-privileges
  startup check failed closed without configuration.
- AMD64: `sha256:ada24433994eb3b12635544d9ddd41f0c903fe8e1cd212cfd6877b560327f3fc`.
- ARM64: `sha256:f83de62f7b1f7445eb1442bb090ee3520892c43fbc5cc5fd6892b3002cdc3107`.
- Index: `sha256:16a0423aebba849f5c78c716e6203845403b116b2a19767c63f99c5ba1bbd388`.
  Attestation references: `sha256:77fdcfdce8735a9af88af343336ca69eb8e01519853b4f41b16bb0e285e0aea6`
  and `sha256:6c767aa5cd5c952c097fcfe7e71ff1671475a125f6e7bf9a1d160a41f45d48ec`.
- The registry tag listing contains `1.0.0` and `latest` at that index. This
  supersedes the earlier `1.0.0` publication from source `026db2e` with image
  index `598dcaa0b9db2e0ac1bf59d747cc728fc9026c8bcc3c66b9cf9deff8afbb5c8b`
  and tag object `8546b9a7357970ca01836a65da60b5d41e3bd674`; the earlier index
  did not contain the bound-PVC field order. No live cluster used it. The
  ignored `deploy/site` overlay is not part of this source commit and must be
  repinned to the new index before an operator rollout. The `0.4.5` registry
  receipt below is historical.
- All 37 real Chrome synthetic-fixture acceptance checks passed against the
  committed assets, including the bound-PVC field-order assertion. Live reclaim
  still requires the operator-enforced isolated class, admission policy,
  `pvReclaim` opt-in and `pv-reclaim` patch RBAC.

```yaml
image: docker.io/muratshagirov/kubeflow-admin@sha256:16a0423aebba849f5c78c716e6203845403b116b2a19767c63f99c5ba1bbd388
```

## 0.4.5 — explicit Retain reclaim and security isolation (26 October 2026)

**Published and registry-verified:** `0.4.5` and `latest` resolve to the same
multi-platform index below. No live cluster mutation or deployment.

- Storage-delete headings name the exact resource using `textContent`.
  Earlier sidebar padding and empty-legacy-template action gating are retained.
- Optional Retain reclaim is off by default and requires its own feature,
  per-plan acknowledgement, isolated-class attestation and separate PV patch
  grant. Default deletion leaves the PV Released. Only the exact deleted
  claim's stale claimRef is removed; Available must be observed before success.
- Unreserved registration requires operator-enforced isolated classes. Dynamic
  PVCs in those classes are refused; existing unbound and replacement claims
  block exposure. Deprecated class annotations cannot bypass the claim checks
  or the supplied admission policy. Reclaim also refuses PV-side aliases.
- Partial outcomes preserve receipts and locks without rollback/replay. The
  deletion overlay remains delete-only; the new `pv-reclaim` overlay is separate.
- Disposable database-check examples no longer mount service-account tokens
  and have bounded CPU/memory. A flaky pre-existing eviction test deadline was
  increased so race instrumentation reaches the intended post-eviction checks.

See [security review](security-review-2026-10-26.md) for findings, fixes,
independent Opus 5.5/high review, local checks and deployment prerequisites.
Go 1.27.3 tests/race/vet/vulnerability checks, browser logic/navigation, API
parity, Python tests, OpenAPI and 13 manifest variants passed. Admission
examples passed Kubernetes 1.32 schema checks and an isolated CEL harness.
Source Trivy HIGH/CRITICAL dependency, secret and misconfiguration scans passed.
These checks are not live tenant-isolation, controller or mount acceptance.

### 0.4.5 publication receipt

- Source commit: `4772a76b8f8c47c04a33dbf39c2936aa9406c5da`, pushed to `main`.
- Source CI: [40280987954](https://github.com/mshagirov/kubeflow-admin/actions/runs/40280987954).
  Both security and test jobs passed, including multi-architecture compilation.
- Built once from the committed Git archive with Go 1.27.3, SPDX SBOM and SLSA
  provenance; promoted the exact scanned OCI artifacts without rebuilding.
- The archived source passed pinned Trivy 0.76.0 HIGH/CRITICAL dependency,
  secret and misconfiguration gates. Each runtime architecture was separately
  selected and verified in the scanner output; both passed vulnerability and
  secret gates. Scanning the full OCI layout with a platform flag initially
  selected AMD64 twice, so the final gates used single-platform layout views
  of the exact two runtime manifest digests instead.
- Both architectures have UID/GID `65532:65532`, correct source/version labels,
  byte-identical LICENSE files, and verified SPDX/SLSA attestations with no
  workstation paths. Network-disabled, read-only-root, dropped-capability,
  no-new-privileges startup checks failed closed without configuration.
- AMD64: `sha256:9f73886e46ef62d9df3fd3631490b71c04a708f46504ca80f49ee6b7394ad6b2`.
- ARM64: `sha256:79cb83d8a9147406675a790b08f846b0b7dd5fb72233ec9330e209ced1925cf3`.
- The registry index, both runtime digests and attestation references match the
  offline artifact. `0.4.5` and `latest` match; `0.4.4` remains unchanged.

```yaml
image: docker.io/muratshagirov/kubeflow-admin@sha256:a6e48cd624ae42081f5dc243a540d1abddfd8acb062bcddb10b033404d2fc965
```

This receipt is documentation-only; the image source label remains the source
commit above. Reclaim and isolated storage still require the operator acceptance
and admission prerequisites in [storage](storage.md).

## 0.4.4 — template-driven storage provisioning (22 October 2026)

**Published and registry-verified:** `0.4.4` and `latest` resolve to the same
multi-platform index below. Existing `0.4.3` is unchanged. No deployment or live
storage mutation has been performed.

- Separate unclaimed NFS/BeeGFS PV registration and template-bound PVC creation;
  independent template-controlled capacities/requests and access modes. New NFS
  templates allow an approved root itself or descendants; legacy templates retain
  their original semantics. NFS UID/GID are required nonzero intent-only metadata.
- Optional dedicated PodDefault creation and explicitly confirmed guarded
  overwrite only within PVC creation. Unused-PVC deletion previews and confirms
  related PodDefault cleanup, including shared/external warnings, orphan
  propagation and the 32-object bound. No standalone PodDefault management,
  automatic PVC rollback, PodDefault recreation, force or finalizer removal.
- Fresh scope/policy and UID/resourceVersion checks, fail-closed discovery across
  served versions, incomplete-list refusals and canonical backend overlap checks.
  Durable append-only mutation receipts retain exact known UIDs through partial
  outcomes, preview expiry and restart; uncertain operations retain locks.
- Drawer body top padding increased from 4px to 16px. Registration payloads use
  the active name input and exact selected-PV identity; namespace and backend
  fields follow the selected workflow/template.
- Release helper now archives exact tracked HEAD, excludes ignored local files,
  sets version/revision/source/license labels and tags version plus `latest`.
  Publication still requires independently scanned artifacts; the direct helper
  is not an offline-artifact promotion gate.

Final local checks passed: serialized Go 1.27.3 race suite, vet/build, module
verification and `govulncheck` (no vulnerabilities); Node regressions, API parity,
16 Python tests, OpenAPI validation and twelve manifest invariant variants.
Real Chrome acceptance passed **37/37**, including NFS/BeeGFS/bound-PVC forms,
PodDefault gating/overwrite/cleanup acknowledgements, partial receipts and drawer
spacing. No skips, JavaScript errors or page overflow at 390px/1280px.

Independent security review produced regression tests for discovery, scope
changes at checkpoints, pagination, backend aliases, provenance and post-cleanup
rejections. All pass; the final race rerun includes the completed fixes. A further
empty-discovery-list test and preserved inventory-warning check also pass.
Tracked/nonignored source review found no matching site-specific metadata,
private keys or credential-shaped tokens; ignored site configuration remains
outside Git and image build contexts. This is not a proof of absence of every
possible secret. The temporary planning document was removed; operator contracts
remain in [storage](storage.md), [deployment](deployment.md) and
[live acceptance](ui-workflow-acceptance.md). The learning guide remains exactly
15 lessons. New features remain disabled by default; installed admission/mount
behavior still needs the disposable operator-run acceptance checklist.

### Publication receipt

- Source commit: `a50742b4a13cd8c98f6feb4ced71589e803781e5`.
- Source CI: [39796210788](https://github.com/mshagirov/kubeflow-admin/actions/runs/39796210788)
  passed both test and security jobs before publication.
- The exact staged source tree was archived and passed pinned Trivy 0.76.0
  HIGH/CRITICAL dependency, secret and misconfiguration gates; its tree was
  checked against the committed source before the release build.
- Built once offline from archived committed source with Go 1.27.3, SPDX SBOM and
  provenance. Both AMD64/ARM64 runtime artifacts passed HIGH/CRITICAL
  vulnerability/secret scans, non-root UID/GID `65532:65532`, correct source/version
  labels and byte-identical `/LICENSE` checks. Attestations contain no workstation
  or private temporary-directory paths. Both architectures failed closed without
  configuration under network-disabled, read-only-root, dropped-capability,
  no-new-privileges smoke tests.
- Linux AMD64 manifest:
  `sha256:e9bd3f5573d6923874f0962950c5cd481b7010199bb02b803b4a2559da58a1e5`.
- Linux ARM64 manifest:
  `sha256:eb6481b6805b9cbf1457eecfb3c26233ec81368bb18e0738755237fc63eb96c2`.
- Pushed the already-scanned OCI artifacts without rebuilding. Registry index
  and both platform digests exactly match the offline artifact; SPDX/provenance
  attestations were verified for both. `0.4.4` and `latest` match, and the previous
  `0.4.3` index was compared before/after and is unchanged.

```yaml
image: docker.io/muratshagirov/kubeflow-admin@sha256:6ffb6676ef6a385fec2a6aa79f865e474cbe51db761cf7872063a40abe746caa
```

This receipt is a documentation-only follow-up: no new image build, CI run or
deployment is required. The published image source label remains the source
commit above; this receipt commit uses `[skip ci]`.

## 0.4.3 — Profile and Notebook panel layout (22 October 2026)

Published and registry-verified: `0.4.3` and `latest` resolve to the same image
index below. Existing `0.4.2` remains unchanged.

- Profiles: the Workspace ownership information panel precedes the Profiles table
  in a full-width vertical layout instead of a side-by-side split.
- Notebooks: the management button stays at the right edge of the panel heading,
  vertically centered. Description and policy explanations wrap in the left grid
  column; button gating, exact target selection and drawer behavior are unchanged.
- Sidebar: removed the WORKSPACE nav-label element and its unused styles.
- Personal review confirmed no backend, API, permission or feature-flag changes.
  Regression coverage checks Profile panel order/full-width geometry and Notebook
  action right/center alignment at narrow/wide viewports.

Local verification passed: Node regressions, full serialized Go 1.27.3 race suite,
vet/build, module verification, `govulncheck` (no vulnerabilities), source Trivy
HIGH/CRITICAL vulnerability/secret/misconfiguration gates, API parity, seven Python
tests, OpenAPI validation, shell syntax and eleven strict Kubernetes 1.32 manifest
variants. Real-Chrome synthetic acceptance passed **32/32**, including the new
panel geometry assertions, with no JavaScript errors or page overflow at
390px/1280px. No deployment performed.

### Publication receipt

- Source commit: `6009e5c154078ed037d68392982ff8723f90a470`.
- Source CI: [39737209030](https://github.com/mshagirov/kubeflow-admin/actions/runs/39737209030)
  passed test and security jobs before promotion.
- Both offline runtime images passed Trivy HIGH/CRITICAL vulnerability/secret
  scans. Non-root UID/GID `65532:65532`, source/version labels, byte-identical
  `/LICENSE` and read-only-root/network-disabled smoke tests passed on both.
- Linux AMD64 manifest:
  `sha256:5d8463c6fa17892261cdb577304a2e9c1e73bd14e258fd77be9b1aab9cd17bfc`.
- Linux ARM64 manifest:
  `sha256:b81de1c43525b053d5f105eee5542d7820017caafe273d8dad990cb4bb29976c`.
- Existing verified local artifacts were pushed without another build. Registry
  platform manifests match the scanned artifacts, both platform SPDX/provenance
  manifests were verified, and `0.4.3`/`latest` resolve to the exact same index.
  The `0.4.2` registry index was compared before/after and is unchanged.

```yaml
image: docker.io/muratshagirov/kubeflow-admin@sha256:02fde5adfb666ab5058ef637d5f195b21c62f763f113b861a84a17d1517c834d
```

### Documentation-only audit

Current UI guide, Nodes/Pods columns, Overview, notifications, storage gates,
recovery, all-profile scope and live acceptance were checked against source.
Contradictory unsupported-storage claims and duplicate checklist IDs were fixed;
historical plans/receipts remain identified as historical. The learning guide was
consolidated to **15 lessons**, retaining authentication, durability, safety,
storage, CI/publication and operator deployment/recovery topics. README links and
lesson count were updated. Local Markdown file links, code fences, checklist IDs,
lesson numbering and whitespace checks passed.

These docs/receipt changes require no image rebuild or separate CI run; the
docs-only commit uses `[skip ci]`. They do not change the published source label
or binaries. The application image promotion above belongs to the previously
authorized UI release, not a new build for the documentation edits.

## 0.4.2 refresh — responsive Nodes table (22 October 2026)

**Latest alias:** `docker.io/muratshagirov/kubeflow-admin:latest` and `:0.4.2`
were both verified to resolve to the refreshed index
`sha256:01761b157722b02f90a99a8262cfc1f4dec14c31f56649d8002acc42dfa2055e`.
The alias was promoted by digest without rebuilding or changing versioned tags.
`make image-push` now publishes version and `latest` together in one build;
publishing-helper tests cover both tags, registry ports and invalid inputs.

The operator authorized reusing `0.4.2` for this follow-up. The tag now points to
the refreshed digest below; existing deployments pinned to the original digest
stay unchanged. The original receipt in the following section is historical.

- Personal review confirmed the changes affect only Nodes table CSS and its
  browser assertion, plus the requested uppercase Overview eyebrow and its tests.
  Overview now reads "CLUSTER AT A GLANCE" in both initial and navigated views.
  The Profiles page management button is labeled "New".
  The Storage page entry button is labeled "New"; the drawer remains unchanged.
  Namespaces follows Storage in the sidebar navigation.
  The old 1560px minimum is replaced by an 800px narrow-view
  fallback; above 1200px the eight-column table uses the available width, smaller
  horizontal padding, explicit column proportions and wrapping. No columns,
  Resource Details, actions, backend behavior, permissions or feature flags change.
- Node regressions, full serialized Go 1.27.3 race suite, vet, build, module
  verification, whitespace checks and `govulncheck` passed (no vulnerabilities).
- Real-Chrome synthetic acceptance passed **32/32**, with no JavaScript errors
  or page overflow at 390px/1280px. A new assertion verifies the Nodes table itself
  has no horizontal scroll at the wide viewport, not just the surrounding page.

No cluster deployment was performed.

### Refreshed publication receipt

- Source commit: `143eba84bbcfa7cbef3cb4341fc06ec67611e4be` (includes all label,
  navigation and table-fit follow-ups).
- Source CI: [39733233969](https://github.com/mshagirov/kubeflow-admin/actions/runs/39733233969)
  passed test and security jobs. Earlier superseded runs were cancelled by the
  workflow concurrency policy as subsequent UI changes were pushed.
- Final serialized Go 1.27.3 race suite, vet and vulnerability scan passed.
  Final real-Chrome rerun passed **32/32**, with no JavaScript errors or overflow.
  The preceding browser attempt reached its external timeout after responsive
  measurements; it was not counted as a pass, and no production timeout changed.
- Source and both runtime Trivy HIGH/CRITICAL vulnerability/secret gates passed;
  source misconfiguration scanning, eleven strict manifest variants and OpenAPI
  checks passed. Non-root identity, revision/version labels, byte-identical
  `/LICENSE`, and read-only-root/network-disabled smoke checks passed on both
  platforms.
- Linux AMD64 manifest:
  `sha256:81384c8ee88485f570c606ec5c9c91c4662e0dec914d5cb0d4ca33efdb901894`.
- Linux ARM64 manifest:
  `sha256:db083505ba52be3b839d0d65be11b3ed16fd6b99f2b4d5aa1dc53a2f29bad947`.
- Registry platform manifests exactly match the offline-scanned images. SPDX
  SBOM/provenance attestations were verified for both; `0.4.1` is unchanged.

Current `0.4.2` immutable deployment image:

```yaml
image: docker.io/muratshagirov/kubeflow-admin@sha256:01761b157722b02f90a99a8262cfc1f4dec14c31f56649d8002acc42dfa2055e
```

This supersedes the original `0.4.2` tag digest
`sha256:9dd818736462bdfced3eb760388c4e63ba984e0b6c5a9833fac05fb6fc95fcdf`.
Update the site image pin explicitly to receive these changes. Receipt-only
documentation commits do not change the published binaries.

## Original 0.4.2 — Overview, notifications and simplified tables (22 October 2026)

Published and registry-verified as `docker.io/muratshagirov/kubeflow-admin:0.4.2`.

- Overview now contains only Workspace Health: larger Nodes card beside Profiles,
  Notebooks, Pods / PVCs, PVs, Events in a three-column, two-row wide layout.
  The eyebrow is "Cluster at a Glance" with no Overview subtitle; other pages
  restore their subtitles, including through client navigation and Back/Forward.
- Notifications default to one visible card with an expandable count. Errors
  sort first, newest-first within each priority group; repeats stay deduplicated.
  Existing dismissal persistence and info/success expiry are preserved.
- Only the Pods table drops CPU/memory request/limit columns. Only the Nodes
  table drops OS-Image, Kernel-Version and Container-Runtime. Resource Details,
  backend projections, Node Pod scope, actions and permissions are unchanged.
- Personal review, Node regressions, full serialized Go race suite, vet, build,
  module verification and whitespace checks passed. Configured real-Chrome
  synthetic acceptance passed **32/32**, with no skips, JavaScript errors or
  horizontal overflow at 390px/1280px. Added assertions verify exact table headers,
  row alignment and preservation of all seven removed fields in Resource Details.

No new cluster deployment or live storage mutation was performed. `0.4.1` remains
unchanged, verified by comparing its registry index before and after publication.

Release-toolchain rerun: `GOTOOLCHAIN=go1.27.3` full serialized race tests and vet
passed; `govulncheck` reported no vulnerabilities. An initial unpinned scan used
the host's Go 1.27.2 and reported GO-2026-6809; the already digest-pinned release
Dockerfile and CI use fixed Go 1.27.3. API parity, all five Python tests, OpenAPI
validation and all eleven manifest variants passed.

### Publication receipt

- Source commit: `f70f0f9d1de2f59bcf4508ebc03b3fdf6ea4b4de`.
- Source CI: [39730380234](https://github.com/mshagirov/kubeflow-admin/actions/runs/39730380234),
  test and security jobs both passed.
- Multi-platform index:
  `sha256:9dd818736462bdfced3eb760388c4e63ba984e0b6c5a9833fac05fb6fc95fcdf`.
- Linux AMD64 manifest:
  `sha256:3c8f4e6274dd6377a2f5c6506dc80e63cbafc0d1e9bbeab4a7c010a286409a2d`.
- Linux ARM64 manifest:
  `sha256:1fface146da84020eb9e98af09322eb1b5659cb72cd7f4fff1df95816eb8451d`.
- Registry platform manifests exactly match the offline-scanned artifacts.
  SPDX SBOM and provenance attestations were verified for both platforms.
- Digest-pinned Trivy 0.76.0 source and both runtime scans passed the
  HIGH/CRITICAL vulnerability/secret gates; source manifest misconfiguration
  scanning also passed. All eleven variants passed strict Kubernetes 1.32
  schema validation with no skips.
- Both runtime images have source/version labels, non-root UID/GID `65532:65532`
  and byte-identical `/LICENSE`. Both failed closed without configuration in
  network-disabled, read-only-root containers with all capabilities dropped.

Immutable deployment image (site pin is an operator deployment step):

```yaml
image: docker.io/muratshagirov/kubeflow-admin@sha256:9dd818736462bdfced3eb760388c4e63ba984e0b6c5a9833fac05fb6fc95fcdf
```

Receipt-only documentation commits do not change the published binaries.

## 0.4.1 — storage, Notebook gates and Node scope (22 October 2026)

This work is published as `0.4.1`, not in the historical `0.4.0` digest below. It adds name-only storage
details controls, separately gated restricted PV/PVC YAML editing and unused-only
deletion, plus explicit Notebook management gate explanations. The Node scoped
Running-Pods view was implemented after the scheduled wake at 05:21 Singapore
time on 22 October. It links the exact count across namespaces to URL-backed
Node/Running filters on the Pods page, with visible chips and filter reset.
The operator chose
restricted YAML rather than full manifests, and refused in-use storage deletion.
Storage implementation/review agents used OpenCode Go DeepSeek V4.1 Flash with
high thinking; the scheduled Node agent used max thinking as requested. The lead
personally reviewed the combined changes. No deployment, feature enablement
or cluster mutation is authorized by the local implementation work.

The new editing and deletion overlays retain false-by-default feature flags,
separate patch/delete grants and no finalizer, namespace-delete or cluster-admin
permissions. Eleven manifest variants render and pass the offline grant checks.
Focused integration evidence before the scheduled Node work:

- The lead personally reviewed backend, lock/fence, HTTP and UI changes, then
  corrected malformed reference checks, digested bound-volume/scope evidence,
  strict YAML/label handling, stale async plan responses and typed safe errors.
- An independent read-only storage safety review found no remaining P1 issue.
  Its P2 recovery-guidance finding was corrected: uncertain storage edits now
  warn about an applied Delete reclaim policy or pending CSI/filesystem resize,
  retain locks and explicitly forbid replay, shrinking or forced cleanup.
- Lead-run focused race tests passed for Kubernetes, store, operations, config,
  model and web packages; both Node regression scripts passed. API parity,
  five Python tests, OpenAPI validation and eleven manifest variants passed.
- Configured real-Chrome synthetic acceptance passed **31/31** checks through
  the storage frontend agent and **32/32** after the Node feature through the
  scheduled agent. Final lead-run combined verification is recorded below;
  no synthetic fixture result proves live storage behavior.

### Final local verification

- Lead-run `GOTOOLCHAIN=go1.27.3 GOMAXPROCS=2 go test -mod=readonly -race -p 1 -count=1 ./...`
  passed all ten packages. Both Node scripts, Go vet, module verification and a
  clean non-CGO production build passed; no production timeout was increased.
- Lead-run configured real-Chrome acceptance passed **32/32**, with no skips,
  JavaScript errors or horizontal overflow at 390px/1280px across all seven
  resource pages. This includes exact Node Running-Pod scope and restricted
  storage edit/delete orchestration, not live Kubernetes/storage behavior.
- API parity (22 routes), all five Python tests, OpenAPI validation, shell syntax,
  eleven manifest variants and strict Kubernetes 1.32 schema validation passed.
- `govulncheck v1.9.0` reported no vulnerabilities. Digest-pinned Trivy 0.76.0
  found no HIGH/CRITICAL dependency vulnerabilities, detected secrets or manifest
  misconfigurations in the staged release-source snapshot.

GitHub CI [`39679933717`](https://github.com/mshagirov/kubeflow-admin/actions/runs/39679933717)
passed both test and security jobs for source commit
`cb1df16edb52c7b0b70b2dff308ae58728a2adbc`, including race tests, vet/build,
OpenAPI/manifests, source security scans and AMD64/ARM64 image compilation.
Both offline runtime images passed Trivy HIGH/CRITICAL vulnerability and secret
scans. Revision/version labels, non-root UID/GID `65532:65532` and the byte-identical
`/LICENSE` were verified. Both platforms also failed closed without configuration
under read-only-root, network-disabled container smoke tests.

### Published image

- Tag: `docker.io/muratshagirov/kubeflow-admin:0.4.1`.
- Multi-platform index:
  `sha256:743e39bf0a7ef6ca36f9c64b63cc4652670002c0a3a4b0219fc16b5d39bd5280`.
- Linux AMD64 manifest:
  `sha256:2b2893664d06ba61fc44474e8bc9c1969d19fe19d6a9c74fc3d999ffe19ffbfd`.
- Linux ARM64 manifest:
  `sha256:c8289f408bd2a71dd0a9bfba327a2aede567465c99a7c9f42bdeb0f70b24a879`.
- Registry platform manifests exactly match the offline-scanned artifacts.
  SPDX SBOM and provenance attestations were verified for both platforms.
  The `0.4.0` registry index was re-read and confirmed unchanged.

Pin the immutable index in the site overlay, on one YAML line:

```yaml
image: docker.io/muratshagirov/kubeflow-admin@sha256:743e39bf0a7ef6ca36f9c64b63cc4652670002c0a3a4b0219fc16b5d39bd5280
```

No deployment or live cluster workflow was performed. Storage editing/deletion
remain separately disabled by default and need reviewed feature flags/RBAC.
Notebook management still requires writes and a verified controller-contract
opt-in; the UI now explains missing gates instead of failing silently. Receipt-only
documentation commits do not change the published application binaries.

## UI and deployed-quota follow-up — 22 October 2026

This follow-up is published in the refreshed `0.4.0` image identified below.
Live-cluster acceptance has not been performed. Older image receipts are historical.

- Profile quota edits support the existing `cpu`/`memory` aliases and existing
  non-reserved raw device keys under their original names. Editing one value
  does not automatically rename keys or add duplicate canonical limits.
  New raw device keys remain unsupported; approved canonical keys retain the
  existing explicit authoring policy.
- Unmanaged keys, including `count/<resource>` object quotas and storage-class
  quotas, remain read-only and are preserved literally. They are not rejected
  against an incomplete duplicate of Kubernetes' quota-key validator.
- Bare device declarations such as `nvidia.com/gpu` need a warning: standard
  Kubernetes device accounting uses `requests.nvidia.com/gpu`. Matching
  `status.hard` is not proof of enforcement. No migration is performed.
- UI interaction changes consolidate Profile information into Profile Summary,
  move Notebook/Pod/Node detail opening to resource-name clicks, and keep the
  Notebooks table limited to Notebook custom resources. A Notebook's details
  offer its preselected management workflow.

Implementation and intermediate review used OpenCode Go DeepSeek V4.1 Flash
with high thinking; the lead personally reviewed the combined changes and
corrected quota classification and Notebook cross-target edit handling.

Verified before publication:

- Both JavaScript regression scripts passed.
- `GOTOOLCHAIN=go1.27.3 GOMAXPROCS=2 go test -mod=readonly -race -p 1 -count=1 ./...`
  passed all ten packages; `go vet -mod=readonly ./...` and whitespace checks passed.
- Configured real Chrome/Playwright acceptance passed **26/26** checks, with
  zero failures/skips and zero horizontal overflow at 390px and 1280px on all
  seven resource pages. Coverage includes consolidated summaries, exact legacy
  quota-key payloads, name-driven drawers, Notebook-only rows, preselected
  management, write/controller gates, focus restoration and clearing stale edits.
- These browser checks use a loopback-only synthetic fixture, not a cluster.
  Notebook resource/image edit fields intentionally start blank (preserve);
  mounts remain unchanged unless replacement is explicitly selected. No
  individual Notebook-details API or full editable-configuration prefill exists.

The first GitHub CI run for `e42d56f` failed in the security job: the pinned
`govulncheck v1.1.4` panicked in its bundled `x/tools v0.29.0` SSA builder with
`unexpected expr: *ast.KeyValueExpr`. The passing local vulnerability scan used
`govulncheck v1.9.0`, not that older CI pin. The follow-up CI fix pins `v1.9.0`
and invokes its installed path explicitly; reachability scanning remains enabled
and any error or finding still fails the job. Replacement GitHub CI
[`39636701613`](https://github.com/mshagirov/kubeflow-admin/actions/runs/39636701613)
for `d418536` passed both security and test jobs, including reachable-vulnerability
and source scans, race tests, vet/build, manifest/OpenAPI checks and both image
architectures. Optional locally emulated Linux scanner runs timed out and are
not counted as evidence; the native Linux CI scanner completed successfully.

Additional local release checks passed: module verification, API parity and all
five Python tests, OpenAPI validation, nine manifest variants and strict
Kubernetes 1.32 schema validation. Digest-pinned Trivy 0.76.0 found no
HIGH/CRITICAL vulnerabilities, secrets or manifest misconfigurations in the
tracked-source snapshot, and no HIGH/CRITICAL vulnerabilities or secrets in
either offline runtime image. Both platforms' non-root identity, source label
and byte-identical `/LICENSE` were verified. The image retains application
source label `e42d56f`; `d418536` changes only CI and this receipt, not runtime
source or image inputs.

### Published image

- Tag: `docker.io/muratshagirov/kubeflow-admin:0.4.0` (deliberately reused).
- Multi-platform index:
  `sha256:52b7cf51cc709d254e1f656c6cea8b9dcf93e3fd666300b46764da5b15df7245`.
- Linux AMD64 manifest:
  `sha256:6b1f57e63204edbcd9267fd0199e5aaa574c4cf1cd6f14f68c37297ced870ce6`.
- Linux ARM64 manifest:
  `sha256:4fafcd2fbdb6871d75a80a1397331cba337c2be0b5912e2302c97fd74d0ba6ab`.
- Registry platform manifests exactly match the offline-scanned artifacts;
  SPDX SBOM and provenance attestations were verified for both platforms.

Pin the fresh immutable index digest in the site overlay, keeping the reference
on one YAML line:

```yaml
image: docker.io/muratshagirov/kubeflow-admin@sha256:52b7cf51cc709d254e1f656c6cea8b9dcf93e3fd666300b46764da5b15df7245
```

No deployment or live cluster mutation was performed. The operator must approve
a disposable Profile/namespace and registered owner, a disposable Notebook and
the StorageClass/static template before live create/delete and storage testing.
Profile deletion remains separately gated and may destroy PVC backing data.

## Profile workflows and resource drawers — 21 October 2026

This release adds unique DNS-1123/63-character Profile names with explicit
dot/underscore suggestions, prefilled quota editing, Profile metadata/quotas,
namespace resource drilldown, resource-detail drawers and clear-filter controls.
The sidebar includes `© 2026 Murat Shagirov` after the source link; the runtime
image also includes the MIT license at `/LICENSE`.

Implementation used **MiniMax M3.1 Flash Free** on OpenCode Go with default
thinking. Intermediate reviews used **DeepSeek V4.1 Flash** with max thinking;
the lead performed final review, integration and additional regression fixes.

Important reviewed boundaries:

- The operator approved **read-only owners** after upstream v1.10 source
  review showed that in-place owner transfer breaks controller reconciliation.
  The API rejects transfers; the app never patches namespace ownership or RBAC.
- Quota forms bind to the observed Profile name/UID, owner and canonical hard
  map. Stale/replaced targets are refused. Integer/zero values round-trip;
  unmanaged quota keys are preserved. The deprecated `profile.quota.change`
  alias requires the same guard; old unguarded clients must reload/upgrade.
  Configuration-digest stability does not waive conservative preview/execution
  UID/resourceVersion checks. An authorized client's desired quota is still an
  explicit change, reviewed in the server's before/after preview.
- Profile deletion is separately disabled by default. Only a conditional
  background Profile DELETE is issued; normal controller/garbage collection
  removes its owned namespace. Both must disappear before success. Namespace
  fences block other app mutations while deletion is unresolved; ambiguous
  outcomes retain the locks and are never replayed automatically.
- Namespace/PV/PVC direct deletion and finalizer removal remain unavailable.
  PVC cascade can irreversibly destroy Delete-policy backing data; retained
  storage and snapshot content follow their own policies. This is not a wipe.
- The Python validation-tools `certifi` pin and hashes were updated to
  `2026.10.19` for GHSA-6q48-6c7w-9gw2, which failed the previous CI run.

No cluster was changed. The disposable-pilot checks in
[UI workflow acceptance](ui-workflow-acceptance.md) remain operator work.
Artifact identity and final verification results are recorded with publication.

### Release verification (21 October 2026)

- `GOTOOLCHAIN=go1.27.3 GOMAXPROCS=2 go test -mod=readonly -race -p 1 -count=1 ./...`:
  all ten packages passed. The initial parallel run hit the existing 500 ms
  auth/plan-cleanup test deadline under concurrent build load; five isolated
  uncached runs passed, followed by the complete sequential-package race run.
  No production timeout was increased.
- `go vet -mod=readonly ./...`, `go mod verify`, both JavaScript regression
  scripts, `bash -n scripts/discover.sh` and whitespace checks passed.
- API parity, five Python tests, OpenAPI validation and all nine manifest
  variants passed. Strict kubeconform validation also passed for every variant.
- `govulncheck@v1.1.4 ./...`: no reachable vulnerabilities found.
- Trivy **0.76.0**, digest-pinned in CI, reported zero HIGH/CRITICAL source
  dependency vulnerabilities, detected secrets or manifest misconfigurations.
  The previous 0.67.2 scanner could not load some current policy modules;
  refreshing the pin removed those compatibility errors without disabling
  policy checks. Ignored local binaries, Git data, virtualenv and private site
  material are not part of the source release scan.
- The Overview explanation panels are removed. `ui.footerText` defaults empty
  and permits bounded, sanitized branding HTML; scripts, inline styles and
  unsafe URL schemes are not supported. Logos are browser requests, not
  server-side fetches. The footer, its safe page projection and CSP have
  regression coverage.
- All **20** configured real-Chrome profile/UI checks passed at 390px and
  1280px, with no horizontal overflow or JavaScript errors. The browser fixture
  now uses real relative timestamps; its previous fixed expiry correctly
  caused the app to block stale plans after the pause. The added expired-plan
  regression confirms the submit control remains disabled and no operation
  is sent. Synthetic checks do not prove real Kubernetes reconciliation.
- Real HTTP/client-go regressions verify that `Retry-After` cannot silently
  replay unsafe Kubernetes writes and turn an uncertain first write into a
  misleading later rejection. GET retries remain enabled; response bodies,
  status codes, authentication and TLS are unchanged.
- Offline reconciliation requires an explicit valid mode and fails before
  config/DB/server startup for incomplete or contradictory recovery flags.
  Successful GET responses without an object UID also fail closed. The
  stopped-app, backup, same-cluster and immutable-evidence requirements are
  documented in [operations](operations.md#offline-reconciliation-of-a-profile-deletion).

### Published image receipt

- Application source commit: [`9fa2fc9131302d97fdbd55635b23fcd359bc9435`](https://github.com/mshagirov/kubeflow-admin/commit/9fa2fc9131302d97fdbd55635b23fcd359bc9435).
- [GitHub CI run 39595823328](https://github.com/mshagirov/kubeflow-admin/actions/runs/39595823328)
  passed both test and security jobs, including the normal parallel package
  race suite, vet/build, manifest/OpenAPI checks, source security scans and
  AMD64/ARM64 container compilation.
- Published **`docker.io/muratshagirov/kubeflow-admin:0.4.0`** with provenance
  and SPDX SBOM attestations for both platforms.
- Multi-platform index:
  `sha256:676a55357615b31237076c75cc83ec8998315221d54ff1cede21946cead2a452`.
- Linux AMD64 manifest:
  `sha256:a9da2bd3b51151e03d807147539794ad4d3829d27e06fc8d64dc95db12d34a2d`.
- Linux ARM64 manifest:
  `sha256:83de53176c128a6ffb1cdbe8aac03cbb2c563caf887612090ad01a258560edd1`.
- Both runtime images passed Trivy HIGH/CRITICAL vulnerability and secret scans.
  The registry platform manifest digests **exactly match** the offline-scanned
  artifacts. Registry provenance/SBOM attestations were checked after upload.
  Both images run as `65532:65532`, carry the source revision label and include
  the byte-identical repository MIT notice at `/LICENSE`.

The tag was deliberately reused; deploy the **new immutable index digest**,
not an older `0.4.0` digest or a cached tag. The container image reference must
remain on one YAML line:

```yaml
image: docker.io/muratshagirov/kubeflow-admin@sha256:676a55357615b31237076c75cc83ec8998315221d54ff1cede21946cead2a452
```

No Kubernetes deployment or live workflow test was performed. Profile deletion
remains separately disabled by default; review its permission and deployment
prerequisites before enabling it. The configurable HTML footer defaults empty.
Receipt-only documentation commits after the source commit do not change the
published binaries.

## Sidebar and security refresh — 21 October 2026

- Removed the sidebar's status dot and `Administrative control plane` label;
  retained the GitHub source link and added regression assertions.
- Review corrected health summaries so missing Node readiness is not healthy
  and `Unavailable` PVs are not counted as `Available`. Restored the unrelated
  parent-lock assertion alongside the new Node target-lock tests.
- Updated the digest-pinned builder and CI to Go 1.27.3 after the Go 1.27.2
  source scan found GO-2026-6809 in `crypto/x509`.
- All package race tests passed with Go 1.27.3; `govulncheck` then reported no
  vulnerabilities with the patched toolchain.
- `make check build` and OpenAPI validation passed. The existing auth/plan
  cleanup timing test hit its 500ms budget during concurrent cold compilation;
  it subsequently passed five consecutive uncached race runs, both at default
  concurrency and with `GOMAXPROCS=2`. No production deadline was relaxed.
- Real Chrome checks passed at 390px and 1280px, and the optional browser
  regression suite passed against synthetic observations. No cluster was changed.
- Pinned Trivy 0.76 scans found no HIGH/CRITICAL findings in source
  (dependencies, secrets, configuration) or the rebuilt runtime image
  (vulnerabilities, secrets). Local binaries, virtualenv, Git data, rendered
  manifests and private site notes were excluded from the source scan.
- Runtime smoke verified linux/amd64, UID/GID `65532:65532`, and fail-closed
  startup without configuration with a read-only root and networking disabled.
- The release retains `docker.io/muratshagirov/kubeflow-admin:0.4.0` and
  linux/amd64. Reused tags must be deployed by the new registry digest, not by
  restarting an `IfNotPresent` Pod against the unchanged tag.

Refreshed image (provenance and SBOM included):

- OCI index: `sha256:fbee42f74ed76eeb52900d6d2637431986514434de36f43f21df1e4f7c242ae8`.
- linux/amd64 manifest: `sha256:2473514e032c22168cb0873c042263738f65d8a09af7ea1c278d91b2d8154f4a`.

## Responsive UI / History / Pods / Nodes work — 20–21 October 2026

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

Follow-up release (21 October 2026): Namespaces, Nodes, Workspace Health:

- Added Namespace and Node projections, the Namespaces page/drawer, Pod Node
  column, `kubectl get node -o wide`-style Nodes page, state summary cards, and
  an opt-in `node.cordon` / `node.uncordon` workflow.
- The cordon workflow patches only `spec.unschedulable` with UID and fresh
  `resourceVersion` preconditions, bounded retries for kubelet heartbeat churn,
  and a required reason. Force is required for control-plane and NotReady Nodes.
  It never drains or evicts workloads and has no delete fallback.
- History renders only readable Actor labels; historical records without a
  display label show `Legacy administrator`. Operations drops Step/Detail/IDs,
  and failure details are available from a fixed-slot red indicator.
- `make check` passed race tests, vet, API parity/script tests and all eight
  manifest variants, including the separate `node-cordon` overlay. The overlay
  grants only `nodes/patch` and is not automatically included in `all-features`.
- `go test -race -count=1 ./...` passed uncached. `govulncheck` reported no
  vulnerabilities.
- Real headless Chrome tested the revised UI at 390×844 and 1280×844 with
  synthetic observations: all eight sidebar pages, cluster-wide Namespaces
  despite a namespace query, Profiles-only rows, Pod Node column, full Nodes
  columns, Cordon/Uncordon drawer copy and Force control, Workspace Health cards
  and optional table, readable/legacy actors, failure-dot tooltip focus/Escape,
  and no page-width overflow.

Published follow-up image (before the sidebar and Go security refresh; historical):

- `docker.io/muratshagirov/kubeflow-admin:0.4.0`, linux/amd64 only.
- OCI index digest: `sha256:546710670f428345a4df37ca06c2d9ae70de9fff3bee54f1648aca093ab28588`.
- linux/amd64 manifest: `sha256:b004713c81f454bee077495b5faa2b28eed717ef7203523b2849682a01b9b7ef`.
- Pinned Trivy 0.76 `vuln,secret` HIGH/CRITICAL scan: **0 vulnerabilities, 0 secrets**.
  Runtime user is `65532:65532`.
- Node cordon remains disabled by default. Live node RBAC, control-plane
  behavior, kubelet/heartbeat timing and real cluster acceptance remain operator
  steps; fake clients and browser fixtures do not prove them.

Before deployment: take and verify a compatible backup before the schema-3
migration, pin the freshly verified release index digest in the site overlay
(not a historical digest above), and keep Pod restart
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
