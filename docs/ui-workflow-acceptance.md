# UI workflow live acceptance — current source

Operator-run checklist for the console's user-facing workflows: profile
create/edit, PV/PVC create/reserve/expansion, notebook start/stop/edit
(resources and storage), the summary-drilldown surfaces and the existing
gated Pod-restart and Node-cordon workflows.

Also covers separately gated Profile deletion and restricted PV/PVC edits and
unused-only deletion. Match this checklist to the exact deployed digest; see
[verification](verification.md) for publication status and [UI guide](ui.md) for
current labels and layout. A pending source change is not a deployed capability.

> **Operator-run only.** Every command in this document is a **placeholder** for
> a context you select and review yourself. Nothing here was executed against a
> cluster and nothing in this repository applies it. Replace `YOUR_CONTEXT`,
> `YOUR_NAMESPACE`, `YOUR_RELEASE_DIGEST` with your own reviewed values, run
> commands yourself in an approved window, and never paste live credentials,
> manifests, tokens or cluster output into this repository.

Record results in a private acceptance record (the pattern used in
[verification](verification.md)), not in this file. This checklist does not
replace the deployment runbook: [deployment](deployment.md) covers the site
overlay, OIDC, ingress and RBAC, [operations](operations.md) covers recovery,
[storage](storage.md) covers backend preparation, [backup](backup.md) covers the
database backup, [pods](pods.md) and [nodes](nodes.md) cover the two optional
gated workflows.

Item identifiers are stable and unique per section: `PC` profile create,
`PU` profile update, `PD` delete, `VC` PVC create, `VE` PVC expansion,
`VS` legacy static pair, `VR` PV reservation, `VP` separate registration/binding,
`NB` notebook start, `NP` notebook stop,
`NR` notebook resource edit, `NM` notebook mount edit, `PR` pod restart,
`NC` node cordon, `SY` storage editing, `SD` storage deletion, `UI` console
surfaces, `MR` refusal matrix, `HY` hygiene.

## Restricted storage management follow-up

Run these only against approved disposable storage after deploying a build that
contains the follow-up. Local synthetic tests do not prove deletion or expansion
behavior on your storage backend.

- [ ] **SY1** Storage has no separate Details buttons. PV, PVC and StorageClass
  names open the exact Resource Details, including same-name PV/PVC collisions.
  StorageClasses offer no edit/delete mutation.
- [ ] **SY2** With either writes or `storageEditing` off, editing is unavailable
  with a visible explanation. With both on and reviewed RBAC, Edit fetches a
  fresh restricted YAML document and identifies exact kind/namespace/name/UID/RV.
- [ ] **SY3** The YAML editor exposes no backend address/path/volume handle,
  driver attribute, annotations, unrelated labels, status, owner references or
  finalizers. Hidden configuration remains unchanged after a supported edit.
  Changing identity, adding unsupported fields, duplicate keys, aliases or a
  second document is refused without echoing sensitive input into errors/history.
- [ ] **SY4** Edit one `kubeflow-admin.io/` label, review the exact before/after
  plan, execute and verify the live value. Try a concurrent UID/RV change: the
  stale editor must be refused, never overwrite it. No-op edits are refused.
- [ ] **SY5** On approved unclaimed Available PVs, verify guarded reclaim-policy
  editing and visible data-reclamation warning. No backend-source or claimRef
  edits are permitted. PVC growth additionally requires the existing expansion
  feature, compatible class/CSI driver and actual resize convergence; shrinkage
  is refused. Do not use a spec-size change as proof of filesystem expansion.
- [ ] **SD1** With writes or `storageDeletion` off, Delete is unavailable with an
  explanation. With both on and separately reviewed RBAC, it requires a reason,
  exact typed target name, data-loss acknowledgment and a separate validated plan.
- [ ] **SD2** A PVC referenced by a Pod or Notebook (including a stopped Notebook)
  is refused. A bound/reserved/referenced or non-Available PV is refused. API list
  errors, protected namespaces and out-of-scope classes fail closed.
- [ ] **SD3** Delete only the approved unused disposable resource. Record its
  original UID and operation ID; success requires actual disappearance. Verify
  backing data independently according to the reviewed reclaim policy. Retain
  is not a wipe; Delete may destroy data even when no workload currently uses it.
- [ ] **SD3a** By default delete an unused Retain-bound PVC: the PV stays Released
  and no patch is sent. Separately enable `pvReclaim`, the `pv-reclaim` grant and
  verified admission isolation for `storage.isolatedStorageClasses`; explicitly
  acknowledge reclaim in the drawer. The preview names the
  post-delete reclaim, warns that Retain leaves backend data intact and Available
  makes the PV bindable (not a wipe). After success the PV is `Available` with no
  `claimRef` and is immediately reusable or deletable from the UI; the PVC stays
  deleted. A **Delete**-policy PV is never patched. Without PV `patch` RBAC, the
  plan is refused before the PVC is deleted. If the PV is replaced, its claimRef
  changed, any PVC names it, or the patch fails after the PVC delete, the result
  is `needs-review` with the PVC deletion receipt kept, no recreate and no retry.
- [ ] **SD4** A stale/replaced target, post-checkpoint drift or uncertain API
  response must not cause replay or forced cleanup. Finalizers may keep the
  operation unresolved; retain locks and investigate. Never remove finalizers
  to make this test pass. Coordinate external writers because separate unused
  checks and Kubernetes writes are not atomic.
- [ ] **SD5** On a disposable already-Released Retain PV with its old claim gone,
  choose Reclaim PV, supply reason, exact typed name and exposure acknowledgement.
  With gates off or class isolation missing it is refused. Verify the exact PV
  becomes Available with only claimRef removed and an available receipt. Existing
  or replacement claims, pending matching claims (including beta annotations),
  stale UID/RV, Delete policy and non-Released phases refuse. A patch timeout or
  uncertain convergence becomes needs-review with the PV locked and no replay.
- [ ] **SY6** Close/Escape return focus to the visible exact resource-name
  control. Switching resources or late document responses cannot overwrite
  another target's form. Merely opening Edit/Delete sends no mutation.
- [ ] **UI9** On a Node's Resource Details, **Scoped view** appears above
  **Observed details** and counts only observed Running Pods assigned to that
  exact node, across all namespaces. Click the count: the Pods page must show
  matching Node/Running filter chips and rows, with unrelated namespace/search
  filters cleared. Verify zero matches, pagination, Back/Forward and Clear filters.
  This is an inventory snapshot, not live usage or an atomic count.

## 0. Evidence rules — read before testing

Every row below is a claim about what the console observed. Keep the two apart.

| Console state | What it proves | What it does **not** prove |
| --- | --- | --- |
| Inventory row / table content | A snapshot at the displayed `ObservedAt` | That a change has converged; the snapshot is not atomic and is not a mutation precondition |
| Plan preview ("Validated before / after") | Server-side validation and dry-run only | Admission, quota admission, controller or backend success |
| Operation `accepted` | The plan was accepted and the operation queued | That anything was applied |
| Operation `succeeded` | The workflow-specific convergence checks passed (for example, the expected notebook workload became Ready, or the expanded claim reported sufficient capacity without pending resize conditions) | Future availability, filesystem contents, application reachability, durable data recovery or every backend-specific guarantee |
| PVC shown as created | The claim object exists | That a volume exists — with `WaitForFirstConsumer` the claim stays `Pending` until a consuming Pod is scheduled, and the console never creates that Pod |
| Expansion shown as applied | The claim's requested size was patched | That the CSI controller and filesystem resize completed |
| `needs-review` operation | The console refused to guess | Success or failure — it is neither; inspect the real cluster state, then reconcile by hand |

Rules that apply to every item below:

- [ ] **R1** Inventory is read-only observation. Never treat a table row as proof that a mutation took effect.
- [ ] **R2** Accepted ≠ completed. Follow each operation in History → Operations through to a terminal state, and capture the operation ID.
- [ ] **R3** Preview never mutates and never guarantees admission.
- [ ] **R4** There is **no forced cleanup**. Direct PVC/PV deletion is separately gated and unused-only; it never removes finalizers or force-deletes resources. Profile deletion cascades into its namespace, including PVCs and potentially their backing data. Recovery from an interrupted pilot needs a reviewed operator procedure.
- [ ] **R5** Do not use any destructive step to "make the test pass". A refused action is a valid pass result.

## 1. Preconditions (all must hold before the UI pilot)

### 1.1 Release identity and configuration

- [ ] **P1** The freshly published image **digest** is pinned in the site overlay (a tag is not an identity; `IfNotPresent` against an unchanged tag will not pick up a new release).
- [ ] **P2** `kustomize build deploy/site` output was reviewed: real digest, real group/host/issuer, no `REPLACE` placeholders, no `.invalid` URLs.
- [ ] **P3** Config was read against `internal/config/config.go` (not the older design-plan schema). Every `features.*` switch starts **false**, and `notebooks.controllerContractConfirmed` starts **false**.
- [ ] **P4** `policy.allowedNamespaces` is restricted to the disposable pilot namespace, and `policy.protectedNamespaces` lists every site system namespace.
- [ ] **P5** The app runs as **one** replica (`Recreate`, no autoscaler) and one database writer. A second writer breaks both the database lock and the static-backend uniqueness check.

### 1.2 Verified backup

- [ ] **B1** A consistent offline backup of the **entire stopped database directory** was taken and its checksum verified, following [backup](backup.md) end to end (archive + `integrity_check` on an isolated restored copy).
- [ ] **B2** The backup was taken **before** enabling writes and **before** any schema migration of this release.
- [ ] **B3** The backup is encrypted, access-restricted, and kept off the node and off Git. It contains identities, sessions, plans, operations and audit data.
- [ ] **B4** The restore was rehearsed on a disposable fixture, not on the production deployment.

### 1.3 Disposable pilot target

- [ ] **D1** The pilot namespace/profile is **explicitly disposable**: no valuable data, no other users, no production workload, and the owner/user has been told what will be interrupted.
- [ ] **D2** The pilot Profile name is unique and unused by any other team; the pilot namespace name matches the Profile-name convention the console expects.
- [ ] **D3** The pilot runs no real user data. Deletion is still tested only on this disposable target (see 2.3).
- [ ] **D4** The pilot namespace is **not** in `protectedNamespaces` and no `kube-*`, system or Kubeflow namespace is in scope.

### 1.4 Feature and RBAC gates

Enable one gate at a time; each overlay is reviewed and applied separately, and
no overlay is stacked on top of another by accident.

- [ ] **G1** Base RBAC only — read-only inventory. Confirm the base ServiceAccount holds no write verbs before the first pilot.
- [ ] **G2** `features.writesEnabled: true` **plus** the `deploy/overlays/writes/` grant (or the reviewed equivalent). This is the minimum for profile create/update and notebook start/stop/edit.
- [ ] **G3** `features.staticStorage: true` **plus** `deploy/overlays/static-storage/` — required for template-based PV registration and PV reservation.
- [ ] **G4** `features.pvcExpansion: true` **plus** `deploy/overlays/pvc-expansion/` — required for PVC expansion.
- [ ] **G5** `features.podRestarts: true` **plus** `deploy/overlays/pod-restarts/` (prefer a namespace-scoped Role for the pilot) — required for Pod restart.
- [ ] **G6** `features.nodeCordon: true` **plus** `deploy/overlays/node-cordon/` — required for cordon/uncordon.
- [ ] **G7** **Profile deletion opt-in.** Both `features.writesEnabled` and `features.profileDeletion` must be true. Deletion is shipped disabled. The separate `deploy/overlays/profile-deletion/rbac.yaml` grants only `kubeflow.org/profiles` `delete` through `kubeflow-admin-profile-deletion`. Install and review the site's grant separately; no live acceptance is claimed here.
  - Config key (settled): `features.profileDeletion` — default `false`
  - Overlay + role (settled): `deploy/overlays/profile-deletion/` → `kubeflow-admin-profile-deletion`
  - RBAC installed at the site: `________________________` *(operator step; the base must stay supplied once)*
- [ ] **G8** With writes enabled but the deletion opt-in **off**, the Delete control is absent/disabled in the UI **and** a direct API attempt is refused server-side. Verify both.
- [ ] **G9** Turning any gate off stops new actions; it does **not** revoke RBAC and does not undo completed mutations. Revoke optional bindings separately through the platform operator's reviewed procedure.
- [ ] **G10** The **Profile-deletion grant** includes only `kubeflow.org/profiles` `delete`: no namespace, PV, PVC or Pod delete, and no finalizer update or patch. Narrow it with `resourceNames` on the ClusterRole for the disposable pilot Profile. This restricts the name, not fields or the namespace cascade; admission policy and namespace protection remain necessary. Storage permissions, if separately enabled, are independent grants.
- [ ] **G11** Restricted storage editing requires writes, `features.storageEditing` and the separately reviewed `deploy/overlays/storage-editing/rbac.yaml` patch grant. PVC request growth additionally requires `features.pvcExpansion` and compatible storage; label-only edits do not require expansion.
- [ ] **G12** Unused-only storage deletion requires writes, `features.storageDeletion` and the independently reviewed `deploy/overlays/storage-deletion/rbac.yaml` delete grant. Optional reclaim additionally requires `pvReclaim`, per-plan acknowledgement, verified class isolation and the separate `pv-reclaim` PV patch grant. These are not enabled by Profile deletion or a broad feature bundle.
- [ ] **G13** Optional PodDefault creation/overwrite and related-PVC cleanup require writes, `features.podDefaults` and the separate `deploy/overlays/poddefaults/rbac.yaml` grant. Prefer a namespace-scoped equivalent for a pilot. Base get/list access detects references even when mutation is disabled. Verify the installed namespaced API and webhook contract independently; discovery denial is not API absence.

### 1.5 Notebook controller contract (independent verification)

- [ ] **C1** Before setting `notebooks.controllerContractConfirmed: true`, independently confirm on a **disposable** notebook: the installed controller's version/digest, the stop/start annotation it honours (`kubeflow-resource-stopped`), its culling behaviour and annotation keys, the generated Pod shape, child-UID ownership, and any admission restrictions.
- [ ] **C2** A CR dry-run is **not** that verification: it does not produce the generated Pod and does not prove controller convergence.
- [ ] **C3** If the installed controller does not honour the expected contract, keep `controllerContractConfirmed` false and keep the notebook controls disabled; report the mismatch instead of enabling the flag.

### 1.6 Identity and email handling (read this before creating or updating a profile)

- [ ] **ID1** The owner field is a **label**, not a credential store and not an identity proof. The console records the exact string you type (case preserved) and sends it to the Profile.
- [ ] **ID2** The console does **not** verify that the entered email/username exists in the IdP, is a real user, is provisioned in LDAP, or can actually log in. It does not contact the IdP for directory lookups and does not handle passwords. **Syntax validation is not registration verification**: a well-formed address can still be unregistered, and a registered identity is never confirmed by this console. The create field is labelled `Email (registered with identity provider, e.g. user email from LDAP)` — the label states the expectation, it does not trigger a lookup.
- [ ] **ID3** Therefore verify out of band, before creating or updating the profile: the identity exists in the site IdP/Dex/LDAP, the user can authenticate, and the identity string matches what Kubeflow's Profile controller and the notebook's home directory expect.
- [ ] **ID4** **Create is email-only.** `profile.create` refuses an owner that is not email-formatted. **Update is not:** `profile.update` accepts a legacy Profile whose owner is a configured username, because the owner is left **unmodified**. Confirm there is no universal email-format rejection on the update path — a legacy owner must still be able to change quotas.
- [ ] **ID5** A typo in the owner field that still satisfies the syntax rule is accepted by the console at creation time. It will surface later as a broken workspace, not as an error at creation or update time.
- [ ] **ID6** **Ownership is immutable** in the installed Profile controller. Owner transfer, owner migration and custom owner controllers are a separate future workflow; do not attempt them here and do not delete existing data to work around them.

## 2. Workflow acceptance

Capture for every executed workflow: the plan ID, the operation ID, the actor
label, the `ObservedAt` of the inventory you compared against, and the final
operation state.

### 2.1 Profile create

- [ ] **PC1** **Name rule.** The name field enforces a DNS-1123 label: lowercase letters, digits and `-`, starting and ending with an alphanumeric character, **maximum 63 characters**. Entering an uppercase letter, `_`, `.`, a leading/trailing `-`, or a 64th character is refused client-side and refused server-side.
- [ ] **PC2** **`.`/`_` suggestion.** When a typed name contains `.` or `_`, the console suggests a DNS-1123-compliant variant (for example `first.last` → `first-last`). Confirm the suggestion is **advisory and never auto-applied**: the submitted value is what you typed unless you accept the suggestion, and the accepted name is what appears in the preview and in the created Profile.
- [ ] **PC3** **Duplicate refusal.** Creating a Profile whose name already exists — as a Profile **or** as a Namespace — is refused before any mutation. The console must not adopt, relabel or take over an existing namespace.
- [ ] **PC4** Fill owner (per ID1–ID6) and, optionally, quota keys, then prepare the plan. The preview shows the owner and the quota before/after, and warns that the exact owner identity is retained and directory existence is not verified.
- [ ] **PC5** Confirm the plan checkbox, submit once, and follow the operation to a terminal state.
- [ ] **PC6** After **Refresh**, the Profile appears on the Profiles page, its namespace appears on the Namespaces page, and the namespace is owned by the Profile with the owner annotation matching.
- [ ] **PC7** Verify with your own read-only tooling that the created Profile's `spec.owner` is exactly the string you entered, and that the namespace's owner annotation matches.

### 2.2 Profile update (quota editing)

The UI uses `profile.update`. The deprecated `profile.quota.change` alias now
requires the same unchanged owner and configuration digest; unguarded requests
from old tabs/clients are refused. Reload the console after upgrading. Profile ownership is
**immutable** in the installed Profile controller, so `profile.update` edits
**quotas only**: the pre-filled owner is read-only, and the request must echo
the current owner back unchanged together with a `baseConfigDigest`.

- [ ] **PU1** **Prefill.** Opening the update form prefills the **current owner** (read-only) and the **current quota** read from the Profile's own quota specification. Confirm the prefill is not the observed `ResourceQuota` hard/used status — quota status is not a substitute for the Profile's quota configuration.
- [ ] **PU2** **Unknown quota preservation.** The Profile's quota may contain operator/controller-managed keys the console does not manage. The update form must show them as preserved/read-only (or otherwise keep them intact), and the server must round-trip them **verbatim**. Changing such a key to a different value is refused; re-submitting it unchanged is accepted and preserved.
- [ ] **PU3** **Complete-set rule.** Every existing supported quota key must be supplied again. A blank field omits the key, it does not preserve it, and removing a key is refused. Confirm the refusal message is explicit rather than silently dropping the key.
- [ ] **PU4** The preview lists actual quota changes as before/after, with no owner change or invented change to a preserved unknown key. Compare the final Profile to confirm those unknown keys remain unchanged.
- [ ] **PU5** Change one supported quota value and confirm the operation reaches a terminal state and the Profile's quota reflects exactly the intended delta — and nothing else changed.
- [ ] **PU6** **Owner is read-only.** The owner field is displayed, not editable, and the submitted owner must equal the current owner exactly. A request that changes the owner is refused as an **unsupported contract** (422 validation failure), never applied as a broken write. Confirm the console does not attempt a namespace-owner or RoleBinding reconciliation for a new owner.
- [ ] **PU7** **Config digest.** The update request carries a `baseConfigDigest` — the identity-bound canonical digest of the reviewed Profile name, UID, owner and quota hard map, taken from the console's own Profile observation (`details.configDigest`) and echoed back unchanged. It is never user-editable or displayed. Confirm a stale prefill is refused: change the quota out of band after the form was filled, then submit — the request must be refused because the digest no longer matches, instead of overwriting the concurrent change. Replacing the Profile under the same name must also invalidate the form. Ordinary metadata and `resourceVersion` churn alone do **not** change the digest; later preview/execution UID/resourceVersion checks remain conservative and can require a new preview.
- [ ] **PU8** **No silent widening.** The update workflow must not change the Profile's namespace, name, owner identity, owner reference kind, or any controller-managed field.
- [ ] **PU9** **Deployed quantity forms.** Valid integer-valued quotas and zero limits (for example CPU `2` and Pods `0`) must appear in the form. Editing another key must preserve zero and unmanaged quota values rather than omit them or force an increase.
- [ ] **PU10** **Existing raw device quota key.** If the Profile already declares a bare device key such as `nvidia.com/gpu`, the plan warns that Kubernetes accepts the key but standard Kubernetes does not enforce it as a device request quota, that enforcement uses `requests.<device>`, and that editing retains the key without migrating or activating the limit. Confirm the warning appears even when only the CPU or memory alias changes, and that the preview neither invents a `requests.<device>` key nor renames the declared one. The warning is information: it is not an approval to migrate, and it does not claim the original GPU value is re-enforced.
- [ ] **PU11** **GPU enforcement evidence (operator-only).** `ResourceQuota.status.hard`, including the Profile controller's copy of `spec.resourceQuotaSpec.hard`, only restates declarations and is **not** proof that a device limit is enforced. Measure enforcement through `status.used` for `requests.<device>` after an admitted Pod requests it, or with a disposable admission test that asks for more than the declared limit and expects the API server to refuse it. Run these only against an isolated disposable namespace in an approved window, never against a production namespace, never exporting Secrets or full manifests. Do not treat the console's convergence check as enforcement evidence. Migrating a bare device key to `requests.<device>` is a separate, explicitly authorized operator change, never part of a console quota edit.

### 2.3 Profile delete — gated, typed confirmation

The typed action kind is `profile.delete`. There is **no** namespace-delete
action and **no** arbitrary delete endpoint. `profile.delete` is the only action
that directly deletes a Profile; unused-only storage deletion has separate typed
actions and gates. There is no HTTP `DELETE` route, and requests require the
corresponding opt-in.

Read this section twice. Deletion is irreversible: no undo, automatic recovery or
forced cleanup. A narrow stopped-app Profile-deletion reconciliation tool can
record an independently verified outcome without writing to Kubernetes; see
[operations](operations.md#offline-reconciliation-of-a-profile-deletion).

- [ ] **PD1** With the deletion opt-in **off** (G7/G8), no delete control is offered, and a direct API attempt is refused with a policy error before any plan is created.
- [ ] **PD2** With the opt-in **on**, deleting requires **two** distinct steps: the standard plan confirmation (with the exact target listed) **and** a typed confirmation of the exact target name, sent as the `confirmName` field. The comparison is raw and exact — no trimming, no case folding. A wrong name, a partial name, or a different case must not be accepted. A non-empty **reason** is also required.
- [ ] **PD3** The plan/preview must state in plain words what is deleted and what is **not** deleted. Confirm the wording before you confirm.
- [ ] **PD4** **Cleanup is delegated.** The console requests Profile deletion with **background propagation**, matching `kubectl delete`; the installed Profile controller and Kubernetes garbage collection remove the owned namespace and its namespaced objects (including Notebooks, Pods and PVCs). Data reclamation follows PD6. The console issues **no Namespace `DELETE`** and holds no namespace delete permission. Confirm the preview names the affected classes.
- [ ] **PD5** **Success means both are gone.** The operation succeeds only when the Profile **and** its owned namespace are observed gone. A same-named replacement object, a namespace still `Terminating`, or a read error is **not** gone and must not be reported as success.
- [ ] **PD6** **PV and data caveat.** The Profile deletion action does not directly delete PVs. PVC deletion applies the bound PV's reclaim policy: **Delete** can irreversibly delete the backing volume and data; **Retain** leaves reclamation to an operator but does not protect against backend lifecycle or later operator deletion. Namespaced VolumeSnapshot objects also disappear; their underlying content follows the VolumeSnapshotContent deletion policy. Verify retained PVs, snapshots, backups and external NFS/CSI data separately.
- [ ] **PD7** **Namespace ownership is required.** The namespace must exist, be active, and be controlled by the exact Profile UID/name with the matching `owner` annotation. Missing, replaced or unowned namespaces are refused; orphaned Profiles require a separate operator procedure.
  - Verify the platform prevents extra/cross-namespace Profile owner references and replacement/reparenting during cleanup. App locks cannot fence external writers; the namespace checks and Profile DELETE are not an atomic transaction. Keep deletion disabled without reviewed admission/RBAC controls and a coordinated maintenance window.
- [ ] **PD8** **Finalizer timeout.** The console waits for cleanup only within its bounded operation deadline; it never removes finalizers or force-deletes terminating objects. An unfinished deletion becomes `needs-review` and retains its Profile/namespace locks for operator reconciliation.
- [ ] **PD9** **No cleanup shortcut.** Deleting a pilot Profile is not the cleanup procedure for a failed pilot, and a delete is never a way to resolve a `needs-review` operation.
- [ ] **PD10** While deletion is unresolved, new child mutations in that namespace must be refused. After successful deletion, missing Profile/namespace scope also refuses child mutations. A later intentionally recreated workspace requires fresh observations and plans; old plans must not mutate its replacement objects.
- [ ] **PD11** Run the delete only on the disposable pilot target; afterwards, reconcile the leftover PVs and backend directories with your own reviewed cleanup plan.
- [ ] **PD12** The delete is recorded in History → Operations and Audit with actor, reason and target, like every other mutation.

### 2.4 PVC create (dynamic)

- [ ] **VC1** Choose only a class from the approved `storage.allowedStorageClasses` list; the console rejects any other class. Choose it explicitly. The app creates only the PVC in this workflow; the StorageClass's external provisioner creates the PV when provisioning occurs.
- [ ] **VC2** The console creates a claim from typed inputs (size, class, access mode), not a free-form creation manifest. The separate restricted YAML editor for existing PVs/PVCs accepts no arbitrary volume source or secret reference.
- [ ] **VC3** A claim name that already exists in that namespace is refused (conflict) before any mutation.
- [ ] **VC4** The plan warns about the StorageClass reclaim policy and, for `WaitForFirstConsumer`, states that the claim will stay `Pending` until a consuming Pod is scheduled.
- [ ] **VC5** After the operation succeeds, verify with your own tooling that the claim exists and observe its real phase. If it is `Pending`, **that is expected** and is not a provisioning failure; record it as "created, not provisioned".
- [ ] **VC6** If the claim must bind, schedule a disposable consumer workload **yourself** with the storage operator's approval and then observe `Bound` and the actual capacity. The console never creates the consumer Pod.
- [ ] **VC7** Check observed `ResourceQuota` headroom handling: a request exceeding the namespace's observed quota headroom is refused before mutation.

### 2.5 PVC expand (update of requested size)

- [ ] **VE1** Expansion requires the gate (G4) and a **bound, non-terminating** claim with an explicit class. Unbound, `Lost`, terminating or class-less claims are refused.
- [ ] **VE2** **Upward only.** A size equal to or smaller than the current request is refused. Shrinking is never possible.
- [ ] **VE3** The class must advertise `allowVolumeExpansion`, and the bound PV must be a **CSI** volume whose driver matches the class provisioner, with a matching claim reference. A plain NFS PV is refused even when its class reports expansion support.
- [ ] **VE4** Expansion does not change the class, access mode or volume mode. Confirm the preview shows only the requested-size change.
- [ ] **VE5** Success requires the claim to report capacity at least as large as requested, with no active Resizing/FileSystemResizePending condition. Still independently verify mounted filesystem size, backend capacity, data and quota accounting with the storage operator.
- [ ] **VE6** Expansion cannot be undone; there is no shrink path in this release.

### 2.6 Legacy combined static pair from an approved template (NFS / CSI)

- [ ] **VS1** Requires the static-storage gate (G3). The backend must already be **prepared** by the storage operator: a distinct NFS export/directory with reviewed ACLs and mount options, or a distinct CSI volume with an exact reviewed handle. The console creates no directories and sets no permissions.
- [ ] **VS2** In this legacy combined workflow, the browser cannot replace the server, driver, fixed CSI handle or arbitrary driver attributes. NFS accepts only the relative subdirectory described below. The separate registration workflow in 2.7a has different, bounded path inputs.
- [ ] **VS3** For NFS templates, the relative subdirectory must be a canonical, non-empty relative path strictly **beneath** the approved base path. `..`, absolute paths, `.` and non-canonical paths are refused.
- [ ] **VS4** For CSI templates, no subdirectory is accepted and the driver must match the class provisioner.
- [ ] **VS5** The template's class and access modes must match the ones you selected; mismatches are refused.
- [ ] **VS6** **Backend uniqueness.** A backend already represented by an existing PV (same NFS server with an overlapping path, or same CSI driver and handle) is refused. This check is **not atomic** — it cannot be closed by a CEL admission rule — so exactly one application writer must run, and cluster-wide uniqueness remains a site-owned registration process (see [storage](storage.md)).
- [ ] **VS7** The new PV is registered with a `Retain` reclaim policy and a claim reference, and the matching claim is created pointing at that PV, so the pair binds without any further console action. Verify the PV's claim reference and the bound claim with your own tooling.
- [ ] **VS8** The preview warns that the operation registers existing storage only: no directory creation, no permission change, no allocation, no file movement.

### 2.7 Reserve an existing PV and claim it

- [ ] **VR1** The PV selector offers only PVs that are `Available`, unreserved, `Retain`-policy, filesystem-mode and free of node affinity/topology constraints. A `Released` PV is deliberately absent and must be reviewed by a human, not reserved here.
- [ ] **VR2** The PV must match the claim's class, capacity and access modes, and must be one of the approved NFS/CSI backends.
- [ ] **VR3** The operation reserves the PV by setting the claim reference and creating the claim. It does not modify the PV's backend, capacity, class or access modes, and it does not repair a mismatched PV.
- [ ] **VR4** Verify the reservation took effect and the claim is bound to exactly the intended PV.

### 2.7a Separate template-driven registration and binding

These checks require configured `storage.registrationTemplates`, the static-storage
gate/grant, and a distinct operator-prepared disposable backend. Do not reuse a
valuable existing directory. See [storage configuration](storage.md).

Boundary probes: joined BeeGFS handles must fit 128 bytes; new/overwritten
PodDefault request names must fit 52 bytes. Existing related PodDefaults with
valid Kubernetes names up to 253 characters must still be included in cleanup
review, not hidden or rejected by the creation-name policy.

- [ ] **VP1** Create an NFS PV with a new administrator-supplied name and full absolute path. The configured approved root itself or a descendant is accepted; escape, traversal, duplicate/overlapping registration and unapproved class are refused. Capacity, access modes, server, mount options and Retain policy are read-only template values.
- [ ] **VP2** Create a BeeGFS PV using the fixed template prefix, including `/home`, plus only the administrator's relative suffix. Refuse URL/authority replacement, traversal, encodings and noncanonical paths. Verify the exact registered handle independently; do not expose it in a public evidence bundle.
- [ ] **VP3** Each standalone PV is unclaimed: no implicit PVC or claimRef. After refresh it becomes selectable only when Available and compatible. Verify independently; API success does not prove a mount works.
- [ ] **VP4** Create a new PVC in an eligible Profile namespace, selecting an approved class/template and the exact compatible PV. PV identity/version changes, reservations, wrong class/mode or insufficient capacity refuse binding. The PVC request/access modes are template-controlled and independent of PV capacity; exercise 2400Gi/10Gi NFS and 500Gi/10Gi BeeGFS examples if approved for the pilot.
- [ ] **VP5** NFS UID/GID are required nonzero decimal values and appear only as the two approved annotations. Verify actual filesystem permissions separately: these annotations do not chown static directories or enforce quotas.
- [ ] **VP6** With Create PodDefault off, create no PodDefault. With the separate PodDefaults gate/grant enabled, review its suggested name/mount path and independent selector, same namespace and exact new PVC reference. No standalone PodDefault management control or arbitrary spec editor exists.
- [ ] **VP7** An existing PodDefault name requires a warning, reviewed before/after change summary and explicit overwrite acknowledgement. A concurrent edit/replacement refuses the guarded patch. Confirm overwritten objects are dedicated to the new PVC; unrelated settings removed by the overwrite must be called out without revealing sensitive values.
- [ ] **VP8** Opt into the generated configuration only on an approved disposable Notebook/new Pod through the normal workflow. Verify admission and actual mount access independently. Existing Pods remain unchanged; CRD presence and PodDefault creation alone do not prove webhook operation.
- [ ] **VP9** PVC deletion is refused before PodDefault cleanup while any Pod or Notebook template, including a stopped Notebook, references the claim. Once unused, preview and explicitly confirm every related PodDefault deletion and the PVC deletion. Shared/external-management warnings permit confirmed removal but other PVCs remain untouched. Test this only on wholly disposable resources.
- [ ] **VP10** Discovery denial/failure, malformed or incomplete dependencies, a changed/replaced object, an unreviewed new reference or more than 32 related PodDefaults fails closed. Do not interpret missing permission as no references. With PodDefault mutation disabled, related references still prevent unsafe PVC deletion.
- [ ] **VP11** Controlled partial-failure tests report actual completed substeps, preserve the PVC if PodDefault creation fails, and never recreate PodDefaults when later cleanup fails. Ambiguous outcomes retain needs-review evidence/locks; no automatic replay, force or finalizer removal. Verify Retain backing data separately after cleanup.

### 2.8 Notebook start

- [ ] **NB1** Requires `features.writesEnabled: true` **and** `notebooks.controllerContractConfirmed: true` (C1–C3). With the contract unconfirmed, the console shows a policy notice and the notebook controls are unavailable.
- [ ] **NB2** The preview shows the exact target notebook, the before/after state and any warnings. Confirm you are acting on the **disposable** pilot notebook.
- [ ] **NB3** The start removes the stop annotation and clears the stale idle-activity annotations the culler uses, so the notebook is not immediately re-culled. It does not refresh the idle timer continuously and does not disable idle culling.
- [ ] **NB4** Independently confirm the expected controller-owned Pod is Ready and the notebook application is reachable. The console waits for its workload observations; those checks do not prove application reachability or future availability.
- [ ] **NB5** Starting a notebook loses nothing by itself, but starting a notebook that was stopped for a reason (for example a culling) may re-trigger the idle policy if the user is inactive. Confirm the observed behaviour matches your site's expectations.

### 2.9 Notebook stop

- [ ] **NP1** The stop sets the stop annotation with a timestamp. It does **not** delete the Pod, does not evict it, and does not force-remove anything.
- [ ] **NP2** Losing kernels, in-memory processes, writable container layers and unsaved files is expected. Persistent volume data is not checkpointed or protected by this workflow.
- [ ] **NP3** Confirm the observed outcome: the notebook shows as stopped and the Pod is gone or terminating according to **your installed controller's** behaviour — not according to this checklist's expectation.
- [ ] **NP4** A stop that does not reach the observed state is reported as `needs-review`, not retried automatically.

### 2.10 Notebook edit — resources (CPU / memory / GPU)

- [ ] **NR1** Choose the exact namespace and notebook, then the **named container** to edit. The container selector lists the container names observed in that notebook.
- [ ] **NR2** Image changes are restricted to the configured `notebooks.allowedImages` list. An unlisted image is refused.
- [ ] **NR3** CPU/memory requests and limits are optional per field; a blank field leaves that field unchanged. Confirm the preview shows the before/after of each changed field and nothing else.
- [ ] **NR4** GPU edits require an approved preset and a bounded count; a count of zero clears the placement, and the previously applied preset is managed by the console's own annotation. A preset that is no longer configured is refused rather than silently dropped.
- [ ] **NR5** **The edit stops the notebook first.** It applies changes while stopped. The explicit "Restart after editing" option can restore a previously running notebook; otherwise it remains stopped. Review that choice in the plan and confirm the observed end state matches it.
- [ ] **NR6** The edit is refused when the resulting Pod would violate the namespace's quota headroom or its LimitRange.
- [ ] **NR7** Confirm the edit did not disturb unrelated containers, volumes, or fields the console does not manage.

### 2.11 Notebook edit — storage (PVC mounts)

- [ ] **NM1** Mount editing is opt-in: unchecked preserves mounts; checked with no rows requests removal of editable PVC mounts, subject to protected-mount refusal.
- [ ] **NM2** Removal of protected workspace mounts (for example `/home/jovyan` and the configured `notebooks.protectedMountPaths`) is refused.
- [ ] **NM3** Each mount needs a valid name, a claim that exists **in the same namespace**, an absolute safe path and a unique mount path. Duplicate names/paths, missing claims, `subPath`/`subPathExpr` use, and unsafe paths are refused.
- [ ] **NM4** A volume that is not one of the notebook's PVC volumes cannot be replaced, and unrelated volumes are preserved.
- [ ] **NM5** The claim list is namespace-scoped: you can only mount a PVC from the notebook's own namespace.
- [ ] **NM6** Confirm the mounted volume is the one you intended and that the notebook comes back with the expected mounts.

### 2.12 Pod restart (existing, separately gated)

- [ ] **PR1** Requires the gate (G5) and a confirmed controller contract (C1). The overlay's cluster-wide grant should be replaced by a namespace-scoped Role for the pilot.
- [ ] **PR2** Only running Pods controlled by an `apps/v1` StatefulSet that is itself controlled by a running Notebook in an allowed namespace are supported. Unmanaged, static, mirror, terminating, stopped-parent and unhealthy-node Pods are refused.
- [ ] **PR3** The restart uses a normal, disruption-budget-aware eviction with UID/resourceVersion preconditions. It never falls back to force deletion.
- [ ] **PR4** Success means the old UID disappeared **and** a distinct, correctly owned replacement is Ready at the desired revision. Confirm both in the observed state.
- [ ] **PR5** A PDB-blocked eviction must fail without deleting anything and must not leave a lock that blocks a fresh, corrected plan.
- [ ] **PR6** An ambiguous interruption becomes `needs-review` and is never replayed automatically.

### 2.13 Node cordon / uncordon (existing, separately gated)

- [ ] **NC1** Requires the gate (G6). The overlay grants cluster-wide `patch` on Nodes; Kubernetes RBAC cannot restrict the patched field, so admission policy must remain in place.
- [ ] **NC2** A reason is mandatory. **Force** is required for control-plane and NotReady nodes, and for re-applying the requested state.
- [ ] **NC3** The preview shows the exact before/after of `spec.unschedulable`. Cordon never evicts, drains or moves running Pods.
- [ ] **NC4** A concurrent node change rejects the plan. Confirm the node's observed state afterwards and uncordon if you cordoned it for the test.

### 2.14 Console surfaces: drawers, drilldown, filters, safe details

- [ ] **UI1** **Manage drawers.** Profile/storage/notebook drawers close via Close, scrim and Escape and restore focus. Enter may prepare a validated plan, but must never execute a mutation without the separate plan confirmation.
- [ ] **UI2** **Summary drawers.** A Profile name opens the single Profile Summary with safe metadata, scoped views, owner, declared quotas and Edit/Delete actions; there is no duplicate Profile Resource Details view. Selecting a namespace opens its summary with observed resources grouped by class. Empty groups are omitted, and the summary states that this is an observation, not a live emptiness proof.
- [ ] **UI3** **Namespace drilldown.** The namespace links from a profile summary set the namespace context (and the `?namespace=` URL parameter). Every scoped page then filters to that namespace, while genuinely cluster-scoped pages (Profiles, Namespaces, Nodes) keep showing all rows.
- [ ] **UI4** **Clearing filters.** After applying search, namespace or resource drilldown, use **Clear filters**. It must remove search/resource focus and namespace scope, reset pagination and restore the default rows/count. Back/forward and refresh must not retain an invisible exact-resource restriction.
- [ ] **UI5** **Pagination and search scope.** Search applies to all loaded rows before pagination (50 rows per page by default), not only the visible page. History's loaded window is bounded and is not a full export.
- [ ] **UI6** **Safe details only.** No drawer, plan preview, history row or notification may reveal Secret values, the OIDC client secret, kubeconfig paths, the database path, environment values of running Pods, full manifests, session tokens, or arbitrary existing PV backend sources/driver attributes. The gated registration form may show approved non-secret template server/options/prefix and administrator-entered paths as required for review. Durable audit/acceptance summaries must redact full paths/handles; keep private execution evidence and all pilot screenshots out of the public repository.
- [ ] **UI7** **Resource names and Notebook-only listing.** The Notebooks table lists only Kubeflow Notebook custom resources, not Pods or StatefulSets. Notebook, Pod and Node names open Resource Details, with no separate Details button on those pages. Storage names continue opening their details. Each drawer must identify the exact kind, namespace and name, including when names collide.
- [ ] **UI8** **Manage the selected Notebook.** From a Notebook's Resource Details, choose **Manage Notebook**. Its management drawer must select that exact namespace and Notebook and the known container when unambiguous. Blank edit fields preserve the existing resource/image configuration; mounts are preserved unless replacement is explicitly selected. The inventory does not supply full editable container or mount configuration, so blank fields must not be mistaken for the actual deployed values. Changing the Notebook/namespace/container must clear another target's typed edits, mounts and restart flags. Check disabled write/controller gates, UID-change refusal and visible focus restoration. This app manages existing Notebooks; create the disposable pilot Notebook through the normal Kubeflow workflow first.
- [ ] **UI10** **Fail-safe surfaces.** Policy explanations identify disabled writes, Notebook controller contract, Pod restarts and Node cordon. Profile Delete is offered only with writes and its deletion gate; PV/PVC Edit/Delete stay visible but disabled with an explanation when gated off. APIs independently refuse gated actions. At 390 px and 1280 px, pages do not overflow horizontally; smaller-view tables scroll within their own region. The Nodes table itself fits its wide-screen container.
- [ ] **UI11** **Inventory freshness.** The `ObservedAt` timestamp updates on Refresh and shows a warning when the fetch fails; switching pages inside the freshness window must not trigger an extra cluster read.
- [ ] **UI12** **Overview.** No separate metrics row: Workspace Health contains Nodes plus Profiles, Notebooks, Pods, PVCs, PVs and Events. Nodes is larger and spans both rows beside the other cards' three-column/two-row wide layout. Verify namespace scoping and optional detailed-table expansion; Profiles policy labels are not readiness evidence.
- [ ] **UI13** **Notebook heading.** Manage notebook stays at the right edge and vertically centered at narrow/wide widths, including when the visible policy message wraps. It remains disabled with the exact missing gate explained where appropriate.
- [ ] **UI14** **Profiles layout.** Workspace ownership appears above the full-width Profiles table. Profiles and Storage entry buttons read New; existing-resource actions remain in their drawers. The sidebar has no WORKSPACE label and lists Storage before Namespaces.
- [ ] **UI15** **Notifications.** One card is visible by default. Expand/collapse preserves all current notices; errors lead, newest-first within error/non-error groups. Repeats stay deduplicated. Verify dismiss/focus behavior, ten-second info/success expiry and that dismissal never acknowledges mutation warnings.

## 3. Explicitly unsupported — do not attempt, and expect a refusal

| Not available in this release | Why it matters |
| --- | --- |
| Arbitrary PV spec/backend/capacity/class/access-mode update | Restricted PV Edit permits only console-owned labels and Retain/Delete reclaim policy on approved, unclaimed Available PVs; hidden fields cannot be edited |
| In-use PVC deletion or bound/reserved/referenced PV deletion | Direct PV/PVC Delete exists, but is separately gated, unused-only, identity-guarded and confirmed. Even unused PVC deletion can destroy backing data under Delete reclaim policy |
| Forced cleanup, force-delete, finalizer removal, "delete everything" | No forced cleanup exists in any form. Cleanup is an operator-run procedure with its own reviewed data/reclaim plan |
| Shrinking a PVC | Expansion is upward-only and irreversible |
| Changing a claim's class, access mode or volume mode | These are immutable in practice and are not exposed |
| Free-form manifest / raw YAML / arbitrary Kubernetes proxy | Unsupported. Existing PV/PVC editing uses a restricted safe YAML projection, never a full manifest or arbitrary patch |
| Adopting an existing namespace or relabelling a Profile | Existing namespaces are refused, not adopted |
| Arbitrary resource deletion or namespace deletion | Only typed, separately gated `profile.delete`, `pv.delete` and `pvc.delete` workflows exist; there is no HTTP DELETE route or Kubernetes proxy |
| Removing a quota key | Removal is unsupported; existing keys must be resubmitted |
| Transferring Profile ownership, migrating an owner, or a custom owner controller | Ownership is immutable in the installed Profile controller; `profile.update` edits quotas only and refuses an owner change as an unsupported contract |
| Editing a Notebook's non-container fields (volumes other than PVC mounts, service account, security context, init containers) | Only the listed container fields and PVC mounts are editable |
| Displaying credentials, secrets, kubeconfig, environment values, full manifests or arbitrary existing PV backend details | Not rendered. Registration forms expose only approved non-secret template settings and administrator-entered paths, not arbitrary existing volume sources |
| Standalone PodDefault management or arbitrary injected Pod settings | PodDefaults are created/explicitly overwritten only within PVC creation, and removed only within confirmed related-PVC cleanup |
| Pipeline counts, live CPU/memory usage, real health probes | No KFP or metrics integration exists; nothing is inferred as zero |
| Upstream LDAP/Dex password handling, user registration, group membership changes | The app authenticates administrators only and never manages identities |
| Any mutation outside the configured namespace scope or against protected namespaces | Scope and protection are enforced server-side |

## 4. Refusal matrix (expected negatives)

Each row must fail **before** any mutation, leave no partial object behind, and
leave the console able to prepare a fresh, corrected plan.

- [ ] **MR1** Profile name that is uppercase, contains `_`/`.`, starts/ends with `-`, or exceeds 63 characters → refused by the client rule and by the server.
- [ ] **MR2** Profile name that already exists (Profile or Namespace) → conflict, no adoption.
- [ ] **MR3** Profile update that omits an existing supported quota key → refused, key not silently dropped.
- [ ] **MR4** Profile update that changes an operator-managed quota key → refused; the same value round-tripped is accepted and preserved.
- [ ] **MR5** Profile update with an empty quota set → refused (removal is unsupported).
- [ ] **MR6** PVC create with a class outside the approved list → refused.
- [ ] **MR7** PVC expand on an unbound, terminating, class-less, non-CSI or NFS-backed claim → refused.
- [ ] **MR8** PVC expand to a smaller or equal size → refused.
- [ ] **MR9** Static create with a template whose class/access modes do not match → refused; new registration outside all approved NFS roots → refused (legacy remains strictly below its base); with a backend already represented by a PV → refused.
- [ ] **MR10** PV reservation of a `Released`, `Bound`, topology-constrained or block-mode PV → refused.
- [ ] **MR11** Notebook start/stop/edit against a namespace outside the configured scope or in a protected namespace → refused.
- [ ] **MR12** Notebook edit with an image outside the approved list, or a GPU count without an approved preset → refused.
- [ ] **MR13** Notebook mount edit removing a protected path, referencing a claim from another namespace, or duplicating a name/path → refused.
- [ ] **MR14** Deletion attempted while the deletion opt-in is off, or with a wrong typed confirmation → refused by the API before any plan is created; nothing is deleted.
- [ ] **MR15** Any write attempted while `features.writesEnabled` is false → refused by the server, even if the UI control is reachable.
- [ ] **MR16** A stale or expired plan submitted after its TTL → refused; prepare a new plan instead of reusing it.
- [ ] **MR17** Double submission of the same request → one operation only; the idempotency key must not create a second mutation.
- [ ] **MR18** Unauthenticated/non-administrator writes or missing/forged CSRF are refused (401/403). Forged identity headers cannot authenticate a request; authenticated sessions do not need identity headers, which the app ignores.
- [ ] **MR19** `profile.update` with an owner that differs from the current owner → refused as an unsupported contract (422 validation failure), with no Profile, namespace or RoleBinding write. A `profile.update` with a missing or stale `baseConfigDigest` → refused, so a stale prefill cannot overwrite a concurrent quota change.

## 5. Evidence hygiene and end of pilot

- [ ] **HY1** Capture operation IDs, plan IDs, actor labels, timestamps and observation timestamps. Report statuses and durations only; do not export HAR files, cookies, response bodies or tokens.
- [ ] **HY2** Keep the acceptance record private. It contains identity labels and cluster names; do not commit it to this repository.
- [ ] **HY3** Verify the History and Audit tabs show the run: actor label, target, action, result and reason, with every stage available in details. Historical rows without a captured label must show the generic legacy-administrator label rather than an encoded identifier.
- [ ] **HY4** After the pilot: revoke the optional RBAC grants through the platform operator's reviewed procedure, disable the gates in config, and confirm the console returns to read-only.
- [ ] **HY5** Note that disabling a gate does not undo completed mutations and does not revoke RBAC.
- [ ] **HY6** Write an **operator** cleanup plan for the disposable pilot's leftovers (namespace, PVCs, PVs, backend directories) — the console will not do it. Execute it only against the disposable pilot targets and only with the storage operator's agreement.
- [ ] **HY7** If any operation ended in `needs-review`, reconcile it against the real cluster state before the next release; do not hand-edit the database and do not delete lock rows to clear it.
- [ ] **HY8** Record the result of every item in the private acceptance record, including the failures — a refused action that was correctly refused is a pass.

## 6. Sign-off

| Area | Result | Evidence reference | Reviewer | Date |
| --- | --- | --- | --- | --- |
| Preconditions (backup, scope, gates, controller contract) | | | | |
| Profile create (PC1–PC7) | | | | |
| Profile update, quota only (PU1–PU11) | | | | |
| Profile delete, gated and typed (PD1–PD12) | | | | |
| PVC create (VC1–VC7) | | | | |
| PVC expansion (VE1–VE6) | | | | |
| Legacy combined static pair (VS1–VS8) | | | | |
| PV reservation (VR1–VR4) | | | | |
| Separate registration/binding and PodDefaults (VP1–VP11) | | | | |
| Notebook start (NB1–NB5) | | | | |
| Notebook stop (NP1–NP4) | | | | |
| Notebook edit — resources (NR1–NR7) | | | | |
| Notebook edit — storage mounts (NM1–NM6) | | | | |
| Pod restart (PR1–PR6) | | | | |
| Node cordon / uncordon (NC1–NC4) | | | | |
| Console surfaces, drilldown, filters, safe details (UI1–UI15) | | | | |
| Refusal matrix (MR1–MR19) | | | | |
| Pilot cleanup plan (HY6) | | | | |

## 7. Contract references

- [API schema](../api/openapi.yaml): `profile.update` and `profile.delete`
  use the existing plan/operation endpoints; no raw delete endpoint exists.
- [Deployment](deployment.md#7-profile-deletion--separate-opt-in-irreversible):
  separate deletion flag, permission, controller review and disposable pilot.
- [Compatibility](compatibility.md#profile-owner-and-deletion-contract):
  immutable owner and normal Profile/namespace garbage collection.
- [Verification](verification.md): local evidence, published artifact identity
  and remaining live acceptance. Unchecked boxes here are operator work, not
  an assertion that these workflows have passed on a real cluster.
